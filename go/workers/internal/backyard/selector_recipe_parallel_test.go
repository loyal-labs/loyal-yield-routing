package backyard

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestSelectorRecipeReadsBoundsConcurrencyAndChecksEveryError(t *testing.T) {
	t.Parallel()
	for fail := 0; fail < 17; fail++ {
		started, release := make(chan struct{}, 17), make(chan struct{})
		var active, peak atomic.Int32
		calls := make([]atomic.Int32, 17)
		want := errors.New("failed input")
		done := make(chan error, 1)
		go func() {
			done <- selectorRecipeReads(17, func(i int) error {
				calls[i].Add(1)
				n := active.Add(1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				started <- struct{}{}
				<-release
				active.Add(-1)
				if i == fail {
					return want
				}
				return nil
			})
		}()
		for i := 0; i < 4; i++ {
			select {
			case <-started:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("four reads did not start concurrently")
			}
		}
		if peak.Load() != 4 {
			close(release)
			t.Fatal("unexpected concurrency", peak.Load())
		}
		close(release)
		if err := <-done; !errors.Is(err, want) {
			t.Fatal("input error lost", fail, err)
		}
		if peak.Load() > 4 || active.Load() != 0 {
			t.Fatal("read bound or join failed")
		}
		for i := range calls {
			if calls[i].Load() != 1 {
				t.Fatal("missing or duplicate read", i)
			}
		}
	}
}
