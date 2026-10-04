package autodeposit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
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
	// leaseRenewWindow is how often an in-flight execution renews its claim
	// lease. Zero selects the default.
	leaseRenewWindow time.Duration
}

// DefaultLeaseRenewWindow renews well inside the ten-minute claim lease.
const DefaultLeaseRenewWindow = 3 * time.Minute

// ControllerDependencies are the controller's explicit dependencies.
type ControllerDependencies struct {
	Store *Store
	Chain Chain
	Wires WireBuilder
	// LeaseRenewWindow is optional.
	LeaseRenewWindow time.Duration
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
	window := deps.LeaseRenewWindow
	if window == 0 {
		window = DefaultLeaseRenewWindow
	}
	if window < 0 || window > DefaultLeaseRenewWindow {
		return nil, errors.New("claim renewal window must be positive and at most three minutes")
	}

	return &Controller{store: deps.Store, chain: deps.Chain, wires: deps.Wires, leaseRenewWindow: window}, nil
}

// Execute resolves one dispatchable target and reports its end state through
// the legacy exit-code protocol. A nil exit code with an error means the run
// never reached an exit.
func (c *Controller) Execute(ctx context.Context, target ExecutableTarget) (*int, error) {
	if err := c.store.requireMainnetTarget(ctx, target.TargetID); err != nil {
		return exit(ExitRecoveryPending), err
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

// durableSettlement adapts the chain and the SQL journal onto the settlement
// protocol: observation and broadcast come from the chain, every record write
// comes from the store under this execution's claim lease.
type durableSettlement struct {
	store      *Store
	chain      Chain
	leaseToken string
}

func (d durableSettlement) Observe(ctx context.Context, attempt DurableAttempt) (AttemptObservation, error) {
	if err := d.store.requireMainnetClaim(ctx, attempt.ClaimToken); err != nil {
		return AttemptObservation{}, err
	}
	return d.chain.Observe(ctx, attempt)
}

func (d durableSettlement) BroadcastExact(ctx context.Context, attempt DurableAttempt) (string, error) {
	if err := d.store.requireMainnetClaim(ctx, attempt.ClaimToken); err != nil {
		return "", err
	}
	return d.chain.BroadcastExact(ctx, attempt)
}

func (d durableSettlement) RecordBroadcast(ctx context.Context, attempt DurableAttempt) (DurableAttempt, error) {
	// First submission always simulates the exact persisted bytes. A transport
	// or simulation error leaves the wire prepared, unsent and still claimed;
	// it is not a chain failure and cannot release custody.
	if err := d.store.requireMainnetClaim(ctx, attempt.ClaimToken); err != nil {
		return DurableAttempt{}, err
	}
	if attempt.BroadcastCount == 0 {
		if err := d.chain.SimulateExact(ctx, attempt); err != nil {
			return DurableAttempt{}, err
		}
	}
	return d.store.RecordAttemptBroadcast(ctx, attempt, d.leaseToken)
}

func (d durableSettlement) RecordObservation(ctx context.Context, attempt DurableAttempt, observation AttemptObservation) (DurableAttempt, error) {
	return d.store.RecordAttemptObservation(ctx, attempt, observation, d.leaseToken)
}

func (c *Controller) settle(scope executionScope, attempt DurableAttempt) (Settlement, error) {
	return SettleDurableAttempt(scope.ctx, attempt, durableSettlement{store: c.store, chain: c.chain, leaseToken: scope.leaseToken})
}

// exit returns an executor exit code for a code value.
func exit(code int) *int { return &code }

func exitCodeIfNotNil(code *int, err error) (*int, error) {
	return code, err
}

// executeRecovery resumes a claim whose pull already holds custody. It runs
// even when the target's desired enablement is off: the funds are already out
// of the wallet.
func (c *Controller) executeRecovery(ctx context.Context, target ExecutableTarget) (*int, error) {
	scope, err := c.holdClaimLease(ctx, target.ClaimToken, target.TargetID)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitRecoveryPending), err)
	}
	defer scope.release()

	recovery, err := c.store.LoadPullRecoveryContext(scope.ctx, target.ClaimToken, target.TargetID, target.ScheduledSlotID)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitRecoveryPending), err)
	}
	if recovery == nil {
		plan, err := c.store.LoadFrozenDepositPlan(scope.ctx, target.ClaimToken, target.TargetID, scope.leaseToken)
		if err != nil {
			return exit(ExitRecoveryPending), err
		}
		return c.executeFrozenClaim(scope, target.ClaimToken, target, plan)
	}
	if recovery.Attempt.State == AttemptConfirmed {
		// Verify the persisted pull's exact effects before treating custody as
		// owned: the wallet debited and the frozen custody credited.
		if err := c.verifyPullEffects(scope.ctx, recovery.Plan, recovery.Attempt); err != nil {
			return exitCodeIfNotNil(exit(ExitTransactionEffectAmbig), err)
		}
	}

	settlement, err := c.settle(scope, recovery.Attempt)
	if err != nil {
		if errors.Is(err, ErrOwnershipLost) {
			return exit(ExitRecoveryPending), err
		}
		return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
	}
	if alert := AlertForAttemptState(settlement.Attempt.State); alert != nil {
		return exit(ExitTransactionEffectAmbig), nil
	}
	if settlement.Attempt.State == AttemptUnknown || settlement.Attempt.State == AttemptSubmitted ||
		settlement.Attempt.State == AttemptPrepared {
		// Unknown is never released and never retried generically: only the
		// settlement protocol may resolve it.
		return exit(ExitRecoveryPending), nil
	}
	if !AttemptAllowsSafeRequeue(settlement.Attempt.State) {
		// Confirmed: the pull landed. The top-up leg decides the rest.
		return c.finishTopUpLeg(scope, target.ClaimToken, target, recovery.Plan, settlement)
	}
	// Conclusive pull failure or expiry: the funds provably never left.
	if _, releaseErr := c.store.ReleaseClaimOnce(scope.ctx, target.ClaimToken, scope.leaseToken); releaseErr != nil {
		return exitCodeIfNotNil(exit(ExitRecoveryPending), releaseErr)
	}
	return exit(ExitDeferred), nil
}

