package backyard

import (
	"strings"
	"testing"
	"time"
)

func TestRestorePersistedDecisionIncludesImmutableAmountAndReason(t *testing.T) {
	t.Parallel()
	expected := []byte(`{"decision":{"amountRaw":896575,"reason":"withdrawal_repay_debt","strategyKey":"Prime/PRIME/USDC"}}`)
	decision, err := restorePersistedDecision(expected, DeleverRouteStep, "durable-key", PrimeUSDCLaneID)
	if err != nil {
		t.Fatal(err)
	}
	if decision.AmountRaw != 896575 || decision.Reason != "withdrawal_repay_debt" || decision.StrategyKey != PrimeUSDCLaneID {
		t.Fatalf("persisted decision was not restored exactly: %+v", decision)
	}
	if _, err := restorePersistedDecision(expected, DeleverRouteStep, "durable-key", MapleSyrupUSDCLaneID); err == nil {
		t.Fatal("strategy drift between the envelope and operation row was accepted")
	}
}

func TestFlatObservationProjectionRetainsConfirmedDashboardTruth(t *testing.T) {
	t.Parallel()
	observedAt := time.Unix(1_700_000_000, 0).UTC()
	observation := Observation{ObservedAt: observedAt, Snapshot: Snapshot{
		ObservationID: "flat", Slot: 123, RouteKind: RouteKind, Fresh: true,
		VoltrIdleRaw: 3, VoltrStrategyIdleRaw: 4, SquadsIdleRaw: 8,
		StrategyNAVRaw: 12, TotalVaultNAVRaw: 15, PriorReportedNAVRaw: 12,
		PriorReportUpdatedUnix: observedAt.Add(-10 * time.Second).Unix(), LastReportAgeSeconds: 10,
		ReportSequence: 123, ReportSnapshotDigest: strings.Repeat("a", 64),
	}}
	projection, err := newRouteObservationProjection(observation)
	if err != nil {
		t.Fatal(err)
	}
	if projection.RouteStatus != "idle" || projection.VoltrIdleRaw != "3" ||
		projection.VoltrStrategyIdleRaw != "4" || projection.SquadsIdleRaw != "8" ||
		projection.AUMUSDMicros != "15" || projection.NAVUSDMicros != "12" || !projection.NAVFresh {
		t.Fatalf("flat projection lost confirmed route truth: %+v", projection)
	}
	for _, required := range []string{"jsonb_set", "'{observation}'", "observedSlot", "<= $3"} {
		if !strings.Contains(RouteProjectionUpdate, required) {
			t.Fatalf("route projection update lacks %q", required)
		}
	}
}

func TestActionableIdempotencyUsesDurableTerminalEpoch(t *testing.T) {
	t.Parallel()
	query := normalizeSQL(LatestDecisionEpochSQL)
	for _, required := range []string{
		"operation_id",
		"route_key = $1",
		"status IN ('reconciled','failed')",
		"status = 'manual_recovery' AND action = 'REPORT_NAV'",
		"ORDER BY updated_at DESC, operation_id DESC LIMIT 1",
		"'genesis'",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("durable operation epoch query is missing %q", required)
		}
	}
	if strings.Contains(query, "status = 'manual_recovery')") {
		t.Fatal("capital-moving manual recovery was made retryable")
	}
	actionable := Decision{Action: VoltrAllocateToSquads, IdempotencyKey: "economic-state"}
	first, err := durableDecisionIdempotencyKey("route", "reconciled-a", actionable)
	if err != nil {
		t.Fatal(err)
	}
	later, err := durableDecisionIdempotencyKey("route", "reconciled-b", actionable)
	if err != nil {
		t.Fatal(err)
	}
	if first == later {
		t.Fatal("a genuinely later reconciled lifecycle reused an actionable operation identity")
	}
	retry, err := durableDecisionIdempotencyKey("route", "failed-prebroadcast", actionable)
	if err != nil || retry == first {
		t.Fatal("a terminal pre-broadcast failure did not advance the durable retry identity")
	}
	if _, err := durableDecisionIdempotencyKey("route", "", actionable); err == nil {
		t.Fatal("actionable decision accepted an absent durable epoch")
	}
}

func normalizeSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}
