package autodeposit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/solana"
)

// landResendEvery matches the fleet landing cadence; landWindow bounds one
// dispatch's landing to a blockhash lifetime.
var (
	landResendEvery = time.Second
	landWindow      = 90 * time.Second
)

// WireBuilder is the wire-construction capability the runtime must supply: the
// exact Loyal subscriptions pull and Kamino reserve top-up instruction
// builders, the route preflight a pull must pass before any funds move, and
// the persisted-wire proof a top-up receipt is verified against. It is
// deliberately a consumer-owned interface — this package owns WHEN a wire may
// exist, the builder owns its bytes, and no other wire is ever accepted.
type WireBuilder interface {
	// BuildPull builds and signs the delegated wallet pull for the frozen plan.
	BuildPull(ctx context.Context, request PullWireRequest) (BuiltWire, error)
	// ConfirmTopUpRoute proves the frozen destination against confirmed chain
	// state. The controller runs it BEFORE the pull: an unexecutable
	// destination never justifies moving wallet funds.
	ConfirmTopUpRoute(ctx context.Context, plan DepositPlan) (TopUpRoute, error)
	// BuildTopUp builds and signs the Kamino top-up of the pulled custody into
	// the frozen destination reserve.
	BuildTopUp(ctx context.Context, request TopUpWireRequest) (BuiltWire, error)
	// ProveTopUpWire verifies a persisted top-up attempt's immutable wire
	// byte-for-byte against the frozen plan and confirmed route.
	ProveTopUpWire(plan DepositPlan, attempt DurableAttempt, route TopUpRoute) error
	// FeePayer is the account that pays for every wire this builder signs.
	FeePayer() string
}

// PullWireRequest freezes every input of one pull wire.
type PullWireRequest struct {
	Plan                 DepositPlan
	RecurringDelegation  string
	RecentBlockhash      string
	LastValidBlockHeight int64
}

// TopUpWireRequest freezes every input of one top-up wire.
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

// Controller is the production Autodeposit executor: a real implementation of
// TargetExecutor over the family's durable SQL journal and live chain reads.
// It owns the durable order — claim, lease, freeze, preflight, persist exact
// wire, simulate the exact bytes, durable broadcast intent, send, observe,
// verify the exact receipt — and never constructs a replacement for a
// persisted wire.
type Controller struct {
	store *Store
	chain Chain
	wires WireBuilder
	facts *engine.Facts
	// leaseRenewWindow is how often an in-flight execution renews its claim
	// lease. Zero selects the default.
	leaseRenewWindow time.Duration
	idleToleranceRaw int64
}

// DefaultLeaseRenewWindow renews well inside the ten-minute claim lease.
const DefaultLeaseRenewWindow = 3 * time.Minute

// FeePayerMinimumLamports is the TS executor's fee-payer floor (04e792e0):
// below it the payer cannot be trusted to land setup, pull and top-up, so no
// claim starts. The 0.55 SOL warning is an alert rule on the balance gauge.
const FeePayerMinimumLamports = 50_000_000

// ControllerDependencies are the controller's explicit dependencies.
type ControllerDependencies struct {
	Store *Store
	Chain Chain
	Wires WireBuilder
	Facts *engine.Facts
	// LeaseRenewWindow is optional.
	LeaseRenewWindow time.Duration
	// IdleToleranceRaw is the vault idle balance a direct deposit may leave
	// beside it. Zero tolerates none.
	IdleToleranceRaw int64
}

// NewController validates the dependencies. A controller without all three
// capabilities is not constructed: there is no partially real execution mode.
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

// FeePayerLamports reads the balance of the one account that pays every wire.
func (c *Controller) FeePayerLamports(ctx context.Context) (string, uint64, error) {
	payer := c.wires.FeePayer()
	lamports, err := c.chain.ConfirmedLamports(ctx, payer)
	return payer, lamports, err
}

// release returns an unspent claim's lots; a release that fails is a claim
// transition failure, never a yield-persistence one.
func (c *Controller) release(scope executionScope, claimToken string, result ExecutorResult, cause error) (ExecutorResult, error) {
	if _, err := c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken); err != nil {
		return ResultClaimTransitionFailed, err
	}
	return result, cause
}

