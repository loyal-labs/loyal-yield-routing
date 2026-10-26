package earnmax

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	h, err := fixtureReader(t, name).History(context.Background(), recordedAuthority, 0)
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
	h, err := fixtureReader(t, "truncated").History(context.Background(), recordedAuthority, 0)
	if err != nil {
		t.Fatal(err)
	}
	if e := earned(h, Position{Value: 100_000_000}); e != nil || h.Complete() {
		t.Fatalf("complete=%v earned=%d, want incomplete and unknown", h.Complete(), *e)
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
	got := operations(t, "claim_then_request", Position{ObservedSlot: 455_763_927, Withdrawal: pending})
	if deref(got["2gQU31Fn:withdraw_request"]) != "4999425" || deref(got["y7KA89kE:withdraw_request"]) != "40000000" {
		t.Fatalf("older request=%s newer request=%s", deref(got["2gQU31Fn:withdraw_request"]), deref(got["y7KA89kE:withdraw_request"]))
	}
}

func TestHistoryOmitsATransactionServedForAnotherSignature(t *testing.T) {
	h, err := fixtureReader(t, "reordered").History(context.Background(), recordedAuthority, 0)
	if err != nil || h.Complete() || len(activity(h, Position{}, nil, time.Unix(1791800000, 0)).Operations) != 0 {
		t.Fatalf("err=%v complete=%v entries=%d, want no error, incomplete and no operations", err, h.Complete(), len(h.Entries))
	}
}

func TestEarnedCountsOnlyHistoryThePositionHasSeen(t *testing.T) {
	paid := USDCRaw(100_000_000)
	h := History{Entries: []Entry{
		Claim{Flow: Flow{Slot: 300}, Paid: &paid},
		Deposit{Flow: Flow{Slot: 100}, Amount: 100_000_000},
	}}
	got := earned(h, Position{ObservedSlot: 200, Withdrawal: &PendingWithdrawal{Payout: 100_000_000}})
	if got == nil || *got != 0 {
		t.Fatalf("earned=%s, want 0", deref(rawString(got)))
	}
}

func TestCancelledOrGappedRequestNeverInheritsAnotherPayout(t *testing.T) {
	got := operations(t, "cancel_across_gap", Position{})
	if deref(got["4SZL6f7U:withdraw_request"]) != "0" || deref(got["2gQU31Fn:withdraw_request"]) != "unknown" {
		t.Fatalf("cancelled request=%s request before a gap=%s", deref(got["4SZL6f7U:withdraw_request"]), deref(got["2gQU31Fn:withdraw_request"]))
	}
}

func TestUnknownWithdrawalDropsLaterPerformancePoints(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	escrow := &Escrow{LP: 100}
	h := History{Entries: []Entry{
		Claim{Flow: Flow{Slot: 3, At: day(3)}, Escrow: escrow},
		Request{Flow: Flow{Slot: 2, At: day(2)}, Escrow: escrow},
		Deposit{Flow: Flow{Slot: 1, At: day(1)}, Amount: 100_000_000},
	}}
	got := fmt.Sprint(activity(h, Position{ObservedSlot: 3}, nil, day(22)).Performance)
	if got != "[{100 2026-10-01T00:00:00.000Z} {0 2026-10-22T00:00:00.000Z}]" {
		t.Fatalf("performance=%s", got)
	}
}

// Recorded mainnet Voltr events, 2026-10-26; no RPC calls or signing.
func TestRecordedCashFlowSupplyIncludesFeesAndDeadWeight(t *testing.T) {
	raw, err := os.ReadFile("testdata/supply-events.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Event, Data               string
		UserBytes                 []byte
		Amount, LP, Before, After uint64
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		user := solana.PublicKey(f.UserBytes)
		logs := []string{programData + f.Data}
		before, after, _, _ := eventSupply(logs, f.Event, user, f.Amount, f.LP)
		if before != f.Before || after != f.After {
			t.Fatalf("%s supply=%d/%d want=%d/%d", f.Event, before, after, f.Before, f.After)
		}
		if before, _, _, _ := eventSupply(logs, f.Event, user, f.Amount+1, f.LP); before != 0 {
			t.Fatal("accepted another cash flow's event")
		}
		if before, _, _, _ := eventSupply(append(logs, logs...), f.Event, user, f.Amount, f.LP); before != 0 {
			t.Fatal("accepted ambiguous events")
		}
	}
}
