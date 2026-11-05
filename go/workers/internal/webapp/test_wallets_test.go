package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
	"github.com/solana-foundation/solana-go/v2"
)

func TestBridgeAndReferralsAdmitOnlyTestWallets(t *testing.T) {
	keys, err := jwt.NewKeys("isolated-session-test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jwt.NewSigner("isolated-session-test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	bearer := func(wallet solana.PublicKey) string {
		token, err := signer.Mint(map[string]any{"sub": wallet.String()}, mobileEarnAudience, time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + token
	}
	routes := earnRoutes{auth: &Auth{keys: keys}}
	router := chi.NewRouter()
	api := humachi.New(router, apiConfig())
	routes.registerBridge(api)
	routes.registerTronDeposit(api)
	routes.registerReferrals(api)

	stranger := solana.NewWallet().PublicKey()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, bridgeBalancesPath},
		{http.MethodPost, tronDepositPath},
		{http.MethodGet, referralsPath},
		{http.MethodPost, referralClaimPath},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", bearer(stranger))
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s: %v: %s", route.method, route.path, err, res.Body.String())
		}
		if res.Code != http.StatusForbidden || body.Error.Code != "not_open" {
			t.Errorf("%s %s for a stranger = %d %s; want 403 not_open", route.method, route.path, res.Code, res.Body.String())
		}
	}

	for _, address := range ownerTestWallets {
		wallet := solana.MustPublicKeyFromBase58(address)
		got, err := routes.evmDepositWallet(SessionInput{Authorization: bearer(wallet)})
		if err != nil || got != wallet {
			t.Errorf("test wallet %s = %s, %v; want admitted", address, got, err)
		}
	}
}
