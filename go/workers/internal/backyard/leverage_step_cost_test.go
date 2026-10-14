package backyard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
)

// twentyBPSStepCost is a priced step cost of 20 bps of the borrowed debt
// (raw ~ USDC micros in these fixtures) plus three $0.01 transactions: the
// economics the leverage fixtures were written against.
func twentyBPSStepCost(raw uint64) (leverageStepCost, error) {
	return leverageStepLegs{EntryInMicros: int64(raw), EntryOutMicros: int64(raw) - int64(raw/500), ExitInMicros: 1, ExitOutMicros: 1, NetworkMicros: 30_000}.cost()
}

// measuredStepCost prices a step at measured AUTO quotes (2026-10-11, $100k):
// PYUSD->AUTO +0.003% (a gain), AUTO->PYUSD -0.064%,
// a zero origination fee, and six transactions at about $0.002 each.
func measuredStepCost(feeBPS int64) leverageStepPricer {
	return func(raw uint64) (leverageStepCost, error) {
		in := int64(raw)
		return leverageStepLegs{BorrowFeeMicros: in * feeBPS / 10_000, EntryInMicros: in, EntryOutMicros: in + in*3/100_000,
			ExitInMicros: in + in*3/100_000, ExitOutMicros: in + in*3/100_000 - in*64/100_000, NetworkMicros: 12_000}.cost()
	}
}

func quoteOutage(uint64) (leverageStepCost, error) {
	return leverageStepCost{}, errors.New("jupiter quote unavailable")
}

// An unknown leg of the expected cost is unknown, never zero.
func TestLeverageStepCostIsWhatCapitalLoses(t *testing.T) {
	t.Parallel()
	for name, legs := range map[string]leverageStepLegs{
		"no exit quote":   {EntryInMicros: 1, EntryOutMicros: 1, ExitInMicros: 1, NetworkMicros: 1},
		"no network fee":  {EntryInMicros: 1, EntryOutMicros: 1, ExitInMicros: 1, ExitOutMicros: 1},
		"no entry quote":  {ExitInMicros: 1, ExitOutMicros: 1, NetworkMicros: 1},
		"negative borrow": {BorrowFeeMicros: -1, EntryInMicros: 1, EntryOutMicros: 1, ExitInMicros: 1, ExitOutMicros: 1, NetworkMicros: 1},
	} {
		if _, err := legs.cost(); err == nil {
			t.Fatalf("%s priced", name)
		}
	}
}

// A pricing outage is not a decision: the stored approval carries and the
// worker still borrows the room under it. An economic refusal revokes it.
// The outage's sanitized cause reaches the log line and the counter.
func TestLeverageCostOutageCarriesTheApproval(t *testing.T) {
	t.Parallel()
	const room = 3_300_000_000
	s, m := oct10WindowSnapshot(t, room)
	m.NativeAPY = m.CurrentBorrowAPY + 0.0288
	approved, _ := decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{m}, DefaultSelectorPolicy(), measuredStepCost(0))
	if approved.BorrowRaw == 0 {
		t.Fatalf("fixture not approved: %+v", approved)
	}
	s.LeverageTargetLevel, s.LeverageUpApproved = approved.level().Level, approved.level().UpApproved
	outage := func(uint64) (leverageStepCost, error) {
		return leverageStepCost{}, costOutage("exit_quote", fmt.Errorf("Jupiter https://api.example/?api-key=secret: %w", context.DeadlineExceeded))
	}
	got, ok := decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{m}, DefaultSelectorPolicy(), outage)
	if !ok || got.Reason != "leverage_cost_unavailable(deadline)" || !got.outage() || got.storeChanges(s) {
		t.Fatalf("an outage overwrote the approval: %+v", got)
	}
	if line := got.logLine(); !strings.Contains(line, "reason=leverage_cost_unavailable(deadline)") || strings.Contains(line, "secret") || strings.Contains(line, "https") {
		t.Fatalf("log line: %s", line)
	}
	quoteFailed := func(uint64) (leverageStepCost, error) {
		return leverageStepCost{}, costOutage("entry_quote", errors.New("route dialect changed"))
	}
	if got, _ := decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{m}, DefaultSelectorPolicy(), quoteFailed); got.Reason != "leverage_cost_unavailable(entry_quote)" || got.storeChanges(s) {
		t.Fatalf("entry quote outage: %+v", got)
	}
	if d := Decide(targeted(s)); d.Action != OpenRouteStep || d.Reason != leverageUpReason || d.AmountRaw != room {
		t.Fatalf("the carried approval did not borrow the room: %+v", d)
	}
	// The spread falls under the 1.5x rule: the target steps down and the
	// approval is revoked.
	weak := m
	weak.NativeAPY = m.CurrentBorrowAPY + 0.005
	got, _ = decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{weak}, DefaultSelectorPolicy(), measuredStepCost(0))
	if got.outage() || got.BorrowRaw != 0 || !got.storeChanges(s) || got.Reason != "spread_rule" {
		t.Fatalf("a spread_rule refusal kept the approval: %+v", got)
	}
	// The counter carries the cause.
	registry := prometheus.NewRegistry()
	e := newEvents(slog.New(slog.NewJSONHandler(io.Discard, nil)), engine.NewFacts(registry), "rwa")
	e.leverageDecisionOutage("leverage_cost_unavailable(deadline)")
	families, _ := registry.Gather()
	counted := false
	for _, f := range families {
		for _, metric := range f.GetMetric() {
			if f.GetName() == "loyal_backyard_leverage_decision_outages_total" && metric.GetLabel()[0].GetValue() == "leverage_cost_unavailable(deadline)" && metric.GetCounter().GetValue() == 1 {
				counted = true
			}
		}
	}
	if !counted {
		t.Fatal("the outage was not counted by cause")
	}
}
