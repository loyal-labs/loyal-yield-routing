package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// scriptedSweepRPC is one confirmed bank: the slot and accounts the next
// getMultipleAccounts context returns. Unknown addresses are null accounts.
type scriptedSweepRPC struct {
	mu       sync.Mutex
	slot     int64
	accounts map[string]chain.Account
	batches  int
}

func (r *scriptedSweepRPC) Accounts(_ context.Context, keys []sdk.PublicKey, _ rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if minContextSlot == 0 || uint64(r.slot) < minContextSlot || len(keys) > 100 {
		return 0, nil, fmt.Errorf("invalid scripted batch")
	}
	r.batches++
	out := make([]*chain.Account, len(keys))
	for i, key := range keys {
		if account, ok := r.accounts[key.String()]; ok {
			account.Data = append([]byte(nil), account.Data...)
			out[i] = &account
		}
	}
	return uint64(r.slot), out, nil
}

func sweepKey(label string) string {
	hash := sha256.Sum256([]byte(label))
	return sdk.PublicKeyFromBytes(hash[:]).String()
}

func putKey(data []byte, offset int, address string) {
	key := sdk.MustPublicKeyFromBase58(address)
	copy(data[offset:offset+32], key[:])
}

// sweepReserve is a KLend reserve whose collateral redeems at 2 liquidity.
func sweepReserve(address, market string) chain.Account {
	data := make([]byte, 8624)
	copy(data[:8], []byte{43, 242, 204, 202, 26, 247, 59, 127})
	binary.LittleEndian.PutUint64(data[8:16], 1)
	putKey(data, 32, market)
	putKey(data, 128, fleet.USDCMint)
	putKey(data, 160, sweepKey(address+":liquidity-supply"))
	putKey(data, 408, sdk.TokenProgramID.String())
	putKey(data, 2560, sweepKey(address+":collateral-mint"))
	putKey(data, 2600, sweepKey(address+":collateral-supply"))
	binary.LittleEndian.PutUint64(data[224:232], 1_000_000)
	binary.LittleEndian.PutUint64(data[2592:2600], 500_000)
	return fixtureAccount(address, fleet.KLendProgram, 1, data)
}

func sweepObligation(address, market, vault, reserve string, collateral uint64) chain.Account {
	data := make([]byte, 3344)
	copy(data[:8], positionSweepObligationDiscriminator)
	putKey(data, 32, market)
	putKey(data, 64, vault)
	if reserve != "" {
		putKey(data, 96, reserve)
		binary.LittleEndian.PutUint64(data[128:136], collateral)
	}
	return fixtureAccount(address, fleet.KLendProgram, 1, data)
}

func sweepTokenAccount(address, mint, owner string, amount uint64) chain.Account {
	data := make([]byte, 165)
	putKey(data, 0, mint)
	putKey(data, 32, owner)
	binary.LittleEndian.PutUint64(data[64:72], amount)
	data[108] = 1
	return fixtureAccount(address, sdk.TokenProgramID.String(), 1, data)
}

func seedSweepCatalog(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cluster string, roles map[string]string) {
	t.Helper()
	hash, err := positionSweepCatalogMintsHash(fleet.EarnStableMints())
	if err != nil {
		t.Fatal(err)
	}
	var family, manifest, revision int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_families(cluster,logical_name,kind,planner_version,catalog_version,active_generation,provisioning_authority,payer,hard_capacity,largest_atomic_expansion,safety_margin,allocation_high_water) VALUES($1,'sweep-shared','shared_market','v1','v1',1,$2,$2,256,239,1,16) RETURNING id`, cluster, sweepKey(cluster+":authority")).Scan(&family); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_manifests(family_id,subject_kind,subject_key,desired_set_hash,address_count,source_slot,planner_version,catalog_version) VALUES($1,'shared_market','sweep-catalog',$2,$3,100,'v1','v1') RETURNING id`, family, hash, len(roles)).Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	ordinal := 0
	for _, address := range sortedRoleKeys(roles) {
		if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_manifest_addresses(manifest_id,address,ordinal,semantic_class,account_role,is_writable) VALUES($1,$2,$3,'shared_market',$4,$5)`, manifest, address, ordinal, roles[address], roles[address] == "reserve"); err != nil {
			t.Fatal(err)
		}
		ordinal++
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_manifests SET sealed_at=clock_timestamp() WHERE id=$1`, manifest); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_revisions(family_id,manifest_id,catalog_revision,catalog_version,desired_set_hash,enabled_mints_hash,reserve_set_hash,address_count,source_slot,reason,updated_by) VALUES($1,$2,1,'v1',$3,$3,$3,$4,100,'fixture','fixture') RETURNING id`, family, manifest, hash, len(roles)).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_heads(family_id,catalog_revision_id,target_generation,readiness_state,activated_at) VALUES($1,$2,1,'active',clock_timestamp())`, family, revision); err != nil {
		t.Fatal(err)
	}
}

