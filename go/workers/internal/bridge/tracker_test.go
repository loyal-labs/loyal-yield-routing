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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
)

func trackerDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
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
	return ctx, pool
}

func TestTrackerStoresEachHashFromTheStatusThatCarriedIt(t *testing.T) {
	ctx, pool := trackerDB(t)
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
		pendingID  = "0xtron-usdc-unsettled"
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
	tronRequest := func(id, hash, status, route, output, amountOut string) string {
		return fmt.Sprintf(`{"id":%q,"status":%q,"recipient":%q,"depositAddress":{"address":%q},"createdAt":"2026-11-03T01:00:00Z",
"data":{"inTxs":[{"chainId":%[5]d,"txHash":%[6]q}],"route":{%[11]q:{"origin":{"inputCurrency":{"currency":{"chainId":%[5]d,"address":%[7]q},"amount":"100000000"}},
"destination":{"outputCurrency":{"currency":{"chainId":%[8]d,"address":%[9]q},"amount":%[10]q}}}}}}`,
			id, status, depositOrderWallet, owned, Tron.RelayChainID, strings.Repeat(hash, 64), Tron.Token, SolanaRelayChainID, output, amountOut, route)
	}
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/requests/v3" {
			switch {
			case r.URL.Query().Get("id") == filledID:
				fmt.Fprintf(w, `{"requests":[%s]}`, tronRequest(filledID, "a", "success", "actual", SolanaUSDCMint, settledOut))
			case r.URL.Query().Get("id") == pendingID:
				fmt.Fprintf(w, `{"requests":[%s]}`, tronRequest(pendingID, "c", "pending", "quoted", SolanaUSDCMint, "96500000"))
			case !strings.Contains(r.URL.Query().Get("filters"), owned):
				http.Error(w, "upstream timeout", http.StatusBadGateway)
			default:
				fmt.Fprintf(w, `{"requests":[%s,%s,%s]}`, tronRequest(filledID, "a", "pending", "actual", SolanaUSDCMint, quotedOut),
					tronRequest(usdtID, "b", "success", "actual", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", quotedOut),
					tronRequest(pendingID, "c", "pending", "quoted", SolanaUSDCMint, quotedOut))
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
	}{filledID, []read{{"success", fillHash}}, TransferFilled, fillHash, "", 1}, struct {
		requestID             string
		reads                 []read
		state                 TransferState
		destinationTx, refund string
		polls                 int32
	}{pendingID, []read{{"success", fillHash}}, TransferSubmitted, "", "", 3})
	polls[filledID] = &atomic.Int32{}
	polls[pendingID] = &atomic.Int32{}

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

	var tronAmounts string
	if err := pool.QueryRow(ctx, `SELECT string_agg(request_id || '=' || amount_out_raw, ' ' ORDER BY request_id) FROM loyal_yield.bridge_transfers WHERE chain = 'tron'`).Scan(&tronAmounts); err != nil {
		t.Fatal(err)
	}
	if want := filledID + "=" + settledOut + " " + pendingID + "=" + quotedOut; tronAmounts != want {
		t.Errorf("got tron amount_out_raw %s; want only the USDC requests, the settled one at Relay's actual amount and the unsettled one unchanged: %s", tronAmounts, want)
	}
}

func TestTrackerAdoptsQuotedTransfersRelaySaw(t *testing.T) {
	ctx, pool := trackerDB(t)
	originHash := "0x" + strings.Repeat("a", 64)
	clientHash := "0x" + strings.Repeat("b", 64)
	const fillHash = "4TgCELNYYxzv2uLxk7wKjaP77SB3fDCyq31EPsLHMQWTSiBmZ5QmSEYzCgdVSgYumZk45CCUMHmvVziAR1bTBmFx"
	const clientSubmit = `UPDATE loyal_yield.bridge_transfers SET state = 'submitted', origin_tx = $2, submitted_at = now() WHERE request_id = $1 AND state = 'quoted'`
	quote := func(requestID, age string) {
		if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.bridge_transfers
(request_id, solana_wallet, evm_address, chain, asset, amount_in_raw, amount_out_raw, quoted_at)
VALUES ($1, 'wallet', '0x0000000000000000000000000000000000000001', 'base', 'usdc', 1000000, 990000, now() - $2::interval)`, requestID, age); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"0xseen", "0xfilled", "0xunseen", "0xwaiting", "0xraced"} {
		quote(id, "1 minute")
	}
	quote("0xexpired", "3 hours")

	var mu sync.Mutex
	polls := map[string]int{}
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("requestId")
		mu.Lock()
		polls[id]++
		mu.Unlock()
		switch id {
		case "0xseen", "0xexpired":
			fmt.Fprintf(w, `{"status":"pending","inTxHashes":[%q],"txHashes":[]}`, "0x"+strings.ToUpper(originHash[2:]))
		case "0xfilled":
			fmt.Fprintf(w, `{"status":"success","inTxHashes":[%q],"txHashes":[%q]}`, originHash, fillHash)
		case "0xwaiting":
			fmt.Fprint(w, `{"status":"waiting"}`)
		case "0xraced":
			if _, err := pool.Exec(r.Context(), clientSubmit, id, clientHash); err != nil {
				t.Error(err)
			}
			fmt.Fprintf(w, `{"status":"pending","inTxHashes":[%q],"txHashes":[]}`, originHash)
		default:
			fmt.Fprint(w, `{"status":"unknown"}`)
		}
	}))
	defer relay.Close()

	tracker := &Tracker{DB: pool, Relay: &Relay{BaseURL: relay.URL, HTTP: relay.Client()}, Facts: engine.NewFacts(prometheus.NewRegistry())}
	if err := tracker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if tag, err := pool.Exec(ctx, clientSubmit, "0xseen", clientHash); err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("a late client submit changed an adopted row: %d rows, err = %v", tag.RowsAffected(), err)
	}

	for id, want := range map[string]struct{ state, originTx string }{
		"0xseen":    {"submitted", originHash},
		"0xfilled":  {"filled", originHash},
		"0xunseen":  {"quoted", ""},
		"0xwaiting": {"quoted", ""},
		"0xexpired": {"quoted", ""},
		"0xraced":   {"submitted", clientHash},
	} {
		var state, originTx string
		var rows int
		if err := pool.QueryRow(ctx, `SELECT min(state), coalesce(min(origin_tx), ''), count(*) FROM loyal_yield.bridge_transfers WHERE request_id = $1`, id).Scan(&state, &originTx, &rows); err != nil {
			t.Fatal(err)
		}
		if state != want.state || originTx != want.originTx || rows != 1 {
			t.Errorf("%s: %d rows, state %q, origin_tx %q; want one row, %q, %q", id, rows, state, originTx, want.state, want.originTx)
		}
	}
	if polls["0xexpired"] != 0 {
		t.Errorf("a quote past the window was polled %d times", polls["0xexpired"])
	}

	for i := range 2 * lostSubmitBatch {
		quote(fmt.Sprintf("0xabandoned-%d", i), "1 minute")
	}
	clear(polls)
	if err := tracker.adoptLostSubmits(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(polls); n != lostSubmitBatch {
		t.Errorf("one tick polled %d quoted rows, want the batch of %d", n, lostSubmitBatch)
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
