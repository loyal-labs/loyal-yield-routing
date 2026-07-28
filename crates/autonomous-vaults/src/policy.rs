use crate::squads::SmartAccountSigner;
use anyhow::{bail, Context, Result};
use borsh::BorshDeserialize;
use loyal_actions::{
    derive_action_account, SquadsAccountConstraintKindView, SquadsAccountConstraintView,
    SquadsDataConstraintView, SquadsDataOperatorView, SquadsDataValueView,
    SquadsInstructionConstraintView, SQUADS_SMART_ACCOUNT_PROGRAM_ID,
};
use solana_sdk::{hash::hashv, pubkey::Pubkey};

const FULL_PERMISSIONS_MASK: u8 = 7;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ProgramInteractionPolicyAccount {
    pub settings: Pubkey,
    pub seed: u64,
    pub bump: u8,
    pub transaction_index: u64,
    pub stale_transaction_index: u64,
    pub signers: Vec<SmartAccountSigner>,
    pub threshold: u16,
    pub time_lock: u32,
    pub account_index: u8,
    pub constraints: Vec<SquadsInstructionConstraintView>,
    pub start: i64,
    pub expiration: Option<PolicyExpirationWire>,
    pub rent_collector: Pubkey,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SpendingLimitPolicyAccount {
    pub settings: Pubkey,
    pub seed: u64,
    pub bump: u8,
    pub transaction_index: u64,
    pub stale_transaction_index: u64,
    pub signers: Vec<SmartAccountSigner>,
    pub threshold: u16,
    pub time_lock: u32,
    pub source_account_index: u8,
    pub destinations: Vec<Pubkey>,
    pub mint: Pubkey,
    pub spending_start: i64,
    pub spending_expiration: Option<i64>,
    pub period: SpendingLimitPeriod,
    pub accumulate_unused: bool,
    pub max_per_period: u64,
    pub max_per_use: u64,
    pub enforce_exact_quantity: bool,
    pub remaining_in_period: u64,
    pub last_reset: i64,
    pub start: i64,
    pub expiration: Option<PolicyExpirationWire>,
    pub rent_collector: Pubkey,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SpendingLimitPeriod {
    OneTime,
    Daily,
    Weekly,
    Monthly,
    Custom(i64),
}

#[derive(BorshDeserialize)]
struct PolicyWire {
    discriminator: [u8; 8],
    settings: Pubkey,
    seed: u64,
    bump: u8,
    transaction_index: u64,
    stale_transaction_index: u64,
    signers: Vec<SmartAccountSigner>,
    threshold: u16,
    time_lock: u32,
    policy_state: PolicyStateWire,
    start: i64,
    expiration: Option<PolicyExpirationWire>,
    rent_collector: Pubkey,
}

#[derive(BorshDeserialize)]
enum PolicyStateWire {
    InternalFundTransfer(()),
    SpendingLimit(SpendingLimitPolicyWire),
    SettingsChange(()),
    ProgramInteraction(ProgramInteractionPolicyWire),
}

#[derive(BorshDeserialize)]
struct SpendingLimitPolicyWire {
    source_account_index: u8,
    destinations: Vec<Pubkey>,
    spending_limit: SpendingLimitWire,
}

#[derive(BorshDeserialize)]
struct ProgramInteractionPolicyWire {
    account_index: u8,
    instructions_constraints: Vec<InstructionConstraintWire>,
    pre_hook: Option<HookWire>,
    post_hook: Option<HookWire>,
    spending_limits: Vec<SpendingLimitWire>,
}

#[derive(BorshDeserialize)]
struct InstructionConstraintWire {
    program_id: Pubkey,
    account_constraints: Vec<AccountConstraintWire>,
    data_constraints: Vec<DataConstraintWire>,
}

#[derive(BorshDeserialize)]
struct AccountConstraintWire {
    account_index: u8,
    account_constraint: AccountConstraintTypeWire,
    owner: Option<Pubkey>,
}

#[derive(BorshDeserialize)]
enum AccountConstraintTypeWire {
    Pubkey(Vec<Pubkey>),
    AccountData(Vec<DataConstraintWire>),
}

#[derive(Clone, Debug, PartialEq, Eq, BorshDeserialize)]
enum DataOperatorWire {
    Equals,
    NotEquals,
    GreaterThan,
    GreaterThanOrEqualTo,
    LessThan,
    LessThanOrEqualTo,
}

#[derive(Clone, Debug, PartialEq, Eq, BorshDeserialize)]
enum DataValueWire {
    U8(u8),
    U16Le(u16),
    U32Le(u32),
    U64Le(u64),
    U128Le(u128),
    U8Slice(Vec<u8>),
}

#[derive(Clone, Debug, PartialEq, Eq, BorshDeserialize)]
struct DataConstraintWire {
    data_offset: u64,
    data_value: DataValueWire,
    operator: DataOperatorWire,
}

#[allow(dead_code)]
#[derive(BorshDeserialize)]
struct HookWire {
    num_extra_accounts: u8,
    account_constraints: Vec<AccountConstraintWire>,
    instruction_data: Vec<u8>,
    program_id: Pubkey,
    pass_inner_instructions: bool,
}

#[derive(BorshDeserialize)]
struct SpendingLimitWire {
    mint: Pubkey,
    time_constraints: TimeConstraintsWire,
    quantity_constraints: QuantityConstraintsWire,
    usage: UsageStateWire,
}

#[derive(BorshDeserialize)]
struct TimeConstraintsWire {
    start: i64,
    expiration: Option<i64>,
    period: PeriodV2Wire,
    accumulate_unused: bool,
}

#[derive(BorshDeserialize)]
enum PeriodV2Wire {
    OneTime,
    Daily,
    Weekly,
    Monthly,
    Custom(i64),
}

#[derive(BorshDeserialize)]
struct QuantityConstraintsWire {
    max_per_period: u64,
    max_per_use: u64,
    enforce_exact_quantity: bool,
}

#[derive(BorshDeserialize)]
struct UsageStateWire {
    remaining_in_period: u64,
    last_reset: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, BorshDeserialize)]
pub enum PolicyExpirationWire {
    Timestamp(i64),
    SettingsState([u8; 32]),
}

pub fn decode_program_interaction_policy(
    owner: Pubkey,
    data: &[u8],
) -> Result<ProgramInteractionPolicyAccount> {
    if owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        bail!("Policy account owner is not the Squads v5 program");
    }
    let mut remaining = data;
    let wire = PolicyWire::deserialize(&mut remaining).context("decode Squads Policy account")?;
    if wire.discriminator != account_discriminator("Policy") {
        bail!("Squads Policy discriminator mismatch");
    }
    // Policy accounts are allocated for the maximum expiration/state size and
    // may retain stale bytes after an update. The logical Borsh payload ends at
    // rent_collector; trailing allocation bytes are not part of policy state.
    let _trailing_allocation = remaining;
    let PolicyStateWire::ProgramInteraction(program_interaction) = wire.policy_state else {
        bail!("Squads Policy is not a ProgramInteraction policy");
    };
    if program_interaction.pre_hook.is_some()
        || program_interaction.post_hook.is_some()
        || !program_interaction.spending_limits.is_empty()
    {
        bail!("venue policy unexpectedly contains hooks or embedded spending limits");
    }

    Ok(ProgramInteractionPolicyAccount {
        settings: wire.settings,
        seed: wire.seed,
        bump: wire.bump,
        transaction_index: wire.transaction_index,
        stale_transaction_index: wire.stale_transaction_index,
        signers: wire.signers,
        threshold: wire.threshold,
        time_lock: wire.time_lock,
        account_index: program_interaction.account_index,
        constraints: program_interaction
            .instructions_constraints
            .into_iter()
            .map(instruction_constraint_view)
            .collect(),
        start: wire.start,
        expiration: wire.expiration,
        rent_collector: wire.rent_collector,
    })
}

