package admin

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"
)

// Run against a fresh disposable database, never the shared Yield database.
func TestEarnMaxReadIsolation(t *testing.T) {
	dsn := os.Getenv("ADMIN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable ADMIN_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.Host != "127.0.0.1" || config.ConnConfig.Database != "admin_vault_test" {
		t.Fatal("requires local disposable admin_vault_test database")
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `CREATE SCHEMA loyal_yield;
 CREATE TABLE loyal_yield.multiply_route_states (route_key text PRIMARY KEY, state jsonb, lease_owner text, updated_at timestamptz);
 CREATE TABLE loyal_yield.multiply_position_snapshots (id bigserial, route_key text, strategy_key text, collateral_raw numeric, debt_raw numeric, equity_usd_micros numeric, collateral_value_usd_micros numeric, debt_value_usd_micros numeric, ltv_bps int, forecast_apy_bps int, observed_at timestamptz);
 CREATE TABLE loyal_yield.multiply_operations (operation_id text, route_key text, action text, status text, engine_version text, expected_effects jsonb, transaction_signature text, recovery_reason text, created_at timestamptz, strategy_key text);
 CREATE TABLE loyal_yield.backyard_manual_recovery_latches (route_key text, reason text, latched_at timestamptz, cleared_at timestamptz);
 CREATE TABLE loyal_yield.backyard_lane_levels (route_key text, lane text, level numeric, decided_at timestamptz, up_approved boolean);`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP SCHEMA loyal_yield CASCADE`) })
	rpcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID any `json:"id"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("bad RPC request")
		}
		values := []any{}
		for _, decimals := range []byte{9, 6} {
			mint := make([]byte, 82)
			mint[44], mint[45] = decimals, 1
			values = append(values, map[string]any{"lamports": 1, "owner": solana.TokenProgramID.String(), "data": []string{base64.StdEncoding.EncodeToString(mint), "base64"}, "executable": false, "rentEpoch": 0})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"context": map[string]any{"slot": 100}, "value": values}})
	}))
	defer rpcServer.Close()
	t.Setenv("LOYAL_ADMIN_SOLANA_RPC_URL", rpcServer.URL)
	d := &Deps{Yield: pool}
	configs := []earnMaxConfig{}
	const lane = "OnRe/ONyc/USDC"
	for i, id := range []string{"loyal", "backyard"} {
		cfg, err := earnMaxConfigFor(id)
		if err != nil {
			t.Fatal(err)
		}
		configs = append(configs, cfg)
		nav := int64(100_000_000 + i*7_000_000)
		state := map[string]any{"engineVersion": "backyard_rwa_v1", "currentApy": map[string]any{"lane": lane, "apyBps": 1200 + i}, "observation": map[string]any{"navKnown": true, "navRaw": fmt.Sprint(nav), "computedStrategyNavRaw": fmt.Sprint(nav), "aumUsdMicros": fmt.Sprint(nav + 3_000_000), "positionDebtValueRaw": fmt.Sprint(i * 1_000_000), "positionCollateralValueRaw": "120000000", "ltvBps": i * 100, "reportObservedAt": time.Now().UTC().Format(time.RFC3339), "voltrIdleRaw": "3000000"}}
		raw, _ := json.Marshal(state)
		_, err = pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states VALUES ($1,$2,'worker:test',now())`, cfg.RouteKey, raw)
		if err != nil {
			t.Fatal(err)
		}
		_, err = pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_position_snapshots (route_key,strategy_key,collateral_raw,debt_raw,equity_usd_micros,collateral_value_usd_micros,debt_value_usd_micros,ltv_bps,observed_at) VALUES ($1,$2,8623340000,0,$3,120000000,0,$4,now())`, cfg.RouteKey, lane, nav, i*100)
		if err != nil {
			t.Fatal(err)
		}
		_, err = pool.Exec(ctx, `INSERT INTO loyal_yield.backyard_lane_levels VALUES ($1,$2,$3,now(),false)`, cfg.RouteKey, lane, 1+float64(i)*0.75)
		if err != nil {
			t.Fatal(err)
		}
		report := base64.StdEncoding.EncodeToString(binary.LittleEndian.AppendUint64(nil, uint64(nav)))
		effects, _ := json.Marshal(map[string]any{"expectedEffects": map[string]any{"returnData": map[string]any{"programId": cfg.Accounts.AdaptorProgram.String(), "dataBase64": report}}, "decision": map[string]any{"amountRaw": fmt.Sprint(nav)}})
		for _, action := range []string{"REPORT_NAV", "SWAP_STABLE_TO_COLLATERAL_STEP"} {
			_, err = pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations (operation_id, route_key, action, status, engine_version, expected_effects, transaction_signature, recovery_reason, created_at) VALUES ($1,$2,$3,'reconciled','backyard_rwa_v1',$4,$5,NULL,now())`, id+action, cfg.RouteKey, action, effects, id+"-signature")
			if err != nil {
				t.Fatal(err)
			}
		}
		_, err = pool.Exec(ctx, `INSERT INTO loyal_yield.backyard_manual_recovery_latches VALUES ($1,$2,now(),NULL)`, cfg.RouteKey, id+"-hold")
		if err != nil {
			t.Fatal(err)
		}
	}
	for round := 0; round < 2; round++ {
		_, err = pool.Exec(ctx, `UPDATE loyal_yield.multiply_position_snapshots SET observed_at=now()+interval '1 second' WHERE route_key=$1`, configs[round].RouteKey)
		if err != nil {
			t.Fatal(err)
		}
		for i, cfg := range configs {
			want := float64(100 + i*7)
			route, err := earnMaxLoadRoute(ctx, d, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if route.NavUsd == nil || *route.NavUsd != want || route.LeverageTarget == nil || *route.LeverageTarget != 1+float64(i)*0.75 {
				t.Fatalf("%s route selected other vault: %+v", cfg.identity.ID, route)
			}
			if route.AumUsd == nil || *route.AumUsd != want+3 || route.DebtUsd == nil || *route.DebtUsd != float64(i) || route.CollateralUsd == nil || *route.CollateralUsd != 120 || route.LtvRatio == nil || *route.LtvRatio != float64(i)/100 {
				t.Fatalf("%s valuation units are incorrect: %+v", cfg.identity.ID, route)
			}
			details, err := loadBackyard(ctx, d, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !details.Available || details.NavUsdMicros == nil || *details.NavUsdMicros != fmt.Sprint(int(want)*1_000_000) || details.Settings[0].Value != cfg.identity.Address {
				t.Fatalf("%s details selected other vault: %+v", cfg.identity.ID, details)
			}
			if details.TokenError != nil || details.CurrentPosition.CollateralSymbol != "ONyc" || details.CurrentPosition.CollateralDecimals == nil || *details.CurrentPosition.CollateralDecimals != 9 {
				t.Fatalf("wrong token metadata: %+v", details)
			}
			series, err := earnMaxLoadNavSeries(ctx, d, cfg)
			if err != nil || len(series) != 1 || series[0].NavUsd != want {
				t.Fatalf("cross-vault NAV series: %+v %v", series, err)
			}
			moves, err := earnMaxLoadMoves(ctx, d, cfg)
			if err != nil || len(moves) != 1 || *moves[0].Signature != cfg.identity.ID+"-signature" {
				t.Fatalf("cross-vault moves: %+v %v", moves, err)
			}
			latches, err := earnMaxLoadLatches(ctx, d, cfg)
			if err != nil || len(latches.Open) != 1 || latches.Open[0].Reason != cfg.identity.ID+"-hold" {
				t.Fatalf("cross-vault latches: %+v %v", latches, err)
			}
			history, err := earnMaxLoadHistory(ctx, d, cfg)
			if err != nil || len(history) != 1 || *history[0].EquityUsd != want {
				t.Fatalf("cross-vault history: %+v %v", history, err)
			}
			health, err := earnMaxLoadHealth(ctx, d, cfg)
			if err != nil || health.NavReconciled != 1 {
				t.Fatalf("cross-vault health: %+v %v", health, err)
			}
		}
	}
	_, err = pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{observation,navKnown}','false') WHERE route_key=$1`, configs[0].RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	route, err := earnMaxLoadRoute(ctx, d, configs[0])
	if err != nil || route.AumUsd != nil || route.NavUsd != nil || route.NavFresh == nil || *route.NavFresh {
		t.Fatalf("unpriced valuation was presented as current: %+v %v", route, err)
	}
}
