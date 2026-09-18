package backyardrwa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A terminal hold is an audit-only journal record with no operation epoch, so
// its durable identity must track the manifest/policy binding it was decided
// under. Executable identities keep the epoch namespace and are never bound
// here, and the epoch-free base identity of both hold actions stays stable.
func TestHoldAuditIdentityBindsManifestAndCatalog(t *testing.T) {
	manifestA, manifestB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	catalogA, catalogB := strings.Repeat("1", 64), strings.Repeat("2", 64)
	hold := Decision{Action: Hold, IdempotencyKey: "obs:HOLD:0:no_eligible_action"}

	epochless, err := durableDecisionIdempotencyKey("route", "", hold)
	if err != nil {
		t.Fatal(err)
	}
	if epochless != "route:obs:HOLD:0:no_eligible_action" {
		t.Fatalf("hold base identity changed shape: %s", epochless)
	}
	boundA := holdBoundIdempotencyKey(epochless, manifestA, catalogA)
	if boundA != holdBoundIdempotencyKey(epochless, manifestA, catalogA) {
		t.Fatal("same-binding hold retries are not deterministic")
	}
	if !strings.Contains(boundA, ":hold-binding:"+manifestA+":"+catalogA) {
		t.Fatalf("hold identity is not manifest+catalog bound: %s", boundA)
	}
	if holdBoundIdempotencyKey(epochless, manifestB, catalogA) == boundA {
		t.Fatal("a manifest rollover reused the previous binding's hold identity")
	}
	if holdBoundIdempotencyKey(epochless, manifestA, catalogB) == boundA {
		t.Fatal("a policy catalog rollover reused the previous binding's hold identity")
	}
	if boundA == epochless {
		t.Fatal("a bound hold identity can rematch a historical unbound row")
	}

	executable := Decision{Action: VoltrAllocateToSquads, IdempotencyKey: "obs:VOLTR_ALLOCATE_TO_SQUADS:1000:eligible_voltr_idle"}
	executableKey, err := durableDecisionIdempotencyKey("route", "reconciled-op", executable)
	if err != nil {
		t.Fatal(err)
	}
	if executableKey != "route:reconciled-op:"+executable.IdempotencyKey {
		t.Fatalf("executable identity namespace changed: %s", executableKey)
	}
	if executableKey == boundA || strings.Contains(executableKey, ":hold-binding:") {
		t.Fatal("the hold binding suffix leaked into the executable namespace")
	}

	manual := Decision{Action: HoldManualRecovery, IdempotencyKey: "obs:custody_mismatch"}
	manualA, err := durableDecisionIdempotencyKey("route", "reconciled-a", manual)
	if err != nil {
		t.Fatal(err)
	}
	manualB, err := durableDecisionIdempotencyKey("route", "reconciled-b", manual)
	if err != nil {
		t.Fatal(err)
	}
	if manualA != manualB || manualA != "route:"+manual.IdempotencyKey {
		t.Fatalf("latched manual recovery identity must stay epoch- and binding-stable: %s vs %s", manualA, manualB)
	}
	if manualA == boundA {
		t.Fatal("manual recovery identity collided with a bound hold identity")
	}
}

func holdReleaseObservation(obsID string, slot int64) Observation {
	return Observation{ObservedAt: time.Unix(slot, 0).UTC(), Snapshot: Snapshot{
		ObservationID: obsID, Slot: slot, RouteKind: RouteKind, Fresh: true,
	}}
}

