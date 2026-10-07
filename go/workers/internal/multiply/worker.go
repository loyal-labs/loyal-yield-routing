package multiply

// Worker lifecycle ported from crates/loyal-fleet-worker/src/multiply/mod.rs:
// synchronous Run(ctx) with cancel-and-join, one fenced lease per tick, and
// recovery branches that own every ambiguous state. No goroutine per route.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	solanaland "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/solana"
)

// landResendEvery matches the fleet landing cadence.
var landResendEvery = time.Second

const tickInterval = 750 * time.Millisecond

// Worker is the multiply route worker. The executor (and only the executor)
// holds the signer; the worker never sees key material.
type Worker struct {
	store    *Store
	observer ObservationReader
	executor *Executor
	quotes   QuoteClient
	workerID string
	// routeKey pins one route when the root composes a single-route worker.
	routeKey        *string
	runtimeReporter func(bool, uint64)
	recoveryOnly    bool
	chain           solanaland.LandChain
	facts           *engine.Facts
}

// WorkerDeps carries the pool-injected store, the observation reader, the
// executor, and the identity for leases.
type WorkerDeps struct {
	Store    *Store
	Observer ObservationReader
	Executor *Executor
	Quotes   QuoteClient
	WorkerID string
	RouteKey *string
	// Chain lands persisted wires; Facts receives the family outcomes.
	Chain solanaland.LandChain
	Facts *engine.Facts
}

// NewWorker validates the dependency set. The signer capability is checked to
// live inside the executor only.
func NewWorker(deps WorkerDeps) (*Worker, error) { return newWorker(deps, false) }

// NewRecoveryWorker has no signing capability. It can adopt landed exact wires,
// reconcile, prove expiry and send an already signed durable attempt. It cannot
// create, replace or sign a transaction.
func NewRecoveryWorker(deps WorkerDeps) (*Worker, error) { return newWorker(deps, true) }

func newWorker(deps WorkerDeps, recoveryOnly bool) (*Worker, error) {
	if recoveryOnly && deps.Executor != nil && (len(deps.Executor.Signer) != 0 || len(deps.Executor.feePayer) != 0) {
		return nil, errors.New("recovery worker must not hold private keys")
	}
	if deps.Store == nil || deps.Store.pool == nil {
		return nil, errors.New("multiply worker requires a store")
	}
	if deps.Observer == nil {
		return nil, errors.New("multiply worker requires an observation reader")
	}
	if deps.Executor == nil || deps.Executor.RPC == nil || (!recoveryOnly && len(deps.Executor.Signer) != ed25519.PrivateKeySize) {
		return nil, errors.New("multiply worker requires an executor holding the delegate capability")
	}
	if deps.Quotes == nil && !recoveryOnly {
		return nil, errors.New("multiply worker requires a quote client for swap actions")
	}
	if deps.WorkerID == "" {
		return nil, errors.New("multiply worker requires a worker identity")
	}
	if deps.Chain == nil || deps.Facts == nil {
		return nil, errors.New("multiply worker requires a landing chain and facts")
	}
	var routeKey *string
	if deps.RouteKey != nil {
		if *deps.RouteKey == "" {
			return nil, errors.New("multiply route scope is empty")
		}
		value := *deps.RouteKey
		routeKey = &value
	}
	return &Worker{
		store: deps.Store, observer: deps.Observer, executor: deps.Executor,
		quotes: deps.Quotes, workerID: deps.WorkerID, routeKey: routeKey, recoveryOnly: recoveryOnly,
		chain: deps.Chain, facts: deps.Facts,
	}, nil
}

// SetRuntimeReporter must be configured before Run. Each positive report
// includes a real live chain frontier and a complete durable recovery census.
func (w *Worker) SetRuntimeReporter(report func(bool, uint64)) { w.runtimeReporter = report }
func (w *Worker) reportRuntime(ready bool, slot uint64) {
	if w.runtimeReporter != nil {
		w.runtimeReporter(ready, slot)
	}
}

// runtimeFrontier is the live chain slot a healthy tick reports.
func (w *Worker) runtimeFrontier(ctx context.Context) (uint64, error) {
	frontier, err := w.executor.RPC.LatestBlockhash(ctx)
	if err != nil {
		return 0, err
	}
	if frontier == nil || frontier.ContextSlot == 0 {
		return 0, errors.New("missing live multiply frontier")
	}
	return frontier.ContextSlot, nil
}

