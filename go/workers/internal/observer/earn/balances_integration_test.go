package earn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
)

// A vault transaction that moves no wallet cash (a fleet move, a deposit of
// idle cash) is observed as the vault's balances: the observer makes them
// current, ignores an older observation, and closes the vault's app
// positions only when an observation that read its accounts finds nothing.
func TestBalancesObservationIsTheOneWriterOfCurrentHoldings(t *testing.T) {
	pool := observerPool(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	settings, vaultPubkey, reserve, market := "settings:"+suffix, "vault:"+suffix, "reserve:"+suffix, "market:"+suffix
	var policyID, vaultID, positionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,threshold,last_seen_slot,last_seen_signature) VALUES($1,$1,1,$2,0,$3,1,100,$1) RETURNING id`, settings, "policy:"+suffix, vaultPubkey).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,0,$2,$3,true) RETURNING id`, settings, vaultPubkey, policyID).Scan(&vaultID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.user_yield_positions(wallet_address,smart_account_address,settings,vault_index,vault_pubkey,policy_id,policy_account,policy_seed,initial_reserve,initial_market,initial_liquidity_mint,deposit_mint,principal_amount_raw,first_deposit_signature,last_deposit_signature,last_confirmed_slot,status,created_at,updated_at,current_reserve,current_market,current_liquidity_mint,current_amount_raw,current_observed_slot,current_observed_at)
VALUES($1,$2,$3,0,$2,$4,$5,1,$6,$7,$8,$8,1000,$9,$9,100,'active',now(),now(),$6,$7,$8,1000,100,now()) RETURNING id`, "wallet:"+suffix, vaultPubkey, settings, policyID, "policy:"+suffix, reserve, market, itestUSDCMint, "signature:"+suffix).Scan(&positionID); err != nil {
		t.Fatal(err)
	}
	observe := func(slot uint64, amount, idle uint64) {
		t.Helper()
		m := EarnBalancesMutation{Settings: settings, VaultPubkey: vaultPubkey, ObservedSlot: slot,
			ReserveState: []EarnReserveMutation{{Reserve: reserve, Market: &market, LiquidityMint: itestUSDCMint, AmountRaw: amount, HasValue: true, PlanningMetadata: []byte(`{"amount_semantics":"redeemable_liquidity_amount"}`)}},
			IdleState:    []EarnIdleTokenMutation{{Mint: itestUSDCMint, AmountRaw: idle, Owner: vaultPubkey, TokenAccount: "ata:" + suffix, ObservedSlot: slot, SourceCommitment: "confirmed"}}}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := applyBalances(ctx, tx, m); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	current := func() (amount int64, status string) {
		t.Helper()
		err := pool.QueryRow(ctx, `SELECT amount_raw FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1 AND reserve=$2`, vaultID, reserve).Scan(&amount)
		if err != nil && err != pgx.ErrNoRows {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT status::text FROM loyal_yield.user_yield_positions WHERE id=$1`, positionID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return amount, status
	}

	observe(200, 1_005, 0)
	if amount, status := current(); amount != 1_005 || status != "active" {
		t.Fatalf("holding vault: current %d, position %s", amount, status)
	}
	// Idle cash alone keeps the funds in the vault.
	observe(250, 0, 3)
	if _, status := current(); status != "active" {
		t.Fatalf("idle cash closed the position: %s", status)
	}
	observe(300, 0, 0)
	if _, status := current(); status != "closed" {
		t.Fatalf("emptied vault left its position %s", status)
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.user_yield_position_holding_events WHERE position_id=$1 AND event_type='snapshot_reconciled' AND observed_slot=300`, positionID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("close holding event %d %v", events, err)
	}
	// An older observation never replaces the current one.
	observe(299, 77, 0)
	if amount, _ := current(); amount == 77 {
		t.Fatal("an older observation became current")
	}
}

// A full withdrawal lost past the stream's replay window is never streamed;
// the observer reads every binding from confirmed state when it resumes. That
// unsigned read of the emptied obligation is the vault's balances: its
// position closes and its reserve row goes, or autodeposit would keep
// pulling cash for a user who left.
func TestRecoveredReadOfAnEmptiedVaultClosesItsPosition(t *testing.T) {
	pool := observerPool(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	vaultKey, market, reserve := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	settings, vaultPubkey, marketText := "settings:"+suffix, vaultKey.String(), market.String()
	var policyID, vaultID, positionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,threshold,last_seen_slot,last_seen_signature) VALUES($1,$1,1,$2,0,$3,1,100,$1) RETURNING id`, settings, "policy:"+suffix, vaultPubkey).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,0,$2,$3,true) RETURNING id`, settings, vaultPubkey, policyID).Scan(&vaultID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.user_yield_positions(wallet_address,smart_account_address,settings,vault_index,vault_pubkey,policy_id,policy_account,policy_seed,initial_reserve,initial_market,initial_liquidity_mint,deposit_mint,principal_amount_raw,first_deposit_signature,last_deposit_signature,last_confirmed_slot,status,created_at,updated_at,current_reserve,current_market,current_liquidity_mint,current_amount_raw,current_observed_slot,current_observed_at)
VALUES($1,$2,$3,0,$2,$4,$5,1,$6,$7,$8,$8,1000,$9,$9,100,'active',now(),now(),$6,$7,$8,1000,100,now()) RETURNING id`, "wallet:"+suffix, vaultPubkey, settings, policyID, "policy:"+suffix, reserve.String(), marketText, itestUSDCMint, "signature:"+suffix).Scan(&positionID); err != nil {
		t.Fatal(err)
	}
	// The last streamed observation: 1,000 in the reserve.
	held := EarnBalancesMutation{Settings: settings, VaultPubkey: vaultPubkey, ObservedSlot: 100,
		ReserveState: []EarnReserveMutation{{Reserve: reserve.String(), Market: &marketText, LiquidityMint: itestUSDCMint, AmountRaw: 1_000, HasValue: true}}}
	apply := func(m EarnBalancesMutation) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := applyBalances(ctx, tx, m); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	apply(held)

	// The chain now: the obligation holds no deposit and the idle account is
	// empty.
	obligation, err := kamino.VanillaObligation(vaultKey, market)
	if err != nil {
		t.Fatal(err)
	}
	obligationData := make([]byte, kamino.ObligationSize)
	copy(obligationData, kamino.ObligationDiscriminator[:])
	copy(obligationData[32:64], market[:])
	copy(obligationData[64:96], vaultKey[:])
	idle := associatedToken(vaultKey, usdcMint, tokenProgram)
	idleData := make([]byte, 165)
	copy(idleData[0:32], usdcMint[:])
	copy(idleData[32:64], vaultKey[:])
	idleData[108] = 1
	accounts := map[string]map[string]any{
		obligation.String(): {"owner": kamino.ProgramID.String(), "lamports": 1, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(obligationData), "base64"}},
		idle.String():       {"owner": tokenProgram.String(), "lamports": 1, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(idleData), "base64"}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     uint64            `json:"id"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		var requested []string
		_ = json.Unmarshal(call.Params[0], &requested)
		values := make([]any, len(requested))
		for i, address := range requested {
			if account, ok := accounts[address]; ok {
				values[i] = account
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{"context": map[string]any{"slot": 500}, "value": values}})
	}))
	defer server.Close()
	client, err := chain.New(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	vault := watch.Vault{Settings: settings, Vault: vaultPubkey, Accounts: []watch.Account{{Pubkey: obligation.String(), Role: "obligation"}, {Pubkey: idle.String(), Role: "idle_token"}}}
	policy := PolicyMatchInput{KaminoMarkets: []string{marketText}, KaminoLiquidityMints: []string{itestUSDCMint}}
	address := obligation.String()
	recovered := NormalizedUpdate{EventKind: "account", AccountPubkey: &address, Slot: 500}
	mutation, err := resolveMutation(ctx, client, recovered, vault, ReconciliationContext{RoutePolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	if mutation.Balances == nil {
		t.Fatalf("the recovered read of the emptied vault changed nothing: %+v", mutation)
	}
	apply(*mutation.Balances)
	var rows int
	var status string
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1 AND amount_raw>0),(SELECT status::text FROM loyal_yield.user_yield_positions WHERE id=$2)`, vaultID, positionID).Scan(&rows, &status); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || status != "closed" {
		t.Fatalf("emptied vault: %d reserve rows, position %s", rows, status)
	}
}
