package backyard

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CountNonterminal is the release gate: a deploy stops the worker only when
// the route has nothing a restart would resume.
func TestCountNonterminalAgainstDatabase(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureManualRecoveryTestSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	routeKey := productionRouteKey
	if _, err := db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey)
	})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version)
		 VALUES($1,'{"generation":1,"cycle":1}',1)
		 ON CONFLICT (route_key) DO UPDATE SET state = EXCLUDED.state, state_version = EXCLUDED.state_version, lease_owner = NULL, lease_expires_at = NULL`, routeKey); err != nil {
		t.Fatal(err)
	}
	expect := func(want int) {
		t.Helper()
		got, err := CountNonterminal(ctx, url, routeKey)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("in-flight = %d, want %d", got, want)
		}
	}
	expect(0)
	// A terminal row is history, not in-flight work.
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status) VALUES('in-flight-done',$1,'reconciled')`, routeKey); err != nil {
		t.Fatal(err)
	}
	expect(0)
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status) VALUES('in-flight-open',$1,'submitted')`, routeKey); err != nil {
		t.Fatal(err)
	}
	expect(1)
}
