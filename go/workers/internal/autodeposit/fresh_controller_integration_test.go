package autodeposit

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedFreshControllerTarget seeds one projected 5,000,000 claimable slot on a
// target with a delegation and an active yield position, and returns its slot.
func seedFreshControllerTarget(t *testing.T, store *Store, name string) (integrationTarget, int64) {
	t.Helper()
	ctx := context.Background()
	seeded := seedIntegrationTarget(t, store, name)
	var previousOffset int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(last_event_id),0) FROM loyal_yield.projection_offsets WHERE consumer_name=$1`, ConsumerName).Scan(&previousOffset); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `UPDATE loyal_yield.projection_offsets SET last_event_id=$2 WHERE consumer_name=$1`, ConsumerName, previousOffset)
	})
	seedProjectedSurplus(t, store, seeded, previousOffset+1, 9_000_000)
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET recurring_delegation='itest-fresh-delegation' WHERE id=$1`, seeded.TargetID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.user_yield_positions (
 wallet_address, smart_account_address, settings, vault_index, vault_pubkey,
 policy_id, policy_account, policy_seed, initial_reserve, initial_market, initial_liquidity_mint,
 deposit_mint, principal_amount_raw, current_reserve, current_market, current_liquidity_mint,
 current_amount_raw, current_observed_slot, current_observed_at,
 first_deposit_signature, last_deposit_signature, last_confirmed_slot, status, created_at, updated_at)
SELECT wallet,vault_pubkey,settings,vault_index,vault_pubkey,
 7,policy_account,7,'fresh-reserve','fresh-market',$2,
 $2,1,'fresh-reserve','fresh-market',$2,1,1,now(),
 'fresh-existing-deposit','fresh-existing-deposit',1,'active',now(),now()
FROM loyal_yield.balance_sweep_targets WHERE id=$1`, seeded.TargetID, USDCMint); err != nil {
		t.Fatal(err)
	}
	seedLiveVaultPosition(t, store, seeded, "fresh-reserve", "fresh-market", 1, time.Now())
	var slot int64
	if err := store.pool.QueryRow(ctx, `SELECT id FROM loyal_yield.balance_sweep_scheduled_slots WHERE target_id=$1`, seeded.TargetID).Scan(&slot); err != nil {
		t.Fatal(err)
	}
	return seeded, slot
}

// seedLiveVaultPosition records the vault's observed holding in one reserve,
// as the reconciler's position sweep projects it.
func seedLiveVaultPosition(t *testing.T, store *Store, seeded integrationTarget, reserve, market string, amountRaw int64, observedAt time.Time) {
	t.Helper()
	seedTargetLiveReserve(t, store, seeded.TargetID, reserve, market, amountRaw, observedAt)
}

func seedTargetLiveReserve(t *testing.T, store *Store, targetID int64, reserve, market string, amountRaw int64, observedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	var vault, snapshot int64
	if err := store.pool.QueryRow(ctx, `
INSERT INTO loyal_yield.vault_position_snapshots (vault_id, policy_id, observed_slot, observed_at, is_current)
SELECT vault.id, vault.active_policy_id, 1, $2, false
FROM loyal_yield.balance_sweep_targets AS target
JOIN loyal_yield.managed_vaults AS vault
  ON vault.settings = target.settings AND vault.vault_index = target.vault_index AND vault.vault_pubkey = target.vault_pubkey AND vault.active
WHERE target.id = $1
RETURNING vault_id, id`, targetID, observedAt).Scan(&vault, &snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.vault_reserve_positions_current
    (vault_id, reserve, market, liquidity_mint, amount_raw, has_value, snapshot_id, observed_slot, observed_at)
VALUES ($1, $2, $3, $4, $5::bigint, $5::bigint > 0, $6, 1, $7)`, vault, reserve, market, USDCMint, amountRaw, snapshot, observedAt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM loyal_yield.vault_reserve_positions_current WHERE snapshot_id=$1`, snapshot)
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM loyal_yield.vault_position_snapshots WHERE id=$1`, snapshot)
	})
}

