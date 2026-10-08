package fleetexec

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A failed executor tick names its cause instead of a bare "tick failed".
func TestExecutorTickFailureLogsItsCause(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	// An incomplete claim is refused before any database work.
	w := &Worker{config: Config{BatchSize: 1, TickInterval: time.Hour, Facts: testFacts()}, store: &Store{}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.Run(ctx); err == nil {
		t.Fatal("run returned without its context ending")
	}
	out := buf.String()
	if !strings.Contains(out, "fleetexec tick failed") || !strings.Contains(out, "claim recovery work: incomplete claim request") {
		t.Fatalf("tick failure logged without its cause: %s", out)
	}
}
