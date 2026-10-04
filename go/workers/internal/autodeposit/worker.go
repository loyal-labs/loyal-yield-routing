package autodeposit

import (
	"context"
	"errors"
	"log"
	"time"
)

// TargetExecutor is the consumer-defined execution effect for one dispatchable
// target. It carries no financial decisions: which claim to take, how much to
// sweep, and when a wire may be rebroadcast are all owned by this family's
// typed functions and Store SQL. The implementation owns only the effects this
// package cannot do — chain reads, wire construction, broadcast — and reports
// its end state through the legacy exit-code protocol.
type TargetExecutor interface {
	// Execute resolves one executable target: recovering a claim whose pull
	// already holds custody, or claiming and sweeping a fresh scheduled slot.
	// A nil exit code with an error means the run never reached an exit and is
	// classified as failed.
	Execute(ctx context.Context, target ExecutableTarget) (*int, error)
}

// WorkerDependencies are the worker's explicit dependencies and bounds.
type WorkerDependencies struct {
	// Store is the durable family state. Required.
	Store *Store
	// Executor performs one target's chain effects. Required.
	Executor TargetExecutor
	// RuntimeChain supplies the actual confirmed RPC frontier for readiness.
	// Optional until a runtime reporter is installed.
	RuntimeChain ConfirmedSlotReader
	// SlotHints carries externally signalled slot ids (for example realtime
	// events) that should be dispatched first. Optional; drained every tick.
	SlotHints *SlotHintQueue
	// OnAlert receives the alerts a tick's exits deserve. Optional.
	OnAlert func(ExecutorFailureAlert)
	// OnError receives each Tick's housekeeping failure. Run keeps the family's
	// recovery model — the next tick re-reads durable state — but a projection
	// or release failure must reach an operator, not disappear into the loop.
	// Optional; when nil, Run returns the first tick error it cannot recover
	// from so the runtime still sees the failure.
	OnError func(error)

	PollInterval          time.Duration
	ProjectionBatchLimit  int64
	DispatchLimit         int64
	StaleRequestedSeconds int64
	StaleSelectedSeconds  int64
	ReleaseBatchLimit     int64
}

// Worker is the synchronous Autodeposit runtime: one Tick is one complete
// reconcile-and-dispatch pass. Run repeats it until the context is cancelled.
type Worker struct {
	store           *Store
	executor        TargetExecutor
	hints           *SlotHintQueue
	onAlert         func(ExecutorFailureAlert)
	onError         func(error)
	runtimeChain    ConfirmedSlotReader
	runtimeReporter func(bool, uint64)
	executionErrors int

	pollInterval          time.Duration
	projectionBatchLimit  int64
	dispatchLimit         int64
	staleRequestedSeconds int64
	staleSelectedSeconds  int64
	releaseBatchLimit     int64
}

// Defaults for unset dependency bounds.
const (
	DefaultPollInterval         = time.Minute
	DefaultProjectionBatchLimit = 500
	DefaultDispatchLimit        = 20
	DefaultReleaseBatchLimit    = 100
)

// NewWorker validates the dependencies and applies the family defaults.
func NewWorker(deps WorkerDependencies) (*Worker, error) {
	if deps.Store == nil {
		return nil, errors.New("autodeposit worker requires a store")
	}
	if deps.Executor == nil {
		return nil, errors.New("autodeposit worker requires an executor")
	}
	worker := &Worker{
		store:                 deps.Store,
		executor:              deps.Executor,
		hints:                 deps.SlotHints,
		onAlert:               deps.OnAlert,
		onError:               deps.OnError,
		runtimeChain:          deps.RuntimeChain,
		pollInterval:          deps.PollInterval,
		projectionBatchLimit:  deps.ProjectionBatchLimit,
		dispatchLimit:         deps.DispatchLimit,
		staleRequestedSeconds: deps.StaleRequestedSeconds,
		staleSelectedSeconds:  deps.StaleSelectedSeconds,
		releaseBatchLimit:     deps.ReleaseBatchLimit,
	}
	if worker.runtimeChain == nil {
		if controller, ok := deps.Executor.(*Controller); ok {
			worker.runtimeChain, _ = controller.chain.(ConfirmedSlotReader)
		}
	}
	if worker.pollInterval <= 0 {
		worker.pollInterval = DefaultPollInterval
	}
	if worker.projectionBatchLimit <= 0 {
		worker.projectionBatchLimit = DefaultProjectionBatchLimit
	}
	if worker.dispatchLimit <= 0 {
		worker.dispatchLimit = DefaultDispatchLimit
	}
	if worker.staleRequestedSeconds <= 0 {
		worker.staleRequestedSeconds = StaleRequestedSlotSeconds
	}
	if worker.staleSelectedSeconds <= 0 {
		worker.staleSelectedSeconds = StaleRequestedSlotSeconds
	}
	if worker.releaseBatchLimit <= 0 {
		worker.releaseBatchLimit = DefaultReleaseBatchLimit
	}
	return worker, nil
}

