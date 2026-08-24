use crate::domain::{
    draft_same_mint_decision, route_amount_evidence, state_transition,
    supported_idle_deposit_mints, PlannedDecision, AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
    MAX_QUEUE_POSITIVE_AMOUNT_DRIFT_PPM, ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
};
use crate::fleet_orchestration::{
    project_frontend, MultiplyRouteState, RebalanceOpportunityLease, RebalanceOpportunityRecord,
    SignedRouteSubmissionInput, SignedRouteSubmissionRecord, TargetCapacityReservationInput,
    MULTIPLY_ENGINE_VERSION,
};
use crate::types::*;
use crate::{OrchestratorError, ACTIVE_DECISION_STATUSES};
use chrono::{DateTime, Utc};
use log::LevelFilter;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use sqlx::postgres::PgPoolOptions;
use sqlx::{PgConnection, PgPool, Postgres, Row, Transaction};
use std::{collections::BTreeSet, future::Future};

const MIGRATION_0001: &str = include_str!("../migrations/0001_loyal_yield_orchestration.sql");
const MIGRATION_0002: &str = include_str!("../migrations/0002_balance_sweep_surplus_lots.sql");
const MIGRATION_0003: &str = include_str!("../migrations/0003_balance_sweep_initial_surplus.sql");
const MIGRATION_0004: &str = include_str!("../migrations/0004_managed_vault_setup_policy.sql");
const MIGRATION_0005: &str =
    include_str!("../migrations/0005_add_unsupported_amount_semantics.sql");
const MIGRATION_0006: &str =
    include_str!("../migrations/0006_generic_balance_sweep_token_accounts.sql");
const MIGRATION_0007: &str = include_str!("../migrations/0007_balance_sweep_scheduled_slots.sql");
const MIGRATION_0008: &str = include_str!("../migrations/0008_route_lookup_tables.sql");
const MIGRATION_0009: &str = include_str!("../migrations/0009_idle_vault_routing.sql");
const MIGRATION_0010: &str = include_str!("../migrations/0010_realtime_events.sql");
const MIGRATION_0011: &str = include_str!("../migrations/0011_autodeposit_realtime_events.sql");
const MIGRATION_0012: &str =
    include_str!("../migrations/0012_idle_vault_decision_plan_guardrails.sql");
const MIGRATION_0035: &str = include_str!("../migrations/0035_durable_cross_mint_movements.sql");
const MIGRATION_0036: &str = include_str!("../migrations/0036_cross_mint_swap_policies.sql");
const MIGRATION_0037: &str = include_str!("../migrations/0037_cross_mint_vault_opt_ins.sql");
const MIGRATION_0038: &str =
    include_str!("../migrations/0038_durable_autodeposit_confirmation.sql");
const MIGRATION_0039: &str =
    include_str!("../migrations/0039_unbroadcast_cross_mint_expiry_check.sql");
const MIGRATION_0040: &str = include_str!("../migrations/0040_durable_autodeposit_operation.sql");
const MIGRATION_0046: &str = include_str!("../migrations/0046_laserstream_replay_cursor.sql");
const MIGRATION_0049: &str =
    include_str!("../migrations/0049_durable_earn_reconciliation_jobs.sql");
const MIGRATION_0050: &str = include_str!("../migrations/0050_autoswap_opt_in_realtime.sql");
const MIGRATION_0051: &str = include_str!("../migrations/0051_multiply_route_state.sql");
const MIGRATION_0052: &str = include_str!("../migrations/0052_voltr_opportunity_classes.sql");
const MIGRATION_0053: &str = include_str!("../migrations/0053_multiply_production_engine.sql");
const MIGRATION_0054: &str = include_str!("../migrations/0054_earn_max_per_user.sql");
const MIGRATION_0055: &str = include_str!("../migrations/0055_earn_max_repeated_lifecycle.sql");
const MIGRATION_0056: &str = include_str!("../migrations/0056_earn_max_dynamic_policy_seeds.sql");
const MIGRATION_0057: &str = include_str!("../migrations/0057_autodeposit_client_projection.sql");
const MIGRATION_0058: &str =
    include_str!("../migrations/0058_autoswap_confirmed_reconciliation.sql");
const MIGRATION_0059: &str = include_str!("../migrations/0059_autodeposit_single_target_state.sql");
const MIGRATION_0060: &str =
    include_str!("../migrations/0060_rebalance_confirmation_target_state.sql");
const MIGRATION_0061: &str =
    include_str!("../migrations/0061_coalesced_autodeposit_reconciliation.sql");
const MIGRATION_0062: &str = include_str!("../migrations/0062_earn_chain_cash_flow_projection.sql");
const MIGRATION_0063: &str = include_str!("../migrations/0063_earn_max_external_operations.sql");
const MIGRATION_0064: &str = include_str!("../migrations/0064_earn_max_partial_lifecycle.sql");
const LIVE_MIGRATION_0008_CHECKSUM: &str =
    "d20151ef6d6076961195da6c6cf3b4e11bb3e2045f729bdf4b118f6c7d3ddc34";
const SAME_MINT_CHAIN_RECONCILE_PREVIEW_KIND: &str = "same_mint_chain_reconcile_preview";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum VaultPublicationScope {
    /// A bounded post-confirm/read repair. It may upsert what was observed but
    /// cannot erase unseen reserves, erase unseen idle mints, or close rows.
    ObservedSubset,
    /// One RPC epoch containing every policy reserve plus all six product
    /// idle ATAs. Only this scope is allowed to replace current vault state.
    CompleteProductVault,
}

#[derive(Clone)]
pub struct NeonSqlClient {
    pool: PgPool,
}

pub type OrchestratorStore = NeonSqlClient;

pub struct RouteLookupTableProvisioningLock {
    tx: Option<Transaction<'static, Postgres>>,
    key: String,
}

impl RouteLookupTableProvisioningLock {
    pub fn key(&self) -> &str {
        &self.key
    }

    pub async fn release(mut self) -> Result<(), OrchestratorError> {
        if let Some(tx) = self.tx.take() {
            tx.commit().await?;
        }
        Ok(())
    }
}

#[derive(Debug, sqlx::FromRow)]
struct RoutePolicyRow {
    id: i64,
    cluster: String,
    source_commitment: String,
    finalized_eligible: bool,
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
struct CrossMintSwapPolicyRow {
    id: i64,
    cluster: String,
    settings: String,
    authority: String,
    policy_seed: Option<i64>,
    policy_account: String,
    vault_index: Option<i16>,
    vault_pubkey: Option<String>,
    delegated_signer: Option<String>,
    source_shard: Option<String>,
    max_slippage_bps: Option<i32>,
    daily_source_mint_spending_cap: Option<i64>,
    manifest_fingerprint: Option<String>,
    active: bool,
    start_eligible: bool,
    last_mutation: String,
    source_commitment: String,
    first_seen_at: DateTime<Utc>,
    last_seen_at: DateTime<Utc>,
    last_seen_slot: i64,
    last_seen_signature: String,
}

#[derive(Debug, sqlx::FromRow)]
struct CrossMintVaultOptInRow {
    cluster: String,
    settings: String,
    vault_index: i16,
    vault_pubkey: String,
    enabled: bool,
    generation: i64,
    created_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
}

#[derive(Debug, sqlx::FromRow)]
struct ManagedVaultRow {
    id: i64,
    settings: String,
    vault_index: i16,
    vault_pubkey: String,
    active_policy_id: i64,
    active: bool,
    first_seen_at: DateTime<Utc>,
    last_seen_at: DateTime<Utc>,
}

#[derive(Debug, sqlx::FromRow)]
struct ExistingEarnPositionProjectionRow {
    id: i64,
    current_reserve: String,
    current_market: Option<String>,
    current_liquidity_mint: String,
    current_amount_raw: i64,
    current_observed_slot: i64,
    current_observed_at: DateTime<Utc>,
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
            .acquire_time_level(LevelFilter::Debug)
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

    pub async fn require_schema_migration(
        &self,
        version: i64,
        name: &str,
    ) -> Result<(), OrchestratorError> {
        let ledger_exists: bool =
            sqlx::query_scalar("SELECT to_regclass('loyal_yield.schema_migrations') IS NOT NULL")
                .fetch_one(&self.pool)
                .await?;
        if !ledger_exists {
            return Err(OrchestratorError::StoreInvariant(format!(
                "database schema is not initialized; run the dedicated migration command before starting this process (required migration {version} {name})"
            )));
        }

        let applied: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.schema_migrations
                WHERE version = $1
                  AND name = $2
            )
            "#,
        )
        .bind(version)
        .bind(name)
        .fetch_one(&self.pool)
        .await?;
        if !applied {
            return Err(OrchestratorError::StoreInvariant(format!(
                "required database migration {version} {name} is not applied; run the dedicated migration command before starting this process"
            )));
        }

