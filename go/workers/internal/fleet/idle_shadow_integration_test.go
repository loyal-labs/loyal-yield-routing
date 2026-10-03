package fleet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestIdleShadowLoaderIncludesIdleOnlyAndRetainsPolicyFences(t *testing.T) {
	raw := os.Getenv("FLEET_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := OpenStore(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "idle-shadow-" + suffix
	source := ReserveIdentity{Address: testIdentity(70), Market: testIdentity(71), Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(72), Market: source.Market, Mint: USDCMint}
	id := seedWorkerVault(t, ctx, store, suffix, source.Market, source.Address)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE loyal_yield.route_policies SET cluster=$2,source_commitment='finalized',finalized_eligible=true WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, id, cluster)
	exec(`INSERT INTO loyal_yield.vault_idle_token_balances_current(vault_id,mint,amount_raw,owner,token_account,observed_slot,observed_at,source_commitment,updated_at) VALUES($1,$2,3000000,$3,$4,999,clock_timestamp(),'finalized',clock_timestamp())`, id, USDCMint, "vault:"+suffix, "idle:"+suffix)
	snapshot := idleTestSnapshot()
	snapshot.Reserves = map[string]ReserveState{}
	for _, identity := range []ReserveIdentity{source, target} {
		snapshot.Reserves[identity.Address] = ReserveState{ReserveIdentity: identity, Slot: 1000, LastUpdateSlot: 999, SupplyAPYBPS: 900, TotalSupplyUSDMicros: 1_000_000_000_000_000, DataHash: fmt.Sprintf("%064x", identity.Address[0])}
	}
	epoch := testImmutableMarketEpoch(t, snapshot, source, target)
	options := FleetLoadOptions{DelegatedSigner: "authority:" + suffix, IncludeIdleShadowSources: true}
	load := func() []FleetVault {
		t.Helper()
		rows, err := store.LoadMigratedFleet(ctx, cluster, epoch, options)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	rows := load()
	if len(rows) != 2 || rows[1].IdleTokenAccount != "idle:"+suffix || len(rows[1].AllowedTargets) != 2 {
		t.Fatalf("mixed source coverage: %+v", rows)
	}
	options.IncludeIdleShadowSources = false
	if rows := load(); len(rows) != 1 {
		t.Fatal("idle source appeared without shadow opt-in")
	}
	options.IncludeIdleShadowSources = true
	exec(`DELETE FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1`, id)
	if rows := load(); len(rows) != 1 || rows[0].IdleTokenAccount == "" {
		t.Fatal("idle-only vault omitted")
	}
	// Finality, signer, amount, and policy mint admission must all remain intact.
	options.DelegatedSigner = "wrong-signer"
	if len(load()) != 0 {
		t.Fatal("wrong signer admitted")
	}
	options.DelegatedSigner = "authority:" + suffix
	exec(`UPDATE loyal_yield.route_policies SET finalized_eligible=false WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, id)
	if len(load()) != 0 {
		t.Fatal("unfinalized policy admitted")
	}
	exec(`UPDATE loyal_yield.route_policies SET finalized_eligible=true WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, id)
	exec(`UPDATE loyal_yield.vault_idle_token_balances_current SET amount_raw=0 WHERE vault_id=$1`, id)
	if len(load()) != 0 {
		t.Fatal("zero idle balance admitted")
	}
	exec(`UPDATE loyal_yield.vault_idle_token_balances_current SET amount_raw=3000000,mint=$2 WHERE vault_id=$1`, id, USDTMint)
	if len(load()) != 0 {
		t.Fatal("unauthorized mint admitted")
	}
	exec(`UPDATE loyal_yield.vault_idle_token_balances_current SET mint=$2 WHERE vault_id=$1`, id, USDCMint)
	var targetID int64
	err = store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_targets
 (settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,wallet,wallet_usdc_ata,vault_usdc_ata,token_mint,wallet_token_ata,vault_token_ata,delegated_signers,threshold,max_amount_per_period,desired_active,chain_status,chain_observation_slot,last_seen_slot,last_seen_signature,cluster)
 VALUES($1,$2,2,$3,0,$4,$2,$5,$6,$7,$5,$6,ARRAY[$2],1,1000,true,'active',999,999,$8,$9) RETURNING id`, "settings:"+suffix, "authority:"+suffix, "sweep:"+suffix, "vault:"+suffix, "wallet-ata:"+suffix, "idle:"+suffix, USDCMint, "sweep-seen:"+suffix, "mainnet-beta").Scan(&targetID)
	if err != nil {
		t.Fatal(err)
	}
	claim := "idle-claim:" + suffix
	exec(`INSERT INTO loyal_yield.balance_sweep_lot_claims(claim_token,target_id,amount_raw,status) VALUES($1,$2,100,'selected')`, claim, targetID)
	if len(load()) != 1 {
		t.Fatal("claim without a pull incorrectly blocked idle funds")
	}
	insertAttempt := func(kind, state string) {
		exec(`INSERT INTO loyal_yield.balance_sweep_transaction_attempts
 (claim_token,target_id,operation_kind,amount_raw,source_pre_balance_raw,destination_pre_balance_raw,signature,signed_transaction_base64,signed_transaction_sha256,recent_blockhash,last_valid_block_height,attempt_state)
 VALUES($1,$2,$3,100,100,0,$4,'dGVzdA==',repeat('a',64),'local-blockhash',1000,$5)`, claim, targetID, kind, "attempt:"+kind+suffix, state)
	}
	insertAttempt("pull", "prepared")
	for _, state := range []string{"prepared", "submitted", "confirmed", "unknown", "ambiguous"} {
		exec(`UPDATE loyal_yield.balance_sweep_transaction_attempts SET attempt_state=$2 WHERE claim_token=$1 AND operation_kind='pull'`, claim, state)
		if len(load()) != 0 {
			t.Fatalf("idle funds escaped %s pull ownership", state)
		}
	}
	insertAttempt("top_up", "prepared")
	if len(load()) != 0 {
		t.Fatal("unconfirmed top-up released pull ownership")
	}
	exec(`UPDATE loyal_yield.balance_sweep_transaction_attempts SET attempt_state='confirmed',confirmed_slot=1000 WHERE claim_token=$1 AND operation_kind='top_up'`, claim)
	if len(load()) != 1 {
		t.Fatal("confirmed top-up did not release idle funds")
	}
}
