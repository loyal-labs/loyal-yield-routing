package multiply

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func runtimeFixtureRoute(t *testing.T, store *Store) (*RouteState, *EarnMaxTopology) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seed := sha256.Sum256([]byte(t.Name() + time.Now().UTC().String()))
	settings := solana.PublicKey(seed)
	topology, err := DeriveEarnMaxTopology(settings, 320)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Pool().Exec(ctx, `INSERT INTO loyal_yield.earn_max_policy_sets
 (settings,vault_index,vault,manifest_version,manifest_sha256,status,policy_accounts,observed_signature,observed_slot,observed_at,policy_seed_base)
 VALUES ($1,$2,$3,'earn-max-v2',$4,'ready','[]'::jsonb,'fixture',500,now(),320)`, settings.String(), topology.VaultIndex, topology.Vault.String(), PolicyDataHash([]byte("fixture")))
	if err != nil {
		t.Fatal(err)
	}
	routeKey := routeKeyFor(settings, topology.VaultIndex)
	t.Cleanup(func() {
		cleanup, close := context.WithTimeout(context.Background(), 5*time.Second)
		defer close()
		if _, err := store.Pool().Exec(cleanup, `DELETE FROM loyal_yield.multiply_route_states route
WHERE route_key=$1 AND NOT EXISTS (SELECT 1 FROM loyal_yield.multiply_operations op WHERE op.route_key=route.route_key)`, routeKey); err != nil {
			t.Error(err)
		}
		if _, err := store.Pool().Exec(cleanup, "DELETE FROM loyal_yield.earn_max_policy_sets WHERE settings=$1", settings.String()); err != nil {
			t.Error(err)
		}
	})
	state, err := NewRouteState(routeKey, settings.String(), topology.VaultIndex, topology.Vault.String(), 320, TokenBalance{Account: topology.ClaimCustody.String(), Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: 0}, 500, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateRouteState(ctx, state)
	if err != nil || !created {
		t.Fatalf("create route %v %v", created, err)
	}
	return state, topology
}

