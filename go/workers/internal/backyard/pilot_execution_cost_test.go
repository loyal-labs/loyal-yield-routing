package backyard

import (
	"testing"
)

func pilotCostFixture(t *testing.T, request any, effects ExpectedEffects) ValuedTransactionCost {
	t.Helper()
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	input, err := encodePhase3BuildInput(request, encoded)
	if err != nil {
		t.Fatal(err)
	}
	_, _, message, err := input.decode()
	if err != nil {
		t.Fatal(err)
	}
	debit, err := MeasureExecutableDebit(request, effects)
	if err != nil {
		t.Fatal(err)
	}
	decimals := uint8(6)
	token := budgetTestPrice(debit.Mint, debit.TokenProgram, decimals, 1, 1)
	sol := budgetTestPrice(nativeSOLBudgetAsset, "11111111111111111111111111111111", 9, 100, 1)
	var rent uint64
	if r, ok := request.(KaminoInitializationRequest); ok {
		rent = r.RentLamports
	}
	cost, err := ValueTransactionCost(message, debit, MessageFeeObservation{MessageSHA256: sha256Bytes(message), Slot: 42, Lamports: 5000}, rent, token, sol, 42)
	if err != nil {
		t.Fatal(err)
	}
	return cost
}

func TestPilotExecutionCostPreservesPrincipalAndBindsExactValuation(t *testing.T) {
	_, _, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 500_000, 500_000, 0, 0)
	request, effects := evidence.Request, evidence.ExpectedEffects
	cost := pilotCostFixture(t, request, effects)
	bound, err := classifyPilotExecutionCost(request, effects, cost, nil)
	if err != nil || bound.TotalMicros != 500 || cost.PrincipalMicros != 500_000 {
		t.Fatalf("principal charged as expense: %+v %v", bound, err)
	}
	cost.PrincipalMicros--
	cost.TotalMicros--
	_, err = classifyPilotExecutionCost(request, effects, cost, nil)
	assertBudgetHold(t, err, "execution_cost_valuation_mismatch")
}

func TestPilotSwapCostUsesMinimumOutputAndLowerPrice(t *testing.T) {
	_, _, e, _, _, _, _ := entrySwapAdmissionFixture(t)
	cost := pilotCostFixture(t, e.Request, e.ExpectedEffects)
	dest := e.ExpectedEffects.Accounts[1]
	credit := budgetTestPrice(dest.Mint, dest.Owner, 6, 2, 1)
	credit.Credit = &BudgetCreditBounds{TokenLowerSF: budgetTestPrice("", "", 6, 1, 1).TokenUpperSF, USDCUpperSF: credit.USDCLowerSF}
	bound, err := classifyPilotExecutionCost(e.Request, e.ExpectedEffects, cost, &credit)
	want := max(0, cost.PrincipalMicros-int64(e.Request.MinimumOutputRaw))
	if err != nil || bound.SwapLossMicros != want || bound.CreditPrice == nil {
		t.Fatalf("did not use floor credit: %+v want %d: %v", bound, want, err)
	}
	_, err = classifyPilotExecutionCost(e.Request, e.ExpectedEffects, cost, nil)
	assertBudgetHold(t, err, "missing_swap_execution_cost_bound")
	credit.Credit = nil
	_, err = classifyPilotExecutionCost(e.Request, e.ExpectedEffects, cost, &credit)
	assertBudgetHold(t, err, "missing_credit_valuation_bounds")
	credit.Credit = &BudgetCreditBounds{TokenLowerSF: credit.TokenUpperSF, USDCUpperSF: credit.USDCLowerSF}
	credit.ValidThroughSlot = 41
	_, err = classifyPilotExecutionCost(e.Request, e.ExpectedEffects, cost, &credit)
	assertBudgetHold(t, err, "missing_stale_or_mismatched_usdc_valuation")
}

func TestPilotNativeInitializationBooksFeeAndRetainsRentAsAsset(t *testing.T) {
	effects, _ := initializationReconcileFixture(t)
	request := *effects.Initialization
	cost := pilotCostFixture(t, request, effects)
	bound, err := classifyPilotExecutionCost(request, effects, cost, nil)
	if err != nil || bound.TotalMicros != cost.NetworkFeeMicros || cost.SetupLamportsMicros <= bound.TotalMicros {
		t.Fatalf("rent was lost or counted as execution expense: %+v %v", bound, err)
	}
}

func TestPilotProtocolCostIncludesBorrowFeeAndRounding(t *testing.T) {
	_, _, borrow, _, _, _, _ := borrowAdmissionFixture(t, 20_000, "")
	_, _, deposit, _, _, _, _ := depositAdmissionFixture(t, "")
	_, _, payoff, _, _, _, _ := payoffAdmissionFixture(t, 20_000)
	for _, tc := range []struct {
		name     string
		evidence KaminoExecutionEvidence
	}{
		{"borrow fee", borrow}, {"deposit receipt rounding", deposit}, {"full repayment rounding", payoff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, e := tc.evidence.Request, tc.evidence.ExpectedEffects
			cost := pilotCostFixture(t, r, e)
			bound, err := classifyPilotExecutionCost(r, e, cost, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, leg, err := kaminoPrimeUSDCInstruction(r)
			if err != nil {
				t.Fatal(err)
			}
			var raw uint64
			switch leg {
			case kaminoLegBorrow:
				raw = cost.Debit.Raw - r.AmountRaw
			case kaminoLegDeposit:
				raw = e.Deposit.MaximumDebitRaw - e.Deposit.MinimumDebitRaw
			case kaminoLegRepay:
				raw = e.Repayment.MaximumDebitRaw - e.Repayment.MinimumDebitRaw + 1
			default:
				t.Fatal("unexpected fixture leg")
			}
			value, err := cost.TokenPrice.valueUpper(raw, cost.Debit.Mint, cost.Debit.TokenProgram, 42)
			if err != nil || raw == 0 || bound.ProtocolRoundingMicros != value || bound.TotalMicros != value+cost.NetworkFeeMicros {
				t.Fatalf("omitted fee/rounding: raw=%d %+v %v", raw, bound, err)
			}
		})
	}
}

func TestBudgetCreditRejectsInvertedIntervals(t *testing.T) {
	for _, side := range []string{"token", "usdc"} {
		p := budgetTestPrice("asset", classicTokenProgram, 6, 2, 2)
		p.Credit = &BudgetCreditBounds{TokenLowerSF: p.TokenUpperSF, USDCUpperSF: p.USDCLowerSF}
		if side == "token" {
			p.Credit.TokenLowerSF[0]++
		} else {
			p.Credit.USDCUpperSF[0]--
		}
		_, err := p.valueLower(100, "asset", classicTokenProgram, 42)
		assertBudgetHold(t, err, "invalid_credit_valuation_interval")
	}
}