        Ok(())
    }

    pub async fn schema_migration_applied(
        &self,
        version: i64,
        name: &str,
    ) -> Result<bool, OrchestratorError> {
        let ledger_exists: bool =
            sqlx::query_scalar("SELECT to_regclass('loyal_yield.schema_migrations') IS NOT NULL")
                .fetch_one(&self.pool)
                .await?;
        if !ledger_exists {
            return Ok(false);
        }
        Ok(sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.schema_migrations
                WHERE version = $1 AND name = $2
            )
            "#,
        )
        .bind(version)
        .bind(name)
        .fetch_one(&self.pool)
        .await?)
    }

    pub async fn acquire_route_lookup_table_provisioning_lock(
        &self,
        cluster: &str,
        scope: &str,
        authority: &str,
    ) -> Result<RouteLookupTableProvisioningLock, OrchestratorError> {
        let key = route_lookup_table_lock_key(cluster, scope, authority);
        let mut tx = self.pool.begin().await?;
        sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0::bigint))")
            .bind(&key)
            .execute(&mut *tx)
            .await?;
        Ok(RouteLookupTableProvisioningLock { tx: Some(tx), key })
    }

    pub async fn apply_migrations(&self) -> Result<(), OrchestratorError> {
        ensure_schema_migration_ledger(&self.pool).await?;

        for migration in [
            StoreMigration {
                version: 1,
                name: "loyal_yield_orchestration",
                sql: MIGRATION_0001,
                expected_checksum: None,
            },
            StoreMigration {
                version: 2,
                name: "balance_sweep_surplus_lots",
                sql: MIGRATION_0002,
                expected_checksum: None,
            },
            StoreMigration {
                version: 3,
                name: "balance_sweep_initial_surplus",
                sql: MIGRATION_0003,
                expected_checksum: None,
            },
            StoreMigration {
                version: 4,
                name: "managed_vault_setup_policy",
                sql: MIGRATION_0004,
                expected_checksum: None,
            },
            StoreMigration {
                version: 5,
                name: "add_unsupported_amount_semantics",
                sql: MIGRATION_0005,
                expected_checksum: None,
            },
            StoreMigration {
                version: 6,
                name: "generic_balance_sweep_token_accounts",
                sql: MIGRATION_0006,
                expected_checksum: None,
            },
            StoreMigration {
                version: 7,
                name: "balance_sweep_scheduled_slots",
                sql: MIGRATION_0007,
                expected_checksum: None,
            },
            StoreMigration {
                version: 8,
                name: "route_lookup_tables",
                sql: MIGRATION_0008,
                expected_checksum: Some(LIVE_MIGRATION_0008_CHECKSUM),
            },
            StoreMigration {
                version: 9,
                name: "idle_vault_routing",
                sql: MIGRATION_0009,
                expected_checksum: None,
            },
            StoreMigration {
                version: 10,
                name: "realtime_events",
                sql: MIGRATION_0010,
                expected_checksum: None,
            },
            StoreMigration {
                version: 11,
                name: "autodeposit_realtime_events",
                sql: MIGRATION_0011,
                expected_checksum: None,
            },
            StoreMigration {
                version: 12,
                name: "idle_vault_decision_plan_guardrails",
                sql: MIGRATION_0012,
                expected_checksum: None,
            },
            StoreMigration {
                version: 35,
                name: "durable_cross_mint_movements",
                sql: MIGRATION_0035,
                expected_checksum: None,
            },
            StoreMigration {
                version: 36,
                name: "cross_mint_swap_policies",
                sql: MIGRATION_0036,
                expected_checksum: None,
            },
            StoreMigration {
                version: 37,
                name: "cross_mint_vault_opt_ins",
                sql: MIGRATION_0037,
                expected_checksum: None,
            },
            StoreMigration {
                version: 38,
                name: "durable_autodeposit_confirmation",
                sql: MIGRATION_0038,
                expected_checksum: None,
            },
            StoreMigration {
                version: 39,
                name: "unbroadcast_cross_mint_expiry_check",
                sql: MIGRATION_0039,
                expected_checksum: None,
            },
            StoreMigration {
                version: 40,
                name: "durable_autodeposit_operation",
                sql: MIGRATION_0040,
                expected_checksum: None,
            },
            StoreMigration {
                version: 46,
                name: "laserstream_replay_cursor",
                sql: MIGRATION_0046,
                expected_checksum: None,
            },
            StoreMigration {
                version: 49,
                name: "durable_earn_reconciliation_jobs",
                sql: MIGRATION_0049,
                expected_checksum: None,
            },
            StoreMigration {
                version: 50,
                name: "autoswap_opt_in_realtime",
                sql: MIGRATION_0050,
                expected_checksum: None,
            },
            StoreMigration {
                version: 51,
                name: "multiply_route_state",
                sql: MIGRATION_0051,
                expected_checksum: None,
            },
            StoreMigration {
                version: 52,
                name: "voltr_opportunity_classes",
                sql: MIGRATION_0052,
                expected_checksum: None,
            },
            StoreMigration {
                version: 53,
                name: "multiply_production_engine",
                sql: MIGRATION_0053,
                expected_checksum: None,
            },
            StoreMigration {
                version: 54,
                name: "earn_max_per_user",
                sql: MIGRATION_0054,
                expected_checksum: None,
            },
            StoreMigration {
                version: 55,
                name: "earn_max_repeated_lifecycle",
                sql: MIGRATION_0055,
                expected_checksum: None,
            },
            StoreMigration {
                version: 56,
                name: "earn_max_dynamic_policy_seeds",
                sql: MIGRATION_0056,
                expected_checksum: None,
            },
            StoreMigration {
                version: 57,
                name: "autodeposit_client_projection",
                sql: MIGRATION_0057,
                expected_checksum: None,
            },
            StoreMigration {
                version: 58,
                name: "autoswap_confirmed_reconciliation",
                sql: MIGRATION_0058,
                expected_checksum: None,
            },
            StoreMigration {
                version: 59,
                name: "autodeposit_single_target_state",
                sql: MIGRATION_0059,
                expected_checksum: None,
            },
            StoreMigration {
                version: 60,
                name: "rebalance_confirmation_target_state",
                sql: MIGRATION_0060,
                expected_checksum: None,
            },
            StoreMigration {
                version: 61,
                name: "coalesced_autodeposit_reconciliation",
                sql: MIGRATION_0061,
                expected_checksum: None,
            },
            StoreMigration {
                version: 62,
                name: "earn_chain_cash_flow_projection",
                sql: MIGRATION_0062,
                expected_checksum: None,
            },
            StoreMigration {
                version: 63,
                name: "earn_max_external_operations",
                sql: MIGRATION_0063,
                expected_checksum: None,
            },
            StoreMigration {
                version: 64,
                name: "earn_max_partial_lifecycle",
                sql: MIGRATION_0064,
                expected_checksum: None,
            },
        ] {
            apply_store_migration(&self.pool, migration).await?;
        }
        Ok(())
    }

    /// Makes an Earn stream event durable before acknowledging its slot.
    pub async fn enqueue_earn_reconciliation_jobs(
        &self,
        input: EarnReconciliationEnqueueInput,
    ) -> Result<EarnReconciliationEnqueueOutcome, OrchestratorError> {
        if input.vaults.is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "Earn reconciliation event has no affected vaults".to_owned(),
            ));
        }
        let durable_slot = to_i64_slot(input.durable_slot)?;
        let mut tx = self.pool.begin().await?;
        let mut inserted_jobs = 0_usize;
        for vault in &input.vaults {
            let inserted = sqlx::query(
                r#"
                INSERT INTO loyal_yield.earn_reconciliation_jobs (
                    consumer_name, event_key, durable_slot, settings,
                    vault_index, vault_pubkey, event_payload, vault_payload
                ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
                ON CONFLICT (consumer_name, event_key, settings, vault_index, vault_pubkey)
                DO NOTHING
                "#,
            )
            .bind(&input.consumer_name)
            .bind(&input.event_key)
            .bind(durable_slot)
            .bind(&vault.settings)
            .bind(i16::from(vault.vault_index))
            .bind(&vault.vault_pubkey)
            .bind(&input.event_payload)
            .bind(&vault.vault_payload)
            .execute(&mut *tx)
            .await?;
            inserted_jobs += usize::try_from(inserted.rows_affected()).unwrap_or(usize::MAX);
        }

        let mut coalesced_autodeposit_requests = 0_usize;
        for target_id in &input.autodeposit_target_ids {
            let changed =
                upsert_autodeposit_reconciliation_request(&mut tx, *target_id, durable_slot)
                    .await?;
            coalesced_autodeposit_requests += usize::from(changed);
        }

        sqlx::query(
            r#"
            INSERT INTO loyal_yield.laserstream_replay_cursors (consumer_name, durable_slot)
            VALUES ($1, $2)
            ON CONFLICT (consumer_name) DO UPDATE SET
                durable_slot = GREATEST(
                    loyal_yield.laserstream_replay_cursors.durable_slot,
                    EXCLUDED.durable_slot
                ),
                updated_at = NOW()
            "#,
        )
        .bind(&input.consumer_name)
        .bind(durable_slot)
        .execute(&mut *tx)
        .await?;

        tx.commit().await?;
        Ok(EarnReconciliationEnqueueOutcome {
            inserted_jobs,
            coalesced_autodeposit_requests,
            cursor_slot: input.durable_slot,
        })
    }

    pub async fn enqueue_autodeposit_reconciliation_request(
        &self,
        target_id: BalanceSweepTargetId,
        requested_slot: u64,
    ) -> Result<bool, OrchestratorError> {
        let requested_slot = to_i64_slot(requested_slot)?;
        let mut tx = self.pool.begin().await?;
        let changed =
            upsert_autodeposit_reconciliation_request(&mut tx, target_id, requested_slot).await?;
        tx.commit().await?;
        Ok(changed)
    }

    pub async fn claim_autodeposit_reconciliation_request(
        &self,
        claim_owner: &str,
        lease_seconds: i64,
    ) -> Result<Option<AutodepositReconciliationRequest>, OrchestratorError> {
        let row = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT target_id
                FROM loyal_yield.autodeposit_reconciliation_requests
                WHERE processed_slot < requested_slot
                  AND next_attempt_at <= NOW()
                  AND (claim_expires_at IS NULL OR claim_expires_at <= NOW())
                ORDER BY requested_slot, target_id
                FOR UPDATE SKIP LOCKED
                LIMIT 1
            )
            UPDATE loyal_yield.autodeposit_reconciliation_requests request
            SET claim_owner = $1,
                claim_expires_at = NOW() + make_interval(secs => $2::double precision),
                attempt_count = attempt_count + 1,
                updated_at = NOW()
            FROM candidate
            WHERE request.target_id = candidate.target_id
            RETURNING request.target_id, request.requested_slot, request.attempt_count
            "#,
        )
        .bind(claim_owner)
        .bind(lease_seconds)
        .fetch_optional(&self.pool)
        .await?;
        row.map(|row| {
            Ok(AutodepositReconciliationRequest {
                target_id: BalanceSweepTargetId(row.get("target_id")),
                requested_slot: nonnegative_i64_to_u64(
                    row.get("requested_slot"),
                    "Autodeposit requested slot",
                )?,
                attempt_count: row.get("attempt_count"),
            })
        })
        .transpose()
    }

    pub async fn complete_autodeposit_reconciliation_request(
        &self,
        target_id: BalanceSweepTargetId,
        claim_owner: &str,
        processed_slot: u64,
    ) -> Result<bool, OrchestratorError> {
        let processed_slot = to_i64_slot(processed_slot)?;
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.autodeposit_reconciliation_requests
            SET requested_slot = GREATEST(requested_slot, $3),
                processed_slot = GREATEST(processed_slot, $3),
                attempt_count = 0,
                claim_owner = NULL,
                claim_expires_at = NULL,
                last_error = NULL,
                updated_at = NOW()
            WHERE target_id = $1
              AND claim_owner = $2
              AND claim_expires_at > NOW()
            RETURNING processed_slot < requested_slot AS still_pending
            "#,
        )
        .bind(target_id.as_i64())
        .bind(claim_owner)
        .bind(processed_slot)
        .fetch_optional(&self.pool)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "Autodeposit reconciliation request {target_id} lost claim before completion"
            ))
        })?;
        Ok(row.get("still_pending"))
    }

    pub async fn retry_autodeposit_reconciliation_request(
        &self,
        target_id: BalanceSweepTargetId,
        claim_owner: &str,
        error: &str,
        retry_after_seconds: i64,
    ) -> Result<(), OrchestratorError> {
        let updated = sqlx::query(
            r#"
            UPDATE loyal_yield.autodeposit_reconciliation_requests
            SET claim_owner = NULL,
                claim_expires_at = NULL,
                next_attempt_at = NOW() + make_interval(secs => $3::double precision),
                last_error = $4,
                updated_at = NOW()
            WHERE target_id = $1
              AND claim_owner = $2
              AND claim_expires_at > NOW()
            "#,
        )
        .bind(target_id.as_i64())
        .bind(claim_owner)
        .bind(retry_after_seconds)
        .bind(error)
        .execute(&self.pool)
        .await?;
        if updated.rows_affected() != 1 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "Autodeposit reconciliation request {target_id} lost claim before retry"
            )));
        }
        Ok(())
    }

    pub async fn claim_earn_reconciliation_job(
        &self,
        consumer_name: &str,
        claim_owner: &str,
        lease_seconds: i64,
    ) -> Result<Option<EarnReconciliationJob>, OrchestratorError> {
        let row = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT id
                FROM loyal_yield.earn_reconciliation_jobs
                WHERE consumer_name = $1
                  AND completed_at IS NULL
                  AND next_attempt_at <= NOW()
                  AND (claim_expires_at IS NULL OR claim_expires_at <= NOW())
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.earn_reconciliation_jobs earlier
                      WHERE earlier.consumer_name = earn_reconciliation_jobs.consumer_name
                        AND earlier.settings = earn_reconciliation_jobs.settings
                        AND earlier.vault_index = earn_reconciliation_jobs.vault_index
                        AND earlier.vault_pubkey = earn_reconciliation_jobs.vault_pubkey
                        AND earlier.completed_at IS NULL
                        AND (earlier.durable_slot, earlier.id)
                            < (earn_reconciliation_jobs.durable_slot, earn_reconciliation_jobs.id)
                        AND NOT (
                            earlier.durable_slot = earn_reconciliation_jobs.durable_slot
                            AND earlier.event_payload->>'signature'
                                = earn_reconciliation_jobs.event_payload->>'signature'
                            AND earlier.next_attempt_at > NOW()
                            AND earlier.claim_owner IS NULL
                            AND earlier.claim_expires_at IS NULL
                        )
                  )
                ORDER BY durable_slot, id
                FOR UPDATE SKIP LOCKED
                LIMIT 1
            )
            UPDATE loyal_yield.earn_reconciliation_jobs job
            SET claim_owner = $2,
                claim_expires_at = NOW() + make_interval(secs => $3::double precision),
                attempt_count = attempt_count + 1,
                updated_at = NOW()
            FROM candidate
            WHERE job.id = candidate.id
            RETURNING job.id, job.consumer_name, job.event_key, job.durable_slot,
                      job.event_payload, job.vault_payload, job.attempt_count
            "#,
        )
        .bind(consumer_name)
        .bind(claim_owner)
        .bind(lease_seconds)
        .fetch_optional(&self.pool)
        .await?;
        row.map(|row| {
            let durable_slot = u64::try_from(row.get::<i64, _>("durable_slot")).map_err(|_| {
                OrchestratorError::StoreInvariant("Earn job has negative durable slot".to_owned())
            })?;
            Ok(EarnReconciliationJob {
                id: row.get("id"),
                consumer_name: row.get("consumer_name"),
                event_key: row.get("event_key"),
                durable_slot,
                event_payload: row.get("event_payload"),
                vault_payload: row.get("vault_payload"),
                attempt_count: row.get("attempt_count"),
            })
        })
        .transpose()
    }

    pub async fn retry_earn_reconciliation_job(
        &self,
        job_id: i64,
        claim_owner: &str,
        error: &str,
        retry_after_seconds: i64,
    ) -> Result<(), OrchestratorError> {
        let updated = sqlx::query(
            r#"
            UPDATE loyal_yield.earn_reconciliation_jobs
            SET claim_owner = NULL,
                claim_expires_at = NULL,
                next_attempt_at = NOW() + make_interval(secs => $3::double precision),
                last_error = $4,
                updated_at = NOW()
            WHERE id = $1 AND claim_owner = $2 AND completed_at IS NULL
            "#,
        )
        .bind(job_id)
        .bind(claim_owner)
        .bind(retry_after_seconds)
        .bind(error)
        .execute(&self.pool)
        .await?;
        if updated.rows_affected() != 1 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "Earn reconciliation job {job_id} lost claim before retry"
            )));
        }
        Ok(())
    }

    pub async fn complete_earn_reconciliation_job(
        &self,
        job_id: i64,
        claim_owner: &str,
        mutation: &EarnDirectMutation,
    ) -> Result<EarnReconciliationCompletionOutcome, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let completed = sqlx::query(
            r#"
            UPDATE loyal_yield.earn_reconciliation_jobs
            SET completed_at = NOW(), claim_owner = NULL, claim_expires_at = NULL,
                last_error = NULL, updated_at = NOW()
            WHERE id = $1 AND claim_owner = $2 AND completed_at IS NULL
              AND claim_expires_at > NOW()
            "#,
        )
        .bind(job_id)
        .bind(claim_owner)
        .execute(&mut *tx)
        .await?;
        if completed.rows_affected() != 1 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "Earn reconciliation job {job_id} lost claim before completion"
            )));
        }
        let identity = match mutation {
            EarnDirectMutation::Deposit(value) => Some((
                "deposit",
                value.deposit_signature.as_str(),
                value.route_policy.settings.as_str(),
                value.route_policy.vault_index,
                value.route_policy.vault_pubkey.as_str(),
                value.deposit_slot,
            )),
            EarnDirectMutation::Withdrawal(value) => Some((
                "withdrawal",
                value.withdrawal_signature.as_str(),
                value.route_policy.settings.as_str(),
                value.route_policy.vault_index,
                value.vault_pubkey.as_str(),
                value.confirmed_slot,
            )),
            EarnDirectMutation::Cleanup(value) => Some((
                "cleanup",
                value.cleanup_signature.as_str(),
                value.settings.as_str(),
                value.vault_index,
                value.vault_pubkey.as_str(),
                value.confirmed_slot,
            )),
            EarnDirectMutation::Refund(value) => Some((
                "refund",
                value.refund_signature.as_str(),
                value.settings.as_str(),
                value.vault_index,
                value.vault_pubkey.as_str(),
                value.confirmed_slot,
            )),
            EarnDirectMutation::PolicyOnly(_) | EarnDirectMutation::Noop => None,
        };
        if let Some((kind, signature, settings, vault_index, vault_pubkey, slot)) = identity {
            let claimed = sqlx::query(
                r#"
                INSERT INTO loyal_yield.earn_chain_mutations (
                    mutation_kind, chain_signature, settings, vault_index,
                    vault_pubkey, confirmed_slot, created_at
                ) VALUES ($1, $2, $3, $4, $5, $6, now())
                ON CONFLICT (mutation_kind, chain_signature, vault_pubkey) DO NOTHING
                "#,
            )
            .bind(kind)
            .bind(signature)
            .bind(settings)
            .bind(i16::from(vault_index))
            .bind(vault_pubkey)
            .bind(to_i64_slot(slot)?)
            .execute(&mut *tx)
            .await?;
            if claimed.rows_affected() == 0 {
                tx.commit().await?;
                return Ok(EarnReconciliationCompletionOutcome {
                    applied_mutations: 0,
                });
            }
        }
        let applied_mutations = match mutation {
            EarnDirectMutation::PolicyOnly(policy) => {
                apply_earn_policy_only(&mut tx, policy).await?;
                1
            }
            EarnDirectMutation::Deposit(deposit) => {
                apply_earn_deposit(&mut tx, deposit).await?;
                1
            }
            EarnDirectMutation::Withdrawal(withdrawal) => {
                apply_earn_withdrawal(&mut tx, withdrawal).await?;
                1
            }
            EarnDirectMutation::Cleanup(cleanup) => {
                apply_earn_cleanup(&mut tx, cleanup).await?;
                1
            }
            EarnDirectMutation::Refund(refund) => {
                apply_earn_refund(&mut tx, refund).await?;
                if refund.full_cleanup {
                    apply_earn_cleanup(
                        &mut tx,
                        &EarnCleanupMutation {
                            settings: refund.settings.clone(),
                            vault_index: refund.vault_index,
                            vault_pubkey: refund.vault_pubkey.clone(),
                            cleanup_signature: refund.refund_signature.clone(),
                            confirmed_slot: refund.confirmed_slot,
                            observed_at: refund.observed_at,
                        },
                    )
                    .await?;
                }
                1
            }
            EarnDirectMutation::Noop => 0,
        };
        tx.commit().await?;
        Ok(EarnReconciliationCompletionOutcome { applied_mutations })
    }

    pub async fn load_laserstream_replay_cursor(
        &self,
        consumer_name: &str,
    ) -> Result<Option<u64>, OrchestratorError> {
        let slot = sqlx::query_scalar::<_, i64>(
            r#"
            SELECT durable_slot
            FROM loyal_yield.laserstream_replay_cursors
            WHERE consumer_name = $1
            "#,
        )
        .bind(consumer_name)
        .fetch_optional(&self.pool)
        .await?;
        slot.map(|slot| {
            u64::try_from(slot).map_err(|_| {
                OrchestratorError::StoreInvariant(format!(
                    "LaserStream replay cursor {slot} is negative"
                ))
            })
        })
        .transpose()
    }

    /// Loads one restart-safe health snapshot from committed durable state.
    pub async fn load_earn_reconciliation_health_snapshot(
        &self,
        consumer_name: &str,
    ) -> Result<EarnReconciliationHealthSnapshot, OrchestratorError> {
        let row = sqlx::query(
            r#"
            SELECT COALESCE(
                       (
                           SELECT cursor.durable_slot
                           FROM loyal_yield.laserstream_replay_cursors cursor
                           WHERE cursor.consumer_name = $1
                       ),
                       0
                   )::BIGINT AS cursor_slot,
                   COUNT(*)::BIGINT AS pending_jobs,
                   COUNT(*) FILTER (WHERE job.last_error IS NOT NULL)::BIGINT
                       AS failed_pending_jobs,
                   COALESCE(
                       GREATEST(
                           0,
                           FLOOR(EXTRACT(EPOCH FROM (
                               NOW() - MIN(job.created_at)
                           )))::BIGINT
                       ),
                       0
                   ) AS oldest_pending_age_seconds
            FROM loyal_yield.earn_reconciliation_jobs job
            WHERE job.consumer_name = $1
              AND job.completed_at IS NULL
            "#,
        )
        .bind(consumer_name)
        .fetch_one(&self.pool)
        .await?;

        Ok(EarnReconciliationHealthSnapshot {
            cursor_slot: nonnegative_i64_to_u64(row.get("cursor_slot"), "cursor_slot")?,
            pending_jobs: nonnegative_i64_to_u64(row.get("pending_jobs"), "pending_jobs")?,
            failed_pending_jobs: nonnegative_i64_to_u64(
                row.get("failed_pending_jobs"),
                "failed_pending_jobs",
            )?,
            oldest_pending_age_seconds: nonnegative_i64_to_u64(
                row.get("oldest_pending_age_seconds"),
                "oldest_pending_age_seconds",
            )?,
        })
    }

    /// Load only identity and already-recorded policy/market metadata. Address
    /// derivation is deliberately performed by the monitor from this compact
    /// snapshot rather than persisted in a second catalog.
    pub async fn load_earn_subscription_targets(
        &self,
        environment: &str,
    ) -> Result<Vec<EarnSubscriptionTarget>, OrchestratorError> {
        let mut targets = Vec::new();
        let mut environment_settings = BTreeSet::new();

        let app_accounts_exist: bool = sqlx::query_scalar(
            "SELECT to_regclass('app_user_smart_accounts') IS NOT NULL AND to_regclass('app_users') IS NOT NULL",
        )
        .fetch_one(&self.pool)
        .await?;
        if app_accounts_exist {
            let rows = sqlx::query(
                r#"
                SELECT smart.solana_env AS environment,
                       smart.settings_pda AS settings,
                       smart.state,
                       app.subject_address AS wallet
                FROM app_user_smart_accounts smart
                JOIN app_users app ON app.id = smart.user_id
                WHERE smart.solana_env = $1
                "#,
            )
            .bind(environment)
            .fetch_all(&self.pool)
            .await?;
            for row in rows {
                let settings: String = row.get("settings");
                environment_settings.insert(settings.clone());
                if row.get::<String, _>("state") == "ready" {
                    targets.push(EarnSubscriptionTarget {
                        environment: row.get("environment"),
                        settings,
                        wallet: row.get("wallet"),
                        vault_index: 1,
                        vault_pubkey: None,
                        policy_accounts: Vec::new(),
                        markets: Vec::new(),
                        autodeposit_accounts: Vec::new(),
                        observation_start_slot: None,
                    });
                }
            }
        }

        let onboarding_exists: bool = sqlx::query_scalar(
            "SELECT to_regclass('loyal_yield.earn_deposit_onboarding_attempts') IS NOT NULL",
        )
        .fetch_one(&self.pool)
        .await?;
        if onboarding_exists {
            let rows = sqlx::query(
                r#"
                SELECT wallet_address AS wallet, settings, vault_index,
                       vault_pubkey, policy_account, setup_policy_account,
                       market
                FROM loyal_yield.earn_deposit_onboarding_attempts
                WHERE status <> 'complete'
                "#,
            )
            .fetch_all(&self.pool)
            .await?;
            for row in rows {
                let settings: String = row.get("settings");
                if app_accounts_exist && !environment_settings.contains(&settings) {
                    continue;
                }
                targets.push(EarnSubscriptionTarget {
                    environment: environment.to_owned(),
                    settings,
                    wallet: row.get("wallet"),
                    vault_index: row.get("vault_index"),
                    vault_pubkey: Some(row.get("vault_pubkey")),
                    policy_accounts: [
                        row.try_get::<Option<String>, _>("policy_account")?,
                        row.try_get::<Option<String>, _>("setup_policy_account")?,
                    ]
                    .into_iter()
                    .flatten()
                    .collect(),
                    markets: row
                        .try_get::<Option<String>, _>("market")?
                        .into_iter()
                        .collect(),
                    autodeposit_accounts: Vec::new(),
                    observation_start_slot: None,
                });
            }
        }

        let positions_exist: bool = sqlx::query_scalar(
            "SELECT to_regclass('loyal_yield.user_yield_positions') IS NOT NULL",
        )
        .fetch_one(&self.pool)
        .await?;
        if positions_exist {
            let rows = sqlx::query(
                r#"
                SELECT position.wallet_address AS wallet,
                       position.settings,
                       position.vault_index,
                       position.vault_pubkey,
                       position.policy_account,
                       position.current_market AS market,
                       active_policy.policy_account AS active_policy_account,
                       setup_policy.policy_account AS setup_policy_account
                FROM loyal_yield.user_yield_positions position
                LEFT JOIN loyal_yield.managed_vaults vault
                  ON vault.settings = position.settings
                 AND vault.vault_index = position.vault_index
                 AND vault.vault_pubkey = position.vault_pubkey
                LEFT JOIN loyal_yield.route_policies active_policy
                  ON active_policy.id = vault.active_policy_id
                LEFT JOIN loyal_yield.route_policies setup_policy
                  ON setup_policy.id = vault.setup_policy_id
                WHERE position.status = 'active'
                "#,
            )
            .fetch_all(&self.pool)
            .await?;
            for row in rows {
                let settings: String = row.get("settings");
                if app_accounts_exist && !environment_settings.contains(&settings) {
                    continue;
                }
                targets.push(EarnSubscriptionTarget {
                    environment: environment.to_owned(),
                    settings,
                    wallet: row.get("wallet"),
                    vault_index: row.get("vault_index"),
                    vault_pubkey: Some(row.get("vault_pubkey")),
                    policy_accounts: [
                        row.try_get::<Option<String>, _>("policy_account")?,
                        row.try_get::<Option<String>, _>("active_policy_account")?,
                        row.try_get::<Option<String>, _>("setup_policy_account")?,
                    ]
                    .into_iter()
                    .flatten()
                    .collect(),
                    markets: row
                        .try_get::<Option<String>, _>("market")?
                        .into_iter()
                        .collect(),
                    autodeposit_accounts: Vec::new(),
                    observation_start_slot: None,
                });
            }
        }

        let cross_mint_policies_exist: bool = sqlx::query_scalar(
            "SELECT to_regclass('loyal_yield.cross_mint_swap_policies') IS NOT NULL",
        )
        .fetch_one(&self.pool)
        .await?;
        if cross_mint_policies_exist {
            let rows = sqlx::query(
                r#"
                SELECT authority AS wallet, settings, vault_index, vault_pubkey,
                       array_agg(DISTINCT policy_account ORDER BY policy_account)
                           AS policy_accounts
                FROM loyal_yield.cross_mint_swap_policies
                WHERE cluster = $1
                  AND active
                  AND source_shard IN ('classic', 'token_2022')
                GROUP BY authority, settings, vault_index, vault_pubkey
                "#,
            )
            .bind(environment)
            .fetch_all(&self.pool)
            .await?;
            for row in rows {
                let settings: String = row.get("settings");
                if app_accounts_exist && !environment_settings.contains(&settings) {
                    continue;
                }
                targets.push(EarnSubscriptionTarget {
                    environment: environment.to_owned(),
                    settings,
                    wallet: row.get("wallet"),
                    vault_index: row.get("vault_index"),
                    vault_pubkey: Some(row.get("vault_pubkey")),
                    policy_accounts: row.get("policy_accounts"),
                    markets: Vec::new(),
                    autodeposit_accounts: Vec::new(),
                    observation_start_slot: None,
                });
            }
        }

        let autodeposit_targets_exist: bool = sqlx::query_scalar(
            "SELECT to_regclass('loyal_yield.balance_sweep_targets') IS NOT NULL",
        )
        .fetch_one(&self.pool)
        .await?;
        if autodeposit_targets_exist {
            let rows = sqlx::query(
                r#"
                SELECT settings, wallet, vault_index, vault_pubkey,
                       policy_account, subscription_authority, recurring_delegation
                FROM loyal_yield.balance_sweep_targets
                WHERE cluster = $1 AND chain_status <> 'closed'
                "#,
            )
            .bind(environment)
            .fetch_all(&self.pool)
            .await?;
            for row in rows {
                targets.push(EarnSubscriptionTarget {
                    environment: environment.to_owned(),
                    settings: row.get("settings"),
                    wallet: row.get("wallet"),
                    vault_index: row.get("vault_index"),
                    vault_pubkey: Some(row.get("vault_pubkey")),
                    policy_accounts: vec![row.get("policy_account")],
                    markets: Vec::new(),
                    autodeposit_accounts: [
                        row.try_get::<Option<String>, _>("subscription_authority")?,
                        row.try_get::<Option<String>, _>("recurring_delegation")?,
                    ]
                    .into_iter()
                    .flatten()
                    .collect(),
                    observation_start_slot: None,
                });
            }
        }

        Ok(targets)
    }

    pub async fn record_autodeposit_recurring_delegation(
        &self,
        input: AutodepositRecurringDelegationObserved,
    ) -> Result<BalanceSweepTargetId, OrchestratorError> {
        let slot = to_i64_slot(input.slot)?;
        let nonce = to_i64_amount(input.nonce)?;
        let amount = to_i64_amount(input.amount_per_period)?;
        let period = to_i64_amount(input.period_length_seconds)?;
        let mut tx = self.pool.begin().await?;
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.balance_sweep_targets
            SET setup_generation = CASE
                    WHEN recurring_delegation IS NOT NULL
                         AND recurring_delegation IS DISTINCT FROM $2
                    THEN setup_generation + 1
                    ELSE setup_generation
                END,
                subscription_authority = $1,
                recurring_delegation = $2,
                recurring_delegation_nonce = $3,
                max_amount_per_period = $4,
                period_length_seconds = $5,
                start_timestamp = $6,
                recurring_delegation_expiry_timestamp = $7,
                recurring_delegation_signature = $8,
                recurring_delegation_confirmed_slot = $9,
                chain_status = CASE WHEN chain_status = 'closed' THEN chain_status ELSE 'pending' END,
                chain_observation_slot = GREATEST(chain_observation_slot, $9),
                last_seen_at = now(),
                last_seen_slot = GREATEST(last_seen_slot, $9),
                last_seen_signature = CASE WHEN $9 >= last_seen_slot THEN $8 ELSE last_seen_signature END
            WHERE wallet = $10
              AND vault_pubkey = $11
              AND chain_status <> 'closed'
            RETURNING id
            "#,
        )
        .bind(&input.subscription_authority)
        .bind(&input.recurring_delegation)
        .bind(nonce)
        .bind(amount)
        .bind(period)
        .bind(input.start_timestamp)
        .bind(input.expiry_timestamp)
        .bind(&input.signature)
        .bind(slot)
        .bind(&input.wallet)
        .bind(&input.vault_pubkey)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Autodeposit policy target is not indexed yet; retry the durable event".to_owned(),
            )
        })?;
        let target_id = BalanceSweepTargetId(row.get("id"));
        upsert_autodeposit_reconciliation_request(&mut tx, target_id, slot).await?;
        tx.commit().await?;
        Ok(target_id)
    }

    pub async fn load_autodeposit_target_snapshot_context(
        &self,
        settings: &str,
        vault_pubkey: &str,
    ) -> Result<Option<AutodepositTargetSnapshotContext>, OrchestratorError> {
        let row = sqlx::query(
            r#"
            SELECT id, wallet, wallet_token_ata, policy_account,
                   subscription_authority, recurring_delegation, setup_generation
            FROM loyal_yield.balance_sweep_targets
            WHERE settings = $1 AND vault_pubkey = $2 AND chain_status <> 'closed'
              AND subscription_authority IS NOT NULL
              AND recurring_delegation IS NOT NULL
            ORDER BY policy_seed DESC
            LIMIT 1
            "#,
        )
        .bind(settings)
        .bind(vault_pubkey)
        .fetch_optional(&self.pool)
        .await?;
        Ok(row.map(|row| AutodepositTargetSnapshotContext {
            target_id: BalanceSweepTargetId(row.get("id")),
            wallet: row.get("wallet"),
            wallet_token_ata: row.get("wallet_token_ata"),
            policy_account: row.get("policy_account"),
            subscription_authority: row.get("subscription_authority"),
            recurring_delegation: row.get("recurring_delegation"),
            setup_generation: row.get("setup_generation"),
        }))
    }

    pub async fn load_autodeposit_target_snapshot_context_by_id(
        &self,
        target_id: BalanceSweepTargetId,
    ) -> Result<Option<AutodepositTargetSnapshotContext>, OrchestratorError> {
        let row = sqlx::query(
            r#"
            SELECT id, wallet, wallet_token_ata, policy_account,
                   subscription_authority, recurring_delegation, setup_generation
            FROM loyal_yield.balance_sweep_targets
            WHERE id = $1
              AND subscription_authority IS NOT NULL
              AND recurring_delegation IS NOT NULL
            "#,
        )
        .bind(target_id.as_i64())
        .fetch_optional(&self.pool)
        .await?;
        Ok(row.map(|row| AutodepositTargetSnapshotContext {
            target_id: BalanceSweepTargetId(row.get("id")),
            wallet: row.get("wallet"),
            wallet_token_ata: row.get("wallet_token_ata"),
            policy_account: row.get("policy_account"),
            subscription_authority: row.get("subscription_authority"),
            recurring_delegation: row.get("recurring_delegation"),
            setup_generation: row.get("setup_generation"),
        }))
    }

    pub async fn load_autodeposit_reconciliation_target_id(
        &self,
        settings: &str,
        vault_pubkey: &str,
        account_pubkey: &str,
    ) -> Result<Option<BalanceSweepTargetId>, OrchestratorError> {
        let target_id = sqlx::query_scalar(
            r#"
            SELECT id
            FROM loyal_yield.balance_sweep_targets
            WHERE settings = $1
              AND vault_pubkey = $2
              AND chain_status <> 'closed'
              AND (
                    policy_account = $3
                 OR subscription_authority = $3
                 OR recurring_delegation = $3
                 OR wallet_token_ata = $3
              )
            ORDER BY policy_seed DESC
            LIMIT 1
            "#,
        )
        .bind(settings)
        .bind(vault_pubkey)
        .bind(account_pubkey)
        .fetch_optional(&self.pool)
        .await?;
        Ok(target_id.map(BalanceSweepTargetId))
    }

    pub async fn reconcile_autodeposit_chain_observation(
        &self,
        input: AutodepositChainObservation,
    ) -> Result<AutodepositChainObservationResult, OrchestratorError> {
        let observation_slot = to_i64_slot(input.observation_slot)?;
        let wallet_balance_raw = to_i64_amount(input.wallet_balance_raw)?;
        let chain_status = if !input.observation_complete {
            "pending"
        } else if input.policy_valid
            && input.subscription_authority_valid
            && input.recurring_delegation_valid
            && input.token_delegate_valid
        {
            "active"
        } else if !input.policy_valid && !input.recurring_delegation_valid {
            "closed"
        } else {
            "inconsistent"
        };

        let mut tx = self.pool.begin().await?;
        let current = sqlx::query(
            r#"
            SELECT id, setup_generation, bootstrap_generation, chain_status,
                   chain_observation_slot, wallet, wallet_token_ata, token_mint,
                   wallet_balance_floor_raw
            FROM loyal_yield.balance_sweep_targets
            WHERE id = $1
            FOR UPDATE
            "#,
        )
        .bind(input.target_id.as_i64())
        .fetch_one(&mut *tx)
        .await?;
        let current_slot: i64 = current.get("chain_observation_slot");
        if observation_slot < current_slot {
            tx.commit().await?;
            return Ok(AutodepositChainObservationResult {
                target_id: input.target_id,
                chain_status: current.get("chain_status"),
                observation_slot: nonnegative_i64_to_u64(current_slot, "chain_observation_slot")?,
                bootstrap_generation: current.get("bootstrap_generation"),
            });
        }

        let generation: i64 = current.get("setup_generation");
        let bootstrap_generation: Option<i64> = current.get("bootstrap_generation");
        sqlx::query(
            r#"
            UPDATE loyal_yield.balance_sweep_targets
            SET chain_status = $1,
                chain_observation_slot = $2,
                last_seen_at = now(),
                last_seen_slot = GREATEST(last_seen_slot, $2)
            WHERE id = $3
            "#,
        )
        .bind(chain_status)
        .bind(observation_slot)
        .bind(input.target_id.as_i64())
        .execute(&mut *tx)
        .await?;

        let mut stored_bootstrap_generation = bootstrap_generation;
        if chain_status == "active" && bootstrap_generation != Some(generation) {
            if let Some(floor) = current.get::<Option<i64>, _>("wallet_balance_floor_raw") {
                if wallet_balance_raw > floor {
                    let event_id: i64 = sqlx::query_scalar(
                        "SELECT nextval('loyal_yield.autodeposit_bootstrap_event_id_seq')",
                    )
                    .fetch_one(&mut *tx)
                    .await?;
                    sqlx::query(
                        r#"
                    INSERT INTO loyal_yield.balance_sweep_wallet_balance_events
                        (event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata,
                         mint, previous_amount_raw, amount_raw, delta_amount_raw,
                         observed_slot, observed_at, source, source_commitment,
                         raw_evidence, projected_at)
                    VALUES ($1,$2,$3,$4,$4,$5,NULL,$6,NULL,$7,now(),
                            'laserstream_autodeposit_activation','finalized',
                            jsonb_build_object('bootstrapGeneration',$8),now())
                    "#,
                    )
                    .bind(event_id)
                    .bind(input.target_id.as_i64())
                    .bind(current.get::<String, _>("wallet"))
                    .bind(current.get::<String, _>("wallet_token_ata"))
                    .bind(current.get::<String, _>("token_mint"))
                    .bind(wallet_balance_raw)
                    .bind(observation_slot)
                    .bind(generation)
                    .execute(&mut *tx)
                    .await?;
                    let slot_id: i64 = sqlx::query_scalar(
                        r#"
                    INSERT INTO loyal_yield.balance_sweep_scheduled_slots
                        (target_id, token_mint, eligible_after, status)
                    VALUES ($1,$2,now() + interval '1 hour','scheduled')
                    RETURNING id
                    "#,
                    )
                    .bind(input.target_id.as_i64())
                    .bind(current.get::<String, _>("token_mint"))
                    .fetch_one(&mut *tx)
                    .await?;
                    sqlx::query(
                        r#"
                    INSERT INTO loyal_yield.balance_sweep_surplus_lots
                        (target_id, scheduled_slot_id, source_event_id, original_amount_raw,
                         remaining_amount_raw, classification, eligible_after, status,
                         confidence, reason)
                    VALUES ($1,$2,$3,$4,$4,'initial_surplus',now() + interval '1 hour',
                            'open','confirmed_snapshot',
                            'initial Autodeposit surplus observed by LaserStream')
                    "#,
                    )
                    .bind(input.target_id.as_i64())
                    .bind(slot_id)
                    .bind(event_id)
                    .bind(wallet_balance_raw - floor)
                    .execute(&mut *tx)
                    .await?;
                }
                sqlx::query(
                    "UPDATE loyal_yield.balance_sweep_targets SET bootstrap_generation = $1 WHERE id = $2",
                )
                .bind(generation)
                .bind(input.target_id.as_i64())
                .execute(&mut *tx)
                .await?;
                stored_bootstrap_generation = Some(generation);
            }
        }

        if chain_status == "closed" {
            sqlx::query(
                r#"
                UPDATE loyal_yield.balance_sweep_scheduled_slots
                SET status = 'canceled', updated_at = now()
                WHERE target_id = $1 AND status IN ('scheduled','requested')
                "#,
            )
            .bind(input.target_id.as_i64())
            .execute(&mut *tx)
            .await?;
            sqlx::query(
                r#"
                UPDATE loyal_yield.balance_sweep_surplus_lots
                SET status = 'suppressed', updated_at = now()
                WHERE target_id = $1 AND status = 'open' AND remaining_amount_raw > 0
                "#,
            )
            .bind(input.target_id.as_i64())
            .execute(&mut *tx)
            .await?;
        }
        tx.commit().await?;

        Ok(AutodepositChainObservationResult {
            target_id: input.target_id,
            chain_status: chain_status.to_owned(),
            observation_slot: input.observation_slot,
            bootstrap_generation: stored_bootstrap_generation,
        })
    }

    pub async fn load_earn_reconciliation_context(
        &self,
        settings: &str,
        vault_index: u8,
        vault_pubkey: &str,
    ) -> Result<EarnReconciliationContext, OrchestratorError> {
        let app_schema_ready: bool = sqlx::query_scalar(
            r#"
            SELECT to_regclass('loyal_yield.user_yield_positions') IS NOT NULL
               AND to_regclass('loyal_yield.user_yield_position_deposits') IS NOT NULL
               AND to_regclass('loyal_yield.user_yield_position_withdrawals') IS NOT NULL
               AND to_regclass('loyal_yield.user_yield_position_holding_events') IS NOT NULL
            "#,
        )
        .fetch_one(&self.pool)
        .await?;
        if !app_schema_ready {
            return Err(OrchestratorError::StoreInvariant(
                "canonical Loyal App Earn tables are required for direct reconciliation".to_owned(),
            ));
        }
        let policy_rows = sqlx::query(
            r#"
            SELECT policy.settings, policy.authority, policy.policy_seed,
                   policy.policy_account, policy.vault_index, policy.vault_pubkey,
                   policy.delegated_signers, policy.threshold, policy.route_modes,
                   policy.stable_mints, policy.kamino_markets,
                   policy.kamino_liquidity_mints, policy.universe_preset,
                   policy.risk_profile, policy.swap_lanes, policy.last_seen_slot,
                   policy.last_seen_signature, policy.cluster,
                   policy.source_commitment,
                   CASE
                     WHEN policy.id = vault.setup_policy_id
                     THEN 'setup'
                     ELSE 'route'
                   END AS role
            FROM loyal_yield.route_policies policy
            LEFT JOIN loyal_yield.managed_vaults vault
              ON vault.settings = policy.settings
             AND vault.vault_index = policy.vault_index
             AND vault.vault_pubkey = policy.vault_pubkey
            WHERE policy.settings = $1 AND policy.vault_index = $2
              AND policy.vault_pubkey = $3
              AND policy.id IN (vault.active_policy_id, vault.setup_policy_id)
            "#,
        )
        .bind(settings)
        .bind(i16::from(vault_index))
        .bind(vault_pubkey)
        .fetch_all(&self.pool)
        .await?;
        let mut route_policy = None;
        let mut setup_policy = None;
        for row in policy_rows {
            let role: String = row.try_get("role")?;
            let policy = policy_match_from_dynamic_row(&row)?;
            if role == "setup" {
                setup_policy = Some(policy);
            } else {
                route_policy = Some(policy);
            }
        }
        Ok(EarnReconciliationContext {
            route_policy,
            setup_policy,
        })
    }

    pub async fn protected_route_lookup_table_addresses(
        &self,
    ) -> Result<Vec<String>, OrchestratorError> {
        if !route_lookup_tables_relation_exists(&self.pool).await? {
            return Ok(Vec::new());
        }
        let rows = sqlx::query_scalar::<_, String>(
            r#"
            SELECT table_address
            FROM loyal_yield.route_lookup_tables
            WHERE durable = TRUE
              AND status NOT IN ('closed')
            "#,
        )
        .fetch_all(&self.pool)
        .await?;
        Ok(rows)
    }

    pub async fn protected_legacy_route_lookup_table_addresses(
        &self,
    ) -> Result<Vec<String>, OrchestratorError> {
        if !route_lookup_tables_relation_exists(&self.pool).await? {
            return Ok(Vec::new());
        }
        let rows = sqlx::query_scalar::<_, String>(
            r#"
            SELECT table_address
            FROM loyal_yield.route_lookup_tables
            WHERE durable = TRUE
              AND status NOT IN ('closed')
              AND family_id IS NULL
            "#,
        )
        .fetch_all(&self.pool)
        .await?;
        Ok(rows)
    }

    pub async fn upsert_route_lookup_table(
        &self,
        input: RouteLookupTableUpsert,
    ) -> Result<RouteLookupTable, OrchestratorError> {
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.route_lookup_tables
                (cluster, scope, table_address, authority, payer, status, durable,
                 address_count, address_hash, addresses, create_signature,
                 extend_signatures, last_extended_slot, warmup_slot, notes)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
            ON CONFLICT (table_address) DO UPDATE SET
                cluster = EXCLUDED.cluster,
                scope = EXCLUDED.scope,
                authority = EXCLUDED.authority,
                payer = EXCLUDED.payer,
                status = EXCLUDED.status,
                durable = EXCLUDED.durable,
                address_count = EXCLUDED.address_count,
                address_hash = EXCLUDED.address_hash,
                addresses = EXCLUDED.addresses,
                create_signature = COALESCE(loyal_yield.route_lookup_tables.create_signature, EXCLUDED.create_signature),
                extend_signatures = COALESCE(loyal_yield.route_lookup_tables.extend_signatures, '[]'::jsonb) || EXCLUDED.extend_signatures,
                last_extended_slot = EXCLUDED.last_extended_slot,
                warmup_slot = EXCLUDED.warmup_slot,
                notes = EXCLUDED.notes,
                updated_at = now()
            RETURNING id, cluster, scope, table_address, authority, payer, status, durable,
                      address_count, address_hash, addresses, create_signature,
                      extend_signatures, last_extended_slot, warmup_slot,
                      deactivated_slot, deactivate_signature, closed_signature,
                      close_recipient, reclaimed_lamports, notes, created_at, updated_at
            "#
        )
        .bind(input.cluster)
        .bind(input.scope)
        .bind(input.table_address)
        .bind(input.authority)
        .bind(input.payer)
        .bind(input.status)
        .bind(input.durable)
        .bind(input.address_count)
        .bind(input.address_hash)
        .bind(input.addresses)
        .bind(input.create_signature)
        .bind(input.extend_signatures)
        .bind(input.last_extended_slot)
        .bind(input.warmup_slot)
        .bind(input.notes)
        .fetch_one(&self.pool)
        .await?;
        Ok(route_lookup_table_from_row(row))
    }

    pub async fn mark_route_lookup_table_deactivated(
        &self,
        table_address: &str,
        deactivated_slot: i64,
        signature: &str,
    ) -> Result<RouteLookupTable, OrchestratorError> {
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.route_lookup_tables
            SET status = 'deactivated',
                deactivated_slot = $2,
                deactivate_signature = $3,
                updated_at = now()
            WHERE table_address = $1
            RETURNING id, cluster, scope, table_address, authority, payer, status, durable,
                      address_count, address_hash, addresses, create_signature,
                      extend_signatures, last_extended_slot, warmup_slot,
                      deactivated_slot, deactivate_signature, closed_signature,
                      close_recipient, reclaimed_lamports, notes, created_at, updated_at
            "#,
        )
        .bind(table_address)
        .bind(deactivated_slot)
        .bind(signature)
        .fetch_one(&self.pool)
        .await?;
        Ok(route_lookup_table_from_row(row))
    }

    pub async fn mark_route_lookup_table_closed(
        &self,
        table_address: &str,
        signature: &str,
        recipient: &str,
        reclaimed_lamports: i64,
    ) -> Result<RouteLookupTable, OrchestratorError> {
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.route_lookup_tables
            SET status = 'closed',
                closed_signature = $2,
                close_recipient = $3,
                reclaimed_lamports = $4,
                updated_at = now()
            WHERE table_address = $1
            RETURNING id, cluster, scope, table_address, authority, payer, status, durable,
                      address_count, address_hash, addresses, create_signature,
                      extend_signatures, last_extended_slot, warmup_slot,
                      deactivated_slot, deactivate_signature, closed_signature,
                      close_recipient, reclaimed_lamports, notes, created_at, updated_at
            "#,
        )
        .bind(table_address)
        .bind(signature)
        .bind(recipient)
        .bind(reclaimed_lamports)
        .fetch_one(&self.pool)
        .await?;
        Ok(route_lookup_table_from_row(row))
    }

    pub async fn record_policy_match(
        &self,
        event: PolicyMatchInput,
    ) -> Result<StoredPolicyMatch, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let policy = upsert_policy(&mut tx, &event).await?;
        let vault = upsert_vault(&mut tx, policy.id, &event).await?;
        tx.commit().await?;
        Ok(StoredPolicyMatch { policy, vault })
    }

    pub async fn record_cross_mint_swap_policy_manifest(
        &self,
        event: CrossMintSwapPolicyManifestInput,
    ) -> Result<Option<CrossMintSwapPolicy>, OrchestratorError> {
        validate_cross_mint_swap_policy_manifest_input(&event)?;
        let slot = to_i64_slot(event.slot)?;
        let policy_seed = event.policy_seed.map(to_i64_policy_seed).transpose()?;
        let daily_source_mint_spending_cap = i64::try_from(event.daily_source_mint_spending_cap)
            .map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "cross-mint daily source-mint spending cap exceeds PostgreSQL BIGINT"
                        .to_owned(),
                )
            })?;
        let mut tx = self.pool.begin().await?;
        let lock_key = format!(
            "cross-mint-swap-policy:{}:{}",
            event.cluster, event.policy_account
        );
        sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0::bigint))")
            .bind(&lock_key)
            .execute(&mut *tx)
            .await?;

        let current =
            fetch_cross_mint_swap_policy_for_update(&mut tx, &event.cluster, &event.policy_account)
                .await?;
        let row = match current {
            None => Some(insert_cross_mint_swap_policy(&mut tx, &event, policy_seed, slot).await?),
            Some(current) if current.last_mutation == "remove" => None,
            Some(current) if slot < current.last_seen_slot => Some(current),
            Some(current)
                if slot == current.last_seen_slot
                    && event.signature == current.last_seen_signature
                    && Some(event.manifest_fingerprint.as_str())
                        == current.manifest_fingerprint.as_deref()
                    && Some(event.source_shard.as_str()) == current.source_shard.as_deref()
                    && event.settings == current.settings
                    && event.authority == current.authority
                    && policy_seed == current.policy_seed
                    && Some(i16::from(event.vault_index)) == current.vault_index
                    && Some(event.vault_pubkey.as_str()) == current.vault_pubkey.as_deref()
                    && Some(event.delegated_signer.as_str())
                        == current.delegated_signer.as_deref()
                    && Some(i32::from(event.max_slippage_bps)) == current.max_slippage_bps
                    && Some(daily_source_mint_spending_cap)
                        == current.daily_source_mint_spending_cap
                    && event.mutation == current.last_mutation
                    && commitment_rank(&event.source_commitment)?
                        > commitment_rank(&current.source_commitment)? =>
            {
                Some(
                    update_cross_mint_policy_finality(
                        &mut tx,
                        &event.cluster,
                        &event.policy_account,
                        &event.source_commitment,
                    )
                    .await?,
                )
            }
            Some(current)
                if slot == current.last_seen_slot
                    && event.signature == current.last_seen_signature
                    && Some(event.manifest_fingerprint.as_str())
                        == current.manifest_fingerprint.as_deref()
                    && Some(event.source_shard.as_str()) == current.source_shard.as_deref()
                    && event.settings == current.settings
                    && event.authority == current.authority
                    && policy_seed == current.policy_seed
                    && Some(i16::from(event.vault_index)) == current.vault_index
                    && Some(event.vault_pubkey.as_str()) == current.vault_pubkey.as_deref()
                    && Some(event.delegated_signer.as_str())
                        == current.delegated_signer.as_deref()
                    && Some(i32::from(event.max_slippage_bps)) == current.max_slippage_bps
                    && Some(daily_source_mint_spending_cap)
                        == current.daily_source_mint_spending_cap
                    && event.mutation == current.last_mutation =>
            {
                Some(current)
            }
            Some(current) => Some(
                mark_cross_mint_policy_ambiguous(
                    &mut tx,
                    &event.cluster,
                    &event.policy_account,
                    stronger_commitment(&event.source_commitment, &current.source_commitment)?,
                    slot.max(current.last_seen_slot),
                    &event.signature,
                )
                .await?,
            ),
        };
        if row.as_ref().is_some_and(|policy| policy.start_eligible) {
            ensure_cross_mint_vault_opt_in_for_canonical_pair(&mut tx, &event).await?;
        }
        tx.commit().await?;
        row.map(cross_mint_swap_policy_from_row).transpose()
    }

    pub async fn load_finalized_active_cross_mint_swap_policies(
        &self,
        lookup: CrossMintSwapPolicyLookup,
    ) -> Result<Vec<CrossMintSwapPolicy>, OrchestratorError> {
        let minimum_slot = to_i64_slot(lookup.minimum_slot)?;
        let rows = sqlx::query_as::<_, CrossMintSwapPolicyRow>(
            r#"
            SELECT *
            FROM loyal_yield.cross_mint_swap_policies
            WHERE cluster = $1
              AND settings = $2
              AND vault_index = $3
              AND vault_pubkey = $4
              AND last_seen_slot >= $5
              AND active
              AND start_eligible
              AND source_commitment IN ('confirmed', 'finalized')
              AND last_mutation IN ('create', 'update')
            ORDER BY last_seen_slot DESC, source_shard ASC, policy_account ASC
            "#,
        )
        .bind(&lookup.cluster)
        .bind(&lookup.settings)
        .bind(i16::from(lookup.vault_index))
        .bind(&lookup.vault_pubkey)
        .bind(minimum_slot)
        .fetch_all(&self.pool)
        .await?;
        rows.into_iter()
            .map(cross_mint_swap_policy_from_row)
            .collect()
    }

    pub async fn load_cross_mint_vault_opt_in(
        &self,
        lookup: CrossMintVaultOptInLookup,
    ) -> Result<Option<CrossMintVaultOptIn>, OrchestratorError> {
        validate_cross_mint_vault_opt_in_lookup(&lookup)?;
        let row = sqlx::query_as::<_, CrossMintVaultOptInRow>(
            r#"
            SELECT cluster, settings, vault_index, vault_pubkey, enabled,
                   generation, created_at, updated_at
            FROM loyal_yield.cross_mint_vault_opt_ins
            WHERE cluster = $1
              AND settings = $2
              AND vault_index = $3
              AND vault_pubkey = $4
            "#,
        )
        .bind(&lookup.cluster)
        .bind(&lookup.settings)
        .bind(i16::from(lookup.vault_index))
        .bind(&lookup.vault_pubkey)
        .fetch_optional(&self.pool)
        .await?;
        row.map(cross_mint_vault_opt_in_from_row).transpose()
    }

    /// Records only run/pause intent. Policy identity and risk settings remain
    /// authoritative in the finalized on-chain policy projection.
    pub async fn upsert_cross_mint_vault_opt_in(
        &self,
        input: CrossMintVaultOptInUpsert,
    ) -> Result<CrossMintVaultOptIn, OrchestratorError> {
        validate_cross_mint_vault_opt_in_upsert(&input)?;
        let row = sqlx::query_as::<_, CrossMintVaultOptInRow>(
            r#"
            INSERT INTO loyal_yield.cross_mint_vault_opt_ins
                (cluster, settings, vault_index, vault_pubkey, enabled)
            VALUES ($1, $2, $3, $4, $5)
            ON CONFLICT (cluster, settings, vault_index, vault_pubkey)
            DO UPDATE SET updated_at = cross_mint_vault_opt_ins.updated_at
            RETURNING cluster, settings, vault_index, vault_pubkey, enabled,
                      generation, created_at, updated_at
            "#,
        )
        .bind(&input.cluster)
        .bind(&input.settings)
        .bind(i16::from(input.vault_index))
        .bind(&input.vault_pubkey)
        .bind(input.enabled)
        .fetch_one(&self.pool)
        .await?;
        cross_mint_vault_opt_in_from_row(row)
    }

    /// Disables new cross-mint starts in its own committed transaction. Callers
    /// must await this method before beginning on-chain policy removal.
    pub async fn disable_cross_mint_vault_opt_in(
        &self,
        lookup: CrossMintVaultOptInLookup,
        expected_generation: i64,
    ) -> Result<Option<CrossMintVaultOptIn>, OrchestratorError> {
        self.set_cross_mint_vault_opt_in_enabled(lookup, false, expected_generation)
            .await
    }

    pub async fn enable_cross_mint_vault_opt_in(
        &self,
        lookup: CrossMintVaultOptInLookup,
        expected_generation: i64,
    ) -> Result<Option<CrossMintVaultOptIn>, OrchestratorError> {
        self.set_cross_mint_vault_opt_in_enabled(lookup, true, expected_generation)
            .await
    }

    async fn set_cross_mint_vault_opt_in_enabled(
        &self,
        lookup: CrossMintVaultOptInLookup,
        enabled: bool,
        expected_generation: i64,
    ) -> Result<Option<CrossMintVaultOptIn>, OrchestratorError> {
        validate_cross_mint_vault_opt_in_lookup(&lookup)?;
        if expected_generation <= 0 {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint opt-in transition requires a positive expected generation".to_owned(),
            ));
        }
        let mut tx = self.pool.begin().await?;
        let current = sqlx::query_as::<_, CrossMintVaultOptInRow>(
            r#"
            SELECT cluster, settings, vault_index, vault_pubkey, enabled,
                   generation, created_at, updated_at
            FROM loyal_yield.cross_mint_vault_opt_ins
            WHERE cluster = $1
              AND settings = $2
              AND vault_index = $3
              AND vault_pubkey = $4
            FOR UPDATE
            "#,
        )
        .bind(&lookup.cluster)
        .bind(&lookup.settings)
        .bind(i16::from(lookup.vault_index))
        .bind(&lookup.vault_pubkey)
        .fetch_optional(&mut *tx)
        .await?;
        let Some(current) = current else {
            tx.commit().await?;
            return Ok(None);
        };
        if current.enabled == enabled {
            if current.generation == expected_generation
                || expected_generation.checked_add(1) == Some(current.generation)
            {
                tx.commit().await?;
                return cross_mint_vault_opt_in_from_row(current).map(Some);
            }
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint opt-in generation changed before transition".to_owned(),
            ));
        }
        if current.generation != expected_generation {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint opt-in generation changed before transition".to_owned(),
            ));
        }
        if enabled && !canonical_cross_mint_pair_exists(&mut tx, &lookup).await? {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint opt-in cannot resume without one canonical finalized policy pair"
                    .to_owned(),
            ));
        }
        let row = sqlx::query_as::<_, CrossMintVaultOptInRow>(
            r#"
            UPDATE loyal_yield.cross_mint_vault_opt_ins
            SET enabled = $5,
                generation = generation + 1,
                updated_at = now()
            WHERE cluster = $1
              AND settings = $2
              AND vault_index = $3
              AND vault_pubkey = $4
              AND generation = $6
            RETURNING cluster, settings, vault_index, vault_pubkey, enabled,
                      generation, created_at, updated_at
            "#,
        )
        .bind(&lookup.cluster)
        .bind(&lookup.settings)
        .bind(i16::from(lookup.vault_index))
        .bind(&lookup.vault_pubkey)
        .bind(enabled)
        .bind(expected_generation)
        .fetch_one(&mut *tx)
        .await?;
        tx.commit().await?;
        cross_mint_vault_opt_in_from_row(row).map(Some)
    }

    pub async fn record_policy_removal(
        &self,
        event: PolicyRemovalInput,
    ) -> Result<PolicyRemovalResult, OrchestratorError> {
        commitment_rank(&event.source_commitment)?;
        let slot = to_i64_slot(event.slot)?;
        let mut tx = self.pool.begin().await?;
        let lock_key = format!(
            "cross-mint-swap-policy:{}:{}",
            event.cluster, event.policy_account
        );
        sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0::bigint))")
            .bind(&lock_key)
            .execute(&mut *tx)
            .await?;

        let swap_policy_deactivated =
            deactivate_cross_mint_swap_policy(&mut tx, &event, slot).await?;
        delete_cross_mint_vault_opt_in_after_complete_removal(&mut tx, &event).await?;
        let route_policy_id = sqlx::query_scalar::<_, i64>(
            r#"
            UPDATE loyal_yield.route_policies
            SET active = FALSE,
                finalized_eligible = FALSE,
                cluster = $1,
                source_commitment = $2,
                last_seen_at = now(),
                last_seen_slot = $3,
                last_seen_signature = $4
            WHERE policy_account = $5
              AND settings = $6
              AND authority = $7
              AND $3 >= last_seen_slot
            RETURNING id
            "#,
        )
        .bind(&event.cluster)
        .bind(&event.source_commitment)
        .bind(slot)
        .bind(&event.signature)
        .bind(&event.policy_account)
        .bind(&event.settings)
        .bind(&event.authority)
        .fetch_optional(&mut *tx)
        .await?;

        let managed_vault_deactivated = if let Some(policy_id) = route_policy_id {
            sqlx::query(
                r#"
                UPDATE loyal_yield.managed_vaults
                SET active = FALSE, last_seen_at = now()
                WHERE active_policy_id = $1 AND active
                "#,
            )
            .bind(policy_id)
            .execute(&mut *tx)
            .await?
            .rows_affected()
                > 0
        } else {
            false
        };

        let balance_sweep_target_deactivated: bool = sqlx::query_scalar(
            r#"
            WITH closed_target AS (
            UPDATE loyal_yield.balance_sweep_targets
            SET chain_status = 'closed',
                chain_observation_slot = GREATEST(chain_observation_slot, $1),
                last_seen_at = now(),
                last_seen_slot = $1,
                last_seen_signature = $2
            WHERE policy_account = $3
              AND settings = $4
              AND authority = $5
              AND $1 >= chain_observation_slot
              AND chain_status <> 'closed'
            RETURNING id
            ), canceled_slots AS (
              UPDATE loyal_yield.balance_sweep_scheduled_slots
              SET status = 'canceled', updated_at = now()
              WHERE target_id IN (SELECT id FROM closed_target)
                AND status IN ('scheduled','requested')
            ), suppressed_lots AS (
              UPDATE loyal_yield.balance_sweep_surplus_lots
              SET status = 'suppressed', updated_at = now()
              WHERE target_id IN (SELECT id FROM closed_target)
                AND status = 'open' AND remaining_amount_raw > 0
            )
            SELECT EXISTS(SELECT 1 FROM closed_target)
            "#,
        )
        .bind(slot)
        .bind(&event.signature)
        .bind(&event.policy_account)
        .bind(&event.settings)
        .bind(&event.authority)
        .fetch_one(&mut *tx)
        .await?;

        tx.commit().await?;
        Ok(PolicyRemovalResult {
            swap_policy_deactivated,
            route_policy_deactivated: route_policy_id.is_some(),
            managed_vault_deactivated,
            balance_sweep_target_deactivated,
        })
    }

    pub async fn record_route_and_setup_policy_match(
        &self,
        route_event: PolicyMatchInput,
        setup_event: PolicyMatchInput,
    ) -> Result<(StoredPolicyMatch, RoutePolicy), OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let route_policy = upsert_policy(&mut tx, &route_event).await?;
        let setup_policy = upsert_policy(&mut tx, &setup_event).await?;
        let vault =
            upsert_vault_with_setup(&mut tx, route_policy.id, setup_policy.id, &route_event)
                .await?;
        tx.commit().await?;
        Ok((
            StoredPolicyMatch {
                policy: route_policy,
                vault,
            },
            setup_policy,
        ))
    }

    pub async fn record_balance_sweep_policy_match(
        &self,
        event: BalanceSweepPolicyMatchInput,
    ) -> Result<BalanceSweepTarget, OrchestratorError> {
        let policy_seed = to_i64_policy_seed(event.policy_seed)?;
        let slot = to_i64_slot(event.slot)?;
        let max_amount_per_period = to_i64_amount(event.max_amount_per_period)?;
        let mut tx = self.pool.begin().await?;
        let rollover_lock = format!(
            "{}|{}|{}|{}",
            event.settings, event.wallet, event.vault_index, event.token_mint
        );
        sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")
            .bind(rollover_lock)
            .execute(&mut *tx)
            .await?;

        let current = sqlx::query(
            r#"
            SELECT id, policy_account,
                   GREATEST(last_seen_slot, chain_observation_slot) AS observation_slot
            FROM loyal_yield.balance_sweep_targets
            WHERE settings = $1
              AND wallet = $2
              AND vault_index = $3
              AND token_mint = $4
              AND chain_status <> 'closed'
            FOR UPDATE
            "#,
        )
        .bind(&event.settings)
        .bind(&event.wallet)
        .bind(i16::from(event.vault_index))
        .bind(&event.token_mint)
        .fetch_optional(&mut *tx)
        .await?;

        if let Some(current) = current {
            let current_policy_account: String = current.get("policy_account");
            let current_observation_slot: i64 = current.get("observation_slot");
            if current_policy_account != event.policy_account {
                if slot <= current_observation_slot {
                    let row = sqlx::query(
                        r#"
                        SELECT
                            id, settings, authority, policy_seed, policy_account, vault_index,
                            vault_pubkey, wallet,
                            COALESCE(wallet_usdc_ata, wallet_token_ata) AS wallet_usdc_ata,
                            COALESCE(vault_usdc_ata, vault_token_ata) AS vault_usdc_ata,
                            token_mint, wallet_token_ata, vault_token_ata, delegated_signers,
                            threshold, max_amount_per_period, desired_active AS active,
                            first_seen_at, last_seen_at, last_seen_slot, last_seen_signature
                        FROM loyal_yield.balance_sweep_targets
                        WHERE id = $1
                        "#,
                    )
                    .bind(current.get::<i64, _>("id"))
                    .fetch_one(&mut *tx)
                    .await?;
                    tx.commit().await?;
                    return balance_sweep_target_from_row(&row);
                }

                sqlx::query(
                    r#"
                    WITH closed_target AS (
                        UPDATE loyal_yield.balance_sweep_targets
                        SET desired_active = FALSE,
                            chain_status = 'closed',
                            chain_observation_slot = GREATEST(chain_observation_slot, $1),
                            closed_at = COALESCE(closed_at, now())
                        WHERE id = $2
                          AND chain_status <> 'closed'
                        RETURNING id
                    ), canceled_slots AS (
                        UPDATE loyal_yield.balance_sweep_scheduled_slots
                        SET status = 'canceled', updated_at = now()
                        WHERE target_id IN (SELECT id FROM closed_target)
                          AND status IN ('scheduled', 'requested')
                    )
                    UPDATE loyal_yield.balance_sweep_surplus_lots
                    SET status = 'suppressed', updated_at = now()
                    WHERE target_id IN (SELECT id FROM closed_target)
                      AND status = 'open'
                      AND remaining_amount_raw > 0
                    "#,
                )
                .bind(slot)
                .bind(current.get::<i64, _>("id"))
                .execute(&mut *tx)
                .await?;
            }
        }

        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.balance_sweep_targets
                (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
                 wallet, wallet_usdc_ata, vault_usdc_ata, token_mint, wallet_token_ata,
                 vault_token_ata, delegated_signers, threshold, max_amount_per_period,
                 desired_active, chain_status, chain_observation_slot,
                 wallet_balance_floor_raw, last_seen_slot, last_seen_signature)
            VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), $10, $11, $12, $13, $14, $15,
                TRUE, 'pending', $16, NULL, $16, $17)
            ON CONFLICT (policy_account) DO UPDATE SET
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
                token_mint = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.token_mint
                    ELSE loyal_yield.balance_sweep_targets.token_mint
                END,
                wallet_token_ata = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.wallet_token_ata
                    ELSE loyal_yield.balance_sweep_targets.wallet_token_ata
                END,
                vault_token_ata = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                    THEN EXCLUDED.vault_token_ata
                    ELSE loyal_yield.balance_sweep_targets.vault_token_ata
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
                chain_status = CASE
                    WHEN EXCLUDED.last_seen_slot > loyal_yield.balance_sweep_targets.last_seen_slot
                         AND loyal_yield.balance_sweep_targets.chain_status <> 'closed'
                    THEN 'pending'
                    ELSE loyal_yield.balance_sweep_targets.chain_status
                END,
                chain_observation_slot = GREATEST(
                    loyal_yield.balance_sweep_targets.chain_observation_slot,
                    EXCLUDED.chain_observation_slot
                ),
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
                id, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
                wallet,
                COALESCE(wallet_usdc_ata, wallet_token_ata) AS wallet_usdc_ata,
                COALESCE(vault_usdc_ata, vault_token_ata) AS vault_usdc_ata,
                token_mint, wallet_token_ata, vault_token_ata, delegated_signers, threshold,
                max_amount_per_period, desired_active AS active, first_seen_at, last_seen_at, last_seen_slot, last_seen_signature
            "#,
        )
        .bind(&event.settings)
        .bind(&event.authority)
        .bind(policy_seed)
        .bind(&event.policy_account)
        .bind(i16::from(event.vault_index))
        .bind(&event.vault_pubkey)
        .bind(&event.wallet)
        .bind(&event.wallet_usdc_ata)
        .bind(&event.vault_usdc_ata)
        .bind(&event.token_mint)
        .bind(&event.wallet_token_ata)
        .bind(&event.vault_token_ata)
        .bind(&event.delegated_signers)
        .bind(i32::from(event.threshold))
        .bind(max_amount_per_period)
        .bind(slot)
        .bind(&event.signature)
        .fetch_one(&mut *tx)
        .await?;

        tx.commit().await?;
        balance_sweep_target_from_row(&row)
    }

    pub async fn load_active_balance_sweep_targets(
        &self,
    ) -> Result<Vec<BalanceSweepTarget>, OrchestratorError> {
        let rows = sqlx::query(
            r#"
            SELECT
                id, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
                wallet,
                COALESCE(wallet_usdc_ata, wallet_token_ata) AS wallet_usdc_ata,
                COALESCE(vault_usdc_ata, vault_token_ata) AS vault_usdc_ata,
                token_mint, wallet_token_ata, vault_token_ata, delegated_signers, threshold,
                max_amount_per_period, desired_active AS active, first_seen_at, last_seen_at, last_seen_slot, last_seen_signature
            FROM loyal_yield.balance_sweep_targets
            WHERE desired_active
              AND chain_status = 'active'
            ORDER BY id
            "#,
        )
        .fetch_all(&self.pool)
        .await?;

        rows.iter().map(balance_sweep_target_from_row).collect()
    }

    pub async fn record_wallet_ata_balance_update(
        &self,
        input: WalletAtaBalanceUpdateInput,
    ) -> Result<WalletAtaBalanceCurrent, OrchestratorError> {
        let mut connection = self.pool.acquire().await?;
        record_wallet_ata_balance_update_in_tx(&mut connection, input).await
    }

    pub async fn project_wallet_ata_balance_updates<F, Fut>(
        &self,
        consumer_name: &str,
        batch_limit: i64,
        fetch_after_cursor: F,
    ) -> Result<ProjectionBatchOutcome, OrchestratorError>
    where
        F: FnOnce(i64, i64) -> Fut,
        Fut: Future<Output = Result<Vec<ProjectedWalletAtaBalanceUpdateInput>, OrchestratorError>>,
    {
        let mut tx = self.pool.begin().await?;
        let last_event_id = lock_projection_offset(&mut tx, consumer_name).await?;
        let updates = fetch_after_cursor(last_event_id, batch_limit).await?;
        let mut projected_count = 0_usize;
        let mut next_event_id = last_event_id;

        for projected in updates {
            if projected.event_id <= last_event_id {
                continue;
            }
            record_projected_wallet_ata_balance_update_in_tx(
                &mut tx,
                projected.event_id,
                projected.update,
            )
            .await?;
            next_event_id = projected.event_id;
            projected_count += 1;
        }

        if next_event_id > last_event_id {
            sqlx::query(
                r#"
                UPDATE loyal_yield.projection_offsets
                SET last_event_id = $2,
                    updated_at = now()
                WHERE consumer_name = $1
                "#,
            )
            .bind(consumer_name)
            .bind(next_event_id)
            .execute(&mut *tx)
            .await?;
        }

        tx.commit().await?;
        Ok(ProjectionBatchOutcome {
            projected_count,
            previous_event_id: last_event_id,
            last_event_id: next_event_id,
        })
    }

    pub async fn projection_offset(&self, consumer_name: &str) -> Result<i64, OrchestratorError> {
        let event_id = sqlx::query_scalar(
            r#"
            SELECT last_event_id
            FROM loyal_yield.projection_offsets
            WHERE consumer_name = $1
            "#,
        )
        .bind(consumer_name)
        .fetch_optional(&self.pool)
        .await?
        .unwrap_or(0);
        Ok(event_id)
    }

    pub async fn project_earn_max_policy_set(
        &self,
        consumer_name: &str,
        input: EarnMaxPolicySetProjectionInput,
    ) -> Result<(), OrchestratorError> {
        if input.settings.trim().is_empty()
            || input.vault.trim().is_empty()
            || input.manifest_version.trim().is_empty()
            || input.observed_signature.trim().is_empty()
            || input.policy_seed_base == 0
            || !matches!(input.status.as_str(), "incomplete" | "ready" | "removed")
            || input.manifest_sha256.len() != 64
            || !input
                .manifest_sha256
                .bytes()
                .all(|value| value.is_ascii_hexdigit() && !value.is_ascii_uppercase())
            || !input.policy_accounts.is_array()
        {
            return Err(OrchestratorError::StoreInvariant(
                "Earn MAX policy projection is malformed".to_owned(),
            ));
        }
        let observed_slot = i64::try_from(input.observed_slot)
            .map_err(|_| OrchestratorError::SlotOutOfRange(input.observed_slot))?;
        let mut tx = self.pool.begin().await?;
        let cursor = lock_projection_offset(&mut tx, consumer_name).await?;
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.earn_max_policy_sets (
                settings, vault_index, vault, manifest_version, manifest_sha256,
                policy_seed_base, status, policy_accounts, observed_signature,
                observed_slot, observed_at, updated_at
            ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
            ON CONFLICT (settings, vault_index) DO UPDATE SET
                vault = EXCLUDED.vault,
                manifest_version = EXCLUDED.manifest_version,
                manifest_sha256 = EXCLUDED.manifest_sha256,
                policy_seed_base = EXCLUDED.policy_seed_base,
                status = EXCLUDED.status,
                policy_accounts = EXCLUDED.policy_accounts,
                observed_signature = EXCLUDED.observed_signature,
                observed_slot = EXCLUDED.observed_slot,
                observed_at = EXCLUDED.observed_at,
                updated_at = now()
            WHERE EXCLUDED.observed_slot >= loyal_yield.earn_max_policy_sets.observed_slot
            "#,
        )
        .bind(&input.settings)
        .bind(i16::from(input.vault_index))
        .bind(&input.vault)
        .bind(&input.manifest_version)
        .bind(&input.manifest_sha256)
        .bind(i64::try_from(input.policy_seed_base).map_err(|_| {
            OrchestratorError::StoreInvariant("Earn MAX policy seed base exceeds BIGINT".to_owned())
        })?)
        .bind(&input.status)
        .bind(&input.policy_accounts)
        .bind(&input.observed_signature)
        .bind(observed_slot)
        .bind(&input.observed_at)
        .execute(&mut *tx)
        .await?;
        if input.status == "ready" {
            let route = sqlx::query(
                r#"
                SELECT state, lease_owner, lease_expires_at
                FROM loyal_yield.multiply_route_states
                WHERE settings=$1 AND vault_index=$2
                FOR UPDATE
                "#,
            )
            .bind(&input.settings)
            .bind(i16::from(input.vault_index))
            .fetch_optional(&mut *tx)
            .await?;
            if let Some(route) = route {
                let lease_owner: Option<String> = route.try_get("lease_owner")?;
                let lease_expires_at: Option<DateTime<Utc>> = route.try_get("lease_expires_at")?;
                if lease_owner.is_some()
                    && lease_expires_at.is_some_and(|expiry| expiry > Utc::now())
                {
                    return Err(OrchestratorError::StoreInvariant(
                        "terminal Earn MAX route is actively leased; retry policy projection"
                            .to_owned(),
                    ));
                }
                let state: MultiplyRouteState = serde_json::from_value(route.try_get("state")?)
                    .map_err(|error| OrchestratorError::StoreInvariant(error.to_string()))?;
                if state.policy_seed_base != input.policy_seed_base {
                    let state = state
                        .roll_terminal_policy_seed_base(
                            input.policy_seed_base,
                            input.observed_slot,
                            input.observed_at,
                        )
                        .map_err(|error| OrchestratorError::StoreInvariant(error.to_string()))?;
                    sqlx::query(
                        r#"
                        UPDATE loyal_yield.multiply_route_states
                        SET state=$2, state_version=$3, lease_owner=NULL,
                            lease_expires_at=NULL, updated_at=now()
                        WHERE route_key=$1
                        "#,
                    )
                    .bind(&state.route_key)
                    .bind(
                        serde_json::to_value(&state).map_err(|error| {
                            OrchestratorError::StoreInvariant(error.to_string())
                        })?,
                    )
                    .bind(i64::try_from(state.generation).map_err(|_| {
                        OrchestratorError::StoreInvariant(
                            "Earn MAX generation exceeds BIGINT".to_owned(),
                        )
                    })?)
                    .execute(&mut *tx)
                    .await?;
                }
            }
        }
        if observed_slot > cursor {
            sqlx::query(
                r#"
                UPDATE loyal_yield.projection_offsets
                SET last_event_id = $2, updated_at = now()
                WHERE consumer_name = $1
                "#,
            )
            .bind(consumer_name)
            .bind(observed_slot)
            .execute(&mut *tx)
            .await?;
        }
        tx.commit().await?;
        Ok(())
    }

    /// Applies one root-signed Earn MAX memo observed in a confirmed Smart
    /// Account transaction. The operation key is the chain location itself;
    /// replaying the same transaction is a no-op.
    pub async fn project_earn_max_intent(
        &self,
        input: EarnMaxIntentProjectionInput,
    ) -> Result<bool, OrchestratorError> {
        if input.settings.trim().is_empty() || input.signature.trim().is_empty() || input.slot == 0
        {
            return Err(OrchestratorError::StoreInvariant(
                "Earn MAX intent identity is malformed".to_owned(),
            ));
        }
        let idempotency_key = format!(
            "{MULTIPLY_ENGINE_VERSION}:intent:{}:{}",
            input.signature, input.instruction_index
        );
        let source_instruction_index = i32::from(input.instruction_index);
        let confirmed_slot =
            i64::try_from(input.slot).map_err(|_| OrchestratorError::SlotOutOfRange(input.slot))?;
        let mut tx = self.pool.begin().await?;
        let row = sqlx::query(
            r#"
            SELECT route_key, state, lease_owner, lease_expires_at
            FROM loyal_yield.multiply_route_states
            WHERE settings=$1 AND vault_index=$2
            FOR UPDATE
            "#,
        )
        .bind(&input.settings)
        .bind(i16::from(input.vault_index))
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Earn MAX intent route is not projected yet".to_owned(),
            )
        })?;
        let duplicate = sqlx::query_scalar::<_, bool>(
            "SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE idempotency_key=$1)",
        )
        .bind(&idempotency_key)
        .fetch_one(&mut *tx)
        .await?;
        if duplicate {
            tx.rollback().await?;
            return Ok(false);
        }
        let lease_owner: Option<String> = row.try_get("lease_owner")?;
        let lease_expires_at: Option<DateTime<Utc>> = row.try_get("lease_expires_at")?;
        if lease_owner.is_some() && lease_expires_at.is_some_and(|expiry| expiry > Utc::now()) {
            return Err(OrchestratorError::StoreInvariant(
                "Earn MAX intent route is actively leased; retry projection".to_owned(),
            ));
        }
        let route_key: String = row.try_get("route_key")?;
        let mut state: MultiplyRouteState = serde_json::from_value(row.try_get("state")?)
            .map_err(|error| OrchestratorError::StoreInvariant(error.to_string()))?;
        if input.slot < state.observed_slot {
            return Err(OrchestratorError::StoreInvariant(
                "Earn MAX intent is older than the projected route".to_owned(),
            ));
        }
        let (action, evidence) = match &input.intent {
            EarnMaxIntent::Withdraw {
                request_id,
                destination_account,
                amount_raw,
            } => {
                let amount_raw = match amount_raw {
                    Some(value) => *value,
                    None => {
                        let value = sqlx::query_scalar::<_, Option<String>>(
                            r#"
                            SELECT equity_usd_micros::text
                            FROM loyal_yield.multiply_position_snapshots
                            WHERE route_key=$1
                            ORDER BY observed_slot DESC, id DESC
                            LIMIT 1
                            "#,
                        )
                        .bind(&route_key)
                        .fetch_optional(&mut *tx)
                        .await?
                        .flatten()
                        .ok_or_else(|| {
                            OrchestratorError::StoreInvariant(
                                "Earn MAX full withdrawal has no current equity".to_owned(),
                            )
                        })?;
                        value.parse::<u64>().map_err(|_| {
                            OrchestratorError::StoreInvariant(
                                "Earn MAX full withdrawal equity does not fit u64".to_owned(),
                            )
                        })?
                    }
                };
                state = state
                    .request_withdrawal(
                        request_id.clone(),
                        destination_account.clone(),
                        amount_raw,
                        input.observed_at,
                    )
                    .map_err(|error| OrchestratorError::StoreInvariant(error.to_string()))?;
                (
                    "request_withdrawal",
                    json!({
                        "kind": "withdraw",
                        "requestId": request_id,
                        "destinationAccount": destination_account,
                        "amountRaw": amount_raw,
                    }),
                )
            }
            EarnMaxIntent::Cancel { request_id } => {
                state = state
                    .cancel_withdrawal(request_id)
                    .map_err(|error| OrchestratorError::StoreInvariant(error.to_string()))?;
                (
                    "cancel_withdrawal",
                    json!({ "kind": "cancel", "requestId": request_id }),
                )
            }
        };
        state.observed_slot = input.slot;
        state.observed_at = input.observed_at;
        state.frontend = project_frontend(&state);
        state
            .validate_persisted()
            .map_err(|error| OrchestratorError::StoreInvariant(error.to_string()))?;
        let evidence_bytes = serde_json::to_vec(&evidence)
            .map_err(|error| OrchestratorError::StoreInvariant(error.to_string()))?;
        let reconciliation_sha256 = format!("{:x}", Sha256::digest(&evidence_bytes));
        let operation_id = format!(
            "intent-{}",
            &format!("{:x}", Sha256::digest(idempotency_key.as_bytes()))[..32]
        );
        let changed = sqlx::query(
            r#"
            UPDATE loyal_yield.multiply_route_states
            SET state=$2, state_version=$3, lease_owner=NULL,
                lease_expires_at=NULL, updated_at=now()
            WHERE route_key=$1
            "#,
        )
        .bind(&route_key)
        .bind(
            serde_json::to_value(&state)
                .map_err(|error| OrchestratorError::StoreInvariant(error.to_string()))?,
        )
        .bind(i64::try_from(state.generation).map_err(|_| {
            OrchestratorError::StoreInvariant("Earn MAX generation exceeds BIGINT".to_owned())
        })?)
        .execute(&mut *tx)
        .await?;
        if changed.rows_affected() != 1 {
            return Err(OrchestratorError::StoreInvariant(
                "Earn MAX intent route update missed".to_owned(),
            ));
        }
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.multiply_operations (
                operation_id, route_key, cycle, engine_version, action, status,
                idempotency_key, expected_effects, transaction_signature,
                source_instruction_index, confirmed_slot, reconciliation_sha256,
                created_at, updated_at
            ) VALUES (
                $1, $2, $3, $4, $5, 'reconciled',
                $6, $7, $8, $9, $10, $11, $12, $12
            )
            "#,
        )
        .bind(operation_id)
        .bind(&route_key)
        .bind(i64::try_from(state.cycle).map_err(|_| {
            OrchestratorError::StoreInvariant("Earn MAX cycle exceeds BIGINT".to_owned())
        })?)
        .bind(MULTIPLY_ENGINE_VERSION)
        .bind(action)
        .bind(idempotency_key)
        .bind(json!({ "tokenDeltas": [], "obligationDelta": null, "intent": evidence }))
        .bind(&input.signature)
        .bind(source_instruction_index)
        .bind(confirmed_slot)
        .bind(reconciliation_sha256)
        .bind(input.observed_at)
        .execute(&mut *tx)
        .await?;
        tx.commit().await?;
        Ok(true)
    }

    pub async fn advance_projection_offset(
        &self,
        consumer_name: &str,
        event_id: i64,
    ) -> Result<(), OrchestratorError> {
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.projection_offsets (consumer_name, last_event_id)
            VALUES ($1, $2)
            ON CONFLICT (consumer_name) DO UPDATE SET
                last_event_id = GREATEST(loyal_yield.projection_offsets.last_event_id, EXCLUDED.last_event_id),
                updated_at = now()
            "#,
        )
        .bind(consumer_name)
        .bind(event_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn record_wallet_ata_balance_update_in_connection(
        connection: &mut PgConnection,
        input: WalletAtaBalanceUpdateInput,
    ) -> Result<WalletAtaBalanceCurrent, OrchestratorError> {
        record_wallet_ata_balance_update_in_tx(connection, input).await
    }

    pub async fn pending_balance_sweep_surplus_lots(
        &self,
        target_id: BalanceSweepTargetId,
    ) -> Result<Vec<PendingBalanceSweepSurplusLot>, OrchestratorError> {
        let rows = sqlx::query(
            r#"
            SELECT
                id, target_id, source_event_id, source_signature, classification,
                source_mint, source_wallet_token_ata, original_amount_raw,
                remaining_amount_raw, eligible_after, status, confidence, reason,
                created_at, updated_at
            FROM loyal_yield.pending_balance_sweep_surplus_lots
            WHERE target_id = $1
            ORDER BY eligible_after ASC, created_at ASC, id ASC
            "#,
        )
        .bind(target_id.as_i64())
        .fetch_all(&self.pool)
        .await?;

        rows.into_iter()
            .map(|row| {
                Ok(PendingBalanceSweepSurplusLot {
                    id: row.try_get("id")?,
                    target_id: BalanceSweepTargetId(row.try_get("target_id")?),
                    source_event_id: row.try_get("source_event_id")?,
                    source_signature: row.try_get("source_signature")?,
                    source_mint: row.try_get("source_mint")?,
                    source_wallet_token_ata: row.try_get("source_wallet_token_ata")?,
                    classification: row.try_get("classification")?,
                    original_amount_raw: row.try_get("original_amount_raw")?,
                    remaining_amount_raw: row.try_get("remaining_amount_raw")?,
                    eligible_after: row.try_get("eligible_after")?,
                    status: row.try_get("status")?,
                    confidence: row.try_get("confidence")?,
                    reason: row.try_get("reason")?,
                    created_at: row.try_get("created_at")?,
                    updated_at: row.try_get("updated_at")?,
                })
            })
            .collect()
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
                (target_id, signature, slot, source_wallet_ata, destination_vault_ata,
                 token_mint, source_token_ata, destination_token_ata,
                 amount_raw, source_pre_balance_raw, source_post_balance_raw,
                 destination_pre_balance_raw, destination_post_balance_raw, source_commitment,
                 raw_evidence, decoded_evidence, received_at, decoded_at, dedupe_key)
            VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
            ON CONFLICT (dedupe_key) DO UPDATE SET dedupe_key = EXCLUDED.dedupe_key
            RETURNING
                id, target_id, signature, slot,
                COALESCE(source_wallet_ata, source_token_ata) AS source_wallet_ata,
                COALESCE(destination_vault_ata, destination_token_ata) AS destination_vault_ata,
                token_mint, source_token_ata, destination_token_ata,
                amount_raw, source_pre_balance_raw, source_post_balance_raw,
                destination_pre_balance_raw, destination_post_balance_raw, source_commitment,
                raw_evidence, decoded_evidence, received_at, decoded_at, inserted_at, dedupe_key
            "#,
        )
        .bind(input.target_id.as_i64())
        .bind(&input.signature)
        .bind(slot)
        .bind(&input.source_wallet_ata)
        .bind(&input.destination_vault_ata)
        .bind(&input.token_mint)
        .bind(&input.source_token_ata)
        .bind(&input.destination_token_ata)
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

    pub async fn current_idle_token_balance(
        &self,
        vault_id: VaultId,
        mint: &str,
    ) -> Result<Option<CurrentIdleTokenBalance>, OrchestratorError> {
        let row = sqlx::query(
            r#"
            SELECT
                vault_id,
                mint,
                amount_raw,
                owner,
                token_account,
                observed_slot,
                observed_at,
                source_commitment,
                updated_at
            FROM loyal_yield.vault_idle_token_balances_current
            WHERE vault_id = $1
              AND mint = $2
            "#,
        )
        .bind(vault_id.as_i64())
        .bind(mint)
        .fetch_optional(&self.pool)
        .await?;

        row.map(|row| current_idle_token_balance_from_row(&row))
            .transpose()
    }

    pub async fn current_idle_token_balances_for_vaults(
        &self,
        vault_ids: &[VaultId],
        mint: &str,
    ) -> Result<Vec<CurrentIdleTokenBalance>, OrchestratorError> {
        if vault_ids.is_empty() {
            return Ok(Vec::new());
        }
        let ids = vault_ids.iter().map(|id| id.as_i64()).collect::<Vec<_>>();
        let rows = sqlx::query(
            r#"
            SELECT
                idle.vault_id,
                idle.mint,
                idle.amount_raw,
                idle.owner,
                idle.token_account,
                idle.observed_slot,
                idle.observed_at,
                idle.source_commitment,
                idle.updated_at
            FROM loyal_yield.vault_idle_token_balances_current AS idle
            WHERE idle.vault_id = ANY($1)
              AND idle.mint = $2
              AND NOT EXISTS (
                SELECT 1
                FROM loyal_yield.balance_sweep_lot_claims AS direct_claim
                JOIN loyal_yield.balance_sweep_targets AS direct_target
                  ON direct_target.id = direct_claim.target_id
                 AND direct_target.token_mint = idle.mint
                JOIN loyal_yield.managed_vaults AS direct_vault
                  ON direct_vault.settings = direct_target.settings
                 AND direct_vault.vault_index = direct_target.vault_index
                 AND direct_vault.vault_pubkey = direct_target.vault_pubkey
                 AND direct_vault.id = idle.vault_id
                JOIN loyal_yield.balance_sweep_transaction_attempts AS direct_pull
                  ON direct_pull.claim_token = direct_claim.claim_token
                 AND direct_pull.operation_kind = 'pull'
                 AND direct_pull.attempt_state = 'confirmed'
                LEFT JOIN loyal_yield.balance_sweep_transaction_attempts AS direct_top_up
                  ON direct_top_up.claim_token = direct_claim.claim_token
                 AND direct_top_up.operation_kind = 'top_up'
                 AND direct_top_up.attempt_state = 'confirmed'
                WHERE direct_claim.status = 'selected'
                  AND direct_top_up.id IS NULL
              )
            ORDER BY idle.vault_id, idle.mint
            "#,
        )
        .bind(&ids)
        .bind(mint)
        .fetch_all(&self.pool)
        .await?;

        rows.iter()
            .map(current_idle_token_balance_from_row)
            .collect()
    }

    pub async fn record_current_idle_token_balance(
        &self,
        balance: CurrentIdleTokenBalance,
    ) -> Result<CurrentIdleTokenBalance, OrchestratorError> {
        let mut connection = self.pool.acquire().await?;
        upsert_current_idle_token_balance(&mut connection, &balance).await
    }

    pub async fn apply_observed_patch(
        &self,
        vault_id: VaultId,
        state: ReconciledVaultState,
    ) -> Result<PositionSnapshot, OrchestratorError> {
        self.reconcile_vault_transaction(
            vault_id,
            state,
            Vec::new(),
            VaultPublicationScope::ObservedSubset,
        )
        .await
    }

    /// Atomically records a bounded reserve/idle observation. This is
    /// intentionally non-destructive and cannot become a complete planning
    /// epoch unless a later complete publication observes every required row
    /// at the same slot.
    pub async fn apply_observed_patch_with_idle_token_balances(
        &self,
        vault_id: VaultId,
        state: ReconciledVaultState,
        idle_balances: Vec<CurrentIdleTokenBalance>,
    ) -> Result<PositionSnapshot, OrchestratorError> {
        if state.positions.is_empty() {
            return Err(OrchestratorError::EmptySnapshot);
        }
        validate_observed_idle_token_balances(vault_id, &state, None, &idle_balances)?;
        self.reconcile_vault_transaction(
            vault_id,
            state,
            idle_balances,
            VaultPublicationScope::ObservedSubset,
        )
        .await
    }

    /// Atomically publishes one reserve-position snapshot and the complete idle
    /// balance set derived from that same RPC observation. The planner must
    /// never observe a new position snapshot beside a partially refreshed set
    /// of mint rows.
    pub async fn publish_complete_vault(
        &self,
        vault_id: VaultId,
        state: ReconciledVaultState,
        idle_balances: Vec<CurrentIdleTokenBalance>,
    ) -> Result<PositionSnapshot, OrchestratorError> {
        if state.positions.is_empty() {
            return Err(OrchestratorError::EmptySnapshot);
        }
        validate_atomic_idle_token_balances(vault_id, &state, None, &idle_balances)?;
        self.reconcile_vault_transaction(
            vault_id,
            state,
            idle_balances,
            VaultPublicationScope::CompleteProductVault,
        )
        .await
    }

    async fn reconcile_vault_transaction(
        &self,
        vault_id: VaultId,
        mut state: ReconciledVaultState,
        idle_balances: Vec<CurrentIdleTokenBalance>,
        publication_scope: VaultPublicationScope,
    ) -> Result<PositionSnapshot, OrchestratorError> {
        if state.positions.is_empty() {
            return Err(OrchestratorError::EmptySnapshot);
        }

        let publication_scope_name = match publication_scope {
            VaultPublicationScope::ObservedSubset => "observed_subset",
            VaultPublicationScope::CompleteProductVault => "complete_product_vault",
        };
        match &mut state.context {
            Value::Object(context) => {
                context.insert(
                    "publication_scope".to_owned(),
                    Value::String(publication_scope_name.to_owned()),
                );
            }
            previous => {
                *previous = json!({
                    "publication_scope": publication_scope_name,
                    "source_context": previous.take(),
                });
            }
        }

        let mut tx = self.pool.begin().await?;
        let vault = fetch_managed_vault_for_update(&mut tx, vault_id).await?;
        validate_observed_idle_token_balances(
            vault_id,
            &state,
            Some(&vault.vault_pubkey),
            &idle_balances,
        )?;
        if publication_scope == VaultPublicationScope::CompleteProductVault {
            validate_atomic_idle_token_balances(
                vault_id,
                &state,
                Some(&vault.vault_pubkey),
                &idle_balances,
            )?;
        }

        // The managed-vault row serializes projectors for this vault. Reject an
        // older RPC response before changing either the current-snapshot flag
        // or the materialized positions; post-confirm reconciliation may race
        // a fresher stream observation and must never move state backwards.
        let current_snapshot = sqlx::query_as::<_, SnapshotRow>(
            r#"
            SELECT
                id,
                vault_id,
                policy_id,
                observed_slot,
                observed_at,
                chain_slot,
                lock_attempt_id,
                is_current,
                context
            FROM loyal_yield.vault_position_snapshots
            WHERE vault_id = $1 AND is_current
            ORDER BY observed_slot DESC, id DESC
            LIMIT 1
            FOR UPDATE
            "#,
        )
        .bind(vault_id.as_i64())
        .fetch_optional(&mut *tx)
        .await?;
        if let Some(current) = current_snapshot {
            if state.observed_slot < current.observed_slot {
                return Err(OrchestratorError::StaleVaultObservation {
                    vault_id,
                    observed_slot: state.observed_slot,
                    current_slot: current.observed_slot,
                });
            }
            if state.observed_slot == current.observed_slot {
                let positions = current_positions_for_update(&mut tx, vault_id).await?;
                let positions_match = match publication_scope {
                    VaultPublicationScope::CompleteProductVault => {
                        reconciled_positions_equal(&state.positions, &positions)?
                    }
                    VaultPublicationScope::ObservedSubset => {
                        reconciled_positions_are_subset(&state.positions, &positions)?
                    }
                };
                if positions_match {
                    match publication_scope {
                        VaultPublicationScope::CompleteProductVault => {
                            validate_same_slot_atomic_idle_set(&mut tx, vault_id, &idle_balances)
                                .await?;
                        }
                        VaultPublicationScope::ObservedSubset => {
                            validate_same_slot_idle_subset(&mut tx, vault_id, &idle_balances)
                                .await?;
                        }
                    }
                    let snapshot = PositionSnapshot {
                        id: SnapshotId(current.id),
                        vault_id: VaultId(current.vault_id),
                        policy_id: PolicyId(current.policy_id),
                        observed_slot: current.observed_slot,
                        observed_at: current.observed_at,
                        chain_slot: current.chain_slot,
                        lock_attempt_id: current.lock_attempt_id,
                        is_current: current.is_current,
                        context: current.context,
                    };
                    tx.commit().await?;
                    return Ok(snapshot);
                }
                return Err(OrchestratorError::ConflictingVaultObservation {
                    vault_id,
                    observed_slot: state.observed_slot,
                });
            }
        }

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

        if publication_scope == VaultPublicationScope::CompleteProductVault {
            sqlx::query(
                r#"
                DELETE FROM loyal_yield.vault_reserve_positions_current
                WHERE vault_id = $1 AND NOT (reserve = ANY($2))
                "#,
            )
            .bind(vault_id.as_i64())
            .bind(&observed_reserves)
            .execute(&mut *tx)
            .await?;

            close_zero_user_yield_positions_for_vault(
                &mut tx,
                &vault,
                SnapshotId(snapshot_row.id),
                snapshot_row.observed_slot,
                snapshot_row.observed_at,
                &snapshot_row.context,
            )
            .await?;

            upsert_atomic_idle_token_balances(&mut tx, vault_id, &idle_balances).await?;
        } else {
            for balance in &idle_balances {
                upsert_current_idle_token_balance(&mut tx, balance).await?;
            }
        }
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
        let _ = fetch_managed_vault_for_update(&mut tx, vault_id).await?;

        if active_decision_exists(&mut tx, vault_id).await? {
            let decision =
                insert_skipped_decision(&mut tx, vault_id, SkipReason::ActiveDecision).await?;
            tx.commit().await?;
            return Ok(PlanOutcome::skipped(
                vault_id,
                SkipReason::ActiveDecision,
                Some(from_row_to_decision(decision)?),
            ));
        }

        let positions = current_positions_for_update(&mut tx, vault_id).await?;
        let planned = match draft_same_mint_decision(&positions, &reserve_scores, config) {
            Ok(value) => value,
            Err(reason) => {
                let decision = insert_skipped_decision(&mut tx, vault_id, reason).await?;
                tx.commit().await?;
                return Ok(PlanOutcome::skipped(
                    vault_id,
                    reason,
                    Some(from_row_to_decision(decision)?),
                ));
            }
        };

        let row =
            insert_planned_decision(&mut tx, vault_id, &planned, config.estimated_cost_lamports)
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
        let _ = fetch_managed_vault_for_update(&mut tx, vault_id).await?;

        if active_decision_exists(&mut tx, vault_id).await? {
            let decision =
                insert_skipped_decision(&mut tx, vault_id, SkipReason::ActiveDecision).await?;
            tx.commit().await?;
            return Ok(PlanOutcome::skipped(
                vault_id,
                SkipReason::ActiveDecision,
                Some(from_row_to_decision(decision)?),
            ));
        }

        validate_planned_decision_input(&input)?;
        let liquidity_mint = if input.source_liquidity_mint == input.target_liquidity_mint {
            Some(input.source_liquidity_mint.clone())
        } else {
            None
        };
        let planned = PlannedDecision {
            source_snapshot_id: Some(input.source_snapshot_id),
            source_reserve: Some(input.source_reserve),
            target_reserve: input.target_reserve,
            liquidity_mint,
            source_liquidity_mint: input.source_liquidity_mint,
            target_liquidity_mint: input.target_liquidity_mint,
            amount_raw: input.amount_raw,
            source_apy_bps: input.source_apy_bps,
            target_apy_bps: input.target_apy_bps,
            estimated_edge_bps: input.estimated_edge_bps,
            route_amount_semantics: input
                .execution_plan
                .get("route_amount_semantics")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned(),
            source_amount_semantics: input
                .execution_plan
                .get("source_amount_semantics")
                .and_then(Value::as_str)
                .map(ToOwned::to_owned),
            source_collateral_amount_raw: json_i64(
                &input.execution_plan,
                "source_collateral_amount_raw",
            ),
            redeemable_source_liquidity_amount_raw: json_i64(
                &input.execution_plan,
                "redeemable_source_liquidity_amount_raw",
            ),
            idle_vault_liquidity_amount_raw: json_i64(
                &input.execution_plan,
                "idle_vault_liquidity_amount_raw",
            ),
            decision_reason: DecisionReason::TargetSupplyApyExceedsSource,
            execution_plan: input.execution_plan,
        };

        let row =
            insert_planned_decision(&mut tx, vault_id, &planned, input.estimated_cost_lamports)
                .await?;
        let decision = from_row_to_decision(row)?;
        tx.commit().await?;
        Ok(PlanOutcome::planned(vault_id, decision))
    }

    pub async fn record_idle_vault_deposit_decision(
        &self,
        vault_id: VaultId,
        input: IdleVaultDepositDecisionInput,
    ) -> Result<PlanOutcome, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let _ = fetch_managed_vault_for_update(&mut tx, vault_id).await?;

        if active_decision_exists(&mut tx, vault_id).await? {
            let decision =
                insert_skipped_decision(&mut tx, vault_id, SkipReason::ActiveDecision).await?;
            tx.commit().await?;
            return Ok(PlanOutcome::skipped(
                vault_id,
                SkipReason::ActiveDecision,
                Some(from_row_to_decision(decision)?),
            ));
        }

        validate_idle_vault_deposit_decision_input(&input)?;
        let execution_plan = idle_vault_deposit_execution_plan(&input);
        let planned = PlannedDecision {
            source_snapshot_id: None,
            source_reserve: None,
            target_reserve: input.target_reserve,
            liquidity_mint: Some(input.liquidity_mint.clone()),
            source_liquidity_mint: input.liquidity_mint.clone(),
            target_liquidity_mint: input.liquidity_mint,
            amount_raw: input.amount_raw,
            source_apy_bps: 0,
            target_apy_bps: input.target_apy_bps,
            estimated_edge_bps: input.estimated_edge_bps,
            route_amount_semantics: ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned(),
            source_amount_semantics: Some("idle_vault".to_owned()),
            source_collateral_amount_raw: None,
            redeemable_source_liquidity_amount_raw: None,
            idle_vault_liquidity_amount_raw: Some(input.amount_raw),
            decision_reason: DecisionReason::IdleVaultLiquidityAvailable,
            execution_plan,
        };

        let row =
            insert_planned_decision(&mut tx, vault_id, &planned, input.estimated_cost_lamports)
                .await?;
        let decision = from_row_to_decision(row)?;
        tx.commit().await?;
        Ok(PlanOutcome::planned(vault_id, decision))
    }

    /// Persists the exact signed fleet transaction and creates its movement
    /// decision in one database transaction. No decision-less signed route is
    /// externally visible, and any validation/link failure rolls back both.
    pub async fn record_idle_vault_deposit_decision_with_signed_submission(
        &self,
        vault_id: VaultId,
        input: IdleVaultDepositDecisionInput,
        opportunity_lease: &RebalanceOpportunityLease,
        capacity_input: TargetCapacityReservationInput,
        signed_input: SignedRouteSubmissionInput,
    ) -> Result<(PlanOutcome, SignedRouteSubmissionRecord), OrchestratorError> {
        if signed_input.decision_id.is_some() {
            return Err(OrchestratorError::StoreInvariant(
                "atomic fleet handoff requires an unlinked signed submission".to_owned(),
            ));
        }
        let mut tx = self.pool.begin().await?;
        let vault = fetch_managed_vault_for_update(&mut tx, vault_id).await?;
        if active_decision_exists(&mut tx, vault_id).await? {
            return Err(OrchestratorError::StoreInvariant(format!(
                "vault {vault_id} acquired an active decision before atomic fleet handoff"
            )));
        }

        validate_idle_vault_deposit_decision_input(&input)?;
        validate_idle_vault_source_for_update(&mut tx, &vault, &input).await?;
        NeonSqlClient::reserve_target_capacity_in_connection(
            &mut tx,
            opportunity_lease,
            &capacity_input,
            signed_input.compiled_fee_lamports,
        )
        .await?;
        let execution_plan = idle_vault_deposit_execution_plan(&input);
        let planned = PlannedDecision {
            source_snapshot_id: None,
            source_reserve: None,
            target_reserve: input.target_reserve,
            liquidity_mint: Some(input.liquidity_mint.clone()),
            source_liquidity_mint: input.liquidity_mint.clone(),
            target_liquidity_mint: input.liquidity_mint,
            amount_raw: input.amount_raw,
            source_apy_bps: 0,
            target_apy_bps: input.target_apy_bps,
            estimated_edge_bps: input.estimated_edge_bps,
            route_amount_semantics: ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned(),
            source_amount_semantics: Some("idle_vault".to_owned()),
            source_collateral_amount_raw: None,
            redeemable_source_liquidity_amount_raw: None,
            idle_vault_liquidity_amount_raw: Some(input.amount_raw),
            decision_reason: DecisionReason::IdleVaultLiquidityAvailable,
            execution_plan,
        };
        let mut submission = NeonSqlClient::persist_signed_route_submission_in_connection(
            &mut tx,
            opportunity_lease,
            &signed_input,
        )
        .await?;
        let row =
            insert_planned_decision(&mut tx, vault_id, &planned, input.estimated_cost_lamports)
                .await?;
        let decision = from_row_to_decision(row)?;
        if decision.status != DecisionStatus::Planned {
            return Err(OrchestratorError::StoreInvariant(
                "atomic idle fleet handoff matched a non-planned decision".to_owned(),
            ));
        }
        let linked_decision_id: Option<i64> = sqlx::query_scalar(
            "SELECT decision_id FROM loyal_yield.signed_route_submissions WHERE id = $1",
        )
        .bind(submission.id)
        .fetch_one(&mut *tx)
        .await?;
        if linked_decision_id != Some(decision.id.as_i64()) {
            return Err(OrchestratorError::StoreInvariant(
                "atomic idle fleet handoff did not link its signed submission".to_owned(),
            ));
        }
        NeonSqlClient::attach_target_capacity_reservation_in_connection(
            &mut tx,
            opportunity_lease,
            decision.id,
            submission.id,
        )
        .await?;
        submission.decision_id = Some(decision.id);
        if let Err(error) = NeonSqlClient::assert_signed_route_publication_lifetime_in_connection(
            &mut tx,
            opportunity_lease.opportunity.id,
            decision.id,
            submission.id,
        )
        .await
        {
            tx.rollback().await?;
            return Err(error);
        }
        tx.commit().await?;
        Ok((PlanOutcome::planned(vault_id, decision), submission))
    }

    /// Atomically links one exact Voltr manager transaction to the existing
    /// generic decision/submission lifecycle. Voltr state is revalidated by
    /// the route adapter; this method deliberately creates no second outbox,
    /// movement graph, or target-capacity reservation.
    pub async fn record_voltr_manager_decision_with_signed_submission(
        &self,
        opportunity_lease: &RebalanceOpportunityLease,
        signed_input: SignedRouteSubmissionInput,
    ) -> Result<(PlanOutcome, SignedRouteSubmissionRecord), OrchestratorError> {
        let opportunity = &opportunity_lease.opportunity;
        if opportunity
            .execution_plan
            .get("kind")
            .and_then(Value::as_str)
            != Some("voltr_kamino")
            || signed_input.decision_id.is_some()
            || signed_input.opportunity_id != opportunity.id
        {
            return Err(OrchestratorError::StoreInvariant(
                "atomic Voltr handoff requires one unlinked voltr_kamino signed submission"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool.begin().await?;
        let _vault = fetch_managed_vault_for_update(&mut tx, opportunity.vault_id).await?;
        if active_decision_exists(&mut tx, opportunity.vault_id).await? {
            return Err(OrchestratorError::StoreInvariant(format!(
                "vault {} acquired an active decision before atomic Voltr handoff",
                opportunity.vault_id
            )));
        }
        let planned = PlannedDecision {
            source_snapshot_id: opportunity.source_snapshot_id,
            source_reserve: opportunity.source_reserve.clone(),
            target_reserve: opportunity.target_reserve.clone(),
            liquidity_mint: Some(opportunity.liquidity_mint.clone()),
            source_liquidity_mint: opportunity.source_liquidity_mint.clone(),
            target_liquidity_mint: opportunity.target_liquidity_mint.clone(),
            amount_raw: opportunity.amount_raw,
            source_apy_bps: opportunity.source_apy_bps,
            target_apy_bps: opportunity.target_apy_bps,
            estimated_edge_bps: opportunity.estimated_edge_bps,
            route_amount_semantics: "voltr_manager_asset_amount".to_owned(),
            source_amount_semantics: Some("voltr_confirmed_position_value".to_owned()),
            source_collateral_amount_raw: None,
            redeemable_source_liquidity_amount_raw: None,
            idle_vault_liquidity_amount_raw: None,
            decision_reason: DecisionReason::VoltrManagerOperation,
            execution_plan: opportunity.execution_plan.clone(),
        };
        let mut submission = NeonSqlClient::persist_signed_route_submission_in_connection(
            &mut tx,
            opportunity_lease,
            &signed_input,
        )
        .await?;
        let row = insert_planned_decision(
            &mut tx,
            opportunity.vault_id,
            &planned,
            opportunity.estimated_cost_lamports,
        )
        .await?;
        let decision = from_row_to_decision(row)?;
        if decision.status != DecisionStatus::Planned {
            return Err(OrchestratorError::StoreInvariant(
                "atomic Voltr handoff matched a non-planned decision".to_owned(),
            ));
        }
        let linked_decision_id: Option<i64> = sqlx::query_scalar(
            "SELECT decision_id FROM loyal_yield.signed_route_submissions WHERE id = $1",
        )
        .bind(submission.id)
        .fetch_one(&mut *tx)
        .await?;
        if linked_decision_id != Some(decision.id.as_i64()) {
            return Err(OrchestratorError::StoreInvariant(
                "atomic Voltr handoff did not link its signed submission".to_owned(),
            ));
        }
        submission.decision_id = Some(decision.id);
        if let Err(error) = NeonSqlClient::assert_signed_route_publication_lifetime_in_connection(
            &mut tx,
            opportunity.id,
            decision.id,
            submission.id,
        )
        .await
        {
            tx.rollback().await?;
            return Err(error);
        }
        tx.commit().await?;
        Ok((
            PlanOutcome::planned(opportunity.vault_id, decision),
            submission,
        ))
    }

    pub async fn prepare_same_mint_rebalance(
        &self,
        input: SameMintRebalanceInput,
    ) -> Result<SameMintRebalanceResult, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let vault = fetch_rebalance_input_vault_for_update(&mut tx, &input).await?;
        let vault_id = vault.id;

        if active_decision_exists(&mut tx, vault_id).await? {
            let decision =
                insert_skipped_decision(&mut tx, vault_id, SkipReason::ActiveDecision).await?;
            let decision = from_row_to_decision(decision)?;
            tx.commit().await?;
            return Ok(same_mint_result_from_decision(
                vault_id,
                input,
                decision,
                Some(SkipReason::ActiveDecision),
                None,
            ));
        }

        let positions = current_positions_for_update(&mut tx, vault_id).await?;
        if let Err(reason) = validate_same_mint_input(&input, &positions, None) {
            tx.commit().await?;
            return Ok(same_mint_error_result(vault_id, input, reason));
        }

        let planned = PlannedDecision {
            source_snapshot_id: Some(input.expected_source_snapshot_id),
            source_reserve: Some(input.source_reserve.clone()),
            target_reserve: input.target_reserve.clone(),
            liquidity_mint: Some(input.liquidity_mint.clone()),
            source_liquidity_mint: input.liquidity_mint.clone(),
            target_liquidity_mint: input.liquidity_mint.clone(),
            amount_raw: input.amount_raw,
            source_apy_bps: input.source_apy_bps,
            target_apy_bps: input.target_apy_bps,
            estimated_edge_bps: input.estimated_edge_bps,
            route_amount_semantics: input.route_amount_semantics.clone(),
            source_amount_semantics: input.source_amount_semantics.clone(),
            source_collateral_amount_raw: input.source_collateral_amount_raw,
            redeemable_source_liquidity_amount_raw: input.redeemable_source_liquidity_amount_raw,
            idle_vault_liquidity_amount_raw: input.idle_vault_liquidity_amount_raw,
            decision_reason: DecisionReason::TargetSupplyApyExceedsSource,
            execution_plan: same_mint_execution_plan(&input),
        };
        let row =
            insert_planned_decision(&mut tx, vault_id, &planned, input.estimated_cost_lamports)
                .await?;
        let decision = from_row_to_decision(row)?;
        tx.commit().await?;
        Ok(same_mint_result_from_decision(
            vault_id,
            input,
            decision,
            None,
            Some(same_mint_execution_preview(&planned)),
        ))
    }

    /// Atomic fleet handoff for a fully built same-mint route. The current
    /// source snapshot is locked and revalidated before the signed bytes and
    /// decision become visible together.
    pub async fn prepare_same_mint_rebalance_with_signed_submission(
        &self,
        input: SameMintRebalanceInput,
        opportunity_lease: &RebalanceOpportunityLease,
        capacity_input: TargetCapacityReservationInput,
        signed_input: SignedRouteSubmissionInput,
    ) -> Result<(SameMintRebalanceResult, SignedRouteSubmissionRecord), OrchestratorError> {
        if signed_input.decision_id.is_some() {
            return Err(OrchestratorError::StoreInvariant(
                "atomic fleet handoff requires an unlinked signed submission".to_owned(),
            ));
        }
        let mut tx = self.pool.begin().await?;
        let vault = fetch_rebalance_input_vault_for_update(&mut tx, &input).await?;
        let vault_id = vault.id;
        if active_decision_exists(&mut tx, vault_id).await? {
            return Err(OrchestratorError::StoreInvariant(format!(
                "vault {vault_id} acquired an active decision before atomic fleet handoff"
            )));
        }
        let positions = current_positions_for_update(&mut tx, vault_id).await?;
        if let Err(reason) =
            validate_same_mint_input(&input, &positions, Some(&opportunity_lease.opportunity))
        {
            return Err(OrchestratorError::SameMintRebalanceValidation(reason));
        }

        NeonSqlClient::reserve_target_capacity_in_connection(
            &mut tx,
            opportunity_lease,
            &capacity_input,
            signed_input.compiled_fee_lamports,
        )
        .await?;
        let planned = PlannedDecision {
            source_snapshot_id: Some(input.expected_source_snapshot_id),
            source_reserve: Some(input.source_reserve.clone()),
            target_reserve: input.target_reserve.clone(),
            liquidity_mint: Some(input.liquidity_mint.clone()),
            source_liquidity_mint: input.liquidity_mint.clone(),
            target_liquidity_mint: input.liquidity_mint.clone(),
            amount_raw: input.amount_raw,
            source_apy_bps: input.source_apy_bps,
            target_apy_bps: input.target_apy_bps,
            estimated_edge_bps: input.estimated_edge_bps,
            route_amount_semantics: input.route_amount_semantics.clone(),
            source_amount_semantics: input.source_amount_semantics.clone(),
            source_collateral_amount_raw: input.source_collateral_amount_raw,
            redeemable_source_liquidity_amount_raw: input.redeemable_source_liquidity_amount_raw,
            idle_vault_liquidity_amount_raw: input.idle_vault_liquidity_amount_raw,
            decision_reason: DecisionReason::TargetSupplyApyExceedsSource,
            execution_plan: same_mint_execution_plan(&input),
        };
        let mut submission = NeonSqlClient::persist_signed_route_submission_in_connection(
            &mut tx,
            opportunity_lease,
            &signed_input,
        )
        .await?;
        let row =
            insert_planned_decision(&mut tx, vault_id, &planned, input.estimated_cost_lamports)
                .await?;
        let decision = from_row_to_decision(row)?;
        if decision.status != DecisionStatus::Planned {
            return Err(OrchestratorError::StoreInvariant(
                "atomic same-mint fleet handoff matched a non-planned decision".to_owned(),
            ));
        }
        let linked_decision_id: Option<i64> = sqlx::query_scalar(
            "SELECT decision_id FROM loyal_yield.signed_route_submissions WHERE id = $1",
        )
        .bind(submission.id)
        .fetch_one(&mut *tx)
        .await?;
        if linked_decision_id != Some(decision.id.as_i64()) {
            return Err(OrchestratorError::StoreInvariant(
                "atomic same-mint fleet handoff did not link its signed submission".to_owned(),
            ));
        }
        NeonSqlClient::attach_target_capacity_reservation_in_connection(
            &mut tx,
            opportunity_lease,
            decision.id,
            submission.id,
        )
        .await?;
        submission.decision_id = Some(decision.id);
        if let Err(error) = NeonSqlClient::assert_signed_route_publication_lifetime_in_connection(
            &mut tx,
            opportunity_lease.opportunity.id,
            decision.id,
            submission.id,
        )
        .await
        {
            tx.rollback().await?;
            return Err(error);
        }
        tx.commit().await?;
        let result = same_mint_result_from_decision(
            vault_id,
            input,
            decision,
            None,
            Some(same_mint_execution_preview(&planned)),
        );
        Ok((result, submission))
    }

    pub async fn prepare_same_mint_rebalance_batch(
        &self,
        inputs: Vec<SameMintRebalanceInput>,
    ) -> Vec<Result<SameMintRebalanceResult, OrchestratorError>> {
        let mut results = Vec::with_capacity(inputs.len());
        for input in inputs {
            results.push(self.prepare_same_mint_rebalance(input).await);
        }
        results
    }

    pub async fn confirm_same_mint_rebalance(
        &self,
        input: ConfirmSameMintRebalanceInput,
    ) -> Result<SameMintRebalanceResult, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let decision = fetch_decision_for_update(&mut tx, input.decision_id).await?;
        if decision.status == DecisionStatus::Confirmed {
            ensure_same_mint_confirm_repeat_matches(&decision, &input)?;
            tx.commit().await?;
            return Ok(same_mint_result_from_confirmed_decision(decision));
        }
        ensure_confirmable_same_mint_decision(&decision)?;
        ensure_same_mint_route_amount_semantics(&decision)?;
        let vault = fetch_managed_vault_for_update(&mut tx, decision.vault_id).await?;
        let current = current_positions_for_update(&mut tx, decision.vault_id).await?;
        let source_reserve = required_decision_field(&decision.source_reserve, "source_reserve")?;
        let target_reserve = required_decision_field(&decision.target_reserve, "target_reserve")?;
        let liquidity_mint = required_decision_field(&decision.liquidity_mint, "liquidity_mint")?;
        let amount_raw = decision
            .amount_raw
            .ok_or_else(|| OrchestratorError::StoreInvariant("missing amount_raw".to_owned()))?;

        if let Some(post_snapshot_id) = input.post_snapshot_id {
            let decision = update_confirmed_decision(
                &mut tx,
                input.decision_id,
                &input.signature,
                input.submitted_slot,
                input.confirmed_slot,
                post_snapshot_id,
            )
            .await?;
            tx.commit().await?;
            return Ok(same_mint_result_from_confirmed_decision(decision));
        }

        let mut next_positions = Vec::with_capacity(current.len());
        let mut saw_source = false;
        let mut saw_target = false;
        for mut position in current {
            if position.reserve == source_reserve {
                if position.liquidity_mint != liquidity_mint {
                    return Err(OrchestratorError::SameMintRebalanceValidation(format!(
                        "source reserve liquidity mint {} does not match decision mint {}",
                        position.liquidity_mint, liquidity_mint
                    )));
                }
                saw_source = true;
                position.amount_raw = 0;
                position.has_value = false;
                position.planning_metadata =
                    same_mint_projection_metadata(&decision, "source_after_confirm", 0);
            } else if position.reserve == target_reserve {
                if position.liquidity_mint != liquidity_mint {
                    return Err(OrchestratorError::SameMintRebalanceValidation(format!(
                        "target reserve liquidity mint {} does not match decision mint {}",
                        position.liquidity_mint, liquidity_mint
                    )));
                }
                saw_target = true;
                position.amount_raw = amount_raw;
                position.has_value = amount_raw > 0;
                position.planning_metadata =
                    same_mint_projection_metadata(&decision, "target_after_confirm", amount_raw);
            }
            next_positions.push(position);
        }
        if !saw_source || !saw_target {
            return Err(OrchestratorError::StoreInvariant(
                "same-mint confirm requires source and target current positions".to_owned(),
            ));
        }

        sqlx::query!(
            r#"
            UPDATE loyal_yield.vault_position_snapshots
            SET is_current = FALSE
            WHERE vault_id = $1 AND is_current
            "#,
            decision.vault_id.as_i64()
        )
        .execute(&mut *tx)
        .await?;

        let snapshot_row = sqlx::query_as!(
            SnapshotRow,
            r#"
            INSERT INTO loyal_yield.vault_position_snapshots
                (vault_id, policy_id, observed_slot, observed_at, chain_slot, context)
            VALUES ($1, $2, $3, COALESCE($4, now()), $5, $6)
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
            decision.vault_id.as_i64(),
            vault.active_policy_id.as_i64(),
            input.confirmed_slot,
            input.observed_at,
            input.confirmed_slot,
            json!({
                "kind": "same_mint_rebalance_confirmed",
                "decision_id": input.decision_id.as_i64(),
                "signature": input.signature,
            })
        )
        .fetch_one(&mut *tx)
        .await?;

        let mut observed_reserves = Vec::with_capacity(next_positions.len());
        for position in next_positions {
            observed_reserves.push(position.reserve.clone());
            sqlx::query!(
                r#"
                INSERT INTO loyal_yield.vault_position_snapshot_positions
                    (snapshot_id, reserve, market, liquidity_mint, amount_raw, supply_apy_bps, borrow_apy_bps, has_value, planning_metadata)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
                "#,
                snapshot_row.id,
                position.reserve,
                position.market,
                position.liquidity_mint,
                position.amount_raw,
                position.supply_apy_bps,
                position.borrow_apy_bps,
                position.has_value,
                position.planning_metadata
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
                decision.vault_id.as_i64(),
                position.reserve,
                position.market,
                position.liquidity_mint,
                position.amount_raw,
                position.has_value,
                position.supply_apy_bps,
                position.borrow_apy_bps,
                snapshot_row.id,
                snapshot_row.observed_slot,
                snapshot_row.observed_at,
                position.planning_metadata
            )
            .execute(&mut *tx)
            .await?;
        }

        sqlx::query!(
            r#"
            DELETE FROM loyal_yield.vault_reserve_positions_current
            WHERE vault_id = $1 AND NOT (reserve = ANY($2))
            "#,
            decision.vault_id.as_i64(),
            &observed_reserves
        )
        .execute(&mut *tx)
        .await?;

        let decision = update_confirmed_decision(
            &mut tx,
            input.decision_id,
            &input.signature,
            input.submitted_slot,
            input.confirmed_slot,
            SnapshotId(snapshot_row.id),
        )
        .await?;
        tx.commit().await?;
        Ok(same_mint_result_from_confirmed_decision(decision))
    }

    pub async fn advance_decision(
        &self,
        decision_id: DecisionId,
        advance: DecisionAdvance,
    ) -> Result<RebalanceDecision, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let decision = fetch_decision_for_update(&mut tx, decision_id).await?;
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

fn validate_atomic_idle_token_balances(
    vault_id: VaultId,
    state: &ReconciledVaultState,
    expected_owner: Option<&str>,
    idle_balances: &[CurrentIdleTokenBalance],
) -> Result<(), OrchestratorError> {
    validate_observed_idle_token_balances(vault_id, state, expected_owner, idle_balances)?;
    let product_mints = supported_idle_deposit_mints()
        .into_iter()
        .collect::<BTreeSet<_>>();
    let observed_mints = idle_balances
        .iter()
        .map(|balance| balance.mint.clone())
        .collect::<BTreeSet<_>>();
    if observed_mints != product_mints {
        return Err(OrchestratorError::StoreInvariant(format!(
            "complete product vault publication requires exactly {product_mints:?}, observed {observed_mints:?}"
        )));
    }
    Ok(())
}

fn validate_observed_idle_token_balances(
    vault_id: VaultId,
    state: &ReconciledVaultState,
    expected_owner: Option<&str>,
    idle_balances: &[CurrentIdleTokenBalance],
) -> Result<(), OrchestratorError> {
    let product_mints = supported_idle_deposit_mints()
        .into_iter()
        .collect::<BTreeSet<_>>();
    let mut observed_mints = BTreeSet::new();
    let mut token_accounts = BTreeSet::new();
    for balance in idle_balances {
        if balance.vault_id != vault_id {
            return Err(OrchestratorError::StoreInvariant(format!(
                "atomic idle balance vault {} does not match reconciled vault {}",
                balance.vault_id, vault_id
            )));
        }
        if balance.observed_slot != state.observed_slot {
            return Err(OrchestratorError::StoreInvariant(format!(
                "atomic idle balance slot {} does not match reconciled slot {}",
                balance.observed_slot, state.observed_slot
            )));
        }
        if balance.amount_raw < 0
            || balance.mint.is_empty()
            || balance.owner.is_empty()
            || balance.token_account.is_empty()
            || balance.source_commitment.is_empty()
        {
            return Err(OrchestratorError::StoreInvariant(
                "atomic idle balances require non-negative amounts and complete identity"
                    .to_owned(),
            ));
        }
        if !product_mints.contains(&balance.mint) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "atomic idle balance mint {} is not an Earn product mint",
                balance.mint
            )));
        }
        if expected_owner.is_some_and(|owner| owner != balance.owner) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "atomic idle balance owner {} does not match managed vault owner",
                balance.owner
            )));
        }
        if !observed_mints.insert(balance.mint.clone()) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "atomic idle balance set repeats mint {}",
                balance.mint
            )));
        }
        if !token_accounts.insert(balance.token_account.clone()) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "atomic idle balance set repeats token account {}",
                balance.token_account
            )));
        }
    }
    Ok(())
}

