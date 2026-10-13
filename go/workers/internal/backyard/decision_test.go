package backyard

import "testing"

// base is a fresh, coherent, flat Maple view: no position, no cash.
func base() Snapshot {
	return Snapshot{ObservationID: "o", Slot: 9, RouteKind: RouteKind, Fresh: true, RouteLane: SelectedRouteID, StrategyKey: SelectedRouteID,
		LiquidationThresholdBPS: 8000, CapacityRaw: 7, PolicyLimitRaw: 10, MaxTargetLTVEntryRaw: 7, MinimumCollateralDepositRaw: 1,
		ApprovedAdminPerformanceFeeBPS: rwa.AdminPerformanceFeeBPS}
}

func TestDecisionPrecedenceAndOneAction(t *testing.T) {
	t.Parallel()
	s := base()
	s.Nonterminal = Submitted
	s.HasAmbiguousSubmission = true
	if got := Decide(s); got.Action != RecoverTransaction || got.Reason != "recover_ambiguous_submission" {
		t.Fatal(got)
	}
	s = base()
	s.Nonterminal = Built
	if got := Decide(s); got.Action != RecoverTransaction || got.Reason != "resume_nonterminal_operation" {
		t.Fatal(got)
	}
	// A journal-explained stage restores before anything reads Voltr idle.
	s = base()
	s.WithdrawalDemandRaw, s.VoltrIdleRaw, s.SquadsIdleRaw = 8, 3, 3
	s.VoltrStrategyIdleRaw, s.StagedAmountKnown, s.StagedAmountRaw = 5, true, 5
	if got := Decide(targeted(s)); got.Action != VoltrRestoreIdle || got.AmountRaw != 5 || got.Reason != "withdrawal_staged" {
		t.Fatal(got)
	}
	// An unexplained strategy custody balance is a manual stop.
	s.StagedAmountRaw = 4
	if got := Decide(targeted(s)); got.Action != HoldManualRecovery || got.Reason != "custody_mismatch" {
		t.Fatal(got)
	}
	// A lane outside the registry, or an incoherent view, never moves.
	s = base()
	s.RouteLane = "PRIME/USDC"
	if got := Decide(s); got.Action != HoldManualRecovery || got.Reason != "unsupported_runtime_lane" {
		t.Fatal(got)
	}
	s = base()
	s.Fresh = false
	if got := Decide(s); got.Action != HoldManualRecovery || got.Reason != "invalid_or_incoherent_snapshot" {
		t.Fatal(got)
	}
}

func TestDuplicateObservationIsIdempotent(t *testing.T) {
	t.Parallel()
	a, b := Decide(targeted(base())), Decide(targeted(base()))
	if a.IdempotencyKey != b.IdempotencyKey || a.Action != b.Action {
		t.Fatalf("%+v %+v", a, b)
	}
	later := base()
	later.Slot++
	c := Decide(targeted(later))
	if a.IdempotencyKey != c.IdempotencyKey {
		t.Fatalf("identical state at a later slot created a new identity: %s != %s", a.IdempotencyKey, c.IdempotencyKey)
	}
}

func TestIdempotencyIdentityIncludesEconomics(t *testing.T) {
	t.Parallel()
	baseline := base()
	baseline.WithdrawalDemandRaw, baseline.SquadsIdleRaw = 9, 4
	a := Decide(targeted(baseline))
	changed := baseline
	changed.SquadsIdleRaw = 3
	b := Decide(targeted(changed))
	if a.Action != StageSquadsToVoltr || a.IdempotencyKey == b.IdempotencyKey || a.AmountRaw == b.AmountRaw {
		t.Fatalf("changed economics aliased: a=%+v b=%+v", a, b)
	}
}
