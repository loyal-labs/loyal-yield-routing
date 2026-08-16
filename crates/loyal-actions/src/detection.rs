use crate::{
    derive_action_account, derive_associated_token_account, derive_loyal_hub_config,
    detect_yield_route_universe_preset, earn_stablecoins,
    ids::*,
    jupiter::{JupiterCrossMintSourceShard, JupiterV2Dialect},
    protocols::{
        derive_kamino_user_metadata, derive_kamino_vanilla_obligation,
        derive_subscription_authority, derive_subscription_event_authority,
    },
    JupiterSwapContract, YieldRouteUniversePreset,
};
use sha2::{Digest, Sha256};
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
    pub universe_preset: Option<YieldRouteUniversePreset>,
    pub swap_lanes: Vec<DetectedSwapLane>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DetectedBalanceSweepPolicy {
    pub settings: Pubkey,
    pub authority: Pubkey,
    pub policy_seed: u64,
    pub policy_account: Pubkey,
    pub vault_index: u8,
    pub vault_pubkey: Pubkey,
    pub wallet: Pubkey,
    pub wallet_usdc_ata: Pubkey,
    pub vault_usdc_ata: Pubkey,
    pub delegated_signers: Vec<Pubkey>,
    pub threshold: u16,
    pub max_amount_per_period: u64,
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

/// Identity carried by a strict Jupiter policy settings action.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DetectedJupiterPolicyIdentity {
    Create {
        policy_seed: u64,
        action_account: Pubkey,
    },
    Update {
        policy_account: Pubkey,
    },
}

impl DetectedJupiterPolicyIdentity {
    pub const fn policy_seed(self) -> Option<u64> {
        match self {
            Self::Create { policy_seed, .. } => Some(policy_seed),
            Self::Update { .. } => None,
        }
    }

    pub const fn policy_account(self) -> Pubkey {
        match self {
            Self::Create { action_account, .. } => action_account,
            Self::Update { policy_account } => policy_account,
        }
    }
}

/// The strict, durable generalized Jupiter authority emitted by the cross-mint
/// policy builder.
///
/// The policy deliberately describes a stablecoin universe and source-mint
/// spending envelope rather than individual pairs. The actual pair and
/// dialect are still selected from this manifest when a route is built.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DetectedJupiterCrossMintPolicy {
    pub settings: Pubkey,
    pub authority: Pubkey,
    pub identity: DetectedJupiterPolicyIdentity,
    pub account_index: u8,
    pub vault: Pubkey,
    pub delegated_signer: Pubkey,
    pub source_shard: JupiterCrossMintSourceShard,
    pub max_slippage_bps: u16,
    pub daily_source_mint_spending_cap: u64,
    pub dialect_constraint_indexes: BTreeMap<JupiterV2Dialect, u8>,
}

/// Durable identity and capability decoded from an on-chain generalized
/// Jupiter policy account.
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

/// Manifest-relevant semantics decoded from a generalized policy.
///
/// Database projection fields are intentionally not part of this value.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DetectedJupiterCrossMintPolicySemantics {
    pub settings: Pubkey,
    pub vault: Pubkey,
    pub delegated_signer: Pubkey,
    pub account_index: u8,
    pub source_shard: JupiterCrossMintSourceShard,
    pub max_slippage_bps: u16,
    pub daily_source_mint_spending_cap: u64,
    pub dialect_constraint_indexes: BTreeMap<JupiterV2Dialect, u8>,
}

impl DetectedJupiterCrossMintPolicy {
    pub fn manifest_semantics(&self) -> DetectedJupiterCrossMintPolicySemantics {
        DetectedJupiterCrossMintPolicySemantics {
            settings: self.settings,
            vault: self.vault,
            delegated_signer: self.delegated_signer,
            account_index: self.account_index,
            source_shard: self.source_shard,
            max_slippage_bps: self.max_slippage_bps,
            daily_source_mint_spending_cap: self.daily_source_mint_spending_cap,
            dialect_constraint_indexes: self.dialect_constraint_indexes.clone(),
        }
    }
}

impl DetectedJupiterCrossMintPolicyAccount {
    pub fn manifest_semantics(&self) -> DetectedJupiterCrossMintPolicySemantics {
        DetectedJupiterCrossMintPolicySemantics {
            settings: self.settings,
            vault: self.vault,
            delegated_signer: self.delegated_signer,
            account_index: self.account_index,
            source_shard: self.source_shard,
            max_slippage_bps: self.max_slippage_bps,
            daily_source_mint_spending_cap: self.daily_source_mint_spending_cap,
            dialect_constraint_indexes: self.dialect_constraint_indexes.clone(),
        }
    }
}

/// Compute the canonical fingerprint for a fully detected generalized policy.
///
/// Keep this byte-level contract stable: the monitor and worker must agree
/// without fingerprinting raw database fields.
pub fn generalized_cross_mint_manifest_fingerprint(
    policy: &DetectedJupiterCrossMintPolicySemantics,
) -> String {
    let mut digest = Sha256::new();
    for field in [
        "canonical_stables_v1".to_owned(),
        policy.settings.to_string(),
        policy.vault.to_string(),
        policy.delegated_signer.to_string(),
        policy.account_index.to_string(),
        policy.max_slippage_bps.to_string(),
    ] {
        digest.update(field.as_bytes());
        digest.update([0]);
    }
    for source_mint in policy.source_shard.source_mints() {
        digest.update(source_mint.to_string().as_bytes());
        digest.update([0]);
        digest.update(
            policy
                .source_shard
                .source_token_program()
                .to_string()
                .as_bytes(),
        );
        digest.update([0]);
        digest.update(policy.daily_source_mint_spending_cap.to_le_bytes());
    }
    for target in earn_stablecoins() {
        digest.update(target.mint.to_string().as_bytes());
        digest.update([0]);
        digest.update(target.token_program.to_string().as_bytes());
        digest.update([0]);
    }
    for (dialect, constraint_index) in &policy.dialect_constraint_indexes {
        digest.update(jupiter_dialect_name(*dialect).as_bytes());
        digest.update([0, *constraint_index]);
    }
    format!("{:x}", digest.finalize())
}

fn jupiter_dialect_name(value: JupiterV2Dialect) -> &'static str {
    match value {
        JupiterV2Dialect::RouteV2 => "route_v2",
        JupiterV2Dialect::SharedAccountsRouteV2 => "shared_accounts_route_v2",
    }
}

/// A policy removal cannot be classified by policy family from its wire data;
/// callers must correlate this account with a previously detected policy.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct DetectedPolicyRemoval {
    pub settings: Pubkey,
    pub authority: Pubkey,
    pub policy_account: Pubkey,
}

