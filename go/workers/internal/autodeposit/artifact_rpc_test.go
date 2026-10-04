package autodeposit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArtifactRPCSuccessfulFullReceiptAndBoundedHistory(t *testing.T) {
	f, _, _ := artifactFixture(t)
	receipt := goldenCreatorReceipt(t, f)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("request decode")
			w.WriteHeader(400)
			return
		}
		var result any
		if request.Method == "getSignaturesForAddress" {
			var config struct {
				Limit      int    `json:"limit"`
				Commitment string `json:"commitment"`
			}
			_ = json.Unmarshal(request.Params[1], &config)
			if config.Limit != 32 || config.Commitment != "confirmed" {
				t.Error("unbounded or wrong commitment")
			}
			result = []any{map[string]any{"signature": receipt.Signature, "slot": receipt.Slot, "err": nil}}
		} else if request.Method == "getTransaction" {
			result = map[string]any{"slot": receipt.Slot, "transaction": []any{f.WireBase64, "base64"}, "meta": map[string]any{"err": nil, "preBalances": receipt.PreLamports, "postBalances": receipt.PostLamports, "loadedAddresses": map[string]any{"writable": []string{}, "readonly": []string{}}, "innerInstructions": []any{}}}
		} else {
			t.Error("unexpected RPC method")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer server.Close()
	rpc, e := NewArtifactRPC(server.URL)
	if e != nil {
		t.Fatal(e)
	}
	history, e := rpc.ArtifactHistory(t.Context(), f.Policy, 32)
	if e != nil || len(history) != 1 || history[0].Failed {
		t.Fatalf("history=%+v err=%v", history, e)
	}
	actual, e := rpc.ArtifactReceipt(t.Context(), receipt.Signature)
	if e != nil || actual.Signature != receipt.Signature || actual.Slot != receipt.Slot || len(actual.Wire) != len(receipt.Wire) {
		t.Fatalf("receipt=%+v err=%v", actual, e)
	}
}
func TestArtifactRPCHoldsFailedReceiptAndRedactsErrors(t *testing.T) {
	f, _, _ := artifactFixture(t)
	receipt := goldenCreatorReceipt(t, f)
	for _, response := range []string{`{"jsonrpc":"2.0","id":2,"result":[]}`, `{"jsonrpc":"2.0","id":1,"error":{"message":"provider-secret"}}`, `{"jsonrpc":"2.0","id":1,"result":{"slot":125,"transaction":["bad","base64"],"meta":{"err":{"InstructionError":[0,"failed"]}}}}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response)) }))
		rpc, e := NewArtifactRPC(server.URL)
		if e != nil {
			t.Fatal(e)
		}
		_, e = rpc.ArtifactReceipt(t.Context(), receipt.Signature)
		server.Close()
		if e == nil || strings.Contains(e.Error(), "provider-secret") || strings.Contains(e.Error(), server.URL) {
			t.Fatalf("response accepted or error leaked: %v", e)
		}
	}
	for _, endpoint := range []string{"http://example.com", "https://user:password@example.com", "https://example.com#token", "file:///tmp/provider"} {
		if _, e := NewArtifactRPC(endpoint); e == nil {
			t.Fatalf("unsafe provider URL accepted: %s", endpoint)
		}
	}
	if _, err := NewArtifactRPC("https://example.com?api-key=provider-secret"); err != nil {
		t.Fatal("configured HTTPS provider credentials rejected")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer server.Close()
	rpc, err := NewArtifactRPC(server.URL + "?api-key=provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = rpc.ArtifactReceipt(t.Context(), receipt.Signature)
	if err == nil || strings.Contains(err.Error(), "provider-secret") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("query credential leaked: %v", err)
	}
}
