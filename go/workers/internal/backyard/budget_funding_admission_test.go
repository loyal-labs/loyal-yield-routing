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
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

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
	for _, table := range retainedJupiterLookups(t) {
		// Controlled clock fixture only: preserve every retained lookup key.
		binary.LittleEndian.PutUint64(table.Data[12:20], 41)
		extra = append(extra, ConfirmedAccount{Address: table.Address, Owner: table.Owner, Lamports: table.Lamports, Data: table.Data})
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
	e.Request.FullPayoffFunding = true
	return o, d, e, m, rpc, client, accounts
}

func TestUSDCFundingAdmissionPricesReturnWithoutDoubleCountingSpentCash(t *testing.T) {
	t.Parallel()
	o, d, e, m, rpc, client, accounts := fundingAdmissionFixtureForSource(t, 20_000, SwapUSDCToDebtStep)
	plan, err := observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, d, e.Request, e.ExpectedEffects)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Exit) != 13 || plan.Payoff == nil || plan.Payoff.ThroughUnix != 1180 || plan.PayoffRepayment == nil || plan.ValidThroughSlot > 74 {
		t.Fatal("USDC funding omitted full interest/return pricing")
	}
	current, _, _, err := plan.Input.decode()
	if err != nil || !reflect.DeepEqual(current, e.Request) || plan.Snapshot != o.Snapshot {
		t.Fatal("funding projection changed persisted current input", err)
	}
	var upper uint64
	var quoteEffects ExpectedEffects
	for _, step := range plan.Exit {
		if step.Action != SwapCollateralToStableStep && step.Action != SwapDebtToUSDCStep {
			continue
		}
		request, effects, _, err := step.Template.decode()
		if err != nil {
			t.Fatal(err)
		}
		if step.Action == SwapCollateralToStableStep {
			quoteEffects = effects
		}
		estimate, err := withdrawalUSDCExitEstimate(request.(JupiterSwapRequest).QuotedOutputRaw)
		if err != nil {
			t.Fatal(err)
		}
		upper += estimate
	}
	if len(quoteEffects.Accounts) != 2 || quoteEffects.Accounts[1].Address != bridgeSquadsATA || quoteEffects.Accounts[1].BeforeRaw != 0 {
		t.Fatal("spent USDC was still counted as return custody")
	}
	var total int64
	for _, step := range plan.Exit {
		total += step.Cost.TotalMicros
		if (step.Action == StageSquadsToVoltr || step.Action == VoltrRestoreIdle) && step.Amount != upper {
			t.Fatal("bridge restoration double-counted funding input", step.Amount, upper)
		}
	}
	if total != plan.ExitAfterMicros || binary.LittleEndian.Uint64(accountAt(accounts, bridgeSquadsATA).Data[64:72]) != 20_000 {
		t.Fatal("cost-only projection changed actual USDC custody or lost costs")
	}
	nav := bridgeTestRequest(ReportNAV, 0)
	nav.Report.Sequence, nav.Report.ObservedSlot = 42, 42
	nd := Decision{Action: ReportNAV, StrategyKey: o.Snapshot.RouteLane}
	ne, _, _, err := bridgeExpectedEffects(nd, 0, 0, 20_000)
	if err != nil {
		t.Fatal(err)
	}
	before, err := observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, nd, nav, ne)
	if err != nil || len(before.Exit) != 14 || before.Exit[0].Action != SwapUSDCToDebtStep || before.Payoff.ThroughUnix != 1240 {
		t.Fatal("NAV before USDC funding lost its funding graph", err)
	}
	// A later observed NAV has sufficient debt cash. Unspent bridge cash is
	// preserved for return, not swapped a second time merely because it exists.
	o.Snapshot.DebtIdleRaw = 20_900
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 20_900)
	after, err := observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, nd, nav, ne)
	if err != nil || len(after.Exit) != 12 || after.Exit[0].Action != DeleverRouteStep {
		t.Fatal("funded NAV attempted a redundant USDC conversion", err)
	}
	for _, step := range after.Exit {
		if step.Action == SwapUSDCToDebtStep {
			t.Fatal("funded NAV priced a redundant USDC conversion")
		}
	}
}

