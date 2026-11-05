package webapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"
)

// The Vercel crons, run from Go. Each is the body of one
// web/apps/web/src/app/api/cron/<name>/route.ts minus its CRON_SECRET hop.

type Crons struct {
	h        earnRoutes
	Earnings func(ctx context.Context, cluster, settings, wallet string) (lifetimeEarnedUSD float64, available bool, err error)
}

func NewCrons(appDB *pgxpool.Pool, earn Earn) Crons {
	return Crons{h: newEarnRoutes(nil, appDB, earn)}
}

var errCronRunning = errors.New("another run holds the lock")

// withRunLock holds a transaction-scoped advisory lock on a connection of its
// own for the whole task, so two runs of one job (a timer tick and a manual
// run, or a run that outlived its tick) never overlap, and a pooled endpoint
// cannot hand the lock to another session. Nothing else is written in it.
func withRunLock(ctx context.Context, pool *pgxpool.Pool, name string, task func(context.Context) error) (bool, error) {
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout = 0`); err != nil {
		return false, err
	}
	var acquired bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, "loyal-web-cron:"+name).Scan(&acquired); err != nil || !acquired {
		return false, err
	}
	return true, task(ctx)
}

const (
	reconcileStale = 5 * time.Minute
	reconcileLimit = 25
)

type SmartAccountReconcileSummary struct {
	Scanned  int `json:"scanned"`
	Repaired int `json:"repaired"`
	Failed   int `json:"failed"`
	Skipped  int `json:"skipped"`
}

type staleSmartAccount struct {
	id, userID, state string
}

func (c Crons) staleSmartAccounts(ctx context.Context, staleBefore time.Time) ([]staleSmartAccount, error) {
	rows, err := c.h.appDB.Query(ctx,
		`SELECT id::text, user_id::text, state
FROM app_user_smart_accounts
WHERE state IN ('provisioning', 'failed') AND updated_at <= $1
ORDER BY updated_at ASC
LIMIT $2`, staleBefore, reconcileLimit)
	if err != nil {
		return nil, fmt.Errorf("list recoverable smart accounts: %w", err)
	}
	defer rows.Close()
	var out []staleSmartAccount
	for rows.Next() {
		var record staleSmartAccount
		if err := rows.Scan(&record.id, &record.userID, &record.state); err != nil {
			return nil, fmt.Errorf("list recoverable smart accounts: %w", err)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// SmartAccountReconcile re-runs ensureSmartAccount for every provisioning or
// failed record untouched for five minutes, oldest first, 25 at a time.
func (c Crons) SmartAccountReconcile(ctx context.Context) (SmartAccountReconcileSummary, error) {
	var summary SmartAccountReconcileSummary
	if c.h.Chain == nil {
		return summary, errors.New("WEB_SOLANA_RPC_URL is not set")
	}
	acquired, err := withRunLock(ctx, c.h.appDB, "smart-account-reconcile", func(ctx context.Context) error {
		records, err := c.staleSmartAccounts(ctx, time.Now().Add(-reconcileStale))
		if err != nil {
			return err
		}
		summary.Scanned = len(records)
		for _, record := range records {
			var subject string
			err := c.h.appDB.QueryRow(ctx, `SELECT subject_address FROM app_users WHERE id = $1`, record.userID).Scan(&subject)
			if errors.Is(err, pgx.ErrNoRows) {
				summary.Skipped++
				slog.WarnContext(ctx, "[smart-accounts] skipping reconciliation for missing user", "userId", record.userID, "smartAccountRecordId", record.id)
				continue
			}
			if err != nil {
				return fmt.Errorf("read app user %s: %w", record.userID, err)
			}
			wallet, err := solana.PublicKeyFromBase58(subject)
			if err == nil {
				_, err = c.h.ensureSmartAccount(ctx, record.userID, wallet)
			}
			if err != nil {
				summary.Failed++
				slog.ErrorContext(ctx, "[smart-accounts] reconciliation failed", "userId", record.userID, "smartAccountRecordId", record.id, "state", record.state, "error", err.Error())
				continue
			}
			summary.Repaired++
		}
		return nil
	})
	if err == nil && !acquired {
		return summary, errCronRunning
	}
	return summary, err
}

const telemetryHeartbeatEvery = 5 * time.Minute

// TelemetryHeartbeat exports the synthetic log record the "Web telemetry
// heartbeat" alert watches: service loyal-frontend, environment production or
// prod, event kind telemetry_heartbeat.
func (c Crons) TelemetryHeartbeat(ctx context.Context, release, environment string) bool {
	nano := strconv.FormatInt(time.Now().UnixNano(), 10)
	return c.h.exportOtlpPayload(ctx, otlpLogsPayload{ResourceLogs: []otlpResourceLogs{{
		Resource: obsObservedResource("loyal-frontend", release, environment),
		ScopeLogs: []otlpScopeLogs{{
			Scope: otlpScope{Name: "loyal.frontend.telemetry-health", Version: "1"},
			LogRecords: []otlpLogRecord{{
				TimeUnixNano:         nano,
				ObservedTimeUnixNano: nano,
				SeverityNumber:       9,
				SeverityText:         "INFO",
				Body:                 otlpLogBody{StringValue: "telemetry.heartbeat"},
				Attributes: []otlpAttribute{
					obsStringAttribute("loyal.event.kind", "telemetry_heartbeat"),
					obsStringAttribute("loyal.traffic.kind", "synthetic"),
				},
			}},
		}},
	}}}, "logs")
}

// RunTelemetryHeartbeat emits one heartbeat now and one every five minutes
// until ctx ends.
func (c Crons) RunTelemetryHeartbeat(ctx context.Context, release, environment string) {
	ticker := time.NewTicker(telemetryHeartbeatEvery)
	defer ticker.Stop()
	for {
		if !c.TelemetryHeartbeat(ctx, release, environment) && ctx.Err() == nil {
			slog.Warn("telemetry heartbeat not delivered")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
