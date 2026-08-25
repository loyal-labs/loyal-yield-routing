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
    Migration {
        version: 3,
        name: "balance_sweep_ata_txn_signature",
        sql: include_str!("../migrations/0003_balance_sweep_ata_txn_signature.sql"),
    },
    Migration {
        version: 4,
        name: "split_balance_sweep_ata_streams",
        sql: include_str!("../migrations/0004_split_balance_sweep_ata_streams.sql"),
    },
    Migration {
        version: 5,
        name: "kamino_confirmed_state_verification",
        sql: include_str!("../migrations/0005_kamino_confirmed_state_verification.sql"),
    },
    Migration {
        version: 6,
        name: "kamino_verification_slot_tolerance",
        sql: include_str!("../migrations/0006_kamino_verification_slot_tolerance.sql"),
    },
    Migration {
        version: 7,
        name: "kamino_rwa_decision_observations",
        sql: include_str!("../migrations/0007_kamino_rwa_decision_observations.sql"),
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
        validate_kamino_market_verification_schema(&pool).await?;
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

    validate_kamino_market_verification_schema(&pool).await?;
    validate_loyal_ata_schema(&pool).await?;
    println!("loyal_timescale migrations are up to date");
    Ok(())
}

async fn validate_kamino_market_verification_schema(pool: &PgPool) -> Result<(), Box<dyn Error>> {
    for (relation, kind) in [
        ("reserve_current_states", "r"),
        ("reserve_confirmed_observation_floors", "r"),
        ("reserve_confirmed_observation_id_seq", "S"),
        ("reserve_confirmed_verifications", "r"),
        ("latest_verified_reserve_updates", "v"),
    ] {
        let exists: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM pg_class c
                JOIN pg_namespace n ON n.oid = c.relnamespace
                WHERE n.nspname = 'kamino'
                  AND c.relname = $1
                  AND c.relkind = $2::"char"
            )
            "#,
        )
        .bind(relation)
        .bind(kind)
        .fetch_one(pool)
        .await?;
        if !exists {
            return Err(format!("missing Timescale relation kamino.{relation}").into());
        }
    }

    // The monitor's admission and eviction SQL and the verified-updates view
    // all read the tolerance from this function, so a missing or renamed
    // function silently changes eligibility rather than failing loudly.
    let tolerance_slots: i64 = sqlx::query_scalar(
        "SELECT kamino.confirmed_verification_slot_tolerance()",
    )
    .fetch_one(pool)
    .await
    .map_err(|error| {
        format!("missing Timescale function kamino.confirmed_verification_slot_tolerance: {error}")
    })?;
    if tolerance_slots < 0 {
        return Err("Kamino confirmed verification slot tolerance must not be negative".into());
    }

    let verification_columns: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM information_schema.columns
        WHERE table_schema = 'kamino'
          AND table_name = 'reserve_confirmed_verifications'
          AND column_name = ANY($1)
        "#,
    )
    .bind([
        "reserve",
        "state_event_id",
        "account_data_hash",
        "verified_slot",
        "verified_at",
        "commitment",
        "verification_source",
    ])
    .fetch_one(pool)
    .await?;
    if verification_columns != 7 {
        return Err("Kamino confirmed verification table is missing required columns".into());
    }

    let current_state_columns: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM information_schema.columns
        WHERE table_schema = 'kamino'
          AND table_name = 'reserve_current_states'
          AND column_name = ANY($1)
        "#,
    )
    .bind([
        "reserve",
        "state_event_id",
        "account_data_hash",
        "state_slot",
        "state_observed_at",
        "state_source",
    ])
    .fetch_one(pool)
    .await?;
    if current_state_columns != 6 {
        return Err("Kamino current reserve state table is missing required columns".into());
    }

    let observation_floor_columns: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM information_schema.columns
        WHERE table_schema = 'kamino'
          AND table_name = 'reserve_confirmed_observation_floors'
          AND column_name = ANY($1)
        "#,
    )
    .bind([
        "reserve",
        "floor_slot",
        "observation_id",
        "account_data_hash",
        "state_valid",
        "source",
        "source_rank",
        "observed_at",
    ])
    .fetch_one(pool)
    .await?;
    if observation_floor_columns != 8 {
        return Err("Kamino confirmed observation floor table is missing required columns".into());
    }

    let verified_market_columns: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM information_schema.columns
        WHERE table_schema = 'kamino'
          AND table_name = 'latest_verified_reserve_updates'
          AND column_name = ANY($1)
        "#,
    )
    .bind([
        "reserve_last_update_slot",
        "reserve_price_status",
        "market_price_last_updated_ts",
        "total_supply_amount",
    ])
    .fetch_one(pool)
    .await?;
    if verified_market_columns != 4 {
        return Err("Kamino latest verified reserve view is missing economic state columns".into());
    }

    let rwa_decision_columns: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM information_schema.columns
        WHERE table_schema = 'kamino'
          AND table_name = 'latest_verified_reserve_updates'
          AND column_name = ANY($1)
        "#,
    )
    .bind([
        "reserve_status",
        "emergency_mode",
        "loan_to_value_pct",
        "liquidation_threshold_pct",
        "borrow_factor_pct",
        "deposit_limit",
        "borrow_limit",
        "utilization_limit_block_borrowing_above_pct",
        "disable_usage_as_coll_outside_emode",
        "borrow_limit_outside_elevation_group",
        "borrowed_amount_outside_elevation_group",
        "origination_fee_sf",
        "flash_loan_fee_sf",
        "borrow_rate_curve",
        "deposit_withdrawal_cap",
        "debt_withdrawal_cap",
    ])
    .fetch_one(pool)
    .await?;
    if rwa_decision_columns != 16 {
        return Err("Kamino latest verified reserve view is missing RWA decision columns".into());
    }
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
    validate_ata_stream_schema(pool, "loyal").await?;
    validate_ata_stream_schema(pool, "loyal_prod").await?;
    validate_ata_stream_schema(pool, "loyal_staging").await?;
    Ok(())
}

