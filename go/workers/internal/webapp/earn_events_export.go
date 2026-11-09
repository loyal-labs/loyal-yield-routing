package webapp

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

const (
	earnDepositsStream    = "earn-deposits"
	earnWithdrawalsStream = "earn-withdrawals"
	earnEventsLimit       = 2000
	exportOverlap         = time.Hour
	// Mixpanel's /track refuses events older than five days; stay inside that.
	exportMaxAge = 4 * 24 * time.Hour
)

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
		build func(rows pgx.Rows) (mixpanelEvent, error)
	}
	streams := []stream{
		{earnDepositsStream, `SELECT id, wallet_address, principal_amount_raw, balance_sweep_execution_id IS NOT NULL, confirmed_at
FROM loyal_yield.user_yield_position_deposits
WHERE vault_index = $1 AND confirmed_at > $2 AND confirmed_at <= now()
ORDER BY confirmed_at, id LIMIT $3`, &summary.Deposits, scanDepositEvent},
		{earnWithdrawalsStream, `SELECT id, wallet_address, withdrawn_amount_raw, source_type, confirmed_at
FROM loyal_yield.user_yield_position_withdrawals
WHERE vault_index = $1 AND confirmed_at > $2 AND confirmed_at <= now()
ORDER BY confirmed_at, id LIMIT $3`, &summary.Withdrawals, scanWithdrawalEvent},
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
		events, newest, err := c.readEarnEvents(ctx, s.query, start, s.build)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		*s.count = len(events)
		if dryRun || len(events) == 0 {
			continue
		}
		if err := c.Mixpanel.track(ctx, events); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := c.h.appDB.Exec(ctx, `UPDATE analytics_export_watermarks SET confirmed_at = GREATEST(confirmed_at, $2), updated_at = now() WHERE stream = $1`, s.name, newest); err != nil {
			errs = append(errs, err)
		}
	}
	return summary, errors.Join(errs...)
}

func (c Crons) readEarnEvents(ctx context.Context, query string, start time.Time, build func(pgx.Rows) (mixpanelEvent, error)) ([]mixpanelEvent, time.Time, error) {
	rows, err := c.h.YieldDB.Query(ctx, query, int16(earnprep.EarnVaultIndex), start, earnEventsLimit)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	var events []mixpanelEvent
	var newest time.Time
	for rows.Next() {
		event, err := build(rows)
		if err != nil {
			return nil, time.Time{}, err
		}
		if event.Time.After(newest) {
			newest = event.Time
		}
		events = append(events, event)
	}
	return events, newest, rows.Err()
}

func scanDepositEvent(rows pgx.Rows) (mixpanelEvent, error) {
	var id, raw int64
	var wallet string
	var sweep bool
	var at time.Time
	if err := rows.Scan(&id, &wallet, &raw, &sweep, &at); err != nil {
		return mixpanelEvent{}, err
	}
	source := "manual"
	if sweep {
		source = "autodeposit"
	}
	return mixpanelEvent{
		Name: "Earn Deposit Confirmed", DistinctID: "mob:" + wallet, InsertID: "earn-deposit:" + strconv.FormatInt(id, 10), Time: msTime(at),
		Properties: map[string]any{"amount_usd": float64(raw) / 1e6, "source": source, "deposit_id": id},
	}, nil
}

func scanWithdrawalEvent(rows pgx.Rows) (mixpanelEvent, error) {
	var id, raw int64
	var wallet string
	var sourceType *string
	var at time.Time
	if err := rows.Scan(&id, &wallet, &raw, &sourceType, &at); err != nil {
		return mixpanelEvent{}, err
	}
	properties := map[string]any{"amount_usd": float64(raw) / 1e6, "source": "manual", "withdrawal_id": id}
	if sourceType != nil {
		properties["source_type"] = *sourceType
	}
	return mixpanelEvent{
		Name: "Earn Withdrawal Confirmed", DistinctID: "mob:" + wallet, InsertID: "earn-withdrawal:" + strconv.FormatInt(id, 10), Time: msTime(at),
		Properties: properties,
	}, nil
}
