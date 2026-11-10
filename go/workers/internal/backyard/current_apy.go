package backyard

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

type CurrentAPY struct {
	Lane       string        `json:"lane"`
	APYBPS     int64         `json:"apyBps"`
	Level      float64       `json:"level"`
	ObservedAt time.Time     `json:"observedAt"`
	Flat       bool          `json:"flat"`
	Portfolio  *PortfolioAPY `json:"portfolio"`
}

type PortfolioAPY struct {
	APYBPS        int64                  `json:"apyBps"`
	TotalValueRaw int64                  `json:"totalValueRaw"`
	IdleUSDCRaw   int64                  `json:"idleUsdcRaw"`
	Positions     []PortfolioAPYPosition `json:"positions" nullable:"false"`
}

type PortfolioAPYPosition struct {
	Lane               string `json:"lane"`
	CollateralValueRaw int64  `json:"collateralValueRaw"`
	DebtValueRaw       int64  `json:"debtValueRaw"`
	EquityRaw          int64  `json:"equityRaw"`
	APYBPS             *int64 `json:"apyBps"`
}

func currentPortfolioAPY(observed Observation, markets []LaneEconomics, now time.Time) *PortfolioAPY {
	s := observed.Snapshot
	if !s.netKnown(now) || !approvedVoltrFeeTerms(s) || s.TotalVaultNAVRaw <= 0 || s.TotalVaultNAVRaw > 1<<53 || len(observed.lanes) == 0 {
		return nil
	}
	out := &PortfolioAPY{TotalValueRaw: s.TotalVaultNAVRaw, IdleUSDCRaw: s.VoltrIdleRaw + s.VoltrStrategyIdleRaw + s.SquadsIdleRaw, Positions: []PortfolioAPYPosition{}}
	seen, custody := map[string]bool{}, map[string]bool{}
	var annualIncome float64
	for _, lane := range observed.lanes {
		l, ok := findEarnLane(lane.RouteLane)
		if !ok || seen[lane.RouteLane] || lane.Slot != s.Slot || !lane.netKnown(now) {
			return nil
		}
		seen[lane.RouteLane] = true
		for _, value := range []int64{lane.PositionCollateralValueRaw, lane.CollateralIdleValueRaw, lane.PositionDebtValueRaw} {
			if value < 0 || value > 1<<53 {
				return nil
			}
		}
		idle := lane.CollateralIdleValueRaw
		if custody[l.route.Kamino.CollateralMint] {
			idle = 0
		}
		custody[l.route.Kamino.CollateralMint] = true
		collateral := lane.PositionCollateralValueRaw + idle
		if collateral == 0 && lane.PositionDebtValueRaw == 0 {
			continue
		}
		m := economicsOf(markets, lane.RouteLane)
		if m == nil || !finite(m.NativeAPY) || !finite(m.SupplyAPY) ||
			lane.PositionDebtValueRaw > 0 && (!finite(m.CurrentBorrowAPY) || m.CurrentBorrowAPY < 0) {
			return nil
		}
		income := float64(collateral)*m.NativeAPY + float64(lane.PositionCollateralValueRaw)*m.SupplyAPY
		if lane.PositionDebtValueRaw > 0 {
			income -= float64(lane.PositionDebtValueRaw) * m.CurrentBorrowAPY
		}
		annualIncome += income
		position := PortfolioAPYPosition{Lane: lane.RouteLane, CollateralValueRaw: collateral, DebtValueRaw: lane.PositionDebtValueRaw, EquityRaw: collateral - lane.PositionDebtValueRaw}
		if position.EquityRaw > 0 {
			bps := int64(math.Round(performanceFeeForecast(income/float64(position.EquityRaw), s.ApprovedAdminPerformanceFeeBPS) * 10_000))
			position.APYBPS = &bps
		}
		out.Positions = append(out.Positions, position)
	}
	for _, lane := range s.heldLanes() {
		if !seen[lane] {
			return nil
		}
	}
	apy := performanceFeeForecast(annualIncome/float64(s.TotalVaultNAVRaw), s.ApprovedAdminPerformanceFeeBPS)
	if !finite(apy) {
		return nil
	}
	out.APYBPS = int64(math.Round(apy * 10_000))
	sort.Slice(out.Positions, func(i, j int) bool { return out.Positions[i].Lane < out.Positions[j].Lane })
	return out
}

