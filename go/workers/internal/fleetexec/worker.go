package fleetexec

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/mr-tron/base58"
)

// Config is the explicit runtime configuration for one fleet executor.
type Config struct {
	// SlotDuration is the same observed/configured economic clock used by planning.
	SlotDuration time.Duration
	Cluster      string
	// Owner is this process instance's lease identity.
	Owner    string
	LeaseTTL time.Duration
	// BatchSize bounds submissions claimed per tick.
	BatchSize int
	// TickInterval spaces recovery sweeps.
	TickInterval time.Duration
}

func (c Config) validate() error {
	if c.Cluster == "" || c.Owner == "" || c.LeaseTTL <= 0 || c.BatchSize <= 0 || c.TickInterval <= 0 {
		return errors.New("incomplete fleet executor configuration")
	}
	return nil
}

// Worker recovers and advances durable signed route submissions. It holds the
// explicit delegated signing key dependency for building fresh wire; recovery
// never re-signs or rebuilds bytes.
type Worker struct {
	config          Config
	store           *Store
	broadcast       BroadcastClient
	status          StatusClient
	signer          DelegateSigner
	recovery        *sameMintRecovery
	fresh           *fleet.Revalidator
	runtimeReporter func(bool, uint64)
	runtimeAdopted  map[int64]struct{}
}

