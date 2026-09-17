package backyardrwa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"
)

const productionRouteKey = "rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"

var immutableImageVersionPattern = regexp.MustCompile(`^sha-[0-9a-f]{40}$`)
var immutableRenderLeaseOwnerPattern = regexp.MustCompile(`^render:srv-[a-z0-9]+:sha-[0-9a-f]{40}$`)
var errConfirmedObservationUnavailable = errors.New("confirmed route observation is temporarily unavailable")

type Worker struct {
	routeKey     string
	interval     time.Duration
	manifest     RouteManifest
	runtime      tickRuntime
	leaseHandoff startupLeaseHandoffRuntime
}

type startupLeaseHandoffRuntime struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) error
}

type tickRuntime struct {
	prepareInitialization            func(context.Context, RouteManifest, Decision) (Observation, KaminoInitializationRequest, error)
	admitInitialization              func(context.Context, string, Observation, Decision, KaminoInitializationRequest) error
	buildInitialization              func(context.Context, string, KaminoInitializationRequest) error
	refreshUnwind                    func(context.Context) error
	completeUnwind                   func(context.Context, Observation) (bool, error)
	allocationSentWindow             func(context.Context, string) (uint64, error)
	loadNonterminal                  func(context.Context, string) (*PersistedOperation, error)
	advance                          func(context.Context, PersistedOperation) error
	observe                          func(context.Context) (Observation, error)
	loadLatch                        func(context.Context, string) (ManualRecoveryLatch, bool, error)
	recordManualRecovery             func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error)
	recordManualRecoveryAtGeneration func(context.Context, string, Observation, Decision, string, string, int64) (DecisionRecord, error)
	beforeRecordLatchedHold          func(context.Context, ManualRecoveryLatch) error
	prepareBridge                    func(context.Context, RouteManifest, Decision) (Observation, BridgeExecutionEvidence, error)
	prepareKamino                    func(context.Context, RouteManifest, Decision) (Observation, KaminoExecutionEvidence, error)
	prepareJupiter                   func(context.Context, RouteManifest, Decision) (Observation, JupiterExecutionEvidence, error)
	recordDecision                   func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error)
	admitBridge                      func(context.Context, string, Observation, Decision, BridgeExecutionEvidence) error
	admitKamino                      func(context.Context, string, Observation, Decision, KaminoExecutionEvidence) error
	admitJupiter                     func(context.Context, string, Observation, Decision, JupiterExecutionEvidence) error
	buildBridge                      func(context.Context, string, BridgeExecutionEvidence) error
	buildKamino                      func(context.Context, string, KaminoExecutionEvidence) error
	buildJupiter                     func(context.Context, string, JupiterExecutionEvidence) error
	recordBudgetHold                 func(context.Context, string, *BudgetHold) error
}

// productionJournal is the journal evidence the production observe path merges
// into a snapshot. *Database implements it.
type productionJournal interface {
	PostMutationNAVRequired(ctx context.Context, routeKey string) (bool, error)
	ReconciledBridgeJournal(ctx context.Context, routeKey string) (ReconciledBridgeJournalState, error)
	RecordPositionSnapshot(ctx context.Context, routeKey string, observation Observation) error
}

// productionObserveState is the production observe path with its three readers
// injectable: the confirmed account batch, the reconciled journal, and the
// pinned program identity. productionTickRuntime wires all three to the real
// database, RPC client, and chain; tests stub them to drive the same merge.
type productionObserveState struct {
	manifest RouteManifest
	routeKey string
	journal  productionJournal
	batch    func(context.Context) (Observation, error)
	identity func(context.Context) (programIdentityObservation, error)
}

// observe merges one confirmed batch with the journal and the pinned program
// identity, so every monitor input the decision engine reads comes from the
// same tick and none of it is inferred from a valuation.
func (p productionObserveState) observe(ctx context.Context) (Observation, error) {
	observation, err := p.batch(ctx)
	if err != nil {
		return Observation{}, err
	}
	if err := p.enrich(ctx, &observation); err != nil {
		return Observation{}, err
	}
	// Integrity or health failures carry no decodable valuation. Preserve the
	// last good position projection; the hold decision is the durable record.
	if observation.Snapshot.StrategyReceiptIntegrityFault || observation.Snapshot.ManualReason != "" {
		return observation, nil
	}
	if err := p.journal.RecordPositionSnapshot(ctx, p.routeKey, observation); err != nil {
		return Observation{}, err
	}
	return observation, nil
}

