package webapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

var errEarnMaxReader = errors.New("earn-yield-joy has no Earn MAX reader: refusing to push an Earn-only total")

const (
	joyRouteEarn    = "/(tabs)/earn"
	joyRouteEarnMax = "/(tabs)/earn-max"
)

// joyEarnings is what a wallet has earned across Earn and Earn MAX, so the
// push number equals the two tabs added together.
type joyEarnings struct {
	earnLifetimeUSD       float64
	earnPrincipalUSD      float64
	earnMaxPerformanceUSD float64
	earnMaxEquityUSD      float64
}

func (e joyEarnings) lifetimeUSD() float64 {
	return e.earnLifetimeUSD + e.earnMaxPerformanceUSD
}

// route is the tab a tap on the push opens: Earn MAX only for a wallet whose
// money sits there alone.
func (e joyEarnings) route() string {
	if e.earnPrincipalUSD <= 0 && e.earnMaxEquityUSD > 0 {
		return joyRouteEarnMax
	}
	return joyRouteEarn
}

// earnMaxEarnings reads the Earn MAX tab's numbers off an activity. A wallet
// that holds Earn MAX but has no earnings figure is not available: pushing
// its Earn-only total would understate it.
func earnMaxEarnings(activity earnmax.Activity) (performanceUSD, equityUSD float64, available bool) {
	if n := len(activity.Performance); n > 0 {
		equityUSD = activity.Performance[n-1].EquityUsd
	}
	if activity.Earnings == nil {
		return 0, 0, equityUSD <= 0
	}
	return activity.Earnings.PerformanceUsd, equityUSD, true
}

// walletEarnings reads Earn, then Earn MAX. A wallet whose Earn or Earn MAX
// cannot be read is reported unavailable, never as an Earn-only number.
func (c Crons) walletEarnings(ctx context.Context, cluster string, candidate joyCandidate) (joyEarnings, bool, error) {
	if c.Earnings != nil {
		return c.Earnings(ctx, cluster, candidate.settingsPda, candidate.wallet)
	}
	if c.EarnMaxActivity == nil && c.EarnMax == nil {
		return joyEarnings{}, false, errEarnMaxReader
	}
	input := earningsReadInput{
		cluster:       cluster,
		settings:      candidate.settingsPda,
		timezone:      normalizeEarningsTimezone(""),
		vaultIndex:    int16(earnprep.EarnVaultIndex),
		walletAddress: candidate.wallet,
	}
	payload, err := readEarningsRangeSetResuming(ctx, input, newEarningsReadDependencies(c.h.YieldDB, c.h.Timescale, false))
	var unavailable *earningsUnavailableError
	if errors.As(err, &unavailable) {
		return joyEarnings{}, false, nil
	}
	if err != nil {
		return joyEarnings{}, false, err
	}
	earn := joyEarnings{
		earnLifetimeUSD:  payload.Ranges.All.LifetimeEarnedUsd,
		earnPrincipalUSD: payload.Ranges.All.PrincipalUsd,
	}
	return c.withEarnMax(ctx, candidate.wallet, candidate.settingsPda, earn)
}

// withEarnMax adds the Earn MAX numbers to an Earn read.
func (c Crons) withEarnMax(ctx context.Context, wallet, settings string, earn joyEarnings) (joyEarnings, bool, error) {
	activityFn := c.EarnMaxActivity
	if activityFn == nil {
		if c.EarnMax == nil {
			return joyEarnings{}, false, errEarnMaxReader
		}
		activityFn = c.EarnMax.Activity
	}
	settingsKey, err := solana.PublicKeyFromBase58(settings)
	if err != nil {
		return joyEarnings{}, false, fmt.Errorf("earn max settings address: %w", err)
	}
	activity, err := activityFn(ctx, settingsKey)
	if err != nil {
		slog.WarnContext(ctx, "[earn-yield-joy] earn max unavailable", "errorMessage", err.Error(), "wallet", wallet)
		return joyEarnings{}, false, nil
	}
	performance, equity, available := earnMaxEarnings(activity)
	if !available {
		return joyEarnings{}, false, nil
	}
	earn.earnMaxPerformanceUSD, earn.earnMaxEquityUSD = performance, equity
	return earn, true, nil
}
