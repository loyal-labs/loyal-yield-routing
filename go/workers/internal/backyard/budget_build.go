package backyard

import (
	"context"
	"sync"
)

// phase3ReadFanout bounds the independent reads one pricer runs at once; each
// cost read already fans out to its fee and price reads.
const phase3ReadFanout = 4

// concurrentReads runs independent read-only steps at once, at most
// phase3ReadFanout in flight, and returns the first error in step order: the
// error the serial sequence would have stopped on. A failure cancels only the
// steps after it, which the serial sequence would never have reached; the
// steps before it finish, so the reported error is never a cancellation it
// caused. Each step writes only its own results.
func concurrentReads(ctx context.Context, steps ...func(context.Context) error) error {
	errs := make([]error, len(steps))
	contexts := make([]context.Context, len(steps))
	cancels := make([]context.CancelFunc, len(steps))
	for i := range steps {
		contexts[i], cancels[i] = context.WithCancel(ctx)
		defer cancels[i]()
	}
	slots := make(chan struct{}, phase3ReadFanout)
	var reads sync.WaitGroup
	for i, step := range steps {
		reads.Add(1)
		go func() {
			defer reads.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			if errs[i] = step(contexts[i]); errs[i] != nil {
				for _, cancel := range cancels[i+1:] {
					cancel()
				}
			}
		}()
	}
	reads.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
