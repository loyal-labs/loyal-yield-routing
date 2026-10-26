package earnmax

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

const currentApyMaxAge = 30 * time.Minute

type RateDay struct {
	Day          string `json:"day"`
	StartMs      int64  `json:"startMs"`
	APYBps       int64  `json:"apyBps"`
	ObservedAtMs int64  `json:"observedAtMs"`
}

type CurrentRate struct {
	Bps  *int64
	Days []RateDay
}

func (r *Reader) loadCurrentRate(ctx context.Context, now time.Time) (CurrentRate, error) {
	if r.yield == nil {
		return CurrentRate{}, errNoYield
	}
	var raw, days []byte
	err := r.yield.QueryRow(ctx, `SELECT
  (SELECT state->'currentApy' FROM loyal_yield.multiply_route_states WHERE route_key = $1),
  COALESCE((SELECT jsonb_agg(jsonb_build_object(
      'day', day,
      'startMs', (extract(epoch FROM day::timestamp AT TIME ZONE 'America/Los_Angeles') * 1000)::bigint,
      'apyBps', apy_bps,
      'observedAtMs', (extract(epoch FROM observed_at) * 1000)::bigint) ORDER BY day)
    FROM loyal_yield.multiply_route_current_apy_days WHERE route_key = $1), '[]'::jsonb)`, RouteKey).Scan(&raw, &days)
	if err != nil {
		return CurrentRate{}, err
	}
	out := CurrentRate{Bps: currentApyBps(raw, now)}
	if err := json.Unmarshal(days, &out.Days); err != nil {
		return CurrentRate{}, err
	}
	return out, nil
}

func currentApyBps(raw []byte, now time.Time) *int64 {
	var apy *struct {
		APYBps     *float64 `json:"apyBps"`
		Flat       *bool    `json:"flat"`
		ObservedAt *string  `json:"observedAt"`
	}
	if raw == nil || json.Unmarshal(raw, &apy) != nil || apy == nil || apy.APYBps == nil || math.IsInf(*apy.APYBps, 0) || apy.ObservedAt == nil {
		return nil
	}
	if apy.Flat != nil && *apy.Flat {
		return nil
	}
	observed, err := time.Parse(time.RFC3339Nano, *apy.ObservedAt)
	if err != nil || now.Sub(observed) >= currentApyMaxAge {
		return nil
	}
	bps := roundHalfUp(*apy.APYBps)
	return &bps
}
