package backyardrwa

// Bounded evidence for doc49 (locked-admission round-trip reduction). The
// first two guard existence reads inside recordSelectorEvaluationWithLanes
// (nonterminal operations, unresolved capital recovery) now ride ONE SELECT
// whose columns wrap the unchanged guard predicates, so the locked admission
// spends one round trip on that pair instead of two inside the same 1s context
// and 25ms lock window. The manual latch read stays its own statement LAST:
// LatchManualRecovery writes the physical latch without the route row lock, so
// its snapshot must not move earlier.
//
// These tests drive the REAL production admission function against the
// disposable database: success shape (entry, receipt, version, no operations),
// every guard rejection in precedence order, no mutation on rejection, stale
// expected version — plus a transport measurement with injected per-read
// latency, so the saved sequential statement exchange is observable locally.
// The measurement is clearly
// labeled synthetic: live production statement RTT is root's 75–167ms
// measurement, so the expected live saving is ONE statement round trip. The
// route-lease check inside the admission (d.currentLease) is an in-memory
// RLock read (store.go), not a database round trip, and is outside this
// measurement either way. Unconditional cleanup: every phase runs on the
// disposable database with the harness context bound at or under 120s and
// every transaction either commits through the production function or is
// explicitly rolled back — including a deferred rollback taken immediately
// after each Begin so a t.Fatal mid-transaction cannot leak it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedAdmissionRouteState reseeds the version-2 activated pilot route with no
// guard rows and NO lease (the route row is replaced, so any previous lease is
// gone); the caller's pool then acquires the lease it needs — the route lease
// is exclusive, so exactly one pool may hold it per phase.
func seedAdmissionRouteState(t *testing.T, ctx context.Context, db *Database, key, owner string) {
	t.Helper()
	resetManualRecoveryProductionRoute(t, ctx, db, owner)
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
	activated.Families["AUTO"] = FamilyBudget{SpentMicros: 7_000_000}
	state, err := json.Marshal(map[string]any{"generation": 2, "phase3": activated, "pilotBudgetActivation": pilotBudgetActivation{authority, mustJSON(t, prior), flat}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, state); err != nil {
		t.Fatal(err)
	}
}

// TestSelectorAdmissionGuardBehavior drives the production admission function
// through every acceptance shape: success preserves entry/receipt/version,
// each guard rejects with the unchanged code, precedence keeps the first
// failing guard, rejection mutates nothing, and stale fencing still rejects.
func TestSelectorAdmissionGuardBehavior(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 100*time.Second)
	defer cancel()
	defer db.Close()
	manifest := autoInitializerFixtureManifest(t)
	input := canaryFixtureInput(t)
	key := productionRouteKey

	insertOperation := func(t *testing.T, operationID, status, action, recoveryReason string, effects string) {
		t.Helper()
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,recovery_reason,expected_effects,transaction_signature,confirmed_slot)
			VALUES($1,$2,$3,$4,$5,$6::jsonb,CASE WHEN $7 THEN 'sig' END,0)`, operationID, key, status, action, recoveryReason, effects, effects != "{}"); err != nil {
			t.Fatal(err)
		}
	}
	admit := func(t *testing.T, version int64) (SelectorResult, error) {
		t.Helper()
		return db.recordSelectorEvaluationWithLanes(ctx, key, &manifest, input, input.Snapshot.Slot, version)
	}
	expectHold := func(t *testing.T, want string, mutate func(t *testing.T)) {
		t.Helper()
		seedAdmissionRouteState(t, ctx, db, key, "selector-admission-test")
		if _, err := db.AcquireRouteLease(ctx, key, "selector-admission-test", time.Minute); err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(t)
		}
		_, _, versionBefore := canaryRouteStateAssertions(t, ctx, db, key)
		var opsBefore int
		if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key=$1`, key).Scan(&opsBefore); err != nil {
			t.Fatal(err)
		}
		_, err := admit(t, 2)
		var hold *BudgetHold
		if !errors.As(err, &hold) || hold.Reason != want {
			t.Fatalf("expected hold %s, got %v", want, err)
		}
		entry, receipts, version := canaryRouteStateAssertions(t, ctx, db, key)
		if version != versionBefore || len(receipts) != 0 || entry != nil {
			t.Fatalf("rejection mutated durable state: version %d->%d receipts %d entry %+v", versionBefore, version, len(receipts), entry)
		}
		var opsAfter int
		if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key=$1`, key).Scan(&opsAfter); err != nil {
			t.Fatal(err)
		}
		if opsAfter != opsBefore {
			t.Fatalf("rejection changed operation rows: %d -> %d", opsBefore, opsAfter)
		}
	}

	// Success: one exact canary admission commits the entry, receipt and one
	// version bump under the caller's lease — which the locked function
	// correctly leaves with its caller — and creates no operation row.
	leaseHeld := func(t *testing.T) {
		t.Helper()
		var owner string
		if err := db.pool.QueryRow(ctx, `SELECT lease_owner FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&owner); err != nil || owner != "selector-admission-test" {
			t.Fatalf("caller lease disturbed: owner %q err %v", owner, err)
		}
	}
	seedAdmissionRouteState(t, ctx, db, key, "selector-admission-test")
	if _, err := db.AcquireRouteLease(ctx, key, "selector-admission-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	result, err := admit(t, 2)
	if err != nil || result.Action != "CANARY_ENTER" || result.EquityRaw != input.canaryRequest.EquityRaw {
		t.Fatalf("admission did not succeed: %v action %s", err, result.Action)
	}
	entry, receipts, version := canaryRouteStateAssertions(t, ctx, db, key)
	if entry == nil || entry.EquityRaw != input.canaryRequest.EquityRaw || len(receipts) != 1 || receipts[input.canaryRequest.ID].Request.EquityRaw != input.canaryRequest.EquityRaw || version != 3 {
		t.Fatalf("success shape wrong: entry %+v receipts %d version %d", entry, len(receipts), version)
	}
	assertNoCanaryOperations(t, ctx, db, key)
	leaseHeld(t)

	// Nonterminal operation: rejected, nothing mutated.
	expectHold(t, "selector_finish_current_work_first", func(t *testing.T) {
		insertOperation(t, "admission-nonterminal", "decided", "VOLTR_ALLOCATE_TO_SQUADS", "", "{}")
	})
	// Unresolved capital recovery: rejected, nothing mutated.
	expectHold(t, "selector_resolve_capital_recovery_first", func(t *testing.T) {
		insertOperation(t, "admission-recovery", "manual_recovery", "VOLTR_ALLOCATE_TO_SQUADS", "manual_test", "{}")
	})
	// A superseded, evidence-bound recovery disposition does NOT hold: the
	// merged column preserves the COALESCE resolution semantics, and admission
	// proceeds to the full success shape.
	seedAdmissionRouteState(t, ctx, db, key, "selector-admission-test")
	if _, err := db.AcquireRouteLease(ctx, key, "selector-admission-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	insertOperation(t, "admission-superseded", "manual_recovery", "VOLTR_ALLOCATE_TO_SQUADS", "manual_test",
		fmt.Sprintf(`{"manualResolution":{"schema":"backyard-manual-resolution/v1","disposition":"superseded_by_strategy_reset","operationId":"admission-superseded","routeKey":%q,"action":"VOLTR_ALLOCATE_TO_SQUADS","signature":"sig","confirmedSlot":"0","evidenceSha256":"%064d","evidencePath":"/tmp/disposition"}}`, key, 1))
	if _, err := admit(t, 2); err != nil {
		t.Fatalf("superseded recovery wrongly held the admission: %v", err)
	}
	if _, receipts, version := canaryRouteStateAssertions(t, ctx, db, key); len(receipts) != 1 || version != 3 {
		t.Fatalf("superseded-recovery admission shape wrong: receipts %d version %d", len(receipts), version)
	}
	// Physical manual latch: rejected.
	expectHold(t, "selector_manual_recovery_active", func(t *testing.T) {
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.backyard_manual_recovery_latches(route_key,reason,observation_id,observation_slot,generation) VALUES($1,'manual_hold_test','obs-test',1,1)`, key); err != nil {
			t.Fatal(err)
		}
	})
	// Derived manual latch (journal hold, no physical row): rejected.
	expectHold(t, "selector_manual_recovery_active", func(t *testing.T) {
		insertOperation(t, "admission-derived-latch", "manual_recovery", "HOLD_MANUAL_RECOVERY", "manual_hold_derived", "{}")
	})
	// Precedence: when every guard is pending, the FIRST guard in the chain
	// (nonterminal operation) names the hold.
	expectHold(t, "selector_finish_current_work_first", func(t *testing.T) {
		insertOperation(t, "admission-nonterminal", "decided", "VOLTR_ALLOCATE_TO_SQUADS", "", "{}")
		insertOperation(t, "admission-recovery", "manual_recovery", "VOLTR_ALLOCATE_TO_SQUADS", "manual_test", "{}")
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.backyard_manual_recovery_latches(route_key,reason,observation_id,observation_slot,generation) VALUES($1,'manual_hold_test','obs-test',1,1)`, key); err != nil {
			t.Fatal(err)
		}
	})
	// Stale expected VERSION — not a fencing-token case: the caller holds the
	// real current lease, so the RouteStateForUpdate fencing columns match and
	// the rejection is the version-mismatch hold. The seeded row is at
	// versionBefore and the admission is called with versionBefore+1, a
	// genuinely different expected version; it rejects before any guard can
	// matter and nothing mutates.
	seedAdmissionRouteState(t, ctx, db, key, "selector-admission-test")
	if _, err := db.AcquireRouteLease(ctx, key, "selector-admission-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, _, versionBefore := canaryRouteStateAssertions(t, ctx, db, key)
	if _, err := admit(t, versionBefore+1); err == nil {
		t.Fatal("stale expected version was accepted")
	} else {
		var hold *BudgetHold
		if !errors.As(err, &hold) || hold.Reason != "selector_state_changed_during_quote" {
			t.Fatalf("stale version hold wrong: %v", err)
		}
	}
	if _, receipts, version := canaryRouteStateAssertions(t, ctx, db, key); version != versionBefore || len(receipts) != 0 {
		t.Fatalf("stale rejection mutated durable state: version %d->%d receipts %d", versionBefore, version, len(receipts))
	}
}

