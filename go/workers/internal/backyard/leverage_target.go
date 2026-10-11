package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// LeverageTarget is the durable B2 option-1 level target for one lane. The
// selector sample loop writes it; Decide reads it through the planning state.
// Route state keeps one per lane under 'leverageTargets', keyed by lane; a
// lane without one keeps the installed behaviour.
type LeverageTarget struct {
	Lane          string    `json:"lane"`
	Level         float64   `json:"level"`
	SpreadBPS     int64     `json:"spreadBps"`
	DecidedAt     time.Time `json:"decidedAt"`
	BorrowRaw     uint64    `json:"borrowRaw,omitempty"`
	SourceDebtRaw uint64    `json:"sourceDebtRaw,omitempty"`
	OperationID   string    `json:"operationId,omitempty"`
}

func (t LeverageTarget) validate() error {
	valid := false
	for _, level := range leverageLevels {
		valid = valid || t.Level == level
	}
	if !valid || !earnActiveLane(t.Lane) || t.DecidedAt.IsZero() || t.SourceDebtRaw > math.MaxInt64 || t.BorrowRaw > math.MaxInt64 ||
		(t.BorrowRaw != 0 && (t.Level <= 1 || t.BorrowRaw < leverageMinimumBorrowRaw)) {
		return fmt.Errorf("invalid_leverage_target")
	}
	return nil
}

func decodeLeverageTarget(raw []byte) (*LeverageTarget, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var target LeverageTarget
	if json.Unmarshal(raw, &target) != nil || target.validate() != nil {
		return nil, budgetHold("invalid_durable_leverage_target")
	}
	return &target, nil
}

// decodeLeverageTargets decodes route state 'leverageTargets': one valid
// target per lane, each stored under its own lane.
func decodeLeverageTargets(raw []byte) (map[string]LeverageTarget, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var targets map[string]LeverageTarget
	if json.Unmarshal(raw, &targets) != nil {
		return nil, budgetHold("invalid_durable_leverage_target")
	}
	for lane, target := range targets {
		if target.Lane != lane || target.validate() != nil {
			return nil, budgetHold("invalid_durable_leverage_target")
		}
	}
	return targets, nil
}

// leverageTargetFor is the lane's stored target, nil when it has none.
func leverageTargetFor(targets map[string]LeverageTarget, lane string) *LeverageTarget {
	target, ok := targets[lane]
	if !ok {
		return nil
	}
	return &target
}

// setLeverageTargetSQL replaces one lane's entry in 'leverageTargets' ($lane
// text, $target jsonb), keeping every other lane's target.
func setLeverageTargetSQL(lane, target string) string {
	return `jsonb_set(state,'{leverageTargets}',COALESCE(state->'leverageTargets','{}'::jsonb)||jsonb_build_object(` + lane + `::text,` + target + `::jsonb),true)`
}

// applyLeverageTarget stamps the stored target into the snapshot only for the
// lane it was decided for.
func applyLeverageTarget(s *Snapshot, target *LeverageTarget) {
	s.LeverageTargetLevel = 0
	s.LeverageApprovedBorrowRaw, s.LeverageSourceDebtRaw = 0, 0
	s.LeverageBorrowOperationID = ""
	if target != nil && target.Lane == s.RouteLane && earnActiveLane(s.RouteLane) {
		s.LeverageTargetLevel = target.Level
		s.LeverageApprovedBorrowRaw, s.LeverageSourceDebtRaw = target.BorrowRaw, target.SourceDebtRaw
		s.LeverageBorrowOperationID = target.OperationID
	}
}

// LoadLeverageTarget reads one lane's stored target for construction
// refreshes, which merge durable state without a planning read.
func (d *Database) LoadLeverageTarget(ctx context.Context, routeKey, lane string) (*LeverageTarget, error) {
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT COALESCE(state->'leverageTargets','null'::jsonb) FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&raw); err != nil {
		return nil, err
	}
	targets, err := decodeLeverageTargets(raw)
	if err != nil {
		return nil, err
	}
	return leverageTargetFor(targets, lane), nil
}

