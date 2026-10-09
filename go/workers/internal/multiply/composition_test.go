package multiply

import (
	"context"
	"errors"
	"testing"
	"time"
)

type startupCancellationRPC struct {
	fakeRPC
	started chan context.Context
}

func (r *startupCancellationRPC) GenesisHash(ctx context.Context) (string, error) {
	r.started <- ctx
	<-ctx.Done()
	// Simulates a transport returning a completed buffered response after the
	// caller has stopped startup. Construction must still honor cancellation.
	return mainnetGenesisHash, nil
}

func TestExecutorStartupCancellationCannotPublishCapability(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rpc := &startupCancellationRPC{started: make(chan context.Context, 1)}
	result := make(chan error, 1)
	go func() {
		executor, err := NewExecutorWithFeePayerContext(ctx, rpc, testDelegateSeed(), testDelegateSeed())
		if executor != nil {
			result <- errors.New("canceled startup published signer capability")
			return
		}
		result <- err
	}()
	select {
	case child := <-rpc.started:
		if _, ok := child.Deadline(); !ok {
			t.Fatal("genesis preflight lost bounded deadline")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("genesis preflight did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup cancellation lost: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("startup did not join canceled preflight")
	}
}

func TestBorrowedStorePreservesEnginePoolLifetime(t *testing.T) {
	owner := integrationStore(t)
	borrowed, err := NewStoreFromPool(context.Background(), owner.Pool())
	if err != nil {
		t.Fatal(err)
	}
	if borrowed.Pool() != owner.Pool() {
		t.Fatal("family constructor replaced engine pool")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if store, err := NewStoreFromPool(ctx, owner.Pool()); store != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled constructor: %v %v", store, err)
	}
	if err := owner.Pool().Ping(context.Background()); err != nil {
		t.Fatalf("failed family constructor closed engine pool: %v", err)
	}
}