// TickReport is what one pass observed and did. It is evidence for callers,
// not control state.
type TickReport struct {
	Projection ProjectionOutcome
	Outcome    ExecutorOutcome
	Dispatched []ExecutableTarget
	Alerts     []ExecutorFailureAlert
	// ExecutorErrors includes errors accompanying a legacy nonfatal exit code.
	// Such an exit can retain custody safely without proving runtime health.
	ExecutorErrors int
}

// Tick runs one complete pass:
//
//  1. advance the surplus-lot projection (schedule/deplete),
//  2. fail requested slots that were never picked up,
//  3. release selected claims that provably never pulled,
//  4. load executable targets — pull recovery first, fresh sweeps after,
//  5. dispatch each through the executor and classify its exit.
//
// A recovery pass runs even while a target's desired_active is false: custody
// that already left the wallet must be resolved regardless of enablement. The
// error return covers only this pass's own housekeeping failures; executor
// exits are tallied as outcomes, never aborted the scan.
func (w *Worker) Tick(ctx context.Context) (TickReport, error) {
	var report TickReport
	if err := ctx.Err(); err != nil {
		return report, err
	}

	projection, err := w.store.ProjectSurplusLotsOnce(ctx, w.projectionBatchLimit)
	if err != nil {
		return report, err
	}
	report.Projection = projection

	staleSlots, err := w.store.FailStaleRequestedSlots(ctx, w.releaseBatchLimit)
	if err != nil {
		return report, err
	}
	report.Outcome.StaleRequestedSlotsFailed = staleSlots

	staleClaims, err := w.store.ReleaseStaleSelectedClaims(ctx, w.staleSelectedSeconds, w.releaseBatchLimit)
	if err != nil {
		return report, err
	}
	report.Outcome.StaleClaimsReleased = staleClaims

	var hints []int64
	if w.hints != nil {
		hints = w.hints.Drain(int(w.dispatchLimit))
	}
	targets, err := w.store.LoadExecutableTargets(ctx, w.dispatchLimit, hints)
	if err != nil {
		return report, err
	}
	report.Dispatched = targets
	report.Outcome.TargetsScanned = len(targets)

	report.Alerts = w.dispatch(ctx, targets, &report.Outcome)
	report.ExecutorErrors = w.executionErrors
	for _, alert := range report.Alerts {
		if w.onAlert != nil {
			w.onAlert(alert)
		}
	}
	return report, nil
}

// dispatch runs the executor over the prioritized target list in order. The
// order is the family's recovery-starvation guarantee: it is preserved here
// even if the executor is slow, because dispatch is synchronous.
func (w *Worker) dispatch(ctx context.Context, targets []ExecutableTarget, outcome *ExecutorOutcome) []ExecutorFailureAlert {
	var alerts []ExecutorFailureAlert
	w.executionErrors = 0
	for _, target := range targets {
		if ctx.Err() != nil {
			return alerts
		}
		outcome.ExecutionsAttempted++
		exitCode, err := w.executor.Execute(ctx, target)
		if err != nil {
			w.executionErrors++
			if exitCode == nil {
				alert := genericExecutorAlert()
				alert.Summary = "autodeposit executor run errored before an exit: " + err.Error()
				alerts = append(alerts, *alert)
				outcome.ExecutionsFailed++
				continue
			}
		}
		if alert := outcome.RecordExecutorExit(exitCode); alert != nil {
			alerts = append(alerts, *alert)
		}
	}
	return alerts
}

// Run repeats Tick until the context is cancelled. A failing tick is not fatal:
// the next tick re-reads the durable state, which is the family's recovery
// model. Sanitized failures reach OnError; standalone callers with neither an
// OnError nor a runtime reporter retain the original fail-fast API.
// Cancellation is honored between ticks and between dispatches; Run returns the
// context's error.
func (w *Worker) Run(ctx context.Context) error {
	w.reportRuntime(false, 0)
	defer w.reportRuntime(false, 0)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		cycle, cancel := context.WithTimeout(ctx, runtimeCycleTimeout)
		report, err := w.Tick(cycle)
		var slot uint64
		if err == nil && w.runtimeReporter != nil {
			if len(report.Alerts) != 0 || report.ExecutorErrors != 0 || report.Outcome.ExecutionsProcessSuccessUnclassifd != 0 {
				err = errRuntimeProofUnavailable
			} else {
				slot, err = runtimeRecoveryHealth(cycle, w.store, w.runtimeChain, true)
			}
		}
		cancel()
		w.reportRuntime(err == nil && ctx.Err() == nil && slot > 0, slot)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if w.onError == nil && w.runtimeReporter == nil {
				return err
			}
			log.Print("autodeposit cycle_or_readiness_proof_failed")
			if w.onError != nil {
				w.onError(errRuntimeProofUnavailable)
			}
		}
		timer.Reset(w.pollInterval)
	}
}