pub fn decode_spending_limit_policy(
    owner: Pubkey,
    data: &[u8],
) -> Result<SpendingLimitPolicyAccount> {
    if owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        bail!("Policy account owner is not the Squads v5 program");
    }
    let mut remaining = data;
    let wire = PolicyWire::deserialize(&mut remaining).context("decode Squads Policy account")?;
    if wire.discriminator != account_discriminator("Policy") {
        bail!("Squads Policy discriminator mismatch");
    }
    let _trailing_allocation = remaining;
    let PolicyStateWire::SpendingLimit(spending_policy) = wire.policy_state else {
        bail!("Squads Policy is not a SpendingLimit policy");
    };
    let spending = spending_policy.spending_limit;

    Ok(SpendingLimitPolicyAccount {
        settings: wire.settings,
        seed: wire.seed,
        bump: wire.bump,
        transaction_index: wire.transaction_index,
        stale_transaction_index: wire.stale_transaction_index,
        signers: wire.signers,
        threshold: wire.threshold,
        time_lock: wire.time_lock,
        source_account_index: spending_policy.source_account_index,
        destinations: spending_policy.destinations,
        mint: spending.mint,
        spending_start: spending.time_constraints.start,
        spending_expiration: spending.time_constraints.expiration,
        period: match spending.time_constraints.period {
            PeriodV2Wire::OneTime => SpendingLimitPeriod::OneTime,
            PeriodV2Wire::Daily => SpendingLimitPeriod::Daily,
            PeriodV2Wire::Weekly => SpendingLimitPeriod::Weekly,
            PeriodV2Wire::Monthly => SpendingLimitPeriod::Monthly,
            PeriodV2Wire::Custom(seconds) => SpendingLimitPeriod::Custom(seconds),
        },
        accumulate_unused: spending.time_constraints.accumulate_unused,
        max_per_period: spending.quantity_constraints.max_per_period,
        max_per_use: spending.quantity_constraints.max_per_use,
        enforce_exact_quantity: spending.quantity_constraints.enforce_exact_quantity,
        remaining_in_period: spending.usage.remaining_in_period,
        last_reset: spending.usage.last_reset,
        start: wire.start,
        expiration: wire.expiration,
        rent_collector: wire.rent_collector,
    })
}

