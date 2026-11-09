package webapp

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

const (
	earnDepositsStream    = "earn-deposits"
	earnWithdrawalsStream = "earn-withdrawals"
	earnEventsMaxPages    = 25
	exportOverlap         = time.Hour
	// Mixpanel's /track refuses events older than five days; stay inside that.
	exportMaxAge = 4 * 24 * time.Hour
)

// earnEventsPageSize is a var so tests can shrink it.
var earnEventsPageSize = 2000

// EarnEventsSummary reports one export run. In a dry run Seeded means a
// watermark would be seeded, and nothing is written or sent.
type EarnEventsSummary struct {
	Deposits    int       `json:"deposits"`
	Withdrawals int       `json:"withdrawals"`
	Seeded      bool      `json:"seeded"`
	DryRun      bool      `json:"dryRun"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
}

// exportWindowStart is max(watermark-1h, now-4d): the hour of overlap catches
// rows confirmed slightly out of order (Mixpanel deduplicates by $insert_id),
// and the cap keeps a long outage inside /track's age limit.
func exportWindowStart(watermark, now time.Time) time.Time {
	start := watermark.Add(-exportOverlap)
	if floor := now.Add(-exportMaxAge); start.Before(floor) {
		return floor
	}
	return start
}

func (c Crons) EarnEventsMixpanel(ctx context.Context, dryRun bool) (EarnEventsSummary, error) {
	summary := EarnEventsSummary{DryRun: dryRun}
	if c.h.YieldDB == nil || c.Mixpanel == nil {
		return summary, errors.New("earn-events-mixpanel needs WEB_YIELD_DATABASE_URL and MIXPANEL_PROJECT_TOKEN")
	}
	task := func(ctx context.Context) error {
		var err error
		summary, err = c.earnEventsMixpanel(ctx, dryRun)
		return err
	}
	if dryRun {
		return summary, task(ctx)
	}
	acquired, err := withRunLock(ctx, c.h.appDB, "earn-events-mixpanel", task)
	if err == nil && !acquired {
		return summary, errCronRunning
	}
	return summary, err
}

func (c Crons) earnEventsMixpanel(ctx context.Context, dryRun bool) (EarnEventsSummary, error) {
	now := time.Now()
	summary := EarnEventsSummary{DryRun: dryRun, To: now}
	type stream struct {
		name  string
		query string
		count *int
		build earnEventScanner
	}
	streams := []stream{
		{earnDepositsStream, `SELECT id, wallet_address, principal_amount_raw, balance_sweep_execution_id IS NOT NULL, confirmed_at
FROM loyal_yield.user_yield_position_deposits
WHERE vault_index = $1 AND (confirmed_at, id) > ($2, $3) AND confirmed_at <= now()
ORDER BY confirmed_at, id LIMIT $4`, &summary.Deposits, scanDepositEvent},
		{earnWithdrawalsStream, `SELECT id, wallet_address, withdrawn_amount_raw, source_type, confirmed_at
FROM loyal_yield.user_yield_position_withdrawals
WHERE vault_index = $1 AND (confirmed_at, id) > ($2, $3) AND confirmed_at <= now()
ORDER BY confirmed_at, id LIMIT $4`, &summary.Withdrawals, scanWithdrawalEvent},
	}
	var errs []error
	for _, s := range streams {
		var watermark time.Time
		err := c.h.appDB.QueryRow(ctx, `SELECT confirmed_at FROM analytics_export_watermarks WHERE stream = $1`, s.name).Scan(&watermark)
		if errors.Is(err, pgx.ErrNoRows) {
			// First run: no history backfill, start from now.
			summary.Seeded = true
			if !dryRun {
				_, err = c.h.appDB.Exec(ctx, `INSERT INTO analytics_export_watermarks (stream, confirmed_at) VALUES ($1, now()) ON CONFLICT (stream) DO NOTHING`, s.name)
			} else {
				err = nil
			}
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		start := exportWindowStart(watermark, now)
		if summary.From.IsZero() || start.Before(summary.From) {
			summary.From = start
		}
		sent, err := c.exportStream(ctx, s.name, s.query, start, s.build, dryRun)
		*s.count = sent
		if err != nil {
			errs = append(errs, err)
		}
	}
	return summary, errors.Join(errs...)
}

type earnEventScanner func(rows pgx.Rows) (event mixpanelEvent, id int64, at time.Time, err error)

// exportStream pages through the window by (confirmed_at, id), advancing the
// watermark after each page Mixpanel accepts, so a busy hour cannot pin it.
func (c Crons) exportStream(ctx context.Context, stream, query string, start time.Time, build earnEventScanner, dryRun bool) (int, error) {
	// The first page is strictly after start: no id exceeds MaxInt64.
	lastAt, lastID := start, int64(math.MaxInt64)
	sent := 0
	for page := 0; page < earnEventsMaxPages; page++ {
		events, newest, newestID, err := c.readEarnEventsPage(ctx, query, lastAt, lastID, build)
		if err != nil {
			return sent, err
		}
		if len(events) == 0 {
			return sent, nil
		}
		sent += len(events)
		if !dryRun {
			if err := c.Mixpanel.track(ctx, events); err != nil {
				return sent - len(events), err
			}
			if _, err := c.h.appDB.Exec(ctx, `UPDATE analytics_export_watermarks SET confirmed_at = GREATEST(confirmed_at, $2), updated_at = now() WHERE stream = $1`, stream, newest); err != nil {
				return sent, err
			}
		}
		if len(events) < earnEventsPageSize {
			return sent, nil
		}
		lastAt, lastID = newest, newestID
	}
	slog.Warn("[earn-events-mixpanel] page cap reached", "stream", stream)
	return sent, nil
}

func (c Crons) readEarnEventsPage(ctx context.Context, query string, lastAt time.Time, lastID int64, build earnEventScanner) ([]mixpanelEvent, time.Time, int64, error) {
	rows, err := c.h.YieldDB.Query(ctx, query, int16(earnprep.EarnVaultIndex), lastAt, lastID, earnEventsPageSize)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	defer rows.Close()
	var events []mixpanelEvent
	var newest time.Time
	var newestID int64
	for rows.Next() {
		event, id, at, err := build(rows)
		if err != nil {
			return nil, time.Time{}, 0, err
		}
		newest, newestID = at, id // rows arrive ordered, so the last is the newest
		events = append(events, event)
	}
	return events, newest, newestID, rows.Err()
}

func scanDepositEvent(rows pgx.Rows) (mixpanelEvent, int64, time.Time, error) {
	var id, raw int64
	var wallet string
	var sweep bool
	var at time.Time
	if err := rows.Scan(&id, &wallet, &raw, &sweep, &at); err != nil {
		return mixpanelEvent{}, 0, time.Time{}, err
	}
	source := "manual"
	if sweep {
		source = "autodeposit"
	}
	return mixpanelEvent{
		Name: "Earn Deposit Confirmed", DistinctID: "mob:" + wallet, InsertID: "earn-deposit:" + strconv.FormatInt(id, 10), Time: msTime(at),
		Properties: map[string]any{"amount_usd": float64(raw) / 1e6, "source": source, "deposit_id": id},
	}, id, at, nil
}

func scanWithdrawalEvent(rows pgx.Rows) (mixpanelEvent, int64, time.Time, error) {
	var id, raw int64
	var wallet string
	var sourceType *string
	var at time.Time
	if err := rows.Scan(&id, &wallet, &raw, &sourceType, &at); err != nil {
		return mixpanelEvent{}, 0, time.Time{}, err
	}
	properties := map[string]any{"amount_usd": float64(raw) / 1e6, "source": "manual", "withdrawal_id": id}
	if sourceType != nil {
		properties["source_type"] = *sourceType
	}
	return mixpanelEvent{
		Name: "Earn Withdrawal Confirmed", DistinctID: "mob:" + wallet, InsertID: "earn-withdrawal:" + strconv.FormatInt(id, 10), Time: msTime(at),
		Properties: properties,
	}, id, at, nil
}
