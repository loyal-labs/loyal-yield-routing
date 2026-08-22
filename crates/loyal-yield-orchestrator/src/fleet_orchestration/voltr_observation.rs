use loyal_actions::autonomous_vaults::{
    embedded_backyard_voltr_route_bundle, BackyardVoltrRouteBundle, BackyardVoltrStrategy,
    BACKYARD_VOLTR_WITHDRAWAL_WAIT_SECONDS, VOLTR_KAMINO_ADAPTOR_PROGRAM_ID,
    VOLTR_VAULT_PROGRAM_ID,
};
use serde_json::json;
use sha2::{Digest, Sha256};
use solana_client::{
    rpc_client::RpcClient,
    rpc_config::RpcAccountInfoConfig,
    rpc_request::RpcRequest,
    rpc_response::{Response, RpcKeyedAccount},
};
use solana_sdk::{
    account::Account, commitment_config::CommitmentConfig, program_pack::Pack, pubkey::Pubkey,
};
use spl_token::state::Account as TokenAccount;
use std::{error::Error, str::FromStr};

pub const VOLTR_WITHDRAWAL_RECEIPT_DATA_LENGTH: usize = 112;
pub const VOLTR_WITHDRAWAL_RECEIPT_DISCRIMINATOR: [u8; 8] =
    [0xcb, 0x51, 0xdf, 0x8d, 0xaf, 0x6c, 0x65, 0x72];
const VOLTR_WITHDRAWAL_RECEIPT_PREFIX_LENGTH: usize = 106;
const DECIMAL_FRACTION_BITS: u32 = 48;
const VOLTR_STRATEGY_RECEIPT_DATA_LENGTH: usize = 192;
const VOLTR_STRATEGY_RECEIPT_DISCRIMINATOR: [u8; 8] = [51, 8, 192, 253, 115, 78, 112, 214];
const VOLTR_VAULT_DATA_LENGTH: usize = 928;
const VOLTR_VAULT_DISCRIMINATOR: [u8; 8] = [211, 8, 232, 43, 2, 152, 117, 119];
const VOLTR_VAULT_ASSET_MINT_OFFSET: usize = 104;
const VOLTR_VAULT_IDLE_ATA_OFFSET: usize = 136;
const VOLTR_VAULT_TOTAL_VALUE_OFFSET: usize = 168;
const VOLTR_VAULT_MANAGER_OFFSET: usize = 368;
const VOLTR_VAULT_WITHDRAWAL_WAIT_OFFSET: usize = 456;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct VoltrWithdrawalReceipt {
    pub address: Pubkey,
    pub vault: Pubkey,
    pub user: Pubkey,
    pub amount_lp_escrowed_raw: u64,
    pub amount_asset_to_withdraw_decimal_bits: u128,
    pub upper_bound_asset_raw: u64,
    pub withdrawable_from_ts: u64,
    pub bump: u8,
    pub version: u8,
    pub data_sha256: String,
    pub generation_fingerprint: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct VoltrObservedPosition {
    pub strategy: BackyardVoltrStrategy,
    pub strategy_receipt: Pubkey,
    pub value_raw: u64,
    pub safely_redeemable_raw: u64,
    pub data_sha256: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ConfirmedVoltrObservation {
    pub context_slot: u64,
    pub min_context_slot: u64,
    pub vault: Pubkey,
    pub vault_total_value_raw: u64,
    pub idle_raw: u64,
    pub positions: Vec<VoltrObservedPosition>,
    pub receipts: Vec<VoltrWithdrawalReceipt>,
    pub receipt_set_fingerprint: String,
    pub protected_state_sha256: String,
    pub protected_address_set_sha256: String,
    pub configured_safety_buffer_raw: u64,
    pub required_idle_raw: u64,
    pub idle_shortfall_raw: u64,
    pub investable_idle_raw: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum VoltrObservationError {
    InvalidReceipt(&'static str),
    InvalidPositionSet,
    MixedOrStaleContext,
    AccountingMismatch,
    ArithmeticOverflow,
    Rpc(String),
}

impl std::fmt::Display for VoltrObservationError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::InvalidReceipt(reason) => write!(formatter, "invalid Voltr receipt: {reason}"),
            Self::InvalidPositionSet => formatter.write_str("invalid Voltr strategy position set"),
            Self::MixedOrStaleContext => {
                formatter.write_str("mixed or stale confirmed Voltr context")
            }
            Self::AccountingMismatch => {
                formatter.write_str("Voltr idle plus positions does not equal vault total value")
            }
            Self::ArithmeticOverflow => {
                formatter.write_str("Voltr observation arithmetic overflow")
            }
            Self::Rpc(reason) => write!(
                formatter,
                "Voltr confirmed RPC observation failed: {reason}"
            ),
        }
    }
}

impl Error for VoltrObservationError {}

impl VoltrObservationError {
    pub fn code(&self) -> &'static str {
        match self {
            Self::InvalidReceipt(_) => "invalid_receipt",
            Self::InvalidPositionSet => "invalid_position_set",
            Self::MixedOrStaleContext => "mixed_or_stale_context",
            Self::AccountingMismatch => "accounting_mismatch",
            Self::ArithmeticOverflow => "arithmetic_overflow",
            Self::Rpc(reason) if reason.contains("required vault state account is absent") => {
                "required_account_absent"
            }
            Self::Rpc(reason) if reason.contains("vault owner, layout") => "vault_layout_drift",
            Self::Rpc(reason) if reason.contains("idle ATA did not decode") => {
                "idle_token_decode_failed"
            }
            Self::Rpc(reason) if reason.contains("idle ATA mint, owner") => {
                "idle_token_identity_drift"
            }
            Self::Rpc(reason)
                if reason.starts_with("receipt_scan_rpc:")
                    && (reason.contains("Invalid param")
                        || reason.contains("invalid param")
                        || reason.contains("-32602")) =>
            {
                "receipt_scan_invalid_params"
            }
            Self::Rpc(reason)
                if reason.starts_with("receipt_scan_rpc:")
                    && (reason.contains("Method not found") || reason.contains("-32601")) =>
            {
                "receipt_scan_method_unavailable"
            }
            Self::Rpc(reason)
                if reason.starts_with("receipt_scan_rpc:")
                    && (reason.contains("429") || reason.contains("rate")) =>
            {
                "receipt_scan_rate_limited"
            }
            Self::Rpc(reason) if reason.starts_with("receipt_scan_rpc:") => {
                "receipt_scan_rpc_failed"
            }
            Self::Rpc(reason) if reason.starts_with("vault_snapshot_rpc:") => {
                "vault_snapshot_rpc_failed"
            }
            Self::Rpc(_) => "rpc_transport_or_unclassified_decode",
        }
    }
}

