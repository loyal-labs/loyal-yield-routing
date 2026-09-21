package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func unwindIntentCommandRequest(lane string) UnwindIntentCommitRequest {
	return UnwindIntentCommitRequest{
		Lane:             lane,
		Reason:           "hard_ltv_reduction",
		ObservationID:    "observation-1",
		MaxCollateralRaw: 100,
		MaxDebtRaw:       50,
		CostBoundRaw:     2_000_000,
		EvidenceID:       sha256Bytes([]byte("exit-evidence")),
	}
}

// The dry-run default validates the exact intent shape and the installed
// embedded manifest's lane authority without opening any database connection.
func TestUnwindIntentCommitDryRunValidatesWithoutDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := RunUnwindIntentCommit(ctx, "", unwindIntentCommandRequest(SelectedRouteID), false)
	if err != nil {
		t.Fatal("embedded dry-run refused an installed-lane intent:", err)
	}
	if !result.DryRun || result.Intent.SourceLane != SelectedRouteID || result.Intent.BudgetScope != Phase3GoalID || result.Intent.BudgetFamily != "Maple" {
		t.Fatalf("dry-run lost the derived intent identity: %+v", result)
	}
	if result.Intent.CreatedAt.IsZero() {
		t.Fatal("dry-run intent has no creation time")
	}
	invalid := unwindIntentCommandRequest(SelectedRouteID)
	invalid.Reason = "operator_preference"
	if _, err = RunUnwindIntentCommit(ctx, "", invalid, false); err == nil || !strings.Contains(err.Error(), "invalid_unwind_intent") {
		t.Fatalf("dry-run accepted an off-list reason: %v", err)
	}
	unfunded := unwindIntentCommandRequest(SelectedRouteID)
	unfunded.CostBoundRaw = 0
	if _, err = RunUnwindIntentCommit(ctx, "", unfunded, false); err == nil || !strings.Contains(err.Error(), "invalid_unwind_intent") {
		t.Fatalf("dry-run accepted a zero cost bound: %v", err)
	}
	unknown := unwindIntentCommandRequest("AUTO/AUTO/USDC")
	if _, err = RunUnwindIntentCommit(ctx, "", unknown, false); err == nil || !strings.Contains(err.Error(), "invalid_unwind_intent") {
		t.Fatalf("dry-run accepted an unregistered lane: %v", err)
	}
}

// Execute without a database configuration is a config hold, after shape
// validation, and never reaches a lease.
func TestUnwindIntentCommitExecuteRequiresDatabaseConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := RunUnwindIntentCommit(ctx, "", unwindIntentCommandRequest(SelectedRouteID), true)
	assertBudgetHold(t, err, "invalid_unwind_intent_config")
}

// The candidate AUTO source is refused by the installed embedded manifest and
// admitted only through a reviewed binding — the same closure the commit and
// every other entry point share.
func TestUnwindIntentCandidateAuthorityMatchesEmbeddedManifest(t *testing.T) {
	embedded := requireEmbeddedInstalledBinding(t)
	reviewed := autoInitializerFixtureManifest(t)
	intent := unwindIntentFromRequest(unwindIntentCommandRequest(autoAUTOPYUSD.Lane), time.Now().UTC())
	// Both binding states: the explicit absent fixture (the shipped pre-install
	// state) refuses the candidate unwind source, and the embedded manifest's
	// installed binding admits it — the same closure the reviewed initializer
	// fixture admits through.
	if autoAbsentBindingManifest(t).validateUnwindIntent(intent) == nil {
		t.Fatal("absent binding admitted the candidate unwind source")
	}
	if err := embedded.validateUnwindIntent(intent); err != nil {
		t.Fatal("installed manifest refused the candidate unwind source:", err)
	}
	if err := reviewed.validateUnwindIntent(intent); err != nil {
		t.Fatal("reviewed manifest refused the candidate unwind source:", err)
	}
}