// latencyTransportConn injects a fixed delay before every transport read and
// counts reads. Read counts are transport observations, not round trips; the
// injected-delay TIME is what models a live statement RTT, which each
// sequential statement exchange pays once.
type latencyTransportConn struct {
	net.Conn
	delay time.Duration
	reads *atomic.Int64
}

func (c *latencyTransportConn) Read(b []byte) (int, error) {
	time.Sleep(c.delay)
	c.reads.Add(1)
	return c.Conn.Read(b)
}

// openLatencyAdmissionPool builds a single-connection pool over the SAME
// disposable URL whose dialer injects the fixed per-read delay. It refuses
// anything but the disposable socket database, exactly like the other
// harnesses, and never contacts any other endpoint.
func openLatencyAdmissionPool(t *testing.T, ctx context.Context, url string, perRead time.Duration) (*Database, *atomic.Int64) {
	t.Helper()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	reads := &atomic.Int64{}
	config.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &latencyTransportConn{Conn: conn, delay: perRead, reads: reads}, nil
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	return &Database{pool: pool}, reads
}

// TestSelectorAdmissionGuardRoundTripMeasurement measures the guard section's
// transport cost before and after the reduction under injected per-read
// latency (SYNTHETIC; live statement RTT is root's 75–167ms), then proves the
// production admission still succeeds end to end over the delayed transport.
func TestSelectorAdmissionGuardRoundTripMeasurement(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 100*time.Second)
	defer cancel()
	defer db.Close()
	manifest := autoInitializerFixtureManifest(t)
	input := canaryFixtureInput(t)
	key := productionRouteKey
	const perRead = 15 * time.Millisecond
	delayed, reads := openLatencyAdmissionPool(t, ctx, url, perRead)
	defer delayed.Close()

	// BEFORE: the previous transport — the previous guard section of THREE
	// statements (nonterminal, capital recovery, manual latch) as sequential
	// per-statement round trips inside a locked transaction. Only the guard
	// section is timed; the surrounding BEGIN/lock/rollback stages are common
	// to both shapes.
	seedAdmissionRouteState(t, ctx, db, key, "selector-admission-latency")
	if _, err := delayed.AcquireRouteLease(ctx, key, "selector-admission-latency", time.Minute); err != nil {
		t.Fatal(err)
	}
	tx, err := delayed.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var opsPending, recoveryPending, manualPending bool
	beforeReads := reads.Load()
	before := time.Now()
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`))`, key).Scan(&opsPending); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, key).Scan(&recoveryPending); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key=$1 AND cleared_at IS NULL) OR EXISTS (`+manualRecoveryDerivedLatchSQL+`)`, key).Scan(&manualPending); err != nil {
		t.Fatal(err)
	}
	oldElapsed, oldGuardReads := time.Since(before), reads.Load()-beforeReads
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if opsPending || recoveryPending || manualPending {
		t.Fatal("seeded baseline unexpectedly pending")
	}

	// AFTER: the production admission function — the first two guards are now
	// one merged SELECT and the manual latch read follows as its own statement,
	// inside the same locked transaction — measured end to end over the same
	// injected latency, with the same success shape asserted.
	seedAdmissionRouteState(t, ctx, db, key, "selector-admission-latency")
	if _, err := delayed.AcquireRouteLease(ctx, key, "selector-admission-latency", time.Minute); err != nil {
		t.Fatal(err)
	}
	afterReads := reads.Load()
	after := time.Now()
	result, err := delayed.recordSelectorEvaluationWithLanes(ctx, key, &manifest, input, input.Snapshot.Slot, 2)
	newElapsed, newTotalReads := time.Since(after), reads.Load()-afterReads
	if err != nil || result.Action != "CANARY_ENTER" {
		t.Fatalf("admission over delayed transport failed: %v action %s", err, result.Action)
	}
	if _, receipts, version := canaryRouteStateAssertions(t, ctx, db, key); len(receipts) != 1 || version != 3 {
		t.Fatalf("delayed-transport success shape wrong: receipts %d version %d", len(receipts), version)
	}
	if oldGuardReads < 3 {
		t.Fatalf("baseline transport unexpectedly used %d reads for three statements", oldGuardReads)
	}

	// AFTER guard section in isolation: the production shape — the merged
	// two-guard SELECT followed by the SEPARATE manual latch statement — inside
	// the same locked-transaction shape. The statements mirror the production
	// composition (unchanged guard predicates) for transport measurement only;
	// every behavior assertion above runs the real production function.
	seedAdmissionRouteState(t, ctx, db, key, "selector-admission-latency")
	if _, err := delayed.AcquireRouteLease(ctx, key, "selector-admission-latency", time.Minute); err != nil {
		t.Fatal(err)
	}
	mergedTx, err := delayed.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mergedTx.Rollback(ctx) }()
	if _, err = mergedTx.Exec(ctx, `SET LOCAL lock_timeout = '25ms'`); err != nil {
		t.Fatal(err)
	}
	var leaseOwner string
	var fencing int64
	if err = db.pool.QueryRow(ctx, `SELECT lease_owner, fencing_token FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&leaseOwner, &fencing); err != nil {
		t.Fatal(err)
	}
	var versionProbe int64
	var stateProbe []byte
	if err = mergedTx.QueryRow(ctx, RouteStateForUpdate, key, leaseOwner, fencing).Scan(&versionProbe, &stateProbe); err != nil {
		t.Fatal(err)
	}
	mergedReadsBefore := reads.Load()
	mergedStart := time.Now()
	var pairOpsPending, pairRecoveryPending bool
	if err = mergedTx.QueryRow(ctx, `SELECT `+
		`EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`)), `+
		`(`+UnresolvedCapitalRecoverySQL+`)`, key).Scan(&pairOpsPending, &pairRecoveryPending); err != nil {
		t.Fatal(err)
	}
	var latchPending bool
	if err = mergedTx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key=$1 AND cleared_at IS NULL) OR EXISTS (`+manualRecoveryDerivedLatchSQL+`)`, key).Scan(&latchPending); err != nil {
		t.Fatal(err)
	}
	mergedElapsed, mergedGuardReads := time.Since(mergedStart), reads.Load()-mergedReadsBefore
	if err = mergedTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if mergedGuardReads >= oldGuardReads {
		t.Fatalf("new guard section did not reduce transport reads: %d vs %d", mergedGuardReads, oldGuardReads)
	}
	if pairOpsPending || pairRecoveryPending || latchPending {
		t.Fatal("seeded baseline unexpectedly pending in new shape")
	}

	// LOCAL: the same production admission with no injected latency, so the
	// local execution floor is distinguishable from the synthetic transport.
	seedAdmissionRouteState(t, ctx, db, key, "selector-admission-latency")
	if _, err := db.AcquireRouteLease(ctx, key, "selector-admission-latency", time.Minute); err != nil {
		t.Fatal(err)
	}
	local := time.Now()
	if _, err := db.recordSelectorEvaluationWithLanes(ctx, key, &manifest, input, input.Snapshot.Slot, 2); err != nil {
		t.Fatalf("local admission failed: %v", err)
	}
	localElapsed := time.Since(local)

	t.Logf("SYNTHETIC per-read %v: guard section OLD 3 statements = %d reads, %v; guard section NEW (merged pair + separate latch) 2 statements = %d reads, %v; production admission NEW = %d total reads, %v total; LOCAL no-injection admission = %v", perRead, oldGuardReads, oldElapsed, mergedGuardReads, mergedElapsed, newTotalReads, newElapsed, localElapsed)
	t.Logf("expected live saving: ONE statement round trip ~= root's measured 75-167ms of the 1s admission context; the route-lease read is in-memory (store.go currentLease RLock), not a database round trip")
}