// RecordLeverageTarget stores a new target under the route fence and the
// generation read with the observation it was decided from. It changes no
// money and no selector state; a lost fence or a newer generation refuses.
func (d *Database) RecordLeverageTarget(ctx context.Context, routeKey string, target LeverageTarget, expectedVersion int64) error {
	if err := target.validate(); err != nil {
		return err
	}
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	if lease.RouteKey != routeKey {
		return fmt.Errorf("leverage_route_lease_mismatch")
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return err
	}
	tag, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(`+setLeverageTargetSQL("$6", "$5")+`,'{generation}',to_jsonb(state_version+1),true),state_version=state_version+1,updated_at=clock_timestamp()
		WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND state_version=$4 AND lease_expires_at>clock_timestamp()
		AND COALESCE(state->'selectorUnwind','null'::jsonb)='null'::jsonb
		AND NOT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`))`,
		routeKey, lease.Owner, lease.FencingToken, expectedVersion, string(raw), target.Lane)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return budgetHold("leverage_target_state_changed")
	}
	return nil
}

// leverageDecision is one B2 option-1 level decision for the funded lane.
type leverageDecision struct {
	Lane          string
	Current, Next float64
	SpreadBPS     int64
	GainRaw       float64
	CostRaw       float64
	Reason        string
	BorrowRaw     uint64
	SourceDebtRaw uint64
	// RoomRaw is the debt reserve's additional debt room this sample.
	RoomRaw uint64
	// Cost is the priced step's expected cost (CostRaw is its total).
	Cost leverageStepCost
}

