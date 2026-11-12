package backyard

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
)

func riskMetric(t *testing.T, r *prometheus.Registry, name, vault, lane string) (float64, bool) {
	t.Helper()
	families, err := r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["vault"] == vault && labels["lane"] == lane {
				return metric.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func TestHolderValueMetricsIgnoreCashflowsAndPreserveUnknownEvidence(t *testing.T) {
	r := prometheus.NewRegistry()
	facts := engine.NewFacts(r)
	e := newEvents(slog.New(slog.NewJSONHandler(io.Discard, nil)), facts, "rwa")
	now := time.Now()
	e.now = func() time.Time { return now }
	s := Snapshot{Fresh: true, Slot: 100, TotalVaultNAVRaw: 100_000_000, LPSupplyInclFeesRaw: 100_000_000}
	observe := func() { s.net = fixtureSnapshotCertificate(s); e.noteSnapshot(s, now) }
	share := func() float64 {
		v, ok := riskMetric(t, r, "loyal_backyard_net_value_per_share", "rwa", "")
		if !ok {
			t.Fatal("missing holder-value observation")
		}
		return v
	}
	observe()
	initial := share()
	// A proportional deposit and a withdrawal change pool size, not holder value.
	s.TotalVaultNAVRaw, s.LPSupplyInclFeesRaw = 200_000_000, 200_000_000
	observe()
	if share() != initial {
		t.Fatal("deposit looked like a gain or loss")
	}
	s.TotalVaultNAVRaw, s.LPSupplyInclFeesRaw = 50_000_000, 50_000_000
	observe()
	if share() != initial {
		t.Fatal("withdrawal looked like a gain or loss")
	}
	// Costs reduce value with no corresponding share burn.
	s.TotalVaultNAVRaw = 49_000_000
	observe()
	if share() != 0.98 {
		t.Fatal("holder loss was hidden")
	}
	stamp, _ := riskMetric(t, r, "loyal_backyard_valued_timestamp_seconds", "rwa", "")
	now = now.Add(time.Second)
	s.net = nil
	s.TotalVaultNAVRaw = 0
	e.noteSnapshot(s, now)
	after, _ := riskMetric(t, r, "loyal_backyard_valued_timestamp_seconds", "rwa", "")
	if share() != 0.98 || after != stamp {
		t.Fatal("unavailable NAV fabricated zero value or fresh evidence")
	}
	// A genuinely emptied vault has no share price, with explicit zero supply.
	s.LPSupplyInclFeesRaw = 0
	observe()
	if _, ok := riskMetric(t, r, "loyal_backyard_net_value_per_share", "rwa", ""); ok {
		t.Fatal("drained vault retained a current share price")
	}
	supply, ok := riskMetric(t, r, "loyal_backyard_share_supply_raw", "rwa", "")
	if !ok || supply != 0 {
		t.Fatal("drained vault cannot be distinguished from missing data")
	}
}

func TestLoanRiskCoversEveryLaneAndKeepsUnsafeStateUntilProvenCleared(t *testing.T) {
	r := prometheus.NewRegistry()
	e := newEvents(slog.Default(), engine.NewFacts(r), "rwa")
	now := time.Now()
	safe := Snapshot{Fresh: true, RouteLane: "healthy", PositionDebtRaw: 50, PositionCollateralValueRaw: 100, PositionDebtValueRaw: 50, LTVBPS: 5000, LiquidationThresholdBPS: 8000, MaxLTVBPS: 7500}
	underwater := safe
	underwater.RouteLane = "underwater"
	underwater.LTVBPS = 10000
	underwater.PositionDebtValueRaw = 150
	o := Observation{Snapshot: safe, ObservedAt: now, lanes: []Snapshot{safe, underwater}}
	e.riskObserved(o)
	for _, lane := range []string{"healthy", "underwater"} {
		if _, ok := riskMetric(t, r, "loyal_backyard_risk_observed_timestamp_seconds", "rwa", lane); !ok {
			t.Fatalf("lane %s hidden by selected lane or negative equity", lane)
		}
	}
	margin, _ := riskMetric(t, r, "loyal_backyard_liquidation_margin_bps", "rwa", "underwater")
	if margin != -2000 {
		t.Fatal("underwater position is not visible")
	}
	o.ObservedAt = now.Add(time.Minute)
	o.lanes = nil
	e.riskObserved(o)
	stamp, _ := riskMetric(t, r, "loyal_backyard_risk_observed_timestamp_seconds", "rwa", "underwater")
	debt, _ := riskMetric(t, r, "loyal_backyard_lane_has_debt", "rwa", "underwater")
	if stamp != float64(now.Unix()) || debt != 1 {
		t.Fatal("incomplete observation cleared known exposure")
	}
	stale := safe
	stale.Fresh = false
	o.lanes = []Snapshot{stale, underwater}
	e.riskObserved(o)
	stamp, _ = riskMetric(t, r, "loyal_backyard_risk_observed_timestamp_seconds", "rwa", "underwater")
	if stamp != float64(now.Unix()) {
		t.Fatal("partially stale observation refreshed the exposure")
	}
	underwater.PositionDebtRaw = 0
	o.lanes = []Snapshot{safe, underwater}
	e.riskObserved(o)
	debt, _ = riskMetric(t, r, "loyal_backyard_lane_has_debt", "rwa", "underwater")
	if debt != 0 {
		t.Fatal("confirmed repayment did not clear exposure")
	}
	if _, ok := riskMetric(t, r, "loyal_backyard_liquidation_margin_bps", "rwa", "underwater"); ok {
		t.Fatal("repaid loan still exports current risk")
	}
	restartedRegistry := prometheus.NewRegistry()
	restarted := newEvents(slog.Default(), engine.NewFacts(restartedRegistry), "rwa")
	restarted.riskObserved(o)
	debt, ok := riskMetric(t, restartedRegistry, "loyal_backyard_lane_has_debt", "rwa", "underwater")
	if !ok || debt != 0 {
		t.Fatal("repayment after restart cannot clear historical exposure")
	}
}

func TestRecoveryMetricsSeparateVaultsAndSurviveSuccessfulLatchedTicks(t *testing.T) {
	r := prometheus.NewRegistry()
	facts := engine.NewFacts(r)
	rwaEvents, loopingEvents := newEvents(slog.Default(), facts, "rwa"), newEvents(slog.Default(), facts, "looping")
	rwaEvents.noteLatch(true, "operator_review")
	loopingEvents.noteLatch(false, "")
	rwaEvents.tickResult(nil, false)
	loopingEvents.tickResult(nil, false)
	rwaValue, _ := riskMetric(t, r, "loyal_backyard_recovery_required", "rwa", "")
	loopingValue, _ := riskMetric(t, r, "loyal_backyard_recovery_required", "looping", "")
	if rwaValue != 1 || loopingValue != 0 {
		t.Fatal("healthy sibling or successful stopped tick hid recovery state")
	}
	rwaEvents.noteLatch(false, "")
	if value, _ := riskMetric(t, r, "loyal_backyard_recovery_required", "rwa", ""); value != 0 {
		t.Fatal("verified latch clear did not clear recovery signal")
	}
}
