package backyardrwa

import (
	"testing"
	"time"
)

func TestStaleObservationIdentityCanProduceDurableManualHold(t *testing.T) {
	observation := Observation{ObservedAt: time.Unix(1, 0), Snapshot: Snapshot{ObservationID: "stale", Slot: 9, RouteKind: RouteKind, Fresh: false}}
	if err := observation.Validate(); err != nil {
		t.Fatalf("stale evidence identity was discarded instead of becoming a hold: %v", err)
	}
	decision := Decide(observation.Snapshot)
	if decision.Action != HoldManualRecovery {
		t.Fatalf("stale observation did not fail closed: %+v", decision)
	}
}
