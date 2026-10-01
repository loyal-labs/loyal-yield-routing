package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// LeverageTarget is the durable B2 option-1 level target for one lane. The
// selector sample loop writes it; Decide reads it through the planning state.
// A target for another lane, or none, keeps the installed behaviour.
type LeverageTarget struct {
	Lane      string    `json:"lane"`
	Level     float64   `json:"level"`
	SpreadBPS int64     `json:"spreadBps"`
	DecidedAt time.Time `json:"decidedAt"`
}

func (t LeverageTarget) validate() error {
	valid := false
	for _, level := range leverageLevels {
		valid = valid || t.Level == level
	}
	if !valid || !leverageLane(t.Lane) || t.DecidedAt.IsZero() {
		return fmt.Errorf("invalid_leverage_target")
	}
	return nil
}

// leverageLane: B2 runs on AUTO and OnRe only; Maple stays as is.
func leverageLane(lane string) bool {
	return lane == autoAUTOPYUSD.Lane || lane == onreONycUSDC
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

// applyLeverageTarget stamps the stored target into the snapshot only for the
// lane it was decided for.
func applyLeverageTarget(s *Snapshot, target *LeverageTarget) {
	s.LeverageTargetLevel = 0
	if target != nil && target.Lane == s.RouteLane && leverageLane(s.RouteLane) {
		s.LeverageTargetLevel = target.Level
	}
}

// LoadLeverageTarget reads the stored target for construction refreshes,
// which merge durable state without a planning read.
func (d *Database) LoadLeverageTarget(ctx context.Context, routeKey string) (*LeverageTarget, error) {
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT COALESCE(state->'leverageTarget','null'::jsonb) FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&raw); err != nil {
		return nil, err
	}
	return decodeLeverageTarget(raw)
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
	tag, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(jsonb_set(state,'{leverageTarget}',$5::jsonb,true),'{generation}',to_jsonb(state_version+1),true),state_version=state_version+1,updated_at=clock_timestamp()
		WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND state_version=$4 AND lease_expires_at>clock_timestamp()
		AND COALESCE(state->'selectorUnwind','null'::jsonb)='null'::jsonb
		AND NOT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`))`,
		routeKey, lease.Owner, lease.FencingToken, expectedVersion, string(raw))
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
}