// Execute resolves one dispatchable target and reports its end state through
// a typed family outcome. Errors retain the outcome reached before the failure.
func (c *Controller) Execute(ctx context.Context, target ExecutableTarget) (ExecutorResult, error) {
	if err := c.store.requireMainnetTarget(ctx, target.TargetID); err != nil {
		return ResultRecoveryPending, err
	}
	if target.isRecovery() {
		return c.executeRecovery(ctx, target)
	}
	return c.executeFresh(ctx, target)
}

// executionScope is one execution's owned resources: the lease token, the
// cancellable context every chain call runs under, and the release that joins
// the lease keeper before returning.
type executionScope struct {
	leaseToken string
	ctx        context.Context
	release    func()
}

// assertOwnership renews the claim lease, failing when another executor took
// it. It is called before every custody mutation so a lost fence can never
// write state the executor no longer owns.
func (c *Controller) assertOwnership(scope executionScope, claimToken string) error {
	if err := c.store.RenewClaimLease(scope.ctx, claimToken, scope.leaseToken); err != nil {
		return err
	}
	return nil
}

// settle lands one persisted pull or top-up through the shared send path.
// Every send is counted first, as the TS executor's
// recordAutodepositAttemptBroadcast does, and the first send simulates the
// exact bytes. The outcome is recorded as the TS observation state: confirmed,
// failed or expired. When the landing window closes first, the attempt stays
// submitted and the next dispatch lands it again.
func (c *Controller) settle(scope executionScope, attempt DurableAttempt) (Settlement, error) {
	if attempt.Signature == "" || attempt.SignedTransactionBase64 == "" {
		return Settlement{}, errors.New("durable attempt has no persisted wire identity")
	}
	if attempt.State == AttemptConfirmed || attempt.State == AttemptFailed || attempt.State == AttemptExpired {
		return Settlement{Attempt: attempt}, nil
	}
	if err := c.store.requireMainnetClaim(scope.ctx, attempt.ClaimToken); err != nil {
		return Settlement{}, err
	}
	wire, err := base64StdDecode(attempt.SignedTransactionBase64)
	if err != nil {
		return Settlement{}, fmt.Errorf("decode persisted %s wire: %w", attempt.OperationKind, err)
	}
	landCtx, cancel := context.WithTimeout(scope.ctx, landWindow)
	out, err := solana.Land(landCtx, c.chain, solana.Attempt{
		Wire: wire, Signature: attempt.Signature, LastValidBlockHeight: uint64(attempt.LastValidBlockHeight),
		Sends: attempt.BroadcastCount, Required: solana.Confirmed,
	}, landResendEvery, func(sendCtx context.Context) error {
		if attempt.BroadcastCount == 0 {
			// A simulation error leaves the wire prepared, unsent and claimed.
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
	case solana.Landed:
		slot := int64(out.Slot)
		observation, code = AttemptObservation{State: AttemptConfirmed, ConfirmedSlot: &slot}, ""
	case solana.Failed:
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

// executeRecovery resumes a claim whose pull already holds custody. It runs
// even when the target's desired enablement is off: the funds are already out
// of the wallet.
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
		// Verify the persisted pull's exact effects before treating custody as
		// owned: the wallet debited and the frozen custody credited.
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
	if alert := AlertForAttemptState(settlement.Attempt.State); alert != nil {
		return ResultTransactionEffectAmbig, nil
	}
	if settlement.Attempt.State == AttemptUnknown || settlement.Attempt.State == AttemptSubmitted ||
		settlement.Attempt.State == AttemptPrepared {
		// Unknown is never released and never retried generically: only the
		// settlement protocol may resolve it.
		return ResultRecoveryPending, nil
	}
	if !AttemptAllowsSafeRequeue(settlement.Attempt.State) {
		// Confirmed: the pull landed. The top-up leg decides the rest.
		return c.finishTopUpLeg(scope, target.ClaimToken, target, recovery.Plan, settlement)
	}
	// Conclusive pull failure or expiry: the funds provably never left.
	return c.release(scope, target.ClaimToken, ResultDeferred, nil)
}

// executeFresh claims and sweeps one scheduled slot.
func (c *Controller) executeFresh(ctx context.Context, target ExecutableTarget) (ExecutorResult, error) {
	targetContext, err := c.store.LoadTargetExecutionContext(ctx, target.TargetID)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	if targetContext == nil {
		return ResultNotActionable, nil
	}
	// The delegated authority is validated separately from its allowance: a
	// target without a delegation can never be pulled, but that is a
	// configuration fault, not an exhausted allowance.
	if targetContext.RecurringDelegation == "" {
		return ResultNotActionable, fmt.Errorf("autodeposit target %d has no recurring delegation", target.TargetID)
	}
	if !targetContext.hasRouteIdentity() {
		// The route is unexecutable and no funds moved; waiting for the
		// observer may clear it.
		return ResultPreflightBlocked, fmt.Errorf("autodeposit target %d has no active same_mint_kamino policy", target.TargetID)
	}

	walletBalance, err := c.chain.ConfirmedTokenBalanceRaw(ctx, targetContext.WalletUsdcAta, targetContext.Wallet)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	remainingAllowance, err := c.readRemainingAllowance(ctx, targetContext)
	if err != nil {
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
		deferred, err := c.store.DeferIdleScheduledSlot(ctx, target.TargetID, target.ScheduledSlotID, custody)
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
	claim, err := c.store.ClaimEligibleLotsOnce(ctx, target.TargetID, claimToken, &target.ScheduledSlotID,
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

	// Freeze amount and destination before any pull wire exists.
	if !targetContext.hasReserveIdentity() {
		return c.release(scope, claimToken, ResultPreflightBlocked, fmt.Errorf("autodeposit target %d has no observed reserve identity", target.TargetID))
	}
	// No fleet move updates the position pointer; the vault's observed holding
	// is the destination. Only a fresh claim may redirect: a frozen plan is
	// immutable.
	if err := c.store.ResolveDepositReserve(scope.ctx, targetContext, time.Now()); err != nil {
		return c.release(scope, claimToken, ResultPreflightBlocked, err)
	}
	plan := targetContext.depositPlan(claim.AmountRaw)
	frozen, err := c.store.FreezeDepositPlan(scope.ctx, claimToken, scope.leaseToken, plan)
	if err != nil {
		return c.release(scope, claimToken, ResultRecoveryPending, err)
	}

	return c.executeFrozenClaim(scope, claimToken, target, frozen)
}

// An unsigned claim may need account setup before its pull. Like the TS
// executor's ensure-before-pull path the chain account is the fact: each stage
// is inspected, built, landed and read back here. A stage creates its account
// and pays its own rent shortfall in one atomic transaction, so a stage
// repeated after a crash fails on the existing account and costs only a fee.
func (c *Controller) executeFrozenClaim(scope executionScope, claimToken string, target ExecutableTarget, frozen DepositPlan) (ExecutorResult, error) {
	ready, err := c.ensureDestinationSetup(scope, claimToken, frozen)
	if err != nil {
		// No pull wire exists yet, so the wallet never moved: release like
		// every other pre-pull refusal, as the TS executor did.
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
		return ResultDeferred, err
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
	// KLend refuses a deposit that mints no collateral, and a pulled amount it
	// refuses can never leave custody. The TS executor's pre-pull dry run
	// refused it here.
	if uint64(frozen.AmountRaw) < route.MinimumDepositRaw {
		return c.release(scope, claimToken, ResultPreflightBlocked, fmt.Errorf("%w: amount %d is below the reserve's minimum deposit %d", ErrRouteNotExecutable, frozen.AmountRaw, route.MinimumDepositRaw))
	}

	custodyBefore, err := c.chain.ConfirmedTokenBalanceRaw(scope.ctx, frozen.Target.VaultUsdcAta, frozen.Target.VaultPubkey)
	if err != nil {
		return ResultDependencyUnavailable, err
	}
	// Idle above the tolerance arrived after the pre-claim check, or this is a
	// frozen claim resumed after a crash: the pull still waits for it.
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
		// A wire that was never signed never moved funds: release the claim.
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
	// The pull receipt must prove the exact integer movement before custody is
	// treated as owned.
	if err := c.verifyPullEffects(scope.ctx, frozen, settlement.Attempt); err != nil {
		return ResultTransactionEffectAmbig, err
	}
	return c.finishTopUpLeg(scope, claimToken, target, frozen, settlement)
}

func (c *Controller) ensureDestinationSetup(scope executionScope, claimToken string, plan DepositPlan) (bool, error) {
	if err := c.store.requireMainnetTarget(scope.ctx, plan.Target.ID); err != nil {
		return false, err
	}
	builder, ok := c.wires.(DestinationSetupBuilder)
	if !ok {
		return true, nil
	} // Existing exact-route test adapters have no setup capability.
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
		out, err := solana.Land(landCtx, c.chain, solana.Attempt{
			Wire: bytes, Signature: wire.Signature, LastValidBlockHeight: uint64(wire.LastValidBlockHeight), Required: solana.Confirmed,
		}, landResendEvery, func(context.Context) error { return c.assertOwnership(scope, claimToken) })
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && scope.ctx.Err() == nil {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if out.Kind != solana.Landed {
			// Not landed: the next dispatch inspects the chain again.
			return false, nil
		}
		if err = builder.ReadbackDestinationSetup(scope.ctx, plan, *next, int64(out.Slot)); err != nil {
			return false, err
		}
	}
	// Four dependency stages were created; route inspection runs once more.
	next, err := builder.InspectDestinationSetup(scope.ctx, plan)
	return next == nil, err
}

// finishTopUpLeg resolves the deposit side of a confirmed pull: record the
// execution the pull owns, reconcile the persisted top-up when one holds the
// claim, verify the persisted wire and its exact receipt, and finalize the
// claim atomically through the shared accounting function.
func (c *Controller) finishTopUpLeg(scope executionScope, claimToken string, target ExecutableTarget, plan DepositPlan, pull Settlement) (ExecutorResult, error) {
	// The confirmed pull owns the execution row: it is keyed to the pull's
	// signature, slot and amount, exactly the legacy accounted-recovery key.
	confirmedPull := pull.Attempt
	if err := c.verifyPullEffects(scope.ctx, plan, confirmedPull); err != nil {
		return ResultTransactionEffectAmbig, err
	}
	// The custody that the pull funded is the vault's USDC ATA, never the
	// wallet ATA: every top-up read below is against the pulled account.
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
		if alert := AlertForAttemptState(settlement.Attempt.State); alert != nil {
			return ResultTransactionEffectAmbig, nil
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
			return ResultYieldPersistenceFailed, err
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
		if alert := AlertForAttemptState(settlement.Attempt.State); alert != nil {
			return ResultTransactionEffectAmbig, nil
		}
		if settlement.Attempt.State != AttemptConfirmed {
			// Unknown or submitted custody stays claimed; it is never released.
			return ResultRecoveryPending, nil
		}
		pull = settlement
	}

	// The execution id belongs to the immutable prepared top-up. A missing id
	// cannot be repaired by mutating signed custody evidence after confirmation.
	if pull.Attempt.ExecutionID == nil {
		return ResultTransactionEffectAmbig, &EffectAmbiguousError{Detail: "persisted top-up has no immutable execution identity"}
	}

	// The persisted wire is the deposit's proof; the receipt is checked
	// against it. Both must agree on the frozen identity and exact amount.
	route, err := c.wires.ConfirmTopUpRoute(scope.ctx, plan)
	if err != nil {
		return ResultKaminoTopUpFailed, err
	}
	var proofErr error
	if contextual, ok := c.wires.(interface {
		ProveTopUpWireContext(context.Context, DepositPlan, DurableAttempt, TopUpRoute) error
	}); ok {
		proofErr = contextual.ProveTopUpWireContext(scope.ctx, plan, pull.Attempt, route)
	} else {
		proofErr = c.wires.ProveTopUpWire(plan, pull.Attempt, route)
	}
	if err := proofErr; err != nil {
		return ResultTransactionEffectAmbig, err
	}
	if err := c.verifyTopUpEffects(scope.ctx, plan, route, pull); err != nil {
		return ResultTransactionEffectAmbig, err
	}

	// Finalization re-proves ownership and completes claim, slot, execution
	// and yield accounting in one atomic database call. There is no permanent
	// pending here: the execution row already exists.
	if err := c.assertOwnership(scope, claimToken); err != nil {
		return ResultRecoveryPending, err
	}
	positionAmountRaw, observedSlot, err := c.chain.ConfirmedVaultPositionRaw(scope.ctx, plan, route)
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

// ensurePullExecution creates the execution row a confirmed pull owns, from
// the pull attempt's recorded evidence, and links the claim's confirmed
// attempts to it. Idempotent on the pull signature's dedupe key.
func (c *Controller) ensurePullExecution(scope executionScope, claimToken string, target ExecutableTarget, plan DepositPlan, confirmedPull DurableAttempt) (int64, error) {
	// Current custody can include sibling movements. Accounting uses only the
	// immutable receipt of this exact pull, including its actual pre-balances.
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

// verifyTopUpEffects proves the confirmed receipt against the proved wire: the
// custody lost and the reserve's liquidity supply gained exactly the frozen
// amount, in USDC, at a slot that does not precede the recorded confirmation.
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

// verifyPullEffects proves the pull receipt: the wallet debited and the frozen
// custody credited exactly the planned amount, in the frozen mint, at a slot
// that does not precede the recorded confirmation.
func (c *Controller) verifyPullEffects(ctx context.Context, plan DepositPlan, attempt DurableAttempt) error {
	if err := c.store.requireMainnetTarget(ctx, plan.Target.ID); err != nil {
		return err
	}
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

// readRemainingAllowance reads the live delegated allowance against the
// frozen identity. Unknown is deferred, never a zero cap; a conclusive zero is
// a hard stop.
func (c *Controller) readRemainingAllowance(ctx context.Context, targetContext *TargetExecutionContext) (*int64, error) {
	var nonce *uint64
	if targetContext.RecurringDelegationNonce != nil {
		if *targetContext.RecurringDelegationNonce < 0 {
			return nil, ErrAllowanceUnknown
		}
		value := uint64(*targetContext.RecurringDelegationNonce)
		nonce = &value
	}
	now := time.Now().Unix()
	if targetContext.PeriodLengthSeconds != nil && *targetContext.PeriodLengthSeconds <= 0 {
		return nil, ErrAllowanceUnknown
	}
	if targetContext.StartTimestamp != nil && now < *targetContext.StartTimestamp {
		return nil, ErrAllowanceUnknown
	}
	// The Subscriptions program reads expiry 0 as a delegation that never
	// expires; nearly every production delegation is created that way.
	if targetContext.ExpiryTimestamp != nil && *targetContext.ExpiryTimestamp != 0 && now >= *targetContext.ExpiryTimestamp {
		return nil, ErrAllowanceUnknown
	}
	allowance, err := c.chain.RemainingDelegationAllowanceRaw(ctx, targetContext.RecurringDelegation, DelegationIdentity{
		Delegator:          targetContext.Wallet,
		Delegatee:          targetContext.VaultPubkey,
		Mint:               targetContext.TokenMint,
		Nonce:              nonce,
		WalletTokenAccount: targetContext.WalletUsdcAta,
	})
	if err != nil {
		return nil, err
	}
	return &allowance, nil
}

// holdClaimLease takes the claim lease and keeps renewing it while the
// execution runs. The returned context is owned by this execution: losing the
// lease cancels it, so every in-flight chain call is abandoned with the fence.
// The keeper goroutine joins before release returns.
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
					// Losing the fence stops the execution that no longer owns
					// the claim; its in-flight chain call is abandoned with it.
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

func (t *TargetExecutionContext) hasReserveIdentity() bool {
	return t.CurrentReserve != nil && *t.CurrentReserve != "" &&
		t.CurrentMarket != nil && *t.CurrentMarket != "" &&
		t.CurrentLiquidityMint != nil && *t.CurrentLiquidityMint != ""
}

// depositPlan freezes the sweep into the immutable plan shape. The frozen
// identity comes from the observed context at freeze time; recovery never
// re-derives it.
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
