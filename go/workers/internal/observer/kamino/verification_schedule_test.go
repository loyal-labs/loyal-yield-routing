package kamino

import (
	"slices"
	"testing"
)

// Ports of the Rust verification_schedule.rs tests: a confirmed read taken
// before a newer stream write must never be admitted as that write's proof.
func TestRepeatedStreamWritesCoalesceIntoOneBatchEntry(t *testing.T) {
	schedule := newVerificationSchedule()
	schedule.markDirty("a")
	schedule.markDirty("a")
	schedule.markDirty("a")
	batch, ok := schedule.begin()
	if !ok || !slices.Equal(batch.reserves(), []string{"a"}) {
		t.Fatalf("batch = %v %v, want [a]", batch.reserves(), ok)
	}
	if _, again := schedule.begin(); again {
		t.Fatal("a second batch started while one was in flight")
	}
}

func TestWriteDuringInFlightReadDiscardsStaleProofAndStaysPending(t *testing.T) {
	schedule := newVerificationSchedule()
	schedule.markDirty("a")
	stale, _ := schedule.begin()
	schedule.markDirty("a")
	if accepted := schedule.completeSuccess(stale); len(accepted) != 0 {
		t.Fatalf("stale read accepted for %v", accepted)
	}
	next, ok := schedule.begin()
	if !ok || !slices.Equal(next.reserves(), []string{"a"}) {
		t.Fatal("newer stream write was not re-verified")
	}
	if accepted := schedule.completeSuccess(next); len(accepted) != 1 {
		t.Fatalf("current read rejected: %v", accepted)
	}
	if _, again := schedule.begin(); again {
		t.Fatal("verified reserve stayed pending")
	}
}

func TestFailedReadAndSafetySweepRequeueReserves(t *testing.T) {
	schedule := newVerificationSchedule()
	schedule.markDirty("a")
	batch, _ := schedule.begin()
	schedule.completeFailure(batch)
	schedule.requestSafetySweep([]string{"a", "b"})
	retry, ok := schedule.begin()
	if !ok || !slices.Equal(retry.reserves(), []string{"a", "b"}) {
		t.Fatalf("retry batch = %v, want failed and swept reserves", retry.reserves())
	}
	if accepted := schedule.completeSuccess(retry); len(accepted) != 2 {
		t.Fatalf("sweep read rejected: %v", accepted)
	}
}
