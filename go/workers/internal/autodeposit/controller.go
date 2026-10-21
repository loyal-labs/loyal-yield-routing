package autodeposit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// landResendEvery matches the fleet landing cadence; landWindow bounds one
// dispatch's landing to a blockhash lifetime.
var (
	landResendEvery = time.Second
	landWindow      = 90 * time.Second
)

type WireBuilder interface {
	BuildPull(ctx context.Context, request PullWireRequest) (BuiltWire, error)
	ConfirmTopUpRoute(ctx context.Context, plan DepositPlan) (TopUpRoute, error)
	BuildTopUp(ctx context.Context, request TopUpWireRequest) (BuiltWire, error)
	ProveTopUpWire(ctx context.Context, plan DepositPlan, attempt DurableAttempt, route TopUpRoute) error
	FeePayer() string
	DestinationSetupBuilder
}

type PullWireRequest struct {
	Plan                 DepositPlan
	RecurringDelegation  string
	RecentBlockhash      string
	LastValidBlockHeight int64
}

type TopUpWireRequest struct {
	Plan                 DepositPlan
	RecentBlockhash      string
	LastValidBlockHeight int64
}

// BuiltWire is a signed transaction this family may persist. The digest is the
// lowercase hex sha256 of the exact wire bytes.
type BuiltWire struct {
	Signature               string
	SignedTransactionBase64 string
	SignedTransactionSHA256 string
	RecentBlockhash         string
	LastValidBlockHeight    int64
}

type Controller struct {
	store            *Store
	chain            Chain
	wires            WireBuilder
	facts            *engine.Facts
	leaseRenewWindow time.Duration
	idleToleranceRaw int64
}

// DefaultLeaseRenewWindow renews well inside the ten-minute claim lease.
const DefaultLeaseRenewWindow = 3 * time.Minute

const FeePayerMinimumLamports = 50_000_000

type ControllerDependencies struct {
	Store            *Store
	Chain            Chain
	Wires            WireBuilder
	Facts            *engine.Facts
	LeaseRenewWindow time.Duration
	IdleToleranceRaw int64
}

func NewController(deps ControllerDependencies) (*Controller, error) {
	if deps.Store == nil || deps.Store.pool == nil {
		return nil, errors.New("autodeposit controller requires a store")
	}
	if deps.Chain == nil {
		return nil, errors.New("autodeposit controller requires a chain adapter")
	}
	if deps.Wires == nil {
		return nil, errors.New("autodeposit controller requires a wire builder")
	}
	if deps.Facts == nil {
		return nil, errors.New("autodeposit controller requires facts")
	}
	window := deps.LeaseRenewWindow
	if window == 0 {
		window = DefaultLeaseRenewWindow
	}
	if window < 0 || window > DefaultLeaseRenewWindow {
		return nil, errors.New("claim renewal window must be positive and at most three minutes")
	}
	if deps.IdleToleranceRaw < 0 {
		return nil, errors.New("autodeposit idle tolerance must not be negative")
	}

	return &Controller{store: deps.Store, chain: deps.Chain, wires: deps.Wires, facts: deps.Facts, leaseRenewWindow: window, idleToleranceRaw: deps.IdleToleranceRaw}, nil
}

func (c *Controller) FeePayerLamports(ctx context.Context) (string, uint64, error) {
	payer := c.wires.FeePayer()
	lamports, err := c.chain.ConfirmedLamports(ctx, payer)
	return payer, lamports, err
}

func (c *Controller) release(scope executionScope, claimToken string, result ExecutorResult, cause error) (ExecutorResult, error) {
	if _, err := c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken); err != nil {
		return ResultClaimTransitionFailed, err
	}
	return result, cause
}

func (c *Controller) Execute(ctx context.Context, target ExecutableTarget) (ExecutorResult, error) {
	if target.isRecovery() {
		return c.executeRecovery(ctx, target)
	}
	return c.executeFresh(ctx, target)
}

type executionScope struct {
	leaseToken string
	ctx        context.Context
	release    func()
}

func (c *Controller) assertOwnership(scope executionScope, claimToken string) error {
	if err := c.store.RenewClaimLease(scope.ctx, claimToken, scope.leaseToken); err != nil {
		return err
	}
	return nil
}

