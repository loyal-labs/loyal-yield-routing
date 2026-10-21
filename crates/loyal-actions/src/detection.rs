use crate::{
    derive_action_account, derive_associated_token_account, earn_stablecoins,
    ids::*,
    jupiter::{JupiterCrossMintSourceShard, JupiterV2Dialect},
};
use solana_sdk::{hash::hashv, instruction::Instruction, pubkey::Pubkey};
use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
};

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
    pub spending_limits: Vec<SquadsLimitedSpendingLimitView>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SquadsLimitedSpendingLimitView {
    pub mint: Pubkey,
    pub start: i64,
    pub expiration: Option<i64>,
    pub period: SquadsPeriodV2View,
    pub max_per_period: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SquadsPeriodV2View {
    OneTime,
    Daily,
    Weekly,
    Monthly,
    Custom(i64),
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
pub struct DetectedJupiterCrossMintPolicyAccount {
    pub settings: Pubkey,
    pub policy_seed: u64,
    pub policy_account: Pubkey,
    pub account_index: u8,
    pub vault: Pubkey,
    pub delegated_signer: Pubkey,
    pub threshold: u16,
    pub source_shard: JupiterCrossMintSourceShard,
    pub max_slippage_bps: u16,
    pub daily_source_mint_spending_cap: u64,
    pub dialect_constraint_indexes: BTreeMap<JupiterV2Dialect, u8>,
}

pub fn detect_jupiter_cross_mint_policy_account(
    account_data: &[u8],
) -> Result<Option<DetectedJupiterCrossMintPolicyAccount>, PolicyDetectionError> {
    let mut cursor = Cursor::new(account_data);
    if cursor.read_array::<8>()? != anchor_account_discriminator("Policy") {
        return Err(PolicyDetectionError::InvalidInstructionData(
            "account discriminator is not a Squads Policy account",
        ));
    }

    let settings = cursor.read_pubkey()?;
    let policy_seed = cursor.read_u64()?;
    let policy_bump = cursor.read_u8()?;
    let transaction_index = cursor.read_u64()?;
    let stale_transaction_index = cursor.read_u64()?;
    let signer_count = read_bounded_u32_len(&mut cursor, 32, "too many policy signers")?;
    let mut signers = Vec::with_capacity(signer_count);
    for _ in 0..signer_count {
        signers.push((cursor.read_pubkey()?, cursor.read_u8()?));
    }
    let threshold = cursor.read_u16()?;
    let time_lock = cursor.read_u32()?;
    let policy_state_tag = cursor.read_u8()?;

    if policy_state_tag != 3 {
        skip_non_program_interaction_policy_state(policy_state_tag, &mut cursor)?;
        read_policy_account_tail(&mut cursor)?;
        return Ok(None);
    }

    let account_index = cursor.read_u8()?;
    let legacy = read_legacy_program_interaction_policy_account(cursor, account_index);
    let compact = read_compact_program_interaction_policy_account(cursor, account_index);
    let candidates = [legacy.ok(), compact.ok()]
        .into_iter()
        .flatten()
        .collect::<Vec<_>>();
    if candidates.is_empty() {
        return Err(PolicyDetectionError::InvalidInstructionData(
            "invalid ProgramInteraction policy account",
        ));
    }

    let (policy_account, expected_bump) = derive_action_account(&settings, policy_seed);
    let Some((delegated_signer, permissions)) = signers.as_slice().first().copied() else {
        return Ok(None);
    };
    if signers.len() != 1
        || permissions != SQUADS_FULL_PERMISSIONS_MASK
        || threshold != 1
        || time_lock != 0
        || stale_transaction_index > transaction_index
        || policy_bump != expected_bump
    {
        return Ok(None);
    }

    let vault = derive_squads_vault(&settings, account_index);
    let mut detected = Vec::new();
    for candidate in &candidates {
        if candidate.pre_hook
            || candidate.post_hook
            || !candidate.exact_spending_limit_state
            || candidate.start < 0
            || candidate.has_expiration
            || !compact_pubkey_table_is_tight(&candidate.payload)
        {
            continue;
        }
        let Some(policy) = classify_jupiter_cross_mint_policy(&candidate.payload) else {
            continue;
        };
        if policy.vault != vault {
            continue;
        }
        detected.push(DetectedJupiterCrossMintPolicyAccount {
            settings,
            policy_seed,
            policy_account,
            account_index,
            vault,
            delegated_signer,
            threshold,
            source_shard: policy.source_shard,
            max_slippage_bps: policy.max_slippage_bps,
            daily_source_mint_spending_cap: policy.daily_source_mint_spending_cap,
            dialect_constraint_indexes: policy.dialect_constraint_indexes,
        });
    }

    let Some(first) = detected.first().cloned() else {
        return Ok(None);
    };
    if detected.len() != candidates.len() || detected.iter().any(|policy| *policy != first) {
        return Ok(None);
    }
    Ok(Some(first))
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct ProgramInteractionPolicyAccountView {
    payload: SquadsProgramInteractionPolicyView,
    pre_hook: bool,
    post_hook: bool,
    exact_spending_limit_state: bool,
    start: i64,
    has_expiration: bool,
}

fn read_legacy_program_interaction_policy_account(
    mut cursor: Cursor<'_>,
    account_index: u8,
) -> Result<ProgramInteractionPolicyAccountView, PolicyDetectionError> {
    let constraints = cursor.read_vec(read_raw_instruction_constraint)?;
    let pre_hook = read_option(&mut cursor, skip_policy_account_raw_hook_body)?.is_some();
    let post_hook = read_option(&mut cursor, skip_policy_account_raw_hook_body)?.is_some();
    let spending_limit_count = read_bounded_u32_len(
        &mut cursor,
        128,
        "too many ProgramInteraction spending limits",
    )?;
    let mut spending_limits = Vec::with_capacity(spending_limit_count);
    let mut exact_spending_limit_state = true;
    for _ in 0..spending_limit_count {
        let (limit, exact_state) = read_policy_account_spending_limit(&mut cursor)?;
        spending_limits.push(limit);
        exact_spending_limit_state &= exact_state;
    }
    let tail = read_policy_account_tail(&mut cursor)?;

    Ok(ProgramInteractionPolicyAccountView {
        payload: SquadsProgramInteractionPolicyView {
            vault_index: account_index,
            pubkey_table: vec![],
            constraints,
            spending_limits,
        },
        pre_hook,
        post_hook,
        exact_spending_limit_state,
        start: tail.start,
        has_expiration: tail.has_expiration,
    })
}

fn read_compact_program_interaction_policy_account(
    mut cursor: Cursor<'_>,
    account_index: u8,
) -> Result<ProgramInteractionPolicyAccountView, PolicyDetectionError> {
    let pubkey_table = cursor.read_small_pubkey_vec()?;
    if pubkey_table.len() > 240 {
        return Err(PolicyDetectionError::InvalidInstructionData(
            "ProgramInteraction pubkey table exceeds Squads limit",
        ));
    }
    let constraints =
        cursor.read_small_vec(|cursor| read_instruction_constraint(cursor, &pubkey_table))?;
    let pre_hook = read_option(&mut cursor, |cursor| {
        skip_policy_account_compact_hook_body(cursor, &pubkey_table)
    })?
    .is_some();
    let post_hook = read_option(&mut cursor, |cursor| {
        skip_policy_account_compact_hook_body(cursor, &pubkey_table)
    })?
    .is_some();
    let spending_limit_count = cursor.read_u8()? as usize;
    let mut spending_limits = Vec::with_capacity(spending_limit_count);
    for _ in 0..spending_limit_count {
        spending_limits.push(read_policy_account_compact_spending_limit(
            &mut cursor,
            &pubkey_table,
        )?);
    }
    let tail = read_policy_account_tail(&mut cursor)?;

    Ok(ProgramInteractionPolicyAccountView {
        payload: SquadsProgramInteractionPolicyView {
            vault_index: account_index,
            pubkey_table,
            constraints,
            spending_limits,
        },
        pre_hook,
        post_hook,
        exact_spending_limit_state: true,
        start: tail.start,
        has_expiration: tail.has_expiration,
    })
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct PolicyAccountTail {
    start: i64,
    has_expiration: bool,
}

fn read_policy_account_tail(
    cursor: &mut Cursor<'_>,
) -> Result<PolicyAccountTail, PolicyDetectionError> {
    let start = cursor.read_i64()?;
    let expiration = read_option(cursor, |cursor| match cursor.read_u8()? {
        0 => cursor.skip(8),
        1 => cursor.skip(32),
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "unknown policy expiration",
        )),
    })?;
    cursor.read_pubkey()?;
    Ok(PolicyAccountTail {
        start,
        has_expiration: expiration.is_some(),
    })
}

fn skip_non_program_interaction_policy_state(
    tag: u8,
    cursor: &mut Cursor<'_>,
) -> Result<(), PolicyDetectionError> {
    match tag {
        0 => {
            cursor.skip(64)?;
            skip_pubkey_vec(cursor)
        }
        1 => {
            cursor.read_u8()?;
            skip_pubkey_vec(cursor)?;
            skip_policy_account_spending_limit(cursor)
        }
        2 => {
            let action_count =
                read_bounded_u32_len(cursor, 128, "too many settings-change actions")?;
            for _ in 0..action_count {
                skip_allowed_settings_change(cursor)?;
            }
            Ok(())
        }
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "unknown policy state",
        )),
    }
}