/// Read one stable confirmed view of the exact Backyard vault, its idle token
/// account, all four strategy receipts, and the complete withdrawal-receipt
/// set. The receipt scan brackets the account snapshot; any receipt-set change
/// during the read fails closed and is retried by the planner's bounded poll.
pub fn observe_backyard_voltr_confirmed(
    rpc_url: &str,
    min_context_slot: u64,
) -> Result<ConfirmedVoltrObservation, VoltrObservationError> {
    if rpc_url.trim().is_empty() || min_context_slot == 0 {
        return Err(VoltrObservationError::MixedOrStaleContext);
    }
    let bundle = embedded_backyard_voltr_route_bundle()
        .map_err(|error| VoltrObservationError::Rpc(format!("bundle_decode:{error}")))?;
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    observe_backyard_voltr_confirmed_with_rpc(&rpc, min_context_slot, &bundle)
}

pub fn observe_backyard_voltr_confirmed_with_rpc(
    rpc: &RpcClient,
    min_context_slot: u64,
    bundle: &BackyardVoltrRouteBundle,
) -> Result<ConfirmedVoltrObservation, VoltrObservationError> {
    if min_context_slot == 0 {
        return Err(VoltrObservationError::MixedOrStaleContext);
    }
    let (receipt_slot_before, receipts_before) =
        scan_withdrawal_receipts(rpc, bundle, min_context_slot)?;

    let strategy_receipts = BackyardVoltrStrategy::ALL
        .iter()
        .map(|strategy| {
            bundle
                .template(
                    *strategy,
                    loyal_actions::autonomous_vaults::BackyardVoltrManagerOperation::Deposit,
                )
                .strategy_init_receipt
        })
        .collect::<Vec<_>>();
    let mut addresses = vec![bundle.vault, bundle.idle_ata];
    addresses.extend(strategy_receipts.iter().copied());
    let response = rpc
        .get_multiple_accounts_with_config(
            &addresses,
            RpcAccountInfoConfig {
                encoding: Some(solana_account_decoder_client_types::UiAccountEncoding::Base64),
                commitment: Some(CommitmentConfig::confirmed()),
                min_context_slot: Some(receipt_slot_before),
                ..RpcAccountInfoConfig::default()
            },
        )
        .map_err(|error| VoltrObservationError::Rpc(format!("vault_snapshot_rpc:{error}")))?;
    if response.context.slot < receipt_slot_before || response.value.len() != addresses.len() {
        return Err(VoltrObservationError::MixedOrStaleContext);
    }
    let accounts = response
        .value
        .into_iter()
        .collect::<Option<Vec<_>>>()
        .ok_or_else(|| {
            VoltrObservationError::Rpc("required vault state account is absent".into())
        })?;
    let (vault_total_value_raw, expected_idle_ata) = decode_vault_state(&accounts[0], &bundle)?;
    if expected_idle_ata != bundle.idle_ata {
        return Err(VoltrObservationError::AccountingMismatch);
    }
    let idle = TokenAccount::unpack(&accounts[1].data)
        .map_err(|_| VoltrObservationError::Rpc("idle ATA did not decode".into()))?;
    if accounts[1].owner != spl_token::id()
        || idle.mint != loyal_actions::USDC_MINT
        || idle.owner != bundle.idle_authority
    {
        return Err(VoltrObservationError::Rpc(
            "idle ATA mint, owner, or token program drifted".into(),
        ));
    }
    let positions = BackyardVoltrStrategy::ALL
        .iter()
        .enumerate()
        .map(|(index, strategy)| {
            decode_strategy_receipt(
                &accounts[index + 2],
                bundle,
                *strategy,
                strategy_receipts[index],
            )
        })
        .collect::<Result<Vec<_>, _>>()?;

    let (receipt_slot_after, receipts_after) =
        scan_withdrawal_receipts(rpc, bundle, response.context.slot)?;
    let before_fingerprints = receipts_before
        .iter()
        .map(|receipt| (&receipt.address, &receipt.generation_fingerprint))
        .collect::<Vec<_>>();
    let after_fingerprints = receipts_after
        .iter()
        .map(|receipt| (&receipt.address, &receipt.generation_fingerprint))
        .collect::<Vec<_>>();
    if receipt_slot_after < response.context.slot || before_fingerprints != after_fingerprints {
        return Err(VoltrObservationError::MixedOrStaleContext);
    }
    validated_confirmed_voltr_observation(
        response.context.slot,
        min_context_slot,
        bundle.vault,
        vault_total_value_raw,
        idle.amount,
        positions,
        receipts_after,
        bundle.configured_idle_safety_buffer_raw,
    )
}

