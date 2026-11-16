package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
)

type Lane interface{ Run(context.Context) error }

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
			// pgx reports a cancelled SQL statement separately from context cancellation.
			for cause := err; owned.Err() != nil && cause != nil; cause = errors.Unwrap(cause) {
				if pgErr, ok := cause.(*pgconn.PgError); ok && pgErr.Code == "57014" {
					err = owned.Err()
					break
				}
			}
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
