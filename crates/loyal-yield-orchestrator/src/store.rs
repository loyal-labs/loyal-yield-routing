use crate::domain::{draft_same_mint_decision, state_transition, PlannedDecision};
use crate::types::*;
use crate::{OrchestratorError, ACTIVE_DECISION_STATUSES};
use chrono::{DateTime, Utc};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use sqlx::postgres::PgPoolOptions;
use sqlx::{PgConnection, PgPool, Row};

const MIGRATION_0001: &str = include_str!("../migrations/0001_loyal_yield_orchestration.sql");

#[derive(Clone)]
pub struct NeonSqlClient {
    pool: PgPool,
}

pub type OrchestratorStore = NeonSqlClient;

#[derive(Debug)]
struct RoutePolicyRow {
    id: i64,
    cluster: String,
    settings: String,
    authority: String,
    policy_seed: i64,
    policy_account: String,
    vault_index: i16,
    vault_pubkey: String,
    delegated_signers: Vec<String>,
    threshold: i32,
    route_modes: Vec<String>,
    stable_mints: Vec<String>,
    kamino_markets: Vec<String>,
    kamino_liquidity_mints: Vec<String>,
    universe_preset: Option<String>,
    risk_profile: Option<String>,
    swap_lanes: Value,
    active: bool,
    first_seen_at: DateTime<Utc>,
    last_seen_at: DateTime<Utc>,
    last_seen_slot: i64,
    last_seen_signature: String,
}

#[derive(Debug)]
struct ManagedVaultRow {
    id: i64,
    cluster: String,
    settings: String,
    vault_index: i16,
    vault_pubkey: String,
    active_policy_id: i64,
    active: bool,
    first_seen_at: DateTime<Utc>,
    last_seen_at: DateTime<Utc>,
}

#[derive(Debug)]
struct SnapshotRow {
    id: i64,
    vault_id: i64,
    policy_id: i64,
    observed_slot: i64,
    observed_at: DateTime<Utc>,
    chain_slot: Option<i64>,
    lock_attempt_id: Option<i64>,
    is_current: bool,
    context: Value,
}

#[derive(Debug)]
struct CurrentPositionRow {
    vault_id: i64,
    reserve: String,
    market: Option<String>,
    liquidity_mint: String,
    amount_raw: i64,
    supply_apy_bps: Option<i64>,
    borrow_apy_bps: Option<i64>,
    has_value: bool,
    snapshot_id: i64,
    observed_slot: i64,
    observed_at: DateTime<Utc>,
    planning_metadata: Value,
}

#[derive(Debug)]
struct DecisionRow {
    id: i64,
    vault_id: i64,
    source_snapshot_id: Option<i64>,
    status: String,
    source_reserve: Option<String>,
    target_reserve: Option<String>,
    liquidity_mint: Option<String>,
    source_liquidity_mint: Option<String>,
    target_liquidity_mint: Option<String>,
    amount_raw: Option<i64>,
    source_apy_bps: Option<i64>,
    target_apy_bps: Option<i64>,
    estimated_edge_bps: Option<i64>,
    estimated_cost_lamports: i64,
    decision_reason: String,
    execution_plan: Value,
    abandon_reason: Option<String>,
    signature: Option<String>,
    submitted_slot: Option<i64>,
    confirmed_slot: Option<i64>,
    preflight_chain_slot: Option<i64>,
    post_snapshot_id: Option<i64>,
    created_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
}

impl NeonSqlClient {
    pub async fn connect(config: NeonSqlConfig) -> Result<Self, OrchestratorError> {
        let pool = PgPoolOptions::new()
            .max_connections(config.max_connections)
            .acquire_timeout(config.acquire_timeout)
            .connect(&config.url)
            .await?;
        Ok(Self { pool })
    }

    pub fn from_pool(pool: PgPool) -> Self {
        Self { pool }
    }

    pub fn pool(&self) -> &PgPool {
        &self.pool
    }

    pub async fn apply_migrations(&self) -> Result<(), OrchestratorError> {
        sqlx::raw_sql(MIGRATION_0001).execute(&self.pool).await?;
        Ok(())
    }

