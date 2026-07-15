use super::domain::{
    evaluate_economics, CapacityBand, CapacityCurveError, EconomicPolicy, EconomicScore,
    IneligibleReason, OpportunityInput, TargetCapacityCurve,
};
use serde::{Deserialize, Serialize};
use std::{
    cmp::Ordering,
    collections::{BTreeMap, BTreeSet, BinaryHeap},
};

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RankedOpportunity {
    pub opportunity: OpportunityInput,
    pub economics: EconomicScore,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RejectedOpportunity {
    pub opportunity: OpportunityInput,
    pub reason: IneligibleReason,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RankedOpportunitySet {
    pub eligible: Vec<RankedOpportunity>,
    pub rejected: Vec<RejectedOpportunity>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WaveLimits {
    pub max_opportunities: usize,
    pub max_notional_usd_micros: i64,
    pub max_per_tenant: usize,
    pub max_per_writable_conflict_key: usize,
}

impl Default for WaveLimits {
    fn default() -> Self {
        Self {
            max_opportunities: 256,
            max_notional_usd_micros: 50_000_000_000_000,
            max_per_tenant: 64,
            max_per_writable_conflict_key: 16,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DeferredReason {
    WaveOpportunityLimit,
    WaveNotionalLimit,
    VaultLimit,
    TenantLimit,
    WritableConflictLimit,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DeferredOpportunity {
    pub opportunity: OpportunityInput,
    pub economics: EconomicScore,
    pub reason: DeferredReason,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TargetProjection {
    pub target_reserve: String,
    pub added_inflow_usd_micros: i64,
    pub final_target_net_apy_bps: Option<i64>,
    pub capacity_version: u64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WavePlan {
    /// Ordered send candidates; the first element has the greatest economic urgency.
    pub selected: Vec<RankedOpportunity>,
    pub deferred: Vec<DeferredOpportunity>,
    pub rejected: Vec<RejectedOpportunity>,
    pub target_projections: Vec<TargetProjection>,
    pub selected_notional_usd_micros: i64,
    pub selected_lost_yield_usd_micros_per_hour: i64,
    pub selected_net_holding_gain_usd_micros: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum PlannerError {
    InvalidEconomicPolicy,
    InvalidWaveLimits,
    DuplicateOpportunityId {
        opportunity_id: i64,
    },
    DuplicateTargetCurve {
        target_reserve: String,
    },
    InvalidTargetCurve {
        target_reserve: String,
        reason: CapacityCurveError,
    },
    ArithmeticOverflow,
}

pub fn rank_opportunities(
    opportunities: Vec<OpportunityInput>,
    policy: &EconomicPolicy,
) -> Result<RankedOpportunitySet, PlannerError> {
    validate_policy(policy)?;
    reject_duplicate_opportunity_ids(&opportunities)?;
    let mut eligible = Vec::with_capacity(opportunities.len());
    let mut rejected = Vec::new();
    for opportunity in opportunities {
        match evaluate_economics(&opportunity, policy, opportunity.target_net_apy_bps) {
            Ok(economics) => eligible.push(RankedOpportunity {
                opportunity,
                economics,
            }),
            Err(reason) => rejected.push(RejectedOpportunity {
                opportunity,
                reason,
            }),
        }
    }
    eligible.sort_by(|left, right| compare_ranked(right, left));
    rejected.sort_by_key(|rejected| rejected.opportunity.opportunity_id);
    Ok(RankedOpportunitySet { eligible, rejected })
}

pub fn plan_capacity_aware_wave(
    opportunities: Vec<OpportunityInput>,
    policy: &EconomicPolicy,
    curves: Vec<TargetCapacityCurve>,
    limits: &WaveLimits,
) -> Result<WavePlan, PlannerError> {
    validate_policy(policy)?;
    validate_wave_limits(limits)?;
    reject_duplicate_opportunity_ids(&opportunities)?;

    let mut targets = target_runtime_by_reserve(curves)?;
    let mut candidates = BinaryHeap::with_capacity(opportunities.len());
    let mut rejected = Vec::new();
    for opportunity in opportunities {
        let (target_apy, target_version) = initial_capacity_apy(&opportunity, &targets)?;
        let Some(target_apy) = target_apy else {
            rejected.push(RejectedOpportunity {
                opportunity,
                reason: IneligibleReason::TargetCapacityExhausted,
            });
            continue;
        };
        match evaluate_economics(
            &opportunity,
            policy,
            target_apy.min(opportunity.target_net_apy_bps),
        ) {
            Ok(economics) => candidates.push(HeapCandidate {
                ranked: RankedOpportunity {
                    opportunity,
                    economics,
                },
                target_version,
            }),
            Err(reason) => rejected.push(RejectedOpportunity {
                opportunity,
                reason,
            }),
        }
    }

    let mut selected = Vec::with_capacity(limits.max_opportunities);
    let mut deferred = Vec::new();
    let mut selected_notional = 0i64;
    let mut selected_lost_yield = 0i128;
    let mut selected_net_gain = 0i128;
    let mut tenant_counts = BTreeMap::<String, usize>::new();
    let mut conflict_counts = BTreeMap::<String, usize>::new();
    let mut selected_vaults = BTreeSet::<i64>::new();

    while let Some(mut candidate) = candidates.pop() {
        if selected.len() >= limits.max_opportunities {
            defer_candidate(
                candidate,
                DeferredReason::WaveOpportunityLimit,
                &mut deferred,
            );
            while let Some(remaining) = candidates.pop() {
                defer_candidate(
                    remaining,
                    DeferredReason::WaveOpportunityLimit,
                    &mut deferred,
                );
            }
            break;
        }

        if let Some(target) = targets.get(&candidate.ranked.opportunity.target_reserve) {
            if candidate.target_version != target.version {
                let additional_inflow = target
                    .projected_inflow_usd_micros
                    .checked_add(candidate.ranked.opportunity.notional_usd_micros)
                    .ok_or(PlannerError::ArithmeticOverflow)?;
                let target_apy =
                    target
                        .curve
                        .target_apy_after(additional_inflow)
                        .map_err(|reason| PlannerError::InvalidTargetCurve {
                            target_reserve: target.curve.target_reserve.clone(),
                            reason,
                        })?;
                let Some(target_apy) = target_apy else {
                    rejected.push(RejectedOpportunity {
                        opportunity: candidate.ranked.opportunity,
                        reason: IneligibleReason::TargetCapacityExhausted,
                    });
                    continue;
                };
                match evaluate_economics(
                    &candidate.ranked.opportunity,
                    policy,
                    target_apy.min(candidate.ranked.opportunity.target_net_apy_bps),
                ) {
                    Ok(economics) => {
                        candidate.ranked.economics = economics;
                        candidate.target_version = target.version;
                        candidates.push(candidate);
                    }
                    Err(reason) => rejected.push(RejectedOpportunity {
                        opportunity: candidate.ranked.opportunity,
                        reason,
                    }),
                }
                continue;
            }
        }

        let next_notional = selected_notional
            .checked_add(candidate.ranked.opportunity.notional_usd_micros)
            .ok_or(PlannerError::ArithmeticOverflow)?;
        if next_notional > limits.max_notional_usd_micros {
            defer_candidate(candidate, DeferredReason::WaveNotionalLimit, &mut deferred);
            continue;
        }
        if selected_vaults.contains(&candidate.ranked.opportunity.vault_id) {
            defer_candidate(candidate, DeferredReason::VaultLimit, &mut deferred);
            continue;
        }
        if tenant_counts
            .get(&candidate.ranked.opportunity.tenant_id)
            .copied()
            .unwrap_or_default()
            >= limits.max_per_tenant
        {
            defer_candidate(candidate, DeferredReason::TenantLimit, &mut deferred);
            continue;
        }
        let unique_conflict_keys = candidate
            .ranked
            .opportunity
            .writable_conflict_keys
            .iter()
            .cloned()
            .collect::<BTreeSet<_>>();
        if unique_conflict_keys.iter().any(|key| {
            conflict_counts.get(key).copied().unwrap_or_default()
                >= limits.max_per_writable_conflict_key
        }) {
            defer_candidate(
                candidate,
                DeferredReason::WritableConflictLimit,
                &mut deferred,
            );
            continue;
        }

        if let Some(target) = targets.get_mut(&candidate.ranked.opportunity.target_reserve) {
            target.projected_inflow_usd_micros = target
                .projected_inflow_usd_micros
                .checked_add(candidate.ranked.opportunity.notional_usd_micros)
                .ok_or(PlannerError::ArithmeticOverflow)?;
            target.version = target
                .version
                .checked_add(1)
                .ok_or(PlannerError::ArithmeticOverflow)?;
        }
        selected_notional = next_notional;
        selected_vaults.insert(candidate.ranked.opportunity.vault_id);
        *tenant_counts
            .entry(candidate.ranked.opportunity.tenant_id.clone())
            .or_default() += 1;
        for key in unique_conflict_keys {
            *conflict_counts.entry(key).or_default() += 1;
        }
        selected_lost_yield +=
            i128::from(candidate.ranked.economics.lost_yield_usd_micros_per_hour);
        selected_net_gain += i128::from(candidate.ranked.economics.net_holding_gain_usd_micros);
        selected.push(candidate.ranked);
    }

    rejected.sort_by_key(|rejected| rejected.opportunity.opportunity_id);
    deferred.sort_by(|left, right| {
        compare_ranked(
            &RankedOpportunity {
                opportunity: right.opportunity.clone(),
                economics: right.economics.clone(),
            },
            &RankedOpportunity {
                opportunity: left.opportunity.clone(),
                economics: left.economics.clone(),
            },
        )
    });
    let target_projections = targets
        .into_values()
        .map(|target| TargetProjection {
            final_target_net_apy_bps: target
                .curve
                .target_apy_after(target.projected_inflow_usd_micros)
                .ok()
                .flatten(),
            target_reserve: target.curve.target_reserve,
            added_inflow_usd_micros: target.projected_inflow_usd_micros,
            capacity_version: target.version,
        })
        .collect();

    Ok(WavePlan {
        selected,
        deferred,
        rejected,
        target_projections,
        selected_notional_usd_micros: selected_notional,
        selected_lost_yield_usd_micros_per_hour: clamp_i128_to_i64(selected_lost_yield),
        selected_net_holding_gain_usd_micros: clamp_i128_to_i64(selected_net_gain),
    })
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DeterministicBenchmarkFixture {
    pub seed: u64,
    pub opportunities: Vec<OpportunityInput>,
    pub capacity_curves: Vec<TargetCapacityCurve>,
    pub economic_policy: EconomicPolicy,
    pub wave_limits: WaveLimits,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DeterministicBenchmarkResult {
    pub seed: u64,
    pub input_count: usize,
    pub selected_count: usize,
    pub deferred_count: usize,
    pub rejected_count: usize,
    pub selected_notional_usd_micros: i64,
    pub selected_lost_yield_usd_micros_per_hour: i64,
    pub deterministic_digest: u64,
    pub wave: WavePlan,
}

pub fn deterministic_benchmark_fixture(
    opportunity_count: usize,
    seed: u64,
) -> DeterministicBenchmarkFixture {
    let mut random = SplitMix64::new(seed);
    let target_count = 16usize;
    let capacity_curves = (0..target_count)
        .map(|target_index| {
            let initial_apy = 850 - i64::try_from(target_index).unwrap_or_default() * 5;
            TargetCapacityCurve {
                target_reserve: format!("target-{target_index:02}"),
                already_committed_inflow_usd_micros: i64::try_from(target_index)
                    .unwrap_or_default()
                    * 25_000_000_000,
                bands: vec![
                    CapacityBand {
                        cumulative_inflow_usd_micros: 500_000_000_000,
                        target_net_apy_bps: initial_apy,
                    },
                    CapacityBand {
                        cumulative_inflow_usd_micros: 2_000_000_000_000,
                        target_net_apy_bps: initial_apy - 175,
                    },
                    CapacityBand {
                        cumulative_inflow_usd_micros: 5_000_000_000_000,
                        target_net_apy_bps: initial_apy - 350,
                    },
                    CapacityBand {
                        cumulative_inflow_usd_micros: 10_000_000_000_000,
                        target_net_apy_bps: initial_apy - 525,
                    },
                ],
            }
        })
        .collect::<Vec<_>>();
    let opportunities = (0..opportunity_count)
        .map(|index| {
            let target_index =
                usize::try_from(random.next() % target_count as u64).unwrap_or_default();
            let tenant_index = random.next() % 32;
            let mint_index = random.next() % 6;
            let source_apy = 150 + i64::try_from(random.next() % 400).unwrap_or_default();
            let target_apy = 850 - i64::try_from(target_index).unwrap_or_default() * 5;
            let notional_usd = 5 + i64::try_from(random.next() % 500_000).unwrap_or_default();
            let mut estimated_cost =
                150_000 + i64::try_from(random.next() % 850_000).unwrap_or_default();
            if index % 37 == 0 {
                estimated_cost = 100_000_000;
            }
            OpportunityInput {
                opportunity_id: i64::try_from(index).unwrap_or(i64::MAX - 1) + 1,
                optimizer_epoch_id: 1,
                vault_id: i64::try_from(index).unwrap_or(i64::MAX - 10_000) + 10_000,
                tenant_id: format!("tenant-{tenant_index:02}"),
                source_snapshot_id: i64::try_from(index).unwrap_or(i64::MAX - 1) + 1,
                observed_slot: 300_000_000 + i64::try_from(index).unwrap_or_default(),
                mint: format!("stable-{mint_index}"),
                source_reserve: format!("source-{:02}", random.next() % 32),
                target_reserve: format!("target-{target_index:02}"),
                notional_usd_micros: notional_usd * 1_000_000,
                source_net_apy_bps: source_apy,
                target_net_apy_bps: if index % 41 == 0 {
                    source_apy
                } else {
                    target_apy
                },
                confidence_ppm: 850_000
                    + u32::try_from(random.next() % 150_001).unwrap_or_default(),
                expected_service_millis: 5_000 + random.next() % 15_001,
                holding_horizon_seconds: 30 * 24 * 60 * 60,
                estimated_execution_cost_usd_micros: estimated_cost,
                age_seconds: random.next() % (60 * 60),
                fairness_credit: i64::try_from((31 - tenant_index) * 250).unwrap_or_default(),
                writable_conflict_keys: vec![
                    format!("target-lane-{target_index:02}"),
                    format!("fee-payer-shard-{}", index % 8),
                ],
            }
        })
        .collect::<Vec<_>>();
    DeterministicBenchmarkFixture {
        seed,
        opportunities,
        capacity_curves,
        economic_policy: EconomicPolicy::default(),
        wave_limits: WaveLimits::default(),
    }
}

pub fn run_deterministic_benchmark(
    opportunity_count: usize,
    seed: u64,
) -> Result<DeterministicBenchmarkResult, PlannerError> {
    let fixture = deterministic_benchmark_fixture(opportunity_count, seed);
    let input_count = fixture.opportunities.len();
    let wave = plan_capacity_aware_wave(
        fixture.opportunities,
        &fixture.economic_policy,
        fixture.capacity_curves,
        &fixture.wave_limits,
    )?;
    let deterministic_digest = wave_digest(&wave);
    Ok(DeterministicBenchmarkResult {
        seed,
        input_count,
        selected_count: wave.selected.len(),
        deferred_count: wave.deferred.len(),
        rejected_count: wave.rejected.len(),
        selected_notional_usd_micros: wave.selected_notional_usd_micros,
        selected_lost_yield_usd_micros_per_hour: wave.selected_lost_yield_usd_micros_per_hour,
        deterministic_digest,
        wave,
    })
}

fn compare_ranked(left: &RankedOpportunity, right: &RankedOpportunity) -> Ordering {
    left.economics
        .scheduling_cmp(&right.economics)
        .then_with(|| {
            left.opportunity
                .age_seconds
                .cmp(&right.opportunity.age_seconds)
        })
        .then_with(|| {
            left.opportunity
                .notional_usd_micros
                .cmp(&right.opportunity.notional_usd_micros)
        })
        .then_with(|| {
            left.opportunity
                .observed_slot
                .cmp(&right.opportunity.observed_slot)
        })
        // A lower durable id wins a complete economic tie.
        .then_with(|| {
            right
                .opportunity
                .opportunity_id
                .cmp(&left.opportunity.opportunity_id)
        })
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct HeapCandidate {
    ranked: RankedOpportunity,
    target_version: u64,
}

impl Ord for HeapCandidate {
    fn cmp(&self, other: &Self) -> Ordering {
        compare_ranked(&self.ranked, &other.ranked)
            .then_with(|| self.target_version.cmp(&other.target_version))
    }
}

impl PartialOrd for HeapCandidate {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

struct TargetRuntime {
    curve: TargetCapacityCurve,
    projected_inflow_usd_micros: i64,
    version: u64,
}

fn target_runtime_by_reserve(
    curves: Vec<TargetCapacityCurve>,
) -> Result<BTreeMap<String, TargetRuntime>, PlannerError> {
    let mut targets = BTreeMap::new();
    for curve in curves {
        curve
            .validate()
            .map_err(|reason| PlannerError::InvalidTargetCurve {
                target_reserve: curve.target_reserve.clone(),
                reason,
            })?;
        let target_reserve = curve.target_reserve.clone();
        if targets
            .insert(
                target_reserve.clone(),
                TargetRuntime {
                    curve,
                    projected_inflow_usd_micros: 0,
                    version: 0,
                },
            )
            .is_some()
        {
            return Err(PlannerError::DuplicateTargetCurve { target_reserve });
        }
    }
    Ok(targets)
}

fn initial_capacity_apy(
    opportunity: &OpportunityInput,
    targets: &BTreeMap<String, TargetRuntime>,
) -> Result<(Option<i64>, u64), PlannerError> {
    let Some(target) = targets.get(&opportunity.target_reserve) else {
        return Ok((Some(opportunity.target_net_apy_bps), 0));
    };
    let target_apy = target
        .curve
        .target_apy_after(opportunity.notional_usd_micros)
        .map_err(|reason| PlannerError::InvalidTargetCurve {
            target_reserve: target.curve.target_reserve.clone(),
            reason,
        })?;
    Ok((target_apy, target.version))
}

fn defer_candidate(
    candidate: HeapCandidate,
    reason: DeferredReason,
    deferred: &mut Vec<DeferredOpportunity>,
) {
    deferred.push(DeferredOpportunity {
        opportunity: candidate.ranked.opportunity,
        economics: candidate.ranked.economics,
        reason,
    });
}

fn validate_policy(policy: &EconomicPolicy) -> Result<(), PlannerError> {
    if policy.minimum_notional_usd_micros <= 0
        || policy.minimum_net_edge_bps <= 0
        || policy.minimum_net_gain_usd_micros < 0
        || policy.cost_safety_multiplier_bps < 10_000
        || policy.fixed_safety_margin_usd_micros < 0
        || policy.maximum_fairness_boost < 0
        || policy.starvation_deadline_seconds == 0
    {
        return Err(PlannerError::InvalidEconomicPolicy);
    }
    Ok(())
}

fn validate_wave_limits(limits: &WaveLimits) -> Result<(), PlannerError> {
    if limits.max_opportunities == 0
        || limits.max_notional_usd_micros <= 0
        || limits.max_per_tenant == 0
        || limits.max_per_writable_conflict_key == 0
    {
        return Err(PlannerError::InvalidWaveLimits);
    }
    Ok(())
}

fn reject_duplicate_opportunity_ids(
    opportunities: &[OpportunityInput],
) -> Result<(), PlannerError> {
    let mut ids = BTreeSet::new();
    for opportunity in opportunities {
        if !ids.insert(opportunity.opportunity_id) {
            return Err(PlannerError::DuplicateOpportunityId {
                opportunity_id: opportunity.opportunity_id,
            });
        }
    }
    Ok(())
}

fn wave_digest(wave: &WavePlan) -> u64 {
    let mut digest = 0xcbf29ce484222325u64;
    digest_value(&mut digest, wave.selected.len() as u64);
    digest_value(&mut digest, wave.deferred.len() as u64);
    digest_value(&mut digest, wave.rejected.len() as u64);
    for selected in &wave.selected {
        digest_value(&mut digest, selected.opportunity.opportunity_id as u64);
        digest_value(&mut digest, selected.economics.total_priority as u64);
        digest_value(
            &mut digest,
            selected.economics.capacity_adjusted_target_net_apy_bps as u64,
        );
    }
    digest
}

fn digest_value(digest: &mut u64, value: u64) {
    for byte in value.to_le_bytes() {
        *digest ^= u64::from(byte);
        *digest = digest.wrapping_mul(0x100000001b3);
    }
}

fn clamp_i128_to_i64(value: i128) -> i64 {
    value.clamp(i128::from(i64::MIN), i128::from(i64::MAX)) as i64
}

struct SplitMix64 {
    state: u64,
}

impl SplitMix64 {
    fn new(seed: u64) -> Self {
        Self { state: seed }
    }

    fn next(&mut self) -> u64 {
        self.state = self.state.wrapping_add(0x9e3779b97f4a7c15);
        let mut value = self.state;
        value = (value ^ (value >> 30)).wrapping_mul(0xbf58476d1ce4e5b9);
        value = (value ^ (value >> 27)).wrapping_mul(0x94d049bb133111eb);
        value ^ (value >> 31)
    }
}
