package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"reflect"
	"strconv"
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
	// Every registry lane's payoff window also captures its lending market.
	accounts := []ConfirmedAccount{reserve, obligation, clock, marketFixture(t, route.Kamino.Market)}
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
	request.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	effects, err := boundedKaminoRepaymentEffects(accounts, source, destination, 1_000, 1_001)
	if err != nil {
		t.Fatal(err)
	}
	return o, d, KaminoExecutionEvidence{request, effects}, manifest, rpc, client, accounts
}

func debtResidueAdmissionFixture(t *testing.T, debtOutput uint64, extraAccounts ...ConfirmedAccount) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client) {
	t.Helper()
	route := ethenaUSDePYUSD
	reserve := reserveFixture(t, route.Kamino.DebtReserve, route.Kamino.DebtMint, 42, new(big.Int).Lsh(big.NewInt(2), 60), 1_000_000, 1_000_000)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
	mint := ConfirmedAccount{Address: route.Kamino.DebtMint, Owner: token2022Program, Lamports: 1, Data: make([]byte, 82)}
	mint.Data[44], mint.Data[45] = 6, 1
	extra := []ConfirmedAccount{reserve, mint}
	var headers struct {
		Rows []struct {
			Key         string
			Instruction struct {
				ProgramID, DataBase64 string
				Accounts              []jupiter.AccountMeta
			}
		}
	}
	data, err := os.ReadFile(jupiterV2FixturePath)
	if err != nil || json.Unmarshal(data, &headers) != nil {
		t.Fatal("header evidence missing", err)
	}
	var instruction JupiterSwapInstruction
	for _, r := range headers.Rows {
		if r.Key == "PYUSD->USDC" {
			instruction = JupiterSwapInstruction{ProgramID: r.Instruction.ProgramID, Data: r.Instruction.DataBase64, Accounts: r.Instruction.Accounts}
		}
	}
	data, err = base64.StdEncoding.Strict().DecodeString(instruction.Data)
	if err != nil || len(data) < jupiter.V2RoutePlanOffset {
		t.Fatal("missing debt exit bytes")
	}
	binary.LittleEndian.PutUint64(data[jupiter.V2InAmountOffset:], 10_000)
	binary.LittleEndian.PutUint64(data[jupiter.V2QuotedOutOffset:], debtOutput)
	binary.LittleEndian.PutUint16(data[jupiter.V2SlippageOffset:], 50)
	instruction.Data = base64.StdEncoding.EncodeToString(data)
	o, d, e, m, rpc, client := withdrawalAdmissionFixture(t, 100_000, append(extra, extraAccounts...)...)
	o.Snapshot.DebtIdleRaw = 10_000
	previous := fixtureHTTP(client).Transport
	debtQuote := false
	fixtureHTTP(client).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == "GET" {
			debtQuote = req.URL.Query().Get("inputMint") == route.Kamino.DebtMint
		}
		if !debtQuote {
			return previous.RoundTrip(req)
		}
		var payload any
		if req.Method == "GET" {
			if req.URL.Query().Get("amount") != "10000" || req.URL.Query().Get("outputMint") != bridgeUSDC {
				t.Fatal("debt residue quote changed amount or destination")
			}
			payload = jupiter.Quote{InputMint: route.Kamino.DebtMint, OutputMint: bridgeUSDC, InAmount: "10000", OutAmount: fmt.Sprint(debtOutput), OtherAmountThreshold: fmt.Sprint(debtOutput), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
		} else {
			payload = map[string]any{"swapInstruction": instruction}
		}
		encoded, _ := json.Marshal(payload)
		return response(string(encoded)), nil
	})
	return o, d, e, m, rpc, client
}

