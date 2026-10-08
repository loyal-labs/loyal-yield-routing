package multiply

// Gated integration tests against the real registered loyal_yield schema.
// They run only when MULTIPLY_TEST_DATABASE_URL is exported (root CI provides
// the workers_v2 role on loopback database workers_v2_multiply); without it
// the tests skip rather than fake a pass. No surrogate DDL is created here.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

func integrationStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("MULTIPLY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MULTIPLY_TEST_DATABASE_URL not set; database integration skipped")
	}
	if err := validateFixtureDSN(dsn); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(context.Background(), dsn)
	if err != nil {
		t.Fatalf("configured database unavailable: %v", err)
	}
	var database, role string
	if err := store.Pool().QueryRow(context.Background(), "SELECT current_database(), current_user").Scan(&database, &role); err != nil {
		t.Fatalf("configured fixture identity unavailable: %v", err)
	}
	if database != "workers_v2_multiply" || role != "workers_v2" {
		t.Fatalf("configured fixture identity refused: database=%q role=%q", database, role)
	}
	if err := store.RequireSchema(context.Background()); err != nil {
		t.Fatalf("registered multiply schema missing: %v", err)
	}
	t.Cleanup(store.Pool().Close)
	return store
}

// seedPolicySet inserts one ready earn-max-v2 policy set for a unique
// settings identity and returns its topology inputs.
func seedPolicySet(t *testing.T, store *Store) (solana.PublicKey, string) {
	t.Helper()
	identity := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", t.Name(), time.Now().UnixNano())))
	settings := solana.PublicKey(identity).String()
	topology, err := DeriveEarnMaxTopology(solana.PublicKey(identity), 320)
	if err != nil {
		t.Fatal(err)
	}
	vault := topology.Vault.String()
	_, err = store.Pool().Exec(context.Background(), `
        INSERT INTO loyal_yield.earn_max_policy_sets (
            settings, vault_index, vault, manifest_version, manifest_sha256,
            status, policy_accounts, observed_signature, observed_slot,
            observed_at, policy_seed_base
        ) VALUES ($1, $3, $2, 'earn-max-v2',
                  'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
                  'ready', '[]'::jsonb, 'seed', 331895401, now(), 320)
        ON CONFLICT (settings, vault_index) DO NOTHING`,
		settings, vault, topology.VaultIndex)
	if err != nil {
		t.Fatalf("seed policy set: %v", err)
	}
	t.Cleanup(func() {
		store.Pool().Exec(context.Background(),
			"DELETE FROM loyal_yield.earn_max_policy_sets WHERE settings=$1", settings)
	})
	key, err := solana.PublicKeyFromBase58(settings)
	if err != nil {
		t.Fatal(err)
	}
	return key, vault
}

func integrationOperation(routeKey string, cycle uint64, generation int64) *MultiplyOperation {
	digest := sha256.Sum256([]byte(routeKey))
	action := ActionDepositCollateral
	return &MultiplyOperation{
		OperationID: "mul-" + hex.EncodeToString(digest[:])[:32],
		RouteKey:    routeKey, Cycle: cycle, EngineVersion: EngineVersion,
		Action: action, StrategyKey: SyrupUsdcUsdc, Status: StatusPrepared,
		IDempotencyKey: "engine:" + routeKey + ":cycle:generation",
	}
}

