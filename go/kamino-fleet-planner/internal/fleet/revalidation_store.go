package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type RevalidationLease struct {
	OpportunityID, OptimizerEpochID int64
	IdempotencyKey, Owner           string
	FencingToken                    int64
	ExpiresAt                       time.Time
	Cluster                         string
	VaultID                         int64
	VaultPubkey                     string
	VaultIndex                      uint8
	PolicyAccount                   string
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
 WHERE o.cluster=$1 AND o.available_at<=clock_timestamp()
   AND o.execution_plan->>'route_kind' IN ('same_mint','cross_mint_jupiter')
   AND o.execution_plan->>'source_kind'='reserve_position'
   AND (($6 AND o.execution_plan->>'route_kind'='cross_mint_jupiter'
         AND 'cross_mint_jupiter'=ANY(candidate_policy.route_modes))
     OR (o.execution_plan->>'route_kind'='same_mint'
         AND 'same_mint_kamino'=ANY(candidate_policy.route_modes)))
   AND ($5='' OR (candidate_policy.cluster=$1
        AND candidate_policy.source_commitment='finalized'
        AND candidate_policy.finalized_eligible
        AND $5=ANY(candidate_policy.delegated_signers)))
   AND o.source_reserve IS NOT NULL
   AND o.liquidity_mint=o.target_liquidity_mint
   AND ((o.execution_plan->>'route_kind'='same_mint'
         AND o.source_liquidity_mint=o.target_liquidity_mint)
     OR (o.execution_plan->>'route_kind'='cross_mint_jupiter'
         AND o.source_liquidity_mint<>o.target_liquidity_mint))
   AND o.expires_at>clock_timestamp()+interval '60 seconds'
   AND e.expires_at>clock_timestamp()+interval '60 seconds'
   AND (o.opportunity_state='revalidate'
        OR ($4 AND o.execution_plan->>'route_kind'='same_mint' AND o.opportunity_state='ready')
        OR (o.opportunity_state='leased' AND o.lease_kind='revalidate' AND o.lease_expires_at<=clock_timestamp()))
   AND (o.lease_expires_at IS NULL OR o.lease_expires_at<=clock_timestamp())
 ORDER BY o.scheduler_priority_anchor DESC,o.economic_priority DESC,o.created_at,o.id
 FOR UPDATE OF o SKIP LOCKED LIMIT 1
), claimed AS (
 UPDATE loyal_yield.rebalance_opportunities o SET opportunity_state='leased',lease_kind='revalidate',lease_owner=$2,
 lease_expires_at=clock_timestamp()+$3::interval,fencing_token=fencing_token+1,attempt_count=attempt_count+1,updated_at=clock_timestamp()
 FROM candidate WHERE o.id=candidate.id RETURNING o.*)
SELECT claimed.id,claimed.optimizer_epoch_id,claimed.idempotency_key,claimed.fencing_token,
       claimed.lease_expires_at,claimed.vault_id,vault.vault_pubkey,vault.vault_index,
       policy.policy_account,policy.delegated_signers,claimed.source_reserve,
       claimed.target_reserve,claimed.liquidity_mint,
       claimed.source_liquidity_mint,claimed.target_liquidity_mint,
       claimed.execution_plan->>'route_kind',claimed.amount_raw,
       COALESCE((claimed.execution_plan->>'source_collateral_amount_raw')::bigint,0),
       claimed.principal_usd_micros,claimed.source_apy_bps,claimed.target_apy_bps,
       claimed.estimated_edge_bps,claimed.expected_net_gain_usd_micros,
       claimed.estimated_cost_lamports,epoch.epoch_key,claimed.execution_plan
