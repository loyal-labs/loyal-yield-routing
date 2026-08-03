//! Coverage for the confirmed-verification slot tolerance on the batch
//! classifier, which is the path the periodic confirmed refresh actually takes.
//!
//! The SQL fixture in `crates/loyal-timescale-migrations/fixtures` covers the
//! verified-updates view. The view is only half the protocol: a read that the
//! classifier defers never reaches admission at all, so a reserve evicted from
//! the view could still be unable to re-enter even with a correct view.
//!
//! Requires a throwaway PostgreSQL database with the loyal_timescale migrations
//! 0001-0006 applied. Gated on `KAMINO_VERIFICATION_TEST_DATABASE_URL` rather
//! than the monitor's own `TIMESCALEDB_URL` on purpose: this test writes
//! synthetic reserve rows and must never be able to reach a live database by
//! inheriting the monitor's configuration.

use std::{env, time::Duration};

use anyhow::{Context, Result};
use kamino_reserve_monitor::timescale::{
    ConfirmedStateVerification, TimescaleSink, TimescaleSinkConfig,
};
use sqlx::postgres::PgPoolOptions;

const BASE_SLOT: i64 = 1_000;
const HASH_VERIFIED: &str = "HASH_VERIFIED";
const HASH_STREAM: &str = "HASH_STREAM";

fn test_database_url() -> Option<String> {
    env::var("KAMINO_VERIFICATION_TEST_DATABASE_URL")
        .ok()
        .filter(|url| !url.trim().is_empty())
}

/// Seeds one reserve whose HTTP-owned state sits at `BASE_SLOT`, with the
/// LaserStream observation floor placed at `floor_slot` carrying `floor_hash`.
async fn seed(
    pool: &sqlx::PgPool,
    reserve: &str,
    floor_slot: i64,
    floor_hash: Option<&str>,
) -> Result<()> {
    sqlx::query("DELETE FROM kamino.reserve_confirmed_verifications WHERE reserve = $1")
        .bind(reserve)
        .execute(pool)
        .await?;
    sqlx::query("DELETE FROM kamino.reserve_confirmed_observation_floors WHERE reserve = $1")
        .bind(reserve)
        .execute(pool)
        .await?;
    sqlx::query("DELETE FROM kamino.reserve_current_states WHERE reserve = $1")
        .bind(reserve)
        .execute(pool)
        .await?;
    sqlx::query("DELETE FROM kamino.reserve_updates WHERE reserve = $1")
        .bind(reserve)
        .execute(pool)
        .await?;

    sqlx::query(
        r#"
        INSERT INTO kamino.reserve_updates (
          observed_at, slot, kind, source, source_commitment, reserve, market, market_name,
          symbol, liquidity_mint, mint_decimals, reserve_last_update_slot,
          reserve_last_update_stale, reserve_price_status, available_amount, borrowed_amount,
          borrowed_amount_sf, total_supply_amount, market_price_usd,
          market_price_last_updated_ts, cumulative_borrow_rate_bsf, total_supply_usd_estimate,
          total_borrow_usd_estimate, utilization, borrow_apr, supply_apr, borrow_apy, supply_apy,
          protocol_take_rate_pct, host_fixed_interest_rate_bps, diff_changed, changed_fields,
          diff_summary, diff, target, snapshot, record, account_data_hash
        ) VALUES (
          now(), $2, 'state', 'http_snapshot', 'confirmed', $1, 'MKT', 'm', 'USDC', 'MINT', 6,
          $2, false, 0, 1, 1, '1', 2, 1.0, 0, '0', 2, 1, 0.5, 0.05, 0.04, 0.05, 0.04, 0, 0,
          false, '{}', '', '{}', '{}', '{}', '{}', $3
        )
        "#,
    )
    .bind(reserve)
    .bind(BASE_SLOT)
    .bind(HASH_VERIFIED)
    .execute(pool)
    .await?;

    sqlx::query(
        r#"
        INSERT INTO kamino.reserve_current_states (
          reserve, state_event_id, account_data_hash, state_slot, state_observed_at, state_source
        )
        SELECT reserve, event_id, account_data_hash, slot, observed_at, source
        FROM kamino.reserve_updates WHERE reserve = $1
        "#,
    )
    .bind(reserve)
    .execute(pool)
    .await?;

    sqlx::query(
        r#"
        INSERT INTO kamino.reserve_confirmed_observation_floors (
          reserve, floor_slot, account_data_hash, state_valid, source, source_rank, observed_at
        ) VALUES ($1, $2, $3, $4, 'laserstream_grpc', 1, now())
        "#,
    )
    .bind(reserve)
    .bind(floor_slot)
    .bind(floor_hash)
    // A floor with no hash is the malformed-stream marker the monitor writes for
    // a wrong-owner or undecodable reserve account; the schema ties the two
    // together, so deriving state_valid here keeps the fixture honest.
    .bind(floor_hash.is_some())
    .execute(pool)
    .await?;

    Ok(())
}

fn verification(reserve: &str, verified_slot: i64) -> ConfirmedStateVerification {
    ConfirmedStateVerification {
        reserve: reserve.to_string(),
        account_data_hash: HASH_VERIFIED.to_string(),
        verified_slot,
        verified_at: chrono::Utc::now(),
        commitment: "confirmed",
        verification_source: "http_confirmed_refresh",
        state_valid: true,
    }
}

