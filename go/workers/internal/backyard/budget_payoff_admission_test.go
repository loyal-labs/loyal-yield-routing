package backyard

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

func payoffAdmissionFixture(t *testing.T, debtOutput uint64, extraAccounts ...ConfirmedAccount) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	route := ethenaUSDePYUSD
	sf := new(big.Int).Lsh(big.NewInt(1), 60)
	reserve := reserveFixture(t, route.Kamino.DebtReserve, route.Kamino.DebtMint, 42, new(big.Int).Mul(sf, big.NewInt(2)), 1_000_000, 1_000_000)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
	binary.LittleEndian.PutUint32(reserve.Data[28:32], 1000)
	reserve.Data[kaminoReserveConfigOffset+9] = 1 // true APR, elapsed seconds
	for i := 0; i < 11; i++ {
		offset := kaminoReserveConfigOffset + 64 + i*8
		if i > 0 {
			binary.LittleEndian.PutUint32(reserve.Data[offset:], 10_000)
		}
		binary.LittleEndian.PutUint32(reserve.Data[offset+4:], 7500)
	}
	obligation := obligationFixture(t, 42, 100_000_000, 1_000)
	obligation.Address = route.Kamino.Obligation
	putKey(t, obligation.Data[32:64], route.Kamino.Market)
	putKey(t, obligation.Data[96:128], route.Kamino.CollateralReserve)
	putKey(t, obligation.Data[1208:1240], route.Kamino.DebtReserve)
	clock := ConfirmedAccount{Address: budgetClockAddress, Owner: "Sysvar1111111111111111111111111111111111111", Lamports: 1, Data: make([]byte, 40)}
	binary.LittleEndian.PutUint64(clock.Data[:8], 42)
	binary.LittleEndian.PutUint64(clock.Data[32:40], 1000)
	accounts := []ConfirmedAccount{reserve, obligation, clock}
	for _, row := range []struct {
		boundary kaminoCustodyBoundary
		program  string
		amount   uint64
	}{
		{kaminoCustodyBoundary{route.DebtCustody, route.Kamino.DebtMint, bridgeVault}, token2022Program, 11_000},
		{kaminoCustodyBoundary{route.DebtLiquiditySupply, route.Kamino.DebtMint, route.Kamino.MarketAuthority}, token2022Program, 1_000_000},
		{kaminoCustodyBoundary{route.CollateralCustody, route.Kamino.CollateralMint, bridgeVault}, classicTokenProgram, 0},
		{kaminoCustodyBoundary{route.CollateralLiquiditySupply, route.Kamino.CollateralMint, route.Kamino.MarketAuthority}, classicTokenProgram, 1_000_000_000},
	} {
		data := make([]byte, 165)
		putKey(t, data[:32], row.boundary.Mint)
		putKey(t, data[32:64], row.boundary.Authority)
		binary.LittleEndian.PutUint64(data[64:72], row.amount)
		data[108] = 1
		accounts = append(accounts, ConfirmedAccount{Address: row.boundary.Address, Owner: row.program, Lamports: 1, Data: data})
	}
	var installed struct {
		Operations []struct{ PolicyAddress, DataBase64 string }
	}
	data, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/policy-install-readback-v1.json")
	if err != nil || json.Unmarshal(data, &installed) != nil {
		t.Fatal("missing installed policy bytes", err)
	}
	for _, p := range installed.Operations {
		data, err := base64.StdEncoding.Strict().DecodeString(p.DataBase64)
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, ConfirmedAccount{Address: p.PolicyAddress, Owner: squads.ProgramID.String(), Lamports: 1, Data: data})
	}
	accounts = append(accounts, extraAccounts...)
	o, d, _, manifest, rpc, client := debtResidueAdmissionFixture(t, debtOutput, accounts...)
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.DebtIdleRaw = 1_000, 2_000, 11_000
	d.AmountRaw, d.Reason = 1_000, "withdrawal_repay_debt"
	request, err := manifest.kaminoPacketForRoute(testPolicies(t), DeleverRouteStep, kaminoLegRepay, 1_001, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	request.FullPayoff = true
	request.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	effects, err := boundedKaminoRepaymentEffects(accounts, source, destination, 1_000, 1_001)
	if err != nil {
		t.Fatal(err)
	}
	return o, d, KaminoExecutionEvidence{request, effects}, manifest, rpc, client, accounts
}