async fn upsert_atomic_idle_token_balances(
    tx: &mut Transaction<'_, Postgres>,
    vault_id: VaultId,
    idle_balances: &[CurrentIdleTokenBalance],
) -> Result<(), OrchestratorError> {
    for balance in idle_balances {
        let existing = sqlx::query(
            r#"
            SELECT
                vault_id,
                mint,
                amount_raw,
                owner,
                token_account,
                observed_slot,
                observed_at,
                source_commitment,
                updated_at
            FROM loyal_yield.vault_idle_token_balances_current
            WHERE vault_id = $1 AND mint = $2
            FOR UPDATE
            "#,
        )
        .bind(balance.vault_id.as_i64())
        .bind(&balance.mint)
        .fetch_optional(&mut **tx)
        .await?
        .map(|row| current_idle_token_balance_from_row(&row))
        .transpose()?;
        if let Some(existing) = existing {
            if existing.observed_slot > balance.observed_slot {
                return Err(OrchestratorError::StaleVaultObservation {
                    vault_id: balance.vault_id,
                    observed_slot: balance.observed_slot,
                    current_slot: existing.observed_slot,
                });
            }
            if existing.observed_slot == balance.observed_slot {
                validate_same_slot_atomic_idle_repeat(&existing, balance)?;
                continue;
            }
        }
        upsert_current_idle_token_balance(tx, balance).await?;
    }
    let observed_mints = idle_balances
        .iter()
        .map(|balance| balance.mint.as_str())
        .collect::<Vec<_>>();
    sqlx::query(
        r#"
        DELETE FROM loyal_yield.vault_idle_token_balances_current
        WHERE vault_id = $1
          AND NOT (mint = ANY($2::TEXT[]))
        "#,
    )
    .bind(vault_id.as_i64())
    .bind(&observed_mints)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

async fn validate_same_slot_atomic_idle_set(
    tx: &mut Transaction<'_, Postgres>,
    vault_id: VaultId,
    incoming: &[CurrentIdleTokenBalance],
) -> Result<(), OrchestratorError> {
    let rows = sqlx::query(
        r#"
        SELECT
            vault_id,
            mint,
            amount_raw,
            owner,
            token_account,
            observed_slot,
            observed_at,
            source_commitment,
            updated_at
        FROM loyal_yield.vault_idle_token_balances_current
        WHERE vault_id = $1
        ORDER BY mint
        FOR UPDATE
        "#,
    )
    .bind(vault_id.as_i64())
    .fetch_all(&mut **tx)
    .await?;
    let existing = rows
        .iter()
        .map(current_idle_token_balance_from_row)
        .collect::<Result<Vec<_>, _>>()?;
    if existing.len() != incoming.len() {
        return Err(OrchestratorError::StoreInvariant(format!(
            "atomic idle balance set for vault {vault_id} conflicts at repeated snapshot slot"
        )));
    }
    let incoming_by_mint = incoming
        .iter()
        .map(|balance| (balance.mint.as_str(), balance))
        .collect::<std::collections::BTreeMap<_, _>>();
    for balance in &existing {
        let Some(incoming) = incoming_by_mint.get(balance.mint.as_str()) else {
            return Err(OrchestratorError::StoreInvariant(format!(
                "atomic idle balance set for vault {vault_id} conflicts at repeated snapshot slot"
            )));
        };
        validate_same_slot_atomic_idle_repeat(balance, incoming)?;
    }
    Ok(())
}

async fn validate_same_slot_idle_subset(
    tx: &mut Transaction<'_, Postgres>,
    vault_id: VaultId,
    incoming: &[CurrentIdleTokenBalance],
) -> Result<(), OrchestratorError> {
    for balance in incoming {
        let existing = sqlx::query(
            r#"
            SELECT
                vault_id, mint, amount_raw, owner, token_account,
                observed_slot, observed_at, source_commitment, updated_at
            FROM loyal_yield.vault_idle_token_balances_current
            WHERE vault_id = $1 AND mint = $2
            FOR UPDATE
            "#,
        )
        .bind(vault_id.as_i64())
        .bind(&balance.mint)
        .fetch_optional(&mut **tx)
        .await?;
        if let Some(existing) = existing {
            validate_same_slot_atomic_idle_repeat(
                &current_idle_token_balance_from_row(&existing)?,
                balance,
            )?;
        }
    }
    Ok(())
}

fn validate_same_slot_atomic_idle_repeat(
    existing: &CurrentIdleTokenBalance,
    incoming: &CurrentIdleTokenBalance,
) -> Result<(), OrchestratorError> {
    if existing.vault_id != incoming.vault_id
        || existing.mint != incoming.mint
        || existing.amount_raw != incoming.amount_raw
        || existing.owner != incoming.owner
        || existing.token_account != incoming.token_account
        || existing.observed_slot != incoming.observed_slot
    {
        return Err(OrchestratorError::StoreInvariant(format!(
            "atomic idle balance for vault {} mint {} conflicts at observed slot {}",
            incoming.vault_id, incoming.mint, incoming.observed_slot
        )));
    }
    Ok(())
}

async fn upsert_current_idle_token_balance(
    connection: &mut PgConnection,
    balance: &CurrentIdleTokenBalance,
) -> Result<CurrentIdleTokenBalance, OrchestratorError> {
    let row = sqlx::query(
        r#"
        INSERT INTO loyal_yield.vault_idle_token_balances_current
            (vault_id, mint, amount_raw, owner, token_account, observed_slot, observed_at, source_commitment, updated_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
        ON CONFLICT (vault_id, mint) DO UPDATE SET
            amount_raw = CASE
                WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
                THEN EXCLUDED.amount_raw
                ELSE loyal_yield.vault_idle_token_balances_current.amount_raw
            END,
            owner = CASE
                WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
                THEN EXCLUDED.owner
                ELSE loyal_yield.vault_idle_token_balances_current.owner
            END,
            token_account = CASE
                WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
                THEN EXCLUDED.token_account
                ELSE loyal_yield.vault_idle_token_balances_current.token_account
            END,
            observed_slot = GREATEST(loyal_yield.vault_idle_token_balances_current.observed_slot, EXCLUDED.observed_slot),
            observed_at = CASE
                WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
                THEN EXCLUDED.observed_at
                ELSE loyal_yield.vault_idle_token_balances_current.observed_at
            END,
            source_commitment = CASE
                WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
                THEN EXCLUDED.source_commitment
                ELSE loyal_yield.vault_idle_token_balances_current.source_commitment
            END,
            updated_at = CASE
                WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
                THEN now()
                ELSE loyal_yield.vault_idle_token_balances_current.updated_at
            END
        RETURNING
            vault_id,
            mint,
            amount_raw,
            owner,
            token_account,
            observed_slot,
            observed_at,
            source_commitment,
            updated_at
        "#,
    )
    .bind(balance.vault_id.as_i64())
    .bind(&balance.mint)
    .bind(balance.amount_raw)
    .bind(&balance.owner)
    .bind(&balance.token_account)
    .bind(balance.observed_slot)
    .bind(balance.observed_at)
    .bind(&balance.source_commitment)
    .fetch_one(connection)
    .await?;

    current_idle_token_balance_from_row(&row)
}

fn reconciled_positions_equal(
    incoming: &[ReconciledReservePosition],
    current: &[CurrentReservePosition],
) -> Result<bool, OrchestratorError> {
    if incoming.len() != current.len() {
        return Ok(false);
    }
    for incoming_position in incoming {
        let Some(current_position) = current
            .iter()
            .find(|position| position.reserve == incoming_position.reserve)
        else {
            return Ok(false);
        };
        let amount_raw = to_i64_amount(incoming_position.amount_raw)?;
        if current_position.market != incoming_position.market
            || current_position.liquidity_mint != incoming_position.liquidity_mint
            || current_position.amount_raw != amount_raw
            || current_position.has_value != (amount_raw > 0)
            || current_position.supply_apy_bps != incoming_position.supply_apy_bps
            || current_position.borrow_apy_bps != incoming_position.borrow_apy_bps
            || current_position.planning_metadata != incoming_position.planning_metadata
        {
            return Ok(false);
        }
    }
    Ok(true)
}

fn reconciled_positions_are_subset(
    incoming: &[ReconciledReservePosition],
    current: &[CurrentReservePosition],
) -> Result<bool, OrchestratorError> {
    for incoming_position in incoming {
        let Some(current_position) = current
            .iter()
            .find(|position| position.reserve == incoming_position.reserve)
        else {
            return Ok(false);
        };
        if !reconciled_positions_equal(
            std::slice::from_ref(incoming_position),
            std::slice::from_ref(current_position),
        )? {
            return Ok(false);
        }
    }
    Ok(true)
}

struct StoreMigration {
    version: i64,
    name: &'static str,
    sql: &'static str,
    expected_checksum: Option<&'static str>,
}

async fn ensure_schema_migration_ledger(pool: &PgPool) -> Result<(), OrchestratorError> {
    sqlx::raw_sql("CREATE SCHEMA IF NOT EXISTS loyal_yield;")
        .execute(pool)
        .await?;
    sqlx::raw_sql(
        r#"
        CREATE TABLE IF NOT EXISTS loyal_yield.schema_migrations (
            version BIGINT PRIMARY KEY,
            name TEXT NOT NULL,
            checksum TEXT NOT NULL,
            applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
        );
        "#,
    )
    .execute(pool)
    .await?;
    Ok(())
}

async fn apply_store_migration(
    pool: &PgPool,
    migration: StoreMigration,
) -> Result<(), OrchestratorError> {
    let expected_checksum = migration
        .expected_checksum
        .map(ToOwned::to_owned)
        .unwrap_or_else(|| migration_checksum(migration.sql));
    let applied_checksum = sqlx::query_scalar::<_, String>(
        "SELECT checksum FROM loyal_yield.schema_migrations WHERE version = $1",
    )
    .bind(migration.version)
    .fetch_optional(pool)
    .await?;

    match applied_checksum {
        Some(applied) if applied == expected_checksum => return Ok(()),
        Some(_) => {
            return Err(OrchestratorError::StoreInvariant(format!(
                "migration {} {} was applied with a different checksum",
                migration.version, migration.name
            )));
        }
        None => {}
    }

    sqlx::raw_sql(migration.sql).execute(pool).await?;
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.schema_migrations (version, name, checksum)
        VALUES ($1, $2, $3)
        ON CONFLICT (version) DO UPDATE
        SET name = EXCLUDED.name,
            checksum = EXCLUDED.checksum
        "#,
    )
    .bind(migration.version)
    .bind(migration.name)
    .bind(expected_checksum)
    .execute(pool)
    .await?;
    Ok(())
}

fn migration_checksum(sql: &str) -> String {
    format!("{:x}", Sha256::digest(sql.as_bytes()))
}

fn validate_cross_mint_swap_policy_manifest_input(
    event: &CrossMintSwapPolicyManifestInput,
) -> Result<(), OrchestratorError> {
    commitment_rank(&event.source_commitment)?;
    if !matches!(event.mutation.as_str(), "create" | "update") {
        return Err(OrchestratorError::StoreInvariant(format!(
            "cross-mint manifest mutation must be create or update, got {:?}",
            event.mutation
        )));
    }
    if event.manifest_fingerprint.trim().is_empty()
        || event.manifest_fingerprint != event.manifest_fingerprint.trim()
    {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint manifest fingerprint must be non-empty and trimmed".to_owned(),
        ));
    }
    if !matches!(event.source_shard.as_str(), "classic" | "token_2022")
        || event.max_slippage_bps == 0
        || event.max_slippage_bps > 10_000
        || event.daily_source_mint_spending_cap == 0
    {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint policy has an invalid source shard, slippage, or daily cap".to_owned(),
        ));
    }
    if [
        event.signature.as_str(),
        event.cluster.as_str(),
        event.settings.as_str(),
        event.authority.as_str(),
        event.policy_account.as_str(),
        event.vault_pubkey.as_str(),
        event.delegated_signer.as_str(),
        event.source_shard.as_str(),
    ]
    .into_iter()
    .any(str::is_empty)
    {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint manifest identity fields must be non-empty".to_owned(),
        ));
    }

    Ok(())
}

