package worker

import (
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/watch"
)

func TestNewBindingStartUsesOnlyNewAccountBoundary(t *testing.T) {
	oldSlot, newSlot := uint64(100), uint64(900)
	previous := &watch.Set{Vaults: []watch.Vault{{Environment: "mainnet", Vault: "vault", ObservationStartSlot: &oldSlot, Accounts: []watch.Account{{Pubkey: "existing", Role: "policy"}}}}}
	next := &watch.Set{Vaults: []watch.Vault{{Environment: "mainnet", Vault: "vault", ObservationStartSlot: &newSlot, Accounts: []watch.Account{{Pubkey: "existing", Role: "policy"}, {Pubkey: "added", Role: "recurring_delegation"}}}}}
	got := newBindingStart(previous, next)
	if got == nil || *got != 900 {
		t.Fatalf("new binding start = %v, want 900", got)
	}
}

func TestNewBindingStartIgnoresRetainedOldHistory(t *testing.T) {
	oldSlot := uint64(100)
	set := &watch.Set{Vaults: []watch.Vault{{Environment: "mainnet", Vault: "vault", ObservationStartSlot: &oldSlot, Accounts: []watch.Account{{Pubkey: "existing", Role: "policy"}}}}}
	if got := newBindingStart(set, set); got != nil {
		t.Fatalf("unchanged watch set replayed from old boundary %d", *got)
	}
}

func TestColdStartReplayIncludesEarnObservationAnchor(t *testing.T) {
	observationStart := uint64(250)
	got, err := selectReplayStart(100_000, 99_000, 98_000, 97_000, 32, &observationStart)
	if err != nil {
		t.Fatal(err)
	}
	if got != observationStart {
		t.Fatalf("cold-start replay = %d, want observation anchor %d", got, observationStart)
	}
}

func TestSubtractNeverRequestsGenesis(t *testing.T) {
	if got := subtract(10, 32); got != 1 {
		t.Fatalf("saturated replay start = %d, want 1", got)
	}
}