// Run mirrors run(): bootstrap unpinned routes, tick, sleep, drain on
// cancellation. A route IO failure leaves its durable attempt for recovery
// and does not terminate every route sharing this worker.
func (w *Worker) Run(ctx context.Context) error {
	w.reportRuntime(false, 0)
	defer w.reportRuntime(false, 0)
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		// Bootstrap, lease acquisition and the recovery census share the same
		// bounded cycle as route IO. A busy or locked database must not leave
		// those unleased stages waiting on the process context indefinitely.
		cycle, cancelCycle := context.WithTimeout(ctx, LeaseTTL)
		bootstrapFailed := false
		if w.routeKey == nil && !w.recoveryOnly {
			if key, err := w.BootstrapReadyRoute(cycle); err != nil {
				bootstrapFailed = true
				fmt.Printf("{\"condition\":\"earn_max_route_bootstrap_failed\"}\n")
				_ = key
			} else if key != "" {
				fmt.Printf("{\"condition\":\"earn_max_route_bootstrapped\",\"routeKey\":%q}\n", key)
			}
		}
		result, err := w.Tick(cycle)
		if err != nil {
			fmt.Printf("{\"condition\":\"multiply_tick_failed\"}\n")
		} else {
			encoded, err := jsonMarshal(result)
			if err != nil {
				cancelCycle()
				return err
			}
			fmt.Printf("%s\n", encoded)
		}
		var slot uint64
		healthErr := err
		if bootstrapFailed {
			healthErr = errors.New("multiply bootstrap failed")
		}
		if healthErr == nil {
			w.facts.Progress(engine.FamilyMultiply)
			if w.runtimeReporter != nil {
				slot, healthErr = w.runtimeFrontier(cycle)
			}
		}
		ready := healthErr == nil && cycle.Err() == nil && slot > 0
		cancelCycle()
		w.reportRuntime(ready, slot)
		if healthErr != nil && err == nil {
			fmt.Printf("{\"condition\":\"multiply_recovery_health_unavailable\"}\n")
		}
		select {
		case <-ctx.Done():
			fmt.Printf("{\"condition\":\"multiply_worker_drained\"}\n")
			return nil
		case <-ticker.C:
		}
	}
}

// BootstrapReadyRoute mirrors bootstrap_ready_route: create the deterministic
// schema-9 route for the first ready earn-max-v2 policy projection.
func (w *Worker) BootstrapReadyRoute(ctx context.Context) (string, error) {
	policy, err := w.store.LoadUnbootstrappedPolicySet(ctx)
	if err != nil {
		return "", err
	}
	if policy == nil {
		return "", nil
	}
	settings, err := solana.PublicKeyFromBase58(policy.Settings)
	if err != nil {
		return "", errors.New("ready policy settings identity is invalid")
	}
	topology, err := DeriveEarnMaxTopology(settings, policy.PolicySeedBase)
	if err != nil {
		return "", err
	}
	if policy.VaultIndex != topology.VaultIndex || policy.Vault != topology.Vault.String() {
		return "", errors.New("ready policy projection drifted from deterministic topology")
	}
	routeKey := routeKeyFor(settings, topology.VaultIndex)
	claim := TokenBalance{
		Account: topology.ClaimCustody.String(), Mint: USDCMint,
		TokenProgram: TokenProgram, AmountRaw: 0,
	}
	state, err := NewRouteState(routeKey, policy.Settings, topology.VaultIndex,
		topology.Vault.String(), policy.PolicySeedBase, claim, policy.ObservedSlot, time.Now().UTC())
	if err != nil {
		return "", err
	}
	created, err := w.store.CreateRouteState(ctx, state)
	if err != nil {
		return "", err
	}
	if created {
		return routeKey, nil
	}
	return "", nil
}

// Tick mirrors tick(): acquire a lease, run the leased body, release the
// lease, and refuse to report success if the lease was lost before release.
func (w *Worker) Tick(ctx context.Context) (TickResult, error) {
	expiresAt := time.Now().UTC().Add(LeaseTTL)
	var (
		lease *Lease
		err   error
	)
	if w.routeKey != nil {
		lease, err = w.store.LeaseRoute(ctx, *w.routeKey, w.workerID, expiresAt)
	} else {
		lease, err = w.store.LeaseNextRoute(ctx, w.workerID, expiresAt)
	}
	if err != nil {
		return TickResult{}, err
	}
	if lease == nil {
		var key *string
		if w.routeKey != nil {
			value := *w.routeKey
			key = &value
		}
		return TickResult{RouteKey: key, Condition: "no_route_available"}, nil
	}
	// The persisted fence cannot revoke a signed chain transaction. Stop all
	// route IO at this lease's deadline, retaining any durable attempt if an
	// acknowledgement is lost; a subsequent lease must recover it first.
	workCtx, cancelWork := context.WithDeadline(ctx, lease.ExpiresAt)
	result, tickErr := w.tickLeased(workCtx, lease)
	cancelWork()
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	released, releaseErr := w.store.ReleaseLease(cleanupCtx, lease)
	cancelCleanup()
	if tickErr != nil {
		return TickResult{}, tickErr
	}
	if releaseErr != nil {
		return TickResult{}, releaseErr
	}
	if !released {
		return TickResult{}, errors.New("route lease was lost before release")
	}
	return result, nil
}

