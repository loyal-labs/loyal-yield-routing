//! Configured same-mint Kamino route preparation.
//!
//! The preparer turns a planned same-mint rebalance into one Squads
//! ProgramInteraction policy execution containing a Kamino redeem leg followed
//! by a Kamino deposit leg. Mainnet addresses and quote assumptions come from a
//! reviewable route config so dry runs do not depend on hidden defaults.

use std::{env, fs, path::Path, str::FromStr};

use borsh::BorshSerialize;
use serde::Deserialize;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
use thiserror::Error;

use crate::{
    kamino_deposit_reserve_liquidity_policy_payload,
    kamino_redeem_reserve_collateral_policy_payload, KaminoDepositReserveLiquidityArgs,
    KaminoRedeemReserveCollateralArgs, KaminoReserveInstructionAccounts, SameMintLoopFuture,
    SameMintRouteLoopError, SameMintRoutePreparer, SameMintRouteQuote, SameMintRouteQuoteRequest,
    SquadsPolicyCompiledInstruction, SquadsPolicyInstructionPayload, VaultId,
};
use loyal_actions::SQUADS_SMART_ACCOUNT_PROGRAM_ID;

pub const SAME_MINT_ROUTE_CONFIG_JSON_ENV: &str = "SAME_MINT_ROUTE_CONFIG_JSON";
pub const SAME_MINT_ROUTE_CONFIG_PATH_ENV: &str = "SAME_MINT_ROUTE_CONFIG_PATH";
const SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR: [u8; 8] =
    [90, 81, 187, 81, 39, 70, 128, 78];
const SQUADS_SYNC_SIGNER_COUNT: u8 = 1;
const BPS_DENOMINATOR: u64 = 10_000;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ConfiguredSameMintRoutePreparer {
    routes: Vec<ConfiguredSameMintRoute>,
}

impl ConfiguredSameMintRoutePreparer {
    pub fn new(routes: Vec<ConfiguredSameMintRoute>) -> Result<Self, SameMintRoutePreparerError> {
        if routes.is_empty() {
            return Err(SameMintRoutePreparerError::EmptyRoutes);
        }
        Ok(Self { routes })
    }

    pub fn from_env() -> Result<Self, SameMintRoutePreparerError> {
        if let Ok(json) = env::var(SAME_MINT_ROUTE_CONFIG_JSON_ENV) {
            return Self::from_json(&json);
        }
        let path = env::var(SAME_MINT_ROUTE_CONFIG_PATH_ENV).map_err(|_| {
            SameMintRoutePreparerError::MissingConfigEnv {
                json_env: SAME_MINT_ROUTE_CONFIG_JSON_ENV,
                path_env: SAME_MINT_ROUTE_CONFIG_PATH_ENV,
            }
        })?;
        Self::from_path(path)
    }

    pub fn from_path(path: impl AsRef<Path>) -> Result<Self, SameMintRoutePreparerError> {
        let json = fs::read_to_string(path)?;
        Self::from_json(&json)
    }

    pub fn from_json(json: &str) -> Result<Self, SameMintRoutePreparerError> {
        let config: SameMintRouteConfigEnvelope = serde_json::from_str(json)?;
        config.try_into_preparer()
    }

    pub fn routes(&self) -> &[ConfiguredSameMintRoute] {
        &self.routes
    }

    fn route_for(
        &self,
        request: &SameMintRouteQuoteRequest,
    ) -> Result<&ConfiguredSameMintRoute, SameMintRoutePreparerError> {
        self.routes
            .iter()
            .find(|route| route.matches(request))
            .ok_or_else(|| SameMintRoutePreparerError::MissingRoute {
                vault_id: request.vault_id,
                source_reserve: request.source_reserve.clone(),
                target_reserve: request.target_reserve.clone(),
                liquidity_mint: request.liquidity_mint.clone(),
            })
    }
}