func currentPositionAPY(s Snapshot, markets []LaneEconomics) (CurrentAPY, bool) {
	out := CurrentAPY{Lane: s.RouteLane}
	collateral := float64(s.PositionCollateralValueRaw) + float64(max(s.CollateralIdleValueRaw, 0))
	equityRaw := s.PositionCollateralValueRaw + max(s.CollateralIdleValueRaw, 0) - s.PositionDebtValueRaw
	if !s.HasPosition || s.PositionCollateralValueRaw <= 0 || equityRaw <= 0 {
		out.Flat = true
		return out, true
	}
	market := economicsOf(markets, s.RouteLane)
	if market == nil {
		return out, false
	}
	actual := collateral / float64(equityRaw)
	if !finite(market.CurrentBorrowAPY) || market.CurrentBorrowAPY < 0 || s.PositionDebtValueRaw < 0 {
		return out, false
	}
	apy := performanceFeeForecast((collateral*market.NativeAPY+float64(s.PositionCollateralValueRaw)*market.SupplyAPY-float64(s.PositionDebtValueRaw)*market.CurrentBorrowAPY)/float64(equityRaw), s.ApprovedAdminPerformanceFeeBPS)
	if !finite(apy) {
		return out, false
	}
	out.APYBPS = int64(math.Round(apy * 10_000))
	out.Level = math.Round(actual*100) / 100
	return out, true
}

type currentAPYThrottle struct {
	last    CurrentAPY
	written time.Time
}

const (
	currentAPYMinChangeBPS = 5
	currentAPYMaxAge       = 10 * time.Minute
)

func (t *currentAPYThrottle) due(now time.Time, next CurrentAPY) bool {
	if t.written.IsZero() || next.Flat != t.last.Flat || next.Lane != t.last.Lane || now.Sub(t.written) >= currentAPYMaxAge {
		return true
	}
	if (next.Portfolio == nil) != (t.last.Portfolio == nil) {
		return true
	}
	if p, old := next.Portfolio, t.last.Portfolio; p != nil && old != nil {
		if math.Abs(float64(p.APYBPS-old.APYBPS)) >= currentAPYMinChangeBPS || len(p.Positions) != len(old.Positions) || math.Abs(float64(p.IdleUSDCRaw-old.IdleUSDCRaw)) >= 1e6 {
			return true
		}
		for i, position := range p.Positions {
			prior := old.Positions[i]
			if position.Lane != prior.Lane || math.Abs(float64(position.EquityRaw-prior.EquityRaw)) >= 1e6 || math.Abs(float64(position.DebtValueRaw-prior.DebtValueRaw)) >= 1e6 {
				return true
			}
		}
	}
	if next.Flat {
		return false
	}
	change := next.APYBPS - t.last.APYBPS
	return change >= currentAPYMinChangeBPS || change <= -currentAPYMinChangeBPS
}

func (t *currentAPYThrottle) wrote(now time.Time, value CurrentAPY) {
	t.last, t.written = value, now
}