fn scan_withdrawal_receipts(
    rpc: &RpcClient,
    bundle: &BackyardVoltrRouteBundle,
    min_context_slot: u64,
) -> Result<(u64, Vec<VoltrWithdrawalReceipt>), VoltrObservationError> {
    let response: Response<Vec<RpcKeyedAccount>> = rpc
        .send(
            RpcRequest::GetProgramAccounts,
            json!([VOLTR_VAULT_PROGRAM_ID.to_string(), {
                "encoding": "base64",
                "commitment": "confirmed",
                "minContextSlot": min_context_slot,
                "withContext": true,
                "filters": [
                    {"memcmp": {"offset": 0, "bytes": bs58::encode(VOLTR_WITHDRAWAL_RECEIPT_DISCRIMINATOR).into_string()}},
                    {"memcmp": {"offset": 8, "bytes": bundle.vault.to_string()}}
                ]
            }]),
        )
        .map_err(|error| VoltrObservationError::Rpc(format!("receipt_scan_rpc:{error}")))?;
    if response.context.slot < min_context_slot {
        return Err(VoltrObservationError::MixedOrStaleContext);
    }
    let mut receipts =
        response
            .value
            .into_iter()
            .map(|keyed| {
                let address = Pubkey::from_str(&keyed.pubkey)
                    .map_err(|_| VoltrObservationError::InvalidReceipt("address"))?;
                let account = keyed.account.decode::<Account>().ok_or(
                    VoltrObservationError::InvalidReceipt("base64 account decode"),
                )?;
                decode_voltr_withdrawal_receipt(address, account.owner, &account.data, bundle.vault)
            })
            .collect::<Result<Vec<_>, _>>()?;
    receipts.sort_by_key(|receipt| receipt.address);
    Ok((response.context.slot, receipts))
}