    pub async fn record_policy_match(
        &self,
        event: PolicyMatchInput,
    ) -> Result<StoredPolicyMatch, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let policy = upsert_policy(&mut *tx, &event).await?;
        let vault = upsert_vault(&mut *tx, policy.id, &event).await?;
        tx.commit().await?;
        Ok(StoredPolicyMatch { policy, vault })
    }

    pub async fn record_balance_sweep_policy_match(
        &self,
        event: BalanceSweepPolicyMatchInput,
    ) -> Result<BalanceSweepTarget, OrchestratorError> {
        let policy_seed = to_i64_policy_seed(event.policy_seed)?;
        let slot = to_i64_slot(event.slot)?;
        let max_amount_per_period = to_i64_amount(event.max_amount_per_period)?;
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.balance_sweep_targets
                (cluster, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
                 wallet, wallet_usdc_ata, vault_usdc_ata, delegated_signers, threshold,
                 max_amount_per_period, active, last_seen_slot, last_seen_signature)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, TRUE, $14, $15)
            ON CONFLICT (cluster, policy_account) DO UPDATE SET
                settings = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.settings
                    ELSE loyal_yield.balance_sweep_targets.settings
                END,
                authority = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.authority
                    ELSE loyal_yield.balance_sweep_targets.authority
                END,
                vault_index = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.vault_index
                    ELSE loyal_yield.balance_sweep_targets.vault_index
                END,
                vault_pubkey = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.vault_pubkey
                    ELSE loyal_yield.balance_sweep_targets.vault_pubkey
                END,
                wallet = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.wallet
                    ELSE loyal_yield.balance_sweep_targets.wallet
                END,
                wallet_usdc_ata = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.wallet_usdc_ata
                    ELSE loyal_yield.balance_sweep_targets.wallet_usdc_ata
                END,
                vault_usdc_ata = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.vault_usdc_ata
                    ELSE loyal_yield.balance_sweep_targets.vault_usdc_ata
                END,
                delegated_signers = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.delegated_signers
                    ELSE loyal_yield.balance_sweep_targets.delegated_signers
                END,
                threshold = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.threshold
                    ELSE loyal_yield.balance_sweep_targets.threshold
                END,
                max_amount_per_period = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.max_amount_per_period
                    ELSE loyal_yield.balance_sweep_targets.max_amount_per_period
                END,
                active = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN TRUE
                    ELSE loyal_yield.balance_sweep_targets.active
                END,
                last_seen_at = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN now()
                    ELSE loyal_yield.balance_sweep_targets.last_seen_at
                END,
                last_seen_slot = GREATEST(loyal_yield.balance_sweep_targets.last_seen_slot, EXCLUDED.last_seen_slot),
                last_seen_signature = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.last_seen_signature
                    ELSE loyal_yield.balance_sweep_targets.last_seen_signature
                END
            RETURNING
                id, cluster, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
                wallet, wallet_usdc_ata, vault_usdc_ata, delegated_signers, threshold,
                max_amount_per_period, active, first_seen_at, last_seen_at, last_seen_slot, last_seen_signature
            "#,
        )
        .bind(&event.cluster)
        .bind(&event.settings)
        .bind(&event.authority)
        .bind(policy_seed)
        .bind(&event.policy_account)
        .bind(i16::from(event.vault_index))
        .bind(&event.vault_pubkey)
        .bind(&event.wallet)
        .bind(&event.wallet_usdc_ata)
        .bind(&event.vault_usdc_ata)
        .bind(&event.delegated_signers)
        .bind(i32::from(event.threshold))
        .bind(max_amount_per_period)
        .bind(slot)
        .bind(&event.signature)
        .fetch_one(&self.pool)
        .await?;

        balance_sweep_target_from_row(&row)
    }

    pub async fn load_active_balance_sweep_targets(
        &self,
        cluster: &str,
    ) -> Result<Vec<BalanceSweepTarget>, OrchestratorError> {
        let rows = sqlx::query(
            r#"
            SELECT
                id, cluster, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
                wallet, wallet_usdc_ata, vault_usdc_ata, delegated_signers, threshold,
                max_amount_per_period, active, first_seen_at, last_seen_at, last_seen_slot, last_seen_signature
            FROM loyal_yield.balance_sweep_targets
            WHERE cluster = $1 AND active
            ORDER BY id
            "#,
        )
        .bind(cluster)
        .fetch_all(&self.pool)
        .await?;

        rows.iter().map(balance_sweep_target_from_row).collect()
    }

    pub async fn record_wallet_ata_balance_update(
        &self,
        input: WalletAtaBalanceUpdateInput,
    ) -> Result<WalletAtaBalanceCurrent, OrchestratorError> {
        let amount_raw = to_i64_amount(input.amount_raw)?;
        let observed_slot = to_i64_slot(input.observed_slot)?;
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.balance_sweep_wallet_balances_current
                (target_id, cluster, wallet, wallet_usdc_ata, amount_raw, owner, mint,
                 observed_slot, observed_at, source, source_commitment, account_data_hash, raw_evidence)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, now()), $10, $11, $12, $13)
            ON CONFLICT (target_id) DO UPDATE SET
                cluster = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN EXCLUDED.cluster
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.cluster
                END,
                wallet = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN EXCLUDED.wallet
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.wallet
                END,
                wallet_usdc_ata = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN EXCLUDED.wallet_usdc_ata
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.wallet_usdc_ata
                END,
                amount_raw = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN EXCLUDED.amount_raw
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.amount_raw
                END,
                owner = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN EXCLUDED.owner
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.owner
                END,
                mint = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN EXCLUDED.mint
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.mint
                END,
                observed_slot = GREATEST(loyal_yield.balance_sweep_wallet_balances_current.observed_slot, EXCLUDED.observed_slot),
                observed_at = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN EXCLUDED.observed_at
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.observed_at
                END,
                source = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN EXCLUDED.source
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.source
                END,
                source_commitment = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN EXCLUDED.source_commitment
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.source_commitment
                END,
                account_data_hash = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN EXCLUDED.account_data_hash
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.account_data_hash
                END,
                raw_evidence = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN EXCLUDED.raw_evidence
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.raw_evidence
                END,
                updated_at = CASE
                    WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                    THEN now()
                    ELSE loyal_yield.balance_sweep_wallet_balances_current.updated_at
                END
            RETURNING
                target_id, cluster, wallet, wallet_usdc_ata, amount_raw, owner, mint,
                observed_slot, observed_at, source, source_commitment, account_data_hash,
                raw_evidence, updated_at
            "#,
        )
        .bind(input.target_id.as_i64())
        .bind(&input.cluster)
        .bind(&input.wallet)
        .bind(&input.wallet_usdc_ata)
        .bind(amount_raw)
        .bind(input.owner.as_deref())
        .bind(&input.mint)
        .bind(observed_slot)
        .bind(input.observed_at)
        .bind(&input.source)
        .bind(&input.source_commitment)
        .bind(input.account_data_hash.as_deref())
        .bind(&input.raw_evidence)
        .fetch_one(&self.pool)
        .await?;

        wallet_ata_balance_from_row(&row)
    }

    pub async fn record_balance_sweep_execution(
        &self,
        input: BalanceSweepExecutionInput,
    ) -> Result<BalanceSweepExecution, OrchestratorError> {
        let slot = to_i64_slot(input.slot)?;
        let amount_raw = to_i64_amount(input.amount_raw)?;
        let source_pre_balance_raw = optional_to_i64_amount(input.source_pre_balance_raw)?;
        let source_post_balance_raw = optional_to_i64_amount(input.source_post_balance_raw)?;
        let destination_pre_balance_raw =
            optional_to_i64_amount(input.destination_pre_balance_raw)?;
        let destination_post_balance_raw =
            optional_to_i64_amount(input.destination_post_balance_raw)?;
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.balance_sweep_executions
                (target_id, cluster, signature, slot, source_wallet_ata, destination_vault_ata,
                 amount_raw, source_pre_balance_raw, source_post_balance_raw,
                 destination_pre_balance_raw, destination_post_balance_raw, source_commitment,
                 raw_evidence, decoded_evidence, received_at, decoded_at, dedupe_key)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
            ON CONFLICT (dedupe_key) DO UPDATE SET dedupe_key = EXCLUDED.dedupe_key
            RETURNING
                id, target_id, cluster, signature, slot, source_wallet_ata, destination_vault_ata,
                amount_raw, source_pre_balance_raw, source_post_balance_raw,
                destination_pre_balance_raw, destination_post_balance_raw, source_commitment,
                raw_evidence, decoded_evidence, received_at, decoded_at, inserted_at, dedupe_key
            "#,
        )
        .bind(input.target_id.as_i64())
        .bind(&input.cluster)
        .bind(&input.signature)
        .bind(slot)
        .bind(&input.source_wallet_ata)
        .bind(&input.destination_vault_ata)
        .bind(amount_raw)
        .bind(source_pre_balance_raw)
        .bind(source_post_balance_raw)
        .bind(destination_pre_balance_raw)
        .bind(destination_post_balance_raw)
        .bind(&input.source_commitment)
        .bind(&input.raw_evidence)
        .bind(&input.decoded_evidence)
        .bind(input.received_at)
        .bind(input.decoded_at)
        .bind(&input.dedupe_key)
        .fetch_one(&self.pool)
        .await?;

        balance_sweep_execution_from_row(&row)
    }

    pub async fn current_positions(
        &self,
        vault_id: VaultId,
    ) -> Result<Vec<CurrentReservePosition>, OrchestratorError> {
        let rows = sqlx::query_as!(
            CurrentPositionRow,
            r#"
            SELECT
                vault_id,
                reserve,
                market,
                liquidity_mint,
                amount_raw,
                supply_apy_bps,
                borrow_apy_bps,
                has_value,
                snapshot_id,
                observed_slot,
                observed_at,
                planning_metadata
            FROM loyal_yield.vault_reserve_positions_current
            WHERE vault_id = $1
            "#,
            vault_id.as_i64()
        )
        .fetch_all(&self.pool)
        .await?;

        rows.into_iter().map(current_position_from_row).collect()
    }

    pub async fn reconcile_vault(
        &self,
        vault_id: VaultId,
        state: ReconciledVaultState,
    ) -> Result<PositionSnapshot, OrchestratorError> {
        if state.positions.is_empty() {
            return Err(OrchestratorError::EmptySnapshot);
        }

        let mut tx = self.pool.begin().await?;
        let vault = fetch_managed_vault_for_update(&mut *tx, vault_id).await?;

        sqlx::query!(
            r#"
            UPDATE loyal_yield.vault_position_snapshots
            SET is_current = FALSE
            WHERE vault_id = $1 AND is_current
            "#,
            vault_id.as_i64()
        )
        .execute(&mut *tx)
        .await?;

        let snapshot_row = sqlx::query_as!(
            SnapshotRow,
            r#"
            INSERT INTO loyal_yield.vault_position_snapshots
                (vault_id, policy_id, observed_slot, observed_at, chain_slot, lock_attempt_id, context)
            VALUES ($1, $2, $3, COALESCE($4, now()), $5, $6, $7)
            RETURNING
                id,
                vault_id,
                policy_id,
                observed_slot,
                observed_at,
                chain_slot,
                lock_attempt_id,
                is_current,
                context
            "#,
            vault_id.as_i64(),
            vault.active_policy_id.as_i64(),
            state.observed_slot,
            state.observed_at,
            state.chain_slot,
            state.lock_attempt_id,
            state.context
        )
        .fetch_one(&mut *tx)
        .await?;

        let mut observed_reserves = Vec::with_capacity(state.positions.len());
        for position in state.positions {
            let amount = to_i64_amount(position.amount_raw)?;
            let reserve = position.reserve;
            let market = position.market;
            let liquidity_mint = position.liquidity_mint;
            let supply_apy_bps = position.supply_apy_bps;
            let borrow_apy_bps = position.borrow_apy_bps;
            let planning_metadata = position.planning_metadata;
            observed_reserves.push(reserve.clone());

            sqlx::query!(
                r#"
                INSERT INTO loyal_yield.vault_position_snapshot_positions
                    (snapshot_id, reserve, market, liquidity_mint, amount_raw, supply_apy_bps, borrow_apy_bps, has_value, planning_metadata)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
                "#,
                snapshot_row.id,
                reserve,
                market,
                liquidity_mint,
                amount,
                supply_apy_bps,
                borrow_apy_bps,
                amount > 0,
                planning_metadata
            )
            .execute(&mut *tx)
            .await?;

            sqlx::query!(
                r#"
                INSERT INTO loyal_yield.vault_reserve_positions_current
                    (vault_id, reserve, market, liquidity_mint, amount_raw, has_value, supply_apy_bps, borrow_apy_bps,
                     snapshot_id, observed_slot, observed_at, planning_metadata)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
                ON CONFLICT (vault_id, reserve) DO UPDATE SET
                    amount_raw = EXCLUDED.amount_raw,
                    has_value = EXCLUDED.has_value,
                    supply_apy_bps = EXCLUDED.supply_apy_bps,
                    borrow_apy_bps = EXCLUDED.borrow_apy_bps,
                    snapshot_id = EXCLUDED.snapshot_id,
                    observed_slot = EXCLUDED.observed_slot,
                    observed_at = EXCLUDED.observed_at,
                    market = EXCLUDED.market,
                    liquidity_mint = EXCLUDED.liquidity_mint,
                    planning_metadata = EXCLUDED.planning_metadata
                "#,
                vault_id.as_i64(),
                reserve,
                market,
                liquidity_mint,
                amount,
                amount > 0,
                supply_apy_bps,
                borrow_apy_bps,
                snapshot_row.id,
                snapshot_row.observed_slot,
                snapshot_row.observed_at,
                planning_metadata
            )
            .execute(&mut *tx)
            .await?;
        }

        sqlx::query!(
            r#"
            DELETE FROM loyal_yield.vault_reserve_positions_current
            WHERE vault_id = $1 AND NOT (reserve = ANY($2))
            "#,
            vault_id.as_i64(),
            &observed_reserves
        )
        .execute(&mut *tx)
        .await?;

        tx.commit().await?;

        Ok(PositionSnapshot {
            id: SnapshotId(snapshot_row.id),
            vault_id: VaultId(snapshot_row.vault_id),
            policy_id: PolicyId(snapshot_row.policy_id),
            observed_slot: snapshot_row.observed_slot,
            observed_at: snapshot_row.observed_at,
            chain_slot: snapshot_row.chain_slot,
            lock_attempt_id: snapshot_row.lock_attempt_id,
            is_current: snapshot_row.is_current,
            context: snapshot_row.context,
        })
    }

    pub async fn plan_same_mint_rebalance(
        &self,
        vault_id: VaultId,
        reserve_scores: Vec<ReserveScore>,
        config: PlannerConfig,
    ) -> Result<PlanOutcome, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let _ = fetch_managed_vault_for_update(&mut *tx, vault_id).await?;

        if active_decision_exists(&mut *tx, vault_id).await? {
            let decision =
                insert_skipped_decision(&mut *tx, vault_id, SkipReason::ActiveDecision).await?;
            tx.commit().await?;
            return Ok(PlanOutcome::skipped(
                vault_id,
                SkipReason::ActiveDecision,
                Some(from_row_to_decision(decision)?),
            ));
        }

        let positions = current_positions_for_update(&mut *tx, vault_id).await?;
        let planned = match draft_same_mint_decision(&positions, &reserve_scores, config) {
            Ok(value) => value,
            Err(reason) => {
                let decision = insert_skipped_decision(&mut *tx, vault_id, reason).await?;
                tx.commit().await?;
                return Ok(PlanOutcome::skipped(
                    vault_id,
                    reason,
                    Some(from_row_to_decision(decision)?),
                ));
            }
        };

        let row =
            insert_planned_decision(&mut *tx, vault_id, &planned, config.estimated_cost_lamports)
                .await?;
        let decision = from_row_to_decision(row)?;
        tx.commit().await?;
        Ok(PlanOutcome::planned(vault_id, decision))
    }

    pub async fn record_planned_rebalance_decision(
        &self,
        vault_id: VaultId,
        input: PlannedRebalanceDecisionInput,
    ) -> Result<PlanOutcome, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let _ = fetch_managed_vault_for_update(&mut *tx, vault_id).await?;

        if active_decision_exists(&mut *tx, vault_id).await? {
            let decision =
                insert_skipped_decision(&mut *tx, vault_id, SkipReason::ActiveDecision).await?;
            tx.commit().await?;
            return Ok(PlanOutcome::skipped(
                vault_id,
                SkipReason::ActiveDecision,
                Some(from_row_to_decision(decision)?),
            ));
        }

        let liquidity_mint = if input.source_liquidity_mint == input.target_liquidity_mint {
            Some(input.source_liquidity_mint.clone())
        } else {
            None
        };
        let planned = PlannedDecision {
            source_snapshot_id: input.source_snapshot_id,
            source_reserve: input.source_reserve,
            target_reserve: input.target_reserve,
            liquidity_mint,
            source_liquidity_mint: input.source_liquidity_mint,
            target_liquidity_mint: input.target_liquidity_mint,
            amount_raw: input.amount_raw,
            source_apy_bps: input.source_apy_bps,
            target_apy_bps: input.target_apy_bps,
            estimated_edge_bps: input.estimated_edge_bps,
            execution_plan: input.execution_plan,
        };

        let row =
            insert_planned_decision(&mut *tx, vault_id, &planned, input.estimated_cost_lamports)
                .await?;
        let decision = from_row_to_decision(row)?;
        tx.commit().await?;
        Ok(PlanOutcome::planned(vault_id, decision))
    }

    pub async fn advance_decision(
        &self,
        decision_id: DecisionId,
        advance: DecisionAdvance,
    ) -> Result<RebalanceDecision, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let decision = fetch_decision_for_update(&mut *tx, decision_id).await?;
        ensure_terminal_repeat_matches(&decision, &advance)?;
        let transition = state_transition(decision.status, advance)?;
        if transition.idempotent {
            tx.commit().await?;
            return Ok(decision);
        }

        let status = transition.status.as_str();
        let post_snapshot_id = transition.post_snapshot_id.map(SnapshotId::as_i64);
        let row = sqlx::query_as!(
            DecisionRow,
            r#"
            UPDATE loyal_yield.rebalance_decisions
            SET
                status = $2::text::loyal_yield.decision_status,
                signature = COALESCE($3, signature),
                submitted_slot = COALESCE($4, submitted_slot),
                confirmed_slot = COALESCE($5, confirmed_slot),
                preflight_chain_slot = COALESCE($6, preflight_chain_slot),
                post_snapshot_id = COALESCE($7, post_snapshot_id),
                abandon_reason = COALESCE($8, abandon_reason),
                updated_at = now()
            WHERE id = $1
            RETURNING
                id,
                vault_id,
                source_snapshot_id,
                status::text AS "status!",
                source_reserve,
                target_reserve,
                liquidity_mint,
                source_liquidity_mint,
                target_liquidity_mint,
                amount_raw,
                source_apy_bps,
                target_apy_bps,
                estimated_edge_bps,
                estimated_cost_lamports,
                decision_reason::text AS "decision_reason!",
                execution_plan,
                abandon_reason,
                signature,
                submitted_slot,
                confirmed_slot,
                preflight_chain_slot,
                post_snapshot_id,
                created_at,
                updated_at
            "#,
            decision_id.as_i64(),
            status,
            transition.signature,
            transition.submitted_slot,
            transition.confirmed_slot,
            transition.preflight_chain_slot,
            post_snapshot_id,
            transition.abandon_reason
        )
        .fetch_one(&mut *tx)
        .await?;

        let decision = from_row_to_decision(row)?;
        tx.commit().await?;
        Ok(decision)
    }
}