impl SameMintRoutePreparer for ConfiguredSameMintRoutePreparer {
    fn prepare_same_mint_route<'a>(
        &'a self,
        request: SameMintRouteQuoteRequest,
    ) -> SameMintLoopFuture<'a, SameMintRouteQuote> {
        Box::pin(async move {
            self.prepare_route(request)
                .map_err(|error| SameMintRouteLoopError::quote(error.to_string()))
        })
    }
}

impl ConfiguredSameMintRoutePreparer {
    fn prepare_route(
        &self,
        request: SameMintRouteQuoteRequest,
    ) -> Result<SameMintRouteQuote, SameMintRoutePreparerError> {
        let route = self.route_for(&request)?;
        let quote = route.quote.quote(request.redeem_amount_raw)?;
        let redeem_payload =
            kamino_redeem_reserve_collateral_policy_payload(KaminoRedeemReserveCollateralArgs {
                vault: route.vault,
                accounts: route.source_accounts,
                collateral_amount: request.redeem_amount_raw,
            });
        let deposit_payload =
            kamino_deposit_reserve_liquidity_policy_payload(KaminoDepositReserveLiquidityArgs {
                vault: route.vault,
                accounts: route.target_accounts,
                liquidity_amount: quote.deposit_liquidity_amount_raw,
            });
        let (compiled_instructions, transaction_accounts) =
            merge_policy_payloads([redeem_payload, deposit_payload])?;
        let route_instruction = execute_squads_program_interaction_instruction(
            route.policy_account,
            route.delegated_signer,
            route.vault_index,
            compiled_instructions,
            vec![
                route.withdraw_constraint_index,
                route.deposit_constraint_index,
            ],
            transaction_accounts,
        )?;

        Ok(SameMintRouteQuote {
            redeem_amount_raw: request.redeem_amount_raw,
            deposit_amount_raw: quote.deposit_liquidity_amount_raw,
            route_instructions: vec![route_instruction],
        })
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ConfiguredSameMintRoute {
    pub vault_id: Option<VaultId>,
    pub source_reserve: String,
    pub target_reserve: String,
    pub liquidity_mint: String,
    pub policy_account: Pubkey,
    pub delegated_signer: Pubkey,
    pub vault_index: u8,
    pub vault: Pubkey,
    pub withdraw_constraint_index: u8,
    pub deposit_constraint_index: u8,
    pub source_accounts: KaminoReserveInstructionAccounts,
    pub target_accounts: KaminoReserveInstructionAccounts,
    pub quote: SameMintRouteQuoteConfig,
}

impl ConfiguredSameMintRoute {
    fn matches(&self, request: &SameMintRouteQuoteRequest) -> bool {
        self.vault_id
            .is_none_or(|vault_id| vault_id == request.vault_id)
            && self.source_reserve == request.source_reserve
            && self.target_reserve == request.target_reserve
            && self.liquidity_mint == request.liquidity_mint
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SameMintRouteQuoteConfig {
    pub redeem_collateral_to_liquidity_bps: u64,
    pub deposit_liquidity_bps: u64,
    pub max_redeem_collateral_raw: Option<u64>,
    pub min_deposit_liquidity_raw: Option<u64>,
}

impl SameMintRouteQuoteConfig {
    fn quote(
        self,
        redeem_amount_raw: u64,
    ) -> Result<PreparedSameMintQuote, SameMintRoutePreparerError> {
        if let Some(max) = self.max_redeem_collateral_raw {
            if redeem_amount_raw > max {
                return Err(SameMintRoutePreparerError::RedeemAmountExceedsRouteCap {
                    redeem_amount_raw,
                    max_redeem_collateral_raw: max,
                });
            }
        }
        let liquidity_out = mul_div_u64(
            redeem_amount_raw,
            self.redeem_collateral_to_liquidity_bps,
            BPS_DENOMINATOR,
        )?;
        let deposit_liquidity_amount_raw =
            mul_div_u64(liquidity_out, self.deposit_liquidity_bps, BPS_DENOMINATOR)?;
        if deposit_liquidity_amount_raw == 0 {
            return Err(SameMintRoutePreparerError::ZeroDepositQuote);
        }
        if let Some(min) = self.min_deposit_liquidity_raw {
            if deposit_liquidity_amount_raw < min {
                return Err(SameMintRoutePreparerError::DepositQuoteBelowMinimum {
                    deposit_liquidity_amount_raw,
                    min_deposit_liquidity_raw: min,
                });
            }
        }

        Ok(PreparedSameMintQuote {
            deposit_liquidity_amount_raw,
        })
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct PreparedSameMintQuote {
    deposit_liquidity_amount_raw: u64,
}

#[derive(Debug, Deserialize)]
#[serde(untagged)]
enum SameMintRouteConfigEnvelope {
    Routes { routes: Vec<SameMintRouteInput> },
    RouteList(Vec<SameMintRouteInput>),
}

impl SameMintRouteConfigEnvelope {
    fn try_into_preparer(
        self,
    ) -> Result<ConfiguredSameMintRoutePreparer, SameMintRoutePreparerError> {
        let routes = match self {
            Self::Routes { routes } | Self::RouteList(routes) => routes,
        }
        .into_iter()
        .map(ConfiguredSameMintRoute::try_from)
        .collect::<Result<Vec<_>, _>>()?;
        ConfiguredSameMintRoutePreparer::new(routes)
    }
}

#[derive(Debug, Deserialize)]
struct SameMintRouteInput {
    vault_id: Option<i64>,
    source_reserve: String,
    target_reserve: String,
    liquidity_mint: String,
    policy_account: String,
    delegated_signer: String,
    vault_index: u8,
    vault: String,
    withdraw_constraint_index: u8,
    deposit_constraint_index: u8,
    source_accounts: KaminoReserveAccountsInput,
    target_accounts: KaminoReserveAccountsInput,
    quote: SameMintRouteQuoteInput,
}

impl TryFrom<SameMintRouteInput> for ConfiguredSameMintRoute {
    type Error = SameMintRoutePreparerError;

    fn try_from(value: SameMintRouteInput) -> Result<Self, Self::Error> {
        if value.source_reserve == value.target_reserve {
            return Err(SameMintRoutePreparerError::SameSourceAndTargetReserve {
                reserve: value.source_reserve,
            });
        }
        if value.liquidity_mint.is_empty() {
            return Err(SameMintRoutePreparerError::EmptyLiquidityMint);
        }

        Ok(Self {
            vault_id: value.vault_id.map(VaultId),
            source_reserve: value.source_reserve,
            target_reserve: value.target_reserve,
            liquidity_mint: value.liquidity_mint,
            policy_account: parse_pubkey("policy_account", &value.policy_account)?,
            delegated_signer: parse_pubkey("delegated_signer", &value.delegated_signer)?,
            vault_index: value.vault_index,
            vault: parse_pubkey("vault", &value.vault)?,
            withdraw_constraint_index: value.withdraw_constraint_index,
            deposit_constraint_index: value.deposit_constraint_index,
            source_accounts: value.source_accounts.try_into_accounts("source_accounts")?,
            target_accounts: value.target_accounts.try_into_accounts("target_accounts")?,
            quote: value.quote.try_into()?,
        })
    }
}

#[derive(Debug, Deserialize)]
struct KaminoReserveAccountsInput {
    reserve: String,
    market: String,
    lending_market_authority: String,
    liquidity_mint: String,
    reserve_liquidity_supply: String,
    collateral_mint: String,
    vault_liquidity: String,
    vault_collateral: String,
}

impl KaminoReserveAccountsInput {
    fn try_into_accounts(
        self,
        prefix: &'static str,
    ) -> Result<KaminoReserveInstructionAccounts, SameMintRoutePreparerError> {
        Ok(KaminoReserveInstructionAccounts {
            reserve: parse_pubkey_field(prefix, "reserve", &self.reserve)?,
            market: parse_pubkey_field(prefix, "market", &self.market)?,
            lending_market_authority: parse_pubkey_field(
                prefix,
                "lending_market_authority",
                &self.lending_market_authority,
            )?,
            liquidity_mint: parse_pubkey_field(prefix, "liquidity_mint", &self.liquidity_mint)?,
            reserve_liquidity_supply: parse_pubkey_field(
                prefix,
                "reserve_liquidity_supply",
                &self.reserve_liquidity_supply,
            )?,
            collateral_mint: parse_pubkey_field(prefix, "collateral_mint", &self.collateral_mint)?,
            vault_liquidity: parse_pubkey_field(prefix, "vault_liquidity", &self.vault_liquidity)?,
            vault_collateral: parse_pubkey_field(
                prefix,
                "vault_collateral",
                &self.vault_collateral,
            )?,
        })
    }
}

#[derive(Debug, Deserialize)]
struct SameMintRouteQuoteInput {
    redeem_collateral_to_liquidity_bps: u64,
    #[serde(default = "default_bps")]
    deposit_liquidity_bps: u64,
    max_redeem_collateral_raw: Option<u64>,
    min_deposit_liquidity_raw: Option<u64>,
}

impl TryFrom<SameMintRouteQuoteInput> for SameMintRouteQuoteConfig {
    type Error = SameMintRoutePreparerError;

    fn try_from(value: SameMintRouteQuoteInput) -> Result<Self, Self::Error> {
        if value.redeem_collateral_to_liquidity_bps == 0 {
            return Err(SameMintRoutePreparerError::InvalidQuoteBps {
                field: "redeem_collateral_to_liquidity_bps",
                value: value.redeem_collateral_to_liquidity_bps,
            });
        }
        if value.deposit_liquidity_bps == 0 || value.deposit_liquidity_bps > BPS_DENOMINATOR {
            return Err(SameMintRoutePreparerError::InvalidQuoteBps {
                field: "deposit_liquidity_bps",
                value: value.deposit_liquidity_bps,
            });
        }
        Ok(Self {
            redeem_collateral_to_liquidity_bps: value.redeem_collateral_to_liquidity_bps,
            deposit_liquidity_bps: value.deposit_liquidity_bps,
            max_redeem_collateral_raw: value.max_redeem_collateral_raw,
            min_deposit_liquidity_raw: value.min_deposit_liquidity_raw,
        })
    }
}

fn default_bps() -> u64 {
    BPS_DENOMINATOR
}

fn merge_policy_payloads<const N: usize>(
    payloads: [SquadsPolicyInstructionPayload; N],
) -> Result<(Vec<SquadsPolicyCompiledInstruction>, Vec<AccountMeta>), SameMintRoutePreparerError> {
    let mut transaction_accounts = Vec::new();
    let mut compiled = Vec::new();
    for payload in payloads {
        for instruction in payload.instructions {
            let program_id_index = remap_account_index(
                &mut transaction_accounts,
                &payload.accounts,
                instruction.program_id_index,
            )?;
            let accounts = instruction
                .accounts
                .into_iter()
                .map(|index| {
                    remap_account_index(&mut transaction_accounts, &payload.accounts, index)
                })
                .collect::<Result<Vec<_>, _>>()?;
            compiled.push(SquadsPolicyCompiledInstruction {
                program_id_index,
                accounts,
                data: instruction.data,
            });
        }
    }
    Ok((compiled, transaction_accounts))
}

fn remap_account_index(
    transaction_accounts: &mut Vec<AccountMeta>,
    source_accounts: &[AccountMeta],
    index: usize,
) -> Result<usize, SameMintRoutePreparerError> {
    let meta = source_accounts
        .get(index)
        .ok_or(SameMintRoutePreparerError::InvalidCompiledAccountIndex { index })?
        .clone();
    Ok(push_or_update_account_meta(transaction_accounts, meta))
}

fn push_or_update_account_meta(accounts: &mut Vec<AccountMeta>, meta: AccountMeta) -> usize {
    if let Some(index) = accounts
        .iter()
        .position(|existing| existing.pubkey == meta.pubkey)
    {
        accounts[index].is_writable |= meta.is_writable;
        accounts[index].is_signer |= meta.is_signer;
        return index;
    }

    let index = accounts.len();
    accounts.push(meta);
    index
}

fn execute_squads_program_interaction_instruction(
    policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    compiled_instructions: Vec<SquadsPolicyCompiledInstruction>,
    instruction_constraint_indices: Vec<u8>,
    mut transaction_accounts: Vec<AccountMeta>,
) -> Result<Instruction, SameMintRoutePreparerError> {
    let mut accounts = vec![
        AccountMeta::new(policy, false),
        AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
        AccountMeta::new_readonly(signer, true),
    ];
    accounts.append(&mut transaction_accounts);

    Ok(Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts,
        data: serialize_squads_sync_policy_payload_args(
            account_index,
            SquadsPolicyPayload::ProgramInteraction(SquadsProgramInteractionPayload {
                instruction_constraint_indices: Some(instruction_constraint_indices),
                transaction_payload: SquadsProgramInteractionTransactionPayload::SyncTransaction(
                    SquadsProgramInteractionSyncPayload {
                        account_index,
                        instructions: squads_compiled_instruction_payload(&compiled_instructions)?,
                    },
                ),
            }),
        )?,
    })
}

fn serialize_squads_sync_policy_payload_args(
    account_index: u8,
    policy_payload: SquadsPolicyPayload,
) -> Result<Vec<u8>, SameMintRoutePreparerError> {
    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    SquadsSyncTransactionArgs {
        account_index,
        num_signers: SQUADS_SYNC_SIGNER_COUNT,
        payload: SquadsSyncPayload::Policy(policy_payload),
    }
    .serialize(&mut data)
    .map_err(|error| SameMintRoutePreparerError::SquadsSerialize(error.to_string()))?;
    Ok(data)
}

fn squads_compiled_instruction_payload(
    instructions: &[SquadsPolicyCompiledInstruction],
) -> Result<Vec<u8>, SameMintRoutePreparerError> {
    let mut payload = Vec::new();
    payload.push(checked_u8(
        instructions.len(),
        "compiled instruction count",
    )?);

    for instruction in instructions {
        payload.push(checked_u8(
            instruction.program_id_index,
            "program id index",
        )?);
        payload.push(checked_u8(
            instruction.accounts.len(),
            "instruction account count",
        )?);
        for account in &instruction.accounts {
            payload.push(checked_u8(*account, "instruction account index")?);
        }
        let data_len = u16::try_from(instruction.data.len()).map_err(|_| {
            SameMintRoutePreparerError::SquadsPayloadOverflow {
                field: "instruction data length",
                value: instruction.data.len(),
            }
        })?;
        payload.extend_from_slice(&data_len.to_le_bytes());
        payload.extend_from_slice(&instruction.data);
    }

    Ok(payload)
}

fn checked_u8(value: usize, field: &'static str) -> Result<u8, SameMintRoutePreparerError> {
    u8::try_from(value)
        .map_err(|_| SameMintRoutePreparerError::SquadsPayloadOverflow { field, value })
}

fn mul_div_u64(
    value: u64,
    multiplier: u64,
    denominator: u64,
) -> Result<u64, SameMintRoutePreparerError> {
    let result = (u128::from(value) * u128::from(multiplier)) / u128::from(denominator);
    u64::try_from(result).map_err(|_| SameMintRoutePreparerError::AmountOverflow)
}

fn parse_pubkey_field(
    prefix: &'static str,
    field: &'static str,
    value: &str,
) -> Result<Pubkey, SameMintRoutePreparerError> {
    parse_pubkey(format!("{prefix}.{field}"), value)
}

fn parse_pubkey(
    field: impl Into<String>,
    value: &str,
) -> Result<Pubkey, SameMintRoutePreparerError> {
    Pubkey::from_str(value).map_err(|_| SameMintRoutePreparerError::InvalidPubkey {
        field: field.into(),
        value: value.to_owned(),
    })
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsSyncPayload {
    Transaction(Vec<u8>),
    Policy(SquadsPolicyPayload),
}

#[derive(BorshSerialize)]
struct SquadsSyncTransactionArgs {
    account_index: u8,
    num_signers: u8,
    payload: SquadsSyncPayload,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsPolicyPayload {
    InternalFundTransfer(Vec<u8>),
    ProgramInteraction(SquadsProgramInteractionPayload),
    SpendingLimit(Vec<u8>),
    SettingsChange(Vec<u8>),
}

#[derive(BorshSerialize)]
struct SquadsProgramInteractionPayload {
    instruction_constraint_indices: Option<Vec<u8>>,
    transaction_payload: SquadsProgramInteractionTransactionPayload,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsProgramInteractionTransactionPayload {
    AsyncTransaction(Vec<u8>),
    SyncTransaction(SquadsProgramInteractionSyncPayload),
}

#[derive(BorshSerialize)]
struct SquadsProgramInteractionSyncPayload {
    account_index: u8,
    instructions: Vec<u8>,
}

#[derive(Debug, Error)]
pub enum SameMintRoutePreparerError {
    #[error("set {json_env} or {path_env} with same-mint route config")]
    MissingConfigEnv {
        json_env: &'static str,
        path_env: &'static str,
    },
    #[error("same-mint route config contains no routes")]
    EmptyRoutes,
    #[error("same-mint route config source and target reserve are both {reserve}")]
    SameSourceAndTargetReserve { reserve: String },
    #[error("same-mint route config liquidity_mint is empty")]
    EmptyLiquidityMint,
    #[error("invalid pubkey in {field}: {value}")]
    InvalidPubkey { field: String, value: String },
    #[error("same-mint route is not configured for vault {vault_id}, {source_reserve} -> {target_reserve}, mint {liquidity_mint}")]
    MissingRoute {
        vault_id: VaultId,
        source_reserve: String,
        target_reserve: String,
        liquidity_mint: String,
    },
    #[error("{field} must be in a supported basis-point range, got {value}")]
    InvalidQuoteBps { field: &'static str, value: u64 },
    #[error("redeem amount {redeem_amount_raw} exceeds route cap {max_redeem_collateral_raw}")]
    RedeemAmountExceedsRouteCap {
        redeem_amount_raw: u64,
        max_redeem_collateral_raw: u64,
    },
    #[error("same-mint quote produced zero deposit liquidity")]
    ZeroDepositQuote,
    #[error(
        "deposit quote {deposit_liquidity_amount_raw} is below minimum {min_deposit_liquidity_raw}"
    )]
    DepositQuoteBelowMinimum {
        deposit_liquidity_amount_raw: u64,
        min_deposit_liquidity_raw: u64,
    },
    #[error("same-mint amount calculation overflowed")]
    AmountOverflow,
    #[error("compiled instruction references missing transaction account index {index}")]
    InvalidCompiledAccountIndex { index: usize },
    #[error("Squads payload {field} does not fit u8/u16 encoding: {value}")]
    SquadsPayloadOverflow { field: &'static str, value: usize },
    #[error("failed to read same-mint route config: {0}")]
    Io(#[from] std::io::Error),
    #[error("failed to parse same-mint route config JSON: {0}")]
    Json(#[from] serde_json::Error),
    #[error("failed to serialize Squads payload: {0}")]
    SquadsSerialize(String),
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{DecisionId, VaultId};

    fn key(seed: u8) -> Pubkey {
        Pubkey::new_from_array([seed; 32])
    }

    fn reserve_accounts(offset: u8) -> KaminoReserveInstructionAccounts {
        KaminoReserveInstructionAccounts {
            reserve: key(offset),
            market: key(offset + 1),
            lending_market_authority: key(offset + 2),
            liquidity_mint: key(4),
            reserve_liquidity_supply: key(offset + 3),
            collateral_mint: key(offset + 4),
            vault_liquidity: key(7),
            vault_collateral: key(offset + 5),
        }
    }

    fn route() -> ConfiguredSameMintRoute {
        ConfiguredSameMintRoute {
            vault_id: Some(VaultId(7)),
            source_reserve: "source".to_owned(),
            target_reserve: "target".to_owned(),
            liquidity_mint: "USDC".to_owned(),
            policy_account: key(20),
            delegated_signer: key(21),
            vault_index: 2,
            vault: key(22),
            withdraw_constraint_index: 0,
            deposit_constraint_index: 2,
            source_accounts: reserve_accounts(30),
            target_accounts: reserve_accounts(40),
            quote: SameMintRouteQuoteConfig {
                redeem_collateral_to_liquidity_bps: 9_900,
                deposit_liquidity_bps: 9_800,
                max_redeem_collateral_raw: Some(1_000_000),
                min_deposit_liquidity_raw: Some(1),
            },
        }
    }

    #[tokio::test]
    async fn prepares_same_mint_route_instruction_from_configured_kamino_legs() {
        let preparer = ConfiguredSameMintRoutePreparer::new(vec![route()]).unwrap();

        let quote = preparer
            .prepare_same_mint_route(SameMintRouteQuoteRequest {
                decision_id: DecisionId(99),
                vault_id: VaultId(7),
                source_reserve: "source".to_owned(),
                target_reserve: "target".to_owned(),
                liquidity_mint: "USDC".to_owned(),
                redeem_amount_raw: 1_000,
            })
            .await
            .expect("route quote");

        assert_eq!(quote.redeem_amount_raw, 1_000);
        assert_eq!(quote.deposit_amount_raw, 970);
        assert_eq!(quote.route_instructions.len(), 1);
        let instruction = &quote.route_instructions[0];
        assert_eq!(instruction.program_id, SQUADS_SMART_ACCOUNT_PROGRAM_ID);
        assert_eq!(instruction.accounts[0], AccountMeta::new(key(20), false));
        assert_eq!(
            instruction.accounts[1],
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false)
        );
        assert_eq!(
            instruction.accounts[2],
            AccountMeta::new_readonly(key(21), true)
        );
        assert!(instruction
            .data
            .starts_with(&SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR));
    }

    #[test]
    fn quote_enforces_route_amount_cap() {
        let error = route().quote.quote(1_000_001).unwrap_err();

        assert!(matches!(
            error,
            SameMintRoutePreparerError::RedeemAmountExceedsRouteCap { .. }
        ));
    }

    #[test]
    fn parses_json_route_config() {
        let json = format!(
            r#"{{
                "routes": [{{
                    "vault_id": 7,
                    "source_reserve": "source",
                    "target_reserve": "target",
                    "liquidity_mint": "USDC",
                    "policy_account": "{policy}",
                    "delegated_signer": "{signer}",
                    "vault_index": 2,
                    "vault": "{vault}",
                    "withdraw_constraint_index": 0,
                    "deposit_constraint_index": 2,
                    "source_accounts": {source_accounts},
                    "target_accounts": {target_accounts},
                    "quote": {{
                        "redeem_collateral_to_liquidity_bps": 10000,
                        "deposit_liquidity_bps": 9900,
                        "max_redeem_collateral_raw": 1000000
                    }}
                }}]
            }}"#,
            policy = key(20),
            signer = key(21),
            vault = key(22),
            source_accounts = reserve_accounts_json(30),
            target_accounts = reserve_accounts_json(40),
        );

        let preparer = ConfiguredSameMintRoutePreparer::from_json(&json).unwrap();

        assert_eq!(preparer.routes().len(), 1);
        assert_eq!(preparer.routes()[0].deposit_constraint_index, 2);
    }

    fn reserve_accounts_json(offset: u8) -> String {
        format!(
            r#"{{
                "reserve": "{reserve}",
                "market": "{market}",
                "lending_market_authority": "{authority}",
                "liquidity_mint": "{mint}",
                "reserve_liquidity_supply": "{supply}",
                "collateral_mint": "{collateral_mint}",
                "vault_liquidity": "{vault_liquidity}",
                "vault_collateral": "{vault_collateral}"
            }}"#,
            reserve = key(offset),
            market = key(offset + 1),
            authority = key(offset + 2),
            mint = key(4),
            supply = key(offset + 3),
            collateral_mint = key(offset + 4),
            vault_liquidity = key(7),
            vault_collateral = key(offset + 5),
        )
    }
}