// Exercises a fresh claim through both transaction legs against the registered
// SQL schema. RPC effects are explicit immutable receipts, not a live cluster.
func TestControllerFreshClaimCompletesWithoutAppRepair(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, slot := seedFreshControllerTarget(t, store, "fresh-controller")
	wallet, custody := "itest-wallet-usdc-fresh-controller", "itest-vault-usdc-fresh-controller"
	pull, topup := "itest-controller-pull-sig-fresh", "itest-controller-topup-sig-fresh"
	chain := &scriptedControllerChain{
		balances: map[string]int64{wallet: 9_000_000, custody: 0},
		observations: map[string]AttemptObservation{
			pull:  {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_001)},
			topup: {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_002)},
		},
		receipts: map[string]ReceiptEvidence{
			pull: {Signature: pull, Slot: 870_001, Effects: []ReceiptEffect{
				{TokenAccount: wallet, Mint: USDCMint, PreRaw: 9_000_000, PostRaw: 4_000_000},
				{TokenAccount: custody, Mint: USDCMint, PreRaw: 0, PostRaw: 5_000_000},
			}},
			topup: {Signature: topup, Slot: 870_002, Effects: []ReceiptEffect{
				{TokenAccount: custody, Mint: USDCMint, PreRaw: 5_000_000, PostRaw: 0},
				{TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 10, PostRaw: 5_000_010},
			}},
		},
		positions: map[string][2]int64{"fresh-reserve": {5_000_001, 870_002}},
	}
	wires := &scriptedControllerWires{suffix: "-fresh"}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: wires, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	code, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slot})
	if err != nil || code != ResultCompleted {
		t.Fatalf("fresh controller outcome=%v error=%v", code, err)
	}
	if len(wires.built) != 2 || wires.built[0] != "pull" || wires.built[1] != "top_up" {
		t.Fatalf("transaction legs = %v", wires.built)
	}
	var state string
	var walletPre, walletPost, custodyPre, custodyPost, completed, principal, position int64
	if err := store.pool.QueryRow(ctx, `
SELECT claim.status::text, execution.source_pre_balance_raw, execution.source_post_balance_raw,
 execution.destination_pre_balance_raw, execution.destination_post_balance_raw,
 (SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
  WHERE attempt.claim_token=claim.claim_token AND attempt.attempt_state='confirmed' AND attempt.broadcast_count=1),
 yp.principal_amount_raw, yp.current_amount_raw
FROM loyal_yield.balance_sweep_lot_claims AS claim
JOIN loyal_yield.balance_sweep_executions AS execution ON execution.id=claim.execution_id
JOIN loyal_yield.user_yield_positions AS yp ON yp.id=execution.yield_position_id
WHERE claim.target_id=$1`, seeded.TargetID).Scan(&state, &walletPre, &walletPost, &custodyPre, &custodyPost, &completed, &principal, &position); err != nil {
		t.Fatal(err)
	}
	if state != "executed" || walletPre != 9_000_000 || walletPost != 4_000_000 || custodyPre != 0 || custodyPost != 5_000_000 || completed != 2 || principal != 5_000_001 || position != 5_000_001 {
		t.Fatalf("incorrect finalization: %s wallet=%d/%d custody=%d/%d completed=%d principal=%d position=%d", state, walletPre, walletPost, custodyPre, custodyPost, completed, principal, position)
	}
}