async fn upsert_policy(
    conn: &mut PgConnection,
    event: &PolicyMatchInput,
) -> Result<RoutePolicy, OrchestratorError> {
    let slot =
        i64::try_from(event.slot).map_err(|_| OrchestratorError::SlotOutOfRange(event.slot))?;
    let policy_seed = i64::try_from(event.policy_seed)
        .map_err(|_| OrchestratorError::PolicySeedOutOfRange(event.policy_seed))?;
    let row = sqlx::query_as!(
        RoutePolicyRow,
        r#"
        INSERT INTO loyal_yield.route_policies
            (cluster, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
             delegated_signers, threshold, route_modes, stable_mints, kamino_markets, kamino_liquidity_mints,
             universe_preset, risk_profile, swap_lanes, active, last_seen_slot, last_seen_signature)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, TRUE, $17, $18)
        ON CONFLICT (cluster, policy_account) DO UPDATE SET
            settings = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.settings ELSE loyal_yield.route_policies.settings END,
            authority = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.authority ELSE loyal_yield.route_policies.authority END,
            policy_seed = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.policy_seed ELSE loyal_yield.route_policies.policy_seed END,
            vault_index = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.vault_index ELSE loyal_yield.route_policies.vault_index END,
            vault_pubkey = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.vault_pubkey ELSE loyal_yield.route_policies.vault_pubkey END,
            delegated_signers = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.delegated_signers ELSE loyal_yield.route_policies.delegated_signers END,
            threshold = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.threshold ELSE loyal_yield.route_policies.threshold END,
            route_modes = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.route_modes ELSE loyal_yield.route_policies.route_modes END,
            stable_mints = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.stable_mints ELSE loyal_yield.route_policies.stable_mints END,
            kamino_markets = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.kamino_markets ELSE loyal_yield.route_policies.kamino_markets END,
            kamino_liquidity_mints = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.kamino_liquidity_mints ELSE loyal_yield.route_policies.kamino_liquidity_mints END,
            universe_preset = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.universe_preset ELSE loyal_yield.route_policies.universe_preset END,
            risk_profile = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.risk_profile ELSE loyal_yield.route_policies.risk_profile END,
            swap_lanes = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.swap_lanes ELSE loyal_yield.route_policies.swap_lanes END,
            active = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN TRUE ELSE loyal_yield.route_policies.active END,
            last_seen_at = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN now() ELSE loyal_yield.route_policies.last_seen_at END,
            last_seen_slot = GREATEST(loyal_yield.route_policies.last_seen_slot, EXCLUDED.last_seen_slot),
            last_seen_signature = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.last_seen_signature ELSE loyal_yield.route_policies.last_seen_signature END
        RETURNING
            id,
            cluster,
            settings,
            authority,
            policy_seed,
            policy_account,
            vault_index,
            vault_pubkey,
            delegated_signers AS "delegated_signers!",
            threshold,
            route_modes AS "route_modes!",
            stable_mints AS "stable_mints!",
            kamino_markets AS "kamino_markets!",
            kamino_liquidity_mints AS "kamino_liquidity_mints!",
            universe_preset,
            risk_profile,
            swap_lanes,
            active,
            first_seen_at,
            last_seen_at,
            last_seen_slot,
            last_seen_signature
        "#,
        &event.cluster,
        &event.settings,
        &event.authority,
        policy_seed,
        &event.policy_account,
        i16::from(event.vault_index),
        &event.vault_pubkey,
        &event.delegated_signers,
        i32::from(event.threshold),
        &event.route_modes,
        &event.stable_mints,
        &event.kamino_markets,
        &event.kamino_liquidity_mints,
        event.universe_preset.as_deref(),
        event.risk_profile.as_deref(),
        &event.swap_lanes,
        slot,
        &event.signature
    )
    .fetch_one(conn)
    .await?;

    Ok(route_policy_from_row(row))
}

