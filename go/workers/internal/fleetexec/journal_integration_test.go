package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	urlpkg "net/url"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/mr-tron/base58"
)

// The integration suite runs only against the explicitly provisioned
// disposable fleet-execution fixture database (CI: role workers_v2 on
// 127.0.0.1, database workers_v2_fleetexec). It never creates, drops, or
// renames a database and never applies a surrogate schema: the registered
// loyal-yield-store migrations are the only DDL source.
const (
	testEnvURL     = "FLEET_EXEC_TEST_DATABASE_URL"
	testEnvURLAlt  = "FLEETEXEC_TEST_DATABASE_URL"
	disposableName = "workers_v2_fleetexec"
)

func integrationURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv(testEnvURL)
	if url == "" {
		url = os.Getenv(testEnvURLAlt)
	}
	if url == "" {
		t.Skipf("%s is not set", testEnvURL)
	}
	parsed, err := urlpkg.Parse(url)
	if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" ||
		parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" && parsed.Hostname() != "::1" ||
		parsed.User == nil || parsed.User.Username() != "workers_v2" || parsed.Path != "/workers_v2_fleetexec" || (parsed.RawQuery != "" && parsed.RawQuery != "sslmode=disable") || parsed.Fragment != "" {
		t.Fatal("refusing database URL outside the dedicated loopback workers_v2_fleetexec fixture")
	}
	return url
}

// requireDisposableDatabase refuses to run against anything but the
// registered disposable fixture database, whatever the URL claims.
func requireDisposableDatabase(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var name, role string
	if err := pool.QueryRow(ctx, "SELECT current_database(),current_user").Scan(&name, &role); err != nil {
		t.Fatal(err)
	}
	if name != disposableName || role != "workers_v2" {
		t.Fatalf("refusing to integrate against %q: only the %q disposable fixture is permitted", name, disposableName)
	}
}

func integrationStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, integrationURL(t))
	if err != nil {
		t.Fatalf("configured fixture database unavailable: %v", err)
	}
	requireDisposableDatabase(t, pool)
	store := NewStore(pool)
	t.Cleanup(pool.Close)
	return store, pool
}

// seededBaseline is the actual registered baseline relationship chain one
// signed submission hangs from: policy → vault → snapshot → optimizer epoch
// → opportunity → decision → capacity frontier + reservation.
type seededBaseline struct {
	Cluster       string
	SemanticKey   string
	OpportunityID int64
	DecisionID    int64
	EpochID       int64
	FeePayer      ed25519.PrivateKey
	Frontier      string
	Reserve       string
	Mint          string
}

func integrationWire(t *testing.T, seed byte) (WireIdentity, ed25519.PrivateKey) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(func() []byte {
		seedBytes := make([]byte, 32)
		for i := range seedBytes {
			seedBytes[i] = seed + byte(i)
		}
		return seedBytes
	}())
	message := []byte{0x80, 1, 0, 1, 2}
	message = append(message, key[32:]...)
	message = append(message, rotateSeed(seed, 11)...)
	blockhashBytes := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", seed, time.Now().UnixNano())))
	message = append(message, blockhashBytes[:]...)
	message = append(message, 1, 1, 1, 0, 1, 0xAA, 0)
	signature := ed25519.Sign(key, message)
	signed := append([]byte{0x01}, signature...)
	signed = append(signed, message...)
	signedHash := sha256.Sum256(signed)
	messageHash := sha256.Sum256(message)
	blockhash := base58.Encode(blockhashBytes[:])
	return WireIdentity{
		SignedTransaction:     signed,
		SignedTransactionHash: hex.EncodeToString(signedHash[:]),
		MessageHash:           hex.EncodeToString(messageHash[:]),
		TransactionSignature:  base58.Encode(signature),
		RecentBlockhash:       blockhash,
		LastValidBlockHeight:  4_000,
	}, key
}

func rotateSeed(seed byte, offset byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = seed + byte(i) + offset
	}
	return out
}

