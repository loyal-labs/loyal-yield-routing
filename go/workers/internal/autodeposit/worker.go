package autodeposit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// TargetExecutor is the consumer-defined execution effect for one dispatchable
// target. It carries no financial decisions: which claim to take, how much to
// sweep, and when a wire may be rebroadcast are all owned by this family's
// typed functions and Store SQL. The implementation owns only the effects this
// package cannot do — chain reads, wire construction, broadcast — and reports
// its typed family outcome.
type TargetExecutor interface {
	// Execute resolves one executable target: recovering a claim whose pull
	// already holds custody, or claiming and sweeping a fresh scheduled slot.
	// An unknown outcome never proves completion; with an error it is failed.
	Execute(ctx context.Context, target ExecutableTarget) (ExecutorResult, error)
}

// WorkerDependencies are the worker's explicit dependencies and bounds.
type WorkerDependencies struct {
	// Store is the durable family state. Required.
	Store *Store
	// Executor performs one target's chain effects. Required.
	Executor TargetExecutor
	// Facts receives the family's progress, inflight count and failure codes.
	// Required.
	Facts *engine.Facts
	// FeePayer reads the payer every pass. Required.
	FeePayer FeePayerReader
	// Notifier tells the app a due sweep could not start. Nil disables it.
	Notifier *SweepNotifier

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
	store    *Store
	executor TargetExecutor
	facts    *engine.Facts
	feePayer FeePayerReader
	notifier *SweepNotifier
	// notified holds the slots pushed during the current payer outage. The
	// app dedupes by slot; this only keeps a long outage from re-posting
	// every pass. A restart re-sends, which the app absorbs.
	notified        map[int64]struct{}
	executionErrors int

	pollInterval          time.Duration
	projectionBatchLimit  int64
	dispatchLimit         int64
	staleRequestedSeconds int64
	staleSelectedSeconds  int64
	releaseBatchLimit     int64
}

// Defaults for unset dependency bounds. The poll interval is the Rust
// trigger's: the timeout of the one wakeup loop.
const (
	DefaultPollInterval         = 10 * time.Second
	DefaultProjectionBatchLimit = 500
	DefaultDispatchLimit        = 20
	DefaultReleaseBatchLimit    = 100
)

// FeePayerReader reads the balance of the one account that pays every wire.
type FeePayerReader interface {
	FeePayerLamports(ctx context.Context) (payer string, lamports uint64, err error)
}

// notifyBudget bounds one pass's failed-sweep pushes, so a hung app endpoint
// cannot eat the pass (each push has its own 5 s timeout).
const notifyBudget = 15 * time.Second

// WakeupChannel carries {"scheduled_slot_id": N} for each slot that becomes
// requested (migration 0016's trigger on balance_sweep_scheduled_slots).
const WakeupChannel = "loyal_yield_autodeposit_wakeup"

// NewWorker validates the dependencies and applies the family defaults.
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

// TickReport is what one pass observed and did. It is evidence for callers,
// not control state.
type TickReport struct {
	Projection ProjectionOutcome
	Outcome    ExecutorOutcome
	Dispatched []ExecutableTarget
	Alerts     []ExecutorFailureAlert
	// ExecutorErrors includes errors accompanying a nonfatal family outcome.
	// Such an outcome can retain custody safely without proving runtime health.
	ExecutorErrors int
	// FeePayerExhausted means the pass claimed and sent nothing.
	FeePayerExhausted bool
}

// settled reports a pass that left nothing undone: every due target landed
// or had nothing to act on (a capped allowance or a drained vault).
func (r TickReport) settled() bool {
	o := r.Outcome
	return !r.FeePayerExhausted && o.ExecutionsAttempted == o.ExecutionsCompleted+o.ExecutionsNoop+o.ExecutionsNotActionable
}

// Tick runs one complete pass:
//
//  1. advance the surplus-lot projection (schedule/deplete),
//  2. fail requested slots that were never picked up,
//  3. release selected claims that provably never pulled,
//  4. load executable targets — pull recovery first, fresh sweeps after,
//  5. dispatch each through the executor and record its outcome.
//
// A recovery pass runs even while a target's desired_active is false: custody
// that already left the wallet must be resolved regardless of enablement. The
// error return covers only this pass's own housekeeping failures; executor
// outcomes are tallied, never aborted the scan.
func (w *Worker) Tick(ctx context.Context) (TickReport, error) { return w.tick(ctx, nil) }