func holdReleaseEvidenceEnvelope(t *testing.T, manifest, catalog, obsID string, slot int64, reason string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"schema":                "loyal-backyard-rwa-operation-evidence/v1",
		"journalStrategyConfig": bridgeStrategy,
		"decision": map[string]any{
			"amountRaw":           0,
			"reason":              reason,
			"observationId":       obsID,
			"observationSlot":     slot,
			"manifestSha256":      manifest,
			"policyCatalogSha256": catalog,
			"strategyKey":         RouteID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func holdReleaseSeedLegacyHold(t *testing.T, ctx context.Context, db *Database, routeKey string, decision Decision, manifest, catalog string, slot int64) string {
	t.Helper()
	legacyKey := routeKey + ":" + decision.IdempotencyKey
	digest := sha256.Sum256([]byte(legacyKey))
	if _, err := db.pool.Exec(ctx, OperationInsert,
		hex.EncodeToString(digest[:]), routeKey, 1, string(decision.Action), string(Held),
		legacyKey, decision.StrategyKey,
		string(holdReleaseEvidenceEnvelope(t, manifest, catalog, "obs1", slot, decision.Reason)), nil); err != nil {
		t.Fatal(err)
	}
	return legacyKey
}

func holdReleaseCountRows(t *testing.T, ctx context.Context, db *Database, routeKey string) int {
	t.Helper()
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func holdReleaseRowIdentity(t *testing.T, ctx context.Context, db *Database, operationID string) (string, string) {
	t.Helper()
	var key string
	var effects []byte
	if err := db.pool.QueryRow(ctx, `SELECT idempotency_key, expected_effects::text FROM loyal_yield.multiply_operations WHERE operation_id = $1`, operationID).Scan(&key, &effects); err != nil {
		t.Fatalf("journal row %s disappeared: %v", operationID, err)
	}
	return key, string(effects)
}

func holdReleaseRequireError(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("expected error containing %q, got %v", fragment, err)
	}
}

// The production crash loop: an identical observation+hold reason recurring
// after a bridge manifest rollover reused the historical HOLD's identity and
// the evidence comparison rejected the changed binding on every tick. The
// legacy row must survive untouched while each new binding gets its own
// deterministic identity, and executable money identities must keep being
// rejected on binding conflicts.
func TestHoldReleaseIdentityAcrossManifestRolloverAgainstDatabase(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 60*time.Second)
	defer cancel()
	defer db.Close()
	routeKey := "rwa-multiply:hold-release-identity-a"
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state)
		 VALUES($1,'{"generation":1,"cycle":1}')
		 ON CONFLICT (route_key) DO UPDATE SET state = EXCLUDED.state, lease_owner = NULL, lease_expires_at = NULL`, routeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, routeKey, "hold-release-identity", time.Minute); err != nil {
		t.Fatal(err)
	}

	manifestA, manifestB, manifestC := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	catalogA, catalogB := strings.Repeat("1", 64), strings.Repeat("2", 64)
	hold := Decision{Action: Hold, Reason: "no_eligible_action", AmountRaw: 0,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d:%s", "obs1", Hold, 0, "no_eligible_action"), StrategyKey: RouteID}

	// Historical row decided under the pre-rollover binding, stored with the
	// legacy unbound key shape exactly as production wrote it.
	legacyKey := holdReleaseSeedLegacyHold(t, ctx, db, routeKey, hold, manifestA, catalogA, 500)
	if count := holdReleaseCountRows(t, ctx, db, routeKey); count != 1 {
		t.Fatalf("legacy seed row count = %d", count)
	}

	// The next tick rolls the manifest and decides the identical hold again.
	rolled, err := db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 501), hold, manifestB, catalogA)
	if err != nil {
		t.Fatalf("hold after manifest rollover collided with history: %v", err)
	}
	if count := holdReleaseCountRows(t, ctx, db, routeKey); count != 2 {
		t.Fatalf("rollover did not start a fresh hold identity: rows=%d", count)
	}
	rolledKey, _ := holdReleaseRowIdentity(t, ctx, db, rolled.OperationID)
	if rolledKey != legacyKey+":hold-binding:"+manifestB+":"+catalogA {
		t.Fatalf("rollover hold identity is not bound to the new manifest: %s", rolledKey)
	}
	legacyKeyCheck, legacyEffects := holdReleaseRowIdentity(t, ctx, db, hex.EncodeToString(func() []byte { d := sha256.Sum256([]byte(legacyKey)); return d[:] }()))
	if legacyKeyCheck != legacyKey || !strings.Contains(legacyEffects, manifestA) {
		t.Fatalf("historical HOLD row was rewritten: key=%s effects=%s", legacyKeyCheck, legacyEffects)
	}

	// Same-binding retries dedupe deterministically, at any later slot.
	retry, err := db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 502), hold, manifestB, catalogA)
	if err != nil || retry.OperationID != rolled.OperationID {
		t.Fatalf("same-binding hold retry did not dedupe: %+v %v", retry, err)
	}
	if count := holdReleaseCountRows(t, ctx, db, routeKey); count != 2 {
		t.Fatalf("same-binding retry inserted a duplicate: rows=%d", count)
	}

	// A further manifest rollover, then a policy catalog rollover, each start
	// their own identity and dedupe within the binding.
	next, err := db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 503), hold, manifestC, catalogA)
	if err != nil || next.OperationID == rolled.OperationID {
		t.Fatalf("second manifest rollover did not advance hold identity: %+v %v", next, err)
	}
	if retry, err = db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 504), hold, manifestC, catalogA); err != nil || retry.OperationID != next.OperationID {
		t.Fatalf("second binding retry did not dedupe: %+v %v", retry, err)
	}
	catalogRolled, err := db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 505), hold, manifestC, catalogB)
	if err != nil || catalogRolled.OperationID == next.OperationID {
		t.Fatalf("policy catalog rollover did not advance hold identity: %+v %v", catalogRolled, err)
	}
	if _, err = db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 506), hold, manifestC, catalogB); err != nil {
		t.Fatalf("catalog-binding retry did not dedupe: %v", err)
	}
	if count := holdReleaseCountRows(t, ctx, db, routeKey); count != 4 {
		t.Fatalf("binding rollovers produced unexpected journal shape: rows=%d", count)
	}

	// The evidence comparison still guards same-binding drift.
	drift := hold
	drift.Reason = "different_reason"
	holdReleaseRequireError(t,
		func() error {
			_, err := db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 507), drift, manifestB, catalogA)
			return err
		}(),
		"idempotency identity has different decision evidence")
	if count := holdReleaseCountRows(t, ctx, db, routeKey); count != 4 {
		t.Fatalf("rejected drift wrote a journal row: rows=%d", count)
	}

	// Executable money identities are untouched: epoch namespaced, retry
	// deduping within an epoch, and a binding conflict still rejected.
	executable := Decision{Action: VoltrAllocateToSquads, Reason: "eligible_voltr_idle", AmountRaw: 1000,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d:%s", "obs1", VoltrAllocateToSquads, 1000, "eligible_voltr_idle"), StrategyKey: RouteID}
	first, err := db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 510), executable, manifestB, catalogA)
	if err != nil || first.Status != Decided {
		t.Fatalf("executable decision failed: %+v %v", first, err)
	}
	if again, err := db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 511), executable, manifestB, catalogA); err != nil || again.OperationID != first.OperationID {
		t.Fatalf("executable retry within an epoch did not dedupe: %+v %v", again, err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status = 'reconciled', confirmed_slot = 510 WHERE operation_id = $1`, first.OperationID); err != nil {
		t.Fatal(err)
	}
	advanced, err := db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 512), executable, manifestB, catalogA)
	if err != nil || advanced.OperationID == first.OperationID {
		t.Fatalf("a genuinely later epoch did not advance executable identity: %+v %v", advanced, err)
	}
	if again, err := db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 513), executable, manifestB, catalogA); err != nil || again.OperationID != advanced.OperationID {
		t.Fatalf("executable retry under the new epoch did not dedupe: %+v %v", again, err)
	}
	holdReleaseRequireError(t,
		func() error {
			_, err := db.RecordDecision(ctx, routeKey, holdReleaseObservation("obs1", 514), executable, manifestC, catalogA)
			return err
		}(),
		"idempotency identity has different decision evidence")
	if count := holdReleaseCountRows(t, ctx, db, routeKey); count != 6 {
		t.Fatalf("executable conflict or retry mutated the journal: rows=%d", count)
	}
}

