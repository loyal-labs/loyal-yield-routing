package backyardrwa

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestCandidateUnwindManifestReloadApplyCompletion drives one real route row
// on the disposable database through the exact production wiring: a
// candidate-source unwind intent commits, survives restart, merges into the
// snapshot and completes only under the reviewed manifest, while every
// embedded entry point keeps its installed installed-lane closure. The same
// activated pilot budget pins the manifest-authorized tranche-cap stamp
// end to end.
func TestCandidateUnwindManifestReloadApplyCompletion(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("selector-auto-unwind-%d", time.Now().UnixNano())
	manifest := autoInitializerFixtureManifest(t)
	embedded, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	// A real activated pilot budget with an AUTO exit reservation: the same
	// seeding sequence the pilot entry tests use. The exit family joins after
	// activation — a pilot transition cannot clear an exit reserve it never
	// carried, so the activated budget is the one that books it.
	prior := emptyTestBudget()
	flat := pilotFlatFixture(t)
	flatJSON, err := json.Marshal(flat)
	if err != nil {
		t.Fatal(err)
	}
	authority := pilotTestAuthority(prior)
	authority.Generation = 2
	authority.FinalizedSlot = flat.Slot
	authority.FlatEvidenceSHA256 = sha256Bytes(flatJSON)
	activated, err := activatePilotBudget(prior, authority)
	if err != nil {
		t.Fatal(err)
	}
	activated.Families["AUTO"] = FamilyBudget{SpentMicros: 7_000_000, ExitMicros: 3_000_000}
	state, err := json.Marshal(map[string]any{"generation": 2, "phase3": activated, "pilotBudgetActivation": pilotBudgetActivation{authority, mustJSON(t, prior), flat}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, state); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "unwind-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	family := phase3BudgetFamilyForLane(autoAUTOPYUSD.Lane)
	intent := UnwindIntent{SourceLane: autoAUTOPYUSD.Lane, Reason: "hard_ltv_reduction", ObservationID: "source", MaxCollateralRaw: 100, MaxDebtRaw: 50, CostBoundRaw: 2_000_000, BudgetScope: Phase3GoalID, BudgetFamily: family, EvidenceID: sha256Bytes([]byte("auto-exit")), CreatedAt: time.Now().UTC()}
	// Installed closure: the embedded commit refuses the candidate source.
	if err = db.CommitUnwindIntent(ctx, key, intent); err == nil {
		t.Fatal("embedded commit accepted the candidate source")
	}
	if err = db.CommitUnwindIntentOnManifest(ctx, manifest, key, intent); err != nil {
		t.Fatal("manifest commit refused a funded candidate unwind:", err)
	}
	if _, err = db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.AcquireRouteLease(ctx, key, "unwind-b", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Installed closure: the embedded reload refuses; the manifest reload
	// returns the exact persisted intent.
	if _, err = restarted.LoadUnwindIntent(ctx, key); err == nil {
		t.Fatal("embedded reload accepted the candidate source")
	}
	got, err := restarted.LoadUnwindIntentOnManifest(ctx, manifest, key)
	if err != nil || got == nil || *got != intent {
		t.Fatalf("restart lost the candidate intent: %+v %v", got, err)
	}
	// Apply: the embedded merge refuses; the manifest merge arms the snapshot.
	s := base()
	s.RouteLane = autoAUTOPYUSD.Lane
	if err = applyUnwindIntent(&s, got); err == nil {
		t.Fatal("embedded apply accepted the candidate source")
	}
	if err = applyUnwindIntentWithLane(&s, got, manifest.selectorEntryLaneAllowed); err != nil || !s.Unwind {
		t.Fatalf("manifest apply did not arm the unwind: %v", err)
	}
	// Planning read, both binding states: the explicit absent fixture (the
	// shipped pre-install state) refuses the candidate unwind, the reviewed
	// manifest decodes it exactly, and the embedded manifest — which carries
	// the installed binding after the release — decodes it identically.
	if _, err := restarted.readRoutePlanningStateOnManifest(ctx, autoAbsentBindingManifest(t), key, false); err == nil {
		t.Fatal("absent binding planning read accepted the candidate unwind")
	}
	planning, err := restarted.readRoutePlanningStateOnManifest(ctx, manifest, key, false)
	if err != nil || planning.unwind == nil || *planning.unwind != intent {
		t.Fatalf("manifest planning read lost the candidate unwind: %+v %v", planning, err)
	}
	embeddedPlanning, err := restarted.readRoutePlanningState(ctx, key, false)
	if err != nil || embeddedPlanning.unwind == nil || *embeddedPlanning.unwind != intent {
		t.Fatalf("embedded planning read lost the installed candidate unwind: %+v %v", embeddedPlanning, err)
	}
	// mergeJournal planning path: the unwind merges through the manifest
	// authority and the reviewed manifest stamps its funded candidate lane.
	// The route observer has already established the observed source lane on
	// the snapshot, exactly as production observations carry it.
	merged := &Observation{planning: planning}
	merged.Snapshot.RouteLane = autoAUTOPYUSD.Lane
	observe := productionObserveState{manifest: manifest, routeKey: key, journal: restarted}
	if err = observe.mergeJournal(ctx, merged); err != nil {
		t.Fatal(err)
	}
	if !merged.Snapshot.Unwind || !merged.Snapshot.PilotActive {
		t.Fatalf("merge lost the pilot unwind facts: %+v", merged.Snapshot)
	}
	if merged.Snapshot.PilotTrancheCapLane != autoAUTOPYUSD.Lane {
		t.Fatalf("reviewed manifest did not stamp the candidate tranche lane: %q", merged.Snapshot.PilotTrancheCapLane)
	}
	// The audited cap gap and its narrow manifest-authorized resolution.
	if workingTrancheCap(Snapshot{PilotActive: true, RouteLane: autoAUTOPYUSD.Lane}) != Phase3WorkingTrancheCapRaw {
		t.Fatal("unauthorized candidate snapshot must stay at the ordinary cap")
	}
	authorized := Snapshot{PilotActive: true, RouteLane: autoAUTOPYUSD.Lane, PilotTrancheCapLane: autoAUTOPYUSD.Lane}
	if workingTrancheCap(authorized) != PilotWorkingTrancheCapRaw {
		t.Fatal("manifest-authorized candidate snapshot did not size the pilot tranche")
	}
	if workingTrancheCap(Snapshot{RouteLane: autoAUTOPYUSD.Lane, PilotTrancheCapLane: autoAUTOPYUSD.Lane}) != Phase3WorkingTrancheCapRaw {
		t.Fatal("inactive pilot sized the pilot tranche")
	}
	installed := Snapshot{PilotActive: true, RouteLane: SelectedRouteID}
	if workingTrancheCap(installed) != PilotWorkingTrancheCapRaw {
		t.Fatal("installed lane cap changed")
	}
	// Completion: the embedded completion keeps refusing, a pending
	// transaction blocks, and the manifest completion clears the intent.
	flatSnapshot := base()
	flatSnapshot.RouteLane = autoAUTOPYUSD.Lane
	if err = restarted.CompleteUnwindIntent(ctx, key, intent, flatSnapshot); err == nil {
		t.Fatal("embedded completion accepted the candidate source")
	}
	if _, err = restarted.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,expected_effects) VALUES($1,$2,'signed','OPEN_ROUTE_STEP','{}')`, key+"-signed", key); err != nil {
		t.Fatal(err)
	}
	if err = restarted.CompleteUnwindIntentOnManifest(ctx, manifest, key, intent, flatSnapshot); err == nil {
		t.Fatal("completion ran during an unresolved transaction")
	}
	if _, err = restarted.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciled' WHERE operation_id=$1`, key+"-signed"); err != nil {
		t.Fatal(err)
	}
	if err = restarted.CompleteUnwindIntentOnManifest(ctx, manifest, key, intent, flatSnapshot); err != nil {
		t.Fatal("manifest completion refused the reconciled flat unwind:", err)
	}
	if cleared, err := restarted.LoadUnwindIntentOnManifest(ctx, manifest, key); err != nil || cleared != nil {
		t.Fatalf("completed intent survived: %+v %v", cleared, err)
	}
	if paused, err := restarted.SelectorEntryPaused(ctx, key); err != nil || !paused {
		t.Fatal("completed exit allowed a stale entry")
	}
	// mergeJournal fallback path: the manifest-aware reader is preferred (the
	// legacy reader would refuse this row), applies nothing once cleared, and
	// the embedded manifest leaves the tranche lane unstamped.
	fallback := &Observation{}
	if err = observe.mergeJournal(ctx, fallback); err != nil {
		t.Fatal(err)
	}
	if fallback.Snapshot.Unwind || fallback.Snapshot.PilotTrancheCapLane != autoAUTOPYUSD.Lane {
		t.Fatalf("fallback merge lost the manifest facts: %+v", fallback.Snapshot)
	}
	embeddedObserve := productionObserveState{manifest: embedded, routeKey: key, journal: restarted}
	embeddedPlanning, err = restarted.readRoutePlanningState(ctx, key, false)
	if err != nil {
		t.Fatal(err)
	}
	embeddedMerge := &Observation{planning: embeddedPlanning}
	if err = embeddedObserve.mergeJournal(ctx, embeddedMerge); err != nil {
		t.Fatal(err)
	}
	// Both readers now resolve the same installed binding: the embedded merge
	// must land exactly where the manifest-aware merge landed.
	if embeddedMerge.Snapshot.Unwind != fallback.Snapshot.Unwind || embeddedMerge.Snapshot.PilotTrancheCapLane != fallback.Snapshot.PilotTrancheCapLane {
		t.Fatalf("embedded merge drifted from the manifest merge: %+v vs %+v", embeddedMerge.Snapshot, fallback.Snapshot)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
