package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
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

	adoptDeadline    = 10 * time.Second
	LostSubmitWindow = 2 * time.Hour
	lostSubmitBatch  = 32

	abandonedQuoteAge   = "interval '24 hours'"
	abandonedQuoteBatch = 500
)

const (
	openState = `(state IN ('submitted', 'failed') OR (state = 'filled' AND destination_tx IS NULL) OR (state = 'refunded' AND refund_tx IS NULL))`

	openTransfers = `SELECT request_id, state, CASE WHEN deposit_address IS NULL THEN '' ELSE solana_wallet END FROM loyal_yield.bridge_transfers
WHERE ` + openState + ` AND submitted_at > now() - ` + trackWindow + `
ORDER BY submitted_at`

	advanceTransfer = `UPDATE loyal_yield.bridge_transfers
SET state = $3, destination_tx = coalesce(destination_tx, nullif($4, '')), refund_tx = coalesce(refund_tx, nullif($5, '')),
    amount_in_raw = coalesce($7::text::numeric, amount_in_raw), amount_out_raw = coalesce($8::text::numeric, amount_out_raw),
    settled_at = CASE WHEN $6 THEN coalesce(settled_at, now()) ELSE settled_at END, updated_at = now()
WHERE request_id = $1 AND state = $2 AND ` + openState + ` AND submitted_at > now() - ` + trackWindow

	markUnsettled = `UPDATE loyal_yield.bridge_transfers SET updated_at = now()
WHERE ` + openState + ` AND submitted_at <= now() - ` + trackWindow + `
  AND updated_at < submitted_at + ` + trackWindow + `
RETURNING request_id, state, submitted_at`

	adoptSubmit = `UPDATE loyal_yield.bridge_transfers
SET state = 'submitted', origin_tx = $2, submitted_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE request_id = $1 AND state = 'quoted'`

	abandonedQuote = `state = 'quoted' AND confirmed_at IS NULL AND quoted_at <= now() - ` + abandonedQuoteAge

	pruneAbandonedQuotes = `DELETE FROM loyal_yield.bridge_transfers
WHERE ` + abandonedQuote + ` AND request_id IN (
  SELECT request_id FROM loyal_yield.bridge_transfers WHERE ` + abandonedQuote + ` ORDER BY quoted_at LIMIT $1)`

	trackedDepositAddresses = `SELECT address, solana_wallet, chain, asset FROM loyal_yield.relay_deposit_addresses
WHERE chain = $1 AND expires_at > now() - ` + trackWindow + `
ORDER BY address`

	insertDetected = `INSERT INTO loyal_yield.bridge_transfers
(request_id, solana_wallet, counterparty, deposit_address, chain, asset, amount_in_raw, amount_out_raw, state, origin_tx, submitted_at)
VALUES ($1, $2, $3, $3, $4, $5, $6::text::numeric, $7::text::numeric, $8, $9, $10)
ON CONFLICT (request_id) DO NOTHING`
)

var confirmedQuotes = fmt.Sprintf(`SELECT request_id, direction FROM loyal_yield.bridge_transfers
WHERE state = 'quoted' AND confirmed_at > now() - make_interval(secs => %d)
ORDER BY confirmed_at DESC LIMIT $1`, int(LostSubmitWindow.Seconds()))

type Tracker struct {
	DB    *pgxpool.Pool
	Relay *Relay
	Facts *engine.Facts
	Privy PrivyGet
}