func seedBaseline(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix string, clusters ...string) seededBaseline {
	t.Helper()
	var policyID, vaultID, snapshotID, epochID, opportunityID, decisionID, reservationID int64
	err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,last_seen_slot,last_seen_signature) VALUES($1,$2,1,$3,0,$4,ARRAY[$2]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY['usdc']::text[],ARRAY[$6]::text[],ARRAY['usdc']::text[],'[]',true,999,$5) RETURNING id`,
		"settings:"+suffix, "authority:"+suffix, "policy:"+suffix, "vault:"+suffix, "signature:"+suffix, "market:"+suffix).Scan(&policyID)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,0,$2,$3,true) RETURNING id`, "settings:"+suffix, "vault:"+suffix, policyID).Scan(&vaultID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.vault_position_snapshots(vault_id,policy_id,observed_slot,observed_at,is_current,context) VALUES($1,$2,999,clock_timestamp(),true,'{}') RETURNING id`, vaultID, policyID).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}
	cluster := "cluster:" + suffix
	if len(clusters) != 0 {
		cluster = clusters[0]
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.optimizer_epochs(cluster,epoch_key,market_slot,observed_at,expires_at,market_state) VALUES($1,$2,1000,clock_timestamp(),clock_timestamp()+interval '1 hour','{}') RETURNING id`, cluster, "epoch:"+suffix).Scan(&epochID); err != nil {
		t.Fatal(err)
	}
	reserve, mint := "reserve:"+suffix, "usdc:"+suffix
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.rebalance_decisions(vault_id,decision_reason,idempotency_key,status) VALUES($1,'target_supply_apy_exceeds_source',$2,'ready') RETURNING id`, vaultID, "decision:"+suffix).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.rebalance_opportunities(cluster,idempotency_key,vault_id,source_snapshot_id,optimizer_epoch_id,target_reserve,liquidity_mint,amount_raw,principal_usd_micros,source_apy_bps,target_apy_bps,estimated_edge_bps,annual_yield_gain_usd_micros,expected_net_gain_usd_micros,economic_priority,scheduler_priority_anchor,priority_version,opportunity_state,decision_id,expires_at,execution_plan) VALUES($1,$2,$3,$4,$5,$6,$7,1000000,1000000,100,900,800,1000,900,1,1,'v1','decision_created',$8,clock_timestamp()+interval '1 hour','{"route_kind":"same_mint","source_kind":"reserve_position"}') RETURNING id`,
		cluster, "opportunity:"+suffix, vaultID, snapshotID, epochID, reserve, mint, decisionID).Scan(&opportunityID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.target_capacity_frontiers(cluster,target_reserve,liquidity_mint,observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros) VALUES($1,$2,$3,1000000000,500,900000000) ON CONFLICT (cluster,target_reserve,liquidity_mint) DO NOTHING`, cluster, reserve, mint); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.target_capacity_reservations(cluster,target_reserve,liquidity_mint,opportunity_id,decision_id,principal_usd_micros,admitted_observed_supply_usd_micros,admitted_observed_slot,admitted_maximum_inflight_usd_micros,admitted_telemetry_version,reservation_generation,admitted_observed_target_apy_bps,admitted_projected_target_apy_bps,admitted_source_apy_bps,admitted_edge_bps,admitted_net_holding_gain_usd_micros,admitted_fee_cap_lamports,reservation_fencing_token) VALUES($1,$2,$3,$4,$5,1000000,1000000000,500,900000000,1,1,900,800,100,700,100,1000,1) RETURNING id`,
		cluster, reserve, mint, opportunityID, decisionID).Scan(&reservationID); err != nil {
		t.Fatal(err)
	}
	_ = reservationID
	return seededBaseline{
		Cluster: cluster, SemanticKey: "semantic:" + suffix,
		OpportunityID: opportunityID, DecisionID: decisionID, EpochID: epochID,
		Frontier: reserve, Reserve: reserve, Mint: mint,
	}
}

