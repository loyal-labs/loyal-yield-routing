package backyard

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

const productionRouteKey = "rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"

// leaseOwnerPattern is the shared engine instance identity
// (engine.InstanceOwner): fixed Backyard scope, unique instance, immutable
// release. Authority comes from the database fencing token, never this text.
var leaseOwnerPattern = regexp.MustCompile(`^worker:backyard:[a-zA-Z0-9_-]{1,80}:sha-[0-9a-f]{40}$`)

// ValidLeaseOwner reports whether owner is a deployment identity this runtime
// accepts; any other string is rejected before it can reach the database.
func ValidLeaseOwner(owner string) bool { return leaseOwnerPattern.MatchString(owner) }

var errConfirmedObservationUnavailable = errors.New("confirmed route observation is temporarily unavailable")

type Worker struct {
	wake         chan struct{}
	routeKey     string
	interval     time.Duration
	manifest     RouteManifest
	runtime      tickRuntime
	leaseHandoff startupLeaseHandoffRuntime
	// noRoom reports a no_room hold: always on /metrics, hourly in the log.
	noRoom noRoomLog
}

type startupLeaseHandoffRuntime struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) error
}

type tickRuntime struct {
	withdrawalHealth                 func(context.Context, WithdrawalHealth) error
	observeWithdrawalHealth          func(context.Context) (Observation, error)
	prepareInitialization            func(context.Context, RouteManifest, Decision) (Observation, KaminoInitializationRequest, error)
	buildInitialization              func(context.Context, string, KaminoInitializationRequest) error
	loadNonterminal                  func(context.Context, string) (*PersistedOperation, error)
	advance                          func(context.Context, PersistedOperation) error
	observe                          func(context.Context) (Observation, error)
	loadLatch                        func(context.Context, string) (ManualRecoveryLatch, bool, error)
	recordManualRecovery             func(context.Context, string, Observation, Decision, string) (DecisionRecord, error)
	recordManualRecoveryAtGeneration func(context.Context, string, Observation, Decision, string, int64) (DecisionRecord, error)
	beforeRecordLatchedHold          func(context.Context, ManualRecoveryLatch) error
	prepareBridge                    func(context.Context, RouteManifest, Decision, Observation) (Observation, BridgeExecutionEvidence, error)
	prepareKamino                    func(context.Context, RouteManifest, Decision, Observation) (Observation, KaminoExecutionEvidence, error)
	prepareJupiter                   func(context.Context, RouteManifest, Decision, Observation) (Observation, JupiterExecutionEvidence, error)
	prepareHastra                    func(context.Context, Decision, Observation) (Observation, HastraExecutionEvidence, error)
	// prepareStep builds an atomic step (stepDecision) from the tick's batch.
	prepareStep    func(context.Context, RouteManifest, Decision, Observation) (Observation, StepExecutionEvidence, error)
	recordDecision func(context.Context, string, Observation, Decision, string) (DecisionRecord, error)
	// bind persists the decided operation's integrity record before build.
	bind                  func(context.Context, string, Observation, Decision, any, ExpectedEffects) error
	buildBridge           func(context.Context, string, BridgeExecutionEvidence) error
	buildKamino           func(context.Context, string, KaminoExecutionEvidence) error
	buildJupiter          func(context.Context, string, JupiterExecutionEvidence) error
	buildHastra           func(context.Context, string, HastraExecutionEvidence) error
	buildStep             func(context.Context, string, StepExecutionEvidence) error
	recordBudgetHold      func(context.Context, string, *BudgetHold) error
	// watchRoom hands the account view the reserves this tick waits on
	// (leverage_room_watch.go); nil without a streaming view.
	watchRoom func([]roomWatch, func(roomUpdate))
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
		if observation.planning != nil {
			reader, ok := p.journal.(interface {
				validateRoutePlanningState(context.Context, string, *routePlanningState) error
			})
			if !ok {
				return Observation{}, fmt.Errorf("planning generation reader unavailable")
			}
			if err := reader.validateRoutePlanningState(ctx, p.routeKey, observation.planning); err != nil {
				return Observation{}, err
			}
		}
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
	var journalErr, identityErr error
	var identity programIdentityObservation
	var reads sync.WaitGroup
	reads.Add(2)
	go func() { defer reads.Done(); journalErr = p.mergeJournal(ctx, observation) }()
	go func() { defer reads.Done(); identity, identityErr = p.identity(ctx) }()
	reads.Wait()
	if journalErr != nil {
		return journalErr
	}
	if identityErr != nil {
		return identityErr
	}
	applyProgramIdentityObservation(observation, identity)
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
	var required bool
	var journal ReconciledBridgeJournalState
	var requiredErr, journalErr error
	var reads sync.WaitGroup
	reads.Add(2)
	go func() { defer reads.Done(); required, requiredErr = p.journal.PostMutationNAVRequired(ctx, p.routeKey) }()
	go func() { defer reads.Done(); journal, journalErr = p.journal.ReconciledBridgeJournal(ctx, p.routeKey) }()
	reads.Wait()
	if requiredErr != nil {
		return requiredErr
	}
	if journalErr != nil {
		return journalErr
	}
	observation.Snapshot.PostMutationNAVRequired = required
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
	return nil
}

