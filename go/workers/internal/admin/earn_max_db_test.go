package admin

import (
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEarnMaxHealthUsesSignedStateAndReportedWorker(t *testing.T) {
	dsn := os.Getenv("EARN_MAX_HEALTH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set EARN_MAX_HEALTH_TEST_DATABASE_URL to a fresh local earn_max_health_test database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || config.ConnConfig.Host != "127.0.0.1" || config.ConnConfig.Database != "earn_max_health_test" {
		t.Fatal("test requires a disposable local earn_max_health_test database")
	}
	ctx := t.Context()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	defer func() {
		if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS loyal_yield CASCADE`); err != nil {
			t.Error(err)
		}
	}()
	_, err = pool.Exec(ctx, `CREATE SCHEMA loyal_yield;
CREATE TABLE loyal_yield.multiply_route_states (route_key text PRIMARY KEY, state jsonb, lease_owner text, updated_at timestamptz DEFAULT now());
CREATE TABLE loyal_yield.backyard_lane_levels (route_key text, lane text, level double precision, decided_at timestamptz, up_approved boolean);
CREATE TABLE loyal_yield.multiply_operations (route_key text, action text, status text, recovery_reason text, transaction_signature text, expected_effects jsonb, created_at timestamptz DEFAULT now());`)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := earnMaxConfigFor("loyal")
	_, err = pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states (route_key, state) VALUES ($1,
'{"observation":{"workerOwner":"worker:backyard:rwa:sha-579af446b0fdcb783e9befb366660342647ed5b4","navKnown":true,"navFresh":false,"computedStrategyNavRaw":"10000000","aumUsdMicros":"12000000","ltvBps":3300}}')`, cfg.RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations (route_key,action,status,recovery_reason,transaction_signature) VALUES
($1,'REPORT_NAV','failed','hold:stale',NULL),
($1,'REPORT_NAV','failed','hold:chain_refused',NULL),
($1,'REPORT_NAV','failed','signature_absent_after_blockhash_expiry','signature'),
($1,'REPORT_NAV','reconciled',NULL,'landed'),
($1,'REPORT_NAV','decided',NULL,NULL),
('other-vault','REPORT_NAV','failed','hold:stale',NULL),
($1,'DELEVER_ROUTE_STEP','failed','hold:stale',NULL)`, cfg.RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	d := &Deps{Yield: pool}
	health, err := earnMaxLoadHealth(ctx, d, cfg)
	if err != nil || health.NavAll != 5 || health.NavReconciled != 1 || health.NavFailed != 3 || health.NavRefusedBeforeSending != 2 || health.RefusedBeforeSending != 1 {
		t.Fatalf("health: %+v, %v", health, err)
	}
	for _, reason := range health.NavFailureReasons {
		if reason.BeforeSending != (reason.Reason != "signature_absent_after_blockhash_expiry") {
			t.Fatalf("wrong signature classification: %+v", reason)
		}
	}
	route, err := earnMaxLoadRoute(ctx, d, cfg)
	if err != nil || route.Version == nil || *route.Version != "579af44" || route.LeaseOwner != nil || route.WorkerOwner == nil || route.NavUsd == nil || *route.NavUsd != 10 || route.NavFresh == nil || *route.NavFresh {
		t.Fatalf("known valuation awaiting publication: %+v, %v", route, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{observation,navKnown}','false')`); err != nil {
		t.Fatal(err)
	}
	route, err = earnMaxLoadRoute(ctx, d, cfg)
	if err != nil || route.NavUsd != nil || route.AumUsd != nil {
		t.Fatalf("unknown valuation reported as current: %+v, %v", route, err)
	}
}