// NewWorker composes the executor from its concrete dependencies.
func NewWorker(config Config, store *Store, broadcast BroadcastClient, status StatusClient, signer DelegateSigner) (*Worker, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if store == nil || broadcast == nil || status == nil {
		return nil, errors.New("missing fleet executor dependency")
	}
	worker := &Worker{config: config, store: store, broadcast: broadcast, status: status, signer: signer}
	if adapter, ok := status.(*RPCAdapter); ok {
		if config.SlotDuration <= 0 || config.SlotDuration > 10*time.Second {
			return nil, errors.New("real fleet reconciliation requires explicit slot duration")
		}
		worker.recovery = &sameMintRecovery{store: store, accounts: fleet.NewRPCClient(adapter.url), status: status, slotDuration: config.SlotDuration}
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
func (w *Worker) reportRuntime(ready bool, slot uint64) {
	if w.runtimeReporter != nil {
		w.runtimeReporter(ready, slot)
	}
}

func (w *Worker) runtimeRecoveryHealth(ctx context.Context) (uint64, error) {
	adapter, ok := w.status.(*RPCAdapter)
	if !ok {
		return 0, errors.New("live recovery frontier unavailable")
	}
	var slot, height int64
	if err := adapter.call(ctx, &slot, "getSlot", map[string]any{"commitment": "confirmed"}); err != nil {
		return 0, err
	}
	if err := adapter.call(ctx, &height, "getBlockHeight", map[string]any{"commitment": "finalized"}); err != nil {
		return 0, err
	}
	if slot <= 0 || height <= 0 {
		return 0, errors.New("invalid recovery frontier")
	}
	var adopted []int64
	for id := range w.runtimeAdopted {
		adopted = append(adopted, id)
	}
	var blocked bool
	var retained []int64
	// Ordinary same-mint attempts remain healthy only while this executor has
	// their actual unexpired source lease and their blockhash is still live.
	// Foreign/ambiguous/expired/unadopted work retains custody and closes health.
	err := w.store.pool.QueryRow(ctx, `SELECT COALESCE(bool_or(NOT COALESCE(healthy,false)),false), COALESCE(array_agg(id) FILTER(WHERE healthy),'{}'::bigint[]) FROM (
 SELECT s.id, (s.id=ANY($4::bigint[]) AND
 s.submission_state IN ('signed','submitted','confirmed','reconciliation_pending')
 AND s.movement_leg='route' AND s.confirmation_lease_owner=$2 AND s.confirmation_lease_expires_at>clock_timestamp()
 AND s.last_valid_block_height>$3 AND EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions d
 JOIN loyal_yield.rebalance_opportunities o ON o.id=s.opportunity_id WHERE d.id=s.decision_id
 AND d.movement_route='same_mint' AND o.execution_plan->>'route_kind'='same_mint'
 AND o.execution_plan->>'source_kind' IN ('reserve_position','idle_vault_usdc'))) AS healthy FROM loyal_yield.signed_route_submissions s
 WHERE s.cluster=$1 AND s.submission_state NOT IN ('reconciled','expired','failed')) census`, w.config.Cluster, w.config.Owner, height, adopted).Scan(&blocked, &retained)
	if err != nil {
		return 0, err
	}
	w.runtimeAdopted = make(map[int64]struct{}, len(retained))
	for _, id := range retained {
		w.runtimeAdopted[id] = struct{}{}
	}
	if blocked {
		return 0, errors.New("fleet signed recovery holds remain")
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return uint64(slot), nil
}

// Run ticks until the context is canceled and joins before returning.
func (w *Worker) Run(ctx context.Context) error {
	w.reportRuntime(false, 0)
	defer w.reportRuntime(false, 0)
	ticker := time.NewTicker(w.config.TickInterval)
	defer ticker.Stop()
	for {
		_, err := w.Tick(ctx)
		var slot uint64
		if err == nil && w.runtimeReporter != nil {
			slot, err = w.runtimeRecoveryHealth(ctx)
		}
		w.reportRuntime(err == nil && ctx.Err() == nil && slot > 0, slot)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Print("fleetexec cycle_or_recovery_health_failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Tick executes one recovery-first sweep and returns the number of
// submissions it advanced.
func (w *Worker) Tick(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	leases, err := w.store.ClaimRecoveryWork(ctx, w.config.Cluster, w.config.Owner, w.config.LeaseTTL, w.config.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("claim recovery work: %w", err)
	}
	advanced := 0
	for _, lease := range leases {
		if err := w.handleLease(ctx, lease); err != nil {
			if errors.Is(err, ErrStaleOwner) {
				continue // another executor owns this row now.
			}
			return advanced, fmt.Errorf("submission %d: %w", lease.Submission.ID, err)
		}
		if w.runtimeReporter != nil {
			if w.runtimeAdopted == nil {
				w.runtimeAdopted = map[int64]struct{}{}
			}
			w.runtimeAdopted[lease.Submission.ID] = struct{}{}
		}
		advanced++
	}
	if w.fresh != nil && advanced < w.config.BatchSize {
		admission, worked, err := w.fresh.PrepareExecution(ctx, w.config.Cluster)
		if err != nil {
			return advanced, fmt.Errorf("fresh preparation: %w", err)
		}
		if admission != nil {
			deadline := leaseWorkDeadline(admission.Lease.ExpiresAt, w.config.LeaseTTL)
			if !deadline.After(time.Now()) {
				return advanced, ErrStaleOwner
			}
			workCtx, cancel := context.WithDeadline(ctx, deadline)
			_, err = w.ExecuteFresh(workCtx, *admission)
			cancel()
			if err != nil {
				return advanced, fmt.Errorf("publish fresh signed route: %w", err)
			}
		}
		if worked {
			advanced++
		}
	}
	return advanced, nil
}

func (w *Worker) handleLease(ctx context.Context, lease SubmissionLease) error {
	// Refuse a lapsed batch claim, and use the deadline returned by the
	// database rather than granting a new TTL after network delay.
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
	plan, err := planFor(lease.Submission)
	if err != nil {
		return err
	}
	switch {
	case plan.SendExactWire:
		return w.broadcastSigned(leaseCtx, lease)
	case plan.CheckStatus:
		return w.recoverBySignature(leaseCtx, lease)
	case plan.AwaitConfirmation:
		return w.confirmSubmission(leaseCtx, lease)
	case plan.ReconcileFinalized:
		return w.reconcileFinalized(leaseCtx, lease)
	}
	return fmt.Errorf("unplanned submission state %s", lease.Submission.State)
}

// broadcastSigned persists the durable broadcast intent BEFORE any send, then
// sends the exact persisted bytes once. If this process dies between intent
// and send, the row stays signed with broadcast_count > 0 and recovery
// resolves it by signature status — it is never re-sent and never rebuilt.
// A transport outcome that leaves delivery unknown records effect ambiguity
// with observed evidence; it never re-sends.
func (w *Worker) broadcastSigned(ctx context.Context, lease SubmissionLease) error {
	record := lease.Submission
	if err := w.store.RecordBroadcastIntent(ctx, lease); err != nil {
		return fmt.Errorf("record broadcast intent: %w", err)
	}
	err := w.broadcast.Send(ctx, record.SignedTransaction)
	if isAmbiguousSend(err) {
		return w.store.AdvanceSubmission(ctx, lease, Advance{
			NextState:     StateEffectAmbiguous,
			BroadcastSent: true,
			ErrorDetail:   errPtr("broadcast outcome unknown: " + err.Error()),
		})
	}
	if err != nil {
		return w.store.AdvanceSubmission(ctx, lease, Advance{
			NextState: StateEffectAmbiguous, BroadcastSent: true,
			ErrorDetail: errPtr("broadcast outcome unproven: " + err.Error()),
		})
	}
	// The exact bytes are on the wire exactly once; the count was already
	// durably incremented by the intent.
	return w.store.AdvanceSubmission(ctx, lease, Advance{
		NextState:     StateSubmitted,
		BroadcastSent: true,
	})
}

// recoverBySignature resolves an uncertain spend only through its exact
// signature. Absent history before blockhash expiry waits. Absent history
// past expiry becomes the protocol's no-effect proof only in the legacy
// 0039 shape: an unbroadcast attempt expires outright; a broadcast attempt
// first binds an expiry observation (observed block height plus the slot of
// the performed effect check) and only expires when the same height's proof
// is complete. Heights are compared to heights, never to slots.
func (w *Worker) recoverBySignature(ctx context.Context, lease SubmissionLease) error {
	record := lease.Submission
	status, err := w.status.SignatureStatus(ctx, record.Signature)
	if err != nil {
		return fmt.Errorf("signature status: %w", err)
	}
	if status.Found && status.Confirmed && status.Err != "" {
		if record.MovementLeg != LegRoute {
			return errors.New("cross-mint failure requires leg custody reconciliation; reservation retained")
		}
		// Terminal on-chain result: no capacity waits on this route.
		return w.store.AdvanceSubmission(ctx, lease, Advance{
			NextState:     StateFailed,
			ConfirmedSlot: int64Ptr(status.Slot),
			ErrorDetail:   errPtr("chain failure: " + status.Err),
		})
	}
	if status.Found && status.Confirmed {
		if record.MovementLeg != LegRoute {
			return errors.New("cross-mint confirmation requires finalized leg custody; reservation retained")
		}
		return w.store.ConfirmSameMint(ctx, lease, status.Slot)
	}
	// A processed observation, including a processed error, can still land
	// or be rolled back. Its presence never proves expiry or no effect.
	if status.Found {
		return nil
	}
	height := status.BlockHeight
	if height <= record.LastValidBlockHeight {
		return nil
	}
	if record.BroadcastCount == 0 && record.MovementLeg == LegRoute {
		return w.store.AdvanceSubmission(ctx, lease, Advance{NextState: StateExpired,
			ExpiryObservedBlockHeight: int64Ptr(height), ErrorDetail: errPtr("unbroadcast_blockhash_expired")})
	}
	// Each protocol must inspect actual effects; RPC status context is only
	// a lower bound on the subsequent finalized account observation.
	if w.recovery == nil || record.MovementLeg != LegRoute {
		return nil
	}
	floor := status.ContextSlot
	if record.EffectCheckSlot != nil && *record.EffectCheckSlot > floor {
		floor = *record.EffectCheckSlot
	}
	proof, err := w.recovery.inspect(ctx, record, floor)
	if err != nil {
		return fmt.Errorf("no-effect evidence unavailable; custody retained: %w", err)
	}
	if record.State != StateExpiryCheckPending && record.State != StateEffectAmbiguous || record.EffectCheckSlot == nil || record.ExpiryObservedBlockHeight == nil {
		next := StateExpiryCheckPending
		if record.State == StateEffectAmbiguous {
			next = StateEffectAmbiguous
		}
		return w.store.AdvanceSubmission(ctx, lease, Advance{NextState: next,
			EffectCheckSlot: int64Ptr(proof.observedSlot), ExpiryObservedBlockHeight: int64Ptr(proof.observedHeight)})
	}
	return w.store.AdvanceSubmission(ctx, lease, Advance{NextState: StateExpired,
		ExpiryObservedBlockHeight: record.ExpiryObservedBlockHeight, noEffect: proof})

}

// confirmSubmission promotes a confirmed route into reconciliation only once
// its confirmation is durable.
func (w *Worker) confirmSubmission(ctx context.Context, lease SubmissionLease) error {
	record := lease.Submission
	if record.ConfirmedSlot == nil {
		return fmt.Errorf("confirmed submission %d has no confirmed slot", record.ID)
	}
	return w.store.ConfirmSameMint(ctx, lease, *record.ConfirmedSlot)
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
	if record.MovementLeg != LegRoute {
		return errors.New("cross-mint finalized custody reconciliation is required; reservation retained")
	}
	if w.recovery == nil {
		return errors.New("same-mint chain observer is required; reservation retained")
	}
	return w.recovery.reconcile(ctx, lease, receipt)
}

// ExecutePreparedRoute is a compatibility signing seam for an already linked
// same-mint decision. Fresh production work uses ExecuteFresh and its exact
// capacity, execute-lease, account-bank and ALT admission contract.
func (w *Worker) ExecutePreparedRoute(ctx context.Context, preparation fleet.PreparedTransaction, lastValidBlockHeight int64, admission PersistRouteInput) (int64, bool, error) {
	wire, err := w.signer.SignPreparedRoute(preparation, lastValidBlockHeight)
	if err != nil {
		return 0, false, fmt.Errorf("sign prepared route: %w", err)
	}
	expectedPayer := base58.Encode(ed25519.PublicKey(w.signer.FeePayer[32:]))
	if admission.FeePayer != expectedPayer {
		return 0, false, fmt.Errorf("admission fee payer %s is not the delegated signer %s", admission.FeePayer, expectedPayer)
	}
	admission.Wire = wire
	if w.store == nil {
		return 0, false, errors.New("missing fleet executor dependency")
	}
	return w.store.PersistSignedRoute(ctx, admission)
}

func isAmbiguousSend(err error) bool {
	if err == nil {
		return false
	}
	var ambiguous *AmbiguousSendError
	return errors.As(err, &ambiguous)
}

// AmbiguousSendError marks a broadcast whose delivery outcome is unknown
// (timeout, connection loss after flush). Definitive rejections use plain
// errors.
type AmbiguousSendError struct{ Cause error }

func (e *AmbiguousSendError) Error() string { return e.Cause.Error() }
func (e *AmbiguousSendError) Unwrap() error { return e.Cause }

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
