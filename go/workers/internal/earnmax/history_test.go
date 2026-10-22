package earnmax

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

var recordedAuthority = Authority{solana.MustPublicKeyFromBase58("8vih4bMxHD3ffxb48f7gtZvbFa7z5o8vzUdCTk6Y2vgi")}

type rpcFixture struct {
	Signatures []struct {
		Signature string `json:"signature"`
		Slot      uint64 `json:"slot"`
		BlockTime int64  `json:"blockTime"`
	} `json:"signatures"`
	Transactions map[string]json.RawMessage `json:"transactions"`
	Serve        map[string]string          `json:"serve"`
}

func fixtureReader(t *testing.T, name string) *Reader {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var f rpcFixture
	var top struct {
		Signatures json.RawMessage `json:"signatures"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result := json.RawMessage("null")
		switch req.Method {
		case "getSignaturesForAddress":
			result = top.Signatures
		case "getTransaction":
			var sig string
			_ = json.Unmarshal(req.Params[0], &sig)
			if served, ok := f.Serve[sig]; ok {
				sig = served
			}
			if tx, ok := f.Transactions[sig]; ok {
				result = tx
				break
			}
			for _, row := range f.Signatures {
				if row.Signature == sig {
					result = emptyTransaction(t, row.Signature, row.Slot, row.BlockTime)
				}
			}
		default:
			t.Errorf("unexpected RPC %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	client, err := chain.New(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return NewReader(client, nil)
}

func emptyTransaction(t *testing.T, signature string, slot uint64, blockTime int64) json.RawMessage {
	tx := solana.Transaction{
		Signatures: []solana.Signature{solana.MustSignatureFromBase58(signature)},
		Message:    solana.Message{Header: solana.MessageHeader{NumRequiredSignatures: 1}, AccountKeys: solana.PublicKeySlice{recordedAuthority.key}},
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{
		"slot": slot, "blockTime": blockTime, "transaction": []string{base64.StdEncoding.EncodeToString(wire), "base64"},
		"meta": map[string]any{"err": nil, "fee": 5000, "preBalances": []int{0}, "postBalances": []int{0}, "innerInstructions": []any{},
			"logMessages": []string{}, "preTokenBalances": []any{}, "postTokenBalances": []any{}},
	})
	return out
}

func operations(t *testing.T, name string, p Position) map[string]*string {
	t.Helper()
	h, err := fixtureReader(t, name).History(context.Background(), recordedAuthority)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*string{}
	for _, op := range activity(h, p, nil, time.Unix(1791800000, 0)).Operations {
		out[op.ID[:8]+":"+op.Action] = op.AmountRaw
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return "unknown"
	}
	return *s
}

func TestEarnedIsUnknownWhenAFailedSignatureFillsThePage(t *testing.T) {
	h, err := fixtureReader(t, "truncated").History(context.Background(), recordedAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if e := earned(h, Position{Value: 100_000_000}); e != nil || h.Complete {
		t.Fatalf("complete=%v earned=%d, want incomplete and unknown", h.Complete, *e)
	}
}

func TestClaimIsChargedOnlyItsOwnPayout(t *testing.T) {
	got := operations(t, "claim_and_deposit", Position{})
	if deref(got["y7KA89kE:claim"]) != "4999425" || deref(got["y7KA89kE:deposit"]) != "40000000" {
		t.Fatalf("claim=%s deposit=%s", deref(got["y7KA89kE:claim"]), deref(got["y7KA89kE:deposit"]))
	}
}

func TestClaimPairsWithTheRequestThatEscrowedItsLP(t *testing.T) {
	pending := &PendingWithdrawal{EscrowedLP: 107_632, Payout: 40_000_000}
	got := operations(t, "claim_then_request", Position{Withdrawal: pending})
	if deref(got["2gQU31Fn:withdraw_request"]) != "4999425" || deref(got["y7KA89kE:withdraw_request"]) != "40000000" {
		t.Fatalf("older request=%s newer request=%s", deref(got["2gQU31Fn:withdraw_request"]), deref(got["y7KA89kE:withdraw_request"]))
	}
}

func TestHistoryRejectsATransactionServedForAnotherSignature(t *testing.T) {
	_, err := fixtureReader(t, "reordered").History(context.Background(), recordedAuthority)
	if err == nil {
		t.Fatal("history accepted a transaction served for another signature")
	}
}