async fn upsert_vault(
    conn: &mut PgConnection,
    policy_id: PolicyId,
    event: &PolicyMatchInput,
) -> Result<ManagedVault, OrchestratorError> {
    let row = sqlx::query_as!(
        ManagedVaultRow,
        r#"
        INSERT INTO loyal_yield.managed_vaults
            (cluster, settings, vault_index, vault_pubkey, active_policy_id, active)
        VALUES ($1, $2, $3, $4, $5, TRUE)
        ON CONFLICT (cluster, settings, vault_index, vault_pubkey) DO UPDATE SET
            active_policy_id = CASE
                WHEN (
                    SELECT last_seen_slot
                    FROM loyal_yield.route_policies
                    WHERE id = EXCLUDED.active_policy_id
                ) > (
                    SELECT last_seen_slot
                    FROM loyal_yield.route_policies
                    WHERE id = loyal_yield.managed_vaults.active_policy_id
                )
                THEN EXCLUDED.active_policy_id
                ELSE loyal_yield.managed_vaults.active_policy_id
            END,
            active = CASE
                WHEN (
                    SELECT last_seen_slot
                    FROM loyal_yield.route_policies
                    WHERE id = EXCLUDED.active_policy_id
                ) > (
                    SELECT last_seen_slot
                    FROM loyal_yield.route_policies
                    WHERE id = loyal_yield.managed_vaults.active_policy_id
                )
                THEN TRUE
                ELSE loyal_yield.managed_vaults.active
            END,
            last_seen_at = CASE
                WHEN (
                    SELECT last_seen_slot
                    FROM loyal_yield.route_policies
                    WHERE id = EXCLUDED.active_policy_id
                ) > (
                    SELECT last_seen_slot
                    FROM loyal_yield.route_policies
                    WHERE id = loyal_yield.managed_vaults.active_policy_id
                )
                THEN now()
                ELSE loyal_yield.managed_vaults.last_seen_at
            END
        RETURNING id, cluster, settings, vault_index, vault_pubkey, active_policy_id, active, first_seen_at, last_seen_at
        "#,
        &event.cluster,
        &event.settings,
        i16::from(event.vault_index),
        &event.vault_pubkey,
        policy_id.as_i64()
    )
    .fetch_one(conn)
    .await?;

    Ok(managed_vault_from_row(row))
}

