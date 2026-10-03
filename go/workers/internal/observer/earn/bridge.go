package earn

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"google.golang.org/protobuf/proto"
)

const (
	bridgePrefix = "EARN_BRIDGE_ACK "
	bridgeReady  = "EARN_BRIDGE_READY"

	defaultBridgeBinary        = "/usr/local/bin/earn-domain-bridge"
	defaultBridgeStartupBound  = 20 * time.Second
	defaultBridgeShutdownBound = 10 * time.Second
)

// BridgeConfig is the explicit, allowlisted child environment. The bridge child
// never inherits the worker process environment: POLICY_KEYPAIR and other
// signing capabilities must stay outside the observer's projection child.
type BridgeConfig struct {
	Binary         string
	Env            []string
	StartupTimeout time.Duration
	// ShutdownTimeout bounds Close so a stuck child cannot hang process exit.
	ShutdownTimeout time.Duration
}

type Bridge struct {
	logger  *slog.Logger
	command *exec.Cmd

	cancel context.CancelFunc
	stdin  io.WriteCloser

	// writeMu serializes stdin writes against Close; requestMu serializes
	// request/acknowledgement rounds so one round never consumes another's
	// acknowledgement. Close never takes requestMu, so a cancelled or stuck
	// request cannot block shutdown.
	writeMu   sync.Mutex
	requestMu sync.Mutex
	acks      chan bridgeAck
	ready     chan struct{}
	reader    chan struct{}
	done      chan error
	shutdown  time.Duration
	abandoned atomic.Bool
}

type bridgeAck struct {
	OK    bool    `json:"ok"`
	Slot  uint64  `json:"slot"`
	Error *string `json:"error"`
}

func StartBridge(ctx context.Context, logger *slog.Logger, config BridgeConfig) (*Bridge, error) {
	if logger == nil {
		logger = slog.Default()
	}
	binary := config.Binary
	if binary == "" {
		binary = defaultBridgeBinary
	}
	startupBound := config.StartupTimeout
	if startupBound <= 0 {
		startupBound = defaultBridgeStartupBound
	}
	shutdownBound := config.ShutdownTimeout
	if shutdownBound <= 0 {
		shutdownBound = defaultBridgeShutdownBound
	}
	// The bridge owns a cancellable child of the caller context: runtime
	// cancellation kills the child, and Close can cancel it independently
	// without waiting on any in-flight request.
	bridgeCtx, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(bridgeCtx, binary)
	command.Env = config.Env
	command.Stderr = os.Stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err = command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start Earn domain bridge: %w", err)
	}
	bridge := &Bridge{
		logger: logger, command: command, cancel: cancel, stdin: stdin,
		acks:  make(chan bridgeAck, 1),
		ready: make(chan struct{}), reader: make(chan struct{}),
		done: make(chan error, 1), shutdown: shutdownBound,
	}
	go bridge.readOutput(stdout)
	go func() {
		bridge.done <- command.Wait()
		close(bridge.done)
	}()
	startup := time.NewTimer(startupBound)
	defer startup.Stop()
	select {
	case <-bridge.ready:
		return bridge, nil
	case err := <-bridge.done:
		bridge.cancel()
		<-bridge.reader
		return nil, fmt.Errorf("earn domain bridge exited during startup: %w", err)
	case <-bridgeCtx.Done():
		bridge.cancel()
		<-bridge.done
		<-bridge.reader
		if ctx.Err() != nil {
			return nil, fmt.Errorf("earn domain bridge startup abandoned: %w", ctx.Err())
		}
		return nil, fmt.Errorf("earn domain bridge startup exceeded %s", startupBound)
	case <-startup.C:
		bridge.cancel()
		<-bridge.done
		<-bridge.reader
		return nil, fmt.Errorf("earn domain bridge did not report ready within %s", startupBound)
	}
}

