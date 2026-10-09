package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"
)

func TestLookupUnsignedAgedReservationRefreshesOnlyToActualProducedBank(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	store, op := seedLookupSource(t, ctx, pool, f)
	// These are actual fixture bank advances. A skipped numeric warp alone is
	// insufficient to replace the original SlotHashes membership proof.
	for slot := 1001; slot <= 1513; slot++ {
		if err := svm.direct("advanceSlot", []any{slot}, nil); err != nil {
			t.Fatal(err)
		}
	}
	old, err := lookupSnapshot(ctx, svm.rpc, f.Table, 1513)
	if err != nil || lookupProducedSlot(old.SlotHashes, f.RecentSlot) {
		t.Fatal("fixture retained original bank", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	worker, err := NewLookupWorker(store, svm.rpc, LookupWorkerConfig{Cluster: "localnet", Owner: "refresh-fixture", LeaseTTL: time.Minute, TickDeadline: 30 * time.Second, PollInterval: time.Second, Budget: LookupBudget{MaximumLamports: 10000000, RollingWindow: time.Hour}, Facts: testFacts()}, func(context.Context, string) (ed25519.PrivateKey, error) {
		return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.Tick(ctx); err != nil || !worked {
		t.Fatal("actual aged reservation fresh tick", worked, err)
	}
	owned, err := lookupAttemptOf(loadLookupOperation(t, ctx, pool, op.Intent.OperationID))
	if err != nil || owned.Intent.TableAddress == f.Table || owned.Intent.RecentSlot == nil || !lookupProducedSlot(old.SlotHashes, *owned.Intent.RecentSlot) || owned.BroadcastCount < 1 {
		t.Fatal("reservation invented slot or reused old address", owned, err)
	}
	receipt, err := lookupFinalizedReceipt(ctx, svm.rpc, owned.Wire.TransactionSignature)
	if err != nil || receipt == nil || receipt.Err != "" {
		t.Fatal("refreshed PDA did not execute actual ALT create", receipt, err)
	}
	if err = svm.direct("advanceSlot", []any{1514}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = pool.QueryRow(ctx, `SELECT operation_state FROM loyal_yield.lookup_table_operations WHERE id=$1`, op.Intent.OperationID).Scan(&state); err != nil || state != "complete" {
		t.Fatal("refreshed actual packet not reconciled", state, err)
	}
}
