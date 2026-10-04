package fleetexec

import (
	"context"
	"crypto/ed25519"
	"errors"
	"math"
	"sync"
	"time"
)

// LookupWorkerConfig bounds one leased operation; planner/catalog polling is a
// separate responsibility and never receives ManagerKey.
type LookupWorkerConfig struct {
	Cluster, Owner                       string
	LeaseTTL, TickDeadline, PollInterval time.Duration
	Budget                               LookupBudget
	ReconcileOnly                        bool
	// OnHealth must not block; nil means a successful tick with actual RPC evidence.
	OnHealth func(error)
}

type LookupWorker struct {
	store      *Store
	chain      *LookupRPC
	config     LookupWorkerConfig
	managerKey func(context.Context, string) (ed25519.PrivateKey, error)
	gate       chan struct{}
	reporterMu sync.RWMutex
	reporter   func(bool, uint64)
}

func NewLookupWorker(store *Store, chain *LookupRPC, config LookupWorkerConfig, managerKey func(context.Context, string) (ed25519.PrivateKey, error)) (*LookupWorker, error) {
	if store == nil || store.pool == nil || chain == nil || config.Cluster == "" || config.Owner == "" || config.LeaseTTL < 10*time.Second || config.LeaseTTL > 5*time.Minute || config.LeaseTTL%time.Second != 0 || config.TickDeadline <= 0 || config.TickDeadline+5*time.Second > config.LeaseTTL || config.PollInterval <= 0 || config.PollInterval > time.Minute || (!config.ReconcileOnly && managerKey == nil) || config.Budget.MaximumLamports <= 0 || config.Budget.RollingWindow < time.Second || config.Budget.RollingWindow > 365*24*time.Hour || config.Budget.RollingWindow%time.Second != 0 {
		return nil, errors.New("lookup worker dependencies/configuration invalid")
	}
	return &LookupWorker{store: store, chain: chain, config: config, managerKey: managerKey, gate: make(chan struct{}, 1)}, nil
}

// SetRuntimeReporter installs a synchronous health callback; it must not block
// the leased cycle or its joined shutdown.
func (w *LookupWorker) SetRuntimeReporter(reporter func(bool, uint64)) {
	w.reporterMu.Lock()
	w.reporter = reporter
	w.reporterMu.Unlock()
}
func (w *LookupWorker) reportRuntime(ready bool, slot uint64) {
	w.reporterMu.RLock()
	reporter := w.reporter
	w.reporterMu.RUnlock()
	if reporter != nil {
		reporter(ready, slot)
	}
}

func (w *LookupWorker) Run(ctx context.Context) error {
	w.reportRuntime(false, 0)
	defer w.reportRuntime(false, 0)
	startup, cancelStartup := context.WithTimeout(ctx, w.config.TickDeadline)
	err := w.store.RequireLookupSchema(startup)
	cancelStartup()
	if err != nil {
		return err
	}
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
		w.reportRuntime(false, 0)
		return false, ctx.Err()
	}
	defer func() { <-w.gate }()
	var observedBank uint64
	defer func() { w.reportRuntime(resultErr == nil && ctx.Err() == nil && observedBank > 0, observedBank) }()
	return w.tick(ctx, &observedBank)
}

func (w *LookupWorker) tick(ctx context.Context, observedBank *uint64) (bool, error) {
	op, err := w.store.LeaseLookupOperation(ctx, w.config.Cluster, w.config.Owner, w.config.LeaseTTL, w.config.ReconcileOnly)
	if err != nil {
		return false, err
	}
	if op == nil {
		// An idle source queue is not proof that its RPC dependency works.
		var bank int64
		_, _, bank, err = w.chain.LookupBlockhash(ctx)
		if err == nil {
			*observedBank = uint64(bank)
		}
		return false, err
	}
	owned, err := w.store.LoadLookupAttempt(ctx, op.Intent.OperationID)
	if err != nil {
		return true, err
	}
	if owned != nil {
		return true, w.recoverOwned(ctx, *op, *owned, observedBank)
	}
	if op.LegacySignature != nil || op.LegacyMessageHash != nil || op.LegacyBlockhash != nil || op.LegacyLastValidBlockHeight != nil {
		return true, w.recoverLegacy(ctx, *op, observedBank)
	}
	if op.Intent.Kind == LookupVerify {
		snapshot, err := w.chain.LookupSnapshot(ctx, op.Intent.TableAddress, 0)
		if err != nil {
			return true, err
		}
		*observedBank = uint64(snapshot.Slot)
		return true, w.store.finishLookupVerification(ctx, *op, snapshot)
	}
	if w.config.ReconcileOnly {
		return true, w.store.deferLookupUnsigned(ctx, *op, "recovery-only writer")
	}
	return true, w.prepare(ctx, *op, observedBank)
}