// After a successful repayment the debt-free position has a fresh NAV ->
// withdrawal -> return path, and a later nonzero debt invalidates it.
func TestPostPayoffNAVPricesTheDebtFreeReturn(t *testing.T) {
	t.Parallel()
	o, _, _, m, rpc, client, accounts := payoffAdmissionFixture(t, 20_000)
	// Model a 1000-unit actual repayment; largest possible residue is 10000.
	obligation := accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation)
	clear(obligation.Data[1208:1408])
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.DebtIdleRaw = 0, 0, 10_000
	d := Decision{Action: ReportNAV, StrategyKey: o.Snapshot.RouteLane}
	effects, _, _, err := bridgeExpectedEffects(d, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	request := bridgeTestRequest(ReportNAV, 0)
	request.Report.Sequence, request.Report.ObservedSlot = 42, 42
	nav, err := pricePhase3PositionReturn(context.Background(), rpc, fixtureView(t, rpc), client, m, o, d, request, effects, false)
	if err != nil || len(nav.Exit) != 10 || nav.Exit[0].Action != DeleverRouteStep || nav.Exit[0].Template == nil {
		t.Fatal("post-payoff NAV lost its return", err)
	}
	withdrawRequest, _, _, err := nav.Exit[0].Template.decode()
	if err != nil || withdrawRequest.(KaminoPrimeUSDCRequest).AmountRaw != 100_000_000 {
		t.Fatal("missing full withdrawal template", err)
	}
	putScaledFraction(obligation.Data[1296:1312], new(big.Int).Lsh(big.NewInt(1), 60))
	putKey(t, obligation.Data[1208:1240], ethenaUSDePYUSD.Kamino.DebtReserve)
	if _, err := pricePhase3PositionReturn(context.Background(), rpc, fixtureView(t, rpc), client, m, o, d, request, effects, false); err == nil {
		t.Fatal("nonzero debt became a debt-free NAV")
	}
}

// The full-payoff condition runs on the persisted signed bytes at build and
// send: a changed rate is not grandfathered by yesterday's sufficient wire.
func TestFullPayoffPrestateRefusesUnderfundingBeforeSigner(t *testing.T) {
	t.Parallel()
	_, _, e, _, rpc, _, a := payoffAdmissionFixture(t, 20_000)
	if err := validateBuildPrestate(context.Background(), rpc, fixtureView(t, rpc), e.Request, e.ExpectedEffects); err != nil {
		t.Fatal(err)
	}
	reserve := accountAt(a, ethenaUSDePYUSD.Kamino.DebtReserve)
	for i := 0; i < 11; i++ {
		binary.LittleEndian.PutUint32(reserve.Data[kaminoReserveConfigOffset+68+i*8:], 1_000_000_000)
	}
	assertBudgetHold(t, validateBuildPrestate(context.Background(), rpc, fixtureView(t, rpc), e.Request, e.ExpectedEffects), "full_payoff_request_underfunded")
}

func TestPayoffBoundUsesAccrualBasisAndUnroundedDebt(t *testing.T) {
	t.Parallel()
	_, _, _, _, _, _, accounts := payoffAdmissionFixture(t, 20_000)
	route := ethenaUSDePYUSD
	obligation := accountAt(accounts, route.Kamino.Obligation)
	reserve := accountAt(accounts, route.Kamino.DebtReserve)
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	putScaledFraction(obligation.Data[1296:1312], new(big.Int).Mul(big.NewInt(1_000_000), one))
	seconds, err := decodeKaminoPayoffBound(accounts, route, 42)
	if err != nil || seconds.UpperDebtRaw != 1_000_002 {
		t.Fatal("seconds mode lost elapsed interest", seconds, err)
	}
	reserve.Data[kaminoReserveConfigOffset+9] = 0
	slots, err := decodeKaminoPayoffBound(accounts, route, 42)
	if err != nil || slots.UpperDebtRaw != 1_000_001 {
		t.Fatal("legacy slot mode changed", slots, err)
	}
	reserve.Data[kaminoReserveConfigOffset+9] = 1
	fraction := new(big.Int).Add(new(big.Int).Mul(big.NewInt(1_000), one), new(big.Int).Quo(new(big.Int).Set(one), big.NewInt(100)))
	putScaledFraction(obligation.Data[1296:1312], fraction)
	refreshed, err := decodeKaminoPayoffBound(accounts, route, 42)
	if err != nil || refreshed.ObservedDebtRaw != 1_001 || refreshed.UpperDebtRaw != 1_001 {
		t.Fatal("raw ceil was compounded into fictitious debt", refreshed, err)
	}
	for _, mutate := range []func([]ConfirmedAccount){
		func(a []ConfirmedAccount) {
			accountAt(a, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+7] = 1
		},
		func(a []ConfirmedAccount) {
			accountAt(a, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+920] = 1
		},
		func(a []ConfirmedAccount) { clear(accountAt(a, route.Kamino.DebtReserve).Data[28:32]) },
		func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint32(accountAt(a, route.Kamino.DebtReserve).Data[28:32], 1001)
		},
		func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint32(accountAt(a, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+64:], 1)
		},
		func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint32(accountAt(a, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+72:], 0)
		},
		func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(accountAt(a, budgetClockAddress).Data[:8], 44)
		},
	} {
		_, _, _, _, _, _, a := payoffAdmissionFixture(t, 20_000)
		mutate(a)
		if _, err := decodeKaminoPayoffBound(a, route, 42); err == nil {
			t.Fatal("unsupported interest state accepted")
		}
	}
	if _, err := upperKaminoCompoundedDebtSF(one, ^uint64(0), ^uint64(0), 1); err == nil {
		t.Fatal("unbounded interest overflow accepted")
	}
	if got, err := upperKaminoCompoundedDebtSF(one, 0, 10_000, 31_536_000); err != nil || got != 1 {
		t.Fatal("zero rate creates interest", got, err)
	}
}