func (w *Worker) tickLeased(ctx context.Context, lease *Lease) (TickResult, error) {
	stored, err := w.store.LoadRouteState(ctx, lease.RouteKey)
	if err != nil {
		return TickResult{}, err
	}
	if stored == nil {
		return TickResult{}, errors.New("leased route disappeared")
	}
	if stored.Version != lease.Version || stored.FencingToken != lease.FencingToken {
		return TickResult{}, errors.New("leased route version or fencing token drifted")
	}
	topology, err := TopologyForRoute(stored.State)
	if err != nil {
		return TickResult{}, err
	}
	if stored.Operation != nil {
		return w.recover(ctx, lease, stored, topology)
	}
	if w.recoveryOnly {
		return TickResult{RouteKey: &stored.RouteKey, Condition: "recovery_only_no_operation"}, nil
	}
	ready, err := w.store.EarnMaxPolicySetReady(ctx, stored.Settings,
		stored.State.VaultIndex, stored.State.PolicySeedBase)
	if err != nil {
		return TickResult{}, err
	}
	if !ready {
		return TickResult{RouteKey: &stored.RouteKey, Condition: "earn_max_policy_set_not_ready"}, nil
	}
	var extra []TokenBalance
	if stored.State.Withdrawal != nil {
		destination, destErr := solana.PublicKeyFromBase58(stored.State.Withdrawal.DestinationAccount)
		if destErr != nil {
			return TickResult{}, errors.New("withdrawal destination identity is invalid")
		}
		// The persisted request binds a token account, not a wallet. Rust's
		// transfer_checked and the user-signed Claim use this exact address.
		extra = append(extra, TokenBalance{
			Account: destination.String(), Mint: USDCMint, TokenProgram: TokenProgram,
		})
	}
	observed, err := ObserveConfirmed(ctx, w.observer, topology, extra)
	if err != nil {
		return TickResult{}, err
	}
	if !observed.ActiveStrategyIsCoherent() {
		return TickResult{RouteKey: &stored.RouteKey, Condition: "awaiting_coherent_confirmed_observation"}, nil
	}
	if stored.State.Goal == GoalDeploy {
		for _, position := range observed.Strategies {
			if position.StrategyKey != SyrupUsdcUsdc && (position.CollateralDepositedRaw != 0 || position.DebtRaw != 0) {
				return TickResult{RouteKey: &stored.RouteKey, Condition: "unsupported_deploy_active_strategy"}, nil
			}
		}
	}
	snapshot, err := snapshotInput(stored.State, observed)
	if err != nil {
		return TickResult{}, err
	}
	if _, err := w.store.RecordPositionSnapshot(ctx, snapshot); err != nil {
		return TickResult{}, err
	}
	decision := NextAction(stored.State, observed, topology)
	switch decision.Kind {
	case "complete":
		next := *stored.State
		if next.Goal == GoalWithdraw && next.Withdrawal != nil &&
			next.Withdrawal.Status != WithdrawalClaimable {
			condition, err := withdrawalClaimCondition(&next, observed, topology)
			if err != nil {
				return TickResult{}, err
			}
			if condition != "withdrawal_claimable" {
				return TickResult{RouteKey: &stored.RouteKey, Condition: condition}, nil
			}
			withdrawal := *next.Withdrawal
			next.Withdrawal = &withdrawal
			next.Generation++
			next.Withdrawal.Status = WithdrawalClaimable
			now := time.Now().UTC()
			next.Withdrawal.UnwindCompletedAt = &now
			if saved, err := w.store.SaveRouteState(ctx, lease, &next); err != nil {
				return TickResult{}, err
			} else if !saved {
				return TickResult{}, errors.New("claimable transition CAS lost its lease")
			}
		} else if next.Goal == GoalDeploy {
			next.Generation++
			next.Goal = GoalIdle
			if saved, err := w.store.SaveRouteState(ctx, lease, &next); err != nil {
				return TickResult{}, err
			} else if !saved {
				return TickResult{}, errors.New("route completion CAS lost its lease")
			}
		}
		return TickResult{RouteKey: &stored.RouteKey, Condition: "route_complete"}, nil
	case "resume":
		return TickResult{}, errors.New("current operation was handled above")
	case "unresolved_custody":
		return TickResult{RouteKey: &stored.RouteKey, Condition: decision.Condition}, nil
	case "execute":
		return w.executePlan(ctx, lease, stored, topology, observed, decision.Plan)
	}
	return TickResult{}, fmt.Errorf("unknown planner decision %q", decision.Kind)
}

func (w *Worker) executePlan(ctx context.Context, lease *Lease, stored *StoredRoute,
	topology *EarnMaxTopology, observed *ObservedRoute, plan *ActionPlan) (TickResult, error) {
	residualCleanup := false
	if plan != nil && stored.State.Goal == GoalWithdraw && plan.Action == ActionSwapDebtToCollateral {
		position := observed.Position(plan.StrategyKey)
		residualCleanup = position != nil && position.CollateralDepositedRaw == 0 && position.DebtRaw == 0
	}
	built, err := BuildOperation(plan, observed, topology, w.quotes, ctx)
	if err != nil {
		if residualCleanup && ctx.Err() == nil {
			return TickResult{RouteKey: &stored.RouteKey, Condition: "withdrawal_residual_debt_quote_unavailable"}, nil
		}
		return TickResult{}, err
	}
	if residualCleanup {
		// Do not publish even an unsigned operation for cleanup which lacks
		// its existing literal policy. Admission never creates new authority.
		if _, err := w.executor.EnsureExactPolicy(ctx, topology, plan, built); err != nil {
			if ctx.Err() != nil {
				return TickResult{}, ctx.Err()
			}
			return TickResult{RouteKey: &stored.RouteKey, Condition: "withdrawal_residual_debt_policy_unavailable"}, nil
		}
	}
	if err := bindBefore(&built.ExpectedEffects, observed, plan.StrategyKey, topology); err != nil {
		return TickResult{}, err
	}
	now := time.Now().UTC()
	operationID := OperationID(stored.State.RouteKey, stored.State.Cycle, stored.State.Generation, string(plan.Action))
	operation := &MultiplyOperation{
		OperationID: operationID, RouteKey: stored.State.RouteKey,
		Cycle: stored.State.Cycle, EngineVersion: EngineVersion,
		Action: plan.Action, StrategyKey: plan.StrategyKey, Status: StatusPrepared,
		IDempotencyKey: fmt.Sprintf("%s:%s:%d:%d", EngineVersion,
			stored.State.RouteKey, stored.State.Cycle, stored.State.Generation),
		ExpectedEffects: built.ExpectedEffects, CreatedAt: now, UpdatedAt: now,
	}
	route := *stored.State
	route.Generation++
	value := operationID
	route.CurrentOperationID = &value
	route.ObservedSlot = observed.Slot
	route.ObservedAt = now
	prepared, err := w.store.PrepareOperation(ctx, lease, &route, operation)
	if err != nil {
		return TickResult{}, err
	}
	if !prepared {
		return TickResult{}, errors.New("prepared operation lost its route lease or idempotency key")
	}
	signed, err := w.signAndPersist(ctx, lease, operation, plan, built, observed, topology)
	if err != nil {
		return TickResult{}, err
	}
	return w.land(ctx, lease, &route, signed, topology)
}

