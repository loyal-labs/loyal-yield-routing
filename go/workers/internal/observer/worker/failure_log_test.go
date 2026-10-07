package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/prometheus/client_golang/prometheus"
)

// An operator reading the ATA projection warning must see why it failed.
func TestATAProjectionFailureLogsItsCause(t *testing.T) {
	var output bytes.Buffer
	runtime := &Runtime{cfg: config.Config{ATAStream: "production"}, logger: slog.New(slog.NewJSONHandler(&output, nil)), facts: engine.NewFacts(prometheus.NewRegistry())}
	runtime.ataProjectionFailed(context.Background(), errors.New("capture cursor relation missing"))
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode log record %q: %v", output.String(), err)
	}
	if record["error"] != "capture cursor relation missing" {
		t.Fatalf("ATA projection failure log = %v, want its cause", record)
	}
}
