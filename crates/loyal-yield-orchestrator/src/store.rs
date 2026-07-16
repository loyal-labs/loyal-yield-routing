use crate::domain::{
    draft_same_mint_decision, route_amount_evidence, state_transition, PlannedDecision,
    AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED, MAX_QUEUE_POSITIVE_AMOUNT_DRIFT_PPM,
    ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
};
use crate::fleet_orchestration::{
    RebalanceOpportunityLease, RebalanceOpportunityRecord, SignedRouteSubmissionInput,
    SignedRouteSubmissionRecord, TargetCapacityReservationInput,
};
use crate::types::*;
use crate::{OrchestratorError, ACTIVE_DECISION_STATUSES};
use chrono::{DateTime, Utc};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use sqlx::postgres::PgPoolOptions;
use sqlx::{PgConnection, PgPool, Postgres, Row, Transaction};
use std::future::Future;

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
const LIVE_MIGRATION_0008_CHECKSUM: &str =
    "d20151ef6d6076961195da6c6cf3b4e11bb3e2045f729bdf4b118f6c7d3ddc34";
const SAME_MINT_CHAIN_RECONCILE_PREVIEW_KIND: &str = "same_mint_chain_reconcile_preview";

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
        ] {
            apply_store_migration(&self.pool, migration).await?;
        }
        Ok(())
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
        let policy = upsert_policy(&mut *tx, &event).await?;
        let vault = upsert_vault(&mut *tx, policy.id, &event).await?;
        tx.commit().await?;
        Ok(StoredPolicyMatch { policy, vault })
    }

    pub async fn record_route_and_setup_policy_match(
        &self,
        route_event: PolicyMatchInput,
        setup_event: PolicyMatchInput,
    ) -> Result<(StoredPolicyMatch, RoutePolicy), OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let route_policy = upsert_policy(&mut *tx, &route_event).await?;
        let setup_policy = upsert_policy(&mut *tx, &setup_event).await?;
        let vault =
            upsert_vault_with_setup(&mut *tx, route_policy.id, setup_policy.id, &route_event)
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
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.balance_sweep_targets
                (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
                 wallet, wallet_usdc_ata, vault_usdc_ata, token_mint, wallet_token_ata,
                 vault_token_ata, delegated_signers, threshold, max_amount_per_period, active,
                 last_seen_slot, last_seen_signature)
            VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), $10, $11, $12, $13, $14, $15, TRUE, $16, $17)
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
                id, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
                wallet,
                COALESCE(wallet_usdc_ata, wallet_token_ata) AS wallet_usdc_ata,
                COALESCE(vault_usdc_ata, vault_token_ata) AS vault_usdc_ata,
                token_mint, wallet_token_ata, vault_token_ata, delegated_signers, threshold,
                max_amount_per_period, active, first_seen_at, last_seen_at, last_seen_slot, last_seen_signature
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
        .fetch_one(&self.pool)
        .await?;

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
                max_amount_per_period, active, first_seen_at, last_seen_at, last_seen_slot, last_seen_signature
            FROM loyal_yield.balance_sweep_targets
            WHERE active
              AND lifecycle_status = 'active'
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
        let last_event_id = lock_projection_offset(&mut *tx, consumer_name).await?;
        let updates = fetch_after_cursor(last_event_id, batch_limit).await?;
        let mut projected_count = 0_usize;
        let mut next_event_id = last_event_id;

        for projected in updates {
            if projected.event_id <= last_event_id {
                continue;
            }
            record_projected_wallet_ata_balance_update_in_tx(
                &mut *tx,
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
            WHERE vault_id = ANY($1)
              AND mint = $2
            ORDER BY vault_id, mint
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
        .fetch_one(&self.pool)
        .await?;

        current_idle_token_balance_from_row(&row)
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
                let positions = current_positions_for_update(&mut *tx, vault_id).await?;
                if reconciled_positions_equal(&state.positions, &positions)? {
                    tx.commit().await?;
                    return Ok(PositionSnapshot {
                        id: SnapshotId(current.id),
                        vault_id: VaultId(current.vault_id),
                        policy_id: PolicyId(current.policy_id),
                        observed_slot: current.observed_slot,
                        observed_at: current.observed_at,
                        chain_slot: current.chain_slot,
                        lock_attempt_id: current.lock_attempt_id,
                        is_current: current.is_current,
                        context: current.context,
                    });
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

        close_zero_user_yield_positions_for_vault(
            &mut *tx,
            &vault,
            SnapshotId(snapshot_row.id),
            snapshot_row.observed_slot,
            snapshot_row.observed_at,
            &snapshot_row.context,
        )
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
            insert_planned_decision(&mut *tx, vault_id, &planned, input.estimated_cost_lamports)
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
            insert_planned_decision(&mut *tx, vault_id, &planned, input.estimated_cost_lamports)
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

    pub async fn prepare_same_mint_rebalance(
        &self,
        input: SameMintRebalanceInput,
    ) -> Result<SameMintRebalanceResult, OrchestratorError> {
        let mut tx = self.pool.begin().await?;
        let vault = fetch_rebalance_input_vault_for_update(&mut *tx, &input).await?;
        let vault_id = vault.id;

        if active_decision_exists(&mut *tx, vault_id).await? {
            let decision =
                insert_skipped_decision(&mut *tx, vault_id, SkipReason::ActiveDecision).await?;
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

        let positions = current_positions_for_update(&mut *tx, vault_id).await?;
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
            insert_planned_decision(&mut *tx, vault_id, &planned, input.estimated_cost_lamports)
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
        let decision = fetch_decision_for_update(&mut *tx, input.decision_id).await?;
        if decision.status == DecisionStatus::Confirmed {
            ensure_same_mint_confirm_repeat_matches(&decision, &input)?;
            tx.commit().await?;
            return Ok(same_mint_result_from_confirmed_decision(decision));
        }
        ensure_confirmable_same_mint_decision(&decision)?;
        ensure_same_mint_route_amount_semantics(&decision)?;
        let vault = fetch_managed_vault_for_update(&mut *tx, decision.vault_id).await?;
        let current = current_positions_for_update(&mut *tx, decision.vault_id).await?;
        let source_reserve = required_decision_field(&decision.source_reserve, "source_reserve")?;
        let target_reserve = required_decision_field(&decision.target_reserve, "target_reserve")?;
        let liquidity_mint = required_decision_field(&decision.liquidity_mint, "liquidity_mint")?;
        let amount_raw = decision
            .amount_raw
            .ok_or_else(|| OrchestratorError::StoreInvariant("missing amount_raw".to_owned()))?;

        if let Some(post_snapshot_id) = input.post_snapshot_id {
            let decision = update_confirmed_decision(
                &mut *tx,
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
            &mut *tx,
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

async fn upsert_policy(
    conn: &mut PgConnection,
    event: &PolicyMatchInput,
) -> Result<RoutePolicy, OrchestratorError> {
    let slot =
        i64::try_from(event.slot).map_err(|_| OrchestratorError::SlotOutOfRange(event.slot))?;
    let policy_seed = i64::try_from(event.policy_seed)
        .map_err(|_| OrchestratorError::PolicySeedOutOfRange(event.policy_seed))?;
    let row = sqlx::query_as::<_, RoutePolicyRow>(
        r#"
        INSERT INTO loyal_yield.route_policies
            (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
             delegated_signers, threshold, route_modes, stable_mints, kamino_markets, kamino_liquidity_mints,
             universe_preset, risk_profile, swap_lanes, active, last_seen_slot, last_seen_signature)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, TRUE, $16, $17)
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
            active = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN TRUE ELSE loyal_yield.route_policies.active END,
            last_seen_at = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN now() ELSE loyal_yield.route_policies.last_seen_at END,
            last_seen_slot = GREATEST(loyal_yield.route_policies.last_seen_slot, EXCLUDED.last_seen_slot),
            last_seen_signature = CASE WHEN EXCLUDED.last_seen_slot > loyal_yield.route_policies.last_seen_slot THEN EXCLUDED.last_seen_signature ELSE loyal_yield.route_policies.last_seen_signature END
        RETURNING
            id,
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

    // Collateral snapshots are planner input; they are not proof that app-visible
    // Earn principal is gone.
    let amount_semantics = snapshot_context
        .get("amount_semantics")
        .and_then(Value::as_str);
    amount_semantics == Some(AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED)
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
        || observed_slot != input.idle_observed_slot
        || observed_at != input.idle_observed_at
    {
        return Err(OrchestratorError::SameMintRebalanceValidation(
            "idle vault source changed before atomic fleet handoff".to_owned(),
        ));
    }
    Ok(())
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

#[cfg(test)]
mod tests {
    use super::*;

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
        let context = json!({
            "kind": SAME_MINT_CHAIN_RECONCILE_PREVIEW_KIND,
            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
        });

        assert!(should_skip_zero_user_yield_position_close(&context));
    }

    #[test]
    fn zero_app_position_close_skips_collateral_amount_semantics() {
        let context = json!({
            "source": "some_other_reconcile",
            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
        });

        assert!(should_skip_zero_user_yield_position_close(&context));
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

fn to_i64_slot(slot: u64) -> Result<i64, OrchestratorError> {
    i64::try_from(slot).map_err(|_| OrchestratorError::SlotOutOfRange(slot))
}

fn to_i64_policy_seed(policy_seed: u64) -> Result<i64, OrchestratorError> {
    i64::try_from(policy_seed).map_err(|_| OrchestratorError::PolicySeedOutOfRange(policy_seed))
}

fn optional_to_i64_amount(amount: Option<u64>) -> Result<Option<i64>, OrchestratorError> {
    amount.map(to_i64_amount).transpose()
}
