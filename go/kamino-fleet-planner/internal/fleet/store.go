package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func (s *Store) Publish(ctx context.Context, cluster string, snapshot MarketSnapshot, position VaultPosition, decision Decision) (PublishResult, error) {
	if cluster == "" {
		return PublishResult{}, fmt.Errorf("cluster is required")
	}
	if err := decision.Validate(); err != nil {
		return PublishResult{}, err
	}
	// A stale source remains withdrawable after the route revalidator's refresh
	// instruction. The eligible target, not stale source economics, bounds this
	// same-mint route's publication lifetime.
	economicLifetime := snapshot.Reserves[decision.TargetReserve].EconomicLifetimeMillis
	epochLifetime := minimumInt64(economicLifetime, int64((90 * time.Second).Milliseconds()))
	epochExpires := snapshot.ObservedAt.Add(time.Duration(epochLifetime) * time.Millisecond)
	if time.Until(epochExpires) < minimumPublicationLifetime {
		return PublishResult{Reason: "epoch_lifetime_too_short"}, nil
	}
	marketState, err := json.Marshal(map[string]any{"schemaVersion": 1, "owner": "kamino_fleet_planner_go_v1", "snapshotHash": snapshot.Hash, "reserves": snapshot.Reserves})
	if err != nil {
		return PublishResult{}, err
	}
	feeTier := "base"
	if decision.EstimatedCostLamports >= 50_000 {
		feeTier = "high_value"
	} else if decision.EstimatedCostLamports >= 15_000 {
		feeTier = "standard"
	}
	plan := map[string]any{
		"kind": "same_mint", "route_kind": "same_mint", "settings": position.Settings, "vault_index": position.VaultIndex,
		"vault_pubkey": position.VaultPubkey, "policy_id": position.PolicyID, "source_kind": "reserve_position",
		"source_reserve": decision.SourceReserve, "target_reserve": decision.TargetReserve, "liquidity_mint": decision.Mint,
		"source_liquidity_mint": decision.Mint, "target_liquidity_mint": decision.Mint, "amount_raw": decision.AmountRaw,
		"route_amount_semantics": amountSemanticsRedeemableLiquidity, "source_amount_semantics": position.SourceAmountSemantics,
		"source_collateral_amount_raw": position.SourceCollateralAmountRaw, "redeemable_source_liquidity_amount_raw": decision.AmountRaw,
		"idle_vault_liquidity_amount_raw": position.IdleVaultLiquidityAmountRaw, "idle_token_account": nil,
		"source_apy_bps": decision.SourceAPYBPS, "observed_source_apy_bps": snapshot.Reserves[decision.SourceReserve].SupplyAPYBPS,
		"observed_target_apy_bps": snapshot.Reserves[decision.TargetReserve].SupplyAPYBPS, "target_apy_bps": decision.TargetAPYBPS,
		"capacity_adjusted_target_apy_bps": decision.TargetAPYBPS, "estimated_edge_bps": decision.EdgeBPS,
		"confidence_ppm": decision.ConfidencePPM, "expected_service_millis": expectedServiceMillis,
		"holding_horizon_seconds": decision.HoldingHorizonSeconds, "estimated_execution_cost_usd_micros": decision.EstimatedCostUSDMicros,
		"estimated_execution_costs": map[string]any{"kind": "same_mint", "route_usd_micros": decision.EstimatedCostUSDMicros},
		"fee_cap_lamports":          decision.EstimatedCostLamports, "fee_tier": feeTier, "fee_gain_fraction_ppm": 50_000,
		"minimum_transaction_fee_lamports": 5_000, "conservative_sol_price_usd_micros": 1_000_000_000,
		"source_observed_at": position.ObservedAt.UTC(), "source_observed_slot": position.ObservedSlot,
		"optimizer_market_slot": decision.MarketSlot, "target_observed_at": snapshot.ObservedAt, "target_observed_slot": snapshot.Slot,
		"writable_conflict_keys": []string{
			"vault:" + position.VaultPubkey,
			"policy:" + strconv.FormatInt(position.PolicyID, 10),
			"source-reserve:" + decision.SourceReserve,
			"target-reserve:" + decision.TargetReserve,
		}, "planning_economics_are_executable_quote": false,
		"fresh_executable_jupiter_minimum_output_required": false, "policy_bindings": nil,
		"source_recovery_anchor_collateral_raw": nil, "cross_mint_maximum_value_loss_bps": nil,
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return PublishResult{}, err
	}
	key := economicKey(cluster, position, decision)
	epochKey := "kamino-fleet-planner-go-v1:" + fmt.Sprint(snapshot.Slot) + ":" + snapshot.Hash
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
	var existingID int64
	err = tx.QueryRow(ctx, `SELECT id FROM loyal_yield.rebalance_opportunities WHERE idempotency_key=$1`, key).Scan(&existingID)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return PublishResult{}, err
		}
		return PublishResult{OpportunityID: existingID, Reason: "economic_duplicate"}, nil
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
		return PublishResult{Reason: "active_work"}, nil
	}
	var epochID int64
	if err := tx.QueryRow(ctx, `WITH inserted AS (INSERT INTO loyal_yield.optimizer_epochs (cluster,epoch_key,market_slot,observed_at,expires_at,market_state) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (cluster,epoch_key) DO NOTHING RETURNING id) SELECT id FROM inserted UNION ALL SELECT id FROM loyal_yield.optimizer_epochs WHERE cluster=$1 AND epoch_key=$2 LIMIT 1`, cluster, epochKey, snapshot.Slot, snapshot.ObservedAt, epochExpires, marketState).Scan(&epochID); err != nil {
		return PublishResult{}, err
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

func minimumInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func economicKey(cluster string, position VaultPosition, decision Decision) string {
	values := []any{"kamino-fleet-planner-economic-v1", cluster, decision.VaultID, position.PolicyID, position.Settings, position.VaultIndex, decision.SourceSnapshotID, decision.SourceReserve, decision.TargetReserve, decision.Mint, decision.AmountRaw, decision.SourceAPYBPS, decision.TargetAPYBPS, decision.EdgeBPS}
	encoded, _ := json.Marshal(values)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
