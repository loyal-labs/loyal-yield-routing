package bridge

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"golang.org/x/sync/errgroup"
)

const (
	trackInterval = 10 * time.Second
	trackDeadline = 30 * time.Second
	trackParallel = 8
	trackWindow   = "interval '7 days'"
)

const (
	openTransfers = `SELECT request_id, state FROM loyal_yield.bridge_transfers
WHERE state IN ('submitted', 'failed') AND submitted_at > now() - ` + trackWindow

	advanceTransfer = `UPDATE loyal_yield.bridge_transfers
SET state = $3, destination_tx = coalesce($4, destination_tx),
    settled_at = CASE WHEN $5 THEN now() ELSE settled_at END, updated_at = now()
WHERE request_id = $1 AND state = $2 AND submitted_at > now() - ` + trackWindow

	markUnsettled = `UPDATE loyal_yield.bridge_transfers SET updated_at = now()
WHERE state IN ('submitted', 'failed') AND submitted_at <= now() - ` + trackWindow + `
  AND updated_at < submitted_at + ` + trackWindow + `
RETURNING request_id, state, submitted_at`
)

type Tracker struct {
	DB    *pgxpool.Pool
	Relay *Relay
	Facts *engine.Facts
}

func (t *Tracker) Run(ctx context.Context) error {
	for _, s := range []TransferState{TransferFilled, TransferRefunded, TransferFailed} {
		t.Facts.BridgeTransfersAdvanced(string(s), 0)
	}
	ticker := time.NewTicker(trackInterval)
	defer ticker.Stop()
	for {
		tick, cancel := context.WithTimeout(ctx, trackDeadline)
		err := t.Tick(tick)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("bridge tracker tick failed", "error", engine.ErrorText(err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (t *Tracker) Tick(ctx context.Context) error {
	start := time.Now()
	if err := t.dropUnsettled(ctx); err != nil {
		return err
	}
	rows, err := t.DB.Query(ctx, openTransfers)
	if err != nil {
		return err
	}
	open, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (openTransfer, error) {
		var o openTransfer
		err := row.Scan(&o.requestID, &o.state)
		return o, err
	})
	if err != nil {
		return err
	}
	polled := map[TransferState]int{TransferSubmitted: 0, TransferFailed: 0}
	for _, o := range open {
		polled[o.state]++
	}
	for s, n := range polled {
		t.Facts.BridgeTransfersPolled(string(s), n)
	}

	var mu sync.Mutex
	var relayErrs int
	var firstRelayErr error
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(trackParallel)
	for _, o := range open {
		group.Go(func() error {
			status, err := t.Relay.Status(gctx, o.requestID)
			if err != nil {
				mu.Lock()
				relayErrs++
				if firstRelayErr == nil {
					firstRelayErr = err
				}
				mu.Unlock()
				return nil
			}
			return t.advance(gctx, o, status)
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	if relayErrs > 0 {
		slog.Warn("bridge relay status failed", "rows", relayErrs, "error", engine.ErrorText(firstRelayErr))
	}
	slog.Debug("bridge tracker tick", "polled", len(open), "relay_errors", relayErrs, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

type openTransfer struct {
	requestID string
	state     TransferState
}

func (t *Tracker) advance(ctx context.Context, o openTransfer, status Status) error {
	next, known := o.state.Advance(status.State)
	if !known {
		slog.Debug("bridge relay status unknown", "request_id", o.requestID, "relay_status", status.State)
		return nil
	}
	if next == o.state {
		return nil
	}
	var destinationTx *string
	if n := len(status.TxHashes); n > 0 {
		destinationTx = &status.TxHashes[n-1]
	}
	tag, err := t.DB.Exec(ctx, advanceTransfer, o.requestID, string(o.state), string(next), destinationTx, next.Settled())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		t.Facts.BridgeTransfersAdvanced(string(next), 1)
		slog.Info("bridge transfer advanced", "request_id", o.requestID, "from", o.state, "to", next, "destination_tx", destinationTx)
	}
	return nil
}

func (t *Tracker) dropUnsettled(ctx context.Context) error {
	rows, err := t.DB.Query(ctx, markUnsettled)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var requestID, state string
		var submittedAt time.Time
		if err := rows.Scan(&requestID, &state, &submittedAt); err != nil {
			return err
		}
		slog.Warn("bridge transfer unsettled", "request_id", requestID, "state", state, "submitted_at", submittedAt)
	}
	return rows.Err()
}