// executeFresh claims and sweeps one scheduled slot.
func (c *Controller) executeFresh(ctx context.Context, target ExecutableTarget) (*int, error) {
	targetContext, err := c.store.LoadTargetExecutionContext(ctx, target.TargetID)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
	}
	if targetContext == nil {
		return exit(ExitNotActionable), nil
	}
	// The delegated authority is validated separately from its allowance: a
	// target without a delegation can never be pulled, but that is a
	// configuration fault, not an exhausted allowance.
	if targetContext.RecurringDelegation == "" {
		return exit(ExitNotActionable), fmt.Errorf("autodeposit target %d has no recurring delegation", target.TargetID)
	}
	if !targetContext.hasRouteIdentity() {
		// The route is unexecutable and no funds moved; waiting for the
		// observer may clear it.
		return exit(ExitPreflightBlocked), fmt.Errorf("autodeposit target %d has no active same_mint_kamino policy", target.TargetID)
	}

	walletBalance, err := c.chain.ConfirmedTokenBalanceRaw(ctx, targetContext.WalletUsdcAta, targetContext.Wallet)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
	}
	remainingAllowance, err := c.readRemainingAllowance(ctx, targetContext)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitDeferred), err)
	}

	claimToken, err := newClaimToken()
	if err != nil {
		return nil, err
	}
	claim, err := c.store.ClaimEligibleLotsOnce(ctx, target.TargetID, claimToken, &target.ScheduledSlotID,
		walletBalance, targetContext.WalletBalanceFloorRaw, targetContext.MaxAmountPerPeriodRaw, remainingAllowance)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitYieldPersistenceFailed), err)
	}
	if claim.Status != ClaimSelected {
		return exit(ExitNoop), nil
	}

	scope, err := c.holdClaimLease(ctx, claimToken, target.TargetID)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitRecoveryPending), err)
	}
	defer scope.release()

	// Freeze amount and destination before any pull wire exists.
	if !targetContext.hasReserveIdentity() {
		if _, releaseErr := c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken); releaseErr != nil {
			return exitCodeIfNotNil(exit(ExitYieldPersistenceFailed), releaseErr)
		}
		return exit(ExitPreflightBlocked), fmt.Errorf("autodeposit target %d has no observed reserve identity", target.TargetID)
	}
	plan := targetContext.depositPlan(claim.AmountRaw)
	frozen, err := c.store.FreezeDepositPlan(scope.ctx, claimToken, scope.leaseToken, plan)
	if err != nil {
		if _, releaseErr := c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken); releaseErr != nil {
			return exitCodeIfNotNil(exit(ExitYieldPersistenceFailed), releaseErr)
		}
		return exitCodeIfNotNil(exit(ExitRecoveryPending), err)
	}

	return c.executeFrozenClaim(scope, claimToken, target, frozen)
}