fn validate_cross_mint_vault_opt_in_lookup(
    lookup: &CrossMintVaultOptInLookup,
) -> Result<(), OrchestratorError> {
    if lookup.cluster.trim().is_empty()
        || lookup.settings.trim().is_empty()
        || lookup.vault_pubkey.trim().is_empty()
    {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint opt-in identity fields must be non-empty".to_owned(),
        ));
    }
    Ok(())
}

fn validate_cross_mint_vault_opt_in_upsert(
    input: &CrossMintVaultOptInUpsert,
) -> Result<(), OrchestratorError> {
    validate_cross_mint_vault_opt_in_lookup(&CrossMintVaultOptInLookup {
        cluster: input.cluster.clone(),
        settings: input.settings.clone(),
        vault_index: input.vault_index,
        vault_pubkey: input.vault_pubkey.clone(),
    })?;
    Ok(())
}

fn commitment_rank(commitment: &str) -> Result<u8, OrchestratorError> {
    match commitment {
        "processed" => Ok(0),
        "confirmed" => Ok(1),
        "finalized" => Ok(2),
        other => Err(OrchestratorError::StoreInvariant(format!(
            "unsupported policy source commitment {other:?}"
        ))),
    }
}

fn autoswap_commitment_eligible(commitment: &str) -> Result<bool, OrchestratorError> {
    Ok(commitment_rank(commitment)? >= commitment_rank("confirmed")?)
}

