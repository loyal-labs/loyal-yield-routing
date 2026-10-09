package worker

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	solanago "github.com/solana-foundation/solana-go/v2"
)

// Production shape from the rehearsal: fresh durable cursors, while 1,985 of
// 1,992 active route policies were last seen before LaserStream's retention.
// The stream must start from the cursors, inside the provider window.
func TestReplayStartFollowsDurableCursorsInsideProviderWindow(t *testing.T) {
	const current = 449_073_607
	requested, err := selectReplayStart(current, current-50, current-400, current-300, current-200, 32)
	if err != nil {
		t.Fatal(err)
	}
	if requested != current-432 || clampToReplayWindow(requested, current) != requested {
		t.Fatalf("replay requested=%d, want continuity from oldest cursor %d inside the window", requested, current-432)
	}
}

// ASK-2252 shape: a cursor 1M slots behind. Rust clamps to the window edge
// (clamp_laserstream_replay_start); the plan reports the gap so every binding
// is recovered from confirmed account state.
func TestReplayStartOutsideProviderWindowIsClampedAndReported(t *testing.T) {
	const current = 449_073_607
	requested, err := selectReplayStart(current, current-50, current-1_051_842, current-300, current-200, 32)
	if err != nil {
		t.Fatal(err)
	}
	plan := streamPlan{requested: requested, from: clampToReplayWindow(requested, current)}
	if plan.from != current-laserStreamReplaySlots || plan.requested != current-1_051_874 || !plan.gap() {
		t.Fatalf("plan = %+v, want window edge %d with the unreplayable start reported", plan, current-laserStreamReplaySlots)
	}
}

func TestReplayStartFollowsDurableWatchObservation(t *testing.T) {
	requested, err := selectReplayStart(100_000, 99_000, 98_000, 97_000, 500, 32)
	if err != nil {
		t.Fatal(err)
	}
	if requested != 468 {
		t.Fatalf("replay = %d, want watch observation overlap 468", requested)
	}
}

func TestFirstDeploymentUsesBoundedDiscoveryReplay(t *testing.T) {
	requested, err := selectReplayStart(100_000, 99_000, 98_000, 97_000, 0, 32)
	if err != nil {
		t.Fatal(err)
	}
	if requested != 90_000 {
		t.Fatalf("first-deployment replay = %d, want bounded discovery floor 90000", requested)
	}
}

// A reconnect has no seed; an absent seed must not become slot 1.
func TestReconnectWithoutSeedFollowsDurableCursors(t *testing.T) {
	requested, err := selectReplayStart(100_000, 0, 98_000, 97_000, 96_000, 32)
	if err != nil {
		t.Fatal(err)
	}
	if requested != 95_968 {
		t.Fatalf("seedless replay = %d, want durable watch cursor overlap 95968", requested)
	}
}

// A handoff for a newly discovered binding whose policy was last seen before
// the window must still request a replayable slot.
func TestHandoffBindingAnchorIsClampedIntoProviderWindow(t *testing.T) {
	const current = 449_073_607
	if got := clampToReplayWindow(1_000, current); got != current-laserStreamReplaySlots {
		t.Fatalf("ancient binding anchor = %d, want window edge %d", got, current-laserStreamReplaySlots)
	}
	if got := clampToReplayWindow(current-10, current); got != current-10 {
		t.Fatalf("recent binding anchor moved to %d", got)
	}
}

// A restart reads the same confirmed state at a later slot; the event must be
// the same so the job key deduplicates it. A changed state is a new event.
func TestBindingRecoveryEventIsIdentifiedByStateNotReadSlot(t *testing.T) {
	address := "11111111111111111111111111111111"
	first, kind := bindingRecoveryEvent(address, &chain.Account{Lamports: 2_039_280, Owner: solanago.TokenProgramID, Data: []byte{1, 2, 3}})
	again, _ := bindingRecoveryEvent(address, &chain.Account{Lamports: 2_039_280, Owner: solanago.TokenProgramID, Data: []byte{1, 2, 3}})
	if kind != "account" || first != again {
		t.Fatalf("same confirmed state produced %q then %q", first, again)
	}
	changed, _ := bindingRecoveryEvent(address, &chain.Account{Lamports: 2_039_280, Owner: solanago.TokenProgramID, Data: []byte{1, 2, 4}})
	if changed == first {
		t.Fatal("changed account data reused the recovered event")
	}
	deleted, deletedKind := bindingRecoveryEvent(address, nil)
	if deletedKind != "account_deleted" || deleted == first {
		t.Fatalf("missing account recovered as %q/%q", deleted, deletedKind)
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
	budget := kaminoVerificationFailureThreshold * 30 * time.Second
	if err := persistentVerificationError(kaminoVerificationFailureThreshold-1, budget, budget, cause); err != nil {
		t.Fatalf("transient failure became terminal: %v", err)
	}
	// Dirty batches retry every 100 ms, so a burst of fast failures inside
	// the sweep budget is still a transient RPC outage.
	if err := persistentVerificationError(50, time.Second, budget, cause); err != nil {
		t.Fatalf("short failure burst became terminal: %v", err)
	}
	if err := persistentVerificationError(kaminoVerificationFailureThreshold, budget, budget, cause); err == nil || !errors.Is(err, cause) {
		t.Fatalf("threshold failure = %v, want wrapped cause", err)
	}
}

func TestSubtractNeverRequestsGenesis(t *testing.T) {
	if got := subtract(10, 32); got != 1 {
		t.Fatalf("saturated replay start = %d, want 1", got)
	}
}
