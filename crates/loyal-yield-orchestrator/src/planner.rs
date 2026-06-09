use crate::{CurrentReservePosition, ManagedVaultRoutePolicy, PlannedRebalanceDecisionInput};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::{collections::HashMap, future::Future};
use thiserror::Error;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct YieldRoutePlannerConfig {
    pub targets: Vec<YieldReserveTarget>,
    #[serde(default = "default_min_edge_bps")]
    pub min_edge_bps: i64,
    #[serde(default)]
    pub estimated_cost_lamports: i64,
}

pub type SameMintPlannerConfig = YieldRoutePlannerConfig;
pub type SameMintReserveTarget = YieldReserveTarget;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct YieldReserveTarget {
    pub reserve: String,
    pub market: String,
    pub liquidity_mint: String,
    pub supply_apy_bps: i64,
    pub accounts: KaminoReserveAccountsConfig,
    #[serde(default)]
    pub metadata: Value,
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

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CrossMintQuote {
    pub redeem_collateral_amount: u64,
    pub redeem_liquidity_amount: u64,
    pub swap: SwapQuote,
    pub deposit_liquidity_amount: u64,
    pub expected_collateral_amount: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SwapQuote {
    pub lane_kind: String,
    pub lane_index: u8,
    pub source_mint: String,
    pub target_mint: String,
    pub amount_in: u64,
    pub min_out: u64,
    #[serde(default)]
    pub max_slippage_bps: Option<u16>,
    #[serde(default)]
    pub max_fee_bps: Option<u16>,
    #[serde(default)]
    pub instruction: Option<RouteInstructionConfig>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct RouteInstructionConfig {
    pub program_id: String,
    pub accounts: Vec<RouteAccountMetaConfig>,
    pub data: Vec<u8>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct RouteAccountMetaConfig {
    pub pubkey: String,
    #[serde(default)]
    pub is_signer: bool,
    #[serde(default)]
    pub is_writable: bool,
}

#[derive(Debug, Clone, Copy)]
pub struct SameMintQuoteRequest<'a> {
    pub source: &'a CurrentReservePosition,
    pub source_target: &'a YieldReserveTarget,
    pub target: &'a YieldReserveTarget,
    pub amount: u64,
}

#[derive(Debug, Clone)]
pub struct CrossMintQuoteRequest<'a> {
    pub vault_pubkey: &'a str,
    pub source: &'a CurrentReservePosition,
    pub source_target: &'a YieldReserveTarget,
    pub target: &'a YieldReserveTarget,
    pub lane: CrossMintSwapLane,
    pub amount: u64,
    pub redeem_liquidity_amount: u64,
}

pub trait RouteQuoteProvider {
    fn quote_same_mint(
        &self,
        request: SameMintQuoteRequest<'_>,
    ) -> impl Future<Output = Result<SameMintQuote, RouteQuoteError>> + Send;

    fn quote_cross_mint(
        &self,
        request: CrossMintQuoteRequest<'_>,
    ) -> impl Future<Output = Result<CrossMintQuote, RouteQuoteError>> + Send;
}

#[derive(Debug, Clone, Default)]
pub struct ConservativeRouteQuoteProvider;

impl RouteQuoteProvider for ConservativeRouteQuoteProvider {
    fn quote_same_mint(
        &self,
        request: SameMintQuoteRequest<'_>,
    ) -> impl Future<Output = Result<SameMintQuote, RouteQuoteError>> + Send {
        async move { Ok(SameMintQuote::passthrough(request.amount)) }
    }

    fn quote_cross_mint(
        &self,
        _request: CrossMintQuoteRequest<'_>,
    ) -> impl Future<Output = Result<CrossMintQuote, RouteQuoteError>> + Send {
        async move {
            Err(RouteQuoteError::Unavailable(
                "cross-mint quote provider is not configured".to_owned(),
            ))
        }
    }
}

#[derive(Debug, Error, Clone, PartialEq, Eq)]
pub enum RouteQuoteError {
    #[error("{0}")]
    Unavailable(String),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RoutePlanSkip {
    NoPolicyMode,
    NoValueSource,
    MissingSourceTarget,
    NoSameMintTarget,
    NoCrossMintTarget,
    SplitSwapPolicyUnsupported,
    NoEdge,
    InvalidAmount,
    QuoteUnavailable(String),
}

pub type SameMintPlanSkip = RoutePlanSkip;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CrossMintSwapLane {
    pub lane_index: u8,
    pub constraint_index: u8,
    pub kind: CrossMintSwapLaneKind,
    pub max_slippage_bps: Option<u16>,
    pub max_fee_bps: Option<u16>,
    pub policy_account: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CrossMintSwapLaneKind {
    Jupiter,
    LoyalHub,
}

impl CrossMintSwapLaneKind {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Jupiter => "jupiter",
            Self::LoyalHub => "loyal_hub",
        }
    }
}

#[derive(Debug, Clone)]
pub struct YieldRoutePlanner {
    config: YieldRoutePlannerConfig,
}

pub type SameMintRoutePlanner = YieldRoutePlanner;

impl YieldRoutePlanner {
    pub fn new(config: YieldRoutePlannerConfig) -> Self {
        Self { config }
    }

    pub fn config(&self) -> &YieldRoutePlannerConfig {
        &self.config
    }

    pub async fn plan_vault<Q>(
        &self,
        vault_policy: &ManagedVaultRoutePolicy,
        positions: &[CurrentReservePosition],
        quote_provider: &Q,
    ) -> Result<PlannedRebalanceDecisionInput, RoutePlanSkip>
    where
        Q: RouteQuoteProvider + Sync,
    {
        let source = positions
            .iter()
            .filter(|position| position.has_value && position.amount_raw > 0)
            .max_by_key(|position| position.amount_raw)
            .ok_or(RoutePlanSkip::NoValueSource)?;

        let amount = u64::try_from(source.amount_raw).map_err(|_| RoutePlanSkip::InvalidAmount)?;
        let targets_by_reserve = self
            .config
            .targets
            .iter()
            .map(|target| (target.reserve.as_str(), target))
            .collect::<HashMap<_, _>>();
        let source_target = targets_by_reserve
            .get(source.reserve.as_str())
            .copied()
            .ok_or(RoutePlanSkip::MissingSourceTarget)?;
        let source_apy_bps = source
            .supply_apy_bps
            .unwrap_or(source_target.supply_apy_bps);

        let same_mint_candidate = self.best_same_mint_target(vault_policy, source, source_apy_bps);
        let cross_mint_candidate =
            self.best_cross_mint_target(vault_policy, source, source_apy_bps);
        if same_mint_candidate.is_none()
            && cross_mint_candidate.is_none()
            && supports_cross_mint(vault_policy)
            && uses_split_swap_policy(vault_policy)
        {
            return Err(RoutePlanSkip::SplitSwapPolicyUnsupported);
        }

        match (same_mint_candidate, cross_mint_candidate) {
            (Some(same), Some((cross, lane))) if cross.edge_bps > same.edge_bps => {
                match self
                    .plan_cross_mint(
                        vault_policy,
                        source,
                        source_target,
                        cross.target,
                        cross.edge_bps,
                        lane,
                        amount,
                        quote_provider,
                    )
                    .await
                {
                    Ok(plan) => Ok(plan),
                    Err(RoutePlanSkip::QuoteUnavailable(_)) => {
                        self.plan_same_mint(
                            vault_policy,
                            source,
                            source_target,
                            same.target,
                            same.edge_bps,
                            amount,
                            quote_provider,
                        )
                        .await
                    }
                    Err(other) => Err(other),
                }
            }
            (Some(same), _) => {
                self.plan_same_mint(
                    vault_policy,
                    source,
                    source_target,
                    same.target,
                    same.edge_bps,
                    amount,
                    quote_provider,
                )
                .await
            }
            (None, Some((cross, lane))) => {
                self.plan_cross_mint(
                    vault_policy,
                    source,
                    source_target,
                    cross.target,
                    cross.edge_bps,
                    lane,
                    amount,
                    quote_provider,
                )
                .await
            }
            (None, None)
                if !supports_same_mint(vault_policy) && cross_mint_lane(vault_policy).is_none() =>
            {
                Err(RoutePlanSkip::NoPolicyMode)
            }
            (None, None) => Err(RoutePlanSkip::NoEdge),
        }
    }

    fn best_same_mint_target<'a>(
        &'a self,
        vault_policy: &ManagedVaultRoutePolicy,
        source: &CurrentReservePosition,
        source_apy_bps: i64,
    ) -> Option<RouteCandidate<'a>> {
        if !supports_same_mint(vault_policy) {
            return None;
        }

        self.config
            .targets
            .iter()
            .filter(|target| {
                target.reserve != source.reserve
                    && target.liquidity_mint == source.liquidity_mint
                    && target_allowed_by_policy(target, vault_policy)
            })
            .filter_map(|target| {
                let edge_bps = target.supply_apy_bps - source_apy_bps;
                (edge_bps >= self.config.min_edge_bps)
                    .then_some(RouteCandidate { target, edge_bps })
            })
            .max_by_key(|candidate| candidate.edge_bps)
    }

    fn best_cross_mint_target<'a>(
        &'a self,
        vault_policy: &ManagedVaultRoutePolicy,
        source: &CurrentReservePosition,
        source_apy_bps: i64,
    ) -> Option<(RouteCandidate<'a>, CrossMintSwapLane)> {
        let lane = cross_mint_lane(vault_policy)?;
        self.config
            .targets
            .iter()
            .filter(|target| {
                target.reserve != source.reserve
                    && target.liquidity_mint != source.liquidity_mint
                    && target_allowed_by_policy(target, vault_policy)
                    && mint_allowed_for_cross_mint(&source.liquidity_mint, vault_policy)
                    && mint_allowed_for_cross_mint(&target.liquidity_mint, vault_policy)
            })
            .filter_map(|target| {
                let edge_bps = target.supply_apy_bps - source_apy_bps;
                (edge_bps >= self.config.min_edge_bps)
                    .then_some(RouteCandidate { target, edge_bps })
            })
            .max_by_key(|candidate| candidate.edge_bps)
            .map(|candidate| (candidate, lane))
    }

    async fn plan_same_mint<Q>(
        &self,
        vault_policy: &ManagedVaultRoutePolicy,
        source: &CurrentReservePosition,
        source_target: &YieldReserveTarget,
        target: &YieldReserveTarget,
        edge_bps: i64,
        amount: u64,
        quote_provider: &Q,
    ) -> Result<PlannedRebalanceDecisionInput, RoutePlanSkip>
    where
        Q: RouteQuoteProvider + Sync,
    {
        let quote = quote_provider
            .quote_same_mint(SameMintQuoteRequest {
                source,
                source_target,
                target,
                amount,
            })
            .await
            .map_err(|error| RoutePlanSkip::QuoteUnavailable(error.to_string()))?;
        let execution_plan =
            same_mint_execution_plan(vault_policy, source, source_target, target, &quote);

        Ok(PlannedRebalanceDecisionInput {
            source_snapshot_id: source.snapshot_id,
            source_reserve: source.reserve.clone(),
            target_reserve: target.reserve.clone(),
            source_liquidity_mint: source.liquidity_mint.clone(),
            target_liquidity_mint: target.liquidity_mint.clone(),
            amount_raw: source.amount_raw,
            source_apy_bps: source
                .supply_apy_bps
                .unwrap_or(source_target.supply_apy_bps),
            target_apy_bps: target.supply_apy_bps,
            estimated_edge_bps: edge_bps,
            estimated_cost_lamports: self.config.estimated_cost_lamports,
            execution_plan,
        })
    }

    async fn plan_cross_mint<Q>(
        &self,
        vault_policy: &ManagedVaultRoutePolicy,
        source: &CurrentReservePosition,
        source_target: &YieldReserveTarget,
        target: &YieldReserveTarget,
        edge_bps: i64,
        lane: CrossMintSwapLane,
        amount: u64,
        quote_provider: &Q,
    ) -> Result<PlannedRebalanceDecisionInput, RoutePlanSkip>
    where
        Q: RouteQuoteProvider + Sync,
    {
        let quote = quote_provider
            .quote_cross_mint(CrossMintQuoteRequest {
                vault_pubkey: &vault_policy.vault.vault_pubkey,
                source,
                source_target,
                target,
                lane: lane.clone(),
                amount,
                redeem_liquidity_amount: collateral_to_liquidity_amount(
                    amount,
                    source,
                    source_target,
                )?,
            })
            .await
            .map_err(|error| RoutePlanSkip::QuoteUnavailable(error.to_string()))?;
        if quote.swap.instruction.is_none() {
            return Err(RoutePlanSkip::QuoteUnavailable(
                "cross-mint quote did not include a swap instruction".to_owned(),
            ));
        }
        let execution_plan =
            cross_mint_execution_plan(vault_policy, source, source_target, target, lane, &quote);

        Ok(PlannedRebalanceDecisionInput {
            source_snapshot_id: source.snapshot_id,
            source_reserve: source.reserve.clone(),
            target_reserve: target.reserve.clone(),
            source_liquidity_mint: source.liquidity_mint.clone(),
            target_liquidity_mint: target.liquidity_mint.clone(),
            amount_raw: source.amount_raw,
            source_apy_bps: source
                .supply_apy_bps
                .unwrap_or(source_target.supply_apy_bps),
            target_apy_bps: target.supply_apy_bps,
            estimated_edge_bps: edge_bps,
            estimated_cost_lamports: self.config.estimated_cost_lamports,
            execution_plan,
        })
    }
}

