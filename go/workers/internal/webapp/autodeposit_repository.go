package webapp

import (
	"context"
	"errors"
	"fmt"
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
	RecurringDelegationNull bool
	RecurringDelegationNu64 uint64
}

const autodepositTargetColumns = `id, active, balance_sweep_policy_id, chain_status, policy_account, recurring_delegation, wallet_balance_floor_raw,
	settings, wallet, vault_index, vault_pubkey, wallet_usdc_ata, vault_usdc_ata, token_mint, wallet_token_ata, vault_token_ata,
	subscription_authority, recurring_delegation_nonce`

func scanAutodepositTarget(row pgx.Row) (autodepositTargetRow, error) {
	var target autodepositTargetRow
	var nonce *int64
	err := row.Scan(
		&target.ID, &target.Active, &target.BalanceSweepPolicyID, &target.LifecycleStatus, &target.PolicyAccount,
		&target.RecurringDelegation, &target.WalletBalanceFloorRaw, &target.Settings, &target.Wallet,
		&target.VaultIndex, &target.VaultPubkey, &target.WalletUsdcAta, &target.VaultUsdcAta,
		&target.TokenMint, &target.WalletTokenAta, &target.VaultTokenAta,
		&target.SubscriptionAuthority, &nonce,
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
