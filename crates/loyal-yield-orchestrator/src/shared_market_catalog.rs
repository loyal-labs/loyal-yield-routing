use chrono::{DateTime, Utc};
use klend_interface::{FARMS_PROGRAM_ID, KLEND_PROGRAM_ID};
use loyal_actions::{SharedMarketRole, ASSOCIATED_TOKEN_PROGRAM_ID};
pub use loyal_kamino_codec::{
    decode_kamino_reserve_account, validate_supported_reserve, KaminoReserveCatalogAccount,
    SharedMarketCatalogError,
};
use serde::Serialize;
use sha2::{Digest, Sha256};
use solana_client::rpc_client::RpcClient;
#[allow(deprecated)]
use solana_sdk::system_program;
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey};
use std::{
    collections::{BTreeMap, BTreeSet},
    str::FromStr,
};

use crate::{
    lookup_table_manifest_address_records_hash, LookupTableManifestAddressRecord,
    LookupTableManifestSubject,
};

const GET_MULTIPLE_ACCOUNTS_LIMIT: usize = 100;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, sqlx::FromRow)]
#[serde(rename_all = "camelCase")]
pub struct SupportedKaminoReserve {
    pub market: String,
    pub liquidity_mint: String,
    pub reserve: String,
    pub market_name: Option<String>,
    pub symbol: Option<String>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FinalizedKaminoReserveCatalog {
    /// Lowest finalized RPC context slot across all account batches.
    pub source_slot: u64,
    /// Highest finalized RPC context slot across all account batches.
    pub max_source_slot: u64,
    pub reserves: Vec<KaminoReserveCatalogAccount>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DerivedSharedMarketCatalog {
    pub addresses: Vec<LookupTableManifestAddressRecord>,
    /// Hash of the typed address, role, ordinal, and writability records.
    pub desired_set_hash: String,
    /// Hash of the ordered physical ALT address sequence only.
    pub ordered_address_hash: String,
    /// Hash of the ordered reserve/market/mint identity tuples.
    pub reserve_set_hash: String,
}

pub fn load_finalized_kamino_reserve_catalog(
    rpc: &RpcClient,
    supported_reserves: &[SupportedKaminoReserve],
) -> Result<FinalizedKaminoReserveCatalog, SharedMarketCatalogError> {
    if supported_reserves.is_empty() {
        return Err(SharedMarketCatalogError::EmptySupportedReserveSet);
    }
    let mut parsed = Vec::with_capacity(supported_reserves.len());
    let mut seen_reserves = BTreeSet::new();
    for supported in supported_reserves {
        let reserve = parse_supported_pubkey("reserve", &supported.reserve)?;
        let market = parse_supported_pubkey("market", &supported.market)?;
        let liquidity_mint = parse_supported_pubkey("liquidity_mint", &supported.liquidity_mint)?;
        if !seen_reserves.insert(reserve) {
            return Err(SharedMarketCatalogError::DuplicateSupportedReserve {
                reserve: reserve.to_string(),
            });
        }
        parsed.push((supported, reserve, market, liquidity_mint));
    }

    if parsed.len() > GET_MULTIPLE_ACCOUNTS_LIMIT {
        return Err(SharedMarketCatalogError::TooManySupportedReserves {
            actual: parsed.len(),
            limit: GET_MULTIPLE_ACCOUNTS_LIMIT,
        });
    }
    let reserve_pubkeys = parsed
        .iter()
        .map(|(_, reserve, _, _)| *reserve)
        .collect::<Vec<_>>();
    let response = rpc
        .get_multiple_accounts_with_commitment(&reserve_pubkeys, CommitmentConfig::finalized())
        .map_err(|error| SharedMarketCatalogError::Rpc(error.to_string()))?;
    if response.value.len() != parsed.len() {
        return Err(SharedMarketCatalogError::InconsistentRpcBatch);
    }
    let source_slot = response.context.slot;
    let mut decoded = Vec::with_capacity(parsed.len());
    for ((_, reserve, expected_market, expected_mint), account) in parsed.iter().zip(response.value)
    {
        let account = account
            .as_ref()
            .ok_or(SharedMarketCatalogError::MissingReserveAccount { reserve: *reserve })?;
        let decoded_reserve = decode_kamino_reserve_account(*reserve, account)?;
        validate_supported_reserve(&decoded_reserve, *expected_market, *expected_mint)?;
        decoded.push(decoded_reserve);
    }
    decoded.sort_by_key(|reserve| reserve.reserve.to_string());
    Ok(FinalizedKaminoReserveCatalog {
        source_slot,
        max_source_slot: source_slot,
        reserves: decoded,
    })
}

pub fn derive_shared_market_catalog(
    reserves: &[KaminoReserveCatalogAccount],
) -> Result<DerivedSharedMarketCatalog, SharedMarketCatalogError> {
    if reserves.is_empty() {
        return Err(SharedMarketCatalogError::EmptySupportedReserveSet);
    }
    let mut roles = BTreeMap::<String, (BTreeSet<SharedMarketRole>, bool)>::new();
    let mut seen_reserves = BTreeSet::new();
    for reserve in reserves {
        if !seen_reserves.insert(reserve.reserve) {
            return Err(SharedMarketCatalogError::DuplicateSupportedReserve {
                reserve: reserve.reserve.to_string(),
            });
        }
        add_catalog_role(&mut roles, reserve.market, SharedMarketRole::Market, false);
        add_catalog_role(
            &mut roles,
            reserve.market_authority,
            SharedMarketRole::MarketAuthority,
            false,
        );
        add_catalog_role(&mut roles, reserve.reserve, SharedMarketRole::Reserve, true);
        add_catalog_role(
            &mut roles,
            reserve.liquidity_mint,
            SharedMarketRole::LiquidityMint,
            false,
        );
        add_catalog_role(
            &mut roles,
            reserve.liquidity_supply,
            SharedMarketRole::LiquiditySupply,
            true,
        );
        add_catalog_role(
            &mut roles,
            reserve.collateral_mint,
            SharedMarketRole::CollateralMint,
            true,
        );
        add_catalog_role(
            &mut roles,
            reserve.collateral_supply,
            SharedMarketRole::CollateralSupply,
            true,
        );
        for oracle in [
            reserve.pyth_oracle,
            reserve.switchboard_price_oracle,
            reserve.switchboard_twap_oracle,
        ]
        .into_iter()
        .flatten()
        {
            add_catalog_role(&mut roles, oracle, SharedMarketRole::Oracle, false);
        }
        if let Some(scope_prices) = reserve.scope_prices {
            add_catalog_role(
                &mut roles,
                scope_prices,
                SharedMarketRole::ScopePrices,
                false,
            );
        }
        if let Some(collateral_farm) = reserve.collateral_farm {
            add_catalog_role(
                &mut roles,
                collateral_farm,
                SharedMarketRole::ReserveFarmState,
                true,
            );
        }
        add_catalog_role(
            &mut roles,
            reserve.liquidity_token_program,
            SharedMarketRole::Infrastructure,
            false,
        );
    }
    for infrastructure in [
        KLEND_PROGRAM_ID,
        FARMS_PROGRAM_ID,
        // The outer route and Kamino collateral instructions always use the
        // classic token program, even when a reserve's liquidity mint uses
        // Token-2022.
        spl_token::ID,
        ASSOCIATED_TOKEN_PROGRAM_ID,
        solana_sdk::sysvar::instructions::id(),
        solana_sdk::sysvar::rent::id(),
        system_program::ID,
        // Vanilla-obligation setup passes the default pubkey as both seed
        // accounts, so it is a real compiler-eligible route account.
        Pubkey::default(),
    ] {
        add_catalog_role(
            &mut roles,
            infrastructure,
            SharedMarketRole::Infrastructure,
            false,
        );
    }

    let addresses = roles
        .into_iter()
        .enumerate()
        .map(|(ordinal, (address, (roles, is_writable)))| {
            Ok(LookupTableManifestAddressRecord {
                address,
                ordinal: i32::try_from(ordinal)
                    .map_err(|_| SharedMarketCatalogError::AddressCountOverflow)?,
                semantic_class: LookupTableManifestSubject::SharedMarket,
                account_role: roles
                    .into_iter()
                    .map(SharedMarketRole::as_str)
                    .collect::<Vec<_>>()
                    .join(","),
                is_writable,
            })
        })
        .collect::<Result<Vec<_>, SharedMarketCatalogError>>()?;
    let desired_set_hash = lookup_table_manifest_address_records_hash(&addresses);
    let ordered_address_hash =
        length_prefixed_hash(addresses.iter().map(|row| row.address.as_str()));
    let mut reserve_identities = reserves
        .iter()
        .map(|reserve| {
            format!(
                "{}:{}:{}",
                reserve.reserve, reserve.market, reserve.liquidity_mint
            )
        })
        .collect::<Vec<_>>();
    reserve_identities.sort();
    reserve_identities.dedup();
    let reserve_set_hash = length_prefixed_hash(reserve_identities.iter().map(String::as_str));
    Ok(DerivedSharedMarketCatalog {
        addresses,
        desired_set_hash,
        ordered_address_hash,
        reserve_set_hash,
    })
}

fn parse_supported_pubkey(
    field: &'static str,
    value: &str,
) -> Result<Pubkey, SharedMarketCatalogError> {
    Pubkey::from_str(value).map_err(|_| SharedMarketCatalogError::InvalidSupportedPubkey {
        field,
        value: value.to_owned(),
    })
}

fn add_catalog_role(
    roles: &mut BTreeMap<String, (BTreeSet<SharedMarketRole>, bool)>,
    address: Pubkey,
    role: SharedMarketRole,
    is_writable: bool,
) {
    let entry = roles.entry(address.to_string()).or_default();
    entry.0.insert(role);
    entry.1 |= is_writable;
}

fn length_prefixed_hash<'a>(values: impl IntoIterator<Item = &'a str>) -> String {
    let mut hasher = Sha256::new();
    for value in values {
        hasher.update((value.len() as u64).to_le_bytes());
        hasher.update(value.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

#[cfg(test)]
mod tests {
    use super::*;
    use bytemuck::{bytes_of, Zeroable};
    use klend_interface::state::{Reserve, SplDiscriminate};
    use solana_sdk::account::Account;

    fn reserve_account(market: Pubkey, liquidity_mint: Pubkey, seed: u8) -> (Pubkey, Account) {
        let reserve_address = Pubkey::new_unique();
        let mut reserve = Reserve::zeroed();
        reserve.lending_market = market;
        reserve.farm_collateral = Pubkey::new_unique();
        reserve.liquidity.mint_pubkey = liquidity_mint;
        reserve.liquidity.token_program = spl_token::ID;
        reserve.liquidity.supply_vault = Pubkey::new_unique();
        reserve.collateral.mint_pubkey = Pubkey::new_unique();
        reserve.collateral.supply_vault = Pubkey::new_unique();
        reserve.config.token_info.pyth_configuration.price = Pubkey::new_unique();
        reserve
            .config
            .token_info
            .switchboard_configuration
            .price_aggregator = Pubkey::new_unique();
        reserve
            .config
            .token_info
            .switchboard_configuration
            .twap_aggregator = Pubkey::new_unique();
        reserve.config.token_info.scope_configuration.price_feed = Pubkey::new_unique();
        reserve.version = u64::from(seed);
        let mut data = Reserve::SPL_DISCRIMINATOR_SLICE.to_vec();
        data.extend_from_slice(bytes_of(&reserve));
        (
            reserve_address,
            Account {
                lamports: 1,
                data,
                owner: KLEND_PROGRAM_ID,
                executable: false,
                rent_epoch: 0,
            },
        )
    }

    #[test]
    fn finalized_reserve_decoder_checks_owner_and_sql_identity() {
        let market = Pubkey::new_unique();
        let mint = Pubkey::new_unique();
        let (reserve, account) = reserve_account(market, mint, 1);
        let decoded = decode_kamino_reserve_account(reserve, &account).unwrap();
        validate_supported_reserve(&decoded, market, mint).unwrap();
        assert!(matches!(
            validate_supported_reserve(&decoded, Pubkey::new_unique(), mint),
            Err(SharedMarketCatalogError::MarketMismatch { .. })
        ));
        assert!(matches!(
            validate_supported_reserve(&decoded, market, Pubkey::new_unique()),
            Err(SharedMarketCatalogError::LiquidityMintMismatch { .. })
        ));

        let mut wrong_owner = account;
        wrong_owner.owner = Pubkey::new_unique();
        assert!(matches!(
            decode_kamino_reserve_account(reserve, &wrong_owner),
            Err(SharedMarketCatalogError::InvalidReserveOwner { .. })
        ));
    }

    #[test]
    fn shared_market_catalog_union_and_hashes_are_order_independent() {
        let market = Pubkey::new_unique();
        let mint = Pubkey::new_unique();
        let (reserve_a, account_a) = reserve_account(market, mint, 1);
        let (reserve_b, account_b) = reserve_account(market, mint, 2);
        let decoded_a = decode_kamino_reserve_account(reserve_a, &account_a).unwrap();
        let decoded_b = decode_kamino_reserve_account(reserve_b, &account_b).unwrap();
        let forward =
            derive_shared_market_catalog(&[decoded_a.clone(), decoded_b.clone()]).unwrap();
        let reverse = derive_shared_market_catalog(&[decoded_b, decoded_a]).unwrap();
        assert_eq!(forward, reverse);
        assert_eq!(
            forward
                .addresses
                .iter()
                .map(|row| row.address.as_str())
                .collect::<Vec<_>>(),
            forward
                .addresses
                .iter()
                .map(|row| row.address.as_str())
                .collect::<BTreeSet<_>>()
                .into_iter()
                .collect::<Vec<_>>()
        );
        assert!(forward.addresses.iter().any(|row| {
            row.address == reserve_a.to_string()
                && row.account_role == SharedMarketRole::Reserve.as_str()
                && row.is_writable
        }));
        assert!(forward.addresses.iter().any(|row| {
            row.address == market.to_string()
                && row.account_role == SharedMarketRole::Market.as_str()
                && !row.is_writable
        }));
        assert_eq!(forward.desired_set_hash.len(), 64);
        assert_eq!(forward.ordered_address_hash.len(), 64);
        assert_eq!(forward.reserve_set_hash.len(), 64);
    }

    #[test]
    fn shared_market_catalog_unions_roles_and_writability_per_address() {
        let market = Pubkey::new_unique();
        let mint = Pubkey::new_unique();
        let (reserve_a, account_a) = reserve_account(market, mint, 1);
        let (reserve_b, account_b) = reserve_account(market, mint, 2);
        let mut decoded_a = decode_kamino_reserve_account(reserve_a, &account_a).unwrap();
        let mut decoded_b = decode_kamino_reserve_account(reserve_b, &account_b).unwrap();
        decoded_a.pyth_oracle = Some(market);
        decoded_b.liquidity_supply = market;

        let catalog = derive_shared_market_catalog(&[decoded_a, decoded_b]).unwrap();
        let shared = catalog
            .addresses
            .iter()
            .find(|row| row.address == market.to_string())
            .expect("shared multi-role address");
        assert_eq!(shared.account_role, "market,liquidity_supply,oracle");
        assert!(shared.is_writable);
        assert_eq!(
            catalog.desired_set_hash,
            lookup_table_manifest_address_records_hash(&catalog.addresses)
        );
    }
}
