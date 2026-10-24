package webapp

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Port of earnings-cache.server.ts: serve a fresh stored snapshot, fall back
// to stale-while-revalidate, and rebuild under a transaction-scoped advisory
// lock so concurrent requests share one rebuild per scope.

const earningsFreshMs = int64(5 * 60 * 1000)

// earningsRefreshLockKey mirrors the JSON.stringify lock key (cache:87): a
// JS array literal with the scope fields in order.
func earningsRefreshLockKey(input earningsReadInput, timezone string) string {
	var w jsonWriter
	w.raw(`["earn-earnings-refresh",`)
	w.str(input.cluster)
	w.raw(",")
	w.str(input.walletAddress)
	w.raw(",")
	w.str(input.settings)
	w.raw(",")
	w.bigint(int64(input.vaultIndex))
	w.raw(",")
	w.str(timezone)
	w.raw("]")
	return w.String()
}

// isFresh ports cache:63: fresh payload, same source revision, generated this
// calendar day in the scope timezone, and younger than five minutes.
func isFresh(snapshot earningsSnapshotRecord, sourceRevision string, now time.Time, timezone string) bool {
	if snapshot.payload.Freshness != "fresh" {
		return false
	}
	if snapshot.sourceRevision == nil || *snapshot.sourceRevision != sourceRevision {
		return false
	}
	age := now.UnixMilli() - snapshot.generatedAt.UnixMilli()
	if age < 0 || age >= earningsFreshMs {
		return false
	}
	return localDayString(snapshot.generatedAt, timezone) == localDayString(now, timezone)
}

// localDayString is Intl.DateTimeFormat("en-CA", {timeZone}) — YYYY-MM-DD.
func localDayString(t time.Time, timezone string) string {
	p := getZonedParts(t, timezone)
	return itoa4(p.year) + "-" + pad2(p.month) + "-" + pad2(p.day)
}

func pad2(v int) string {
	if v < 10 {
		return "0" + formatInt64(int64(v))
	}
	return formatInt64(int64(v))
}

func itoa4(v int) string { return formatInt64(int64(v)) }

// withEarningsRefreshLock ports withEarnEarningsRefreshLock (cache:30). The
// advisory lock is transaction-scoped on its own connection, opened outside
// the pool so a held lock never takes a connection the refresh reads need;
// closing the connection, or the process dying, releases it.
func withEarningsRefreshLock(ctx context.Context, pool *pgxpool.Pool, key string, task func(context.Context) error) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = 10000`); err != nil {
		return false, err
	}
	var acquired bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, key).Scan(&acquired); err != nil {
		return false, err
	}
	if !acquired {
		return false, nil
	}
	if err := task(ctx); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// earningsScheduleRefresh is the `after` callback shape the route passes in
// (unit 2 wires it to a background goroutine).
type earningsScheduleRefresh func(func(context.Context))

// readCachedEarnEarningsRangeSet ports cache:80.
func readCachedEarnEarningsRangeSet(ctx context.Context, pool *pgxpool.Pool, input earningsReadInput, schedule earningsScheduleRefresh, deps earningsReadDeps) (EarningsRangeSet, error) {
	timezone := normalizeEarningsTimezone(input.timezone)
	input.timezone = timezone
	key := earningsRefreshLockKey(input, timezone)

	snapshot, err := deps.loadSnapshot(ctx, input, timezone)
	if err != nil {
		return EarningsRangeSet{}, err
	}
	usable := snapshot != nil && isServerVerifiedEarnEarningsPayload(&snapshot.payload)
	revision, err := deps.loadSourceRevision(ctx, input)
	if err != nil {
		return EarningsRangeSet{}, err
	}
	now := deps.now()
	if usable && isFresh(*snapshot, revision, now, timezone) {
		return snapshot.payload, nil
	}

	refresh := func(ctx context.Context) (EarningsRangeSet, bool, error) {
		var result EarningsRangeSet
		found := false
		acquired, err := withEarningsRefreshLock(ctx, pool, key, func(ctx context.Context) error {
			latest, err := deps.loadSnapshot(ctx, input, timezone)
			if err != nil {
				return err
			}
			latestRevision, err := deps.loadSourceRevision(ctx, input)
			if err != nil {
				return err
			}
			if latest != nil && isServerVerifiedEarnEarningsPayload(&latest.payload) &&
				isFresh(*latest, latestRevision, deps.now(), timezone) {
				result = latest.payload
			} else {
				result, err = readEarningsRangeSet(ctx, input, deps, true)
				if err != nil {
					return err
				}
			}
			found = true
			return nil
		})
		if err != nil {
			return EarningsRangeSet{}, false, err
		}
		_ = acquired
		return result, found, nil
	}

	if !usable {
		result, found, err := refresh(ctx)
		if err != nil {
			return EarningsRangeSet{}, err
		}
		if found {
			return result, nil
		}
		return EarningsRangeSet{}, &earningsUnavailableError{
			code:       earningsUnavailableCode,
			message:    "Earnings history refresh is in progress.",
			detailCode: earningsUnavailableCode,
		}
	}

	stored := *snapshot
	schedule(func(ctx context.Context) {
		if _, _, err := refresh(ctx); err != nil {
			slog.Error("[earnings-cache] refresh failed", "errorName", errorNameOf(err))
		}
	})
	stale := stored.payload
	ageMs := deps.now().UnixMilli() - stored.generatedAt.UnixMilli()
	if ageMs < 0 {
		ageMs = 0
	}
	stale.Freshness = "stale"
	stale.SnapshotAgeMs = &ageMs
	if stored.sourceRevision != nil && *stored.sourceRevision == revision {
		stale.StaleReason = strPtr("cache_expired")
	} else {
		stale.StaleReason = strPtr("history_changed")
	}
	return stale, nil
}
