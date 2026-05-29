//! Postgres-backed orchestration state for Loyal yield routing.
//!
//! Chain reconciliation remains the source of truth. This crate records policy
//! capabilities, immutable vault position snapshots, rebalance decisions, and
//! idempotent execution transitions.

use std::{fmt, time::Duration};

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use sqlx::{postgres::PgPoolOptions, FromRow, PgPool, Postgres, QueryBuilder, Transaction};
use thiserror::Error;

const MIGRATION_0001: &str = include_str!("../migrations/0001_loyal_yield_orchestration.sql");
const ACTIVE_ATTEMPT_STATUSES: &[AttemptStatus] = &[
    AttemptStatus::Planned,
    AttemptStatus::Simulating,
    AttemptStatus::Ready,
    AttemptStatus::Submitted,
    AttemptStatus::Confirming,
];

#[derive(Debug, Clone)]
pub struct OrchestratorConfig {
    pub url: String,
    pub max_connections: u32,
    pub acquire_timeout: Duration,
}

impl OrchestratorConfig {
    pub fn new(url: impl Into<String>) -> Self {
        Self {
            url: url.into(),
            max_connections: 5,
            acquire_timeout: Duration::from_secs(5),
        }
    }
}

#[derive(Clone)]
pub struct OrchestratorStore {
    pool: PgPool,
}

impl OrchestratorStore {
    pub async fn connect(config: OrchestratorConfig) -> Result<Self, OrchestratorError> {
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
        for statement in MIGRATION_0001.split(';').map(str::trim) {
            if statement.is_empty() {
                continue;
            }
            sqlx::query(statement).execute(&self.pool).await?;
        }
        Ok(())
    }