async fn validate_ata_stream_schema(pool: &PgPool, schema: &str) -> Result<(), Box<dyn Error>> {
    for (schema, relation, kind) in [
        (schema, "balance_sweep_wallet_ata_observations", "r"),
        (schema, "balance_sweep_wallet_ata_observation_dedupe", "r"),
        (schema, "latest_balance_sweep_wallet_ata_observations", "v"),
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

    let observation_columns: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM information_schema.columns
        WHERE table_schema = $1
          AND table_name = 'balance_sweep_wallet_ata_observations'
          AND column_name = ANY($2)
        "#,
    )
    .bind(schema)
    .bind([
        "event_id",
        "cluster",
        "target_id",
        "wallet",
        "wallet_usdc_ata",
        "vault_pubkey",
        "vault_usdc_ata",
        "amount_raw",
        "owner",
        "mint",
        "slot",
        "observed_at",
        "source",
        "source_commitment",
        "account_data_hash",
        "raw_account_data_base64",
        "txn_signature",
        "raw_evidence",
        "received_at",
        "inserted_at",
    ])
    .fetch_one(pool)
    .await?;
    if observation_columns != 20 {
        return Err(format!("{schema} ATA observations table is missing required columns").into());
    }

    let dedupe_columns: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM information_schema.columns
        WHERE table_schema = $1
          AND table_name = 'balance_sweep_wallet_ata_observation_dedupe'
          AND column_name = ANY($2)
        "#,
    )
    .bind(schema)
    .bind([
        "source_commitment",
        "wallet_usdc_ata",
        "slot",
        "account_data_hash",
    ])
    .fetch_one(pool)
    .await?;
    if dedupe_columns != 4 {
        return Err(format!("{schema} ATA observation dedupe table is missing key columns").into());
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
