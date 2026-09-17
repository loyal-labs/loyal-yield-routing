package backyardrwa

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The manual recovery latch is the durability contract behind every
// HOLD_MANUAL_RECOVERY decision: it is persisted when the decision is recorded,
// re-read at the top of every tick, and lifted only by the operator command.
var errExecutionResumed = errors.New("execution resumed past the cleared latch")

func TestManualRecoveryLatchLifecycleAgainstDatabase(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureManualRecoveryTestSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	// The worker is fixed to the production route, so the test route row is that
	// key; the upsert resets any earlier run's lease.
	routeKey := productionRouteKey
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1`, routeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state)
		 VALUES($1,'{"generation":1,"cycle":1}')
		 ON CONFLICT (route_key) DO UPDATE SET state = '{"generation":1,"cycle":1}', lease_owner = NULL, lease_expires_at = NULL`, routeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, routeKey, "latch-facts-writer", time.Minute); err != nil {
		t.Fatal(err)
	}

	manifest := readyWorkerManifest(t)
	fault := true
	var observeCalls, buildCalls, preparedCalls int
	var recorded []Decision
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe: func(context.Context) (Observation, error) {
			observeCalls++
			mutate := func(batch []ConfirmedAccount) {
				if fault {
					replaceAccount(batch, bridgeStrategyReceipt, ConfirmedAccount{})
				}
			}
			state := productionObserveState{
				routeKey: routeKey,
				journal:  &stubProductionJournal{journal: reconciledJournal()},
				batch:    productionConfirmedBatchForManifest(t, manifest, 77, mutate),
				identity: pinnedIdentityObservation,
			}
			return state.observe(ctx)
		},
		loadLatch: db.ManualRecoveryLatch,
		recordManualRecovery: func(ctx context.Context, key string, observation Observation, decision Decision, manifestHash, policyHash string) (DecisionRecord, error) {
			recorded = append(recorded, decision)
			return db.RecordManualRecovery(ctx, key, observation, decision, manifestHash, policyHash)
		},
		prepareBridge: func(context.Context, RouteManifest, Decision, Observation) (Observation, BridgeExecutionEvidence, error) {
			preparedCalls++
			return Observation{}, BridgeExecutionEvidence{}, errExecutionResumed
		},
		prepareKamino: func(context.Context, RouteManifest, Decision) (Observation, KaminoExecutionEvidence, error) {
			preparedCalls++
			return Observation{}, KaminoExecutionEvidence{}, errExecutionResumed
		},
		prepareJupiter: func(context.Context, RouteManifest, Decision) (Observation, JupiterExecutionEvidence, error) {
			preparedCalls++
			return Observation{}, JupiterExecutionEvidence{}, errExecutionResumed
		},
		buildBridge: func(context.Context, string, BridgeExecutionEvidence) error {
			buildCalls++
			return nil
		},
	}}

	// (a) The production observe path classifies the broken receipt, Decide
	// persists the manual recovery hold, and the route is latched.
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	latch, latched, err := db.ManualRecoveryLatch(ctx, routeKey)
	if err != nil || !latched {
		t.Fatalf("the manual recovery hold was not latched: %+v %v", latch, err)
	}
	if latch.Reason != "strategy_receipt_integrity" || latch.ObservationSlot != 77 || latch.ObservationID == "" || latch.Generation != 0 {
		t.Fatalf("unexpected latch identity: %+v", latch)
	}

	// (d) Restart simulation: a new worker state object over the same database
	// still sees the stop.
	reopened, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, latched, err := reopened.ManualRecoveryLatch(ctx, routeKey); err != nil || !latched {
		t.Fatalf("a restarted worker lost the latch: %v", err)
	}

	// (b) A healthy batch behind the latch is never observed and never executes.
	fault = false
	healthy := len(recorded)
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if observeCalls != 1 {
		t.Fatalf("a latched tick observed the chain (%d reads)", observeCalls)
	}
	if preparedCalls != 0 || buildCalls != 0 {
		t.Fatalf("a latched tick executed (%d prepared, %d built)", preparedCalls, buildCalls)
	}
	if len(recorded) != healthy+1 || recorded[len(recorded)-1].Action != HoldManualRecovery ||
		recorded[len(recorded)-1].Reason != "latched:strategy_receipt_integrity" {
		t.Fatalf("the latched tick did not re-record the hold: %+v", recorded)
	}

	// (c) clear-hold is the only way out: the journal row carries the operator
	// reason and the next healthy tick decides from live data again.
	if _, err := ClearManualRecoveryHold(ctx, url, routeKey, "operator re-verified the strategy receipt"); err != nil {
		t.Fatal(err)
	}
	if _, latched, err := db.ManualRecoveryLatch(ctx, routeKey); err != nil || latched {
		t.Fatalf("the latch survived its explicit clear: %v", err)
	}
	var action, status, reason string
	var effects []byte
	if err := db.pool.QueryRow(ctx, `SELECT action, status::text, recovery_reason, expected_effects
		 FROM loyal_yield.multiply_operations WHERE action = 'HOLD_CLEARED' AND route_key = $1`, routeKey).
		Scan(&action, &status, &reason, &effects); err != nil {
		t.Fatalf("the clear was not journaled: %v", err)
	}
	if status != "manual_recovery" || reason != "operator re-verified the strategy receipt" || !strings.Contains(string(effects), "strategy_receipt_integrity") || !strings.Contains(string(effects), "latchGeneration") {
		t.Fatalf("unexpected HOLD_CLEARED journal row: %s %s %s", status, reason, effects)
	}
	tickErr := worker.Tick(ctx)
	if tickErr != nil && !errors.Is(tickErr, errExecutionResumed) {
		t.Fatalf("the cleared tick neither executed nor decided: %v", tickErr)
	}
	if observeCalls != 2 {
		t.Fatalf("the cleared tick did not observe the chain (%d reads)", observeCalls)
	}
	if preparedCalls != 1 {
		t.Fatalf("a healthy tick after an explicit clear did not proceed to preparation (%d calls)", preparedCalls)
	}
	if len(recorded) != healthy+1 {
		t.Fatalf("a healthy tick after an explicit clear recorded an unexpected hold: %+v", recorded)
	}

	// An empty reason never clears anything, and an unknown route fails loudly.
	if _, err := ClearManualRecoveryHold(ctx, url, routeKey, ""); err == nil {
		t.Fatal("an empty reason cleared a manual recovery hold")
	}
	if _, err := ClearManualRecoveryHold(ctx, url, routeKey+"-absent", "no latch here"); err == nil {
		t.Fatal("cleared a latch that did not exist")
	}
}

