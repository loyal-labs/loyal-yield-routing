package webapp

// DB reads for the non-position earn-state parts. Ports
// yield-deposit-repository.server.ts findCurrentEarnDepositOnboardingAttempt
// :3378, earn-autodeposit-repository.server.ts findCurrentEarnAutodepositState
// :635, sumEarnAutodepositCurrentPeriodDeposits :740,
// findPendingEarnAutodepositScheduledSweeps :768 and
// earn-cross-mint-repository.server.ts findEarnCrossMintSnapshot :93.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func scanStateOnboardingAttempt(row pgx.Row) (*stateOnboardingAttemptRow, error) {
	var a stateOnboardingAttemptRow
	err := row.Scan(&a.PolicyID, &a.PolicyAccount, &a.PolicySeed,
		&a.RoutePolicySignature, &a.RoutePolicyConfirmedSlot,
		&a.SetupPolicyID, &a.SetupPolicyAccount, &a.SetupPolicySeed,
		&a.SetupPolicySignature, &a.SetupPolicyConfirmedSlot,
		&a.DepositSignature, &a.DepositConfirmedSlot,
		&a.Status, &a.LastErrorCode, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// stateOnboardingAttempt ports findCurrentEarnDepositOnboardingAttempt: the
// latest non-complete attempt for the Earn vault.
func (h earnRoutes) stateOnboardingAttempt(ctx context.Context, settings, wallet, vaultPubkey string) (*stateOnboardingAttemptRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	attempt, err := scanStateOnboardingAttempt(h.YieldDB.QueryRow(ctx, `SELECT policy_id, policy_account, policy_seed,
  route_policy_signature, route_policy_confirmed_slot,
  setup_policy_id, setup_policy_account, setup_policy_seed,
  setup_policy_signature, setup_policy_confirmed_slot,
  deposit_signature, deposit_confirmed_slot, status, last_error_code, updated_at
FROM loyal_yield.earn_deposit_onboarding_attempts
WHERE settings = $1 AND vault_index = $2 AND wallet_address = $3 AND vault_pubkey = $4
  AND status <> 'complete'
ORDER BY updated_at DESC, id DESC
LIMIT 1`, settings, int16(earnVaultIndex), wallet, vaultPubkey))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read earn onboarding attempt: %w", err)
	}
	return attempt, nil
}

// stateAutodepositState ports findCurrentEarnAutodepositState: the wallet's
// most relevant non-closed balance sweep target with its policy, preferring
// the active row over paused/pending ones, newest first.
func (h earnRoutes) stateAutodepositState(ctx context.Context, settings, wallet string) (*stateAutodepositTargetRow, *stateBalanceSweepPolicyRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var target stateAutodepositTargetRow
	var policyID, policySeed *int64
	var policyAccount, subscriptionAuthority, subscriptionDelegatee *string
	var policySigners []string
	err := h.YieldDB.QueryRow(ctx, `SELECT t.id, t.balance_sweep_policy_id, t.settings, t.policy_seed,
  t.policy_account, t.vault_index, t.vault_pubkey, t.wallet, t.wallet_usdc_ata,
  t.vault_usdc_ata, t.delegated_signers, t.cluster, t.max_amount_per_period,
  t.desired_active, t.first_seen_at, t.last_seen_slot, t.last_seen_signature,
  t.subscription_authority, t.recurring_delegation, t.recurring_delegation_nonce,
  t.recurring_delegation_expiry_timestamp, t.period_length_seconds,
  t.start_timestamp, t.wallet_balance_floor_raw, t.policy_signature,
  t.policy_confirmed_slot, t.recurring_delegation_signature,
  t.recurring_delegation_confirmed_slot, t.chain_status,
  p.id, p.policy_seed, p.policy_account, p.delegated_signers,
  p.subscription_authority, p.subscription_delegatee
FROM loyal_yield.balance_sweep_targets t
LEFT JOIN loyal_yield.balance_sweep_policies p ON t.balance_sweep_policy_id = p.id
WHERE t.wallet = $1 AND t.settings = $2 AND t.vault_index = $3
  AND t.chain_status <> 'closed'
ORDER BY CASE WHEN t.desired_active = TRUE AND t.chain_status = 'active' THEN 0 ELSE 1 END,
  t.last_seen_slot DESC
LIMIT 1`, wallet, settings, int16(earnVaultIndex)).Scan(&target.ID, &target.BalanceSweepPolicyID,
		&target.Settings, &target.PolicySeed, &target.PolicyAccount, &target.VaultIndex,
		&target.VaultPubkey, &target.Wallet, &target.WalletUsdcAta, &target.VaultUsdcAta,
		&target.DelegatedSigners, &target.Cluster, &target.MaxAmountPerPeriod,
		&target.Active, &target.FirstSeenAt, &target.LastSeenSlot, &target.LastSeenSignature,
		&target.SubscriptionAuthority, &target.RecurringDelegation,
		&target.RecurringDelegationNonce, &target.RecurringDelegationExpiryTimestamp,
		&target.PeriodLengthSeconds, &target.StartTimestamp, &target.WalletBalanceFloorRaw,
		&target.PolicySignature, &target.PolicyConfirmedSlot,
		&target.RecurringDelegationSignature, &target.RecurringDelegationConfirmedSlot,
		&target.ChainStatus,
		&policyID, &policySeed, &policyAccount, &policySigners,
		&subscriptionAuthority, &subscriptionDelegatee)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read earn autodeposit state: %w", err)
	}
	var policy *stateBalanceSweepPolicyRow
	if policyID != nil {
		policy = &stateBalanceSweepPolicyRow{
			ID:                    *policyID,
			PolicySeed:            *policySeed,
			PolicyAccount:         *policyAccount,
			DelegatedSigners:      policySigners,
			SubscriptionAuthority: subscriptionAuthority,
			SubscriptionDelegatee: subscriptionDelegatee,
		}
	}
	return &target, policy, nil
}

