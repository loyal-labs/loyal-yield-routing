package fleet

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
)

type failedHealthEpoch struct{}

func (failedHealthEpoch) LoadImmutableMarketEpoch(context.Context) (ImmutableMarketEpoch, error) {
	return ImmutableMarketEpoch{}, errors.New("https://rpc.invalid/test-secret")
}

func TestRuntimeCycleSanitizesErrors(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	w := &Worker{config: Config{Mode: ModeShadow}, marketEvidence: failedHealthEpoch{}}
	w.runtimeCycle(context.Background())
	if !strings.Contains(output.String(), "kamino_fleet_planner_cycle_failed") || strings.Contains(output.String(), "test-secret") || strings.Contains(output.String(), "rpc.invalid") {
		t.Fatalf("unsafe/missing cycle report %q", output.String())
	}
}