FROM claimed
JOIN loyal_yield.optimizer_epochs epoch ON epoch.id=claimed.optimizer_epoch_id
JOIN loyal_yield.managed_vaults vault ON vault.id=claimed.vault_id AND vault.active
JOIN loyal_yield.route_policies policy ON policy.id=vault.active_policy_id AND policy.active`, cluster, owner, ttl.String(), includeReady, signer, crossMintEnabled).Scan(
		&l.OpportunityID, &l.OptimizerEpochID, &l.IdempotencyKey, &l.FencingToken,
		&l.ExpiresAt, &l.VaultID, &l.VaultPubkey, &l.VaultIndex, &l.PolicyAccount,
		&l.DelegatedSigners, &l.SourceReserve, &l.TargetReserve, &l.LiquidityMint,
		&l.SourceLiquidityMint, &l.TargetLiquidityMint, &l.RouteKind,
		&l.LiquidityAmountRaw, &l.SourceCollateralRaw, &l.PrincipalUSDMicros,
		&l.SourceAPYBPS, &l.TargetAPYBPS, &l.EdgeBPS, &l.NetGainUSDMicros,
		&l.FeeCapLamports, &l.OptimizerEpochKey, &l.ExecutionPlan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim revalidation: %w", err)
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
SELECT route_table.table_address,
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
GROUP BY route_table.id,route_table.table_address
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
		if err := rows.Scan(&table.Address, &table.Addresses, &table.UsableAfterSlot, &table.LastVerifiedSlot); err != nil {
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
	defer tx.Rollback(ctx)
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
	defer tx.Rollback(ctx)
	var currentKey, state, kind, owner string
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
		if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.target_capacity_reservations(cluster,target_reserve,liquidity_mint,opportunity_id,principal_usd_micros,admitted_observed_supply_usd_micros,admitted_observed_slot,admitted_maximum_inflight_usd_micros,admitted_telemetry_version,reservation_generation,admitted_observed_target_apy_bps,admitted_projected_target_apy_bps,admitted_source_apy_bps,admitted_edge_bps,admitted_net_holding_gain_usd_micros,admitted_fee_cap_lamports,reservation_fencing_token) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, lease.Cluster, lease.TargetReserve, lease.LiquidityMint, lease.OpportunityID, lease.PrincipalUSDMicros, observedSupply, observedSlot, max, telemetryVersion, reservationGeneration, economics.ObservedTargetAPYBPS, economics.ProjectedTargetAPYBPS, economics.SourceAPYBPS, economics.EdgeBPS, economics.NetGainUSDMicros, economics.FeeCapLamports, token); err != nil {
			return fmt.Errorf("reserve target capacity: %w", err)
		}
		for _, key := range keys {
			tag, err := tx.Exec(ctx, `INSERT INTO loyal_yield.route_account_conflict_leases(cluster,writable_account_key,opportunity_id,lease_owner,fencing_token,expires_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(cluster,writable_account_key) DO NOTHING`, lease.Cluster, key, lease.OpportunityID, lease.Owner, token, lease.ExpiresAt)
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
	missing = canonicalStrings(missing)
	if len(missing) == 0 {
		return 0, errors.New("waiting_alt requires missing ALT addresses")
	}
	addresses := make([]provisioningAddress, len(missing))
	for i, address := range missing {
		if address == "" {
			return 0, errors.New("waiting_alt contains an empty ALT address")
		}
		addresses[i] = provisioningAddress{Address: address, SemanticClass: "vault", Ordinal: int32(i), AccountRole: "route", Writable: false}
	}
	emptyHash := provisioningAddressesHash(nil)
	vaultHash := provisioningAddressesHash(addresses)
	var requestID int64
	err := tx.QueryRow(ctx, `
INSERT INTO loyal_yield.lookup_table_provisioning_requests(
 cluster,vault_id,route_fingerprint,requirements_fingerprint,
 desired_shared_hash,desired_vault_hash,desired_shared_address_count,
 desired_vault_address_count,request_status)
VALUES($1,$2,$3,$4,$5,$6,0,$7,'requested')
ON CONFLICT(cluster,vault_id,requirements_fingerprint) DO NOTHING
RETURNING id`, lease.Cluster, lease.VaultID, prep.RouteFingerprint, prep.RequirementsFingerprint, emptyHash, vaultHash, len(addresses)).Scan(&requestID)
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
	if !sealed || existingSharedHash != emptyHash || existingVaultHash != vaultHash || sharedCount != 0 || vaultCount != len(addresses) {
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
	if provisioningAddressesHash(persisted) != vaultHash {
		return 0, errors.New("sealed ALT provisioning request address collision")
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

type reservationEconomics struct {
	ObservedTargetAPYBPS  int64
	ProjectedTargetAPYBPS int64
	SourceAPYBPS          int64
	EdgeBPS               int64
	NetGainUSDMicros      int64
	FeeCapLamports        int64
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
