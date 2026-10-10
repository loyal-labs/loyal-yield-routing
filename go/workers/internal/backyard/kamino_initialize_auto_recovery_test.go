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
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// autoInitializerRecoveryFixture compiles the candidate AUTO initializer
// through the reviewed seed-901 binding and derives the exact finalized
// receipt the chain would return for its message: the delegate fee debit, the
// vault rent debit, the obligation rent credit, and the empty obligation
// account the poststate validator admits.
type autoInitializerRecoveryFixture struct {
	manifest RouteManifest
	request  KaminoInitializationRequest
	effects  ExpectedEffects
	raw      []byte
	message  []byte
	wire     []byte
	receipt  ConfirmedTransactionEvidence
}

func newAutoInitializerRecoveryFixture(t *testing.T) autoInitializerRecoveryFixture {
	t.Helper()
	manifest, r := autoInitializerRequestFixture(t)
	message, err := manifest.compileKaminoInitializationMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		t.Fatal(err)
	}
	offset := 3
	count, err := decodeShortVec(message, &offset)
	if err != nil || count < 3 {
		t.Fatal("initializer message accounts", err)
	}
	pre, post := make([]uint64, count), make([]uint64, count)
	for i := 0; i < count; i++ {
		pre[i], post[i] = 1, 1
		switch keyString(message[offset+i*32 : offset+(i+1)*32]) {
		case bridgeDelegate:
			pre[i], post[i] = 1_000_000, 995_000
		case bridgeVault:
			pre[i], post[i] = 100_000_000, 100_000_000-r.RentLamports
		case route.Kamino.Obligation:
			pre[i], post[i] = 0, r.RentLamports
		}
	}
	obligation := ConfirmedAccount{Address: route.Kamino.Obligation, Owner: kamino.ProgramID.String(), Lamports: r.RentLamports, Data: make([]byte, kamino.ObligationSize)}
	copy(obligation.Data, kamino.ObligationDiscriminator[:])
	binary.LittleEndian.PutUint64(obligation.Data[8:16], 1)
	putKey(t, obligation.Data[32:64], route.Kamino.Market)
	putKey(t, obligation.Data[64:96], route.Kamino.Vault)
	effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}
	raw, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	wire := append([]byte{1}, bytes.Repeat([]byte{9}, 64)...)
	wire = append(wire, message...)
	return autoInitializerRecoveryFixture{manifest: manifest, request: r, effects: effects, raw: raw, message: message, wire: wire,
		receipt: ConfirmedTransactionEvidence{Finalized: true, Signature: encodeBase58(wire[1:65]), Slot: 77,
			Initialization: &KaminoInitializationReceipt{MessageSHA256: sha256Bytes(message), SignedWireSHA256: sha256Bytes(wire),
				FeeLamports: 5000, PreBalances: pre, PostBalances: post, AccountReadSlot: 78, Obligation: obligation}}}
}