fn stronger_commitment(left: &str, right: &str) -> Result<&'static str, OrchestratorError> {
    match commitment_rank(left)?.max(commitment_rank(right)?) {
        0 => Ok("processed"),
        1 => Ok("confirmed"),
        2 => Ok("finalized"),
        _ => unreachable!("commitment rank is bounded"),
    }
}

async fn ensure_cross_mint_vault_opt_in_for_canonical_pair(
    tx: &mut Transaction<'_, Postgres>,
    event: &CrossMintSwapPolicyManifestInput,
) -> Result<(), OrchestratorError> {
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.cross_mint_vault_opt_ins
            (cluster, settings, vault_index, vault_pubkey, enabled)
        SELECT $1, $2, $3, $4, TRUE
        WHERE (
            SELECT count(*) = 2
               AND count(DISTINCT source_shard) = 2
               AND count(DISTINCT authority) = 1
               AND count(DISTINCT delegated_signer) = 1
               AND count(DISTINCT max_slippage_bps) = 1
               AND count(DISTINCT daily_source_mint_spending_cap) = 1
            FROM loyal_yield.cross_mint_swap_policies
            WHERE cluster = $1 AND settings = $2
              AND vault_index = $3 AND vault_pubkey = $4
              AND active AND start_eligible
              AND source_commitment IN ('confirmed', 'finalized')
              AND last_mutation IN ('create', 'update')
              AND source_shard IN ('classic', 'token_2022')
        )
        ON CONFLICT (cluster, settings, vault_index, vault_pubkey) DO NOTHING
        "#,
    )
    .bind(&event.cluster)
    .bind(&event.settings)
    .bind(i16::from(event.vault_index))
    .bind(&event.vault_pubkey)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

async fn canonical_cross_mint_pair_exists(
    tx: &mut Transaction<'_, Postgres>,
    lookup: &CrossMintVaultOptInLookup,
) -> Result<bool, OrchestratorError> {
    Ok(sqlx::query_scalar::<_, bool>(
        r#"
        SELECT count(*) = 2
           AND count(DISTINCT source_shard) = 2
           AND count(DISTINCT authority) = 1
           AND count(DISTINCT delegated_signer) = 1
           AND count(DISTINCT max_slippage_bps) = 1
           AND count(DISTINCT daily_source_mint_spending_cap) = 1
        FROM loyal_yield.cross_mint_swap_policies
        WHERE cluster = $1 AND settings = $2
          AND vault_index = $3 AND vault_pubkey = $4
          AND active AND start_eligible
          AND source_commitment IN ('confirmed', 'finalized')
          AND last_mutation IN ('create', 'update')
          AND source_shard IN ('classic', 'token_2022')
        "#,
    )
    .bind(&lookup.cluster)
    .bind(&lookup.settings)
    .bind(i16::from(lookup.vault_index))
    .bind(&lookup.vault_pubkey)
    .fetch_one(&mut **tx)
    .await?)
}

async fn insert_cross_mint_swap_policy(
    tx: &mut Transaction<'_, Postgres>,
    event: &CrossMintSwapPolicyManifestInput,
    policy_seed: Option<i64>,
    slot: i64,
) -> Result<CrossMintSwapPolicyRow, OrchestratorError> {
    let daily_cap = i64::try_from(event.daily_source_mint_spending_cap).map_err(|_| {
        OrchestratorError::StoreInvariant(
            "cross-mint daily source-mint spending cap exceeds PostgreSQL BIGINT".to_owned(),
        )
    })?;
    Ok(sqlx::query_as::<_, CrossMintSwapPolicyRow>(
        r#"
        INSERT INTO loyal_yield.cross_mint_swap_policies
            (cluster, settings, authority, policy_seed, policy_account,
             vault_index, vault_pubkey, delegated_signer, source_shard,
             max_slippage_bps, daily_source_mint_spending_cap,
             manifest_fingerprint, active, start_eligible, last_mutation,
             source_commitment, last_seen_slot, last_seen_signature)
        VALUES
            ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
             TRUE, $13, $14, $15, $16, $17)
        RETURNING *
        "#,
    )
    .bind(&event.cluster)
    .bind(&event.settings)
    .bind(&event.authority)
    .bind(policy_seed)
    .bind(&event.policy_account)
    .bind(i16::from(event.vault_index))
    .bind(&event.vault_pubkey)
    .bind(&event.delegated_signer)
    .bind(&event.source_shard)
    .bind(i32::from(event.max_slippage_bps))
    .bind(daily_cap)
    .bind(&event.manifest_fingerprint)
    .bind(autoswap_commitment_eligible(&event.source_commitment)?)
    .bind(&event.mutation)
    .bind(&event.source_commitment)
    .bind(slot)
    .bind(&event.signature)
    .fetch_one(&mut **tx)
    .await?)
}

async fn fetch_cross_mint_swap_policy_for_update(
    tx: &mut Transaction<'_, Postgres>,
    cluster: &str,
    policy_account: &str,
) -> Result<Option<CrossMintSwapPolicyRow>, OrchestratorError> {
    Ok(sqlx::query_as::<_, CrossMintSwapPolicyRow>(
        r#"
        SELECT *
        FROM loyal_yield.cross_mint_swap_policies
        WHERE cluster = $1 AND policy_account = $2
        FOR UPDATE
        "#,
    )
    .bind(cluster)
    .bind(policy_account)
    .fetch_optional(&mut **tx)
    .await?)
}

async fn update_cross_mint_policy_finality(
    tx: &mut Transaction<'_, Postgres>,
    cluster: &str,
    policy_account: &str,
    source_commitment: &str,
) -> Result<CrossMintSwapPolicyRow, OrchestratorError> {
    Ok(sqlx::query_as::<_, CrossMintSwapPolicyRow>(
        r#"
        UPDATE loyal_yield.cross_mint_swap_policies
        SET source_commitment = $3,
            start_eligible = active AND $3 IN ('confirmed', 'finalized')
                AND last_mutation IN ('create', 'update'),
            last_seen_at = now()
        WHERE cluster = $1 AND policy_account = $2
        RETURNING *
        "#,
    )
    .bind(cluster)
    .bind(policy_account)
    .bind(source_commitment)
    .fetch_one(&mut **tx)
    .await?)
}

async fn mark_cross_mint_policy_ambiguous(
    tx: &mut Transaction<'_, Postgres>,
    cluster: &str,
    policy_account: &str,
    source_commitment: &str,
    slot: i64,
    signature: &str,
) -> Result<CrossMintSwapPolicyRow, OrchestratorError> {
    Ok(sqlx::query_as::<_, CrossMintSwapPolicyRow>(
        r#"
        UPDATE loyal_yield.cross_mint_swap_policies
        SET active = FALSE,
            start_eligible = FALSE,
            last_mutation = 'ambiguous',
            source_commitment = $3,
            last_seen_at = now(),
            last_seen_slot = GREATEST(last_seen_slot, $4),
            last_seen_signature = $5
        WHERE cluster = $1 AND policy_account = $2
        RETURNING *
        "#,
    )
    .bind(cluster)
    .bind(policy_account)
    .bind(source_commitment)
    .bind(slot)
    .bind(signature)
    .fetch_one(&mut **tx)
    .await?)
}

async fn deactivate_cross_mint_swap_policy(
    tx: &mut Transaction<'_, Postgres>,
    event: &PolicyRemovalInput,
    slot: i64,
) -> Result<bool, OrchestratorError> {
    let Some(current) =
        fetch_cross_mint_swap_policy_for_update(tx, &event.cluster, &event.policy_account).await?
    else {
        insert_cross_mint_policy_removal_tombstone(tx, event, slot).await?;
        return Ok(false);
    };
    if slot < current.last_seen_slot {
        return Ok(false);
    }

    let was_enabled = current.active || current.start_eligible;
    let identity_matches =
        current.settings == event.settings && current.authority == event.authority;
    if !identity_matches
        || (slot == current.last_seen_slot
            && current.last_seen_signature != event.signature
            && current.last_mutation != "remove")
    {
        mark_cross_mint_policy_ambiguous(
            tx,
            &event.cluster,
            &event.policy_account,
            stronger_commitment(&event.source_commitment, &current.source_commitment)?,
            slot,
            &event.signature,
        )
        .await?;
        return Ok(was_enabled);
    }

    if current.last_mutation == "remove"
        && slot == current.last_seen_slot
        && current.last_seen_signature == event.signature
        && commitment_rank(&event.source_commitment)?
            <= commitment_rank(&current.source_commitment)?
    {
        return Ok(false);
    }

    sqlx::query(
        r#"
        UPDATE loyal_yield.cross_mint_swap_policies
        SET active = FALSE,
            start_eligible = FALSE,
            last_mutation = 'remove',
            source_commitment = $3,
            last_seen_at = now(),
            last_seen_slot = $4,
            last_seen_signature = $5
        WHERE cluster = $1 AND policy_account = $2
        "#,
    )
    .bind(&event.cluster)
    .bind(&event.policy_account)
    .bind(&event.source_commitment)
    .bind(slot)
    .bind(&event.signature)
    .execute(&mut **tx)
    .await?;
    Ok(was_enabled)
}

async fn delete_cross_mint_vault_opt_in_after_complete_removal(
    tx: &mut Transaction<'_, Postgres>,
    event: &PolicyRemovalInput,
) -> Result<(), OrchestratorError> {
    sqlx::query(
        r#"
        DELETE FROM loyal_yield.cross_mint_vault_opt_ins AS opt_in
        WHERE opt_in.cluster = $1
          AND opt_in.settings = $2
          AND NOT EXISTS (
              SELECT 1
              FROM loyal_yield.cross_mint_swap_policies AS active_policy
              WHERE active_policy.cluster = opt_in.cluster
                AND active_policy.settings = opt_in.settings
                AND active_policy.vault_index = opt_in.vault_index
                AND active_policy.vault_pubkey = opt_in.vault_pubkey
                AND active_policy.active
          )
          AND 2 = (
              SELECT count(DISTINCT removed_policy.source_shard)
              FROM loyal_yield.cross_mint_swap_policies AS removed_policy
              WHERE removed_policy.cluster = opt_in.cluster
                AND removed_policy.settings = opt_in.settings
                AND removed_policy.vault_index = opt_in.vault_index
                AND removed_policy.vault_pubkey = opt_in.vault_pubkey
                AND removed_policy.authority = $3
                AND removed_policy.source_shard IN ('classic', 'token_2022')
                AND removed_policy.last_mutation = 'remove'
                AND NOT removed_policy.active
          )
        "#,
    )
    .bind(&event.cluster)
    .bind(&event.settings)
    .bind(&event.authority)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

async fn insert_cross_mint_policy_removal_tombstone(
    tx: &mut Transaction<'_, Postgres>,
    event: &PolicyRemovalInput,
    slot: i64,
) -> Result<(), OrchestratorError> {
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.cross_mint_swap_policies
            (cluster, settings, authority, policy_seed, policy_account,
             vault_index, vault_pubkey, delegated_signer, source_shard,
             max_slippage_bps, daily_source_mint_spending_cap,
             manifest_fingerprint, active, start_eligible, last_mutation,
             source_commitment, last_seen_slot, last_seen_signature)
        VALUES
            ($1, $2, $3, NULL, $4, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
             FALSE, FALSE, 'remove', $5, $6, $7)
        "#,
    )
    .bind(&event.cluster)
    .bind(&event.settings)
    .bind(&event.authority)
    .bind(&event.policy_account)
    .bind(&event.source_commitment)
    .bind(slot)
    .bind(&event.signature)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

async fn upsert_policy(
    conn: &mut PgConnection,
    event: &PolicyMatchInput,
) -> Result<RoutePolicy, OrchestratorError> {
    commitment_rank(&event.source_commitment)?;
    let slot =
        i64::try_from(event.slot).map_err(|_| OrchestratorError::SlotOutOfRange(event.slot))?;
    let policy_seed = i64::try_from(event.policy_seed)
        .map_err(|_| OrchestratorError::PolicySeedOutOfRange(event.policy_seed))?;
    let row = sqlx::query_as::<_, RoutePolicyRow>(
        r#"
        INSERT INTO loyal_yield.route_policies
            (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
             delegated_signers, threshold, route_modes, stable_mints, kamino_markets, kamino_liquidity_mints,
             universe_preset, risk_profile, swap_lanes, active, last_seen_slot, last_seen_signature,
             cluster, source_commitment, finalized_eligible)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, TRUE, $16, $17, $18, $19, $20)
        ON CONFLICT (policy_account) DO UPDATE SET
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
            cluster = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.cluster ELSE loyal_yield.route_policies.cluster END,
            source_commitment = CASE
                WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot
                  OR (EXCLUDED.last_seen_slot = loyal_yield.route_policies.last_seen_slot
                      AND EXCLUDED.last_seen_signature = loyal_yield.route_policies.last_seen_signature
                      AND EXCLUDED.source_commitment = 'finalized')
                THEN EXCLUDED.source_commitment
                ELSE loyal_yield.route_policies.source_commitment
            END,
            finalized_eligible = CASE
                WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot
                  OR (EXCLUDED.last_seen_slot = loyal_yield.route_policies.last_seen_slot
                      AND EXCLUDED.last_seen_signature = loyal_yield.route_policies.last_seen_signature
                      AND EXCLUDED.source_commitment = 'finalized')
                THEN EXCLUDED.finalized_eligible
                ELSE loyal_yield.route_policies.finalized_eligible
            END,
            active = CASE
                WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot
                  OR (EXCLUDED.last_seen_slot = loyal_yield.route_policies.last_seen_slot
                      AND EXCLUDED.last_seen_signature = loyal_yield.route_policies.last_seen_signature
                      AND EXCLUDED.source_commitment = 'finalized')
                THEN TRUE
                ELSE loyal_yield.route_policies.active
            END,
            last_seen_at = CASE
                WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot
                  OR (EXCLUDED.last_seen_slot = loyal_yield.route_policies.last_seen_slot
                      AND EXCLUDED.last_seen_signature = loyal_yield.route_policies.last_seen_signature
                      AND EXCLUDED.source_commitment = 'finalized')
                THEN now()
                ELSE loyal_yield.route_policies.last_seen_at
            END,
            last_seen_slot = GREATEST(loyal_yield.route_policies.last_seen_slot, EXCLUDED.last_seen_slot),
            last_seen_signature = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.last_seen_signature ELSE loyal_yield.route_policies.last_seen_signature END
        RETURNING
            id,
            cluster,
            source_commitment,
            finalized_eligible,
            settings,
            authority,
            policy_seed,
            policy_account,
            vault_index,
            vault_pubkey,
            delegated_signers,
            threshold,
            route_modes,
            stable_mints,
            kamino_markets,
            kamino_liquidity_mints,
            universe_preset,
            risk_profile,
            swap_lanes,
            active,
            first_seen_at,
            last_seen_at,
            last_seen_slot,
            last_seen_signature
        "#,
    )
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
    .bind(&event.cluster)
    .bind(&event.source_commitment)
    .bind(event.source_commitment == "finalized")
    .fetch_one(conn)
    .await?;

    Ok(route_policy_from_row(row))
}

