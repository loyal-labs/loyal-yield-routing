package backyard

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

var errConfirmedObservationUnavailable = errors.New("confirmed route observation is temporarily unavailable")

type Worker struct {
	wake     chan struct{}
	routeKey string
	interval time.Duration
	manifest RouteManifest
	runtime  tickRuntime
	events   *events
	holds    holdLog

	navReportKnown    bool
	navReportSeededAt time.Time
}

type tickRuntime struct {
	withdrawalHealth        func(context.Context, withdrawalAssessment) error
	observeWithdrawalHealth func(context.Context) (Observation, error)
	persistedNAVReport      func(context.Context, string) (navReport, bool, error)
	prepareInitialization   func(context.Context, RouteManifest, Decision, Observation) (Observation, KaminoInitializationRequest, error)
	loadNonterminal         func(context.Context, string) (*PersistedOperation, error)
	advance                 func(context.Context, PersistedOperation) error
	observe                 func(context.Context) (Observation, error)
	loadLatch               func(context.Context, string) (ManualRecoveryLatch, bool, error)
	latch                   func(context.Context, string, ManualRecoveryLatch) error
	prepareBridge           func(context.Context, RouteManifest, Decision, Observation) (Observation, BridgeExecutionEvidence, error)
	prepareKamino           func(context.Context, RouteManifest, Decision, Observation) (Observation, KaminoExecutionEvidence, error)
	prepareJupiter          func(context.Context, RouteManifest, Decision, Observation) (Observation, JupiterExecutionEvidence, error)
	prepareHastra           func(context.Context, RouteManifest, Decision, Observation) (Observation, HastraExecutionEvidence, error)
	prepareStep             func(context.Context, RouteManifest, Decision, Observation) (Observation, StepExecutionEvidence, error)
	recordDecision          func(context.Context, string, Observation, Decision, string) (DecisionRecord, error)
	build                   func(context.Context, string, any, ExpectedEffects) error
	recordPreSendHold       func(context.Context, string, *PreSendHold) error
	watchRoom               func([]roomWatch, func(roomUpdate))
}

type journalReader interface {
	PostMutationNAVRequired(ctx context.Context, routeKey string) (bool, error)
	ReconciledBridgeJournal(ctx context.Context, routeKey string) (ReconciledBridgeJournalState, error)
}

type productionJournal interface {
	journalReader
	RecordPositionSnapshot(ctx context.Context, routeKey string, observation Observation) error
}

type productionObserveState struct {
	manifest RouteManifest
	routeKey string
	journal  productionJournal
	batch    func(context.Context) (Observation, error)
	identity func(context.Context) (programIdentityObservation, error)
}

func (p productionObserveState) observe(ctx context.Context) (Observation, error) {
	observation, err := p.batch(ctx)
	if err != nil {
		return Observation{}, err
	}
	if err := enrichObservation(ctx, p.journal, p.routeKey, p.identity, &observation); err != nil {
		return Observation{}, err
	}
	if !observation.Snapshot.valued() {
		return observation, nil
	}
	if err := p.journal.RecordPositionSnapshot(ctx, p.routeKey, observation); err != nil {
		return Observation{}, err
	}
	return observation, nil
}

func enrichObservation(ctx context.Context, journal journalReader, routeKey string, identity func(context.Context) (programIdentityObservation, error), observation *Observation) error {
	if observation != nil && observation.Snapshot.Trustful {
		return nil
	}
	var journalErr, identityErr error
	var observed programIdentityObservation
	var reads sync.WaitGroup
	reads.Add(2)
	go func() { defer reads.Done(); journalErr = mergeJournal(ctx, journal, routeKey, observation) }()
	go func() { defer reads.Done(); observed, identityErr = identity(ctx) }()
	reads.Wait()
	if journalErr != nil {
		return journalErr
	}
	if identityErr != nil {
		return identityErr
	}
	applyProgramIdentityObservation(observation, observed)
	return nil
}

