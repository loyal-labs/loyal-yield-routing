use serde::{Deserialize, Serialize};
use std::cmp::Ordering;

pub const BPS_DENOMINATOR: i128 = 10_000;
pub const CONFIDENCE_PPM_DENOMINATOR: i128 = 1_000_000;
pub const MILLIS_PER_SECOND: i128 = 1_000;
pub const HOURS_PER_YEAR: i128 = 8_760;
pub const SECONDS_PER_YEAR: i128 = 365 * 24 * 60 * 60;
pub const LAMPORTS_PER_SOL: i128 = 1_000_000_000;
pub const PPM_DENOMINATOR: i128 = 1_000_000;

const MAX_NOTIONAL_USD_MICROS: i64 = 9_000_000_000_000_000_000;
const MAX_ABSOLUTE_APY_BPS: i64 = 1_000_000;
const MAX_HOLDING_HORIZON_SECONDS: u64 = 10 * 365 * 24 * 60 * 60;
const MAX_EXPECTED_SERVICE_MILLIS: u64 = 24 * 60 * 60 * 1_000;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RouteFeePolicy {
    pub minimum_cap_lamports: i64,
    pub maximum_cap_lamports: i64,
    pub maximum_fraction_of_net_gain_ppm: i64,
    pub conservative_sol_price_usd_micros: i64,
}