async fn upsert_vault(
    conn: &mut PgConnection,
    policy_id: PolicyId,
    event: &PolicyMatchInput,
) -> Result<ManagedVault, OrchestratorError> {
    let row = sqlx::query_as::<_, ManagedVaultRow>(
        r#"
        INSERT INTO loyal_yield.managed_vaults
            (settings, vault_index, vault_pubkey, active_policy_id, active)
        VALUES ($1, $2, $3, $4, TRUE)
        ON CONFLICT (settings, vault_index, vault_pubkey) DO UPDATE SET
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
        RETURNING id, settings, vault_index, vault_pubkey, active_policy_id, active, first_seen_at, last_seen_at
        "#,
    )
    .bind(&event.settings)
    .bind(i16::from(event.vault_index))
    .bind(&event.vault_pubkey)
    .bind(policy_id.as_i64())
    .fetch_one(conn)
    .await?;

    Ok(managed_vault_from_row(row))
}

async fn upsert_vault_with_setup(
    conn: &mut PgConnection,
    route_policy_id: PolicyId,
    setup_policy_id: PolicyId,
    event: &PolicyMatchInput,
) -> Result<ManagedVault, OrchestratorError> {
    let row = sqlx::query_as::<_, ManagedVaultRow>(
        r#"
        INSERT INTO loyal_yield.managed_vaults
            (settings, vault_index, vault_pubkey, active_policy_id, setup_policy_id, active)
        VALUES ($1, $2, $3, $4, $5, TRUE)
        ON CONFLICT (settings, vault_index, vault_pubkey) DO UPDATE SET
            active_policy_id = EXCLUDED.active_policy_id,
            setup_policy_id = EXCLUDED.setup_policy_id,
            active = TRUE,
            last_seen_at = now()
        RETURNING id, settings, vault_index, vault_pubkey, active_policy_id, active, first_seen_at, last_seen_at
        "#,
    )
    .bind(&event.settings)
    .bind(i16::from(event.vault_index))
    .bind(&event.vault_pubkey)
    .bind(route_policy_id.as_i64())
    .bind(setup_policy_id.as_i64())
    .fetch_one(conn)
    .await?;

    Ok(managed_vault_from_row(row))
}

async fn apply_earn_policy_only(
    conn: &mut PgConnection,
    mutation: &EarnPolicyOnlyMutation,
) -> Result<(), OrchestratorError> {
    let route = upsert_policy(conn, &mutation.route_policy).await?;
    let setup = upsert_policy(conn, &mutation.setup_policy).await?;
    upsert_vault_with_setup(conn, route.id, setup.id, &mutation.route_policy).await?;

    sqlx::query(
        r#"
        UPDATE loyal_yield.earn_deposit_onboarding_attempts
        SET route_policy_db_id = $1,
            route_policy_signature = $2,
            route_policy_confirmed_slot = $3,
            setup_policy_id = $4,
            setup_policy_account = $5,
            setup_policy_seed = $6,
            setup_policy_db_id = $7,
            setup_policy_signature = $8,
            setup_policy_confirmed_slot = $9,
            status = 'setup_policy_confirmed',
            last_error_code = NULL,
            updated_at = now()
        WHERE settings = $10 AND vault_index = $11 AND vault_pubkey = $12
          AND status <> 'complete'
        "#,
    )
    .bind(route.id.as_i64())
    .bind(&mutation.route_policy.signature)
    .bind(to_i64_slot(mutation.route_policy.slot)?)
    .bind(to_i64_policy_seed(mutation.setup_policy.policy_seed)?)
    .bind(&mutation.setup_policy.policy_account)
    .bind(to_i64_policy_seed(mutation.setup_policy.policy_seed)?)
    .bind(setup.id.as_i64())
    .bind(&mutation.setup_policy.signature)
    .bind(to_i64_slot(mutation.setup_policy.slot)?)
    .bind(&mutation.route_policy.settings)
    .bind(i16::from(mutation.route_policy.vault_index))
    .bind(&mutation.route_policy.vault_pubkey)
    .execute(conn)
    .await?;
    Ok(())
}

async fn apply_earn_deposit(
    conn: &mut PgConnection,
    mutation: &EarnDepositMutation,
) -> Result<(), OrchestratorError> {
    let route = upsert_policy(conn, &mutation.route_policy).await?;
    let vault = if let Some(setup_policy) = &mutation.setup_policy {
        let setup = upsert_policy(conn, setup_policy).await?;
        upsert_vault_with_setup(conn, route.id, setup.id, &mutation.route_policy).await?
    } else {
        upsert_vault(conn, route.id, &mutation.route_policy).await?
    };
    let confirmed_slot = to_i64_slot(mutation.deposit_slot)?;
    let balance_observed_slot = to_i64_slot(mutation.observed_slot)?;
    let amount = to_i64_amount(mutation.principal_amount_raw)?;
    let current_amount_raw = mutation
        .reserve_state
        .iter()
        .filter(|reserve| reserve.liquidity_mint == mutation.liquidity_mint)
        .map(|reserve| reserve.amount_raw)
        .chain(
            mutation
                .idle_state
                .iter()
                .filter(|idle| idle.mint == mutation.liquidity_mint)
                .map(|idle| idle.amount_raw),
        )
        .try_fold(0_u64, |total, value| total.checked_add(value))
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant("Earn holding amount overflow".to_owned())
        })?;
    let current_amount = to_i64_amount(current_amount_raw)?;
    let observed_at = mutation.observed_at.unwrap_or_else(Utc::now);

    let inserted_deposit_id = sqlx::query_scalar::<_, i64>(
        r#"
        INSERT INTO loyal_yield.user_yield_position_deposits (
            deposit_signature, policy_signature, confirmed_slot, wallet_address,
            smart_account_address, settings, vault_index, vault_pubkey, policy_id,
            policy_account, policy_seed, target_reserve, market, liquidity_mint,
            target_supply_apy_bps, deposit_mint, principal_amount_raw, confirmed_at,
            created_at
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
            $15, $16, $17, $18, now()
        )
        ON CONFLICT (deposit_signature) DO NOTHING
        RETURNING id
        "#,
    )
    .bind(&mutation.deposit_signature)
    .bind(&mutation.route_policy.signature)
    .bind(confirmed_slot)
    .bind(&mutation.wallet)
    .bind(&mutation.smart_account_address)
    .bind(&mutation.route_policy.settings)
    .bind(i16::from(mutation.route_policy.vault_index))
    .bind(&mutation.route_policy.vault_pubkey)
    .bind(route.id.as_i64())
    .bind(&mutation.route_policy.policy_account)
    .bind(to_i64_policy_seed(mutation.route_policy.policy_seed)?)
    .bind(&mutation.target_reserve)
    .bind(&mutation.market)
    .bind(&mutation.liquidity_mint)
    .bind(mutation.target_supply_apy_bps)
    .bind(&mutation.deposit_mint)
    .bind(amount)
    .bind(observed_at)
    .fetch_optional(&mut *conn)
    .await?;

    let Some(deposit_id) = inserted_deposit_id else {
        // This exact deposit was already committed. Do not republish its old
        // account snapshot during replay and risk replacing newer vault state.
        return Ok(());
    };

    let existing_position = sqlx::query_as::<_, ExistingEarnPositionProjectionRow>(
        r#"
            SELECT id, current_reserve, current_market, current_liquidity_mint,
                   current_amount_raw, current_observed_slot, current_observed_at
            FROM loyal_yield.user_yield_positions
            WHERE settings = $1 AND vault_index = $2
              AND wallet_address = $3 AND vault_pubkey = $4
              AND status = 'active'::loyal_yield.yield_position_status
            ORDER BY updated_at DESC, id DESC
            LIMIT 1
            FOR UPDATE
            "#,
    )
    .bind(&mutation.route_policy.settings)
    .bind(i16::from(mutation.route_policy.vault_index))
    .bind(&mutation.wallet)
    .bind(&mutation.route_policy.vault_pubkey)
    .fetch_optional(&mut *conn)
    .await?;

    let (
        position_id,
        event_type,
        event_reserve,
        event_market,
        event_liquidity_mint,
        resulting_amount,
        holding_delta,
        event_observed_slot,
        event_observed_at,
    ) = if let Some(existing) = existing_position {
        let incoming_is_current = balance_observed_slot >= existing.current_observed_slot;
        let event_reserve = if incoming_is_current {
            mutation.target_reserve.clone()
        } else {
            existing.current_reserve
        };
        let event_market = if incoming_is_current {
            mutation.market.clone()
        } else {
            existing.current_market
        };
        let event_liquidity_mint = if incoming_is_current {
            mutation.liquidity_mint.clone()
        } else {
            existing.current_liquidity_mint
        };
        let resulting_amount = if incoming_is_current {
            current_amount
        } else {
            existing.current_amount_raw
        };
        let event_observed_slot = if incoming_is_current {
            balance_observed_slot
        } else {
            existing.current_observed_slot
        };
        let event_observed_at = if incoming_is_current {
            observed_at
        } else {
            existing.current_observed_at
        };
        let holding_delta = resulting_amount
            .checked_sub(existing.current_amount_raw)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "Earn holding delta overflow during deposit reconciliation".to_owned(),
                )
            })?;
        sqlx::query(
            r#"
                UPDATE loyal_yield.user_yield_positions
                SET wallet_address = $2,
                    smart_account_address = $3,
                    vault_pubkey = $4,
                    policy_id = $5,
                    policy_account = $6,
                    policy_seed = $7,
                    principal_amount_raw = principal_amount_raw + $8,
                    last_deposit_signature = $9,
                    last_confirmed_slot = GREATEST(last_confirmed_slot, $10),
                    status = 'active'::loyal_yield.yield_position_status,
                    current_reserve = $11,
                    current_market = $12,
                    current_liquidity_mint = $13,
                    current_amount_raw = $14,
                    current_observed_slot = $15,
                    current_observed_at = $16,
                    updated_at = now()
                WHERE id = $1
                "#,
        )
        .bind(existing.id)
        .bind(&mutation.wallet)
        .bind(&mutation.smart_account_address)
        .bind(&mutation.route_policy.vault_pubkey)
        .bind(route.id.as_i64())
        .bind(&mutation.route_policy.policy_account)
        .bind(to_i64_policy_seed(mutation.route_policy.policy_seed)?)
        .bind(amount)
        .bind(&mutation.deposit_signature)
        .bind(confirmed_slot)
        .bind(&event_reserve)
        .bind(&event_market)
        .bind(&event_liquidity_mint)
        .bind(resulting_amount)
        .bind(event_observed_slot)
        .bind(event_observed_at)
        .execute(&mut *conn)
        .await?;
        (
            existing.id,
            "deposit_top_up",
            event_reserve,
            event_market,
            event_liquidity_mint,
            resulting_amount,
            holding_delta,
            event_observed_slot,
            event_observed_at,
        )
    } else {
        let position_id = sqlx::query_scalar::<_, i64>(
            r#"
                INSERT INTO loyal_yield.user_yield_positions (
                    wallet_address, smart_account_address, settings, vault_index,
                    vault_pubkey, policy_id, policy_account, policy_seed,
                    initial_reserve, initial_market, initial_liquidity_mint,
                    initial_supply_apy_bps, deposit_mint, principal_amount_raw,
                    first_deposit_signature, last_deposit_signature,
                    last_confirmed_slot, status, created_at, updated_at,
                    current_reserve, current_market, current_liquidity_mint,
                    current_amount_raw, current_observed_slot, current_observed_at
                ) VALUES (
                    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
                    $14, $15, $15, $16, 'active'::loyal_yield.yield_position_status,
                    now(), now(), $9, $10, $11, $18, $19, $17
                ) RETURNING id
                "#,
        )
        .bind(&mutation.wallet)
        .bind(&mutation.smart_account_address)
        .bind(&mutation.route_policy.settings)
        .bind(i16::from(mutation.route_policy.vault_index))
        .bind(&mutation.route_policy.vault_pubkey)
        .bind(route.id.as_i64())
        .bind(&mutation.route_policy.policy_account)
        .bind(to_i64_policy_seed(mutation.route_policy.policy_seed)?)
        .bind(&mutation.target_reserve)
        .bind(&mutation.market)
        .bind(&mutation.liquidity_mint)
        .bind(mutation.target_supply_apy_bps)
        .bind(&mutation.deposit_mint)
        .bind(amount)
        .bind(&mutation.deposit_signature)
        .bind(confirmed_slot)
        .bind(observed_at)
        .bind(current_amount)
        .bind(balance_observed_slot)
        .fetch_one(&mut *conn)
        .await?;
        (
            position_id,
            "deposit_initialized",
            mutation.target_reserve.clone(),
            mutation.market.clone(),
            mutation.liquidity_mint.clone(),
            current_amount,
            current_amount,
            balance_observed_slot,
            observed_at,
        )
    };

    let holding_id = sqlx::query_scalar::<_, i64>(
        r#"
            INSERT INTO loyal_yield.user_yield_position_holding_events (
                position_id, event_type, reserve, market, liquidity_mint,
                amount_raw, principal_delta_raw, holding_delta_raw, observed_slot,
                observed_at, source_signature, source_deposit_id, created_at
            ) VALUES (
                $1, $2::text::loyal_yield.user_yield_holding_event_type, $3, $4,
                $5, $6, $7, $8, $9, $10, $11, $12, now()
            ) RETURNING id
            "#,
    )
    .bind(position_id)
    .bind(event_type)
    .bind(&event_reserve)
    .bind(&event_market)
    .bind(&event_liquidity_mint)
    .bind(resulting_amount)
    .bind(amount)
    .bind(holding_delta)
    .bind(event_observed_slot)
    .bind(event_observed_at)
    .bind(&mutation.deposit_signature)
    .bind(deposit_id)
    .fetch_one(&mut *conn)
    .await?;
    sqlx::query(
        "UPDATE loyal_yield.user_yield_positions SET last_holding_event_id = $2 WHERE id = $1",
    )
    .bind(position_id)
    .bind(holding_id)
    .execute(&mut *conn)
    .await?;

    apply_earn_observed_balances(
        conn,
        &vault,
        &mutation.reserve_state,
        &mutation.idle_state,
        balance_observed_slot,
        observed_at,
    )
    .await?;
    Ok(())
}

async fn apply_earn_observed_balances(
    conn: &mut PgConnection,
    vault: &ManagedVault,
    reserve_state: &[EarnReserveMutation],
    idle_state: &[EarnIdleTokenMutation],
    observed_slot: i64,
    observed_at: DateTime<Utc>,
) -> Result<(), OrchestratorError> {
    let current_slot = sqlx::query_scalar::<_, i64>(
        "SELECT observed_slot FROM loyal_yield.vault_position_snapshots WHERE vault_id = $1 AND is_current FOR UPDATE",
    )
    .bind(vault.id.as_i64())
    .fetch_optional(&mut *conn)
    .await?;
    if current_slot.is_some_and(|current| current > observed_slot) {
        return Ok(());
    }
    sqlx::query("UPDATE loyal_yield.vault_position_snapshots SET is_current = FALSE WHERE vault_id = $1 AND is_current")
        .bind(vault.id.as_i64())
        .execute(&mut *conn)
        .await?;
    let snapshot_id = sqlx::query_scalar::<_, i64>(
        r#"
        INSERT INTO loyal_yield.vault_position_snapshots
            (vault_id, policy_id, observed_slot, observed_at, chain_slot, context)
        VALUES ($1, $2, $3, $4, $3, jsonb_build_object('kind', 'earn_laserstream_direct'))
        RETURNING id
        "#,
    )
    .bind(vault.id.as_i64())
    .bind(vault.active_policy_id.as_i64())
    .bind(observed_slot)
    .bind(observed_at)
    .fetch_one(&mut *conn)
    .await?;
    let observed_reserves = reserve_state
        .iter()
        .map(|reserve| reserve.reserve.clone())
        .collect::<Vec<_>>();
    sqlx::query(
        r#"
        DELETE FROM loyal_yield.vault_reserve_positions_current
        WHERE vault_id = $1
          AND observed_slot <= $3
          AND NOT (reserve = ANY($2::text[]))
        "#,
    )
    .bind(vault.id.as_i64())
    .bind(&observed_reserves)
    .bind(observed_slot)
    .execute(&mut *conn)
    .await?;
    let observed_idle_mints = idle_state
        .iter()
        .map(|idle| idle.mint.clone())
        .collect::<Vec<_>>();
    sqlx::query(
        r#"
        DELETE FROM loyal_yield.vault_idle_token_balances_current
        WHERE vault_id = $1
          AND observed_slot <= $3
          AND NOT (mint = ANY($2::text[]))
        "#,
    )
    .bind(vault.id.as_i64())
    .bind(&observed_idle_mints)
    .bind(observed_slot)
    .execute(&mut *conn)
    .await?;
    for reserve in reserve_state {
        let amount = to_i64_amount(reserve.amount_raw)?;
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.vault_position_snapshot_positions
                (snapshot_id, reserve, market, liquidity_mint, amount_raw,
                 supply_apy_bps, borrow_apy_bps, has_value, planning_metadata)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
            "#,
        )
        .bind(snapshot_id)
        .bind(&reserve.reserve)
        .bind(&reserve.market)
        .bind(&reserve.liquidity_mint)
        .bind(amount)
        .bind(reserve.supply_apy_bps)
        .bind(reserve.borrow_apy_bps)
        .bind(reserve.has_value)
        .bind(&reserve.planning_metadata)
        .execute(&mut *conn)
        .await?;
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.vault_reserve_positions_current
                (vault_id, reserve, market, liquidity_mint, amount_raw, has_value,
                 supply_apy_bps, borrow_apy_bps, snapshot_id, observed_slot,
                 observed_at, planning_metadata)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
            ON CONFLICT (vault_id, reserve) DO UPDATE SET
                market = EXCLUDED.market,
                liquidity_mint = EXCLUDED.liquidity_mint,
                amount_raw = EXCLUDED.amount_raw,
                has_value = EXCLUDED.has_value,
                supply_apy_bps = EXCLUDED.supply_apy_bps,
                borrow_apy_bps = EXCLUDED.borrow_apy_bps,
                snapshot_id = EXCLUDED.snapshot_id,
                observed_slot = EXCLUDED.observed_slot,
                observed_at = EXCLUDED.observed_at,
                planning_metadata = EXCLUDED.planning_metadata
            "#,
        )
        .bind(vault.id.as_i64())
        .bind(&reserve.reserve)
        .bind(&reserve.market)
        .bind(&reserve.liquidity_mint)
        .bind(amount)
        .bind(reserve.has_value)
        .bind(reserve.supply_apy_bps)
        .bind(reserve.borrow_apy_bps)
        .bind(snapshot_id)
        .bind(observed_slot)
        .bind(observed_at)
        .bind(&reserve.planning_metadata)
        .execute(&mut *conn)
        .await?;
    }
    for idle in idle_state {
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.vault_idle_token_balances_current
                (vault_id, mint, amount_raw, owner, token_account, observed_slot,
                 observed_at, source_commitment, updated_at)
            VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()), $8, now())
            ON CONFLICT (vault_id, mint) DO UPDATE SET
                amount_raw = EXCLUDED.amount_raw,
                owner = EXCLUDED.owner,
                token_account = EXCLUDED.token_account,
                observed_slot = EXCLUDED.observed_slot,
                observed_at = EXCLUDED.observed_at,
                source_commitment = EXCLUDED.source_commitment,
                updated_at = now()
            "#,
        )
        .bind(vault.id.as_i64())
        .bind(&idle.mint)
        .bind(to_i64_amount(idle.amount_raw)?)
        .bind(&idle.owner)
        .bind(&idle.token_account)
        .bind(to_i64_slot(idle.observed_slot)?)
        .bind(idle.observed_at)
        .bind(&idle.source_commitment)
        .execute(&mut *conn)
        .await?;
    }
    Ok(())
}

async fn apply_earn_withdrawal(
    conn: &mut PgConnection,
    mutation: &EarnWithdrawalMutation,
) -> Result<(), OrchestratorError> {
    let route = upsert_policy(conn, &mutation.route_policy).await?;
    let vault = upsert_vault(conn, route.id, &mutation.route_policy).await?;
    let confirmed_slot = to_i64_slot(mutation.confirmed_slot)?;
    let observed_slot = to_i64_slot(mutation.observed_slot)?;
    let withdrawn_amount = to_i64_amount(mutation.withdrawn_amount_raw)?;
    let remaining_amount = to_i64_amount(mutation.remaining_amount_raw)?;
    let observed_at = mutation.observed_at.unwrap_or_else(Utc::now);

    let position = sqlx::query(
        r#"
        SELECT id, principal_amount_raw, current_amount_raw, current_observed_slot
        FROM loyal_yield.user_yield_positions
        WHERE settings = $1 AND vault_index = $2 AND wallet_address = $3
          AND vault_pubkey = $4 AND status = 'active'::loyal_yield.yield_position_status
        ORDER BY updated_at DESC, id DESC
        LIMIT 1
        FOR UPDATE
        "#,
    )
    .bind(&mutation.route_policy.settings)
    .bind(i16::from(mutation.route_policy.vault_index))
    .bind(&mutation.wallet)
    .bind(&mutation.vault_pubkey)
    .fetch_optional(&mut *conn)
    .await?;
    let Some(position) = position else {
        let replayed: bool = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM loyal_yield.user_yield_position_withdrawals WHERE withdrawal_signature = $1)",
        )
        .bind(&mutation.withdrawal_signature)
        .fetch_one(&mut *conn)
        .await?;
        if replayed {
            return Ok(());
        }
        return Err(OrchestratorError::StoreInvariant(format!(
            "finalized Earn withdrawal {} has no active projected position",
            mutation.withdrawal_signature
        )));
    };
    let position_id: i64 = position.try_get("id")?;
    let principal: i64 = position.try_get("principal_amount_raw")?;
    let previous_amount: i64 = position.try_get("current_amount_raw")?;
    let previous_observed_slot: i64 = position.try_get("current_observed_slot")?;
    let mode = if remaining_amount == 0 {
        "full"
    } else {
        "partial"
    };
    let withdrawal_id = sqlx::query_scalar::<_, i64>(
        r#"
        INSERT INTO loyal_yield.user_yield_position_withdrawals (
            withdrawal_signature, confirmed_slot, wallet_address,
            smart_account_address, settings, vault_index, vault_pubkey, policy_id,
            policy_account, policy_seed, target_reserve, market, liquidity_mint,
            withdrawn_amount_raw, source_type, source_id, source_metadata,
            reserve_withdrawals, mode, confirmed_at, created_at
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
            'chain_snapshot', $11, jsonb_build_object('kind', 'earn_laserstream_finalized'),
            '[]'::jsonb, $15, $16, now()
        )
        ON CONFLICT (withdrawal_signature) DO NOTHING
        RETURNING id
        "#,
    )
    .bind(&mutation.withdrawal_signature)
    .bind(confirmed_slot)
    .bind(&mutation.wallet)
    .bind(&mutation.vault_pubkey)
    .bind(&mutation.route_policy.settings)
    .bind(i16::from(mutation.route_policy.vault_index))
    .bind(&mutation.vault_pubkey)
    .bind(route.id.as_i64())
    .bind(&mutation.route_policy.policy_account)
    .bind(to_i64_policy_seed(mutation.route_policy.policy_seed)?)
    .bind(&mutation.target_reserve)
    .bind(&mutation.market)
    .bind(&mutation.liquidity_mint)
    .bind(withdrawn_amount)
    .bind(mode)
    .bind(observed_at)
    .fetch_optional(&mut *conn)
    .await?;
    let Some(withdrawal_id) = withdrawal_id else {
        return Ok(());
    };

    let principal_delta = -principal.min(withdrawn_amount);
    let next_principal = principal.saturating_sub(withdrawn_amount);
    let snapshot_is_current = observed_slot >= previous_observed_slot;
    let resulting_amount = if snapshot_is_current {
        remaining_amount
    } else {
        previous_amount
    };
    let event_slot = if snapshot_is_current {
        observed_slot
    } else {
        previous_observed_slot
    };
    let holding_delta = resulting_amount
        .checked_sub(previous_amount)
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Earn holding delta overflow during withdrawal reconciliation".to_owned(),
            )
        })?;
    let holding_id = sqlx::query_scalar::<_, i64>(
        r#"
        INSERT INTO loyal_yield.user_yield_position_holding_events (
            position_id, event_type, reserve, market, liquidity_mint,
            amount_raw, principal_delta_raw, holding_delta_raw, observed_slot,
            observed_at, source_signature, source_withdrawal_id, created_at
        ) VALUES (
            $1, $2::text::loyal_yield.user_yield_holding_event_type, $3, $4, $5,
            $6, $7, $8, $9, $10, $11, $12, now()
        ) RETURNING id
        "#,
    )
    .bind(position_id)
    .bind(if mode == "full" {
        "withdrawal_full"
    } else {
        "withdrawal_partial"
    })
    .bind(&mutation.target_reserve)
    .bind(&mutation.market)
    .bind(&mutation.liquidity_mint)
    .bind(resulting_amount)
    .bind(principal_delta)
    .bind(holding_delta)
    .bind(event_slot)
    .bind(observed_at)
    .bind(&mutation.withdrawal_signature)
    .bind(withdrawal_id)
    .fetch_one(&mut *conn)
    .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.user_yield_positions
        SET principal_amount_raw = $2,
            current_reserve = CASE WHEN $3 THEN $4 ELSE current_reserve END,
            current_market = CASE WHEN $3 THEN $5 ELSE current_market END,
            current_liquidity_mint = CASE WHEN $3 THEN $6 ELSE current_liquidity_mint END,
            current_amount_raw = $7,
            current_observed_slot = $8,
            current_observed_at = $9,
            last_confirmed_slot = GREATEST(last_confirmed_slot, $10),
            last_holding_event_id = $11,
            updated_at = now()
        WHERE id = $1
        "#,
    )
    .bind(position_id)
    .bind(next_principal)
    .bind(snapshot_is_current)
    .bind(&mutation.target_reserve)
    .bind(&mutation.market)
    .bind(&mutation.liquidity_mint)
    .bind(resulting_amount)
    .bind(event_slot)
    .bind(observed_at)
    .bind(confirmed_slot)
    .bind(holding_id)
    .execute(&mut *conn)
    .await?;
    apply_earn_observed_balances(
        conn,
        &vault,
        &mutation.reserve_state,
        &mutation.idle_state,
        observed_slot,
        observed_at,
    )
    .await?;
    Ok(())
}

async fn apply_earn_refund(
    conn: &mut PgConnection,
    mutation: &EarnRefundMutation,
) -> Result<(), OrchestratorError> {
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.earn_chain_refund_events (
            cluster, settings, vault_index, vault_pubkey, wallet_address,
            refund_signature, confirmed_slot, refund_kind, confirmed_at, created_at
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
        ON CONFLICT (refund_signature) DO NOTHING
        "#,
    )
    .bind(&mutation.cluster)
    .bind(&mutation.settings)
    .bind(i16::from(mutation.vault_index))
    .bind(&mutation.vault_pubkey)
    .bind(&mutation.wallet)
    .bind(&mutation.refund_signature)
    .bind(to_i64_slot(mutation.confirmed_slot)?)
    .bind(&mutation.refund_kind)
    .bind(mutation.observed_at.unwrap_or_else(Utc::now))
    .execute(conn)
    .await?;
    Ok(())
}

async fn apply_earn_cleanup(
    conn: &mut PgConnection,
    mutation: &EarnCleanupMutation,
) -> Result<(), OrchestratorError> {
    let slot = to_i64_slot(mutation.confirmed_slot)?;
    let observed_at = mutation.observed_at.unwrap_or_else(Utc::now);
    let vault_row = sqlx::query(
        r#"
        SELECT id, active_policy_id, setup_policy_id
        FROM loyal_yield.managed_vaults
        WHERE settings = $1 AND vault_index = $2 AND vault_pubkey = $3
        FOR UPDATE
        "#,
    )
    .bind(&mutation.settings)
    .bind(i16::from(mutation.vault_index))
    .bind(&mutation.vault_pubkey)
    .fetch_optional(&mut *conn)
    .await?;
    let Some(vault_row) = vault_row else {
        return Ok(());
    };
    let vault_id: i64 = vault_row.try_get("id")?;
    let active_policy_id: i64 = vault_row.try_get("active_policy_id")?;
    let setup_policy_id: Option<i64> = vault_row.try_get("setup_policy_id")?;

    sqlx::query(
        r#"
        UPDATE loyal_yield.route_policies
        SET active = FALSE,
            finalized_eligible = FALSE,
            last_seen_slot = GREATEST(last_seen_slot, $3),
            last_seen_signature = CASE WHEN $3 >= last_seen_slot THEN $4 ELSE last_seen_signature END,
            last_seen_at = now()
        WHERE id = $1 OR id = $2
        "#,
    )
    .bind(active_policy_id)
    .bind(setup_policy_id)
    .bind(slot)
    .bind(&mutation.cleanup_signature)
    .execute(&mut *conn)
    .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.vault_reserve_positions_current
        SET amount_raw = 0, has_value = FALSE, observed_slot = $2, observed_at = $3
        WHERE vault_id = $1 AND observed_slot <= $2
        "#,
    )
    .bind(vault_id)
    .bind(slot)
    .bind(observed_at)
    .execute(&mut *conn)
    .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.vault_idle_token_balances_current
        SET amount_raw = 0, observed_slot = $2, observed_at = $3, updated_at = now()
        WHERE vault_id = $1 AND observed_slot <= $2
        "#,
    )
    .bind(vault_id)
    .bind(slot)
    .bind(observed_at)
    .execute(&mut *conn)
    .await?;

    let positions = sqlx::query(
        r#"
        SELECT id, current_reserve, current_market, current_liquidity_mint,
               principal_amount_raw, current_amount_raw
        FROM loyal_yield.user_yield_positions
        WHERE settings = $1 AND vault_index = $2 AND vault_pubkey = $3
          AND status::text = 'active'
        FOR UPDATE
        "#,
    )
    .bind(&mutation.settings)
    .bind(i16::from(mutation.vault_index))
    .bind(&mutation.vault_pubkey)
    .fetch_all(&mut *conn)
    .await?;
    for position in positions {
        let position_id: i64 = position.try_get("id")?;
        let principal: i64 = position.try_get("principal_amount_raw")?;
        let current: i64 = position.try_get("current_amount_raw")?;
        let holding_id = sqlx::query_scalar::<_, i64>(
            r#"
            INSERT INTO loyal_yield.user_yield_position_holding_events (
                position_id, event_type, reserve, market, liquidity_mint,
                amount_raw, principal_delta_raw, holding_delta_raw, observed_slot,
                observed_at, source_signature, created_at
            ) VALUES (
                $1, 'snapshot_reconciled'::loyal_yield.user_yield_holding_event_type,
                $2, $3, $4, 0, $5, $6, $7, $8, $9, now()
            ) RETURNING id
            "#,
        )
        .bind(position_id)
        .bind(position.try_get::<String, _>("current_reserve")?)
        .bind(position.try_get::<Option<String>, _>("current_market")?)
        .bind(position.try_get::<String, _>("current_liquidity_mint")?)
        .bind(-principal)
        .bind(-current)
        .bind(slot)
        .bind(observed_at)
        .bind(&mutation.cleanup_signature)
        .fetch_one(&mut *conn)
        .await?;
        sqlx::query(
            r#"
            UPDATE loyal_yield.user_yield_positions
            SET principal_amount_raw = 0,
                current_amount_raw = 0,
                current_observed_slot = $2,
                current_observed_at = $3,
                last_holding_event_id = $4,
                status = 'closed'::loyal_yield.yield_position_status,
                updated_at = now()
            WHERE id = $1
            "#,
        )
        .bind(position_id)
        .bind(slot)
        .bind(observed_at)
        .bind(holding_id)
        .execute(&mut *conn)
        .await?;
    }
    sqlx::query(
        "UPDATE loyal_yield.managed_vaults SET active = FALSE, last_seen_at = now() WHERE id = $1",
    )
    .bind(vault_id)
    .execute(&mut *conn)
    .await?;
    Ok(())
}

