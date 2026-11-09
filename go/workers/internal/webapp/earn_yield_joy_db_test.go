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
	_, err = pool.Exec(ctx, `DROP TABLE IF EXISTS earn_yield_push_state, app_user_smart_accounts, app_users;
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
CREATE TABLE app_user_smart_accounts (user_id uuid NOT NULL, state text NOT NULL, solana_env text NOT NULL, settings_pda text, updated_at timestamptz NOT NULL DEFAULT now())`)
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
			IncludeAliases struct {
				ExternalID []string `json:"external_id"`
			} `json:"include_aliases"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		wallet := body.IncludeAliases.ExternalID[0]
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
	if len(summary.Pushed) != 2 || len(sent()) != 2 {
		t.Fatalf("summary = %+v, sent = %d", summary, len(sent()))
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
