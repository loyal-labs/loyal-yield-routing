use std::{borrow::Cow, collections::BTreeMap, env, error::Error, str::FromStr};

#[cfg(test)]
use loyal_yield_orchestrator::LookupTableAlertCondition;
use sha2::{Digest, Sha256};
use sqlx::{
    postgres::{PgConnectOptions, PgPoolOptions},
    Connection, PgConnection, PgPool,
};

const MIGRATIONS: &[Migration] = &[
    Migration {
        version: 1,
        name: "loyal_yield_orchestration",
        sql: include_str!("../../migrations/0001_loyal_yield_orchestration.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 2,
        name: "balance_sweep_surplus_lots",
        sql: include_str!("../../migrations/0002_balance_sweep_surplus_lots.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 3,
        name: "balance_sweep_initial_surplus",
        sql: include_str!("../../migrations/0003_balance_sweep_initial_surplus.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 4,
        name: "managed_vault_setup_policy",
        sql: include_str!("../../migrations/0004_managed_vault_setup_policy.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 5,
        name: "add_unsupported_amount_semantics",
        sql: include_str!("../../migrations/0005_add_unsupported_amount_semantics.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 6,
        name: "generic_balance_sweep_token_accounts",
        sql: include_str!("../../migrations/0006_generic_balance_sweep_token_accounts.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 7,
        name: "balance_sweep_scheduled_slots",
        sql: include_str!("../../migrations/0007_balance_sweep_scheduled_slots.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 8,
        name: "route_lookup_tables",
        sql: include_str!("../../migrations/0008_route_lookup_tables.sql"),
        expected_checksum: Some("d20151ef6d6076961195da6c6cf3b4e11bb3e2045f729bdf4b118f6c7d3ddc34"),
    },
    Migration {
        version: 9,
        name: "idle_vault_routing",
        sql: include_str!("../../migrations/0009_idle_vault_routing.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 10,
        name: "realtime_events",
        sql: include_str!("../../migrations/0010_realtime_events.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 11,
        name: "autodeposit_realtime_events",
        sql: include_str!("../../migrations/0011_autodeposit_realtime_events.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 12,
        name: "idle_vault_decision_plan_guardrails",
        sql: include_str!("../../migrations/0012_idle_vault_decision_plan_guardrails.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 13,
        name: "earn_realtime_events",
        sql: include_str!("../../migrations/0013_earn_realtime_events.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 14,
        name: "autodeposit_execution_slot_realtime",
        sql: include_str!("../../migrations/0014_autodeposit_execution_slot_realtime.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 15,
        name: "realtime_web_mobile_protocol",
        sql: include_str!("../../migrations/0015_realtime_web_mobile_protocol.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 16,
        name: "autodeposit_requested_slot_wakeup",
        sql: include_str!("../../migrations/0016_autodeposit_requested_slot_wakeup.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 17,
        name: "reusable_route_lookup_tables",
        sql: include_str!("../../migrations/0017_reusable_route_lookup_tables.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 18,
        name: "earn_activity_realtime",
        sql: include_str!("../../migrations/0018_earn_activity_realtime.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 19,
        name: "legacy_lookup_table_imports",
        sql: include_str!("../../migrations/0019_legacy_lookup_table_imports.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 20,
        name: "demand_driven_shared_market_catalog",
        sql: include_str!("../../migrations/0020_demand_driven_shared_market_catalog.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 21,
        name: "reusable_alt_production_controls",
        sql: include_str!("../../migrations/0021_reusable_alt_production_controls.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 22,
        name: "shared_market_alt_bundles",
        sql: include_str!("../../migrations/0022_shared_market_alt_bundles.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 23,
        name: "value_priority_rebalance_queue",
        sql: include_str!("../../migrations/0023_value_priority_rebalance_queue.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 24,
        name: "fleet_route_confirmer",
        sql: include_str!("../../migrations/0024_fleet_route_confirmer.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 25,
        name: "fee_only_route_payer_shards",
        sql: include_str!("../../migrations/0025_fee_only_route_payer_shards.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 26,
        name: "target_capacity_reservations",
        sql: include_str!("../../migrations/0026_target_capacity_reservations.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 27,
        name: "rebalance_opportunity_attempt_generations",
        sql: include_str!("../../migrations/0027_rebalance_opportunity_attempt_generations.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 28,
        name: "reusable_alt_terminal_repair",
        sql: include_str!("../../migrations/0028_reusable_alt_terminal_repair.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 29,
        name: "fleet_commit_lifetime_fences",
        sql: include_str!("../../migrations/0029_fleet_commit_lifetime_fences.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 30,
        name: "fused_queue_accrual_binding",
        sql: include_str!("../../migrations/0030_fused_queue_accrual_binding.sql"),
        expected_checksum: None,
    },
    Migration {
        version: 31,
        name: "fleet_commit_lifetime_fence_errcode",
        sql: include_str!("../../migrations/0031_fleet_commit_lifetime_fence_errcode.sql"),
        expected_checksum: None,
    },
];

const LEDGER_SCHEMA: &str = "loyal_yield";
const LEDGER_TABLE: &str = "schema_migrations";
const MIGRATION_APPLY_ADVISORY_LOCK: i64 = 5_497_570_743_993_490_033;

struct Migration {
    version: i64,
    name: &'static str,
    sql: &'static str,
    expected_checksum: Option<&'static str>,
}

#[derive(Clone, Copy)]
enum Mode {
    Apply,
    Check,
    VerifyReusableAlts,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let mode = parse_mode()?;
    let database_url = env::var("NEON_DATABASE_URL")
        .map_err(|_| "NEON_DATABASE_URL must be set for Yield Neon migrations")?;
    let mut migration_apply_lock_connection = if matches!(mode, Mode::Apply) {
        // Render starts every role with the same preDeploy command. A
        // dedicated session-level lock keeps concurrent direct-cutover
        // preDeploys from racing the ledger/DDL even if the work pool recycles
        // or reconnects its single physical connection.
        let mut connection = connect_session(&database_url).await?;
        sqlx::query("SELECT pg_advisory_lock($1)")
            .bind(MIGRATION_APPLY_ADVISORY_LOCK)
            .execute(&mut connection)
            .await?;
        Some(connection)
    } else {
        None
    };
    let pool = connect(&database_url).await?;

    if matches!(mode, Mode::Apply) {
        ensure_ledger(&pool).await?;
    } else {
        require_ledger(&pool).await?;
    }

    // Read the checksum ledger once. On Neon, one query per migration kept
    // every checksum-current Render predeploy inside the global apply lock for
    // roughly eleven seconds; six worker roles then paid that latency
    // serially despite having no DDL to run.
    let applied_checksums = applied_checksums(&pool).await?;
    let mut pending = Vec::new();
    for migration in MIGRATIONS {
        match applied_checksums.get(&migration.version) {
            Some(applied) if applied == &migration.checksum() => {
                println!(
                    "migration {} {} already applied",
                    migration.version, migration.name
                );
            }
            Some(_) => {
                return Err(format!(
                    "migration {} {} was applied with a different checksum",
                    migration.version, migration.name
                )
                .into());
            }
            None => pending.push(migration),
        }
    }

    if pending.is_empty() {
        // Render runs this pre-deploy command independently for every worker.
        // Once the checksum-bound ledger is current, repeating the exhaustive
        // catalog validation under the global apply lock only serializes
        // otherwise independent deploys. The invocation that actually applies
        // a migration still validates below; explicit --check and
        // --verify-reusable-alts retain the full read-only schema audit.
        if !matches!(mode, Mode::Apply) {
            validate_schema(&pool).await?;
        }
        if matches!(mode, Mode::VerifyReusableAlts) {
            verify_reusable_alts(&pool).await?;
        }
        if matches!(mode, Mode::Apply) {
            release_migration_apply_lock(migration_apply_lock_connection.as_mut()).await?;
        }
        println!("loyal_yield migrations are up to date");
        return Ok(());
    }

    if matches!(mode, Mode::Check | Mode::VerifyReusableAlts) {
        return Err(format!("{} loyal_yield migration(s) pending", pending.len()).into());
    }

    // Keep the final pending ledger row absent until the exhaustive catalog
    // validation succeeds. It is the durable validation fence for later
    // `--apply` fast paths: a failed or interrupted validation must leave at
    // least one migration pending instead of making an unvalidated schema look
    // checksum-current.
    let validation_fence = pending
        .pop()
        .ok_or("pending migration set unexpectedly became empty")?;
    for migration in pending {
        println!(
            "applying migration {} {}",
            migration.version, migration.name
        );
        let execution_sql = migration_execution_sql(migration);
        sqlx::raw_sql(&execution_sql).execute(&pool).await?;
        record_applied(&pool, migration).await?;
    }

    println!(
        "applying migration {} {}",
        validation_fence.version, validation_fence.name
    );
    let execution_sql = migration_execution_sql(validation_fence);
    sqlx::raw_sql(&execution_sql).execute(&pool).await?;

    validate_schema(&pool).await?;
    record_applied(&pool, validation_fence).await?;
    release_migration_apply_lock(migration_apply_lock_connection.as_mut()).await?;
    println!("loyal_yield migrations are up to date");
    Ok(())
}

async fn release_migration_apply_lock(
    connection: Option<&mut PgConnection>,
) -> Result<(), Box<dyn Error>> {
    let connection =
        connection.ok_or("yield migration apply advisory lock connection was not retained")?;
    let released: bool = sqlx::query_scalar("SELECT pg_advisory_unlock($1)")
        .bind(MIGRATION_APPLY_ADVISORY_LOCK)
        .fetch_one(connection)
        .await?;
    if !released {
        return Err("yield migration apply advisory lock was not held by this session".into());
    }
    Ok(())
}

/// Migration 13 predates the standalone Yield migration runner and guards
/// optional app-owned Earn relations with `to_regclass(...)`. PostgreSQL still
/// resolves the sibling literal `::regclass` function arguments while planning
/// the DO block, so a genuinely blank Yield database fails before that guard
/// can run. Keep the immutable/checksummed migration bytes intact for already
/// applied databases, but execute the semantically equivalent nullable
/// `to_regclass(...)` form when applying that migration for the first time.
fn migration_execution_sql(migration: &Migration) -> Cow<'static, str> {
    if migration.version != 13 {
        return Cow::Borrowed(migration.sql);
    }

    let mut sql = migration.sql.to_owned();
    for relation in [
        "loyal_yield.user_yield_positions",
        "loyal_yield.user_yield_position_holding_events",
        "loyal_yield.earn_deposit_onboarding_attempts",
    ] {
        let eager_cast = format!("'{relation}'::regclass");
        let nullable_lookup = format!("to_regclass('{relation}')");
        assert_eq!(
            sql.matches(&eager_cast).count(),
            1,
            "migration 13 optional-relation compatibility target drifted"
        );
        sql = sql.replace(&eager_cast, &nullable_lookup);
    }
    Cow::Owned(sql)
}

fn parse_mode() -> Result<Mode, Box<dyn Error>> {
    let mut mode = Mode::Apply;
    for arg in env::args().skip(1) {
        match arg.as_str() {
            "--apply" => mode = Mode::Apply,
            "--check" => mode = Mode::Check,
            "--verify-reusable-alts" => mode = Mode::VerifyReusableAlts,
            "--help" | "-h" => {
                println!(
                    "Usage: yield-migrations [--apply|--check|--verify-reusable-alts]\n\nReads NEON_DATABASE_URL from the environment. Verification requires every migration to be applied and performs no schema writes."
                );
                std::process::exit(0);
            }
            _ => return Err(format!("unknown argument: {arg}").into()),
        }
    }
    Ok(mode)
}

async fn connect(database_url: &str) -> Result<PgPool, sqlx::Error> {
    let options = PgConnectOptions::from_str(database_url)?.statement_cache_capacity(0);
    PgPoolOptions::new()
        .max_connections(1)
        .connect_with(options)
        .await
}

async fn connect_session(database_url: &str) -> Result<PgConnection, sqlx::Error> {
    let options = PgConnectOptions::from_str(database_url)?.statement_cache_capacity(0);
    PgConnection::connect_with(&options).await
}

async fn ensure_ledger(pool: &PgPool) -> Result<(), sqlx::Error> {
    sqlx::raw_sql(&format!("CREATE SCHEMA IF NOT EXISTS {LEDGER_SCHEMA};"))
        .execute(pool)
        .await?;
    sqlx::raw_sql(&format!(
        "CREATE TABLE IF NOT EXISTS {LEDGER_SCHEMA}.{LEDGER_TABLE} (
            version BIGINT PRIMARY KEY,
            name TEXT NOT NULL,
            checksum TEXT NOT NULL,
            applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
        );"
    ))
    .execute(pool)
    .await?;
    Ok(())
}

async fn require_ledger(pool: &PgPool) -> Result<(), Box<dyn Error>> {
    let exists: bool =
        sqlx::query_scalar("SELECT to_regclass('loyal_yield.schema_migrations') IS NOT NULL")
            .fetch_one(pool)
            .await?;
    if !exists {
        return Err("missing loyal_yield.schema_migrations; run --apply first".into());
    }
    Ok(())
}

async fn applied_checksums(pool: &PgPool) -> Result<BTreeMap<i64, String>, sqlx::Error> {
    let versions = MIGRATIONS
        .iter()
        .map(|migration| migration.version)
        .collect::<Vec<_>>();
    let rows = sqlx::query_as::<_, (i64, String)>(&format!(
        "SELECT version, checksum
         FROM {LEDGER_SCHEMA}.{LEDGER_TABLE}
         WHERE version = ANY($1::BIGINT[])"
    ))
    .bind(versions)
    .fetch_all(pool)
    .await?;
    Ok(rows.into_iter().collect())
}

async fn record_applied(pool: &PgPool, migration: &Migration) -> Result<(), sqlx::Error> {
    sqlx::query(&format!(
        "INSERT INTO {LEDGER_SCHEMA}.{LEDGER_TABLE} (version, name, checksum)
         VALUES ($1, $2, $3)
         ON CONFLICT (version) DO UPDATE
         SET name = EXCLUDED.name,
             checksum = EXCLUDED.checksum"
    ))
    .bind(migration.version)
    .bind(migration.name)
    .bind(migration.checksum())
    .execute(pool)
    .await?;
    Ok(())
}

async fn validate_schema(pool: &PgPool) -> Result<(), Box<dyn Error>> {
    for relation in [
        "schema_migrations",
        "projection_offsets",
        "balance_sweep_targets",
        "balance_sweep_wallet_balances_current",
        "balance_sweep_wallet_balance_events",
        "balance_sweep_surplus_lots",
        "balance_sweep_lot_claims",
        "balance_sweep_lot_claim_items",
        "balance_sweep_execution_lots",
        "balance_sweep_executions",
        "balance_sweep_scheduled_slots",
        "pending_balance_sweep_surplus_lots",
        "route_lookup_tables",
        "lookup_table_legacy_import_runs",
        "lookup_table_legacy_import_evidence",
        "lookup_table_shared_market_catalog_revisions",
        "lookup_table_shared_market_catalog_heads",
        "lookup_table_shared_market_physical_drifts",
        "lookup_table_cluster_budget_reservations",
        "lookup_table_families",
        "lookup_table_manifests",
        "lookup_table_manifest_addresses",
        "lookup_table_vault_desired_heads",
        "lookup_table_vault_bindings",
        "lookup_table_usage_leases",
        "lookup_table_provisioning_requests",
        "lookup_table_provisioning_request_addresses",
        "lookup_table_addresses",
        "lookup_table_operations",
        "lookup_table_operation_addresses",
        "lookup_table_route_readiness_current",
        "lookup_table_rollout_controls",
        "lookup_table_provisioner_controls",
        "lookup_table_provisioner_broadcast_permits",
        "lookup_table_precutover_probe_runs",
        "lookup_table_precutover_probe_shared_tables",
        "lookup_table_alert_rules",
        "lookup_table_alert_incidents",
        "lookup_table_alert_deliveries",
        "lookup_table_legacy_cleanup_attempts",
        "lookup_table_legacy_cleanup_budget_reservations",
        "optimizer_epochs",
        "fleet_planning_state",
        "fleet_planning_clusters",
        "fleet_planning_dirty_vaults",
        "rebalance_opportunities",
        "lookup_table_provisioning_request_consumers",
        "orchestration_outbox",
        "signed_route_submissions",
        "route_account_conflict_leases",
        "fleet_orchestration_status",
        "vault_idle_token_balances_current",
        "realtime_events",
        "realtime_configuration",
    ] {
        let exists: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM pg_class c
                JOIN pg_namespace n ON n.oid = c.relnamespace
                WHERE n.nspname = 'loyal_yield'
                  AND c.relname = $1
            )
            "#,
        )
        .bind(relation)
        .fetch_one(pool)
        .await?;
        if !exists {
            return Err(format!("missing loyal_yield.{relation}").into());
        }
    }
    let has_initial_surplus: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM pg_enum e
            JOIN pg_type t ON t.oid = e.enumtypid
            JOIN pg_namespace n ON n.oid = t.typnamespace
            WHERE n.nspname = 'loyal_yield'
              AND t.typname = 'balance_sweep_surplus_classification'
              AND e.enumlabel = 'initial_surplus'
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !has_initial_surplus {
        return Err(
            "missing loyal_yield.balance_sweep_surplus_classification initial_surplus value".into(),
        );
    }
    for (relation, column) in [
        ("balance_sweep_targets", "wallet_token_ata"),
        ("balance_sweep_targets", "vault_token_ata"),
        ("balance_sweep_targets", "token_mint"),
        ("balance_sweep_targets", "cluster"),
        ("balance_sweep_wallet_balances_current", "wallet_token_ata"),
        ("balance_sweep_wallet_balance_events", "wallet_token_ata"),
        ("balance_sweep_wallet_balance_events", "mint"),
        ("balance_sweep_executions", "source_token_ata"),
        ("balance_sweep_executions", "destination_token_ata"),
        ("balance_sweep_executions", "token_mint"),
        ("balance_sweep_surplus_lots", "scheduled_slot_id"),
        ("balance_sweep_scheduled_slots", "target_id"),
        ("balance_sweep_scheduled_slots", "token_mint"),
        ("balance_sweep_scheduled_slots", "eligible_after"),
        ("balance_sweep_scheduled_slots", "status"),
        ("balance_sweep_scheduled_slots", "request_source"),
        ("balance_sweep_scheduled_slots", "requested_at"),
        ("balance_sweep_scheduled_slots", "claim_token"),
        ("balance_sweep_scheduled_slots", "execution_id"),
        ("balance_sweep_scheduled_slots", "last_error"),
        ("balance_sweep_scheduled_slots", "created_at"),
        ("balance_sweep_scheduled_slots", "updated_at"),
        ("pending_balance_sweep_surplus_lots", "scheduled_slot_id"),
        ("pending_balance_sweep_surplus_lots", "source_mint"),
        (
            "pending_balance_sweep_surplus_lots",
            "source_wallet_token_ata",
        ),
        ("route_lookup_tables", "cluster"),
        ("route_lookup_tables", "scope"),
        ("route_lookup_tables", "table_address"),
        ("route_lookup_tables", "authority"),
        ("route_lookup_tables", "payer"),
        ("route_lookup_tables", "status"),
        ("route_lookup_tables", "durable"),
        ("route_lookup_tables", "address_count"),
        ("route_lookup_tables", "address_hash"),
        ("route_lookup_tables", "addresses"),
        ("route_lookup_tables", "family_id"),
        ("route_lookup_tables", "legacy_import_run_id"),
        ("lookup_table_legacy_import_evidence", "import_run_id"),
        (
            "lookup_table_legacy_import_evidence",
            "route_lookup_table_id",
        ),
        ("route_lookup_tables", "allocation_kind"),
        ("route_lookup_tables", "generation"),
        ("route_lookup_tables", "shard_ordinal"),
        ("route_lookup_tables", "desired_state"),
        ("route_lookup_tables", "accepting_allocations"),
        ("route_lookup_tables", "allocation_high_water"),
        ("route_lookup_tables", "reserved_address_count"),
        ("route_lookup_tables", "usable_address_count"),
        ("route_lookup_tables", "last_extended_start_index"),
        ("route_lookup_tables", "last_verified_slot"),
        ("route_lookup_tables", "last_verified_at"),
        ("route_lookup_tables", "mutation_epoch"),
        ("route_lookup_tables", "rollback_until"),
        ("route_lookup_tables", "legacy_kind"),
        ("route_lookup_tables", "legacy_import_run_id"),
        ("lookup_table_legacy_import_runs", "id"),
        ("lookup_table_legacy_import_runs", "cluster"),
        ("lookup_table_legacy_import_runs", "rpc_genesis_hash"),
        ("lookup_table_legacy_import_runs", "verified_slot"),
        ("lookup_table_legacy_import_runs", "verified_at"),
        ("lookup_table_legacy_import_runs", "legacy_kind"),
        ("lookup_table_legacy_import_runs", "expected_table_count"),
        ("lookup_table_legacy_import_runs", "verified_table_count"),
        ("lookup_table_legacy_import_runs", "import_fingerprint"),
        ("lookup_table_legacy_import_runs", "reason"),
        ("lookup_table_legacy_import_runs", "updated_by"),
        ("lookup_table_legacy_import_runs", "created_at"),
        ("lookup_table_legacy_import_evidence", "id"),
        ("lookup_table_legacy_import_evidence", "import_run_id"),
        (
            "lookup_table_legacy_import_evidence",
            "route_lookup_table_id",
        ),
        ("lookup_table_legacy_import_evidence", "table_address"),
        ("lookup_table_legacy_import_evidence", "scope"),
        ("lookup_table_legacy_import_evidence", "legacy_kind"),
        ("lookup_table_legacy_import_evidence", "expected_authority"),
        ("lookup_table_legacy_import_evidence", "observed_authority"),
        ("lookup_table_legacy_import_evidence", "observed_owner"),
        (
            "lookup_table_legacy_import_evidence",
            "observed_deactivation_slot",
        ),
        (
            "lookup_table_legacy_import_evidence",
            "observed_last_extended_slot",
        ),
        (
            "lookup_table_legacy_import_evidence",
            "observed_last_extended_start_index",
        ),
        ("lookup_table_legacy_import_evidence", "address_count"),
        ("lookup_table_legacy_import_evidence", "address_hash"),
        ("lookup_table_legacy_import_evidence", "addresses"),
        ("lookup_table_legacy_import_evidence", "verified_slot"),
        ("lookup_table_legacy_import_evidence", "verified_at"),
        ("lookup_table_legacy_import_evidence", "created_at"),
        ("lookup_table_shared_market_catalog_revisions", "id"),
        ("lookup_table_shared_market_catalog_revisions", "family_id"),
        (
            "lookup_table_shared_market_catalog_revisions",
            "manifest_id",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "catalog_revision",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "catalog_version",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "desired_set_hash",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "enabled_mints_hash",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "reserve_set_hash",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "address_count",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "source_slot",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "source_observed_at",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "source_metadata",
        ),
        ("lookup_table_shared_market_catalog_revisions", "reason"),
        ("lookup_table_shared_market_catalog_revisions", "updated_by"),
        ("lookup_table_shared_market_catalog_revisions", "created_at"),
        ("lookup_table_shared_market_catalog_heads", "family_id"),
        (
            "lookup_table_shared_market_catalog_heads",
            "catalog_revision_id",
        ),
        (
            "lookup_table_shared_market_catalog_heads",
            "target_generation",
        ),
        (
            "lookup_table_shared_market_catalog_heads",
            "readiness_state",
        ),
        ("lookup_table_shared_market_catalog_heads", "activated_at"),
        ("lookup_table_shared_market_catalog_heads", "created_at"),
        ("lookup_table_shared_market_catalog_heads", "updated_at"),
        ("lookup_table_shared_market_physical_drifts", "id"),
        (
            "lookup_table_shared_market_physical_drifts",
            "evidence_hash",
        ),
        ("lookup_table_shared_market_physical_drifts", "cluster"),
        ("lookup_table_shared_market_physical_drifts", "family_id"),
        (
            "lookup_table_shared_market_physical_drifts",
            "catalog_revision_id",
        ),
        (
            "lookup_table_shared_market_physical_drifts",
            "route_lookup_table_id",
        ),
        (
            "lookup_table_shared_market_physical_drifts",
            "expected_mutation_epoch",
        ),
        (
            "lookup_table_shared_market_physical_drifts",
            "observed_slot",
        ),
        (
            "lookup_table_shared_market_physical_drifts",
            "observed_address_hash",
        ),
        (
            "lookup_table_shared_market_physical_drifts",
            "resolution_state",
        ),
        ("lookup_table_provisioner_controls", "cluster"),
        ("lookup_table_provisioner_controls", "paused"),
        ("lookup_table_provisioner_controls", "reason"),
        ("lookup_table_provisioner_controls", "updated_by"),
        ("lookup_table_provisioner_controls", "control_epoch"),
        ("lookup_table_provisioner_controls", "created_at"),
        ("lookup_table_provisioner_controls", "updated_at"),
        ("lookup_table_provisioner_broadcast_permits", "id"),
        ("lookup_table_provisioner_broadcast_permits", "cluster"),
        ("lookup_table_provisioner_broadcast_permits", "operation_id"),
        (
            "lookup_table_provisioner_broadcast_permits",
            "fencing_token",
        ),
        (
            "lookup_table_provisioner_broadcast_permits",
            "control_epoch",
        ),
        (
            "lookup_table_provisioner_broadcast_permits",
            "transaction_signature",
        ),
        ("lookup_table_provisioner_broadcast_permits", "message_hash"),
        ("lookup_table_provisioner_broadcast_permits", "permit_state"),
        (
            "lookup_table_provisioner_broadcast_permits",
            "resolution_detail",
        ),
        ("lookup_table_provisioner_broadcast_permits", "granted_at"),
        ("lookup_table_provisioner_broadcast_permits", "resolved_at"),
        ("lookup_table_provisioner_broadcast_permits", "created_at"),
        ("lookup_table_provisioner_broadcast_permits", "updated_at"),
        ("lookup_table_precutover_probe_runs", "id"),
        ("lookup_table_precutover_probe_runs", "probe_token"),
        ("lookup_table_precutover_probe_runs", "cluster"),
        ("lookup_table_precutover_probe_runs", "vault_id"),
        ("lookup_table_precutover_probe_runs", "catalog_revision_id"),
        ("lookup_table_precutover_probe_runs", "shared_manifest_id"),
        (
            "lookup_table_precutover_probe_runs",
            "route_lookup_table_id",
        ),
        ("lookup_table_precutover_probe_runs", "shared_table_address"),
        ("lookup_table_precutover_probe_runs", "shared_authority"),
        (
            "lookup_table_precutover_probe_runs",
            "shared_mutation_epoch",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "provisioner_control_epoch",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "requirements_fingerprint",
        ),
        ("lookup_table_precutover_probe_runs", "finalized_slot"),
        (
            "lookup_table_precutover_probe_runs",
            "finalized_last_extended_slot",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "finalized_address_hash",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "finalized_address_count",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "shared_table_bundle_hash",
        ),
        ("lookup_table_precutover_probe_runs", "shared_table_count"),
        (
            "lookup_table_precutover_probe_runs",
            "finalized_bundle_address_count",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "finalized_shared_exact",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "synthetic_drift_evidence_hash",
        ),
        ("lookup_table_precutover_probe_runs", "drift_signal_count"),
        (
            "lookup_table_precutover_probe_runs",
            "drift_provisioning_request_count",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "duplicate_request_attempt_count",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "distinct_request_count",
        ),
        ("lookup_table_precutover_probe_runs", "decision_count"),
        ("lookup_table_precutover_probe_runs", "binding_count"),
        ("lookup_table_precutover_probe_runs", "operation_count"),
        (
            "lookup_table_precutover_probe_runs",
            "rollback_residue_count",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "catalog_head_restored",
        ),
        ("lookup_table_precutover_probe_runs", "signer_loaded"),
        ("lookup_table_precutover_probe_runs", "transactions_sent"),
        ("lookup_table_precutover_probe_runs", "result"),
        ("lookup_table_precutover_probe_runs", "created_at"),
        (
            "lookup_table_precutover_probe_shared_tables",
            "probe_run_id",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "shard_ordinal",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "route_lookup_table_id",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "shared_table_address",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "shared_authority",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "shared_mutation_epoch",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "finalized_slot",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "finalized_last_extended_slot",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "finalized_address_hash",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "finalized_address_count",
        ),
        ("lookup_table_precutover_probe_shared_tables", "created_at"),
        ("lookup_table_alert_rules", "rule_key"),
        ("lookup_table_alert_rules", "rule_version"),
        ("lookup_table_alert_rules", "enabled"),
        ("lookup_table_alert_rules", "severity"),
        ("lookup_table_alert_rules", "description"),
        ("lookup_table_alert_rules", "configuration"),
        ("lookup_table_alert_rules", "created_at"),
        ("lookup_table_alert_rules", "updated_at"),
        ("lookup_table_alert_incidents", "id"),
        ("lookup_table_alert_incidents", "cluster"),
        ("lookup_table_alert_incidents", "policy_pubkey"),
        ("lookup_table_alert_incidents", "alert_condition"),
        ("lookup_table_alert_incidents", "scope_key"),
        ("lookup_table_alert_incidents", "incident_status"),
        ("lookup_table_alert_incidents", "severity"),
        ("lookup_table_alert_incidents", "fingerprint"),
        ("lookup_table_alert_incidents", "summary"),
        ("lookup_table_alert_incidents", "details"),
        ("lookup_table_alert_incidents", "first_observed_at"),
        ("lookup_table_alert_incidents", "opened_at"),
        ("lookup_table_alert_incidents", "last_observed_at"),
        ("lookup_table_alert_incidents", "last_notified_at"),
        ("lookup_table_alert_incidents", "occurrence_count"),
        ("lookup_table_alert_incidents", "revision"),
        ("lookup_table_alert_incidents", "resolved_at"),
        ("lookup_table_alert_incidents", "created_at"),
        ("lookup_table_alert_incidents", "updated_at"),
        ("lookup_table_alert_deliveries", "id"),
        ("lookup_table_alert_deliveries", "incident_id"),
        ("lookup_table_alert_deliveries", "incident_revision"),
        ("lookup_table_alert_deliveries", "alert_condition"),
        ("lookup_table_alert_deliveries", "event_kind"),
        ("lookup_table_alert_deliveries", "idempotency_key"),
        ("lookup_table_alert_deliveries", "cluster"),
        ("lookup_table_alert_deliveries", "policy_pubkey"),
        ("lookup_table_alert_deliveries", "payload"),
        ("lookup_table_alert_deliveries", "delivery_state"),
        ("lookup_table_alert_deliveries", "delivered_via"),
        ("lookup_table_alert_deliveries", "attempt_count"),
        ("lookup_table_alert_deliveries", "max_attempts"),
        ("lookup_table_alert_deliveries", "next_attempt_at"),
        ("lookup_table_alert_deliveries", "lease_owner"),
        ("lookup_table_alert_deliveries", "lease_expires_at"),
        ("lookup_table_alert_deliveries", "fencing_token"),
        ("lookup_table_alert_deliveries", "http_status"),
        ("lookup_table_alert_deliveries", "last_error"),
        ("lookup_table_alert_deliveries", "delivered_at"),
        ("lookup_table_alert_deliveries", "created_at"),
        ("lookup_table_alert_deliveries", "updated_at"),
        ("lookup_table_legacy_cleanup_attempts", "id"),
        (
            "lookup_table_legacy_cleanup_attempts",
            "route_lookup_table_id",
        ),
        ("lookup_table_legacy_cleanup_attempts", "cluster"),
        ("lookup_table_legacy_cleanup_attempts", "table_address"),
        ("lookup_table_legacy_cleanup_attempts", "operation_kind"),
        ("lookup_table_legacy_cleanup_attempts", "attempt_number"),
        (
            "lookup_table_legacy_cleanup_attempts",
            "authorization_token",
        ),
        ("lookup_table_legacy_cleanup_attempts", "expected_authority"),
        (
            "lookup_table_legacy_cleanup_attempts",
            "expected_address_count",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "expected_address_hash",
        ),
        ("lookup_table_legacy_cleanup_attempts", "close_recipient"),
        (
            "lookup_table_legacy_cleanup_attempts",
            "expected_reclaimed_lamports",
        ),
        ("lookup_table_legacy_cleanup_attempts", "attempt_state"),
        (
            "lookup_table_legacy_cleanup_attempts",
            "transaction_signature",
        ),
        ("lookup_table_legacy_cleanup_attempts", "message_hash"),
        ("lookup_table_legacy_cleanup_attempts", "recent_blockhash"),
        (
            "lookup_table_legacy_cleanup_attempts",
            "last_valid_block_height",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "estimated_fee_lamports",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "recipient_balance_before",
        ),
        ("lookup_table_legacy_cleanup_attempts", "submitted_at"),
        ("lookup_table_legacy_cleanup_attempts", "finalized_slot"),
        (
            "lookup_table_legacy_cleanup_attempts",
            "recipient_balance_after",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "actual_reclaimed_lamports",
        ),
        ("lookup_table_legacy_cleanup_attempts", "error_code"),
        ("lookup_table_legacy_cleanup_attempts", "error_detail"),
        ("lookup_table_legacy_cleanup_attempts", "created_at"),
        ("lookup_table_legacy_cleanup_attempts", "updated_at"),
        ("lookup_table_legacy_cleanup_budget_reservations", "id"),
        (
            "lookup_table_legacy_cleanup_budget_reservations",
            "legacy_cleanup_attempt_id",
        ),
        ("lookup_table_legacy_cleanup_budget_reservations", "cluster"),
        (
            "lookup_table_legacy_cleanup_budget_reservations",
            "estimated_fee_lamports",
        ),
        (
            "lookup_table_legacy_cleanup_budget_reservations",
            "estimated_rent_lamports",
        ),
        (
            "lookup_table_legacy_cleanup_budget_reservations",
            "reserved_lamports",
        ),
        (
            "lookup_table_legacy_cleanup_budget_reservations",
            "reserved_at",
        ),
        (
            "lookup_table_legacy_cleanup_budget_reservations",
            "reserved_until",
        ),
        (
            "lookup_table_legacy_cleanup_budget_reservations",
            "created_at",
        ),
        ("lookup_table_cluster_budget_reservations", "id"),
        ("lookup_table_cluster_budget_reservations", "cluster"),
        ("lookup_table_cluster_budget_reservations", "operation_id"),
        ("lookup_table_cluster_budget_reservations", "fencing_token"),
        (
            "lookup_table_cluster_budget_reservations",
            "reserved_lamports",
        ),
        ("lookup_table_cluster_budget_reservations", "reserved_until"),
        ("lookup_table_families", "id"),
        ("lookup_table_families", "cluster"),
        ("lookup_table_families", "logical_name"),
        ("lookup_table_families", "kind"),
        ("lookup_table_families", "desired_state"),
        ("lookup_table_families", "planner_version"),
        ("lookup_table_families", "catalog_version"),
        ("lookup_table_families", "active_generation"),
        ("lookup_table_families", "previous_generation"),
        ("lookup_table_families", "provisioning_authority"),
        ("lookup_table_families", "payer"),
        ("lookup_table_families", "hard_capacity"),
        ("lookup_table_families", "largest_atomic_expansion"),
        ("lookup_table_families", "safety_margin"),
        ("lookup_table_families", "allocation_high_water"),
        ("lookup_table_families", "created_at"),
        ("lookup_table_families", "updated_at"),
        ("lookup_table_families", "rollback_until"),
        ("lookup_table_manifests", "id"),
        ("lookup_table_manifests", "family_id"),
        ("lookup_table_manifests", "subject_kind"),
        ("lookup_table_manifests", "subject_key"),
        ("lookup_table_manifests", "vault_id"),
        ("lookup_table_manifests", "desired_set_hash"),
        ("lookup_table_manifests", "address_count"),
        ("lookup_table_manifests", "source_slot"),
        ("lookup_table_manifests", "planner_version"),
        ("lookup_table_manifests", "catalog_version"),
        ("lookup_table_manifests", "sealed_at"),
        ("lookup_table_manifests", "created_at"),
        ("lookup_table_manifest_addresses", "manifest_id"),
        ("lookup_table_manifest_addresses", "address"),
        ("lookup_table_manifest_addresses", "ordinal"),
        ("lookup_table_manifest_addresses", "semantic_class"),
        ("lookup_table_manifest_addresses", "account_role"),
        ("lookup_table_manifest_addresses", "is_writable"),
        ("lookup_table_manifest_addresses", "created_at"),
        ("lookup_table_vault_desired_heads", "family_id"),
        ("lookup_table_vault_desired_heads", "vault_id"),
        ("lookup_table_vault_desired_heads", "binding_ordinal"),
        ("lookup_table_vault_desired_heads", "manifest_id"),
        ("lookup_table_vault_desired_heads", "desired_revision"),
        ("lookup_table_vault_desired_heads", "created_at"),
        ("lookup_table_vault_desired_heads", "updated_at"),
        ("lookup_table_vault_bindings", "id"),
        ("lookup_table_vault_bindings", "vault_id"),
        ("lookup_table_vault_bindings", "family_id"),
        ("lookup_table_vault_bindings", "route_lookup_table_id"),
        ("lookup_table_vault_bindings", "manifest_id"),
        ("lookup_table_vault_bindings", "binding_ordinal"),
        ("lookup_table_vault_bindings", "desired_head_revision"),
        ("lookup_table_vault_bindings", "allocation_mode"),
        ("lookup_table_vault_bindings", "reserved_capacity"),
        ("lookup_table_vault_bindings", "predecessor_binding_id"),
        ("lookup_table_vault_bindings", "lifecycle_state"),
        ("lookup_table_vault_bindings", "active_from_slot"),
        ("lookup_table_vault_bindings", "active_until_slot"),
        ("lookup_table_vault_bindings", "activated_at"),
        ("lookup_table_vault_bindings", "deactivated_at"),
        ("lookup_table_vault_bindings", "created_at"),
        ("lookup_table_vault_bindings", "updated_at"),
        ("lookup_table_vault_bindings", "rollback_until"),
        ("lookup_table_usage_leases", "id"),
        ("lookup_table_usage_leases", "cluster"),
        ("lookup_table_usage_leases", "lease_kind"),
        ("lookup_table_usage_leases", "reference_key"),
        ("lookup_table_usage_leases", "route_lookup_table_id"),
        ("lookup_table_usage_leases", "vault_id"),
        ("lookup_table_usage_leases", "binding_id"),
        ("lookup_table_usage_leases", "route_fingerprint"),
        ("lookup_table_usage_leases", "requirements_fingerprint"),
        ("lookup_table_usage_leases", "expires_at"),
        ("lookup_table_usage_leases", "released_at"),
        ("lookup_table_usage_leases", "created_at"),
        ("lookup_table_usage_leases", "updated_at"),
        ("lookup_table_provisioning_requests", "id"),
        ("lookup_table_provisioning_requests", "cluster"),
        ("lookup_table_provisioning_requests", "vault_id"),
        ("lookup_table_provisioning_requests", "route_fingerprint"),
        (
            "lookup_table_provisioning_requests",
            "requirements_fingerprint",
        ),
        ("lookup_table_provisioning_requests", "shared_manifest_id"),
        ("lookup_table_provisioning_requests", "vault_manifest_id"),
        ("lookup_table_provisioning_requests", "desired_shared_hash"),
        ("lookup_table_provisioning_requests", "desired_vault_hash"),
        (
            "lookup_table_provisioning_requests",
            "desired_shared_address_count",
        ),
        (
            "lookup_table_provisioning_requests",
            "desired_vault_address_count",
        ),
        ("lookup_table_provisioning_requests", "sealed_at"),
        ("lookup_table_provisioning_requests", "request_status"),
        ("lookup_table_provisioning_requests", "lease_owner"),
        ("lookup_table_provisioning_requests", "lease_expires_at"),
        ("lookup_table_provisioning_requests", "fencing_token"),
        ("lookup_table_provisioning_requests", "attempt_count"),
        ("lookup_table_provisioning_requests", "next_attempt_at"),
        ("lookup_table_provisioning_requests", "error_code"),
        ("lookup_table_provisioning_requests", "error_detail"),
        ("lookup_table_provisioning_requests", "requested_at"),
        ("lookup_table_provisioning_requests", "satisfied_at"),
        ("lookup_table_provisioning_requests", "created_at"),
        ("lookup_table_provisioning_requests", "updated_at"),
        ("lookup_table_provisioning_request_addresses", "request_id"),
        ("lookup_table_provisioning_request_addresses", "address"),
        (
            "lookup_table_provisioning_request_addresses",
            "semantic_class",
        ),
        ("lookup_table_provisioning_request_addresses", "ordinal"),
        (
            "lookup_table_provisioning_request_addresses",
            "account_role",
        ),
        ("lookup_table_provisioning_request_addresses", "is_writable"),
        ("lookup_table_provisioning_request_addresses", "created_at"),
        ("lookup_table_addresses", "route_lookup_table_id"),
        ("lookup_table_addresses", "address"),
        ("lookup_table_addresses", "ordinal"),
        ("lookup_table_addresses", "added_operation_id"),
        ("lookup_table_addresses", "added_slot"),
        ("lookup_table_addresses", "usable_after_slot"),
        ("lookup_table_addresses", "last_verified_slot"),
        ("lookup_table_addresses", "last_verified_at"),
        ("lookup_table_addresses", "created_at"),
        ("lookup_table_operations", "id"),
        ("lookup_table_operations", "idempotency_key"),
        ("lookup_table_operations", "family_id"),
        ("lookup_table_operations", "route_lookup_table_id"),
        ("lookup_table_operations", "manifest_id"),
        ("lookup_table_operations", "binding_id"),
        ("lookup_table_operations", "operation_kind"),
        ("lookup_table_operations", "operation_state"),
        ("lookup_table_operations", "target_generation"),
        ("lookup_table_operations", "target_shard_ordinal"),
        ("lookup_table_operations", "operation_context"),
        ("lookup_table_operations", "mutation_epoch"),
        ("lookup_table_operations", "lease_owner"),
        ("lookup_table_operations", "lease_expires_at"),
        ("lookup_table_operations", "fencing_token"),
        ("lookup_table_operations", "transaction_signature"),
        ("lookup_table_operations", "message_hash"),
        ("lookup_table_operations", "recent_blockhash"),
        ("lookup_table_operations", "last_valid_block_height"),
        ("lookup_table_operations", "attempt_count"),
        ("lookup_table_operations", "next_attempt_at"),
        ("lookup_table_operations", "error_code"),
        ("lookup_table_operations", "error_detail"),
        ("lookup_table_operations", "submitted_slot"),
        ("lookup_table_operations", "submitted_at"),
        ("lookup_table_operations", "confirmed_slot"),
        ("lookup_table_operations", "confirmed_at"),
        ("lookup_table_operations", "finalized_slot"),
        ("lookup_table_operations", "finalized_at"),
        ("lookup_table_operations", "reconciled_slot"),
        ("lookup_table_operations", "reconciled_at"),
        ("lookup_table_operations", "completed_at"),
        ("lookup_table_operations", "created_at"),
        ("lookup_table_operations", "updated_at"),
        ("lookup_table_operations", "estimated_fee_lamports"),
        ("lookup_table_operations", "estimated_rent_lamports"),
        ("lookup_table_operations", "actual_fee_lamports"),
        ("lookup_table_operations", "actual_rent_lamports"),
        ("lookup_table_operations", "reclaimed_rent_lamports"),
        ("lookup_table_operation_addresses", "operation_id"),
        ("lookup_table_operation_addresses", "address"),
        ("lookup_table_operation_addresses", "ordinal"),
        ("lookup_table_operation_addresses", "created_at"),
        ("lookup_table_route_readiness_current", "cluster"),
        ("lookup_table_route_readiness_current", "vault_id"),
        ("lookup_table_route_readiness_current", "route_fingerprint"),
        (
            "lookup_table_route_readiness_current",
            "requirements_fingerprint",
        ),
        ("lookup_table_route_readiness_current", "route_kind"),
        ("lookup_table_route_readiness_current", "source_reserve"),
        ("lookup_table_route_readiness_current", "target_reserve"),
        ("lookup_table_route_readiness_current", "manifest_id"),
        ("lookup_table_route_readiness_current", "shared_family_id"),
        ("lookup_table_route_readiness_current", "vault_binding_id"),
        ("lookup_table_route_readiness_current", "readiness_state"),
        (
            "lookup_table_route_readiness_current",
            "required_address_count",
        ),
        (
            "lookup_table_route_readiness_current",
            "covered_address_count",
        ),
        ("lookup_table_route_readiness_current", "missing_addresses"),
        ("lookup_table_route_readiness_current", "legacy_table_ids"),
        ("lookup_table_route_readiness_current", "reusable_table_ids"),
        (
            "lookup_table_route_readiness_current",
            "compiled_message_size",
        ),
        ("lookup_table_route_readiness_current", "packet_limit"),
        ("lookup_table_route_readiness_current", "observed_slot"),
        ("lookup_table_route_readiness_current", "observed_at"),
        ("lookup_table_route_readiness_current", "updated_at"),
        ("lookup_table_route_readiness_current", "selection_kind"),
        ("lookup_table_route_readiness_current", "fallback_reason"),
        ("lookup_table_route_readiness_current", "rollout_mode"),
        ("lookup_table_route_readiness_current", "selected_table_ids"),
        (
            "lookup_table_route_readiness_current",
            "selected_table_count",
        ),
        ("lookup_table_route_readiness_current", "packet_fits"),
        ("lookup_table_route_readiness_current", "simulation_state"),
        (
            "lookup_table_route_readiness_current",
            "simulation_units_consumed",
        ),
        ("lookup_table_route_readiness_current", "simulation_error"),
        ("lookup_table_rollout_controls", "id"),
        ("lookup_table_rollout_controls", "cluster"),
        ("lookup_table_rollout_controls", "vault_id"),
        ("lookup_table_rollout_controls", "rollout_mode"),
        ("lookup_table_rollout_controls", "force_legacy"),
        ("lookup_table_rollout_controls", "reason"),
        ("lookup_table_rollout_controls", "updated_by"),
        ("lookup_table_rollout_controls", "created_at"),
        ("lookup_table_rollout_controls", "updated_at"),
        ("vault_idle_token_balances_current", "vault_id"),
        ("vault_idle_token_balances_current", "mint"),
        ("vault_idle_token_balances_current", "amount_raw"),
        ("vault_idle_token_balances_current", "owner"),
        ("vault_idle_token_balances_current", "token_account"),
        ("vault_idle_token_balances_current", "observed_slot"),
        ("vault_idle_token_balances_current", "observed_at"),
        ("vault_idle_token_balances_current", "source_commitment"),
        ("vault_idle_token_balances_current", "updated_at"),
        ("realtime_events", "id"),
        ("realtime_events", "created_at"),
        ("realtime_events", "event_type"),
        ("realtime_events", "scope"),
        ("realtime_events", "reason"),
        ("realtime_events", "solana_env"),
        ("realtime_events", "wallet_address"),
        ("realtime_events", "settings_pda"),
        ("realtime_events", "smart_account_address"),
        ("realtime_events", "vault_pubkey"),
        ("realtime_events", "target_id"),
        ("realtime_events", "scheduled_slot_id"),
        ("realtime_events", "execution_id"),
        ("realtime_events", "source_table"),
        ("realtime_events", "source_id"),
        ("realtime_events", "payload"),
        ("realtime_events", "schema_version"),
        ("realtime_events", "earn_vault_address"),
        ("realtime_events", "failure_code"),
        ("realtime_events", "deliverable"),
        ("realtime_configuration", "solana_env"),
        ("balance_sweep_executions", "scheduled_slot_id"),
        ("balance_sweep_executions", "yield_deposit_id"),
        ("balance_sweep_executions", "yield_position_id"),
        ("balance_sweep_executions", "kamino_deposit_signature"),
        ("balance_sweep_executions", "completed_at"),
        ("balance_sweep_executions", "completion_failure_code"),
        ("lookup_table_provisioning_requests", "economic_priority"),
        (
            "lookup_table_provisioning_requests",
            "active_consumer_count",
        ),
        ("rebalance_opportunities", "id"),
        ("rebalance_opportunities", "cluster"),
        ("rebalance_opportunities", "idempotency_key"),
        ("rebalance_opportunities", "vault_id"),
        ("rebalance_opportunities", "source_snapshot_id"),
        ("rebalance_opportunities", "optimizer_epoch_id"),
        ("rebalance_opportunities", "route_fingerprint"),
        ("rebalance_opportunities", "requirements_fingerprint"),
        ("rebalance_opportunities", "economic_priority"),
        ("rebalance_opportunities", "scheduler_priority_anchor"),
        ("rebalance_opportunities", "opportunity_state"),
        ("rebalance_opportunities", "state_entered_at"),
        ("rebalance_opportunities", "ready_at"),
        ("rebalance_opportunities", "waiting_alt_at"),
        ("rebalance_opportunities", "available_at"),
        ("rebalance_opportunities", "expires_at"),
        ("rebalance_opportunities", "lease_kind"),
        ("rebalance_opportunities", "lease_owner"),
        ("rebalance_opportunities", "lease_expires_at"),
        ("rebalance_opportunities", "fencing_token"),
        ("rebalance_opportunities", "decision_id"),
        (
            "lookup_table_provisioning_request_consumers",
            "opportunity_id",
        ),
        (
            "lookup_table_provisioning_request_consumers",
            "provisioning_request_id",
        ),
        ("optimizer_epochs", "epoch_key"),
        ("optimizer_epochs", "market_slot"),
        ("optimizer_epochs", "market_state"),
        ("fleet_planning_state", "optimizer_epoch_key"),
        ("fleet_planning_state", "complete_frontier"),
        ("fleet_planning_state", "full_sweep_completed_at"),
        ("fleet_planning_clusters", "cluster"),
        ("fleet_planning_clusters", "last_seen_at"),
        ("fleet_planning_dirty_vaults", "vault_id"),
        ("fleet_planning_dirty_vaults", "reasons"),
        ("fleet_planning_dirty_vaults", "available_at"),
        ("fleet_planning_dirty_vaults", "fencing_token"),
        ("fleet_planning_dirty_vaults", "generation"),
        ("orchestration_outbox", "dedupe_key"),
        ("orchestration_outbox", "processed_at"),
        ("signed_route_submissions", "semantic_key"),
        ("signed_route_submissions", "signed_transaction"),
        ("signed_route_submissions", "signed_transaction_hash"),
        ("signed_route_submissions", "transaction_signature"),
        ("signed_route_submissions", "optimizer_epoch_id"),
        ("signed_route_submissions", "alt_mutation_epochs"),
        ("signed_route_submissions", "writable_account_keys"),
        ("signed_route_submissions", "conflict_account_keys"),
        ("signed_route_submissions", "compiled_fee_lamports"),
        ("signed_route_submissions", "executor_fencing_token"),
        ("signed_route_submissions", "submission_state"),
        ("signed_route_submissions", "submission_state_entered_at"),
        ("signed_route_submissions", "confirmation_available_at"),
        ("signed_route_submissions", "confirmation_lease_owner"),
        ("signed_route_submissions", "confirmation_lease_expires_at"),
        ("signed_route_submissions", "confirmation_fencing_token"),
        ("signed_route_submissions", "confirmation_attempt_count"),
        ("signed_route_submissions", "broadcast_count"),
        ("signed_route_submissions", "last_broadcast_at"),
        ("signed_route_submissions", "last_status_checked_at"),
        ("signed_route_submissions", "expiry_observed_block_height"),
        ("signed_route_submissions", "effect_check_slot"),
        ("route_account_conflict_leases", "writable_account_key"),
        ("route_account_conflict_leases", "opportunity_id"),
        ("route_account_conflict_leases", "fencing_token"),
        ("route_account_conflict_leases", "expires_at"),
        ("route_account_conflict_leases", "submission_id"),
    ] {
        let exists: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM information_schema.columns
                WHERE table_schema = 'loyal_yield'
                  AND table_name = $1
                  AND column_name = $2
            )
            "#,
        )
        .bind(relation)
        .bind(column)
        .fetch_one(pool)
        .await?;
        if !exists {
            return Err(format!("missing loyal_yield.{relation}.{column}").into());
        }
    }
    for (relation, constraint) in [
        (
            "route_lookup_tables",
            "route_lookup_tables_table_address_key",
        ),
        (
            "route_lookup_tables",
            "route_lookup_tables_allocation_kind_check",
        ),
        (
            "route_lookup_tables",
            "route_lookup_tables_desired_state_check",
        ),
        (
            "route_lookup_tables",
            "route_lookup_tables_v2_capacity_check",
        ),
        (
            "route_lookup_tables",
            "route_lookup_tables_v2_metadata_check",
        ),
        (
            "route_lookup_tables",
            "route_lookup_tables_legacy_import_check",
        ),
        (
            "lookup_table_legacy_import_runs",
            "lookup_table_legacy_import_runs_identity_unique",
        ),
        (
            "lookup_table_legacy_import_runs",
            "lookup_table_legacy_import_runs_kind_check",
        ),
        (
            "lookup_table_legacy_import_runs",
            "lookup_table_legacy_import_runs_count_check",
        ),
        (
            "lookup_table_legacy_import_runs",
            "lookup_table_legacy_import_runs_slot_check",
        ),
        (
            "lookup_table_legacy_import_runs",
            "lookup_table_legacy_import_runs_text_check",
        ),
        (
            "lookup_table_legacy_import_evidence",
            "lookup_table_legacy_import_evidence_run_table_unique",
        ),
        (
            "lookup_table_legacy_import_evidence",
            "lookup_table_legacy_import_evidence_kind_check",
        ),
        (
            "lookup_table_legacy_import_evidence",
            "lookup_table_legacy_import_evidence_count_check",
        ),
        (
            "lookup_table_legacy_import_evidence",
            "lookup_table_legacy_import_evidence_slot_check",
        ),
        (
            "lookup_table_legacy_import_evidence",
            "lookup_table_legacy_import_evidence_hash_check",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "lookup_table_shared_catalog_revision_unique",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "lookup_table_shared_catalog_revision_check",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "lookup_table_shared_catalog_address_count_check",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "lookup_table_shared_catalog_source_slot_check",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "lookup_table_shared_catalog_metadata_check",
        ),
        (
            "lookup_table_shared_market_catalog_heads",
            "lookup_table_shared_catalog_head_generation_check",
        ),
        (
            "lookup_table_shared_market_catalog_heads",
            "lookup_table_shared_catalog_head_readiness_check",
        ),
        (
            "lookup_table_shared_market_catalog_heads",
            "lookup_table_shared_catalog_head_lifecycle_check",
        ),
        (
            "lookup_table_shared_market_physical_drifts",
            "lookup_table_shared_market_physical_drift_hash_check",
        ),
        (
            "lookup_table_shared_market_physical_drifts",
            "lookup_table_shared_market_physical_drift_observation_check",
        ),
        (
            "lookup_table_shared_market_physical_drifts",
            "lookup_table_shared_market_physical_drift_resolution_check",
        ),
        (
            "lookup_table_provisioner_controls",
            "lookup_table_provisioner_controls_epoch_check",
        ),
        (
            "lookup_table_provisioner_controls",
            "lookup_table_provisioner_controls_text_check",
        ),
        (
            "lookup_table_provisioner_broadcast_permits",
            "lookup_table_provisioner_broadcast_permits_identity_unique",
        ),
        (
            "lookup_table_provisioner_broadcast_permits",
            "lookup_table_provisioner_broadcast_permits_identity_check",
        ),
        (
            "lookup_table_provisioner_broadcast_permits",
            "lookup_table_provisioner_broadcast_permits_state_check",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "lookup_table_precutover_probe_identity_check",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "lookup_table_precutover_probe_pass_check",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "lookup_table_precutover_probe_bundle_check",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "lookup_table_precutover_probe_shared_tables_pkey",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "lookup_table_precutover_probe_shared_table_id_unique",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "lookup_table_precutover_probe_shared_table_address_unique",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "lookup_table_precutover_probe_shared_table_identity_check",
        ),
        (
            "lookup_table_alert_rules",
            "lookup_table_alert_rules_key_check",
        ),
        (
            "lookup_table_alert_rules",
            "lookup_table_alert_rules_version_check",
        ),
        (
            "lookup_table_alert_rules",
            "lookup_table_alert_rules_severity_check",
        ),
        (
            "lookup_table_alert_rules",
            "lookup_table_alert_rules_configuration_check",
        ),
        (
            "lookup_table_alert_rules",
            "lookup_table_alert_rules_text_check",
        ),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_identity_unique",
        ),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_id_condition_unique",
        ),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_rule_fkey",
        ),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_condition_check",
        ),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_status_check",
        ),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_severity_check",
        ),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_fingerprint_check",
        ),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_details_check",
        ),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_text_check",
        ),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_counter_check",
        ),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_time_check",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_idempotency_key_unique",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_incident_revision_unique",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_rule_fkey",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_incident_fkey",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_event_check",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_state_check",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_channel_check",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_payload_check",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_identity_check",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_counter_check",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_lease_check",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_completion_check",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_http_check",
        ),
        (
            "lookup_table_alert_deliveries",
            "lookup_table_alert_deliveries_text_check",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "lookup_table_legacy_cleanup_attempt_identity_unique",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "lookup_table_legacy_cleanup_attempt_kind_check",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "lookup_table_legacy_cleanup_attempt_state_check",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "lookup_table_legacy_cleanup_attempt_identity_check",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "lookup_table_legacy_cleanup_attempt_refund_shape_check",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "lookup_table_legacy_cleanup_attempt_signed_shape_check",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "lookup_table_legacy_cleanup_attempt_completion_check",
        ),
        (
            "lookup_table_legacy_cleanup_budget_reservations",
            "lookup_table_legacy_cleanup_budget_attempt_unique",
        ),
        (
            "lookup_table_legacy_cleanup_budget_reservations",
            "lookup_table_legacy_cleanup_budget_amount_check",
        ),
        (
            "lookup_table_cluster_budget_reservations",
            "lookup_table_cluster_budget_operation_fence_unique",
        ),
        (
            "lookup_table_cluster_budget_reservations",
            "lookup_table_cluster_budget_amount_check",
        ),
        (
            "lookup_table_families",
            "lookup_table_families_cluster_logical_name_unique",
        ),
        ("lookup_table_families", "lookup_table_families_kind_check"),
        (
            "lookup_table_families",
            "lookup_table_families_desired_state_check",
        ),
        (
            "lookup_table_families",
            "lookup_table_families_generation_check",
        ),
        (
            "lookup_table_families",
            "lookup_table_families_capacity_check",
        ),
        (
            "lookup_table_manifests",
            "lookup_table_manifests_identity_unique",
        ),
        (
            "lookup_table_manifests",
            "lookup_table_manifests_subject_kind_check",
        ),
        (
            "lookup_table_manifests",
            "lookup_table_manifests_subject_vault_check",
        ),
        (
            "lookup_table_manifests",
            "lookup_table_manifests_address_count_check",
        ),
        (
            "lookup_table_manifests",
            "lookup_table_manifests_source_slot_check",
        ),
        (
            "lookup_table_manifest_addresses",
            "lookup_table_manifest_addresses_manifest_ordinal_unique",
        ),
        (
            "lookup_table_manifest_addresses",
            "lookup_table_manifest_addresses_ordinal_check",
        ),
        (
            "lookup_table_manifest_addresses",
            "lookup_table_manifest_addresses_semantic_class_check",
        ),
        (
            "lookup_table_vault_desired_heads",
            "lookup_table_vault_desired_heads_ordinal_check",
        ),
        (
            "lookup_table_vault_desired_heads",
            "lookup_table_vault_desired_heads_revision_check",
        ),
        (
            "lookup_table_vault_bindings",
            "lookup_table_vault_bindings_allocation_mode_check",
        ),
        (
            "lookup_table_vault_bindings",
            "lookup_table_vault_bindings_lifecycle_state_check",
        ),
        (
            "lookup_table_vault_bindings",
            "lookup_table_vault_bindings_capacity_check",
        ),
        (
            "lookup_table_vault_bindings",
            "lookup_table_vault_bindings_ordinal_check",
        ),
        (
            "lookup_table_vault_bindings",
            "lookup_table_vault_bindings_desired_revision_check",
        ),
        (
            "lookup_table_vault_bindings",
            "lookup_table_vault_bindings_activation_interval_check",
        ),
        (
            "lookup_table_vault_bindings",
            "lookup_table_vault_bindings_predecessor_check",
        ),
        (
            "lookup_table_usage_leases",
            "lookup_table_usage_leases_reference_unique",
        ),
        (
            "lookup_table_usage_leases",
            "lookup_table_usage_leases_kind_check",
        ),
        (
            "lookup_table_usage_leases",
            "lookup_table_usage_leases_interval_check",
        ),
        (
            "lookup_table_provisioning_requests",
            "lookup_table_provisioning_requests_identity_unique",
        ),
        (
            "lookup_table_provisioning_requests",
            "lookup_table_provisioning_requests_status_check",
        ),
        (
            "lookup_table_provisioning_requests",
            "lookup_table_provisioning_requests_lease_check",
        ),
        (
            "lookup_table_provisioning_requests",
            "lookup_table_provisioning_requests_desired_check",
        ),
        (
            "lookup_table_provisioning_request_addresses",
            "lookup_table_request_addresses_class_ordinal_unique",
        ),
        (
            "lookup_table_provisioning_request_addresses",
            "lookup_table_provisioning_request_addresses_class_check",
        ),
        (
            "lookup_table_provisioning_request_addresses",
            "lookup_table_provisioning_request_addresses_ordinal_check",
        ),
        (
            "lookup_table_operations",
            "lookup_table_operations_idempotency_key_unique",
        ),
        (
            "lookup_table_operations",
            "lookup_table_operations_kind_check",
        ),
        (
            "lookup_table_operations",
            "lookup_table_operations_state_check",
        ),
        (
            "lookup_table_operations",
            "lookup_table_operations_target_check",
        ),
        (
            "lookup_table_operations",
            "lookup_table_operations_context_check",
        ),
        (
            "lookup_table_operations",
            "lookup_table_operations_lease_check",
        ),
        (
            "lookup_table_operations",
            "lookup_table_operations_signed_metadata_check",
        ),
        (
            "lookup_table_operations",
            "lookup_table_operations_counter_check",
        ),
        (
            "lookup_table_operations",
            "lookup_table_operations_slot_check",
        ),
        (
            "lookup_table_operations",
            "lookup_table_operations_lamports_check",
        ),
        (
            "lookup_table_operation_addresses",
            "lookup_table_operation_addresses_operation_ordinal_unique",
        ),
        (
            "lookup_table_operation_addresses",
            "lookup_table_operation_addresses_ordinal_check",
        ),
        (
            "lookup_table_addresses",
            "lookup_table_addresses_table_ordinal_unique",
        ),
        (
            "lookup_table_addresses",
            "lookup_table_addresses_ordinal_check",
        ),
        (
            "lookup_table_addresses",
            "lookup_table_addresses_slot_check",
        ),
        (
            "lookup_table_route_readiness_current",
            "lookup_table_route_readiness_state_check",
        ),
        (
            "lookup_table_route_readiness_current",
            "lookup_table_route_readiness_coverage_check",
        ),
        (
            "lookup_table_route_readiness_current",
            "lookup_table_route_readiness_packet_check",
        ),
        (
            "lookup_table_route_readiness_current",
            "lookup_table_route_readiness_slot_check",
        ),
        (
            "lookup_table_route_readiness_current",
            "lookup_table_route_readiness_selection_check",
        ),
        (
            "lookup_table_route_readiness_current",
            "lookup_table_route_readiness_simulation_check",
        ),
        (
            "lookup_table_rollout_controls",
            "lookup_table_rollout_controls_mode_check",
        ),
        (
            "signed_route_submissions",
            "signed_route_submissions_confirmation_lease_check",
        ),
    ] {
        let exists: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM pg_constraint c
                WHERE c.conrelid = format('loyal_yield.%I', $1)::regclass
                  AND c.conname = $2
            )
            "#,
        )
        .bind(relation)
        .bind(constraint)
        .fetch_one(pool)
        .await?;
        if !exists {
            return Err(format!("missing constraint loyal_yield.{relation}.{constraint}").into());
        }
    }
    for (relation, column) in [
        ("route_lookup_tables", "family_id"),
        ("lookup_table_manifests", "family_id"),
        ("lookup_table_manifests", "vault_id"),
        ("lookup_table_manifest_addresses", "manifest_id"),
        ("lookup_table_vault_desired_heads", "family_id"),
        ("lookup_table_vault_desired_heads", "vault_id"),
        ("lookup_table_vault_desired_heads", "manifest_id"),
        ("lookup_table_vault_bindings", "vault_id"),
        ("lookup_table_vault_bindings", "family_id"),
        ("lookup_table_vault_bindings", "route_lookup_table_id"),
        ("lookup_table_vault_bindings", "manifest_id"),
        ("lookup_table_vault_bindings", "predecessor_binding_id"),
        ("lookup_table_usage_leases", "route_lookup_table_id"),
        ("lookup_table_usage_leases", "vault_id"),
        ("lookup_table_usage_leases", "binding_id"),
        ("lookup_table_provisioning_requests", "vault_id"),
        ("lookup_table_provisioning_requests", "shared_manifest_id"),
        ("lookup_table_provisioning_requests", "vault_manifest_id"),
        ("lookup_table_provisioning_request_addresses", "request_id"),
        ("lookup_table_operations", "family_id"),
        ("lookup_table_operations", "route_lookup_table_id"),
        ("lookup_table_operations", "manifest_id"),
        ("lookup_table_operations", "binding_id"),
        ("lookup_table_operation_addresses", "operation_id"),
        ("lookup_table_addresses", "route_lookup_table_id"),
        ("lookup_table_addresses", "added_operation_id"),
        ("lookup_table_route_readiness_current", "vault_id"),
        ("lookup_table_route_readiness_current", "manifest_id"),
        ("lookup_table_route_readiness_current", "shared_family_id"),
        ("lookup_table_route_readiness_current", "vault_binding_id"),
        ("lookup_table_rollout_controls", "vault_id"),
        ("lookup_table_shared_market_physical_drifts", "family_id"),
        (
            "lookup_table_shared_market_physical_drifts",
            "catalog_revision_id",
        ),
        (
            "lookup_table_shared_market_physical_drifts",
            "route_lookup_table_id",
        ),
        ("lookup_table_precutover_probe_runs", "vault_id"),
        ("lookup_table_precutover_probe_runs", "catalog_revision_id"),
        ("lookup_table_precutover_probe_runs", "shared_manifest_id"),
        (
            "lookup_table_precutover_probe_runs",
            "route_lookup_table_id",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "probe_run_id",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "route_lookup_table_id",
        ),
        ("lookup_table_provisioner_broadcast_permits", "operation_id"),
        ("lookup_table_provisioner_broadcast_permits", "cluster"),
        ("lookup_table_alert_incidents", "alert_condition"),
        ("lookup_table_alert_deliveries", "incident_id"),
        ("lookup_table_alert_deliveries", "alert_condition"),
        (
            "lookup_table_legacy_cleanup_attempts",
            "route_lookup_table_id",
        ),
        (
            "lookup_table_legacy_cleanup_budget_reservations",
            "legacy_cleanup_attempt_id",
        ),
        ("lookup_table_cluster_budget_reservations", "operation_id"),
        ("rebalance_opportunities", "vault_id"),
        ("rebalance_opportunities", "source_snapshot_id"),
        ("rebalance_opportunities", "optimizer_epoch_id"),
        ("rebalance_opportunities", "decision_id"),
        (
            "lookup_table_provisioning_request_consumers",
            "opportunity_id",
        ),
        (
            "lookup_table_provisioning_request_consumers",
            "provisioning_request_id",
        ),
        ("signed_route_submissions", "opportunity_id"),
        ("signed_route_submissions", "optimizer_epoch_id"),
        ("route_account_conflict_leases", "opportunity_id"),
        ("route_account_conflict_leases", "submission_id"),
    ] {
        let exists: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM pg_constraint c
                JOIN LATERAL unnest(c.conkey) AS key(attnum) ON TRUE
                JOIN pg_attribute a
                  ON a.attrelid = c.conrelid
                 AND a.attnum = key.attnum
                WHERE c.conrelid = format('loyal_yield.%I', $1)::regclass
                  AND c.contype = 'f'
                  AND a.attname = $2
            )
            "#,
        )
        .bind(relation)
        .bind(column)
        .fetch_one(pool)
        .await?;
        if !exists {
            return Err(format!("missing foreign key loyal_yield.{relation}.{column}").into());
        }
    }
    for index in [
        "lookup_table_families_one_active_kind_idx",
        "route_lookup_tables_unique_family_generation_shard_idx",
        "route_lookup_tables_legacy_import_idx",
        "lookup_table_legacy_import_evidence_table_idx",
        "lookup_table_shared_catalog_revision_family_idx",
        "lookup_table_shared_market_physical_drift_open_idx",
        "lookup_table_cluster_budget_active_idx",
        "lookup_table_provisioner_broadcast_permits_active_idx",
        "lookup_table_provisioner_broadcast_permits_cluster_active_idx",
        "lookup_table_precutover_probe_shared_tables_route_idx",
        "lookup_table_vault_bindings_one_active_idx",
        "lookup_table_usage_leases_active_table_idx",
        "lookup_table_provisioning_requests_work_queue_idx",
        "lookup_table_rollout_controls_global_idx",
        "lookup_table_rollout_controls_vault_idx",
        "lookup_table_alert_rules_enabled_idx",
        "lookup_table_alert_incidents_open_idx",
        "lookup_table_alert_deliveries_work_idx",
        "lookup_table_alert_deliveries_incident_idx",
        "lookup_table_legacy_cleanup_attempt_active_unique",
        "lookup_table_legacy_cleanup_attempt_recovery_idx",
        "lookup_table_legacy_cleanup_budget_active_idx",
        "active_rebalance_opportunity_slots_pkey",
        "rebalance_opportunities_ready_priority_idx",
        "rebalance_opportunities_expired_lease_idx",
        "rebalance_opportunities_status_idx",
        "fleet_planning_dirty_vaults_ready_idx",
        "lookup_table_provisioning_request_consumers_request_idx",
        "lookup_table_provisioning_requests_priority_queue_idx",
        "orchestration_outbox_pending_idx",
        "signed_route_submissions_state_idx",
        "signed_route_submissions_confirmation_queue_idx",
        "signed_route_submissions_one_nonterminal_opportunity_idx",
        "route_account_conflict_leases_opportunity_idx",
        "route_account_conflict_leases_submission_idx",
    ] {
        let exists: bool =
            sqlx::query_scalar("SELECT to_regclass(format('loyal_yield.%I', $1)) IS NOT NULL")
                .bind(index)
                .fetch_one(pool)
                .await?;
        if !exists {
            return Err(format!("missing index loyal_yield.{index}").into());
        }
    }
    let runnable_priority_index: String = sqlx::query_scalar(
        "SELECT pg_get_indexdef('loyal_yield.rebalance_opportunities_ready_priority_idx'::regclass)",
    )
    .fetch_one(pool)
    .await?;
    if !runnable_priority_index.contains(
        "(cluster, opportunity_state, scheduler_priority_anchor DESC, economic_priority DESC, created_at, id)",
    ) || !runnable_priority_index.contains("'ready'::text")
        || !runnable_priority_index.contains("'revalidate'::text")
        || runnable_priority_index.contains("'leased'::text")
    {
        return Err(
            "rebalance runnable-priority index must key exact runnable state and exclude leased rows"
                .into(),
        );
    }
    let expired_lease_index: String = sqlx::query_scalar(
        "SELECT pg_get_indexdef('loyal_yield.rebalance_opportunities_expired_lease_idx'::regclass)",
    )
    .fetch_one(pool)
    .await?;
    if !expired_lease_index.contains("(cluster, lease_kind, lease_expires_at, id)")
        || !expired_lease_index.contains("opportunity_state = 'leased'::text")
    {
        return Err(
            "rebalance expired-lease index must key lease lane/expiry and contain only leased rows"
                .into(),
        );
    }
    let obsolete_active_queue_index_exists: bool = sqlx::query_scalar(
        "SELECT to_regclass('loyal_yield.rebalance_opportunities_one_active_vault_idx') IS NOT NULL",
    )
    .fetch_one(pool)
    .await?;
    if obsolete_active_queue_index_exists {
        return Err(
            "obsolete active-vault queue index reintroduces ALT-cold claim write amplification"
                .into(),
        );
    }
    let retired_reference_guard: String = sqlx::query_scalar(
        "SELECT pg_get_functiondef('loyal_yield.guard_retired_legacy_lookup_table_reference()'::regprocedure)",
    )
    .fetch_one(pool)
    .await?;
    if retired_reference_guard.contains("reusable-alt-rollout:")
        || !retired_reference_guard.contains("ORDER BY route_table.id")
        || !retired_reference_guard.contains("FOR SHARE")
    {
        return Err("retired legacy reference guard must use canonical physical-table share locks without the rollout advisory lock".into());
    }
    let rollout_guard: String = sqlx::query_scalar(
        "SELECT pg_get_functiondef('loyal_yield.guard_rollout_during_legacy_cleanup()'::regprocedure)",
    )
    .fetch_one(pool)
    .await?;
    if !rollout_guard.contains("reusable-alt-rollout:")
        || !rollout_guard.contains("pg_advisory_xact_lock")
    {
        return Err("legacy cleanup rollout mutation lost its cluster-wide advisory fence".into());
    }
    for (relation, trigger) in [
        (
            "route_lookup_tables",
            "route_lookup_tables_legacy_kind_immutable",
        ),
        (
            "lookup_table_legacy_import_runs",
            "lookup_table_legacy_import_runs_immutable",
        ),
        (
            "lookup_table_legacy_import_evidence",
            "lookup_table_legacy_import_evidence_immutable",
        ),
        (
            "lookup_table_legacy_import_evidence",
            "lookup_table_legacy_import_evidence_consistent",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "lookup_table_shared_catalog_revision_consistent",
        ),
        (
            "lookup_table_shared_market_catalog_revisions",
            "lookup_table_shared_catalog_revisions_immutable",
        ),
        (
            "lookup_table_shared_market_catalog_heads",
            "lookup_table_shared_catalog_head_consistent",
        ),
        (
            "lookup_table_shared_market_physical_drifts",
            "lookup_table_shared_market_physical_drift_immutable",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "lookup_table_precutover_probe_runs_immutable",
        ),
        (
            "lookup_table_precutover_probe_runs",
            "lookup_table_precutover_probe_bundle_consistent",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "lookup_table_precutover_probe_shared_tables_immutable",
        ),
        (
            "lookup_table_precutover_probe_shared_tables",
            "lookup_table_precutover_probe_shared_tables_consistent",
        ),
        ("lookup_table_alert_rules", "lookup_table_alert_rules_guard"),
        (
            "lookup_table_alert_incidents",
            "lookup_table_alert_incidents_guard",
        ),
        (
            "lookup_table_cluster_budget_reservations",
            "lookup_table_cluster_budget_reservations_immutable",
        ),
        (
            "lookup_table_legacy_cleanup_budget_reservations",
            "lookup_table_legacy_cleanup_budget_reservations_immutable",
        ),
        (
            "lookup_table_legacy_cleanup_attempts",
            "lookup_table_legacy_cleanup_attempt_budget_guard",
        ),
        ("lookup_table_manifests", "lookup_table_manifests_immutable"),
        (
            "lookup_table_manifest_addresses",
            "lookup_table_manifest_addresses_immutable",
        ),
        (
            "lookup_table_vault_bindings",
            "lookup_table_vault_bindings_reservation_accounting",
        ),
        (
            "lookup_table_provisioning_requests",
            "lookup_table_provisioning_requests_immutable",
        ),
        (
            "lookup_table_provisioning_request_addresses",
            "lookup_table_provisioning_request_addresses_immutable",
        ),
        ("optimizer_epochs", "optimizer_epochs_immutable"),
        (
            "vault_reserve_positions_current",
            "vault_reserve_position_fleet_planning_dirty",
        ),
        (
            "vault_idle_token_balances_current",
            "vault_idle_balance_fleet_planning_dirty",
        ),
        ("managed_vaults", "managed_vault_fleet_planning_dirty"),
        ("route_policies", "route_policy_fleet_planning_dirty"),
        (
            "rebalance_decisions",
            "rebalance_decision_fleet_cooldown_dirty",
        ),
        (
            "lookup_table_provisioning_request_consumers",
            "lookup_table_request_consumer_priority",
        ),
        ("rebalance_opportunities", "rebalance_opportunity_wakeup"),
        (
            "lookup_table_provisioning_requests",
            "lookup_table_request_rebalance_wakeup",
        ),
        (
            "rebalance_opportunities",
            "rebalance_opportunity_scheduler_priority",
        ),
        (
            "rebalance_opportunities",
            "rebalance_opportunity_state_entry",
        ),
        (
            "rebalance_decisions",
            "rebalance_decision_links_execute_opportunity",
        ),
        (
            "signed_route_submissions",
            "signed_route_submission_finishes_terminal_state",
        ),
        (
            "signed_route_submissions",
            "signed_route_submission_requires_decision",
        ),
        (
            "signed_route_submissions",
            "signed_route_submission_evidence_immutable",
        ),
        (
            "signed_route_submissions",
            "signed_route_submission_state_entered_at",
        ),
    ] {
        let exists: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM pg_trigger t
                WHERE t.tgrelid = format('loyal_yield.%I', $1)::regclass
                  AND t.tgname = $2
                  AND NOT t.tgisinternal
            )
            "#,
        )
        .bind(relation)
        .bind(trigger)
        .fetch_one(pool)
        .await?;
        if !exists {
            return Err(format!("missing trigger loyal_yield.{relation}.{trigger}").into());
        }
    }
    let alert_rule_keys: Vec<String> = sqlx::query_scalar(
        r#"
        SELECT COALESCE(array_agg(rule_key ORDER BY rule_key), '{}'::TEXT[])
        FROM loyal_yield.lookup_table_alert_rules
        "#,
    )
    .fetch_one(pool)
    .await?;
    let expected_alert_rule_keys = vec![
        "authority_prefix_drift".to_owned(),
        "capacity_headroom".to_owned(),
        "cleanup_anomalies".to_owned(),
        "fallback_use".to_owned(),
        "missing_coverage".to_owned(),
        "operation_backlog".to_owned(),
        "orphaned_tables".to_owned(),
        "provisioning_budget".to_owned(),
        "readiness_regression".to_owned(),
    ];
    if alert_rule_keys != expected_alert_rule_keys {
        return Err(
            "reusable ALT alert rule catalog must contain exactly the nine durable rule identities"
                .into(),
        );
    }
    let yield_deposits_exist: bool = sqlx::query_scalar(
        "SELECT to_regclass('loyal_yield.user_yield_position_deposits') IS NOT NULL",
    )
    .fetch_one(pool)
    .await?;
    if yield_deposits_exist {
        for column in [
            "balance_sweep_execution_id",
            "balance_sweep_scheduled_slot_id",
        ] {
            let exists: bool = sqlx::query_scalar(
                r#"
                SELECT EXISTS (
                    SELECT 1
                    FROM information_schema.columns
                    WHERE table_schema = 'loyal_yield'
                      AND table_name = 'user_yield_position_deposits'
                      AND column_name = $1
                )
                "#,
            )
            .bind(column)
            .fetch_one(pool)
            .await?;
            if !exists {
                return Err(
                    format!("missing loyal_yield.user_yield_position_deposits.{column}").into(),
                );
            }
        }
    }
    for (relation, column) in [
        ("balance_sweep_targets", "wallet_usdc_ata"),
        ("balance_sweep_targets", "vault_usdc_ata"),
        ("balance_sweep_wallet_balances_current", "wallet_usdc_ata"),
        ("balance_sweep_wallet_balance_events", "wallet_usdc_ata"),
        ("balance_sweep_executions", "source_wallet_ata"),
        ("balance_sweep_executions", "destination_vault_ata"),
    ] {
        let is_nullable: Option<String> = sqlx::query_scalar(
            r#"
            SELECT is_nullable
            FROM information_schema.columns
            WHERE table_schema = 'loyal_yield'
              AND table_name = $1
              AND column_name = $2
            "#,
        )
        .bind(relation)
        .bind(column)
        .fetch_optional(pool)
        .await?;
        if is_nullable.as_deref() != Some("YES") {
            return Err(format!("loyal_yield.{relation}.{column} must be nullable").into());
        }
    }
    let current_balance_pkey_columns: Option<Vec<String>> = sqlx::query_scalar(
        r#"
        SELECT ARRAY_AGG(a.attname ORDER BY cols.ordinality)
        FROM pg_constraint c
        CROSS JOIN LATERAL UNNEST(c.conkey) WITH ORDINALITY AS cols(attnum, ordinality)
        JOIN pg_attribute a
          ON a.attrelid = c.conrelid
         AND a.attnum = cols.attnum
        WHERE c.conrelid = 'loyal_yield.balance_sweep_wallet_balances_current'::regclass
          AND c.contype = 'p'
        "#,
    )
    .fetch_one(pool)
    .await?;
    if current_balance_pkey_columns != Some(vec!["target_id".to_owned(), "mint".to_owned()]) {
        return Err(
            "loyal_yield.balance_sweep_wallet_balances_current primary key must be (target_id, mint)"
                .into(),
        );
    }
    let idle_balance_pkey_columns: Option<Vec<String>> = sqlx::query_scalar(
        r#"
        SELECT ARRAY_AGG(a.attname ORDER BY cols.ordinality)
        FROM pg_constraint c
        CROSS JOIN LATERAL UNNEST(c.conkey) WITH ORDINALITY AS cols(attnum, ordinality)
        JOIN pg_attribute a
          ON a.attrelid = c.conrelid
         AND a.attnum = cols.attnum
        WHERE c.conrelid = 'loyal_yield.vault_idle_token_balances_current'::regclass
          AND c.contype = 'p'
        "#,
    )
    .fetch_one(pool)
    .await?;
    if idle_balance_pkey_columns != Some(vec!["vault_id".to_owned(), "mint".to_owned()]) {
        return Err(
            "loyal_yield.vault_idle_token_balances_current primary key must be (vault_id, mint)"
                .into(),
        );
    }
    let has_idle_decision_reason: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM pg_enum e
            JOIN pg_type t ON t.oid = e.enumtypid
            JOIN pg_namespace n ON n.oid = t.typnamespace
            WHERE n.nspname = 'loyal_yield'
              AND t.typname = 'decision_reason'
              AND e.enumlabel = 'idle_vault_liquidity_available'
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !has_idle_decision_reason {
        return Err(
            "missing loyal_yield.decision_reason idle_vault_liquidity_available value".into(),
        );
    }
    let has_realtime_emit_function: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM pg_proc p
            JOIN pg_namespace n ON n.oid = p.pronamespace
            WHERE n.nspname = 'loyal_yield'
              AND p.proname = 'emit_realtime_event'
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !has_realtime_emit_function {
        return Err("missing loyal_yield.emit_realtime_event function".into());
    }
    let has_autodeposit_realtime_function: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM pg_proc p
            JOIN pg_namespace n ON n.oid = p.pronamespace
            WHERE n.nspname = 'loyal_yield'
              AND p.proname = 'emit_autodeposit_scheduled_slot_realtime_event'
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !has_autodeposit_realtime_function {
        return Err(
            "missing loyal_yield.emit_autodeposit_scheduled_slot_realtime_event function".into(),
        );
    }
    let has_autodeposit_realtime_trigger: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM pg_trigger
            WHERE tgrelid = 'loyal_yield.balance_sweep_scheduled_slots'::regclass
              AND tgname = 'balance_sweep_scheduled_slots_realtime_event'
              AND NOT tgisinternal
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !has_autodeposit_realtime_trigger {
        return Err("missing loyal_yield.balance_sweep_scheduled_slots realtime trigger".into());
    }
    let has_autodeposit_wakeup_function: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM pg_proc p
            JOIN pg_namespace n ON n.oid = p.pronamespace
            WHERE n.nspname = 'loyal_yield'
              AND p.proname = 'notify_autodeposit_requested_slot'
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !has_autodeposit_wakeup_function {
        return Err("missing loyal_yield.notify_autodeposit_requested_slot function".into());
    }
    let has_autodeposit_wakeup_trigger: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM pg_trigger
            WHERE tgrelid = 'loyal_yield.balance_sweep_scheduled_slots'::regclass
              AND tgname = 'balance_sweep_scheduled_slots_autodeposit_wakeup'
              AND NOT tgisinternal
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !has_autodeposit_wakeup_trigger {
        return Err(
            "missing loyal_yield.balance_sweep_scheduled_slots autodeposit wakeup trigger".into(),
        );
    }

    let has_realtime_private_scope_function: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM pg_proc p
            JOIN pg_namespace n ON n.oid = p.pronamespace
            WHERE n.nspname = 'loyal_yield'
              AND p.proname = 'realtime_private_scope_requires_identity'
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !has_realtime_private_scope_function {
        return Err("missing loyal_yield.realtime_private_scope_requires_identity function".into());
    }

    let has_autodeposit_execution_function: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM pg_proc p
            JOIN pg_namespace n ON n.oid = p.pronamespace
            WHERE n.nspname = 'loyal_yield'
              AND p.proname = 'emit_autodeposit_execution_realtime_event'
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !has_autodeposit_execution_function {
        return Err(
            "missing loyal_yield.emit_autodeposit_execution_realtime_event function".into(),
        );
    }

    let has_autodeposit_execution_trigger: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM pg_trigger
            WHERE tgrelid = 'loyal_yield.balance_sweep_executions'::regclass
              AND tgname = 'balance_sweep_executions_realtime_event'
              AND NOT tgisinternal
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !has_autodeposit_execution_trigger {
        return Err("missing loyal_yield.balance_sweep_executions realtime trigger".into());
    }

    for (function, description) in [
        (
            "emit_user_yield_position_realtime_event",
            "user yield position realtime",
        ),
        (
            "emit_user_yield_holding_event_realtime_event",
            "user yield holding event realtime",
        ),
        (
            "emit_earn_onboarding_realtime_event",
            "earn onboarding realtime",
        ),
        (
            "mark_autodeposit_execution_completed",
            "autodeposit completion transition",
        ),
        (
            "mark_autodeposit_execution_failed",
            "autodeposit failure transition",
        ),
        (
            "emit_autodeposit_configuration_realtime_event",
            "autodeposit configuration realtime",
        ),
        (
            "emit_rebalance_confirmation_realtime_event",
            "rebalance confirmation realtime",
        ),
    ] {
        let exists: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM pg_proc p
                JOIN pg_namespace n ON n.oid = p.pronamespace
                WHERE n.nspname = 'loyal_yield'
                  AND p.proname = $1
            )
            "#,
        )
        .bind(function)
        .fetch_one(pool)
        .await?;
        if !exists {
            return Err(format!("missing loyal_yield.{function} function ({description})").into());
        }
    }

    for (relation, trigger) in [
        (
            "user_yield_positions",
            "user_yield_positions_realtime_event",
        ),
        (
            "user_yield_position_holding_events",
            "user_yield_position_holding_events_realtime_event",
        ),
        (
            "earn_deposit_onboarding_attempts",
            "earn_deposit_onboarding_attempts_realtime_event",
        ),
        (
            "balance_sweep_targets",
            "balance_sweep_targets_configuration_realtime_event",
        ),
        (
            "rebalance_decisions",
            "rebalance_decisions_confirmation_realtime_event",
        ),
    ] {
        let relation_exists: bool = sqlx::query_scalar(
            r#"
            SELECT to_regclass(format('loyal_yield.%I', $1)) IS NOT NULL
            "#,
        )
        .bind(relation)
        .fetch_one(pool)
        .await?;
        if relation_exists {
            let trigger_exists: bool = sqlx::query_scalar(
                r#"
                SELECT EXISTS (
                    SELECT 1
                    FROM pg_trigger
                    WHERE tgrelid = format('loyal_yield.%I', $1)::regclass
                      AND tgname = $2
                      AND NOT tgisinternal
                )
                "#,
            )
            .bind(relation)
            .bind(trigger)
            .fetch_one(pool)
            .await?;
            if !trigger_exists {
                return Err(format!("missing loyal_yield.{relation} realtime trigger").into());
            }
        }
    }
    Ok(())
}

async fn verify_reusable_alts(pool: &PgPool) -> Result<(), Box<dyn Error>> {
    let migrations_applied: bool = sqlx::query_scalar(
        r#"
        SELECT count(*) = 3
        FROM loyal_yield.schema_migrations
        WHERE (version = 20 AND name = 'demand_driven_shared_market_catalog')
           OR (version = 21 AND name = 'reusable_alt_production_controls')
           OR (version = 22 AND name = 'shared_market_alt_bundles')
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !migrations_applied {
        return Err("migrations 20 demand_driven_shared_market_catalog, 21 reusable_alt_production_controls, and 22 shared_market_alt_bundles must all be recorded".into());
    }

    let invalid_shared_catalogs: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_shared_market_catalog_heads head
        JOIN loyal_yield.lookup_table_shared_market_catalog_revisions revision
          ON revision.id = head.catalog_revision_id
        JOIN loyal_yield.lookup_table_families family ON family.id = head.family_id
        JOIN loyal_yield.lookup_table_manifests manifest ON manifest.id = revision.manifest_id
        WHERE revision.family_id <> head.family_id
           OR family.kind <> 'shared_market'
           OR family.desired_state <> 'active'
           OR manifest.family_id <> head.family_id
           OR manifest.subject_kind <> 'shared_market'
           OR manifest.sealed_at IS NULL
           OR manifest.catalog_version <> revision.catalog_version
           OR manifest.desired_set_hash <> revision.desired_set_hash
           OR manifest.address_count <> revision.address_count
           OR revision.address_count <> (
               SELECT count(*)
               FROM loyal_yield.lookup_table_manifest_addresses address
               WHERE address.manifest_id = revision.manifest_id
                 AND address.semantic_class = 'shared_market'
           )
           OR (
               head.readiness_state = 'active'
               AND (
                   head.target_generation IS DISTINCT FROM family.active_generation
                   OR head.activated_at IS NULL
                   OR (
                       revision.address_count + family.allocation_high_water - 1
                   ) / family.allocation_high_water <> (
                       SELECT count(*)
                       FROM loyal_yield.route_lookup_tables route_table
                       WHERE route_table.family_id = family.id
                         AND route_table.generation = family.active_generation
                         AND route_table.allocation_kind = 'shared_market'
                         AND route_table.desired_state = 'active'
                         AND route_table.usable_address_count = route_table.address_count
                         AND route_table.last_verified_slot IS NOT NULL
                   )
                   OR EXISTS (
                       SELECT 1
                       FROM loyal_yield.route_lookup_tables route_table
                       WHERE route_table.family_id = family.id
                         AND route_table.generation = family.active_generation
                         AND route_table.allocation_kind = 'shared_market'
                         AND (
                             route_table.desired_state <> 'active'
                             OR route_table.address_count > family.allocation_high_water
                             OR route_table.usable_address_count <> route_table.address_count
                             OR route_table.last_verified_slot IS NULL
                         )
                   )
                   OR revision.address_count <> (
                       SELECT count(*)
                       FROM loyal_yield.route_lookup_tables route_table
                       JOIN loyal_yield.lookup_table_addresses membership
                         ON membership.route_lookup_table_id = route_table.id
                       WHERE route_table.family_id = family.id
                         AND route_table.generation = family.active_generation
                         AND route_table.allocation_kind = 'shared_market'
                   )
                   OR revision.address_count <> (
                       SELECT count(DISTINCT membership.address)
                       FROM loyal_yield.route_lookup_tables route_table
                       JOIN loyal_yield.lookup_table_addresses membership
                         ON membership.route_lookup_table_id = route_table.id
                       WHERE route_table.family_id = family.id
                         AND route_table.generation = family.active_generation
                         AND route_table.allocation_kind = 'shared_market'
                   )
                   OR EXISTS (
                       SELECT address.address
                       FROM loyal_yield.lookup_table_manifest_addresses address
                       WHERE address.manifest_id = revision.manifest_id
                       EXCEPT
                       SELECT membership.address
                       FROM loyal_yield.route_lookup_tables route_table
                       JOIN loyal_yield.lookup_table_addresses membership
                         ON membership.route_lookup_table_id = route_table.id
                       WHERE route_table.family_id = family.id
                         AND route_table.generation = family.active_generation
                         AND route_table.allocation_kind = 'shared_market'
                   )
                   OR EXISTS (
                       SELECT membership.address
                       FROM loyal_yield.route_lookup_tables route_table
                       JOIN loyal_yield.lookup_table_addresses membership
                         ON membership.route_lookup_table_id = route_table.id
                       WHERE route_table.family_id = family.id
                         AND route_table.generation = family.active_generation
                         AND route_table.allocation_kind = 'shared_market'
                       EXCEPT
                       SELECT address.address
                       FROM loyal_yield.lookup_table_manifest_addresses address
                       WHERE address.manifest_id = revision.manifest_id
                   )
                   OR EXISTS (
                       WITH expected AS (
                           SELECT (
                                      address.ordinal
                                      / family.allocation_high_water
                                  )::INTEGER AS shard_ordinal,
                                  (
                                      address.ordinal
                                      % family.allocation_high_water
                                  )::INTEGER AS physical_ordinal,
                                  address.address
                           FROM loyal_yield.lookup_table_manifest_addresses address
                           WHERE address.manifest_id = revision.manifest_id
                             AND address.semantic_class = 'shared_market'
                       ), observed AS (
                           SELECT route_table.shard_ordinal,
                                  membership.ordinal AS physical_ordinal,
                                  membership.address
                           FROM loyal_yield.route_lookup_tables route_table
                           JOIN loyal_yield.lookup_table_addresses membership
                             ON membership.route_lookup_table_id = route_table.id
                           WHERE route_table.family_id = family.id
                             AND route_table.generation = family.active_generation
                             AND route_table.allocation_kind = 'shared_market'
                       )
                       SELECT 1
                       FROM expected
                       FULL JOIN observed
                         USING (shard_ordinal, physical_ordinal)
                       WHERE expected.address IS DISTINCT FROM observed.address
                   )
               )
           )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_shared_catalogs != 0 {
        return Err(format!(
            "invalid authoritative shared-market catalog head(s): {invalid_shared_catalogs}"
        )
        .into());
    }

    let invalid_precutover_probe_bundles: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_precutover_probe_runs probe
        LEFT JOIN loyal_yield.lookup_table_shared_market_catalog_revisions revision
          ON revision.id = probe.catalog_revision_id
        LEFT JOIN loyal_yield.lookup_table_manifests manifest
          ON manifest.id = probe.shared_manifest_id
        WHERE revision.id IS NULL
           OR manifest.id IS NULL
           OR revision.manifest_id <> probe.shared_manifest_id
           OR manifest.family_id <> revision.family_id
           OR probe.result <> 'pass'
           OR probe.shared_table_bundle_hash !~ '^[0-9a-f]{64}$'
           OR probe.shared_table_bundle_hash IS DISTINCT FROM (
               SELECT loyal_yield.hash_length_prefixed_text(
                   ARRAY['loyal-reusable-shared-table-bundle-v1']::TEXT[]
                   || COALESCE(
                       array_agg(
                           bundle_field.field_value
                           ORDER BY shared.shard_ordinal,
                                    bundle_field.field_ordinal
                       ),
                       ARRAY[]::TEXT[]
                   )
               )
               FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
               CROSS JOIN LATERAL (
                   VALUES
                       (0, shared.route_lookup_table_id::TEXT),
                       (1, shared.shard_ordinal::TEXT),
                       (2, shared.shared_table_address),
                       (3, shared.shared_authority),
                       (4, shared.shared_mutation_epoch::TEXT),
                       (5, shared.finalized_last_extended_slot::TEXT),
                       (6, shared.finalized_address_hash),
                       (7, shared.finalized_address_count::TEXT)
               ) AS bundle_field(field_ordinal, field_value)
               WHERE shared.probe_run_id = probe.id
           )
           OR probe.shared_table_count <> (
               SELECT count(*)
               FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
               WHERE shared.probe_run_id = probe.id
           )
           OR probe.finalized_bundle_address_count <> revision.address_count
           OR probe.finalized_bundle_address_count <> (
               SELECT COALESCE(sum(shared.finalized_address_count), 0)
               FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
               WHERE shared.probe_run_id = probe.id
           )
           OR NOT EXISTS (
               SELECT 1
               FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
               WHERE shared.probe_run_id = probe.id
                 AND shared.route_lookup_table_id = probe.route_lookup_table_id
                 AND shared.shared_table_address = probe.shared_table_address
                 AND shared.shared_authority = probe.shared_authority
                 AND shared.shared_mutation_epoch = probe.shared_mutation_epoch
                 AND shared.finalized_slot = probe.finalized_slot
                 AND shared.finalized_last_extended_slot = probe.finalized_last_extended_slot
                 AND shared.finalized_address_hash = probe.finalized_address_hash
                 AND shared.finalized_address_count = probe.finalized_address_count
           )
           OR EXISTS (
               SELECT 1
               FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
               LEFT JOIN loyal_yield.route_lookup_tables route_table
                 ON route_table.id = shared.route_lookup_table_id
               JOIN loyal_yield.lookup_table_families family
                 ON family.id = revision.family_id
               WHERE shared.probe_run_id = probe.id
                 AND (
                     shared.shard_ordinal < 0
                     OR shared.finalized_slot <> probe.finalized_slot
                     OR shared.finalized_slot <= shared.finalized_last_extended_slot
                     OR shared.finalized_address_hash !~ '^[0-9a-f]{64}$'
                     OR shared.finalized_address_count NOT BETWEEN 1 AND 256
                     OR route_table.id IS NULL
                     OR family.cluster <> probe.cluster
                     OR route_table.cluster <> probe.cluster
                     OR route_table.family_id <> revision.family_id
                     OR route_table.allocation_kind <> 'shared_market'
                     OR route_table.shard_ordinal <> shared.shard_ordinal
                     OR route_table.table_address <> shared.shared_table_address
                     OR route_table.authority <> shared.shared_authority
                     OR route_table.mutation_epoch < shared.shared_mutation_epoch
                     OR route_table.address_count < shared.finalized_address_count
                     OR shared.finalized_address_count > family.allocation_high_water
                 )
           )
           OR EXISTS (
               WITH expected AS (
                   SELECT (
                              address.ordinal / family.allocation_high_water
                          )::INTEGER AS shard_ordinal,
                          count(*)::INTEGER AS finalized_address_count,
                          loyal_yield.hash_length_prefixed_text(
                              array_agg(address.address ORDER BY address.ordinal)
                          ) AS finalized_address_hash
                   FROM loyal_yield.lookup_table_manifest_addresses address
                   JOIN loyal_yield.lookup_table_families family
                     ON family.id = manifest.family_id
                   WHERE address.manifest_id = probe.shared_manifest_id
                     AND address.semantic_class = 'shared_market'
                   GROUP BY (
                       address.ordinal / family.allocation_high_water
                   )::INTEGER
               ), observed AS (
                   SELECT shared.shard_ordinal,
                          shared.finalized_address_count,
                          shared.finalized_address_hash
                   FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
                   WHERE shared.probe_run_id = probe.id
               )
               SELECT 1
               FROM expected
               FULL JOIN observed
                 USING (shard_ordinal)
               WHERE expected.finalized_address_count
                         IS DISTINCT FROM observed.finalized_address_count
                  OR expected.finalized_address_hash
                         IS DISTINCT FROM observed.finalized_address_hash
           )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_precutover_probe_bundles != 0 {
        return Err(format!(
            "invalid immutable pre-cutover shared bundle(s): {invalid_precutover_probe_bundles}"
        )
        .into());
    }

    let invalid_legacy_imports: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.route_lookup_tables route_table
        WHERE (
            route_table.family_id IS NOT NULL
            AND (route_table.legacy_kind IS NOT NULL OR route_table.legacy_import_run_id IS NOT NULL)
        ) OR (
            route_table.legacy_import_run_id IS NOT NULL
            AND NOT EXISTS (
                SELECT 1
                FROM loyal_yield.lookup_table_legacy_import_evidence evidence
                JOIN loyal_yield.lookup_table_legacy_import_runs import_run
                  ON import_run.id = evidence.import_run_id
                WHERE evidence.import_run_id = route_table.legacy_import_run_id
                  AND evidence.route_lookup_table_id = route_table.id
                  AND evidence.table_address = route_table.table_address
                  AND evidence.scope = route_table.scope
                  AND evidence.legacy_kind = route_table.legacy_kind
                  AND evidence.expected_authority = route_table.authority
                  AND evidence.address_count = route_table.address_count
                  AND evidence.address_hash = route_table.address_hash
                  AND evidence.addresses = route_table.addresses
                  AND evidence.observed_last_extended_slot = route_table.last_extended_slot
                  AND evidence.observed_last_extended_start_index = route_table.last_extended_start_index
                  AND evidence.verified_slot = route_table.last_verified_slot
                  AND evidence.verified_at = route_table.last_verified_at
                  AND import_run.cluster = route_table.cluster
            )
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    let invalid_legacy_import_runs: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_legacy_import_runs import_run
        WHERE import_run.expected_table_count <> (
            SELECT count(*)
            FROM loyal_yield.lookup_table_legacy_import_evidence evidence
            WHERE evidence.import_run_id = import_run.id
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_legacy_imports != 0 || invalid_legacy_import_runs != 0 {
        return Err(format!(
            "legacy lookup-table import invariant failed for {invalid_legacy_imports} table(s) and {invalid_legacy_import_runs} run(s)"
        )
        .into());
    }

    let invalid_family_capacity: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM loyal_yield.lookup_table_families
            WHERE hard_capacity NOT BETWEEN 1 AND 256
               OR largest_atomic_expansion <= 0
               OR safety_margin <= 0
               OR largest_atomic_expansion + safety_margin >= hard_capacity
               OR allocation_high_water
                    <> hard_capacity - largest_atomic_expansion - safety_margin
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_family_capacity {
        return Err("lookup-table family durable capacity formula invariant failed".into());
    }
    let nondeterministic_active_family: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1 FROM loyal_yield.lookup_table_families
            WHERE desired_state = 'active'
            GROUP BY cluster, kind HAVING count(*) > 1
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if nondeterministic_active_family {
        return Err("lookup-table family active kind is not deterministic".into());
    }

    let invalid_physical_capacity: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM loyal_yield.route_lookup_tables
            WHERE address_count > 256
               OR COALESCE(usable_address_count, 0) > address_count
               OR COALESCE(reserved_address_count, 0)
                    > COALESCE(allocation_high_water, 256)
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_physical_capacity {
        return Err("physical lookup-table capacity invariant failed".into());
    }

    let inconsistent_reservations: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.route_lookup_tables route_table
        LEFT JOIN (
            SELECT route_lookup_table_id,
                   sum(reserved_capacity)::INTEGER AS expected_reserved
            FROM (
                SELECT route_lookup_table_id, vault_id, family_id, binding_ordinal,
                       max(reserved_capacity) AS reserved_capacity
                FROM loyal_yield.lookup_table_vault_bindings
                WHERE lifecycle_state IN (
                    'preparing', 'warming', 'active', 'standby', 'retiring'
                )
                GROUP BY route_lookup_table_id, vault_id, family_id, binding_ordinal
            ) live_heads
            GROUP BY route_lookup_table_id
        ) binding_totals
          ON binding_totals.route_lookup_table_id = route_table.id
        WHERE route_table.family_id IS NOT NULL
          AND route_table.reserved_address_count
              <> COALESCE(binding_totals.expected_reserved, 0)
        "#,
    )
    .fetch_one(pool)
    .await?;
    if inconsistent_reservations != 0 {
        return Err(format!(
            "reservation accounting mismatch on {inconsistent_reservations} table(s)"
        )
        .into());
    }

    let invalid_bindings: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_vault_bindings binding
        JOIN loyal_yield.route_lookup_tables route_table
          ON route_table.id = binding.route_lookup_table_id
        JOIN loyal_yield.lookup_table_manifests manifest
          ON manifest.id = binding.manifest_id
        WHERE route_table.family_id <> binding.family_id
           OR manifest.family_id <> binding.family_id
           OR manifest.vault_id <> binding.vault_id
           OR manifest.subject_kind <> 'vault'
           OR (
               binding.allocation_mode = 'packed_shard'
               AND route_table.allocation_kind <> 'vault_shard'
           )
           OR (
               binding.allocation_mode = 'dedicated'
               AND route_table.allocation_kind <> 'dedicated_vault'
           )
           OR (
               binding.lifecycle_state IN ('preparing', 'warming')
               AND NOT EXISTS (
                   SELECT 1
                   FROM loyal_yield.lookup_table_vault_desired_heads desired
                   WHERE desired.family_id = binding.family_id
                     AND desired.vault_id = binding.vault_id
                     AND desired.binding_ordinal = binding.binding_ordinal
                     AND desired.manifest_id = binding.manifest_id
                     AND desired.desired_revision = binding.desired_head_revision
               )
           )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_bindings != 0 {
        return Err(
            format!("family/manifest mismatch on {invalid_bindings} vault binding(s)").into(),
        );
    }
    let invalid_desired_heads: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_vault_desired_heads desired
        JOIN loyal_yield.lookup_table_manifests manifest ON manifest.id = desired.manifest_id
        WHERE manifest.family_id <> desired.family_id
           OR manifest.vault_id <> desired.vault_id
           OR manifest.subject_kind <> 'vault'
           OR manifest.sealed_at IS NULL
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_desired_heads != 0 {
        return Err(
            format!("invalid durable desired vault head(s): {invalid_desired_heads}").into(),
        );
    }

    let invalid_membership: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.route_lookup_tables route_table
        WHERE route_table.family_id IS NOT NULL
          AND route_table.last_verified_at IS NOT NULL
          AND route_table.address_count <> (
              SELECT count(*)
              FROM loyal_yield.lookup_table_addresses address
              WHERE address.route_lookup_table_id = route_table.id
          )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_membership != 0 {
        return Err(
            format!("confirmed membership mismatch on {invalid_membership} table(s)").into(),
        );
    }

    let invalid_operations: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_operations
        WHERE operation_kind IN ('create', 'extend', 'rollover', 'deactivate', 'close')
          AND operation_state IN (
              'signed', 'submitted', 'confirmed', 'finalized', 'reconciled', 'complete'
          )
          AND (
              transaction_signature IS NULL
              OR message_hash IS NULL
              OR recent_blockhash IS NULL
              OR last_valid_block_height IS NULL
          )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_operations != 0 {
        return Err(format!(
            "signed operation metadata missing on {invalid_operations} operation(s)"
        )
        .into());
    }

    let invalid_usage_leases: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_usage_leases usage_lease
        LEFT JOIN loyal_yield.lookup_table_vault_bindings binding
          ON binding.id = usage_lease.binding_id
        WHERE usage_lease.binding_id IS NOT NULL
          AND (
              binding.route_lookup_table_id <> usage_lease.route_lookup_table_id
              OR binding.vault_id IS DISTINCT FROM usage_lease.vault_id
          )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_usage_leases != 0 {
        return Err(
            format!("usage lease binding mismatch on {invalid_usage_leases} lease(s)").into(),
        );
    }

    let invalid_accounting: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_operations
        WHERE (
            operation_kind IN ('create', 'extend', 'rollover', 'deactivate', 'close')
            AND operation_state IN ('confirmed', 'finalized', 'reconciled', 'complete')
            AND actual_fee_lamports IS NULL
        ) OR (
            operation_kind IN ('create', 'extend', 'rollover')
            AND operation_state IN ('reconciled', 'complete')
            AND actual_rent_lamports IS NULL
        ) OR (
            operation_kind = 'close'
            AND operation_state IN ('reconciled', 'complete')
            AND reclaimed_rent_lamports IS NULL
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_accounting != 0 {
        return Err(
            format!("lamport accounting missing on {invalid_accounting} operation(s)").into(),
        );
    }

    let invalid_provisioning_requests: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_provisioning_requests request
        WHERE (shared_manifest_id IS NULL AND NULLIF(desired_shared_hash, '') IS NULL)
           OR (vault_manifest_id IS NULL AND NULLIF(desired_vault_hash, '') IS NULL)
           OR (
               request_status = 'planning'
               AND (lease_owner IS NULL OR lease_expires_at IS NULL OR sealed_at IS NULL)
           )
           OR (request_status = 'satisfied' AND satisfied_at IS NULL)
           OR (
               request.sealed_at IS NOT NULL
               AND request.desired_shared_address_count <> (
                   SELECT count(*)
                   FROM loyal_yield.lookup_table_provisioning_request_addresses address
                   WHERE address.request_id = request.id
                     AND address.semantic_class = 'shared_market'
               )
           )
           OR (
               request.sealed_at IS NOT NULL
               AND request.desired_vault_address_count <> (
                   SELECT count(*)
                   FROM loyal_yield.lookup_table_provisioning_request_addresses address
                   WHERE address.request_id = request.id
                     AND address.semantic_class = 'vault'
               )
           )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if invalid_provisioning_requests != 0 {
        return Err(format!(
            "invalid provisioning lifecycle on {invalid_provisioning_requests} request(s)"
        )
        .into());
    }

    let family_count: i64 =
        sqlx::query_scalar("SELECT count(*) FROM loyal_yield.lookup_table_families")
            .fetch_one(pool)
            .await?;
    let physical_table_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.route_lookup_tables WHERE family_id IS NOT NULL",
    )
    .fetch_one(pool)
    .await?;
    let verified_legacy_table_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.route_lookup_tables WHERE family_id IS NULL AND legacy_import_run_id IS NOT NULL",
    )
    .fetch_one(pool)
    .await?;
    let legacy_import_run_count: i64 =
        sqlx::query_scalar("SELECT count(*) FROM loyal_yield.lookup_table_legacy_import_runs")
            .fetch_one(pool)
            .await?;
    let shared_catalog_head_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_shared_market_catalog_heads",
    )
    .fetch_one(pool)
    .await?;
    let precutover_probe_shared_table_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_precutover_probe_shared_tables",
    )
    .fetch_one(pool)
    .await?;
    let manifest_count: i64 =
        sqlx::query_scalar("SELECT count(*) FROM loyal_yield.lookup_table_manifests")
            .fetch_one(pool)
            .await?;
    let binding_count: i64 =
        sqlx::query_scalar("SELECT count(*) FROM loyal_yield.lookup_table_vault_bindings")
            .fetch_one(pool)
            .await?;
    let desired_vault_head_count: i64 =
        sqlx::query_scalar("SELECT count(*) FROM loyal_yield.lookup_table_vault_desired_heads")
            .fetch_one(pool)
            .await?;
    let active_usage_lease_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_usage_leases
        WHERE released_at IS NULL AND expires_at > now()
        "#,
    )
    .fetch_one(pool)
    .await?;
    let pending_provisioning_request_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_provisioning_requests
        WHERE request_status IN ('requested', 'planning', 'queued', 'failed')
        "#,
    )
    .fetch_one(pool)
    .await?;
    let pending_operation_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_operations
        WHERE operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
        "#,
    )
    .fetch_one(pool)
    .await?;
    let lamports: serde_json::Value = sqlx::query_scalar(
        r#"
        SELECT jsonb_build_object(
            'estimatedFees', COALESCE(sum(estimated_fee_lamports), 0),
            'estimatedRent', COALESCE(sum(estimated_rent_lamports), 0),
            'actualFees', COALESCE(sum(actual_fee_lamports), 0),
            'actualRent', COALESCE(sum(actual_rent_lamports), 0),
            'reclaimedRent', COALESCE(sum(reclaimed_rent_lamports), 0)
        )
        FROM loyal_yield.lookup_table_operations
        "#,
    )
    .fetch_one(pool)
    .await?;

    println!(
        "{}",
        serde_json::json!({
            "status": "reusable_alt_schema_ready",
            "families": family_count,
            "physicalTables": physical_table_count,
            "verifiedLegacyTables": verified_legacy_table_count,
            "legacyImportRuns": legacy_import_run_count,
            "sharedMarketCatalogHeads": shared_catalog_head_count,
            "precutoverProbeSharedTables": precutover_probe_shared_table_count,
            "manifests": manifest_count,
            "bindings": binding_count,
            "desiredVaultHeads": desired_vault_head_count,
            "activeUsageLeases": active_usage_lease_count,
            "pendingProvisioningRequests": pending_provisioning_request_count,
            "pendingOperations": pending_operation_count,
            "lamports": lamports,
        })
    );
    Ok(())
}

