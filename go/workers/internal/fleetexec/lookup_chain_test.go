package fleetexec

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	sdk "github.com/solana-foundation/solana-go/v2"
)

// serveLookupBank answers finalized getMultipleAccounts at slot 1000 with an
// absent table and the given SlotHashes account; any other read fails.
func serveLookupBank(t *testing.T, hashes map[string]any) *chain.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "getMultipleAccounts" || len(req.Params) != 2 || !strings.Contains(string(req.Params[1]), `"finalized"`) {
			http.Error(w, "unexpected read", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 0, "result": map[string]any{"context": map[string]any{"slot": 1000}, "value": []any{nil, hashes}}})
	}))
	t.Cleanup(server.Close)
	client, err := chain.New(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestLookupReadsRefreshInconsistentBankOnce(t *testing.T) {
	for _, catalog := range []bool{false, true} {
		for _, scenario := range []string{"valid", "future-then-valid", "unordered-then-valid", "still-invalid"} {
			name := "single/" + scenario
			if catalog {
				name = "catalog/" + scenario
			}
			t.Run(name, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := calls.Add(1)
					var request struct {
						Method string
						Params []json.RawMessage
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Method != "getMultipleAccounts" || len(request.Params) != 2 {
						t.Error("unexpected lookup read")
						http.Error(w, "bad request", 400)
						return
					}
					var options struct {
						Commitment     string
						MinContextSlot uint64
					}
					if err := json.Unmarshal(request.Params[1], &options); err != nil || options.Commitment != "finalized" || (n == 2 && options.MinContextSlot != 1001) || n > 2 {
						t.Errorf("unbounded or stale lookup reread: call=%d options=%+v", n, options)
						http.Error(w, "bad request", 400)
						return
					}
					slot := uint64(1000 + 2*(n-1))
					data := make([]byte, 88)
					binary.LittleEndian.PutUint64(data, 2)
					binary.LittleEndian.PutUint64(data[8:], slot-1)
					binary.LittleEndian.PutUint64(data[48:], slot-2)
					if scenario == "still-invalid" || (n == 1 && scenario == "future-then-valid") {
						binary.LittleEndian.PutUint64(data[8:], slot+1)
					}
					if n == 1 && scenario == "unordered-then-valid" {
						binary.LittleEndian.PutUint64(data[48:], slot-1)
					}
					hashes := map[string]any{"owner": sdk.SysVarPubkey.String(), "lamports": 1, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 0, "result": map[string]any{"context": map[string]any{"slot": slot}, "value": []any{nil, hashes}}})
				}))
				t.Cleanup(server.Close)
				client, err := chain.New(server.URL, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				address := readLookupFixture(t).Table
				var snapshot LookupSnapshot
				if catalog {
					var observations []lookupCatalogObservation
					observations, err = lookupCatalogObservations(t.Context(), client, []string{address}, 0)
					if len(observations) == 1 {
						snapshot = observations[0].snapshot
					}
				} else {
					snapshot, err = lookupSnapshot(t.Context(), client, address, 0)
				}
				wantCalls := int32(2)
				if scenario == "valid" {
					wantCalls = 1
				}
				if calls.Load() != wantCalls || (err != nil) != (scenario == "still-invalid") {
					t.Fatalf("calls=%d want=%d error=%v", calls.Load(), wantCalls, err)
				}
				if err == nil && (snapshot.Slot != int64(1000+2*(wantCalls-1)) || len(snapshot.SlotHashes) != 2 || snapshot.SlotHashes[0] != uint64(snapshot.Slot-1)) {
					t.Fatalf("accepted mixed or incomplete bank: %+v", snapshot)
				}
			})
		}
	}
}

func TestLookupSnapshotDecodesOfficialPaddedSlotHashes(t *testing.T) {
	f := readLookupFixture(t)
	var account struct {
		Owner    string
		Lamports uint64
		Data     string
	}
	if err := json.Unmarshal(f.Accounts["SysvarS1otHashes111111111111111111111111111"], &account); err != nil {
		t.Fatal(err)
	}
	hashes := func(owner string, data []byte) map[string]any {
		return map[string]any{"owner": owner, "lamports": account.Lamports, "executable": false, "rentEpoch": 0, "space": len(data), "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}
	}
	d, err := base64.StdEncoding.DecodeString(account.Data)
	if err != nil {
		t.Fatal(err)
	}
	out, err := lookupSnapshot(t.Context(), serveLookupBank(t, hashes(account.Owner, d)), f.Table, 0)
	if err != nil || !out.Absent || out.Slot != 1000 || len(out.SlotHashes) != 2 || out.SlotHashes[0] != 999 {
		t.Fatalf("actual official fixture: %+v %v", out, err)
	}
	for _, kind := range []string{"future", "padding", "wrong-owner"} {
		t.Run(kind, func(t *testing.T) {
			data, owner := append([]byte(nil), d...), account.Owner
			switch kind {
			case "future":
				binary.LittleEndian.PutUint64(data[8:], 1001)
			case "padding":
				data[len(data)-1] = 1
			case "wrong-owner":
				owner = f.Manager
			}
			if _, err := lookupSnapshot(t.Context(), serveLookupBank(t, hashes(owner, data)), f.Table, 0); err == nil {
				t.Fatal("malformed cooldown proof accepted")
			}
		})
	}
}