func productionTickRuntime(database *Database, rpc *chain.Client, view *View, manifest RouteManifest, credentials Credentials) tickRuntime {
	state := productionObserveState{
		manifest: manifest, routeKey: productionRouteKey,
		journal: database,
		batch: func(ctx context.Context) (Observation, error) {
			refreshObservationLagSlots(ctx, rpc)
			planning, err := database.readRoutePlanningStateOnManifest(ctx, manifest, productionRouteKey, true)
			if err != nil {
				return Observation{}, err
			}
			observation, _, err := ObserveConfirmedRouteSnapshot(ctx, rpc, view, manifest, planning.landedSlot, planning.inputs)
			if err != nil {
				return Observation{}, err
			}
			observation.planning = planning
			return observation, nil
		},
		identity: newProgramIdentityWatcher(viewProgramIdentity(view)).observe,
	}
	return tickRuntime{
		withdrawalHealth: database.RecordWithdrawalHealth,
		observeWithdrawalHealth: func(ctx context.Context) (Observation, error) {
			// Latches and recovery skip normal observation. Read for display only;
			// do not write position state or feed this back into execution.
			o, err := state.batch(ctx)
			if err == nil {
				err = state.enrich(ctx, &o)
			}
			return o, err
		},
		watchRoom: view.WatchRoom,
		prepareInitialization: func(ctx context.Context, m RouteManifest, d Decision) (Observation, KaminoInitializationRequest, error) {
			return prepareKaminoInitialization(ctx, rpc, view, m, d, state.observe)
		},
		buildInitialization: func(ctx context.Context, id string, r KaminoInitializationRequest) error {
			return BuildSimulateAndPersistKaminoInitialization(ctx, database, rpc, view, id, manifest, r, credentials)
		},
		loadNonterminal: func(ctx context.Context, routeKey string) (*PersistedOperation, error) {
			return database.LoadNonterminal(ctx, routeKey)
		},
		advance: func(ctx context.Context, operation PersistedOperation) error {
			return advanceNonterminalWithManifest(ctx, manifest, database, rpc, view, operation)
		},
		observe:                          state.observe,
		loadLatch:                        database.ManualRecoveryLatch,
		recordManualRecovery:             database.RecordManualRecovery,
		recordManualRecoveryAtGeneration: database.RecordManualRecoveryAtGeneration,
		prepareBridge: func(ctx context.Context, manifest RouteManifest, decision Decision, observation Observation) (Observation, BridgeExecutionEvidence, error) {
			return prepareBridgeFromTickObservation(ctx, rpc, view, manifest, decision, observation)
		},
		prepareKamino: func(ctx context.Context, manifest RouteManifest, decision Decision, observation Observation) (Observation, KaminoExecutionEvidence, error) {
			return prepareKaminoFromTickObservation(ctx, rpc, view, manifest, decision, observation)
		},
		prepareJupiter: func(ctx context.Context, manifest RouteManifest, decision Decision, observation Observation) (Observation, JupiterExecutionEvidence, error) {
			return prepareJupiterFromTickObservation(ctx, rpc, manifest, decision, productionJupiter, observation)
		},
		prepareHastra: func(ctx context.Context, decision Decision, observation Observation) (Observation, HastraExecutionEvidence, error) {
			return prepareHastraFromTickObservation(ctx, rpc, view, decision, observation)
		},
		prepareStep: func(ctx context.Context, manifest RouteManifest, decision Decision, observation Observation) (Observation, StepExecutionEvidence, error) {
			return prepareStepFromTickObservation(ctx, rpc, view, manifest, productionJupiter, decision, observation)
		},
		recordDecision: func(ctx context.Context, routeKey string, observation Observation, decision Decision, manifestSHA256 string) (DecisionRecord, error) {
			return database.RecordDecisionOnManifest(ctx, manifest, routeKey, observation, decision, manifestSHA256)
		},
		recordBudgetHold: database.RecordPhase3BudgetHold,
		bind: func(ctx context.Context, operationID string, observation Observation, decision Decision, request any, effects ExpectedEffects) error {
			return database.bindOperation(ctx, rpc, view, manifest, operationID, observation, decision, request, effects)
		},
		buildBridge: func(ctx context.Context, operationID string, evidence BridgeExecutionEvidence) error {
			return BuildSimulateAndPersistBridge(ctx, database, rpc, view, operationID, evidence, credentials)
		},
		buildKamino: func(ctx context.Context, operationID string, evidence KaminoExecutionEvidence) error {
			return BuildSimulateAndPersistKamino(ctx, database, rpc, view, operationID, evidence, credentials)
		},
		buildJupiter: func(ctx context.Context, operationID string, evidence JupiterExecutionEvidence) error {
			return BuildSimulateAndPersistJupiter(ctx, database, rpc, view, operationID, evidence, credentials)
		},
		buildHastra: func(ctx context.Context, operationID string, evidence HastraExecutionEvidence) error {
			return BuildSimulateAndPersistHastra(ctx, database, rpc, view, operationID, evidence, credentials)
		},
		buildStep: func(ctx context.Context, operationID string, evidence StepExecutionEvidence) error {
			return BuildSimulateAndPersistStep(ctx, database, rpc, view, operationID, evidence, credentials)
		},
	}
}