// decideLeverageTarget applies the option-1 rule to the funded lane. It runs
// only on a settled position: no selector SWITCH/unwind, nothing nonterminal,
// no withdrawal, and the selector itself chose KEEP. An up move needs the
// level's spread (1% to 1.5x, 2% to 1.75x) and a positive gain over KEEP
// after the move's own cost and the performance fee on the extra income. It
// does not need the selector's MinimumBenefit, which is SWITCH hysteresis
// against a full unwind, not a cost of borrowing more in place.
// DOWN/no-change never use this gate.
//
// The move is sized at the whole step to the next level (desiredLeverage
// Borrow), not at the debt reserve's momentary room, so the approval stands
// while the reserve is full. Its rate is the curve at the step's utilization,
// capped at the reserve's utilization borrow limit (leverageRateDebtRaw): no
// executable part of the step borrows above it. The worker borrows this tick's capacity-sized part of it
// the moment room opens (leverageBorrowReceive).
//
// Borrowing more in place changes the same vault's income by the increment
// only, so the gain compares like with like: the candidate's and KEEP's
// gross income over the horizon, the full performance fee on a positive
// difference (a vault below its high-water mark pays less, never more), then
// the move's cost. The selector's SWITCH forecast instead reserves fees on
// the whole position's income against KEEP's fee-favoured upper bound; on a
// $100k position that asymmetry alone outweighed any in-place borrow the
// debt reserve could fund (live 2026-10-10: AUTO held 1.01x under a 1.75x
// target while PYUSD had $3k-$5k of room for five hours).
//
// The cost is the step's expected cost (leverage_step_cost.go): the KLend
// origination fee, the expected swap loss in and out at the step's size from
// live quotes, and the six transactions' network fees, all at central
// prices. It was 2 x UncertaintyBPS of the borrowed amount: a haircut that
// bounds execution, not what capital loses, and at AUTO's realized spread
// (~2.9 pts) it refused every up step whatever the room. Uncertainty stays
// where it bounds execution: the swap minimums and the recipe limits. The
// income forecast values the borrow and its proceeds at central prices too.
// An unpriced step is leverage_cost_unavailable, never free.
func decideLeverageTarget(s Snapshot, selector SelectorResult, markets []LaneEconomics, p SelectorPolicy, price leverageStepPricer) (leverageDecision, bool) {
	out := leverageDecision{Lane: s.RouteLane, RoomRaw: s.AdditionalDebtRoomRaw}
	if !earnActiveLane(s.RouteLane) || !s.HasPosition || s.PositionCollateralRaw <= 0 ||
		selector.Action != "KEEP" || s.PartialWithdrawalOperationID != "" || s.Unwind || s.UnwindRefreshRequired || s.CutoverDrain || s.Nonterminal != "" || s.WithdrawalDemandRaw != 0 ||
		s.SquadsIdleRaw != 0 || s.DebtIdleRaw != 0 || s.VoltrStrategyIdleRaw != 0 ||
		(s.CollateralIdleRaw > 0 && s.MinimumCollateralDepositRaw > 0 && s.CollateralIdleRaw >= s.MinimumCollateralDepositRaw) {
		return out, false
	}
	var market *LaneEconomics
	for i := range markets {
		if markets[i].Lane == s.RouteLane {
			market = &markets[i]
		}
	}
	equity := s.PositionCollateralValueRaw - s.PositionDebtValueRaw
	if market == nil || equity <= 0 {
		return out, false
	}
	// The rule steps from the STORED target, not the position: a target the
	// position has not reached yet (borrowing blocked) is still the decided
	// level, so re-deciding from the position flipped it (live 2026-09-28:
	// 1.5x <-> 1x writes, each bumping the generation). Without a stored
	// target the position level is the base. Live levels stop at
	// leverageMaxLiveLevel (see leverage_up.go).
	positionLevel := min(currentLeverageBand(s), leverageMaxLiveLevel)
	out.Current = positionLevel
	if s.LeverageTargetLevel > 0 {
		out.Current = min(s.LeverageTargetLevel, leverageMaxLiveLevel)
	}
	// Down rules use current borrowing cost even when new capacity is unknown.
	currentSpread := market.NativeAPY + market.SupplyAPY - market.CurrentBorrowAPY
	spreadAt := func(level float64) (float64, bool) {
		if level <= out.Current {
			return currentSpread, finite(currentSpread) && market.CurrentBorrowAPY >= 0
		}
		raw := leverageDesiredRaw(s, level)
		if raw < leverageMinimumBorrowRaw {
			return 0, false
		}
		fee, err := kaminoBorrowFeeAtRate(s.BorrowFeeRate, raw)
		if err != nil {
			return 0, false
		}
		return leverageBorrowSpread(*market, float64(leverageRateDebtRaw(s, raw+fee)))
	}
	out.Next = min(nextLiveLeverageLevel(out.Current, spreadAt), leverageMaxLiveLevel)
	out.SpreadBPS = int64(currentSpread * 10_000)
	out.Reason = "spread_rule"
	if out.Next < out.Current {
		return out, true
	}
	// A stored desired ceiling may be above the actual position. Price the
	// whole remaining step, including its entire fee/cost.
	candidateSnapshot := s
	candidateSnapshot.LeverageTargetLevel = out.Next
	level := leverageUpLevel(candidateSnapshot)
	raw := leverageDesiredRaw(s, level)
	if raw < leverageMinimumBorrowRaw {
		if level > 0 && !s.BorrowCapacityKnown {
			out.Reason = "borrow_capacity_unknown"
		} else if level > 0 {
			out.Reason = "leverage_step_below_minimum"
		}
		return out, true
	}
	fee, err := kaminoBorrowFeeAtRate(s.BorrowFeeRate, raw)
	proceeds, priceErr := capacityBorrowCentralValue(s, raw, false)
	liability, debtErr := capacityBorrowCentralValue(s, raw+fee, true)
	rateDebt := float64(leverageRateDebtRaw(s, raw+fee))
	apr, rateErr := projectedBorrowAPR(*market, rateDebt)
	spread, allowed := leverageUpAllowed(*market, level, rateDebt)
	if err != nil || priceErr != nil || debtErr != nil || rateErr != nil || !allowed {
		out.Next = out.Current
		return out, true
	}
	out.SpreadBPS = int64(spread * 10_000)
	moved := float64(proceeds)
	if !s.MonitorsArmed || !approvedVoltrFeeTerms(s) {
		out.Next, out.Reason = out.Current, "fee_terms_unavailable"
		return out, true
	}
	if p.validate() != nil || s.CollateralIdleRaw > 0 || s.PositionDebtValueRaw < 0 || !finite(market.CurrentBorrowAPY) || market.CurrentBorrowAPY < 0 {
		out.Next, out.Reason = out.Current, "fee_forecast_unavailable"
		return out, true
	}
	if price == nil {
		out.Next, out.Reason = out.Current, "leverage_cost_unavailable"
		return out, true
	}
	stepCost, costErr := price(raw)
	if costErr != nil || stepCost.TotalMicros <= 0 || stepCost.TotalMicros > 1<<53 {
		out.Next, out.Reason = out.Current, "leverage_cost_unavailable"
		return out, true
	}
	out.Cost, out.CostRaw = stepCost, float64(stepCost.TotalMicros)
	years := p.Horizon.Hours() / (365.25 * 24)
	collateral, debt := float64(s.PositionCollateralValueRaw), float64(s.PositionDebtValueRaw)
	keepGross := forecastGain(collateral, collateral, debt, *market, math.Log1p(market.CurrentBorrowAPY), years)
	// The held collateral is not reduced by the move; its cost is. Pass the
	// collateral the move leaves earning, net of that cost; the candidate's
	// income is its gain before the cost is charged.
	cost := math.Ceil(out.CostRaw)
	candidate := pilotForecastEconomics(pilotEconomics{Debt: debt + float64(liability), Proceeds: moved, APR: apr}, collateral-cost, *market, years, int64(cost))
	extra := candidate.Gain + cost - keepGross
	if !finite(extra) || !finite(keepGross) {
		out.Next, out.Reason = out.Current, "fee_forecast_unavailable"
		return out, true
	}
	if extra > 0 {
		extra *= 1 - float64(approvedAdminPerformanceFeeBPS)/10_000
	}
	out.GainRaw = extra - cost
	if out.GainRaw <= 0 {
		out.Next, out.Reason = out.Current, "up_move_not_worth_cost"
	} else {
		out.Reason = "up_move_approved"
		out.BorrowRaw, out.SourceDebtRaw = raw, uint64(s.PositionDebtRaw)
	}
	return out, true
}

