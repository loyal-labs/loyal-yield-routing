package stream

import "testing"

func TestHandoffReplayStartUsesNegativeOverlap(t *testing.T) {
	requested := uint64(200)
	if got := handoffReplayStart(150, 32, &requested); got != 118 {
		t.Fatalf("handoff replay start = %d, want frontier minus 32 = 118", got)
	}
}

func TestHandoffReplayStartHonorsDeeperNewBinding(t *testing.T) {
	requested := uint64(90)
	if got := handoffReplayStart(150, 32, &requested); got != 90 {
		t.Fatalf("handoff replay start = %d, want new binding start 90", got)
	}
}

func TestHandoffReplayStartSaturatesAtZero(t *testing.T) {
	if got := handoffReplayStart(10, 32, nil); got != 0 {
		t.Fatalf("handoff replay start = %d, want 0", got)
	}
}
