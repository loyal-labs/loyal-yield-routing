package backyardrwa

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPolicySetupSigningNeverFallsBackToDelegate(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	t.Setenv(policyKeypairEnvironment, hex.EncodeToString(key))
	t.Setenv(setupKeypairEnvironment, "")
	if _, err := loadPinnedPolicySetupSigner(); err == nil || err.Error() != "SOLANA_TESTING_PK is not configured" {
		t.Fatalf("setup loader fell back to another credential: %v", err)
	}
	for _, material := range []string{"private-material-do-not-echo", hex.EncodeToString(key)} {
		t.Setenv(setupKeypairEnvironment, material)
		if _, err := loadPinnedPolicySetupSigner(); err == nil || strings.Contains(err.Error(), material) {
			t.Fatalf("invalid admin accepted or secret exposed: %v", err)
		}
	}
	// Exercise the shared loader's positive decoding/pin branch with a local key;
	// the production setup wrapper above always supplies the fixed real admin.
	loaded, err := loadPinnedSigner(setupKeypairEnvironment, publicKeyFromBytes(key.Public().(ed25519.PublicKey)), "local test")
	if err != nil || !bytes.Equal(loaded, key) {
		t.Fatal("valid local signer decode/pin failed")
	}
	auth := setupPaymentAuth(t, "direct")
	input, err := policySetupPaymentInput(auth, PolicySetupCreate)
	if err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, func() error { _, err := signPolicySetupPayment(input, key); return err }(), "setup_signer_mismatch")
	forged := append(ed25519.PrivateKey(nil), key...)
	admin := mustKey(bridgeSettingsSigner)
	copy(forged[32:], admin[:])
	assertBudgetHold(t, func() error { _, err := signPolicySetupPayment(input, forged); return err }(), "setup_signer_mismatch")
	_, err = signPolicySetupPayment(input, nil)
	assertBudgetHold(t, err, "setup_signer_mismatch")
}

// Controlled journal witness, not a real-admin signing/simulation witness. The
// production wrapper must reject this synthetic wire before any mutation. The
// private post-signature journal half is then exercised independently to prove
// atomic wire/simulation/reservation persistence and lease rollback.
func testPolicySetupSignedPersistence(t *testing.T, ctx context.Context, db *Database, newRoute func(*testing.T, Phase3Budget, time.Duration) string, plan policySetupObservation) {
	t.Helper()
	for _, drift := range []string{"", "unpriced", "simulation old", "simulation expired", "slot expired", "wire exists", "lease expired"} {
		t.Run(drift, func(t *testing.T) {
			key := newRoute(t, emptyTestBudget(), time.Minute)
			r, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, plan)
			if err != nil {
				t.Fatal(err)
			}
			var auth phase3OperationAuthorization
			var raw []byte
			if err = db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, r.OperationID).Scan(&raw); err != nil || json.Unmarshal(raw, &auth) != nil {
				t.Fatal("missing setup authorization", err)
			}
			rpc := setupPaymentRPC(t, auth, "")
			payment, err := db.authorizePolicySetupPayment(ctx, rpc, r.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			op := setupPrefundOperation(t, plan)
			build := BuildResult{MessageSHA256: sha256Bytes(payment.Message), SignedWire: op.SignedWire,
				SignedWireSHA256: op.SignedWireSHA256, TransactionSignature: op.TransactionSignature,
				RecentBlockhash: op.RecentBlockhash, LastValidBlockHeight: op.LastValidBlockHeight}
			simulation := SimulationResult{Slot: 42, UnitsConsumed: 150, Logs: []string{"controlled simulation fixture"}}
			assertBudgetHold(t, db.persistPolicySetupPreparation(ctx, rpc, r.OperationID, build, nil), "setup_signature_invalid")
			// Storage-only admission of synthetic bytes. The public-key check
			// above is NOT bypassable from the production preparation method.
			first, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Rollback(ctx)
			initialBudget, initialAuth, err := db.readPhase3BudgetTx(ctx, first, r.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.persistValidatedPolicySetupSignedTx(ctx, first, rpc, r.OperationID, initialBudget, initialAuth, build, nil); err != nil {
				t.Fatal(err)
			}
			if err = first.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = db.ReleaseRouteLease(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = db.AcquireRouteLease(ctx, key, "setup-simulation-restart", time.Minute); err != nil {
				t.Fatal(err)
			}
			built, err := db.LoadNonterminal(ctx, key)
			if err != nil || built == nil || built.Status != Built || !bytes.Equal(built.SignedWire, build.SignedWire) || built.BroadcastIntentRecorded {
				t.Fatal("pre-simulation crash lost signed-unsent identity", err)
			}
			assertBudgetHold(t, AdvanceNonterminal(ctx, db, rpc, *built), "policy_setup_execution_not_enabled")
			if drift == "" {
				testPolicySetupPreSimulationMigration(t, ctx, db, r.OperationID)
			}
			if err = db.cancelUnsentPolicySetupIntent(ctx, r.OperationID); err == nil {
				t.Fatal("pre-simulation signed wire canceled as unsigned")
			}
			build.SimulationSlot = simulation.Slot
			assertBudgetHold(t, db.persistPolicySetupPreparation(ctx, rpc, r.OperationID, build, &simulation), "setup_signature_invalid")
			var before string
			if err = db.pool.QueryRow(ctx, `SELECT (state-'generation')::text FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if drift == "wire exists" {
				if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET signed_wire_sha256=$2 WHERE operation_id=$1`, r.OperationID, strings.Repeat("a", 64)); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			budget, current, err := db.readPhase3BudgetTx(ctx, tx, r.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.validatePolicySetupReservationTx(ctx, tx, r.OperationID, budget, current, Built); err != nil {
				t.Fatal(err)
			}
			switch drift {
			case "unpriced":
				current.SetupBuildCost = nil
			case "simulation old":
				build.SimulationSlot, simulation.Slot = 1, 1
			case "simulation expired":
				build.SimulationSlot, simulation.Slot = 100, 100
			case "slot expired":
				rpc, _ = NewRPCClient("https://rpc.invalid")
				rpc.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					var body struct{ Method string }
					if json.NewDecoder(r.Body).Decode(&body) != nil || body.Method != "getSlot" {
						t.Fatal("signed persistence attempted unexpected RPC")
					}
					return response(`{"jsonrpc":"2.0","id":1,"result":100}`), nil
				})
			case "lease expired":
				if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE route_key=$1`, key); err != nil {
					t.Fatal(err)
				}
			}
			err = db.persistValidatedPolicySetupSignedTx(ctx, tx, rpc, r.OperationID, budget, current, build, &simulation)
			if drift != "" {
				if err == nil {
					t.Fatal("unsafe signed persistence accepted")
				}
				_ = tx.Rollback(ctx)
			} else if err != nil {
				t.Fatal(err)
			} else if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var after string
			if err = db.pool.QueryRow(ctx, `SELECT (state-'generation')::text FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&after); err != nil || before != after {
				t.Fatal("preparing a wire changed budget or setup root", err)
			}
			pending, err := db.LoadNonterminal(ctx, key)
			if err != nil || pending == nil {
				t.Fatal("lost setup operation", err)
			}
			if drift != "" {
				if pending.Status != Built || !bytes.Equal(pending.SignedWire, build.SignedWire) {
					t.Fatal("failed simulation persistence lost the pre-simulation wire")
				}
				return
			}
			if pending.Status != Signed || !bytes.Equal(pending.SignedWire, build.SignedWire) || pending.SignedWireSHA256 != build.SignedWireSHA256 || pending.BroadcastIntentRecorded {
				t.Fatal("exact signed-unsent wire not retained")
			}
			var retained SimulationResult
			if err = db.pool.QueryRow(ctx, `SELECT simulation_result FROM loyal_yield.multiply_operations WHERE operation_id=$1`, r.OperationID).Scan(&raw); err != nil || json.Unmarshal(raw, &retained) != nil || retained.Slot != 42 || retained.UnitsConsumed != 150 {
				t.Fatal("wire was not committed with its simulation", err)
			}
			assertBudgetHold(t, AdvanceNonterminal(ctx, db, rpc, *pending), "policy_setup_execution_not_enabled")
			if err = db.cancelUnsentPolicySetupIntent(ctx, r.OperationID); err == nil {
				t.Fatal("signed setup canceled as unsigned")
			}
		})
	}
}