func manualRecoveryTestObservation(id string, slot int64) Observation {
	return Observation{ObservedAt: time.Unix(slot, 0).UTC(), Snapshot: Snapshot{
		ObservationID: id, Slot: slot, RouteKind: RouteKind, Fresh: true, MonitorsArmed: true,
	}}
}

func manualRecoveryTestDecision(id, reason string) Decision {
	return Decision{Action: HoldManualRecovery, Reason: reason, AmountRaw: 0,
		IdempotencyKey: fmt.Sprintf("%s:%s", id, reason), StrategyKey: RouteID}
}

func manualRecoveryHealthyRefreshObservation(id string, slot int64) Observation {
	snapshot := base()
	snapshot.ObservationID = id
	snapshot.Slot = slot
	snapshot.MonitorsArmed = true
	snapshot.ProgramIdentityKnown = false
	snapshot.TicketLastConsumedSequenceRaw = 4
	snapshot.VoltrIdleRaw = 7
	snapshot.VoltrTotalValueRaw = 49
	snapshot.PriorReportedNAVRaw = 42
	snapshot.StrategyNAVRaw = 42
	return Observation{Snapshot: snapshot, ObservedAt: time.Unix(slot, 0).UTC()}
}

func manualRecoveryKaminoRefreshObservation(id string, slot int64) Observation {
	observation := manualRecoveryHealthyRefreshObservation(id, slot)
	observation.Snapshot.PositionDebtRaw = 3
	return observation
}

func openManualRecoveryTestDatabase(t *testing.T, timeout time.Duration) (context.Context, context.CancelFunc, *Database, string) {
	t.Helper()
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := ensureManualRecoveryTestSchema(ctx, db); err != nil {
		db.Close()
		cancel()
		t.Fatal(err)
	}
	return ctx, cancel, db, url
}

