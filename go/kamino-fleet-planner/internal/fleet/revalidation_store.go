package fleet

import (
	"context"
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
func (s *Store) ClaimRevalidation(ctx context.Context, cluster, owner string, ttl time.Duration, includeReady bool) (*RevalidationLease, error) {
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
   AND o.expires_at>clock_timestamp()+interval '60 seconds'
   AND e.expires_at>clock_timestamp()+interval '60 seconds'
   AND (o.opportunity_state IN ('revalidate','waiting_alt')
        OR ($4 AND o.opportunity_state='ready')
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
       claimed.target_reserve,claimed.liquidity_mint,claimed.amount_raw,
       (claimed.execution_plan->>'source_collateral_amount_raw')::bigint,
       claimed.principal_usd_micros,claimed.source_apy_bps,claimed.target_apy_bps,
       claimed.estimated_edge_bps,claimed.expected_net_gain_usd_micros,
       claimed.estimated_cost_lamports,epoch.epoch_key,claimed.execution_plan
FROM claimed
JOIN loyal_yield.optimizer_epochs epoch ON epoch.id=claimed.optimizer_epoch_id
JOIN loyal_yield.managed_vaults vault ON vault.id=claimed.vault_id AND vault.active
JOIN loyal_yield.route_policies policy ON policy.id=vault.active_policy_id AND policy.active`, cluster, owner, ttl.String(), includeReady).Scan(
		&l.OpportunityID, &l.OptimizerEpochID, &l.IdempotencyKey, &l.FencingToken,
		&l.ExpiresAt, &l.VaultID, &l.VaultPubkey, &l.VaultIndex, &l.PolicyAccount,
		&l.DelegatedSigners, &l.SourceReserve, &l.TargetReserve, &l.LiquidityMint,
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

func (s *Store) LoadReusableLookupTables(ctx context.Context, cluster string, minimumSlot int64) ([]LookupTable, error) {
	if s == nil || s.pool == nil || cluster == "" || minimumSlot <= 0 {
		return nil, errors.New("invalid reusable ALT query")
	}
	rows, err := s.pool.Query(ctx, `
SELECT route_table.table_address,
       array_agg(address.address ORDER BY address.ordinal),
       min(address.usable_after_slot),min(address.last_verified_slot)
FROM loyal_yield.route_lookup_tables route_table
JOIN loyal_yield.lookup_table_addresses address ON address.route_lookup_table_id=route_table.id
WHERE route_table.cluster=$1 AND route_table.durable
  AND route_table.status IN ('active','usable')
  AND route_table.deactivated_slot IS NULL
GROUP BY route_table.id,route_table.table_address
HAVING max(address.usable_after_slot)<=$2 AND min(address.last_verified_slot)>=$2
ORDER BY route_table.table_address`, cluster, minimumSlot)
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

type RevalidationCommit struct {
	Disposition              string
	Preparation              *RoutePreparation
	MissingAddresses         []string
	ConflictKeys             []string
	ExpectedEpochFingerprint string
	ExpectedOpportunityKey   string
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
		if len(prep.Transaction.Message) == 0 || len(prep.Transaction.UnsignedWire) == 0 || prep.Transaction.PacketBytes > SolanaPacketLimit || prep.Transaction.FeeLamports > uint64(lease.FeeCapLamports) || prep.Transaction.ComputeLimit == 0 || prep.Transaction.ComputeLimit > defaultComputeLimit || !prep.Simulation.Succeeded || prep.Simulation.WireSHA256 != prep.Transaction.WireSHA256 {
			return errors.New("executable route bytes or simulation evidence is incomplete")
		}
	}
	next := "ready"
	var leaseKind any
	var leaseOwner any
	var leaseExpiry any
	if input.Disposition == "waiting_alt" {
		next = "waiting_alt"
	} else if input.Disposition == "fused_execute" {
		next = "leased"
		leaseKind = "execute"
		leaseOwner = lease.Owner
		leaseExpiry = lease.ExpiresAt
		reservationGeneration++
		if tag, err := tx.Exec(ctx, `UPDATE loyal_yield.target_capacity_frontiers SET reservation_generation=$4,updated_at=clock_timestamp() WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3`, lease.Cluster, lease.TargetReserve, lease.LiquidityMint, reservationGeneration); err != nil || tag.RowsAffected() != 1 {
			return errors.New("capacity frontier generation fence failed")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.target_capacity_reservations(cluster,target_reserve,liquidity_mint,opportunity_id,principal_usd_micros,admitted_observed_supply_usd_micros,admitted_observed_slot,admitted_maximum_inflight_usd_micros,admitted_telemetry_version,reservation_generation,admitted_observed_target_apy_bps,admitted_projected_target_apy_bps,admitted_source_apy_bps,admitted_edge_bps,admitted_net_holding_gain_usd_micros,admitted_fee_cap_lamports,reservation_fencing_token) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,$13,$14,$15,$16)`, lease.Cluster, lease.TargetReserve, lease.LiquidityMint, lease.OpportunityID, lease.PrincipalUSDMicros, observedSupply, observedSlot, max, telemetryVersion, reservationGeneration, lease.TargetAPYBPS, lease.SourceAPYBPS, lease.EdgeBPS, lease.NetGainUSDMicros, lease.FeeCapLamports, token); err != nil {
			return fmt.Errorf("reserve target capacity: %w", err)
		}
		for _, key := range keys {
			tag, err := tx.Exec(ctx, `INSERT INTO loyal_yield.route_account_conflict_leases(cluster,writable_account_key,opportunity_id,lease_owner,fencing_token,expires_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(cluster,writable_account_key) DO NOTHING`, lease.Cluster, key, lease.OpportunityID, lease.Owner, token, lease.ExpiresAt)
			if err != nil || tag.RowsAffected() != 1 {
				return errors.New("atomic conflict lease acquisition failed")
			}
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state=$5,lease_kind=$6,lease_owner=$7,lease_expires_at=$8,route_fingerprint=$9,requirements_fingerprint=$10,execution_plan=$11::jsonb,updated_at=clock_timestamp() WHERE id=$1 AND opportunity_state='leased' AND lease_kind='revalidate' AND lease_owner=$2 AND fencing_token=$3 AND optimizer_epoch_id=$4 AND lease_expires_at>clock_timestamp() AND expires_at>clock_timestamp()`, lease.OpportunityID, lease.Owner, token, epochID, next, leaseKind, leaseOwner, leaseExpiry, prep.RouteFingerprint, prep.RequirementsFingerprint, nonemptyPlan(prep.ExecutionPlan))
	if err != nil || tag.RowsAffected() != 1 {
		return errors.New("revalidation transition was fenced")
	}
	return tx.Commit(ctx)
}
func nonemptyPlan(v []byte) string {
	if len(v) == 0 {
		return "{}"
	}
	return string(v)
}
