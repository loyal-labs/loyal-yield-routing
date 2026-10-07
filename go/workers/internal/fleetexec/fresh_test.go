package fleetexec

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	sdk "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"strings"
	"testing"
	"time"
)

func seedFresh(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (fleet.ExecutionAdmission, DelegateSigner) {
	t.Helper()
	suffix := fmt.Sprint(time.Now().UnixNano())
	b := seedBaseline(t, ctx, pool, suffix)
	wire, key := integrationWire(t, 112)
	input := fixturePersistInput(t, ctx, pool, b, wire)
	var tableID, familyID int64
	var tableAddress string
	if err := pool.QueryRow(ctx, `SELECT t.id,t.family_id,t.table_address FROM loyal_yield.lookup_table_usage_leases u JOIN loyal_yield.route_lookup_tables t ON t.id=u.route_lookup_table_id WHERE u.reference_key=$1`, b.SemanticKey).Scan(&tableID, &familyID, &tableAddress); err != nil {
		t.Fatal(err)
	}
	// The synthetic message only proves durable admission/signing plumbing. The
	// official mature policy wrapper and ABI fixtures remain C's responsibility.
	tableKey := sdk.PublicKey(sha256.Sum256([]byte(suffix + "table")))
	tableAddress = tableKey.String()
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET table_address=$2,address_count=1,usable_address_count=1,reserved_address_count=1 WHERE id=$1`, tableID, tableAddress); err != nil {
		t.Fatal(err)
	}
	member := sdk.PublicKeyFromBytes(rotateSeed(113, 1)).String()
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_addresses(route_lookup_table_id,address,ordinal,added_slot,usable_after_slot,last_verified_slot,last_verified_at) VALUES($1,$2,0,990,991,1000,clock_timestamp())`, tableID, member); err != nil {
		t.Fatal(err)
	}
	tx, err := sdk.TransactionFromBytes(wire.SignedTransaction)
	if err != nil {
		t.Fatal(err)
	}
	tx.Signatures[0] = sdk.Signature{}
	tx.Message.AddressTableLookups = sdk.MessageAddressTableLookupSlice{{AccountKey: tableKey, WritableIndexes: []uint8{0}}}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	unsigned, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	mh, wh := sha256.Sum256(message), sha256.Sum256(unsigned)
	plan := json.RawMessage(`{"kind":"same_mint","route_kind":"same_mint","source_kind":"reserve_position","route_amount_semantics":"redeemable_source_liquidity","source_amount_semantics":"kamino_obligation_collateral_deposited_amount"}`)
	routeFP, reqFP := strings.Repeat("a", 64), strings.Repeat("b", 64)
	var snapshot, vault, capacity int64
	var expires time.Time
	if err := pool.QueryRow(ctx, `SELECT source_snapshot_id,vault_id FROM loyal_yield.rebalance_opportunities WHERE id=$1`, b.OpportunityID).Scan(&snapshot, &vault); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET status='skipped' WHERE id=$1`, b.DecisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.target_capacity_reservations SET decision_id=NULL WHERE opportunity_id=$1`, b.OpportunityID); err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='leased',decision_id=NULL,lease_kind='execute',lease_owner='fresh-owner',lease_expires_at=clock_timestamp()+interval '2 minutes',fencing_token=1,route_fingerprint=$2,requirements_fingerprint=$3,source_reserve='source-fresh',source_liquidity_mint=liquidity_mint,target_liquidity_mint=liquidity_mint,execution_plan=$4 WHERE id=$1 RETURNING lease_expires_at`, b.OpportunityID, routeFP, reqFP, plan).Scan(&expires)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, b.OpportunityID).Scan(&capacity); err != nil {
		t.Fatal(err)
	}
	a := fleet.ExecutionAdmission{
		Lease:                fleet.RevalidationLease{OpportunityID: b.OpportunityID, OptimizerEpochID: b.EpochID, SourceSnapshotID: &snapshot, IdempotencyKey: "opportunity:" + suffix, Owner: "fresh-owner", FencingToken: 1, ExpiresAt: expires, Cluster: b.Cluster, VaultID: vault, VaultPubkey: "vault:" + suffix, SourceReserve: "source-fresh", TargetReserve: b.Reserve, LiquidityMint: b.Mint, RouteKind: "same_mint", LiquidityAmountRaw: 1000000, PrincipalUSDMicros: 1000000, FeeCapLamports: 10000, OptimizerEpochKey: "epoch:" + suffix, ExecutionPlan: plan},
		Preparation:          fleet.RoutePreparation{RouteFingerprint: routeFP, RequirementsFingerprint: reqFP, ExecutionPlan: plan, Transaction: fleet.PreparedTransaction{Message: message, UnsignedWire: unsigned, MessageSHA256: hex.EncodeToString(mh[:]), WireSHA256: hex.EncodeToString(wh[:]), PacketBytes: len(unsigned), LookupTables: []string{tableAddress}, WritableAccounts: []string{input.FeePayer, member}, FeeLamports: 5000, ComputeLimit: 200000}, Simulation: fleet.SimulationEvidence{Succeeded: true, Slot: 1000, WireSHA256: hex.EncodeToString(wh[:]), UnitsConsumed: 10000}},
		LastValidBlockHeight: 4000, CapacityReservationID: capacity, ReservationGeneration: 1, CapacityFencingToken: 1, SelectedALTs: []fleet.ExecutionALT{{TableID: tableID, FamilyID: familyID, MutationEpoch: 0, Generation: 1, Address: tableAddress, Addresses: []string{member}}}, AltSelectionFingerprint: strings.Repeat("c", 64), ConflictKeys: []string{"vault-write:" + "vault:" + suffix, fmt.Sprintf("fleet-shared-write-lane:%02d", vault%64)}, Evidence: fleet.FreshRouteEvidence{Slot: 1000, ObservedAt: time.Now()},
	}
	identityJSON, _ := json.Marshal(a.SelectedALTs)
	selectionHash := sha256.Sum256(identityJSON)
	a.AltSelectionFingerprint = hex.EncodeToString(selectionHash[:])
	a.Anchors = fleet.ExecutionBalanceAnchors{Owner: a.Lease.VaultPubkey, Mint: a.Lease.LiquidityMint, SourceReserve: a.Lease.SourceReserve, TargetReserve: a.Lease.TargetReserve, SourceMarket: "source-market", TargetMarket: "target-market", SourceCollateralMint: "source-collateral-mint", TargetCollateralMint: "target-collateral-mint", LiquidityTokenProgram: sdk.TokenProgramID.String(), SourceObligation: "source-obligation", TargetObligation: "target-obligation", VaultLiquidityATA: "ata", SourceCollateralRaw: 100, MinimumSlot: 1000}
	a.Evidence.Anchors = a.Anchors
	a.Evidence.OpportunityID = a.Lease.OpportunityID
	a.Evidence.OpportunityKey = a.Lease.IdempotencyKey
	a.Evidence.EpochID = a.Lease.OptimizerEpochID
	a.Evidence.EpochFingerprint = a.Lease.OptimizerEpochKey
	for _, conflict := range a.ConflictKeys {
		if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.route_account_conflict_leases(cluster,writable_account_key,opportunity_id,lease_owner,fencing_token,expires_at) VALUES($1,$2,$3,$4,1,clock_timestamp()+interval '2 minutes')`, a.Lease.Cluster, conflict, a.Lease.OpportunityID, a.Lease.Owner); err != nil {
			t.Fatal(err)
		}
	}
	return a, DelegateSigner{FeePayer: ed25519.PrivateKey(key)}
}