// enrich is the one production snapshot merge used by both the outer
// observation and construction refreshes. Keeping the identity watcher in
// this callback prevents a refresh from receiving a raw NAV snapshot while
// the outer tick sees a verified program image.
func (p productionObserveState) enrich(ctx context.Context, observation *Observation) error {
	if err := p.mergeJournal(ctx, observation); err != nil {
		return err
	}
	identity, err := p.identity(ctx)
	if err != nil {
		return err
	}
	applyProgramIdentityObservation(observation, identity)
	observation.Snapshot.InitializationPolicyReady = false
	if observation.Snapshot.PilotActive {
		_, bindingErr := p.manifest.initializerBinding(observation.Snapshot.RouteLane)
		observation.Snapshot.InitializationPolicyReady = bindingErr == nil
	}
	return nil
}

// mergeJournal copies the durable facts that arm the monitors into a
// construction refresh. It is shared with the outer observation so a refresh
// cannot accidentally decide from a raw NAV snapshot with default journal
// state.
func (p productionObserveState) mergeJournal(ctx context.Context, observation *Observation) error {
	if observation == nil {
		return fmt.Errorf("production observation is nil")
	}
	required, err := p.journal.PostMutationNAVRequired(ctx, p.routeKey)
	if err != nil {
		return err
	}
	if reader, ok := p.journal.(interface {
		PilotRuntimeEnabled(context.Context, string) (bool, error)
	}); ok {
		active, err := reader.PilotRuntimeEnabled(ctx, p.routeKey)
		if err != nil {
			return err
		}
		observation.Snapshot.PilotActive = active
	}
	observation.Snapshot.PostMutationNAVRequired = required
	journal, err := p.journal.ReconciledBridgeJournal(ctx, p.routeKey)
	if err != nil {
		return err
	}
	observation.Snapshot.JournalSequenceKnown = journal.TicketSequenceKnown
	observation.Snapshot.JournalReconciledSequenceRaw = journal.TicketSequenceRaw
	observation.Snapshot.JournalArmedNAVKnown = journal.ArmedNAVKnown
	observation.Snapshot.JournalArmedNAVRaw = journal.ArmedNAVRaw
	observation.Snapshot.JournalArmedNAVReturnDataMissing = journal.ArmedNAVReturnDataMissing
	observation.Snapshot.JournalArmedNAVMalformed = journal.ArmedNAVMalformed
	observation.Snapshot.StagedAmountRaw = journal.StagedAmountRaw
	observation.Snapshot.StagedAmountKnown = journal.StagedAmountKnown
	observation.Snapshot.StageTransient = journal.StageAfterTicket
	observation.Snapshot.CapitalMutated = journal.MutationAfterReport
	if reader, ok := p.journal.(interface {
		LoadUnwindIntent(context.Context, string) (*UnwindIntent, error)
	}); ok {
		intent, err := reader.LoadUnwindIntent(ctx, p.routeKey)
		if err != nil {
			return err
		}
		if err := applyUnwindIntent(&observation.Snapshot, intent); err != nil {
			observation.Snapshot.ManualReason = err.Error()
		}
	}
	if reader, ok := p.journal.(interface {
		SelectorEntryPaused(context.Context, string) (bool, error)
	}); ok {
		paused, err := reader.SelectorEntryPaused(ctx, p.routeKey)
		if err != nil {
			return err
		}
		observation.Snapshot.SelectorEntryPaused = paused
	}
	if reader, ok := p.journal.(interface {
		LoadSelectorEntry(context.Context, string) (*SelectorEntry, error)
	}); ok {
		entry, err := reader.LoadSelectorEntry(ctx, p.routeKey)
		if err != nil {
			return err
		}
		if err = applySelectorEntry(&observation.Snapshot, entry, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

func productionTickRuntime(database *Database, rpc *RPCClient, manifest RouteManifest) tickRuntime {
	state := productionObserveState{
		manifest: manifest, routeKey: productionRouteKey,
		journal: database,
		batch: func(ctx context.Context) (Observation, error) {
			observedManifest, err := manifestForUnwind(ctx, database, manifest)
			if err != nil {
				return Observation{}, err
			}
			return ObserveConfirmedRouteSnapshot(ctx, rpc, observedManifest)
		},
		identity: newProgramIdentityWatcher(rpc).observe,
	}
	return tickRuntime{
		refreshUnwind: func(ctx context.Context) error {
			return database.refreshSelectorUnwind(ctx, rpc, manifest, state.observe)
		},
		prepareInitialization: func(ctx context.Context, m RouteManifest, d Decision) (Observation, KaminoInitializationRequest, error) {
			return prepareKaminoInitialization(ctx, rpc, m, d, state.observe)
		},
		admitInitialization: func(ctx context.Context, id string, o Observation, d Decision, r KaminoInitializationRequest) error {
			return database.admitKaminoInitialization(ctx, rpc, manifest, id, o, d, r)
		},
		buildInitialization: func(ctx context.Context, id string, r KaminoInitializationRequest) error {
			return BuildSimulateAndPersistKaminoInitialization(ctx, database, rpc, id, manifest, r)
		},
		completeUnwind: func(ctx context.Context, observation Observation) (bool, error) {
			if !observation.Snapshot.Unwind || !unwindComplete(observation.Snapshot) {
				return false, nil
			}
			intent, err := database.LoadUnwindIntent(ctx, productionRouteKey)
			if err != nil {
				return false, err
			}
			if intent == nil {
				return false, fmt.Errorf("unwind disappeared before completion")
			}
			return true, database.CompleteUnwindIntent(ctx, productionRouteKey, *intent, observation.Snapshot)
		},
		loadNonterminal: database.LoadNonterminal,
		advance: func(ctx context.Context, operation PersistedOperation) error {
			return AdvanceNonterminal(ctx, database, rpc, operation)
		},
		observe:                          state.observe,
		loadLatch:                        database.ManualRecoveryLatch,
		recordManualRecovery:             database.RecordManualRecovery,
		recordManualRecoveryAtGeneration: database.RecordManualRecoveryAtGeneration,
		prepareBridge: func(ctx context.Context, manifest RouteManifest, decision Decision) (Observation, BridgeExecutionEvidence, error) {
			manifest, err := manifestForUnwind(ctx, database, manifest)
			if err != nil {
				return Observation{}, BridgeExecutionEvidence{}, err
			}
			return observeConfirmedBridgeExecutionEvidenceWithEnrichment(ctx, rpc, manifest, decision, state.enrich)
		},
		prepareKamino: func(ctx context.Context, manifest RouteManifest, decision Decision) (Observation, KaminoExecutionEvidence, error) {
			manifest, err := manifestForUnwind(ctx, database, manifest)
			if err != nil {
				return Observation{}, KaminoExecutionEvidence{}, err
			}
			return observeConfirmedKaminoExecutionEvidenceWithEnrichment(ctx, rpc, manifest, decision, state.enrich)
		},
		prepareJupiter: func(ctx context.Context, manifest RouteManifest, decision Decision) (Observation, JupiterExecutionEvidence, error) {
			manifest, err := manifestForUnwind(ctx, database, manifest)
			if err != nil {
				return Observation{}, JupiterExecutionEvidence{}, err
			}
			return observeConfirmedJupiterExecutionEvidenceWithEnrichment(ctx, rpc, manifest, decision, productionJupiterClient(), state.enrich)
		},
		allocationSentWindow: database.AllocationSentRawTrailingWindow,
		recordDecision:       database.RecordDecision,
		recordBudgetHold:     database.RecordPhase3BudgetHold,
		admitBridge: func(ctx context.Context, operationID string, observation Observation, decision Decision, evidence BridgeExecutionEvidence) error {
			if evidence.Request.Action == ReportNAV && observation.Snapshot.PositionDebtRaw > 0 && positionReturnRoute(observation.Snapshot.RouteLane) {
				return database.admitPhase3Funding(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence.Request, evidence.ExpectedEffects)
			}
			if evidence.Request.Action == ReportNAV && observation.Snapshot.PositionCollateralRaw > 0 && observation.Snapshot.PositionDebtRaw == 0 {
				return database.admitPhase3PositionReturnNAV(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence)
			}
			if observation.Snapshot.CollateralIdleRaw > 0 || observation.Snapshot.DebtIdleRaw > 0 {
				return database.admitPhase3CollateralReturn(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence.Request, evidence.ExpectedEffects)
			}
			return database.admitPhase3Bridge(ctx, rpc, operationID, observation, decision, evidence)
		},
		admitKamino: func(ctx context.Context, operationID string, observation Observation, decision Decision, evidence KaminoExecutionEvidence) error {
			_, leg, err := kaminoPrimeUSDCInstruction(evidence.Request)
			if err != nil {
				return err
			}
			if leg == kaminoLegDeposit && evidence.Request.Action == OpenRouteStep {
				return database.admitPhase3Deposit(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence)
			}
			if leg == kaminoLegBorrow && evidence.Request.Action == OpenRouteStep {
				return database.admitPhase3Borrow(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence)
			}
			return database.admitPhase3Withdrawal(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence)
		},
		admitJupiter: func(ctx context.Context, operationID string, observation Observation, decision Decision, evidence JupiterExecutionEvidence) error {
			if evidence.Request.Action == SwapDebtToCollateralStep {
				return database.admitPhase3LeverageSwap(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence)
			}
			if evidence.Request.Action == SwapStableToCollateralStep {
				return database.admitPhase3EntrySwap(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence)
			}
			if evidence.Request.FullPayoffFunding {
				return database.admitPhase3Funding(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence.Request, evidence.ExpectedEffects)
			}
			return database.admitPhase3CollateralReturn(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence.Request, evidence.ExpectedEffects)
		},
		buildBridge: func(ctx context.Context, operationID string, evidence BridgeExecutionEvidence) error {
			return BuildSimulateAndPersistBridge(ctx, database, rpc, operationID, evidence)
		},
		buildKamino: func(ctx context.Context, operationID string, evidence KaminoExecutionEvidence) error {
			return BuildSimulateAndPersistKamino(ctx, database, rpc, operationID, evidence)
		},
		buildJupiter: func(ctx context.Context, operationID string, evidence JupiterExecutionEvidence) error {
			return BuildSimulateAndPersistJupiter(ctx, database, rpc, operationID, evidence)
		},
	}
}

func confirmedObservationUnavailable(err error) error {
	if err == nil || errors.Is(err, errConfirmedObservationUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", errConfirmedObservationUnavailable, err)
}

func NewWorker(database *Database, rpc *RPCClient, routeKey string, config Config) (*Worker, error) {
	if database == nil || rpc == nil || routeKey != productionRouteKey || config.validateLease() != nil {
		return nil, fmt.Errorf("invalid concrete worker configuration")
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return nil, err
	}
	return &Worker{routeKey: routeKey, interval: config.PollInterval, manifest: manifest, runtime: productionTickRuntime(database, rpc, manifest)}, nil
}

func (w *Worker) Tick(ctx context.Context) error {
	if w == nil || w.routeKey != productionRouteKey {
		return fmt.Errorf("worker route is not the fixed production route")
	}
	// The manual recovery latch is re-read before anything else in the tick, so
	// a healthy batch, a restart, or a new worker state object can never resume
	// execution behind a durable stop.
	if w.runtime.loadLatch != nil {
		latch, latched, err := w.runtime.loadLatch(ctx, w.routeKey)
		if err != nil {
			return err
		}
		if latched {
			return w.recordLatchedHold(ctx, latch)
		}
	}
	operation, err := w.runtime.loadNonterminal(ctx, w.routeKey)
	if err != nil {
		return err
	}
	if operation != nil {
		if err := w.runtime.advance(ctx, *operation); err != nil {
			return err
		}
		// Reconciling is the only state whose successful advance finalizes a
		// confirmed mutation. Reobserve immediately before another decision can
		// be created; the next poll will journal from this fresh state.
		if operation.Status == Reconciling {
			remaining, err := w.runtime.loadNonterminal(ctx, w.routeKey)
			if err != nil {
				return err
			}
			if remaining == nil {
				_, err = w.runtime.observe(ctx)
				return err
			}
		}
		return nil
	}
	observation, err := w.runtime.observe(ctx)
	if err != nil {
		return err
	}
	if w.runtime.completeUnwind != nil {
		completed, err := w.runtime.completeUnwind(ctx, observation)
		if err != nil {
			return err
		}
		if completed {
			return nil
		}
	}
	decision := Decide(observation.Snapshot)
	if decision.Action == Hold && decision.Reason == "unwind_requires_fresh_admission" && w.runtime.refreshUnwind != nil {
		if err = w.runtime.refreshUnwind(ctx); err == nil {
			return nil
		}
		var hold *BudgetHold
		if !errors.As(err, &hold) {
			return err
		}
		// A refused renewal stays a retryable journaled hold. It never turns
		// ordinary accrued interest into a permanent manual recovery latch.
		decision.Reason = hold.Reason
	}
	// Decide maps every observation-level hold to one generic reason. The
	// observer records the audited Kamino health reason on the snapshot, so it
	// is carried into the durable decision instead of being discarded.
	if decision.Action == HoldManualRecovery && observation.Snapshot.ManualReason != "" {
		decision.Reason = observation.Snapshot.ManualReason
	}
	if err := decision.Validate(); err != nil {
		return err
	}
	if w.manifest.PolicyCatalog.SHA256 == nil || !sha256Pattern.MatchString(*w.manifest.PolicyCatalog.SHA256) {
		return ErrBridgePrerequisitesUnavailable
	}
	policyHash := *w.manifest.PolicyCatalog.SHA256
	if executionDecision, err := fixedRouteAction(decision.Action, decision.StrategyKey); err == nil &&
		executionDecision == VoltrAllocateToSquads && w.runtime.allocationSentWindow != nil {
		// The strategy-two allocation policy's daily window is enforced on
		// chain, but a wire that only fails at landing still burned the
		// attempt. Guard the journal too: what this worker already sent in
		// the trailing 24h plus the next amount must stay inside the bound.
		sentRaw, err := w.runtime.allocationSentWindow(ctx, w.routeKey)
		if err != nil {
			return err
		}
		if err := evaluateAllocationDailyLimit(sentRaw, uint64(decision.AmountRaw)); err != nil {
			var hold *BudgetHold
			if !errors.As(err, &hold) {
				return err
			}
			decision.Action = Hold
			decision.Reason = hold.Reason
			decision.AmountRaw = 0
			if err := decision.Validate(); err != nil {
				return err
			}
		}
	}
	if decision.Action == Hold || decision.Action == HoldManualRecovery {
		if decision.Action == HoldManualRecovery {
			return w.recordManualRecoveryDecision(ctx, observation, decision, policyHash)
		}
		if _, err := w.runtime.recordDecision(ctx, w.routeKey, observation, decision, w.manifest.SHA256, policyHash); err != nil {
			return err
		}
		return nil
	}
	if blocker := w.manifest.executionBlocker(); blocker != nil {
		return blocker
	}
	var initializationRequest KaminoInitializationRequest
	var bridgeEvidence BridgeExecutionEvidence
	var kaminoEvidence KaminoExecutionEvidence
	var jupiterEvidence JupiterExecutionEvidence
	preparedDecision := decision
	executionDecision, err := fixedRouteAction(decision.Action, decision.StrategyKey)
	if err != nil {
		return err
	}
	wireDecision := decision
	wireDecision.Action = executionDecision
	switch executionDecision {
	case InitializeKaminoObligation:
		if w.runtime.prepareInitialization == nil {
			return budgetHold("initializer_preparation_unavailable")
		}
		observation, initializationRequest, err = w.runtime.prepareInitialization(ctx, w.manifest, wireDecision)
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		observation, bridgeEvidence, err = w.runtime.prepareBridge(ctx, w.manifest, wireDecision)
	case OpenPrimeUSDCStep, DeleverPrimeUSDCStep, OpenRouteStep, DeleverRouteStep:
		observation, kaminoEvidence, err = w.runtime.prepareKamino(ctx, w.manifest, wireDecision)
	case SwapUSDCToPrimeStep, SwapPrimeToUSDCStep, SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep:
		observation, jupiterEvidence, err = w.runtime.prepareJupiter(ctx, w.manifest, wireDecision)
	default:
		return fmt.Errorf("action %s is not dispatchable", decision.Action)
	}
	// Construction refreshes the same confirmed inputs that will be sent. A
	// refreshed manual-recovery decision is a new durable stop, not ordinary
	// decision drift, and must be recorded before the tick returns.
	if ok, persistErr := w.persistRefreshedManualRecovery(ctx, observation, policyHash); ok {
		if persistErr != nil {
			return persistErr
		}
		return nil
	}
	if err != nil {
		return err
	}
	decision = Decide(observation.Snapshot)
	if err := decision.Validate(); err != nil {
		return err
	}
	if !decisionsEqual(decision, preparedDecision) || !decisionsEqual(Decide(observation.Snapshot), preparedDecision) {
		return fmt.Errorf("prepared evidence does not match the refreshed decision")
	}
	record, err := w.runtime.recordDecision(ctx, w.routeKey, observation, decision, w.manifest.SHA256, policyHash)
	if err != nil {
		return err
	}
	if record.Status != Decided || record.OperationID == "" {
		return fmt.Errorf("actionable decision was not durably recorded as decided")
	}
	switch executionDecision {
	case InitializeKaminoObligation:
		if w.runtime.admitInitialization == nil || w.runtime.buildInitialization == nil {
			err = budgetHold("initializer_admission_unavailable")
		} else {
			err = w.runtime.admitInitialization(ctx, record.OperationID, observation, decision, initializationRequest)
			if err == nil {
				err = w.runtime.buildInitialization(ctx, record.OperationID, initializationRequest)
			}
		}
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		if w.runtime.admitBridge == nil {
			err = budgetHold("bridge_admission_unavailable")
		} else {
			err = w.runtime.admitBridge(ctx, record.OperationID, observation, decision, bridgeEvidence)
			if err == nil {
				err = w.runtime.buildBridge(ctx, record.OperationID, bridgeEvidence)
			}
		}
	case OpenPrimeUSDCStep, DeleverPrimeUSDCStep, OpenRouteStep, DeleverRouteStep:
		if w.runtime.admitKamino == nil {
			err = budgetHold("position_admission_unavailable")
		} else {
			err = w.runtime.admitKamino(ctx, record.OperationID, observation, decision, kaminoEvidence)
			if err == nil {
				err = w.runtime.buildKamino(ctx, record.OperationID, kaminoEvidence)
			}
		}
	case SwapUSDCToPrimeStep, SwapPrimeToUSDCStep, SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep:
		if w.runtime.admitJupiter == nil {
			err = budgetHold("swap_admission_unavailable")
		} else {
			err = w.runtime.admitJupiter(ctx, record.OperationID, observation, decision, jupiterEvidence)
			if err == nil {
				err = w.runtime.buildJupiter(ctx, record.OperationID, jupiterEvidence)
			}
		}
	default:
		return fmt.Errorf("prepared evidence no longer matches an actionable decision")
	}
	return w.journalTickError(ctx, record.OperationID, err)
}

// journalTickError journals an admission hold for the tick's operation once.
// A hold already recorded durably by its own send path - the pre-broadcast
// spending-limit refusal - must not be journaled again: the operation row is
// already failed, so the store would reject the transition, and joining that
// rejection into the hold would let the run loop treat the whole thing as a
// pure hold and mask the store failure.
func (w *Worker) journalTickError(ctx context.Context, operationID string, err error) error {
	var hold *BudgetHold
	if !errors.As(err, &hold) || hold.alreadyJournaled || w.runtime.recordBudgetHold == nil {
		return err
	}
	if journalErr := w.runtime.recordBudgetHold(ctx, operationID, hold); journalErr != nil {
		return errors.Join(err, journalErr)
	}
	return err
}

// recordLatchedHold re-records the same durable hold identity on every latched
// tick. The observation identity comes from the latch itself, so the recorded
// evidence stays byte-identical across ticks and restarts and the journal keeps
// showing the stop without anything touching the chain.
func (w *Worker) recordLatchedHold(ctx context.Context, latch ManualRecoveryLatch) error {
	if latch.Reason == "" || latch.ObservationID == "" || latch.ObservationSlot <= 0 || latch.Generation < 0 {
		return fmt.Errorf("latched manual recovery identity is incomplete")
	}
	if w.runtime.beforeRecordLatchedHold != nil {
		if err := w.runtime.beforeRecordLatchedHold(ctx, latch); err != nil {
			return err
		}
	}
	observation := Observation{ObservedAt: time.Now().UTC(), Snapshot: Snapshot{
		ObservationID: latch.ObservationID, Slot: latch.ObservationSlot,
		RouteKind: RouteKind, Fresh: true, MonitorsArmed: true,
	}}
	decision := Decision{
		Action:         HoldManualRecovery,
		Reason:         "latched:" + latch.Reason,
		AmountRaw:      0,
		IdempotencyKey: fmt.Sprintf("%s:latched:%s", latch.ObservationID, latch.Reason),
	}
	if err := decision.Validate(); err != nil {
		return err
	}
	if w.manifest.PolicyCatalog.SHA256 == nil || !sha256Pattern.MatchString(*w.manifest.PolicyCatalog.SHA256) {
		return ErrBridgePrerequisitesUnavailable
	}
	policyHash := *w.manifest.PolicyCatalog.SHA256
	if w.runtime.recordManualRecoveryAtGeneration != nil {
		if _, err := w.runtime.recordManualRecoveryAtGeneration(ctx, w.routeKey, observation, decision, w.manifest.SHA256, policyHash, latch.Generation); err != nil {
			if errors.Is(err, errManualRecoveryLatchGenerationChanged) && w.runtime.loadLatch != nil {
				_, _, rereadErr := w.runtime.loadLatch(ctx, w.routeKey)
				return rereadErr
			}
			return err
		}
		return nil
	}
	return w.recordManualRecoveryDecision(ctx, observation, decision, policyHash)
}

func (w *Worker) recordManualRecoveryDecision(ctx context.Context, observation Observation, decision Decision, policyHash string) error {
	if w.runtime.recordManualRecovery == nil {
		return fmt.Errorf("manual recovery persistence runtime is unavailable")
	}
	if _, err := w.runtime.recordManualRecovery(ctx, w.routeKey, observation, decision, w.manifest.SHA256, policyHash); err != nil {
		return err
	}
	return nil
}

func (w *Worker) persistRefreshedManualRecovery(ctx context.Context, observation Observation, policyHash string) (bool, error) {
	if observation.Snapshot.ObservationID == "" || observation.Snapshot.Slot <= 0 {
		return false, nil
	}
	decision := Decide(observation.Snapshot)
	if decision.Action != HoldManualRecovery {
		return false, nil
	}
	return true, w.recordManualRecoveryDecision(ctx, observation, decision, policyHash)
}

type routeLeaser interface {
	AcquireRouteLease(context.Context, string, string, time.Duration) (RouteLease, error)
	RefreshRouteLease(context.Context, time.Duration) (RouteLease, error)
	ReleaseRouteLease(context.Context) (bool, error)
}

func (r startupLeaseHandoffRuntime) acquire(ctx context.Context, leases routeLeaser, routeKey, owner string, ttl time.Duration) error {
	now := r.now
	if now == nil {
		now = time.Now
	}
	wait := r.wait
	if wait == nil {
		wait = func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	window := 2 * ttl
	cadence := ttl / 10
	if cadence < 10*time.Millisecond {
		cadence = 10 * time.Millisecond
	}
	if cadence > time.Second {
		cadence = time.Second
	}
	deadline := now().Add(window)
	lastUnavailable := ErrRouteLeaseUnavailable
	for {
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return lastUnavailable
		}
		attemptCtx, cancel := context.WithTimeout(ctx, remaining)
		_, err := leases.AcquireRouteLease(attemptCtx, routeKey, owner, ttl)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return lastUnavailable
		}
		if !errors.Is(err, ErrRouteLeaseUnavailable) {
			return err
		}
		lastUnavailable = err
		remaining = deadline.Sub(now())
		if remaining <= 0 {
			return lastUnavailable
		}
		delay := cadence
		if remaining < delay {
			delay = remaining
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
}

// isPureHold reports whether a tick ended in exactly the journaled
// spending-limit hold and nothing else. The refusal is already recorded on
// the operation row under squadsSpendingLimitReason and self-heals at the
// limit's period boundary, so exiting the process would only restart into the
// same refusal. A hold joined or wrapped together with any other fault - a
// store rejection, a wire error - is a real fault: continuing would suppress
// the accompanying error, so only a pure hold skips the leg.
func isPureHold(err error) bool {
	var hold *BudgetHold
	if !errors.As(err, &hold) || hold.Reason != squadsSpendingLimitReason {
		return false
	}
	switch unwrappable := err.(type) {
	case interface{ Unwrap() []error }:
		for _, member := range unwrappable.Unwrap() {
			if !isPureHold(member) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return isPureHold(unwrappable.Unwrap())
	default:
		return true
	}
}

func (w *Worker) runTicks(ctx context.Context, leaseErrors <-chan error, tick func(context.Context) error) error {
	for {
		if err := tick(ctx); err != nil {
			select {
			case leaseErr := <-leaseErrors:
				return leaseErr
			default:
			}
			// Confirmed-observation gaps and pure journaled spending-limit
			// holds skip this tick's leg and retry on the next interval;
			// anything else - including a hold joined with a store error -
			// is a process fault and stops the worker.
			if !errors.Is(err, errConfirmedObservationUnavailable) && !isPureHold(err) {
				return err
			}
		}
		timer := time.NewTimer(w.interval)
		select {
		case err := <-leaseErrors:
			timer.Stop()
			return err
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Run acquires the route fence before the first observation, refreshes it on a
// bounded cadence, and cancels the active tick immediately if ownership is
// lost. Release is compare-and-clear on the exact fencing token, so a stale
// process can never clear its successor's lease.
func (w *Worker) Run(ctx context.Context, leases routeLeaser, owner string, config Config) (runErr error) {
	if w == nil || leases == nil || !immutableRenderLeaseOwnerPattern.MatchString(owner) || config.validateLease() != nil {
		return fmt.Errorf("invalid leased worker runtime")
	}
	if err := w.leaseHandoff.acquire(ctx, leases, w.routeKey, owner, config.LeaseTTL); err != nil {
		return err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := leases.ReleaseRouteLease(releaseCtx)
		if err != nil {
			if runErr == nil || errors.Is(runErr, context.Canceled) {
				runErr = err
			} else {
				runErr = errors.Join(runErr, fmt.Errorf("release route lease: %w", err))
			}
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	leaseErrors := make(chan error, 1)
	refreshStopped := make(chan struct{})
	go func() {
		defer close(refreshStopped)
		ticker := time.NewTicker(config.LeaseRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if _, err := leases.RefreshRouteLease(runCtx, config.LeaseTTL); err != nil {
					select {
					case leaseErrors <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	runErr = w.runTicks(runCtx, leaseErrors, w.Tick)
	cancel()
	<-refreshStopped
	select {
	case leaseErr := <-leaseErrors:
		if leaseErr != nil {
			runErr = leaseErr
		}
	default:
	}
	return runErr
}

// Run wires the single direct pgx/RPC process. Missing deployment artifacts are
// an explicit startup failure, not a read-only mode or an alternate executor.
func Run(ctx context.Context, out io.Writer) error {
	if ctx == nil || out == nil {
		return fmt.Errorf("missing runtime dependency")
	}
	runtimeConfig := RuntimeConfigFromEnvironment()
	if err := runtimeConfig.Validate(); err != nil {
		return err
	}
	if runtimeConfig.RouteKey != productionRouteKey {
		return fmt.Errorf("Backyard worker route key does not match the fixed production route")
	}
	leaseOwner, err := runtimeConfig.LeaseOwner()
	if err != nil {
		return err
	}
	if _, err := loadPinnedPolicySigner(); err != nil {
		return err
	}
	database, err := OpenDatabase(ctx, runtimeConfig.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	rpc, err := NewRPCClient(runtimeConfig.RPCURL)
	if err != nil {
		return err
	}
	worker, err := NewWorker(database, rpc, runtimeConfig.RouteKey, DefaultConfig())
	if err != nil {
		return err
	}
	shadowMode := os.Getenv("BACKYARD_RWA_SELECTOR_SHADOW")
	if shadowMode != "" && shadowMode != "0" && shadowMode != "1" {
		return fmt.Errorf("BACKYARD_RWA_SELECTOR_SHADOW must be 0 or 1")
	}
	liveMode := os.Getenv("BACKYARD_RWA_SELECTOR_LIVE")
	if liveMode != "" && liveMode != "0" && liveMode != "1" {
		return fmt.Errorf("BACKYARD_RWA_SELECTOR_LIVE must be 0 or 1")
	}
	if liveMode == "1" && shadowMode == "1" {
		return fmt.Errorf("choose one selector mode")
	}
	if shadowMode == "1" || liveMode == "1" {
		feed, err := NewEconomicFeed(ctx, os.Getenv("TIMESCALEDB_URL"))
		if err != nil {
			return err
		}
		feedCtx, cancelFeed := context.WithCancel(ctx)
		feedDone := make(chan struct{})
		shadowIdentity := newProgramIdentityWatcher(rpc).observe
		// Economic collection stays off the transaction loop. Live acceptance
		// is fenced against its pre-observation version and existing pilot;
		// shadow records rankings only. Neither collector sends transactions.
		go func() {
			defer close(feedDone)
			interval := time.Minute
			if liveMode == "1" {
				interval = 15 * time.Second
			}
			runSelectorSamples(feedCtx, interval, func(ctx context.Context) {
				_ = feed.Refresh(ctx)
				if liveMode == "1" {
					markets, _ := feed.Snapshot()
					result, err := database.evaluateSelector(ctx, rpc, worker.manifest, markets, shadowIdentity, DefaultSelectorPolicy())
					if err != nil {
						// Avoid emitting RPC/DB errors that may contain service URLs.
						_, _ = fmt.Fprintln(out, "backyard-rwa-worker: selector sample unavailable; retaining current authority")
					} else if result.Action == "ENTER" || result.Action == "SWITCH" {
						_, _ = fmt.Fprintf(out, "backyard-rwa-worker: selector action=%s source=%s destination=%s\n", result.Action, result.SourceLane, result.DestinationLane)
					}
					return
				}
				observation, err := observeSelectorShadow(ctx, database, rpc, worker.manifest, shadowIdentity)
				if err == nil {
					markets, _ := feed.Snapshot()
					_, _ = database.RecordSelectorShadow(ctx, worker.routeKey, observation, markets)
				}
			})
		}()
		defer func() { cancelFeed(); <-feedDone; feed.Close() }()
	}
	if _, err := fmt.Fprintf(out,
		"backyard-rwa-worker: starting serialized confirmed lifecycle route=%s image=%s lease_owner=%s manifest_sha256=%s\n",
		runtimeConfig.RouteKey, runtimeConfig.ImageVersion, leaseOwner, worker.manifest.SHA256,
	); err != nil {
		return err
	}
	err = worker.Run(ctx, database, leaseOwner, DefaultConfig())
	if errors.Is(err, context.Canceled) {
		return nil
	}
	if errors.Is(err, ErrTransactionConstructionUnavailable) {
		return ErrTransactionConstructionUnavailable
	}
	return err
}