// statePeriodDepositSum ports sumEarnAutodepositCurrentPeriodDeposits: the
// sum of sweep executions decoded since the current period started, zero when
// the target has no start timestamp.
func (h earnRoutes) statePeriodDepositSum(ctx context.Context, target *stateAutodepositTargetRow) (int64, error) {
	periodStart := stateAutodepositPeriodStart(target.StartTimestamp, target.PeriodLengthSeconds, time.Now())
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var sum *int64
	var err error
	if periodStart == nil {
		err = h.YieldDB.QueryRow(ctx, `SELECT sum(amount_raw) FROM loyal_yield.balance_sweep_executions
WHERE target_id = $1`, target.ID).Scan(&sum)
	} else {
		err = h.YieldDB.QueryRow(ctx, `SELECT sum(amount_raw) FROM loyal_yield.balance_sweep_executions
WHERE target_id = $1 AND coalesce(decoded_at, received_at) >= $2`, target.ID, *periodStart).Scan(&sum)
	}
	if err != nil {
		return 0, fmt.Errorf("sum earn autodeposit period deposits: %w", err)
	}
	if sum == nil {
		return 0, nil
	}
	return *sum, nil
}

// statePendingSweeps ports findPendingEarnAutodepositScheduledSweeps, whose
// SQL is ported verbatim (Drizzle interpolates only the table names).
func (h earnRoutes) statePendingSweeps(ctx context.Context, target *stateAutodepositTargetRow) ([]statePendingSweepRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, `WITH slot_lots AS (
  SELECT slot.id AS slot_id, slot.eligible_after, slot.status::text AS slot_status,
    lot.classification::text AS classification, lot.confidence, lot.reason,
    lot.original_amount_raw, lot.remaining_amount_raw, lot.status::text AS lot_status,
    lot.created_at, claim.amount_raw AS claim_amount_raw
  FROM loyal_yield.balance_sweep_scheduled_slots AS slot
  LEFT JOIN loyal_yield.balance_sweep_surplus_lots AS lot ON lot.scheduled_slot_id = slot.id
  LEFT JOIN loyal_yield.balance_sweep_lot_claims AS claim
    ON claim.claim_token = slot.claim_token AND claim.status = 'selected'
  WHERE slot.target_id = $1
    AND slot.status IN ('scheduled', 'requested', 'selected', 'failed', 'released')
), aggregated AS (
  SELECT slot_id, MIN(eligible_after) AS eligible_after,
    MIN(classification) FILTER (WHERE lot_status = 'open') AS classification,
    MIN(confidence) FILTER (WHERE lot_status = 'open') AS confidence,
    MIN(reason) FILTER (WHERE lot_status = 'open') AS reason,
    SUM(original_amount_raw) FILTER (WHERE lot_status = 'open') AS original_amount_raw,
    SUM(remaining_amount_raw) FILTER (WHERE lot_status = 'open') AS remaining_amount_raw,
    MAX(claim_amount_raw) AS claim_amount_raw,
    COUNT(*) FILTER (WHERE lot_status = 'open' AND remaining_amount_raw > 0) AS lot_count,
    MIN(created_at) FILTER (WHERE lot_status = 'open') AS first_lot_created_at,
    MIN(slot_status) AS status
  FROM slot_lots GROUP BY slot_id
)
SELECT aggregated.slot_id AS slot_id, aggregated.slot_id AS id,
  COALESCE(aggregated.classification, 'unknown') AS classification,
  COALESCE(aggregated.confidence, 'unknown') AS confidence,
  aggregated.eligible_after AS eligible_after,
  COALESCE(aggregated.lot_count, 0)::bigint AS lot_count,
  COALESCE(aggregated.original_amount_raw, aggregated.claim_amount_raw, 0)::bigint AS original_amount_raw,
  COALESCE(aggregated.reason, 'Autodeposit scheduled sweep') AS reason,
  COALESCE(aggregated.remaining_amount_raw, aggregated.claim_amount_raw, 0)::bigint AS remaining_amount_raw,
  aggregated.status AS status
FROM aggregated
WHERE COALESCE(aggregated.remaining_amount_raw, aggregated.claim_amount_raw, 0) > 0
  OR aggregated.status IN ('requested', 'selected', 'failed', 'released')
ORDER BY aggregated.eligible_after ASC, aggregated.first_lot_created_at ASC NULLS LAST,
  aggregated.slot_id ASC`, target.ID)
	if err != nil {
		return nil, fmt.Errorf("read pending earn autodeposit sweeps: %w", err)
	}
	defer rows.Close()
	executeNow := stateAutodepositDelegationReadyAt(target.StartTimestamp)
	var out []statePendingSweepRow
	for rows.Next() {
		var row statePendingSweepRow
		if err := rows.Scan(&row.SlotID, &row.ID, &row.Classification, &row.Confidence,
			&row.EligibleAfter, &row.LotCount, &row.OriginalAmountRaw, &row.Reason,
			&row.RemainingAmountRaw, &row.Status); err != nil {
			return nil, fmt.Errorf("read pending earn autodeposit sweeps: %w", err)
		}
		row.ExecuteNowAvailableAt = executeNow
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending earn autodeposit sweeps: %w", err)
	}
	return out, nil
}

