package autodeposit

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

// wireSHA256 hashes the DECODED wire bytes: OwnSignedWire validates the sha256
// of the exact packet, never of its base64 rendering.
func wireSHA256(base64Wire string) string {
	raw, err := base64.StdEncoding.DecodeString(base64Wire)
	if err != nil {
		panic(fmt.Sprintf("test wire is not base64: %v", err))
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// seedProjectedSurplus drives the projection over one seeded event so the tests
// exercise the real lot/slot rows, never hand-inserted ones.
func seedProjectedSurplus(t *testing.T, store *Store, seeded integrationTarget, eventID, amountRaw int64) {
	t.Helper()
	ctx := context.Background()
	store.insertIntegrationEvent(t, seeded.TargetID, eventID, amountRaw, nil, time.Now().Add(-2*time.Hour))
	if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.balance_sweep_wallet_balances_current
    (target_id, wallet, wallet_usdc_ata, wallet_token_ata, amount_raw, mint, observed_slot, source, source_commitment)
VALUES ($1, 'itest-wallet', 'itest-wallet-usdc', 'itest-wallet-usdc', $2, $3, 1, 'itest', 'confirmed')
ON CONFLICT DO NOTHING`, seeded.TargetID, amountRaw, USDCMint); err != nil {
		t.Fatalf("seed current wallet balance: %v", err)
	}
	outcome, err := store.ProjectSurplusLotsOnce(ctx, 100)
	if err != nil {
		t.Fatalf("project surplus: %v", err)
	}
	if outcome.LotsCreated != 1 {
		t.Fatalf("projection created %d lots, want 1", outcome.LotsCreated)
	}
}

// TestClaimConsumptionRestoresAndStaleLeaseReleases pins the SQL custody
// cycle: a claim consumes lots and selects the slot, a release restores the
// lots (bounded by their original amounts) and fails the slot, and a claim
// lease that expired transfers to a second executor while a live one does not.
func TestClaimConsumptionRestoresAndStaleLeaseReleases(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded := seedIntegrationTarget(t, store, "custody")
	seedProjectedSurplus(t, store, seeded, 9_300_001, 9_000_000)

	var slotID, lotID int64
	var lotRemaining int64
	if err := store.pool.QueryRow(ctx, `
SELECT slot.id, lot.id, lot.remaining_amount_raw
FROM loyal_yield.balance_sweep_scheduled_slots AS slot
JOIN loyal_yield.balance_sweep_surplus_lots AS lot ON lot.scheduled_slot_id = slot.id
WHERE slot.target_id = $1`, seeded.TargetID).Scan(&slotID, &lotID, &lotRemaining); err != nil {
		t.Fatalf("read projected lot: %v", err)
	}

	// Consume: 5M claim of the 5M surplus (9M balance - 4M floor).
	claimToken := "itest-claim-consume"
	spendCap := int64(100_000_000)
	outcome, err := store.ClaimEligibleLotsOnce(ctx, seeded.TargetID, claimToken, &slotID, 9_000_000, 4_000_000, &spendCap, &spendCap)
	if err != nil {
		t.Fatalf("claim eligible lots: %v", err)
	}
	if outcome.Status != ClaimSelected || outcome.AmountRaw != 5_000_000 || len(outcome.Lots) != 1 {
		t.Fatalf("claim outcome %+v, want selected 5000000 from one lot", outcome)
	}
	var consumed int64
	var status string
	if err := store.pool.QueryRow(ctx, `
SELECT remaining_amount_raw, status::text FROM loyal_yield.balance_sweep_surplus_lots WHERE id = $1`,
		lotID).Scan(&consumed, &status); err != nil {
		t.Fatalf("read claimed lot: %v", err)
	}
	if consumed != 0 || status != "consumed" {
		t.Fatalf("claimed lot remaining=%d status=%s, want fully consumed", consumed, status)
	}
	var slotStatus string
	if err := store.pool.QueryRow(ctx, `
SELECT status::text FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = $1`, slotID).
		Scan(&slotStatus); err != nil {
		t.Fatalf("read claimed slot: %v", err)
	}
	if slotStatus != "selected" {
		t.Fatalf("claimed slot status %s, want selected", slotStatus)
	}

	// An expired lease cannot block a second executor; a live lease can.
	acquired, err := store.AcquireClaimLease(ctx, claimToken, seeded.TargetID, "lease-one")
	if err != nil || !acquired {
		t.Fatalf("first lease acquired=%v err=%v", acquired, err)
	}
	acquired, err = store.AcquireClaimLease(ctx, claimToken, seeded.TargetID, "lease-two")
	if err != nil || acquired {
		t.Fatalf("live lease must not transfer: acquired=%v err=%v", acquired, err)
	}
	if _, err := store.pool.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_lot_claims
SET autodeposit_executor_lease_expires_at = now() - interval '1 second'
WHERE claim_token = $1`, claimToken); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	acquired, err = store.AcquireClaimLease(ctx, claimToken, seeded.TargetID, "lease-two")
	if err != nil || !acquired {
		t.Fatalf("expired lease must transfer: acquired=%v err=%v", acquired, err)
	}
	if err := store.RenewClaimLease(ctx, claimToken, "lease-one"); err == nil {
		t.Fatal("the displaced lease must not renew")
	}

	// Restore: releasing the unspent claim gives the surplus back and fails
	// the slot.
	released, err := store.ReleaseClaimOnce(ctx, claimToken, "lease-two")
	if err != nil {
		t.Fatalf("release claim: %v", err)
	}
	if released.Status != ClaimReleased {
		t.Fatalf("release status %s, want released", released.Status)
	}
	if err := store.pool.QueryRow(ctx, `
SELECT remaining_amount_raw, status::text FROM loyal_yield.balance_sweep_surplus_lots WHERE id = $1`,
		lotID).Scan(&consumed, &status); err != nil {
		t.Fatalf("read restored lot: %v", err)
	}
	if consumed != 5_000_000 || status != "open" {
		t.Fatalf("restored lot remaining=%d status=%s, want the full 5000000 open", consumed, status)
	}
	if err := store.pool.QueryRow(ctx, `
SELECT status::text FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = $1`, slotID).
		Scan(&slotStatus); err != nil {
		t.Fatalf("read released slot: %v", err)
	}
	if slotStatus != "failed" {
		t.Fatalf("released slot status %s, want failed", slotStatus)
	}
}

// TestExactWireAndPlanImmutability pins the durable identity contract: the
// first frozen plan wins, the first persisted wire wins, a wire that fails the
// shared packet contract never becomes durable state, and a broadcast whose
// recorded identity disagrees is refused.
func TestExactWireAndPlanImmutability(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded := seedIntegrationTarget(t, store, "immutable")
	seedProjectedSurplus(t, store, seeded, 9_400_001, 9_000_000)

	var slotID int64
	if err := store.pool.QueryRow(ctx, `
SELECT slot.id FROM loyal_yield.balance_sweep_scheduled_slots AS slot
WHERE slot.target_id = $1`, seeded.TargetID).Scan(&slotID); err != nil {
		t.Fatalf("read scheduled slot: %v", err)
	}
	claimToken := "itest-claim-immutable"
	spendCap := int64(100_000_000)
	outcome, err := store.ClaimEligibleLotsOnce(ctx, seeded.TargetID, claimToken, &slotID, 9_000_000, 4_000_000, &spendCap, &spendCap)
	if err != nil || outcome.Status != ClaimSelected {
		t.Fatalf("claim outcome %+v err=%v, want selected", outcome, err)
	}
	if acquired, err := store.AcquireClaimLease(ctx, claimToken, seeded.TargetID, "lease-immutable"); err != nil || !acquired {
		t.Fatalf("lease acquired=%v err=%v", acquired, err)
	}

	plan := DepositPlan{
		Version:       DepositPlanVersion,
		AmountRaw:     outcome.AmountRaw,
		Reserve:       "reserve-a",
		Market:        "market-a",
		LiquidityMint: USDCMint,
		Target: DepositPlanTarget{
			ID:                 seeded.TargetID,
			ManagedVaultID:     seeded.ManagedVaultID,
			Settings:           fmt.Sprintf("itest-settings-immutable"),
			VaultIndex:         1,
			Wallet:             "itest-wallet",
			WalletUsdcAta:      "itest-wallet-usdc",
			WalletTokenAta:     "itest-wallet-usdc",
			VaultPubkey:        fmt.Sprintf("itest-vault-immutable"),
			VaultUsdcAta:       "itest-vault-usdc",
			VaultTokenAta:      "itest-vault-usdc",
			TokenMint:          USDCMint,
			RoutePolicyAccount: "itest-policy-immutable",
			RoutePolicySeed:    7,
		},
	}
	frozen, err := store.FreezeDepositPlan(ctx, claimToken, "lease-immutable", plan)
	if err != nil {
		t.Fatalf("freeze plan: %v", err)
	}
	// A repoint at a different reserve must keep the first frozen destination.
	repointed := plan
	repointed.Reserve = "reserve-b"
	if _, err := store.FreezeDepositPlan(ctx, claimToken, "lease-immutable", repointed); err == nil {
		t.Fatal("a conflicting plan freeze must be refused")
	}
	if frozen.Reserve != "reserve-a" {
		t.Fatalf("frozen reserve moved to %q, want the first plan's reserve-a", frozen.Reserve)
	}
	var storedPlanRaw []byte
	if err := store.pool.QueryRow(ctx, `
SELECT autodeposit_deposit_plan FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = $1`,
		claimToken).Scan(&storedPlanRaw); err != nil {
		t.Fatalf("read frozen plan: %v", err)
	}
	var storedPlan DepositPlan
	if err := json.Unmarshal(storedPlanRaw, &storedPlan); err != nil {
		t.Fatalf("parse frozen plan: %v", err)
	}
	if storedPlan.Reserve != "reserve-a" {
		t.Fatalf("durable plan reserve %q, want the first frozen plan", storedPlan.Reserve)
	}

	// A wire whose bytes do not match its digest is refused before SQL.
	forged := PreparedAttempt{
		ClaimToken:               claimToken,
		TargetID:                 seeded.TargetID,
		ScheduledSlotID:          slotID,
		OperationKind:            OperationPull,
		AmountRaw:                outcome.AmountRaw,
		SourcePreBalanceRaw:      9_000_000,
		DestinationPreBalanceRaw: 0,
		ProtectionFloorRaw:       ptrInt64(4_000_000),
		Signature:                "itest-sig-forged",
		SignedTransactionBase64:  base64.StdEncoding.EncodeToString([]byte("exact-pull-wire-bytes")),
		SignedTransactionSHA256:  wireSHA256(base64.StdEncoding.EncodeToString([]byte("other-bytes"))),
		RecentBlockhash:          "itest-blockhash",
		LastValidBlockHeight:     500,
	}
	if _, err := store.PersistPreparedAttempt(ctx, forged, "lease-immutable"); err == nil {
		t.Fatal("a wire whose digest disagrees with its bytes must never be persisted")
	}

	wire := base64.StdEncoding.EncodeToString([]byte("exact-pull-wire-bytes"))
	prepared := forged
	prepared.Signature = "itest-sig-pull-immutable"
	prepared.SignedTransactionSHA256 = wireSHA256(wire)
	attempt, err := store.PersistPreparedAttempt(ctx, prepared, "lease-immutable")
	if err != nil {
		t.Fatalf("persist prepared attempt: %v", err)
	}
	if attempt.State != AttemptPrepared || attempt.Signature != prepared.Signature {
		t.Fatalf("persisted attempt %+v, want the exact prepared wire", attempt)
	}

	// A second, different wire for the same operation can never replace the
	// persisted one: an existing active attempt wins, but only while this
	// executor still owns the claim lease (guarded_claim EXISTS).
	replacement := prepared
	replacement.Signature = "itest-sig-pull-replacement"
	replacement.SignedTransactionBase64 = base64.StdEncoding.EncodeToString([]byte("different-wire"))
	replacement.SignedTransactionSHA256 = wireSHA256(replacement.SignedTransactionBase64)
	still, err := store.PersistPreparedAttempt(ctx, replacement, "lease-immutable")
	if err != nil {
		t.Fatalf("persist over an existing active attempt: %v", err)
	}
	if still.Signature != prepared.Signature || still.SignedTransactionBase64 != wire {
		t.Fatalf("persisted wire changed to %s; the exact first wire must be immutable", still.Signature)
	}

	// The durable broadcast intent requires the exact recorded identity.
	if _, err := store.RecordAttemptBroadcast(ctx, DurableAttempt{
		ID: attempt.ID, ClaimToken: claimToken, OperationKind: OperationPull,
		Signature: prepared.Signature, SignedTransactionSHA256: strings.Repeat("f", 64),
	}, "lease-immutable"); err == nil {
		t.Fatal("a broadcast whose wire digest disagrees with the persisted row must be refused")
	}
	recorded, err := store.RecordAttemptBroadcast(ctx, DurableAttempt{
		ID: attempt.ID, ClaimToken: claimToken, OperationKind: OperationPull,
		Signature: prepared.Signature, SignedTransactionSHA256: wireSHA256(wire),
	}, "lease-immutable")
	if err != nil {
		t.Fatalf("record broadcast intent: %v", err)
	}
	if recorded.State != AttemptSubmitted || recorded.BroadcastCount != 1 {
		t.Fatalf("recorded intent state=%s broadcasts=%d, want submitted/1", recorded.State, recorded.BroadcastCount)
	}
	// The recorded wire columns never moved.
	var persistedWire string
	if err := store.pool.QueryRow(ctx, `
SELECT signed_transaction_base64 FROM loyal_yield.balance_sweep_transaction_attempts WHERE id = $1`,
		attempt.ID).Scan(&persistedWire); err != nil {
		t.Fatalf("re-read persisted wire: %v", err)
	}
	if persistedWire != wire {
		t.Fatal("the recorded broadcast rewrote the immutable wire bytes")
	}
}

// scriptedControllerChain replays observations per signature for the
// controller's recovery path.
type scriptedControllerChain struct {
	balances     map[string]int64
	observations map[string]AttemptObservation
	receipts     map[string]ReceiptEvidence
	positions    map[string][2]int64
	simulateErr  error
	broadcastErr error
}

func (s *scriptedControllerChain) ConfirmedTokenBalanceRaw(ctx context.Context, tokenAccount, authority string) (int64, error) {
	amount, known := s.balances[tokenAccount]
	if !known {
		return 0, errors.New("scripted token account is not observed")
	}
	return amount, nil
}

func (s *scriptedControllerChain) RemainingDelegationAllowanceRaw(ctx context.Context, delegation string, identity DelegationIdentity) (int64, error) {
	return 1 << 40, nil
}

func (s *scriptedControllerChain) LatestBlockhash(ctx context.Context) (string, int64, error) {
	return "itest-controller-blockhash", 900, nil
}

func (s *scriptedControllerChain) Observe(ctx context.Context, attempt DurableAttempt) (AttemptObservation, error) {
	if strings.HasPrefix(attempt.Signature, "itest-controller-") && attempt.BroadcastCount == 0 {
		return AttemptObservation{State: AttemptUnknown}, nil
	}
	observation, seen := s.observations[attempt.Signature]
	if !seen {
		return AttemptObservation{State: AttemptUnknown}, nil
	}
	return observation, nil
}

func (s *scriptedControllerChain) BroadcastExact(ctx context.Context, attempt DurableAttempt) (string, error) {
	if attempt.BroadcastCount == 0 {
		return "", errors.New("broadcast preceded durable intent")
	}
	if s.broadcastErr != nil {
		return "", s.broadcastErr
	}
	for _, effect := range s.receipts[attempt.Signature].Effects {
		s.balances[effect.TokenAccount] = effect.PostRaw
	}
	return attempt.Signature, nil
}

func (s *scriptedControllerChain) ConfirmedReceipt(ctx context.Context, signature string) (ReceiptEvidence, error) {
	receipt, seen := s.receipts[signature]
	if !seen {
		return ReceiptEvidence{}, errors.New("no scripted receipt")
	}
	return receipt, nil
}

func (s *scriptedControllerChain) ReadAccounts(ctx context.Context, addresses []string) (int64, []backyard.ConfirmedAccount, error) {
	return 1, nil, nil
}

func (s *scriptedControllerChain) SimulateExact(ctx context.Context, attempt DurableAttempt) error {
	return s.simulateErr
}

func (s *scriptedControllerChain) ConfirmedVaultPositionRaw(ctx context.Context, plan DepositPlan, route TopUpRoute) (int64, int64, error) {
	if position, seen := s.positions[plan.Reserve]; seen {
		return position[0], position[1], nil
	}
	return plan.AmountRaw, 870_002, nil
}

type scriptedControllerWires struct {
	built      []string
	suffix     string
	routeErr   error
	proofError error
}

func (s *scriptedControllerWires) BuildPull(ctx context.Context, request PullWireRequest) (BuiltWire, error) {
	s.built = append(s.built, "pull")
	return BuiltWire{Signature: "itest-controller-pull-sig" + s.suffix, SignedTransactionBase64: "cHVsbC13aXJl",
		SignedTransactionSHA256: wireSHA256("cHVsbC13aXJl"), RecentBlockhash: request.RecentBlockhash,
		LastValidBlockHeight: request.LastValidBlockHeight}, nil
}

func (s *scriptedControllerWires) ConfirmTopUpRoute(ctx context.Context, plan DepositPlan) (TopUpRoute, error) {
	if s.routeErr != nil {
		return TopUpRoute{}, s.routeErr
	}
	return TopUpRoute{Position: fleet.KaminoPositionAccounts{LiquiditySupply: "itest-liquidity-supply"},
		Obligation: "itest-obligation"}, nil
}

func (s *scriptedControllerWires) BuildTopUp(ctx context.Context, request TopUpWireRequest) (BuiltWire, error) {
	s.built = append(s.built, "top_up")
	return BuiltWire{Signature: "itest-controller-topup-sig" + s.suffix, SignedTransactionBase64: "dG9wdXAtd2lyZQ==",
		SignedTransactionSHA256: wireSHA256("dG9wdXAtd2lyZQ=="), RecentBlockhash: request.RecentBlockhash,
		LastValidBlockHeight: request.LastValidBlockHeight}, nil
}

func (s *scriptedControllerWires) ProveTopUpWire(plan DepositPlan, attempt DurableAttempt, route TopUpRoute) error {
	return s.proofError
}

// seedRecoveryClaim installs a selected claim, slot, frozen plan and prepared
// pull attempt, the exact state a crashed executor leaves behind.
func seedRecoveryClaim(t *testing.T, store *Store, suffix string) (integrationTarget, string, string, int64) {
	t.Helper()
	ctx := context.Background()
	seeded := seedIntegrationTarget(t, store, suffix)
	claimToken := "itest-claim-" + suffix
	leaseToken := "itest-lease-" + suffix
	amount := int64(2_000_000)
	plan := DepositPlan{
		Version:       DepositPlanVersion,
		AmountRaw:     amount,
		Reserve:       "reserve-" + suffix,
		Market:        "market-" + suffix,
		LiquidityMint: USDCMint,
		Target: DepositPlanTarget{
			ID:                 seeded.TargetID,
			ManagedVaultID:     seeded.ManagedVaultID,
			Settings:           "itest-settings-" + suffix,
			VaultIndex:         1,
			Wallet:             "itest-wallet",
			WalletUsdcAta:      "itest-wallet-usdc",
			WalletTokenAta:     "itest-wallet-usdc",
			VaultPubkey:        "itest-vault-" + suffix,
			VaultUsdcAta:       "itest-vault-usdc-" + suffix,
			VaultTokenAta:      "itest-vault-usdc-" + suffix,
			TokenMint:          USDCMint,
			RoutePolicyAccount: "itest-policy-" + suffix,
			RoutePolicySeed:    7,
		},
	}
	planRaw, err := MarshalDepositPlan(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.balance_sweep_lot_claims (claim_token, target_id, amount_raw, status, stale_check_event_id)
VALUES ($1, $2, $3, 'selected', 0)`, claimToken, seeded.TargetID, amount); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.balance_sweep_scheduled_slots (target_id, token_mint, eligible_after, status, claim_token)
VALUES ($1, $2, now() - interval '1 hour', 'selected', $3) RETURNING id`,
		seeded.TargetID, USDCMint, claimToken); err != nil {
		t.Fatalf("seed slot: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_lot_claims
SET autodeposit_deposit_plan = $2::jsonb,
    autodeposit_executor_lease_token = $3,
    autodeposit_executor_lease_expires_at = now() + interval '10 minutes'
WHERE claim_token = $1`, claimToken, string(planRaw), leaseToken); err != nil {
		t.Fatalf("seed frozen plan: %v", err)
	}
	var slotID int64
	if err := store.pool.QueryRow(ctx, `
SELECT id FROM loyal_yield.balance_sweep_scheduled_slots WHERE claim_token = $1`, claimToken).Scan(&slotID); err != nil {
		t.Fatalf("read seeded slot: %v", err)
	}
	pullWire := "prepared-pull-wire"
	if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.balance_sweep_transaction_attempts
    (claim_token, target_id, scheduled_slot_id, operation_kind, attempt_number,
     amount_raw, source_pre_balance_raw, destination_pre_balance_raw, signature,
     signed_transaction_base64, signed_transaction_sha256, recent_blockhash,
     last_valid_block_height, attempt_state)
VALUES ($1, $2, $3, 'pull', 1, $4, 9_000_000, 0, $5, $6, $7, 'bh', 900, 'prepared')`,
		claimToken, seeded.TargetID, slotID, amount,
		"itest-"+suffix+"-pull-sig",
		base64.StdEncoding.EncodeToString([]byte(pullWire)), wireSHA256(base64.StdEncoding.EncodeToString([]byte(pullWire)))); err != nil {
		t.Fatalf("seed pull attempt: %v", err)
	}
	// The controller takes over the seeded lease itself.
	if _, err := store.pool.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_lot_claims
SET autodeposit_executor_lease_expires_at = now() - interval '1 second'
WHERE claim_token = $1`, claimToken); err != nil {
		t.Fatalf("expire seeded lease: %v", err)
	}
	return seeded, claimToken, leaseToken, slotID
}

// TestControllerRecoversConfirmedPull runs the recovery path against real SQL
// rows: a prepared pull whose wire already holds custody settles, the exact
// pull effects are verified, the top-up leg is built and persisted exactly
// once, and the shared finalization completes the claim atomically.
func TestControllerRecoversConfirmedPull(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, claimToken, _, slotID := seedRecoveryClaim(t, store, "recover")
	amount := int64(2_000_000)
	custody := "itest-vault-usdc-recover"

	chain := &scriptedControllerChain{
		balances: map[string]int64{custody: amount},
		observations: map[string]AttemptObservation{
			"itest-recover-pull-sig":     {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_001)},
			"itest-controller-topup-sig": {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_002)},
		},
		receipts: map[string]ReceiptEvidence{
			"itest-recover-pull-sig": {
				Signature: "itest-recover-pull-sig", Slot: 870_001,
				Effects: []ReceiptEffect{
					{TokenAccount: "itest-wallet-usdc", Mint: USDCMint, PreRaw: amount, PostRaw: 0},
					{TokenAccount: custody, Mint: USDCMint, PreRaw: 0, PostRaw: amount},
				},
			},
			"itest-controller-topup-sig": {
				Signature: "itest-controller-topup-sig", Slot: 870_002,
				Effects: []ReceiptEffect{
					{TokenAccount: custody, Mint: USDCMint, PreRaw: amount, PostRaw: 0},
					{TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 0, PostRaw: amount},
				},
			},
		},
	}
	wires := &scriptedControllerWires{}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: wires, LeaseRenewWindow: time.Second})
	if err != nil {
		t.Fatalf("build controller: %v", err)
	}

	target := ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slotID, ClaimToken: claimToken}
	result, err := controller.Execute(ctx, target)
	if err != nil {
		t.Fatalf("execute recovery: %v", err)
	}

	// The top-up wire was built exactly once and persisted as the durable
	// second leg, linked to the execution the pull owns.
	if len(wires.built) != 1 || wires.built[0] != "top_up" {
		t.Fatalf("built wires %v, want exactly one top_up", wires.built)
	}
	var topUpExecutionID *int64
	var topUpState string
	if err := store.pool.QueryRow(ctx, `
SELECT execution_id, attempt_state::text FROM loyal_yield.balance_sweep_transaction_attempts
WHERE claim_token = $1 AND operation_kind = 'top_up'`, claimToken).Scan(&topUpExecutionID, &topUpState); err != nil {
		t.Fatalf("read top-up attempt: %v", err)
	}
	if topUpState != string(AttemptConfirmed) || topUpExecutionID == nil {
		t.Fatalf("top-up state=%s execution=%v, want one confirmed durable attempt linked to its execution", topUpState, topUpExecutionID)
	}
	var pullExecutionID *int64
	if err := store.pool.QueryRow(ctx, `
SELECT execution_id FROM loyal_yield.balance_sweep_transaction_attempts
WHERE claim_token = $1 AND operation_kind = 'pull'`, claimToken).Scan(&pullExecutionID); err != nil {
		t.Fatalf("read pull attempt: %v", err)
	}
	if pullExecutionID != nil {
		t.Fatalf("pull immutable execution_id %v, want NULL; top-up owns %v", pullExecutionID, topUpExecutionID)
	}

	var claimStatus string
	if err := store.pool.QueryRow(ctx, `SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = $1`, claimToken).Scan(&claimStatus); err != nil {
		t.Fatal(err)
	}
	if result != ResultCompleted || claimStatus != "executed" {
		t.Fatalf("recovery outcome %v claim %s, want completed/executed with registered accounting schema", result, claimStatus)
	}
}

// TestControllerRefusesWrongTopUpReceipt pins the receipt gates: a top-up
// receipt whose custody delta, mint, signature or slot disagrees with the
// persisted attempt is ambiguous, and the claim is never completed over it.
func TestControllerRefusesWrongTopUpReceipt(t *testing.T) {
	for name, mutate := range map[string]func(receipt ReceiptEvidence, topUp AttemptObservation) (ReceiptEvidence, AttemptObservation){
		"wrong-custody-delta": func(receipt ReceiptEvidence, topUp AttemptObservation) (ReceiptEvidence, AttemptObservation) {
			receipt.Effects[0].PostRaw = receipt.Effects[0].PreRaw // custody did not move
			return receipt, topUp
		},
		"wrong-mint": func(receipt ReceiptEvidence, topUp AttemptObservation) (ReceiptEvidence, AttemptObservation) {
			receipt.Effects[0].Mint = "not-usdc"
			return receipt, topUp
		},
		"early-slot": func(receipt ReceiptEvidence, topUp AttemptObservation) (ReceiptEvidence, AttemptObservation) {
			topUp.ConfirmedSlot = ptrInt64(870_003) // receipt slot 870_002 precedes it
			return receipt, topUp
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := integrationStore(t)
			ctx := context.Background()
			seeded, claimToken, _, slotID := seedRecoveryClaim(t, store, "reject")
			amount := int64(2_000_000)
			custody := "itest-vault-usdc-reject"
			observation := AttemptObservation{State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_002)}
			receipt := ReceiptEvidence{
				Signature: "itest-controller-topup-sig", Slot: 870_002,
				Effects: []ReceiptEffect{
					{TokenAccount: custody, Mint: USDCMint, PreRaw: amount, PostRaw: 0},
					{TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 0, PostRaw: amount},
				},
			}
			receipt, observation = mutate(receipt, observation)
			chain := &scriptedControllerChain{
				balances: map[string]int64{custody: amount},
				observations: map[string]AttemptObservation{
					"itest-reject-pull-sig":      {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_001)},
					"itest-controller-topup-sig": observation,
				},
				receipts: map[string]ReceiptEvidence{
					"itest-reject-pull-sig": {
						Signature: "itest-reject-pull-sig", Slot: 870_001,
						Effects: []ReceiptEffect{
							{TokenAccount: "itest-wallet-usdc", Mint: USDCMint, PreRaw: amount, PostRaw: 0},
							{TokenAccount: custody, Mint: USDCMint, PreRaw: 0, PostRaw: amount},
						},
					},
					"itest-controller-topup-sig": receipt,
				},
			}
			wires := &scriptedControllerWires{}
			controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: wires, LeaseRenewWindow: time.Second})
			if err != nil {
				t.Fatalf("build controller: %v", err)
			}
			result, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slotID, ClaimToken: claimToken})
			if err == nil {
				t.Fatal("contradictory receipt must report an error")
			}
			if result != ResultTransactionEffectAmbig {
				t.Fatalf("outcome %v, want %s: a disagreeing top-up receipt is ambiguous", result, ResultTransactionEffectAmbig)
			}
			var claimStatus string
			if err := store.pool.QueryRow(ctx, `
SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = $1`,
				claimToken).Scan(&claimStatus); err != nil {
				t.Fatalf("read claim: %v", err)
			}
			if claimStatus != "selected" {
				t.Fatalf("claim status %s, want selected: an ambiguous receipt never completes a claim", claimStatus)
			}
		})
	}
}

// TestControllerSimulationFailureKeepsCustodyClaimed pins the exact-simulation
// gate: the persisted top-up wire is simulated before broadcast, and a
// simulation error leaves the immutable wire prepared and unsent while the
// pulled custody stays claimed.
func TestControllerSimulationFailureKeepsCustodyClaimed(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, claimToken, _, slotID := seedRecoveryClaim(t, store, "simulate")
	amount := int64(2_000_000)
	custody := "itest-vault-usdc-simulate"
	chain := &scriptedControllerChain{
		balances: map[string]int64{custody: amount},
		observations: map[string]AttemptObservation{
			"itest-simulate-pull-sig": {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_001)},
		},
		receipts: map[string]ReceiptEvidence{
			"itest-simulate-pull-sig": {
				Signature: "itest-simulate-pull-sig", Slot: 870_001,
				Effects: []ReceiptEffect{
					{TokenAccount: "itest-wallet-usdc", Mint: USDCMint, PreRaw: amount, PostRaw: 0},
					{TokenAccount: custody, Mint: USDCMint, PreRaw: 0, PostRaw: amount},
				},
			},
		},
		simulateErr: errors.New("scripted simulation failure"),
	}
	wires := &scriptedControllerWires{}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: wires, LeaseRenewWindow: time.Second})
	if err != nil {
		t.Fatalf("build controller: %v", err)
	}
	result, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slotID, ClaimToken: claimToken})
	if !errors.Is(err, chain.simulateErr) {
		t.Fatalf("simulation error = %v", err)
	}
	if result != ResultDependencyUnavailable {
		t.Fatalf("result %v, want %s", result, ResultDependencyUnavailable)
	}
	var topUpState string
	if err := store.pool.QueryRow(ctx, `
SELECT attempt_state::text FROM loyal_yield.balance_sweep_transaction_attempts
WHERE claim_token = $1 AND operation_kind = 'top_up'`, claimToken).Scan(&topUpState); err != nil {
		t.Fatalf("read top-up attempt: %v", err)
	}
	if topUpState != string(AttemptPrepared) {
		t.Fatalf("top-up state %s, want prepared after simulation dependency failure", topUpState)
	}
	var claimStatus string
	if err := store.pool.QueryRow(ctx, `
SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = $1`,
		claimToken).Scan(&claimStatus); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if claimStatus != "selected" {
		t.Fatalf("claim status %s, want selected: pulled custody is never released while only the deposit failed", claimStatus)
	}
}
