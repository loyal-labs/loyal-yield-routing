package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5/pgxpool"
)

func lookupRegisteredPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	endpoint := os.Getenv("LOOKUP_TEST_DATABASE_URL")
	if endpoint == "" {
		t.Skip("requires registered dedicated lookup fixture")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "postgresql" || u.Hostname() != "127.0.0.1" || lookupFixturePortInvalid(u.Port()) || u.User == nil || u.User.Username() != "workers_v2" || u.Path != "/workers_v2_lookup" || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("refusing database outside registered dedicated lookup fixture")
	}
	if _, present := u.User.Password(); present {
		t.Fatal("lookup fixture URL cannot contain a password")
	}
	c, err := pgxpool.ParseConfig(endpoint)
	if err != nil {
		t.Fatal("invalid lookup fixture configuration")
	}
	c.MaxConns = 2
	c.ConnConfig.ConnectTimeout = 3 * time.Second
	pool, err := pgxpool.NewWithConfig(t.Context(), c)
	if err != nil {
		t.Fatal("lookup fixture unavailable")
	}
	t.Cleanup(pool.Close)
	var database, role string
	if err = pool.QueryRow(t.Context(), `SELECT current_database(),current_user`).Scan(&database, &role); err != nil || database != "workers_v2_lookup" || role != "workers_v2" {
		t.Fatal("lookup fixture identity differs")
	}
	return pool
}
func TestLookupRegisteredJournalOwnsActualPacketAcrossPauseAndLeaseTransfer(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	hash, height, bank, err := svm.rpc.LookupBlockhash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store, operation, prepared, owned := seedLookupJournal(t, ctx, pool, f, hash, height, bank)
	intent := operation.Intent
	for _, query := range []string{
		`UPDATE loyal_yield.lookup_table_operations SET transaction_signature=NULL,operation_state='retry_wait' WHERE id=$1`,
		`UPDATE loyal_yield.lookup_table_operations SET operation_state='complete' WHERE id=$1`,
	} {
		if _, err = pool.Exec(ctx, query, intent.OperationID); err == nil {
			t.Fatal("source writer reset/terminalized owned unresolved packet")
		}
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET mutation_epoch=1 WHERE id=$1`, intent.TableID); err == nil {
		t.Fatal("physical epoch changed under unresolved packet")
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_signed_attempts SET signed_transaction=substring(signed_transaction from 2) WHERE id=$1`, owned.ID); err == nil {
		t.Fatal("owned packet mutated")
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioner_controls SET paused=true,control_epoch=control_epoch+1 WHERE cluster='localnet'`); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordLookupBroadcastIntent(ctx, operation, owned); !errors.Is(err, ErrLookupPaused) {
		t.Fatalf("pause grant: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET desired_state='paused' WHERE id=$1`, intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	replay, err := store.PersistLookupPrepared(ctx, operation, prepared)
	if err != nil || replay.ID != owned.ID || !bytes.Equal(replay.Wire.SignedTransaction, owned.Wire.SignedTransaction) {
		t.Fatalf("pause replaced prepared packet: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET desired_state='active' WHERE id=$1`, intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioner_controls SET paused=false,control_epoch=control_epoch+1 WHERE cluster='localnet'`); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordLookupBroadcastIntent(ctx, operation, owned); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordLookupBroadcastIntent(ctx, operation, owned); err == nil {
		t.Fatal("duplicate broadcast intent accepted")
	}
	// The original authorized sender reached the bank, then lost its
	// response before updating PostgreSQL. Recovery will only observe it.
	if err = svm.rpc.Send(ctx, owned.Wire.SignedTransaction); err != nil {
		t.Fatal(err)
	}
	// Crash/lease transfer after the durable send boundary. New owner recovers
	// the immutable packet rather than creating a new signature or a new permit.
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET lease_owner='lookup-recovery',fencing_token=2,lease_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, intent.OperationID); err != nil {
		t.Fatal(err)
	}
	if err = store.deferLookupRecovery(ctx, operation, owned, "stale sender"); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("old lease updated recovery: %v", err)
	}
	operation.Lease.Owner, operation.Lease.FencingToken = "lookup-recovery", 2
	durable, err := store.LoadLookupAttempt(ctx, intent.OperationID)
	if err != nil || durable == nil || durable.BroadcastCount != 1 || durable.State != LookupUnknown || !bytes.Equal(durable.Wire.SignedTransaction, owned.Wire.SignedTransaction) {
		t.Fatalf("restart owned packet: %+v %v", durable, err)
	}
	receipt, err := svm.rpc.LookupFinalizedReceipt(ctx, durable.Wire.TransactionSignature)
	if err != nil || receipt == nil {
		t.Fatal("actual receipt missing", err)
	}
	status, err := svm.rpc.SignatureStatus(ctx, durable.Wire.TransactionSignature)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := svm.rpc.LookupSnapshot(ctx, f.Table, receipt.Slot)
	if err != nil {
		t.Fatal(err)
	}
	result, err := recoverLookup(*durable, status, receipt, snapshot)
	if err != nil || result.proof != nil {
		t.Fatal("same-bank growth was not held", err)
	}
	if err = svm.direct("advanceSlot", []any{1001}, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err = svm.rpc.LookupSnapshot(ctx, f.Table, receipt.Slot)
	if err != nil {
		t.Fatal(err)
	}
	result, err = recoverLookup(*durable, status, receipt, snapshot)
	if err != nil || result.proof == nil {
		t.Fatal("actual warmed proof missing", err)
	}
	if err = store.commitLookupProof(ctx, operation, *durable, result.proof); err != nil {
		t.Fatal(err)
	}
	var attemptState, sourceState string
	var broadcasts, count, usable int
	var epoch, fee, rent int64
	var saved []byte
	if err = pool.QueryRow(ctx, `SELECT a.attempt_state,a.broadcast_count,a.signed_transaction,o.operation_state,o.actual_fee_lamports,o.actual_rent_lamports,t.address_count,t.usable_address_count,t.mutation_epoch FROM loyal_yield.lookup_table_signed_attempts a JOIN loyal_yield.lookup_table_operations o ON o.id=a.operation_id JOIN loyal_yield.route_lookup_tables t ON t.id=a.route_lookup_table_id WHERE a.id=$1`, owned.ID).Scan(&attemptState, &broadcasts, &saved, &sourceState, &fee, &rent, &count, &usable, &epoch); err != nil {
		t.Fatal(err)
	}
	if attemptState != "reconciled" || sourceState != "complete" || broadcasts != 1 || !bytes.Equal(saved, owned.Wire.SignedTransaction) || fee != 5000 || rent != int64(snapshot.Lamports) || count != 2 || usable != 2 || epoch != 1 {
		t.Fatalf("incorrect actual source projection: %s/%s broadcasts%d fee%d rent%d count%d/%d epoch%d", attemptState, sourceState, broadcasts, fee, rent, count, usable, epoch)
	}
	t.Log("registered source lease/budget/permit + immutable packet survives pause/restart/fence; actual ALT execution/readback atomically finalizes source membership and SOL accounting")
}

func lookupFixturePortInvalid(port string) bool {
	value, err := strconv.Atoi(port)
	return err != nil || value < 1024 || value > 65535
}
func seedLookupJournal(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f lookupFixture, hash string, height, bank int64) (*Store, LookupOperation, LookupAttempt, LookupAttempt) {
	store, operation := seedLookupSource(t, ctx, pool, f)
	intent := operation.Intent
	prepared := LookupAttempt{Intent: intent, SigningContextSlot: bank, EstimatedFeeLamports: 5000, EstimatedRentLamports: 5000000}
	// Existing source budget is deliberately reserved BEFORE invoking the key.
	policy := LookupBudget{MaximumLamports: 5000000, RollingWindow: time.Hour}
	if approved, err := store.ReserveLookupBudget(ctx, operation, policy, 5000, 5000000); err != nil || approved {
		t.Fatalf("budget cap failed: %v %v", approved, err)
	}
	policy.MaximumLamports = 6000000
	if approved, err := store.ReserveLookupBudget(ctx, operation, policy, 5000, 5000000); err != nil || !approved {
		t.Fatalf("source budget reservation: %v %v", approved, err)
	}
	if approved, err := store.ReserveLookupBudget(ctx, operation, policy, 5000, 5000000); err != nil || !approved {
		t.Fatalf("source exact budget replay: %v %v", approved, err)
	}
	if _, err := store.ReserveLookupBudget(ctx, operation, policy, 5001, 5000000); err == nil {
		t.Fatal("budget fence replay changed accounting")
	}
	var err error
	prepared.Wire, err = signLookupMutation(intent, hash, height, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	owned, err := store.PersistLookupPrepared(ctx, operation, prepared)
	if err != nil {
		t.Fatal(err)
	}
	return store, operation, prepared, owned
}

func seedLookupSource(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f lookupFixture, kinds ...string) (*Store, LookupOperation) {
	t.Helper()
	store, err := NewStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.RequireLookupSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `TRUNCATE loyal_yield.lookup_table_families CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioner_controls SET paused=false,control_epoch=control_epoch+1 WHERE cluster='localnet'`); err != nil {
		t.Fatal(err)
	}
	intent := lookupFixtureIntent(f)
	kind, allocation := "vault_shards", "vault_shard"
	if len(kinds) > 0 {
		kind = kinds[0]
		if kind != "shared_market" {
			t.Fatal("unsupported lookup fixture family")
		}
		allocation = "shared_market"
	}
	if err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_families(cluster,logical_name,kind,planner_version,catalog_version,provisioning_authority,payer,hard_capacity,largest_atomic_expansion,safety_margin,allocation_high_water) VALUES('localnet','lookup-fixture',$2,'v2-test','v2-test',$1,$1,256,20,8,228) RETURNING id`, f.Manager, kind).Scan(&intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_lookup_tables(cluster,scope,table_address,authority,payer,status,family_id,allocation_kind,generation,shard_ordinal,desired_state,accepting_allocations,allocation_high_water,reserved_address_count,usable_address_count,mutation_epoch) VALUES('localnet','lookup-fixture',$1,$2,$2,'warming',$3,$4,0,0,'preparing',false,228,2,0,0) RETURNING id`, f.Table, f.Manager, intent.FamilyID, allocation).Scan(&intent.TableID); err != nil {
		t.Fatal(err)
	}
	contextBytes, _ := json.Marshal(map[string]any{"recent_slot": f.RecentSlot})
	if err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_operations(idempotency_key,family_id,route_lookup_table_id,operation_kind,operation_state,target_generation,target_shard_ordinal,operation_context,mutation_epoch,lease_owner,lease_expires_at,fencing_token) VALUES('lookup-fixture-create',$1,$2,'create','leased',0,0,$3,0,'lookup-first',clock_timestamp()+interval '60 seconds',1) RETURNING id`, intent.FamilyID, intent.TableID, contextBytes).Scan(&intent.OperationID); err != nil {
		t.Fatal(err)
	}
	for n, address := range intent.Extension {
		if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_operation_addresses(operation_id,address,ordinal) VALUES($1,$2,$3)`, intent.OperationID, address, n); err != nil {
			t.Fatal(err)
		}
	}
	operation := LookupOperation{Intent: intent, Lease: LookupLease{Owner: "lookup-first", FencingToken: 1}}
	return store, operation
}

