package stream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
)

func TestHandoffReplayStartUsesNegativeOverlap(t *testing.T) {
	requested := uint64(200)
	if got := handoffReplayStart(150, 32, &requested); got != 118 {
		t.Fatalf("handoff replay start = %d, want frontier minus 32 = 118", got)
	}
}

func TestHandoffReplayStartHonorsDeeperNewBinding(t *testing.T) {
	requested := uint64(90)
	if got := handoffReplayStart(150, 32, &requested); got != 90 {
		t.Fatalf("handoff replay start = %d, want new binding start 90", got)
	}
}

func TestHandoffReplayStartPreservesRequestedCursorWithoutFrontier(t *testing.T) {
	requested := uint64(200)
	if got := handoffReplayStart(0, 32, &requested); got != 200 {
		t.Fatalf("immediate handoff replay start = %d, want requested cursor 200", got)
	}
}

func TestHandoffReplayStartSaturatesAtZero(t *testing.T) {
	if got := handoffReplayStart(10, 32, nil); got != 0 {
		t.Fatalf("handoff replay start = %d, want 0", got)
	}
}

func TestImmediateHandoffPreservesRequestedCursor(t *testing.T) {
	connector := &recordingBlockingConnector{}
	manager := NewManager(connector, HandlerFunc(func(context.Context, *pb.SubscribeUpdate) error {
		return nil
	}), Config{HandoffTimeout: time.Second})
	defer manager.Close()

	initialCursor := uint64(100)
	if err := manager.Start(context.Background(), &pb.SubscribeRequest{FromSlot: &initialCursor}); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	replacementCursor := uint64(200)
	if err := manager.Handoff(context.Background(), &pb.SubscribeRequest{FromSlot: &replacementCursor}); err != nil {
		t.Fatalf("immediate handoff: %v", err)
	}

	if got := connector.fromSlots(); len(got) != 2 || got[1] != replacementCursor {
		t.Fatalf("opened stream cursors = %v, want [100 200]", got)
	}
}

func TestPromotionRejectsCandidateThatAlreadyFinished(t *testing.T) {
	manager := NewManager(nil, nil, Config{})
	defer manager.cancel()
	old := &session{}
	candidate := &session{}
	manager.active = old
	terminalErr := errors.New("candidate failed")

	manager.sessionFinished(candidate, terminalErr)
	err := manager.promoteCandidate(old, candidate, &pb.SubscribeRequest{})
	if !errors.Is(err, terminalErr) {
		t.Fatalf("promotion error = %v, want terminal candidate error", err)
	}
	if manager.active != old {
		t.Fatal("terminal candidate replaced the live old session")
	}
}

func TestCandidateFailureAfterPromotionIsFatal(t *testing.T) {
	manager := NewManager(nil, nil, Config{})
	defer manager.cancel()
	old := &session{}
	candidate := &session{}
	manager.active = old
	terminalErr := errors.New("candidate failed")

	if err := manager.promoteCandidate(old, candidate, &pb.SubscribeRequest{}); err != nil {
		t.Fatalf("promote live candidate: %v", err)
	}
	manager.sessionFinished(candidate, terminalErr)

	select {
	case got := <-manager.Errors():
		if !errors.Is(got, terminalErr) {
			t.Fatalf("fatal error = %v, want %v", got, terminalErr)
		}
	default:
		t.Fatal("promoted candidate failure was silently suppressed")
	}
}

func TestCandidateCompletionRacingPromotionNeverStopsSilently(t *testing.T) {
	terminalErr := errors.New("candidate failed")
	for range 1_000 {
		manager := NewManager(nil, nil, Config{})
		old := &session{}
		candidate := &session{}
		manager.active = old
		start := make(chan struct{})
		promotionResult := make(chan error, 1)
		completionDone := make(chan struct{})

		go func() {
			<-start
			manager.sessionFinished(candidate, terminalErr)
			close(completionDone)
		}()
		go func() {
			<-start
			promotionResult <- manager.promoteCandidate(old, candidate, &pb.SubscribeRequest{})
		}()
		close(start)
		promotionErr := <-promotionResult
		<-completionDone

		if promotionErr == nil {
			select {
			case got := <-manager.Errors():
				if !errors.Is(got, terminalErr) {
					t.Fatalf("fatal error = %v, want %v", got, terminalErr)
				}
			default:
				t.Fatal("candidate won promotion but its concurrent failure was silent")
			}
		} else {
			if !errors.Is(promotionErr, terminalErr) {
				t.Fatalf("promotion error = %v, want %v", promotionErr, terminalErr)
			}
			if manager.active != old {
				t.Fatal("failed candidate replaced old session")
			}
		}
		manager.cancel()
	}
}