// An unsigned claim may need account setup. Signed setup remains owned until
// its exact confirmation and decoded account readback, before a pull exists.
func (c *Controller) executeFrozenClaim(scope executionScope, claimToken string, target ExecutableTarget, frozen DepositPlan) (*int, error) {
	ready, err := c.ensureDestinationSetup(scope, claimToken, frozen)
	if err != nil {
		if errors.Is(err, ErrDesiredControlsPending) {
			if _, releaseErr := c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken); releaseErr != nil {
				return exit(ExitRecoveryPending), releaseErr
			}
			return exit(ExitDeferred), nil
		}
		return exit(ExitPreflightBlocked), err
	}
	if !ready {
		return exit(ExitRecoveryPending), nil
	}
	targetContext, err := c.store.LoadTargetExecutionContext(scope.ctx, target.TargetID)
	if err != nil {
		return exit(ExitDependencyUnavailable), err
	}
	if targetContext == nil || !targetContext.FreshActionable || targetContext.RecurringDelegation == "" || targetContext.SweepPolicyAccount != frozen.Target.SweepPolicyAccount || targetContext.Wallet != frozen.Target.Wallet || targetContext.VaultPubkey != frozen.Target.VaultPubkey {
		_, err = c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken)
		return exit(ExitDeferred), err
	}
	walletBalance, err := c.chain.ConfirmedTokenBalanceRaw(scope.ctx, frozen.Target.WalletUsdcAta, frozen.Target.Wallet)
	if err != nil {
		return exit(ExitDependencyUnavailable), err
	}
	allowance, err := c.readRemainingAllowance(scope.ctx, targetContext)
	if err != nil {
		return exit(ExitDeferred), err
	}
	if walletBalance < frozen.AmountRaw || targetContext.WalletBalanceFloorRaw < 0 || walletBalance-frozen.AmountRaw < targetContext.WalletBalanceFloorRaw || *allowance < frozen.AmountRaw || (targetContext.MaxAmountPerPeriodRaw != nil && *targetContext.MaxAmountPerPeriodRaw < frozen.AmountRaw) {
		_, err = c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken)
		return exit(ExitDeferred), err
	}
	// Preflight the destination BEFORE the pull: the top-up must be executable
	// against the frozen reserve, obligation and custody, or the wallet never
	// moves. No funds have moved when this fails.
	if _, err := c.wires.ConfirmTopUpRoute(scope.ctx, frozen); err != nil {
		if _, releaseErr := c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken); releaseErr != nil {
			return exitCodeIfNotNil(exit(ExitYieldPersistenceFailed), releaseErr)
		}
		return exit(ExitPreflightBlocked), err
	}

	custodyBefore, err := c.chain.ConfirmedTokenBalanceRaw(scope.ctx, frozen.Target.VaultUsdcAta, frozen.Target.VaultPubkey)
	if err != nil {
		return exit(ExitDependencyUnavailable), err
	}
	if custodyBefore != 0 {
		if _, err := c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken); err != nil {
			return exit(ExitYieldPersistenceFailed), err
		}
		return exit(ExitPreflightBlocked), errors.New("direct autodeposit requires empty idle custody before pull")
	}
	blockhash, lastValid, err := c.chain.LatestBlockhash(scope.ctx)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
	}
	if err = c.store.checkUnsignedDesiredAdmission(scope.ctx, target.TargetID, claimToken, scope.leaseToken); err != nil {
		if errors.Is(err, ErrDesiredControlsPending) {
			if _, releaseErr := c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken); releaseErr != nil {
				return exit(ExitRecoveryPending), releaseErr
			}
			return exit(ExitDeferred), nil
		}
		return exit(ExitRecoveryPending), err
	}
	wire, err := c.wires.BuildPull(scope.ctx, PullWireRequest{
		Plan: frozen, RecurringDelegation: targetContext.RecurringDelegation,
		RecentBlockhash: blockhash, LastValidBlockHeight: lastValid,
	})
	if err != nil {
		// A wire that was never signed never moved funds: release the claim.
		if _, releaseErr := c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken); releaseErr != nil {
			return exitCodeIfNotNil(exit(ExitYieldPersistenceFailed), releaseErr)
		}
		return exit(ExitPreflightBlocked), err
	}
	prepared, err := c.store.PersistPreparedAttempt(scope.ctx, PreparedAttempt{
		ClaimToken:               claimToken,
		TargetID:                 target.TargetID,
		ScheduledSlotID:          target.ScheduledSlotID,
		OperationKind:            OperationPull,
		AmountRaw:                frozen.AmountRaw,
		SourcePreBalanceRaw:      walletBalance,
		DestinationPreBalanceRaw: 0,
		ProtectionFloorRaw:       &targetContext.WalletBalanceFloorRaw,
		SourceDesiredRevision:    targetContext.DesiredRevision,
		Signature:                wire.Signature,
		SignedTransactionBase64:  wire.SignedTransactionBase64,
		SignedTransactionSHA256:  wire.SignedTransactionSHA256,
		RecentBlockhash:          wire.RecentBlockhash,
		LastValidBlockHeight:     wire.LastValidBlockHeight,
	}, scope.leaseToken)
	if err != nil {
		if errors.Is(err, ErrDesiredControlsPending) {
			if _, releaseErr := c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken); releaseErr != nil {
				return exit(ExitRecoveryPending), releaseErr
			}
			return exit(ExitDeferred), nil
		}
		if errors.Is(err, ErrOwnershipLost) {
			return exit(ExitRecoveryPending), err
		}
		return exitCodeIfNotNil(exit(ExitYieldPersistenceFailed), err)
	}

	settlement, err := c.settle(scope, prepared)
	if err != nil {
		if errors.Is(err, ErrOwnershipLost) {
			return exit(ExitRecoveryPending), err
		}
		return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
	}
	switch {
	case AttemptAllowsSafeRequeue(settlement.Attempt.State):
		if _, releaseErr := c.store.ReleaseClaimOnce(scope.ctx, claimToken, scope.leaseToken); releaseErr != nil {
			return exitCodeIfNotNil(exit(ExitYieldPersistenceFailed), releaseErr)
		}
		return exit(ExitDeferred), nil
	case !AttemptHoldsClaim(settlement.Attempt.State):
		return exit(ExitRecoveryPending), nil
	case settlement.Attempt.State != AttemptConfirmed:
		return exit(ExitRecoveryPending), nil
	}
	// The pull receipt must prove the exact integer movement before custody is
	// treated as owned.
	if err := c.verifyPullEffects(scope.ctx, frozen, settlement.Attempt); err != nil {
		return exitCodeIfNotNil(exit(ExitTransactionEffectAmbig), err)
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
	for completed := 0; completed < 4; completed++ {
		attempt, err := c.store.LoadDestinationSetup(scope.ctx, claimToken, scope.leaseToken)
		if err != nil {
			return false, err
		}
		if attempt != nil && attempt.State == AttemptAmbiguous {
			return false, nil
		}
		if attempt != nil && attempt.State == AttemptConfirmed {
			if _, err = builder.ReadbackDestinationSetup(scope.ctx, plan, attempt.Plan, *attempt.ConfirmedSlot); err != nil {
				return false, err
			}
		}
		if attempt == nil || attempt.State == AttemptConfirmed || attempt.State == AttemptFailed || attempt.State == AttemptExpired {
			current, err := c.store.LoadTargetExecutionContext(scope.ctx, plan.Target.ID)
			if err != nil {
				return false, err
			}
			if current == nil || !current.FreshActionable {
				return true, nil
			}
			next, err := builder.InspectDestinationSetup(scope.ctx, plan)
			if err != nil {
				return false, err
			}
			if next == nil {
				return true, nil
			}
			blockhash, height, err := c.chain.LatestBlockhash(scope.ctx)
			if err != nil {
				return false, err
			}
			if err = c.store.checkUnsignedDesiredAdmission(scope.ctx, plan.Target.ID, claimToken, scope.leaseToken); err != nil {
				return false, err
			}
			wire, err := builder.BuildDestinationSetup(scope.ctx, plan, *next, blockhash, height)
			if err != nil {
				return false, err
			}
			saved, err := c.store.PersistDestinationSetup(scope.ctx, claimToken, scope.leaseToken, *next, wire, current.DesiredRevision)
			if err != nil {
				return false, err
			}
			attempt = &saved
		}
		if err = builder.ProveDestinationSetup(scope.ctx, plan, *attempt); err != nil {
			return false, err
		}
		if err := c.store.requireMainnetTarget(scope.ctx, plan.Target.ID); err != nil {
			return false, err
		}
		observation, err := c.chain.Observe(scope.ctx, attempt.durable())
		if err != nil {
			return false, err
		}
		if observation.State == AttemptUnknown {
			if attempt.BroadcastCount == 0 {
				if err = c.chain.SimulateExact(scope.ctx, attempt.durable()); err != nil {
					return false, err
				}
			}
			saved, err := c.store.RecordDestinationSetupBroadcast(scope.ctx, *attempt, scope.leaseToken)
			if err != nil {
				return false, err
			}
			attempt = &saved
			if err := c.store.requireMainnetTarget(scope.ctx, plan.Target.ID); err != nil {
				return false, err
			}
			returned, sendErr := c.chain.BroadcastExact(scope.ctx, attempt.durable())
			if sendErr == nil && returned != attempt.Wire.Signature {
				observation = AttemptObservation{State: AttemptAmbiguous, Err: errors.New("setup broadcast returned another signature")}
			} else {
				observation, err = c.chain.Observe(scope.ctx, attempt.durable())
				if err != nil {
					return false, err
				}
				if observation.State == AttemptUnknown {
					observation.Err = sendErr
				}
			}
		}
		var evidence *SetupReadback
		if observation.State == AttemptConfirmed {
			if observation.ConfirmedSlot == nil {
				return false, errors.New("setup confirmed without slot")
			}
			readback, readErr := builder.ReadbackDestinationSetup(scope.ctx, plan, attempt.Plan, *observation.ConfirmedSlot)
			err = readErr
			evidence = &readback
			if err != nil {
				return false, err
			}
		}
		if _, err = c.store.RecordDestinationSetupObservation(scope.ctx, *attempt, observation, evidence, scope.leaseToken); err != nil {
			return false, err
		}
		if observation.State != AttemptConfirmed {
			return false, nil
		}
	}
	// Four dependency stages were proved; route inspection runs once more.
	next, err := builder.InspectDestinationSetup(scope.ctx, plan)
	return next == nil, err
}

