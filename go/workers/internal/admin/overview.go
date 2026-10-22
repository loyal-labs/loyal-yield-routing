package admin

import "context"

type OverviewAutodepositPoint struct {
	BucketStartedAt    string `json:"bucketStartedAt"`
	DepositedAmountRaw string `json:"depositedAmountRaw"`
	Successful         int64  `json:"successful"`
}

type OverviewAutodepositRange struct {
	BucketHours int64                      `json:"bucketHours"`
	Key         string                     `json:"key" enum:"2d,7d,30d"`
	Points      []OverviewAutodepositPoint `json:"points" nullable:"false"`
}

type OverviewProps struct {
	Autodeposit        []OverviewAutodepositRange         `json:"autodeposit" nullable:"false"`
	OptimizationVolume []RebalanceOptimizationVolumePoint `json:"optimizationVolume" nullable:"false"`
}

func registerOverview(m *Mux) {
	Page(m, "/", overviewLoad)
	Page(m, "/overview", overviewLoad)
}

func overviewLoad(ctx context.Context, d *Deps, _ PageRequest) (OverviewProps, error) {
	ranges, err := rebalanceAutodepositTimeSeries(ctx, d)
	if err != nil {
		return OverviewProps{}, err
	}
	volume, err := rebalanceOptimizationVolumeSeries(ctx, d)
	if err != nil {
		return OverviewProps{}, err
	}
	props := OverviewProps{Autodeposit: make([]OverviewAutodepositRange, len(ranges)), OptimizationVolume: volume}
	for i, rng := range ranges {
		points := make([]OverviewAutodepositPoint, len(rng.points))
		for j, p := range rng.points {
			points[j] = OverviewAutodepositPoint{BucketStartedAt: p.bucketStartedAt, DepositedAmountRaw: p.depositedAmountRaw, Successful: p.successful}
		}
		props.Autodeposit[i] = OverviewAutodepositRange{BucketHours: rng.bucketHours, Key: rng.key, Points: points}
	}
	return props, nil
}
