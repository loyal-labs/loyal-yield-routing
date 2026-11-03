package bridge

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
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
	trackLookback = 7 * 24 * time.Hour
)

const (
	openState = `(state IN ('submitted', 'failed') OR (state = 'filled' AND destination_tx IS NULL) OR (state = 'refunded' AND refund_tx IS NULL))`

	openTransfers = `SELECT request_id, state FROM loyal_yield.bridge_transfers
WHERE ` + openState + ` AND submitted_at > now() - ` + trackWindow + `
ORDER BY submitted_at`

	advanceTransfer = `UPDATE loyal_yield.bridge_transfers
SET state = $3, destination_tx = coalesce(destination_tx, nullif($4, '')), refund_tx = coalesce(refund_tx, nullif($5, '')),
    settled_at = CASE WHEN $6 THEN coalesce(settled_at, now()) ELSE settled_at END, updated_at = now()
WHERE request_id = $1 AND state = $2 AND ` + openState + ` AND submitted_at > now() - ` + trackWindow

	markUnsettled = `UPDATE loyal_yield.bridge_transfers SET updated_at = now()
WHERE ` + openState + ` AND submitted_at <= now() - ` + trackWindow + `
  AND updated_at < submitted_at + ` + trackWindow + `
RETURNING request_id, state, submitted_at`

	trackedDepositAddresses = `SELECT address, solana_wallet, chain, asset FROM loyal_yield.relay_deposit_addresses
WHERE chain = $1 AND expires_at > now() - ` + trackWindow + `
ORDER BY address`

	insertDetected = `INSERT INTO loyal_yield.bridge_transfers
(request_id, solana_wallet, origin_address, deposit_address, chain, asset, amount_in_raw, amount_out_raw, state, origin_tx, submitted_at)
VALUES ($1, $2, $3, $3, $4, $5, $6::text::numeric, $7::text::numeric, $8, $9, $10)
ON CONFLICT (request_id) DO NOTHING`
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
	if t.Relay.APIKey == "" {
		slog.Warn("relay requests disabled: no api key")
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
	if t.Relay.APIKey != "" {
		if err := t.detect(ctx, Tron); err != nil {
			slog.Warn("bridge deposit detection failed", "chain", Tron.Chain, "error", engine.ErrorText(err))
		}
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
	polled := map[TransferState]int{TransferSubmitted: 0, TransferFailed: 0, TransferFilled: 0, TransferRefunded: 0}
	for _, o := range open {
		polled[o.state]++
	}
	for s, n := range polled {
		t.Facts.BridgeTransfersPolled(string(s), n)
	}

	var mu sync.Mutex
	var relayErrs int
	var firstRelayErr error
	var group errgroup.Group
	group.SetLimit(trackParallel)
	for _, o := range open {
		group.Go(func() error {
			status, err := t.Relay.Status(ctx, o.requestID)
			if err != nil {
				mu.Lock()
				relayErrs++
				if firstRelayErr == nil {
					firstRelayErr = err
				}
				mu.Unlock()
				return nil
			}
			return t.advance(ctx, o, status)
		})
	}
	err = group.Wait()
	if relayErrs > 0 {
		slog.Warn("bridge relay status failed", "rows", relayErrs, "error", engine.ErrorText(firstRelayErr))
	}
	if err != nil {
		return err
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
	var destinationTx, refundTx string
	if n := len(status.TxHashes); n > 0 {
		switch {
		case status.State == "success" && next == TransferFilled:
			destinationTx = status.TxHashes[n-1]
		case status.State == "refund" && next == TransferRefunded:
			refundTx = status.TxHashes[n-1]
		}
	}
	if next == o.state && destinationTx == "" && refundTx == "" {
		return nil
	}
	tag, err := t.DB.Exec(ctx, advanceTransfer, o.requestID, string(o.state), string(next), destinationTx, refundTx, next.Settled())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if next != o.state {
		t.Facts.BridgeTransfersAdvanced(string(next), 1)
	}
	slog.Info("bridge transfer advanced", "request_id", o.requestID, "from", o.state, "to", next, "destination_tx", destinationTx, "refund_tx", refundTx)
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

var tronTxHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

type trackedAddress struct {
	address, solanaWallet, chain, asset string
}

// detect records each Relay request that moved a deposit into one of Loyal's
// open deposit addresses on route, so the poll below follows it like any
// other transfer.
func (t *Tracker) detect(ctx context.Context, route DepositRoute) error {
	rows, err := t.DB.Query(ctx, trackedDepositAddresses, string(route.Chain))
	if err != nil {
		return err
	}
	addresses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (trackedAddress, error) {
		var a trackedAddress
		err := row.Scan(&a.address, &a.solanaWallet, &a.chain, &a.asset)
		return a, err
	})
	if err != nil {
		return err
	}
	since := time.Now().Add(-trackLookback)
	inserted := 0
	for batch := range slices.Chunk(addresses, RequestsBatch) {
		owners := make(map[string]trackedAddress, len(batch))
		list := make([]string, 0, len(batch))
		for _, a := range batch {
			owners[a.address] = a
			list = append(list, a.address)
		}
		requests, err := t.Relay.Requests(ctx, route.RelayChainID, list, since)
		if err != nil {
			return err
		}
		for _, r := range requests {
			owner, ok := owners[r.DepositAddress]
			if !ok {
				continue
			}
			if reason := unrecordable(route, owner, r); reason != "" {
				slog.Debug("relay deposit request not recorded", "request_id", r.ID, "deposit_address", r.DepositAddress, "reason", reason)
				continue
			}
			state, _ := TransferSubmitted.Advance(r.Status)
			tag, err := t.DB.Exec(ctx, insertDetected, r.ID, owner.solanaWallet, owner.address, owner.chain, owner.asset,
				r.AmountInRaw.String(), r.AmountOutRaw.String(), string(state), r.OriginTx, r.CreatedAt)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				inserted++
				slog.Info("bridge deposit detected", "request_id", r.ID, "deposit_address", owner.address, "state", state, "origin_tx", r.OriginTx)
			}
		}
	}
	slog.Debug("bridge deposit detection", "chain", route.Chain, "addresses", len(addresses), "inserted", inserted)
	return nil
}

func unrecordable(route DepositRoute, owner trackedAddress, r DepositRequest) string {
	switch {
	case r.OriginTx == "":
		return "no origin deposit yet"
	case !tronTxHash.MatchString(r.OriginTx):
		return "origin tx is not a Tron transaction hash"
	case r.Recipient != owner.solanaWallet:
		return "recipient is not the address owner"
	case r.OriginCurrency != route.Token:
		return "origin currency is not the route token"
	case r.AmountInRaw == nil || r.AmountInRaw.Sign() <= 0 || r.AmountOutRaw == nil:
		return "amounts unknown"
	}
	return ""
}
