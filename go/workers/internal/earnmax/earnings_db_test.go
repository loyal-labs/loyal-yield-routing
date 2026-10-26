package earnmax

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Opt-in, disposable local Postgres only. Never point this at a shared database.
func TestExitLiabilityUsesConfirmedReportAtEachMark(t *testing.T) {
	dsn := os.Getenv("EARNMAX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set EARNMAX_TEST_DATABASE_URL to a disposable local database")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != "127.0.0.1" || !strings.HasPrefix(config.Database, "earnmax_test") {
		t.Fatal("requires a disposable local earnmax_test database")
	}
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `CREATE SCHEMA loyal_yield;
 CREATE TABLE loyal_yield.multiply_operations(route_key text, status text, action text, confirmed_slot bigint, updated_at timestamptz, expected_effects jsonb, created_at timestamptz, operation_id text)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		slot                   int
		status, route, effects string
	}{
		{10, "reconciled", RouteKey, `{"expectedEffects":{"returnData":"legacy"}}`},
		{20, "reconciled", RouteKey, `{"expectedEffects":{"returnData":"net-nav","exitLiabilityRaw":"20000000"}}`},
		{30, "submitted", RouteKey, `{"expectedEffects":{"returnData":"net-nav","exitLiabilityRaw":"999"}}`},
		{60, "reconciled", RouteKey, `{}`},
		{40, "reconciled", "unrelated", `{"expectedEffects":{"returnData":"net-nav","exitLiabilityRaw":"999"}}`},
	} {
		_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations VALUES ($1,$2,'REPORT_NAV',$3,$4,$5::jsonb,$4,($3::bigint)::text)`, row.route, row.status, row.slot, earningsTime(row.slot/10), row.effects)
		if err != nil {
			t.Fatal(err)
		}
	}
	reader := NewReader(nil, tx)
	got, err := reader.exitLiabilities(ctx, []costPoint{{Slot: 9, At: earningsTime(5)}, {Slot: 15, At: earningsTime(5)}, {Slot: 25, At: earningsTime(5)}, {Slot: 50, At: earningsTime(5)}, {At: earningsTime(1)}, {At: earningsTime(3)}, {Slot: 20, At: earningsTime(5), BeforeFlow: true}, {Slot: 20, At: earningsTime(5)}, {Slot: 65, At: earningsTime(7)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 9 || got[0] != nil || got[6] != nil || got[8] != nil {
		t.Fatalf("missing history became a value: %v", got)
	}
	for i, want := range map[int]USDCRaw{1: 0, 2: 20_000_000, 3: 20_000_000, 4: 0, 5: 20_000_000, 7: 20_000_000} {
		if got[i] == nil || *got[i] != want {
			t.Fatalf("mark %d = %v, want %d", i, got[i], want)
		}
	}
}
