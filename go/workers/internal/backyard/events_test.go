package backyard

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
)

// Backyard events are journald/ClickStack inputs: a fatal exit must carry its
// stable code without leaking RPC or database credentials, and the heartbeat
// must expose the fee-accumulator boundary the alert rule reads.
func TestEventsRedactCredentialsAndExposeHeartbeatFields(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	e := newEvents(slog.New(slog.NewJSONHandler(&out, nil)), nil)
	now := time.Unix(kaminoFixtureUnix, 0)
	e.now = func() time.Time { return now }
	s := feePolicySnapshot(t, 0, 0)
	s.FeeAccumulatorRaw, s.LPSupplyInclFeesRaw = 21, 2000
	e.noteSnapshot(s)
	e.tickResult(errors.New("dial https://rpc.example/?api-key=secret-value failed password=hunter2"), true)
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if len(records) != 2 || records[0]["msg"] != "backyard_heartbeat" || records[1]["msg"] != "backyard_worker_exit" {
		t.Fatalf("unexpected event sequence: %s", out.String())
	}
	if records[0]["fee_accumulator_warning"] != true || records[0]["family"] != "backyard" {
		t.Fatalf("heartbeat lost the fee boundary: %v", records[0])
	}
	if records[1]["code"] != "worker_fault" || strings.Contains(out.String(), "secret-value") || strings.Contains(out.String(), "hunter2") {
		t.Fatalf("exit event leaked credentials or lost its code: %v", records[1])
	}
	s.FeeAccumulatorRaw = 20
	e.noteSnapshot(s)
	out.Reset()
	now = now.Add(heartbeatInterval)
	e.tickResult(nil, false)
	if !strings.Contains(out.String(), `"fee_accumulator_warning":false`) {
		t.Fatalf("one-percent boundary reported as a warning: %s", out.String())
	}
}

// A latched route keeps ticking without error but is stopped: progress must go
// stale so the backyard stale-progress alert pages until clear-hold.
func TestLatchedRouteStopsProgress(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	facts := engine.NewFacts(registry)
	facts.Own(engine.FamilyBackyard)
	progress := func() float64 {
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if family.GetName() == "loyal_family_last_progress_timestamp_seconds" {
				return family.GetMetric()[0].GetGauge().GetValue()
			}
		}
		return -1
	}
	e := newEvents(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), facts)
	e.noteLatch(true, "operator_review")
	e.tickResult(nil, false)
	if progress() != 0 {
		t.Fatal("a latched tick reported progress")
	}
	e.noteLatch(false, "")
	e.tickResult(nil, false)
	if progress() <= 0 {
		t.Fatal("a cleared tick did not report progress")
	}
}

func TestSelectorExpectedDeferralDoesNotCountAsFailed(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	e := newEvents(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), engine.NewFacts(registry))
	e.selectorSampleError(budgetHold("selector_finish_current_work_first"))
	e.selectorSampleError(errors.New("selector_finish_current_work_first"))
	e.selectorSampleError(budgetHold("selector_fee_evidence_unavailable"))
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "loyal_family_failed_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "code" {
					got[label.GetValue()] = metric.GetCounter().GetValue()
				}
			}
		}
	}
	if len(got) != 2 || got["selector_evaluate_unavailable"] != 1 || got["selector_fee_evidence_unavailable"] != 1 {
		t.Fatalf("deferral/failure classification: %v", got)
	}
}

func TestWithdrawalMetricsKeepAttentionAndClockWhenEvidenceUnavailable(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	e := newEvents(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), engine.NewFacts(registry))
	now := time.Unix(1000, 0)
	e.withdrawalHealthStored(WithdrawalHealth{RouteKey: productionRouteKey, Status: "operator_attention", ObservedAt: now})
	e.withdrawalHealthStored(WithdrawalHealth{RouteKey: productionRouteKey, Status: "unavailable", ObservedAt: now.Add(time.Minute)})
	check := func(attention, observed float64) {
		t.Helper()
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]float64{}
		for _, family := range families {
			if strings.HasPrefix(family.GetName(), "loyal_backyard_withdrawal_") {
				got[family.GetName()] = family.GetMetric()[0].GetGauge().GetValue()
			}
		}
		if got["loyal_backyard_withdrawal_attention"] != attention || got["loyal_backyard_withdrawal_observed_timestamp_seconds"] != observed {
			t.Fatalf("health metrics: %v", got)
		}
	}
	check(1, 1000)
	e.withdrawalHealthStored(WithdrawalHealth{RouteKey: productionRouteKey, Status: "waiting", ObservedAt: now.Add(2 * time.Minute)})
	check(0, 1120)
}