async fn deferred_for(
    reserve: &str,
    floor_slot: i64,
    floor_hash: Option<&str>,
    verified_slot: i64,
) -> Result<bool> {
    let url = test_database_url().expect("checked by caller");
    let pool = PgPoolOptions::new()
        .max_connections(2)
        .acquire_timeout(Duration::from_secs(10))
        .connect(&url)
        .await
        .context("connect test database")?;
    seed(&pool, reserve, floor_slot, floor_hash).await?;

    let sink = TimescaleSink::connect(TimescaleSinkConfig::new(url.clone()))
        .await
        .context("connect TimescaleSink")?;
    let outcome = sink
        .verify_confirmed_states(&[verification(reserve, verified_slot)])
        .await?;
    Ok(outcome.deferred.contains(reserve))
}

#[tokio::test]
#[ignore = "requires KAMINO_VERIFICATION_TEST_DATABASE_URL pointing at a throwaway database with migrations 0001-0006 applied"]
async fn trailing_confirmed_read_within_tolerance_is_admitted() -> Result<()> {
    if test_database_url().is_none() {
        eprintln!("skipping: KAMINO_VERIFICATION_TEST_DATABASE_URL is not set");
        return Ok(());
    }

    // LaserStream moved ten slots ahead with different account data, which is
    // the ordinary outcome of a confirmed HTTP read racing the stream. Deferring
    // this is what prevented an evicted reserve from ever re-entering.
    let deferred = deferred_for(
        "RESERVE_TRAILING_WITHIN_TOLERANCE",
        BASE_SLOT + 10,
        Some(HASH_STREAM),
        BASE_SLOT,
    )
    .await?;
    assert!(
        !deferred,
        "a confirmed read trailing the floor by 10 slots must not be deferred"
    );
    Ok(())
}

#[tokio::test]
#[ignore = "requires KAMINO_VERIFICATION_TEST_DATABASE_URL pointing at a throwaway database with migrations 0001-0006 applied"]
async fn trailing_confirmed_read_past_tolerance_is_deferred() -> Result<()> {
    if test_database_url().is_none() {
        eprintln!("skipping: KAMINO_VERIFICATION_TEST_DATABASE_URL is not set");
        return Ok(());
    }

    // Past the tolerance the read is genuinely stale, so staleness stays bounded
    // rather than merely tolerated.
    let deferred = deferred_for(
        "RESERVE_TRAILING_PAST_TOLERANCE",
        BASE_SLOT + 200,
        Some(HASH_STREAM),
        BASE_SLOT,
    )
    .await?;
    assert!(
        deferred,
        "a confirmed read trailing the floor past the tolerance must be deferred"
    );
    Ok(())
}

#[tokio::test]
#[ignore = "requires KAMINO_VERIFICATION_TEST_DATABASE_URL pointing at a throwaway database with migrations 0001-0006 applied"]
async fn equal_slot_conflicting_hash_stays_fenced() -> Result<()> {
    if test_database_url().is_none() {
        eprintln!("skipping: KAMINO_VERIFICATION_TEST_DATABASE_URL is not set");
        return Ok(());
    }

    // Two observers disagreeing about the same slot is a conflict, not lag. The
    // slot difference is zero and therefore trivially inside the tolerance, so
    // this is exactly the case a subtraction-only guard would wrongly admit.
    let deferred = deferred_for(
        "RESERVE_EQUAL_SLOT_CONFLICT",
        BASE_SLOT,
        Some(HASH_STREAM),
        BASE_SLOT,
    )
    .await?;
    assert!(
        deferred,
        "an equal-slot read conflicting on hash must stay fenced"
    );
    Ok(())
}

#[tokio::test]
#[ignore = "requires KAMINO_VERIFICATION_TEST_DATABASE_URL pointing at a throwaway database with migrations 0001-0006 applied"]
async fn invalid_floor_within_tolerance_stays_fenced() -> Result<()> {
    if test_database_url().is_none() {
        eprintln!("skipping: KAMINO_VERIFICATION_TEST_DATABASE_URL is not set");
        return Ok(());
    }

    // A floor with no hash is what the monitor writes when the stream saw a
    // wrong-owner or undecodable reserve account, and that fences routability
    // immediately by contract. Being inside the slot tolerance must not buy such
    // a reserve another window of visibility.
    let deferred = deferred_for(
        "RESERVE_INVALID_FLOOR_WITHIN_TOLERANCE",
        BASE_SLOT + 10,
        None,
        BASE_SLOT,
    )
    .await?;
    assert!(
        deferred,
        "an invalid floor must fence at any distance, including inside the tolerance"
    );
    Ok(())
}

#[tokio::test]
#[ignore = "requires KAMINO_VERIFICATION_TEST_DATABASE_URL pointing at a throwaway database with migrations 0001-0006 applied"]
async fn invalid_floor_one_slot_ahead_stays_fenced() -> Result<()> {
    if test_database_url().is_none() {
        eprintln!("skipping: KAMINO_VERIFICATION_TEST_DATABASE_URL is not set");
        return Ok(());
    }

    // The tightest possible trailing distance, where a slot-difference guard is
    // most tempted to treat the floor as ordinary drift.
    let deferred = deferred_for(
        "RESERVE_INVALID_FLOOR_ONE_SLOT",
        BASE_SLOT + 1,
        None,
        BASE_SLOT,
    )
    .await?;
    assert!(deferred, "an invalid floor one slot ahead must still fence");
    Ok(())
}
