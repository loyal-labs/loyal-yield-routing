//! Autonomous treasury-vault policy plans.
//!
//! This module is the review surface for the first Loyal autonomous vault. It
//! keeps protocol-specific constraints separate while sharing the repository's
//! deployed Squads v5 `ProgramInteraction` policy encoder.

mod kamino;
mod meteora;
mod returns;

pub use kamino::{
    create_kamino_policies, AutonomousKaminoPolicies, AutonomousVaultError,
    KaminoPolicyConstraintIndexes, KaminoPolicyPlan, KaminoReservePolicyTemplate,
};
pub use meteora::{
    create_meteora_policies, derive_meteora_vault_token_accounts, AutonomousMeteoraPolicies,
    MeteoraPolicyError, MeteoraPolicyPlan, METEORA_ADD_LIQUIDITY_BY_STRATEGY2_DISCRIMINATOR,
    METEORA_CLAIM_FEE2_DISCRIMINATOR, METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO,
    METEORA_DLMM_PROGRAM_ID, METEORA_EVENT_AUTHORITY, METEORA_LOYAL_MINT, METEORA_LOYAL_RESERVE,
    METEORA_MEMO_PROGRAM_ID, METEORA_POOL, METEORA_REMOVE_LIQUIDITY_BY_RANGE2_DISCRIMINATOR,
    METEORA_SPOT_BALANCED_STRATEGY, METEORA_USDC_RESERVE,
};
pub use returns::{
    create_return_to_mother_policies, return_to_mother_instruction,
    AutonomousTreasuryReturnPolicies, TreasuryReturnKind, TreasuryReturnPolicyError,
    TreasuryReturnPolicyPlan, LOYAL_RETURN_POLICY_SEED, MOTHER_TREASURY_VAULT,
    USDC_RETURN_POLICY_SEED,
};
