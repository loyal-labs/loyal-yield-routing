package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func assertBudgetHold(t *testing.T, err error, reason string) {
	t.Helper()
	var hold *BudgetHold
	if !errors.As(err, &hold) || hold.Reason != reason {
		t.Fatalf("wanted HOLD %s; got %v", reason, err)
	}
}

// signedBindFixture is a bound ReportNAV with its unsigned local wire; neither
// signer proof nor submission is claimed.
func signedBindFixture(t *testing.T) (phase3OperationAuthorization, PersistedOperation) {
	t.Helper()
	request := bridgeTestRequest(ReportNAV, 0)
	effects, _, _, err := bridgeExpectedEffects(Decision{Action: ReportNAV}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	input, err := encodePhase3BuildInput(request, encoded)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := Phase3IntentDigest(request, encoded)
	if err != nil {
		t.Fatal(err)
	}
	message, err := CompileBridgeMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	operation := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: request.RecentBlockhash, LastValidBlockHeight: request.LastValidBlockHeight}
	return phase3OperationAuthorization{IntentSHA256: digest, SignedWireSHA256: operation.SignedWireSHA256, BuildInput: input}, operation
}

func TestSignedIdentityProvesTheBoundWireBeforeAnyRPC(t *testing.T) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	auth, operation := signedBindFixture(t)
	// A JSON round trip models JSONB storage; key ordering cannot change the
	// exact effects digest.
	encoded, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &auth); err != nil {
		t.Fatal(err)
	}
	if _, _, err = manifest.validateSignedIdentity(auth, operation); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*phase3OperationAuthorization, *PersistedOperation){
		"missing build input": func(a *phase3OperationAuthorization, _ *PersistedOperation) { a.BuildInput = nil },
		"unknown kind":        func(a *phase3OperationAuthorization, _ *PersistedOperation) { a.BuildInput.Kind = "policy-setup" },
		"trailing request bytes": func(a *phase3OperationAuthorization, _ *PersistedOperation) {
			a.BuildInput.Request = append(a.BuildInput.Request, []byte(` {"extra":1}`)...)
		},
		"other intent":    func(a *phase3OperationAuthorization, _ *PersistedOperation) { a.IntentSHA256 = strings.Repeat("a", 64) },
		"unbound wire":    func(a *phase3OperationAuthorization, _ *PersistedOperation) { a.SignedWireSHA256 = "" },
		"other expiry":    func(_ *phase3OperationAuthorization, o *PersistedOperation) { o.LastValidBlockHeight-- },
		"other signature": func(_ *phase3OperationAuthorization, o *PersistedOperation) { o.TransactionSignature = "changed" },
		"other message bytes": func(a *phase3OperationAuthorization, o *PersistedOperation) {
			o.SignedWire[len(o.SignedWire)-1] ^= 1
			o.SignedWireSHA256 = sha256Bytes(o.SignedWire)
			a.SignedWireSHA256 = o.SignedWireSHA256
		},
	} {
		t.Run(name, func(t *testing.T) {
			auth, operation := signedBindFixture(t)
			mutate(&auth, &operation)
			_, _, err := manifest.validateSignedIdentity(auth, operation)
			var hold *BudgetHold
			if !errors.As(err, &hold) {
				t.Fatalf("untrusted persisted identity passed: %v", err)
			}
		})
	}
}

