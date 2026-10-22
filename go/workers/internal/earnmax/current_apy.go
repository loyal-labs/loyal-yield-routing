package earnmax

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

const currentApyMaxAge = 30 * time.Minute

func (r *Reader) CurrentApyBps(ctx context.Context, now time.Time) (*int64, error) {
	raw, err := r.routeState(ctx, "'currentApy'")
	if err != nil || raw == nil {
		return nil, err
	}
	var apy *struct {
		APYBps     *float64 `json:"apyBps"`
		Flat       *bool    `json:"flat"`
		ObservedAt *string  `json:"observedAt"`
	}
	if json.Unmarshal(raw, &apy) != nil || apy == nil || apy.APYBps == nil || math.IsInf(*apy.APYBps, 0) || apy.ObservedAt == nil {
		return nil, nil
	}
	if apy.Flat != nil && *apy.Flat {
		return nil, nil
	}
	observed, err := time.Parse(time.RFC3339Nano, *apy.ObservedAt)
	if err != nil || now.Sub(observed) >= currentApyMaxAge {
		return nil, nil
	}
	bps := roundHalfUp(*apy.APYBps)
	return &bps, nil
}
