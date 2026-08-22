use chrono::{DateTime, TimeZone, Utc};
use loyal_actions::{
    autonomous_vaults::{
        embedded_backyard_voltr_route_bundle, BackyardVoltrManagerOperation, BackyardVoltrStrategy,
        BACKYARD_VOLTR_NORMAL_OPTIMIZATION_INTERVAL_SECONDS,
    },
    USDC_MINT,
};
use loyal_yield_store::{
    fleet_orchestration::{
        RebalanceOpportunityInput, RebalanceOpportunityOperationClass, VoltrVaultPlanningState,
    },
    VaultId,
};
use serde_json::json;

use super::{
    next_voltr_leg, ConfirmedVoltrObservation, ImmutableMarketEpoch, VoltrControllerSnapshot,
    VoltrManagerOperation, VoltrNextLeg, VoltrOperationClass, VoltrPosition,
};

const VOLTR_PRIORITY_VERSION: &str = "backyard-voltr-confirmed-one-leg-v1";
const CAPACITY_INCREMENT_DIVISOR: u64 = 50;

#[derive(Clone, Copy, Debug)]
pub struct BackyardVoltrPlanningConfig {
    pub vault_id: VaultId,
    pub estimated_cost_lamports: i64,
    pub now: DateTime<Utc>,
}

#[derive(Clone, Debug)]
pub enum BackyardVoltrPlanningOutcome {
    RecoverExisting,
    Opportunity(RebalanceOpportunityInput),
    Noop,
}

#[derive(Debug, thiserror::Error)]
pub enum BackyardVoltrPlanningError {
    #[error("embedded Backyard Voltr bundle rejected: {0}")]
    Bundle(String),
    #[error("Backyard Voltr observation does not match its embedded route")]
    ObservationIdentity,
    #[error("Backyard Voltr market epoch is missing an exact, fresh four-market reserve")]
    MarketCoverage,
    #[error("Backyard Voltr amount or economics overflowed")]
    Arithmetic,
    #[error("Backyard Voltr controller rejected the confirmed state: {0:?}")]
    Controller(super::VoltrControllerError),
}