// signAndPersist mirrors execute_operation up to its durable boundary: sign,
// simulate and persist the exact wire before anything is sent.
func (w *Worker) signAndPersist(ctx context.Context, lease *Lease, operation *MultiplyOperation, plan *ActionPlan, built *BuiltOperation,
	before *ObservedRoute, topology *EarnMaxTopology) (*MultiplyOperation, error) {
	policy, err := w.executor.EnsureExactPolicy(ctx, topology, plan, built)
	if err != nil {
		return nil, err
	}
	accountIndex := uint8(0)
	signed, minSlot, err := w.executor.PrepareAndSign(ctx, built, policy.Account, accountIndex,
		policy.ConstraintIndexes, before.Slot)
	if err != nil {
		return nil, err
	}
	messageHash, err := MessageSHA256(signed.Wire)
	if err != nil {
		return nil, err
	}
	if outcome, err := w.executor.Simulate(ctx, signed, minSlot); err != nil {
		return nil, err
	} else if outcome.Err != nil {
		return nil, fmt.Errorf("simulation failed: %s", *outcome.Err)
	}
	persisted, err := w.store.PersistSignedOperation(ctx, lease, operation.OperationID,
		policy.Account.String(), policy.DataSHA256, messageHash, signed)
	if err != nil {
		return nil, err
	}
	if !persisted {
		return nil, errors.New("lost prepared operation before signed-byte persistence")
	}
	stored, err := w.store.LoadOperation(ctx, operation.OperationID)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, errors.New("signed operation disappeared")
	}
	return stored, nil
}

// land resends the operation's persisted wire until it confirms, fails on
// chain or its blockhash expires. The first send moves signed_persisted to
// broadcast_intent exactly as mark_multiply_broadcast_intent; a resend only
// proves this worker still holds the route lease. Expiry is Rust's
// expire_multiply_operation: the route clears and the planner replans.
func (w *Worker) land(ctx context.Context, lease *Lease, route *RouteState, operation *MultiplyOperation, topology *EarnMaxTopology) (TickResult, error) {
	if _, err := PersistedTransaction(operation); err != nil {
		return TickResult{}, err
	}
	if operation.LastValidBlockHeight == nil || *operation.LastValidBlockHeight > math.MaxInt64 {
		return TickResult{}, errors.New("signed operation omitted a supported blockhash expiry")
	}
	unsent := operation.Status == StatusSignedPersisted
	sends := 1
	if unsent {
		sends = 0
	}
	intent := func(sendCtx context.Context) error {
		ok, err := w.store.MarkBroadcastIntent(sendCtx, lease, operation.OperationID, time.Now().UTC())
		if err == nil && !ok {
			err = errors.New("signed operation lost its lease before broadcast intent")
		}
		if err == nil {
			unsent = false
		}
		return err
	}
	out, err := solanaland.Land(ctx, w.chain, solanaland.Attempt{
		Wire: operation.SignedWire, Signature: *operation.TransactionSignature,
		LastValidBlockHeight: *operation.LastValidBlockHeight, Sends: sends, Required: solanaland.Confirmed,
	}, landResendEvery, func(sendCtx context.Context) error {
		if unsent {
			return intent(sendCtx)
		}
		ok, err := w.store.RenewLease(sendCtx, lease, lease.ExpiresAt)
		if err == nil && !ok {
			err = errors.New("route lease lost while landing")
		}
		return err
	})
	if errors.Is(err, context.DeadlineExceeded) {
		// The lease window ended first; the next lease lands the same wire.
		return tickResult(route, operation, "awaiting_stored_signature"), nil
	}
	if err != nil {
		return TickResult{}, err
	}
	switch out.Kind {
	case solanaland.Landed:
		w.facts.Landed(engine.FamilyMultiply)
		if unsent {
			// Landed without a send of ours: a lost acknowledgement.
			if err := intent(ctx); err != nil {
				return TickResult{}, err
			}
		}
		return w.confirmAndReconcile(ctx, lease, route, operation, out.Slot, topology)
	case solanaland.Failed:
		w.facts.Failed(engine.FamilyMultiply, "transaction_failed")
		return w.enterManualRecovery(ctx, lease, route, operation, "stored signed transaction failed at confirmed commitment: "+out.Err)
	}
	w.facts.Failed(engine.FamilyMultiply, "blockhash_expired")
	next := *route
	next.Generation++
	next.CurrentOperationID = nil
	next.ObservedAt = time.Now().UTC()
	expired, err := w.store.ExpireOperation(ctx, lease, operation.OperationID, &next)
	if err != nil {
		return TickResult{}, err
	}
	if !expired {
		return TickResult{}, errors.New("expiry lost its route fence")
	}
	return tickResult(&next, operation, "operation_expired_without_effect"), nil
}

