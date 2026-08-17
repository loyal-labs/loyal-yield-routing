use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    error::Error,
    time::Instant,
};

use chrono::{DateTime, Duration as ChronoDuration, Utc};
use loyal_observability::{init_from_env, OperationalError};
use loyal_yield_orchestrator::fleet_orchestration::{
    code_owned_stablecoin_valuations, fleet_worker_role_probe, observe_fleet_opportunities,
    observe_fleet_opportunities_for_vaults, observe_fleet_opportunities_without_queue_schema,
    observe_market_epoch, plan_capacity_aware_wave, route_fee_budget, run_deterministic_benchmark,
    CapacityBand, DurablePgWakeupEvent, DurablePgWakeupListener, EconomicPolicy,
    FleetObservationConfig, FleetPlanningDirtyVaultLease, FleetPlanningStateInput, FleetWorkerRole,
    IneligibleReason, MaterialMarketFrontier, ObservedFleetOpportunity, ObservedSourceKind,
    OptimizerEpochInput, RebalanceOpportunityInput, RouteFeePolicy, TargetCapacityCurve,
    WaveLimits, MARKET_MATERIAL_CAPACITY_DRIFT_PPM, MARKET_WAKE_PRICE_BUCKET_USD_MICROS,
    MAXIMUM_CONFIRMED_VERIFICATION_AGE_SECONDS, MAXIMUM_RESERVE_ECONOMIC_SLOT_LAG,
    MAXIMUM_SUPPORTED_RESERVE_CATALOG_AGE_SECONDS, MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS,
    RESERVE_ECONOMIC_EXPIRY_MILLIS_PER_SLOT,
};
use loyal_yield_orchestrator::{
    enabled_stable_mints_from_env, NeonSqlClient, NeonSqlConfig, OrchestratorError, SnapshotId,
    STANDARD_POLICY_AUTHORITY,
};
use loyal_yield_router::timescale::{TimescaleRouterClient, TimescaleRouterClientConfig};
use loyal_yield_store::fleet_orchestration::{CrossMintEarnPolicyBinding, CrossMintPolicyBindings};
use serde_json::{json, Value};
use tokio::{task::JoinSet, time::Duration};

const DEFAULT_CLUSTER: &str = "mainnet-beta";
const CLUSTER_ENV: &str = "YIELD_ALT_CLUSTER";
const DEFAULT_POLL_INTERVAL_SECONDS: u64 = 1;
const DEFAULT_FULL_SWEEP_INTERVAL_SECONDS: u64 = 30;
const DEFAULT_MARKET_PROBE_INTERVAL_SECONDS: u64 = 5;
const DEFAULT_PLANNER_LOG_HEARTBEAT_SECONDS: u64 = 5;
const DEFAULT_WAVE_SIZE: usize = 128;
const DEFAULT_DIRTY_BATCH_SIZE: usize = 256;
const DIRTY_LEASE_SECONDS: i64 = 60;
const DEFAULT_QUEUE_CONNECTIONS: u32 = 20;
const DEFAULT_ESTIMATED_COST_USD_MICROS: i64 = 100_000;
const CROSS_MINT_JUPITER_ENABLED_ENV: &str = "EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER";
const CROSS_MINT_MAXIMUM_VALUE_LOSS_BPS_ENV: &str = "EARN_ROUTER_CROSS_MINT_MAX_VALUE_LOSS_BPS";
const DEFAULT_CROSS_MINT_MAXIMUM_VALUE_LOSS_BPS: u16 = 50;
const PLANNER_MAXIMUM_CYCLE_BACKOFF_SECONDS: u64 = 30;
const PLANNER_RECOVERY_VERIFICATION_CYCLES: usize = 10_000;
const PRIORITY_VERSION: &str = "lost-yield-service-net-reserve-capacity-v3";

#[derive(Debug)]
struct Options {
    once: bool,
    dry_run: bool,
    benchmark: bool,
    json: bool,
    count: usize,
    rounds: usize,
    seed: u64,
    cluster: String,
    poll_interval_seconds: u64,
    full_sweep_interval_seconds: u64,
    max_opportunities_per_wave: usize,
    dirty_batch_size: usize,
    enable_cross_mint_jupiter: bool,
}

fn parse_options() -> Result<Options, Box<dyn Error>> {
    let mut options = Options {
        once: false,
        dry_run: false,
        benchmark: false,
        json: false,
        count: 10_000,
        rounds: 7,
        seed: 0x004c_4f59_414c,
        cluster: env::var(CLUSTER_ENV).unwrap_or_else(|_| DEFAULT_CLUSTER.to_owned()),
        poll_interval_seconds: DEFAULT_POLL_INTERVAL_SECONDS,
        full_sweep_interval_seconds: DEFAULT_FULL_SWEEP_INTERVAL_SECONDS,
        max_opportunities_per_wave: DEFAULT_WAVE_SIZE,
        dirty_batch_size: DEFAULT_DIRTY_BATCH_SIZE,
        enable_cross_mint_jupiter: false,
    };
    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--once" => options.once = true,
            "--dry-run" => options.dry_run = true,
            "--benchmark" => options.benchmark = true,
            "--json" => options.json = true,
            "--enable-cross-mint-jupiter" => options.enable_cross_mint_jupiter = true,
            "--count" => {
                options.count = args.next().ok_or("--count requires a value")?.parse()?;
            }
            "--rounds" => {
                options.rounds = args.next().ok_or("--rounds requires a value")?.parse()?;
            }
            "--seed" => {
                options.seed = args.next().ok_or("--seed requires a value")?.parse()?;
            }
            "--cluster" => {
                options.cluster = args.next().ok_or("--cluster requires a value")?;
            }
            "--poll-interval-seconds" => {
                options.poll_interval_seconds = args
                    .next()
                    .ok_or("--poll-interval-seconds requires a value")?
                    .parse()?;
            }
            "--max-opportunities-per-wave" => {
                options.max_opportunities_per_wave = args
                    .next()
                    .ok_or("--max-opportunities-per-wave requires a value")?
                    .parse()?;
            }
            "--full-sweep-interval-seconds" => {
                options.full_sweep_interval_seconds = args
                    .next()
                    .ok_or("--full-sweep-interval-seconds requires a value")?
                    .parse()?;
            }
            "--dirty-batch-size" => {
                options.dirty_batch_size = args
                    .next()
                    .ok_or("--dirty-batch-size requires a value")?
                    .parse()?;
            }
            "--help" | "-h" => {
                println!(
                    "fleet-opportunity-planner [--once] [--dry-run] [--json] [--enable-cross-mint-jupiter] [--cluster NAME] [--poll-interval-seconds N] [--full-sweep-interval-seconds N] [--dirty-batch-size N] [--max-opportunities-per-wave N]\n\
                     fleet-opportunity-planner --once --dry-run --benchmark [--json] [--count N] [--rounds N] [--seed N]\n\n\
                     Cross-mint planning is default-off and requires --enable-cross-mint-jupiter or EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER=true.\n\
                     Live mode reads YIELD_ALT_CLUSTER (overridden by --cluster), NEON_DATABASE_URL, and TIMESCALEDB_URL."
                );
                std::process::exit(0);
            }
            other => return Err(format!("unknown argument: {other}").into()),
        }
    }
    if options.benchmark && (!options.once || !options.dry_run) {
        return Err("--benchmark requires --once and --dry-run".into());
    }
    if !options.enable_cross_mint_jupiter {
        options.enable_cross_mint_jupiter = match env::var(CROSS_MINT_JUPITER_ENABLED_ENV) {
            Ok(value) if value.eq_ignore_ascii_case("true") || value == "1" => true,
            Ok(value) if value.eq_ignore_ascii_case("false") || value == "0" => false,
            Ok(value) => {
                return Err(format!(
                    "{CROSS_MINT_JUPITER_ENABLED_ENV} must be true, false, 1, or 0; got {value:?}"
                )
                .into())
            }
            Err(env::VarError::NotPresent) => false,
            Err(error) => return Err(error.into()),
        };
    }
    if options.count == 0
        || options.rounds == 0
        || options.poll_interval_seconds == 0
        || options.full_sweep_interval_seconds == 0
        || options.max_opportunities_per_wave == 0
        || options.dirty_batch_size == 0
        || options.dirty_batch_size > 1_024
        || !matches!(
            options.cluster.as_str(),
            "mainnet-beta" | "devnet" | "testnet" | "localnet"
        )
    {
        return Err(
            "counts and intervals must be positive, and YIELD_ALT_CLUSTER/--cluster must be mainnet-beta, devnet, testnet, or localnet"
                .into(),
        );
    }
    Ok(options)
}

fn percentile_95(sorted: &[u128]) -> u128 {
    let index = (sorted.len() * 95).div_ceil(100).saturating_sub(1);
    sorted[index]
}

fn rejection_reason_counts<'a>(
    reasons: impl Iterator<Item = &'a IneligibleReason>,
) -> BTreeMap<&'static str, usize> {
    let mut counts = BTreeMap::new();
    for reason in reasons {
        let key = match reason {
            IneligibleReason::InvalidIdentity => "invalid_identity",
            IneligibleReason::InvalidNotional => "invalid_notional",
            IneligibleReason::InvalidApy => "invalid_apy",
            IneligibleReason::InvalidConfidence => "invalid_confidence",
            IneligibleReason::InvalidExpectedServiceTime => "invalid_expected_service_time",
            IneligibleReason::InvalidHoldingHorizon => "invalid_holding_horizon",
            IneligibleReason::InvalidCost => "invalid_cost",
            IneligibleReason::BelowMinimumNotional => "below_minimum_notional",
            IneligibleReason::NonPositiveEdge => "non_positive_edge",
            IneligibleReason::BelowMinimumEdge => "below_minimum_edge",
            IneligibleReason::ExpectedGainDoesNotCoverCost { .. } => {
                "expected_gain_does_not_cover_cost"
            }
            IneligibleReason::BelowMinimumNetGain { .. } => "below_minimum_net_gain",
            IneligibleReason::TargetCapacityExhausted => "target_capacity_exhausted",
        };
        *counts.entry(key).or_default() += 1;
    }
    counts
}

