package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
)

func TestEvmDepositPostChecksSessionBeforeBody(t *testing.T) {
	keys, err := jwt.NewKeys("isolated-session-test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	api := humachi.New(router, apiConfig())
	earnRoutes{auth: &Auth{keys: keys}}.registerEvmDeposit(api)
	req := httptest.NewRequest(http.MethodPost, evmDepositPath, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
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
	if res.Code != http.StatusUnauthorized || body.Error.Code != "unauthenticated" {
		t.Fatalf("unauthenticated POST with empty claim = %d %s; want 401 unauthenticated", res.Code, res.Body.String())
	}
}
