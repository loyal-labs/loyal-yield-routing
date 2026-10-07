package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type RevalidationLease struct {
	SourceSnapshotID                *int64
	OpportunityID, OptimizerEpochID int64
	IdempotencyKey, Owner           string
	FencingToken                    int64
	ExpiresAt                       time.Time
	Cluster                         string
	VaultID                         int64
	VaultPubkey                     string
	VaultIndex                      uint8
	PolicyAccount                   string
	SetupPolicyAccount              string // active setup policy: init_obligation authority
	DelegatedSigners                []string
	SourceReserve                   string
	TargetReserve                   string
	LiquidityMint                   string
	SourceLiquidityMint             string
	TargetLiquidityMint             string
	RouteKind                       string
	LiquidityAmountRaw              uint64
	SourceCollateralRaw             uint64
	PrincipalUSDMicros              int64
	SourceAPYBPS, TargetAPYBPS      int64
	EdgeBPS, NetGainUSDMicros       int64
	FeeCapLamports                  int64
	OptimizerEpochKey               string
	ExecutionPlan                   json.RawMessage
}

// ClaimRevalidation leases one runnable/recoverable row with SKIP LOCKED. A
// crashed lease is only reclaimed in the same revalidation lane.
func (s *Store) ClaimRevalidation(ctx context.Context, cluster, owner string, ttl time.Duration, includeReady, crossMintEnabled bool, delegatedSigner ...string) (*RevalidationLease, error) {
	signer := ""
	if len(delegatedSigner) > 0 {
		signer = delegatedSigner[0]
	}
	return s.claimRoutePreparation(ctx, cluster, owner, ttl, includeReady, crossMintEnabled, signer, "", "revalidate")
}

// ClaimCrossMintActivation only takes untouched cross-mint reserve candidates.
// Same-mint fused admission and already signed movement custody remain owned by
// their existing claims. Expired execute leases are recoverable only pre-decision.
func (s *Store) ClaimCrossMintActivation(ctx context.Context, cluster, owner string, ttl time.Duration, signer string) (*RevalidationLease, error) {
	if signer == "" {
		return nil, errors.New("cross-mint activation requires delegated signer")
	}
	return s.claimRoutePreparation(ctx, cluster, owner, ttl, true, true, signer, "cross_mint_jupiter", "execute")
}

// ClaimCrossMintPreflight leaves every other family with its current owner.
// Its revalidation lease cannot authorize a signed movement admission.
func (s *Store) ClaimCrossMintPreflight(ctx context.Context, cluster, owner string, ttl time.Duration, signer string) (*RevalidationLease, error) {
	if signer == "" {
		return nil, errors.New("cross-mint preflight requires delegated signer")
	}
	return s.claimRoutePreparation(ctx, cluster, owner, ttl, false, true, signer, "cross_mint_jupiter", "revalidate")
}

func (s *Store) claimRoutePreparation(ctx context.Context, cluster, owner string, ttl time.Duration, includeReady, crossMintEnabled bool, signer, routeKind, leaseKind string) (*RevalidationLease, error) {
	if s == nil || s.pool == nil || cluster == "" || owner == "" || ttl < time.Second {
		return nil, errors.New("invalid revalidation claim")
	}
	var l RevalidationLease
	l.Cluster = cluster
	l.Owner = owner
	err := s.pool.QueryRow(ctx, `
WITH candidate AS (
 SELECT o.id FROM loyal_yield.rebalance_opportunities o
 JOIN loyal_yield.optimizer_epochs e ON e.id=o.optimizer_epoch_id AND e.cluster=o.cluster
 JOIN loyal_yield.managed_vaults candidate_vault ON candidate_vault.id=o.vault_id AND candidate_vault.active
 JOIN loyal_yield.route_policies candidate_policy ON candidate_policy.id=candidate_vault.active_policy_id AND candidate_policy.active
 LEFT JOIN loyal_yield.route_policies bound_withdraw_policy
   ON o.execution_plan->>'route_kind'='cross_mint_jupiter'
  AND bound_withdraw_policy.policy_account=o.execution_plan#>>'{policy_bindings,withdraw,policy_account}'
  AND bound_withdraw_policy.active
  AND bound_withdraw_policy.cluster=$1
  AND bound_withdraw_policy.source_commitment='finalized'
  AND bound_withdraw_policy.finalized_eligible
  AND bound_withdraw_policy.settings=candidate_vault.settings
  AND bound_withdraw_policy.vault_index=candidate_vault.vault_index
  AND bound_withdraw_policy.vault_pubkey=candidate_vault.vault_pubkey
  AND 'same_mint_kamino'=ANY(bound_withdraw_policy.route_modes)
  AND ($5='' OR $5=ANY(bound_withdraw_policy.delegated_signers))
 WHERE o.cluster=$1 AND o.available_at<=clock_timestamp()
   AND o.execution_plan->>'route_kind' IN ('same_mint','cross_mint_jupiter')
   AND ($7='' OR o.execution_plan->>'route_kind'=$7)
   AND o.execution_plan->>'source_kind'='reserve_position'
   AND ($7='' OR (o.decision_id IS NULL
        AND ($8<>'execute' OR (NULLIF(btrim(o.route_fingerprint),'') IS NOT NULL
          AND NULLIF(btrim(o.requirements_fingerprint),'') IS NOT NULL))
        AND EXISTS(SELECT 1 FROM loyal_yield.cross_mint_movement_controls control
          WHERE control.cluster=o.cluster AND control.start_new_movements AND control.continue_or_recover_existing)
        AND NOT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions holding
          WHERE holding.opportunity_id=o.id AND holding.submission_state IN ('signed','submitted','confirmed','needs_reconcile'))))
   AND (($6 AND o.execution_plan->>'route_kind'='cross_mint_jupiter'
         AND bound_withdraw_policy.id IS NOT NULL
         AND ($5='' OR o.execution_plan#>>'{policy_bindings,delegated_signer}'=$5))
     OR (o.execution_plan->>'route_kind'='same_mint'
         AND 'same_mint_kamino'=ANY(candidate_policy.route_modes)
         AND ($5='' OR (candidate_policy.cluster=$1
              AND candidate_policy.source_commitment='finalized'
              AND candidate_policy.finalized_eligible
              AND $5=ANY(candidate_policy.delegated_signers)))))
   AND o.source_reserve IS NOT NULL
   AND o.liquidity_mint=o.target_liquidity_mint
   AND ((o.execution_plan->>'route_kind'='same_mint'
         AND o.source_liquidity_mint=o.target_liquidity_mint)
     OR (o.execution_plan->>'route_kind'='cross_mint_jupiter'
         AND o.source_liquidity_mint<>o.target_liquidity_mint))
   AND o.expires_at>clock_timestamp()+interval '60 seconds'
   AND e.expires_at>clock_timestamp()+interval '60 seconds'
   AND (o.opportunity_state='revalidate'
        OR ($4 AND (o.execution_plan->>'route_kind'='same_mint' OR $7='cross_mint_jupiter') AND o.opportunity_state='ready')
        OR (o.opportunity_state='leased' AND o.lease_kind=$8 AND o.lease_expires_at<=clock_timestamp()))
   AND (o.lease_expires_at IS NULL OR o.lease_expires_at<=clock_timestamp())
 ORDER BY o.scheduler_priority_anchor DESC,o.economic_priority DESC,o.created_at,o.id
 FOR UPDATE OF o SKIP LOCKED LIMIT 1
), claimed AS (
 UPDATE loyal_yield.rebalance_opportunities o SET opportunity_state='leased',lease_kind=$8,lease_owner=$2,
 lease_expires_at=clock_timestamp()+$3::interval,fencing_token=fencing_token+1,attempt_count=attempt_count+1,updated_at=clock_timestamp()
 FROM candidate WHERE o.id=candidate.id RETURNING o.*)
SELECT claimed.id,claimed.optimizer_epoch_id,claimed.idempotency_key,claimed.fencing_token,
       claimed.lease_expires_at,claimed.vault_id,vault.vault_pubkey,vault.vault_index,
       CASE WHEN claimed.execution_plan->>'route_kind'='cross_mint_jupiter'
            THEN withdraw_policy.policy_account ELSE policy.policy_account END,
       CASE WHEN claimed.execution_plan->>'route_kind'='cross_mint_jupiter'
            THEN withdraw_policy.delegated_signers ELSE policy.delegated_signers END,
       COALESCE(setup_policy.policy_account,''),
       claimed.source_reserve,
       claimed.target_reserve,claimed.liquidity_mint,
       claimed.source_liquidity_mint,claimed.target_liquidity_mint,
       claimed.execution_plan->>'route_kind',claimed.amount_raw,
       COALESCE((claimed.execution_plan->>'source_collateral_amount_raw')::bigint,0),
       claimed.principal_usd_micros,claimed.source_apy_bps,claimed.target_apy_bps,
       claimed.estimated_edge_bps,claimed.expected_net_gain_usd_micros,
       claimed.estimated_cost_lamports,epoch.epoch_key,claimed.execution_plan,claimed.source_snapshot_id
FROM claimed
JOIN loyal_yield.optimizer_epochs epoch ON epoch.id=claimed.optimizer_epoch_id
JOIN loyal_yield.managed_vaults vault ON vault.id=claimed.vault_id AND vault.active
JOIN loyal_yield.route_policies policy ON policy.id=vault.active_policy_id AND policy.active
LEFT JOIN loyal_yield.route_policies setup_policy ON setup_policy.id=vault.setup_policy_id AND setup_policy.active
LEFT JOIN loyal_yield.route_policies withdraw_policy
  ON claimed.execution_plan->>'route_kind'='cross_mint_jupiter'
 AND withdraw_policy.policy_account=claimed.execution_plan#>>'{policy_bindings,withdraw,policy_account}'
 AND withdraw_policy.active AND withdraw_policy.cluster=claimed.cluster
 AND withdraw_policy.source_commitment='finalized' AND withdraw_policy.finalized_eligible
 AND withdraw_policy.settings=vault.settings AND withdraw_policy.vault_index=vault.vault_index
 AND withdraw_policy.vault_pubkey=vault.vault_pubkey
 AND 'same_mint_kamino'=ANY(withdraw_policy.route_modes)
 AND ($5='' OR $5=ANY(withdraw_policy.delegated_signers))`, cluster, owner, ttl.String(), includeReady, signer, crossMintEnabled, routeKind, leaseKind).Scan(
		&l.OpportunityID, &l.OptimizerEpochID, &l.IdempotencyKey, &l.FencingToken,
		&l.ExpiresAt, &l.VaultID, &l.VaultPubkey, &l.VaultIndex, &l.PolicyAccount,
		&l.DelegatedSigners, &l.SetupPolicyAccount, &l.SourceReserve, &l.TargetReserve, &l.LiquidityMint,
		&l.SourceLiquidityMint, &l.TargetLiquidityMint, &l.RouteKind,
		&l.LiquidityAmountRaw, &l.SourceCollateralRaw, &l.PrincipalUSDMicros,
		&l.SourceAPYBPS, &l.TargetAPYBPS, &l.EdgeBPS, &l.NetGainUSDMicros,
		&l.FeeCapLamports, &l.OptimizerEpochKey, &l.ExecutionPlan, &l.SourceSnapshotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim revalidation: %w", err)
	}
	return &l, nil
}