// stateCrossMintSnapshot ports findEarnCrossMintSnapshot: the wallet's opt-in
// plus its cross-mint policy rows projected through
// deriveEarnCrossMintPolicyIndex and findEarnCrossMintSnapshot's shape.
func (h earnRoutes) stateCrossMintSnapshot(ctx context.Context, cluster, settings, authority, vaultPubkey string) (*EarnStateAutoswap, *EarnStateAutoswapIndex, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var optIn *stateCrossMintOptInRow
	rows, err := h.YieldDB.Query(ctx, `SELECT enabled, generation FROM loyal_yield.cross_mint_vault_opt_ins
WHERE cluster = $1 AND settings = $2 AND vault_index = $3 AND vault_pubkey = $4
LIMIT 1`, cluster, settings, int16(earnVaultIndex), vaultPubkey)
	if err != nil {
		return nil, nil, fmt.Errorf("read earn cross-mint opt-in: %w", err)
	}
	for rows.Next() {
		var row stateCrossMintOptInRow
		if err := rows.Scan(&row.Enabled, &row.Generation); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("read earn cross-mint opt-in: %w", err)
		}
		optIn = &row
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("read earn cross-mint opt-in: %w", err)
	}
	rows.Close()

	policyRows, err := h.YieldDB.Query(ctx, `SELECT active, daily_source_mint_spending_cap, delegated_signer,
  last_mutation, last_seen_signature, last_seen_slot, max_slippage_bps,
  policy_account, policy_seed, source_commitment, source_shard, start_eligible
FROM loyal_yield.cross_mint_swap_policies
WHERE cluster = $1 AND settings = $2 AND authority = $3 AND vault_index = $4 AND vault_pubkey = $5
ORDER BY last_seen_slot DESC`, cluster, settings, authority, int16(earnVaultIndex), vaultPubkey)
	if err != nil {
		return nil, nil, fmt.Errorf("read earn cross-mint policies: %w", err)
	}
	defer policyRows.Close()
	var policies []stateCrossMintPolicyRow
	for policyRows.Next() {
		var row stateCrossMintPolicyRow
		if err := policyRows.Scan(&row.Active, &row.DailySourceMintSpendingCap, &row.DelegatedSigner,
			&row.LastMutation, &row.LastSeenSignature, &row.LastSeenSlot, &row.MaxSlippageBps,
			&row.PolicyAccount, &row.PolicySeed, &row.SourceCommitment, &row.SourceShard,
			&row.StartEligible); err != nil {
			return nil, nil, fmt.Errorf("read earn cross-mint policies: %w", err)
		}
		policies = append(policies, row)
	}
	if err := policyRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read earn cross-mint policies: %w", err)
	}

	index := deriveEarnCrossMintIndex(policies)
	autoswap := buildEarnStateAutoswap(optIn, index)
	return autoswap, &index, nil
}