fn decode_vault_state(
    account: &Account,
    bundle: &BackyardVoltrRouteBundle,
) -> Result<(u64, Pubkey), VoltrObservationError> {
    if account.owner != VOLTR_VAULT_PROGRAM_ID
        || account.data.len() != VOLTR_VAULT_DATA_LENGTH
        || account.data[..8] != VOLTR_VAULT_DISCRIMINATOR
        || pubkey_at(&account.data, VOLTR_VAULT_ASSET_MINT_OFFSET)? != loyal_actions::USDC_MINT
        || pubkey_at(&account.data, VOLTR_VAULT_MANAGER_OFFSET)? != bundle.manager
        || u64_at(&account.data, VOLTR_VAULT_WITHDRAWAL_WAIT_OFFSET)?
            != BACKYARD_VOLTR_WITHDRAWAL_WAIT_SECONDS
    {
        return Err(VoltrObservationError::Rpc(
            "vault owner, layout, manager, asset, or withdrawal wait drifted".into(),
        ));
    }
    Ok((
        u64_at(&account.data, VOLTR_VAULT_TOTAL_VALUE_OFFSET)?,
        pubkey_at(&account.data, VOLTR_VAULT_IDLE_ATA_OFFSET)?,
    ))
}

fn decode_strategy_receipt(
    account: &Account,
    bundle: &BackyardVoltrRouteBundle,
    strategy: BackyardVoltrStrategy,
    expected_address: Pubkey,
) -> Result<VoltrObservedPosition, VoltrObservationError> {
    let template = bundle.template(
        strategy,
        loyal_actions::autonomous_vaults::BackyardVoltrManagerOperation::Deposit,
    );
    if account.owner != VOLTR_VAULT_PROGRAM_ID
        || account.data.len() != VOLTR_STRATEGY_RECEIPT_DATA_LENGTH
        || account.data[..8] != VOLTR_STRATEGY_RECEIPT_DISCRIMINATOR
        || pubkey_at(&account.data, 8)? != bundle.vault
        || pubkey_at(&account.data, 40)? != template.reserve
        || pubkey_at(&account.data, 72)? != VOLTR_KAMINO_ADAPTOR_PROGRAM_ID
        || account.data[120] != 1
        || account.data[123..].iter().any(|byte| *byte != 0)
        || template.strategy_init_receipt != expected_address
    {
        return Err(VoltrObservationError::InvalidPositionSet);
    }
    let value_raw = u64_at(&account.data, 104)?;
    Ok(VoltrObservedPosition {
        strategy,
        strategy_receipt: expected_address,
        value_raw,
        // The market observer applies reserve-liquidity capacity before a
        // withdrawal leg is admitted. This account-level ceiling prevents any
        // route from claiming more than the position itself.
        safely_redeemable_raw: value_raw,
        data_sha256: sha256_hex(&account.data),
    })
}

