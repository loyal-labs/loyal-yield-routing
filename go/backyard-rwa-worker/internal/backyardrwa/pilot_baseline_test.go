package backyardrwa

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// M8's pilot-activation baseline: an active validated baseline explains
// exactly its own archived consumed sequence while the journal has no
// reconciled ticket-consuming operation; the journal always wins; without an
// active baseline any nonzero ticket holds; and a reset back to zero after a
// consumed baseline holds like any other out-of-band move.
func TestMonitorPilotBaselineExplainsExactArchivedSequence(t *testing.T) {
	baseline := func(s *Snapshot) {
		s.PilotActive = true
		s.PilotBaselineKnown = true
		s.PilotBaselineTicketSequenceRaw = 4
	}
	exact := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
		s.JournalSequenceKnown = false
		baseline(s)
	})
	if hold, blocked := bridgeMonitorHold(exact); blocked {
		t.Fatalf("exact archived baseline sequence held: %+v", hold)
	}
	if got := Decide(exact); got.Action == HoldManualRecovery && got.Reason == "out_of_band_crank" {
		t.Fatalf("exact archived baseline sequence held in Decide: %+v", got)
	}
	moved := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
		s.JournalSequenceKnown = false
		s.TicketLastConsumedSequenceRaw = 5
		baseline(s)
	})
	if got := Decide(moved); got.Action != HoldManualRecovery || got.Reason != "out_of_band_crank" {
		t.Fatalf("ticket past the archived baseline did not hold: %+v", got)
	}
	// A reset back to zero after the consumed baseline is itself out of band.
	reset := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
		s.JournalSequenceKnown = false
		s.TicketLastConsumedSequenceRaw = 0
		baseline(s)
	})
	if got := Decide(reset); got.Action != HoldManualRecovery || got.Reason != "out_of_band_crank" {
		t.Fatalf("ticket reset to zero under an active baseline did not hold: %+v", got)
	}
	// A zero baseline compared against a zero ticket is exact and coherent.
	preConsumed := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
		s.JournalSequenceKnown = false
		s.TicketLastConsumedSequenceRaw = 0
		s.PilotActive = true
		s.PilotBaselineKnown = true
	})
	if hold, blocked := bridgeMonitorHold(preConsumed); blocked {
		t.Fatalf("zero archived baseline held against a zero ticket: %+v", hold)
	}
}

func TestMonitorJournalTakesPrecedenceOverPilotBaseline(t *testing.T) {
	// The reconciled journal wins: a baseline claiming a different sequence
	// cannot disturb a coherent journal match.
	matched := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
		s.PilotActive = true
		s.PilotBaselineKnown = true
		s.PilotBaselineTicketSequenceRaw = 3
	})
	if hold, blocked := bridgeMonitorHold(matched); blocked {
		t.Fatalf("journal match held despite a divergent baseline: %+v", hold)
	}
	// A journal mismatch holds even when the baseline claims the observed
	// sequence: the baseline never papers over journal divergence.
	divergent := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
		s.TicketLastConsumedSequenceRaw = 5
		s.PilotActive = true
		s.PilotBaselineKnown = true
		s.PilotBaselineTicketSequenceRaw = 5
	})
	if got := Decide(divergent); got.Action != HoldManualRecovery || got.Reason != "out_of_band_crank" {
		t.Fatalf("journal mismatch excused by the baseline: %+v", got)
	}
}

func TestMonitorPilotBaselineRequiresActivePilot(t *testing.T) {
	// Closed or inactive pilot with a stale baseline still known: the ticket
	// consumed sequence is out of band again.
	stale := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
		s.JournalSequenceKnown = false
		s.TicketLastConsumedSequenceRaw = 4
		s.PilotActive = false
		s.PilotBaselineKnown = true
		s.PilotBaselineTicketSequenceRaw = 4
	})
	if got := Decide(stale); got.Action != HoldManualRecovery || got.Reason != "out_of_band_crank" {
		t.Fatalf("stale baseline without an active pilot explained the ticket: %+v", got)
	}
	// Legacy zero remains the only no-baseline explanation.
	unconsumed := monitorSnapshot(t, func(_ []ConfirmedAccount, s *Snapshot) {
		s.JournalSequenceKnown = false
		s.TicketLastConsumedSequenceRaw = 0
	})
	if hold, blocked := bridgeMonitorHold(unconsumed); blocked {
		t.Fatalf("legacy zero ticket held without a baseline: %+v", hold)
	}
}

type pilotBaselineJournal struct {
	stubProductionJournal
	active   bool
	baseline *pilotActivationBaseline
	err      error
}

func (p *pilotBaselineJournal) PilotRuntimeState(context.Context, string) (bool, *pilotActivationBaseline, error) {
	return p.active, p.baseline, p.err
}

