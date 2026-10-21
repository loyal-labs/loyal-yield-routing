package backyard

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

const heartbeatInterval = time.Minute

const causeWindow = 10 * time.Minute

type events struct {
	log   *slog.Logger
	facts *engine.Facts
	vault string
	now   func() time.Time

	mu            sync.Mutex
	action        Action
	inflightOpen  bool
	manual        bool
	latchReason   string
	cause         string
	causeAt       time.Time
	snapshot      Snapshot
	withdrawSince time.Time
	heartbeatAt   time.Time

	roomOpenedAt map[string]time.Time
	roomRaw      map[string]uint64
	heldLanes    map[string]bool
}

func newEvents(log *slog.Logger, facts *engine.Facts, vault string) *events {
	if log == nil {
		log = slog.Default()
	}
	return &events{log: log.With("family", string(engine.FamilyBackyard), "vault", vault), facts: facts, vault: vault, now: time.Now}
}

func (e *events) withdrawalHealthStored(h WithdrawalHealth) {
	if e == nil || e.facts == nil || h.Status == "unavailable" {
		return
	}
	e.facts.BackyardWithdrawalHealth(h.RouteKey, h.Status == "operator_attention", h.ObservedAt)
}

type navReport struct {
	at         time.Time
	holdsValue bool
}

func observedNAVReport(s Snapshot) (navReport, bool) {
	if !s.Fresh || !s.valued() {
		return navReport{}, false
	}
	return navReport{at: time.Unix(s.PriorReportUpdatedUnix, 0), holdsValue: s.StrategyNAVRaw > 0 || s.PriorReportedNAVRaw > 0 || s.HasPosition}, true
}

func (e *events) navReported(route string, r navReport) {
	if e == nil || e.facts == nil {
		return
	}
	if r.holdsValue {
		e.facts.BackyardNAVReported(route, r.at)
	} else {
		e.facts.BackyardNAVDrained(route)
	}
}

func (e *events) navReportUnread(err error) {
	if e == nil {
		return
	}
	e.log.Warn("backyard_nav_report_unread", "detail", chain.Scrub(err.Error()))
}

func (e *events) workerStart(owner, manifestSHA256 string) {
	if e == nil {
		return
	}
	e.log.Info("backyard_worker_start", "owner", owner, "manifest_sha256", manifestSHA256)
}

func (e *events) noteCause(detail string) {
	if e == nil || detail == "" {
		return
	}
	e.mu.Lock()
	e.cause, e.causeAt = chain.Scrub(detail), e.now()
	e.mu.Unlock()
}

func (e *events) latched(reason string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	cause := ""
	if e.cause != "" && e.now().Sub(e.causeAt) < causeWindow {
		cause = e.cause
	}
	e.cause, e.manual, e.latchReason = "", true, reason
	e.mu.Unlock()
	e.log.Error("backyard_latched", "code", reason, "cause", cause)
	if e.facts != nil {
		e.facts.Failed(engine.FamilyBackyard, "manual_recovery_latched")
	}
}

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

func (e *events) inflight(open bool) {
	if e == nil || e.facts == nil {
		return
	}
	e.mu.Lock()
	changed := open != e.inflightOpen
	e.inflightOpen = open
	e.mu.Unlock()
	switch {
	case changed && open:
		e.facts.InflightAdd(engine.FamilyBackyard, 1)
	case changed:
		e.facts.InflightAdd(engine.FamilyBackyard, -1)
	}
}

