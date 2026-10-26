package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
)

func TestEarningsPreservesSessionRejectionForMobileRenewal(t *testing.T) {
	keys, err := jwt.NewKeys("isolated-session-test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	api := humachi.New(router, apiConfig())
	h := earnRoutes{auth: &Auth{keys: keys}}
	h.registerEarnings(api)
	for _, tc := range []struct{ authorization, code string }{
		{"Bearer rejected-session", "invalid_mobile_session"},
		{"", "unauthenticated"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/smart-accounts/yield-optimization/earnings", nil)
		req.Header.Set("Authorization", tc.authorization)
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
		if res.Code != http.StatusUnauthorized || body.Error.Code != tc.code {
			t.Fatalf("session rejection = %d %s; want 401 %s", res.Code, res.Body.String(), tc.code)
		}
	}
}
