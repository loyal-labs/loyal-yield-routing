package fleetexec

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

type Config struct {
	Cluster      string
	Owner        string
	LeaseTTL     time.Duration
	BatchSize    int
	TickInterval time.Duration
	Facts        *engine.Facts
	OnHealth     func(error)
}

func (c Config) validate() error {
	if c.Cluster == "" || c.Owner == "" || c.LeaseTTL <= 0 || c.BatchSize <= 0 || c.TickInterval <= 0 || c.Facts == nil {
		return errors.New("incomplete fleet executor configuration")
	}
	return nil
}

type Worker struct {
	config   Config
	store    *Store
	chain    chain.LandChain
	receipts receiptReader
	signer   DelegateSigner
	recovery *sameMintRecovery
	balances fleet.AccountReader
	fresh    *fleet.Revalidator
	landing  atomic.Int64
	wg       sync.WaitGroup
}

func NewWorker(config Config, store *Store, land chain.LandChain, receipts receiptReader, signer DelegateSigner) (*Worker, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if store == nil || land == nil || receipts == nil {
		return nil, errors.New("missing fleet executor dependency")
	}
	worker := &Worker{config: config, store: store, chain: land, receipts: receipts, signer: signer}
	if client, ok := receipts.(*chain.Client); ok {
		worker.recovery = &sameMintRecovery{store: store, accounts: client, slotRate: client.SlotsPerSecond}
		worker.balances = client
	}
	return worker, nil
}

func (w *Worker) SetFreshRevalidator(r *fleet.Revalidator) error {
	if r == nil || len(w.signer.FeePayer) != ed25519.PrivateKeySize {
		return errors.New("fresh execution requires revalidator and delegated key")
	}
	w.fresh = r
	return nil
}

