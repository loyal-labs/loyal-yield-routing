package backyardrwa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestAutoUnwindRenewalResolvesThroughReviewedManifest drives one real route
// row through renewal for the committed candidate AUTO source: absent and
// malformed bindings reject exactly like the installed closure, the reviewed
// manifest renews, and the legacy embedded wrapper renews the same intent
// through the installed binding. Locked evidence, reservation, lease, latch
// and version checks stay byte-identical.
func TestAutoUnwindRenewalResolvesThroughReviewedManifest(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("selector-auto-renew-%d", time.Now().UnixNano())
	reviewed := autoInitializerFixtureManifest(t)
	absent := autoAbsentBindingManifest(t)
	malformedBinding := autoInitializerFixtureBinding(t)
	malformedBinding.Lane = "AUTO/AUTO/USDC"
	malformed := autoInitializerFixtureManifest(t)
	malformed.RuntimeBindings.AutoPolicy = &malformedBinding

	// The same activated pilot budget the wiring test seeds, with the funded
	// AUTO exit reservation the renewal must keep respecting.
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
	if _, err = db.AcquireRouteLease(ctx, key, "renew-auto", time.Minute); err != nil {
		t.Fatal(err)
	}
	intent := UnwindIntent{SourceLane: autoAUTOPYUSD.Lane, Reason: "economic_rotation", ObservationID: "auto-source", MaxCollateralRaw: 200, MaxDebtRaw: 50, CostBoundRaw: 2_000_000, BudgetScope: Phase3GoalID, BudgetFamily: phase3BudgetFamilyForLane(autoAUTOPYUSD.Lane), EvidenceID: sha256Bytes([]byte("auto-exit")), CreatedAt: time.Now().UTC()}
	if err = db.CommitUnwindIntentOnManifest(ctx, reviewed, key, intent); err != nil {
		t.Fatal(err)
	}
	var version int64
	if err = db.pool.QueryRow(ctx, `SELECT state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&version); err != nil {
		t.Fatal(err)
	}
	previous, err := db.LoadUnwindIntentOnManifest(ctx, reviewed, key)
	if err != nil || previous == nil || *previous != intent {
		t.Fatalf("restart lost the candidate intent: %+v %v", previous, err)
	}

	fresh := base()
	fresh.RouteLane, fresh.StrategyKey = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane
	fresh.PilotActive, fresh.HasPosition = true, true
	fresh.PositionCollateralRaw, fresh.PositionCollateralValueRaw = 150, 150
	fresh.PositionDebtRaw, fresh.PositionDebtValueRaw = previous.MaxDebtRaw+10, previous.MaxDebtRaw+10
	fresh.PayoffDebtRaw, fresh.LTVBPS = fresh.PositionDebtRaw+1, 3400
	if err = applyUnwindIntentWithLane(&fresh, previous, reviewed.selectorEntryLaneAllowed); err != nil || !fresh.Unwind || !fresh.UnwindRefreshRequired {
		t.Fatal("interest did not require readmission", err, fresh)
	}
	o := tickObservation(fresh)
	o.ObservedAt = time.Now().UTC()
	source := selectorSourceQuote{Lane: fresh.RouteLane, ObservationID: fresh.ObservationID, ExitBound: &selectorExitBound{MaxCollateralRaw: fresh.PositionCollateralRaw, MaxDebtRaw: fresh.PositionDebtRaw + 1000, GrossMicros: intent.CostBoundRaw}, Recipe: selectorRecipe{Costs: []ValuedTransactionCost{{ObservationSlot: fresh.Slot, TotalMicros: intent.CostBoundRaw}}, EvidenceID: sha256Bytes([]byte("auto-renewal")), ValidThroughSlot: fresh.Slot + 32}}

	// Absent and malformed bindings keep the installed closure: renewal is a
	// hold before any evidence is consulted.
	assertBudgetHold(t, db.renewSelectorUnwindOnManifest(ctx, absent, key, version, *previous, o, source, fresh.Slot), "unwind_refresh_evidence_unavailable")
	assertBudgetHold(t, db.renewSelectorUnwindOnManifest(ctx, malformed, key, version, *previous, o, source, fresh.Slot), "unwind_refresh_evidence_unavailable")

	// The reviewed binding renews in place: only the fresh observation,
	// evidence and bounds move; identity, scope, family and reservation stay.
	if err = db.renewSelectorUnwindOnManifest(ctx, reviewed, key, version, *previous, o, source, fresh.Slot); err != nil {
		t.Fatal(err)
	}
	renewed, err := db.LoadUnwindIntentOnManifest(ctx, reviewed, key)
	if err != nil || renewed == nil {
		t.Fatalf("renewed intent unavailable: %+v %v", renewed, err)
	}
	if renewed.MaxDebtRaw != source.ExitBound.MaxDebtRaw || renewed.MaxCollateralRaw != source.ExitBound.MaxCollateralRaw || renewed.CostBoundRaw != source.ExitBound.GrossMicros || renewed.EvidenceID != source.Recipe.EvidenceID {
		t.Fatalf("renewal did not adopt the fresh exit bound: %+v", renewed)
	}
	if renewed.SourceLane != intent.SourceLane || renewed.Reason != intent.Reason || renewed.BudgetScope != intent.BudgetScope || renewed.BudgetFamily != intent.BudgetFamily {
		t.Fatalf("renewal lost intent identity: %+v", renewed)
	}

	// The legacy wrapper keeps its signature and renews the same source
	// through the installed embedded binding.
	fresh2 := fresh
	fresh2.PositionDebtRaw = renewed.MaxDebtRaw + 5
	fresh2.PositionDebtValueRaw = fresh2.PositionDebtRaw
	fresh2.PayoffDebtRaw = fresh2.PositionDebtRaw + 1
	if err = applyUnwindIntentWithLane(&fresh2, renewed, reviewed.selectorEntryLaneAllowed); err != nil || !fresh2.UnwindRefreshRequired {
		t.Fatal("renewed envelope did not re-latch", err)
	}
	o2 := tickObservation(fresh2)
	o2.ObservedAt = time.Now().UTC()
	source2 := selectorSourceQuote{Lane: fresh2.RouteLane, ObservationID: fresh2.ObservationID, ExitBound: &selectorExitBound{MaxCollateralRaw: fresh2.PositionCollateralRaw, MaxDebtRaw: fresh2.PositionDebtRaw + 1000, GrossMicros: intent.CostBoundRaw}, Recipe: selectorRecipe{Costs: []ValuedTransactionCost{{ObservationSlot: fresh2.Slot, TotalMicros: intent.CostBoundRaw}}, EvidenceID: sha256Bytes([]byte("auto-renewal-2")), ValidThroughSlot: fresh2.Slot + 32}}
	if err = db.renewSelectorUnwind(ctx, key, version+1, *renewed, o2, source2, fresh2.Slot); err != nil {
		t.Fatal(err)
	}
	regenerated, err := db.LoadUnwindIntentOnManifest(ctx, reviewed, key)
	if err != nil || regenerated == nil || regenerated.MaxDebtRaw != source2.ExitBound.MaxDebtRaw || regenerated.EvidenceID != source2.Recipe.EvidenceID {
		t.Fatalf("wrapper renewal lost the fresh bound: %+v %v", regenerated, err)
	}

	// Installed lanes keep renewing through the manifest-parameterized check
	// regardless of the AUTO binding state.
	maple := UnwindIntent{SourceLane: SelectedRouteID, Reason: "economic_rotation", ObservationID: "maple", MaxCollateralRaw: 1, MaxDebtRaw: 1, CostBoundRaw: 1, BudgetScope: Phase3GoalID, BudgetFamily: phase3BudgetFamilyForLane(SelectedRouteID), EvidenceID: sha256Bytes([]byte("maple-exit")), CreatedAt: time.Now().UTC()}
	if reviewed.validateUnwindIntent(maple) != nil || absent.validateUnwindIntent(maple) != nil {
		t.Fatal("installed lane renewal authority changed")
	}
}

// TestRefreshSelectorUnwindResolvesAutoIntentThroughManifest pins the refresh
// gate: an admitted candidate AUTO intent survives the exact previous-intent
// validation only while the explicit manifest's reviewed binding resolves.
func TestRefreshSelectorUnwindResolvesAutoIntentThroughManifest(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	reviewed := autoInitializerFixtureManifest(t)
	absent := autoAbsentBindingManifest(t)
	malformedBinding := autoInitializerFixtureBinding(t)
	malformedBinding.Lane = "AUTO/AUTO/USDC"
	malformed := autoInitializerFixtureManifest(t)
	malformed.RuntimeBindings.AutoPolicy = &malformedBinding

	resetManualRecoveryProductionRoute(t, ctx, db, "refresh-auto-a")
	// Restore the production row to a valid persistent fixture even on
	// failure: a valid empty phase3 budget with generation aligned to
	// state_version and no lease held. The reset helper's bare
	// {"generation":1,"cycle":1} state carries no phase3, and this test's
	// candidate unwind must never leak into later suite tests.
	defer restoreProductionRouteFixture(t, ctx, db)
	intent := UnwindIntent{SourceLane: autoAUTOPYUSD.Lane, Reason: "hard_ltv_reduction", ObservationID: "refresh-source", MaxCollateralRaw: 100, MaxDebtRaw: 50, CostBoundRaw: 2_000_000, BudgetScope: Phase3GoalID, BudgetFamily: phase3BudgetFamilyForLane(autoAUTOPYUSD.Lane), EvidenceID: sha256Bytes([]byte("auto-exit")), CreatedAt: time.Now().UTC()}
	raw, err := json.Marshal(map[string]any{"generation": 1, "cycle": 1, "selectorUnwind": intent})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$2 WHERE route_key=$1`, productionRouteKey, raw); err != nil {
		t.Fatal(err)
	}
	refuse := func(t *testing.T, manifest RouteManifest) {
		t.Helper()
		observe := func(context.Context) (Observation, error) {
			t.Fatal("refresh gate passed with an unresolved binding")
			return Observation{}, nil
		}
		assertBudgetHold(t, db.refreshSelectorUnwind(ctx, nil, manifest, observe), "unwind_refresh_intent_unavailable")
	}
	refuse(t, absent)
	refuse(t, malformed)
	// The reviewed binding opens the gate: the refresh proceeds to the
	// confirmed observation instead of rejecting the candidate source.
	observe := func(context.Context) (Observation, error) {
		return Observation{}, errors.New("sentinel: past the refresh gate")
	}
	if err = db.refreshSelectorUnwind(ctx, nil, reviewed, observe); !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatalf("reviewed refresh did not reach the observation: %v", err)
	}
}

func restoreProductionRouteFixture(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	var version int64
	if err := db.pool.QueryRow(ctx, `SELECT state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, productionRouteKey).Scan(&version); err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(map[string]any{"generation": version, "cycle": 1, "phase3": emptyTestBudget()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$2, lease_owner=NULL, lease_expires_at=NULL WHERE route_key=$1`, productionRouteKey, state); err != nil {
		t.Fatal(err)
	}
}
