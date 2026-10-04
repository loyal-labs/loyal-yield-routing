package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
}