fn run_benchmark(options: &Options) -> Result<Value, Box<dyn Error>> {
    let mut elapsed_micros = Vec::with_capacity(options.rounds);
    let mut last = None;
    for round in 0..options.rounds {
        let started = Instant::now();
        let result =
            run_deterministic_benchmark(options.count, options.seed.wrapping_add(round as u64))
                .map_err(|error| format!("planner failed: {error:?}"))?;
        elapsed_micros.push(started.elapsed().as_micros());
        last = Some(result);
    }
    elapsed_micros.sort_unstable();
    let p95_micros = percentile_95(&elapsed_micros);
    let result = last.expect("at least one benchmark round");
    Ok(json!({
        "status": if p95_micros < 10_000_000 { "pass" } else { "fail" },
        "mode": "deterministic_in_memory_replay",
        "mutating": false,
        "childProcessesSpawned": 0,
        "inputCount": options.count,
        "rounds": options.rounds,
        "elapsedMicros": elapsed_micros,
        "planningP95Micros": p95_micros,
        "planningLimitMicros": 10_000_000,
        "economicPriorityOrdered": result.wave.selected.windows(2).all(|pair| {
            pair[0].economics.total_priority >= pair[1].economics.total_priority
        }),
        "selectedCount": result.selected_count,
        "deferredCount": result.deferred_count,
        "rejectedCount": result.rejected_count,
        "rejectionReasonCounts": rejection_reason_counts(
            result.wave.rejected.iter().map(|rejected| &rejected.reason),
        ),
        "selectedNotionalUsdMicros": result.selected_notional_usd_micros,
        "selectedLostYieldUsdMicrosPerHour": result.selected_lost_yield_usd_micros_per_hour,
        "deterministicDigest": result.deterministic_digest,
        "seed": options.seed,
        "productionPerformance": "not_run",
    }))
}

fn live_observation_config(
    cluster: &str,
    enabled_mints: Vec<String>,
    enable_cross_mint_jupiter: bool,
) -> Result<FleetObservationConfig, Box<dyn Error>> {
    let stablecoin_valuations = code_owned_stablecoin_valuations(&enabled_mints)?;
    let cross_mint_maximum_value_loss_bps = if enable_cross_mint_jupiter {
        match env::var(CROSS_MINT_MAXIMUM_VALUE_LOSS_BPS_ENV) {
            Ok(value) => value.parse::<u16>().map_err(|_| {
                format!("{CROSS_MINT_MAXIMUM_VALUE_LOSS_BPS_ENV} must be an integer in 1..=1000")
            })?,
            Err(env::VarError::NotPresent) => DEFAULT_CROSS_MINT_MAXIMUM_VALUE_LOSS_BPS,
            Err(error) => return Err(error.into()),
        }
    } else {
        DEFAULT_CROSS_MINT_MAXIMUM_VALUE_LOSS_BPS
    };
    if !(1..=1_000).contains(&cross_mint_maximum_value_loss_bps) {
        return Err(format!("{CROSS_MINT_MAXIMUM_VALUE_LOSS_BPS_ENV} must be in 1..=1000").into());
    }
    Ok(FleetObservationConfig {
        cluster: cluster.to_owned(),
        // Stable notional and reserve capacity use this code-owned contract;
        // reserve oracle status/time remains evidence but does not size supply.
        stablecoin_valuations,
        enabled_mints,
        estimated_reserve_move_cost_usd_micros: DEFAULT_ESTIMATED_COST_USD_MICROS,
        estimated_idle_deposit_cost_usd_micros: DEFAULT_ESTIMATED_COST_USD_MICROS,
        enable_cross_mint_jupiter,
        estimated_cross_mint_withdraw_cost_usd_micros: DEFAULT_ESTIMATED_COST_USD_MICROS,
        estimated_cross_mint_jupiter_swap_cost_usd_micros: DEFAULT_ESTIMATED_COST_USD_MICROS,
        estimated_cross_mint_deposit_cost_usd_micros: DEFAULT_ESTIMATED_COST_USD_MICROS,
        cross_mint_maximum_value_loss_bps,
        ..FleetObservationConfig::default()
    })
}

fn market_wake_policy_evidence() -> Value {
    json!({
        "probeIntervalSeconds": DEFAULT_MARKET_PROBE_INTERVAL_SECONDS,
        "priceBucketUsdMicros": MARKET_WAKE_PRICE_BUCKET_USD_MICROS,
        "maximumNonmaterialCapacityDriftPpm": MARKET_MATERIAL_CAPACITY_DRIFT_PPM,
        "catalogMaximumAgeSeconds": MAXIMUM_SUPPORTED_RESERVE_CATALOG_AGE_SECONDS,
        "confirmedVerificationMaximumAgeSeconds": MAXIMUM_CONFIRMED_VERIFICATION_AGE_SECONDS,
        "economicMaximumSlotLag": MAXIMUM_RESERVE_ECONOMIC_SLOT_LAG,
        "economicExpiryMillisPerRemainingSlot": RESERVE_ECONOMIC_EXPIRY_MILLIS_PER_SLOT,
        "minimumPublicationLifetimeSeconds": MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS,
        "capacityFence": "latest_non_released_reservations_plus_execution_time_target_version",
        "includedFields": [
            "eligible_reserve_set",
            "target_order_integer_apy_bps",
            "stablecoin_price_bucket",
            "material_target_capacity_drift"
        ],
        "excludedFields": ["observation_time", "observation_slot", "exact_total_supply"],
    })
}

fn capacity_curves(
    observation: &loyal_yield_orchestrator::fleet_orchestration::FleetObservationResult,
) -> Vec<TargetCapacityCurve> {
    let committed_inflows = observation
        .committed_target_inflows
        .iter()
        .map(|flow| (flow.target_reserve.as_str(), flow.principal_usd_micros))
        .collect::<BTreeMap<_, _>>();
    let committed_outflows = observation
        .committed_source_outflows
        .iter()
        .map(|flow| (flow.source_reserve.as_str(), flow.principal_usd_micros))
        .collect::<BTreeMap<_, _>>();
    observation
        .market_epoch
        .reserves
        .iter()
        .map(|reserve| {
            let already_committed = committed_inflows
                .get(reserve.reserve.as_str())
                .copied()
                .unwrap_or_default()
                .max(0);
            let already_committed_outflow = committed_outflows
                .get(reserve.reserve.as_str())
                .copied()
                .unwrap_or_default()
                .max(0);
            let one_tenth_percent = (reserve.total_supply_usd_micros / 1_000).max(1_000_000);
            let half_percent = (reserve.total_supply_usd_micros / 200).max(2_000_000);
            let one_percent = (reserve.total_supply_usd_micros / 100).max(3_000_000);
            let two_percent = (reserve.total_supply_usd_micros / 50).max(4_000_000);
            TargetCapacityCurve {
                target_reserve: reserve.reserve.clone(),
                observed_supply_usd_micros: reserve.total_supply_usd_micros,
                observed_net_apy_bps: reserve.supply_apy_bps,
                already_committed_inflow_usd_micros: already_committed,
                already_committed_outflow_usd_micros: already_committed_outflow,
                bands: vec![
                    CapacityBand {
                        // Ceilings are absolute within the epoch. Committed
                        // inflight dollars consume this headroom; moving the
                        // ceiling forward with every planner pass would grant
                        // a fresh capacity allowance every five seconds.
                        cumulative_inflow_usd_micros: one_tenth_percent,
                        target_net_apy_bps: marginal_supply_apy_bps(
                            reserve.supply_apy_bps,
                            reserve.total_supply_usd_micros,
                            one_tenth_percent,
                        ),
                    },
                    CapacityBand {
                        cumulative_inflow_usd_micros: half_percent,
                        target_net_apy_bps: marginal_supply_apy_bps(
                            reserve.supply_apy_bps,
                            reserve.total_supply_usd_micros,
                            half_percent,
                        ),
                    },
                    CapacityBand {
                        cumulative_inflow_usd_micros: one_percent,
                        target_net_apy_bps: marginal_supply_apy_bps(
                            reserve.supply_apy_bps,
                            reserve.total_supply_usd_micros,
                            one_percent,
                        ),
                    },
                    CapacityBand {
                        cumulative_inflow_usd_micros: two_percent.max(already_committed),
                        target_net_apy_bps: marginal_supply_apy_bps(
                            reserve.supply_apy_bps,
                            reserve.total_supply_usd_micros,
                            two_percent.max(already_committed),
                        ),
                    },
                ],
            }
        })
        .collect()
}

fn marginal_supply_apy_bps(
    current_supply_apy_bps: i64,
    current_supply_usd_micros: i64,
    cumulative_inflow_usd_micros: i64,
) -> i64 {
    if current_supply_usd_micros <= 0 || cumulative_inflow_usd_micros <= 0 {
        return current_supply_apy_bps;
    }
    // Over a bounded wave, outstanding borrows and reserve take rate are held
    // fixed. Deposits dilute utilization, so supplier yield scales by
    // S/(S+inflow). Each following wave refreshes this from chain/Timescale.
    let numerator = i128::from(current_supply_apy_bps) * i128::from(current_supply_usd_micros);
    let denominator = i128::from(current_supply_usd_micros)
        .saturating_add(i128::from(cumulative_inflow_usd_micros));
    (numerator / denominator).clamp(i128::from(i64::MIN), i128::from(i64::MAX)) as i64
}