func mergeJournal(ctx context.Context, journal journalReader, routeKey string, observation *Observation) error {
	if observation == nil {
		return fmt.Errorf("production observation is nil")
	}
	var required bool
	var state ReconciledBridgeJournalState
	var requiredErr, journalErr error
	var reads sync.WaitGroup
	reads.Add(2)
	go func() { defer reads.Done(); required, requiredErr = journal.PostMutationNAVRequired(ctx, routeKey) }()
	go func() { defer reads.Done(); state, journalErr = journal.ReconciledBridgeJournal(ctx, routeKey) }()
	reads.Wait()
	if requiredErr != nil {
		return requiredErr
	}
	if journalErr != nil {
		return journalErr
	}
	observation.Snapshot.PostMutationNAVRequired = required
	observation.Snapshot.JournalSequenceKnown = state.TicketSequenceKnown
	observation.Snapshot.JournalReconciledSequenceRaw = state.TicketSequenceRaw
	observation.Snapshot.JournalArmedNAVKnown = state.ArmedNAVKnown
	observation.Snapshot.JournalArmedNAVRaw = state.ArmedNAVRaw
	observation.Snapshot.JournalArmedNAVReturnDataMissing = state.ArmedNAVReturnDataMissing
	observation.Snapshot.JournalArmedNAVMalformed = state.ArmedNAVMalformed
	observation.Snapshot.StagedAmountRaw = state.StagedAmountRaw
	observation.Snapshot.StagedAmountKnown = state.StagedAmountKnown
	observation.Snapshot.StageTransient = state.StageAfterTicket
	observation.Snapshot.CapitalMutated = state.MutationAfterReport
	return nil
}