// PeekRevalidation is the read-only shadow twin of ClaimRevalidation: the same
// candidate predicate, a plain SELECT with no lock, no lease, and no UPDATE.
// It also admits rows the durable revalidator has already leased or prepared
// (lease_kind='revalidate'), because Rust claims a row within ~250ms and the
// shadow only reads; each (id, fencing_token) revision is returned once.
// Owner is "shadow" and FencingToken is the row's current token, so the result
// can never satisfy CommitRevalidation.
func (s *Store) PeekRevalidation(ctx context.Context, cluster, signer string, crossMintEnabled bool, seenIDs, seenTokens []int64) (*RevalidationLease, error) {
	if s == nil || s.pool == nil || cluster == "" || len(seenIDs) != len(seenTokens) {
		return nil, errors.New("invalid revalidation peek")
	}
	if seenIDs == nil {
		seenIDs, seenTokens = []int64{}, []int64{}
	}
	var l RevalidationLease
	l.Cluster = cluster
	l.Owner = "shadow"
	err := s.pool.QueryRow(ctx, `
WITH candidate AS (
 SELECT o.id FROM loyal_yield.rebalance_opportunities o
 JOIN loyal_yield.optimizer_epochs e ON e.id=o.optimizer_epoch_id AND e.cluster=o.cluster
 JOIN loyal_yield.managed_vaults candidate_vault ON candidate_vault.id=o.vault_id AND candidate_vault.active
 JOIN loyal_yield.route_policies candidate_policy ON candidate_policy.id=candidate_vault.active_policy_id AND candidate_policy.active
 LEFT JOIN loyal_yield.route_policies bound_withdraw_policy
   ON o.execution_plan->>'route_kind'='cross_mint_jupiter'
  AND bound_withdraw_policy.policy_account=o.execution_plan#>>'{policy_bindings,withdraw,policy_account}'
  AND bound_withdraw_policy.active
  AND bound_withdraw_policy.cluster=$1
  AND bound_withdraw_policy.source_commitment='finalized'
  AND bound_withdraw_policy.finalized_eligible
  AND bound_withdraw_policy.settings=candidate_vault.settings
  AND bound_withdraw_policy.vault_index=candidate_vault.vault_index
  AND bound_withdraw_policy.vault_pubkey=candidate_vault.vault_pubkey
  AND 'same_mint_kamino'=ANY(bound_withdraw_policy.route_modes)
  AND ($2='' OR $2=ANY(bound_withdraw_policy.delegated_signers))
 WHERE o.cluster=$1 AND o.available_at<=clock_timestamp()
   AND o.execution_plan->>'route_kind' IN ('same_mint','cross_mint_jupiter')
   AND o.execution_plan->>'source_kind'='reserve_position'
   AND (($3 AND o.execution_plan->>'route_kind'='cross_mint_jupiter'
         AND bound_withdraw_policy.id IS NOT NULL
         AND ($2='' OR o.execution_plan#>>'{policy_bindings,delegated_signer}'=$2))
     OR (o.execution_plan->>'route_kind'='same_mint'
         AND 'same_mint_kamino'=ANY(candidate_policy.route_modes)
         AND ($2='' OR (candidate_policy.cluster=$1
              AND candidate_policy.source_commitment='finalized'
              AND candidate_policy.finalized_eligible
              AND $2=ANY(candidate_policy.delegated_signers)))))
   AND o.source_reserve IS NOT NULL
   AND o.liquidity_mint=o.target_liquidity_mint
   AND ((o.execution_plan->>'route_kind'='same_mint'
         AND o.source_liquidity_mint=o.target_liquidity_mint)
     OR (o.execution_plan->>'route_kind'='cross_mint_jupiter'
         AND o.source_liquidity_mint<>o.target_liquidity_mint))
   AND o.expires_at>clock_timestamp()+interval '60 seconds'
   AND e.expires_at>clock_timestamp()+interval '60 seconds'
   AND (o.opportunity_state='revalidate'
        OR (o.opportunity_state IN ('leased','ready') AND o.lease_kind='revalidate'))
   AND NOT EXISTS (SELECT 1 FROM unnest($4::bigint[], $5::bigint[]) AS seen(id, token)
                   WHERE seen.id=o.id AND seen.token=o.fencing_token)
 ORDER BY o.updated_at DESC LIMIT 1
)
SELECT claimed.id,claimed.optimizer_epoch_id,claimed.idempotency_key,claimed.fencing_token,
       claimed.expires_at,claimed.vault_id,vault.vault_pubkey,vault.vault_index,
       CASE WHEN claimed.execution_plan->>'route_kind'='cross_mint_jupiter'
            THEN withdraw_policy.policy_account ELSE policy.policy_account END,
       CASE WHEN claimed.execution_plan->>'route_kind'='cross_mint_jupiter'
            THEN withdraw_policy.delegated_signers ELSE policy.delegated_signers END,
       COALESCE(setup_policy.policy_account,''),
       claimed.source_reserve,
       claimed.target_reserve,claimed.liquidity_mint,
       claimed.source_liquidity_mint,claimed.target_liquidity_mint,
       claimed.execution_plan->>'route_kind',claimed.amount_raw,
       COALESCE((claimed.execution_plan->>'source_collateral_amount_raw')::bigint,0),
       claimed.principal_usd_micros,claimed.source_apy_bps,claimed.target_apy_bps,
       claimed.estimated_edge_bps,claimed.expected_net_gain_usd_micros,
       claimed.estimated_cost_lamports,epoch.epoch_key,claimed.execution_plan,claimed.source_snapshot_id
FROM loyal_yield.rebalance_opportunities claimed
JOIN candidate ON candidate.id=claimed.id
JOIN loyal_yield.optimizer_epochs epoch ON epoch.id=claimed.optimizer_epoch_id
JOIN loyal_yield.managed_vaults vault ON vault.id=claimed.vault_id AND vault.active
JOIN loyal_yield.route_policies policy ON policy.id=vault.active_policy_id AND policy.active
LEFT JOIN loyal_yield.route_policies setup_policy ON setup_policy.id=vault.setup_policy_id AND setup_policy.active
LEFT JOIN loyal_yield.route_policies withdraw_policy
  ON claimed.execution_plan->>'route_kind'='cross_mint_jupiter'
 AND withdraw_policy.policy_account=claimed.execution_plan#>>'{policy_bindings,withdraw,policy_account}'
 AND withdraw_policy.active AND withdraw_policy.cluster=claimed.cluster
 AND withdraw_policy.source_commitment='finalized' AND withdraw_policy.finalized_eligible
 AND withdraw_policy.settings=vault.settings AND withdraw_policy.vault_index=vault.vault_index
 AND withdraw_policy.vault_pubkey=vault.vault_pubkey
 AND 'same_mint_kamino'=ANY(withdraw_policy.route_modes)
 AND ($2='' OR $2=ANY(withdraw_policy.delegated_signers))`, cluster, signer, crossMintEnabled, seenIDs, seenTokens).Scan(
		&l.OpportunityID, &l.OptimizerEpochID, &l.IdempotencyKey, &l.FencingToken,
		&l.ExpiresAt, &l.VaultID, &l.VaultPubkey, &l.VaultIndex, &l.PolicyAccount,
		&l.DelegatedSigners, &l.SetupPolicyAccount, &l.SourceReserve, &l.TargetReserve, &l.LiquidityMint,
		&l.SourceLiquidityMint, &l.TargetLiquidityMint, &l.RouteKind,
		&l.LiquidityAmountRaw, &l.SourceCollateralRaw, &l.PrincipalUSDMicros,
		&l.SourceAPYBPS, &l.TargetAPYBPS, &l.EdgeBPS, &l.NetGainUSDMicros,
		&l.FeeCapLamports, &l.OptimizerEpochKey, &l.ExecutionPlan, &l.SourceSnapshotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("peek revalidation: %w", err)
	}
	return &l, nil
}