// recover mirrors recover(): every non-terminal status has exactly one owned
// resolution; nothing is ever replaced behind an ambiguous send.
func (w *Worker) recover(ctx context.Context, lease *Lease, stored *StoredRoute,
	topology *EarnMaxTopology) (TickResult, error) {
	operation := stored.Operation
	route := stored.State
	switch operation.Status {
	case StatusPrepared:
		next := *route
		next.Generation++
		next.CurrentOperationID = nil
		cancelled, err := w.store.CancelPreparedOperation(ctx, lease, operation.OperationID, &next)
		if err != nil {
			return TickResult{}, err
		}
		if !cancelled {
			return TickResult{}, errors.New("prepared operation cancellation lost its lease")
		}
		return tickResult(route, operation, "prepared_operation_rebuilt"), nil
	case StatusSignedPersisted, StatusBroadcastIntent:
		return w.land(ctx, lease, route, operation, topology)
	case StatusConfirmed, StatusReconciliationPending:
		if _, err := PersistedTransaction(operation); err != nil {
			return TickResult{}, err
		}
		if operation.ConfirmedSlot == nil {
			return TickResult{}, errors.New("confirmed operation omitted its slot")
		}
		_, err := w.reconcileOperation(ctx, lease, route, operation, *operation.ConfirmedSlot, topology)
		if err != nil {
			return w.receiptRecoveryFailure(ctx, lease, route, operation, err)
		}
		return tickResult(route, operation, "confirmed_operation_reconciled"), nil
	}
	return TickResult{}, errors.New("route points at a terminal operation")
}

func operationExternalCustodies(operation *MultiplyOperation, topology *EarnMaxTopology) ([]TokenBalance, error) {
	known := map[string]struct{}{topology.ClaimCustody.String(): {}}
	for _, strategy := range topology.StrategyCatalog() {
		known[strategy.CollateralCustody.String()] = struct{}{}
		known[strategy.DebtCustody.String()] = struct{}{}
	}
	var extra []TokenBalance
	for _, delta := range operation.ExpectedEffects.TokenDeltas {
		if _, ok := known[delta.Account]; ok {
			continue
		}
		if _, err := solana.PublicKeyFromBase58(delta.Account); err != nil {
			return nil, err
		}
		// The only external transfer supported by this family's current
		// request contract is a classic USDC user claim.
		if delta.Mint != USDCMint {
			return nil, errors.New("external effect token program is not established by the family contract")
		}
		extra = append(extra, TokenBalance{Account: delta.Account, Mint: delta.Mint, TokenProgram: TokenProgram})
	}
	return extra, nil
}

func (w *Worker) confirmAndReconcile(ctx context.Context, lease *Lease, route *RouteState,
	operation *MultiplyOperation, slot uint64, topology *EarnMaxTopology) (TickResult, error) {
	proof, err := w.executor.readReceipt(ctx, operation, topology)
	if err != nil {
		return w.receiptRecoveryFailure(ctx, lease, route, operation, err)
	}
	if proof.evidence.ConfirmedSlot != slot {
		return w.receiptRecoveryFailure(ctx, lease, route, operation, errors.New("signature status slot disagrees with actual receipt"))
	}
	if ok, err := w.store.MarkConfirmed(ctx, lease, operation.OperationID, slot); err != nil {
		return TickResult{}, err
	} else if !ok {
		return TickResult{}, errors.New("broadcast operation lost before confirmation persistence")
	}
	confirmed, err := w.store.LoadOperation(ctx, operation.OperationID)
	if err != nil {
		return TickResult{}, err
	}
	if confirmed == nil {
		return TickResult{}, errors.New("confirmed operation disappeared")
	}
	if _, err := w.reconcileOperation(ctx, lease, route, confirmed, slot, topology); err != nil {
		return w.receiptRecoveryFailure(ctx, lease, route, confirmed, err)
	}
	return tickResult(route, confirmed, "operation_reconciled"), nil
}

func (w *Worker) receiptRecoveryFailure(ctx context.Context, lease *Lease, route *RouteState, operation *MultiplyOperation, err error) (TickResult, error) {
	if ctx.Err() != nil {
		return TickResult{}, ctx.Err()
	}
	if errors.Is(err, errReceiptUnavailable) {
		return tickResult(route, operation, "awaiting_confirmed_transaction_receipt"), nil
	}
	if errors.Is(err, errReconciliationBankUnavailable) {
		return tickResult(route, operation, "awaiting_coherent_confirmed_observation"), nil
	}
	return w.enterManualRecovery(ctx, lease, route, operation, "confirmed transaction receipt or effects failed validation")
}

var errReconciliationBankUnavailable = errors.New("confirmed reconciliation bank is stale or incoherent")

// Reject a stale provider frontier before decoding financial account state.
// The confirmed receipt is the minimum bank context, not a before-balance.
type reconciliationReader struct {
	ObservationReader
	minimumSlot uint64
}

