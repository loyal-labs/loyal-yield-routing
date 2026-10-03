package worker

import "testing"

func TestWatchRefreshSignalsCoalesce(t *testing.T) {
	refresh := make(chan struct{}, 1)
	signalWatchRefresh(refresh)
	signalWatchRefresh(refresh)
	if len(refresh) != 1 {
		t.Fatalf("queued refresh signals = %d, want 1", len(refresh))
	}
}
