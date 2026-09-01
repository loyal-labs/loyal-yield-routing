package backyardrwa

import "testing"

func base() Snapshot {
	return Snapshot{ObservationID: "o", Slot: 9, RouteKind: RouteKind, Fresh: true, LiquidationThresholdBPS: 8000, NetAPYBPS: 1, CapacityRaw: 7, PolicyLimitRaw: 10, MaxTargetLTVEntryRaw: 7, PolicyReady: true, ExitBuildable: true}
}
func TestDecisionPrecedenceAndOneAction(t *testing.T) {
	s := base()
	s.Nonterminal = Submitted
	s.HasAmbiguousSubmission = true
	if got := Decide(s); got.Action != RecoverTransaction {
		t.Fatal(got)
	}
	s = base()
	s.Nonterminal = Built
	if got := Decide(s); got.Action != RecoverTransaction || got.Reason != "resume_nonterminal_operation" {
		t.Fatal(got)
	}
	s = base()
	s.HasPosition = true
	s.LTVBPS = 6000
	s.WithdrawalDemandRaw = 3
	if got := Decide(s); got.Action != DeleverPrimeUSDCStep {
		t.Fatal(got)
	}
	s = base()
	s.WithdrawalDemandRaw = 3
	s.SquadsIdleRaw = 5
	if got := Decide(s); got.Action != StageSquadsToVoltr || got.AmountRaw != 3 {
		t.Fatal(got)
	}
	s = base()
	s.WithdrawalDemandRaw = 8
	s.VoltrIdleRaw = 3
	s.VoltrStrategyIdleRaw = 5
	if got := Decide(s); got.Action != VoltrRestoreIdle || got.AmountRaw != 5 {
		t.Fatal(got)
	}
	s = base()
	s.WithdrawalDemandRaw = 8
	s.SquadsIdleRaw = 2
	s.HasPosition = true
	if got := Decide(s); got.Action != DeleverPrimeUSDCStep || got.AmountRaw != 6 {
		t.Fatal(got)
	}
	s = base()
	s.WithdrawalDemandRaw = 8
	s.VoltrStrategyIdleRaw = 2
	s.SquadsIdleRaw = 3
	s.HasPosition = true
	if got := Decide(s); got.Action != DeleverPrimeUSDCStep || got.AmountRaw != 3 {
		t.Fatal(got)
	}
	s = base()
	s.WithdrawalDemandRaw = 8
	s.VoltrIdleRaw = 8
	if got := Decide(s); got.Action != Hold || got.Reason != "withdrawal_covered" {
		t.Fatal(got)
	}
}
func TestDuplicateObservationIsIdempotent(t *testing.T) {
	a, b := Decide(base()), Decide(base())
	if a.IdempotencyKey != b.IdempotencyKey || a.Action != b.Action {
		t.Fatalf("%+v %+v", a, b)
	}
	later := base()
	later.Slot++
	c := Decide(later)
	if a.IdempotencyKey != c.IdempotencyKey {
		t.Fatalf("identical state at a later slot created a new identity: %s != %s", a.IdempotencyKey, c.IdempotencyKey)
	}
}

func TestIdempotencyIdentityIncludesEconomics(t *testing.T) {
	baseline := base()
	baseline.SquadsIdleRaw = 4
	a := Decide(baseline)
	changed := baseline
	changed.SquadsIdleRaw = 3
	b := Decide(changed)
	if a.IdempotencyKey == b.IdempotencyKey || a.AmountRaw == b.AmountRaw {
		t.Fatalf("changed economics aliased: a=%+v b=%+v", a, b)
	}
}

func TestEntryIsBoundedAndInvalidThresholdFailsClosed(t *testing.T) {
	s := base()
	s.SquadsIdleRaw = 4
	if got := Decide(s); got.Action != OpenPrimeUSDCStep || got.AmountRaw != 4 {
		t.Fatal(got)
	}
	s.LiquidationThresholdBPS = 6000
	if got := Decide(s); got.Action != HoldManualRecovery {
		t.Fatal(got)
	}
	s = base()
	s.SquadsIdleRaw = 9
	s.MaxTargetLTVEntryRaw = 2
	if got := Decide(s); got.Action != OpenPrimeUSDCStep || got.AmountRaw != 2 {
		t.Fatalf("target-LTV entry bound not applied: %+v", got)
	}
	s.MaxTargetLTVEntryRaw = 0
	if got := Decide(s); got.Action != Hold {
		t.Fatalf("zero target-LTV headroom did not hold: %+v", got)
	}
}

func TestEntryRequiresPositiveObservedNetAPY(t *testing.T) {
	s := base()
	s.SquadsIdleRaw = 4
	s.NetAPYBPS = 0
	if got := Decide(s); got.Action != Hold {
		t.Fatalf("nonpositive APY entered risk: %+v", got)
	}
	s.NetAPYBPS = -1
	if got := Decide(s); got.Action != Hold {
		t.Fatalf("negative APY entered risk: %+v", got)
	}
}