func TestLookupRegisteredJournalStructuralGuards(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	store, operation, prepared, owned := seedLookupJournal(t, t.Context(), pool, f, sdk.Hash([32]byte{42}).String(), 1150, 1000)
	if err := store.RecordLookupBroadcastIntent(t.Context(), operation, owned); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordLookupBroadcastIntent(t.Context(), operation, owned); err == nil {
		t.Fatal("second network intent accepted")
	}
	current, err := store.LoadLookupAttempt(t.Context(), operation.Intent.OperationID)
	if err != nil || current == nil || current.BroadcastCount != 1 || current.State != LookupUnknown {
		t.Fatalf("durable unknown journal: %+v %v", current, err)
	}
	if _, err = pool.Exec(t.Context(), `UPDATE loyal_yield.lookup_table_operations SET operation_state='complete' WHERE id=$1`, operation.Intent.OperationID); err == nil {
		t.Fatal("legacy terminalization erased unresolved packet")
	}
	if _, err = pool.Exec(t.Context(), `UPDATE loyal_yield.lookup_table_signed_attempts SET attempt_state='expired',history_complete=false WHERE id=$1`, owned.ID); err == nil {
		t.Fatal("signature absence expired packet without actual history/noeffect evidence")
	}
	if _, err = pool.Exec(t.Context(), `UPDATE loyal_yield.lookup_table_operations SET lease_owner='new-owner',fencing_token=fencing_token+1 WHERE id=$1`, operation.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.PersistLookupPrepared(t.Context(), operation, prepared); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("stale lease wrote journal: %v", err)
	}
}
