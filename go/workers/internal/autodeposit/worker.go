package autodeposit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

type TargetExecutor interface {
	Execute(ctx context.Context, target ExecutableTarget) (ExecutorResult, error)
}

type WorkerDependencies struct {
	Store    *Store
	Executor TargetExecutor
	Facts    *engine.Facts
	FeePayer FeePayerReader
	Notifier *SweepNotifier

	PollInterval          time.Duration
	ProjectionBatchLimit  int64
	DispatchLimit         int64
	StaleRequestedSeconds int64
	StaleSelectedSeconds  int64
	ReleaseBatchLimit     int64
}

type Worker struct {
	store           *Store
	executor        TargetExecutor
	facts           *engine.Facts
	feePayer        FeePayerReader
	notifier        *SweepNotifier
	notified        map[int64]struct{}
	executionErrors int

	pollInterval          time.Duration
	projectionBatchLimit  int64
	dispatchLimit         int64
	staleRequestedSeconds int64
	staleSelectedSeconds  int64
	releaseBatchLimit     int64
}

const (
	DefaultPollInterval         = 10 * time.Second
	DefaultProjectionBatchLimit = 500
	DefaultDispatchLimit        = 20
	DefaultReleaseBatchLimit    = 100
)

type FeePayerReader interface {
	FeePayerLamports(ctx context.Context) (payer string, lamports uint64, err error)
}

const notifyBudget = 15 * time.Second

const WakeupChannel = "loyal_yield_autodeposit_wakeup"

func NewWorker(deps WorkerDependencies) (*Worker, error) {
	if deps.Store == nil {
		return nil, errors.New("autodeposit worker requires a store")
	}
	if deps.Executor == nil {
		return nil, errors.New("autodeposit worker requires an executor")
	}
	if deps.Facts == nil {
		return nil, errors.New("autodeposit worker requires facts")
	}
	if deps.FeePayer == nil {
		return nil, errors.New("autodeposit worker requires a fee payer reader")
	}
	worker := &Worker{
		store:                 deps.Store,
		executor:              deps.Executor,
		facts:                 deps.Facts,
		feePayer:              deps.FeePayer,
		notifier:              deps.Notifier,
		notified:              map[int64]struct{}{},
		pollInterval:          deps.PollInterval,
		projectionBatchLimit:  deps.ProjectionBatchLimit,
		dispatchLimit:         deps.DispatchLimit,
		staleRequestedSeconds: deps.StaleRequestedSeconds,
		staleSelectedSeconds:  deps.StaleSelectedSeconds,
		releaseBatchLimit:     deps.ReleaseBatchLimit,
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

type TickReport struct {
	Projection        ProjectionOutcome
	Outcome           ExecutorOutcome
	Dispatched        []ExecutableTarget
	Alerts            []ExecutorFailureAlert
	ExecutorErrors    int
	FeePayerExhausted bool
}

func (r TickReport) settled() bool {
	o := r.Outcome
	return !r.FeePayerExhausted && o.ExecutionsAttempted == o.ExecutionsCompleted+o.ExecutionsNoop+o.ExecutionsNotActionable
}

func (w *Worker) Tick(ctx context.Context) (TickReport, error) { return w.tick(ctx, nil) }

func (w *Worker) tick(ctx context.Context, hints []int64) (TickReport, error) {
	var report TickReport
	if err := ctx.Err(); err != nil {
		return report, err
	}

	projection, err := w.store.ProjectSurplusLotsOnce(ctx, w.projectionBatchLimit)
	if err != nil {
		return report, w.failed("autodeposit_projection_failed", err)
	}
	report.Projection = projection

	staleSlots, err := w.store.FailStaleRequestedSlots(ctx, w.releaseBatchLimit)
	if err != nil {
		return report, w.failed("autodeposit_execution_queue_preparation_failed", err)
	}
	report.Outcome.StaleRequestedSlotsFailed = staleSlots
	for range staleSlots {
		w.failed("autodeposit_requested_slot_timed_out", nil)
	}

	staleClaims, err := w.store.ReleaseStaleSelectedClaims(ctx, w.staleSelectedSeconds, w.releaseBatchLimit)
	if err != nil {
		return report, w.failed("autodeposit_execution_queue_preparation_failed", err)
	}
	report.Outcome.StaleClaimsReleased = staleClaims
	for range staleClaims {
		w.failed("autodeposit_stale_claim_released", nil)
	}

	payer, lamports, err := w.feePayer.FeePayerLamports(ctx)
	if err != nil {
		return report, w.failed("autodeposit_dependency_unavailable", err)
	}
	w.facts.FeePayerBalance(payer, lamports)
	report.FeePayerExhausted = lamports < FeePayerMinimumLamports
	if !report.FeePayerExhausted {
		clear(w.notified)
	}

	targets, err := w.store.LoadExecutableTargets(ctx, w.dispatchLimit, hints)
	if err != nil {
		return report, w.failed("autodeposit_execution_queue_preparation_failed", err)
	}
	if report.FeePayerExhausted {
		slog.Error("autodeposit fee payer is out of SOL; top up the delegated signer", "code", "autodeposit_fee_payer_exhausted",
			"feePayer", payer, "balanceLamports", lamports, "minimumLamports", FeePayerMinimumLamports, "dueTargets", len(targets))
		w.notifyUnstarted(ctx, targets)
		return report, nil
	}
	report.Dispatched = targets
	report.Outcome.TargetsScanned = len(targets)

	report.Alerts = w.dispatch(ctx, targets, &report.Outcome)
	report.ExecutorErrors = w.executionErrors
	return report, nil
}

func (w *Worker) notifyUnstarted(ctx context.Context, targets []ExecutableTarget) {
	if w.notifier == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, notifyBudget)
	defer cancel()
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		if _, done := w.notified[target.ScheduledSlotID]; done || target.isRecovery() {
			continue
		}
		if w.notifier.NotifyFailed(ctx, target.Wallet, target.ScheduledSlotID) {
			w.notified[target.ScheduledSlotID] = struct{}{}
		}
	}
}

