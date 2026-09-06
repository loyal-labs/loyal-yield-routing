package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func setupTestRequest(operation string) policySetupRequest {
	return policySetupRequest{Operation: operation, Seed: 170, RecentBlockhash: bridgeTestRequest(ReportNAV, 0).RecentBlockhash, LastValidBlockHeight: 100}
}

func TestPolicySetupCompilerMatchesInstalledSDKAndRetainedConstraints(t *testing.T) {
	type oracleInput struct {
		policySetupRequest
		Seed            string
		FundingLamports string
		Messages        []string
	}
	var inputs []oracleInput
	for _, operation := range []string{"borrow", "repay", "onre-entry-swap", "onre-return-swap"} {
		for _, seed := range []uint64{1, 170, 256, math.MaxUint64} {
			r := setupTestRequest(operation)
			r.Seed = seed
			messages, err := compilePolicySetupMessages(r, 5_317_440)
			if err != nil {
				t.Fatal(err)
			}
			inputs = append(inputs, oracleInput{r, strconv.FormatUint(seed, 10), "5317440", []string{base64.StdEncoding.EncodeToString(messages[0]), base64.StdEncoding.EncodeToString(messages[1])}})
		}
	}
	encoded, _ := json.Marshal(inputs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bun", "testdata/policy-setup-oracle.mjs")
	cmd.Stdin = bytes.NewReader(encoded)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("policy SDK oracle: %v %s", err, stderr.String())
	}
	var expected []struct {
		Policy, Data string
		Messages     []string
	}
	if json.Unmarshal(output, &expected) != nil || len(expected) != len(inputs) {
		t.Fatal("invalid policy oracle output")
	}
	for i, input := range inputs {
		r := input.policySetupRequest
		instruction, _, err := policySetupCreateInstruction(r)
		if err != nil || base64.StdEncoding.EncodeToString(instruction.data) != expected[i].Data {
			t.Fatalf("policy payload differs from retained constraints + SDK: %s %d %v", r.Operation, r.Seed, err)
		}
		policy, err := policySetupAddress(r.Seed)
		if err != nil || encodeBase58(policy[:]) != expected[i].Policy {
			t.Fatalf("policy PDA mismatch at seed %d: %v", r.Seed, err)
		}
		messages, err := compilePolicySetupMessages(r, 5_317_440)
		if err != nil {
			t.Fatal(err)
		}
		for j, message := range messages {
			if len(expected[i].Messages) != 2 || base64.StdEncoding.EncodeToString(message) != expected[i].Messages[j] {
				t.Fatalf("SDK message mismatch: %s seed %d payment %d", r.Operation, r.Seed, j)
			}
		}
	}
}

func setupTestFees(t *testing.T, r policySetupRequest, rent uint64) [2]MessageFeeObservation {
	t.Helper()
	messages, err := compilePolicySetupMessages(r, rent/2)
	if err != nil {
		t.Fatal(err)
	}
	return [2]MessageFeeObservation{{MessageSHA256: sha256Bytes(messages[0]), Slot: 42, Lamports: 5000}, {MessageSHA256: sha256Bytes(messages[1]), Slot: 42, Lamports: 5000}}
}