// The manifest reconciler re-derives admission from the same reviewed binding
// that compiled the message, and rejects every drifted identity: binding seed
// or digest, lane, message hash, fee, slot, wire hash, rent, poststate and the
// native balance graph. The installed-only public reconciler stays closed.
func TestAutoInitializerReconcilesFinalizedReceiptThroughManifest(t *testing.T) {
	f := newAutoInitializerRecoveryFixture(t)
	reconciled, body, err := f.manifest.reconcileKaminoInitialization(f.effects, f.receipt)
	if err != nil || reconciled.Validate() != nil || reconciled.ConfirmedSlot != 77 || !json.Valid(body) {
		t.Fatalf("bound reconciliation failed: %+v %v", reconciled, err)
	}
	viaDispatch, _, err := f.manifest.ReconcileConfirmedTransaction(f.effects, f.receipt)
	if err != nil || viaDispatch != reconciled {
		t.Fatalf("manifest reconciliation dispatch drifted: %+v %v", viaDispatch, err)
	}
	decoded, err := decodeExpectedEffectsWithManifest(f.manifest, f.raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.manifest.ReconcileConfirmedTransaction(decoded, f.receipt); err != nil {
		t.Fatalf("decoded AUTO effects refused through the same manifest: %v", err)
	}

	drifts := []struct {
		name   string
		mutate func(*autoInitializerRecoveryFixture)
	}{
		{"policy", func(f *autoInitializerRecoveryFixture) {
			request := *f.effects.Initialization
			request.Policy = testPolicyAccount(policyKey{family: BasicDebtLifecycle})
			f.effects.Initialization = &request
		}},
		{"installed_lane", func(f *autoInitializerRecoveryFixture) {
			request := *f.effects.Initialization
			request.RouteLane = PhaseOneLaneID
			f.effects.Initialization = &request
		}},
		{"foreign_lane", func(f *autoInitializerRecoveryFixture) {
			request := *f.effects.Initialization
			request.RouteLane = "Ethena/USDe/PYUSD"
			f.effects.Initialization = &request
		}},
		{"message_hash", func(f *autoInitializerRecoveryFixture) {
			f.receipt.Initialization.MessageSHA256 = sha256Bytes([]byte("other"))
		}},
		{"receipt_slot", func(f *autoInitializerRecoveryFixture) { f.receipt.Slot = 79 }},
		{"fee_missing", func(f *autoInitializerRecoveryFixture) { f.receipt.Initialization.FeeLamports = 0 }},
		{"fee_above_bound", func(f *autoInitializerRecoveryFixture) { f.receipt.Initialization.FeeLamports = 5001 }},
		{"wire_hash", func(f *autoInitializerRecoveryFixture) { f.receipt.Initialization.SignedWireSHA256 = "not-a-digest" }},
		{"read_before_receipt", func(f *autoInitializerRecoveryFixture) { f.receipt.Initialization.AccountReadSlot = 76 }},
		{"obligation_rent", func(f *autoInitializerRecoveryFixture) { f.receipt.Initialization.Obligation.Lamports-- }},
		{"hidden_collateral", func(f *autoInitializerRecoveryFixture) { f.receipt.Initialization.Obligation.Data[128] = 1 }},
		{"foreign_debit", func(f *autoInitializerRecoveryFixture) {
			n := len(f.receipt.Initialization.PostBalances) - 1
			f.receipt.Initialization.PostBalances[n]--
		}},
		{"unfinalized", func(f *autoInitializerRecoveryFixture) { f.receipt.Finalized = false }},
	}
	for _, drift := range drifts {
		t.Run(drift.name, func(t *testing.T) {
			mutated := newAutoInitializerRecoveryFixture(t)
			drift.mutate(&mutated)
			if _, _, err := mutated.manifest.ReconcileConfirmedTransaction(mutated.effects, mutated.receipt); err == nil {
				t.Fatal("drifted AUTO initializer reconciled")
			}
		})
	}
	// The installed-only public reconciler keeps refusing the candidate lane.
	if _, _, err := reconcileKaminoInitialization(f.effects, f.receipt); err == nil || !strings.Contains(err.Error(), "unreviewed Multiply initializer lane") {
		t.Fatalf("public reconciler admitted AUTO: %v", err)
	}
	if _, _, err := ReconcileConfirmedTransaction(f.effects, f.receipt); err == nil {
		t.Fatal("public reconciliation dispatch admitted AUTO")
	}
}

// The narrow manifest-aware decision validator admits the candidate journal
// decision; the public Decision.Validate
// keeps refusing every AUTO initializer decision outright, and the exact
// installed shape requirements hold in both forms.
func TestAutoInitializerDecisionValidatesOnlyThroughManifestBinding(t *testing.T) {
	manifest, r := autoInitializerRequestFixture(t)
	decision := Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: r.RouteLane, IdempotencyKey: "controlled-init"}
	if err := manifest.validateInitializerDecision(decision, r); err != nil {
		t.Fatalf("bound AUTO initializer decision refused: %v", err)
	}
	// The public decision gate keeps the candidate lane closed.
	if err := decision.Validate(); err == nil {
		t.Fatal("public Decision.Validate admitted an AUTO initializer decision")
	}
	for name, mutate := range map[string]func(*Decision){
		"reason":        func(d *Decision) { d.Reason = "other" },
		"amount":        func(d *Decision) { d.AmountRaw = 1 },
		"idempotency":   func(d *Decision) { d.IdempotencyKey = "" },
		"lane_mismatch": func(d *Decision) { d.StrategyKey = PhaseOneLaneID },
		"action":        func(d *Decision) { d.Action = ReportNAV },
	} {
		t.Run(name, func(t *testing.T) {
			drifted := decision
			mutate(&drifted)
			if err := manifest.validateInitializerDecision(drifted, r); err == nil {
				t.Fatal("drifted AUTO initializer decision admitted")
			}
		})
	}
	// Installed selector decisions keep validating in both forms.
	installed := decision
	installed.StrategyKey = PhaseOneLaneID
	if err := installed.Validate(); err != nil || manifest.validateInitializerDecision(installed, func() KaminoInitializationRequest {
		req := r
		req.RouteLane = PhaseOneLaneID
		req.Policy = testPolicyAccount(policyKey{lane: PhaseOneLaneID, action: InitializeKaminoObligation})
		return req
	}()) != nil {
		t.Fatalf("installed selector decision drifted: %v", err)
	}
}

