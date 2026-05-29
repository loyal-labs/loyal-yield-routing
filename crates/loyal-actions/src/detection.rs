use crate::{derive_action_account, derive_loyal_hub_config, ids::*, JupiterSwapContract};
use solana_sdk::{hash::hashv, instruction::Instruction, pubkey::Pubkey};
use std::{collections::BTreeMap, fmt};

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PolicyDetectionError {
    InvalidInstructionData(&'static str),
    UnsupportedSettingsInstruction,
}

impl fmt::Display for PolicyDetectionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidInstructionData(message) => formatter.write_str(message),
            Self::UnsupportedSettingsInstruction => {
                formatter.write_str("instruction is not a supported Squads settings instruction")
            }
        }
    }
}

impl std::error::Error for PolicyDetectionError {}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SquadsSettingsActionView {
    pub settings: Pubkey,
    pub authority: Pubkey,
    pub policy_seed: u64,
    pub policy_account: Pubkey,
    pub delegated_signers: Vec<Pubkey>,
    pub threshold: u16,
    pub payload: SquadsProgramInteractionPolicyView,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SquadsProgramInteractionPolicyView {
    pub vault_index: u8,
    pub pubkey_table: Vec<Pubkey>,
    pub constraints: Vec<SquadsInstructionConstraintView>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SquadsInstructionConstraintView {
    pub program_id: Pubkey,
    pub account_constraints: Vec<SquadsAccountConstraintView>,
    pub data_constraints: Vec<SquadsDataConstraintView>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SquadsAccountConstraintView {
    pub account_index: u8,
    pub kind: SquadsAccountConstraintKindView,
    pub owner: Option<Pubkey>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SquadsAccountConstraintKindView {
    Pubkey(Vec<Pubkey>),
    AccountData(Vec<SquadsDataConstraintView>),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SquadsDataConstraintView {
    pub data_offset: u64,
    pub data_value: SquadsDataValueView,
    pub operator: SquadsDataOperatorView,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SquadsDataValueView {
    U8(u8),
    U16Le(u16),
    U32Le(u32),
    U64Le(u64),
    U128Le(u128),
    U8Slice(Vec<u8>),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SquadsDataOperatorView {
    Equals,
    NotEquals,
    GreaterThan,
    GreaterThanOrEqualTo,
    LessThan,
    LessThanOrEqualTo,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DetectedYieldRoutePolicy {
    pub settings: Pubkey,
    pub authority: Pubkey,
    pub policy_seed: u64,
    pub policy_account: Pubkey,
    pub vault_index: u8,
    pub delegated_signers: Vec<Pubkey>,
    pub threshold: u16,
    pub route_modes: Vec<DetectedYieldRouteMode>,
    pub stable_mints: Vec<Pubkey>,
    pub kamino_markets: Vec<Pubkey>,
    pub kamino_liquidity_mints: Vec<Pubkey>,
    pub swap_lanes: Vec<DetectedSwapLane>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DetectedYieldRouteMode {
    SameMint,
    CrossMintJupiter,
    CrossMintLoyalHub,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DetectedSwapLane {
    Jupiter(JupiterSwapContract),
    LoyalHub {
        hub_authorizer: Pubkey,
        max_fee_bps: u16,
    },
}

pub fn decode_squads_policy_create_actions(
    instruction: &Instruction,
) -> Result<Vec<SquadsSettingsActionView>, PolicyDetectionError> {
    if instruction.program_id != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        return Ok(vec![]);
    }
    let settings = instruction
        .accounts
        .first()
        .map(|account| account.pubkey)
        .ok_or(PolicyDetectionError::InvalidInstructionData(
            "missing Squads settings account",
        ))?;
    let authority = instruction
        .accounts
        .get(1)
        .map(|account| account.pubkey)
        .ok_or(PolicyDetectionError::InvalidInstructionData(
            "missing Squads authority account",
        ))?;
    let policy_account_hint = instruction.accounts.get(5).map(|account| account.pubkey);
    decode_squads_policy_create_actions_from_parts(
        settings,
        authority,
        policy_account_hint,
        &instruction.data,
    )
}

pub fn decode_squads_policy_create_actions_from_parts(
    settings: Pubkey,
    authority: Pubkey,
    policy_account_hint: Option<Pubkey>,
    instruction_data: &[u8],
) -> Result<Vec<SquadsSettingsActionView>, PolicyDetectionError> {
    let mut cursor = Cursor::new(instruction_data);
    let discriminator = cursor.read_array::<8>()?;
    if discriminator != anchor_instruction_discriminator("execute_settings_transaction_sync") {
        return Err(PolicyDetectionError::UnsupportedSettingsInstruction);
    }
    cursor.read_u8()?;
    let action_count = cursor.read_u32()? as usize;
    let mut actions = Vec::new();

    for _ in 0..action_count {
        match cursor.read_u8()? {
            7 => {
                let seed = cursor.read_u64()?;
                let Some(payload) = read_policy_payload(&mut cursor)? else {
                    skip_policy_create_tail(&mut cursor)?;
                    continue;
                };
                let delegated_signers = read_signers(&mut cursor)?;
                let threshold = cursor.read_u16()?;
                cursor.read_u32()?;
                skip_option_i64(&mut cursor)?;
                skip_policy_expiration_args(&mut cursor)?;
                let policy_account =
                    policy_account_hint.unwrap_or_else(|| derive_action_account(&settings, seed).0);
                actions.push(SquadsSettingsActionView {
                    settings,
                    authority,
                    policy_seed: seed,
                    policy_account,
                    delegated_signers,
                    threshold,
                    payload,
                });
            }
            tag => skip_settings_action(tag, &mut cursor)?,
        }
    }
    skip_option_string(&mut cursor)?;
    Ok(actions)
}

pub fn detect_yield_route_policy_create(
    action: &SquadsSettingsActionView,
) -> Option<DetectedYieldRoutePolicy> {
    let constraints = &action.payload.constraints;
    if constraints.len() < 2 {
        return None;
    }

    let withdraw = classify_kamino_withdraw(constraints.first()?)?;
    let deposit = classify_kamino_deposit(constraints.last()?, withdraw.vault)?;
    let mut stable_mints = Vec::new();
    let mut swap_lanes = Vec::new();
    let mut has_jupiter = false;
    let mut has_hub = false;

    for constraint in &constraints[1..constraints.len() - 1] {
        if let Some(jupiter) = classify_jupiter_swap(constraint, withdraw.vault) {
            stable_mints.extend(jupiter.stable_mints);
            swap_lanes.push(DetectedSwapLane::Jupiter(jupiter.contract));
            has_jupiter = true;
            continue;
        }
        if let Some(hub) = classify_loyal_hub_swap(constraint, withdraw.vault) {
            stable_mints.extend(hub.stable_mints);
            swap_lanes.push(DetectedSwapLane::LoyalHub {
                hub_authorizer: hub.hub_authorizer,
                max_fee_bps: hub.max_fee_bps,
            });
            has_hub = true;
            continue;
        }
        return None;
    }

    if swap_lanes.is_empty() {
        return None;
    }

    let mut route_modes = vec![DetectedYieldRouteMode::SameMint];
    if has_jupiter {
        route_modes.push(DetectedYieldRouteMode::CrossMintJupiter);
    }
    if has_hub {
        route_modes.push(DetectedYieldRouteMode::CrossMintLoyalHub);
    }

    let mut kamino_markets = withdraw.markets;
    kamino_markets.extend(deposit.markets);
    let mut kamino_liquidity_mints = withdraw.liquidity_mints;
    kamino_liquidity_mints.extend(deposit.liquidity_mints);

    Some(DetectedYieldRoutePolicy {
        settings: action.settings,
        authority: action.authority,
        policy_seed: action.policy_seed,
        policy_account: action.policy_account,
        vault_index: action.payload.vault_index,
        delegated_signers: unique_pubkeys(delegated_signers_as_pubkeys(&action.delegated_signers)),
        threshold: action.threshold,
        route_modes,
        stable_mints: unique_pubkeys(stable_mints),
        kamino_markets: unique_pubkeys(kamino_markets),
        kamino_liquidity_mints: unique_pubkeys(kamino_liquidity_mints),
        swap_lanes,
    })
}

fn delegated_signers_as_pubkeys(signers: &[Pubkey]) -> Vec<Pubkey> {
    signers.to_vec()
}

struct KaminoLeg {
    vault: Pubkey,
    markets: Vec<Pubkey>,
    liquidity_mints: Vec<Pubkey>,
}

struct JupiterLeg {
    stable_mints: Vec<Pubkey>,
    contract: JupiterSwapContract,
}

struct HubLeg {
    stable_mints: Vec<Pubkey>,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
}

fn classify_kamino_withdraw(constraint: &SquadsInstructionConstraintView) -> Option<KaminoLeg> {
    if constraint.program_id != KAMINO_LEND_PROGRAM_ID
        || !has_data_slice_equals(
            &constraint.data_constraints,
            0,
            &KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
        )
    {
        return None;
    }
    let accounts = accounts_by_index(constraint);
    let vault = single_pubkey(accounts.get(&0)?, None)?;
    has_token_authority(accounts.get(&8)?, vault)?;
    single_pubkey(accounts.get(&10)?, None).filter(|key| *key == spl_token::id())?;
    Some(KaminoLeg {
        vault,
        markets: pubkeys(accounts.get(&1)?, None)?,
        liquidity_mints: pubkeys(accounts.get(&4)?, Some(spl_token::id()))?,
    })
}

fn classify_kamino_deposit(
    constraint: &SquadsInstructionConstraintView,
    vault: Pubkey,
) -> Option<KaminoLeg> {
    if constraint.program_id != KAMINO_LEND_PROGRAM_ID
        || !has_data_slice_equals(
            &constraint.data_constraints,
            0,
            &KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
        )
    {
        return None;
    }
    let accounts = accounts_by_index(constraint);
    single_pubkey(accounts.get(&0)?, None).filter(|key| *key == vault)?;
    has_token_authority(accounts.get(&8)?, vault)?;
    single_pubkey(accounts.get(&10)?, None).filter(|key| *key == spl_token::id())?;
    Some(KaminoLeg {
        vault,
        markets: pubkeys(accounts.get(&2)?, None)?,
        liquidity_mints: pubkeys(accounts.get(&4)?, Some(spl_token::id()))?,
    })
}

fn classify_jupiter_swap(
    constraint: &SquadsInstructionConstraintView,
    vault: Pubkey,
) -> Option<JupiterLeg> {
    if !has_data_slice_equals(&constraint.data_constraints, 0, &JUPITER_SWAP_DISCRIMINATOR) {
        return None;
    }
    let max_slippage_bps = data_u16_lte(
        &constraint.data_constraints,
        JUPITER_SWAP_SLIPPAGE_BPS_OFFSET,
    )?;
    let accounts = accounts_by_index(constraint);
    single_pubkey(accounts.get(&0)?, None).filter(|key| *key == vault)?;
    has_token_authority(accounts.get(&1)?, vault)?;
    has_token_authority(accounts.get(&2)?, vault)?;
    single_pubkey(accounts.get(&5)?, None).filter(|key| *key == spl_token::id())?;
    let mut stable_mints = pubkeys(accounts.get(&3)?, Some(spl_token::id()))?;
    stable_mints.extend(pubkeys(accounts.get(&4)?, Some(spl_token::id()))?);
    Some(JupiterLeg {
        stable_mints,
        contract: JupiterSwapContract {
            program_id: constraint.program_id,
            exact_in_discriminator: JUPITER_SWAP_DISCRIMINATOR,
            max_slippage_bps,
        },
    })
}

fn classify_loyal_hub_swap(
    constraint: &SquadsInstructionConstraintView,
    vault: Pubkey,
) -> Option<HubLeg> {
    if constraint.program_id != LOYAL_HUB_SWAP_PROGRAM_ID
        || !has_data_u8_equals(
            &constraint.data_constraints,
            LOYAL_HUB_SWAP_TAG_OFFSET,
            LOYAL_HUB_SWAP_EXACT_IN,
        )
    {
        return None;
    }
    let max_fee_bps = data_u16_lte(
        &constraint.data_constraints,
        LOYAL_HUB_SWAP_MAX_FEE_BPS_OFFSET,
    )?;
    let accounts = accounts_by_index(constraint);
    single_pubkey(
        accounts.get(&loyal_hub_abi::swap_exact_in_accounts::CONFIG)?,
        Some(LOYAL_HUB_SWAP_PROGRAM_ID),
    )
    .filter(|key| *key == derive_loyal_hub_config())?;
    single_pubkey(
        accounts.get(&loyal_hub_abi::swap_exact_in_accounts::USER_VAULT)?,
        None,
    )
    .filter(|key| *key == vault)?;
    has_token_authority(
        accounts.get(&loyal_hub_abi::swap_exact_in_accounts::USER_INPUT)?,
        vault,
    )?;
    has_token_authority(
        accounts.get(&loyal_hub_abi::swap_exact_in_accounts::USER_OUTPUT)?,
        vault,
    )?;
    let mut stable_mints = pubkeys(
        accounts.get(&loyal_hub_abi::swap_exact_in_accounts::INPUT_MINT)?,
        Some(spl_token::id()),
    )?;
    stable_mints.extend(pubkeys(
        accounts.get(&loyal_hub_abi::swap_exact_in_accounts::OUTPUT_MINT)?,
        Some(spl_token::id()),
    )?);
    let hub_authorizer = single_pubkey(
        accounts.get(&loyal_hub_abi::swap_exact_in_accounts::HUB_AUTHORIZER)?,
        None,
    )?;
    single_pubkey(
        accounts.get(&loyal_hub_abi::swap_exact_in_accounts::TOKEN_PROGRAM)?,
        None,
    )
    .filter(|key| *key == spl_token::id())?;
    Some(HubLeg {
        stable_mints,
        hub_authorizer,
        max_fee_bps,
    })
}

fn accounts_by_index(
    constraint: &SquadsInstructionConstraintView,
) -> BTreeMap<u8, &SquadsAccountConstraintView> {
    constraint
        .account_constraints
        .iter()
        .map(|account| (account.account_index, account))
        .collect()
}

fn pubkeys(constraint: &SquadsAccountConstraintView, owner: Option<Pubkey>) -> Option<Vec<Pubkey>> {
    if constraint.owner != owner {
        return None;
    }
    match &constraint.kind {
        SquadsAccountConstraintKindView::Pubkey(pubkeys) => Some(pubkeys.clone()),
        SquadsAccountConstraintKindView::AccountData(_) => None,
    }
}

fn single_pubkey(
    constraint: &SquadsAccountConstraintView,
    owner: Option<Pubkey>,
) -> Option<Pubkey> {
    let pubkeys = pubkeys(constraint, owner)?;
    if pubkeys.len() == 1 {
        Some(pubkeys[0])
    } else {
        None
    }
}

fn has_token_authority(constraint: &SquadsAccountConstraintView, authority: Pubkey) -> Option<()> {
    if constraint.owner != Some(spl_token::id()) {
        return None;
    }
    let SquadsAccountConstraintKindView::AccountData(data_constraints) = &constraint.kind else {
        return None;
    };
    has_data_slice_equals(data_constraints, 32, authority.as_ref()).then_some(())
}

fn has_data_slice_equals(
    constraints: &[SquadsDataConstraintView],
    offset: u64,
    expected: &[u8],
) -> bool {
    constraints.iter().any(|constraint| {
        constraint.data_offset == offset
            && constraint.operator == SquadsDataOperatorView::Equals
            && constraint.data_value == SquadsDataValueView::U8Slice(expected.to_vec())
    })
}

fn has_data_u8_equals(constraints: &[SquadsDataConstraintView], offset: u64, expected: u8) -> bool {
    constraints.iter().any(|constraint| {
        constraint.data_offset == offset
            && constraint.operator == SquadsDataOperatorView::Equals
            && constraint.data_value == SquadsDataValueView::U8(expected)
    })
}

fn data_u16_lte(constraints: &[SquadsDataConstraintView], offset: u64) -> Option<u16> {
    constraints.iter().find_map(|constraint| {
        if constraint.data_offset == offset
            && constraint.operator == SquadsDataOperatorView::LessThanOrEqualTo
        {
            if let SquadsDataValueView::U16Le(value) = constraint.data_value {
                return Some(value);
            }
        }
        None
    })
}

fn read_policy_payload(
    cursor: &mut Cursor<'_>,
) -> Result<Option<SquadsProgramInteractionPolicyView>, PolicyDetectionError> {
    match cursor.read_u8()? {
        4 => {
            let vault_index = cursor.read_u8()?;
            let pubkey_table = cursor.read_small_pubkey_vec()?;
            let constraints = cursor
                .read_small_vec(|cursor| read_instruction_constraint(cursor, &pubkey_table))?;
            skip_compiled_hook(cursor)?;
            skip_compiled_hook(cursor)?;
            skip_small_vec(cursor, skip_compiled_spending_limit)?;
            Ok(Some(SquadsProgramInteractionPolicyView {
                vault_index,
                pubkey_table,
                constraints,
            }))
        }
        tag => {
            skip_policy_payload_body(tag, cursor)?;
            Ok(None)
        }
    }
}

fn read_instruction_constraint(
    cursor: &mut Cursor<'_>,
    pubkey_table: &[Pubkey],
) -> Result<SquadsInstructionConstraintView, PolicyDetectionError> {
    let program_id = indexed_pubkey(pubkey_table, cursor.read_u8()?)?;
    let account_constraints =
        cursor.read_small_vec(|cursor| read_account_constraint(cursor, pubkey_table))?;
    let data_constraints = cursor.read_small_vec(read_data_constraint)?;
    Ok(SquadsInstructionConstraintView {
        program_id,
        account_constraints,
        data_constraints,
    })
}

fn read_account_constraint(
    cursor: &mut Cursor<'_>,
    pubkey_table: &[Pubkey],
) -> Result<SquadsAccountConstraintView, PolicyDetectionError> {
    let account_index = cursor.read_u8()?;
    let kind = match cursor.read_u8()? {
        0 => SquadsAccountConstraintKindView::Pubkey(
            cursor
                .read_small_u8_vec()?
                .into_iter()
                .map(|index| indexed_pubkey(pubkey_table, index))
                .collect::<Result<Vec<_>, _>>()?,
        ),
        1 => SquadsAccountConstraintKindView::AccountData(
            cursor.read_small_vec(read_data_constraint)?,
        ),
        _ => {
            return Err(PolicyDetectionError::InvalidInstructionData(
                "unknown account constraint kind",
            ))
        }
    };
    let owner = read_option(cursor, |cursor| {
        let index = cursor.read_u8()?;
        indexed_pubkey(pubkey_table, index)
    })?;
    Ok(SquadsAccountConstraintView {
        account_index,
        kind,
        owner,
    })
}

fn read_data_constraint(
    cursor: &mut Cursor<'_>,
) -> Result<SquadsDataConstraintView, PolicyDetectionError> {
    let data_offset = cursor.read_u64()?;
    let data_value = match cursor.read_u8()? {
        0 => SquadsDataValueView::U8(cursor.read_u8()?),
        1 => SquadsDataValueView::U16Le(cursor.read_u16()?),
        2 => SquadsDataValueView::U32Le(cursor.read_u32()?),
        3 => SquadsDataValueView::U64Le(cursor.read_u64()?),
        4 => SquadsDataValueView::U128Le(cursor.read_u128()?),
        5 => SquadsDataValueView::U8Slice(cursor.read_vec_u8()?),
        _ => {
            return Err(PolicyDetectionError::InvalidInstructionData(
                "unknown data value kind",
            ))
        }
    };
    let operator = match cursor.read_u8()? {
        0 => SquadsDataOperatorView::Equals,
        1 => SquadsDataOperatorView::NotEquals,
        2 => SquadsDataOperatorView::GreaterThan,
        3 => SquadsDataOperatorView::GreaterThanOrEqualTo,
        4 => SquadsDataOperatorView::LessThan,
        5 => SquadsDataOperatorView::LessThanOrEqualTo,
        _ => {
            return Err(PolicyDetectionError::InvalidInstructionData(
                "unknown data operator",
            ))
        }
    };
    Ok(SquadsDataConstraintView {
        data_offset,
        data_value,
        operator,
    })
}

fn read_signers(cursor: &mut Cursor<'_>) -> Result<Vec<Pubkey>, PolicyDetectionError> {
    let len = cursor.read_u32()? as usize;
    let mut signers = Vec::with_capacity(len);
    for _ in 0..len {
        signers.push(cursor.read_pubkey()?);
        cursor.read_u8()?;
    }
    Ok(signers)
}

fn skip_settings_action(tag: u8, cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    match tag {
        0 => {
            cursor.skip(32)?;
            cursor.skip(1)
        }
        1 => cursor.skip(32),
        2 => cursor.skip(2),
        3 => cursor.skip(4),
        4 => {
            cursor.skip(32 + 1 + 32 + 8)?;
            skip_legacy_period(cursor)?;
            skip_pubkey_vec(cursor)?;
            skip_pubkey_vec(cursor)?;
            cursor.skip(8)
        }
        5 => cursor.skip(32),
        6 => skip_option_pubkey(cursor),
        8 => {
            cursor.skip(32)?;
            read_signers(cursor)?;
            cursor.skip(2 + 4)?;
            let payload_tag = cursor.read_u8()?;
            skip_policy_payload_body(payload_tag, cursor)?;
            skip_policy_expiration_args(cursor)
        }
        9 => cursor.skip(32),
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "unknown settings action",
        )),
    }
}

fn skip_policy_create_tail(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    read_signers(cursor)?;
    cursor.skip(2 + 4)?;
    skip_option_i64(cursor)?;
    skip_policy_expiration_args(cursor)
}

fn skip_policy_payload_body(tag: u8, cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    match tag {
        0 | 2 | 3 => {
            let len = cursor.read_u32()? as usize;
            cursor.skip(len)
        }
        1 => skip_spending_limit_policy_payload(cursor),
        4 => Err(PolicyDetectionError::InvalidInstructionData(
            "unexpected ProgramInteraction skip path",
        )),
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "unknown policy payload",
        )),
    }
}

fn skip_compiled_hook(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    read_option(cursor, |cursor| {
        cursor.read_u8()?;
        skip_small_vec(cursor, |cursor| {
            let pubkey_table = Vec::new();
            read_account_constraint(cursor, &pubkey_table).map(|_| ())
        })?;
        cursor.read_small_u8_vec()?;
        cursor.read_u8()?;
        cursor.read_u8()?;
        Ok(())
    })
    .map(|_| ())
}

fn skip_compiled_spending_limit(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    cursor.read_u8()?;
    cursor.skip(8)?;
    skip_option_i64(cursor)?;
    skip_period_v2(cursor)?;
    cursor.skip(8)
}

fn skip_spending_limit_policy_payload(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    cursor.skip(32 + 1)?;
    cursor.skip(8)?;
    skip_option_i64(cursor)?;
    skip_period_v2(cursor)?;
    cursor.read_u8()?;
    cursor.skip(8 + 8 + 1)?;
    read_option(cursor, |cursor| cursor.skip(8 + 8)).map(|_| ())?;
    skip_pubkey_vec(cursor)
}

fn skip_small_vec(
    cursor: &mut Cursor<'_>,
    mut skip_item: impl FnMut(&mut Cursor<'_>) -> Result<(), PolicyDetectionError>,
) -> Result<(), PolicyDetectionError> {
    let len = cursor.read_u8()? as usize;
    for _ in 0..len {
        skip_item(cursor)?;
    }
    Ok(())
}

fn skip_pubkey_vec(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    let len = cursor.read_u32()? as usize;
    cursor.skip(32 * len)
}

fn skip_option_pubkey(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    read_option(cursor, |cursor| cursor.skip(32)).map(|_| ())
}

fn skip_option_i64(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    read_option(cursor, |cursor| cursor.skip(8)).map(|_| ())
}

fn skip_option_string(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    read_option(cursor, |cursor| {
        let len = cursor.read_u32()? as usize;
        cursor.skip(len)
    })
    .map(|_| ())
}

fn skip_policy_expiration_args(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    read_option(cursor, |cursor| match cursor.read_u8()? {
        0 => cursor.skip(8),
        1 => Ok(()),
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "unknown policy expiration args",
        )),
    })
    .map(|_| ())
}

fn skip_legacy_period(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    match cursor.read_u8()? {
        0..=3 => Ok(()),
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "unknown legacy period",
        )),
    }
}

fn skip_period_v2(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    match cursor.read_u8()? {
        0..=3 => Ok(()),
        4 => cursor.skip(8),
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "unknown period v2",
        )),
    }
}

