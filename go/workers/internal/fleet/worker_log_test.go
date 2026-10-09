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
	cause := &url.Error{Op: "Post", URL: "https://rpc.example/?api-key=SECRET", Err: errors.New("dial tcp: connection refused")}
	out := LogErrorText(fmt.Errorf("load market epoch: %w", cause))
	if !strings.Contains(out, "dial tcp: connection refused") {
		t.Fatalf("planning failure logged without its cause: %s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("planning failure log leaked the RPC credential: %s", out)
	}
}

// plannerLaneSucceededAt reads the planner lane's success clock; zero means
// the lane has never recorded a successful cycle.
func plannerLaneSucceededAt(t *testing.T, registry *prometheus.Registry) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "loyal_lane_last_success_timestamp_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["family"] == string(engine.FamilyFleet) && labels["lane"] == "planner" {
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}

// The executor marks fleet progress every tick, so a planner failing every
// cycle (stale market evidence) is visible only as a planner lane that never
// succeeds; LoyalLaneStalled pages on it.
func TestFailedPlanningCycleDoesNotAdvanceThePlannerLane(t *testing.T) {
	log.SetOutput(&bytes.Buffer{})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	registry := prometheus.NewRegistry()
	w := &Worker{config: Config{}, marketEvidence: failingEpochSource{errors.New("frontier is incomplete")}, facts: engine.NewFacts(registry)}
	w.runtimeCycle(context.Background())
	if at := plannerLaneSucceededAt(t, registry); at != 0 {
		t.Fatalf("failed planning cycle recorded planner lane success at %v", at)
	}
}
