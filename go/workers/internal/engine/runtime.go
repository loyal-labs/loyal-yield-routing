package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Lane is a synchronous process-lifetime capability, not a workflow interface.
type Lane interface{ Run(context.Context) error }

// Run owns lane lifetimes and joins all work before dependencies can be closed.
func Run(ctx context.Context, lanes ...Lane) error {
	if ctx == nil || len(lanes) == 0 {
		return errors.New("missing runtime lanes")
	}
	for _, lane := range lanes {
		if lane == nil {
			return errors.New("nil runtime lane")
		}
	}
	owned, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	results := make(chan error, len(lanes))
	for i, lane := range lanes {
		wg.Add(1)
		go func(index int, lane Lane) {
			defer wg.Done()
			err := lane.Run(owned)
			if owned.Err() == nil && (err == nil || errors.Is(err, context.Canceled)) {
				err = fmt.Errorf("persistent lane %d stopped unexpectedly", index)
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				results <- err
			}
			cancel()
		}(i, lane)
	}
	wg.Wait()
	close(results)
	var failures []error
	for err := range results {
		failures = append(failures, err)
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return ctx.Err()
}
