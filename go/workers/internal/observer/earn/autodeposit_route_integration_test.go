package earn

import (
	"context"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

const itestUSDCMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

func observerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("OBSERVER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OBSERVER_TEST_DATABASE_URL is required")
	}
	endpoint, err := url.Parse(databaseURL)
	if err != nil || endpoint.Hostname() != "127.0.0.1" || endpoint.User == nil || endpoint.User.Username() != "workers_v2" || endpoint.Path != "/workers_v2_observer" {
		t.Fatal("observer integration requires the isolated workers_v2 fixture")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// routedAutodeposit is one Earn vault with its route and setup policies and
// an active Autodeposit target on it.
type routedAutodeposit struct {
	settings, authority, vault, routePolicy string
	targetID                                int64
}

func seedRoutedAutodeposit(t *testing.T, pool *pgxpool.Pool) routedAutodeposit {
	t.Helper()
	ctx := context.Background()
	f := routedAutodeposit{
		settings:    solana.NewWallet().PublicKey().String(),
		authority:   solana.NewWallet().PublicKey().String(),
		vault:       solana.NewWallet().PublicKey().String(),
		routePolicy: solana.NewWallet().PublicKey().String(),
	}
	var routeID, setupID int64
	insertPolicy := `INSERT INTO loyal_yield.route_policies
        (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
         threshold, route_modes, active, last_seen_slot, last_seen_signature, cluster)
        VALUES ($1, $2, $3, $4, 1, $5, 1, $6, true, 100, 'itest', 'mainnet-beta') RETURNING id`
	if err := pool.QueryRow(ctx, insertPolicy, f.settings, f.authority, 7, f.routePolicy, f.vault, []string{"same_mint_kamino"}).Scan(&routeID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, insertPolicy, f.settings, f.authority, 8, solana.NewWallet().PublicKey().String(), f.vault, []string{"kamino_init_obligation"}).Scan(&setupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.managed_vaults (settings, vault_index, vault_pubkey, active_policy_id, setup_policy_id, active)
        VALUES ($1, 1, $2, $3, $4, true)`, f.settings, f.vault, routeID, setupID); err != nil {
		t.Fatal(err)
	}
	wallet := solana.NewWallet().PublicKey().String()
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_targets
        (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
         wallet, wallet_usdc_ata, vault_usdc_ata, wallet_token_ata, vault_token_ata, token_mint, threshold,
         max_amount_per_period, desired_active, chain_status, wallet_balance_floor_raw,
         last_seen_slot, last_seen_signature, cluster)
        VALUES ($1, $2, 9, $3, 1, $4, $5, $5, $4, $5, $4, $6, 1, 1000000000, true, 'active', 100000,
                1, 'itest', 'mainnet-beta') RETURNING id`,
		f.settings, f.authority, solana.NewWallet().PublicKey().String(), f.vault, wallet, itestUSDCMint).Scan(&f.targetID); err != nil {
		t.Fatal(err)
	}
	return f
}

var itestEventID atomic.Int64

// seedSlotWithLot schedules one slot holding one open 5 USDC lot.
func seedSlotWithLot(t *testing.T, pool *pgxpool.Pool, targetID int64, status string, requestSource *string) (slotID, lotID int64) {
	t.Helper()
	ctx := context.Background()
	eventID := time.Now().UnixNano()/1000 + itestEventID.Add(1)
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_wallet_balance_events
        (event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata, amount_raw, delta_amount_raw,
         observed_slot, observed_at, source, source_commitment, mint)
        VALUES ($1, $2, 'w', 'w', 'w', 5100000, 5000000, 1, now(), 'itest', 'confirmed', $3)`, eventID, targetID, itestUSDCMint); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_scheduled_slots
        (target_id, token_mint, eligible_after, status, request_source)
        VALUES ($1, $2, now() - interval '1 minute', $3::text::loyal_yield.balance_sweep_scheduled_slot_status, $4) RETURNING id`,
		targetID, itestUSDCMint, status, requestSource).Scan(&slotID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_surplus_lots
        (target_id, source_event_id, original_amount_raw, remaining_amount_raw, classification,
         eligible_after, status, confidence, reason, scheduled_slot_id)
        VALUES ($1, $2, 5000000, 5000000, 'unknown', now(), 'open', 'derived', 'itest', $3) RETURNING id`,
		targetID, eventID, slotID).Scan(&lotID); err != nil {
		t.Fatal(err)
	}
	return slotID, lotID
}

func seedClaim(t *testing.T, pool *pgxpool.Pool, targetID int64) string {
	t.Helper()
	token := solana.NewWallet().PublicKey().String()
	if _, err := pool.Exec(context.Background(), `INSERT INTO loyal_yield.balance_sweep_lot_claims (claim_token, target_id, amount_raw, status)
        VALUES ($1, $2, 5000000, 'released')`, token, targetID); err != nil {
		t.Fatal(err)
	}
	return token
}

type autodepositWork struct {
	name            string
	slot, lot       int64
	wantSlotStatus  string
	wantLotStatus   string
	wantSkipMessage bool
}

// seedVaultWork lays out every shape of work a retired route meets: automatic
// slots that must end, and requested, claimed or attempted ones that must not.
func seedVaultWork(t *testing.T, pool *pgxpool.Pool, f routedAutodeposit) []autodepositWork {
	t.Helper()
	ctx := context.Background()
	requested := "web_execute_now"
	var work []autodepositWork
	add := func(name, status string, source *string, wantSlot, wantLot string) *autodepositWork {
		slot, lot := seedSlotWithLot(t, pool, f.targetID, status, source)
		work = append(work, autodepositWork{name: name, slot: slot, lot: lot, wantSlotStatus: wantSlot, wantLotStatus: wantLot, wantSkipMessage: wantSlot == "canceled"})
		return &work[len(work)-1]
	}
	add("automatic", "scheduled", nil, "canceled", "suppressed")
	add("failed unheld", "failed", nil, "canceled", "suppressed")
	add("user requested", "scheduled", &requested, "scheduled", "open")
	claimed := add("claimed", "scheduled", nil, "scheduled", "open")
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_scheduled_slots SET claim_token = $2 WHERE id = $1`, claimed.slot, seedClaim(t, pool, f.targetID)); err != nil {
		t.Fatal(err)
	}
	attempted := add("attempted", "failed", nil, "failed", "open")
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_transaction_attempts
        (claim_token, target_id, scheduled_slot_id, operation_kind, amount_raw, source_pre_balance_raw,
         destination_pre_balance_raw, signature, signed_transaction_base64, signed_transaction_sha256,
         recent_blockhash, last_valid_block_height, attempt_state)
        VALUES ($1, $2, $3, 'pull', 5000000, 5100000, 0, $4, 'AA==', $5, 'blockhash', 1, 'failed')`,
		seedClaim(t, pool, f.targetID), f.targetID, attempted.slot, solana.NewWallet().PublicKey().String(),
		"0000000000000000000000000000000000000000000000000000000000000000"); err != nil {
		t.Fatal(err)
	}
	return work
}

func assertVaultWork(t *testing.T, pool *pgxpool.Pool, f routedAutodeposit, work []autodepositWork) {
	t.Helper()
	ctx := context.Background()
	for _, w := range work {
		var slotStatus, lotStatus string
		var lastError *string
		if err := pool.QueryRow(ctx, `SELECT slot.status::text, slot.last_error, lot.status::text
            FROM loyal_yield.balance_sweep_scheduled_slots AS slot
            JOIN loyal_yield.balance_sweep_surplus_lots AS lot ON lot.id = $2
            WHERE slot.id = $1`, w.slot, w.lot).Scan(&slotStatus, &lastError, &lotStatus); err != nil {
			t.Fatal(err)
		}
		skipMessage := lastError != nil && *lastError == "autodeposit skipped: the user withdrew everything from yield"
		if slotStatus != w.wantSlotStatus || lotStatus != w.wantLotStatus || skipMessage != w.wantSkipMessage {
			t.Errorf("%s slot=%s lot=%s lastError=%v, want slot=%s lot=%s withdrawn reason=%v",
				w.name, slotStatus, lotStatus, lastError, w.wantSlotStatus, w.wantLotStatus, w.wantSkipMessage)
		}
	}
	var desired bool
	var chainStatus string
	if err := pool.QueryRow(ctx, `SELECT desired_active, chain_status FROM loyal_yield.balance_sweep_targets WHERE id = $1`, f.targetID).Scan(&desired, &chainStatus); err != nil {
		t.Fatal(err)
	}
	if !desired || chainStatus != "active" {
		t.Fatalf("target desired_active=%v chain_status=%s, want the Autodeposit target left active", desired, chainStatus)
	}
}

// Removing the vault's route policy leaves its Autodeposit slots without a
// destination; dispatch requires the route, so the removal itself must end
// them or they stay scheduled forever.
func TestRoutePolicyRemovalEndsVaultAutodepositWork(t *testing.T) {
	pool := observerPool(t)
	f := seedRoutedAutodeposit(t, pool)
	work := seedVaultWork(t, pool, f)
	if err := NewStore(pool).RecordPolicyRemoval(context.Background(), PolicyRemovalInput{
		Signature: solana.NewWallet().PublicKey().String(), Slot: 200, Cluster: "mainnet-beta", SourceCommitment: "confirmed",
		Settings: f.settings, Authority: f.authority, PolicyAccount: f.routePolicy,
	}); err != nil {
		t.Fatal(err)
	}
	assertVaultWork(t, pool, f, work)
}

// Earn cleanup (the user's final exit) retires both policies and the vault
// in one transaction, and with them the vault's automatic Autodeposit work.
func TestEarnCleanupEndsVaultAutodepositWork(t *testing.T) {
	pool := observerPool(t)
	f := seedRoutedAutodeposit(t, pool)
	work := seedVaultWork(t, pool, f)
	ctx := context.Background()
	observedAt := time.Now()
	if err := db.WithTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return applyCleanup(ctx, tx, EarnCleanupMutation{Settings: f.settings, VaultIndex: 1, VaultPubkey: f.vault,
			CleanupSignature: solana.NewWallet().PublicKey().String(), ConfirmedSlot: 300, ObservedAt: &observedAt})
	}); err != nil {
		t.Fatal(err)
	}
	assertVaultWork(t, pool, f, work)
}
