package webapp

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

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

var joyTestNow = time.Date(2026, 11, 9, 15, 41, 0, 0, time.UTC)

func joyV2(baseline float64, sent []string, recent ...time.Time) *joyState {
	return &joyState{ruleVersion: 2, lastPushedEarnedUSD: baseline, sentCampaigns: sent, recentPushedAt: recent}
}

func TestSelectJoyPushRule(t *testing.T) {
	now := joyTestNow
	day := 24 * time.Hour
	one := []string{"yield-total-1"}
	cases := []struct {
		name           string
		age, lifetime  float64
		state          *joyState
		kind, campaign string
		amount         float64
	}{
		{"new wallet seeds", 10, 37.2, nil, joyDecisionSeed, "", 0},
		{"0.99 above baseline", 10, 20.99, joyV2(20, one), joyDecisionNone, "", 0},
		{"exactly 1.00 above", 10, 21, joyV2(20, one), joyDecisionPush, "yield-digest", 1},
		{"first dollar", 10, 1.2, joyV2(0.1, []string{}), joyDecisionPush, "yield-total-1", 1},
		{"jump past two milestones", 10, 520, joyV2(90, one), joyDecisionPush, "yield-total-500", 500},
		{"anniversary needs a dollar", 200, 0.8, joyV2(0, []string{}), joyDecisionNone, "", 0},
		{"anniversary", 200, 3, joyV2(1.5, one), joyDecisionPush, "yield-six-months", 3},
		{"same UTC day cap", 10, 30, joyV2(20, one, now.Add(-2*time.Hour)), joyDecisionNone, "", 0},
		{"yesterday is fine", 10, 30, joyV2(20, one, now.Add(-23*time.Hour)), joyDecisionPush, "yield-digest", 10},
		{"weekly cap", 10, 30, joyV2(20, one, now.Add(-day), now.Add(-2*day), now.Add(-3*day), now.Add(-4*day)), joyDecisionNone, "", 0},
		{"cap window rolls", 10, 30, joyV2(20, one, now.Add(-day), now.Add(-2*day), now.Add(-3*day), now.Add(-8*day)), joyDecisionPush, "yield-digest", 10},
		{"milestone beats anniversary", 200, 120, joyV2(0, one), joyDecisionPush, "yield-total-100", 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectJoyPush(tc.age, tc.lifetime, now, tc.state)
			if got.kind != tc.kind {
				t.Fatalf("kind = %q, want %q (%+v)", got.kind, tc.kind, got)
			}
			if tc.kind == joyDecisionSeed {
				if !slices.Equal(got.campaigns, []string{"yield-total-1"}) || got.recentPushedAt == nil || len(got.recentPushedAt) != 0 {
					t.Fatalf("seed = %+v", got)
				}
			}
			if tc.kind != joyDecisionPush {
				return
			}
			if got.push.campaign != tc.campaign || got.push.amountUSD != tc.amount || got.push.lifetimeUSD != tc.lifetime {
				t.Fatalf("push = %+v", got.push)
			}
		})
	}
	t.Run("cap window rolls stores", func(t *testing.T) {
		st := joyV2(20, one, now.Add(-day), now.Add(-2*day), now.Add(-3*day), now.Add(-8*day))
		got := selectJoyPush(10, 30, now, st)
		want := []time.Time{now.Add(-day), now.Add(-2 * day), now.Add(-3 * day), now}
		if !slices.Equal(got.recentPushedAt, want) {
			t.Fatalf("recent = %v, want %v", got.recentPushedAt, want)
		}
	})
}

func TestSelectJoyPushHighWaterMark(t *testing.T) {
	now := joyTestNow
	one := []string{"yield-total-1"}
	if got := selectJoyPush(10, 47, now, joyV2(50, one)); got.kind != joyDecisionNone {
		t.Fatalf("drop: %+v", got)
	}
	if got := selectJoyPush(10, 50.9, now, joyV2(50, one)); got.kind != joyDecisionNone {
		t.Fatalf("recovery: %+v", got)
	}
	got := selectJoyPush(10, 51, now, joyV2(50, one))
	if got.kind != joyDecisionPush || got.push.amountUSD != 1 {
		t.Fatalf("51: %+v", got)
	}
	if got := selectJoyPush(400, -3, now, joyV2(0, []string{})); got.kind != joyDecisionNone {
		t.Fatalf("negative: %+v", got)
	}
}