func TestUSDCFundingRejectsChangedCashAndUnderfunding(t *testing.T) {
	t.Parallel()
	o, d, e, m, rpc, client, accounts := fundingAdmissionFixtureForSource(t, 20_000, SwapUSDCToDebtStep)
	// The persisted funding swap passes the build and send prestate against
	// its own custody, and refuses changed custody.
	if err := validateBuildPrestate(context.Background(), rpc, fixtureView(t, rpc), e.Request, e.ExpectedEffects); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 19_999)
	assertBudgetHold(t, validateBuildPrestate(context.Background(), rpc, fixtureView(t, rpc), e.Request, e.ExpectedEffects), "funding_custody_changed")
	_, err := observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, d, e.Request, e.ExpectedEffects)
	assertBudgetHold(t, err, "funding_custody_changed")
	o, d, e, m, rpc, client, accounts = fundingAdmissionFixtureForSource(t, 20_000, SwapUSDCToDebtStep)
	putScaledFraction(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(30_000), 60))
	o.Snapshot.PositionDebtRaw = 30_000
	_, err = observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, d, e.Request, e.ExpectedEffects)
	assertBudgetHold(t, err, "funding_quote_cannot_cover_full_payoff")
}

func TestFundingAdmissionPricesPayoffReturnAndBothNAVContinuations(t *testing.T) {
	t.Parallel()
	o, d, e, m, rpc, client, accounts := fundingAdmissionFixture(t, 20_000)
	plan, err := observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, d, e.Request, e.ExpectedEffects)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{ReportNAV, DeleverRouteStep, ReportNAV, DeleverRouteStep, ReportNAV, SwapCollateralToStableStep, ReportNAV, SwapDebtToUSDCStep, ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}
	var actions []Action
	var total int64
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
		total += step.Cost.TotalMicros
	}
	if !reflect.DeepEqual(actions, want) || total != plan.ExitAfterMicros || plan.Payoff == nil || plan.Payoff.ThroughUnix != 1180 || plan.PayoffRepayment == nil || plan.ValidThroughSlot > 74 {
		t.Fatal("funding omitted complete return or extended current-wire freshness", actions, plan.Payoff)
	}
	if binary.LittleEndian.Uint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72]) != 20_000_000 {
		t.Fatal("cost projection mutated observed custody")
	}
	current, _, _, _ := plan.Input.decode()
	if current.(JupiterSwapRequest).AmountRaw != e.Request.AmountRaw {
		t.Fatal("future payoff became current wire")
	}
	// NAV before funding must also reserve the actual swap and all 13 exits.
	nav := bridgeTestRequest(ReportNAV, 0)
	nav.Report.Sequence, nav.Report.ObservedSlot = 42, 42
	nd := Decision{Action: ReportNAV, StrategyKey: o.Snapshot.RouteLane}
	ne, _, _, err := bridgeExpectedEffects(nd, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	before, err := observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, nd, nav, ne)
	if err != nil || len(before.Exit) != 14 || before.Exit[0].Action != SwapCollateralToDebtStep || before.Payoff.ThroughUnix != 1240 {
		t.Fatal("NAV before funding stalls", err)
	}
	// Actual observed post-swap custody, not the optimistic cost template.
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.DebtIdleRaw = 0, 0, 20_900
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 20_900)
	after, err := observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, nd, nav, ne)
	if err != nil || len(after.Exit) != 12 || after.Exit[0].Action != DeleverRouteStep || after.Payoff.ThroughUnix != 1120 {
		t.Fatal("NAV after funding stalls", err)
	}
}

func TestFundingAdmissionRejectsUnderfundingAndFinalSendDrift(t *testing.T) {
	t.Parallel()
	o, d, e, m, rpc, client, accounts := fundingAdmissionFixture(t, 20_000)
	// A declared threshold above the wire's slippage-adjusted minimum must
	// not masquerade as enforceable repayment funding.
	e.Request.MinimumOutputRaw = e.Request.QuotedOutputRaw
	minimum := uint64(o.Snapshot.DebtIdleRaw) + e.Request.MinimumOutputRaw
	e.ExpectedEffects.Accounts[1].AfterRaw = minimum
	e.ExpectedEffects.Accounts[1].MinimumAfterRaw = &minimum
	_, err := observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, d, e.Request, e.ExpectedEffects)
	assertBudgetHold(t, err, "funding_quote_cannot_cover_full_payoff")
	_, _, e, _, rpc, _, accounts = fundingAdmissionFixture(t, 20_000)
	if err := validateBuildPrestate(context.Background(), rpc, fixtureView(t, rpc), e.Request, e.ExpectedEffects); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 999)
	if err := validateBuildPrestate(context.Background(), rpc, fixtureView(t, rpc), e.Request, e.ExpectedEffects); err == nil {
		t.Fatal("changed signed funding custody passed the send prestate")
	}
	o, d, e, m, rpc, client, accounts = fundingAdmissionFixture(t, 20_000)
	// Increase actual observed debt so the executable minimum cannot repay.
	putScaledFraction(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(30_000), 60))
	o.Snapshot.PositionDebtRaw = 30_000
	_, err = observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, d, e.Request, e.ExpectedEffects)
	assertBudgetHold(t, err, "funding_quote_cannot_cover_full_payoff")
}