// decideLeverageTarget applies the option-1 rule to the funded lane. It runs
// only on a settled position: no selector SWITCH/unwind, nothing nonterminal,
// no withdrawal, and the selector itself chose KEEP. Up moves must also beat
// MinimumBenefit after the existing estimated move expense and the shared
// whole-position fee reserve, compared to KEEP gross. Unarmed model fixtures
// retain the old spread projection only. DOWN/no-change never use this gate.
// Cost = moved notional x 2 x UncertaintyBPS + 3 fees of 10,000 raw.
func decideLeverageTarget(s Snapshot, selector SelectorResult, markets []LaneEconomics, p SelectorPolicy) (leverageDecision, bool) {
	out := leverageDecision{Lane: s.RouteLane}
	if !leverageLane(s.RouteLane) || !s.PilotActive || !s.HasPosition || s.PositionCollateralRaw <= 0 ||
		selector.Action != "KEEP" || s.Unwind || s.UnwindRefreshRequired || s.CutoverDrain || s.Nonterminal != "" || s.WithdrawalDemandRaw != 0 ||
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
	positionLevel := min(currentLeverageLevel(s), leverageMaxLiveLevel)
	out.Current = positionLevel
	if s.LeverageTargetLevel > 0 {
		out.Current = min(s.LeverageTargetLevel, leverageMaxLiveLevel)
	}
	spreadAt := func(level float64) (float64, bool) { return leverageSpread(*market, level, equity, positionLevel > 1) }
	// Every spread the rule reads from this level must be available; an
	// unavailable one (e.g. our borrow does not fit the pool's free
	// liquidity) is no decision, never a silent "stay" that rewrites the
	// target.
	for _, step := range leverageWatchOptions["1"] {
		if step.from != out.Current || step.to > leverageMaxLiveLevel {
			continue
		}
		if _, ok := spreadAt(max(step.from, step.to)); !ok {
			return out, false
		}
	}
	out.Next = min(nextLiveLeverageLevel(out.Current, spreadAt), leverageMaxLiveLevel)
	spread, _ := spreadAt(max(out.Current, out.Next))
	out.SpreadBPS = int64(spread * 10_000)
	out.Reason = "spread_rule"
	if out.Next > out.Current {
		moved := float64(equity) * (out.Next - out.Current)
		out.GainRaw = moved * spread * 30 / 365
		out.CostRaw = moved*2*float64(p.UncertaintyBPS)/10_000 + 3*10_000
		if s.MonitorsArmed {
			if !selectorFeeBaselineKnown(s) {
				out.Next, out.Reason = out.Current, "fee_hwm_baseline_unavailable"
				return out, true
			}
			// A pending target or unsupplied collateral is not the shared
			// all-supplied, one-positive-exponential fee-reserve scenario.
			if p.validate() != nil || out.Current != positionLevel || s.CollateralIdleRaw > 0 ||
				!finite(out.CostRaw) || out.CostRaw > 1<<53 || s.PositionDebtValueRaw < 0 ||
				!finite(market.CurrentBorrowAPY) || market.CurrentBorrowAPY < 0 {
				out.Next, out.Reason = out.Current, "fee_forecast_unavailable"
				return out, true
			}
			years := p.Horizon.Hours() / (365.25 * 24)
			collateral, debt := float64(s.PositionCollateralValueRaw), float64(s.PositionDebtValueRaw)
			keepGross := forecastGain(collateral, collateral, debt, *market, math.Log1p(market.CurrentBorrowAPY), years)
			// Preserve the spread rule's projected source rate, including its
			// existing utilization assumptions; do not silently reprice it.
			nextAPR := math.Log1p(market.NativeAPY + market.SupplyAPY - spread)
			candidate := pilotForecastEconomics(pilotEconomics{Debt: debt + moved, Proceeds: moved, APR: nextAPR},
				collateral, *market, years, int64(math.Ceil(out.CostRaw)))
			net, known := selectorFeeReservedGain(s, p.Horizon, candidate, float64(s.TotalVaultNAVRaw)-float64(equity))
			if !known || !finite(keepGross) {
				out.Next, out.Reason = out.Current, "fee_forecast_unavailable"
				return out, true
			}
			out.GainRaw = net - keepGross // fee- and expense-reserved edge
			if out.GainRaw <= float64(p.MinimumBenefitRaw) {
				out.Next, out.Reason = out.Current, "up_move_below_minimum_benefit"
			}
		} else if out.GainRaw <= float64(p.MinimumBenefitRaw)+out.CostRaw {
			out.Next, out.Reason = out.Current, "up_move_below_minimum_benefit"
		}
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

func (d leverageDecision) logLine() string {
	return fmt.Sprintf("backyard-rwa-worker: leverage decision lane=%s %.2fx->%.2fx spread=%.2f gain=%.0f cost=%.0f reason=%s",
		d.Lane, d.Current, d.Next, float64(d.SpreadBPS)/100, d.GainRaw, d.CostRaw, d.Reason)
}

// leverageDecisionLog rate-limits the 'leverage decision' line: it prints
// when the decision changes the stored target, otherwise at most once an
// hour (live 2026-09-28: a 1x->1.5x decision held by blocked borrowing
// printed on every 15 s selector sample).
type leverageDecisionLog struct {
	printed time.Time
}

func (l *leverageDecisionLog) due(now time.Time, d leverageDecision, storedLevel float64) bool {
	if d.Next == d.Current {
		return false
	}
	if d.changesTarget(storedLevel) || now.Sub(l.printed) >= time.Hour {
		l.printed = now
		return true
	}
	return false
}