// KLend refuses a deposit that mints no collateral. A claim below the
// reserve's minimum deposit is released before the pull, so the wallet never
// funds custody that can never be deposited (production target 7702 pulled 1
// raw unit and its top-up failed with InvalidAmount on every retry).
func TestControllerReleasesClaimBelowMinimumDepositBeforePull(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, slot := seedFreshControllerTarget(t, store, "below-minimum")
	chain := &scriptedControllerChain{balances: map[string]int64{
		"itest-wallet-usdc-below-minimum": 9_000_000, "itest-vault-usdc-below-minimum": 0,
	}}
	wires := &scriptedControllerWires{suffix: "-below-minimum", minimumDeposit: 5_000_001}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: wires, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	code, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slot})
	if code != ResultPreflightBlocked || !errors.Is(err, ErrRouteNotExecutable) {
		t.Fatalf("outcome=%v error=%v, want a preflight block before the pull", code, err)
	}
	if len(wires.built) != 0 {
		t.Fatalf("built %v before refusing the amount", wires.built)
	}
	var status string
	var attempts int64
	if err := store.pool.QueryRow(ctx, `
SELECT claim.status::text,
 (SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts AS attempt WHERE attempt.claim_token=claim.claim_token)
FROM loyal_yield.balance_sweep_lot_claims AS claim WHERE claim.target_id=$1`, seeded.TargetID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "released" || attempts != 0 {
		t.Fatalf("claim %s with %d attempts, want released with none", status, attempts)
	}
}

type refusingSetupWires struct {
	*scriptedControllerWires
	refusal error
}

func (w refusingSetupWires) InspectDestinationSetup(context.Context, DepositPlan) (*DestinationSetupPlan, error) {
	return nil, w.refusal
}

func (w refusingSetupWires) BuildDestinationSetup(context.Context, DepositPlan, DestinationSetupPlan, string, int64) (BuiltWire, error) {
	return BuiltWire{}, w.refusal
}

func (w refusingSetupWires) ReadbackDestinationSetup(context.Context, DepositPlan, DestinationSetupPlan, int64) error {
	return w.refusal
}

// A destination the setup inspection refuses (production target 7940: its
// obligation holds a foreign deposit) moved no wallet funds, so the claim is
// released like every other pre-pull refusal instead of held and retried.
func TestControllerReleasesClaimWhenSetupInspectionRefuses(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, slot := seedFreshControllerTarget(t, store, "setup-refused")
	chain := &scriptedControllerChain{balances: map[string]int64{
		"itest-wallet-usdc-setup-refused": 9_000_000, "itest-vault-usdc-setup-refused": 0,
	}}
	scripted := &scriptedControllerWires{suffix: "-setup-refused"}
	refusal := errors.New("setup obligation contains foreign deposit")
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: refusingSetupWires{scripted, refusal}, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	code, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slot})
	if code != ResultPreflightBlocked || !errors.Is(err, refusal) {
		t.Fatalf("outcome=%v error=%v, want a preflight block", code, err)
	}
	if len(scripted.built) != 0 {
		t.Fatalf("built %v after the setup refusal", scripted.built)
	}
	var status string
	if err := store.pool.QueryRow(ctx, `SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE target_id=$1`, seeded.TargetID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "released" {
		t.Fatalf("claim %s, want released: no wallet funds moved", status)
	}
}

// KLend takes only the liquidity its floored collateral is worth (production
// target 6143: asked 17017816, took 17017815). A shortfall under one
// collateral unit is the deposit, not an ambiguous effect.
func TestControllerCompletesTopUpRoundedDownByKLend(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, slot := seedFreshControllerTarget(t, store, "klend-rounding")
	wallet, custody := "itest-wallet-usdc-klend-rounding", "itest-vault-usdc-klend-rounding"
	pull, topup := "itest-controller-pull-sig-klend-rounding", "itest-controller-topup-sig-klend-rounding"
	chain := &scriptedControllerChain{
		balances: map[string]int64{wallet: 9_000_000, custody: 0},
		observations: map[string]AttemptObservation{
			pull:  {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_001)},
			topup: {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_002)},
		},
		receipts: map[string]ReceiptEvidence{
			pull: {Signature: pull, Slot: 870_001, Effects: []ReceiptEffect{
				{TokenAccount: wallet, Mint: USDCMint, PreRaw: 9_000_000, PostRaw: 4_000_000},
				{TokenAccount: custody, Mint: USDCMint, PreRaw: 0, PostRaw: 5_000_000},
			}},
			topup: {Signature: topup, Slot: 870_002, Effects: []ReceiptEffect{
				{TokenAccount: custody, Mint: USDCMint, PreRaw: 5_000_000, PostRaw: 1},
				{TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 10, PostRaw: 5_000_009},
			}},
		},
		positions: map[string][2]int64{"fresh-reserve": {5_000_000, 870_002}},
	}
	wires := &scriptedControllerWires{suffix: "-klend-rounding", minimumDeposit: 2}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: wires, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	code, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slot})
	if err != nil || code != ResultCompleted {
		t.Fatalf("outcome=%v error=%v, want the rounded deposit completed", code, err)
	}
	var status string
	if err := store.pool.QueryRow(ctx, `SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE target_id=$1`, seeded.TargetID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "executed" {
		t.Fatalf("claim %s, want executed", status)
	}
}