func (s *Store) CheckRevalidationLease(ctx context.Context, lease RevalidationLease) error {
	if s == nil || s.pool == nil || lease.OpportunityID <= 0 {
		return errors.New("invalid revalidation lease check")
	}
	var current bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
SELECT 1 FROM loyal_yield.rebalance_opportunities opportunity
JOIN loyal_yield.optimizer_epochs epoch ON epoch.id=opportunity.optimizer_epoch_id AND epoch.cluster=opportunity.cluster
WHERE opportunity.id=$1 AND opportunity.idempotency_key=$2 AND opportunity.optimizer_epoch_id=$3
  AND opportunity.opportunity_state='leased' AND opportunity.lease_kind='revalidate'
  AND opportunity.lease_owner=$4 AND opportunity.fencing_token=$5
  AND opportunity.lease_expires_at>clock_timestamp() AND opportunity.expires_at>clock_timestamp()
  AND epoch.epoch_key=$6 AND epoch.expires_at>clock_timestamp())`, lease.OpportunityID, lease.IdempotencyKey, lease.OptimizerEpochID, lease.Owner, lease.FencingToken, lease.OptimizerEpochKey).Scan(&current)
	if err != nil {
		return err
	}
	if !current {
		return errors.New("lost lease or changed opportunity/epoch before route build")
	}
	return nil
}

func (s *Store) LoadReusableLookupTables(ctx context.Context, cluster string, vaultID, minimumSlot int64, requiredAddresses []string) ([]LookupTable, error) {
	requiredAddresses = canonicalStrings(requiredAddresses)
	if s == nil || s.pool == nil || cluster == "" || vaultID <= 0 || minimumSlot <= 0 || len(requiredAddresses) == 0 {
		return nil, errors.New("invalid reusable ALT query")
	}
	// Persisted verification establishes the normalized membership baseline; it
	// need not be from the current evidence slot because the caller reloads and
	// verifies every scoped candidate from confirmed RPC immediately afterward.
	rows, err := s.pool.Query(ctx, `
SELECT route_table.id,route_table.mutation_epoch,route_table.family_id,route_table.generation,
       CASE WHEN family.kind='vault_shards' THEN binding.id ELSE NULL END,route_table.table_address,
       array_agg(address.address ORDER BY address.ordinal),
       min(address.usable_after_slot),min(address.last_verified_slot)
FROM loyal_yield.route_lookup_tables route_table
JOIN loyal_yield.lookup_table_families family ON family.id=route_table.family_id
LEFT JOIN loyal_yield.lookup_table_vault_bindings binding
  ON binding.route_lookup_table_id=route_table.id
 AND binding.vault_id=$2 AND binding.lifecycle_state='active'
JOIN loyal_yield.lookup_table_addresses address ON address.route_lookup_table_id=route_table.id
WHERE family.cluster=$1 AND family.desired_state='active'
  AND route_table.cluster=$1 AND route_table.durable
  AND route_table.status IN ('active','usable')
  AND route_table.deactivated_slot IS NULL
  AND route_table.generation=family.active_generation
  AND route_table.desired_state='active'
  AND ((family.kind='shared_market' AND route_table.allocation_kind='shared_market')
    OR (family.kind='vault_shards' AND binding.id IS NOT NULL))
  AND EXISTS (
    SELECT 1 FROM loyal_yield.lookup_table_addresses relevant
    WHERE relevant.route_lookup_table_id=route_table.id
      AND relevant.address=ANY($4) AND relevant.usable_after_slot<=$3)
GROUP BY route_table.id,route_table.table_address,family.kind,binding.id
HAVING max(address.usable_after_slot)<=$3
   AND min(address.last_verified_slot) IS NOT NULL
   AND count(*)=route_table.address_count
   AND count(*)=route_table.usable_address_count