func TestSelectJoyPushRebaselinesOldRows(t *testing.T) {
	now := joyTestNow
	old := func(last time.Time) *joyState {
		return &joyState{ruleVersion: 1, lastPushedEarnedUSD: 12, sentCampaigns: []string{"yield-first", "yield-total-10"}, lastPushedAt: &last}
	}
	got := selectJoyPush(10, 640, now, old(now.Add(-3*24*time.Hour)))
	if got.kind != joyDecisionRebaseline {
		t.Fatalf("kind = %q", got.kind)
	}
	for _, want := range []string{"yield-first", "yield-total-10", "yield-total-500"} {
		if !slices.Contains(got.campaigns, want) {
			t.Fatalf("campaigns %v missing %q", got.campaigns, want)
		}
	}
	if !slices.Equal(got.recentPushedAt, []time.Time{now.Add(-3 * 24 * time.Hour)}) {
		t.Fatalf("recent = %v", got.recentPushedAt)
	}
	got = selectJoyPush(10, 640, now, old(now.Add(-9*24*time.Hour)))
	if got.recentPushedAt == nil || len(got.recentPushedAt) != 0 {
		t.Fatalf("recent = %v", got.recentPushedAt)
	}
}

func TestJoyPushPayloadCopy(t *testing.T) {
	now := joyTestNow
	cases := []struct {
		name        string
		push        joyPush
		title, body string
		idem        string
	}{
		{"digest", joyPush{amountUSD: 1.37, lifetimeUSD: 48.2, route: "/(tabs)/earn", campaign: joyDigestCampaign, kind: joyKindDigest},
			"+$1.37 earned", "$48.20 earned with Loyal so far.", "earnings:yield-digest:W1:2026-11-09"},
		{"milestone 1", joyPush{amountUSD: 1, lifetimeUSD: 1.2, route: "/(tabs)/earn", campaign: "yield-total-1", kind: joyKindMilestone},
			"$1 earned with Loyal", "Your first dollar, and it keeps going on its own.", "earnings:yield-total-1:W1:2026-11-09"},
		{"milestone 5000", joyPush{amountUSD: 5000, lifetimeUSD: 5012.4, route: "/(tabs)/earn", campaign: "yield-total-5000", kind: joyKindMilestone},
			"$5,000 earned with Loyal", "And it keeps going on its own.", "earnings:yield-total-5000:W1:2026-11-09"},
		{"anniversary 6", joyPush{lifetimeUSD: 48.2, route: "/(tabs)/earn", campaign: joyAnniversarySixMo, months: 6, kind: joyKindAnniversary},
			"Six months with Loyal", "$48.20 earned since you joined.", "earnings:yield-six-months:W1:2026-11-09"},
		{"anniversary 12", joyPush{lifetimeUSD: 48.2, route: "/(tabs)/earn", campaign: joyAnniversaryOneYear, months: 12, kind: joyKindAnniversary},
			"A year with Loyal", "$48.20 earned since you joined.", "earnings:yield-one-year:W1:2026-11-09"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := joyPushPayload(tc.push, now, "W1")
			if got.Title != tc.title || got.Body != tc.body || got.IdempotencyName != tc.idem ||
				got.Category != pushCategoryEarnings || got.Delivery != pushDeliveryIntelligent || got.Route != "/(tabs)/earn" {
				t.Fatalf("payload = %+v", got)
			}
		})
	}
	if formatUSDWhole(1000) != "$1,000" || formatUSDWhole(1) != "$1" {
		t.Fatalf("formatUSDWhole: %q %q", formatUSDWhole(1000), formatUSDWhole(1))
	}
}
