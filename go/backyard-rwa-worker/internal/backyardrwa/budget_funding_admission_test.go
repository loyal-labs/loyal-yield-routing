package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"testing"
)

func fundingAdmissionFixture(t *testing.T, output uint64) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	return fundingAdmissionFixtureForSource(t, output, SwapCollateralToDebtStep)
}

func fundingAdmissionFixtureForSource(t *testing.T, output uint64, fundingAction Action) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
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
				Accounts              []JupiterInstructionAccount
			}
		}
	}
	data, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json")
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
	client, err := newJupiterClient("https://jupiter.invalid", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
			binding, err := catalogJupiterBindingForRoute(action, route.Lane)
			if err != nil {
				t.Fatal(err)
			}
			instruction = instructions[action]
			wire, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
			if err != nil || len(wire) <= binding.FeeOffset {
				t.Fatal("missing edge wire", action, err)
			}
			binary.LittleEndian.PutUint64(wire[binding.AmountOffset:], amount)
			binary.LittleEndian.PutUint64(wire[binding.AmountOffset+8:], out)
			binary.LittleEndian.PutUint16(wire[binding.SlippageOffset:], 50)
			instruction.Data = base64.StdEncoding.EncodeToString(wire)
			payload = JupiterQuote{InputMint: q.Get("inputMint"), OutputMint: q.Get("outputMint"), InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(out), OtherAmountThreshold: fmt.Sprint(out * 9950 / 10000), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
		} else {
			payload = map[string]any{"swapInstruction": instruction}
		}
		encoded, _ := json.Marshal(payload)
		return response(string(encoded)), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	d := Decision{Action: SwapCollateralToDebtStep, AmountRaw: o.Snapshot.CollateralIdleRaw, StrategyKey: route.Lane, Reason: "withdrawal_swap_repayment_buffer", IdempotencyKey: "funding-return"}
	if fundingAction == SwapUSDCToDebtStep {
		d.Action, d.AmountRaw, d.Reason = fundingAction, o.Snapshot.SquadsIdleRaw, "withdrawal_usdc_repayment_buffer"
	}
	e, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, uint64(d.AmountRaw), uint64(o.Snapshot.DebtIdleRaw), 42)
	if err != nil {
		t.Fatal(err)
	}
	e.Request.FullPayoffFunding = true
	return o, d, e, m, rpc, client, accounts
}

func TestUSDCFundingAdmissionReservesReturnWithoutDoubleCountingSpentCash(t *testing.T) {
	o, d, e, m, rpc, client, accounts := fundingAdmissionFixtureForSource(t, 20_000, SwapUSDCToDebtStep)
	plan, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Exit) != 13 || plan.Payoff == nil || plan.Payoff.ThroughUnix != 1180 || plan.PayoffRepayment == nil || plan.PayoffWithdrawal == nil || plan.FundingSwap == nil || plan.ValidThroughSlot > 74 {
		t.Fatal("USDC funding omitted full interest/return reservation")
	}
	current, _, _, err := plan.Input.decode()
	if err != nil || !reflect.DeepEqual(current, e.Request) || plan.Snapshot != o.Snapshot {
		t.Fatal("funding projection changed persisted current input", err)
	}
	_, quoteEffects, _, err := plan.QuotedExit.Input.decode()
	if err != nil || quoteEffects.Accounts[1].Address != bridgeSquadsATA || quoteEffects.Accounts[1].BeforeRaw != 0 {
		t.Fatal("spent USDC was still counted as return custody", err)
	}
	upper := plan.QuotedExit.EstimatedUpperOutputRaw + plan.AdditionalQuotedExits[0].EstimatedUpperOutputRaw
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
	before, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, nd, nav, ne)
	if err != nil || len(before.Exit) != 14 || before.Exit[0].Action != SwapUSDCToDebtStep || before.Payoff.ThroughUnix != 1240 {
		t.Fatal("NAV before USDC funding lost its funding graph", err)
	}
	// A later observed NAV has sufficient debt cash. Unspent bridge cash is
	// preserved for return, not swapped a second time merely because it exists.
	o.Snapshot.DebtIdleRaw = 20_900
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 20_900)
	after, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, nd, nav, ne)
	if err != nil || len(after.Exit) != 12 || after.FundingSwap != nil || after.Exit[0].Action != DeleverRouteStep {
		t.Fatal("funded NAV attempted a redundant USDC conversion", err)
	}
}

func TestUSDCFundingRejectsChangedCashUnderfundingAndUnreservedReturn(t *testing.T) {
	o, d, e, m, rpc, client, accounts := fundingAdmissionFixtureForSource(t, 20_000, SwapUSDCToDebtStep)
	encoded, _ := jsonMarshalExpectedEffects(e.ExpectedEffects)
	input, _ := encodePhase3BuildInput(e.Request, encoded)
	intent, _ := Phase3IntentDigest(e.Request, encoded)
	message, _ := CompileJupiterMessage(e.Request)
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
	binding := phase3OperationAuthorization{GoalID: Phase3GoalID, BuildInput: input, IntentSHA256: intent, SignedWireSHA256: op.SignedWireSHA256}
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, binding, op); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 19_999)
	_, err := revaluePhase3SignedInput(context.Background(), rpc, binding, op)
	assertBudgetHold(t, err, "funding_custody_changed")
	_, err = observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects)
	assertBudgetHold(t, err, "funding_custody_changed")
	for _, output := range []uint64{20_000, 900_000} {
		o, d, e, m, rpc, client, accounts := fundingAdmissionFixtureForSource(t, output, SwapUSDCToDebtStep)
		if output == 20_000 {
			putScaledFraction(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(30_000), 60))
			o.Snapshot.PositionDebtRaw = 30_000
		}
		_, err := legacyAdmissionCostCheck(observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects))
		if output == 20_000 {
			assertBudgetHold(t, err, "funding_quote_cannot_cover_full_payoff")
		} else {
			assertBudgetHold(t, err, "bridge_exit_or_transaction_cap_exceeded")
		}
	}
}