func (c *Controller) settle(scope executionScope, attempt DurableAttempt) (Settlement, error) {
	if attempt.Signature == "" || attempt.SignedTransactionBase64 == "" {
		return Settlement{}, errors.New("durable attempt has no persisted wire identity")
	}
	if attempt.State == AttemptConfirmed || attempt.State == AttemptFailed || attempt.State == AttemptExpired {
		return Settlement{Attempt: attempt}, nil
	}
	wire, err := base64StdDecode(attempt.SignedTransactionBase64)
	if err != nil {
		return Settlement{}, fmt.Errorf("decode persisted %s wire: %w", attempt.OperationKind, err)
	}
	landCtx, cancel := context.WithTimeout(scope.ctx, landWindow)
	out, err := chain.Land(landCtx, c.chain, chain.Attempt{
		Wire: wire, Signature: attempt.Signature, LastValidBlockHeight: uint64(attempt.LastValidBlockHeight),
		Required: chain.Confirmed,
	}, landResendEvery, func(sendCtx context.Context) error {
		if attempt.BroadcastCount == 0 {
			if err := c.chain.SimulateExact(sendCtx, attempt); err != nil {
				return err
			}
		}
		recorded, err := c.store.RecordAttemptBroadcast(sendCtx, attempt, scope.leaseToken)
		if err == nil {
			attempt = recorded
		}
		return err
	})
	cancel()
	if errors.Is(err, context.DeadlineExceeded) && scope.ctx.Err() == nil {
		return Settlement{Attempt: attempt}, nil
	}
	if err != nil {
		return Settlement{}, err
	}
	observation, code := AttemptObservation{State: AttemptExpired}, "blockhash_expired"
	switch out.Kind {
	case chain.Landed:
		slot := int64(out.Slot)
		observation, code = AttemptObservation{State: AttemptConfirmed, ConfirmedSlot: &slot}, ""
	case chain.Failed:
		observation, code = AttemptObservation{State: AttemptFailed, Err: errors.New("transaction failed on chain: " + out.Err)}, "transaction_failed"
	}
	recorded, err := c.store.RecordAttemptObservation(scope.ctx, attempt, observation, scope.leaseToken)
	if err != nil {
		return Settlement{}, err
	}
	if code == "" {
		c.facts.Landed(engine.FamilyAutodeposit)
	} else {
		c.facts.Failed(engine.FamilyAutodeposit, code)
	}
	return Settlement{Attempt: recorded}, nil
}

func (c *Controller) executeRecovery(ctx context.Context, target ExecutableTarget) (ExecutorResult, error) {
	scope, err := c.holdClaimLease(ctx, target.ClaimToken, target.TargetID)
	if err != nil {
		return ResultRecoveryPending, err
	}
	defer scope.release()

	recovery, err := c.store.LoadPullRecoveryContext(scope.ctx, target.ClaimToken, target.TargetID, target.ScheduledSlotID)
	if err != nil {
		return ResultRecoveryPending, err
	}
	if recovery == nil {
		plan, err := c.store.LoadFrozenDepositPlan(scope.ctx, target.ClaimToken, target.TargetID, scope.leaseToken)
		if err != nil {
			return ResultRecoveryPending, err
		}
		return c.executeFrozenClaim(scope, target.ClaimToken, target, plan)
	}
	if recovery.Attempt.State == AttemptConfirmed {
		if err := c.verifyPullEffects(scope.ctx, recovery.Plan, recovery.Attempt); err != nil {
			return ResultTransactionEffectAmbig, err
		}
	}

	settlement, err := c.settle(scope, recovery.Attempt)
	if err != nil {
		if errors.Is(err, ErrOwnershipLost) {
			return ResultRecoveryPending, err
		}
		return ResultDependencyUnavailable, err
	}
	if settlement.Attempt.State == AttemptSubmitted || settlement.Attempt.State == AttemptPrepared {
		return ResultRecoveryPending, nil
	}
	if !AttemptAllowsSafeRequeue(settlement.Attempt.State) {
		return c.finishTopUpLeg(scope, target.ClaimToken, target, recovery.Plan, settlement)
	}
	return c.release(scope, target.ClaimToken, ResultDeferred, nil)
}