async fn fetch_managed_vault_for_update(
    conn: &mut PgConnection,
    vault_id: VaultId,
) -> Result<ManagedVault, OrchestratorError> {
    let row = sqlx::query_as!(
        ManagedVaultRow,
        r#"
        SELECT id, cluster, settings, vault_index, vault_pubkey, active_policy_id, active, first_seen_at, last_seen_at
        FROM loyal_yield.managed_vaults
        WHERE id = $1
        FOR UPDATE
        "#,
        vault_id.as_i64()
    )
    .fetch_one(conn)
    .await?;

    Ok(managed_vault_from_row(row))
}

async fn active_decision_exists(
    conn: &mut PgConnection,
    vault_id: VaultId,
) -> Result<bool, OrchestratorError> {
    let active_statuses = ACTIVE_DECISION_STATUSES
        .iter()
        .map(|status| (*status).to_owned())
        .collect::<Vec<_>>();

    sqlx::query_scalar!(
        r#"
        SELECT EXISTS(
            SELECT 1
            FROM loyal_yield.rebalance_decisions
            WHERE vault_id = $1 AND status::text = ANY($2)
        ) AS "exists!"
        "#,
        vault_id.as_i64(),
        &active_statuses
    )
    .fetch_one(conn)
    .await
    .map_err(OrchestratorError::from)
}