func resetManualRecoveryProductionRoute(t *testing.T, ctx context.Context, db *Database, owner string) {
	t.Helper()
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state)
		VALUES($1,'{"generation":1,"cycle":1}')
		ON CONFLICT (route_key) DO UPDATE SET state = EXCLUDED.state, lease_owner = NULL, lease_expires_at = NULL`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, productionRouteKey, owner, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func ensureManualRecoveryTestSchema(ctx context.Context, db *Database) error {
	_, err := db.pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS loyal_yield;
	CREATE TABLE IF NOT EXISTS loyal_yield.multiply_route_states (
	 route_key text PRIMARY KEY,state_version bigint NOT NULL DEFAULT 1,state jsonb NOT NULL,
	 lease_owner text,lease_expires_at timestamptz,fencing_token bigint NOT NULL DEFAULT 0,
	 updated_at timestamptz NOT NULL DEFAULT now(),
	 CHECK ((state->>'generation')::bigint=state_version),
	 CHECK ((lease_owner IS NULL)=(lease_expires_at IS NULL)));
	CREATE TABLE IF NOT EXISTS loyal_yield.multiply_operations (
	 operation_id text PRIMARY KEY,route_key text NOT NULL REFERENCES loyal_yield.multiply_route_states,
	 cycle bigint NOT NULL DEFAULT 1,engine_version text NOT NULL DEFAULT 'backyard_rwa_v1',
	 action text,status text NOT NULL,idempotency_key text,strategy_key text,
	 expected_effects jsonb NOT NULL DEFAULT '{}'::jsonb,recovery_reason text,
	 signed_wire bytea,broadcast_intent_at timestamptz,confirmed_slot bigint,
	 transaction_signature text,created_at timestamptz NOT NULL DEFAULT now(),
	 updated_at timestamptz NOT NULL DEFAULT now());
	ALTER TABLE loyal_yield.multiply_route_states ADD COLUMN IF NOT EXISTS state jsonb NOT NULL DEFAULT '{"generation":1,"cycle":1}'::jsonb;
	ALTER TABLE loyal_yield.multiply_route_states ADD COLUMN IF NOT EXISTS state_version bigint NOT NULL DEFAULT 1;
	ALTER TABLE loyal_yield.multiply_route_states ADD COLUMN IF NOT EXISTS lease_owner text;
	ALTER TABLE loyal_yield.multiply_route_states ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz;
	ALTER TABLE loyal_yield.multiply_route_states ADD COLUMN IF NOT EXISTS fencing_token bigint NOT NULL DEFAULT 0;
	ALTER TABLE loyal_yield.multiply_route_states ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS cycle bigint NOT NULL DEFAULT 1;
	ALTER TABLE loyal_yield.multiply_operations ALTER COLUMN cycle SET DEFAULT 1;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS engine_version text NOT NULL DEFAULT 'backyard_rwa_v1';
	ALTER TABLE loyal_yield.multiply_operations ALTER COLUMN engine_version SET DEFAULT 'backyard_rwa_v1';
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS action text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS idempotency_key text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS strategy_key text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS recovery_reason text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS confirmed_slot bigint;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS transaction_signature text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();
	CREATE TABLE IF NOT EXISTS loyal_yield.backyard_manual_recovery_latches (
	 route_key text PRIMARY KEY REFERENCES loyal_yield.multiply_route_states(route_key),
	 reason text NOT NULL,observation_id text NOT NULL,observation_slot bigint NOT NULL,
	 generation bigint NOT NULL DEFAULT 0,
	 latched_at timestamptz NOT NULL DEFAULT clock_timestamp(),cleared_at timestamptz,
	 cleared_reason text,CHECK ((cleared_at IS NULL)=(cleared_reason IS NULL)));`)
	if _, err := db.pool.Exec(ctx, `ALTER TABLE loyal_yield.backyard_manual_recovery_latches ADD COLUMN IF NOT EXISTS generation bigint NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return err
}

func TestManualRecoveryHoldAndLatchAreAtomic(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureManualRecoveryTestSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	routeKey := fmt.Sprintf("manual-recovery-atomic-%d", time.Now().UnixNano())
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1,"cycle":1}')`, routeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, routeKey, "manual-recovery-atomic", time.Minute); err != nil {
		t.Fatal(err)
	}
	observation := manualRecoveryTestObservation("atomic-observation", 901)
	decision := manualRecoveryTestDecision(observation.Snapshot.ObservationID, "injected_after_hold_insert")
	injected := errors.New("injected failure after hold insert")
	if _, err := db.recordManualRecovery(ctx, routeKey, observation, decision, strings.Repeat("a", 64), strings.Repeat("b", 64), func() error {
		return injected
	}); !errors.Is(err, injected) {
		t.Fatalf("post-insert failure was not returned: %v", err)
	}
	var operationCount, latchCount int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1`, routeKey).Scan(&latchCount); err != nil {
		t.Fatal(err)
	}
	if operationCount != 0 || latchCount != 0 {
		t.Fatalf("atomic manual recovery transaction left partial state: %d operations, %d latches", operationCount, latchCount)
	}
}

func TestManualRecoveryDerivedLatchBlocksWithoutLatchRow(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureManualRecoveryTestSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1,"cycle":1}')
		ON CONFLICT (route_key) DO UPDATE SET state = EXCLUDED.state, lease_owner = NULL, lease_expires_at = NULL`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations
		(operation_id,route_key,cycle,engine_version,action,status,idempotency_key,strategy_key,expected_effects,recovery_reason,confirmed_slot,updated_at)
		VALUES('derived-hold',$1,1,'backyard_rwa_v1','HOLD_MANUAL_RECOVERY','manual_recovery','derived-hold-key',$2,
		'{"decision":{"reason":"derived_safety","observationId":"derived-observation","observationSlot":902}}'::jsonb,
		'derived_safety',902,clock_timestamp())`, productionRouteKey, RouteID); err != nil {
		t.Fatal(err)
	}
	manifest := readyWorkerManifest(t)
	var observeCalls, buildCalls int
	var recorded Decision
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadLatch: db.ManualRecoveryLatch,
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			t.Fatal("derived latch allowed the tick to load execution state")
			return nil, nil
		},
		observe: func(context.Context) (Observation, error) {
			observeCalls++
			t.Fatal("derived latch allowed a chain observation")
			return Observation{}, nil
		},
		recordManualRecovery: func(_ context.Context, _ string, _ Observation, decision Decision, _, _ string) (DecisionRecord, error) {
			recorded = decision
			return DecisionRecord{OperationID: "derived-latched", Cycle: 1, Status: ManualRecovery}, nil
		},
		buildBridge: func(context.Context, string, BridgeExecutionEvidence) error {
			buildCalls++
			return nil
		},
	}}
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if observeCalls != 0 || buildCalls != 0 {
		t.Fatalf("derived latch did not stop the tick: %d observes, %d builds", observeCalls, buildCalls)
	}
	if recorded.Action != HoldManualRecovery || recorded.Reason != "latched:derived_safety" {
		t.Fatalf("derived latch was not re-journaled: %+v", recorded)
	}
	var activeLatchCount int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1 AND cleared_at IS NULL`, productionRouteKey).Scan(&activeLatchCount); err != nil {
		t.Fatal(err)
	}
	if activeLatchCount != 0 {
		t.Fatalf("derived-latch test unexpectedly created a physical latch row: %d", activeLatchCount)
	}
	if _, err := ClearManualRecoveryHold(ctx, url, productionRouteKey, "operator cleared the derived legacy hold"); err != nil {
		t.Fatalf("explicit clear could not release a derived latch: %v", err)
	}
	if _, latched, err := db.ManualRecoveryLatch(ctx, productionRouteKey); err != nil || latched {
		t.Fatalf("derived hold remained after explicit clear: %v", err)
	}
}

