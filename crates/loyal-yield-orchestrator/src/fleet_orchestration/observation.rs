use super::domain::OpportunityInput;
use crate::{route_amount_evidence_from_metadata, NeonSqlClient, ACTIVE_DECISION_STATUSES};
use chrono::{DateTime, Duration, Utc};
use loyal_actions::USDC_MINT;
use loyal_yield_router::timescale::{
    SupportedReserveLatestQuery, SupportedReserveLatestRow, TimescaleRouterClient,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use thiserror::Error;

const SAME_MINT_ROUTE_MODE: &str = "same_mint_kamino";
const USD_MICROS_PER_USD: i64 = 1_000_000;
/// A one-millidollar stablecoin price bucket (roughly 10 bps near $1) avoids
/// turning harmless oracle ticks into full-fleet planning work while still
/// waking promptly on economically material depegs.
pub const MARKET_WAKE_PRICE_BUCKET_USD_MICROS: i64 = 1_000;
/// A target whose observed supply moved by less than ten basis points remains
/// inside the last authoritative fleet frontier. The scoped planner still
/// uses the new exact supply and every durable inflight reservation; this
/// tolerance only avoids rescanning unrelated vault source rows for ordinary
/// reserve churn. Drift accumulates against the last full sweep, so it cannot
/// hide a sequence of individually-small changes forever.
pub const MARKET_MATERIAL_CAPACITY_DRIFT_PPM: i64 = 1_000;

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct StablecoinValuation {
    pub mint: String,
    pub decimals: u8,
    /// USD micro-dollars per whole token. This must be supplied explicitly.
    pub price_usd_micros: i64,
    pub confidence_ppm: u32,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetObservationConfig {
    pub cluster: String,
    pub enabled_mints: Vec<String>,
    pub stablecoin_valuations: Vec<StablecoinValuation>,
    pub risk_baskets: Vec<String>,
    pub minimum_reserve_supply_usd: f64,
    pub minimum_supply_apy: f64,
    pub maximum_supply_apy: f64,
    pub maximum_market_age_seconds: i64,
    pub rebalance_cooldown_seconds: i64,
    pub holding_horizon_seconds: u64,
    pub expected_reserve_move_service_millis: u64,
    pub expected_idle_deposit_service_millis: u64,
    pub estimated_reserve_move_cost_usd_micros: i64,
    pub estimated_idle_deposit_cost_usd_micros: i64,
    /// Optional additive credits keyed by policy authority (the current tenant identity).
    pub tenant_fairness_credits: BTreeMap<String, i64>,
}

impl Default for FleetObservationConfig {
    fn default() -> Self {
        Self {
            cluster: "mainnet-beta".to_owned(),
            enabled_mints: Vec::new(),
            stablecoin_valuations: Vec::new(),
            risk_baskets: vec!["safe".to_owned()],
            minimum_reserve_supply_usd: 100_000.0,
            minimum_supply_apy: 0.0,
            maximum_supply_apy: 0.5,
            maximum_market_age_seconds: 5 * 60,
            rebalance_cooldown_seconds: 5 * 60,
            holding_horizon_seconds: 30 * 24 * 60 * 60,
            expected_reserve_move_service_millis: 15_000,
            expected_idle_deposit_service_millis: 15_000,
            estimated_reserve_move_cost_usd_micros: 500_000,
            estimated_idle_deposit_cost_usd_micros: 500_000,
            tenant_fairness_credits: BTreeMap::new(),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MarketEpochReserve {
    pub reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
    pub mint_decimals: u8,
    pub market_price_usd_micros: i64,
    pub observed_at: DateTime<Utc>,
    pub slot: i64,
    pub supply_apy_bps: i64,
    pub total_supply_usd_micros: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ImmutableMarketEpoch {
    pub optimizer_epoch_id: i64,
    pub fingerprint: String,
    pub captured_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
    pub oldest_market_observed_at: Option<DateTime<Utc>>,
    pub newest_market_observed_at: Option<DateTime<Utc>>,
    pub minimum_market_slot: Option<i64>,
    pub maximum_market_slot: Option<i64>,
    pub reserves: Vec<MarketEpochReserve>,
}

impl ImmutableMarketEpoch {
    /// Returns the canonical evidence persisted for this market snapshot.
    ///
    /// `captured_at` is the planner's wall-clock read time and therefore changes
    /// when the same Timescale snapshot is read again. Durable optimizer epochs
    /// instead pin that field to the newest source observation, which is already
    /// covered by the fingerprint. Repeated reads of unchanged market data then
    /// produce byte-for-byte equivalent immutable evidence.
    pub fn durable_optimizer_epoch_evidence(&self) -> Self {
        let mut evidence = self.clone();
        evidence.captured_at = self.newest_market_observed_at.unwrap_or(self.captured_at);
        evidence
    }

    /// Compact diagnostic signature for the five-second market probe. Exact
    /// source slot/time do not justify scanning the fleet. Capacity is checked
    /// separately against the last full sweep with a relative tolerance, so
    /// this hash is never used as the scoped-admission fence by itself.
    pub fn material_frontier_fingerprint(&self) -> String {
        let mut hasher = Sha256::new();
        for reserve in &self.reserves {
            hash_part(&mut hasher, reserve.reserve.as_bytes());
            hash_part(
                &mut hasher,
                reserve.market.as_deref().unwrap_or_default().as_bytes(),
            );
            hash_part(&mut hasher, reserve.liquidity_mint.as_bytes());
            hash_part(&mut hasher, &[reserve.mint_decimals]);
            hash_part(
                &mut hasher,
                &reserve
                    .market_price_usd_micros
                    .div_euclid(MARKET_WAKE_PRICE_BUCKET_USD_MICROS)
                    .to_le_bytes(),
            );
            hash_part(&mut hasher, &reserve.supply_apy_bps.to_le_bytes());
        }
        format!("{:x}", hasher.finalize())
    }

    /// Extracts the low-churn economic frontier that may safely gate a scoped
    /// dirty-vault pass. Source observation slots/timestamps and exact supply
    /// values remain in the immutable optimizer epoch used by each admitted
    /// route, but are compared here by economic significance rather than byte
    /// equality.
    pub fn material_market_frontier(&self) -> MaterialMarketFrontier {
        MaterialMarketFrontier {
            reserves: self
                .reserves
                .iter()
                .map(|reserve| MaterialMarketFrontierReserve {
                    reserve: reserve.reserve.clone(),
                    market: reserve.market.clone(),
                    liquidity_mint: reserve.liquidity_mint.clone(),
                    mint_decimals: reserve.mint_decimals,
                    market_price_usd_micros: reserve.market_price_usd_micros,
                    supply_apy_bps: reserve.supply_apy_bps,
                    total_supply_usd_micros: reserve.total_supply_usd_micros,
                })
                .collect(),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MaterialMarketFrontierReserve {
    pub reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
    pub mint_decimals: u8,
    pub market_price_usd_micros: i64,
    pub supply_apy_bps: i64,
    pub total_supply_usd_micros: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MaterialMarketFrontier {
    pub reserves: Vec<MaterialMarketFrontierReserve>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MaterialFrontierDisposition {
    ReuseScopedFrontier,
    FullSweepReserveTopologyChanged,
    FullSweepMarketPriceChanged,
    FullSweepSupplyApyChanged,
    FullSweepTargetCapacityChanged,
}

impl MaterialFrontierDisposition {
    pub fn allows_scoped_planning(self) -> bool {
        self == Self::ReuseScopedFrontier
    }
}

impl MaterialMarketFrontier {
    /// Compares current market data to the last complete full-fleet frontier.
    ///
    /// Any reserve topology or integer-basis-point APY change can create a new
    /// best target for vaults outside the dirty cohort, so it forces a sweep.
    /// Price and target capacity use explicit bounded tolerances. The exact
    /// current values are still used for scoped economics and durable route
    /// evidence when reuse is allowed.
    pub fn disposition_against(
        &self,
        current: &MaterialMarketFrontier,
    ) -> MaterialFrontierDisposition {
        if self.reserves.len() != current.reserves.len() {
            return MaterialFrontierDisposition::FullSweepReserveTopologyChanged;
        }
        for (baseline, latest) in self.reserves.iter().zip(&current.reserves) {
            if baseline.reserve != latest.reserve
                || baseline.market != latest.market
                || baseline.liquidity_mint != latest.liquidity_mint
                || baseline.mint_decimals != latest.mint_decimals
            {
                return MaterialFrontierDisposition::FullSweepReserveTopologyChanged;
            }
            if i128::from(baseline.market_price_usd_micros)
                .saturating_sub(i128::from(latest.market_price_usd_micros))
                .abs()
                >= i128::from(MARKET_WAKE_PRICE_BUCKET_USD_MICROS)
            {
                return MaterialFrontierDisposition::FullSweepMarketPriceChanged;
            }
            if baseline.supply_apy_bps != latest.supply_apy_bps {
                return MaterialFrontierDisposition::FullSweepSupplyApyChanged;
            }
            if material_capacity_changed(
                baseline.total_supply_usd_micros,
                latest.total_supply_usd_micros,
            ) {
                return MaterialFrontierDisposition::FullSweepTargetCapacityChanged;
            }
        }
        MaterialFrontierDisposition::ReuseScopedFrontier
    }
}

fn material_capacity_changed(baseline: i64, current: i64) -> bool {
    if baseline == current {
        return false;
    }
    if baseline <= 0 || current <= 0 {
        return true;
    }
    let absolute_drift = i128::from(baseline)
        .saturating_sub(i128::from(current))
        .abs();
    let reference = i128::from(baseline);
    absolute_drift.saturating_mul(1_000_000)
        >= reference.saturating_mul(i128::from(MARKET_MATERIAL_CAPACITY_DRIFT_PPM))
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MaterialFrontierDeterministicEvidence {
    pub exact_epoch_changed_under_harmless_churn: bool,
    pub harmless_churn_disposition: MaterialFrontierDisposition,
    pub material_apy_change_disposition: MaterialFrontierDisposition,
    pub material_capacity_change_disposition: MaterialFrontierDisposition,
    pub material_topology_change_disposition: MaterialFrontierDisposition,
    pub nonmaterial_capacity_drift_ppm: i64,
    pub material_capacity_drift_ppm: i64,
}

/// Deterministic verifier fixture for the dirty-cohort gate. It deliberately
/// changes high-churn epoch fields and exact supply while preserving the
/// economic frontier, then proves that APY, capacity, and topology changes
/// independently force an authoritative sweep.
pub fn material_frontier_deterministic_evidence() -> MaterialFrontierDeterministicEvidence {
    let observed_at = DateTime::<Utc>::from_timestamp(1_752_000_000, 0)
        .expect("fixed material-frontier fixture timestamp must be valid");
    let mut baseline = ImmutableMarketEpoch {
        optimizer_epoch_id: 1,
        fingerprint: String::new(),
        captured_at: observed_at,
        expires_at: observed_at + Duration::minutes(5),
        oldest_market_observed_at: Some(observed_at),
        newest_market_observed_at: Some(observed_at),
        minimum_market_slot: Some(100),
        maximum_market_slot: Some(100),
        reserves: vec![MarketEpochReserve {
            reserve: "reserve-a".to_owned(),
            market: Some("market-a".to_owned()),
            liquidity_mint: "USDC".to_owned(),
            mint_decimals: 6,
            market_price_usd_micros: USD_MICROS_PER_USD,
            observed_at,
            slot: 100,
            supply_apy_bps: 500,
            total_supply_usd_micros: 1_000_000_000_000,
        }],
    };
    baseline.fingerprint = market_epoch_fingerprint(&baseline.reserves, &["USDC".to_owned()]);
    let mut harmless = baseline.clone();
    harmless.optimizer_epoch_id = 2;
    harmless.captured_at += Duration::seconds(15);
    harmless.expires_at += Duration::seconds(15);
    harmless.oldest_market_observed_at = Some(observed_at + Duration::seconds(15));
    harmless.newest_market_observed_at = harmless.oldest_market_observed_at;
    harmless.minimum_market_slot = Some(250);
    harmless.maximum_market_slot = Some(250);
    harmless.reserves[0].observed_at += Duration::seconds(15);
    harmless.reserves[0].slot = 250;
    harmless.reserves[0].total_supply_usd_micros = 1_000_500_000_000;
    harmless.fingerprint = market_epoch_fingerprint(&harmless.reserves, &["USDC".to_owned()]);

    let mut material_apy = harmless.clone();
    material_apy.reserves[0].supply_apy_bps += 1;
    let mut material_capacity = harmless.clone();
    material_capacity.reserves[0].total_supply_usd_micros = 1_002_000_000_000;
    let mut material_topology = harmless.clone();
    material_topology.reserves[0].reserve = "reserve-b".to_owned();

    let frontier = baseline.material_market_frontier();
    MaterialFrontierDeterministicEvidence {
        exact_epoch_changed_under_harmless_churn: baseline.fingerprint != harmless.fingerprint
            && baseline.maximum_market_slot != harmless.maximum_market_slot
            && baseline.newest_market_observed_at != harmless.newest_market_observed_at
            && baseline.reserves[0].total_supply_usd_micros
                != harmless.reserves[0].total_supply_usd_micros,
        harmless_churn_disposition: frontier
            .disposition_against(&harmless.material_market_frontier()),
        material_apy_change_disposition: frontier
            .disposition_against(&material_apy.material_market_frontier()),
        material_capacity_change_disposition: frontier
            .disposition_against(&material_capacity.material_market_frontier()),
        material_topology_change_disposition: frontier
            .disposition_against(&material_topology.material_market_frontier()),
        nonmaterial_capacity_drift_ppm: 500,
        material_capacity_drift_ppm: 2_000,
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ObservedSourceKind {
    ReservePosition,
    IdleVaultUsdc,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ObservedFleetOpportunity {
    pub economics: OpportunityInput,
    pub source_kind: ObservedSourceKind,
    pub policy_id: i64,
    pub settings: String,
    pub vault_index: i16,
    pub vault_pubkey: String,
    pub amount_raw: i64,
    pub route_amount_semantics: String,
    pub source_amount_semantics: Option<String>,
    pub source_collateral_amount_raw: Option<i64>,
    pub redeemable_source_liquidity_amount_raw: Option<i64>,
    pub idle_vault_liquidity_amount_raw: Option<i64>,
    /// Present only for router-owned idle liquidity. The executor revalidates
    /// this exact ATA instead of deriving an unversioned source from a balance.
    pub idle_token_account: Option<String>,
    pub source_observed_slot: i64,
    pub source_observed_at: DateTime<Utc>,
    pub target_observed_at: DateTime<Utc>,
    pub target_observed_slot: i64,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetObservationStats {
    pub market_read_count: u32,
    pub neon_read_count: u32,
    pub rpc_read_count: u32,
    pub child_process_count: u32,
    /// Authoritative denominator: every active managed vault whose active
    /// policy is eligible for this planner/cluster/mint universe. This count
    /// is taken before active-movement and source-availability exclusions.
    pub eligible_vault_count: i64,
    /// Eligible vaults with at least one positive, policy-compatible current
    /// source row after active-movement exclusions.
    pub source_candidate_vault_count: i64,
    /// Eligible vaults that produced at least one positive-edge route
    /// candidate from the immutable market epoch.
    pub opportunity_vault_count: i64,
    pub active_opportunity_vaults_excluded: i64,
    pub active_opportunity_vaults_excluded_by_state: BTreeMap<String, i64>,
    pub no_positive_current_source_vault_count: i64,
    /// Mutually exclusive terminal outcome for every eligible vault. Values
    /// must sum exactly to `eligible_vault_count`.
    pub vault_outcomes_by_reason: BTreeMap<String, i64>,
    pub accounted_vault_count: i64,
    pub complete_vault_accounting: bool,
    pub committed_target_inflow_reserve_count: usize,
    pub committed_target_inflow_usd_micros: i64,
    pub valued_position_source_count: usize,
    pub idle_usdc_source_count: usize,
    pub missing_valuation_source_count: usize,
    pub unsupported_amount_semantics_count: usize,
    pub unsupported_market_semantics_source_count: usize,
    pub missing_target_count: usize,
    pub opportunity_count: usize,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetObservationResult {
    pub market_epoch: ImmutableMarketEpoch,
    pub opportunities: Vec<ObservedFleetOpportunity>,
    pub committed_target_inflows: Vec<CommittedTargetInflow>,
    pub stats: FleetObservationStats,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CommittedTargetInflow {
    pub target_reserve: String,
    pub principal_usd_micros: i64,
}

impl FleetObservationResult {
    pub fn economic_inputs(&self) -> Vec<OpportunityInput> {
        self.opportunities
            .iter()
            .map(|opportunity| opportunity.economics.clone())
            .collect()
    }
}

#[derive(Debug, Error)]
pub enum FleetObservationError {
    #[error("invalid fleet observation configuration: {0}")]
    InvalidConfig(String),
    #[error("market observation query failed: {0}")]
    MarketRead(#[source] crate::sqlx::Error),
    #[error("fleet observation query failed: {0}")]
    NeonRead(#[source] crate::sqlx::Error),
    #[error("fleet observation row decode failed: {0}")]
    RowDecode(#[source] serde_json::Error),
    #[error("fleet observation completeness invariant failed: {0}")]
    CompletenessInvariant(String),
    #[error("fixed-point observation arithmetic overflowed")]
    ArithmeticOverflow,
}

pub async fn observe_fleet_opportunities(
    neon: &NeonSqlClient,
    timescale: &TimescaleRouterClient,
    delegated_signer: &str,
    config: &FleetObservationConfig,
) -> Result<FleetObservationResult, FleetObservationError> {
    observe_fleet_opportunities_at(neon, timescale, delegated_signer, config, Utc::now()).await
}

/// Reads only the compact market frontier used to decide whether a new
/// authoritative fleet sweep is necessary. This deliberately avoids the Neon
/// fleet/source query so a short market-change feedback loop does not become a
/// repeated full-fleet scan.
pub async fn observe_market_epoch(
    timescale: &TimescaleRouterClient,
    config: &FleetObservationConfig,
) -> Result<ImmutableMarketEpoch, FleetObservationError> {
    let enabled_mints = config
        .enabled_mints
        .iter()
        .cloned()
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect::<Vec<_>>();
    load_market_epoch(timescale, config, &enabled_mints, Utc::now()).await
}

/// Read-only cutover evidence for databases that have not applied the complete
/// durable fleet schema through migration 26. Its Neon query intentionally
/// never names queue or target-capacity tables, so PostgreSQL can parse and run
/// it against the migration-22 schema. No result from this path may be
/// published because committed inflows are unavailable.
pub async fn observe_fleet_opportunities_without_queue_schema(
    neon: &NeonSqlClient,
    timescale: &TimescaleRouterClient,
    delegated_signer: &str,
    config: &FleetObservationConfig,
) -> Result<FleetObservationResult, FleetObservationError> {
    let captured_at = Utc::now();
    let validated = ValidatedConfig::new(config, delegated_signer)?;
    let market_epoch =
        load_market_epoch(timescale, config, &validated.enabled_mints, captured_at).await?;
    let source_set = load_fleet_sources_without_queue_schema(
        neon,
        delegated_signer,
        &validated.enabled_mints,
        config.rebalance_cooldown_seconds,
        captured_at,
    )
    .await?;
    build_observation_result(market_epoch, source_set, &validated, config)
}

/// Observes one durable dirty cohort while retaining a fleet-wide committed
/// inflow aggregate. This reduces source work from O(fleet) to O(cohort), but
/// callers must still prove that reusing the previous global ranking frontier
/// is safe before publishing the scoped result.
pub async fn observe_fleet_opportunities_for_vaults(
    neon: &NeonSqlClient,
    timescale: &TimescaleRouterClient,
    delegated_signer: &str,
    config: &FleetObservationConfig,
    vault_ids: &[i64],
) -> Result<FleetObservationResult, FleetObservationError> {
    if vault_ids.is_empty()
        || vault_ids.iter().any(|vault_id| *vault_id <= 0)
        || vault_ids.iter().copied().collect::<BTreeSet<_>>().len() != vault_ids.len()
    {
        return Err(FleetObservationError::InvalidConfig(
            "scoped fleet observation requires unique positive vault ids".to_owned(),
        ));
    }
    observe_fleet_opportunities_at_scope(
        neon,
        timescale,
        delegated_signer,
        config,
        Utc::now(),
        Some(vault_ids),
    )
    .await
}

/// Executes exactly one Timescale market read and one set-based Neon read.
pub async fn observe_fleet_opportunities_at(
    neon: &NeonSqlClient,
    timescale: &TimescaleRouterClient,
    delegated_signer: &str,
    config: &FleetObservationConfig,
    captured_at: DateTime<Utc>,
) -> Result<FleetObservationResult, FleetObservationError> {
    observe_fleet_opportunities_at_scope(
        neon,
        timescale,
        delegated_signer,
        config,
        captured_at,
        None,
    )
    .await
}

async fn observe_fleet_opportunities_at_scope(
    neon: &NeonSqlClient,
    timescale: &TimescaleRouterClient,
    delegated_signer: &str,
    config: &FleetObservationConfig,
    captured_at: DateTime<Utc>,
    vault_ids: Option<&[i64]>,
) -> Result<FleetObservationResult, FleetObservationError> {
    let validated = ValidatedConfig::new(config, delegated_signer)?;
    let market_epoch =
        load_market_epoch(timescale, config, &validated.enabled_mints, captured_at).await?;
    let source_set = load_fleet_sources(
        neon,
        &config.cluster,
        delegated_signer,
        &validated.enabled_mints,
        config.rebalance_cooldown_seconds,
        captured_at,
        vault_ids,
    )
    .await?;
    build_observation_result(market_epoch, source_set, &validated, config)
}

async fn load_market_epoch(
    timescale: &TimescaleRouterClient,
    config: &FleetObservationConfig,
    enabled_mints: &[String],
    captured_at: DateTime<Utc>,
) -> Result<ImmutableMarketEpoch, FleetObservationError> {
    let latest = timescale
        .latest_supported_reserves(SupportedReserveLatestQuery {
            risk_baskets: config.risk_baskets.clone(),
            liquidity_mint: None,
            markets: Vec::new(),
            min_supply_usd: Some(config.minimum_reserve_supply_usd),
            min_supply_apy: Some(config.minimum_supply_apy),
            max_supply_apy: Some(config.maximum_supply_apy),
            stale: Some(false),
            limit: None,
        })
        .await
        .map_err(FleetObservationError::MarketRead)?;
    build_market_epoch(
        latest,
        enabled_mints,
        captured_at,
        config.maximum_market_age_seconds,
    )
}

pub fn stablecoin_raw_to_usd_micros(
    amount_raw: i64,
    valuation: &StablecoinValuation,
) -> Result<i64, FleetObservationError> {
    if amount_raw <= 0 || valuation.price_usd_micros <= 0 || valuation.decimals > 18 {
        return Err(FleetObservationError::InvalidConfig(
            "stablecoin amount, price, and decimals must be positive and bounded".to_owned(),
        ));
    }
    let raw_units_per_token = 10_i128
        .checked_pow(u32::from(valuation.decimals))
        .ok_or(FleetObservationError::ArithmeticOverflow)?;
    let value = i128::from(amount_raw)
        .checked_mul(i128::from(valuation.price_usd_micros))
        .ok_or(FleetObservationError::ArithmeticOverflow)?
        / raw_units_per_token;
    i64::try_from(value).map_err(|_| FleetObservationError::ArithmeticOverflow)
}

struct ValidatedConfig {
    enabled_mints: Vec<String>,
    valuations: BTreeMap<String, StablecoinValuation>,
}

impl ValidatedConfig {
    fn new(
        config: &FleetObservationConfig,
        delegated_signer: &str,
    ) -> Result<Self, FleetObservationError> {
        if delegated_signer.is_empty()
            || config.cluster.trim().is_empty()
            || config.enabled_mints.is_empty()
            || config.risk_baskets.is_empty()
            || !config.minimum_reserve_supply_usd.is_finite()
            || !config.minimum_supply_apy.is_finite()
            || !config.maximum_supply_apy.is_finite()
            || config.minimum_reserve_supply_usd < 0.0
            || config.minimum_supply_apy < 0.0
            || config.maximum_supply_apy <= config.minimum_supply_apy
            || config.maximum_market_age_seconds <= 0
            || config.rebalance_cooldown_seconds < 0
            || config.holding_horizon_seconds == 0
            || config.expected_reserve_move_service_millis == 0
            || config.expected_idle_deposit_service_millis == 0
            || config.estimated_reserve_move_cost_usd_micros < 0
            || config.estimated_idle_deposit_cost_usd_micros < 0
        {
            return Err(FleetObservationError::InvalidConfig(
                "invalid signer, mint universe, market bounds, cooldown, horizon, service time, or cost"
                    .to_owned(),
            ));
        }
        let enabled_mints = config
            .enabled_mints
            .iter()
            .cloned()
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect::<Vec<_>>();
        let mut valuations = BTreeMap::new();
        for valuation in &config.stablecoin_valuations {
            if valuation.mint.is_empty()
                || valuation.decimals > 18
                || valuation.price_usd_micros <= 0
                || valuation.confidence_ppm == 0
                || valuation.confidence_ppm > 1_000_000
                || valuations
                    .insert(valuation.mint.clone(), valuation.clone())
                    .is_some()
            {
                return Err(FleetObservationError::InvalidConfig(
                    "stablecoin valuations must be unique, positive, and confidence-bounded"
                        .to_owned(),
                ));
            }
        }
        Ok(Self {
            enabled_mints,
            valuations,
        })
    }
}

fn build_market_epoch(
    latest: Vec<SupportedReserveLatestRow>,
    enabled_mints: &[String],
    captured_at: DateTime<Utc>,
    maximum_market_age_seconds: i64,
) -> Result<ImmutableMarketEpoch, FleetObservationError> {
    let enabled = enabled_mints
        .iter()
        .map(String::as_str)
        .collect::<BTreeSet<_>>();
    let cutoff = captured_at - Duration::seconds(maximum_market_age_seconds);
    let mut reserves = latest
        .into_iter()
        .filter(|reserve| {
            enabled.contains(reserve.liquidity_mint.as_str())
                && reserve.observed_at >= cutoff
                && reserve.supply_apy.is_finite()
                && reserve.total_supply_usd_estimate.is_finite()
                && reserve.total_supply_usd_estimate >= 0.0
                && reserve.market_price_usd.is_finite()
                && reserve.market_price_usd > 0.0
                && (0..=18).contains(&reserve.mint_decimals)
        })
        .map(|reserve| {
            Ok(MarketEpochReserve {
                reserve: reserve.reserve,
                market: reserve.market,
                liquidity_mint: reserve.liquidity_mint,
                mint_decimals: u8::try_from(reserve.mint_decimals)
                    .map_err(|_| FleetObservationError::ArithmeticOverflow)?,
                market_price_usd_micros: usd_to_micros(reserve.market_price_usd)?,
                observed_at: reserve.observed_at,
                slot: reserve.slot,
                supply_apy_bps: apy_to_bps(reserve.supply_apy)?,
                total_supply_usd_micros: usd_to_micros(reserve.total_supply_usd_estimate)?,
            })
        })
        .collect::<Result<Vec<_>, FleetObservationError>>()?;
    reserves.sort_by(|left, right| {
        left.liquidity_mint
            .cmp(&right.liquidity_mint)
            .then_with(|| left.reserve.cmp(&right.reserve))
            .then_with(|| left.market.cmp(&right.market))
    });
    let fingerprint = market_epoch_fingerprint(&reserves, enabled_mints);
    let optimizer_epoch_id = positive_epoch_id(&fingerprint);
    let oldest_market_observed_at = reserves.iter().map(|reserve| reserve.observed_at).min();
    let expires_at = oldest_market_observed_at
        .map(|observed_at| observed_at + Duration::seconds(maximum_market_age_seconds))
        .unwrap_or(captured_at);
    Ok(ImmutableMarketEpoch {
        optimizer_epoch_id,
        fingerprint,
        captured_at,
        expires_at,
        oldest_market_observed_at,
        newest_market_observed_at: reserves.iter().map(|reserve| reserve.observed_at).max(),
        minimum_market_slot: reserves.iter().map(|reserve| reserve.slot).min(),
        maximum_market_slot: reserves.iter().map(|reserve| reserve.slot).max(),
        reserves,
    })
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct FleetSourceRow {
    vault_id: i64,
    settings: String,
    vault_index: i16,
    vault_pubkey: String,
    policy_id: i64,
    policy_authority: String,
    policy_markets: Vec<String>,
    policy_stable_mints: Vec<String>,
    policy_liquidity_mints: Vec<String>,
    source_kind: String,
    source_reserve: Option<String>,
    liquidity_mint: String,
    amount_raw: i64,
    source_snapshot_id: Option<i64>,
    idle_token_account: Option<String>,
    observed_slot: i64,
    observed_at: DateTime<Utc>,
    planning_metadata: Value,
}

struct FleetSourceSet {
    eligible_vault_count: i64,
    source_candidate_vault_count: i64,
    active_opportunity_vaults_excluded: i64,
    active_opportunity_vaults_excluded_by_state: BTreeMap<String, i64>,
    no_positive_current_source_vault_count: i64,
    sources: Vec<FleetSourceRow>,
    committed_target_inflows: Vec<CommittedTargetInflow>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum SourceVaultRejection {
    MissingValuation,
    UnsupportedAmountSemantics,
    UnsupportedMarketSemantics,
    NoEconomicTarget,
}

impl SourceVaultRejection {
    fn outcome_key(self) -> &'static str {
        match self {
            Self::MissingValuation => "missing_valuation",
            Self::UnsupportedAmountSemantics => "unsupported_amount_semantics",
            Self::UnsupportedMarketSemantics => "unsupported_market_semantics",
            Self::NoEconomicTarget => "no_economic_target",
        }
    }

    /// Prefer the reason reached furthest through the observation pipeline
    /// when a vault has multiple source rows but none yields an opportunity.
    fn precedence(self) -> u8 {
        match self {
            Self::MissingValuation => 0,
            Self::UnsupportedAmountSemantics => 1,
            Self::UnsupportedMarketSemantics => 2,
            Self::NoEconomicTarget => 3,
        }
    }
}

fn record_source_vault_rejection(
    rejections: &mut BTreeMap<i64, SourceVaultRejection>,
    vault_id: i64,
    rejection: SourceVaultRejection,
) {
    rejections
        .entry(vault_id)
        .and_modify(|current| {
            if rejection.precedence() > current.precedence() {
                *current = rejection;
            }
        })
        .or_insert(rejection);
}

async fn load_fleet_sources(
    neon: &NeonSqlClient,
    cluster: &str,
    delegated_signer: &str,
    enabled_mints: &[String],
    rebalance_cooldown_seconds: i64,
    captured_at: DateTime<Utc>,
    vault_ids: Option<&[i64]>,
) -> Result<FleetSourceSet, FleetObservationError> {
    let active_statuses = ACTIVE_DECISION_STATUSES
        .iter()
        .map(|status| (*status).to_owned())
        .collect::<Vec<_>>();
    let row = crate::sqlx::query(
        r#"
        WITH active_opportunities AS (
            SELECT id, vault_id, target_reserve, liquidity_mint,
                   principal_usd_micros, opportunity_state, lease_kind
            FROM loyal_yield.rebalance_opportunities
            WHERE cluster = $8
              AND opportunity_state IN (
                'waiting_alt', 'revalidate', 'ready', 'leased', 'decision_created'
            )
              AND (
                  expires_at > $7::TIMESTAMPTZ
                  OR opportunity_state IN ('leased', 'decision_created')
              )
        ),
        live_capacity_reservations AS (
            -- Execution admission is authoritative even after the queue row
            -- becomes terminal. In particular, reconciled flow remains here
            -- as awaiting_telemetry until a strictly newer target observation
            -- proves that market supply reflects the movement.
            SELECT opportunity_id, target_reserve, liquidity_mint,
                   principal_usd_micros
            FROM loyal_yield.target_capacity_reservations
            WHERE cluster = $8
              AND reservation_state <> 'released'
        ),
        committed_target_inflows AS (
            -- Count every durable reservation exactly once and never subtract
            -- it merely because its vault belongs to a scoped dirty cohort.
            SELECT target_reserve, principal_usd_micros
            FROM live_capacity_reservations

            UNION ALL

            -- Pre-execution intent has not acquired an execution-time
            -- reservation yet, but must still consume projected planner
            -- headroom. Scoped replacement removes only its own replaceable
            -- waiting/revalidate/ready intents. Leased/decision-backed work is
            -- already executing and remains committed even before the narrow
            -- reservation handoff completes.
            SELECT opportunity.target_reserve,
                   opportunity.principal_usd_micros
            FROM active_opportunities opportunity
            LEFT JOIN live_capacity_reservations reservation
              ON reservation.opportunity_id = opportunity.id
            WHERE reservation.opportunity_id IS NULL
              AND (
                  opportunity.opportunity_state IN ('leased', 'decision_created')
                  OR (
                      opportunity.opportunity_state IN (
                          'waiting_alt', 'revalidate', 'ready'
                      )
                      AND (
                          $9::BIGINT[] IS NULL
                          OR NOT (opportunity.vault_id = ANY($9::BIGINT[]))
                      )
                  )
              )
        ),
        eligible_vaults AS (
            SELECT
                v.id AS vault_id,
                v.settings,
                v.vault_index,
                v.vault_pubkey,
                p.id AS policy_id,
                p.authority AS policy_authority,
                p.kamino_markets AS policy_markets,
                p.stable_mints AS policy_stable_mints,
                p.kamino_liquidity_mints AS policy_liquidity_mints
            FROM loyal_yield.managed_vaults v
            JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
            WHERE v.active = TRUE
              AND p.active = TRUE
              AND ($9::BIGINT[] IS NULL OR v.id = ANY($9::BIGINT[]))
              AND $1 = ANY(p.delegated_signers)
              AND $3 = ANY(p.route_modes)
              AND p.stable_mints && $2::TEXT[]
              AND p.kamino_liquidity_mints && $2::TEXT[]
              AND cardinality(p.kamino_markets) > 0
        ),
        active_queue_exclusions AS (
            -- Assign one deterministic queue state to each excluded vault so
            -- the state breakdown remains a true partition of the fleet.
            -- Full sweeps drain around all active opportunities. Scoped
            -- dirty/coverage passes may replace pre-execution states but
            -- never a leased or decision-backed movement.
            SELECT DISTINCT ON (opportunity.vault_id)
                   opportunity.vault_id,
                   opportunity.opportunity_state
            FROM active_opportunities opportunity
            JOIN eligible_vaults eligible
              ON eligible.vault_id = opportunity.vault_id
            WHERE $9::BIGINT[] IS NULL
               OR opportunity.opportunity_state IN ('leased', 'decision_created')
            ORDER BY opportunity.vault_id,
                     CASE opportunity.opportunity_state
                         WHEN 'decision_created' THEN 0
                         WHEN 'leased' THEN 1
                         WHEN 'ready' THEN 2
                         WHEN 'revalidate' THEN 3
                         WHEN 'waiting_alt' THEN 4
                         ELSE 5
                     END
        ),
        excluded_active_vaults AS (
            SELECT vault_id, opportunity_state
            FROM active_queue_exclusions

            UNION ALL

            -- A legacy/in-flight active decision without a queue row is still
            -- part of the denominator and receives its own explicit outcome.
            SELECT eligible.vault_id, 'active_decision'::TEXT
            FROM eligible_vaults eligible
            WHERE NOT EXISTS (
                      SELECT 1 FROM active_queue_exclusions queued
                      WHERE queued.vault_id = eligible.vault_id
                  )
              AND EXISTS (
                  SELECT 1 FROM loyal_yield.rebalance_decisions decision
                  WHERE decision.vault_id = eligible.vault_id
                    AND decision.status::TEXT = ANY($4::TEXT[])
              )
        ),
        planning_vaults AS (
            SELECT eligible.*
            FROM eligible_vaults eligible
            WHERE NOT EXISTS (
                SELECT 1 FROM excluded_active_vaults excluded
                WHERE excluded.vault_id = eligible.vault_id
            )
        ),
        sources AS (
            SELECT
                eligible.*,
                'reserve_position'::TEXT AS source_kind,
                position.reserve AS source_reserve,
                position.market AS source_market,
                position.liquidity_mint,
                position.amount_raw,
                position.supply_apy_bps,
                position.snapshot_id AS source_snapshot_id,
                NULL::TEXT AS idle_token_account,
                position.observed_slot,
                position.observed_at,
                position.planning_metadata
            FROM planning_vaults eligible
            JOIN loyal_yield.vault_reserve_positions_current position
              ON position.vault_id = eligible.vault_id
            WHERE position.has_value = TRUE
              AND position.amount_raw > 0
              AND position.liquidity_mint = ANY($2::TEXT[])
              AND position.liquidity_mint = ANY(eligible.policy_stable_mints)
              AND position.liquidity_mint = ANY(eligible.policy_liquidity_mints)
              AND (position.market IS NULL OR position.market = ANY(eligible.policy_markets))
              AND (
                  $5::BIGINT = 0 OR NOT EXISTS (
                      SELECT 1 FROM loyal_yield.rebalance_decisions recent
                      WHERE recent.vault_id = eligible.vault_id
                        AND recent.status::TEXT = 'confirmed'
                        AND recent.source_reserve = position.reserve
                        AND recent.updated_at >= $7::TIMESTAMPTZ - ($5::DOUBLE PRECISION * INTERVAL '1 second')
                  )
              )

            UNION ALL

            SELECT
                eligible.*,
                'idle_vault_usdc'::TEXT AS source_kind,
                NULL::TEXT AS source_reserve,
                NULL::TEXT AS source_market,
                idle.mint AS liquidity_mint,
                idle.amount_raw,
                0::BIGINT AS supply_apy_bps,
                NULL::BIGINT AS source_snapshot_id,
                idle.token_account AS idle_token_account,
                idle.observed_slot,
                idle.observed_at,
                '{}'::JSONB AS planning_metadata
            FROM planning_vaults eligible
            JOIN loyal_yield.vault_idle_token_balances_current idle
              ON idle.vault_id = eligible.vault_id
            WHERE idle.amount_raw > 0
              AND idle.mint = $6
              AND idle.mint = ANY($2::TEXT[])
              AND idle.mint = ANY(eligible.policy_stable_mints)
              AND idle.mint = ANY(eligible.policy_liquidity_mints)
              AND (
                  $5::BIGINT = 0 OR NOT EXISTS (
                      SELECT 1 FROM loyal_yield.rebalance_decisions recent
                      WHERE recent.vault_id = eligible.vault_id
                        AND recent.status::TEXT = 'confirmed'
                        AND recent.source_reserve IS NULL
                        AND recent.liquidity_mint = idle.mint
                        AND recent.updated_at >= $7::TIMESTAMPTZ - ($5::DOUBLE PRECISION * INTERVAL '1 second')
                  )
              )
        )
        SELECT
            (SELECT count(*)::BIGINT FROM eligible_vaults)
                AS eligible_vault_count,
            (SELECT count(DISTINCT source.vault_id)::BIGINT FROM sources source)
                AS source_candidate_vault_count,
            (SELECT count(*)::BIGINT FROM excluded_active_vaults)
                AS active_opportunity_vaults_excluded,
            (
                SELECT count(*)::BIGINT
                FROM planning_vaults planning
                WHERE NOT EXISTS (
                    SELECT 1 FROM sources source
                    WHERE source.vault_id = planning.vault_id
                )
            ) AS no_positive_current_source_vault_count,
            COALESCE((
                SELECT jsonb_object_agg(
                    excluded_state.opportunity_state,
                    excluded_state.vault_count
                    ORDER BY excluded_state.opportunity_state
                )
                FROM (
                    SELECT excluded.opportunity_state,
                           count(*)::BIGINT AS vault_count
                    FROM excluded_active_vaults excluded
                    GROUP BY excluded.opportunity_state
                ) excluded_state
            ), '{}'::JSONB) AS active_opportunity_vaults_excluded_by_state,
            COALESCE(
                (SELECT jsonb_agg(
                    jsonb_build_object(
                        'vaultId', source.vault_id,
                        'settings', source.settings,
                        'vaultIndex', source.vault_index,
                        'vaultPubkey', source.vault_pubkey,
                        'policyId', source.policy_id,
                        'policyAuthority', source.policy_authority,
                        'policyMarkets', source.policy_markets,
                        'policyStableMints', source.policy_stable_mints,
                        'policyLiquidityMints', source.policy_liquidity_mints,
                        'sourceKind', source.source_kind,
                        'sourceReserve', source.source_reserve,
                        'liquidityMint', source.liquidity_mint,
                        'amountRaw', source.amount_raw,
                        'sourceSnapshotId', source.source_snapshot_id,
                        'idleTokenAccount', source.idle_token_account,
                        'observedSlot', source.observed_slot,
                        'observedAt', source.observed_at,
                        'planningMetadata', source.planning_metadata
                    )
                    ORDER BY source.vault_id, source.source_kind, source.source_reserve, source.liquidity_mint
                ) FROM sources source),
                '[]'::JSONB
            ) AS sources
            , COALESCE(
                (SELECT jsonb_agg(
                    jsonb_build_object(
                        'targetReserve', committed.target_reserve,
                        'principalUsdMicros', committed.principal_usd_micros
                    ) ORDER BY committed.target_reserve
                )
                FROM (
                    SELECT target_reserve, sum(principal_usd_micros)::BIGINT AS principal_usd_micros
                    FROM committed_target_inflows
                    GROUP BY target_reserve
                ) committed),
                '[]'::JSONB
            ) AS committed_target_inflows
        "#,
    )
    .bind(delegated_signer)
    .bind(enabled_mints)
    .bind(SAME_MINT_ROUTE_MODE)
    .bind(active_statuses)
    .bind(rebalance_cooldown_seconds)
    .bind(USDC_MINT.to_string())
    .bind(captured_at)
    .bind(cluster)
    .bind(vault_ids.map(|ids| ids.to_vec()))
    .fetch_one(neon.pool())
    .await
    .map_err(FleetObservationError::NeonRead)?;
    use crate::sqlx::Row;
    let eligible_vault_count = row
        .try_get("eligible_vault_count")
        .map_err(FleetObservationError::NeonRead)?;
    let source_candidate_vault_count = row
        .try_get("source_candidate_vault_count")
        .map_err(FleetObservationError::NeonRead)?;
    let active_opportunity_vaults_excluded = row
        .try_get("active_opportunity_vaults_excluded")
        .map_err(FleetObservationError::NeonRead)?;
    let no_positive_current_source_vault_count = row
        .try_get("no_positive_current_source_vault_count")
        .map_err(FleetObservationError::NeonRead)?;
    let active_opportunity_vaults_excluded_by_state_json: Value = row
        .try_get("active_opportunity_vaults_excluded_by_state")
        .map_err(FleetObservationError::NeonRead)?;
    let active_opportunity_vaults_excluded_by_state =
        serde_json::from_value(active_opportunity_vaults_excluded_by_state_json)
            .map_err(FleetObservationError::RowDecode)?;
    let sources_json: Value = row
        .try_get("sources")
        .map_err(FleetObservationError::NeonRead)?;
    let sources = serde_json::from_value(sources_json).map_err(FleetObservationError::RowDecode)?;
    let committed_target_inflows_json: Value = row
        .try_get("committed_target_inflows")
        .map_err(FleetObservationError::NeonRead)?;
    let committed_target_inflows = serde_json::from_value(committed_target_inflows_json)
        .map_err(FleetObservationError::RowDecode)?;
    Ok(FleetSourceSet {
        eligible_vault_count,
        source_candidate_vault_count,
        active_opportunity_vaults_excluded,
        active_opportunity_vaults_excluded_by_state,
        no_positive_current_source_vault_count,
        sources,
        committed_target_inflows,
    })
}

async fn load_fleet_sources_without_queue_schema(
    neon: &NeonSqlClient,
    delegated_signer: &str,
    enabled_mints: &[String],
    rebalance_cooldown_seconds: i64,
    captured_at: DateTime<Utc>,
) -> Result<FleetSourceSet, FleetObservationError> {
    let active_statuses = ACTIVE_DECISION_STATUSES
        .iter()
        .map(|status| (*status).to_owned())
        .collect::<Vec<_>>();
    let row = crate::sqlx::query(
        r#"
        WITH eligible_vaults AS (
            SELECT
                v.id AS vault_id,
                v.settings,
                v.vault_index,
                v.vault_pubkey,
                p.id AS policy_id,
                p.authority AS policy_authority,
                p.kamino_markets AS policy_markets,
                p.stable_mints AS policy_stable_mints,
                p.kamino_liquidity_mints AS policy_liquidity_mints
            FROM loyal_yield.managed_vaults v
            JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
            WHERE v.active = TRUE
              AND p.active = TRUE
              AND $1 = ANY(p.delegated_signers)
              AND $3 = ANY(p.route_modes)
              AND p.stable_mints && $2::TEXT[]
              AND p.kamino_liquidity_mints && $2::TEXT[]
              AND cardinality(p.kamino_markets) > 0
        ),
        excluded_active_vaults AS (
            SELECT eligible.vault_id, 'active_decision'::TEXT AS opportunity_state
            FROM eligible_vaults eligible
            WHERE EXISTS (
                SELECT 1 FROM loyal_yield.rebalance_decisions decision
                WHERE decision.vault_id = eligible.vault_id
                  AND decision.status::TEXT = ANY($4::TEXT[])
            )
        ),
        planning_vaults AS (
            SELECT eligible.*
            FROM eligible_vaults eligible
            WHERE NOT EXISTS (
                SELECT 1 FROM excluded_active_vaults excluded
                WHERE excluded.vault_id = eligible.vault_id
            )
        ),
        sources AS (
            SELECT
                eligible.*,
                'reserve_position'::TEXT AS source_kind,
                position.reserve AS source_reserve,
                position.market AS source_market,
                position.liquidity_mint,
                position.amount_raw,
                position.supply_apy_bps,
                position.snapshot_id AS source_snapshot_id,
                NULL::TEXT AS idle_token_account,
                position.observed_slot,
                position.observed_at,
                position.planning_metadata
            FROM planning_vaults eligible
            JOIN loyal_yield.vault_reserve_positions_current position
              ON position.vault_id = eligible.vault_id
            WHERE position.has_value = TRUE
              AND position.amount_raw > 0
              AND position.liquidity_mint = ANY($2::TEXT[])
              AND position.liquidity_mint = ANY(eligible.policy_stable_mints)
              AND position.liquidity_mint = ANY(eligible.policy_liquidity_mints)
              AND (position.market IS NULL OR position.market = ANY(eligible.policy_markets))
              AND (
                  $5::BIGINT = 0 OR NOT EXISTS (
                      SELECT 1 FROM loyal_yield.rebalance_decisions recent
                      WHERE recent.vault_id = eligible.vault_id
                        AND recent.status::TEXT = 'confirmed'
                        AND recent.source_reserve = position.reserve
                        AND recent.updated_at >= $7::TIMESTAMPTZ - ($5::DOUBLE PRECISION * INTERVAL '1 second')
                  )
              )

            UNION ALL

            SELECT
                eligible.*,
                'idle_vault_usdc'::TEXT AS source_kind,
                NULL::TEXT AS source_reserve,
                NULL::TEXT AS source_market,
                idle.mint AS liquidity_mint,
                idle.amount_raw,
                0::BIGINT AS supply_apy_bps,
                NULL::BIGINT AS source_snapshot_id,
                idle.token_account AS idle_token_account,
                idle.observed_slot,
                idle.observed_at,
                '{}'::JSONB AS planning_metadata
            FROM planning_vaults eligible
            JOIN loyal_yield.vault_idle_token_balances_current idle
              ON idle.vault_id = eligible.vault_id
            WHERE idle.amount_raw > 0
              AND idle.mint = $6
              AND idle.mint = ANY($2::TEXT[])
              AND idle.mint = ANY(eligible.policy_stable_mints)
              AND idle.mint = ANY(eligible.policy_liquidity_mints)
              AND (
                  $5::BIGINT = 0 OR NOT EXISTS (
                      SELECT 1 FROM loyal_yield.rebalance_decisions recent
                      WHERE recent.vault_id = eligible.vault_id
                        AND recent.status::TEXT = 'confirmed'
                        AND recent.source_reserve IS NULL
                        AND recent.liquidity_mint = idle.mint
                        AND recent.updated_at >= $7::TIMESTAMPTZ - ($5::DOUBLE PRECISION * INTERVAL '1 second')
                  )
              )
        )
        SELECT
            (SELECT count(*)::BIGINT FROM eligible_vaults)
                AS eligible_vault_count,
            (SELECT count(DISTINCT source.vault_id)::BIGINT FROM sources source)
                AS source_candidate_vault_count,
            (SELECT count(*)::BIGINT FROM excluded_active_vaults)
                AS active_opportunity_vaults_excluded,
            (
                SELECT count(*)::BIGINT
                FROM planning_vaults planning
                WHERE NOT EXISTS (
                    SELECT 1 FROM sources source
                    WHERE source.vault_id = planning.vault_id
                )
            ) AS no_positive_current_source_vault_count,
            COALESCE((
                SELECT jsonb_object_agg(
                    excluded_state.opportunity_state,
                    excluded_state.vault_count
                )
                FROM (
                    SELECT excluded.opportunity_state,
                           count(*)::BIGINT AS vault_count
                    FROM excluded_active_vaults excluded
                    GROUP BY excluded.opportunity_state
                ) excluded_state
            ), '{}'::JSONB) AS active_opportunity_vaults_excluded_by_state,
            COALESCE(
                (SELECT jsonb_agg(
                    jsonb_build_object(
                        'vaultId', source.vault_id,
                        'settings', source.settings,
                        'vaultIndex', source.vault_index,
                        'vaultPubkey', source.vault_pubkey,
                        'policyId', source.policy_id,
                        'policyAuthority', source.policy_authority,
                        'policyMarkets', source.policy_markets,
                        'policyStableMints', source.policy_stable_mints,
                        'policyLiquidityMints', source.policy_liquidity_mints,
                        'sourceKind', source.source_kind,
                        'sourceReserve', source.source_reserve,
                        'liquidityMint', source.liquidity_mint,
                        'amountRaw', source.amount_raw,
                        'sourceSnapshotId', source.source_snapshot_id,
                        'idleTokenAccount', source.idle_token_account,
                        'observedSlot', source.observed_slot,
                        'observedAt', source.observed_at,
                        'planningMetadata', source.planning_metadata
                    )
                    ORDER BY source.vault_id, source.source_kind, source.source_reserve, source.liquidity_mint
                ) FROM sources source),
                '[]'::JSONB
            ) AS sources,
            '[]'::JSONB AS committed_target_inflows
        "#,
    )
    .bind(delegated_signer)
    .bind(enabled_mints)
    .bind(SAME_MINT_ROUTE_MODE)
    .bind(active_statuses)
    .bind(rebalance_cooldown_seconds)
    .bind(USDC_MINT.to_string())
    .bind(captured_at)
    .fetch_one(neon.pool())
    .await
    .map_err(FleetObservationError::NeonRead)?;
    use crate::sqlx::Row;
    let eligible_vault_count = row
        .try_get("eligible_vault_count")
        .map_err(FleetObservationError::NeonRead)?;
    let source_candidate_vault_count = row
        .try_get("source_candidate_vault_count")
        .map_err(FleetObservationError::NeonRead)?;
    let active_opportunity_vaults_excluded = row
        .try_get("active_opportunity_vaults_excluded")
        .map_err(FleetObservationError::NeonRead)?;
    let no_positive_current_source_vault_count = row
        .try_get("no_positive_current_source_vault_count")
        .map_err(FleetObservationError::NeonRead)?;
    let active_opportunity_vaults_excluded_by_state_json: Value = row
        .try_get("active_opportunity_vaults_excluded_by_state")
        .map_err(FleetObservationError::NeonRead)?;
    let active_opportunity_vaults_excluded_by_state =
        serde_json::from_value(active_opportunity_vaults_excluded_by_state_json)
            .map_err(FleetObservationError::RowDecode)?;
    let sources_json: Value = row
        .try_get("sources")
        .map_err(FleetObservationError::NeonRead)?;
    let sources = serde_json::from_value(sources_json).map_err(FleetObservationError::RowDecode)?;
    Ok(FleetSourceSet {
        eligible_vault_count,
        source_candidate_vault_count,
        active_opportunity_vaults_excluded,
        active_opportunity_vaults_excluded_by_state,
        no_positive_current_source_vault_count,
        sources,
        committed_target_inflows: Vec::new(),
    })
}

fn build_observation_result(
    market_epoch: ImmutableMarketEpoch,
    source_set: FleetSourceSet,
    validated: &ValidatedConfig,
    config: &FleetObservationConfig,
) -> Result<FleetObservationResult, FleetObservationError> {
    let mut by_mint = BTreeMap::<&str, Vec<&MarketEpochReserve>>::new();
    for reserve in &market_epoch.reserves {
        by_mint
            .entry(reserve.liquidity_mint.as_str())
            .or_default()
            .push(reserve);
    }
    let valuations = epoch_valuations(&market_epoch, &validated.valuations)?;
    let source_vault_ids = source_set
        .sources
        .iter()
        .map(|source| source.vault_id)
        .collect::<BTreeSet<_>>();
    let source_candidate_vault_count = i64::try_from(source_vault_ids.len())
        .map_err(|_| FleetObservationError::ArithmeticOverflow)?;
    let active_exclusion_breakdown_count = source_set
        .active_opportunity_vaults_excluded_by_state
        .values()
        .try_fold(0i64, |total, count| total.checked_add(*count))
        .ok_or(FleetObservationError::ArithmeticOverflow)?;
    let partitioned_before_epoch = source_candidate_vault_count
        .checked_add(source_set.active_opportunity_vaults_excluded)
        .and_then(|count| count.checked_add(source_set.no_positive_current_source_vault_count))
        .ok_or(FleetObservationError::ArithmeticOverflow)?;
    if source_candidate_vault_count != source_set.source_candidate_vault_count
        || active_exclusion_breakdown_count != source_set.active_opportunity_vaults_excluded
        || partitioned_before_epoch != source_set.eligible_vault_count
    {
        return Err(FleetObservationError::CompletenessInvariant(format!(
            "eligible={}, source_candidates={} (decoded={}), active={} (breakdown={}), no_source={}",
            source_set.eligible_vault_count,
            source_set.source_candidate_vault_count,
            source_candidate_vault_count,
            source_set.active_opportunity_vaults_excluded,
            active_exclusion_breakdown_count,
            source_set.no_positive_current_source_vault_count,
        )));
    }
    let mut vault_outcomes_by_reason = BTreeMap::<String, i64>::new();
    for (state, count) in &source_set.active_opportunity_vaults_excluded_by_state {
        let key = if state == "active_decision" {
            "active_decision".to_owned()
        } else {
            format!("active_queue_{state}")
        };
        vault_outcomes_by_reason.insert(key, *count);
    }
    if source_set.no_positive_current_source_vault_count > 0 {
        vault_outcomes_by_reason.insert(
            "no_positive_current_source".to_owned(),
            source_set.no_positive_current_source_vault_count,
        );
    }
    let mut stats = FleetObservationStats {
        market_read_count: 1,
        neon_read_count: 1,
        eligible_vault_count: source_set.eligible_vault_count,
        source_candidate_vault_count: source_set.source_candidate_vault_count,
        active_opportunity_vaults_excluded: source_set.active_opportunity_vaults_excluded,
        active_opportunity_vaults_excluded_by_state: source_set
            .active_opportunity_vaults_excluded_by_state
            .clone(),
        no_positive_current_source_vault_count: source_set.no_positive_current_source_vault_count,
        vault_outcomes_by_reason,
        committed_target_inflow_reserve_count: source_set.committed_target_inflows.len(),
        committed_target_inflow_usd_micros: source_set
            .committed_target_inflows
            .iter()
            .fold(0i64, |total, inflow| {
                total.saturating_add(inflow.principal_usd_micros)
            }),
        ..FleetObservationStats::default()
    };
    let mut opportunities = Vec::new();
    let mut opportunity_vault_ids = BTreeSet::new();
    let mut source_vault_rejections = BTreeMap::new();
    for source in source_set.sources {
        let source_kind = match source.source_kind.as_str() {
            "reserve_position" => {
                stats.valued_position_source_count += 1;
                ObservedSourceKind::ReservePosition
            }
            "idle_vault_usdc" => {
                stats.idle_usdc_source_count += 1;
                ObservedSourceKind::IdleVaultUsdc
            }
            _ => {
                stats.unsupported_market_semantics_source_count += 1;
                record_source_vault_rejection(
                    &mut source_vault_rejections,
                    source.vault_id,
                    SourceVaultRejection::UnsupportedMarketSemantics,
                );
                continue;
            }
        };
        let Some(valuation) = valuations.get(&source.liquidity_mint) else {
            stats.missing_valuation_source_count += 1;
            record_source_vault_rejection(
                &mut source_vault_rejections,
                source.vault_id,
                SourceVaultRejection::MissingValuation,
            );
            continue;
        };
        let (
            amount_raw,
            route_amount_semantics,
            source_amount_semantics,
            source_collateral,
            redeemable_liquidity,
            idle_liquidity,
        ) = match source_kind {
            ObservedSourceKind::ReservePosition => {
                let Some(evidence) = route_amount_evidence_from_metadata(
                    source.amount_raw,
                    &source.planning_metadata,
                ) else {
                    stats.unsupported_amount_semantics_count += 1;
                    record_source_vault_rejection(
                        &mut source_vault_rejections,
                        source.vault_id,
                        SourceVaultRejection::UnsupportedAmountSemantics,
                    );
                    continue;
                };
                (
                    evidence.amount_raw,
                    evidence.route_amount_semantics,
                    evidence.source_amount_semantics,
                    evidence.source_collateral_amount_raw,
                    evidence.redeemable_source_liquidity_amount_raw,
                    evidence.idle_vault_liquidity_amount_raw,
                )
            }
            ObservedSourceKind::IdleVaultUsdc => (
                source.amount_raw,
                "idle_vault_liquidity".to_owned(),
                None,
                None,
                None,
                Some(source.amount_raw),
            ),
        };
        let source_apy_bps = match source_kind {
            ObservedSourceKind::IdleVaultUsdc => 0,
            ObservedSourceKind::ReservePosition => {
                let Some(source_reserve) = source.source_reserve.as_ref() else {
                    stats.unsupported_market_semantics_source_count += 1;
                    record_source_vault_rejection(
                        &mut source_vault_rejections,
                        source.vault_id,
                        SourceVaultRejection::UnsupportedMarketSemantics,
                    );
                    continue;
                };
                // Source and target APYs must come from the same immutable,
                // fresh market epoch. Falling back to the projected position's
                // stored APY can manufacture an edge from different vintages.
                let Some(source_epoch_reserve) = by_mint
                    .get(source.liquidity_mint.as_str())
                    .and_then(|reserves| {
                        reserves
                            .iter()
                            .find(|reserve| reserve.reserve == *source_reserve)
                    })
                else {
                    stats.unsupported_market_semantics_source_count += 1;
                    record_source_vault_rejection(
                        &mut source_vault_rejections,
                        source.vault_id,
                        SourceVaultRejection::UnsupportedMarketSemantics,
                    );
                    continue;
                };
                source_epoch_reserve.supply_apy_bps
            }
        };
        let targets = policy_targets(&source, by_mint.get(source.liquidity_mint.as_str()))
            .into_iter()
            .filter(|target| target.supply_apy_bps > source_apy_bps)
            .collect::<Vec<_>>();
        if targets.is_empty() {
            stats.missing_target_count += 1;
            record_source_vault_rejection(
                &mut source_vault_rejections,
                source.vault_id,
                SourceVaultRejection::NoEconomicTarget,
            );
            continue;
        }
        let notional_usd_micros = stablecoin_raw_to_usd_micros(amount_raw, valuation)?;
        // Scheduler aging starts when a durable opportunity enters the queue;
        // source telemetry age is evidence freshness, not fairness credit.
        let age_seconds = 0;
        let fairness_credit = config
            .tenant_fairness_credits
            .get(&source.policy_authority)
            .copied()
            .unwrap_or_default();
        let source_reserve = source
            .source_reserve
            .clone()
            .unwrap_or_else(|| format!("idle-vault:{}", source.vault_pubkey));
        let (expected_service_millis, estimated_cost) = match source_kind {
            ObservedSourceKind::ReservePosition => (
                config.expected_reserve_move_service_millis,
                config.estimated_reserve_move_cost_usd_micros,
            ),
            ObservedSourceKind::IdleVaultUsdc => (
                config.expected_idle_deposit_service_millis,
                config.estimated_idle_deposit_cost_usd_micros,
            ),
        };
        let source_snapshot_id = source
            .source_snapshot_id
            .unwrap_or(source.observed_slot)
            .max(1);
        opportunity_vault_ids.insert(source.vault_id);
        for target in targets {
            opportunities.push(ObservedFleetOpportunity {
                economics: OpportunityInput {
                    opportunity_id: 1,
                    optimizer_epoch_id: market_epoch.optimizer_epoch_id,
                    vault_id: source.vault_id,
                    tenant_id: source.policy_authority.clone(),
                    source_snapshot_id,
                    observed_slot: source.observed_slot.max(1),
                    mint: source.liquidity_mint.clone(),
                    source_reserve: source_reserve.clone(),
                    target_reserve: target.reserve.clone(),
                    notional_usd_micros,
                    source_net_apy_bps: source_apy_bps,
                    target_net_apy_bps: target.supply_apy_bps,
                    confidence_ppm: valuation.confidence_ppm,
                    expected_service_millis,
                    holding_horizon_seconds: config.holding_horizon_seconds,
                    estimated_execution_cost_usd_micros: estimated_cost,
                    age_seconds,
                    fairness_credit,
                    writable_conflict_keys: vec![
                        format!("vault:{}", source.vault_pubkey),
                        format!("policy:{}", source.policy_id),
                        format!(
                            "source-reserve:{}",
                            source.source_reserve.as_deref().unwrap_or("idle")
                        ),
                        format!("target-reserve:{}", target.reserve),
                    ],
                },
                source_kind,
                policy_id: source.policy_id,
                settings: source.settings.clone(),
                vault_index: source.vault_index,
                vault_pubkey: source.vault_pubkey.clone(),
                amount_raw,
                route_amount_semantics: route_amount_semantics.clone(),
                source_amount_semantics: source_amount_semantics.clone(),
                source_collateral_amount_raw: source_collateral,
                redeemable_source_liquidity_amount_raw: redeemable_liquidity,
                idle_vault_liquidity_amount_raw: idle_liquidity,
                idle_token_account: source.idle_token_account.clone(),
                source_observed_slot: source.observed_slot,
                source_observed_at: source.observed_at,
                target_observed_at: target.observed_at,
                target_observed_slot: target.slot,
            });
        }
    }
    opportunities.sort_by(|left, right| {
        left.economics
            .vault_id
            .cmp(&right.economics.vault_id)
            .then_with(|| {
                source_kind_rank(left.source_kind).cmp(&source_kind_rank(right.source_kind))
            })
            .then_with(|| left.economics.mint.cmp(&right.economics.mint))
            .then_with(|| {
                left.economics
                    .source_reserve
                    .cmp(&right.economics.source_reserve)
            })
            .then_with(|| {
                left.economics
                    .target_reserve
                    .cmp(&right.economics.target_reserve)
            })
    });
    for (index, opportunity) in opportunities.iter_mut().enumerate() {
        opportunity.economics.opportunity_id = i64::try_from(index)
            .map_err(|_| FleetObservationError::ArithmeticOverflow)?
            .checked_add(1)
            .ok_or(FleetObservationError::ArithmeticOverflow)?;
    }
    stats.opportunity_vault_count = i64::try_from(opportunity_vault_ids.len())
        .map_err(|_| FleetObservationError::ArithmeticOverflow)?;
    if stats.opportunity_vault_count > 0 {
        stats.vault_outcomes_by_reason.insert(
            "opportunity_observed".to_owned(),
            stats.opportunity_vault_count,
        );
    }
    for vault_id in source_vault_ids.difference(&opportunity_vault_ids) {
        let rejection = source_vault_rejections
            .get(vault_id)
            .copied()
            .ok_or_else(|| {
                FleetObservationError::CompletenessInvariant(format!(
                    "source-bearing vault {vault_id} produced neither an opportunity nor a rejection"
                ))
            })?;
        *stats
            .vault_outcomes_by_reason
            .entry(rejection.outcome_key().to_owned())
            .or_default() += 1;
    }
    stats.accounted_vault_count = stats
        .vault_outcomes_by_reason
        .values()
        .try_fold(0i64, |total, count| total.checked_add(*count))
        .ok_or(FleetObservationError::ArithmeticOverflow)?;
    stats.complete_vault_accounting = stats.accounted_vault_count == stats.eligible_vault_count;
    if !stats.complete_vault_accounting {
        return Err(FleetObservationError::CompletenessInvariant(format!(
            "eligible vault denominator {} differs from mutually exclusive outcomes {}",
            stats.eligible_vault_count, stats.accounted_vault_count,
        )));
    }
    stats.opportunity_count = opportunities.len();
    Ok(FleetObservationResult {
        market_epoch,
        opportunities,
        committed_target_inflows: source_set.committed_target_inflows,
        stats,
    })
}

fn policy_targets<'a>(
    source: &FleetSourceRow,
    reserves: Option<&Vec<&'a MarketEpochReserve>>,
) -> Vec<&'a MarketEpochReserve> {
    let mut targets = reserves
        .map(Vec::as_slice)
        .unwrap_or_default()
        .iter()
        .copied()
        .filter(|target| {
            source
                .source_reserve
                .as_ref()
                .is_none_or(|source_reserve| target.reserve != *source_reserve)
                && source
                    .policy_stable_mints
                    .iter()
                    .any(|mint| mint == &target.liquidity_mint)
                && source
                    .policy_liquidity_mints
                    .iter()
                    .any(|mint| mint == &target.liquidity_mint)
                && target.market.as_ref().is_some_and(|market| {
                    source
                        .policy_markets
                        .iter()
                        .any(|allowed| allowed == market)
                })
        })
        .collect::<Vec<_>>();
    targets.sort_by(|left, right| {
        right
            .supply_apy_bps
            .cmp(&left.supply_apy_bps)
            .then_with(|| right.observed_at.cmp(&left.observed_at))
            .then_with(|| right.slot.cmp(&left.slot))
            .then_with(|| left.reserve.cmp(&right.reserve))
    });
    targets
}

fn source_kind_rank(kind: ObservedSourceKind) -> u8 {
    match kind {
        ObservedSourceKind::IdleVaultUsdc => 0,
        ObservedSourceKind::ReservePosition => 1,
    }
}

fn market_epoch_fingerprint(reserves: &[MarketEpochReserve], enabled_mints: &[String]) -> String {
    let mut hasher = Sha256::new();
    for mint in enabled_mints {
        hash_part(&mut hasher, mint.as_bytes());
    }
    for reserve in reserves {
        hash_part(&mut hasher, reserve.reserve.as_bytes());
        hash_part(
            &mut hasher,
            reserve.market.as_deref().unwrap_or_default().as_bytes(),
        );
        hash_part(&mut hasher, reserve.liquidity_mint.as_bytes());
        hash_part(&mut hasher, &[reserve.mint_decimals]);
        hash_part(&mut hasher, &reserve.market_price_usd_micros.to_le_bytes());
        hash_part(
            &mut hasher,
            &reserve.observed_at.timestamp_micros().to_le_bytes(),
        );
        hash_part(&mut hasher, &reserve.slot.to_le_bytes());
        hash_part(&mut hasher, &reserve.supply_apy_bps.to_le_bytes());
        hash_part(&mut hasher, &reserve.total_supply_usd_micros.to_le_bytes());
    }
    format!("{:x}", hasher.finalize())
}

fn epoch_valuations(
    market_epoch: &ImmutableMarketEpoch,
    configured: &BTreeMap<String, StablecoinValuation>,
) -> Result<BTreeMap<String, StablecoinValuation>, FleetObservationError> {
    let mut valuations = configured.clone();
    let mut by_mint = BTreeMap::<&str, Vec<&MarketEpochReserve>>::new();
    for reserve in &market_epoch.reserves {
        by_mint
            .entry(reserve.liquidity_mint.as_str())
            .or_default()
            .push(reserve);
    }
    for (mint, reserves) in by_mint {
        if valuations.contains_key(mint) {
            continue;
        }
        let Some(first) = reserves.first() else {
            continue;
        };
        if reserves
            .iter()
            .any(|reserve| reserve.mint_decimals != first.mint_decimals)
        {
            return Err(FleetObservationError::InvalidConfig(format!(
                "market epoch disagrees on decimals for mint {mint}"
            )));
        }
        let minimum_price = reserves
            .iter()
            .map(|reserve| reserve.market_price_usd_micros)
            .min()
            .ok_or(FleetObservationError::ArithmeticOverflow)?;
        let maximum_price = reserves
            .iter()
            .map(|reserve| reserve.market_price_usd_micros)
            .max()
            .ok_or(FleetObservationError::ArithmeticOverflow)?;
        if minimum_price <= 0 || maximum_price <= 0 {
            continue;
        }
        // Use the lowest contemporaneous reserve oracle price for notional
        // and reduce confidence when reserve price observations disagree.
        let consistency_ppm = (i128::from(minimum_price) * 1_000_000 / i128::from(maximum_price))
            .clamp(1, 1_000_000) as u32;
        valuations.insert(
            mint.to_owned(),
            StablecoinValuation {
                mint: mint.to_owned(),
                decimals: first.mint_decimals,
                price_usd_micros: minimum_price,
                confidence_ppm: consistency_ppm.min(950_000),
            },
        );
    }
    Ok(valuations)
}

fn hash_part(hasher: &mut Sha256, value: &[u8]) {
    hasher.update((value.len() as u64).to_le_bytes());
    hasher.update(value);
}

fn positive_epoch_id(fingerprint: &str) -> i64 {
    let bytes = fingerprint.as_bytes();
    let mut folded = 0xcbf29ce484222325u64;
    for byte in bytes {
        folded ^= u64::from(*byte);
        folded = folded.wrapping_mul(0x100000001b3);
    }
    i64::try_from(folded & i64::MAX as u64)
        .unwrap_or(i64::MAX)
        .max(1)
}

fn apy_to_bps(apy: f64) -> Result<i64, FleetObservationError> {
    let bps = apy * 10_000.0;
    if !bps.is_finite() || bps < i64::MIN as f64 || bps > i64::MAX as f64 {
        return Err(FleetObservationError::ArithmeticOverflow);
    }
    Ok(bps.round() as i64)
}

fn usd_to_micros(usd: f64) -> Result<i64, FleetObservationError> {
    let micros = usd * USD_MICROS_PER_USD as f64;
    if !micros.is_finite() || micros < 0.0 || micros > i64::MAX as f64 {
        return Err(FleetObservationError::ArithmeticOverflow);
    }
    Ok(micros.round() as i64)
}