// Our own pull reaches the projection as a wallet outflow carrying the pull's
// signature. The claim already consumed those lots, so depleting the outflow
// again wrote off a second amount of surplus that arrived meanwhile: each
// deposit stranded its own size in the wallet.
func TestProjectionDoesNotDepleteSurplusForOwnPull(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, slot := seedFreshControllerTarget(t, store, "own-pull")
	wallet, custody := "itest-wallet-usdc-own-pull", "itest-vault-usdc-own-pull"
	pull, topup := "itest-controller-pull-sig-own-pull", "itest-controller-topup-sig-own-pull"
	chain := &scriptedControllerChain{
		balances: map[string]int64{wallet: 9_000_000, custody: 0},
		observations: map[string]AttemptObservation{
			pull:  {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_101)},
			topup: {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_102)},
		},
		receipts: map[string]ReceiptEvidence{
			pull: {Signature: pull, Slot: 870_101, Effects: []ReceiptEffect{
				{TokenAccount: wallet, Mint: USDCMint, PreRaw: 9_000_000, PostRaw: 4_000_000},
				{TokenAccount: custody, Mint: USDCMint, PreRaw: 0, PostRaw: 5_000_000},
			}},
			topup: {Signature: topup, Slot: 870_102, Effects: []ReceiptEffect{
				{TokenAccount: custody, Mint: USDCMint, PreRaw: 5_000_000, PostRaw: 0},
				{TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 10, PostRaw: 5_000_010},
			}},
		},
		positions: map[string][2]int64{"fresh-reserve": {5_000_001, 870_102}},
	}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: &scriptedControllerWires{suffix: "-own-pull"}, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slot}); err != nil || code != ResultCompleted {
		t.Fatalf("own-pull controller outcome=%v error=%v", code, err)
	}
	var offset int64
	if err := store.pool.QueryRow(ctx, `SELECT last_event_id FROM loyal_yield.projection_offsets WHERE consumer_name=$1`, ConsumerName).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	// 3,000,000 arrives while the pull is in flight; then the pull's own outflow.
	inflow, outflow := int64(3_000_000), int64(-5_000_000)
	store.insertIntegrationEvent(t, seeded.TargetID, offset+1, 12_000_000, &inflow, time.Now().Add(-time.Minute))
	if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.balance_sweep_wallet_balance_events
    (event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata, amount_raw, delta_amount_raw, observed_slot,
     observed_at, source, source_commitment, mint, txn_signature)
VALUES ($1, $2, 'itest-wallet', 'itest-wallet-ata', 'itest-wallet-ata', 7000000, $3, 870101, now(), 'itest', 'confirmed', $4, $5)`,
		offset+2, seeded.TargetID, outflow, USDCMint, pull); err != nil {
		t.Fatal(err)
	}
	outcome, err := store.ProjectSurplusLotsOnce(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var open int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(sum(remaining_amount_raw),0) FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id=$1 AND status='open'`, seeded.TargetID).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != inflow || outcome.LotAmountDepletedRaw != 0 {
		t.Fatalf("open surplus %d depleted %d after our own pull, want the %d that arrived untouched", open, outcome.LotAmountDepletedRaw, inflow)
	}
}