// changesTarget reports whether storing d would change the stored target in
// effect (a stored level above the live cap counts as the cap).
func (d leverageDecision) changesTarget(stored float64) bool {
	if stored > 0 {
		stored = min(stored, leverageMaxLiveLevel)
	}
	return d.Next != stored
}

// storeChanges reports whether d must be written over the stored target:
// a new level, a consumed approval, an approval granted or revoked, one
// decided from a different debt, or one whose size moved by more than a
// tenth. The approved size drifts with prices every sample; rewriting it
// each time would bump the route generation every 15 s and refuse every
// other fenced write.
func (d leverageDecision) storeChanges(s Snapshot) bool {
	approved, stored := d.BorrowRaw >= leverageMinimumBorrowRaw, s.LeverageApprovedBorrowRaw >= leverageMinimumBorrowRaw
	if d.changesTarget(s.LeverageTargetLevel) || s.LeverageBorrowOperationID != "" || approved != stored {
		return true
	}
	if !approved {
		return s.LeverageApprovedBorrowRaw != 0 || s.LeverageSourceDebtRaw != 0
	}
	drift := max(d.BorrowRaw, s.LeverageApprovedBorrowRaw) - min(d.BorrowRaw, s.LeverageApprovedBorrowRaw)
	return !borrowDebtMatches(d.SourceDebtRaw, s.LeverageSourceDebtRaw) || drift > s.LeverageApprovedBorrowRaw/10
}

func (d leverageDecision) logLine() string {
	return fmt.Sprintf("backyard-rwa-worker: leverage decision lane=%s %.2fx->%.2fx spread=%.2f gain=%.0f cost=%.0f (fee=%d in=%d out=%d network=%d) borrow=%d room=%d reason=%s",
		d.Lane, d.Current, d.Next, float64(d.SpreadBPS)/100, d.GainRaw, d.CostRaw, d.Cost.BorrowFeeMicros, d.Cost.EntrySwapLossMicros, d.Cost.ExitSwapLossMicros, d.Cost.NetworkMicros, d.BorrowRaw, d.RoomRaw, d.Reason)
}

// leverageDecisionLog rate-limits the 'leverage decision' line: it prints
// when the outcome (level, reason, approved borrow) changes, otherwise at
// most once an hour (live 2026-09-28: a 1x->1.5x decision held by blocked
// borrowing printed on every 15 s selector sample). Holds print too: the
// stored target is the cap, so an approved or refused up move keeps
// Next == Current, and keying on the level alone hid every one of them
// (live 2026-10-10).
type leverageDecisionLog struct {
	printed time.Time
	last    leverageDecisionKey
}

// The key leaves out the drifting numbers (approved size, room); whether an
// approval and room exist are in it.
type leverageDecisionKey struct {
	next     float64
	reason   string
	approved bool
	room     bool
}

func (l *leverageDecisionLog) due(now time.Time, d leverageDecision) bool {
	key := leverageDecisionKey{d.Next, d.Reason, d.BorrowRaw > 0, d.RoomRaw >= leverageMinimumBorrowRaw}
	if key != l.last || now.Sub(l.printed) >= time.Hour {
		l.printed, l.last = now, key
		return true
	}
	return false
}