fn checksum(sql: &str) -> String {
    let digest = Sha256::digest(sql.as_bytes());
    let mut encoded = String::with_capacity(digest.len() * 2);
    for byte in digest {
        encoded.push_str(&format!("{byte:02x}"));
    }
    encoded
}

impl Migration {
    fn checksum(&self) -> String {
        self.expected_checksum
            .map(ToOwned::to_owned)
            .unwrap_or_else(|| checksum(self.sql))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn migrations_are_strictly_ordered_and_alt_versions_follow_realtime() {
        assert!(MIGRATIONS
            .windows(2)
            .all(|pair| pair[0].version < pair[1].version));
        for (version, name) in [
            (18, "earn_activity_realtime"),
            (19, "legacy_lookup_table_imports"),
            (20, "demand_driven_shared_market_catalog"),
            (21, "reusable_alt_production_controls"),
            (22, "shared_market_alt_bundles"),
        ] {
            assert_eq!(
                MIGRATIONS
                    .iter()
                    .find(|migration| migration.version == version)
                    .map(|migration| migration.name),
                Some(name),
            );
        }
    }

    #[test]
    fn non_legacy_earn_migrations_execute_the_original_bytes() {
        let migration = MIGRATIONS
            .iter()
            .find(|migration| migration.version == 17)
            .expect("migration 17 exists");

        assert!(matches!(
            migration_execution_sql(migration),
            Cow::Borrowed(sql) if std::ptr::eq(sql, migration.sql)
        ));
    }

    #[test]
    fn blank_database_compatibility_keeps_optional_relations_nullable() {
        let migration = MIGRATIONS
            .iter()
            .find(|migration| migration.version == 13)
            .expect("migration 13 exists");
        let execution_sql = migration_execution_sql(migration);

        for relation in [
            "loyal_yield.user_yield_positions",
            "loyal_yield.user_yield_position_holding_events",
            "loyal_yield.earn_deposit_onboarding_attempts",
        ] {
            assert!(!execution_sql.contains(&format!("'{relation}'::regclass")));
            assert!(execution_sql.contains(&format!("to_regclass('{relation}')")));
        }
    }

    #[test]
    fn blank_database_compatibility_does_not_change_the_recorded_checksum() {
        let migration = MIGRATIONS
            .iter()
            .find(|migration| migration.version == 13)
            .expect("migration 13 exists");
        let execution_sql = migration_execution_sql(migration);

        assert_ne!(execution_sql.as_ref(), migration.sql);
        assert_eq!(migration.checksum(), checksum(migration.sql));
        assert_ne!(migration.checksum(), checksum(execution_sql.as_ref()));
    }

    #[test]
    fn earn_activity_migration_has_scoped_deduped_wakeups() {
        let migration = MIGRATIONS
            .iter()
            .find(|migration| migration.version == 18)
            .expect("migration 18 exists");

        for required in [
            "earn.autodeposit.configuration.changed",
            "earn.rebalance.confirmed",
            "OLD.lifecycle_status IS DISTINCT FROM NEW.lifecycle_status",
            "OLD.status::text = 'confirmed'",
            "OLD.post_snapshot_id IS NOT NULL",
            "p_payload => '{}'::jsonb",
            "balance_sweep_targets_configuration_realtime_event",
            "rebalance_decisions_confirmation_realtime_event",
        ] {
            assert!(
                migration.sql.contains(required),
                "migration 18 is missing {required}"
            );
        }
    }

    #[test]
    fn reusable_alt_alert_migration_has_durable_exact_condition_outbox_contract() {
        let migration = MIGRATIONS
            .iter()
            .find(|migration| migration.version == 21)
            .expect("migration 21 exists");

        for required in [
            "lookup_table_alert_rules",
            "enabled BOOLEAN NOT NULL DEFAULT TRUE",
            "rule_version BIGINT NOT NULL DEFAULT 1",
            "lookup_table_alert_rules_enabled_idx",
            "lookup_table_alert_rules_guard",
            "lookup_table_alert_incidents",
            "lookup_table_alert_deliveries",
            "lookup_table_alert_incidents_identity_unique",
            "lookup_table_alert_incidents_id_condition_unique",
            "lookup_table_alert_incidents_rule_fkey",
            "lookup_table_alert_deliveries_idempotency_key_unique",
            "lookup_table_alert_deliveries_incident_revision_unique",
            "lookup_table_alert_deliveries_rule_fkey",
            "lookup_table_alert_deliveries_incident_fkey",
            "'pending', 'leased', 'retry_wait', 'delivered', 'dead_letter'",
            "'webhook', 'render_failure'",
        ] {
            assert!(
                migration.sql.contains(required),
                "migration 21 is missing {required}"
            );
        }
        for condition in LookupTableAlertCondition::ALL {
            assert!(
                migration.sql.contains(&format!("'{}'", condition.as_str())),
                "migration 21 is missing {}",
                condition.as_str()
            );
        }
    }

    #[test]
    fn reusable_alt_production_control_migration_has_pause_and_immutable_probe_contract() {
        let migration = MIGRATIONS
            .iter()
            .find(|migration| migration.version == 21)
            .expect("migration 21 exists");

        for required in [
            "lookup_table_provisioner_controls",
            "control_epoch BIGINT NOT NULL DEFAULT 0",
            "lookup_table_precutover_probe_runs",
            "shared_table_address TEXT NOT NULL",
            "shared_authority TEXT NOT NULL",
            "shared_mutation_epoch BIGINT NOT NULL",
            "provisioner_control_epoch BIGINT NOT NULL",
            "lookup_table_provisioner_broadcast_permits",
            "lookup_table_provisioner_broadcast_permits_active_idx",
            "lookup_table_provisioner_broadcast_permits_cluster_active_idx",
            "No database transaction is held while",
            "finalized_last_extended_slot BIGINT NOT NULL",
            "lookup_table_precutover_probe_pass_check",
            "drift_provisioning_request_count = 0",
            "duplicate_request_attempt_count = 2",
            "distinct_request_count = 1",
            "decision_count = 0",
            "binding_count = 0",
            "operation_count = 0",
            "rollback_residue_count = 0",
            "NOT signer_loaded",
            "NOT transactions_sent",
            "lookup_table_precutover_probe_runs_immutable",
            "lookup_table_legacy_cleanup_attempts",
            "lookup_table_legacy_cleanup_attempt_active_unique",
            "lookup_table_legacy_cleanup_attempt_recovery_idx",
            "transaction_signature TEXT UNIQUE",
            "actual_reclaimed_lamports = expected_reclaimed_lamports",
            "lookup_table_legacy_cleanup_budget_reservations",
            "lookup_table_legacy_cleanup_budget_attempt_unique",
            "lookup_table_legacy_cleanup_budget_active_idx",
            "lookup_table_legacy_cleanup_budget_reservations_immutable",
            "lookup_table_legacy_cleanup_attempt_budget_guard",
            "legacy cleanup signing requires an exact durable cluster budget reservation",
        ] {
            assert!(
                migration.sql.contains(required),
                "migration 21 is missing {required}"
            );
        }
    }
}
