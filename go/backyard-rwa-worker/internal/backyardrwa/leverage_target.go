package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
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
// MinimumBenefit plus an estimated move cost (Q2): gain = equity x (next -
// current) x spread(next) x 30/365; cost = moved notional x 2 x UncertaintyBPS
// + 3 transaction fees of 10,000 raw.
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
	out.Current = currentLeverageLevel(s)
	spreadAt := func(level float64) (float64, bool) { return leverageSpread(*market, level, equity, out.Current > 1) }
	out.Next = nextLiveLeverageLevel(out.Current, spreadAt)
	spread, _ := spreadAt(max(out.Current, out.Next))
	out.SpreadBPS = int64(spread * 10_000)
	out.Reason = "spread_rule"
	if out.Next > out.Current {
		moved := float64(equity) * (out.Next - out.Current)
		out.GainRaw = moved * spread * 30 / 365
		out.CostRaw = moved*2*float64(p.UncertaintyBPS)/10_000 + 3*10_000
		if out.GainRaw <= float64(p.MinimumBenefitRaw)+out.CostRaw {
			out.Next, out.Reason = out.Current, "up_move_below_minimum_benefit"
		}
	}
	return out, true
}

func (d leverageDecision) logLine() string {
	return fmt.Sprintf("backyard-rwa-worker: leverage decision lane=%s %.2fx->%.2fx spread=%.2f gain=%.0f cost=%.0f reason=%s",
		d.Lane, d.Current, d.Next, float64(d.SpreadBPS)/100, d.GainRaw, d.CostRaw, d.Reason)
}
