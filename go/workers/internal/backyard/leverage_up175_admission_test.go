package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"testing"
)

// Live 2026-09-29 (IMG55): every leverage_up/175 borrow held with
// funding_quote_cannot_cover_full_payoff: the borrow admission priced the
// post-borrow exit as one release + one payoff because the cycle pre-check
// counted the borrowed cash as if it were already repaid. Driven through the
// REAL borrow admission on a 1.5x position with a 1.75x borrow: the complete
// exit must be priced with a cycle.
func TestLeverageUp175BorrowAdmissionPricesTheCycleExit(t *testing.T) {
	o, m, rpc, client, accounts, route := leverage175Fixture(t)
	// 1.5x: C $100, D $33.33 (debt raw at $1).
	debt := uint64(33_333_333)
	putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(debt), 60))
	putScaledFraction(accountAt(accounts, route.Kamino.DebtReserve).Data[232:248], new(big.Int).Lsh(new(big.Int).SetUint64(debt), 60))
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.DebtReserve).Data[224:232], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtLiquiditySupply).Data[64:72], 1_000_000_000_000)
	putKey(t, accountAt(accounts, route.Kamino.DebtReserve).Data[192:224], route.DebtFeeReceiver)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+40:], 0)
	if accountAt(accounts, route.DebtFeeReceiver).Address != route.DebtFeeReceiver {
		fee := accountAt(accounts, route.DebtLiquiditySupply)
		fee.Address, fee.Data = route.DebtFeeReceiver, append([]byte(nil), fee.Data...)
		binary.LittleEndian.PutUint64(fee.Data[64:72], 5)
		accounts = append(accounts, fee)
		rpc = budgetBuildRPCWithAccounts(t, 5000, 42, accounts)
	}
	s := &o.Snapshot
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS = int64(debt), int64(debt), int64(debt)+100, 3333
	s.LeverageTargetLevel, s.SquadsIdleRaw, s.DebtIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw = 1.75, 0, 0, 0, 0
	d := Decision{Action: OpenRouteStep, StrategyKey: route.Lane, Reason: leverageUpReason, AmountRaw: 175}
	position := KaminoPosition{CollateralDepositedRaw: 100_000_000_000, RedeemablePrimeRaw: 100_000_000_000, DebtRaw: debt, CollateralDecimals: 9, DebtDecimals: 6}
	binary.LittleEndian.PutUint64(position.CollateralPriceSF[:8], uint64(1)<<60)
	binary.LittleEndian.PutUint64(position.DebtPriceSF[:8], uint64(1)<<60)
	for _, address := range []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve} {
		a := accountAt(accounts, address).Data
		binary.LittleEndian.PutUint64(a[kaminoReserveConfigOffset+160:], 1_000_000_000_000_000)
		binary.LittleEndian.PutUint64(a[kaminoReserveConfigOffset+168:], 1_000_000_000_000_000)
		binary.LittleEndian.PutUint64(a[kaminoOutsideBorrowLimitOffset:], 1_000_000_000_000_000)
		binary.LittleEndian.PutUint64(a[kaminoBorrowFactorOffset:], 100)
		binary.LittleEndian.PutUint64(a[kaminoMarketPriceLastUpdatedTSOffset:], 1000)
	}
	clock := clockFixture()
	binary.LittleEndian.PutUint64(clock.Data[:8], 42)
	binary.LittleEndian.PutUint64(clock.Data[32:], 1000)
	accounts = append(accounts, clock)
	rpc = budgetBuildRPCWithAccounts(t, 5000, 42, accounts)
	applyBorrowCapacity(s, position, accounts, route)
	borrow, err := capacitySizedBorrow(position, accounts, route, 175)
	d.AmountRaw = int64(borrow)
	s.LeverageApprovedBorrowRaw, s.LeverageSourceDebtRaw = borrow, debt
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.kaminoPacketForRoute(OpenRouteStep, kaminoLegBorrow, borrow, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	effects, err := kaminoBorrowEffects(accounts, route, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	// The borrow simulation returns the post-borrow accounts.
	addresses := append(depositProjectionAddresses(route), route.Kamino.DebtReserve, route.DebtLiquiditySupply, route.DebtFeeReceiver)
	_, full, err := rpc.GetMultipleAccounts(context.Background(), addresses, 42)
	if err != nil {
		t.Fatal(err)
	}
	after := debt + borrow
	underlying := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string
			Params []json.RawMessage
		}
		if json.Unmarshal(body, &call) != nil || call.Method != "simulateTransaction" {
			return underlying.RoundTrip(req)
		}
		var rows []any
		for _, original := range full {
			a := original
			a.Data = append([]byte(nil), a.Data...)
			switch a.Address {
			case route.Kamino.Obligation:
				putScaledFraction(a.Data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(after), 60))
			case route.Kamino.DebtReserve:
				binary.LittleEndian.PutUint64(a.Data[224:232], 1_000_000_000_000-borrow)
				putScaledFraction(a.Data[232:248], new(big.Int).Lsh(new(big.Int).SetUint64(after), 60))
			case route.DebtLiquiditySupply:
				binary.LittleEndian.PutUint64(a.Data[64:72], 1_000_000_000_000-borrow)
			case route.DebtCustody:
				binary.LittleEndian.PutUint64(a.Data[64:72], borrow)
			}
			rows = append(rows, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"err": nil, "unitsConsumed": 250_000, "accounts": rows}}})
		return response(string(payload)), nil
	})
	plan, err := observePhase3BorrowAdmission(context.Background(), rpc, client, m, o, d, KaminoExecutionEvidence{r, effects})
	if err != nil {
		t.Fatalf("leverage_up 175 borrow admission: %v", err)
	}
	cycleRepays := 0
	for _, step := range plan.Exit {
		if step.Action == DeleverRouteStep && step.Template != nil {
			if req, _, _, err := step.Template.decode(); err == nil {
				if k, ok := req.(KaminoPrimeUSDCRequest); ok {
					if _, leg, _ := kaminoPrimeUSDCInstruction(k); leg == kaminoLegRepay && !k.FullPayoff && step.Amount < after {
						cycleRepays++
					}
				}
			}
		}
	}
	if cycleRepays < 1 || plan.BorrowProjection == nil || plan.Payoff == nil || plan.ExitAfterMicros <= 0 || plan.ExitCycles != 1 || plan.BorrowRelease == nil {
		t.Fatalf("1.75x exit not priced with a cycle: %d cycle repays, %d cycles", cycleRepays, plan.ExitCycles)
	}
	// Build / final send: the same revalidation revaluePhase3SignedInput runs
	// (validatePilotProjectedReleaseRisk re-simulates the borrow and checks
	// the bound first-cycle release against the fresh post-borrow state).
	if _, err := validatePilotProjectedReleaseRisk(context.Background(), rpc, &plan, 42); err != nil {
		t.Fatalf("final-send revalidation refused the admitted 1.75x plan: %v", err)
	}
	// Safety kept: a release above the safe size on the fresh state is refused.
	req, effects2, _, _ := plan.BorrowRelease.decode()
	big1 := req.(KaminoPrimeUSDCRequest)
	big1.AmountRaw *= 3
	encoded, _ := jsonMarshalExpectedEffects(effects2)
	plan.BorrowRelease, _ = encodePhase3BuildInput(big1, encoded)
	if _, err = validatePilotProjectedReleaseRisk(context.Background(), rpc, &plan, 42); err == nil {
		t.Fatal("a release 3x above the admitted cycle release passed final send")
	}
}

