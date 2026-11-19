package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestReferralIdentity(t *testing.T) {
	if id, err := (earnRoutes{}).referralIdentity(context.Background(), "native"); err != nil || id.Subject != "" {
		t.Fatalf("wallet-only installation: %+v %v", id, err)
	}
	const wallet = "11111111111111111111111111111111"
	const alias = "So11111111111111111111111111111111111111112"
	const proof = `{"id":"did:privy:fixture","linked_accounts":[
{"type":"wallet","chain_type":"solana","address":"%s","verified_at":5},
{"type":"wallet","chain_type":"solana","address":"%s","verified_at":6},
{"type":"wallet","chain_type":"solana","address":"unverified","verified_at":0},
{"type":"wallet","chain_type":"ethereum","address":"0x1","verified_at":2},
{"type":"email","address":"unknown","verified_at":7}]}`
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantKnown bool
		wantErr   bool
	}{
		{"two linked Solana wallets", 200, fmt.Sprintf(proof, wallet, alias), true, false},
		{"no matching wallet", 404, `{"error":"user not found"}`, false, false},
		{"provider rate limit", 429, `{"error":"rate limited"}`, false, true},
		{"inconsistent returned wallet", 200, fmt.Sprintf(proof, alias, alias), false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Address string `json:"address"`
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/users/wallet/address" ||
					r.Header.Get("privy-app-id") != "test-app" || json.NewDecoder(r.Body).Decode(&req) != nil || req.Address != wallet {
					t.Errorf("unexpected provider lookup %s %s, address=%q", r.Method, r.URL.Path, req.Address)
				}
				id, secret, ok := r.BasicAuth()
				if !ok || id != "test-app" || secret != "fixture" {
					t.Error("app authentication missing")
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			prev := privyAPIBase
			privyAPIBase = server.URL + "/v1"
			defer func() { privyAPIBase = prev }()
			h := earnRoutes{Earn: Earn{HTTP: server.Client(), Privy: &Privy{AppID: "test-app", AppSecret: "fixture"}}}
			proof, err := h.referralIdentity(context.Background(), wallet)
			if (err != nil) != test.wantErr || (proof.Subject != "") != test.wantKnown {
				t.Fatalf("identity=%+v err=%v", proof, err)
			}
			if test.wantKnown && !slices.Equal(proof.Wallets, []string{wallet, alias}) {
				t.Fatalf("verified wallet set=%v", proof.Wallets)
			}
		})
	}
}
