package backyardrwa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func selectorEntryFixture(now time.Time, lane string, amount int64) SelectorEntry {
	q := MoveQuote{BorrowReceiveRaw: uint64(amount / 2), SourceLane: SelectedRouteID, DestinationLane: lane, ObservationID: "quoted-source", EquityRaw: amount, CostRaw: 1, ObservedAt: now.Add(-time.Second), EvidenceID: sha256Bytes([]byte("complete-recipe")), SampleSlot: 42, ValidThroughSlot: 74}
	return SelectorEntry{Lane: lane, EquityRaw: amount, ObservationID: q.ObservationID, Quote: q, AcceptedAt: now, ExpiresAt: q.ObservedAt.Add(30 * time.Second)}
}

func TestSelectorEntryExpiryAndCapacityPreserveLifecycle(t *testing.T) {
	now := time.Now().UTC()
	for _, lane := range selectorLanes {
		entry := selectorEntryFixture(now, lane, 3_000_000)
		original := base()
		original.Slot = 42
		original.PilotActive = true
		original.RouteLane, original.StrategyKey = lane, lane
		original.VoltrIdleRaw = 100_000_000
		original.CapacityRaw, original.PolicyLimitRaw, original.MaxTargetLTVEntryRaw = 10_000_000, 10_000_000, 10_000_000
		for _, tc := range []struct {
			name   string
			at     time.Time
			amount int64
		}{
			{"before acceptance", now.Add(-time.Nanosecond), 0}, {"current", now, 3_000_000}, {"expiry boundary", entry.ExpiresAt, 0},
		} {
			s := original
			if err := applySelectorEntry(&s, &entry, tc.at); err != nil {
				t.Fatal(err)
			}
			d := Decide(s)
			if tc.amount == 0 && d.Action != Hold || tc.amount != 0 && (d.Action != VoltrAllocateToSquads || d.AmountRaw != tc.amount) {
				t.Fatalf("%s %s: %+v", lane, tc.name, d)
			}
		}
		s := original
		if err := applySelectorEntry(&s, nil, now); err != nil || Decide(s).Action != Hold {
			t.Fatal("missing choice allocated", err)
		}
		s = original
		s.SelectorEntryPaused = true
		if err := applySelectorEntry(&s, &entry, now); err != nil || !s.SelectorEntryPaused {
			t.Fatal("choice cleared explicit pause", err)
		}
		s = original
		consumed := entry
		consumed.AllocationOperationID = "first-attempt"
		if err := applySelectorEntry(&s, &consumed, now); err != nil || Decide(s).Action != Hold {
			t.Fatal("returned attempt reused entry", err)
		}
		s = original
		s.Slot = entry.Quote.ValidThroughSlot + 1
		if err := applySelectorEntry(&s, &entry, now); err != nil || Decide(s).Action != Hold {
			t.Fatal("slot-expired choice allocated despite fresh timestamp", err)
		}
		s = original
		s.Slot = entry.Quote.ValidThroughSlot + 1
		s.SquadsIdleRaw = entry.EquityRaw
		if err := applySelectorEntry(&s, &entry, now); err != nil || Decide(s).Action != SwapStableToCollateralStep {
			t.Fatal("slot expiry stranded allocated tranche", err)
		}
		s = original
		s.CapacityRaw = entry.EquityRaw - 1
		if err := applySelectorEntry(&s, &entry, now); err != nil || Decide(s).Action != Hold {
			t.Fatal("capacity silently shrank quoted entry", err)
		}
		s = original
		s.VoltrIdleRaw -= entry.EquityRaw
		s.SquadsIdleRaw = entry.EquityRaw
		if err := applySelectorEntry(&s, &entry, entry.ExpiresAt); err != nil || Decide(s).Action != SwapStableToCollateralStep {
			t.Fatal("expiry stranded allocated tranche", err, Decide(s))
		}
		s.CapacityRaw = 1
		if d := Decide(s); d.Action != StageSquadsToVoltr || d.AmountRaw != entry.EquityRaw {
			t.Fatal("capacity race stranded working cash", d)
		}
		s = original
		s.WithdrawalDemandRaw = original.VoltrIdleRaw + 1
		s.SquadsIdleRaw = 1
		s.SelectorEntryPaused = true
		if d := Decide(s); d.Action != StageSquadsToVoltr {
			t.Fatal("entry gate stopped withdrawal", d)
		}
		s = original
		s.ObligationPresenceKnown = true
		s.InitializationPolicyReady = true
		if d := Decide(s); d.Action == InitializeKaminoObligation {
			t.Fatal("no quote spent initializer rent", d)
		}
		if err := applySelectorEntry(&s, &entry, now); err != nil || Decide(s).Action != InitializeKaminoObligation {
			t.Fatal("quoted setup not selected", err, Decide(s))
		}
		s.CapacityRaw = entry.EquityRaw - 1
		if d := Decide(s); d.Action == InitializeKaminoObligation {
			t.Fatal("setup spent rent on shrunken capacity", d)
		}
	}
}