ORDER BY route_table.table_address`, cluster, vaultID, minimumSlot, requiredAddresses)
	if err != nil {
		return nil, fmt.Errorf("load reusable lookup tables: %w", err)
	}
	defer rows.Close()
	var result []LookupTable
	for rows.Next() {
		var table LookupTable
		if err := rows.Scan(&table.ID, &table.MutationEpoch, &table.FamilyID, &table.Generation, &table.BindingID, &table.Address, &table.Addresses, &table.UsableAfterSlot, &table.LastVerifiedSlot); err != nil {
			return nil, err
		}
		table.Active = true
		result = append(result, table)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) RefreshCapacityEpoch(ctx context.Context, cluster string, epoch ImmutableMarketEpoch) error {
	for _, reserve := range epoch.Reserves {
		if !reserve.TargetEligible || !isEarnStableMint(reserve.LiquidityMint) {
			continue
		}
		if err := s.RefreshTargetCapacity(ctx, cluster, reserve.Reserve, reserve.LiquidityMint, reserve.TotalSupplyUSDMicros, reserve.Slot); err != nil {
			return fmt.Errorf("refresh target capacity %s: %w", reserve.Reserve, err)
		}
	}
	return nil
}

func (s *Store) RefreshTargetCapacity(ctx context.Context, cluster, reserve, mint string, supply, slot int64) error {
	if s == nil || s.pool == nil || cluster == "" || reserve == "" || mint == "" || supply < 0 || slot <= 0 {
		return errors.New("invalid target capacity observation")
	}
	maximum := supply / 50
	if maximum < 4_000_000 {
		maximum = 4_000_000
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.target_capacity_frontiers(cluster,target_reserve,liquidity_mint,observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(cluster,target_reserve,liquidity_mint) DO NOTHING`, cluster, reserve, mint, supply, slot, maximum); err != nil {
		return err
	}
	var durableSupply, durableSlot, durableMaximum int64
	if err := tx.QueryRow(ctx, `SELECT observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3 FOR UPDATE`, cluster, reserve, mint).Scan(&durableSupply, &durableSlot, &durableMaximum); err != nil {
		return err
	}
	if slot < durableSlot {
		return errors.New("target capacity observation is older than durable telemetry")
	}
	if slot == durableSlot && (supply != durableSupply || maximum != durableMaximum) {
		return errors.New("target capacity observation conflicts at the same slot")
	}
	if slot > durableSlot {
		if _, err := tx.Exec(ctx, `UPDATE loyal_yield.target_capacity_frontiers SET observed_supply_usd_micros=$4,observed_slot=$5,maximum_inflight_usd_micros=$6,telemetry_version=telemetry_version+1,updated_at=clock_timestamp() WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3`, cluster, reserve, mint, supply, slot, maximum); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.target_capacity_reservations SET reservation_state='released',released_at=clock_timestamp(),release_reason='planner_target_telemetry_reflected_movement',state_version=state_version+1,updated_at=clock_timestamp() WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3 AND reservation_state='awaiting_telemetry' AND movement_slot<$4`, cluster, reserve, mint, slot); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type RevalidationCommit struct {
	CrossMintControlGeneration    *int64
	Disposition                   string
	Preparation                   *RoutePreparation
	MissingAddresses              []string
	ConflictKeys                  []string
	ExpectedEpochFingerprint      string
	ExpectedOpportunityKey        string
	FreshEconomics                bool
	ObservedSourceAPYBPS          int64
	ObservedTargetAPYBPS          int64
	TargetObservedSupplyUSDMicros int64
	TargetObservedSlot            int64
}

// CommitRevalidation performs every mutable fence and the queue transition in
// one transaction. Exact message/wire/simulation evidence is inserted before
// ready/execute becomes visible. A zero-row or changed identity is a fence,
// never a retry with stale bytes.
func (s *Store) CommitRevalidation(ctx context.Context, lease RevalidationLease, input RevalidationCommit) error {
	if input.Disposition == "fused_execute" {
		return errors.New("fused admission requires fresh typed execution evidence")
	}
	return s.commitRevalidation(ctx, lease, input, nil)
}

func (s *Store) commitRevalidation(ctx context.Context, lease RevalidationLease, input RevalidationCommit, admission *ExecutionAdmission) error {
	if s == nil || s.pool == nil || lease.FencingToken <= 0 || input.ExpectedOpportunityKey != lease.IdempotencyKey {
		return errors.New("invalid revalidation commit identity")
	}
	if input.Disposition != "ready" && input.Disposition != "waiting_alt" && input.Disposition != "fused_execute" {
		return errors.New("invalid revalidation disposition")
	}
	if input.Preparation == nil {
		return errors.New("every disposition requires exact route and requirements evidence")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var currentKey, state, kind, owner string
	if lease.RouteKind == "cross_mint_jupiter" {
		if input.CrossMintControlGeneration == nil {
			return errors.New("cross-mint publication requires captured movement control generation")
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "loyal-yield-cross-mint-control:"+lease.Cluster); err != nil {
			return err
		}
		generation, err := checkCrossMintSourceLease(ctx, tx, lease, "revalidate", true)
		if err != nil || generation != *input.CrossMintControlGeneration {
			if err == nil {
				err = errors.New("cross-mint control generation changed before source publication")
			}
			return err
		}
	}
	var token, epochID int64
	var leaseCurrent bool
	err = tx.QueryRow(ctx, `SELECT o.idempotency_key,o.opportunity_state,COALESCE(o.lease_kind,''),COALESCE(o.lease_owner,''),o.fencing_token,o.optimizer_epoch_id,o.lease_expires_at>clock_timestamp() AND o.expires_at>clock_timestamp()
FROM loyal_yield.rebalance_opportunities o WHERE o.id=$1 FOR UPDATE`, lease.OpportunityID).Scan(&currentKey, &state, &kind, &owner, &token, &epochID, &leaseCurrent)
	if errors.Is(err, pgx.ErrNoRows) || currentKey != lease.IdempotencyKey || state != "leased" || kind != "revalidate" || owner != lease.Owner || token != lease.FencingToken || epochID != lease.OptimizerEpochID || !leaseCurrent {
		return errors.New("lost lease or changed opportunity fence")
	}
	if err != nil {
		return err
	}
	var epochFingerprint string
	var epochCurrent bool
	if err := tx.QueryRow(ctx, `SELECT epoch_key,expires_at>clock_timestamp() FROM loyal_yield.optimizer_epochs WHERE id=$1 AND cluster=$2 FOR SHARE`, epochID, lease.Cluster).Scan(&epochFingerprint, &epochCurrent); err != nil || epochFingerprint != input.ExpectedEpochFingerprint || !epochCurrent {
		return errors.New("stale market epoch fence")
	}
	var max, committed, observedSupply, observedSlot, telemetryVersion, reservationGeneration int64
	if err := tx.QueryRow(ctx, `SELECT maximum_inflight_usd_micros,observed_supply_usd_micros,observed_slot,telemetry_version,reservation_generation FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3 FOR UPDATE`, lease.Cluster, lease.TargetReserve, lease.LiquidityMint).Scan(&max, &observedSupply, &observedSlot, &telemetryVersion, &reservationGeneration); err != nil {
		return fmt.Errorf("capacity frontier: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(principal_usd_micros),0) FROM loyal_yield.target_capacity_reservations WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3 AND reservation_state<>'released'`, lease.Cluster, lease.TargetReserve, lease.LiquidityMint).Scan(&committed); err != nil {
		return err
	}
	if lease.PrincipalUSDMicros <= 0 || max < lease.PrincipalUSDMicros || committed < 0 || committed > max-lease.PrincipalUSDMicros {
		return errors.New("target capacity fence exhausted")
	}
	keys := canonicalStrings(input.ConflictKeys)
	for _, key := range keys {
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.route_account_conflict_leases WHERE cluster=$1 AND writable_account_key=$2 AND opportunity_id<>$3 AND expires_at>clock_timestamp())`, lease.Cluster, key, lease.OpportunityID).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return errors.New("route conflict fence unavailable")
		}
	}
	prep := *input.Preparation
	if prep.RouteFingerprint == "" || prep.RequirementsFingerprint == "" || len(prep.ExecutionPlan) == 0 || !json.Valid(prep.ExecutionPlan) {
		return errors.New("route preparation evidence is incomplete")
	}
	if input.Disposition != "waiting_alt" {
		if len(input.MissingAddresses) != 0 {
			return errors.New("only waiting_alt may persist missing ALT addresses")
		}
		if len(prep.Transaction.Message) == 0 || len(prep.Transaction.UnsignedWire) == 0 || prep.Transaction.PacketBytes > SolanaPacketLimit || prep.Transaction.FeeLamports > uint64(lease.FeeCapLamports) || prep.Transaction.ComputeLimit == 0 || prep.Transaction.ComputeLimit > defaultComputeLimit || !prep.Simulation.Succeeded || prep.Simulation.WireSHA256 != prep.Transaction.WireSHA256 {
			return errors.New("executable route bytes or simulation evidence is incomplete")
		}
	}
	if admission != nil {
		if err := lockExecutionALTs(ctx, tx, lease, admission); err != nil {
			return err
		}
	}
	var provisioningRequestID int64
	if input.Disposition == "waiting_alt" {
		provisioningRequestID, err = upsertWaitingALTRequest(ctx, tx, lease, prep, input.MissingAddresses)
		if err != nil {
			return err
		}
	}
	next := "ready"
	var leaseKind any
	var leaseOwner any
	var leaseExpiry any
	if input.Disposition == "waiting_alt" {
		next = "waiting_alt"
	} else if input.Disposition == "fused_execute" {
		economicsPlan := lease.ExecutionPlan
		if input.FreshEconomics {
			if input.ObservedSourceAPYBPS < 0 || input.ObservedTargetAPYBPS < 0 || input.TargetObservedSupplyUSDMicros != observedSupply || input.TargetObservedSlot != observedSlot {
				return errors.New("fresh route economics do not match locked capacity telemetry")
			}
			var durable map[string]any
			if json.Unmarshal(economicsPlan, &durable) != nil {
				return errors.New("durable economics are invalid")
			}
			durable["source_apy_bps"] = input.ObservedSourceAPYBPS
			durable["observed_source_apy_bps"] = input.ObservedSourceAPYBPS
			durable["observed_target_apy_bps"] = input.ObservedTargetAPYBPS
			economicsPlan, err = json.Marshal(durable)
			if err != nil {
				return err
			}
		}
		economics, err := recomputeReservationEconomics(lease, economicsPlan, observedSupply, committed)
		if err != nil {
			return fmt.Errorf("atomic capacity economics: %w", err)
		}
		if prep.Transaction.FeeLamports > uint64(economics.FeeCapLamports) {
			return fmt.Errorf("compiled fee %d exceeds atomically recomputed cap %d", prep.Transaction.FeeLamports, economics.FeeCapLamports)
		}
		var preparedPlan map[string]any
		if json.Unmarshal(prep.ExecutionPlan, &preparedPlan) != nil {
			return errors.New("prepared execution plan is invalid")
		}
		preparedPlan["observed_target_apy_bps"] = economics.ObservedTargetAPYBPS
		preparedPlan["source_apy_bps"] = economics.SourceAPYBPS
		preparedPlan["target_apy_bps"] = economics.ProjectedTargetAPYBPS
		preparedPlan["capacity_adjusted_target_apy_bps"] = economics.ProjectedTargetAPYBPS
		preparedPlan["estimated_edge_bps"] = economics.EdgeBPS
		preparedPlan["fee_cap_lamports"] = economics.FeeCapLamports
		prep.ExecutionPlan, err = json.Marshal(preparedPlan)
		if err != nil {
			return err
		}
		next = "leased"
		leaseKind = "execute"
		leaseOwner = lease.Owner
		leaseExpiry = lease.ExpiresAt
		reservationGeneration++
		if tag, err := tx.Exec(ctx, `UPDATE loyal_yield.target_capacity_frontiers SET reservation_generation=$4,updated_at=clock_timestamp() WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3`, lease.Cluster, lease.TargetReserve, lease.LiquidityMint, reservationGeneration); err != nil || tag.RowsAffected() != 1 {
			return errors.New("capacity frontier generation fence failed")
		}
		var reservationID int64
		if err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.target_capacity_reservations(cluster,target_reserve,liquidity_mint,opportunity_id,principal_usd_micros,admitted_observed_supply_usd_micros,admitted_observed_slot,admitted_maximum_inflight_usd_micros,admitted_telemetry_version,reservation_generation,admitted_observed_target_apy_bps,admitted_projected_target_apy_bps,admitted_source_apy_bps,admitted_edge_bps,admitted_net_holding_gain_usd_micros,admitted_fee_cap_lamports,reservation_fencing_token) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) ON CONFLICT(opportunity_id) DO UPDATE SET cluster=EXCLUDED.cluster,target_reserve=EXCLUDED.target_reserve,liquidity_mint=EXCLUDED.liquidity_mint,principal_usd_micros=EXCLUDED.principal_usd_micros,admitted_observed_supply_usd_micros=EXCLUDED.admitted_observed_supply_usd_micros,admitted_observed_slot=EXCLUDED.admitted_observed_slot,admitted_maximum_inflight_usd_micros=EXCLUDED.admitted_maximum_inflight_usd_micros,admitted_telemetry_version=EXCLUDED.admitted_telemetry_version,reservation_generation=EXCLUDED.reservation_generation,admitted_observed_target_apy_bps=EXCLUDED.admitted_observed_target_apy_bps,admitted_projected_target_apy_bps=EXCLUDED.admitted_projected_target_apy_bps,admitted_source_apy_bps=EXCLUDED.admitted_source_apy_bps,admitted_edge_bps=EXCLUDED.admitted_edge_bps,admitted_net_holding_gain_usd_micros=EXCLUDED.admitted_net_holding_gain_usd_micros,admitted_fee_cap_lamports=EXCLUDED.admitted_fee_cap_lamports,reservation_fencing_token=EXCLUDED.reservation_fencing_token,reservation_state='active',released_at=NULL,release_reason=NULL,movement_slot=NULL,state_version=loyal_yield.target_capacity_reservations.state_version+1,updated_at=clock_timestamp() WHERE loyal_yield.target_capacity_reservations.reservation_state='released' AND loyal_yield.target_capacity_reservations.decision_id IS NULL AND loyal_yield.target_capacity_reservations.signed_submission_id IS NULL AND NOT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions submission WHERE submission.opportunity_id=EXCLUDED.opportunity_id) RETURNING id`, lease.Cluster, lease.TargetReserve, lease.LiquidityMint, lease.OpportunityID, lease.PrincipalUSDMicros, observedSupply, observedSlot, max, telemetryVersion, reservationGeneration, economics.ObservedTargetAPYBPS, economics.ProjectedTargetAPYBPS, economics.SourceAPYBPS, economics.EdgeBPS, economics.NetGainUSDMicros, economics.FeeCapLamports, token).Scan(&reservationID); err != nil {
			return fmt.Errorf("reserve target capacity: %w", err)
		}
		if admission != nil {
			admission.CapacityReservationID, admission.ReservationGeneration, admission.CapacityFencingToken = reservationID, reservationGeneration, token
			admission.Preparation = prep
		}
		for _, key := range keys {
			tag, err := tx.Exec(ctx, `INSERT INTO loyal_yield.route_account_conflict_leases(cluster,writable_account_key,opportunity_id,lease_owner,fencing_token,expires_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(cluster,writable_account_key) DO UPDATE SET opportunity_id=EXCLUDED.opportunity_id,lease_owner=EXCLUDED.lease_owner,fencing_token=EXCLUDED.fencing_token,expires_at=EXCLUDED.expires_at,submission_id=NULL,updated_at=clock_timestamp()
 WHERE loyal_yield.route_account_conflict_leases.submission_id IS NULL AND loyal_yield.route_account_conflict_leases.expires_at<=clock_timestamp()`, lease.Cluster, key, lease.OpportunityID, lease.Owner, token, lease.ExpiresAt)
			if err != nil || tag.RowsAffected() != 1 {
				return errors.New("atomic conflict lease acquisition failed")
			}
		}
	}
	if provisioningRequestID > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_request_consumers(opportunity_id,provisioning_request_id) VALUES($1,$2) ON CONFLICT(opportunity_id) DO UPDATE SET provisioning_request_id=EXCLUDED.provisioning_request_id`, lease.OpportunityID, provisioningRequestID); err != nil {
			return fmt.Errorf("attach ALT provisioning request: %w", err)
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state=$5,lease_kind=$6,lease_owner=$7,lease_expires_at=$8,route_fingerprint=$9,requirements_fingerprint=$10,execution_plan=$11::jsonb,updated_at=clock_timestamp() WHERE id=$1 AND opportunity_state='leased' AND lease_kind='revalidate' AND lease_owner=$2 AND fencing_token=$3 AND optimizer_epoch_id=$4 AND lease_expires_at>clock_timestamp() AND expires_at>clock_timestamp()`, lease.OpportunityID, lease.Owner, token, epochID, next, leaseKind, leaseOwner, leaseExpiry, prep.RouteFingerprint, prep.RequirementsFingerprint, nonemptyPlan(prep.ExecutionPlan))
	if err != nil || tag.RowsAffected() != 1 {
		return errors.New("revalidation transition was fenced")
	}
	return tx.Commit(ctx)
}