/// Detect the strict generalized cross-mint Jupiter policy create/update
/// shape. This intentionally does not accept one-dialect policies or policies
/// with a broader account/mint envelope.
pub fn detect_jupiter_cross_mint_policy_action(
    instruction: &Instruction,
) -> Result<Option<DetectedJupiterCrossMintPolicy>, PolicyDetectionError> {
    let Some(envelope) = strict_settings_envelope(instruction) else {
        return Ok(None);
    };
    let mut cursor = Cursor::new(&instruction.data);
    require_settings_discriminator(&mut cursor)?;
    if cursor.read_u8()? != SQUADS_SYNC_SIGNER_COUNT || cursor.read_u32()? != 1 {
        return Ok(None);
    }

    let (identity, payload, delegated_signer, exact_metadata) = match cursor.read_u8()? {
        7 => {
            let policy_seed = cursor.read_u64()?;
            let payload = read_program_interaction_payload(&mut cursor)?;
            let delegated_signer = read_delegated_signer(&mut cursor)?;
            let threshold = cursor.read_u16()?;
            let time_lock = cursor.read_u32()?;
            let start_timestamp = read_option(&mut cursor, |cursor| cursor.skip(8))?;
            let expiration = read_policy_expiration(&mut cursor)?;
            let action_account = derive_action_account(&envelope.settings, policy_seed).0;
            (
                DetectedJupiterPolicyIdentity::Create {
                    policy_seed,
                    action_account,
                },
                payload,
                delegated_signer,
                threshold == 1
                    && time_lock == 0
                    && start_timestamp.is_none()
                    && expiration.is_none(),
            )
        }
        8 => {
            let policy_account = cursor.read_pubkey()?;
            let delegated_signer = read_delegated_signer(&mut cursor)?;
            let threshold = cursor.read_u16()?;
            let time_lock = cursor.read_u32()?;
            let payload = read_program_interaction_payload(&mut cursor)?;
            let expiration = read_policy_expiration(&mut cursor)?;
            (
                DetectedJupiterPolicyIdentity::Update { policy_account },
                payload,
                delegated_signer,
                threshold == 1 && time_lock == 0 && expiration.is_none(),
            )
        }
        _ => return Ok(None),
    };

    let memo = read_option(&mut cursor, |cursor| {
        let len = cursor.read_u32()? as usize;
        cursor.skip(len)
    })?;
    if !exact_metadata
        || memo.is_some()
        || cursor.remaining() != 0
        || identity.policy_account() != envelope.policy_account
    {
        return Ok(None);
    }
    let Some(payload) = payload else {
        return Ok(None);
    };
    let Some(delegated_signer) = delegated_signer else {
        return Ok(None);
    };
    let Some(policy) =
        classify_jupiter_cross_mint_policy(&payload, SpendingLimitStartContract::CreationRequest)
    else {
        return Ok(None);
    };
    if policy.vault != derive_squads_vault(&envelope.settings, payload.vault_index) {
        return Ok(None);
    }

    Ok(Some(DetectedJupiterCrossMintPolicy {
        settings: envelope.settings,
        authority: envelope.authority,
        identity,
        account_index: payload.vault_index,
        vault: policy.vault,
        delegated_signer,
        source_shard: policy.source_shard,
        max_slippage_bps: policy.max_slippage_bps,
        daily_source_mint_spending_cap: policy.daily_source_mint_spending_cap,
        dialect_constraint_indexes: policy.dialect_constraint_indexes,
    }))
}

/// Decode and strictly classify an on-chain generalized cross-mint Jupiter
/// policy account. Both the legacy and compact ProgramInteraction account
/// encodings are accepted; their semantic policy must be identical.
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
        let Some(policy) = classify_jupiter_cross_mint_policy(
            &candidate.payload,
            SpendingLimitStartContract::OnChainEffective,
        ) else {
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

/// Detect a canonical one-action Squads policy removal.
///
/// The instruction does not include the removed policy payload, so this result
/// deliberately makes no claim about whether the account was a Jupiter policy.
pub fn detect_squads_policy_remove(
    instruction: &Instruction,
) -> Result<Option<DetectedPolicyRemoval>, PolicyDetectionError> {
    let Some(envelope) = strict_settings_envelope(instruction) else {
        return Ok(None);
    };
    let mut cursor = Cursor::new(&instruction.data);
    require_settings_discriminator(&mut cursor)?;
    if cursor.read_u8()? != SQUADS_SYNC_SIGNER_COUNT
        || cursor.read_u32()? != 1
        || cursor.read_u8()? != 9
    {
        return Ok(None);
    }
    let policy_account = cursor.read_pubkey()?;
    let memo = read_option(&mut cursor, |cursor| {
        let len = cursor.read_u32()? as usize;
        cursor.skip(len)
    })?;
    if policy_account != envelope.policy_account || memo.is_some() || cursor.remaining() != 0 {
        return Ok(None);
    }

    Ok(Some(DetectedPolicyRemoval {
        settings: envelope.settings,
        authority: envelope.authority,
        policy_account,
    }))
}

/// Detect the identity of any canonical one-action Squads policy update.
///
/// This intentionally does not classify the new payload. Monitors use it only
/// after every recognized policy-family detector declined the instruction, so
/// a previously known capability is invalidated instead of remaining active
/// after an incompatible update.
pub fn detect_squads_policy_update_identity(
    instruction: &Instruction,
) -> Result<Option<DetectedPolicyRemoval>, PolicyDetectionError> {
    let Some(envelope) = strict_settings_envelope(instruction) else {
        return Ok(None);
    };
    let mut cursor = Cursor::new(&instruction.data);
    require_settings_discriminator(&mut cursor)?;
    if cursor.read_u8()? != SQUADS_SYNC_SIGNER_COUNT
        || cursor.read_u32()? != 1
        || cursor.read_u8()? != 8
    {
        return Ok(None);
    }
    let policy_account = cursor.read_pubkey()?;
    if policy_account != envelope.policy_account {
        return Ok(None);
    }
    Ok(Some(DetectedPolicyRemoval {
        settings: envelope.settings,
        authority: envelope.authority,
        policy_account,
    }))
}

#[derive(Clone, Copy)]
struct StrictSettingsEnvelope {
    settings: Pubkey,
    authority: Pubkey,
    policy_account: Pubkey,
}

fn strict_settings_envelope(instruction: &Instruction) -> Option<StrictSettingsEnvelope> {
    if instruction.program_id != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        return None;
    }
    let [settings, authority, system_program, squads_program, repeated_authority, policy_account] =
        instruction.accounts.as_slice()
    else {
        return None;
    };
    if system_program.pubkey != solana_sdk::system_program::ID
        || squads_program.pubkey != SQUADS_SMART_ACCOUNT_PROGRAM_ID
        || repeated_authority.pubkey != authority.pubkey
        || !authority.is_signer
        || !repeated_authority.is_signer
        || !policy_account.is_writable
    {
        return None;
    }
    Some(StrictSettingsEnvelope {
        settings: settings.pubkey,
        authority: authority.pubkey,
        policy_account: policy_account.pubkey,
    })
}

fn require_settings_discriminator(cursor: &mut Cursor<'_>) -> Result<(), PolicyDetectionError> {
    if cursor.read_array::<8>()?
        != anchor_instruction_discriminator("execute_settings_transaction_sync")
    {
        return Err(PolicyDetectionError::UnsupportedSettingsInstruction);
    }
    Ok(())
}