#[derive(Clone, Copy)]
struct RouteCandidate<'a> {
    target: &'a YieldReserveTarget,
    edge_bps: i64,
}

fn same_mint_execution_plan(
    vault_policy: &ManagedVaultRoutePolicy,
    source: &CurrentReservePosition,
    source_target: &YieldReserveTarget,
    target: &YieldReserveTarget,
    quote: &SameMintQuote,
) -> Value {
    let deposit_constraint_index = deposit_constraint_index(vault_policy);
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
            "deposit_constraint_index": deposit_constraint_index,
            "delegated_signers": vault_policy.policy.delegated_signers,
        },
        "quote": quote,
        "source": source_target,
        "target": target,
    })
}

fn cross_mint_execution_plan(
    vault_policy: &ManagedVaultRoutePolicy,
    source: &CurrentReservePosition,
    source_target: &YieldReserveTarget,
    target: &YieldReserveTarget,
    lane: CrossMintSwapLane,
    quote: &CrossMintQuote,
) -> Value {
    let deposit_constraint_index = deposit_constraint_index(vault_policy);
    json!({
        "kind": "cross_mint",
        "version": 1,
        "source_reserve": source.reserve,
        "target_reserve": target.reserve,
        "source_liquidity_mint": source.liquidity_mint,
        "target_liquidity_mint": target.liquidity_mint,
        "amount_raw": source.amount_raw,
        "route": {
            "policy_account": vault_policy.policy.policy_account,
            "swap_policy_account": lane.policy_account,
            "settings": vault_policy.vault.settings,
            "vault_pubkey": vault_policy.vault.vault_pubkey,
            "vault_index": vault_policy.vault.vault_index,
            "withdraw_constraint_index": 0,
            "swap_constraint_index": lane.constraint_index,
            "deposit_constraint_index": deposit_constraint_index,
            "swap_lane_index": lane.lane_index,
            "swap_lane_kind": lane.kind.as_str(),
            "delegated_signers": vault_policy.policy.delegated_signers,
        },
        "quote": quote,
        "source": source_target,
        "target": target,
    })
}

