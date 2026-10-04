package stream

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
)

type stalledReplacementConnector struct{ calls atomic.Int32 }

func (c *stalledReplacementConnector) Open(ctx context.Context, _ *pb.SubscribeRequest) (OpenStream, error) {
	if c.calls.Add(1) == 1 {
		return &blockingOpenStream{ctx: ctx}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestHandoffDeadlineCancelsOpeningAndRetainsOldSession(t *testing.T) {
	manager := NewManager(&stalledReplacementConnector{}, HandlerFunc(func(context.Context, *pb.SubscribeUpdate) error { return nil }), Config{HandoffTimeout: 50 * time.Millisecond})
	defer manager.Close()
	if err := manager.Start(context.Background(), &pb.SubscribeRequest{}); err != nil {
		t.Fatal(err)
	}
	old := manager.active
	result := make(chan error, 1)
	go func() { result <- manager.Handoff(context.Background(), &pb.SubscribeRequest{}) }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stalled candidate opening: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handoff deadline did not interrupt connector.Open")
	}
	if manager.active != old || old.ctx.Err() != nil {
		t.Fatal("failed opening stopped the old subscription")
	}
}

func TestPromotedSubscriptionOutlivesControlPass(t *testing.T) {
	manager := NewManager(&recordingBlockingConnector{}, HandlerFunc(func(context.Context, *pb.SubscribeUpdate) error { return nil }), Config{HandoffTimeout: time.Second})
	defer manager.Close()
	if err := manager.Start(context.Background(), &pb.SubscribeRequest{}); err != nil {
		t.Fatal(err)
	}
	control, cancel := context.WithCancel(context.Background())
	if err := manager.Handoff(control, &pb.SubscribeRequest{}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if manager.active.ctx.Err() != nil {
		t.Fatal("successful handoff inherited the completed control pass")
	}
	if err := manager.Handoff(context.Background(), &pb.SubscribeRequest{}); err != nil {
		t.Fatalf("next handoff after control cancellation: %v", err)
	}
}

type updatingOpenStream struct {
	blockingOpenStream
	updates <-chan *pb.SubscribeUpdate
}

func (s *updatingOpenStream) Recv() (*pb.SubscribeUpdate, error) {
	select {
	case update := <-s.updates:
		return update, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

type updatingFirstConnector struct {
	calls   atomic.Int32
	updates <-chan *pb.SubscribeUpdate
}

func (c *updatingFirstConnector) Open(ctx context.Context, _ *pb.SubscribeRequest) (OpenStream, error) {
	if c.calls.Add(1) == 1 {
		return &updatingOpenStream{blockingOpenStream: blockingOpenStream{ctx: ctx}, updates: c.updates}, nil
	}
	return &blockingOpenStream{ctx: ctx}, nil
}

func TestHandoffDeadlineDoesNotWaitForOldDurableHandler(t *testing.T) {
	updates := make(chan *pb.SubscribeUpdate, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	manager := NewManager(&updatingFirstConnector{updates: updates}, HandlerFunc(func(context.Context, *pb.SubscribeUpdate) error {
		close(entered)
		<-release
		return nil
	}), Config{HandoffTimeout: 50 * time.Millisecond})
	defer func() { close(release); manager.Close() }()
	if err := manager.Start(context.Background(), &pb.SubscribeRequest{}); err != nil {
		t.Fatal(err)
	}
	old := manager.active
	updates <- &pb.SubscribeUpdate{UpdateOneof: &pb.SubscribeUpdate_Slot{Slot: &pb.SubscribeUpdateSlot{Slot: 123}}}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("old handler did not acquire durable delivery")
	}
	result := make(chan error, 1)
	go func() { result <- manager.Handoff(context.Background(), &pb.SubscribeRequest{}) }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("handoff waiting for old durable work: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handoff deadline waited for an unrelated old handler")
	}
	if manager.active != old || old.ctx.Err() != nil || old.Frontier() != 0 {
		t.Fatal("aborted handoff changed ownership or acknowledged unfinished work")
	}
}
