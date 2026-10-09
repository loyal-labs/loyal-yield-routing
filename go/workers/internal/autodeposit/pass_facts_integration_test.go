package autodeposit

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
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

// Rust's overdue check counted an idle-blocked slot only while the wallet was
// above its floor, after three deferrals, with no claim owning the target. The
// port dropped those, so September markers on lots the wallet no longer backs
// paged as 26 days overdue.
func TestPassFactsCountIdleBlockedWorkOnlyWhileTheWalletOwesIt(t *testing.T) {
	store := releaseContextStore(t)
	ctx := t.Context()
	seeded := seedIntegrationTarget(t, store, "idle-overdue")
	seedProjectedSurplus(t, store, seeded, 9_700_001, 10_000_000)
	var slotID int64
	if err := store.pool.QueryRow(ctx, `SELECT scheduled_slot_id FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id=$1`, seeded.TargetID).Scan(&slotID); err != nil {
		t.Fatal(err)
	}
	blockedSince := time.Now().Add(-3 * time.Hour).Unix()
	registry := prometheus.NewRegistry()
	worker, err := NewWorker(WorkerDependencies{Store: store, Executor: &scriptedExecutor{}, Facts: engine.NewFacts(registry), FeePayer: fundedPayer{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		deferrals      int
		walletRaw      int64
		wantThreeHours bool
	}{
		{"owed", 3, 10_000_000, true},
		{"wallet-at-floor", 3, 4_000_000, false},
		{"two-deferrals", 2, 10_000_000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := fmt.Sprintf("%s25000001 [idle_blocked_since=%d; idle_deferrals=%d]", idleDeferralPrefix, blockedSince, tc.deferrals)
			if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_scheduled_slots SET last_error=$2 WHERE id=$1`, slotID, marker); err != nil {
				t.Fatal(err)
			}
			if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_wallet_balances_current SET amount_raw=$2 WHERE target_id=$1`, seeded.TargetID, tc.walletRaw); err != nil {
				t.Fatal(err)
			}
			if err := worker.passFacts(ctx); err != nil {
				t.Fatal(err)
			}
			age := metricValue(t, registry, "loyal_autodeposit_oldest_due_lot_age_seconds", nil)
			if counted := age > 10790 && age < 10900; counted != tc.wantThreeHours {
				t.Fatalf("oldest due age %v, want counted=%v", age, tc.wantThreeHours)
			}
		})
	}
}
