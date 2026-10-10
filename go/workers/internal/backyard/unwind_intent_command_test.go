package backyard

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
		EvidenceID:       sha256Bytes([]byte("exit-evidence")),
	}
}

func confirmedUnwindIntentCommandRequest(lane string) UnwindIntentCommitRequest {
	req := unwindIntentCommandRequest(lane)
	req.Confirmation = &DebtClearConfirmation{RequestID: sha256Bytes([]byte("operator-confirmation:" + lane)), ConfirmedBy: "test-operator", ConfirmationRecord: sha256Bytes([]byte("explicit-debt-clear-record")), AcknowledgeUnavailableReborrow: true, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}
	return req
}

// The dry-run default validates the exact intent shape and the installed
// embedded manifest's lane authority without opening any database connection.
func TestUnwindIntentCommitDryRunValidatesWithoutDatabase(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := RunUnwindIntentCommit(ctx, "", unwindIntentCommandRequest(SelectedRouteID), false)
	if err != nil {
		t.Fatal("embedded dry-run refused an installed-lane intent:", err)
	}
	if !result.DryRun || result.Intent.SourceLane != SelectedRouteID || result.Intent.MaxDebtRaw != 50 || result.Intent.MaxCollateralRaw != 100 {
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
	unknown := unwindIntentCommandRequest("AUTO/AUTO/USDC")
	if _, err = RunUnwindIntentCommit(ctx, "", unknown, false); err == nil || !strings.Contains(err.Error(), "invalid_unwind_intent") {
		t.Fatalf("dry-run accepted an unregistered lane: %v", err)
	}
}

// Execute without a database configuration is a config hold, after shape
// validation, and never reaches a lease.
func TestUnwindIntentCommitExecuteRequiresDatabaseConfig(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := RunUnwindIntentCommit(ctx, "", confirmedUnwindIntentCommandRequest(SelectedRouteID), true)
	assertBudgetHold(t, err, "invalid_unwind_intent_config")
}

// The unwind source authority is the registry: every held lane, the
// exit-only Ethena lane included, may be unwound; a lane outside it may not.
func TestUnwindIntentSourceAuthorityIsTheRegistry(t *testing.T) {
	t.Parallel()
	for _, lane := range earnLaneIDs(true) {
		intent := unwindIntentFromRequest(unwindIntentCommandRequest(lane), time.Now().UTC())
		if err := intent.validate(); err != nil {
			t.Fatal("registry lane cannot be unwound:", lane, err)
		}
	}
	intent := unwindIntentFromRequest(unwindIntentCommandRequest(RouteID), time.Now().UTC())
	if intent.validate() == nil {
		t.Fatal("a lane outside the registry validated as an unwind source")
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
	reviewed := embeddedTestManifest(t)
	// Dry-run through the core needs no database and admits the candidate.
	result, err := runUnwindIntentCommitOnManifest(ctx, reviewed, "", "selector-auto-cmd", unwindIntentCommandRequest(autoAUTOPYUSD.Lane), false)
	if err != nil || !result.DryRun || result.Intent.SourceLane != autoAUTOPYUSD.Lane {
		t.Fatalf("reviewed dry-run refused the candidate intent: %+v %v", result, err)
	}
	key := fmt.Sprintf("selector-auto-unwind-cmd-%d", time.Now().UnixNano())
	state, err := json.Marshal(map[string]any{"generation": 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, state); err != nil {
		t.Fatal(err)
	}
	committed, err := runUnwindIntentCommitOnManifest(ctx, reviewed, url, key, confirmedUnwindIntentCommandRequest(autoAUTOPYUSD.Lane), true)
	if err != nil {
		t.Fatal("candidate execute through the reviewed core failed:", err)
	}
	if committed.DryRun {
		t.Fatal("candidate execute reported itself as a dry-run")
	}
	stored, err := db.LoadUnwindIntent(ctx, key)
	if err != nil || stored == nil || !sameUnwindIntent(*stored, committed.Intent) {
		t.Fatalf("candidate execute lost the committed intent: %+v %v", stored, err)
	}
	// The command's short lease is released, so the worker can take the route.
	if _, err = db.AcquireRouteLease(ctx, key, "unwind-candidate-after", time.Minute); err != nil {
		t.Fatal("command lease was not released:", err)
	}
	// The public wrapper resolves the same lane through the installed
	// embedded binding and commits on a clean production row.
	if _, err = db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `DELETE FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version)
		VALUES($1,'{"generation":1}',1)
		ON CONFLICT (route_key) DO UPDATE SET state = EXCLUDED.state, state_version = 1, lease_owner = NULL, lease_expires_at = NULL`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	public, err := RunUnwindIntentCommit(ctx, url, confirmedUnwindIntentCommandRequest(autoAUTOPYUSD.Lane), true)
	if err != nil || public.Intent.SourceLane != autoAUTOPYUSD.Lane {
		t.Fatalf("installed binding refused the AUTO unwind: %+v %v", public, err)
	}
	stored, err = db.LoadUnwindIntent(ctx, productionRouteKey)
	if err != nil || stored == nil || !sameUnwindIntent(*stored, public.Intent) {
		t.Fatalf("public execute lost the committed intent: %+v %v", stored, err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{selectorUnwind}','null',true), lease_owner=NULL, lease_expires_at=NULL WHERE route_key=$1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
}

// Execute acquires its own short route lease, commits through the shared
// guarded store call, and releases the lease — proven against one real route
// row on the disposable database with an installed lane.
func TestUnwindIntentCommitExecuteCommitsUnderOwnLease(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	resetManualRecoveryProductionRoute(t, ctx, db, "unwind-command-seed")
	if _, err := db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(map[string]any{"generation": 2})
	if err != nil {
		t.Fatal(err)
	}
	// The reset above re-seeds a minimal production-route row; replace it with
	// the generation this execute run commits against.
	if _, err = db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, productionRouteKey, state); err != nil {
		t.Fatal(err)
	}
	request := confirmedUnwindIntentCommandRequest(SelectedRouteID)
	result, err := RunUnwindIntentCommit(ctx, url, request, true)
	if err != nil {
		t.Fatal("execute refused an installed-lane unwind:", err)
	}
	if result.DryRun {
		t.Fatal("execute reported itself as a dry-run")
	}
	committed, err := db.LoadUnwindIntent(ctx, productionRouteKey)
	if err != nil || committed == nil || !sameUnwindIntent(*committed, result.Intent) {
		t.Fatalf("execute lost the committed intent: %+v %v", committed, err)
	}
	// The same explicit confirmation is idempotent, not another debt-clear flow.
	repeated, err := RunUnwindIntentCommit(ctx, url, request, true)
	if err != nil || !sameUnwindIntent(repeated.Intent, result.Intent) {
		t.Fatal("repeat confirmation changed its flow", err)
	}
	changed := request
	confirmation := *request.Confirmation
	confirmation.ConfirmationRecord = sha256Bytes([]byte("different-record"))
	changed.Confirmation = &confirmation
	_, err = RunUnwindIntentCommit(ctx, url, changed, true)
	assertBudgetHold(t, err, "debt_clear_confirmation_reused")
	// The command's short lease is released, so the worker can take the route.
	if _, err = db.AcquireRouteLease(ctx, productionRouteKey, "unwind-command-after", time.Minute); err != nil {
		t.Fatal("command lease was not released:", err)
	}
}
