package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// This is a real PostgreSQL admission/locking/wire-binding slice, not a full
// migration replay. Only a disposable Unix-socket database can enable it.
// The final verifier must separately check production schema and deployment.
func TestPhase3DatabaseAdmissionAndSendFence(t *testing.T) {
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
	_, err = db.pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS loyal_yield;
	CREATE TABLE IF NOT EXISTS loyal_yield.multiply_route_states (
	 route_key text PRIMARY KEY,state_version bigint NOT NULL DEFAULT 1,state jsonb NOT NULL,
	 lease_owner text,lease_expires_at timestamptz,fencing_token bigint NOT NULL DEFAULT 0,
	 updated_at timestamptz NOT NULL DEFAULT now(),
	 CHECK ((state->>'generation')::bigint=state_version),
	 CHECK ((lease_owner IS NULL)=(lease_expires_at IS NULL)));
	CREATE TABLE IF NOT EXISTS loyal_yield.multiply_operations (
	 operation_id text PRIMARY KEY,route_key text NOT NULL REFERENCES loyal_yield.multiply_route_states,
	 status text NOT NULL,expected_effects jsonb NOT NULL,signed_wire bytea,broadcast_intent_at timestamptz,
	 updated_at timestamptz NOT NULL DEFAULT now());
	CREATE UNIQUE INDEX IF NOT EXISTS multiply_operations_one_nonterminal_per_route
	 ON loyal_yield.multiply_operations(route_key) WHERE status IN ('decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling');`)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("phase3-test-%d", time.Now().UnixNano())
	op := key + "-op"
	b := emptyTestBudget()
	state, err := json.Marshal(map[string]any{"generation": 1, "phase3": b})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,$2::jsonb)`, key, string(state))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,expected_effects) VALUES($1,$2,'decided','{}')`, op, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "writer-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	request := struct{ MaximumDebit uint64 }{900_000}
	effects := []byte(`{"maximumDebit":900000}`)
	digest, err := Phase3IntentDigest(request, effects)
	if err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.AuthorizePhase3Build(ctx, op, request, effects), "unreserved_build_intent")
	r := testReservation()
	r.OperationID = op
	r.IntentSHA256 = digest
	over := r
	over.UpperMicros = 1_000_001
	assertBudgetHold(t, db.ReservePhase3(ctx, over), "transaction_cap_exceeded")
	// Concurrent requests sharing the one lease cannot both consume a fresh
	// budget for different economic interpretations of the same operation.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- db.ReservePhase3(ctx, r) }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = db.AuthorizePhase3Build(ctx, op, request, effects); err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.MaximumDebit++
	assertBudgetHold(t, db.AuthorizePhase3Build(ctx, op, changed, effects), "unreserved_build_intent")
	// Inject a simulated persisted wire in the isolated fixture. The real send
	// transition must reject it until the reservation is bound to those bytes.
	wire := []byte("controlled unsigned fixture: never sent")
	_, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2 WHERE operation_id=$1`, op, wire)
	if err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.MarkBroadcastIntent(ctx, op), "signed_wire_reservation_mismatch")
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.bindPhase3WireTx(ctx, tx, op, sha256Bytes(wire)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.AcquireRouteLease(ctx, key, "writer-b", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = restarted.MarkBroadcastIntent(ctx, op); err != nil {
		t.Fatal(err)
	}
	var persisted []byte
	if err = restarted.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	var retained Phase3Budget
	if err = json.Unmarshal(persisted, &retained); err != nil {
		t.Fatal(err)
	}
	if len(retained.Reservations) != 1 || retained.Reservations[op] != r {
		t.Fatalf("restart lost reservation: %+v", retained)
	}
	// The old writer lost its fence; a durable intent is not a send permission
	// for a new or stale process. This test makes no RPC or signer calls.
	if err = db.MarkBroadcastIntent(ctx, op); err == nil {
		t.Fatal("stale writer retained authority")
	}
}
