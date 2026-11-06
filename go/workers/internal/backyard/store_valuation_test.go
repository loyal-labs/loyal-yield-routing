package backyard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestValuationPersistencePreservesLegacyReplayAndProjectedSource(t *testing.T) {
	t.Parallel()
	o := Observation{ObservedAt: time.Unix(1_700_000_000, 0), Snapshot: Snapshot{Slot: 123, ObservationID: "same-economics", ReportSequence: 123, ReportSnapshotDigest: strings.Repeat("a", 64)}}
	d := Decision{Action: Hold, Reason: "no_action", StrategyKey: PrimeUSDCLaneID}
	old := newDecisionEvidence(o, d, "manifest")
	before, _ := json.Marshal(old)
	o.ValuationSource, o.ValuationSlot = "confirmed", 123
	o.Snapshot.ValuationSource, o.Snapshot.ValuationSlot = "confirmed", 123
	confirmed := newDecisionEvidence(o, d, "manifest")
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
	projected := newDecisionEvidence(o, d, "manifest")
	projection, err = newRouteObservationProjection(o)
	if err != nil {
		t.Fatal(err)
	}
	if projection.ValuationSource != routeRefreshValuationSource || projection.ValuationSlot != 123 || projected.ValuationSource != routeRefreshValuationSource {
		t.Fatal("projected provenance dropped")
	}
	o.Snapshot.ValuationSlot = 122
	if _, err := newRouteObservationProjection(o); err == nil {
		t.Fatal("mixed bank provenance accepted")
	}
}

func certifyProjectionFixture(s *Snapshot, at time.Time) {
	s.net = &certifiedNAV{vault: "fixture", manifest: strings.Repeat("a", 64), slot: s.Slot, validUntil: at.Add(time.Minute)}
	s.NAVUnavailable = ""
	bindSnapshotNAV(s)
}

func TestUnknownNetPublishesCurrentRiskWithoutZeroOrOldNAV(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	s := Snapshot{Slot: 123, HasPosition: true, PositionCollateralRaw: 100, PositionDebtRaw: 80,
		PositionCollateralValueRaw: 100, PositionDebtValueRaw: 80, LTVBPS: 8000,
		VoltrIdleRaw: 5, StrategyNAVRaw: 0, TotalVaultNAVRaw: 0, PriorReportedNAVRaw: 90,
		ReportSequence: 120, ReportSnapshotDigest: strings.Repeat("a", 64), LastReportAgeSeconds: 60,
		PriorReportUpdatedUnix: at.Add(-time.Minute).Unix(), NAVUnavailable: exitUnpriced}
	p, err := newRouteObservationProjection(Observation{ObservedAt: at, Snapshot: s})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"aumRaw", "aumUsdMicros", "navRaw", "navUsdMicros", "computedStrategyNavRaw"} {
		if _, exists := wire[key]; exists {
			t.Fatalf("unknown net published %s: %s", key, raw)
		}
	}
	if p.NAVKnown || p.NAVFresh || p.NAVUnavailable != exitUnpriced || p.PositionCollateralRaw != "100" || p.PositionDebtRaw != "80" || p.LTVBPS != 8000 || snapshotEquity(s, at) != nil {
		t.Fatalf("unknown current risk lost: %s", raw)
	}
	report, known, err := p.navReport()
	if err != nil || !known || !report.at.Equal(at.Add(-time.Minute)) {
		t.Fatalf("prior report rebadged: %+v %v", report, err)
	}
	p.ReportedNAVRaw, p.RouteStatus = "0", "withdrawal_pending"
	if report, _, err := p.navReport(); err != nil || !report.holdsValue {
		t.Fatal("unknown net hid a funded withdrawal from report monitoring", err)
	}
	s.VoltrIdleRaw = 0
	certifyProjectionFixture(&s, at)
	zero, err := newRouteObservationProjection(Observation{ObservedAt: at, Snapshot: s})
	if err != nil || !zero.NAVKnown || zero.ComputedStrategyNAV != "0" || zero.AUMUSDMicros != "0" || snapshotEquity(s, at) == nil || *snapshotEquity(s, at) != 0 {
		t.Fatalf("known zero lost: %+v %v", zero, err)
	}
	s.StrategyNAVRaw, s.TotalVaultNAVRaw = 20, 20
	s.Slot++
	certifyProjectionFixture(&s, at.Add(time.Second))
	recovered, err := newRouteObservationProjection(Observation{ObservedAt: at.Add(time.Second), Snapshot: s})
	if err != nil || !recovered.NAVKnown || !recovered.NAVFresh || recovered.ComputedStrategyNAV != "20" || recovered.NAVUnavailable != "" {
		t.Fatalf("fresh net did not recover: %+v %v", recovered, err)
	}
	expired, err := newRouteObservationProjection(Observation{ObservedAt: at.Add(2 * time.Minute), Snapshot: s})
	if err != nil || expired.NAVKnown || expired.ComputedStrategyNAV != "" {
		t.Fatalf("expired certificate published: %+v %v", expired, err)
	}
}
