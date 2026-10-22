package webapp

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func TestEveryGoAPIResponseNamesItsSource(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://nobody:nothing@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	rpc, err := chain.New("http://127.0.0.1:1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuth("secret-for-the-test-only", "", pool, rpc)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	handler := New(Config{
		Upstream: upstreamURL, AppDB: pool, Auth: auth, EarnMax: &EarnMax{}, PushDB: true,
		Dist: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}},
	})
	for _, tc := range []struct {
		method, path string
		status       int
		source       string
	}{
		{http.MethodGet, "/api/openapi.json", http.StatusOK, "go"},
		{http.MethodGet, "/api/smart-accounts/earn-max/summary", http.StatusUnauthorized, "go"},
		{http.MethodGet, "/api/smart-accounts/earn-max/activity", http.StatusUnauthorized, "go"},
		{http.MethodPost, "/api/push-tokens", http.StatusUnauthorized, "go"},
		{http.MethodGet, "/api/auth/session", http.StatusAccepted, ""},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(w, req)
		if w.Code != tc.status || w.Header().Get("X-Loyal-API-Source") != tc.source {
			t.Errorf("%s %s = %d with X-Loyal-API-Source %q, want %d with %q", tc.method, tc.path, w.Code, w.Header().Get("X-Loyal-API-Source"), tc.status, tc.source)
		}
	}
}