    pub async fn record_policy_match(
        &self,
        event: PolicyMatchInput,
    ) -> Result<StoredPolicyMatch, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let policy = upsert_policy(&mut tx, &event).await?;
        let vault = upsert_managed_vault(&mut tx, policy.id, &event).await?;
        tx.commit().await?;
        Ok(StoredPolicyMatch { policy, vault })
    }

    pub async fn reconcile_vault(
        &self,
        vault_id: i64,
        state: ReconciledVaultState,
    ) -> Result<PositionSnapshot, OrchestratorError> {
        if state.positions.is_empty() {
            return Err(OrchestratorError::EmptySnapshot);
        }

        let mut tx = self.pool.begin().await?;
        let vault = fetch_managed_vault_for_update(&mut tx, vault_id).await?;
        sqlx::query(
            "UPDATE loyal_yield.vault_position_snapshots SET is_current = FALSE WHERE vault_id = $1 AND is_current",
        )
        .bind(vault_id)
        .execute(&mut *tx)
        .await?;

        let snapshot = sqlx::query_as::<_, PositionSnapshot>(
            "INSERT INTO loyal_yield.vault_position_snapshots \
             (vault_id, policy_id, observed_slot, observed_at, chain_slot, lock_attempt_id, context) \
             VALUES ($1, $2, $3, COALESCE($4, now()), $5, $6, $7) \
             RETURNING id, vault_id, policy_id, observed_slot, observed_at, chain_slot, lock_attempt_id, is_current, context",
        )
        .bind(vault_id)
        .bind(vault.active_policy_id)
        .bind(state.observed_slot)
        .bind(state.observed_at)
        .bind(state.chain_slot)
        .bind(state.lock_attempt_id)
        .bind(state.context)
        .fetch_one(&mut *tx)
        .await?;

        for position in state.positions {
            sqlx::query(
                "INSERT INTO loyal_yield.vault_position_snapshot_positions \
                 (snapshot_id, reserve, market, liquidity_mint, amount_raw, supply_apy_bps, borrow_apy_bps, has_value, planning_metadata) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)",
            )
            .bind(snapshot.id)
            .bind(position.reserve)
            .bind(position.market)
            .bind(position.liquidity_mint)
            .bind(position.amount_raw.to_string())
            .bind(position.supply_apy_bps)
            .bind(position.borrow_apy_bps)
            .bind(position.amount_raw > 0)
            .bind(position.planning_metadata)
            .execute(&mut *tx)
            .await?;
        }

        tx.commit().await?;
        Ok(snapshot)
    }

    pub async fn plan_same_mint_rebalance(
        &self,
        vault_id: i64,
        snapshot_id: i64,
        config: PlannerConfig,
    ) -> Result<PlanOutcome, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let vault = fetch_managed_vault_for_update(&mut tx, vault_id).await?;
        if let Some(active_attempt) = fetch_active_attempt(&mut tx, vault_id).await? {
            insert_vault_event(
                &mut tx,
                None,
                vault_id,
                "plan_skipped",
                None,
                None,
                Some("active_attempt"),
                json!({ "active_attempt_id": active_attempt.id }),
            )
            .await?;
            tx.commit().await?;
            return Ok(PlanOutcome::Skipped {
                reason: SkipReason::ActiveAttempt,
            });
        }

        let snapshot = fetch_snapshot_for_update(&mut tx, snapshot_id).await?;
        if snapshot.vault_id != vault_id
            || !snapshot.is_current
            || snapshot.policy_id != vault.active_policy_id
        {
            insert_vault_event(
                &mut tx,
                None,
                vault_id,
                "plan_skipped",
                None,
                None,
                Some("stale_snapshot"),
                json!({ "snapshot_id": snapshot_id }),
            )
            .await?;
            tx.commit().await?;
            return Ok(PlanOutcome::Skipped {
                reason: SkipReason::StaleSnapshot,
            });
        }

        let positions = fetch_snapshot_positions(&mut tx, snapshot_id).await?;
        let decision = plan_same_mint_from_positions(&positions, config)?;
        let Some(decision) = decision else {
            insert_vault_event(
                &mut tx,
                None,
                vault_id,
                "plan_skipped",
                None,
                None,
                Some("no_same_mint_edge"),
                json!({ "snapshot_id": snapshot_id }),
            )
            .await?;
            tx.commit().await?;
            return Ok(PlanOutcome::Skipped {
                reason: SkipReason::NoSameMintEdge,
            });
        };

        let idempotency_key = rebalance_idempotency_key(vault_id, snapshot_id, &decision);
        let attempt = sqlx::query_as::<_, RebalanceAttempt>(
            "INSERT INTO loyal_yield.rebalance_attempts \
             (vault_id, source_snapshot_id, status, source_reserve, target_reserve, liquidity_mint, amount_raw, \
              source_apy_bps, target_apy_bps, estimated_edge_bps, estimated_cost_lamports, decision_reason, idempotency_key) \
             VALUES ($1, $2, 'planned', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) \
             ON CONFLICT (idempotency_key) DO UPDATE SET updated_at = loyal_yield.rebalance_attempts.updated_at \
             RETURNING id, vault_id, source_snapshot_id, status, source_reserve, target_reserve, liquidity_mint, amount_raw, \
                       source_apy_bps, target_apy_bps, estimated_edge_bps, estimated_cost_lamports, decision_reason, \
                       abandon_reason, idempotency_key, signature, submitted_slot, confirmed_slot, preflight_chain_slot, \
                       post_snapshot_id, created_at, updated_at",
        )
        .bind(vault_id)
        .bind(snapshot_id)
        .bind(&decision.source_reserve)
        .bind(&decision.target_reserve)
        .bind(&decision.liquidity_mint)
        .bind(decision.amount_raw.to_string())
        .bind(decision.source_apy_bps)
        .bind(decision.target_apy_bps)
        .bind(decision.estimated_edge_bps)
        .bind(config.estimated_cost_lamports)
        .bind("target_supply_apy_exceeds_source")
        .bind(idempotency_key)
        .fetch_one(&mut *tx)
        .await?;

        insert_vault_event(
            &mut tx,
            Some(attempt.id),
            vault_id,
            "attempt_planned",
            None,
            Some(AttemptStatus::Planned.as_str()),
            Some("target_supply_apy_exceeds_source"),
            json!({
                "snapshot_id": snapshot_id,
                "source_reserve": attempt.source_reserve,
                "target_reserve": attempt.target_reserve,
                "amount_raw": attempt.amount_raw,
            }),
        )
        .await?;
        tx.commit().await?;
        Ok(PlanOutcome::Planned(attempt))
    }

    pub async fn advance_attempt(
        &self,
        attempt_id: i64,
        advance: AttemptAdvance,
    ) -> Result<RebalanceAttempt, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let attempt = fetch_attempt_for_update(&mut tx, attempt_id).await?;
        let current_status = attempt.status()?;
        let transition = current_status.transition(advance)?;
        if transition.to == current_status && transition.idempotent {
            tx.commit().await?;
            return Ok(attempt);
        }

        let updated = update_attempt_status(&mut tx, attempt_id, &transition).await?;
        insert_vault_event(
            &mut tx,
            Some(updated.id),
            updated.vault_id,
            "attempt_transition",
            Some(current_status.as_str()),
            Some(transition.to.as_str()),
            transition.reason.as_deref(),
            transition.payload,
        )
        .await?;
        tx.commit().await?;
        Ok(updated)
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct PolicyMatchInput {
    pub signature: String,
    pub slot: u64,
    pub cluster: String,
    pub settings: String,
    pub authority: String,
    pub policy_seed: u64,
    pub policy_account: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub delegated_signers: Vec<String>,
    pub threshold: u16,
    pub route_modes: Vec<String>,
    pub stable_mints: Vec<String>,
    pub kamino_markets: Vec<String>,
    pub kamino_liquidity_mints: Vec<String>,
    pub universe_preset: Option<String>,
    pub risk_profile: Option<String>,
    pub swap_lanes: Value,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StoredPolicyMatch {
    pub policy: RoutePolicy,
    pub vault: ManagedVault,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, FromRow)]