func TestFundingAdmissionReservesPayoffReturnAndBothNAVContinuations(t *testing.T) {
	o, d, e, m, rpc, client, accounts := fundingAdmissionFixture(t, 20_000)
	plan, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects)
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
	if !reflect.DeepEqual(actions, want) || total != plan.ExitAfterMicros || plan.Payoff == nil || plan.Payoff.ThroughUnix != 1180 || plan.PayoffRepayment == nil || plan.PayoffWithdrawal == nil || plan.FundingSwap == nil || plan.ValidThroughSlot > 74 {
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
	before, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, nd, nav, ne)
	if err != nil || len(before.Exit) != 14 || before.Exit[0].Action != SwapCollateralToDebtStep || before.Payoff.ThroughUnix != 1240 {
		t.Fatal("NAV before funding stalls", err)
	}
	// Actual observed post-swap custody, not the optimistic cost template.
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.DebtIdleRaw = 0, 0, 20_900
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 20_900)
	after, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, nd, nav, ne)
	if err != nil || len(after.Exit) != 12 || after.Exit[0].Action != DeleverRouteStep || after.Payoff.ThroughUnix != 1120 || after.FundingSwap != nil {
		t.Fatal("NAV after funding stalls", err)
	}
}

func TestFundingAdmissionRejectsUnderfundingAndFinalSendDrift(t *testing.T) {
	o, d, e, m, rpc, client, accounts := fundingAdmissionFixture(t, 20_000)
	// A declared threshold above the wire's slippage-adjusted minimum must
	// not masquerade as enforceable repayment funding.
	e.Request.MinimumOutputRaw = e.Request.QuotedOutputRaw
	minimum := uint64(o.Snapshot.DebtIdleRaw) + e.Request.MinimumOutputRaw
	e.ExpectedEffects.Accounts[1].AfterRaw = minimum
	e.ExpectedEffects.Accounts[1].MinimumAfterRaw = &minimum
	_, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects)
	assertBudgetHold(t, err, "funding_quote_cannot_cover_full_payoff")
	o, d, e, m, rpc, client, accounts = fundingAdmissionFixture(t, 20_000)
	encoded, _ := jsonMarshalExpectedEffects(e.ExpectedEffects)
	input, _ := encodePhase3BuildInput(e.Request, encoded)
	intent, _ := Phase3IntentDigest(e.Request, encoded)
	message, _ := CompileJupiterMessage(e.Request)
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
	binding := phase3OperationAuthorization{GoalID: Phase3GoalID, BuildInput: input, IntentSHA256: intent, SignedWireSHA256: op.SignedWireSHA256}
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, binding, op); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 999)
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, binding, op); err == nil {
		t.Fatal("changed signed funding custody passed final-send revaluation")
	}
	o, d, e, m, rpc, client, accounts = fundingAdmissionFixture(t, 20_000)
	// Increase actual observed debt so the executable minimum cannot repay.
	putScaledFraction(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(30_000), 60))
	o.Snapshot.PositionDebtRaw = 30_000
	_, err = observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects)
	assertBudgetHold(t, err, "funding_quote_cannot_cover_full_payoff")
	o, d, e, m, rpc, client, _ = fundingAdmissionFixture(t, 900_000)
	_, err = legacyAdmissionCostCheck(observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects))
	assertBudgetHold(t, err, "bridge_exit_or_transaction_cap_exceeded")
}

func TestFundingPayoffWindowIncludesInterveningSteps(t *testing.T) {
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
	b, _ := catalogJupiterBindingForRoute(e.Request.Action, e.Request.RouteLane)
	wire, _ := base64.StdEncoding.Strict().DecodeString(e.Request.Instruction.Data)
	binary.LittleEndian.PutUint64(wire[b.AmountOffset+8:], e.Request.QuotedOutputRaw)
	binary.LittleEndian.PutUint16(wire[b.SlippageOffset:], 0)
	e.Request.Instruction.Data = base64.StdEncoding.EncodeToString(wire)
	minimum := short.UpperDebtRaw
	e.ExpectedEffects.Accounts[1].AfterRaw = minimum
	e.ExpectedEffects.Accounts[1].MinimumAfterRaw = &minimum
	if _, _, err := validatePayoffFunding(context.Background(), rpc, e.Request, e.ExpectedEffects, 42, 1); err != nil {
		t.Fatal("immediate payoff should be funded", err)
	}
	_, _, err = validatePayoffFunding(context.Background(), rpc, e.Request, e.ExpectedEffects, 42, 4)
	assertBudgetHold(t, err, "funding_quote_cannot_cover_full_payoff")
}
