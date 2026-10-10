package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// autoInitializerAuthorizationFixture recompiles the shared candidate fixture
// at an observed rent. The reviewed binding pins the policy seed and account
// digest, never the synthetically observed rent: the value below is an
// ordinary chain observable (per-byte rent times the exact obligation size),
// and the rent sysvar served by the transport serves it exactly.
func autoInitializerAuthorizationFixture(t *testing.T) autoInitializerRecoveryFixture {
	t.Helper()
	f := newAutoInitializerRecoveryFixture(t)
	f.request.RentLamports = 289 * (kamino.ObligationSize + 128)
	var err error
	f.effects.Initialization = &f.request
	f.raw, err = jsonMarshalExpectedEffects(f.effects)
	if err != nil {
		t.Fatal(err)
	}
	f.message, err = f.manifest.compileKaminoInitializationMessage(f.request)
	if err != nil {
		t.Fatal(err)
	}
	f.wire = append([]byte{1}, bytes.Repeat([]byte{9}, 64)...)
	f.wire = append(f.wire, f.message...)
	return f
}

// autoInitializerAuthorizationRPC composes the shared budget-build transport
// (slot, message-fee, native-price and rent-exemption reads) with the shared
// candidate AUTO prestate account set from autoInitializerPrestateAccounts:
// the initializer prestate batch is served from those accounts with the target
// obligation absent, and every other read delegates to the base transport.
// The rent sysvar prices the fixture request's exact rent. sendTransaction is
// counted and refused, and the signature lands once it was attempted;
// simulateTransaction is refused outright: no signer exists in these tests.
func autoInitializerAuthorizationRPC(t *testing.T, f autoInitializerRecoveryFixture) (*chain.Client, *int) {
	t.Helper()
	accounts := autoInitializerPrestateAccounts(t, f.request)
	const rentAddress = "SysvarRent111111111111111111111111111111111"
	rent := accounts[rentAddress]
	binary.LittleEndian.PutUint64(rent.Data, f.request.RentLamports/(kamino.ObligationSize+128))
	accounts[rentAddress] = rent
	rpc := budgetBuildRPC(t, 5000, 42)
	base := rpcOf(rpc).Transport
	sends := 0
	rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Method string
			Params []json.RawMessage
			ID     any
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		switch body.Method {
		case "sendTransaction":
			// The lifecycle attempts the broadcast only after durable
			// broadcast intent committed. Count the attempt and refuse it;
			// the status below lands the signature once it was sent.
			sends++
			return &http.Response{StatusCode: 500, Body: io.NopCloser(bytes.NewReader([]byte(`{}`))), Header: make(http.Header)}, nil
		case "getSignatureStatuses":
			status := `null`
			if sends > 0 {
				status = `{"slot":43,"confirmations":1,"err":null,"confirmationStatus":"confirmed"}`
			}
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":[` + status + `]}}`), nil
		case "simulateTransaction":
			t.Fatalf("authorization must not simulate: no signer exists in this chain")
		case "getMultipleAccounts":
			var addresses []string
			if err = json.Unmarshal(body.Params[0], &addresses); err == nil && len(addresses) > 0 && addresses[0] == bridgeDelegate {
				values := make([]any, len(addresses))
				for i, address := range addresses {
					if a, ok := accounts[address]; ok {
						values[i] = map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable,
							"data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
					}
				}
				encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID,
					"result": map[string]any{"context": map[string]any{"slot": 42}, "value": values}})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(encoded)), Header: make(http.Header)}, nil
			}
		}
		return base.RoundTrip(req)
	})
	return rpc, &sends
}