fn supports_same_mint(vault_policy: &ManagedVaultRoutePolicy) -> bool {
    vault_policy
        .policy
        .route_modes
        .iter()
        .any(|mode| mode == "same_mint")
}

fn supports_cross_mint(vault_policy: &ManagedVaultRoutePolicy) -> bool {
    vault_policy
        .policy
        .route_modes
        .iter()
        .any(|mode| mode.starts_with("cross_mint_"))
}

fn cross_mint_lane(vault_policy: &ManagedVaultRoutePolicy) -> Option<CrossMintSwapLane> {
    let route_modes = &vault_policy.policy.route_modes;
    let lanes = vault_policy.policy.swap_lanes.as_array()?;
    if uses_split_swap_policy(vault_policy) {
        return None;
    }
    for (index, lane) in lanes.iter().enumerate() {
        let kind = lane.get("kind").and_then(Value::as_str)?;
        let lane_index = u8::try_from(index).ok()?;
        let constraint_index = lane
            .get("constraint_index")
            .and_then(Value::as_u64)
            .and_then(|value| u8::try_from(value).ok())
            .or_else(|| lane_index.checked_add(1))?;
        let policy_account = lane
            .get("policy_account")
            .and_then(Value::as_str)
            .map(str::to_owned);
        match kind {
            "jupiter" if route_modes.iter().any(|mode| mode == "cross_mint_jupiter") => {
                return Some(CrossMintSwapLane {
                    lane_index,
                    constraint_index,
                    kind: CrossMintSwapLaneKind::Jupiter,
                    max_slippage_bps: lane
                        .get("max_slippage_bps")
                        .and_then(Value::as_u64)
                        .and_then(|value| u16::try_from(value).ok()),
                    max_fee_bps: None,
                    policy_account,
                });
            }
            "loyal_hub"
                if route_modes
                    .iter()
                    .any(|mode| mode == "cross_mint_loyal_hub") =>
            {
                return Some(CrossMintSwapLane {
                    lane_index,
                    constraint_index,
                    kind: CrossMintSwapLaneKind::LoyalHub,
                    max_slippage_bps: None,
                    max_fee_bps: lane
                        .get("max_fee_bps")
                        .and_then(Value::as_u64)
                        .and_then(|value| u16::try_from(value).ok()),
                    policy_account,
                });
            }
            _ => {}
        }
    }
    None
}

