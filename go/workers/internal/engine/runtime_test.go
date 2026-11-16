package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type laneFunc func(context.Context) error

func (f laneFunc) Run(ctx context.Context) error { return f(ctx) }

func TestRuntimeJoinsOwnedLanesOnFailure(t *testing.T) {
	failed := errors.New("ownership lost")
	joined := make(chan struct{})
	err := Run(context.Background(), laneFunc(func(context.Context) error { return failed }), laneFunc(func(ctx context.Context) error {
		<-ctx.Done()
		close(joined)
		return ctx.Err()
	}))
	if !errors.Is(err, failed) {
		t.Fatal(err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("dependency cleanup could race surviving lane")
	}
}

func TestRuntimeRejectsLaneCancellationWithoutRuntimeShutdown(t *testing.T) {
	if err := Run(context.Background(), laneFunc(func(context.Context) error {
		return context.Canceled
	})); err == nil {
		t.Fatal("a persistent lane stopped while the runtime still owned it")
	}
}

func TestRuntimeDatabaseCancellationRequiresShutdown(t *testing.T) {
	queryCanceled := &pgconn.PgError{Code: "57014"}
	otherFailure := errors.New("ownership lost")
	for _, tc := range []struct {
		name     string
		canceled bool
		err      error
		normal   bool
	}{
		{"planned", true, fmt.Errorf("read state: %w", queryCanceled), true},
		{"unexpected", false, queryCanceled, false},
		{"other_database_failure", true, &pgconn.PgError{Code: "23505"}, false},
		{"combined_failure", true, errors.Join(queryCanceled, otherFailure), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			err := Run(ctx, laneFunc(func(context.Context) error { return tc.err }))
			if tc.normal {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("planned query cancellation was reported as failure: %v", err)
				}
			} else if !errors.Is(err, tc.err) {
				t.Fatalf("runtime hid a real failure: %v", err)
			}
		})
	}
}
