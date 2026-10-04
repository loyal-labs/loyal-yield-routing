package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func assertRetailReady(t *testing.T, r *retailReadiness, want bool) {
	t.Helper()
	response := httptest.NewRecorder()
	r.health.Handler(time.Minute).ServeHTTP(response, httptest.NewRequest("GET", "/readyz", nil))
	status := http.StatusServiceUnavailable
	if want {
		status = http.StatusOK
	}
	if response.Code != status {
		t.Fatalf("readiness status %d, want %d: %s", response.Code, status, response.Body.String())
	}
}

func TestRetailRequiresEveryFamilyFreshAndCannotMaskStall(t *testing.T) {
	r := newRetailReadiness(retailHealth())
	now := time.Now()
	assertRetailReady(t, r, false)
	for family := range r.families {
		r.report(family, true, 100, now)
	}
	assertRetailReady(t, r, true)
	// Only the executor stops. Every other loop continues at increasing slots.
	late := now.Add(31 * time.Second)
	for family := range r.families {
		if family != "fleet-executor" {
			r.report(family, true, 101, late)
		}
	}
	assertRetailReady(t, r, false)
	r.report("fleet-executor", true, 101, late)
	assertRetailReady(t, r, true)
	r.report("multiply", false, 101, late)
	assertRetailReady(t, r, false)
	// A healthy-looking callback with no real chain slot does not open a gate.
	r.report("multiply", true, 0, late)
	assertRetailReady(t, r, false)
	r.report("multiply", true, 101, late)
	assertRetailReady(t, r, true)
	// A regressed bank cannot freshen readiness with older evidence.
	r.report("multiply", true, 99, late)
	assertRetailReady(t, r, false)
}

func TestRetailHealthLaneCancellationClosesAllGates(t *testing.T) {
	r := newRetailReadiness(retailHealth())
	now := time.Now()
	for family := range r.families {
		r.report(family, true, 100, now)
	}
	assertRetailReady(t, r, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("health lane failed to join cancellation: %v", err)
	}
	assertRetailReady(t, r, false)
	// A family can finish a cycle concurrently with joined shutdown. Its last
	// healthy callback must never reopen the stopped runtime's public gate.
	for family := range r.families {
		r.report(family, true, 101, time.Now())
	}
	r.refresh(time.Now())
	assertRetailReady(t, r, false)
}

func TestRetailShutdownJoinsConcurrentReadinessCallbacks(t *testing.T) {
	r := newRetailReadiness(retailHealth(), "cross-mint", "lookup-planner", "lookup-writer")
	var families []string
	for family := range r.families {
		families = append(families, family)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	var reporters sync.WaitGroup
	reporters.Add(len(families))
	for _, family := range families {
		go func() {
			defer reporters.Done()
			for slot := uint64(1); slot <= 1000; slot++ {
				r.report(family, true, slot, time.Now())
			}
		}()
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("health shutdown failed: %v", err)
	}
	reporters.Wait()
	assertRetailReady(t, r, false)
}

func TestRetailCrossMintRecoveryStallCannotBeMaskedByOtherFamilies(t *testing.T) {
	r := newRetailReadiness(retailHealth(), "cross-mint")
	now := time.Now()
	for family := range r.families {
		r.report(family, true, 100, now)
	}
	assertRetailReady(t, r, true)
	late := now.Add(31 * time.Second)
	for family := range r.families {
		if family != "cross-mint" {
			r.report(family, true, 101, late)
		}
	}
	assertRetailReady(t, r, false)
	r.report("cross-mint", true, 101, late)
	assertRetailReady(t, r, true)
}

func TestRetailLookupPlanningAndRecoveryHaveIndependentReadiness(t *testing.T) {
	for _, stalled := range []string{"lookup-planner", "lookup-writer"} {
		t.Run(stalled, func(t *testing.T) {
			r := newRetailReadiness(retailHealth(), "lookup-planner", "lookup-writer")
			now := time.Now()
			for family := range r.families {
				r.report(family, true, 100, now)
			}
			assertRetailReady(t, r, true)
			late := now.Add(31 * time.Second)
			for family := range r.families {
				if family != stalled {
					r.report(family, true, 101, late)
				}
			}
			assertRetailReady(t, r, false)
			// An idle loop without a verified bank cannot conceal the stall.
			r.report(stalled, true, 0, late)
			assertRetailReady(t, r, false)
			r.report(stalled, true, 101, late)
			assertRetailReady(t, r, true)
		})
	}
}