func (c *Controller) executeFresh(ctx context.Context, target ExecutableTarget) (ExecutorResult, error) {
	targetContext, err := c.store.LoadTargetExecutionContext(ctx, target.TargetID)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	if targetContext == nil {
		return ResultNotActionable, nil
	}
	if targetContext.RecurringDelegation == "" {
		return ResultNotActionable, fmt.Errorf("autodeposit target %d has no recurring delegation", target.TargetID)
	}
	if !targetContext.hasRouteIdentity() {
		return ResultPreflightBlocked, fmt.Errorf("autodeposit target %d has no active same_mint_kamino policy", target.TargetID)
	}
	withdrawn, err := c.store.VaultWithdrawn(ctx, targetContext)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	if withdrawn {
		skipped, err := c.store.SkipWithdrawnScheduledSlot(ctx, target.ScheduledSlotID)
		if err != nil {
			return ResultDependencyUnavailable, err
		}
		if skipped {
			return ResultNotActionable, fmt.Errorf("autodeposit target %d: %s", target.TargetID, withdrawnSkipReason)
		}
	}

	walletBalance, err := c.chain.ConfirmedTokenBalanceRaw(ctx, targetContext.WalletUsdcAta, targetContext.Wallet)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	remainingAllowance, err := c.readRemainingAllowance(ctx, targetContext)
	if err != nil {
		if _, deferErr := c.store.DeferScheduledSlot(ctx, target.TargetID, target.ScheduledSlotID, err.Error(), false); deferErr != nil {
			return ResultDependencyUnavailable, errors.Join(err, deferErr)
		}
		return ResultDeferred, err
	}
	// Rust fleet same-mint routes withdraw all source collateral but deposit
	// the planned liquidity, so interest accrued since planning stays in the
	// vault ATA, and nothing drains idle custody. Residue up to the tolerance
	// rides beside the pull (the top-up deposits the pulled amount; every check
	// is on deltas). Idle above it may be custody another family owns: the TS
	// executor defers the slot before claiming, and so does this.
	// The tolerance is deleted once fleet deposits what it redeems and the
	// existing residue is drained.
	custody, err := c.chain.ConfirmedTokenBalanceRaw(ctx, targetContext.VaultUsdcAta, targetContext.VaultPubkey)
	if errors.Is(err, ErrTokenAccountAbsent) {
		custody, err = 0, nil
	}
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	if custody > c.idleToleranceRaw && walletBalance > targetContext.WalletBalanceFloorRaw && *remainingAllowance > 0 {
		deferred, err := c.store.DeferScheduledSlot(ctx, target.TargetID, target.ScheduledSlotID, fmt.Sprintf("%s%d", idleDeferralPrefix, custody), true)
		if err != nil {
			return ResultDependencyUnavailable, err
		}
		if deferred {
			return ResultDeferred, fmt.Errorf("%s%d", idleDeferralPrefix, custody)
		}
	}

	claimToken, err := newClaimToken()
	if err != nil {
		return ResultUnknown, err
	}
	claim, err := c.store.ClaimEligibleLotsOnce(ctx, target.TargetID, claimToken, target.ScheduledSlotID,
		walletBalance, targetContext.WalletBalanceFloorRaw, targetContext.MaxAmountPerPeriodRaw, remainingAllowance)
	if err != nil {
		return ResultClaimTransitionFailed, err
	}
	if claim.Status != ClaimSelected {
		return ResultNoop, nil
	}

	scope, err := c.holdClaimLease(ctx, claimToken, target.TargetID)
	if err != nil {
		return ResultRecoveryPending, err
	}
	defer scope.release()

	if err := c.store.ResolveDepositReserve(scope.ctx, targetContext); err != nil {
		return c.release(scope, claimToken, ResultPreflightBlocked, err)
	}
	plan := targetContext.depositPlan(claim.AmountRaw)
	frozen, err := c.store.FreezeDepositPlan(scope.ctx, claimToken, scope.leaseToken, plan)
	if err != nil {
		return c.release(scope, claimToken, ResultRecoveryPending, err)
	}

	return c.executeFrozenClaim(scope, claimToken, target, frozen)
}