func fixturePersistInput(t *testing.T, ctx context.Context, pool *pgxpool.Pool, baseline seededBaseline, wire WireIdentity) PersistRouteInput {
	t.Helper()
	decoded, err := sdk.TransactionFromBytes(wire.SignedTransaction)
	if err != nil {
		t.Fatal(err)
	}
	payer := decoded.Message.AccountKeys[0].String()
	var vaultID, policyID int64
	if err := pool.QueryRow(ctx, `SELECT v.id,v.active_policy_id FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.managed_vaults v ON v.id=o.vault_id WHERE o.id=$1`, baseline.OpportunityID).Scan(&vaultID, &policyID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET delegated_signers=ARRAY[$2]::text[] WHERE id=$1`, policyID, payer); err != nil {
		t.Fatal(err)
	}
	var familyID, tableID int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_families(cluster,logical_name,kind,planner_version,catalog_version,active_generation,provisioning_authority,payer,largest_atomic_expansion,safety_margin,allocation_high_water) VALUES($1,$2,'shared_market','v1','v1',1,$3,$3,16,16,224) ON CONFLICT (cluster,kind) WHERE desired_state='active' DO UPDATE SET updated_at=clock_timestamp() RETURNING id`, baseline.Cluster, baseline.SemanticKey, payer).Scan(&familyID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_lookup_tables(cluster,scope,table_address,authority,payer,status,family_id,allocation_kind,generation,shard_ordinal,desired_state,accepting_allocations,allocation_high_water,reserved_address_count,usable_address_count,mutation_epoch) VALUES($1,$2,$3,$4,$4,'active',$5,'shared_market',1,0,'active',true,224,0,0,0) ON CONFLICT (family_id,generation,shard_ordinal) WHERE family_id IS NOT NULL DO UPDATE SET updated_at=clock_timestamp() RETURNING id`, baseline.Cluster, baseline.SemanticKey, "table:"+baseline.SemanticKey, payer, familyID).Scan(&tableID); err != nil {
		t.Fatal(err)
	}
	epochs, _ := json.Marshal(map[string]any{"tables": []any{map[string]any{"tableId": tableID, "mutationEpoch": 0}}})
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_usage_leases(cluster,lease_kind,reference_key,route_lookup_table_id,vault_id,requirements_fingerprint,expires_at) VALUES($1,'prepared_transaction',$2,$3,$4,$5,clock_timestamp()+interval '1 hour')`, baseline.Cluster, baseline.SemanticKey, tableID, vaultID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	effect := []byte(`{"route":"same_mint"}`)
	anchors := []byte(`{"account_addresses":["payer","source","target"],"anchors":[{"account":"source","mint":"usdc","expected_delta":1000000,"decimals":6},{"account":"target","mint":"usdc","expected_delta":-1000000,"decimals":6}]}`)
	return PersistRouteInput{
		Cluster: baseline.Cluster, SemanticKey: baseline.SemanticKey,
		OpportunityID: baseline.OpportunityID, DecisionID: &baseline.DecisionID,
		Wire: wire, FeePayer: payer,
		CompiledFeeLamports: 5_000,
		WritableAccountKeys: []string{payer, "source", "target"},
		ConflictAccountKeys: []string{"vault-exclusive:" + fmt.Sprint(baseline.OpportunityID), "shared-lane:" + fmt.Sprint(baseline.OpportunityID)},
		ExecutorOwner:       "owner-a", ExecutorFencingToken: 1,
		MovementLeg: LegRoute, LegPurpose: PurposeOptimizeYield, LegGeneration: 1,
		OptimizerEpochID:           baseline.EpochID,
		AltRequirementsFingerprint: strings.Repeat("a", 64),
		AltSelectionFingerprint:    strings.Repeat("b", 64),
		AltMutationEpochs:          epochs,
		ExpectedEffect:             effect, ExpectedBalanceAnchors: anchors,
		ConflictLeaseExpiration: time.Now().Add(time.Hour),
	}
}

func claimOne(t *testing.T, ctx context.Context, store *Store, cluster, owner string) SubmissionLease {
	t.Helper()
	leases, err := store.ClaimRecoveryWork(ctx, cluster, owner, time.Minute, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range leases {
		if lease.Submission.SemanticKey != "" {
			return lease
		}
	}
	t.Fatalf("no claimable submission for %s", cluster)
	return SubmissionLease{}
}

func durableRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) (string, int, *string, *int64, *int64) {
	t.Helper()
	var state string
	var count int
	var owner *string
	var expiryHeight *int64
	var effectSlot *int64
	if err := pool.QueryRow(ctx, `SELECT submission_state, broadcast_count, confirmation_lease_owner, expiry_observed_block_height, effect_check_slot FROM loyal_yield.signed_route_submissions WHERE id=$1`, id).
		Scan(&state, &count, &owner, &expiryHeight, &effectSlot); err != nil {
		t.Fatal(err)
	}
	return state, count, owner, expiryHeight, effectSlot
}

func TestPersistSignedRouteWireIsImmutableAndContendedRefused(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	baseline := seedBaseline(t, ctx, pool, suffix)
	wire, _ := integrationWire(t, 5)
	input := fixturePersistInput(t, ctx, pool, baseline, wire)
	id, reused, err := store.PersistSignedRoute(ctx, input)
	if err != nil || reused {
		t.Fatalf("persist: id=%d reused=%v err=%v", id, reused, err)
	}

	// Identical bytes are the same durable intent: reused, never duplicated.
	idAgain, reusedAgain, err := store.PersistSignedRoute(ctx, input)
	if err != nil || !reusedAgain || idAgain != id {
		t.Fatalf("identical wire must reuse submission %d: id=%d reused=%v err=%v", id, idAgain, reusedAgain, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE opportunity_id=$1`, baseline.OpportunityID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("identical wire created %d rows", rows)
	}

	// Different bytes for the same opportunity are a second spend: refused,
	// and the persisted wire is untouched.
	tampered := input
	tampered.Wire.TransactionSignature = base58.Encode(rotateSeed(9, 1))
	tampered.Wire.SignedTransaction = append([]byte{0x01}, []byte("different-bytes-at-least-some-length")...)
	tamperedHash := sha256.Sum256(tampered.Wire.SignedTransaction)
	tampered.Wire.SignedTransactionHash = hex.EncodeToString(tamperedHash[:])
	tampered.Wire.MessageHash = hex.EncodeToString(tamperedHash[:])
	tampered.SemanticKey = baseline.SemanticKey + ":other"
	if _, _, err := store.PersistSignedRoute(ctx, tampered); !errors.Is(err, ErrRouteContended) {
		t.Fatalf("second spend must be refused, got %v", err)
	}
	var storedWire []byte
	if err := pool.QueryRow(ctx, `SELECT signed_transaction FROM loyal_yield.signed_route_submissions WHERE id=$1`, id).Scan(&storedWire); err != nil {
		t.Fatal(err)
	}
	if string(storedWire) != string(wire.SignedTransaction) {
		t.Fatalf("persisted wire was mutated")
	}
}

func TestPersistSignedRouteConflictLeaseContention(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	first := seedBaseline(t, ctx, pool, suffix)
	wire, _ := integrationWire(t, 6)
	if _, _, err := store.PersistSignedRoute(ctx, fixturePersistInput(t, ctx, pool, first, wire)); err != nil {
		t.Fatal(err)
	}

	// A second opportunity contending the same writable account keys is
	// blocked while the first lease is live: custody is never replaced
	// merely because the opportunity differs.
	second := seedBaseline(t, ctx, pool, suffix+"-b", first.Cluster)
	wire2, _ := integrationWire(t, 6)
	decoded, _ := sdk.TransactionFromBytes(wire2.SignedTransaction)
	uniqueHash := sha256.Sum256([]byte(suffix + "-second"))
	decoded.Message.RecentBlockhash = sdk.Hash(uniqueHash)
	msg, _ := decoded.Message.MarshalBinary()
	key := ed25519.NewKeyFromSeed(rotateSeed(6, 0))
	sig := ed25519.Sign(key, msg)
	copy(decoded.Signatures[0][:], sig)
	wire2.SignedTransaction, _ = decoded.MarshalBinary()
	wh := sha256.Sum256(wire2.SignedTransaction)
	mh := sha256.Sum256(msg)
	wire2.SignedTransactionHash = hex.EncodeToString(wh[:])
	wire2.MessageHash = hex.EncodeToString(mh[:])
	wire2.RecentBlockhash = decoded.Message.RecentBlockhash.String()
	wire2.TransactionSignature = decoded.Signatures[0].String()
	input := fixturePersistInput(t, ctx, pool, second, wire2)
	input.ConflictAccountKeys = []string{"vault-exclusive:" + fmt.Sprint(first.OpportunityID), "shared-lane:" + fmt.Sprint(first.OpportunityID)}
	if _, _, err := store.PersistSignedRoute(ctx, input); !errors.Is(err, ErrConflictLeaseHeld) {
		t.Fatalf("live conflicting lease must block admission, got %v", err)
	}
	var leaseSubmission *int64
	if err := pool.QueryRow(ctx, `SELECT submission_id FROM loyal_yield.route_account_conflict_leases WHERE writable_account_key=$1`, "vault-exclusive:"+fmt.Sprint(first.OpportunityID)).Scan(&leaseSubmission); err != nil {
		t.Fatal(err)
	}
	if leaseSubmission == nil || *leaseSubmission != firstOwnedSubmission(t, ctx, pool, first.OpportunityID) {
		t.Fatalf("conflict lease custody was replaced: %v", leaseSubmission)
	}

	// Expiring the account lease cannot release a still-live signed intent.
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_account_conflict_leases SET created_at=clock_timestamp()-interval '1 hour',expires_at=clock_timestamp()-interval '1 second' WHERE submission_id=$1`, *leaseSubmission); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PersistSignedRoute(ctx, input); !errors.Is(err, ErrConflictLeaseHeld) {
		t.Fatalf("expired conflict belonging to live signed custody was replaced: %v", err)
	}

	// Terminal prior custody releases the account for the next submission.
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state='expired', error_detail='blockhash_expired_at_height_5000' WHERE opportunity_id=$1`, first.OpportunityID); err != nil {
		t.Fatal(err)
	}
	input.SemanticKey = second.SemanticKey
	if _, reused, err := store.PersistSignedRoute(ctx, input); err != nil || reused {
		t.Fatalf("terminal custody must release the account: reused=%v err=%v", reused, err)
	}
}

func firstOwnedSubmission(t *testing.T, ctx context.Context, pool *pgxpool.Pool, opportunityID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM loyal_yield.signed_route_submissions WHERE opportunity_id=$1 ORDER BY id LIMIT 1`, opportunityID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRestartAfterDurableIntentResendsSameBytesUntilConfirmed(t *testing.T) {
	// The Go executor used to send once and treat any later send as a double
	// spend, so a dropped forward sat until its blockhash expired. Resending
	// the identical signed bytes cannot spend twice; the Rust confirmer did it.
	store, pool := integrationStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	baseline := seedBaseline(t, ctx, pool, suffix)
	wire, _ := integrationWire(t, 7)
	id, _, err := store.PersistSignedRoute(ctx, fixturePersistInput(t, ctx, pool, baseline, wire))
	if err != nil {
		t.Fatal(err)
	}

	// First owner records the durable broadcast intent, then the process dies
	// before the send (the lease is abandoned, not released).
	lease := claimOne(t, ctx, store, baseline.Cluster, "owner-crash")
	if lease.Submission.ID != id {
		t.Fatalf("claimed submission %d, want %d", lease.Submission.ID, id)
	}
	if err := store.RecordBroadcastIntent(ctx, lease); err != nil {
		t.Fatal(err)
	}
	state, count, _, _, _ := durableRow(t, ctx, pool, id)
	if state != string(StateSubmitted) || count != 1 {
		t.Fatalf("durable intent state = %s count = %d, want submitted/1", state, count)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET confirmation_lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}

	// The restarted family claims the row and lands it: the first resend is
	// dropped, the second confirms.
	chain := &countingChain{height: 3_990, landOn: 2}
	worker, err := NewWorker(Config{Cluster: baseline.Cluster, Owner: "owner-recovery", LeaseTTL: time.Minute, BatchSize: 8, TickInterval: time.Second, Facts: testFacts()}, store, chain, &fakeStatus{}, DelegateSigner{})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker.wg.Wait()
	if len(chain.wires) != 2 {
		t.Fatalf("sent %d times, want 2", len(chain.wires))
	}
	for _, sent := range chain.wires {
		if !bytes.Equal(sent, wire.SignedTransaction) {
			t.Fatal("landing rebuilt or changed the signed bytes")
		}
	}
	state, count, owner, _, _ := durableRow(t, ctx, pool, id)
	if state != string(StateReconciliationPending) || count != 3 || owner != nil {
		t.Fatalf("landed row state = %s count = %d owner = %v", state, count, owner)
	}
}

func TestExpiredRouteIsTerminalAndReleasesItsReservations(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	baseline := seedBaseline(t, ctx, pool, suffix)
	wire, _ := integrationWire(t, 12)
	id, _, err := store.PersistSignedRoute(ctx, fixturePersistInput(t, ctx, pool, baseline, wire))
	if err != nil {
		t.Fatal(err)
	}
	chain := &countingChain{height: 3_999, heightStep: 1}
	worker, err := NewWorker(Config{Cluster: baseline.Cluster, Owner: "owner-expiry", LeaseTTL: time.Minute, BatchSize: 8, TickInterval: time.Second, Facts: testFacts()}, store, chain, &fakeStatus{}, DelegateSigner{})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker.wg.Wait()
	state, count, _, height, effectSlot := durableRow(t, ctx, pool, id)
	// Rust writes no effect-check slot for an expiry it proved by signature
	// absence: that column is the floor of an account readback.
	if state != string(StateExpired) || count != 1 || height == nil || *height != 4_001 || effectSlot != nil {
		t.Fatalf("expired row state = %s count = %d height = %v slot = %v", state, count, height, effectSlot)
	}
	var reservation, opportunity, decision string
	var conflicts int
	if err := pool.QueryRow(ctx, `SELECT r.reservation_state,o.opportunity_state,d.status::text,(SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE submission_id=$2)
 FROM loyal_yield.target_capacity_reservations r JOIN loyal_yield.rebalance_opportunities o ON o.id=r.opportunity_id JOIN loyal_yield.rebalance_decisions d ON d.id=o.decision_id WHERE r.opportunity_id=$1`, baseline.OpportunityID, id).Scan(&reservation, &opportunity, &decision, &conflicts); err != nil {
		t.Fatal(err)
	}
	if reservation != "released" || opportunity != "failed" || decision != "failed" || conflicts != 0 {
		t.Fatalf("expiry kept work alive: reservation=%s opportunity=%s decision=%s conflicts=%d", reservation, opportunity, decision, conflicts)
	}
}

// countingChain is a cluster that drops forwards until send number landOn.
type countingChain struct {
	wires      [][]byte
	height     uint64
	heightStep uint64
	landOn     int
}

func (c *countingChain) SendWire(_ context.Context, wire []byte, _ bool) error {
	c.wires = append(c.wires, append([]byte(nil), wire...))
	return nil
}

func (c *countingChain) FinalizedBlockHeight(context.Context) (uint64, uint64, error) {
	c.height += c.heightStep
	return c.height, 1, nil
}

func (c *countingChain) SignatureState(context.Context, string) (chain.SignatureState, error) {
	if c.landOn > 0 && len(c.wires) >= c.landOn {
		return chain.SignatureState{Found: true, Slot: 700, Commitment: chain.Confirmed, ContextSlot: 700}, nil
	}
	return chain.SignatureState{ContextSlot: 650}, nil
}

type fakeStatus struct{}

func (f *fakeStatus) SignatureStatus(ctx context.Context, signature string) (SignatureStatus, error) {
	return SignatureStatus{}, nil
}

func (f *fakeStatus) FinalizedTransaction(ctx context.Context, signature string) (*TransactionReceipt, error) {
	return nil, nil
}

func TestStaleOwnerCannotMutate(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	baseline := seedBaseline(t, ctx, pool, suffix)
	wire, _ := integrationWire(t, 8)
	id, _, err := store.PersistSignedRoute(ctx, fixturePersistInput(t, ctx, pool, baseline, wire))
	if err != nil {
		t.Fatal(err)
	}
	lease := claimOne(t, ctx, store, baseline.Cluster, "owner-stale")

	// Another executor re-claims with a newer fencing token.
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET confirmation_lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	fresh := claimOne(t, ctx, store, baseline.Cluster, "owner-fresh")
	if fresh.FencingToken <= lease.FencingToken {
		t.Fatalf("fencing token did not advance: %d then %d", lease.FencingToken, fresh.FencingToken)
	}

	// The stale owner's advance is refused and changes nothing.
	err = store.AdvanceSubmission(ctx, lease, Advance{NextState: StateFailed, ErrorDetail: errPtr("stale")})
	if !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("stale advance = %v, want stale owner", err)
	}
	state, _, owner, _, _ := durableRow(t, ctx, pool, id)
	if state != string(StateSigned) || owner == nil || *owner != "owner-fresh" {
		t.Fatalf("stale owner mutated durable state: %s %v", state, owner)
	}
	// The current owner still advances under its own fence.
	if err := store.RecordBroadcastIntent(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBroadcastIntent(ctx, lease); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("stale owner counted a send: %v", err)
	}
}

func TestExpiryRequiresHeightBeyondBlockhashAndTelemetryIsStrictlyNewer(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	baseline := seedBaseline(t, ctx, pool, suffix)
	wire, _ := integrationWire(t, 9)
	input := fixturePersistInput(t, ctx, pool, baseline, wire)
	input.ConflictAccountKeys = []string{"vault-exclusive:" + suffix, "fleet-shared-write-lane:" + suffix}
	id, _, err := store.PersistSignedRoute(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	lease := claimOne(t, ctx, store, baseline.Cluster, "owner-expiry")
	// The signed bytes are still valid at their last valid height.
	if err := store.AdvanceSubmission(ctx, lease, Advance{
		NextState: StateExpired, ExpiryObservedBlockHeight: int64Ptr(4_000),
		ErrorDetail: errPtr("blockhash_expired_at_height_4000"),
	}); err == nil {
		t.Fatalf("expiry at the last valid height must be refused")
	}
	state, _, _, _, _ := durableRow(t, ctx, pool, id)
	if state != string(StateSigned) {
		t.Fatalf("refused expiry changed state: %s", state)
	}

	// A reconciled movement's capacity waits for strictly newer telemetry:
	// the slot-unit frontier observation gates the release, never a
	// height/slot mix.
	baseline2 := seedBaseline(t, ctx, pool, suffix+"-recon")
	wire2, _ := integrationWire(t, 10)
	input2 := fixturePersistInput(t, ctx, pool, baseline2, wire2)
	input2.ConflictAccountKeys = []string{"vault-exclusive:" + suffix + "-recon", "fleet-shared-write-lane:" + suffix + "-recon"}
	id2, _, err := store.PersistSignedRoute(ctx, input2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET status='confirmed',signature=$2,confirmed_slot=600,post_snapshot_id=source_snapshot_id WHERE id=$1`, baseline2.DecisionID, wire2.TransactionSignature); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state='reconciled', confirmed_slot=600, reconciled_slot=600, finalized_slot=600, finalized_at=clock_timestamp(),effect_credit_amount_raw=1000000,reconciled_effect='{"signature":"proof"}' WHERE id=$1`, id2); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.target_capacity_frontiers SET observed_slot=600 WHERE cluster=$1`, baseline2.Cluster); err != nil {
		t.Fatal(err)
	}
	released, err := store.ReleaseTelemetryReflectedCapacity(ctx, baseline2.Cluster)
	if err != nil {
		t.Fatal(err)
	}
	if released != 0 {
		t.Fatalf("equal-slot telemetry released capacity early: %d", released)
	}
	var movementReserved bool
	if err := pool.QueryRow(ctx, `SELECT reservation_state='awaiting_telemetry' FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, baseline2.OpportunityID).Scan(&movementReserved); err != nil {
		t.Fatal(err)
	}
	if !movementReserved {
		t.Fatalf("movement capacity must stay reserved until strictly newer telemetry")
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.target_capacity_frontiers SET observed_slot=601 WHERE cluster=$1`, baseline2.Cluster); err != nil {
		t.Fatal(err)
	}
	if released, err = store.ReleaseTelemetryReflectedCapacity(ctx, baseline2.Cluster); err != nil || released != 1 {
		t.Fatalf("strictly newer telemetry must release: released=%d err=%v", released, err)
	}
}

func TestReceiptMismatchRetainsCustody(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	baseline := seedBaseline(t, ctx, pool, fmt.Sprint(time.Now().UnixNano()))
	wire, _ := integrationWire(t, 11)
	input := fixturePersistInput(t, ctx, pool, baseline, wire)
	id, _, err := store.PersistSignedRoute(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state='reconciliation_pending',confirmed_slot=700 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	status := &finalizedStatus{receipt: &TransactionReceipt{Slot: 700, Signature: wire.TransactionSignature, MessageB64: base64.StdEncoding.EncodeToString([]byte("different-message")), accountAddresses: []string{"payer", "source", "target"}}}
	worker, err := NewWorker(Config{Cluster: baseline.Cluster, Owner: "owner-reconcile", LeaseTTL: time.Minute, BatchSize: 8, TickInterval: time.Second, Facts: testFacts()}, store, &countingChain{}, status, DelegateSigner{})
	if err != nil {
		t.Fatal(err)
	}
	lease := claimOne(t, ctx, store, baseline.Cluster, "owner-reconcile")
	if err := worker.reconcileFinalized(ctx, lease); err == nil || !strings.HasPrefix(err.Error(), "receipt identity") {
		t.Fatalf("receipt mismatch must fail reconciliation, got %v", err)
	}
	state, _, _, _, _ := durableRow(t, ctx, pool, id)
	if state != string(StateReconciliationPending) {
		t.Fatalf("mismatch released custody: %s", state)
	}
	var reservation string
	if err := pool.QueryRow(ctx, `SELECT reservation_state FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, baseline.OpportunityID).Scan(&reservation); err != nil {
		t.Fatal(err)
	}
	if reservation != "active" {
		t.Fatalf("mismatch released capacity: %s", reservation)
	}
}

type finalizedStatus struct {
	receipt *TransactionReceipt
}

func (f *finalizedStatus) SignatureStatus(ctx context.Context, signature string) (SignatureStatus, error) {
	return SignatureStatus{Found: true, Slot: 700, Confirmed: true, BlockHeight: 4_100}, nil
}

func (f *finalizedStatus) FinalizedTransaction(ctx context.Context, signature string) (*TransactionReceipt, error) {
	return f.receipt, nil
}

var _ = fleet.PreparedTransaction{}
var _ = pgx.ErrNoRows