func confirmedObservationUnavailable(err error) error {
	if err == nil || errors.Is(err, errConfirmedObservationUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", errConfirmedObservationUnavailable, err)
}

// NewWorker constructs the single serialized lifecycle worker. The signing
// capability is injected, validated here, and never derivable from a
// decision, observation, or recovery input.
func NewWorker(database *Database, rpc *chain.Client, view *View, config Config, credentials Credentials) (*Worker, error) {
	if database == nil || database.pool == nil || rpc == nil || view == nil || config.validateLease() != nil {
		return nil, fmt.Errorf("invalid concrete worker configuration")
	}
	key, err := credentials.signer()
	if err != nil {
		return nil, err
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return nil, err
	}
	return &Worker{wake: make(chan struct{}, 1), routeKey: productionRouteKey, interval: config.PollInterval, manifest: manifest, runtime: productionTickRuntime(database, rpc, view, manifest, Credentials{PolicyKey: key})}, nil
}

func (w *Worker) Tick(ctx context.Context) (tickErr error) {
	if w == nil || w.routeKey != productionRouteKey {
		return fmt.Errorf("worker route is not the fixed production route")
	}
	var healthObservation Observation
	var healthDecision Decision
	defer func() { w.publishWithdrawalHealth(ctx, healthObservation, healthDecision, tickErr) }()
	// The manual recovery latch is re-read before anything else in the tick, so
	// a healthy batch, a restart, or a new worker state object can never resume
	// execution behind a durable stop.
	if w.runtime.loadLatch != nil {
		latch, latched, err := w.runtime.loadLatch(ctx, w.routeKey)
		if err != nil {
			return err
		}
		backyardEvents.noteLatch(latched, latch.Reason)
		if latched {
			healthDecision = Decision{Action: HoldManualRecovery}
			return w.recordLatchedHold(ctx, latch)
		}
	}
	operation, err := w.runtime.loadNonterminal(ctx, w.routeKey)
	if err != nil {
		return err
	}
	backyardEvents.inflight(operation != nil)
	if operation != nil {
		healthDecision = Decision{Action: RecoverTransaction}
		backyardEvents.noteAction(operation.Decision.Action)
		if err := w.runtime.advance(ctx, *operation); err != nil {
			if operation.BroadcastIntentRecorded || !preBroadcastStatus(operation.Status) {
				return &afterBroadcastError{err}
			}
			return err
		}
		return nil
	}
	tickStart := time.Now()
	observation, err := w.runtime.observe(ctx)
	if err != nil {
		return err
	}
	healthObservation = observation
	backyardEvents.noteSnapshot(observation.Snapshot)
	backyardEvents.leverageObserved(heldLaneLeverage(observation))
	if w.runtime.watchRoom != nil {
		w.runtime.watchRoom(leverageRoomWatches(observation), w.roomObserved)
	}
	decision := Decide(observation.Snapshot)
	healthDecision = decision
	w.noRoom.note(time.Now(), observation.Snapshot, decision)
	// Decide maps every observation-level hold to one generic reason. The
	// observer records the audited Kamino health reason on the snapshot, so it
	// is carried into the durable decision instead of being discarded.
	if decision.Action == HoldManualRecovery && observation.Snapshot.ManualReason != "" {
		decision.Reason = observation.Snapshot.ManualReason
	}
	if err := decision.Validate(); err != nil {
		return err
	}
	healthDecision = decision
	backyardEvents.noteAction(decision.Action)
	if decision.Action == Hold || decision.Action == HoldManualRecovery {
		healthDecision = decision
		if decision.Action == HoldManualRecovery {
			return w.recordManualRecoveryDecision(ctx, observation, decision)
		}
		if _, err := w.runtime.recordDecision(ctx, w.routeKey, observation, decision, w.manifest.SHA256); err != nil {
			return err
		}
		return nil
	}
	if blocker := w.manifest.executionBlocker(); blocker != nil {
		return blocker
	}
	logStage("observe_decide", tickStart)
	var initializationRequest KaminoInitializationRequest
	var bridgeEvidence BridgeExecutionEvidence
	var kaminoEvidence KaminoExecutionEvidence
	var jupiterEvidence JupiterExecutionEvidence
	var hastraEvidence HastraExecutionEvidence
	var stepEvidence StepExecutionEvidence
	preparedDecision := decision
	executionDecision, err := fixedRouteAction(decision.Action, decision.StrategyKey)
	if err != nil {
		return err
	}
	wireDecision := decision
	wireDecision.Action = executionDecision
	// A conversion executes through Hastra or Jupiter by its lane and action
	// (hastraConversionLeg); every case below splits on it.
	hastraConversion := conversionVenueHastra(wireDecision)
	// A leverage move executes as one atomic step (step.go).
	_, atomic := stepDecision(observation.Snapshot, wireDecision)
	decided := observation.Snapshot
	switch executionDecision {
	case InitializeKaminoObligation:
		if w.runtime.prepareInitialization == nil {
			return budgetHold("initializer_preparation_unavailable")
		}
		observation, initializationRequest, err = w.runtime.prepareInitialization(ctx, w.manifest, wireDecision)
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		observation, bridgeEvidence, err = w.runtime.prepareBridge(ctx, w.manifest, wireDecision, observation)
	case OpenPrimeUSDCStep, DeleverPrimeUSDCStep, OpenRouteStep, DeleverRouteStep:
		if atomic {
			if w.runtime.prepareStep == nil {
				return budgetHold("step_preparation_unavailable")
			}
			observation, stepEvidence, err = w.runtime.prepareStep(ctx, w.manifest, wireDecision, observation)
		} else {
			observation, kaminoEvidence, err = w.runtime.prepareKamino(ctx, w.manifest, wireDecision, observation)
		}
	case SwapUSDCToPrimeStep, SwapPrimeToUSDCStep, SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep:
		if hastraConversion {
			if w.runtime.prepareHastra == nil {
				return budgetHold("hastra_preparation_unavailable")
			}
			observation, hastraEvidence, err = w.runtime.prepareHastra(ctx, wireDecision, observation)
		} else {
			observation, jupiterEvidence, err = w.runtime.prepareJupiter(ctx, w.manifest, wireDecision, observation)
		}
	default:
		return fmt.Errorf("action %s is not dispatchable", decision.Action)
	}
	if observation.Validate() == nil && observation.Snapshot.Fresh && observation.Snapshot.ManualReason == "" && !observation.Snapshot.StrategyReceiptIntegrityFault {
		healthObservation = observation
	}
	// Construction refreshes the same confirmed inputs that will be sent. A
	// refreshed manual-recovery decision is a new durable stop, not ordinary
	// decision drift, and must be recorded before the tick returns.
	if ok, persistErr := w.persistRefreshedManualRecovery(ctx, observation); ok {
		healthDecision = Decision{Action: HoldManualRecovery}
		if persistErr != nil {
			return persistErr
		}
		return nil
	}
	logStage("prepare", tickStart)
	if err != nil {
		// A step the chain has no room for (no withdraw room) is the
		// target's no_room, counted like Decide's.
		var hold *BudgetHold
		if errors.As(err, &hold) && hold.Reason == holdNoRoom {
			w.noRoom.note(time.Now(), decided, Decision{Action: Hold, Reason: holdNoRoom, StrategyKey: decision.StrategyKey})
		}
		return err
	}
	decision = Decide(observation.Snapshot)
	healthDecision = decision
	if err := decision.Validate(); err != nil {
		return err
	}
	// Drift is a new state: nothing is recorded yet, so retry the leg on the
	// next tick instead of stopping the worker.
	if !decisionsEqual(decision, preparedDecision) {
		return confirmedObservationUnavailable(fmt.Errorf("prepared evidence does not match the refreshed decision: decided %s/%s/%d, refreshed %s/%s/%d",
			preparedDecision.Action, preparedDecision.Reason, preparedDecision.AmountRaw, decision.Action, decision.Reason, decision.AmountRaw))
	}
	record, err := w.runtime.recordDecision(ctx, w.routeKey, observation, decision, w.manifest.SHA256)
	if err != nil {
		return err
	}
	logStage("record_decision", tickStart)
	if record.Status != Decided || record.OperationID == "" {
		return fmt.Errorf("actionable decision was not durably recorded as decided")
	}
	var request any
	var effects ExpectedEffects
	switch executionDecision {
	case InitializeKaminoObligation:
		request, effects = initializationRequest, kaminoInitializationEffects(initializationRequest)
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		request, effects = bridgeEvidence.Request, bridgeEvidence.ExpectedEffects
	case OpenPrimeUSDCStep, DeleverPrimeUSDCStep, OpenRouteStep, DeleverRouteStep:
		request, effects = kaminoEvidence.Request, kaminoEvidence.ExpectedEffects
		if atomic {
			request, effects = stepEvidence.Request, stepEvidence.ExpectedEffects
		}
	case SwapUSDCToPrimeStep, SwapPrimeToUSDCStep, SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep:
		request, effects = jupiterEvidence.Request, jupiterEvidence.ExpectedEffects
		if hastraConversion {
			request, effects = hastraEvidence.Request, hastraEvidence.ExpectedEffects
		}
	default:
		return fmt.Errorf("prepared evidence no longer matches an actionable decision")
	}
	if w.runtime.bind == nil {
		err = budgetHold("bind_unavailable")
	} else {
		err = w.runtime.bind(ctx, record.OperationID, observation, decision, request, effects)
	}
	logStage("bind", tickStart)
	if err == nil {
		switch executionDecision {
		case InitializeKaminoObligation:
			err = w.runtime.buildInitialization(ctx, record.OperationID, initializationRequest)
		case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
			err = w.runtime.buildBridge(ctx, record.OperationID, bridgeEvidence)
		case OpenPrimeUSDCStep, DeleverPrimeUSDCStep, OpenRouteStep, DeleverRouteStep:
			if atomic {
				if w.runtime.buildStep == nil {
					err = budgetHold("step_build_unavailable")
				} else {
					err = w.runtime.buildStep(ctx, record.OperationID, stepEvidence)
				}
			} else {
				err = w.runtime.buildKamino(ctx, record.OperationID, kaminoEvidence)
			}
		default:
			if hastraConversion {
				if w.runtime.buildHastra == nil {
					err = budgetHold("hastra_build_unavailable")
				} else {
					err = w.runtime.buildHastra(ctx, record.OperationID, hastraEvidence)
				}
			} else {
				err = w.runtime.buildJupiter(ctx, record.OperationID, jupiterEvidence)
			}
		}
	}
	logStage("build_sign", tickStart)
	if err != nil {
		return w.journalTickError(ctx, record.OperationID, err)
	}
	// A newly signed report has only 32 slots to land. Persisting the wire is
	// the recovery boundary; waiting a polling interval adds no safety and
	// can expire it. Reload durable state and use the normal send/recovery
	// path immediately, with the same fresh manual-stop check as a new tick.
	if w.runtime.loadLatch != nil {
		latch, latched, err := w.runtime.loadLatch(ctx, w.routeKey)
		if err != nil {
			return err
		}
		if latched {
			healthDecision = Decision{Action: HoldManualRecovery}
			return w.recordLatchedHold(ctx, latch)
		}
	}
	pending, err := w.runtime.loadNonterminal(ctx, w.routeKey)
	if err != nil {
		return err
	}
	if pending != nil {
		if pending.ID != record.OperationID {
			return fmt.Errorf("operation changed after durable build")
		}
		if pending.Status == Signed {
			defer logStage("final_check_send", tickStart)
			return w.runtime.advance(ctx, *pending)
		}
	}
	return nil
}

// journalTickError journals a pre-send hold for the tick's operation once.
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
	// An endpoint that could not answer says nothing about the operation: it
	// stays decided and the next tick builds it again.
	if errors.Is(hold, chain.ErrUnavailable) {
		return confirmedObservationUnavailable(err)
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
	if w.runtime.recordManualRecoveryAtGeneration != nil {
		if _, err := w.runtime.recordManualRecoveryAtGeneration(ctx, w.routeKey, observation, decision, w.manifest.SHA256, latch.Generation); err != nil {
			if errors.Is(err, errManualRecoveryLatchGenerationChanged) && w.runtime.loadLatch != nil {
				_, _, rereadErr := w.runtime.loadLatch(ctx, w.routeKey)
				return rereadErr
			}
			return err
		}
		return nil
	}
	return w.recordManualRecoveryDecision(ctx, observation, decision)
}

func (w *Worker) recordManualRecoveryDecision(ctx context.Context, observation Observation, decision Decision) error {
	if w.runtime.recordManualRecovery == nil {
		return fmt.Errorf("manual recovery persistence runtime is unavailable")
	}
	if _, err := w.runtime.recordManualRecovery(ctx, w.routeKey, observation, decision, w.manifest.SHA256); err != nil {
		return err
	}
	// A "latched:" reason is the per-tick re-record of an existing stop.
	if !strings.HasPrefix(decision.Reason, "latched:") {
		backyardEvents.latched(decision.Reason)
	}
	return nil
}

func (w *Worker) persistRefreshedManualRecovery(ctx context.Context, observation Observation) (bool, error) {
	if observation.Snapshot.ObservationID == "" || observation.Snapshot.Slot <= 0 {
		return false, nil
	}
	decision := Decide(observation.Snapshot)
	if decision.Action != HoldManualRecovery {
		return false, nil
	}
	return true, w.recordManualRecoveryDecision(ctx, observation, decision)
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

// afterBroadcastError marks a tick error from advancing an operation whose
// broadcast intent is already durable. Its holds are never pre-send retries.
type afterBroadcastError struct{ err error }

func (e *afterBroadcastError) Error() string { return e.err.Error() }
func (e *afterBroadcastError) Unwrap() error { return e.err }

// preBroadcastStatus reports states in which lifecycle recovery can still
// only refuse or retire the wire: a send first commits broadcast_intent.
func preBroadcastStatus(status OperationStatus) bool {
	return status == Decided || status == Built || status == Simulated || status == Signed
}

// isPreSendHold reports a tick error made only of holds raised
// before anything was broadcast. The next tick re-observes the chain and
// re-derives the whole leg, exactly what a process restart would do, without
// the Render restart (13 tries, 10 restarts in the 09-26 AUTO entry; the
// per-reason list it replaces grew with every move). A hold joined with any
// other fault (a store or wire error) still stops the worker,
// and so does any hold from an operation whose broadcast intent is recorded.
func isPreSendHold(err error) bool {
	var hold *BudgetHold
	var sent *afterBroadcastError
	if !errors.As(err, &hold) || errors.As(err, &sent) {
		return false
	}
	switch unwrappable := err.(type) {
	case *BudgetHold:
		return true
	case interface{ Unwrap() []error }:
		for _, member := range unwrappable.Unwrap() {
			if !isPreSendHold(member) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return isPreSendHold(unwrappable.Unwrap())
	default:
		return true
	}
}

// notifySelectorCommit requests another serialized tick; never execute a
// transaction from the collector. A queued wake survives an active tick.
func (w *Worker) notifySelectorCommit(action string) {
	if action != "ENTER" && action != "CANARY_ENTER" {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) runTicks(ctx context.Context, leaseErrors <-chan error, tick func(context.Context) error) error {
	for {
		err := tick(ctx)
		if err != nil {
			select {
			case leaseErr := <-leaseErrors:
				backyardEvents.tickResult(leaseErr, true)
				return leaseErr
			default:
			}
			// Confirmed-observation gaps and pre-send holds (the journaled
			// spending-limit refusal among them) skip this tick's leg and retry
			// on the next interval; anything else - including a hold joined
			// with a store error - is a process fault and stops the worker.
			if !errors.Is(err, errConfirmedObservationUnavailable) && !isPreSendHold(err) {
				// A SIGTERM cancellation is a normal stop, not an alert.
				if ctx.Err() == nil {
					backyardEvents.tickResult(err, true)
				}
				return err
			}
		}
		backyardEvents.tickResult(err, false)
		timer := time.NewTimer(w.interval)
		select {
		case err := <-leaseErrors:
			timer.Stop()
			backyardEvents.tickResult(err, true)
			return err
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-w.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Run acquires the route fence before the first observation, refreshes it on a
// bounded cadence, and cancels the active tick immediately if ownership is
// lost. Release is compare-and-clear on the exact fencing token, so a stale
// process can never clear its successor's lease.
func (w *Worker) Run(ctx context.Context, leases routeLeaser, owner string, config Config) (runErr error) {
	if w == nil || leases == nil || !ValidLeaseOwner(owner) || config.validateLease() != nil {
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
