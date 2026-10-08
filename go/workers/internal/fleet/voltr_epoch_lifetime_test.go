package fleet

import (
	"context"
	"testing"
	"time"
)

type fixedMarketEpoch struct{ epoch ImmutableMarketEpoch }

func (f fixedMarketEpoch) LoadImmutableMarketEpoch(context.Context) (ImmutableMarketEpoch, error) {
	return f.epoch, nil
}

// On Oct 8 2026 the catalog stopped renewing and every mint went incomplete,
// so the envelope fell back to the capture time. Rust's Voltr cycle upserted
// that epoch and died on the optimizer epoch invariant. A stale or too-short
// envelope is a non-mutating deferral: no epoch row, no store access.
func TestVoltrCycleDefersAnEpochThatCannotOutlivePlanning(t *testing.T) {
	now := time.Now().UTC()
	slot := int64(454401152)
	short := now.Add(30 * time.Second)
	for name, epoch := range map[string]ImmutableMarketEpoch{
		"no complete mint": {CapturedAt: now, ExpiresAt: now, MaximumMarketSlot: &slot},
		"under a minute":   {CapturedAt: now, ExpiresAt: now, MaximumMarketSlot: &slot, MintCoverage: []MarketMintCoverage{{Mint: USDCMint, Complete: true, ExpiresAt: &short}}},
	} {
		// A nil store panics on any write, so reaching EnsureOptimizerEpoch fails the test.
		w := &Worker{marketEvidence: fixedMarketEpoch{epoch}, voltr: &VoltrRoute{}}
		status, err := w.voltrCycle(context.Background())
		if err != nil || status != "no_fresh_market_epoch" {
			t.Fatalf("%s: status=%q err=%v, want a non-mutating no_fresh_market_epoch", name, status, err)
		}
	}
}
