use crate::squads::{
    SquadsAccountConstraint, SquadsAccountConstraintType, SquadsDataConstraint, SquadsDataOperator,
    SquadsDataValue, SquadsInstructionConstraint,
};
use crate::{ids::*, LoyalActionError, Result};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey,
    pubkey::Pubkey,
};

use crate::actions::JupiterSwapContract;

fn validate_fee_bps(fee_bps: u16) -> Result<()> {
    if fee_bps > loyal_hub_abi::MAX_FEE_BPS as u16 {
        return Err(LoyalActionError::InvalidFeeBps);
    }
    Ok(())
}

fn validate_lane_count(lane_count: u8) -> Result<()> {
    if lane_count == 0 {
        return Err(LoyalActionError::InvalidLaneCount);
    }
    Ok(())
}

fn write_bytes(data: &mut [u8], offset: usize, bytes: &[u8]) {
    data[offset..offset + bytes.len()].copy_from_slice(bytes);
}

const SPL_TOKEN_ACCOUNT_AUTHORITY_OFFSET: u64 = 32;

pub(crate) fn kamino_redeem_reserve_collateral_constraint(
    vault: Pubkey,
    markets: Vec<Pubkey>,
    liquidity_mints: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![vault], None),
            pubkey_constraint(2, unique_pubkeys(markets), None),
            pubkey_constraint(5, unique_pubkeys(liquidity_mints), Some(spl_token::id())),
            spl_token_authority_constraint(9, vault),
            pubkey_constraint(11, vec![spl_token::id()], None),
            pubkey_constraint(12, vec![spl_token::id()], None),
        ],
        data_constraints: discriminator_constraint(KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR),
    }
}

pub(crate) fn kamino_redeem_reserve_collateral_market_mint_constraint(
    vault: Pubkey,
    markets: Vec<Pubkey>,
    liquidity_mints: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![vault], None),
            pubkey_constraint(2, unique_pubkeys(markets), None),
            pubkey_constraint(5, unique_pubkeys(liquidity_mints), None),
        ],
        data_constraints: discriminator_constraint(KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR),
    }
}

pub(crate) fn kamino_deposit_reserve_liquidity_constraint(
    vault: Pubkey,
    markets: Vec<Pubkey>,
    liquidity_mints: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![vault], None),
            pubkey_constraint(2, unique_pubkeys(markets), None),
            pubkey_constraint(5, unique_pubkeys(liquidity_mints), Some(spl_token::id())),
            spl_token_authority_constraint(9, vault),
            pubkey_constraint(11, vec![spl_token::id()], None),
            pubkey_constraint(12, vec![spl_token::id()], None),
        ],
        data_constraints: discriminator_constraint(KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR),
    }
}

pub(crate) fn kamino_deposit_reserve_liquidity_market_mint_constraint(
    vault: Pubkey,
    markets: Vec<Pubkey>,
    liquidity_mints: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![vault], None),
            pubkey_constraint(2, unique_pubkeys(markets), None),
            pubkey_constraint(5, unique_pubkeys(liquidity_mints), None),
        ],
        data_constraints: discriminator_constraint(KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR),
    }
}

pub(crate) fn kamino_init_obligation_constraint(
    vault: Pubkey,
    markets: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    let markets = unique_pubkeys(markets);
    let mut init_obligation_data_prefix = KAMINO_INIT_OBLIGATION_DISCRIMINATOR.to_vec();
    init_obligation_data_prefix.push(KAMINO_VANILLA_OBLIGATION_TAG);
    init_obligation_data_prefix.push(KAMINO_VANILLA_OBLIGATION_ID);

    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![vault], None),
            pubkey_constraint(1, vec![vault], None),
            pubkey_constraint(3, markets, None),
            pubkey_constraint(4, vec![Pubkey::default()], None),
            pubkey_constraint(5, vec![Pubkey::default()], None),
            pubkey_constraint(6, vec![derive_kamino_user_metadata(vault)], None),
            pubkey_constraint(7, vec![solana_sdk::sysvar::rent::id()], None),
            pubkey_constraint(8, vec![solana_sdk::system_program::ID], None),
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(init_obligation_data_prefix),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

pub(crate) fn kamino_refresh_obligation_constraint(
    vault: Pubkey,
    markets: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    let markets = unique_pubkeys(markets);
    let obligations = markets
        .iter()
        .map(|market| derive_kamino_vanilla_obligation(vault, *market))
        .collect();

    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, markets, None),
            pubkey_constraint(1, obligations, None),
        ],
        data_constraints: discriminator_constraint(KAMINO_REFRESH_OBLIGATION_DISCRIMINATOR),
    }
}

pub(crate) fn jupiter_constraint(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    contract: JupiterSwapContract,
) -> SquadsInstructionConstraint {
    let allowed_mints = unique_pubkeys(allowed_mints);
    let account_constraints = vec![
        pubkey_constraint(0, vec![vault], None),
        spl_token_authority_constraint(1, vault),
        spl_token_authority_constraint(2, vault),
        pubkey_constraint(3, allowed_mints.clone(), Some(spl_token::id())),
        pubkey_constraint(4, allowed_mints, Some(spl_token::id())),
        pubkey_constraint(5, vec![spl_token::id()], None),
    ];

    SquadsInstructionConstraint {
        program_id: contract.program_id,
        account_constraints,
        data_constraints: vec![
            SquadsDataConstraint {
                data_offset: 0,
                data_value: SquadsDataValue::U8Slice(contract.exact_in_discriminator.to_vec()),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: JUPITER_SWAP_SLIPPAGE_BPS_OFFSET,
                data_value: SquadsDataValue::U16Le(contract.max_slippage_bps),
                operator: SquadsDataOperator::LessThanOrEqualTo,
            },
        ],
    }
}

pub(crate) fn loyal_hub_constraint(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
) -> SquadsInstructionConstraint {
    let allowed_mints = unique_pubkeys(allowed_mints);

    SquadsInstructionConstraint {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(
                loyal_hub_abi::swap_exact_in_accounts::CONFIG,
                vec![derive_loyal_hub_config()],
                Some(LOYAL_HUB_SWAP_PROGRAM_ID),
            ),
            pubkey_constraint(
                loyal_hub_abi::swap_exact_in_accounts::USER_VAULT,
                vec![vault],
                None,
            ),
            account_data_constraint(loyal_hub_abi::swap_exact_in_accounts::HUB_INPUT, None),
            account_data_constraint(loyal_hub_abi::swap_exact_in_accounts::HUB_OUTPUT, None),
            token_authority_constraint(
                loyal_hub_abi::swap_exact_in_accounts::USER_INPUT,
                vault,
                None,
            ),
            token_authority_constraint(
                loyal_hub_abi::swap_exact_in_accounts::USER_OUTPUT,
                vault,
                None,
            ),
            pubkey_constraint(
                loyal_hub_abi::swap_exact_in_accounts::INPUT_MINT,
                allowed_mints.clone(),
                None,
            ),
            pubkey_constraint(
                loyal_hub_abi::swap_exact_in_accounts::OUTPUT_MINT,
                allowed_mints,
                None,
            ),
            pubkey_constraint(
                loyal_hub_abi::swap_exact_in_accounts::HUB_AUTHORIZER,
                vec![hub_authorizer],
                None,
            ),
            pubkey_constraint(
                loyal_hub_abi::swap_exact_in_accounts::TOKEN_PROGRAM,
                vec![spl_token::id()],
                None,
            ),
            pubkey_constraint(
                loyal_hub_abi::swap_exact_in_accounts::TOKEN_2022_PROGRAM,
                vec![TOKEN_2022_PROGRAM_ID],
                None,
            ),
        ],
        data_constraints: vec![
            SquadsDataConstraint {
                data_offset: LOYAL_HUB_SWAP_TAG_OFFSET,
                data_value: SquadsDataValue::U8(LOYAL_HUB_SWAP_EXACT_IN),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: LOYAL_HUB_SWAP_MAX_FEE_BPS_OFFSET,
                data_value: SquadsDataValue::U16Le(max_fee_bps),
                operator: SquadsDataOperator::LessThanOrEqualTo,
            },
        ],
    }
}

#[allow(dead_code)]
pub(crate) fn subscription_recurring_sweep_constraint(
    delegator: Pubkey,
    vault: Pubkey,
    delegator_usdc_ata: Pubkey,
    vault_usdc_ata: Pubkey,
    nonce: u64,
) -> SquadsInstructionConstraint {
    let subscription_authority = derive_subscription_authority(delegator, USDC_MINT);
    let delegation = derive_recurring_delegation(subscription_authority, delegator, vault, nonce);
    let event_authority = derive_subscription_event_authority();

    SquadsInstructionConstraint {
        program_id: SUBSCRIPTIONS_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![delegation], Some(SUBSCRIPTIONS_PROGRAM_ID)),
            pubkey_constraint(
                1,
                vec![subscription_authority],
                Some(SUBSCRIPTIONS_PROGRAM_ID),
            ),
            pubkey_constraint(2, vec![delegator_usdc_ata], Some(spl_token::id())),
            pubkey_constraint(3, vec![vault_usdc_ata], Some(spl_token::id())),
            pubkey_constraint(4, vec![USDC_MINT], Some(spl_token::id())),
            pubkey_constraint(5, vec![spl_token::id()], None),
            pubkey_constraint(6, vec![vault], None),
            pubkey_constraint(7, vec![event_authority], None),
            pubkey_constraint(8, vec![SUBSCRIPTIONS_PROGRAM_ID], None),
        ],
        data_constraints: vec![
            SquadsDataConstraint {
                data_offset: 0,
                data_value: SquadsDataValue::U8(SUBSCRIPTIONS_TRANSFER_RECURRING),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: SUBSCRIPTION_TRANSFER_DELEGATOR_OFFSET,
                data_value: SquadsDataValue::U8Slice(delegator.to_bytes().to_vec()),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: SUBSCRIPTION_TRANSFER_MINT_OFFSET,
                data_value: SquadsDataValue::U8Slice(USDC_MINT.to_bytes().to_vec()),
                operator: SquadsDataOperator::Equals,
            },
        ],
    }
}

pub(crate) fn pubkey_constraint(
    account_index: u8,
    pubkeys: Vec<Pubkey>,
    owner: Option<Pubkey>,
) -> SquadsAccountConstraint {
    SquadsAccountConstraint {
        account_index,
        account_constraint: SquadsAccountConstraintType::Pubkey(pubkeys),
        owner,
    }
}

pub(crate) fn account_data_constraint(
    account_index: u8,
    owner: Option<Pubkey>,
) -> SquadsAccountConstraint {
    SquadsAccountConstraint {
        account_index,
        account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
        owner,
    }
}

