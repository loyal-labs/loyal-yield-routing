package webapp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func earningsLockTestPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	dsn := os.Getenv("LOYAL_WEB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable local Postgres cluster")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/tmp/loyal-web-pg-") || config.ConnConfig.Database != "loyal_web_pilot" {
		t.Fatal("test requires the loyal_web_pilot database on a /tmp/loyal-web-pg-* Unix socket")
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestEarningsRefreshLockLeavesThePoolToTheRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool := earningsLockTestPool(t, ctx)

	acquired, err := withEarningsRefreshLock(ctx, pool, "earnings-lock-test", func(ctx context.Context) error {
		var one int
		return pool.QueryRow(ctx, `SELECT 1`).Scan(&one)
	})

	if err != nil || !acquired {
		t.Fatalf("acquired=%v err=%v, want the refresh to read through the pool while the lock is held", acquired, err)
	}
}

func TestEarningsRefreshLockLeavesTheRebuildItsCallersDeadline(t *testing.T) {
	deadline := time.Now().Add(5 * time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	pool := earningsLockTestPool(t, ctx)

	var taskDeadline time.Time
	acquired, err := withEarningsRefreshLock(ctx, pool, "earnings-lock-deadline-test", func(ctx context.Context) error {
		taskDeadline, _ = ctx.Deadline()
		return nil
	})

	if err != nil || !acquired || !taskDeadline.Equal(deadline) {
		t.Fatalf("acquired=%v err=%v task deadline=%s, want the caller's %s: a cold vault history rebuild runs past 15s", acquired, err, taskDeadline, deadline)
	}
}

func TestEarningsRefreshLockBoundsTheHolderToTheRebuildBudget(t *testing.T) {
	setup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool := earningsLockTestPool(t, setup)

	var taskDeadline time.Time
	var bounded bool
	acquired, err := withEarningsRefreshLock(context.Background(), pool, "earnings-lock-budget-test", func(ctx context.Context) error {
		taskDeadline, bounded = ctx.Deadline()
		return nil
	})
	latest := time.Now().Add(earningsRefreshBudget)

	if err != nil || !acquired || !bounded || taskDeadline.After(latest) {
		t.Fatalf("acquired=%v err=%v bounded=%v task deadline=%s, want a deadline by %s: a hung foreground rebuild must not hold the lock and a slot forever", acquired, err, bounded, taskDeadline, latest)
	}
}

func TestEarningsRefreshLockOutlivesTheIdleInTransactionTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := earningsLockTestPool(t, ctx).Config()
	config.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "200ms"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	acquired, err := withEarningsRefreshLock(ctx, pool, "earnings-lock-idle-test", func(ctx context.Context) error {
		_, err := pool.Exec(ctx, `SELECT pg_sleep(0.6)`)
		return err
	})

	if err != nil || !acquired {
		t.Fatalf("acquired=%v err=%v, want the lock to outlive a rebuild longer than the server's idle_in_transaction_session_timeout (5min on Yield, the rebuild budget)", acquired, err)
	}
}

func TestEarningsReadAnswersAtOnceWhileOtherRebuildsHoldEverySlot(t *testing.T) {
	for range cap(earningsRefreshLockSlots) {
		earningsRefreshLockSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(earningsRefreshLockSlots) {
			<-earningsRefreshLockSlots
		}
	})
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	generatedAt := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	unverified := &earningsSnapshotRecord{generatedAt: generatedAt, payload: EarningsRangeSet{Outcome: "ready", Freshness: "fresh"}}
	read := func(snapshot *earningsSnapshotRecord) (EarningsRangeSet, time.Duration, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		deps := earningsReadDeps{
			loadSnapshot: func(context.Context, earningsReadInput, string) (*earningsSnapshotRecord, error) {
				return snapshot, nil
			},
			loadSourceRevision: func(context.Context, earningsReadInput) (string, error) { return "revision", nil },
			now:                func() time.Time { return generatedAt.Add(time.Hour) },
		}
		input := earningsReadInput{cluster: "mainnet", settings: "settings", timezone: "UTC", vaultIndex: 1, walletAddress: "wallet"}
		start := time.Now()
		payload, err := readCachedEarnEarningsRangeSet(ctx, nil, input, func(func(context.Context)) {}, deps)
		return payload, time.Since(start), err
	}

	payload, took, err := read(unverified)
	if err != nil || took > 200*time.Millisecond || payload.Freshness != "stale" || payload.StaleReason == nil || *payload.StaleReason != "refresh_in_progress" {
		t.Fatalf("with a stored snapshot: took %s err=%v freshness=%q, want the snapshot served stale for refresh_in_progress at once", took, err, payload.Freshness)
	}

	_, took, err = read(nil)
	var unavailable *earningsUnavailableError
	if !errors.As(err, &unavailable) || unavailable.code != "earnings_unavailable" || took > 200*time.Millisecond {
		t.Fatalf("without a snapshot: took %s err=%v, want earnings_unavailable at once", took, err)
	}
	if !strings.Contains(logs.String(), `"msg":"earnings-read unavailable"`) || !strings.Contains(logs.String(), `"cause":"refresh_in_progress"`) {
		t.Fatalf("logs %s, want earnings-read unavailable with cause refresh_in_progress", logs.String())
	}
}