func (t *Tracker) Run(ctx context.Context) error {
	for _, s := range []TransferState{TransferSubmitted, TransferFilled, TransferRefunded, TransferFailed} {
		t.Facts.BridgeTransfersAdvanced(string(s), 0)
	}
	var detection sync.WaitGroup
	defer detection.Wait()
	if t.Relay.APIKey == "" {
		slog.Warn("relay requests disabled: no api key")
	} else {
		for _, s := range []TransferState{TransferSubmitted, TransferFilled, TransferRefunded, TransferFailed} {
			t.Facts.BridgeDepositsRecorded(string(Tron.Chain), string(s), 0)
		}
		t.Facts.BridgeDetectionBatchesFailed(string(Tron.Chain), 0)
		detection.Go(func() { t.detectEvery(ctx, Tron) })
	}
	if t.Privy == nil {
		slog.Warn("bridge incoming unmoved scan disabled: no privy credentials")
	} else {
		t.Facts.BridgeIncomingScanStarted()
		detection.Go(func() { t.unmovedEvery(ctx) })
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
	if err := t.dropUnsettled(ctx); err != nil {
		return err
	}
	if err := t.pollOpen(ctx); err != nil {
		return err
	}
	adopt, cancel := context.WithTimeout(ctx, adoptDeadline)
	defer cancel()
	if err := t.adoptLostSubmits(adopt); err != nil {
		return err
	}
	return t.pruneAbandoned(ctx)
}

func (t *Tracker) pollOpen(ctx context.Context) error {
	start := time.Now()
	rows, err := t.DB.Query(ctx, openTransfers)
	if err != nil {
		return err
	}
	open, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (openTransfer, error) {
		var o openTransfer
		err := row.Scan(&o.requestID, &o.state, &o.depositWallet)
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
			step := o.step(status)
			var final DepositRequest
			if err == nil && o.depositWallet != "" && step.destinationTx != "" {
				final, err = t.settledFill(ctx, o)
			}
			if err != nil {
				mu.Lock()
				relayErrs++
				if firstRelayErr == nil {
					firstRelayErr = err
				}
				mu.Unlock()
				return nil
			}
			return t.advance(ctx, o, step, final)
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

func (t *Tracker) adoptLostSubmits(ctx context.Context) error {
	rows, err := t.DB.Query(ctx, confirmedQuotes, lostSubmitBatch)
	if err != nil {
		return err
	}
	quoted, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (quotedTransfer, error) {
		var q quotedTransfer
		err := row.Scan(&q.requestID, &q.direction)
		return q, err
	})
	if err != nil {
		return err
	}
	t.Facts.BridgeTransfersPolled(string(TransferQuoted), len(quoted))

	var relayErrs atomic.Int32
	var group errgroup.Group
	group.SetLimit(trackParallel)
	for _, q := range quoted {
		group.Go(func() error {
			status, err := t.Relay.Status(ctx, q.requestID)
			if err != nil {
				relayErrs.Add(1)
				return nil
			}
			originTx := status.originTx(q.direction)
			if originTx == "" {
				return nil
			}
			tag, err := t.DB.Exec(ctx, adoptSubmit, q.requestID, originTx)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				t.Facts.BridgeTransfersAdvanced(string(TransferSubmitted), 1)
				slog.Info("bridge transfer adopted", "request_id", q.requestID, "origin_tx", originTx, "relay_status", status.State)
			}
			return nil
		})
	}
	err = group.Wait()
	if n := relayErrs.Load(); n > 0 {
		slog.Warn("bridge relay status failed for quoted transfers", "rows", n)
	}
	return err
}

func (t *Tracker) pruneAbandoned(ctx context.Context) error {
	tag, err := t.DB.Exec(ctx, pruneAbandonedQuotes, abandonedQuoteBatch)
	if err != nil {
		return err
	}
	if n := tag.RowsAffected(); n > 0 {
		slog.Info("bridge abandoned quotes pruned", "rows", n)
	}
	return nil
}

type quotedTransfer struct {
	requestID string
	direction Direction
}

type openTransfer struct {
	requestID     string
	state         TransferState
	depositWallet string
}

type transferStep struct {
	relayStatus             string
	next                    TransferState
	known                   bool
	destinationTx, refundTx string
}

func (o openTransfer) step(status Status) transferStep {
	next, known := o.state.Advance(status.State)
	s := transferStep{relayStatus: status.State, next: next, known: known}
	if n := len(status.TxHashes); known && n > 0 {
		switch {
		case status.State == "success" && next == TransferFilled:
			s.destinationTx = status.TxHashes[n-1]
		case status.State == "refund" && next == TransferRefunded:
			s.refundTx = status.TxHashes[n-1]
		}
	}
	return s
}

// settledFill is Relay's settled request for a deposit-address row's fill.
func (t *Tracker) settledFill(ctx context.Context, o openTransfer) (DepositRequest, error) {
	final, err := t.Relay.Request(ctx, Tron.RelayChainID, o.requestID)
	if err != nil {
		return DepositRequest{}, err
	}
	if !final.Settled {
		return DepositRequest{}, fmt.Errorf("relay request %s is %s without a settled route", o.requestID, final.Status)
	}
	if reason := unrecordable(Tron, trackedAddress{solanaWallet: o.depositWallet}, final); reason != "" {
		return DepositRequest{}, fmt.Errorf("relay request %s settled but %s", o.requestID, reason)
	}
	return final, nil
}

// final, when Settled, is Relay's settled request for a deposit-address fill;
// its amounts replace the ones detection froze.
func (t *Tracker) advance(ctx context.Context, o openTransfer, s transferStep, final DepositRequest) error {
	if !s.known {
		slog.Debug("bridge relay status unknown", "request_id", o.requestID, "relay_status", s.relayStatus)
		return nil
	}
	if s.next == o.state && s.destinationTx == "" && s.refundTx == "" {
		return nil
	}
	var amountIn, amountOut any
	if final.Settled {
		amountIn, amountOut = final.AmountInRaw.String(), final.AmountOutRaw.String()
	}
	tag, err := t.DB.Exec(ctx, advanceTransfer, o.requestID, string(o.state), string(s.next), s.destinationTx, s.refundTx, s.next.Settled(), amountIn, amountOut)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if s.next != o.state {
		t.Facts.BridgeTransfersAdvanced(string(s.next), 1)
	}
	slog.Info("bridge transfer advanced", "request_id", o.requestID, "from", o.state, "to", s.next, "destination_tx", s.destinationTx, "refund_tx", s.refundTx,
		"amount_in_raw", amountIn, "amount_out_raw", amountOut)
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

func (t *Tracker) detectEvery(ctx context.Context, route DepositRoute) {
	ticker := time.NewTicker(trackInterval)
	defer ticker.Stop()
	for {
		pass, cancel := context.WithTimeout(ctx, trackDeadline)
		err := t.detect(pass, route, time.Now().Add(-trackLookback))
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("bridge deposit detection failed", "chain", route.Chain, "error", engine.ErrorText(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// detect records each Relay request that moved a deposit into one of Loyal's
// open deposit addresses on route, so the poll follows it like any other
// transfer.
func (t *Tracker) detect(ctx context.Context, route DepositRoute, since time.Time) error {
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
	seen, inserted, failedBatches, failedRows := 0, 0, 0, 0
	var truncated []string
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for batch := range slices.Chunk(addresses, RequestsBatch) {
		owners := make(map[string]trackedAddress, len(batch))
		list := make([]string, 0, len(batch))
		for _, a := range batch {
			owners[a.address] = a
			list = append(list, a.address)
		}
		requests, cut, err := t.Relay.Requests(ctx, route.RelayChainID, list, since)
		if err != nil {
			failedBatches++
			fail(err)
			continue
		}
		if cut {
			truncated = append(truncated, list...)
		}
		for _, r := range requests {
			owner, ok := owners[r.DepositAddress]
			if !ok {
				continue
			}
			seen++
			if reason := unrecordable(route, owner, r); reason != "" {
				slog.Debug("relay deposit request not recorded", "request_id", r.ID, "deposit_address", r.DepositAddress, "reason", reason)
				continue
			}
			state, _ := TransferSubmitted.Advance(r.Status)
			tag, err := t.DB.Exec(ctx, insertDetected, r.ID, owner.solanaWallet, owner.address, owner.chain, owner.asset,
				r.AmountInRaw.String(), r.AmountOutRaw.String(), string(state), r.OriginTx, r.CreatedAt)
			if err != nil {
				failedRows++
				fail(err)
				continue
			}
			if tag.RowsAffected() == 1 {
				inserted++
				t.Facts.BridgeDepositsRecorded(string(route.Chain), string(state), 1)
				slog.Info("bridge deposit detected", "request_id", r.ID, "deposit_address", owner.address, "state", state, "origin_tx", r.OriginTx)
			}
		}
	}
	t.Facts.BridgeDepositRequestsSeen(string(route.Chain), seen)
	t.Facts.BridgeDetectionBatchesFailed(string(route.Chain), failedBatches)
	if len(truncated) > 0 {
		slog.Warn("bridge deposit requests truncated", "chain", route.Chain, "pages", requestsMaxPages, "deposit_addresses", truncated)
	}
	if firstErr != nil {
		slog.Warn("bridge deposit detection incomplete", "chain", route.Chain, "failed_batches", failedBatches, "failed_rows", failedRows, "error", engine.ErrorText(firstErr))
	} else {
		t.Facts.BridgeDetectionPassed(string(route.Chain))
	}
	slog.Debug("bridge deposit detection", "chain", route.Chain, "addresses", len(addresses), "seen", seen, "inserted", inserted)
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
	case r.OutputChainID != SolanaRelayChainID || r.OutputCurrency != SolanaUSDCMint:
		return "output is not USDC on solana"
	case r.AmountInRaw == nil || r.AmountInRaw.Sign() <= 0 || r.AmountOutRaw == nil || r.AmountOutRaw.Sign() <= 0:
		return "amounts unknown"
	}
	return ""
}