func (w *LookupWorker) prepare(ctx context.Context, op LookupOperation, observedBank *uint64) error {
	hash, height, bank, err := w.chain.LookupBlockhash(ctx)
	if err != nil {
		return err
	}
	snapshot, err := w.chain.LookupSnapshot(ctx, op.Intent.TableAddress, bank)
	if err != nil {
		return err
	}
	if (op.Intent.Kind == LookupCreate || op.Intent.Kind == LookupRollover) && op.Intent.RecentSlot != nil && snapshot.Absent && !lookupProducedSlot(snapshot.SlotHashes, *op.Intent.RecentSlot) {
		op, err = w.refreshCreate(ctx, op, snapshot)
		if err != nil {
			return err
		}
		hash, height, bank, err = w.chain.LookupBlockhash(ctx)
		if err != nil {
			return err
		}
		if bank < int64(*op.Intent.RecentSlot) {
			return errors.New("lookup refreshed PDA bank is newer than signing bank")
		}
		snapshot, err = w.chain.LookupSnapshot(ctx, op.Intent.TableAddress, bank)
		if err != nil {
			return err
		}
	}
	if op.Intent.Kind == LookupClose && !snapshot.Absent {
		*observedBank = uint64(snapshot.Slot)
		expected := snapshot.DeactivationSlot
		op.Intent.ExpectedDeactivationSlot = &expected
		if !lookupCloseReady(snapshot, expected) {
			return w.store.deferLookupUnsigned(ctx, op, "actual SlotHashes retains deactivation bank")
		}
	}
	if !lookupUnchanged(op.Intent, snapshot) {
		*observedBank = uint64(snapshot.Slot)
		return w.store.deferLookupUnsigned(ctx, op, "chain account differs from frozen source prefix")
	}
	*observedBank = uint64(snapshot.Slot)
	wire, message, err := buildLookupUnsigned(op.Intent, hash, height)
	if err != nil {
		return err
	}
	if err = w.chain.SimulateLookup(ctx, wire); err != nil {
		return err
	}
	fee, err := w.chain.LookupFee(ctx, message)
	if err != nil {
		return err
	}
	rent, reclaimed := uint64(0), uint64(0)
	if op.Intent.Kind == LookupCreate || op.Intent.Kind == LookupRollover || op.Intent.Kind == LookupExtend {
		minimum, err := w.chain.LookupRent(ctx, 56+32*(len(op.Intent.Prefix)+len(op.Intent.Extension)))
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
	balance, err := w.chain.LookupBalance(ctx, op.Intent.Payer)
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
	return w.recoverOwned(ctx, op, prepared, observedBank)
}

func (w *LookupWorker) recoverOwned(ctx context.Context, op LookupOperation, attempt LookupAttempt, observedBank *uint64) error {
	status, err := w.chain.SignatureStatus(ctx, attempt.Wire.TransactionSignature)
	if err != nil {
		return err
	}
	var receipt *LookupReceipt
	if status.Finalized {
		receipt, err = w.chain.LookupFinalizedReceipt(ctx, attempt.Wire.TransactionSignature)
		if err != nil {
			return err
		}
	}
	minimum := attempt.SigningContextSlot
	if receipt != nil && receipt.Slot > minimum {
		minimum = receipt.Slot
	}
	snapshot, err := w.chain.LookupSnapshot(ctx, attempt.Intent.TableAddress, minimum)
	if err != nil {
		return err
	}
	*observedBank = uint64(snapshot.Slot)
	recovery, err := recoverLookup(attempt, status, receipt, snapshot)
	if err != nil {
		return err
	}
	if recovery.proof != nil {
		return w.store.commitLookupProof(ctx, op, attempt, recovery.proof)
	}
	if !status.Found && status.BlockHeight > attempt.Wire.LastValidBlockHeight {
		history, err := w.chain.lookupHistory(ctx, attempt, snapshot)
		if err != nil {
			if deferErr := w.store.deferLookupRecovery(ctx, op, attempt, "expired packet has no complete finalized landing-window proof"); deferErr != nil {
				return deferErr
			}
			return err
		}
		proof, err := expireLookup(attempt, status, snapshot, history)
		if err != nil {
			return err
		}
		return w.store.commitLookupProof(ctx, op, attempt, proof)
	}
	if w.config.ReconcileOnly || attempt.BroadcastCount != 0 || attempt.State != LookupPrepared || status.Found {
		return w.store.deferLookupRecovery(ctx, op, attempt, recovery.wait)
	}
	if !lookupUnchanged(attempt.Intent, snapshot) || (attempt.Intent.Kind == LookupClose && !lookupCloseReady(snapshot, *attempt.Intent.ExpectedDeactivationSlot)) {
		return w.store.deferLookupRecovery(ctx, op, attempt, "prepared packet has changed pre-broadcast account")
	}
	if err = w.store.RecordLookupBroadcastIntent(ctx, op, attempt); err != nil {
		if errors.Is(err, ErrLookupPaused) {
			return w.store.deferLookupRecovery(ctx, op, attempt, "owned prepared packet held by source controls")
		}
		return err
	}
	// All Send outcomes schedule observation; even a timeout owns the same
	// signature and never obtains another broadcast grant on restart.
	sendErr := w.chain.Send(ctx, attempt.Wire.SignedTransaction)
	reason := "owned packet broadcast; awaiting exact finalized receipt"
	if sendErr != nil {
		reason = "owned packet broadcast outcome unknown"
	}
	return w.store.deferLookupRecovery(ctx, op, attempt, reason)
}