func (c *Controller) executeFrozenClaim(scope executionScope, claimToken string, target ExecutableTarget, frozen DepositPlan) (ExecutorResult, error) {
	ready, err := c.ensureDestinationSetup(scope, claimToken, frozen)
	if err != nil {
		return c.release(scope, claimToken, ResultPreflightBlocked, err)
	}
	if !ready {
		return ResultRecoveryPending, nil
	}
	targetContext, err := c.store.LoadTargetExecutionContext(scope.ctx, target.TargetID)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	if targetContext == nil || !targetContext.FreshActionable || targetContext.RecurringDelegation == "" || targetContext.SweepPolicyAccount != frozen.Target.SweepPolicyAccount || targetContext.Wallet != frozen.Target.Wallet || targetContext.VaultPubkey != frozen.Target.VaultPubkey {
		return c.release(scope, claimToken, ResultDeferred, nil)
	}
	walletBalance, err := c.chain.ConfirmedTokenBalanceRaw(scope.ctx, frozen.Target.WalletUsdcAta, frozen.Target.Wallet)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	allowance, err := c.readRemainingAllowance(scope.ctx, targetContext)
	if err != nil {
		return c.release(scope, claimToken, ResultDeferred, err)
	}
	if walletBalance < frozen.AmountRaw || targetContext.WalletBalanceFloorRaw < 0 || walletBalance-frozen.AmountRaw < targetContext.WalletBalanceFloorRaw || *allowance < frozen.AmountRaw || (targetContext.MaxAmountPerPeriodRaw != nil && *targetContext.MaxAmountPerPeriodRaw < frozen.AmountRaw) {
		return c.release(scope, claimToken, ResultDeferred, nil)
	}
	// Preflight the destination BEFORE the pull: the top-up must be executable
	// against the frozen reserve, obligation and custody, or the wallet never
	// moves. No funds have moved when this fails.
	route, err := c.wires.ConfirmTopUpRoute(scope.ctx, frozen)
	if err != nil {
		return c.release(scope, claimToken, ResultPreflightBlocked, err)
	}
	if uint64(frozen.AmountRaw) < route.MinimumDepositRaw {
		return c.release(scope, claimToken, ResultPreflightBlocked, fmt.Errorf("%w: amount %d is below the reserve's minimum deposit %d", ErrRouteNotExecutable, frozen.AmountRaw, route.MinimumDepositRaw))
	}

	custodyBefore, err := c.chain.ConfirmedTokenBalanceRaw(scope.ctx, frozen.Target.VaultUsdcAta, frozen.Target.VaultPubkey)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	if custodyBefore > c.idleToleranceRaw {
		return c.release(scope, claimToken, ResultDeferred, fmt.Errorf("%s%d", idleDeferralPrefix, custodyBefore))
	}
	blockhash, lastValid, err := c.chain.LatestBlockhash(scope.ctx)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	wire, err := c.wires.BuildPull(scope.ctx, PullWireRequest{
		Plan: frozen, RecurringDelegation: targetContext.RecurringDelegation,
		RecentBlockhash: blockhash, LastValidBlockHeight: lastValid,
	})
	if err != nil {
		return c.release(scope, claimToken, ResultPreflightBlocked, err)
	}
	prepared, err := c.store.PersistPreparedAttempt(scope.ctx, PreparedAttempt{
		ClaimToken:               claimToken,
		TargetID:                 target.TargetID,
		ScheduledSlotID:          target.ScheduledSlotID,
		OperationKind:            OperationPull,
		AmountRaw:                frozen.AmountRaw,
		SourcePreBalanceRaw:      walletBalance,
		DestinationPreBalanceRaw: custodyBefore,
		ProtectionFloorRaw:       &targetContext.WalletBalanceFloorRaw,
		Signature:                wire.Signature,
		SignedTransactionBase64:  wire.SignedTransactionBase64,
		SignedTransactionSHA256:  wire.SignedTransactionSHA256,
		RecentBlockhash:          wire.RecentBlockhash,
		LastValidBlockHeight:     wire.LastValidBlockHeight,
	}, scope.leaseToken)
	if err != nil {
		if errors.Is(err, ErrOwnershipLost) {
			return ResultRecoveryPending, err
		}
		return ResultYieldPersistenceFailed, err
	}

	settlement, err := c.settle(scope, prepared)
	if err != nil {
		if errors.Is(err, ErrOwnershipLost) {
			return ResultRecoveryPending, err
		}
		return ResultDependencyUnavailable, err
	}
	switch {
	case AttemptAllowsSafeRequeue(settlement.Attempt.State):
		return c.release(scope, claimToken, ResultDeferred, nil)
	case !AttemptHoldsClaim(settlement.Attempt.State):
		return ResultRecoveryPending, nil
	case settlement.Attempt.State != AttemptConfirmed:
		return ResultRecoveryPending, nil
	}
	if err := c.verifyPullEffects(scope.ctx, frozen, settlement.Attempt); err != nil {
		return ResultTransactionEffectAmbig, err
	}
	return c.finishTopUpLeg(scope, claimToken, target, frozen, settlement)
}

