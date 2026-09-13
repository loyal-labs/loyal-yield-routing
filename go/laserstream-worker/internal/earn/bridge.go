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

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"google.golang.org/protobuf/proto"
)

const (
	bridgePrefix = "EARN_BRIDGE_ACK "
	bridgeReady  = "EARN_BRIDGE_READY"
)

type Bridge struct {
	logger  *slog.Logger
	command *exec.Cmd
	stdin   io.WriteCloser
	mu      sync.Mutex
	acks    chan bridgeAck
	ready   chan struct{}
	done    chan error
}

type bridgeAck struct {
	OK    bool    `json:"ok"`
	Slot  uint64  `json:"slot"`
	Error *string `json:"error"`
}

func StartBridge(ctx context.Context, logger *slog.Logger, binary string) (*Bridge, error) {
	if binary == "" {
		binary = "/usr/local/bin/earn-domain-bridge"
	}
	command := exec.CommandContext(ctx, binary)
	command.Stderr = os.Stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = command.Start(); err != nil {
		return nil, fmt.Errorf("start Earn domain bridge: %w", err)
	}
	bridge := &Bridge{logger: logger, command: command, stdin: stdin, acks: make(chan bridgeAck, 1), ready: make(chan struct{}), done: make(chan error, 1)}
	go bridge.readOutput(stdout)
	go func() {
		bridge.done <- command.Wait()
		close(bridge.done)
	}()
	select {
	case <-bridge.ready:
		return bridge, nil
	case err := <-bridge.done:
		return nil, fmt.Errorf("earn domain bridge exited during startup: %w", err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *Bridge) readOutput(output io.Reader) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == bridgeReady {
			close(b.ready)
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
		b.acks <- ack
	}
	if err := scanner.Err(); err != nil {
		b.logger.Error("Earn bridge output reader failed", "error", err)
	}
}

func (b *Bridge) HandleTransaction(ctx context.Context, update *pb.SubscribeUpdate) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	bytes, err := proto.Marshal(update)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(b.stdin, base64.StdEncoding.EncodeToString(bytes)); err != nil {
		return fmt.Errorf("send policy update to Earn bridge: %w", err)
	}
	select {
	case ack := <-b.acks:
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
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Bridge) Done() <-chan error { return b.done }
func (b *Bridge) Close() error {
	b.mu.Lock()
	_ = b.stdin.Close()
	b.mu.Unlock()
	return <-b.done
}
