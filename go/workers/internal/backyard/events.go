package backyard

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// Backyard writes its operational events as structured slog JSON on stdout,
// which journald ships to ClickStack, and its health as engine facts on
// /metrics. Alert thresholds (LTV, waiting withdrawals, repeated failures)
// are rules over these fields in Alertmanager/ClickStack, not decisions here.
//
// backyardEvents is set once by Engine.Run before any goroutine starts; every
// method is nil-safe, so tests and one-shot commands emit nothing.
var backyardEvents *events

const heartbeatInterval = time.Minute

type events struct {
	log   *slog.Logger
	facts *engine.Facts
	now   func() time.Time

	mu            sync.Mutex
	action        Action
	manual        bool
	latchReason   string
	cause         string
	causeAt       time.Time
	snapshot      Snapshot
	withdrawSince time.Time
	heartbeatAt   time.Time
}

func newEvents(log *slog.Logger, facts *engine.Facts) *events {
	if log == nil {
		log = slog.Default()
	}
	return &events{log: log.With("family", string(engine.FamilyBackyard)), facts: facts, now: time.Now}
}

func (e *events) workerStart(owner, manifestSHA256 string) {
	if e == nil {
		return
	}
	e.log.Info("backyard_worker_start", "lease_owner", owner, "manifest_sha256", manifestSHA256)
}

// noteCause remembers the latest underlying failure so the latch it causes
// can carry it: one slot, last cause wins within ten minutes.
func (e *events) noteCause(detail string) {
	if e == nil || detail == "" {
		return
	}
	e.mu.Lock()
	e.cause, e.causeAt = sanitizedDetail(detail), e.now()
	e.mu.Unlock()
}

// latched reports a newly recorded manual recovery stop.
func (e *events) latched(reason string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	cause := ""
	if e.cause != "" && e.now().Sub(e.causeAt) < 10*time.Minute {
		cause = e.cause
	}
	e.cause, e.manual, e.latchReason = "", true, reason
	e.mu.Unlock()
	e.log.Error("backyard_latched", "code", reason, "cause", cause)
	if e.facts != nil {
		e.facts.Failed(engine.FamilyBackyard, "manual_recovery_latched")
	}
}

// noteLatch is the tick's latch read. Only the operator clear-hold command
// lifts a latch, so a latched -> clear edge is reported here.
func (e *events) noteLatch(latched bool, reason string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	cleared, previous := e.manual && !latched, e.latchReason
	e.manual = latched
	if latched {
		e.latchReason = reason
	}
	e.mu.Unlock()
	if cleared {
		e.log.Info("backyard_latch_cleared", "code", previous)
	}
}

// inflight reports the route's one nonterminal operation, if any.
func (e *events) inflight(open bool) {
	if e == nil || e.facts == nil {
		return
	}
	n := 0
	if open {
		n = 1
	}
	e.facts.Inflight(engine.FamilyBackyard, n)
}

func (e *events) noteAction(action Action) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.action = action
	e.mu.Unlock()
}

// noteSnapshot keeps the latest full confirmed valuation for the heartbeat.
// Health-hold snapshots carry no valuation and are skipped.
func (e *events) noteSnapshot(s Snapshot) {
	if e == nil || !s.Fresh || s.ManualReason != "" {
		return
	}
	e.mu.Lock()
	e.snapshot = s
	if s.WithdrawalDemandRaw > s.VoltrIdleRaw {
		if e.withdrawSince.IsZero() {
			e.withdrawSince = e.now()
		}
	} else {
		e.withdrawSince = time.Time{}
	}
	e.mu.Unlock()
}