fn read_option<T>(
    cursor: &mut Cursor<'_>,
    read_some: impl FnOnce(&mut Cursor<'_>) -> Result<T, PolicyDetectionError>,
) -> Result<Option<T>, PolicyDetectionError> {
    match cursor.read_u8()? {
        0 => Ok(None),
        1 => read_some(cursor).map(Some),
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "invalid option tag",
        )),
    }
}

fn indexed_pubkey(pubkey_table: &[Pubkey], index: u8) -> Result<Pubkey, PolicyDetectionError> {
    pubkey_table
        .get(index as usize)
        .copied()
        .ok_or(PolicyDetectionError::InvalidInstructionData(
            "pubkey table index out of bounds",
        ))
}

fn unique_pubkeys(pubkeys: Vec<Pubkey>) -> Vec<Pubkey> {
    let mut unique = Vec::new();
    for pubkey in pubkeys {
        if !unique.contains(&pubkey) {
            unique.push(pubkey);
        }
    }
    unique
}

fn anchor_instruction_discriminator(name: &str) -> [u8; 8] {
    let preimage = format!("global:{name}");
    let hash = hashv(&[preimage.as_bytes()]).to_bytes();
    hash[..8].try_into().expect("slice length")
}

struct Cursor<'a> {
    data: &'a [u8],
    offset: usize,
}

