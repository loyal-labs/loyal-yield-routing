package fleet

import (
	"testing"
)

func TestCrossMintFirstSendSDKSeparatesExternalVectorsFromManagedOwnership(t *testing.T) {
	input, binding := crossMintFirstSendWireFixture(t)
	managed := input.SelectedALTs[0]
	hash, err := CrossMintExternalAddressHash(managed.Addresses)
	if err != nil {
		t.Fatal(err)
	}
	external := CrossMintExternalALT{Address: managed.Address, Addresses: append([]string{}, managed.Addresses...), OrderedAddressHash: hash, ObservedSlot: 1000, UsableAfterSlot: 999}
	input.ExternalALTs = []CrossMintExternalALT{external}
	if _, _, _, tables, err := decodeCrossMintFirstSendWire(input, binding.DelegatedSigner); err != nil || len(tables) != 1 || tables[0].ID != managed.TableID {
		t.Fatalf("identical provider overlap displaced authentic ownership: %v %v", tables, err)
	}
	// This proves SDK decoding only. Retained SQL currently holds an
	// external-only policy route; a successful decode grants no admission.
	input.SelectedALTs = nil
	if _, _, _, tables, err := decodeCrossMintFirstSendWire(input, binding.DelegatedSigner); err != nil || len(tables) != 1 || tables[0].ID != 0 || tables[0].FamilyID != 0 {
		t.Fatalf("external decoding fabricated managed authority: %v %v", tables, err)
	}
	unused := external
	unused.Address = testPubkey(243)
	input.ExternalALTs = append(input.ExternalALTs, unused)
	if _, _, _, tables, err := decodeCrossMintFirstSendWire(input, binding.DelegatedSigner); err != nil || len(tables) != 1 {
		t.Fatalf("unused provider snapshot invented a wire lookup: %v %v", tables, err)
	}
	for _, change := range []string{"forged hash", "cold snapshot", "wrong table", "missing snapshot", "conflicting managed copy"} {
		t.Run(change, func(t *testing.T) {
			bad := input
			bad.ExternalALTs = cloneCrossMintExternalALTs(input.ExternalALTs)
			switch change {
			case "forged hash":
				bad.ExternalALTs[0].OrderedAddressHash = "forged"
			case "cold snapshot":
				bad.ExternalALTs[0].UsableAfterSlot = 1001
			case "wrong table":
				bad.ExternalALTs[0].Address = testPubkey(242)
			case "missing snapshot":
				bad.ExternalALTs = nil
			case "conflicting managed copy":
				bad.SelectedALTs = []ExecutionALT{managed}
				bad.ExternalALTs[0].Addresses[0] = testPubkey(242)
				bad.ExternalALTs[0].OrderedAddressHash, _ = CrossMintExternalAddressHash(bad.ExternalALTs[0].Addresses)
			}
			if _, _, _, _, err := decodeCrossMintFirstSendWire(bad, binding.DelegatedSigner); err == nil {
				t.Fatal("changed external proof decoded")
			}
		})
	}
}
