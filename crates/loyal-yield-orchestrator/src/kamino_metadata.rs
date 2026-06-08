use std::{
    collections::HashMap,
    str::FromStr,
    time::{Duration, Instant},
};

use chrono::{DateTime, Utc};
use klend_interface::state::Reserve;
use loyal_actions::KAMINO_LENDING_PROGRAM_ID;
use loyal_yield_router::timescale::ReserveUpdateRow;
use serde_json::json;
use solana_client::rpc_client::RpcClient;
use solana_sdk::pubkey::Pubkey;
use thiserror::Error;

use crate::{KaminoReserveAccountsConfig, YieldReserveTarget};

const DEFAULT_METADATA_TTL: Duration = Duration::from_secs(60 * 60);

#[derive(Debug, Error)]
pub enum KaminoMetadataError {
    #[error("invalid pubkey in {field}: {value}")]
    InvalidPubkey { field: &'static str, value: String },
    #[error("Timescale row for reserve {reserve} is missing market metadata")]
    MissingMarket { reserve: String },
    #[error("RPC error while loading reserve {reserve}: {error}")]
    Rpc { reserve: String, error: String },
    #[error("failed to decode Kamino reserve {reserve}: {error}")]
    Decode { reserve: String, error: String },
    #[error("supply APY {0} is not finite")]
    InvalidApy(f64),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReserveAccountMetadata {
    pub market: String,
    pub liquidity_mint: String,
    pub reserve_liquidity_supply: String,
    pub reserve_collateral_mint: String,
    pub lending_market_authority: String,
    pub liquidity_token_program: String,
}

#[derive(Debug, Clone)]
struct CachedReserveMetadata {
    metadata: ReserveAccountMetadata,
    cached_at: Instant,
}

#[derive(Debug, Clone)]
pub struct KaminoReserveMetadataResolver {
    ttl: Duration,
    cache: HashMap<String, CachedReserveMetadata>,
}

impl Default for KaminoReserveMetadataResolver {
    fn default() -> Self {
        Self::new(DEFAULT_METADATA_TTL)
    }
}

impl KaminoReserveMetadataResolver {
    pub fn new(ttl: Duration) -> Self {
        Self {
            ttl,
            cache: HashMap::new(),
        }
    }

    pub async fn resolve_reserve_target(
        &mut self,
        row: ReserveUpdateRow,
        rpc: &RpcClient,
    ) -> Result<YieldReserveTarget, KaminoMetadataError> {
        let metadata = self.resolve_account_metadata(&row, rpc).await?;
        reserve_target_from_row_and_metadata(row, metadata)
    }

    async fn resolve_account_metadata(
        &mut self,
        row: &ReserveUpdateRow,
        rpc: &RpcClient,
    ) -> Result<ReserveAccountMetadata, KaminoMetadataError> {
        if let Some(cached) = self.cache.get(&row.reserve) {
            if cached.cached_at.elapsed() <= self.ttl {
                return Ok(cached.metadata.clone());
            }
        }

        let metadata = decode_reserve_account_metadata(row, rpc)?;
        self.cache.insert(
            row.reserve.clone(),
            CachedReserveMetadata {
                metadata: metadata.clone(),
                cached_at: Instant::now(),
            },
        );
        Ok(metadata)
    }
}

pub async fn resolve_reserve_target(
    row: ReserveUpdateRow,
    rpc: &RpcClient,
) -> Result<YieldReserveTarget, KaminoMetadataError> {
    KaminoReserveMetadataResolver::default()
        .resolve_reserve_target(row, rpc)
        .await
}

pub fn reserve_target_from_row_and_metadata(
    row: ReserveUpdateRow,
    metadata: ReserveAccountMetadata,
) -> Result<YieldReserveTarget, KaminoMetadataError> {
    Ok(YieldReserveTarget {
        reserve: row.reserve.clone(),
        market: metadata.market.clone(),
        liquidity_mint: metadata.liquidity_mint.clone(),
        supply_apy_bps: apy_ratio_to_bps(row.supply_apy)?,
        accounts: KaminoReserveAccountsConfig {
            lending_market_authority: metadata.lending_market_authority,
            reserve_liquidity_supply: metadata.reserve_liquidity_supply,
            reserve_collateral_mint: metadata.reserve_collateral_mint,
            liquidity_token_program: Some(metadata.liquidity_token_program),
        },
        metadata: json!({
            "source": "timescale",
            "eventId": row.event_id,
            "observedAt": row.observed_at,
            "slot": row.slot,
            "sourceName": row.source,
            "sourceCommitment": row.source_commitment,
            "marketName": row.market_name,
            "symbol": row.symbol,
            "supplyApy": row.supply_apy,
            "borrowApy": row.borrow_apy,
            "utilization": row.utilization,
            "totalSupplyUsdEstimate": row.total_supply_usd_estimate,
            "totalBorrowUsdEstimate": row.total_borrow_usd_estimate,
            "reserveLastUpdateStale": row.reserve_last_update_stale,
            "diffChanged": row.diff_changed,
            "changedFields": row.changed_fields,
            "diffSummary": row.diff_summary,
        }),
    })
}

pub fn apy_ratio_to_bps(value: f64) -> Result<i64, KaminoMetadataError> {
    if !value.is_finite() {
        return Err(KaminoMetadataError::InvalidApy(value));
    }
    Ok((value * 10_000.0).round() as i64)
}

pub fn row_is_fresh(row: &ReserveUpdateRow, max_age: Duration, now: DateTime<Utc>) -> bool {
    now.signed_duration_since(row.observed_at)
        .to_std()
        .map(|age| age <= max_age)
        .unwrap_or(true)
}

fn decode_reserve_account_metadata(
    row: &ReserveUpdateRow,
    rpc: &RpcClient,
) -> Result<ReserveAccountMetadata, KaminoMetadataError> {
    let reserve_pubkey = parse_pubkey("reserve", &row.reserve)?;
    let account = rpc
        .get_account(&reserve_pubkey)
        .map_err(|error| KaminoMetadataError::Rpc {
            reserve: row.reserve.clone(),
            error: error.to_string(),
        })?;
    let reserve =
        klend_interface::from_account_data::<Reserve>(&account.data).map_err(|error| {
            KaminoMetadataError::Decode {
                reserve: row.reserve.clone(),
                error: error.to_string(),
            }
        })?;

    let market = row
        .market
        .clone()
        .or_else(|| Some(reserve.lending_market.to_string()))
        .ok_or_else(|| KaminoMetadataError::MissingMarket {
            reserve: row.reserve.clone(),
        })?;
    let market_pubkey = parse_pubkey(
        "reserve.lending_market",
        &reserve.lending_market.to_string(),
    )?;
    let lending_market_authority = Pubkey::find_program_address(
        &[b"lma", market_pubkey.as_ref()],
        &KAMINO_LENDING_PROGRAM_ID,
    )
    .0;

    Ok(ReserveAccountMetadata {
        market,
        liquidity_mint: reserve.liquidity.mint_pubkey.to_string(),
        reserve_liquidity_supply: reserve.liquidity.supply_vault.to_string(),
        reserve_collateral_mint: reserve.collateral.mint_pubkey.to_string(),
        lending_market_authority: lending_market_authority.to_string(),
        liquidity_token_program: reserve.liquidity.token_program.to_string(),
    })
}

fn parse_pubkey(field: &'static str, value: &str) -> Result<Pubkey, KaminoMetadataError> {
    Pubkey::from_str(value).map_err(|_| KaminoMetadataError::InvalidPubkey {
        field,
        value: value.to_owned(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;
    use loyal_yield_router::timescale::ReserveUpdateRow;

    #[test]
    fn converts_timescale_apy_ratio_to_bps() {
        assert_eq!(apy_ratio_to_bps(0.0425).unwrap(), 425);
        assert_eq!(apy_ratio_to_bps(0.00014).unwrap(), 1);
    }

    #[test]
    fn builds_target_from_timescale_row_and_cached_metadata() {
        let row = row(0.0525);
        let target = reserve_target_from_row_and_metadata(
            row,
            ReserveAccountMetadata {
                market: "market-a".to_owned(),
                liquidity_mint: "USDC".to_owned(),
                reserve_liquidity_supply: "supply-a".to_owned(),
                reserve_collateral_mint: "collateral-a".to_owned(),
                lending_market_authority: "authority-a".to_owned(),
                liquidity_token_program: spl_token::ID.to_string(),
            },
        )
        .unwrap();

        assert_eq!(target.reserve, "reserve-a");
        assert_eq!(target.market, "market-a");
        assert_eq!(target.liquidity_mint, "USDC");
        assert_eq!(target.supply_apy_bps, 525);
        assert_eq!(target.accounts.reserve_liquidity_supply, "supply-a");
        assert_eq!(target.metadata["eventId"], 9);
    }

    #[test]
    fn freshness_uses_observed_at_without_stale_flag_filtering() {
        let mut row = row(0.01);
        row.reserve_last_update_stale = true;
        assert!(row_is_fresh(
            &row,
            Duration::from_secs(60),
            Utc.with_ymd_and_hms(2026, 6, 8, 0, 0, 30).unwrap()
        ));
        assert!(!row_is_fresh(
            &row,
            Duration::from_secs(10),
            Utc.with_ymd_and_hms(2026, 6, 8, 0, 0, 30).unwrap()
        ));
    }

    fn row(supply_apy: f64) -> ReserveUpdateRow {
        ReserveUpdateRow {
            event_id: 9,
            observed_at: Utc.with_ymd_and_hms(2026, 6, 8, 0, 0, 0).unwrap(),
            slot: 100,
            source: "websocket".to_owned(),
            source_commitment: "confirmed".to_owned(),
            reserve: "reserve-a".to_owned(),
            market: Some("market-a".to_owned()),
            market_name: Some("Main".to_owned()),
            symbol: Some("USDC".to_owned()),
            liquidity_mint: "USDC".to_owned(),
            supply_apy,
            borrow_apy: 0.07,
            utilization: 0.5,
            total_supply_usd_estimate: 1_000_000.0,
            total_borrow_usd_estimate: 500_000.0,
            reserve_last_update_stale: false,
            diff_changed: true,
            changed_fields: vec!["supply_apy".to_owned()],
            diff_summary: "supply_apy".to_owned(),
            record: json!({}),
        }
    }
}
