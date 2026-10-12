package backyard

import "testing"

func TestLeverageLevelsAndOneStepRule(t *testing.T) {
	t.Parallel()
	for level, want := range map[float64]int64{1: 0, 1.5: 3333, 1.75: 4285} {
		if got := leverageLevelLTVBPS(level); got != want {
			t.Fatalf("level %.2f LTV %d, want %d", level, got, want)
		}
		if level > 1 && leverageLevelLTVBPS(level) > leverageMaxLTVBPS {
			t.Fatalf("level %.2f above the 45%% cap", level)
		}
	}
	s := base()
	if currentLeverageBand(s) != 1 {
		t.Fatal("debt 0 is not 1x")
	}
	s.PositionDebtRaw = 1
	for ltv, want := range map[int64]float64{3322: 1.5, 3700: 1.5, 3900: 1.75, 4100: 1.75, 4285: 1.75, 5501: 1.75} {
		s.LTVBPS = ltv
		if got := currentLeverageBand(s); got != want {
			t.Fatalf("LTV %d snapped to %.2f, want %.2f", ltv, got, want)
		}
	}
	// The live rule is the watch's option 1 exactly.
	spread := func(v float64) func(float64) (float64, bool) { return func(float64) (float64, bool) { return v, true } }
	for _, tc := range []struct{ current, spread, want float64 }{
		{1, 0.009, 1}, {1, 0.012, 1.5}, {1.5, 0.019, 1.5}, {1.5, 0.02, 1.75}, {1.5, -0.001, 1},
		{1.75, 0.03, 1.75}, {1.75, 0.011, 1.75}, {1.75, 0.009, 1.5},
	} {
		if got := nextLiveLeverageLevel(tc.current, spread(tc.spread)); got != tc.want {
			t.Fatalf("from %.2fx at spread %.3f: %.2fx, want %.2fx", tc.current, tc.spread, got, tc.want)
		}
	}
}
