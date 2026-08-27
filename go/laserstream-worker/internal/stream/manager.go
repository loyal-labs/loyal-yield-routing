package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"google.golang.org/protobuf/proto"
)

// Handler must not return until the update has reached its durable boundary.
// Returning an error makes that physical stream unusable; the caller can then
// reconnect from its durable cursor without allowing a receive-side slot to
// outrun persistence.
type Handler interface {
	Handle(context.Context, *pb.SubscribeUpdate) error
}

type HandlerFunc func(context.Context, *pb.SubscribeUpdate) error

func (f HandlerFunc) Handle(ctx context.Context, update *pb.SubscribeUpdate) error {
	return f(ctx, update)
}

type Config struct {
	ReplayOverlapSlots uint64
	HandoffTimeout     time.Duration
}

func (c Config) withDefaults() Config {
	if c.ReplayOverlapSlots == 0 {
		c.ReplayOverlapSlots = 32
	}
	if c.HandoffTimeout == 0 {
		c.HandoffTimeout = 2 * time.Minute
	}
	return c
}

// Manager owns exactly one active logical subscription. A handoff briefly
// opens a second physical subscription with the complete replacement filter
// set. The network streams overlap, but the old durable-delivery gate is frozen
// before replay reaches domain handlers. Once the candidate catches the stable
// frontier, ownership swaps atomically. Handlers must remain idempotent because
// the negative slot overlap deliberately replays already durable updates.
type Manager struct {
	connector Connector
	handler   Handler
	config    Config

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.RWMutex
	active  *session
	request *pb.SubscribeRequest
	closed  bool

	handoffMu sync.Mutex
	fatal     chan error
}

func NewManager(connector Connector, handler Handler, config Config) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		connector: connector,
		handler:   handler,
		config:    config.withDefaults(),
		ctx:       ctx,
		cancel:    cancel,
		fatal:     make(chan error, 1),
	}
}

func (m *Manager) Start(ctx context.Context, request *pb.SubscribeRequest) error {
	if request == nil {
		return errors.New("subscription request is required")
	}
	if m.connector == nil {
		return errors.New("LaserStream connector is required")
	}
	if m.handler == nil {
		return errors.New("durable LaserStream handler is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("subscription manager is closed")
	}
	if m.active != nil {
		return errors.New("subscription manager is already started")
	}

	root, cancel := context.WithCancel(ctx)
	m.cancel()
	m.ctx = root
	m.cancel = cancel

	s, err := m.openSession(root, request)
	if err != nil {
		return err
	}
	m.active = s
	m.request = cloneRequest(request)
	s.Start()
	return nil
}

// Handoff replaces the complete filter set without creating an observation
// gap. The candidate starts from the smaller of its requested FromSlot and the
// active durable frontier minus the configured overlap.
func (m *Manager) Handoff(ctx context.Context, replacement *pb.SubscribeRequest) error {
	if replacement == nil {
		return errors.New("replacement subscription request is required")
	}
	m.handoffMu.Lock()
	defer m.handoffMu.Unlock()

	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return errors.New("subscription manager is closed")
	}
	old := m.active
	root := m.ctx
	m.mu.RUnlock()
	if old == nil {
		return errors.New("subscription manager is not started")
	}

	request := cloneRequest(replacement)
	frontier := old.Frontier()
	request.FromSlot = uint64Pointer(handoffReplayStart(
		frontier,
		m.config.ReplayOverlapSlots,
		request.FromSlot,
	))

	candidate, err := m.openSession(root, request)
	if err != nil {
		return fmt.Errorf("open handoff candidate: %w", err)
	}
	promoted := false
	defer func() {
		if !promoted {
			candidate.Stop()
		}
	}()

	deadline := time.NewTimer(m.config.HandoffTimeout)
	defer deadline.Stop()

	// Freeze the old stream at an application-durable boundary before candidate
	// replay reaches domain handlers. Its receive goroutine may have one frame
	// waiting behind this gate, while HTTP/2 flow control safely backpressures
	// subsequent frames.
	old.deliveryMu.Lock()
	defer old.deliveryMu.Unlock()
	target := old.Frontier()
	// Start durable candidate delivery only after old delivery is frozen. The
	// two network subscriptions overlap, but domain handlers never receive old
	// and replayed state concurrently or race their in-memory projections.
	candidate.Start()

	for candidate.Frontier() < target {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-candidate.Done():
			return fmt.Errorf("handoff candidate stopped before slot %d: %w", target, err)
		case <-candidate.Progress():
		case <-deadline.C:
			return fmt.Errorf(
				"handoff candidate timed out at slot %d while old stream was durable through %d",
				candidate.Frontier(), target,
			)
		}
	}
	select {
	case err := <-candidate.Done():
		return fmt.Errorf("handoff candidate stopped at promotion frontier %d: %w", target, err)
	default:
	}

	m.mu.Lock()
	if m.closed || m.active != old {
		m.mu.Unlock()
		return errors.New("active subscription changed during handoff")
	}
	m.active = candidate
	m.request = cloneRequest(replacement)
	m.mu.Unlock()

	promoted = true
	old.Stop()
	return nil
}

