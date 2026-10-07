package backyard

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
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
	retryLog     tickRetryLog
	// borrowBlockedLog reports a borrow-blocked hold at most once an hour.
	borrowBlockedLog borrowBlockedLog
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
	prepareBridge                    func(context.Context, RouteManifest, Decision, Observation) (Observation, BridgeExecutionEvidence, error)
	prepareKamino                    func(context.Context, RouteManifest, Decision) (Observation, KaminoExecutionEvidence, error)
	prepareJupiter                   func(context.Context, RouteManifest, Decision) (Observation, JupiterExecutionEvidence, error)
	recordDecision                   func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error)
	admitBridge                      func(context.Context, string, Observation, Decision, BridgeExecutionEvidence) error
	admitKamino                      func(context.Context, string, Observation, Decision, KaminoExecutionEvidence) error
	admitJupiter                     func(context.Context, string, Observation, Decision, JupiterExecutionEvidence) error
	buildBridge                      func(context.Context, string, BridgeExecutionEvidence) error
	buildKamino                      func(context.Context, string, KaminoExecutionEvidence) error
	buildJupiter                     func(context.Context, string, JupiterExecutionEvidence) error
	custodyOwnershipProof            func(context.Context, RouteManifest, sharedCustodyAttributionConfig, ExpectedEffects, uint64, int64) (sharedCustodyAdmissionProof, error)
	// prefetchCustodyProof starts the pre-decision custody journal read while
	// the spend is prepared; nil keeps the serial custodyOwnershipProof.
	prefetchCustodyProof func(context.Context, RouteManifest, sharedCustodyAttributionConfig) custodyProofFinisher
	recordBudgetHold     func(context.Context, string, *BudgetHold) error
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
	observation.Snapshot.InitializationPolicyReady = false
	if observation.Snapshot.PilotActive {
		// Resolve the binding the same way initializationRequest does: the
		// candidate AUTO lane is governed by the auto-initializer constraint
		// set, the installed lanes by the multiply-initializer bindings.
		var bindingErr error
		if observation.Snapshot.RouteLane == autoAUTOPYUSD.Lane {
			_, _, bindingErr = p.manifest.autoInitializerBinding()
		} else {
			_, bindingErr = p.manifest.initializerBinding(observation.Snapshot.RouteLane)
		}
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
	// The extended read is preferred so production and shadow each make one
	// database read and still carry the activation baseline into the snapshot;
	// the bool-only reader stays for journals that predate the baseline. The
	// baseline fields are cleared first: mergeJournal runs on reused
	// observations, so a stale baseline from a previous merge must never
	// survive into this snapshot.
	observation.Snapshot.PilotBaselineKnown = false
	observation.Snapshot.PilotBaselineTicketSequenceRaw = 0
	if observation.planning != nil {
		planning := observation.planning
		observation.Snapshot.PilotActive = planning.pilot
		if planning.pilot && planning.baseline != nil {
			observation.Snapshot.PilotBaselineKnown = true
			observation.Snapshot.PilotBaselineTicketSequenceRaw = planning.baseline.TicketLastConsumedSequenceRaw
		}
	} else if reader, ok := p.journal.(interface {
		PilotRuntimeState(context.Context, string) (bool, *pilotActivationBaseline, error)
	}); ok {
		active, baseline, err := reader.PilotRuntimeState(ctx, p.routeKey)
		if err != nil {
			return err
		}
		observation.Snapshot.PilotActive = active
		if active && baseline != nil {
			observation.Snapshot.PilotBaselineKnown = true
			observation.Snapshot.PilotBaselineTicketSequenceRaw = baseline.TicketLastConsumedSequenceRaw
		}
	} else if reader, ok := p.journal.(interface {
		PilotRuntimeEnabled(context.Context, string) (bool, error)
	}); ok {
		active, err := reader.PilotRuntimeEnabled(ctx, p.routeKey)
		if err != nil {
			return err
		}
		observation.Snapshot.PilotActive = active
	}
	// The reviewed manifest's funded candidate lane is the only non-installed
	// route lane this snapshot may size at the pilot tranche. The stamp is
	// cleared unconditionally: a reused observation must never keep a lane the
	// current reviewed manifest no longer authorizes. No amount travels with
	// it — the tranche value stays the reviewed pilot cap.
	observation.Snapshot.PilotTrancheCapLane = ""
	if observation.Snapshot.PilotActive && p.manifest.selectorEntryFundingLane(autoAUTOPYUSD.Lane, false) {
		observation.Snapshot.PilotTrancheCapLane = autoAUTOPYUSD.Lane
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
	if observation.planning != nil {
		planning := observation.planning
		if err := applyUnwindIntentWithLane(&observation.Snapshot, planning.unwind, p.manifest.selectorEntryLaneAllowed); err != nil {
			observation.Snapshot.ManualReason = err.Error()
		}
		observation.Snapshot.SelectorEntryPaused = planning.paused
		applyLeverageTarget(&observation.Snapshot, planning.leverage)
		if err := applyPartialWithdrawal(&observation.Snapshot, planning.partialWithdrawal); err != nil {
			return err
		}
		return p.manifest.applySelectorEntry(&observation.Snapshot, planning.entry, time.Now().UTC())
	}
	// The manifest-aware reader is preferred exactly as the entry read below:
	// a recorded candidate-source unwind survives restart only while the
	// reviewed manifest's binding resolves. The plain reader stays for legacy
	// test interfaces and keeps its installed closure.
	if reader, ok := p.journal.(interface {
		LoadUnwindIntentOnManifest(context.Context, RouteManifest, string) (*UnwindIntent, error)
	}); ok {
		intent, err := reader.LoadUnwindIntentOnManifest(ctx, p.manifest, p.routeKey)
		if err != nil {
			return err
		}
		if err := applyUnwindIntentWithLane(&observation.Snapshot, intent, p.manifest.selectorEntryLaneAllowed); err != nil {
			observation.Snapshot.ManualReason = err.Error()
		}
	} else if reader, ok := p.journal.(interface {
		LoadUnwindIntent(context.Context, string) (*UnwindIntent, error)
	}); ok {
		intent, err := reader.LoadUnwindIntent(ctx, p.routeKey)
		if err != nil {
			return err
		}
		if err := applyUnwindIntentWithLane(&observation.Snapshot, intent, p.manifest.selectorEntryLaneAllowed); err != nil {
			observation.Snapshot.ManualReason = err.Error()
		}
	}
	// The B2 level target is a durable planning input like the entry and the
	// unwind: a construction refresh (no planning read) must see the same
	// stored target as the outer decision, or leverage_up never matches
	// (live 2026-09-29: every tick 'prepared evidence does not match').
	if reader, ok := p.journal.(interface {
		LoadPartialWithdrawal(context.Context, string) (*partialWithdrawalState, error)
	}); ok {
		partial, err := reader.LoadPartialWithdrawal(ctx, p.routeKey)
		if err != nil {
			return err
		}
		if err = applyPartialWithdrawal(&observation.Snapshot, partial); err != nil {
			return err
		}
	}
	observation.Snapshot.LeverageTargetLevel = 0
	if reader, ok := p.journal.(interface {
		LoadLeverageTarget(context.Context, string) (*LeverageTarget, error)
	}); ok {
		target, err := reader.LoadLeverageTarget(ctx, p.routeKey)
		if err != nil {
			return err
		}
		applyLeverageTarget(&observation.Snapshot, target)
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
		LoadSelectorEntryOnManifest(context.Context, RouteManifest, string) (*SelectorEntry, error)
	}); ok {
		entry, err := reader.LoadSelectorEntryOnManifest(ctx, p.manifest, p.routeKey)
		if err != nil {
			return err
		}
		if err = p.manifest.applySelectorEntry(&observation.Snapshot, entry, time.Now().UTC()); err != nil {
			return err
		}
	} else if reader, ok := p.journal.(interface {
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

func productionTickRuntime(database *Database, rpc *RPCClient, manifest RouteManifest, credentials Credentials) tickRuntime {
	state := productionObserveState{
		manifest: manifest, routeKey: productionRouteKey,
		journal: database,
		batch: func(ctx context.Context) (Observation, error) {
			rpc.refreshObservationLagSlots(ctx)
			planning, err := database.readRoutePlanningStateOnManifest(ctx, manifest, productionRouteKey, true)
			if err != nil {
				return Observation{}, err
			}
			observation, err := ObserveConfirmedRouteSnapshot(ctx, rpc, planning.observationManifest(manifest))
			if err != nil {
				return Observation{}, err
			}
			observation.planning = planning
			return observation, nil
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
			return BuildSimulateAndPersistKaminoInitialization(ctx, database, rpc, id, manifest, r, credentials)
		},
		completeUnwind: func(ctx context.Context, observation Observation) (bool, error) {
			if !observation.Snapshot.Unwind || !unwindComplete(observation.Snapshot) {
				return false, nil
			}
			intent, err := database.LoadUnwindIntentOnManifest(ctx, manifest, productionRouteKey)
			if err != nil {
				return false, err
			}
			if intent == nil {
				return false, fmt.Errorf("unwind disappeared before completion")
			}
			return true, database.CompleteUnwindIntentOnManifest(ctx, manifest, productionRouteKey, *intent, observation.Snapshot)
		},
		loadNonterminal: func(ctx context.Context, routeKey string) (*PersistedOperation, error) {
			return database.LoadNonterminalOnManifest(ctx, routeKey, manifest)
		},
		advance: func(ctx context.Context, operation PersistedOperation) error {
			return advanceNonterminalWithManifest(ctx, manifest, database, rpc, operation)
		},
		observe:                          state.observe,
		loadLatch:                        database.ManualRecoveryLatch,
		recordManualRecovery:             database.RecordManualRecovery,
		recordManualRecoveryAtGeneration: database.RecordManualRecoveryAtGeneration,
		prepareBridge: func(ctx context.Context, manifest RouteManifest, decision Decision, observation Observation) (Observation, BridgeExecutionEvidence, error) {
			return prepareBridgeFromTickObservation(ctx, rpc, manifest, decision, observation)
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
		recordDecision: func(ctx context.Context, routeKey string, observation Observation, decision Decision, manifestSHA256, policyCatalogSHA256 string) (DecisionRecord, error) {
			return database.RecordDecisionOnManifest(ctx, manifest, routeKey, observation, decision, manifestSHA256, policyCatalogSHA256)
		},
		prefetchCustodyProof: func(ctx context.Context, manifest RouteManifest, cfg sharedCustodyAttributionConfig) custodyProofFinisher {
			return database.prefetchSharedCustodyOwnershipProof(ctx, manifest, cfg, rpc)
		},
		custodyOwnershipProof: func(ctx context.Context, manifest RouteManifest, cfg sharedCustodyAttributionConfig, expected ExpectedEffects, observedRaw uint64, observedSlot int64) (sharedCustodyAdmissionProof, error) {
			return database.ObserveSharedCustodyOwnershipProofWithRPC(ctx, manifest, cfg, expected, observedRaw, observedSlot, rpc)
		},
		recordBudgetHold: database.RecordPhase3BudgetHold,
		admitBridge: func(ctx context.Context, operationID string, observation Observation, decision Decision, evidence BridgeExecutionEvidence) error {
			if plan, err, ok := admitPartialWithdrawalLeg(ctx, rpc, productionJupiterClient(), manifest, observation, decision, evidence.Request, evidence.ExpectedEffects); ok {
				if err != nil {
					return err
				}
				return database.persistPhase3ExitAdmission(ctx, rpc, operationID, observation, decision, plan)
			}
			if evidence.Request.Action == VoltrAllocateToSquads && decision.Reason == topupAllocationReason {
				return database.admitPhase3TopupAllocation(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence)
			}
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
			if plan, err, ok := admitPartialWithdrawalLeg(ctx, rpc, productionJupiterClient(), manifest, observation, decision, evidence.Request, evidence.ExpectedEffects); ok {
				if err != nil {
					return err
				}
				return database.persistPhase3ExitAdmission(ctx, rpc, operationID, observation, decision, plan)
			}
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
			if plan, err, ok := admitPartialWithdrawalLeg(ctx, rpc, productionJupiterClient(), manifest, observation, decision, evidence.Request, evidence.ExpectedEffects); ok {
				if err != nil {
					return err
				}
				return database.persistPhase3ExitAdmission(ctx, rpc, operationID, observation, decision, plan)
			}
			if evidence.Request.Action == SwapDebtToCollateralStep {
				return database.admitPhase3LeverageSwap(ctx, rpc, productionJupiterClient(), manifest, operationID, observation, decision, evidence)
			}
			if evidence.Request.Action == SwapCollateralToDebtStep && decision.Reason == exitCycleSwapReason {
				plan, err := observePhase3ExitCycleSwapAdmission(ctx, rpc, productionJupiterClient(), manifest, observation, decision, evidence)
				if err != nil {
					return err
				}
				return database.persistPhase3ExitAdmission(ctx, rpc, operationID, observation, decision, plan)
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
			return BuildSimulateAndPersistBridge(ctx, database, rpc, operationID, evidence, credentials)
		},
		buildKamino: func(ctx context.Context, operationID string, evidence KaminoExecutionEvidence) error {
			return BuildSimulateAndPersistKamino(ctx, database, rpc, operationID, evidence, credentials)
		},
		buildJupiter: func(ctx context.Context, operationID string, evidence JupiterExecutionEvidence) error {
			return BuildSimulateAndPersistJupiter(ctx, database, rpc, operationID, evidence, credentials)
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
func NewWorker(database *Database, rpc *RPCClient, config Config, credentials Credentials) (*Worker, error) {
	if database == nil || database.pool == nil || rpc == nil || config.validateLease() != nil {
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
	return &Worker{wake: make(chan struct{}, 1), routeKey: productionRouteKey, interval: config.PollInterval, manifest: manifest, runtime: productionTickRuntime(database, rpc, manifest, Credentials{PolicyKey: key})}, nil
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
		backyardEvents.noteLatch(latched, latch.Reason)
		if latched {
			return w.recordLatchedHold(ctx, latch)
		}
	}
	operation, err := w.runtime.loadNonterminal(ctx, w.routeKey)
	if err != nil {
		return err
	}
	backyardEvents.inflight(operation != nil)
	if operation != nil {
		backyardEvents.noteAction(operation.Decision.Action)
		if err := w.runtime.advance(ctx, *operation); err != nil {
			if operation.BroadcastIntentRecorded || !preBroadcastStatus(operation.Status) {
				return &afterBroadcastError{err}
			}
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
	tickStart := time.Now()
	observation, err := w.runtime.observe(ctx)
	if err != nil {
		return err
	}
	backyardEvents.noteSnapshot(observation.Snapshot)
	if w.runtime.completeUnwind != nil {
		completed, err := w.runtime.completeUnwind(ctx, observation)
		if err != nil {
			return err
		}
		if completed {
			return nil
		}
	}
	decision := w.manifest.DecideOnManifest(observation.Snapshot)
	w.borrowBlockedLog.note(time.Now(), decision)
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
	if err := w.manifest.validateDecision(decision); err != nil {
		return err
	}
	backyardEvents.noteAction(decision.Action)
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
	logStage("observe_decide", tickStart)
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
	// The custody journal read of a PYUSD spend does not depend on the
	// prepared wire, so it runs while preparation re-observes the chain.
	var prefetchedCustody custodyProofFinisher
	if w.runtime.prefetchCustodyProof != nil && custodyProofPrefetchAction(decision, observation.Snapshot) {
		prefetchedCustody = w.runtime.prefetchCustodyProof(ctx, w.manifest, autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, w.routeKey))
	}
	switch executionDecision {
	case InitializeKaminoObligation:
		if w.runtime.prepareInitialization == nil {
			return budgetHold("initializer_preparation_unavailable")
		}
		observation, initializationRequest, err = w.runtime.prepareInitialization(ctx, w.manifest, wireDecision)
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		observation, bridgeEvidence, err = w.runtime.prepareBridge(ctx, w.manifest, wireDecision, observation)
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
	logStage("prepare", tickStart)
	if err != nil {
		return err
	}
	decision = w.manifest.DecideOnManifest(observation.Snapshot)
	if err := w.manifest.validateDecision(decision); err != nil {
		return err
	}
	// A whole-debt repayment accrues interest between decide and prepare; it
	// is accepted only when preparation built the full payoff of that debt.
	// Any other drift is a new state: nothing is recorded yet, so retry the
	// leg on the next tick instead of stopping the worker (live 2026-09-28).
	accruedRepayment := executionDecision == DeleverRouteStep && kaminoEvidence.Request.FullPayoff &&
		fullDebtRepaymentRefreshed(preparedDecision, decision, observation.Snapshot)
	if !decisionsEqual(decision, preparedDecision) && !accruedRepayment {
		return confirmedObservationUnavailable(fmt.Errorf("prepared evidence does not match the refreshed decision: decided %s/%s/%d, refreshed %s/%s/%d",
			preparedDecision.Action, preparedDecision.Reason, preparedDecision.AmountRaw, decision.Action, decision.Reason, decision.AmountRaw))
	}
	// Strict pre-decision shared-custody ownership proof (doc 26): a prepared
	// AUTO-PYUSD spend is proofed against the prepared evidence's exact
	// effects BEFORE the row exists, and the proof rides this observation into
	// RecordDecision and the locked measured admission. Zero-spend and other
	// lanes are untouched.
	switch executionDecision {
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		err = w.observePreDecisionCustodyOwnershipProof(ctx, &observation, decision, bridgeEvidence.ExpectedEffects, prefetchedCustody)
	case OpenPrimeUSDCStep, DeleverPrimeUSDCStep, OpenRouteStep, DeleverRouteStep:
		err = w.observePreDecisionCustodyOwnershipProof(ctx, &observation, decision, kaminoEvidence.ExpectedEffects, prefetchedCustody)
	case SwapUSDCToPrimeStep, SwapPrimeToUSDCStep, SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep:
		err = w.observePreDecisionCustodyOwnershipProof(ctx, &observation, decision, jupiterEvidence.ExpectedEffects, prefetchedCustody)
	}
	if err != nil {
		return err
	}
	logStage("custody_proof", tickStart)
	record, err := w.runtime.recordDecision(ctx, w.routeKey, observation, decision, w.manifest.SHA256, policyHash)
	if err != nil {
		return err
	}
	logStage("record_decision", tickStart)
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
			logStage("admit", tickStart)
			if err == nil {
				err = w.runtime.buildKamino(ctx, record.OperationID, kaminoEvidence)
			}
		}
	case SwapUSDCToPrimeStep, SwapPrimeToUSDCStep, SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep:
		if w.runtime.admitJupiter == nil {
			err = budgetHold("swap_admission_unavailable")
		} else {
			err = w.runtime.admitJupiter(ctx, record.OperationID, observation, decision, jupiterEvidence)
			logStage("admit", tickStart)
			if err == nil {
				err = w.runtime.buildJupiter(ctx, record.OperationID, jupiterEvidence)
			}
		}
	default:
		return fmt.Errorf("prepared evidence no longer matches an actionable decision")
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
	// A "latched:" reason is the per-tick re-record of an existing stop.
	if !strings.HasPrefix(decision.Reason, "latched:") {
		backyardEvents.latched(decision.Reason)
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

// isPreSendHold reports a tick error made only of admission holds raised
// before anything was broadcast. The next tick re-observes the chain and
// re-derives the whole leg, exactly what a process restart would do, without
// the Render restart (13 tries, 10 restarts in the 09-26 AUTO entry; the
// per-reason list it replaces grew with every move). Like isPureHold, a hold
// joined with any other fault (a store or wire error) still stops the worker,
// and so does any hold from an operation whose broadcast intent is recorded.
func isPreSendHold(err error) bool {
	var hold *BudgetHold
	var sent *afterBroadcastError
	if !errors.As(err, &hold) || errors.As(err, &sent) {
		return false
	}
	switch unwrappable := err.(type) {
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
	if action != "ENTER" && action != "CANARY_ENTER" && action != "SWITCH" {
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
			// Confirmed-observation gaps, pure journaled spending-limit holds
			// and pre-send holds skip this tick's leg and retry on the next interval;
			// anything else - including a hold joined with a store error -
			// is a process fault and stops the worker.
			if !errors.Is(err, errConfirmedObservationUnavailable) && !isPureHold(err) && !isPreSendHold(err) {
				// A SIGTERM cancellation is a normal stop, not an alert.
				if ctx.Err() == nil {
					backyardEvents.tickResult(err, true)
				}
				return err
			}
		}
		backyardEvents.tickResult(err, false)
		if err != nil {
			w.retryLog.note(time.Now(), err)
		}
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
