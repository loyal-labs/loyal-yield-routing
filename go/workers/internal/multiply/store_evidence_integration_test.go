package multiply

import (
	"context"
	"testing"
	"time"
)

func TestPrestatePublicationIsAtomicFencedAndImmutable(t *testing.T) {
	store := integrationStore(t)
	state, topology := runtimeFixtureRoute(t, store)
	ctx := context.Background()
	lease, err := store.LeaseRoute(ctx, state.RouteKey, "prestate-owner", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("lease: %v", err)
	}
	op := integrationOperation(state.RouteKey, state.Cycle, lease.Version+1)
	balance := TokenBalance{Account: topology.ClaimCustody.String(), Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: 1000}
	op.ExpectedEffects = ExpectedEffects{TokenAmountsBefore: []TokenAmountBefore{{Account: balance.Account, Mint: balance.Mint, AmountRaw: 1000}}, TokenDeltas: []TokenDelta{{Account: balance.Account, Mint: balance.Mint, RawDelta: -100}}}
	before := &ObservedRoute{Slot: 500, Claim: balance}
	pre, err := NewOperationPrestate(op, before, topology)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation++
	state.CurrentOperationID = &op.OperationID
	if ok, err := store.PrepareOperation(ctx, lease, state, op); err != nil || !ok {
		t.Fatalf("prepare: %v %v", ok, err)
	}
	var original string
	if err := store.Pool().QueryRow(ctx, "SELECT expected_effects::text FROM loyal_yield.multiply_operations WHERE operation_id=$1", op.OperationID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	signed := signedWireFixture(t, false)
	mh, err := MessageSHA256(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	stale := *lease
	stale.FencingToken++
	if ok, err := store.PersistSignedOperationWithPrestate(ctx, &stale, op.OperationID, fixtureKey(34).String(), PolicyDataHash([]byte("policy")), mh, signed, pre); err != nil || ok {
		t.Fatalf("stale publish: %v %v", ok, err)
	}
	var count int
	if err := store.Pool().QueryRow(ctx, "SELECT count(*) FROM loyal_yield.multiply_operation_evidence WHERE operation_id=$1", op.OperationID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial prestate published: %d %v", count, err)
	}
	if ok, err := store.PersistSignedOperationWithPrestate(ctx, lease, op.OperationID, fixtureKey(34).String(), PolicyDataHash([]byte("policy")), mh, signed, pre); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	saved, err := store.LoadRouteState(ctx, state.RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := store.LoadOperationPrestate(ctx, saved.Operation)
	if err != nil || evidence == nil || evidence.ObservedSlot != before.Slot || evidence.Signature != signed.TransactionSignature {
		t.Fatalf("original evidence unavailable: %v %v", evidence, err)
	}
	if _, err := store.Pool().Exec(ctx, "UPDATE loyal_yield.multiply_operation_evidence SET observed_slot=501 WHERE operation_id=$1", op.OperationID); err == nil {
		t.Fatal("registered schema allowed original prestate rewrite")
	}
	var after string
	if err := store.Pool().QueryRow(ctx, "SELECT expected_effects::text FROM loyal_yield.multiply_operations WHERE operation_id=$1", op.OperationID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if original != after {
		t.Fatal("signed publication rewrote Rust financial JSON")
	}
	next := saved.State
	next.Generation++
	next.CurrentOperationID = nil
	if ok, err := store.ExpireOperationWithProof(ctx, lease, op.OperationID, next, nil); err == nil || ok {
		t.Fatalf("expiry without proof: %v %v", ok, err)
	}
	retained, err := store.LoadRouteState(ctx, state.RouteKey)
	if err != nil || retained.Operation == nil || retained.Operation.Status != StatusSignedPersisted || retained.State.CurrentOperationID == nil {
		t.Fatalf("unproved intent lost ownership: %v", err)
	}
	manual := retained.State
	manual.Generation++
	manual.CurrentOperationID = nil
	manual.Goal = GoalManualRecovery
	reason := "offline fixture retained for immutable evidence audit"
	manual.ManualRecoveryReason = &reason
	if ok, err := store.MarkManualRecovery(ctx, lease, op.OperationID, manual); err != nil || !ok {
		t.Fatalf("fixture manual hold: %v %v", ok, err)
	}

}