func sortedRoleKeys(roles map[string]string) []string {
	keys := make([]string, 0, len(roles))
	for key := range roles {
		keys = append(keys, key)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

type sweptPosition struct {
	amount, slot int64
	hasValue     bool
	observedAt   time.Time
}

func sweptPositions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, vaultID int64) map[string]sweptPosition {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT reserve,amount_raw,observed_slot,has_value,observed_at FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1`, vaultID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]sweptPosition{}
	for rows.Next() {
		var reserve string
		var p sweptPosition
		if err := rows.Scan(&reserve, &p.amount, &p.slot, &p.hasValue, &p.observedAt); err != nil {
			t.Fatal(err)
		}
		out[reserve] = p
	}
	return out
}

func currentSweepSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, vaultID int64) (int64, int64, map[string]any) {
	t.Helper()
	var id, slot int64
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT id,observed_slot,context FROM loyal_yield.vault_position_snapshots WHERE vault_id=$1 AND is_current`, vaultID).Scan(&id, &slot, &raw); err != nil {
		t.Fatal(err)
	}
	var context map[string]any
	if err := json.Unmarshal(raw, &context); err != nil {
		t.Fatal(err)
	}
	return id, slot, context
}

// The reconciler's position sweep re-reads every managed vault and publishes
// a complete snapshot: moved positions are current, a drained reserve gets a
// fresh zero row (the evidence Autodeposit's reserve resolution reads), an
// empty vault closes its app positions, and an observation older than what
// another projector already published replaces nothing.
func TestPositionSweepPublishesFreshCompleteVaultSnapshots(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "sweep:" + suffix
	signer, vault, settings := sweepKey(suffix+":signer"), sweepKey(suffix+":vault"), "sweep-settings:"+suffix
	markets := [2]string{sweepKey(suffix + ":market-a"), sweepKey(suffix + ":market-b")}
	reserves := [2]string{sweepKey(suffix + ":reserve-a"), sweepKey(suffix + ":reserve-b")}
	seedSweepCatalog(t, ctx, pool, cluster, map[string]string{reserves[0]: "reserve", reserves[1]: "reserve", markets[0]: "market", markets[1]: "market"})
	var policyID, vaultID, snapshotID int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,last_seen_slot,last_seen_signature) VALUES($1,$2,1,$3,0,$4,ARRAY[$2]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY[$5]::text[],ARRAY[$6,$7]::text[],ARRAY[$5]::text[],'[]',true,900,$8) RETURNING id`,
		settings, signer, sweepKey(suffix+":policy"), vault, fleet.USDCMint, markets[0], markets[1], "sig:"+suffix).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,0,$2,$3,true) RETURNING id`, settings, vault, policyID).Scan(&vaultID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.vault_position_snapshots(vault_id,policy_id,observed_slot,observed_at,is_current,context) VALUES($1,$2,900,clock_timestamp()-interval '1 hour',true,'{}') RETURNING id`, vaultID, policyID).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.vault_reserve_positions_current(vault_id,reserve,market,liquidity_mint,amount_raw,has_value,snapshot_id,observed_slot,observed_at) VALUES($1,$2,$3,$4,500,true,$5,900,clock_timestamp()-interval '1 hour')`, vaultID, reserves[0], markets[0], fleet.USDCMint, snapshotID); err != nil {
		t.Fatal(err)
	}
	// A zero row for a reserve the policy no longer routes is not part of the
	// complete product set and is dropped by the next complete publication.
	retired := sweepKey(suffix + ":retired-reserve")
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.vault_reserve_positions_current(vault_id,reserve,market,liquidity_mint,amount_raw,has_value,snapshot_id,observed_slot,observed_at) VALUES($1,$2,$3,$4,0,false,$5,900,clock_timestamp()-interval '1 hour')`, vaultID, retired, markets[0], fleet.USDCMint, snapshotID); err != nil {
		t.Fatal(err)
	}
	userTables := false
	if err := pool.QueryRow(ctx, `SELECT to_regclass('loyal_yield.user_yield_positions') IS NOT NULL AND to_regclass('loyal_yield.user_yield_position_holding_events') IS NOT NULL`).Scan(&userTables); err != nil {
		t.Fatal(err)
	}
	if userTables {
		if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.user_yield_positions(wallet_address,smart_account_address,settings,vault_index,vault_pubkey,policy_id,policy_account,policy_seed,initial_reserve,initial_market,initial_liquidity_mint,deposit_mint,principal_amount_raw,current_reserve,current_market,current_liquidity_mint,current_amount_raw,current_observed_slot,current_observed_at,first_deposit_signature,last_deposit_signature,last_confirmed_slot,status,created_at,updated_at)
VALUES($1,$2,$3,0,$2,$4,$5,1,$6,$7,$8,$8,1000,$6,$7,$8,1000,900,now(),'sig','sig',900,'active',now(),now())`, "wallet:"+suffix, vault, settings, policyID, sweepKey(suffix+":policy"), reserves[0], markets[0], fleet.USDCMint); err != nil {
			t.Fatal(err)
		}
	}
	userStatus := func() string {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status::text FROM loyal_yield.user_yield_positions WHERE settings=$1`, settings).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}

	var obligations [2]string
	for i := range reserves {
		derived, err := fleet.DeriveVaultReserveAccounts(fleet.KaminoPositionAccounts{Market: markets[i], LiquidityMint: fleet.USDCMint, LiquidityTokenProgram: sdk.TokenProgramID.String()}, vault)
		if err != nil {
			t.Fatal(err)
		}
		obligations[i] = derived.Obligation
	}
	ata, err := associatedCustodyAccount(vault, fleet.USDCMint, sdk.TokenProgramID.String())
	if err != nil {
		t.Fatal(err)
	}
	rpc := &scriptedSweepRPC{slot: 1_000, accounts: map[string]chain.Account{
		reserves[0]:    sweepReserve(reserves[0], markets[0]),
		reserves[1]:    sweepReserve(reserves[1], markets[1]),
		obligations[0]: sweepObligation(obligations[0], markets[0], vault, reserves[0], 700),
		ata:            sweepTokenAccount(ata, fleet.USDCMint, vault, 50),
	}}
	sweep, err := NewPositionSweep(PositionSweepConfig{Cluster: cluster, DelegatedSigner: signer, EnabledMints: fleet.EarnStableMints(), Interval: time.Minute, Concurrency: 4, Facts: testFacts()}, store, rpc)
	if err != nil {
		t.Fatal(err)
	}

	// The positions moved since the last snapshot: the sweep publishes them.
	metrics, err := sweep.sweep(ctx)
	if err != nil || metrics.Refreshed != 1 || metrics.Failed != 0 {
		t.Fatalf("first sweep: %+v %v", metrics, err)
	}
	firstSnapshot, slot, snapshotContext := currentSweepSnapshot(t, ctx, pool, vaultID)
	if slot != 1_000 || snapshotContext["kind"] != positionSweepKind || snapshotContext["publication_scope"] != positionSweepCompleteScope || snapshotContext["idle_vault_liquidity_amount_raw"] != float64(50) {
		t.Fatalf("snapshot not the complete sweep observation: slot=%d context=%v", slot, snapshotContext)
	}
	positions := sweptPositions(t, ctx, pool, vaultID)
	if p := positions[reserves[0]]; p.amount != 700 || !p.hasValue || p.slot != 1_000 {
		t.Fatalf("moved position not published: %+v", p)
	}
	if p := positions[reserves[1]]; p.amount != 0 || p.hasValue || p.slot != 1_000 || time.Since(p.observedAt) > time.Minute {
		t.Fatalf("empty policy reserve lacks a fresh zero row: %+v", p)
	}
	if _, ok := positions[retired]; ok || len(positions) != 2 {
		t.Fatalf("complete publication kept an unobserved reserve: %v", positions)
	}
	var redeemable string
	if err := pool.QueryRow(ctx, `SELECT planning_metadata->>'redeemable_source_liquidity_amount_raw' FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1 AND reserve=$2`, vaultID, reserves[0]).Scan(&redeemable); err != nil || redeemable != "1400" {
		t.Fatalf("redeemable liquidity: %q %v", redeemable, err)
	}
	var idleRows, idleUSDC int64
	var commitment string
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(amount_raw) FILTER (WHERE mint=$2),0),min(source_commitment) FROM loyal_yield.vault_idle_token_balances_current WHERE vault_id=$1 AND observed_slot=1000`, vaultID, fleet.USDCMint).Scan(&idleRows, &idleUSDC, &commitment); err != nil || idleRows != 6 || idleUSDC != 50 || commitment != "confirmed" {
		t.Fatalf("idle set: rows=%d usdc=%d commitment=%s err=%v", idleRows, idleUSDC, commitment, err)
	}
	if userTables && userStatus() != "active" {
		t.Fatal("a funded vault closed its app position")
	}

	// A rebalance withdrew into the vault's own token account: no collateral
	// but idle liquidity is funds in flight, never a closed position.
	rpc.mu.Lock()
	rpc.slot = 1_050
	rpc.accounts[obligations[0]] = sweepObligation(obligations[0], markets[0], vault, "", 0)
	rpc.mu.Unlock()
	if metrics, err = sweep.sweep(ctx); err != nil || metrics.Refreshed != 1 {
		t.Fatalf("in-flight sweep: %+v %v", metrics, err)
	}
	if userTables && userStatus() != "active" {
		t.Fatal("funds parked in the vault token account closed the app position")
	}

	// The reserve is drained and the vault holds nothing: a fresh zero row
	// replaces the funded one and the app position closes.
	rpc.mu.Lock()
	rpc.slot = 1_100
	rpc.accounts[ata] = sweepTokenAccount(ata, fleet.USDCMint, vault, 0)
	rpc.mu.Unlock()
	if metrics, err = sweep.sweep(ctx); err != nil || metrics.Refreshed != 1 {
		t.Fatalf("drain sweep: %+v %v", metrics, err)
	}
	positions = sweptPositions(t, ctx, pool, vaultID)
	if p := positions[reserves[0]]; p.amount != 0 || p.hasValue || p.slot != 1_100 || time.Since(p.observedAt) > time.Minute {
		t.Fatalf("drained reserve lacks a fresh zero row: %+v", p)
	}
	var funded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.vault_position_snapshot_positions p JOIN loyal_yield.vault_position_snapshots s ON s.id=p.snapshot_id WHERE s.vault_id=$1 AND s.observed_slot=1100`, vaultID).Scan(&funded); err != nil || funded != 0 {
		t.Fatalf("history kept zero rows: %d %v", funded, err)
	}
	if userTables && userStatus() != "closed" {
		t.Fatal("an emptied vault left its app position open")
	}
	drainSnapshot, _, _ := currentSweepSnapshot(t, ctx, pool, vaultID)
	if drainSnapshot == firstSnapshot {
		t.Fatal("drain did not publish a new snapshot")
	}

	// Another projector (fleet post-state) already published slot 1300; a
	// sweep that read slot 1200 is older and must replace nothing.
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.vault_reserve_positions_current SET amount_raw=999,has_value=true,observed_slot=1300 WHERE vault_id=$1 AND reserve=$2`, vaultID, reserves[1]); err != nil {
		t.Fatal(err)
	}
	rpc.mu.Lock()
	rpc.slot = 1_200
	rpc.accounts[obligations[1]] = sweepObligation(obligations[1], markets[1], vault, reserves[1], 5)
	rpc.mu.Unlock()
	if metrics, err = sweep.sweep(ctx); err != nil || metrics.Stale != 1 || metrics.Refreshed != 0 {
		t.Fatalf("older sweep: %+v %v", metrics, err)
	}
	if p := sweptPositions(t, ctx, pool, vaultID)[reserves[1]]; p.amount != 999 || p.slot != 1_300 {
		t.Fatalf("older observation replaced a newer one: %+v", p)
	}
	if current, _, _ := currentSweepSnapshot(t, ctx, pool, vaultID); current != drainSnapshot {
		t.Fatal("older observation replaced the current snapshot")
	}
}
