pub mod apy;
mod catalog;

pub use apy::{
    diff_snapshot, snapshot_from_account, snapshot_from_account_at, DiffSummaryItem, ReserveDiff,
    ReserveSnapshot,
};
pub use catalog::{
    decode_kamino_reserve_account, validate_supported_reserve, KaminoReserveCatalogAccount,
    SharedMarketCatalogError,
};

use serde::Serialize;
use solana_sdk::pubkey::Pubkey;

#[derive(Debug, Clone, Serialize)]
pub struct ReserveTarget {
    pub reserve: Pubkey,
    pub market: Option<Pubkey>,
    pub market_name: Option<String>,
    pub symbol: Option<String>,
    pub liquidity_mint: Option<Pubkey>,
    pub api_supply_apy: Option<f64>,
    pub api_borrow_apy: Option<f64>,
    pub api_total_supply_usd: Option<f64>,
    pub api_total_borrow_usd: Option<f64>,
}

#[derive(Debug, Clone)]
pub struct SupportedReserveRecord {
    pub reserve: Pubkey,
    pub market: Pubkey,
    pub market_name: Option<String>,
    pub symbol: Option<String>,
    pub liquidity_mint: Pubkey,
    pub risk_baskets: Vec<String>,
}