async fn current_positions_for_update(
    conn: &mut PgConnection,
    vault_id: VaultId,
) -> Result<Vec<CurrentReservePosition>, OrchestratorError> {
    let rows = sqlx::query_as!(
        CurrentPositionRow,
        r#"
        SELECT
            vault_id,
            reserve,
            market,
            liquidity_mint,
            amount_raw,
            supply_apy_bps,
            borrow_apy_bps,
            has_value,
            snapshot_id,
            observed_slot,
            observed_at,
            planning_metadata
        FROM loyal_yield.vault_reserve_positions_current
        WHERE vault_id = $1
        FOR UPDATE
        "#,
        vault_id.as_i64()
    )
    .fetch_all(conn)
    .await?;

    rows.into_iter().map(current_position_from_row).collect()
}

async fn insert_planned_decision(
    conn: &mut PgConnection,
    vault_id: VaultId,
    planned: &PlannedDecision,
    estimated_cost_lamports: i64,
) -> Result<DecisionRow, OrchestratorError> {
    let idempotency_key = rebalance_idempotency_key(vault_id, planned.source_snapshot_id, planned);
    sqlx::query_as!(
        DecisionRow,
        r#"
        INSERT INTO loyal_yield.rebalance_decisions
            (vault_id, source_snapshot_id, status, source_reserve, target_reserve, liquidity_mint,
             source_liquidity_mint, target_liquidity_mint, amount_raw, source_apy_bps,
             target_apy_bps, estimated_edge_bps, estimated_cost_lamports, decision_reason,
             execution_plan, idempotency_key)
        VALUES ($1, $2, 'planned'::loyal_yield.decision_status, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::text::loyal_yield.decision_reason, $14, $15)
        ON CONFLICT (idempotency_key) DO UPDATE SET updated_at = now()
        RETURNING
            id,
            vault_id,
            source_snapshot_id,
            status::text AS "status!",
            source_reserve,
            target_reserve,
            liquidity_mint,
            source_liquidity_mint,
            target_liquidity_mint,
            amount_raw,
            source_apy_bps,
            target_apy_bps,
            estimated_edge_bps,
            estimated_cost_lamports,
            decision_reason::text AS "decision_reason!",
            execution_plan,
            abandon_reason,
            signature,
            submitted_slot,
            confirmed_slot,
            preflight_chain_slot,
            post_snapshot_id,
            created_at,
            updated_at
        "#,
        vault_id.as_i64(),
        planned.source_snapshot_id.as_i64(),
        &planned.source_reserve,
        &planned.target_reserve,
        planned.liquidity_mint.as_deref(),
        &planned.source_liquidity_mint,
        &planned.target_liquidity_mint,
        planned.amount_raw,
        planned.source_apy_bps,
        planned.target_apy_bps,
        planned.estimated_edge_bps,
        estimated_cost_lamports,
        DecisionReason::TargetSupplyApyExceedsSource.as_str(),
        &planned.execution_plan,
        idempotency_key
    )
    .fetch_one(conn)
    .await
    .map_err(OrchestratorError::from)
}

async fn insert_skipped_decision(
    conn: &mut PgConnection,
    vault_id: VaultId,
    reason: SkipReason,
) -> Result<DecisionRow, OrchestratorError> {
    let idempotency_key = skipped_idempotency_key(vault_id, reason);
    sqlx::query_as!(
        DecisionRow,
        r#"
        INSERT INTO loyal_yield.rebalance_decisions
            (vault_id, status, estimated_cost_lamports, decision_reason, idempotency_key)
        VALUES ($1, 'skipped'::loyal_yield.decision_status, 0, $2::text::loyal_yield.decision_reason, $3)
        ON CONFLICT (idempotency_key) DO UPDATE SET updated_at = now()
        RETURNING
            id,
            vault_id,
            source_snapshot_id,
            status::text AS "status!",
            source_reserve,
            target_reserve,
            liquidity_mint,
            source_liquidity_mint,
            target_liquidity_mint,
            amount_raw,
            source_apy_bps,
            target_apy_bps,
            estimated_edge_bps,
            estimated_cost_lamports,
            decision_reason::text AS "decision_reason!",
            execution_plan,
            abandon_reason,
            signature,
            submitted_slot,
            confirmed_slot,
            preflight_chain_slot,
            post_snapshot_id,
            created_at,
            updated_at
        "#,
        vault_id.as_i64(),
        reason.decision_reason().as_str(),
        idempotency_key
    )
    .fetch_one(conn)
    .await
    .map_err(OrchestratorError::from)
}

