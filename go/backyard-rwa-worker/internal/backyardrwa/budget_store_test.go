package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS recovery_reason text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS transaction_signature text,
	 ADD COLUMN IF NOT EXISTS confirmed_slot bigint,ADD COLUMN IF NOT EXISTS confirmation_status text,
	 ADD COLUMN IF NOT EXISTS reconciliation_sha256 text,ADD COLUMN IF NOT EXISTS reconciled_effects jsonb;
	CREATE UNIQUE INDEX IF NOT EXISTS multiply_operations_one_nonterminal_per_route
	 ON loyal_yield.multiply_operations(route_key) WHERE status IN ('decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling');`)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("phase3-test-%d", time.Now().UnixNano())
	op := key + "-op"
	b := emptyTestBudget()
	b.Families["OnRe"] = FamilyBudget{ExitMicros: 4_000_000}
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
	r.Recovery = true
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
	expectedReservation := r
	expectedReservation.ExitBeforeMicros = 4_000_000
	if len(retained.Reservations) != 1 || retained.Reservations[op] != expectedReservation {
		t.Fatalf("restart lost reservation: %+v", retained)
	}
	// The old writer lost its fence; a durable intent is not a send permission
	// for a new or stale process. This test makes no RPC or signer calls.
	if err = db.MarkBroadcastIntent(ctx, op); err == nil {
		t.Fatal("stale writer retained authority")
	}
	// Exercise the real recovery coordinator and journal transition against
	// a controlled RPC transport. No signer or network submission is possible.
	rpc, _ := NewRPCClient("https://rpc.invalid")
	statusReads := 0
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		if strings.Contains(string(body), `"method":"getSignatureStatuses"`) {
			statusReads++
			return response(`{"jsonrpc":"2.0","id":1,"result":{"value":[null]}}`), nil
		}
		if strings.Contains(string(body), `"method":"getBlockHeight"`) && strings.Contains(string(body), `"commitment":"finalized"`) {
			return response(`{"jsonrpc":"2.0","id":1,"result":11}`), nil
		}
		t.Fatalf("unexpected RPC during unspent release: %s", body)
		return nil, fmt.Errorf("unexpected RPC")
	})
	operation := PersistedOperation{Operation: Operation{ID: op, RouteKey: key}, Status: BroadcastIntent, TransactionSignature: "controlled-signature", LastValidBlockHeight: 10}
	if err = AdvanceNonterminal(ctx, restarted, rpc, operation); err != nil {
		t.Fatal(err)
	}
	if statusReads != 2 {
		t.Fatalf("expiry did not recheck signature absence: %d", statusReads)
	}
	if err = restarted.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	var released Phase3Budget
	if err = json.Unmarshal(persisted, &released); err != nil {
		t.Fatal(err)
	}
	if len(released.Reservations) != 0 || released.Families["OnRe"].ExitMicros != 4_000_000 || released.Families["OnRe"].SpentMicros != 0 {
		t.Fatalf("expiry did not restore original exit budget: %+v", released)
	}
	// A second controlled journal operation checks finalized settlement. Its
	// token effects are an unchanged pinned custody; no chain call is made.
	settleID := op + "-settle"
	expected := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{{Address: bridgeSquadsATA, Owner: classicTokenProgram, Mint: bridgeUSDC, Authority: bridgeVault}}}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	_, err = restarted.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,expected_effects) VALUES($1,$2,'decided',$3::jsonb)`, settleID, key, string(expectedJSON))
	if err != nil {
		t.Fatal(err)
	}
	settleReservation := r
	settleReservation.OperationID = settleID
	settleReservation.IntentSHA256, err = Phase3IntentDigest(request, expectedJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.ReservePhase3(ctx, settleReservation); err != nil {
		t.Fatal(err)
	}
	tx, err = restarted.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.bindPhase3WireTx(ctx, tx, settleID, sha256Bytes(wire)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = restarted.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciling',signed_wire=$2,transaction_signature='settlement-signature',confirmed_slot=42,confirmation_status='confirmed' WHERE operation_id=$1`, settleID, wire)
	if err != nil {
		t.Fatal(err)
	}
	balances := []TransactionTokenBalance{{Address: bridgeSquadsATA, OwnerProgram: classicTokenProgram, Mint: bridgeUSDC, Authority: bridgeVault}}
	receipt := ConfirmedTransactionEvidence{Signature: "settlement-signature", Slot: 42, PreTokenBalances: balances, PostTokenBalances: balances}
	reconciliation, reconciledEffects, err := ReconcileConfirmedTransaction(expected, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.MarkReconciled(ctx, settleID, reconciliation, reconciledEffects, receipt); err == nil {
		t.Fatal("confirmed-only receipt released budget")
	}
	receipt.Finalized = true
	receipt.Signature = "wrong-signature"
	if err = restarted.MarkReconciled(ctx, settleID, reconciliation, reconciledEffects, receipt); err == nil {
		t.Fatal("unrelated finalized receipt settled budget")
	}
	receipt.Signature = "settlement-signature"
	if err = restarted.MarkReconciled(ctx, settleID, reconciliation, reconciledEffects, receipt); err != nil {
		t.Fatal(err)
	}
	if err = restarted.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	var settled Phase3Budget
	if err = json.Unmarshal(persisted, &settled); err != nil {
		t.Fatal(err)
	}
	if len(settled.Reservations) != 0 || settled.Families["OnRe"].SpentMicros != 900_000 || settled.Families["OnRe"].ExitMicros != 3_000_000 {
		t.Fatalf("finalized settlement is not atomic: %+v", settled)
	}
}