/// Translate one confirmed chain observation plus the existing immutable Earn
/// market epoch into zero or one generic fleet opportunity. No sibling graph,
/// signer, sender, or Voltr-specific durable state exists here.
pub fn plan_backyard_voltr_opportunity(
    observation: &ConfirmedVoltrObservation,
    market_epoch: &ImmutableMarketEpoch,
    durable: &VoltrVaultPlanningState,
    config: BackyardVoltrPlanningConfig,
) -> Result<BackyardVoltrPlanningOutcome, BackyardVoltrPlanningError> {
    let bundle = embedded_backyard_voltr_route_bundle()
        .map_err(|error| BackyardVoltrPlanningError::Bundle(error.to_string()))?;
    if observation.vault != bundle.vault
        || observation.configured_safety_buffer_raw != bundle.configured_idle_safety_buffer_raw
        || config.estimated_cost_lamports < 0
    {
        return Err(BackyardVoltrPlanningError::ObservationIdentity);
    }

    let mut positions = Vec::with_capacity(BackyardVoltrStrategy::ALL.len());
    let mut capacities = Vec::with_capacity(BackyardVoltrStrategy::ALL.len());
    for strategy in BackyardVoltrStrategy::ALL {
        let template = bundle.template(strategy, BackyardVoltrManagerOperation::Deposit);
        let reserve = market_epoch
            .reserves
            .iter()
            .find(|reserve| reserve.reserve == template.reserve.to_string())
            .filter(|reserve| {
                reserve.market.as_deref() == Some(template.lending_market.to_string().as_str())
                    && reserve.liquidity_mint == USDC_MINT.to_string()
                    && reserve.economic_expires_at > config.now
            })
            .ok_or(BackyardVoltrPlanningError::MarketCoverage)?;
        let observed = observation
            .positions
            .iter()
            .find(|position| position.strategy == strategy)
            .ok_or(BackyardVoltrPlanningError::MarketCoverage)?;
        let available_raw = decimal_raw_floor(&reserve.available_amount_raw)?;
        let total_supply_raw = decimal_raw_floor(&reserve.total_supply_amount_raw)?;
        let extra_capacity_raw = total_supply_raw / CAPACITY_INCREMENT_DIVISOR;
        positions.push(VoltrPosition {
            strategy,
            value_raw: observed.value_raw,
            safely_redeemable_raw: observed.value_raw.min(available_raw),
            target_raw: 0,
            net_apy_bps: reserve.supply_apy_bps,
            unwind_cost_bps: 0,
        });
        capacities.push((
            strategy,
            observed.value_raw.saturating_add(extra_capacity_raw),
            reserve.target_eligible,
        ));
    }
    assign_capacity_adjusted_targets(
        &mut positions,
        &capacities,
        observation.vault_total_value_raw,
    );

    let demand_raw = observation
        .required_idle_raw
        .checked_sub(observation.configured_safety_buffer_raw)
        .ok_or(BackyardVoltrPlanningError::Arithmetic)?;
    let snapshot = VoltrControllerSnapshot {
        context_slot: observation.context_slot,
        idle_raw: observation.idle_raw,
        safety_buffer_raw: observation.configured_safety_buffer_raw,
        active_receipt_demand_raw: demand_raw,
        receipt_set_fingerprint: observation.receipt_set_fingerprint.clone(),
        positions,
        // The adapter reads today's cap from the immutable bundle. The future
        // 50k production policy update is a separate activation authority.
        max_manager_amount_raw: bundle.max_operation_amount_raw,
        now_unix_seconds: u64::try_from(config.now.timestamp())
            .map_err(|_| BackyardVoltrPlanningError::Arithmetic)?,
        last_normal_optimization_started_at: durable
            .last_normal_optimization_started_at
            .and_then(|value| u64::try_from(value.timestamp()).ok()),
        normal_optimization_interval_seconds: BACKYARD_VOLTR_NORMAL_OPTIMIZATION_INTERVAL_SECONDS,
        has_nonterminal_signed_generation: durable.has_nonterminal_signed_generation,
    };
    let leg = match next_voltr_leg(&snapshot) {
        Ok(VoltrNextLeg::RecoverExisting) => {
            return Ok(BackyardVoltrPlanningOutcome::RecoverExisting)
        }
        Ok(VoltrNextLeg::Noop) => return Ok(BackyardVoltrPlanningOutcome::Noop),
        Ok(VoltrNextLeg::Execute(leg)) => leg,
        Err(error) => return Err(BackyardVoltrPlanningError::Controller(error)),
    };

    let strategy = leg.strategy;
    let operation = match leg.operation {
        VoltrManagerOperation::Deposit => BackyardVoltrManagerOperation::Deposit,
        VoltrManagerOperation::Withdraw => BackyardVoltrManagerOperation::Withdraw,
    };
    let template = bundle.template(strategy, operation);
    let pre_position_raw = observation
        .positions
        .iter()
        .find(|position| position.strategy == strategy)
        .map(|position| position.value_raw)
        .ok_or(BackyardVoltrPlanningError::MarketCoverage)?;
    let amount_raw =
        i64::try_from(leg.amount_raw).map_err(|_| BackyardVoltrPlanningError::Arithmetic)?;
    let (operation_class, service_deadline_at, source_apy_bps, target_apy_bps) =
        match leg.operation_class {
            VoltrOperationClass::WithdrawalRestoration => {
                let deadline = observation
                    .receipts
                    .iter()
                    .map(|receipt| receipt.withdrawable_from_ts)
                    .min()
                    .and_then(|timestamp| Utc.timestamp_opt(timestamp as i64, 0).single())
                    .ok_or(BackyardVoltrPlanningError::Arithmetic)?;
                (
                    RebalanceOpportunityOperationClass::WithdrawalRestoration,
                    Some(deadline),
                    0,
                    0,
                )
            }
            VoltrOperationClass::IdleAllocation => {
                let target = snapshot
                    .positions
                    .iter()
                    .find(|position| position.strategy == strategy)
                    .ok_or(BackyardVoltrPlanningError::MarketCoverage)?;
                if target.net_apy_bps <= 0 {
                    return Ok(BackyardVoltrPlanningOutcome::Noop);
                }
                (
                    RebalanceOpportunityOperationClass::IdleAllocation,
                    None,
                    0,
                    target.net_apy_bps,
                )
            }
            VoltrOperationClass::YieldOptimization => {
                let source = snapshot
                    .positions
                    .iter()
                    .find(|position| position.strategy == strategy)
                    .ok_or(BackyardVoltrPlanningError::MarketCoverage)?;
                let target_apy = snapshot
                    .positions
                    .iter()
                    .filter(|position| position.target_raw > position.value_raw)
                    .map(|position| position.net_apy_bps)
                    .max()
                    .unwrap_or(source.net_apy_bps);
                if target_apy <= source.net_apy_bps {
                    return Ok(BackyardVoltrPlanningOutcome::Noop);
                }
                (
                    RebalanceOpportunityOperationClass::YieldOptimization,
                    None,
                    source.net_apy_bps,
                    target_apy,
                )
            }
        };
    let edge_bps = if operation_class == RebalanceOpportunityOperationClass::WithdrawalRestoration {
        0
    } else {
        target_apy_bps - source_apy_bps
    };
    let annual_gain = if edge_bps > 0 {
        amount_raw
            .checked_mul(edge_bps)
            .and_then(|value| value.checked_div(10_000))
            .filter(|value| *value > 0)
            .ok_or(BackyardVoltrPlanningError::Arithmetic)?
    } else {
        0
    };
    let market_expires_at = market_epoch
        .route_expires_at(&USDC_MINT.to_string(), &USDC_MINT.to_string())
        .ok_or(BackyardVoltrPlanningError::MarketCoverage)?;
    let expires_at = service_deadline_at
        .filter(|deadline| *deadline > config.now)
        .map_or(market_expires_at, |deadline| {
            market_expires_at.min(deadline)
        });
    if expires_at <= config.now {
        return Err(BackyardVoltrPlanningError::MarketCoverage);
    }
    let source_reserve = (operation == BackyardVoltrManagerOperation::Withdraw)
        .then(|| template.reserve.to_string());
    let target_reserve = if operation == BackyardVoltrManagerOperation::Deposit {
        template.reserve.to_string()
    } else {
        format!("voltr_idle:{}", bundle.vault)
    };
    let requirements_fingerprint = bundle.requirements_fingerprint(strategy, operation);
    let intent_sha256 = bundle.manager_intent_sha256(
        strategy,
        operation,
        leg.amount_raw,
        observation.context_slot,
        &observation.receipt_set_fingerprint,
        &observation.protected_state_sha256,
        &observation.protected_address_set_sha256,
    );
    let execution_plan = json!({
        "kind": "voltr_kamino",
        "route_kind": "voltr_kamino",
        "route_id": bundle.route_id,
        "route_spec_sha256": bundle.route_spec_sha256,
        "route_bundle_sha256": bundle.route_bundle_sha256,
        "route_fingerprint": bundle.route_bundle_sha256,
        "requirements_fingerprint": requirements_fingerprint,
        "manager": bundle.manager.to_string(),
        "guardian": bundle.guardian.to_string(),
        "vault": bundle.vault.to_string(),
        "strategy_id": strategy.as_str(),
        "operation": operation.as_str(),
        "source_kind": if operation == BackyardVoltrManagerOperation::Deposit { "voltr_idle" } else { "voltr_strategy" },
        "target_kind": if operation == BackyardVoltrManagerOperation::Deposit { "voltr_strategy" } else { "voltr_idle" },
        "amount_raw": amount_raw,
        "max_operation_amount_raw": bundle.max_operation_amount_raw,
        "protected_context_slot": observation.context_slot,
        "receipt_set_fingerprint": observation.receipt_set_fingerprint,
        "protected_state_sha256": observation.protected_state_sha256,
        "protected_address_set_sha256": observation.protected_address_set_sha256,
        "intent_sha256": intent_sha256,
        "pre_total_value_raw": observation.vault_total_value_raw,
        "pre_idle_raw": observation.idle_raw,
        "pre_position_raw": pre_position_raw,
        "conflict_account_keys": [
            format!("voltr:vault:{}", bundle.vault),
            format!("kamino:reserve:{}", template.reserve),
        ],
    });
    Ok(BackyardVoltrPlanningOutcome::Opportunity(
        RebalanceOpportunityInput {
            cluster: bundle.cluster.clone(),
            vault_id: config.vault_id,
            source_snapshot_id: None,
            optimizer_epoch_id: market_epoch.optimizer_epoch_id,
            route_fingerprint: Some(bundle.route_bundle_sha256.clone()),
            requirements_fingerprint: Some(requirements_fingerprint),
            source_reserve,
            target_reserve,
            liquidity_mint: USDC_MINT.to_string(),
            amount_raw,
            principal_usd_micros: amount_raw,
            source_apy_bps,
            target_apy_bps,
            estimated_edge_bps: edge_bps,
            estimated_cost_lamports: config.estimated_cost_lamports,
            annual_yield_gain_usd_micros: annual_gain,
            expected_net_gain_usd_micros: annual_gain,
            economic_priority: annual_gain,
            priority_version: VOLTR_PRIORITY_VERSION.to_owned(),
            operation_class,
            service_deadline_at,
            execution_plan,
            available_at: config.now,
            expires_at,
            provisioning_request_id: None,
        },
    ))
}

