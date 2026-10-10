package backyard

import (
	"context"
	"testing"
)

func TestPreBroadcastRecoveryTerminatesWithoutRPCForNonEntryAction(t *testing.T) {
	t.Parallel()
	operation := PersistedOperation{
		Operation: Operation{Decision: Decision{Action: ReportNAV}},
		Status:    Built,
	}
	reason, err := preBroadcastRecoveryReason(context.Background(), nil, operation)
	if err != nil || reason != "prebroadcast_restart_reobserve_required" {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	if !CanTransition(Built, Failed) || CanTransition(Confirmed, Failed) {
		t.Fatal("pre-broadcast terminal boundary drifted")
	}
}

func TestPreBroadcastRecoveryRejectsPostBroadcastStatus(t *testing.T) {
	t.Parallel()
	operation := PersistedOperation{Status: BroadcastIntent}
	if _, err := preBroadcastRecoveryReason(context.Background(), nil, operation); err == nil {
		t.Fatal("post-broadcast operation accepted by pre-broadcast recovery")
	}
}
