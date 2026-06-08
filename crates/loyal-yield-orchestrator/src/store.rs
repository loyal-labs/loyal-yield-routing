use crate::domain::{draft_same_mint_decision, state_transition, PlannedDecision};
use crate::types::*;
use crate::{OrchestratorError, ACTIVE_DECISION_STATUSES};
use chrono::{DateTime, Utc};
use serde_json::Value;
use sha2::{Digest, Sha256};
use sqlx::postgres::{PgPoolOptions, PgRow};
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

    pub async fn active_vault_route_policies(
        &self,
        cluster: Option<&str>,
        limit: i64,
    ) -> Result<Vec<ManagedVaultRoutePolicy>, OrchestratorError> {
        let rows = sqlx::query(
            r#"
            SELECT
                mv.id AS vault_id,
                mv.cluster AS vault_cluster,
                mv.settings AS vault_settings,
                mv.vault_index AS vault_vault_index,
                mv.vault_pubkey AS vault_vault_pubkey,
                mv.active_policy_id AS vault_active_policy_id,
                mv.active AS vault_active,
                mv.first_seen_at AS vault_first_seen_at,
                mv.last_seen_at AS vault_last_seen_at,
                rp.id AS policy_id,
                rp.cluster AS policy_cluster,
                rp.settings AS policy_settings,
                rp.authority AS policy_authority,
                rp.policy_seed AS policy_policy_seed,
                rp.policy_account AS policy_policy_account,
                rp.vault_index AS policy_vault_index,
                rp.vault_pubkey AS policy_vault_pubkey,
                rp.delegated_signers AS policy_delegated_signers,
                rp.threshold AS policy_threshold,
                rp.route_modes AS policy_route_modes,
                rp.stable_mints AS policy_stable_mints,
                rp.kamino_markets AS policy_kamino_markets,
                rp.kamino_liquidity_mints AS policy_kamino_liquidity_mints,
                rp.universe_preset AS policy_universe_preset,
                rp.risk_profile AS policy_risk_profile,
                rp.swap_lanes AS policy_swap_lanes,
                rp.active AS policy_active,
                rp.first_seen_at AS policy_first_seen_at,
                rp.last_seen_at AS policy_last_seen_at,
                rp.last_seen_slot AS policy_last_seen_slot,
                rp.last_seen_signature AS policy_last_seen_signature
            FROM loyal_yield.managed_vaults mv
            JOIN loyal_yield.route_policies rp ON rp.id = mv.active_policy_id
            WHERE mv.active
              AND rp.active
              AND ($1::text IS NULL OR mv.cluster = $1)
            ORDER BY mv.last_seen_at DESC, mv.id ASC
            LIMIT $2
            "#,
        )
        .bind(cluster)
        .bind(limit)
        .fetch_all(&self.pool)
        .await?;

        rows.into_iter()
            .map(managed_vault_route_policy_from_row)
            .collect()
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

    pub async fn claim_same_mint_decisions(
        &self,
        limit: i64,
    ) -> Result<Vec<RebalanceDecision>, OrchestratorError> {
        self.claim_route_decisions(limit, vec!["same_mint".to_owned()])
            .await
    }

    pub async fn claim_yield_route_decisions(
        &self,
        limit: i64,
    ) -> Result<Vec<RebalanceDecision>, OrchestratorError> {
        self.claim_route_decisions(limit, vec!["same_mint".to_owned(), "cross_mint".to_owned()])
            .await
    }

    async fn claim_route_decisions(
        &self,
        limit: i64,
        plan_kinds: Vec<String>,
    ) -> Result<Vec<RebalanceDecision>, OrchestratorError> {
        let rows = sqlx::query(
            r#"
            WITH claimed AS (
                SELECT id
                FROM loyal_yield.rebalance_decisions
                WHERE status = 'planned'::loyal_yield.decision_status
                  AND execution_plan->>'kind' = ANY($2::text[])
                ORDER BY created_at ASC
                LIMIT $1
                FOR UPDATE SKIP LOCKED
            )
            UPDATE loyal_yield.rebalance_decisions decisions
            SET status = 'simulating'::loyal_yield.decision_status,
                updated_at = now()
            FROM claimed
            WHERE decisions.id = claimed.id
            RETURNING
                decisions.id,
                decisions.vault_id,
                decisions.source_snapshot_id,
                decisions.status::text AS status,
                decisions.source_reserve,
                decisions.target_reserve,
                decisions.liquidity_mint,
                decisions.source_liquidity_mint,
                decisions.target_liquidity_mint,
                decisions.amount_raw,
                decisions.source_apy_bps,
                decisions.target_apy_bps,
                decisions.estimated_edge_bps,
                decisions.estimated_cost_lamports,
                decisions.decision_reason::text AS decision_reason,
                decisions.execution_plan,
                decisions.abandon_reason,
                decisions.signature,
                decisions.submitted_slot,
                decisions.confirmed_slot,
                decisions.preflight_chain_slot,
                decisions.post_snapshot_id,
                decisions.created_at,
                decisions.updated_at
            "#,
        )
        .bind(limit)
        .bind(plan_kinds)
        .fetch_all(&self.pool)
        .await?;

        rows.into_iter().map(decision_from_pg_row).collect()
    }

    pub async fn record_rebalance_attempt(
        &self,
        decision_id: DecisionId,
        input: RebalanceAttemptInput,
    ) -> Result<RebalanceAttempt, OrchestratorError> {
        let row = sqlx::query(
            r#"
            WITH next_attempt AS (
                SELECT COALESCE(MAX(attempt_no), 0) + 1 AS attempt_no
                FROM loyal_yield.rebalance_attempts
                WHERE decision_id = $1
            )
            INSERT INTO loyal_yield.rebalance_attempts
                (decision_id, attempt_no, status, worker_id, dry_run, transaction_plan,
                 simulation_result, submit_result, signature, slot, error)
            SELECT
                $1, next_attempt.attempt_no, $2, $3, $4, $5, $6, $7, $8, $9, $10
            FROM next_attempt
            RETURNING
                id,
                decision_id,
                attempt_no,
                status,
                worker_id,
                dry_run,
                transaction_plan,
                simulation_result,
                submit_result,
                signature,
                slot,
                error,
                created_at,
                updated_at
            "#,
        )
        .bind(decision_id.as_i64())
        .bind(input.status)
        .bind(input.worker_id)
        .bind(input.dry_run)
        .bind(input.transaction_plan)
        .bind(input.simulation_result)
        .bind(input.submit_result)
        .bind(input.signature)
        .bind(input.slot)
        .bind(input.error)
        .fetch_one(&self.pool)
        .await?;

        attempt_from_pg_row(row)
    }

    pub async fn update_rebalance_attempt(
        &self,
        attempt_id: i64,
        update: RebalanceAttemptUpdate,
    ) -> Result<RebalanceAttempt, OrchestratorError> {
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.rebalance_attempts
            SET status = $2,
                simulation_result = $3,
                submit_result = $4,
                signature = COALESCE($5, signature),
                slot = COALESCE($6, slot),
                error = $7,
                updated_at = now()
            WHERE id = $1
            RETURNING
                id,
                decision_id,
                attempt_no,
                status,
                worker_id,
                dry_run,
                transaction_plan,
                simulation_result,
                submit_result,
                signature,
                slot,
                error,
                created_at,
                updated_at
            "#,
        )
        .bind(attempt_id)
        .bind(update.status)
        .bind(update.simulation_result)
        .bind(update.submit_result)
        .bind(update.signature)
        .bind(update.slot)
        .bind(update.error)
        .fetch_one(&self.pool)
        .await?;

        attempt_from_pg_row(row)
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

fn managed_vault_route_policy_from_row(
    row: PgRow,
) -> Result<ManagedVaultRoutePolicy, OrchestratorError> {
    Ok(ManagedVaultRoutePolicy {
        vault: ManagedVault {
            id: VaultId(row.try_get("vault_id")?),
            cluster: row.try_get("vault_cluster")?,
            settings: row.try_get("vault_settings")?,
            vault_index: row.try_get("vault_vault_index")?,
            vault_pubkey: row.try_get("vault_vault_pubkey")?,
            active_policy_id: PolicyId(row.try_get("vault_active_policy_id")?),
            active: row.try_get("vault_active")?,
            first_seen_at: row.try_get("vault_first_seen_at")?,
            last_seen_at: row.try_get("vault_last_seen_at")?,
        },
        policy: RoutePolicy {
            id: PolicyId(row.try_get("policy_id")?),
            cluster: row.try_get("policy_cluster")?,
            settings: row.try_get("policy_settings")?,
            authority: row.try_get("policy_authority")?,
            policy_seed: row.try_get("policy_policy_seed")?,
            policy_account: row.try_get("policy_policy_account")?,
            vault_index: row.try_get("policy_vault_index")?,
            vault_pubkey: row.try_get("policy_vault_pubkey")?,
            delegated_signers: row.try_get("policy_delegated_signers")?,
            threshold: row.try_get("policy_threshold")?,
            route_modes: row.try_get("policy_route_modes")?,
            stable_mints: row.try_get("policy_stable_mints")?,
            kamino_markets: row.try_get("policy_kamino_markets")?,
            kamino_liquidity_mints: row.try_get("policy_kamino_liquidity_mints")?,
            universe_preset: row.try_get("policy_universe_preset")?,
            risk_profile: row.try_get("policy_risk_profile")?,
            swap_lanes: row.try_get("policy_swap_lanes")?,
            active: row.try_get("policy_active")?,
            first_seen_at: row.try_get("policy_first_seen_at")?,
            last_seen_at: row.try_get("policy_last_seen_at")?,
            last_seen_slot: row.try_get("policy_last_seen_slot")?,
            last_seen_signature: row.try_get("policy_last_seen_signature")?,
        },
    })
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

fn decision_from_pg_row(row: PgRow) -> Result<RebalanceDecision, OrchestratorError> {
    let status: String = row.try_get("status")?;
    let decision_reason: String = row.try_get("decision_reason")?;
    let status = DecisionStatus::parse(&status)
        .ok_or_else(|| OrchestratorError::UnknownDecisionStatus(status))?;
    let decision_reason = DecisionReason::parse(&decision_reason)
        .ok_or_else(|| OrchestratorError::StoreInvariant("unknown decision_reason".to_owned()))?;

    Ok(RebalanceDecision {
        id: DecisionId(row.try_get("id")?),
        vault_id: VaultId(row.try_get("vault_id")?),
        source_snapshot_id: row
            .try_get::<Option<i64>, _>("source_snapshot_id")?
            .map(SnapshotId),
        status,
        source_reserve: row.try_get("source_reserve")?,
        target_reserve: row.try_get("target_reserve")?,
        liquidity_mint: row.try_get("liquidity_mint")?,
        source_liquidity_mint: row.try_get("source_liquidity_mint")?,
        target_liquidity_mint: row.try_get("target_liquidity_mint")?,
        amount_raw: row.try_get("amount_raw")?,
        source_apy_bps: row.try_get("source_apy_bps")?,
        target_apy_bps: row.try_get("target_apy_bps")?,
        estimated_edge_bps: row.try_get("estimated_edge_bps")?,
        estimated_cost_lamports: row.try_get("estimated_cost_lamports")?,
        decision_reason,
        execution_plan: row.try_get("execution_plan")?,
        abandon_reason: row.try_get("abandon_reason")?,
        signature: row.try_get("signature")?,
        submitted_slot: row.try_get("submitted_slot")?,
        confirmed_slot: row.try_get("confirmed_slot")?,
        preflight_chain_slot: row.try_get("preflight_chain_slot")?,
        post_snapshot_id: row
            .try_get::<Option<i64>, _>("post_snapshot_id")?
            .map(SnapshotId),
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn attempt_from_pg_row(row: PgRow) -> Result<RebalanceAttempt, OrchestratorError> {
    Ok(RebalanceAttempt {
        id: row.try_get("id")?,
        decision_id: DecisionId(row.try_get("decision_id")?),
        attempt_no: row.try_get("attempt_no")?,
        status: row.try_get("status")?,
        worker_id: row.try_get("worker_id")?,
        dry_run: row.try_get("dry_run")?,
        transaction_plan: row.try_get("transaction_plan")?,
        simulation_result: row.try_get("simulation_result")?,
        submit_result: row.try_get("submit_result")?,
        signature: row.try_get("signature")?,
        slot: row.try_get("slot")?,
        error: row.try_get("error")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
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

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::time::{SystemTime, UNIX_EPOCH};

    async fn database_store() -> Option<OrchestratorStore> {
        let url = match std::env::var("DATABASE_URL") {
            Ok(url) => url,
            Err(_) => {
                eprintln!("skipping database test because DATABASE_URL is not set");
                return None;
            }
        };
        let store = OrchestratorStore::connect(
            NeonSqlConfig::new(url)
                .with_max_connections(1)
                .with_acquire_timeout(std::time::Duration::from_secs(10)),
        )
        .await
        .expect("connect to test database");
        store.apply_migrations().await.expect("apply migrations");
        Some(store)
    }

    fn unique_cluster(test_name: &str) -> String {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("system clock after unix epoch")
            .as_nanos();
        format!("test_{test_name}_{nanos}")
    }

    async fn delete_cluster(store: &OrchestratorStore, cluster: &str) {
        sqlx::query(
            r#"
            DELETE FROM loyal_yield.rebalance_decisions
            WHERE vault_id IN (
                SELECT id FROM loyal_yield.managed_vaults WHERE cluster = $1
            )
            "#,
        )
        .bind(cluster)
        .execute(store.pool())
        .await
        .expect("delete test decisions");
        sqlx::query(
            r#"
            DELETE FROM loyal_yield.vault_reserve_positions_current
            WHERE vault_id IN (
                SELECT id FROM loyal_yield.managed_vaults WHERE cluster = $1
            )
            "#,
        )
        .bind(cluster)
        .execute(store.pool())
        .await
        .expect("delete test current positions");
        sqlx::query(
            r#"
            DELETE FROM loyal_yield.vault_position_snapshots
            WHERE vault_id IN (
                SELECT id FROM loyal_yield.managed_vaults WHERE cluster = $1
            )
            "#,
        )
        .bind(cluster)
        .execute(store.pool())
        .await
        .expect("delete test snapshots");
        sqlx::query("DELETE FROM loyal_yield.managed_vaults WHERE cluster = $1")
            .bind(cluster)
            .execute(store.pool())
            .await
            .expect("delete test vaults");
        sqlx::query("DELETE FROM loyal_yield.route_policies WHERE cluster = $1")
            .bind(cluster)
            .execute(store.pool())
            .await
            .expect("delete test policies");
    }

    fn policy_match(cluster: &str, policy_account: &str, slot: u64) -> PolicyMatchInput {
        PolicyMatchInput {
            signature: format!("sig-{policy_account}-{slot}"),
            slot,
            cluster: cluster.to_owned(),
            settings: "settings-1".to_owned(),
            authority: "authority-1".to_owned(),
            policy_seed: 7,
            policy_account: policy_account.to_owned(),
            vault_index: 2,
            vault_pubkey: "vault-1".to_owned(),
            delegated_signers: vec!["delegated-1".to_owned()],
            threshold: 1,
            route_modes: vec!["same_mint".to_owned()],
            stable_mints: vec!["USDC".to_owned()],
            kamino_markets: vec!["market-1".to_owned()],
            kamino_liquidity_mints: vec!["USDC".to_owned()],
            universe_preset: Some("kamino_stable".to_owned()),
            risk_profile: Some("safe".to_owned()),
            swap_lanes: json!([{
                "kind": "jupiter",
                "program_id": "jupiter-1",
                "exact_in_discriminator": [1, 2, 3, 4, 5, 6, 7, 8]
            }]),
        }
    }

    fn reconciled_state(
        observed_slot: i64,
        positions: Vec<(&str, &str, u64, Option<i64>)>,
    ) -> ReconciledVaultState {
        ReconciledVaultState {
            observed_slot,
            observed_at: None,
            chain_slot: Some(observed_slot + 1),
            lock_attempt_id: None,
            context: json!({ "test_observed_slot": observed_slot }),
            positions: positions
                .into_iter()
                .map(|(reserve, liquidity_mint, amount_raw, supply_apy_bps)| {
                    ReconciledReservePosition {
                        reserve: reserve.to_owned(),
                        market: Some("market-1".to_owned()),
                        liquidity_mint: liquidity_mint.to_owned(),
                        amount_raw,
                        supply_apy_bps,
                        borrow_apy_bps: None,
                        planning_metadata: json!({ "reserve": reserve }),
                    }
                })
                .collect(),
        }
    }

    #[tokio::test]
    async fn record_policy_match_is_slot_safe_and_idempotent() {
        let Some(store) = database_store().await else {
            return;
        };
        let cluster = unique_cluster("record_policy_match");
        delete_cluster(&store, &cluster).await;

        let first = store
            .record_policy_match(policy_match(&cluster, "policy-a", 100))
            .await
            .expect("insert first policy match");
        assert_eq!(first.policy.last_seen_slot, 100);
        assert_eq!(first.policy.threshold, 1);
        assert_eq!(first.vault.active_policy_id, first.policy.id);

        let mut equal_slot = policy_match(&cluster, "policy-a", 100);
        equal_slot.signature = "sig-policy-a-equal".to_owned();
        equal_slot.threshold = 9;
        let repeated = store
            .record_policy_match(equal_slot)
            .await
            .expect("repeat same-slot policy match");
        assert_eq!(repeated.policy.id, first.policy.id);
        assert_eq!(repeated.policy.last_seen_slot, 100);
        assert_eq!(
            repeated.policy.last_seen_signature,
            first.policy.last_seen_signature
        );
        assert_eq!(repeated.policy.threshold, 1);
        assert_eq!(repeated.vault.active_policy_id, first.policy.id);

        let mut newer_same_policy = policy_match(&cluster, "policy-a", 110);
        newer_same_policy.signature = "sig-policy-a-newer".to_owned();
        newer_same_policy.threshold = 3;
        let newer = store
            .record_policy_match(newer_same_policy)
            .await
            .expect("record newer same policy match");
        assert_eq!(newer.policy.id, first.policy.id);
        assert_eq!(newer.policy.last_seen_slot, 110);
        assert_eq!(newer.policy.last_seen_signature, "sig-policy-a-newer");
        assert_eq!(newer.policy.threshold, 3);
        assert_eq!(newer.vault.active_policy_id, newer.policy.id);

        let newer_policy = store
            .record_policy_match(policy_match(&cluster, "policy-b", 120))
            .await
            .expect("record newer replacement policy");
        assert_ne!(newer_policy.policy.id, first.policy.id);
        assert_eq!(newer_policy.policy.last_seen_slot, 120);
        assert_eq!(newer_policy.vault.active_policy_id, newer_policy.policy.id);

        let older_policy = store
            .record_policy_match(policy_match(&cluster, "policy-c", 90))
            .await
            .expect("record older out-of-order policy");
        assert_eq!(older_policy.policy.last_seen_slot, 90);
        assert_eq!(older_policy.vault.active_policy_id, newer_policy.policy.id);

        let mut older_same_policy = policy_match(&cluster, "policy-b", 80);
        older_same_policy.signature = "sig-policy-b-older".to_owned();
        older_same_policy.threshold = 8;
        let older_repeat = store
            .record_policy_match(older_same_policy)
            .await
            .expect("record older repeat for active policy");
        assert_eq!(older_repeat.policy.id, newer_policy.policy.id);
        assert_eq!(older_repeat.policy.last_seen_slot, 120);
        assert_eq!(
            older_repeat.policy.last_seen_signature,
            newer_policy.policy.last_seen_signature
        );
        assert_eq!(older_repeat.policy.threshold, newer_policy.policy.threshold);
        assert_eq!(older_repeat.vault.active_policy_id, newer_policy.policy.id);

        let policy_count: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM loyal_yield.route_policies WHERE cluster = $1",
        )
        .bind(&cluster)
        .fetch_one(store.pool())
        .await
        .expect("count route policies");
        let vault_count: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM loyal_yield.managed_vaults WHERE cluster = $1",
        )
        .bind(&cluster)
        .fetch_one(store.pool())
        .await
        .expect("count managed vaults");
        assert_eq!(policy_count, 3);
        assert_eq!(vault_count, 1);

        delete_cluster(&store, &cluster).await;
    }

    #[tokio::test]
    async fn route_policy_ids_are_generated_and_used_for_active_vault_policy() {
        let Some(store) = database_store().await else {
            return;
        };
        let cluster = unique_cluster("route_policy_generated_ids");
        delete_cluster(&store, &cluster).await;

        let mut manual_id: Option<i64> = None;
        for _ in 0..10 {
            manual_id = sqlx::query_scalar(
                r#"
                WITH candidate AS (
                    SELECT GREATEST(
                        COALESCE((SELECT MAX(id) FROM loyal_yield.route_policies), 0),
                        (SELECT last_value FROM loyal_yield.route_policies_id_seq)
                    ) + 1 AS id
                ), inserted AS (
                    INSERT INTO loyal_yield.route_policies
                        (id, cluster, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
                         delegated_signers, threshold, route_modes, stable_mints, kamino_markets, kamino_liquidity_mints,
                         universe_preset, risk_profile, swap_lanes, active, last_seen_slot, last_seen_signature)
                    SELECT
                        candidate.id, $1, 'settings-manual', 'authority-manual', 1, 'policy-manual', 1, 'vault-manual',
                        ARRAY[]::TEXT[], 1, ARRAY[]::TEXT[], ARRAY[]::TEXT[], ARRAY[]::TEXT[], ARRAY[]::TEXT[],
                        NULL, NULL, '[]'::jsonb, TRUE, 1, 'sig-manual'
                    FROM candidate
                    ON CONFLICT (id) DO NOTHING
                    RETURNING id
                ), repaired AS (
                    SELECT setval('loyal_yield.route_policies_id_seq'::regclass, inserted.id, TRUE)
                    FROM inserted
                )
                SELECT inserted.id
                FROM inserted
                JOIN repaired ON TRUE
                "#,
            )
            .bind(&cluster)
            .fetch_optional(store.pool())
            .await
            .expect("insert manually numbered route policy");
            if manual_id.is_some() {
                break;
            }
        }
        let manual_id = manual_id.expect("reserve manual route policy id");

        store
            .apply_migrations()
            .await
            .expect("repair route policy id sequence");

        let mut first_match = policy_match(&cluster, "policy-a", 100);
        first_match.policy_seed = 70_000;
        let first = store
            .record_policy_match(first_match)
            .await
            .expect("record first generated policy");
        let mut second_match = policy_match(&cluster, "policy-b", 110);
        second_match.policy_seed = 70_000;
        let second = store
            .record_policy_match(second_match)
            .await
            .expect("record same-seed policy with different account");

        assert_eq!(first.policy.policy_seed, second.policy.policy_seed);
        assert_ne!(first.policy.policy_account, second.policy.policy_account);
        assert_ne!(first.policy.id, second.policy.id);
        assert!(first.policy.id.as_i64() > manual_id);
        assert!(second.policy.id.as_i64() > manual_id);
        assert_eq!(first.vault.active_policy_id, first.policy.id);
        assert_eq!(second.vault.active_policy_id, second.policy.id);
        assert_ne!(
            second.vault.active_policy_id.as_i64(),
            second.policy.policy_seed
        );

        delete_cluster(&store, &cluster).await;
    }

    #[tokio::test]
    async fn reconcile_vault_replaces_current_positions_and_keeps_snapshots() {
        let Some(store) = database_store().await else {
            return;
        };
        let cluster = unique_cluster("reconcile_vault");
        delete_cluster(&store, &cluster).await;

        let stored = store
            .record_policy_match(policy_match(&cluster, "policy-a", 100))
            .await
            .expect("record policy match");

        let first_snapshot = store
            .reconcile_vault(
                stored.vault.id,
                reconciled_state(
                    200,
                    vec![
                        ("reserve-a", "USDC", 1_000, Some(100)),
                        ("reserve-b", "USDC", 0, Some(140)),
                    ],
                ),
            )
            .await
            .expect("write first snapshot");
        assert!(first_snapshot.is_current);

        let second_snapshot = store
            .reconcile_vault(
                stored.vault.id,
                reconciled_state(
                    210,
                    vec![
                        ("reserve-a", "USDC", 700, Some(105)),
                        ("reserve-c", "PYUSD", 0, Some(180)),
                    ],
                ),
            )
            .await
            .expect("write replacement snapshot");

        let current = store
            .current_positions(stored.vault.id)
            .await
            .expect("load current positions");
        let reserves = current
            .iter()
            .map(|position| position.reserve.as_str())
            .collect::<Vec<_>>();
        assert_eq!(reserves.len(), 2);
        assert!(reserves.contains(&"reserve-a"));
        assert!(reserves.contains(&"reserve-c"));
        assert!(!reserves.contains(&"reserve-b"));
        assert!(current
            .iter()
            .all(|position| position.snapshot_id == second_snapshot.id));

        let snapshot_count: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM loyal_yield.vault_position_snapshots WHERE vault_id = $1",
        )
        .bind(stored.vault.id.as_i64())
        .fetch_one(store.pool())
        .await
        .expect("count snapshots");
        let current_snapshot_count: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM loyal_yield.vault_position_snapshots WHERE vault_id = $1 AND is_current",
        )
        .bind(stored.vault.id.as_i64())
        .fetch_one(store.pool())
        .await
        .expect("count current snapshots");
        assert_eq!(snapshot_count, 2);
        assert_eq!(current_snapshot_count, 1);

        delete_cluster(&store, &cluster).await;
    }

    #[tokio::test]
    async fn rebalance_decisions_record_mints_execution_plan_and_terminal_reuse() {
        let Some(store) = database_store().await else {
            return;
        };
        let cluster = unique_cluster("rebalance_decisions");
        delete_cluster(&store, &cluster).await;

        let stored = store
            .record_policy_match(policy_match(&cluster, "policy-a", 100))
            .await
            .expect("record policy match");
        let snapshot = store
            .reconcile_vault(
                stored.vault.id,
                reconciled_state(
                    200,
                    vec![
                        ("reserve-a", "USDC", 1_000, Some(100)),
                        ("reserve-b", "USDC", 0, Some(160)),
                        ("reserve-c", "PYUSD", 0, Some(240)),
                    ],
                ),
            )
            .await
            .expect("write snapshot");

        let same_mint = store
            .plan_same_mint_rebalance(
                stored.vault.id,
                vec![
                    ReserveScore {
                        reserve: "reserve-a".to_owned(),
                        supply_apy_bps: 100,
                        borrow_apy_bps: None,
                    },
                    ReserveScore {
                        reserve: "reserve-b".to_owned(),
                        supply_apy_bps: 160,
                        borrow_apy_bps: None,
                    },
                ],
                PlannerConfig {
                    min_edge_bps: 10,
                    estimated_cost_lamports: 5,
                },
            )
            .await
            .expect("plan same mint rebalance");

        let PlanOutcomeStatus::Planned(same_mint_decision) = same_mint.status else {
            panic!("expected same-mint planned decision");
        };
        assert_eq!(same_mint_decision.source_snapshot_id, Some(snapshot.id));
        assert_eq!(
            same_mint_decision.source_reserve.as_deref(),
            Some("reserve-a")
        );
        assert_eq!(
            same_mint_decision.target_reserve.as_deref(),
            Some("reserve-b")
        );
        assert_eq!(same_mint_decision.liquidity_mint.as_deref(), Some("USDC"));
        assert_eq!(
            same_mint_decision.source_liquidity_mint.as_deref(),
            Some("USDC")
        );
        assert_eq!(
            same_mint_decision.target_liquidity_mint.as_deref(),
            Some("USDC")
        );
        assert_eq!(same_mint_decision.amount_raw, Some(1_000));
        assert_eq!(same_mint_decision.source_apy_bps, Some(100));
        assert_eq!(same_mint_decision.target_apy_bps, Some(160));
        assert_eq!(same_mint_decision.estimated_edge_bps, Some(60));
        assert_eq!(same_mint_decision.estimated_cost_lamports, 5);
        assert_eq!(same_mint_decision.execution_plan["kind"], "same_mint");

        let blocked = store
            .record_planned_rebalance_decision(
                stored.vault.id,
                PlannedRebalanceDecisionInput {
                    source_snapshot_id: snapshot.id,
                    source_reserve: "reserve-a".to_owned(),
                    target_reserve: "reserve-c".to_owned(),
                    source_liquidity_mint: "USDC".to_owned(),
                    target_liquidity_mint: "PYUSD".to_owned(),
                    amount_raw: 1_000,
                    source_apy_bps: 100,
                    target_apy_bps: 240,
                    estimated_edge_bps: 140,
                    estimated_cost_lamports: 9,
                    execution_plan: json!({ "kind": "swap", "via": "jupiter" }),
                },
            )
            .await
            .expect("active decision blocks cross-mint plan");
        assert_eq!(
            blocked.status,
            PlanOutcomeStatus::Skipped {
                reason: SkipReason::ActiveDecision
            }
        );

        let active_count: i64 = sqlx::query_scalar(
            r#"
            SELECT COUNT(*)
            FROM loyal_yield.rebalance_decisions
            WHERE vault_id = $1
              AND status IN ('planned', 'simulating', 'ready', 'submitted', 'confirming')
            "#,
        )
        .bind(stored.vault.id.as_i64())
        .fetch_one(store.pool())
        .await
        .expect("count active decisions");
        assert_eq!(active_count, 1);

        store
            .advance_decision(
                same_mint_decision.id,
                DecisionAdvance::Fail {
                    reason: "simulation failed".to_owned(),
                },
            )
            .await
            .expect("fail same-mint decision");

        let cross_mint = store
            .record_planned_rebalance_decision(
                stored.vault.id,
                PlannedRebalanceDecisionInput {
                    source_snapshot_id: snapshot.id,
                    source_reserve: "reserve-a".to_owned(),
                    target_reserve: "reserve-c".to_owned(),
                    source_liquidity_mint: "USDC".to_owned(),
                    target_liquidity_mint: "PYUSD".to_owned(),
                    amount_raw: 1_000,
                    source_apy_bps: 100,
                    target_apy_bps: 240,
                    estimated_edge_bps: 140,
                    estimated_cost_lamports: 9,
                    execution_plan: json!({
                        "kind": "swap",
                        "legs": [
                            { "kind": "withdraw", "reserve": "reserve-a" },
                            { "kind": "swap", "source_mint": "USDC", "target_mint": "PYUSD" },
                            { "kind": "deposit", "reserve": "reserve-c" }
                        ]
                    }),
                },
            )
            .await
            .expect("record cross-mint decision after terminal prior decision");
        let PlanOutcomeStatus::Planned(cross_mint_decision) = cross_mint.status else {
            panic!("expected cross-mint planned decision");
        };
        assert_eq!(cross_mint_decision.source_snapshot_id, Some(snapshot.id));
        assert_eq!(
            cross_mint_decision.source_reserve.as_deref(),
            Some("reserve-a")
        );
        assert_eq!(
            cross_mint_decision.target_reserve.as_deref(),
            Some("reserve-c")
        );
        assert_eq!(cross_mint_decision.liquidity_mint, None);
        assert_eq!(
            cross_mint_decision.source_liquidity_mint.as_deref(),
            Some("USDC")
        );
        assert_eq!(
            cross_mint_decision.target_liquidity_mint.as_deref(),
            Some("PYUSD")
        );
        assert_eq!(cross_mint_decision.amount_raw, Some(1_000));
        assert_eq!(cross_mint_decision.execution_plan["kind"], "swap");
        assert_eq!(
            cross_mint_decision.execution_plan["legs"][1]["target_mint"],
            "PYUSD"
        );

        store
            .advance_decision(
                cross_mint_decision.id,
                DecisionAdvance::Abandon {
                    reason: "route expired".to_owned(),
                },
            )
            .await
            .expect("abandon cross-mint decision");

        let later = store
            .record_planned_rebalance_decision(
                stored.vault.id,
                PlannedRebalanceDecisionInput {
                    source_snapshot_id: snapshot.id,
                    source_reserve: "reserve-a".to_owned(),
                    target_reserve: "reserve-c".to_owned(),
                    source_liquidity_mint: "USDC".to_owned(),
                    target_liquidity_mint: "PYUSD".to_owned(),
                    amount_raw: 900,
                    source_apy_bps: 100,
                    target_apy_bps: 250,
                    estimated_edge_bps: 150,
                    estimated_cost_lamports: 11,
                    execution_plan: json!({ "kind": "swap", "nonce": "later" }),
                },
            )
            .await
            .expect("terminal decisions do not block later decisions");
        assert!(matches!(later.status, PlanOutcomeStatus::Planned(_)));

        delete_cluster(&store, &cluster).await;
    }
}