func productionTickRuntime(database *Database, rpc *chain.Client, view *View, jupiterClient *jupiter.Client, vault *vaultState, manifest RouteManifest, credentials Credentials) tickRuntime {
	v := manifest.vault
	state := productionObserveState{
		manifest: manifest, routeKey: v.RouteKey,
		journal: database,
		batch: func(ctx context.Context) (Observation, error) {
			refreshObservationLagSlots(ctx, rpc)
			planning, err := database.readRoutePlanningState(ctx, v.RouteKey)
			if err != nil {
				return Observation{}, err
			}
			observation, _, err := ObserveConfirmedRouteSnapshot(ctx, rpc, view, manifest, vault, planning.landedSlot, planning.inputs)
			if err != nil {
				return Observation{}, err
			}
			observation.planning = planning
			return observation, nil
		},
		identity: newProgramIdentityWatcher(viewProgramIdentity(view)).observe,
	}
	return tickRuntime{
		withdrawalHealth:   database.RecordWithdrawalHealth,
		persistedNAVReport: database.ObservedNAVReport,
		observeWithdrawalHealth: func(ctx context.Context) (Observation, error) {
			o, err := state.batch(ctx)
			if err == nil {
				err = enrichObservation(ctx, state.journal, state.routeKey, state.identity, &o)
			}
			return o, err
		},
		watchRoom: func(watches []roomWatch, on func(roomUpdate)) { view.WatchRoom(v, watches, on) },
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
			return prepareJupiterFromTickObservation(ctx, rpc, manifest, decision, jupiterClient, observation)
		},
		prepareHastra: func(ctx context.Context, manifest RouteManifest, decision Decision, observation Observation) (Observation, HastraExecutionEvidence, error) {
			return prepareHastraFromTickObservation(ctx, rpc, view, manifest, decision, observation)
		},
		prepareStep: func(ctx context.Context, manifest RouteManifest, decision Decision, observation Observation) (Observation, StepExecutionEvidence, error) {
			return prepareStepFromTickObservation(ctx, rpc, view, manifest, jupiterClient, decision, observation)
		},
		recordDecision: func(ctx context.Context, routeKey string, observation Observation, decision Decision, manifestSHA256 string) (DecisionRecord, error) {
			return database.RecordDecisionOnManifest(ctx, manifest, routeKey, observation, decision, manifestSHA256)
		},
		recordPreSendHold: database.RecordPreSendHold,
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

func NewWorker(database *Database, rpc *chain.Client, view *View, jupiterClient *jupiter.Client, config Config, credentials Credentials, v *Vault, state *vaultState) (*Worker, error) {
	if database == nil || database.pool == nil || rpc == nil || view == nil || v == nil || state == nil || config.PollInterval <= 0 {
		return nil, fmt.Errorf("invalid concrete worker configuration")
	}
	if _, err := credentials.signer(); err != nil {
		return nil, err
	}
	return &Worker{wake: make(chan struct{}, 1), routeKey: v.RouteKey, interval: config.PollInterval, manifest: v.manifest, events: state.events, holds: holdLog{events: state.events},
		runtime: productionTickRuntime(database, rpc, view, jupiterClient, state, v.manifest, credentials)}, nil
}

func (w *Worker) Tick(ctx context.Context) (tickErr error) {
	var healthObservation Observation
	var healthDecision Decision
	defer func() { w.publishWithdrawalHealth(ctx, healthObservation, healthDecision, tickErr) }()
	w.seedNAVReport(ctx)
	stopped, err := w.stopWhileLatched(ctx)
	if err != nil {
		return err
	}
	if stopped {
		healthDecision = Decision{Action: HoldManualRecovery}
		return nil
	}
	operation, err := w.runtime.loadNonterminal(ctx, w.routeKey)
	if err != nil {
		return err
	}
	w.events.inflight(operation != nil)
	if operation != nil {
		healthDecision = Decision{Action: RecoverTransaction}
		w.holds.acted(operation.StrategyKey)
		w.events.noteAction(operation.Decision.Action)
		if err := w.runtime.advance(ctx, *operation); err != nil {
			if operation.BroadcastIntentRecorded || !operation.Status.preBroadcast() {
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
	w.events.noteSnapshot(observation.Snapshot)
	if r, ok := observedNAVReport(observation.Snapshot); ok {
		w.events.navReported(w.routeKey, r)
		w.navReportKnown = true
	}
	w.events.leverageObserved(heldLaneLeverage(observation))
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
	w.events.noteAction(decision.Action)
	switch decision.Action {
	case Hold:
		w.holds.note(observation.Snapshot, decision.StrategyKey, HoldReason(decision.Reason), "")
		return nil
	case HoldManualRecovery:
		w.holds.note(observation.Snapshot, decision.StrategyKey, holdStopped, decision.Reason)
		return w.latchManualRecovery(ctx, observation, decision)
	}
	w.holds.acted(decision.StrategyKey)
	logStage("observe_decide", tickStart)
	request, effects, prepared, err := w.prepare(ctx, decision, observation)
	if prepared.Validate() == nil && prepared.Snapshot.Fresh && prepared.Snapshot.valued() {
		healthObservation = prepared
	}
	logStage("prepare", tickStart)
	if err != nil {
		w.noteHold(observation.Snapshot, decision.StrategyKey, err)
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
		err = staleHold("build_unavailable")
	} else {
		err = w.runtime.build(ctx, record.OperationID, request, effects)
	}
	logStage("build_sign", tickStart)
	if err != nil {
		w.noteHold(observation.Snapshot, decision.StrategyKey, err)
		return w.journalTickError(ctx, record.OperationID, err)
	}
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

func (w *Worker) seedNAVReport(ctx context.Context) {
	if w.navReportKnown || w.runtime.persistedNAVReport == nil || time.Since(w.navReportSeededAt) < time.Minute {
		return
	}
	w.navReportSeededAt = time.Now()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	r, ok, err := w.runtime.persistedNAVReport(ctx, w.routeKey)
	if err != nil {
		w.events.navReportUnread(err)
		return
	}
	w.navReportKnown = true
	if ok {
		w.events.navReported(w.routeKey, r)
	}
}

func (w *Worker) stopWhileLatched(ctx context.Context) (bool, error) {
	if w.runtime.loadLatch == nil {
		return false, nil
	}
	latch, latched, err := w.runtime.loadLatch(ctx, w.routeKey)
	if err != nil {
		return false, err
	}
	w.events.noteLatch(latched, latch.Reason)
	if latched {
		w.holds.note(Snapshot{}, "", holdStopped, latch.Reason)
	}
	return latched, nil
}

func (w *Worker) prepare(ctx context.Context, decision Decision, observation Observation) (any, ExpectedEffects, Observation, error) {
	wireDecision := decision
	switch decision.Action {
	case InitializeKaminoObligation:
		if w.runtime.prepareInitialization == nil {
			return nil, ExpectedEffects{}, observation, staleHold("initializer_preparation_unavailable")
		}
		prepared, r, err := w.runtime.prepareInitialization(ctx, w.manifest, wireDecision, observation)
		return r, kaminoInitializationEffects(r), prepared, err
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		prepared, e, err := w.runtime.prepareBridge(ctx, w.manifest, wireDecision, observation)
		return e.Request, e.ExpectedEffects, prepared, err
	case OpenRouteStep, DeleverRouteStep:
		if stepKind(wireDecision) != notStep {
			if w.runtime.prepareStep == nil {
				return nil, ExpectedEffects{}, observation, staleHold("step_preparation_unavailable")
			}
			prepared, e, err := w.runtime.prepareStep(ctx, w.manifest, wireDecision, observation)
			return e.Request, e.ExpectedEffects, prepared, err
		}
		prepared, e, err := w.runtime.prepareKamino(ctx, w.manifest, wireDecision, observation)
		return e.Request, e.ExpectedEffects, prepared, err
	case SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep:
		if conversionVenueHastra(wireDecision) {
			if w.runtime.prepareHastra == nil {
				return nil, ExpectedEffects{}, observation, staleHold("hastra_preparation_unavailable")
			}
			prepared, e, err := w.runtime.prepareHastra(ctx, w.manifest, wireDecision, observation)
			return e.Request, e.ExpectedEffects, prepared, err
		}
		prepared, e, err := w.runtime.prepareJupiter(ctx, w.manifest, wireDecision, observation)
		return e.Request, e.ExpectedEffects, prepared, err
	}
	return nil, ExpectedEffects{}, observation, fmt.Errorf("action %s is not dispatchable", decision.Action)
}

func (w *Worker) noteHold(s Snapshot, lane string, err error) {
	var hold *PreSendHold
	if errors.As(err, &hold) {
		w.holds.note(s, lane, hold.Reason, holdCause(hold))
	}
}

func (w *Worker) journalTickError(ctx context.Context, operationID string, err error) error {
	var hold *PreSendHold
	if !errors.As(err, &hold) || w.runtime.recordPreSendHold == nil {
		return err
	}
	if errors.Is(hold, chain.ErrUnavailable) {
		return confirmedObservationUnavailable(err)
	}
	if journalErr := w.runtime.recordPreSendHold(ctx, operationID, hold); journalErr != nil {
		return errors.Join(err, journalErr)
	}
	return err
}

func (w *Worker) latchManualRecovery(ctx context.Context, observation Observation, decision Decision) error {
	if w.runtime.latch == nil {
		return fmt.Errorf("manual recovery latch runtime is unavailable")
	}
	latch := ManualRecoveryLatch{Reason: decision.Reason, ObservationID: observation.Snapshot.ObservationID, ObservationSlot: observation.Snapshot.Slot}
	if err := w.runtime.latch(ctx, w.routeKey, latch); err != nil {
		return err
	}
	w.events.latched(decision.Reason)
	return nil
}

type afterBroadcastError struct{ err error }

func (e *afterBroadcastError) Error() string { return e.err.Error() }
func (e *afterBroadcastError) Unwrap() error { return e.err }

func isPreSendHold(err error) bool {
	var hold *PreSendHold
	var sent *afterBroadcastError
	if !errors.As(err, &hold) || errors.As(err, &sent) {
		return false
	}
	switch unwrappable := err.(type) {
	case *PreSendHold:
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

func (w *Worker) requestTick() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) notifySelectorCommit(action SelectorAction) {
	if action.entersOrRotates() {
		w.requestTick()
	}
}

func (w *Worker) runTicks(ctx context.Context, tick func(context.Context) error) error {
	for {
		err := tick(ctx)
		if err != nil {
			if !errors.Is(err, errConfirmedObservationUnavailable) && !isPreSendHold(err) {
				if ctx.Err() == nil {
					w.events.tickResult(err, true)
				}
				return err
			}
		}
		w.events.tickResult(err, false)
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

func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return fmt.Errorf("invalid worker runtime")
	}
	return w.runTicks(ctx, w.Tick)
}

type vaultState struct {
	events        *events
	refreshFailed failureStreak
	staleHeld     staleStreak
}

type failureStreak struct{ consecutive atomic.Int64 }

func (s *failureStreak) record() int64 { return s.consecutive.Add(1) }

func (s *failureStreak) clear() { s.consecutive.Store(0) }

type staleStreak struct{ firstUnixNano atomic.Int64 }

func (s *staleStreak) clear() { s.firstUnixNano.Store(0) }

func (s *staleStreak) startedAt(now time.Time) time.Time {
	first := now.UnixNano()
	if !s.firstUnixNano.CompareAndSwap(0, first) {
		if v := s.firstUnixNano.Load(); v != 0 {
			first = v
		}
	}
	return time.Unix(0, first)
}
