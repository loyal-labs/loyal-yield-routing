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
	"testing"
)

func debtResidueAdmissionFixture(t *testing.T, debtOutput uint64, extraAccounts ...ConfirmedAccount) (Observation, Decision, KaminoExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient) {
	t.Helper()
	route := ethenaUSDePYUSD
	binding, err := catalogJupiterBindingForRoute(SwapDebtToUSDCStep, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	reserve := reserveFixture(t, route.Kamino.DebtReserve, route.Kamino.DebtMint, 42, new(big.Int).Lsh(big.NewInt(2), 60), 1_000_000, 1_000_000)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
	mint := ConfirmedAccount{Address: route.Kamino.DebtMint, Owner: token2022Program, Lamports: 1, Data: make([]byte, 82)}
	mint.Data[44], mint.Data[45] = 6, 1
	var installed struct {
		Operations []struct{ PolicyAddress, DataBase64 string }
	}
	data, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/policy-install-readback-v1.json")
	if err != nil || json.Unmarshal(data, &installed) != nil {
		t.Fatal("policy evidence missing", err)
	}
	extra := []ConfirmedAccount{reserve, mint}
	for _, p := range installed.Operations {
		if p.PolicyAddress == binding.Policy {
			data, err := base64.StdEncoding.Strict().DecodeString(p.DataBase64)
			if err != nil {
				t.Fatal(err)
			}
			extra = append(extra, ConfirmedAccount{Address: p.PolicyAddress, Owner: bridgeSquadsProgram, Lamports: 1, Data: data})
		}
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
	data, err = os.ReadFile("../../../../docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json")
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
	if err != nil || len(data) <= binding.FeeOffset {
		t.Fatal("missing debt exit bytes")
	}
	binary.LittleEndian.PutUint64(data[binding.AmountOffset:], 10_000)
	binary.LittleEndian.PutUint64(data[binding.AmountOffset+8:], debtOutput)
	binary.LittleEndian.PutUint16(data[binding.SlippageOffset:], 50)
	instruction.Data = base64.StdEncoding.EncodeToString(data)
	o, d, e, m, rpc, client := withdrawalAdmissionFixture(t, 100_000, append(extra, extraAccounts...)...)
	o.Snapshot.DebtIdleRaw = 10_000
	previous := client.http.Transport
	debtQuote := false
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
			payload = JupiterQuote{InputMint: route.Kamino.DebtMint, OutputMint: bridgeUSDC, InAmount: "10000", OutAmount: fmt.Sprint(debtOutput), OtherAmountThreshold: fmt.Sprint(debtOutput), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
		} else {
			payload = map[string]any{"swapInstruction": instruction}
		}
		encoded, _ := json.Marshal(payload)
		return response(string(encoded)), nil
	})
	return o, d, e, m, rpc, client
}

func TestDebtFreeReturnReservesBothCollateralAndDebtResidue(t *testing.T) {
	o, d, e, m, rpc, client := debtResidueAdmissionFixture(t, 20_000)
	plan, err := observePhase3WithdrawalAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	actions := []Action{}
	var total int64
	for _, s := range plan.Exit {
		actions = append(actions, s.Action)
		total += s.Cost.TotalMicros
	}
	if !reflect.DeepEqual(actions, []Action{ReportNAV, SwapCollateralToStableStep, ReportNAV, SwapDebtToUSDCStep, ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}) || total != plan.ExitAfterMicros || len(plan.AdditionalQuotedExits) != 1 {
		t.Fatal("omitted debt exit or NAV", actions)
	}
	request, _, _, err := plan.AdditionalQuotedExits[0].Input.decode()
	if err != nil {
		t.Fatal(err)
	}
	r := request.(JupiterSwapRequest)
	if r.Action != SwapDebtToUSDCStep || r.AmountRaw != 10_000 {
		t.Fatal("wrong debt quote")
	}
	// The second cost template assumes a conservative first-swap output.
	// It cannot become the current wire after a different actual output.
	post := o
	post.Snapshot.HasPosition = false
	post.Snapshot.PositionCollateralRaw, post.Snapshot.PositionCollateralValueRaw = 0, 0
	post.Snapshot.SquadsIdleRaw = 100_000
	_, prospectiveEffects, _, err := plan.AdditionalQuotedExits[0].Input.decode()
	if err != nil {
		t.Fatal(err)
	}
	_, err = observePhase3CollateralReturnAdmission(context.Background(), rpc, nil, m, post, Decision{Action: SwapDebtToUSDCStep, AmountRaw: 10_000, StrategyKey: o.Snapshot.RouteLane}, r, prospectiveEffects)
	assertBudgetHold(t, err, "collateral_return_custody_mismatch")
	for _, s := range plan.Exit {
		if s.Action == StageSquadsToVoltr || s.Action == VoltrRestoreIdle {
			if s.Amount != plan.QuotedExit.EstimatedUpperOutputRaw+plan.AdditionalQuotedExits[0].EstimatedUpperOutputRaw {
				t.Fatal("full custody restore omitted an asset")
			}
		}
	}
	// Aggregate output, not either quote alone, must fit each full restoration.
	o, d, e, m, rpc, client = debtResidueAdmissionFixture(t, 900_000)
	_, err = observePhase3WithdrawalAdmission(context.Background(), rpc, client, m, o, d, e)
	assertBudgetHold(t, err, "bridge_exit_or_transaction_cap_exceeded")
}