fn decimal_raw_floor(value: &str) -> Result<u64, BackyardVoltrPlanningError> {
    let integer = value.split_once('.').map_or(value, |(integer, _)| integer);
    if integer.is_empty() || !integer.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err(BackyardVoltrPlanningError::MarketCoverage);
    }
    integer
        .parse()
        .map_err(|_| BackyardVoltrPlanningError::Arithmetic)
}

fn assign_capacity_adjusted_targets(
    positions: &mut [VoltrPosition],
    capacities: &[(BackyardVoltrStrategy, u64, bool)],
    total_value_raw: u64,
) {
    let mut ranked = positions
        .iter()
        .map(|position| (position.strategy, position.net_apy_bps))
        .collect::<Vec<_>>();
    ranked.sort_by_key(|(strategy, apy)| (std::cmp::Reverse(*apy), *strategy));
    let mut remaining = total_value_raw;
    for (strategy, _) in ranked {
        let capacity = capacities
            .iter()
            .find(|(candidate, _, eligible)| *candidate == strategy && *eligible)
            .map(|(_, capacity, _)| *capacity)
            .unwrap_or(0);
        let target = remaining.min(capacity);
        if let Some(position) = positions
            .iter_mut()
            .find(|position| position.strategy == strategy)
        {
            position.target_raw = target;
        }
        remaining = remaining.saturating_sub(target);
    }
    // Capacity exhaustion does not fabricate a destination. Preserve any
    // unmatched capital where it already sits so the controller emits no
    // unsupported withdrawal.
    for position in positions.iter_mut() {
        if remaining == 0 {
            break;
        }
        let preserved = remaining.min(position.value_raw.saturating_sub(position.target_raw));
        position.target_raw = position.target_raw.saturating_add(preserved);
        remaining = remaining.saturating_sub(preserved);
    }
}
