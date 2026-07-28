use anyhow::{bail, Context, Result};
use loyal_actions::{
    autonomous_vaults::{
        create_return_to_mother_policies, AutonomousTreasuryReturnPolicies, TreasuryReturnKind,
        TreasuryReturnPolicyPlan, LOYAL_RETURN_POLICY_SEED, MOTHER_TREASURY_VAULT,
        USDC_RETURN_POLICY_SEED,
    },
    derive_squads_vault, SQUADS_SMART_ACCOUNT_PROGRAM_ID,
};
use solana_client::rpc_client::RpcClient;
use solana_sdk::{program_pack::Pack, pubkey::Pubkey};
use spl_token::state::Mint;
use std::str::FromStr;

use crate::{
    policy::{
        decode_spending_limit_policy, verify_spending_limit_policy, ExpectedSpendingLimitPolicy,
        SpendingLimitPolicyAccount,
    },
    squads,
    state::{PolicyRecord, PolicyStatus, SmartAccountStatus, TreasuryReturnRecord, VaultState},
    token_account_amount,
};

pub const RETURN_LOYAL_TEST_RAW: u64 = 1;
pub const RETURN_USDC_REVENUE_RAW: u64 = 6;

#[derive(Clone, Debug)]
pub struct TreasuryReturnPlan {
    pub source_slot: u64,
    pub settings: Pubkey,
    pub vault: Pubkey,
    pub vault_index: u8,
    pub policies: AutonomousTreasuryReturnPolicies,
}

impl TreasuryReturnPlan {
    pub fn policy(&self, kind: TreasuryReturnKind) -> &TreasuryReturnPolicyPlan {
        self.policies.policy(kind)
    }

    pub fn amount(&self, kind: TreasuryReturnKind) -> u64 {
        match kind {
            TreasuryReturnKind::Loyal => RETURN_LOYAL_TEST_RAW,
            TreasuryReturnKind::Usdc => RETURN_USDC_REVENUE_RAW,
        }
    }
}

pub fn load_plan(
    rpc: &RpcClient,
    state: &VaultState,
    authority: Pubkey,
    delegated_signer: Pubkey,
) -> Result<TreasuryReturnPlan> {
    require_completed_meteora(state)?;
    let smart = state
        .smart_account
        .as_ref()
        .context("Smart Account state is missing")?;
    if smart.status != SmartAccountStatus::Finalized {
        bail!("Smart Account must be finalized before planning treasury returns");
    }
    let settings = Pubkey::from_str(&smart.settings).context("parse Settings address")?;
    let vault = Pubkey::from_str(&smart.vault).context("parse autonomous vault address")?;
    if derive_squads_vault(&settings, smart.vault_index).0 != vault {
        bail!("recorded autonomous vault does not derive from Settings");
    }

    let settings_account = rpc
        .get_account(&settings)
        .context("fetch Settings before planning treasury returns")?;
    if settings_account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        bail!("Settings has an unexpected owner");
    }
    let decoded_settings = squads::decode_settings(&settings_account.data)?;
    let current_policy_seed = decoded_settings
        .policy_seed
        .context("Settings policy seed is absent")?;
    if !(5..=7).contains(&current_policy_seed) {
        bail!("Settings policy seed is outside the expected return-policy sequence");
    }

    let policies = create_return_to_mother_policies(
        settings,
        authority,
        delegated_signer,
        vault,
        smart.vault_index,
        LOYAL_RETURN_POLICY_SEED,
        USDC_RETURN_POLICY_SEED,
    )?;
    validate_token_graph(rpc, vault, &policies)?;

    Ok(TreasuryReturnPlan {
        source_slot: rpc.get_slot()?,
        settings,
        vault,
        vault_index: smart.vault_index,
        policies,
    })
}

pub fn record_from_plan(plan: &TreasuryReturnPlan) -> TreasuryReturnRecord {
    TreasuryReturnRecord {
        source_slot: plan.source_slot,
        mother: MOTHER_TREASURY_VAULT.to_string(),
        vault_loyal_token_account: plan.policies.loyal.source_token_account.to_string(),
        vault_usdc_token_account: plan.policies.usdc.source_token_account.to_string(),
        mother_loyal_token_account: plan.policies.loyal.destination_token_account.to_string(),
        mother_usdc_token_account: plan.policies.usdc.destination_token_account.to_string(),
        loyal_policy: None,
        usdc_policy: None,
        live_steps: Vec::new(),
    }
}