pub struct ExpectedProgramInteractionPolicy<'a> {
    pub policy_address: Pubkey,
    pub settings: Pubkey,
    pub seed: u64,
    pub delegated_signer: Pubkey,
    pub account_index: u8,
    pub constraints: &'a [SquadsInstructionConstraintView],
    pub rent_collector: Pubkey,
}

pub fn verify_program_interaction_policy(
    decoded: &ProgramInteractionPolicyAccount,
    expected: ExpectedProgramInteractionPolicy<'_>,
) -> Result<()> {
    let (derived_policy, derived_bump) = derive_action_account(&expected.settings, expected.seed);
    if derived_policy != expected.policy_address || decoded.bump != derived_bump {
        bail!("Policy PDA or bump does not match its Settings seed");
    }
    if decoded.settings != expected.settings
        || decoded.seed != expected.seed
        || decoded.signers
            != vec![SmartAccountSigner {
                key: expected.delegated_signer,
                permissions: crate::squads::Permissions {
                    mask: FULL_PERMISSIONS_MASK,
                },
            }]
        || decoded.threshold != 1
        || decoded.time_lock != 0
        || decoded.account_index != expected.account_index
        || decoded.constraints != expected.constraints
        || decoded.expiration.is_some()
        || decoded.rent_collector != expected.rent_collector
        || decoded.start < 0
        || decoded.stale_transaction_index > decoded.transaction_index
    {
        bail!("decoded ProgramInteraction policy does not match the required manifest");
    }
    Ok(())
}

pub struct ExpectedSpendingLimitPolicy {
    pub policy_address: Pubkey,
    pub settings: Pubkey,
    pub seed: u64,
    pub delegated_signer: Pubkey,
    pub source_account_index: u8,
    pub mint: Pubkey,
    pub destination_owner: Pubkey,
    pub rent_collector: Pubkey,
    pub expected_remaining_in_period: Option<u64>,
}

pub fn verify_spending_limit_policy(
    decoded: &SpendingLimitPolicyAccount,
    expected: ExpectedSpendingLimitPolicy,
) -> Result<()> {
    let (derived_policy, derived_bump) = derive_action_account(&expected.settings, expected.seed);
    if derived_policy != expected.policy_address || decoded.bump != derived_bump {
        bail!("SpendingLimit Policy PDA or bump does not match its Settings seed");
    }
    if decoded.settings != expected.settings
        || decoded.seed != expected.seed
        || decoded.signers
            != vec![SmartAccountSigner {
                key: expected.delegated_signer,
                permissions: crate::squads::Permissions {
                    mask: FULL_PERMISSIONS_MASK,
                },
            }]
        || decoded.threshold != 1
        || decoded.time_lock != 0
        || decoded.source_account_index != expected.source_account_index
        || decoded.destinations != vec![expected.destination_owner]
        || decoded.mint != expected.mint
        || decoded.spending_start < 0
        || decoded.spending_expiration.is_some()
        || decoded.period != SpendingLimitPeriod::OneTime
        || decoded.accumulate_unused
        || decoded.max_per_period != u64::MAX
        || decoded.max_per_use != 0
        || decoded.enforce_exact_quantity
        || decoded.remaining_in_period > decoded.max_per_period
        || decoded.last_reset != decoded.spending_start
        || decoded.start < 0
        || decoded.expiration.is_some()
        || decoded.rent_collector != expected.rent_collector
        || decoded.stale_transaction_index > decoded.transaction_index
    {
        bail!("decoded SpendingLimit policy does not match the return-to-Mother manifest");
    }
    if let Some(expected_remaining) = expected.expected_remaining_in_period {
        if decoded.remaining_in_period != expected_remaining {
            bail!("decoded SpendingLimit remaining allowance does not match execution evidence");
        }
    }
    Ok(())
}