fn skip_allowed_settings_change(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    match cursor.read_u8()? {
        0 => {
            skip_option_pubkey(cursor)?;
            read_option(cursor, |cursor| cursor.skip(1)).map(|_| ())
        }
        1 => skip_option_pubkey(cursor),
        2 => Ok(()),
        3 => read_option(cursor, |cursor| cursor.skip(4)).map(|_| ()),
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "unknown allowed settings change",
        )),
    }
}

fn skip_policy_account_raw_hook_body(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    cursor.read_u8()?;
    cursor.read_vec(read_raw_account_constraint)?;
    cursor.read_vec_u8()?;
    cursor.read_pubkey()?;
    read_bool(cursor)
}

fn skip_policy_account_compact_hook_body(
    cursor: &mut Cursor<'_>,
    pubkey_table: &[Pubkey],
) -> Result<(), PolicyDetectionError> {
    cursor.read_u8()?;
    cursor.read_small_vec(|cursor| read_account_constraint(cursor, pubkey_table))?;
    cursor.read_small_u8_vec()?;
    indexed_pubkey(pubkey_table, cursor.read_u8()?)?;
    read_bool(cursor)
}

fn skip_policy_account_spending_limit(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    read_policy_account_spending_limit(cursor).map(|_| ())
}

fn read_policy_account_spending_limit(
    cursor: &mut Cursor<'_>,
) -> Result<(SquadsLimitedSpendingLimitView, bool), PolicyDetectionError> {
    let mint = cursor.read_pubkey()?;
    let start = cursor.read_i64()?;
    let expiration = read_option(cursor, |cursor| cursor.read_i64())?;
    let period = read_period_v2(cursor)?;
    let accumulate_unused = read_bool_value(cursor)?;
    let max_per_period = cursor.read_u64()?;
    let max_per_use = cursor.read_u64()?;
    let enforce_exact_quantity = read_bool_value(cursor)?;
    let remaining_in_period = cursor.read_u64()?;
    let last_reset = cursor.read_i64()?;
    let exact_state = !accumulate_unused
        && max_per_use == 0
        && !enforce_exact_quantity
        && remaining_in_period <= max_per_period
        && last_reset >= start;
    Ok((
        SquadsLimitedSpendingLimitView {
            mint,
            start,
            expiration,
            period,
            max_per_period,
        },
        exact_state,
    ))
}

