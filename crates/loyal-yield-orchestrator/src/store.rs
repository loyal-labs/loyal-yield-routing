use crate::domain::{draft_same_mint_decision, state_transition, PlannedDecision};
use crate::types::*;
use crate::{OrchestratorError, ACTIVE_DECISION_STATUSES};
use chrono::{DateTime, Utc};
use serde_json::Value;
use sha2::{Digest, Sha256};
use sqlx::postgres::PgPoolOptions;
use sqlx::{PgConnection, PgPool, Row};

const MIGRATION_0001: &str = include_str!("../migrations/0001_loyal_yield_orchestration.sql");

#[derive(Clone)]
pub struct NeonSqlClient {
    pool: PgPool,
}

pub type OrchestratorStore = NeonSqlClient;

#[derive(Debug, sqlx::FromRow)]
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

#[derive(Debug, sqlx::FromRow)]
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

#[derive(Debug, sqlx::FromRow)]
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

#[derive(Debug, sqlx::FromRow)]
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

#[derive(Debug, sqlx::FromRow)]
struct DecisionRow {
    id: i64,
    vault_id: i64,
    source_snapshot_id: Option<i64>,
    status: String,
    source_reserve: Option<String>,
    target_reserve: Option<String>,
    liquidity_mint: Option<String>,
    amount_raw: Option<i64>,
    source_apy_bps: Option<i64>,
    target_apy_bps: Option<i64>,
    estimated_edge_bps: Option<i64>,
    estimated_cost_lamports: i64,
    decision_reason: String,
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

    pub async fn current_positions(
        &self,
        vault_id: VaultId,
    ) -> Result<Vec<CurrentReservePosition>, OrchestratorError> {
        let rows = sqlx::query_as::<_, CurrentPositionRow>(
            "SELECT vault_id, reserve, market, liquidity_mint, amount_raw, supply_apy_bps, borrow_apy_bps, \
             has_value, snapshot_id, observed_slot, observed_at, planning_metadata \
             FROM loyal_yield.vault_reserve_positions_current WHERE vault_id = $1",
        )
        .bind(vault_id.as_i64())
        .fetch_all(&self.pool)
        .await?;

        rows.into_iter()
            .map(|row| {
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
            })
            .collect()
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

        sqlx::query("UPDATE loyal_yield.vault_position_snapshots SET is_current = FALSE WHERE vault_id = $1 AND is_current")
            .bind(vault_id.as_i64())
            .execute(&mut *tx)
            .await?;

        let snapshot_row = sqlx::query_as::<_, SnapshotRow>(
            "INSERT INTO loyal_yield.vault_position_snapshots \
             (vault_id, policy_id, observed_slot, observed_at, chain_slot, lock_attempt_id, context) \
             VALUES ($1, $2, $3, COALESCE($4, now()), $5, $6, $7) \
             RETURNING id, vault_id, policy_id, observed_slot, observed_at, chain_slot, lock_attempt_id, is_current, context",
        )
        .bind(vault_id.as_i64())
        .bind(vault.active_policy_id.as_i64())
        .bind(state.observed_slot)
        .bind(state.observed_at)
        .bind(state.chain_slot)
        .bind(state.lock_attempt_id)
        .bind(state.context)
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

            sqlx::query(
                "INSERT INTO loyal_yield.vault_position_snapshot_positions \
                 (snapshot_id, reserve, market, liquidity_mint, amount_raw, supply_apy_bps, borrow_apy_bps, has_value, planning_metadata) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)",
            )
            .bind(snapshot_row.id)
            .bind(&reserve)
            .bind(&market)
            .bind(&liquidity_mint)
            .bind(amount)
            .bind(supply_apy_bps)
            .bind(borrow_apy_bps)
            .bind(amount > 0)
            .bind(&planning_metadata)
            .execute(&mut *tx)
            .await?;

            sqlx::query(
                "INSERT INTO loyal_yield.vault_reserve_positions_current \
                 (vault_id, reserve, market, liquidity_mint, amount_raw, has_value, supply_apy_bps, borrow_apy_bps, \
                  snapshot_id, observed_slot, observed_at, planning_metadata) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) \
                 ON CONFLICT (vault_id, reserve) DO UPDATE SET \
                    amount_raw = EXCLUDED.amount_raw, \
                    has_value = EXCLUDED.has_value, \
                    supply_apy_bps = EXCLUDED.supply_apy_bps, \
                    borrow_apy_bps = EXCLUDED.borrow_apy_bps, \
                    snapshot_id = EXCLUDED.snapshot_id, \
                    observed_slot = EXCLUDED.observed_slot, \
                    observed_at = EXCLUDED.observed_at, \
                    market = EXCLUDED.market, \
                    liquidity_mint = EXCLUDED.liquidity_mint, \
                    planning_metadata = EXCLUDED.planning_metadata",
            )
            .bind(vault_id.as_i64())
            .bind(&reserve)
            .bind(&market)
            .bind(&liquidity_mint)
            .bind(amount)
            .bind(amount > 0)
            .bind(supply_apy_bps)
            .bind(borrow_apy_bps)
            .bind(snapshot_row.id)
            .bind(snapshot_row.observed_slot)
            .bind(snapshot_row.observed_at)
            .bind(&planning_metadata)
            .execute(&mut *tx)
            .await?;
        }

