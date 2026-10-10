package earn

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
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

// streamTransaction is one successful stream transaction whose signer is keys[0].
func streamTransaction(slot uint64, keys [][]byte, instructions ...*pb.CompiledInstruction) *pb.SubscribeUpdate {
	signature := solana.NewWallet().PublicKey().Bytes()
	return &pb.SubscribeUpdate{UpdateOneof: &pb.SubscribeUpdate_Transaction{Transaction: &pb.SubscribeUpdateTransaction{Slot: slot, Transaction: &pb.SubscribeUpdateTransactionInfo{
		Signature: append(signature, signature...), Meta: &pb.TransactionStatusMeta{},
		Transaction: &pb.Transaction{Message: &pb.Message{Header: &pb.MessageHeader{NumRequiredSignatures: 1}, AccountKeys: keys, Instructions: instructions}},
	}}}}
}

// createRecurringUpdate is a transaction in which wallet creates a USDC
// Subscriptions recurring delegation to vault, outside Squads.
func createRecurringUpdate(t *testing.T, slot uint64, wallet, vault, delegation string, nonce, amount, period uint64) *pb.SubscribeUpdate {
	t.Helper()
	authority, err := subscriptionAuthority(wallet, itestUSDCMint)
	if err != nil {
		t.Fatal(err)
	}
	keys := [][]byte{solana.MustPublicKeyFromBase58(wallet).Bytes(), solana.MustPublicKeyFromBase58(authority).Bytes(), solana.MustPublicKeyFromBase58(delegation).Bytes(),
		solana.MustPublicKeyFromBase58(vault).Bytes(), solana.SystemProgramID.Bytes(), subscriptionsProgram.Bytes()}
	data := []byte{subscriptionsCreateRecurring}
	for _, value := range []uint64{nonce, amount, period, 0, 2_000_000_000, 123} {
		data = binary.LittleEndian.AppendUint64(data, value)
	}
	return streamTransaction(slot, keys, &pb.CompiledInstruction{ProgramIdIndex: 5, Accounts: []byte{0, 1, 2, 3, 4}, Data: data})
}