func TestManualRecoveryDerivedLatchIgnoresLatchedRerecords(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 60*time.Second)
	defer cancel()
	defer db.Close()
	resetManualRecoveryProductionRoute(t, ctx, db, "manual-recovery-derived-ordering")

	hash := strings.Repeat("a", 64)
	policyHash := strings.Repeat("b", 64)
	initial := manualRecoveryTestObservation("derived-ordering-initial", 903)
	initialDecision := manualRecoveryTestDecision(initial.Snapshot.ObservationID, "initial_safety")
	if _, err := db.RecordManualRecovery(ctx, productionRouteKey, initial, initialDecision, hash, policyHash); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearManualRecoveryHold(ctx, url, productionRouteKey, "operator cleared the initial hold"); err != nil {
		t.Fatal(err)
	}

	// A genuine hold after the clear re-arms generation 1. A subsequent normal
	// latched:* journal re-record must not hide this genuine hold if the
	// physical row disappears.
	genuine := manualRecoveryTestObservation("derived-ordering-genuine", 904)
	genuineDecision := manualRecoveryTestDecision(genuine.Snapshot.ObservationID, "new_safety")
	if _, err := db.RecordManualRecovery(ctx, productionRouteKey, genuine, genuineDecision, hash, policyHash); err != nil {
		t.Fatal(err)
	}
	latch, latched, err := db.ManualRecoveryLatch(ctx, productionRouteKey)
	if err != nil || !latched || latch.Generation != 1 {
		t.Fatalf("the genuine post-clear hold did not re-arm generation 1: %+v %v", latch, err)
	}
	latchedDecision := manualRecoveryTestDecision("derived-ordering-rerecord", "latched:"+genuineDecision.Reason)
	if _, err := db.RecordManualRecoveryAtGeneration(ctx, productionRouteKey, manualRecoveryTestObservation("derived-ordering-rerecord", 905), latchedDecision, hash, policyHash, latch.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}

	fallback, latched, err := db.ManualRecoveryLatch(ctx, productionRouteKey)
	if err != nil || !latched {
		t.Fatalf("the latest latched:* row hid the genuine hold: %+v %v", fallback, err)
	}
	if fallback.Reason != genuineDecision.Reason || fallback.ObservationID != genuine.Snapshot.ObservationID || fallback.Generation != 1 {
		t.Fatalf("fallback selected the wrong hold: %+v", fallback)
	}

	manifest := readyWorkerManifest(t)
	var observeCalls, buildCalls int
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadLatch: db.ManualRecoveryLatch,
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			t.Fatal("genuine fallback latch allowed the tick to load execution state")
			return nil, nil
		},
		observe: func(context.Context) (Observation, error) {
			observeCalls++
			t.Fatal("genuine fallback latch allowed a chain observation")
			return Observation{}, nil
		},
		buildBridge: func(context.Context, string, BridgeExecutionEvidence) error {
			buildCalls++
			return nil
		},
		recordManualRecoveryAtGeneration: db.RecordManualRecoveryAtGeneration,
	}}
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if observeCalls != 0 || buildCalls != 0 {
		t.Fatalf("genuine fallback latch did not stop the tick: %d observes, %d builds", observeCalls, buildCalls)
	}

	// Clearing after the genuine hold advances the fence to generation 2. The
	// higher-generation clear must win even though the newest genuine hold is
	// older in the journal ordering.
	if _, err := ClearManualRecoveryHold(ctx, url, productionRouteKey, "operator cleared the genuine hold"); err != nil {
		t.Fatal(err)
	}
	if fallback, latched, err := db.ManualRecoveryLatch(ctx, productionRouteKey); err != nil || latched {
		t.Fatalf("higher-generation clear did not release the fallback latch: %+v %v", fallback, err)
	}
}