func TestDebtResidueAdmissionContinuesFromNAVThroughActualSwap(t *testing.T) {
	o, _, _, m, rpc, client := debtResidueAdmissionFixture(t, 20_000)
	o.Snapshot.HasPosition = false
	o.Snapshot.PositionCollateralRaw, o.Snapshot.PositionCollateralValueRaw = 0, 0
	o.Snapshot.SquadsIdleRaw = 100_000
	report := bridgeTestRequest(ReportNAV, 0)
	report.Report.Sequence, report.Report.ObservedSlot, report.Report.NAVAfterRaw = 42, 42, 120_000
	d := Decision{Action: ReportNAV, StrategyKey: o.Snapshot.RouteLane}
	effects, _, _, err := bridgeExpectedEffects(d, 0, 0, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	effects.Kind, effects.ReturnData = "bridge", expectedAdaptorReturnData(120_000)
	plan, err := observePhase3CollateralReturnAdmission(context.Background(), rpc, client, m, o, d, report, effects)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Exit) != 6 || plan.Exit[0].Action != SwapDebtToUSDCStep {
		t.Fatal("debt-only NAV has no full exit", plan.Exit)
	}
	request, swapEffects, _, err := plan.QuotedExit.Input.decode()
	if err != nil {
		t.Fatal(err)
	}
	d.Action, d.AmountRaw = SwapDebtToUSDCStep, 10_000
	plan, err = observePhase3CollateralReturnAdmission(context.Background(), rpc, nil, m, o, d, request, swapEffects)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Input.Kind != "jupiter" || len(plan.Exit) != 5 || plan.Exit[0].Action != ReportNAV || plan.CurrentCost.PrincipalMicros <= 20_000 {
		t.Fatal("debt swap lost nonpeg cost or terminal return")
	}
	b := emptyTestBudget()
	b.Families["Ethena"] = FamilyBudget{ExitMicros: 1_000_000}
	intent, err := Phase3IntentDigest(request, plan.Input.Effects)
	if err != nil {
		t.Fatal(err)
	}
	reservation := BudgetReservation{OperationID: "debt-residue", Family: "Ethena", IntentSHA256: intent, UpperMicros: plan.CurrentCost.TotalMicros, ExitAfterMicros: plan.ExitAfterMicros, Recovery: true}
	if err = b.Admit(reservation); err != nil {
		t.Fatal(err)
	}
	if err = b.Settle(reservation.OperationID, intent, reservation.UpperMicros); err != nil {
		t.Fatal(err)
	}
	if b.Families["Ethena"].ExitMicros != plan.ExitAfterMicros || b.Families["Ethena"].SpentMicros != plan.CurrentCost.TotalMicros {
		t.Fatal("debt residue reset spent or exit reservation")
	}
	for _, mutate := range []func(*Observation){func(o *Observation) { o.Snapshot.PositionDebtRaw = 1 }, func(o *Observation) { o.Snapshot.DebtIdleRaw++ }, func(o *Observation) { o.Snapshot.CollateralIdleRaw = 1; o.Snapshot.PrimeIdleRaw = 1 }} {
		bad := o
		mutate(&bad)
		if _, err := observePhase3CollateralReturnAdmission(context.Background(), nil, nil, m, bad, d, request, swapEffects); err == nil {
			t.Fatal("incomplete or mismatched return admitted")
		}
	}
}