fn opportunity_execution_plan(
    observed: &ObservedFleetOpportunity,
    optimizer_market_slot: i64,
    capacity_adjusted_source_apy_bps: i64,
    capacity_adjusted_target_apy_bps: i64,
    fee_cap_lamports: i64,
    fee_tier: &'static str,
    fee_policy: RouteFeePolicy,
) -> Value {
    let policy_bindings = matches!(
        observed.route_kind,
        loyal_yield_orchestrator::fleet_orchestration::CandidateRouteKind::CrossMintJupiter
    )
    .then(|| {
        let withdraw = observed
            .source_earn_policy
            .as_ref()
            .expect("cross-mint observation must include an exact withdraw policy");
        let swap = observed
            .jupiter_swap_lane
            .as_ref()
            .expect("cross-mint observation must include a generalized swap policy")
            .clone();
        let deposit = observed
            .target_earn_policy
            .as_ref()
            .expect("cross-mint observation must include an exact deposit policy");
        CrossMintPolicyBindings {
            settings: observed.settings.clone(),
            vault_index: u8::try_from(observed.vault_index)
                .expect("cross-mint vault index must fit an unsigned byte"),
            vault_pubkey: observed.vault_pubkey.clone(),
            delegated_signer: withdraw.delegated_signer.clone(),
            withdraw: CrossMintEarnPolicyBinding {
                policy_account: withdraw.policy_account.clone(),
                observed_slot: u64::try_from(withdraw.observed_slot)
                    .expect("withdraw policy slot must be nonnegative"),
                observed_signature: withdraw.observed_signature.clone(),
                source_commitment: withdraw.source_commitment.clone(),
                constraint_index: 0,
            },
            swap,
            deposit: CrossMintEarnPolicyBinding {
                policy_account: deposit.policy_account.clone(),
                observed_slot: u64::try_from(deposit.observed_slot)
                    .expect("deposit policy slot must be nonnegative"),
                observed_signature: deposit.observed_signature.clone(),
                source_commitment: deposit.source_commitment.clone(),
                constraint_index: 1,
            },
        }
    });
    let mut plan = json!({
        "kind": observed.route_kind,
        "route_kind": observed.route_kind,
        "settings": observed.settings,
        "vault_index": observed.vault_index,
        "vault_pubkey": observed.vault_pubkey,
        "policy_id": observed.policy_id,
        "source_kind": observed.source_kind,
        "source_reserve": match observed.source_kind {
            ObservedSourceKind::ReservePosition => Some(observed.economics.source_reserve.as_str()),
            ObservedSourceKind::IdleVaultUsdc => None,
        },
        "target_reserve": observed.economics.target_reserve,
        "liquidity_mint": observed.economics.mint,
        "source_liquidity_mint": observed.source_liquidity_mint,
        "target_liquidity_mint": observed.target_liquidity_mint,
        "amount_raw": observed.amount_raw,
        "route_amount_semantics": observed.route_amount_semantics,
        "source_amount_semantics": observed.source_amount_semantics,
        "source_collateral_amount_raw": observed.source_collateral_amount_raw,
        "redeemable_source_liquidity_amount_raw": observed.redeemable_source_liquidity_amount_raw,
        "idle_vault_liquidity_amount_raw": observed.idle_vault_liquidity_amount_raw,
        "idle_token_account": observed.idle_token_account,
        "source_apy_bps": capacity_adjusted_source_apy_bps,
        "observed_source_apy_bps": observed.economics.source_net_apy_bps,
        "observed_target_apy_bps": observed.economics.target_net_apy_bps,
        "target_apy_bps": capacity_adjusted_target_apy_bps,
        "capacity_adjusted_target_apy_bps": capacity_adjusted_target_apy_bps,
        "estimated_edge_bps": capacity_adjusted_target_apy_bps - capacity_adjusted_source_apy_bps,
        "confidence_ppm": observed.economics.confidence_ppm,
        "expected_service_millis": observed.economics.expected_service_millis,
        "holding_horizon_seconds": observed.economics.holding_horizon_seconds,
        "estimated_execution_cost_usd_micros": observed.economics.estimated_execution_cost_usd_micros,
        "estimated_execution_costs": observed.estimated_execution_costs,
        "fee_cap_lamports": fee_cap_lamports,
        "fee_tier": fee_tier,
        "fee_gain_fraction_ppm": fee_policy.maximum_fraction_of_net_gain_ppm,
        "conservative_sol_price_usd_micros": fee_policy.conservative_sol_price_usd_micros,
        "source_observed_at": observed.source_observed_at,
        "source_observed_slot": observed.source_observed_slot,
        "optimizer_market_slot": optimizer_market_slot,
        "target_observed_at": observed.target_observed_at,
        "target_observed_slot": observed.target_observed_slot,
        "writable_conflict_keys": observed.economics.writable_conflict_keys,
    });
    let plan_object = plan
        .as_object_mut()
        .expect("opportunity execution plan literal must be an object");
    plan_object.insert("policy_bindings".to_owned(), json!(policy_bindings));
    plan_object.insert(
        "cross_mint_maximum_value_loss_bps".to_owned(),
        json!(observed.cross_mint_maximum_value_loss_bps),
    );
    plan_object.insert(
        "planning_economics_are_executable_quote".to_owned(),
        Value::Bool(false),
    );
    plan_object.insert(
        "fresh_executable_jupiter_minimum_output_required".to_owned(),
        Value::Bool(matches!(
            observed.route_kind,
            loyal_yield_orchestrator::fleet_orchestration::CandidateRouteKind::CrossMintJupiter
        )),
    );
    plan
}

struct PublicationTerms<'a> {
    cluster: &'a str,
    optimizer_epoch_id: i64,
    optimizer_market_slot: i64,
    admitted_source_apy_bps: i64,
    admitted_target_apy_bps: i64,
    annual_yield_gain_usd_micros: i64,
    expected_net_gain_usd_micros: i64,
    economic_priority: i64,
    fee_cap_lamports: i64,
    fee_tier: &'static str,
    fee_policy: RouteFeePolicy,
    available_at: DateTime<Utc>,
    expires_at: DateTime<Utc>,
}

fn rebalance_opportunity_input(
    observed: &ObservedFleetOpportunity,
    terms: PublicationTerms<'_>,
) -> RebalanceOpportunityInput {
    RebalanceOpportunityInput {
        cluster: terms.cluster.to_owned(),
        vault_id: loyal_yield_orchestrator::VaultId(observed.economics.vault_id),
        source_snapshot_id: match observed.source_kind {
            ObservedSourceKind::ReservePosition => {
                Some(SnapshotId(observed.economics.source_snapshot_id))
            }
            ObservedSourceKind::IdleVaultUsdc => None,
        },
        optimizer_epoch_id: terms.optimizer_epoch_id,
        route_fingerprint: None,
        requirements_fingerprint: None,
        source_reserve: match observed.source_kind {
            ObservedSourceKind::ReservePosition => Some(observed.economics.source_reserve.clone()),
            ObservedSourceKind::IdleVaultUsdc => None,
        },
        target_reserve: observed.economics.target_reserve.clone(),
        // The legacy queue mint is target-authoritative for cross-mint rows.
        // Source and target remain explicit, immutable fields in the plan and
        // are materialized by the store into their dedicated columns.
        liquidity_mint: observed.target_liquidity_mint.clone(),
        amount_raw: observed.amount_raw,
        principal_usd_micros: observed.economics.notional_usd_micros,
        source_apy_bps: terms.admitted_source_apy_bps,
        target_apy_bps: terms.admitted_target_apy_bps,
        estimated_edge_bps: terms.admitted_target_apy_bps - terms.admitted_source_apy_bps,
        estimated_cost_lamports: terms.fee_cap_lamports,
        annual_yield_gain_usd_micros: terms.annual_yield_gain_usd_micros,
        expected_net_gain_usd_micros: terms.expected_net_gain_usd_micros,
        economic_priority: terms.economic_priority.max(1),
        priority_version: PRIORITY_VERSION.to_owned(),
        execution_plan: opportunity_execution_plan(
            observed,
            terms.optimizer_market_slot,
            terms.admitted_source_apy_bps,
            terms.admitted_target_apy_bps,
            terms.fee_cap_lamports,
            terms.fee_tier,
            terms.fee_policy,
        ),
        available_at: terms.available_at,
        expires_at: terms.expires_at,
        provisioning_request_id: None,
    }
}

#[derive(Debug, Default)]
struct PublishWaveResult {
    published: usize,
    deferred_contention: usize,
    deferred_lifetime: usize,
}

fn is_publish_contention(error: &OrchestratorError) -> bool {
    matches!(
        error,
        OrchestratorError::OpportunityDeferredBehindLease { .. }
            | OrchestratorError::OpportunityDeferredBehindActiveSlot { .. }
    )
}

/// The optimizer epoch crossed the minimum usable lifetime while this wave was
/// being written. The route is simply not publishable against that evidence
/// any more; the next observation republishes it against a fresh epoch.
fn is_publish_lifetime_deferral(error: &OrchestratorError) -> bool {
    matches!(
        error,
        OrchestratorError::OpportunityDeferredBehindEpochLifetime { .. }
    )
}

async fn publish_wave(
    neon: &NeonSqlClient,
    inputs: Vec<RebalanceOpportunityInput>,
) -> Result<PublishWaveResult, Box<dyn Error>> {
    let mut tasks = JoinSet::new();
    for input in inputs {
        let neon = neon.clone();
        let vault_id = input.vault_id;
        tasks.spawn(async move {
            let result = async {
                let optimizer_epoch_id = input.optimizer_epoch_id;
                let opportunity = neon.upsert_rebalance_opportunity(input).await?;
                if opportunity.state
                    == loyal_yield_orchestrator::fleet_orchestration::RebalanceOpportunityState::WaitingAlt
                {
                    neon.re_admit_waiting_alt_opportunity(opportunity.id, optimizer_epoch_id)
                        .await?;
                }
                Ok::<(), OrchestratorError>(())
            }
            .await;
            (vault_id, result)
        });
    }
    let mut summary = PublishWaveResult::default();
    while let Some(joined) = tasks.join_next().await {
        let (vault_id, task_result) =
            joined.map_err(|error| format!("opportunity publish task failed: {error}"))?;
        match task_result {
            Ok(()) => summary.published += 1,
            Err(error) if is_publish_contention(&error) => {
                summary.deferred_contention += 1;
                let (reason, slot_opportunity_id, slot_opportunity_state) = match &error {
                    OrchestratorError::OpportunityDeferredBehindLease { leased_id, .. } => (
                        "unexpired_competing_lease",
                        Some(*leased_id),
                        Some("leased"),
                    ),
                    OrchestratorError::OpportunityDeferredBehindActiveSlot {
                        slot_opportunity_id,
                        slot_opportunity_state,
                        reason,
                        ..
                    } => (
                        *reason,
                        *slot_opportunity_id,
                        slot_opportunity_state.as_deref(),
                    ),
                    _ => unreachable!("publish contention classification must stay exhaustive"),
                };
                eprintln!(
                    "{}",
                    json!({
                        "status": "fleet_opportunity_publish_deferred",
                        "vaultId": vault_id.as_i64(),
                        "slotOpportunityId": slot_opportunity_id,
                        "slotOpportunityState": slot_opportunity_state,
                        "reason": reason,
                        "durableRecoveryRequired": true,
                    })
                );
            }
            Err(error) if is_publish_lifetime_deferral(&error) => {
                summary.deferred_lifetime += 1;
                let stage = match &error {
                    OrchestratorError::OpportunityDeferredBehindEpochLifetime { stage, .. } => {
                        *stage
                    }
                    _ => unreachable!("publish lifetime classification must stay exhaustive"),
                };
                eprintln!(
                    "{}",
                    json!({
                        "status": "fleet_opportunity_publish_deferred",
                        "vaultId": vault_id.as_i64(),
                        "reason": "optimizer_epoch_lifetime_exhausted",
                        "stage": stage,
                        "durableRecoveryRequired": false,
                        "republishedOnNextObservation": true,
                    })
                );
            }
            Err(error) => return Err(error.into()),
        }
    }
    Ok(summary)
}

#[derive(Debug)]
struct PlanningEvidence {
    optimizer_epoch_key: String,
    material_frontier_fingerprint: String,
    material_frontier: MaterialMarketFrontier,
    optimizer_epoch_expires_at: chrono::DateTime<Utc>,
    observed_vault_count: i64,
    opportunity_count: i64,
    selected_count: i64,
    deferred_count: i64,
    complete_frontier: bool,
}

#[derive(Debug)]
struct LivePlanningRun {
    output: Value,
    evidence: Option<PlanningEvidence>,
    fallback_to_full: bool,
}

fn next_full_sweep_delay(run: &LivePlanningRun, options: &Options) -> Duration {
    match run.evidence.as_ref() {
        Some(evidence) if evidence.deferred_count == 0 => {
            Duration::from_secs(options.full_sweep_interval_seconds)
        }
        Some(_) | None => Duration::from_secs(options.poll_interval_seconds),
    }
}