// A fleet rebalance moves the vault's holding without touching the position
// pointer (production: 238 active positions named a reserve the vault no
// longer held). The TS executor redirected a fresh claim to the one live
// reserve and wrote the pointer back; the port deposited into the stale one.
func TestControllerRedirectsStalePointerToLiveReserve(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, slot := seedFreshControllerTarget(t, store, "moved")
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.vault_reserve_positions_current SET amount_raw=0, has_value=false WHERE vault_id=$1 AND reserve='fresh-reserve'`, seeded.ManagedVaultID); err != nil {
		t.Fatal(err)
	}
	seedLiveVaultPosition(t, store, seeded, "moved-reserve", "moved-market", 7_000_000, time.Now())
	wallet, custody := "itest-wallet-usdc-moved", "itest-vault-usdc-moved"
	pull, topup := "itest-controller-pull-sig-moved", "itest-controller-topup-sig-moved"
	chain := &scriptedControllerChain{
		balances: map[string]int64{wallet: 9_000_000, custody: 0},
		observations: map[string]AttemptObservation{
			pull:  {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_201)},
			topup: {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_202)},
		},
		receipts: map[string]ReceiptEvidence{
			pull: {Signature: pull, Slot: 870_201, Effects: []ReceiptEffect{
				{TokenAccount: wallet, Mint: USDCMint, PreRaw: 9_000_000, PostRaw: 4_000_000},
				{TokenAccount: custody, Mint: USDCMint, PreRaw: 0, PostRaw: 5_000_000},
			}},
			topup: {Signature: topup, Slot: 870_202, Effects: []ReceiptEffect{
				{TokenAccount: custody, Mint: USDCMint, PreRaw: 5_000_000, PostRaw: 0},
				{TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 10, PostRaw: 5_000_010},
			}},
		},
		positions: map[string][2]int64{"moved-reserve": {12_000_000, 870_202}},
	}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: &scriptedControllerWires{suffix: "-moved"}, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slot}); err != nil || code != ResultCompleted {
		t.Fatalf("moved controller outcome=%v error=%v", code, err)
	}
	var planReserve, pointer string
	if err := store.pool.QueryRow(ctx, `
SELECT claim.autodeposit_deposit_plan->>'reserve', yp.current_reserve
FROM loyal_yield.balance_sweep_lot_claims AS claim
JOIN loyal_yield.balance_sweep_targets AS target ON target.id = claim.target_id
JOIN loyal_yield.user_yield_positions AS yp ON yp.settings = target.settings AND yp.wallet_address = target.wallet AND yp.status = 'active'
WHERE claim.target_id=$1`, seeded.TargetID).Scan(&planReserve, &pointer); err != nil {
		t.Fatal(err)
	}
	if planReserve != "moved-reserve" || pointer != "moved-reserve" {
		t.Fatalf("deposit planned into %q with pointer %q, want the live moved-reserve", planReserve, pointer)
	}
}

// Without one fresh live position there is no destination to trust: the
// claim is released before any pull wire exists.
func TestControllerRefusesPullWhenLiveReserveIsAmbiguous(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, slot := seedFreshControllerTarget(t, store, "ambiguous")
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.vault_reserve_positions_current SET amount_raw=0, has_value=false WHERE vault_id=$1 AND reserve='fresh-reserve'`, seeded.ManagedVaultID); err != nil {
		t.Fatal(err)
	}
	seedLiveVaultPosition(t, store, seeded, "live-a", "market-a", 3_000_000, time.Now())
	seedLiveVaultPosition(t, store, seeded, "live-b", "market-b", 4_000_000, time.Now())
	chain := &scriptedControllerChain{balances: map[string]int64{"itest-wallet-usdc-ambiguous": 9_000_000, "itest-vault-usdc-ambiguous": 0}}
	wires := &scriptedControllerWires{suffix: "-ambiguous"}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: wires, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	code, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slot})
	if code != ResultPreflightBlocked || !errors.Is(err, ErrUnresolvedCurrentReserve) || len(wires.built) != 0 {
		t.Fatalf("ambiguous reserve outcome=%v error=%v wires=%v, want released before any wire", code, err, wires.built)
	}
}