pub(crate) fn spl_token_authority_constraint(
    account_index: u8,
    authority: Pubkey,
) -> SquadsAccountConstraint {
    token_authority_constraint(account_index, authority, Some(spl_token::id()))
}

pub(crate) fn token_authority_constraint(
    account_index: u8,
    authority: Pubkey,
    owner: Option<Pubkey>,
) -> SquadsAccountConstraint {
    SquadsAccountConstraint {
        account_index,
        account_constraint: SquadsAccountConstraintType::AccountData(vec![SquadsDataConstraint {
            data_offset: SPL_TOKEN_ACCOUNT_AUTHORITY_OFFSET,
            data_value: SquadsDataValue::U8Slice(authority.to_bytes().to_vec()),
            operator: SquadsDataOperator::Equals,
        }]),
        owner,
    }
}

pub(crate) fn discriminator_constraint(discriminator: [u8; 8]) -> Vec<SquadsDataConstraint> {
    vec![SquadsDataConstraint {
        data_offset: 0,
        data_value: SquadsDataValue::U8Slice(discriminator.to_vec()),
        operator: SquadsDataOperator::Equals,
    }]
}

pub fn derive_loyal_hub_config() -> Pubkey {
    derive_loyal_hub_config_for_program(LOYAL_HUB_SWAP_PROGRAM_ID)
}

pub fn derive_loyal_hub_config_for_program(program_id: Pubkey) -> Pubkey {
    Pubkey::find_program_address(&[LOYAL_HUB_CONFIG_SEED], &program_id).0
}

pub fn derive_loyal_hub_pending_admin_for_program(program_id: Pubkey) -> Pubkey {
    Pubkey::find_program_address(&[LOYAL_HUB_PENDING_ADMIN_SEED], &program_id).0
}

pub fn derive_loyal_hub_pending_admin() -> Pubkey {
    derive_loyal_hub_pending_admin_for_program(LOYAL_HUB_SWAP_PROGRAM_ID)
}

pub fn derive_loyal_hub_authority() -> Pubkey {
    derive_loyal_hub_lane_authority(0)
}

pub fn derive_loyal_hub_authority_for_program(program_id: Pubkey) -> Pubkey {
    derive_loyal_hub_lane_authority_for_program(program_id, 0)
}

pub fn derive_kamino_vanilla_obligation(vault: Pubkey, lending_market: Pubkey) -> Pubkey {
    derive_kamino_obligation(
        vault,
        lending_market,
        KAMINO_VANILLA_OBLIGATION_TAG,
        KAMINO_VANILLA_OBLIGATION_ID,
        Pubkey::default(),
        Pubkey::default(),
    )
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KaminoInitObligationFarm {
    pub payer: Pubkey,
    pub owner: Pubkey,
    pub lending_market: Pubkey,
    pub reserve: Pubkey,
    pub reserve_farm_state: Pubkey,
}

pub fn kamino_init_obligation_farm_instruction(accounts: KaminoInitObligationFarm) -> Instruction {
    let obligation = derive_kamino_vanilla_obligation(accounts.owner, accounts.lending_market);
    let lending_market_authority = derive_kamino_lending_market_authority(accounts.lending_market);
    let obligation_farm =
        derive_kamino_obligation_farm_user_state(accounts.reserve_farm_state, obligation);
    let mut data = KAMINO_INIT_OBLIGATION_FARMS_FOR_RESERVE_DISCRIMINATOR.to_vec();
    data.push(KAMINO_COLLATERAL_FARM_MODE);

    Instruction {
        program_id: KAMINO_LEND_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(accounts.payer, true),
            AccountMeta::new_readonly(accounts.owner, false),
            AccountMeta::new(obligation, false),
            AccountMeta::new_readonly(lending_market_authority, false),
            AccountMeta::new(accounts.reserve, false),
            AccountMeta::new(accounts.reserve_farm_state, false),
            AccountMeta::new(obligation_farm, false),
            AccountMeta::new_readonly(accounts.lending_market, false),
            AccountMeta::new_readonly(KAMINO_FARMS_PROGRAM_ID, false),
            AccountMeta::new_readonly(solana_sdk::sysvar::rent::id(), false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data,
    }
}

pub fn derive_kamino_lending_market_authority(lending_market: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[
            KAMINO_LENDING_MARKET_AUTHORITY_SEED,
            lending_market.as_ref(),
        ],
        &KAMINO_LEND_PROGRAM_ID,
    )
    .0
}

pub fn derive_kamino_obligation(
    vault: Pubkey,
    lending_market: Pubkey,
    tag: u8,
    id: u8,
    seed1: Pubkey,
    seed2: Pubkey,
) -> Pubkey {
    Pubkey::find_program_address(
        &[
            &[tag],
            &[id],
            vault.as_ref(),
            lending_market.as_ref(),
            seed1.as_ref(),
            seed2.as_ref(),
        ],
        &KAMINO_LEND_PROGRAM_ID,
    )
    .0
}

pub fn derive_kamino_obligation_farm_user_state(
    reserve_farm_state: Pubkey,
    obligation: Pubkey,
) -> Pubkey {
    Pubkey::find_program_address(
        &[
            KAMINO_FARM_USER_STATE_SEED,
            reserve_farm_state.as_ref(),
            obligation.as_ref(),
        ],
        &KAMINO_FARMS_PROGRAM_ID,
    )
    .0
}

pub fn derive_kamino_user_metadata(vault: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[KAMINO_USER_METADATA_SEED, vault.as_ref()],
        &KAMINO_LEND_PROGRAM_ID,
    )
    .0
}

pub fn derive_subscription_authority(user: Pubkey, mint: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[SUBSCRIPTION_AUTHORITY_SEED, user.as_ref(), mint.as_ref()],
        &SUBSCRIPTIONS_PROGRAM_ID,
    )
    .0
}

pub fn derive_recurring_delegation(
    subscription_authority: Pubkey,
    delegator: Pubkey,
    delegatee: Pubkey,
    nonce: u64,
) -> Pubkey {
    Pubkey::find_program_address(
        &[
            SUBSCRIPTION_DELEGATION_SEED,
            subscription_authority.as_ref(),
            delegator.as_ref(),
            delegatee.as_ref(),
            &nonce.to_le_bytes(),
        ],
        &SUBSCRIPTIONS_PROGRAM_ID,
    )
    .0
}

pub fn derive_subscription_event_authority() -> Pubkey {
    Pubkey::find_program_address(
        &[SUBSCRIPTION_EVENT_AUTHORITY_SEED],
        &SUBSCRIPTIONS_PROGRAM_ID,
    )
    .0
}

pub fn subscription_init_authority_data() -> Vec<u8> {
    vec![SUBSCRIPTIONS_INIT_AUTHORITY]
}

pub fn subscription_create_recurring_delegation_data(
    nonce: u64,
    amount_per_period: u64,
    period_length_s: u64,
    start_ts: i64,
    expiry_ts: i64,
    expected_subscription_authority_init_id: i64,
) -> Vec<u8> {
    let mut data = Vec::with_capacity(49);
    data.push(SUBSCRIPTIONS_CREATE_RECURRING_DELEGATION);
    data.extend_from_slice(&nonce.to_le_bytes());
    data.extend_from_slice(&amount_per_period.to_le_bytes());
    data.extend_from_slice(&period_length_s.to_le_bytes());
    data.extend_from_slice(&start_ts.to_le_bytes());
    data.extend_from_slice(&expiry_ts.to_le_bytes());
    data.extend_from_slice(&expected_subscription_authority_init_id.to_le_bytes());
    data
}

pub fn subscription_transfer_recurring_data(
    amount: u64,
    delegator: Pubkey,
    mint: Pubkey,
) -> Vec<u8> {
    let mut data = Vec::with_capacity(73);
    data.push(SUBSCRIPTIONS_TRANSFER_RECURRING);
    data.extend_from_slice(&amount.to_le_bytes());
    data.extend_from_slice(delegator.as_ref());
    data.extend_from_slice(mint.as_ref());
    data
}

pub fn subscription_revoke_delegation_data() -> Vec<u8> {
    vec![SUBSCRIPTIONS_REVOKE_DELEGATION]
}

pub fn derive_loyal_hub_lane_authority(lane_id: u8) -> Pubkey {
    derive_loyal_hub_lane_authority_for_program(LOYAL_HUB_SWAP_PROGRAM_ID, lane_id)
}

pub fn derive_loyal_hub_lane_authority_for_program(program_id: Pubkey, lane_id: u8) -> Pubkey {
    Pubkey::find_program_address(&[LOYAL_HUB_AUTHORITY_SEED, &[lane_id]], &program_id).0
}

pub fn derive_loyal_hub_inventory_account(mint: Pubkey) -> Pubkey {
    derive_loyal_hub_lane_inventory_account(mint, 0)
}

pub fn derive_loyal_hub_inventory_account_for_program(program_id: Pubkey, mint: Pubkey) -> Pubkey {
    derive_loyal_hub_lane_inventory_account_for_program(program_id, mint, 0)
}

pub fn derive_loyal_hub_inventory_account_for_token_program(
    program_id: Pubkey,
    mint: Pubkey,
    token_program_id: Pubkey,
) -> Pubkey {
    derive_loyal_hub_lane_inventory_account_for_token_program(program_id, mint, 0, token_program_id)
}

pub fn derive_loyal_hub_lane_inventory_account(mint: Pubkey, lane_id: u8) -> Pubkey {
    derive_loyal_hub_lane_inventory_account_for_program(LOYAL_HUB_SWAP_PROGRAM_ID, mint, lane_id)
}

pub fn derive_loyal_hub_lane_inventory_account_for_program(
    program_id: Pubkey,
    mint: Pubkey,
    lane_id: u8,
) -> Pubkey {
    derive_loyal_hub_lane_inventory_account_for_token_program(
        program_id,
        mint,
        lane_id,
        spl_token::id(),
    )
}

