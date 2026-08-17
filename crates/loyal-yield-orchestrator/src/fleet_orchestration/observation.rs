pub use super::queue::MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS;
use super::{
    domain::OpportunityInput,
    planner::{CandidateExecutionCosts, CandidateRouteKind},
};
use crate::{route_amount_evidence_from_metadata, NeonSqlClient, ACTIVE_DECISION_STATUSES};
use chrono::{DateTime, Duration, Utc};
use loyal_actions::earn_stablecoins;
use loyal_yield_router::timescale::{
    SupportedReserveCatalogRow, SupportedReserveMarketSnapshot,
    SupportedReserveMarketSnapshotQuery, TimescaleRouterClient, VerifiedSupportedReserveRow,
};
use loyal_yield_store::fleet_orchestration::CrossMintSwapPolicyBinding;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, BTreeSet},
    time::Instant,
};
use thiserror::Error;

const SAME_MINT_ROUTE_MODE: &str = "same_mint_kamino";
const USD_MICROS_PER_USD: i64 = 1_000_000;
/// Domain-separates optimizer epochs whose durable row lifetime is the
/// longest complete-mint envelope while each route retains its own mint
/// lifetime. Historical epoch rows remain immutable under their pre-v3
/// fingerprints instead of colliding during rollout.
///
/// v3 additionally covers `MarketMintBlocker::detail`. v2 hashed only the
/// blocker code and reserve, so every value a detail interpolates was outside
/// the key. That is inert while a mint is complete, because its reserves are
/// hashed in full, but an incomplete mint contributes no reserves at all
/// (see the `complete` gate below) and pins `expires_at` to `None`. Its
/// details still carry live slot lag and economic expiry, so two reads of an
/// unchanged complete frontier re-derived one key over two different
/// `market_state` bodies and the durable upsert rejected the second.
const MARKET_EPOCH_FINGERPRINT_DOMAIN: &[u8] = b"loyal-yield-market-epoch-envelope-v3";
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
/// Routes need enough immutable market lifetime to survive queue publication,
/// claim, compilation, and a normal submission round without relaxing the
/// hard confirmed-verification expiry.
/// Exact confirmed verification is hard-expired after four minutes even if a
/// caller supplies a looser market-age configuration.
pub const MAXIMUM_CONFIRMED_VERIFICATION_AGE_SECONDS: i64 = 240;
/// The monitor refreshes catalog identity independently from reserve account
/// updates. Routing refuses catalog rows older than this bound and also
/// reserves the publication lifetime above before admitting work.
pub const MAXIMUM_SUPPORTED_RESERVE_CATALOG_AGE_SECONDS: i64 = 300;
/// Maximum admitted distance between the confirmed RPC context and Klend's
/// internal LastUpdate slot. This is a reserve-local safety fence: crossing it
/// quarantines that reserve without freezing healthy peers for the mint.
pub const MAXIMUM_RESERVE_ECONOMIC_SLOT_LAG: i64 = 1_500;
/// Conservative conversion used to expire an admitted reserve before its
/// LastUpdate lag can cross the reserve-local bound above.
pub const RESERVE_ECONOMIC_EXPIRY_MILLIS_PER_SLOT: i64 = 250;
const CODE_OWNED_STABLECOIN_DECIMALS: u8 = 6;
const CODE_OWNED_STABLECOIN_CONFIDENCE_PPM: u32 = 950_000;

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct StablecoinValuation {
    pub mint: String,
    pub decimals: u8,
    /// USD micro-dollars per whole token. This must be supplied explicitly.
    pub price_usd_micros: i64,
    pub confidence_ppm: u32,
}

