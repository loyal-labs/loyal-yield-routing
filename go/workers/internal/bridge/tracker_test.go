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

func TestTrackerStoresEachHashFromTheStatusThatCarriedIt(t *testing.T) {
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
	const (
		refundHash  = "0x1111111111111111111111111111111111111111111111111111111111111111"
		successHash = "0x2222222222222222222222222222222222222222222222222222222222222222"
		fillHash    = "4TgCELNYYxzv2uLxk7wKjaP77SB3fDCyq31EPsLHMQWTSiBmZ5QmSEYzCgdVSgYumZk45CCUMHmvVziAR1bTBmFx"
	)
	type read struct{ status, hash string }
	cases := []struct {
		requestID             string
		reads                 []read
		state                 TransferState
		destinationTx, refund string
		polls                 int32
	}{
		{"0xrefund", []read{{"refund", ""}, {"refund", refundHash}}, TransferRefunded, "", refundHash, 2},
		{"0xrefund-then-success", []read{{"refund", ""}, {"success", successHash}}, TransferRefunded, "", "", 3},
		{"0xsuccess", []read{{"success", ""}, {"success", fillHash}}, TransferFilled, fillHash, "", 2},
	}
	polls := map[string]*atomic.Int32{}
	for _, c := range cases {
		polls[c.requestID] = &atomic.Int32{}
		if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.bridge_transfers
(request_id, solana_wallet, evm_address, chain, asset, amount_in_raw, amount_out_raw, state, submitted_at)
VALUES ($1, 'wallet', '0x0000000000000000000000000000000000000001', 'base', 'usdc', 1000000, 990000, 'submitted', now())`, c.requestID); err != nil {
			t.Fatal(err)
		}
	}
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("requestId")
		for _, c := range cases {
			if c.requestID != id {
				continue
			}
			n := int(polls[id].Add(1))
			step := c.reads[min(n, len(c.reads))-1]
			hashes := "[]"
			if step.hash != "" {
				hashes = `["` + step.hash + `"]`
			}
			fmt.Fprintf(w, `{"status":%q,"inTxHashes":[],"txHashes":%s}`, step.status, hashes)
			return
		}
		http.NotFound(w, r)
	}))
	defer relay.Close()

	tracker := &Tracker{DB: pool, Relay: &Relay{BaseURL: relay.URL, HTTP: relay.Client()}, Facts: engine.NewFacts(prometheus.NewRegistry())}
	for range 3 {
		if err := tracker.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range cases {
		var state string
		var destinationTx, refundTx *string
		if err := pool.QueryRow(ctx, `SELECT state, to_jsonb(b)->>'destination_tx', to_jsonb(b)->>'refund_tx'
FROM loyal_yield.bridge_transfers b WHERE request_id = $1`, c.requestID).Scan(&state, &destinationTx, &refundTx); err != nil {
			t.Fatal(err)
		}
		if state != string(c.state) || deref(destinationTx) != orNull(c.destinationTx) || deref(refundTx) != orNull(c.refund) || polls[c.requestID].Load() != c.polls {
			t.Errorf("%s: got state %q destination_tx %s refund_tx %s after %d relay reads; want %q, %s, %s, %d reads",
				c.requestID, state, deref(destinationTx), deref(refundTx), polls[c.requestID].Load(), c.state, orNull(c.destinationTx), orNull(c.refund), c.polls)
		}
	}
}

func orNull(s string) string {
	if s == "" {
		return "NULL"
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
}