// tickResult runs after every tick: progress, the once-a-minute heartbeat with
// the position's valuation fields, and the fatal exit record.
func (e *events) tickResult(err error, fatal bool) {
	if e == nil {
		return
	}
	e.mu.Lock()
	now := e.now()
	heartbeat := now.Sub(e.heartbeatAt) >= heartbeatInterval
	if heartbeat {
		e.heartbeatAt = now
	}
	s, manual, action, waiting := e.snapshot, e.manual, e.action, int64(0)
	if !e.withdrawSince.IsZero() {
		waiting = int64(now.Sub(e.withdrawSince).Seconds())
	}
	e.action = ""
	e.mu.Unlock()
	// A latched route keeps ticking without error; it is stopped, not
	// progressing, so the stale-progress alert pages until clear-hold.
	if err == nil && !manual && e.facts != nil {
		e.facts.Progress(engine.FamilyBackyard)
	}
	if heartbeat {
		// mid_move: an admitted unwind, borrowed cash or withdrawn collateral
		// not yet redeposited legitimately passes the 55% LTV line by design.
		e.log.Info("backyard_heartbeat", "lane", s.RouteLane, "ltv_bps", s.LTVBPS, "nav_raw", s.StrategyNAVRaw, "manual", manual,
			"has_position", s.HasPosition, "mid_move", s.Unwind || debtCashRaw(s) > 0 || s.CollateralIdleRaw > 0,
			"withdrawal_demand_raw", s.WithdrawalDemandRaw, "voltr_idle_raw", s.VoltrIdleRaw, "withdrawal_waiting_seconds", waiting,
			"fee_accumulator_raw", s.FeeAccumulatorRaw, "lp_supply_incl_fees_raw", s.LPSupplyInclFeesRaw,
			"fee_accumulator_warning", s.MonitorsArmed && s.Nonterminal == "" && approvedVoltrFeeTerms(s) && s.FeeAccumulatorRaw >= 0 && s.LPSupplyInclFeesRaw >= s.FeeAccumulatorRaw && feeAccumulatorNeedsWarning(uint64(s.FeeAccumulatorRaw), uint64(s.LPSupplyInclFeesRaw)))
	}
	if err == nil {
		return
	}
	code := errorCode(err)
	if !fatal {
		e.log.Warn("backyard_tick_deferred", "code", code, "action", string(action))
		return
	}
	e.log.Error("backyard_worker_exit", "code", code, "action", string(action), "hold", holdDetail(err), "detail", sanitizedDetail(err.Error()))
	if e.facts != nil {
		e.facts.Failed(engine.FamilyBackyard, code)
	}
}

// selectorUnavailable reports a live selector sample that could not be
// evaluated (change-only); the route keeps its current authority.
func (e *events) selectorUnavailable(code string) {
	if e == nil {
		return
	}
	e.log.Warn("backyard_selector_unavailable", "code", code)
}

// selectorSampleFailed counts every failed live sample. The log line above is
// change-only to keep volume down; the counter is not, so a persistent outage
// keeps failing and pages.
func (e *events) selectorSampleFailed(code string) {
	if e == nil || e.facts == nil {
		return
	}
	e.facts.Failed(engine.FamilyBackyard, code)
}

// operationFailedAfterSend reports a broadcast money operation that ended
// failed or in manual recovery.
func (e *events) operationFailedAfterSend(action Action, reason, signature string) {
	if e == nil {
		return
	}
	// A failed REPORT_NAV is retried by the next tick (mostly blockhash
	// expiry); the nav_reported absence pages if retries stop working, so only
	// fund-moving steps page here.
	if action == ReportNAV {
		e.log.Info("backyard_operation_failed_after_send", "code", reason, "action", string(action), "signature", signature)
		return
	}
	e.log.Error("backyard_operation_failed_after_send", "code", reason, "action", string(action), "signature", signature)
	if e.facts != nil {
		e.facts.Failed(engine.FamilyBackyard, reason)
	}
}

// operationReconciled reports one finalized, reconciled money step; a
// reconciled REPORT_NAV additionally reports the NAV it armed.
func (e *events) operationReconciled(action Action, amountRaw int64, signature string, navRaw int64, navKnown bool) {
	if e == nil {
		return
	}
	e.log.Info("backyard_operation_reconciled", "action", string(action), "amount_raw", amountRaw, "signature", signature)
	if action == ReportNAV && navKnown {
		e.log.Info("backyard_nav_reported", "nav_raw", navRaw, "signature", signature)
	}
	if e.facts != nil {
		e.facts.Landed(engine.FamilyBackyard)
	}
}