type provisioningAddress struct {
	Address       string
	SemanticClass string
	Ordinal       int32
	AccountRole   string
	Writable      bool
}

// upsertWaitingALTRequest mirrors the retained Rust handoff: the immutable
// address demand is inserted, sealed, and linked to its opportunity in the
// same transaction that makes waiting_alt visible.
func upsertWaitingALTRequest(ctx context.Context, tx pgx.Tx, lease RevalidationLease, prep RoutePreparation, missing []string) (int64, error) {
	shared, vault, err := waitingALTManifestAddresses(prep, missing)
	if err != nil {
		return 0, err
	}
	// Finalized external tables can cover an identity account and remove it
	// from durable demand. Bind the source lease to the builder's original
	// full typed accounts; persist and catalog-check only the durable vectors.
	sourceShared, sourceVault, err := ALTManifestSourceAddresses(prep.Manifest)
	if err != nil {
		return 0, err
	}
	if err := lockWaitingALTIdentity(ctx, tx, lease, sourceShared, sourceVault); err != nil {
		return 0, err
	}
	if err := lockWaitingALTSharedCatalog(ctx, tx, lease.Cluster, shared); err != nil {
		return 0, err
	}
	sharedHash, vaultHash := provisioningAddressesHash(shared), provisioningAddressesHash(vault)
	addresses := append(append([]provisioningAddress(nil), shared...), vault...)
	var requestID int64
	err = tx.QueryRow(ctx, `
INSERT INTO loyal_yield.lookup_table_provisioning_requests(
 cluster,vault_id,route_fingerprint,requirements_fingerprint,
 desired_shared_hash,desired_vault_hash,desired_shared_address_count,
 desired_vault_address_count,request_status)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'requested')
ON CONFLICT(cluster,vault_id,requirements_fingerprint) DO NOTHING
RETURNING id`, lease.Cluster, lease.VaultID, prep.RouteFingerprint, prep.RequirementsFingerprint, sharedHash, vaultHash, len(shared), len(vault)).Scan(&requestID)
	if err == nil {
		for _, address := range addresses {
			if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_request_addresses(request_id,address,semantic_class,ordinal,account_role,is_writable) VALUES($1,$2,$3,$4,$5,$6)`, requestID, address.Address, address.SemanticClass, address.Ordinal, address.AccountRole, address.Writable); err != nil {
				return 0, fmt.Errorf("insert ALT provisioning address: %w", err)
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET sealed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1 AND sealed_at IS NULL`, requestID)
		if err != nil || tag.RowsAffected() != 1 {
			return 0, errors.New("seal ALT provisioning request failed")
		}
		return requestID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("create ALT provisioning request: %w", err)
	}
	var existingSharedHash, existingVaultHash, status, errorCode string
	var sharedCount, vaultCount int
	var sealed bool
	if err := tx.QueryRow(ctx, `
SELECT id,COALESCE(desired_shared_hash,''),COALESCE(desired_vault_hash,''),
 desired_shared_address_count,desired_vault_address_count,sealed_at IS NOT NULL,
 request_status,COALESCE(error_code,'')
FROM loyal_yield.lookup_table_provisioning_requests
WHERE cluster=$1 AND vault_id=$2 AND requirements_fingerprint=$3
FOR UPDATE`, lease.Cluster, lease.VaultID, prep.RequirementsFingerprint).Scan(&requestID, &existingSharedHash, &existingVaultHash, &sharedCount, &vaultCount, &sealed, &status, &errorCode); err != nil {
		return 0, fmt.Errorf("load existing ALT provisioning request: %w", err)
	}
	// Source lookup_tables.rs13217: identical requirements may serve different
	// route shapes. Preserve the first route fingerprint as immutable audit data.
	if !sealed || existingSharedHash != sharedHash || existingVaultHash != vaultHash || sharedCount != len(shared) || vaultCount != len(vault) {
		return 0, errors.New("sealed ALT provisioning request idempotency collision")
	}
	rows, err := tx.Query(ctx, `SELECT address,semantic_class,ordinal,account_role,is_writable FROM loyal_yield.lookup_table_provisioning_request_addresses WHERE request_id=$1 ORDER BY semantic_class,ordinal`, requestID)
	if err != nil {
		return 0, err
	}
	var persisted []provisioningAddress
	for rows.Next() {
		var address provisioningAddress
		if err := rows.Scan(&address.Address, &address.SemanticClass, &address.Ordinal, &address.AccountRole, &address.Writable); err != nil {
			rows.Close()
			return 0, err
		}
		persisted = append(persisted, address)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(persisted) != len(addresses) {
		return 0, errors.New("sealed ALT provisioning request address collision")
	}
	for i := range persisted {
		if persisted[i] != addresses[i] {
			return 0, errors.New("sealed ALT provisioning request typed address collision")
		}
	}
	if status == "failed" && errorCode == "terminal_lookup_table_operation" {
		return 0, errors.New("ALT provisioning request has a terminal operation failure")
	}
	if status == "failed" || status == "cancelled" || status == "satisfied" {
		if _, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET request_status='requested',requested_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=NULL,error_code=NULL,error_detail=NULL,satisfied_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, requestID); err != nil {
			return 0, fmt.Errorf("reactivate ALT provisioning request: %w", err)
		}
	}
	return requestID, nil
}

