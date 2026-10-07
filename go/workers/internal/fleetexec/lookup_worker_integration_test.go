package fleetexec

import (
	"bytes"
	"encoding/base64"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLookupAutonomousTickSignsOnlyAfterBudgetAndRecoversWithoutKey(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	store, op := seedLookupSource(t, ctx, pool, f)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	keys := 0
	config := LookupWorkerConfig{Cluster: "localnet", Owner: "lookup-autonomous", LeaseTTL: time.Minute, TickDeadline: 30 * time.Second, PollInterval: time.Second, Budget: LookupBudget{MaximumLamports: 10000000, RollingWindow: time.Hour}, Facts: testFacts()}
	worker, err := NewLookupWorker(store, svm.rpc, config, func(keyctx context.Context, address string) (ed25519.PrivateKey, error) {
		keys++
		if address != f.Manager {
			t.Fatal("key callback received a different authority")
		}
		var durable bool
		if err := pool.QueryRow(keyctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_cluster_budget_reservations r JOIN loyal_yield.lookup_table_operations o ON o.id=r.operation_id WHERE o.id=$1 AND r.fencing_token=o.fencing_token AND r.lease_owner=o.lease_owner AND r.reserved_until>clock_timestamp() AND o.transaction_signature IS NULL)`, op.Intent.OperationID).Scan(&durable); err != nil || !durable {
			t.Fatalf("key accessed before durable source budget: %v", err)
		}
		return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var reportedBank uint64
	var reportedReady bool
	worker.SetRuntimeReporter(func(ready bool, slot uint64) { reportedReady = ready; reportedBank = slot })
	if worked, err := worker.Tick(ctx); err != nil || !worked {
		t.Fatalf("actual fresh tick: %v/%v", worked, err)
	}
	if !reportedReady || reportedBank != 1000 {
		t.Fatal("runtime fabricated or omitted actual bank frontier", reportedReady, reportedBank)
	}
	owned, err := lookupAttemptOf(loadLookupOperation(t, ctx, pool, op.Intent.OperationID))
	if err != nil || len(owned.Wire.SignedTransaction) == 0 || owned.BroadcastCount < 1 || keys != 1 {
		t.Fatalf("signed packet not landed from its operation row %+v keys%d error%v", owned, keys, err)
	}
	var reservations int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_cluster_budget_reservations WHERE operation_id=$1`, op.Intent.OperationID).Scan(&reservations); err != nil || reservations != 1 {
		t.Fatal("unexpected budget ownership", reservations, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioner_controls SET paused=true,control_epoch=control_epoch+1 WHERE cluster='localnet'`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	config.Owner = "lookup-restarted"
	config.ReconcileOnly = true
	recovery, err := NewLookupWorker(store, svm.rpc, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := recovery.Tick(ctx); err != nil || !worked {
		t.Fatalf("same-bank paused recovery: %v/%v", worked, err)
	}
	if err = svm.direct("advanceSlot", []any{1001}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	if worked, err := recovery.Tick(ctx); err != nil || !worked {
		t.Fatalf("actual warmed paused recovery: %v/%v", worked, err)
	}
	var state, saved string
	var amount int
	if err = pool.QueryRow(ctx, `SELECT o.operation_state,o.operation_context->>'signedTransaction',t.usable_address_count FROM loyal_yield.lookup_table_operations o JOIN loyal_yield.route_lookup_tables t ON t.id=o.route_lookup_table_id WHERE o.id=$1`, op.Intent.OperationID).Scan(&state, &saved, &amount); err != nil {
		t.Fatal(err)
	}
	if state != "complete" || keys != 1 || amount != 2 || saved != base64.StdEncoding.EncodeToString(owned.Wire.SignedTransaction) {
		t.Fatal("restart changed exact packet/key/capacity", state, keys, amount)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET desired_state='paused' WHERE id=$1`, op.Intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	var verifyID int64
	if err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_operations(idempotency_key,family_id,route_lookup_table_id,operation_kind,mutation_epoch) VALUES('lookup-readonly-verify',$1,$2,'verify',1) RETURNING id`, op.Intent.FamilyID, op.Intent.TableID).Scan(&verifyID); err != nil {
		t.Fatal(err)
	}
	if worked, err := recovery.Tick(ctx); err != nil || !worked {
		t.Fatal("paused read-only verify failed", worked, err)
	}
	var epoch int64
	if err = pool.QueryRow(ctx, `SELECT o.operation_state,t.mutation_epoch FROM loyal_yield.lookup_table_operations o JOIN loyal_yield.route_lookup_tables t ON t.id=o.route_lookup_table_id WHERE o.id=$1`, verifyID).Scan(&state, &epoch); err != nil || state != "complete" || epoch != 2 {
		t.Fatal("read-only source projection version not atomically advanced", state, epoch, err)
	}
	if worked, err := recovery.Tick(ctx); err != nil || worked {
		t.Fatal("completed verification reclaimed on restart", worked, err)
	}
	if err = pool.QueryRow(ctx, `SELECT mutation_epoch FROM loyal_yield.route_lookup_tables WHERE id=$1`, op.Intent.TableID).Scan(&epoch); err != nil || epoch != 2 || keys != 1 {
		t.Fatal("replay advanced verification or accessed key", epoch, keys, err)
	}
	t.Log("real autonomous Go lease→simulation→source budget→key→immutable packet→single send→paused restart→actual warmed ALT receipt")
}

func TestLookupWorkerTransportOutageReportsUnhealthyAndJoins(t *testing.T) {
	pool := lookupRegisteredPool(t)
	store, _ := seedLookupSource(t, t.Context(), pool, readLookupFixture(t))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	rpc, err := NewLookupRPC(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reports := make(chan error, 1)
	frontiers := make(chan struct {
		ready bool
		slot  uint64
	}, 1)
	worker, err := NewLookupWorker(store, rpc, LookupWorkerConfig{Cluster: "localnet", Owner: "lookup-health", LeaseTTL: time.Minute, TickDeadline: time.Second, PollInterval: 10 * time.Millisecond, Budget: LookupBudget{MaximumLamports: 10000000, RollingWindow: time.Hour}, ReconcileOnly: true, Facts: testFacts(), OnHealth: func(err error) { reports <- err }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	worker.SetRuntimeReporter(func(ready bool, slot uint64) {
		select {
		case frontiers <- struct {
			ready bool
			slot  uint64
		}{ready, slot}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case err := <-reports:
		if err == nil {
			t.Fatal("transport outage reported healthy")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("outage never reported")
	}
	select {
	case frontier := <-frontiers:
		if frontier.ready || frontier.slot != 0 {
			t.Fatal("failed transport reported fabricated readiness/frontier", frontier)
		}
	case <-time.After(time.Second):
		t.Fatal("failed transport had no runtime report")
	}
	select {
	case err := <-done:
		t.Fatal("operational outage terminated autonomous runtime", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("runtime cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lookup runtime failed to join cancellation")
	}
}

func TestLookupRustSignedPacketWithoutBytesResolvesBySignature(t *testing.T) {
	// A packet the Rust provisioner signed has no bytes on its row: the family
	// cannot resend it, but it lands or expires by its signature like any other.
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	store, op := seedLookupSource(t, ctx, pool, f)
	wire := lookupOfficialWire(t, f, "create")
	hash, _, _, err := svm.rpc.LookupBlockhash(ctx)
	if err != nil || hash != wire.RecentBlockhash {
		t.Fatal("official original bank hash drift", hash, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state='signed',transaction_signature=$2,message_hash=$3,recent_blockhash=$4,last_valid_block_height=$5,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, op.Intent.OperationID, wire.TransactionSignature, wire.MessageHash, wire.RecentBlockhash, wire.LastValidBlockHeight); err != nil {
		t.Fatal(err)
	}
	config := LookupWorkerConfig{Cluster: "localnet", Owner: "lookup-rust-signed", LeaseTTL: time.Minute, TickDeadline: 20 * time.Second, PollInterval: time.Second, Budget: LookupBudget{MaximumLamports: 10000000, RollingWindow: time.Hour}, Facts: testFacts()}
	worker, err := NewLookupWorker(store, svm.rpc, config, func(context.Context, string) (ed25519.PrivateKey, error) {
		t.Fatal("resolving a signed packet loaded a key")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.Tick(ctx); err != nil || !worked {
		t.Fatal("unseen Rust packet not held", worked, err)
	}
	var state, signature string
	if err = pool.QueryRow(ctx, `SELECT operation_state,transaction_signature FROM loyal_yield.lookup_table_operations WHERE id=$1`, op.Intent.OperationID).Scan(&state, &signature); err != nil || state != "needs_reconcile" || signature != wire.TransactionSignature {
		t.Fatal("unseen packet reset its signature", state, signature, err)
	}
	// The Rust sender's packet reaches the bank.
	if err = svm.rpc.SendWire(ctx, wire.SignedTransaction, true); err != nil {
		t.Fatal(err)
	}
	if err = svm.direct("advanceSlot", []any{1001}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.Tick(ctx); err != nil || !worked {
		t.Fatal("finalized Rust packet not applied", worked, err)
	}
	var usable int
	var fee, epoch int64
	if err = pool.QueryRow(ctx, `SELECT o.operation_state,o.transaction_signature,o.actual_fee_lamports,t.usable_address_count,t.mutation_epoch FROM loyal_yield.lookup_table_operations o JOIN loyal_yield.route_lookup_tables t ON t.id=o.route_lookup_table_id WHERE o.id=$1`, op.Intent.OperationID).Scan(&state, &signature, &fee, &usable, &epoch); err != nil {
		t.Fatal(err)
	}
	if state != "complete" || signature != wire.TransactionSignature || fee != 5000 || usable != 2 || epoch != 1 {
		t.Fatal("Rust packet completion", state, signature, fee, usable, epoch)
	}
}