func (d *Database) RecordCurrentAPY(ctx context.Context, routeKey string, value CurrentAPY) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if value.Flat {
		_, err = d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{currentApy}',COALESCE(state->'currentApy','{}'::jsonb) || jsonb_build_object('flat',true,'observedAt',$2::jsonb->'observedAt','portfolio',$2::jsonb->'portfolio'),true),updated_at=clock_timestamp() WHERE route_key=$1`, routeKey, string(raw))
		return err
	}
	_, err = d.pool.Exec(ctx, `WITH route AS (
  UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{currentApy}',$2::jsonb,true),updated_at=clock_timestamp() WHERE route_key=$1 RETURNING route_key)
INSERT INTO loyal_yield.multiply_route_current_apy_days (route_key, day, apy_bps, observed_at)
SELECT route_key, ($3::timestamptz AT TIME ZONE 'America/Los_Angeles')::date, $4, $3 FROM route
ON CONFLICT (route_key, day) DO UPDATE SET apy_bps=EXCLUDED.apy_bps, observed_at=EXCLUDED.observed_at
WHERE multiply_route_current_apy_days.observed_at <= EXCLUDED.observed_at`, routeKey, string(raw), value.ObservedAt, value.APYBPS)
	return err
}

func publishCurrentAPY(ctx context.Context, now time.Time, throttle *currentAPYThrottle, observed Observation, markets []LaneEconomics, write func(context.Context, CurrentAPY) error, logf func(string, ...any)) {
	if observed.planning == nil || !earnHeldLane(observed.Snapshot.RouteLane) {
		return
	}
	value, ok := currentPositionAPY(observed.Snapshot, markets)
	if !ok {
		return
	}
	value.ObservedAt = now.UTC()
	value.Portfolio = currentPortfolioAPY(observed, markets, now)
	if !throttle.due(now, value) {
		return
	}
	if err := write(ctx, value); err != nil {
		logf("backyard-rwa-worker: current APY not stored: %s\n", sanitizedSelectorEvaluateFailure(err))
		return
	}
	throttle.wrote(now, value)
}

type LeverageWatchSummary struct {
	ObservedAt time.Time           `json:"observedAt"`
	Lanes      []leverageWatchLane `json:"lanes"`
}

func mergeLeverageWatch(previous *LeverageWatchSummary, next LeverageWatchSummary) LeverageWatchSummary {
	out := LeverageWatchSummary{ObservedAt: next.ObservedAt}
	seen := map[string]bool{}
	for _, lane := range next.Lanes {
		seen[lane.Lane] = true
		out.Lanes = append(out.Lanes, lane)
	}
	if previous != nil {
		for _, lane := range previous.Lanes {
			if !seen[lane.Lane] {
				out.Lanes = append(out.Lanes, lane)
			}
		}
	}
	sort.Slice(out.Lanes, func(i, j int) bool { return out.Lanes[i].Lane < out.Lanes[j].Lane })
	return out
}

func (d *Database) RecordLeverageWatch(ctx context.Context, routeKey string, summary LeverageWatchSummary) error {
	return d.inTx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT COALESCE(state->'leverageWatch','null'::jsonb) FROM loyal_yield.multiply_route_states WHERE route_key=$1 FOR UPDATE`, routeKey).Scan(&raw); err != nil {
			return err
		}
		var previous *LeverageWatchSummary
		if len(raw) > 0 && string(raw) != "null" {
			var stored LeverageWatchSummary
			if json.Unmarshal(raw, &stored) == nil {
				previous = &stored
			}
		}
		merged, err := json.Marshal(mergeLeverageWatch(previous, summary))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{leverageWatch}',$2::jsonb,true),updated_at=clock_timestamp() WHERE route_key=$1`, routeKey, string(merged))
		return err
	})
}

func publishLeverageWatch(ctx context.Context, now time.Time, watch *leverageWatch, observed Observation, write func(context.Context, LeverageWatchSummary) error, logf func(string, ...any)) {
	if watch == nil || len(watch.summary) == 0 || observed.planning == nil {
		return
	}
	summary := LeverageWatchSummary{ObservedAt: now.UTC()}
	for _, lane := range watch.summary {
		lane.ObservedAt = now.UTC()
		summary.Lanes = append(summary.Lanes, lane)
	}
	if err := write(ctx, summary); err != nil {
		logf("backyard-rwa-worker: leverage watch not stored: %s\n", sanitizedSelectorEvaluateFailure(err))
	}
}