// seedAutoInitializerDecidedOperation seeds one decided candidate initializer
// operation on a test-owned route key, bound as bindOperation leaves it when
// bound is set, so cleanup deletes exactly these rows and nothing else.
func seedAutoInitializerDecidedOperation(t *testing.T, ctx context.Context, db *Database, f autoInitializerRecoveryFixture, key string, bound bool) string {
	t.Helper()
	if bound {
		return seedBoundOperation(t, ctx, db, key, "auto-initializer-authorization-test", InitializeKaminoObligation, f.request.RouteLane, f.request, f.raw)
	}
	id := key + "-op"
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":1}',1)`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "auto-initializer-authorization-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
	 VALUES($1,$2,'decided',$3,$4,$5)`, id, key, string(InitializeKaminoObligation), f.request.RouteLane, f.raw); err != nil {
		t.Fatal(err)
	}
	return id
}

// loadAutoInitializerAuth reads the persisted phase3 record for the operation
// under test.
func loadAutoInitializerAuth(t *testing.T, ctx context.Context, db *Database, id string) phase3OperationAuthorization {
	t.Helper()
	var encoded []byte
	if err := db.pool.QueryRow(ctx, `SELECT COALESCE(expected_effects->'phase3','null'::jsonb) FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(encoded, &auth) != nil {
		t.Fatal("phase3 record decode")
	}
	return auth
}

// The candidate AUTO initializer reaches its signer only through the real
// builder gates — the bound intent, then the fresh prestate through the
// reviewed binding — while an unbound row, the embedded public prestate gate
// and a drifted binding all refuse it without transitioning the journal.
func TestAutoInitializerBuildGateThroughReviewedManifest(t *testing.T) {
	f := autoInitializerAuthorizationFixture(t)
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_auto_build_authorization_test", 30*time.Second)
	defer cancel()
	var routeKeys []string
	rpc, sends := autoInitializerAuthorizationRPC(t, f)
	newOp := func(bound bool) string {
		t.Helper()
		key := "auto-initializer-buildauth-" + time.Now().Format("150405.000000000") + "-" + strconv.Itoa(len(routeKeys))
		routeKeys = append(routeKeys, key)
		return seedAutoInitializerDecidedOperation(t, ctx, db, f, key, bound)
	}

	// The candidate passes every gate and stops only at the absent signer.
	id := newOp(true)
	err := BuildSimulateAndPersistKaminoInitialization(ctx, db, rpc, id, f.manifest, f.request, Credentials{})
	if err == nil || err.Error() != "Backyard signing capability is not configured" {
		t.Fatalf("candidate build did not reach the signer boundary: %v", err)
	}
	if status := operationStatus(t, ctx, db, id); status != "decided" {
		t.Fatalf("build gates transitioned the journal: %s", status)
	}
	auth := loadAutoInitializerAuth(t, ctx, db, id)
	request, effects, message, err := auth.BuildInput.decodeWithManifest(f.manifest)
	if err != nil {
		t.Fatalf("persisted candidate build input refused by the reviewed manifest: %v", err)
	}
	if request.(KaminoInitializationRequest) != f.request || *effects.Initialization != f.request || !bytes.Equal(message, f.message) {
		t.Fatal("persisted candidate build input drifted from the reviewed binding")
	}

	// No bind, no signer.
	unbound := newOp(false)
	assertBudgetHold(t, BuildSimulateAndPersistKaminoInitialization(ctx, db, rpc, unbound, f.manifest, f.request, Credentials{}), "operation_not_bound")
	if status := operationStatus(t, ctx, db, unbound); status != "decided" {
		t.Fatalf("unbound refusal transitioned the journal: %s", status)
	}

	// A request that names another policy is refused before the binding
	// comparison even runs, and the journal row keeps its bound build input.
	drifted := f.request
	drifted.Policy = testPolicyAccount(policyKey{family: BasicDebtLifecycle})
	if _, err := f.manifest.validateRequestPrestate(ctx, rpc, drifted, f.effects); err == nil {
		t.Fatal("drifted initializer request passed the prestate gate")
	}
	if auth := loadAutoInitializerAuth(t, ctx, db, id); auth.BuildInput == nil {
		t.Fatal("drifted request erased the bound build input")
	}
	if *sends != 0 {
		t.Fatalf("build gates attempted a broadcast %d times", *sends)
	}
}