async fn fetch_managed_vault_for_update(
    conn: &mut PgConnection,
    vault_id: VaultId,
) -> Result<ManagedVault, OrchestratorError> {
    let row = sqlx::query_as::<_, ManagedVaultRow>(
        r#"
        SELECT id, settings, vault_index, vault_pubkey, active_policy_id, active, first_seen_at, last_seen_at
        FROM loyal_yield.managed_vaults
        WHERE id = $1
        FOR UPDATE
        "#,
    )
    .bind(vault_id.as_i64())
    .fetch_one(conn)
    .await?;

    Ok(managed_vault_from_row(row))
}

async fn app_position_tables_exist(conn: &mut PgConnection) -> Result<bool, OrchestratorError> {
    let exists = sqlx::query_scalar::<_, bool>(
        r#"
        SELECT to_regclass('loyal_yield.user_yield_positions') IS NOT NULL
           AND to_regclass('loyal_yield.user_yield_position_holding_events') IS NOT NULL
        "#,
    )
    .fetch_one(conn)
    .await?;

    Ok(exists)
}

async fn close_zero_user_yield_positions_for_vault(
    conn: &mut PgConnection,
    vault: &ManagedVault,
    snapshot_id: SnapshotId,
    observed_slot: i64,
    observed_at: DateTime<Utc>,
    snapshot_context: &Value,
) -> Result<u64, OrchestratorError> {
    if should_skip_zero_user_yield_position_close(snapshot_context) {
        return Ok(0);
    }

    if !app_position_tables_exist(conn).await? {
        return Ok(0);
    }

    let total_current_amount = sqlx::query_scalar::<_, i64>(
        r#"
        SELECT COALESCE(SUM(amount_raw), 0)::bigint
        FROM loyal_yield.vault_reserve_positions_current
        WHERE vault_id = $1
        "#,
    )
    .bind(vault.id.as_i64())
    .fetch_one(&mut *conn)
    .await?;

    if total_current_amount > 0 {
        return Ok(0);
    }

    let result = sqlx::query(
        r#"
        WITH active_positions AS (
            SELECT
                id,
                current_reserve,
                current_market,
                current_liquidity_mint,
                current_amount_raw,
                principal_amount_raw
            FROM loyal_yield.user_yield_positions
            WHERE settings = $1
              AND vault_index = $2
              AND vault_pubkey = $3
              AND status::text = 'active'
              AND current_observed_slot <= $4
            FOR UPDATE
        ),
        inserted_events AS (
            INSERT INTO loyal_yield.user_yield_position_holding_events (
                position_id,
                event_type,
                reserve,
                market,
                liquidity_mint,
                amount_raw,
                principal_delta_raw,
                holding_delta_raw,
                observed_slot,
                observed_at,
                source_snapshot_id,
                created_at
            )
            SELECT
                id,
                'snapshot_reconciled'::loyal_yield.user_yield_holding_event_type,
                current_reserve,
                current_market,
                current_liquidity_mint,
                0,
                -principal_amount_raw,
                -current_amount_raw,
                $4,
                $5,
                $6,
                now()
            FROM active_positions
            RETURNING id, position_id
        )
        UPDATE loyal_yield.user_yield_positions p
        SET
            smart_account_address = $7,
            principal_amount_raw = 0,
            current_amount_raw = 0,
            current_observed_slot = $4,
            current_observed_at = $5,
            last_holding_event_id = inserted_events.id,
            status = 'closed'::loyal_yield.yield_position_status,
            updated_at = now()
        FROM inserted_events
        WHERE p.id = inserted_events.position_id
        "#,
    )
    .bind(&vault.settings)
    .bind(vault.vault_index)
    .bind(&vault.vault_pubkey)
    .bind(observed_slot)
    .bind(observed_at)
    .bind(snapshot_id.as_i64())
    .bind(&vault.vault_pubkey)
    .execute(conn)
    .await?;

    Ok(result.rows_affected())
}

fn should_skip_zero_user_yield_position_close(snapshot_context: &Value) -> bool {
    let kind = snapshot_context.get("kind").and_then(Value::as_str);
    if kind == Some(SAME_MINT_CHAIN_RECONCILE_PREVIEW_KIND) {
        return true;
    }

    // Collateral snapshots are planner input, so zero deposited collateral is not on its
    // own proof that app-visible Earn principal is gone: a rebalance parks the funds in
    // the vault's own token account between the withdraw and the deposit, and a snapshot
    // taken in that window reads zero while nothing was lost.
    //
    // What settles it is the idle balance recorded alongside. Zero deposited *and* zero
    // idle means the funds left the vault entirely, which is the one reading that
    // justifies closing a user's position. Anything else — funds still parked, or a
    // snapshot that never recorded the idle balance at all — stays skipped, because the
    // cost of closing a live position far exceeds the cost of leaving a dead one open.
    let amount_semantics = snapshot_context
        .get("amount_semantics")
        .and_then(Value::as_str);
    if amount_semantics != Some(AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED) {
        return false;
    }
    snapshot_context
        .get("idle_vault_liquidity_amount_raw")
        .and_then(json_u128)
        .is_none_or(|idle| idle != 0)
}

/// Reads a non-negative integer that may have been stored as a number or a string.
///
/// Raw token amounts exceed what JSON numbers carry safely, so writers legitimately emit
/// them either way. A value that cannot be read as one is rejected rather than defaulted:
/// this feeds a close decision, where an unreadable balance must not pass as zero.
fn json_u128(value: &Value) -> Option<u128> {
    value
        .as_u64()
        .map(u128::from)
        .or_else(|| value.as_str().and_then(|raw| raw.parse::<u128>().ok()))
}

async fn fetch_rebalance_input_vault_for_update(
    conn: &mut PgConnection,
    input: &SameMintRebalanceInput,
) -> Result<ManagedVault, OrchestratorError> {
    if let Some(vault_id) = input.vault_id {
        return fetch_managed_vault_for_update(conn, vault_id).await;
    }

    let settings = input.settings.as_deref().ok_or_else(|| {
        OrchestratorError::SameMintRebalanceValidation(
            "settings is required when vault_id is omitted".to_owned(),
        )
    })?;
    let vault_index = input.vault_index.ok_or_else(|| {
        OrchestratorError::SameMintRebalanceValidation(
            "vault_index is required when vault_id is omitted".to_owned(),
        )
    })?;

    let row = sqlx::query_as::<_, ManagedVaultRow>(
        r#"
        SELECT id, settings, vault_index, vault_pubkey, active_policy_id, active, first_seen_at, last_seen_at
        FROM loyal_yield.managed_vaults
        WHERE settings = $1 AND vault_index = $2 AND active
        ORDER BY last_seen_at DESC, id DESC
        LIMIT 1
        FOR UPDATE
        "#,
    )
    .bind(settings)
    .bind(vault_index)
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
    let source_snapshot_id = planned.source_snapshot_id.map(SnapshotId::as_i64);
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
        source_snapshot_id,
        planned.source_reserve.as_deref(),
        &planned.target_reserve,
        planned.liquidity_mint.as_deref(),
        &planned.source_liquidity_mint,
        &planned.target_liquidity_mint,
        planned.amount_raw,
        planned.source_apy_bps,
        planned.target_apy_bps,
        planned.estimated_edge_bps,
        estimated_cost_lamports,
        planned.decision_reason.as_str(),
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

async fn update_confirmed_decision(
    conn: &mut PgConnection,
    decision_id: DecisionId,
    signature: &str,
    submitted_slot: Option<i64>,
    confirmed_slot: i64,
    post_snapshot_id: SnapshotId,
) -> Result<RebalanceDecision, OrchestratorError> {
    let row = sqlx::query_as!(
        DecisionRow,
        r#"
        UPDATE loyal_yield.rebalance_decisions
        SET
            status = 'confirmed'::loyal_yield.decision_status,
            signature = $2,
            submitted_slot = COALESCE($3, submitted_slot),
            confirmed_slot = $4,
            post_snapshot_id = $5,
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
        signature,
        submitted_slot,
        confirmed_slot,
        post_snapshot_id.as_i64()
    )
    .fetch_one(conn)
    .await?;
    from_row_to_decision(row)
}

fn validate_same_mint_input(
    input: &SameMintRebalanceInput,
    positions: &[CurrentReservePosition],
    queue_opportunity: Option<&RebalanceOpportunityRecord>,
) -> Result<(), String> {
    if input.source_reserve == input.target_reserve {
        return Err("source and target reserve must differ".to_owned());
    }
    if input.amount_raw <= 0 {
        return Err("amount_raw must be greater than 0".to_owned());
    }

    let source = positions
        .iter()
        .find(|position| position.reserve == input.source_reserve)
        .ok_or_else(|| "source reserve is not in current positions".to_owned())?;
    let target = positions
        .iter()
        .find(|position| position.reserve == input.target_reserve)
        .ok_or_else(|| "target reserve is not in current positions".to_owned())?;

    if source.snapshot_id != input.expected_source_snapshot_id {
        return Err("current source snapshot does not match expected snapshot".to_owned());
    }
    if source.amount_raw <= 0 || !source.has_value {
        return Err("source reserve has no value".to_owned());
    }
    if input.route_amount_semantics != ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY {
        return Err(format!(
            "unsupported_amount_semantics: route_amount_semantics must be {ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY}"
        ));
    }
    let evidence = route_amount_evidence(source).ok_or_else(|| {
        format!(
            "unsupported_amount_semantics: source reserve amount_semantics {:?} cannot route as planned liquidity mint {}",
            source
                .planning_metadata
                .get("amount_semantics")
                .and_then(Value::as_str),
            input.liquidity_mint
        )
    })?;
    let bounded_queue_accrual = queue_opportunity.is_some_and(|opportunity| {
        input.vault_id == Some(opportunity.vault_id)
            && opportunity.source_snapshot_id == Some(input.expected_source_snapshot_id)
            && opportunity.source_reserve.as_deref() == Some(input.source_reserve.as_str())
            && opportunity.target_reserve == input.target_reserve
            && opportunity.liquidity_mint == input.liquidity_mint
            && opportunity.amount_raw == evidence.amount_raw
            && evidence.redeemable_source_liquidity_amount_raw == Some(opportunity.amount_raw)
            && input.amount_raw > opportunity.amount_raw
            && i128::from(input.amount_raw - opportunity.amount_raw) * 1_000_000
                <= i128::from(opportunity.amount_raw)
                    * i128::from(MAX_QUEUE_POSITIVE_AMOUNT_DRIFT_PPM)
            && input.source_amount_semantics == evidence.source_amount_semantics
            && input.source_collateral_amount_raw == evidence.source_collateral_amount_raw
            && input.redeemable_source_liquidity_amount_raw == Some(input.amount_raw)
            && input.idle_vault_liquidity_amount_raw == evidence.idle_vault_liquidity_amount_raw
    });
    if input.amount_raw != evidence.amount_raw && !bounded_queue_accrual {
        return Err(format!(
            "amount_raw {} does not match routeable source liquidity amount {}",
            input.amount_raw, evidence.amount_raw
        ));
    }
    if input.source_amount_semantics != evidence.source_amount_semantics {
        return Err("source_amount_semantics does not match current source metadata".to_owned());
    }
    if input.source_collateral_amount_raw != evidence.source_collateral_amount_raw {
        return Err(
            "source_collateral_amount_raw does not match current source metadata".to_owned(),
        );
    }
    if input.redeemable_source_liquidity_amount_raw
        != evidence.redeemable_source_liquidity_amount_raw
        && !bounded_queue_accrual
    {
        return Err(
            "redeemable_source_liquidity_amount_raw does not match current source metadata"
                .to_owned(),
        );
    }
    if input.idle_vault_liquidity_amount_raw != evidence.idle_vault_liquidity_amount_raw {
        return Err(
            "idle_vault_liquidity_amount_raw does not match current source metadata".to_owned(),
        );
    }
    if source.liquidity_mint != input.liquidity_mint {
        return Err("source liquidity mint does not match input mint".to_owned());
    }
    if target.liquidity_mint != input.liquidity_mint {
        return Err("target liquidity mint does not match input mint".to_owned());
    }
    Ok(())
}

fn validate_planned_decision_input(
    input: &PlannedRebalanceDecisionInput,
) -> Result<(), OrchestratorError> {
    if input.source_liquidity_mint != input.target_liquidity_mint {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "planned decision source and target liquidity mints must match".to_owned(),
        ));
    }
    require_plan_string(
        &input.execution_plan,
        "liquidity_mint",
        &input.source_liquidity_mint,
    )?;
    require_plan_string(
        &input.execution_plan,
        "source_liquidity_mint",
        &input.source_liquidity_mint,
    )?;
    require_plan_string(
        &input.execution_plan,
        "target_liquidity_mint",
        &input.target_liquidity_mint,
    )?;
    let route_semantics = input
        .execution_plan
        .get("route_amount_semantics")
        .and_then(Value::as_str);
    if route_semantics != Some(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY) {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "same-mint planned decision requires redeemable_liquidity_amount route semantics"
                .to_owned(),
        ));
    }
    if json_i64(
        &input.execution_plan,
        "redeemable_source_liquidity_amount_raw",
    ) != Some(input.amount_raw)
    {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "same-mint planned decision routeable liquidity amount must match amount_raw"
                .to_owned(),
        ));
    }
    Ok(())
}

fn validate_idle_vault_deposit_decision_input(
    input: &IdleVaultDepositDecisionInput,
) -> Result<(), OrchestratorError> {
    if input.target_reserve.trim().is_empty() {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "idle vault deposit target reserve is required".to_owned(),
        ));
    }
    if input.liquidity_mint.trim().is_empty() {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "idle vault deposit liquidity mint is required".to_owned(),
        ));
    }
    if input.idle_token_account.trim().is_empty() {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "idle vault deposit token account is required".to_owned(),
        ));
    }
    if input.amount_raw <= 0 {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "idle vault deposit amount_raw must be greater than 0".to_owned(),
        ));
    }
    if input.idle_observed_slot < 0 {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "idle vault deposit observed slot must be non-negative".to_owned(),
        ));
    }
    if input.estimated_edge_bps <= 0 {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "idle vault deposit requires a positive edge".to_owned(),
        ));
    }
    if input.setup_obligation_vault_rent_top_up_lamports < 0 {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "idle vault deposit setup obligation rent top-up must be non-negative".to_owned(),
        ));
    }
    if input.setup_obligation_before_deposit {
        if input
            .setup_obligation_policy
            .as_deref()
            .unwrap_or_default()
            .trim()
            .is_empty()
        {
            return Err(OrchestratorError::SameMintRebalanceValidation(
                "idle vault deposit setup obligation policy is required when setup is planned"
                    .to_owned(),
            ));
        }
        if input
            .setup_obligation_policy_source
            .as_deref()
            .unwrap_or_default()
            .trim()
            .is_empty()
        {
            return Err(OrchestratorError::SameMintRebalanceValidation(
                "idle vault deposit setup obligation policy source is required when setup is planned"
                    .to_owned(),
            ));
        }
    } else if input.setup_obligation_vault_rent_top_up_lamports != 0 {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "idle vault deposit setup obligation rent top-up requires setup to be planned"
                .to_owned(),
        ));
    }
    Ok(())
}

async fn validate_idle_vault_source_for_update(
    conn: &mut PgConnection,
    vault: &ManagedVault,
    input: &IdleVaultDepositDecisionInput,
) -> Result<(), OrchestratorError> {
    let row = sqlx::query(
        r#"
        SELECT amount_raw, owner, token_account, observed_slot, observed_at
        FROM loyal_yield.vault_idle_token_balances_current
        WHERE vault_id = $1 AND mint = $2
        FOR UPDATE
        "#,
    )
    .bind(vault.id.as_i64())
    .bind(&input.liquidity_mint)
    .fetch_optional(conn)
    .await?
    .ok_or_else(|| {
        OrchestratorError::SameMintRebalanceValidation(
            "idle vault source disappeared before atomic fleet handoff".to_owned(),
        )
    })?;
    let amount_raw: i64 = row.try_get("amount_raw")?;
    let owner: String = row.try_get("owner")?;
    let token_account: String = row.try_get("token_account")?;
    let observed_slot: i64 = row.try_get("observed_slot")?;
    let observed_at: DateTime<Utc> = row.try_get("observed_at")?;
    if amount_raw != input.amount_raw
        || owner != vault.vault_pubkey
        || token_account != input.idle_token_account
        || !idle_source_observation_covers(
            observed_slot,
            observed_at,
            input.idle_observed_slot,
            input.idle_observed_at,
        )
    {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "idle vault source changed before atomic fleet handoff".to_owned(),
        ));
    }
    Ok(())
}

fn idle_source_observation_covers(
    current_slot: i64,
    current_observed_at: DateTime<Utc>,
    planned_slot: i64,
    planned_observed_at: DateTime<Utc>,
) -> bool {
    // A newer finalized observation of the same amount, owner, and token
    // account strengthens the source proof. Treating its provenance timestamp
    // as source identity lets the balance projector starve every queued route.
    current_slot >= planned_slot && current_observed_at >= planned_observed_at
}

fn idle_vault_deposit_execution_plan(input: &IdleVaultDepositDecisionInput) -> Value {
    let mut route_steps = Vec::new();
    if input.setup_obligation_before_deposit {
        if input.setup_obligation_vault_rent_top_up_lamports > 0 {
            route_steps.push(SYSTEM_TRANSFER_VAULT_RENT_TOP_UP_ROUTE_STEP_FOR_PLAN);
        }
        route_steps.push(KAMINO_INIT_OBLIGATION_ROUTE_STEP_FOR_PLAN);
    }
    route_steps.push(KAMINO_DEPOSIT_ROUTE_STEP_FOR_PLAN);
    let policy_executions = if input.setup_obligation_before_deposit {
        2
    } else {
        1
    };
    json!({
        "kind": "idle_vault_deposit",
        "source_kind": "idle_vault",
        "source_reserve": Value::Null,
        "target_reserve": input.target_reserve,
        "target_market": input.target_market,
        "liquidity_mint": input.liquidity_mint,
        "source_liquidity_mint": input.liquidity_mint,
        "target_liquidity_mint": input.liquidity_mint,
        "amount_raw": input.amount_raw,
        "route_amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
        "source_amount_semantics": "idle_vault",
        "source_collateral_amount_raw": Value::Null,
        "redeemable_source_liquidity_amount_raw": Value::Null,
        "idle_vault_liquidity_amount_raw": input.amount_raw,
        "idle_token_account": input.idle_token_account,
        "idle_observed_slot": input.idle_observed_slot,
        "observed_slot": input.idle_observed_slot,
        "idle_observed_at": input.idle_observed_at,
        "observed_at": input.idle_observed_at,
        "source_apy_bps": 0,
        "target_apy_bps": input.target_apy_bps,
        "target_supply_apy_bps": input.target_apy_bps,
        "estimated_edge_bps": input.estimated_edge_bps,
        "edge_bps": input.estimated_edge_bps,
        "policy_executions": policy_executions,
        "setup_obligation_before_deposit": input.setup_obligation_before_deposit,
        "setup_obligation_policy": input.setup_obligation_policy,
        "setup_obligation_policy_source": input.setup_obligation_policy_source,
        "setup_obligation_vault_rent_top_up_lamports": input.setup_obligation_vault_rent_top_up_lamports,
        "route_steps": route_steps,
    })
}

const SYSTEM_TRANSFER_VAULT_RENT_TOP_UP_ROUTE_STEP_FOR_PLAN: &str =
    "system_transfer_vault_rent_top_up";
const KAMINO_INIT_OBLIGATION_ROUTE_STEP_FOR_PLAN: &str = "kamino_init_obligation";
const KAMINO_DEPOSIT_ROUTE_STEP_FOR_PLAN: &str =
    "kamino_deposit_reserve_liquidity_and_obligation_collateral_v2";

fn require_plan_string(
    plan: &Value,
    field: &'static str,
    expected: &str,
) -> Result<(), OrchestratorError> {
    let actual = plan.get(field).and_then(Value::as_str).ok_or_else(|| {
        OrchestratorError::SameMintRebalanceValidation(format!(
            "same-mint planned decision execution_plan.{field} is missing"
        ))
    })?;
    if actual != expected {
        return Err(OrchestratorError::SameMintRebalanceValidation(format!(
            "same-mint planned decision execution_plan.{field} {actual} does not match {expected}"
        )));
    }
    Ok(())
}

fn json_i64(value: &Value, field: &str) -> Option<i64> {
    let value = value.get(field)?;
    value
        .as_i64()
        .or_else(|| value.as_u64().and_then(|amount| i64::try_from(amount).ok()))
        .or_else(|| value.as_str().and_then(|amount| amount.parse::<i64>().ok()))
}

fn same_mint_execution_plan(input: &SameMintRebalanceInput) -> Value {
    json!({
        "kind": "same_mint",
        "source_reserve": input.source_reserve,
        "target_reserve": input.target_reserve,
        "liquidity_mint": input.liquidity_mint,
        "source_liquidity_mint": input.liquidity_mint,
        "target_liquidity_mint": input.liquidity_mint,
        "amount_raw": input.amount_raw,
        "route_amount_semantics": input.route_amount_semantics,
        "source_amount_semantics": input.source_amount_semantics,
        "source_collateral_amount_raw": input.source_collateral_amount_raw,
        "redeemable_source_liquidity_amount_raw": input.redeemable_source_liquidity_amount_raw,
        "idle_vault_liquidity_amount_raw": input.idle_vault_liquidity_amount_raw,
        "policy_executions": 3,
        "init_obligation": "inline_if_missing",
        "route_steps": ["kamino_withdraw", "kamino_init_obligation_if_missing", "kamino_deposit"],
    })
}

fn same_mint_execution_preview(planned: &PlannedDecision) -> SameMintExecutionPreview {
    SameMintExecutionPreview {
        kind: "same_mint".to_owned(),
        source_reserve: planned.source_reserve.clone().unwrap_or_default(),
        target_reserve: planned.target_reserve.clone(),
        liquidity_mint: planned.source_liquidity_mint.clone(),
        amount_raw: planned.amount_raw,
        route_amount_semantics: planned.route_amount_semantics.clone(),
        source_amount_semantics: planned.source_amount_semantics.clone(),
        source_collateral_amount_raw: planned.source_collateral_amount_raw,
        redeemable_source_liquidity_amount_raw: planned.redeemable_source_liquidity_amount_raw,
        idle_vault_liquidity_amount_raw: planned.idle_vault_liquidity_amount_raw,
        policy_executions: 3,
        route_steps: vec![
            "kamino_withdraw".to_owned(),
            "kamino_init_obligation_if_missing".to_owned(),
            "kamino_deposit".to_owned(),
        ],
    }
}

fn same_mint_result_from_decision(
    vault_id: VaultId,
    input: SameMintRebalanceInput,
    decision: RebalanceDecision,
    skip_reason: Option<SkipReason>,
    execution_preview: Option<SameMintExecutionPreview>,
) -> SameMintRebalanceResult {
    SameMintRebalanceResult {
        vault_id,
        decision_id: Some(decision.id),
        status: decision.status,
        source_reserve: input.source_reserve,
        target_reserve: input.target_reserve,
        liquidity_mint: input.liquidity_mint,
        amount_raw: input.amount_raw,
        signature: decision.signature,
        confirmed_slot: decision.confirmed_slot,
        skip_reason,
        error_reason: None,
        dry_run: input.dry_run,
        execution_preview,
    }
}

fn same_mint_error_result(
    vault_id: VaultId,
    input: SameMintRebalanceInput,
    reason: String,
) -> SameMintRebalanceResult {
    SameMintRebalanceResult {
        vault_id,
        decision_id: None,
        status: DecisionStatus::Skipped,
        source_reserve: input.source_reserve,
        target_reserve: input.target_reserve,
        liquidity_mint: input.liquidity_mint,
        amount_raw: input.amount_raw,
        signature: None,
        confirmed_slot: None,
        skip_reason: None,
        error_reason: Some(reason),
        dry_run: input.dry_run,
        execution_preview: None,
    }
}

fn ensure_confirmable_same_mint_decision(
    decision: &RebalanceDecision,
) -> Result<(), OrchestratorError> {
    if decision.status != DecisionStatus::Confirming {
        return Err(OrchestratorError::TerminalDecision(decision.status));
    }
    if decision.liquidity_mint.is_none()
        || decision.source_liquidity_mint != decision.target_liquidity_mint
    {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "decision is not same-mint".to_owned(),
        ));
    }
    Ok(())
}

fn ensure_same_mint_route_amount_semantics(
    decision: &RebalanceDecision,
) -> Result<(), OrchestratorError> {
    let route_semantics = decision
        .execution_plan
        .get("route_amount_semantics")
        .and_then(Value::as_str);
    if route_semantics != Some(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY) {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "decision execution_plan.route_amount_semantics is not redeemable_liquidity_amount"
                .to_owned(),
        ));
    }
    if json_i64(
        &decision.execution_plan,
        "redeemable_source_liquidity_amount_raw",
    ) != decision.amount_raw
    {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "decision execution_plan.redeemable_source_liquidity_amount_raw must match amount_raw"
                .to_owned(),
        ));
    }
    Ok(())
}

fn same_mint_projection_metadata(
    decision: &RebalanceDecision,
    projection_role: &str,
    projected_amount_raw: i64,
) -> Value {
    json!({
        "source": "same_mint_confirmation_projection",
        "projection_role": projection_role,
        "decision_id": decision.id.as_i64(),
        "route_amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
        "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
        "projected_amount_raw": projected_amount_raw,
        "source_reserve": decision.source_reserve,
        "target_reserve": decision.target_reserve,
        "liquidity_mint": decision.liquidity_mint,
        "source_liquidity_mint": decision.source_liquidity_mint,
        "target_liquidity_mint": decision.target_liquidity_mint,
        "source_amount_semantics": decision.execution_plan.get("source_amount_semantics").cloned().unwrap_or(Value::Null),
        "source_collateral_amount_raw": json_i64(&decision.execution_plan, "source_collateral_amount_raw"),
        "redeemable_source_liquidity_amount_raw": json_i64(&decision.execution_plan, "redeemable_source_liquidity_amount_raw"),
        "idle_vault_liquidity_amount_raw": json_i64(&decision.execution_plan, "idle_vault_liquidity_amount_raw"),
    })
}

fn ensure_same_mint_confirm_repeat_matches(
    decision: &RebalanceDecision,
    input: &ConfirmSameMintRebalanceInput,
) -> Result<(), OrchestratorError> {
    if decision
        .signature
        .as_deref()
        .is_some_and(|stored| stored != input.signature)
    {
        return Err(OrchestratorError::ConflictingTerminalRepeat { field: "signature" });
    }
    if decision.confirmed_slot != Some(input.confirmed_slot) {
        return Err(OrchestratorError::ConflictingTerminalRepeat {
            field: "confirmed_slot",
        });
    }
    if input.post_snapshot_id.is_some() && decision.post_snapshot_id != input.post_snapshot_id {
        return Err(OrchestratorError::ConflictingTerminalRepeat {
            field: "post_snapshot_id",
        });
    }
    Ok(())
}

fn same_mint_result_from_confirmed_decision(
    decision: RebalanceDecision,
) -> SameMintRebalanceResult {
    SameMintRebalanceResult {
        vault_id: decision.vault_id,
        decision_id: Some(decision.id),
        status: decision.status,
        source_reserve: decision.source_reserve.unwrap_or_default(),
        target_reserve: decision.target_reserve.unwrap_or_default(),
        liquidity_mint: decision.liquidity_mint.unwrap_or_default(),
        amount_raw: decision.amount_raw.unwrap_or_default(),
        signature: decision.signature,
        confirmed_slot: decision.confirmed_slot,
        skip_reason: None,
        error_reason: None,
        dry_run: false,
        execution_preview: None,
    }
}