// A vault that holds nothing has no position to fragment: the TS executor
// deposited a pointerless target into its default Earn reserve, and the owner
// extended that to a drained vault. Refusing left these users' deposits waiting.
func TestControllerDepositsEmptyVaultIntoDefaultReserve(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dropPointer bool
		wantPointer string
	}{
		{name: "drained", wantPointer: "fresh-reserve"},
		// current_reserve is NOT NULL: a pointerless target has no position
		// row until settlement records the deposit.
		{name: "pointerless", dropPointer: true, wantPointer: defaultEarnReserve},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := integrationStore(t)
			ctx := context.Background()
			seeded, slot := seedFreshControllerTarget(t, store, tc.name)
			if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.vault_reserve_positions_current SET amount_raw=0, has_value=false WHERE vault_id=$1 AND reserve='fresh-reserve'`, seeded.ManagedVaultID); err != nil {
				t.Fatal(err)
			}
			if tc.dropPointer {
				if _, err := store.pool.Exec(ctx, `DELETE FROM loyal_yield.user_yield_positions AS yp USING loyal_yield.balance_sweep_targets AS target WHERE target.id=$1 AND yp.settings=target.settings AND yp.wallet_address=target.wallet`, seeded.TargetID); err != nil {
					t.Fatal(err)
				}
			}
			wallet, custody := "itest-wallet-usdc-"+tc.name, "itest-vault-usdc-"+tc.name
			pull, topup := "itest-controller-pull-sig-"+tc.name, "itest-controller-topup-sig-"+tc.name
			chain := &scriptedControllerChain{
				balances: map[string]int64{wallet: 9_000_000, custody: 0},
				observations: map[string]AttemptObservation{
					pull:  {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_301)},
					topup: {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_302)},
				},
				receipts: map[string]ReceiptEvidence{
					pull: {Signature: pull, Slot: 870_301, Effects: []ReceiptEffect{
						{TokenAccount: wallet, Mint: USDCMint, PreRaw: 9_000_000, PostRaw: 4_000_000},
						{TokenAccount: custody, Mint: USDCMint, PreRaw: 0, PostRaw: 5_000_000},
					}},
					topup: {Signature: topup, Slot: 870_302, Effects: []ReceiptEffect{
						{TokenAccount: custody, Mint: USDCMint, PreRaw: 5_000_000, PostRaw: 0},
						{TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 10, PostRaw: 5_000_010},
					}},
				},
				positions: map[string][2]int64{defaultEarnReserve: {5_000_000, 870_302}},
			}
			controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: &scriptedControllerWires{suffix: "-" + tc.name}, Facts: testFacts()})
			if err != nil {
				t.Fatal(err)
			}
			if code, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slot}); err != nil || code != ResultCompleted {
				t.Fatalf("%s controller outcome=%v error=%v", tc.name, code, err)
			}
			var planReserve, planMarket, pointer string
			if err := store.pool.QueryRow(ctx, `
SELECT claim.autodeposit_deposit_plan->>'reserve', claim.autodeposit_deposit_plan->>'market', COALESCE(yp.current_reserve, '')
FROM loyal_yield.balance_sweep_lot_claims AS claim
JOIN loyal_yield.balance_sweep_targets AS target ON target.id = claim.target_id
LEFT JOIN loyal_yield.user_yield_positions AS yp ON yp.settings = target.settings AND yp.wallet_address = target.wallet AND yp.status = 'active'
WHERE claim.target_id=$1`, seeded.TargetID).Scan(&planReserve, &planMarket, &pointer); err != nil {
				t.Fatal(err)
			}
			if planReserve != defaultEarnReserve || planMarket != defaultEarnMarket || pointer != tc.wantPointer {
				t.Fatalf("deposit planned into %q/%q with pointer %q, want the default reserve and pointer %q", planReserve, planMarket, pointer, tc.wantPointer)
			}
		})
	}
}