        sqlx::query(
            "DELETE FROM loyal_yield.vault_reserve_positions_current \
             WHERE vault_id = $1 AND NOT (reserve = ANY($2))",
        )
        .bind(vault_id.as_i64())
        .bind(&observed_reserves)
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
        let row = sqlx::query_as::<_, DecisionRow>(
            "UPDATE loyal_yield.rebalance_decisions \
             SET status = $2, signature = COALESCE($3, signature), submitted_slot = COALESCE($4, submitted_slot), \
             confirmed_slot = COALESCE($5, confirmed_slot), preflight_chain_slot = COALESCE($6, preflight_chain_slot), \
             post_snapshot_id = COALESCE($7, post_snapshot_id), abandon_reason = COALESCE($8, abandon_reason), \
             updated_at = now() \
             WHERE id = $1 \
             RETURNING id, vault_id, source_snapshot_id, status, source_reserve, target_reserve, liquidity_mint, amount_raw, \
                     source_apy_bps, target_apy_bps, estimated_edge_bps, estimated_cost_lamports, decision_reason, abandon_reason, \
                     signature, submitted_slot, confirmed_slot, preflight_chain_slot, post_snapshot_id, created_at, updated_at",
        )
        .bind(decision_id.as_i64())
        .bind(status)
        .bind(transition.signature)
        .bind(transition.submitted_slot)
        .bind(transition.confirmed_slot)
        .bind(transition.preflight_chain_slot)
        .bind(transition.post_snapshot_id.map(SnapshotId::as_i64))
        .bind(transition.abandon_reason)
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
    let row = sqlx::query_as::<_, RoutePolicyRow>(
        "INSERT INTO loyal_yield.route_policies \
         (cluster, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey, \
          delegated_signers, threshold, route_modes, stable_mints, kamino_markets, kamino_liquidity_mints, \
          universe_preset, risk_profile, swap_lanes, active, last_seen_slot, last_seen_signature) \
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, TRUE, $17, $18) \
         ON CONFLICT (cluster, policy_account) DO UPDATE SET \
             settings = EXCLUDED.settings, \
             authority = EXCLUDED.authority, \
             policy_seed = EXCLUDED.policy_seed, \
             vault_index = EXCLUDED.vault_index, \
             vault_pubkey = EXCLUDED.vault_pubkey, \
             delegated_signers = EXCLUDED.delegated_signers, \
             threshold = EXCLUDED.threshold, \
             route_modes = EXCLUDED.route_modes, \
             stable_mints = EXCLUDED.stable_mints, \
             kamino_markets = EXCLUDED.kamino_markets, \
             kamino_liquidity_mints = EXCLUDED.kamino_liquidity_mints, \
             universe_preset = EXCLUDED.universe_preset, \
             risk_profile = EXCLUDED.risk_profile, \
             swap_lanes = EXCLUDED.swap_lanes, \
             active = TRUE, \
             last_seen_at = now(), \
             last_seen_slot = EXCLUDED.last_seen_slot, \
             last_seen_signature = EXCLUDED.last_seen_signature \
         RETURNING id, cluster, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey, \
                   delegated_signers, threshold, route_modes, stable_mints, kamino_markets, kamino_liquidity_mints, \
                   universe_preset, risk_profile, swap_lanes, active, first_seen_at, last_seen_at, last_seen_slot, last_seen_signature",
    )
    .bind(&event.cluster)
    .bind(&event.settings)
    .bind(&event.authority)
    .bind(policy_seed)
    .bind(&event.policy_account)
    .bind(i16::from(event.vault_index))
    .bind(&event.vault_pubkey)
    .bind(&event.delegated_signers)
    .bind(i32::from(event.threshold))
    .bind(&event.route_modes)
    .bind(&event.stable_mints)
    .bind(&event.kamino_markets)
    .bind(&event.kamino_liquidity_mints)
    .bind(event.universe_preset.as_deref())
    .bind(event.risk_profile.as_deref())
    .bind(&event.swap_lanes)
    .bind(slot)
    .bind(&event.signature)
    .fetch_one(conn)
    .await?;

