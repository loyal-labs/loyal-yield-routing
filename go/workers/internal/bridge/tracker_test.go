package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	const (
		owned      = "TZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
		filledID   = "0xtron-usdc"
		usdtID     = "0xtron-solana-usdt"
		quotedOut  = "97000000"
		settledOut = "97911694"
	)
	const base58Letters = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	for i := range RequestsBatch {
		if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.relay_deposit_addresses (address, solana_wallet, app_user_id, chain, asset, quote_request_id, expires_at)
VALUES ($1, 'other', gen_random_uuid(), 'tron', 'usdt', '0xquote', now() + interval '90 days')`, "T"+strings.Repeat("A", 31)+string(base58Letters[i/24])+string(base58Letters[i%24])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.relay_deposit_addresses (address, solana_wallet, app_user_id, chain, asset, quote_request_id, expires_at)
VALUES ($1, $2, gen_random_uuid(), 'tron', 'usdt', '0xquote', now() + interval '90 days')`, owned, depositOrderWallet); err != nil {
		t.Fatal(err)
	}
	tronRequest := func(id, hash, status, output, amountOut string) string {
		return fmt.Sprintf(`{"id":%q,"status":%q,"recipient":%q,"depositAddress":{"address":%q},"createdAt":"2026-11-03T01:00:00Z",
"data":{"inTxs":[{"chainId":%[5]d,"txHash":%[6]q}],"route":{"actual":{"origin":{"inputCurrency":{"currency":{"chainId":%[5]d,"address":%[7]q},"amount":"100000000"}},
"destination":{"outputCurrency":{"currency":{"chainId":%[8]d,"address":%[9]q},"amount":%[10]q}}}}}}`,
			id, status, depositOrderWallet, owned, Tron.RelayChainID, strings.Repeat(hash, 64), Tron.Token, SolanaRelayChainID, output, amountOut)
	}
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/requests/v3" {
			switch {
			case r.URL.Query().Get("id") == filledID:
				fmt.Fprintf(w, `{"requests":[%s]}`, tronRequest(filledID, "a", "success", SolanaUSDCMint, settledOut))
			case !strings.Contains(r.URL.Query().Get("filters"), owned):
				http.Error(w, "upstream timeout", http.StatusBadGateway)
			default:
				fmt.Fprintf(w, `{"requests":[%s,%s]}`, tronRequest(filledID, "a", "pending", SolanaUSDCMint, quotedOut),
					tronRequest(usdtID, "b", "success", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", quotedOut))
			}
			return
		}
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

	cases = append(cases, struct {
		requestID             string
		reads                 []read
		state                 TransferState
		destinationTx, refund string
		polls                 int32
	}{filledID, []read{{"success", fillHash}}, TransferFilled, fillHash, "", 1})
	polls[filledID] = &atomic.Int32{}

	tracker := &Tracker{DB: pool, Relay: &Relay{BaseURL: relay.URL, HTTP: relay.Client(), APIKey: "key"}, Facts: engine.NewFacts(prometheus.NewRegistry())}
	if err := tracker.detect(ctx, Tron, time.Now().Add(-trackLookback)); err != nil {
		t.Fatal(err)
	}
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

	var tronRows int
	var amountOut string
	if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(max(amount_out_raw::text) FILTER (WHERE request_id = $1), '') FROM loyal_yield.bridge_transfers WHERE chain = 'tron'`, filledID).Scan(&tronRows, &amountOut); err != nil {
		t.Fatal(err)
	}
	if tronRows != 1 || amountOut != settledOut {
		t.Errorf("got %d tron rows, %s amount_out_raw %q; want only the USDC request, at Relay's settled %s", tronRows, filledID, amountOut, settledOut)
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
