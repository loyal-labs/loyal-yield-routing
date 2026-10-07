package fleetexec

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/solana"
)

// resendEvery matches the Rust confirmer's one-second durable poll.
var resendEvery = time.Second

// Config is the explicit runtime configuration for one fleet executor.
type Config struct {
	// SlotDuration is the same observed/configured economic clock used by planning.
	SlotDuration time.Duration
	Cluster      string
	// Owner is written to the Rust confirmation-lease columns so a restarted
	// Rust confirmer reads the rows correctly.
	Owner    string
	LeaseTTL time.Duration
	// BatchSize bounds submissions claimed per tick.
	BatchSize int
	// TickInterval spaces recovery sweeps.
	TickInterval time.Duration
	Facts        *engine.Facts
}

func (c Config) validate() error {
	if c.Cluster == "" || c.Owner == "" || c.LeaseTTL <= 0 || c.BatchSize <= 0 || c.TickInterval <= 0 || c.Facts == nil {
		return errors.New("incomplete fleet executor configuration")
	}
	return nil
}

// Worker lands and reconciles durable signed route submissions. It holds the
// delegated signing key only for fresh wire; landing never re-signs or
// rebuilds bytes.
type Worker struct {
	config          Config
	store           *Store
	chain           solana.LandChain
	status          StatusClient
	signer          DelegateSigner
	recovery        *sameMintRecovery
	fresh           *fleet.Revalidator
	runtimeReporter func(bool, uint64)
	// landing counts claimed rows still being landed or reconciled; at most
	// BatchSize run at once. Run joins them before returning.
	landing atomic.Int64
	wg      sync.WaitGroup
	newest  atomic.Uint64
}

// NewWorker composes the executor from its concrete dependencies.
func NewWorker(config Config, store *Store, chain solana.LandChain, status StatusClient, signer DelegateSigner) (*Worker, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if store == nil || chain == nil || status == nil {
		return nil, errors.New("missing fleet executor dependency")
	}
	worker := &Worker{config: config, store: store, chain: chain, status: status, signer: signer}
	if adapter, ok := status.(*RPCAdapter); ok {
		if config.SlotDuration <= 0 || config.SlotDuration > 10*time.Second {
			return nil, errors.New("real fleet reconciliation requires explicit slot duration")
		}
		worker.recovery = &sameMintRecovery{store: store, accounts: fleet.NewRPCClient(adapter.url), slotDuration: config.SlotDuration}
	}
	return worker, nil
}

// SetFreshRevalidator wires the concrete read-only preparation controller.
// Configure it before Run; callers retain the database and RPC lifecycles.
func (w *Worker) SetFreshRevalidator(r *fleet.Revalidator) error {
	if r == nil || len(w.signer.FeePayer) != ed25519.PrivateKeySize {
		return errors.New("fresh execution requires revalidator and delegated key")
	}
	w.fresh = r
	return nil
}

// SetRuntimeReporter is configured before Run; nil preserves standalone behavior.
func (w *Worker) SetRuntimeReporter(report func(bool, uint64)) { w.runtimeReporter = report }

