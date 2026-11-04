package webapp

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/bridge"
	"github.com/solana-foundation/solana-go/v2"
)

func bridgeOutTestDB(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("BRIDGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable local bridge_test database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.Host != "127.0.0.1" || !strings.HasPrefix(config.ConnConfig.Database, "bridge_test") {
		t.Fatal("requires a disposable local bridge_test database")
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE SCHEMA loyal_yield`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DROP SCHEMA loyal_yield CASCADE`); err != nil {
			t.Error(err)
		}
	})
	var migrations []string
	for _, pattern := range []string{"*_bridge_transfers*.sql", "*_relay_deposit_addresses.sql"} {
		found, err := filepath.Glob("../../../../migrations/yield/" + pattern)
		if err != nil || len(found) == 0 {
			t.Fatal(pattern, "migrations not found", err)
		}
		migrations = append(migrations, found...)
	}
	slices.Sort(migrations)
	for _, path := range migrations {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatal(path, err)
		}
	}
	return pool
}

func TestBridgeOutQuoteIsNotServedWithoutItsRow(t *testing.T) {
	pool := bridgeOutTestDB(t)
	ctx := context.Background()
	wallet := solana.NewWallet().PublicKey()
	q := bridge.OutQuote{RequestID: "0xaaaa", AmountInRaw: big.NewInt(25_000_000), AmountOutRaw: big.NewInt(24_900_000)}
	const recipient = "0x98f566dc02faf8fd0f3ebb6aab48c40164207fc7"
	if err := insertBridgeOut(ctx, pool, wallet, recipient, bridge.Base, bridge.USDC, q); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insertBridgeOut(ctx, pool, wallet, recipient, bridge.Base, bridge.USDC, q); err == nil {
		t.Fatal("a request id that already has a row was served again")
	}
}