// finishTopUpLeg resolves the deposit side of a confirmed pull: record the
// execution the pull owns, reconcile the persisted top-up when one holds the
// claim, verify the persisted wire and its exact receipt, and finalize the
// claim atomically through the shared accounting function.
func (c *Controller) finishTopUpLeg(scope executionScope, claimToken string, target ExecutableTarget, plan DepositPlan, pull Settlement) (*int, error) {
	// The confirmed pull owns the execution row: it is keyed to the pull's
	// signature, slot and amount, exactly the legacy accounted-recovery key.
	confirmedPull := pull.Attempt
	if err := c.verifyPullEffects(scope.ctx, plan, confirmedPull); err != nil {
		return exit(ExitTransactionEffectAmbig), err
	}
	// The custody that the pull funded is the vault's USDC ATA, never the
	// wallet ATA: every top-up read below is against the pulled account.
	custody, err := c.chain.ConfirmedTokenBalanceRaw(scope.ctx, plan.Target.VaultUsdcAta, plan.Target.VaultPubkey)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
	}
	existingTopUp, err := c.store.LoadLatestAttempt(scope.ctx, claimToken, OperationTopUp)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
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
		return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
	}

	switch ClassifyDirectTopUpRecovery(existingState, custody, plan.AmountRaw, persistedPreBalance, siblingDeposits) {
	case TopUpEffectAmbiguous:
		return exit(ExitTransactionEffectAmbig), nil
	case TopUpReconcilePersisted:
		settlement, err := c.settle(scope, *existingTopUp)
		if err != nil {
			if errors.Is(err, ErrOwnershipLost) {
				return exit(ExitRecoveryPending), err
			}
			return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
		}
		if alert := AlertForAttemptState(settlement.Attempt.State); alert != nil {
			return exit(ExitTransactionEffectAmbig), nil
		}
		if settlement.Attempt.State != AttemptConfirmed {
			return exit(ExitRecoveryPending), nil
		}
		pull = settlement
	case TopUpPrepareOrRequeue:
		if err := c.assertOwnership(scope, claimToken); err != nil {
			return exit(ExitRecoveryPending), err
		}
		executionID, err := c.ensurePullExecution(scope, claimToken, target, plan, confirmedPull)
		if err != nil {
			if errors.Is(err, ErrOwnershipLost) {
				return exit(ExitRecoveryPending), err
			}
			return exitCodeIfNotNil(exit(ExitYieldPersistenceFailed), err)
		}
		blockhash, lastValid, err := c.chain.LatestBlockhash(scope.ctx)
		if err != nil {
			return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
		}
		wire, err := c.wires.BuildTopUp(scope.ctx, TopUpWireRequest{Plan: plan, RecentBlockhash: blockhash, LastValidBlockHeight: lastValid})
		if err != nil {
			// The pulled custody stays claimed for recovery; it must never be
			// released while only the deposit leg failed.
			return exitCodeIfNotNil(exit(ExitKaminoTopUpFailed), err)
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
				return exit(ExitRecoveryPending), err
			}
			return exitCodeIfNotNil(exit(ExitYieldPersistenceFailed), err)
		}
		settlement, err := c.settle(scope, prepared)
		if err != nil {
			if errors.Is(err, ErrOwnershipLost) {
				return exit(ExitRecoveryPending), err
			}
			return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
		}
		if alert := AlertForAttemptState(settlement.Attempt.State); alert != nil {
			return exit(ExitTransactionEffectAmbig), nil
		}
		if settlement.Attempt.State != AttemptConfirmed {
			// Unknown or submitted custody stays claimed; it is never released.
			return exit(ExitRecoveryPending), nil
		}
		pull = settlement
	}

	// The execution id belongs to the immutable prepared top-up. A missing id
	// cannot be repaired by mutating signed custody evidence after confirmation.
	if pull.Attempt.ExecutionID == nil {
		return exit(ExitTransactionEffectAmbig), &EffectAmbiguousError{Detail: "persisted top-up has no immutable execution identity"}
	}

	// The persisted wire is the deposit's proof; the receipt is checked
	// against it. Both must agree on the frozen identity and exact amount.
	route, err := c.wires.ConfirmTopUpRoute(scope.ctx, plan)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitKaminoTopUpFailed), err)
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
		return exitCodeIfNotNil(exit(ExitTransactionEffectAmbig), err)
	}
	if err := c.verifyTopUpEffects(scope.ctx, plan, route, pull); err != nil {
		return exitCodeIfNotNil(exit(ExitTransactionEffectAmbig), err)
	}

	// Finalization re-proves ownership and completes claim, slot, execution
	// and yield accounting in one atomic database call. There is no permanent
	// pending here: the execution row already exists.
	if err := c.assertOwnership(scope, claimToken); err != nil {
		return exit(ExitRecoveryPending), err
	}
	positionAmountRaw, observedSlot, err := c.chain.ConfirmedVaultPositionRaw(scope.ctx, plan, route)
	if err != nil {
		return exitCodeIfNotNil(exit(ExitDependencyUnavailable), err)
	}
	if _, err := c.store.FinalizeConfirmedAutodeposit(scope.ctx, claimToken, *pull.Attempt.ExecutionID, target.ScheduledSlotID,
		scope.leaseToken, positionAmountRaw, observedSlot); err != nil {
		if errors.Is(err, ErrOwnershipLost) {
			return exit(ExitRecoveryPending), err
		}
		return exitCodeIfNotNil(exit(ExitYieldPersistenceFailed), err)
	}
	return exit(ExitCompleted), nil
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
	if delta := custody.PostRaw - custody.PreRaw; delta != -plan.AmountRaw {
		return fmt.Errorf("top-up custody moved %d, want exactly minus the frozen %d", delta, plan.AmountRaw)
	}
	supply, ok := receipt.EffectFor(route.Position.LiquiditySupply)
	if !ok {
		return fmt.Errorf("top-up receipt shows no movement for the reserve liquidity supply %s", route.Position.LiquiditySupply)
	}
	if supply.PreRaw < 0 || supply.PostRaw < 0 || supply.Mint != plan.LiquidityMint {
		return fmt.Errorf("liquidity supply movement mint is %s, want the frozen %s", supply.Mint, plan.LiquidityMint)
	}
	if delta := supply.PostRaw - supply.PreRaw; delta != plan.AmountRaw {
		return fmt.Errorf("reserve liquidity moved %d, want exactly the frozen %d", delta, plan.AmountRaw)
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
	if targetContext.ExpiryTimestamp != nil && now >= *targetContext.ExpiryTimestamp {
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
