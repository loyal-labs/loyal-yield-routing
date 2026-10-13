package earn

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
		m := EarnBalancesMutation{Settings: settings, VaultPubkey: vaultPubkey, ObservedSlot: slot, VaultEmpty: amount == 0 && idle == 0,
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

// recoveryVault is a managed vault with one obligation per market and a USDC
// idle account, one active app position of 1,000 and a streamed observation
// of that 1,000 at slot 100, under a route policy listing only its first
// market and USDC.
type recoveryVault struct {
	pool                  *pgxpool.Pool
	settings, vaultPubkey string
	vaultKey              solana.PublicKey
	markets               []solana.PublicKey
	vaultID, positionID   int64
	accounts              map[string]map[string]any
	watch                 watch.Vault
	policy                PolicyMatchInput
}

func newRecoveryVault(t *testing.T) *recoveryVault {
	t.Helper()
	pool := observerPool(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	v := &recoveryVault{pool: pool, vaultKey: solana.NewWallet().PublicKey(), markets: []solana.PublicKey{solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()}, accounts: map[string]map[string]any{}}
	v.settings, v.vaultPubkey = "settings:"+suffix, v.vaultKey.String()
	reserve, market := solana.NewWallet().PublicKey().String(), v.markets[0].String()
	var policyID int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,threshold,kamino_markets,kamino_liquidity_mints,last_seen_slot,last_seen_signature) VALUES($1,$1,1,$2,0,$3,1,ARRAY[$4]::text[],ARRAY[$5]::text[],100,$1) RETURNING id`, v.settings, "policy:"+suffix, v.vaultPubkey, market, itestUSDCMint).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id,active) VALUES($1,0,$2,$3,true) RETURNING id`, v.settings, v.vaultPubkey, policyID).Scan(&v.vaultID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.user_yield_position_deposits(deposit_signature,policy_signature,confirmed_slot,wallet_address,smart_account_address,settings,vault_index,vault_pubkey,policy_id,policy_account,policy_seed,target_reserve,market,liquidity_mint,deposit_mint,principal_amount_raw,confirmed_at,created_at)
VALUES($1,$2,50,$3,$4,$5,0,$4,$6,$7,1,$8,$9,$10,$10,1000,now(),now()) RETURNING policy_id`, "first:"+suffix, v.settings, "wallet:"+suffix, v.vaultPubkey, v.settings, policyID, "policy:"+suffix, reserve, market, itestUSDCMint).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.user_yield_positions(wallet_address,smart_account_address,settings,vault_index,vault_pubkey,policy_id,policy_account,policy_seed,initial_reserve,initial_market,initial_liquidity_mint,deposit_mint,principal_amount_raw,first_deposit_signature,last_deposit_signature,last_confirmed_slot,status,created_at,updated_at,current_reserve,current_market,current_liquidity_mint,current_amount_raw,current_observed_slot,current_observed_at)
VALUES($1,$2,$3,0,$2,$4,$5,1,$6,$7,$8,$8,1000,$9,$9,50,'active',now(),now(),$6,$7,$8,1000,100,now()) RETURNING id`, "wallet:"+suffix, v.vaultPubkey, v.settings, policyID, "policy:"+suffix, reserve, market, itestUSDCMint, "first:"+suffix).Scan(&v.positionID); err != nil {
		t.Fatal(err)
	}
	v.apply(t, func(tx pgx.Tx) error {
		return applyBalances(ctx, tx, EarnBalancesMutation{Settings: v.settings, VaultPubkey: v.vaultPubkey, ObservedSlot: 100,
			ReserveState: []EarnReserveMutation{{Reserve: reserve, Market: &market, LiquidityMint: itestUSDCMint, AmountRaw: 1_000, HasValue: true}}})
	})
	for _, m := range v.markets {
		obligation, err := kamino.VanillaObligation(v.vaultKey, m)
		if err != nil {
			t.Fatal(err)
		}
		v.watch.Accounts = append(v.watch.Accounts, watch.Account{Pubkey: obligation.String(), Role: "obligation"})
	}
	idle := associatedToken(v.vaultKey, usdcMint, tokenProgram)
	v.watch.Accounts = append(v.watch.Accounts, watch.Account{Pubkey: idle.String(), Role: "idle_token"})
	v.watch.Settings, v.watch.Vault = v.settings, v.vaultPubkey
	v.policy = PolicyMatchInput{KaminoMarkets: []string{market}, KaminoLiquidityMints: []string{itestUSDCMint}}
	// The chain now: empty obligations and an empty idle account.
	for i := range v.markets {
		v.obligation(t, i, solana.PublicKey{}, 0)
	}
	idleData := make([]byte, 165)
	copy(idleData[0:32], usdcMint[:])
	copy(idleData[32:64], v.vaultKey[:])
	idleData[108] = 1
	v.accounts[idle.String()] = map[string]any{"owner": tokenProgram.String(), "lamports": 1, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(idleData), "base64"}}
	return v
}

