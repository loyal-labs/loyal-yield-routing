package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

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
	keys, err := messageKeys(message)
	count := len(keys)
	if err != nil || count < 3 {
		t.Fatal("initializer message accounts", err)
	}
	pre, post := make([]uint64, count), make([]uint64, count)
	for i := 0; i < count; i++ {
		pre[i], post[i] = 1, 1
		switch encodeBase58(keys[i][:]) {
		case backyardDelegate:
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
	wire := signedWire(message, bytes.Repeat([]byte{9}, 64))
	return autoInitializerRecoveryFixture{manifest: manifest, request: r, effects: effects, raw: raw, message: message, wire: wire,
		receipt: ConfirmedTransactionEvidence{Finalized: true, Signature: encodeBase58(wireSig(wire)), Slot: 77,
			Initialization: &KaminoInitializationReceipt{MessageSHA256: sha256Bytes(message), SignedWireSHA256: sha256Bytes(wire),
				FeeLamports: 5000, PreBalances: pre, PostBalances: post, AccountReadSlot: 78, Obligation: obligation}}}
}

// The manifest reconciler re-derives admission from the same reviewed binding
// that compiled the message, and rejects every drifted identity: binding seed
// or digest, lane, message hash, fee, slot, wire hash, rent, poststate and the
// native balance graph. The installed-only public reconciler stays closed.
func TestAutoInitializerReconcilesFinalizedReceiptThroughManifest(t *testing.T) {
	t.Parallel()
	f := newAutoInitializerRecoveryFixture(t)
	reconciled, body, err := f.manifest.reconcileKaminoInitialization(f.effects, f.receipt)
	if err != nil || reconciled.Validate() != nil || reconciled.ConfirmedSlot != 77 || !json.Valid(body) {
		t.Fatalf("bound reconciliation failed: %+v %v", reconciled, err)
	}
	viaDispatch, _, err := ReconcileConfirmedTransaction(f.manifest.vault, f.effects, f.receipt)
	if err != nil || viaDispatch != reconciled {
		t.Fatalf("manifest reconciliation dispatch drifted: %+v %v", viaDispatch, err)
	}
	decoded, err := DecodeExpectedEffects(f.manifest.vault, f.raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = ReconcileConfirmedTransaction(f.manifest.vault, decoded, f.receipt); err != nil {
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
			request.RouteLane = PrimeUSDCLaneID
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
			if _, _, err := ReconcileConfirmedTransaction(mutated.manifest.vault, mutated.effects, mutated.receipt); err == nil {
				t.Fatal("drifted AUTO initializer reconciled")
			}
		})
	}
}

// The AUTO initializer decision validates (its lane has an installed
// initializer policy); a lane without one never does, and the exact shape
// requirements hold.
func TestAutoInitializerDecisionValidatesThroughTheRegistry(t *testing.T) {
	t.Parallel()
	manifest, r := autoInitializerRequestFixture(t)
	decision := Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: r.RouteLane, IdempotencyKey: "controlled-init"}
	if err := manifest.validateInitializerDecision(decision, r); err != nil {
		t.Fatalf("bound AUTO initializer decision refused: %v", err)
	}
	if err := decision.Validate(); err != nil {
		t.Fatalf("Decision.Validate refused the AUTO initializer decision: %v", err)
	}
	for _, lane := range []string{primePRIMEPYUSD.Lane, primePRIMEUSDS.Lane, ethenaUSDePYUSD.Lane} {
		sibling := decision
		sibling.StrategyKey = lane
		if sibling.Validate() == nil {
			t.Fatal("initializer decision admitted for a lane without an initializer policy", lane)
		}
	}
	for name, mutate := range map[string]func(*Decision){
		"reason":        func(d *Decision) { d.Reason = "other" },
		"amount":        func(d *Decision) { d.AmountRaw = 1 },
		"idempotency":   func(d *Decision) { d.IdempotencyKey = "" },
		"lane_mismatch": func(d *Decision) { d.StrategyKey = PrimeUSDCLaneID },
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
	installed.StrategyKey = PrimeUSDCLaneID
	if err := installed.Validate(); err != nil || manifest.validateInitializerDecision(installed, func() KaminoInitializationRequest {
		req := r
		req.RouteLane = PrimeUSDCLaneID
		req.Policy = testPolicyAccount(policyKey{lane: PrimeUSDCLaneID, action: InitializeKaminoObligation})
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
	t.Parallel()
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
				_, _, err = ReconcileConfirmedTransaction(f.manifest.vault, f.effects, got)
			}
			if (err == nil) != (drift == "") {
				t.Fatalf("drift=%s err=%v", drift, err)
			}
		})
	}
}
