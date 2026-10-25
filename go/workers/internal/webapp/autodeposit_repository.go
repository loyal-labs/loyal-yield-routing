package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Autodeposit setup persistence, ported from
// web/apps/web/src/lib/yield-optimization/earn-autodeposit-repository.server.ts
// (findTargetForAutodepositSetup :298, assertTargetCanResumeAutodepositSetup
// :317, targetValuesFromSetup :1375, recordAutodepositSetupIntent :1501) and
// the bootstrap scheduler (:989) with ensureScheduledAutodepositSlot (:457)
// and findPendingEarnAutodepositScheduledSweeps (:768). Released clients keep
// calling setup confirmation after the transaction lands: persist only the
// user's configuration; LaserStream owns chain_status, signatures, observed
// slots, policy rows and the delegation projection.
type autodepositTargetRow struct {
	ID                      int64
	Active                  bool
	BalanceSweepPolicyID    *int64
	LifecycleStatus         string
	PolicyAccount           string
	RecurringDelegation     *string
	WalletBalanceFloorRaw   *uint64
	Settings                string
	Wallet                  string
	VaultIndex              int32
	VaultPubkey             string
	WalletUsdcAta           string
	VaultUsdcAta            string
	TokenMint               string
	WalletTokenAta          string
	VaultTokenAta           string
	SubscriptionAuthority   *string
	LastSeenSignature       string
	StartTimestamp          uint64
	RecurringDelegationNull bool
	RecurringDelegationNu64 uint64
	DelegatedSigners        []string
}

// The targets table's active flag is desired_active and its lifecycle column
// is chain_status (migrations/yield/0059_autodeposit_single_target_state.sql:6).
const autodepositTargetColumns = `id, desired_active, balance_sweep_policy_id, chain_status, policy_account, recurring_delegation, wallet_balance_floor_raw,
	settings, wallet, vault_index, vault_pubkey, wallet_usdc_ata, vault_usdc_ata, token_mint, wallet_token_ata, vault_token_ata,
	subscription_authority, last_seen_signature, start_timestamp, recurring_delegation_nonce, delegated_signers`

func scanAutodepositTarget(row pgx.Row) (autodepositTargetRow, error) {
	var target autodepositTargetRow
	var nonce *int64
	err := row.Scan(
		&target.ID, &target.Active, &target.BalanceSweepPolicyID, &target.LifecycleStatus, &target.PolicyAccount,
		&target.RecurringDelegation, &target.WalletBalanceFloorRaw, &target.Settings, &target.Wallet,
		&target.VaultIndex, &target.VaultPubkey, &target.WalletUsdcAta, &target.VaultUsdcAta,
		&target.TokenMint, &target.WalletTokenAta, &target.VaultTokenAta,
		&target.SubscriptionAuthority, &target.LastSeenSignature, &target.StartTimestamp, &nonce,
		&target.DelegatedSigners,
	)
	if err != nil {
		return autodepositTargetRow{}, err
	}
	target.RecurringDelegationNull = nonce == nil
	if nonce != nil {
		target.RecurringDelegationNu64 = uint64(*nonce)
	}
	return target, nil
}

