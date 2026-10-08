package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/kamino"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	catalogTestMainMarket = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	catalogTestUSDC       = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	catalogTestReserve    = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"
	catalogTestSmaller    = "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"
)

// kaminoMetricsAPI serves /kamino-market/{market}/reserves/metrics with the
// current USDC reserve set of the main market and no reserves elsewhere.
type kaminoMetricsAPI struct{ largest atomic.Value }

func (a *kaminoMetricsAPI) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if !strings.HasPrefix(request.URL.Path, "/kamino-market/") || !strings.HasSuffix(request.URL.Path, "/reserves/metrics") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	metrics := []map[string]any{}
	if strings.Contains(request.URL.Path, catalogTestMainMarket) {
		largest := a.largest.Load().(string)
		smaller := catalogTestSmaller
		if largest == catalogTestSmaller {
			smaller = catalogTestReserve
		}
		metrics = append(metrics,
			map[string]any{"reserve": largest, "liquidityToken": "usdc", "liquidityTokenMint": catalogTestUSDC, "totalSupplyUsd": "250000000"},
			map[string]any{"reserve": smaller, "liquidityToken": "USDC", "liquidityTokenMint": catalogTestUSDC, "totalSupplyUsd": 10.0},
			map[string]any{"reserve": "So11111111111111111111111111111111111111112", "liquidityToken": "SOL", "liquidityTokenMint": "So11111111111111111111111111111111111111112", "totalSupplyUsd": 9e9})
	}
	_ = json.NewEncoder(w).Encode(metrics)
}