func TestPolicySetupCostReservesBothPaymentsWithoutResettingBudget(t *testing.T) {
	r := setupTestRequest("borrow")
	const rent uint64 = 10_634_881 // odd rent exercises the remaining lamport
	fees := setupTestFees(t, r, rent)
	price := budgetTestPrice(nativeSOLBudgetAsset, "11111111111111111111111111111111", 9, 100, 1)
	plan, err := valuePolicySetupPlan(r, rent, 890_880, fees, price, 42)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Costs[0].SetupLamports+plan.Costs[1].SetupLamports != rent || plan.TotalMicros != 1_064_489 || plan.RemainingMicros != 532_245 {
		t.Fatalf("rent/fees not fully reserved with conservative rounding: %+v", plan)
	}
	// Pure existing-ledger feasibility only: production may NOT substitute this
	// reserve for an open position's exit or send without a durable setup plan.
	b := emptyTestBudget()
	b.Families["OnRe"] = FamilyBudget{SpentMicros: 3_000_000}
	first := BudgetReservation{OperationID: "prefund", Family: "OnRe", IntentSHA256: sha256Bytes([]byte("fixed setup plan + prefund")), UpperMicros: plan.Costs[0].TotalMicros, ExitAfterMicros: plan.RemainingMicros}
	if err = b.Admit(first); err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(b)
	var restarted Phase3Budget
	if err = json.Unmarshal(wire, &restarted); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Admit(first); err != nil {
		t.Fatal(err)
	}
	second := BudgetReservation{OperationID: "create", Family: "OnRe", IntentSHA256: sha256Bytes([]byte("same fixed setup plan + create")), UpperMicros: plan.RemainingMicros, Recovery: true}
	assertBudgetHold(t, restarted.Admit(second), "unresolved_submission_reservation")
	if err = restarted.Settle(first.OperationID, first.IntentSHA256, first.UpperMicros); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Admit(second); err != nil {
		t.Fatal(err)
	}
	if err = restarted.releaseUnspent(second.OperationID, second.IntentSHA256); err != nil {
		t.Fatal(err)
	}
	if restarted.Families["OnRe"].ExitMicros != plan.RemainingMicros {
		t.Fatal("proven-unsent create lost completion reserve")
	}
	if err = restarted.Admit(second); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Settle(second.OperationID, second.IntentSHA256, second.UpperMicros); err != nil {
		t.Fatal(err)
	}
	if restarted.Families["OnRe"] != (FamilyBudget{SpentMicros: 3_000_000 + plan.TotalMicros}) {
		t.Fatal("setup spend reset, double counted or refunded")
	}
	b = emptyTestBudget()
	b.Families["OnRe"] = FamilyBudget{SpentMicros: Phase3FamilyCapMicros - plan.TotalMicros + 1}
	assertBudgetHold(t, b.Admit(first), "family_cap_exceeded")
}

func TestPolicySetupRejectsInvalidCandidatesCostsAndLiveBuildRegistration(t *testing.T) {
	r := setupTestRequest("borrow")
	price := budgetTestPrice(nativeSOLBudgetAsset, "11111111111111111111111111111111", 9, 100, 1)
	const rent = 10_634_880
	fees := setupTestFees(t, r, rent)
	_, err := valuePolicySetupPlan(r, rent, rent, fees, price, 42)
	assertBudgetHold(t, err, "policy_prefunding_not_rent_exempt")
	_, err = valuePolicySetupPlan(r, rent, 890_880, fees, price, 75)
	assertBudgetHold(t, err, "fee_message_or_slot_mismatch")
	changed := fees
	changed[1].Lamports = 0
	_, err = valuePolicySetupPlan(r, rent, 890_880, changed, price, 42)
	assertBudgetHold(t, err, "fee_message_or_slot_mismatch")
	changed = fees
	changed[1].MessageSHA256 = strings.Repeat("a", 64)
	_, err = valuePolicySetupPlan(r, rent, 890_880, changed, price, 42)
	assertBudgetHold(t, err, "fee_message_or_slot_mismatch")
	price = budgetTestPrice(nativeSOLBudgetAsset, "11111111111111111111111111111111", 9, 200, 1)
	_, err = valuePolicySetupPlan(r, rent, 890_880, fees, price, 42)
	assertBudgetHold(t, err, "transaction_cap_exceeded")
	r.Operation = "withdraw"
	_, err = compilePolicySetupMessages(r, rent/2)
	assertBudgetHold(t, err, "unmapped_policy_setup_candidate")
	r.Operation, r.Seed = "borrow", 0
	_, err = compilePolicySetupMessages(r, rent/2)
	assertBudgetHold(t, err, "invalid_policy_setup_seed")
	r = setupTestRequest("borrow")
	_, err = compilePolicySetupMessages(r, 0)
	assertBudgetHold(t, err, "invalid_policy_setup_funding")
	_, err = encodePhase3BuildInput(r, nil)
	assertBudgetHold(t, err, "unmapped_economic_action")
	input := phase3BuildInput{Kind: "policy-setup", Request: []byte(`{}`), Effects: []byte(`{}`)}
	_, _, _, err = input.decode()
	assertBudgetHold(t, err, "invalid_persisted_build_input")
}
