package autodeposit

import (
	"context"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// setupReplayChain drops forwards until send number landOn.
type setupReplayChain struct {
	*scriptedControllerChain
	landOn     int
	broadcasts int
}

func (s *setupReplayChain) LatestBlockhash(context.Context) (string, int64, error) {
	return fixedKey("setup-controller-blockhash"), 900, nil
}
func (s *setupReplayChain) SignatureState(context.Context, string) (chain.SignatureState, error) {
	if s.broadcasts >= s.landOn {
		return chain.SignatureState{Found: true, Slot: 500, Commitment: chain.Confirmed, ContextSlot: 500}, nil
	}
	return chain.SignatureState{ContextSlot: 1}, nil
}
func (s *setupReplayChain) SendWire(context.Context, []byte, bool) error {
	s.broadcasts++
	return nil
}

func TestControllerSetupLandsStageAndReadsItBackBeforePull(t *testing.T) {
	// Setup used to be journaled in branch-only 0087 and sent once per tick;
	// it now lands in-process like the TS ensure-before-pull path, with the
	// chain account as the fact.
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
	// The first forward is dropped; the same bytes land on the second send.
	chain := &setupReplayChain{scriptedControllerChain: &scriptedControllerChain{}, landOn: 2}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: builder, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	scope := executionScope{ctx: ctx, leaseToken: "lease-current"}
	// Confirmation alone cannot release the setup until the decoded account is
	// visible at the confirmed slot.
	ready, err := controller.ensureDestinationSetup(scope, claim, plan)
	if err == nil || ready || chain.broadcasts != 2 {
		t.Fatalf("missing readback ready=%v broadcasts=%d err=%v", ready, chain.broadcasts, err)
	}
	data := make([]byte, 165)
	mint, vault := mustKey(USDCMint), mustKey(plan.Target.VaultPubkey)
	copy(data[:32], mint[:])
	copy(data[32:64], vault[:])
	data[108] = 1
	accounts[setup.Account] = backyard.ConfirmedAccount{Address: setup.Account, Owner: splTokenID, Data: data}
	ready, err = controller.ensureDestinationSetup(scope, claim, plan)
	if err != nil || !ready || chain.broadcasts != 2 {
		t.Fatalf("created account ready=%v broadcasts=%d err=%v", ready, chain.broadcasts, err)
	}
	var pullCount int
	if err = store.pool.QueryRow(ctx, `SELECT COUNT(*)FROM loyal_yield.balance_sweep_transaction_attempts WHERE claim_token=$1`, claim).Scan(&pullCount); err != nil || pullCount != 0 {
		t.Fatalf("financial attempts before setup completion=%d err=%v", pullCount, err)
	}
}
