package backyard

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDownApprovalStoreSurvivesRestartAndRevokes(t *testing.T) {
	dsn := os.Getenv("BACKYARD_FOREIGN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable local backyard_foreign_test database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.Host != "127.0.0.1" || !strings.HasPrefix(config.ConnConfig.Database, "backyard_foreign_test") {
		t.Fatal("requires a disposable local backyard_foreign_test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA loyal_yield;
		CREATE TABLE loyal_yield.multiply_route_states (route_key text PRIMARY KEY, state jsonb NOT NULL DEFAULT '{}');`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := pool.Exec(context.Background(), `DROP SCHEMA loyal_yield CASCADE`); err != nil {
			t.Error(err)
		}
	}()
	for _, migration := range []string{"0103_backyard_target_inputs.sql", "0114_backyard_lane_levels_ladder.sql", "0123_backyard_lane_exit_costs.sql", "0137_backyard_down_approval.sql"} {
		if migration == "0137_backyard_down_approval.sql" {
			if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key) VALUES ($1), ($2)`, rwa.RouteKey, looping.RouteKey); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.backyard_lane_levels(route_key, lane, level)
				VALUES ($1, $3, 1), ($1, $4, 2.5), ($2, $3, 1)`, rwa.RouteKey, looping.RouteKey, onreONycUSDC, autoAUTOPYUSD.Lane); err != nil {
				t.Fatal(err)
			}
		}
		data, err := os.ReadFile("../../../../migrations/yield/" + migration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Fatalf("%s: %v", migration, err)
		}
	}
	for _, tc := range []struct {
		route, lane string
		want        float64
	}{{rwa.RouteKey, onreONycUSDC, 1.5}, {rwa.RouteKey, autoAUTOPYUSD.Lane, 2.5}, {looping.RouteKey, onreONycUSDC, 1}} {
		var levels, exits, costs []byte
		if err := pool.QueryRow(ctx, "SELECT "+targetInputsSQL, tc.route).Scan(&levels, &exits, &costs); err != nil {
			t.Fatal(err)
		}
		in, err := decodeTargetInputs(levels, exits, nil, costs)
		if err != nil || in.Levels[tc.lane] != (laneLevel{Level: tc.want}) {
			t.Fatalf("migration changed target meaning: %+v (%v)", in.Levels, err)
		}
		if tc.route == rwa.RouteKey && tc.lane == onreONycUSDC {
			s := leverageSnapshot(1.5)
			s.RouteLane, s.StrategyKey, s.BorrowUtilizationBlocked = tc.lane, tc.lane, true
			if got := Decide(targetedWith(s, in)); got.Action != Hold {
				t.Fatalf("upgrade moved the existing position: %+v", got)
			}
		}
	}
	db := &Database{pool: pool}
	level := laneLevel{Level: 1, Down: downApproval{MaxCostBPS: 60_000_000, ExpiresUnix: time.Now().Add(10 * time.Minute).Unix()}}
	for _, want := range []laneLevel{level, {Level: 1}} {
		if err := db.RecordLaneLevel(ctx, rwa.RouteKey, onreONycUSDC, want); err != nil {
			t.Fatal(err)
		}
		var levels, exits, costs []byte
		if err := pool.QueryRow(ctx, "SELECT "+targetInputsSQL, rwa.RouteKey).Scan(&levels, &exits, &costs); err != nil {
			t.Fatal(err)
		}
		in, err := decodeTargetInputs(levels, exits, nil, costs)
		if err != nil || in.Levels[onreONycUSDC] != want {
			t.Fatalf("approval after reload: %+v (%v)", in.Levels, err)
		}
	}
	level.UpApproved = true
	if err := db.RecordLaneLevel(ctx, rwa.RouteKey, onreONycUSDC, level); err == nil {
		t.Fatal("simultaneous increase/reduction authority accepted")
	}
}
