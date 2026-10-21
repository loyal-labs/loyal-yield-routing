package autodeposit

import (
	"context"
	"errors"
	"fmt"
)

type TargetExecutionContext struct {
	FreshActionable          bool
	SetupPolicyAccount       string
	SweepPolicyAccount       string
	TargetID                 int64
	Settings                 string
	VaultIndex               int64
	Wallet                   string
	WalletUsdcAta            string
	VaultPubkey              string
	VaultUsdcAta             string
	TokenMint                string
	RecurringDelegation      *string
	RecurringDelegationNonce uint64
	WalletBalanceFloorRaw    int64
	MaxAmountPerPeriodRaw    *int64
	PeriodLengthSeconds      *int64
	StartTimestamp           *int64
	ExpiryTimestamp          *int64
	RoutePolicy              *RoutePolicyContext
	CurrentReserve           *string
	CurrentMarket            *string
	CurrentLiquidityMint     *string
}

type RoutePolicyContext struct {
	ID             int64
	Account        string
	Seed           int64
	ManagedVaultID int64
	LastSeenSlot   int64
}

func (s *Store) LoadTargetExecutionContext(ctx context.Context, targetID int64) (*TargetExecutionContext, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("autodeposit store has no database pool")
	}
	rows, err := s.pool.Query(ctx, `
SELECT
    target.id,
    target.policy_account,
    target.settings,
    target.vault_index,
    target.wallet,
    target.wallet_token_ata,
    target.vault_pubkey,
    target.vault_token_ata,
    target.token_mint,
    NULLIF(target.recurring_delegation, ''),
    target.wallet_balance_floor_raw,
    target.max_amount_per_period,
    target.period_length_seconds,
    target.start_timestamp,
    target.recurring_delegation_nonce,
    target.recurring_delegation_expiry_timestamp,
    target.desired_active AND target.chain_status = 'active',
    setup_policy.policy_account,
    policy.id,
    policy.policy_account,
    policy.policy_seed,
    policy.managed_vault_id,
    policy.last_seen_slot
FROM loyal_yield.balance_sweep_targets AS target
LEFT JOIN loyal_yield.managed_vaults AS setup_vault
  ON setup_vault.settings=target.settings
 AND setup_vault.vault_index=target.vault_index
 AND setup_vault.vault_pubkey=target.vault_pubkey
 AND setup_vault.active=true
LEFT JOIN loyal_yield.route_policies AS setup_policy
  ON setup_policy.id=setup_vault.setup_policy_id
 AND setup_policy.active=true
 AND setup_policy.cluster='mainnet-beta'
 AND setup_policy.authority=target.authority
 AND setup_policy.settings=target.settings
 AND setup_policy.vault_index=target.vault_index
 AND setup_policy.vault_pubkey=target.vault_pubkey
LEFT JOIN LATERAL (
    SELECT rp.id, rp.policy_account, rp.policy_seed, rp.last_seen_slot,
           mv.id AS managed_vault_id
    FROM loyal_yield.managed_vaults AS mv
    JOIN loyal_yield.route_policies AS rp
      ON mv.active_policy_id = rp.id
     AND rp.active = true
     AND rp.cluster = 'mainnet-beta'
     AND rp.authority = target.authority
     AND rp.settings = target.settings
     AND rp.vault_index = target.vault_index
     AND rp.vault_pubkey = target.vault_pubkey
     AND 'same_mint_kamino' = ANY(rp.route_modes)
    WHERE mv.settings = target.settings
      AND mv.vault_index = target.vault_index
      AND mv.vault_pubkey = target.vault_pubkey
      AND mv.active = true
    LIMIT 1
) AS policy ON TRUE
WHERE target.id = $1
  AND target.token_mint = $2`, targetID, USDCMint)
	if err != nil {
		return nil, fmt.Errorf("load autodeposit target context %d: %w", targetID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var context TargetExecutionContext
	var (
		floor         *int64
		nonce         *int64
		policyID      *int64
		policyAccount *string
		policySeed    *int64
		vaultID       *int64
		lastSeen      *int64
		setupPolicy   *string
	)
	if err := rows.Scan(
		&context.TargetID, &context.SweepPolicyAccount, &context.Settings, &context.VaultIndex, &context.Wallet,
		&context.WalletUsdcAta, &context.VaultPubkey, &context.VaultUsdcAta,
		&context.TokenMint, &context.RecurringDelegation, &floor,
		&context.MaxAmountPerPeriodRaw, &context.PeriodLengthSeconds, &context.StartTimestamp,
		&nonce, &context.ExpiryTimestamp,
		&context.FreshActionable, &setupPolicy,
		&policyID, &policyAccount, &policySeed, &vaultID, &lastSeen,
	); err != nil {
		return nil, fmt.Errorf("scan autodeposit target context %d: %w", targetID, err)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if floor == nil {
		return nil, fmt.Errorf("autodeposit target %d has no configured wallet balance floor", targetID)
	}
	context.WalletBalanceFloorRaw = *floor
	if nonce != nil {
		context.RecurringDelegationNonce = uint64(*nonce)
	}
	if setupPolicy != nil {
		context.SetupPolicyAccount = *setupPolicy
	}
	if policyID != nil && policyAccount != nil && policySeed != nil && vaultID != nil && lastSeen != nil {
		context.RoutePolicy = &RoutePolicyContext{
			ID: *policyID, Account: *policyAccount, Seed: *policySeed,
			ManagedVaultID: *vaultID, LastSeenSlot: *lastSeen,
		}
	}
	return &context, nil
}