// The latch assessment: HOLD_MANUAL_RECOVERY is bound like plain HOLD, because
// the latched re-record stores the live manifest binding on every tick and an
// unbound identity reproduced the same rollover crash loop. The stop itself is
// the physical latch and its generation fence: a rollover re-anchors the
// journal row per binding while the latch keeps its original reason, observed
// identity, and generation, and retries under one binding keep deduping.
func TestLatchedManualRecoveryIdentityIgnoresBindingRolloverAgainstDatabase(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 60*time.Second)
	defer cancel()
	defer db.Close()
	routeKey := "rwa-multiply:hold-release-identity-b"
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1`, routeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state)
		 VALUES($1,'{"generation":1,"cycle":1}')
		 ON CONFLICT (route_key) DO UPDATE SET state = EXCLUDED.state, lease_owner = NULL, lease_expires_at = NULL`, routeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, routeKey, "hold-release-identity", time.Minute); err != nil {
		t.Fatal(err)
	}

	manifestA, manifestB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	catalogA, catalogB := strings.Repeat("1", 64), strings.Repeat("2", 64)
	manual := Decision{Action: HoldManualRecovery, Reason: "custody_mismatch", AmountRaw: 0,
		IdempotencyKey: fmt.Sprintf("%s:%s", "obs2", "custody_mismatch"), StrategyKey: RouteID}

	originalRecord, err := db.RecordManualRecovery(ctx, routeKey, holdReleaseObservation("obs2", 600), manual, manifestA, catalogA)
	if err != nil {
		t.Fatal(err)
	}
	// Full manifest+catalog rollover while the route stays latched: the
	// re-record re-anchors the journal row instead of failing the tick.
	afterRollover, err := db.RecordManualRecovery(ctx, routeKey, holdReleaseObservation("obs2", 601), manual, manifestB, catalogB)
	if err != nil {
		t.Fatalf("latched re-record collided with the pre-rollover stop: %v", err)
	}
	if afterRollover.OperationID == originalRecord.OperationID {
		t.Fatal("the rollover re-record reused the previous binding's identity")
	}
	if key, _ := holdReleaseRowIdentity(t, ctx, db, afterRollover.OperationID); key != routeKey+":"+manual.IdempotencyKey+":hold-binding:"+manifestB+":"+catalogB {
		t.Fatalf("re-anchored stop identity is not bound to the new binding: %s", key)
	}
	// Retries under the new binding dedupe to the re-anchored row.
	if retry, err := db.RecordManualRecovery(ctx, routeKey, holdReleaseObservation("obs2", 602), manual, manifestB, catalogB); err != nil || retry.OperationID != afterRollover.OperationID {
		t.Fatalf("same-binding latched retry did not dedupe: %+v %v", retry, err)
	}
	if count := holdReleaseCountRows(t, ctx, db, routeKey); count != 2 {
		t.Fatalf("unexpected journal shape across the rollover: rows=%d", count)
	}
	// The physical latch survives the rebinding unchanged: original reason,
	// original observed identity, generation still 0, still latched.
	latch, latched, err := db.ManualRecoveryLatch(ctx, routeKey)
	if err != nil || !latched {
		t.Fatalf("the stop did not survive the binding rollover: %+v %v", latch, err)
	}
	if latch.Reason != "custody_mismatch" || latch.ObservationID != "obs2" || latch.ObservationSlot != 600 || latch.Generation != 0 {
		t.Fatalf("the physical latch was mutated by the rebinding: %+v", latch)
	}
	// The generation-gated re-record still passes the unchanged fence.
	if gated, err := db.RecordManualRecoveryAtGeneration(ctx, routeKey, holdReleaseObservation("obs2", 603), manual, manifestB, catalogB, 0); err != nil || gated.OperationID != afterRollover.OperationID {
		t.Fatalf("generation-gated re-record failed across a binding rollover: %+v %v", gated, err)
	}
	if count := holdReleaseCountRows(t, ctx, db, routeKey); count != 2 {
		t.Fatalf("generation-gated re-record duplicated the stop: rows=%d", count)
	}
}