func (w *Worker) failed(code string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	w.facts.Failed(engine.FamilyAutodeposit, code)
	if err != nil {
		slog.Error("autodeposit "+code, "code", code, "error", engine.ErrorText(err))
	} else {
		slog.Warn("autodeposit "+code, "code", code)
	}
	return err
}

func (w *Worker) dispatch(ctx context.Context, targets []ExecutableTarget, outcome *ExecutorOutcome) []ExecutorFailureAlert {
	var alerts []ExecutorFailureAlert
	w.executionErrors = 0
	for _, target := range targets {
		if ctx.Err() != nil {
			return alerts
		}
		outcome.ExecutionsAttempted++
		result, err := w.executor.Execute(ctx, target)
		var alert *ExecutorFailureAlert
		if err != nil {
			w.executionErrors++
		}
		if err != nil && result == ResultUnknown {
			alert = genericExecutorAlert()
			alert.Summary = "autodeposit executor run errored without a classified outcome: " + engine.ErrorText(err)
			outcome.ExecutionsFailed++
		} else {
			alert = outcome.RecordExecutorResult(result)
		}
		attributes := []any{"result", string(result), "targetId", target.TargetID, "scheduledSlotId", target.ScheduledSlotID}
		if err != nil {
			attributes = append(attributes, "error", engine.ErrorText(err))
		}
		if alert != nil {
			alerts = append(alerts, *alert)
			w.facts.Failed(engine.FamilyAutodeposit, alert.Code)
			level := slog.LevelError
			if alert.SelfRecovering {
				level = slog.LevelWarn
			}
			slog.Log(ctx, level, alert.Summary, append(attributes, "code", alert.Code, "operation", alert.Operation, "retryable", alert.Retryable)...)
		} else if err != nil {
			slog.Info("autodeposit execution "+string(result), attributes...)
		}
	}
	return alerts
}

