package backyardrwa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSelectorUnwindDurabilityAndBudgetContinuity(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("selector-intent-%d", time.Now().UnixNano())
	budget := emptyTestBudget()
	budget.Families["Maple"] = FamilyBudget{SpentMicros: 7_000_000, ExitMicros: 3_000_000}
	state, _ := json.Marshal(map[string]any{"generation": 1, "phase3": budget})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,$2)`, key, state); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "selector-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	intent := UnwindIntent{SourceLane: SelectedRouteID, Reason: "economic_rotation", ObservationID: "source", MaxCollateralRaw: 100, MaxDebtRaw: 50, CostBoundRaw: 2_000_000, BudgetScope: Phase3GoalID, BudgetFamily: "Maple", EvidenceID: sha256Bytes([]byte("exit")), CreatedAt: time.Now().UTC()}
	if err := db.CommitUnwindIntent(ctx, key, intent); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitUnwindIntent(ctx, key, intent); err != nil {
		t.Fatal("idempotent retry", err)
	}
	if _, err := db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.AcquireRouteLease(ctx, key, "selector-b", time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.LoadUnwindIntent(ctx, key)
	if err != nil || got == nil || *got != intent {
		t.Fatalf("restart lost intent: %+v %v", got, err)
	}
	if err = db.CommitUnwindIntent(ctx, key, intent); err == nil {
		t.Fatal("stale lease wrote intent")
	}
	s := base()
	s.RouteLane = SelectedRouteID
	if _, err = restarted.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,expected_effects) VALUES($1,$2,'signed','OPEN_ROUTE_STEP','{}')`, key+"-signed", key); err != nil {
		t.Fatal(err)
	}
	if err = restarted.CompleteUnwindIntent(ctx, key, intent, s); err == nil {
		t.Fatal("cleared intent during unresolved signed transaction")
	}
	if _, err = restarted.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciled' WHERE operation_id=$1`, key+"-signed"); err != nil {
		t.Fatal(err)
	}
	if err = restarted.CompleteUnwindIntent(ctx, key, intent, s); err != nil {
		t.Fatal(err)
	}
	paused, err := restarted.SelectorEntryPaused(ctx, key)
	if err != nil || !paused {
		t.Fatal("completed exit allowed stale entry")
	}
	var storedBudget []byte
	var version, generation int64
	if err = restarted.pool.QueryRow(ctx, `SELECT state->'phase3',state_version,(state->>'generation')::bigint FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&storedBudget, &version, &generation); err != nil {
		t.Fatal(err)
	}
	var after Phase3Budget
	if err = json.Unmarshal(storedBudget, &after); err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(budget)
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) || version != generation {
		t.Fatal("intent changed budget or generation")
	}
}

func TestIncidentResolutionMigrationPreservesFailureAndBindsDisposition(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	migration, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "crates", "loyal-yield-store", "migrations", "0078_backyard_rwa_incident_resolution.sql"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}') ON CONFLICT DO NOTHING`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	id := "fe45a0369bf950da3ea311a4c493377cf9720a92c359c0bfbe739a3d9f699cbe"
	if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,transaction_signature,confirmed_slot,recovery_reason,expected_effects,signed_wire) VALUES($1,$2,'manual_recovery','VOLTR_RESTORE_IDLE','46UBvSw1zjtZyDVUVaissm9SEXsKFKnYCQYKd23njb1NS1Ktkzsup5ic9XA55FxyTCpkoYuuM8hhn4MioGU2X7Wz',444157954,'exact_effect_reconciliation_failed','{"original":true}',decode('010203','hex'))`, id, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(migration)); err != nil {
		t.Fatal("migration retry", err)
	}
	var status, reason string
	var wire, meta []byte
	var original bool
	if err = tx.QueryRow(ctx, `SELECT status,recovery_reason,signed_wire,(expected_effects->>'original')::boolean,expected_effects->'manualResolution' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status, &reason, &wire, &original, &meta); err != nil {
		t.Fatal(err)
	}
	if status != "manual_recovery" || reason != "exact_effect_reconciliation_failed" || !bytes.Equal(wire, []byte{1, 2, 3}) || !original {
		t.Fatal("historical failure or wire rewritten")
	}
	// Use an isolated route for the guard; copied resolution must not clear it.
	route := "selector-resolution-copy"
	if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}');`, route); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,transaction_signature,confirmed_slot,expected_effects) VALUES('different-incident',$1,'manual_recovery','VOLTR_RESTORE_IDLE','different',1,jsonb_build_object('manualResolution',$2::jsonb))`, route, meta); err != nil {
		t.Fatal(err)
	}
	var blocked bool
	if err = tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, route).Scan(&blocked); err != nil || !blocked {
		t.Fatal("copied disposition bypassed recovery", err)
	}
}
func TestDeploymentLimitsCannotResetOrIncreaseBudget(t *testing.T) {
	b := emptyTestBudget()
	b.Families["OnRe"] = FamilyBudget{SpentMicros: 4_000_000, ExitMicros: 2_000_000}
	limits := DeploymentLimits{500_000, 10_000_000, 30_000_000}
	if err := b.ConstrainLimits(limits); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(b)
	var restarted Phase3Budget
	if err := json.Unmarshal(encoded, &restarted); err != nil {
		t.Fatal(err)
	}
	if restarted.Families["OnRe"] != b.Families["OnRe"] || restarted.GoalID != Phase3GoalID {
		t.Fatal("limits reset budget")
	}
	if err := restarted.ConstrainLimits(legacyDeploymentLimits()); err == nil {
		t.Fatal("limits increased")
	}
	before, _ := json.Marshal(restarted)
	if err := restarted.ConstrainLimits(DeploymentLimits{100_000, 5_000_000, 20_000_000}); err == nil {
		t.Fatal("limits consumed reserved exit")
	}
	after, _ := json.Marshal(restarted)
	if !bytes.Equal(before, after) {
		t.Fatal("rejection mutated budget")
	}
	reservation := BudgetReservation{OperationID: "too-large", Family: "OnRe", IntentSHA256: sha256Bytes([]byte("intent")), UpperMicros: 600_000, ExitAfterMicros: 2_000_000}
	if err := restarted.Admit(reservation); err == nil {
		t.Fatal("new deployment cap ignored")
	}
}