    Ok(RoutePolicy {
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
    })
}

async fn upsert_vault(
    conn: &mut PgConnection,
    policy_id: PolicyId,
    event: &PolicyMatchInput,
) -> Result<ManagedVault, OrchestratorError> {
    let row = sqlx::query_as::<_, ManagedVaultRow>(
        "INSERT INTO loyal_yield.managed_vaults \
         (cluster, settings, vault_index, vault_pubkey, active_policy_id, active) \
         VALUES ($1, $2, $3, $4, $5, TRUE) \
         ON CONFLICT (cluster, settings, vault_index, vault_pubkey) DO UPDATE SET \
             active_policy_id = EXCLUDED.active_policy_id, \
             active = TRUE, \
             last_seen_at = now() \
         RETURNING id, cluster, settings, vault_index, vault_pubkey, active_policy_id, active, first_seen_at, last_seen_at",
    )
    .bind(&event.cluster)
    .bind(&event.settings)
    .bind(i16::from(event.vault_index))
    .bind(&event.vault_pubkey)
    .bind(policy_id.as_i64())
    .fetch_one(conn)
    .await?;

    Ok(ManagedVault {
        id: VaultId(row.id),
        cluster: row.cluster,
        settings: row.settings,
        vault_index: row.vault_index,
        vault_pubkey: row.vault_pubkey,
        active_policy_id: PolicyId(row.active_policy_id),
        active: row.active,
        first_seen_at: row.first_seen_at,
        last_seen_at: row.last_seen_at,
    })
}

async fn fetch_managed_vault_for_update(
    conn: &mut PgConnection,
    vault_id: VaultId,
) -> Result<ManagedVault, OrchestratorError> {
    let row = sqlx::query_as::<_, ManagedVaultRow>(
        "SELECT id, cluster, settings, vault_index, vault_pubkey, active_policy_id, active, first_seen_at, last_seen_at \
         FROM loyal_yield.managed_vaults WHERE id = $1 FOR UPDATE",
    )
    .bind(vault_id.as_i64())
    .fetch_one(conn)
    .await?;

    Ok(ManagedVault {
        id: VaultId(row.id),
        cluster: row.cluster,
        settings: row.settings,
        vault_index: row.vault_index,
        vault_pubkey: row.vault_pubkey,
        active_policy_id: PolicyId(row.active_policy_id),
        active: row.active,
        first_seen_at: row.first_seen_at,
        last_seen_at: row.last_seen_at,
    })
}

async fn active_decision_exists(
    conn: &mut PgConnection,
    vault_id: VaultId,
) -> Result<bool, OrchestratorError> {
    let row = sqlx::query("SELECT EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions WHERE vault_id = $1 AND status::text = ANY($2))")
        .bind(vault_id.as_i64())
        .bind(&ACTIVE_DECISION_STATUSES)
        .fetch_one(conn)
        .await?;
    row.try_get::<bool, _>(0).map_err(OrchestratorError::from)
}

async fn current_positions_for_update(
    conn: &mut PgConnection,
    vault_id: VaultId,
) -> Result<Vec<CurrentReservePosition>, OrchestratorError> {
    let rows = sqlx::query_as::<_, CurrentPositionRow>(
        "SELECT vault_id, reserve, market, liquidity_mint, amount_raw, supply_apy_bps, borrow_apy_bps, \
         has_value, snapshot_id, observed_slot, observed_at, planning_metadata \
         FROM loyal_yield.vault_reserve_positions_current WHERE vault_id = $1 FOR UPDATE",
    )
    .bind(vault_id.as_i64())
    .fetch_all(conn)
    .await?;

    rows.into_iter()
        .map(|row| {
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
        })
        .collect()
}

