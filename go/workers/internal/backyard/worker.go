package backyard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

const productionRouteKey = "rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"

// The current lease owner is the platform-neutral scope+instance+release
// identity. The legacy Render owner format stays valid through the cutover so
// an unexpired fence held by the previous deployment identity keeps its exact
// generation/token semantics instead of being released by owner-text drift.
var platformNeutralLeaseOwnerPattern = regexp.MustCompile(`^worker:backyard:[a-zA-Z0-9_-]{1,80}:sha-[0-9a-f]{40}$`)
var legacyRenderLeaseOwnerPattern = regexp.MustCompile(`^render:srv-[a-z0-9]+:sha-[0-9a-f]{40}$`)

// ValidLeaseOwner reports whether the caller-supplied owner is a deployment
// identity this runtime accepts. Any other string is rejected before it can
// reach the database; authority never comes from the owner text itself.
func ValidLeaseOwner(owner string) bool {
	return platformNeutralLeaseOwnerPattern.MatchString(owner) || legacyRenderLeaseOwnerPattern.MatchString(owner)
}

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

func productionTickRuntime(database *Database, rpc *RPCClient, manifest RouteManifest, credentials Credentials) tickRuntime {
	return tickRuntime{
		loadNonterminal: database.LoadNonterminal,
		advance: func(ctx context.Context, operation PersistedOperation) error {
			return AdvanceNonterminal(ctx, database, rpc, operation)
		},
		observe: func(ctx context.Context) (Observation, error) {
			observation, err := ObserveConfirmedRouteSnapshot(ctx, rpc, manifest)
			if err != nil {
				return Observation{}, err
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
// capability is injected, validated here, and never derivable from a decision,
// observation, or recovery input.
func NewWorker(database *Database, rpc *RPCClient, routeKey string, config Config, credentials Credentials) (*Worker, error) {
	if database == nil || rpc == nil || routeKey != productionRouteKey || config.validateLease() != nil {
		return nil, fmt.Errorf("invalid concrete worker configuration")
	}
	key, err := credentials.signer()
	if err != nil {
		return nil, err
	}
	if database.pool == nil || rpc.client == nil || rpc.url == "" {
		return nil, fmt.Errorf("Backyard database and RPC must be constructed")
	}
	credentials = Credentials{PolicyKey: key}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return nil, err
	}
	return &Worker{routeKey: routeKey, interval: config.PollInterval, manifest: manifest, runtime: productionTickRuntime(database, rpc, manifest, credentials)}, nil
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
	executionDecision, err := fixedRouteAction(decision.Action, decision.StrategyKey)
	if err != nil {
		return err
	}
	wireDecision := decision
	wireDecision.Action = executionDecision
	switch executionDecision {
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		observation, bridgeEvidence, err = w.runtime.prepareBridge(ctx, w.manifest, wireDecision)
	case OpenPrimeUSDCStep, DeleverPrimeUSDCStep:
		observation, kaminoEvidence, err = w.runtime.prepareKamino(ctx, w.manifest, wireDecision)
	case SwapUSDCToPrimeStep, SwapPrimeToUSDCStep:
		observation, jupiterEvidence, err = w.runtime.prepareJupiter(ctx, w.manifest, wireDecision)
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

// Run is the legacy standalone bootstrap: the only path that reads the
// environment. It composes the same explicit injected runtime the loyal-engine
// command uses and then behaves identically. Missing deployment artifacts are
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
	signer, err := loadPinnedPolicySigner()
	if err != nil {
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
	err = RunWithConfig(ctx, EngineConfig{
		Database:     database,
		RPC:          rpc,
		Credentials:  Credentials{PolicyKey: signer},
		RouteKey:     runtimeConfig.RouteKey,
		Config:       DefaultConfig(),
		Owner:        leaseOwner,
		Out:          out,
		ImageVersion: runtimeConfig.ImageVersion,
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