pub fn derive_loyal_hub_lane_inventory_account_for_token_program(
    program_id: Pubkey,
    mint: Pubkey,
    lane_id: u8,
    token_program_id: Pubkey,
) -> Pubkey {
    let hub_authority = derive_loyal_hub_lane_authority_for_program(program_id, lane_id);
    Pubkey::find_program_address(
        &[
            hub_authority.as_ref(),
            token_program_id.as_ref(),
            mint.as_ref(),
        ],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct LoyalHubSwapExactIn {
    pub amount_in: u64,
    pub amount_out: u64,
    pub min_out: u64,
    pub max_fee_bps: u16,
    pub lane_id: u8,
}

pub fn loyal_hub_config_data(
    admin: Pubkey,
    hub_authorizer: Pubkey,
    inventory_rebalancer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    lane_count: u8,
    allowed_mints: &[Pubkey],
) -> Result<Vec<u8>> {
    validate_fee_bps(max_fee_bps)?;
    validate_lane_count(lane_count)?;
    if allowed_mints.is_empty() {
        return Err(LoyalActionError::EmptyAllowedMints);
    }
    if allowed_mints.len() > LOYAL_HUB_MAX_ALLOWED_MINTS {
        return Err(LoyalActionError::InvalidAllowedMintCount);
    }

    let mut data = vec![
        0;
        loyal_hub_abi::config_init::FIXED_LEN
            + (allowed_mints.len()
                * loyal_hub_abi::config_init::ALLOWED_MINT_ITEM_LEN)
    ];
    write_bytes(
        &mut data,
        loyal_hub_abi::config_init::ADMIN_OFFSET,
        admin.as_ref(),
    );
    write_bytes(
        &mut data,
        loyal_hub_abi::config_init::HUB_AUTHORIZER_OFFSET,
        hub_authorizer.as_ref(),
    );
    write_bytes(
        &mut data,
        loyal_hub_abi::config_init::INVENTORY_REBALANCER_OFFSET,
        inventory_rebalancer.as_ref(),
    );
    write_bytes(
        &mut data,
        loyal_hub_abi::config_init::MAX_FEE_BPS_OFFSET,
        &max_fee_bps.to_le_bytes(),
    );
    data[loyal_hub_abi::config_init::PAUSED_OFFSET] = u8::from(paused);
    data[loyal_hub_abi::config_init::LANE_COUNT_OFFSET] = lane_count;
    data[loyal_hub_abi::config_init::MINT_COUNT_OFFSET] = allowed_mints
        .len()
        .try_into()
        .map_err(|_| LoyalActionError::InvalidAllowedMintCount)?;
    for (index, mint) in allowed_mints.iter().enumerate() {
        let offset = loyal_hub_abi::config_init::ALLOWED_MINT_OFFSET
            + (index * loyal_hub_abi::config_init::ALLOWED_MINT_ITEM_LEN);
        write_bytes(&mut data, offset, mint.as_ref());
    }
    Ok(data)
}

pub fn loyal_hub_initialize_config_data(
    admin: Pubkey,
    hub_authorizer: Pubkey,
    inventory_rebalancer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    lane_count: u8,
    allowed_mints: &[Pubkey],
) -> Result<Vec<u8>> {
    let config_data = loyal_hub_config_data(
        admin,
        hub_authorizer,
        inventory_rebalancer,
        max_fee_bps,
        paused,
        lane_count,
        allowed_mints,
    )?;
    let mut data = vec![0; loyal_hub_abi::INITIALIZE_CONFIG_ARGS_OFFSET + config_data.len()];
    data[loyal_hub_abi::INITIALIZE_CONFIG_TAG_OFFSET as usize] = LOYAL_HUB_INITIALIZE_CONFIG;
    write_bytes(
        &mut data,
        loyal_hub_abi::INITIALIZE_CONFIG_ARGS_OFFSET,
        &config_data,
    );
    Ok(data)
}

pub fn loyal_hub_initialize_config_instruction(
    payer: Pubkey,
    admin: Pubkey,
    hub_authorizer: Pubkey,
    inventory_rebalancer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    lane_count: u8,
    allowed_mints: &[Pubkey],
) -> Result<Instruction> {
    loyal_hub_initialize_config_instruction_for_program(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        payer,
        admin,
        hub_authorizer,
        inventory_rebalancer,
        max_fee_bps,
        paused,
        lane_count,
        allowed_mints,
    )
}

pub fn loyal_hub_initialize_config_instruction_for_program(
    program_id: Pubkey,
    payer: Pubkey,
    admin: Pubkey,
    hub_authorizer: Pubkey,
    inventory_rebalancer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    lane_count: u8,
    allowed_mints: &[Pubkey],
) -> Result<Instruction> {
    if payer != admin {
        return Err(LoyalActionError::InvalidHubAdmin);
    }

    Ok(Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(payer, true),
            AccountMeta::new(derive_loyal_hub_config_for_program(program_id), false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data: loyal_hub_initialize_config_data(
            admin,
            hub_authorizer,
            inventory_rebalancer,
            max_fee_bps,
            paused,
            lane_count,
            allowed_mints,
        )?,
    })
}

pub fn loyal_hub_set_max_fee_data(max_fee_bps: u16) -> Result<Vec<u8>> {
    validate_fee_bps(max_fee_bps)?;
    let mut data = vec![0; LOYAL_HUB_SET_MAX_FEE_DATA_LEN];
    data[loyal_hub_abi::SET_MAX_FEE_TAG_OFFSET as usize] = LOYAL_HUB_SET_MAX_FEE;
    write_bytes(
        &mut data,
        loyal_hub_abi::SET_MAX_FEE_MAX_FEE_BPS_DATA_OFFSET as usize,
        &max_fee_bps.to_le_bytes(),
    );
    Ok(data)
}

pub fn loyal_hub_set_max_fee_instruction(admin: Pubkey, max_fee_bps: u16) -> Result<Instruction> {
    loyal_hub_set_max_fee_instruction_for_program(LOYAL_HUB_SWAP_PROGRAM_ID, admin, max_fee_bps)
}

pub fn loyal_hub_set_max_fee_instruction_for_program(
    program_id: Pubkey,
    admin: Pubkey,
    max_fee_bps: u16,
) -> Result<Instruction> {
    Ok(Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config_for_program(program_id), false),
            AccountMeta::new_readonly(admin, true),
        ],
        data: loyal_hub_set_max_fee_data(max_fee_bps)?,
    })
}

pub fn loyal_hub_set_paused_data(paused: bool) -> Vec<u8> {
    let mut data = vec![0; LOYAL_HUB_SET_PAUSED_DATA_LEN];
    data[loyal_hub_abi::SET_PAUSED_TAG_OFFSET as usize] = LOYAL_HUB_SET_PAUSED;
    data[loyal_hub_abi::SET_PAUSED_PAUSED_DATA_OFFSET as usize] = u8::from(paused);
    data
}

pub fn loyal_hub_set_paused_instruction(admin: Pubkey, paused: bool) -> Instruction {
    loyal_hub_set_paused_instruction_for_program(LOYAL_HUB_SWAP_PROGRAM_ID, admin, paused)
}

pub fn loyal_hub_set_paused_instruction_for_program(
    program_id: Pubkey,
    admin: Pubkey,
    paused: bool,
) -> Instruction {
    Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config_for_program(program_id), false),
            AccountMeta::new_readonly(admin, true),
        ],
        data: loyal_hub_set_paused_data(paused),
    }
}

pub fn loyal_hub_set_admin_data() -> Vec<u8> {
    let mut data = vec![0; LOYAL_HUB_SET_ADMIN_DATA_LEN];
    data[loyal_hub_abi::SET_ADMIN_TAG_OFFSET as usize] = LOYAL_HUB_SET_ADMIN;
    data
}

pub fn loyal_hub_set_admin_instruction(admin: Pubkey, new_admin: Pubkey) -> Instruction {
    loyal_hub_set_admin_instruction_for_program(LOYAL_HUB_SWAP_PROGRAM_ID, admin, new_admin)
}

pub fn loyal_hub_set_admin_instruction_for_program(
    program_id: Pubkey,
    admin: Pubkey,
    new_admin: Pubkey,
) -> Instruction {
    Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config_for_program(program_id), false),
            AccountMeta::new_readonly(admin, true),
            AccountMeta::new_readonly(new_admin, true),
        ],
        data: loyal_hub_set_admin_data(),
    }
}

pub fn loyal_hub_request_admin_transfer_data() -> Vec<u8> {
    let mut data = vec![0; LOYAL_HUB_REQUEST_ADMIN_TRANSFER_DATA_LEN];
    data[loyal_hub_abi::REQUEST_ADMIN_TRANSFER_TAG_OFFSET as usize] =
        LOYAL_HUB_REQUEST_ADMIN_TRANSFER;
    data
}

pub fn loyal_hub_request_admin_transfer_instruction(
    admin: Pubkey,
    new_admin: Pubkey,
) -> Instruction {
    loyal_hub_request_admin_transfer_instruction_for_program(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        admin,
        new_admin,
    )
}

pub fn loyal_hub_request_admin_transfer_instruction_for_program(
    program_id: Pubkey,
    admin: Pubkey,
    new_admin: Pubkey,
) -> Instruction {
    Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config_for_program(program_id), false),
            AccountMeta::new_readonly(admin, true),
            AccountMeta::new(
                derive_loyal_hub_pending_admin_for_program(program_id),
                false,
            ),
            AccountMeta::new_readonly(new_admin, false),
            AccountMeta::new_readonly(pubkey!("11111111111111111111111111111111"), false),
        ],
        data: loyal_hub_request_admin_transfer_data(),
    }
}

pub fn loyal_hub_accept_admin_transfer_data() -> Vec<u8> {
    let mut data = vec![0; LOYAL_HUB_ACCEPT_ADMIN_TRANSFER_DATA_LEN];
    data[loyal_hub_abi::ACCEPT_ADMIN_TRANSFER_TAG_OFFSET as usize] =
        LOYAL_HUB_ACCEPT_ADMIN_TRANSFER;
    data
}

pub fn loyal_hub_accept_admin_transfer_instruction(new_admin: Pubkey) -> Instruction {
    loyal_hub_accept_admin_transfer_instruction_for_program(LOYAL_HUB_SWAP_PROGRAM_ID, new_admin)
}

pub fn loyal_hub_accept_admin_transfer_instruction_for_program(
    program_id: Pubkey,
    new_admin: Pubkey,
) -> Instruction {
    Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config_for_program(program_id), false),
            AccountMeta::new_readonly(
                derive_loyal_hub_pending_admin_for_program(program_id),
                false,
            ),
            AccountMeta::new_readonly(new_admin, true),
        ],
        data: loyal_hub_accept_admin_transfer_data(),
    }
}

pub fn loyal_hub_set_hub_authorizer_data() -> Vec<u8> {
    let mut data = vec![0; LOYAL_HUB_SET_HUB_AUTHORIZER_DATA_LEN];
    data[loyal_hub_abi::SET_HUB_AUTHORIZER_TAG_OFFSET as usize] = LOYAL_HUB_SET_HUB_AUTHORIZER;
    data
}