fn read_policy_account_compact_spending_limit(
    cursor: &mut Cursor<'_>,
    pubkey_table: &[Pubkey],
) -> Result<SquadsLimitedSpendingLimitView, PolicyDetectionError> {
    Ok(SquadsLimitedSpendingLimitView {
        mint: indexed_pubkey(pubkey_table, cursor.read_u8()?)?,
        start: cursor.read_i64()?,
        expiration: read_option(cursor, |cursor| cursor.read_i64())?,
        period: read_period_v2(cursor)?,
        max_per_period: cursor.read_u64()?,
    })
}

fn read_bool(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    read_bool_value(cursor).map(|_| ())
}

fn read_bool_value(cursor: &mut Cursor<'_>) -> Result<bool, PolicyDetectionError> {
    match cursor.read_u8()? {
        0 => Ok(false),
        1 => Ok(true),
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "invalid bool value",
        )),
    }
}

fn read_bounded_u32_len(
    cursor: &mut Cursor<'_>,
    maximum: usize,
    error: &'static str,
) -> Result<usize, PolicyDetectionError> {
    let len = cursor.read_u32()? as usize;
    if len > maximum {
        return Err(PolicyDetectionError::InvalidInstructionData(error));
    }
    Ok(len)
}

struct GeneralizedJupiterCrossMintPolicy {
    vault: Pubkey,
    source_shard: JupiterCrossMintSourceShard,
    max_slippage_bps: u16,
    daily_source_mint_spending_cap: u64,
    dialect_constraint_indexes: BTreeMap<JupiterV2Dialect, u8>,
}

fn classify_jupiter_cross_mint_policy(
    payload: &SquadsProgramInteractionPolicyView,
) -> Option<GeneralizedJupiterCrossMintPolicy> {
    if !compact_pubkey_table_is_tight(payload) {
        return None;
    }

    let [route_v2, shared_accounts_route_v2] = payload.constraints.as_slice() else {
        return None;
    };
    let mut dialect_constraint_indexes = BTreeMap::new();
    let mut max_slippage_bps = None;

    for (constraint_index, (constraint, dialect)) in [
        (route_v2, JupiterV2Dialect::RouteV2),
        (
            shared_accounts_route_v2,
            JupiterV2Dialect::SharedAccountsRouteV2,
        ),
    ]
    .into_iter()
    .enumerate()
    {
        if constraint.program_id != JUPITER_V6_PROGRAM_ID {
            return None;
        }
        let [authority, output_token_account] = constraint.account_constraints.as_slice() else {
            return None;
        };
        let layout = dialect.account_layout();
        let vault = single_pubkey_constraint(authority, layout.authority, None)?;
        let expected_atas = canonical_vault_atas(vault);
        pubkey_allowlist_constraint(
            output_token_account,
            layout.output_token_account,
            &expected_atas,
        )?;

        let [discriminator, slippage, platform_fee] = constraint.data_constraints.as_slice() else {
            return None;
        };
        if !policy_data_slice_equals(discriminator, 0, &dialect.discriminator())
            || !policy_data_u8_equals(platform_fee, dialect.platform_fee_bps_offset(), 0)
        {
            return None;
        }
        let slippage_bps = policy_data_u16_lte(slippage, dialect.slippage_bps_offset())?;
        if slippage_bps == 0 || slippage_bps > 10_000 {
            return None;
        }
        if max_slippage_bps
            .replace(slippage_bps)
            .is_some_and(|previous| previous != slippage_bps)
        {
            return None;
        }
        if dialect_constraint_indexes
            .insert(dialect, u8::try_from(constraint_index).ok()?)
            .is_some()
        {
            return None;
        }
    }

    let vault = single_pubkey_constraint(
        &route_v2.account_constraints[0],
        JupiterV2Dialect::RouteV2.account_layout().authority,
        None,
    )?;
    if shared_accounts_route_v2.account_constraints[0].kind
        != SquadsAccountConstraintKindView::Pubkey(vec![vault])
        || shared_accounts_route_v2.account_constraints[0]
            .owner
            .is_some()
    {
        return None;
    }

    let source_mints = payload
        .spending_limits
        .iter()
        .map(|limit| limit.mint)
        .collect::<BTreeSet<_>>();
    let source_shard = source_shard_for_mints(&source_mints)?;
    if payload.spending_limits.len() != 3 {
        return None;
    }
    let mut daily_source_mint_spending_cap = None;
    for spending_limit in &payload.spending_limits {
        if spending_limit.start < 0
            || spending_limit.expiration.is_some()
            || spending_limit.period != SquadsPeriodV2View::Daily
            || spending_limit.max_per_period == 0
        {
            return None;
        }
        if daily_source_mint_spending_cap
            .replace(spending_limit.max_per_period)
            .is_some_and(|previous| previous != spending_limit.max_per_period)
        {
            return None;
        }
    }

    Some(GeneralizedJupiterCrossMintPolicy {
        vault,
        source_shard,
        max_slippage_bps: max_slippage_bps?,
        daily_source_mint_spending_cap: daily_source_mint_spending_cap?,
        dialect_constraint_indexes,
    })
}