fn instruction_constraint_view(
    constraint: InstructionConstraintWire,
) -> SquadsInstructionConstraintView {
    SquadsInstructionConstraintView {
        program_id: constraint.program_id,
        account_constraints: constraint
            .account_constraints
            .into_iter()
            .map(account_constraint_view)
            .collect(),
        data_constraints: constraint
            .data_constraints
            .into_iter()
            .map(data_constraint_view)
            .collect(),
    }
}

fn account_constraint_view(constraint: AccountConstraintWire) -> SquadsAccountConstraintView {
    let kind = match constraint.account_constraint {
        AccountConstraintTypeWire::Pubkey(pubkeys) => {
            SquadsAccountConstraintKindView::Pubkey(pubkeys)
        }
        AccountConstraintTypeWire::AccountData(constraints) => {
            SquadsAccountConstraintKindView::AccountData(
                constraints.into_iter().map(data_constraint_view).collect(),
            )
        }
    };
    SquadsAccountConstraintView {
        account_index: constraint.account_index,
        kind,
        owner: constraint.owner,
    }
}

fn data_constraint_view(constraint: DataConstraintWire) -> SquadsDataConstraintView {
    SquadsDataConstraintView {
        data_offset: constraint.data_offset,
        data_value: match constraint.data_value {
            DataValueWire::U8(value) => SquadsDataValueView::U8(value),
            DataValueWire::U16Le(value) => SquadsDataValueView::U16Le(value),
            DataValueWire::U32Le(value) => SquadsDataValueView::U32Le(value),
            DataValueWire::U64Le(value) => SquadsDataValueView::U64Le(value),
            DataValueWire::U128Le(value) => SquadsDataValueView::U128Le(value),
            DataValueWire::U8Slice(value) => SquadsDataValueView::U8Slice(value),
        },
        operator: match constraint.operator {
            DataOperatorWire::Equals => SquadsDataOperatorView::Equals,
            DataOperatorWire::NotEquals => SquadsDataOperatorView::NotEquals,
            DataOperatorWire::GreaterThan => SquadsDataOperatorView::GreaterThan,
            DataOperatorWire::GreaterThanOrEqualTo => SquadsDataOperatorView::GreaterThanOrEqualTo,
            DataOperatorWire::LessThan => SquadsDataOperatorView::LessThan,
            DataOperatorWire::LessThanOrEqualTo => SquadsDataOperatorView::LessThanOrEqualTo,
        },
    }
}

fn account_discriminator(name: &str) -> [u8; 8] {
    let preimage = format!("account:{name}");
    hashv(&[preimage.as_bytes()]).to_bytes()[..8]
        .try_into()
        .expect("Anchor discriminator is eight bytes")
}

#[cfg(test)]
mod tests {
    use borsh::BorshSerialize;

    #[test]
    fn creation_and_stored_program_interaction_variants_are_distinct() {
        #[allow(dead_code)]
        #[derive(BorshSerialize)]
        enum CreationVariantProbe {
            Internal(()),
            Spending(()),
            Settings(()),
            Legacy(()),
            ProgramInteraction(()),
        }
        #[allow(dead_code)]
        #[derive(BorshSerialize)]
        enum StoredVariantProbe {
            Internal(()),
            Spending(()),
            Settings(()),
            ProgramInteraction(()),
        }

        assert_eq!(
            borsh::to_vec(&CreationVariantProbe::ProgramInteraction(())).unwrap(),
            vec![4]
        );
        assert_eq!(
            borsh::to_vec(&StoredVariantProbe::ProgramInteraction(())).unwrap(),
            vec![3]
        );
    }
}