pub fn loyal_hub_set_hub_authorizer_instruction(
    admin: Pubkey,
    new_hub_authorizer: Pubkey,
) -> Instruction {
    loyal_hub_set_hub_authorizer_instruction_for_program(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        admin,
        new_hub_authorizer,
    )
}

pub fn loyal_hub_set_hub_authorizer_instruction_for_program(
    program_id: Pubkey,
    admin: Pubkey,
    new_hub_authorizer: Pubkey,
) -> Instruction {
    Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config_for_program(program_id), false),
            AccountMeta::new_readonly(admin, true),
            AccountMeta::new_readonly(new_hub_authorizer, false),
        ],
        data: loyal_hub_set_hub_authorizer_data(),
    }
}

pub fn loyal_hub_set_inventory_rebalancer_data() -> Vec<u8> {
    let mut data = vec![0; LOYAL_HUB_SET_INVENTORY_REBALANCER_DATA_LEN];
    data[loyal_hub_abi::SET_INVENTORY_REBALANCER_TAG_OFFSET as usize] =
        LOYAL_HUB_SET_INVENTORY_REBALANCER;
    data
}

pub fn loyal_hub_set_inventory_rebalancer_instruction(
    admin: Pubkey,
    new_inventory_rebalancer: Pubkey,
) -> Instruction {
    loyal_hub_set_inventory_rebalancer_instruction_for_program(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        admin,
        new_inventory_rebalancer,
    )
}

pub fn loyal_hub_set_inventory_rebalancer_instruction_for_program(
    program_id: Pubkey,
    admin: Pubkey,
    new_inventory_rebalancer: Pubkey,
) -> Instruction {
    Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config_for_program(program_id), false),
            AccountMeta::new_readonly(admin, true),
            AccountMeta::new_readonly(new_inventory_rebalancer, false),
        ],
        data: loyal_hub_set_inventory_rebalancer_data(),
    }
}

pub fn loyal_hub_set_lane_count_data(lane_count: u8) -> Result<Vec<u8>> {
    validate_lane_count(lane_count)?;
    let mut data = vec![0; LOYAL_HUB_SET_LANE_COUNT_DATA_LEN];
    data[loyal_hub_abi::SET_LANE_COUNT_TAG_OFFSET as usize] = LOYAL_HUB_SET_LANE_COUNT;
    data[loyal_hub_abi::SET_LANE_COUNT_LANE_COUNT_DATA_OFFSET as usize] = lane_count;
    Ok(data)
}

pub fn loyal_hub_set_lane_count_instruction(admin: Pubkey, lane_count: u8) -> Result<Instruction> {
    loyal_hub_set_lane_count_instruction_for_program(LOYAL_HUB_SWAP_PROGRAM_ID, admin, lane_count)
}

pub fn loyal_hub_set_lane_count_instruction_for_program(
    program_id: Pubkey,
    admin: Pubkey,
    lane_count: u8,
) -> Result<Instruction> {
    Ok(Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config_for_program(program_id), false),
            AccountMeta::new_readonly(admin, true),
        ],
        data: loyal_hub_set_lane_count_data(lane_count)?,
    })
}

pub fn loyal_hub_withdraw_inventory_data(amount: u64, lane_id: u8) -> Vec<u8> {
    let mut data = vec![0; LOYAL_HUB_WITHDRAW_INVENTORY_DATA_LEN];
    data[loyal_hub_abi::WITHDRAW_INVENTORY_TAG_OFFSET as usize] = LOYAL_HUB_WITHDRAW_INVENTORY;
    write_bytes(
        &mut data,
        loyal_hub_abi::WITHDRAW_INVENTORY_AMOUNT_DATA_OFFSET as usize,
        &amount.to_le_bytes(),
    );
    data[loyal_hub_abi::WITHDRAW_INVENTORY_LANE_ID_DATA_OFFSET as usize] = lane_id;
    data
}

pub fn loyal_hub_withdraw_inventory_instruction(
    admin: Pubkey,
    destination: Pubkey,
    mint: Pubkey,
    amount: u64,
    lane_id: u8,
) -> Instruction {
    loyal_hub_withdraw_inventory_instruction_for_program(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        admin,
        destination,
        mint,
        amount,
        lane_id,
    )
}

pub fn loyal_hub_withdraw_inventory_instruction_for_program(
    program_id: Pubkey,
    admin: Pubkey,
    destination: Pubkey,
    mint: Pubkey,
    amount: u64,
    lane_id: u8,
) -> Instruction {
    loyal_hub_withdraw_inventory_instruction_for_program_with_token_program(
        program_id,
        admin,
        destination,
        mint,
        spl_token::id(),
        amount,
        lane_id,
    )
}

pub fn loyal_hub_withdraw_inventory_instruction_with_token_program(
    admin: Pubkey,
    destination: Pubkey,
    mint: Pubkey,
    token_program: Pubkey,
    amount: u64,
    lane_id: u8,
) -> Instruction {
    loyal_hub_withdraw_inventory_instruction_for_program_with_token_program(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        admin,
        destination,
        mint,
        token_program,
        amount,
        lane_id,
    )
}

pub fn loyal_hub_withdraw_inventory_instruction_for_program_with_token_program(
    program_id: Pubkey,
    admin: Pubkey,
    destination: Pubkey,
    mint: Pubkey,
    token_program: Pubkey,
    amount: u64,
    lane_id: u8,
) -> Instruction {
    loyal_hub_withdraw_inventory_instruction_with_source_for_program_with_token_program(
        program_id,
        admin,
        derive_loyal_hub_lane_inventory_account_for_token_program(
            program_id,
            mint,
            lane_id,
            token_program,
        ),
        destination,
        mint,
        token_program,
        amount,
        lane_id,
    )
}

pub fn loyal_hub_withdraw_inventory_instruction_with_source_for_program(
    program_id: Pubkey,
    admin: Pubkey,
    hub_source: Pubkey,
    destination: Pubkey,
    mint: Pubkey,
    amount: u64,
    lane_id: u8,
) -> Instruction {
    loyal_hub_withdraw_inventory_instruction_with_source_for_program_with_token_program(
        program_id,
        admin,
        hub_source,
        destination,
        mint,
        spl_token::id(),
        amount,
        lane_id,
    )
}

pub fn loyal_hub_withdraw_inventory_instruction_with_source_for_program_with_token_program(
    program_id: Pubkey,
    admin: Pubkey,
    hub_source: Pubkey,
    destination: Pubkey,
    mint: Pubkey,
    token_program: Pubkey,
    amount: u64,
    lane_id: u8,
) -> Instruction {
    Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config_for_program(program_id), false),
            AccountMeta::new_readonly(admin, true),
            AccountMeta::new(hub_source, false),
            AccountMeta::new(destination, false),
            AccountMeta::new_readonly(mint, false),
            AccountMeta::new_readonly(
                derive_loyal_hub_lane_authority_for_program(program_id, lane_id),
                false,
            ),
            AccountMeta::new_readonly(token_program, false),
        ],
        data: loyal_hub_withdraw_inventory_data(amount, lane_id),
    }
}

pub fn loyal_hub_withdraw_inventory_instruction_with_source(
    admin: Pubkey,
    hub_source: Pubkey,
    destination: Pubkey,
    mint: Pubkey,
    amount: u64,
    lane_id: u8,
) -> Instruction {
    loyal_hub_withdraw_inventory_instruction_with_source_for_program(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        admin,
        hub_source,
        destination,
        mint,
        amount,
        lane_id,
    )
}

pub fn loyal_hub_withdraw_inventory_instruction_with_source_with_token_program(
    admin: Pubkey,
    hub_source: Pubkey,
    destination: Pubkey,
    mint: Pubkey,
    token_program: Pubkey,
    amount: u64,
    lane_id: u8,
) -> Instruction {
    loyal_hub_withdraw_inventory_instruction_with_source_for_program_with_token_program(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        admin,
        hub_source,
        destination,
        mint,
        token_program,
        amount,
        lane_id,
    )
}

pub fn loyal_hub_swap_exact_in_data(args: LoyalHubSwapExactIn) -> Vec<u8> {
    let mut data = vec![0; LOYAL_HUB_SWAP_EXACT_IN_DATA_LEN];
    data[LOYAL_HUB_SWAP_TAG_OFFSET as usize] = LOYAL_HUB_SWAP_EXACT_IN;
    write_bytes(
        &mut data,
        loyal_hub_abi::SWAP_EXACT_IN_AMOUNT_IN_DATA_OFFSET as usize,
        &args.amount_in.to_le_bytes(),
    );
    write_bytes(
        &mut data,
        loyal_hub_abi::SWAP_EXACT_IN_AMOUNT_OUT_DATA_OFFSET as usize,
        &args.amount_out.to_le_bytes(),
    );
    write_bytes(
        &mut data,
        loyal_hub_abi::SWAP_EXACT_IN_MIN_OUT_DATA_OFFSET as usize,
        &args.min_out.to_le_bytes(),
    );
    write_bytes(
        &mut data,
        LOYAL_HUB_SWAP_MAX_FEE_BPS_OFFSET as usize,
        &args.max_fee_bps.to_le_bytes(),
    );
    data[loyal_hub_abi::SWAP_EXACT_IN_LANE_ID_DATA_OFFSET as usize] = args.lane_id;
    data
}

pub fn loyal_hub_swap_exact_in_instruction(
    user_vault: Pubkey,
    user_input: Pubkey,
    user_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    hub_authorizer: Pubkey,
    args: LoyalHubSwapExactIn,
) -> Instruction {
    loyal_hub_swap_exact_in_instruction_with_token_programs(
        user_vault,
        user_input,
        user_output,
        input_mint,
        output_mint,
        hub_authorizer,
        spl_token::id(),
        spl_token::id(),
        args,
    )
}

pub fn loyal_hub_swap_exact_in_instruction_with_token_programs(
    user_vault: Pubkey,
    user_input: Pubkey,
    user_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    hub_authorizer: Pubkey,
    input_token_program: Pubkey,
    output_token_program: Pubkey,
    args: LoyalHubSwapExactIn,
) -> Instruction {
    loyal_hub_swap_exact_in_instruction_for_program_with_token_programs(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        user_vault,
        user_input,
        user_output,
        input_mint,
        output_mint,
        hub_authorizer,
        input_token_program,
        output_token_program,
        args,
    )
}

pub fn loyal_hub_swap_exact_in_instruction_for_program(
    program_id: Pubkey,
    user_vault: Pubkey,
    user_input: Pubkey,
    user_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    hub_authorizer: Pubkey,
    args: LoyalHubSwapExactIn,
) -> Instruction {
    loyal_hub_swap_exact_in_instruction_for_program_with_token_programs(
        program_id,
        user_vault,
        user_input,
        user_output,
        input_mint,
        output_mint,
        hub_authorizer,
        spl_token::id(),
        spl_token::id(),
        args,
    )
}