func TestManualRecoveryConstructionRefreshPersistsHoldBeforeDispatch(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureManualRecoveryTestSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1,"cycle":1}')
		ON CONFLICT (route_key) DO UPDATE SET state = EXCLUDED.state, lease_owner = NULL, lease_expires_at = NULL`, productionRouteKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, productionRouteKey, "manual-recovery-refresh", time.Minute); err != nil {
		t.Fatal(err)
	}
	manifest := readyWorkerManifest(t)
	initial := manualRecoveryTestObservation("refresh-before", 903)
	initial.Snapshot.MonitorsArmed = false
	initial.Snapshot.VoltrIdleRaw = 7
	refreshed := manualRecoveryTestObservation("refresh-hold", 904)
	refreshed.Snapshot.MonitorsArmed = true
	refreshed.Snapshot.StrategyReceiptIntegrityFault = true
	var observeCalls, prepareCalls, admitCalls, buildCalls int
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadLatch:       db.ManualRecoveryLatch,
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe: func(context.Context) (Observation, error) {
			observeCalls++
			return initial, nil
		},
		recordManualRecovery: db.RecordManualRecovery,
		prepareBridge: func(context.Context, RouteManifest, Decision, Observation) (Observation, BridgeExecutionEvidence, error) {
			prepareCalls++
			return refreshed, BridgeExecutionEvidence{}, nil
		},
		admitBridge: func(context.Context, string, Observation, Decision, BridgeExecutionEvidence) error {
			admitCalls++
			return nil
		},
		buildBridge: func(context.Context, string, BridgeExecutionEvidence) error {
			buildCalls++
			return nil
		},
	}}
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	latch, latched, err := db.ManualRecoveryLatch(ctx, productionRouteKey)
	if err != nil || !latched {
		t.Fatalf("construction refresh hold was not latched: %+v %v", latch, err)
	}
	if latch.Reason != "strategy_receipt_integrity" || latch.ObservationID != refreshed.Snapshot.ObservationID || latch.ObservationSlot != refreshed.Snapshot.Slot {
		t.Fatalf("unexpected construction-refresh latch: %+v", latch)
	}
	if observeCalls != 1 || prepareCalls != 1 || admitCalls != 0 || buildCalls != 0 {
		t.Fatalf("construction refresh hold reached execution: %d observes, %d prepares, %d admissions, %d builds", observeCalls, prepareCalls, admitCalls, buildCalls)
	}
}

func TestManualRecoveryClearBetweenLatchReadAndRerecordDoesNotRearm(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 60*time.Second)
	defer cancel()
	defer db.Close()
	resetManualRecoveryProductionRoute(t, ctx, db, "manual-recovery-generation-race")

	initial := manualRecoveryTestObservation("generation-race-hold", 905)
	decision := manualRecoveryTestDecision(initial.Snapshot.ObservationID, "operator_review_required")
	if _, err := db.RecordManualRecovery(ctx, productionRouteKey, initial, decision, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	manifest := readyWorkerManifest(t)
	var rerecordHookCalls, observeCalls, buildCalls int
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadLatch: db.ManualRecoveryLatch,
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			t.Fatal("a latched tick loaded execution state")
			return nil, nil
		},
		beforeRecordLatchedHold: func(ctx context.Context, latch ManualRecoveryLatch) error {
			rerecordHookCalls++
			if latch.Generation != 0 {
				t.Fatalf("the initial latch generation drifted before the race: %+v", latch)
			}
			_, err := ClearManualRecoveryHold(ctx, url, productionRouteKey, "operator won the latch race")
			return err
		},
		recordManualRecoveryAtGeneration: db.RecordManualRecoveryAtGeneration,
	}}
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if rerecordHookCalls != 1 {
		t.Fatalf("the clear race hook ran %d times", rerecordHookCalls)
	}
	var generation int64
	var clearedReason string
	if err := db.pool.QueryRow(ctx, `SELECT generation, cleared_reason
		FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1`, productionRouteKey).
		Scan(&generation, &clearedReason); err != nil {
		t.Fatal(err)
	}
	if generation != 1 || clearedReason != "operator won the latch race" {
		t.Fatalf("the operator clear did not win the generation race: generation=%d reason=%q", generation, clearedReason)
	}
	if _, latched, err := db.ManualRecoveryLatch(ctx, productionRouteKey); err != nil || latched {
		t.Fatalf("the stale tick re-armed the cleared latch: %v", err)
	}
	var staleHoldCount int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.multiply_operations
		WHERE route_key = $1 AND action = 'HOLD_MANUAL_RECOVERY' AND recovery_reason LIKE 'latched:%'`, productionRouteKey).Scan(&staleHoldCount); err != nil {
		t.Fatal(err)
	}
	if staleHoldCount != 0 {
		t.Fatalf("the stale tick left %d latched journal rows", staleHoldCount)
	}

	// The next tick must reach the normal production decision path after the
	// compare-and-set observes the operator's newer generation.
	healthy := manualRecoveryHealthyRefreshObservation("generation-race-healthy", 906)
	healthy.Snapshot.MonitorsArmed = false
	worker.runtime.loadNonterminal = func(context.Context, string) (*PersistedOperation, error) { return nil, nil }
	worker.runtime.beforeRecordLatchedHold = nil
	worker.runtime.observe = func(context.Context) (Observation, error) {
		observeCalls++
		return healthy, nil
	}
	worker.runtime.recordDecision = func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
		return DecisionRecord{OperationID: "generation-race-operation", Cycle: 1, Status: Decided}, nil
	}
	worker.runtime.prepareBridge = func(_ context.Context, _ RouteManifest, decision Decision, _ Observation) (Observation, BridgeExecutionEvidence, error) {
		if decision.Action != VoltrAllocateToSquads {
			t.Fatalf("the cleared tick chose %s instead of the healthy bridge action", decision.Action)
		}
		return healthy, BridgeExecutionEvidence{Request: BridgeBuildRequest{Action: decision.Action}}, nil
	}
	worker.runtime.admitBridge = func(context.Context, string, Observation, Decision, BridgeExecutionEvidence) error {
		return nil
	}
	worker.runtime.buildBridge = func(context.Context, string, BridgeExecutionEvidence) error {
		buildCalls++
		return nil
	}
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if observeCalls != 1 || buildCalls != 1 {
		t.Fatalf("the next tick did not proceed after clear: %d observes, %d builds", observeCalls, buildCalls)
	}
}

