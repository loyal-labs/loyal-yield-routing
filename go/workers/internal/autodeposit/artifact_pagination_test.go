package autodeposit

import (
	"errors"
	"testing"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func TestArtifactCreatorHistoryProgressesBeyondPassBound(t *testing.T) {
	f, target, b := artifactFixture(t)
	installArtifactSnapshot(t, f, b)
	receipt := goldenCreatorReceipt(t, f)
	h := &artifactHistoryFake{pages: map[solana.Signature][]chain.Signed{}, receipts: map[solana.Signature]chain.Receipt{receipt.signature: receipt.Receipt}}
	var before solana.Signature
	for page := 0; page < 5; page++ {
		entries := make([]chain.Signed, 32)
		for index := range entries {
			entries[index] = chain.Signed{Signature: testSignature(page*32 + index), Slot: uint64(1000 - page*32 - index), Failed: true}
		}
		h.pages[before] = entries
		before = entries[31].Signature
	}
	h.pages[before] = []chain.Signed{{Signature: receipt.signature, Slot: receipt.Slot}}
	r := &ArtifactProofReader{Wires: b, History: h}
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); !errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatalf("first bounded pass: %v", err)
	}
	if len(h.before) != 4 {
		t.Fatalf("unbounded pass: %v", h.before)
	}
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); err != nil {
		t.Fatal(err)
	}
	if h.before[4] != testSignature(3*32+31) || h.calls != 1 {
		t.Fatalf("did not continue exact proof: %v receipt calls=%d", h.before, h.calls)
	}
	// Success clears the read checkpoint; a later generation starts at head.
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); !errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatal(err)
	}
	if !h.before[6].IsZero() {
		t.Fatalf("successful checkpoint retained: %v", h.before)
	}
	target.SetupGeneration++
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); !errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatal(err)
	}
	if !h.before[10].IsZero() {
		t.Fatalf("new setup inherited old cursor: %v", h.before)
	}
}

func TestArtifactPagedHistoryRejectsTouchAndBrokenCursor(t *testing.T) {
	f, target, b := artifactFixture(t)
	installArtifactSnapshot(t, f, b)
	receipt := goldenCreatorReceipt(t, f)
	touch := receipt.Receipt
	touch.PreLamports = append([]uint64(nil), receipt.PostLamports...)
	h := &artifactHistoryFake{pages: map[solana.Signature][]chain.Signed{{}: {{Signature: receipt.signature, Slot: receipt.Slot}}}, receipts: map[solana.Signature]chain.Receipt{receipt.signature: touch}}
	r := &ArtifactProofReader{Wires: b, History: h}
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); !errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatalf("touch became creator: %v", err)
	}
	entries := make([]chain.Signed, 32)
	for i := range entries {
		entries[i] = chain.Signed{Signature: testSignature(i), Slot: 200, Failed: true}
	}
	h.pages[solana.Signature{}] = entries
	h.pages[entries[31].Signature] = entries
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); err == nil || errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatalf("nonadvancing history accepted: %v", err)
	}
}

func TestArtifactKnownSignatureStillRequiresExactCreator(t *testing.T) {
	f, target, b := artifactFixture(t)
	installArtifactSnapshot(t, f, b)
	receipt := goldenCreatorReceipt(t, f)
	known := receipt.signature.String()
	target.PolicySignature = &known
	h := &artifactHistoryFake{receipts: map[solana.Signature]chain.Receipt{receipt.signature: receipt.Receipt}}
	r := &ArtifactProofReader{Wires: b, History: h}
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); err != nil {
		t.Fatal(err)
	}
	if len(h.before) != 0 || h.calls != 1 {
		t.Fatal("known exact creator did not avoid history scan")
	}
	failed := receipt.Receipt
	failed.Err = map[string]any{"InstructionError": []any{0, "Custom"}}
	h.receipts[receipt.signature] = failed
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); !errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatalf("failed transaction became creator: %v", err)
	}
	receipt.PreLamports = append([]uint64(nil), receipt.PostLamports...)
	h.receipts[receipt.signature] = receipt.Receipt
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); !errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatalf("known touch became creator: %v", err)
	}
}
