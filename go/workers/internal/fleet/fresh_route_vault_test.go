package fleet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Production shadow revalidation failed every same-mint route at
// load_fresh_route with "required account <x> is absent" for an account that
// was neither reserve and was genuinely absent at confirmed commitment
// (rehearsal shadow-approved-account-diagnostic-20261004T0009Z). The Go port
// read the Squads vault PDA as a required account; a smart-account vault is a
// system account only while it holds lamports and signs through CPI either
// way. The fresh read must not depend on the vault account existing.
func TestFreshRouteDoesNotRequireTheVaultPDAAccount(t *testing.T) {
	market := testIdentity(40)
	sourceIdentity := ReserveIdentity{Address: testIdentity(1), Market: market, Mint: USDCMint}
	targetIdentity := ReserveIdentity{Address: testIdentity(2), Market: market, Mint: USDCMint}
	reserves := map[string]Account{
		sourceIdentity.Address: reserveFixture(sourceIdentity, 1_000_000, 0),
		targetIdentity.Address: reserveFixture(targetIdentity, 1_000_000, 0),
	}
	vault := testIdentity(4)
	var requested [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &call); err != nil {
			t.Error(err)
		}
		var result any = int64(100)
		if call.Method == "getMultipleAccounts" {
			var addresses []string
			_ = json.Unmarshal(call.Params[0], &addresses)
			requested = append(requested, addresses)
			values := make([]any, len(addresses))
			for i, address := range addresses {
				if account, ok := reserves[address]; ok {
					values[i] = map[string]any{"owner": account.Owner, "lamports": account.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(account.Data), "base64"}}
				} else if address != vault {
					// Every other account exists but is not a valid obligation.
					values[i] = map[string]any{"owner": SquadsProgram, "lamports": 1, "executable": false, "data": []string{"", "base64"}}
				}
			}
			result = map[string]any{"context": map[string]any{"slot": 100}, "value": values}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer server.Close()
	r := &Revalidator{rpc: NewRPCClient(server.URL), slotDuration: 400 * time.Millisecond}
	lease := RevalidationLease{VaultPubkey: vault, SourceReserve: sourceIdentity.Address, TargetReserve: targetIdentity.Address, LiquidityMint: USDCMint, PolicyAccount: testIdentity(5)}
	_, _, err := r.loadFreshRoute(context.Background(), lease)
	if err == nil || strings.Contains(err.Error(), "is absent") || !strings.Contains(err.Error(), "obligation") {
		t.Fatalf("fresh read did not proceed past an absent vault PDA: %v", err)
	}
	for _, addresses := range requested {
		for _, address := range addresses {
			if address == vault {
				t.Fatal("fresh route read the vault PDA as route evidence")
			}
		}
	}
}