func (h earnRoutes) findAutodepositTargetByPolicy(ctx context.Context, policyAccount string) (*autodepositTargetRow, error) {
	row := h.YieldDB.QueryRow(ctx,
		`SELECT `+autodepositTargetColumns+` FROM loyal_yield.balance_sweep_targets WHERE policy_account = $1 LIMIT 1`,
		policyAccount)
	target, err := scanAutodepositTarget(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &target, nil
}

func (h earnRoutes) findAutodepositTargetByDelegation(ctx context.Context, recurringDelegation string) (*autodepositTargetRow, error) {
	row := h.YieldDB.QueryRow(ctx,
		`SELECT `+autodepositTargetColumns+` FROM loyal_yield.balance_sweep_targets WHERE recurring_delegation = $1 LIMIT 1`,
		recurringDelegation)
	target, err := scanAutodepositTarget(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &target, nil
}

func (h earnRoutes) findAutodepositTargetForSetup(ctx context.Context, policyAccount, recurringDelegation string) (*autodepositTargetRow, error) {
	byPolicy, err := h.findAutodepositTargetByPolicy(ctx, policyAccount)
	if err != nil || byPolicy != nil {
		return byPolicy, err
	}
	return h.findAutodepositTargetByDelegation(ctx, recurringDelegation)
}

func assertAutodepositTargetCanResume(existing autodepositTargetRow, in autodepositSetupInput) error {
	var mismatches []string
	add := func(mismatch bool, label string) {
		if mismatch {
			mismatches = append(mismatches, label)
		}
	}
	add(existing.Settings != in.Settings, "settings")
	add(existing.Wallet != in.WalletAddress, "wallet")
	add(int(existing.VaultIndex) != in.VaultIndex, "vaultIndex")
	add(existing.VaultPubkey != in.VaultPubkey, "vaultPubkey")
	add(existing.WalletUsdcAta != in.WalletUsdcAta, "walletUsdcAta")
	add(existing.VaultUsdcAta != in.VaultUsdcAta, "vaultUsdcAta")
	add(existing.TokenMint != in.LiquidityMint, "tokenMint")
	add(existing.WalletTokenAta != in.WalletUsdcAta, "walletTokenAta")
	add(existing.VaultTokenAta != in.VaultUsdcAta, "vaultTokenAta")
	add(existing.SubscriptionAuthority != nil && *existing.SubscriptionAuthority != in.SubscriptionAuthority, "subscriptionAuthority")
	add(existing.RecurringDelegation != nil && *existing.RecurringDelegation != in.RecurringDelegation, "recurringDelegation")
	add(!existing.RecurringDelegationNull && existing.RecurringDelegationNu64 != in.Nonce, "recurringDelegationNonce")
	if len(mismatches) > 0 {
		return fmt.Errorf("Existing autodeposit setup target does not match confirmed setup metadata: %s.", strings.Join(mismatches, ", "))
	}
	return nil
}

func (h earnRoutes) recordAutodepositSetupIntent(ctx context.Context, in autodepositSetupInput) (autodepositTargetRow, error) {
	if in.PolicyAccount == "" || in.PolicySeed == 0 {
		return autodepositTargetRow{}, errors.New("Confirmed autodeposit setup has no policy.")
	}
	now := time.Now()
	existing, err := h.findAutodepositTargetForSetup(ctx, in.PolicyAccount, in.RecurringDelegation)
	if err != nil {
		return autodepositTargetRow{}, err
	}
	if existing != nil {
		if err := assertAutodepositTargetCanResume(*existing, in); err != nil {
			return autodepositTargetRow{}, err
		}
		if existing.LifecycleStatus == "closed" {
			return autodepositTargetRow{}, errors.New("Closed autodeposit targets cannot be reactivated. Create a new autodeposit policy with a new policy seed.")
		}
	}

	row := h.YieldDB.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_targets (
		desired_active, authority, balance_sweep_policy_id, chain_observation_slot, chain_status, cluster,
		delegated_signers, first_seen_at, last_seen_at, last_seen_signature, last_seen_slot,
		max_amount_per_period, period_length_seconds,
		policy_account, policy_confirmed_slot, policy_seed, policy_signature,
		recurring_delegation, recurring_delegation_confirmed_slot,
		recurring_delegation_expiry_timestamp, recurring_delegation_nonce, recurring_delegation_signature,
		settings, start_timestamp, subscription_authority, threshold, token_mint,
		vault_index, vault_token_ata, vault_pubkey, vault_usdc_ata, wallet,
		wallet_balance_floor_raw, wallet_token_ata, wallet_usdc_ata
	) VALUES (
		true, $1, NULL, 0, 'pending', $2,
		ARRAY[$3::text], $4, $4, $5, 0,
		$6, $7,
		$8, NULL, $9, NULL,
		$10, NULL,
		$11, $12, NULL,
		$13, $14, $15, 1, $16,
		$17, $18, $19, $20, $1,
		$21, $22, $23
	)
	ON CONFLICT (policy_account) DO UPDATE SET
		desired_active = true,
		max_amount_per_period = EXCLUDED.max_amount_per_period,
		period_length_seconds = EXCLUDED.period_length_seconds,
		recurring_delegation_expiry_timestamp = EXCLUDED.recurring_delegation_expiry_timestamp,
		recurring_delegation_nonce = EXCLUDED.recurring_delegation_nonce,
		start_timestamp = EXCLUDED.start_timestamp,
		wallet_balance_floor_raw = EXCLUDED.wallet_balance_floor_raw
	RETURNING `+autodepositTargetColumns,
		in.WalletAddress, in.Cluster, in.DelegatedSigner, now, "intent:"+in.PolicyAccount,
		in.AmountPerPeriodRaw, in.PeriodLengthSeconds,
		in.PolicyAccount, in.PolicySeed,
		in.RecurringDelegation,
		in.ExpiryTimestamp, in.Nonce,
		in.Settings, in.StartTimestamp, in.SubscriptionAuthority, in.LiquidityMint,
		in.VaultIndex, in.VaultUsdcAta, in.VaultPubkey, in.WalletUsdcAta,
		in.WalletBalanceFloorRaw, in.WalletUsdcAta, in.WalletUsdcAta)
	target, err := scanAutodepositTarget(row)
	if err != nil {
		return autodepositTargetRow{}, errors.New("Failed to record autodeposit setup intent.")
	}
	return target, nil
}

type autodepositBootstrapSnapshot struct {
	AmountRaw        uint64
	ObservedSlot     uint64
	ObservedAt       time.Time
	Mint             string
	Owner            string
	AccountDataHash  string
	AccountLamports  string
	AccountOwner     string
	SetupSignature   string
	Source           string
	SourceCommitment string
}

type autodepositSweepRow struct {
	Classification        string
	Confidence            string
	EligibleAfter         time.Time
	ExecuteNowAvailableAt *time.Time
	ID                    int64
	LotCount              int64
	OriginalAmountRaw     uint64
	Reason                string
	RemainingAmountRaw    uint64
	SlotID                int64
	Status                string
}

func (s autodepositSweepRow) isOpen() bool {
	return s.Status == "scheduled" && s.RemainingAmountRaw > 0
}

// autodepositBootstrapResult is EarnAutodepositBootstrapSweepResult: status is
// "scheduled", "already_exists" or "skipped" (skipped carries a reason).
type autodepositBootstrapResult struct {
	Status string
	Reason string
	Sweep  *autodepositSweepRow
}

const autodepositBootstrapEventSource = "app_autodeposit_setup_confirm"
const autodepositEventTargetIDMax = 999999999999

// createBootstrapWalletBalanceEventID ports createBootstrapWalletBalanceEventId
// (earn-autodeposit-event-id.shared.ts): App bootstrap events reserve the
// negative target-ID range.
func createBootstrapWalletBalanceEventID(targetID int64) (int64, error) {
	if targetID <= 0 || targetID > autodepositEventTargetIDMax {
		return 0, fmt.Errorf("Autodeposit target ID %d is outside the reserved App event ID range.", targetID)
	}
	return -targetID, nil
}

func isAppAutodepositBootstrapEventSource(source string) bool {
	switch source {
	case "app_autodeposit_artifact_reconcile", "app_autodeposit_setup_confirm", "mobile_autodeposit_artifact_reconcile":
		return true
	}
	return false
}

func resolveEarnAutodepositDelegationReadyAt(startTimestamp uint64) *time.Time {
	if startTimestamp == 0 {
		return nil
	}
	ms := new(big.Int).Mul(new(big.Int).SetUint64(startTimestamp), big.NewInt(1000))
	if !ms.IsInt64() {
		return nil
	}
	ready := time.UnixMilli(ms.Int64())
	return &ready
}

func resolveAutodepositSweepEligibleAfter(startTimestamp uint64, scheduledAt time.Time) time.Time {
	if ready := resolveEarnAutodepositDelegationReadyAt(startTimestamp); ready != nil && ready.After(scheduledAt) {
		return *ready
	}
	return scheduledAt
}

type autodepositBootstrapEvidence struct {
	AccountLamports string `json:"accountLamports"`
	AccountOwner    string `json:"accountOwner"`
	Bootstrap       bool   `json:"bootstrap"`
	SetupSignature  string `json:"setupSignature"`
}

func (s autodepositBootstrapSnapshot) evidence(setupSignature string) ([]byte, error) {
	return json.Marshal(autodepositBootstrapEvidence{
		AccountLamports: s.AccountLamports,
		AccountOwner:    s.AccountOwner,
		Bootstrap:       true,
		SetupSignature:  setupSignature,
	})
}

func (h earnRoutes) scheduleBootstrapAutodepositSweep(ctx context.Context, snapshot autodepositBootstrapSnapshot, target autodepositTargetRow) (autodepositBootstrapResult, error) {
	tx, err := h.YieldDB.Begin(ctx)
	if err != nil {
		return autodepositBootstrapResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := scheduleBootstrapAutodepositSweepTx(ctx, tx, time.Now(), snapshot, target)
	if err != nil {
		return autodepositBootstrapResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return autodepositBootstrapResult{}, err
	}
	return result, nil
}

func scheduleBootstrapAutodepositSweepTx(ctx context.Context, tx pgx.Tx, now time.Time, snapshot autodepositBootstrapSnapshot, target autodepositTargetRow) (autodepositBootstrapResult, error) {
	sourceEventID, err := createBootstrapWalletBalanceEventID(target.ID)
	if err != nil {
		return autodepositBootstrapResult{}, err
	}
	if !isAppAutodepositBootstrapEventSource(snapshot.Source) {
		return autodepositBootstrapResult{}, fmt.Errorf("Autodeposit bootstrap source %s cannot use the App event ID range.", snapshot.Source)
	}

	var previousAmountRaw *uint64
	err = tx.QueryRow(ctx, `SELECT amount_raw FROM loyal_yield.balance_sweep_wallet_balances_current
		WHERE target_id = $1 AND mint = $2`, target.ID, snapshot.Mint).Scan(&previousAmountRaw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return autodepositBootstrapResult{}, err
	}

	projectionEvidence, err := snapshot.evidence(snapshot.SetupSignature)
	if err != nil {
		return autodepositBootstrapResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_wallet_balances_current (
		target_id, wallet, wallet_usdc_ata, wallet_token_ata, amount_raw, owner, mint,
		observed_slot, observed_at, source, source_commitment, account_data_hash, raw_evidence, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	ON CONFLICT (target_id, mint) DO UPDATE SET
		wallet = EXCLUDED.wallet, wallet_usdc_ata = EXCLUDED.wallet_usdc_ata,
		wallet_token_ata = EXCLUDED.wallet_token_ata, amount_raw = EXCLUDED.amount_raw,
		owner = EXCLUDED.owner, observed_slot = EXCLUDED.observed_slot,
		observed_at = EXCLUDED.observed_at, source = EXCLUDED.source,
		source_commitment = EXCLUDED.source_commitment,
		account_data_hash = EXCLUDED.account_data_hash,
		raw_evidence = EXCLUDED.raw_evidence, updated_at = EXCLUDED.updated_at
	WHERE balance_sweep_wallet_balances_current.observed_slot <= EXCLUDED.observed_slot`,
		target.ID, target.Wallet, target.WalletUsdcAta, target.WalletTokenAta, snapshot.AmountRaw,
		snapshot.Owner, snapshot.Mint, snapshot.ObservedSlot, snapshot.ObservedAt, snapshot.Source,
		snapshot.SourceCommitment, snapshot.AccountDataHash, projectionEvidence, now); err != nil {
		return autodepositBootstrapResult{}, err
	}

	floorRaw := uint64(0)
	if target.WalletBalanceFloorRaw != nil {
		floorRaw = *target.WalletBalanceFloorRaw
	}
	if snapshot.AmountRaw <= floorRaw {
		return autodepositBootstrapResult{Status: "skipped", Reason: "wallet_balance_at_or_below_floor"}, nil
	}
	surplusRaw := snapshot.AmountRaw - floorRaw

	eventEvidence, err := snapshot.evidence(target.LastSeenSignature)
	if err != nil {
		return autodepositBootstrapResult{}, err
	}
	var deltaAmountRaw *int64
	if previousAmountRaw != nil {
		delta := int64(snapshot.AmountRaw) - int64(*previousAmountRaw)
		deltaAmountRaw = &delta
	}
	var eventID int64
	err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_wallet_balance_events (
		event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata, mint,
		previous_amount_raw, amount_raw, delta_amount_raw, observed_slot, observed_at,
		source, source_commitment, txn_signature, account_data_hash, raw_evidence, projected_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULL,$14,$15,$16)
	ON CONFLICT (event_id) DO NOTHING
	RETURNING event_id`,
		sourceEventID, target.ID, target.Wallet, target.WalletUsdcAta, target.WalletTokenAta, snapshot.Mint,
		previousAmountRaw, snapshot.AmountRaw, deltaAmountRaw, snapshot.ObservedSlot, snapshot.ObservedAt,
		snapshot.Source, snapshot.SourceCommitment, snapshot.AccountDataHash, eventEvidence, now).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		var owner int64
		var source string
		err := tx.QueryRow(ctx, `SELECT target_id, source FROM loyal_yield.balance_sweep_wallet_balance_events
			WHERE event_id = $1`, sourceEventID).Scan(&owner, &source)
		if err != nil || owner != target.ID || !isAppAutodepositBootstrapEventSource(source) {
			conflict := "an event that could not be loaded"
			if err == nil {
				conflict = fmt.Sprintf("target %d from %s", owner, source)
			}
			return autodepositBootstrapResult{}, fmt.Errorf(
				"Autodeposit bootstrap event ID %d for target %d conflicts with %s.", sourceEventID, target.ID, conflict)
		}
	} else if err != nil {
		return autodepositBootstrapResult{}, err
	}

	var lotID int64
	var lotRemaining uint64
	var lotSlotID *int64
	var lotStatus string
	var lotTargetID int64
	err = tx.QueryRow(ctx, `SELECT id, remaining_amount_raw, scheduled_slot_id, status, target_id
		FROM loyal_yield.balance_sweep_surplus_lots WHERE source_event_id = $1`, sourceEventID).
		Scan(&lotID, &lotRemaining, &lotSlotID, &lotStatus, &lotTargetID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return autodepositBootstrapResult{}, err
	}
	if lotID != 0 {
		if lotTargetID != target.ID {
			return autodepositBootstrapResult{}, fmt.Errorf(
				"Autodeposit bootstrap event ID %d for target %d is linked to a lot for target %d.",
				sourceEventID, target.ID, lotTargetID)
		}
		if lotStatus == "open" {
			slotID := int64(0)
			if lotSlotID != nil {
				slotID = *lotSlotID
			} else {
				slotID, err = ensureScheduledAutodepositSlotTx(ctx, tx, now, target.ID, snapshot.Mint,
					resolveAutodepositSweepEligibleAfter(startTimestampOf(target), snapshot.ObservedAt.Add(time.Hour)))
				if err != nil {
					return autodepositBootstrapResult{}, err
				}
				if _, err := tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_surplus_lots
					SET scheduled_slot_id = $1, updated_at = $2 WHERE id = $3`, slotID, now, lotID); err != nil {
					return autodepositBootstrapResult{}, err
				}
			}
			sweep, err := findScheduledAutodepositSweepBySlotID(ctx, tx, target, slotID)
			if err != nil {
				return autodepositBootstrapResult{}, err
			}
			if sweep != nil && sweep.isOpen() {
				return autodepositBootstrapResult{Status: "already_exists", Sweep: sweep}, nil
			}
		}
		return autodepositBootstrapResult{Status: "skipped", Reason: "bootstrap_sweep_already_closed"}, nil
	}

	eligibleAfter := resolveAutodepositSweepEligibleAfter(startTimestampOf(target), snapshot.ObservedAt.Add(time.Hour))
	slotID, err := ensureScheduledAutodepositSlotTx(ctx, tx, now, target.ID, snapshot.Mint, eligibleAfter)
	if err != nil {
		return autodepositBootstrapResult{}, err
	}
	var insertedLotID *int64
	err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_surplus_lots (
		target_id, scheduled_slot_id, source_event_id, source_signature,
		original_amount_raw, remaining_amount_raw, classification, eligible_after,
		status, confidence, reason, created_at, updated_at
	) VALUES ($1,$2,$3,NULL,$4,$5,'initial_surplus',$6,'open','confirmed_snapshot',
		'initial Autodeposit surplus detected at setup confirmation',$7,$7)
	ON CONFLICT (source_event_id) DO NOTHING
	RETURNING id`, target.ID, slotID, sourceEventID, surplusRaw, surplusRaw, eligibleAfter, now).Scan(&insertedLotID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return autodepositBootstrapResult{}, err
	}
	if insertedLotID != nil {
		sweep, err := findScheduledAutodepositSweepBySlotID(ctx, tx, target, slotID)
		if err != nil {
			return autodepositBootstrapResult{}, err
		}
		if sweep != nil {
			return autodepositBootstrapResult{Status: "scheduled", Sweep: sweep}, nil
		}
	}

	var raceSlotID *int64
	var raceStatus string
	var raceTargetID int64
	var raceLotID int64
	err = tx.QueryRow(ctx, `SELECT id, scheduled_slot_id, status, target_id
		FROM loyal_yield.balance_sweep_surplus_lots WHERE source_event_id = $1`, sourceEventID).
		Scan(&raceLotID, &raceSlotID, &raceStatus, &raceTargetID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return autodepositBootstrapResult{}, err
	}
	if err == nil {
		if raceTargetID != target.ID {
			return autodepositBootstrapResult{}, fmt.Errorf(
				"Autodeposit bootstrap event ID %d for target %d raced with a lot for target %d.",
				sourceEventID, target.ID, raceTargetID)
		}
		if raceStatus == "open" {
			raceWinnerSlotID := slotID
			if raceSlotID != nil {
				raceWinnerSlotID = *raceSlotID
			} else {
				if _, err := tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_surplus_lots
					SET scheduled_slot_id = $1, updated_at = $2 WHERE id = $3`, raceWinnerSlotID, now, raceLotID); err != nil {
					return autodepositBootstrapResult{}, err
				}
			}
			sweep, err := findScheduledAutodepositSweepBySlotID(ctx, tx, target, raceWinnerSlotID)
			if err != nil {
				return autodepositBootstrapResult{}, err
			}
			if sweep != nil && sweep.isOpen() {
				return autodepositBootstrapResult{Status: "already_exists", Sweep: sweep}, nil
			}
		}
	}
	return autodepositBootstrapResult{Status: "skipped", Reason: "bootstrap_sweep_already_closed"}, nil
}

