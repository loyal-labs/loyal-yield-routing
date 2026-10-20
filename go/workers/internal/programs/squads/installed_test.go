package squads

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
)

func settingsAccountData(policySeed uint64) []byte {
	b := append([]byte(nil), SettingsDiscriminator[:]...)
	b = append(b, make([]byte, 16+32)...) // seed, settings authority
	b = append(b, 1, 0, 0, 0, 0, 0)       // threshold, time lock
	b = append(b, make([]byte, 16)...)    // transaction indexes
	b = append(b, 0)                      // archival authority: none
	b = append(b, make([]byte, 8)...)     // archivable after
	b = append(b, 255, 0, 0, 0, 0, 0)     // bump, no signers, account utilization
	b = append(b, 1)
	b = binary.LittleEndian.AppendUint64(b, policySeed)
	return append(b, 0) // reserved
}

func TestPoliciesAreReadByDerivedAddressWhenProgramScansAreRefused(t *testing.T) {
	settings := solana.PublicKey{1}
	const settingsSlot = 500
	stored := map[solana.PublicKey][]byte{settings: settingsAccountData(4)}
	for seed := uint64(1); seed <= 4; seed++ {
		address, bump, err := PolicyAddress(settings, seed)
		if err != nil {
			t.Fatal(err)
		}
		data := syntheticPolicy(t, true)
		binary.LittleEndian.PutUint64(data[40:48], seed)
		data[48] = bump
		switch seed {
		case 3:
			continue // closed
		case 4:
			data[8] = 9 // another settings' policy at this address
		}
		stored[address] = data
	}
	var pinned []uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Method != "getMultipleAccounts" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 0, "error": map[string]any{"code": -32600, "message": "Request deprioritized due to number of accounts requested"}})
			return
		}
		var keys []solana.PublicKey
		var opts struct {
			MinContextSlot uint64 `json:"minContextSlot"`
		}
		_ = json.Unmarshal(req.Params[0], &keys)
		_ = json.Unmarshal(req.Params[1], &opts)
		pinned = append(pinned, opts.MinContextSlot)
		values := make([]any, len(keys))
		for i, key := range keys {
			if data, ok := stored[key]; ok {
				values[i] = map[string]any{"lamports": 1, "owner": ProgramID.String(), "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}, "executable": false}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 0, "result": map[string]any{"context": map[string]any{"slot": settingsSlot + len(pinned) - 1}, "value": values}})
	}))
	t.Cleanup(server.Close)
	client, err := chain.New(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	installed, err := Policies(context.Background(), client, settings, 400)
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := PolicyAddress(settings, 1)
	second, _, _ := PolicyAddress(settings, 2)
	if len(installed) != 2 || installed[0].Account != first || installed[1].Account != second || installed[0].View == nil || installed[1].View == nil {
		t.Fatalf("installed %v, want the policies at seeds 1 and 2", installed)
	}
	if len(pinned) != 2 || pinned[0] != 400 || pinned[1] != settingsSlot {
		t.Fatalf("minContextSlot per read %v, want the asked slot then the settings read's slot", pinned)
	}
}