func TestStoreLifecycleCannotReconcileWithoutActualReceipt(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	settings, vault := seedPolicySet(t, store)
	topology, err := DeriveEarnMaxTopology(settings, 320)
	if err != nil {
		t.Fatal(err)
	}
	routeKey := routeKeyFor(settings, EarnMaxVaultIndex)
	t.Cleanup(func() {
		store.Pool().Exec(ctx, "DELETE FROM loyal_yield.multiply_route_states WHERE route_key=$1", routeKey)
	})

	state, err := NewRouteState(routeKey, settings.String(), EarnMaxVaultIndex, vault, 320,
		TokenBalance{Account: topology.ClaimCustody.String(), Mint: USDCMint,
			TokenProgram: TokenProgram, AmountRaw: 1_000_000},
		331895401, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateRouteState(ctx, state)
	if err != nil || !created {
		t.Fatalf("create route: %v %v", created, err)
	}
	if ready, err := store.EarnMaxPolicySetReady(ctx, settings.String(), EarnMaxVaultIndex, 320); err != nil || !ready {
		t.Fatalf("policy readiness %v %v", ready, err)
	}

	// Lease with fencing; a second worker cannot steal a live lease.
	lease, err := store.LeaseRoute(ctx, routeKey, "worker-a", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("lease: %v %v", lease, err)
	}
	if lease.FencingToken != 1 {
		t.Fatalf("first fencing token %d", lease.FencingToken)
	}
	stolen, err := store.LeaseRoute(ctx, routeKey, "worker-b", time.Now().Add(time.Minute))
	if err != nil || stolen != nil {
		t.Fatalf("live lease was stolen: %v %v", stolen, err)
	}
	if ok, err := store.RenewLease(ctx, lease, time.Now().Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("renew: %v %v", ok, err)
	}

	stored, err := store.LoadRouteState(ctx, routeKey)
	if err != nil || stored == nil {
		t.Fatalf("load: %v %v", stored, err)
	}
	if stored.Version != lease.Version || stored.FencingToken != lease.FencingToken {
		t.Fatal("stored lease identity drifted")
	}

	// Prepare the operation bound to the route generation.
	route := stored.State
	operation := integrationOperation(routeKey, route.Cycle, lease.Version+1)
	operationID := operation.OperationID
	route.Generation = uint64(lease.Version + 1)
	route.CurrentOperationID = &operationID
	route.ObservedSlot = 331_895_402
	route.ObservedAt = time.Now().UTC()
	initialLease := *lease
	ok, err := store.PrepareOperation(ctx, lease, route, operation)
	if err != nil || !ok {
		t.Fatalf("prepare: %v %v", ok, err)
	}
	duplicate, err := store.PrepareOperation(ctx, &initialLease, route, operation)
	if err != nil || duplicate {
		t.Fatalf("idempotency replay inserted again: %v %v", duplicate, err)
	}

	// Persist the signed wire; afterwards the wire is immutable.
	signed := signedWireFixture(t, false)
	wire := signed.Wire
	wireMessageHash, err := MessageSHA256(wire)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := store.PersistSignedOperation(ctx, lease, operation.OperationID,
		fixtureKey(31).String(), PolicyDataHash([]byte("policy")), wireMessageHash, signed)
	if err != nil || !persisted {
		t.Fatalf("persist signed: %v %v", persisted, err)
	}
	replacement := signedWireFixture(t, true)
	replacementMessageHash, err := MessageSHA256(replacement.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.PersistSignedOperation(ctx, lease, operation.OperationID,
		fixtureKey(31).String(), PolicyDataHash([]byte("policy")), replacementMessageHash, replacement); err != nil || ok {
		t.Fatalf("signed wire was replaced after persistence: %v %v", ok, err)
	}
	loaded, err := store.LoadOperation(ctx, operation.OperationID)
	if err != nil || loaded == nil || loaded.Status != StatusSignedPersisted {
		t.Fatalf("load operation: %v %v", loaded, err)
	}
	if string(loaded.SignedWire) != string(wire) {
		t.Fatal("persisted wire drifted from the signed bytes")
	}

	// Broadcast intent, then confirm; a stale fencing token cannot advance.
	stale := *lease
	stale.FencingToken--
	if ok, _ := store.MarkBroadcastIntent(ctx, &stale, operation.OperationID, time.Now().UTC()); ok {
		t.Fatal("stale fencing token advanced the operation")
	}
	if ok, err := store.MarkBroadcastIntent(ctx, lease, operation.OperationID, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("broadcast intent: %v %v", ok, err)
	}
	if ok, err := store.MarkConfirmed(ctx, lease, operation.OperationID, 331_895_500); err != nil || !ok {
		t.Fatalf("confirm: %v %v", ok, err)
	}

	// Intentional stronger contract: a generic fixture packet and a caller hash
	// cannot stand in for an actual financial transaction receipt.
	stored, err = store.LoadRouteState(ctx, routeKey)
	if err != nil {
		t.Fatal(err)
	}
	next := stored.State
	next.Generation = uint64(lease.Version + 1)
	next.CurrentOperationID = nil
	next.ObservedSlot = 331_895_500
	next.ObservedAt = time.Now().UTC()
	next.Position = NewIdlePosition(TokenBalance{Account: topology.ClaimCustody.String(),
		Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: 1_000_000})
	digest := sha256.Sum256([]byte("reconciliation:" + operation.OperationID))
	if ok, err := store.ReconcileOperation(ctx, lease, operation.OperationID,
		signed.TransactionSignature, hex.EncodeToString(digest[:]), 331_895_500, next); err == nil || ok {
		t.Fatalf("receipt-less reconcile: %v %v", ok, err)
	}
	held, err := store.LoadRouteState(ctx, routeKey)
	if err != nil || held.Operation == nil || held.Operation.Status != StatusConfirmed || held.State.CurrentOperationID == nil || string(held.Operation.SignedWire) != string(wire) {
		t.Fatalf("receipt-less reconciliation lost ownership/wire: %v", err)
	}
	next.Goal = GoalManualRecovery
	reason := "generic store fixture has no financial transaction receipt"
	next.ManualRecoveryReason = &reason
	if ok, err := store.MarkManualRecovery(ctx, lease, operation.OperationID, next); err != nil || !ok {
		t.Fatalf("fixture manual hold: %v %v", ok, err)
	}
	if exists, err := store.OperationExistsForSignature(ctx, routeKey, signed.TransactionSignature); err != nil || !exists {
		t.Fatalf("signature lookup: %v %v", exists, err)
	}

	// Snapshot bookkeeping on the reconciled route.
	recorded, err := store.RecordPositionSnapshot(ctx, &PositionSnapshotInput{
		RouteKey: routeKey, Generation: next.Generation, ObservedSlot: 331_895_500,
		ObservedAt: time.Now().UTC(), ClaimRaw: "1000000",
		CollateralRaw: "0", DebtRaw: "0",
		EquityUSD: "1000000", CollateralValueUSD: "0", DebtValueUSD: "0",
		ValuationSource: "confirmed_kamino_reserve_curve_500ms",
	})
	if err != nil || !recorded {
		t.Fatalf("snapshot: %v %v", recorded, err)
	}
	if replayed, _ := store.RecordPositionSnapshot(ctx, &PositionSnapshotInput{
		RouteKey: routeKey, Generation: next.Generation, ObservedSlot: 331_895_500,
		ObservedAt: time.Now().UTC(), ClaimRaw: "1000000", CollateralRaw: "0",
		DebtRaw: "0", EquityUSD: "1000000", CollateralValueUSD: "0",
		DebtValueUSD: "0", ValuationSource: "confirmed_kamino_reserve_curve_500ms",
	}); replayed {
		t.Fatal("duplicate snapshot slot was recorded")
	}

	if ok, err := store.ReleaseLease(ctx, lease); err != nil || !ok {
		t.Fatalf("release: %v %v", ok, err)
	}
}

func TestStoreExpiryMatchesRustAndPersistedCorruptionIsDetected(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	settings, vault := seedPolicySet(t, store)
	topology, err := DeriveEarnMaxTopology(settings, 320)
	if err != nil {
		t.Fatal(err)
	}
	routeKey := routeKeyFor(settings, EarnMaxVaultIndex)
	t.Cleanup(func() {
		store.Pool().Exec(ctx, "DELETE FROM loyal_yield.multiply_route_states WHERE route_key=$1", routeKey)
	})
	state, err := NewRouteState(routeKey, settings.String(), EarnMaxVaultIndex, vault, 320, TokenBalance{Account: topology.ClaimCustody.String(), Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: 0}, 331895401, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if created, err := store.CreateRouteState(ctx, state); err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	lease, err := store.LeaseRoute(ctx, routeKey, "expiry-test", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("lease: %v", err)
	}
	stored, err := store.LoadRouteState(ctx, routeKey)
	if err != nil {
		t.Fatal(err)
	}
	route := stored.State
	operation := integrationOperation(routeKey, route.Cycle, lease.Version+1)
	id := operation.OperationID
	route.Generation = uint64(lease.Version + 1)
	route.CurrentOperationID = &id
	if ok, err := store.PrepareOperation(ctx, lease, route, operation); err != nil || !ok {
		t.Fatalf("prepare: %v %v", ok, err)
	}
	signed := signedWireFixture(t, false)
	messageHash, err := MessageSHA256(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.PersistSignedOperation(ctx, lease, id, fixtureKey(34).String(), PolicyDataHash([]byte("policy")), messageHash, signed); err != nil || !ok {
		t.Fatalf("persist: %v %v", ok, err)
	}
	if ok, err := store.MarkBroadcastIntent(ctx, lease, id, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("intent: %v %v", ok, err)
	}
	stored, err = store.LoadRouteState(ctx, routeKey)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := store.LoadOperation(ctx, id)
	if err != nil || retained == nil || retained.Status != StatusBroadcastIntent || len(retained.SignedWire) == 0 {
		t.Fatalf("sent operation not retained: %v %v", retained, err)
	}
	if _, err := PersistedTransaction(retained); err != nil {
		t.Fatal(err)
	}
	// Persisted corruption must be detected instead of replacing the stored hash.
	if _, err := store.Pool().Exec(ctx, "UPDATE loyal_yield.multiply_operations SET signed_wire_sha256=$2 WHERE operation_id=$1", id, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadOperation(ctx, id); err == nil {
		t.Fatal("stored wire corruption was hidden")
	}
	if _, err := store.Pool().Exec(ctx, "UPDATE loyal_yield.multiply_operations SET signed_wire_sha256=$2 WHERE operation_id=$1", id, *retained.SignedWireSHA256); err != nil {
		t.Fatal(err)
	}
	next := stored.State
	next.Generation = uint64(lease.Version + 1)
	next.CurrentOperationID = nil
	// expire_multiply_operation: terminal, wire cleared, route released.
	if ok, err := store.ExpireOperation(ctx, lease, id, next); err != nil || !ok {
		t.Fatalf("expiry: %v %v", ok, err)
	}
	var status string
	var cleared bool
	if err := store.Pool().QueryRow(ctx, "SELECT status,signed_wire IS NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1", id).Scan(&status, &cleared); err != nil || status != "expired" || !cleared {
		t.Fatalf("expired row %s %v %v", status, cleared, err)
	}
}

func TestStoreLeaseNextRouteRunsEligibleRoutes(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	settings, vault := seedPolicySet(t, store)
	topology, err := DeriveEarnMaxTopology(settings, 320)
	if err != nil {
		t.Fatal(err)
	}
	routeKey := routeKeyFor(settings, EarnMaxVaultIndex)
	t.Cleanup(func() {
		store.Pool().Exec(ctx, "DELETE FROM loyal_yield.multiply_route_states WHERE route_key=$1", routeKey)
	})
	state, err := NewRouteState(routeKey, settings.String(), EarnMaxVaultIndex, vault, 320,
		TokenBalance{Account: topology.ClaimCustody.String(), Mint: USDCMint,
			TokenProgram: TokenProgram, AmountRaw: 4_000_000},
		331895401, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	state.Goal = GoalDeploy
	if created, _ := store.CreateRouteState(ctx, state); !created {
		t.Fatal("route create failed")
	}
	lease, err := store.LeaseNextRoute(ctx, "worker-next", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("lease next: %v %v", lease, err)
	}
	if lease.RouteKey != routeKey {
		t.Fatalf("leased %s, want %s", lease.RouteKey, routeKey)
	}
	// A second candidate-less pass must not return the leased route again.
	if again, err := store.LeaseNextRoute(ctx, "worker-b", time.Now().Add(time.Minute)); err != nil || again != nil {
		// Another CI route may legitimately surface; only assert our route
		// cannot be handed out twice.
		if again != nil && again.RouteKey == routeKey {
			t.Fatal("leased route was handed to a second worker")
		}
	}
	if _, err := store.ReleaseLease(ctx, lease); err != nil {
		t.Fatal(err)
	}
}

func validateFixtureDSN(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return err
	}
	if (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.User == nil || u.User.Username() != "workers_v2" || u.Path != "/workers_v2_multiply" || (u.RawQuery != "" && u.RawQuery != "sslmode=disable") || u.Fragment != "" {
		return fmt.Errorf("requires disposable workers_v2_multiply fixture URL")
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return fmt.Errorf("fixture host must be loopback")
	}
	if strings.Contains(dsn, "@/") {
		return fmt.Errorf("fixture socket is not permitted")
	}
	return nil
}

func TestRecoveryLeaseSurvivesPolicyRemovalAndWithdrawalDebounce(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	settings, vault := seedPolicySet(t, store)
	topology, err := DeriveEarnMaxTopology(settings, 320)
	if err != nil {
		t.Fatal(err)
	}
	routeKey := routeKeyFor(settings, EarnMaxVaultIndex)
	t.Cleanup(func() {
		store.Pool().Exec(ctx, "DELETE FROM loyal_yield.multiply_route_states WHERE route_key=$1", routeKey)
	})
	state, err := NewRouteState(routeKey, settings.String(), EarnMaxVaultIndex, vault, 320, TokenBalance{Account: topology.ClaimCustody.String(), Mint: USDCMint, TokenProgram: TokenProgram, AmountRaw: 1000}, 331895401, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if created, err := store.CreateRouteState(ctx, state); err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	lease, err := store.LeaseRoute(ctx, routeKey, "legacy-rust-owner", time.Now().Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("legacy lease: %v", err)
	}
	stored, err := store.LoadRouteState(ctx, routeKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stored.State.RequestWithdrawal("pending-request", fixtureKey(71).String(), 1000, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.SaveRouteState(ctx, lease, stored.State); err != nil || !ok {
		t.Fatalf("request withdrawal: %v %v", ok, err)
	}
	stored, err = store.LoadRouteState(ctx, routeKey)
	if err != nil {
		t.Fatal(err)
	}
	op := integrationOperation(routeKey, stored.State.Cycle, lease.Version+1)
	route := stored.State
	id := op.OperationID
	route.Generation = uint64(lease.Version + 1)
	route.CurrentOperationID = &id
	if ok, err := store.PrepareOperation(ctx, lease, route, op); err != nil || !ok {
		t.Fatalf("prepare: %v %v", ok, err)
	}
	signed := signedWireFixture(t, false)
	mh, err := MessageSHA256(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.PersistSignedOperation(ctx, lease, id, fixtureKey(34).String(), PolicyDataHash([]byte("policy")), mh, signed); err != nil || !ok {
		t.Fatalf("persist: %v %v", ok, err)
	}
	if ok, err := store.MarkBroadcastIntent(ctx, lease, id, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("intent: %v %v", ok, err)
	}
	if _, err := store.Pool().Exec(ctx, "UPDATE loyal_yield.earn_max_policy_sets SET status='removed' WHERE settings=$1", settings.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReleaseLease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	adopted, err := store.LeaseNextRoute(ctx, "go-recovery-owner", time.Now().Add(time.Minute))
	if err != nil || adopted == nil || adopted.RouteKey != routeKey {
		t.Fatalf("signed legacy intent excluded by admission gates: %v %v", adopted, err)
	}
	if adopted.FencingToken <= lease.FencingToken {
		t.Fatal("adoption did not fence the previous owner")
	}
	if ok, err := store.MarkConfirmed(ctx, lease, id, 331895500); err != nil || ok {
		t.Fatalf("old owner advanced adopted operation: %v %v", ok, err)
	}
}
