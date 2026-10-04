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

// Handoff replaces the complete filter set without creating an observation
// gap. The candidate starts from the smaller of its requested FromSlot and the
// active durable frontier minus the configured overlap.
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
	promoted := false
	defer func() {
		if !promoted {
			candidate.stopAndWait()
		}
	}()
	// A promoted handoff owns the old session's shutdown: cancel AND join its
	// receive worker before returning, so no old delivery can outlive the
	// ownership swap. Registered before the delivery gate is taken so the gate
	// unlocks before the join waits on the frozen receive worker.
	oldJoined := false
	defer func() {
		if oldJoined {
			old.stopAndWait()
		}
	}()

	// Freeze the old stream at an application-durable boundary before candidate
	// replay reaches domain handlers. Its receive goroutine may have one frame
	// waiting behind this gate, while HTTP/2 flow control safely backpressures
	// subsequent frames.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-old.deliveryGate:
	}
	defer func() { old.deliveryGate <- struct{}{} }()
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

	if err := m.promoteCandidate(old, candidate, replacement); err != nil {
		return err
	}

	promoted = true
	oldJoined = true
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
	// Close owns the active receive worker's exit: cancel AND join so callers
	// never observe handled work after Close returns.
	if active != nil {
		active.stopAndWait()
	}
}

func (m *Manager) openSession(ctx context.Context, request *pb.SubscribeRequest) (*session, error) {
	opening, cancel := context.WithTimeout(ctx, m.config.HandoffTimeout)
	defer cancel()
	return m.openSessionBounded(ctx, opening, request)
}

// Opening a replacement is bounded by its control pass, but the resulting
// subscription belongs to the long-lived manager. Detach the temporary cancel
// hook before publishing it so a successful pass's deadline cannot stop it.
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

// promoteCandidate serializes the active-session swap with sessionFinished.
// If completion wins the lock, a terminal candidate cannot be promoted. If
// promotion wins, completion observes the candidate as active and reports its
// error through the fatal channel.
func (m *Manager) promoteCandidate(old, candidate *session, replacement *pb.SubscribeRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.active != old {
		return errors.New("active subscription changed during handoff")
	}
	if candidate.terminal {
		err := candidate.terminalErr
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
	s.terminal = true
	s.terminalErr = err
	isActive := !m.closed && m.active == s
	if isActive && err != nil && !errors.Is(err, context.Canceled) {
		select {
		case m.fatal <- err:
		default:
		}
	}
	m.mu.Unlock()
}

type session struct {
	ctx     context.Context
	cancel  context.CancelFunc
	stream  OpenStream
	handler Handler

	// deliveryGate freezes a durable frontier without blocking cancellation.
	deliveryGate chan struct{}
	frontier     atomic.Uint64
	progress     chan struct{}
	done         chan error
	start        chan struct{}
	startOnce    sync.Once
	stopOnce     sync.Once

	// terminal and terminalErr are guarded by the owning Manager's mu so
	// completion state and active-session promotion form one critical section.
	terminal    bool
	terminalErr error
}

func (s *session) run(onDone func(error)) {
	select {
	case <-s.start:
	case <-s.ctx.Done():
		onDone(context.Canceled)
		_ = s.stream.Close()
		s.done <- context.Canceled
		close(s.done)
		return
	}
	err := s.receive()
	// Publish terminal state before connection cleanup or waking Done readers.
	// This closes the promotion race between a non-blocking Done check and the
	// active session swap, even if transport cleanup is slow.
	onDone(err)
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

		select {
		case <-s.ctx.Done():
			return context.Canceled
		case <-s.deliveryGate:
		}
		if s.ctx.Err() != nil {
			s.deliveryGate <- struct{}{}
			return context.Canceled
		}
		if err := s.handler.Handle(s.ctx, update); err != nil {
			s.deliveryGate <- struct{}{}
			return fmt.Errorf("durably process LaserStream update: %w", err)
		}
		if slot, ok := updateSlot(update); ok {
			s.advance(slot)
		}
		s.deliveryGate <- struct{}{}
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

// stopAndWait cancels the session and blocks until its receive worker has
// fully exited, including transport cleanup and the final done publication.
func (s *session) stopAndWait() {
	s.Stop()
	<-s.done
}
func handoffReplayStart(frontier, overlap uint64, requested *uint64) uint64 {
	// A zero frontier means the active stream has not established a durable
	// boundary yet. Preserve the replacement's validated cursor rather than
	// turning an immediate handoff into a genesis replay.
	if frontier == 0 && requested != nil {
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
