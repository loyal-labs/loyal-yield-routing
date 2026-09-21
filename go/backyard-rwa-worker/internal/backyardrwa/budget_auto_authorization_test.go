package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// autoInitializerAuthorizationFixture recompiles the shared candidate fixture
// at a rent the legacy no-pilot transaction cap admits. The reviewed binding
// pins the policy seed and account digest, never the synthetically observed
// rent: the value below is an ordinary chain observable (per-byte rent times
// the exact obligation size), and the rent sysvar served by the transport
// prices it exactly. Production initializer admission additionally runs under
// pilot budgets and selector-entry authority — the reported remaining gate.
func autoInitializerAuthorizationFixture(t *testing.T) autoInitializerRecoveryFixture {
	t.Helper()
	f := newAutoInitializerRecoveryFixture(t)
	f.request.RentLamports = 289 * (kaminoObligationLength + 128)
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
// counted and refused, simulateTransaction is refused outright: these tests
// prove locked build/send authorization only, never a signer or a broadcast.
func autoInitializerAuthorizationRPC(t *testing.T, f autoInitializerRecoveryFixture) (*RPCClient, *int) {
	t.Helper()
	accounts := autoInitializerPrestateAccounts(t, f.request)
	const rentAddress = "SysvarRent111111111111111111111111111111111"
	rent := accounts[rentAddress]
	binary.LittleEndian.PutUint64(rent.Data, f.request.RentLamports/(kaminoObligationLength+128))
	accounts[rentAddress] = rent
	rpc := budgetBuildRPC(t, 5000, 42)
	base := rpc.client.Transport
	sends := 0
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
			// The lifecycle attempts the broadcast only after the durable
			// broadcast-intent reservation committed. Count the attempt and
			// refuse it: the test proves the reservation, never a send.
			sends++
			return &http.Response{StatusCode: 500, Body: io.NopCloser(bytes.NewReader([]byte(`{}`))), Header: make(http.Header)}, nil
		case "simulateTransaction":
			t.Fatalf("authorization must not simulate: no signer exists in this chain")
		case "getMultipleAccounts":
			var addresses []string
			if err = json.Unmarshal(body.Params[0], &addresses); err == nil && len(addresses) > 0 && addresses[0] == bridgeSettings {
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
// operation on a test-owned route key with a phase3 reservation, so cleanup
// deletes exactly these rows and nothing else.
func seedAutoInitializerDecidedOperation(t *testing.T, ctx context.Context, db *Database, f autoInitializerRecoveryFixture, key string, upperMicros int64, reserve bool) string {
	t.Helper()
	id := key + "-op"
	budget := emptyTestBudget()
	state, _ := json.Marshal(map[string]any{"generation": 1, "phase3": budget})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,$3)`, key, state, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "auto-initializer-authorization-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
	 VALUES($1,$2,'decided',$3,$4,$5)`, id, key, string(InitializeKaminoObligation), f.request.RouteLane, f.raw); err != nil {
		t.Fatal(err)
	}
	if !reserve {
		return id
	}
	digest, err := Phase3IntentDigest(f.request, f.raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReservePhase3(ctx, BudgetReservation{OperationID: id, Family: "AUTO", IntentSHA256: digest, UpperMicros: upperMicros}); err != nil {
		t.Fatal(err)
	}
	return id
}

// loadAutoInitializerAuth reads the persisted phase3 authorization for the
// operation under test.
func loadAutoInitializerAuth(t *testing.T, ctx context.Context, db *Database, id string) phase3OperationAuthorization {
	t.Helper()
	var encoded []byte
	if err := db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(encoded, &auth) != nil {
		t.Fatal("phase3 authorization decode")
	}
	return auth
}

// The candidate AUTO initializer passes the real locked production build
// authorization — fresh prestate/cost observation through the reviewed
// binding, then the locked reservation transaction — while the embedded public
// gate, a missing reservation, an under-priced reservation and a drifted
// binding all refuse it without transitioning the journal.
func TestAutoInitializerBuildAuthorizationThroughReviewedManifest(t *testing.T) {
	f := autoInitializerAuthorizationFixture(t)
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	// Close runs after the constraint restore (registered inside relax...),
	// so the shared fixture database never keeps the initializer scope absent.
	t.Cleanup(func() { db.Close() })
	var routeKeys []string
	relaxInitializerScopeForSyntheticTest(t, ctx, db, func() []string { return routeKeys })
	rpc, sends := autoInitializerAuthorizationRPC(t, f)
	digest, err := Phase3IntentDigest(f.request, f.raw)
	if err != nil {
		t.Fatal(err)
	}
	// Production admission reserves the measured fresh cost; measure it once
	// through the same reviewed manifest so the reservation covers it without
	// exceeding the legacy transaction cap that Admit enforces.
	measured, err := f.manifest.observePhase3KnownBuildCost(ctx, rpc, f.request, f.effects)
	if err != nil {
		t.Fatal(err)
	}

	newOp := func(upperMicros int64, reserve bool) string {
		t.Helper()
		key := "auto-initializer-buildauth-" + time.Now().Format("150405.000000000") + "-" + strconv.Itoa(len(routeKeys))
		routeKeys = append(routeKeys, key)
		return seedAutoInitializerDecidedOperation(t, ctx, db, f, key, upperMicros, reserve)
	}

	// The candidate: real cost/prestate observation and the locked reservation.
	id := newOp(measured.TotalMicros, true)
	if err := f.manifest.authorizePhase3ProductionBuild(ctx, db, rpc, id, f.request, f.effects, f.raw); err != nil {
		t.Fatalf("candidate build authorization refused: %v", err)
	}
	var status string
	if err := db.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "decided" {
		t.Fatalf("build authorization transitioned the journal: %s", status)
	}
	auth := loadAutoInitializerAuth(t, ctx, db, id)
	if auth.GoalID != Phase3GoalID || auth.IntentSHA256 != digest || auth.BuildInput == nil || auth.SignedWireSHA256 != "" {
		t.Fatalf("build authorization drift: goal=%s intent=%s buildInput=%v wire=%q", auth.GoalID, auth.IntentSHA256, auth.BuildInput != nil, auth.SignedWireSHA256)
	}
	request, effects, message, err := auth.BuildInput.decodeWithManifest(f.manifest)
	if err != nil {
		t.Fatalf("persisted candidate build input refused by the reviewed manifest: %v", err)
	}
	if request.(KaminoInitializationRequest) != f.request || *effects.Initialization != f.request || !bytes.Equal(message, f.message) {
		t.Fatal("persisted candidate build input drifted from the reviewed binding")
	}
	// The same persisted bytes stay closed through the embedded public decode:
	// only the explicit reviewed manifest admits them.
	if _, _, _, err := auth.BuildInput.decode(); err == nil {
		t.Fatal("embedded decode admitted the candidate build input")
	}
	// Reauthorization is an idempotent recheck, not a second reservation.
	if err := f.manifest.authorizePhase3ProductionBuild(ctx, db, rpc, id, f.request, f.effects, f.raw); err != nil {
		t.Fatalf("idempotent reauthorization refused: %v", err)
	}
	if _, reservations, _ := autoRecoverySettlementState(t, ctx, db, id); reservations != 1 {
		t.Fatalf("reauthorization changed the reservation count: %d", reservations)
	}

	// The embedded public production gate keeps the candidate lane closed
	// before any signer access, with no journal transition.
	if err := authorizePhase3ProductionBuild(ctx, db, rpc, id, f.request, f.effects, f.raw); err == nil {
		t.Fatal("embedded public build gate admitted the AUTO candidate")
	}
	if err := db.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "decided" {
		t.Fatalf("public gate refusal transitioned the journal: %s", status)
	}

	// A fresh known cost above the reservation cannot inherit a larger allowance.
	underpriced := newOp(1, true)
	assertBudgetHold(t, f.manifest.authorizePhase3ProductionBuild(ctx, db, rpc, underpriced, f.request, f.effects, f.raw), "fresh_build_cost_exceeds_reservation")

	// No reservation, no build authorization.
	unreserved := newOp(0, false)
	assertBudgetHold(t, f.manifest.authorizePhase3ProductionBuild(ctx, db, rpc, unreserved, f.request, f.effects, f.raw), "unreserved_build_intent")

	// A request that lost the reviewed seed is refused by the executable-debit
	// identity check before the binding comparison even runs, and the journal
	// row keeps its admitted build input.
	drifted := f.request
	drifted.PolicySeed = autoFixtureSeed
	assertBudgetHold(t, f.manifest.authorizePhase3ProductionBuild(ctx, db, rpc, id, drifted, f.effects, f.raw), "initializer_effects_request_mismatch")
	if auth := loadAutoInitializerAuth(t, ctx, db, id); auth.BuildInput == nil {
		t.Fatal("drifted request erased the admitted build input")
	}
	if *sends != 0 {
		t.Fatalf("build authorization attempted a broadcast %d times", *sends)
	}
}

// The real Signed transition: through the manifest-threaded internal lifecycle
// path, the candidate's persisted wire is revalued against the reviewed
// binding, the locked final-send fence passes, and broadcast intent is
// recorded atomically before any broadcast — which this transport refuses.
// The embedded public entrypoint keeps the same wire closed at decode, a
// drifted journal identity never reaches valuation, and a completed send
// authorization cannot be replayed.
func TestAutoInitializerSignedTransitionThroughReviewedManifest(t *testing.T) {
	f := autoInitializerAuthorizationFixture(t)
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	t.Cleanup(func() { db.Close() })
	var routeKeys []string
	relaxInitializerScopeForSyntheticTest(t, ctx, db, func() []string { return routeKeys })
	rpc, sends := autoInitializerAuthorizationRPC(t, f)

	key := "auto-initializer-send-" + time.Now().Format("150405.000000000")
	routeKeys = append(routeKeys, key)
	// Production admission reserves the measured fresh cost, bounded by the
	// legacy transaction cap that Admit enforces.
	measured, err := f.manifest.observePhase3KnownBuildCost(ctx, rpc, f.request, f.effects)
	if err != nil {
		t.Fatal(err)
	}
	id := seedAutoInitializerDecidedOperation(t, ctx, db, f, key, measured.TotalMicros, true)
	if err := f.manifest.authorizePhase3ProductionBuild(ctx, db, rpc, id, f.request, f.effects, f.raw); err != nil {
		t.Fatal(err)
	}
	// A local unsigned wire fixture: no signer exists in this chain, the exact
	// compiled message is bound into the journal like any recovered wire.
	hash := sha256Bytes(f.wire)
	auth := loadAutoInitializerAuth(t, ctx, db, id)
	auth.SignedWireSHA256 = hash
	encodedAuth, _ := json.Marshal(auth)
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,signed_wire_sha256=$3,expected_effects=jsonb_set(expected_effects,'{phase3}',$4) WHERE operation_id=$1`,
		id, f.wire, hash, encodedAuth); err != nil {
		t.Fatal(err)
	}
	op := PersistedOperation{Operation: Operation{ID: id, RouteKey: key, StrategyKey: f.request.RouteLane,
		Decision: Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: f.request.RouteLane, IdempotencyKey: "controlled-init"}},
		Status: Signed, ExpectedEffects: f.raw, SignedWire: f.wire, SignedWireSHA256: hash,
		TransactionSignature: encodeBase58(f.wire[1:65]), RecentBlockhash: f.request.RecentBlockhash, LastValidBlockHeight: f.request.LastValidBlockHeight}

	// The embedded public final-send entrypoint refuses the candidate at
	// decode, before any transition or RPC.
	if err := db.RevalueAndMarkBroadcastIntent(ctx, rpc, op); err == nil {
		t.Fatal("embedded public send entrypoint admitted the AUTO candidate")
	}
	if status, _, _ := autoRecoverySettlementState(t, ctx, db, id); status != "signed" {
		t.Fatalf("public send refusal transitioned the journal: %s", status)
	}

	// The wired internal lifecycle path: real revaluation, locked final-send
	// fence, durable broadcast intent, then the refused broadcast attempt.
	if err := advanceNonterminalWithManifest(ctx, f.manifest, db, rpc, op); err == nil || !strings.Contains(err.Error(), "ambiguous send after durable broadcast intent") {
		t.Fatalf("expected the ambiguous-send fence, got %v", err)
	}
	status, reservations, _ := autoRecoverySettlementState(t, ctx, db, id)
	if status != "broadcast_intent" {
		t.Fatalf("durable broadcast intent missing: %s", status)
	}
	if reservations != 1 {
		t.Fatalf("broadcast intent released the reservation: %d", reservations)
	}
	sentAuth := loadAutoInitializerAuth(t, ctx, db, id)
	if sentAuth.SendKnownCost == nil || sentAuth.SendKnownCost.TotalMicros <= 0 {
		t.Fatalf("final-send fence did not persist its known cost: %+v", sentAuth.SendKnownCost)
	}
	if *sends != 1 {
		t.Fatalf("broadcast attempted %d times, exactly-once fence lost", *sends)
	}

	// A completed send authorization cannot be replayed: the durable row is no
	// longer signed, so neither the final-send entrypoint nor a repeated
	// lifecycle pass can re-fence or re-record it.
	if err := db.RevalueAndMarkBroadcastIntentOnManifest(ctx, f.manifest, rpc, op); err == nil {
		t.Fatal("completed send authorization replayed")
	}
	reloaded := op
	reloaded.Status = BroadcastIntent
	if err := db.RevalueAndMarkBroadcastIntentOnManifest(ctx, f.manifest, rpc, reloaded); err == nil {
		t.Fatal("non-signed durable row re-entered the final-send fence")
	}
	if status, _, _ := autoRecoverySettlementState(t, ctx, db, id); status != "broadcast_intent" {
		t.Fatalf("replay mutated the durable transition: %s", status)
	}
	if *sends != 1 {
		t.Fatalf("replay attempted another broadcast: %d", *sends)
	}

	// A journal decision that lost its initializer identity is refused before
	// any valuation RPC and before any transition — on its own still-signed
	// operation, seeded and build-authorized exactly like the first.
	identityKey := "auto-initializer-identity-" + time.Now().Format("150405.000000000")
	routeKeys = append(routeKeys, identityKey)
	identityID := seedAutoInitializerDecidedOperation(t, ctx, db, f, identityKey, measured.TotalMicros, true)
	if err := f.manifest.authorizePhase3ProductionBuild(ctx, db, rpc, identityID, f.request, f.effects, f.raw); err != nil {
		t.Fatal(err)
	}
	identityAuth := loadAutoInitializerAuth(t, ctx, db, identityID)
	identityAuth.SignedWireSHA256 = hash
	encodedIdentityAuth, _ := json.Marshal(identityAuth)
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,signed_wire_sha256=$3,expected_effects=jsonb_set(expected_effects,'{phase3}',$4) WHERE operation_id=$1`,
		identityID, f.wire, hash, encodedIdentityAuth); err != nil {
		t.Fatal(err)
	}
	foreign := PersistedOperation{Operation: Operation{ID: identityID, RouteKey: identityKey, StrategyKey: f.request.RouteLane,
		Decision: Decision{Action: Hold, Reason: "unrelated", StrategyKey: f.request.RouteLane, IdempotencyKey: "controlled-init"}},
		Status: Signed, ExpectedEffects: f.raw, SignedWire: f.wire, SignedWireSHA256: hash,
		TransactionSignature: encodeBase58(f.wire[1:65]), RecentBlockhash: f.request.RecentBlockhash, LastValidBlockHeight: f.request.LastValidBlockHeight}
	assertBudgetHold(t, db.RevalueAndMarkBroadcastIntentOnManifest(ctx, f.manifest, rpc, foreign), "initializer_journal_identity_mismatch")
	if status, _, _ := autoRecoverySettlementState(t, ctx, db, identityID); status != "signed" {
		t.Fatalf("identity refusal mutated the signed journal row: %s", status)
	}
}
