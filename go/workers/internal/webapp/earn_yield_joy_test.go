package webapp

import (
	"context"
	"errors"
	"testing"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

func TestJoyEarningsCombinesAndRoutes(t *testing.T) {
	e := joyEarnings{earnLifetimeUSD: 40, earnPrincipalUSD: 1000, earnMaxPerformanceUSD: -2.5, earnMaxEquityUSD: 300}
	if got := e.lifetimeUSD(); got != 37.5 {
		t.Fatalf("lifetimeUSD = %v, want 37.5", got)
	}
	if got := e.route(); got != "/(tabs)/earn" {
		t.Fatalf("route = %q, want earn", got)
	}
	if got := (joyEarnings{earnMaxPerformanceUSD: 3, earnMaxEquityUSD: 300}).route(); got != "/(tabs)/earn-max" {
		t.Fatalf("route = %q, want earn-max", got)
	}
	if got := (joyEarnings{}).route(); got != "/(tabs)/earn" {
		t.Fatalf("route = %q, want earn", got)
	}
}

func TestEarnMaxEarningsAvailability(t *testing.T) {
	cases := []struct {
		name      string
		activity  earnmax.Activity
		perf, eq  float64
		available bool
	}{
		{"held with earnings", earnmax.Activity{Earnings: &earnmax.Earnings{PerformanceUsd: 4.2}, Performance: []earnmax.PerformancePoint{{EquityUsd: 0}, {EquityUsd: 310}}}, 4.2, 310, true},
		{"never held", earnmax.Activity{Performance: []earnmax.PerformancePoint{{EquityUsd: 0}}}, 0, 0, true},
		{"no history", earnmax.Activity{Performance: []earnmax.PerformancePoint{}}, 0, 0, true},
		{"held but earnings unknown", earnmax.Activity{Performance: []earnmax.PerformancePoint{{EquityUsd: 310}}}, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			perf, eq, ok := earnMaxEarnings(tc.activity)
			if ok != tc.available {
				t.Fatalf("available = %v, want %v", ok, tc.available)
			}
			if ok && (perf != tc.perf || eq != tc.eq) {
				t.Fatalf("got (%v, %v), want (%v, %v)", perf, eq, tc.perf, tc.eq)
			}
		})
	}
}

// Review Focus 1: a wallet whose Earn MAX cannot be read is skipped, never
// pushed with an Earn-only number.
func TestWalletEarningsUnavailableWhenEarnMaxFails(t *testing.T) {
	_, ok, err := (Crons{}).walletEarnings(context.Background(), "mainnet", joyCandidate{})
	if ok || err == nil {
		t.Fatalf("got ok=%v err=%v, want an error", ok, err)
	}
}

func TestWalletEarningsUnavailableWhenEarnMaxActivityErrors(t *testing.T) {
	settings := solana.NewWallet().PublicKey().String()
	earn := joyEarnings{earnLifetimeUSD: 40, earnPrincipalUSD: 1000}
	run := func(fn func(context.Context, solana.PublicKey) (earnmax.Activity, error)) (joyEarnings, bool, error) {
		return (Crons{EarnMaxActivity: fn}).withEarnMax(context.Background(), "wallet", settings, earn)
	}
	t.Run("call error", func(t *testing.T) {
		got, ok, err := run(func(context.Context, solana.PublicKey) (earnmax.Activity, error) {
			return earnmax.Activity{}, errors.New("rpc down")
		})
		if got != (joyEarnings{}) || ok || err != nil {
			t.Fatalf("got (%+v, %v, %v), want (zero, false, nil)", got, ok, err)
		}
	})
	t.Run("holds Earn MAX but earnings unknown", func(t *testing.T) {
		got, ok, err := run(func(context.Context, solana.PublicKey) (earnmax.Activity, error) {
			return earnmax.Activity{Performance: []earnmax.PerformancePoint{{EquityUsd: 310}}}, nil
		})
		if got != (joyEarnings{}) || ok || err != nil {
			t.Fatalf("got (%+v, %v, %v), want (zero, false, nil)", got, ok, err)
		}
	})
	t.Run("readable combines", func(t *testing.T) {
		got, ok, err := run(func(context.Context, solana.PublicKey) (earnmax.Activity, error) {
			return earnmax.Activity{Earnings: &earnmax.Earnings{PerformanceUsd: 4}, Performance: []earnmax.PerformancePoint{{EquityUsd: 310}}}, nil
		})
		want := joyEarnings{earnLifetimeUSD: 40, earnPrincipalUSD: 1000, earnMaxPerformanceUSD: 4, earnMaxEquityUSD: 310}
		if got != want || !ok || err != nil {
			t.Fatalf("got (%+v, %v, %v), want (%+v, true, nil)", got, ok, err, want)
		}
	})
}
