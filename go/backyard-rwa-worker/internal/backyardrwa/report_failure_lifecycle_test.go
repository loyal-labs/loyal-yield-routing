package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The classified-receipt recovery paths must run the production state machine
// against the real journal, not only pure helpers. Like
// TestPhase3DatabaseAdmissionAndSendFence this is a real PostgreSQL slice on a
// disposable Unix-socket database, and a controlled RPC transport answers.
func TestReportFailureLifecycleAgainstDatabase(t *testing.T) {
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

	newSubmittedOperation := func(t *testing.T, suffix string) (string, string) {
		t.Helper()
		routeKey := fmt.Sprintf("failure-lifecycle-%d-%s", time.Now().UnixNano(), suffix)
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, routeKey); err != nil {
			t.Fatal(err)
		}
		id := routeKey + "-op"
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,expected_effects,transaction_signature,broadcast_intent_at)
			VALUES($1,$2,'submitted','{}','failure-signature',clock_timestamp())`, id, routeKey); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AcquireRouteLease(ctx, routeKey, "failure-lifecycle-writer", time.Minute); err != nil {
			t.Fatal(err)
		}
		return routeKey, id
	}
	submittedOperation := func(id, routeKey string) PersistedOperation {
		return PersistedOperation{
			Operation: Operation{ID: id, RouteKey: routeKey, Decision: Decision{Action: ReportNAV}}, Status: Submitted,
			TransactionSignature: "failure-signature", LastValidBlockHeight: 10,
		}
	}
	// statusValue is the getSignatureStatuses row; transactionValue is the
	// getTransaction result, or "RPC_ERROR" for a pruned/lagging receipt.
	advance := func(t *testing.T, op PersistedOperation, statusValue, transactionValue string) (string, string, int) {
		t.Helper()
		receiptReads := 0
		rpc, err := NewRPCClient("https://rpc.invalid")
		if err != nil {
			t.Fatal(err)
		}
		rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			switch {
			case strings.Contains(string(body), `"method":"getSignatureStatuses"`):
				return response(`{"jsonrpc":"2.0","id":1,"result":{"value":[` + statusValue + `]}}`), nil
			case strings.Contains(string(body), `"method":"getTransaction"`):
				receiptReads++
				if transactionValue == "RPC_ERROR" {
					return nil, fmt.Errorf("failure receipt pruned")
				}
				return response(`{"jsonrpc":"2.0","id":1,"result":` + transactionValue + `}`), nil
			}
			t.Fatalf("unexpected RPC during failure recovery: %s", body)
			return nil, fmt.Errorf("unexpected RPC")
		})
		if err := AdvanceNonterminal(ctx, db, rpc, op); err != nil {
			t.Fatal(err)
		}
		var status, reason string
		if err := db.pool.QueryRow(ctx, `SELECT status,COALESCE(recovery_reason,'') FROM loyal_yield.multiply_operations WHERE operation_id=$1`, op.ID).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		return status, reason, receiptReads
	}
	finalizedFailure := `{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"finalized"}`

	t.Run("a processed-only failure keeps observing", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "processed")
		op := submittedOperation(id, routeKey)
		status, reason, receiptReads := advance(t, op,
			`{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"processed"}`,
			`{"slot":45,"meta":{"err":null,"logMessages":[]}}`)
		if status != "submitted" || reason != "" || receiptReads != 0 {
			t.Fatalf("a forked-away failure transitioned the journal: status=%s reason=%q receiptReads=%d", status, reason, receiptReads)
		}
	})

	// advanceRechecked is advance with a mutable signature status: the receipt
	// timeout re-reads the signature at finalized commitment, and the re-read
	// must be able to observe a different confirmation state than the failure
	// that first reached the ambiguous path.
	advanceRechecked := func(t *testing.T, op PersistedOperation, statusValue *string, transactionValue string) (string, string) {
		t.Helper()
		rpc, err := NewRPCClient("https://rpc.invalid")
		if err != nil {
			t.Fatal(err)
		}
		rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			switch {
			case strings.Contains(string(body), `"method":"getSignatureStatuses"`):
				return response(`{"jsonrpc":"2.0","id":1,"result":{"value":[` + *statusValue + `]}}`), nil
			case strings.Contains(string(body), `"method":"getTransaction"`):
				if transactionValue == "RPC_ERROR" {
					return nil, fmt.Errorf("failure receipt pruned")
				}
				return response(`{"jsonrpc":"2.0","id":1,"result":` + transactionValue + `}`), nil
			}
			t.Fatalf("unexpected RPC during the receipt timeout re-read: %s", body)
			return nil, fmt.Errorf("unexpected RPC")
		})
		if err := AdvanceNonterminal(ctx, db, rpc, op); err != nil {
			t.Fatal(err)
		}
		var status, reason string
		if err := db.pool.QueryRow(ctx, `SELECT status,COALESCE(recovery_reason,'') FROM loyal_yield.multiply_operations WHERE operation_id=$1`, op.ID).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		return status, reason
	}
	ageBroadcast := func(t *testing.T, id string) {
		t.Helper()
		if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET broadcast_intent_at=clock_timestamp()-interval '16 minutes' WHERE operation_id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("an unreadable receipt retries and then keeps observing past its window when the re-read is not finalized", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "unreadable")
		op := submittedOperation(id, routeKey)
		receipt := `{"slot":45,"meta":{"err":null,"logMessages":[]}}`
		status, reason, _ := advance(t, op, finalizedFailure, receipt)
		if status != "submitted" || reason != "" {
			t.Fatalf("a contradictory receipt left the ambiguous submission state: %s %q", status, reason)
		}
		status, reason, _ = advance(t, op, finalizedFailure, "RPC_ERROR")
		if status != "submitted" || reason != "" {
			t.Fatalf("a lagging RPC entered manual recovery: %s %q", status, reason)
		}
		ageBroadcast(t, id)
		// A confirmed-only re-read is still forkable: the window may not
		// terminate the row, and it must stay in its ambiguous submission
		// state for the next tick.
		confirmedOnly := `{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"confirmed"}`
		status, reason = advanceRechecked(t, op, &confirmedOnly, "RPC_ERROR")
		if status != "submitted" || reason != "" {
			t.Fatalf("a confirmed-only re-read terminated the row: %s %q", status, reason)
		}
	})

	t.Run("a forked confirmation that settles as a success reaches confirmation instead of failed", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "forked-success")
		op := submittedOperation(id, routeKey)
		ageBroadcast(t, id)
		settledSuccess := `{"slot":45,"err":null,"confirmationStatus":"finalized"}`
		status, reason := advanceRechecked(t, op, &settledSuccess, "RPC_ERROR")
		if status != "confirmed" || reason != "" {
			t.Fatalf("a settled success did not reach the confirmation path: %s %q", status, reason)
		}
	})

	t.Run("an aged finalized failure retains its reservation without a complete receipt", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "aged-finalized")
		op := submittedOperation(id, routeKey)
		ageBroadcast(t, id)
		finalizedFailureAgain := `{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"finalized"}`
		status, reason := advanceRechecked(t, op, &finalizedFailureAgain, "RPC_ERROR")
		if status != "submitted" || reason != "" {
			t.Fatalf("status-only proof released an ambiguous paid failure: %s %q", status, reason)
		}
	})

	t.Run("finalized adaptor logs still require exact wire and fee evidence", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "adaptor")
		op := submittedOperation(id, routeKey)
		receipt := `{"slot":500,"meta":{"err":{"InstructionError":[0,{"Custom":9}]},"logMessages":` +
			mustJSONLogs(t, adaptorFailureLogs(bridgeAdaptorProgram, 9)) + `}}`
		status, reason, _ := advance(t, op, finalizedFailure, receipt)
		if status != "submitted" || reason != "" {
			t.Fatalf("logs-only refusal settled without fee and wire proof: %s %q", status, reason)
		}
	})

	t.Run("unattributable and non-adaptor errors stay capital stops", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "unattributable")
		op := submittedOperation(id, routeKey)
		status, reason, _ := advance(t, op, finalizedFailure,
			`{"slot":500,"meta":{"err":{"InstructionError":[0,{"Custom":9}]},"logMessages":[]}}`)
		if status != "manual_recovery" || reason != unclassifiedTransactionErrReason {
			t.Fatalf("truncated failure logs lost the capital stop: %s %q", status, reason)
		}
		otherRoute, otherID := newSubmittedOperation(t, "voltr")
		other := submittedOperation(otherID, otherRoute)
		otherReceipt := `{"slot":500,"meta":{"err":{"InstructionError":[0,{"Custom":6004}]},"logMessages":` +
			mustJSONLogs(t, adaptorFailureLogs(bridgeVoltrProgram, 6004)) + `}}`
		status, reason, _ = advance(t, other, finalizedFailure, otherReceipt)
		if status != "manual_recovery" || reason != unclassifiedTransactionErrReason {
			t.Fatalf("a non-adaptor error lost the capital stop: %s %q", status, reason)
		}
	})

	t.Run("a settled success is never classified", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "success")
		op := submittedOperation(id, routeKey)
		status, reason, receiptReads := advance(t, op, `{"slot":45,"err":null,"confirmationStatus":"confirmed"}`, "RPC_ERROR")
		if status != "confirmed" || reason != "" || receiptReads != 0 {
			t.Fatalf("a successful receipt was classified as a failure: status=%s reason=%q receiptReads=%d", status, reason, receiptReads)
		}
	})

	// The send fence runs the real signed path: reservation, build input, wire
	// binding, then refusal. A refused wire must terminate without ever being
	// revalued, submitted, or kept advancing on a later tick.
	t.Run("the stale signed fence refuses without revaluation or send", func(t *testing.T) {
		routeKey := fmt.Sprintf("failure-lifecycle-stale-%d", time.Now().UnixNano())
		budget := emptyTestBudget()
		budget.Families["OnRe"] = FamilyBudget{ExitMicros: 4_000_000}
		state, err := json.Marshal(map[string]any{"generation": 1, "phase3": budget})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,$2::jsonb)`, routeKey, string(state)); err != nil {
			t.Fatal(err)
		}
		id := routeKey + "-op"
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,expected_effects,strategy_key)
			VALUES($1,$2,'decided','{}','OnRe/ONyc/USDC')`, id, routeKey); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AcquireRouteLease(ctx, routeKey, "failure-lifecycle-writer", time.Minute); err != nil {
			t.Fatal(err)
		}
		request := bridgeTestRequest(ReportNAV, 0)
		request.LastValidBlockHeight = 10
		request.Report.ObservedSlot, request.Report.Sequence = 42, 42
		buildEffects, _, _, err := bridgeExpectedEffects(Decision{Action: ReportNAV}, 0, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		effects, err := jsonMarshalExpectedEffects(buildEffects)
		if err != nil {
			t.Fatal(err)
		}
		reservation := testReservation()
		reservation.Recovery = true
		reservation.OperationID = id
		reservation.IntentSHA256, err = Phase3IntentDigest(request, effects)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.ReservePhase3(ctx, reservation); err != nil {
			t.Fatal(err)
		}
		if err := db.AuthorizePhase3Build(ctx, id, request, effects); err != nil {
			t.Fatal(err)
		}
		message, err := CompileBridgeMessage(request)
		if err != nil {
			t.Fatal(err)
		}
		wire := append(make([]byte, 65), message...)
		wire[0] = 1
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.bindPhase3WireTx(ctx, tx, id, sha256Bytes(wire)); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2 WHERE operation_id=$1`, id, wire); err != nil {
			t.Fatal(err)
		}
		operation := PersistedOperation{
			Operation: Operation{ID: id, RouteKey: routeKey, Decision: Decision{Action: ReportNAV}}, Status: Signed,
			SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]),
			RecentBlockhash: request.RecentBlockhash, LastValidBlockHeight: 10,
		}
		slotReads := 0
		rpc, err := NewRPCClient("https://rpc.invalid")
		if err != nil {
			t.Fatal(err)
		}
		// observed slot 42 + a confirmed slot of 71 is past observed+28, so the
		// report can no longer land inside the adaptor's age window.
		rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			if strings.Contains(string(body), `"method":"getSlot"`) {
				slotReads++
				return response(`{"jsonrpc":"2.0","id":1,"result":71}`), nil
			}
			t.Fatalf("the stale fence allowed another RPC call: %s", body)
			return nil, fmt.Errorf("unexpected RPC")
		})
		if err := AdvanceNonterminal(ctx, db, rpc, operation); err != nil {
			t.Fatal(err)
		}
		if slotReads != 1 {
			t.Fatalf("refusal did not stop the lifecycle: %d slot reads", slotReads)
		}
		var status, reason string
		var submitted bool
		if err := db.pool.QueryRow(ctx, `SELECT status,COALESCE(recovery_reason,''),broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status, &reason, &submitted); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || reason != "report_stale" || submitted {
			t.Fatalf("a refused wire crossed the broadcast boundary: status=%s reason=%q submitted=%t", status, reason, submitted)
		}
		var persisted []byte
		if err := db.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&persisted); err != nil {
			t.Fatal(err)
		}
		var released Phase3Budget
		if json.Unmarshal(persisted, &released) != nil || len(released.Reservations) != 0 ||
			released.Families["OnRe"].ExitMicros != 4_000_000 || released.Families["OnRe"].SpentMicros != 0 {
			t.Fatalf("a refused wire did not release its unspent reservation: %s", persisted)
		}
	})
}