// obligation sets the vault's obligation in its i-th market to hold collateral
// in reserve.
func (v *recoveryVault) obligation(t *testing.T, i int, reserve solana.PublicKey, collateral uint64) {
	t.Helper()
	address, err := kamino.VanillaObligation(v.vaultKey, v.markets[i])
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, kamino.ObligationSize)
	copy(data, kamino.ObligationDiscriminator[:])
	copy(data[32:64], v.markets[i][:])
	copy(data[64:96], v.vaultKey[:])
	copy(data[96:128], reserve[:])
	binary.LittleEndian.PutUint64(data[128:136], collateral)
	v.accounts[address.String()] = map[string]any{"owner": kamino.ProgramID.String(), "lamports": 1, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}
}

func (v *recoveryVault) apply(t *testing.T, write func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := v.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := write(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// recover resolves the confirmed read of the vault's first obligation at
// slot, as the stream's recovery after a gap enqueues it, and applies it.
func (v *recoveryVault) recover(t *testing.T, slot uint64) {
	t.Helper()
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
			if account, ok := v.accounts[address]; ok {
				values[i] = account
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{"context": map[string]any{"slot": slot}, "value": values}})
	}))
	defer server.Close()
	client, err := chain.New(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	address := v.watch.Accounts[0].Pubkey
	mutation, err := resolveMutation(context.Background(), client, NormalizedUpdate{EventKind: "account", AccountPubkey: &address, Slot: slot}, v.watch, ReconciliationContext{RoutePolicy: &v.policy})
	if err != nil {
		t.Fatal(err)
	}
	if mutation.Balances == nil {
		t.Fatalf("the recovered read changed nothing: %+v", mutation)
	}
	v.apply(t, func(tx pgx.Tx) error { return applyBalances(context.Background(), tx, *mutation.Balances) })
}

func (v *recoveryVault) position(t *testing.T) (status string, principal, reserveRows int64) {
	t.Helper()
	if err := v.pool.QueryRow(context.Background(), `SELECT status::text,principal_amount_raw,(SELECT count(*) FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$2 AND amount_raw>0) FROM loyal_yield.user_yield_positions WHERE id=$1`, v.positionID, v.vaultID).Scan(&status, &principal, &reserveRows); err != nil {
		t.Fatal(err)
	}
	return status, principal, reserveRows
}

// A full withdrawal lost past the stream's replay window is never streamed;
// the observer reads every binding from confirmed state when it resumes. That
// unsigned read of the emptied vault is its balances: its position closes and
// its reserve row goes, or autodeposit would keep pulling cash for a user who
// left.
func TestRecoveredReadOfAnEmptiedVaultClosesItsPosition(t *testing.T) {
	v := newRecoveryVault(t)
	v.recover(t, 500)
	if status, _, rows := v.position(t); rows != 0 || status != "closed" {
		t.Fatalf("emptied vault: %d reserve rows, position %s", rows, status)
	}
}

// Funds in a reserve the active route policy does not list (the policy
// changed, or a cross-mint move left them in another mint) are still the
// vault's: the read that finds them does not empty the vault.
func TestFundsOutsideTheRoutePolicyKeepTheVaultOpen(t *testing.T) {
	v := newRecoveryVault(t)
	v.obligation(t, 1, solana.NewWallet().PublicKey(), 900)
	v.recover(t, 500)
	if status, principal, _ := v.position(t); status != "active" || principal != 1_000 {
		t.Fatalf("vault holding unlisted funds: position %s, principal %d", status, principal)
	}
}

// A recovery read at slot 500 that found the vault empty can apply before a
// replayed deposit made earlier in the gap, whose own read (a node behind)
// is older. The deposit's funds left by 500, so the position it reopens
// closes again rather than staying active with nothing in the vault.
func TestADepositReplayedAfterAnEmptyRecoveryStaysClosed(t *testing.T) {
	v := newRecoveryVault(t)
	v.recover(t, 500)
	ctx := context.Background()
	reconciliation, err := NewStore(v.pool).LoadContext(ctx, v.settings, 0, v.vaultPubkey)
	if err != nil || reconciliation.RoutePolicy == nil {
		t.Fatalf("route policy: %+v %v", reconciliation, err)
	}
	market := v.markets[0].String()
	deposit := EarnDepositMutation{RoutePolicy: *reconciliation.RoutePolicy, DepositSignature: fmt.Sprint("replayed:", time.Now().UnixNano()), DepositSlot: 300, ObservedSlot: 400,
		DepositMint: itestUSDCMint, PrincipalAmountRaw: 700, TargetReserve: solana.NewWallet().PublicKey().String(), Market: &market, LiquidityMint: itestUSDCMint,
		Wallet: "wallet:" + v.settings[len("settings:"):], SmartAccountAddress: v.vaultPubkey}
	v.apply(t, func(tx pgx.Tx) error { return applyDeposit(ctx, tx, deposit) })
	if status, _, _ := v.position(t); status != "closed" {
		t.Fatalf("a deposit older than the empty observation left the position %s", status)
	}
}
