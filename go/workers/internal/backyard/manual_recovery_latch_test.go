package backyard

import (
	"context"
	"errors"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
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
	// The test's own handle takes no lock; reopened below is the worker's.
	db, err := openOperatorDatabase(ctx, url)
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
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version)
		 VALUES($1,'{"generation":1,"cycle":1}',1)
		 ON CONFLICT (route_key) DO UPDATE SET state = EXCLUDED.state, state_version = EXCLUDED.state_version, lease_owner = NULL, lease_expires_at = NULL`, routeKey); err != nil {
		t.Fatal(err)
	}

	manifest := readyWorkerManifest(t)
	fault := true
	var observeCalls, buildCalls, preparedCalls int
	var recorded []string
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
		latch: func(ctx context.Context, key string, latch ManualRecoveryLatch) error {
			recorded = append(recorded, latch.Reason)
			return db.LatchManualRecovery(ctx, key, latch)
		},
		prepareBridge: func(context.Context, RouteManifest, Decision, Observation) (Observation, BridgeExecutionEvidence, error) {
			preparedCalls++
			return Observation{}, BridgeExecutionEvidence{}, errExecutionResumed
		},
		prepareKamino: func(context.Context, RouteManifest, Decision, Observation) (Observation, KaminoExecutionEvidence, error) {
			preparedCalls++
			return Observation{}, KaminoExecutionEvidence{}, errExecutionResumed
		},
		prepareJupiter: func(context.Context, RouteManifest, Decision, Observation) (Observation, JupiterExecutionEvidence, error) {
			preparedCalls++
			return Observation{}, JupiterExecutionEvidence{}, errExecutionResumed
		},
		build: func(context.Context, string, any, ExpectedEffects) error {
			buildCalls++
			return nil
		},
	}}

	// (a) The production observe path classifies the broken receipt, Decide
	// returns the manual recovery hold, and the route is latched: one latch
	// row, no journal row.
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	latch, latched, err := db.ManualRecoveryLatch(ctx, routeKey)
	if err != nil || !latched {
		t.Fatalf("the manual recovery hold was not latched: %+v %v", latch, err)
	}
	if latch.Reason != "strategy_receipt_integrity" || latch.ObservationSlot != 77 || latch.ObservationID == "" {
		t.Fatalf("unexpected latch identity: %+v", latch)
	}
	assertNoJournalRows := func(t *testing.T) {
		t.Helper()
		var rows int
		if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("a hold wrote %d journal rows: %v", rows, err)
		}
	}
	assertNoJournalRows(t)

	// (d) Restart simulation: a new worker state object over the same database
	// still sees the stop.
	reopened, err := OpenDatabase(ctx, engine.DirectDSN(url))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, latched, err := reopened.ManualRecoveryLatch(ctx, routeKey); err != nil || !latched {
		t.Fatalf("a restarted worker lost the latch: %v", err)
	}

	// (b) A healthy batch behind the latch is never observed, never executes
	// and writes nothing.
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
	if len(recorded) != healthy {
		t.Fatalf("the latched tick wrote the stop again: %+v", recorded)
	}
	assertNoJournalRows(t)

	// (c) clear-hold is the only way out: the latch row carries the operator
	// reason and the next healthy tick decides from live data again.
	if _, err := ClearManualRecoveryHold(ctx, url, routeKey, "operator re-verified the strategy receipt"); err != nil {
		t.Fatal(err)
	}
	if _, latched, err := db.ManualRecoveryLatch(ctx, routeKey); err != nil || latched {
		t.Fatalf("the latch survived its explicit clear: %v", err)
	}
	var clearedReason string
	if err := db.pool.QueryRow(ctx, `SELECT cleared_reason FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1`, routeKey).Scan(&clearedReason); err != nil ||
		clearedReason != "operator re-verified the strategy receipt" {
		t.Fatalf("the clear did not keep its reason: %q %v", clearedReason, err)
	}
	assertNoJournalRows(t)
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
	if len(recorded) != healthy {
		t.Fatalf("a healthy tick after an explicit clear latched again: %+v", recorded)
	}

	// An empty reason never clears anything, and an unknown route fails loudly.
	if _, err := ClearManualRecoveryHold(ctx, url, routeKey, ""); err == nil {
		t.Fatal("an empty reason cleared a manual recovery hold")
	}
	if _, err := ClearManualRecoveryHold(ctx, url, routeKey+"-absent", "no latch here"); err == nil {
		t.Fatal("cleared a latch that did not exist")
	}
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
	// The fixture's handle takes no lock: a test opens the writer itself.
	db, err := openOperatorDatabase(ctx, url)
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
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version)
		VALUES($1,'{"generation":1,"cycle":1}',1)
		ON CONFLICT (route_key) DO UPDATE SET state = EXCLUDED.state, state_version = EXCLUDED.state_version, lease_owner = NULL, lease_expires_at = NULL`, productionRouteKey); err != nil {
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
	if err != nil {
		return err
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE loyal_yield.backyard_manual_recovery_latches ADD COLUMN IF NOT EXISTS generation bigint NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return applyTargetInputMigration(ctx, db)
}

// applyTargetInputMigration applies the shipped 0103 migration: target()'s
// stored inputs, the lane levels and the operator exits.
func applyTargetInputMigration(ctx context.Context, db *Database) error {
	for _, file := range []string{"0103_backyard_target_inputs.sql", "0123_backyard_lane_exit_costs.sql"} {
		raw, err := os.ReadFile("../../../../migrations/yield/" + file)
		if err != nil {
			return err
		}
		if _, err = db.pool.Exec(ctx, string(raw)); err != nil {
			return err
		}
	}
	return nil
}