// eventOperation reads the facts an operation record carries. Read-only, it
// runs after the state change committed.
func (d *Database) eventOperation(ctx context.Context, operationID string) (action Action, amountRaw int64, signature, navBase64 string, broadcast, ok bool) {
	if backyardEvents == nil || d == nil || d.pool == nil {
		return "", 0, "", "", false, false
	}
	var name string
	err := d.pool.QueryRow(ctx, `SELECT action, COALESCE(transaction_signature,''), broadcast_intent_at IS NOT NULL,
		COALESCE(CASE WHEN expected_effects->'decision'->>'amountRaw' ~ '^[0-9]+$' THEN (expected_effects->'decision'->>'amountRaw')::bigint END, 0),
		COALESCE(expected_effects->'expectedEffects'->'returnData'->>'dataBase64','')
		FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&name, &signature, &broadcast, &amountRaw, &navBase64)
	return Action(name), amountRaw, signature, navBase64, broadcast, err == nil
}

// eventFailedAfterSend reports an operation that ended failed or in manual
// recovery, only if it was broadcast; pre-send refusals are not reported.
func (d *Database) eventFailedAfterSend(ctx context.Context, operationID, reason string) {
	if action, _, signature, _, broadcast, ok := d.eventOperation(ctx, operationID); ok && broadcast {
		backyardEvents.operationFailedAfterSend(action, reason, signature)
	}
}

// eventReconciled reports a reconciled money step and, for REPORT_NAV, the NAV
// its adaptor return data armed.
func (d *Database) eventReconciled(ctx context.Context, operationID string) {
	action, amountRaw, signature, navBase64, _, ok := d.eventOperation(ctx, operationID)
	if !ok {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(navBase64)
	navKnown := err == nil && len(raw) == 8 && int64(binary.LittleEndian.Uint64(raw)) >= 0
	nav := int64(0)
	if navKnown {
		nav = int64(binary.LittleEndian.Uint64(raw))
	}
	backyardEvents.operationReconciled(action, amountRaw, signature, nav, navKnown)
}

var (
	detailURLPattern    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	detailSecretPattern = regexp.MustCompile(`(?i)(password|api[-_]?key|token|secret)=\S+`)
)

// sanitizedDetail removes anything URL- or credential-shaped (RPC/DB URLs can
// carry credentials) and bounds the text.
func sanitizedDetail(text string) string {
	text = detailURLPattern.ReplaceAllString(text, "<url>")
	text = detailSecretPattern.ReplaceAllString(text, "$1=<redacted>")
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

// holdTransactionError is a hold's bounded simulation error, if any.
func holdTransactionError(err error) string {
	var hold *BudgetHold
	if !errors.As(err, &hold) || hold.Details["transactionError"] == "" {
		return ""
	}
	return sanitizedDetail(hold.Details["transactionError"])
}

// holdDetail is a BudgetHold's reason plus its transaction error, if any.
func holdDetail(err error) string {
	var hold *BudgetHold
	if !errors.As(err, &hold) {
		return ""
	}
	if te := holdTransactionError(err); te != "" {
		return sanitizedDetail(hold.Reason + " transactionError=" + te)
	}
	return hold.Reason
}

// errorCode maps a tick error to a closed, stable snake_case code.
func errorCode(err error) string {
	var hold *BudgetHold
	var blocker *RuntimeBlocker
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &hold):
		return hold.Reason
	case errors.As(err, &blocker):
		return blocker.Code
	case errors.Is(err, errConfirmedObservationUnavailable):
		return "confirmed_observation_unavailable"
	case errors.Is(err, ErrRouteLeaseLost):
		return "route_lease_lost"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &pgErr):
		return "database_" + pgErr.Code
	}
	return "worker_fault"
}
