package autodeposit

import (
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
)

// The Rust trigger emitted one operational error per pass however many rows
// timed out; the port copied that, so ten timed-out slots counted as one
// failure. Each row is now one fact. A selected claim's age is the Rust
// overdue clock for owned work and feeds the oldest-due gauge.
func TestPassFactsCountRowsAndAgeOwnedWork(t *testing.T) {
	store := releaseContextStore(t)
	ctx := t.Context()
	_, claim, _ := selectedReleaseClaim(t, store, "owned-age")
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_lot_claims SET created_at = now() - interval '2 hours' WHERE claim_token=$1`, claim); err != nil {
		t.Fatal(err)
	}
	stale := seedIntegrationTarget(t, store, "stale-requests")
	for range 2 {
		if _, err := store.pool.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_scheduled_slots (target_id, token_mint, eligible_after, status, requested_at) VALUES ($1, $2, now(), 'requested', now() - interval '1 hour')`, stale.TargetID, USDCMint); err != nil {
			t.Fatal(err)
		}
	}
	registry := prometheus.NewRegistry()
	worker, err := NewWorker(WorkerDependencies{Store: store, Executor: &scriptedExecutor{}, Facts: engine.NewFacts(registry), FeePayer: fundedPayer{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.passFacts(ctx); err != nil {
		t.Fatal(err)
	}
	if age := metricValue(t, registry, "loyal_autodeposit_oldest_due_lot_age_seconds", nil); age < 7190 || age > 7300 {
		t.Fatalf("oldest due age %v, want the selected claim's two hours", age)
	}
	if _, err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := failedCount(t, registry, "autodeposit_requested_slot_timed_out"); got != 2 {
		t.Fatalf("two timed-out slots counted %v times", got)
	}
}