// Controlled quote/RPC transport around actual compilers and installed Jupiter
// bytes. Synthetic reserve prices/bridge-policy bytes do not prove live state.
func withdrawalAdmissionFixture(t *testing.T, quoted uint64, extraAccounts ...ConfirmedAccount) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client) {
	t.Helper()
	route := ethenaUSDePYUSD
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	o := tickObservation(Snapshot{ObservationID: "withdrawal-admission", Slot: 42, Fresh: true, RouteKind: RouteKind,
		RouteLane: route.Lane, StrategyKey: route.Lane, HasPosition: true, PositionCollateralRaw: 100_000_000,
		PositionCollateralValueRaw: 100_000, ReportSnapshotDigest: sha256Bytes([]byte("controlled-withdrawal-state"))})
	d := Decision{Action: DeleverRouteStep, AmountRaw: 0, StrategyKey: route.Lane, Reason: "withdrawal_withdraw_collateral", IdempotencyKey: "withdrawal-admission"}
	r, err := manifest.kaminoPacketForRoute(testPolicies(t), d.Action, kaminoLegWithdraw, 100_000_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{
		{Address: source.Address, Owner: classicTokenProgram, Mint: source.Mint, Authority: source.Authority, BeforeRaw: 1_000_000_000, AfterRaw: 900_000_000},
		{Address: destination.Address, Owner: classicTokenProgram, Mint: destination.Mint, Authority: destination.Authority, BeforeRaw: 0, AfterRaw: 100_000_000},
	}}
	o.policies = testPolicies(t)
	extra := []ConfirmedAccount{exactReportTicketAccount(t, 1)}
	reserve := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 42, new(big.Int).Lsh(big.NewInt(1), 60), 1_000_000_000, 1_000_000_000)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(reserve.Data[272:280], 9)
	binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
	mint := ConfirmedAccount{Address: route.Kamino.CollateralMint, Owner: classicTokenProgram, Lamports: 1, Data: make([]byte, 82)}
	mint.Data[44], mint.Data[45] = 9, 1
	extra = append(extra, reserve, mint)
	rpc := budgetBuildRPCWithAccounts(t, 5_000, 42, append(extra, extraAccounts...))
	var headers struct {
		Rows []struct {
			Key         string
			Instruction struct {
				ProgramID, DataBase64 string
				Accounts              []jupiter.AccountMeta
			}
		}
	}
	if raw, err := os.ReadFile(jupiterV2FixturePath); err != nil || json.Unmarshal(raw, &headers) != nil {
		t.Fatal("v2 swap fixture unavailable", err)
	}
	var instruction JupiterSwapInstruction
	for _, row := range headers.Rows {
		if row.Key == "USDe->USDC" {
			instruction = JupiterSwapInstruction{ProgramID: row.Instruction.ProgramID, Data: row.Instruction.DataBase64, Accounts: row.Instruction.Accounts}
		}
	}
	data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
	if err != nil || len(data) < jupiter.V2RoutePlanOffset {
		t.Fatal("missing retained exit instruction")
	}
	binary.LittleEndian.PutUint64(data[jupiter.V2InAmountOffset:], 100_000_000)
	binary.LittleEndian.PutUint64(data[jupiter.V2QuotedOutOffset:], quoted)
	binary.LittleEndian.PutUint16(data[jupiter.V2SlippageOffset:], 50)
	instruction.Data = base64.StdEncoding.EncodeToString(data)
	client, err := fixtureJupiter(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload any
		if req.Method == "GET" && req.URL.Path == "/quote" {
			if req.URL.Query().Get("amount") != "100000000" || req.URL.Query().Get("inputMint") != route.Kamino.CollateralMint || req.URL.Query().Get("outputMint") != bridgeUSDC {
				t.Fatal("exit quote changed custody or amount")
			}
			payload = jupiter.Quote{InputMint: route.Kamino.CollateralMint, OutputMint: bridgeUSDC, InAmount: "100000000", OutAmount: fmt.Sprint(quoted), OtherAmountThreshold: fmt.Sprint(quoted), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
		} else if req.Method == "POST" && req.URL.Path == "/swap-instructions" {
			payload = map[string]any{"swapInstruction": instruction}
		} else {
			t.Fatal("unexpected quote transport request")
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return response(string(encoded)), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return o, d, KaminoExecutionEvidence{r, effects}, manifest, rpc, client
}

func fundingAdmissionFixture(t *testing.T, output uint64) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	return fundingAdmissionFixtureForSource(t, output, SwapCollateralToDebtStep)
}

func fundingAdmissionFixtureForSource(t *testing.T, output uint64, fundingAction Action) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	var extra []ConfirmedAccount
	if fundingAction == SwapUSDCToDebtStep {
		data := make([]byte, 165)
		putKey(t, data[:32], bridgeUSDC)
		putKey(t, data[32:64], bridgeVault)
		binary.LittleEndian.PutUint64(data[64:72], 20_000)
		data[108] = 1
		extra = append(extra, ConfirmedAccount{Address: bridgeSquadsATA, Owner: classicTokenProgram, Lamports: 1, Data: data})
	}
	o, _, _, m, rpc, _, accounts := payoffAdmissionFixture(t, 20_000, extra...)
	route := ethenaUSDePYUSD
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.DebtIdleRaw = 20_000_000, 20_000_000, 1_000
	o.Snapshot.CollateralIdleValueRaw = 20_000
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 20_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 1_000)
	if fundingAction == SwapUSDCToDebtStep {
		o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.SquadsIdleRaw = 0, 0, 20_000
		o.Snapshot.CollateralIdleValueRaw = 0
		binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 0)
	}
	var headers struct {
		Rows []struct {
			Key         string
			Instruction struct {
				ProgramID, DataBase64 string
				Accounts              []jupiter.AccountMeta
			}
		}
	}
	data, err := os.ReadFile(jupiterV2FixturePath)
	if err != nil || json.Unmarshal(data, &headers) != nil {
		t.Fatal("missing actual instruction headers", err)
	}
	instructions := map[Action]JupiterSwapInstruction{}
	for _, row := range headers.Rows {
		for action, key := range map[Action]string{SwapDebtToCollateralStep: "PYUSD->USDe", SwapStableToCollateralStep: "USDC->USDe", SwapCollateralToDebtStep: "USDe->PYUSD", SwapUSDCToDebtStep: "USDC->PYUSD", SwapCollateralToStableStep: "USDe->USDC", SwapDebtToUSDCStep: "PYUSD->USDC"} {
			if row.Key == key {
				instructions[action] = JupiterSwapInstruction{ProgramID: row.Instruction.ProgramID, Data: row.Instruction.DataBase64, Accounts: row.Instruction.Accounts}
			}
		}
	}
	var instruction JupiterSwapInstruction
	client, err := fixtureJupiter(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload any
		if req.Method == "GET" {
			q := req.URL.Query()
			amount, err := strconv.ParseUint(q.Get("amount"), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			action, out := SwapCollateralToStableStep, amount/1000
			if q.Get("inputMint") == bridgeUSDC && q.Get("outputMint") == route.Kamino.CollateralMint {
				action, out = SwapStableToCollateralStep, amount*1000
			}
			if q.Get("outputMint") == route.Kamino.DebtMint {
				action, out = SwapCollateralToDebtStep, output
				if q.Get("inputMint") == bridgeUSDC {
					action = SwapUSDCToDebtStep
				}
			}
			if q.Get("inputMint") == route.Kamino.DebtMint {
				action, out = SwapDebtToUSDCStep, amount*2
				if q.Get("outputMint") == route.Kamino.CollateralMint {
					action, out = SwapDebtToCollateralStep, amount*2000
				}
			}
			instruction = instructions[action]
			wire, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
			if err != nil || len(wire) < jupiter.V2RoutePlanOffset {
				t.Fatal("missing edge wire", action, err)
			}
			binary.LittleEndian.PutUint64(wire[jupiter.V2InAmountOffset:], amount)
			binary.LittleEndian.PutUint64(wire[jupiter.V2QuotedOutOffset:], out)
			binary.LittleEndian.PutUint16(wire[jupiter.V2SlippageOffset:], 50)
			instruction.Data = base64.StdEncoding.EncodeToString(wire)
			payload = jupiter.Quote{InputMint: q.Get("inputMint"), OutputMint: q.Get("outputMint"), InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(out), OtherAmountThreshold: fmt.Sprint(out * 9950 / 10000), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
		} else {
			payload = map[string]any{"swapInstruction": instruction}
		}
		encoded, _ := json.Marshal(payload)
		return response(string(encoded)), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	d := Decision{Action: SwapCollateralToDebtStep, AmountRaw: o.Snapshot.CollateralIdleRaw, StrategyKey: route.Lane, Reason: "withdrawal_swap_repayment_buffer", IdempotencyKey: "funding-return"}
	if fundingAction == SwapUSDCToDebtStep {
		d.Action, d.AmountRaw, d.Reason = fundingAction, o.Snapshot.SquadsIdleRaw, "withdrawal_usdc_repayment_buffer"
	}
	e, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, testPolicies(t), d, uint64(d.AmountRaw), uint64(o.Snapshot.DebtIdleRaw), 42)
	if err != nil {
		t.Fatal(err)
	}
	return o, d, e, m, rpc, client, accounts
}

func bridgeAdmissionFixture(t *testing.T, action Action, amount, idle, strategy, squads int64) (Observation, Decision, BridgeExecutionEvidence) {
	t.Helper()
	s := Snapshot{ObservationID: "bridge-admission", Slot: 42, Fresh: true, RouteKind: RouteKind,
		RouteLane: ethenaUSDePYUSD.Lane, StrategyKey: ethenaUSDePYUSD.Lane,
		VoltrIdleRaw: idle, VoltrStrategyIdleRaw: strategy, SquadsIdleRaw: squads}
	d := Decision{Action: action, AmountRaw: amount, StrategyKey: s.RouteLane, Reason: "bridge-admission-test", IdempotencyKey: "bridge-admission-test"}
	r := bridgeTestRequest(action, uint64(amount))
	r.Report.ObservedSlot, r.Report.Sequence = 42, 42
	effects, _, afterSquads, err := bridgeExpectedEffects(rwa, d, uint64(idle), uint64(strategy), uint64(squads))
	if err != nil {
		t.Fatal(err)
	}
	r.Report.NAVAfterRaw = afterSquads
	effects.Kind = "bridge"
	if action != StageSquadsToVoltr {
		effects.ReturnData = expectedAdaptorReturnData(rwa, r.Report.NAVAfterRaw)
	}
	return tickObservation(s), d, BridgeExecutionEvidence{r, effects}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// receiptFor renders the expected effects as a transaction receipt, with
// optional per-address pre/post overrides.
func receiptFor(e ExpectedEffects, override map[string][2]uint64) ConfirmedTransactionEvidence {
	receipt := ConfirmedTransactionEvidence{Signature: "receipt-fixture", Slot: 42}
	for _, a := range e.Accounts {
		pre, post := a.BeforeRaw, a.AfterRaw
		if o, ok := override[a.Address]; ok {
			pre, post = o[0], o[1]
		}
		balance := TransactionTokenBalance{Address: a.Address, OwnerProgram: a.Owner, Mint: a.Mint, Authority: a.Authority}
		balance.Raw = pre
		receipt.PreTokenBalances = append(receipt.PreTokenBalances, balance)
		balance.Raw = post
		receipt.PostTokenBalances = append(receipt.PostTokenBalances, balance)
	}
	return receipt
}

// Controlled simulation and quotes around actual compilers/valuation. Real
// deployed borrow fee behavior is separately compared in kamino_borrow_test.
func borrowAdmissionFixture(t *testing.T, output uint64, variant string) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, _, client, accounts := fundingAdmissionFixture(t, output)
	route := ethenaUSDePYUSD
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.DebtIdleRaw = 0, 0, 0
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw = 1, 1
	clear(accountAt(accounts, route.Kamino.Obligation).Data[1208:1408])
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 1)
	reserve := accountAt(accounts, route.Kamino.DebtReserve)
	putKey(t, reserve.Data[192:224], route.DebtFeeReceiver)
	binary.LittleEndian.PutUint64(reserve.Data[kaminoReserveConfigOffset+40:], 1<<52)
	feeAmount := uint64(4)
	if variant == "funded" {
		feeAmount = 0
		binary.LittleEndian.PutUint64(reserve.Data[kaminoReserveConfigOffset+40:], 0)
		for i := 0; i < 11; i++ {
			binary.LittleEndian.PutUint32(reserve.Data[kaminoReserveConfigOffset+68+i*8:], 0)
		}
	}
	fee := accountAt(accounts, route.DebtLiquiditySupply)
	fee.Address, fee.Data = route.DebtFeeReceiver, append([]byte(nil), fee.Data...)
	binary.LittleEndian.PutUint64(fee.Data[64:72], 5)
	accounts = append(accounts, fee)
	_, _, _, _, rpc, _ := debtResidueAdmissionFixture(t, output, accounts...)
	r, err := m.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegBorrow, 1000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve}
	effects, err := kaminoBorrowEffects(accounts, route, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	addresses := append(depositProjectionAddresses(route), route.Kamino.DebtReserve, route.DebtLiquiditySupply, route.DebtFeeReceiver)
	_, full, err := confirmedAccounts(context.Background(), rpc, addresses, 42)
	if err != nil {
		t.Fatal(err)
	}
	message, err := CompileKaminoMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	underlying := rpcOf(rpc).Transport
	rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string
			Params []json.RawMessage
		}
		if json.Unmarshal(body, &call) != nil {
			t.Fatal("invalid RPC")
		}
		if call.Method != "simulateTransaction" {
			return underlying.RoundTrip(req)
		}
		var encoded string
		var options map[string]any
		if len(call.Params) != 2 || json.Unmarshal(call.Params[0], &encoded) != nil || json.Unmarshal(call.Params[1], &options) != nil {
			t.Fatal("invalid borrow simulation")
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(encoded)
		var expectedAddresses []any
		for _, a := range addresses {
			expectedAddresses = append(expectedAddresses, a)
		}
		want := map[string]any{"encoding": "base64", "commitment": "confirmed", "minContextSlot": float64(42), "accounts": map[string]any{"encoding": "base64", "addresses": expectedAddresses}}
		if err != nil || len(wire) <= 65 || wire[0] != 0x81 || !allZero(wireSig(wire)) || !bytes.Equal(wireMsg(wire), message) || !reflect.DeepEqual(options, want) {
			t.Fatal("borrow simulation changed exact unsigned wire/options")
		}
		var rows []any
		for _, original := range full {
			a := original
			a.Data = append([]byte(nil), a.Data...)
			switch a.Address {
			case route.Kamino.Obligation:
				putKey(t, a.Data[1208:1240], route.Kamino.DebtReserve)
				copy(a.Data[1240:1272], reserve.Data[296:328])
				putScaledFraction(a.Data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(1000+feeAmount), 60))
				if variant == "position" {
					binary.LittleEndian.PutUint64(a.Data[128:136], 100_000_001)
				}
			case route.Kamino.DebtReserve:
				binary.LittleEndian.PutUint64(a.Data[224:232], 1_000_000-1000-feeAmount)
				putScaledFraction(a.Data[232:248], new(big.Int).Lsh(new(big.Int).SetUint64(1000+feeAmount), 60))
			case route.DebtCustody:
				binary.LittleEndian.PutUint64(a.Data[64:72], 1000)
			case route.DebtLiquiditySupply:
				binary.LittleEndian.PutUint64(a.Data[64:72], 1_000_000-1000-feeAmount)
			case route.DebtFeeReceiver:
				binary.LittleEndian.PutUint64(a.Data[64:72], 5+feeAmount)
				if variant == "fee" {
					binary.LittleEndian.PutUint64(a.Data[64:72], 5)
				}
			case route.CollateralCustody:
				if variant == "collateral" {
					binary.LittleEndian.PutUint64(a.Data[64:72], 0)
				}
			case budgetClockAddress:
				if variant == "clock" {
					binary.LittleEndian.PutUint64(a.Data[:8], 41)
				}
			}
			rows = append(rows, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		var failure any
		if variant == "failed" {
			failure = "invalid"
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"err": failure, "unitsConsumed": 250_000, "accounts": rows}}})
		return response(string(payload)), nil
	})
	d := Decision{Action: OpenRouteStep, StrategyKey: route.Lane, AmountRaw: 1, Reason: "collateral_requires_borrow", IdempotencyKey: "borrow-admission"}
	return o, d, KaminoExecutionEvidence{r, effects}, m, rpc, client, accounts
}