fn annotate_full_sweep_schedule(run: &mut LivePlanningRun, delay: Duration) {
    let deferred_count = run
        .evidence
        .as_ref()
        .map_or(0, |evidence| evidence.deferred_count);
    if let Some(output) = run.output.as_object_mut() {
        output.insert(
            "deferredFrontierDrainRequired".to_owned(),
            json!(deferred_count > 0),
        );
        output.insert(
            "nextAuthoritativeFullSweepDelayMilliseconds".to_owned(),
            json!(u64::try_from(delay.as_millis()).unwrap_or(u64::MAX)),
        );
    }
}

#[allow(clippy::too_many_arguments)]
async fn run_live_once(
    options: &Options,
    neon: &NeonSqlClient,
    timescale: &TimescaleRouterClient,
    delegated_signer: &str,
    config: &FleetObservationConfig,
    scoped_vault_ids: Option<&[i64]>,
    required_material_frontier: Option<&MaterialMarketFrontier>,
    allow_scoped_admission: bool,
    queue_schema_available: bool,
) -> Result<LivePlanningRun, Box<dyn Error>> {
    let started = Instant::now();
    let expired_opportunities_swept =
        if options.dry_run || scoped_vault_ids.is_some() || !queue_schema_available {
            0
        } else {
            neon.sweep_expired_rebalance_opportunities(&options.cluster, 10_000)
                .await?
        };
    let observation = if !queue_schema_available {
        if scoped_vault_ids.is_some() || !options.dry_run {
            return Err("queue-less observation is restricted to full-fleet dry runs".into());
        }
        observe_fleet_opportunities_without_queue_schema(neon, timescale, delegated_signer, config)
            .await?
    } else {
        match scoped_vault_ids {
            Some(vault_ids) => {
                observe_fleet_opportunities_for_vaults(
                    neon,
                    timescale,
                    delegated_signer,
                    config,
                    vault_ids,
                )
                .await?
            }
            None => observe_fleet_opportunities(neon, timescale, delegated_signer, config).await?,
        }
    };
    let observed_micros = started.elapsed().as_micros();
    let minimum_usable_until =
        Utc::now() + ChronoDuration::seconds(MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS);
    let optimizer_envelope_expires_at = observation.market_epoch.optimizer_envelope_expires_at();
    if observation.market_epoch.reserves.is_empty()
        || optimizer_envelope_expires_at <= minimum_usable_until
    {
        return Ok(LivePlanningRun {
            output: json!({
                "status": "no_fresh_market_epoch",
                "mutating": false,
                "planningScope": if scoped_vault_ids.is_some() { "dirty_cohort" } else { "full_fleet" },
                "observation": observation.stats,
                "minimumUsableEpochLifetimeSeconds": MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS,
                "epochExpiresAt": observation.market_epoch.expires_at,
                "optimizerEnvelopeExpiresAt": optimizer_envelope_expires_at,
                "expiredOpportunitiesSwept": expired_opportunities_swept,
                "elapsedMicros": started.elapsed().as_micros(),
            }),
            evidence: None,
            fallback_to_full: scoped_vault_ids.is_some(),
        });
    }
    if let Some(vault_ids) =
        scoped_vault_ids.filter(|_| allow_scoped_admission && required_material_frontier.is_none())
    {
        return Ok(LivePlanningRun {
            output: json!({
                "status": "dirty_cohort_requires_full_sweep",
                "reason": "missing_material_frontier_fence",
                "mutating": false,
                "planningScope": "dirty_cohort",
                "scopedVaultCount": vault_ids.len(),
                "observationMicros": observed_micros,
                "elapsedMicros": started.elapsed().as_micros(),
            }),
            evidence: None,
            fallback_to_full: true,
        });
    }
    if let Some(vault_ids) = scoped_vault_ids.filter(|_| !allow_scoped_admission) {
        let candidate_vault_ids = observation
            .opportunities
            .iter()
            .map(|opportunity| opportunity.economics.vault_id)
            .collect::<BTreeSet<_>>();
        let invalidated_vault_ids = vault_ids
            .iter()
            .copied()
            .filter(|vault_id| !candidate_vault_ids.contains(vault_id))
            .collect::<Vec<_>>();
        let retired_dirty_opportunities = if options.dry_run || invalidated_vault_ids.is_empty() {
            0
        } else {
            neon.retire_unselected_dirty_vault_opportunities(
                &options.cluster,
                &invalidated_vault_ids,
                &[],
            )
            .await?
        };
        let needs_full_sweep = !candidate_vault_ids.is_empty();
        return Ok(LivePlanningRun {
            output: json!({
                "status": if needs_full_sweep { "dirty_cohort_requires_full_sweep" } else { "dirty_cohort_invalidated" },
                "reason": if needs_full_sweep { "global_frontier_incomplete" } else { "no_current_route_candidate" },
                "mutating": !options.dry_run && !invalidated_vault_ids.is_empty(),
                "planningScope": "dirty_invalidation_only",
                "scopedVaultCount": vault_ids.len(),
                "candidateVaultCount": candidate_vault_ids.len(),
                "invalidatedVaultCount": invalidated_vault_ids.len(),
                "retiredDirtyOpportunityCount": retired_dirty_opportunities,
                "observationMicros": observed_micros,
                "elapsedMicros": started.elapsed().as_micros(),
            }),
            evidence: None,
            fallback_to_full: needs_full_sweep,
        });
    }
    let current_material_frontier = observation.market_epoch.material_market_frontier();
    let material_frontier_disposition = required_material_frontier
        .map(|required| required.disposition_against(&current_material_frontier));
    if material_frontier_disposition
        .is_some_and(|disposition| !disposition.allows_scoped_planning())
    {
        return Ok(LivePlanningRun {
            output: json!({
                "status": "dirty_cohort_requires_full_sweep",
                "reason": "material_market_frontier_changed",
                "materialFrontierDisposition": material_frontier_disposition,
                "mutating": false,
                "planningScope": "dirty_cohort",
                "observedEpochFingerprint": observation.market_epoch.fingerprint,
                "scopedVaultCount": scoped_vault_ids.map(|ids| ids.len()).unwrap_or_default(),
                "observationMicros": observed_micros,
                "elapsedMicros": started.elapsed().as_micros(),
            }),
            evidence: None,
            fallback_to_full: true,
        });
    }

    let wave = plan_capacity_aware_wave(
        observation.economic_inputs(),
        &EconomicPolicy::default(),
        capacity_curves(&observation),
        &WaveLimits {
            max_opportunities: options.max_opportunities_per_wave,
            max_notional_usd_micros: 1_000_000_000_000_000,
            max_per_tenant: options.max_opportunities_per_wave.clamp(1, 64),
            // The durable 64-lane executor is the admission ceiling. The
            // planner must not recreate a smaller fleet-wide bottleneck from
            // a speculative POLICY fee payer before shard selection.
            max_per_writable_conflict_key: 64,
        },
    )
    .map_err(|error| format!("fleet wave planning failed: {error:?}"))?;
    let planned_micros = started.elapsed().as_micros();
    if scoped_vault_ids.is_some() && !wave.deferred.is_empty() {
        return Ok(LivePlanningRun {
            output: json!({
                "status": "dirty_cohort_requires_full_sweep",
                "reason": "cohort_has_deferred_global_contenders",
                "mutating": false,
                "planningScope": "dirty_cohort",
                "scopedVaultCount": scoped_vault_ids.map(|ids| ids.len()).unwrap_or_default(),
                "deferredCount": wave.deferred.len(),
                "observationMicros": observed_micros,
                "observationAndPlanningMicros": planned_micros,
                "elapsedMicros": started.elapsed().as_micros(),
            }),
            evidence: None,
            fallback_to_full: true,
        });
    }
    let by_id = observation
        .opportunities
        .iter()
        .map(|opportunity| (opportunity.economics.opportunity_id, opportunity))
        .collect::<BTreeMap<_, _>>();

    let mut queue_inputs = Vec::with_capacity(wave.selected.len());
    let mut fee_budget_rejected_count = 0usize;
    let mut mint_lifetime_deferred_count = 0usize;
    let mut queued_notional_usd_micros = 0i128;
    let mut queued_lost_yield_usd_micros_per_hour = 0i128;
    let fee_policy = RouteFeePolicy::default();
    let publication_minimum_usable_until =
        Utc::now() + ChronoDuration::seconds(MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS);
    let optimizer_envelope_expires_at = observation.market_epoch.optimizer_envelope_expires_at();
    if optimizer_envelope_expires_at <= publication_minimum_usable_until {
        return Ok(LivePlanningRun {
            output: json!({
                "status": "no_fresh_market_epoch",
                "reason": "insufficient_lifetime_after_planning",
                "mutating": false,
                "planningScope": if scoped_vault_ids.is_some() { "dirty_cohort" } else { "full_fleet" },
                "observation": observation.stats,
                "minimumUsableEpochLifetimeSeconds": MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS,
                "epochExpiresAt": observation.market_epoch.expires_at,
                "optimizerEnvelopeExpiresAt": optimizer_envelope_expires_at,
                "minimumUsableUntil": publication_minimum_usable_until,
                "expiredOpportunitiesSwept": expired_opportunities_swept,
                "observationMicros": observed_micros,
                "observationAndPlanningMicros": planned_micros,
                "elapsedMicros": started.elapsed().as_micros(),
            }),
            evidence: None,
            fallback_to_full: scoped_vault_ids.is_some(),
        });
    }
    let optimizer_epoch_id = if options.dry_run {
        observation.market_epoch.optimizer_epoch_id
    } else {
        let durable_epoch = observation.market_epoch.durable_optimizer_epoch_evidence();
        let upserted = neon
            .upsert_optimizer_epoch(OptimizerEpochInput {
                cluster: options.cluster.clone(),
                epoch_key: durable_epoch.fingerprint.clone(),
                market_slot: durable_epoch.maximum_market_slot.unwrap_or_default(),
                observed_at: durable_epoch.captured_at,
                expires_at: durable_epoch.optimizer_envelope_expires_at(),
                market_state: serde_json::to_value(&durable_epoch)?,
            })
            .await;
        match upserted {
            Ok(epoch) => epoch.id,
            // The key is already claimed by different immutable evidence. The
            // stored row wins; publishing this wave against it would admit
            // routes under evidence they were not planned from. Re-observing
            // is the whole remedy, so this wave ends non-mutating instead of
            // taking the process down with it.
            Err(OrchestratorError::OptimizerEpochEvidenceConflict { epoch_key }) => {
                return Ok(LivePlanningRun {
                    output: json!({
                        "status": "optimizer_epoch_evidence_conflict",
                        "reason": "epoch_key_stored_under_different_immutable_evidence",
                        "mutating": false,
                        "planningScope": if scoped_vault_ids.is_some() { "dirty_cohort" } else { "full_fleet" },
                        "optimizerEpochKey": epoch_key,
                        "observation": observation.stats,
                        "expiredOpportunitiesSwept": expired_opportunities_swept,
                        "observationMicros": observed_micros,
                        "observationAndPlanningMicros": planned_micros,
                        "elapsedMicros": started.elapsed().as_micros(),
                        "reobservationScheduled": true,
                    }),
                    evidence: None,
                    fallback_to_full: scoped_vault_ids.is_some(),
                });
            }
            Err(error) => return Err(error.into()),
        }
    };
    for selected in &wave.selected {
        let observed = by_id
            .get(&selected.opportunity.opportunity_id)
            .ok_or("selected opportunity disappeared from immutable observation")?;
        let Some(route_expires_at) = observation.market_epoch.route_expires_at(
            &observed.source_liquidity_mint,
            &observed.target_liquidity_mint,
        ) else {
            mint_lifetime_deferred_count += 1;
            continue;
        };
        if route_expires_at <= publication_minimum_usable_until {
            mint_lifetime_deferred_count += 1;
            continue;
        }
        let admitted_target_apy = selected.economics.capacity_adjusted_target_net_apy_bps;
        let admitted_source_apy = selected.economics.capacity_adjusted_source_net_apy_bps;
        let annual_yield_gain = selected
            .economics
            .lost_yield_usd_micros_per_hour
            .saturating_mul(8_760)
            .max(1);
        let fee_budget =
            match route_fee_budget(selected.economics.net_holding_gain_usd_micros, fee_policy) {
                Ok(value) => value,
                Err(_) => {
                    fee_budget_rejected_count += 1;
                    continue;
                }
            };
        queued_notional_usd_micros = queued_notional_usd_micros
            .saturating_add(i128::from(observed.economics.notional_usd_micros));
        queued_lost_yield_usd_micros_per_hour = queued_lost_yield_usd_micros_per_hour
            .saturating_add(i128::from(
                selected.economics.lost_yield_usd_micros_per_hour,
            ));
        queue_inputs.push(rebalance_opportunity_input(
            observed,
            PublicationTerms {
                cluster: &options.cluster,
                optimizer_epoch_id,
                optimizer_market_slot: observation
                    .market_epoch
                    .maximum_market_slot
                    .unwrap_or_default(),
                admitted_source_apy_bps: admitted_source_apy,
                admitted_target_apy_bps: admitted_target_apy,
                annual_yield_gain_usd_micros: annual_yield_gain,
                expected_net_gain_usd_micros: selected.economics.net_holding_gain_usd_micros,
                economic_priority: selected.economics.total_priority,
                fee_cap_lamports: fee_budget.cap_lamports,
                fee_tier: fee_budget.tier.as_str(),
                fee_policy,
                available_at: Utc::now(),
                expires_at: route_expires_at,
            },
        ));
    }

    let queue_input_count = queue_inputs.len();
    let selected_vault_ids = queue_inputs
        .iter()
        .map(|input| input.vault_id.as_i64())
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect::<Vec<_>>();
    let planned_routes = queue_inputs
        .iter()
        .map(|input| {
            json!({
                "vaultId": input.vault_id.as_i64(),
                "sourceReserve": input.source_reserve,
                "targetReserve": input.target_reserve,
                "routeKind": input.execution_plan.get("route_kind"),
                "sourceLiquidityMint": input.execution_plan.get("source_liquidity_mint"),
                "targetLiquidityMint": input.execution_plan.get("target_liquidity_mint"),
                "estimatedExecutionCostUsdMicros": input.execution_plan.get("estimated_execution_cost_usd_micros"),
                "estimatedExecutionCosts": input.execution_plan.get("estimated_execution_costs"),
                "expiresAt": input.expires_at,
            })
        })
        .collect::<Vec<_>>();
    let publish = if options.dry_run {
        PublishWaveResult::default()
    } else {
        publish_wave(neon, queue_inputs).await?
    };
    let classified_selected_count = if options.dry_run {
        queue_input_count
    } else {
        let classified_publish_count = publish
            .published
            .saturating_add(publish.deferred_contention)
            .saturating_add(publish.deferred_lifetime);
        if classified_publish_count != queue_input_count {
            return Err(format!(
                "published, contention-deferred, and lifetime-deferred opportunities {classified_publish_count} do not partition queue inputs {queue_input_count}"
            )
            .into());
        }
        publish.published
    };
    let total_deferred_count = wave
        .deferred
        .len()
        .saturating_add(publish.deferred_contention)
        .saturating_add(publish.deferred_lifetime)
        .saturating_add(mint_lifetime_deferred_count);
    // This durable denominator is the economically admitted frontier that the
    // queue must drain, not every pre-economic route candidate. Capacity- and
    // fee-rejected candidates are already named outside this frontier.
    let planned_frontier_count = classified_selected_count.saturating_add(total_deferred_count);
    let retired_dirty_opportunities = if options.dry_run {
        0
    } else if let Some(vault_ids) = scoped_vault_ids {
        neon.retire_unselected_dirty_vault_opportunities(
            &options.cluster,
            vault_ids,
            &selected_vault_ids,
        )
        .await?
    } else {
        0
    };
    let elapsed_micros = started.elapsed().as_micros();
    let material_frontier_fingerprint = observation.market_epoch.material_frontier_fingerprint();
    let observed_opportunity_epoch_ids = observation
        .opportunities
        .iter()
        .map(|opportunity| opportunity.economics.optimizer_epoch_id)
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect::<Vec<_>>();
    let selected_opportunity_epoch_ids = wave
        .selected
        .iter()
        .map(|item| item.opportunity.optimizer_epoch_id)
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect::<Vec<_>>();
    let rejection_reason_counts =
        rejection_reason_counts(wave.rejected.iter().map(|rejected| &rejected.reason));
    let evidence = PlanningEvidence {
        optimizer_epoch_key: observation.market_epoch.fingerprint.clone(),
        material_frontier_fingerprint: material_frontier_fingerprint.clone(),
        material_frontier: current_material_frontier,
        optimizer_epoch_expires_at: optimizer_envelope_expires_at,
        observed_vault_count: observation.stats.eligible_vault_count,
        opportunity_count: i64::try_from(planned_frontier_count).unwrap_or(i64::MAX),
        selected_count: i64::try_from(classified_selected_count).unwrap_or(i64::MAX),
        deferred_count: i64::try_from(total_deferred_count).unwrap_or(i64::MAX),
        complete_frontier: total_deferred_count == 0,
    };
    let fleet_completeness = json!({
        "eligibleManagedVaults": observation.stats.eligible_vault_count,
        "sourceCandidateVaults": observation.stats.source_candidate_vault_count,
        "opportunityVaults": observation.stats.opportunity_vault_count,
        "activeOpportunityVaultsExcluded": observation.stats.active_opportunity_vaults_excluded,
        "activeOpportunityVaultsExcludedByState": observation.stats.active_opportunity_vaults_excluded_by_state,
        "noPositiveCurrentSourceVaults": observation.stats.no_positive_current_source_vault_count,
        "vaultOutcomesByReason": observation.stats.vault_outcomes_by_reason,
        "fleetVaultsAccounted": observation.stats.accounted_vault_count,
        "completeVaultAccounting": observation.stats.complete_vault_accounting,
    });
    let mut output = json!({
    "status": "planned",
    "mode": if options.dry_run { "live_read_only" } else { "durable_publish" },
    "queueSchemaAvailable": queue_schema_available,
    "planningScope": if scoped_vault_ids.is_some() { "dirty_cohort" } else { "full_fleet" },
    "scopedVaultCount": scoped_vault_ids.map(|ids| ids.len()).unwrap_or_default(),
    "cluster": options.cluster,
    "mutating": !options.dry_run,
    "childProcessesSpawned": 0,
    "epochFingerprint": observation.market_epoch.fingerprint,
    "epochExpiresAt": observation.market_epoch.expires_at,
    "marketEpochOptimizerId": observation.market_epoch.optimizer_epoch_id,
    "observedOpportunityEpochIds": observed_opportunity_epoch_ids,
    "selectedOpportunityEpochIds": selected_opportunity_epoch_ids,
    "marketReserveCount": observation.market_epoch.reserves.len(),
    "fleetCompleteness": fleet_completeness,
    "committedTargetInflowReserveCount": observation.stats.committed_target_inflow_reserve_count,
    "committedTargetInflowUsdMicros": observation.stats.committed_target_inflow_usd_micros,
    "observation": observation.stats,
    "capacitySelectedCount": wave.selected.len(),
    "eligibleCount": queue_input_count,
    "feeBudgetRejectedCount": fee_budget_rejected_count,
    "capacityDeferredCount": wave.deferred.len(),
    "deferredCount": total_deferred_count,
    "publishContentionDeferredCount": publish.deferred_contention,
    "totalDeferredCount": total_deferred_count,
    "rejectedCount": wave.rejected.len(),
    "publishedCount": publish.published,
    "retiredDirtyOpportunityCount": retired_dirty_opportunities,
    "expiredOpportunitiesSwept": expired_opportunities_swept,
    "selectedNotionalUsdMicros": i64::try_from(queued_notional_usd_micros).unwrap_or(i64::MAX),
    "selectedLostYieldUsdMicrosPerHour": i64::try_from(queued_lost_yield_usd_micros_per_hour).unwrap_or(i64::MAX),
    "observationMicros": observed_micros,
    "observationAndPlanningMicros": planned_micros,
    "elapsedMicros": elapsed_micros,
    "planningUnderFiveSeconds": planned_micros < 5_000_000,
    "topValueCohort": wave.selected.iter().take(20).map(|item| json!({
        "vaultId": item.opportunity.vault_id,
        "notionalUsdMicros": item.opportunity.notional_usd_micros,
        "lostYieldUsdMicrosPerHour": item.economics.lost_yield_usd_micros_per_hour,
        "priority": item.economics.total_priority,
    })).collect::<Vec<_>>(),
    });
    if let Some(fields) = output.as_object_mut() {
        fields.insert(
            "crossMintJupiterEnabled".to_owned(),
            json!(options.enable_cross_mint_jupiter),
        );
        fields.insert("plannedRoutes".to_owned(), json!(planned_routes));
        fields.insert(
            "rejectionReasonCounts".to_owned(),
            json!(rejection_reason_counts),
        );
        fields.insert(
            "materialMarketFrontierFingerprint".to_owned(),
            json!(material_frontier_fingerprint),
        );
        fields.insert(
            "plannedFrontierCount".to_owned(),
            json!(planned_frontier_count),
        );
        fields.insert(
            "classifiedSelectedCount".to_owned(),
            json!(classified_selected_count),
        );
        fields.insert(
            "optimizerEnvelopeExpiresAt".to_owned(),
            json!(optimizer_envelope_expires_at),
        );
        fields.insert(
            "mintLifetimeDeferredCount".to_owned(),
            json!(mint_lifetime_deferred_count),
        );
        fields.insert(
            "publishLifetimeDeferredCount".to_owned(),
            json!(publish.deferred_lifetime),
        );
        fields.insert("marketWakePolicy".to_owned(), market_wake_policy_evidence());
    }
    Ok(LivePlanningRun {
        output,
        evidence: Some(evidence),
        fallback_to_full: scoped_vault_ids.is_some()
            && (publish.deferred_contention > 0
                || publish.deferred_lifetime > 0
                || mint_lifetime_deferred_count > 0),
    })
}

