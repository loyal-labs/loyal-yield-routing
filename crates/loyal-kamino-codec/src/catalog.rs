use klend_interface::{
    from_account_data, pda::lending_market_authority, state::Reserve, KLEND_PROGRAM_ID,
};
use solana_sdk::{account::Account, pubkey::Pubkey};
use thiserror::Error;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct KaminoReserveCatalogAccount {
    pub reserve: Pubkey,
    pub market: Pubkey,
    pub market_authority: Pubkey,
    pub liquidity_mint: Pubkey,
    pub liquidity_token_program: Pubkey,
    pub liquidity_supply: Pubkey,
    pub collateral_mint: Pubkey,
    pub collateral_supply: Pubkey,
    pub collateral_farm: Option<Pubkey>,
    pub pyth_oracle: Option<Pubkey>,
    pub switchboard_price_oracle: Option<Pubkey>,
    pub switchboard_twap_oracle: Option<Pubkey>,
    pub scope_prices: Option<Pubkey>,
}

#[derive(Debug, Error)]
pub enum SharedMarketCatalogError {
    #[error("shared-market catalog requires at least one active safe supported reserve")]
    EmptySupportedReserveSet,
    #[error("supported reserve row has invalid {field} pubkey {value}")]
    InvalidSupportedPubkey { field: &'static str, value: String },
    #[error("supported reserve {reserve} is duplicated")]
    DuplicateSupportedReserve { reserve: String },
    #[error(
        "shared-market catalog has {actual} supported reserves, exceeding the single finalized RPC snapshot limit {limit}"
    )]
    TooManySupportedReserves { actual: usize, limit: usize },
    #[error("finalized RPC account for supported reserve {reserve} is missing")]
    MissingReserveAccount { reserve: Pubkey },
    #[error("reserve {reserve} is owned by {actual}, expected Kamino lend program {expected}")]
    InvalidReserveOwner {
        reserve: Pubkey,
        actual: Pubkey,
        expected: Pubkey,
    },
    #[error("reserve {reserve} account data could not be decoded: {detail}")]
    InvalidReserveData { reserve: Pubkey, detail: String },
    #[error(
        "reserve {reserve} finalized market {actual} does not match supported-reserve market {expected}"
    )]
    MarketMismatch {
        reserve: Pubkey,
        actual: Pubkey,
        expected: Pubkey,
    },
    #[error(
        "reserve {reserve} finalized liquidity mint {actual} does not match supported-reserve mint {expected}"
    )]
    LiquidityMintMismatch {
        reserve: Pubkey,
        actual: Pubkey,
        expected: Pubkey,
    },
    #[error("finalized reserve RPC request failed: {0}")]
    Rpc(String),
    #[error("finalized reserve RPC returned an inconsistent account batch")]
    InconsistentRpcBatch,
    #[error("shared-market catalog address count exceeds PostgreSQL INTEGER")]
    AddressCountOverflow,
}

pub fn decode_kamino_reserve_account(
    reserve: Pubkey,
    account: &Account,
) -> Result<KaminoReserveCatalogAccount, SharedMarketCatalogError> {
    if account.owner != KLEND_PROGRAM_ID {
        return Err(SharedMarketCatalogError::InvalidReserveOwner {
            reserve,
            actual: account.owner,
            expected: KLEND_PROGRAM_ID,
        });
    }
    let state = from_account_data::<Reserve>(&account.data).map_err(|error| {
        SharedMarketCatalogError::InvalidReserveData {
            reserve,
            detail: error.to_string(),
        }
    })?;
    Ok(KaminoReserveCatalogAccount {
        reserve,
        market: state.lending_market,
        market_authority: lending_market_authority(&KLEND_PROGRAM_ID, &state.lending_market).0,
        liquidity_mint: state.liquidity.mint_pubkey,
        liquidity_token_program: state.liquidity.token_program,
        liquidity_supply: state.liquidity.supply_vault,
        collateral_mint: state.collateral.mint_pubkey,
        collateral_supply: state.collateral.supply_vault,
        collateral_farm: non_default_pubkey(state.farm_collateral),
        pyth_oracle: non_default_pubkey(state.config.token_info.pyth_configuration.price),
        switchboard_price_oracle: non_default_pubkey(
            state
                .config
                .token_info
                .switchboard_configuration
                .price_aggregator,
        ),
        switchboard_twap_oracle: non_default_pubkey(
            state
                .config
                .token_info
                .switchboard_configuration
                .twap_aggregator,
        ),
        scope_prices: non_default_pubkey(state.config.token_info.scope_configuration.price_feed),
    })
}

pub fn validate_supported_reserve(
    decoded: &KaminoReserveCatalogAccount,
    expected_market: Pubkey,
    expected_liquidity_mint: Pubkey,
) -> Result<(), SharedMarketCatalogError> {
    if decoded.market != expected_market {
        return Err(SharedMarketCatalogError::MarketMismatch {
            reserve: decoded.reserve,
            actual: decoded.market,
            expected: expected_market,
        });
    }
    if decoded.liquidity_mint != expected_liquidity_mint {
        return Err(SharedMarketCatalogError::LiquidityMintMismatch {
            reserve: decoded.reserve,
            actual: decoded.liquidity_mint,
            expected: expected_liquidity_mint,
        });
    }
    Ok(())
}

fn non_default_pubkey(value: Pubkey) -> Option<Pubkey> {
    (value != Pubkey::default()).then_some(value)
}