func (c *Controller) ensureDestinationSetup(scope executionScope, claimToken string, plan DepositPlan) (bool, error) {
	builder := c.wires
	for stage := 0; stage < 4; stage++ {
		current, err := c.store.LoadTargetExecutionContext(scope.ctx, plan.Target.ID)
		if err != nil {
			return false, err
		}
		if current == nil || !current.FreshActionable {
			return true, nil
		}
		next, err := builder.InspectDestinationSetup(scope.ctx, plan)
		if err != nil || next == nil {
			return next == nil && err == nil, err
		}
		blockhash, height, err := c.chain.LatestBlockhash(scope.ctx)
		if err != nil {
			return false, err
		}
		wire, err := builder.BuildDestinationSetup(scope.ctx, plan, *next, blockhash, height)
		if err != nil {
			return false, err
		}
		setup := SetupAttempt{ClaimToken: claimToken, Plan: *next, Wire: wire}
		if err = c.chain.SimulateExact(scope.ctx, setup.durable()); err != nil {
			return false, err
		}
		bytes, err := base64StdDecode(wire.SignedTransactionBase64)
		if err != nil {
			return false, err
		}
		landCtx, cancel := context.WithTimeout(scope.ctx, landWindow)
		out, err := chain.Land(landCtx, c.chain, chain.Attempt{
			Wire: bytes, Signature: wire.Signature, LastValidBlockHeight: uint64(wire.LastValidBlockHeight), Required: chain.Confirmed,
		}, landResendEvery, func(context.Context) error { return c.assertOwnership(scope, claimToken) })
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && scope.ctx.Err() == nil {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if out.Kind != chain.Landed {
			return false, nil
		}
		if err = builder.ReadbackDestinationSetup(scope.ctx, plan, *next, int64(out.Slot)); err != nil {
			return false, err
		}
	}
	next, err := builder.InspectDestinationSetup(scope.ctx, plan)
	return next == nil, err
}

func (c *Controller) finishTopUpLeg(scope executionScope, claimToken string, target ExecutableTarget, plan DepositPlan, pull Settlement) (ExecutorResult, error) {
	confirmedPull := pull.Attempt
	if err := c.verifyPullEffects(scope.ctx, plan, confirmedPull); err != nil {
		return ResultTransactionEffectAmbig, err
	}
	custody, err := c.chain.ConfirmedTokenBalanceRaw(scope.ctx, plan.Target.VaultUsdcAta, plan.Target.VaultPubkey)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	existingTopUp, err := c.store.LoadLatestAttempt(scope.ctx, claimToken, OperationTopUp)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	var existingState *AttemptState
	var persistedPreBalance *int64
	if existingTopUp != nil {
		state := existingTopUp.State
		existingState = &state
		persistedPreBalance = &existingTopUp.SourcePreBalanceRaw
	}
	siblingDeposits, err := c.store.LoadConfirmedSiblingTopUpsSince(scope.ctx, target.TargetID, claimToken, attemptIDOrZero(existingTopUp))
	if err != nil {
		return ResultDependencyUnavailable, err
	}

	switch ClassifyDirectTopUpRecovery(existingState, custody, plan.AmountRaw, persistedPreBalance, siblingDeposits) {
	case TopUpEffectAmbiguous:
		return ResultTransactionEffectAmbig, nil
	case TopUpReconcilePersisted:
		settlement, err := c.settle(scope, *existingTopUp)
		if err != nil {
			if errors.Is(err, ErrOwnershipLost) {
				return ResultRecoveryPending, err
			}
			return ResultDependencyUnavailable, err
		}
		if settlement.Attempt.State != AttemptConfirmed {
			return ResultRecoveryPending, nil
		}
		pull = settlement
	case TopUpPrepareOrRequeue:
		if err := c.assertOwnership(scope, claimToken); err != nil {
			return ResultRecoveryPending, err
		}
		executionID, err := c.ensurePullExecution(scope, claimToken, target, plan, confirmedPull)
		if err != nil {
			if errors.Is(err, ErrOwnershipLost) {
				return ResultRecoveryPending, err
			}
			return unanswered(ResultYieldPersistenceFailed, err)
		}
		blockhash, lastValid, err := c.chain.LatestBlockhash(scope.ctx)
		if err != nil {
			return ResultDependencyUnavailable, err
		}
		wire, err := c.wires.BuildTopUp(scope.ctx, TopUpWireRequest{Plan: plan, RecentBlockhash: blockhash, LastValidBlockHeight: lastValid})
		if err != nil {
			// The pulled custody stays claimed for recovery; it must never be
			// released while only the deposit leg failed.
			return ResultKaminoTopUpFailed, err
		}
		prepared, err := c.store.PersistPreparedAttempt(scope.ctx, PreparedAttempt{
			ClaimToken:               claimToken,
			TargetID:                 target.TargetID,
			ScheduledSlotID:          target.ScheduledSlotID,
			OperationKind:            OperationTopUp,
			ExecutionID:              &executionID,
			AmountRaw:                plan.AmountRaw,
			SourcePreBalanceRaw:      custody,
			DestinationPreBalanceRaw: 0,
			Signature:                wire.Signature,
			SignedTransactionBase64:  wire.SignedTransactionBase64,
			SignedTransactionSHA256:  wire.SignedTransactionSHA256,
			RecentBlockhash:          wire.RecentBlockhash,
			LastValidBlockHeight:     wire.LastValidBlockHeight,
		}, scope.leaseToken)
		if err != nil {
			if errors.Is(err, ErrOwnershipLost) {
				return ResultRecoveryPending, err
			}
			return ResultYieldPersistenceFailed, err
		}
		settlement, err := c.settle(scope, prepared)
		if err != nil {
			if errors.Is(err, ErrOwnershipLost) {
				return ResultRecoveryPending, err
			}
			return ResultDependencyUnavailable, err
		}
		if settlement.Attempt.State != AttemptConfirmed {
			return ResultRecoveryPending, nil
		}
		pull = settlement
	}

	if pull.Attempt.ExecutionID == nil {
		return ResultTransactionEffectAmbig, &EffectAmbiguousError{Detail: "persisted top-up has no immutable execution identity"}
	}

	route, err := c.wires.ConfirmTopUpRoute(scope.ctx, plan)
	if err != nil {
		return ResultKaminoTopUpFailed, err
	}
	if err := c.wires.ProveTopUpWire(scope.ctx, plan, pull.Attempt, route); err != nil {
		return unanswered(ResultTransactionEffectAmbig, err)
	}
	if err := c.verifyTopUpEffects(scope.ctx, plan, route, pull); err != nil {
		return unanswered(ResultTransactionEffectAmbig, err)
	}

	if err := c.assertOwnership(scope, claimToken); err != nil {
		return ResultRecoveryPending, err
	}
	var confirmedSlot int64
	if pull.Attempt.ConfirmedSlot != nil {
		confirmedSlot = *pull.Attempt.ConfirmedSlot
	}
	positionAmountRaw, observedSlot, err := c.chain.ConfirmedVaultPositionRaw(scope.ctx, plan, route, confirmedSlot)
	if errors.Is(err, chain.ErrBehind) {
		return ResultRecoveryPending, err
	}
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	if _, err := c.store.FinalizeConfirmedAutodeposit(scope.ctx, claimToken, *pull.Attempt.ExecutionID, target.ScheduledSlotID,
		scope.leaseToken, positionAmountRaw, observedSlot); err != nil {
		if errors.Is(err, ErrOwnershipLost) {
			return ResultRecoveryPending, err
		}
		return ResultYieldPersistenceFailed, err
	}
	return ResultCompleted, nil
}

func unanswered(result ExecutorResult, err error) (ExecutorResult, error) {
	if errors.Is(err, chain.ErrUnavailable) {
		return ResultDependencyUnavailable, err
	}
	return result, err
}

func (c *Controller) ensurePullExecution(scope executionScope, claimToken string, target ExecutableTarget, plan DepositPlan, confirmedPull DurableAttempt) (int64, error) {
	receipt, err := c.chain.ConfirmedReceipt(scope.ctx, confirmedPull.Signature)
	if err != nil {
		return 0, err
	}
	if err := validatePullReceipt(plan, confirmedPull, receipt); err != nil {
		return 0, err
	}
	wallet, _ := receipt.EffectFor(plan.Target.WalletUsdcAta)
	custody, _ := receipt.EffectFor(plan.Target.VaultUsdcAta)
	return c.store.CreatePullExecution(scope.ctx, claimToken, target.TargetID, target.ScheduledSlotID, scope.leaseToken, plan,
		confirmedPull, wallet.PreRaw, wallet.PostRaw, custody.PreRaw, custody.PostRaw)
}

func (c *Controller) verifyTopUpEffects(ctx context.Context, plan DepositPlan, route TopUpRoute, settlement Settlement) error {
	receipt, err := c.chain.ConfirmedReceipt(ctx, settlement.Attempt.Signature)
	if err != nil {
		return err
	}
	if receipt.Signature != settlement.Attempt.Signature {
		return fmt.Errorf("top-up receipt is for %s, want the persisted %s", receipt.Signature, settlement.Attempt.Signature)
	}
	if settlement.Attempt.ConfirmedSlot != nil && receipt.Slot > 0 && receipt.Slot < *settlement.Attempt.ConfirmedSlot {
		return fmt.Errorf("top-up receipt slot %d precedes the recorded confirmation slot %d", receipt.Slot, *settlement.Attempt.ConfirmedSlot)
	}
	custody, ok := receipt.EffectFor(plan.Target.VaultUsdcAta)
	if !ok {
		return fmt.Errorf("top-up receipt shows no movement for the custody account %s", plan.Target.VaultUsdcAta)
	}
	if custody.PreRaw < 0 || custody.PostRaw < 0 || custody.Mint != plan.LiquidityMint {
		return fmt.Errorf("custody movement mint is %s, want the frozen %s", custody.Mint, plan.LiquidityMint)
	}
	supply, ok := receipt.EffectFor(route.Position.LiquiditySupply)
	if !ok {
		return fmt.Errorf("top-up receipt shows no movement for the reserve liquidity supply %s", route.Position.LiquiditySupply)
	}
	if supply.PreRaw < 0 || supply.PostRaw < 0 || supply.Mint != plan.LiquidityMint {
		return fmt.Errorf("liquidity supply movement mint is %s, want the frozen %s", supply.Mint, plan.LiquidityMint)
	}
	// KLend mints floor(amount × rate) collateral and takes only the liquidity
	// that collateral is worth, so it may take less than the frozen amount by
	// under one collateral unit's value; the rest stays in custody. The rate
	// only rises, so the confirmed route's minimum deposit bounds that unit.
	taken := supply.PostRaw - supply.PreRaw
	if delta := custody.PostRaw - custody.PreRaw; delta != -taken {
		return fmt.Errorf("top-up custody moved %d, want minus the %d the reserve received", delta, taken)
	}
	if taken <= 0 || taken > plan.AmountRaw || uint64(plan.AmountRaw-taken) >= route.MinimumDepositRaw {
		return fmt.Errorf("reserve liquidity moved %d, want the frozen %d less under one collateral unit (%d)", taken, plan.AmountRaw, route.MinimumDepositRaw)
	}
	return nil
}

func (c *Controller) verifyPullEffects(ctx context.Context, plan DepositPlan, attempt DurableAttempt) error {
	if attempt.State != AttemptConfirmed {
		return fmt.Errorf("pull %s is %s, not confirmed; its effects cannot be verified", attempt.Signature, attempt.State)
	}
	receipt, err := c.chain.ConfirmedReceipt(ctx, attempt.Signature)
	if err != nil {
		return err
	}
	return validatePullReceipt(plan, attempt, receipt)
}

func validatePullReceipt(plan DepositPlan, attempt DurableAttempt, receipt ReceiptEvidence) error {
	if attempt.State != AttemptConfirmed || attempt.OperationKind != OperationPull || plan.AmountRaw <= 0 || attempt.AmountRaw != plan.AmountRaw || receipt.Slot <= 0 {
		return errors.New("pull receipt or confirmed attempt identity is incomplete")
	}
	if receipt.Signature != attempt.Signature {
		return fmt.Errorf("pull receipt is for %s, want the persisted %s", receipt.Signature, attempt.Signature)
	}
	if attempt.ConfirmedSlot != nil && receipt.Slot > 0 && receipt.Slot < *attempt.ConfirmedSlot {
		return fmt.Errorf("pull receipt slot %d precedes the recorded confirmation slot %d", receipt.Slot, *attempt.ConfirmedSlot)
	}
	wallet, ok := receipt.EffectFor(plan.Target.WalletUsdcAta)
	if !ok {
		return fmt.Errorf("pull receipt shows no movement for the wallet account %s", plan.Target.WalletUsdcAta)
	}
	if wallet.PreRaw < 0 || wallet.PostRaw < 0 || wallet.Mint != plan.LiquidityMint {
		return fmt.Errorf("wallet movement mint is %s, want the frozen %s", wallet.Mint, plan.LiquidityMint)
	}
	if delta := wallet.PostRaw - wallet.PreRaw; delta != -plan.AmountRaw {
		return fmt.Errorf("pull wallet moved %d, want exactly minus the frozen %d", delta, plan.AmountRaw)
	}
	vault, ok := receipt.EffectFor(plan.Target.VaultUsdcAta)
	if !ok {
		return fmt.Errorf("pull receipt shows no movement for the frozen custody %s", plan.Target.VaultUsdcAta)
	}
	if vault.PreRaw < 0 || vault.PostRaw < 0 || vault.Mint != plan.LiquidityMint {
		return fmt.Errorf("custody movement mint is %s, want the frozen %s", vault.Mint, plan.LiquidityMint)
	}
	if delta := vault.PostRaw - vault.PreRaw; delta != plan.AmountRaw {
		return fmt.Errorf("pull custody moved %d, want exactly the frozen %d", delta, plan.AmountRaw)
	}
	return nil
}

func (c *Controller) readRemainingAllowance(ctx context.Context, targetContext *TargetExecutionContext) (*int64, error) {
	now := time.Now().Unix()
	if targetContext.PeriodLengthSeconds != nil && *targetContext.PeriodLengthSeconds <= 0 {
		return nil, ErrAllowanceUnknown
	}
	if targetContext.StartTimestamp != nil && now < *targetContext.StartTimestamp {
		return nil, ErrAllowanceUnknown
	}
	// The Subscriptions program reads expiry 0 as never expires.
	if targetContext.ExpiryTimestamp != nil && *targetContext.ExpiryTimestamp != 0 && now >= *targetContext.ExpiryTimestamp {
		return nil, ErrAllowanceUnknown
	}
	allowance, err := c.chain.RemainingDelegationAllowanceRaw(ctx, targetContext.RecurringDelegation, DelegationIdentity{
		Delegator:          targetContext.Wallet,
		Delegatee:          targetContext.VaultPubkey,
		Mint:               targetContext.TokenMint,
		Nonce:              targetContext.RecurringDelegationNonce,
		WalletTokenAccount: targetContext.WalletUsdcAta,
	})
	if err != nil {
		return nil, err
	}
	return &allowance, nil
}

func (c *Controller) holdClaimLease(parent context.Context, claimToken string, targetID int64) (executionScope, error) {
	leaseToken, err := newLeaseToken()
	if err != nil {
		return executionScope{}, err
	}
	acquired, err := c.store.AcquireClaimLease(parent, claimToken, targetID, leaseToken)
	if err != nil {
		return executionScope{}, err
	}
	if !acquired {
		return executionScope{}, fmt.Errorf("%w: claim %s is leased by another executor", ErrOwnershipLost, claimToken)
	}
	execCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(c.leaseRenewWindow)
		defer timer.Stop()
		for {
			select {
			case <-execCtx.Done():
				return
			case <-timer.C:
				renewCtx, renewCancel := context.WithTimeout(execCtx, 15*time.Second)
				renewErr := c.store.RenewClaimLease(renewCtx, claimToken, leaseToken)
				renewCancel()
				if renewErr != nil {
					cancel()
					return
				}
				timer.Reset(c.leaseRenewWindow)
			}
		}
	}()
	release := func() {
		cancel()
		<-done
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		_ = c.store.ReleaseClaimLease(releaseCtx, claimToken, leaseToken)
	}
	return executionScope{leaseToken: leaseToken, ctx: execCtx, release: release}, nil
}