type recordingBlockingConnector struct {
	mu      sync.Mutex
	cursors []uint64
}

func (c *recordingBlockingConnector) Open(ctx context.Context, request *pb.SubscribeRequest) (OpenStream, error) {
	c.mu.Lock()
	c.cursors = append(c.cursors, request.GetFromSlot())
	c.mu.Unlock()
	return &blockingOpenStream{ctx: ctx}, nil
}

func (c *recordingBlockingConnector) fromSlots() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]uint64(nil), c.cursors...)
}

type blockingOpenStream struct {
	ctx context.Context
}

func (s *blockingOpenStream) Recv() (*pb.SubscribeUpdate, error) {
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func (*blockingOpenStream) Send(*pb.SubscribeRequest) error { return nil }
func (*blockingOpenStream) CloseSend() error                { return nil }
func (*blockingOpenStream) Close() error                    { return nil }

// slowCloseStream simulates a transport whose Close takes real time. The
// manager owns cancelling AND joining the receive worker, so a completed
// Manager.Close or promoted Handoff must imply the stream Close has returned.
type slowCloseStream struct {
	ctx       context.Context
	closeOnce sync.Once
	closed    chan struct{}
}

func newSlowCloseStream(ctx context.Context) *slowCloseStream {
	return &slowCloseStream{ctx: ctx, closed: make(chan struct{})}
}

func (s *slowCloseStream) Recv() (*pb.SubscribeUpdate, error) {
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}
func (*slowCloseStream) Send(*pb.SubscribeRequest) error { return nil }
func (*slowCloseStream) CloseSend() error                { return nil }
func (s *slowCloseStream) Close() error {
	s.closeOnce.Do(func() {
		time.Sleep(200 * time.Millisecond)
		close(s.closed)
	})
	return nil
}

type slowCloseConnector struct {
	mu      sync.Mutex
	streams []*slowCloseStream
}

func (c *slowCloseConnector) Open(ctx context.Context, _ *pb.SubscribeRequest) (OpenStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stream := newSlowCloseStream(ctx)
	c.streams = append(c.streams, stream)
	return stream, nil
}

func (c *slowCloseConnector) streamsSnapshot() []*slowCloseStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*slowCloseStream(nil), c.streams...)
}

func TestCloseCancelsAndJoinsActiveReceiveWorker(t *testing.T) {
	manager := NewManager(&slowCloseConnector{}, HandlerFunc(func(context.Context, *pb.SubscribeUpdate) error {
		return nil
	}), Config{})
	cursor := uint64(10)
	if err := manager.Start(context.Background(), &pb.SubscribeRequest{FromSlot: &cursor}); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	manager.Close()
	for _, stream := range manager.connector.(*slowCloseConnector).streamsSnapshot() {
		select {
		case <-stream.closed:
		default:
			t.Fatal("Manager.Close returned before the receive worker's transport closed")
		}
	}
}

func TestPromotedHandoffCancelsAndJoinsOldReceiveWorker(t *testing.T) {
	connector := &slowCloseConnector{}
	manager := NewManager(connector, HandlerFunc(func(context.Context, *pb.SubscribeUpdate) error {
		return nil
	}), Config{HandoffTimeout: 5 * time.Second})
	defer manager.Close()
	cursor := uint64(10)
	if err := manager.Start(context.Background(), &pb.SubscribeRequest{FromSlot: &cursor}); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	// The old frontier is zero, so the candidate promotes immediately and the
	// frozen old receive worker must be cancelled and joined before the handoff
	// returns.
	if err := manager.Handoff(context.Background(), &pb.SubscribeRequest{FromSlot: &cursor}); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	streams := connector.streamsSnapshot()
	if len(streams) != 2 {
		t.Fatalf("opened streams = %d, want initial plus candidate", len(streams))
	}
	select {
	case <-streams[0].closed:
	default:
		t.Fatal("promoted handoff left the old receive worker's transport open")
	}
}
