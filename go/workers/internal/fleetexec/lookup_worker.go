package fleetexec

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// lookupResendEvery matches the fleet landing cadence.
var lookupResendEvery = time.Second

// LookupWorkerConfig bounds one leased operation; planner/catalog polling is a
// separate responsibility and never receives ManagerKey.
type LookupWorkerConfig struct {
	Cluster, Owner                       string
	LeaseTTL, TickDeadline, PollInterval time.Duration
	Budget                               LookupBudget
	ReconcileOnly                        bool
	Facts                                *engine.Facts
	// OnHealth must not block; nil means a successful tick with actual RPC evidence.
	OnHealth func(error)
}

type LookupWorker struct {
	store      *Store
	chain      *chain.Client
	config     LookupWorkerConfig
	managerKey func(context.Context, string) (ed25519.PrivateKey, error)
	gate       chan struct{}
}

func NewLookupWorker(store *Store, chain *chain.Client, config LookupWorkerConfig, managerKey func(context.Context, string) (ed25519.PrivateKey, error)) (*LookupWorker, error) {
	if store == nil || store.pool == nil || chain == nil || config.Cluster == "" || config.Owner == "" || config.LeaseTTL < 10*time.Second || config.LeaseTTL > 5*time.Minute || config.LeaseTTL%time.Second != 0 || config.TickDeadline <= 0 || config.TickDeadline+5*time.Second > config.LeaseTTL || config.PollInterval <= 0 || config.PollInterval > time.Minute || config.Facts == nil || (!config.ReconcileOnly && managerKey == nil) || config.Budget.MaximumLamports <= 0 || config.Budget.RollingWindow < time.Second || config.Budget.RollingWindow > 365*24*time.Hour || config.Budget.RollingWindow%time.Second != 0 {
		return nil, errors.New("lookup worker dependencies/configuration invalid")
	}
	return &LookupWorker{store: store, chain: chain, config: config, managerKey: managerKey, gate: make(chan struct{}, 1)}, nil
}