// seedBoundOperation records, on a fresh route the database leases, a decided
// operation already bound to request and effects as bindOperation leaves it.
// The row's expected effects are the given effects bytes.
func seedBoundOperation(t *testing.T, ctx context.Context, db *Database, key, owner string, action Action, lane string, request any, effects []byte) string {
	t.Helper()
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":1}',1)`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, owner, time.Minute); err != nil {
		t.Fatal(err)
	}
	digest, err := Phase3IntentDigest(request, effects)
	if err != nil {
		t.Fatal(err)
	}
	input, err := encodePhase3BuildInput(request, effects)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := json.Marshal(phase3OperationAuthorization{IntentSHA256: digest, BuildInput: input})
	if err != nil {
		t.Fatal(err)
	}
	id := key + "-op"
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
	 VALUES($1,$2,'decided',$3,$4,$5::jsonb || jsonb_build_object('phase3',$6::jsonb))`, id, key, string(action), lane, string(effects), string(bound)); err != nil {
		t.Fatal(err)
	}
	return id
}

// markBroadcastIntent runs the final-send identity check and its locked
// fence, which records broadcast intent.
func markBroadcastIntent(ctx context.Context, db *Database, m RouteManifest, operation PersistedOperation) error {
	mark, err := db.finalSend(ctx, m, operation)
	if err != nil {
		return err
	}
	return mark(ctx)
}

// bindTestWire binds a signed wire hash exactly as PersistSigned does.
func bindTestWire(t *testing.T, ctx context.Context, db *Database, id, hash string) {
	t.Helper()
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = bindSignedWireTx(ctx, tx, id, hash); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// operationStatus reads one operation's durable status.
func operationStatus(t *testing.T, ctx context.Context, db *Database, id string) string {
	t.Helper()
	var status string
	if err := db.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// openBindTestDatabase opens the disposable PostgreSQL the bind and send
// fences run against; only a Unix-socket database under /private/tmp enables it.
func openBindTestDatabase(t *testing.T) (*Database, string) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
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
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS signed_wire_sha256 text,
	 ADD COLUMN IF NOT EXISTS recovery_reason text,ADD COLUMN IF NOT EXISTS action text,
	 ADD COLUMN IF NOT EXISTS strategy_key text,ADD COLUMN IF NOT EXISTS transaction_signature text,
	 ADD COLUMN IF NOT EXISTS message_sha256 text,ADD COLUMN IF NOT EXISTS recent_blockhash text,
	 ADD COLUMN IF NOT EXISTS last_valid_block_height bigint,
	 ADD COLUMN IF NOT EXISTS confirmed_slot bigint,ADD COLUMN IF NOT EXISTS confirmation_status text,
	 ADD COLUMN IF NOT EXISTS reconciliation_sha256 text,ADD COLUMN IF NOT EXISTS reconciled_effects jsonb;
	CREATE UNIQUE INDEX IF NOT EXISTS multiply_operations_one_nonterminal_per_route
	 ON loyal_yield.multiply_operations(route_key) WHERE status IN ('decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling');`)
	if err != nil {
		t.Fatal(err)
	}
	return db, url
}

