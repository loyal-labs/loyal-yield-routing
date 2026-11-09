package webapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEarnYieldJoyResumesUntilDailyCycleCompletes(t *testing.T) {
	dsn := os.Getenv("EARN_JOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set EARN_JOY_TEST_DATABASE_URL to a fresh local earn_joy_test database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || config.ConnConfig.Host != "127.0.0.1" || config.ConnConfig.Database != "earn_joy_test" {
		t.Fatal("test requires a disposable local earn_joy_test database")
	}
	ctx := t.Context()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	defer func() {
		if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS loyal_yield CASCADE;
DROP TABLE IF EXISTS app_user_smart_accounts, app_users, earn_yield_push_state`); err != nil {
			t.Error(err)
		}
	}()
	_, err = pool.Exec(ctx, `CREATE SCHEMA loyal_yield;
CREATE TABLE app_users (id text PRIMARY KEY, subject_address text, created_at timestamptz);
CREATE TABLE app_user_smart_accounts (user_id text, settings_pda text, state text, solana_env text, updated_at timestamptz);
CREATE TABLE earn_yield_push_state (wallet_public_key text PRIMARY KEY, last_pushed_at timestamptz, last_pushed_earned_usd numeric, sent_campaigns jsonb, updated_at timestamptz);`)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../../../migrations/yield/0144_earn_yield_joy_progress.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		wallet := fmt.Sprintf("wallet%02d", i)
		if _, err := pool.Exec(ctx, `INSERT INTO app_users VALUES ($1,$1,now()-interval '2 days')`, wallet); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO app_user_smart_accounts VALUES ($1,$1,'ready',$2,now())`, wallet, smartAccountsSolanaEnv); err != nil {
			t.Fatal(err)
		}
	}
	// A second ready account for a wallet must not cause a second notification attempt.
	if _, err := pool.Exec(ctx, `INSERT INTO app_user_smart_accounts VALUES ('wallet00','older','ready',$1,now()-interval '1 day')`, smartAccountsSolanaEnv); err != nil {
		t.Fatal(err)
	}
	c := NewCrons(pool, Earn{YieldDB: pool, Cluster: Mainnet})
	seen := map[string]int{}
	var mu sync.Mutex
	c.Earnings = func(_ context.Context, _, settings, wallet string) (float64, bool, error) {
		if settings == "older" {
			return 0, false, errors.New("selected old settings")
		}
		mu.Lock()
		seen[wallet]++
		mu.Unlock()
		time.Sleep(1100 * time.Millisecond)
		return 0, true, nil
	}
	first, err := c.earnYieldJoyBatch(ctx, false, time.Second)
	if err != nil || first.Processed != 5 || !first.Truncated || first.Errors != 0 {
		t.Fatalf("first batch: %+v, %v", first, err)
	}
	cycle, after, completed, err := c.joyProgress(ctx, time.Now())
	if err != nil || after != "wallet04" || completed {
		t.Fatalf("progress: %s %s %v %v", cycle, after, completed, err)
	}
	// An unfinished scan must survive the next day's boundary.
	laterCycle, laterAfter, _, err := c.joyProgress(ctx, cycle.Add(25*time.Hour))
	if err != nil || !laterCycle.Equal(cycle) || laterAfter != after {
		t.Fatal("unfinished cycle reset at midnight")
	}
	c.Earnings = func(_ context.Context, _, _, wallet string) (float64, bool, error) {
		mu.Lock()
		seen[wallet]++
		mu.Unlock()
		if wallet == "wallet06" {
			return 0, false, errors.New("temporary earnings failure")
		}
		if wallet == "wallet07" {
			return 0, false, nil
		}
		return 0, true, nil
	}
	// A dry run must not advance progress or claim a notification.
	dry, err := c.EarnYieldJoy(ctx, true)
	if err != nil || dry.Processed != 7 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	_, dryAfter, _, err := c.joyProgress(ctx, time.Now())
	var claims int
	if err != nil || dryAfter != after {
		t.Fatal("dry run advanced cursor")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM earn_yield_push_state`).Scan(&claims); err != nil || claims != 5 {
		t.Fatalf("dry run wrote claims: %d %v", claims, err)
	}
	second, err := c.earnYieldJoyBatch(ctx, false, time.Second)
	if err != nil || second.Processed != 7 || second.Truncated || second.Errors != 1 || second.Unavailable != 1 {
		t.Fatalf("second batch: %+v %v", second, err)
	}
	for wallet, count := range seen {
		want := 2 // Dry run plus live continuation.
		if strings.Compare(wallet, "wallet04") <= 0 {
			want = 1
		}
		if count != want {
			t.Fatalf("%s processed %d times, want %d", wallet, count, want)
		}
	}
	idle, err := c.earnYieldJoyBatch(ctx, false, time.Second)
	if err != nil || idle.Processed != 0 {
		t.Fatalf("completed cycle repeated: %+v %v", idle, err)
	}
	_, nextAfter, done, err := c.joyProgress(ctx, cycle.Add(25*time.Hour))
	if err != nil || nextAfter != "" || done {
		t.Fatal("next daily cycle did not restart")
	}
	// Replaying a chunk after a crash must not claim a campaign twice.
	now := time.Now()
	claimed, err := c.claimJoyState(ctx, "claim", nil, []string{joyFirstCampaign}, now, 1)
	if err != nil || !claimed {
		t.Fatalf("first claim: %v %v", claimed, err)
	}
	claimed, err = c.claimJoyState(ctx, "claim", nil, []string{joyFirstCampaign}, now, 1)
	if err != nil || claimed {
		t.Fatalf("duplicate claim: %v %v", claimed, err)
	}
}
