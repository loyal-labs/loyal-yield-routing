use std::{env, error::Error, str::FromStr};

use sha2::{Digest, Sha256};
use sqlx::{
    postgres::{PgConnectOptions, PgPoolOptions},
    PgPool,
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
];

const LEDGER_SCHEMA: &str = "loyal_yield";
const LEDGER_TABLE: &str = "schema_migrations";

struct Migration {
    version: i64,
    name: &'static str,
    sql: &'static str,
    expected_checksum: Option<&'static str>,
}

enum Mode {
    Apply,
    Check,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let mode = parse_mode()?;
    let database_url = env::var("NEON_DATABASE_URL")
        .map_err(|_| "NEON_DATABASE_URL must be set for Yield Neon migrations")?;
    let pool = connect(&database_url).await?;

    ensure_ledger(&pool).await?;

    let mut pending = Vec::new();
    for migration in MIGRATIONS {
        match applied_checksum(&pool, migration.version).await? {
            Some(applied) if applied == migration.checksum() => {
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
        validate_schema(&pool).await?;
        println!("loyal_yield migrations are up to date");
        return Ok(());
    }

    if matches!(mode, Mode::Check) {
        return Err(format!("{} loyal_yield migration(s) pending", pending.len()).into());
    }

    for migration in pending {
        println!(
            "applying migration {} {}",
            migration.version, migration.name
        );
        sqlx::raw_sql(migration.sql).execute(&pool).await?;
        record_applied(&pool, migration).await?;
    }

    validate_schema(&pool).await?;
    println!("loyal_yield migrations are up to date");
    Ok(())
}

fn parse_mode() -> Result<Mode, Box<dyn Error>> {
    let mut mode = Mode::Apply;
    for arg in env::args().skip(1) {
        match arg.as_str() {
            "--apply" => mode = Mode::Apply,
            "--check" => mode = Mode::Check,
            "--help" | "-h" => {
                println!(
                    "Usage: yield-migrations [--apply|--check]\n\nReads NEON_DATABASE_URL from the environment."
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

async fn applied_checksum(pool: &PgPool, version: i64) -> Result<Option<String>, sqlx::Error> {
    sqlx::query_scalar::<_, String>(&format!(
        "SELECT checksum FROM {LEDGER_SCHEMA}.{LEDGER_TABLE} WHERE version = $1"
    ))
    .bind(version)
    .fetch_optional(pool)
    .await
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
        "vault_idle_token_balances_current",
        "realtime_events",
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