fn canonical_vault_atas(vault: Pubkey) -> Vec<Pubkey> {
    earn_stablecoins()
        .iter()
        .map(|stablecoin| {
            derive_associated_token_account(vault, stablecoin.mint, stablecoin.token_program)
        })
        .collect()
}

fn pubkey_allowlist_constraint(
    constraint: &SquadsAccountConstraintView,
    account_index: u8,
    expected: &[Pubkey],
) -> Option<()> {
    if constraint.account_index != account_index || constraint.owner.is_some() {
        return None;
    }
    let SquadsAccountConstraintKindView::Pubkey(pubkeys) = &constraint.kind else {
        return None;
    };
    let actual = pubkeys.iter().copied().collect::<BTreeSet<_>>();
    let expected_set = expected.iter().copied().collect::<BTreeSet<_>>();
    if pubkeys.len() != actual.len() || actual != expected_set {
        return None;
    }
    Some(())
}

fn source_shard_for_mints(mints: &BTreeSet<Pubkey>) -> Option<JupiterCrossMintSourceShard> {
    [
        JupiterCrossMintSourceShard::Classic,
        JupiterCrossMintSourceShard::Token2022,
    ]
    .into_iter()
    .find(|shard| shard.source_mints().into_iter().collect::<BTreeSet<_>>() == *mints)
}

fn compact_pubkey_table_is_tight(payload: &SquadsProgramInteractionPolicyView) -> bool {
    if payload.pubkey_table.is_empty() {
        return true;
    }
    let table = payload
        .pubkey_table
        .iter()
        .copied()
        .collect::<BTreeSet<_>>();
    if table.len() != payload.pubkey_table.len() {
        return false;
    }
    let mut referenced = BTreeSet::new();
    for constraint in &payload.constraints {
        referenced.insert(constraint.program_id);
        for account in &constraint.account_constraints {
            if let Some(owner) = account.owner {
                referenced.insert(owner);
            }
            if let SquadsAccountConstraintKindView::Pubkey(pubkeys) = &account.kind {
                referenced.extend(pubkeys.iter().copied());
            }
        }
    }
    referenced.extend(payload.spending_limits.iter().map(|limit| limit.mint));
    table == referenced
}

fn single_pubkey_constraint(
    constraint: &SquadsAccountConstraintView,
    account_index: u8,
    owner: Option<Pubkey>,
) -> Option<Pubkey> {
    if constraint.account_index != account_index || constraint.owner != owner {
        return None;
    }
    let SquadsAccountConstraintKindView::Pubkey(pubkeys) = &constraint.kind else {
        return None;
    };
    let [pubkey] = pubkeys.as_slice() else {
        return None;
    };
    Some(*pubkey)
}

fn policy_data_slice_equals(
    constraint: &SquadsDataConstraintView,
    offset: u64,
    expected: &[u8],
) -> bool {
    constraint.data_offset == offset
        && constraint.operator == SquadsDataOperatorView::Equals
        && constraint.data_value == SquadsDataValueView::U8Slice(expected.to_vec())
}

fn policy_data_u16_lte(constraint: &SquadsDataConstraintView, offset: u64) -> Option<u16> {
    if constraint.data_offset != offset
        || constraint.operator != SquadsDataOperatorView::LessThanOrEqualTo
    {
        return None;
    }
    let SquadsDataValueView::U16Le(value) = constraint.data_value else {
        return None;
    };
    Some(value)
}