func testPolicySetupPreSimulationMigration(t *testing.T, ctx context.Context, db *Database, id string) {
	t.Helper()
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `CREATE TEMP TABLE setup_pre_simulation (LIKE loyal_yield.multiply_operations) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"0074_backyard_rwa_phase3_journal_actions.sql", "0075_backyard_rwa_setup_pre_simulation_wire.sql"} {
		sql, err := os.ReadFile("../../../../crates/loyal-yield-store/migrations/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, strings.ReplaceAll(string(sql), "loyal_yield.multiply_operations", "pg_temp.setup_pre_simulation")); err != nil {
			t.Fatal(err)
		}
	}
	// This is the actual Built row produced by the journal method, not a
	// hand-written approximation of its fields or the migration expression.
	if _, err = tx.Exec(ctx, `INSERT INTO pg_temp.setup_pre_simulation SELECT * FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id); err != nil {
		t.Fatal("prepared setup row is incompatible with production migrations", err)
	}
	for _, tc := range []struct {
		set  string
		pass bool
	}{
		{"action='REPORT_NAV'", false},
		{"action=NULL", false},
		{"strategy_key='Ethena/USDe/PYUSD'", false},
		{"expected_effects='{}'::jsonb", false},
		{"signed_wire_sha256=NULL", false},
		{"transaction_signature=NULL", false},
		{"last_valid_block_height=NULL", false},
		{"broadcast_intent_at=now()", false},
		{"simulation_slot=42", false},
		{"status='signed'", false},
		{"status='signed',simulation_slot=42,simulation_result='{}'::jsonb", true},
		{"action='REPORT_NAV',signed_wire=NULL,transaction_signature=NULL", true},
	} {
		nested, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = nested.Exec(ctx, "UPDATE pg_temp.setup_pre_simulation SET "+tc.set)
		if (err == nil) != tc.pass {
			t.Fatalf("migration boundary %s: %v", tc.set, err)
		}
		_ = nested.Rollback(ctx)
	}
}