func TestManualRecoveryVerifiedConstructionRefreshBuildsThroughProductionPath(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 60*time.Second)
	defer cancel()
	defer db.Close()
	resetManualRecoveryProductionRoute(t, ctx, db, "manual-recovery-verified-refresh")

	manifest := readyWorkerManifest(t)
	initial := manualRecoveryHealthyRefreshObservation("verified-refresh-initial", 907)
	initial.Snapshot.MonitorsArmed = false
	refreshed := manualRecoveryHealthyRefreshObservation("verified-refresh-construction", 908)
	identityCalls := 0
	identity := func(context.Context) (programIdentityObservation, error) {
		identityCalls++
		return programIdentityObservation{Verified: true,
			VoltrProgramDeploySlot: voltrProgramDeploySlot, AdaptorProgramDeploySlot: adaptorProgramDeploySlot}, nil
	}
	state := productionObserveState{
		routeKey: productionRouteKey,
		journal:  &stubProductionJournal{journal: reconciledJournal()},
		batch:    func(context.Context) (Observation, error) { return initial, nil },
		identity: identity,
	}
	var prepareCalls, buildCalls int
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadLatch:       db.ManualRecoveryLatch,
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         state.observe,
		recordManualRecovery: func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
			t.Fatal("a verified construction refresh produced a manual-recovery hold")
			return DecisionRecord{}, nil
		},
		prepareBridge: func(ctx context.Context, _ RouteManifest, decision Decision, _ Observation) (Observation, BridgeExecutionEvidence, error) {
			prepareCalls++
			if err := state.enrich(ctx, &refreshed); err != nil {
				return Observation{}, BridgeExecutionEvidence{}, err
			}
			if !refreshed.Snapshot.ProgramIdentityKnown {
				t.Fatal("the verified identity was not carried into the construction snapshot")
			}
			return refreshed, BridgeExecutionEvidence{Request: BridgeBuildRequest{Action: decision.Action}}, nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
			return DecisionRecord{OperationID: "verified-refresh-operation", Cycle: 1, Status: Decided}, nil
		},
		admitBridge: func(context.Context, string, Observation, Decision, BridgeExecutionEvidence) error { return nil },
		buildBridge: func(context.Context, string, BridgeExecutionEvidence) error {
			buildCalls++
			return nil
		},
	}}
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if identityCalls != 2 || prepareCalls != 1 || buildCalls != 1 {
		t.Fatalf("verified refresh did not complete the production path: identity=%d prepare=%d build=%d", identityCalls, prepareCalls, buildCalls)
	}
	if decision := Decide(refreshed.Snapshot); decision.Action == HoldManualRecovery {
		t.Fatalf("verified construction snapshot still held: %+v", decision)
	}
	if _, latched, err := db.ManualRecoveryLatch(ctx, productionRouteKey); err != nil || latched {
		t.Fatalf("verified construction refresh unexpectedly latched: %v", err)
	}
}