func (r reconciliationReader) GetMultipleAccounts(ctx context.Context, keys []solana.PublicKey) (uint64, []*Account, error) {
	slot, accounts, err := r.ObservationReader.GetMultipleAccounts(ctx, keys)
	if err == nil && slot < r.minimumSlot {
		return 0, nil, errReconciliationBankUnavailable
	}
	return slot, accounts, err
}

func (w *Worker) enterManualRecovery(ctx context.Context, lease *Lease, route *RouteState,
	operation *MultiplyOperation, reason string) (TickResult, error) {
	next := *route
	next.Generation++
	next.CurrentOperationID = nil
	value := reason
	next.ManualRecoveryReason = &value
	next.Goal = GoalManualRecovery
	moved, err := w.store.MarkManualRecovery(ctx, lease, operation.OperationID, &next)
	if err != nil {
		return TickResult{}, err
	}
	if !moved {
		return TickResult{}, errors.New("manual recovery transition lost its lease")
	}
	return tickResult(route, operation, "manual_recovery_required"), nil
}

// reconcileOperation mirrors reconcile_operation: re-observe confirmed state
// (with the operation's unknown destination custodies as extra reads), pin
// the read to the confirmed slot, re-hash the policy account against the
// persisted binding, verify the persisted expected effects, and advance the
// route position once.
func (w *Worker) reconcileOperation(ctx context.Context, lease *Lease, route *RouteState,
	operation *MultiplyOperation, confirmedSlot uint64, topology *EarnMaxTopology) (*RouteState, error) {
	proof, err := w.executor.readReceipt(ctx, operation, topology)
	if err != nil {
		return nil, err
	}
	if proof.evidence.ConfirmedSlot != confirmedSlot {
		return nil, errors.New("receipt confirmed slot drifted")
	}
	extra, err := operationExternalCustodies(operation, topology)
	if err != nil {
		return nil, err
	}
	if route.Withdrawal != nil {
		extra = append(extra, TokenBalance{Account: route.Withdrawal.DestinationAccount, Mint: USDCMint, TokenProgram: TokenProgram})
	}
	after, err := ObserveConfirmed(ctx, reconciliationReader{w.observer, confirmedSlot}, topology, extra)
	if err != nil {
		return nil, err
	}
	if after.Slot < confirmedSlot || !after.ActiveStrategyIsCoherent() {
		return nil, errReconciliationBankUnavailable
	}
	proof.evidence.ObservationSlot = after.Slot
	if operation.PolicyAccount == nil {
		return nil, errors.New("operation omitted its policy account")
	}
	policyAccount, err := solana.PublicKeyFromBase58(*operation.PolicyAccount)
	if err != nil {
		return nil, err
	}
	policyData, policySlot, err := w.executor.RPC.AccountAtConfirmed(ctx, policyAccount)
	if err != nil {
		return nil, err
	}
	if policySlot < confirmedSlot || operation.PolicyDataSHA256 == nil ||
		PolicyDataHash(policyData) != *operation.PolicyDataSHA256 {
		return nil, errors.New("confirmed policy account drifted from the persisted binding")
	}
	// The verifier owns the persisted pre-effect anchors. A fresh chain read
	// here is already after execution and cannot establish a before-balance.
	if err := VerifyExpectedEffects(&operation.ExpectedEffects, operation.Action, nil, after, topology); err != nil {
		return nil, err
	}
	var active StrategyKey
	for _, position := range after.Strategies {
		if position.CollateralDepositedRaw > 0 || position.DebtRaw > 0 {
			active = position.StrategyKey
			break
		}
	}
	var position MultiplyPosition
	if active.Valid() {
		position, err = PositionBalance(after, active, topology)
		if err != nil {
			return nil, err
		}
	} else {
		position = NewIdlePosition(after.Claim)
	}
	next := *route
	next.Generation++
	next.Position = position
	if route.Withdrawal != nil {
		withdrawal := *route.Withdrawal
		next.Withdrawal = &withdrawal
	}
	next.CurrentOperationID = nil
	next.ObservedSlot = after.Slot
	next.ObservedAt = time.Now().UTC()
	if operation.Action == ActionClaim {
		if next.Withdrawal != nil {
			next.Withdrawal.Status = WithdrawalClaimed
			signature := *operation.TransactionSignature
			next.Withdrawal.ClaimSignature = &signature
		}
		if after.Claim.AmountRaw > 0 {
			next.Goal = GoalDeploy
		} else {
			next.Goal = GoalClaimed
		}
	} else if next.Goal == GoalWithdraw && !active.Valid() {
		remaining := false
		for _, entry := range after.CollateralCustodies {
			if entry.Balance.AmountRaw != 0 {
				remaining = true
				break
			}
		}
		if !remaining && next.Withdrawal != nil {
			condition, err := withdrawalClaimCondition(&next, after, topology)
			if err != nil {
				return nil, err
			}
			if condition == "withdrawal_claimable" {
				next.Withdrawal.Status = WithdrawalClaimable
				now := time.Now().UTC()
				next.Withdrawal.UnwindCompletedAt = &now
			}
		}
	}
	reconciliation := map[string]any{
		"operationId":     operation.OperationID,
		"signature":       operation.TransactionSignature,
		"confirmedSlot":   confirmedSlot,
		"observationSlot": after.Slot,
		"position":        position,
	}
	encoded, err := jsonMarshal(reconciliation)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	if operation.TransactionSignature == nil {
		return nil, errors.New("operation signature was not persisted")
	}
	reconciled, err := w.store.ReconcileOperation(ctx, lease, operation.OperationID,
		*operation.TransactionSignature, fmt.Sprintf("%x", digest[:]), confirmedSlot, &next, proof)
	if err != nil {
		return nil, err
	}
	if !reconciled {
		return nil, errors.New("lost confirmed operation during reconciliation")
	}
	return &next, nil
}

