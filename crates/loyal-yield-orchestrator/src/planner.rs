use crate::{CurrentReservePosition, ManagedVaultRoutePolicy, PlannedRebalanceDecisionInput};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::collections::HashMap;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SameMintPlannerConfig {
    pub targets: Vec<SameMintReserveTarget>,
    #[serde(default = "default_min_edge_bps")]
    pub min_edge_bps: i64,
    #[serde(default)]
    pub estimated_cost_lamports: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SameMintReserveTarget {
    pub reserve: String,
    pub market: String,
    pub liquidity_mint: String,
    pub supply_apy_bps: i64,
    pub accounts: KaminoReserveAccountsConfig,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct KaminoReserveAccountsConfig {
    pub lending_market_authority: String,
    pub reserve_liquidity_supply: String,
    pub reserve_collateral_mint: String,
    #[serde(default)]
    pub liquidity_token_program: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SameMintQuote {
    pub redeem_collateral_amount: u64,
    pub redeem_liquidity_amount: u64,
    pub deposit_liquidity_amount: u64,
    pub expected_collateral_amount: u64,
}

impl SameMintQuote {
    pub fn passthrough(amount: u64) -> Self {
        Self {
            redeem_collateral_amount: amount,
            redeem_liquidity_amount: amount,
            deposit_liquidity_amount: amount,
            expected_collateral_amount: amount,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SameMintPlanSkip {
    NoPolicyMode,
    NoValueSource,
    MissingSourceTarget,
    NoSameMintTarget,
    NoEdge,
    InvalidAmount,
}

#[derive(Debug, Clone)]
pub struct SameMintRoutePlanner {
    config: SameMintPlannerConfig,
}

impl SameMintRoutePlanner {
    pub fn new(config: SameMintPlannerConfig) -> Self {
        Self { config }
    }

    pub fn config(&self) -> &SameMintPlannerConfig {
        &self.config
    }

    pub fn plan_vault(
        &self,
        vault_policy: &ManagedVaultRoutePolicy,
        positions: &[CurrentReservePosition],
    ) -> Result<PlannedRebalanceDecisionInput, SameMintPlanSkip> {
        if !vault_policy
            .policy
            .route_modes
            .iter()
            .any(|mode| mode == "same_mint")
        {
            return Err(SameMintPlanSkip::NoPolicyMode);
        }

        let targets_by_reserve = self
            .config
            .targets
            .iter()
            .map(|target| (target.reserve.as_str(), target))
            .collect::<HashMap<_, _>>();

        let source = positions
            .iter()
            .filter(|position| position.has_value && position.amount_raw > 0)
            .max_by_key(|position| position.amount_raw)
            .ok_or(SameMintPlanSkip::NoValueSource)?;

        let source_target = targets_by_reserve
            .get(source.reserve.as_str())
            .copied()
            .ok_or(SameMintPlanSkip::MissingSourceTarget)?;

        let target = self
            .config
            .targets
            .iter()
            .filter(|candidate| {
                candidate.reserve != source.reserve
                    && candidate.liquidity_mint == source.liquidity_mint
            })
            .max_by_key(|candidate| candidate.supply_apy_bps)
            .ok_or(SameMintPlanSkip::NoSameMintTarget)?;

        let source_apy_bps = source
            .supply_apy_bps
            .unwrap_or(source_target.supply_apy_bps);
        let estimated_edge_bps = target.supply_apy_bps - source_apy_bps;
        if estimated_edge_bps < self.config.min_edge_bps {
            return Err(SameMintPlanSkip::NoEdge);
        }

        let amount =
            u64::try_from(source.amount_raw).map_err(|_| SameMintPlanSkip::InvalidAmount)?;
        let quote = SameMintQuote::passthrough(amount);
        let execution_plan =
            same_mint_execution_plan(vault_policy, source, source_target, target, &quote);

        Ok(PlannedRebalanceDecisionInput {
            source_snapshot_id: source.snapshot_id,
            source_reserve: source.reserve.clone(),
            target_reserve: target.reserve.clone(),
            source_liquidity_mint: source.liquidity_mint.clone(),
            target_liquidity_mint: target.liquidity_mint.clone(),
            amount_raw: source.amount_raw,
            source_apy_bps,
            target_apy_bps: target.supply_apy_bps,
            estimated_edge_bps,
            estimated_cost_lamports: self.config.estimated_cost_lamports,
            execution_plan,
        })
    }
}

fn same_mint_execution_plan(
    vault_policy: &ManagedVaultRoutePolicy,
    source: &CurrentReservePosition,
    source_target: &SameMintReserveTarget,
    target: &SameMintReserveTarget,
    quote: &SameMintQuote,
) -> Value {
    let swap_lane_count = vault_policy
        .policy
        .swap_lanes
        .as_array()
        .map_or(0, Vec::len);
    json!({
        "kind": "same_mint",
        "version": 1,
        "source_reserve": source.reserve,
        "target_reserve": target.reserve,
        "liquidity_mint": source.liquidity_mint,
        "amount_raw": source.amount_raw,
        "route": {
            "policy_account": vault_policy.policy.policy_account,
            "settings": vault_policy.vault.settings,
            "vault_pubkey": vault_policy.vault.vault_pubkey,
            "vault_index": vault_policy.vault.vault_index,
            "withdraw_constraint_index": 0,
            "deposit_constraint_index": 1 + swap_lane_count,
            "delegated_signers": vault_policy.policy.delegated_signers,
        },
        "quote": quote,
        "source": source_target,
        "target": target,
    })
}

fn default_min_edge_bps() -> i64 {
    1
}