func TestManualRecoveryUnverifiedConstructionRefreshLatchesThroughProductionPath(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 60*time.Second)
	defer cancel()
	defer db.Close()
	resetManualRecoveryProductionRoute(t, ctx, db, "manual-recovery-unverified-refresh")

	manifest := readyWorkerManifest(t)
	initial := manualRecoveryHealthyRefreshObservation("unverified-refresh-initial", 909)
	initial.Snapshot.MonitorsArmed = false
	refreshed := manualRecoveryHealthyRefreshObservation("unverified-refresh-construction", 910)
	identityCalls := 0
	identity := func(context.Context) (programIdentityObservation, error) {
		identityCalls++
		if identityCalls == 1 {
			return programIdentityObservation{Verified: true,
				VoltrProgramDeploySlot: voltrProgramDeploySlot, AdaptorProgramDeploySlot: adaptorProgramDeploySlot}, nil
		}
		return programIdentityObservation{}, nil
	}
	state := productionObserveState{
		routeKey: productionRouteKey,
		journal:  &stubProductionJournal{journal: reconciledJournal()},
		batch:    func(context.Context) (Observation, error) { return initial, nil },
		identity: identity,
	}
	var prepareCalls, buildCalls int
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadLatch:       db.ManualRecoveryLatch,
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         state.observe,
		prepareBridge: func(ctx context.Context, _ RouteManifest, decision Decision, _ Observation) (Observation, BridgeExecutionEvidence, error) {
			prepareCalls++
			if err := state.enrich(ctx, &refreshed); err != nil {
				return Observation{}, BridgeExecutionEvidence{}, err
			}
			if refreshed.Snapshot.ProgramIdentityKnown {
				t.Fatal("the unverified identity unexpectedly became verified")
			}
			return refreshed, BridgeExecutionEvidence{Request: BridgeBuildRequest{Action: decision.Action}}, nil
		},
		recordManualRecovery: db.RecordManualRecovery,
		admitBridge: func(context.Context, string, Observation, Decision, BridgeExecutionEvidence) error {
			t.Fatal("an unverified construction refresh reached admission")
			return nil
		},
		buildBridge: func(context.Context, string, BridgeExecutionEvidence) error {
			buildCalls++
			return nil
		},
	}}
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	latch, latched, err := db.ManualRecoveryLatch(ctx, productionRouteKey)
	if err != nil || !latched {
		t.Fatalf("unverified construction refresh did not latch: %+v %v", latch, err)
	}
	if latch.Reason != "program_identity_unverified" || latch.ObservationID != refreshed.Snapshot.ObservationID || latch.ObservationSlot != refreshed.Snapshot.Slot {
		t.Fatalf("unexpected unverified-refresh latch: %+v", latch)
	}
	if identityCalls != 2 || prepareCalls != 1 || buildCalls != 0 {
		t.Fatalf("unverified refresh reached execution or skipped identity merge: identity=%d prepare=%d build=%d", identityCalls, prepareCalls, buildCalls)
	}
}