// autoInitializerRecoveryRPC serves the finalized immutable receipt for the
// exact persisted wire, plus the obligation account read anchored to it.
func autoInitializerRecoveryRPC(t *testing.T, f autoInitializerRecoveryFixture, receiptSlot int64, mutate func(result any) any) *chain.Client {
	t.Helper()
	rpc := newFakeChain(t, nil)
	rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Method string
			Params []json.RawMessage
			ID     any
		}
		if json.NewDecoder(req.Body).Decode(&body) != nil {
			t.Fatal("request")
		}
		var result any
		switch body.Method {
		case "getSignatureStatuses":
			result = map[string]any{"context": map[string]any{"slot": f.receipt.Slot}, "value": []any{map[string]any{"slot": f.receipt.Slot, "err": nil, "confirmationStatus": "finalized"}}}
		case "getTransaction":
			var signature string
			_ = json.Unmarshal(body.Params[0], &signature)
			if signature != f.receipt.Signature {
				t.Fatal("wrong immutable receipt request")
			}
			actual := append([]byte(nil), f.wire...)
			meta := map[string]any{"err": nil, "fee": f.receipt.Initialization.FeeLamports,
				"preBalances": f.receipt.Initialization.PreBalances, "postBalances": f.receipt.Initialization.PostBalances,
				"preTokenBalances": []any{}, "postTokenBalances": []any{}}
			result = map[string]any{"slot": receiptSlot, "transaction": []string{base64.StdEncoding.EncodeToString(actual), "base64"}, "meta": meta}
		case "getMultipleAccounts":
			a := f.receipt.Initialization.Obligation
			result = map[string]any{"context": map[string]any{"slot": 78}, "value": []any{map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false,
				"data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}}}
		default:
			t.Fatal("unexpected RPC method", body.Method)
		}
		if mutate != nil {
			result = mutate(result)
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}, nil
	})
	return rpc
}

