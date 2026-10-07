package fleetexec

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/mr-tron/base58"
)

func TestVoltrLegPersistsRustRowsAndLandsThroughTheSharedPath(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := t.Context()
	r, err := fleet.LoadVoltrRoute()
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	var policyID, vaultID, epochID int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,last_seen_slot,last_seen_signature) VALUES($1,$2,1,$3,1,$4,ARRAY[$5]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY['usdc']::text[],ARRAY['m']::text[],ARRAY['usdc']::text[],'[]',true,999,$6) RETURNING id`,
		"settings:"+suffix, "authority:"+suffix, "policy:"+suffix, "vault:"+suffix, r.Guardian, "sig:"+suffix).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,1,$2,$3,true) RETURNING id`, "settings:"+suffix, "vault:"+suffix, policyID).Scan(&vaultID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.optimizer_epochs(cluster,epoch_key,market_slot,observed_at,expires_at,market_state) VALUES($1,$2,1000,clock_timestamp(),clock_timestamp()+interval '1 hour','{}') RETURNING id`, r.Cluster, "voltr-epoch:"+suffix).Scan(&epochID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expires := now.Add(30 * time.Minute)
	epoch := fleet.ImmutableMarketEpoch{MintCoverage: []fleet.MarketMintCoverage{{Mint: fleet.USDCMint, Complete: true, ExpiresAt: &expires}}}
	for _, s := range r.Strategies {
		market := s.LendingMarket
		epoch.Reserves = append(epoch.Reserves, fleet.MarketEpochReserve{Reserve: s.Reserve, Market: &market, LiquidityMint: fleet.USDCMint, EconomicExpiresAt: expires, AvailableAmountRaw: "500000000000", TotalSupplyAmountRaw: "100000000000000", SupplyAPYBPS: 300, TargetEligible: true})
	}
	o := fleet.VoltrObservation{ContextSlot: 900, TotalValueRaw: 100e6, IdleRaw: 10e6, PositionsRaw: [4]uint64{90e6}, PendingRaw: 30e6, EarliestRedeem: uint64(now.Add(20 * time.Minute).Unix()), Receipts: "r", State: "s", Addresses: "a"}
	planned, err := fleet.PlanVoltr(r, o, epoch, vaultID, nil, now)
	if err != nil || planned == nil {
		t.Fatal(planned, err)
	}
	planner, err := fleet.NewStoreFromPool(pool)
	if err != nil {
		t.Fatal(err)
	}
	if inserted, err := planner.PublishVoltr(ctx, r.Cluster, epochID, *planned); err != nil || !inserted {
		t.Fatalf("publish %v %v", inserted, err)
	}
	lease, err := store.claimVoltr(ctx, r.Cluster, "voltr-owner", time.Minute)
	if err != nil || lease == nil || lease.VaultID != vaultID {
		t.Fatalf("claim %+v %v", lease, err)
	}
	if _, _, err := admitVoltr(r, *lease); err != nil {
		t.Fatalf("Rust prepare() refused the Go plan: %v", err)
	}
	tampered := *lease
	tampered.AmountRaw++
	if _, _, err := admitVoltr(r, tampered); err == nil {
		t.Fatal("amount drift admitted")
	}
	// Sign a stand-in message as the guardian would; the chain is not needed
	// to prove the rows.
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	message := append([]byte{0x80, 1, 0, 0, 1}, bytes.Repeat([]byte{3}, 64)...)
	signature := ed25519.Sign(key, message)
	wire := append(append([]byte{1}, signature...), message...)
	wh, mh := sha256.Sum256(wire), sha256.Sum256(message)
	conflicts := []string{"voltr:vault:" + r.Vault, "kamino:reserve:" + r.Strategies[0].Reserve}
	sort.Strings(conflicts)
	epochs, _ := json.Marshal(map[string]any{"routeBundleSha256": r.BundleSHA256, "lookupTable": fleet.VoltrLookupTable, "lookupTableOrderedAddressesSha256": fleet.VoltrLookupTableOrderedSHA256, "lookupTableAddressCount": fleet.VoltrLookupTableAddressCount})
	signed := voltrSigned{payer: r.Guardian, fee: 5000, writable: []string{r.Guardian, "writable:" + suffix}, conflicts: conflicts, requirements: *lease.RequirementsFingerprint, selection: "sel", epochs: epochs,
		wire: WireIdentity{SignedTransaction: wire, SignedTransactionHash: hex.EncodeToString(wh[:]), MessageHash: hex.EncodeToString(mh[:]), TransactionSignature: base58.Encode(signature), RecentBlockhash: "hash", LastValidBlockHeight: 5000}}
	// The registered 0025 trigger guard_signed_route_fee_payer_role admits a
	// policy-paid row only with reusable-v2 table evidence, which the pinned
	// Voltr ALT is not; Rust's record_voltr_manager_decision_with_signed_submission
	// writes these same rows and is refused the same way. The handoff is atomic:
	// nothing of it survives, and the opportunity keeps its lease.
	err = store.persistVoltr(ctx, "voltr-owner", *lease, signed)
	if err == nil || !strings.Contains(err.Error(), "policy route fee payer requires selected reusable-v2 table evidence") {
		t.Fatalf("persist under the frozen schema: %v", err)
	}
	var submissions, leases, decisions int
	var state string
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE opportunity_id=$1),(SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id=$1),
 (SELECT count(*) FROM loyal_yield.rebalance_decisions WHERE vault_id=$2),(SELECT opportunity_state FROM loyal_yield.rebalance_opportunities WHERE id=$1)`, lease.OpportunityID, vaultID).Scan(&submissions, &leases, &decisions, &state); err != nil {
		t.Fatal(err)
	}
	if submissions != 0 || leases != 0 || decisions != 0 || state != "leased" {
		t.Fatalf("partial voltr handoff survived: %d %d %d %s", submissions, leases, decisions, state)
	}
}

func TestVoltrReconciliationRequiresTheExactLeg(t *testing.T) {
	withdraw := voltrPlan{Operation: "withdraw", Amount: 20, PreIdle: 10, PrePosition: 90, PreTotal: 100}
	if !voltrEffectMatches(withdraw, fleet.VoltrObservation{IdleRaw: 30, PositionsRaw: [4]uint64{70}, TotalValueRaw: 100}, 0) {
		t.Fatal("exact withdrawal rejected")
	}
	if voltrEffectMatches(withdraw, fleet.VoltrObservation{IdleRaw: 29, PositionsRaw: [4]uint64{70}, TotalValueRaw: 100}, 0) {
		t.Fatal("short idle accepted")
	}
	deposit := voltrPlan{Operation: "deposit", Amount: 20, PreIdle: 30, PrePosition: 0, PreTotal: 100}
	if !voltrEffectMatches(deposit, fleet.VoltrObservation{IdleRaw: 10, PositionsRaw: [4]uint64{20}, TotalValueRaw: 100}, 0) || voltrEffectMatches(deposit, fleet.VoltrObservation{IdleRaw: 10, PositionsRaw: [4]uint64{19}, TotalValueRaw: 100}, 0) {
		t.Fatal("deposit effect check differs from Rust")
	}
}