func TestFreshPublicationBindsRegisteredHandoffAndFirstSend(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	a, signer := seedFresh(t, ctx, pool)
	worker, err := NewWorker(Config{Cluster: a.Lease.Cluster, Owner: a.Lease.Owner, LeaseTTL: time.Minute, BatchSize: 1, TickInterval: time.Second, Facts: testFacts()}, store, &countingChain{}, &fakeStatus{}, signer)
	if err != nil {
		t.Fatal(err)
	}
	id, err := worker.ExecuteFresh(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	var decision, capacitySubmission int64
	var opState string
	if err := pool.QueryRow(ctx, `SELECT s.decision_id,o.opportunity_state,r.signed_submission_id FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_opportunities o ON o.id=s.opportunity_id JOIN loyal_yield.target_capacity_reservations r ON r.opportunity_id=o.id WHERE s.id=$1`, id).Scan(&decision, &opState, &capacitySubmission); err != nil {
		t.Fatal(err)
	}
	if decision <= 0 || capacitySubmission != id || opState != "decision_created" {
		t.Fatalf("handoff not bound: %d %d %s", decision, capacitySubmission, opState)
	}
	lease := claimOne(t, ctx, store, a.Lease.Cluster, "fresh-owner")
	if err := store.RecordBroadcastIntent(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBroadcastIntent(ctx, lease); err != nil {
		t.Fatal("a resend of the same bytes must be countable under the same fence:", err)
	}
	var status, signature string
	if err := pool.QueryRow(ctx, `SELECT status::text,signature FROM loyal_yield.rebalance_decisions WHERE id=$1`, decision).Scan(&status, &signature); err != nil {
		t.Fatal(err)
	}
	if status != "confirming" || signature != lease.Submission.Signature {
		t.Fatalf("decision not bound to intent: %s %s", status, signature)
	}
}

func TestFreshPublicationRejectsChangedCustodyFencesWithoutSignedRows(t *testing.T) {
	for _, change := range []string{"capacity", "alt_epoch", "alt_membership", "conflict", "snapshot", "lease"} {
		t.Run(change, func(t *testing.T) {
			store, pool := integrationStore(t)
			ctx := context.Background()
			a, signer := seedFresh(t, ctx, pool)
			switch change {
			case "capacity":
				_, _ = pool.Exec(ctx, `UPDATE loyal_yield.target_capacity_reservations SET reservation_fencing_token=2 WHERE id=$1`, a.CapacityReservationID)
			case "alt_epoch":
				_, _ = pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET mutation_epoch=1 WHERE id=$1`, a.SelectedALTs[0].TableID)
			case "alt_membership":
				_, _ = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_addresses SET address=$2 WHERE route_lookup_table_id=$1`, a.SelectedALTs[0].TableID, sdk.PublicKeyFromBytes(rotateSeed(1, 2)).String())
			case "conflict":
				_, _ = pool.Exec(ctx, `DELETE FROM loyal_yield.route_account_conflict_leases WHERE cluster=$1 AND writable_account_key=$2`, a.Lease.Cluster, a.ConflictKeys[0])
			case "snapshot":
				a.Lease.SourceSnapshotID = nil
			case "lease":
				_, _ = pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET fencing_token=2 WHERE id=$1`, a.Lease.OpportunityID)
			}
			wire, err := signer.SignPreparedRoute(a.Preparation.Transaction, a.LastValidBlockHeight)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.PersistFreshAdmission(ctx, a, wire); err == nil {
				t.Fatal("changed custody fence published signed wire")
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE opportunity_id=$1`, a.Lease.OpportunityID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed handoff left signed row: %d %v", count, err)
			}
		})
	}
}

func TestFirstSendRejectsExpiredOrMutatedALTWithoutIntent(t *testing.T) {
	for _, change := range []string{"expired_usage", "mutation"} {
		t.Run(change, func(t *testing.T) {
			store, pool := integrationStore(t)
			ctx := context.Background()
			a, signer := seedFresh(t, ctx, pool)
			wire, err := signer.SignPreparedRoute(a.Preparation.Transaction, a.LastValidBlockHeight)
			if err != nil {
				t.Fatal(err)
			}
			id, err := store.PersistFreshAdmission(ctx, a, wire)
			if err != nil {
				t.Fatal(err)
			}
			lease := claimOne(t, ctx, store, a.Lease.Cluster, "first-send")
			if change == "expired_usage" {
				if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_usage_leases SET created_at=clock_timestamp()-interval '10 minutes',expires_at=clock_timestamp()-interval '1 second' WHERE reference_key=$1`, lease.Submission.SemanticKey); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET mutation_epoch=mutation_epoch+1 WHERE id=$1`, a.SelectedALTs[0].TableID); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.RecordBroadcastIntent(ctx, lease); err == nil {
				t.Fatal("first send ignored changed ALT custody")
			}
			state, count, _, _, _ := durableRow(t, ctx, pool, id)
			if state != "signed" || count != 0 {
				t.Fatalf("rejected ALT crossed send boundary: %s %d", state, count)
			}
		})
	}
}