// The manifest receipt observer binds the persisted journal wire, the journal
// decision and the finalized RPC receipt before reconciliation; every drifted
// observation is refused.
func TestAutoInitializerObserveBindsPersistedWireThroughManifest(t *testing.T) {
	for _, drift := range []string{"", "wire", "slot", "fee_missing", "failed", "tokens", "journal_action", "journal_signature"} {
		t.Run(drift, func(t *testing.T) {
			f := newAutoInitializerRecoveryFixture(t)
			op := PersistedOperation{Operation: Operation{Decision: Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: f.request.RouteLane, IdempotencyKey: "controlled-init"}},
				ConfirmedSlot: 77, TransactionSignature: f.receipt.Signature, SignedWire: f.wire, SignedWireSHA256: sha256Bytes(f.wire),
				RecentBlockhash: f.request.RecentBlockhash, LastValidBlockHeight: f.request.LastValidBlockHeight}
			receiptSlot := int64(77)
			switch drift {
			case "journal_action":
				op.Decision.Action = ReportNAV
			case "journal_signature":
				op.TransactionSignature = "other"
			}
			rpc := autoInitializerRecoveryRPC(t, f, receiptSlot, func(result any) any {
				mutated := result.(map[string]any)
				if drift == "wire" {
					mutated["transaction"] = []string{base64.StdEncoding.EncodeToString(append([]byte(nil), f.wire[:len(f.wire)-1]...)), "base64"}
				}
				if drift == "slot" {
					mutated["slot"] = 78
				}
				if drift == "failed" {
					mutated["meta"] = map[string]any{"err": "failure", "fee": 5000, "preBalances": f.receipt.Initialization.PreBalances, "postBalances": f.receipt.Initialization.PostBalances}
				}
				if drift == "fee_missing" {
					mutated["meta"] = map[string]any{"err": nil, "preBalances": f.receipt.Initialization.PreBalances, "postBalances": f.receipt.Initialization.PostBalances}
				}
				if drift == "tokens" {
					mutated["meta"] = map[string]any{"err": nil, "fee": 5000, "preBalances": f.receipt.Initialization.PreBalances, "postBalances": f.receipt.Initialization.PostBalances,
						"preTokenBalances": []any{map[string]any{"accountIndex": 1}}, "postTokenBalances": []any{}}
				}
				return mutated
			})
			got, err := f.manifest.observeFinalizedKaminoInitialization(context.Background(), rpc, f.request, op)
			if err == nil {
				_, _, err = f.manifest.ReconcileConfirmedTransaction(f.effects, got)
			}
			if (err == nil) != (drift == "") {
				t.Fatalf("drift=%s err=%v", drift, err)
			}
		})
	}
}

// seedAutoInitializerReconcilingOperation reconstructs the durable pre-settle
// state: a bound AUTO initializer operation, the wire bound to the journal,
// and the operation parked in 'reconciling'. The route key is supplied by the
// owning test so cleanup deletes exactly these rows.
func seedAutoInitializerReconcilingOperation(t *testing.T, ctx context.Context, db *Database, f autoInitializerRecoveryFixture, key string) (string, PersistedOperation) {
	t.Helper()
	id := seedBoundOperation(t, ctx, db, key, "auto-initializer-recovery-test", InitializeKaminoObligation, f.request.RouteLane, f.request, f.raw)
	bindTestWire(t, ctx, db, id, sha256Bytes(f.wire))
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciling',signed_wire_sha256=$2,transaction_signature=$3,confirmed_slot=$4,signed_wire=$5 WHERE operation_id=$1`,
		id, sha256Bytes(f.wire), f.receipt.Signature, f.receipt.Slot, f.wire); err != nil {
		t.Fatal(err)
	}
	op := PersistedOperation{Operation: Operation{ID: id, RouteKey: key, StrategyKey: f.request.RouteLane,
		Decision: Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: f.request.RouteLane, IdempotencyKey: "controlled-init"}},
		Status: Reconciling, ExpectedEffects: f.raw, SignedWire: f.wire, SignedWireSHA256: sha256Bytes(f.wire),
		TransactionSignature: f.receipt.Signature, RecentBlockhash: f.request.RecentBlockhash,
		LastValidBlockHeight: f.request.LastValidBlockHeight, ConfirmedSlot: f.receipt.Slot}
	return id, op
}

