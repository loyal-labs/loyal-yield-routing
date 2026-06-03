//! TimescaleDB APY rows mapped into same-mint routing inputs.

use loyal_yield_router::timescale::{ReserveUpdateFilter, ReserveUpdateRow, TimescaleRouterClient};
use thiserror::Error;

use crate::SameMintReserveApy;

pub const TIMESCALEDB_URL_ENV: &str = "TIMESCALEDB_URL";
pub const TIMESCALEDB_SCHEMA_ENV: &str = "TIMESCALEDB_SCHEMA";
pub const TIMESCALEDB_NOTIFY_CHANNEL_ENV: &str = "TIMESCALEDB_NOTIFY_CHANNEL";
pub const SAME_MINT_TIMESCALE_RESERVES_ENV: &str = "SAME_MINT_TIMESCALE_RESERVES";
pub const SAME_MINT_TIMESCALE_SYMBOLS_ENV: &str = "SAME_MINT_TIMESCALE_SYMBOLS";
pub const SAME_MINT_TIMESCALE_MARKETS_ENV: &str = "SAME_MINT_TIMESCALE_MARKETS";
pub const SAME_MINT_TIMESCALE_CHANGED_FIELDS_ENV: &str = "SAME_MINT_TIMESCALE_CHANGED_FIELDS";
pub const SAME_MINT_TIMESCALE_MIN_SUPPLY_USD_ENV: &str = "SAME_MINT_TIMESCALE_MIN_SUPPLY_USD";
pub const SAME_MINT_TIMESCALE_INCLUDE_STALE_ENV: &str = "SAME_MINT_TIMESCALE_INCLUDE_STALE";

pub async fn latest_same_mint_apys(
    client: &TimescaleRouterClient,
    filter: ReserveUpdateFilter,
) -> Result<Vec<SameMintReserveApy>, TimescaleSameMintError> {
    let rows = client.latest_reserves(filter).await?;
    same_mint_apys_from_rows(&rows)
}

pub fn same_mint_apys_from_rows(
    rows: &[ReserveUpdateRow],
) -> Result<Vec<SameMintReserveApy>, TimescaleSameMintError> {
    rows.iter()
        .map(|row| {
            Ok(SameMintReserveApy {
                reserve: row.reserve.clone(),
                liquidity_mint: row.liquidity_mint.clone(),
                supply_apy_bps: apy_fraction_to_bps(row.supply_apy, "supply_apy")?,
                borrow_apy_bps: Some(apy_fraction_to_bps(row.borrow_apy, "borrow_apy")?),
            })
        })
        .collect()
}

pub fn apy_fraction_to_bps(value: f64, field: &'static str) -> Result<i64, TimescaleSameMintError> {
    if !value.is_finite() {
        return Err(TimescaleSameMintError::InvalidApy { field, value });
    }
    let bps = (value * 10_000.0).round();
    if bps < i64::MIN as f64 || bps > i64::MAX as f64 {
        return Err(TimescaleSameMintError::InvalidApy { field, value });
    }
    Ok(bps as i64)
}

pub fn comma_list(value: Option<String>) -> Vec<String> {
    value
        .into_iter()
        .flat_map(|value| {
            value
                .split(',')
                .map(str::trim)
                .filter(|item| !item.is_empty())
                .map(ToOwned::to_owned)
                .collect::<Vec<_>>()
        })
        .collect()
}

#[derive(Debug, Error)]
pub enum TimescaleSameMintError {
    #[error("TimescaleDB error: {0}")]
    Sqlx(#[from] sqlx::Error),
    #[error("{field} APY must be a finite fraction, got {value}")]
    InvalidApy { field: &'static str, value: f64 },
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn converts_fractional_apy_to_basis_points() {
        assert_eq!(apy_fraction_to_bps(0.0521, "supply_apy").unwrap(), 521);
        assert_eq!(apy_fraction_to_bps(0.00004, "supply_apy").unwrap(), 0);
        assert_eq!(apy_fraction_to_bps(1.0, "supply_apy").unwrap(), 10_000);
    }

    #[test]
    fn comma_list_trims_empty_items() {
        assert_eq!(
            comma_list(Some(" USDC, PYUSD,, ".to_owned())),
            vec!["USDC".to_owned(), "PYUSD".to_owned()]
        );
    }
}