// Apps commit 45de590113312d54de7585a710620a8a80b504db persists an exact
// payout (apps/web/src/features/earn-max/server/repository.server.ts:193-225)
// and prepares a user-signed transfer_checked for it (prepared.server.ts:
// 287-308). This is newer source than the rewrite's initial Apps pin.
// Completing the unwind is not proof that NAV after fees can fund
// that payout. Shortfall remains derivable from the durable requested amount
// and confirmed position snapshot; the worker never lowers user intent.
func withdrawalClaimCondition(route *RouteState, observed *ObservedRoute, topology *EarnMaxTopology) (string, error) {
	if route == nil || route.Withdrawal == nil || observed == nil || topology == nil || observed.Slot == 0 || !observed.ActiveStrategyIsCoherent() {
		return "", errors.New("withdrawal claim evidence is absent or incoherent")
	}
	if observed.Claim.Account != topology.ClaimCustody.String() || observed.Claim.Mint != USDCMint || observed.Claim.TokenProgram != TokenProgram {
		return "", errors.New("withdrawal claim custody identity drifted")
	}
	for _, config := range topology.StrategyCatalog() {
		position := observed.Position(config.Key)
		custody := observed.CollateralCustody(config.Key)
		debt := observed.DebtCustody(config.Key)
		if position == nil || custody == nil || debt == nil {
			return "", errors.New("withdrawal position or custody is unknown")
		}
		if position.CollateralDepositedRaw != 0 || position.DebtRaw != 0 || custody.AmountRaw != 0 || (debt.Account != observed.Claim.Account && debt.AmountRaw != 0) {
			return "withdrawal_unwind_incomplete", nil
		}
	}
	destinationKnown := false
	for _, balance := range observed.ExternalCustody {
		if balance.Account == route.Withdrawal.DestinationAccount {
			if balance.Mint != USDCMint || balance.TokenProgram != TokenProgram {
				return "", errors.New("withdrawal destination mint or program drifted")
			}
			destinationKnown = true
		}
	}
	if !destinationKnown {
		return "", errors.New("withdrawal destination observation is missing")
	}
	if route.Withdrawal.AmountRaw == 0 {
		return "", errors.New("withdrawal requested amount is invalid")
	}
	if observed.Claim.AmountRaw < route.Withdrawal.AmountRaw {
		return "withdrawal_liquidity_shortfall", nil
	}
	return "withdrawal_claimable", nil
}

// OperationID mirrors operation_id: "mul-" plus the first 32 hex chars of
// sha256(engine_version || route_key || cycle_le || generation_le || action).
func OperationID(routeKey string, cycle, generation uint64, action string) string {
	hash := sha256.New()
	hash.Write([]byte(EngineVersion))
	hash.Write([]byte(routeKey))
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], cycle)
	hash.Write(raw[:])
	binary.LittleEndian.PutUint64(raw[:], generation)
	hash.Write(raw[:])
	hash.Write([]byte(action))
	return "mul-" + fmt.Sprintf("%x", hash.Sum(nil))[:32]
}

// bindBefore mirrors bind_before: every expected token delta is bound to the
// confirmed before-balance, and the obligation state is recorded.
func bindBefore(effects *ExpectedEffects, observed *ObservedRoute,
	strategyKey StrategyKey, topology *EarnMaxTopology) error {
	if effects == nil || observed == nil || observed.Slot == 0 || topology == nil {
		return errors.New("pre-transaction financial observation is absent")
	}
	effects.TokenAmountsBefore = make([]TokenAmountBefore, 0, len(effects.TokenDeltas))
	for _, delta := range effects.TokenDeltas {
		var match *TokenBalance
		candidates := []*TokenBalance{&observed.Claim}
		for index := range observed.CollateralCustodies {
			candidates = append(candidates, &observed.CollateralCustodies[index].Balance)
		}
		for index := range observed.DebtCustodies {
			candidates = append(candidates, &observed.DebtCustodies[index].Balance)
		}
		for index := range observed.ExternalCustody {
			candidates = append(candidates, &observed.ExternalCustody[index])
		}
		for _, balance := range candidates {
			if balance.Account == delta.Account && balance.Mint == delta.Mint {
				match = balance
				break
			}
		}
		if match == nil {
			return errors.New("expected token effect is not in the confirmed observation")
		}
		effects.TokenAmountsBefore = append(effects.TokenAmountsBefore, TokenAmountBefore{
			Account: match.Account, Mint: match.Mint, AmountRaw: match.AmountRaw,
		})
	}
	if strategyKey.Valid() {
		config, err := topology.Strategy(strategyKey)
		if err != nil {
			return err
		}
		position := observed.Position(strategyKey)
		if position == nil {
			return errors.New("expected obligation is not in the confirmed observation")
		}
		effects.ObligationBefore = &ObligationBefore{
			Obligation:    config.Obligation.String(),
			CollateralRaw: position.CollateralDepositedRaw,
			DebtRaw:       position.DebtRaw,
			DebtAmountSF:  position.DebtAmountSF,
		}
	}
	return nil
}

