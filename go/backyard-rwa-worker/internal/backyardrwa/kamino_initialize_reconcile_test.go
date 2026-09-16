package backyardrwa

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
)

func TestInitializationRPCBindsThePersistedWireAndRejectsUnexpectedMetadata(t *testing.T) {
	for _, drift := range []string{"", "wire", "slot", "fee_missing", "failed", "tokens", "return_data", "account_owner", "journal_action", "journal_signature"} {
		t.Run(drift, func(t *testing.T) {
			e, receipt := initializationReconcileFixture(t)
			r := *e.Initialization
			message, err := CompileKaminoInitializationMessage(r)
			if err != nil {
				t.Fatal(err)
			}
			wire := append([]byte{1}, bytes.Repeat([]byte{9}, 64)...)
			wire = append(wire, message...)
			op := PersistedOperation{Operation: Operation{Decision: Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: r.RouteLane, IdempotencyKey: "controlled-init"}},
				ConfirmedSlot: 77, TransactionSignature: encodeBase58(wire[1:65]), SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), RecentBlockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}
			if drift == "journal_action" {
				op.Decision.Action = ReportNAV
			}
			if drift == "journal_signature" {
				op.TransactionSignature = "other"
			}
			rpc, _ := NewRPCClient("https://rpc.invalid")
			rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
				case "getTransaction":
					var signature string
					var cfg map[string]any
					_ = json.Unmarshal(body.Params[0], &signature)
					_ = json.Unmarshal(body.Params[1], &cfg)
					if signature != op.TransactionSignature || cfg["commitment"] != "finalized" || cfg["encoding"] != "base64" {
						t.Fatal("wrong immutable receipt request")
					}
					actual := append([]byte(nil), wire...)
					slot := 77
					if drift == "wire" {
						actual[len(actual)-1] ^= 1
					}
					if drift == "slot" {
						slot = 78
					}
					meta := map[string]any{"err": nil, "fee": 5000, "preBalances": receipt.Initialization.PreBalances, "postBalances": receipt.Initialization.PostBalances, "preTokenBalances": []any{}, "postTokenBalances": []any{}}
					if drift == "fee_missing" {
						delete(meta, "fee")
					}
					if drift == "failed" {
						meta["err"] = "failure"
					}
					if drift == "tokens" {
						meta["preTokenBalances"] = []any{map[string]any{"accountIndex": 1}}
					}
					if drift == "return_data" {
						meta["returnData"] = map[string]any{"programId": kaminoProgram, "data": []string{"AA==", "base64"}}
					}
					result = map[string]any{"slot": slot, "transaction": []string{base64.StdEncoding.EncodeToString(actual), "base64"}, "meta": meta}
				case "getMultipleAccounts":
					var cfg map[string]any
					var addresses []string
					_ = json.Unmarshal(body.Params[1], &cfg)
					_ = json.Unmarshal(body.Params[0], &addresses)
					a := receipt.Initialization.Obligation
					if len(addresses) != 1 || addresses[0] != a.Address || cfg["commitment"] != "finalized" || cfg["minContextSlot"] != float64(77) {
						t.Fatal("account not anchored to receipt")
					}
					if drift == "account_owner" {
						a.Owner = bridgeSquadsProgram
					}
					result = map[string]any{"context": map[string]any{"slot": 78}, "value": []any{map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}}}
				default:
					t.Fatal("unexpected RPC method", body.Method)
				}
				encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}, nil
			})
			got, err := observeFinalizedKaminoInitialization(context.Background(), rpc, r, op)
			if err == nil {
				_, _, err = ReconcileConfirmedTransaction(e, got)
			}
			if (err == nil) != (drift == "") {
				t.Fatalf("drift=%s error=%v", drift, err)
			}
		})
	}
}

func initializationReconcileFixture(t *testing.T) (ExpectedEffects, ConfirmedTransactionEvidence) {
	t.Helper()
	route, accounts, _ := pairCapacityFixture(t)
	r := KaminoInitializationRequest{RouteLane: route.Lane, PolicySeed: 145, PolicyAccountDataSHA256: sha256Bytes([]byte("controlled policy")),
		RecentBlockhash: bridgeVault, LastValidBlockHeight: 100, RentLamports: 17_637_760, MaximumFeeLamports: 5000}
	message, err := CompileKaminoInitializationMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	offset := 3
	count, err := decodeShortVec(message, &offset)
	if err != nil {
		t.Fatal(err)
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
	a := accounts[2]
	a.Lamports = r.RentLamports
	binary.LittleEndian.PutUint64(a.Data[8:16], 1)
	e := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}
	receipt := ConfirmedTransactionEvidence{Finalized: true, Signature: "controlled-initialization", Slot: 77,
		Initialization: &KaminoInitializationReceipt{MessageSHA256: sha256Bytes(message), SignedWireSHA256: sha256Bytes([]byte("controlled wire")),
			FeeLamports: 5000, PreBalances: pre, PostBalances: post, AccountReadSlot: 78, Obligation: a}}
	return e, receipt
}

