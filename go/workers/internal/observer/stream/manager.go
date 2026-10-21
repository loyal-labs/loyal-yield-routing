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
	ended   map[*session]error

	handoffGate chan struct{}
	fatal       chan error
}

func NewManager(connector Connector, handler Handler, config Config) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &Manager{
		connector:   connector,
		handler:     handler,
		config:      config.withDefaults(),
		ctx:         ctx,
		cancel:      cancel,
		ended:       map[*session]error{},
		fatal:       make(chan error, 1),
		handoffGate: gate,
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

func (m *Manager) Handoff(ctx context.Context, replacement *pb.SubscribeRequest) error {
	if replacement == nil {
		return errors.New("replacement subscription request is required")
	}
	ctx, cancel := context.WithTimeout(ctx, m.config.HandoffTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.handoffGate:
	}
	defer func() { m.handoffGate <- struct{}{} }()

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

	candidate, err := m.openSessionBounded(root, ctx, request)
	if err != nil {
		return fmt.Errorf("open handoff candidate: %w", err)
	}
	if err := m.replayBehindFrozen(ctx, old, candidate, replacement); err != nil {
		m.retire(candidate)
		return err
	}
	m.retire(old)
	return nil
}

func (m *Manager) replayBehindFrozen(ctx context.Context, old, candidate *session, replacement *pb.SubscribeRequest) error {
	if err := old.acquireDelivery(ctx); err != nil {
		return err
	}
	defer old.releaseDelivery()
	target := old.Frontier()
	candidate.Start()

	for candidate.Frontier() < target {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-candidate.Done():
			return fmt.Errorf("handoff candidate stopped before slot %d: %w", target, err)
		case <-candidate.Progress():
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case err := <-candidate.Done():
		return fmt.Errorf("handoff candidate stopped at promotion frontier %d: %w", target, err)
	default:
	}
	return m.promoteCandidate(old, candidate, replacement)
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
		m.retire(active)
	}
}

func (m *Manager) openSession(ctx context.Context, request *pb.SubscribeRequest) (*session, error) {
	opening, cancel := context.WithTimeout(ctx, m.config.HandoffTimeout)
	defer cancel()
	return m.openSessionBounded(ctx, opening, request)
}

func (m *Manager) openSessionBounded(parent, opening context.Context, request *pb.SubscribeRequest) (*session, error) {
	sessionCtx, cancel := context.WithCancel(parent)
	stopOpening := context.AfterFunc(opening, cancel)
	wire, err := m.connector.Open(sessionCtx, cloneRequest(request))
	stopOpening()
	if opening.Err() != nil {
		cancel()
		if wire != nil {
			_ = wire.Close()
		}
		return nil, opening.Err()
	}
	if err != nil {
		cancel()
		return nil, err
	}
	s := &session{
		ctx:          sessionCtx,
		cancel:       cancel,
		stream:       wire,
		handler:      m.handler,
		progress:     make(chan struct{}, 1),
		done:         make(chan error, 1),
		start:        make(chan struct{}),
		deliveryGate: make(chan struct{}, 1),
	}
	s.deliveryGate <- struct{}{}
	go s.run(func(err error) {
		m.sessionFinished(s, err)
	})
	return s, nil
}

func (m *Manager) promoteCandidate(old, candidate *session, replacement *pb.SubscribeRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.active != old {
		return errors.New("active subscription changed during handoff")
	}
	if err, ended := m.ended[candidate]; ended {
		if err == nil {
			err = errors.New("candidate stopped without an error")
		}
		return fmt.Errorf("handoff candidate stopped before promotion: %w", err)
	}
	m.active = candidate
	m.request = cloneRequest(replacement)
	return nil
}

func (m *Manager) sessionFinished(s *session, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ended[s] = err
	if !m.closed && m.active == s && err != nil && !errors.Is(err, context.Canceled) {
		select {
		case m.fatal <- err:
		default:
		}
	}
}

func (m *Manager) retire(s *session) {
	s.Stop()
	<-s.done
	m.mu.Lock()
	delete(m.ended, s)
	m.mu.Unlock()
}

type session struct {
	ctx     context.Context
	cancel  context.CancelFunc
	stream  OpenStream
	handler Handler

	deliveryGate chan struct{}
	frontier     atomic.Uint64
	progress     chan struct{}
	done         chan error
	start        chan struct{}
	startOnce    sync.Once
	stopOnce     sync.Once
}

func (s *session) run(publishTerminal func(error)) {
	select {
	case <-s.start:
		s.finish(s.receive(), publishTerminal)
	case <-s.ctx.Done():
		s.finish(context.Canceled, publishTerminal)
	}
}

func (s *session) finish(err error, publishTerminal func(error)) {
	publishTerminal(err)
	_ = s.stream.Close()
	s.done <- err
	close(s.done)
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

		if s.acquireDelivery(s.ctx) != nil {
			return context.Canceled
		}
		err = s.deliver(update)
		s.releaseDelivery()
		if err != nil {
			return err
		}
	}
}

func (s *session) acquireDelivery(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.deliveryGate:
		return nil
	}
}

func (s *session) releaseDelivery() { s.deliveryGate <- struct{}{} }

func (s *session) deliver(update *pb.SubscribeUpdate) error {
	if s.ctx.Err() != nil {
		return context.Canceled
	}
	if err := s.handler.Handle(s.ctx, update); err != nil {
		return fmt.Errorf("durably process LaserStream update: %w", err)
	}
	if slot, ok := updateSlot(update); ok {
		s.advance(slot)
	}
	return nil
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
	activeHasNoDurableBoundary := frontier == 0
	if activeHasNoDurableBoundary && requested != nil {
		return *requested
	}
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