func (e *events) noteAction(action Action) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.action = action
	e.mu.Unlock()
}

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
	if err == nil && !manual && e.facts != nil {
		e.facts.Progress(engine.FamilyBackyard)
		e.facts.LaneSucceeded(engine.FamilyBackyard, e.vault)
	}
	if heartbeat {
		e.log.Info("backyard_heartbeat", "lane", s.RouteLane, "ltv_bps", s.LTVBPS, "nav_raw", s.StrategyNAVRaw, "manual", manual,
			"has_position", s.HasPosition, "target_rule", s.Target.Rule,
			"withdrawal_demand_raw", s.WithdrawalDemandRaw, "voltr_idle_raw", s.VoltrIdleRaw, "withdrawal_waiting_seconds", waiting,
			"fee_accumulator_raw", s.FeeAccumulatorRaw, "lp_supply_incl_fees_raw", s.LPSupplyInclFeesRaw,
			"fee_accumulator_warning", s.MonitorsArmed && s.Nonterminal == "" && approvedVoltrFeeTerms(s) && s.FeeAccumulatorRaw >= 0 && s.LPSupplyInclFeesRaw >= s.FeeAccumulatorRaw && feeAccumulatorNeedsWarning(uint64(s.FeeAccumulatorRaw), uint64(s.LPSupplyInclFeesRaw)))
	}
	if err == nil {
		return
	}
	code := errorCode(err)
	if !fatal {
		e.log.Warn("backyard_tick_deferred", "code", code, "action", string(action), "detail", chain.Scrub(err.Error()))
		return
	}
	e.log.Error("backyard_worker_exit", "code", code, "action", string(action), "hold", holdDetail(err), "detail", chain.Scrub(err.Error()))
	if e.facts != nil {
		e.facts.Failed(engine.FamilyBackyard, code)
	}
}

func (e *events) selectorUnavailable(code string) {
	if e == nil {
		return
	}
	e.log.Warn("backyard_selector_unavailable", "code", code)
}

func (e *events) selectorSampleError(err error) {
	e.selectorSampleFailed(sanitizedSelectorEvaluateFailure(err))
}

func (e *events) selectorSampleFailed(code string) {
	if e == nil || e.facts == nil {
		return
	}
	e.facts.Failed(engine.FamilyBackyard, code)
}

func (e *events) operationFailedAfterSend(action Action, reason, signature string) {
	if e == nil {
		return
	}
	if action == ReportNAV {
		e.log.Info("backyard_operation_failed_after_send", "code", reason, "action", string(action), "signature", signature)
		return
	}
	e.log.Error("backyard_operation_failed_after_send", "code", reason, "action", string(action), "signature", signature)
	if e.facts != nil {
		e.facts.Failed(engine.FamilyBackyard, reason)
	}
}

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

type eventOperationFacts struct {
	action               Action
	amountRaw            int64
	signature, navBase64 string
	reason, lane         string
	broadcast            bool
}

