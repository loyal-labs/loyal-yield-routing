package webapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
)

func TestPrivyCookieUsesRequestHostWithoutForwardedHost(t *testing.T) {
	t.Setenv("VERCEL_ENV", "production")
	t.Setenv("NEXT_PUBLIC_APP_ENVIRONMENT", "prod")
	router := chi.NewRouter()
	api := humachi.New(router, apiConfig())
	huma.Register(api, huma.Operation{OperationID: "privy-cookie-host", Method: http.MethodPost, Path: privyCompletePath},
		func(ctx context.Context, in *privyCompleteInput) (*privyCompleteOutput, error) {
			cookie, err := privySessionCookieHeader([]string{"askloyal.com"}, walletSessionCookie, "test-session", 60,
				in.ForwardedProto, in.ForwardedHost, in.SessionInput.host, in.scheme)
			if err != nil {
				return nil, huma.Error403Forbidden("cookie host refused")
			}
			return &privyCompleteOutput{SetCookie: cookie}, nil
		})
	for _, host := range []string{"app.askloyal.com", "attacker.example"} {
		t.Run(host, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, privyCompletePath, strings.NewReader(`{}`))
			r.Host = host
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Forwarded-Proto", "https")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			cookies := w.Result().Cookies()
			if host == "attacker.example" {
				if w.Code != http.StatusForbidden || len(cookies) != 0 {
					t.Fatalf("untrusted host: status=%d cookies=%d", w.Code, len(cookies))
				}
				return
			}
			if w.Code != http.StatusOK || len(cookies) != 1 {
				t.Fatalf("valid host: status=%d cookies=%d body=%s", w.Code, len(cookies), w.Body.String())
			}
			cookie := cookies[0]
			if cookie.Domain != "askloyal.com" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatal("session cookie lost its domain or security attributes")
			}
		})
	}
}
