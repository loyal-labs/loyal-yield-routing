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

// Isolate the storage identity/lease cases below from the RPC valuation tests.
// There is no unpriced build authorization entrypoint in the production binary.
func (d *Database) AuthorizePhase3Build(ctx context.Context, operationID string, request any, effects []byte) error {
	return d.authorizePhase3Build(ctx, operationID, request, effects, ValuedTransactionCost{TotalMicros: 1})
}

// Storage-only legacy cases below isolate locking from fresh RPC valuation.
// Production has no unpriced MarkBroadcastIntent method.
func (d *Database) MarkBroadcastIntent(ctx context.Context, operationID string) error {
	var encoded, wire []byte
	if err := d.pool.QueryRow(ctx, `SELECT expected_effects->'phase3',signed_wire FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&encoded, &wire); err != nil {
		return err
	}
	var auth phase3OperationAuthorization
	if err := json.Unmarshal(encoded, &auth); err != nil {
		return err
	}
	rpc, _ := NewRPCClient("https://rpc.invalid")
	rpc.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"result":42}`), nil
	})
	return d.markBroadcastIntent(ctx, operationID, rpc, auth.IntentSHA256, sha256Bytes(wire), ValuedTransactionCost{TotalMicros: 1, ObservationSlot: 42, ValidThroughSlot: 74})
}

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
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS action text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS strategy_key text;
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
	request := bridgeTestRequest(ReportNAV, 0)
	request.LastValidBlockHeight = 10
	buildEffects, _, _, err := bridgeExpectedEffects(Decision{Action: ReportNAV}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	effects, err := jsonMarshalExpectedEffects(buildEffects)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := Phase3IntentDigest(request, effects)
	if err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.AuthorizePhase3Build(ctx, op, request, effects), "unreserved_build_intent")
	r := testReservation()
	r.Recovery = true
	r.OperationID = op
	r.IntentSHA256 = digest
	// A syntactically valid family cannot pay for an unrelated journal lane.
	assertBudgetHold(t, db.ReservePhase3(ctx, r), "reservation_family_does_not_match_journal_lane")
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET strategy_key='AUTO/AUTO/PYUSD' WHERE operation_id=$1`, op); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.ReservePhase3(ctx, r), "reservation_family_does_not_match_journal_lane")
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET strategy_key='OnRe/ONyc/USDC' WHERE operation_id=$1`, op); err != nil {
		t.Fatal(err)
	}
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
	assertBudgetHold(t, db.authorizePhase3Build(ctx, op, request, effects, ValuedTransactionCost{TotalMicros: r.UpperMicros + 1}), "fresh_build_cost_exceeds_reservation")
	assertBudgetHold(t, db.authorizePhase3Build(ctx, op, request, effects, ValuedTransactionCost{}), "fresh_build_cost_exceeds_reservation")
	// The same identity fence applies again after admission, before signing
	// and sending; a changed journal lane cannot inherit the old reservation.
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET strategy_key='AUTO/AUTO/PYUSD' WHERE operation_id=$1`, op); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, db.AuthorizePhase3Build(ctx, op, request, effects), "reservation_family_does_not_match_journal_lane")
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET strategy_key='OnRe/ONyc/USDC' WHERE operation_id=$1`, op); err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.Report.NAVAfterRaw++
	assertBudgetHold(t, db.AuthorizePhase3Build(ctx, op, changed, effects), "unreserved_build_intent")
	// Inject a simulated persisted wire in the isolated fixture. The real send
	// transition must reject it until the reservation is bound to those bytes.
	message, err := CompileBridgeMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately unsigned signature bytes. The local fixture does not call
	// PersistSigned or send; cryptographic signer proof is a separate gate.
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
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
	// Resume from persisted typed build inputs after restart. Expensive/stale
	// quotes reject without committing broadcast intent or releasing budget.
	signedOperation := PersistedOperation{Operation: Operation{ID: op, RouteKey: key, Decision: Decision{Action: ReportNAV}}, Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: request.RecentBlockhash, LastValidBlockHeight: 10}
	assertBudgetHold(t, AdvanceNonterminal(ctx, restarted, budgetBuildRPC(t, 20_000_000, 42), signedOperation), "transaction_cap_exceeded")
	assertBudgetHold(t, AdvanceNonterminal(ctx, restarted, budgetBuildRPC(t, 5_000, 75), signedOperation), "fee_message_or_slot_mismatch")
	var rejectedStatus string
	var rejectedIntent bool
	if err = restarted.pool.QueryRow(ctx, `SELECT status,broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, op).Scan(&rejectedStatus, &rejectedIntent); err != nil {
		t.Fatal(err)
	}
	if rejectedStatus != "signed" || rejectedIntent {
		t.Fatal("rejected revaluation crossed the broadcast boundary")
	}
	if err = restarted.RevalueAndMarkBroadcastIntent(ctx, budgetBuildRPC(t, 5_000, 42), signedOperation); err != nil {
		t.Fatal(err)
	}
	var sendAuthJSON []byte
	if err = restarted.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, op).Scan(&sendAuthJSON); err != nil {
		t.Fatal(err)
	}
	var sendAuth phase3OperationAuthorization
	if json.Unmarshal(sendAuthJSON, &sendAuth) != nil || sendAuth.SendKnownCost == nil || sendAuth.SendKnownCost.MessageSHA256 != sha256Bytes(message) || sendAuth.SendKnownCost.ValidThroughSlot != 74 {
		t.Fatal("final-send cost was not durably bound to the original message")
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
	_, err = restarted.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,expected_effects,strategy_key) VALUES($1,$2,'decided',$3::jsonb,'OnRe/ONyc/USDC')`, settleID, key, string(expectedJSON))
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
	holdID := op + "-hold"
	if _, err = restarted.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,expected_effects,strategy_key) VALUES($1,$2,'decided','{}','OnRe/ONyc/USDC')`, holdID, key); err != nil {
		t.Fatal(err)
	}
	holdReservation := r
	holdReservation.OperationID = holdID
	holdReservation.ExitAfterMicros = 2_000_000
	if err = restarted.ReservePhase3(ctx, holdReservation); err != nil {
		t.Fatal(err)
	}
	hold := &BudgetHold{Reason: "transaction_cap_exceeded", Details: map[string]string{"upperMicros": "1000001"}}
	if _, err = restarted.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed' WHERE operation_id=$1`, holdID); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, restarted.RecordPhase3BudgetHold(ctx, holdID, hold), "budget_hold_requires_never_submitted_operation")
	if _, err = restarted.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='decided' WHERE operation_id=$1`, holdID); err != nil {
		t.Fatal(err)
	}
	if err = restarted.RecordPhase3BudgetHold(ctx, holdID, hold); err != nil {
		t.Fatal(err)
	}
	var status, reason string
	var holdJSON []byte
	if err = restarted.pool.QueryRow(ctx, `SELECT status,recovery_reason,expected_effects->'budgetHold' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, holdID).Scan(&status, &reason, &holdJSON); err != nil {
		t.Fatal(err)
	}
	var retainedHold BudgetHold
	if json.Unmarshal(holdJSON, &retainedHold) != nil || status != "failed" || reason != "phase3_budget_hold:transaction_cap_exceeded" || retainedHold.Details["upperMicros"] != "1000001" {
		t.Fatalf("typed admission hold was not retained: %s %s %s", status, reason, holdJSON)
	}
	if err = restarted.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(persisted, &settled) != nil || len(settled.Reservations) != 0 || settled.Families["OnRe"].SpentMicros != 900_000 || settled.Families["OnRe"].ExitMicros != 3_000_000 {
		t.Fatalf("unsent HOLD did not restore prior exit reservation: %s", persisted)
	}
	// A cap-held signed wire must not hold the queue forever after finalized
	// expiry. Malformed absence evidence retains it; explicit absence releases
	// only that unspent reservation and restores the original exit reserve.
	expiredID := op + "-signed-expiry"
	if _, err = restarted.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,expected_effects,strategy_key) VALUES($1,$2,'decided','{}','OnRe/ONyc/USDC')`, expiredID, key); err != nil {
		t.Fatal(err)
	}
	expiredReservation := holdReservation
	expiredReservation.OperationID = expiredID
	if err = restarted.ReservePhase3(ctx, expiredReservation); err != nil {
		t.Fatal(err)
	}
	if err = restarted.AuthorizePhase3Build(ctx, expiredID, request, effects); err != nil {
		t.Fatal(err)
	}
	tx, err = restarted.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.bindPhase3WireTx(ctx, tx, expiredID, sha256Bytes(wire)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2 WHERE operation_id=$1`, expiredID, wire); err != nil {
		t.Fatal(err)
	}
	expiredOperation := signedOperation
	expiredOperation.ID = expiredID
	expiryRPC := budgetBuildRPC(t, 20_000_000, 42)
	baseTransport := expiryRPC.client.Transport
	absenceJSON := "[]"
	expiryRPC.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		request.Body = io.NopCloser(strings.NewReader(string(body)))
		if strings.Contains(string(body), `"method":"getBlockHeight"`) {
			if !strings.Contains(string(body), `"commitment":"finalized"`) {
				t.Fatal("expiry was not finalized")
			}
			return response(`{"jsonrpc":"2.0","id":1,"result":11}`), nil
		}
		if strings.Contains(string(body), `"method":"getSignatureStatuses"`) {
			return response(`{"jsonrpc":"2.0","id":1,"result":{"value":` + absenceJSON + `}}`), nil
		}
		return baseTransport.RoundTrip(request)
	})
	if err = AdvanceNonterminal(ctx, restarted, expiryRPC, expiredOperation); err == nil {
		t.Fatal("malformed absence released a signed HOLD")
	}
	if err = restarted.pool.QueryRow(ctx, `SELECT status,broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, expiredID).Scan(&rejectedStatus, &rejectedIntent); err != nil {
		t.Fatal(err)
	}
	if rejectedStatus != "signed" || rejectedIntent {
		t.Fatal("malformed absence changed the signed boundary")
	}
	absenceJSON = "[null]"
	if err = AdvanceNonterminal(ctx, restarted, expiryRPC, expiredOperation); err != nil {
		t.Fatal(err)
	}
	if err = restarted.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(persisted, &settled) != nil || len(settled.Reservations) != 0 || settled.Families["OnRe"].SpentMicros != 900_000 || settled.Families["OnRe"].ExitMicros != 3_000_000 {
		t.Fatal("signed expiry did not preserve gross spend and restore exit headroom")
	}
	t.Run("non-USDC conversion journal safety", func(t *testing.T) {
		tx, err := restarted.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		decisionRoute := key + "-debt-decisions"
		if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, decisionRoute); err != nil {
			t.Fatal(err)
		}
		for i, action := range []Action{SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
			id := fmt.Sprintf("%s-%d", decisionRoute, i)
			if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,expected_effects,confirmed_slot) VALUES($1,$2,'manual_recovery',$3,'{}',$4)`, id, decisionRoute, action, i*2+1); err != nil {
				t.Fatal(err)
			}
			var blocked, navRequired bool
			if err = tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, decisionRoute).Scan(&blocked); err != nil || !blocked {
				t.Fatalf("ambiguous %s lost capital fence: %v", action, err)
			}
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciled' WHERE operation_id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if err = tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, decisionRoute).Scan(&blocked); err != nil || blocked {
				t.Fatalf("reconciled %s retained ambiguity: %v", action, err)
			}
			if err = tx.QueryRow(ctx, PostMutationNAVRequiredSQL, decisionRoute).Scan(&navRequired); err != nil || !navRequired {
				t.Fatalf("%s lost NAV obligation: %v", action, err)
			}
			if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,expected_effects,confirmed_slot) VALUES($1,$2,'reconciled','REPORT_NAV','{}',$3)`, id+"-report", decisionRoute, i*2+2); err != nil {
				t.Fatal(err)
			}
			if err = tx.QueryRow(ctx, PostMutationNAVRequiredSQL, decisionRoute).Scan(&navRequired); err != nil || navRequired {
				t.Fatalf("later report did not clear %s NAV obligation: %v", action, err)
			}
		}
	})
}
