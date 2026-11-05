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
	routes := earnRoutes{auth: &Auth{keys: keys}, Earn: Earn{TestWallets: parseTestWallets(strings.Join(ownerTestWallets, ","))}}
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

func TestLaunchAnswersTheGateForTheSessionWalletOrEveryone(t *testing.T) {
	signer, err := jwt.NewSigner("isolated-session-test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := jwt.NewKeys("isolated-session-test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	listed := solana.MustPublicKeyFromBase58(ownerTestWallets[0])
	stranger := solana.NewWallet().PublicKey()
	for _, tc := range []struct {
		name      string
		list      string
		wallet    solana.PublicKey
		signedOut bool
		want      bool
	}{
		{"listed wallet", strings.Join(ownerTestWallets, ","), listed, false, true},
		{"unlisted wallet", strings.Join(ownerTestWallets, ","), stranger, false, false},
		{"star opens every wallet", "*", stranger, false, true},
		{"signed out under the default list", strings.Join(ownerTestWallets, ","), solana.PublicKey{}, true, false},
		{"signed out under star", "*", solana.PublicKey{}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := chi.NewRouter()
			earnRoutes{auth: &Auth{keys: keys}, Earn: Earn{TestWallets: parseTestWallets(tc.list)}}.registerLaunch(humachi.New(router, apiConfig()))
			req := httptest.NewRequest(http.MethodGet, launchPath, nil)
			if !tc.signedOut {
				token, err := signer.Mint(map[string]any{"sub": tc.wallet.String()}, mobileEarnAudience, time.Hour, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
			}
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			var body LaunchResponse
			if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil || res.Code != http.StatusOK {
				t.Fatalf("GET %s = %d %s: %v", launchPath, res.Code, res.Body.String(), err)
			}
			if body.Launched != tc.want {
				t.Errorf("launched = %v, want %v", body.Launched, tc.want)
			}
		})
	}
}