fn deposit_constraint_index(vault_policy: &ManagedVaultRoutePolicy) -> u8 {
    1 + swap_lane_count(vault_policy)
}

fn uses_split_swap_policy(vault_policy: &ManagedVaultRoutePolicy) -> bool {
    vault_policy
        .policy
        .swap_lanes
        .as_array()
        .is_some_and(|lanes| {
            lanes
                .iter()
                .any(|lane| lane.get("policy_account").and_then(Value::as_str).is_some())
        })
}

fn swap_lane_count(vault_policy: &ManagedVaultRoutePolicy) -> u8 {
    vault_policy
        .policy
        .swap_lanes
        .as_array()
        .and_then(|lanes| u8::try_from(lanes.len()).ok())
        .unwrap_or_default()
}

fn target_allowed_by_policy(
    target: &YieldReserveTarget,
    vault_policy: &ManagedVaultRoutePolicy,
) -> bool {
    list_allows(&vault_policy.policy.kamino_markets, &target.market)
        && list_allows(
            &vault_policy.policy.kamino_liquidity_mints,
            &target.liquidity_mint,
        )
        && mint_allowed_for_cross_mint(&target.liquidity_mint, vault_policy)
}

fn mint_allowed_for_cross_mint(mint: &str, vault_policy: &ManagedVaultRoutePolicy) -> bool {
    list_allows(&vault_policy.policy.stable_mints, mint)
}