// Fingerprints hash raw pubkey ordering including static accounts, while these
// persisted vectors use base58 lexical ordering and restart ordinals per class.
// They are different source contracts; never sort one to impersonate the other.
func waitingALTManifestAddresses(prep RoutePreparation, missing []string) ([]provisioningAddress, []provisioningAddress, error) {
	m := prep.Manifest
	requirements, err := ALTManifestRequirementsFingerprint(m)
	if err != nil {
		return nil, nil, err
	}
	validHash := func(s string) bool {
		b, err := hex.DecodeString(s)
		return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
	}
	if m == nil || requirements != prep.RequirementsFingerprint || !validHash(requirements) || !validHash(prep.RouteFingerprint) || len(m.VaultAddresses) == 0 || len(m.SharedAddresses)+len(m.VaultAddresses) > 256 || len(prep.Transaction.UnsignedWire) != 0 || len(prep.Transaction.Message) != 0 {
		return nil, nil, errors.New("waiting ALT requires a bounded complete unsigned typed manifest")
	}
	wanted := make(map[string]bool, len(missing))
	for _, key := range missing {
		if key == "" || wanted[key] {
			return nil, nil, errors.New("missing ALT keys must be nonempty and unique")
		}
		wanted[key] = true
	}
	if len(wanted) == 0 {
		return nil, nil, errors.New("waiting ALT requires missing vault addresses")
	}
	seen := make(map[string]bool, len(m.SharedAddresses)+len(m.VaultAddresses))
	sets := [][]ALTManifestAddress{m.SharedAddresses, m.VaultAddresses}
	classes := []string{"shared_market", "vault"}
	roles := [][]string{{"market", "market_authority", "reserve", "liquidity_mint", "liquidity_supply", "collateral_mint", "collateral_supply", "oracle", "scope_prices", "reserve_farm_state", "infrastructure"}, {"settings", "vault", "obligation", "policy", "action_account", "vault_token_account", "metadata", "farm_user_state"}}
	result := [2][]provisioningAddress{}
	for group, set := range sets {
		for i, a := range set {
			key, err := decodePublicKey(a.Address)
			if err != nil || encodeBase58(key[:]) != a.Address || seen[a.Address] || a.SemanticClass != classes[group] || a.Ordinal != int32(i) || (i > 0 && set[i-1].Address >= a.Address) {
				return nil, nil, errors.New("ALT manifest address class/order/ordinal or pubkey is invalid")
			}
			last := -1
			for _, role := range strings.Split(a.AccountRole, ",") {
				index := -1
				for j, name := range roles[group] {
					if role == name {
						index = j
						break
					}
				}
				if index <= last {
					return nil, nil, errors.New("ALT manifest role is unknown, duplicated or noncanonical")
				}
				last = index
			}
			seen[a.Address] = true
			if wanted[a.Address] {
				if group != 1 {
					return nil, nil, errors.New("missing shared ALT keys require catalog reconciliation")
				}
				delete(wanted, a.Address)
			}
			result[group] = append(result[group], provisioningAddress{a.Address, a.SemanticClass, a.Ordinal, a.AccountRole, a.Writable})
		}
	}
	if len(wanted) != 0 {
		return nil, nil, errors.New("missing ALT keys are outside the complete vault manifest")
	}
	return result[0], result[1], nil
}