#[allow(clippy::items_after_test_module)]
#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::{
        IDLE_DEPOSIT_MINT_CASH, IDLE_DEPOSIT_MINT_PYUSD, IDLE_DEPOSIT_MINT_USDC,
        IDLE_DEPOSIT_MINT_USDG, IDLE_DEPOSIT_MINT_USDS, IDLE_DEPOSIT_MINT_USDT,
    };

    #[test]
    fn newer_idle_observation_covers_queued_source_proof() {
        let planned_at = Utc::now();
        assert!(idle_source_observation_covers(
            101,
            planned_at + chrono::Duration::seconds(1),
            100,
            planned_at,
        ));
        assert!(!idle_source_observation_covers(
            99,
            planned_at + chrono::Duration::seconds(1),
            100,
            planned_at,
        ));
        assert!(!idle_source_observation_covers(
            101,
            planned_at - chrono::Duration::seconds(1),
            100,
            planned_at,
        ));
    }

    fn valid_cross_mint_manifest() -> CrossMintSwapPolicyManifestInput {
        CrossMintSwapPolicyManifestInput {
            signature: "manifest-test-signature".to_owned(),
            slot: 1,
            cluster: "manifest-test-cluster".to_owned(),
            source_commitment: "finalized".to_owned(),
            mutation: "create".to_owned(),
            settings: "manifest-test-settings".to_owned(),
            authority: "manifest-test-authority".to_owned(),
            policy_seed: Some(1),
            policy_account: "manifest-test-policy".to_owned(),
            vault_index: 0,
            vault_pubkey: "manifest-test-vault".to_owned(),
            delegated_signer: "manifest-test-signer".to_owned(),
            source_shard: "token_2022".to_owned(),
            max_slippage_bps: 50,
            daily_source_mint_spending_cap: 1_000,
            manifest_fingerprint: "manifest-test-fingerprint".to_owned(),
        }
    }

    #[test]
    fn generalized_manifest_requires_source_shard() {
        let mut manifest = valid_cross_mint_manifest();
        manifest.source_shard = "unknown".to_owned();
        let error = validate_cross_mint_swap_policy_manifest_input(&manifest).unwrap_err();
        assert!(error.to_string().contains("source shard"));
    }

    #[test]
    fn generalized_manifest_rejects_zero_slippage() {
        let mut manifest = valid_cross_mint_manifest();
        manifest.max_slippage_bps = 0;
        assert!(validate_cross_mint_swap_policy_manifest_input(&manifest).is_err());
    }

    fn atomic_idle_state() -> (VaultId, ReconciledVaultState, Vec<CurrentIdleTokenBalance>) {
        let vault_id = VaultId(419);
        let observed_at = Utc::now();
        let position = |reserve: &str, mint: &str| ReconciledReservePosition {
            reserve: reserve.to_owned(),
            market: Some("market".to_owned()),
            liquidity_mint: mint.to_owned(),
            amount_raw: 0,
            supply_apy_bps: Some(100),
            borrow_apy_bps: None,
            planning_metadata: json!({}),
        };
        let balance = |mint: &str, token_account: &str| CurrentIdleTokenBalance {
            vault_id,
            mint: mint.to_owned(),
            amount_raw: 1,
            owner: "vault-owner".to_owned(),
            token_account: token_account.to_owned(),
            observed_slot: 77,
            observed_at,
            source_commitment: "finalized".to_owned(),
            updated_at: observed_at,
        };
        (
            vault_id,
            ReconciledVaultState {
                observed_slot: 77,
                observed_at: Some(observed_at),
                chain_slot: Some(77),
                lock_attempt_id: None,
                context: json!({"kind": "fleet_position_sweep"}),
                positions: vec![
                    position("usdc-main", IDLE_DEPOSIT_MINT_USDC),
                    position("usdc-prime", IDLE_DEPOSIT_MINT_USDC),
                    position("pyusd-main", IDLE_DEPOSIT_MINT_PYUSD),
                ],
            },
            vec![
                balance(IDLE_DEPOSIT_MINT_CASH, "cash-ata"),
                balance(IDLE_DEPOSIT_MINT_USDG, "usdg-ata"),
                balance(IDLE_DEPOSIT_MINT_PYUSD, "pyusd-ata"),
                balance(IDLE_DEPOSIT_MINT_USDC, "usdc-ata"),
                balance(IDLE_DEPOSIT_MINT_USDT, "usdt-ata"),
                balance(IDLE_DEPOSIT_MINT_USDS, "usds-ata"),
            ],
        )
    }

    #[test]
    fn atomic_idle_snapshot_requires_complete_one_row_per_mint_at_the_same_slot() {
        let (vault_id, state, balances) = atomic_idle_state();
        validate_atomic_idle_token_balances(vault_id, &state, Some("vault-owner"), &balances)
            .unwrap();

        assert!(validate_atomic_idle_token_balances(
            vault_id,
            &state,
            Some("vault-owner"),
            &balances[..1],
        )
        .unwrap_err()
        .to_string()
        .contains("requires exactly"));

        let mut wrong_slot = balances.clone();
        wrong_slot[1].observed_slot += 1;
        assert!(validate_atomic_idle_token_balances(
            vault_id,
            &state,
            Some("vault-owner"),
            &wrong_slot,
        )
        .unwrap_err()
        .to_string()
        .contains("does not match reconciled slot"));
    }

    #[test]
    fn atomic_idle_snapshot_rejects_duplicate_mints_and_wrong_vault_identity() {
        let (vault_id, state, balances) = atomic_idle_state();
        let mut duplicate = balances.clone();
        duplicate[1].mint = IDLE_DEPOSIT_MINT_USDC.to_owned();
        assert!(validate_atomic_idle_token_balances(
            vault_id,
            &state,
            Some("vault-owner"),
            &duplicate,
        )
        .unwrap_err()
        .to_string()
        .contains("repeats mint"));

        let mut wrong_vault = balances;
        wrong_vault[0].vault_id = VaultId(420);
        assert!(validate_atomic_idle_token_balances(
            vault_id,
            &state,
            Some("vault-owner"),
            &wrong_vault,
        )
        .unwrap_err()
        .to_string()
        .contains("does not match reconciled vault"));
    }

    #[test]
    fn atomic_idle_snapshot_accepts_all_policy_mints_and_rejects_unsupported_mints() {
        let (vault_id, state, mut balances) = atomic_idle_state();
        validate_atomic_idle_token_balances(vault_id, &state, Some("vault-owner"), &balances)
            .unwrap();

        balances[3].mint = "not-a-policy-mint".to_owned();
        assert!(validate_atomic_idle_token_balances(
            vault_id,
            &state,
            Some("vault-owner"),
            &balances,
        )
        .unwrap_err()
        .to_string()
        .contains("is not an Earn product mint"));
    }

    #[test]
    fn observed_subset_is_non_destructive_but_not_a_complete_epoch() {
        let (vault_id, state, balances) = atomic_idle_state();
        validate_observed_idle_token_balances(
            vault_id,
            &state,
            Some("vault-owner"),
            &balances[..1],
        )
        .unwrap();
        assert!(validate_atomic_idle_token_balances(
            vault_id,
            &state,
            Some("vault-owner"),
            &balances[..1],
        )
        .is_err());
    }

    #[test]
    fn atomic_idle_same_slot_repeat_is_equality_only() {
        let (_, _, balances) = atomic_idle_state();
        let existing = &balances[0];

        validate_same_slot_atomic_idle_repeat(existing, existing).unwrap();

        let mut conflicting_amount = existing.clone();
        conflicting_amount.amount_raw += 1;
        assert!(
            validate_same_slot_atomic_idle_repeat(existing, &conflicting_amount)
                .unwrap_err()
                .to_string()
                .contains("conflicts at observed slot")
        );

        let mut conflicting_account = existing.clone();
        conflicting_account.token_account = "different-usdc-ata".to_owned();
        assert!(
            validate_same_slot_atomic_idle_repeat(existing, &conflicting_account)
                .unwrap_err()
                .to_string()
                .contains("conflicts at observed slot")
        );
    }

    #[tokio::test]
    async fn atomic_idle_write_failure_rolls_back_reserve_epoch() {
        let (Ok(database_url), Ok(expected_data_dir)) = (
            std::env::var("EARN_ROUTER_ATOMIC_RECONCILE_TEST_DATABASE_URL"),
            std::env::var("EARN_ROUTER_ATOMIC_RECONCILE_TEST_DATA_DIR"),
        ) else {
            eprintln!(
                "skipping isolated Postgres rollback proof; use the dedicated verifier script"
            );
            return;
        };
        let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
            .await
            .unwrap();

        // This test mutates only a verifier-owned local Postgres cluster. Check
        // the server-reported data directory before applying any schema or row.
        let actual_data_dir: String = sqlx::query_scalar("SHOW data_directory")
            .fetch_one(client.pool())
            .await
            .unwrap();
        let expected_data_dir = std::fs::canonicalize(expected_data_dir).unwrap();
        let actual_data_dir = std::fs::canonicalize(actual_data_dir).unwrap();
        assert_eq!(actual_data_dir, expected_data_dir);
        assert!(actual_data_dir
            .file_name()
            .and_then(|name| name.to_str())
            .is_some_and(|name| name.starts_with("earn-router-atomic-")));
        let server_address: String =
            sqlx::query_scalar("SELECT COALESCE(inet_server_addr()::TEXT, '')")
                .fetch_one(client.pool())
                .await
                .unwrap();
        assert!(
            server_address.starts_with("127.0.0.1") || server_address.starts_with("::1"),
            "atomic rollback verifier requires loopback Postgres, got {server_address}"
        );

        client.apply_migrations().await.unwrap();
        run_atomic_idle_rollback_fixture(&client).await;
    }

    async fn run_atomic_idle_rollback_fixture(client: &NeonSqlClient) {
        let product_mints = supported_idle_deposit_mints();
        let policy_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.route_policies
                (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
                 delegated_signers, threshold, route_modes, stable_mints, kamino_markets,
                 kamino_liquidity_mints, swap_lanes, last_seen_slot, last_seen_signature)
            VALUES
                ('atomic-settings', 'atomic-authority', 1, 'atomic-policy', 0, 'vault-owner',
                 ARRAY['delegate'], 1, ARRAY['same_mint_kamino'], $1,
                 ARRAY['market'], $1, '[]'::jsonb, 1, 'signature')
            RETURNING id
            "#,
        )
        .bind(&product_mints)
        .fetch_one(client.pool())
        .await
        .unwrap();
        let vault_id = VaultId(
            sqlx::query_scalar(
                r#"
                INSERT INTO loyal_yield.managed_vaults
                    (settings, vault_index, vault_pubkey, active_policy_id)
                VALUES ('atomic-settings', 0, 'vault-owner', $1)
                RETURNING id
                "#,
            )
            .bind(policy_id)
            .fetch_one(client.pool())
            .await
            .unwrap(),
        );

        let (_, mut baseline_state, mut baseline_idle) = atomic_idle_state();
        for balance in &mut baseline_idle {
            balance.vault_id = vault_id;
        }
        client
            .publish_complete_vault(vault_id, baseline_state.clone(), baseline_idle.clone())
            .await
            .unwrap();

        sqlx::query(
            r#"
            CREATE FUNCTION loyal_yield.fail_atomic_idle_slot_78()
            RETURNS trigger LANGUAGE plpgsql AS $$
            BEGIN
                IF NEW.observed_slot = 78 THEN
                    RAISE EXCEPTION 'injected idle publication failure';
                END IF;
                RETURN NEW;
            END
            $$
            "#,
        )
        .execute(client.pool())
        .await
        .unwrap();
        sqlx::query(
            r#"
            CREATE TRIGGER fail_atomic_idle_slot_78
            BEFORE INSERT OR UPDATE ON loyal_yield.vault_idle_token_balances_current
            FOR EACH ROW EXECUTE FUNCTION loyal_yield.fail_atomic_idle_slot_78()
            "#,
        )
        .execute(client.pool())
        .await
        .unwrap();

        baseline_state.observed_slot = 78;
        baseline_state.chain_slot = Some(78);
        baseline_state.positions[0].amount_raw = 99;
        for balance in &mut baseline_idle {
            balance.observed_slot = 78;
            balance.amount_raw = 99;
        }
        let error = client
            .publish_complete_vault(vault_id, baseline_state, baseline_idle)
            .await
            .unwrap_err();
        assert!(error
            .to_string()
            .contains("injected idle publication failure"));

        let current_position_slots = sqlx::query_scalar::<_, i64>(
            "SELECT DISTINCT observed_slot FROM loyal_yield.vault_reserve_positions_current WHERE vault_id = $1",
        )
        .bind(vault_id.as_i64())
        .fetch_all(client.pool())
        .await
        .unwrap();
        let current_idle_slots = sqlx::query_scalar::<_, i64>(
            "SELECT DISTINCT observed_slot FROM loyal_yield.vault_idle_token_balances_current WHERE vault_id = $1",
        )
        .bind(vault_id.as_i64())
        .fetch_all(client.pool())
        .await
        .unwrap();
        let current_snapshot_slot: i64 = sqlx::query_scalar(
            "SELECT observed_slot FROM loyal_yield.vault_position_snapshots WHERE vault_id = $1 AND is_current",
        )
        .bind(vault_id.as_i64())
        .fetch_one(client.pool())
        .await
        .unwrap();
        let mixed_epoch_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*)::BIGINT
            FROM loyal_yield.vault_reserve_positions_current position
            JOIN loyal_yield.vault_idle_token_balances_current idle
              ON idle.vault_id = position.vault_id
            WHERE position.vault_id = $1
              AND position.observed_slot <> idle.observed_slot
            "#,
        )
        .bind(vault_id.as_i64())
        .fetch_one(client.pool())
        .await
        .unwrap();

        assert_eq!(current_position_slots, vec![77]);
        assert_eq!(current_idle_slots, vec![77]);
        assert_eq!(current_snapshot_slot, 77);
        assert_eq!(mixed_epoch_count, 0);

        sqlx::query("DROP TRIGGER fail_atomic_idle_slot_78 ON loyal_yield.vault_idle_token_balances_current")
            .execute(client.pool())
            .await
            .unwrap();
        let (_, mut subset_state, mut subset_idle) = atomic_idle_state();
        subset_state.observed_slot = 78;
        subset_state.chain_slot = Some(78);
        subset_state.positions.truncate(1);
        subset_idle.truncate(1);
        subset_idle[0].vault_id = vault_id;
        subset_idle[0].observed_slot = 78;
        client
            .apply_observed_patch_with_idle_token_balances(vault_id, subset_state, subset_idle)
            .await
            .unwrap();
        assert_eq!(complete_epoch_count(client, vault_id).await, 0);

        let (_, mut complete_state, mut complete_idle) = atomic_idle_state();
        complete_state.observed_slot = 79;
        complete_state.chain_slot = Some(79);
        for balance in &mut complete_idle {
            balance.vault_id = vault_id;
            balance.observed_slot = 79;
        }
        client
            .publish_complete_vault(vault_id, complete_state, complete_idle)
            .await
            .unwrap();
        assert_eq!(complete_epoch_count(client, vault_id).await, 1);
    }

    async fn complete_epoch_count(client: &NeonSqlClient, vault_id: VaultId) -> i64 {
        let product_mints = supported_idle_deposit_mints();
        sqlx::query_scalar(
            r#"
            SELECT count(*)::BIGINT
            FROM loyal_yield.vault_position_snapshots snapshot
            WHERE snapshot.vault_id = $1
              AND snapshot.is_current = TRUE
              AND snapshot.context->>'publication_scope' = 'complete_product_vault'
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.vault_reserve_positions_current position
                  WHERE position.vault_id = snapshot.vault_id
                    AND position.observed_slot <> snapshot.observed_slot
              )
              AND (
                  SELECT count(*)
                  FROM loyal_yield.vault_idle_token_balances_current idle
                  WHERE idle.vault_id = snapshot.vault_id
                    AND idle.mint = ANY($2::TEXT[])
                    AND idle.observed_slot = snapshot.observed_slot
              ) = cardinality($2::TEXT[])
            "#,
        )
        .bind(vault_id.as_i64())
        .bind(&product_mints)
        .fetch_one(client.pool())
        .await
        .unwrap()
    }

    fn same_mint_decision(execution_plan: Value, amount_raw: Option<i64>) -> RebalanceDecision {
        RebalanceDecision {
            id: DecisionId(229),
            vault_id: VaultId(419),
            source_snapshot_id: Some(SnapshotId(947)),
            status: DecisionStatus::Confirming,
            source_reserve: Some("source".to_owned()),
            target_reserve: Some("target".to_owned()),
            liquidity_mint: Some("USDC".to_owned()),
            source_liquidity_mint: Some("USDC".to_owned()),
            target_liquidity_mint: Some("USDC".to_owned()),
            amount_raw,
            source_apy_bps: Some(100),
            target_apy_bps: Some(200),
            estimated_edge_bps: Some(100),
            estimated_cost_lamports: 0,
            decision_reason: DecisionReason::TargetSupplyApyExceedsSource,
            execution_plan,
            abandon_reason: None,
            signature: None,
            submitted_slot: None,
            confirmed_slot: None,
            preflight_chain_slot: None,
            post_snapshot_id: None,
            created_at: Utc::now(),
            updated_at: Utc::now(),
        }
    }

    #[test]
    fn confirmation_rejects_missing_route_amount_semantics() {
        let decision = same_mint_decision(
            json!({
                "kind": "same_mint",
                "amount_raw": 404_323_479,
            }),
            Some(404_323_479),
        );

        let error = ensure_same_mint_route_amount_semantics(&decision).unwrap_err();

        assert!(matches!(
            error,
            OrchestratorError::SameMintRebalanceValidation(_)
        ));
    }

    #[test]
    fn zero_app_position_close_skips_chain_reconcile_preview() {
        // A preview is skipped on kind alone, even carrying proof of a zero idle
        // balance, because it describes an intended state rather than an observed one.
        let context = json!({
            "kind": SAME_MINT_CHAIN_RECONCILE_PREVIEW_KIND,
            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
            "idle_vault_liquidity_amount_raw": 0,
        });

        assert!(should_skip_zero_user_yield_position_close(&context));
    }

    #[test]
    fn zero_app_position_close_skips_collateral_snapshot_without_idle_evidence() {
        let context = json!({
            "source": "some_other_reconcile",
            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
        });

        assert!(should_skip_zero_user_yield_position_close(&context));
    }

    #[test]
    fn zero_app_position_close_skips_collateral_snapshot_with_funds_still_parked() {
        // The rebalance window: nothing deposited, but the funds are sitting in the
        // vault waiting to be moved onward. Closing here would erase a live position.
        let context = json!({
            "source": "fleet_position_sweep",
            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
            "idle_vault_liquidity_amount_raw": 1_000_003,
        });

        assert!(should_skip_zero_user_yield_position_close(&context));
    }

    #[test]
    fn zero_app_position_close_allows_collateral_snapshot_with_zero_idle_balance() {
        let context = json!({
            "source": "fleet_position_sweep",
            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
            "idle_vault_liquidity_amount_raw": 0,
        });

        assert!(!should_skip_zero_user_yield_position_close(&context));
    }

    #[test]
    fn zero_app_position_close_reads_string_encoded_idle_balances() {
        // Raw amounts outrun JSON's safe integer range, so writers may emit strings.
        // An unreadable value must not be mistaken for a zero balance.
        let zero = json!({
            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
            "idle_vault_liquidity_amount_raw": "0",
        });
        let funded = json!({
            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
            "idle_vault_liquidity_amount_raw": "1000003",
        });
        let unreadable = json!({
            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
            "idle_vault_liquidity_amount_raw": Value::Null,
        });

        assert!(!should_skip_zero_user_yield_position_close(&zero));
        assert!(should_skip_zero_user_yield_position_close(&funded));
        assert!(should_skip_zero_user_yield_position_close(&unreadable));
    }

    #[test]
    fn zero_app_position_close_allows_non_chain_redeemable_context() {
        let context = json!({
            "source": "frontend_position_reconcile",
            "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
        });

        assert!(!should_skip_zero_user_yield_position_close(&context));
    }

    #[test]
    fn confirmation_projection_metadata_preserves_routeable_units() {
        let decision = same_mint_decision(
            json!({
                "kind": "same_mint",
                "amount_raw": 480_000_000,
                "route_amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                "source_amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
                "source_collateral_amount_raw": 404_323_479,
                "redeemable_source_liquidity_amount_raw": 480_000_000,
                "idle_vault_liquidity_amount_raw": 75_676_540,
            }),
            Some(480_000_000),
        );

        ensure_same_mint_route_amount_semantics(&decision).expect("routeable plan is confirmable");
        let metadata =
            same_mint_projection_metadata(&decision, "target_after_confirm", 480_000_000);

        assert_eq!(
            metadata.get("amount_semantics").and_then(Value::as_str),
            Some(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY)
        );
        assert_eq!(
            metadata
                .get("source_collateral_amount_raw")
                .and_then(Value::as_i64),
            Some(404_323_479)
        );
        assert_eq!(
            metadata
                .get("redeemable_source_liquidity_amount_raw")
                .and_then(Value::as_i64),
            Some(480_000_000)
        );
    }
}

async fn lock_projection_offset(
    connection: &mut PgConnection,
    consumer_name: &str,
) -> Result<i64, OrchestratorError> {
    let row = sqlx::query(
        r#"
        INSERT INTO loyal_yield.projection_offsets (consumer_name, last_event_id)
        VALUES ($1, 0)
        ON CONFLICT (consumer_name) DO UPDATE
        SET consumer_name = EXCLUDED.consumer_name
        RETURNING last_event_id
        "#,
    )
    .bind(consumer_name)
    .fetch_one(&mut *connection)
    .await?;

    let last_event_id: i64 = row.try_get("last_event_id")?;
    let locked: i64 = sqlx::query_scalar(
        r#"
        SELECT last_event_id
        FROM loyal_yield.projection_offsets
        WHERE consumer_name = $1
        FOR UPDATE
        "#,
    )
    .bind(consumer_name)
    .fetch_one(&mut *connection)
    .await?;

    Ok(locked.max(last_event_id))
}

async fn record_wallet_ata_balance_update_in_tx(
    connection: &mut PgConnection,
    input: WalletAtaBalanceUpdateInput,
) -> Result<WalletAtaBalanceCurrent, OrchestratorError> {
    let amount_raw = to_i64_amount(input.amount_raw)?;
    let observed_slot = to_i64_slot(input.observed_slot)?;
    let row = sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_wallet_balances_current
            (target_id, wallet, wallet_usdc_ata, wallet_token_ata, amount_raw, owner, mint,
             observed_slot, observed_at, source, source_commitment, txn_signature, account_data_hash, raw_evidence)
        VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8, COALESCE($9, now()), $10, $11, $12, $13, $14)
        ON CONFLICT (target_id, mint) DO UPDATE SET
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
            wallet_token_ata = CASE
                WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                THEN EXCLUDED.wallet_token_ata
                ELSE loyal_yield.balance_sweep_wallet_balances_current.wallet_token_ata
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
            txn_signature = CASE
                WHEN EXCLUDED.observed_slot >= loyal_yield.balance_sweep_wallet_balances_current.observed_slot
                THEN EXCLUDED.txn_signature
                ELSE loyal_yield.balance_sweep_wallet_balances_current.txn_signature
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
            target_id, wallet, COALESCE(wallet_usdc_ata, wallet_token_ata) AS wallet_usdc_ata,
            wallet_token_ata, amount_raw, owner, mint,
            observed_slot, observed_at, source, source_commitment, txn_signature, account_data_hash,
            raw_evidence, updated_at
        "#,
    )
    .bind(input.target_id.as_i64())
    .bind(&input.wallet)
    .bind(&input.wallet_usdc_ata)
    .bind(&input.wallet_token_ata)
    .bind(amount_raw)
    .bind(input.owner.as_deref())
    .bind(&input.mint)
    .bind(observed_slot)
    .bind(input.observed_at)
    .bind(&input.source)
    .bind(&input.source_commitment)
    .bind(input.txn_signature.as_deref())
    .bind(input.account_data_hash.as_deref())
    .bind(&input.raw_evidence)
    .fetch_one(&mut *connection)
    .await?;

    wallet_ata_balance_from_row(&row)
}

async fn record_projected_wallet_ata_balance_update_in_tx(
    connection: &mut PgConnection,
    event_id: i64,
    input: WalletAtaBalanceUpdateInput,
) -> Result<WalletAtaBalanceCurrent, OrchestratorError> {
    let amount_raw = to_i64_amount(input.amount_raw)?;
    let observed_slot = to_i64_slot(input.observed_slot)?;
    let previous_amount_raw: Option<i64> = sqlx::query_scalar(
        r#"
        SELECT amount_raw
        FROM loyal_yield.balance_sweep_wallet_balances_current
        WHERE target_id = $1
          AND mint = $2
        FOR UPDATE
        "#,
    )
    .bind(input.target_id.as_i64())
    .bind(&input.mint)
    .fetch_optional(&mut *connection)
    .await?;
    let delta_amount_raw = previous_amount_raw.map(|previous| amount_raw - previous);

    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_wallet_balance_events
            (event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata, mint, previous_amount_raw, amount_raw,
             delta_amount_raw, observed_slot, observed_at, source, source_commitment,
             txn_signature, account_data_hash, raw_evidence)
        VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9, $10, COALESCE($11, now()), $12, $13, $14, $15, $16)
        ON CONFLICT (event_id) DO NOTHING
        "#,
    )
    .bind(event_id)
    .bind(input.target_id.as_i64())
    .bind(&input.wallet)
    .bind(&input.wallet_usdc_ata)
    .bind(&input.wallet_token_ata)
    .bind(&input.mint)
    .bind(previous_amount_raw)
    .bind(amount_raw)
    .bind(delta_amount_raw)
    .bind(observed_slot)
    .bind(input.observed_at)
    .bind(&input.source)
    .bind(&input.source_commitment)
    .bind(input.txn_signature.as_deref())
    .bind(input.account_data_hash.as_deref())
    .bind(&input.raw_evidence)
    .execute(&mut *connection)
    .await?;

    record_wallet_ata_balance_update_in_tx(connection, input).await
}

fn required_decision_field<'a>(
    value: &'a Option<String>,
    field: &'static str,
) -> Result<&'a str, OrchestratorError> {
    value
        .as_deref()
        .ok_or_else(|| OrchestratorError::StoreInvariant(format!("missing {field}")))
}

fn route_policy_from_row(row: RoutePolicyRow) -> RoutePolicy {
    RoutePolicy {
        id: PolicyId(row.id),
        cluster: row.cluster,
        source_commitment: row.source_commitment,
        finalized_eligible: row.finalized_eligible,
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

fn cross_mint_swap_policy_from_row(
    row: CrossMintSwapPolicyRow,
) -> Result<CrossMintSwapPolicy, OrchestratorError> {
    let vault_index = row.vault_index.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "active cross-mint policy row is missing its vault index".to_owned(),
        )
    })?;
    let vault_pubkey = row.vault_pubkey.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "active cross-mint policy row is missing its vault pubkey".to_owned(),
        )
    })?;
    let delegated_signer = row.delegated_signer.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "active cross-mint policy row is missing its delegated signer".to_owned(),
        )
    })?;
    let source_shard = row.source_shard.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "active cross-mint policy row is missing its source shard".to_owned(),
        )
    })?;
    let max_slippage_bps = row.max_slippage_bps.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "active cross-mint policy row is missing its slippage cap".to_owned(),
        )
    })?;
    let daily_source_mint_spending_cap = row.daily_source_mint_spending_cap.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "active cross-mint policy row is missing its spending cap".to_owned(),
        )
    })?;
    let manifest_fingerprint = row.manifest_fingerprint.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "active cross-mint policy row is missing its manifest fingerprint".to_owned(),
        )
    })?;
    Ok(CrossMintSwapPolicy {
        id: row.id,
        cluster: row.cluster,
        settings: row.settings,
        authority: row.authority,
        policy_seed: row.policy_seed,
        policy_account: row.policy_account,
        vault_index,
        vault_pubkey,
        delegated_signer,
        source_shard,
        max_slippage_bps,
        daily_source_mint_spending_cap,
        manifest_fingerprint,
        active: row.active,
        start_eligible: row.start_eligible,
        last_mutation: row.last_mutation,
        source_commitment: row.source_commitment,
        first_seen_at: row.first_seen_at,
        last_seen_at: row.last_seen_at,
        last_seen_slot: row.last_seen_slot,
        last_seen_signature: row.last_seen_signature,
    })
}

fn cross_mint_vault_opt_in_from_row(
    row: CrossMintVaultOptInRow,
) -> Result<CrossMintVaultOptIn, OrchestratorError> {
    if row.vault_index < 0 || row.generation <= 0 {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint opt-in row has an invalid vault index or generation".to_owned(),
        ));
    }
    Ok(CrossMintVaultOptIn {
        cluster: row.cluster,
        settings: row.settings,
        vault_index: row.vault_index,
        vault_pubkey: row.vault_pubkey,
        enabled: row.enabled,
        generation: row.generation,
        created_at: row.created_at,
        updated_at: row.updated_at,
    })
}

fn managed_vault_from_row(row: ManagedVaultRow) -> ManagedVault {
    ManagedVault {
        id: VaultId(row.id),
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
        settings: row.try_get("settings")?,
        authority: row.try_get("authority")?,
        policy_seed: row.try_get("policy_seed")?,
        policy_account: row.try_get("policy_account")?,
        vault_index: row.try_get("vault_index")?,
        vault_pubkey: row.try_get("vault_pubkey")?,
        wallet: row.try_get("wallet")?,
        wallet_usdc_ata: row.try_get("wallet_usdc_ata")?,
        vault_usdc_ata: row.try_get("vault_usdc_ata")?,
        token_mint: row.try_get("token_mint")?,
        wallet_token_ata: row.try_get("wallet_token_ata")?,
        vault_token_ata: row.try_get("vault_token_ata")?,
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
        wallet: row.try_get("wallet")?,
        wallet_usdc_ata: row.try_get("wallet_usdc_ata")?,
        wallet_token_ata: row.try_get("wallet_token_ata")?,
        amount_raw: row.try_get("amount_raw")?,
        owner: row.try_get("owner")?,
        mint: row.try_get("mint")?,
        observed_slot: row.try_get("observed_slot")?,
        observed_at: row.try_get("observed_at")?,
        source: row.try_get("source")?,
        source_commitment: row.try_get("source_commitment")?,
        txn_signature: row.try_get("txn_signature")?,
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
        signature: row.try_get("signature")?,
        slot: row.try_get("slot")?,
        source_wallet_ata: row.try_get("source_wallet_ata")?,
        destination_vault_ata: row.try_get("destination_vault_ata")?,
        token_mint: row.try_get("token_mint")?,
        source_token_ata: row.try_get("source_token_ata")?,
        destination_token_ata: row.try_get("destination_token_ata")?,
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

async fn route_lookup_tables_relation_exists(pool: &PgPool) -> Result<bool, sqlx::Error> {
    let exists: Option<String> =
        sqlx::query_scalar("SELECT to_regclass('loyal_yield.route_lookup_tables')::text")
            .fetch_one(pool)
            .await?;
    Ok(exists.is_some())
}

fn route_lookup_table_from_row(row: sqlx::postgres::PgRow) -> RouteLookupTable {
    RouteLookupTable {
        id: row.get("id"),
        cluster: row.get("cluster"),
        scope: row.get("scope"),
        table_address: row.get("table_address"),
        authority: row.get("authority"),
        payer: row.get("payer"),
        status: row.get("status"),
        durable: row.get("durable"),
        address_count: row.get("address_count"),
        address_hash: row.get("address_hash"),
        addresses: row.get("addresses"),
        create_signature: row.get("create_signature"),
        extend_signatures: row.get("extend_signatures"),
        last_extended_slot: row.get("last_extended_slot"),
        warmup_slot: row.get("warmup_slot"),
        deactivated_slot: row.get("deactivated_slot"),
        deactivate_signature: row.get("deactivate_signature"),
        closed_signature: row.get("closed_signature"),
        close_recipient: row.get("close_recipient"),
        reclaimed_lamports: row.get("reclaimed_lamports"),
        notes: row.get("notes"),
        created_at: row.get("created_at"),
        updated_at: row.get("updated_at"),
    }
}

fn route_lookup_table_lock_key(cluster: &str, scope: &str, authority: &str) -> String {
    format!("route_lookup_table:{cluster}:{scope}:{authority}")
}

fn current_idle_token_balance_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<CurrentIdleTokenBalance, OrchestratorError> {
    Ok(CurrentIdleTokenBalance {
        vault_id: VaultId(row.try_get("vault_id")?),
        mint: row.try_get("mint")?,
        amount_raw: row.try_get("amount_raw")?,
        owner: row.try_get("owner")?,
        token_account: row.try_get("token_account")?,
        observed_slot: row.try_get("observed_slot")?,
        observed_at: row.try_get("observed_at")?,
        source_commitment: row.try_get("source_commitment")?,
        updated_at: row.try_get("updated_at")?,
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
    snapshot_id: Option<SnapshotId>,
    planned: &PlannedDecision,
) -> String {
    let mut hasher = Sha256::new();
    hasher.update(vault_id.as_i64().to_le_bytes());
    if let Some(snapshot_id) = snapshot_id {
        hasher.update(b"source_snapshot_id");
        hasher.update(snapshot_id.as_i64().to_le_bytes());
    } else {
        hasher.update(b"no_source_snapshot_id");
    }
    if let Some(source_reserve) = &planned.source_reserve {
        hasher.update(b"source_reserve");
        hasher.update(source_reserve.as_bytes());
    } else {
        hasher.update(b"no_source_reserve");
    }
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

async fn upsert_autodeposit_reconciliation_request(
    tx: &mut Transaction<'_, Postgres>,
    target_id: BalanceSweepTargetId,
    requested_slot: i64,
) -> Result<bool, OrchestratorError> {
    let row = sqlx::query(
        r#"
        INSERT INTO loyal_yield.autodeposit_reconciliation_requests
            (target_id, requested_slot)
        VALUES ($1, $2)
        ON CONFLICT (target_id) DO UPDATE SET
            requested_slot = EXCLUDED.requested_slot,
            next_attempt_at = LEAST(
                loyal_yield.autodeposit_reconciliation_requests.next_attempt_at,
                NOW()
            ),
            updated_at = NOW()
        WHERE EXCLUDED.requested_slot
            > loyal_yield.autodeposit_reconciliation_requests.requested_slot
        RETURNING target_id
        "#,
    )
    .bind(target_id.as_i64())
    .bind(requested_slot)
    .fetch_optional(&mut **tx)
    .await?;
    Ok(row.is_some())
}

fn to_i64_slot(slot: u64) -> Result<i64, OrchestratorError> {
    i64::try_from(slot).map_err(|_| OrchestratorError::SlotOutOfRange(slot))
}

fn to_i64_policy_seed(policy_seed: u64) -> Result<i64, OrchestratorError> {
    i64::try_from(policy_seed).map_err(|_| OrchestratorError::PolicySeedOutOfRange(policy_seed))
}

fn nonnegative_i64_to_u64(value: i64, field: &str) -> Result<u64, OrchestratorError> {
    u64::try_from(value)
        .map_err(|_| OrchestratorError::StoreInvariant(format!("{field} {value} is negative")))
}

fn policy_match_from_dynamic_row(
    row: &sqlx::postgres::PgRow,
) -> Result<PolicyMatchInput, OrchestratorError> {
    Ok(PolicyMatchInput {
        signature: row.try_get("last_seen_signature")?,
        slot: nonnegative_i64_to_u64(row.try_get("last_seen_slot")?, "policy slot")?,
        cluster: row.try_get("cluster")?,
        source_commitment: row.try_get("source_commitment")?,
        settings: row.try_get("settings")?,
        authority: row.try_get("authority")?,
        policy_seed: nonnegative_i64_to_u64(row.try_get("policy_seed")?, "policy seed")?,
        policy_account: row.try_get("policy_account")?,
        vault_index: u8::try_from(row.try_get::<i16, _>("vault_index")?).map_err(|_| {
            OrchestratorError::StoreInvariant("policy vault index is outside u8".to_owned())
        })?,
        vault_pubkey: row.try_get("vault_pubkey")?,
        delegated_signers: row.try_get("delegated_signers")?,
        threshold: u16::try_from(row.try_get::<i32, _>("threshold")?).map_err(|_| {
            OrchestratorError::StoreInvariant("policy threshold is outside u16".to_owned())
        })?,
        route_modes: row.try_get("route_modes")?,
        stable_mints: row.try_get("stable_mints")?,
        kamino_markets: row.try_get("kamino_markets")?,
        kamino_liquidity_mints: row.try_get("kamino_liquidity_mints")?,
        universe_preset: row.try_get("universe_preset")?,
        risk_profile: row.try_get("risk_profile")?,
        swap_lanes: row.try_get("swap_lanes")?,
    })
}

fn optional_to_i64_amount(amount: Option<u64>) -> Result<Option<i64>, OrchestratorError> {
    amount.map(to_i64_amount).transpose()
}
