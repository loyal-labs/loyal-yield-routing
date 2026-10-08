package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
)

func TestRejectionCountsSummarizesFleetWithoutVaultIDs(t *testing.T) {
	rejections := make(map[int64]string)
	for id := int64(1); id <= 1600; id++ {
		rejections[id] = "no_eligible_target"
	}
	rejections[1601] = "target_capacity_exhausted"
	counts := rejectionCounts(rejections)
	if len(counts) != 2 || counts["no_eligible_target"] != 1600 || counts["target_capacity_exhausted"] != 1 {
		t.Fatalf("unexpected summary: %v", counts)
	}
	encoded, err := json.Marshal(counts)
	if err != nil || len(encoded) > 100 {
		t.Fatalf("summary grew with fleet size: bytes=%d error=%v", len(encoded), err)
	}
	if len(rejections) != 1601 || rejections[1600] != "no_eligible_target" {
		t.Fatal("logging changed the detailed planning results")
	}
	encoded, err = json.Marshal(rejectionCounts(nil))
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("empty fleet summary: %s error=%v", encoded, err)
	}
}

type failingEpochSource struct{ err error }

func (f failingEpochSource) LoadImmutableMarketEpoch(context.Context) (ImmutableMarketEpoch, error) {
	return ImmutableMarketEpoch{}, f.err
}

// A failed planning cycle names its cause. The one secret an error can carry,
// the RPC URL with its API key, is reduced to the operation and cause.
func TestPlanningCycleFailureLogsItsCauseWithoutTheRPCURL(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	cause := &url.Error{Op: "Post", URL: "https://rpc.example/?api-key=SECRET", Err: errors.New("dial tcp: connection refused")}
	w := &Worker{config: Config{Mode: ModeShadow}, marketEvidence: failingEpochSource{fmt.Errorf("load market epoch: %w", cause)}, facts: engine.NewFacts(prometheus.NewRegistry())}
	w.runtimeCycle(context.Background())
	out := buf.String()
	if !strings.Contains(out, "kamino_fleet_planner_cycle_failed") || !strings.Contains(out, "dial tcp: connection refused") {
		t.Fatalf("planning failure logged without its cause: %s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("planning failure log leaked the RPC credential: %s", out)
	}
}