pub fn decode_voltr_withdrawal_receipt(
    address: Pubkey,
    owner: Pubkey,
    data: &[u8],
    expected_vault: Pubkey,
) -> Result<VoltrWithdrawalReceipt, VoltrObservationError> {
    if owner != VOLTR_VAULT_PROGRAM_ID
        || data.len() != VOLTR_WITHDRAWAL_RECEIPT_DATA_LENGTH
        || data[..8] != VOLTR_WITHDRAWAL_RECEIPT_DISCRIMINATOR
        || data[VOLTR_WITHDRAWAL_RECEIPT_PREFIX_LENGTH..]
            .iter()
            .any(|byte| *byte != 0)
    {
        return Err(VoltrObservationError::InvalidReceipt(
            "owner, length, discriminator, or trailing bytes",
        ));
    }
    let vault = pubkey_at(data, 8)?;
    let user = pubkey_at(data, 40)?;
    if vault != expected_vault {
        return Err(VoltrObservationError::InvalidReceipt("vault"));
    }
    let amount_lp_escrowed_raw = u64_at(data, 72)?;
    let amount_asset_to_withdraw_decimal_bits = u128_at(data, 80)?;
    let withdrawable_from_ts = u64_at(data, 96)?;
    let bump = data[104];
    let version = data[105];
    if amount_lp_escrowed_raw == 0
        || amount_asset_to_withdraw_decimal_bits == 0
        || withdrawable_from_ts == 0
        || version != 0
    {
        return Err(VoltrObservationError::InvalidReceipt(
            "amount, deadline, or version",
        ));
    }
    let scale = 1u128 << DECIMAL_FRACTION_BITS;
    let rounded = amount_asset_to_withdraw_decimal_bits
        .checked_add(scale - 1)
        .ok_or(VoltrObservationError::ArithmeticOverflow)?
        >> DECIMAL_FRACTION_BITS;
    let upper_bound_asset_raw =
        u64::try_from(rounded).map_err(|_| VoltrObservationError::ArithmeticOverflow)?;
    let data_sha256 = sha256_hex(data);
    let generation_fingerprint = sha256_hex(
        format!(
            "{address}:{vault}:{user}:{amount_lp_escrowed_raw}:{amount_asset_to_withdraw_decimal_bits}:{withdrawable_from_ts}:{bump}:{version}:{data_sha256}"
        )
        .as_bytes(),
    );
    Ok(VoltrWithdrawalReceipt {
        address,
        vault,
        user,
        amount_lp_escrowed_raw,
        amount_asset_to_withdraw_decimal_bits,
        upper_bound_asset_raw,
        withdrawable_from_ts,
        bump,
        version,
        data_sha256,
        generation_fingerprint,
    })
}

