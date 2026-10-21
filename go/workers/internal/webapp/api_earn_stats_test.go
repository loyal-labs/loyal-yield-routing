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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEarnStatsSubtractBeforeConvertingToUSD(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	snapshot := earnStatsSnapshot{refreshedAt: now, series: json.RawMessage(`[
 {"aumRaw":"9007199254740993","weekStart":"2026-10-05","weekEnd":"2026-10-11"},
 {"aumRaw":"9007199254740992","weekStart":"2026-10-12","weekEnd":"2026-10-18"}
]`)}
	stats, err := snapshot.publicStats(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.AumDeltaVsPriorWeekUSD == nil || *stats.AumDeltaVsPriorWeekUSD != -0.000001 {
		t.Fatalf("lost the one-unit decrease above JS safe integer range: %+v", stats)
	}
	if stats.AumSeries[0].Value != stats.AumSeries[1].Value {
		t.Fatal("fixture must exercise values that round to the same JS number")
	}
}

func TestEarnStatsRejectStaleAndInvalidSnapshots(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, series string
		age          time.Duration
		fail         bool
	}{
		{"five minutes remains fresh", "[]", 5 * time.Minute, false},
		{"one millisecond past expiry", "[]", 5*time.Minute + time.Millisecond, true},
		{"future timestamp", "[]", -time.Second, false},
		{"missing series", "null", 0, true},
		{"invalid amount", `[{"aumRaw":"unknown","weekStart":"2026-10-05","weekEnd":"2026-10-11"}]`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := (earnStatsSnapshot{refreshedAt: now.Add(-tc.age), series: json.RawMessage(tc.series)}).publicStats(now)
			if (err != nil) != tc.fail {
				t.Fatalf("stats=%+v error=%v", stats, err)
			}
			if !tc.fail && (stats.AumDeltaVsPriorWeekUSD != nil || stats.AumSeries == nil) {
				t.Fatal("an empty history must expose an unknown delta and an empty array")
			}
		})
	}
}

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
	var stats EarnPublicStats
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