// Run ticks until the context is canceled and joins every landing before
// returning.
func (w *Worker) Run(ctx context.Context) error {
	defer w.wg.Wait()
	ticker := time.NewTicker(w.config.TickInterval)
	defer ticker.Stop()
	for {
		err := w.Tick(ctx)
		if w.runtimeReporter != nil {
			w.runtimeReporter(err == nil, max(w.newest.Load(), 1))
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Print("fleetexec tick failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Tick claims rows up to the free landing capacity and lands or reconciles
// each in its own goroutine, then publishes one fresh signed route. The next
// tick's claim lands that route. A fresh route never waits behind a landing.
func (w *Worker) Tick(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
				slot, err := w.handleLease(ctx, lease)
				if slot > w.newest.Load() {
					w.newest.Store(slot)
				}
				if err != nil && !errors.Is(err, ErrStaleOwner) && !errors.Is(err, context.Canceled) {
					// Errors can carry RPC detail; the row itself holds the state.
					log.Print("fleetexec submission handling failed")
				}
			}(lease)
		}
	}
	inflight, err := w.store.InflightCount(ctx, w.config.Cluster)
	if err != nil {
		return err
	}
	w.config.Facts.Inflight(engine.FamilyFleet, inflight)
	w.config.Facts.Progress(engine.FamilyFleet)
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

func (w *Worker) handleLease(ctx context.Context, lease SubmissionLease) (uint64, error) {
	// Use the deadline returned by the database rather than granting a new
	// TTL after network delay.
	deadline, err := w.store.RenewClaimLease(ctx, lease, w.config.LeaseTTL)
	if err != nil {
		return 0, err
	}
	deadline = leaseWorkDeadline(deadline, w.config.LeaseTTL)
	if !deadline.After(time.Now()) {
		return 0, ErrStaleOwner
	}
	leaseCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	switch lease.Submission.State {
	case StateSigned, StateSubmitted, StateExpiryCheckPending:
		return w.land(leaseCtx, ctx, lease)
	case StateConfirmed:
		if lease.Submission.ConfirmedSlot == nil {
			return 0, fmt.Errorf("confirmed submission %d has no confirmed slot", lease.Submission.ID)
		}
		return uint64(*lease.Submission.ConfirmedSlot), w.store.ConfirmSameMint(leaseCtx, lease, *lease.Submission.ConfirmedSlot)
	case StateReconciliationPending:
		return 0, w.reconcileFinalized(leaseCtx, lease)
	}
	return 0, fmt.Errorf("%w: %s", ErrNotClaimable, lease.Submission.State)
}

// land resends the row's exact bytes until they land or expire. When the
// lease window ends first, the row stays signed or submitted and the next
// tick's claim lands it again: that is the same path a restart takes.
func (w *Worker) land(leaseCtx, ctx context.Context, lease SubmissionLease) (uint64, error) {
	record := lease.Submission
	if err := verifyDurableWire(record); err != nil {
		return 0, err
	}
	out, err := solana.Land(leaseCtx, w.chain, solana.Attempt{
		Wire: record.SignedTransaction, Signature: record.Signature,
		LastValidBlockHeight: uint64(record.LastValidBlockHeight), Sends: record.BroadcastCount,
		Required: solana.Confirmed,
	}, resendEvery, func(sendCtx context.Context) error {
		if err := w.store.RecordBroadcastIntent(sendCtx, lease); err != nil {
			return err
		}
		lease.Submission.State = StateSubmitted
		return nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return 0, nil
		}
		return 0, err
	}
	switch out.Kind {
	case solana.Landed:
		w.config.Facts.Landed(engine.FamilyFleet)
		return out.Slot, w.store.ConfirmSameMint(leaseCtx, lease, int64(out.Slot))
	case solana.Failed:
		w.config.Facts.Failed(engine.FamilyFleet, "transaction_failed")
		return out.Slot, w.store.AdvanceSubmission(leaseCtx, lease, Advance{
			NextState: StateFailed, ConfirmedSlot: int64Ptr(int64(out.Slot)), ErrorDetail: errPtr("chain failure: " + out.Err),
		})
	default:
		w.config.Facts.Failed(engine.FamilyFleet, "blockhash_expired")
		return out.ContextSlot, w.store.AdvanceSubmission(leaseCtx, lease, Advance{
			NextState: StateExpired, ExpiryObservedBlockHeight: int64Ptr(int64(out.BlockHeight)),
			EffectCheckSlot: int64Ptr(int64(out.ContextSlot)), ErrorDetail: errPtr("blockhash_expired_not_landed"),
		})
	}
}

// reconcileFinalized verifies the exact finalized receipt identity and its
// anchored collateral/liquidity effects before declaring the movement
// reconciled. Capacity stays reserved until this proof and newer telemetry.
func (w *Worker) reconcileFinalized(ctx context.Context, lease SubmissionLease) error {
	record := lease.Submission
	if record.ConfirmedSlot == nil {
		return fmt.Errorf("reconciliation submission %d has no confirmed slot", record.ID)
	}
	receipt, err := w.status.FinalizedTransaction(ctx, record.Signature)
	if err != nil {
		return fmt.Errorf("finalized transaction: %w", err)
	}
	if receipt == nil {
		return nil // Finality not reached; the durable backoff continues.
	}
	if err := VerifyReceiptIdentity(receipt, record, *record.ConfirmedSlot); err != nil {
		return w.store.AdvanceSubmission(ctx, lease, Advance{NextState: StateReconciliationPending, ErrorDetail: errPtr("receipt identity: " + err.Error())})
	}
	if w.recovery == nil {
		return errors.New("same-mint chain observer is required; reservation retained")
	}
	return w.recovery.reconcile(ctx, lease, receipt)
}

func int64Ptr(v int64) *int64 { return &v }
func errPtr(v string) *string { return &v }

// reconciledEffect durably records what the verified receipt proved.
func reconciledEffect(receipt *TransactionReceipt) []byte {
	raw, err := json.Marshal(struct {
		Signature string       `json:"signature"`
		Slot      int64        `json:"slot"`
		Message   string       `json:"message_base64"`
		Deltas    []TokenDelta `json:"token_deltas"`
	}{receipt.Signature, receipt.Slot, receipt.MessageB64, receipt.TokenDeltas})
	if err != nil {
		return nil
	}
	return raw
}

// Leave a bounded cancellation margin before the actual database lease ends.
func leaseWorkDeadline(expires time.Time, ttl time.Duration) time.Time {
	margin := ttl / 10
	if margin > 5*time.Second {
		margin = 5 * time.Second
	}
	return expires.Add(-margin)
}
