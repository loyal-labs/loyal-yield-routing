package backyardrwa

import "testing"

func TestExecutableBridgeDebitsUseSourceAndFullSweep(t *testing.T) {
	for _, action := range []Action{VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV} {
		amount := uint64(100_000)
		if action == ReportNAV {
			amount = 0
		}
		effects, _, _, err := bridgeExpectedEffects(Decision{Action: action, AmountRaw: int64(amount)}, 200_000, 100_000, 100_000)
		if err != nil {
			t.Fatal(err)
		}
		debit, err := MeasureExecutableDebit(bridgeTestRequest(action, amount), effects)
		if err != nil || debit.Raw != amount {
			t.Fatalf("%s measured %+v %v", action, debit, err)
		}
	}
	// Exact retained Phase 2 incident amounts: the requested 1 USDC was not
	// a transfer cap. Reject its partial-sweep effect contract before signing.
	effects, _, _, err := bridgeExpectedEffects(Decision{Action: VoltrRestoreIdle, AmountRaw: 1_000_000}, 0, 3_793_417, 0)
	if err != nil {
		t.Fatal(err)
	}
	debit, err := MeasureExecutableDebit(bridgeTestRequest(VoltrRestoreIdle, 1_000_000), effects)
	assertBudgetHold(t, err, "full_sweep_amount_mismatch")
	if debit.Raw != 3_793_417 {
		t.Fatalf("sweep understated: %+v", debit)
	}
	b := emptyTestBudget()
	reservation := testReservation()
	reservation.UpperMicros = int64(debit.Raw)
	assertBudgetHold(t, b.Admit(reservation), "transaction_cap_exceeded")
}

func TestWithdrawalChargesUnderlyingNotReceiptUnits(t *testing.T) {
	request := kaminoTestRequest(DeleverPrimeUSDCStep, kaminoLegWithdraw)
	source, destination := kaminoLegCustodies(kaminoLegWithdraw)
	effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{
		{Address: source.Address, Mint: source.Mint, Authority: source.Authority, Owner: classicTokenProgram, BeforeRaw: 2_000_000, AfterRaw: 1_500_000},
		{Address: destination.Address, Mint: destination.Mint, Authority: destination.Authority, Owner: classicTokenProgram, BeforeRaw: 0, AfterRaw: 500_000},
	}}
	debit, err := MeasureExecutableDebit(request, effects)
	if err != nil || debit.Raw != 500_000 || debit.Raw == request.AmountRaw {
		t.Fatalf("receipt units mispriced as liquidity: %+v %v", debit, err)
	}
}

func TestTransactionCapIncludesNetworkFee(t *testing.T) {
	principal, err := ValueDebitMicros(999_500, 6, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	fee, err := ValueDebitMicros(5_000, 9, 150_000_000)
	if err != nil {
		t.Fatal(err)
	}
	total, err := budgetSum(principal, fee)
	if err != nil {
		t.Fatal(err)
	}
	b := emptyTestBudget()
	r := testReservation()
	r.UpperMicros = total
	assertBudgetHold(t, b.Admit(r), "transaction_cap_exceeded")
}