func (b *Bridge) readOutput(output io.Reader) {
	defer close(b.reader)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == bridgeReady {
			select {
			case <-b.ready:
			default:
				close(b.ready)
			}
			continue
		}
		if !strings.HasPrefix(line, bridgePrefix) {
			b.logger.Info("earn domain bridge", "message", line)
			continue
		}
		var ack bridgeAck
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, bridgePrefix)), &ack); err != nil {
			message := fmt.Sprintf("decode Earn bridge acknowledgement: %v", err)
			ack.Error = &message
		}
		// Never block the reader on an unconsumed acknowledgement: a request
		// that already timed out or was cancelled owns no future receiver, and
		// a blocked reader would deadlock the child on its next acknowledgement.
		select {
		case b.acks <- ack:
		default:
			b.logger.Warn("dropped unconsumed Earn bridge acknowledgement", "slot", ack.Slot)
		}
	}
	if err := scanner.Err(); err != nil {
		b.logger.Error("Earn bridge output reader failed", "error", err)
	}
}

func (b *Bridge) HandleTransaction(ctx context.Context, update *pb.SubscribeUpdate) error {
	if update == nil || update.GetTransaction() == nil {
		return fmt.Errorf("Earn bridge requires a transaction update")
	}
	// After a request is abandoned no later ACK may be used by this child.
	// Restart from the durable cursor rather than correlating ACKs by slot.
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(requestCtx, func() { b.abandoned.Store(true); b.cancel() })
	defer stop()
	b.requestMu.Lock()
	defer b.requestMu.Unlock()
	if b.abandoned.Load() || requestCtx.Err() != nil {
		return fmt.Errorf("Earn bridge request channel was abandoned")
	}
	// Acknowledgements left over from a cancelled or timed-out round must never
	// satisfy this request; the slot comparison alone cannot distinguish a
	// stale acknowledgement from a genuine one at the same slot.
drain:
	for {
		select {
		case <-b.acks:
		default:
			break drain
		}
	}
	bytes, err := proto.Marshal(update)
	if err != nil {
		return err
	}
	b.writeMu.Lock()
	_, err = fmt.Fprintln(b.stdin, base64.StdEncoding.EncodeToString(bytes))
	b.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("send policy update to Earn bridge: %w", err)
	}
	select {
	case ack := <-b.acks:
		// True success requires the child to have acknowledged this exact
		// slot; absence (exit), failure and mismatched slots all stay errors.
		if !ack.OK {
			message := "unknown bridge failure"
			if ack.Error != nil {
				message = *ack.Error
			}
			return fmt.Errorf("earn policy projection failed: %s", message)
		}
		if transaction := update.GetTransaction(); transaction != nil && ack.Slot != transaction.GetSlot() {
			return fmt.Errorf("earn bridge acknowledged slot %d, expected %d", ack.Slot, transaction.GetSlot())
		}
		return nil
	case err := <-b.done:
		return fmt.Errorf("earn domain bridge exited before acknowledgement: %w", err)
	case <-requestCtx.Done():
		b.abandoned.Store(true)
		b.cancel()
		return requestCtx.Err()
	}
}

func (b *Bridge) Done() <-chan error { return b.done }

// Close stops the child and joins both owned goroutines. It does not wait for
// any in-flight request, and the shutdown bound keeps a wedged child from
// hanging process exit.
func (b *Bridge) Close() error {
	b.writeMu.Lock()
	_ = b.stdin.Close()
	b.writeMu.Unlock()
	b.cancel()
	shutdown := time.NewTimer(b.shutdown)
	defer shutdown.Stop()
	var err error
	select {
	case err = <-b.done:
	case <-shutdown.C:
		b.logger.Error("Earn bridge shutdown exceeded bound; abandoning child", "bound", b.shutdown)
		return fmt.Errorf("earn domain bridge did not exit within %s", b.shutdown)
	}
	select {
	case <-b.reader:
	case <-shutdown.C:
		return fmt.Errorf("earn domain bridge reader did not exit within %s", defaultBridgeShutdownBound)
	}
	return err
}