// tick dispatches the woken slots first. Each failure is counted under the
// Rust trigger's OperationalError code for the same stage.
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
	if err := w.store.RepairUnsignedSchedules(ctx, w.releaseBatchLimit); err != nil {
		return report, w.failed("autodeposit_execution_queue_preparation_failed", err)
	}

	// One payer pays setup, pull and top-up. It is read once per pass, before
	// any claim: a payer that cannot finish a deposit starts none (04e792e0).
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
		// LoyalFeePayerExhausted on the balance gauge is the one page.
		w.notifyUnstarted(ctx, targets)
		return report, nil
	}
	report.Dispatched = targets
	report.Outcome.TargetsScanned = len(targets)

	report.Alerts = w.dispatch(ctx, targets, &report.Outcome)
	report.ExecutorErrors = w.executionErrors
	return report, nil
}

// notifyUnstarted pushes each due fresh slot the failed sweep the TS executor
// reported on fee-payer exhaustion (6557c221), once per slot per outage; a
// push that did not reach the app is tried again next pass. Claims already
// holding custody are not a promise this pass breaks.
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

// failed counts and logs one stable failure code. Shutdown is not a failure.
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

// dispatch runs the executor over the prioritized target list in order. The
// order is the family's recovery-starvation guarantee: it is preserved here
// even if the executor is slow, because dispatch is synchronous. Each alert
// an outcome deserves is counted under its code.
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

// Run repeats the pass until the context is cancelled. There is one wakeup:
// a requested-slot notification or, failing that, the poll interval. A
// failing pass is not fatal: the next one re-reads the durable state, which is
// the family's recovery model.
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
				// A landing marks progress itself; so does a pass with nothing undone.
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
		// Wakeups already delivered ride the same pass; nothing waits for more.
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

// passFacts reads the inflight count and the age of the oldest deposit the
// family owes, from the rows. The clocks are the Rust overdue check's
// (OVERDUE_AUTODEPOSIT_WORK_SQL): a selected claim's created_at, and an
// idle-blocked slot's first-blocked time. Neither moves on retry, unlike
// eligible_after. Lots that are due but legitimately wait (a capped period
// allowance, an expired delegation) write nothing and are not counted. An
// idle-blocked slot counts only as Rust's did: deferred three times, the
// wallet still above its floor, and no claim owning the target. Lots the
// wallet no longer backs are owed nothing; their September markers read as
// 26 days overdue without the floor.
func (w *Worker) passFacts(ctx context.Context) error {
	var inflight int
	var oldest float64
	err := w.store.pool.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts
   WHERE attempt_state IN ('prepared','submitted','unknown','ambiguous')),
  COALESCE(EXTRACT(EPOCH FROM now() - LEAST(
    (SELECT min(created_at) FROM loyal_yield.balance_sweep_lot_claims WHERE status = 'selected'),
    (SELECT min(to_timestamp(substring(slot.last_error FROM $1)::double precision))
     FROM loyal_yield.balance_sweep_targets AS target
     JOIN loyal_yield.balance_sweep_surplus_lots AS lot
       ON lot.target_id = target.id AND lot.status = 'open' AND lot.remaining_amount_raw > 0
     JOIN loyal_yield.balance_sweep_scheduled_slots AS slot ON slot.id = lot.scheduled_slot_id
     JOIN loyal_yield.balance_sweep_wallet_balances_current AS balance
       ON balance.target_id = target.id AND balance.mint = target.token_mint
     WHERE target.token_mint = $2 AND target.desired_active AND target.chain_status = 'active'
       AND slot.status IN ('scheduled', 'requested') AND slot.last_error LIKE $3
       AND substring(slot.last_error FROM $4)::bigint >= 3
       AND target.wallet_balance_floor_raw IS NOT NULL
       AND balance.amount_raw > target.wallet_balance_floor_raw
       AND NOT EXISTS (SELECT 1 FROM loyal_yield.balance_sweep_lot_claims AS owned
                       WHERE owned.target_id = target.id AND owned.status = 'selected'))
  ))::float8, 0)`, idleBlockedSincePattern,
		USDCMint, idleDeferralPrefix+"%", idleDeferralsPattern).Scan(&inflight, &oldest)
	if err != nil {
		return w.failed("autodeposit_progress_check_failed", err)
	}
	w.facts.Inflight(engine.FamilyAutodeposit, inflight)
	w.facts.AutodepositOldestDueAge(oldest)
	return nil
}

// listen holds one LISTEN session and turns each requested-slot notification
// into a wakeup. A lost session reconnects after the poll interval, which
// alone drives the loop meanwhile. A full wakeup buffer drops the hint: the
// slot is durable and the next poll dispatches it.
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