// The pre-check on the live post-borrow shape (IMG55, 20:20): 1772.66 AUTO
// @ 1.021 ($1,809.88), debt 602.87 + 302.40 borrowed = 905.26 PYUSD, the
// 302.40 still in debt custody. The old check netted the cash against the
// debt before sizing the release ($664 release, "covered"); the real safe
// release sits on the full debt ($164) and cannot fund the payoff.
func TestLeverageExitPreCheckSizesTheReleaseOnTheFullDebt(t *testing.T) {
	c, d, cash := big.NewInt(1_809_882_868), big.NewInt(905_262_160), big.NewInt(302_395_505)
	if !leverageExitOneReleaseMayNotCover(c, big.NewInt(0), d, cash) {
		t.Fatal("live post-borrow 1.75x judged coverable by one release")
	}
	// The same position at 1.5x with no cash: one release covers.
	if leverageExitOneReleaseMayNotCover(big.NewInt(1_809_882_868), big.NewInt(0), big.NewInt(602_866_655), big.NewInt(0)) {
		t.Fatal("1.5x flagged for cycles")
	}
	s := base()
	s.RouteLane, s.StrategyKey = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw, s.DebtIdleRaw = 1_809_882_868, 905_262_160, 302_395_505
	if !leverageExitMayNeedCycles(s) {
		t.Fatal("snapshot pre-check missed the post-borrow cycle")
	}
}

// A borrow reconciles its own fee delta on the shared fee receiver even
// when other borrowers paid fees into it between observation and landing.
func TestBorrowReconcilesTheSharedFeeReceiverByDelta(t *testing.T) {
	route := ethenaUSDePYUSD
	e := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-borrow", Conserved: true, Accounts: []ExpectedAccountEffect{
		{Address: route.DebtLiquiditySupply, Owner: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: 1_000, AfterRaw: 896},
		{Address: route.DebtCustody, Owner: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: 100},
		{Address: route.DebtFeeReceiver, Owner: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: 50, AfterRaw: 54},
	}}
	fresh := e
	fresh.Accounts = append([]ExpectedAccountEffect(nil), e.Accounts...)
	fresh.Accounts[2].BeforeRaw, fresh.Accounts[2].AfterRaw = 70, 74
	if !borrowEffectsMatch(fresh, e) {
		t.Fatal("shared receiver balance move refused")
	}
	fresh.Accounts[2].AfterRaw = 75
	if borrowEffectsMatch(fresh, e) {
		t.Fatal("a changed fee accepted")
	}
	fresh.Accounts[2].AfterRaw = 74
	fresh.Accounts[1].BeforeRaw = 1
	if borrowEffectsMatch(fresh, e) {
		t.Fatal("our custody drift accepted")
	}
}
