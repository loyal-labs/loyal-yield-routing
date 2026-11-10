package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Opt-in, disposable local Postgres only. Never point this at a shared database.
func joyTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("EARN_JOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set EARN_JOY_TEST_DATABASE_URL to a disposable local database")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != "127.0.0.1" || !strings.HasPrefix(config.Database, "earn_joy_test") {
		t.Fatal("requires a disposable local earn_joy_test database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `DROP TABLE IF EXISTS earn_yield_push_state, app_user_smart_accounts, app_users, push_tokens;
CREATE TABLE earn_yield_push_state (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	wallet_public_key text NOT NULL UNIQUE,
	last_pushed_earned_usd numeric(20,6) NOT NULL DEFAULT 0,
	last_pushed_at timestamptz,
	sent_campaigns jsonb NOT NULL,
	recent_pushed_at timestamptz[] NOT NULL DEFAULT '{}'::timestamptz[],
	rule_version smallint NOT NULL DEFAULT 1,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE app_users (id uuid PRIMARY KEY, created_at timestamptz NOT NULL, subject_address text);
CREATE TABLE app_user_smart_accounts (user_id uuid NOT NULL, state text NOT NULL, solana_env text NOT NULL, settings_pda text, updated_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE push_tokens (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	token text NOT NULL UNIQUE,
	telegram_user_id bigint,
	wallet_public_key text,
	platform text NOT NULL,
	provider text NOT NULL DEFAULT 'expo',
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
)`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

type joyRow struct {
	lastPushedAt *time.Time
	earned       string
	sent         string
	recent       []time.Time
	ruleVersion  int16
}

func readJoyRow(t *testing.T, pool *pgxpool.Pool, wallet string) (joyRow, bool) {
	t.Helper()
	var row joyRow
	err := pool.QueryRow(context.Background(),
		`SELECT last_pushed_at, last_pushed_earned_usd::text, sent_campaigns::text, recent_pushed_at, rule_version
FROM earn_yield_push_state WHERE wallet_public_key = $1`, wallet).
		Scan(&row.lastPushedAt, &row.earned, &row.sent, &row.recent, &row.ruleVersion)
	if err == pgx.ErrNoRows {
		return row, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return row, true
}

func TestEarnJoyClaimIsCompareAndSwap(t *testing.T) {
	pool := joyTestPool(t)
	ctx := context.Background()
	c := NewCrons(pool, Earn{})
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	seed := selectJoyPush(10, 10, now, nil)
	if ok, err := c.claimJoyState(ctx, "W1", nil, seed, now, 10); err != nil || !ok {
		t.Fatalf("seed claim = %v, %v", ok, err)
	}
	if ok, err := c.claimJoyState(ctx, "W1", nil, seed, now, 10); err != nil || ok {
		t.Fatalf("second seed claim = %v, %v; want false", ok, err)
	}
	row, _ := readJoyRow(t, pool, "W1")
	if row.ruleVersion != 2 || len(row.recent) != 0 || row.recent == nil || row.earned != "10.000000" || row.sent != `["yield-total-1"]` {
		t.Fatalf("seeded row = %+v", row)
	}
	states, err := c.joyStates(ctx, []string{"W1"})
	if err != nil {
		t.Fatal(err)
	}
	read := states["W1"]
	if read.ruleVersion != 2 || read.recentPushedAt == nil || len(read.recentPushedAt) != 0 {
		t.Fatalf("read state = %+v", read)
	}

	later := now.Add(48 * time.Hour)
	push := selectJoyPush(12, 25, later, &read)
	if push.kind != joyDecisionPush {
		t.Fatalf("decision = %+v", push)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 2 {
		wg.Go(func() {
			ok, err := c.claimJoyState(ctx, "W1", &read, push, later, 25)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("racing claims won %d times, want 1", wins)
	}
	row, _ = readJoyRow(t, pool, "W1")
	if row.lastPushedAt == nil || !row.lastPushedAt.Equal(later) || row.earned != "25.000000" || len(row.recent) != 1 || !row.recent[0].Equal(later) || row.sent != `["yield-total-1"]` {
		t.Fatalf("pushed row = %+v", row)
	}

	// A stale rule version never matches a v2 row.
	stale := joyState{lastPushedAt: row.lastPushedAt, sentCampaigns: []string{"yield-total-1"}, ruleVersion: 1}
	if ok, err := c.claimJoyState(ctx, "W1", &stale, push, later, 40); err != nil || ok {
		t.Fatalf("stale-version claim = %v, %v; want false", ok, err)
	}

	// A v1 row is re-baselined: last_pushed_at stays, recent takes the decision value.
	old := now.Add(-24 * time.Hour)
	_, err = pool.Exec(ctx, `INSERT INTO earn_yield_push_state (wallet_public_key, last_pushed_at, last_pushed_earned_usd, sent_campaigns)
VALUES ('W3', $1, 5, '[]'::jsonb)`, old)
	if err != nil {
		t.Fatal(err)
	}
	states, err = c.joyStates(ctx, []string{"W3"})
	if err != nil {
		t.Fatal(err)
	}
	v1 := states["W3"]
	if v1.ruleVersion != 1 {
		t.Fatalf("v1 state = %+v", v1)
	}
	rebase := selectJoyPush(10, 7, now, &v1)
	if rebase.kind != joyDecisionRebaseline || len(rebase.recentPushedAt) != 1 {
		t.Fatalf("rebase decision = %+v", rebase)
	}
	if ok, err := c.claimJoyState(ctx, "W3", &v1, rebase, now, 7); err != nil || !ok {
		t.Fatalf("rebaseline claim = %v, %v", ok, err)
	}
	row, _ = readJoyRow(t, pool, "W3")
	if row.ruleVersion != 2 || row.lastPushedAt == nil || !row.lastPushedAt.Equal(old) || len(row.recent) != 1 || !row.recent[0].Equal(old) || row.earned != "7.000000" || row.sent != `["yield-total-1"]` {
		t.Fatalf("rebaselined row = %+v", row)
	}

	// An underwater wallet is re-baselined at zero, never at the loss.
	if _, err := pool.Exec(ctx, `INSERT INTO earn_yield_push_state (wallet_public_key, last_pushed_earned_usd, sent_campaigns) VALUES ('W4', 5, '[]'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	states, err = c.joyStates(ctx, []string{"W4"})
	if err != nil {
		t.Fatal(err)
	}
	v1 = states["W4"]
	rebase = selectJoyPush(10, -20, now, &v1)
	if ok, err := c.claimJoyState(ctx, "W4", &v1, rebase, now, -20); err != nil || !ok {
		t.Fatalf("underwater rebaseline claim = %v, %v", ok, err)
	}
	if row, _ = readJoyRow(t, pool, "W4"); row.earned != "0.000000" {
		t.Fatalf("underwater rebaselined row = %+v", row)
	}
}

func TestEarnJoyRunSkipsUnavailableWallet(t *testing.T) {
	pool := joyTestPool(t)
	ctx := context.Background()
	for i, wallet := range []string{"W1", "W2"} {
		id := "00000000-0000-0000-0000-00000000000" + string(rune('1'+i))
		if _, err := pool.Exec(ctx, `INSERT INTO app_users (id, created_at, subject_address) VALUES ($1, now() - interval '10 days', $2)`, id, wallet); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO app_user_smart_accounts (user_id, state, solana_env, settings_pda) VALUES ($1, 'ready', $2, $3)`, id, smartAccountsSolanaEnv, "settings-"+wallet); err != nil {
			t.Fatal(err)
		}
	}
	push, _ := oneSignalStub(t, struct {
		status int
		body   string
	}{200, `{"id":"n1"}`})
	c := NewCrons(pool, Earn{})
	c.h.CampaignPush = push
	c.Earnings = func(_ context.Context, _, _, wallet string) (joyEarnings, bool, error) {
		if wallet == "W2" {
			return joyEarnings{}, false, nil
		}
		return joyEarnings{earnLifetimeUSD: 5}, true, nil
	}
	summary, err := c.EarnYieldJoy(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Candidates != 2 || summary.Unavailable != 1 || summary.Seeded != 1 || summary.Errors != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if _, ok := readJoyRow(t, pool, "W2"); ok {
		t.Fatal("unavailable wallet got a state row")
	}
	if row, ok := readJoyRow(t, pool, "W1"); !ok || row.ruleVersion != 2 {
		t.Fatalf("W1 row = %+v, %v", row, ok)
	}
}

func TestEarnJoyEmitsOnlyAcceptedPushes(t *testing.T) {
	pool := joyTestPool(t)
	ctx := context.Background()
	for i, wallet := range []string{"W1", "W2"} {
		id := "00000000-0000-0000-0000-00000000000" + string(rune('1'+i))
		if _, err := pool.Exec(ctx, `INSERT INTO app_users (id, created_at, subject_address) VALUES ($1, now() - interval '10 days', $2)`, id, wallet); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO app_user_smart_accounts (user_id, state, solana_env, settings_pda) VALUES ($1, 'ready', $2, $3)`, id, smartAccountsSolanaEnv, "settings-"+wallet); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO earn_yield_push_state (wallet_public_key, last_pushed_earned_usd, sent_campaigns, rule_version) VALUES ($1, 0, '[]'::jsonb, 2)`, wallet); err != nil {
			t.Fatal(err)
		}
	}
	push, _ := oneSignalStub(t, reply{200, `{"id":"n"}`})
	var sentMu sync.Mutex
	sentTo := []string{}
	oneSignal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SubscriptionIDs []string `json:"include_subscription_ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		wallet := strings.TrimPrefix(body.SubscriptionIDs[0], "sub-")
		sentMu.Lock()
		sentTo = append(sentTo, wallet)
		sentMu.Unlock()
		if wallet == "W1" {
			_, _ = w.Write([]byte(`{"id":"n"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":""}`))
	}))
	t.Cleanup(oneSignal.Close)
	push.url = oneSignal.URL
	sent := func() []string {
		sentMu.Lock()
		defer sentMu.Unlock()
		return append([]string(nil), sentTo...)
	}
	tracker, tracked := mixpanelStub(t)
	c := NewCrons(pool, Earn{})
	c.h.CampaignPush = push
	c.Mixpanel = tracker
	c.Earnings = func(_ context.Context, _, _, wallet string) (joyEarnings, bool, error) {
		return joyEarnings{earnLifetimeUSD: 5}, true, nil
	}
	if _, err := c.EarnYieldJoy(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(sent()) != 0 || len(tracked()) != 0 {
		t.Fatalf("dry run sent %d pushes and %d tracks", len(sent()), len(tracked()))
	}
	summary, err := c.EarnYieldJoy(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Pushed) != 1 || summary.Pushed[0].Wallet != "W1" || summary.Undelivered != 1 || len(sent()) != 2 {
		t.Fatalf("summary = %+v, sent = %d", summary, len(sent()))
	}
	// W2's claim is spent but OneSignal accepted nothing: report it undelivered.
	if _, err := pool.Exec(ctx, `UPDATE earn_yield_push_state SET last_pushed_at = NULL, sent_campaigns = '[]'::jsonb, recent_pushed_at = '{}' WHERE wallet_public_key = 'W2'`); err != nil {
		t.Fatal(err)
	}
	states, err := c.joyStates(ctx, []string{"W2"})
	if err != nil {
		t.Fatal(err)
	}
	w2 := states["W2"]
	outcome, err := c.processJoyCandidate(ctx, joyCandidate{wallet: "W2", settingsPda: "settings-W2", createdAt: time.Now().Add(-10 * 24 * time.Hour)}, "mainnet", false, time.Now(), &w2)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "undelivered" || outcome.Campaign != "yield-total-1" || outcome.AmountUSD == nil || outcome.event != nil {
		t.Fatalf("W2 outcome = %+v", outcome)
	}
	requests := tracked()
	if len(requests) != 1 || len(requests[0]) != 1 {
		t.Fatalf("tracked = %v", requests)
	}
	event := requests[0][0]
	props, _ := event["properties"].(map[string]any)
	if event["event"] != "Earnings Push Sent" || props["distinct_id"] != "mob:W1" || props["type"] != "milestone" ||
		props["campaign"] != "yield-total-1" || props["lifetime_usd"] != float64(5) {
		t.Fatalf("event = %v", event)
	}
	if want := "earnings:yield-total-1:W1:" + time.Now().UTC().Format("2006-01-02"); props["$insert_id"] != want {
		t.Fatalf("$insert_id = %v, want %v", props["$insert_id"], want)
	}
}

func TestWalletPushTargetsOnlyRegisteredOneSignalSubscriptions(t *testing.T) {
	pool := joyTestPool(t)
	ctx := context.Background()
	const sub = "3f2b8c1e-9d4a-4e7b-8c6f-1a2b3c4d5e6f"
	if _, err := pool.Exec(ctx, `INSERT INTO push_tokens (token, wallet_public_key, platform, provider) VALUES
	($1, 'W1', 'android', 'onesignal'), ('ExponentPushToken[w1]', 'W1', 'android', 'expo'), ('ExponentPushToken[w2]', 'W2', 'ios', 'expo')`, sub); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var targeted [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SubscriptionIDs []string `json:"include_subscription_ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		targeted = append(targeted, body.SubscriptionIDs)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"n"}`))
	}))
	t.Cleanup(server.Close)
	push := NewWalletPush(pool, OneSignalConfig{AppID: "app", RESTAPIKey: "key", AlertsChannelID: "alerts"}, "secret")
	push.url = server.URL
	if sent, err := push.sendToWallet(ctx, "W1", autodepositWalletPush{Title: "t", Body: "b"}, nil); !sent || err != nil {
		t.Fatalf("W1 sendToWallet = (%v, %v)", sent, err)
	}
	// W2 only has a legacy Expo token, so OneSignal is never called for it.
	if sent, err := push.sendToWallet(ctx, "W2", autodepositWalletPush{Title: "t", Body: "b"}, nil); sent || err != nil {
		t.Fatalf("W2 sendToWallet = (%v, %v), want (false, nil)", sent, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(targeted) != 1 || len(targeted[0]) != 1 || targeted[0][0] != sub {
		t.Fatalf("targeted = %v, want [[%s]]", targeted, sub)
	}
}

func TestEarnJoyUnregisteredWalletKeepsItsClaim(t *testing.T) {
	pool := joyTestPool(t)
	ctx := context.Background()
	const id = "00000000-0000-0000-0000-000000000009"
	if _, err := pool.Exec(ctx, `INSERT INTO app_users (id, created_at, subject_address) VALUES ($1, now() - interval '10 days', 'W9')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_user_smart_accounts (user_id, state, solana_env, settings_pda) VALUES ($1, 'ready', $2, 'settings-W9')`, id, smartAccountsSolanaEnv); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO earn_yield_push_state (wallet_public_key, last_pushed_earned_usd, sent_campaigns, rule_version) VALUES ('W9', 0, '[]'::jsonb, 2)`); err != nil {
		t.Fatal(err)
	}
	push, requests := oneSignalStub(t, reply{200, `{"id":"n"}`})
	var registered bool
	push.subscriptions = func(_ context.Context, wallet string) ([]string, error) {
		if !registered {
			return nil, nil
		}
		return []string{"sub-" + wallet}, nil
	}
	c := NewCrons(pool, Earn{})
	c.h.CampaignPush = push
	c.Earnings = func(context.Context, string, string, string) (joyEarnings, bool, error) {
		return joyEarnings{earnLifetimeUSD: 5}, true, nil
	}

	summary, err := c.EarnYieldJoy(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Unregistered != 1 || len(summary.Pushed) != 0 || len(requests()) != 0 {
		t.Fatalf("unregistered run: summary = %+v, requests = %d", summary, len(requests()))
	}
	row, _ := readJoyRow(t, pool, "W9")
	if row.lastPushedAt != nil || row.sent != "[]" || len(row.recent) != 0 {
		t.Fatalf("claim spent without a registered device: %+v", row)
	}

	registered = true
	summary, err = c.EarnYieldJoy(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Pushed) != 1 || summary.Pushed[0].Campaign != "yield-total-1" || len(requests()) != 1 {
		t.Fatalf("registered run: summary = %+v, requests = %d", summary, len(requests()))
	}
}