func TestManualRecoveryKaminoConstructionErrorAfterRefreshHoldPersists(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 60*time.Second)
	defer cancel()
	defer db.Close()
	resetManualRecoveryProductionRoute(t, ctx, db, "manual-recovery-kamino-refresh")

	manifest := readyWorkerManifest(t)
	initial := manualRecoveryHealthyRefreshObservation("kamino-refresh-initial", 911)
	initial.Snapshot.MonitorsArmed = false
	initial.Snapshot.HasPosition = true
	initial.Snapshot.PositionDebtRaw = 3
	initial.Snapshot.SquadsIdleRaw = 3
	initial.Snapshot.LTVBPS = 7_000
	refreshed := manualRecoveryKaminoRefreshObservation("kamino-refresh-hold", 912)
	identityCalls := 0
	identity := func(context.Context) (programIdentityObservation, error) {
		identityCalls++
		if identityCalls == 1 {
			return programIdentityObservation{Verified: true,
				VoltrProgramDeploySlot: voltrProgramDeploySlot, AdaptorProgramDeploySlot: adaptorProgramDeploySlot}, nil
		}
		return programIdentityObservation{}, nil
	}
	state := productionObserveState{
		routeKey: productionRouteKey,
		journal:  &stubProductionJournal{journal: reconciledJournal()},
		batch:    func(context.Context) (Observation, error) { return initial, nil },
		identity: identity,
	}
	constructionErr := errors.New("Kamino reserve construction failed after refresh")
	var prepareCalls, buildCalls int
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadLatch:       db.ManualRecoveryLatch,
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         state.observe,
		prepareKamino: func(ctx context.Context, _ RouteManifest, decision Decision) (Observation, KaminoExecutionEvidence, error) {
			prepareCalls++
			if decision.Action != DeleverPrimeUSDCStep {
				t.Fatalf("initial decision did not select the Kamino path: %+v", decision)
			}
			if err := state.enrich(ctx, &refreshed); err != nil {
				return Observation{}, KaminoExecutionEvidence{}, err
			}
			return refreshed, KaminoExecutionEvidence{}, constructionErr
		},
		recordManualRecovery: db.RecordManualRecovery,
		admitKamino: func(context.Context, string, Observation, Decision, KaminoExecutionEvidence) error {
			t.Fatal("a Kamino construction hold reached admission")
			return nil
		},
		buildKamino: func(context.Context, string, KaminoExecutionEvidence) error {
			buildCalls++
			return nil
		},
	}}
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	latch, latched, err := db.ManualRecoveryLatch(ctx, productionRouteKey)
	if err != nil || !latched {
		t.Fatalf("Kamino construction error discarded the refresh hold: %+v %v", latch, err)
	}
	if latch.Reason != "program_identity_unverified" || latch.ObservationID != refreshed.Snapshot.ObservationID || latch.ObservationSlot != refreshed.Snapshot.Slot {
		t.Fatalf("unexpected Kamino refresh latch: %+v", latch)
	}
	if identityCalls != 2 || prepareCalls != 1 || buildCalls != 0 {
		t.Fatalf("Kamino construction error reached execution or skipped refresh identity: identity=%d prepare=%d build=%d", identityCalls, prepareCalls, buildCalls)
	}
}
