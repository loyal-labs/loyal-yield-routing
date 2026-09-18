package backyardrwa

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func setupExpiryRPC(t *testing.T, auth phase3OperationAuthorization, op PersistedOperation, drift string) *RPCClient {
	t.Helper()
	rpc := setupPaymentRPC(t, auth, drift)
	base := rpc.client.Transport
	expired := false
	rpc.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Method string
			Params []json.RawMessage
		}
		if json.Unmarshal(raw, &body) != nil {
			t.Fatal("invalid expiry RPC")
		}
		var result any
		switch body.Method {
		case "getBlockHeight":
			var config map[string]string
			if json.Unmarshal(body.Params[0], &config) != nil || config["commitment"] != "finalized" {
				t.Fatal("expiry used non-finalized height")
			}
			result = op.LastValidBlockHeight + 1
			expired = true
			if drift == "not expired" {
				result, expired = op.LastValidBlockHeight, false
			}
		case "getSignatureStatuses":
			var signatures []string
			var config map[string]bool
			if !expired || json.Unmarshal(body.Params[0], &signatures) != nil || len(signatures) != 1 || signatures[0] != op.TransactionSignature || json.Unmarshal(body.Params[1], &config) != nil || !config["searchTransactionHistory"] {
				t.Fatal("absence did not follow finalized expiry with exact historical signature lookup")
			}
			result = map[string]any{"value": []any{nil}}
			if drift == "transport" {
				return nil, context.DeadlineExceeded
			}
			if drift == "found" || drift == "failed" {
				value := map[string]any{"slot": 42, "confirmationStatus": "processed", "err": nil}
				if drift == "failed" {
					value["err"] = "controlled failure"
				}
				result = map[string]any{"value": []any{value}}
			}
			if drift == "malformed" {
				result = map[string]any{"value": []any{}}
			}
		case "getMultipleAccounts":
			var config map[string]any
			if !expired || json.Unmarshal(body.Params[1], &config) != nil || config["commitment"] != "finalized" {
				t.Fatal("unspent setup prestate was not finalized after expiry")
			}
			return base.RoundTrip(r)
		case "getGenesisHash":
			return base.RoundTrip(r)
		case "getTransaction":
			// Missing finalized receipt is not proof of absence; the worker
			// must still use the separately signature-checked expiry boundary.
			result = nil
		default:
			t.Fatalf("expiry attempted unnecessary or mutating RPC %s", body.Method)
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		return response(string(encoded)), nil
	})
	return rpc
}