fn read_delegated_signer(cursor: &mut Cursor<'_>) -> Result<Option<Pubkey>, PolicyDetectionError> {
    let len = cursor.read_u32()? as usize;
    let mut signer = None;
    let mut exact = len == 1;
    for index in 0..len {
        let key = cursor.read_pubkey()?;
        let permissions = cursor.read_u8()?;
        if index == 0 {
            signer = Some(key);
        }
        exact &= permissions == SQUADS_FULL_PERMISSIONS_MASK;
    }
    Ok(exact.then_some(signer).flatten())
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

fn read_policy_expiration(cursor: &mut Cursor<'_>) -> Result<Option<()>, PolicyDetectionError> {
    read_option(cursor, |cursor| match cursor.read_u8()? {
        0 => cursor.skip(8),
        1 => Ok(()),
        _ => Err(PolicyDetectionError::InvalidInstructionData(
            "unknown policy expiration args",
        )),
    })
}

fn read_program_interaction_payload(
    cursor: &mut Cursor<'_>,
) -> Result<Option<SquadsProgramInteractionPolicyView>, PolicyDetectionError> {
    let payload_tag = cursor.read_u8()?;
    if payload_tag != 3 {
        skip_policy_payload_body(payload_tag, cursor)?;
        return Ok(None);
    }

    let vault_index = cursor.read_u8()?;
    let constraints = cursor.read_vec(read_raw_instruction_constraint)?;
    let pre_hook = read_option(cursor, skip_raw_hook_body)?;
    let post_hook = read_option(cursor, skip_raw_hook_body)?;
    let spending_limits = cursor.read_vec(read_raw_spending_limit)?;
    if pre_hook.is_some() || post_hook.is_some() {
        return Ok(None);
    }

    Ok(Some(SquadsProgramInteractionPolicyView {
        vault_index,
        pubkey_table: vec![],
        constraints,
        spending_limits,
    }))
}

#[derive(Clone, Copy)]
enum SpendingLimitStartContract {
    CreationRequest,
    OnChainEffective,
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
    start_contract: SpendingLimitStartContract,
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
        let valid_start = match start_contract {
            SpendingLimitStartContract::CreationRequest => spending_limit.start == 0,
            SpendingLimitStartContract::OnChainEffective => spending_limit.start >= 0,
        };
        if !valid_start
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

/// Compact policy accounts use a shared pubkey table. A strict detector must
/// not silently accept unused or duplicate table entries as policy metadata.
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

pub fn detect_yield_route_policy_create(
    action: &SquadsSettingsActionView,
) -> Option<DetectedYieldRoutePolicy> {
    let constraints = &action.payload.constraints;
    if constraints.len() < 2 {
        return None;
    }

    let mut withdraw = None;
    let mut deposit = None;
    let mut init_markets = Vec::new();
    let mut refresh_markets = Vec::new();
    let mut stable_mints = Vec::new();
    let mut swap_lanes = Vec::new();
    let mut has_jupiter = false;
    let mut has_hub = false;

    for constraint in constraints {
        if withdraw.is_none() {
            if let Some(kamino_withdraw) = classify_kamino_withdraw(constraint) {
                withdraw = Some(kamino_withdraw);
                continue;
            }
        }
    }
    let withdraw = withdraw?;

    for constraint in constraints {
        if let Some(kamino_deposit) = classify_kamino_deposit(constraint, withdraw.vault) {
            if deposit.is_some() {
                return None;
            }
            deposit = Some(kamino_deposit);
            continue;
        }
        if classify_kamino_withdraw(constraint).is_some() {
            continue;
        }
        if let Some(markets) = classify_kamino_init_obligation(constraint, withdraw.vault) {
            init_markets.extend(markets);
            continue;
        }
        if let Some(markets) = classify_kamino_refresh_obligation(constraint, withdraw.vault) {
            refresh_markets.extend(markets);
            continue;
        }
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
    let deposit = deposit?;

    let mut route_modes = vec![DetectedYieldRouteMode::SameMint];
    if has_jupiter {
        route_modes.push(DetectedYieldRouteMode::CrossMintJupiter);
    }
    if has_hub {
        route_modes.push(DetectedYieldRouteMode::CrossMintLoyalHub);
    }

    let mut kamino_markets = withdraw.markets;
    kamino_markets.extend(deposit.markets);
    let kamino_markets = unique_pubkeys(kamino_markets);
    let init_markets = unique_pubkeys(init_markets);
    if !init_markets.is_empty() && !same_pubkey_set(&init_markets, &kamino_markets) {
        return None;
    }
    let refresh_markets = unique_pubkeys(refresh_markets);
    if !refresh_markets.is_empty() && !same_pubkey_set(&refresh_markets, &kamino_markets) {
        return None;
    }
    let mut kamino_liquidity_mints = withdraw.liquidity_mints;
    kamino_liquidity_mints.extend(deposit.liquidity_mints);
    let kamino_liquidity_mints = unique_pubkeys(kamino_liquidity_mints);
    let stable_mints = if stable_mints.is_empty() {
        kamino_liquidity_mints.clone()
    } else {
        unique_pubkeys(stable_mints)
    };
    let universe_preset = detect_yield_route_universe_preset(&kamino_markets);

    Some(DetectedYieldRoutePolicy {
        settings: action.settings,
        authority: action.authority,
        policy_seed: action.policy_seed,
        policy_account: action.policy_account,
        vault_index: action.payload.vault_index,
        delegated_signers: unique_pubkeys(delegated_signers_as_pubkeys(&action.delegated_signers)),
        threshold: action.threshold,
        route_modes,
        stable_mints,
        kamino_markets,
        kamino_liquidity_mints,
        universe_preset,
        swap_lanes,
    })
}

pub fn detect_balance_sweep_policy_create(
    action: &SquadsSettingsActionView,
) -> Option<DetectedBalanceSweepPolicy> {
    let [constraint] = action.payload.constraints.as_slice() else {
        return None;
    };
    let sweep = classify_subscription_sweep(constraint)?;
    let vault_pubkey = derive_squads_vault(&action.settings, action.payload.vault_index);
    if sweep.vault != vault_pubkey {
        return None;
    }

    Some(DetectedBalanceSweepPolicy {
        settings: action.settings,
        authority: action.authority,
        policy_seed: action.policy_seed,
        policy_account: action.policy_account,
        vault_index: action.payload.vault_index,
        vault_pubkey,
        wallet: sweep.wallet,
        wallet_usdc_ata: sweep.wallet_usdc_ata,
        vault_usdc_ata: sweep.vault_usdc_ata,
        delegated_signers: unique_pubkeys(delegated_signers_as_pubkeys(&action.delegated_signers)),
        threshold: action.threshold,
        max_amount_per_period: sweep.max_amount_per_period,
    })
}

fn delegated_signers_as_pubkeys(signers: &[Pubkey]) -> Vec<Pubkey> {
    signers.to_vec()
}

struct SubscriptionSweepLeg {
    wallet: Pubkey,
    vault: Pubkey,
    wallet_usdc_ata: Pubkey,
    vault_usdc_ata: Pubkey,
    max_amount_per_period: u64,
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
    if let Some(leg) = classify_full_kamino_leg(&accounts, vault) {
        return Some(leg);
    }
    classify_compact_same_mint_kamino_leg(&accounts, vault)
}

fn classify_full_kamino_leg(
    accounts: &BTreeMap<u8, &SquadsAccountConstraintView>,
    vault: Pubkey,
) -> Option<KaminoLeg> {
    has_token_authority(accounts.get(&9)?, vault)?;
    single_pubkey(accounts.get(&11)?, None).filter(|key| *key == spl_token::id())?;
    single_pubkey(accounts.get(&12)?, None).filter(|key| *key == spl_token::id())?;
    Some(KaminoLeg {
        vault,
        markets: pubkeys(accounts.get(&2)?, None)?,
        liquidity_mints: pubkeys(accounts.get(&5)?, Some(spl_token::id()))?,
    })
}

fn classify_compact_same_mint_kamino_leg(
    accounts: &BTreeMap<u8, &SquadsAccountConstraintView>,
    vault: Pubkey,
) -> Option<KaminoLeg> {
    let markets = unique_pubkeys(pubkeys(accounts.get(&2)?, None)?);
    let liquidity_mints = unique_pubkeys(pubkeys(accounts.get(&5)?, None)?);
    if !same_mint_usdc_markets_are_supported(&markets) || liquidity_mints != vec![USDC_MINT] {
        return None;
    }
    Some(KaminoLeg {
        vault,
        markets,
        liquidity_mints,
    })
}

fn same_mint_usdc_markets_are_supported(markets: &[Pubkey]) -> bool {
    if markets.is_empty() {
        return false;
    }
    for market in markets {
        match *market {
            KAMINO_MAIN_MARKET | KAMINO_FIGURE_MARKET | KAMINO_MAPLE_MARKET
            | KAMINO_ONRE_MARKET | KAMINO_ETHENA_MARKET => {}
            _ => return false,
        }
    }
    true
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
    classify_full_kamino_leg(&accounts, vault)
        .or_else(|| classify_compact_same_mint_kamino_leg(&accounts, vault))
}

fn classify_kamino_init_obligation(
    constraint: &SquadsInstructionConstraintView,
    vault: Pubkey,
) -> Option<Vec<Pubkey>> {
    if constraint.program_id != KAMINO_LEND_PROGRAM_ID
        || !has_data_bytes_equals(
            &constraint.data_constraints,
            0,
            &KAMINO_INIT_OBLIGATION_DISCRIMINATOR,
        )
        || !has_data_u8_or_slice_equals(
            &constraint.data_constraints,
            KAMINO_INIT_OBLIGATION_TAG_OFFSET,
            KAMINO_VANILLA_OBLIGATION_TAG,
        )
        || !has_data_u8_or_slice_equals(
            &constraint.data_constraints,
            KAMINO_INIT_OBLIGATION_ID_OFFSET,
            KAMINO_VANILLA_OBLIGATION_ID,
        )
    {
        return None;
    }
    let accounts = accounts_by_index(constraint);
    single_pubkey(accounts.get(&0)?, None).filter(|key| *key == vault)?;
    single_pubkey(accounts.get(&1)?, None).filter(|key| *key == vault)?;
    let markets = unique_pubkeys(pubkeys(accounts.get(&3)?, None)?);
    if let Some(obligation_constraint) = accounts.get(&2) {
        let obligations = unique_pubkeys(pubkeys(obligation_constraint, None)?);
        if obligations.len() != markets.len() {
            return None;
        }
        let expected_obligations = markets
            .iter()
            .map(|market| derive_kamino_vanilla_obligation(vault, *market))
            .collect::<Vec<_>>();
        if !same_pubkey_set(&obligations, &expected_obligations) {
            return None;
        }
    }
    single_pubkey(accounts.get(&4)?, None).filter(|key| *key == Pubkey::default())?;
    single_pubkey(accounts.get(&5)?, None).filter(|key| *key == Pubkey::default())?;
    single_pubkey(accounts.get(&6)?, None)
        .filter(|key| *key == derive_kamino_user_metadata(vault))?;
    single_pubkey(accounts.get(&7)?, None).filter(|key| *key == solana_sdk::sysvar::rent::id())?;
    single_pubkey(accounts.get(&8)?, None).filter(|key| *key == solana_sdk::system_program::ID)?;
    Some(markets)
}

fn classify_kamino_refresh_obligation(
    constraint: &SquadsInstructionConstraintView,
    vault: Pubkey,
) -> Option<Vec<Pubkey>> {
    if constraint.program_id != KAMINO_LEND_PROGRAM_ID
        || !has_data_slice_equals(
            &constraint.data_constraints,
            0,
            &KAMINO_REFRESH_OBLIGATION_DISCRIMINATOR,
        )
    {
        return None;
    }
    let accounts = accounts_by_index(constraint);
    let markets = unique_pubkeys(pubkeys(accounts.get(&0)?, None)?);
    let obligations = unique_pubkeys(pubkeys(accounts.get(&1)?, None)?);
    if obligations.len() != markets.len() {
        return None;
    }
    let expected_obligations = markets
        .iter()
        .map(|market| derive_kamino_vanilla_obligation(vault, *market))
        .collect::<Vec<_>>();
    if !same_pubkey_set(&obligations, &expected_obligations) {
        return None;
    }
    Some(markets)
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
    has_hub_token_authority(
        accounts.get(&loyal_hub_abi::swap_exact_in_accounts::USER_INPUT)?,
        vault,
    )?;
    has_hub_token_authority(
        accounts.get(&loyal_hub_abi::swap_exact_in_accounts::USER_OUTPUT)?,
        vault,
    )?;
    let mut stable_mints =
        hub_token_pubkeys(accounts.get(&loyal_hub_abi::swap_exact_in_accounts::INPUT_MINT)?)?;
    stable_mints.extend(hub_token_pubkeys(
        accounts.get(&loyal_hub_abi::swap_exact_in_accounts::OUTPUT_MINT)?,
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

fn classify_subscription_sweep(
    constraint: &SquadsInstructionConstraintView,
) -> Option<SubscriptionSweepLeg> {
    if constraint.program_id != SUBSCRIPTIONS_PROGRAM_ID
        || !has_data_u8_equals(
            &constraint.data_constraints,
            0,
            SUBSCRIPTIONS_TRANSFER_RECURRING,
        )
        || !has_data_slice_equals(
            &constraint.data_constraints,
            SUBSCRIPTION_TRANSFER_MINT_OFFSET,
            USDC_MINT.as_ref(),
        )
    {
        return None;
    }

    let accounts = accounts_by_index(constraint);
    let recurring = accounts.get(&0)?;
    let wallet = account_data_pubkey_equals(
        recurring,
        Some(SUBSCRIPTIONS_PROGRAM_ID),
        SUBSCRIPTION_RECURRING_DELEGATION_DELEGATOR_OFFSET,
    )?;
    has_data_slice_equals(
        &constraint.data_constraints,
        SUBSCRIPTION_TRANSFER_DELEGATOR_OFFSET,
        wallet.as_ref(),
    )
    .then_some(())?;
    has_account_data_u8_equals(
        recurring,
        SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET,
        SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR,
    )?;
    let vault = account_data_pubkey_equals(
        recurring,
        Some(SUBSCRIPTIONS_PROGRAM_ID),
        SUBSCRIPTION_RECURRING_DELEGATION_DELEGATEE_OFFSET,
    )?;
    let subscription_authority = account_data_pubkey_equals(
        recurring,
        Some(SUBSCRIPTIONS_PROGRAM_ID),
        SUBSCRIPTION_RECURRING_DELEGATION_AUTHORITY_OFFSET,
    )?;
    if subscription_authority != derive_subscription_authority(wallet, USDC_MINT) {
        return None;
    }
    account_data_pubkey_equals(
        recurring,
        Some(SUBSCRIPTIONS_PROGRAM_ID),
        SUBSCRIPTION_RECURRING_DELEGATION_MINT_OFFSET,
    )
    .filter(|mint| *mint == USDC_MINT)?;
    let max_amount_per_period = account_data_u64_lte(
        recurring,
        SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PER_PERIOD_OFFSET,
    )?;

    single_pubkey(accounts.get(&1)?, Some(SUBSCRIPTIONS_PROGRAM_ID))
        .filter(|key| *key == subscription_authority)?;
    let wallet_usdc_ata = single_pubkey(accounts.get(&2)?, Some(spl_token::id()))?;
    let vault_usdc_ata = single_pubkey(accounts.get(&3)?, Some(spl_token::id()))?;
    single_pubkey(accounts.get(&4)?, Some(spl_token::id())).filter(|key| *key == USDC_MINT)?;
    single_pubkey(accounts.get(&5)?, None).filter(|key| *key == spl_token::id())?;
    single_pubkey(accounts.get(&6)?, None).filter(|key| *key == vault)?;
    single_pubkey(accounts.get(&7)?, None)
        .filter(|key| *key == derive_subscription_event_authority())?;
    single_pubkey(accounts.get(&8)?, None).filter(|key| *key == SUBSCRIPTIONS_PROGRAM_ID)?;

    Some(SubscriptionSweepLeg {
        wallet,
        vault,
        wallet_usdc_ata,
        vault_usdc_ata,
        max_amount_per_period,
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

fn account_data_pubkey_equals(
    constraint: &SquadsAccountConstraintView,
    owner: Option<Pubkey>,
    offset: u64,
) -> Option<Pubkey> {
    if constraint.owner != owner {
        return None;
    }
    let SquadsAccountConstraintKindView::AccountData(data_constraints) = &constraint.kind else {
        return None;
    };
    data_constraints.iter().find_map(|constraint| {
        if constraint.data_offset == offset && constraint.operator == SquadsDataOperatorView::Equals
        {
            if let SquadsDataValueView::U8Slice(value) = &constraint.data_value {
                let bytes: [u8; 32] = value.as_slice().try_into().ok()?;
                return Some(Pubkey::new_from_array(bytes));
            }
        }
        None
    })
}

fn has_account_data_u8_equals(
    constraint: &SquadsAccountConstraintView,
    offset: u64,
    expected: u8,
) -> Option<()> {
    constraint.owner?;
    let SquadsAccountConstraintKindView::AccountData(data_constraints) = &constraint.kind else {
        return None;
    };
    has_data_u8_equals(data_constraints, offset, expected).then_some(())
}

fn account_data_u64_lte(constraint: &SquadsAccountConstraintView, offset: u64) -> Option<u64> {
    let SquadsAccountConstraintKindView::AccountData(data_constraints) = &constraint.kind else {
        return None;
    };
    data_constraints.iter().find_map(|constraint| {
        if constraint.data_offset == offset
            && constraint.operator == SquadsDataOperatorView::LessThanOrEqualTo
        {
            if let SquadsDataValueView::U64Le(value) = constraint.data_value {
                return Some(value);
            }
        }
        None
    })
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

fn hub_token_pubkeys(constraint: &SquadsAccountConstraintView) -> Option<Vec<Pubkey>> {
    if !is_supported_hub_token_constraint_owner(constraint.owner) {
        return None;
    }
    match &constraint.kind {
        SquadsAccountConstraintKindView::Pubkey(pubkeys) => Some(pubkeys.clone()),
        SquadsAccountConstraintKindView::AccountData(_) => None,
    }
}

fn has_hub_token_authority(
    constraint: &SquadsAccountConstraintView,
    authority: Pubkey,
) -> Option<()> {
    if !is_supported_hub_token_constraint_owner(constraint.owner) {
        return None;
    }
    let SquadsAccountConstraintKindView::AccountData(data_constraints) = &constraint.kind else {
        return None;
    };
    has_data_slice_equals(data_constraints, 32, authority.as_ref()).then_some(())
}

fn is_supported_hub_token_constraint_owner(owner: Option<Pubkey>) -> bool {
    owner.is_none() || owner == Some(spl_token::id()) || owner == Some(TOKEN_2022_PROGRAM_ID)
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

fn has_data_bytes_equals(
    constraints: &[SquadsDataConstraintView],
    offset: u64,
    expected: &[u8],
) -> bool {
    constraints.iter().any(|constraint| {
        if constraint.operator != SquadsDataOperatorView::Equals {
            return false;
        }
        let SquadsDataValueView::U8Slice(bytes) = &constraint.data_value else {
            return false;
        };
        let Some(relative_offset) = offset.checked_sub(constraint.data_offset) else {
            return false;
        };
        let relative_offset = relative_offset as usize;
        bytes
            .get(relative_offset..relative_offset + expected.len())
            .is_some_and(|actual| actual == expected)
    })
}

fn has_data_u8_equals(constraints: &[SquadsDataConstraintView], offset: u64, expected: u8) -> bool {
    constraints.iter().any(|constraint| {
        constraint.data_offset == offset
            && constraint.operator == SquadsDataOperatorView::Equals
            && constraint.data_value == SquadsDataValueView::U8(expected)
    })
}

fn has_data_u8_or_slice_equals(
    constraints: &[SquadsDataConstraintView],
    offset: u64,
    expected: u8,
) -> bool {
    has_data_u8_equals(constraints, offset, expected)
        || has_data_bytes_equals(constraints, offset, &[expected])
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

fn unique_pubkeys(pubkeys: Vec<Pubkey>) -> Vec<Pubkey> {
    let mut unique = Vec::new();
    for pubkey in pubkeys {
        if !unique.contains(&pubkey) {
            unique.push(pubkey);
        }
    }
    unique
}

fn same_pubkey_set(left: &[Pubkey], right: &[Pubkey]) -> bool {
    left.len() == right.len() && left.iter().all(|pubkey| right.contains(pubkey))
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
        create_preset_all_in_one_yield_route_action, create_three_step_yield_route_actions,
        remove_policy_instruction, yield_route_universe_for_preset, KaminoStableRiskProfile,
        LoyalActionContext, RouteTopology, SwapLane, YieldRouteActionBuilder,
        YieldRouteActionSeeds, YieldRouteUniverse, YieldRouteUniversePreset, JUPITER_V6_PROGRAM_ID,
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
            daily_source_mint_spending_cap: 1_000_000_000,
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

    fn detected_action_with_init_constraint() -> SquadsSettingsActionView {
        let context = context();
        let universe = universe();
        let setup = create_all_in_one_market_mint_yield_route_action(
            context,
            universe.clone(),
            vec![jupiter_lane()],
        )
        .unwrap();
        let mut action = decode_squads_policy_create_actions(&setup.instructions[0])
            .unwrap()
            .remove(0);
        let vault = classify_kamino_withdraw(&action.payload.constraints[0])
            .expect("fixture has withdraw")
            .vault;
        let three_step = create_three_step_yield_route_actions(
            context,
            universe,
            vec![jupiter_lane()],
            YieldRouteActionSeeds::default(),
        )
        .unwrap();
        let mut init_constraint = None;
        for instruction in &three_step.instructions {
            for decoded_action in decode_squads_policy_create_actions(instruction).unwrap() {
                init_constraint =
                    decoded_action
                        .payload
                        .constraints
                        .into_iter()
                        .find(|constraint| {
                            classify_kamino_init_obligation(constraint, vault).is_some()
                        });
                if init_constraint.is_some() {
                    break;
                }
            }
            if init_constraint.is_some() {
                break;
            }
        }
        action
            .payload
            .constraints
            .push(init_constraint.expect("three-step fixture has init obligation constraint"));
        action
    }

    fn balance_sweep_action() -> SquadsSettingsActionView {
        let settings = Pubkey::new_unique();
        let authority = Pubkey::new_unique();
        let delegated_signer = Pubkey::new_unique();
        let vault_index = 1;
        let vault = derive_squads_vault(&settings, vault_index);
        let wallet = Pubkey::new_unique();
        let wallet_usdc_ata = Pubkey::new_unique();
        let vault_usdc_ata = Pubkey::new_unique();
        let subscription_authority = derive_subscription_authority(wallet, USDC_MINT);
        let event_authority = derive_subscription_event_authority();

        SquadsSettingsActionView {
            settings,
            authority,
            policy_seed: 7,
            policy_account: Pubkey::new_unique(),
            delegated_signers: vec![delegated_signer],
            threshold: 1,
            payload: SquadsProgramInteractionPolicyView {
                vault_index,
                pubkey_table: vec![],
                constraints: vec![SquadsInstructionConstraintView {
                    program_id: SUBSCRIPTIONS_PROGRAM_ID,
                    account_constraints: vec![
                        account_data_view(
                            0,
                            Some(SUBSCRIPTIONS_PROGRAM_ID),
                            vec![
                                data_u8_eq(
                                    SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET,
                                    SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR,
                                ),
                                data_pubkey_eq(
                                    SUBSCRIPTION_RECURRING_DELEGATION_DELEGATOR_OFFSET,
                                    wallet,
                                ),
                                data_pubkey_eq(
                                    SUBSCRIPTION_RECURRING_DELEGATION_DELEGATEE_OFFSET,
                                    vault,
                                ),
                                data_pubkey_eq(
                                    SUBSCRIPTION_RECURRING_DELEGATION_AUTHORITY_OFFSET,
                                    subscription_authority,
                                ),
                                data_pubkey_eq(
                                    SUBSCRIPTION_RECURRING_DELEGATION_MINT_OFFSET,
                                    USDC_MINT,
                                ),
                                data_u64_lte(
                                    SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PER_PERIOD_OFFSET,
                                    500_000,
                                ),
                            ],
                        ),
                        pubkey_view(1, subscription_authority, Some(SUBSCRIPTIONS_PROGRAM_ID)),
                        pubkey_view(2, wallet_usdc_ata, Some(spl_token::id())),
                        pubkey_view(3, vault_usdc_ata, Some(spl_token::id())),
                        pubkey_view(4, USDC_MINT, Some(spl_token::id())),
                        pubkey_view(5, spl_token::id(), None),
                        pubkey_view(6, vault, None),
                        pubkey_view(7, event_authority, None),
                        pubkey_view(8, SUBSCRIPTIONS_PROGRAM_ID, None),
                    ],
                    data_constraints: vec![
                        data_u8_eq(0, SUBSCRIPTIONS_TRANSFER_RECURRING),
                        data_pubkey_eq(SUBSCRIPTION_TRANSFER_DELEGATOR_OFFSET, wallet),
                        data_pubkey_eq(SUBSCRIPTION_TRANSFER_MINT_OFFSET, USDC_MINT),
                    ],
                }],
                spending_limits: vec![],
            },
        }
    }

    fn pubkey_view(
        account_index: u8,
        pubkey: Pubkey,
        owner: Option<Pubkey>,
    ) -> SquadsAccountConstraintView {
        SquadsAccountConstraintView {
            account_index,
            kind: SquadsAccountConstraintKindView::Pubkey(vec![pubkey]),
            owner,
        }
    }

    fn account_data_view(
        account_index: u8,
        owner: Option<Pubkey>,
        data_constraints: Vec<SquadsDataConstraintView>,
    ) -> SquadsAccountConstraintView {
        SquadsAccountConstraintView {
            account_index,
            kind: SquadsAccountConstraintKindView::AccountData(data_constraints),
            owner,
        }
    }

    fn data_u8_eq(offset: u64, value: u8) -> SquadsDataConstraintView {
        SquadsDataConstraintView {
            data_offset: offset,
            data_value: SquadsDataValueView::U8(value),
            operator: SquadsDataOperatorView::Equals,
        }
    }

    fn data_pubkey_eq(offset: u64, pubkey: Pubkey) -> SquadsDataConstraintView {
        SquadsDataConstraintView {
            data_offset: offset,
            data_value: SquadsDataValueView::U8Slice(pubkey.to_bytes().to_vec()),
            operator: SquadsDataOperatorView::Equals,
        }
    }

    fn data_u64_lte(offset: u64, value: u64) -> SquadsDataConstraintView {
        SquadsDataConstraintView {
            data_offset: offset,
            data_value: SquadsDataValueView::U64Le(value),
            operator: SquadsDataOperatorView::LessThanOrEqualTo,
        }
    }

    fn init_constraint_mut(
        action: &mut SquadsSettingsActionView,
    ) -> &mut SquadsInstructionConstraintView {
        let vault = classify_kamino_withdraw(&action.payload.constraints[0])
            .expect("fixture has withdraw")
            .vault;
        action
            .payload
            .constraints
            .iter_mut()
            .find(|constraint| classify_kamino_init_obligation(constraint, vault).is_some())
            .expect("fixture has init obligation constraint")
    }

    fn init_obligation_constraint_view(
        init: &SquadsInstructionConstraintView,
        vault: Pubkey,
        extra_obligations: Vec<Pubkey>,
    ) -> SquadsAccountConstraintView {
        let markets = init
            .account_constraints
            .iter()
            .find(|constraint| constraint.account_index == 3)
            .expect("init lending market account constraint");
        let SquadsAccountConstraintKindView::Pubkey(markets) = &markets.kind else {
            panic!("init lending market should use pubkey constraints");
        };
        let mut obligations = markets
            .iter()
            .map(|market| derive_kamino_vanilla_obligation(vault, *market))
            .collect::<Vec<_>>();
        obligations.extend(extra_obligations);
        SquadsAccountConstraintView {
            account_index: 2,
            kind: SquadsAccountConstraintKindView::Pubkey(obligations),
            owner: None,
        }
    }

    fn refresh_constraint_mut(
        action: &mut SquadsSettingsActionView,
    ) -> &mut SquadsInstructionConstraintView {
        let vault = classify_kamino_withdraw(&action.payload.constraints[0])
            .expect("fixture has withdraw")
            .vault;
        action
            .payload
            .constraints
            .iter_mut()
            .find(|constraint| classify_kamino_refresh_obligation(constraint, vault).is_some())
            .expect("fixture has refresh obligation constraint")
    }

    #[test]
    fn detects_generalized_cross_mint_policy_and_rejects_shape_mutations() {
        let context = policy_context(3);
        let spec = generalized_cross_mint_spec(JupiterCrossMintSourceShard::Classic);
        let setup = create_jupiter_cross_mint_policy_action(context, spec, 101).unwrap();
        let detected = detect_jupiter_cross_mint_policy_action(&setup.instruction)
            .unwrap()
            .expect("detect generalized cross-mint policy create");

        assert_eq!(detected.settings, context.settings);
        assert_eq!(detected.authority, context.authority);
        assert_eq!(detected.identity.policy_seed(), Some(101));
        assert_eq!(detected.identity.policy_account(), setup.account);
        assert_eq!(detected.account_index, context.account_index);
        assert_eq!(detected.vault, context.vault);
        assert_eq!(detected.delegated_signer, context.delegated_signer);
        assert_eq!(detected.source_shard, spec.source_shard);
        assert_eq!(detected.max_slippage_bps, spec.max_slippage_bps);
        assert_eq!(
            detected.daily_source_mint_spending_cap,
            spec.daily_source_mint_spending_cap
        );
        assert_eq!(
            detected.dialect_constraint_indexes,
            BTreeMap::from([
                (JupiterV2Dialect::RouteV2, 0),
                (JupiterV2Dialect::SharedAccountsRouteV2, 1),
            ])
        );

        let payload =
            decoded_generalized_cross_mint_payload(context, JupiterCrossMintSourceShard::Classic);
        assert!(classify_jupiter_cross_mint_policy(
            &payload,
            SpendingLimitStartContract::CreationRequest
        )
        .is_some());

        let mut missing_output_ata = payload.clone();
        let SquadsAccountConstraintKindView::Pubkey(pubkeys) =
            &mut missing_output_ata.constraints[0].account_constraints[1].kind
        else {
            panic!("output account must be a pubkey allowlist");
        };
        pubkeys.pop();
        assert!(classify_jupiter_cross_mint_policy(
            &missing_output_ata,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut wrong_output_ata = payload.clone();
        let SquadsAccountConstraintKindView::Pubkey(pubkeys) =
            &mut wrong_output_ata.constraints[1].account_constraints[1].kind
        else {
            panic!("output account must be a pubkey allowlist");
        };
        pubkeys[0] = Pubkey::new_unique();
        assert!(classify_jupiter_cross_mint_policy(
            &wrong_output_ata,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut unexpected_input_constraint = payload.clone();
        unexpected_input_constraint.constraints[0]
            .account_constraints
            .push(pubkey_view(1, Pubkey::new_unique(), None));
        assert!(classify_jupiter_cross_mint_policy(
            &unexpected_input_constraint,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut wrong_source_mint = payload.clone();
        wrong_source_mint.spending_limits[0].mint = CASH_MINT;
        assert!(classify_jupiter_cross_mint_policy(
            &wrong_source_mint,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut duplicate_source_mint = payload.clone();
        duplicate_source_mint.spending_limits[1].mint =
            duplicate_source_mint.spending_limits[0].mint;
        assert!(classify_jupiter_cross_mint_policy(
            &duplicate_source_mint,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut mismatched_cap = payload.clone();
        mismatched_cap.spending_limits[1].max_per_period -= 1;
        assert!(classify_jupiter_cross_mint_policy(
            &mismatched_cap,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut reversed_dialects = payload.clone();
        reversed_dialects.constraints.swap(0, 1);
        assert!(classify_jupiter_cross_mint_policy(
            &reversed_dialects,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut missing_dialect = payload.clone();
        missing_dialect.constraints.pop();
        assert!(classify_jupiter_cross_mint_policy(
            &missing_dialect,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut mismatched_fee = payload.clone();
        mismatched_fee.constraints[0].data_constraints[2].data_value = SquadsDataValueView::U8(1);
        assert!(classify_jupiter_cross_mint_policy(
            &mismatched_fee,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut mismatched_slippage = payload.clone();
        mismatched_slippage.constraints[1].data_constraints[1].data_value =
            SquadsDataValueView::U16Le(spec.max_slippage_bps - 1);
        assert!(classify_jupiter_cross_mint_policy(
            &mismatched_slippage,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut zero_slippage = payload.clone();
        zero_slippage.constraints[0].data_constraints[1].data_value = SquadsDataValueView::U16Le(0);
        assert!(classify_jupiter_cross_mint_policy(
            &zero_slippage,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut zero_slippage = payload.clone();
        zero_slippage.constraints[0].data_constraints[1].data_value = SquadsDataValueView::U16Le(0);
        assert!(classify_jupiter_cross_mint_policy(
            &zero_slippage,
            SpendingLimitStartContract::CreationRequest
        )
        .is_none());

        let mut loose_metadata = setup.instruction.clone();
        let last = loose_metadata.data.last_mut().expect("memo option");
        assert_eq!(*last, 0);
        *last = 1;
        loose_metadata.data.extend_from_slice(&0_u32.to_le_bytes());
        assert!(detect_jupiter_cross_mint_policy_action(&loose_metadata)
            .unwrap()
            .is_none());

        let mut hook_payload = vec![3, payload.vault_index];
        push_raw_instruction_constraints(&mut hook_payload, &payload.constraints);
        hook_payload.push(1); // pre-hook present
        hook_payload.push(0); // hook kind
        hook_payload.extend_from_slice(&0_u32.to_le_bytes()); // accounts
        hook_payload.extend_from_slice(&0_u32.to_le_bytes()); // data
        push_pubkey(&mut hook_payload, Pubkey::new_unique());
        hook_payload.push(0); // allow-pre-hook failure flag
        hook_payload.push(0); // no post-hook
        hook_payload.extend_from_slice(&0_u32.to_le_bytes()); // spending limits
        assert!(
            read_program_interaction_payload(&mut Cursor::new(&hook_payload))
                .unwrap()
                .is_none()
        );
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
    fn detects_policy_remove_without_claiming_a_policy_family() {
        let context = policy_context(0);
        let policy_account = Pubkey::new_unique();
        let remove = remove_policy_instruction(context.settings, context.authority, policy_account);

        let detected = detect_squads_policy_remove(&remove)
            .unwrap()
            .expect("detect canonical policy removal");
        assert_eq!(detected.settings, context.settings);
        assert_eq!(detected.authority, context.authority);
        assert_eq!(detected.policy_account, policy_account);
        let mut mismatched_account = remove;
        mismatched_account.accounts[5].pubkey = Pubkey::new_unique();
        assert!(detect_squads_policy_remove(&mismatched_account)
            .unwrap()
            .is_none());
    }

    #[test]
    fn detects_balance_sweep_policy() {
        let action = balance_sweep_action();
        let detected = detect_balance_sweep_policy_create(&action).unwrap();

        assert_eq!(detected.settings, action.settings);
        assert_eq!(detected.authority, action.authority);
        assert_eq!(detected.policy_seed, action.policy_seed);
        assert_eq!(detected.policy_account, action.policy_account);
        assert_eq!(detected.vault_index, 1);
        assert_eq!(detected.delegated_signers, action.delegated_signers);
        assert_eq!(detected.threshold, 1);
        assert_eq!(detected.max_amount_per_period, 500_000);
        assert_eq!(
            detected.vault_pubkey,
            derive_squads_vault(&action.settings, 1)
        );
    }

    #[test]
    fn rejects_balance_sweep_when_vault_or_mint_do_not_match() {
        let mut wrong_vault = balance_sweep_action();
        let recurring = wrong_vault.payload.constraints[0]
            .account_constraints
            .iter_mut()
            .find(|constraint| constraint.account_index == 0)
            .unwrap();
        let SquadsAccountConstraintKindView::AccountData(data_constraints) = &mut recurring.kind
        else {
            panic!("recurring delegation should use data constraints");
        };
        data_constraints
            .iter_mut()
            .find(|constraint| {
                constraint.data_offset == SUBSCRIPTION_RECURRING_DELEGATION_DELEGATEE_OFFSET
            })
            .unwrap()
            .data_value = SquadsDataValueView::U8Slice(Pubkey::new_unique().to_bytes().to_vec());
        assert!(detect_balance_sweep_policy_create(&wrong_vault).is_none());

        let mut wrong_mint = balance_sweep_action();
        wrong_mint.payload.constraints[0]
            .data_constraints
            .iter_mut()
            .find(|constraint| constraint.data_offset == SUBSCRIPTION_TRANSFER_MINT_OFFSET)
            .unwrap()
            .data_value = SquadsDataValueView::U8Slice(Pubkey::new_unique().to_bytes().to_vec());
        assert!(detect_balance_sweep_policy_create(&wrong_mint).is_none());
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
        assert_eq!(detected.universe_preset, None);
    }

    #[test]
    fn detects_compact_same_mint_usdc_market_mint_policy() {
        let context = context();
        let setup = create_all_in_one_market_mint_yield_route_action(
            context,
            YieldRouteUniverse::new(
                vec![USDC_MINT],
                vec![
                    KAMINO_MAIN_MARKET,
                    KAMINO_FIGURE_MARKET,
                    KAMINO_MAPLE_MARKET,
                    KAMINO_ONRE_MARKET,
                    KAMINO_ETHENA_MARKET,
                ],
                vec![USDC_MINT],
            ),
            vec![],
        )
        .unwrap();

        let actions = decode_squads_policy_create_actions(&setup.instructions[0]).unwrap();
        let detected = detect_yield_route_policy_create(&actions[0]).unwrap();

        assert_eq!(detected.route_modes, vec![DetectedYieldRouteMode::SameMint]);
        assert_eq!(
            detected.kamino_markets,
            vec![
                KAMINO_MAIN_MARKET,
                KAMINO_FIGURE_MARKET,
                KAMINO_MAPLE_MARKET,
                KAMINO_ONRE_MARKET,
                KAMINO_ETHENA_MARKET,
            ]
        );
        assert_eq!(detected.kamino_liquidity_mints, vec![USDC_MINT]);
        assert_eq!(detected.stable_mints, vec![USDC_MINT]);
        assert_eq!(
            detected.universe_preset,
            Some(YieldRouteUniversePreset::KaminoStable(
                KaminoStableRiskProfile::Safe
            ))
        );
    }

    #[test]
    fn detects_preset_all_in_one_policy() {
        let setup = create_preset_all_in_one_yield_route_action(
            context(),
            YieldRouteUniversePreset::KaminoStable(KaminoStableRiskProfile::Safe),
            vec![jupiter_lane()],
        )
        .unwrap();

        let actions = decode_squads_policy_create_actions(&setup.instructions[0]).unwrap();
        let detected = detect_yield_route_policy_create(&actions[0]).unwrap();
        let preset = YieldRouteUniversePreset::KaminoStable(KaminoStableRiskProfile::Safe);
        let universe = yield_route_universe_for_preset(preset);

        assert_eq!(detected.universe_preset, Some(preset));
        assert_eq!(detected.kamino_markets, universe.kamino_markets);
        assert_eq!(
            detected.kamino_liquidity_mints,
            universe.kamino_liquidity_mints
        );
        assert_eq!(detected.stable_mints, universe.stable_mints);
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
        let withdraw = classify_kamino_withdraw(&missing_deposit.payload.constraints[0])
            .expect("fixture has withdraw");
        missing_deposit
            .payload
            .constraints
            .retain(|constraint| classify_kamino_deposit(constraint, withdraw.vault).is_none());
        assert!(detect_yield_route_policy_create(&missing_deposit).is_none());
    }

    #[test]
    fn rejects_wrong_token_owner_constraints() {
        let mut action = detected_action();
        let withdraw = action.payload.constraints.first_mut().unwrap();
        let source_liquidity = withdraw
            .account_constraints
            .iter_mut()
            .find(|constraint| constraint.account_index == 5)
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
    fn rejects_loose_init_obligation_policy_constraints() {
        let mut unsupported_market = detected_action_with_init_constraint();
        let market = Pubkey::new_unique();
        let init = init_constraint_mut(&mut unsupported_market);
        let markets = init
            .account_constraints
            .iter_mut()
            .find(|constraint| constraint.account_index == 3)
            .unwrap();
        let SquadsAccountConstraintKindView::Pubkey(pubkeys) = &mut markets.kind else {
            panic!("init lending market should use pubkey constraints");
        };
        pubkeys.push(market);
        assert!(detect_yield_route_policy_create(&unsupported_market).is_none());

        let mut wrong_owner = detected_action_with_init_constraint();
        let init = init_constraint_mut(&mut wrong_owner);
        let owner = init
            .account_constraints
            .iter_mut()
            .find(|constraint| constraint.account_index == 0)
            .unwrap();
        owner.kind = SquadsAccountConstraintKindView::Pubkey(vec![Pubkey::new_unique()]);
        assert!(detect_yield_route_policy_create(&wrong_owner).is_none());

        let mut wrong_fee_payer = detected_action_with_init_constraint();
        let init = init_constraint_mut(&mut wrong_fee_payer);
        let fee_payer = init
            .account_constraints
            .iter_mut()
            .find(|constraint| constraint.account_index == 1)
            .unwrap();
        fee_payer.kind = SquadsAccountConstraintKindView::Pubkey(vec![Pubkey::new_unique()]);
        assert!(detect_yield_route_policy_create(&wrong_fee_payer).is_none());

        let mut extra_obligation = detected_action_with_init_constraint();
        let extra_vault = classify_kamino_withdraw(&extra_obligation.payload.constraints[0])
            .expect("fixture has withdraw")
            .vault;
        let init = init_constraint_mut(&mut extra_obligation);
        let obligation_constraint =
            init_obligation_constraint_view(init, extra_vault, vec![Pubkey::new_unique()]);
        init.account_constraints.push(obligation_constraint);
        assert!(detect_yield_route_policy_create(&extra_obligation).is_none());

        let mut wrong_obligation = detected_action_with_init_constraint();
        let init = init_constraint_mut(&mut wrong_obligation);
        init.account_constraints.push(SquadsAccountConstraintView {
            account_index: 2,
            kind: SquadsAccountConstraintKindView::Pubkey(vec![Pubkey::new_unique()]),
            owner: None,
        });
        assert!(detect_yield_route_policy_create(&wrong_obligation).is_none());

        let mut wrong_seed_account = detected_action_with_init_constraint();
        let init = init_constraint_mut(&mut wrong_seed_account);
        let seed = init
            .account_constraints
            .iter_mut()
            .find(|constraint| constraint.account_index == 4)
            .unwrap();
        seed.kind = SquadsAccountConstraintKindView::Pubkey(vec![Pubkey::new_unique()]);
        assert!(detect_yield_route_policy_create(&wrong_seed_account).is_none());

        let mut wrong_user_metadata = detected_action_with_init_constraint();
        let init = init_constraint_mut(&mut wrong_user_metadata);
        let user_metadata = init
            .account_constraints
            .iter_mut()
            .find(|constraint| constraint.account_index == 6)
            .unwrap();
        user_metadata.kind = SquadsAccountConstraintKindView::Pubkey(vec![Pubkey::new_unique()]);
        assert!(detect_yield_route_policy_create(&wrong_user_metadata).is_none());
    }

    #[test]
    fn rejects_loose_refresh_obligation_policy_constraints() {
        let mut route_without_refresh = detected_action();
        let vault = classify_kamino_withdraw(&route_without_refresh.payload.constraints[0])
            .expect("fixture has withdraw")
            .vault;
        route_without_refresh
            .payload
            .constraints
            .retain(|constraint| classify_kamino_refresh_obligation(constraint, vault).is_none());
        assert!(detect_yield_route_policy_create(&route_without_refresh).is_some());

        let mut unsupported_market = detected_action();
        let market = Pubkey::new_unique();
        let obligation = derive_kamino_vanilla_obligation(vault, market);
        let refresh = refresh_constraint_mut(&mut unsupported_market);
        let markets = refresh
            .account_constraints
            .iter_mut()
            .find(|constraint| constraint.account_index == 0)
            .unwrap();
        let SquadsAccountConstraintKindView::Pubkey(pubkeys) = &mut markets.kind else {
            panic!("refresh lending market should use pubkey constraints");
        };
        pubkeys.push(market);
        let obligations = refresh
            .account_constraints
            .iter_mut()
            .find(|constraint| constraint.account_index == 1)
            .unwrap();
        let SquadsAccountConstraintKindView::Pubkey(pubkeys) = &mut obligations.kind else {
            panic!("refresh obligation account should use pubkey constraints");
        };
        pubkeys.push(obligation);
        assert!(detect_yield_route_policy_create(&unsupported_market).is_none());

        let mut wrong_obligation = detected_action();
        let refresh = refresh_constraint_mut(&mut wrong_obligation);
        let obligations = refresh
            .account_constraints
            .iter_mut()
            .find(|constraint| constraint.account_index == 1)
            .unwrap();
        let SquadsAccountConstraintKindView::Pubkey(pubkeys) = &mut obligations.kind else {
            panic!("refresh obligation account should use pubkey constraints");
        };
        pubkeys[0] = Pubkey::new_unique();
        assert!(detect_yield_route_policy_create(&wrong_obligation).is_none());
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
