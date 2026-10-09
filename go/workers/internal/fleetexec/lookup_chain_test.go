package fleetexec

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
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