func streamApplication(t *testing.T, pool *pgxpool.Pool) *Application {
	t.Helper()
	app, err := NewApplication(context.Background(), pool, nil, "mainnet-beta", solana.NewWallet().PublicKey(), nil, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// The shared policy cursor follows these slots; the worker fixture expects it
// within 100,000 slots of 449,073,607.
const streamSlot = 449_073_500

type delegationRow struct {
	delegation, status          string
	nonce, amount, period, slot int64
	requested                   int64
}

func targetDelegation(t *testing.T, pool *pgxpool.Pool, targetID int64) delegationRow {
	t.Helper()
	var row delegationRow
	if err := pool.QueryRow(context.Background(), `SELECT target.recurring_delegation, target.recurring_delegation_nonce, target.max_amount_per_period,
            target.period_length_seconds, target.recurring_delegation_confirmed_slot, target.chain_status, request.requested_slot
        FROM loyal_yield.balance_sweep_targets AS target
        JOIN loyal_yield.autodeposit_reconciliation_requests AS request ON request.target_id = target.id
        WHERE target.id = $1`, targetID).Scan(&row.delegation, &row.nonce, &row.amount, &row.period, &row.slot, &row.status, &row.requested); err != nil {
		t.Fatal(err)
	}
	return row
}

// The delegation transaction carries no Squads instruction: the stream
// projects it onto the wallet's Autodeposit target with no chain read, and
// another wallet's delegation to the same vault leaves the target alone.
func TestStreamRecurringDelegationProjectsAutodepositTarget(t *testing.T) {
	pool := observerPool(t)
	ctx := context.Background()
	f := seedRoutedAutodeposit(t, pool)
	var wallet string
	if err := pool.QueryRow(ctx, `SELECT wallet FROM loyal_yield.balance_sweep_targets WHERE id = $1`, f.targetID).Scan(&wallet); err != nil {
		t.Fatal(err)
	}
	app := streamApplication(t, pool)
	stranger, delegation := solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String()
	if err := app.HandlePolicyTransaction(ctx, createRecurringUpdate(t, streamSlot-1, stranger, f.vault, solana.NewWallet().PublicKey().String(), 1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	var untouched bool
	if err := pool.QueryRow(ctx, `SELECT recurring_delegation IS NULL FROM loyal_yield.balance_sweep_targets WHERE id = $1`, f.targetID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("another wallet's delegation attached to the target: %v", err)
	}
	if err := app.HandlePolicyTransaction(ctx, createRecurringUpdate(t, streamSlot, wallet, f.vault, delegation, 7, 5_000_000, 86_400)); err != nil {
		t.Fatal(err)
	}
	if got := targetDelegation(t, pool, f.targetID); got != (delegationRow{delegation, "pending", 7, 5_000_000, 86_400, streamSlot, streamSlot}) {
		t.Fatalf("target delegation = %+v", got)
	}
}

// Mobile sends the Autodeposit policy and its delegation together, so the
// delegation can land first; the target the policy creates still gets it.
func TestDelegationBeforeItsPolicyReachesTheTarget(t *testing.T) {
	pool := observerPool(t)
	ctx := context.Background()
	f := seedRoutedAutodeposit(t, pool)
	wallet, delegation := solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String()
	if err := streamApplication(t, pool).HandlePolicyTransaction(ctx, createRecurringUpdate(t, streamSlot, wallet, f.vault, delegation, 3, 9_000_000, 3_600)); err != nil {
		t.Fatal(err)
	}
	policy := solana.NewWallet().PublicKey().String()
	if err := NewStore(pool).RecordBalanceSweepPolicyMatch(ctx, BalanceSweepPolicyMatchInput{Signature: solana.NewWallet().PublicKey().String(), Slot: streamSlot + 1,
		Cluster: "mainnet-beta", Settings: f.settings, Authority: f.authority, PolicySeed: 11, PolicyAccount: policy, VaultIndex: 1, VaultPubkey: f.vault,
		Wallet: wallet, WalletUSDCATA: wallet, VaultUSDCATA: f.vault, TokenMint: itestUSDCMint, WalletTokenATA: wallet, VaultTokenATA: f.vault,
		DelegatedSigners: []string{f.authority}, Threshold: 1, MaxAmountPerPeriod: 9_000_000}); err != nil {
		t.Fatal(err)
	}
	var targetID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM loyal_yield.balance_sweep_targets WHERE policy_account = $1`, policy).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	if got := targetDelegation(t, pool, targetID); got != (delegationRow{delegation, "pending", 3, 9_000_000, 3_600, streamSlot, streamSlot}) {
		t.Fatalf("target delegation = %+v", got)
	}
}

// Anyone can call the Subscriptions program or write a memo: bytes that are
// not ours, or that are out of range, never stop the stream.
func TestOutsideBytesNeverStopTheStream(t *testing.T) {
	pool := observerPool(t)
	ctx := context.Background()
	f := seedRoutedAutodeposit(t, pool)
	app := streamApplication(t, pool)
	wallet, delegation := solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String()
	for name, update := range map[string]*pb.SubscribeUpdate{
		"out of range delegation to a managed vault": createRecurringUpdate(t, streamSlot, wallet, f.vault, delegation, 1<<63, 1, 1),
		"delegation to an unknown vault":             createRecurringUpdate(t, streamSlot, wallet, solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String(), 1, 1, 1),
	} {
		if err := app.HandlePolicyTransaction(ctx, update); err != nil {
			t.Fatalf("%s stopped the stream: %v", name, err)
		}
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM loyal_yield.recurring_delegation_observations WHERE wallet = $1)`, wallet).Scan(&recorded); err != nil || recorded {
		t.Fatalf("outside delegation was recorded: %v", err)
	}
	settings := solana.NewWallet().PublicKey()
	vault := squadsVault(settings, 0)
	keys := [][]byte{solana.NewWallet().PublicKey().Bytes(), settings.Bytes(), vault.Bytes(), squads.ProgramID.Bytes(), memoProgram.Bytes()}
	for _, memo := range []string{"loyal:earn-max:v2:unknown", "loyal:earn-max:v2:cancel:request-1"} {
		update := streamTransaction(streamSlot, keys, &pb.CompiledInstruction{ProgramIdIndex: 3, Accounts: []byte{1}, Data: []byte{0}},
			&pb.CompiledInstruction{ProgramIdIndex: 4, Accounts: []byte{2}, Data: []byte(memo)})
		if err := app.HandlePolicyTransaction(ctx, update); err != nil {
			t.Fatalf("memo %q stopped the stream: %v", memo, err)
		}
	}
}

// Fleet reads managed_vaults.active: a setup replayed after its route policy
// was removed must not reactivate the vault.
func TestReplayedSetupKeepsARemovedVaultInactive(t *testing.T) {
	pool := observerPool(t)
	ctx := context.Background()
	f := seedRoutedAutodeposit(t, pool)
	store := NewStore(pool)
	if err := store.RecordPolicyRemoval(ctx, PolicyRemovalInput{Signature: solana.NewWallet().PublicKey().String(), Slot: 200, Cluster: "mainnet-beta",
		SourceCommitment: "confirmed", Settings: f.settings, Authority: f.authority, PolicyAccount: f.routePolicy}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSetupPolicyMatch(ctx, PolicyMatchInput{Signature: solana.NewWallet().PublicKey().String(), Slot: 150, Cluster: "mainnet-beta",
		SourceCommitment: "confirmed", Settings: f.settings, Authority: f.authority, PolicySeed: 8, PolicyAccount: solana.NewWallet().PublicKey().String(),
		VaultIndex: 1, VaultPubkey: f.vault, Threshold: 1, RouteModes: []string{"kamino_init_obligation"}}); err != nil {
		t.Fatal(err)
	}
	var active bool
	if err := pool.QueryRow(ctx, `SELECT active FROM loyal_yield.managed_vaults WHERE vault_pubkey = $1`, f.vault).Scan(&active); err != nil || active {
		t.Fatalf("replayed setup reactivated the removed vault: active=%v %v", active, err)
	}
}
