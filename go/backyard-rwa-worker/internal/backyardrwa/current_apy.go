package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// CurrentAPY is the vault position's current net APY, published for the web
// app in multiply_route_states.state.currentApy. It is display data only:
// no money path, admission or selector decision reads it.
type CurrentAPY struct {
	Lane       string    `json:"lane"`
	APYBPS     int64     `json:"apyBps"`
	Level      float64   `json:"level"`
	ObservedAt time.Time `json:"observedAt"`
	Flat       bool      `json:"flat"`
}

// currentPositionAPY publishes the leverage watch's own APY figure for the
// route lane at the position's level (collateral/equity snapped to the
// nearest watch level: 1, 1.5, 1.75), from the same LaneEconomics the
// selector sample loads (no RPC). ok=false: flat, or no economics for the
// lane. Level is the actual collateral/equity (2 decimals).
func currentPositionAPY(s Snapshot, markets []LaneEconomics) (CurrentAPY, bool) {
	out := CurrentAPY{Lane: s.RouteLane}
	collateral := float64(s.PositionCollateralValueRaw) + float64(max(s.CollateralIdleValueRaw, 0))
	equityRaw := s.PositionCollateralValueRaw + max(s.CollateralIdleValueRaw, 0) - s.PositionDebtValueRaw
	if !s.HasPosition || s.PositionCollateralValueRaw <= 0 || equityRaw <= 0 {
		out.Flat = true
		return out, false
	}
	var market *LaneEconomics
	for i := range markets {
		if markets[i].Lane == s.RouteLane {
			market = &markets[i]
		}
	}
	if market == nil {
		return out, false
	}
	actual := collateral / float64(equityRaw)
	level := leverageLevels[0]
	for _, candidate := range leverageLevels {
		if math.Abs(candidate-actual) < math.Abs(level-actual) {
			level = candidate
		}
	}
	apy, ok := leverageLevelAPY(*market, level, equityRaw, true)
	if !ok || !finite(apy) {
		return out, false
	}
	out.APYBPS = int64(math.Round(apy * 10_000))
	out.Level = math.Round(actual*100) / 100
	return out, true
}

// currentAPYThrottle limits writes: an APY change of >= 5 bps, a flat
// change, or 10 minutes since the last write.
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
	if next.Flat {
		return false
	}
	change := next.APYBPS - t.last.APYBPS
	return change >= currentAPYMinChangeBPS || change <= -currentAPYMinChangeBPS
}

func (t *currentAPYThrottle) wrote(now time.Time, value CurrentAPY) {
	t.last, t.written = value, now
}

// RecordCurrentAPY stores the published APY under the route lease and the
// state version read with the observation. It changes no money and does NOT
// advance the generation (display data must not invalidate in-flight
// quotes or custody proofs). A flat position only sets flat=true and keeps
// the last APY. A lost fence or a newer state refuses.
func (d *Database) RecordCurrentAPY(ctx context.Context, routeKey string, value CurrentAPY, expectedVersion int64) error {
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	if lease.RouteKey != routeKey {
		return fmt.Errorf("current_apy_route_lease_mismatch")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	update := `jsonb_set(state,'{currentApy}',$5::jsonb,true)`
	if value.Flat {
		// Keep the last APY; mark it flat.
		update = `jsonb_set(state,'{currentApy}',COALESCE(state->'currentApy','{}'::jsonb) || jsonb_build_object('flat',true,'observedAt',$5::jsonb->'observedAt'),true)`
	}
	tag, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=`+update+`,updated_at=clock_timestamp()
		WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND state_version=$4 AND lease_expires_at>clock_timestamp()`,
		routeKey, lease.Owner, lease.FencingToken, expectedVersion, string(raw))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return budgetHold("current_apy_state_changed")
	}
	return nil
}

// publishCurrentAPY is the selector-loop hook: compute, throttle, write.
// Every failure only logs; it never reaches a tick or a money path.
func publishCurrentAPY(ctx context.Context, now time.Time, throttle *currentAPYThrottle, observed Observation, markets []LaneEconomics, write func(context.Context, CurrentAPY, int64) error, logf func(string, ...any)) {
	if observed.planning == nil || !leverageLane(observed.Snapshot.RouteLane) && !selectorLane(observed.Snapshot.RouteLane) {
		return
	}
	value, ok := currentPositionAPY(observed.Snapshot, markets)
	if !ok && !value.Flat {
		return
	}
	value.ObservedAt = now.UTC()
	if !throttle.due(now, value) {
		return
	}
	if err := write(ctx, value, observed.planning.generation); err != nil {
		logf("backyard-rwa-worker: current APY not stored: %s\n", sanitizedSelectorEvaluateFailure(err))
		return
	}
	throttle.wrote(now, value)
}
