package backyardrwa

import (
	"encoding/binary"
	"testing"
)

// The S1/S2 gates decide on the snapshot the production merge produces, so
// these cases merge a real confirmed batch through ComputeRouteNAV and
// applyRouteNAVSnapshot and then apply only the journal facts that
// productionObserveState would copy out of reconciled rows. The one case the
// full productionObserveState.observe path cannot reach here is the bare drift
// hold: this route's fixture sits in the legacy PRIME cutover state, whose
// unwind legs intentionally outrank the drift gate, so the drift hold is
// decided on the identical merged snapshot instead.
func TestUnexplainedNavDriftHoldsWithoutReconciledMutation(t *testing.T) {
	drift := monitorSnapshot(t, func(accounts []ConfirmedAccount, s *Snapshot) {
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 20_000)
		s.SquadsIdleRaw = 20_000
		s.StrategyNAVRaw += 19_994
	})
	if got := Decide(drift); got.Action != HoldManualRecovery || got.Reason != "nav_drift_unexplained" {
		t.Fatalf("unexplained NAV drift was reported instead of held: %+v", got)
	}
	// A reconciled bridge mutation newer than the last report is the only
	// accepted explanation, and the journal is where it comes from.
	explained := drift
	explained.CapitalMutated = true
	if got := Decide(explained); got.Action != ReportNAV || got.Reason != "nav_due" {
		t.Fatalf("a reconciled mutation did not explain its own valuation move: %+v", got)
	}
	unreported := drift
	unreported.PostMutationNAVRequired = true
	if got := Decide(unreported); got.Action != ReportNAV || got.Reason != "post_mutation_nav_due" {
		t.Fatalf("an unreported reconciled mutation did not take precedence: %+v", got)
	}
	// Sub-tolerance drift stays inside the bound and keeps the route live.
	subtle := drift
	subtle.SquadsIdleRaw, subtle.StrategyNAVRaw = 10, 37
	if got := Decide(subtle); got.Action == HoldManualRecovery {
		t.Fatalf("sub-tolerance drift held the route: %+v", got)
	}
}

// TestReportChurnToleranceSuppressesUnchangedValuation is the S2 acceptance
// proof: a reconciled capital mutation schedules a report only when the
// valuation moved beyond the drift tolerance, while the post-mutation
// requirement and the aging cadence still report unconditionally.
func TestReportChurnToleranceSuppressesUnchangedValuation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nav        int64
		age        int64
		wantReport bool
	}{
		{"unchanged valuation does not churn a report", 33, 10, false},
		{"sub-tolerance valuation move does not churn a report", 43, 10, false},
		{"valuation move beyond tolerance reports", 1_043, 10, true},
		{"aging cadence reports an unchanged valuation", 33, 60, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
				s.StrategyNAVRaw = tc.nav
				s.CapitalMutated = true // reconciled mutation newer than the last report
				s.LastReportAgeSeconds = tc.age
			})
			got := Decide(s)
			if tc.wantReport && (got.Action != ReportNAV || got.Reason != "nav_due") {
				t.Fatalf("nav=%d age=%d did not report: %+v", tc.nav, tc.age, got)
			}
			if !tc.wantReport && got.Action == ReportNAV {
				t.Fatalf("nav=%d age=%d churned a report: %+v", tc.nav, tc.age, got)
			}
		})
	}
}
