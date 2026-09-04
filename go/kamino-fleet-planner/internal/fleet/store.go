package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func OpenStore(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open fleet database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping fleet database: %w", err)
	}
	return &Store{pool: pool}, nil
}
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

func (s *Store) LoadVaultPosition(ctx context.Context, cluster string, vaultID int64, source, target ReserveIdentity) (VaultPosition, error) {
	var position VaultPosition
	position.VaultID = vaultID
	var metadata json.RawMessage
	err := s.pool.QueryRow(ctx, `
SELECT vault.settings, vault.vault_index, vault.vault_pubkey,
       policy.id, policy.policy_account,
       position.reserve, position.market, position.liquidity_mint,
       position.amount_raw, position.snapshot_id, position.observed_slot,
       position.observed_at, position.planning_metadata,
       CASE
         WHEN NOT vault.active OR NOT policy.active THEN 'inactive_vault_or_policy'
         WHEN NOT ('same_mint_kamino'=ANY(policy.route_modes)) THEN 'route_mode_not_allowed'
         WHEN NOT ($4=ANY(policy.stable_mints) AND $4=ANY(policy.kamino_liquidity_mints)) THEN 'mint_not_allowed'
         WHEN NOT ($3=ANY(policy.kamino_markets) AND $7=ANY(policy.kamino_markets)) THEN 'market_not_allowed'
         WHEN position.observed_at < clock_timestamp()-interval '5 minutes' THEN 'stale_vault_position'
         WHEN EXISTS (SELECT 1 FROM loyal_yield.rebalance_opportunities opportunity WHERE opportunity.cluster=$5 AND opportunity.vault_id=vault.id AND opportunity.opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created')) THEN 'active_opportunity'
         WHEN EXISTS (SELECT 1 FROM loyal_yield.rebalance_decisions decision WHERE decision.vault_id=vault.id AND decision.status::text IN ('planned','simulating','ready','submitted','confirming')) THEN 'active_decision'
         WHEN EXISTS (SELECT 1 FROM loyal_yield.rebalance_decisions decision WHERE decision.vault_id=vault.id AND decision.status::text='confirmed' AND decision.source_reserve=$2 AND decision.updated_at>=clock_timestamp()-interval '5 minutes') THEN 'cooldown'
         ELSE ''
       END AS blocked_reason,
       COALESCE((SELECT sum(opportunity.principal_usd_micros) FROM loyal_yield.rebalance_opportunities opportunity WHERE opportunity.cluster=$5 AND opportunity.target_reserve=$2 AND opportunity.opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created')),0),
       COALESCE((SELECT sum(opportunity.principal_usd_micros) FROM loyal_yield.rebalance_opportunities opportunity WHERE opportunity.cluster=$5 AND opportunity.source_reserve=$2 AND opportunity.opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created')),0),
       COALESCE((SELECT sum(opportunity.principal_usd_micros) FROM loyal_yield.rebalance_opportunities opportunity WHERE opportunity.cluster=$5 AND opportunity.target_reserve=$1 AND opportunity.opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created')),0),
       COALESCE((SELECT sum(opportunity.principal_usd_micros) FROM loyal_yield.rebalance_opportunities opportunity WHERE opportunity.cluster=$5 AND opportunity.source_reserve=$1 AND opportunity.opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created')),0)
FROM loyal_yield.managed_vaults vault
JOIN loyal_yield.route_policies policy ON policy.id=vault.active_policy_id
JOIN loyal_yield.vault_reserve_positions_current position ON position.vault_id=vault.id AND position.reserve=$2
WHERE vault.id=$6 AND position.has_value AND position.amount_raw>0
`, target.Address, source.Address, source.Market, source.Mint, cluster, vaultID, target.Market).Scan(
		&position.Settings, &position.VaultIndex, &position.VaultPubkey, &position.PolicyID, &position.PolicyAccount,
		&position.SourceReserve, &position.Market, &position.Mint, &position.SourceCollateralAmountRaw, &position.SnapshotID,
		&position.ObservedSlot, &position.ObservedAt, &metadata, &position.BlockedReason,
		&position.SourceCommittedInflowUSDMicros, &position.SourceCommittedOutflowUSDMicros,
		&position.TargetCommittedInflowUSDMicros, &position.TargetCommittedOutflowUSDMicros)
	if err != nil {
		return VaultPosition{}, fmt.Errorf("load fixed cohort vault: %w", err)
	}
	var evidence struct {
		AmountSemantics  string          `json:"amount_semantics"`
		Redeemable       json.RawMessage `json:"redeemable_source_liquidity_amount_raw"`
		RedeemableLegacy json.RawMessage `json:"redeemable_liquidity_amount_raw"`
		SourceCollateral json.RawMessage `json:"source_collateral_amount_raw"`
		IdleLiquidity    json.RawMessage `json:"idle_vault_liquidity_amount_raw"`
		IdleLegacy       json.RawMessage `json:"vault_liquidity_amount_raw"`
	}
	if err := json.Unmarshal(metadata, &evidence); err != nil {
		return VaultPosition{}, fmt.Errorf("decode route amount evidence: %w", err)
	}
	position.SourceAmountSemantics = evidence.AmountSemantics
	switch evidence.AmountSemantics {
	case amountSemanticsKaminoCollateralDeposited:
		position.AmountRaw, err = jsonInt64(evidence.Redeemable)
		if err != nil {
			position.AmountRaw, err = jsonInt64(evidence.RedeemableLegacy)
		}
	case amountSemanticsRedeemableLiquidity:
		position.AmountRaw = position.SourceCollateralAmountRaw
		if raw, parseErr := jsonInt64(evidence.SourceCollateral); parseErr == nil {
			position.SourceCollateralAmountRaw = raw
		} else {
			err = parseErr
		}
	default:
		err = fmt.Errorf("unsupported amount semantics")
	}
	if err != nil || position.AmountRaw <= 0 || position.SourceCollateralAmountRaw <= 0 {
		return VaultPosition{}, fmt.Errorf("fixed cohort requires exact redeemable and collateral amounts")
	}
	idleLiquidity, idleErr := jsonInt64(evidence.IdleLiquidity)
	if idleErr != nil {
		idleLiquidity, idleErr = jsonInt64(evidence.IdleLegacy)
	}
	if idleErr == nil && idleLiquidity >= 0 {
		position.IdleVaultLiquidityAmountRaw = &idleLiquidity
	}
	return position, nil
}