func (m *Manager) ActiveFrontier() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active == nil {
		return 0
	}
	return m.active.Frontier()
}

func (m *Manager) Errors() <-chan error { return m.fatal }

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	active := m.active
	m.active = nil
	m.cancel()
	m.mu.Unlock()
	if active != nil {
		active.Stop()
	}
}

func (m *Manager) openSession(ctx context.Context, request *pb.SubscribeRequest) (*session, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	wire, err := m.connector.Open(sessionCtx, cloneRequest(request))
	if err != nil {
		cancel()
		return nil, err
	}
	s := &session{
		ctx:      sessionCtx,
		cancel:   cancel,
		stream:   wire,
		handler:  m.handler,
		progress: make(chan struct{}, 1),
		done:     make(chan error, 1),
		start:    make(chan struct{}),
	}
	go s.run(func(err error) {
		m.mu.RLock()
		isActive := !m.closed && m.active == s
		m.mu.RUnlock()
		if isActive && err != nil && !errors.Is(err, context.Canceled) {
			select {
			case m.fatal <- err:
			default:
			}
		}
	})
	return s, nil
}

type session struct {
	ctx     context.Context
	cancel  context.CancelFunc
	stream  OpenStream
	handler Handler

	// deliveryMu turns the current frontier into a stable handoff boundary.
	deliveryMu sync.Mutex
	frontier   atomic.Uint64
	progress   chan struct{}
	done       chan error
	start      chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
}

func (s *session) run(onDone func(error)) {
	select {
	case <-s.start:
	case <-s.ctx.Done():
		s.done <- context.Canceled
		close(s.done)
		onDone(context.Canceled)
		return
	}
	err := s.receive()
	_ = s.stream.Close()
	s.done <- err
	close(s.done)
	onDone(err)
}

func (s *session) receive() error {
	for {
		update, err := s.stream.Recv()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) && s.ctx.Err() != nil {
				return context.Canceled
			}
			return fmt.Errorf("receive LaserStream update: %w", err)
		}

		if update.GetPing() != nil {
			if err := s.stream.Send(&pb.SubscribeRequest{
				Ping: &pb.SubscribeRequestPing{Id: 1},
			}); err != nil {
				return fmt.Errorf("send LaserStream pong: %w", err)
			}
			continue
		}
		if update.GetPong() != nil {
			continue
		}

		s.deliveryMu.Lock()
		if s.ctx.Err() != nil {
			s.deliveryMu.Unlock()
			return context.Canceled
		}
		if err := s.handler.Handle(s.ctx, update); err != nil {
			s.deliveryMu.Unlock()
			return fmt.Errorf("durably process LaserStream update: %w", err)
		}
		if slot, ok := updateSlot(update); ok {
			s.advance(slot)
		}
		s.deliveryMu.Unlock()
	}
}

func (s *session) advance(slot uint64) {
	for {
		current := s.frontier.Load()
		if slot <= current || s.frontier.CompareAndSwap(current, slot) {
			break
		}
	}
	select {
	case s.progress <- struct{}{}:
	default:
	}
}

func (s *session) Frontier() uint64          { return s.frontier.Load() }
func (s *session) Progress() <-chan struct{} { return s.progress }
func (s *session) Done() <-chan error        { return s.done }
func (s *session) Start()                    { s.startOnce.Do(func() { close(s.start) }) }
func (s *session) Stop()                     { s.stopOnce.Do(s.cancel) }
func handoffReplayStart(frontier, overlap uint64, requested *uint64) uint64 {
	overlapStart := frontier - min(frontier, overlap)
	if requested != nil && *requested < overlapStart {
		return *requested
	}
	return overlapStart
}

func uint64Pointer(value uint64) *uint64 { return &value }
func cloneRequest(r *pb.SubscribeRequest) *pb.SubscribeRequest {
	return proto.Clone(r).(*pb.SubscribeRequest)
}

func updateSlot(update *pb.SubscribeUpdate) (uint64, bool) {
	if update == nil {
		return 0, false
	}
	switch value := update.UpdateOneof.(type) {
	case *pb.SubscribeUpdate_Account:
		if value.Account != nil {
			return value.Account.Slot, true
		}
	case *pb.SubscribeUpdate_Slot:
		if value.Slot != nil {
			return value.Slot.Slot, true
		}
	case *pb.SubscribeUpdate_Transaction:
		if value.Transaction != nil {
			return value.Transaction.Slot, true
		}
	case *pb.SubscribeUpdate_TransactionStatus:
		if value.TransactionStatus != nil {
			return value.TransactionStatus.Slot, true
		}
	case *pb.SubscribeUpdate_Block:
		if value.Block != nil {
			return value.Block.Slot, true
		}
	case *pb.SubscribeUpdate_BlockMeta:
		if value.BlockMeta != nil {
			return value.BlockMeta.Slot, true
		}
	case *pb.SubscribeUpdate_Entry:
		if value.Entry != nil {
			return value.Entry.Slot, true
		}
	}
	return 0, false
}
