package fleet

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The root owns schema installation. This probe performs one read-only query
// against the registered disposable fixture and never applies migration DDL.
func TestCrossMintPreparationGuardRegisteredSchema(t *testing.T) {
	dsn := os.Getenv("FLEET_EXEC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires root-registered disposable fleet execution database")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "postgresql" || u.Hostname() != "127.0.0.1" || u.Port() != "51913" || u.Path != "/workers_v2_fleetexec" || u.User == nil || u.User.Username() != "workers_v2" || u.Fragment != "" {
		t.Fatal("requires registered loopback workers_v2 fixture before connecting")
	}
	if _, present := u.User.Password(); present {
		t.Fatal("disposable fixture must not contain credentials")
	}
	for key, values := range u.Query() {
		if key != "sslmode" || len(values) != 1 || values[0] != "disable" {
			t.Fatal("fixture URL contains unregistered connection options")
		}
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("configured registered database is unavailable:", err)
	}
	store, err := NewStoreFromPool(pool)
	if err != nil {
		t.Fatal(err)
	}
	q, _, _ := crossMintPreparationFixture(t)
	q.Movement.DecisionID = -1
	if err := store.CheckCrossMintPreparation(ctx, q); err == nil || !strings.Contains(err.Error(), "continuation authority, custody or leg budget changed") {
		t.Fatalf("registered source SQL did not return expected absent-row fence: %v", err)
	}
}