func TestMergeJournalCarriesAndClearsPilotBaseline(t *testing.T) {
	ctx := context.Background()
	state := productionObserveState{routeKey: "test"}
	o := Observation{}
	o.Snapshot.PilotBaselineKnown = true
	o.Snapshot.PilotBaselineTicketSequenceRaw = 9
	journal := &pilotBaselineJournal{active: true, baseline: &pilotActivationBaseline{TicketLastConsumedSequenceRaw: 1, FinalizedSlot: 447400000}}
	state.journal = journal
	if err := state.mergeJournal(ctx, &o); err != nil {
		t.Fatal(err)
	}
	if !o.Snapshot.PilotActive || !o.Snapshot.PilotBaselineKnown || o.Snapshot.PilotBaselineTicketSequenceRaw != 1 {
		t.Fatalf("merge lost the validated baseline: %+v", o.Snapshot)
	}
	// A closed pilot clears the baseline: the stale field values the
	// observation carried into the merge must not survive.
	journal.active, journal.baseline = false, &pilotActivationBaseline{TicketLastConsumedSequenceRaw: 1, FinalizedSlot: 447400000}
	o.Snapshot.PilotBaselineKnown, o.Snapshot.PilotBaselineTicketSequenceRaw = true, 9
	if err := state.mergeJournal(ctx, &o); err != nil {
		t.Fatal(err)
	}
	if o.Snapshot.PilotActive || o.Snapshot.PilotBaselineKnown || o.Snapshot.PilotBaselineTicketSequenceRaw != 0 {
		t.Fatalf("closed pilot retained a stale baseline: %+v", o.Snapshot)
	}
	// A read failure is not silently absorbed into a default snapshot: the
	// merge surfaces the error so the observation is discarded.
	journal.err = budgetHold("incoherent_pilot_activation_evidence")
	o.Snapshot.PilotBaselineKnown, o.Snapshot.PilotBaselineTicketSequenceRaw = true, 9
	if err := state.mergeJournal(ctx, &o); err == nil {
		t.Fatal("corrupt activation evidence silently fell back")
	}
}

// The extended read returns the archived consumed sequence out of the
// validated activation's flat evidence, refuses a sequence that cannot belong
// to the baseline, and returns no baseline for a closed pilot.
func TestPilotRuntimeStateReturnsArchivedBaselineSequence(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("pilot-baseline-%d", time.Now().UnixNano())
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{}',2)`, key); err != nil {
		t.Fatal(err)
	}
	sequence := func(e pilotFlatEvidence, value uint64) pilotFlatEvidence {
		t.Helper()
		ticket := accountAt(e.Accounts, reportTicketPDA)
		binary.LittleEndian.PutUint64(ticket.Data[48:56], value)
		return e
	}
	flat := pilotFlatFixture(t)
	flat = sequence(flat, 1)
	state := func(prior Phase3Budget, flat pilotFlatEvidence, closed bool, withMarker bool) map[string]any {
		t.Helper()
		previous, _ := json.Marshal(prior)
		flatJSON, _ := json.Marshal(flat)
		a := pilotTestAuthority(prior)
		a.Generation = 2
		a.FinalizedSlot = flat.Slot
		a.FlatEvidenceSHA256 = sha256Bytes(flatJSON)
		b, err := activatePilotBudget(prior, a)
		if err != nil {
			t.Fatal(err)
		}
		b.Closed = closed
		if !withMarker {
			return map[string]any{"phase3": b}
		}
		return map[string]any{"phase3": b, "pilotBudgetActivation": pilotBudgetActivation{a, previous, flat}}
	}
	put := func(raw map[string]any) {
		t.Helper()
		encoded, _ := json.Marshal(raw)
		if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$2 WHERE route_key=$1`, key, encoded); err != nil {
			t.Fatal(err)
		}
	}
	put(state(emptyTestBudget(), flat, false, true))
	active, baseline, err := db.PilotRuntimeState(ctx, key)
	if err != nil || !active || baseline == nil || baseline.TicketLastConsumedSequenceRaw != 1 || baseline.FinalizedSlot != flat.Slot {
		t.Fatalf("validated baseline sequence not returned: active=%v baseline=%+v err=%v", active, baseline, err)
	}
	if wrappedActive, wrappedErr := db.PilotRuntimeEnabled(ctx, key); wrappedErr != nil || !wrappedActive {
		t.Fatalf("bool wrapper diverged from the extended read: %v", wrappedErr)
	}
	// A consumed sequence at or beyond the activation slot cannot belong to
	// this baseline.
	put(state(emptyTestBudget(), sequence(pilotFlatFixture(t), uint64(flat.Slot)+1), false, true))
	if _, baseline, err = db.PilotRuntimeState(ctx, key); err == nil || baseline != nil {
		t.Fatalf("sequence beyond the activation slot accepted: %+v %v", baseline, err)
	}
	// A closed pilot returns no baseline at all.
	put(state(emptyTestBudget(), flat, true, true))
	if active, baseline, err = db.PilotRuntimeState(ctx, key); err != nil || active || baseline != nil {
		t.Fatalf("closed pilot kept its baseline: active=%v baseline=%+v err=%v", active, baseline, err)
	}
	// Missing activation marker stays an incoherent-evidence hold.
	put(state(emptyTestBudget(), flat, false, false))
	if _, baseline, err = db.PilotRuntimeState(ctx, key); err == nil || baseline != nil {
		t.Fatalf("missing activation marker accepted: %+v %v", baseline, err)
	}
	assertBudgetHold(t, err, "incoherent_pilot_activation_evidence")
}
