package earn

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
)

// Inject cancellation at the real COMMIT boundary, after all actual SQL ran.
// No query results or financial state are fabricated by this tracer.
type rejectEarnCommit struct{ armed atomic.Bool }

func (r *rejectEarnCommit) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if r.armed.Load() && strings.EqualFold(data.SQL, "commit") {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return canceled
	}
	return ctx
}
func (*rejectEarnCommit) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestEnqueueFailedCommitReturnsNoOutcomeAndCanReplay(t *testing.T) {
	dsn := os.Getenv("OBSERVER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OBSERVER_TEST_DATABASE_URL is required")
	}
	endpoint, err := url.Parse(dsn)
	if err != nil || endpoint == nil {
		t.Fatal("transaction proof fixture URL is invalid")
	}
	port, portErr := strconv.Atoi(endpoint.Port())
	if (endpoint.Scheme != "postgresql" && endpoint.Scheme != "postgres") || endpoint.Hostname() != "127.0.0.1" || portErr != nil || port < 1024 || port > 65535 || endpoint.User == nil || endpoint.User.Username() != "workers_v2" || endpoint.Path != "/workers_v2_observer" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		t.Fatal("transaction proof requires the isolated workers_v2 observer fixture")
	}
	if _, hasPassword := endpoint.User.Password(); hasPassword {
		t.Fatal("transaction proof fixture must not contain credentials")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	tracer := &rejectEarnCommit{}
	cfg.ConnConfig.Tracer = tracer
	cfg.MaxConns = 1
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	consumer := fmt.Sprintf("workers-v2-earn-commit-%d", time.Now().UnixNano())
	// Cleanup must run before the borrowed connection pool is closed.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanup, `DELETE FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1`, consumer)
		_, _ = pool.Exec(cleanup, `DELETE FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name=$1`, consumer)
	}()
	store := NewStore(pool)
	vaults := []watch.Vault{{Settings: "11111111111111111111111111111111", Vault: "11111111111111111111111111111111", VaultIndex: 1}}
	tracer.armed.Store(true)
	outcome, err := store.Enqueue(ctx, consumer, consumer, 101, NormalizedUpdate{Slot: 101}, vaults, "unmatched-fixture-account")
	tracer.armed.Store(false)
	if !errors.Is(err, context.Canceled) || outcome != (EnqueueOutcome{}) {
		t.Fatalf("failed commit published outcome=%+v error=%v", outcome, err)
	}
	var jobs int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1`, consumer).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	cursor, err := store.ReplayCursor(ctx, consumer)
	if err != nil || jobs != 0 || cursor != 0 {
		t.Fatalf("failed commit left jobs=%d cursor=%d error=%v", jobs, cursor, err)
	}
	outcome, err = store.Enqueue(ctx, consumer, consumer, 101, NormalizedUpdate{Slot: 101}, vaults, "unmatched-fixture-account")
	if err != nil || outcome.InsertedJobs != 1 || outcome.Cursor != 101 {
		t.Fatalf("exact replay after failed commit outcome=%+v error=%v", outcome, err)
	}
}