pub struct RoutePolicy {
    pub id: i64,
    pub cluster: String,
    pub settings: String,
    pub authority: String,
    pub policy_seed: i64,
    pub policy_account: String,
    pub vault_index: i16,
    pub vault_pubkey: String,
    pub delegated_signers: Value,
    pub threshold: i32,
    pub route_modes: Value,
    pub stable_mints: Value,
    pub kamino_markets: Value,
    pub kamino_liquidity_mints: Value,
    pub universe_preset: Option<String>,
    pub risk_profile: Option<String>,
    pub swap_lanes: Value,
    pub active: bool,
    pub first_seen_at: DateTime<Utc>,
    pub last_seen_at: DateTime<Utc>,
    pub last_seen_slot: i64,
    pub last_seen_signature: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, FromRow)]
pub struct ManagedVault {
    pub id: i64,
    pub cluster: String,
    pub settings: String,
    pub vault_index: i16,
    pub vault_pubkey: String,
    pub active_policy_id: i64,
    pub active: bool,
    pub first_seen_at: DateTime<Utc>,
    pub last_seen_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ReconciledVaultState {
    pub observed_slot: i64,
    pub observed_at: Option<DateTime<Utc>>,
    pub chain_slot: Option<i64>,
    pub lock_attempt_id: Option<i64>,
    pub context: Value,
    pub positions: Vec<ReconciledReservePosition>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ReconciledReservePosition {
    pub reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
    pub amount_raw: u64,
    pub supply_apy_bps: Option<i64>,
    pub borrow_apy_bps: Option<i64>,
    pub planning_metadata: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, FromRow)]
pub struct PositionSnapshot {
    pub id: i64,
    pub vault_id: i64,
    pub policy_id: i64,
    pub observed_slot: i64,
    pub observed_at: DateTime<Utc>,
    pub chain_slot: Option<i64>,
    pub lock_attempt_id: Option<i64>,
    pub is_current: bool,
    pub context: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, FromRow)]
pub struct SnapshotPosition {
    pub id: i64,
    pub snapshot_id: i64,
    pub reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
    pub amount_raw: String,
    pub supply_apy_bps: Option<i64>,
    pub borrow_apy_bps: Option<i64>,
    pub has_value: bool,
    pub planning_metadata: Value,
}