func (w *LookupWorker) Run(ctx context.Context) error {
	delay := w.config.PollInterval
	for {
		_, err := w.Tick(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.config.OnHealth != nil {
			w.config.OnHealth(err)
		}
		if err != nil {
			delay = min(max(delay*2, 5*time.Second), time.Minute)
		} else {
			delay = w.config.PollInterval
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Tick performs at most one source operation and one broadcast. There is no
// goroutine or automatic retry around Send, and the lease outlives its deadline.
func (w *LookupWorker) Tick(parent context.Context) (worked bool, resultErr error) {
	ctx, cancel := context.WithTimeout(parent, w.config.TickDeadline)
	defer cancel()
	select {
	case w.gate <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-w.gate }()
	worked, resultErr = w.tick(ctx)
	if resultErr == nil {
		var inflight int
		if resultErr = w.store.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_operations o JOIN loyal_yield.lookup_table_families f ON f.id=o.family_id WHERE f.cluster=$1 AND o.transaction_signature IS NOT NULL AND o.operation_state NOT IN ('complete','permanent_failure','cancelled')`, w.config.Cluster).Scan(&inflight); resultErr == nil {
			w.config.Facts.Inflight(engine.FamilyLookup, inflight)
			w.config.Facts.Progress(engine.FamilyLookup)
		}
	}
	return worked, resultErr
}

func (w *LookupWorker) tick(ctx context.Context) (bool, error) {
	op, err := w.store.LeaseLookupOperation(ctx, w.config.Cluster, w.config.Owner, w.config.LeaseTTL, w.config.ReconcileOnly)
	if err != nil {
		return false, err
	}
	if op == nil {
		// An idle source queue is not proof that its RPC dependency works.
		_, _, _, err = lookupBlockhash(ctx, w.chain)
		return false, err
	}
	if op.Signature != nil || op.MessageHash != nil || op.Blockhash != nil || op.LastValidBlockHeight != nil {
		attempt, err := lookupAttemptOf(*op)
		if err != nil {
			return true, err
		}
		return true, w.land(ctx, *op, attempt)
	}
	if op.Intent.Kind == LookupVerify {
		snapshot, err := lookupSnapshot(ctx, w.chain, op.Intent.TableAddress, 0)
		if err != nil {
			return true, err
		}
		return true, w.store.finishLookupVerification(ctx, *op, snapshot)
	}
	if w.config.ReconcileOnly {
		return true, w.store.deferLookupUnsigned(ctx, *op, "recovery-only writer")
	}
	return true, w.prepare(ctx, *op)
}

func (w *LookupWorker) prepare(ctx context.Context, op LookupOperation) error {
	hash, height, bank, err := lookupBlockhash(ctx, w.chain)
	if err != nil {
		return err
	}
	snapshot, err := lookupSnapshot(ctx, w.chain, op.Intent.TableAddress, bank)
	if err != nil {
		return err
	}
	if (op.Intent.Kind == LookupCreate || op.Intent.Kind == LookupRollover) && op.Intent.RecentSlot != nil && snapshot.Absent && !lookupProducedSlot(snapshot.SlotHashes, *op.Intent.RecentSlot) {
		op, err = w.refreshCreate(ctx, op, snapshot)
		if err != nil {
			return err
		}
		hash, height, bank, err = lookupBlockhash(ctx, w.chain)
		if err != nil {
			return err
		}
		if bank < int64(*op.Intent.RecentSlot) {
			return errors.New("lookup refreshed PDA bank is newer than signing bank")
		}
		snapshot, err = lookupSnapshot(ctx, w.chain, op.Intent.TableAddress, bank)
		if err != nil {
			return err
		}
	}
	if op.Intent.Kind == LookupClose && !snapshot.Absent {
		expected := snapshot.DeactivationSlot
		op.Intent.ExpectedDeactivationSlot = &expected
		if !lookupCloseReady(snapshot, expected) {
			return w.store.deferLookupUnsigned(ctx, op, "actual SlotHashes retains deactivation bank")
		}
	}
	if !lookupUnchanged(op.Intent, snapshot) {
		return w.store.deferLookupUnsigned(ctx, op, "chain account differs from frozen source prefix")
	}
	wire, message, err := buildLookupUnsigned(op.Intent, hash, height)
	if err != nil {
		return err
	}
	// Unsigned: the bank checks the program effects, not the signature.
	if _, err = w.chain.Simulate(ctx, wire, rpc.SimulateTransactionOpts{Commitment: rpc.CommitmentFinalized, ReplaceRecentBlockhash: true}); err != nil {
		return err
	}
	fee, err := w.chain.Fee(ctx, message, rpc.CommitmentFinalized)
	if err != nil {
		return err
	}
	rent, reclaimed := uint64(0), uint64(0)
	if op.Intent.Kind == LookupCreate || op.Intent.Kind == LookupRollover || op.Intent.Kind == LookupExtend {
		minimum, err := w.chain.RentExempt(ctx, uint64(56+32*(len(op.Intent.Prefix)+len(op.Intent.Extension))))
		if err != nil {
			return err
		}
		if minimum > snapshot.Lamports {
			rent = minimum - snapshot.Lamports
		}
	}
	if op.Intent.Kind == LookupClose {
		reclaimed = snapshot.Lamports
	}
	if fee > math.MaxInt64 || rent > math.MaxInt64 || fee > math.MaxInt64-rent || reclaimed > math.MaxInt64 {
		return errors.New("lookup simulation accounting exceeds durable range")
	}
	balance, err := lookupBalance(ctx, w.chain, op.Intent.Payer)
	if err != nil {
		return err
	}
	if balance < fee+rent {
		return w.store.deferLookupUnsigned(ctx, op, "manager SOL insufficient for exact fee and rent")
	}
	approved, err := w.store.ReserveLookupBudget(ctx, op, w.config.Budget, fee, rent)
	if err != nil {
		return err
	}
	if !approved {
		return w.store.deferLookupUnsigned(ctx, op, "source rolling SOL budget exhausted")
	}
	// The callback is first invoked only after simulation, custody fences and
	// the exact durable reservation. It receives no mutable recipe to sign.
	key, err := w.managerKey(ctx, op.Intent.Payer)
	if err != nil {
		return err
	}
	signed, err := signLookupMutation(op.Intent, hash, height, key)
	if err != nil {
		return err
	}
	prepared, err := w.store.PersistLookupPrepared(ctx, op, LookupAttempt{Intent: op.Intent, Wire: signed, SigningContextSlot: bank, EstimatedFeeLamports: fee, EstimatedRentLamports: rent, EstimatedReclaimedLamports: reclaimed})
	if err != nil {
		return err
	}
	return w.land(ctx, op, prepared)
}

// land resends the operation's signed bytes until they finalize or expire,
// then applies the finalized effect. A packet without bytes (signed by the
// Rust provisioner) and reconcile-only mode resolve by signature status alone,
// with the same height-first classifier.
func (w *LookupWorker) land(ctx context.Context, op LookupOperation, attempt LookupAttempt) error {
	target := chain.Attempt{
		Wire: attempt.Wire.SignedTransaction, Signature: attempt.Wire.TransactionSignature,
		LastValidBlockHeight: uint64(attempt.Wire.LastValidBlockHeight), Sends: attempt.BroadcastCount,
		Required: chain.Finalized,
	}
	var out chain.Outcome
	var err error
	if len(target.Wire) > 0 && !w.config.ReconcileOnly {
		// Leave the tick time to record the outcome inside its own deadline.
		landCtx, cancel := context.WithTimeout(ctx, w.config.TickDeadline/2)
		out, err = chain.Land(landCtx, w.chain, target, lookupResendEvery, func(sendCtx context.Context) error {
			return w.store.RecordLookupSend(sendCtx, op, attempt)
		})
		cancel()
		switch {
		case errors.Is(err, ErrLookupPaused):
			return w.store.deferLookupRecovery(ctx, op, "signed packet held by source controls", true)
		case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
			return w.store.deferLookupRecovery(ctx, op, "landing continues", false)
		}
	} else {
		out, err = chain.Observe(ctx, w.chain, target)
	}
	if err != nil {
		return err
	}
	switch out.Kind {
	case 0:
		return w.store.deferLookupRecovery(ctx, op, "signature not finalized", false)
	case chain.Expired:
		// Rust re-signs only when the mutation is also absent on chain; a
		// landed packet the history has not indexed yet must not be archived.
		snapshot, err := lookupSnapshot(ctx, w.chain, attempt.Intent.TableAddress, int64(out.ContextSlot))
		if err != nil {
			return err
		}
		if !lookupUnchanged(attempt.Intent, snapshot) {
			return w.store.markLookupDrift(ctx, op, attempt, "signature absent after blockhash expiry but the table changed on chain")
		}
		return w.expire(ctx, op, attempt)
	case chain.Failed:
		if out.Commitment != chain.Finalized {
			return w.store.deferLookupRecovery(ctx, op, "failed packet not finalized", false)
		}
	}
	receipt, err := lookupFinalizedReceipt(ctx, w.chain, attempt.Wire.TransactionSignature)
	if err != nil {
		return err
	}
	if receipt == nil {
		return w.store.deferLookupRecovery(ctx, op, "finalized packet history unavailable", false)
	}
	if len(attempt.Wire.SignedTransaction) == 0 {
		hash := sha256.Sum256(receipt.Wire)
		attempt.Wire.SignedTransaction, attempt.Wire.SignedTransactionHash = receipt.Wire, hex.EncodeToString(hash[:])
	}
	snapshot, err := lookupSnapshot(ctx, w.chain, attempt.Intent.TableAddress, max(attempt.SigningContextSlot, receipt.Slot))
	if err != nil {
		return err
	}
	status := chain.SignatureState{Found: true, Slot: out.Slot, Commitment: chain.Finalized, Err: out.Err}
	recovery, err := recoverLookup(attempt, status, receipt, snapshot)
	if err != nil {
		return w.store.markLookupDrift(ctx, op, attempt, err.Error())
	}
	if recovery.proof == nil {
		return w.store.deferLookupRecovery(ctx, op, recovery.wait, false)
	}
	if err := w.store.commitLookupProof(ctx, op, attempt, recovery.proof); err != nil {
		return err
	}
	if recovery.proof.state == LookupFailed {
		w.config.Facts.Failed(engine.FamilyLookup, "transaction_failed")
	} else {
		w.config.Facts.Landed(engine.FamilyLookup)
	}
	return nil
}

func (w *LookupWorker) expire(ctx context.Context, op LookupOperation, attempt LookupAttempt) error {
	if err := w.store.expireLookupOperation(ctx, op, attempt); err != nil {
		return err
	}
	w.config.Facts.Failed(engine.FamilyLookup, "blockhash_expired")
	return nil
}
