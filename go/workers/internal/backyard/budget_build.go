package backyard

import (
	"context"
	"sync"
)

const maxConcurrentReads = 4

func concurrentReads(ctx context.Context, steps ...func(context.Context) error) error {
	errs := make([]error, len(steps))
	contexts := make([]context.Context, len(steps))
	cancels := make([]context.CancelFunc, len(steps))
	for i := range steps {
		contexts[i], cancels[i] = context.WithCancel(ctx)
		defer cancels[i]()
	}
	slots := make(chan struct{}, maxConcurrentReads)
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