fn print_output(output: &Value, json_output: bool) -> Result<(), Box<dyn Error>> {
    if json_output {
        println!("{}", serde_json::to_string(output)?);
    } else {
        println!("{}", serde_json::to_string_pretty(output)?);
    }
    Ok(())
}

struct PlannerLogLimiter {
    next_heartbeat_at: Instant,
}

impl PlannerLogLimiter {
    fn new() -> Self {
        Self {
            next_heartbeat_at: Instant::now(),
        }
    }

    fn should_emit(&mut self, output: &Value) -> bool {
        let published = output
            .get("publishedCount")
            .and_then(Value::as_u64)
            .unwrap_or(0)
            > 0;
        let heartbeat_due = Instant::now() >= self.next_heartbeat_at;
        if published || heartbeat_due {
            self.next_heartbeat_at =
                Instant::now() + Duration::from_secs(DEFAULT_PLANNER_LOG_HEARTBEAT_SECONDS);
            true
        } else {
            false
        }
    }
}

fn print_daemon_output(
    output: &Value,
    json_output: bool,
    limiter: &mut PlannerLogLimiter,
) -> Result<(), Box<dyn Error>> {
    if limiter.should_emit(output) {
        print_output(output, json_output)?;
    }
    Ok(())
}

async fn durable_fleet_schema_available(neon: &NeonSqlClient) -> Result<bool, Box<dyn Error>> {
    Ok(loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT to_regclass('loyal_yield.rebalance_opportunities') IS NOT NULL
           AND to_regclass('loyal_yield.target_capacity_reservations') IS NOT NULL
           AND EXISTS (
               SELECT 1 FROM information_schema.columns
               WHERE table_schema = 'loyal_yield'
                 AND table_name = 'rebalance_opportunities'
                 AND column_name = 'rediscovery_key'
           )
           AND EXISTS (
               SELECT 1 FROM information_schema.columns
               WHERE table_schema = 'loyal_yield'
                 AND table_name = 'rebalance_opportunities'
                 AND column_name = 'attempt_generation'
           )
        "#,
    )
    .fetch_one(neon.pool())
    .await?)
}

