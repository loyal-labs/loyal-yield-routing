package backyard

import "testing"

// B2 watch-only: one step per sample, up/down thresholds with a gap, and an
// unreadable spread never moves.
func TestNextLeverageLevelStepsOnceWithHysteresis(t *testing.T) {
	t.Parallel()
	spread := func(v float64) func(float64) (float64, bool) {
		return func(float64) (float64, bool) { return v, true }
	}
	one := leverageWatchOptions["1"]
	for _, tc := range []struct {
		current, spread, want float64
	}{
		{1.5, 0.0353, 1.75},  // AUTO today: up one level
		{1.75, 0.0353, 1.75}, // option 1 caps at 1.75x
		{1.75, 0.015, 1.75},  // inside the gap: stay
		{1.75, 0.009, 1.5},   // below 1 pt: down one level
		{1.5, 0.015, 1.5},    // 1-2 pt at 1.5x: stay
		{1.5, -0.0048, 1},    // borrow > yield (Maple today): to 1x
		{1, 0.009, 1},        // below 1 pt: stay at 1x
		{1, 0.012, 1.5},      // back up
	} {
		if got := nextLeverageLevel(one, tc.current, spread(tc.spread)); got != tc.want {
			t.Fatalf("option 1 from %.2fx at spread %.4f: got %.2fx, want %.2fx", tc.current, tc.spread, got, tc.want)
		}
	}
	if got := nextLeverageLevel(leverageWatchOptions["2"], 1.75, spread(0.03)); got != 2 {
		t.Fatalf("option 2 should reach 2x, got %.2fx", got)
	}
	if got := nextLeverageLevel(leverageWatchOptions["3"], 1.5, spread(0.05)); got != 1.5 {
		t.Fatalf("option 3 must never exceed 1.5x, got %.2fx", got)
	}
	if got := nextLeverageLevel(one, 1.5, func(float64) (float64, bool) { return 0, false }); got != 1.5 {
		t.Fatalf("unreadable spread moved the level to %.2fx", got)
	}
}