// The bind and final-send fences against real PostgreSQL: a decided row binds
// only its own decision, a builder signs only the bound intent, a signed wire
// is sent only once its hash is bound and only under a live lease, and a
// pre-send hold fails only a never-submitted row.
func TestBindAndFinalSendFenceAgainstDatabase(t *testing.T) {
	db, url := openBindTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	const lane = "OnRe/ONyc/USDC"
	key := fmt.Sprintf("bind-test-%d", time.Now().UnixNano())
	op := key + "-op"
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, _ = db.pool.Exec(cleanup, `DELETE FROM loyal_yield.multiply_operations WHERE route_key=$1`, key)
		_, _ = db.pool.Exec(cleanup, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key)
	})
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, key); err != nil {
		t.Fatal(err)
	}
	decision := Decision{Action: ReportNAV, StrategyKey: lane, Reason: "nav_due"}
	evidence, err := json.Marshal(map[string]any{"decision": decisionEvidence{Reason: decision.Reason, ObservationID: "obs-47", ObservationSlot: 47, StrategyKey: lane}})
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id, status string) {
		t.Helper()
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,$3,'REPORT_NAV',$4,$5::jsonb)`, id, key, status, lane, string(evidence)); err != nil {
			t.Fatal(err)
		}
	}
	insert(op, "decided")
	if _, err = db.AcquireRouteLease(ctx, key, "writer-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	slotRPC := newFakeChain(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"result":47}`), nil
	}))
	request := bridgeTestRequest(ReportNAV, 0)
	request.LastValidBlockHeight = 10
	request.Report.ObservedSlot, request.Report.Sequence = 47, 47
	effects, _, _, err := bridgeExpectedEffects(Decision{Action: ReportNAV}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	observation := Observation{Snapshot: Snapshot{ObservationID: "obs-47", Slot: 47, RouteLane: lane, StrategyKey: lane, Fresh: true}}

	// No builder reaches its signer before the bind.
	assertBudgetHold(t, db.requireBoundIntent(ctx, op, request, encoded), "operation_not_bound")
	// The bind binds only the decision the row records.
	other := observation
	other.Snapshot.ObservationID = "obs-other"
	assertBudgetHold(t, db.bindOperation(ctx, slotRPC, manifest, op, other, decision, request, effects), "bind_journal_mismatch")
	if err = db.bindOperation(ctx, slotRPC, manifest, op, observation, decision, request, effects); err != nil {
		t.Fatal(err)
	}
	// A bound row is never bound again, and the builder signs only its intent.
	assertBudgetHold(t, db.bindOperation(ctx, slotRPC, manifest, op, observation, decision, request, effects), "bind_journal_mismatch")
	if err = db.requireBoundIntent(ctx, op, request, encoded); err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.Report.NAVAfterRaw++
	assertBudgetHold(t, db.requireBoundIntent(ctx, op, changed, encoded), "operation_not_bound")

	// A persisted wire is not sendable until its hash is bound to the intent.
	message, err := CompileBridgeMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	signed := PersistedOperation{Operation: Operation{ID: op, RouteKey: key, Decision: decision}, Status: Signed, ExpectedEffects: evidence, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: request.RecentBlockhash, LastValidBlockHeight: 10}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,signed_wire_sha256=$3,transaction_signature=$4,recent_blockhash=$5,last_valid_block_height=10 WHERE operation_id=$1`, op, wire, signed.SignedWireSHA256, signed.TransactionSignature, signed.RecentBlockhash); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, markBroadcastIntent(ctx, db, manifest, signed), "signed_wire_binding_mismatch")
	bindWire := func(hash string) error {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err = bindSignedWireTx(ctx, tx, op, hash); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err = bindWire(signed.SignedWireSHA256); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, bindWire(strings.Repeat("b", 64)), "signed_wire_binding_mismatch")

	// A pre-send hold fails only a never-submitted row and keeps its reason.
	holdID := op + "-hold"
	hold := &BudgetHold{Reason: "squads_spending_limit_exceeded", Details: map[string]string{"amountRaw": "1"}}
	assertBudgetHold(t, db.RecordPhase3BudgetHold(ctx, op, hold), "budget_hold_requires_never_submitted_operation")

	// Only the live lease sends: after a restart takes the route, the old
	// writer can no longer record broadcast intent.
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
	if err = markBroadcastIntent(ctx, db, manifest, signed); err == nil {
		t.Fatal("stale writer retained send authority")
	}
	if err = markBroadcastIntent(ctx, restarted, manifest, signed); err != nil {
		t.Fatal(err)
	}
	var status string
	var intent bool
	if err = restarted.pool.QueryRow(ctx, `SELECT status,broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, op).Scan(&status, &intent); err != nil {
		t.Fatal(err)
	}
	if status != "broadcast_intent" || !intent {
		t.Fatalf("bound wire did not record broadcast intent: %s %v", status, intent)
	}
	// Free the route's one nonterminal slot for the next decided row.
	if _, err = restarted.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='failed' WHERE operation_id=$1`, op); err != nil {
		t.Fatal(err)
	}
	insert(holdID, "decided")
	if err = restarted.RecordPhase3BudgetHold(ctx, holdID, hold); err != nil {
		t.Fatal(err)
	}
	var reason string
	var holdJSON []byte
	if err = restarted.pool.QueryRow(ctx, `SELECT status,recovery_reason,expected_effects->'budgetHold' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, holdID).Scan(&status, &reason, &holdJSON); err != nil {
		t.Fatal(err)
	}
	var retained BudgetHold
	if json.Unmarshal(holdJSON, &retained) != nil || status != "failed" || reason != "phase3_budget_hold:squads_spending_limit_exceeded" || retained.Details["amountRaw"] != "1" {
		t.Fatalf("pre-send hold was not retained: %s %s %s", status, reason, holdJSON)
	}

	// Finalized reconciliation needs the journal's own finalized receipt.
	settleID := op + "-settle"
	expected := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{{Address: bridgeSquadsATA, Owner: classicTokenProgram, Mint: bridgeUSDC, Authority: bridgeVault}}}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,expected_effects,strategy_key,signed_wire,transaction_signature,confirmed_slot,confirmation_status) VALUES($1,$2,'reconciling',$3::jsonb,$4,$5,'settlement-signature',42,'confirmed')`, settleID, key, string(expectedJSON), lane, wire); err != nil {
		t.Fatal(err)
	}
	balances := []TransactionTokenBalance{{Address: bridgeSquadsATA, OwnerProgram: classicTokenProgram, Mint: bridgeUSDC, Authority: bridgeVault}}
	receipt := ConfirmedTransactionEvidence{Signature: "settlement-signature", Slot: 42, PreTokenBalances: balances, PostTokenBalances: balances}
	reconciliation, reconciledEffects, err := ReconcileConfirmedTransaction(expected, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.MarkReconciled(ctx, settleID, reconciliation, reconciledEffects, receipt); err == nil {
		t.Fatal("confirmed-only receipt reconciled")
	}
	receipt.Finalized = true
	receipt.Signature = "wrong-signature"
	if err = restarted.MarkReconciled(ctx, settleID, reconciliation, reconciledEffects, receipt); err == nil {
		t.Fatal("unrelated finalized receipt reconciled")
	}
	receipt.Signature = "settlement-signature"
	if err = restarted.MarkReconciled(ctx, settleID, reconciliation, reconciledEffects, receipt); err != nil {
		t.Fatal(err)
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

// Rows written while the budget existed keep their extra phase3 fields;
// decoding ignores them and keeps the integrity record.
func TestLegacyPhase3RecordDecodes(t *testing.T) {
	auth, _ := signedBindFixture(t)
	encoded, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err = json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["goalId"] = "01a06b6c-8023-72b1-ad5d-c97c0662820e"
	legacy["pilotAuthorityId"] = "01a0a776-cb66-7333-99eb-7e6927c1e114"
	legacy["bookedSpentMicros"] = 9
	legacy["sendKnownCost"] = map[string]any{"totalMicros": 9}
	legacy["bridgeAdmission"] = map[string]any{"exitAfterMicros": 9}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var decoded phase3OperationAuthorization
	if err = json.Unmarshal(raw, &decoded); err != nil || decoded.IntentSHA256 != auth.IntentSHA256 || decoded.SignedWireSHA256 != auth.SignedWireSHA256 || decoded.BuildInput == nil {
		t.Fatalf("legacy record lost its integrity fields: %+v %v", decoded, err)
	}
	var intent UnwindIntent
	if err = json.Unmarshal([]byte(`{"sourceLane":"OnRe/ONyc/USDC","reason":"economic_rotation","observationId":"o","maxCollateralRaw":1,"maxDebtRaw":1,"costBoundRaw":5,"budgetScope":"g","budgetFamily":"OnRe","evidenceId":"`+strings.Repeat("a", 64)+`","createdAt":"2026-10-01T00:00:00Z"}`), &intent); err != nil || intent.validate() != nil {
		t.Fatalf("legacy unwind intent no longer decodes: %+v %v", intent, err)
	}
}
