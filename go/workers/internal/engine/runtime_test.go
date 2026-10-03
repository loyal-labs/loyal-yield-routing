package engine

import (
	"context"
	"errors"
	"testing"
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
