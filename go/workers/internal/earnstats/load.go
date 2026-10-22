package earnstats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type EarnPublicStats struct {
	AumUSD                 float64          `json:"aumUsd"`
	AumDeltaVsPriorWeekUSD *float64         `json:"aumDeltaVsPriorWeekUsd"`
	AumSeries              []EarnStatsPoint `json:"aumSeries"`
	OptimizationVolumeUSD  float64          `json:"optimizationVolumeUsd"`
	RefreshedAt            string           `json:"refreshedAt"`
	TotalUsers             int              `json:"totalUsers"`
}

type EarnStatsPoint struct {
	Label       string  `json:"label"`
	PeriodLabel string  `json:"periodLabel"`
	Value       float64 `json:"value"`
}

type snapshot struct {
	aumRaw      int64
	volumeRaw   int64
	totalUsers  int
	refreshedAt time.Time
	series      json.RawMessage
}

func Load(ctx context.Context, pool *pgxpool.Pool) (*EarnPublicStats, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 10000"); err != nil {
		return nil, err
	}
	var row snapshot
	err = tx.QueryRow(ctx, `SELECT total_aum_raw, total_optimized_volume_raw, total_users, refreshed_at, earn_aum_series
FROM public.loyal_stats_snapshots WHERE snapshot_key = 'current' LIMIT 1`).Scan(
		&row.aumRaw, &row.volumeRaw, &row.totalUsers, &row.refreshedAt, &row.series,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return row.publicStats(time.Now())
}

func (s snapshot) publicStats(now time.Time) (*EarnPublicStats, error) {
	// JS Date truncates Postgres timestamps to milliseconds.
	refreshed := s.refreshedAt.UTC().Truncate(time.Millisecond)
	if now.Truncate(time.Millisecond).Sub(refreshed) > 5*time.Minute {
		return nil, errors.New("stats snapshot is stale")
	}
	var points []struct {
		AumRaw    string `json:"aumRaw"`
		WeekStart string `json:"weekStart"`
		WeekEnd   string `json:"weekEnd"`
	}
	if err := json.Unmarshal(s.series, &points); err != nil {
		return nil, err
	}
	if points == nil {
		return nil, errors.New("missing stats series")
	}
	stats := &EarnPublicStats{
		AumUSD:                float64(s.aumRaw) / 1e6,
		OptimizationVolumeUSD: float64(s.volumeRaw) / 1e6,
		RefreshedAt:           refreshed.Format("2006-01-02T15:04:05.000Z"),
		TotalUsers:            s.totalUsers,
		AumSeries:             make([]EarnStatsPoint, 0, len(points)),
	}
	var prior *big.Int
	for _, point := range points {
		raw, ok := new(big.Int).SetString(point.AumRaw, 10)
		if !ok {
			return nil, errors.New("invalid stats amount")
		}
		start, err := time.Parse("2006-01-02", point.WeekStart)
		if err != nil {
			return nil, fmt.Errorf("invalid stats week start: %w", err)
		}
		end, err := time.Parse("2006-01-02", point.WeekEnd)
		if err != nil {
			return nil, fmt.Errorf("invalid stats week end: %w", err)
		}
		value, err := statsUSDC(raw)
		if err != nil {
			return nil, err
		}
		label := start.Format("Jan 2")
		stats.AumSeries = append(stats.AumSeries, EarnStatsPoint{
			Label: label, PeriodLabel: label + " - " + end.Format("Jan 2"), Value: value,
		})
		if prior != nil {
			delta, err := statsUSDC(new(big.Int).Sub(raw, prior))
			if err != nil {
				return nil, err
			}
			stats.AumDeltaVsPriorWeekUSD = &delta
		}
		prior = raw
	}
	return stats, nil
}

func statsUSDC(raw *big.Int) (float64, error) {
	value, _ := new(big.Float).SetInt(raw).Float64()
	value /= 1e6
	if math.IsInf(value, 0) {
		return 0, errors.New("stats amount exceeds JSON number range")
	}
	return value, nil
}
