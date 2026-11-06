package webapp

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

func TestWithdrawalConsentReportsUnansweredRPCAsUnavailableNotGenerationRefusal(t *testing.T) {
	wallet := solana.NewWallet().PublicKey()
	settings := solana.PublicKey{7}
	settingsData := append([]byte(nil), squads.SettingsDiscriminator[:]...)
	settingsData = append(settingsData, make([]byte, 16+32)...)
	settingsData = append(settingsData, 1, 0, 0, 0, 0, 0)
	settingsData = append(settingsData, make([]byte, 16)...)
	settingsData = append(settingsData, 0)
	settingsData = append(settingsData, make([]byte, 8)...)
	settingsData = append(settingsData, 255, 1, 0, 0, 0)
	settingsData = append(append(settingsData, wallet[:]...), squads.FullPermissions)
	settingsData = append(settingsData, 0, 0, 0)

	rpcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var keys []string
		if req.Method == "getMultipleAccounts" && json.Unmarshal(req.Params[0], &keys) == nil && len(keys) == 1 && keys[0] == settings.String() {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"context": map[string]any{"slot": 100},
				"value":   []any{map[string]any{"data": []string{base64.StdEncoding.EncodeToString(settingsData), "base64"}, "owner": squads.ProgramID.String(), "lamports": 1, "executable": false, "rentEpoch": 0}},
			}})
			return
		}
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(rpcServer.Close)
	client, err := chain.New(rpcServer.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := jwt.NewKeys("isolated-session-test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jwt.NewSigner("isolated-session-test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	smartAccount, _, err := squads.SmartAccountAddress(settings, 0)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Mint(map[string]any{"authMethod": "wallet", "subjectAddress": wallet.String(), "displayAddress": wallet.String(), "walletAddress": wallet.String(), "smartAccountAddress": smartAccount.String(), "settingsPda": settings.String()}, jwt.NoAudience, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	router := chi.NewRouter()
	earnMaxRoutes{auth: &Auth{keys: keys, chain: client}, cfg: EarnMax{Cluster: Mainnet, Reader: earnmax.NewReader(client, nil), Consents: earnmax.NewWithdrawalConsentStore(nil)}}.registerWithdrawalConsents(humachi.New(router, apiConfig()))
	req := httptest.NewRequest(http.MethodGet, withdrawalConsentPath, nil)
	req.Header.Set("Cookie", walletSessionCookie+"="+token)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusServiceUnavailable || body.Error.Code != "earn_max_unavailable" {
		t.Fatalf("unanswered RPC = %d %s; want 503 earn_max_unavailable, not a generation refusal", res.Code, res.Body.String())
	}
}