func (d *Database) eventOperation(ctx context.Context, operationID string) (eventOperationFacts, *events, bool) {
	var f eventOperationFacts
	if d == nil || d.pool == nil || len(d.events) == 0 {
		return f, nil, false
	}
	var name, routeKey string
	err := d.pool.QueryRow(ctx, `SELECT route_key, action, COALESCE(transaction_signature,''), broadcast_intent_at IS NOT NULL,
		COALESCE(CASE WHEN expected_effects->'decision'->>'amountRaw' ~ '^[0-9]+$' THEN (expected_effects->'decision'->>'amountRaw')::bigint END, 0),
		COALESCE(expected_effects->'expectedEffects'->'returnData'->>'dataBase64',''),
		COALESCE(expected_effects->'decision'->>'reason',''), COALESCE(strategy_key,'')
		FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&routeKey, &name, &f.signature, &f.broadcast, &f.amountRaw, &f.navBase64, &f.reason, &f.lane)
	f.action = Action(name)
	return f, d.events[routeKey], err == nil
}

func (d *Database) eventFailedAfterSend(ctx context.Context, operationID, reason string) {
	if f, e, ok := d.eventOperation(ctx, operationID); ok && f.broadcast {
		e.operationFailedAfterSend(f.action, reason, f.signature)
	}
}

func (d *Database) eventReconciled(ctx context.Context, operationID string) {
	f, e, ok := d.eventOperation(ctx, operationID)
	if !ok {
		return
	}
	action, amountRaw, signature, navBase64 := f.action, f.amountRaw, f.signature, f.navBase64
	if f.action == OpenRouteStep && f.reason == leverageUpReason {
		e.leverageUpLanded(f.lane, f.amountRaw, f.signature)
	}
	raw, err := base64.StdEncoding.DecodeString(navBase64)
	navKnown := err == nil && len(raw) == 8 && int64(binary.LittleEndian.Uint64(raw)) >= 0
	nav := int64(0)
	if navKnown {
		nav = int64(binary.LittleEndian.Uint64(raw))
	}
	e.operationReconciled(action, amountRaw, signature, nav, navKnown)
}

func holdTransactionError(err error) string {
	var hold *PreSendHold
	if !errors.As(err, &hold) || hold.Details["transactionError"] == "" {
		return ""
	}
	return chain.Scrub(hold.Details["transactionError"])
}

func holdDetail(err error) string {
	var hold *PreSendHold
	if !errors.As(err, &hold) {
		return ""
	}
	if te := holdTransactionError(err); te != "" {
		return chain.Scrub(holdCause(hold) + " transactionError=" + te)
	}
	return chain.Scrub(holdCause(hold))
}

func errorCode(err error) string {
	var hold *PreSendHold
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &hold):
		if cause := holdCause(hold); hold.Details["cause"] != "" {
			return cause
		}
		return string(hold.Reason)
	case errors.Is(err, errConfirmedObservationUnavailable):
		return "confirmed_observation_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &pgErr):
		return "database_" + pgErr.Code
	}
	return "worker_fault"
}

func (e *events) roomObserved(u roomUpdate) {
	if e == nil {
		return
	}
	if e.facts != nil && u.roomPriced {
		e.facts.BackyardLeverageRoom(e.vault, u.watch.lane, u.roomUSD)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.roomOpenedAt == nil {
		e.roomOpenedAt, e.roomRaw = map[string]time.Time{}, map[string]uint64{}
	}
	lane := u.watch.lane
	e.roomRaw[lane] = u.roomRaw
	if !u.watch.belowTarget || !u.open {
		delete(e.roomOpenedAt, lane)
		return
	}
	if _, started := e.roomOpenedAt[lane]; !started {
		e.roomOpenedAt[lane] = u.at
	}
}

func (e *events) leverageObserved(lanes []laneLeverage) {
	if e == nil || e.facts == nil {
		return
	}
	held := map[string]bool{}
	for _, l := range lanes {
		held[l.lane] = true
		e.facts.BackyardLeverageGap(e.vault, l.lane, l.gap, l.targeted)
		e.facts.BackyardTermDebtDays(e.vault, l.lane, l.termDays, l.termOwed)
	}
	e.mu.Lock()
	previous := e.heldLanes
	e.heldLanes = held
	e.mu.Unlock()
	for lane := range previous {
		if !held[lane] {
			e.facts.BackyardLeverageLaneGone(e.vault, lane)
		}
	}
}

func (e *events) leverageUpLanded(lane string, borrowedRaw int64, signature string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	opened, known := e.roomOpenedAt[lane]
	room := e.roomRaw[lane]
	delete(e.roomOpenedAt, lane)
	now := e.now()
	e.mu.Unlock()
	reaction := now.Sub(opened)
	if known {
		e.log.Info("backyard_leverage_up_landed", "lane", lane, "borrowed_raw", borrowedRaw, "room_raw", room, "reaction_seconds", reaction.Seconds(), "signature", signature)
	} else {
		e.log.Info("backyard_leverage_up_landed", "lane", lane, "borrowed_raw", borrowedRaw, "room_raw", room, "reaction_seconds", "unknown", "signature", signature)
	}
	if e.facts != nil {
		e.facts.BackyardLeverageUpLanded(e.vault, lane, reaction, known)
	}
}

func (e *events) hold(lane string, reason HoldReason) {
	if e == nil || e.facts == nil {
		return
	}
	if reason == holdNone {
		e.facts.BackyardHoldCleared(e.vault)
		return
	}
	e.facts.BackyardHold(e.vault, lane, string(reason))
}

func (e *events) stepNoRoom(lane, rule string) {
	if e == nil || e.facts == nil {
		return
	}
	e.facts.BackyardStepNoRoom(e.vault, lane, rule)
}

func (e *events) leverageDecisionOutage(reason string) {
	if e == nil || e.facts == nil {
		return
	}
	e.facts.BackyardLeverageOutage(e.vault, reason)
}