// Real PostgreSQL and controlled RPC proof after the signature gate, which
// rejects these deliberately synthetic wires. This never claims admin-key or
// mainnet proof. prepare supplies an existing Decided initial/funded intent.
func testPolicySetupExpiredWire(t *testing.T, ctx context.Context, db *Database, prepare func(*testing.T) PersistedOperation) {
	t.Helper()
	for _, drift := range []string{"", "not expired", "found", "failed", "malformed", "transport", "genesis", "settings", "target", "lease", "submitted", "broadcast intent", "built"} {
		name := drift
		if name == "" {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			pending := prepare(t)
			var raw []byte
			var auth phase3OperationAuthorization
			if err := db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, pending.ID).Scan(&raw); err != nil || json.Unmarshal(raw, &auth) != nil {
				t.Fatal("missing expiry intent", err)
			}
			input, err := policySetupPaymentInput(auth, pending.Decision.Action)
			if err != nil {
				t.Fatal(err)
			}
			wire := append([]byte{1}, bytes.Repeat([]byte{7}, 64)...)
			wire = append(wire, input.Message...)
			auth.SignedWireSHA256, auth.SetupBuildCost = sha256Bytes(wire), &input.Cost
			auth.SetupCompletionCost = input.CompletionCost
			status := Signed
			if drift == "built" {
				status = Built
			}
			if drift == "submitted" || drift == "broadcast intent" {
				status = Submitted
				if drift == "broadcast intent" {
					status = BroadcastIntent
				}
				auth.SendKnownCost = &input.Cost
			}
			encoded, _ := json.Marshal(auth)
			_, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status=$2,
			 message_sha256=$3,signed_wire=$4,signed_wire_sha256=$5,transaction_signature=$6,
			 recent_blockhash=$7,last_valid_block_height=$8,
			 simulation_slot=CASE WHEN $2='built' THEN NULL ELSE 42 END,
			 simulation_result=CASE WHEN $2='built' THEN NULL ELSE '{"Slot":42}'::jsonb END,
			 broadcast_intent_at=CASE WHEN $2 IN ('submitted','broadcast_intent') THEN clock_timestamp() ELSE NULL END,
			 expected_effects=jsonb_set(expected_effects,'{phase3}',$9::jsonb) WHERE operation_id=$1`,
				pending.ID, status, sha256Bytes(input.Message), wire, auth.SignedWireSHA256, encodeBase58(wire[1:65]), input.Request.RecentBlockhash, input.Request.LastValidBlockHeight, string(encoded))
			if err != nil {
				t.Fatal(err)
			}
			snapshot := func() string {
				var value string
				if err := db.pool.QueryRow(ctx, `SELECT jsonb_build_object('state',r.state,'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id) FROM loyal_yield.multiply_operations o WHERE o.route_key=r.route_key))::text FROM loyal_yield.multiply_route_states r WHERE route_key=$1`, pending.RouteKey).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := snapshot()
			var parentBefore string
			if auth.PolicySetupCompletion != nil {
				if err = db.pool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM loyal_yield.multiply_operations o WHERE operation_id=$1`, auth.PolicySetupCompletion.PrefundOperationID).Scan(&parentBefore); err != nil {
					t.Fatal(err)
				}
			}
			// No positive shortcut exists in the real entrypoint.
			assertBudgetHold(t, db.recoverExpiredPolicySetup(ctx, setupPaymentRPC(t, auth, ""), pending.ID, sha256Bytes([]byte("different wire"))), "setup_expiry_wire_or_status_changed")
			assertBudgetHold(t, db.recoverExpiredPolicySetup(ctx, setupPaymentRPC(t, auth, ""), pending.ID, auth.SignedWireSHA256), "setup_signature_invalid")
			if snapshot() != before {
				t.Fatal("rejected signature mutated journal")
			}
			if drift == "submitted" || drift == "broadcast intent" {
				operation, err := db.LoadNonterminal(ctx, pending.RouteKey)
				if err != nil || operation == nil {
					t.Fatal(err)
				}
				if err = AdvanceNonterminal(ctx, db, setupExpiryRPC(t, auth, *operation, ""), *operation); err == nil || snapshot() != before {
					t.Fatal("receipt absence bypassed signature validation in worker expiry recovery", err)
				}
			}
			tx, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			budget, current, err := db.readPhase3BudgetTx(ctx, tx, pending.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.validatePolicySetupReservationTx(ctx, tx, pending.ID, budget, current, status); err != nil {
				t.Fatal(err)
			}
			signed, journal, _, err := loadPolicySetupExpiryTx(ctx, tx, pending.ID)
			if err != nil {
				t.Fatal(err)
			}
			if drift == "lease" {
				if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE route_key=$1`, pending.RouteKey); err != nil {
					t.Fatal(err)
				}
			}
			err = db.recoverVerifiedExpiredPolicySetupTx(ctx, tx, setupExpiryRPC(t, auth, signed, drift), budget, current, signed, journal)
			pass := drift == "" || drift == "submitted" || drift == "broadcast intent" || drift == "built"
			if !pass {
				if err == nil {
					t.Fatal("unproven expiry accepted")
				}
				_ = tx.Rollback(ctx)
				if snapshot() != before {
					t.Fatal("failed expiry changed intent, signed evidence, prefund or budget")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			duplicate := make(chan error, 1)
			if drift == "" {
				// This contender waits on the same locked route and must observe
				// the committed retirement, not retire/reserve a second time.
				go func() {
					duplicate <- db.recoverExpiredPolicySetup(ctx, setupPaymentRPC(t, auth, ""), pending.ID, auth.SignedWireSHA256)
				}()
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if drift == "" {
				if err = <-duplicate; err != nil {
					t.Fatal("concurrent expiry replay failed", err)
				}
			}
			var saved struct {
				Auth    phase3OperationAuthorization `json:"phase3"`
				History []struct {
					Operation PersistedOperation `json:"operation"`
					Journal   json.RawMessage    `json:"journal"`
				} `json:"setupExpiredWires"`
			}
			if err = db.pool.QueryRow(ctx, `SELECT expected_effects FROM loyal_yield.multiply_operations WHERE operation_id=$1`, pending.ID).Scan(&raw); err != nil || json.Unmarshal(raw, &saved) != nil {
				t.Fatal(err)
			}
			if len(saved.History) != 1 || !bytes.Equal(saved.History[0].Operation.SignedWire, wire) || saved.History[0].Operation.SignedWireSHA256 != auth.SignedWireSHA256 || !reflect.DeepEqual(saved.Auth.PolicySetup, auth.PolicySetup) || !reflect.DeepEqual(saved.Auth.PolicySetupCompletion, auth.PolicySetupCompletion) || saved.Auth.SignedWireSHA256 != "" || saved.Auth.SetupBuildCost != nil || saved.Auth.SendKnownCost != nil {
				t.Fatal("expiry lost wire/intent or retained stale signing authority")
			}
			var previousJournal, retainedJournal any
			_ = json.Unmarshal(journal, &previousJournal)
			_ = json.Unmarshal(saved.History[0].Journal, &retainedJournal)
			if !reflect.DeepEqual(previousJournal, retainedJournal) {
				t.Fatal("expiry changed original simulation/submission metadata")
			}
			if auth.PolicySetupCompletion != nil {
				var parentAfter string
				if err = db.pool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM loyal_yield.multiply_operations o WHERE operation_id=$1`, auth.PolicySetupCompletion.PrefundOperationID).Scan(&parentAfter); err != nil || parentBefore != parentAfter {
					t.Fatal("creation expiry changed finalized prefunding", err)
				}
			}
			var actual Phase3Budget
			if err = db.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, pending.RouteKey).Scan(&raw); err != nil || json.Unmarshal(raw, &actual) != nil || !reflect.DeepEqual(actual, budget) {
				t.Fatal("expiry released or replenished budget", err)
			}
			recovered, err := db.LoadNonterminal(ctx, pending.RouteKey)
			if err != nil || recovered == nil || recovered.Status != Decided || recovered.ID != pending.ID || len(recovered.SignedWire) != 0 || recovered.BroadcastIntentRecorded {
				t.Fatal("expiry did not preserve one reserved unsigned intent", err)
			}
			if _, err = db.ReleaseRouteLease(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = db.AcquireRouteLease(ctx, pending.RouteKey, "setup-expiry-restart", time.Minute); err != nil {
				t.Fatal(err)
			}
			retryBefore := snapshot()
			hostile, _ := NewRPCClient("https://rpc.invalid")
			hostile.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("expiry replay called RPC"); return nil, nil })
			if err = db.recoverExpiredPolicySetup(ctx, hostile, pending.ID, auth.SignedWireSHA256); err != nil || snapshot() != retryBefore {
				t.Fatal("restart replay duplicated expiry or changed funding", err)
			}
			if drift == "" {
				// Prove that expiry recovery connects to the existing priced
				// refresh, rather than leaving a terminal-looking dead end.
				var resumed DecisionRecord
				if auth.PolicySetupCompletion == nil {
					fresh, err := observePolicySetup(ctx, setupObservationRPC(t, &setupRPCScenario{rent: auth.PolicySetup.RentLamports, blockhash: bridgeDelegate}), auth.PolicySetup.Request.Operation)
					if err != nil {
						t.Fatal(err)
					}
					resumed, err = db.refreshUnsentPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), pending.ID, fresh)
					if err != nil || resumed.OperationID == pending.ID {
						t.Fatal("expired initial setup could not refresh", err)
					}
				} else {
					parent := setupPrefundOperation(t, *auth.PolicySetup)
					parent.ID = auth.PolicySetupCompletion.PrefundOperationID
					if parent.SignedWireSHA256 != auth.PolicySetupCompletion.Prefund.WireSHA256 {
						t.Fatal("refresh fixture no longer matches finalized prefund")
					}
					resumed, err = db.refreshPolicySetupCompletion(ctx, setupCompletionRPC(t, *auth.PolicySetup, parent, "refresh"), pending.ID, auth.IntentSHA256)
					if err != nil || resumed.OperationID != pending.ID {
						t.Fatal("expired creation lost its reserved continuation", err)
					}
				}
				if pending.Decision.Action == PolicySetupCreate {
					t.Run("refreshed creation settles", func(t *testing.T) {
						var reserved int64
						if err := db.pool.QueryRow(ctx, `SELECT (state->'phase3'->'reservations'->$2->>'upperMicros')::bigint FROM loyal_yield.multiply_route_states WHERE route_key=$1`, pending.RouteKey, resumed.OperationID).Scan(&reserved); err != nil {
							t.Fatal(err)
						}
						testPolicySetupCreationSettlement(t, ctx, db, resumed.OperationID, pending.RouteKey, budget.Families["OnRe"].SpentMicros+reserved)
					})
				}
			}
		})
	}
}