func partialWithdrawalRestoreFixture(t *testing.T, lane string, debt int64) (Observation, Decision, BridgeExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	o, m, rpc, client, accounts, route := leverage175Fixture(t)
	if lane == autoAUTOPYUSD.Lane {
		m, route, accounts = autoObservationBatch(t, 42, func(batch []ConfirmedAccount) {
			autoSourceReleasedBatch(autoAUTOPYUSD)(batch)
			// Scale the coherent 2:1 reserve and collateral position together:
			// the retained AUTO equity remains above the partial-flow minimum.
			reserve := accountAt(batch, autoAUTOPYUSD.Kamino.CollateralReserve).Data
			binary.LittleEndian.PutUint64(reserve[224:232], 3*autoFixturePoolLiquidity)
			binary.LittleEndian.PutUint64(reserve[2592:2600], 3*autoFixturePoolReceiptSupply)
			binary.LittleEndian.PutUint64(accountAt(batch, autoAUTOPYUSD.CollateralLiquiditySupply).Data[64:72], 3*autoFixturePoolLiquidity)
			obligation := kaminoObligationImage(t, autoAUTOPYUSD, 42, 3*autoFixtureDepositReceiptRaw, uint64(debt))
			copy(accountAt(batch, autoAUTOPYUSD.Kamino.Obligation).Data, obligation.Data)
			binary.LittleEndian.PutUint64(accountAt(batch, bridgeSquadsATA).Data[64:72], 0)
			binary.LittleEndian.PutUint64(accountAt(batch, bridgeIdleATA).Data[64:72], 0)
			binary.LittleEndian.PutUint64(accountAt(batch, bridgeStrategyATA).Data[64:72], 11_000_000)
		})
		// The last report still includes the $11 waiting in staged custody;
		// staging has not yet changed Voltr's book or consumed its ticket.
		upsertConfirmedAccount(&accounts, strategyReceiptFixture(t, 94_000_000))
		upsertConfirmedAccount(&accounts, voltrVaultFixture(t, 94_000_000))
		binary.LittleEndian.PutUint16(accountAt(accounts, bridgeVoltrVault).Data[514:516], uint16(rwa.AdminPerformanceFeeBPS))
		upsertConfirmedAccount(&accounts, exactReportTicketAccount(t, 40))
		var err error
		o, _, err = autoObservationForAccounts(m, 42, accounts)(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		o.policies = testPolicies(t) // the policies the admission builds through
		// Controlled counterparts of journal/identity enrichment; keep all
		// production monitors armed rather than disabling their checks.
		o.Snapshot.JournalSequenceKnown, o.Snapshot.JournalReconciledSequenceRaw = true, 40
		o.Snapshot.JournalArmedNAVKnown, o.Snapshot.JournalArmedNAVRaw = true, o.Snapshot.PriorReportedNAVRaw
		o.Snapshot.CapitalMutated, o.Snapshot.ProgramIdentityKnown = true, true
		o.Snapshot.VoltrProgramDeploySlot, o.Snapshot.AdaptorProgramDeploySlot = voltrProgramDeploySlot, adaptorProgramDeploySlot
		rpc, client = autoCleanupRPC(t, 42, accounts), autoCandidateJupiter(t, route)
	}
	putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(debt), 60))
	s := &o.Snapshot
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw = debt, debt, debt+100
	s.LTVBPS, s.LeverageTargetLevel = debt*10_000/s.PositionCollateralValueRaw, 1.5
	s.WithdrawalDemandRaw, s.VoltrIdleRaw = 10_000_000, 0
	s.VoltrStrategyIdleRaw, s.StagedAmountRaw, s.StagedAmountKnown, s.StageTransient = 11_000_000, 11_000_000, true, true
	s.CollateralIdleRaw, s.PrimeIdleRaw, s.DebtIdleRaw, s.SquadsIdleRaw = 0, 0, 0, 0
	s.StrategyNAVRaw, s.TotalVaultNAVRaw = s.PositionCollateralValueRaw-debt, s.PositionCollateralValueRaw-debt
	d := Decide(*s)
	if d.Action != VoltrRestoreIdle || d.Reason != "withdrawal_staged" {
		t.Fatalf("restore decision: %+v", d)
	}
	r := bridgeTestRequest(d.Action, uint64(d.AmountRaw))
	r.Report = BridgeReport{Sequence: uint64(s.Slot), ObservedSlot: uint64(s.Slot), NAVAfterRaw: uint64(s.StrategyNAVRaw), SnapshotDigest: s.ReportSnapshotDigest}
	effects, _, _, err := bridgeExpectedEffects(rwa, d, uint64(s.VoltrIdleRaw), uint64(s.VoltrStrategyIdleRaw), uint64(s.SquadsIdleRaw))
	if err != nil {
		t.Fatal(err)
	}
	effects.Kind, effects.ReturnData = "bridge", expectedAdaptorReturnData(rwa, r.Report.NAVAfterRaw)
	return o, d, BridgeExecutionEvidence{r, effects}, m, rpc, client, accounts
}

