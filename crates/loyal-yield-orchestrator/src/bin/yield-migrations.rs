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
    },
    Migration {
        version: 2,
        name: "balance_sweep_surplus_lots",
        sql: include_str!("../../migrations/0002_balance_sweep_surplus_lots.sql"),
    },
    Migration {
        version: 3,
        name: "balance_sweep_initial_surplus",
        sql: include_str!("../../migrations/0003_balance_sweep_initial_surplus.sql"),
    },
];

const LEDGER_SCHEMA: &str = "loyal_yield";
const LEDGER_TABLE: &str = "schema_migrations";

struct Migration {
    version: i64,
    name: &'static str,
    sql: &'static str,
}

enum Mode {
    Apply,
    Check,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let mode = parse_mode()?;
    let database_url = env::var("NEON_DATABASE_URL")
        .or_else(|_| env::var("DATABASE_URL"))
        .map_err(|_| "NEON_DATABASE_URL must be set for Yield Neon migrations")?;
    let pool = connect(&database_url).await?;

    ensure_ledger(&pool).await?;

    let mut pending = Vec::new();
    for migration in MIGRATIONS {
        match applied_checksum(&pool, migration.version).await? {
            Some(applied) if applied == checksum(migration.sql) => {
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
    .bind(checksum(migration.sql))
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
        "pending_balance_sweep_surplus_lots",
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
