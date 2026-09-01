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
	routeKey string
	interval time.Duration
	manifest RouteManifest
	runtime  tickRuntime
}

type tickRuntime struct {
	loadNonterminal func(context.Context, string) (*PersistedOperation, error)
	advance         func(context.Context, PersistedOperation) error
	observe         func(context.Context) (Observation, error)
	prepareBridge   func(context.Context, RouteManifest, Decision) (Observation, BridgeExecutionEvidence, error)
	prepareKamino   func(context.Context, RouteManifest, Decision) (Observation, KaminoExecutionEvidence, error)
	prepareJupiter  func(context.Context, RouteManifest, Decision) (Observation, JupiterExecutionEvidence, error)
	recordDecision  func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error)
	buildBridge     func(context.Context, string, BridgeExecutionEvidence) error
	buildKamino     func(context.Context, string, KaminoExecutionEvidence) error
	buildJupiter    func(context.Context, string, JupiterExecutionEvidence) error
}

func productionTickRuntime(database *Database, rpc *RPCClient, manifest RouteManifest) tickRuntime {
	return tickRuntime{
		loadNonterminal: database.LoadNonterminal,
		advance: func(ctx context.Context, operation PersistedOperation) error {
			return AdvanceNonterminal(ctx, database, rpc, operation)
		},
		observe: func(ctx context.Context) (Observation, error) {
			observation, err := ObserveConfirmedRouteSnapshot(ctx, rpc, manifest)
			if err != nil {
				return Observation{}, fmt.Errorf("%w: %v", errConfirmedObservationUnavailable, err)
			}
			required, err := database.PostMutationNAVRequired(ctx, productionRouteKey)
			if err != nil {
				return Observation{}, err
			}
			observation.Snapshot.PostMutationNAVRequired = required
			if err := database.RecordPositionSnapshot(ctx, productionRouteKey, observation); err != nil {
				return Observation{}, err
			}
			return observation, nil
		},
		prepareBridge: func(ctx context.Context, manifest RouteManifest, decision Decision) (Observation, BridgeExecutionEvidence, error) {
			required, err := database.PostMutationNAVRequired(ctx, productionRouteKey)
			if err != nil {
				return Observation{}, BridgeExecutionEvidence{}, err
			}
			return ObserveConfirmedBridgeExecutionEvidence(ctx, rpc, manifest, decision, required)
		},
		prepareKamino: func(ctx context.Context, manifest RouteManifest, decision Decision) (Observation, KaminoExecutionEvidence, error) {
			return ObserveConfirmedKaminoExecutionEvidence(ctx, rpc, manifest, decision)
		},
		prepareJupiter: func(ctx context.Context, manifest RouteManifest, decision Decision) (Observation, JupiterExecutionEvidence, error) {
			return ObserveConfirmedJupiterExecutionEvidence(ctx, rpc, manifest, decision, productionJupiterClient())
		},
		recordDecision: database.RecordDecision,
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
	if err := decision.Validate(); err != nil {
		return err
	}
	if w.manifest.PolicyCatalog.SHA256 == nil || !sha256Pattern.MatchString(*w.manifest.PolicyCatalog.SHA256) {
		return ErrBridgePrerequisitesUnavailable
	}
	policyHash := *w.manifest.PolicyCatalog.SHA256
	if decision.Action == Hold || decision.Action == HoldManualRecovery {
		_, err = w.runtime.recordDecision(ctx, w.routeKey, observation, decision, w.manifest.SHA256, policyHash)
		return err
	}
	if blocker := w.manifest.executionBlocker(); blocker != nil {
		return blocker
	}
	var bridgeEvidence BridgeExecutionEvidence
	var kaminoEvidence KaminoExecutionEvidence
	var jupiterEvidence JupiterExecutionEvidence
	preparedDecision := decision
	switch decision.Action {
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		observation, bridgeEvidence, err = w.runtime.prepareBridge(ctx, w.manifest, decision)
	case OpenPrimeUSDCStep, DeleverPrimeUSDCStep:
		observation, kaminoEvidence, err = w.runtime.prepareKamino(ctx, w.manifest, decision)
	case SwapUSDCToPrimeStep, SwapPrimeToUSDCStep:
		observation, jupiterEvidence, err = w.runtime.prepareJupiter(ctx, w.manifest, decision)
	default:
		return fmt.Errorf("action %s is not dispatchable", decision.Action)
	}
	if err != nil {
		return err
	}
	decision = Decide(observation.Snapshot)
	if err := decision.Validate(); err != nil {
		return err
	}
	if decision != preparedDecision {
		return fmt.Errorf("prepared evidence does not match the refreshed decision")
	}
	record, err := w.runtime.recordDecision(ctx, w.routeKey, observation, decision, w.manifest.SHA256, policyHash)
	if err != nil {
		return err
	}
	if record.Status != Decided || record.OperationID == "" {
		return fmt.Errorf("actionable decision was not durably recorded as decided")
	}
	switch decision.Action {
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		return w.runtime.buildBridge(ctx, record.OperationID, bridgeEvidence)
	case OpenPrimeUSDCStep, DeleverPrimeUSDCStep:
		return w.runtime.buildKamino(ctx, record.OperationID, kaminoEvidence)
	case SwapUSDCToPrimeStep, SwapPrimeToUSDCStep:
		return w.runtime.buildJupiter(ctx, record.OperationID, jupiterEvidence)
	default:
		return fmt.Errorf("prepared evidence no longer matches an actionable decision")
	}
}

type routeLeaser interface {
	AcquireRouteLease(context.Context, string, string, time.Duration) (RouteLease, error)
	RefreshRouteLease(context.Context, time.Duration) (RouteLease, error)
	ReleaseRouteLease(context.Context) (bool, error)
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
	if _, err := leases.AcquireRouteLease(ctx, w.routeKey, owner, config.LeaseTTL); err != nil {
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