async fn insert_planned_decision(
    conn: &mut PgConnection,
    vault_id: VaultId,
    planned: &PlannedDecision,
    estimated_cost_lamports: i64,
) -> Result<DecisionRow, OrchestratorError> {
    let idempotency_key = rebalance_idempotency_key(vault_id, planned.source_snapshot_id, planned);
    sqlx::query_as::<_, DecisionRow>(
        "INSERT INTO loyal_yield.rebalance_decisions \
         (vault_id, source_snapshot_id, status, source_reserve, target_reserve, liquidity_mint, amount_raw, \
          source_apy_bps, target_apy_bps, estimated_edge_bps, estimated_cost_lamports, decision_reason, idempotency_key) \
         VALUES ($1, $2, 'planned', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) \
         ON CONFLICT (idempotency_key) DO UPDATE SET updated_at = now() \
         RETURNING id, vault_id, source_snapshot_id, status, source_reserve, target_reserve, liquidity_mint, amount_raw, \
         source_apy_bps, target_apy_bps, estimated_edge_bps, estimated_cost_lamports, decision_reason, abandon_reason, \
         signature, submitted_slot, confirmed_slot, preflight_chain_slot, post_snapshot_id, created_at, updated_at",
    )
    .bind(vault_id.as_i64())
    .bind(planned.source_snapshot_id.as_i64())
    .bind(&planned.source_reserve)
    .bind(&planned.target_reserve)
    .bind(&planned.liquidity_mint)
    .bind(planned.amount_raw)
    .bind(planned.source_apy_bps)
    .bind(planned.target_apy_bps)
    .bind(planned.estimated_edge_bps)
    .bind(estimated_cost_lamports)
    .bind(DecisionReason::TargetSupplyApyExceedsSource.as_str())
    .bind(idempotency_key)
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
    sqlx::query_as::<_, DecisionRow>(
        "INSERT INTO loyal_yield.rebalance_decisions \
         (vault_id, status, estimated_cost_lamports, decision_reason, idempotency_key) \
         VALUES ($1, 'skipped', 0, $2, $3) \
         ON CONFLICT (idempotency_key) DO UPDATE SET updated_at = now() \
         RETURNING id, vault_id, source_snapshot_id, status, source_reserve, target_reserve, liquidity_mint, amount_raw, \
         source_apy_bps, target_apy_bps, estimated_edge_bps, estimated_cost_lamports, decision_reason, abandon_reason, \
         signature, submitted_slot, confirmed_slot, preflight_chain_slot, post_snapshot_id, created_at, updated_at",
    )
    .bind(vault_id.as_i64())
    .bind(reason.decision_reason().as_str())
    .bind(idempotency_key)
    .fetch_one(conn)
    .await
    .map_err(OrchestratorError::from)
}

async fn fetch_decision_for_update(
    conn: &mut PgConnection,
    decision_id: DecisionId,
) -> Result<RebalanceDecision, OrchestratorError> {
    let row = sqlx::query_as::<_, DecisionRow>(
        "SELECT id, vault_id, source_snapshot_id, status, source_reserve, target_reserve, liquidity_mint, amount_raw, \
         source_apy_bps, target_apy_bps, estimated_edge_bps, estimated_cost_lamports, decision_reason, abandon_reason, \
         signature, submitted_slot, confirmed_slot, preflight_chain_slot, post_snapshot_id, created_at, updated_at \
         FROM loyal_yield.rebalance_decisions WHERE id = $1 FOR UPDATE",
    )
    .bind(decision_id.as_i64())
    .fetch_one(conn)
    .await?;
    from_row_to_decision(row)
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
        amount_raw: row.amount_raw,
        source_apy_bps: row.source_apy_bps,
        target_apy_bps: row.target_apy_bps,
        estimated_edge_bps: row.estimated_edge_bps,
        estimated_cost_lamports: row.estimated_cost_lamports,
        decision_reason,
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
    hasher.update(planned.liquidity_mint.as_bytes());
    hasher.update(planned.amount_raw.to_le_bytes());
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
