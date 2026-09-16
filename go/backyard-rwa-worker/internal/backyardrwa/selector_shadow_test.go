package backyardrwa

import (
	"context"
	"testing"
	"time"
)

func TestSlowShadowDoesNotBlockLifecycleTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		runSelectorSamples(ctx, time.Minute, func(sampleCtx context.Context) {
			close(started)
			<-sampleCtx.Done()
		})
	}()
	<-started
	observation := tickObservation(base())
	recorded := false
	worker := &Worker{routeKey: productionRouteKey, manifest: readyWorkerManifest(t), runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return observation, nil },
		recordDecision: func(_ context.Context, _ string, _ Observation, d Decision, _, _ string) (DecisionRecord, error) {
			recorded = true
			if d.Action != Hold {
				t.Fatal(d)
			}
			return DecisionRecord{Status: Held}, nil
		},
	}}
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !recorded || worker.manifest.selectorObservation {
		t.Fatal("shadow changed execution")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shadow shutdown did not cancel sample")
	}
}