// LoadMigratedFleet reads every active same-mint Kamino vault and its current
// source position in one repeatable-read snapshot. The epoch is the complete
// reserve denominator; no configured source/target pair can narrow this set.
func (s *Store) LoadMigratedFleet(ctx context.Context, cluster string, epoch ImmutableMarketEpoch) ([]FleetVault, error) {
	if cluster == "" || len(epoch.Reserves) < 2 {
		return nil, fmt.Errorf("cluster and complete reserve epoch are required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
SELECT DISTINCT ON (vault.id)
       vault.id, vault.settings, vault.vault_index, vault.vault_pubkey,
       policy.id, policy.policy_account, policy.kamino_markets,
       position.reserve, position.market, position.liquidity_mint,
       position.amount_raw, position.snapshot_id, position.observed_slot,
       position.observed_at, position.planning_metadata
FROM loyal_yield.managed_vaults vault
JOIN loyal_yield.route_policies policy ON policy.id=vault.active_policy_id
JOIN loyal_yield.vault_reserve_positions_current position ON position.vault_id=vault.id
WHERE vault.active AND policy.active
  AND 'same_mint_kamino'=ANY(policy.route_modes)
  AND position.has_value AND position.amount_raw>0
  AND position.liquidity_mint=ANY(policy.stable_mints)
  AND position.liquidity_mint=ANY(policy.kamino_liquidity_mints)
  AND position.market=ANY(policy.kamino_markets)
  AND NOT EXISTS (SELECT 1 FROM loyal_yield.rebalance_opportunities o WHERE o.cluster=$1 AND o.vault_id=vault.id AND o.opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created'))
  AND NOT EXISTS (SELECT 1 FROM loyal_yield.rebalance_decisions d WHERE d.vault_id=vault.id AND d.status::text IN ('planned','simulating','ready','submitted','confirming'))
  AND NOT EXISTS (SELECT 1 FROM loyal_yield.rebalance_decisions d WHERE d.vault_id=vault.id AND d.status::text='confirmed' AND d.source_reserve=position.reserve AND d.updated_at>=clock_timestamp()-interval '5 minutes')
ORDER BY vault.id, position.amount_raw DESC, position.reserve`, cluster)
	if err != nil {
		return nil, fmt.Errorf("load migrated fleet: %w", err)
	}
	defer rows.Close()
	var fleet []FleetVault
	for rows.Next() {
		var p VaultPosition
		var metadata json.RawMessage
		var markets []string
		if err := rows.Scan(&p.VaultID, &p.Settings, &p.VaultIndex, &p.VaultPubkey, &p.PolicyID, &p.PolicyAccount, &markets, &p.SourceReserve, &p.Market, &p.Mint, &p.SourceCollateralAmountRaw, &p.SnapshotID, &p.ObservedSlot, &p.ObservedAt, &metadata); err != nil {
			return nil, err
		}
		var evidence struct {
			AmountSemantics  string          `json:"amount_semantics"`
			Redeemable       json.RawMessage `json:"redeemable_source_liquidity_amount_raw"`
			RedeemableLegacy json.RawMessage `json:"redeemable_liquidity_amount_raw"`
			SourceCollateral json.RawMessage `json:"source_collateral_amount_raw"`
			Idle             json.RawMessage `json:"idle_vault_liquidity_amount_raw"`
		}
		if json.Unmarshal(metadata, &evidence) != nil {
			return nil, fmt.Errorf("vault %d has invalid planning metadata", p.VaultID)
		}
		p.SourceAmountSemantics = evidence.AmountSemantics
		if p.AmountRaw, err = jsonInt64(evidence.Redeemable); err != nil {
			p.AmountRaw, err = jsonInt64(evidence.RedeemableLegacy)
		}
		if raw, e := jsonInt64(evidence.SourceCollateral); e == nil {
			p.SourceCollateralAmountRaw = raw
		}
		if idle, e := jsonInt64(evidence.Idle); e == nil {
			p.IdleVaultLiquidityAmountRaw = &idle
		}
		if err != nil || p.AmountRaw <= 0 || p.SourceCollateralAmountRaw <= 0 {
			return nil, fmt.Errorf("vault %d lacks executable amount evidence", p.VaultID)
		}
		allowed := []string{}
		for _, reserve := range epoch.Reserves {
			if reserve.LiquidityMint == p.Mint && reserve.Market != nil && contains(markets, *reserve.Market) && reserve.TargetEligible {
				allowed = append(allowed, reserve.Reserve)
			}
		}
		fleet = append(fleet, FleetVault{Position: p, AllowedTargets: canonicalStrings(allowed)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	committedInflow, committedOutflow := map[string]int64{}, map[string]int64{}
	commitRows, err := tx.Query(ctx, `
SELECT reserve,sum(inflow),sum(outflow) FROM (
 SELECT target_reserve AS reserve,principal_usd_micros AS inflow,0::bigint AS outflow
 FROM loyal_yield.rebalance_opportunities WHERE cluster=$1 AND opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created')
 UNION ALL
 SELECT source_reserve AS reserve,0::bigint,principal_usd_micros
 FROM loyal_yield.rebalance_opportunities WHERE cluster=$1 AND source_reserve IS NOT NULL AND opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created')
) committed GROUP BY reserve`, cluster)
	if err != nil {
		return nil, fmt.Errorf("load committed reserve frontier: %w", err)
	}
	for commitRows.Next() {
		var reserve string
		var inflow, outflow int64
		if err := commitRows.Scan(&reserve, &inflow, &outflow); err != nil {
			commitRows.Close()
			return nil, err
		}
		if inflow < 0 || outflow < 0 {
			commitRows.Close()
			return nil, errors.New("negative committed reserve frontier")
		}
		committedInflow[reserve], committedOutflow[reserve] = inflow, outflow
	}
	if err := commitRows.Err(); err != nil {
		commitRows.Close()
		return nil, err
	}
	commitRows.Close()
	for i := range fleet {
		fleet[i].CommittedInflows, fleet[i].CommittedOutflows = committedInflow, committedOutflow
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return fleet, nil
}

func jsonInt64(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, fmt.Errorf("amount is absent")
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, err
	}
	number, parseErr := strconv.ParseInt(text, 10, 64)
	return number, parseErr
}

// EnsureOptimizerEpoch resolves the database-assigned epoch identity before
// planning. Rust includes this ID in the opportunity key, so publication plans
// must not use a synthetic fingerprint-derived ID.
func (s *Store) EnsureOptimizerEpoch(ctx context.Context, cluster string, epoch ImmutableMarketEpoch) (int64, error) {
	if cluster == "" {
		return 0, fmt.Errorf("cluster is required")
	}
	if err := epoch.Validate(); err != nil {
		return 0, err
	}
	durable := epoch.DurableEvidence()
	marketState, err := json.Marshal(durable)
	if err != nil {
		return 0, err
	}
	var epochID int64
	var evidenceMatches bool
	err = s.pool.QueryRow(ctx, `
WITH inserted AS (
  INSERT INTO loyal_yield.optimizer_epochs (cluster,epoch_key,market_slot,observed_at,expires_at,market_state)
  VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (cluster,epoch_key) DO NOTHING
  RETURNING id,market_slot,observed_at,expires_at,market_state
), candidate AS (
  SELECT id,market_slot,observed_at,expires_at,market_state FROM inserted
  UNION ALL
  SELECT id,market_slot,observed_at,expires_at,market_state FROM loyal_yield.optimizer_epochs
  WHERE cluster=$1 AND epoch_key=$2 AND NOT EXISTS (SELECT 1 FROM inserted)
)
SELECT id, market_slot=$3 AND observed_at=$4 AND expires_at=$5 AND market_state=$6::jsonb
FROM candidate LIMIT 1`, cluster, durable.Fingerprint, *durable.MaximumMarketSlot, durable.CapturedAt, durable.OptimizerEnvelopeExpiresAt(), marketState).Scan(&epochID, &evidenceMatches)
	if err != nil {
		return 0, err
	}
	if !evidenceMatches {
		return 0, fmt.Errorf("optimizer epoch key is stored under different immutable evidence")
	}
	return epochID, nil
}

func (s *Store) Publish(ctx context.Context, cluster string, epoch ImmutableMarketEpoch, position VaultPosition, decision Decision) (PublishResult, error) {
	if cluster == "" {
		return PublishResult{}, fmt.Errorf("cluster is required")
	}
	if err := decision.Validate(); err != nil {
		return PublishResult{}, err
	}
	if err := epoch.Validate(); err != nil {
		return PublishResult{}, err
	}
	epochExpires, complete := epoch.MintExpiresAt(decision.Mint)
	if !complete || time.Until(epochExpires) < minimumPublicationLifetime {
		return PublishResult{Reason: "epoch_lifetime_too_short"}, nil
	}
	sourceEvidence, sourcePresent := epoch.Reserve(decision.SourceReserve)
	targetEvidence, targetPresent := epoch.Reserve(decision.TargetReserve)
	if !sourcePresent || !targetPresent || !targetEvidence.TargetEligible || sourceEvidence.LiquidityMint != decision.Mint || targetEvidence.LiquidityMint != decision.Mint {
		return PublishResult{}, fmt.Errorf("opportunity reserves are not covered by the complete immutable mint frontier")
	}
	durableEpoch := epoch.DurableEvidence()
	marketState, err := json.Marshal(durableEpoch)
	if err != nil {
		return PublishResult{}, err
	}
	planJSON, err := canonicalSameMintExecutionPlan(position, decision, sourceEvidence.SupplyAPYBPS, targetEvidence.SupplyAPYBPS, targetEvidence.Slot, targetEvidence.ObservedAt)
	if err != nil {
		return PublishResult{}, err
	}
	epochKey := durableEpoch.Fingerprint
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PublishResult{}, err
	}
	defer tx.Rollback(ctx)
	// This is the same per-vault publication mutex used by the Rust queue API.
	// The cutover still requires stop-then-start singleton operation, while this
	// row lock, economic idempotency key, and active-opportunity slot make each
	// durable admission atomic without planner-specific persistence.
	var vaultActive bool
	err = tx.QueryRow(ctx, `SELECT active FROM loyal_yield.managed_vaults WHERE id=$1 FOR UPDATE`, decision.VaultID).Scan(&vaultActive)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !vaultActive {
		return PublishResult{}, fmt.Errorf("cannot queue opportunity for missing or inactive vault %d", decision.VaultID)
	}
	if err != nil {
		return PublishResult{}, err
	}
	var epochID int64
	var evidenceMatches bool
	if err := tx.QueryRow(ctx, `
WITH inserted AS (
  INSERT INTO loyal_yield.optimizer_epochs (cluster,epoch_key,market_slot,observed_at,expires_at,market_state)
  VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (cluster,epoch_key) DO NOTHING
  RETURNING id,market_slot,observed_at,expires_at,market_state
), candidate AS (
  SELECT id,market_slot,observed_at,expires_at,market_state FROM inserted
  UNION ALL
  SELECT id,market_slot,observed_at,expires_at,market_state
  FROM loyal_yield.optimizer_epochs
  WHERE cluster=$1 AND epoch_key=$2 AND NOT EXISTS (SELECT 1 FROM inserted)
)
SELECT id, market_slot=$3 AND observed_at=$4 AND expires_at=$5 AND market_state=$6::jsonb
FROM candidate LIMIT 1`, cluster, epochKey, *durableEpoch.MaximumMarketSlot, durableEpoch.CapturedAt, durableEpoch.OptimizerEnvelopeExpiresAt(), marketState).Scan(&epochID, &evidenceMatches); err != nil {
		return PublishResult{}, err
	}
	if !evidenceMatches {
		return PublishResult{}, fmt.Errorf("optimizer epoch key is stored under different immutable evidence")
	}
	key := opportunityIdentity(cluster, epochID, decision, planJSON, epochExpires)
	var existingID int64
	err = tx.QueryRow(ctx, `SELECT id FROM loyal_yield.rebalance_opportunities WHERE idempotency_key=$1`, key).Scan(&existingID)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return PublishResult{}, err
		}
		return PublishResult{OpportunityID: existingID, EpochID: epochID, Reason: "rust_identity_duplicate"}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PublishResult{}, err
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.rebalance_opportunities WHERE cluster=$1 AND vault_id=$2 AND opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created')) OR EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions WHERE vault_id=$2 AND status::text IN ('planned','simulating','ready','submitted','confirming'))`, cluster, decision.VaultID).Scan(&active); err != nil {
		return PublishResult{}, err
	}
	if active {
		if err := tx.Commit(ctx); err != nil {
			return PublishResult{}, err
		}
		return PublishResult{EpochID: epochID, Reason: "active_work"}, nil
	}
	var opportunityID int64
	err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.rebalance_opportunities
(cluster,idempotency_key,rediscovery_key,attempt_generation,vault_id,source_snapshot_id,optimizer_epoch_id,source_reserve,target_reserve,liquidity_mint,source_liquidity_mint,target_liquidity_mint,amount_raw,principal_usd_micros,source_apy_bps,target_apy_bps,estimated_edge_bps,estimated_cost_lamports,annual_yield_gain_usd_micros,expected_net_gain_usd_micros,economic_priority,priority_version,operation_class,opportunity_state,execution_plan,available_at,expires_at)
VALUES ($1,$2,$2,1,$3,$4,$5,$6,$7,$8,$8,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,'lost-yield-service-net-reserve-capacity-v3','yield_optimization','revalidate',$18,clock_timestamp(),$19) RETURNING id`, cluster, key, decision.VaultID, decision.SourceSnapshotID, epochID, decision.SourceReserve, decision.TargetReserve, decision.Mint, decision.AmountRaw, decision.PrincipalUSDMicros, decision.SourceAPYBPS, decision.TargetAPYBPS, decision.EdgeBPS, decision.EstimatedCostLamports, decision.AnnualYieldGainUSDMicros, decision.ExpectedNetGainUSDMicros, decision.EconomicPriority, planJSON, epochExpires).Scan(&opportunityID)
	if err != nil {
		return PublishResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PublishResult{}, err
	}
	return PublishResult{Inserted: true, OpportunityID: opportunityID, EpochID: epochID, Reason: "published"}, nil
}