fn policy_data_u8_equals(constraint: &SquadsDataConstraintView, offset: u64, expected: u8) -> bool {
    constraint.data_offset == offset
        && constraint.operator == SquadsDataOperatorView::Equals
        && constraint.data_value == SquadsDataValueView::U8(expected)
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
            8 => {
                let policy_account = cursor.read_pubkey()?;
                let delegated_signers = read_signers(&mut cursor)?;
                let threshold = cursor.read_u16()?;
                cursor.read_u32()?;
                let Some(payload) = read_policy_payload(&mut cursor)? else {
                    skip_policy_expiration_args(&mut cursor)?;
                    continue;
                };
                skip_policy_expiration_args(&mut cursor)?;
                actions.push(SquadsSettingsActionView {
                    settings,
                    authority,
                    policy_seed: 0,
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

fn derive_squads_vault(settings: &Pubkey, vault_index: u8) -> Pubkey {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            settings.as_ref(),
            SQUADS_SEED_PREFIX,
            &[vault_index],
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

fn read_policy_payload(
    cursor: &mut Cursor<'_>,
) -> Result<Option<SquadsProgramInteractionPolicyView>, PolicyDetectionError> {
    match cursor.read_u8()? {
        3 => {
            let vault_index = cursor.read_u8()?;
            let constraints = cursor.read_vec(read_raw_instruction_constraint)?;
            skip_raw_hook(cursor)?;
            skip_raw_hook(cursor)?;
            let spending_limits = cursor.read_vec(read_raw_spending_limit)?;
            Ok(Some(SquadsProgramInteractionPolicyView {
                vault_index,
                pubkey_table: vec![],
                constraints,
                spending_limits,
            }))
        }
        4 => {
            let vault_index = cursor.read_u8()?;
            let pubkey_table = cursor.read_small_pubkey_vec()?;
            let constraints = cursor
                .read_small_vec(|cursor| read_instruction_constraint(cursor, &pubkey_table))?;
            skip_compiled_hook(cursor)?;
            skip_compiled_hook(cursor)?;
            let spending_limits = cursor
                .read_small_vec(|cursor| read_compiled_spending_limit(cursor, &pubkey_table))?;
            Ok(Some(SquadsProgramInteractionPolicyView {
                vault_index,
                pubkey_table,
                constraints,
                spending_limits,
            }))
        }
        tag => {
            skip_policy_payload_body(tag, cursor)?;
            Ok(None)
        }
    }
}

fn read_raw_instruction_constraint(
    cursor: &mut Cursor<'_>,
) -> Result<SquadsInstructionConstraintView, PolicyDetectionError> {
    let program_id = cursor.read_pubkey()?;
    let account_constraints = cursor.read_vec(read_raw_account_constraint)?;
    let data_constraints = cursor.read_vec(read_data_constraint)?;
    Ok(SquadsInstructionConstraintView {
        program_id,
        account_constraints,
        data_constraints,
    })
}

fn read_raw_account_constraint(
    cursor: &mut Cursor<'_>,
) -> Result<SquadsAccountConstraintView, PolicyDetectionError> {
    let account_index = cursor.read_u8()?;
    let kind = match cursor.read_u8()? {
        0 => SquadsAccountConstraintKindView::Pubkey(cursor.read_pubkey_vec()?),
        1 => SquadsAccountConstraintKindView::AccountData(cursor.read_vec(read_data_constraint)?),
        _ => {
            return Err(PolicyDetectionError::InvalidInstructionData(
                "unknown account constraint kind",
            ))
        }
    };
    let owner = read_option(cursor, |cursor| cursor.read_pubkey())?;
    Ok(SquadsAccountConstraintView {
        account_index,
        kind,
        owner,
    })
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
        0 | 2 => {
            let len = cursor.read_u32()? as usize;
            cursor.skip(len)
        }
        1 => skip_spending_limit_policy_payload(cursor),
        3 => skip_raw_program_interaction_policy_payload(cursor),
        4 => skip_compiled_program_interaction_policy_payload(cursor),
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "unknown policy payload",
        )),
    }
}

fn skip_raw_program_interaction_policy_payload(
    cursor: &mut Cursor<'_>,
) -> Result<(), PolicyDetectionError> {
    cursor.read_u8()?;
    cursor.read_vec(read_raw_instruction_constraint)?;
    skip_raw_hook(cursor)?;
    skip_raw_hook(cursor)?;
    skip_raw_spending_limits(cursor)
}

fn skip_compiled_program_interaction_policy_payload(
    cursor: &mut Cursor<'_>,
) -> Result<(), PolicyDetectionError> {
    cursor.read_u8()?;
    let pubkey_table = cursor.read_small_pubkey_vec()?;
    cursor.read_small_vec(|cursor| read_instruction_constraint(cursor, &pubkey_table))?;
    skip_compiled_hook(cursor)?;
    skip_compiled_hook(cursor)?;
    skip_small_vec(cursor, skip_compiled_spending_limit)
}

fn skip_raw_hook(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    read_option(cursor, skip_raw_hook_body).map(|_| ())
}

fn skip_raw_hook_body(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    cursor.read_u8()?;
    cursor.read_vec(read_raw_account_constraint)?;
    cursor.read_vec_u8()?;
    cursor.skip(32)?;
    cursor.read_u8()?;
    Ok(())
}

fn skip_raw_spending_limits(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    cursor.read_vec(skip_raw_spending_limit_body).map(|_| ())
}

fn skip_raw_spending_limit_body(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    read_raw_spending_limit(cursor).map(|_| ())
}

fn read_raw_spending_limit(
    cursor: &mut Cursor<'_>,
) -> Result<SquadsLimitedSpendingLimitView, PolicyDetectionError> {
    Ok(SquadsLimitedSpendingLimitView {
        mint: cursor.read_pubkey()?,
        start: cursor.read_i64()?,
        expiration: read_option(cursor, |cursor| cursor.read_i64())?,
        period: read_period_v2(cursor)?,
        max_per_period: cursor.read_u64()?,
    })
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

fn read_compiled_spending_limit(
    cursor: &mut Cursor<'_>,
    pubkey_table: &[Pubkey],
) -> Result<SquadsLimitedSpendingLimitView, PolicyDetectionError> {
    Ok(SquadsLimitedSpendingLimitView {
        mint: indexed_pubkey(pubkey_table, cursor.read_u8()?)?,
        start: cursor.read_i64()?,
        expiration: read_option(cursor, |cursor| cursor.read_i64())?,
        period: read_period_v2(cursor)?,
        max_per_period: cursor.read_u64()?,
    })
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
    read_period_v2(cursor).map(|_| ())
}

fn read_period_v2(cursor: &mut Cursor<'_>) -> Result<SquadsPeriodV2View, PolicyDetectionError> {
    match cursor.read_u8()? {
        0 => Ok(SquadsPeriodV2View::OneTime),
        1 => Ok(SquadsPeriodV2View::Daily),
        2 => Ok(SquadsPeriodV2View::Weekly),
        3 => Ok(SquadsPeriodV2View::Monthly),
        4 => Ok(SquadsPeriodV2View::Custom(cursor.read_i64()?)),
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

fn anchor_instruction_discriminator(name: &str) -> [u8; 8] {
    let preimage = format!("global:{name}");
    let hash = hashv(&[preimage.as_bytes()]).to_bytes();
    hash[..8].try_into().expect("slice length")
}

fn anchor_account_discriminator(name: &str) -> [u8; 8] {
    let preimage = format!("account:{name}");
    let hash = hashv(&[preimage.as_bytes()]).to_bytes();
    hash[..8].try_into().expect("slice length")
}

#[derive(Clone, Copy)]
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

    fn read_i64(&mut self) -> Result<i64, PolicyDetectionError> {
        Ok(i64::from_le_bytes(self.read_array()?))
    }

    fn read_u128(&mut self) -> Result<u128, PolicyDetectionError> {
        Ok(u128::from_le_bytes(self.read_array()?))
    }

    fn read_pubkey(&mut self) -> Result<Pubkey, PolicyDetectionError> {
        Ok(Pubkey::new_from_array(self.read_array()?))
    }

    fn read_pubkey_vec(&mut self) -> Result<Vec<Pubkey>, PolicyDetectionError> {
        let len = self.read_u32()? as usize;
        if len > self.remaining() / 32 {
            return Err(PolicyDetectionError::InvalidInstructionData(
                "invalid pubkey vector length",
            ));
        }
        (0..len).map(|_| self.read_pubkey()).collect()
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

    fn read_vec<T>(
        &mut self,
        mut read_item: impl FnMut(&mut Cursor<'a>) -> Result<T, PolicyDetectionError>,
    ) -> Result<Vec<T>, PolicyDetectionError> {
        let len = self.read_u32()? as usize;
        if len > self.remaining() {
            return Err(PolicyDetectionError::InvalidInstructionData(
                "invalid vector length",
            ));
        }
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
    use crate::jupiter::{
        JupiterCrossMintPolicySpec, JupiterCrossMintSourceShard, JupiterV2Dialect,
    };
    use crate::{
        create_all_in_one_market_mint_yield_route_action, create_jupiter_cross_mint_policy_action,
        JupiterSwapContract, LoyalActionContext, SwapLane, YieldRouteUniverse,
        JUPITER_V6_PROGRAM_ID,
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

    fn policy_context(account_index: u8) -> LoyalActionContext {
        let settings = Pubkey::new_unique();
        LoyalActionContext {
            settings,
            authority: Pubkey::new_unique(),
            delegated_signer: Pubkey::new_unique(),
            account_index,
            vault: derive_squads_vault(&settings, account_index),
        }
    }

    fn generalized_cross_mint_spec(
        source_shard: JupiterCrossMintSourceShard,
    ) -> JupiterCrossMintPolicySpec {
        JupiterCrossMintPolicySpec {
            source_shard,
            max_slippage_bps: 75,
            daily_source_mint_spending_cap_base_units: 1_000_000_000,
        }
    }

    fn decoded_generalized_cross_mint_payload(
        context: LoyalActionContext,
        source_shard: JupiterCrossMintSourceShard,
    ) -> SquadsProgramInteractionPolicyView {
        let setup = create_jupiter_cross_mint_policy_action(
            context,
            generalized_cross_mint_spec(source_shard),
            101,
        )
        .unwrap();
        decode_squads_policy_create_actions(&setup.instruction)
            .unwrap()
            .remove(0)
            .payload
    }

    #[derive(Clone, Copy)]
    enum PolicyAccountConstraintEncoding {
        Legacy,
        Compact,
    }

    fn serialized_policy_account(
        context: LoyalActionContext,
        policy_seed: u64,
        payload: &SquadsProgramInteractionPolicyView,
        encoding: PolicyAccountConstraintEncoding,
        permission_mask: u8,
        threshold: u16,
    ) -> Vec<u8> {
        let (_, bump) = derive_action_account(&context.settings, policy_seed);
        let mut data = Vec::new();
        data.extend_from_slice(&anchor_account_discriminator("Policy"));
        push_pubkey(&mut data, context.settings);
        data.extend_from_slice(&policy_seed.to_le_bytes());
        data.push(bump);
        data.extend_from_slice(&4_u64.to_le_bytes());
        data.extend_from_slice(&3_u64.to_le_bytes());
        data.extend_from_slice(&1_u32.to_le_bytes());
        push_pubkey(&mut data, context.delegated_signer);
        data.push(permission_mask);
        data.extend_from_slice(&threshold.to_le_bytes());
        data.extend_from_slice(&0_u32.to_le_bytes());
        data.push(3); // PolicyState::ProgramInteraction
        data.push(payload.vault_index);
        match encoding {
            PolicyAccountConstraintEncoding::Legacy => {
                push_raw_instruction_constraints(&mut data, &payload.constraints);
                data.push(0); // pre-hook
                data.push(0); // post-hook
                push_u32_len(&mut data, payload.spending_limits.len());
                for limit in &payload.spending_limits {
                    push_policy_account_spending_limit(&mut data, limit);
                }
            }
            PolicyAccountConstraintEncoding::Compact => {
                let pubkey_table = push_compact_instruction_constraints(
                    &mut data,
                    &payload.constraints,
                    &payload.spending_limits,
                );
                data.push(0); // pre-hook
                data.push(0); // post-hook
                data.push(u8::try_from(payload.spending_limits.len()).unwrap());
                for limit in &payload.spending_limits {
                    data.push(
                        u8::try_from(
                            pubkey_table
                                .iter()
                                .position(|pubkey| *pubkey == limit.mint)
                                .unwrap(),
                        )
                        .unwrap(),
                    );
                    push_limited_spending_limit_body(&mut data, limit);
                }
            }
        }
        data.extend_from_slice(&1_i64.to_le_bytes());
        data.push(0); // expiration
        push_pubkey(&mut data, Pubkey::new_unique());
        data.extend_from_slice(&[0xa5; 24]); // retained account-allocation bytes
        data
    }

    fn push_raw_instruction_constraints(
        data: &mut Vec<u8>,
        constraints: &[SquadsInstructionConstraintView],
    ) {
        push_u32_len(data, constraints.len());
        for constraint in constraints {
            push_pubkey(data, constraint.program_id);
            push_u32_len(data, constraint.account_constraints.len());
            for account in &constraint.account_constraints {
                push_raw_account_constraint(data, account);
            }
            push_u32_len(data, constraint.data_constraints.len());
            for constraint in &constraint.data_constraints {
                push_data_constraint(data, constraint);
            }
        }
    }

    fn push_raw_account_constraint(data: &mut Vec<u8>, constraint: &SquadsAccountConstraintView) {
        data.push(constraint.account_index);
        match &constraint.kind {
            SquadsAccountConstraintKindView::Pubkey(pubkeys) => {
                data.push(0);
                push_u32_len(data, pubkeys.len());
                for pubkey in pubkeys {
                    push_pubkey(data, *pubkey);
                }
            }
            SquadsAccountConstraintKindView::AccountData(constraints) => {
                data.push(1);
                push_u32_len(data, constraints.len());
                for constraint in constraints {
                    push_data_constraint(data, constraint);
                }
            }
        }
        push_optional_pubkey(data, constraint.owner);
    }

    fn push_compact_instruction_constraints(
        data: &mut Vec<u8>,
        constraints: &[SquadsInstructionConstraintView],
        spending_limits: &[SquadsLimitedSpendingLimitView],
    ) -> Vec<Pubkey> {
        let mut pubkey_table = Vec::new();
        for constraint in constraints {
            test_pubkey_index(&mut pubkey_table, constraint.program_id);
            for account in &constraint.account_constraints {
                if let SquadsAccountConstraintKindView::Pubkey(pubkeys) = &account.kind {
                    for pubkey in pubkeys {
                        test_pubkey_index(&mut pubkey_table, *pubkey);
                    }
                }
                if let Some(owner) = account.owner {
                    test_pubkey_index(&mut pubkey_table, owner);
                }
            }
        }
        for limit in spending_limits {
            test_pubkey_index(&mut pubkey_table, limit.mint);
        }

        data.push(u8::try_from(pubkey_table.len()).unwrap());
        for pubkey in &pubkey_table {
            push_pubkey(data, *pubkey);
        }
        data.push(u8::try_from(constraints.len()).unwrap());
        for constraint in constraints {
            data.push(test_pubkey_index(&mut pubkey_table, constraint.program_id));
            data.push(u8::try_from(constraint.account_constraints.len()).unwrap());
            for account in &constraint.account_constraints {
                data.push(account.account_index);
                match &account.kind {
                    SquadsAccountConstraintKindView::Pubkey(pubkeys) => {
                        data.push(0);
                        data.push(u8::try_from(pubkeys.len()).unwrap());
                        for pubkey in pubkeys {
                            data.push(test_pubkey_index(&mut pubkey_table, *pubkey));
                        }
                    }
                    SquadsAccountConstraintKindView::AccountData(constraints) => {
                        data.push(1);
                        data.push(u8::try_from(constraints.len()).unwrap());
                        for constraint in constraints {
                            push_data_constraint(data, constraint);
                        }
                    }
                }
                match account.owner {
                    Some(owner) => {
                        data.push(1);
                        data.push(test_pubkey_index(&mut pubkey_table, owner));
                    }
                    None => data.push(0),
                }
            }
            data.push(u8::try_from(constraint.data_constraints.len()).unwrap());
            for constraint in &constraint.data_constraints {
                push_data_constraint(data, constraint);
            }
        }
        pubkey_table
    }

    fn push_policy_account_spending_limit(
        data: &mut Vec<u8>,
        limit: &SquadsLimitedSpendingLimitView,
    ) {
        push_pubkey(data, limit.mint);
        data.extend_from_slice(&limit.start.to_le_bytes());
        match limit.expiration {
            Some(expiration) => {
                data.push(1);
                data.extend_from_slice(&expiration.to_le_bytes());
            }
            None => data.push(0),
        }
        push_period_v2(data, limit.period);
        data.push(0); // accumulate unused
        data.extend_from_slice(&limit.max_per_period.to_le_bytes());
        data.extend_from_slice(&0_u64.to_le_bytes()); // max per use
        data.push(0); // enforce exact quantity
        data.extend_from_slice(&limit.max_per_period.to_le_bytes());
        data.extend_from_slice(&limit.start.to_le_bytes());
    }

    fn push_limited_spending_limit_body(
        data: &mut Vec<u8>,
        limit: &SquadsLimitedSpendingLimitView,
    ) {
        data.extend_from_slice(&limit.start.to_le_bytes());
        match limit.expiration {
            Some(expiration) => {
                data.push(1);
                data.extend_from_slice(&expiration.to_le_bytes());
            }
            None => data.push(0),
        }
        push_period_v2(data, limit.period);
        data.extend_from_slice(&limit.max_per_period.to_le_bytes());
    }

    fn push_period_v2(data: &mut Vec<u8>, period: SquadsPeriodV2View) {
        match period {
            SquadsPeriodV2View::OneTime => data.push(0),
            SquadsPeriodV2View::Daily => data.push(1),
            SquadsPeriodV2View::Weekly => data.push(2),
            SquadsPeriodV2View::Monthly => data.push(3),
            SquadsPeriodV2View::Custom(seconds) => {
                data.push(4);
                data.extend_from_slice(&seconds.to_le_bytes());
            }
        }
    }

    fn test_pubkey_index(pubkey_table: &mut Vec<Pubkey>, pubkey: Pubkey) -> u8 {
        if let Some(index) = pubkey_table.iter().position(|existing| *existing == pubkey) {
            return u8::try_from(index).unwrap();
        }
        let index = pubkey_table.len();
        pubkey_table.push(pubkey);
        u8::try_from(index).unwrap()
    }

    fn push_data_constraint(data: &mut Vec<u8>, constraint: &SquadsDataConstraintView) {
        data.extend_from_slice(&constraint.data_offset.to_le_bytes());
        match &constraint.data_value {
            SquadsDataValueView::U8(value) => {
                data.push(0);
                data.push(*value);
            }
            SquadsDataValueView::U16Le(value) => {
                data.push(1);
                data.extend_from_slice(&value.to_le_bytes());
            }
            SquadsDataValueView::U32Le(value) => {
                data.push(2);
                data.extend_from_slice(&value.to_le_bytes());
            }
            SquadsDataValueView::U64Le(value) => {
                data.push(3);
                data.extend_from_slice(&value.to_le_bytes());
            }
            SquadsDataValueView::U128Le(value) => {
                data.push(4);
                data.extend_from_slice(&value.to_le_bytes());
            }
            SquadsDataValueView::U8Slice(value) => {
                data.push(5);
                push_u32_len(data, value.len());
                data.extend_from_slice(value);
            }
        }
        data.push(match constraint.operator {
            SquadsDataOperatorView::Equals => 0,
            SquadsDataOperatorView::NotEquals => 1,
            SquadsDataOperatorView::GreaterThan => 2,
            SquadsDataOperatorView::GreaterThanOrEqualTo => 3,
            SquadsDataOperatorView::LessThan => 4,
            SquadsDataOperatorView::LessThanOrEqualTo => 5,
        });
    }

    fn push_optional_pubkey(data: &mut Vec<u8>, pubkey: Option<Pubkey>) {
        match pubkey {
            Some(pubkey) => {
                data.push(1);
                push_pubkey(data, pubkey);
            }
            None => data.push(0),
        }
    }

    fn push_pubkey(data: &mut Vec<u8>, pubkey: Pubkey) {
        data.extend_from_slice(pubkey.as_ref());
    }

    fn push_u32_len(data: &mut Vec<u8>, len: usize) {
        data.extend_from_slice(&u32::try_from(len).unwrap().to_le_bytes());
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

    #[test]
    fn detects_generalized_cross_mint_policy_account_in_legacy_and_compact_encodings() {
        let context = policy_context(4);
        let payload =
            decoded_generalized_cross_mint_payload(context, JupiterCrossMintSourceShard::Token2022);
        for encoding in [
            PolicyAccountConstraintEncoding::Legacy,
            PolicyAccountConstraintEncoding::Compact,
        ] {
            let account_data = serialized_policy_account(
                context,
                102,
                &payload,
                encoding,
                SQUADS_FULL_PERMISSIONS_MASK,
                1,
            );
            let detected = detect_jupiter_cross_mint_policy_account(&account_data)
                .unwrap()
                .expect("detect generalized on-chain policy");
            assert_eq!(detected.settings, context.settings);
            assert_eq!(detected.policy_seed, 102);
            assert_eq!(detected.vault, context.vault);
            assert_eq!(detected.delegated_signer, context.delegated_signer);
            assert_eq!(detected.threshold, 1);
            assert_eq!(
                detected.source_shard,
                JupiterCrossMintSourceShard::Token2022
            );
            assert_eq!(detected.max_slippage_bps, 75);
            assert_eq!(detected.daily_source_mint_spending_cap, 1_000_000_000);
            assert_eq!(
                detected.dialect_constraint_indexes,
                BTreeMap::from([
                    (JupiterV2Dialect::RouteV2, 0),
                    (JupiterV2Dialect::SharedAccountsRouteV2, 1),
                ])
            );
        }

        let mut extra_limit = payload.clone();
        extra_limit
            .spending_limits
            .push(extra_limit.spending_limits[0]);
        let extra_limit_data = serialized_policy_account(
            context,
            103,
            &extra_limit,
            PolicyAccountConstraintEncoding::Legacy,
            SQUADS_FULL_PERMISSIONS_MASK,
            1,
        );
        assert!(detect_jupiter_cross_mint_policy_account(&extra_limit_data)
            .unwrap()
            .is_none());

        let mut future_start = payload;
        future_start.spending_limits[0].start = -1;
        let future_start_data = serialized_policy_account(
            context,
            104,
            &future_start,
            PolicyAccountConstraintEncoding::Legacy,
            SQUADS_FULL_PERMISSIONS_MASK,
            1,
        );
        assert!(detect_jupiter_cross_mint_policy_account(&future_start_data)
            .unwrap()
            .is_none());
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