func (t *TargetExecutionContext) hasRouteIdentity() bool { return t.RoutePolicy != nil }

func (t *TargetExecutionContext) depositPlan(amountRaw int64) DepositPlan {
	tokenAta := t.WalletUsdcAta
	vaultTokenAta := t.VaultUsdcAta
	plan := DepositPlan{
		Version:       DepositPlanVersion,
		AmountRaw:     amountRaw,
		Reserve:       derefString(t.CurrentReserve),
		Market:        derefString(t.CurrentMarket),
		LiquidityMint: derefString(t.CurrentLiquidityMint),
		Target: DepositPlanTarget{
			ID:                   t.TargetID,
			ManagedVaultID:       t.RoutePolicy.ManagedVaultID,
			Settings:             t.Settings,
			VaultIndex:           int(t.VaultIndex),
			Wallet:               t.Wallet,
			WalletUsdcAta:        tokenAta,
			WalletTokenAta:       tokenAta,
			VaultPubkey:          t.VaultPubkey,
			VaultUsdcAta:         vaultTokenAta,
			VaultTokenAta:        vaultTokenAta,
			TokenMint:            t.TokenMint,
			RoutePolicyAccount:   t.RoutePolicy.Account,
			SweepPolicyAccount:   t.SweepPolicyAccount,
			SetupPolicyAccount:   t.SetupPolicyAccount,
			RoutePolicySeed:      t.RoutePolicy.Seed,
			CurrentReserve:       t.CurrentReserve,
			CurrentMarket:        t.CurrentMarket,
			CurrentLiquidityMint: t.CurrentLiquidityMint,
		},
	}
	return plan
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func attemptIDOrZero(attempt *DurableAttempt) int64 {
	if attempt == nil {
		return 0
	}
	return attempt.ID
}

func newClaimToken() (string, error) {
	return newRandomToken("autodeposit-claim-")
}

func newLeaseToken() (string, error) {
	return newRandomToken("autodeposit-lease-")
}

func newRandomToken(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate autodeposit token: %w", err)
	}
	return prefix + hex.EncodeToString(random[:]), nil
}