pub fn loyal_hub_swap_exact_in_instruction_for_program_with_token_programs(
    program_id: Pubkey,
    user_vault: Pubkey,
    user_input: Pubkey,
    user_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    hub_authorizer: Pubkey,
    input_token_program: Pubkey,
    output_token_program: Pubkey,
    args: LoyalHubSwapExactIn,
) -> Instruction {
    Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new_readonly(derive_loyal_hub_config_for_program(program_id), false),
            AccountMeta::new_readonly(user_vault, true),
            AccountMeta::new(user_input, false),
            AccountMeta::new(user_output, false),
            AccountMeta::new(
                derive_loyal_hub_lane_inventory_account_for_token_program(
                    program_id,
                    input_mint,
                    args.lane_id,
                    input_token_program,
                ),
                false,
            ),
            AccountMeta::new(
                derive_loyal_hub_lane_inventory_account_for_token_program(
                    program_id,
                    output_mint,
                    args.lane_id,
                    output_token_program,
                ),
                false,
            ),
            AccountMeta::new_readonly(input_mint, false),
            AccountMeta::new_readonly(output_mint, false),
            AccountMeta::new_readonly(
                derive_loyal_hub_lane_authority_for_program(program_id, args.lane_id),
                false,
            ),
            AccountMeta::new_readonly(hub_authorizer, true),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(TOKEN_2022_PROGRAM_ID, false),
        ],
        data: loyal_hub_swap_exact_in_data(args),
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct LoyalHubLaneRebalanceTransfer {
    pub from_lane_id: u8,
    pub to_lane_id: u8,
    pub amount: u64,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct LoyalHubRebalanceTransfer {
    pub mint: Pubkey,
    pub from_lane_id: u8,
    pub to_lane_id: u8,
    pub amount: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct LoyalHubMintRebalanceBatch {
    pub mint: Pubkey,
    pub transfers: Vec<LoyalHubLaneRebalanceTransfer>,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct LoyalHubRebalanceBuilder;

pub fn hub_rebalance() -> LoyalHubRebalanceBuilder {
    LoyalHubRebalanceBuilder
}

impl LoyalHubRebalanceBuilder {
    pub fn batch(
        self,
        transfers: impl IntoIterator<Item = LoyalHubRebalanceTransfer>,
    ) -> Result<Vec<LoyalHubMintRebalanceBatch>> {
        group_loyal_hub_rebalance_transfers(transfers)
    }

    pub fn instructions(
        self,
        inventory_rebalancer: Pubkey,
        transfers: impl IntoIterator<Item = LoyalHubRebalanceTransfer>,
    ) -> Result<Vec<Instruction>> {
        self.instructions_for_program(LOYAL_HUB_SWAP_PROGRAM_ID, inventory_rebalancer, transfers)
    }

    pub fn instructions_for_program(
        self,
        program_id: Pubkey,
        inventory_rebalancer: Pubkey,
        transfers: impl IntoIterator<Item = LoyalHubRebalanceTransfer>,
    ) -> Result<Vec<Instruction>> {
        group_loyal_hub_rebalance_transfers(transfers)?
            .into_iter()
            .map(|batch| {
                loyal_hub_rebalance_inventory_instruction_for_program(
                    program_id,
                    inventory_rebalancer,
                    batch.mint,
                    &batch.transfers,
                )
            })
            .collect()
    }
}

pub fn group_loyal_hub_rebalance_transfers(
    transfers: impl IntoIterator<Item = LoyalHubRebalanceTransfer>,
) -> Result<Vec<LoyalHubMintRebalanceBatch>> {
    let mut batches: Vec<LoyalHubMintRebalanceBatch> = Vec::new();
    for transfer in transfers {
        let lane_transfer = LoyalHubLaneRebalanceTransfer {
            from_lane_id: transfer.from_lane_id,
            to_lane_id: transfer.to_lane_id,
            amount: transfer.amount,
        };
        if lane_transfer.amount == 0 {
            return Err(LoyalActionError::InvalidRebalanceTransferCount);
        }
        match batches.iter_mut().find(|batch| batch.mint == transfer.mint) {
            Some(batch) => batch.transfers.push(lane_transfer),
            None => batches.push(LoyalHubMintRebalanceBatch {
                mint: transfer.mint,
                transfers: vec![lane_transfer],
            }),
        }
    }

    if batches.is_empty() {
        return Err(LoyalActionError::EmptyRebalanceTransfers);
    }
    if batches
        .iter()
        .any(|batch| batch.transfers.len() > LOYAL_HUB_MAX_REBALANCE_TRANSFERS)
    {
        return Err(LoyalActionError::InvalidRebalanceTransferCount);
    }

    Ok(batches)
}

pub fn loyal_hub_rebalance_inventory_instruction(
    inventory_rebalancer: Pubkey,
    mint: Pubkey,
    transfers: &[LoyalHubLaneRebalanceTransfer],
) -> Result<Instruction> {
    loyal_hub_rebalance_inventory_instruction_for_program(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        inventory_rebalancer,
        mint,
        transfers,
    )
}

pub fn loyal_hub_rebalance_inventory_instruction_for_program(
    program_id: Pubkey,
    inventory_rebalancer: Pubkey,
    mint: Pubkey,
    transfers: &[LoyalHubLaneRebalanceTransfer],
) -> Result<Instruction> {
    loyal_hub_rebalance_inventory_instruction_for_program_with_token_program(
        program_id,
        inventory_rebalancer,
        mint,
        spl_token::id(),
        transfers,
    )
}

pub fn loyal_hub_rebalance_inventory_instruction_with_token_program(
    inventory_rebalancer: Pubkey,
    mint: Pubkey,
    token_program: Pubkey,
    transfers: &[LoyalHubLaneRebalanceTransfer],
) -> Result<Instruction> {
    loyal_hub_rebalance_inventory_instruction_for_program_with_token_program(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        inventory_rebalancer,
        mint,
        token_program,
        transfers,
    )
}

pub fn loyal_hub_rebalance_inventory_instruction_for_program_with_token_program(
    program_id: Pubkey,
    inventory_rebalancer: Pubkey,
    mint: Pubkey,
    token_program: Pubkey,
    transfers: &[LoyalHubLaneRebalanceTransfer],
) -> Result<Instruction> {
    let mut accounts = vec![
        AccountMeta::new_readonly(derive_loyal_hub_config_for_program(program_id), false),
        AccountMeta::new_readonly(inventory_rebalancer, true),
        AccountMeta::new_readonly(token_program, false),
        AccountMeta::new_readonly(mint, false),
    ];
    for transfer in transfers {
        accounts.push(AccountMeta::new_readonly(
            derive_loyal_hub_lane_authority_for_program(program_id, transfer.from_lane_id),
            false,
        ));
        accounts.push(AccountMeta::new(
            derive_loyal_hub_lane_inventory_account_for_token_program(
                program_id,
                mint,
                transfer.from_lane_id,
                token_program,
            ),
            false,
        ));
        accounts.push(AccountMeta::new(
            derive_loyal_hub_lane_inventory_account_for_token_program(
                program_id,
                mint,
                transfer.to_lane_id,
                token_program,
            ),
            false,
        ));
    }

    Ok(Instruction {
        program_id,
        accounts,
        data: loyal_hub_rebalance_inventory_data(transfers)?,
    })
}

pub fn loyal_hub_rebalance_inventory_data(
    transfers: &[LoyalHubLaneRebalanceTransfer],
) -> Result<Vec<u8>> {
    if transfers.is_empty() || transfers.len() > LOYAL_HUB_MAX_REBALANCE_TRANSFERS {
        return Err(LoyalActionError::InvalidRebalanceTransferCount);
    }
    let mut data = vec![
        0;
        LOYAL_HUB_REBALANCE_INVENTORY_ARGS_OFFSET
            + loyal_hub_abi::rebalance_inventory_args::FIXED_LEN
            + (transfers.len()
                * loyal_hub_abi::rebalance_inventory_args::TRANSFER_ITEM_LEN)
    ];
    data[loyal_hub_abi::REBALANCE_INVENTORY_TAG_OFFSET as usize] = LOYAL_HUB_REBALANCE_INVENTORY;
    data[loyal_hub_abi::REBALANCE_INVENTORY_TRANSFER_COUNT_DATA_OFFSET as usize] = transfers
        .len()
        .try_into()
        .map_err(|_| LoyalActionError::InvalidRebalanceTransferCount)?;
    for (index, transfer) in transfers.iter().enumerate() {
        if transfer.amount == 0 {
            return Err(LoyalActionError::InvalidRebalanceTransferCount);
        }
        let offset = loyal_hub_abi::REBALANCE_INVENTORY_TRANSFER_DATA_OFFSET as usize
            + (index * loyal_hub_abi::rebalance_inventory_args::TRANSFER_ITEM_LEN);
        data[offset + loyal_hub_abi::rebalance_transfer::FROM_LANE_ID_OFFSET] =
            transfer.from_lane_id;
        data[offset + loyal_hub_abi::rebalance_transfer::TO_LANE_ID_OFFSET] = transfer.to_lane_id;
        write_bytes(
            &mut data,
            offset + loyal_hub_abi::rebalance_transfer::AMOUNT_OFFSET,
            &transfer.amount.to_le_bytes(),
        );
    }
    Ok(data)
}

pub(crate) fn unique_pubkeys(pubkeys: Vec<Pubkey>) -> Vec<Pubkey> {
    let mut unique = Vec::new();
    for pubkey in pubkeys {
        if !unique.contains(&pubkey) {
            unique.push(pubkey);
        }
    }
    unique
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hub_rebalance_batches_global_lane_transfers_by_mint() {
        let usdc = Pubkey::new_unique();
        let pyusd = Pubkey::new_unique();

        let batches = hub_rebalance()
            .batch([
                LoyalHubRebalanceTransfer {
                    mint: usdc,
                    from_lane_id: 0,
                    to_lane_id: 1,
                    amount: 10,
                },
                LoyalHubRebalanceTransfer {
                    mint: pyusd,
                    from_lane_id: 1,
                    to_lane_id: 2,
                    amount: 20,
                },
                LoyalHubRebalanceTransfer {
                    mint: usdc,
                    from_lane_id: 2,
                    to_lane_id: 0,
                    amount: 30,
                },
            ])
            .unwrap();

        assert_eq!(batches.len(), 2);
        assert_eq!(batches[0].mint, usdc);
        assert_eq!(batches[0].transfers.len(), 2);
        assert_eq!(batches[1].mint, pyusd);
        assert_eq!(batches[1].transfers.len(), 1);
    }

    #[test]
    fn loyal_hub_initialize_data_and_accounts_match_wire_abi() {
        let payer = Pubkey::new_unique();
        let admin = payer;
        let hub_authorizer = Pubkey::new_unique();
        let inventory_rebalancer = Pubkey::new_unique();
        let mint_a = Pubkey::new_unique();
        let mint_b = Pubkey::new_unique();

        let ix = loyal_hub_initialize_config_instruction(
            payer,
            admin,
            hub_authorizer,
            inventory_rebalancer,
            50,
            true,
            3,
            &[mint_a, mint_b],
        )
        .unwrap();

        assert_eq!(ix.program_id, LOYAL_HUB_SWAP_PROGRAM_ID);
        assert_eq!(ix.accounts.len(), 3);
        assert_eq!(ix.accounts[0], AccountMeta::new(payer, true));
        assert_eq!(
            ix.accounts[1],
            AccountMeta::new(derive_loyal_hub_config(), false)
        );
        assert_eq!(
            ix.accounts[2],
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false)
        );
        assert_eq!(
            ix.data.len(),
            LOYAL_HUB_INITIALIZE_CONFIG_DATA_LEN
                - (LOYAL_HUB_MAX_ALLOWED_MINTS - 2)
                    * loyal_hub_abi::config_init::ALLOWED_MINT_ITEM_LEN
        );
        assert_eq!(ix.data[0], LOYAL_HUB_INITIALIZE_CONFIG);
        assert_eq!(
            &ix.data[loyal_hub_abi::INITIALIZE_CONFIG_ADMIN_DATA_OFFSET as usize
                ..loyal_hub_abi::INITIALIZE_CONFIG_ADMIN_DATA_OFFSET as usize
                    + loyal_hub_abi::config_init::ADMIN_LEN],
            admin.as_ref()
        );
        assert_eq!(
            &ix.data[loyal_hub_abi::INITIALIZE_CONFIG_HUB_AUTHORIZER_DATA_OFFSET as usize
                ..loyal_hub_abi::INITIALIZE_CONFIG_HUB_AUTHORIZER_DATA_OFFSET as usize
                    + loyal_hub_abi::config_init::HUB_AUTHORIZER_LEN],
            hub_authorizer.as_ref()
        );
        assert_eq!(
            &ix.data[loyal_hub_abi::INITIALIZE_CONFIG_INVENTORY_REBALANCER_DATA_OFFSET as usize
                ..loyal_hub_abi::INITIALIZE_CONFIG_INVENTORY_REBALANCER_DATA_OFFSET as usize
                    + loyal_hub_abi::config_init::INVENTORY_REBALANCER_LEN],
            inventory_rebalancer.as_ref()
        );
        assert_eq!(
            &ix.data[loyal_hub_abi::INITIALIZE_CONFIG_MAX_FEE_BPS_DATA_OFFSET as usize
                ..loyal_hub_abi::INITIALIZE_CONFIG_MAX_FEE_BPS_DATA_OFFSET as usize
                    + loyal_hub_abi::config_init::MAX_FEE_BPS_LEN],
            &50u16.to_le_bytes()
        );
        assert_eq!(
            ix.data[loyal_hub_abi::INITIALIZE_CONFIG_PAUSED_DATA_OFFSET as usize],
            1
        );
        assert_eq!(
            ix.data[loyal_hub_abi::INITIALIZE_CONFIG_LANE_COUNT_DATA_OFFSET as usize],
            3
        );
        assert_eq!(
            ix.data[loyal_hub_abi::INITIALIZE_CONFIG_MINT_COUNT_DATA_OFFSET as usize],
            2
        );
        let mint_offset = loyal_hub_abi::INITIALIZE_CONFIG_ALLOWED_MINT_DATA_OFFSET as usize;
        assert_eq!(
            &ix.data[mint_offset..mint_offset + loyal_hub_abi::config_init::ALLOWED_MINT_ITEM_LEN],
            mint_a.as_ref()
        );
        assert_eq!(
            &ix.data[mint_offset + loyal_hub_abi::config_init::ALLOWED_MINT_ITEM_LEN
                ..mint_offset + (2 * loyal_hub_abi::config_init::ALLOWED_MINT_ITEM_LEN)],
            mint_b.as_ref()
        );
    }

    #[test]
    fn loyal_hub_initialize_rejects_non_admin_payer() {
        let payer = Pubkey::new_unique();
        let admin = Pubkey::new_unique();
        let hub_authorizer = Pubkey::new_unique();
        let inventory_rebalancer = Pubkey::new_unique();
        let mint_a = Pubkey::new_unique();
        let mint_b = Pubkey::new_unique();

        let result = loyal_hub_initialize_config_instruction(
            payer,
            admin,
            hub_authorizer,
            inventory_rebalancer,
            50,
            true,
            3,
            &[mint_a, mint_b],
        );

        assert_eq!(result, Err(LoyalActionError::InvalidHubAdmin));
    }

    #[test]
    fn loyal_hub_swap_instruction_uses_canonical_accounts_and_offsets() {
        let user_vault = Pubkey::new_unique();
        let user_input = Pubkey::new_unique();
        let user_output = Pubkey::new_unique();
        let input_mint = Pubkey::new_unique();
        let output_mint = Pubkey::new_unique();
        let hub_authorizer = Pubkey::new_unique();
        let args = LoyalHubSwapExactIn {
            amount_in: 10,
            amount_out: 9,
            min_out: 8,
            max_fee_bps: 50,
            lane_id: 2,
        };

        let ix = loyal_hub_swap_exact_in_instruction(
            user_vault,
            user_input,
            user_output,
            input_mint,
            output_mint,
            hub_authorizer,
            args,
        );

        assert_eq!(ix.program_id, LOYAL_HUB_SWAP_PROGRAM_ID);
        assert_eq!(ix.accounts.len(), 12);
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::CONFIG as usize],
            AccountMeta::new_readonly(derive_loyal_hub_config(), false)
        );
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::USER_VAULT as usize],
            AccountMeta::new_readonly(user_vault, true)
        );
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::USER_INPUT as usize],
            AccountMeta::new(user_input, false)
        );
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::USER_OUTPUT as usize],
            AccountMeta::new(user_output, false)
        );
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::HUB_INPUT as usize],
            AccountMeta::new(
                derive_loyal_hub_lane_inventory_account(input_mint, 2),
                false
            )
        );
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::HUB_OUTPUT as usize],
            AccountMeta::new(
                derive_loyal_hub_lane_inventory_account(output_mint, 2),
                false
            )
        );
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::HUB_AUTHORITY as usize],
            AccountMeta::new_readonly(derive_loyal_hub_lane_authority(2), false)
        );
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::HUB_AUTHORIZER as usize],
            AccountMeta::new_readonly(hub_authorizer, true)
        );
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::TOKEN_PROGRAM as usize],
            AccountMeta::new_readonly(spl_token::id(), false)
        );
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::TOKEN_2022_PROGRAM as usize],
            AccountMeta::new_readonly(TOKEN_2022_PROGRAM_ID, false)
        );
        assert_eq!(ix.data.len(), LOYAL_HUB_SWAP_EXACT_IN_DATA_LEN);
        assert_eq!(
            ix.data[LOYAL_HUB_SWAP_TAG_OFFSET as usize],
            LOYAL_HUB_SWAP_EXACT_IN
        );
        assert_eq!(
            &ix.data[LOYAL_HUB_SWAP_MAX_FEE_BPS_OFFSET as usize
                ..LOYAL_HUB_SWAP_MAX_FEE_BPS_OFFSET as usize + 2],
            &50u16.to_le_bytes()
        );
    }

    #[test]
    fn loyal_hub_swap_instruction_can_derive_token_2022_inventory_accounts() {
        let user_vault = Pubkey::new_unique();
        let user_input = Pubkey::new_unique();
        let user_output = Pubkey::new_unique();
        let input_mint = Pubkey::new_unique();
        let output_mint = Pubkey::new_unique();
        let hub_authorizer = Pubkey::new_unique();
        let args = LoyalHubSwapExactIn {
            amount_in: 10,
            amount_out: 9,
            min_out: 8,
            max_fee_bps: 50,
            lane_id: 2,
        };

        let ix = loyal_hub_swap_exact_in_instruction_with_token_programs(
            user_vault,
            user_input,
            user_output,
            input_mint,
            output_mint,
            hub_authorizer,
            TOKEN_2022_PROGRAM_ID,
            spl_token::id(),
            args,
        );

        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::HUB_INPUT as usize],
            AccountMeta::new(
                derive_loyal_hub_lane_inventory_account_for_token_program(
                    LOYAL_HUB_SWAP_PROGRAM_ID,
                    input_mint,
                    2,
                    TOKEN_2022_PROGRAM_ID,
                ),
                false,
            )
        );
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::HUB_OUTPUT as usize],
            AccountMeta::new(
                derive_loyal_hub_lane_inventory_account(output_mint, 2),
                false,
            )
        );
        assert_eq!(
            ix.accounts[loyal_hub_abi::swap_exact_in_accounts::TOKEN_2022_PROGRAM as usize],
            AccountMeta::new_readonly(TOKEN_2022_PROGRAM_ID, false)
        );
    }

    #[test]
    fn loyal_hub_admin_and_rebalance_builders_match_account_order() {
        let admin = Pubkey::new_unique();
        let new_admin = Pubkey::new_unique();
        let new_hub_authorizer = Pubkey::new_unique();
        let new_rebalancer = Pubkey::new_unique();
        let destination = Pubkey::new_unique();
        let mint = Pubkey::new_unique();
        let rebalancer = Pubkey::new_unique();

        let set_fee = loyal_hub_set_max_fee_instruction(admin, 25).unwrap();
        assert_eq!(
            set_fee.accounts[loyal_hub_abi::set_max_fee_accounts::CONFIG as usize],
            AccountMeta::new(derive_loyal_hub_config(), false)
        );
        assert_eq!(
            set_fee.accounts[loyal_hub_abi::set_max_fee_accounts::ADMIN as usize],
            AccountMeta::new_readonly(admin, true)
        );
        assert_eq!(set_fee.data, vec![LOYAL_HUB_SET_MAX_FEE, 25, 0]);

        let set_admin = loyal_hub_set_admin_instruction(admin, new_admin);
        assert_eq!(
            set_admin.accounts[loyal_hub_abi::set_admin_accounts::CONFIG as usize],
            AccountMeta::new(derive_loyal_hub_config(), false)
        );
        assert_eq!(
            set_admin.accounts[loyal_hub_abi::set_admin_accounts::ADMIN as usize],
            AccountMeta::new_readonly(admin, true)
        );
        assert_eq!(
            set_admin.accounts[loyal_hub_abi::set_admin_accounts::NEW_ADMIN as usize],
            AccountMeta::new_readonly(new_admin, true)
        );
        assert_eq!(set_admin.data, vec![LOYAL_HUB_SET_ADMIN]);

        let set_hub_authorizer =
            loyal_hub_set_hub_authorizer_instruction(admin, new_hub_authorizer);
        assert_eq!(
            set_hub_authorizer.accounts
                [loyal_hub_abi::set_hub_authorizer_accounts::NEW_HUB_AUTHORIZER as usize],
            AccountMeta::new_readonly(new_hub_authorizer, false)
        );
        assert_eq!(set_hub_authorizer.data, vec![LOYAL_HUB_SET_HUB_AUTHORIZER]);

        let set_rebalancer = loyal_hub_set_inventory_rebalancer_instruction(admin, new_rebalancer);
        assert_eq!(
            set_rebalancer.accounts
                [loyal_hub_abi::set_inventory_rebalancer_accounts::NEW_INVENTORY_REBALANCER
                    as usize],
            AccountMeta::new_readonly(new_rebalancer, false)
        );
        assert_eq!(
            set_rebalancer.data,
            vec![LOYAL_HUB_SET_INVENTORY_REBALANCER]
        );

        let set_lane_count = loyal_hub_set_lane_count_instruction(admin, 8).unwrap();
        assert_eq!(
            set_lane_count.accounts[loyal_hub_abi::set_lane_count_accounts::ADMIN as usize],
            AccountMeta::new_readonly(admin, true)
        );
        assert_eq!(set_lane_count.data, vec![LOYAL_HUB_SET_LANE_COUNT, 8]);

        let withdraw = loyal_hub_withdraw_inventory_instruction(admin, destination, mint, 42, 1);
        assert_eq!(withdraw.accounts.len(), 7);
        assert_eq!(
            withdraw.accounts[loyal_hub_abi::withdraw_inventory_accounts::HUB_SOURCE as usize],
            AccountMeta::new(derive_loyal_hub_lane_inventory_account(mint, 1), false)
        );
        assert_eq!(
            withdraw.accounts[loyal_hub_abi::withdraw_inventory_accounts::HUB_AUTHORITY as usize],
            AccountMeta::new_readonly(derive_loyal_hub_lane_authority(1), false)
        );
        assert_eq!(
            withdraw.data[loyal_hub_abi::WITHDRAW_INVENTORY_TAG_OFFSET as usize],
            LOYAL_HUB_WITHDRAW_INVENTORY
        );
        let token_2022_withdraw = loyal_hub_withdraw_inventory_instruction_with_token_program(
            admin,
            destination,
            mint,
            TOKEN_2022_PROGRAM_ID,
            42,
            1,
        );
        assert_eq!(
            token_2022_withdraw.accounts
                [loyal_hub_abi::withdraw_inventory_accounts::HUB_SOURCE as usize],
            AccountMeta::new(
                derive_loyal_hub_lane_inventory_account_for_token_program(
                    LOYAL_HUB_SWAP_PROGRAM_ID,
                    mint,
                    1,
                    TOKEN_2022_PROGRAM_ID,
                ),
                false,
            )
        );
        assert_eq!(
            token_2022_withdraw.accounts
                [loyal_hub_abi::withdraw_inventory_accounts::TOKEN_PROGRAM as usize],
            AccountMeta::new_readonly(TOKEN_2022_PROGRAM_ID, false)
        );

        let rebalance = loyal_hub_rebalance_inventory_instruction(
            rebalancer,
            mint,
            &[LoyalHubLaneRebalanceTransfer {
                from_lane_id: 1,
                to_lane_id: 2,
                amount: 55,
            }],
        )
        .unwrap();
        assert_eq!(rebalance.accounts.len(), 7);
        assert_eq!(
            rebalance.accounts[loyal_hub_abi::rebalance_inventory_accounts::CONFIG as usize],
            AccountMeta::new_readonly(derive_loyal_hub_config(), false)
        );
        assert_eq!(
            rebalance.accounts
                [loyal_hub_abi::rebalance_inventory_accounts::INVENTORY_REBALANCER as usize],
            AccountMeta::new_readonly(rebalancer, true)
        );
        let transfer_start =
            loyal_hub_abi::rebalance_inventory_accounts::TRANSFER_ACCOUNTS_START as usize;
        assert_eq!(
            rebalance.accounts[transfer_start
                + loyal_hub_abi::rebalance_inventory_accounts::SOURCE_AUTHORITY_RELATIVE as usize],
            AccountMeta::new_readonly(derive_loyal_hub_lane_authority(1), false)
        );
        assert_eq!(
            rebalance.accounts[transfer_start
                + loyal_hub_abi::rebalance_inventory_accounts::SOURCE_INVENTORY_RELATIVE as usize],
            AccountMeta::new(derive_loyal_hub_lane_inventory_account(mint, 1), false)
        );
        assert_eq!(
            rebalance.accounts[transfer_start
                + loyal_hub_abi::rebalance_inventory_accounts::DESTINATION_INVENTORY_RELATIVE
                    as usize],
            AccountMeta::new(derive_loyal_hub_lane_inventory_account(mint, 2), false)
        );
        let token_2022_rebalance = loyal_hub_rebalance_inventory_instruction_with_token_program(
            rebalancer,
            mint,
            TOKEN_2022_PROGRAM_ID,
            &[LoyalHubLaneRebalanceTransfer {
                from_lane_id: 1,
                to_lane_id: 2,
                amount: 55,
            }],
        )
        .unwrap();
        assert_eq!(
            token_2022_rebalance.accounts
                [loyal_hub_abi::rebalance_inventory_accounts::TOKEN_PROGRAM as usize],
            AccountMeta::new_readonly(TOKEN_2022_PROGRAM_ID, false)
        );
        assert_eq!(
            token_2022_rebalance.accounts[transfer_start
                + loyal_hub_abi::rebalance_inventory_accounts::SOURCE_INVENTORY_RELATIVE as usize],
            AccountMeta::new(
                derive_loyal_hub_lane_inventory_account_for_token_program(
                    LOYAL_HUB_SWAP_PROGRAM_ID,
                    mint,
                    1,
                    TOKEN_2022_PROGRAM_ID,
                ),
                false,
            )
        );
        assert_eq!(
            token_2022_rebalance.accounts[transfer_start
                + loyal_hub_abi::rebalance_inventory_accounts::DESTINATION_INVENTORY_RELATIVE
                    as usize],
            AccountMeta::new(
                derive_loyal_hub_lane_inventory_account_for_token_program(
                    LOYAL_HUB_SWAP_PROGRAM_ID,
                    mint,
                    2,
                    TOKEN_2022_PROGRAM_ID,
                ),
                false,
            )
        );
    }

    #[test]
    fn hub_rebalance_enforces_empty_zero_and_transfer_cap() {
        let mint = Pubkey::new_unique();
        assert_eq!(
            hub_rebalance().batch([]),
            Err(LoyalActionError::EmptyRebalanceTransfers)
        );
        assert_eq!(
            hub_rebalance().batch([LoyalHubRebalanceTransfer {
                mint,
                from_lane_id: 0,
                to_lane_id: 1,
                amount: 0,
            }]),
            Err(LoyalActionError::InvalidRebalanceTransferCount)
        );

        let too_many = (0..=LOYAL_HUB_MAX_REBALANCE_TRANSFERS).map(|_| LoyalHubRebalanceTransfer {
            mint,
            from_lane_id: 0,
            to_lane_id: 1,
            amount: 1,
        });
        assert_eq!(
            hub_rebalance().batch(too_many),
            Err(LoyalActionError::InvalidRebalanceTransferCount)
        );
    }

    #[test]
    fn kamino_route_constraints_anchor_expected_indexes_and_discriminators() {
        let vault = Pubkey::new_unique();
        let market = Pubkey::new_unique();
        let liquidity_mint = Pubkey::new_unique();

        let withdraw =
            kamino_redeem_reserve_collateral_constraint(vault, vec![market], vec![liquidity_mint]);
        assert_eq!(withdraw.program_id, KAMINO_LEND_PROGRAM_ID);
        assert!(has_pubkey_constraint(&withdraw, 0, &[vault], None));
        assert!(has_pubkey_constraint(&withdraw, 2, &[market], None));
        assert!(has_pubkey_constraint(
            &withdraw,
            5,
            &[liquidity_mint],
            Some(spl_token::id()),
        ));
        assert!(has_token_authority_constraint(&withdraw, 9, vault));
        assert!(has_pubkey_constraint(
            &withdraw,
            11,
            &[spl_token::id()],
            None
        ));
        assert!(has_pubkey_constraint(
            &withdraw,
            12,
            &[spl_token::id()],
            None
        ));
        assert!(has_u8_slice_data_constraint(
            &withdraw.data_constraints,
            0,
            &KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
            SquadsDataOperator::Equals,
        ));

        let deposit =
            kamino_deposit_reserve_liquidity_constraint(vault, vec![market], vec![liquidity_mint]);
        assert_eq!(deposit.program_id, KAMINO_LEND_PROGRAM_ID);
        assert!(has_pubkey_constraint(&deposit, 0, &[vault], None));
        assert!(has_pubkey_constraint(&deposit, 2, &[market], None));
        assert!(has_pubkey_constraint(
            &deposit,
            5,
            &[liquidity_mint],
            Some(spl_token::id()),
        ));
        assert!(has_token_authority_constraint(&deposit, 9, vault));
        assert!(has_pubkey_constraint(
            &deposit,
            11,
            &[spl_token::id()],
            None
        ));
        assert!(has_pubkey_constraint(
            &deposit,
            12,
            &[spl_token::id()],
            None
        ));
        assert!(has_u8_slice_data_constraint(
            &deposit.data_constraints,
            0,
            &KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
            SquadsDataOperator::Equals,
        ));
    }

    #[test]
    fn kamino_same_mint_constraints_keep_market_and_mint_scope() {
        let vault = Pubkey::new_unique();
        let market = Pubkey::new_unique();
        let other_market = Pubkey::new_unique();
        let mint = Pubkey::new_unique();
        let markets = vec![market, other_market];
        let mints = vec![mint];

        let withdraw = kamino_redeem_reserve_collateral_market_mint_constraint(
            vault,
            markets.clone(),
            mints.clone(),
        );
        assert_eq!(withdraw.program_id, KAMINO_LEND_PROGRAM_ID);
        assert!(has_pubkey_constraint(&withdraw, 0, &[vault], None));
        assert!(has_pubkey_constraint(&withdraw, 2, &markets, None));
        assert!(has_pubkey_constraint(&withdraw, 5, &mints, None));
        assert!(has_u8_slice_data_constraint(
            &withdraw.data_constraints,
            0,
            &KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
            SquadsDataOperator::Equals,
        ));

        let deposit = kamino_deposit_reserve_liquidity_market_mint_constraint(
            vault,
            markets.clone(),
            mints.clone(),
        );
        assert_eq!(deposit.program_id, KAMINO_LEND_PROGRAM_ID);
        assert!(has_pubkey_constraint(&deposit, 0, &[vault], None));
        assert!(has_pubkey_constraint(&deposit, 2, &markets, None));
        assert!(has_pubkey_constraint(&deposit, 5, &mints, None));
        assert!(has_u8_slice_data_constraint(
            &deposit.data_constraints,
            0,
            &KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
            SquadsDataOperator::Equals,
        ));
    }

    #[test]
    fn kamino_init_obligation_constraint_anchors_vault_market_and_seeds() {
        let vault = Pubkey::new_unique();
        let market = Pubkey::new_unique();
        let other_market = Pubkey::new_unique();
        let constraint = kamino_init_obligation_constraint(vault, vec![market, other_market]);

        assert_eq!(constraint.program_id, KAMINO_LEND_PROGRAM_ID);
        assert!(has_pubkey_constraint(&constraint, 0, &[vault], None));
        assert!(has_pubkey_constraint(&constraint, 1, &[vault], None));
        assert!(has_pubkey_constraint(
            &constraint,
            3,
            &[market, other_market],
            None,
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            4,
            &[Pubkey::default()],
            None,
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            5,
            &[Pubkey::default()],
            None,
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            6,
            &[derive_kamino_user_metadata(vault)],
            None,
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            7,
            &[solana_sdk::sysvar::rent::id()],
            None,
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            8,
            &[solana_sdk::system_program::ID],
            None,
        ));
        let mut expected_data_prefix = KAMINO_INIT_OBLIGATION_DISCRIMINATOR.to_vec();
        expected_data_prefix.push(KAMINO_VANILLA_OBLIGATION_TAG);
        expected_data_prefix.push(KAMINO_VANILLA_OBLIGATION_ID);
        assert!(has_u8_slice_data_constraint(
            &constraint.data_constraints,
            0,
            &expected_data_prefix,
            SquadsDataOperator::Equals,
        ));
    }

    #[test]
    fn jupiter_route_constraint_anchors_program_mints_token_program_and_output() {
        let vault = Pubkey::new_unique();
        let usdc = Pubkey::new_unique();
        let pyusd = Pubkey::new_unique();
        let constraint = jupiter_constraint(
            vault,
            vec![usdc, pyusd],
            JupiterSwapContract {
                program_id: JUPITER_V6_PROGRAM_ID,
                exact_in_discriminator: JUPITER_SWAP_DISCRIMINATOR,
                max_slippage_bps: JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
            },
        );

        assert_eq!(constraint.program_id, JUPITER_V6_PROGRAM_ID);
        assert!(has_pubkey_constraint(&constraint, 0, &[vault], None));
        assert!(has_token_authority_constraint(&constraint, 1, vault));
        assert!(has_token_authority_constraint(&constraint, 2, vault));
        assert!(has_pubkey_constraint(
            &constraint,
            3,
            &[usdc, pyusd],
            Some(spl_token::id()),
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            4,
            &[usdc, pyusd],
            Some(spl_token::id()),
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            5,
            &[spl_token::id()],
            None
        ));
        assert_eq!(constraint.account_constraints.len(), 6);
        assert!(has_u8_slice_data_constraint(
            &constraint.data_constraints,
            0,
            &JUPITER_SWAP_DISCRIMINATOR,
            SquadsDataOperator::Equals,
        ));
        assert!(has_u16_data_constraint(
            &constraint.data_constraints,
            JUPITER_SWAP_SLIPPAGE_BPS_OFFSET,
            JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
            SquadsDataOperator::LessThanOrEqualTo,
        ));
    }

    #[test]
    fn loyal_hub_swap_policy_uses_canonical_abi_offsets_and_accounts() {
        let vault = Pubkey::new_unique();
        let usdc = Pubkey::new_unique();
        let pyusd = Pubkey::new_unique();
        let authorizer = Pubkey::new_unique();
        let max_fee_bps = 50;
        let constraint = loyal_hub_constraint(vault, vec![usdc, pyusd], authorizer, max_fee_bps);

        assert_eq!(constraint.program_id, LOYAL_HUB_SWAP_PROGRAM_ID);
        assert!(has_pubkey_constraint(
            &constraint,
            loyal_hub_abi::swap_exact_in_accounts::CONFIG,
            &[derive_loyal_hub_config()],
            Some(LOYAL_HUB_SWAP_PROGRAM_ID),
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            loyal_hub_abi::swap_exact_in_accounts::USER_VAULT,
            &[vault],
            None,
        ));
        assert!(has_account_data_constraint(
            &constraint,
            loyal_hub_abi::swap_exact_in_accounts::HUB_INPUT,
            None,
        ));
        assert!(has_account_data_constraint(
            &constraint,
            loyal_hub_abi::swap_exact_in_accounts::HUB_OUTPUT,
            None,
        ));
        assert!(has_token_authority_constraint_with_owner(
            &constraint,
            loyal_hub_abi::swap_exact_in_accounts::USER_INPUT,
            vault,
            None,
        ));
        assert!(has_token_authority_constraint_with_owner(
            &constraint,
            loyal_hub_abi::swap_exact_in_accounts::USER_OUTPUT,
            vault,
            None,
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            loyal_hub_abi::swap_exact_in_accounts::INPUT_MINT,
            &[usdc, pyusd],
            None,
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            loyal_hub_abi::swap_exact_in_accounts::OUTPUT_MINT,
            &[usdc, pyusd],
            None,
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            loyal_hub_abi::swap_exact_in_accounts::HUB_AUTHORIZER,
            &[authorizer],
            None,
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            loyal_hub_abi::swap_exact_in_accounts::TOKEN_PROGRAM,
            &[spl_token::id()],
            None,
        ));
        assert!(has_pubkey_constraint(
            &constraint,
            loyal_hub_abi::swap_exact_in_accounts::TOKEN_2022_PROGRAM,
            &[TOKEN_2022_PROGRAM_ID],
            None,
        ));
        assert!(has_u8_data_constraint(
            &constraint.data_constraints,
            LOYAL_HUB_SWAP_TAG_OFFSET,
            LOYAL_HUB_SWAP_EXACT_IN,
            SquadsDataOperator::Equals,
        ));
        assert!(has_u16_data_constraint(
            &constraint.data_constraints,
            LOYAL_HUB_SWAP_MAX_FEE_BPS_OFFSET,
            max_fee_bps,
            SquadsDataOperator::LessThanOrEqualTo,
        ));
    }

    fn has_token_authority_constraint(
        constraint: &SquadsInstructionConstraint,
        account_index: u8,
        authority: Pubkey,
    ) -> bool {
        has_token_authority_constraint_with_owner(
            constraint,
            account_index,
            authority,
            Some(spl_token::id()),
        )
    }

    fn has_token_authority_constraint_with_owner(
        constraint: &SquadsInstructionConstraint,
        account_index: u8,
        authority: Pubkey,
        expected_owner: Option<Pubkey>,
    ) -> bool {
        constraint.account_constraints.iter().any(|account| {
            account.account_index == account_index
                && account.owner == expected_owner
                && matches!(
                    &account.account_constraint,
                    SquadsAccountConstraintType::AccountData(data_constraints)
                        if data_constraints.iter().any(|data| {
                            data.data_offset == SPL_TOKEN_ACCOUNT_AUTHORITY_OFFSET
                                && matches!(
                                    &data.data_value,
                                    SquadsDataValue::U8Slice(bytes)
                                        if bytes.as_slice() == authority.as_ref()
                                )
                                && matches!(&data.operator, SquadsDataOperator::Equals)
                        })
                )
        })
    }

    fn has_pubkey_constraint(
        constraint: &SquadsInstructionConstraint,
        account_index: u8,
        expected_pubkeys: &[Pubkey],
        expected_owner: Option<Pubkey>,
    ) -> bool {
        constraint.account_constraints.iter().any(|account| {
            account.account_index == account_index
                && account.owner == expected_owner
                && matches!(
                    &account.account_constraint,
                    SquadsAccountConstraintType::Pubkey(pubkeys)
                        if pubkeys.as_slice() == expected_pubkeys
                )
        })
    }

    fn has_account_data_constraint(
        constraint: &SquadsInstructionConstraint,
        account_index: u8,
        expected_owner: Option<Pubkey>,
    ) -> bool {
        constraint.account_constraints.iter().any(|account| {
            account.account_index == account_index
                && account.owner == expected_owner
                && matches!(
                    &account.account_constraint,
                    SquadsAccountConstraintType::AccountData(data_constraints)
                        if data_constraints.is_empty()
                )
        })
    }

    fn has_u8_data_constraint(
        constraints: &[SquadsDataConstraint],
        data_offset: u64,
        expected: u8,
        expected_operator: SquadsDataOperator,
    ) -> bool {
        constraints.iter().any(|data| {
            data.data_offset == data_offset
                && matches!(&data.data_value, SquadsDataValue::U8(value) if *value == expected)
                && same_operator(&data.operator, &expected_operator)
        })
    }

    fn has_u16_data_constraint(
        constraints: &[SquadsDataConstraint],
        data_offset: u64,
        expected: u16,
        expected_operator: SquadsDataOperator,
    ) -> bool {
        constraints.iter().any(|data| {
            data.data_offset == data_offset
                && matches!(&data.data_value, SquadsDataValue::U16Le(value) if *value == expected)
                && same_operator(&data.operator, &expected_operator)
        })
    }

    fn has_u8_slice_data_constraint(
        constraints: &[SquadsDataConstraint],
        data_offset: u64,
        expected: &[u8],
        expected_operator: SquadsDataOperator,
    ) -> bool {
        constraints.iter().any(|data| {
            data.data_offset == data_offset
                && matches!(
                    &data.data_value,
                    SquadsDataValue::U8Slice(bytes) if bytes.as_slice() == expected
                )
                && same_operator(&data.operator, &expected_operator)
        })
    }

    fn same_operator(left: &SquadsDataOperator, right: &SquadsDataOperator) -> bool {
        std::mem::discriminant(left) == std::mem::discriminant(right)
    }
}