// The command core with the reviewed candidate manifest injected commits the
// actual AUTO request end to end — dry-run without a database, then execute
// under its own lease against real candidate DB state on the disposable
// database — while the public wrapper keeps the embedded authority.
func TestUnwindIntentCommitCoreCommitsCandidateUnderReviewedManifest(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	reviewed := autoInitializerFixtureManifest(t)
	// Dry-run through the core needs no database and admits the candidate.
	result, err := runUnwindIntentCommitOnManifest(ctx, reviewed, "", "selector-auto-cmd", unwindIntentCommandRequest(autoAUTOPYUSD.Lane), false)
	if err != nil || !result.DryRun || result.Intent.BudgetFamily != "AUTO" {
		t.Fatalf("reviewed dry-run refused the candidate intent: %+v %v", result, err)
	}
	key := fmt.Sprintf("selector-auto-unwind-cmd-%d", time.Now().UnixNano())
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
	committed, err := runUnwindIntentCommitOnManifest(ctx, reviewed, url, key, unwindIntentCommandRequest(autoAUTOPYUSD.Lane), true)
	if err != nil {
		t.Fatal("candidate execute through the reviewed core failed:", err)
	}
	if committed.DryRun {
		t.Fatal("candidate execute reported itself as a dry-run")
	}
	stored, err := db.LoadUnwindIntentOnManifest(ctx, reviewed, key)
	if err != nil || stored == nil || !sameUnwindIntent(*stored, committed.Intent) {
		t.Fatalf("candidate execute lost the committed intent: %+v %v", stored, err)
	}
	// The command's short lease is released, so the worker can take the route.
	if _, err = db.AcquireRouteLease(ctx, key, "unwind-candidate-after", time.Minute); err != nil {
		t.Fatal("command lease was not released:", err)
	}
	// Both binding states on the public path: before the release the embedded
	// wrapper refused the candidate lane outright (invalid_unwind_intent);
	// with the installed binding shipped, the lane authority admits it and the
	// next guard in the same chain — the funded exit reservation — is what
	// closes this second attempt. The reviewed commit above ran on the random
	// candidate key and never touched the public production row, so this
	// attempt is grounded in explicitly seeded state instead of whatever
	// earlier tests left behind: release the asserted random-key lease, clear
	// only the production route's operations and manual latch, and upsert its
	// route state as generation 1 with an empty non-pilot budget — no funded
	// AUTO exit, no lease, no committed unwind.
	if _, err = db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `DELETE FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	publicPrior := emptyTestBudget()
	publicState, err := json.Marshal(map[string]any{"generation": 1, "phase3": publicPrior})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version)
		VALUES($1,$2,1)
		ON CONFLICT (route_key) DO UPDATE SET state = EXCLUDED.state, state_version = 1, lease_owner = NULL, lease_expires_at = NULL`, productionRouteKey, publicState); err != nil {
		t.Fatal(err)
	}
	_, publicErr := RunUnwindIntentCommit(ctx, url, unwindIntentCommandRequest(autoAUTOPYUSD.Lane), true)
	assertBudgetHold(t, publicErr, "unwind_requires_existing_exit_reservation")
}

// Execute acquires its own short route lease, commits through the shared
// guarded store call, and releases the lease — proven against one real route
// row on the disposable database with an installed lane and a funded exit
// reservation.
func TestUnwindIntentCommitExecuteCommitsUnderOwnLease(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	resetManualRecoveryProductionRoute(t, ctx, db, "unwind-command-seed")
	if _, err := db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
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
	activated.Families["Maple"] = FamilyBudget{SpentMicros: 7_000_000, ExitMicros: 3_000_000}
	state, err := json.Marshal(map[string]any{"generation": 2, "phase3": activated, "pilotBudgetActivation": pilotBudgetActivation{authority, mustJSON(t, prior), flat}})
	if err != nil {
		t.Fatal(err)
	}
	// The reset above re-seeds a minimal production-route row; replace it with
	// the activated budget this execute run commits against.
	if _, err = db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, productionRouteKey, state); err != nil {
		t.Fatal(err)
	}
	result, err := RunUnwindIntentCommit(ctx, url, unwindIntentCommandRequest(SelectedRouteID), true)
	if err != nil {
		t.Fatal("execute refused a funded installed-lane unwind:", err)
	}
	if result.DryRun {
		t.Fatal("execute reported itself as a dry-run")
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	committed, err := db.LoadUnwindIntentOnManifest(ctx, manifest, productionRouteKey)
	if err != nil || committed == nil || !sameUnwindIntent(*committed, result.Intent) {
		t.Fatalf("execute lost the committed intent: %+v %v", committed, err)
	}
	// A repeat execute builds a fresh CreatedAt, so it is a different intent
	// and the durable commit refuses to replace it.
	if _, err = RunUnwindIntentCommit(ctx, url, unwindIntentCommandRequest(SelectedRouteID), true); err == nil {
		t.Fatal("repeat execute replaced the committed unwind")
	} else {
		assertBudgetHold(t, err, "another_unwind_is_committed")
	}
	// The command's short lease is released, so the worker can take the route.
	if _, err = db.AcquireRouteLease(ctx, productionRouteKey, "unwind-command-after", time.Minute); err != nil {
		t.Fatal("command lease was not released:", err)
	}
}
