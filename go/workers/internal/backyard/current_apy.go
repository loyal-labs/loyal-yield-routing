package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// CurrentAPY is the vault position's fee-paying APY estimate, published for the
// web app in multiply_route_states.state.currentApy. It includes the approved
// performance fee on profitable carry, not exact realized HWM fees. Display only:
// no money path, admission or selector decision reads it.
type CurrentAPY struct {
	Lane       string    `json:"lane"`
	APYBPS     int64     `json:"apyBps"`
	Level      float64   `json:"level"`
	ObservedAt time.Time `json:"observedAt"`
	Flat       bool      `json:"flat"`
}

// currentPositionAPY publishes the leverage watch's own APY figure for the
// route lane at its actual collateral/equity ratio and current debt cost,
// from the same LaneEconomics the
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
	if !finite(market.CurrentBorrowAPY) || market.CurrentBorrowAPY < 0 || s.PositionDebtValueRaw < 0 {
		return out, false
	}
	apy := performanceFeeForecast((collateral*market.NativeAPY + float64(s.PositionCollateralValueRaw)*market.SupplyAPY - float64(s.PositionDebtValueRaw)*market.CurrentBorrowAPY) / float64(equityRaw))
	ok := true
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
	if observed.planning == nil || !earnHeldLane(observed.Snapshot.RouteLane) {
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

// LeverageWatchSummary is the stored copy of the hourly leverage-watch
// summary line (display data for the admin app).
type LeverageWatchSummary struct {
	ObservedAt time.Time           `json:"observedAt"`
	Lanes      []leverageWatchLane `json:"lanes"`
}

// mergeLeverageWatch keeps every previous lane the new summary does not
// carry (with its older observedAt) and replaces the ones it does.
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

// RecordLeverageWatch merges and stores the summary under
// state.leverageWatch with the same fence as RecordCurrentAPY (lease,
// fencing token, state_version; no generation bump). The per-lane merge
// reads the stored value in the same statement's row lock.
func (d *Database) RecordLeverageWatch(ctx context.Context, routeKey string, summary LeverageWatchSummary, expectedVersion int64) error {
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	if lease.RouteKey != routeKey {
		return fmt.Errorf("leverage_watch_route_lease_mismatch")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	if err = tx.QueryRow(ctx, `SELECT COALESCE(state->'leverageWatch','null'::jsonb) FROM loyal_yield.multiply_route_states
		WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND state_version=$4 AND lease_expires_at>clock_timestamp() FOR UPDATE`,
		routeKey, lease.Owner, lease.FencingToken, expectedVersion).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return budgetHold("leverage_watch_state_changed")
		}
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
	if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{leverageWatch}',$2::jsonb,true),updated_at=clock_timestamp() WHERE route_key=$1`, routeKey, string(merged)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// publishLeverageWatch stores the summary the watch just printed. Every
// failure only logs.
func publishLeverageWatch(ctx context.Context, now time.Time, watch *leverageWatch, observed Observation, write func(context.Context, LeverageWatchSummary, int64) error, logf func(string, ...any)) {
	if watch == nil || len(watch.summary) == 0 || observed.planning == nil {
		return
	}
	summary := LeverageWatchSummary{ObservedAt: now.UTC()}
	for _, lane := range watch.summary {
		lane.ObservedAt = now.UTC()
		summary.Lanes = append(summary.Lanes, lane)
	}
	if err := write(ctx, summary, observed.planning.generation); err != nil {
		logf("backyard-rwa-worker: leverage watch not stored: %s\n", sanitizedSelectorEvaluateFailure(err))
	}
}
