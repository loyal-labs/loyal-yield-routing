package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnstats"
)

// This opt-in test owns a fresh local database; it never accepts a shared host.
func TestEarnStatsHTTPReadOnly(t *testing.T) {
	dsn := os.Getenv("LOYAL_WEB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable local Postgres cluster")
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/tmp/loyal-web-pg-") || config.ConnConfig.Database != "loyal_web_pilot" {
		t.Fatal("test requires the loyal_web_pilot database on a /tmp/loyal-web-pg-* Unix socket")
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, `
CREATE TABLE public.stats_fixture (snapshot_key text PRIMARY KEY, total_aum_raw bigint, total_optimized_volume_raw bigint, total_users integer, refreshed_at timestamptz, earn_aum_series jsonb);
CREATE FUNCTION public.assert_stats_read_only() RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
BEGIN
  IF current_setting('transaction_read_only') <> 'on' OR current_setting('statement_timeout') <> '10s' THEN
    RAISE EXCEPTION 'unsafe reader transaction';
  END IF;
  RETURN true;
END $$;
CREATE VIEW public.loyal_stats_snapshots AS SELECT * FROM public.stats_fixture WHERE public.assert_stats_read_only();
INSERT INTO public.stats_fixture VALUES ('current', 100000001, 999999999, 12, now(), '[{"aumRaw":"50000000","weekStart":"2026-10-05","weekEnd":"2026-10-11"},{"aumRaw":"40000001","weekStart":"2026-10-12","weekEnd":"2026-10-18"}]');`)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test-Upstream", "yes")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	handler := New(Config{AppDB: pool, Upstream: upstreamURL})
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	response := get(handler, "/api/earn/stats")
	if response.Code != http.StatusOK || response.Header().Get("X-Loyal-API-Source") != "go" {
		t.Fatalf("Go read failed: %d %s", response.Code, response.Body.String())
	}
	var stats earnstats.EarnPublicStats
	if err := json.Unmarshal(response.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.AumUSD != 100.000001 || stats.AumDeltaVsPriorWeekUSD == nil || *stats.AumDeltaVsPriorWeekUSD != -9.999999 {
		t.Fatalf("incorrect amount conversion: %+v", stats)
	}
	if strings.Contains(response.Body.String(), `"$schema"`) || response.Header().Get("Cache-Control") != "public, max-age=60, s-maxage=300, stale-while-revalidate=3600" {
		t.Fatalf("Next response contract changed: %s %s", response.Header(), response.Body.String())
	}
	var readOnly, timeout string
	if err := pool.QueryRow(ctx, "SELECT current_setting('transaction_read_only'), current_setting('statement_timeout')").Scan(&readOnly, &timeout); err != nil {
		t.Fatal(err)
	}
	if readOnly != "off" || timeout != "0" {
		t.Fatalf("session state leaked: readOnly=%s timeout=%s", readOnly, timeout)
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/auth/session"},
		{http.MethodPost, "/api/smart-accounts/mobile/earn/state"},
		{http.MethodPost, "/api/smart-accounts/mobile/earn-max"},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(route.method, route.path, nil))
		if w.Code != http.StatusAccepted || w.Header().Get("X-Test-Upstream") != "yes" {
			t.Fatalf("unmigrated API stopped forwarding: %s %s %d", route.method, route.path, w.Code)
		}
	}
	if w := get(New(Config{Upstream: upstreamURL}), "/api/earn/stats"); w.Code != http.StatusAccepted {
		t.Fatal("credential-free deployment must keep forwarding stats")
	}
	for _, query := range []string{
		"UPDATE public.stats_fixture SET refreshed_at = now() - interval '6 minutes'",
		"DELETE FROM public.stats_fixture",
		"DROP VIEW public.loyal_stats_snapshots",
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
		w := get(handler, "/api/earn/stats")
		if w.Code != http.StatusServiceUnavailable || strings.TrimSpace(w.Body.String()) != `{"error":{"code":"earn_stats_unavailable","message":"Earn stats are unavailable."}}` || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("missing/stale/broken DB must fail closed: %d %s", w.Code, w.Body.String())
		}
	}
}
