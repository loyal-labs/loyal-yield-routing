package backyardrwa

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// A receipt-integrity observation carries no decodable book, so there is no
	// coherent position projection to record; the strategy_receipt_integrity
	// hold decision is itself the durable record of that tick.
	if observation.Snapshot.StrategyReceiptIntegrityFault {
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
	return nil
}

func productionTickRuntime(database *Database, rpc *RPCClient, manifest RouteManifest) tickRuntime {
	state := productionObserveState{
		routeKey: productionRouteKey,
		journal:  database,
		batch: func(ctx context.Context) (Observation, error) {
			return ObserveConfirmedRouteSnapshot(ctx, rpc, manifest)
		},
		identity: newProgramIdentityWatcher(rpc).observe,
	}
	return tickRuntime{
		loadNonterminal: database.LoadNonterminal,
		advance: func(ctx context.Context, operation PersistedOperation) error {
			return AdvanceNonterminal(ctx, database, rpc, operation)
		},
		observe:                          state.observe,
		loadLatch:                        database.ManualRecoveryLatch,
		recordManualRecovery:             database.RecordManualRecovery,
		recordManualRecoveryAtGeneration: database.RecordManualRecoveryAtGeneration,
		prepareBridge: func(ctx context.Context, manifest RouteManifest, decision Decision) (Observation, BridgeExecutionEvidence, error) {
			return observeConfirmedBridgeExecutionEvidenceWithEnrichment(ctx, rpc, manifest, decision, state.enrich)
		},
		prepareKamino: func(ctx context.Context, manifest RouteManifest, decision Decision) (Observation, KaminoExecutionEvidence, error) {
			return observeConfirmedKaminoExecutionEvidenceWithEnrichment(ctx, rpc, manifest, decision, state.enrich)
		},
		prepareJupiter: func(ctx context.Context, manifest RouteManifest, decision Decision) (Observation, JupiterExecutionEvidence, error) {
			return observeConfirmedJupiterExecutionEvidenceWithEnrichment(ctx, rpc, manifest, decision, productionJupiterClient(), state.enrich)
		},
		recordDecision:   database.RecordDecision,
		recordBudgetHold: database.RecordPhase3BudgetHold,
		admitBridge: func(ctx context.Context, operationID string, observation Observation, decision Decision, evidence BridgeExecutionEvidence) error {
			if evidence.Request.Action == ReportNAV && observation.Snapshot.PositionDebtRaw > 0 && catalogJupiterRoute(observation.Snapshot.RouteLane) {
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
	decision := Decide(observation.Snapshot)
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
	var hold *BudgetHold
	if errors.As(err, &hold) && w.runtime.recordBudgetHold != nil {
		if journalErr := w.runtime.recordBudgetHold(ctx, record.OperationID, hold); journalErr != nil {
			return errors.Join(err, journalErr)
		}
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

func (w *Worker) runTicks(ctx context.Context, leaseErrors <-chan error) error {
	for {
		if err := w.Tick(ctx); err != nil {
			select {
			case leaseErr := <-leaseErrors:
				return leaseErr
			default:
			}
			if !errors.Is(err, errConfirmedObservationUnavailable) {
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
	runErr = w.runTicks(runCtx, leaseErrors)
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
