//! Pure Kamino reserve decoding — no RPC, no stream client.
//!
//! This crate exists so that consumers which only need to *decode* reserve
//! accounts do not have to depend on a package that also ships a laserstream
//! client. `kamino-historic-data` is the motivating case: depending on
//! `kamino-reserve-monitor` for two record types pulled in `helius-laserstream`,
//! and with it the entire Solana 3.1 / agave / prost-0.14 generation alongside
//! the 2.3 generation we pin.
//!
//! Keep `solana-client`, `solana-rpc-client`, `reqwest`, and `helius-laserstream`
//! out of this crate's manifest. RPC-fetching wrappers belong in the
//! orchestrator or the monitor; only decoding belongs here.

pub mod apy;
mod catalog;

pub use apy::{
    diff_snapshot, snapshot_from_account, snapshot_from_account_at, BorrowRateCurvePointSnapshot,
    DiffSummaryItem, ReserveDiff, ReserveSnapshot, WithdrawalCapSnapshot,
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
