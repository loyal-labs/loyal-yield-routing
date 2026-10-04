package main

import (
	"context"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/observability"
)

type retailFamilyHealth struct {
	slot        uint64
	lastSuccess time.Time
	maxAge      time.Duration
}

// Retail has no capture stream. Readiness requires a completed, healthy cycle
// from every family. Each family's freshness is independent: a busy planner
// cannot conceal a stopped executor or control reconciler.
type retailReadiness struct {
	mu       sync.Mutex
	health   *observability.Health
	families map[string]retailFamilyHealth
	frontier uint64
}

func newRetailReadiness(health *observability.Health) *retailReadiness {
	r := &retailReadiness{health: health, families: map[string]retailFamilyHealth{}}
	for _, family := range []string{"autodeposit-control", "autodeposit", "fleet-planner", "fleet-executor", "multiply"} {
		age := 30 * time.Second
		if family == "autodeposit" {
			age = 2 * time.Minute // retained one-minute dispatch cadence
		}
		r.families[family] = retailFamilyHealth{maxAge: age}
		health.SetDomainReady(family, false)
	}
	health.SetReady(false)
	health.SetConnected(false)
	return r
}

func (r *retailReadiness) reporter(family string) func(bool, uint64) {
	return func(ready bool, slot uint64) { r.report(family, ready, slot, time.Now()) }
}

func (r *retailReadiness) report(family string, ready bool, slot uint64, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.families[family]
	if !ok {
		return
	}
	if ready && slot > 0 && slot >= f.slot {
		f.slot, f.lastSuccess = slot, now
		r.health.DomainProgress(family, slot)
		r.health.DomainAppliedProgress(family, slot)
	} else {
		f.lastSuccess = time.Time{}
	}
	r.families[family] = f
	r.refreshLocked(now)
}

func (r *retailReadiness) refresh(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(now)
}

func (r *retailReadiness) refreshLocked(now time.Time) {
	allReady := true
	for family, f := range r.families {
		ready := f.slot > 0 && !f.lastSuccess.IsZero() && !now.Before(f.lastSuccess) && now.Sub(f.lastSuccess) <= f.maxAge
		r.health.SetDomainReady(family, ready)
		allReady = allReady && ready
		if f.slot > r.frontier {
			r.frontier = f.slot
		}
	}
	r.health.SetConnected(allReady)
	if allReady {
		r.health.Progress(r.frontier)
	}
	r.health.SetReady(allReady)
}

// Run is a joined runtime lane, so timeouts are enforced even when a family
// stops reporting. It adds no goroutine or provider/database traffic.
func (r *retailReadiness) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		r.health.SetReady(false)
		r.health.SetConnected(false)
		for family := range r.families {
			r.health.SetDomainReady(family, false)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			r.refresh(now)
		}
	}
}
