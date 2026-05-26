use crate::ids::*;
use crate::squads::{
    SquadsAccountConstraint, SquadsAccountConstraintType, SquadsDataConstraint, SquadsDataOperator,
    SquadsDataValue, SquadsInstructionConstraint,
};
use solana_sdk::pubkey::Pubkey;

use crate::actions::JupiterSwapContract;

pub(crate) fn kamino_market_mint_constraint(
    vault: Pubkey,
    markets: Vec<Pubkey>,
    liquidity_mints: Vec<Pubkey>,
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![vault], None),
            pubkey_constraint(2, unique_pubkeys(markets), None),
            pubkey_constraint(3, unique_pubkeys(liquidity_mints), Some(spl_token::id())),
            pubkey_constraint(10, vec![spl_token::id()], None),
        ],
        data_constraints: discriminator_constraint(discriminator),
    }
}

pub(crate) fn kamino_mint_constraint(
    vault: Pubkey,
    liquidity_mints: Vec<Pubkey>,
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![vault], None),
            pubkey_constraint(3, unique_pubkeys(liquidity_mints), Some(spl_token::id())),
            pubkey_constraint(10, vec![spl_token::id()], None),
        ],
        data_constraints: discriminator_constraint(discriminator),
    }
}

pub(crate) fn jupiter_constraint(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    contract: JupiterSwapContract,
) -> SquadsInstructionConstraint {
    let allowed_mints = unique_pubkeys(allowed_mints);
    let mut account_constraints = vec![
        pubkey_constraint(0, vec![vault], None),
        account_data_constraint(1, Some(spl_token::id())),
        account_data_constraint(2, Some(spl_token::id())),
        pubkey_constraint(3, allowed_mints.clone(), Some(spl_token::id())),
        pubkey_constraint(4, allowed_mints, Some(spl_token::id())),
        pubkey_constraint(5, vec![spl_token::id()], None),
    ];

    if contract.include_intermediate_token_accounts {
        account_constraints.extend([
            account_data_constraint(6, Some(spl_token::id())),
            account_data_constraint(7, Some(spl_token::id())),
        ]);
    }

    SquadsInstructionConstraint {
        program_id: contract.program_id,
        account_constraints,
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8(contract.exact_in_discriminator),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

pub(crate) fn loyal_hub_constraint(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
) -> SquadsInstructionConstraint {
    let allowed_mints = unique_pubkeys(allowed_mints);
    let inventory_accounts = allowed_mints
        .iter()
        .map(|mint| derive_loyal_hub_inventory_account(*mint))
        .collect::<Vec<_>>();

    SquadsInstructionConstraint {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(
                0,
                vec![derive_loyal_hub_config()],
                Some(LOYAL_HUB_SWAP_PROGRAM_ID),
            ),
            pubkey_constraint(1, vec![vault], None),
            pubkey_constraint(4, inventory_accounts.clone(), Some(spl_token::id())),
            pubkey_constraint(5, inventory_accounts, Some(spl_token::id())),
            pubkey_constraint(6, allowed_mints.clone(), Some(spl_token::id())),
            pubkey_constraint(7, allowed_mints, Some(spl_token::id())),
            pubkey_constraint(9, vec![hub_authorizer], None),
            pubkey_constraint(10, vec![spl_token::id()], None),
        ],
        data_constraints: vec![
            SquadsDataConstraint {
                data_offset: 0,
                data_value: SquadsDataValue::U8(LOYAL_HUB_SWAP_EXACT_IN),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: 25,
                data_value: SquadsDataValue::U16Le(max_fee_bps),
                operator: SquadsDataOperator::LessThanOrEqualTo,
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

pub(crate) fn discriminator_constraint(discriminator: [u8; 8]) -> Vec<SquadsDataConstraint> {
    vec![SquadsDataConstraint {
        data_offset: 0,
        data_value: SquadsDataValue::U8Slice(discriminator.to_vec()),
        operator: SquadsDataOperator::Equals,
    }]
}

pub fn derive_loyal_hub_config() -> Pubkey {
    Pubkey::find_program_address(&[LOYAL_HUB_CONFIG_SEED], &LOYAL_HUB_SWAP_PROGRAM_ID).0
}

pub fn derive_loyal_hub_authority() -> Pubkey {
    Pubkey::find_program_address(&[LOYAL_HUB_AUTHORITY_SEED], &LOYAL_HUB_SWAP_PROGRAM_ID).0
}

pub fn derive_loyal_hub_inventory_account(mint: Pubkey) -> Pubkey {
    let hub_authority = derive_loyal_hub_authority();
    Pubkey::find_program_address(
        &[
            hub_authority.as_ref(),
            spl_token::id().as_ref(),
            mint.as_ref(),
        ],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
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
