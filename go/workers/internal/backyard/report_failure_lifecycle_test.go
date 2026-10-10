package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
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
			VALUES($1,$2,'submitted','{}',$3,clock_timestamp())`, id, routeKey, testSignature); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AcquireRouteLease(ctx, routeKey, "failure-lifecycle-writer", time.Minute); err != nil {
			t.Fatal(err)
		}
		return routeKey, id
	}
	// A wire whose signature is testSignature, as an older binary left a
	// submitted row.
	signature := solana.Signature{5}
	submittedWire := append(append([]byte{1}, signature[:]...), 7)
	submittedOperation := func(id, routeKey string) PersistedOperation {
		return PersistedOperation{
			Operation: Operation{ID: id, RouteKey: routeKey, Decision: Decision{Action: ReportNAV}}, Status: Submitted,
			SignedWire: submittedWire, SignedWireSHA256: sha256Bytes(submittedWire), TransactionSignature: testSignature,
			RecentBlockhash: bridgeVault, LastValidBlockHeight: 10,
		}
	}
	// statusValue is the getSignatureStatuses row; transactionValue is the
	// getTransaction result, or "RPC_ERROR" for a pruned/lagging receipt.
	advance := func(t *testing.T, landCtx context.Context, op PersistedOperation, statusValue, transactionValue string) (string, string, int) {
		t.Helper()
		receiptReads := 0
		rpc := newFakeChain(t, nil)
		rpcOf(rpc).Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			switch {
			case strings.Contains(string(body), `"method":"getEpochInfo"`):
				return response(finalizedEpochJSON(5)), nil
			case strings.Contains(string(body), `"method":"getSignatureStatuses"`):
				return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":600},"value":[` + statusValue + `]}}`), nil
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
		// An unreadable receipt ends the tick with an unavailable error; the
		// row is the outcome.
		_ = AdvanceNonterminal(landCtx, db, rpc, nil, op)
		var status, reason string
		if err := db.pool.QueryRow(ctx, `SELECT status,COALESCE(recovery_reason,'') FROM loyal_yield.multiply_operations WHERE operation_id=$1`, op.ID).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		return status, reason, receiptReads
	}
	finalizedFailure := `{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"finalized"}`

	t.Run("a processed-only failure keeps observing without a resend", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "processed")
		op := submittedOperation(id, routeKey)
		waiting, stop := context.WithTimeout(ctx, 1500*time.Millisecond)
		defer stop()
		status, reason, receiptReads := advance(t, waiting, op,
			`{"slot":45,"err":{"InstructionError":[0,{"Custom":9}]},"confirmationStatus":"processed"}`,
			`{"slot":45,"meta":{"err":null,"logMessages":[]}}`)
		if status != "submitted" || reason != "" || receiptReads != 0 {
			t.Fatalf("a forked-away failure transitioned the journal: status=%s reason=%q receiptReads=%d", status, reason, receiptReads)
		}
	})

	t.Run("an unreadable or contradictory receipt keeps the submission state", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "unreadable")
		op := submittedOperation(id, routeKey)
		for _, receipt := range []string{`{"slot":45,"meta":{"err":null,"logMessages":[]}}`, "RPC_ERROR"} {
			if status, reason, _ := advance(t, ctx, op, finalizedFailure, receipt); status != "submitted" || reason != "" {
				t.Fatalf("receipt %s left the submission state: %s %q", receipt, status, reason)
			}
		}
	})

	t.Run("finalized adaptor logs still require exact wire and fee evidence", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "adaptor")
		op := submittedOperation(id, routeKey)
		receipt := `{"slot":500,"meta":{"err":{"InstructionError":[0,{"Custom":9}]},"logMessages":` +
			mustJSONLogs(t, adaptorFailureLogs(bridgeAdaptorProgram, 9)) + `}}`
		status, reason, _ := advance(t, ctx, op, finalizedFailure, receipt)
		if status != "submitted" || reason != "" {
			t.Fatalf("logs-only refusal settled without fee and wire proof: %s %q", status, reason)
		}
	})

	t.Run("unattributable and non-adaptor errors stay capital stops", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "unattributable")
		op := submittedOperation(id, routeKey)
		status, reason, _ := advance(t, ctx, op, finalizedFailure,
			transactionResult(t, 500, nil, map[string]any{"err": map[string]any{"InstructionError": []any{0, map[string]any{"Custom": 9}}}, "logMessages": []string{}}))
		if status != "manual_recovery" || reason != unclassifiedTransactionErrReason {
			t.Fatalf("truncated failure logs lost the capital stop: %s %q", status, reason)
		}
		otherRoute, otherID := newSubmittedOperation(t, "voltr")
		other := submittedOperation(otherID, otherRoute)
		otherReceipt := transactionResult(t, 500, nil, map[string]any{"err": map[string]any{"InstructionError": []any{0, map[string]any{"Custom": 6004}}},
			"logMessages": adaptorFailureLogs(voltr.ProgramID.String(), 6004)})
		status, reason, _ = advance(t, ctx, other, finalizedFailure, otherReceipt)
		if status != "manual_recovery" || reason != unclassifiedTransactionErrReason {
			t.Fatalf("a non-adaptor error lost the capital stop: %s %q", status, reason)
		}
	})

	t.Run("a settled success is never classified", func(t *testing.T) {
		routeKey, id := newSubmittedOperation(t, "success")
		op := submittedOperation(id, routeKey)
		status, reason, receiptReads := advance(t, ctx, op, `{"slot":45,"err":null,"confirmationStatus":"confirmed"}`, "RPC_ERROR")
		if status != "confirmed" || reason != "" || receiptReads != 0 {
			t.Fatalf("a successful receipt was classified as a failure: status=%s reason=%q receiptReads=%d", status, reason, receiptReads)
		}
	})

	// The landing path on the real journal: a signed row records broadcast
	// intent before its first send, a resumed row resends the same bytes, every
	// send keeps preflight, and only the signature status and the finalized
	// height decide the row.
	for _, tc := range []struct {
		name, from, want, reason string
		// landAfter is the send count after which the signature confirms;
		// zero never lands and the blockhash expires after the first send.
		landAfter int
		// refused: preflight refuses every send, so the cluster never
		// forwards the wire.
		refused bool
		sends   int
	}{
		{"a signed wire is resent until it lands and confirms at its slot", "signed", "confirmed", "", 2, false, 2},
		{"a resumed wire resends the same bytes and fails once expired and absent", "broadcast_intent", "failed", "signature_absent_after_blockhash_expiry", 0, false, 1},
		{"a wire refused by preflight is never forced on chain and fails once expired", "signed", "failed", "signature_absent_after_blockhash_expiry", 0, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routeKey := fmt.Sprintf("failure-lifecycle-land-%d", time.Now().UnixNano())
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
			id := seedBoundOperation(t, ctx, db, routeKey, "failure-lifecycle-writer", ReportNAV, "OnRe/ONyc/USDC", request, effects)
			message, err := CompileBridgeMessage(request)
			if err != nil {
				t.Fatal(err)
			}
			wire := append(append([]byte{1}, bytes.Repeat([]byte{3}, 64)...), message...)
			bindTestWire(t, ctx, db, id, sha256Bytes(wire))
			evidence := `{"decision":{"observationSlot":42}}`
			if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status=$2,signed_wire=$3,expected_effects=expected_effects||$4::jsonb,
				broadcast_intent_at=CASE WHEN $2='signed' THEN NULL ELSE clock_timestamp() END WHERE operation_id=$1`, id, tc.from, wire, evidence); err != nil {
				t.Fatal(err)
			}
			operation := PersistedOperation{
				Operation: Operation{ID: id, RouteKey: routeKey, Decision: Decision{Action: ReportNAV}}, Status: OperationStatus(tc.from),
				ExpectedEffects: []byte(evidence), SignedWire: wire, SignedWireSHA256: sha256Bytes(wire),
				TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: request.RecentBlockhash, LastValidBlockHeight: 10,
			}
			sends := 0
			rpc := newFakeChain(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				switch {
				case strings.Contains(string(body), `"method":"sendTransaction"`):
					if !strings.Contains(string(body), base64.StdEncoding.EncodeToString(wire)) {
						t.Errorf("sent bytes other than the persisted wire")
					}
					if strings.Contains(string(body), `"skipPreflight":true`) {
						t.Errorf("a send skipped preflight")
					}
					sends++
					if tc.refused {
						return response(`{"jsonrpc":"2.0","id":1,"error":{"code":-32002,"message":"Transaction simulation failed: custom program error: 0x9"}}`), nil
					}
					return response(`{"jsonrpc":"2.0","id":1,"result":"` + operation.TransactionSignature + `"}`), nil
				case strings.Contains(string(body), `"method":"getEpochInfo"`):
					if tc.landAfter == 0 && sends > 0 {
						return response(finalizedEpochJSON(11)), nil
					}
					return response(finalizedEpochJSON(5)), nil
				case strings.Contains(string(body), `"method":"getSignatureStatuses"`):
					status := "null"
					if tc.landAfter > 0 && sends >= tc.landAfter {
						status = `{"slot":45,"confirmations":1,"err":null,"confirmationStatus":"confirmed"}`
					}
					return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":5000},"value":[` + status + `]}}`), nil
				}
				t.Errorf("unexpected RPC while landing: %s", body)
				return nil, fmt.Errorf("unexpected RPC")
			}))
			if err := AdvanceNonterminal(ctx, db, rpc, nil, operation); err != nil {
				t.Fatal(err)
			}
			var status, reason string
			var slot int64
			var intent bool
			if err := db.pool.QueryRow(ctx, `SELECT status,COALESCE(recovery_reason,''),COALESCE(confirmed_slot,0),broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status, &reason, &slot, &intent); err != nil {
				t.Fatal(err)
			}
			if wantSlot := int64(45 * min(tc.landAfter, 1)); status != tc.want || reason != tc.reason || slot != wantSlot || !intent || sends != tc.sends {
				t.Fatalf("landing outcome: status=%s reason=%q slot=%d intent=%t sends=%d", status, reason, slot, intent, sends)
			}
		})
	}
}