async fn fetch_decision_for_update(
    conn: &mut PgConnection,
    decision_id: DecisionId,
) -> Result<RebalanceDecision, OrchestratorError> {
    let row = sqlx::query_as!(
        DecisionRow,
        r#"
        SELECT
            id,
            vault_id,
            source_snapshot_id,
            status::text AS "status!",
            source_reserve,
            target_reserve,
            liquidity_mint,
            source_liquidity_mint,
            target_liquidity_mint,
            amount_raw,
            source_apy_bps,
            target_apy_bps,
            estimated_edge_bps,
            estimated_cost_lamports,
            decision_reason::text AS "decision_reason!",
            execution_plan,
            abandon_reason,
            signature,
            submitted_slot,
            confirmed_slot,
            preflight_chain_slot,
            post_snapshot_id,
            created_at,
            updated_at
        FROM loyal_yield.rebalance_decisions
        WHERE id = $1
        FOR UPDATE
        "#,
        decision_id.as_i64()
    )
    .fetch_one(conn)
    .await?;
    from_row_to_decision(row)
}

fn route_policy_from_row(row: RoutePolicyRow) -> RoutePolicy {
    RoutePolicy {
        id: PolicyId(row.id),
        cluster: row.cluster,
        settings: row.settings,
        authority: row.authority,
        policy_seed: row.policy_seed,
        policy_account: row.policy_account,
        vault_index: row.vault_index,
        vault_pubkey: row.vault_pubkey,
        delegated_signers: row.delegated_signers,
        threshold: row.threshold,
        route_modes: row.route_modes,
        stable_mints: row.stable_mints,
        kamino_markets: row.kamino_markets,
        kamino_liquidity_mints: row.kamino_liquidity_mints,
        universe_preset: row.universe_preset,
        risk_profile: row.risk_profile,
        swap_lanes: row.swap_lanes,
        active: row.active,
        first_seen_at: row.first_seen_at,
        last_seen_at: row.last_seen_at,
        last_seen_slot: row.last_seen_slot,
        last_seen_signature: row.last_seen_signature,
    }
}

fn managed_vault_from_row(row: ManagedVaultRow) -> ManagedVault {
    ManagedVault {
        id: VaultId(row.id),
        cluster: row.cluster,
        settings: row.settings,
        vault_index: row.vault_index,
        vault_pubkey: row.vault_pubkey,
        active_policy_id: PolicyId(row.active_policy_id),
        active: row.active,
        first_seen_at: row.first_seen_at,
        last_seen_at: row.last_seen_at,
    }
}

fn balance_sweep_target_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<BalanceSweepTarget, OrchestratorError> {
    Ok(BalanceSweepTarget {
        id: BalanceSweepTargetId(row.try_get("id")?),
        cluster: row.try_get("cluster")?,
        settings: row.try_get("settings")?,
        authority: row.try_get("authority")?,
        policy_seed: row.try_get("policy_seed")?,
        policy_account: row.try_get("policy_account")?,
        vault_index: row.try_get("vault_index")?,
        vault_pubkey: row.try_get("vault_pubkey")?,
        wallet: row.try_get("wallet")?,
        wallet_usdc_ata: row.try_get("wallet_usdc_ata")?,
        vault_usdc_ata: row.try_get("vault_usdc_ata")?,
        delegated_signers: row.try_get("delegated_signers")?,
        threshold: row.try_get("threshold")?,
        max_amount_per_period: row.try_get("max_amount_per_period")?,
        active: row.try_get("active")?,
        first_seen_at: row.try_get("first_seen_at")?,
        last_seen_at: row.try_get("last_seen_at")?,
        last_seen_slot: row.try_get("last_seen_slot")?,
        last_seen_signature: row.try_get("last_seen_signature")?,
    })
}