impl<'a> Cursor<'a> {
    fn new(data: &'a [u8]) -> Self {
        Self { data, offset: 0 }
    }

    fn remaining(&self) -> usize {
        self.data.len().saturating_sub(self.offset)
    }

    fn skip(&mut self, len: usize) -> Result<(), PolicyDetectionError> {
        self.take(len).map(|_| ())
    }

    fn take(&mut self, len: usize) -> Result<&'a [u8], PolicyDetectionError> {
        if self.remaining() < len {
            return Err(PolicyDetectionError::InvalidInstructionData(
                "truncated instruction data",
            ));
        }
        let start = self.offset;
        self.offset += len;
        Ok(&self.data[start..self.offset])
    }

    fn read_array<const N: usize>(&mut self) -> Result<[u8; N], PolicyDetectionError> {
        Ok(self.take(N)?.try_into().expect("slice length"))
    }

    fn read_u8(&mut self) -> Result<u8, PolicyDetectionError> {
        Ok(self.take(1)?[0])
    }

    fn read_u16(&mut self) -> Result<u16, PolicyDetectionError> {
        Ok(u16::from_le_bytes(self.read_array()?))
    }

    fn read_u32(&mut self) -> Result<u32, PolicyDetectionError> {
        Ok(u32::from_le_bytes(self.read_array()?))
    }

    fn read_u64(&mut self) -> Result<u64, PolicyDetectionError> {
        Ok(u64::from_le_bytes(self.read_array()?))
    }

    fn read_u128(&mut self) -> Result<u128, PolicyDetectionError> {
        Ok(u128::from_le_bytes(self.read_array()?))
    }

    fn read_pubkey(&mut self) -> Result<Pubkey, PolicyDetectionError> {
        Ok(Pubkey::new_from_array(self.read_array()?))
    }

    fn read_vec_u8(&mut self) -> Result<Vec<u8>, PolicyDetectionError> {
        let len = self.read_u32()? as usize;
        Ok(self.take(len)?.to_vec())
    }

    fn read_small_u8_vec(&mut self) -> Result<Vec<u8>, PolicyDetectionError> {
        let len = self.read_u8()? as usize;
        (0..len).map(|_| self.read_u8()).collect()
    }

    fn read_small_pubkey_vec(&mut self) -> Result<Vec<Pubkey>, PolicyDetectionError> {
        let len = self.read_u8()? as usize;
        (0..len).map(|_| self.read_pubkey()).collect()
    }

    fn read_small_vec<T>(
        &mut self,
        mut read_item: impl FnMut(&mut Cursor<'a>) -> Result<T, PolicyDetectionError>,
    ) -> Result<Vec<T>, PolicyDetectionError> {
        let len = self.read_u8()? as usize;
        let mut items = Vec::with_capacity(len);
        for _ in 0..len {
            items.push(read_item(self)?);
        }
        Ok(items)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        create_all_in_one_market_mint_yield_route_action, create_three_step_yield_route_actions,
        LoyalActionContext, RouteTopology, SwapLane, YieldRouteActionBuilder,
        YieldRouteActionSeeds, YieldRouteUniverse, JUPITER_V6_PROGRAM_ID,
    };

    fn context() -> LoyalActionContext {
        LoyalActionContext {
            settings: Pubkey::new_unique(),
            authority: Pubkey::new_unique(),
            delegated_signer: Pubkey::new_unique(),
            account_index: 0,
            vault: Pubkey::new_unique(),
        }
    }

    fn universe() -> YieldRouteUniverse {
        YieldRouteUniverse::new(
            vec![Pubkey::new_unique(), Pubkey::new_unique()],
            vec![Pubkey::new_unique()],
            vec![Pubkey::new_unique()],
        )
    }

    fn jupiter_lane() -> SwapLane {
        SwapLane::Jupiter(JupiterSwapContract {
            program_id: JUPITER_V6_PROGRAM_ID,
            exact_in_discriminator: JUPITER_SWAP_DISCRIMINATOR,
            max_slippage_bps: JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
        })
    }

    fn detected_action() -> SquadsSettingsActionView {
        let setup = create_all_in_one_market_mint_yield_route_action(
            context(),
            universe(),
            vec![jupiter_lane()],
        )
        .unwrap();
        decode_squads_policy_create_actions(&setup.instructions[0])
            .unwrap()
            .remove(0)
    }

    #[test]
    fn detects_all_in_one_same_mint_jupiter_and_hub_routes() {
        let context = context();
        let hub_authorizer = Pubkey::new_unique();
        let setup = create_all_in_one_market_mint_yield_route_action(
            context,
            universe(),
            vec![
                jupiter_lane(),
                SwapLane::LoyalHub {
                    hub_authorizer,
                    max_fee_bps: 50,
                },
            ],
        )
        .unwrap();

        let actions = decode_squads_policy_create_actions(&setup.instructions[0]).unwrap();
        let detected = detect_yield_route_policy_create(&actions[0]).unwrap();

        assert_eq!(detected.settings, context.settings);
        assert_eq!(detected.authority, context.authority);
        assert_eq!(detected.policy_seed, YIELD_ROUTE_WITHDRAW_ACTION_SEED);
        assert_eq!(detected.vault_index, context.account_index);
        assert_eq!(detected.delegated_signers, vec![context.delegated_signer]);
        assert_eq!(
            detected.route_modes,
            vec![
                DetectedYieldRouteMode::SameMint,
                DetectedYieldRouteMode::CrossMintJupiter,
                DetectedYieldRouteMode::CrossMintLoyalHub,
            ]
        );
        assert_eq!(detected.swap_lanes.len(), 2);
        assert!(detected.stable_mints.len() >= 2);
        assert_eq!(detected.kamino_markets.len(), 1);
        assert_eq!(detected.kamino_liquidity_mints.len(), 1);
    }

    #[test]
    fn rejects_split_three_step_policy() {
        let setup = create_three_step_yield_route_actions(
            context(),
            universe(),
            vec![jupiter_lane()],
            YieldRouteActionSeeds::default(),
        )
        .unwrap();

        let actions = decode_squads_policy_create_actions(&setup.instructions[0]).unwrap();

        assert!(detect_yield_route_policy_create(&actions[0]).is_none());
    }

    #[test]
    fn rejects_wrong_program_order() {
        let context = context();
        let setup = YieldRouteActionBuilder::new(context, universe())
            .topology(RouteTopology::SwapOnly)
            .swap_lanes(vec![jupiter_lane()])
            .build()
            .unwrap();

        let actions = decode_squads_policy_create_actions(&setup.instructions[0]).unwrap();

        assert!(detect_yield_route_policy_create(&actions[0]).is_none());
    }

    #[test]
    fn rejects_missing_withdraw_or_deposit() {
        let mut missing_withdraw = detected_action();
        missing_withdraw.payload.constraints.remove(0);
        assert!(detect_yield_route_policy_create(&missing_withdraw).is_none());

        let mut missing_deposit = detected_action();
        missing_deposit.payload.constraints.pop();
        assert!(detect_yield_route_policy_create(&missing_deposit).is_none());
    }

    #[test]
    fn rejects_wrong_token_owner_constraints() {
        let mut action = detected_action();
        let withdraw = action.payload.constraints.first_mut().unwrap();
        let source_liquidity = withdraw
            .account_constraints
            .iter_mut()
            .find(|constraint| constraint.account_index == 8)
            .unwrap();
        source_liquidity.owner = None;

        assert!(detect_yield_route_policy_create(&action).is_none());
    }

    #[test]
    fn rejects_wrong_discriminators() {
        let mut action = detected_action();
        let withdraw = action.payload.constraints.first_mut().unwrap();
        let discriminator = withdraw
            .data_constraints
            .iter_mut()
            .find(|constraint| constraint.data_offset == 0)
            .unwrap();
        discriminator.data_value = SquadsDataValueView::U8Slice(vec![0; 8]);

        assert!(detect_yield_route_policy_create(&action).is_none());
    }

    #[test]
    fn malformed_policy_data_is_fallible() {
        let mut setup = create_all_in_one_market_mint_yield_route_action(
            context(),
            universe(),
            vec![jupiter_lane()],
        )
        .unwrap();
        setup.instructions[0].data.truncate(12);

        assert!(decode_squads_policy_create_actions(&setup.instructions[0]).is_err());
    }
}
