//! Autonomous treasury-vault policy plans.
//!
//! This module is the review surface for the first Loyal autonomous vault. It
//! keeps protocol-specific constraints separate while sharing the repository's
//! deployed Squads v5 `ProgramInteraction` policy encoder.

mod kamino;
mod meteora;
mod returns;
mod voltr_custom;
mod voltr_kamino;

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
pub use voltr_custom::{
    create_voltr_custom_policies, VoltrCustomPolicies, VoltrCustomPolicyError,
    VoltrCustomPolicyIdentity, VoltrCustomPolicyPlan, VoltrCustomPolicySeeds,
    VoltrCustomPolicyTemplates, CUSTOM_ADAPTOR_DEPOSIT_DISCRIMINATOR,
    CUSTOM_ADAPTOR_WITHDRAW_DISCRIMINATOR,
};
pub use voltr_kamino::{
    create_backyard_voltr_runtime_policy_catalog, create_voltr_kamino_policies,
    create_voltr_kamino_runtime_policies, embedded_backyard_voltr_route_bundle,
    BackyardVoltrBundleError, BackyardVoltrManagerOperation, BackyardVoltrManagerTemplate,
    BackyardVoltrRouteBundle, BackyardVoltrRuntimePolicyCatalog, BackyardVoltrRuntimePolicySpec,
    BackyardVoltrStrategy, VoltrKaminoConstraintProfile, VoltrKaminoPolicies,
    VoltrKaminoPolicyError, VoltrKaminoPolicyPlan, VoltrKaminoPolicySeeds,
    VoltrKaminoPolicyTemplate, VoltrKaminoRuntimePolicies, VoltrKaminoRuntimePolicySeeds,
    VoltrKaminoRuntimePolicyTemplate, BACKYARD_VOLTR_IDLE_ATA, BACKYARD_VOLTR_LP_MINT,
    BACKYARD_VOLTR_NORMAL_OPTIMIZATION_INTERVAL_SECONDS,
    BACKYARD_VOLTR_POLICY_ARTIFACT_FILE_SHA256, BACKYARD_VOLTR_POLICY_ARTIFACT_SHA256,
    BACKYARD_VOLTR_ROUTE_ID, BACKYARD_VOLTR_ROUTE_SPEC_SHA256, BACKYARD_VOLTR_STRATEGY_FARMS,
    BACKYARD_VOLTR_STRATEGY_IDS, BACKYARD_VOLTR_STRATEGY_LENDING_MARKETS,
    BACKYARD_VOLTR_STRATEGY_RESERVES, BACKYARD_VOLTR_WITHDRAWAL_WAIT_SECONDS,
    VOLTR_DEPOSIT_STRATEGY_DISCRIMINATOR, VOLTR_INITIALIZE_STRATEGY_DISCRIMINATOR,
    VOLTR_KAMINO_ADAPTOR_PROGRAM_ID, VOLTR_KAMINO_DEPOSIT_MARKET_DISCRIMINATOR,
    VOLTR_KAMINO_INITIALIZE_MARKET_DISCRIMINATOR, VOLTR_KAMINO_WITHDRAW_MARKET_DISCRIMINATOR,
    VOLTR_VAULT_PROGRAM_ID, VOLTR_WITHDRAW_STRATEGY_DISCRIMINATOR,
};
