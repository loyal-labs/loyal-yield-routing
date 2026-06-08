use std::{env, error::Error, str::FromStr};

use sha2::{Digest, Sha256};
use sqlx::{
    postgres::{PgConnectOptions, PgPoolOptions},
    PgPool,
};

const MIGRATIONS: &[Migration] = &[
    Migration {
        version: 1,
        name: "kamino_timescale_v1",
        sql: include_str!("../migrations/0001_kamino_timescale_v1.sql"),
    },
    Migration {
        version: 2,
        name: "loyal_balance_sweep_ata_observations",
        sql: include_str!("../migrations/0002_loyal_balance_sweep_ata_observations.sql"),
    },
];

const LEDGER_SCHEMA: &str = "loyal";
const LEDGER_TABLE: &str = "timescale_schema_migrations";

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
    let database_url = env::var("TIMESCALEDB_URL")
        .map_err(|_| "TIMESCALEDB_URL must be set for Loyal Timescale migrations")?;
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
        validate_loyal_ata_schema(&pool).await?;
        println!("loyal_timescale migrations are up to date");
        return Ok(());
    }

    if matches!(mode, Mode::Check) {
        return Err(format!("{} loyal_timescale migration(s) pending", pending.len()).into());
    }

    for migration in pending {
        println!(
            "applying migration {} {}",
            migration.version, migration.name
        );
        sqlx::raw_sql(migration.sql).execute(&pool).await?;
        record_applied(&pool, migration).await?;
    }

    validate_loyal_ata_schema(&pool).await?;
    println!("loyal_timescale migrations are up to date");
    Ok(())
}

fn parse_mode() -> Result<Mode, Box<dyn Error>> {
    let mut mode = Mode::Apply;
    for arg in env::args().skip(1) {
        match arg.as_str() {
            "--check" => mode = Mode::Check,
            "--apply" => mode = Mode::Apply,
            "--help" | "-h" => {
                println!(
                    "Usage: loyal-timescale-migrations [--apply|--check]\n\nReads TIMESCALEDB_URL from the environment."
                );
                std::process::exit(0);
            }
            _ => return Err(format!("unknown argument: {arg}").into()),
        }
    }
    Ok(mode)
}

async fn connect(database_url: &str) -> Result<PgPool, Box<dyn Error>> {
    let options = PgConnectOptions::from_str(database_url)?.statement_cache_capacity(0);
    Ok(PgPoolOptions::new()
        .max_connections(1)
        .connect_with(options)
        .await?)
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

async fn validate_loyal_ata_schema(pool: &PgPool) -> Result<(), Box<dyn Error>> {
    for (schema, relation, kind) in [
        ("loyal", "balance_sweep_wallet_ata_observations", "r"),
        ("loyal", "balance_sweep_wallet_ata_observation_dedupe", "r"),
        ("loyal", "latest_balance_sweep_wallet_ata_observations", "v"),
    ] {
        let exists: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM pg_class c
                JOIN pg_namespace n ON n.oid = c.relnamespace
                WHERE n.nspname = $1
                  AND c.relname = $2
                  AND c.relkind = $3::"char"
            )
            "#,
        )
        .bind(schema)
        .bind(relation)
        .bind(kind)
        .fetch_one(pool)
        .await?;
        if !exists {
            return Err(format!("missing Timescale relation {schema}.{relation}").into());
        }
    }

    let has_raw_account_data: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM information_schema.columns
            WHERE table_schema = 'loyal'
              AND table_name = 'balance_sweep_wallet_ata_observations'
              AND column_name = 'raw_account_data_base64'
        )
        "#,
    )
    .fetch_one(pool)
    .await?;
    if !has_raw_account_data {
        return Err(
            "loyal.balance_sweep_wallet_ata_observations is missing raw_account_data_base64".into(),
        );
    }

    let dedupe_columns: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM information_schema.columns
        WHERE table_schema = 'loyal'
          AND table_name = 'balance_sweep_wallet_ata_observation_dedupe'
          AND column_name = ANY($1)
        "#,
    )
    .bind(&[
        "source_commitment",
        "wallet_usdc_ata",
        "slot",
        "account_data_hash",
    ])
    .fetch_one(pool)
    .await?;
    if dedupe_columns != 4 {
        return Err("loyal ATA observation dedupe table is missing key columns".into());
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
