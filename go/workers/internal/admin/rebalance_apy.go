package admin

import (
	"math"
	"sort"
	"strconv"
	"time"
)

const (
	rebalanceFixedSafetyMarginUsdMicros = 50_000
	rebalanceHoldingHorizonSeconds      = 30 * 24 * 60 * 60
	rebalanceMinimumNetGainUsdMicros    = 100_000
	rebalanceMinimumNotionalUsdMicros   = 1_000_000
	rebalanceSecondsPerYear             = 31_536_000
)

func rebalanceEligibilityFloorRaw(statuses []SafeReserveApyStatusRow) *string {
	usdc, _ := EarnStablecoinBySymbol("USDC")
	apys := []float64{}
	for _, s := range statuses {
		if s.LiquidityMint != usdc.Mint || s.Status != "eligible" || s.SupplyApyPercent == nil {
			continue
		}
		if v := *s.SupplyApyPercent; !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 {
			apys = append(apys, v)
		}
	}
	if len(apys) < 2 {
		return nil
	}
	sort.Float64s(apys)
	edge := (apys[len(apys)-1] - apys[0]) / 100
	if edge <= 0 {
		return nil
	}
	horizon := float64(rebalanceHoldingHorizonSeconds) / rebalanceSecondsPerYear
	required := float64(rebalanceMinimumNetGainUsdMicros+rebalanceFixedSafetyMarginUsdMicros) / 1_000_000
	floor := math.Max(required/(edge*horizon), float64(rebalanceMinimumNotionalUsdMicros)/1_000_000)
	raw := strconv.FormatFloat(math.Ceil(floor*math.Pow10(usdc.Decimals)), 'f', 0, 64)
	return &raw
}

type rebalanceApyHistory struct {
	GeneratedAt           string                    `json:"generatedAt"`
	SampleIntervalMinutes int                       `json:"sampleIntervalMinutes"`
	Series                []SafeReserveApySeries    `json:"series"`
	Statuses              []SafeReserveApyStatusRow `json:"statuses"`
	Values                [][]*float64              `json:"values"`
	ObservedAtMs          []int64                   `json:"observedAtMs"`
	Window                SafeReserveApyWindow      `json:"window"`
}

func rebalanceMedian(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	v := math.Floor(sorted[int(math.Floor(float64(len(sorted)-1)*0.5+0.5))]*1_000_000+0.5) / 1_000_000
	return &v
}

func rebalanceChartNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func rebalanceSerializeApyData(data SafeReserveApyMonitorData) rebalanceApyHistory {
	const bucketMs = 30 * 60 * 1_000
	var startMs int64
	if started, err := time.Parse(time.RFC3339Nano, data.Window.StartedAt); err == nil {
		startMs = started.UnixMilli()
	}
	buckets := map[int64][][]float64{}
	for _, point := range data.ChartPoints {
		observed, _ := rebalanceChartNumber(point["observedAtMs"])
		offset := int64(math.Floor((observed - float64(startMs)) / bucketMs))
		key := startMs + offset*bucketMs
		bucket, ok := buckets[key]
		if !ok {
			bucket = make([][]float64, len(data.Series))
		}
		for i, s := range data.Series {
			if v, ok := rebalanceChartNumber(point[s.Key]); ok {
				bucket[i] = append(bucket[i], v)
			}
		}
		buckets[key] = bucket
	}
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	values := make([][]*float64, len(data.Series))
	for i := range data.Series {
		values[i] = make([]*float64, len(keys))
		for j, k := range keys {
			values[i][j] = rebalanceMedian(buckets[k][i])
		}
	}
	series, statuses := data.Series, data.Statuses
	if series == nil {
		series = []SafeReserveApySeries{}
	}
	if statuses == nil {
		statuses = []SafeReserveApyStatusRow{}
	}
	return rebalanceApyHistory{
		GeneratedAt:           data.GeneratedAt,
		SampleIntervalMinutes: 30,
		Series:                series,
		Statuses:              statuses,
		Values:                values,
		ObservedAtMs:          keys,
		Window:                data.Window,
	}
}