/// Returns the code-owned valuation contract for the production stablecoin
/// universe. Reserve oracle price age/status remains observable evidence, but
/// cannot distort stable capacity or make two reserves of the same mint carry
/// different USD notionals.
pub fn code_owned_stablecoin_valuations(
    enabled_mints: &[String],
) -> Result<Vec<StablecoinValuation>, FleetObservationError> {
    let supported = earn_stablecoins()
        .iter()
        .map(|stablecoin| stablecoin.mint.to_string())
        .collect::<BTreeSet<_>>();
    let mut valuations = Vec::with_capacity(enabled_mints.len());
    for mint in enabled_mints.iter().cloned().collect::<BTreeSet<_>>() {
        if !supported.contains(mint.as_str()) {
            return Err(FleetObservationError::InvalidConfig(format!(
                "missing code-owned stable valuation for mint {mint}"
            )));
        }
        valuations.push(StablecoinValuation {
            mint,
            decimals: CODE_OWNED_STABLECOIN_DECIMALS,
            price_usd_micros: USD_MICROS_PER_USD,
            confidence_ppm: CODE_OWNED_STABLECOIN_CONFIDENCE_PPM,
        });
    }
    Ok(valuations)
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
    /// Default-off rollout gate. Enabling it admits only exact, finalized,
    /// directed swap capabilities observed independently from the base policy.
    pub enable_cross_mint_jupiter: bool,
    pub estimated_cross_mint_withdraw_cost_usd_micros: i64,
    pub estimated_cross_mint_jupiter_swap_cost_usd_micros: i64,
    pub estimated_cross_mint_deposit_cost_usd_micros: i64,
    /// Protection ceiling for the fresh executable quote. It is intentionally
    /// not treated as expected execution cost by the optimizer.
    pub cross_mint_maximum_value_loss_bps: u16,
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
            maximum_market_age_seconds: MAXIMUM_CONFIRMED_VERIFICATION_AGE_SECONDS,
            rebalance_cooldown_seconds: 5 * 60,
            holding_horizon_seconds: 30 * 24 * 60 * 60,
            expected_reserve_move_service_millis: 15_000,
            expected_idle_deposit_service_millis: 15_000,
            estimated_reserve_move_cost_usd_micros: 500_000,
            estimated_idle_deposit_cost_usd_micros: 500_000,
            enable_cross_mint_jupiter: false,
            estimated_cross_mint_withdraw_cost_usd_micros: 500_000,
            estimated_cross_mint_jupiter_swap_cost_usd_micros: 500_000,
            estimated_cross_mint_deposit_cost_usd_micros: 500_000,
            cross_mint_maximum_value_loss_bps: 50,
            tenant_fairness_credits: BTreeMap::new(),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MarketEpochReserve {
    pub state_event_id: i64,
    pub account_data_hash: String,
    pub state_observed_at: DateTime<Utc>,
    pub state_slot: i64,
    pub verification_commitment: String,
    pub reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
    pub mint_decimals: u8,
    /// Code-owned stable valuation used for source notional and capacity.
    pub market_price_usd_micros: i64,
    pub reserve_last_update_slot: i64,
    /// Distance between the confirmed RPC context and Klend's LastUpdate slot.
    pub economic_slot_lag: i64,
    /// Reserve-local LastUpdate expiry, additionally bounded by the confirmed
    /// HTTP verification expiry at the epoch level.
    pub economic_expires_at: DateTime<Utc>,
    pub reserve_last_update_stale: bool,
    /// Retained as evidence, but deliberately not used as a supply-APY gate.
    pub reserve_price_status: i16,
    pub market_price_last_updated_ts: i64,
    pub available_amount_raw: String,
    pub borrowed_amount_raw: String,
    pub total_supply_amount_raw: String,
    pub utilization_ppm: i64,
    pub borrow_apy_bps: i64,
    /// Confirmed verification time used for freshness and expiry.
    pub observed_at: DateTime<Utc>,
    /// Confirmed RPC context slot used for freshness and epoch identity.
    pub slot: i64,
    pub supply_apy_bps: i64,
    pub total_supply_usd_micros: i64,
    pub target_eligible: bool,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MarketMintBlockerCode {
    MissingCatalog,
    CatalogSourceMismatch,
    CatalogFetchedInFuture,
    CatalogStale,
    CatalogInsufficientLifetime,
    DuplicateCatalogReserveIdentity,
    DuplicateVerifiedReserveIdentity,
    MissingVerifiedReserve,
    VerifiedIdentityMismatch,
    VerificationSourceMismatch,
    VerificationCommitmentMismatch,
    VerificationInFuture,
    VerificationStale,
    VerificationInsufficientLifetime,
    InvalidStateIdentity,
    MissingStableValuation,
    MintDecimalsMismatch,
    ExplicitStaleEconomics,
    InvalidEconomicSlotOrder,
    EconomicSlotLagExceeded,
    EconomicInsufficientLifetime,
    InvalidEconomicFields,
    NoEligibleTarget,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MarketMintBlocker {
    pub code: MarketMintBlockerCode,
    pub reserve: Option<String>,
    pub detail: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MarketMintCoverage {
    pub mint: String,
    pub catalog_reserve_count: usize,
    pub verified_reserve_count: usize,
    pub eligible_target_reserve_count: usize,
    /// True when the mint-wide contract is sound and at least one admissible
    /// target exists. Reserve-scoped blockers remain explicit below, but only
    /// exclude that reserve instead of freezing healthy peers for the mint.
    pub complete: bool,
    pub expires_at: Option<DateTime<Utc>>,
    pub blockers: Vec<MarketMintBlocker>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ImmutableMarketEpoch {
    pub optimizer_epoch_id: i64,
    pub fingerprint: String,
    pub catalog_fingerprint: String,
    pub captured_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
    pub catalog_expires_at: DateTime<Utc>,
    pub catalog_reserve_count: usize,
    pub oldest_market_observed_at: Option<DateTime<Utc>>,
    pub newest_market_observed_at: Option<DateTime<Utc>>,
    pub minimum_market_slot: Option<i64>,
    pub maximum_market_slot: Option<i64>,
    pub mint_coverage: Vec<MarketMintCoverage>,
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

    /// Returns the durable lifetime of the multi-mint optimizer envelope.
    ///
    /// `expires_at` deliberately remains the conservative global-minimum
    /// diagnostic. The optimizer row may remain addressable while any complete
    /// mint in this immutable snapshot is still usable; individual routes must
    /// use [`Self::mint_expires_at`] and never inherit this wider envelope.
    pub fn optimizer_envelope_expires_at(&self) -> DateTime<Utc> {
        self.mint_coverage
            .iter()
            .filter(|coverage| coverage.complete)
            .filter_map(|coverage| coverage.expires_at)
            .max()
            .unwrap_or(self.expires_at)
    }

    /// Returns the hard lifetime for one complete mint inside this immutable
    /// snapshot. Missing or incomplete mint coverage is never made usable by
    /// the wider optimizer envelope.
    pub fn mint_expires_at(&self, mint: &str) -> Option<DateTime<Utc>> {
        self.mint_coverage
            .iter()
            .find(|coverage| coverage.complete && coverage.mint == mint)
            .and_then(|coverage| coverage.expires_at)
    }

    /// A cross-mint route is publishable only while both mint frontiers remain
    /// valid. Same-mint callers naturally receive the existing mint lifetime.
    pub fn route_expires_at(&self, source_mint: &str, target_mint: &str) -> Option<DateTime<Utc>> {
        let source_expires_at = self.mint_expires_at(source_mint)?;
        let target_expires_at = self.mint_expires_at(target_mint)?;
        Some(source_expires_at.min(target_expires_at))
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
                    target_eligible: reserve.target_eligible,
                })
                .collect(),
        }
    }

    /// Extracts the material frontier for one mint. This lets route workers
    /// compare only the market topology and economics that can affect their
    /// same-mint route, without unrelated mint expiry or churn invalidating it.
    pub fn material_market_frontier_for_mint(&self, mint: &str) -> MaterialMarketFrontier {
        MaterialMarketFrontier {
            reserves: self
                .reserves
                .iter()
                .filter(|reserve| reserve.liquidity_mint == mint)
                .map(|reserve| MaterialMarketFrontierReserve {
                    reserve: reserve.reserve.clone(),
                    market: reserve.market.clone(),
                    liquidity_mint: reserve.liquidity_mint.clone(),
                    mint_decimals: reserve.mint_decimals,
                    market_price_usd_micros: reserve.market_price_usd_micros,
                    supply_apy_bps: reserve.supply_apy_bps,
                    total_supply_usd_micros: reserve.total_supply_usd_micros,
                    target_eligible: reserve.target_eligible,
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
    pub target_eligible: bool,
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

    /// Route execution re-reads the source and target APYs, recomputes target
    /// capacity, and reruns the economic gate before signing. Those bounded
    /// changes therefore do not need to starve an otherwise valid route.
    /// Price changes can invalidate the durable principal, while topology
    /// changes may remove or replace a route account, so both remain fenced.
    pub fn allows_current_route_revalidation(self) -> bool {
        matches!(
            self,
            Self::ReuseScopedFrontier
                | Self::FullSweepSupplyApyChanged
                | Self::FullSweepTargetCapacityChanged
        )
    }

    pub fn requires_current_route_topology_convergence(self) -> bool {
        self == Self::FullSweepReserveTopologyChanged
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
                || baseline.target_eligible != latest.target_eligible
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
    pub economic_boundary_lag_slots: i64,
    pub economic_boundary_is_rejected: bool,
    pub economic_one_slot_fresher_is_usable: bool,
    pub economic_nonzero_verification_age_seconds: i64,
    pub economic_nonzero_age_is_rejected: bool,
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
        catalog_fingerprint: "catalog-fixture".to_owned(),
        captured_at: observed_at,
        expires_at: observed_at + Duration::minutes(5),
        catalog_expires_at: observed_at + Duration::minutes(5),
        catalog_reserve_count: 1,
        oldest_market_observed_at: Some(observed_at),
        newest_market_observed_at: Some(observed_at),
        minimum_market_slot: Some(100),
        maximum_market_slot: Some(100),
        mint_coverage: vec![MarketMintCoverage {
            mint: "USDC".to_owned(),
            catalog_reserve_count: 1,
            verified_reserve_count: 1,
            eligible_target_reserve_count: 1,
            complete: true,
            expires_at: Some(observed_at + Duration::minutes(5)),
            blockers: Vec::new(),
        }],
        reserves: vec![MarketEpochReserve {
            state_event_id: 1,
            account_data_hash: "00".repeat(32),
            state_observed_at: observed_at,
            state_slot: 100,
            verification_commitment: "confirmed".to_owned(),
            reserve: "reserve-a".to_owned(),
            market: Some("market-a".to_owned()),
            liquidity_mint: "USDC".to_owned(),
            mint_decimals: 6,
            market_price_usd_micros: USD_MICROS_PER_USD,
            reserve_last_update_slot: 100,
            economic_slot_lag: 0,
            economic_expires_at: observed_at + Duration::minutes(5),
            reserve_last_update_stale: false,
            reserve_price_status: 0,
            market_price_last_updated_ts: observed_at.timestamp(),
            available_amount_raw: "1000000000000".to_owned(),
            borrowed_amount_raw: "0".to_owned(),
            total_supply_amount_raw: "1000000000000".to_owned(),
            utilization_ppm: 0,
            borrow_apy_bps: 0,
            observed_at,
            slot: 100,
            supply_apy_bps: 500,
            total_supply_usd_micros: 1_000_000_000_000,
            target_eligible: true,
        }],
    };
    baseline.fingerprint = market_epoch_fingerprint(
        &baseline.reserves,
        &["USDC".to_owned()],
        &baseline.catalog_fingerprint,
        &baseline.mint_coverage,
    );
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
    harmless.reserves[0].economic_slot_lag = 150;
    harmless.reserves[0].economic_expires_at += Duration::seconds(15);
    harmless.reserves[0].total_supply_usd_micros = 1_000_500_000_000;
    harmless.fingerprint = market_epoch_fingerprint(
        &harmless.reserves,
        &["USDC".to_owned()],
        &harmless.catalog_fingerprint,
        &harmless.mint_coverage,
    );

    let mut material_apy = harmless.clone();
    material_apy.reserves[0].supply_apy_bps += 1;
    let mut material_capacity = harmless.clone();
    material_capacity.reserves[0].total_supply_usd_micros = 1_002_000_000_000;
    let mut material_topology = harmless.clone();
    material_topology.reserves[0].reserve = "reserve-b".to_owned();

    let frontier = baseline.material_market_frontier();
    let publication_millis = MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS * 1_000;
    let boundary_remaining_slots = (publication_millis + RESERVE_ECONOMIC_EXPIRY_MILLIS_PER_SLOT
        - 1)
        / RESERVE_ECONOMIC_EXPIRY_MILLIS_PER_SLOT;
    let economic_boundary_lag_slots = MAXIMUM_RESERVE_ECONOMIC_SLOT_LAG - boundary_remaining_slots;
    let nonzero_age_lag_slots = 1_200;
    let economic_nonzero_verification_age_seconds = 30;
    let nonzero_age_remaining_millis = (MAXIMUM_RESERVE_ECONOMIC_SLOT_LAG - nonzero_age_lag_slots)
        * RESERVE_ECONOMIC_EXPIRY_MILLIS_PER_SLOT
        - economic_nonzero_verification_age_seconds * 1_000;
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
        economic_boundary_lag_slots,
        economic_boundary_is_rejected: boundary_remaining_slots
            * RESERVE_ECONOMIC_EXPIRY_MILLIS_PER_SLOT
            <= publication_millis,
        economic_one_slot_fresher_is_usable: (boundary_remaining_slots + 1)
            * RESERVE_ECONOMIC_EXPIRY_MILLIS_PER_SLOT
            > publication_millis,
        economic_nonzero_verification_age_seconds,
        economic_nonzero_age_is_rejected: nonzero_age_remaining_millis <= publication_millis,
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
    pub route_kind: CandidateRouteKind,
    pub source_liquidity_mint: String,
    pub target_liquidity_mint: String,
    pub estimated_execution_costs: CandidateExecutionCosts,
    /// Present for cross-mint candidates so publication records exactly which
    /// conservative loss ceiling was priced before the executable quote gate.
    pub cross_mint_maximum_value_loss_bps: Option<u16>,
    /// Exact stored Jupiter policy lane which admitted a cross-mint candidate.
    /// Candidate planning never calls Jupiter; the executor must still obtain
    /// and validate a fresh build before signing the swap leg.
    pub jupiter_swap_lane: Option<CrossMintSwapPolicyBinding>,
    /// Exact finalized Earn policy authorizing the cross-mint withdrawal.
    /// Same-mint routes continue to use the active base-policy fields below.
    pub source_earn_policy: Option<ObservedEarnPolicyEvidence>,
    /// Exact finalized Earn policy authorizing the cross-mint deposit.
    pub target_earn_policy: Option<ObservedEarnPolicyEvidence>,
    /// Active base Earn policy retained for existing same-mint paths.
    pub base_policy_account: String,
    pub base_policy_delegated_signer: String,
    pub base_policy_source_commitment: String,
    pub base_policy_observed_slot: i64,
    pub base_policy_observed_signature: String,
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

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ObservedEarnPolicyEvidence {
    pub settings: String,
    pub authority: String,
    pub policy_account: String,
    pub vault_index: i16,
    pub vault_pubkey: String,
    pub delegated_signer: String,
    pub threshold: i32,
    pub stable_mints: Vec<String>,
    pub kamino_markets: Vec<String>,
    pub kamino_liquidity_mints: Vec<String>,
    pub source_commitment: String,
    pub observed_slot: i64,
    pub observed_signature: String,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetObservationStats {
    pub market_read_count: u32,
    pub neon_read_count: u32,
    pub market_read_micros: u64,
    pub neon_read_micros: u64,
    pub projection_micros: u64,
    pub rpc_read_count: u32,
    pub child_process_count: u32,
    pub market_catalog_fingerprint: String,
    pub market_catalog_reserve_count: usize,
    pub complete_market_mint_count: usize,
    pub blocked_market_mint_count: usize,
    pub market_mint_coverage: Vec<MarketMintCoverage>,
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
    pub committed_source_outflow_reserve_count: usize,
    pub committed_source_outflow_usd_micros: i64,
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
    pub committed_source_outflows: Vec<CommittedSourceOutflow>,
    pub stats: FleetObservationStats,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CommittedTargetInflow {
    pub target_reserve: String,
    pub principal_usd_micros: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CommittedSourceOutflow {
    pub source_reserve: String,
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
    let market_started = Instant::now();
    let source_started = Instant::now();
    let ((market_epoch, market_read_micros), (source_set, neon_read_micros)) = tokio::try_join!(
        async {
            Ok::<_, FleetObservationError>((
                load_market_epoch(timescale, config, &validated.enabled_mints, captured_at).await?,
                elapsed_micros(market_started),
            ))
        },
        async {
            Ok::<_, FleetObservationError>((
                load_fleet_sources_without_queue_schema(
                    neon,
                    delegated_signer,
                    &validated.enabled_mints,
                    config.rebalance_cooldown_seconds,
                    captured_at,
                )
                .await?,
                elapsed_micros(source_started),
            ))
        },
    )?;
    build_timed_observation_result(
        market_epoch,
        source_set,
        &validated,
        config,
        market_read_micros,
        neon_read_micros,
    )
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
    let market_started = Instant::now();
    let source_started = Instant::now();
    let ((market_epoch, market_read_micros), (source_set, neon_read_micros)) = tokio::try_join!(
        async {
            Ok::<_, FleetObservationError>((
                load_market_epoch(timescale, config, &validated.enabled_mints, captured_at).await?,
                elapsed_micros(market_started),
            ))
        },
        async {
            Ok::<_, FleetObservationError>((
                load_fleet_sources(
                    neon,
                    delegated_signer,
                    config,
                    &validated.enabled_mints,
                    captured_at,
                    vault_ids,
                )
                .await?,
                elapsed_micros(source_started),
            ))
        },
    )?;
    build_timed_observation_result(
        market_epoch,
        source_set,
        &validated,
        config,
        market_read_micros,
        neon_read_micros,
    )
}

fn elapsed_micros(started: Instant) -> u64 {
    u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX)
}

fn build_timed_observation_result(
    market_epoch: ImmutableMarketEpoch,
    source_set: FleetSourceSet,
    validated: &ValidatedConfig,
    config: &FleetObservationConfig,
    market_read_micros: u64,
    neon_read_micros: u64,
) -> Result<FleetObservationResult, FleetObservationError> {
    let projection_started = Instant::now();
    let mut result = build_observation_result(market_epoch, source_set, validated, config)?;
    result.stats.market_read_micros = market_read_micros;
    result.stats.neon_read_micros = neon_read_micros;
    result.stats.projection_micros = elapsed_micros(projection_started);
    Ok(result)
}

async fn load_market_epoch(
    timescale: &TimescaleRouterClient,
    config: &FleetObservationConfig,
    enabled_mints: &[String],
    _captured_at: DateTime<Utc>,
) -> Result<ImmutableMarketEpoch, FleetObservationError> {
    let snapshot = timescale
        .supported_reserve_market_snapshot(SupportedReserveMarketSnapshotQuery {
            risk_baskets: config.risk_baskets.clone(),
            liquidity_mints: enabled_mints.to_vec(),
        })
        .await
        .map_err(FleetObservationError::MarketRead)?;
    build_market_epoch(snapshot, enabled_mints, config)
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
            || config.maximum_market_age_seconds <= MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS
            || config.rebalance_cooldown_seconds < 0
            || config.holding_horizon_seconds == 0
            || config.expected_reserve_move_service_millis == 0
            || config.expected_idle_deposit_service_millis == 0
            || config.estimated_reserve_move_cost_usd_micros < 0
            || config.estimated_idle_deposit_cost_usd_micros < 0
            || config.estimated_cross_mint_withdraw_cost_usd_micros < 0
            || config.estimated_cross_mint_jupiter_swap_cost_usd_micros < 0
            || config.estimated_cross_mint_deposit_cost_usd_micros < 0
            || config.cross_mint_maximum_value_loss_bps == 0
            || config.cross_mint_maximum_value_loss_bps > 1_000
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
    snapshot: SupportedReserveMarketSnapshot,
    enabled_mints: &[String],
    config: &FleetObservationConfig,
) -> Result<ImmutableMarketEpoch, FleetObservationError> {
    let captured_at = snapshot.captured_at;
    let publication_minimum =
        captured_at + Duration::seconds(MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS);
    let valuations = config
        .stablecoin_valuations
        .iter()
        .map(|valuation| (valuation.mint.clone(), valuation.clone()))
        .collect::<BTreeMap<_, _>>();

    let mut catalog = snapshot.catalog;
    catalog.sort_by(|left, right| {
        left.liquidity_mint
            .cmp(&right.liquidity_mint)
            .then_with(|| left.market.cmp(&right.market))
            .then_with(|| left.reserve.cmp(&right.reserve))
            .then_with(|| left.fetched_at.cmp(&right.fetched_at))
    });
    let catalog_fingerprint = supported_reserve_catalog_fingerprint(&catalog);
    let catalog_reserve_count = catalog.len();

    let mut catalog_identity_counts = BTreeMap::<String, usize>::new();
    for row in &catalog {
        *catalog_identity_counts
            .entry(row.reserve.clone())
            .or_default() += 1;
    }
    let mut verified_by_identity =
        BTreeMap::<(String, String, String), Vec<VerifiedSupportedReserveRow>>::new();
    let mut verified_by_reserve = BTreeSet::new();
    for row in snapshot.verified_reserves {
        verified_by_reserve.insert(row.reserve.clone());
        if let Some(market) = row.market.clone() {
            verified_by_identity
                .entry((row.reserve.clone(), market, row.liquidity_mint.clone()))
                .or_default()
                .push(row);
        }
    }

    let mut reserves = Vec::new();
    let mut mint_coverage = Vec::with_capacity(enabled_mints.len());
    let mut routable_catalog_expiries = Vec::new();
    let mut routable_epoch_expiries = Vec::new();
    for mint in enabled_mints {
        let mint_catalog = catalog
            .iter()
            .filter(|row| row.liquidity_mint == *mint)
            .collect::<Vec<_>>();
        let mut blockers = Vec::new();
        let mut mint_wide_blocked = false;
        let mut verified_reserve_count = 0usize;
        let mut candidate_reserves = Vec::with_capacity(mint_catalog.len());
        let mut mint_catalog_expiries = Vec::new();
        let mut mint_verification_expiries = Vec::new();
        let mut mint_economic_expiries = Vec::new();

        if mint_catalog.is_empty() {
            mint_wide_blocked = true;
            push_market_blocker(
                &mut blockers,
                MarketMintBlockerCode::MissingCatalog,
                None,
                format!("active safe catalog has no reserve for enabled mint {mint}"),
            );
        }
        let valuation = valuations.get(mint);
        if valuation.is_none() {
            mint_wide_blocked = true;
            push_market_blocker(
                &mut blockers,
                MarketMintBlockerCode::MissingStableValuation,
                None,
                format!("enabled mint {mint} has no code-owned stable valuation"),
            );
        }

        for catalog_row in &mint_catalog {
            let reserve = Some(catalog_row.reserve.clone());
            let mut reserve_blockers = Vec::new();
            if catalog_row.source != "kamino-api" {
                push_market_blocker(
                    &mut reserve_blockers,
                    MarketMintBlockerCode::CatalogSourceMismatch,
                    reserve.clone(),
                    format!("catalog source is {}", catalog_row.source),
                );
            }
            let catalog_expiry = catalog_row.fetched_at
                + Duration::seconds(MAXIMUM_SUPPORTED_RESERVE_CATALOG_AGE_SECONDS);
            if catalog_row.fetched_at > captured_at {
                push_market_blocker(
                    &mut reserve_blockers,
                    MarketMintBlockerCode::CatalogFetchedInFuture,
                    reserve.clone(),
                    format!(
                        "catalog fetched_at {} is in the future",
                        catalog_row.fetched_at
                    ),
                );
            } else if catalog_expiry <= captured_at {
                push_market_blocker(
                    &mut reserve_blockers,
                    MarketMintBlockerCode::CatalogStale,
                    reserve.clone(),
                    format!("catalog expired at {catalog_expiry}"),
                );
            } else if catalog_expiry <= publication_minimum {
                push_market_blocker(
                    &mut reserve_blockers,
                    MarketMintBlockerCode::CatalogInsufficientLifetime,
                    reserve.clone(),
                    format!(
                        "catalog expires at {catalog_expiry}; remaining lifetime is below {MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS} seconds"
                    ),
                );
            }
            if catalog_identity_counts
                .get(&catalog_row.reserve)
                .copied()
                .unwrap_or_default()
                != 1
            {
                push_market_blocker(
                    &mut reserve_blockers,
                    MarketMintBlockerCode::DuplicateCatalogReserveIdentity,
                    reserve.clone(),
                    "reserve identity appears more than once in the active safe enabled catalog"
                        .to_owned(),
                );
            }

            let key = (
                catalog_row.reserve.clone(),
                catalog_row.market.clone(),
                catalog_row.liquidity_mint.clone(),
            );
            let Some(matches) = verified_by_identity.get(&key) else {
                let code = if verified_by_reserve.contains(&catalog_row.reserve) {
                    MarketMintBlockerCode::VerifiedIdentityMismatch
                } else {
                    MarketMintBlockerCode::MissingVerifiedReserve
                };
                push_market_blocker(
                    &mut reserve_blockers,
                    code,
                    reserve.clone(),
                    "catalog identity has no exact row in latest_verified_reserve_updates"
                        .to_owned(),
                );
                blockers.extend(reserve_blockers);
                continue;
            };
            if matches.len() != 1 {
                push_market_blocker(
                    &mut reserve_blockers,
                    MarketMintBlockerCode::DuplicateVerifiedReserveIdentity,
                    reserve.clone(),
                    format!("exact verified identity returned {} rows", matches.len()),
                );
                blockers.extend(reserve_blockers);
                continue;
            }
            verified_reserve_count += 1;
            let exact = &matches[0];
            let verification_expiry = exact.verified_at
                + Duration::seconds(
                    config
                        .maximum_market_age_seconds
                        .min(MAXIMUM_CONFIRMED_VERIFICATION_AGE_SECONDS),
                );
            let target_economic_expiry = validate_exact_verified_reserve(
                exact,
                catalog_row,
                valuation,
                captured_at,
                publication_minimum,
                verification_expiry,
                config,
                &mut reserve_blockers,
                &mut blockers,
            );
            if !reserve_blockers.is_empty() {
                blockers.extend(reserve_blockers);
                continue;
            }
            let valuation = valuation.expect("validated stable valuation must exist");
            let converted_economics = (
                stable_supply_to_usd_micros(exact.total_supply_amount, valuation),
                apy_to_bps(exact.supply_apy),
                ratio_to_ppm(exact.utilization),
                apy_to_bps(exact.borrow_apy),
            );
            let (
                Ok(total_supply_usd_micros),
                Ok(supply_apy_bps),
                Ok(utilization_ppm),
                Ok(borrow_apy_bps),
            ) = converted_economics
            else {
                push_market_blocker(
                    &mut blockers,
                    MarketMintBlockerCode::InvalidEconomicFields,
                    reserve,
                    "reserve economics cannot be represented in routing fixed-point units"
                        .to_owned(),
                );
                continue;
            };
            // Kamino routes refresh both reserves before the protected
            // withdraw/deposit instructions. A freshly verified reserve whose
            // internal economics need that refresh remains a valid source, but
            // must never be selected as the destination until its pre-refresh
            // economics are independently target-fresh.
            let target_eligible = target_economic_expiry.is_some()
                && total_supply_usd_micros > usd_to_micros(config.minimum_reserve_supply_usd)?
                && exact.supply_apy >= config.minimum_supply_apy
                && exact.supply_apy < config.maximum_supply_apy;
            let economic_expires_at = target_economic_expiry.unwrap_or(verification_expiry);
            mint_catalog_expiries.push(catalog_expiry);
            mint_verification_expiries.push(verification_expiry);
            if target_economic_expiry.is_some() {
                mint_economic_expiries.push(economic_expires_at);
            }
            candidate_reserves.push(MarketEpochReserve {
                state_event_id: exact.state_event_id,
                account_data_hash: exact.account_data_hash.clone(),
                state_observed_at: exact.state_observed_at,
                state_slot: exact.state_slot,
                verification_commitment: exact.verification_commitment.clone(),
                reserve: exact.reserve.clone(),
                market: exact.market.clone(),
                liquidity_mint: exact.liquidity_mint.clone(),
                mint_decimals: valuation.decimals,
                market_price_usd_micros: valuation.price_usd_micros,
                reserve_last_update_slot: exact.reserve_last_update_slot,
                economic_slot_lag: exact.verified_slot - exact.reserve_last_update_slot,
                economic_expires_at,
                reserve_last_update_stale: exact.reserve_last_update_stale,
                reserve_price_status: exact.reserve_price_status,
                market_price_last_updated_ts: exact.market_price_last_updated_ts,
                available_amount_raw: canonical_f64(exact.available_amount),
                borrowed_amount_raw: canonical_f64(exact.borrowed_amount),
                total_supply_amount_raw: canonical_f64(exact.total_supply_amount),
                utilization_ppm,
                borrow_apy_bps,
                observed_at: exact.verified_at,
                slot: exact.verified_slot,
                supply_apy_bps,
                total_supply_usd_micros,
                target_eligible,
            });
        }

        let eligible_target_reserve_count = candidate_reserves
            .iter()
            .filter(|reserve| reserve.target_eligible)
            .count();
        if eligible_target_reserve_count == 0 {
            mint_wide_blocked = true;
            push_market_blocker(
                &mut blockers,
                MarketMintBlockerCode::NoEligibleTarget,
                None,
                "admissible catalog subset contains no reserve inside target safety bounds"
                    .to_owned(),
            );
        }
        blockers.sort_by(|left, right| {
            left.code
                .cmp(&right.code)
                .then_with(|| left.reserve.cmp(&right.reserve))
                .then_with(|| left.detail.cmp(&right.detail))
        });
        blockers.dedup();
        let complete = !mint_wide_blocked && !mint_catalog.is_empty();
        let mint_catalog_expiry = mint_catalog_expiries.into_iter().min();
        let mint_epoch_expiry = mint_catalog_expiry
            .into_iter()
            .chain(mint_verification_expiries.into_iter().min())
            .chain(mint_economic_expiries.into_iter().min())
            .min();
        if complete {
            if let Some(expiry) = mint_catalog_expiry {
                routable_catalog_expiries.push(expiry);
            }
            if let Some(expiry) = mint_epoch_expiry {
                routable_epoch_expiries.push(expiry);
            }
            reserves.extend(candidate_reserves);
        }
        mint_coverage.push(MarketMintCoverage {
            mint: mint.clone(),
            catalog_reserve_count: mint_catalog.len(),
            verified_reserve_count,
            eligible_target_reserve_count,
            complete,
            expires_at: complete.then_some(mint_epoch_expiry).flatten(),
            blockers,
        });
    }

    reserves.sort_by(|left, right| {
        left.liquidity_mint
            .cmp(&right.liquidity_mint)
            .then_with(|| left.reserve.cmp(&right.reserve))
            .then_with(|| left.market.cmp(&right.market))
    });
    mint_coverage.sort_by(|left, right| left.mint.cmp(&right.mint));
    let catalog_expires_at = routable_catalog_expiries
        .into_iter()
        .min()
        .unwrap_or(captured_at);
    let expires_at = routable_epoch_expiries
        .into_iter()
        .min()
        .unwrap_or(captured_at);
    let fingerprint = market_epoch_fingerprint(
        &reserves,
        enabled_mints,
        &catalog_fingerprint,
        &mint_coverage,
    );
    let optimizer_epoch_id = positive_epoch_id(&fingerprint);
    let oldest_market_observed_at = reserves.iter().map(|reserve| reserve.observed_at).min();
    Ok(ImmutableMarketEpoch {
        optimizer_epoch_id,
        fingerprint,
        catalog_fingerprint,
        captured_at,
        expires_at,
        catalog_expires_at,
        catalog_reserve_count,
        oldest_market_observed_at,
        newest_market_observed_at: reserves.iter().map(|reserve| reserve.observed_at).max(),
        minimum_market_slot: reserves.iter().map(|reserve| reserve.slot).min(),
        maximum_market_slot: reserves.iter().map(|reserve| reserve.slot).max(),
        mint_coverage,
        reserves,
    })
}

#[allow(clippy::too_many_arguments)]
fn validate_exact_verified_reserve(
    exact: &VerifiedSupportedReserveRow,
    catalog: &SupportedReserveCatalogRow,
    valuation: Option<&StablecoinValuation>,
    captured_at: DateTime<Utc>,
    publication_minimum: DateTime<Utc>,
    verification_expiry: DateTime<Utc>,
    config: &FleetObservationConfig,
    blockers: &mut Vec<MarketMintBlocker>,
    refreshable_economic_blockers: &mut Vec<MarketMintBlocker>,
) -> Option<DateTime<Utc>> {
    let reserve = Some(catalog.reserve.clone());
    let mut target_economic_expiry = None;
    if exact.reserve != catalog.reserve
        || exact.market.as_deref() != Some(catalog.market.as_str())
        || exact.liquidity_mint != catalog.liquidity_mint
    {
        push_market_blocker(
            blockers,
            MarketMintBlockerCode::VerifiedIdentityMismatch,
            reserve.clone(),
            "verified row does not exactly match catalog reserve/market/mint".to_owned(),
        );
    }
    if exact.verification_source != "http_snapshot"
        && exact.verification_source != "http_confirmed_refresh"
    {
        push_market_blocker(
            blockers,
            MarketMintBlockerCode::VerificationSourceMismatch,
            reserve.clone(),
            format!("verification source is {}", exact.verification_source),
        );
    }
    if exact.verification_commitment != "confirmed" {
        push_market_blocker(
            blockers,
            MarketMintBlockerCode::VerificationCommitmentMismatch,
            reserve.clone(),
            format!(
                "verification commitment is {}",
                exact.verification_commitment
            ),
        );
    }
    if exact.verified_at > captured_at {
        push_market_blocker(
            blockers,
            MarketMintBlockerCode::VerificationInFuture,
            reserve.clone(),
            format!("verified_at {} is in the future", exact.verified_at),
        );
    } else if verification_expiry <= captured_at {
        push_market_blocker(
            blockers,
            MarketMintBlockerCode::VerificationStale,
            reserve.clone(),
            format!("verification expired at {verification_expiry}"),
        );
    } else if verification_expiry <= publication_minimum {
        push_market_blocker(
            blockers,
            MarketMintBlockerCode::VerificationInsufficientLifetime,
            reserve.clone(),
            format!(
                "verification expires at {verification_expiry}; remaining lifetime is below {MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS} seconds"
            ),
        );
    }
    if exact.state_event_id <= 0
        || exact.account_data_hash.len() != 64
        || !exact
            .account_data_hash
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit())
        || exact.state_slot < 0
        || exact.verified_slot < exact.state_slot
    {
        push_market_blocker(
            blockers,
            MarketMintBlockerCode::InvalidStateIdentity,
            reserve.clone(),
            format!(
                "invalid event/hash/state/verification coordinates event={} state_slot={} verified_slot={}",
                exact.state_event_id, exact.state_slot, exact.verified_slot
            ),
        );
    }
    match valuation {
        Some(valuation)
            if exact.mint_decimals != i32::from(valuation.decimals)
                || !(0..=18).contains(&exact.mint_decimals) =>
        {
            push_market_blocker(
                blockers,
                MarketMintBlockerCode::MintDecimalsMismatch,
                reserve.clone(),
                format!(
                    "verified mint decimals {} differ from code-owned {}",
                    exact.mint_decimals, valuation.decimals
                ),
            );
        }
        None => push_market_blocker(
            blockers,
            MarketMintBlockerCode::MissingStableValuation,
            reserve.clone(),
            "verified reserve has no code-owned stable valuation".to_owned(),
        ),
        Some(_) => {}
    }
    if exact.reserve_last_update_stale {
        push_market_blocker(
            refreshable_economic_blockers,
            MarketMintBlockerCode::ExplicitStaleEconomics,
            reserve.clone(),
            "reserve last_update.stale is set; admitted as refresh-before-withdraw source only"
                .to_owned(),
        );
    }
    if exact.reserve_last_update_slot < 0
        || exact.reserve_last_update_slot > exact.state_slot
        || exact.reserve_last_update_slot > exact.verified_slot
    {
        push_market_blocker(
            blockers,
            MarketMintBlockerCode::InvalidEconomicSlotOrder,
            reserve.clone(),
            format!(
                "last_update_slot={} state_slot={} verified_slot={}",
                exact.reserve_last_update_slot, exact.state_slot, exact.verified_slot
            ),
        );
    } else {
        let slot_lag = exact.verified_slot - exact.reserve_last_update_slot;
        if slot_lag > MAXIMUM_RESERVE_ECONOMIC_SLOT_LAG {
            push_market_blocker(
                refreshable_economic_blockers,
                MarketMintBlockerCode::EconomicSlotLagExceeded,
                reserve.clone(),
                format!(
                    "economic slot lag {slot_lag} exceeds {}; admitted as refresh-before-withdraw source only",
                    MAXIMUM_RESERVE_ECONOMIC_SLOT_LAG
                ),
            );
        } else if let Ok(economic_expiry) = reserve_economic_expires_at(exact) {
            if economic_expiry <= publication_minimum {
                push_market_blocker(
                    refreshable_economic_blockers,
                    MarketMintBlockerCode::EconomicInsufficientLifetime,
                    reserve.clone(),
                    format!(
                        "economic evidence expires at {economic_expiry}; remaining lifetime is below {MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS} seconds; admitted as refresh-before-withdraw source only"
                    ),
                );
            } else if !exact.reserve_last_update_stale {
                target_economic_expiry = Some(economic_expiry);
            }
        }
    }
    let economics_valid = exact.available_amount.is_finite()
        && exact.available_amount >= 0.0
        && exact.borrowed_amount.is_finite()
        && exact.borrowed_amount >= 0.0
        && exact.total_supply_amount.is_finite()
        && exact.total_supply_amount > 0.0
        && exact.market_price_usd.is_finite()
        && exact.utilization.is_finite()
        && (0.0..=1.000_001).contains(&exact.utilization)
        && exact.borrow_apy.is_finite()
        && exact.borrow_apy >= 0.0
        && exact.supply_apy.is_finite()
        && exact.supply_apy >= 0.0
        && exact.supply_apy < config.maximum_supply_apy;
    if !economics_valid {
        push_market_blocker(
            blockers,
            MarketMintBlockerCode::InvalidEconomicFields,
            reserve,
            format!(
                "invalid available={} borrowed={} total_supply={} utilization={} borrow_apy={} supply_apy={}",
                exact.available_amount,
                exact.borrowed_amount,
                exact.total_supply_amount,
                exact.utilization,
                exact.borrow_apy,
                exact.supply_apy,
            ),
        );
    }
    target_economic_expiry
}

fn push_market_blocker(
    blockers: &mut Vec<MarketMintBlocker>,
    code: MarketMintBlockerCode,
    reserve: Option<String>,
    detail: String,
) {
    blockers.push(MarketMintBlocker {
        code,
        reserve,
        detail,
    });
}

fn supported_reserve_catalog_fingerprint(catalog: &[SupportedReserveCatalogRow]) -> String {
    let mut hasher = Sha256::new();
    for row in catalog {
        hash_part(&mut hasher, row.market.as_bytes());
        hash_part(&mut hasher, row.liquidity_mint.as_bytes());
        hash_part(&mut hasher, row.reserve.as_bytes());
        hash_part(
            &mut hasher,
            row.market_name.as_deref().unwrap_or_default().as_bytes(),
        );
        hash_part(
            &mut hasher,
            row.symbol.as_deref().unwrap_or_default().as_bytes(),
        );
        let mut risk_baskets = row.risk_baskets.clone();
        risk_baskets.sort();
        for risk_basket in risk_baskets {
            hash_part(&mut hasher, risk_basket.as_bytes());
        }
        hash_part(&mut hasher, row.source.as_bytes());
        hash_part(
            &mut hasher,
            &row.fetched_at.timestamp_micros().to_le_bytes(),
        );
    }
    format!("{:x}", hasher.finalize())
}

fn stable_supply_to_usd_micros(
    total_supply_amount: f64,
    valuation: &StablecoinValuation,
) -> Result<i64, FleetObservationError> {
    if !total_supply_amount.is_finite()
        || total_supply_amount <= 0.0
        || valuation.price_usd_micros <= 0
        || valuation.decimals > 18
    {
        return Err(FleetObservationError::ArithmeticOverflow);
    }
    let raw_units_per_token = 10_f64.powi(i32::from(valuation.decimals));
    let value = total_supply_amount * valuation.price_usd_micros as f64 / raw_units_per_token;
    if !value.is_finite() || value < 0.0 || value > i64::MAX as f64 {
        return Err(FleetObservationError::ArithmeticOverflow);
    }
    Ok(value.round() as i64)
}

fn reserve_economic_expires_at(
    exact: &VerifiedSupportedReserveRow,
) -> Result<DateTime<Utc>, FleetObservationError> {
    let lag = exact
        .verified_slot
        .checked_sub(exact.reserve_last_update_slot)
        .ok_or(FleetObservationError::ArithmeticOverflow)?;
    let remaining_slots = MAXIMUM_RESERVE_ECONOMIC_SLOT_LAG
        .checked_sub(lag)
        .ok_or(FleetObservationError::ArithmeticOverflow)?;
    let remaining_millis = remaining_slots
        .checked_mul(RESERVE_ECONOMIC_EXPIRY_MILLIS_PER_SLOT)
        .ok_or(FleetObservationError::ArithmeticOverflow)?;
    exact
        .verified_at
        .checked_add_signed(Duration::milliseconds(remaining_millis))
        .ok_or(FleetObservationError::ArithmeticOverflow)
}

fn ratio_to_ppm(value: f64) -> Result<i64, FleetObservationError> {
    if !value.is_finite() || !(0.0..=1.000_001).contains(&value) {
        return Err(FleetObservationError::ArithmeticOverflow);
    }
    Ok((value * 1_000_000.0).round() as i64)
}

fn canonical_f64(value: f64) -> String {
    value.to_string()
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct FleetSourceRow {
    vault_id: i64,
    settings: String,
    vault_index: i16,
    vault_pubkey: String,
    policy_id: i64,
    base_policy_account: String,
    base_policy_delegated_signer: String,
    base_policy_cluster: String,
    base_policy_source_commitment: String,
    base_policy_finalized_eligible: bool,
    base_policy_observed_slot: i64,
    base_policy_observed_signature: String,
    policy_authority: String,
    policy_markets: Vec<String>,
    policy_stable_mints: Vec<String>,
    policy_liquidity_mints: Vec<String>,
    policy_route_modes: Vec<String>,
    earn_policy_evidence: Vec<ObservedEarnPolicyEvidence>,
    cross_mint_swap_policies: Vec<CrossMintSwapPolicyEvidence>,
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

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct CrossMintSwapPolicyEvidence {
    settings: String,
    authority: String,
    policy_account: String,
    vault_index: i16,
    vault_pubkey: String,
    delegated_signer: String,
    source_shard: String,
    max_slippage_bps: i32,
    daily_source_mint_spending_cap: i64,
    manifest_fingerprint: String,
    active: bool,
    start_eligible: bool,
    source_commitment: String,
    last_seen_slot: i64,
    last_seen_signature: String,
}

struct FleetSourceSet {
    eligible_vault_count: i64,
    source_candidate_vault_count: i64,
    active_opportunity_vaults_excluded: i64,
    active_opportunity_vaults_excluded_by_state: BTreeMap<String, i64>,
    no_positive_current_source_vault_count: i64,
    sources: Vec<FleetSourceRow>,
    committed_target_inflows: Vec<CommittedTargetInflow>,
    committed_source_outflows: Vec<CommittedSourceOutflow>,
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
    delegated_signer: &str,
    config: &FleetObservationConfig,
    enabled_mints: &[String],
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
            SELECT id, vault_id, source_reserve, target_reserve, liquidity_mint,
                   principal_usd_micros, opportunity_state, lease_kind
            FROM loyal_yield.rebalance_opportunities
            WHERE cluster = $7
              AND opportunity_state IN (
                'waiting_alt', 'revalidate', 'ready', 'leased', 'decision_created'
            )
              AND (
                  expires_at > $6::TIMESTAMPTZ
                  OR opportunity_state IN ('leased', 'decision_created')
              )
        ),
        live_capacity_reservations AS (
            -- Execution admission is authoritative even after the queue row
            -- becomes terminal. In particular, reconciled flow remains here
            -- as awaiting_telemetry until a strictly newer target observation
            -- proves that market supply reflects the movement.
            SELECT reservation.opportunity_id,
                   opportunity.source_reserve,
                   reservation.target_reserve,
                   reservation.liquidity_mint,
                   reservation.principal_usd_micros
            FROM loyal_yield.target_capacity_reservations reservation
            JOIN loyal_yield.rebalance_opportunities opportunity
              ON opportunity.id = reservation.opportunity_id
            WHERE reservation.cluster = $7
              AND reservation.reservation_state <> 'released'
        ),
        committed_route_flows AS (
            -- Count every durable reservation exactly once and never subtract
            -- it merely because its vault belongs to a scoped dirty cohort.
            SELECT source_reserve, target_reserve, principal_usd_micros
            FROM live_capacity_reservations

            UNION ALL

            -- Pre-execution intent has not acquired an execution-time
            -- reservation yet, but must still consume projected planner
            -- headroom. Scoped replacement removes only its own replaceable
            -- waiting/revalidate/ready intents. Leased/decision-backed work is
            -- already executing and remains committed even before the narrow
            -- reservation handoff completes.
            SELECT opportunity.source_reserve,
                   opportunity.target_reserve,
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
                          $8::BIGINT[] IS NULL
                          OR NOT (opportunity.vault_id = ANY($8::BIGINT[]))
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
                p.policy_account AS base_policy_account,
                $1::TEXT AS base_policy_delegated_signer,
                p.cluster AS base_policy_cluster,
                p.source_commitment AS base_policy_source_commitment,
                p.finalized_eligible AS base_policy_finalized_eligible,
                p.last_seen_slot AS base_policy_observed_slot,
                p.last_seen_signature AS base_policy_observed_signature,
                p.authority AS policy_authority,
                p.kamino_markets AS policy_markets,
                p.stable_mints AS policy_stable_mints,
                p.kamino_liquidity_mints AS policy_liquidity_mints,
                p.route_modes AS policy_route_modes,
                COALESCE((
                    SELECT jsonb_agg(
                        jsonb_build_object(
                            'settings', earn_policy.settings,
                            'authority', earn_policy.authority,
                            'policyAccount', earn_policy.policy_account,
                            'vaultIndex', earn_policy.vault_index,
                            'vaultPubkey', earn_policy.vault_pubkey,
                            'delegatedSigner', $1::TEXT,
                            'threshold', earn_policy.threshold,
                            'stableMints', earn_policy.stable_mints,
                            'kaminoMarkets', earn_policy.kamino_markets,
                            'kaminoLiquidityMints', earn_policy.kamino_liquidity_mints,
                            'sourceCommitment', earn_policy.source_commitment,
                            'observedSlot', earn_policy.last_seen_slot,
                            'observedSignature', earn_policy.last_seen_signature
                        )
                        ORDER BY earn_policy.last_seen_slot DESC,
                                 earn_policy.policy_account
                    )
                    FROM loyal_yield.route_policies earn_policy
                    WHERE $9::BOOLEAN
                      AND earn_policy.active = TRUE
                      AND earn_policy.finalized_eligible = TRUE
                      AND earn_policy.source_commitment = 'finalized'
                      AND earn_policy.cluster = $7
                      AND earn_policy.settings = v.settings
                      AND earn_policy.authority = p.authority
                      AND earn_policy.vault_index = v.vault_index
                      AND earn_policy.vault_pubkey = v.vault_pubkey
                      AND $1 = ANY(earn_policy.delegated_signers)
                      AND $3 = ANY(earn_policy.route_modes)
                ), '[]'::JSONB) AS earn_policy_evidence,
                COALESCE((
                    SELECT jsonb_agg(
                        jsonb_build_object(
                            'settings', swap_policy.settings,
                            'authority', swap_policy.authority,
                            'policyAccount', swap_policy.policy_account,
                            'vaultIndex', swap_policy.vault_index,
                            'vaultPubkey', swap_policy.vault_pubkey,
                            'delegatedSigner', swap_policy.delegated_signer,
                            'sourceShard', swap_policy.source_shard,
                            'maxSlippageBps', swap_policy.max_slippage_bps,
                            'dailySourceMintSpendingCap',
                                swap_policy.daily_source_mint_spending_cap,
                            'manifestFingerprint', swap_policy.manifest_fingerprint,
                            'active', swap_policy.active,
                            'startEligible', swap_policy.start_eligible,
                            'sourceCommitment', swap_policy.source_commitment,
                            'lastSeenSlot', swap_policy.last_seen_slot,
                            'lastSeenSignature', swap_policy.last_seen_signature
                        )
                        ORDER BY swap_policy.source_shard,
                                 swap_policy.last_seen_slot DESC,
                                 swap_policy.policy_account
                    )
                    FROM loyal_yield.cross_mint_swap_policies swap_policy
                    WHERE $9::BOOLEAN
                      AND swap_policy.active = TRUE
                      AND swap_policy.start_eligible = TRUE
                      AND swap_policy.source_commitment = 'finalized'
                      AND swap_policy.cluster = $7
                      AND swap_policy.cluster = p.cluster
                      AND swap_policy.settings = v.settings
                      AND swap_policy.authority = p.authority
                      AND swap_policy.vault_index = v.vault_index
                      AND swap_policy.vault_pubkey = v.vault_pubkey
                      AND swap_policy.delegated_signer = $1
                      AND EXISTS (
                          SELECT 1
                          FROM loyal_yield.cross_mint_vault_opt_ins opt_in
                          WHERE opt_in.enabled = TRUE
                            AND opt_in.cluster = swap_policy.cluster
                            AND opt_in.settings = swap_policy.settings
                            AND opt_in.vault_index = swap_policy.vault_index
                            AND opt_in.vault_pubkey = swap_policy.vault_pubkey
                            AND CASE swap_policy.source_shard
                                WHEN 'classic' THEN
                                    opt_in.classic_policy_account = swap_policy.policy_account
                                    AND opt_in.classic_policy_seed = swap_policy.policy_seed
                                WHEN 'token_2022' THEN
                                    opt_in.token_2022_policy_account = swap_policy.policy_account
                                    AND opt_in.token_2022_policy_seed = swap_policy.policy_seed
                                ELSE FALSE
                            END
                            AND opt_in.max_slippage_bps = swap_policy.max_slippage_bps
                            AND opt_in.daily_source_mint_spending_cap =
                                swap_policy.daily_source_mint_spending_cap
                      )
                      AND 2 = (
                          SELECT count(DISTINCT policy.source_shard)
                          FROM loyal_yield.cross_mint_swap_policies policy
                          WHERE policy.cluster = swap_policy.cluster
                            AND policy.settings = swap_policy.settings
                            AND policy.authority = swap_policy.authority
                            AND policy.vault_index = swap_policy.vault_index
                            AND policy.vault_pubkey = swap_policy.vault_pubkey
                            AND policy.delegated_signer = swap_policy.delegated_signer
                            AND policy.active = TRUE
                            AND policy.start_eligible = TRUE
                            AND policy.source_commitment = 'finalized'
                            AND policy.last_mutation IN ('create', 'update')
                            AND policy.max_slippage_bps = swap_policy.max_slippage_bps
                            AND policy.daily_source_mint_spending_cap =
                                swap_policy.daily_source_mint_spending_cap
                            AND EXISTS (
                                SELECT 1
                                FROM loyal_yield.cross_mint_vault_opt_ins opt_in
                                WHERE opt_in.enabled = TRUE
                                  AND opt_in.cluster = policy.cluster
                                  AND opt_in.settings = policy.settings
                                  AND opt_in.vault_index = policy.vault_index
                                  AND opt_in.vault_pubkey = policy.vault_pubkey
                                  AND CASE policy.source_shard
                                      WHEN 'classic' THEN
                                          opt_in.classic_policy_account = policy.policy_account
                                          AND opt_in.classic_policy_seed = policy.policy_seed
                                      WHEN 'token_2022' THEN
                                          opt_in.token_2022_policy_account = policy.policy_account
                                          AND opt_in.token_2022_policy_seed = policy.policy_seed
                                      ELSE FALSE
                                  END
                            )
                      )
                      ), '[]'::JSONB) AS cross_mint_swap_policies
            FROM loyal_yield.managed_vaults v
            JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
            WHERE v.active = TRUE
              AND p.active = TRUE
              AND ($8::BIGINT[] IS NULL OR v.id = ANY($8::BIGINT[]))
              AND $1 = ANY(p.delegated_signers)
              AND $3 = ANY(p.route_modes)
              AND p.cluster = $7
              AND p.source_commitment = 'finalized'
              AND p.finalized_eligible = TRUE
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
            WHERE $8::BIGINT[] IS NULL
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
                        AND recent.updated_at >= $6::TIMESTAMPTZ - ($5::DOUBLE PRECISION * INTERVAL '1 second')
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
                        AND recent.updated_at >= $6::TIMESTAMPTZ - ($5::DOUBLE PRECISION * INTERVAL '1 second')
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
                        'basePolicyAccount', source.base_policy_account,
                        'basePolicyDelegatedSigner', source.base_policy_delegated_signer,
                        'basePolicyCluster', source.base_policy_cluster,
                        'basePolicySourceCommitment', source.base_policy_source_commitment,
                        'basePolicyFinalizedEligible', source.base_policy_finalized_eligible,
                        'basePolicyObservedSlot', source.base_policy_observed_slot,
                        'basePolicyObservedSignature', source.base_policy_observed_signature,
                        'policyAuthority', source.policy_authority,
                        'policyMarkets', source.policy_markets,
                        'policyStableMints', source.policy_stable_mints,
                        'policyLiquidityMints', source.policy_liquidity_mints,
                        'policyRouteModes', source.policy_route_modes,
                        'earnPolicyEvidence', source.earn_policy_evidence,
                        'crossMintSwapPolicies', source.cross_mint_swap_policies,
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
                    FROM committed_route_flows
                    GROUP BY target_reserve
                ) committed),
                '[]'::JSONB
            ) AS committed_target_inflows,
            COALESCE(
                (SELECT jsonb_agg(
                    jsonb_build_object(
                        'sourceReserve', committed.source_reserve,
                        'principalUsdMicros', committed.principal_usd_micros
                    ) ORDER BY committed.source_reserve
                )
                FROM (
                    SELECT source_reserve,
                           sum(principal_usd_micros)::BIGINT AS principal_usd_micros
                    FROM committed_route_flows
                    WHERE source_reserve IS NOT NULL
                    GROUP BY source_reserve
                ) committed),
                '[]'::JSONB
            ) AS committed_source_outflows
        "#,
    )
    .bind(delegated_signer)
    .bind(enabled_mints)
    .bind(SAME_MINT_ROUTE_MODE)
    .bind(active_statuses)
    .bind(config.rebalance_cooldown_seconds)
    .bind(captured_at)
    .bind(&config.cluster)
    .bind(vault_ids.map(|ids| ids.to_vec()))
    .bind(config.enable_cross_mint_jupiter)
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
    let committed_source_outflows_json: Value = row
        .try_get("committed_source_outflows")
        .map_err(FleetObservationError::NeonRead)?;
    let committed_source_outflows = serde_json::from_value(committed_source_outflows_json)
        .map_err(FleetObservationError::RowDecode)?;
    Ok(FleetSourceSet {
        eligible_vault_count,
        source_candidate_vault_count,
        active_opportunity_vaults_excluded,
        active_opportunity_vaults_excluded_by_state,
        no_positive_current_source_vault_count,
        sources,
        committed_target_inflows,
        committed_source_outflows,
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
                p.policy_account AS base_policy_account,
                $1::TEXT AS base_policy_delegated_signer,
                'legacy-unverified'::TEXT AS base_policy_cluster,
                'unknown'::TEXT AS base_policy_source_commitment,
                FALSE AS base_policy_finalized_eligible,
                p.last_seen_slot AS base_policy_observed_slot,
                p.last_seen_signature AS base_policy_observed_signature,
                p.authority AS policy_authority,
                p.kamino_markets AS policy_markets,
                p.stable_mints AS policy_stable_mints,
                p.kamino_liquidity_mints AS policy_liquidity_mints,
                p.route_modes AS policy_route_modes,
                '[]'::JSONB AS earn_policy_evidence,
                '[]'::JSONB AS cross_mint_swap_policies
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
                        AND recent.updated_at >= $6::TIMESTAMPTZ - ($5::DOUBLE PRECISION * INTERVAL '1 second')
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
                        AND recent.updated_at >= $6::TIMESTAMPTZ - ($5::DOUBLE PRECISION * INTERVAL '1 second')
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
                        'basePolicyAccount', source.base_policy_account,
                        'basePolicyDelegatedSigner', source.base_policy_delegated_signer,
                        'basePolicyCluster', source.base_policy_cluster,
                        'basePolicySourceCommitment', source.base_policy_source_commitment,
                        'basePolicyFinalizedEligible', source.base_policy_finalized_eligible,
                        'basePolicyObservedSlot', source.base_policy_observed_slot,
                        'basePolicyObservedSignature', source.base_policy_observed_signature,
                        'policyAuthority', source.policy_authority,
                        'policyMarkets', source.policy_markets,
                        'policyStableMints', source.policy_stable_mints,
                        'policyLiquidityMints', source.policy_liquidity_mints,
                        'policyRouteModes', source.policy_route_modes,
                        'earnPolicyEvidence', source.earn_policy_evidence,
                        'crossMintSwapPolicies', source.cross_mint_swap_policies,
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
        committed_source_outflows: Vec::new(),
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
        market_catalog_fingerprint: market_epoch.catalog_fingerprint.clone(),
        market_catalog_reserve_count: market_epoch.catalog_reserve_count,
        complete_market_mint_count: market_epoch
            .mint_coverage
            .iter()
            .filter(|coverage| coverage.complete)
            .count(),
        blocked_market_mint_count: market_epoch
            .mint_coverage
            .iter()
            .filter(|coverage| !coverage.complete)
            .count(),
        market_mint_coverage: market_epoch.mint_coverage.clone(),
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
        committed_source_outflow_reserve_count: source_set.committed_source_outflows.len(),
        committed_source_outflow_usd_micros: source_set
            .committed_source_outflows
            .iter()
            .fold(0i64, |total, outflow| {
                total.saturating_add(outflow.principal_usd_micros)
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
        let (source_apy_bps, source_market) = match source_kind {
            ObservedSourceKind::IdleVaultUsdc => (0, None),
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
                (
                    source_epoch_reserve.supply_apy_bps,
                    source_epoch_reserve.market.as_deref(),
                )
            }
        };
        let mut targets = Vec::new();
        if policy_authorizes_route_mode(&source, SAME_MINT_ROUTE_MODE) {
            targets.extend(
                policy_targets(&source, by_mint.get(source.liquidity_mint.as_str()))
                    .into_iter()
                    .filter(|target| target.supply_apy_bps > source_apy_bps)
                    .map(|target| {
                        let route_kind = match source_kind {
                            ObservedSourceKind::ReservePosition => CandidateRouteKind::SameMint,
                            ObservedSourceKind::IdleVaultUsdc => {
                                CandidateRouteKind::IdleVaultDeposit
                            }
                        };
                        FleetCandidateTarget {
                            route_kind,
                            target,
                            jupiter_swap_lane: None,
                            source_earn_policy: None,
                            target_earn_policy: None,
                        }
                    }),
            );
        }
        if source_kind == ObservedSourceKind::ReservePosition && config.enable_cross_mint_jupiter {
            for target_asset in earn_stablecoins() {
                let target_mint = target_asset.mint.to_string();
                let Some(capability) =
                    cross_mint_policy_selection(&source, &config.cluster, target_mint.as_str())
                else {
                    continue;
                };
                let Some(source_market) = source_market else {
                    continue;
                };
                let jupiter_swap_lane = jupiter_swap_lane(capability);
                targets.extend(
                    cross_mint_policy_targets(
                        &source,
                        source_market,
                        target_mint.as_str(),
                        by_mint.get(target_mint.as_str()),
                    )
                    .into_iter()
                    .filter(|target| target.target.supply_apy_bps > source_apy_bps)
                    .map(|target| FleetCandidateTarget {
                        route_kind: CandidateRouteKind::CrossMintJupiter,
                        target: target.target,
                        jupiter_swap_lane: Some(jupiter_swap_lane.clone()),
                        source_earn_policy: Some(target.source_policy),
                        target_earn_policy: Some(target.target_policy),
                    }),
                );
            }
        }
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
        let source_snapshot_id = source
            .source_snapshot_id
            .unwrap_or(source.observed_slot)
            .max(1);
        opportunity_vault_ids.insert(source.vault_id);
        for candidate in targets {
            let FleetCandidateTarget {
                route_kind,
                target,
                jupiter_swap_lane,
                source_earn_policy,
                target_earn_policy,
            } = candidate;
            let estimated_execution_costs = match route_kind {
                CandidateRouteKind::SameMint => CandidateExecutionCosts::SameMint {
                    route_usd_micros: config.estimated_reserve_move_cost_usd_micros,
                },
                CandidateRouteKind::IdleVaultDeposit => CandidateExecutionCosts::IdleVaultDeposit {
                    deposit_usd_micros: config.estimated_idle_deposit_cost_usd_micros,
                },
                CandidateRouteKind::CrossMintJupiter => CandidateExecutionCosts::CrossMintJupiter {
                    withdraw_usd_micros: config.estimated_cross_mint_withdraw_cost_usd_micros,
                    jupiter_swap_usd_micros: config
                        .estimated_cross_mint_jupiter_swap_cost_usd_micros,
                    deposit_usd_micros: config.estimated_cross_mint_deposit_cost_usd_micros,
                },
            };
            let estimated_cost = estimated_execution_costs
                .total_usd_micros()
                .ok_or(FleetObservationError::ArithmeticOverflow)?;
            let expected_service_millis = match route_kind {
                CandidateRouteKind::SameMint => config.expected_reserve_move_service_millis,
                CandidateRouteKind::IdleVaultDeposit => config.expected_idle_deposit_service_millis,
                CandidateRouteKind::CrossMintJupiter => config
                    .expected_reserve_move_service_millis
                    .checked_mul(3)
                    .ok_or(FleetObservationError::ArithmeticOverflow)?,
            };
            let target_valuation = valuations.get(&target.liquidity_mint).ok_or_else(|| {
                FleetObservationError::InvalidConfig(format!(
                    "missing code-owned target valuation for mint {}",
                    target.liquidity_mint
                ))
            })?;
            let mut writable_conflict_keys = vec![
                format!("vault:{}", source.vault_pubkey),
                format!("policy:{}", source.policy_id),
                format!(
                    "source-reserve:{}",
                    source.source_reserve.as_deref().unwrap_or("idle")
                ),
                format!("target-reserve:{}", target.reserve),
            ];
            if let Some(lane) = jupiter_swap_lane.as_ref() {
                writable_conflict_keys.push(format!("swap-policy:{}", lane.policy_account));
            }
            if let Some(source_policy) = source_earn_policy.as_ref() {
                writable_conflict_keys
                    .push(format!("earn-policy:{}", source_policy.policy_account));
            }
            if let Some(target_policy) = target_earn_policy.as_ref() {
                if source_earn_policy.as_ref().is_none_or(|source_policy| {
                    source_policy.policy_account != target_policy.policy_account
                }) {
                    writable_conflict_keys
                        .push(format!("earn-policy:{}", target_policy.policy_account));
                }
            }
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
                    confidence_ppm: valuation
                        .confidence_ppm
                        .min(target_valuation.confidence_ppm),
                    expected_service_millis,
                    holding_horizon_seconds: config.holding_horizon_seconds,
                    estimated_execution_cost_usd_micros: estimated_cost,
                    age_seconds,
                    fairness_credit,
                    writable_conflict_keys,
                },
                route_kind,
                source_liquidity_mint: source.liquidity_mint.clone(),
                target_liquidity_mint: target.liquidity_mint.clone(),
                estimated_execution_costs,
                cross_mint_maximum_value_loss_bps: matches!(
                    route_kind,
                    CandidateRouteKind::CrossMintJupiter
                )
                .then_some(config.cross_mint_maximum_value_loss_bps),
                jupiter_swap_lane,
                source_earn_policy,
                target_earn_policy,
                base_policy_account: source.base_policy_account.clone(),
                base_policy_delegated_signer: source.base_policy_delegated_signer.clone(),
                base_policy_source_commitment: source.base_policy_source_commitment.clone(),
                base_policy_observed_slot: source.base_policy_observed_slot,
                base_policy_observed_signature: source.base_policy_observed_signature.clone(),
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
        committed_source_outflows: source_set.committed_source_outflows,
        stats,
    })
}

struct FleetCandidateTarget<'a> {
    route_kind: CandidateRouteKind,
    target: &'a MarketEpochReserve,
    jupiter_swap_lane: Option<CrossMintSwapPolicyBinding>,
    source_earn_policy: Option<ObservedEarnPolicyEvidence>,
    target_earn_policy: Option<ObservedEarnPolicyEvidence>,
}

struct CrossMintEarnTarget<'a> {
    target: &'a MarketEpochReserve,
    source_policy: ObservedEarnPolicyEvidence,
    target_policy: ObservedEarnPolicyEvidence,
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
            target.target_eligible
                && source
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

fn cross_mint_policy_targets<'a>(
    source: &FleetSourceRow,
    source_market: &str,
    target_mint: &str,
    reserves: Option<&Vec<&'a MarketEpochReserve>>,
) -> Vec<CrossMintEarnTarget<'a>> {
    let mut targets = reserves
        .map(Vec::as_slice)
        .unwrap_or_default()
        .iter()
        .copied()
        .filter_map(|target| {
            let target_market = target.market.as_deref()?;
            let (source_policy, target_policy) =
                exact_cross_mint_earn_bindings(source, source_market, target_mint, target_market)?;
            (target.target_eligible
                && source
                    .source_reserve
                    .as_ref()
                    .is_none_or(|source_reserve| target.reserve != *source_reserve))
            .then(|| CrossMintEarnTarget {
                target,
                source_policy: source_policy.clone(),
                target_policy: target_policy.clone(),
            })
        })
        .collect::<Vec<_>>();
    targets.sort_by(|left, right| {
        right
            .target
            .supply_apy_bps
            .cmp(&left.target.supply_apy_bps)
            .then_with(|| right.target.observed_at.cmp(&left.target.observed_at))
            .then_with(|| right.target.slot.cmp(&left.target.slot))
            .then_with(|| left.target.reserve.cmp(&right.target.reserve))
    });
    targets
}

fn exact_cross_mint_earn_bindings<'a>(
    source: &'a FleetSourceRow,
    source_market: &str,
    target_mint: &str,
    target_market: &str,
) -> Option<(
    &'a ObservedEarnPolicyEvidence,
    &'a ObservedEarnPolicyEvidence,
)> {
    let source_policy = exact_earn_policy(source, &source.liquidity_mint, source_market)?;
    let target_policy = exact_earn_policy(source, target_mint, target_market)?;
    Some((source_policy, target_policy))
}

fn exact_earn_policy<'a>(
    source: &'a FleetSourceRow,
    mint: &str,
    market: &str,
) -> Option<&'a ObservedEarnPolicyEvidence> {
    source
        .earn_policy_evidence
        .iter()
        .filter(|policy| {
            policy.settings == source.settings
                && policy.authority == source.policy_authority
                && policy.vault_index == source.vault_index
                && policy.vault_pubkey == source.vault_pubkey
                && policy.delegated_signer == source.base_policy_delegated_signer
                && policy.source_commitment == "finalized"
                && policy.threshold == 1
                && policy.stable_mints.iter().any(|allowed| allowed == mint)
                && policy
                    .kamino_liquidity_mints
                    .iter()
                    .any(|allowed| allowed == mint)
                && policy
                    .kamino_markets
                    .iter()
                    .any(|allowed| allowed == market)
        })
        .min_by(|left, right| {
            right
                .observed_slot
                .cmp(&left.observed_slot)
                .then_with(|| left.policy_account.cmp(&right.policy_account))
        })
}

fn policy_authorizes_route_mode(source: &FleetSourceRow, route_mode: &str) -> bool {
    source
        .policy_route_modes
        .iter()
        .any(|allowed| allowed == route_mode)
}

fn cross_mint_policy_selection<'a>(
    source: &'a FleetSourceRow,
    cluster: &str,
    target_mint: &str,
) -> Option<&'a CrossMintSwapPolicyEvidence> {
    if !source.base_policy_finalized_eligible
        || source.base_policy_cluster != cluster
        || source.base_policy_source_commitment != "finalized"
        || source.liquidity_mint == target_mint
    {
        return None;
    }
    let source_asset = earn_stablecoins()
        .iter()
        .find(|asset| asset.mint.to_string() == source.liquidity_mint)?;
    earn_stablecoins()
        .iter()
        .find(|asset| asset.mint.to_string() == target_mint)?;
    let required_shard = if source_asset.token_program == spl_token::ID {
        "classic"
    } else {
        "token_2022"
    };
    let policies = source
        .cross_mint_swap_policies
        .iter()
        .filter(|policy| {
            policy.active
                && policy.start_eligible
                && policy.source_commitment == "finalized"
                && policy.daily_source_mint_spending_cap > 0
                && policy.max_slippage_bps > 0
                && policy.manifest_fingerprint.len() == 64
                && policy.settings == source.settings
                && policy.authority == source.policy_authority
                && policy.vault_index == source.vault_index
                && policy.vault_pubkey == source.vault_pubkey
                && policy.delegated_signer == source.base_policy_delegated_signer
        })
        .collect::<Vec<_>>();
    if policies.len() != 2
        || policies
            .iter()
            .filter(|policy| policy.source_shard == "classic")
            .count()
            != 1
        || policies
            .iter()
            .filter(|policy| policy.source_shard == "token_2022")
            .count()
            != 1
    {
        return None;
    }
    let configured_slippage_bps = policies[0].max_slippage_bps;
    let configured_daily_cap = policies[0].daily_source_mint_spending_cap;
    if policies.iter().any(|policy| {
        policy.max_slippage_bps != configured_slippage_bps
            || policy.daily_source_mint_spending_cap != configured_daily_cap
    }) {
        return None;
    }
    policies
        .into_iter()
        .find(|policy| policy.source_shard == required_shard)
}