fn wallet_ata_balance_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<WalletAtaBalanceCurrent, OrchestratorError> {
    Ok(WalletAtaBalanceCurrent {
        target_id: BalanceSweepTargetId(row.try_get("target_id")?),
        cluster: row.try_get("cluster")?,
        wallet: row.try_get("wallet")?,
        wallet_usdc_ata: row.try_get("wallet_usdc_ata")?,
        amount_raw: row.try_get("amount_raw")?,
        owner: row.try_get("owner")?,
        mint: row.try_get("mint")?,
        observed_slot: row.try_get("observed_slot")?,
        observed_at: row.try_get("observed_at")?,
        source: row.try_get("source")?,
        source_commitment: row.try_get("source_commitment")?,
        account_data_hash: row.try_get("account_data_hash")?,
        raw_evidence: row.try_get("raw_evidence")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn balance_sweep_execution_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<BalanceSweepExecution, OrchestratorError> {
    Ok(BalanceSweepExecution {
        id: row.try_get("id")?,
        target_id: BalanceSweepTargetId(row.try_get("target_id")?),
        cluster: row.try_get("cluster")?,
        signature: row.try_get("signature")?,
        slot: row.try_get("slot")?,
        source_wallet_ata: row.try_get("source_wallet_ata")?,
        destination_vault_ata: row.try_get("destination_vault_ata")?,
        amount_raw: row.try_get("amount_raw")?,
        source_pre_balance_raw: row.try_get("source_pre_balance_raw")?,
        source_post_balance_raw: row.try_get("source_post_balance_raw")?,
        destination_pre_balance_raw: row.try_get("destination_pre_balance_raw")?,
        destination_post_balance_raw: row.try_get("destination_post_balance_raw")?,
        source_commitment: row.try_get("source_commitment")?,
        raw_evidence: row.try_get("raw_evidence")?,
        decoded_evidence: row.try_get("decoded_evidence")?,
        received_at: row.try_get("received_at")?,
        decoded_at: row.try_get("decoded_at")?,
        inserted_at: row.try_get("inserted_at")?,
        dedupe_key: row.try_get("dedupe_key")?,
    })
}

fn current_position_from_row(
    row: CurrentPositionRow,
) -> Result<CurrentReservePosition, OrchestratorError> {
    Ok(CurrentReservePosition {
        vault_id: VaultId(row.vault_id),
        reserve: row.reserve,
        market: row.market,
        liquidity_mint: row.liquidity_mint,
        amount_raw: row.amount_raw,
        has_value: row.has_value,
        supply_apy_bps: row.supply_apy_bps,
        borrow_apy_bps: row.borrow_apy_bps,
        snapshot_id: SnapshotId(row.snapshot_id),
        observed_slot: row.observed_slot,
        observed_at: row.observed_at,
        planning_metadata: row.planning_metadata,
    })
}

fn ensure_terminal_repeat_matches(
    decision: &RebalanceDecision,
    advance: &DecisionAdvance,
) -> Result<(), OrchestratorError> {
    match (decision.status, advance) {
        (
            DecisionStatus::Confirmed,
            DecisionAdvance::Confirm {
                slot,
                post_snapshot_id,
            },
        ) => {
            if slot.is_some() && *slot != decision.confirmed_slot {
                return Err(OrchestratorError::ConflictingTerminalRepeat {
                    field: "confirmed_slot",
                });
            }
            if post_snapshot_id.is_some() && *post_snapshot_id != decision.post_snapshot_id {
                return Err(OrchestratorError::ConflictingTerminalRepeat {
                    field: "post_snapshot_id",
                });
            }
        }
        (DecisionStatus::Failed, DecisionAdvance::Fail { reason })
        | (DecisionStatus::Abandoned, DecisionAdvance::Abandon { reason }) => {
            if decision
                .abandon_reason
                .as_deref()
                .is_some_and(|stored| stored != reason)
            {
                return Err(OrchestratorError::ConflictingTerminalRepeat {
                    field: "abandon_reason",
                });
            }
        }
        _ => {}
    }
    Ok(())
}

fn from_row_to_decision(row: DecisionRow) -> Result<RebalanceDecision, OrchestratorError> {
    let status = DecisionStatus::parse(&row.status)
        .ok_or_else(|| OrchestratorError::UnknownDecisionStatus(row.status))?;
    let decision_reason = DecisionReason::parse(&row.decision_reason)
        .ok_or_else(|| OrchestratorError::StoreInvariant("unknown decision_reason".to_owned()))?;

    Ok(RebalanceDecision {
        id: DecisionId(row.id),
        vault_id: VaultId(row.vault_id),
        source_snapshot_id: row.source_snapshot_id.map(SnapshotId),
        status,
        source_reserve: row.source_reserve,
        target_reserve: row.target_reserve,
        liquidity_mint: row.liquidity_mint,
        source_liquidity_mint: row.source_liquidity_mint,
        target_liquidity_mint: row.target_liquidity_mint,
        amount_raw: row.amount_raw,
        source_apy_bps: row.source_apy_bps,
        target_apy_bps: row.target_apy_bps,
        estimated_edge_bps: row.estimated_edge_bps,
        estimated_cost_lamports: row.estimated_cost_lamports,
        decision_reason,
        execution_plan: row.execution_plan,
        abandon_reason: row.abandon_reason,
        signature: row.signature,
        submitted_slot: row.submitted_slot,
        confirmed_slot: row.confirmed_slot,
        preflight_chain_slot: row.preflight_chain_slot,
        post_snapshot_id: row.post_snapshot_id.map(SnapshotId),
        created_at: row.created_at,
        updated_at: row.updated_at,
    })
}

fn rebalance_idempotency_key(
    vault_id: VaultId,
    snapshot_id: SnapshotId,
    planned: &PlannedDecision,
) -> String {
    let mut hasher = Sha256::new();
    hasher.update(vault_id.as_i64().to_le_bytes());
    hasher.update(snapshot_id.as_i64().to_le_bytes());
    hasher.update(planned.source_reserve.as_bytes());
    hasher.update(planned.target_reserve.as_bytes());
    if let Some(liquidity_mint) = &planned.liquidity_mint {
        hasher.update(b"liquidity_mint");
        hasher.update(liquidity_mint.as_bytes());
    }
    hasher.update(planned.source_liquidity_mint.as_bytes());
    hasher.update(planned.target_liquidity_mint.as_bytes());
    hasher.update(planned.amount_raw.to_le_bytes());
    hasher.update(planned.execution_plan.to_string().as_bytes());
    format!("{:x}", hasher.finalize())
}

fn skipped_idempotency_key(vault_id: VaultId, reason: SkipReason) -> String {
    let mut hasher = Sha256::new();
    hasher.update(b"skipped");
    hasher.update(vault_id.as_i64().to_le_bytes());
    hasher.update(reason.decision_reason().as_str().as_bytes());
    format!("{:x}", hasher.finalize())
}

fn to_i64_amount(amount: u64) -> Result<i64, OrchestratorError> {
    i64::try_from(amount).map_err(|_| OrchestratorError::amount_out_of_range(amount))
}

fn to_i64_slot(slot: u64) -> Result<i64, OrchestratorError> {
    i64::try_from(slot).map_err(|_| OrchestratorError::SlotOutOfRange(slot))
}

fn to_i64_policy_seed(policy_seed: u64) -> Result<i64, OrchestratorError> {
    i64::try_from(policy_seed).map_err(|_| OrchestratorError::PolicySeedOutOfRange(policy_seed))
}

fn optional_to_i64_amount(amount: Option<u64>) -> Result<Option<i64>, OrchestratorError> {
    amount.map(to_i64_amount).transpose()
}