func startTimestampOf(target autodepositTargetRow) uint64 {
	return target.StartTimestamp
}

func (h earnRoutes) recordAutodepositCloseIntent(ctx context.Context, in autodepositCloseInput) (autodepositTargetRow, error) {
	existing, err := h.findAutodepositTargetByPolicy(ctx, in.PolicyAccount)
	if err != nil {
		return autodepositTargetRow{}, err
	}
	if existing == nil {
		return autodepositTargetRow{}, errors.New("Autodeposit target does not exist.")
	}
	target := *existing
	if target.Settings != in.Settings || target.Wallet != in.WalletAddress ||
		int(target.VaultIndex) != in.VaultIndex || target.VaultPubkey != in.VaultPubkey {
		return autodepositTargetRow{}, errors.New("Autodeposit close target does not match the wallet.")
	}
	if !slices.Contains(target.DelegatedSigners, in.DelegatedSigner) {
		return autodepositTargetRow{}, errors.New("Autodeposit close signer does not match target policy.")
	}
	if target.RecurringDelegation != nil && *target.RecurringDelegation != in.RecurringDelegation {
		return autodepositTargetRow{}, errors.New("Autodeposit recurring delegation does not match target.")
	}

	now := time.Now()
	tx, err := h.YieldDB.Begin(ctx)
	if err != nil {
		return autodepositTargetRow{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := cancelScheduledAutodepositTransactionsForCloseTx(ctx, tx, now, target.ID); err != nil {
		return autodepositTargetRow{}, err
	}
	updated, err := scanAutodepositTarget(tx.QueryRow(ctx,
		`UPDATE loyal_yield.balance_sweep_targets SET desired_active = false WHERE id = $1 RETURNING `+autodepositTargetColumns,
		target.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return autodepositTargetRow{}, errors.New("Failed to record autodeposit close intent.")
	}
	if err != nil {
		return autodepositTargetRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return autodepositTargetRow{}, err
	}
	return updated, nil
}

func cancelScheduledAutodepositTransactionsForCloseTx(ctx context.Context, tx pgx.Tx, now time.Time, targetID int64) error {
	_, err := tx.Exec(ctx, `WITH scheduled_slots AS (
  SELECT DISTINCT event.observed_slot
  FROM loyal_yield.balance_sweep_surplus_lots lot
  INNER JOIN loyal_yield.balance_sweep_wallet_balance_events event
    ON event.event_id = lot.source_event_id
  WHERE lot.target_id = $1
    AND lot.status IN ('open', 'selected')
    AND lot.remaining_amount_raw > 0
  UNION
  SELECT DISTINCT event.observed_slot
  FROM loyal_yield.balance_sweep_lot_claims claim
  INNER JOIN loyal_yield.balance_sweep_lot_claim_items item
    ON item.claim_token = claim.claim_token
  INNER JOIN loyal_yield.balance_sweep_surplus_lots lot
    ON lot.id = item.lot_id
  INNER JOIN loyal_yield.balance_sweep_wallet_balance_events event
    ON event.event_id = lot.source_event_id
  WHERE claim.target_id = $1
    AND claim.status = 'selected'
),
scoped_lots AS (
  SELECT lot.id
  FROM loyal_yield.balance_sweep_surplus_lots lot
  INNER JOIN loyal_yield.balance_sweep_wallet_balance_events event
    ON event.event_id = lot.source_event_id
  INNER JOIN scheduled_slots
    ON scheduled_slots.observed_slot = event.observed_slot
  WHERE lot.target_id = $1
),
selected_claims AS (
  SELECT claim.claim_token
  FROM loyal_yield.balance_sweep_lot_claims claim
  WHERE claim.target_id = $1
    AND claim.status = 'selected'
    AND EXISTS (
      SELECT 1
      FROM loyal_yield.balance_sweep_lot_claim_items scoped_item
      INNER JOIN scoped_lots
        ON scoped_lots.id = scoped_item.lot_id
      WHERE scoped_item.claim_token = claim.claim_token
    )
),
restored_amounts AS (
  SELECT item.lot_id AS lot_id, SUM(item.amount_raw)::bigint AS amount_raw
  FROM loyal_yield.balance_sweep_lot_claim_items item
  INNER JOIN selected_claims
    ON selected_claims.claim_token = item.claim_token
  GROUP BY item.lot_id
),
restored_lots AS (
  UPDATE loyal_yield.balance_sweep_surplus_lots lot
  SET remaining_amount_raw = lot.remaining_amount_raw + restored_amounts.amount_raw,
      status = CASE WHEN lot.status = 'suppressed' THEN lot.status
        ELSE 'open'::loyal_yield.balance_sweep_surplus_lot_status END,
      updated_at = $2
  FROM restored_amounts
  WHERE lot.id = restored_amounts.lot_id
    AND lot.target_id = $1
    AND lot.id IN (SELECT id FROM scoped_lots)
  RETURNING lot.id
),
released_claims AS (
  UPDATE loyal_yield.balance_sweep_lot_claims claim
  SET status = 'released'::loyal_yield.balance_sweep_lot_claim_status,
      updated_at = $2
  WHERE claim.claim_token IN (SELECT claim_token FROM selected_claims)
  RETURNING claim.claim_token
)
UPDATE loyal_yield.balance_sweep_surplus_lots lot
SET status = 'suppressed'::loyal_yield.balance_sweep_surplus_lot_status,
    updated_at = $2
WHERE lot.target_id = $1
  AND lot.id IN (SELECT id FROM scoped_lots)
  AND lot.status IN ('open', 'selected')
  AND lot.remaining_amount_raw > 0`, targetID, now)
	return err
}

func (h earnRoutes) updateAutodepositTargetActive(ctx context.Context, in autodepositToggleInput) (autodepositTargetRow, error) {
	existing, err := h.findAutodepositTargetByPolicy(ctx, in.PolicyAccount)
	if err != nil {
		return autodepositTargetRow{}, err
	}
	if existing == nil {
		return autodepositTargetRow{}, errors.New("Autodeposit target does not exist.")
	}
	target := *existing
	if target.Settings != in.Settings || target.Wallet != in.WalletAddress || int(target.VaultIndex) != in.VaultIndex {
		return autodepositTargetRow{}, errors.New("Autodeposit target does not match the wallet.")
	}
	if target.LifecycleStatus == "closed" {
		return autodepositTargetRow{}, errors.New("Closed autodeposit targets cannot be toggled.")
	}
	if target.LifecycleStatus != "active" {
		return autodepositTargetRow{}, errors.New("Pending autodeposit targets cannot be toggled.")
	}
	if target.RecurringDelegation == nil || *target.RecurringDelegation != in.RecurringDelegation {
		return autodepositTargetRow{}, errors.New("Autodeposit recurring delegation does not match target.")
	}

	updated, err := scanAutodepositTarget(h.YieldDB.QueryRow(ctx,
		`UPDATE loyal_yield.balance_sweep_targets SET desired_active = $1 WHERE policy_account = $2 RETURNING `+autodepositTargetColumns,
		in.Active, in.PolicyAccount))
	if errors.Is(err, pgx.ErrNoRows) {
		return autodepositTargetRow{}, errors.New("Failed to update autodeposit target active state.")
	}
	if err != nil {
		return autodepositTargetRow{}, err
	}
	return updated, nil
}

type autodepositFloorSweep struct {
	Classification        string
	Confidence            string
	EligibleAfter         time.Time
	ExecuteNowAvailableAt *time.Time
	SlotID                int64
	OriginalAmountRaw     int64
	Reason                string
	RemainingAmountRaw    int64
	Status                string
}

// autodepositFloorRebaseline is EarnAutodepositFloorRebaselineSweepResult:
// status is "scheduled" (with the sweep) or "skipped" (with the reason).
type autodepositFloorRebaseline struct {
	Status string
	Reason *string
	Sweep  *autodepositFloorSweep
}

type autodepositFloorUpdateResult struct {
	Rebaseline autodepositFloorRebaseline
	Target     autodepositTargetRow
}

func (h earnRoutes) updateAutodepositWalletBalanceFloor(ctx context.Context, in autodepositFloorInput) (autodepositFloorUpdateResult, error) {
	existing, err := h.findAutodepositTargetByPolicy(ctx, in.PolicyAccount)
	if err != nil {
		return autodepositFloorUpdateResult{}, err
	}
	if existing == nil {
		return autodepositFloorUpdateResult{}, errors.New("Autodeposit target does not exist.")
	}
	target := *existing
	if target.Settings != in.Settings || target.Wallet != in.WalletAddress || int(target.VaultIndex) != in.VaultIndex {
		return autodepositFloorUpdateResult{}, errors.New("Autodeposit target does not match the wallet.")
	}
	if target.LifecycleStatus == "closed" {
		return autodepositFloorUpdateResult{}, errors.New("Closed autodeposit targets cannot be updated.")
	}
	if !target.Active || target.LifecycleStatus != "active" {
		return autodepositFloorUpdateResult{}, errors.New("Pending autodeposit targets cannot be updated.")
	}
	if target.RecurringDelegation == nil || *target.RecurringDelegation != in.RecurringDelegation {
		return autodepositFloorUpdateResult{}, errors.New("Autodeposit recurring delegation does not match target.")
	}

	now := time.Now()
	eligibleAfter := resolveAutodepositSweepEligibleAfter(target.StartTimestamp, now.Add(time.Hour))
	var previousFloor *string
	if target.WalletBalanceFloorRaw != nil {
		text := strconv.FormatUint(*target.WalletBalanceFloorRaw, 10)
		previousFloor = &text
	}
	floorText := strconv.FormatUint(in.WalletBalanceFloorRaw, 10)

	var (
		projectionAmountRaw   *int64
		lotClassification     *string
		lotConfidence         *string
		lotEligibleAfter      *time.Time
		lotID                 *int64
		lotSlotID             *int64
		lotOriginalAmountRaw  *int64
		lotReason             *string
		lotRemainingAmountRaw *int64
		lotStatus             *string
		skippedReason         *string
	)
	err = h.YieldDB.QueryRow(ctx, autodepositFloorRebaselineSQL,
		target.ID, in.PolicyAccount, in.Settings, in.WalletAddress, in.VaultIndex,
		in.RecurringDelegation, int64(in.WalletBalanceFloorRaw), now,
		previousFloor, floorText, eligibleAfter).
		Scan(&projectionAmountRaw, &lotClassification, &lotConfidence, &lotEligibleAfter,
			&lotID, &lotSlotID, &lotOriginalAmountRaw, &lotReason, &lotRemainingAmountRaw,
			&lotStatus, &skippedReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return autodepositFloorUpdateResult{}, errors.New("Failed to update autodeposit wallet balance floor.")
	}
	if err != nil {
		return autodepositFloorUpdateResult{}, err
	}

	target.WalletBalanceFloorRaw = &in.WalletBalanceFloorRaw
	if lotID != nil {
		reason := ""
		if lotReason != nil {
			reason = *lotReason
		}
		sweep := &autodepositFloorSweep{
			Classification:        derefString(lotClassification),
			Confidence:            derefString(lotConfidence),
			EligibleAfter:         derefTime(lotEligibleAfter),
			ExecuteNowAvailableAt: resolveEarnAutodepositDelegationReadyAt(target.StartTimestamp),
			SlotID:                derefInt64(lotSlotID),
			OriginalAmountRaw:     derefInt64(lotOriginalAmountRaw),
			Reason:                reason,
			RemainingAmountRaw:    derefInt64(lotRemainingAmountRaw),
			Status:                derefString(lotStatus),
		}
		return autodepositFloorUpdateResult{
			Rebaseline: autodepositFloorRebaseline{Status: "scheduled", Sweep: sweep},
			Target:     target,
		}, nil
	}
	reason := "wallet_balance_projection_missing"
	if skippedReason != nil && *skippedReason == "wallet_balance_at_or_below_floor" {
		reason = *skippedReason
	}
	return autodepositFloorUpdateResult{
		Rebaseline: autodepositFloorRebaseline{Status: "skipped", Reason: &reason},
		Target:     target,
	}, nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func derefTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

// autodepositFloorRebaselineSQL is the retained Apps floor-rebaseline CTE,
// ported statement for statement from
// earn-autodeposit-repository.server.ts:1812-2004 (locked_target through the
// final projection join). Event IDs come from the reserved negative floor
// sequence (migrations/yield/0069_autodeposit_event_id_ranges.sql:13).
const autodepositFloorRebaselineSQL = `WITH locked_target AS (
  SELECT target.id
  FROM loyal_yield.balance_sweep_targets target
  WHERE target.id = $1
    AND target.policy_account = $2
    AND target.settings = $3
    AND target.wallet = $4
    AND target.vault_index = $5
    AND target.desired_active = true
    AND target.chain_status = 'active'
    AND target.recurring_delegation = $6
  FOR UPDATE
),
updated_target AS (
  UPDATE loyal_yield.balance_sweep_targets
  SET wallet_balance_floor_raw = $7
  WHERE id IN (SELECT id FROM locked_target)
  RETURNING id, wallet, wallet_token_ata, wallet_usdc_ata
),
suppressed_lots AS (
  UPDATE loyal_yield.balance_sweep_surplus_lots lot
  SET status = 'suppressed'::loyal_yield.balance_sweep_surplus_lot_status,
      updated_at = $8
  WHERE lot.target_id IN (SELECT id FROM updated_target)
    AND lot.status = 'open'::loyal_yield.balance_sweep_surplus_lot_status
    AND lot.remaining_amount_raw > 0
  RETURNING lot.id
),
projection AS (
  SELECT current_balance.*
  FROM loyal_yield.balance_sweep_wallet_balances_current current_balance
  INNER JOIN updated_target
    ON updated_target.id = current_balance.target_id
),
inserted_event AS (
  INSERT INTO loyal_yield.balance_sweep_wallet_balance_events (
    event_id,
    target_id,
    wallet,
    wallet_usdc_ata,
    wallet_token_ata,
    mint,
    previous_amount_raw,
    amount_raw,
    delta_amount_raw,
    observed_slot,
    observed_at,
    source,
    source_commitment,
    txn_signature,
    account_data_hash,
    raw_evidence,
    projected_at
  )
  SELECT
    nextval('loyal_yield.balance_sweep_floor_rebaseline_event_id_seq'::regclass)::bigint,
    projection.target_id,
    projection.wallet,
    projection.wallet_usdc_ata,
    projection.wallet_token_ata,
    projection.mint,
    projection.amount_raw,
    projection.amount_raw,
    0,
    projection.observed_slot,
    projection.observed_at,
    'app_autodeposit_floor_rebaseline',
    projection.source_commitment,
    NULL,
    projection.account_data_hash,
    jsonb_build_object(
      'floorRebaseline', true,
      'previousWalletBalanceFloorRaw', $9::text,
      'walletBalanceFloorRaw', $10::text,
      'suppressedOpenLotCount', (SELECT COUNT(*) FROM suppressed_lots)
    ),
    $8
  FROM projection
  WHERE projection.amount_raw > $7
  RETURNING event_id
),
scheduled_slot AS (
  -- A target has at most one scheduled slot (unique index). Every open lot of
  -- the target was suppressed above, so the slot's idle clock and error
  -- belonged to lots that no longer count.
  INSERT INTO loyal_yield.balance_sweep_scheduled_slots AS slot (
    target_id,
    token_mint,
    eligible_after,
    status,
    created_at,
    updated_at
  )
  SELECT
    projection.target_id,
    projection.mint,
    $11,
    'scheduled',
    $8,
    $8
  FROM projection
  CROSS JOIN inserted_event
  WHERE true
  ON CONFLICT (target_id) WHERE status = 'scheduled' DO UPDATE
  SET eligible_after = GREATEST(slot.eligible_after, EXCLUDED.eligible_after),
      idle_blocked_since = NULL,
      last_error = NULL,
      updated_at = EXCLUDED.updated_at
  RETURNING
    slot.id,
    slot.eligible_after,
    slot.status::text AS status
),
inserted_lot AS (
  INSERT INTO loyal_yield.balance_sweep_surplus_lots (
    target_id,
    scheduled_slot_id,
    source_event_id,
    source_signature,
    original_amount_raw,
    remaining_amount_raw,
    classification,
    eligible_after,
    status,
    confidence,
    reason,
    created_at,
    updated_at
  )
  SELECT
    projection.target_id,
    scheduled_slot.id,
    inserted_event.event_id,
    NULL,
    projection.amount_raw - $7,
    projection.amount_raw - $7,
    'floor_rebaseline'::loyal_yield.balance_sweep_surplus_classification,
    $11,
    'open'::loyal_yield.balance_sweep_surplus_lot_status,
    'confirmed_projection',
    'Autodeposit floor update rebaseline',
    $8,
    $8
  FROM projection
  CROSS JOIN inserted_event
  CROSS JOIN scheduled_slot
  RETURNING
    classification::text AS "lotClassification",
    confidence AS "lotConfidence",
    eligible_after AS "lotEligibleAfter",
    id AS "lotId",
    scheduled_slot_id AS "lotSlotId",
    original_amount_raw AS "lotOriginalAmountRaw",
    reason AS "lotReason",
    remaining_amount_raw AS "lotRemainingAmountRaw",
    (SELECT status FROM scheduled_slot) AS "lotStatus"
)
SELECT
  projection.amount_raw AS "projectionAmountRaw",
  inserted_lot."lotClassification",
  inserted_lot."lotConfidence",
  inserted_lot."lotEligibleAfter",
  inserted_lot."lotId",
  inserted_lot."lotSlotId",
  inserted_lot."lotOriginalAmountRaw",
  inserted_lot."lotReason",
  inserted_lot."lotRemainingAmountRaw",
  inserted_lot."lotStatus",
  CASE
    WHEN projection.target_id IS NULL THEN 'wallet_balance_projection_missing'
    WHEN projection.amount_raw <= $7 THEN 'wallet_balance_at_or_below_floor'
    ELSE NULL
  END AS "skippedReason"
FROM updated_target
LEFT JOIN projection ON true
LEFT JOIN inserted_lot ON true`

// ensureScheduledAutodepositSlotTx ports ensureScheduledAutodepositSlot
// (:457): the same three steps as the Go worker's openSlotFor — take the
// target's turn, cancel a slot whose lots drained, then upsert against the
// partial unique index (migrations/yield/0109_autodeposit_one_scheduled_slot.sql:115).
func ensureScheduledAutodepositSlotTx(ctx context.Context, tx pgx.Tx, now time.Time, targetID int64, mint string, eligibleAfter time.Time) (int64, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM loyal_yield.balance_sweep_targets WHERE id = $1 FOR UPDATE`, targetID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
		SET status = 'canceled', last_error = 'its lots drained before new lots arrived', updated_at = $1
		WHERE slot.target_id = $2 AND slot.status = 'scheduled'
		AND NOT EXISTS (
			SELECT 1 FROM loyal_yield.balance_sweep_surplus_lots AS lot
			WHERE lot.scheduled_slot_id = slot.id AND lot.status = 'open' AND lot.remaining_amount_raw > 0
		)`, now, targetID); err != nil {
		return 0, err
	}
	var slotID int64
	err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_scheduled_slots
		(target_id, token_mint, eligible_after, status, created_at, updated_at)
		VALUES ($1, $2, $3, 'scheduled', $4, $4)
		ON CONFLICT (target_id) WHERE status = 'scheduled' DO UPDATE
		SET eligible_after = GREATEST(balance_sweep_scheduled_slots.eligible_after, EXCLUDED.eligible_after),
		updated_at = EXCLUDED.updated_at
		RETURNING id`, targetID, mint, eligibleAfter, now).Scan(&slotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("Failed to create Autodeposit scheduled slot.")
	}
	if err != nil {
		return 0, err
	}
	return slotID, nil
}

func findScheduledAutodepositSweepBySlotID(ctx context.Context, tx pgx.Tx, target autodepositTargetRow, slotID int64) (*autodepositSweepRow, error) {
	// The sweep aggregation runs on the caller's pool in TS; inside the
	// bootstrap tx the same rows are visible, so read them here directly.
	var sweep autodepositSweepRow
	err := tx.QueryRow(ctx, `SELECT slot.id,
		COALESCE(MIN(lot.classification::text) FILTER (WHERE lot.status = 'open'), 'unknown'),
		COALESCE(MIN(lot.confidence) FILTER (WHERE lot.status = 'open'), 'unknown'),
		MIN(slot.eligible_after),
		COUNT(*) FILTER (WHERE lot.status = 'open' AND lot.remaining_amount_raw > 0),
		COALESCE(SUM(lot.original_amount_raw) FILTER (WHERE lot.status = 'open'), claim.amount_raw, 0)::bigint,
		COALESCE(MIN(lot.reason) FILTER (WHERE lot.status = 'open'), 'Autodeposit scheduled sweep'),
		COALESCE(SUM(lot.remaining_amount_raw) FILTER (WHERE lot.status = 'open'), claim.amount_raw, 0)::bigint,
		MIN(slot.status::text)
		FROM loyal_yield.balance_sweep_scheduled_slots AS slot
		LEFT JOIN loyal_yield.balance_sweep_surplus_lots AS lot ON lot.scheduled_slot_id = slot.id
		LEFT JOIN loyal_yield.balance_sweep_lot_claims AS claim
			ON claim.claim_token = slot.claim_token AND claim.status = 'selected'
		WHERE slot.id = $1 AND slot.target_id = $2
		GROUP BY slot.id, claim.amount_raw`, slotID, target.ID).
		Scan(&sweep.ID, &sweep.Classification, &sweep.Confidence, &sweep.EligibleAfter, &sweep.LotCount,
			&sweep.OriginalAmountRaw, &sweep.Reason, &sweep.RemainingAmountRaw, &sweep.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sweep.SlotID = slotID
	ready := resolveEarnAutodepositDelegationReadyAt(startTimestampOf(target))
	sweep.ExecuteNowAvailableAt = ready
	return &sweep, nil
}