fn jupiter_swap_lane(policy: &CrossMintSwapPolicyEvidence) -> CrossMintSwapPolicyBinding {
    CrossMintSwapPolicyBinding {
        policy_account: policy.policy_account.clone(),
        source_shard: policy.source_shard.clone(),
        observed_slot: u64::try_from(policy.last_seen_slot)
            .expect("stored swap policy slot must be nonnegative"),
        observed_signature: policy.last_seen_signature.clone(),
        source_commitment: policy.source_commitment.clone(),
        max_slippage_bps: u16::try_from(policy.max_slippage_bps)
            .expect("stored swap policy slippage must fit u16"),
        daily_source_mint_spending_cap: u64::try_from(policy.daily_source_mint_spending_cap)
            .expect("stored swap policy cap must be nonnegative"),
        manifest_fingerprint: policy.manifest_fingerprint.clone(),
    }
}

fn source_kind_rank(kind: ObservedSourceKind) -> u8 {
    match kind {
        ObservedSourceKind::IdleVaultUsdc => 0,
        ObservedSourceKind::ReservePosition => 1,
    }
}

fn market_epoch_fingerprint(
    reserves: &[MarketEpochReserve],
    enabled_mints: &[String],
    catalog_fingerprint: &str,
    mint_coverage: &[MarketMintCoverage],
) -> String {
    let mut hasher = Sha256::new();
    hash_part(&mut hasher, MARKET_EPOCH_FINGERPRINT_DOMAIN);
    hash_part(&mut hasher, catalog_fingerprint.as_bytes());
    for mint in enabled_mints {
        hash_part(&mut hasher, mint.as_bytes());
    }
    for coverage in mint_coverage {
        hash_part(&mut hasher, coverage.mint.as_bytes());
        hash_part(
            &mut hasher,
            &u64::try_from(coverage.catalog_reserve_count)
                .unwrap_or(u64::MAX)
                .to_le_bytes(),
        );
        hash_part(
            &mut hasher,
            &u64::try_from(coverage.verified_reserve_count)
                .unwrap_or(u64::MAX)
                .to_le_bytes(),
        );
        hash_part(
            &mut hasher,
            &u64::try_from(coverage.eligible_target_reserve_count)
                .unwrap_or(u64::MAX)
                .to_le_bytes(),
        );
        hash_part(&mut hasher, &[u8::from(coverage.complete)]);
        hash_part(
            &mut hasher,
            &coverage
                .expires_at
                .map(|expires_at| expires_at.timestamp_micros())
                .unwrap_or_default()
                .to_le_bytes(),
        );
        for blocker in &coverage.blockers {
            hash_part(&mut hasher, &[market_blocker_code_rank(blocker.code)]);
            hash_part(
                &mut hasher,
                blocker.reserve.as_deref().unwrap_or_default().as_bytes(),
            );
            // The detail is persisted verbatim inside the immutable epoch
            // evidence, so it must be part of the key that claims that
            // evidence. Blocked reserves are excluded from `reserves`, which
            // makes this the only channel through which their observation
            // reaches the durable row.
            hash_part(&mut hasher, blocker.detail.as_bytes());
        }
    }
    for reserve in reserves {
        hash_part(&mut hasher, &reserve.state_event_id.to_le_bytes());
        hash_part(&mut hasher, reserve.account_data_hash.as_bytes());
        hash_part(
            &mut hasher,
            &reserve.state_observed_at.timestamp_micros().to_le_bytes(),
        );
        hash_part(&mut hasher, &reserve.state_slot.to_le_bytes());
        hash_part(&mut hasher, reserve.verification_commitment.as_bytes());
        hash_part(&mut hasher, reserve.reserve.as_bytes());
        hash_part(
            &mut hasher,
            reserve.market.as_deref().unwrap_or_default().as_bytes(),
        );
        hash_part(&mut hasher, reserve.liquidity_mint.as_bytes());
        hash_part(&mut hasher, &[reserve.mint_decimals]);
        hash_part(&mut hasher, &reserve.market_price_usd_micros.to_le_bytes());
        hash_part(&mut hasher, &reserve.reserve_last_update_slot.to_le_bytes());
        hash_part(&mut hasher, &reserve.economic_slot_lag.to_le_bytes());
        hash_part(
            &mut hasher,
            &reserve.economic_expires_at.timestamp_micros().to_le_bytes(),
        );
        hash_part(&mut hasher, &[u8::from(reserve.reserve_last_update_stale)]);
        hash_part(&mut hasher, &reserve.reserve_price_status.to_le_bytes());
        hash_part(
            &mut hasher,
            &reserve.market_price_last_updated_ts.to_le_bytes(),
        );
        hash_part(&mut hasher, reserve.available_amount_raw.as_bytes());
        hash_part(&mut hasher, reserve.borrowed_amount_raw.as_bytes());
        hash_part(&mut hasher, reserve.total_supply_amount_raw.as_bytes());
        hash_part(&mut hasher, &reserve.utilization_ppm.to_le_bytes());
        hash_part(&mut hasher, &reserve.borrow_apy_bps.to_le_bytes());
        hash_part(
            &mut hasher,
            &reserve.observed_at.timestamp_micros().to_le_bytes(),
        );
        hash_part(&mut hasher, &reserve.slot.to_le_bytes());
        hash_part(&mut hasher, &reserve.supply_apy_bps.to_le_bytes());
        hash_part(&mut hasher, &reserve.total_supply_usd_micros.to_le_bytes());
        hash_part(&mut hasher, &[u8::from(reserve.target_eligible)]);
    }
    format!("{:x}", hasher.finalize())
}