func (w *Worker) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	wake := make(chan int64, w.dispatchLimit)
	listening := make(chan struct{})
	go func() {
		defer close(listening)
		w.listen(ctx, wake)
	}()
	defer func() {
		cancel()
		<-listening
	}()
	var hints []int64
	for {
		cycle, cycleCancel := context.WithTimeout(ctx, runtimeCycleTimeout)
		if report, err := w.tick(cycle, hints); err == nil {
			if err = w.passFacts(cycle); err == nil && report.settled() {
				w.facts.Progress(engine.FamilyAutodeposit)
			}
		}
		cycleCancel()
		hints = nil
		timer := time.NewTimer(w.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		case slot := <-wake:
			timer.Stop()
			hints = append(hints, slot)
		}
	delivered:
		for len(hints) > 0 && len(hints) < cap(wake) {
			select {
			case slot := <-wake:
				hints = append(hints, slot)
			default:
				break delivered
			}
		}
	}
}

func (w *Worker) passFacts(ctx context.Context) error {
	var inflight int
	var oldest float64
	err := w.store.pool.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts
   WHERE attempt_state IN ('prepared','submitted')),
  COALESCE(EXTRACT(EPOCH FROM now() - LEAST(
    (SELECT min(created_at) FROM loyal_yield.balance_sweep_lot_claims WHERE status = 'selected'),
    (SELECT min(slot.idle_blocked_since)
     FROM loyal_yield.balance_sweep_targets AS target
     JOIN loyal_yield.balance_sweep_scheduled_slots AS slot
       ON slot.target_id = target.id AND slot.status IN ('scheduled', 'requested')
     JOIN loyal_yield.balance_sweep_wallet_balances_current AS balance
       ON balance.target_id = target.id AND balance.mint = target.token_mint
     WHERE target.token_mint = $1 AND target.desired_active AND target.chain_status = 'active'
       AND slot.idle_blocked_since IS NOT NULL
       AND EXISTS (SELECT 1 FROM loyal_yield.balance_sweep_surplus_lots AS lot
                   WHERE lot.scheduled_slot_id = slot.id AND lot.status = 'open' AND lot.remaining_amount_raw > 0)
       AND target.wallet_balance_floor_raw IS NOT NULL
       AND balance.amount_raw > target.wallet_balance_floor_raw
       AND NOT EXISTS (SELECT 1 FROM loyal_yield.balance_sweep_lot_claims AS owned
                       WHERE owned.target_id = target.id AND owned.status = 'selected'))
  ))::float8, 0)`, USDCMint).Scan(&inflight, &oldest)
	if err != nil {
		return w.failed("autodeposit_progress_check_failed", err)
	}
	w.facts.Inflight(engine.FamilyAutodeposit, inflight)
	w.facts.AutodepositOldestDueAge(oldest)
	return nil
}

func (w *Worker) listen(ctx context.Context, wake chan<- int64) {
	for {
		err := w.listenOnce(ctx, wake)
		if ctx.Err() != nil {
			return
		}
		w.failed("autodeposit_realtime_listener_failed", err)
		timer := time.NewTimer(w.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w *Worker) listenOnce(ctx context.Context, wake chan<- int64) error {
	pooled, err := w.store.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pooled.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = conn.Close(closeCtx)
		cancel()
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+WakeupChannel); err != nil {
		return err
	}
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var hint struct {
			ScheduledSlotID int64 `json:"scheduled_slot_id"`
		}
		if json.Unmarshal([]byte(notification.Payload), &hint) != nil || hint.ScheduledSlotID <= 0 {
			slog.Warn("autodeposit wakeup payload was not a valid scheduled-slot hint")
			continue
		}
		select {
		case wake <- hint.ScheduledSlotID:
		default:
		}
	}
}