async fn run_full_sweep(
    options: &Options,
    neon: &NeonSqlClient,
    timescale: &TimescaleRouterClient,
    delegated_signer: &str,
    config: &FleetObservationConfig,
) -> Result<LivePlanningRun, Box<dyn Error>> {
    let full_sweep_started_at = Utc::now();
    let mut run = run_live_once(
        options,
        neon,
        timescale,
        delegated_signer,
        config,
        None,
        None,
        true,
        true,
    )
    .await?;
    if options.dry_run {
        return Ok(run);
    }
    let Some(evidence) = run.evidence.as_ref() else {
        return Ok(run);
    };
    let full_sweep_completed_at = Utc::now();
    let state = neon
        .record_fleet_planning_full_sweep(FleetPlanningStateInput {
            cluster: options.cluster.clone(),
            full_sweep_started_at,
            full_sweep_completed_at,
            optimizer_epoch_key: evidence.optimizer_epoch_key.clone(),
            optimizer_epoch_expires_at: evidence.optimizer_epoch_expires_at,
            complete_frontier: evidence.complete_frontier,
            observed_vault_count: evidence.observed_vault_count,
            opportunity_count: evidence.opportunity_count,
            selected_count: evidence.selected_count,
            deferred_count: evidence.deferred_count,
        })
        .await?;
    let cleared = neon
        .clear_fleet_planning_dirty_observed_before(&options.cluster, full_sweep_started_at)
        .await?;
    if let Some(output) = run.output.as_object_mut() {
        output.insert("fullSweepGeneration".to_owned(), json!(state.generation));
        output.insert(
            "completeGlobalFrontier".to_owned(),
            json!(state.complete_frontier),
        );
        output.insert("dirtyHintsCovered".to_owned(), json!(cleared));
    }
    Ok(run)
}

fn dirty_vault_ids(leases: &[FleetPlanningDirtyVaultLease]) -> Vec<i64> {
    leases
        .iter()
        .map(|lease| lease.dirty.vault_id.as_i64())
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect()
}

/// Backs a failing planner cycle off exponentially up to a bounded ceiling.
///
/// The planner is the only writer of its own dirty-hint leases, so retrying
/// fast is safe; the ceiling exists so a sustained outage does not turn the
/// recovery poll into a hot loop against Neon or Timescale.
fn planner_cycle_backoff(consecutive_failures: u32) -> Duration {
    let seconds = 1u64
        .checked_shl(consecutive_failures.saturating_sub(1).min(u32::BITS - 1))
        .unwrap_or(PLANNER_MAXIMUM_CYCLE_BACKOFF_SECONDS)
        .min(PLANNER_MAXIMUM_CYCLE_BACKOFF_SECONDS);
    Duration::from_secs(seconds)
}

fn error_chain<'a>(
    error: &'a (dyn Error + 'static),
) -> impl Iterator<Item = &'a (dyn Error + 'static)> {
    std::iter::successors(Some(error), |source| (*source).source())
}

fn is_retryable_postgres_sqlstate(code: &str) -> bool {
    code.starts_with("08")
        || matches!(
            code,
            "40001" | "40P01" | "53300" | "53400" | "55P03" | "57P01" | "57P02" | "57P03"
        )
}

/// Classifies only failures that can clear while the already-validated pools
/// remain usable. Store invariants, decode/schema errors, bad SQL, and unknown
/// failures must escape to `main` so Render restarts and pages the worker.
fn is_retryable_sqlx_error(error: &loyal_yield_orchestrator::sqlx::Error) -> bool {
    use loyal_yield_orchestrator::sqlx::Error as SqlxError;

    match error {
        SqlxError::Io(_)
        | SqlxError::Tls(_)
        | SqlxError::PoolTimedOut
        | SqlxError::WorkerCrashed
        | SqlxError::BeginFailed => true,
        SqlxError::Database(database) => database
            .code()
            .as_deref()
            .is_some_and(is_retryable_postgres_sqlstate),
        _ => false,
    }
}