fn market_blocker_code_rank(code: MarketMintBlockerCode) -> u8 {
    match code {
        MarketMintBlockerCode::MissingCatalog => 0,
        MarketMintBlockerCode::CatalogSourceMismatch => 1,
        MarketMintBlockerCode::CatalogFetchedInFuture => 2,
        MarketMintBlockerCode::CatalogStale => 3,
        MarketMintBlockerCode::CatalogInsufficientLifetime => 4,
        MarketMintBlockerCode::DuplicateCatalogReserveIdentity => 5,
        MarketMintBlockerCode::DuplicateVerifiedReserveIdentity => 6,
        MarketMintBlockerCode::MissingVerifiedReserve => 7,
        MarketMintBlockerCode::VerifiedIdentityMismatch => 8,
        MarketMintBlockerCode::VerificationSourceMismatch => 9,
        MarketMintBlockerCode::VerificationCommitmentMismatch => 10,
        MarketMintBlockerCode::VerificationInFuture => 11,
        MarketMintBlockerCode::VerificationStale => 12,
        MarketMintBlockerCode::VerificationInsufficientLifetime => 13,
        MarketMintBlockerCode::InvalidStateIdentity => 14,
        MarketMintBlockerCode::MissingStableValuation => 15,
        MarketMintBlockerCode::MintDecimalsMismatch => 16,
        MarketMintBlockerCode::ExplicitStaleEconomics => 17,
        MarketMintBlockerCode::InvalidEconomicSlotOrder => 18,
        MarketMintBlockerCode::EconomicSlotLagExceeded => 19,
        MarketMintBlockerCode::EconomicInsufficientLifetime => 20,
        MarketMintBlockerCode::InvalidEconomicFields => 21,
        MarketMintBlockerCode::NoEligibleTarget => 22,
    }
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

#[cfg(test)]
mod tests {
    use super::*;

    fn swap_policy(source_shard: &str, policy_account: &str) -> CrossMintSwapPolicyEvidence {
        CrossMintSwapPolicyEvidence {
            settings: "settings".to_owned(),
            authority: "authority".to_owned(),
            policy_account: policy_account.to_owned(),
            vault_index: 1,
            vault_pubkey: "vault".to_owned(),
            delegated_signer: "signer".to_owned(),
            source_shard: source_shard.to_owned(),
            max_slippage_bps: 50,
            daily_source_mint_spending_cap: 1_000_000_000,
            manifest_fingerprint: "a".repeat(64),
            active: true,
            start_eligible: true,
            source_commitment: "finalized".to_owned(),
            last_seen_slot: 100,
            last_seen_signature: "swap-signature".to_owned(),
        }
    }

    fn complete_swap_policies() -> Vec<CrossMintSwapPolicyEvidence> {
        let classic = swap_policy("classic", "classic-policy");
        let mut token_2022 = swap_policy("token_2022", "token-2022-policy");
        token_2022.manifest_fingerprint = "b".repeat(64);
        token_2022.last_seen_slot = 110;
        token_2022.last_seen_signature = "token-2022-signature".to_owned();
        vec![classic, token_2022]
    }

    fn source_row(source_mint: &str, policies: Vec<CrossMintSwapPolicyEvidence>) -> FleetSourceRow {
        let canonical_mints = earn_stablecoins()
            .iter()
            .map(|stablecoin| stablecoin.mint.to_string())
            .collect::<Vec<_>>();
        FleetSourceRow {
            vault_id: 1,
            settings: "settings".to_owned(),
            vault_index: 1,
            vault_pubkey: "vault".to_owned(),
            policy_id: 2,
            base_policy_account: "base-policy".to_owned(),
            base_policy_delegated_signer: "signer".to_owned(),
            base_policy_cluster: "mainnet-beta".to_owned(),
            base_policy_source_commitment: "finalized".to_owned(),
            base_policy_finalized_eligible: true,
            base_policy_observed_slot: 99,
            base_policy_observed_signature: "base-signature".to_owned(),
            policy_authority: "authority".to_owned(),
            policy_markets: vec!["market".to_owned()],
            policy_stable_mints: canonical_mints.clone(),
            policy_liquidity_mints: canonical_mints,
            policy_route_modes: vec![SAME_MINT_ROUTE_MODE.to_owned()],
            earn_policy_evidence: Vec::new(),
            cross_mint_swap_policies: policies,
            source_kind: "reserve_position".to_owned(),
            source_reserve: Some("source-reserve".to_owned()),
            liquidity_mint: source_mint.to_owned(),
            amount_raw: 1_000_000,
            source_snapshot_id: Some(3),
            idle_token_account: None,
            observed_slot: 101,
            observed_at: Utc::now(),
            planning_metadata: Value::Null,
        }
    }

    fn earn_policy(
        mint: &str,
        market: &str,
        policy_account: &str,
        observed_slot: i64,
    ) -> ObservedEarnPolicyEvidence {
        ObservedEarnPolicyEvidence {
            settings: "settings".to_owned(),
            authority: "authority".to_owned(),
            policy_account: policy_account.to_owned(),
            vault_index: 1,
            vault_pubkey: "vault".to_owned(),
            delegated_signer: "signer".to_owned(),
            threshold: 1,
            stable_mints: vec![mint.to_owned()],
            kamino_markets: vec![market.to_owned()],
            kamino_liquidity_mints: vec![mint.to_owned()],
            source_commitment: "finalized".to_owned(),
            observed_slot,
            observed_signature: format!("signature-{policy_account}"),
        }
    }

    #[test]
    fn cross_mint_earn_bindings_select_distinct_exact_policies_deterministically() {
        let source_asset = &earn_stablecoins()[0];
        let target_asset = earn_stablecoins()
            .iter()
            .find(|asset| asset.token_program != source_asset.token_program)
            .expect("the Earn registry covers classic and Token-2022 mints");
        assert_ne!(source_asset.token_program, target_asset.token_program);
        let source_mint = source_asset.mint.to_string();
        let target_mint = target_asset.mint.to_string();
        let mut source = source_row(&source_mint, Vec::new());
        source.earn_policy_evidence = vec![
            earn_policy(&source_mint, "source-market", "source-older", 90),
            earn_policy(&source_mint, "source-market", "source-z", 100),
            earn_policy(&source_mint, "source-market", "source-a", 100),
            earn_policy(&target_mint, "target-market", "target-policy", 95),
        ];

        let (withdraw, deposit) =
            exact_cross_mint_earn_bindings(&source, "source-market", &target_mint, "target-market")
                .expect("distinct source and target policies authorize the route");

        assert_eq!(withdraw.policy_account, "source-a");
        assert_eq!(deposit.policy_account, "target-policy");
        assert_ne!(withdraw.policy_account, deposit.policy_account);
        assert!(!withdraw.stable_mints.contains(&target_mint));
        assert!(!deposit.stable_mints.contains(&source_mint));
    }

    #[test]
    fn cross_mint_earn_bindings_reject_an_absent_exact_target_policy() {
        let source_mint = earn_stablecoins()[0].mint.to_string();
        let target_mint = earn_stablecoins()[4].mint.to_string();
        let mut source = source_row(&source_mint, Vec::new());
        source.earn_policy_evidence = vec![earn_policy(
            &source_mint,
            "source-market",
            "source-policy",
            100,
        )];

        assert!(exact_cross_mint_earn_bindings(
            &source,
            "source-market",
            &target_mint,
            "target-market",
        )
        .is_none());
    }

    #[test]
    fn two_generalized_policies_cover_all_canonical_directed_pairs() {
        let source = &earn_stablecoins()[0];
        let source_row = source_row(&source.mint.to_string(), complete_swap_policies());
        let admitted_targets = earn_stablecoins()
            .iter()
            .filter(|target| {
                cross_mint_policy_selection(&source_row, "mainnet-beta", &target.mint.to_string())
                    .is_some()
            })
            .map(|target| target.mint.to_string())
            .collect::<BTreeSet<_>>();

        assert_eq!(admitted_targets.len(), 5);
        assert!(!admitted_targets.contains(&source.mint.to_string()));
        let mut covered_mints = admitted_targets;
        covered_mints.insert(source.mint.to_string());
        assert_eq!(
            covered_mints,
            earn_stablecoins()
                .iter()
                .map(|stablecoin| stablecoin.mint.to_string())
                .collect()
        );
    }

    #[test]
    fn generalized_manifest_requires_both_complete_source_shards() {
        let source = &earn_stablecoins()[3];
        let target = &earn_stablecoins()[2];
        let complete = source_row(&source.mint.to_string(), complete_swap_policies());
        let selection =
            cross_mint_policy_selection(&complete, "mainnet-beta", &target.mint.to_string())
                .unwrap();
        let lane = jupiter_swap_lane(selection);
        assert_eq!(lane.source_shard, "classic");
        assert_eq!(lane.policy_account, "classic-policy");
        assert_eq!(lane.manifest_fingerprint, "a".repeat(64));
    }

    #[test]
    fn generalized_manifest_half_install_and_half_removal_fail_closed() {
        let source = &earn_stablecoins()[3];
        let target = &earn_stablecoins()[2];
        let policies = complete_swap_policies();
        let half_install = source_row(&source.mint.to_string(), vec![policies[0].clone()]);
        assert!(cross_mint_policy_selection(
            &half_install,
            "mainnet-beta",
            &target.mint.to_string()
        )
        .is_none());

        let mut half_removed = policies;
        half_removed[1].active = false;
        assert!(cross_mint_policy_selection(
            &source_row(&source.mint.to_string(), half_removed),
            "mainnet-beta",
            &target.mint.to_string()
        )
        .is_none());
    }

    #[test]
    fn generalized_manifest_mismatched_risk_settings_fail_closed() {
        let source = &earn_stablecoins()[0];
        let target = &earn_stablecoins()[1];
        let mut policies = complete_swap_policies();
        policies[1].max_slippage_bps = 75;
        assert!(cross_mint_policy_selection(
            &source_row(&source.mint.to_string(), policies),
            "mainnet-beta",
            &target.mint.to_string()
        )
        .is_none());

        let mut policies = complete_swap_policies();
        policies[1].daily_source_mint_spending_cap += 1;
        assert!(cross_mint_policy_selection(
            &source_row(&source.mint.to_string(), policies),
            "mainnet-beta",
            &target.mint.to_string()
        )
        .is_none());
    }
}
