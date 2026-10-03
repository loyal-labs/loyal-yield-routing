package fleet

import (
	"testing"
	"time"
)

func TestSnapshotOwnerRejectsMixedOutOfOrderAndConflictingEvidence(t *testing.T) {
	source := ReserveIdentity{Address: "source", Market: "market", Mint: USDCMint}
	target := ReserveIdentity{Address: "target", Market: "market", Mint: USDCMint}
	owner, err := NewSnapshotOwner(source, target)
	if err != nil {
		t.Fatal(err)
	}
	states := []ReserveState{{ReserveIdentity: source, Slot: 100, DataHash: "a"}, {ReserveIdentity: target, Slot: 100, DataHash: "b"}}
	first, changed, err := owner.Apply(100, time.Now(), states)
	if err != nil || !changed {
		t.Fatalf("first apply: changed=%v err=%v", changed, err)
	}
	if _, changed, err := owner.Apply(100, time.Now(), states); err != nil || changed {
		t.Fatalf("duplicate: changed=%v err=%v", changed, err)
	}
	conflict := append([]ReserveState(nil), states...)
	conflict[1].DataHash = "different"
	if current, _, err := owner.Apply(100, time.Now(), conflict); err == nil || current.Hash != first.Hash {
		t.Fatalf("conflicting same slot was accepted")
	}
	if _, _, err := owner.Apply(99, time.Now(), []ReserveState{{ReserveIdentity: source, Slot: 99, DataHash: "a"}, {ReserveIdentity: target, Slot: 99, DataHash: "b"}}); err == nil {
		t.Fatal("out-of-order slot was accepted")
	}
	mixed := append([]ReserveState(nil), states...)
	mixed[1].Slot = 101
	if _, _, err := owner.Apply(101, time.Now(), mixed); err == nil {
		t.Fatal("mixed-slot batch was accepted")
	}
}