#[allow(clippy::too_many_arguments)]
pub fn validated_confirmed_voltr_observation(
    context_slot: u64,
    min_context_slot: u64,
    vault: Pubkey,
    vault_total_value_raw: u64,
    idle_raw: u64,
    mut positions: Vec<VoltrObservedPosition>,
    mut receipts: Vec<VoltrWithdrawalReceipt>,
    configured_safety_buffer_raw: u64,
) -> Result<ConfirmedVoltrObservation, VoltrObservationError> {
    if min_context_slot == 0 || context_slot < min_context_slot {
        return Err(VoltrObservationError::MixedOrStaleContext);
    }
    positions.sort_by_key(|position| position.strategy);
    if positions.len() != BackyardVoltrStrategy::ALL.len()
        || positions
            .iter()
            .zip(BackyardVoltrStrategy::ALL)
            .any(|(position, expected)| position.strategy != expected)
    {
        return Err(VoltrObservationError::InvalidPositionSet);
    }
    let position_total_raw = positions.iter().try_fold(0u64, |sum, position| {
        sum.checked_add(position.value_raw)
            .ok_or(VoltrObservationError::ArithmeticOverflow)
    })?;
    if idle_raw
        .checked_add(position_total_raw)
        .ok_or(VoltrObservationError::ArithmeticOverflow)?
        != vault_total_value_raw
    {
        return Err(VoltrObservationError::AccountingMismatch);
    }
    receipts.sort_by_key(|receipt| receipt.address);
    if receipts
        .windows(2)
        .any(|pair| pair[0].address == pair[1].address)
        || receipts.iter().any(|receipt| receipt.vault != vault)
    {
        return Err(VoltrObservationError::InvalidReceipt(
            "duplicate or wrong-vault receipt",
        ));
    }
    let pending_withdrawal_raw = receipts.iter().try_fold(0u64, |sum, receipt| {
        sum.checked_add(receipt.upper_bound_asset_raw)
            .ok_or(VoltrObservationError::ArithmeticOverflow)
    })?;
    let required_idle_raw = configured_safety_buffer_raw
        .checked_add(pending_withdrawal_raw)
        .ok_or(VoltrObservationError::ArithmeticOverflow)?;
    let idle_shortfall_raw = required_idle_raw.saturating_sub(idle_raw);
    let investable_idle_raw = idle_raw.saturating_sub(required_idle_raw);
    let receipt_set_fingerprint = sha256_hex(
        receipts
            .iter()
            .fold(format!("{vault}"), |mut canonical, receipt| {
                canonical.push(':');
                canonical.push_str(&receipt.generation_fingerprint);
                canonical
            })
            .as_bytes(),
    );
    let protected_state_sha256 = sha256_hex(
        format!(
            "{vault}:{context_slot}:{vault_total_value_raw}:{idle_raw}:{}:{}",
            positions
                .iter()
                .map(|position| format!(
                    "{}:{}:{}:{}",
                    position.strategy.as_str(),
                    position.strategy_receipt,
                    position.value_raw,
                    position.data_sha256
                ))
                .collect::<Vec<_>>()
                .join(":"),
            receipts
                .iter()
                .map(|receipt| receipt.generation_fingerprint.as_str())
                .collect::<Vec<_>>()
                .join(":")
        )
        .as_bytes(),
    );
    let mut protected_addresses = vec![vault.to_string()];
    protected_addresses.extend(
        positions
            .iter()
            .map(|position| position.strategy_receipt.to_string()),
    );
    protected_addresses.extend(receipts.iter().map(|receipt| receipt.address.to_string()));
    protected_addresses.sort();
    protected_addresses.dedup();
    let protected_address_set_sha256 = sha256_hex(protected_addresses.join(":").as_bytes());
    let _withdrawal_wait_seconds = BACKYARD_VOLTR_WITHDRAWAL_WAIT_SECONDS;
    Ok(ConfirmedVoltrObservation {
        context_slot,
        min_context_slot,
        vault,
        vault_total_value_raw,
        idle_raw,
        positions,
        receipts,
        receipt_set_fingerprint,
        protected_state_sha256,
        protected_address_set_sha256,
        configured_safety_buffer_raw,
        required_idle_raw,
        idle_shortfall_raw,
        investable_idle_raw,
    })
}

fn pubkey_at(data: &[u8], offset: usize) -> Result<Pubkey, VoltrObservationError> {
    let bytes: [u8; 32] = data
        .get(offset..offset + 32)
        .ok_or(VoltrObservationError::InvalidReceipt("truncated pubkey"))?
        .try_into()
        .map_err(|_| VoltrObservationError::InvalidReceipt("pubkey"))?;
    Ok(Pubkey::new_from_array(bytes))
}

fn u64_at(data: &[u8], offset: usize) -> Result<u64, VoltrObservationError> {
    let bytes: [u8; 8] = data
        .get(offset..offset + 8)
        .ok_or(VoltrObservationError::InvalidReceipt("truncated u64"))?
        .try_into()
        .map_err(|_| VoltrObservationError::InvalidReceipt("u64"))?;
    Ok(u64::from_le_bytes(bytes))
}

fn u128_at(data: &[u8], offset: usize) -> Result<u128, VoltrObservationError> {
    let bytes: [u8; 16] = data
        .get(offset..offset + 16)
        .ok_or(VoltrObservationError::InvalidReceipt("truncated u128"))?
        .try_into()
        .map_err(|_| VoltrObservationError::InvalidReceipt("u128"))?;
    Ok(u128::from_le_bytes(bytes))
}

fn sha256_hex(bytes: &[u8]) -> String {
    let digest = Sha256::digest(bytes);
    let mut encoded = String::with_capacity(digest.len() * 2);
    for byte in digest {
        encoded.push_str(&format!("{byte:02x}"));
    }
    encoded
}
