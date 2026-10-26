package fleet

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
)

type failingCatalog struct{ err error }

func (f failingCatalog) LoadCatalog(context.Context) ([]ReserveIdentity, error) { return nil, f.err }

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
// cycle is visible only as a planner lane that never succeeds;
// LoyalLaneStalled pages on it.
func TestFailedPlanningCycleDoesNotAdvanceThePlannerLane(t *testing.T) {
	log.SetOutput(&bytes.Buffer{})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	registry := prometheus.NewRegistry()
	w := &Worker{config: Config{}, catalog: failingCatalog{errors.New("catalog unavailable")}, facts: engine.NewFacts(registry)}
	w.runtimeCycle(context.Background())
	if at := plannerLaneSucceededAt(t, registry); at != 0 {
		t.Fatalf("failed planning cycle recorded planner lane success at %v", at)
	}
}