impl Default for RouteFeePolicy {
    fn default() -> Self {
        Self {
            minimum_cap_lamports: 5_000,
            maximum_cap_lamports: 50_000,
            // The default economics gate requires at least $0.10 of net
            // holding gain. At the deliberately conservative $1,000/SOL
            // price, a 5,000-lamport base-fee floor is exactly 5% of that
            // minimum—not an implicit exception to the economic cap.
            maximum_fraction_of_net_gain_ppm: 50_000,
            conservative_sol_price_usd_micros: 1_000_000_000,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RouteFeeTier {
    Base,
    Standard,
    HighValue,
}

impl RouteFeeTier {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Base => "base",
            Self::Standard => "standard",
            Self::HighValue => "high_value",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RouteFeeBudget {
    pub cap_lamports: i64,
    pub allowed_fee_usd_micros: i64,
    pub tier: RouteFeeTier,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum RouteFeeBudgetError {
    InvalidPolicy,
    InvalidNetGain,
    FeeFloorExceedsEconomicCap {
        computed_cap_lamports: i64,
        minimum_cap_lamports: i64,
    },
}

/// Converts a durable net-yield budget into a conservative lamport ceiling.
/// The minimum transaction-fee floor is never allowed to silently exceed the
/// configured share of incremental yield; such work remains economically
/// ineligible until its expected gain grows.
pub fn route_fee_budget(
    expected_net_gain_usd_micros: i64,
    policy: RouteFeePolicy,
) -> Result<RouteFeeBudget, RouteFeeBudgetError> {
    if expected_net_gain_usd_micros <= 0 {
        return Err(RouteFeeBudgetError::InvalidNetGain);
    }
    if policy.minimum_cap_lamports <= 0
        || policy.maximum_cap_lamports < policy.minimum_cap_lamports
        || policy.maximum_fraction_of_net_gain_ppm <= 0
        || i128::from(policy.maximum_fraction_of_net_gain_ppm) > PPM_DENOMINATOR
        || policy.conservative_sol_price_usd_micros <= 0
    {
        return Err(RouteFeeBudgetError::InvalidPolicy);
    }
    let allowed_fee_usd_micros = i128::from(expected_net_gain_usd_micros)
        .saturating_mul(i128::from(policy.maximum_fraction_of_net_gain_ppm))
        / PPM_DENOMINATOR;
    let computed_cap_lamports = allowed_fee_usd_micros.saturating_mul(LAMPORTS_PER_SOL)
        / i128::from(policy.conservative_sol_price_usd_micros);
    let computed_cap_lamports = clamp_i128_to_i64(computed_cap_lamports);
    if computed_cap_lamports < policy.minimum_cap_lamports {
        return Err(RouteFeeBudgetError::FeeFloorExceedsEconomicCap {
            computed_cap_lamports,
            minimum_cap_lamports: policy.minimum_cap_lamports,
        });
    }
    let cap_lamports = computed_cap_lamports.min(policy.maximum_cap_lamports);
    let tier = if cap_lamports >= policy.maximum_cap_lamports {
        RouteFeeTier::HighValue
    } else if cap_lamports >= 15_000 {
        RouteFeeTier::Standard
    } else {
        RouteFeeTier::Base
    };
    Ok(RouteFeeBudget {
        cap_lamports,
        allowed_fee_usd_micros: clamp_i128_to_i64(allowed_fee_usd_micros),
        tier,
    })
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EconomicPolicy {
    pub minimum_notional_usd_micros: i64,
    pub minimum_net_edge_bps: i64,
    pub minimum_net_gain_usd_micros: i64,
    pub cost_safety_multiplier_bps: u32,
    pub fixed_safety_margin_usd_micros: i64,
    pub age_boost_bps_per_hour: u32,
    pub maximum_age_boost_bps: u32,
    pub maximum_fairness_boost: i64,
    pub starvation_deadline_seconds: u64,
}

impl Default for EconomicPolicy {
    fn default() -> Self {
        Self {
            minimum_notional_usd_micros: 1_000_000,
            minimum_net_edge_bps: 1,
            minimum_net_gain_usd_micros: 100_000,
            cost_safety_multiplier_bps: 12_500,
            fixed_safety_margin_usd_micros: 50_000,
            age_boost_bps_per_hour: 250,
            maximum_age_boost_bps: 10_000,
            maximum_fairness_boost: 1_000_000,
            starvation_deadline_seconds: 15 * 60,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct OpportunityInput {
    pub opportunity_id: i64,
    pub optimizer_epoch_id: i64,
    pub vault_id: i64,
    pub tenant_id: String,
    pub source_snapshot_id: i64,
    pub observed_slot: i64,
    pub mint: String,
    pub source_reserve: String,
    pub target_reserve: String,
    pub notional_usd_micros: i64,
    pub source_net_apy_bps: i64,
    pub target_net_apy_bps: i64,
    pub confidence_ppm: u32,
    pub expected_service_millis: u64,
    pub holding_horizon_seconds: u64,
    pub estimated_execution_cost_usd_micros: i64,
    pub age_seconds: u64,
    /// Credit supplied by the tenant fairness controller in priority units.
    pub fairness_credit: i64,
    /// Exact writable-account identities, including the chosen fee payer.
    pub writable_conflict_keys: Vec<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum IneligibleReason {
    InvalidIdentity,
    InvalidNotional,
    InvalidApy,
    InvalidConfidence,
    InvalidExpectedServiceTime,
    InvalidHoldingHorizon,
    InvalidCost,
    BelowMinimumNotional,
    NonPositiveEdge,
    BelowMinimumEdge,
    ExpectedGainDoesNotCoverCost {
        expected_gain_usd_micros: i64,
        guarded_cost_usd_micros: i64,
    },
    BelowMinimumNetGain {
        net_gain_usd_micros: i64,
        minimum_net_gain_usd_micros: i64,
    },
    TargetCapacityExhausted,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EconomicScore {
    pub capacity_adjusted_target_net_apy_bps: i64,
    pub capacity_adjusted_net_edge_bps: i64,
    pub lost_yield_usd_micros_per_hour: i64,
    pub gross_holding_gain_usd_micros: i64,
    pub expected_holding_gain_usd_micros: i64,
    pub guarded_cost_usd_micros: i64,
    pub net_holding_gain_usd_micros: i64,
    pub service_adjusted_priority: i64,
    pub age_boost: i64,
    pub fairness_boost: i64,
    pub total_priority: i64,
    pub starved: bool,
}

impl EconomicScore {
    pub fn scheduling_cmp(&self, other: &Self) -> Ordering {
        // Starvation is represented by the bounded age contribution already
        // included in total_priority. Treating it as a categorical first key
        // lets an old dust route jump every fresh high-value movement.
        self.total_priority
            .cmp(&other.total_priority)
            .then_with(|| {
                self.lost_yield_usd_micros_per_hour
                    .cmp(&other.lost_yield_usd_micros_per_hour)
            })
            .then_with(|| {
                self.net_holding_gain_usd_micros
                    .cmp(&other.net_holding_gain_usd_micros)
            })
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CapacityBand {
    /// Inclusive cumulative inflow ceiling, including already committed flow.
    pub cumulative_inflow_usd_micros: i64,
    pub target_net_apy_bps: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TargetCapacityCurve {
    pub target_reserve: String,
    pub already_committed_inflow_usd_micros: i64,
    pub bands: Vec<CapacityBand>,
}

impl TargetCapacityCurve {
    pub fn validate(&self) -> Result<(), CapacityCurveError> {
        if self.target_reserve.is_empty()
            || self.already_committed_inflow_usd_micros < 0
            || self.bands.is_empty()
        {
            return Err(CapacityCurveError::InvalidShape);
        }
        let mut previous_ceiling = 0i64;
        let mut previous_apy = i64::MAX;
        for band in &self.bands {
            if band.cumulative_inflow_usd_micros <= previous_ceiling
                || !apy_is_bounded(band.target_net_apy_bps)
                || band.target_net_apy_bps > previous_apy
            {
                return Err(CapacityCurveError::InvalidShape);
            }
            previous_ceiling = band.cumulative_inflow_usd_micros;
            previous_apy = band.target_net_apy_bps;
        }
        if self.already_committed_inflow_usd_micros > previous_ceiling {
            return Err(CapacityCurveError::CommittedFlowExceedsCapacity);
        }
        Ok(())
    }

    pub fn target_apy_after(
        &self,
        additional_inflow_usd_micros: i64,
    ) -> Result<Option<i64>, CapacityCurveError> {
        self.validate()?;
        if additional_inflow_usd_micros < 0 {
            return Err(CapacityCurveError::InvalidShape);
        }
        let cumulative = self
            .already_committed_inflow_usd_micros
            .checked_add(additional_inflow_usd_micros)
            .ok_or(CapacityCurveError::ArithmeticOverflow)?;
        Ok(self
            .bands
            .iter()
            .find(|band| cumulative <= band.cumulative_inflow_usd_micros)
            .map(|band| band.target_net_apy_bps))
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CapacityCurveError {
    InvalidShape,
    CommittedFlowExceedsCapacity,
    ArithmeticOverflow,
}

pub fn evaluate_economics(
    input: &OpportunityInput,
    policy: &EconomicPolicy,
    capacity_adjusted_target_net_apy_bps: i64,
) -> Result<EconomicScore, IneligibleReason> {
    validate_input(input)?;
    if input.notional_usd_micros < policy.minimum_notional_usd_micros {
        return Err(IneligibleReason::BelowMinimumNotional);
    }
    if !apy_is_bounded(capacity_adjusted_target_net_apy_bps) {
        return Err(IneligibleReason::InvalidApy);
    }

    let edge_bps = capacity_adjusted_target_net_apy_bps - input.source_net_apy_bps;
    if edge_bps <= 0 {
        return Err(IneligibleReason::NonPositiveEdge);
    }
    if edge_bps < policy.minimum_net_edge_bps {
        return Err(IneligibleReason::BelowMinimumEdge);
    }

    let notional = i128::from(input.notional_usd_micros);
    let edge = i128::from(edge_bps);
    let confidence = i128::from(input.confidence_ppm);
    let gross_holding_gain = notional * edge * i128::from(input.holding_horizon_seconds)
        / (BPS_DENOMINATOR * SECONDS_PER_YEAR);
    let expected_holding_gain = gross_holding_gain * confidence / CONFIDENCE_PPM_DENOMINATOR;
    let guarded_variable_cost = i128::from(input.estimated_execution_cost_usd_micros)
        * i128::from(policy.cost_safety_multiplier_bps)
        / BPS_DENOMINATOR;
    let guarded_cost = guarded_variable_cost + i128::from(policy.fixed_safety_margin_usd_micros);
    if expected_holding_gain <= guarded_cost {
        return Err(IneligibleReason::ExpectedGainDoesNotCoverCost {
            expected_gain_usd_micros: clamp_i128_to_i64(expected_holding_gain),
            guarded_cost_usd_micros: clamp_i128_to_i64(guarded_cost),
        });
    }
    let net_holding_gain = expected_holding_gain - guarded_cost;
    if net_holding_gain < i128::from(policy.minimum_net_gain_usd_micros) {
        return Err(IneligibleReason::BelowMinimumNetGain {
            net_gain_usd_micros: clamp_i128_to_i64(net_holding_gain),
            minimum_net_gain_usd_micros: policy.minimum_net_gain_usd_micros,
        });
    }

    let lost_yield_per_hour = notional * edge / (BPS_DENOMINATOR * HOURS_PER_YEAR);
    let service_adjusted_priority = lost_yield_per_hour * confidence * MILLIS_PER_SECOND
        / (CONFIDENCE_PPM_DENOMINATOR * i128::from(input.expected_service_millis));
    let age_boost_bps = (u128::from(input.age_seconds) * u128::from(policy.age_boost_bps_per_hour)
        / 3_600)
        .min(u128::from(policy.maximum_age_boost_bps));
    let age_boost = service_adjusted_priority * age_boost_bps as i128 / BPS_DENOMINATOR;
    let fairness_boost = input
        .fairness_credit
        .max(0)
        .min(policy.maximum_fairness_boost);
    let total_priority = service_adjusted_priority
        .max(1)
        .saturating_add(age_boost)
        .saturating_add(i128::from(fairness_boost));

    Ok(EconomicScore {
        capacity_adjusted_target_net_apy_bps,
        capacity_adjusted_net_edge_bps: edge_bps,
        lost_yield_usd_micros_per_hour: clamp_i128_to_i64(lost_yield_per_hour),
        gross_holding_gain_usd_micros: clamp_i128_to_i64(gross_holding_gain),
        expected_holding_gain_usd_micros: clamp_i128_to_i64(expected_holding_gain),
        guarded_cost_usd_micros: clamp_i128_to_i64(guarded_cost),
        net_holding_gain_usd_micros: clamp_i128_to_i64(net_holding_gain),
        service_adjusted_priority: clamp_i128_to_i64(service_adjusted_priority.max(1)),
        age_boost: clamp_i128_to_i64(age_boost),
        fairness_boost,
        total_priority: clamp_i128_to_i64(total_priority),
        starved: input.age_seconds >= policy.starvation_deadline_seconds,
    })
}

/// Re-evaluates an already planned route against one fresh immutable market
/// snapshot. The planner's capacity haircut remains durable policy evidence,
/// while current source/target APYs determine whether the move is still worth
/// executing and how much fee budget it may consume.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FreshRouteEconomicsInput {
    pub opportunity: OpportunityInput,
    pub durable_observed_target_apy_bps: i64,
    pub durable_capacity_adjusted_target_apy_bps: i64,
    pub current_source_apy_bps: i64,
    pub current_observed_target_apy_bps: i64,
    pub economic_policy: EconomicPolicy,
    pub fee_policy: RouteFeePolicy,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FreshRouteEconomics {
    pub capacity_haircut_bps: i64,
    pub current_capacity_adjusted_target_apy_bps: i64,
    pub score: EconomicScore,
    pub fee_budget: RouteFeeBudget,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum FreshRouteEconomicsError {
    InvalidCapacityEvidence,
    ArithmeticOverflow,
    EconomicallyIneligible { reason: IneligibleReason },
    FeeBudgetIneligible { reason: RouteFeeBudgetError },
}

pub fn evaluate_fresh_route_economics(
    mut input: FreshRouteEconomicsInput,
) -> Result<FreshRouteEconomics, FreshRouteEconomicsError> {
    let capacity_haircut_bps = input
        .durable_observed_target_apy_bps
        .checked_sub(input.durable_capacity_adjusted_target_apy_bps)
        .filter(|haircut| *haircut >= 0)
        .ok_or(FreshRouteEconomicsError::InvalidCapacityEvidence)?;
    let current_capacity_adjusted_target_apy_bps = input
        .current_observed_target_apy_bps
        .checked_sub(capacity_haircut_bps)
        .ok_or(FreshRouteEconomicsError::ArithmeticOverflow)?;
    input.opportunity.source_net_apy_bps = input.current_source_apy_bps;
    input.opportunity.target_net_apy_bps = input.current_observed_target_apy_bps;
    let score = evaluate_economics(
        &input.opportunity,
        &input.economic_policy,
        current_capacity_adjusted_target_apy_bps,
    )
    .map_err(|reason| FreshRouteEconomicsError::EconomicallyIneligible { reason })?;
    let fee_budget = route_fee_budget(score.net_holding_gain_usd_micros, input.fee_policy)
        .map_err(|reason| FreshRouteEconomicsError::FeeBudgetIneligible { reason })?;
    Ok(FreshRouteEconomics {
        capacity_haircut_bps,
        current_capacity_adjusted_target_apy_bps,
        score,
        fee_budget,
    })
}

fn validate_input(input: &OpportunityInput) -> Result<(), IneligibleReason> {
    if input.opportunity_id <= 0
        || input.optimizer_epoch_id <= 0
        || input.vault_id <= 0
        || input.source_snapshot_id <= 0
        || input.observed_slot <= 0
        || input.tenant_id.is_empty()
        || input.mint.is_empty()
        || input.source_reserve.is_empty()
        || input.target_reserve.is_empty()
        || input.source_reserve == input.target_reserve
    {
        return Err(IneligibleReason::InvalidIdentity);
    }
    if input.notional_usd_micros <= 0 || input.notional_usd_micros > MAX_NOTIONAL_USD_MICROS {
        return Err(IneligibleReason::InvalidNotional);
    }
    if !apy_is_bounded(input.source_net_apy_bps) || !apy_is_bounded(input.target_net_apy_bps) {
        return Err(IneligibleReason::InvalidApy);
    }
    if input.confidence_ppm == 0 || i128::from(input.confidence_ppm) > CONFIDENCE_PPM_DENOMINATOR {
        return Err(IneligibleReason::InvalidConfidence);
    }
    if input.expected_service_millis == 0
        || input.expected_service_millis > MAX_EXPECTED_SERVICE_MILLIS
    {
        return Err(IneligibleReason::InvalidExpectedServiceTime);
    }
    if input.holding_horizon_seconds == 0
        || input.holding_horizon_seconds > MAX_HOLDING_HORIZON_SECONDS
    {
        return Err(IneligibleReason::InvalidHoldingHorizon);
    }
    if input.estimated_execution_cost_usd_micros < 0 {
        return Err(IneligibleReason::InvalidCost);
    }
    Ok(())
}

fn clamp_i128_to_i64(value: i128) -> i64 {
    value.clamp(i128::from(i64::MIN), i128::from(i64::MAX)) as i64
}

fn apy_is_bounded(apy_bps: i64) -> bool {
    (-MAX_ABSOLUTE_APY_BPS..=MAX_ABSOLUTE_APY_BPS).contains(&apy_bps)
}
