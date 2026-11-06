package webapp

import (
	"context"
	"os"
	"strings"

	"encoding/base64"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/releasecontrol"
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
	auth, token := withdrawalRouteAuth(t)
	client := auth.chain

	router := chi.NewRouter()
	earnMaxRoutes{auth: auth, cfg: EarnMax{Cluster: Mainnet, Reader: earnmax.NewReader(client, nil), Consents: earnmax.NewWithdrawalConsentStore(nil)}}.registerWithdrawalConsents(humachi.New(router, apiConfig()))
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

func withdrawalRouteAuth(t *testing.T) (*Auth, string) {
	t.Helper()
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

	return &Auth{keys: keys, chain: client}, token
}

type publicClaimNAVFixture struct {
	err    error
	checks int
}

func (f *publicClaimNAVFixture) CheckNAVPublication(context.Context) error { f.checks++; return f.err }
func (*publicClaimNAVFixture) ExistingExact(context.Context, earnmax.WithdrawalConsent, []byte) (string, bool, error) {
	panic("public claim must not reserve a consent")
}
func (*publicClaimNAVFixture) Reserve(context.Context, earnmax.WithdrawalConsent, []byte, string, string, uint64, uint64) (string, error) {
	panic("public claim must not reserve a consent")
}

func TestPublicClaimReadinessNeedsAuthAndFreshPriceButNoConsent(t *testing.T) {
	dsn := os.Getenv("EARNMAX_WITHDRAWAL_TEST_DSN")
	if dsn == "" {
		t.Skip("fresh local withdrawal database required")
	}
	if !strings.Contains(dsn, "@127.0.0.1:") || !strings.Contains(dsn, "/vault_cost_test_") {
		t.Fatal("requires disposable local database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	auth, token := withdrawalRouteAuth(t)
	checker := &publicClaimNAVFixture{}
	router := chi.NewRouter()
	earnMaxRoutes{auth: auth, cfg: EarnMax{Cluster: Mainnet, Attempts: checker, Releases: pool}}.registerRelease(humachi.New(router, apiConfig()))
	control, _, err := releasecontrol.Capability(ctx, pool, earnmax.Vault.String(), releasecontrol.ClaimAttempt, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		signedIn bool
		state    releasecontrol.State
		nav      error
		status   int
	}{
		{"signed out", false, releasecontrol.Open, nil, 401},
		{"operator hold", true, releasecontrol.Held, nil, 409},
		{"stale high price", true, releasecontrol.Open, earnmax.ErrClaimWaitingNAVPublication, 409},
		{"public claim", true, releasecontrol.Open, nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control, err = releasecontrol.Set(ctx, pool, releasecontrol.Change{Vault: earnmax.Vault.String(), Action: releasecontrol.ClaimAttempt, State: tc.state, ExpectVersion: control.Version, Reason: "public readiness test", Operator: "test"})
			if err != nil {
				t.Fatal(err)
			}
			checker.err, checker.checks = tc.nav, 0
			req := httptest.NewRequest(http.MethodGet, "/api/smart-accounts/earn-max/withdrawal-claim-readiness", nil)
			if tc.signedIn {
				req.Header.Set("Cookie", walletSessionCookie+"="+token)
			}
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("got %d %s", res.Code, res.Body.String())
			}
			if tc.status == 200 && !strings.Contains(res.Body.String(), `"ready":true`) {
				t.Fatal("public claim was not admitted")
			}
			if (!tc.signedIn || tc.state == releasecontrol.Held) && checker.checks != 0 {
				t.Fatal("price checked before admission")
			}
		})
	}
}