// autoSourceReleasedBatch is the common zero-idle case: no collateral and no
// debt sit in custody, so the payoff must be funded by a PILOT RELEASE of
// excess position collateral (release -> funding -> repay -> remaining
// withdrawal -> conversions -> stage/restore). The reserve's loan-to-value
// byte and the market's global allowed borrow value give the release risk
// model its reviewed room; every other byte stays the default owned batch.
func autoSourceReleasedBatch(route RuntimeRoute) func([]ConfirmedAccount) {
	return func(accounts []ConfirmedAccount) {
		autoSourceFundedBatch(route)(accounts)
		binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 0)
		accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoLoanToValueOffset] = 60
	}
}

func autoCleanupRPC(t *testing.T, slot int64, accounts []ConfirmedAccount) *chain.Client {
	t.Helper()
	return autoPayoffRPC(t, slot, append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, autoAUTOPYUSD)...))
}

// autoSourceFundedBatch mutates the default owned-position batch so the
// funding trace is solvent: the collateral custody holds 5e9 nine-dec tokens
// (7_500_000 micro-USDC at the 1.5 price — above the funding margin's need of
// about 7.14e6 against the ~7.0e6 PYUSD payoff), and the idle PYUSD custody is
// emptied so the source gate's DebtIdleRaw==0 contract holds and debt cash
// starts at zero.
func autoSourceFundedBatch(route RuntimeRoute) func([]ConfirmedAccount) {
	return func(accounts []ConfirmedAccount) {
		binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 5_000_000_000)
		binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 0)
	}
}