// snapshotInput mirrors snapshot_input with exact u128->USD-micros math.
func snapshotInput(route *RouteState, observed *ObservedRoute) (*PositionSnapshotInput, error) {
	if route == nil || observed == nil {
		return nil, errors.New("position snapshot observation is absent")
	}
	const fractionOneSF = uint64(1) << 60
	var active *StrategyObservation
	for _, position := range observed.Strategies {
		if position == nil {
			return nil, errors.New("position snapshot omitted a strategy observation")
		}
		if position.CollateralDepositedRaw > 0 || position.DebtRaw > 0 {
			active = position
			break
		}
	}
	toUSDMicros := func(value *big.Int) *big.Int {
		return new(big.Int).Div(
			new(big.Int).Mul(value, big.NewInt(1_000_000)),
			big.NewInt(int64(fractionOneSF)))
	}
	input := &PositionSnapshotInput{
		RouteKey: route.RouteKey, Generation: route.Generation,
		ObservedSlot: observed.Slot, ObservedAt: time.Now().UTC(),
		ClaimRaw:        formatUint(observed.Claim.AmountRaw),
		CollateralRaw:   "0",
		DebtRaw:         "0",
		ValuationSource: "confirmed_kamino_reserve_curve_500ms",
	}
	input.ValuationSlot = &observed.Slot
	now := time.Now().UTC()
	input.ValuationObservedAt = &now
	coverageStart := route.ObservedAt
	if route.Deposit != nil {
		coverageStart = route.Deposit.ObservedAt
	}
	input.CoverageStartAt = &coverageStart
	if active == nil {
		input.EquityUSD = formatUint(observed.Claim.AmountRaw)
		input.CollateralValueUSD = "0"
		input.DebtValueUSD = "0"
		return input, nil
	}
	if !validMarketValues(active) || active.UnhealthyValueSF == nil || active.UnhealthyValueSF.Sign() < 0 || active.UnhealthyValueSF.BitLen() > 128 {
		return nil, errors.New("position snapshot valuation is unknown or invalid")
	}
	collateralValue := toUSDMicros(active.CollateralValueSF)
	debtValue := toUSDMicros(active.DebtValueSF)
	// USDC raw units already are USD micros (six mint decimals).
	claimMicros := new(big.Int).SetUint64(observed.Claim.AmountRaw)
	equity := new(big.Int).Add(claimMicros, collateralValue)
	equity.Sub(equity, debtValue)
	if equity.Sign() < 0 {
		equity = big.NewInt(0)
	}
	key := string(active.StrategyKey)
	input.StrategyKey = &key
	input.CollateralRaw = formatUint(active.CollateralDepositedRaw)
	input.DebtRaw = formatUint(active.DebtRaw)
	input.CollateralValueUSD = collateralValue.String()
	input.DebtValueUSD = debtValue.String()
	input.EquityUSD = equity.String()
	tenThousand := big.NewInt(10_000)
	if equity.Sign() > 0 {
		leverage := new(big.Int).Div(new(big.Int).Mul(collateralValue, tenThousand), equity)
		if !leverage.IsInt64() {
			return nil, errors.New("snapshot leverage exceeds SQL BIGINT")
		}
		value := leverage.Int64()
		input.LeverageBPS = &value
	}
	if collateralValue.Sign() > 0 {
		ltv := new(big.Int).Div(new(big.Int).Mul(debtValue, tenThousand), collateralValue)
		if !ltv.IsInt64() {
			return nil, errors.New("snapshot LTV exceeds SQL BIGINT")
		}
		value := ltv.Int64()
		input.LTVBPS = &value
	}
	if active.DebtValueSF.Sign() > 0 {
		health := new(big.Int).Div(
			new(big.Int).Mul(active.UnhealthyValueSF, big.NewInt(1_000_000)),
			active.DebtValueSF)
		if !health.IsInt64() {
			return nil, errors.New("snapshot health exceeds SQL BIGINT")
		}
		value := health.Int64()
		input.HealthFactorPPM = &value
	}
	if active.CollateralSupplyAPYBPS > math.MaxInt64 || active.DebtBorrowAPYBPS > math.MaxInt64 {
		return nil, errors.New("snapshot APY exceeds SQL BIGINT")
	}
	supply := int64(active.CollateralSupplyAPYBPS)
	borrow := int64(active.DebtBorrowAPYBPS)
	input.SupplyAPYBPS = &supply
	input.BorrowAPYBPS = &borrow
	if equity.Sign() > 0 {
		income := new(big.Int).Mul(collateralValue, big.NewInt(supply))
		cost := new(big.Int).Mul(debtValue, big.NewInt(borrow))
		net := new(big.Int).Sub(income, cost)
		net.Quo(net, equity) // signed Rust division truncates toward zero
		if !net.IsInt64() {
			return nil, errors.New("snapshot forecast APY exceeds SQL BIGINT")
		}
		value := net.String()
		input.ForecastAPYBPS = &value
	}
	return input, nil
}

func formatUint(value uint64) string {
	return new(big.Int).SetUint64(value).String()
}

func tickResult(route *RouteState, operation *MultiplyOperation, condition string) TickResult {
	return TickResult{
		RouteKey: &route.RouteKey, Condition: condition,
		OperationID: &operation.OperationID, Signature: operation.TransactionSignature,
	}
}
