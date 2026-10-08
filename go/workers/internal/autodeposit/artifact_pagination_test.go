package autodeposit

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type pagedArtifactFake struct {
	artifactHistoryFake
	pages  map[string][]ArtifactHistoryEntry
	before []string
	err    error
}

func (h *pagedArtifactFake) ArtifactHistoryPage(_ context.Context, _ string, limit int, before string) ([]ArtifactHistoryEntry, error) {
	if limit != 32 {
		return nil, errors.New("unbounded page")
	}
	h.before = append(h.before, before)
	if h.err != nil {
		return nil, h.err
	}
	return h.pages[before], nil
}

func TestArtifactCreatorHistoryProgressesBeyondPassBound(t *testing.T) {
	f, target, b := artifactFixture(t)
	installArtifactSnapshot(t, f, b)
	receipt := goldenCreatorReceipt(t, f)
	h := &pagedArtifactFake{pages: map[string][]ArtifactHistoryEntry{}}
	h.receipts = map[string]ArtifactReceipt{receipt.Signature: receipt}
	before := ""
	for page := 0; page < 5; page++ {
		entries := make([]ArtifactHistoryEntry, 32)
		for index := range entries {
			entries[index] = ArtifactHistoryEntry{Signature: fmt.Sprintf("touch-%d-%d", page, index), Slot: int64(1000 - page*32 - index), Failed: true}
		}
		h.pages[before] = entries
		before = entries[31].Signature
	}
	h.pages[before] = []ArtifactHistoryEntry{{Signature: receipt.Signature, Slot: receipt.Slot}}
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
	if h.before[4] != "touch-3-31" || h.calls != 1 {
		t.Fatalf("did not continue exact proof: %v receipt calls=%d", h.before, h.calls)
	}
	// Success clears the read checkpoint; a later generation starts at head.
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); !errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatal(err)
	}
	if h.before[6] != "" {
		t.Fatalf("successful checkpoint retained: %v", h.before)
	}
	target.SetupGeneration++
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); !errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatal(err)
	}
	if h.before[10] != "" {
		t.Fatalf("new setup inherited old cursor: %v", h.before)
	}
}

func TestArtifactPagedHistoryRejectsTouchAndBrokenCursor(t *testing.T) {
	f, target, b := artifactFixture(t)
	installArtifactSnapshot(t, f, b)
	receipt := goldenCreatorReceipt(t, f)
	touch := receipt
	touch.PreLamports = append([]uint64(nil), receipt.PostLamports...)
	h := &pagedArtifactFake{pages: map[string][]ArtifactHistoryEntry{"": {{Signature: receipt.Signature, Slot: receipt.Slot}}}}
	h.receipts = map[string]ArtifactReceipt{receipt.Signature: touch}
	r := &ArtifactProofReader{Wires: b, History: h}
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); !errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatalf("touch became creator: %v", err)
	}
	entries := make([]ArtifactHistoryEntry, 32)
	for i := range entries {
		entries[i] = ArtifactHistoryEntry{Signature: fmt.Sprintf("failed-%d", i), Slot: 200, Failed: true}
	}
	h.pages[""] = entries
	h.pages["failed-31"] = entries
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); err == nil || errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatalf("nonadvancing history accepted: %v", err)
	}
}

func TestArtifactKnownSignatureStillRequiresExactCreator(t *testing.T) {
	f, target, b := artifactFixture(t)
	installArtifactSnapshot(t, f, b)
	receipt := goldenCreatorReceipt(t, f)
	target.PolicySignature = &receipt.Signature
	h := &artifactHistoryFake{receipts: map[string]ArtifactReceipt{receipt.Signature: receipt}}
	r := &ArtifactProofReader{Wires: b, History: h}
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); err != nil {
		t.Fatal(err)
	}
	if h.address != "" || h.calls != 1 {
		t.Fatal("known exact creator did not avoid history scan")
	}
	receipt.PreLamports = append([]uint64(nil), receipt.PostLamports...)
	h.receipts[receipt.Signature] = receipt
	if _, err := r.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); !errors.Is(err, ErrArtifactCreationProofPending) {
		t.Fatalf("known touch became creator: %v", err)
	}
}