func lockWaitingALTIdentity(ctx context.Context, tx pgx.Tx, lease RevalidationLease, shared, vault []ALTManifestAddress) error {
	hasRole := func(address, role string) bool {
		for _, a := range shared {
			if a.Address == address {
				for _, tag := range strings.Split(a.AccountRole, ",") {
					if tag == role {
						return true
					}
				}
			}
		}
		return false
	}
	if !hasRole(lease.SourceReserve, "reserve") || (lease.RouteKind != "cross_mint_jupiter" && !hasRole(lease.TargetReserve, "reserve")) {
		return errors.New("ALT manifest omits actual route reserve identity")
	}
	for _, mint := range []string{lease.LiquidityMint, lease.SourceLiquidityMint, lease.TargetLiquidityMint} {
		if mint != "" && !hasRole(mint, "liquidity_mint") {
			return errors.New("ALT manifest omits actual route mint identity")
		}
	}
	var settings, pubkey string
	var index int16
	if err := tx.QueryRow(ctx, `SELECT settings,vault_pubkey,vault_index FROM loyal_yield.managed_vaults WHERE id=$1 AND active FOR SHARE`, lease.VaultID).Scan(&settings, &pubkey, &index); err != nil {
		return fmt.Errorf("ALT manifest vault identity: %w", err)
	}
	if pubkey != lease.VaultPubkey || index != int16(lease.VaultIndex) {
		return errors.New("ALT manifest vault identity changed")
	}
	policies := map[string]bool{lease.PolicyAccount: true}
	if lease.RouteKind == "cross_mint_jupiter" {
		var plan crossMintPlan
		if err := json.Unmarshal(lease.ExecutionPlan, &plan); err != nil || plan.Bindings.Settings != settings || plan.Bindings.VaultPubkey != pubkey || plan.Bindings.VaultIndex != lease.VaultIndex || plan.Bindings.Withdraw.PolicyAccount != lease.PolicyAccount {
			return errors.New("ALT manifest canonical policy bindings changed")
		}
		policies[plan.Bindings.Swap.PolicyAccount] = true
	}
	foundVault, foundPolicy := false, false
	for _, a := range vault {
		for _, role := range strings.Split(a.AccountRole, ",") {
			switch role {
			case "settings":
				if a.Address != settings {
					return errors.New("ALT manifest names foreign settings")
				}
			case "vault":
				if a.Address != pubkey {
					return errors.New("ALT manifest names foreign vault")
				}
				foundVault = true
			case "policy":
				if !policies[a.Address] {
					return errors.New("ALT manifest names unbound policy")
				}
				var actual string
				if err := tx.QueryRow(ctx, `SELECT policy_account FROM loyal_yield.route_policies WHERE policy_account=$1 AND settings=$2 AND vault_pubkey=$3 AND vault_index=$4 AND active FOR SHARE`, a.Address, settings, pubkey, index).Scan(&actual); err != nil {
					return fmt.Errorf("ALT manifest policy identity: %w", err)
				}
				if a.Address == lease.PolicyAccount {
					foundPolicy = true
				}
			}
		}
	}
	if !foundVault || !foundPolicy {
		return errors.New("ALT manifest omits actual vault or bound source policy")
	}
	return nil
}

// LockSharedALTManifestCoverage ports lookup_tables.rs4051/10785/11111. The
// caller owns its source fence and transaction. Arbitrary table membership is
// insufficient: typed route requirements must fit the current catalog and its
// exact active physical generation, with no append remnants or pending mutation.
func LockSharedALTManifestCoverage(ctx context.Context, tx pgx.Tx, cluster string, addresses []ALTManifestAddress) error {
	rows := make([]provisioningAddress, len(addresses))
	for i, a := range addresses {
		rows[i] = provisioningAddress{a.Address, a.SemanticClass, a.Ordinal, a.AccountRole, a.Writable}
	}
	return lockWaitingALTSharedCatalog(ctx, tx, cluster, rows)
}