func (w *Worker) Run(ctx context.Context) error {
	defer w.wg.Wait()
	ticker := time.NewTicker(w.config.TickInterval)
	defer ticker.Stop()
	for {
		err := w.Tick(ctx)
		switch {
		case ctx.Err() != nil:
		case w.config.OnHealth != nil && err == nil:
			w.config.OnHealth(nil)
		case w.config.OnHealth != nil:
			w.config.OnHealth(fmt.Errorf("fleetexec tick: %s", engine.ErrorText(err)))
		case err != nil:
			slog.Error("fleetexec tick failed", "error", engine.ErrorText(err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) Tick(ctx context.Context) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	defer func() {
		if err == nil {
			w.config.Facts.Progress(engine.FamilyFleet)
		}
	}()
	if free := w.config.BatchSize - int(w.landing.Load()); free > 0 {
		leases, err := w.store.ClaimRecoveryWork(ctx, w.config.Cluster, w.config.Owner, w.config.LeaseTTL, free)
		if err != nil {
			return fmt.Errorf("claim recovery work: %w", err)
		}
		for _, lease := range leases {
			w.landing.Add(1)
			w.wg.Add(1)
			go func(lease SubmissionLease) {
				defer w.wg.Done()
				defer w.landing.Add(-1)
				if err := w.handleLease(ctx, lease); err != nil && !errors.Is(err, ErrStaleOwner) && !errors.Is(err, context.Canceled) {
					slog.Error("fleetexec submission handling failed", "submissionId", lease.Submission.ID, "error", engine.ErrorText(err))
				}
			}(lease)
		}
	}
	inflight, err := w.store.InflightCount(ctx, w.config.Cluster)
	if err != nil {
		return err
	}
	w.config.Facts.Inflight(engine.FamilyFleet, inflight)
	if w.fresh == nil {
		return nil
	}
	admission, _, err := w.fresh.PrepareExecution(ctx, w.config.Cluster)
	if err != nil || admission == nil {
		return err
	}
	deadline := leaseWorkDeadline(admission.Lease.ExpiresAt, w.config.LeaseTTL)
	if !deadline.After(time.Now()) {
		return ErrStaleOwner
	}
	workCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if _, err = w.ExecuteFresh(workCtx, *admission); err != nil {
		return fmt.Errorf("publish fresh signed route: %w", err)
	}
	return nil
}

func (w *Worker) handleLease(ctx context.Context, lease SubmissionLease) error {
	deadline, err := w.store.RenewClaimLease(ctx, lease, w.config.LeaseTTL)
	if err != nil {
		return err
	}
	deadline = leaseWorkDeadline(deadline, w.config.LeaseTTL)
	if !deadline.After(time.Now()) {
		return ErrStaleOwner
	}
	leaseCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	switch lease.Submission.State {
	case StateSigned, StateSubmitted, StateExpiryCheckPending:
		return w.land(leaseCtx, ctx, lease)
	case StateConfirmed:
		if lease.Submission.ConfirmedSlot == nil {
			return fmt.Errorf("confirmed submission %d has no confirmed slot", lease.Submission.ID)
		}
		return w.store.ConfirmSameMint(leaseCtx, lease, *lease.Submission.ConfirmedSlot)
	case StateReconciliationPending:
		err := w.reconcileFinalized(leaseCtx, lease)
		if errors.Is(err, chain.ErrBehind) {
			return w.store.releaseNotYet(ctx, lease, reconciliationMinRetry)
		}
		if err == nil || errors.Is(err, ErrStaleOwner) || ctx.Err() != nil {
			return err
		}
		return w.deferReconciliation(ctx, lease, err)
	}
	return fmt.Errorf("%w: %s", ErrNotClaimable, lease.Submission.State)
}

func (w *Worker) land(leaseCtx, ctx context.Context, lease SubmissionLease) error {
	record := lease.Submission
	if err := verifyDurableWire(record); err != nil {
		return err
	}
	out, err := chain.Land(leaseCtx, w.chain, chain.Attempt{
		Wire: record.SignedTransaction, Signature: record.Signature,
		LastValidBlockHeight: uint64(record.LastValidBlockHeight), Required: chain.Confirmed,
	}, chain.LandResendEvery, func(sendCtx context.Context) error {
		if err := w.store.RecordBroadcastIntent(sendCtx, lease); err != nil {
			return err
		}
		lease.Submission.State = StateSubmitted
		return nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil
		}
		return err
	}
	switch out.Kind {
	case chain.Landed:
		if err = w.store.ConfirmSameMint(leaseCtx, lease, int64(out.Slot)); err == nil {
			w.config.Facts.Landed(engine.FamilyFleet)
		}
	case chain.Failed:
		if err = w.store.AdvanceSubmission(leaseCtx, lease, Advance{
			NextState: StateFailed, ConfirmedSlot: int64Ptr(int64(out.Slot)), ErrorDetail: errPtr("chain failure: " + out.Err),
		}); err == nil {
			w.config.Facts.Failed(engine.FamilyFleet, "transaction_failed")
		}
	default:
		if err = w.store.AdvanceSubmission(leaseCtx, lease, Advance{
			NextState: StateExpired, ExpiryObservedBlockHeight: int64Ptr(int64(out.BlockHeight)),
			ErrorDetail: errPtr(fmt.Sprintf("blockhash_expired_at_height_%d", out.BlockHeight)),
		}); err == nil {
			w.config.Facts.Failed(engine.FamilyFleet, "blockhash_expired")
		}
	}
	return err
}

func (w *Worker) reconcileFinalized(ctx context.Context, lease SubmissionLease) error {
	record := lease.Submission
	if record.ConfirmedSlot == nil {
		return fmt.Errorf("reconciliation submission %d has no confirmed slot", record.ID)
	}
	receipt, err := finalizedReceipt(ctx, w.receipts, record.Signature)
	if errors.Is(err, chain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("finalized transaction: %w", err)
	}
	if err := VerifyReceiptIdentity(receipt, record, *record.ConfirmedSlot); err != nil {
		return fmt.Errorf("receipt identity: %w", err)
	}
	if w.recovery == nil {
		return errors.New("same-mint chain observer is required; reservation retained")
	}
	return w.recovery.reconcile(ctx, lease, receipt)
}

const (
	reconciliationFastRetryAttempts = 12
	reconciliationMinRetry          = time.Second
	reconciliationMaxRetry          = time.Minute
	reconciliationStallAttempts     = 60
)

func reconciliationRetryDelay(attempts int) time.Duration {
	over := attempts - reconciliationFastRetryAttempts
	if over <= 0 {
		return reconciliationMinRetry
	}
	return min(reconciliationMinRetry<<min(over, 16), reconciliationMaxRetry)
}

func (w *Worker) deferReconciliation(ctx context.Context, lease SubmissionLease, cause error) error {
	attempts := lease.Submission.AttemptCount
	detail := engine.ErrorText(cause)
	if err := w.store.DeferSubmission(ctx, lease, reconciliationRetryDelay(attempts), detail); err != nil {
		return fmt.Errorf("reconciliation defer failed after %s: %w", detail, err)
	}
	if attempts == reconciliationStallAttempts {
		w.config.Facts.Failed(engine.FamilyFleet, "fleet_reconciliation_stalled")
		slog.Error("fleet_reconciliation_stalled", "submissionId", lease.Submission.ID, "attemptCount", attempts, "retryDelay", reconciliationRetryDelay(attempts).String(), "reason", detail)
	}
	return fmt.Errorf("reconciliation deferred: %w", cause)
}

func int64Ptr(v int64) *int64 { return &v }
func errPtr(v string) *string { return &v }

func leaseWorkDeadline(expires time.Time, ttl time.Duration) time.Time {
	margin := ttl / 10
	if margin > 5*time.Second {
		margin = 5 * time.Second
	}
	return expires.Add(-margin)
}