type selectorEntryJournal struct {
	pilotPlanningJournal
	entry    *SelectorEntry
	entryErr error
}

func (j *selectorEntryJournal) LoadSelectorEntry(context.Context, string) (*SelectorEntry, error) {
	return j.entry, j.entryErr
}
func TestSelectorEntryEnrichesPreparationAndRefusesCorruptState(t *testing.T) {
	entry := selectorEntryFixture(time.Now().UTC(), SelectedRouteID, 1_000_000)
	j := &selectorEntryJournal{pilotPlanningJournal: pilotPlanningJournal{active: true}, entry: &entry}
	state := productionObserveState{routeKey: "fixture", journal: j}
	o := Observation{Snapshot: Snapshot{RouteLane: SelectedRouteID, Slot: 42}}
	if err := state.mergeJournal(context.Background(), &o); err != nil || o.Snapshot.SelectorEntryEquityRaw != entry.EquityRaw {
		t.Fatal("planning omitted quote", err)
	}
	entry.Lane = "unapproved"
	if err := state.mergeJournal(context.Background(), &o); err == nil {
		t.Fatal("corrupt durable choice ignored")
	}
}

func TestSelectorEvaluationDurabilityFencesAndBudgetContinuity(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("selector-entry-%d", time.Now().UnixNano())
	prior := emptyTestBudget()
	prior.Families["Maple"] = FamilyBudget{SpentMicros: 1_000_000}
	previous, _ := json.Marshal(prior)
	flat := pilotFlatFixture(t)
	flatJSON, _ := json.Marshal(flat)
	a := pilotTestAuthority(prior)
	a.Generation, a.FinalizedSlot, a.FlatEvidenceSHA256 = 2, flat.Slot, sha256Bytes(flatJSON)
	budget, err := activatePilotBudget(prior, a)
	if err != nil {
		t.Fatal(err)
	}
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.PilotActive = true
	in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = 100_000_000, 100_000_000
	in.Quotes[0].EquityRaw = PilotWorkingTrancheCapRaw
	in.Quotes[0].BorrowReceiveRaw = uint64(PilotWorkingTrancheCapRaw / 2)
	in.Quotes[0].EvidenceID = sha256Bytes([]byte("complete-exit-entry"))
	// Equal net equity does not make two gross-input quotes interchangeable.
	other := in.Quotes[0]
	other.EquityRaw -= 5_000
	other.CostRaw -= 5_000
	other.EvidenceID = sha256Bytes([]byte("different-gross-input"))
	in.Quotes = append([]MoveQuote{other}, in.Quotes...)
	history := SelectorResult{State: SelectorState{SourceLane: in.Snapshot.RouteLane, Advantages: map[string]AdvantageWindow{in.Markets[0].Lane: {Since: in.Now.Add(-2 * time.Minute), LastSample: in.Now.Add(-time.Second)}}}}
	state := map[string]any{"generation": 2, "phase3": budget, "pilotBudgetActivation": pilotBudgetActivation{a, previous, flat}, "selector": map[string]any{"mode": "live", "result": history}, "selectorEntryPaused": true}
	raw, _ := json.Marshal(state)
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "entry-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	refresh := func() { advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now)) }
	refresh()
	_, err = db.RecordSelectorEvaluation(ctx, key, in, in.Snapshot.Slot, 1)
	assertBudgetHold(t, err, "selector_state_changed_during_quote")
	// An unresolved signed transaction prevents selection and preserves the quote history.
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,expected_effects) VALUES($1,$2,'signed','OPEN_ROUTE_STEP','{}')`, key+"-pending", key); err != nil {
		t.Fatal(err)
	}
	refresh()
	_, err = db.RecordSelectorEvaluation(ctx, key, in, in.Snapshot.Slot, 2)
	assertBudgetHold(t, err, "selector_finish_current_work_first")
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciled' WHERE operation_id=$1`, key+"-pending"); err != nil {
		t.Fatal(err)
	}
	// The derived manual latch matters even if no physical latch row was written.
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,recovery_reason,expected_effects) VALUES($1,$2,'manual_recovery','HOLD_MANUAL_RECOVERY','fixture_hold','{}')`, key+"-hold", key); err != nil {
		t.Fatal(err)
	}
	refresh()
	_, err = db.RecordSelectorEvaluation(ctx, key, in, in.Snapshot.Slot, 2)
	assertBudgetHold(t, err, "selector_manual_recovery_active")
	if _, err = db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE operation_id=$1`, key+"-hold"); err != nil {
		t.Fatal(err)
	}
	refresh()
	noQuote := in
	noQuote.Quotes = nil
	result, err := db.RecordSelectorEvaluation(ctx, key, noQuote, in.Snapshot.Slot, 2)
	if err != nil || result.Action != "KEEP" {
		t.Fatal("shadow economics authorized entry", err, result)
	}
	if e, err := db.LoadSelectorEntry(ctx, key); err != nil || e != nil {
		t.Fatal("quote-free choice persisted", err)
	}
	refresh()
	oldQuote := in
	oldQuote.Quotes = append([]MoveQuote(nil), in.Quotes...)
	for i := range oldQuote.Quotes {
		oldQuote.Quotes[i].ValidThroughSlot = in.Snapshot.Slot
	}
	result, err = db.RecordSelectorEvaluation(ctx, key, oldQuote, in.Snapshot.Slot+1, 2)
	if err != nil || result.Action != "KEEP" || result.SelectedQuote != nil {
		t.Fatal("final collection slot outlived source quote", err, result)
	}
	if e, err := db.LoadSelectorEntry(ctx, key); err != nil || e != nil {
		t.Fatal("slot-expired choice persisted", err)
	}
	refresh()
	result, err = db.RecordSelectorEvaluation(ctx, key, in, in.Snapshot.Slot, 2)
	if err != nil || result.Action != "ENTER" {
		t.Fatal("complete quote did not select", err, result)
	}
	if _, err = db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	entry, err := restarted.LoadSelectorEntry(ctx, key)
	if err != nil || entry == nil || entry.Lane != in.Markets[0].Lane || entry.EquityRaw != PilotWorkingTrancheCapRaw || entry.Quote.EvidenceID != in.Quotes[1].EvidenceID {
		t.Fatal("restart lost exact entry", err, entry)
	}
	var saved []byte
	var version int64
	var paused bool
	if err = restarted.pool.QueryRow(ctx, `SELECT state->'phase3',state_version,(state->>'selectorEntryPaused')::boolean FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&saved, &version, &paused); err != nil {
		t.Fatal(err)
	}
	var after Phase3Budget
	if json.Unmarshal(saved, &after) != nil {
		t.Fatal("budget decode")
	}
	beforeJSON, _ := json.Marshal(budget)
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) || version != 3 || paused {
		t.Fatal("choice changed budget or failed generation/unpause", version, paused)
	}
	refresh()
	_, err = db.RecordSelectorEvaluation(ctx, key, in, in.Snapshot.Slot, 3)
	if err == nil {
		t.Fatal("released lease wrote entry")
	}
}

func storeTestSelectorEntry(t *testing.T, ctx context.Context, db *Database, key string, entry SelectorEntry) {
	t.Helper()
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{selectorEntry}',$2::jsonb,true) WHERE route_key=$1`, key, raw); err != nil {
		t.Fatal(err)
	}
}