// The route observer prices Snapshot.PositionDebtRaw on the unsigned
// reserve-refresh simulation bank whenever raw reserves are health-stale. A
// payoff window re-captured raw still prices the same unmutated obligation at
// the last on-chain refresh's older cumulative borrow rate; once that basis
// difference represents one unit of accrual, the whole-unit ceil straddles an
// integer and a strict debt comparison refuses funding after ordinary
// interest. Admission must re-derive the window over the identical simulation
// so both sides measure executable debt on one basis — while a genuinely
// mutated obligation principal still fails the exact comparison.
func TestFundingAdmissionBindsDebtComparisonToSnapshotReserveBasis(t *testing.T) {
	t.Parallel()
	nav := bridgeTestRequest(ReportNAV, 0)
	nav.Report.Sequence, nav.Report.ObservedSlot = 42, 42
	decision := func(o Observation) Decision { return Decision{Action: ReportNAV, StrategyKey: o.Snapshot.RouteLane} }

	simulatedDebtBank := func(t *testing.T, rpc *chain.Client, accounts []ConfirmedAccount, obligationDebtRaw uint64) *bool {
		t.Helper()
		route := ethenaUSDePYUSD
		bank := map[string]ConfirmedAccount{}
		for _, a := range accounts {
			bank[a.Address] = a
		}
		// The fixture returns its slice without the collateral reserve the rpc
		// mock serves internally; rebuild it with the same shape.
		if _, ok := bank[route.Kamino.CollateralReserve]; !ok {
			reserve := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 42, new(big.Int).Lsh(big.NewInt(1), 60), 1_000_000_000, 1_000_000_000)
			putKey(t, reserve.Data[32:64], route.Kamino.Market)
			binary.LittleEndian.PutUint64(reserve.Data[272:280], 9)
			binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
			bank[reserve.Address] = reserve
		}
		reserve := append([]byte(nil), bank[route.Kamino.DebtReserve].Data...)
		// One step of reserve accrual: raw basis ceils to 1000, refreshed
		// basis to 1001. LastUpdate moves to the capture slot like a real
		// refresh would.
		putScaledFraction(reserve[296:328], new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 60), big.NewInt(1024)))
		binary.LittleEndian.PutUint64(reserve[16:24], 42)
		obligation := append([]byte(nil), bank[route.Kamino.Obligation].Data...)
		putScaledFraction(obligation[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(obligationDebtRaw), 60))
		mutated := false
		previous := rpcOf(rpc).Transport
		rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read rpc body: %v", err)
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			var payload struct {
				Method string            `json:"method"`
				Params []json.RawMessage `json:"params"`
			}
			if json.Unmarshal(body, &payload) != nil || payload.Method != "simulateTransaction" {
				return previous.RoundTrip(req)
			}
			var wire string
			var config struct {
				Accounts struct {
					Addresses []string `json:"addresses"`
				} `json:"accounts"`
			}
			if err := json.Unmarshal(payload.Params[0], &wire); err != nil {
				t.Fatalf("decode simulation wire: %v", err)
			}
			if err := json.Unmarshal(payload.Params[1], &config); err != nil {
				t.Fatalf("decode simulation config: %v", err)
			}
			if len(wire) < 65 {
				t.Fatal("simulation wire lost its unsigned envelope")
			}
			rows := make([]string, 0, len(config.Accounts.Addresses))
			for _, address := range config.Accounts.Addresses {
				source, ok := bank[address]
				if !ok {
					t.Fatalf("simulated refresh captured unknown account %s", address)
				}
				data := source.Data
				switch address {
				case route.Kamino.DebtReserve:
					data = reserve
				case route.Kamino.Obligation:
					data = obligation
					if mutated {
						data = append([]byte(nil), obligation...)
						putScaledFraction(data[1296:1312], new(big.Int).Lsh(big.NewInt(2_000), 60))
					}
				}
				encoded := base64.StdEncoding.EncodeToString(data)
				rows = append(rows, `{"owner":"`+source.Owner+`","lamports":`+strconv.FormatUint(source.Lamports, 10)+`,"executable":false,"data":["`+encoded+`","base64"]}`)
			}
			body2 := `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":42},"value":{"err":null,"accounts":[` + strings.Join(rows, ",") + `]}}}`
			return response(body2), nil
		})
		return &mutated
	}

	o, _, _, m, rpc, client, accounts := fundingAdmissionFixture(t, 20_000)
	// The snapshot's position debt was priced on the refreshed bank: 1001.
	o.ValuationSource, o.Snapshot.ValuationSource = routeRefreshValuationSource, routeRefreshValuationSource
	o.ValuationSlot, o.Snapshot.ValuationSlot = 42, 42
	o.Snapshot.PositionDebtRaw = 1_001
	mutated := simulatedDebtBank(t, rpc, accounts, 1_000)
	ne, _, _, err := bridgeExpectedEffects(decision(o), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, decision(o), nav, ne)
	if err != nil || len(plan.Exit) != 14 || plan.Exit[0].Action != SwapCollateralToDebtStep || plan.Payoff == nil || plan.Payoff.ObservedDebtRaw != 1_001 {
		t.Fatal("refreshed-basis snapshot refused for accrued reserve basis alone", len(plan.Exit), plan.Payoff, err)
	}
	// A genuinely mutated obligation principal must still fail the exact
	// comparison even through the coherent refreshed bank.
	*mutated = true
	_, err = observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, decision(o), nav, ne)
	assertBudgetHold(t, err, "funding_debt_snapshot_changed")

	// A confirmed raw snapshot keeps the raw window: no simulation, no basis
	// shift, and the raw-basis debt still compares exactly.
	o, _, _, m, rpc, client, _ = fundingAdmissionFixture(t, 20_000)
	o.Snapshot.PositionDebtRaw = 1_000
	ne, _, _, err = bridgeExpectedEffects(decision(o), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = observePhase3FundingAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, decision(o), nav, ne)
	if err != nil || plan.Payoff == nil || plan.Payoff.ObservedDebtRaw != 1_000 {
		t.Fatal("confirmed raw snapshot lost its raw payoff basis", plan.Payoff, err)
	}
}