fn is_retryable_planner_cycle_error(error: &(dyn Error + 'static)) -> bool {
    error_chain(error).any(|source| {
        source
            .downcast_ref::<loyal_yield_orchestrator::sqlx::Error>()
            .is_some_and(is_retryable_sqlx_error)
    })
}

/// Runs the exact production recovery classifier and backoff policy at a load
/// larger than the current managed-vault fleet without opening external pools.
/// The outer verifier starts this through the real worker binary.
fn run_planner_recovery_verification_probe() -> Result<Value, Box<dyn Error>> {
    let mut maximum_backoff = Duration::ZERO;
    for index in 0..PLANNER_RECOVERY_VERIFICATION_CYCLES {
        let error = OrchestratorError::Sqlx(loyal_yield_orchestrator::sqlx::Error::PoolTimedOut);
        if !is_retryable_planner_cycle_error(&error) {
            return Err(format!("transient pool timeout was fatal at cycle {index}").into());
        }
        maximum_backoff = maximum_backoff.max(planner_cycle_backoff(
            u32::try_from(index + 1).unwrap_or(u32::MAX),
        ));
    }

    let invariant =
        OrchestratorError::StoreInvariant("verification invariant must remain fatal".to_owned());
    if is_retryable_planner_cycle_error(&invariant) {
        return Err("store invariant was incorrectly classified as retryable".into());
    }
    let closed_pool = OrchestratorError::Sqlx(loyal_yield_orchestrator::sqlx::Error::PoolClosed);
    if is_retryable_planner_cycle_error(&closed_pool) {
        return Err("closed pool was incorrectly classified as retryable".into());
    }
    if maximum_backoff != Duration::from_secs(PLANNER_MAXIMUM_CYCLE_BACKOFF_SECONDS) {
        return Err("planner recovery backoff did not reach its bounded ceiling".into());
    }

    Ok(json!({
        "status": "pass",
        "worker": "loyal-fleet-opportunity-planner",
        "simulatedCycleCount": PLANNER_RECOVERY_VERIFICATION_CYCLES,
        "retryableTransientCycleCount": PLANNER_RECOVERY_VERIFICATION_CYCLES,
        "fatalInvariantCount": 1,
        "fatalClosedPoolCount": 1,
        "maximumBackoffSeconds": maximum_backoff.as_secs(),
    }))
}

fn planner_owner(cluster: &str) -> String {
    format!(
        "fleet-opportunity-planner:{cluster}:{}:{}",
        std::process::id(),
        Utc::now().timestamp_micros()
    )
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    if env::args().skip(1).eq(["--role-probe"]) {
        println!(
            "{}",
            serde_json::to_string(&fleet_worker_role_probe(FleetWorkerRole::Planner))?
        );
        return Ok(());
    }
    if env::args().skip(1).eq(["--recovery-verification-probe"]) {
        println!(
            "{}",
            serde_json::to_string(&run_planner_recovery_verification_probe()?)?
        );
        return Ok(());
    }
    let _observability = init_from_env("loyal-fleet-opportunity-planner")?;
    if let Err(error) = run().await {
        OperationalError::new(
            "fleet_opportunity_planner_fatal",
            "run_fleet_opportunity_planner",
            "Fleet opportunity planner stopped after a fatal error",
        )
        .retryable(true)
        .recovery_required(true)
        .emit();
        return Err(error);
    }
    Ok(())
}

async fn run() -> Result<(), Box<dyn Error>> {
    let options = parse_options()?;
    if options.benchmark {
        let output = run_benchmark(&options)?;
        let passed = output.get("status").and_then(Value::as_str) == Some("pass");
        print_output(&output, options.json)?;
        return if passed {
            Ok(())
        } else {
            Err("10,000-vault replay exceeded 10 seconds p95".into())
        };
    }

    let neon_url = env::var("NEON_DATABASE_URL").map_err(|_| "NEON_DATABASE_URL is required")?;
    let timescale_url = env::var("TIMESCALEDB_URL").map_err(|_| "TIMESCALEDB_URL is required")?;
    // Discovery needs only the public delegated-signer identity. Private
    // POLICY key material is mounted exclusively into roles that sign or fund
    // transactions (executor and ALT provisioner).
    let delegated_signer = STANDARD_POLICY_AUTHORITY.to_owned();
    let enabled_mints = enabled_stable_mints_from_env()?;
    let config = live_observation_config(
        &options.cluster,
        enabled_mints,
        options.enable_cross_mint_jupiter,
    )?;
    let neon = NeonSqlClient::connect(
        NeonSqlConfig::new(neon_url).with_max_connections(DEFAULT_QUEUE_CONNECTIONS),
    )
    .await?;
    let timescale = TimescaleRouterClient::connect(
        TimescaleRouterClientConfig::new(timescale_url)
            .with_schema("kamino")
            .with_max_connections(2),
    )
    .await?;

    if options.once && options.dry_run {
        let queue_schema_available = durable_fleet_schema_available(&neon).await?;
        let mut run = run_live_once(
            &options,
            &neon,
            &timescale,
            &delegated_signer,
            &config,
            None,
            None,
            true,
            queue_schema_available,
        )
        .await?;
        if let Some(output) = run.output.as_object_mut() {
            output.insert(
                "queueCapacityAccounting".to_owned(),
                json!(if queue_schema_available {
                    "included"
                } else {
                    "unavailable_pre_migration_27"
                }),
            );
        }
        print_output(&run.output, options.json)?;
        return Ok(());
    }

    neon.require_schema_migration(27, "rebalance_opportunity_attempt_generations")
        .await?;
    neon.require_schema_migration(29, "fleet_commit_lifetime_fences")
        .await?;
    neon.require_schema_migration(30, "fused_queue_accrual_binding")
        .await?;
    neon.register_fleet_planning_cluster(&options.cluster)
        .await?;
    let mut wakeup_listener = DurablePgWakeupListener::new("loyal_yield_fleet_planner_wakeup")?;
    let owner = planner_owner(&options.cluster);
    let recovery_poll = Duration::from_secs(options.poll_interval_seconds);
    let mut next_full_sweep_at = Instant::now();
    let market_probe_interval = Duration::from_secs(DEFAULT_MARKET_PROBE_INTERVAL_SECONDS);
    let mut next_market_probe_at = Instant::now() + market_probe_interval;
    let mut current_market_epoch_key = None::<String>;
    let mut current_material_frontier_fingerprint = None::<String>;
    let mut current_material_frontier = None::<MaterialMarketFrontier>;
    let mut consecutive_cycle_failures = 0u32;
    let mut log_limiter = PlannerLogLimiter::new();
    loop {
        // One planning cycle is the supervision unit. Startup already
        // fail-fast validated config, migrations, and both pools, so anything
        // that fails here is a transient store or market condition. Exiting
        // would only hand the same work to a fresh process after losing the
        // wakeup listener and the in-memory market baseline.
        let cycle: Result<(), Box<dyn Error>> = async {
            neon.heartbeat_fleet_planning_cluster(&options.cluster)
                .await?;
        if Instant::now() < next_full_sweep_at && Instant::now() >= next_market_probe_at {
            next_market_probe_at = Instant::now() + market_probe_interval;
            match observe_market_epoch(&timescale, &config).await {
                Ok(epoch) => {
                    let next_material_frontier = epoch.material_frontier_fingerprint();
                    let next_frontier = epoch.material_market_frontier();
                    let disposition = current_material_frontier
                        .as_ref()
                        .map(|current| current.disposition_against(&next_frontier));
                    if disposition.is_none_or(|disposition| !disposition.allows_scoped_planning()) {
                        eprintln!(
                            "{}",
                            json!({
                                "status": "fleet_material_market_frontier_changed",
                                "materialFrontierDisposition": disposition,
                                "marketProbeIntervalSeconds": DEFAULT_MARKET_PROBE_INTERVAL_SECONDS,
                                "previousEpochFingerprint": current_market_epoch_key.as_deref(),
                                "nextEpochFingerprint": epoch.fingerprint,
                                "previousMaterialMarketFrontierFingerprint": current_material_frontier_fingerprint.as_deref(),
                                "nextMaterialMarketFrontierFingerprint": next_material_frontier,
                                "marketWakePolicy": market_wake_policy_evidence(),
                                "fullSweepScheduledImmediately": true,
                            })
                        );
                        next_full_sweep_at = Instant::now();
                    }
                }
                Err(_) => {
                    eprintln!(
                        "{}",
                        json!({
                            "status": "fleet_market_epoch_probe_unavailable",
                            "marketProbeIntervalSeconds": DEFAULT_MARKET_PROBE_INTERVAL_SECONDS,
                            "durableRecoveryPollingActive": true,
                        })
                    );
                }
            }
        }
        let full_sweep_due = Instant::now() >= next_full_sweep_at;
        if full_sweep_due {
            let mut run =
                run_full_sweep(&options, &neon, &timescale, &delegated_signer, &config).await?;
            if let Some(evidence) = run.evidence.as_ref() {
                current_market_epoch_key = Some(evidence.optimizer_epoch_key.clone());
                current_material_frontier_fingerprint =
                    Some(evidence.material_frontier_fingerprint.clone());
                current_material_frontier = Some(evidence.material_frontier.clone());
            }
            let delay = next_full_sweep_delay(&run, &options);
            annotate_full_sweep_schedule(&mut run, delay);
            if let Some(output) = run.output.as_object_mut() {
                output.insert(
                    "marketProbeIntervalSeconds".to_owned(),
                    json!(DEFAULT_MARKET_PROBE_INTERVAL_SECONDS),
                );
            }
            print_daemon_output(&run.output, options.json, &mut log_limiter)?;
            next_full_sweep_at = Instant::now() + delay;
            next_market_probe_at = Instant::now() + market_probe_interval;
        } else {
            let leases = neon
                .lease_fleet_planning_dirty_vaults(
                    &options.cluster,
                    &owner,
                    Utc::now() + ChronoDuration::seconds(DIRTY_LEASE_SECONDS),
                    i64::try_from(options.dirty_batch_size).unwrap_or(1_024),
                )
                .await?;
            if !leases.is_empty() {
                let vault_ids = dirty_vault_ids(&leases);
                let planning_state = neon.fleet_planning_state(&options.cluster).await?;
                let baseline_matches_durable_state = planning_state.as_ref().is_some_and(|state| {
                    current_market_epoch_key.as_deref() == Some(state.optimizer_epoch_key.as_str())
                        && current_material_frontier.is_some()
                });
                let frontier_fresh = planning_state.as_ref().is_some_and(|state| {
                    state.complete_frontier
                        && baseline_matches_durable_state
                        && state.optimizer_epoch_expires_at > Utc::now()
                        && state.full_sweep_completed_at
                            >= Utc::now()
                                - ChronoDuration::seconds(
                                    i64::try_from(options.full_sweep_interval_seconds)
                                        .unwrap_or(i64::MAX),
                                )
                });
                // Do not compare the target-capacity frontier's optimistic
                // `version` here: it intentionally advances on every newer
                // telemetry slot and reservation transition, which would turn
                // normal chain churn back into an O(fleet) scan. This scoped
                // observation already recomputes against every non-released
                // durable reservation; the executor then atomically fences
                // admission against the latest per-target capacity version.
                let required_material_frontier = current_material_frontier.as_ref();
                let mut dirty_run = run_live_once(
                    &options,
                    &neon,
                    &timescale,
                    &delegated_signer,
                    &config,
                    Some(&vault_ids),
                    required_material_frontier,
                    frontier_fresh,
                    true,
                )
                .await?;
                if dirty_run.fallback_to_full {
                    print_daemon_output(&dirty_run.output, options.json, &mut log_limiter)?;
                    let mut full_run =
                        run_full_sweep(&options, &neon, &timescale, &delegated_signer, &config)
                            .await?;
                    if let Some(evidence) = full_run.evidence.as_ref() {
                        current_market_epoch_key = Some(evidence.optimizer_epoch_key.clone());
                        current_material_frontier_fingerprint =
                            Some(evidence.material_frontier_fingerprint.clone());
                        current_material_frontier = Some(evidence.material_frontier.clone());
                    }
                    let delay = next_full_sweep_delay(&full_run, &options);
                    annotate_full_sweep_schedule(&mut full_run, delay);
                    let acknowledged = if full_run.evidence.is_some() {
                        neon.acknowledge_fleet_planning_dirty_vaults(&leases)
                            .await?
                    } else {
                        neon.retry_fleet_planning_dirty_vaults(
                            &leases,
                            Utc::now() + ChronoDuration::seconds(1),
                        )
                        .await?
                    };
                    if let Some(output) = full_run.output.as_object_mut() {
                        output.insert("triggeredByDirtyCohort".to_owned(), json!(vault_ids.len()));
                        output.insert("dirtyHintLeaseActions".to_owned(), json!(acknowledged));
                    }
                    print_daemon_output(&full_run.output, options.json, &mut log_limiter)?;
                    next_full_sweep_at = Instant::now() + delay;
                    next_market_probe_at = Instant::now() + market_probe_interval;
                } else {
                    let acknowledged = neon
                        .acknowledge_fleet_planning_dirty_vaults(&leases)
                        .await?;
                    if let Some(output) = dirty_run.output.as_object_mut() {
                        output.insert("dirtyHintLeaseActions".to_owned(), json!(acknowledged));
                        output.insert("fullFleetRowsRead".to_owned(), json!(0));
                    }
                    print_daemon_output(&dirty_run.output, options.json, &mut log_limiter)?;
                }
            }
            }
            Ok(())
        }
        .await;
        match cycle {
            Ok(()) => consecutive_cycle_failures = 0,
            // A one-shot run has no next cycle to recover into, so it keeps
            // reporting failure to its caller.
            Err(error) if options.once => return Err(error),
            Err(error) if is_retryable_planner_cycle_error(error.as_ref()) => {
                consecutive_cycle_failures = consecutive_cycle_failures.saturating_add(1);
                let backoff = planner_cycle_backoff(consecutive_cycle_failures);
                OperationalError::new(
                    "fleet_opportunity_planner_cycle_failed",
                    "run_fleet_opportunity_planner_cycle",
                    "Fleet opportunity planner cycle failed and will retry",
                )
                .retryable(true)
                .recovery_required(false)
                .emit();
                // The alert channel deliberately carries only stable
                // classifications, so the operator-facing cause goes to stderr
                // alongside it. Losing that text is what made this failure
                // mode expensive to diagnose.
                eprintln!(
                    "{}",
                    json!({
                        "status": "fleet_opportunity_planner_cycle_failed",
                        "consecutiveFailures": consecutive_cycle_failures,
                        "retryAfterMilliseconds": u64::try_from(backoff.as_millis())
                            .unwrap_or(u64::MAX),
                        "error": error.to_string(),
                        "durableRecoveryPollingActive": true,
                    })
                );
                tokio::time::sleep(backoff).await;
                continue;
            }
            // Unknown failures and invariant violations must still terminate
            // the process. Keeping a logically dead planner alive would hide
            // fleet-wide non-progress from Render's restart supervision.
            Err(error) => return Err(error),
        }
        if options.once {
            break;
        }
        let until_full_sweep = next_full_sweep_at.saturating_duration_since(Instant::now());
        let until_market_probe = next_market_probe_at.saturating_duration_since(Instant::now());
        let wakeup_event = wakeup_listener
            .wait(
                neon.pool(),
                recovery_poll.min(until_full_sweep).min(until_market_probe),
            )
            .await;
        log_planner_wakeup_event(&wakeup_event);
    }
    Ok(())
}

fn log_planner_wakeup_event(event: &DurablePgWakeupEvent) {
    let (status, retry_after_milliseconds) = match event {
        DurablePgWakeupEvent::RecoveryPollElapsed | DurablePgWakeupEvent::Notification => return,
        DurablePgWakeupEvent::Reconnected => ("fleet_planner_wakeup_listener_reconnected", None),
        DurablePgWakeupEvent::Disconnected { retry_after, .. } => (
            "fleet_planner_wakeup_listener_disconnected",
            Some(u64::try_from(retry_after.as_millis()).unwrap_or(u64::MAX)),
        ),
        DurablePgWakeupEvent::ReconnectFailed { retry_after, .. } => (
            "fleet_planner_wakeup_listener_reconnect_failed",
            Some(u64::try_from(retry_after.as_millis()).unwrap_or(u64::MAX)),
        ),
    };
    eprintln!(
        "{}",
        json!({
            "status": status,
            "retryAfterMilliseconds": retry_after_milliseconds,
            "durableRecoveryPollingActive": true,
            "immediateDurableScanScheduled": event.requires_immediate_durable_scan(),
            "errorRedacted": matches!(
                event,
                DurablePgWakeupEvent::Disconnected { .. }
                    | DurablePgWakeupEvent::ReconnectFailed { .. }
            ),
        })
    );
}

#[cfg(test)]
mod tests {
    use super::*;
    use loyal_yield_orchestrator::fleet_orchestration::{
        CandidateExecutionCosts, CandidateRouteKind, ObservedEarnPolicyEvidence, OpportunityInput,
    };
    use loyal_yield_orchestrator::sqlx::postgres::PgPoolOptions;

    #[test]
    fn daemon_log_limiter_preserves_progress_and_coalesces_idle_cycles() {
        let mut limiter = PlannerLogLimiter {
            next_heartbeat_at: Instant::now() + Duration::from_secs(60),
        };

        assert!(!limiter.should_emit(&json!({"status": "idle"})));
        assert!(limiter.should_emit(&json!({"publishedCount": 1})));
        assert!(!limiter.should_emit(&json!({"publishedCount": 0})));
    }

    #[tokio::test]
    async fn generated_cross_mint_candidate_passes_actual_store_publication_validation() {
        let now = Utc::now();
        let observed = ObservedFleetOpportunity {
            economics: OpportunityInput {
                opportunity_id: 1,
                optimizer_epoch_id: 7,
                vault_id: 11,
                tenant_id: "test-tenant".to_owned(),
                source_snapshot_id: 13,
                observed_slot: 17,
                mint: "source-mint".to_owned(),
                source_reserve: "source-reserve".to_owned(),
                target_reserve: "target-reserve".to_owned(),
                notional_usd_micros: 1_000_000,
                source_net_apy_bps: 100,
                target_net_apy_bps: 200,
                confidence_ppm: 950_000,
                expected_service_millis: 45_000,
                holding_horizon_seconds: 86_400,
                estimated_execution_cost_usd_micros: 5_000,
                age_seconds: 0,
                fairness_credit: 0,
                writable_conflict_keys: vec!["vault:test".to_owned()],
            },
            route_kind: CandidateRouteKind::CrossMintJupiter,
            source_liquidity_mint: "source-mint".to_owned(),
            target_liquidity_mint: "target-mint".to_owned(),
            estimated_execution_costs: CandidateExecutionCosts::CrossMintJupiter {
                withdraw_usd_micros: 1_000,
                jupiter_swap_usd_micros: 3_000,
                deposit_usd_micros: 1_000,
            },
            cross_mint_maximum_value_loss_bps: Some(50),
            jupiter_swap_lane: Some(
                loyal_yield_store::fleet_orchestration::CrossMintSwapPolicyBinding {
                    policy_account: "swap-policy".to_owned(),
                    source_shard: "classic".to_owned(),
                    enrollment_generation: 1,
                    observed_slot: 23,
                    observed_signature: "swap-signature".to_owned(),
                    source_commitment: "finalized".to_owned(),
                    max_slippage_bps: 50,
                    daily_source_mint_spending_cap: 1_000_000,
                    manifest_fingerprint: "a".repeat(64),
                },
            ),
            source_earn_policy: Some(ObservedEarnPolicyEvidence {
                settings: "test-settings".to_owned(),
                authority: "test-authority".to_owned(),
                policy_account: "withdraw-policy".to_owned(),
                vault_index: 1,
                vault_pubkey: "test-vault".to_owned(),
                delegated_signer: "test-signer".to_owned(),
                threshold: 1,
                stable_mints: vec!["source-mint".to_owned()],
                kamino_markets: vec!["source-market".to_owned()],
                kamino_liquidity_mints: vec!["source-mint".to_owned()],
                source_commitment: "finalized".to_owned(),
                observed_slot: 19,
                observed_signature: "withdraw-signature".to_owned(),
            }),
            target_earn_policy: Some(ObservedEarnPolicyEvidence {
                settings: "test-settings".to_owned(),
                authority: "test-authority".to_owned(),
                policy_account: "deposit-policy".to_owned(),
                vault_index: 1,
                vault_pubkey: "test-vault".to_owned(),
                delegated_signer: "test-signer".to_owned(),
                threshold: 1,
                stable_mints: vec!["target-mint".to_owned()],
                kamino_markets: vec!["target-market".to_owned()],
                kamino_liquidity_mints: vec!["target-mint".to_owned()],
                source_commitment: "finalized".to_owned(),
                observed_slot: 21,
                observed_signature: "deposit-signature".to_owned(),
            }),
            base_policy_account: "base-policy".to_owned(),
            base_policy_delegated_signer: "test-signer".to_owned(),
            base_policy_source_commitment: "finalized".to_owned(),
            base_policy_observed_slot: 19,
            base_policy_observed_signature: "base-signature".to_owned(),
            source_kind: ObservedSourceKind::ReservePosition,
            policy_id: 19,
            settings: "test-settings".to_owned(),
            vault_index: 1,
            vault_pubkey: "test-vault".to_owned(),
            amount_raw: 1_000_000,
            route_amount_semantics: "redeemable_liquidity".to_owned(),
            source_amount_semantics: Some("collateral_deposited".to_owned()),
            source_collateral_amount_raw: Some(1_000_000),
            redeemable_source_liquidity_amount_raw: Some(1_000_000),
            idle_vault_liquidity_amount_raw: None,
            idle_token_account: None,
            source_observed_slot: 17,
            source_observed_at: now,
            target_observed_at: now,
            target_observed_slot: 17,
        };
        let input = rebalance_opportunity_input(
            &observed,
            PublicationTerms {
                cluster: "localnet",
                optimizer_epoch_id: 7,
                optimizer_market_slot: 17,
                admitted_source_apy_bps: 100,
                admitted_target_apy_bps: 200,
                annual_yield_gain_usd_micros: 10_000,
                expected_net_gain_usd_micros: 5_000,
                economic_priority: 1,
                fee_cap_lamports: 1_000,
                fee_tier: "standard",
                fee_policy: RouteFeePolicy::default(),
                available_at: now,
                expires_at: now + ChronoDuration::minutes(5),
            },
        );

        assert_eq!(input.liquidity_mint, "target-mint");
        assert_eq!(
            input
                .execution_plan
                .get("source_liquidity_mint")
                .and_then(Value::as_str),
            Some("source-mint")
        );
        assert_eq!(
            input
                .execution_plan
                .get("target_liquidity_mint")
                .and_then(Value::as_str),
            Some("target-mint")
        );
        let bindings = input
            .execution_plan
            .get("policy_bindings")
            .expect("cross-mint publication must freeze policy bindings");
        assert_eq!(
            bindings,
            &json!({
                "settings": "test-settings",
                "vault_index": 1,
                "vault_pubkey": "test-vault",
                "delegated_signer": "test-signer",
                "withdraw": {
                    "policy_account": "withdraw-policy",
                    "observed_slot": 19,
                    "observed_signature": "withdraw-signature",
                    "source_commitment": "finalized",
                    "constraint_index": 0,
                },
                "swap": {
                    "policy_account": "swap-policy",
                    "source_shard": "classic",
                    "enrollment_generation": 1,
                    "observed_slot": 23,
                    "observed_signature": "swap-signature",
                    "source_commitment": "finalized",
                    "max_slippage_bps": 50,
                    "daily_source_mint_spending_cap": 1_000_000,
                    "manifest_fingerprint": "a".repeat(64),
                },
                "deposit": {
                    "policy_account": "deposit-policy",
                    "observed_slot": 21,
                    "observed_signature": "deposit-signature",
                    "source_commitment": "finalized",
                    "constraint_index": 1,
                },
            })
        );
        assert_ne!(
            bindings
                .pointer("/withdraw/policy_account")
                .and_then(Value::as_str),
            bindings
                .pointer("/deposit/policy_account")
                .and_then(Value::as_str),
            "cross-class routes must preserve independent Earn policy accounts"
        );
        assert!(bindings.get("base").is_none());
        assert!(input
            .execution_plan
            .get("route_fingerprint_material")
            .is_none());
        assert!(input
            .execution_plan
            .get("requirements_fingerprint_material")
            .is_none());
        assert_eq!(
            input
                .execution_plan
                .pointer("/estimated_execution_costs/jupiter_swap_usd_micros")
                .and_then(Value::as_i64),
            Some(3_000)
        );
        assert_eq!(
            input
                .execution_plan
                .get("estimated_execution_cost_usd_micros")
                .and_then(Value::as_i64),
            Some(5_000),
            "the 50 bps protection cap must not be priced as expected loss"
        );
        assert_eq!(
            input
                .execution_plan
                .get("cross_mint_maximum_value_loss_bps")
                .and_then(Value::as_u64),
            Some(50)
        );

        // upsert_rebalance_opportunity runs the production validator before
        // opening its transaction. A deliberately unreachable lazy pool lets
        // this test prove validation accepted the generated input without
        // mutating a database.
        let pool = PgPoolOptions::new()
            .acquire_timeout(std::time::Duration::from_millis(25))
            .connect_lazy("postgres://planner-test@127.0.0.1:1/planner-test")
            .expect("construct deliberately unreachable lazy pool");
        let client = NeonSqlClient::from_pool(pool);
        let error = client
            .upsert_rebalance_opportunity(input)
            .await
            .expect_err("publication should reach the deliberately unavailable database");
        assert!(
            matches!(error, OrchestratorError::Sqlx(_)),
            "generated planner input was rejected before the database boundary: {error}"
        );
    }
}
