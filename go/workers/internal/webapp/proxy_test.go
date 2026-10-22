package webapp

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestProxyKeepsWalletSignInBoundToPublicOrigin(t *testing.T) {
	for _, publicHost := range []string{"next.askloyal.com", "app.askloyal.com"} {
		for _, tc := range []struct {
			path          string
			origin        string
			rewriteOrigin bool
		}{
			{"/api/auth/wallet/challenge", "https://" + publicHost, false},
			{"/api/auth/wallet/complete", "https://" + publicHost, false},
			{"/api/observability/events", "https://" + publicHost, true},
			{"/api/observability/events", "https://untrusted.example", false},
			{"/api/auth/wallet/challenge", "https://untrusted.example", false},
		} {
			t.Run(publicHost+tc.path+tc.origin, func(t *testing.T) {
				var upstreamHost string
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					wantOrigin := tc.origin
					if tc.rewriteOrigin {
						wantOrigin = "http://" + upstreamHost
					}
					if got := r.Header.Get("Origin"); got != wantOrigin {
						t.Errorf("backend Origin = %q, want %q", got, wantOrigin)
					}
					if r.Host != upstreamHost || r.Header.Get("X-Forwarded-Host") != publicHost {
						t.Errorf("routing hosts: Host=%q X-Forwarded-Host=%q", r.Host, r.Header.Get("X-Forwarded-Host"))
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				defer backend.Close()
				upstream, err := url.Parse(backend.URL)
				if err != nil {
					t.Fatal(err)
				}
				upstreamHost = upstream.Host
				req := httptest.NewRequest(http.MethodPost, "https://"+publicHost+tc.path, nil)
				req.Header.Set("Origin", tc.origin)
				response := httptest.NewRecorder()
				forward(upstream).ServeHTTP(response, req)
				if response.Code != http.StatusNoContent {
					t.Fatalf("proxy status = %d", response.Code)
				}
			})
		}
	}
}