func TestFundingPayoffWindowIncludesInterveningSteps(t *testing.T) {
	t.Parallel()
	_, _, e, _, rpc, _, accounts := fundingAdmissionFixture(t, 20_000)
	putScaledFraction(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(1_000_000), 60))
	short, err := decodeKaminoPayoffWindow(accounts, ethenaUSDePYUSD, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	long, err := decodeKaminoPayoffWindow(accounts, ethenaUSDePYUSD, 42, 4)
	if err != nil || long.UpperDebtRaw <= short.UpperDebtRaw {
		t.Fatal("intervening interest not priced", short, long, err)
	}
	e.Request.QuotedOutputRaw = short.UpperDebtRaw - 1_000
	e.Request.MinimumOutputRaw = e.Request.QuotedOutputRaw
	wire, _ := base64.StdEncoding.Strict().DecodeString(e.Request.Instruction.Data)
	binary.LittleEndian.PutUint64(wire[jupiter.V2QuotedOutOffset:], e.Request.QuotedOutputRaw)
	binary.LittleEndian.PutUint16(wire[jupiter.V2SlippageOffset:], 0)
	e.Request.Instruction.Data = base64.StdEncoding.EncodeToString(wire)
	minimum := short.UpperDebtRaw
	e.ExpectedEffects.Accounts[1].AfterRaw = minimum
	e.ExpectedEffects.Accounts[1].MinimumAfterRaw = &minimum
	embedded, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := validatePayoffFunding(context.Background(), rpc, fixtureView(t, rpc), embedded, e.Request, e.ExpectedEffects, 42, 1, false); err != nil {
		t.Fatal("immediate payoff should be funded", err)
	}
	_, _, err = validatePayoffFunding(context.Background(), rpc, fixtureView(t, rpc), embedded, e.Request, e.ExpectedEffects, 42, 4, false)
	assertBudgetHold(t, err, "funding_quote_cannot_cover_full_payoff")
}
