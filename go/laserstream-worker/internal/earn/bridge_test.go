package earn

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
)

func TestBridgeContinuouslyDrainsLogsBeforeAcknowledgement(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires POSIX shell")
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, "bridge")
	script := `#!/bin/sh
echo 'EARN_BRIDGE_READY'
while IFS= read -r line; do
  i=0
  while [ "$i" -lt 5000 ]; do
    echo "bridge background log $i"
    i=$((i + 1))
  done
  echo 'EARN_BRIDGE_ACK {"ok":true,"slot":42,"error":null}'
done
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	bridge, err := StartBridge(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), binary)
	if err != nil {
		t.Fatal(err)
	}
	update := &pb.SubscribeUpdate{UpdateOneof: &pb.SubscribeUpdate_Transaction{Transaction: &pb.SubscribeUpdateTransaction{Slot: 42}}}
	if err := bridge.HandleTransaction(ctx, update); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = bridge.Close()
}
