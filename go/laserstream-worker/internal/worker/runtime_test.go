package worker

import (
	"errors"
	"slices"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/watch"
)

func TestColdStartReplayIncludesEarnObservationAnchor(t *testing.T) {
	observationStart := uint64(250)
	got, err := selectReplayStart(100_000, 99_000, 98_000, 97_000, 96_000, 32, &observationStart)
	if err != nil {
		t.Fatal(err)
	}
	if got != observationStart {
		t.Fatalf("cold-start replay = %d, want observation anchor %d", got, observationStart)
	}
}

func TestColdStartReplayIncludesDurableWatchObservation(t *testing.T) {
	got, err := selectReplayStart(100_000, 99_000, 98_000, 97_000, 500, 32, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 468 {
		t.Fatalf("cold-start replay = %d, want watch observation overlap 468", got)
	}
}

func TestFirstDeploymentUsesBoundedDiscoveryReplay(t *testing.T) {
	got, err := selectReplayStart(100_000, 99_000, 98_000, 97_000, 0, 32, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 90_000 {
		t.Fatalf("first-deployment replay = %d, want bounded discovery floor 90000", got)
	}
}

func TestNewEarnBindingRecoveriesCoverEveryAddedAccount(t *testing.T) {
	previous := &watch.Set{Vaults: []watch.Vault{{Environment: "mainnet-beta", Vault: "vault", Accounts: []watch.Account{{Pubkey: "existing", Role: "policy"}}}}}
	next := &watch.Set{Vaults: []watch.Vault{{Environment: "mainnet-beta", Vault: "vault", Accounts: []watch.Account{{Pubkey: "existing", Role: "policy"}, {Pubkey: "added", Role: "policy"}, {Pubkey: "added", Role: "subscription_authority"}}}}}
	recoveries := newEarnBindingRecoveries(previous, next)
	if len(recoveries) != 1 || recoveries[0].address != "added" {
		t.Fatalf("recoveries = %#v, want one added address", recoveries)
	}
	wantFilters := []string{watch.EarnPolicyAccounts, watch.EarnSubscriptionAuthorities}
	if !slices.Equal(recoveries[0].filters, wantFilters) {
		t.Fatalf("filters = %v, want %v", recoveries[0].filters, wantFilters)
	}
}

func TestPersistentVerificationErrorStartsAtThreshold(t *testing.T) {
	cause := errors.New("rpc unavailable")
	if err := persistentVerificationError(kaminoVerificationFailureThreshold-1, cause); err != nil {
		t.Fatalf("transient failure became terminal: %v", err)
	}
	if err := persistentVerificationError(kaminoVerificationFailureThreshold, cause); err == nil || !errors.Is(err, cause) {
		t.Fatalf("threshold failure = %v, want wrapped cause", err)
	}
}

func TestSubtractNeverRequestsGenesis(t *testing.T) {
	if got := subtract(10, 32); got != 1 {
		t.Fatalf("saturated replay start = %d, want 1", got)
	}
}