func TestSelectorEntryAllocationIsOneAttemptUnderRouteLock(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("selector-once-%d", time.Now().UnixNano())
	entry := selectorEntryFixture(time.Now().UTC(), SelectedRouteID, 1_000_000)
	raw, _ := json.Marshal(map[string]any{"selectorEntry": entry})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,$2)`, key, raw); err != nil {
		t.Fatal(err)
	}
	// Terminal identities isolate the quote association under the route lock;
	// production admission separately enforces one nonterminal operation.
	for _, id := range []string{key + "-first", key + "-second"} {
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'failed','VOLTR_ALLOCATE_TO_SQUADS',$3,'{}')`, id, key, SelectedRouteID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AcquireRouteLease(ctx, key, "single-entry", time.Minute); err != nil {
		t.Fatal(err)
	}
	defer db.ReleaseRouteLease(ctx)
	budget := Phase3Budget{Pilot: &pilotBudgetAuthority{}}
	request := BridgeBuildRequest{Action: VoltrAllocateToSquads, AmountRaw: 1_000_000}
	for _, tc := range []struct {
		id                string
		admission, commit bool
		want              string
	}{
		{key + "-first", false, false, "selector_entry_allocation_not_bound"},
		{key + "-first", true, false, ""}, // failed surrounding admission rolls back consumption
		{key + "-second", true, true, ""},
		{key + "-second", true, false, ""},  // same-operation retry
		{key + "-second", false, false, ""}, // build/send of that attempt
		{key + "-first", true, false, "selector_entry_already_allocated"},
	} {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.lockOperationLease(ctx, tx, tc.id); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		err = db.authorizeSelectorEntryTx(ctx, tx, tc.id, budget, request, ExpectedEffects{}, 42, tc.admission)
		if tc.want != "" {
			assertBudgetHold(t, err, tc.want)
		} else if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if tc.commit && err == nil {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.LoadSelectorEntry(ctx, key)
	if err != nil || got.AllocationOperationID != key+"-second" {
		t.Fatal("allocation association not durable", err, got)
	}
	for _, admission := range []bool{true, false} {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.lockOperationLease(ctx, tx, key+"-second"); err != nil {
			t.Fatal(err)
		}
		err = db.authorizeSelectorEntryTx(ctx, tx, key+"-second", budget, request, ExpectedEffects{}, entry.Quote.ValidThroughSlot+1, admission)
		_ = tx.Rollback(ctx)
		assertBudgetHold(t, err, "selector_entry_quote_expired")
	}
}

func TestSelectorBorrowUsesReviewedAmountAfterQuoteExpiry(t *testing.T) {
	entry := selectorEntryFixture(time.Now().Add(-time.Minute), SelectedRouteID, 1_000_000)
	entry.AllocationOperationID = "funded"
	s := Snapshot{PilotActive: true, RouteLane: entry.Lane, HasPosition: true, PositionCollateralRaw: 1_000_000, Slot: 1000}
	if err := applySelectorEntry(&s, &entry, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := selectorBorrowAmount(s, 600_000)
	if err != nil || got != 500_000 {
		t.Fatal("borrow silently grew with actual collateral", got, err)
	}
	_, err = selectorBorrowAmount(s, 499_999)
	assertBudgetHold(t, err, "selector_entry_borrow_unavailable")
	for _, change := range []func(*Snapshot){
		func(s *Snapshot) { s.SelectorBorrowRaw = 0 }, func(s *Snapshot) { s.SelectorEntryPaused = true },
		func(s *Snapshot) { s.Unwind = true }, func(s *Snapshot) { s.WithdrawalDemandRaw = 1 },
	} {
		changed := s
		change(&changed)
		_, err = selectorBorrowAmount(changed, 600_000)
		assertBudgetHold(t, err, "selector_entry_borrow_unavailable")
	}
	s.PilotActive = false
	if got, err = selectorBorrowAmount(s, 600_000); err != nil || got != 600_000 {
		t.Fatal("historical execution changed", got, err)
	}
}

func TestSelectorBorrowAuthorizationPersistsAcrossRestart(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("selector-borrow-%d", time.Now().UnixNano())
	entry := selectorEntryFixture(time.Now().Add(-time.Minute), SelectedRouteID, 1_000_000)
	entry.AllocationOperationID = key + "-allocation"
	entry.Quote.BorrowFeeRaw = 1
	raw, _ := json.Marshal(map[string]any{"selectorEntry": entry})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,$2)`, key, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'failed','OPEN_ROUTE_STEP',$3,'{}')`, key+"-borrow", key, SelectedRouteID); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.AcquireRouteLease(ctx, key, "borrow-after-restart", time.Minute); err != nil {
		t.Fatal(err)
	}
	defer restarted.ReleaseRouteLease(ctx)
	for _, tc := range []struct {
		amount uint64
		fee    uint64
		bind   bool
		want   string
	}{
		{500_000, 0, true, ""}, {500_000, 1, true, ""}, {500_000, 2, true, "selector_entry_borrow_fee_exceeded"}, {500_001, 0, true, "selector_entry_borrow_mismatch"}, {499_999, 0, true, "selector_entry_borrow_mismatch"}, {500_000, 0, false, "selector_entry_borrow_mismatch"},
	} {
		stored := entry
		if !tc.bind {
			stored.AllocationOperationID = ""
		}
		storeTestSelectorEntry(t, ctx, restarted, key, stored)
		r, err := basicPolicyFixtureManifest(t).kaminoPacketForRoute(OpenRouteStep, kaminoLegBorrow, tc.amount, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, SelectedRouteID)
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, accounts := selectorDestinationFixture(t)
		route, _ := runtimeRoute(SelectedRouteID)
		effects, err := kaminoBorrowEffects(accounts, route, tc.amount)
		if err != nil {
			t.Fatal(err)
		}
		effects.Accounts[0].AfterRaw -= tc.fee
		effects.Accounts[2].AfterRaw += tc.fee
		for _, admission := range []bool{true, false} {
			tx, err := restarted.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = restarted.lockOperationLease(ctx, tx, key+"-borrow"); err != nil {
				t.Fatal(err)
			}
			err = restarted.authorizeSelectorEntryTx(ctx, tx, key+"-borrow", Phase3Budget{Pilot: &pilotBudgetAuthority{}}, r, effects, 1000, admission)
			_ = tx.Rollback(ctx)
			if tc.want != "" {
				assertBudgetHold(t, err, tc.want)
			} else if err != nil {
				t.Fatal(err)
			}
		}
	}
}