func lockWaitingALTSharedCatalog(ctx context.Context, tx pgx.Tx, cluster string, required []provisioningAddress) error {
	if cluster == "" || tx == nil {
		return errors.New("shared ALT catalog requires source transaction and cluster")
	}
	for i, a := range required {
		if _, err := decodePublicKey(a.Address); err != nil || a.SemanticClass != "shared_market" || a.Ordinal != int32(i) || a.AccountRole == "" || (i > 0 && required[i-1].Address >= a.Address) {
			return errors.New("shared ALT demand is not normalized typed vector")
		}
	}
	// Lock source hierarchy one level at a time: family -> head -> physical.
	var familyID int64
	var generation *int32
	var highWater int32
	var version string
	if err := tx.QueryRow(ctx, `SELECT id,active_generation,allocation_high_water,catalog_version FROM loyal_yield.lookup_table_families WHERE cluster=$1 AND kind='shared_market' AND desired_state='active' FOR SHARE`, cluster).Scan(&familyID, &generation, &highWater, &version); err != nil {
		return fmt.Errorf("shared ALT catalog family missing: %w", err)
	}
	if generation == nil || highWater <= 0 || highWater > 256 {
		return errors.New("shared ALT catalog has no valid active generation")
	}
	var manifestID int64
	var expectedCount int
	if err := tx.QueryRow(ctx, `SELECT m.id,m.address_count FROM loyal_yield.lookup_table_shared_market_catalog_heads h
 JOIN loyal_yield.lookup_table_shared_market_catalog_revisions r ON r.id=h.catalog_revision_id AND r.family_id=h.family_id
 JOIN loyal_yield.lookup_table_manifests m ON m.id=r.manifest_id AND m.family_id=h.family_id
 WHERE h.family_id=$1 AND h.readiness_state='active' AND h.target_generation=$2 AND h.activated_at IS NOT NULL
 AND r.catalog_version=$3 AND m.catalog_version=r.catalog_version AND m.subject_kind='shared_market'
 AND m.vault_id IS NULL AND m.sealed_at IS NOT NULL AND m.desired_set_hash=r.desired_set_hash
 AND m.address_count=r.address_count FOR SHARE OF h,r,m`, familyID, *generation, version).Scan(&manifestID, &expectedCount); err != nil {
		return fmt.Errorf("shared ALT catalog head not active: %w", err)
	}
	if expectedCount <= 0 {
		return errors.New("shared ALT catalog is empty")
	}
	rows, err := tx.Query(ctx, `SELECT address,semantic_class,ordinal,account_role,is_writable FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id=$1 ORDER BY ordinal FOR SHARE`, manifestID)
	if err != nil {
		return err
	}
	var catalog []provisioningAddress
	byAddress := map[string]provisioningAddress{}
	for rows.Next() {
		var a provisioningAddress
		if err := rows.Scan(&a.Address, &a.SemanticClass, &a.Ordinal, &a.AccountRole, &a.Writable); err != nil {
			rows.Close()
			return err
		}
		if a.SemanticClass != "shared_market" || a.Ordinal != int32(len(catalog)) || a.AccountRole == "" || byAddress[a.Address].Address != "" {
			rows.Close()
			return errors.New("shared ALT catalog typed rows inconsistent")
		}
		catalog = append(catalog, a)
		byAddress[a.Address] = a
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(catalog) != expectedCount {
		return errors.New("shared ALT catalog sealed count differs")
	}
	for _, a := range required {
		stored, ok := byAddress[a.Address]
		if !ok || (a.Writable && !stored.Writable) {
			return errors.New("shared ALT catalog lacks route address/access")
		}
		roles := map[string]bool{}
		for _, role := range strings.Split(stored.AccountRole, ",") {
			roles[role] = true
		}
		for _, role := range strings.Split(a.AccountRole, ",") {
			if role == "" || !roles[role] {
				return errors.New("shared ALT catalog lacks route roles")
			}
		}
	}
	type shard struct {
		id                               int64
		ordinal, count, usable, capacity int32
		verified                         *int64
		desired, status                  string
		durable                          bool
		deactivated                      *int64
	}
	rows, err = tx.Query(ctx, `SELECT id,shard_ordinal,address_count,COALESCE(usable_address_count,-1),COALESCE(allocation_high_water,-1),last_verified_slot,desired_state,status,durable,deactivated_slot
 FROM loyal_yield.route_lookup_tables WHERE family_id=$1 AND generation=$2 AND allocation_kind='shared_market'
 AND desired_state NOT IN('deactivated','closed','failed') ORDER BY shard_ordinal,id FOR SHARE`, familyID, *generation)
	if err != nil {
		return err
	}
	var shards []shard
	for rows.Next() {
		var s shard
		if err := rows.Scan(&s.id, &s.ordinal, &s.count, &s.usable, &s.capacity, &s.verified, &s.desired, &s.status, &s.durable, &s.deactivated); err != nil {
			rows.Close()
			return err
		}
		shards = append(shards, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(shards) != (len(catalog)-1)/int(highWater)+1 {
		return errors.New("shared ALT catalog physical shard count differs")
	}
	for i, s := range shards {
		start := i * int(highWater)
		end := min(start+int(highWater), len(catalog))
		if s.ordinal != int32(i) || s.count != int32(end-start) || s.usable != s.count || s.capacity != highWater || s.verified == nil || (s.desired != "active" && s.desired != "standby") || (s.status != "active" && s.status != "usable") || !s.durable || s.deactivated != nil {
			return errors.New("shared ALT catalog physical shard not ready")
		}
		members, err := tx.Query(ctx, `SELECT address,ordinal,usable_after_slot,last_verified_slot FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1 ORDER BY ordinal FOR SHARE`, s.id)
		if err != nil {
			return err
		}
		n := 0
		for members.Next() {
			var address string
			var ordinal int32
			var usable, verified int64
			if err := members.Scan(&address, &ordinal, &usable, &verified); err != nil {
				members.Close()
				return err
			}
			if n >= end-start || ordinal != int32(n) || address != catalog[start+n].Address || usable > verified || usable > *s.verified {
				members.Close()
				return errors.New("shared ALT physical membership differs or not usable")
			}
			n++
		}
		err = members.Err()
		members.Close()
		if err != nil {
			return err
		}
		if n != end-start {
			return errors.New("shared ALT physical membership incomplete")
		}
	}
	tableIDs := make([]int64, len(shards))
	for i, s := range shards {
		tableIDs[i] = s.id
	}
	rows, err = tx.Query(ctx, `SELECT id FROM loyal_yield.lookup_table_operations WHERE family_id=$1 AND (target_generation=$2 OR route_lookup_table_id=ANY($3::bigint[])) AND operation_state NOT IN('complete','permanent_failure','cancelled') FOR SHARE`, familyID, *generation, tableIDs)
	if err != nil {
		return err
	}
	pending := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if pending {
		return errors.New("shared ALT generation has pending mutation")
	}
	return ctx.Err()
}

func provisioningAddressesHash(addresses []provisioningAddress) string {
	hasher := sha256.New()
	var length [8]byte
	var ordinal [4]byte
	for _, address := range addresses {
		for _, value := range []string{address.Address, address.SemanticClass, address.AccountRole} {
			binary.LittleEndian.PutUint64(length[:], uint64(len(value)))
			hasher.Write(length[:])
			hasher.Write([]byte(value))
		}
		binary.LittleEndian.PutUint32(ordinal[:], uint32(address.Ordinal))
		hasher.Write(ordinal[:])
		if address.Writable {
			hasher.Write([]byte{1})
		} else {
			hasher.Write([]byte{0})
		}
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

type ReservationEconomics struct {
	ObservedTargetAPYBPS  int64
	ProjectedTargetAPYBPS int64
	SourceAPYBPS          int64
	EdgeBPS               int64
	NetGainUSDMicros      int64
	FeeCapLamports        int64
}

// Reservation economics are pure. Publication callers must supply telemetry
// and commitment totals from the locked frontier transaction.
type reservationEconomics = ReservationEconomics

func ComputeReservationEconomics(lease RevalidationLease, plan json.RawMessage, observedSupply, committed int64) (ReservationEconomics, error) {
	return recomputeReservationEconomics(lease, plan, observedSupply, committed)
}

// recomputeReservationEconomics mirrors the Rust admission transaction. It is
// deliberately called only after the capacity frontier is locked and all
// active reservations have been summed by the same serializable transaction.
func recomputeReservationEconomics(lease RevalidationLease, executionPlan json.RawMessage, observedSupply, committed int64) (reservationEconomics, error) {
	var plan struct {
		ObservedTargetAPYBPS            int64 `json:"observed_target_apy_bps"`
		SourceAPYBPS                    int64 `json:"source_apy_bps"`
		ConfidencePPM                   int64 `json:"confidence_ppm"`
		HoldingHorizonSeconds           int64 `json:"holding_horizon_seconds"`
		EstimatedExecutionCostUSDMicros int64 `json:"estimated_execution_cost_usd_micros"`
	}
	if err := json.Unmarshal(executionPlan, &plan); err != nil {
		return reservationEconomics{}, fmt.Errorf("decode durable economics: %w", err)
	}
	if observedSupply < 0 || committed < 0 || lease.PrincipalUSDMicros <= 0 || plan.ObservedTargetAPYBPS < 0 || plan.SourceAPYBPS < 0 || plan.ConfidencePPM <= 0 || plan.ConfidencePPM > 1_000_000 || plan.HoldingHorizonSeconds <= 0 || plan.EstimatedExecutionCostUSDMicros < 0 {
		return reservationEconomics{}, errors.New("invalid durable economics")
	}
	nextCommitted, ok := sumInt64(committed, lease.PrincipalUSDMicros)
	if !ok {
		return reservationEconomics{}, errors.New("committed inflow overflow")
	}
	projected := plan.ObservedTargetAPYBPS
	if observedSupply > 0 && nextCommitted > 0 {
		projectedSupply, sumOK := sumInt64(observedSupply, nextCommitted)
		if !sumOK || projectedSupply <= 0 {
			return reservationEconomics{}, errors.New("projected target supply overflow")
		}
		projected, ok = mulDivInt64(plan.ObservedTargetAPYBPS, observedSupply, projectedSupply)
		if !ok {
			return reservationEconomics{}, errors.New("projected target APY overflow")
		}
	}
	edge := projected - plan.SourceAPYBPS
	if edge < 1 {
		return reservationEconomics{}, errors.New("target capacity atomic economics became ineligible")
	}
	gross, ok := mulMulDivInt64(lease.PrincipalUSDMicros, edge, plan.HoldingHorizonSeconds, 10_000, secondsPerYear)
	if !ok {
		return reservationEconomics{}, errors.New("gross holding gain overflow")
	}
	expected, ok := mulDivInt64(gross, plan.ConfidencePPM, 1_000_000)
	if !ok {
		return reservationEconomics{}, errors.New("expected holding gain overflow")
	}
	guardedVariable, ok := mulDivInt64(plan.EstimatedExecutionCostUSDMicros, 12_500, 10_000)
	if !ok {
		return reservationEconomics{}, errors.New("guarded cost overflow")
	}
	guardedCost, ok := sumInt64(guardedVariable, 50_000)
	if !ok {
		return reservationEconomics{}, errors.New("guarded cost overflow")
	}
	net := expected - guardedCost
	if net < 100_000 {
		return reservationEconomics{}, errors.New("target capacity atomic net gain became ineligible")
	}
	feeCap, ok := mulDivInt64(net, 50_000, 1_000_000)
	if !ok || feeCap < 5_000 {
		return reservationEconomics{}, errors.New("target capacity atomic fee budget became ineligible")
	}
	if feeCap > 50_000 {
		feeCap = 50_000
	}
	return reservationEconomics{plan.ObservedTargetAPYBPS, projected, plan.SourceAPYBPS, edge, net, feeCap}, nil
}

func nonemptyPlan(v []byte) string {
	if len(v) == 0 {
		return "{}"
	}
	return string(v)
}
