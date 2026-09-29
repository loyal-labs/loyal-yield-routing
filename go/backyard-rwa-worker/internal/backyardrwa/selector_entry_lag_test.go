package backyardrwa

import (
	"encoding/json"
	"testing"
	"time"
)

// Live 2026-09-29: a quote stored when the measured observation lag was 49
// slots failed validation once the lag re-measured to 48, and the worker
// exited on invalid_selector_entry. The stored window is checked against the
// fixed ceiling; an entry that is invalid anyway pauses instead.
func TestStoredSelectorEntryIgnoresTheRuntimeLag(t *testing.T) {
	entry := selectorEntryFixture(time.Now().UTC(), SelectedRouteID, 1_000_000)
	entry.Quote.ValidThroughSlot = entry.Quote.SampleSlot + 49
	restore := observationLag.Load()
	observationLag.Store(48)
	defer observationLag.Store(restore)
	if observationLagSlots() != 48 {
		t.Fatal("lag fixture")
	}
	if err := entry.validate(); err != nil {
		t.Fatalf("window 49 refused at lag 48: %v", err)
	}
	// Beyond the fixed ceiling it is still invalid...
	bad := entry
	bad.Quote.ValidThroughSlot = bad.Quote.SampleSlot + budgetMaxObservationLagCeilingSlots + 1
	if bad.validate() == nil {
		t.Fatal("window above the ceiling accepted")
	}
	// ...and an invalid stored entry pauses, never errors.
	raw, _ := json.Marshal(bad)
	if decoded, err := decodeSelectorEntry(raw); err != nil || decoded != nil {
		t.Fatalf("invalid stored entry: %v %v", decoded, err)
	}
	s := base()
	s.PilotActive, s.RouteLane, s.StrategyKey = true, SelectedRouteID, SelectedRouteID
	if err := applySelectorEntry(&s, &bad, time.Now().UTC()); err != nil || !s.SelectorEntryPaused || s.SelectorEntryEquityRaw != 0 {
		t.Fatalf("apply invalid entry: err=%v paused=%t", err, s.SelectorEntryPaused)
	}
	// A new allocation still needs a quote current at the observed slot.
	s = base()
	s.PilotActive, s.RouteLane, s.StrategyKey = true, SelectedRouteID, SelectedRouteID
	s.Slot = entry.Quote.ValidThroughSlot + 1
	if err := applySelectorEntry(&s, &entry, time.Now().UTC()); err != nil || !s.SelectorEntryPaused {
		t.Fatalf("stale quote still authorized an allocation: err=%v paused=%t", err, s.SelectorEntryPaused)
	}
}