func TestWorkerRecoversPreparedCrashBeforeDisabledAdmission(t *testing.T) {
	store := integrationStore(t)
	state, topology := runtimeFixtureRoute(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	old, err := store.LeaseRoute(ctx, state.RouteKey, "legacy-owner", time.Now().Add(time.Minute))
	if err != nil || old == nil {
		t.Fatalf("old lease %v %v", old, err)
	}
	operation := integrationOperation(state.RouteKey, state.Cycle, old.Version+1)
	state.Generation++
	state.CurrentOperationID = &operation.OperationID
	if ok, err := store.PrepareOperation(ctx, old, state, operation); err != nil || !ok {
		t.Fatalf("prepare %v %v", ok, err)
	}
	if ok, err := store.ReleaseLease(ctx, old); err != nil || !ok {
		t.Fatalf("release %v %v", ok, err)
	}
	// Recover unsigned durable preparation even after the admission projection
	// disappears. No fresh account read, quote, or signing is necessary.
	if _, err := store.Pool().Exec(ctx, "DELETE FROM loyal_yield.earn_max_policy_sets WHERE settings=$1", state.Settings); err != nil {
		t.Fatal(err)
	}
	executor, _, _ := testExecutor(t)
	worker, err := NewWorker(WorkerDeps{Store: store, Observer: forbiddenObservationReader{}, Executor: executor, Quotes: fakeQuoteClient{topology}, WorkerID: "go-recovery", Chain: executor.Chain.(*fakeChain), Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.Tick(ctx)
	if err != nil || result.Condition != "prepared_operation_rebuilt" {
		t.Fatalf("recover %v %v", result, err)
	}
	saved, err := store.LoadRouteState(ctx, state.RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State.CurrentOperationID != nil || saved.Operation != nil {
		t.Fatal("crashed unsigned operation retained active ownership")
	}
	if ok, err := store.SaveRouteState(ctx, old, state); err != nil || ok {
		t.Fatalf("stale legacy fence changed recovered state %v %v", ok, err)
	}
}

func TestWorkerExpiresUnlandedWireLikeRustWithoutSending(t *testing.T) {
	// The Go branch held an expired, unlanded wire until a finalized
	// account-history proof existed; Rust's expire_multiply_operation treats
	// signature absence past the blockhash as terminal, and so does land.
	store := integrationStore(t)
	state, topology := runtimeFixtureRoute(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := store.LeaseRoute(ctx, state.RouteKey, "signer", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("lease %v %v", lease, err)
	}
	operation := integrationOperation(state.RouteKey, state.Cycle, lease.Version+1)
	state.Generation++
	state.CurrentOperationID = &operation.OperationID
	if ok, err := store.PrepareOperation(ctx, lease, state, operation); err != nil || !ok {
		t.Fatalf("prepare %v %v", ok, err)
	}
	signed := signedWireFixture(t, false)
	messageHash, err := MessageSHA256(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.PersistSignedOperation(ctx, lease, operation.OperationID, topology.Vault.String(), PolicyDataHash([]byte("fixture")), messageHash, signed); err != nil || !ok {
		t.Fatalf("persist %v %v", ok, err)
	}
	if ok, err := store.ReleaseLease(ctx, lease); err != nil || !ok {
		t.Fatalf("release %v %v", ok, err)
	}
	_, fake, _ := testExecutor(t)
	fake.height = uint64(signed.LastValidBlockHeight) + 1
	worker, err := NewRecoveryWorker(WorkerDeps{Store: store, Observer: forbiddenObservationReader{}, Executor: &Executor{Chain: fake}, WorkerID: "go-landing", Chain: fake, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.Tick(ctx)
	if err != nil || result.Condition != "operation_expired_without_effect" || len(fake.sent) != 0 {
		t.Fatalf("expiry %v %v sent=%d", result, err, len(fake.sent))
	}
	var status string
	var wireCleared bool
	if err := store.Pool().QueryRow(ctx, `SELECT status,signed_wire IS NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operation.OperationID).Scan(&status, &wireCleared); err != nil || status != "expired" || !wireCleared {
		t.Fatalf("operation row %s %v %v", status, wireCleared, err)
	}
	saved, err := store.LoadRouteState(ctx, state.RouteKey)
	if err != nil || saved.State.CurrentOperationID != nil {
		t.Fatal("expired wire kept the route", err)
	}
}

type forbiddenObservationReader struct{}

func (forbiddenObservationReader) Accounts(context.Context, []solana.PublicKey, rpc.CommitmentType, uint64) (uint64, []*chain.Account, error) {
	return 0, nil, errors.New("fresh observation forbidden during prepared recovery")
}

// blockingReader holds its read until the caller cancels it.
type blockingReader struct{ started, returned chan struct{} }

func (r blockingReader) Accounts(ctx context.Context, _ []solana.PublicKey, _ rpc.CommitmentType, _ uint64) (uint64, []*chain.Account, error) {
	close(r.started)
	<-ctx.Done()
	close(r.returned)
	return 0, nil, ctx.Err()
}

func TestWorkerCancellationReleasesLeaseAfterOwnedReadJoins(t *testing.T) {
	store := integrationStore(t)
	state, topology := runtimeFixtureRoute(t, store)
	started, returned := make(chan struct{}), make(chan struct{})
	executor, fake, _ := testExecutor(t)
	worker, err := NewWorker(WorkerDeps{Store: store, Observer: blockingReader{started, returned}, Executor: executor, Quotes: fakeQuoteClient{topology}, WorkerID: "cancelled-owner", RouteKey: &state.RouteKey, Chain: fake, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := worker.Tick(ctx); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("owned read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("tick cancellation %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not join IO and cleanup")
	}
	select {
	case <-returned:
	default:
		t.Fatal("lease released before read joined")
	}
	check, close := context.WithTimeout(context.Background(), 5*time.Second)
	defer close()
	lease, err := store.LeaseRoute(check, state.RouteKey, "replacement-owner", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("cancelled worker retained lease %v %v", lease, err)
	}
	saved, err := store.LoadRouteState(check, state.RouteKey)
	if err != nil || saved.Operation != nil {
		t.Fatalf("cancelled observation fabricated an operation %v", err)
	}
}