// Production 2026-10-08: Rust renewed kamino.supported_reserves.fetched_at
// every 120 s (last at 01:45:14.95). The Go observer only read the catalog,
// so after the 01:46:15 swap every reserve's catalog expiry froze at
// 01:50:14.95; at 01:49:14.95 its remaining lifetime fell under the planner's
// 60 s minimum, no mint stayed complete, the optimizer envelope collapsed to
// its observation time and the Rust planner died at 01:49:18. The observer
// must own that renewal, as the Rust monitor did.
func TestObserverRenewsSupportedReserveCatalogThePlannerAdmits(t *testing.T) {
	databaseURL := os.Getenv("OBSERVER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OBSERVER_TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := fmt.Sprintf("kamino_catalog_%d", time.Now().UnixNano())
	// supported_reserves is the registered 0001 Timescale DDL; the verified
	// view is a table with the columns both planners select.
	if _, err = pool.Exec(ctx, fmt.Sprintf(`
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.supported_reserves (
 market TEXT NOT NULL, liquidity_mint TEXT NOT NULL, reserve TEXT NOT NULL, market_name TEXT, symbol TEXT,
 risk_baskets TEXT[] NOT NULL DEFAULT '{}', source TEXT NOT NULL DEFAULT 'kamino-api', active BOOLEAN NOT NULL DEFAULT TRUE,
 fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(), created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY (market, liquidity_mint));
CREATE TABLE %[1]s.latest_verified_reserve_updates(
 event_id bigint,account_data_hash text,observed_at timestamptz,slot bigint,verified_at timestamptz,verified_slot bigint,
 verification_commitment text,verification_source text,reserve text,market text,market_name text,liquidity_mint text,symbol text,mint_decimals integer,
 reserve_last_update_slot bigint,reserve_last_update_stale boolean,reserve_price_status smallint,available_amount double precision,
 borrowed_amount double precision,total_supply_amount double precision,market_price_usd double precision,market_price_last_updated_ts bigint,
 utilization double precision,borrow_apy double precision,supply_apy double precision)`, schema)); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")

	// State at the swap: the catalog Rust last published 250 s ago, and a
	// reserve the observer has just verified.
	now := time.Now().UTC()
	rustFetchedAt := now.Add(-250 * time.Second).Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.supported_reserves(market,liquidity_mint,reserve,market_name,symbol,risk_baskets,source,active,fetched_at) VALUES($1,$2,$3,'Main Market','USDC',ARRAY['safe','medium','aggressive'],'kamino-api',true,$4)`, schema), catalogTestMainMarket, catalogTestUSDC, catalogTestReserve, rustFetchedAt); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.latest_verified_reserve_updates VALUES(1,$1,$2,999,$2,1000,'confirmed','http_confirmed_refresh',$3,$4,'Main Market',$5,'USDC',6,998,false,0,1800000000000,200000000000,2000000000000,1,1700000000,.1,.01,.008)`, schema), strings.Repeat("a", 64), now, catalogTestReserve, catalogTestMainMarket, catalogTestUSDC); err != nil {
		t.Fatal(err)
	}
	planner, err := fleet.OpenMarketEvidenceStore(ctx, databaseURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer planner.Close()
	if err := planner.SetEnabledMints([]string{catalogTestUSDC}); err != nil {
		t.Fatal(err)
	}
	if epoch, loadErr := planner.LoadImmutableMarketEpoch(ctx); loadErr == nil && epoch.Validate() == nil {
		t.Fatalf("a catalog with 50 s left produced a usable epoch: %+v", epoch.MintCoverage)
	}

	api := &kaminoMetricsAPI{}
	api.largest.Store(catalogTestReserve)
	server := httptest.NewServer(api)
	defer server.Close()
	runtime := &Runtime{
		cfg:           config.Config{CatalogRefresh: 20 * time.Millisecond, ProgressTimeout: 10 * time.Second},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		facts:         engine.NewFacts(prometheus.NewRegistry()),
		kaminoCatalog: kamino.NewCatalogClient(server.URL, 5*time.Second),
		kaminoStore:   kamino.NewStore(pool, schema),
	}
	refreshCtx, stopRefresh := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); runtime.refreshSupportedReserveCatalog(refreshCtx) }()
	fetchedAt := rustFetchedAt
	for deadline := time.Now().Add(5 * time.Second); !fetchedAt.After(rustFetchedAt) && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT fetched_at FROM %s.supported_reserves WHERE market=$1 AND liquidity_mint=$2`, schema), catalogTestMainMarket, catalogTestUSDC).Scan(&fetchedAt); err != nil {
			t.Fatal(err)
		}
	}
	stopRefresh()
	<-done
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT fetched_at FROM %s.supported_reserves WHERE market=$1 AND liquidity_mint=$2`, schema), catalogTestMainMarket, catalogTestUSDC).Scan(&fetchedAt); err != nil {
		t.Fatal(err)
	}
	if !fetchedAt.After(now.Add(-time.Second)) {
		t.Fatalf("observer left the catalog at fetched_at=%s; the planner admits it only until %s", fetchedAt, fetchedAt.Add(300*time.Second))
	}
	var active int
	var reserve, symbol, source string
	var baskets []string
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) OVER (),reserve,symbol,source,risk_baskets FROM %s.supported_reserves WHERE active`, schema)).Scan(&active, &reserve, &symbol, &source, &baskets); err != nil {
		t.Fatal(err)
	}
	if active != 1 || reserve != catalogTestReserve || symbol != "USDC" || source != "kamino-api" || strings.Join(baskets, ",") != "safe,medium,aggressive" {
		t.Fatalf("renewed catalog changed identity: active=%d reserve=%s symbol=%s source=%s baskets=%v", active, reserve, symbol, source, baskets)
	}
	epoch, err := planner.LoadImmutableMarketEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := epoch.Validate(); err != nil {
		t.Fatalf("planner rejected the renewed catalog: %v (%+v)", err, epoch.MintCoverage)
	}
	if lifetime := epoch.OptimizerEnvelopeExpiresAt().Sub(epoch.CapturedAt); lifetime < 4*time.Minute-5*time.Second {
		t.Fatalf("optimizer envelope lifetime = %s, want the verification-bound ~240 s", lifetime)
	}

	// A topology change is the operator's explicit sync, never a silent
	// renewal: the committed identity and its timestamp stay as they were.
	api.largest.Store(catalogTestSmaller)
	if _, err := runtime.publishSupportedReserves(ctx); err == nil || !strings.Contains(err.Error(), "decoding topology change") {
		t.Fatalf("changed reserve identity was published: %v", err)
	}
	var after time.Time
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT reserve,fetched_at FROM %s.supported_reserves WHERE active`, schema)).Scan(&reserve, &after); err != nil {
		t.Fatal(err)
	}
	if reserve != catalogTestReserve || !after.Equal(fetchedAt) {
		t.Fatalf("rejected refresh mutated the catalog: reserve=%s fetched_at=%s", reserve, after)
	}
}
