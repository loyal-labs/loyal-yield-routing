package autodeposit

import (
	"context"
	"errors"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"testing"
)

type setupReplayChain struct {
	*scriptedControllerChain
	store      *Store
	landed     bool
	broadcasts int
}

func (s *setupReplayChain) LatestBlockhash(context.Context) (string, int64, error) {
	return fixedKey("setup-controller-blockhash"), 900, nil
}
func (s *setupReplayChain) Observe(context.Context, DurableAttempt) (AttemptObservation, error) {
	if s.landed {
		return AttemptObservation{State: AttemptConfirmed, ConfirmedSlot: ptrInt64(500)}, nil
	}
	return AttemptObservation{State: AttemptUnknown}, nil
}
func (s *setupReplayChain) BroadcastExact(ctx context.Context, a DurableAttempt) (string, error) {
	var signature, wire string
	var count int
	if err := s.store.pool.QueryRow(ctx, `SELECT signature,signed_transaction_base64,broadcast_count FROM loyal_yield.balance_sweep_destination_setup_attempts WHERE id=$1`, a.ID).Scan(&signature, &wire, &count); err != nil {
		return "", err
	}
	if signature != a.Signature || wire != a.SignedTransactionBase64 || count != 1 {
		return "", errors.New("setup broadcast preceded exact durable bytes")
	}
	s.broadcasts++
	return a.Signature, nil
}

func TestControllerSetupCrashResumesOwnedWireBeforePull(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, claim, _ := selectedReleaseClaim(t, store, "setup-controller")
	builder, plan, setup, accounts := setupFixture(t, SetupATA)
	plan.Target.ID = seeded.TargetID
	plan.Target.ManagedVaultID = seeded.ManagedVaultID
	plan.AmountRaw = 5_000_000
	if _, err := store.FreezeDepositPlan(ctx, claim, "lease-current", plan); err != nil {
		t.Fatal(err)
	}
	chain := &setupReplayChain{scriptedControllerChain: &scriptedControllerChain{}, store: store}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: builder})
	if err != nil {
		t.Fatal(err)
	}
	scope := executionScope{ctx: ctx, leaseToken: "lease-current"}
	ready, err := controller.ensureDestinationSetup(scope, claim, plan)
	if err != nil || ready {
		t.Fatalf("first setup ready=%v err=%v", ready, err)
	}
	attempt, err := store.LoadDestinationSetup(ctx, claim, "lease-current")
	if err != nil || attempt == nil || attempt.State != AttemptUnknown || attempt.BroadcastCount != 1 {
		t.Fatalf("owned unknown setup=%+v err=%v", attempt, err)
	}
	owned := attempt.Wire
	// A restart adopts the same packet. Its chain confirmation alone cannot
	// release the setup until the decoded account is visible at that slot.
	chain.landed = true
	ready, err = controller.ensureDestinationSetup(scope, claim, plan)
	if err == nil || ready {
		t.Fatalf("missing readback ready=%v err=%v", ready, err)
	}
	data := make([]byte, 165)
	mint, vault := mustKey(USDCMint), mustKey(plan.Target.VaultPubkey)
	copy(data[:32], mint[:])
	copy(data[32:64], vault[:])
	data[108] = 1
	accounts[setup.Account] = backyard.ConfirmedAccount{Address: setup.Account, Owner: splTokenID, Data: data}
	ready, err = controller.ensureDestinationSetup(scope, claim, plan)
	if err != nil || !ready {
		t.Fatalf("proved setup ready=%v err=%v", ready, err)
	}
	attempt, err = store.LoadDestinationSetup(ctx, claim, "lease-current")
	if err != nil || attempt.Wire != owned || attempt.State != AttemptConfirmed || chain.broadcasts != 1 {
		t.Fatalf("adopted setup=%+v broadcasts=%d err=%v", attempt, chain.broadcasts, err)
	}
	var pullCount int
	if err = store.pool.QueryRow(ctx, `SELECT COUNT(*)FROM loyal_yield.balance_sweep_transaction_attempts WHERE claim_token=$1`, claim).Scan(&pullCount); err != nil || pullCount != 0 {
		t.Fatalf("financial attempts before setup completion=%d err=%v", pullCount, err)
	}
}

func TestDestinationSetupJournalKeepsExactIntentUntilProvedReadback(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, claim, _ := selectedReleaseClaim(t, store, "destination-setup")
	builder, plan, setup, accounts := setupFixture(t, SetupATA)
	plan.Target.ID = seeded.TargetID
	plan.Target.ManagedVaultID = seeded.ManagedVaultID
	plan.AmountRaw = 5_000_000
	if _, err := store.FreezeDepositPlan(ctx, claim, "lease-current", plan); err != nil {
		t.Fatal(err)
	}
	wire, err := builder.BuildDestinationSetup(ctx, plan, setup, fixedKey("setup-journal-blockhash"), 900)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := store.PersistDestinationSetup(ctx, claim, "lease-current", setup, wire)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.State != AttemptPrepared || attempt.BroadcastCount != 0 || attempt.Wire != wire {
		t.Fatalf("prepared setup %v", attempt)
	}
	// Neither Go release nor an old worker's direct claim transition can free
	// a claim whose setup still has valid signed bytes.
	if _, err = store.ReleaseClaimOnce(ctx, claim, "lease-current"); !errors.Is(err, ErrClaimCustodyHeld) {
		t.Fatalf("release with unresolved setup: %v", err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_lot_claims SET status='released' WHERE claim_token=$1`, claim); err == nil {
		t.Fatal("legacy release bypassed signed setup guard")
	}
	if _, err = store.RecordDestinationSetupBroadcast(ctx, attempt, "displaced"); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("displaced setup transition: %v", err)
	}
	attempt, err = store.RecordDestinationSetupBroadcast(ctx, attempt, "lease-current")
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Wire != wire || attempt.BroadcastCount != 1 {
		t.Fatal("broadcast intent changed owned wire")
	}
	slot := int64(500)
	observation := AttemptObservation{State: AttemptConfirmed, ConfirmedSlot: &slot}
	if _, err = store.RecordDestinationSetupObservation(ctx, attempt, observation, nil, "lease-current"); err == nil {
		t.Fatal("setup confirmed without decoded account evidence")
	}
	data := make([]byte, 165)
	mint := mustKey(USDCMint)
	owner := mustKey(plan.Target.VaultPubkey)
	copy(data[:32], mint[:])
	copy(data[32:64], owner[:])
	data[108] = 1
	accounts[setup.Account] = backyard.ConfirmedAccount{Address: setup.Account, Owner: splTokenID, Data: data}
	readback, err := builder.ReadbackDestinationSetup(ctx, plan, setup, slot)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = store.RecordDestinationSetupObservation(ctx, attempt, observation, &readback, "lease-current")
	if err != nil {
		t.Fatal(err)
	}
	if attempt.State != AttemptConfirmed || attempt.ReadbackEvidence == nil || attempt.ReadbackEvidence.DataSHA256 != readback.DataSHA256 || attempt.Wire != wire {
		t.Fatalf("confirmed setup %v", attempt)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_destination_setup_attempts SET signed_transaction_sha256=repeat('a',64) WHERE id=$1`, attempt.ID); err == nil {
		t.Fatal("setup journal rewrote immutable signed wire")
	}
	if _, err = store.ReleaseClaimOnce(ctx, claim, "lease-current"); err != nil {
		t.Fatalf("proved setup must not strand unspent wallet claim: %v", err)
	}
}
