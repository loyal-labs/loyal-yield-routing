package webapp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/bridge"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
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
	for _, pattern := range []string{"*_evm_deposit_routes*.sql", "*_bridge_transfers*.sql", "*_relay_deposit_addresses.sql"} {
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

func TestBridgeTransfersServeConfirmedQuotedOutRowsInsideTheWindow(t *testing.T) {
	pool := bridgeOutTestDB(t)
	ctx := context.Background()
	wallet := solana.NewWallet().PublicKey()
	const recipient = "0x98f566dc02faf8fd0f3ebb6aab48c40164207fc7"
	for id, confirmedAgo := range map[string]string{"0xinside": "5 seconds", "0xoutside": "300 seconds", "0xunstamped": ""} {
		q := bridge.OutQuote{RequestID: id, AmountInRaw: big.NewInt(25_000_000), AmountOutRaw: big.NewInt(24_900_000)}
		if err := insertBridgeOut(ctx, pool, wallet, recipient, bridge.Base, bridge.USDC, q); err != nil {
			t.Fatal(err)
		}
		if confirmedAgo == "" {
			continue
		}
		if _, err := pool.Exec(ctx, `UPDATE loyal_yield.bridge_transfers SET confirmed_at = clock_timestamp() - $2::interval WHERE request_id = $1`, id, confirmedAgo); err != nil {
			t.Fatal(err)
		}
	}
	out, err := listBridgeTransfers(ctx, pool, wallet, bridge.DirectionOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].RequestID != "0xinside" || out[0].ConfirmedAt == nil {
		t.Fatalf("out rows served %+v, want only the quoted row confirmed inside the window", out)
	}
}

func TestBridgeTransfersMarkInFlightFromServerWindows(t *testing.T) {
	pool := bridgeOutTestDB(t)
	ctx := context.Background()
	wallet := solana.NewWallet().PublicKey()
	const recipient = "0x98f566dc02faf8fd0f3ebb6aab48c40164207fc7"
	const originTx = "5ZyJqhYV2VoF8mQp7hX1cC3uR9wTn4kLbPd6sEaGfNxM2vUiBtYrKjWz8oHqSeDcAgFnLpXmVtRu3yJbKh7NwQa9"
	add := func(id, state, submittedAgo string) {
		q := bridge.OutQuote{RequestID: id, AmountInRaw: big.NewInt(25_000_000), AmountOutRaw: big.NewInt(24_900_000)}
		if err := insertBridgeOut(ctx, pool, wallet, recipient, bridge.Base, bridge.USDC, q); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE loyal_yield.bridge_transfers SET state = $2, origin_tx = $3, submitted_at = clock_timestamp() - $4::interval,
settled_at = CASE WHEN $2 IN ('filled', 'refunded') THEN clock_timestamp() - $4::interval END WHERE request_id = $1`, id, state, originTx, submittedAgo); err != nil {
			t.Fatal(err)
		}
	}
	add("0xrecent", "submitted", "9 minutes")
	add("0xstuck", "submitted", "11 minutes")
	for i := range 25 {
		add("0xdone"+strings.Repeat("a", i+1), "filled", "1 second")
	}
	out, err := listBridgeTransfers(ctx, pool, wallet, bridge.DirectionOut)
	if err != nil {
		t.Fatal(err)
	}
	flights := map[string]bool{}
	for _, row := range out {
		flights[row.RequestID] = row.InFlight
	}
	if len(out) == 0 || out[0].RequestID != "0xrecent" || !out[0].InFlight {
		t.Fatalf("first of %d rows is %q, want the submitted row inside the window flagged inFlight ahead of 25 newer terminal rows", len(out), out[0].RequestID)
	}
	if inFlight, listed := flights["0xstuck"]; listed && inFlight {
		t.Fatal("a submitted row older than the window is still inFlight")
	}
	for id, inFlight := range flights {
		if id != "0xrecent" && inFlight {
			t.Fatalf("%s flagged inFlight", id)
		}
	}
}

func TestBridgeQuoteRefusesASecondMoveWhileTheFirstIsConfirmedAndUnsubmitted(t *testing.T) {
	pool := bridgeOutTestDB(t)
	ctx := context.Background()
	wallet := solana.NewWallet().PublicKey()
	sender := "0x" + hex.EncodeToString(wallet[:20])
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.evm_deposit_routes (solana_wallet, app_user_id, kind, evm_address) VALUES ($1, gen_random_uuid(), 'relay_signed', $2)`, wallet.String(), sender); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.bridge_transfers (request_id, solana_wallet, counterparty, chain, asset, amount_in_raw, amount_out_raw) VALUES ('0xfirst', $1, $2, 'base', 'usdc', 25000000, 24900000)`, wallet.String(), sender); err != nil {
		t.Fatal(err)
	}
	keys, err := jwt.NewKeys("isolated-session-test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jwt.NewSigner("isolated-session-test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Mint(map[string]any{"sub": wallet.String()}, mobileEarnAudience, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	earnRoutes{
		auth:         &Auth{keys: keys},
		bridgeQuotes: newLimiters(),
		Earn:         Earn{TestWallets: parseTestWallets("*"), YieldDB: pool, Relay: &bridge.Relay{BaseURL: "http://127.0.0.1:9", HTTP: &http.Client{}}},
	}.registerBridge(humachi.New(router, apiConfig()))
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	if res := post("/api/funding/bridge/transfers/0xfirst/confirm", `{}`); res.Code != http.StatusOK {
		t.Fatalf("confirm = %d %s", res.Code, res.Body.String())
	}
	res := post(bridgeQuotePath, `{"chain":"base","asset":"usdc","amountRaw":"25000000"}`)
	var body struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"requestId"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusConflict || body.Error.Code != "bridge_in_flight" || body.Error.RequestID != "0xfirst" {
		t.Fatalf("second quote = %d %s; want 409 bridge_in_flight naming 0xfirst", res.Code, res.Body.String())
	}
}
