package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
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
}

const autodepositTargetColumns = `id, active, balance_sweep_policy_id, chain_status, policy_account, recurring_delegation, wallet_balance_floor_raw,
	settings, wallet, vault_index, vault_pubkey, wallet_usdc_ata, vault_usdc_ata, token_mint, wallet_token_ata, vault_token_ata,
	subscription_authority, last_seen_signature, start_timestamp, recurring_delegation_nonce`

func scanAutodepositTarget(row pgx.Row) (autodepositTargetRow, error) {
	var target autodepositTargetRow
	var nonce *int64
	err := row.Scan(
		&target.ID, &target.Active, &target.BalanceSweepPolicyID, &target.LifecycleStatus, &target.PolicyAccount,
		&target.RecurringDelegation, &target.WalletBalanceFloorRaw, &target.Settings, &target.Wallet,
		&target.VaultIndex, &target.VaultPubkey, &target.WalletUsdcAta, &target.VaultUsdcAta,
		&target.TokenMint, &target.WalletTokenAta, &target.VaultTokenAta,
		&target.SubscriptionAuthority, &target.LastSeenSignature, &target.StartTimestamp, &nonce,
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

// assertAutodepositTargetCanResume ports assertTargetCanResumeAutodepositSetup:
// an existing target may only resume when the confirmed setup matches what is
// already recorded, field by field, and the failure names every mismatch.
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

// recordAutodepositSetupIntent ports recordAutodepositSetupIntent
// (earn-autodeposit-repository.server.ts:1501). The INSERT is idempotent on
// policy_account: a double confirm re-applies the same seven configuration
// fields and returns the row; chain-observed columns stay untouched.
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
		active, authority, balance_sweep_policy_id, chain_observation_slot, chain_status, cluster,
		delegated_signers, first_seen_at, last_seen_at, last_seen_signature, last_seen_slot,
		lifecycle_status, max_amount_per_period, period_length_seconds,
		policy_account, policy_confirmed_slot, policy_seed, policy_signature,
		recurring_delegation, recurring_delegation_confirmed_slot,
		recurring_delegation_expiry_timestamp, recurring_delegation_nonce, recurring_delegation_signature,
		settings, start_timestamp, subscription_authority, threshold, token_mint,
		vault_index, vault_token_ata, vault_pubkey, vault_usdc_ata, wallet,
		wallet_balance_floor_raw, wallet_token_ata, wallet_usdc_ata
	) VALUES (
		true, $1, NULL, 0, 'pending', $2,
		ARRAY[$3::text], $4, $4, $5, 0,
		'pending', $6, $7,
		$8, NULL, $9, NULL,
		$10, NULL,
		$11, $12, NULL,
		$13, $14, $15, 1, $16,
		$17, $18, $19, $20, $1,
		$21, $22, $23
	)
	ON CONFLICT (policy_account) DO UPDATE SET
		active = true,
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

// ---- bootstrap sweep scheduling (earn-autodeposit-repository.server.ts:989) ----

// autodepositBootstrapSnapshot is the wallet USDC balance observed at confirm
// time (route.ts readBootstrapWalletBalanceSnapshot :309).
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

// autodepositSweepRow mirrors PendingEarnAutodepositScheduledSweepRecord.
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

// resolveEarnAutodepositDelegationReadyAt ports
// resolveEarnAutodepositDelegationReadyAt (:385): start seconds to a wall
// clock; nil when the target never set a start.
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

// scheduleBootstrapAutodepositSweep ports scheduleBootstrapEarnAutodepositSweep
// (earn-autodeposit-repository.server.ts:989-1254). The TS runs each batch as
// its own transaction; here the whole schedule runs in one tx, which is
// strictly more atomic (a crash cannot leave a projection without its event).
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

// findScheduledAutodepositSweepBySlotID ports
// findScheduledAutodepositSweepBySlotId (:517), reading the same aggregation
// as findPendingEarnAutodepositScheduledSweeps (:768) inside the open tx.
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
