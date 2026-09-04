use std::io::{self, Read};

use chrono::{DateTime, Utc};
use loyal_yield_orchestrator::fleet_orchestration::observation::{
    build_market_epoch, code_owned_stablecoin_valuations, FleetObservationConfig,
};
use loyal_yield_router::timescale::{
    SupportedReserveCatalogRow, SupportedReserveMarketSnapshot, VerifiedSupportedReserveRow,
};
use serde::Deserialize;

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Input {
    captured_at: DateTime<Utc>,
    enabled_mints: Vec<String>,
    catalog: Vec<CatalogInput>,
    verified_reserves: Vec<VerifiedInput>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct CatalogInput {
    market: String,
    liquidity_mint: String,
    reserve: String,
    market_name: Option<String>,
    symbol: Option<String>,
    risk_baskets: Vec<String>,
    source: String,
    fetched_at: DateTime<Utc>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct VerifiedInput {
    state_event_id: i64,
    account_data_hash: String,
    state_observed_at: DateTime<Utc>,
    state_slot: i64,
    verified_at: DateTime<Utc>,
    verified_slot: i64,
    verification_commitment: String,
    verification_source: String,
    reserve: String,
    market: Option<String>,
    market_name: Option<String>,
    liquidity_mint: String,
    symbol: Option<String>,
    mint_decimals: i32,
    reserve_last_update_slot: i64,
    reserve_last_update_stale: bool,
    reserve_price_status: i16,
    available_amount: f64,
    borrowed_amount: f64,
    total_supply_amount: f64,
    market_price_usd: f64,
    market_price_last_updated_ts: i64,
    utilization: f64,
    borrow_apy: f64,
    supply_apy: f64,
    available_amount_bits: Option<u64>,
    borrowed_amount_bits: Option<u64>,
    total_supply_amount_bits: Option<u64>,
    market_price_usd_bits: Option<u64>,
    utilization_bits: Option<u64>,
    borrow_apy_bits: Option<u64>,
    supply_apy_bits: Option<u64>,
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let mut bytes = Vec::new();
    io::stdin().read_to_end(&mut bytes)?;
    let input: Input = serde_json::from_slice(&bytes)?;
    let mut config = FleetObservationConfig::default();
    config.enabled_mints = input.enabled_mints.clone();
    config.stablecoin_valuations = code_owned_stablecoin_valuations(&input.enabled_mints)?;
    let snapshot = SupportedReserveMarketSnapshot {
        captured_at: input.captured_at,
        catalog: input
            .catalog
            .into_iter()
            .map(|row| SupportedReserveCatalogRow {
                market: row.market,
                liquidity_mint: row.liquidity_mint,
                reserve: row.reserve,
                market_name: row.market_name,
                symbol: row.symbol,
                risk_baskets: row.risk_baskets,
                source: row.source,
                fetched_at: row.fetched_at,
            })
            .collect(),
        verified_reserves: input
            .verified_reserves
            .into_iter()
            .map(|row| VerifiedSupportedReserveRow {
                state_event_id: row.state_event_id,
                account_data_hash: row.account_data_hash,
                state_observed_at: row.state_observed_at,
                state_slot: row.state_slot,
                verified_at: row.verified_at,
                verified_slot: row.verified_slot,
                verification_commitment: row.verification_commitment,
                verification_source: row.verification_source,
                reserve: row.reserve,
                market: row.market,
                market_name: row.market_name,
                liquidity_mint: row.liquidity_mint,
                symbol: row.symbol,
                mint_decimals: row.mint_decimals,
                reserve_last_update_slot: row.reserve_last_update_slot,
                reserve_last_update_stale: row.reserve_last_update_stale,
                reserve_price_status: row.reserve_price_status,
                available_amount: row
                    .available_amount_bits
                    .map(f64::from_bits)
                    .unwrap_or(row.available_amount),
                borrowed_amount: row
                    .borrowed_amount_bits
                    .map(f64::from_bits)
                    .unwrap_or(row.borrowed_amount),
                total_supply_amount: row
                    .total_supply_amount_bits
                    .map(f64::from_bits)
                    .unwrap_or(row.total_supply_amount),
                market_price_usd: row
                    .market_price_usd_bits
                    .map(f64::from_bits)
                    .unwrap_or(row.market_price_usd),
                market_price_last_updated_ts: row.market_price_last_updated_ts,
                utilization: row
                    .utilization_bits
                    .map(f64::from_bits)
                    .unwrap_or(row.utilization),
                borrow_apy: row
                    .borrow_apy_bits
                    .map(f64::from_bits)
                    .unwrap_or(row.borrow_apy),
                supply_apy: row
                    .supply_apy_bits
                    .map(f64::from_bits)
                    .unwrap_or(row.supply_apy),
            })
            .collect(),
    };
    let epoch = build_market_epoch(snapshot, &input.enabled_mints, &config)?
        .durable_optimizer_epoch_evidence();
    serde_json::to_writer(io::stdout().lock(), &epoch)?;
    Ok(())
}