func TestShadowSkipsLockedExecutionRow(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 10*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("selector-busy-%d", time.Now().UnixNano())
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "shadow-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT 1 FROM loyal_yield.multiply_route_states WHERE route_key=$1 FOR UPDATE`, key); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := db.RecordSelectorShadow(ctx, key, tickObservation(base()), nil); err == nil {
		t.Fatal("shadow wrote locked execution state")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("shadow did not respect bounded SQL lock wait")
	}
}

func TestShadowPreservesEvidenceDuringUnresolvedNAV(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 10*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("selector-nav-%d", time.Now().UnixNano())
	in := selectorFixture()
	result := SelectOpportunity(in, SelectorState{})
	state, _ := json.Marshal(map[string]any{"generation": 1, "selector": map[string]any{"result": result}})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,$2)`, key, state); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "shadow-nav", time.Minute); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := db.pool.QueryRow(ctx, `SELECT state->'selector' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// The operation begins after the observer read: the locked write must also
	// check the journal, not only trust the earlier snapshot's Nonterminal.
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,expected_effects) VALUES($1,$2,'submitted','REPORT_NAV','{}')`, key+"-report", key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordSelectorShadow(ctx, key, tickObservation(in.Snapshot), in.Markets); err != nil {
		t.Fatal(err)
	}
	var after []byte
	if err := db.pool.QueryRow(ctx, `SELECT state->'selector' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unresolved report reset or advanced economic evidence")
	}
}