// Locked settlement through the exact same reviewed manifest that compiled the
// message: every drifted receipt leaves the journal in 'reconciling', the
// valid receipt settles exactly once, and a replay after completion cannot
// settle a second time.
func TestAutoInitializerLockedSettlementThroughReviewedManifest(t *testing.T) {
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_auto_locked_settlement_test", 30*time.Second)
	defer cancel()
	routeKey := "auto-initializer-recovery-settlement-" + time.Now().Format("150405.000000000")
	f := newAutoInitializerRecoveryFixture(t)
	id, _ := seedAutoInitializerReconcilingOperation(t, ctx, db, f, routeKey)
	defer db.ReleaseRouteLease(ctx)

	for _, drift := range []string{"binding", "wire", "rent", "unfinalized", ""} {
		receipt := *f.receipt.Initialization
		candidate := f.receipt
		candidate.Initialization = &receipt
		expected := f.effects
		settle := func() error {
			reconciliation, effects, reconcileErr := f.manifest.ReconcileConfirmedTransaction(expected, candidate)
			if reconcileErr != nil {
				return reconcileErr
			}
			return db.markReconciledOnManifest(ctx, f.manifest, id, reconciliation, effects, candidate)
		}
		switch drift {
		case "binding":
			request := *expected.Initialization
			request.Policy = testPolicyAccount(policyKey{family: BasicDebtLifecycle})
			expected.Initialization = &request
		case "wire":
			receipt.SignedWireSHA256 = sha256Bytes([]byte("unrelated wire"))
		case "rent":
			receipt.Obligation.Lamports++
		case "unfinalized":
			candidate.Finalized = false
		case "":
			// The valid receipt is the one that settles, last.
		}
		if err := settle(); (err == nil) != (drift == "") {
			t.Fatalf("drift=%s err=%v", drift, err)
		}
		status := operationStatus(t, ctx, db, id)
		if drift == "" {
			if status != "reconciled" {
				t.Fatalf("finalized settlement did not settle: %s", status)
			}
			continue
		}
		if status != "reconciling" {
			t.Fatalf("drift=%s left the journal: %s", drift, status)
		}
	}

	// No replay after completion: the journal row is no longer 'reconciling',
	// so a repeated settlement cannot rewrite it.
	reconciliation, effects, err := f.manifest.ReconcileConfirmedTransaction(f.effects, f.receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.markReconciledOnManifest(ctx, f.manifest, id, reconciliation, effects, f.receipt); err == nil {
		t.Fatal("completed initializer settled twice")
	}
	if status := operationStatus(t, ctx, db, id); status != "reconciled" {
		t.Fatalf("replay mutated settled state: %s", status)
	}
}

// Restart recovery drives the shared nonterminal state machine with the
// reviewed manifest: the reconstructed 'reconciling' operation observes the
// finalized receipt, reconciles and settles in the locked transaction exactly
// once, never resends, and every drift path lands in the existing durable
// recovery stops. The public embedded-manifest entrypoint keeps the candidate
// lane closed at decode.
func TestAutoInitializerRestartReconcilesThroughSharedStateMachine(t *testing.T) {
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_auto_restart_recovery_test", 30*time.Second)
	defer cancel()
	var routeKeys []string

	newFixture := func() (string, PersistedOperation, autoInitializerRecoveryFixture) {
		f := newAutoInitializerRecoveryFixture(t)
		key := "auto-initializer-recovery-restart-" + time.Now().Format("150405.000000000") + "-" + strconv.Itoa(len(routeKeys))
		routeKeys = append(routeKeys, key)
		id, op := seedAutoInitializerReconcilingOperation(t, ctx, db, f, key)
		return id, op, f
	}
	assertRecoveryStop := func(t *testing.T, id, wantStatus, wantReason string) {
		t.Helper()
		var status, reason string
		if err := db.pool.QueryRow(ctx, `SELECT status,COALESCE(recovery_reason,'') FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		if status != wantStatus || (wantReason != "" && reason != wantReason) {
			t.Fatalf("recovery stop drifted: %s/%s want %s/%s", status, reason, wantStatus, wantReason)
		}
	}

	// Drift first: a receipt slot that differs from the journal identity is
	// refused by the immutable-receipt observer; the operation stays in
	// 'reconciling' for the next tick.
	slotID, slotOp, slotFixture := newFixture()
	rpc := autoInitializerRecoveryRPC(t, slotFixture, 78, nil)
	if err := advanceNonterminalWithManifest(ctx, slotFixture.manifest, db, rpc, slotOp); err == nil {
		t.Fatal("slot-drifted initializer receipt was observed")
	}
	assertRecoveryStop(t, slotID, "reconciling", "")

	// Success: the reconstructed operation advances through the shared state
	// machine into locked settlement, exactly once, with reads only on the wire.
	id, op, f := newFixture()
	sent := 0
	rpc = autoInitializerRecoveryRPC(t, f, 77, nil)
	inner := rpcOf(rpc).Transport
	rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		var body struct{ Method string }
		_ = json.Unmarshal(raw, &body)
		if body.Method == "sendTransaction" {
			sent++
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		return inner.RoundTrip(req)
	})
	if err := advanceNonterminalWithManifest(ctx, f.manifest, db, rpc, op); err != nil {
		t.Fatal(err)
	}
	if status := operationStatus(t, ctx, db, id); status != "reconciled" {
		t.Fatalf("restart did not settle: %s", status)
	}
	if sent != 0 {
		t.Fatalf("recovery resent the wire %d times", sent)
	}

	// No replay after completion: the durable row is terminal, so the shared
	// entrypoint refuses to advance it again and nothing re-settles.
	if err := advanceNonterminalWithManifest(ctx, f.manifest, db, rpc, op); err == nil {
		t.Fatal("terminal initializer advanced twice")
	}
	if status := operationStatus(t, ctx, db, id); status != "reconciled" {
		t.Fatalf("replay mutated settled state: %s", status)
	}
	if sent != 0 {
		t.Fatalf("replay resent the wire: %d", sent)
	}
	var persistedStatus string
	if err := db.pool.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&persistedStatus); err != nil {
		t.Fatal(err)
	}
	if persistedStatus != "reconciled" || IsNonterminal(OperationStatus(persistedStatus)) {
		t.Fatalf("terminal journal state drifted: %s", persistedStatus)
	}
}

// The initializer builder persists its build through the same explicit
// reviewed manifest; the candidate build cannot exist in the journal under the
// embedded manifest, and the produced build effects decode only through that
// same manifest on recovery.
func TestAutoInitializerBuildPersistsThroughReviewedManifest(t *testing.T) {
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_auto_build_persistence_test", 30*time.Second)
	defer cancel()
	f := newAutoInitializerRecoveryFixture(t)
	key := "auto-initializer-build-" + time.Now().Format("150405.000000000")
	id := key + "-op"
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":1}',1)`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "auto-initializer-build-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	defer db.ReleaseRouteLease(ctx)
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects)
	 VALUES($1,$2,'decided',$3,$4,$5)`, id, key, string(InitializeKaminoObligation), f.request.RouteLane, f.raw); err != nil {
		t.Fatal(err)
	}
	if err := db.markBuiltOnManifest(ctx, f.manifest, id, sha256Bytes(f.message), f.raw); err != nil {
		t.Fatalf("reviewed build persistence refused the candidate: %v", err)
	}
	var status, persisted string
	if err := db.pool.QueryRow(ctx, `SELECT status,expected_effects->'expectedEffects' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&status, &persisted); err != nil {
		t.Fatal(err)
	}
	if status != "built" {
		t.Fatalf("build persistence status %s", status)
	}
	// Recovery decodes the persisted effects through the same manifest.
	if _, err := decodeExpectedEffectsWithManifest(f.manifest, []byte(persisted)); err != nil {
		t.Fatalf("persisted candidate effects refused through the reviewed manifest: %v", err)
	}
}
