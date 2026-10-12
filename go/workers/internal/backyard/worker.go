package backyard

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

const productionRouteKey = "rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"

var errConfirmedObservationUnavailable = errors.New("confirmed route observation is temporarily unavailable")

type Worker struct {
	wake     chan struct{}
	routeKey string
	interval time.Duration
	manifest RouteManifest
	runtime  tickRuntime
	// holds reports why a tick did not move the vault: a gauge on /metrics
	// and a log line when it changes; a hold is never a journal row.
	holds holdLog
}

type tickRuntime struct {
	withdrawalHealth        func(context.Context, WithdrawalHealth) error
	observeWithdrawalHealth func(context.Context) (Observation, error)
	prepareInitialization   func(context.Context, RouteManifest, Decision, Observation) (Observation, KaminoInitializationRequest, error)
	loadNonterminal         func(context.Context, string) (*PersistedOperation, error)
	advance                 func(context.Context, PersistedOperation) error
	observe                 func(context.Context) (Observation, error)
	loadLatch               func(context.Context, string) (ManualRecoveryLatch, bool, error)
	// latch writes the operator's stop for a HOLD_MANUAL_RECOVERY decision.
	latch          func(context.Context, string, ManualRecoveryLatch) error
	prepareBridge  func(context.Context, RouteManifest, Decision, Observation) (Observation, BridgeExecutionEvidence, error)
	prepareKamino  func(context.Context, RouteManifest, Decision, Observation) (Observation, KaminoExecutionEvidence, error)
	prepareJupiter func(context.Context, RouteManifest, Decision, Observation) (Observation, JupiterExecutionEvidence, error)
	prepareHastra  func(context.Context, Decision, Observation) (Observation, HastraExecutionEvidence, error)
	// prepareStep builds an atomic step (stepDecision) from the tick's batch.
	prepareStep    func(context.Context, RouteManifest, Decision, Observation) (Observation, StepExecutionEvidence, error)
	recordDecision func(context.Context, string, Observation, Decision, string) (DecisionRecord, error)
	// build signs the decided operation's request, simulates that exact wire
	// and persists it with its expected effects before anything sends it.
	build            func(context.Context, string, any, ExpectedEffects) error
	recordBudgetHold func(context.Context, string, *BudgetHold) error
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
			planning, err := database.readRoutePlanningState(ctx, productionRouteKey)
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
		prepareInitialization: func(ctx context.Context, m RouteManifest, d Decision, o Observation) (Observation, KaminoInitializationRequest, error) {
			return prepareKaminoInitialization(ctx, rpc, m, d, o)
		},
		loadNonterminal: func(ctx context.Context, routeKey string) (*PersistedOperation, error) {
			return database.LoadNonterminal(ctx, routeKey)
		},
		advance: func(ctx context.Context, operation PersistedOperation) error {
			return advanceNonterminalWithManifest(ctx, manifest, database, rpc, operation)
		},
		observe:   state.observe,
		loadLatch: database.ManualRecoveryLatch,
		latch:     database.LatchManualRecovery,
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
		build: func(ctx context.Context, operationID string, request any, effects ExpectedEffects) error {
			return buildSimulatePersist(ctx, database, rpc, manifest, operationID, request, effects, credentials)
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
	if database == nil || database.pool == nil || rpc == nil || view == nil || config.PollInterval <= 0 {
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
	// The manual recovery latch is read before anything else in the tick, so
	// a healthy batch or a restart can never resume execution behind the
	// operator's stop. A latched tick writes nothing.
	if w.runtime.loadLatch != nil {
		latch, latched, err := w.runtime.loadLatch(ctx, w.routeKey)
		if err != nil {
			return err
		}
		backyardEvents.noteLatch(latched, latch.Reason)
		if latched {
			healthDecision = Decision{Action: HoldManualRecovery}
			w.holds.note(Snapshot{}, "", holdManualRecovery, latch.Reason)
			return nil
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
	switch decision.Action {
	case Hold:
		w.holds.note(observation.Snapshot, decision.StrategyKey, decision.Reason, "")
		return nil
	case HoldManualRecovery:
		w.holds.note(observation.Snapshot, decision.StrategyKey, holdManualRecovery, decision.Reason)
		return w.latchManualRecovery(ctx, observation, decision)
	}
	w.holds.note(observation.Snapshot, decision.StrategyKey, "", "")
	if blocker := w.manifest.executionBlocker(); blocker != nil {
		return blocker
	}
	logStage("observe_decide", tickStart)
	executionDecision, err := fixedRouteAction(decision.Action, decision.StrategyKey)
	if err != nil {
		return err
	}
	request, effects, prepared, err := w.prepare(ctx, decision, executionDecision, observation)
	if prepared.Validate() == nil && prepared.Snapshot.Fresh && prepared.Snapshot.ManualReason == "" && !prepared.Snapshot.StrategyReceiptIntegrityFault {
		healthObservation = prepared
	}
	logStage("prepare", tickStart)
	if err != nil {
		// A step the chain has no room for (no withdraw room) is the
		// target's no_room, counted like Decide's.
		var hold *BudgetHold
		if errors.As(err, &hold) && hold.Reason == holdNoRoom {
			w.holds.note(observation.Snapshot, decision.StrategyKey, holdNoRoom, "")
		}
		return err
	}
	record, err := w.runtime.recordDecision(ctx, w.routeKey, observation, decision, w.manifest.SHA256)
	if err != nil {
		return err
	}
	if record.Status != Decided || record.OperationID == "" {
		return fmt.Errorf("actionable decision was not durably recorded as decided")
	}
	if w.runtime.build == nil {
		err = budgetHold("build_unavailable")
	} else {
		err = w.runtime.build(ctx, record.OperationID, request, effects)
	}
	logStage("build_sign", tickStart)
	if err != nil {
		return w.journalTickError(ctx, record.OperationID, err)
	}
	// The signed wire is persisted: send it now through the same path a
	// restart takes. A newly signed report has only 32 slots to land.
	pending, err := w.runtime.loadNonterminal(ctx, w.routeKey)
	if err != nil || pending == nil {
		return err
	}
	if pending.ID != record.OperationID {
		return fmt.Errorf("operation changed after durable build")
	}
	if pending.Status != Signed {
		return nil
	}
	defer logStage("send", tickStart)
	return w.runtime.advance(ctx, *pending)
}

// prepare builds the decided action's request from the tick's batch: the one
// transaction this tick will sign. It never decides again.
func (w *Worker) prepare(ctx context.Context, decision Decision, executionDecision Action, observation Observation) (any, ExpectedEffects, Observation, error) {
	wireDecision := decision
	wireDecision.Action = executionDecision
	switch executionDecision {
	case InitializeKaminoObligation:
		if w.runtime.prepareInitialization == nil {
			return nil, ExpectedEffects{}, observation, budgetHold("initializer_preparation_unavailable")
		}
		prepared, r, err := w.runtime.prepareInitialization(ctx, w.manifest, wireDecision, observation)
		return r, kaminoInitializationEffects(r), prepared, err
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		prepared, e, err := w.runtime.prepareBridge(ctx, w.manifest, wireDecision, observation)
		return e.Request, e.ExpectedEffects, prepared, err
	case OpenPrimeUSDCStep, DeleverPrimeUSDCStep, OpenRouteStep, DeleverRouteStep:
		// A leverage move executes as one atomic step (step.go).
		if _, atomic := stepDecision(observation.Snapshot, wireDecision); atomic {
			if w.runtime.prepareStep == nil {
				return nil, ExpectedEffects{}, observation, budgetHold("step_preparation_unavailable")
			}
			prepared, e, err := w.runtime.prepareStep(ctx, w.manifest, wireDecision, observation)
			return e.Request, e.ExpectedEffects, prepared, err
		}
		prepared, e, err := w.runtime.prepareKamino(ctx, w.manifest, wireDecision, observation)
		return e.Request, e.ExpectedEffects, prepared, err
	case SwapUSDCToPrimeStep, SwapPrimeToUSDCStep, SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep:
		// A conversion executes through Hastra or Jupiter by its lane and
		// action (hastraConversionLeg).
		if conversionVenueHastra(wireDecision) {
			if w.runtime.prepareHastra == nil {
				return nil, ExpectedEffects{}, observation, budgetHold("hastra_preparation_unavailable")
			}
			prepared, e, err := w.runtime.prepareHastra(ctx, wireDecision, observation)
			return e.Request, e.ExpectedEffects, prepared, err
		}
		prepared, e, err := w.runtime.prepareJupiter(ctx, w.manifest, wireDecision, observation)
		return e.Request, e.ExpectedEffects, prepared, err
	}
	return nil, ExpectedEffects{}, observation, fmt.Errorf("action %s is not dispatchable", decision.Action)
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

// latchManualRecovery writes the operator's stop for a manual recovery
// decision; the next tick reads it and does nothing until clear-hold.
func (w *Worker) latchManualRecovery(ctx context.Context, observation Observation, decision Decision) error {
	if w.runtime.latch == nil {
		return fmt.Errorf("manual recovery latch runtime is unavailable")
	}
	latch := ManualRecoveryLatch{Reason: decision.Reason, ObservationID: observation.Snapshot.ObservationID, ObservationSlot: observation.Snapshot.Slot}
	if err := w.runtime.latch(ctx, w.routeKey, latch); err != nil {
		return err
	}
	backyardEvents.latched(decision.Reason)
	return nil
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

func (w *Worker) runTicks(ctx context.Context, tick func(context.Context) error) error {
	for {
		err := tick(ctx)
		if err != nil {
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
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-w.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Run serializes lifecycle ticks until ctx ends. The process holds
// Backyard's one-writer lock (the family advisory lock) for its whole life;
// losing it cancels ctx.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return fmt.Errorf("invalid worker runtime")
	}
	return w.runTicks(ctx, w.Tick)
}