func TestInitializationConservesNativeRentAndPreservesTokenContract(t *testing.T) {
	e, receipt := initializationReconcileFixture(t)
	raw, _ := json.Marshal(e)
	decoded, err := DecodeExpectedEffects(raw)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, body, err := ReconcileConfirmedTransaction(decoded, receipt)
	if err != nil || reconciled.Validate() != nil || !json.Valid(body) {
		t.Fatal("native conservation", reconciled, err)
	}
	for _, kind := range []string{"", "kamino-borrow", "bridge", "cross-mint-swap"} {
		bad := e
		bad.Kind = kind
		raw, _ = json.Marshal(bad)
		if _, err := DecodeExpectedEffects(raw); err == nil {
			t.Fatal("native creation masquerades as token transfer", kind)
		}
		if _, _, err := ReconcileConfirmedTransaction(bad, receipt); err == nil {
			t.Fatal("native effects bypass kind validation", kind)
		}
	}
	bad := e
	bad.Accounts = []ExpectedAccountEffect{{Address: bridgeSquadsATA}}
	if _, _, err := ReconcileConfirmedTransaction(bad, receipt); err == nil {
		t.Fatal("initializer mixes token effects")
	}
	bad = e
	bad.Initialization = nil
	if _, _, err := ReconcileConfirmedTransaction(bad, receipt); err == nil {
		t.Fatal("native contract omitted")
	}
}

func TestInitializationRejectsNativeAndCreatedStateDrift(t *testing.T) {
	for _, row := range []struct {
		name   string
		mutate func(*ConfirmedTransactionEvidence)
	}{
		{"unfinalized", func(r *ConfirmedTransactionEvidence) { r.Finalized = false }},
		{"missing_native", func(r *ConfirmedTransactionEvidence) { r.Initialization = nil }},
		{"missing_fee", func(r *ConfirmedTransactionEvidence) { r.Initialization.FeeLamports = 0 }},
		{"fee_above_bound", func(r *ConfirmedTransactionEvidence) { r.Initialization.FeeLamports = 5001 }},
		{"message", func(r *ConfirmedTransactionEvidence) { r.Initialization.MessageSHA256 = sha256Bytes([]byte("other")) }},
		{"short_balances", func(r *ConfirmedTransactionEvidence) { r.Initialization.PreBalances = r.Initialization.PreBalances[1:] }},
		{"network_fee_debit", func(r *ConfirmedTransactionEvidence) { r.Initialization.PostBalances[0]-- }},
		{"foreign_balance", func(r *ConfirmedTransactionEvidence) {
			i := len(r.Initialization.PostBalances) - 1
			r.Initialization.PostBalances[i]--
		}},
		{"old_account", func(r *ConfirmedTransactionEvidence) { r.Initialization.AccountReadSlot = 76 }},
		{"wrong_program", func(r *ConfirmedTransactionEvidence) { r.Initialization.Obligation.Owner = bridgeSquadsProgram }},
		{"wrong_owner", func(r *ConfirmedTransactionEvidence) { r.Initialization.Obligation.Data[64] ^= 1 }},
		{"wrong_market", func(r *ConfirmedTransactionEvidence) { r.Initialization.Obligation.Data[32] ^= 1 }},
		{"wrong_tag", func(r *ConfirmedTransactionEvidence) { r.Initialization.Obligation.Data[8] = 0 }},
		{"hidden_collateral", func(r *ConfirmedTransactionEvidence) { r.Initialization.Obligation.Data[128] = 1 }},
		{"hidden_debt", func(r *ConfirmedTransactionEvidence) { r.Initialization.Obligation.Data[1296] = 1 }},
		{"elevation_group", func(r *ConfirmedTransactionEvidence) { r.Initialization.Obligation.Data[2285] = 1 }},
		{"referrer", func(r *ConfirmedTransactionEvidence) { r.Initialization.Obligation.Data[2288] = 1 }},
		{"account_rent", func(r *ConfirmedTransactionEvidence) { r.Initialization.Obligation.Lamports-- }},
	} {
		t.Run(row.name, func(t *testing.T) {
			e, r := initializationReconcileFixture(t)
			row.mutate(&r)
			if _, _, err := ReconcileConfirmedTransaction(e, r); err == nil {
				t.Fatal("drift admitted")
			}
		})
	}
}