// The real Signed transition: through the manifest-threaded internal lifecycle
// path, the candidate's persisted wire is proven against the reviewed binding,
// the locked final-send fence passes, broadcast intent is recorded
// atomically before the broadcast, and the landed wire confirms. The embedded
// public entrypoint keeps the same wire closed at decode, a drifted journal
// identity never reaches the chain, and a completed send cannot be replayed.
func TestAutoInitializerSignedTransitionThroughReviewedManifest(t *testing.T) {
	f := autoInitializerAuthorizationFixture(t)
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_auto_signed_transition_test", 30*time.Second)
	defer cancel()
	rpc, sends := autoInitializerAuthorizationRPC(t, f)
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)

	signedOperation := func(key string, decision Decision) PersistedOperation {
		t.Helper()
		id := seedAutoInitializerDecidedOperation(t, ctx, db, f, key, true)
		// The selector entry that authorizes the initializer at send.
		storeTestSelectorEntry(t, ctx, db, key, autoSelectorEntryFixture(time.Now().UTC(), 3_000_000, &price))
		// A local unsigned wire fixture: no signer exists in this chain, the
		// exact compiled message is bound into the journal like any wire.
		hash := sha256Bytes(f.wire)
		bindTestWire(t, ctx, db, id, hash)
		if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,signed_wire_sha256=$3 WHERE operation_id=$1`, id, f.wire, hash); err != nil {
			t.Fatal(err)
		}
		return PersistedOperation{Operation: Operation{ID: id, RouteKey: key, StrategyKey: f.request.RouteLane, Decision: decision},
			Status: Signed, ExpectedEffects: f.raw, SignedWire: f.wire, SignedWireSHA256: hash,
			TransactionSignature: encodeBase58(f.wire[1:65]), RecentBlockhash: f.request.RecentBlockhash, LastValidBlockHeight: f.request.LastValidBlockHeight}
	}
	op := signedOperation("auto-initializer-send-"+time.Now().Format("150405.000000000"),
		Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: f.request.RouteLane, IdempotencyKey: "controlled-init"})
	// The decision slot the custody window starts from.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=expected_effects || '{"decision":{"observationSlot":42}}'::jsonb WHERE operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	op.ExpectedEffects = []byte(`{"decision":{"observationSlot":42}}`)

	// The wired internal lifecycle path: identity proof, locked final-send
	// fence, durable broadcast intent, then the wire lands at its slot.
	if err := advanceNonterminalWithManifest(ctx, f.manifest, db, rpc, op); err != nil {
		t.Fatal(err)
	}
	var confirmedSlot int64
	if err := db.pool.QueryRow(ctx, `SELECT confirmed_slot FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND status='confirmed' AND broadcast_intent_at IS NOT NULL`, op.ID).Scan(&confirmedSlot); err != nil || confirmedSlot != 43 {
		t.Fatalf("landed wire not confirmed at its slot: %d %v", confirmedSlot, err)
	}
	if *sends != 1 {
		t.Fatalf("broadcast attempted %d times before landing", *sends)
	}

	// A completed send cannot be replayed: the durable row is no longer
	// signed, so the final-send fence cannot re-record it.
	if err := markBroadcastIntent(ctx, db, f.manifest, op); err == nil {
		t.Fatal("completed send replayed")
	}

	// A journal decision that lost its initializer identity is refused before
	// any chain read and before any transition.
	foreign := signedOperation("auto-initializer-identity-"+time.Now().Format("150405.000000000"),
		Decision{Action: Hold, Reason: "unrelated", StrategyKey: f.request.RouteLane, IdempotencyKey: "controlled-init"})
	assertBudgetHold(t, markBroadcastIntent(ctx, db, f.manifest, foreign), "initializer_journal_identity_mismatch")
	if status := operationStatus(t, ctx, db, foreign.ID); status != "signed" {
		t.Fatalf("identity refusal mutated the signed journal row: %s", status)
	}
}