fn list_allows(allowlist: &[String], value: &str) -> bool {
    allowlist.is_empty() || allowlist.iter().any(|allowed| allowed == value)
}

fn collateral_to_liquidity_amount(
    collateral_amount: u64,
    source: &CurrentReservePosition,
    source_target: &YieldReserveTarget,
) -> Result<u64, RoutePlanSkip> {
    let Some(rate) = collateral_to_liquidity_rate(source, source_target) else {
        return Ok(collateral_amount);
    };
    let amount = u128::from(collateral_amount)
        .checked_mul(u128::from(rate.liquidity_per_scale_collateral))
        .ok_or(RoutePlanSkip::InvalidAmount)?
        / u128::from(rate.scale);
    u64::try_from(amount).map_err(|_| RoutePlanSkip::InvalidAmount)
}

struct CollateralToLiquidityRate {
    scale: u64,
    liquidity_per_scale_collateral: u64,
}

fn collateral_to_liquidity_rate(
    source: &CurrentReservePosition,
    source_target: &YieldReserveTarget,
) -> Option<CollateralToLiquidityRate> {
    source
        .planning_metadata
        .pointer("/reserve/metadata/collateralToLiquidityRate")
        .or_else(|| source_target.metadata.get("collateralToLiquidityRate"))
        .and_then(parse_collateral_to_liquidity_rate)
}