impl SnapshotPosition {
    pub fn amount(&self) -> Result<u64, OrchestratorError> {
        self.amount_raw
            .parse()
            .map_err(|_| OrchestratorError::InvalidAmount(self.amount_raw.clone()))
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PlannerConfig {
    pub min_edge_bps: i64,
    pub estimated_cost_lamports: i64,
}

impl Default for PlannerConfig {
    fn default() -> Self {
        Self {
            min_edge_bps: 1,
            estimated_cost_lamports: 0,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PlanOutcome {
    Planned(RebalanceAttempt),
    Skipped { reason: SkipReason },
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SkipReason {
    ActiveAttempt,
    StaleSnapshot,
    NoValuedSource,
    NoSameMintEdge,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, FromRow)]
pub struct RebalanceAttempt {
    pub id: i64,
    pub vault_id: i64,
    pub source_snapshot_id: i64,
    pub status: String,
    pub source_reserve: String,
    pub target_reserve: String,
    pub liquidity_mint: String,
    pub amount_raw: String,
    pub source_apy_bps: Option<i64>,
    pub target_apy_bps: Option<i64>,
    pub estimated_edge_bps: Option<i64>,
    pub estimated_cost_lamports: Option<i64>,
    pub decision_reason: String,
    pub abandon_reason: Option<String>,
    pub idempotency_key: String,
    pub signature: Option<String>,
    pub submitted_slot: Option<i64>,
    pub confirmed_slot: Option<i64>,
    pub preflight_chain_slot: Option<i64>,
    pub post_snapshot_id: Option<i64>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

impl RebalanceAttempt {
    pub fn status(&self) -> Result<AttemptStatus, OrchestratorError> {
        AttemptStatus::parse(&self.status)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct RebalanceDecision {
    source_reserve: String,
    target_reserve: String,
    liquidity_mint: String,
    amount_raw: u64,
    source_apy_bps: Option<i64>,
    target_apy_bps: Option<i64>,
    estimated_edge_bps: Option<i64>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AttemptStatus {
    Planned,
    Simulating,
    Ready,
    Submitted,
    Confirming,
    Confirmed,
    Failed,
    Abandoned,
}

impl AttemptStatus {
    pub fn parse(value: &str) -> Result<Self, OrchestratorError> {
        match value {
            "planned" => Ok(Self::Planned),
            "simulating" => Ok(Self::Simulating),
            "ready" => Ok(Self::Ready),
            "submitted" => Ok(Self::Submitted),
            "confirming" => Ok(Self::Confirming),
            "confirmed" => Ok(Self::Confirmed),
            "failed" => Ok(Self::Failed),
            "abandoned" => Ok(Self::Abandoned),
            _ => Err(OrchestratorError::UnknownAttemptStatus(value.to_owned())),
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Self::Planned => "planned",
            Self::Simulating => "simulating",
            Self::Ready => "ready",
            Self::Submitted => "submitted",
            Self::Confirming => "confirming",
            Self::Confirmed => "confirmed",
            Self::Failed => "failed",
            Self::Abandoned => "abandoned",
        }
    }

    fn is_terminal(self) -> bool {
        matches!(self, Self::Confirmed | Self::Failed | Self::Abandoned)
    }

    fn transition(self, advance: AttemptAdvance) -> Result<AttemptTransition, OrchestratorError> {
        if self.is_terminal() {
            let valid_terminal_repeat = matches!(
                (self, &advance),
                (Self::Confirmed, AttemptAdvance::Confirm { .. })
                    | (Self::Failed, AttemptAdvance::Fail { .. })
                    | (Self::Abandoned, AttemptAdvance::Abandon { .. })
            );
            if valid_terminal_repeat {
                return Ok(AttemptTransition::idempotent(self));
            }
            return Err(OrchestratorError::TerminalAttempt(self));
        }

        match (self, advance) {
            (Self::Planned, AttemptAdvance::StartSimulation) => {
                Ok(AttemptTransition::simple(Self::Simulating))
            }
            (Self::Simulating, AttemptAdvance::SimulationReady) => {
                Ok(AttemptTransition::simple(Self::Ready))
            }
            (Self::Ready, AttemptAdvance::Submit { signature, slot }) => Ok(AttemptTransition {
                to: Self::Submitted,
                idempotent: false,
                signature: Some(signature.clone()),
                submitted_slot: slot,
                confirmed_slot: None,
                preflight_chain_slot: None,
                post_snapshot_id: None,
                abandon_reason: None,
                reason: Some("submitted".to_owned()),
                payload: json!({ "signature": signature, "slot": slot }),
            }),
            (Self::Submitted, AttemptAdvance::StartConfirmation) => {
                Ok(AttemptTransition::simple(Self::Confirming))
            }
            (
                Self::Confirming | Self::Submitted,
                AttemptAdvance::Confirm {
                    slot,
                    post_snapshot_id,
                },
            ) => Ok(AttemptTransition {
                to: Self::Confirmed,
                idempotent: false,
                signature: None,
                submitted_slot: None,
                confirmed_slot: slot,
                preflight_chain_slot: None,
                post_snapshot_id,
                abandon_reason: None,
                reason: Some("confirmed".to_owned()),
                payload: json!({ "slot": slot, "post_snapshot_id": post_snapshot_id }),
            }),
            (_, AttemptAdvance::Fail { reason }) => Ok(AttemptTransition {
                to: Self::Failed,
                idempotent: false,
                signature: None,
                submitted_slot: None,
                confirmed_slot: None,
                preflight_chain_slot: None,
                post_snapshot_id: None,
                abandon_reason: Some(reason.clone()),
                reason: Some(reason.clone()),
                payload: json!({ "reason": reason }),
            }),
            (_, AttemptAdvance::Abandon { reason }) => Ok(AttemptTransition {
                to: Self::Abandoned,
                idempotent: false,
                signature: None,
                submitted_slot: None,
                confirmed_slot: None,
                preflight_chain_slot: None,
                post_snapshot_id: None,
                abandon_reason: Some(reason.clone()),
                reason: Some(reason.clone()),
                payload: json!({ "reason": reason }),
            }),
            (status, advance) => Err(OrchestratorError::InvalidTransition {
                from: status,
                advance,
            }),
        }
    }
}

impl fmt::Display for AttemptStatus {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AttemptAdvance {
    StartSimulation,
    SimulationReady,
    Submit {
        signature: String,
        slot: Option<i64>,
    },
    StartConfirmation,
    Confirm {
        slot: Option<i64>,
        post_snapshot_id: Option<i64>,
    },
    Fail {
        reason: String,
    },
    Abandon {
        reason: String,
    },
}

#[derive(Debug)]
struct AttemptTransition {
    to: AttemptStatus,
    idempotent: bool,
    signature: Option<String>,
    submitted_slot: Option<i64>,
    confirmed_slot: Option<i64>,
    preflight_chain_slot: Option<i64>,
    post_snapshot_id: Option<i64>,
    abandon_reason: Option<String>,
    reason: Option<String>,
    payload: Value,
}

impl AttemptTransition {
    fn simple(to: AttemptStatus) -> Self {
        Self {
            to,
            idempotent: false,
            signature: None,
            submitted_slot: None,
            confirmed_slot: None,
            preflight_chain_slot: None,
            post_snapshot_id: None,
            abandon_reason: None,
            reason: None,
            payload: json!({}),
        }
    }

    fn idempotent(to: AttemptStatus) -> Self {
        Self {
            idempotent: true,
            ..Self::simple(to)
        }
    }
}

#[derive(Debug, Error)]
pub enum OrchestratorError {
    #[error("database error: {0}")]
    Sqlx(#[from] sqlx::Error),
    #[error("policy match slot {0} does not fit Postgres BIGINT")]
    SlotOutOfRange(u64),
    #[error("policy seed {0} does not fit Postgres BIGINT")]
    PolicySeedOutOfRange(u64),
    #[error("snapshot must include at least one supported reserve position")]
    EmptySnapshot,
    #[error("invalid amount_raw value: {0}")]
    InvalidAmount(String),
    #[error("unknown attempt status: {0}")]
    UnknownAttemptStatus(String),
    #[error("attempt is terminal in status {0}")]
    TerminalAttempt(AttemptStatus),
    #[error("invalid attempt transition from {from} with {advance:?}")]
    InvalidTransition {
        from: AttemptStatus,
        advance: AttemptAdvance,
    },
}

async fn upsert_policy(
    tx: &mut Transaction<'_, Postgres>,
    event: &PolicyMatchInput,
) -> Result<RoutePolicy, OrchestratorError> {
    let slot =
        i64::try_from(event.slot).map_err(|_| OrchestratorError::SlotOutOfRange(event.slot))?;
    let policy_seed = i64::try_from(event.policy_seed)
        .map_err(|_| OrchestratorError::PolicySeedOutOfRange(event.policy_seed))?;
    sqlx::query_as::<_, RoutePolicy>(
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
    .bind(json!(event.delegated_signers))
    .bind(i32::from(event.threshold))
    .bind(json!(event.route_modes))
    .bind(json!(event.stable_mints))
    .bind(json!(event.kamino_markets))
    .bind(json!(event.kamino_liquidity_mints))
    .bind(&event.universe_preset)
    .bind(&event.risk_profile)
    .bind(event.swap_lanes.clone())
    .bind(slot)
    .bind(&event.signature)
    .fetch_one(&mut **tx)
    .await
    .map_err(Into::into)
}

async fn upsert_managed_vault(
    tx: &mut Transaction<'_, Postgres>,
    policy_id: i64,
    event: &PolicyMatchInput,
) -> Result<ManagedVault, OrchestratorError> {
    sqlx::query_as::<_, ManagedVault>(
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
    .bind(policy_id)
    .fetch_one(&mut **tx)
    .await
    .map_err(Into::into)
}

async fn fetch_managed_vault_for_update(
    tx: &mut Transaction<'_, Postgres>,
    vault_id: i64,
) -> Result<ManagedVault, OrchestratorError> {
    sqlx::query_as::<_, ManagedVault>(
        "SELECT id, cluster, settings, vault_index, vault_pubkey, active_policy_id, active, first_seen_at, last_seen_at \
         FROM loyal_yield.managed_vaults WHERE id = $1 FOR UPDATE",
    )
    .bind(vault_id)
    .fetch_one(&mut **tx)
    .await
    .map_err(Into::into)
}

async fn fetch_snapshot_for_update(
    tx: &mut Transaction<'_, Postgres>,
    snapshot_id: i64,
) -> Result<PositionSnapshot, OrchestratorError> {
    sqlx::query_as::<_, PositionSnapshot>(
        "SELECT id, vault_id, policy_id, observed_slot, observed_at, chain_slot, lock_attempt_id, is_current, context \
         FROM loyal_yield.vault_position_snapshots WHERE id = $1 FOR UPDATE",
    )
    .bind(snapshot_id)
    .fetch_one(&mut **tx)
    .await
    .map_err(Into::into)
}

async fn fetch_snapshot_positions(
    tx: &mut Transaction<'_, Postgres>,
    snapshot_id: i64,
) -> Result<Vec<SnapshotPosition>, OrchestratorError> {
    sqlx::query_as::<_, SnapshotPosition>(
        "SELECT id, snapshot_id, reserve, market, liquidity_mint, amount_raw, supply_apy_bps, borrow_apy_bps, has_value, planning_metadata \
         FROM loyal_yield.vault_position_snapshot_positions WHERE snapshot_id = $1 ORDER BY reserve ASC",
    )
    .bind(snapshot_id)
    .fetch_all(&mut **tx)
    .await
    .map_err(Into::into)
}

async fn fetch_active_attempt(
    tx: &mut Transaction<'_, Postgres>,
    vault_id: i64,
) -> Result<Option<RebalanceAttempt>, OrchestratorError> {
    let mut builder = QueryBuilder::<Postgres>::new(
        "SELECT id, vault_id, source_snapshot_id, status, source_reserve, target_reserve, liquidity_mint, amount_raw, \
         source_apy_bps, target_apy_bps, estimated_edge_bps, estimated_cost_lamports, decision_reason, abandon_reason, \
         idempotency_key, signature, submitted_slot, confirmed_slot, preflight_chain_slot, post_snapshot_id, created_at, updated_at \
         FROM loyal_yield.rebalance_attempts WHERE vault_id = ",
    );
    builder.push_bind(vault_id).push(" AND status IN (");
    let mut separated = builder.separated(", ");
    for status in ACTIVE_ATTEMPT_STATUSES {
        separated.push_bind(status.as_str());
    }
    separated.push_unseparated(") ORDER BY created_at ASC LIMIT 1 FOR UPDATE");
    builder
        .build_query_as::<RebalanceAttempt>()
        .fetch_optional(&mut **tx)
        .await
        .map_err(Into::into)
}

async fn fetch_attempt_for_update(
    tx: &mut Transaction<'_, Postgres>,
    attempt_id: i64,
) -> Result<RebalanceAttempt, OrchestratorError> {
    sqlx::query_as::<_, RebalanceAttempt>(
        "SELECT id, vault_id, source_snapshot_id, status, source_reserve, target_reserve, liquidity_mint, amount_raw, \
         source_apy_bps, target_apy_bps, estimated_edge_bps, estimated_cost_lamports, decision_reason, abandon_reason, \
         idempotency_key, signature, submitted_slot, confirmed_slot, preflight_chain_slot, post_snapshot_id, created_at, updated_at \
         FROM loyal_yield.rebalance_attempts WHERE id = $1 FOR UPDATE",
    )
    .bind(attempt_id)
    .fetch_one(&mut **tx)
    .await
    .map_err(Into::into)
}

async fn update_attempt_status(
    tx: &mut Transaction<'_, Postgres>,
    attempt_id: i64,
    transition: &AttemptTransition,
) -> Result<RebalanceAttempt, OrchestratorError> {
    sqlx::query_as::<_, RebalanceAttempt>(
        "UPDATE loyal_yield.rebalance_attempts SET \
             status = $2, \
             signature = COALESCE($3, signature), \
             submitted_slot = COALESCE($4, submitted_slot), \
             confirmed_slot = COALESCE($5, confirmed_slot), \
             preflight_chain_slot = COALESCE($6, preflight_chain_slot), \
             post_snapshot_id = COALESCE($7, post_snapshot_id), \
             abandon_reason = COALESCE($8, abandon_reason), \
             updated_at = now() \
         WHERE id = $1 \
         RETURNING id, vault_id, source_snapshot_id, status, source_reserve, target_reserve, liquidity_mint, amount_raw, \
                   source_apy_bps, target_apy_bps, estimated_edge_bps, estimated_cost_lamports, decision_reason, abandon_reason, \
                   idempotency_key, signature, submitted_slot, confirmed_slot, preflight_chain_slot, post_snapshot_id, created_at, updated_at",
    )
    .bind(attempt_id)
    .bind(transition.to.as_str())
    .bind(&transition.signature)
    .bind(transition.submitted_slot)
    .bind(transition.confirmed_slot)
    .bind(transition.preflight_chain_slot)
    .bind(transition.post_snapshot_id)
    .bind(&transition.abandon_reason)
    .fetch_one(&mut **tx)
    .await
    .map_err(Into::into)
}

async fn insert_vault_event(
    tx: &mut Transaction<'_, Postgres>,
    attempt_id: Option<i64>,
    vault_id: i64,
    event_type: &str,
    from_status: Option<&str>,
    to_status: Option<&str>,
    reason: Option<&str>,
    payload: Value,
) -> Result<(), OrchestratorError> {
    sqlx::query(
        "INSERT INTO loyal_yield.rebalance_events \
         (attempt_id, vault_id, event_type, from_status, to_status, reason, payload) \
         VALUES ($1, $2, $3, $4, $5, $6, $7)",
    )
    .bind(attempt_id)
    .bind(vault_id)
    .bind(event_type)
    .bind(from_status)
    .bind(to_status)
    .bind(reason)
    .bind(payload)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

fn plan_same_mint_from_positions(
    positions: &[SnapshotPosition],
    config: PlannerConfig,
) -> Result<Option<RebalanceDecision>, OrchestratorError> {
    let mut source: Option<(&SnapshotPosition, u64)> = None;
    for position in positions {
        let amount = position.amount()?;
        if amount == 0 {
            continue;
        }
        if source.is_none_or(|(_, current_amount)| amount > current_amount) {
            source = Some((position, amount));
        }
    }
    let Some((source, amount)) = source else {
        return Ok(None);
    };

    let mut best_target: Option<&SnapshotPosition> = None;
    for target in positions {
        if target.reserve == source.reserve || target.liquidity_mint != source.liquidity_mint {
            continue;
        }
        let edge =
            target.supply_apy_bps.unwrap_or_default() - source.supply_apy_bps.unwrap_or_default();
        if edge < config.min_edge_bps {
            continue;
        }
        if best_target.is_none_or(|best| {
            target.supply_apy_bps.unwrap_or_default() > best.supply_apy_bps.unwrap_or_default()
        }) {
            best_target = Some(target);
        }
    }

    let Some(target) = best_target else {
        return Ok(None);
    };
    let estimated_edge_bps =
        Some(target.supply_apy_bps.unwrap_or_default() - source.supply_apy_bps.unwrap_or_default());
    Ok(Some(RebalanceDecision {
        source_reserve: source.reserve.clone(),
        target_reserve: target.reserve.clone(),
        liquidity_mint: source.liquidity_mint.clone(),
        amount_raw: amount,
        source_apy_bps: source.supply_apy_bps,
        target_apy_bps: target.supply_apy_bps,
        estimated_edge_bps,
    }))
}

fn rebalance_idempotency_key(
    vault_id: i64,
    snapshot_id: i64,
    decision: &RebalanceDecision,
) -> String {
    let mut hasher = Sha256::new();
    hasher.update(vault_id.to_le_bytes());
    hasher.update(snapshot_id.to_le_bytes());
    hasher.update(decision.source_reserve.as_bytes());
    hasher.update(decision.target_reserve.as_bytes());
    hasher.update(decision.liquidity_mint.as_bytes());
    hasher.update(decision.amount_raw.to_le_bytes());
    format!("{:x}", hasher.finalize())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn position(
        reserve: &str,
        mint: &str,
        amount_raw: u64,
        supply_apy_bps: i64,
    ) -> SnapshotPosition {
        SnapshotPosition {
            id: 0,
            snapshot_id: 1,
            reserve: reserve.to_owned(),
            market: None,
            liquidity_mint: mint.to_owned(),
            amount_raw: amount_raw.to_string(),
            supply_apy_bps: Some(supply_apy_bps),
            borrow_apy_bps: None,
            has_value: amount_raw > 0,
            planning_metadata: json!({}),
        }
    }

    #[test]
    fn planner_creates_same_mint_full_position_attempt() {
        let positions = vec![
            position("reserve-a", "usdc", 1_000, 450),
            position("reserve-b", "usdc", 0, 600),
            position("reserve-c", "pyusd", 0, 900),
        ];

        let decision = plan_same_mint_from_positions(
            &positions,
            PlannerConfig {
                min_edge_bps: 25,
                estimated_cost_lamports: 5_000,
            },
        )
        .unwrap()
        .unwrap();

        assert_eq!(decision.source_reserve, "reserve-a");
        assert_eq!(decision.target_reserve, "reserve-b");
        assert_eq!(decision.liquidity_mint, "usdc");
        assert_eq!(decision.amount_raw, 1_000);
        assert_eq!(decision.estimated_edge_bps, Some(150));
    }

    #[test]
    fn planner_skips_cross_mint_and_no_edge_targets() {
        let positions = vec![
            position("reserve-a", "usdc", 1_000, 450),
            position("reserve-b", "pyusd", 0, 900),
            position("reserve-c", "usdc", 0, 460),
        ];

        let decision = plan_same_mint_from_positions(
            &positions,
            PlannerConfig {
                min_edge_bps: 25,
                estimated_cost_lamports: 0,
            },
        )
        .unwrap();

        assert_eq!(decision, None);
    }

    #[test]
    fn state_machine_rejects_invalid_transitions() {
        let error = AttemptStatus::Planned
            .transition(AttemptAdvance::Submit {
                signature: "sig".to_owned(),
                slot: Some(10),
            })
            .unwrap_err();

        assert!(matches!(
            error,
            OrchestratorError::InvalidTransition {
                from: AttemptStatus::Planned,
                ..
            }
        ));
    }

    #[test]
    fn state_machine_allows_idempotent_terminal_repeats() {
        let transition = AttemptStatus::Confirmed
            .transition(AttemptAdvance::Confirm {
                slot: Some(12),
                post_snapshot_id: Some(99),
            })
            .unwrap();

        assert!(transition.idempotent);
        assert_eq!(transition.to, AttemptStatus::Confirmed);
    }

    #[test]
    fn migration_enforces_one_active_attempt_per_vault() {
        assert!(MIGRATION_0001.contains("rebalance_attempts_one_active_per_vault_idx"));
        assert!(MIGRATION_0001.contains(
            "WHERE status IN ('planned', 'simulating', 'ready', 'submitted', 'confirming')"
        ));
    }
}
