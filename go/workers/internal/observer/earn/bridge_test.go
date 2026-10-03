package earn

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeBridgeScript(t *testing.T, body string) string {
	t.Helper()
	directory := t.TempDir()
	binary := filepath.Join(directory, "bridge")
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary
}

func transactionUpdate(slot uint64) *pb.SubscribeUpdate {
	return &pb.SubscribeUpdate{UpdateOneof: &pb.SubscribeUpdate_Transaction{Transaction: &pb.SubscribeUpdateTransaction{Slot: slot}}}
}

func TestBridgeContinuouslyDrainsLogsBeforeAcknowledgement(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires POSIX shell")
	}
	binary := writeBridgeScript(t, `echo 'EARN_BRIDGE_READY'
while IFS= read -r line; do
  i=0
  while [ "$i" -lt 5000 ]; do
    echo "bridge background log $i"
    i=$((i + 1))
  done
  echo 'EARN_BRIDGE_ACK {"ok":true,"slot":42,"error":null}'
done
`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	bridge, err := StartBridge(ctx, quietLogger(), BridgeConfig{Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.HandleTransaction(ctx, transactionUpdate(42)); err != nil {
		t.Fatal(err)
	}
	cancel()
	// Close joins the child even though the caller already cancelled the
	// bridge's context; the resulting kill signal is an expected exit cause.
	_ = bridge.Close()
}

func TestBridgeFailedAcknowledgementIsAnError(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires POSIX shell")
	}
	binary := writeBridgeScript(t, `echo 'EARN_BRIDGE_READY'
while IFS= read -r line; do
  echo 'EARN_BRIDGE_ACK {"ok":false,"slot":42,"error":"manifest rejected"}'
done
`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge, err := StartBridge(ctx, quietLogger(), BridgeConfig{Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bridge.Close() }()
	err = bridge.HandleTransaction(ctx, transactionUpdate(42))
	if err == nil || !strings.Contains(err.Error(), "manifest rejected") {
		t.Fatalf("failed acknowledgement = %v, want bridge failure", err)
	}
}

func TestBridgeExitWithoutAcknowledgementIsAnError(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires POSIX shell")
	}
	binary := writeBridgeScript(t, `echo 'EARN_BRIDGE_READY'
read -r line
exit 3
`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge, err := StartBridge(ctx, quietLogger(), BridgeConfig{Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bridge.Close() }()
	err = bridge.HandleTransaction(ctx, transactionUpdate(42))
	if err == nil || !strings.Contains(err.Error(), "exited before acknowledgement") {
		t.Fatalf("absent acknowledgement = %v, want exit failure", err)
	}
}

func TestCancelledRequestDoesNotHangClose(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires POSIX shell")
	}
	// The child acknowledges only after the request has already been abandoned,
	// so Close must join the child while a stale acknowledgement is in flight.
	binary := writeBridgeScript(t, `echo 'EARN_BRIDGE_READY'
while IFS= read -r line; do
  sleep 1
  echo 'EARN_BRIDGE_ACK {"ok":true,"slot":42,"error":null}'
done
`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge, err := StartBridge(ctx, quietLogger(), BridgeConfig{Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelRequest()
	if err := bridge.HandleTransaction(requestCtx, transactionUpdate(42)); err == nil {
		t.Fatal("abandoned request returned success")
	}
	done := make(chan error, 1)
	go func() { done <- bridge.Close() }()
	select {
	case <-done:
		// Any non-nil cause is the shutdown kill itself; the assertion is that
		// Close did not hang behind the cancelled request.
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung behind the cancelled request")
	}
}

func TestAbandonedRequestPoisonsChildBeforeSameSlotReplay(t *testing.T) {
	binary := writeBridgeScript(t, `echo 'EARN_BRIDGE_READY'
read -r line
sleep 1
echo 'EARN_BRIDGE_ACK {"ok":true,"slot":42,"error":null}'
sleep 30
`)
	bridge, err := StartBridge(context.Background(), quietLogger(), BridgeConfig{Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bridge.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := bridge.HandleTransaction(ctx, transactionUpdate(42)); err == nil {
		t.Fatal("abandoned first request returned success")
	}
	if err := bridge.HandleTransaction(context.Background(), transactionUpdate(42)); err == nil {
		t.Fatal("same-slot replay reused an abandoned acknowledgement channel")
	}
}

func TestStartBridgeBoundsStartupAndJoinsChild(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires POSIX shell")
	}
	binary := writeBridgeScript(t, `sleep 30
echo 'EARN_BRIDGE_READY'
`)
	started := time.Now()
	_, err := StartBridge(context.Background(), quietLogger(), BridgeConfig{Binary: binary, StartupTimeout: 250 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "did not report ready") {
		t.Fatalf("unbounded startup = %v, want readiness timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("startup took %s, want bounded failure", elapsed)
	}
}

func TestBridgeChildReceivesOnlyAllowlistedEnvironment(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires POSIX shell")
	}
	t.Setenv("POLICY_KEYPAIR", "signing-capability-must-not-leak")
	binary := writeBridgeScript(t, `echo 'EARN_BRIDGE_READY'
while IFS= read -r line; do
  if [ -n "${POLICY_KEYPAIR+x}" ]; then leaked=signing; else leaked=clean; fi
  echo "EARN_BRIDGE_ACK {\"ok\":false,\"slot\":1,\"error\":\"signing=$leaked neon=${NEON_DATABASE_URL-unset} cluster=${SOLANA_CLUSTER-unset}\"}"
done
`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge, err := StartBridge(ctx, quietLogger(), BridgeConfig{
		Binary: binary,
		Env:    []string{"NEON_DATABASE_URL=postgres://observer", "SOLANA_CLUSTER=mainnet-beta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bridge.Close() }()
	err = bridge.HandleTransaction(ctx, transactionUpdate(1))
	if err == nil {
		t.Fatal("expected rejection acknowledgement carrying environment evidence")
	}
	message := err.Error()
	if strings.Contains(message, "signing=signing") {
		t.Fatalf("signing capability leaked into bridge child: %v", message)
	}
	if !strings.Contains(message, "signing=clean") || !strings.Contains(message, "neon=postgres://observer") || !strings.Contains(message, "cluster=mainnet-beta") {
		t.Fatalf("allowlisted configuration missing from child environment: %v", message)
	}
}