pub fn validate_record(record: &TreasuryReturnRecord, plan: &TreasuryReturnPlan) -> Result<()> {
    let expected = record_from_plan(plan);
    if record.mother != expected.mother
        || record.vault_loyal_token_account != expected.vault_loyal_token_account
        || record.vault_usdc_token_account != expected.vault_usdc_token_account
        || record.mother_loyal_token_account != expected.mother_loyal_token_account
        || record.mother_usdc_token_account != expected.mother_usdc_token_account
    {
        bail!("persisted treasury-return account graph differs from the fresh plan");
    }
    Ok(())
}

pub fn decode_and_verify_policy(
    rpc: &RpcClient,
    plan: &TreasuryReturnPlan,
    kind: TreasuryReturnKind,
    delegated_signer: Pubkey,
    rent_collector: Pubkey,
    expected_remaining_in_period: Option<u64>,
) -> Result<SpendingLimitPolicyAccount> {
    let policy = plan.policy(kind);
    let account = rpc
        .get_account(&policy.policy)
        .with_context(|| format!("fetch {} return policy", kind.label()))?;
    let decoded = decode_spending_limit_policy(account.owner, &account.data)?;
    verify_spending_limit_policy(
        &decoded,
        ExpectedSpendingLimitPolicy {
            policy_address: policy.policy,
            settings: plan.settings,
            seed: policy.policy_seed,
            delegated_signer,
            source_account_index: plan.vault_index,
            mint: policy.mint,
            destination_owner: MOTHER_TREASURY_VAULT,
            rent_collector,
            expected_remaining_in_period,
        },
    )?;
    Ok(decoded)
}

pub fn policy_record(
    record: &TreasuryReturnRecord,
    kind: TreasuryReturnKind,
) -> Option<&PolicyRecord> {
    match kind {
        TreasuryReturnKind::Loyal => record.loyal_policy.as_ref(),
        TreasuryReturnKind::Usdc => record.usdc_policy.as_ref(),
    }
}

pub fn policy_record_mut(
    record: &mut TreasuryReturnRecord,
    kind: TreasuryReturnKind,
) -> &mut Option<PolicyRecord> {
    match kind {
        TreasuryReturnKind::Loyal => &mut record.loyal_policy,
        TreasuryReturnKind::Usdc => &mut record.usdc_policy,
    }
}

fn validate_token_graph(
    rpc: &RpcClient,
    vault: Pubkey,
    policies: &AutonomousTreasuryReturnPolicies,
) -> Result<()> {
    for policy in [&policies.loyal, &policies.usdc] {
        let mint_account = rpc
            .get_account(&policy.mint)
            .with_context(|| format!("fetch {} mint", policy.kind.label()))?;
        if mint_account.owner != spl_token::id() {
            bail!(
                "{} mint is not a classic Tokenkeg mint",
                policy.kind.label()
            );
        }
        let mint = Mint::unpack(&mint_account.data)
            .with_context(|| format!("decode {} mint", policy.kind.label()))?;
        if mint.decimals != 6 {
            bail!("{} mint decimals are not six", policy.kind.label());
        }
        token_account_amount(rpc, policy.source_token_account, vault, policy.mint)?
            .with_context(|| format!("vault {} ATA is absent", policy.kind.label()))?;
        token_account_amount(
            rpc,
            policy.destination_token_account,
            MOTHER_TREASURY_VAULT,
            policy.mint,
        )?
        .with_context(|| format!("Mother {} ATA is absent", policy.kind.label()))?;
    }
    Ok(())
}

fn require_completed_meteora(state: &VaultState) -> Result<()> {
    let meteora = state
        .meteora
        .as_ref()
        .context("Meteora state is missing before treasury returns")?;
    for policy in [
        meteora.add_liquidity_policy.as_ref(),
        meteora.remove_liquidity_policy.as_ref(),
        meteora.claim_fee_policy.as_ref(),
    ] {
        if policy.map(|record| record.status) != Some(PolicyStatus::Finalized) {
            bail!("all Meteora policies must be finalized before treasury returns");
        }
    }
    if meteora
        .live_steps
        .iter()
        .find(|step| step.name == "meteora-claim-fees")
        .map(|step| step.status)
        != Some(PolicyStatus::Finalized)
    {
        bail!("Meteora fee claim must be finalized before treasury returns");
    }
    Ok(())
}
