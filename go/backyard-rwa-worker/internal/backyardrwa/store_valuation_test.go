package backyardrwa

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestValuationPersistencePreservesLegacyReplayAndProjectedSource(t *testing.T) {
	o := Observation{ObservedAt: time.Unix(1_700_000_000, 0), Snapshot: Snapshot{Slot: 123, ObservationID: "same-economics", ReportSequence: 123, ReportSnapshotDigest: strings.Repeat("a", 64)}}
	d := Decision{Action: Hold, Reason: "no_action", StrategyKey: RouteID}
	old := newDecisionEvidence(o, d, "manifest", "policies")
	before, _ := json.Marshal(old)
	o.ValuationSource, o.ValuationSlot = "confirmed", 123
	o.Snapshot.ValuationSource, o.Snapshot.ValuationSlot = "confirmed", 123
	confirmed := newDecisionEvidence(o, d, "manifest", "policies")
	after, _ := json.Marshal(confirmed)
	if string(before) != string(after) || strings.Contains(string(after), "valuation") {
		t.Fatal("confirmed decision JSON changed")
	}
	projection, err := newRouteObservationProjection(o)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(projection)
	if strings.Contains(string(raw), "valuation") {
		t.Fatal("confirmed projection gained provenance fields")
	}
	o.ValuationSource, o.Snapshot.ValuationSource = routeRefreshValuationSource, routeRefreshValuationSource
	projected := newDecisionEvidence(o, d, "manifest", "policies")
	projection, err = newRouteObservationProjection(o)
	if err != nil {
		t.Fatal(err)
	}
	if projection.ValuationSource != routeRefreshValuationSource || projection.ValuationSlot != 123 || projected.ValuationSource != routeRefreshValuationSource {
		t.Fatal("projected provenance dropped")
	}
	later := projected
	later.ObservationSlot = 124
	later.ValuationSlot = 124
	if !sameDecisionEvidence(projected, later) || !sameDecisionEvidence(old, later) {
		t.Fatal("later slot or missing historical metadata broke replay")
	}
	changed := later
	changed.Reason = "changed"
	if sameDecisionEvidence(old, changed) {
		t.Fatal("legacy replay accepted different decision")
	}
	changed = later
	changed.ValuationSource = "confirmed"
	if sameDecisionEvidence(projected, changed) {
		t.Fatal("explicit provenance changed on replay")
	}
	o.Snapshot.ValuationSlot = 122
	if _, err := newRouteObservationProjection(o); err == nil {
		t.Fatal("mixed bank provenance accepted")
	}
}
