package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
)

func TestRefundHashArrivingAfterRefundIsStored(t *testing.T) {
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
	defer pool.Close()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA loyal_yield`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := pool.Exec(ctx, `DROP SCHEMA loyal_yield CASCADE`); err != nil {
			t.Error(err)
		}
	}()
	migrations, err := filepath.Glob("../../../../migrations/yield/*_bridge_transfers*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("bridge_transfers migrations not found", err)
	}
	for _, path := range migrations {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatal(path, err)
		}
	}
	const requestID = "0xrefund"
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.bridge_transfers
(request_id, solana_wallet, evm_address, chain, asset, amount_in_raw, amount_out_raw, state, submitted_at)
VALUES ($1, 'wallet', '0x0000000000000000000000000000000000000001', 'base', 'usdc', 1000000, 990000, 'submitted', now())`, requestID); err != nil {
		t.Fatal(err)
	}

	const refundHash = "0x1111111111111111111111111111111111111111111111111111111111111111"
	var reads atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hashes := "[]"
		if reads.Add(1) > 1 {
			hashes = `["` + refundHash + `"]`
		}
		fmt.Fprintf(w, `{"status":"refund","inTxHashes":[],"txHashes":%s}`, hashes)
	}))
	defer relay.Close()

	tracker := &Tracker{DB: pool, Relay: &Relay{BaseURL: relay.URL, HTTP: relay.Client()}, Facts: engine.NewFacts(prometheus.NewRegistry())}
	for range 3 {
		if err := tracker.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var state string
	var destinationTx, refundTx *string
	if err := pool.QueryRow(ctx, `SELECT state, to_jsonb(b)->>'destination_tx', to_jsonb(b)->>'refund_tx'
FROM loyal_yield.bridge_transfers b WHERE request_id = $1`, requestID).Scan(&state, &destinationTx, &refundTx); err != nil {
		t.Fatal(err)
	}
	if state != string(TransferRefunded) || destinationTx != nil || refundTx == nil || *refundTx != refundHash || reads.Load() != 2 {
		t.Fatalf("got state %q destination_tx %v refund_tx %v after %d relay reads; want refunded, no destination_tx, refund_tx %s, 2 reads",
			state, deref(destinationTx), deref(refundTx), reads.Load(), refundHash)
	}
}

func deref(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
}