fn parse_collateral_to_liquidity_rate(value: &Value) -> Option<CollateralToLiquidityRate> {
    let scale = parse_u64_json(value.get("scale")?)?;
    let liquidity_per_scale_collateral = parse_u64_json(value.get("liquidityPerScaleCollateral")?)?;
    (scale > 0).then_some(CollateralToLiquidityRate {
        scale,
        liquidity_per_scale_collateral,
    })
}

fn parse_u64_json(value: &Value) -> Option<u64> {
    value
        .as_u64()
        .or_else(|| value.as_str().and_then(|raw| raw.parse::<u64>().ok()))
}

fn default_min_edge_bps() -> i64 {
    1
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::{PolicyId, RoutePolicy, SnapshotId, VaultId};
    use chrono::Utc;

    fn position(
        reserve: &str,
        liquidity_mint: &str,
        amount_raw: i64,
        supply_apy_bps: Option<i64>,
    ) -> CurrentReservePosition {
        CurrentReservePosition {
            vault_id: VaultId(1),
            reserve: reserve.to_owned(),
            market: Some("market-a".to_owned()),
            liquidity_mint: liquidity_mint.to_owned(),
            amount_raw,
            has_value: amount_raw > 0,
            supply_apy_bps,
            borrow_apy_bps: None,
            snapshot_id: SnapshotId(7),
            observed_slot: 42,
            observed_at: Utc::now(),
            planning_metadata: json!({}),
        }
    }

    fn target(reserve: &str, market: &str, liquidity_mint: &str, apy: i64) -> YieldReserveTarget {
        YieldReserveTarget {
            reserve: reserve.to_owned(),
            market: market.to_owned(),
            liquidity_mint: liquidity_mint.to_owned(),
            supply_apy_bps: apy,
            accounts: KaminoReserveAccountsConfig {
                lending_market_authority: "".to_owned(),
                reserve_liquidity_supply: format!("{reserve}-supply"),
                reserve_collateral_mint: format!("{reserve}-collateral"),
                liquidity_token_program: None,
            },
            metadata: json!({}),
        }
    }

    fn vault_policy(route_modes: Vec<&str>, swap_lanes: Value) -> ManagedVaultRoutePolicy {
        ManagedVaultRoutePolicy {
            vault: crate::ManagedVault {
                id: VaultId(1),
                cluster: "test".to_owned(),
                settings: "settings".to_owned(),
                vault_index: 2,
                vault_pubkey: "vault".to_owned(),
                active_policy_id: PolicyId(1),
                active: true,
                first_seen_at: Utc::now(),
                last_seen_at: Utc::now(),
            },
            policy: RoutePolicy {
                id: PolicyId(1),
                cluster: "test".to_owned(),
                settings: "settings".to_owned(),
                authority: "authority".to_owned(),
                policy_seed: 1,
                policy_account: "policy".to_owned(),
                vault_index: 2,
                vault_pubkey: "vault".to_owned(),
                delegated_signers: vec!["signer".to_owned()],
                threshold: 1,
                route_modes: route_modes.into_iter().map(str::to_owned).collect(),
                stable_mints: vec!["USDC".to_owned(), "PYUSD".to_owned()],
                kamino_markets: vec!["market-a".to_owned(), "market-b".to_owned()],
                kamino_liquidity_mints: vec!["USDC".to_owned(), "PYUSD".to_owned()],
                universe_preset: None,
                risk_profile: None,
                swap_lanes,
                active: true,
                first_seen_at: Utc::now(),
                last_seen_at: Utc::now(),
                last_seen_slot: 1,
                last_seen_signature: "sig".to_owned(),
            },
        }
    }

    #[derive(Default)]
    struct StaticQuoteProvider;

    impl RouteQuoteProvider for StaticQuoteProvider {
        fn quote_same_mint(
            &self,
            request: SameMintQuoteRequest<'_>,
        ) -> impl Future<Output = Result<SameMintQuote, RouteQuoteError>> + Send {
            async move { Ok(SameMintQuote::passthrough(request.amount)) }
        }

        fn quote_cross_mint(
            &self,
            request: CrossMintQuoteRequest<'_>,
        ) -> impl Future<Output = Result<CrossMintQuote, RouteQuoteError>> + Send {
            async move {
                Ok(CrossMintQuote {
                    redeem_collateral_amount: request.amount,
                    redeem_liquidity_amount: request.redeem_liquidity_amount,
                    swap: SwapQuote {
                        lane_kind: request.lane.kind.as_str().to_owned(),
                        lane_index: request.lane.lane_index,
                        source_mint: request.source.liquidity_mint.clone(),
                        target_mint: request.target.liquidity_mint.clone(),
                        amount_in: request.redeem_liquidity_amount,
                        min_out: request.redeem_liquidity_amount.saturating_sub(10),
                        max_slippage_bps: request.lane.max_slippage_bps,
                        max_fee_bps: request.lane.max_fee_bps,
                        instruction: Some(RouteInstructionConfig {
                            program_id: "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4".to_owned(),
                            accounts: vec![],
                            data: vec![1, 2, 3],
                        }),
                    },
                    deposit_liquidity_amount: request.redeem_liquidity_amount.saturating_sub(10),
                    expected_collateral_amount: request.redeem_liquidity_amount.saturating_sub(10),
                })
            }
        }
    }

    #[tokio::test]
    async fn drafts_same_mint_move_all_decision() {
        let planner = YieldRoutePlanner::new(YieldRoutePlannerConfig {
            targets: vec![
                target("reserve-a", "market-a", "USDC", 100),
                target("reserve-b", "market-a", "USDC", 160),
                target("reserve-c", "market-b", "PYUSD", 500),
            ],
            min_edge_bps: 10,
            estimated_cost_lamports: 0,
        });
        let positions = vec![
            position("reserve-a", "USDC", 1_000, Some(100)),
            position("reserve-b", "USDC", 0, Some(160)),
        ];

        let planned = planner
            .plan_vault(
                &vault_policy(vec!["same_mint"], json!([])),
                &positions,
                &StaticQuoteProvider,
            )
            .await
            .unwrap();

        assert_eq!(planned.source_reserve, "reserve-a");
        assert_eq!(planned.target_reserve, "reserve-b");
        assert_eq!(planned.amount_raw, 1_000);
        assert_eq!(planned.estimated_edge_bps, 60);
        assert_eq!(planned.execution_plan["kind"], "same_mint");
        assert_eq!(
            planned.execution_plan["route"]["deposit_constraint_index"],
            1
        );
    }

    #[tokio::test]
    async fn cross_mint_quotes_redeemed_liquidity_not_collateral_amount() {
        let mut source = target("reserve-a", "market-a", "USDC", 100);
        source.metadata = json!({
            "collateralToLiquidityRate": {
                "scale": "1000",
                "liquidityPerScaleCollateral": "900"
            }
        });
        let planner = YieldRoutePlanner::new(YieldRoutePlannerConfig {
            targets: vec![source, target("reserve-c", "market-b", "PYUSD", 240)],
            min_edge_bps: 10,
            estimated_cost_lamports: 0,
        });
        let positions = vec![position("reserve-a", "USDC", 10_000, Some(100))];

        let planned = planner
            .plan_vault(
                &vault_policy(vec!["cross_mint_jupiter"], json!([{ "kind": "jupiter" }])),
                &positions,
                &StaticQuoteProvider,
            )
            .await
            .unwrap();

        assert_eq!(planned.amount_raw, 10_000);
        assert_eq!(
            planned.execution_plan["quote"]["redeem_collateral_amount"],
            10_000
        );
        assert_eq!(
            planned.execution_plan["quote"]["redeem_liquidity_amount"],
            9_000
        );
        assert_eq!(planned.execution_plan["quote"]["swap"]["amount_in"], 9_000);
    }

    #[tokio::test]
    async fn drafts_cross_mint_when_policy_has_lane_and_edge_wins() {
        let planner = YieldRoutePlanner::new(YieldRoutePlannerConfig {
            targets: vec![
                target("reserve-a", "market-a", "USDC", 100),
                target("reserve-b", "market-a", "USDC", 130),
                target("reserve-c", "market-b", "PYUSD", 240),
            ],
            min_edge_bps: 10,
            estimated_cost_lamports: 0,
        });
        let positions = vec![position("reserve-a", "USDC", 1_000, Some(100))];

        let planned = planner
            .plan_vault(
                &vault_policy(
                    vec!["same_mint", "cross_mint_jupiter"],
                    json!([{
                        "kind": "jupiter",
                        "max_slippage_bps": 100
                    }]),
                ),
                &positions,
                &StaticQuoteProvider,
            )
            .await
            .unwrap();

        assert_eq!(planned.source_reserve, "reserve-a");
        assert_eq!(planned.target_reserve, "reserve-c");
        assert_eq!(planned.source_liquidity_mint, "USDC");
        assert_eq!(planned.target_liquidity_mint, "PYUSD");
        assert_eq!(planned.execution_plan["kind"], "cross_mint");
        assert_eq!(planned.execution_plan["route"]["swap_constraint_index"], 1);
        assert_eq!(
            planned.execution_plan["route"]["deposit_constraint_index"],
            2
        );
        assert_eq!(
            planned.execution_plan["quote"]["swap"]["target_mint"],
            "PYUSD"
        );
    }

    #[tokio::test]
    async fn rejects_cross_mint_with_split_swap_policy_metadata() {
        let planner = YieldRoutePlanner::new(YieldRoutePlannerConfig {
            targets: vec![
                target("reserve-a", "market-a", "USDC", 100),
                target("reserve-c", "market-b", "PYUSD", 240),
            ],
            min_edge_bps: 10,
            estimated_cost_lamports: 0,
        });
        let positions = vec![position("reserve-a", "USDC", 1_000, Some(100))];
        let swap_policy = "swap-policy-1";

        let skip = planner
            .plan_vault(
                &vault_policy(
                    vec!["same_mint", "cross_mint_jupiter"],
                    json!([{
                        "kind": "jupiter",
                        "policy_account": swap_policy,
                        "constraint_index": 0,
                        "max_slippage_bps": 100
                    }]),
                ),
                &positions,
                &StaticQuoteProvider,
            )
            .await
            .unwrap_err();

        assert_eq!(skip, RoutePlanSkip::SplitSwapPolicyUnsupported);
    }

    #[tokio::test]
    async fn falls_back_to_same_mint_when_cross_quote_is_unavailable() {
        let planner = YieldRoutePlanner::new(YieldRoutePlannerConfig {
            targets: vec![
                target("reserve-a", "market-a", "USDC", 100),
                target("reserve-b", "market-a", "USDC", 130),
                target("reserve-c", "market-b", "PYUSD", 240),
            ],
            min_edge_bps: 10,
            estimated_cost_lamports: 0,
        });
        let positions = vec![position("reserve-a", "USDC", 1_000, Some(100))];

        let planned = planner
            .plan_vault(
                &vault_policy(
                    vec!["same_mint", "cross_mint_jupiter"],
                    json!([{ "kind": "jupiter" }]),
                ),
                &positions,
                &ConservativeRouteQuoteProvider,
            )
            .await
            .unwrap();

        assert_eq!(planned.target_reserve, "reserve-b");
        assert_eq!(planned.execution_plan["kind"], "same_mint");
    }
}
