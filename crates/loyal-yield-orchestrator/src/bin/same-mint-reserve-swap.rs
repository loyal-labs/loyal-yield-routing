use std::process::Command;
use std::{
    collections::{BTreeMap, BTreeSet},
    convert::TryInto,
    env,
    error::Error,
    panic::{catch_unwind, AssertUnwindSafe},
    str::FromStr,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc, Mutex,
    },
    time::{Duration, Instant},
};

use chrono::{DateTime, Duration as ChronoDuration, Utc};
use klend_interface::{
    discriminators::{
        DEPOSIT_RESERVE_LIQUIDITY_AND_OBLIGATION_COLLATERAL_V2, INIT_OBLIGATION,
        REFRESH_OBLIGATION, WITHDRAW_OBLIGATION_COLLATERAL_AND_REDEEM_RESERVE_COLLATERAL_V2,
    },
    from_account_data,
    instructions::{
        deposit::{
            deposit_reserve_liquidity_and_obligation_collateral_v2,
            DepositReserveLiquidityAndObligationCollateralV2Accounts,
        },
        obligation::{init_obligation, InitObligationAccounts},
        refresh::{
            refresh_obligation, refresh_reserve, RefreshObligationAccounts, RefreshReserveAccounts,
        },
        withdraw::{
            withdraw_obligation_collateral_and_redeem_reserve_collateral_v2,
            WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts,
        },
    },
    pda::{farms_user_state, lending_market_authority, obligation, user_metadata},
    state::{Obligation, Reserve},
    types::InitObligationArgs,
    FARMS_PROGRAM_ID, KLEND_PROGRAM_ID,
};
use loyal_actions::{
    compile_squads_inner_instruction, compiler_lookup_eligible_addresses,
    create_init_obligation_yield_route_action, create_same_mint_market_mint_yield_route_action,
    derive_action_account, derive_kamino_obligation_farm_user_state,
    derive_kamino_vanilla_obligation, execute_program_interaction_policy_instruction,
    execute_sync_transaction_instruction, kamino_init_obligation_farm_instruction,
    remove_policy_instruction, update_all_in_one_market_mint_yield_route_action,
    update_init_obligation_yield_route_action, update_same_mint_market_mint_yield_route_action,
    KaminoInitObligationFarm, KaminoReserveLookupTableAccounts, LookupTableManifest,
    LoyalActionContext, RouteTopology, SwapLane, YieldRouteActionBuilder, YieldRouteActionSeeds,
    YieldRouteActionSetup, YieldRouteInstruction, YieldRouteInstructionPlan,
    YieldRouteLookupTableRequirements, YieldRouteUniverse, ASSOCIATED_TOKEN_PROGRAM_ID,
    KAMINO_MAIN_USDC_RESERVE, SQUADS_SMART_ACCOUNT_PROGRAM_ID, USDC_MINT,
    YIELD_ROUTE_WITHDRAW_ACTION_SEED,
};
use loyal_observability::{init_from_env, OperationalError};
use loyal_yield_orchestrator::sqlx::{
    postgres::{PgConnectOptions, PgPoolOptions},
    PgPool, Row,
};
use loyal_yield_orchestrator::{
    enabled_stable_mints_from_env, enabled_stable_mints_hash,
    fleet_orchestration::{
        classify_idle_deposit_post_effect, code_owned_stablecoin_valuations,
        evaluate_fresh_route_economics, fleet_stage_health_report, fleet_worker_role_probe,
        maximum_target_inflight_usd_micros, observe_market_epoch, outer_task_failure_recovery,
        project_fleet_route_source_evidence, projected_target_apy_bps, reconciliation_is_stalled,
        reconciliation_retry_delay_seconds, validate_fleet_route_kind_binding,
        validate_fleet_route_source_evidence, DurablePgWakeupEvent, DurablePgWakeupListener,
        EconomicPolicy, FleetObservationConfig, FleetRouteSourceEvidence,
        FleetRouteSourceKind as SameMintRouteSourceKind, FleetWorkerRole, FreshRouteEconomicsInput,
        IdleDepositPostEffectDecision, IdleDepositPostEffectObservation, IdleDepositRouteContract,
        ImmutableMarketEpoch, OpportunityInput, OuterTaskFailureKind, RebalanceOpportunityAdvance,
        RebalanceOpportunityClaimKind, RebalanceOpportunityLease, RebalanceOpportunityRecord,
        RebalanceOpportunityState, ReconciliationStallLatch, RouteFeePayerKind,
        RouteFeePayerShardConfig, RouteFeePolicy, SignedRouteSubmissionAdvance,
        SignedRouteSubmissionInput, SignedRouteSubmissionLease, SignedRouteSubmissionState,
        TargetCapacityObservation, TargetCapacityReservationInput,
        MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS,
    },
    lookup_table_manifest_hash as control_plane_lookup_table_manifest_hash,
    minimal_verified_table_bundle, policy_keypair_from_env, route_amount_evidence_from_metadata,
    route_fee_payer_keypairs_from_env,
    rpc_safety::{redacted_external_error, validate_rpc_endpoint, validate_rpc_genesis_hash},
    shared_market_manifest_addresses, shared_market_manifest_hash, solana_testing_keypair_from_env,
    standard_policy_keypair_from_env, vault_manifest_addresses, vault_manifest_hash,
    ConfirmSameMintRebalanceInput, CurrentIdleTokenBalance, DecisionAdvance, DecisionId,
    DecisionStatus, EffectiveLookupTableRollout, IdleVaultDepositDecisionInput,
    LookupTableAllocationKind, LookupTableManifestSubject, LookupTableProvisioningRequestUpsert,
    LookupTableReadinessRecord, LookupTableReadinessStatus, LookupTableRolloutMode,
    LookupTableSelectionKind, LookupTableSimulationState, LookupTableUsageLeaseBundle,
    LookupTableUsageLeaseKind, NeonSqlClient, NeonSqlConfig, OrchestratorError, PlanOutcomeStatus,
    PolicyMatchInput, RebalanceDecision, ReconciledReservePosition, ReconciledVaultState,
    ResolvedLookupTableBundle, ResolverTableCandidate, SameMintRebalanceInput,
    SameMintRebalanceResult, SharedMarketCatalogReadiness, SharedMarketCatalogRouteValidation,
    SharedMarketCatalogRouteValidationState, SnapshotId, VaultId,
    AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED, FIXED_KAMINO_MAIN_ROUTE_MODE,
    MAX_QUEUE_POSITIVE_AMOUNT_DRIFT_PPM, ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
    STANDARD_POLICY_AUTHORITY,
};
use loyal_yield_router::timescale::{TimescaleRouterClient, TimescaleRouterClientConfig};
use num_bigint::BigUint;
use num_traits::{ToPrimitive, Zero};
use serde::Serialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_account_decoder_client_types::UiAccountEncoding;
use solana_client::{
    client_error::ClientError, rpc_client::RpcClient, rpc_config::RpcAccountInfoConfig,
    rpc_request::RpcRequest,
};
use solana_rpc_client::mock_sender::MocksMap;
#[allow(deprecated)]
use solana_sdk::address_lookup_table::{
    instruction as address_lookup_table_instruction, program as address_lookup_table_program,
    state::AddressLookupTable,
};
#[allow(deprecated)]
use solana_sdk::compute_budget::ComputeBudgetInstruction;
#[allow(deprecated)]
use solana_sdk::system_instruction;
#[allow(deprecated)]
use solana_sdk::system_program;
use solana_sdk::{
    account::Account,
    commitment_config::CommitmentConfig,
    hash::Hash,
    instruction::{AccountMeta, Instruction},
    message::{v0, AddressLookupTableAccount, VersionedMessage},
    packet::PACKET_DATA_SIZE,
    pubkey::Pubkey,
    signature::{Keypair, Signature, Signer},
    transaction::VersionedTransaction,
};
use tokio::{
    sync::{Mutex as AsyncMutex, Notify, Semaphore},
    task::JoinSet,
};

const KAMINO_PRIME_USDC_RESERVE: &str = "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu";
const KAMINO_MAIN_MARKET: &str = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF";
const KAMINO_PRIME_MARKET: &str = "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA";
const KAMINO_MAPLE_MARKET: &str = "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y";
const KAMINO_ONRE_MARKET: &str = "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8";
const KAMINO_ETHENA_MARKET: &str = "BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF";
const SAME_MINT_ROUTE_MODE: &str = "same_mint_kamino";
const DEFAULT_SOLANA_RPC_URL: &str = "https://api.mainnet-beta.solana.com";
const PUBKEY_LEN: usize = 32;
const SQUADS_POLICY_ACCOUNT_DISCRIMINATOR: [u8; 8] = [222, 135, 7, 163, 235, 177, 33, 68];
const SPL_TOKEN_ACCOUNT_MINT_OFFSET: usize = 0;
const SPL_TOKEN_ACCOUNT_AMOUNT_OFFSET: usize = 64;
const KAMINO_WITHDRAW_ROUTE_STEP: &str =
    "kamino_withdraw_obligation_collateral_and_redeem_reserve_collateral_v2";
const KAMINO_DEPOSIT_ROUTE_STEP: &str =
    "kamino_deposit_reserve_liquidity_and_obligation_collateral_v2";
const KAMINO_INIT_OBLIGATION_ROUTE_STEP: &str = "kamino_init_obligation";
const SYSTEM_TRANSFER_VAULT_RENT_TOP_UP_ROUTE_STEP: &str = "system_transfer_vault_rent_top_up";
const KAMINO_INIT_OBLIGATION_FARM_ROUTE_STEP: &str = "kamino_init_obligation_farms_for_reserve";
const KAMINO_REFRESH_OBLIGATION_ROUTE_STEP: &str = "kamino_refresh_obligation";
const KAMINO_STABLE_UNIVERSE_PRESET: &str = "kamino_stable";
const SAFE_RISK_PROFILE: &str = "safe";
const LOOKUP_TABLE_RESOLVER_EXACT_SEARCH_LIMIT: usize = 16;
const LOOKUP_TABLE_ROUTE_LEASE_MINUTES: i64 = 10;
const LOOKUP_TABLE_PREPARED_LEASE_MINUTES: i64 = 5;
const MAX_KAMINO_OBLIGATION_RENT_LAMPORTS: u64 = 25_000_000;
const DEFAULT_FLEET_WORKER_POLL_MILLISECONDS: u64 = 250;
// Health emission reads `loyal_yield.fleet_orchestration_status`, a plain view
// whose CTE chain re-aggregates the opportunity, outbox, and submission tables
// from scratch on every call — roughly 2s against production. At a 1s interval
// the revalidate, execute, and reconcile processes each kept a backend busy on
// that aggregate continuously, which is what starved every worker's pool on
// 2026-08-03. This is observability only: nothing on the claim or transition
// write path reads it, and stuck-stage thresholds come from the durable
// recovery poll interval, not from this constant. Widening it cuts the
// concurrent hit rate proportionally; the materialized-view follow-up removes
// the per-read cost itself.
const FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS: u64 = 10_000;
const DEFAULT_FLEET_WORKER_LEASE_SECONDS: i64 = 120;
const DEFAULT_FLEET_REVALIDATE_CONCURRENCY: usize = 16;
const MAX_FEE_PAYER_SHARD_CANDIDATES: usize = 16;
const RPC_MULTIPLE_ACCOUNTS_LIMIT: usize = 100;
const SHARED_RESERVE_CACHE_TTL: Duration = Duration::from_millis(500);
const POLICY_ACCOUNT_CACHE_TTL: Duration = Duration::from_secs(1);
const FEE_PAYER_BALANCE_CACHE_TTL: Duration = Duration::from_millis(250);
const SHARED_RESERVE_CACHE_MAX_ENTRIES: usize = 512;
const POLICY_ACCOUNT_CACHE_MAX_ENTRIES: usize = 8_192;
const FEE_PAYER_BALANCE_CACHE_MAX_ENTRIES: usize = 256;
const DEFAULT_FLEET_EXECUTE_CONCURRENCY: usize = 8;
const DEFAULT_FLEET_RECONCILE_CONCURRENCY: usize = 16;
const DEFAULT_FLEET_RECONCILE_BATCH_SIZE: i64 = 32;
const DEFAULT_FLEET_POSITION_SWEEP_INTERVAL_SECONDS: u64 = 300;
const FLEET_POSITION_SWEEP_FAILURE_RETRY_SECONDS: u64 = 5;
/// Consecutive transport failures before the first operational error is
/// emitted. Upstream RPC providers routinely fail for a few attempts and
/// recover on their own; the per-attempt stderr record stays unconditional so
/// forensics keep every occurrence either way.
const FLEET_POSITION_SWEEP_TRANSPORT_ALERT_AFTER_FAILURES: u32 = 12;
/// Additional consecutive transport failures between repeat emissions once the
/// threshold above is crossed, so a long outage stays visible without
/// reproducing one operational error per retry.
const FLEET_POSITION_SWEEP_TRANSPORT_ALERT_REPEAT_FAILURES: u32 = 60;
/// Consecutive per-vault transport failures before the first operational error
/// is emitted. Unlike the initialization counter above this one is not driven by
/// a retry timer, so it pairs with the wave gate in
/// [`FleetPositionSweepVaultTransportStreak`]: the count alone is satisfied
/// instantly by one concurrent wave, and the wave gate supplies the elapsed time
/// that distinguishes a sustained outage from a blip.
const FLEET_POSITION_SWEEP_VAULT_TRANSPORT_ALERT_AFTER_FAILURES: u32 = 12;
/// Additional consecutive per-vault transport failures between repeat emissions
/// once the threshold above is crossed.
const FLEET_POSITION_SWEEP_VAULT_TRANSPORT_ALERT_REPEAT_FAILURES: u32 = 60;
const CURRENT_MARKET_EPOCH_STALE_PREFIX: &str = "current_market_epoch_stale:";
/// Cross-process admission cap, not a claim of physical transaction
/// independence. Each route also owns a vault-specific semantic key. Exact
/// writable evidence exposes the real Solana ceilings: a common fee payer or
/// peak Kamino reserve can still serialize transactions across different DB
/// lanes.
const FLEET_SHARED_WRITE_LANE_COUNT: i64 = 64;
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Direction {
    MainToPrime,
    PrimeToMain,
}

impl Direction {
    fn parse(value: &str) -> Option<Self> {
        match value {
            "main-to-prime" => Some(Self::MainToPrime),
            "prime-to-main" => Some(Self::PrimeToMain),
            _ => None,
        }
    }

    fn as_str(self) -> &'static str {
        match self {
            Self::MainToPrime => "main-to-prime",
            Self::PrimeToMain => "prime-to-main",
        }
    }

    fn source_reserve(self) -> String {
        match self {
            Self::MainToPrime => KAMINO_MAIN_USDC_RESERVE.to_string(),
            Self::PrimeToMain => KAMINO_PRIME_USDC_RESERVE.to_owned(),
        }
    }

    fn target_reserve(self) -> String {
        match self {
            Self::MainToPrime => KAMINO_PRIME_USDC_RESERVE.to_owned(),
            Self::PrimeToMain => KAMINO_MAIN_USDC_RESERVE.to_string(),
        }
    }

    fn source_market(self) -> &'static str {
        match self {
            Self::MainToPrime => KAMINO_MAIN_MARKET,
            Self::PrimeToMain => KAMINO_PRIME_MARKET,
        }
    }

    fn target_market(self) -> &'static str {
        match self {
            Self::MainToPrime => KAMINO_PRIME_MARKET,
            Self::PrimeToMain => KAMINO_MAIN_MARKET,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ReserveMove {
    source_reserve: String,
    target_reserve: String,
}

impl ReserveMove {
    fn from_options(options: &CliOptions) -> Result<Self, String> {
        let (source_reserve, target_reserve) =
            match (&options.source_reserve, &options.target_reserve) {
                (Some(source), Some(target)) => (source.clone(), target.clone()),
                (None, None) => (
                    options.direction.source_reserve(),
                    options.direction.target_reserve(),
                ),
                _ => {
                    return Err(
                        "--source-reserve and --target-reserve must be provided together"
                            .to_owned(),
                    )
                }
            };
        Pubkey::from_str(&source_reserve)
            .map_err(|_| "--source-reserve must be a public key".to_owned())?;
        Pubkey::from_str(&target_reserve)
            .map_err(|_| "--target-reserve must be a public key".to_owned())?;
        if source_reserve == target_reserve {
            return Err("source and target reserves must be distinct".to_owned());
        }
        Ok(Self {
            source_reserve,
            target_reserve,
        })
    }
}

fn reconcile_reserves_for_move(options: &CliOptions, reserve_move: &ReserveMove) -> Vec<String> {
    let mut reserves = Vec::new();
    push_unique_string(&mut reserves, reserve_move.source_reserve.clone());
    push_unique_string(&mut reserves, reserve_move.target_reserve.clone());
    if options.full_withdraw_main_usdc {
        let main = KAMINO_MAIN_USDC_RESERVE.to_string();
        if !reserves.iter().any(|existing| existing == &main) {
            reserves.push(main);
        }
    }
    if let Some(reserve) = &options.full_withdraw_reserve {
        if !reserves.iter().any(|existing| existing == reserve) {
            reserves.push(reserve.clone());
        }
    }
    if let Some(reserve) = &options.initial_deposit_reserve {
        push_unique_string(&mut reserves, reserve.clone());
    }
    if let Some(reserve) = &options.idle_vault_deposit_reserve {
        push_unique_string(&mut reserves, reserve.clone());
    }
    if let Some(reserve) = &options.setup_obligation_reserve {
        if !reserves.iter().any(|existing| existing == reserve) {
            reserves.push(reserve.clone());
        }
    }
    for reserve in &options.reconcile_reserves {
        if !reserves.iter().any(|existing| existing == reserve) {
            reserves.push(reserve.clone());
        }
    }
    reserves
}

fn push_unique_string(values: &mut Vec<String>, value: String) {
    if !values.iter().any(|existing| existing == &value) {
        values.push(value);
    }
}

fn full_withdraw_reserve(options: &CliOptions) -> String {
    options
        .full_withdraw_reserve
        .clone()
        .unwrap_or_else(|| KAMINO_MAIN_USDC_RESERVE.to_string())
}

/// Queue-facing execution mode. Revalidation performs every route-build,
/// reusable-ALT, packet, and simulation check, but stops before decision
/// creation and transaction submission.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum SameMintRouteExecutionMode {
    Revalidate,
    /// Revalidate once, then continue with the same fresh route only when the
    /// worker already owns an immediately available execution permit and can
    /// atomically upgrade the durable lease/conflict fence.
    RevalidateAndExecute,
    Execute,
}

/// Explicit in-process handoff for a planned same-mint opportunity. Keeping
/// the complete monitor evidence here removes process-global argv from the
/// execution boundary without weakening the executor's drift checks.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SameMintRouteExecutionRequest {
    pub mode: SameMintRouteExecutionMode,
    pub opportunity_id: i64,
    pub optimizer_epoch_id: i64,
    pub optimizer_market_slot: i64,
    pub lease_owner: String,
    pub fencing_token: i64,
    pub source_kind: SameMintRouteSourceKind,
    pub settings: String,
    pub vault_index: i16,
    pub source_reserve: Option<String>,
    pub target_reserve: String,
    pub expected_source_snapshot_id: Option<i64>,
    pub expected_idle_token_account: Option<String>,
    pub expected_idle_observed_slot: Option<i64>,
    pub expected_idle_observed_at: Option<DateTime<Utc>>,
    pub expected_liquidity_mint: String,
    pub expected_amount_raw: i64,
    pub expected_route_amount_semantics: String,
    pub expected_source_apy_bps: i64,
    /// Raw target APY from the immutable market epoch before the planner's
    /// capacity haircut. The runtime uses it only to preserve that haircut
    /// while comparing a new market snapshot with the durable plan.
    pub expected_observed_target_apy_bps: i64,
    pub expected_target_apy_bps: i64,
    pub expected_edge_bps: i64,
    pub principal_usd_micros: i64,
    pub confidence_ppm: u32,
    pub expected_service_millis: u64,
    pub holding_horizon_seconds: u64,
    pub estimated_execution_cost_usd_micros: i64,
    pub expected_cost_lamports: i64,
    /// Execute claims bind the payer selected by the preceding revalidation so
    /// the canonical typed-manifest fingerprint cannot change underneath the
    /// opportunity fence. Revalidation claims intentionally leave this empty.
    pub expected_route_fee_payer: Option<String>,
    pub cluster: String,
    pub rpc_url: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum SameMintRouteExecutionState {
    Ready,
    WaitingAlt,
    SubmissionQueued,
    Executed,
    Retry,
    Stale,
    Terminal,
}

#[derive(Clone, Debug)]
struct FleetWorkerOptions {
    claim_kind: RebalanceOpportunityClaimKind,
    cluster: String,
    rpc_url: String,
    owner: String,
    concurrency: usize,
    fused_execute_concurrency: usize,
    lease_seconds: i64,
    poll_interval_milliseconds: u64,
    once: bool,
}

#[derive(Clone, Debug)]
struct FleetReconcilerOptions {
    cluster: String,
    rpc_url: String,
    owner: String,
    concurrency: usize,
    batch_size: i64,
    lease_seconds: i64,
    poll_interval_milliseconds: u64,
    position_sweep_interval_seconds: u64,
    once: bool,
}

#[derive(Clone, Debug)]
struct FleetPositionSweepVault {
    vault: SelectedVault,
}

#[derive(Clone, Debug)]
struct FleetPositionSweepReserve {
    reserve: String,
    market: String,
    liquidity_mint: String,
}

/// SQLSTATE raised by `require_rebalance_opportunity_commit_lifetime` when an
/// opportunity would become visible with less than the fence's minimum usable
/// optimizer-epoch lifetime. See migration 0031.
const SQLSTATE_OPPORTUNITY_COMMIT_LIFETIME_FENCE: &str = "LY001";
/// SQLSTATE raised by `require_signed_route_commit_lifetime` for the same
/// reason on the signed handoff. See migration 0031.
const SQLSTATE_SIGNED_ROUTE_COMMIT_LIFETIME_FENCE: &str = "LY002";
/// The confirmed monitor can advance an observation floor just before its HTTP
/// verifier republishes the matching state. During that bounded handoff the
/// exact verified view temporarily omits one or more otherwise healthy
/// reserves. Give the topology time to converge before fencing the route.
const CURRENT_MARKET_TOPOLOGY_CONVERGENCE_TIMEOUT_SECONDS: u64 = 20;
const CURRENT_MARKET_TOPOLOGY_CONVERGENCE_POLL_MILLISECONDS: u64 = 1_000;

/// Reports whether a durable queue transition was refused by a commit-time
/// lifetime fence rather than failing to persist.
///
/// A fence rejection is designed backpressure: the worker finished its route
/// but the optimizer epoch aged out before COMMIT, so the database refused to
/// republish work that could never execute in time. The next epoch replans it.
/// Treating that as a recovery-required fault made routine end-of-epoch churn
/// page, so it is reported without an operational error. Anything else — a
/// pool timeout, a store invariant, an unexpected state — stays loud.
fn is_commit_lifetime_fence_rejection(error: &(dyn Error + 'static)) -> bool {
    error_chain(error)
        .find_map(|source| match source.downcast_ref::<OrchestratorError>() {
            Some(OrchestratorError::Sqlx(sqlx_error)) => {
                sqlx_error.as_database_error().and_then(|db| db.code())
            }
            _ => None,
        })
        .is_some_and(|code| {
            code == SQLSTATE_OPPORTUNITY_COMMIT_LIFETIME_FENCE
                || code == SQLSTATE_SIGNED_ROUTE_COMMIT_LIFETIME_FENCE
        })
}

/// Iterates an error and everything it wraps, so classification reads the whole
/// chain instead of only the outermost type.
fn error_chain<'a>(
    error: &'a (dyn Error + 'static),
) -> impl Iterator<Item = &'a (dyn Error + 'static)> {
    std::iter::successors(Some(error), |source| (*source).source())
}

/// A chain read that failed for a reason the next attempt clears on its own,
/// carried as a distinct type so callers classify it without matching on
/// message text.
///
/// The chain preview path returns `Box<dyn Error>` and mixes RPC faults with
/// owner and identity assertions. Transport is recognized by error type there:
/// RPC faults arrive as [`ClientError`], and the few self-clearing failures
/// raised by this binary use this type. Everything else is an invariant.
#[derive(Debug, thiserror::Error)]
#[error("{0}")]
struct TransientChainReadError(String);

/// Advances a consecutive-transport-failure counter and reports whether this
/// occurrence should emit an operational error.
///
/// The first emission waits for `alert_after` consecutive failures so a passing
/// upstream outage stays quiet, then repeats every `alert_repeat` failures so a
/// sustained one stays visible without one error per attempt. Shared by the
/// sweep's initialization and per-vault counters, which run the same cadence
/// over different units of work.
fn record_transport_failure(counter: &mut u32, alert_after: u32, alert_repeat: u32) -> bool {
    *counter = counter.saturating_add(1);
    let elapsed = counter.saturating_sub(alert_after);
    *counter >= alert_after && elapsed % alert_repeat == 0
}

/// An uninterrupted run of per-vault transport failures, tracked with the waves
/// it spans rather than the failure count alone.
///
/// The count on its own cannot separate a sustained outage from an instantaneous
/// one. A wave dispatches `concurrency` vaults simultaneously, so a sub-second
/// upstream blip fails the whole wave at once and drives the count past any
/// threshold below the concurrency in a few milliseconds. Requiring the run to
/// survive into a later wave is what makes elapsed time part of the decision:
/// the next wave only starts after the previous one has been awaited, so a
/// second wave of failures proves the outage outlived a full round trip instead
/// of landing inside one.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct FleetPositionSweepVaultTransportStreak {
    failures: u32,
    first_wave: Option<u64>,
    last_wave: Option<u64>,
    failures_at_last_emission: Option<u32>,
}

impl FleetPositionSweepVaultTransportStreak {
    /// Records one failure in `wave` and reports whether it should emit.
    ///
    /// The repeat cadence counts from the previous emission rather than from
    /// `alert_after`, so the wave gate can delay the first emission without
    /// pushing every later one off the modular schedule. Counting from
    /// `alert_after` would mean a run that first became eligible at 13 failures
    /// stayed silent until 72.
    fn record(&mut self, wave: u64, alert_after: u32, alert_repeat: u32) -> bool {
        self.failures = self.failures.saturating_add(1);
        self.first_wave.get_or_insert(wave);
        self.last_wave = Some(wave);

        if self.failures < alert_after || !self.spans_multiple_waves() {
            return false;
        }
        let due = match self.failures_at_last_emission {
            None => true,
            Some(emitted_at) => self.failures.saturating_sub(emitted_at) >= alert_repeat,
        };
        if due {
            self.failures_at_last_emission = Some(self.failures);
        }
        due
    }

    /// Any vault reaching a verdict without a transport fault ends the run, so
    /// the wave span above only ever measures uninterrupted unavailability.
    fn reset(&mut self) {
        *self = Self::default();
    }

    fn spans_multiple_waves(&self) -> bool {
        match (self.first_wave, self.last_wave) {
            (Some(first), Some(last)) => last > first,
            _ => false,
        }
    }

    fn failures(&self) -> u32 {
        self.failures
    }
}

/// Reports whether a database failure means the query no longer matches the
/// schema, which no amount of retrying repairs. Everything else — pool
/// timeouts, dropped connections, transient server errors — is connectivity.
///
/// Shared by the sweep's initialization and per-vault sites so both classify
/// the same database failure the same way.
fn sqlx_failure_is_invariant(error: &loyal_yield_orchestrator::sqlx::Error) -> bool {
    matches!(
        error,
        loyal_yield_orchestrator::sqlx::Error::ColumnDecode { .. }
            | loyal_yield_orchestrator::sqlx::Error::ColumnNotFound(_)
            | loyal_yield_orchestrator::sqlx::Error::ColumnIndexOutOfBounds { .. }
            | loyal_yield_orchestrator::sqlx::Error::TypeNotFound { .. }
    )
}

/// Separates sweep initialization faults that clear on their own from faults
/// that keep the sweep wedged until a person or a catalog rollout intervenes.
///
/// Both used to share one retryable operational error, so a passing RPC outage
/// and a catalog generation mismatch were indistinguishable to an operator.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum FleetPositionSweepInitFailureKind {
    /// Upstream RPC or database unavailability. Retrying is the whole recovery.
    Transport,
    /// The catalog, policy cohort, or configuration cannot support a sweep.
    /// Retrying alone will not clear this.
    Invariant,
}

impl FleetPositionSweepInitFailureKind {
    fn as_str(self) -> &'static str {
        match self {
            Self::Transport => "transport",
            Self::Invariant => "invariant",
        }
    }
}

#[derive(Debug)]
struct FleetPositionSweepInitError {
    kind: FleetPositionSweepInitFailureKind,
    source: Box<dyn Error>,
}

impl FleetPositionSweepInitError {
    fn transport(source: impl Into<Box<dyn Error>>) -> Self {
        Self {
            kind: FleetPositionSweepInitFailureKind::Transport,
            source: source.into(),
        }
    }

    fn invariant(source: impl Into<Box<dyn Error>>) -> Self {
        Self {
            kind: FleetPositionSweepInitFailureKind::Invariant,
            source: source.into(),
        }
    }

    /// Column and type failures mean the query no longer matches the schema,
    /// which no amount of retrying repairs. Everything else is connectivity.
    fn from_sqlx(error: loyal_yield_orchestrator::sqlx::Error) -> Self {
        if sqlx_failure_is_invariant(&error) {
            Self::invariant(error.to_string())
        } else {
            Self::transport(error.to_string())
        }
    }

    fn redacted_message(&self) -> String {
        redacted_external_error(&self.source.to_string())
    }
}

#[derive(Clone, Debug)]
struct FleetPositionSweepUniverse {
    cluster: String,
    enabled_mints: BTreeSet<String>,
    catalog_revision_id: i64,
    catalog_source_slot: Option<i64>,
    reserves: Vec<FleetPositionSweepReserve>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct FleetPositionSweepMetrics {
    sweep_id: u64,
    catalog_revision_id: Option<i64>,
    catalog_source_slot: Option<i64>,
    reserve_count: usize,
    cursor_vault_id: Option<i64>,
    eligible: usize,
    processed: u64,
    refreshed: u64,
    failed: u64,
    stale: u64,
    superseded: u64,
    duration_milliseconds: u64,
    complete: bool,
    error: Option<String>,
}

struct FleetPositionSweepRun {
    sweep_id: u64,
    started_at: DateTime<Utc>,
    started: Instant,
    universe: Arc<FleetPositionSweepUniverse>,
    vaults: Vec<FleetPositionSweepVault>,
    next_index: usize,
    cursor_vault_id: Option<i64>,
    processed: u64,
    refreshed: u64,
    failed: u64,
    stale: u64,
    superseded: u64,
}

impl FleetPositionSweepRun {
    fn metrics(&self, complete: bool, error: Option<String>) -> FleetPositionSweepMetrics {
        FleetPositionSweepMetrics {
            sweep_id: self.sweep_id,
            catalog_revision_id: Some(self.universe.catalog_revision_id),
            catalog_source_slot: self.universe.catalog_source_slot,
            reserve_count: self.universe.reserves.len(),
            cursor_vault_id: self.cursor_vault_id,
            eligible: self.vaults.len(),
            processed: self.processed,
            refreshed: self.refreshed,
            failed: self.failed,
            stale: self.stale,
            superseded: self.superseded,
            duration_milliseconds: u64::try_from(self.started.elapsed().as_millis())
                .unwrap_or(u64::MAX),
            complete,
            error,
        }
    }
}

struct FleetPositionSweepCoordinator {
    interval: Duration,
    next_due_at: Instant,
    next_sweep_id: u64,
    active: Option<FleetPositionSweepRun>,
    latest: Option<FleetPositionSweepMetrics>,
    consecutive_transport_failures: u32,
    vault_transport_streak: FleetPositionSweepVaultTransportStreak,
    vault_wave_sequence: u64,
}

impl FleetPositionSweepCoordinator {
    fn new(interval: Duration) -> Self {
        Self {
            interval,
            next_due_at: Instant::now(),
            next_sweep_id: 1,
            active: None,
            latest: None,
            consecutive_transport_failures: 0,
            vault_transport_streak: FleetPositionSweepVaultTransportStreak::default(),
            vault_wave_sequence: 0,
        }
    }

    /// Opens a new concurrent wave. Every vault in one wave is dispatched
    /// simultaneously, so the wave is the smallest unit that carries real
    /// elapsed time between failures.
    fn begin_vault_wave(&mut self) {
        self.vault_wave_sequence = self.vault_wave_sequence.saturating_add(1);
    }

    fn due(&self) -> bool {
        self.active.is_some() || Instant::now() >= self.next_due_at
    }

    fn next_sweep_id(&mut self) -> u64 {
        let sweep_id = self.next_sweep_id;
        self.next_sweep_id = self.next_sweep_id.saturating_add(1);
        sweep_id
    }

    /// Records a failed initialization and reports whether this occurrence
    /// should emit an operational error.
    ///
    /// Invariant failures always emit because each one is independently
    /// actionable. Transport failures emit only once a sustained run proves the
    /// upstream outage is not self-clearing, then at a slow repeat cadence.
    fn record_initialization_failure(
        &mut self,
        sweep_id: u64,
        kind: FleetPositionSweepInitFailureKind,
        error: String,
    ) -> bool {
        self.latest = Some(FleetPositionSweepMetrics {
            sweep_id,
            catalog_revision_id: None,
            catalog_source_slot: None,
            reserve_count: 0,
            cursor_vault_id: None,
            eligible: 0,
            processed: 0,
            refreshed: 0,
            failed: 0,
            stale: 0,
            superseded: 0,
            duration_milliseconds: 0,
            complete: false,
            error: Some(error),
        });
        self.next_due_at = Instant::now()
            + self.interval.min(Duration::from_secs(
                FLEET_POSITION_SWEEP_FAILURE_RETRY_SECONDS,
            ));
        match kind {
            FleetPositionSweepInitFailureKind::Invariant => {
                self.consecutive_transport_failures = 0;
                true
            }
            FleetPositionSweepInitFailureKind::Transport => record_transport_failure(
                &mut self.consecutive_transport_failures,
                FLEET_POSITION_SWEEP_TRANSPORT_ALERT_AFTER_FAILURES,
                FLEET_POSITION_SWEEP_TRANSPORT_ALERT_REPEAT_FAILURES,
            ),
        }
    }

    fn record_initialization_success(&mut self) {
        self.consecutive_transport_failures = 0;
    }

    fn consecutive_transport_failures(&self) -> u32 {
        self.consecutive_transport_failures
    }

    /// Records one failed vault refresh and reports whether this occurrence
    /// should emit an operational error.
    ///
    /// Invariant failures always emit because each one names a specific vault
    /// whose policy or on-chain identity needs inspection. Transport failures
    /// emit only once the consecutive run both crosses the threshold and
    /// outlives the wave it started in, which is what separates a sustained
    /// upstream outage from the instantaneous blip the next sweep repairs on
    /// its own.
    fn record_vault_failure(&mut self, kind: FleetPositionSweepVaultFailureKind) -> bool {
        match kind {
            FleetPositionSweepVaultFailureKind::Invariant => {
                self.vault_transport_streak.reset();
                true
            }
            FleetPositionSweepVaultFailureKind::Transport => self.vault_transport_streak.record(
                self.vault_wave_sequence,
                FLEET_POSITION_SWEEP_VAULT_TRANSPORT_ALERT_AFTER_FAILURES,
                FLEET_POSITION_SWEEP_VAULT_TRANSPORT_ALERT_REPEAT_FAILURES,
            ),
        }
    }

    fn record_vault_transport_success(&mut self) {
        self.vault_transport_streak.reset();
    }

    fn consecutive_vault_transport_failures(&self) -> u32 {
        self.vault_transport_streak.failures()
    }

    fn record_progress(&mut self) {
        self.latest = self.active.as_ref().map(|run| run.metrics(false, None));
    }

    fn record_completion(&mut self) -> Option<FleetPositionSweepMetrics> {
        let run = self.active.as_ref()?;
        let metrics = run.metrics(true, None);
        let cadence_due_at = run.started + self.interval;
        self.latest = Some(metrics.clone());
        self.active = None;
        self.next_due_at = cadence_due_at.max(Instant::now());
        Some(metrics)
    }

    fn health_json(&self) -> Value {
        self.latest
            .as_ref()
            .map_or(Value::Null, |metrics| json!(metrics))
    }
}

#[derive(Debug)]
struct FleetWorkerTaskResult {
    lease: RebalanceOpportunityLease,
    outcome: SameMintRouteExecutionOutcome,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct FleetWorkerCompletionIdentity<'a> {
    opportunity_id: i64,
    route_fingerprint: Option<&'a str>,
    requirements_fingerprint: Option<&'a str>,
}

fn validate_fleet_worker_completion(
    lease: FleetWorkerCompletionIdentity<'_>,
    outcome: FleetWorkerCompletionIdentity<'_>,
    current: FleetWorkerCompletionIdentity<'_>,
    current_state: RebalanceOpportunityState,
    has_decision_link: bool,
) -> Result<(), String> {
    if lease.route_fingerprint.is_none() || lease.requirements_fingerprint.is_none() {
        return Err(format!(
            "executed opportunity {} is missing its leased route identity",
            lease.opportunity_id
        ));
    }
    if outcome != lease {
        return Err(format!(
            "executed opportunity {} worker outcome identity diverged from its lease",
            lease.opportunity_id
        ));
    }
    if current != lease {
        return Err(format!(
            "executed opportunity {} durable identity diverged from its lease",
            lease.opportunity_id
        ));
    }
    if !has_decision_link {
        return Err(format!(
            "executed opportunity {} is missing its durable decision link",
            lease.opportunity_id
        ));
    }
    if !matches!(
        current_state,
        RebalanceOpportunityState::DecisionCreated | RebalanceOpportunityState::Completed
    ) {
        return Err(format!(
            "executed opportunity {} is {}, expected decision_created or completed",
            lease.opportunity_id,
            current_state.as_str()
        ));
    }
    Ok(())
}

#[derive(Debug)]
enum FleetWorkerWakeup<T> {
    Task(Option<Result<T, tokio::task::JoinError>>),
    Health,
}

async fn next_fleet_worker_wakeup<T: 'static>(
    tasks: &mut JoinSet<T>,
    health_interval: &mut tokio::time::Interval,
) -> FleetWorkerWakeup<T> {
    tokio::select! {
        biased;
        task = tasks.join_next() => FleetWorkerWakeup::Task(task),
        _ = health_interval.tick() => FleetWorkerWakeup::Health,
    }
}

#[derive(Debug)]
struct FleetReconcilerTaskResult {
    lease: SignedRouteSubmissionLease,
    outcome: FleetReconcilerTaskOutcome,
}

#[derive(Debug)]
enum FleetReconcilerTaskOutcome {
    Completed(bool),
    Failed {
        kind: OuterTaskFailureKind,
        error: String,
    },
}

type FusedExecutionLeaseState = Arc<Mutex<Option<RebalanceOpportunityLease>>>;

/// Structured state transition consumed by a persistent queue worker. CLI
/// output remains unchanged; callers never need to capture or parse stdout.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SameMintRouteExecutionOutcome {
    pub state: SameMintRouteExecutionState,
    pub opportunity_id: i64,
    pub source_kind: SameMintRouteSourceKind,
    pub settings: String,
    pub vault_index: i16,
    pub source_reserve: Option<String>,
    pub target_reserve: String,
    pub writes_decision: bool,
    pub sends_transactions: bool,
    pub reason: Option<String>,
    pub route_fingerprint: Option<String>,
    pub requirements_fingerprint: Option<String>,
    pub provisioning_request_id: Option<i64>,
    pub readiness_evidence: Option<Value>,
    /// Exact writable pubkeys derived from the built instructions, including
    /// the selected fee payer. This is immutable audit evidence.
    pub writable_account_keys: Vec<String>,
    /// One vault-exclusive key plus one bounded DB admission lane. This does
    /// not hide physical writable overlap: the exact account list above still
    /// exposes common fee-payer and reserve serialization on Solana.
    pub conflict_account_keys: Vec<String>,
}

#[derive(Debug)]
struct InProcessRouteResult {
    state: SameMintRouteExecutionState,
    reason: Option<String>,
    route_fingerprint: Option<String>,
    requirements_fingerprint: Option<String>,
    provisioning_request_id: Option<i64>,
    readiness_evidence: Option<Value>,
    writable_account_keys: Vec<String>,
    conflict_account_keys: Vec<String>,
}

#[derive(Clone)]
struct SameMintRouteRuntime {
    rpc: Arc<RpcClient>,
    client: NeonSqlClient,
    pool: PgPool,
    timescale: Option<TimescaleRouterClient>,
    rpc_cache: Arc<SameMintRouteRpcCache>,
    market_epoch_cache: Arc<AsyncMutex<BTreeMap<String, CachedMarketEpoch>>>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct CurrentRouteMarketEconomics {
    optimizer_epoch_id: i64,
    optimizer_epoch_fingerprint: String,
    optimizer_epoch_expires_at: DateTime<Utc>,
    fresh_market_fingerprint: String,
    fresh_market_expires_at: DateTime<Utc>,
    material_frontier_disposition: String,
    source_apy_bps: i64,
    capacity_adjusted_target_apy_bps: i64,
    edge_bps: i64,
    fee_cap_lamports: i64,
    capacity_reservation: TargetCapacityReservationInput,
}

#[derive(Clone)]
struct CachedRpcValue<T> {
    value: T,
    context_slot: u64,
    optimizer_epoch_id: Option<i64>,
    observed_at: DateTime<Utc>,
    fetched_at: Instant,
}

#[derive(Clone)]
struct CachedMarketEpoch {
    epoch: ImmutableMarketEpoch,
    fetched_at: Instant,
}

impl<T> CachedRpcValue<T> {
    fn is_fresh_for(
        &self,
        optimizer_epoch_id: Option<i64>,
        min_context_slot: Option<u64>,
        ttl: Duration,
    ) -> bool {
        optimizer_epoch_id.is_none_or(|epoch| self.optimizer_epoch_id == Some(epoch))
            && min_context_slot.is_none_or(|slot| self.context_slot >= slot)
            && self.fetched_at.elapsed() <= ttl
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
struct ReserveSummaryCacheKey {
    reserve: Pubkey,
    optimizer_epoch_id: Option<i64>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
struct ReserveSummaryFlightKey {
    reserve: Pubkey,
    optimizer_epoch_id: Option<i64>,
    min_context_slot: Option<u64>,
}

#[derive(Default)]
struct ReserveSummaryFlight {
    completed: AtomicBool,
    notify: Notify,
}

impl ReserveSummaryFlight {
    async fn wait(&self) {
        loop {
            if self.completed.load(Ordering::Acquire) {
                return;
            }
            let notified = self.notify.notified();
            if self.completed.load(Ordering::Acquire) {
                return;
            }
            notified.await;
        }
    }

    fn complete(&self) {
        self.completed.store(true, Ordering::Release);
        self.notify.notify_waiters();
    }
}

#[derive(Default)]
struct ReserveSummaryCacheState {
    values: BTreeMap<ReserveSummaryCacheKey, CachedRpcValue<KaminoReserveSummary>>,
    in_flight: BTreeMap<ReserveSummaryFlightKey, Arc<ReserveSummaryFlight>>,
}

#[derive(Default)]
struct ReserveSummaryCache {
    // Keep ownership bookkeeping synchronous and tiny. A leader performs no
    // await after claiming flights until it has removed and completed them, so
    // task cancellation cannot strand a flight. Followers still wait
    // asynchronously on the per-key Notify below.
    state: Mutex<ReserveSummaryCacheState>,
}

#[derive(Default)]
struct SameMintRouteRpcCache {
    reserve_summaries: ReserveSummaryCache,
    policy_accounts: Mutex<BTreeMap<Pubkey, CachedRpcValue<DecodedPolicyAccount>>>,
    fee_payer_balances: Mutex<BTreeMap<Pubkey, CachedRpcValue<Option<u64>>>>,
}

fn purge_ttl_cache<K: Ord + Clone, T>(
    cache: &mut BTreeMap<K, CachedRpcValue<T>>,
    ttl: Duration,
    maximum_entries: usize,
) {
    cache.retain(|_, entry| entry.fetched_at.elapsed() <= ttl);
    while cache.len() > maximum_entries {
        let Some(oldest_key) = cache
            .iter()
            .min_by_key(|(_, entry)| entry.fetched_at)
            .map(|(key, _)| key.clone())
        else {
            break;
        };
        cache.remove(&oldest_key);
    }
}

impl SameMintRouteRuntime {
    async fn new(
        rpc_url: &str,
        cluster: &str,
        client: NeonSqlClient,
        require_current_market: bool,
    ) -> Result<Self, Box<dyn Error>> {
        validate_rpc_endpoint(rpc_url)?;
        let rpc = Arc::new(RpcClient::new_with_timeout_and_commitment(
            rpc_url.to_owned(),
            Duration::from_secs(10),
            CommitmentConfig::confirmed(),
        ));
        let observed_genesis_hash = rpc.get_genesis_hash().map_err(|_| {
            "failed to read genesis hash from configured same-mint route RPC endpoint"
        })?;
        validate_same_mint_rpc_genesis(cluster, observed_genesis_hash)?;
        client
            .require_schema_migration(20, "demand_driven_shared_market_catalog")
            .await?;
        let timescale = if require_current_market {
            let timescale_url =
                env::var("TIMESCALEDB_URL").map_err(|_| "TIMESCALEDB_URL must be set")?;
            Some(
                TimescaleRouterClient::connect(
                    TimescaleRouterClientConfig::new(timescale_url)
                        .with_schema("kamino")
                        .with_max_connections(4),
                )
                .await?,
            )
        } else {
            None
        };
        let pool = client.pool().clone();
        Ok(Self {
            rpc,
            client,
            pool,
            timescale,
            rpc_cache: Arc::new(SameMintRouteRpcCache::default()),
            market_epoch_cache: Arc::new(AsyncMutex::new(BTreeMap::new())),
        })
    }

    /// One stampede-safe immutable market read serves a short worker wave.
    /// This is revalidation evidence, not a new planning epoch: route workers
    /// must never publish optimizer epochs that supersede sibling work.
    /// Holding the async mutex across the miss is intentional: thousands of
    /// concurrent revalidators must not all issue the same Timescale query.
    async fn current_market_epoch(
        &self,
        config: &FleetObservationConfig,
    ) -> Result<ImmutableMarketEpoch, Box<dyn Error>> {
        const MARKET_EPOCH_CACHE_TTL: Duration = Duration::from_secs(5);
        let mut mints = config.enabled_mints.clone();
        mints.sort();
        mints.dedup();
        let key = format!("{}:{}", config.cluster, mints.join(","));
        let mut cache = self.market_epoch_cache.lock().await;
        if let Some(cached) = cache.get(&key) {
            if cached.fetched_at.elapsed() <= MARKET_EPOCH_CACHE_TTL
                && cached.epoch.optimizer_envelope_expires_at() > Utc::now()
            {
                return Ok(cached.epoch.clone());
            }
        }
        let timescale = self
            .timescale
            .as_ref()
            .ok_or("queue route is missing its current market snapshot client")?;
        let epoch = observe_market_epoch(timescale, config)
            .await
            .map_err(|error| {
                format!("temporary current full-universe market observation unavailable: {error}")
            })?;
        cache.insert(
            key,
            CachedMarketEpoch {
                epoch: epoch.clone(),
                fetched_at: Instant::now(),
            },
        );
        cache.retain(|_, entry| entry.fetched_at.elapsed() <= MARKET_EPOCH_CACHE_TTL);
        Ok(epoch)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct CliOptions {
    settings: String,
    vault_index: i16,
    direction: Direction,
    source_reserve: Option<String>,
    target_reserve: Option<String>,
    update_policy: bool,
    update_active_policy: bool,
    initial_deposit_reserve: Option<String>,
    initial_deposit_amount_raw: Option<u64>,
    idle_vault_deposit_reserve: Option<String>,
    idle_vault_deposit_amount_raw: Option<u64>,
    full_withdraw_main_usdc: bool,
    full_withdraw_reserve: Option<String>,
    setup_obligation_reserve: Option<String>,
    e2e_deposit_amount_raw: Option<u64>,
    execute: bool,
    prepare_only: bool,
    /// Suppresses every database write a dry run would otherwise make, so an
    /// operator can inspect a route without registering readiness, provisioning
    /// demand, or usage leases. Cannot be combined with a mode that must persist.
    read_only: bool,
    fused_execute: bool,
    optimization_cycle: bool,
    reconcile_from_chain: bool,
    reconcile_current_positions: bool,
    reconcile_reserves: Vec<String>,
    seed_from_user_position: bool,
    expected_source_snapshot_id: Option<i64>,
    expected_liquidity_mint: Option<String>,
    expected_amount_raw: Option<i64>,
    expected_route_amount_semantics: Option<String>,
    expected_idle_token_account: Option<String>,
    expected_idle_observed_slot: Option<i64>,
    expected_idle_observed_at: Option<DateTime<Utc>>,
    expected_source_apy_bps: Option<i64>,
    expected_observed_target_apy_bps: Option<i64>,
    expected_target_apy_bps: Option<i64>,
    expected_edge_bps: Option<i64>,
    principal_usd_micros: Option<i64>,
    confidence_ppm: Option<u32>,
    expected_service_millis: Option<u64>,
    holding_horizon_seconds: Option<u64>,
    estimated_execution_cost_usd_micros: Option<i64>,
    expected_cost_lamports: Option<i64>,
    current_economic_fee_cap_lamports: Option<i64>,
    expected_route_fee_payer: Option<String>,
    optimizer_epoch_id: Option<i64>,
    optimizer_market_slot: Option<i64>,
    opportunity_id: Option<i64>,
    opportunity_lease_owner: Option<String>,
    opportunity_fencing_token: Option<i64>,
    cluster: String,
    rpc_url: String,
}

impl CliOptions {
    fn route_runtime_active(&self) -> bool {
        self.execute || self.prepare_only
    }
}

impl SameMintRouteExecutionRequest {
    fn validate(&self) -> Result<(), String> {
        for (label, value) in [
            ("settings", &self.settings),
            ("target reserve", &self.target_reserve),
            ("liquidity mint", &self.expected_liquidity_mint),
        ] {
            Pubkey::from_str(value)
                .map_err(|_| format!("same-mint in-process {label} must be a public key"))?;
        }
        if self.opportunity_id <= 0
            || self.optimizer_epoch_id <= 0
            || self.optimizer_market_slot < 0
            || self.fencing_token <= 0
            || self.lease_owner.trim().is_empty()
        {
            return Err(
                "same-mint in-process opportunity, optimizer epoch, lease owner, and fencing token are required"
                    .to_owned(),
            );
        }
        if self.expected_amount_raw <= 0
            || self.expected_route_amount_semantics.trim().is_empty()
            || self.expected_edge_bps <= 0
            || self.expected_cost_lamports < 0
        {
            return Err(
                "same-mint in-process expected amount, semantics, and edge must be positive"
                    .to_owned(),
            );
        }
        if self
            .expected_target_apy_bps
            .checked_sub(self.expected_source_apy_bps)
            != Some(self.expected_edge_bps)
        {
            return Err(
                "same-mint in-process APY evidence does not equal the expected edge".to_owned(),
            );
        }
        if self.expected_observed_target_apy_bps < self.expected_target_apy_bps {
            return Err(
                "same-mint in-process capacity-adjusted target APY exceeds its observed target APY"
                    .to_owned(),
            );
        }
        if self.principal_usd_micros <= 0
            || self.confidence_ppm == 0
            || self.confidence_ppm > 1_000_000
            || self.expected_service_millis == 0
            || self.holding_horizon_seconds == 0
            || self.estimated_execution_cost_usd_micros < 0
        {
            return Err(
                "same-mint in-process current-market economics evidence is invalid".to_owned(),
            );
        }
        let source_evidence = FleetRouteSourceEvidence {
            expected_idle_token_account: self.expected_idle_token_account.clone(),
            expected_idle_observed_slot: self.expected_idle_observed_slot,
            expected_idle_observed_at: self.expected_idle_observed_at,
        };
        validate_fleet_route_source_evidence(
            self.source_kind,
            self.source_reserve.as_deref(),
            self.expected_source_snapshot_id,
            &source_evidence,
        )?;
        match self.source_kind {
            SameMintRouteSourceKind::ReservePosition => {
                let source_reserve = self
                    .source_reserve
                    .as_deref()
                    .ok_or("same-mint reserve-position request requires a source reserve")?;
                Pubkey::from_str(source_reserve).map_err(|_| {
                    "same-mint in-process source reserve must be a public key".to_owned()
                })?;
                if source_reserve == self.target_reserve {
                    return Err(
                        "same-mint in-process source and target reserves must differ".to_owned(),
                    );
                }
                if self.expected_route_amount_semantics
                    != ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY
                {
                    return Err(format!(
                        "same-mint reserve-position request requires {ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY} amount semantics"
                    ));
                }
            }
            SameMintRouteSourceKind::IdleVaultUsdc => {
                if self.expected_source_apy_bps != 0
                    || self.expected_route_amount_semantics != "idle_vault_liquidity"
                {
                    return Err(
                        "idle-vault request requires zero source APY and idle_vault_liquidity semantics"
                            .to_owned(),
                    );
                }
                let idle_token_account = self
                    .expected_idle_token_account
                    .as_deref()
                    .ok_or("idle-vault request requires the observed idle token account")?;
                Pubkey::from_str(idle_token_account).map_err(|_| {
                    "idle-vault observed token account must be a public key".to_owned()
                })?;
            }
        }
        validate_alt_cluster(&self.cluster)?;
        if let Some(fee_payer) = self.expected_route_fee_payer.as_deref() {
            Pubkey::from_str(fee_payer)
                .map_err(|_| "expected route fee payer must be a public key".to_owned())?;
        }
        Ok(())
    }

    fn as_cli_options(&self) -> Result<CliOptions, String> {
        self.validate()?;
        let idle_vault_deposit = self.source_kind == SameMintRouteSourceKind::IdleVaultUsdc;
        Ok(CliOptions {
            settings: self.settings.clone(),
            vault_index: self.vault_index,
            direction: Direction::MainToPrime,
            source_reserve: self.source_reserve.clone(),
            target_reserve: (!idle_vault_deposit).then(|| self.target_reserve.clone()),
            update_policy: false,
            update_active_policy: false,
            initial_deposit_reserve: None,
            initial_deposit_amount_raw: None,
            idle_vault_deposit_reserve: idle_vault_deposit.then(|| self.target_reserve.clone()),
            idle_vault_deposit_amount_raw: idle_vault_deposit.then_some(
                u64::try_from(self.expected_amount_raw).map_err(|_| {
                    "idle-vault request amount does not fit an unsigned raw amount".to_owned()
                })?,
            ),
            full_withdraw_main_usdc: false,
            full_withdraw_reserve: None,
            setup_obligation_reserve: None,
            e2e_deposit_amount_raw: None,
            execute: self.mode == SameMintRouteExecutionMode::Execute,
            prepare_only: matches!(
                self.mode,
                SameMintRouteExecutionMode::Revalidate
                    | SameMintRouteExecutionMode::RevalidateAndExecute
            ),
            read_only: false,
            fused_execute: self.mode == SameMintRouteExecutionMode::RevalidateAndExecute,
            optimization_cycle: true,
            reconcile_from_chain: true,
            reconcile_current_positions: false,
            reconcile_reserves: Vec::new(),
            seed_from_user_position: false,
            expected_source_snapshot_id: self.expected_source_snapshot_id,
            expected_liquidity_mint: Some(self.expected_liquidity_mint.clone()),
            expected_amount_raw: Some(self.expected_amount_raw),
            expected_route_amount_semantics: Some(self.expected_route_amount_semantics.clone()),
            expected_idle_token_account: self.expected_idle_token_account.clone(),
            expected_idle_observed_slot: self.expected_idle_observed_slot,
            expected_idle_observed_at: self.expected_idle_observed_at,
            expected_source_apy_bps: Some(self.expected_source_apy_bps),
            expected_observed_target_apy_bps: Some(self.expected_observed_target_apy_bps),
            expected_target_apy_bps: Some(self.expected_target_apy_bps),
            expected_edge_bps: Some(self.expected_edge_bps),
            principal_usd_micros: Some(self.principal_usd_micros),
            confidence_ppm: Some(self.confidence_ppm),
            expected_service_millis: Some(self.expected_service_millis),
            holding_horizon_seconds: Some(self.holding_horizon_seconds),
            estimated_execution_cost_usd_micros: Some(self.estimated_execution_cost_usd_micros),
            expected_cost_lamports: Some(self.expected_cost_lamports),
            current_economic_fee_cap_lamports: None,
            expected_route_fee_payer: self.expected_route_fee_payer.clone(),
            optimizer_epoch_id: Some(self.optimizer_epoch_id),
            optimizer_market_slot: Some(self.optimizer_market_slot),
            opportunity_id: Some(self.opportunity_id),
            opportunity_lease_owner: Some(self.lease_owner.clone()),
            opportunity_fencing_token: Some(self.fencing_token),
            cluster: self.cluster.clone(),
            rpc_url: self.rpc_url.clone(),
        })
    }

    fn outcome(
        &self,
        state: SameMintRouteExecutionState,
        reason: Option<String>,
    ) -> SameMintRouteExecutionOutcome {
        SameMintRouteExecutionOutcome {
            state,
            opportunity_id: self.opportunity_id,
            source_kind: self.source_kind,
            settings: self.settings.clone(),
            vault_index: self.vault_index,
            source_reserve: self.source_reserve.clone(),
            target_reserve: self.target_reserve.clone(),
            writes_decision: matches!(
                state,
                SameMintRouteExecutionState::SubmissionQueued
                    | SameMintRouteExecutionState::Executed
            ),
            sends_transactions: state == SameMintRouteExecutionState::Executed,
            reason,
            route_fingerprint: None,
            requirements_fingerprint: None,
            provisioning_request_id: None,
            readiness_evidence: None,
            writable_account_keys: Vec::new(),
            conflict_account_keys: Vec::new(),
        }
    }

    fn outcome_from_run(&self, result: InProcessRouteResult) -> SameMintRouteExecutionOutcome {
        SameMintRouteExecutionOutcome {
            state: result.state,
            opportunity_id: self.opportunity_id,
            source_kind: self.source_kind,
            settings: self.settings.clone(),
            vault_index: self.vault_index,
            source_reserve: self.source_reserve.clone(),
            target_reserve: self.target_reserve.clone(),
            writes_decision: matches!(
                result.state,
                SameMintRouteExecutionState::SubmissionQueued
                    | SameMintRouteExecutionState::Executed
            ),
            sends_transactions: result.state == SameMintRouteExecutionState::Executed,
            reason: result.reason,
            route_fingerprint: result.route_fingerprint,
            requirements_fingerprint: result.requirements_fingerprint,
            provisioning_request_id: result.provisioning_request_id,
            readiness_evidence: result.readiness_evidence,
            writable_account_keys: result.writable_account_keys,
            conflict_account_keys: result.conflict_account_keys,
        }
    }
}

#[derive(Clone, Debug)]
struct SelectedVault {
    id: VaultId,
    settings: String,
    authority: String,
    policy_seed: i64,
    vault_index: i16,
    vault_pubkey: String,
    policy_account: String,
    setup_policy_account: Option<String>,
    setup_policy_seed: Option<i64>,
    delegated_signers: Vec<String>,
    threshold: i32,
    route_modes: Vec<String>,
    stable_mints: Vec<String>,
    kamino_markets: Vec<String>,
    kamino_liquidity_mints: Vec<String>,
    swap_lanes: Value,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct PositionSummary {
    reserve: String,
    liquidity_mint: String,
    amount_raw: i64,
    has_value: bool,
    snapshot_id: SnapshotId,
    supply_apy_bps: Option<i64>,
    planning_metadata: Value,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ChainPositionSummary {
    reserve: String,
    market: String,
    liquidity_mint: String,
    liquidity_token_program: String,
    reserve_liquidity_supply: String,
    collateral_mint: String,
    reserve_collateral_supply: String,
    collateral_farm: Option<String>,
    collateral_farm_user_state: Option<String>,
    collateral_farm_user_state_exists: bool,
    pyth_oracle: Option<String>,
    switchboard_price_oracle: Option<String>,
    switchboard_twap_oracle: Option<String>,
    scope_prices: Option<String>,
    obligation: String,
    obligation_exists: bool,
    obligation_deposit_reserves: Vec<String>,
    obligation_borrow_reserves: Vec<String>,
    amount_raw: u64,
    redeemable_liquidity_amount_raw: u64,
    vault_liquidity_ata: String,
    vault_liquidity_token_account_exists: bool,
    vault_liquidity_amount_raw: u64,
}

#[derive(Clone, Debug)]
struct ChainReconcilePreview {
    observed_slot: i64,
    vault_user_metadata: String,
    vault_user_metadata_exists: bool,
    positions: Vec<ChainPositionSummary>,
    rpc_account_reads: FleetRpcAccountReadEvidence,
}

#[derive(Clone, Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
struct FleetRpcAccountReadEvidence {
    reserve_batch_requests: usize,
    reserve_cache_hits: usize,
    vault_batch_requests: usize,
    policy_cache_hit: bool,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum IdleVaultDepositBlockerKind {
    SourceStale,
    LookupTable,
    Retry,
    Safety,
}

#[derive(Clone, Debug)]
struct IdleVaultDepositBlocker {
    kind: IdleVaultDepositBlockerKind,
    message: String,
}

impl IdleVaultDepositBlocker {
    fn source_stale(message: impl Into<String>) -> Self {
        Self {
            kind: IdleVaultDepositBlockerKind::SourceStale,
            message: message.into(),
        }
    }

    fn safety(message: impl Into<String>) -> Self {
        Self {
            kind: IdleVaultDepositBlockerKind::Safety,
            message: message.into(),
        }
    }

    fn route_resolution(context: &str, blocker: &str) -> Self {
        let kind = match classify_route_resolution_blocker(blocker) {
            SameMintRouteExecutionState::WaitingAlt => IdleVaultDepositBlockerKind::LookupTable,
            SameMintRouteExecutionState::Retry => IdleVaultDepositBlockerKind::Retry,
            SameMintRouteExecutionState::Stale | SameMintRouteExecutionState::Terminal => {
                IdleVaultDepositBlockerKind::Safety
            }
            SameMintRouteExecutionState::Ready
            | SameMintRouteExecutionState::SubmissionQueued
            | SameMintRouteExecutionState::Executed => IdleVaultDepositBlockerKind::Safety,
        };
        Self {
            kind,
            message: format!("{context}: {blocker}"),
        }
    }

    fn route_preflight(message: impl Into<String>) -> Self {
        let message = message.into();
        let kind = if !message.contains("route_setup_rent_cap_exceeded")
            && classify_in_process_execution_error(&message) == SameMintRouteExecutionState::Retry
        {
            IdleVaultDepositBlockerKind::Retry
        } else {
            IdleVaultDepositBlockerKind::Safety
        };
        Self { kind, message }
    }
}

#[derive(Debug)]
struct UserPositionSeedPreview {
    source: String,
    rows: Vec<UserPositionSeedRow>,
    positions: Vec<PositionSummary>,
}

#[derive(Debug)]
struct UserPositionSeedRow {
    id: i64,
    current_reserve: String,
    current_market: String,
    current_liquidity_mint: String,
    current_amount_raw: i64,
    current_observed_slot: i64,
    current_observed_at: Option<DateTime<Utc>>,
}

#[derive(Debug)]
struct PolicyAccountPreflight {
    policy_account: String,
    source_market: String,
    target_market: String,
    decoded: DecodedPolicyAccount,
}

impl PolicyAccountPreflight {
    fn allows_required_markets(&self) -> bool {
        self.decoded
            .kamino_markets
            .iter()
            .any(|market| market == &self.source_market)
            && self
                .decoded
                .kamino_markets
                .iter()
                .any(|market| market == &self.target_market)
    }

    fn allows_required_route_steps(&self) -> bool {
        self.decoded
            .instructions
            .iter()
            .any(|instruction| instruction.route_step == Some(KAMINO_WITHDRAW_ROUTE_STEP))
            && self
                .decoded
                .instructions
                .iter()
                .any(|instruction| instruction.route_step == Some(KAMINO_DEPOSIT_ROUTE_STEP))
    }

    fn allows_init_obligation(&self) -> bool {
        self.decoded
            .instructions
            .iter()
            .any(|instruction| instruction.route_step == Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP))
    }

    fn allows_refresh_obligation(&self) -> bool {
        self.decoded
            .instructions
            .iter()
            .any(|instruction| instruction.route_step == Some(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP))
    }
}

#[derive(Clone, Debug)]
struct DecodedPolicyAccount {
    layout: PolicyAccountLayout,
    delegated_signers: Vec<String>,
    threshold: u16,
    account_index: u8,
    instruction_count: usize,
    kamino_markets: Vec<String>,
    kamino_liquidity_mints: Vec<String>,
    constraints: Vec<PolicyInstructionConstraint>,
    instructions: Vec<DecodedPolicyInstructionSummary>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum PolicyAccountLayout {
    ProgramInteractionPolicyState,
}

impl PolicyAccountLayout {
    fn as_str(self) -> &'static str {
        match self {
            Self::ProgramInteractionPolicyState => "program_interaction_policy_state",
        }
    }
}

#[derive(Clone, Debug)]
struct DecodedPolicyInstructionSummary {
    program_id: String,
    route_step: Option<&'static str>,
    data_discriminator: Option<Vec<u8>>,
    markets: Vec<String>,
    liquidity_mints: Vec<String>,
    account_constraints: Vec<DecodedPolicyAccountConstraintSummary>,
}

#[derive(Clone, Debug)]
struct DecodedPolicyAccountConstraintSummary {
    account_index: u8,
    kind: &'static str,
    pubkeys: Vec<String>,
    owner: Option<String>,
    data_constraints: Vec<DecodedPolicyDataConstraintSummary>,
}

#[derive(Clone, Debug)]
struct DecodedPolicyDataConstraintSummary {
    data_offset: u64,
    operator: &'static str,
    value: Value,
}

#[derive(Clone, Debug)]
struct PolicyInstructionConstraint {
    program_id: Pubkey,
    account_constraints: Vec<PolicyAccountConstraint>,
    data_constraints: Vec<PolicyDataConstraint>,
}

#[derive(Clone, Debug)]
struct PolicyAccountConstraint {
    account_index: u8,
    pubkeys: Vec<Pubkey>,
    data_constraints: Vec<PolicyDataConstraint>,
    owner: Option<Pubkey>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct PolicyDataConstraint {
    data_offset: u64,
    data_value: PolicyDataValue,
    operator: PolicyDataOperator,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum PolicyDataValue {
    U8(u8),
    U16Le(u16),
    U32Le(u32),
    U64Le(u64),
    U128Le(u128),
    U8Slice(Vec<u8>),
}

impl PolicyDataValue {
    fn to_json(&self) -> Value {
        match self {
            Self::U8(value) => json!({ "kind": "u8", "value": value }),
            Self::U16Le(value) => json!({ "kind": "u16Le", "value": value }),
            Self::U32Le(value) => json!({ "kind": "u32Le", "value": value }),
            Self::U64Le(value) => json!({ "kind": "u64Le", "value": value.to_string() }),
            Self::U128Le(value) => json!({ "kind": "u128Le", "value": value.to_string() }),
            Self::U8Slice(value) => json!({ "kind": "u8Slice", "value": value }),
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum PolicyDataOperator {
    Equals,
    NotEquals,
    GreaterThan,
    GreaterThanOrEqualTo,
    LessThan,
    LessThanOrEqualTo,
}

impl PolicyDataOperator {
    fn as_str(self) -> &'static str {
        match self {
            Self::Equals => "equals",
            Self::NotEquals => "not_equals",
            Self::GreaterThan => "greater_than",
            Self::GreaterThanOrEqualTo => "greater_than_or_equal_to",
            Self::LessThan => "less_than",
            Self::LessThanOrEqualTo => "less_than_or_equal_to",
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct InlineMissingObligationSetupPreview {
    target_obligation: String,
    target_reserve: String,
    target_market: String,
    policy_account: String,
    policy_source: &'static str,
    instruction_constraint_index: u8,
    vault_rent_top_up: Option<MissingObligationSetupFunding>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct RouteFeePayerSelection {
    pubkey: Pubkey,
    kind: RouteFeePayerKind,
    reason: String,
    mature_route: bool,
    observed_balance_lamports: Option<i64>,
    observed_balance_slot: Option<i64>,
    observed_balance_at: Option<DateTime<Utc>>,
    shard: Option<RouteFeePayerShardConfig>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct FeePayerBalanceObservation {
    lamports: u64,
    context_slot: u64,
    observed_at: DateTime<Utc>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct RouteExecutionPreview {
    policy_account: String,
    setup_policy_account: Option<String>,
    fee_payer: String,
    fee_payer_kind: RouteFeePayerKind,
    fee_payer_selection: RouteFeePayerSelection,
    signer: String,
    account_index: u8,
    instruction_constraint_indexes: Vec<u8>,
    init_instruction_constraint_index: Option<u8>,
    policy_constraint_validation: Option<PolicyConstraintValidation>,
    missing_obligation_setup: Option<InlineMissingObligationSetupPreview>,
    source_farm_setup_required: bool,
    target_farm_setup_required: bool,
    setup_instruction_program: Option<String>,
    setup_instruction_discriminator: Option<Vec<u8>>,
    route_steps: Vec<&'static str>,
    refresh_reserves: Vec<String>,
    inner_instruction_count: usize,
    transaction_account_count: usize,
    outer_account_count: usize,
    source_instruction_program: String,
    target_instruction_program: String,
    source_instruction_discriminator: Vec<u8>,
    target_instruction_discriminator: Vec<u8>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct PolicyConstraintValidation {
    matches: bool,
    failures: Vec<String>,
}

#[derive(Clone, Debug)]
struct RouteExecutionPlan {
    pre_instructions: Vec<Instruction>,
    instructions: Vec<Instruction>,
    lookup_table_manifest: LookupTableManifest,
    preview: RouteExecutionPreview,
}

#[derive(Debug)]
struct RouteExecutionSubmitResult {
    signature: String,
    submitted_slot: i64,
    confirmed_slot: i64,
    simulation_units_consumed: Option<u64>,
    transaction_packet: TransactionPacketSummary,
    lookup_table_resolution: Value,
    confirmed: SameMintRebalanceResult,
}

#[derive(Debug)]
struct CompiledLookupTableBundle {
    domain: ResolvedLookupTableBundle,
    transaction: Option<VersionedTransaction>,
    transaction_packet: Option<TransactionPacketSummary>,
    simulation_units_consumed: Option<u64>,
    compute_unit_limit: Option<u32>,
    priority_fee_micro_lamports: Option<u64>,
    compiled_fee_lamports: Option<u64>,
    simulation_error: Option<String>,
    verification_failures: Vec<Value>,
}

#[derive(Clone, Copy, Debug)]
struct TransactionFeeBudget {
    max_total_fee_lamports: u64,
    recent_priority_fee_micro_lamports: u64,
}

struct BudgetedFleetTransaction {
    transaction: VersionedTransaction,
    packet: TransactionPacketSummary,
    simulation_units_consumed: u64,
    compute_unit_limit: u32,
    priority_fee_micro_lamports: u64,
    compiled_fee_lamports: u64,
}

struct QueueSignedRouteHandoff {
    lease: RebalanceOpportunityLease,
    submission: SignedRouteSubmissionInput,
}

#[derive(Debug)]
struct RuntimeLookupTableResolution {
    rollout: EffectiveLookupTableRollout,
    route_fingerprint: String,
    requirements_fingerprint: String,
    selection_fingerprint: Option<String>,
    route_lease_reference: Option<String>,
    active_binding_fingerprint: String,
    active_binding_id: Option<i64>,
    selection_kind: LookupTableSelectionKind,
    blocker: Option<String>,
    selected_bundle: Option<ResolvedLookupTableBundle>,
    selected_transaction: Option<VersionedTransaction>,
    selected_transaction_packet: Option<TransactionPacketSummary>,
    selected_simulation_units_consumed: Option<u64>,
    selected_compiled_fee_lamports: Option<u64>,
    recent_blockhash: Hash,
    last_valid_block_height: i64,
    reusable_table_ids: Vec<i64>,
    required_addresses: BTreeSet<String>,
    writable_account_keys: Vec<String>,
    conflict_account_keys: Vec<String>,
    reusable_missing_addresses: BTreeSet<String>,
    reusable_ready: bool,
    reusable_compiled_message_size: Option<usize>,
    reusable_packet_fits: Option<bool>,
    reusable_simulation_units_consumed: Option<u64>,
    reusable_simulation_error: Option<String>,
    shared_catalog_covered: bool,
    observed_slot: i64,
    evidence: Value,
}

#[derive(Debug)]
struct RouteLookupTablePhase {
    route_kind: &'static str,
    scope: String,
    source_reserve: String,
    target_reserve: String,
    instructions: Vec<Instruction>,
    manifest: LookupTableManifest,
    resolution: RuntimeLookupTableResolution,
}

#[derive(Debug)]
struct SubmittedLookupTablePhase {
    signature: String,
    submitted_slot: i64,
    confirmed_slot: i64,
    simulation_units_consumed: Option<u64>,
    transaction_packet: TransactionPacketSummary,
    lookup_table_resolution: Value,
}

impl RuntimeLookupTableResolution {
    fn has_complete_reusable_static_coverage(&self) -> bool {
        reusable_runtime_enabled(&self.rollout)
            && self.shared_catalog_covered
            && self.reusable_missing_addresses.is_empty()
            && self.reusable_packet_fits == Some(true)
            && !self.reusable_table_ids.is_empty()
            && self.reusable_compiled_message_size.is_some()
    }

    fn selected_table_ids(&self) -> Vec<i64> {
        self.selected_bundle
            .as_ref()
            .map(|bundle| bundle.tables.iter().map(|table| table.table_id).collect())
            .unwrap_or_default()
    }

    fn require_ready(&self) -> Result<(), Box<dyn Error>> {
        if let Some(blocker) = &self.blocker {
            return Err(blocker.clone().into());
        }
        if self.selected_bundle.is_none()
            || self.selected_transaction.is_none()
            || self.selected_transaction_packet.is_none()
            || self.selection_fingerprint.is_none()
            || self.route_lease_reference.is_none()
        {
            return Err("lookup-table resolver did not produce a send-ready selection".into());
        }
        Ok(())
    }

    fn require_deferred_simulation_coverage(&self) -> Result<(), Box<dyn Error>> {
        let ready = reusable_runtime_enabled(&self.rollout) && self.reusable_ready;
        let funding_deferred = self.has_complete_reusable_static_coverage()
            && self
                .blocker
                .as_deref()
                .is_some_and(|blocker| blocker.starts_with("route_funding_required:"));
        if ready || funding_deferred {
            Ok(())
        } else {
            Err(format!(
                "reusable lookup-table coverage/packet/catalog gate failed before prerequisite transaction: mode={}, forceLegacy={}, sharedCatalogCovered={}, reusableMissing={}, reusablePacketFits={:?}",
                self.rollout.rollout_mode.as_str(),
                self.rollout.force_legacy,
                self.shared_catalog_covered,
                self.reusable_missing_addresses.len(),
                self.reusable_packet_fits,
            )
            .into())
        }
    }

    fn require_missing_token_account_deferred_simulation_coverage(
        &self,
        prerequisite_token_account_is_missing: bool,
    ) -> Result<(), Box<dyn Error>> {
        if self.require_deferred_simulation_coverage().is_ok() {
            return Ok(());
        }
        let account_creation_deferred = prerequisite_token_account_is_missing
            && self.has_complete_reusable_static_coverage()
            && self
                .reusable_simulation_error
                .as_deref()
                .is_some_and(is_account_not_initialized_simulation_error);
        if account_creation_deferred {
            Ok(())
        } else {
            Err("reusable lookup-table coverage is incomplete or the exact simulation failure is not the expected missing-token-account prerequisite".into())
        }
    }
}

fn apply_policy_setup_funding_serialization(
    resolution: &mut RuntimeLookupTableResolution,
    policy_signer: &str,
    required: bool,
) {
    if required {
        resolution
            .conflict_account_keys
            .push(format!("policy-setup-funding:{policy_signer}"));
        resolution.conflict_account_keys.sort_unstable();
        resolution.conflict_account_keys.dedup();
    }
    if let Some(fields) = resolution.evidence.as_object_mut() {
        fields.insert("serializesPolicySetupFunding".to_owned(), json!(required));
        fields.insert(
            "conflictAccountKeys".to_owned(),
            json!(&resolution.conflict_account_keys),
        );
    }
}

fn in_process_route_result(
    state: SameMintRouteExecutionState,
    reason: Option<String>,
    resolution: Option<&RuntimeLookupTableResolution>,
    provisioning_request_id: Option<i64>,
) -> InProcessRouteResult {
    InProcessRouteResult {
        state,
        reason,
        route_fingerprint: resolution.map(|value| value.route_fingerprint.clone()),
        requirements_fingerprint: resolution.map(|value| value.requirements_fingerprint.clone()),
        provisioning_request_id,
        readiness_evidence: resolution.map(|value| value.evidence.clone()),
        writable_account_keys: resolution
            .map(|value| value.writable_account_keys.clone())
            .unwrap_or_default(),
        conflict_account_keys: resolution
            .map(|value| value.conflict_account_keys.clone())
            .unwrap_or_default(),
    }
}

fn idle_route_fingerprints(
    setup: Option<&RouteLookupTablePhase>,
    deposit: Option<&RouteLookupTablePhase>,
) -> Option<(String, String)> {
    let phases = [setup, deposit].into_iter().flatten().collect::<Vec<_>>();
    match phases.as_slice() {
        [] => None,
        [phase] => Some((
            phase.resolution.route_fingerprint.clone(),
            phase.resolution.requirements_fingerprint.clone(),
        )),
        phases => Some((
            stable_fingerprint_owned(
                &phases
                    .iter()
                    .map(|phase| {
                        format!(
                            "{}:{}",
                            phase.route_kind, phase.resolution.route_fingerprint
                        )
                    })
                    .collect::<Vec<_>>(),
            ),
            stable_fingerprint_owned(
                &phases
                    .iter()
                    .map(|phase| {
                        format!(
                            "{}:{}",
                            phase.route_kind, phase.resolution.requirements_fingerprint
                        )
                    })
                    .collect::<Vec<_>>(),
            ),
        )),
    }
}

fn idle_in_process_route_result(
    state: SameMintRouteExecutionState,
    reason: Option<String>,
    setup: Option<&RouteLookupTablePhase>,
    setup_request_id: Option<i64>,
    deposit: Option<&RouteLookupTablePhase>,
    deposit_request_id: Option<i64>,
) -> InProcessRouteResult {
    let waiting_phase = (state == SameMintRouteExecutionState::WaitingAlt)
        .then(|| {
            [(setup, setup_request_id), (deposit, deposit_request_id)]
                .into_iter()
                .find_map(|(phase, request_id)| {
                    let phase = phase?;
                    phase.resolution.blocker.as_ref()?;
                    Some((phase, request_id))
                })
        })
        .flatten();
    let (route_fingerprint, requirements_fingerprint, provisioning_request_id) =
        if let Some((phase, request_id)) = waiting_phase {
            (
                Some(phase.resolution.route_fingerprint.clone()),
                Some(phase.resolution.requirements_fingerprint.clone()),
                request_id,
            )
        } else {
            let fingerprints = idle_route_fingerprints(setup, deposit);
            (
                fingerprints.as_ref().map(|value| value.0.clone()),
                fingerprints.map(|value| value.1),
                None,
            )
        };
    InProcessRouteResult {
        state,
        reason,
        route_fingerprint,
        requirements_fingerprint,
        provisioning_request_id,
        readiness_evidence: Some(json!({
            "setup": setup.map(|phase| json!({
                "provisioningRequestId": setup_request_id,
                "resolution": phase.resolution.evidence.clone(),
            })),
            "deposit": deposit.map(|phase| json!({
                "provisioningRequestId": deposit_request_id,
                "resolution": phase.resolution.evidence.clone(),
            })),
        })),
        writable_account_keys: [setup, deposit]
            .into_iter()
            .flatten()
            .flat_map(|phase| phase.resolution.writable_account_keys.iter().cloned())
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect(),
        conflict_account_keys: [setup, deposit]
            .into_iter()
            .flatten()
            .flat_map(|phase| phase.resolution.conflict_account_keys.iter().cloned())
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect(),
    }
}

fn reusable_runtime_enabled(rollout: &EffectiveLookupTableRollout) -> bool {
    rollout.rollout_mode == LookupTableRolloutMode::ReusableOnly && !rollout.force_legacy
}

fn reusable_runtime_blocker(
    rollout: &EffectiveLookupTableRollout,
    shared_catalog_covered: bool,
    reusable_ready: bool,
) -> Option<String> {
    if rollout.force_legacy {
        Some(
            "global force-legacy is a fail-closed stop because legacy ALT resolution has been removed"
                .to_owned(),
        )
    } else if rollout.rollout_mode != LookupTableRolloutMode::ReusableOnly {
        Some(format!(
            "lookup-table rollout mode {} is disabled because legacy ALT resolution has been removed; reusable_only is required",
            rollout.rollout_mode.as_str()
        ))
    } else if !shared_catalog_covered {
        Some(
            "shared_market_catalog_drift: route shared requirements are not covered by the exact active durable catalog generation"
                .to_owned(),
        )
    } else if !reusable_ready {
        Some(
            "reusable-only runtime requires complete reusable ALT coverage and simulation"
                .to_owned(),
        )
    } else {
        None
    }
}

fn route_simulation_blocker(error: &str) -> String {
    let normalized = error.to_ascii_lowercase();
    let funding_shortfall = [
        "insufficient funds",
        "insufficientfundsforfee",
        "insufficient lamports",
        "insufficient balance",
    ]
    .iter()
    .any(|needle| normalized.contains(needle));
    if funding_shortfall {
        format!("route_funding_required: exact route simulation failed: {error}")
    } else {
        format!("route_simulation_failed: {error}")
    }
}

fn is_account_not_initialized_simulation_error(error: &str) -> bool {
    let normalized = error.to_ascii_lowercase();
    normalized.contains("accountnotinitialized")
        || normalized.contains("account not initialized")
        || normalized.contains("custom(3012)")
        || normalized.contains("custom program error: 0xbc4")
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct MissingObligationSetupFunding {
    payer: String,
    vault: String,
    lamports: u64,
    vault_lamports_before: u64,
    payer_lamports_before: u64,
    required_vault_lamports: u64,
}

#[derive(Debug)]
struct MissingObligationSetupDryRun {
    policy_account: String,
    policy_source: &'static str,
    instruction_constraint_index: u8,
    vault_rent_top_up: Option<MissingObligationSetupFunding>,
    instructions: Vec<Instruction>,
    lookup_table_requirements: YieldRouteLookupTableRequirements,
    init_execution: PolicyTransactionBuild,
}

#[derive(Debug)]
struct MissingObligationSetupSubmitResult {
    policy_account: String,
    policy_source: &'static str,
    instruction_constraint_index: u8,
    vault_rent_top_up: Option<MissingObligationSetupFunding>,
    init_signature: String,
    init_submitted_slot: i64,
    init_confirmed_slot: i64,
    init_simulation_units_consumed: Option<u64>,
    init_transaction_packet: TransactionPacketSummary,
}

#[derive(Debug)]
struct InitialDepositSubmitResult {
    funding_signature: Option<String>,
    funding_submitted_slot: Option<i64>,
    funding_confirmed_slot: Option<i64>,
    funding_simulation_units_consumed: Option<u64>,
    funding_transaction_packet: TransactionPacketSummary,
    policy_signature: Option<String>,
    policy_submitted_slot: Option<i64>,
    policy_confirmed_slot: Option<i64>,
    policy_simulation_units_consumed: Option<u64>,
    policy_transaction_packet: TransactionPacketSummary,
    reconciled_snapshot_id: Option<SnapshotId>,
    post_chain_preview: Option<ChainReconcilePreview>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct InitialDepositPolicyPreview {
    policy_account: String,
    signer: String,
    account_index: u8,
    instruction_constraint_indexes: Vec<u8>,
    policy_constraint_validation: Option<PolicyConstraintValidation>,
    setup_instruction_program: Option<String>,
    setup_instruction_discriminator: Option<Vec<u8>>,
    route_steps: Vec<&'static str>,
    inner_instruction_count: usize,
    transaction_account_count: usize,
    outer_account_count: usize,
    deposit_instruction_program: String,
    deposit_instruction_discriminator: Vec<u8>,
}

#[derive(Clone, Debug)]
struct InitialDepositPolicyPlan {
    pre_instructions: Vec<Instruction>,
    instruction: Instruction,
    lookup_table_requirements: YieldRouteLookupTableRequirements,
    preview: InitialDepositPolicyPreview,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct FullWithdrawPolicyPreview {
    policy_account: String,
    signer: String,
    account_index: u8,
    instruction_constraint_indexes: Vec<u8>,
    policy_constraint_validation: Option<PolicyConstraintValidation>,
    route_steps: Vec<&'static str>,
    inner_instruction_count: usize,
    transaction_account_count: usize,
    outer_account_count: usize,
    withdraw_instruction_program: String,
    withdraw_instruction_discriminator: Vec<u8>,
}

#[derive(Clone, Debug)]
struct FullWithdrawPolicyPlan {
    pre_instructions: Vec<Instruction>,
    instruction: Instruction,
    lookup_table_requirements: YieldRouteLookupTableRequirements,
    preview: FullWithdrawPolicyPreview,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct AccountProof {
    pubkey: String,
    exists: bool,
    lamports: u64,
    owner: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ObligationAccountProof {
    account: AccountProof,
    owner: Option<String>,
    lending_market: Option<String>,
    active_deposit_count: Option<usize>,
    active_borrow_count: Option<usize>,
    reserve_deposited_amount_raw: Option<u64>,
}

#[derive(Debug)]
struct PolicyTransactionBuild {
    transaction: VersionedTransaction,
    transaction_packet: TransactionPacketSummary,
    best_case_single_lookup_table_packet: Option<TransactionPacketSummary>,
    simulation_error: Option<String>,
    simulation_logs: Value,
    simulation_skipped_reason: Option<String>,
    simulation_units_consumed: Option<u64>,
}

#[derive(Debug)]
struct TransactionPacketSummary {
    version: &'static str,
    fee_payer: String,
    signer_pubkeys: Vec<String>,
    packet_size_bytes: usize,
    packet_data_size_bytes: usize,
    fits_packet_data_size: bool,
    static_account_key_count: usize,
    address_table_lookup_count: usize,
    loaded_writable_address_count: usize,
    loaded_readonly_address_count: usize,
    compiled_instruction_count: usize,
    instruction_data_bytes: usize,
    lookup_table_accounts: Vec<LookupTableAccountSummary>,
}

#[derive(Debug)]
struct LookupTableAccountSummary {
    account: String,
    address_count: usize,
    addresses: Option<Vec<String>>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct PreparedSameMintDecision {
    id: DecisionId,
    vault_id: VaultId,
    source_snapshot_id: SnapshotId,
    source_reserve: String,
    target_reserve: String,
    liquidity_mint: String,
    source_liquidity_mint: String,
    target_liquidity_mint: String,
    amount_raw: i64,
    source_apy_bps: i64,
    target_apy_bps: i64,
    estimated_edge_bps: i64,
    estimated_cost_lamports: i64,
    execution_plan: Value,
    idempotency_key: String,
}

#[derive(Debug, PartialEq, Eq)]
enum PlanBlocker {
    MissingCurrentPosition,
    MissingSourceReserve(String),
    MissingTargetReserve(String),
    SourceHasNoValue,
    TargetMintMismatch {
        actual: String,
        expected: String,
    },
    UnsupportedAmountSemantics {
        reserve: String,
        amount_semantics: Option<String>,
    },
    MonitorPlanDrift(String),
    ActiveDecision {
        decision_id: i64,
        status: String,
    },
}

#[tokio::main]
async fn main() {
    let args = env::args().skip(1).collect::<Vec<_>>();
    match run_startup_probe(&args) {
        Ok(true) => return,
        Ok(false) => {}
        Err(error) => {
            eprintln!("{}", same_mint_fatal_error_payload(error.as_ref()));
            std::process::exit(1);
        }
    }

    let observability = match init_from_env("loyal-same-mint-reserve-swap") {
        Ok(observability) => observability,
        Err(error) => {
            eprintln!("failed to initialize observability: {error}");
            std::process::exit(1);
        }
    };
    if let Err(error) = run(args).await {
        OperationalError::new(
            "same_mint_route_worker_fatal",
            "run_same_mint_route_worker",
            "same-mint route worker stopped after a fatal error",
        )
        .retryable(false)
        .recovery_required(true)
        .emit();
        eprintln!("{}", same_mint_fatal_error_payload(error.as_ref()));
        let _ = observability.force_flush();
        std::process::exit(1);
    }
}

fn run_startup_probe(args: &[String]) -> Result<bool, Box<dyn Error>> {
    if matches!(
        args,
        [flag] if flag == "--fleet-controlled-transaction-probe"
    ) {
        println!(
            "{}",
            serde_json::to_string(&fleet_controlled_transaction_probe()?)?
        );
        return Ok(true);
    }
    let role_probe = match args {
        [fleet_worker, lane, role_probe]
            if fleet_worker == "--fleet-worker"
                && lane == "revalidate"
                && role_probe == "--role-probe" =>
        {
            Some(FleetWorkerRole::Revalidator)
        }
        [fleet_worker, lane, role_probe]
            if fleet_worker == "--fleet-worker"
                && lane == "execute"
                && role_probe == "--role-probe" =>
        {
            Some(FleetWorkerRole::Executor)
        }
        [fleet_reconciler, role_probe]
            if fleet_reconciler == "--fleet-reconciler" && role_probe == "--role-probe" =>
        {
            Some(FleetWorkerRole::Reconciler)
        }
        _ => None,
    };
    if let Some(role) = role_probe {
        println!("{}", fleet_worker_role_probe(role));
        return Ok(true);
    }

    Ok(false)
}

async fn run(args: Vec<String>) -> Result<(), Box<dyn Error>> {
    if matches!(
        args.as_slice(),
        [flag] if flag == "--fleet-rpc-hot-path-model"
            || flag == "--fleet-rpc-hot-path-benchmark"
    ) {
        println!(
            "{}",
            serde_json::to_string_pretty(&fleet_rpc_hot_path_model())?
        );
        return Ok(());
    }
    if args.first().is_some_and(|arg| arg == "--fleet-reconciler") {
        let options = parse_fleet_reconciler_options(args.into_iter().skip(1))?;
        return run_fleet_reconciler(options).await;
    }
    if args.first().is_some_and(|arg| arg == "--fleet-worker") {
        let options = parse_fleet_worker_options(args.into_iter().skip(1))?;
        return run_fleet_worker(options).await;
    }
    let options = match parse_args(args) {
        Ok(value) => value,
        Err(message) if message == "help" => {
            print_help();
            return Ok(());
        }
        Err(message) => return Err(message.into()),
    };
    let execute = options.execute;
    let prepare_only = options.prepare_only;
    if let Some(result) = run_with_options(options).await? {
        if execute
            && matches!(
                result.state,
                SameMintRouteExecutionState::WaitingAlt
                    | SameMintRouteExecutionState::Retry
                    | SameMintRouteExecutionState::Stale
                    | SameMintRouteExecutionState::Terminal
            )
        {
            return Err(result
                .reason
                .unwrap_or_else(|| "same-mint route execution did not become executable".to_owned())
                .into());
        }
        if !execute && !prepare_only {
            debug_assert!(!matches!(
                result.state,
                SameMintRouteExecutionState::SubmissionQueued
                    | SameMintRouteExecutionState::Executed
            ));
        }
    }
    Ok(())
}

fn controlled_rpc_response(slot: u64, value: Value) -> Value {
    json!({
        "context": {
            "slot": slot,
        },
        "value": value,
    })
}

fn fleet_controlled_transaction_probe() -> Result<Value, Box<dyn Error>> {
    // This is intentionally the real mounted policy signer. The probe never
    // serializes, logs, or returns either its secret material or public key.
    let policy = standard_policy_keypair_from_env()?;
    let policy_pubkey = policy.pubkey();

    // Exercise the same bounded rendezvous ordering used by the production
    // fee-payer selector. The highest-ranked fixture is treated as unhealthy,
    // so the signed route proves bounded failover to the next mounted shard.
    let mut shard_keypairs = (0..MAX_FEE_PAYER_SHARD_CANDIDATES.saturating_add(2))
        .map(|_| Keypair::new())
        .collect::<Vec<_>>();
    let mounted_shard_pubkeys = shard_keypairs
        .iter()
        .map(Signer::pubkey)
        .collect::<BTreeSet<_>>();
    let registry_shard_pubkeys = mounted_shard_pubkeys.clone();
    let ranked_shards = bounded_ranked_fee_payer_pubkeys(
        "controlled",
        "controlled-vault",
        mounted_shard_pubkeys.iter().copied().collect(),
    );
    let first_ranked_shard = *ranked_shards
        .first()
        .ok_or("controlled fee-payer ranking produced no primary shard")?;
    let selected_shard_pubkey = *ranked_shards
        .get(1)
        .ok_or("controlled fee-payer ranking produced no failover shard")?;
    let selected_shard_index = shard_keypairs
        .iter()
        .position(|keypair| keypair.pubkey() == selected_shard_pubkey)
        .ok_or("controlled failover shard has no mounted keypair")?;
    let fee_payer = shard_keypairs.swap_remove(selected_shard_index);
    let shard_registry_keypair_match = registry_shard_pubkeys.contains(&fee_payer.pubkey())
        && mounted_shard_pubkeys.contains(&fee_payer.pubkey())
        && fee_payer.pubkey() != policy_pubkey;
    let bounded_ranked_failover = mounted_shard_pubkeys.len() > MAX_FEE_PAYER_SHARD_CANDIDATES
        && ranked_shards.len() == MAX_FEE_PAYER_SHARD_CANDIDATES
        && first_ranked_shard != fee_payer.pubkey();

    let loaded_writable = Pubkey::new_unique();
    let loaded_readonly = Pubkey::new_unique();
    let route_instruction = Instruction {
        program_id: Pubkey::new_unique(),
        accounts: vec![
            AccountMeta::new_readonly(policy_pubkey, true),
            AccountMeta::new(loaded_writable, false),
            AccountMeta::new_readonly(loaded_readonly, false),
        ],
        data: b"loyal-controlled-fleet-route-v1".to_vec(),
    };
    let route_instructions = vec![route_instruction];
    let manifest_addresses =
        compiler_lookup_eligible_addresses(fee_payer.pubkey(), &route_instructions);
    if manifest_addresses.is_empty() {
        return Err("controlled route produced no ALT-eligible addresses".into());
    }
    let lookup_table_accounts = vec![AddressLookupTableAccount {
        key: Pubkey::new_unique(),
        addresses: manifest_addresses.clone(),
    }];

    let expected_base_fee_lamports = 5_000u64;
    let expected_compiled_fee_lamports = 6_000u64;
    let expected_simulation_units = 175_000u64;
    let confirmed_slot = 7_000u64;
    let mut compilation_mocks = MocksMap::default();
    compilation_mocks.insert(
        RpcRequest::GetFeeForMessage,
        controlled_rpc_response(confirmed_slot, json!(expected_base_fee_lamports)),
    );
    compilation_mocks.insert(
        RpcRequest::GetFeeForMessage,
        controlled_rpc_response(confirmed_slot, json!(expected_compiled_fee_lamports)),
    );
    compilation_mocks.insert(
        RpcRequest::GetFeeForMessage,
        controlled_rpc_response(confirmed_slot, json!(expected_compiled_fee_lamports)),
    );
    compilation_mocks.insert(
        RpcRequest::SimulateTransaction,
        controlled_rpc_response(
            confirmed_slot,
            json!({
                "err": null,
                "logs": [],
                "accounts": null,
                "unitsConsumed": expected_simulation_units,
                "loadedAccountsDataSize": null,
                "returnData": null,
                "innerInstructions": null,
                "replacementBlockhash": null,
            }),
        ),
    );
    compilation_mocks.insert(
        RpcRequest::GetMultipleAccounts,
        controlled_rpc_response(confirmed_slot, json!([null])),
    );
    compilation_mocks.insert(
        RpcRequest::GetMultipleAccounts,
        controlled_rpc_response(confirmed_slot.saturating_sub(1), json!([null])),
    );
    let controlled_rpc =
        RpcClient::new_mock_with_mocks_map("controlled-no-external-network", compilation_mocks);

    let signers = same_mint_route_signers(&fee_payer, &policy);
    let compiled = compile_budgeted_fleet_transaction(
        &controlled_rpc,
        fee_payer.pubkey(),
        &route_instructions,
        &lookup_table_accounts,
        Hash::new_unique(),
        &signers,
        150_000,
        TransactionFeeBudget {
            max_total_fee_lamports: 50_000,
            recent_priority_fee_micro_lamports: 100_000,
        },
    )?;
    let transaction = &compiled.transaction;
    let signer_count = usize::from(transaction.message.header().num_required_signatures);
    let static_signers = transaction
        .message
        .static_account_keys()
        .iter()
        .take(signer_count)
        .copied()
        .collect::<Vec<_>>();
    let signature_results = transaction.verify_with_results();
    let shard_is_final_fee_payer = static_signers.first() == Some(&fee_payer.pubkey())
        && compiled.packet.fee_payer == fee_payer.pubkey().to_string();
    let policy_is_second_static_signer = static_signers.get(1) == Some(&policy_pubkey);
    let route_signatures_valid = signer_count == 2
        && signature_results.len() == signer_count
        && signature_results.iter().all(|verified| *verified);

    let VersionedMessage::V0(message) = &transaction.message else {
        return Err("controlled route did not compile as a v0 transaction".into());
    };
    let mut loaded_writable_addresses = BTreeSet::new();
    let mut loaded_readonly_addresses = BTreeSet::new();
    for lookup in &message.address_table_lookups {
        let table = lookup_table_accounts
            .iter()
            .find(|table| table.key == lookup.account_key)
            .ok_or("compiled transaction referenced an unknown controlled ALT")?;
        for index in &lookup.writable_indexes {
            loaded_writable_addresses.insert(
                *table
                    .addresses
                    .get(usize::from(*index))
                    .ok_or("compiled writable ALT index exceeded controlled manifest")?,
            );
        }
        for index in &lookup.readonly_indexes {
            loaded_readonly_addresses.insert(
                *table
                    .addresses
                    .get(usize::from(*index))
                    .ok_or("compiled readonly ALT index exceeded controlled manifest")?,
            );
        }
    }
    let loaded_addresses = loaded_writable_addresses
        .union(&loaded_readonly_addresses)
        .copied()
        .collect::<BTreeSet<_>>();
    let expected_manifest_addresses = manifest_addresses.iter().copied().collect::<BTreeSet<_>>();
    let final_manifest_and_alt_coverage_match = loaded_addresses == expected_manifest_addresses
        && loaded_writable_addresses.contains(&loaded_writable)
        && loaded_readonly_addresses.contains(&loaded_readonly)
        && compiled.packet.address_table_lookup_count == lookup_table_accounts.len()
        && compiled.packet.loaded_writable_address_count == loaded_writable_addresses.len()
        && compiled.packet.loaded_readonly_address_count == loaded_readonly_addresses.len();

    let signed_transaction_bytes = bincode::serialize(transaction)?;
    let signed_transaction_hash = Sha256::digest(&signed_transaction_bytes);
    let message_bytes = bincode::serialize(&transaction.message)?;
    let message_hash = Sha256::digest(&message_bytes);
    let persisted_transaction: VersionedTransaction =
        bincode::deserialize(&signed_transaction_bytes)?;
    let persisted_packet =
        transaction_packet_summary(&persisted_transaction, &lookup_table_accounts)?;
    let persisted_transaction_hash = Sha256::digest(bincode::serialize(&persisted_transaction)?);
    let persisted_message_hash =
        Sha256::digest(bincode::serialize(&persisted_transaction.message)?);
    let verified_compiled_fee = versioned_message_fee(&controlled_rpc, &transaction.message)?;
    let final_packet_simulation_fee_and_hashes_match = compiled.packet.fits_packet_data_size
        && persisted_packet.packet_size_bytes == compiled.packet.packet_size_bytes
        && persisted_packet.packet_data_size_bytes == compiled.packet.packet_data_size_bytes
        && persisted_packet.address_table_lookup_count
            == compiled.packet.address_table_lookup_count
        && compiled.simulation_units_consumed == expected_simulation_units
        && compiled.compiled_fee_lamports == expected_compiled_fee_lamports
        && verified_compiled_fee == compiled.compiled_fee_lamports
        && signed_transaction_hash == persisted_transaction_hash
        && message_hash == persisted_message_hash
        && persisted_transaction
            .verify_with_results()
            .iter()
            .all(|verified| *verified);

    // Standard reusable ALT mutation instructions must keep POLICY_KEYPAIR as
    // both the table authority and payer. This transaction stays local and is
    // never submitted, but it uses the production v0 compiler and signer path.
    let (alt_create_instruction, _) = address_lookup_table_instruction::create_lookup_table(
        policy_pubkey,
        policy_pubkey,
        confirmed_slot,
    );
    let alt_create_transaction = compile_versioned_transaction(
        policy_pubkey,
        std::slice::from_ref(&alt_create_instruction),
        &[],
        Hash::new_unique(),
        &[&policy],
    )?;
    let alt_create_signatures_valid = alt_create_transaction
        .verify_with_results()
        .iter()
        .all(|verified| *verified);
    let alt_mutation_authorized_and_paid_by_policy =
        alt_create_transaction.message.static_account_keys().first() == Some(&policy_pubkey)
            && alt_create_instruction
                .accounts
                .get(1)
                .is_some_and(|account| account.pubkey == policy_pubkey)
            && alt_create_instruction
                .accounts
                .get(2)
                .is_some_and(|account| account.pubkey == policy_pubkey && account.is_signer)
            && alt_create_signatures_valid;

    let expected_signature = *transaction
        .signatures
        .first()
        .ok_or("controlled signed transaction has no fee-payer signature")?;
    let mut send_mocks = MocksMap::default();
    send_mocks.insert(
        RpcRequest::SendTransaction,
        json!(expected_signature.to_string()),
    );
    send_mocks.insert(
        RpcRequest::SendTransaction,
        json!(expected_signature.to_string()),
    );
    let send_rpc = RpcClient::new_mock_with_mocks_map("controlled-no-external-network", send_mocks);
    let mut identical_byte_rebroadcast_attempts = 0u64;
    let mut rebroadcast_byte_mismatches = 0u64;
    for _ in 0..2 {
        let replay: VersionedTransaction = bincode::deserialize(&signed_transaction_bytes)?;
        if bincode::serialize(&replay)? != signed_transaction_bytes {
            rebroadcast_byte_mismatches = rebroadcast_byte_mismatches.saturating_add(1);
        }
        let submitted_signature = send_rpc.send_transaction(&replay)?;
        if submitted_signature != expected_signature {
            return Err(
                "controlled mock sender changed the persisted transaction signature".into(),
            );
        }
        identical_byte_rebroadcast_attempts = identical_byte_rebroadcast_attempts.saturating_add(1);
    }

    // Production account batching must accept a post-confirmation read at the
    // confirmed slot and reject an RPC response that violates minContextSlot.
    let controlled_read_key = Pubkey::new_unique();
    let (post_confirm_accounts, post_confirm_requests) = get_multiple_accounts_batched(
        &controlled_rpc,
        &[controlled_read_key],
        Some(confirmed_slot),
    )?;
    let post_confirm_read_valid = post_confirm_requests == 1
        && post_confirm_accounts.len() == 1
        && post_confirm_accounts[0].1 >= confirmed_slot;
    let stale_context_rejected = get_multiple_accounts_batched(
        &controlled_rpc,
        &[controlled_read_key],
        Some(confirmed_slot),
    )
    .is_err();
    let post_confirm_reads = u64::from(post_confirm_read_valid);
    let min_context_slot_violations = u64::from(!stale_context_rejected);

    let policy_execution_signed_by_policy_keypair =
        route_signatures_valid && policy_is_second_static_signer;
    let setup_idle_and_farm_init_use_policy_payer =
        fee_only_shard_allowed_for_scope(FleetRouteFeePayerScope::MatureSameMint)
            && [
                FleetRouteFeePayerScope::ObligationSetup,
                FleetRouteFeePayerScope::IdleVault,
                FleetRouteFeePayerScope::FarmInit,
            ]
            .into_iter()
            .all(|scope| {
                let selected = if fee_only_shard_allowed_for_scope(scope) {
                    fee_payer.pubkey()
                } else {
                    policy_pubkey
                };
                selected == policy_pubkey
            });

    if !shard_registry_keypair_match
        || !bounded_ranked_failover
        || !shard_is_final_fee_payer
        || !policy_execution_signed_by_policy_keypair
        || !final_manifest_and_alt_coverage_match
        || !final_packet_simulation_fee_and_hashes_match
        || !alt_mutation_authorized_and_paid_by_policy
        || identical_byte_rebroadcast_attempts != 2
        || rebroadcast_byte_mismatches != 0
        || post_confirm_reads != 1
        || min_context_slot_violations != 0
        || !setup_idle_and_farm_init_use_policy_payer
    {
        return Err("controlled transaction probe invariant failed".into());
    }

    Ok(json!({
        "schemaVersion": 1,
        "event": "fleet_transaction_runtime_probe",
        "externalNetworkAccessed": false,
        "productionTransactionSent": false,
        "execution": {
            "identicalByteRebroadcastAttempts": identical_byte_rebroadcast_attempts,
            "rebroadcastByteMismatches": rebroadcast_byte_mismatches,
            "postConfirmReads": post_confirm_reads,
            "minContextSlotViolations": min_context_slot_violations,
            "policyExecutionSignedByPolicyKeypair": policy_execution_signed_by_policy_keypair,
            "altMutationsAuthorizedAndPaidByPolicyKeypair": alt_mutation_authorized_and_paid_by_policy,
            "shardedRouteFixtures": 1,
            "shardIsFinalFeePayer": shard_is_final_fee_payer,
            "policyIsSecondStaticSigner": policy_is_second_static_signer,
            "finalManifestAndAltCoverageMatch": final_manifest_and_alt_coverage_match,
            "finalPacketSimulationFeeAndHashesMatch": final_packet_simulation_fee_and_hashes_match,
            "setupIdleAndFarmInitUsePolicyPayer": setup_idle_and_farm_init_use_policy_payer,
            "shardRegistryKeypairMatch": shard_registry_keypair_match,
            "boundedRankedFailover": bounded_ranked_failover,
        },
    }))
}

fn rpc_batch_request_count(account_count: usize) -> usize {
    account_count.div_ceil(RPC_MULTIPLE_ACCOUNTS_LIMIT)
}

fn fleet_rpc_hot_path_model() -> Value {
    // Deterministic normal case: a mature two-reserve route, up to one farm
    // account per reserve, two reusable ALTs, and the full 16-payer shard set.
    let reserve_accounts = 2usize;
    let vault_accounts = 2 + (2 * 3); // metadata + policy + ATA/obligation/farm per reserve
    let lookup_table_accounts = 2usize;
    let fee_payer_accounts = MAX_FEE_PAYER_SHARD_CANDIDATES;
    let warm_account_read_requests =
        rpc_batch_request_count(vault_accounts) + rpc_batch_request_count(lookup_table_accounts);
    let cold_account_read_requests = warm_account_read_requests
        + rpc_batch_request_count(reserve_accounts)
        + rpc_batch_request_count(fee_payer_accounts);
    // These calls preserve final safety and price correctness: slot, priority
    // fee sample, blockhash, measurement simulation, final budgeted simulation,
    // baseline fee, and final compiled fee.
    let safety_and_price_requests = 7usize;
    let warm_total_requests = warm_account_read_requests + safety_and_price_requests;
    let cold_total_requests = cold_account_read_requests + safety_and_price_requests;
    let maximum_admission_balance_refresh_requests = 1usize;
    json!({
        "status": "INFORMATIONAL",
        "evidenceKind": "static_request_model_not_runtime_instrumentation",
        "scenario": "mature_two_reserve_route_two_reusable_alts",
        "accountReadRequests": {
            "warmWorker": warm_account_read_requests,
            "coldWorker": cold_account_read_requests,
            "legacyEquivalent": 13,
        },
        "totalRpcRequests": {
            "warmWorkerBeforeConditionalAdmissionRefresh": warm_total_requests,
            "coldWorkerBeforeConditionalAdmissionRefresh": cold_total_requests,
            "warmWorkerMaximum": warm_total_requests + maximum_admission_balance_refresh_requests,
            "coldWorkerMaximum": cold_total_requests + maximum_admission_balance_refresh_requests,
            "conditionalBoundPayerAdmissionRefreshMaximum": maximum_admission_balance_refresh_requests,
            "retainedSafetyAndPriceRequests": safety_and_price_requests,
        },
        "cacheFreshnessMilliseconds": {
            "sharedReserve": SHARED_RESERVE_CACHE_TTL.as_millis(),
            "policy": POLICY_ACCOUNT_CACHE_TTL.as_millis(),
            "feePayerBalance": FEE_PAYER_BALANCE_CACHE_TTL.as_millis(),
        },
        "invariants": [
            "vault and obligation state is never cached",
            "cache hits require the exact optimizer epoch when present",
            "minContextSlot rejects cache entries older than reconciliation fences",
            "the final budgeted transaction is still simulated",
        ],
    })
}

fn parse_fleet_worker_options(
    args: impl IntoIterator<Item = String>,
) -> Result<FleetWorkerOptions, Box<dyn Error>> {
    let mut args = args.into_iter();
    let lane = args
        .next()
        .ok_or("--fleet-worker requires revalidate or execute")?;
    let claim_kind = match lane.as_str() {
        "revalidate" => RebalanceOpportunityClaimKind::Revalidate,
        "execute" => RebalanceOpportunityClaimKind::Execute,
        _ => return Err("--fleet-worker requires revalidate or execute".into()),
    };
    let mut cluster = env::var("YIELD_ALT_CLUSTER").unwrap_or_else(|_| "mainnet-beta".to_owned());
    let mut rpc_url = env::var("SOLANA_RPC_URL").unwrap_or_else(|_| DEFAULT_SOLANA_RPC_URL.into());
    let mut owner = None;
    let mut concurrency = match claim_kind {
        RebalanceOpportunityClaimKind::Revalidate => DEFAULT_FLEET_REVALIDATE_CONCURRENCY,
        RebalanceOpportunityClaimKind::Execute => DEFAULT_FLEET_EXECUTE_CONCURRENCY,
    };
    let mut fused_execute_concurrency = match claim_kind {
        RebalanceOpportunityClaimKind::Revalidate => DEFAULT_FLEET_EXECUTE_CONCURRENCY,
        RebalanceOpportunityClaimKind::Execute => 0,
    };
    let mut lease_seconds = DEFAULT_FLEET_WORKER_LEASE_SECONDS;
    let mut poll_interval_milliseconds = DEFAULT_FLEET_WORKER_POLL_MILLISECONDS;
    let mut once = false;
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--cluster" => cluster = args.next().ok_or("--cluster requires a value")?,
            "--rpc-url" => rpc_url = args.next().ok_or("--rpc-url requires a value")?,
            "--owner" => owner = Some(args.next().ok_or("--owner requires a value")?),
            "--concurrency" => {
                concurrency = args
                    .next()
                    .ok_or("--concurrency requires a value")?
                    .parse()?;
            }
            "--fused-execute-concurrency" => {
                fused_execute_concurrency = args
                    .next()
                    .ok_or("--fused-execute-concurrency requires a value")?
                    .parse()?;
            }
            "--lease-seconds" => {
                lease_seconds = args
                    .next()
                    .ok_or("--lease-seconds requires a value")?
                    .parse()?;
            }
            "--poll-interval-milliseconds" => {
                poll_interval_milliseconds = args
                    .next()
                    .ok_or("--poll-interval-milliseconds requires a value")?
                    .parse()?;
            }
            "--once" => once = true,
            other => return Err(format!("unknown fleet-worker argument: {other}").into()),
        }
    }
    validate_alt_cluster(&cluster)?;
    validate_rpc_endpoint(&rpc_url)?;
    if concurrency == 0 || concurrency > 128 {
        return Err("fleet worker concurrency must be in 1..=128".into());
    }
    if fused_execute_concurrency > 128 {
        return Err("fused execute concurrency must be in 0..=128".into());
    }
    if claim_kind == RebalanceOpportunityClaimKind::Execute && fused_execute_concurrency != 0 {
        return Err("the execute lane cannot enable fused revalidate execution".into());
    }
    if lease_seconds < 30 || lease_seconds > 900 {
        return Err("fleet worker lease seconds must be in 30..=900".into());
    }
    if poll_interval_milliseconds == 0 || poll_interval_milliseconds > 60_000 {
        return Err("fleet worker poll interval must be in 1..=60000 milliseconds".into());
    }
    let owner = owner.unwrap_or_else(|| {
        format!(
            "same-mint-{}-{}-{}",
            claim_kind.as_str(),
            std::process::id(),
            env::var("HOSTNAME").unwrap_or_else(|_| "local".to_owned())
        )
    });
    if owner.trim().is_empty() {
        return Err("fleet worker owner must be nonempty".into());
    }
    Ok(FleetWorkerOptions {
        claim_kind,
        cluster,
        rpc_url,
        owner,
        concurrency,
        fused_execute_concurrency,
        lease_seconds,
        poll_interval_milliseconds,
        once,
    })
}

fn parse_fleet_reconciler_options(
    args: impl IntoIterator<Item = String>,
) -> Result<FleetReconcilerOptions, Box<dyn Error>> {
    let mut cluster = env::var("YIELD_ALT_CLUSTER").unwrap_or_else(|_| "mainnet-beta".to_owned());
    let mut rpc_url = env::var("SOLANA_RPC_URL").unwrap_or_else(|_| DEFAULT_SOLANA_RPC_URL.into());
    let mut owner = None;
    let mut concurrency = DEFAULT_FLEET_RECONCILE_CONCURRENCY;
    let mut batch_size = DEFAULT_FLEET_RECONCILE_BATCH_SIZE;
    let mut lease_seconds = DEFAULT_FLEET_WORKER_LEASE_SECONDS;
    let mut poll_interval_milliseconds = DEFAULT_FLEET_WORKER_POLL_MILLISECONDS;
    let mut position_sweep_interval_seconds = DEFAULT_FLEET_POSITION_SWEEP_INTERVAL_SECONDS;
    let mut once = false;
    let mut args = args.into_iter();
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--cluster" => cluster = args.next().ok_or("--cluster requires a value")?,
            "--rpc-url" => rpc_url = args.next().ok_or("--rpc-url requires a value")?,
            "--owner" => owner = Some(args.next().ok_or("--owner requires a value")?),
            "--concurrency" => {
                concurrency = args
                    .next()
                    .ok_or("--concurrency requires a value")?
                    .parse()?;
            }
            "--batch-size" => {
                batch_size = args
                    .next()
                    .ok_or("--batch-size requires a value")?
                    .parse()?;
            }
            "--lease-seconds" => {
                lease_seconds = args
                    .next()
                    .ok_or("--lease-seconds requires a value")?
                    .parse()?;
            }
            "--poll-interval-milliseconds" => {
                poll_interval_milliseconds = args
                    .next()
                    .ok_or("--poll-interval-milliseconds requires a value")?
                    .parse()?;
            }
            "--position-sweep-interval-seconds" => {
                position_sweep_interval_seconds = args
                    .next()
                    .ok_or("--position-sweep-interval-seconds requires a value")?
                    .parse()?;
            }
            "--once" => once = true,
            other => return Err(format!("unknown fleet-reconciler argument: {other}").into()),
        }
    }
    validate_alt_cluster(&cluster)?;
    validate_rpc_endpoint(&rpc_url)?;
    if concurrency == 0 || concurrency > 128 {
        return Err("fleet reconciler concurrency must be in 1..=128".into());
    }
    if !(1..=256).contains(&batch_size) {
        return Err("fleet reconciler batch size must be in 1..=256".into());
    }
    if lease_seconds < 30 || lease_seconds > 900 {
        return Err("fleet reconciler lease seconds must be in 30..=900".into());
    }
    if poll_interval_milliseconds == 0 || poll_interval_milliseconds > 60_000 {
        return Err("fleet reconciler poll interval must be in 1..=60000 milliseconds".into());
    }
    if position_sweep_interval_seconds == 0 || position_sweep_interval_seconds > 86_400 {
        return Err("fleet position sweep interval must be in 1..=86400 seconds".into());
    }
    let owner = owner.unwrap_or_else(|| {
        format!(
            "same-mint-reconciler-{}-{}",
            std::process::id(),
            env::var("HOSTNAME").unwrap_or_else(|_| "local".to_owned())
        )
    });
    if owner.trim().is_empty() {
        return Err("fleet reconciler owner must be nonempty".into());
    }
    Ok(FleetReconcilerOptions {
        cluster,
        rpc_url,
        owner,
        concurrency,
        batch_size,
        lease_seconds,
        poll_interval_milliseconds,
        position_sweep_interval_seconds,
        once,
    })
}

async fn run_fleet_worker(options: FleetWorkerOptions) -> Result<(), Box<dyn Error>> {
    let database_url =
        env::var("NEON_DATABASE_URL").map_err(|_| "NEON_DATABASE_URL must be set")?;
    let max_connections = u32::try_from(options.concurrency)
        .unwrap_or(128)
        .saturating_mul(3)
        .saturating_add(4)
        .min(128);
    let client = NeonSqlClient::connect(
        NeonSqlConfig::new(database_url).with_max_connections(max_connections),
    )
    .await?;
    client
        .require_schema_migration(24, "fleet_route_confirmer")
        .await?;
    client
        .require_schema_migration(25, "fee_only_route_payer_shards")
        .await?;
    client
        .require_schema_migration(26, "target_capacity_reservations")
        .await?;
    client
        .require_schema_migration(27, "rebalance_opportunity_attempt_generations")
        .await?;
    client
        .require_schema_migration(29, "fleet_commit_lifetime_fences")
        .await?;
    client
        .require_schema_migration(30, "fused_queue_accrual_binding")
        .await?;
    // The commit-lifetime fences must carry their dedicated SQLSTATE before
    // this worker can tell an expected end-of-epoch rejection from a real
    // persistence fault.
    client
        .require_schema_migration(31, "fleet_commit_lifetime_fence_errcode")
        .await?;
    // Validate the standard signer once at startup. Individual route builds
    // re-read and match it to the active policy before signing.
    let delegated_signer = standard_policy_keypair_from_env()?.pubkey().to_string();
    let (fee_payer_keypool_state, mounted_fee_payer_pubkeys) =
        match route_fee_payer_keypairs_from_env() {
            Ok(keypairs) if keypairs.is_empty() => ("unconfigured", BTreeSet::new()),
            Ok(keypairs) => (
                "valid",
                keypairs
                    .iter()
                    .map(|keypair| keypair.pubkey().to_string())
                    .collect::<BTreeSet<_>>(),
            ),
            Err(_) => ("invalid", BTreeSet::new()),
        };
    let route_runtime = Arc::new(
        SameMintRouteRuntime::new(&options.rpc_url, &options.cluster, client.clone(), true).await?,
    );
    let fused_execute_slots = Arc::new(Semaphore::new(options.fused_execute_concurrency));
    let mut wakeup_listener = DurablePgWakeupListener::new("loyal_yield_rebalance_wakeup")?;
    let mut tasks = JoinSet::<FleetWorkerTaskResult>::new();
    let mut claimed = 0u64;
    let mut completed = 0u64;
    let mut failed = 0u64;
    // Transitions the commit-lifetime fence refused. Counted apart from
    // `failed` so end-of-epoch backpressure does not read as a fault rate.
    let mut lifetime_fenced = 0u64;
    let mut outbox_acknowledged = 0u64;
    let mut fused_execute_permits = 0u64;
    let mut fused_execute_promotions = 0u64;
    let mut last_outbox_ack = None::<tokio::time::Instant>;
    let mut health_interval = tokio::time::interval(Duration::from_millis(
        FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS,
    ));
    health_interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    health_interval.tick().await;

    loop {
        if options.claim_kind == RebalanceOpportunityClaimKind::Revalidate
            && last_outbox_ack.is_none_or(|last| last.elapsed() >= Duration::from_secs(1))
        {
            outbox_acknowledged = outbox_acknowledged.saturating_add(
                client
                    .acknowledge_promoted_alt_outbox_batch(&options.cluster, 1024)
                    .await?,
            );
            last_outbox_ack = Some(tokio::time::Instant::now());
        }
        while tasks.len() < options.concurrency {
            let lease_expires_at = Utc::now() + ChronoDuration::seconds(options.lease_seconds);
            let claim_capacity = i64::try_from(options.concurrency - tasks.len())?;
            let leases = client
                .lease_rebalance_opportunity_batch(
                    &options.cluster,
                    &options.owner,
                    options.claim_kind,
                    claim_capacity,
                    lease_expires_at,
                )
                .await?;
            if leases.is_empty() {
                break;
            }
            claimed = claimed.saturating_add(u64::try_from(leases.len())?);
            for lease in leases {
                let fused_execute_permit =
                    if lease.claim_kind == RebalanceOpportunityClaimKind::Revalidate {
                        fused_execute_slots.clone().try_acquire_owned().ok()
                    } else {
                        None
                    };
                if fused_execute_permit.is_some() {
                    fused_execute_permits = fused_execute_permits.saturating_add(1);
                }
                let mut request = same_mint_request_from_opportunity(
                    &lease,
                    &options.rpc_url,
                    options.claim_kind,
                )
                .map_err(|error| error.to_string());
                if fused_execute_permit.is_some() {
                    if let Ok(request) = &mut request {
                        request.mode = SameMintRouteExecutionMode::RevalidateAndExecute;
                    }
                }
                let fallback_lease = lease.clone();
                let fallback_request = request.as_ref().ok().cloned();
                let worker_client = client.clone();
                let worker_runtime = route_runtime.clone();
                let fused_lease_state: FusedExecutionLeaseState = Arc::new(Mutex::new(None));
                let task_fused_lease_state = fused_lease_state.clone();
                let runtime_handle = tokio::runtime::Handle::current();
                // Route construction uses the synchronous Solana client and
                // keeps non-Send signer references across awaits. Catch every
                // task-local failure so an expected conflict or malformed job
                // is fenced back to its queue immediately instead of waiting
                // for the full crash-recovery TTL.
                tasks.spawn_blocking(move || {
                    let _fused_execute_permit = fused_execute_permit;
                    let attempt = catch_unwind(AssertUnwindSafe(
                        || -> Result<SameMintRouteExecutionOutcome, String> {
                            runtime_handle.block_on(async move {
                                let request = request?;
                                let conflict_keys = request_conflict_account_keys(&lease)?;
                                if lease.claim_kind == RebalanceOpportunityClaimKind::Execute {
                                    if conflict_keys.is_empty() {
                                        return Err(
                                            "ready opportunity is missing revalidated semantic conflict keys"
                                                .to_owned(),
                                        );
                                    }
                                    worker_client
                                        .acquire_route_account_conflict_leases(
                                            &lease,
                                            &conflict_keys,
                                            lease.expires_at,
                                        )
                                        .await
                                        .map_err(|error| error.to_string())?;
                                }
                                let outcome = execute_same_mint_route_with_runtime(
                                    request,
                                    &worker_runtime,
                                    Some(&task_fused_lease_state),
                                )
                                .await;
                                Ok(outcome)
                            })
                        },
                    ));
                    let effective_lease = fused_lease_state
                        .lock()
                        .ok()
                        .and_then(|promoted| promoted.clone())
                        .unwrap_or(fallback_lease);
                    match attempt {
                        Ok(Ok(outcome)) => FleetWorkerTaskResult {
                            lease: effective_lease,
                            outcome,
                        },
                        Ok(Err(error)) => fleet_worker_retry_result(
                            effective_lease,
                            fallback_request.as_ref(),
                            error,
                        ),
                        Err(_) => {
                            OperationalError::new(
                                "rebalance_worker_task_panicked",
                                "execute_fleet_rebalance_task",
                                "fleet rebalance task panicked before its fenced transition",
                            )
                            .retryable(true)
                            .recovery_required(true)
                            .emit();
                            fleet_worker_retry_result(
                                effective_lease,
                                fallback_request.as_ref(),
                                "fleet route task panicked before its fenced transition".to_owned(),
                            )
                        }
                    }
                });
            }
        }

        if tasks.is_empty() {
            if options.once {
                emit_fleet_worker_health(
                    &client,
                    &options,
                    &delegated_signer,
                    fee_payer_keypool_state,
                    &mounted_fee_payer_pubkeys,
                    claimed,
                    completed,
                    failed,
                    lifetime_fenced,
                    outbox_acknowledged,
                    fused_execute_permits,
                    fused_execute_promotions,
                    wakeup_listener.is_connected(),
                )
                .await?;
                break;
            }
            let listener_connected = wakeup_listener.is_connected();
            tokio::select! {
                _ = wait_for_rebalance_wakeup(
                    &mut wakeup_listener,
                    &client,
                    Duration::from_millis(options.poll_interval_milliseconds),
                    options.claim_kind,
                ) => {}
                _ = health_interval.tick() => {
                    emit_fleet_worker_health(
                        &client,
                        &options,
                        &delegated_signer,
                        fee_payer_keypool_state,
                        &mounted_fee_payer_pubkeys,
                        claimed,
                        completed,
                        failed,
                        lifetime_fenced,
                        outbox_acknowledged,
                        fused_execute_permits,
                        fused_execute_promotions,
                        listener_connected,
                    ).await?;
                }
            }
            continue;
        }

        let task = match next_fleet_worker_wakeup(&mut tasks, &mut health_interval).await {
            FleetWorkerWakeup::Task(task) => task,
            FleetWorkerWakeup::Health => {
                emit_fleet_worker_health(
                    &client,
                    &options,
                    &delegated_signer,
                    fee_payer_keypool_state,
                    &mounted_fee_payer_pubkeys,
                    claimed,
                    completed,
                    failed,
                    lifetime_fenced,
                    outbox_acknowledged,
                    fused_execute_permits,
                    fused_execute_promotions,
                    wakeup_listener.is_connected(),
                )
                .await?;
                continue;
            }
        }
        .ok_or("fleet worker task set unexpectedly became empty")?;
        match task {
            Ok(result) => {
                if options.claim_kind == RebalanceOpportunityClaimKind::Revalidate
                    && result.lease.claim_kind == RebalanceOpportunityClaimKind::Execute
                {
                    fused_execute_promotions = fused_execute_promotions.saturating_add(1);
                }
                match finish_fleet_worker_task(&client, result).await {
                    Ok(()) => completed = completed.saturating_add(1),
                    Err(error) => {
                        if is_commit_lifetime_fence_rejection(error.as_ref()) {
                            lifetime_fenced = lifetime_fenced.saturating_add(1);
                            println!(
                                "{}",
                                serde_json::to_string(&json!({
                                    "status": "fleet_worker_transition_lifetime_fenced",
                                    "lane": options.claim_kind.as_str(),
                                    "reason": redacted_external_error(&error.to_string()),
                                    "stateChanged": false,
                                    "signerLoaded": false,
                                    "transactionsSent": false,
                                }))?
                            );
                        } else {
                            failed = failed.saturating_add(1);
                            OperationalError::new(
                                "rebalance_queue_transition_failed",
                                "finish_fleet_rebalance_task",
                                "fleet rebalance task could not persist its durable queue transition",
                            )
                            .retryable(true)
                            .recovery_required(true)
                            .emit();
                            eprintln!(
                                "{}",
                                serde_json::to_string(&json!({
                                    "status": "fleet_worker_transition_failed",
                                    "lane": options.claim_kind.as_str(),
                                    "error": redacted_external_error(&error.to_string()),
                                }))?
                            );
                        }
                    }
                }
            }
            Err(error) => {
                failed = failed.saturating_add(1);
                OperationalError::new(
                    "rebalance_worker_task_failed",
                    "join_fleet_rebalance_task",
                    "fleet rebalance task failed to join",
                )
                .retryable(true)
                .recovery_required(true)
                .emit();
                eprintln!(
                    "{}",
                    serde_json::to_string(&json!({
                        "status": "fleet_worker_join_failed",
                        "lane": options.claim_kind.as_str(),
                        "error": redacted_external_error(&error.to_string()),
                    }))?
                );
            }
        }
    }
    Ok(())
}

async fn wait_for_rebalance_wakeup(
    listener: &mut DurablePgWakeupListener,
    client: &NeonSqlClient,
    recovery_poll: Duration,
    claim_kind: RebalanceOpportunityClaimKind,
) {
    match listener.wait(client.pool(), recovery_poll).await {
        DurablePgWakeupEvent::Notification | DurablePgWakeupEvent::RecoveryPollElapsed => {}
        DurablePgWakeupEvent::Reconnected => {
            eprintln!(
                "{}",
                json!({
                    "status": "fleet_worker_wakeup_listener_reconnected",
                    "lane": claim_kind.as_str(),
                    "immediateDurableScan": true,
                })
            );
        }
        DurablePgWakeupEvent::Disconnected { error, retry_after } => {
            eprintln!(
                "{}",
                json!({
                    "status": "fleet_worker_wakeup_listener_disconnected",
                    "lane": claim_kind.as_str(),
                    "error": redacted_external_error(&error),
                    "durablePollingActive": true,
                    "immediateDurableScan": true,
                    "retryBackoffMilliseconds": retry_after.as_millis(),
                })
            );
        }
        DurablePgWakeupEvent::ReconnectFailed { error, retry_after } => {
            eprintln!(
                "{}",
                json!({
                    "status": "fleet_worker_wakeup_listener_reconnect_failed",
                    "lane": claim_kind.as_str(),
                    "error": redacted_external_error(&error),
                    "durablePollingActive": true,
                    "immediateDurableScan": true,
                    "retryBackoffMilliseconds": retry_after.as_millis(),
                })
            );
        }
    }
}

async fn run_fleet_reconciler(options: FleetReconcilerOptions) -> Result<(), Box<dyn Error>> {
    let database_url =
        env::var("NEON_DATABASE_URL").map_err(|_| "NEON_DATABASE_URL must be set")?;
    let max_connections = u32::try_from(options.concurrency)
        .unwrap_or(128)
        .saturating_mul(2)
        .saturating_add(4)
        .min(128);
    let client = NeonSqlClient::connect(
        NeonSqlConfig::new(database_url).with_max_connections(max_connections),
    )
    .await?;
    client
        .require_schema_migration(24, "fleet_route_confirmer")
        .await?;
    client
        .require_schema_migration(25, "fee_only_route_payer_shards")
        .await?;
    client
        .require_schema_migration(26, "target_capacity_reservations")
        .await?;
    client
        .require_schema_migration(27, "rebalance_opportunity_attempt_generations")
        .await?;
    client
        .require_schema_migration(29, "fleet_commit_lifetime_fences")
        .await?;
    client
        .require_schema_migration(30, "fused_queue_accrual_binding")
        .await?;
    let mut wakeup_listener =
        DurablePgWakeupListener::new("loyal_yield_route_reconciliation_wakeup")?;
    let runtime = Arc::new(
        SameMintRouteRuntime::new(&options.rpc_url, &options.cluster, client.clone(), false)
            .await?,
    );
    let mut completed = 0u64;
    let mut deferred = 0u64;
    let mut outer_task_failure_count = 0u64;
    let mut outer_task_panic_count = 0u64;
    let mut outer_task_join_failure_count = 0u64;
    let mut outer_task_fenced_deferral_count = 0u64;
    let mut outer_task_fenced_deferral_failure_count = 0u64;
    let mut first_outer_task_error = None::<String>;
    let mut health_interval = tokio::time::interval(Duration::from_millis(
        FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS,
    ));
    health_interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    health_interval.tick().await;
    // `health_interval` only paces the inner select arm, which runs while
    // reconciliation tasks are in flight. An idle reconciler claims nothing,
    // breaks out of that arm immediately, and reaches the outer-loop emission
    // below on every pass of a 250ms recovery poll. Both paths share this rate
    // limit so the expensive `fleet_orchestration_status` read stays governed
    // by the health interval instead of the poll interval.
    let health_emit_interval =
        Duration::from_millis(FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS);
    let mut last_health_emit: Option<tokio::time::Instant> = None;
    let mut position_sweep = FleetPositionSweepCoordinator::new(Duration::from_secs(
        options.position_sweep_interval_seconds,
    ));
    // Match the planner's durable eligible-vault denominator exactly. This
    // resolved set remains fixed until an explicit worker restart/deploy.
    let enabled_mints = enabled_stable_mints_from_env()?;

    loop {
        let leases = client
            .lease_reconciliation_pending_signed_route_submissions(
                &options.cluster,
                &options.owner,
                options.batch_size,
                Utc::now() + ChronoDuration::seconds(options.lease_seconds),
            )
            .await?;
        let claimed = leases.len();
        let mut tasks = JoinSet::new();
        let mut pending = leases.into_iter();
        loop {
            while tasks.len() < options.concurrency {
                let Some(lease) = pending.next() else {
                    break;
                };
                let fallback_lease = lease.clone();
                let task_runtime = runtime.clone();
                let runtime_handle = tokio::runtime::Handle::current();
                tasks.spawn_blocking(move || {
                    let attempt = catch_unwind(AssertUnwindSafe(|| {
                        runtime_handle
                            .block_on(reconcile_signed_route_submission(task_runtime, lease))
                    }));
                    let outcome = match attempt {
                        Ok(Ok(completed)) => FleetReconcilerTaskOutcome::Completed(completed),
                        Ok(Err(error)) => FleetReconcilerTaskOutcome::Failed {
                            kind: OuterTaskFailureKind::ReturnedError,
                            error,
                        },
                        Err(_) => FleetReconcilerTaskOutcome::Failed {
                            kind: OuterTaskFailureKind::Panicked,
                            error: "fleet reconciler task panicked before its fenced transition"
                                .to_owned(),
                        },
                    };
                    FleetReconcilerTaskResult {
                        lease: fallback_lease,
                        outcome,
                    }
                });
            }
            let result = tokio::select! {
                result = tasks.join_next() => result,
                _ = health_interval.tick() => {
                    emit_fleet_reconciler_health(
                        &client,
                        &options,
                        claimed,
                        completed,
                        deferred,
                        outer_task_failure_count,
                        outer_task_panic_count,
                        outer_task_join_failure_count,
                        outer_task_fenced_deferral_count,
                        outer_task_fenced_deferral_failure_count,
                        first_outer_task_error.as_deref(),
                        wakeup_listener.is_connected(),
                        &position_sweep,
                    )
                    .await?;
                    last_health_emit = Some(tokio::time::Instant::now());
                    continue;
                }
            };
            let Some(result) = result else {
                break;
            };
            match result {
                Ok(FleetReconcilerTaskResult {
                    outcome: FleetReconcilerTaskOutcome::Completed(true),
                    ..
                }) => completed = completed.saturating_add(1),
                Ok(FleetReconcilerTaskResult {
                    outcome: FleetReconcilerTaskOutcome::Completed(false),
                    ..
                }) => deferred = deferred.saturating_add(1),
                Ok(FleetReconcilerTaskResult {
                    lease,
                    outcome: FleetReconcilerTaskOutcome::Failed { kind, error },
                }) => {
                    outer_task_failure_count = outer_task_failure_count.saturating_add(1);
                    if kind == OuterTaskFailureKind::Panicked {
                        outer_task_panic_count = outer_task_panic_count.saturating_add(1);
                    }
                    let redacted_error = redacted_external_error(&error);
                    if first_outer_task_error.is_none() {
                        OperationalError::new(
                            "rebalance_reconciler_execution_failed",
                            "reconcile_fleet_rebalance_submission",
                            "fleet rebalance reconciler execution failed",
                        )
                        .retryable(true)
                        .recovery_required(true)
                        .emit();
                        eprintln!(
                            "{}",
                            json!({
                                "status": "fleet_reconciler_outer_task_failure",
                                "kind": kind,
                                "error": redacted_error,
                                "fencedRecoveryAttempted": true,
                            })
                        );
                        first_outer_task_error = Some(redacted_error);
                    }
                    let recovery = outer_task_failure_recovery(kind, true);
                    if let Some(advance) = recovery.fenced_deferred_advance(Utc::now()) {
                        match client
                            .advance_signed_route_submission(&lease, advance)
                            .await
                        {
                            Ok(_) => {
                                deferred = deferred.saturating_add(1);
                                outer_task_fenced_deferral_count =
                                    outer_task_fenced_deferral_count.saturating_add(1);
                            }
                            Err(error) => {
                                outer_task_fenced_deferral_failure_count =
                                    outer_task_fenced_deferral_failure_count.saturating_add(1);
                                OperationalError::new(
                                    "rebalance_recovery_transition_failed",
                                    "defer_failed_rebalance_reconciliation",
                                    "fleet rebalance recovery transition failed",
                                )
                                .retryable(true)
                                .recovery_required(true)
                                .emit();
                                eprintln!(
                                    "{}",
                                    json!({
                                        "status": "fleet_reconciler_outer_task_fenced_deferral_failed",
                                        "kind": kind,
                                        "error": redacted_external_error(&error.to_string()),
                                        "leaseRetainedUntil": lease.expires_at,
                                    })
                                );
                            }
                        }
                    }
                }
                Err(error) => {
                    outer_task_failure_count = outer_task_failure_count.saturating_add(1);
                    outer_task_join_failure_count = outer_task_join_failure_count.saturating_add(1);
                    let redacted_error = redacted_external_error(&error.to_string());
                    if first_outer_task_error.is_none() {
                        OperationalError::new(
                            "rebalance_reconciler_task_join_failed",
                            "join_fleet_rebalance_reconciler_task",
                            "fleet rebalance reconciler task failed to join",
                        )
                        .retryable(true)
                        .recovery_required(true)
                        .emit();
                        eprintln!(
                            "{}",
                            json!({
                                "status": "fleet_reconciler_outer_task_join_failure",
                                "kind": OuterTaskFailureKind::JoinFailure,
                                "error": redacted_error,
                                "fencedRecoveryAttempted": false,
                                "reason": "lease unavailable after join failure",
                            })
                        );
                        first_outer_task_error = Some(redacted_error);
                    }
                }
            }
        }

        // Signed submissions always claim and reconcile first. Advance only
        // one bounded position wave afterward so a continuously nonempty
        // confirmation queue cannot starve fleet freshness forever.
        let position_sweep_progressed = if position_sweep.due() {
            advance_fleet_position_sweep(
                runtime.clone(),
                &options,
                &enabled_mints,
                &mut position_sweep,
            )
            .await
        } else {
            false
        };

        // A `--once` run must always report before exiting; otherwise emit only
        // when the health interval is actually due. Without this gate an idle
        // reconciler re-ran the `fleet_orchestration_status` aggregate on every
        // 250ms recovery poll, which is the load the interval is meant to cap.
        let health_due = options.once
            || last_health_emit.is_none_or(|last| last.elapsed() >= health_emit_interval);
        if health_due {
            emit_fleet_reconciler_health(
                &client,
                &options,
                claimed,
                completed,
                deferred,
                outer_task_failure_count,
                outer_task_panic_count,
                outer_task_join_failure_count,
                outer_task_fenced_deferral_count,
                outer_task_fenced_deferral_failure_count,
                first_outer_task_error.as_deref(),
                wakeup_listener.is_connected(),
                &position_sweep,
            )
            .await?;
            last_health_emit = Some(tokio::time::Instant::now());
        }
        if options.once {
            break;
        }
        if claimed == 0 && !position_sweep_progressed {
            wait_for_reconciliation_wakeup(
                &mut wakeup_listener,
                &client,
                Duration::from_millis(options.poll_interval_milliseconds),
            )
            .await;
        }
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn emit_fleet_reconciler_health(
    client: &NeonSqlClient,
    options: &FleetReconcilerOptions,
    claimed: usize,
    completed: u64,
    deferred: u64,
    outer_task_failure_count: u64,
    outer_task_panic_count: u64,
    outer_task_join_failure_count: u64,
    outer_task_fenced_deferral_count: u64,
    outer_task_fenced_deferral_failure_count: u64,
    first_outer_task_error: Option<&str>,
    wakeup_listener_connected: bool,
    position_sweep: &FleetPositionSweepCoordinator,
) -> Result<(), Box<dyn Error>> {
    let status = client.fleet_orchestration_status(&options.cluster).await?;
    let observed_at = Utc::now();
    let stage_health = fleet_stage_health_report(
        &status,
        options.poll_interval_milliseconds,
        FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS,
        observed_at,
    )
    .ok();
    println!(
        "{}",
        serde_json::to_string(&json!({
            "status": "fleet_reconciler_healthy",
            "cluster": options.cluster,
            "owner": options.owner,
            "concurrency": options.concurrency,
            "claimed": claimed,
            "completed": completed,
            "deferred": deferred,
            "wakeupListenerConnected": wakeup_listener_connected,
            "durableRecoveryPollMilliseconds": options.poll_interval_milliseconds,
            "healthObservationIntervalMilliseconds": FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS,
            "positionSweepIntervalSeconds": options.position_sweep_interval_seconds,
            "positionSweepBatchSize": options.concurrency,
            "positionSweep": position_sweep.health_json(),
            "positionSweepSignerLoaded": false,
            "positionSweepTransactionsSent": false,
            "outerTaskFailureCount": outer_task_failure_count,
            "outerTaskPanicCount": outer_task_panic_count,
            "outerTaskJoinFailureCount": outer_task_join_failure_count,
            "outerTaskFencedDeferralCount": outer_task_fenced_deferral_count,
            "outerTaskFencedDeferralFailureCount": outer_task_fenced_deferral_failure_count,
            "firstOuterTaskError": first_outer_task_error,
            "queue": status,
            "stageHealth": stage_health,
            "observedAt": observed_at,
        }))?
    );
    Ok(())
}

async fn wait_for_reconciliation_wakeup(
    listener: &mut DurablePgWakeupListener,
    client: &NeonSqlClient,
    recovery_poll: Duration,
) {
    match listener.wait(client.pool(), recovery_poll).await {
        DurablePgWakeupEvent::Notification | DurablePgWakeupEvent::RecoveryPollElapsed => {}
        DurablePgWakeupEvent::Reconnected => {
            eprintln!(
                "{}",
                json!({
                    "status": "fleet_reconciler_wakeup_listener_reconnected",
                    "immediateDurableScan": true,
                })
            );
        }
        DurablePgWakeupEvent::Disconnected { error, retry_after } => {
            eprintln!(
                "{}",
                json!({
                    "status": "fleet_reconciler_wakeup_listener_disconnected",
                    "error": redacted_external_error(&error),
                    "durablePollingActive": true,
                    "immediateDurableScan": true,
                    "retryBackoffMilliseconds": retry_after.as_millis(),
                })
            );
        }
        DurablePgWakeupEvent::ReconnectFailed { error, retry_after } => {
            eprintln!(
                "{}",
                json!({
                    "status": "fleet_reconciler_wakeup_listener_reconnect_failed",
                    "error": redacted_external_error(&error),
                    "durablePollingActive": true,
                    "immediateDurableScan": true,
                    "retryBackoffMilliseconds": retry_after.as_millis(),
                })
            );
        }
    }
}

#[derive(Debug)]
struct FleetPositionSweepTaskResult {
    vault_id: Option<VaultId>,
    outcome: FleetPositionSweepTaskOutcome,
}

/// Separates per-vault sweep faults that clear on their own from faults that
/// need a person, mirroring [`FleetPositionSweepInitFailureKind`] at the
/// initialization site.
///
/// Both used to share one retryable operational error alongside the frozen-
/// cohort races now reported as [`FleetPositionSweepTaskOutcome::Superseded`],
/// so an upstream RPC blip and a chain position outside the policy's stable
/// universe were indistinguishable to an operator.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum FleetPositionSweepVaultFailureKind {
    /// Upstream RPC or database unavailability. Retrying is the whole recovery.
    Transport,
    /// The policy, catalog reserve roles, or on-chain position identity cannot
    /// support a refresh. Retrying alone will not clear this.
    Invariant,
}

impl FleetPositionSweepVaultFailureKind {
    fn as_str(self) -> &'static str {
        match self {
            Self::Transport => "transport",
            Self::Invariant => "invariant",
        }
    }
}

#[derive(Debug)]
struct FleetPositionSweepVaultError {
    kind: FleetPositionSweepVaultFailureKind,
    error: String,
}

impl FleetPositionSweepVaultError {
    fn of_kind(kind: FleetPositionSweepVaultFailureKind, error: impl Into<String>) -> Self {
        Self {
            kind,
            error: redacted_external_error(&error.into()),
        }
    }

    fn transport(error: impl Into<String>) -> Self {
        Self::of_kind(FleetPositionSweepVaultFailureKind::Transport, error)
    }

    fn invariant(error: impl Into<String>) -> Self {
        Self::of_kind(FleetPositionSweepVaultFailureKind::Invariant, error)
    }

    /// Classifies a database failure behind the shared schema-versus-
    /// connectivity rule instead of assuming every database fault is transient.
    fn from_sqlx(error: &loyal_yield_orchestrator::sqlx::Error) -> Self {
        if sqlx_failure_is_invariant(error) {
            Self::invariant(error.to_string())
        } else {
            Self::transport(error.to_string())
        }
    }

    /// Classifies a store failure by variant. `OrchestratorError` is typed, so
    /// only the database arm needs the connectivity rule; every other variant
    /// reports a consistency or conversion fault that retrying cannot repair
    /// and that must stay independently visible.
    ///
    /// `StaleVaultObservation` never reaches here: the caller reports it as
    /// [`FleetPositionSweepTaskOutcome::Stale`] before classification.
    fn from_orchestrator(error: &OrchestratorError) -> Self {
        match error {
            OrchestratorError::Sqlx(sqlx_error) => Self::from_sqlx(sqlx_error),
            other => Self::invariant(other.to_string()),
        }
    }

    /// Classifies a chain-read failure that arrived as an untyped boxed error.
    ///
    /// The preview path fuses RPC transport with owner, mint, market, and
    /// obligation-identity assertions. Only error types known to clear on their
    /// own count as transport; anything else stays an invariant so a genuine
    /// chain-identity fault keeps paging per occurrence instead of being
    /// absorbed by the consecutive-transport threshold, which an isolated vault
    /// would never cross.
    fn from_chain_read(error: &(dyn Error + 'static)) -> Self {
        Self::of_kind(classify_chain_read_error(error), error.to_string())
    }
}

/// Reads the whole source chain so a transport fault stays transport even when
/// the preview path wraps it on the way out.
fn classify_chain_read_error(error: &(dyn Error + 'static)) -> FleetPositionSweepVaultFailureKind {
    let transport = error_chain(error).any(|source| {
        source.downcast_ref::<ClientError>().is_some()
            || source.downcast_ref::<TransientChainReadError>().is_some()
    });
    if transport {
        FleetPositionSweepVaultFailureKind::Transport
    } else {
        FleetPositionSweepVaultFailureKind::Invariant
    }
}

#[derive(Debug)]
enum FleetPositionSweepTaskOutcome {
    Refreshed,
    Stale,
    /// The frozen cohort no longer describes this vault: it was deactivated,
    /// its policy was replaced, or its identity changed between cohort freeze
    /// and refresh. The pre-RPC re-read exists to catch exactly this, so it is
    /// the guard succeeding rather than a failure to report.
    Superseded(String),
    Failed(FleetPositionSweepVaultError),
}

/// Advances at most one bounded RPC wave. The outer reconciler always claims
/// signed submissions before calling this function, and returns to that claim
/// path immediately after the batch, so background freshness cannot starve a
/// confirmed movement awaiting durable reconciliation.
async fn advance_fleet_position_sweep(
    runtime: Arc<SameMintRouteRuntime>,
    options: &FleetReconcilerOptions,
    enabled_mints: &[String],
    coordinator: &mut FleetPositionSweepCoordinator,
) -> bool {
    if coordinator.active.is_none() {
        let sweep_id = coordinator.next_sweep_id();
        match initialize_fleet_position_sweep(
            runtime.as_ref(),
            &options.cluster,
            enabled_mints,
            sweep_id,
        )
        .await
        {
            Ok(run) => {
                coordinator.record_initialization_success();
                coordinator.active = Some(run);
            }
            Err(failure) => {
                let kind = failure.kind;
                let error = failure.redacted_message();
                let emit_operational_error =
                    coordinator.record_initialization_failure(sweep_id, kind, error.clone());
                if emit_operational_error {
                    match kind {
                        FleetPositionSweepInitFailureKind::Transport => OperationalError::new(
                            "rebalance_position_sweep_initialization_failed",
                            "initialize_rebalance_position_sweep",
                            "fleet rebalance position sweep is blocked on a sustained upstream outage",
                        )
                        .retryable(true)
                        .recovery_required(false)
                        .emit(),
                        FleetPositionSweepInitFailureKind::Invariant => OperationalError::new(
                            "rebalance_position_sweep_invariant_blocked",
                            "initialize_rebalance_position_sweep",
                            "fleet rebalance position sweep cannot start under the active catalog or policy cohort",
                        )
                        .retryable(false)
                        .recovery_required(true)
                        .emit(),
                    }
                }
                eprintln!(
                    "{}",
                    json!({
                        "status": "fleet_position_sweep_initialization_failed",
                        "sweepId": sweep_id,
                        "kind": kind.as_str(),
                        "error": error,
                        "consecutiveTransportFailures":
                            coordinator.consecutive_transport_failures(),
                        "operationalErrorEmitted": emit_operational_error,
                        "complete": false,
                        "signerLoaded": false,
                        "transactionsSent": false,
                    })
                );
                return true;
            }
        }
    }

    let Some(run) = coordinator.active.as_ref() else {
        return false;
    };
    if run.next_index >= run.vaults.len() {
        if let Some(metrics) = coordinator.record_completion() {
            println!(
                "{}",
                json!({
                    "status": "fleet_position_sweep_complete",
                    "metrics": metrics,
                    "signerLoaded": false,
                    "transactionsSent": false,
                })
            );
        }
        return true;
    }

    let (batch, universe, sweep_id, started_at) = {
        let run = coordinator
            .active
            .as_ref()
            .expect("position sweep was initialized above");
        let end = run
            .next_index
            .saturating_add(options.concurrency)
            .min(run.vaults.len());
        (
            run.vaults[run.next_index..end].to_vec(),
            run.universe.clone(),
            run.sweep_id,
            run.started_at,
        )
    };
    let batch_cursor = batch.last().map(|entry| entry.vault.id.as_i64());
    coordinator.begin_vault_wave();
    let outcomes = reconcile_fleet_position_sweep_batch(
        runtime,
        batch,
        universe,
        sweep_id,
        started_at,
        options.concurrency,
    )
    .await;

    // The alert decision reads and mutates the coordinator's consecutive-failure
    // run, so it is resolved before the active run is borrowed mutably below.
    // The streak is captured per outcome rather than once after the batch: a
    // later vault in the same batch resets or advances the counter, so a single
    // post-batch read would stamp every record with a value that does not
    // explain the decision the record reports.
    let outcomes = outcomes
        .into_iter()
        .map(|outcome| {
            let emit_operational_error = match &outcome.outcome {
                FleetPositionSweepTaskOutcome::Refreshed
                | FleetPositionSweepTaskOutcome::Stale
                | FleetPositionSweepTaskOutcome::Superseded(_) => {
                    coordinator.record_vault_transport_success();
                    false
                }
                FleetPositionSweepTaskOutcome::Failed(error) => {
                    coordinator.record_vault_failure(error.kind)
                }
            };
            let consecutive_vault_transport_failures =
                coordinator.consecutive_vault_transport_failures();
            (
                outcome,
                emit_operational_error,
                consecutive_vault_transport_failures,
            )
        })
        .collect::<Vec<_>>();

    if let Some(run) = coordinator.active.as_mut() {
        run.next_index = run.next_index.saturating_add(outcomes.len());
        run.cursor_vault_id = batch_cursor.or(run.cursor_vault_id);
        for (outcome, emit_operational_error, consecutive_vault_transport_failures) in outcomes {
            run.processed = run.processed.saturating_add(1);
            let vault_id = outcome.vault_id.map(VaultId::as_i64);
            match outcome.outcome {
                FleetPositionSweepTaskOutcome::Refreshed => {
                    run.refreshed = run.refreshed.saturating_add(1);
                }
                FleetPositionSweepTaskOutcome::Stale => {
                    run.stale = run.stale.saturating_add(1);
                }
                // The frozen cohort going out of date is the pre-RPC re-read
                // doing its job, so this is recorded at info and never pages.
                FleetPositionSweepTaskOutcome::Superseded(reason) => {
                    run.superseded = run.superseded.saturating_add(1);
                    println!(
                        "{}",
                        json!({
                            "status": "fleet_position_sweep_vault_superseded",
                            "sweepId": run.sweep_id,
                            "vaultId": vault_id,
                            "reason": reason,
                            "stateChanged": false,
                            "signerLoaded": false,
                            "transactionsSent": false,
                        })
                    );
                }
                FleetPositionSweepTaskOutcome::Failed(error) => {
                    run.failed = run.failed.saturating_add(1);
                    if emit_operational_error {
                        let invariant =
                            matches!(error.kind, FleetPositionSweepVaultFailureKind::Invariant);
                        let (code, summary) = if invariant {
                            (
                                "rebalance_vault_position_invariant_blocked",
                                "fleet rebalance vault position refresh hit a policy or chain identity invariant",
                            )
                        } else {
                            (
                                "rebalance_vault_position_refresh_failed",
                                "fleet rebalance vault position refresh failed during the sweep",
                            )
                        };
                        OperationalError::new(code, "refresh_rebalance_vault_position", summary)
                            .retryable(!invariant)
                            .recovery_required(invariant)
                            .emit();
                    }
                    eprintln!(
                        "{}",
                        json!({
                            "status": "fleet_position_sweep_vault_failed",
                            "sweepId": run.sweep_id,
                            "vaultId": vault_id,
                            "kind": error.kind.as_str(),
                            "error": error.error,
                            "consecutiveVaultTransportFailures":
                                consecutive_vault_transport_failures,
                            "operationalErrorEmitted": emit_operational_error,
                            "stateChanged": false,
                            "signerLoaded": false,
                            "transactionsSent": false,
                        })
                    );
                }
            }
        }
    }

    let complete = coordinator
        .active
        .as_ref()
        .is_some_and(|run| run.next_index >= run.vaults.len());
    if complete {
        if let Some(metrics) = coordinator.record_completion() {
            println!(
                "{}",
                json!({
                    "status": "fleet_position_sweep_complete",
                    "metrics": metrics,
                    "signerLoaded": false,
                    "transactionsSent": false,
                })
            );
        }
    } else {
        coordinator.record_progress();
        println!(
            "{}",
            json!({
                "status": "fleet_position_sweep_progress",
                "metrics": coordinator.health_json(),
                "signerLoaded": false,
                "transactionsSent": false,
            })
        );
    }
    true
}

async fn initialize_fleet_position_sweep(
    runtime: &SameMintRouteRuntime,
    cluster: &str,
    enabled_mints: &[String],
    sweep_id: u64,
) -> Result<FleetPositionSweepRun, FleetPositionSweepInitError> {
    let started_at = Utc::now();
    let started = Instant::now();
    let vaults = load_fleet_position_sweep_vaults(&runtime.pool, enabled_mints)
        .await
        .map_err(FleetPositionSweepInitError::from_sqlx)?;
    let universe = load_fleet_position_sweep_universe(runtime, cluster, enabled_mints).await?;
    Ok(FleetPositionSweepRun {
        sweep_id,
        started_at,
        started,
        universe: Arc::new(universe),
        vaults,
        next_index: 0,
        cursor_vault_id: None,
        processed: 0,
        refreshed: 0,
        failed: 0,
        stale: 0,
        superseded: 0,
    })
}

/// Freezes the ordered cohort for one sweep.
///
/// Every predicate in the WHERE clause below is re-checked per vault in
/// `reconcile_fleet_position_sweep_vault` immediately before its RPC work, and
/// a predicate that no longer holds there is reported as `Superseded` instead
/// of a failure. Changing the predicates here without updating that recheck
/// makes the sweep page on the expected mid-sweep policy change the recheck
/// exists to absorb.
async fn load_fleet_position_sweep_vaults(
    pool: &PgPool,
    enabled_mints: &[String],
) -> Result<Vec<FleetPositionSweepVault>, loyal_yield_orchestrator::sqlx::Error> {
    let rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            v.id,
            v.settings,
            p.authority,
            p.policy_seed,
            v.vault_index,
            v.vault_pubkey,
            p.policy_account,
            sp.policy_account AS setup_policy_account,
            sp.policy_seed AS setup_policy_seed,
            p.delegated_signers,
            p.threshold,
            p.route_modes,
            p.stable_mints,
            p.kamino_markets,
            p.kamino_liquidity_mints,
            p.swap_lanes
        FROM loyal_yield.managed_vaults v
        JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
        LEFT JOIN loyal_yield.route_policies sp ON sp.id = v.setup_policy_id
          AND sp.active = TRUE
        LEFT JOIN (
            SELECT position.vault_id, max(position.observed_at) AS last_observed_at
            FROM loyal_yield.vault_reserve_positions_current position
            GROUP BY position.vault_id
        ) observed_position ON observed_position.vault_id = v.id
        WHERE v.active = TRUE
          AND p.active = TRUE
          AND $1 = ANY(p.delegated_signers)
          AND p.route_modes && $2::TEXT[]
          AND p.stable_mints && $3::TEXT[]
          AND p.kamino_liquidity_mints && $3::TEXT[]
          AND cardinality(p.kamino_markets) > 0
        -- A restart begins with never-observed/oldest-observed vaults instead
        -- of replaying low database ids forever. The cohort remains frozen
        -- and deterministic once this statement completes.
        ORDER BY observed_position.last_observed_at ASC NULLS FIRST,
        v.id
        "#,
    )
    .bind(STANDARD_POLICY_AUTHORITY)
    .bind(vec![
        SAME_MINT_ROUTE_MODE.to_owned(),
        FIXED_KAMINO_MAIN_ROUTE_MODE.to_owned(),
    ])
    .bind(enabled_mints)
    .fetch_all(pool)
    .await?;

    rows.into_iter()
        .map(|row| {
            Ok(FleetPositionSweepVault {
                vault: SelectedVault {
                    id: VaultId(row.try_get::<i64, _>("id")?),
                    settings: row.try_get("settings")?,
                    authority: row.try_get("authority")?,
                    policy_seed: row.try_get("policy_seed")?,
                    vault_index: row.try_get("vault_index")?,
                    vault_pubkey: row.try_get("vault_pubkey")?,
                    policy_account: row.try_get("policy_account")?,
                    setup_policy_account: row.try_get("setup_policy_account")?,
                    setup_policy_seed: row.try_get("setup_policy_seed")?,
                    delegated_signers: row.try_get("delegated_signers")?,
                    threshold: row.try_get("threshold")?,
                    route_modes: row.try_get("route_modes")?,
                    stable_mints: row.try_get("stable_mints")?,
                    kamino_markets: row.try_get("kamino_markets")?,
                    kamino_liquidity_mints: row.try_get("kamino_liquidity_mints")?,
                    swap_lanes: row.try_get("swap_lanes")?,
                },
            })
        })
        .collect::<Result<Vec<_>, loyal_yield_orchestrator::sqlx::Error>>()
}

async fn load_fleet_position_sweep_universe(
    runtime: &SameMintRouteRuntime,
    cluster: &str,
    enabled_mints: &[String],
) -> Result<FleetPositionSweepUniverse, FleetPositionSweepInitError> {
    let head = runtime
        .client
        .shared_market_catalog_head(cluster)
        .await
        .map_err(FleetPositionSweepInitError::transport)?
        .ok_or_else(|| {
            FleetPositionSweepInitError::invariant(
                "fleet position sweep requires a durable shared-market catalog head",
            )
        })?;
    if head.readiness_state != SharedMarketCatalogReadiness::Active
        || head.active_generation.is_none()
        || head.active_generation != head.target_generation
    {
        return Err(FleetPositionSweepInitError::invariant(
            "fleet position sweep requires the exact active shared-market catalog generation",
        ));
    }
    let expected_enabled_mints_hash =
        enabled_stable_mints_hash(enabled_mints).map_err(FleetPositionSweepInitError::invariant)?;
    if head.enabled_mints_hash != expected_enabled_mints_hash {
        return Err(FleetPositionSweepInitError::invariant(
            "fleet position sweep enabled mints do not match the active shared-market catalog",
        ));
    }

    let mut reserve_pubkeys = Vec::new();
    let mut reserve_addresses = BTreeSet::new();
    for address in &head.addresses {
        if address.semantic_class != LookupTableManifestSubject::SharedMarket {
            return Err(FleetPositionSweepInitError::invariant(
                "shared-market catalog head contains a non-shared semantic row",
            ));
        }
        let role_parts = address.account_role.split(',').collect::<Vec<_>>();
        let roles = role_parts.iter().copied().collect::<BTreeSet<_>>();
        if role_parts.is_empty()
            || role_parts
                .iter()
                .any(|role| role.is_empty() || role.trim() != *role)
            || roles.len() != role_parts.len()
        {
            return Err(FleetPositionSweepInitError::invariant(
                "shared-market catalog contains malformed account roles",
            ));
        }
        if !roles.contains("reserve") {
            continue;
        }
        if !address.is_writable || !reserve_addresses.insert(address.address.clone()) {
            return Err(FleetPositionSweepInitError::invariant(
                "shared-market catalog reserve roles must be unique and writable",
            ));
        }
        reserve_pubkeys.push(Pubkey::from_str(&address.address).map_err(|_| {
            FleetPositionSweepInitError::invariant(
                "shared-market catalog reserve role contains an invalid public key",
            )
        })?);
    }
    if reserve_pubkeys.is_empty() {
        return Err(FleetPositionSweepInitError::invariant(
            "active shared-market catalog contains no reserve-role addresses",
        ));
    }

    let mut evidence = FleetRpcAccountReadEvidence::default();
    let summaries =
        load_cached_reserve_summaries(runtime, &reserve_pubkeys, None, None, &mut evidence)
            .await
            .map_err(|error| FleetPositionSweepInitError::transport(error.to_string()))?;
    let mut reserves = reserve_pubkeys
        .into_iter()
        .map(|reserve| {
            let summary = summaries.get(&reserve).ok_or_else(|| {
                FleetPositionSweepInitError::invariant(
                    "validated reserve summary batch omitted a catalog reserve",
                )
            })?;
            Ok(FleetPositionSweepReserve {
                reserve: reserve.to_string(),
                market: summary.0.market.to_string(),
                liquidity_mint: summary.0.liquidity_mint.to_string(),
            })
        })
        .collect::<Result<Vec<_>, FleetPositionSweepInitError>>()?;
    reserves.sort_by(|left, right| left.reserve.cmp(&right.reserve));

    Ok(FleetPositionSweepUniverse {
        cluster: cluster.to_owned(),
        enabled_mints: enabled_mints.iter().cloned().collect(),
        catalog_revision_id: head.catalog_revision_id,
        catalog_source_slot: head.source_slot,
        reserves,
    })
}

async fn reconcile_fleet_position_sweep_batch(
    runtime: Arc<SameMintRouteRuntime>,
    batch: Vec<FleetPositionSweepVault>,
    universe: Arc<FleetPositionSweepUniverse>,
    sweep_id: u64,
    started_at: DateTime<Utc>,
    concurrency: usize,
) -> Vec<FleetPositionSweepTaskResult> {
    let mut tasks = JoinSet::new();
    let mut pending = batch.into_iter();
    let mut results = Vec::new();
    loop {
        while tasks.len() < concurrency {
            let Some(entry) = pending.next() else {
                break;
            };
            let vault_id = entry.vault.id;
            let task_runtime = runtime.clone();
            let task_universe = universe.clone();
            let runtime_handle = tokio::runtime::Handle::current();
            tasks.spawn_blocking(move || {
                let attempt = catch_unwind(AssertUnwindSafe(|| {
                    runtime_handle.block_on(reconcile_fleet_position_sweep_vault(
                        task_runtime.as_ref(),
                        &entry,
                        task_universe.as_ref(),
                        sweep_id,
                        started_at,
                    ))
                }));
                let outcome = match attempt {
                    Ok(Ok(outcome)) => outcome,
                    Ok(Err(error)) => FleetPositionSweepTaskOutcome::Failed(error),
                    // A panic is a defect in the refresh path itself, not
                    // upstream unavailability, so it stays independently loud.
                    Err(_) => FleetPositionSweepTaskOutcome::Failed(
                        FleetPositionSweepVaultError::invariant(
                            "position sweep task panicked before its monotonic write",
                        ),
                    ),
                };
                FleetPositionSweepTaskResult {
                    vault_id: Some(vault_id),
                    outcome,
                }
            });
        }
        let Some(result) = tasks.join_next().await else {
            break;
        };
        match result {
            Ok(result) => results.push(result),
            Err(error) => results.push(FleetPositionSweepTaskResult {
                vault_id: None,
                outcome: FleetPositionSweepTaskOutcome::Failed(
                    FleetPositionSweepVaultError::invariant(error.to_string()),
                ),
            }),
        }
    }
    results
}

async fn reconcile_fleet_position_sweep_vault(
    runtime: &SameMintRouteRuntime,
    entry: &FleetPositionSweepVault,
    universe: &FleetPositionSweepUniverse,
    sweep_id: u64,
    started_at: DateTime<Utc>,
) -> Result<FleetPositionSweepTaskOutcome, FleetPositionSweepVaultError> {
    // The ordered cohort is frozen for deterministic completion, but policy
    // eligibility is re-read immediately before RPC work so a mid-sweep
    // deactivate or policy replacement cannot be projected with stale rules.
    // Every guard below that re-checks a cohort-selection predicate reports
    // Superseded rather than a failure: the vault matched the predicate when the
    // cohort was frozen, so a mismatch now means the row changed underneath this
    // sweep. Guards that check conditions the cohort query never constrained are
    // genuine invariants.
    let vault = load_active_vault(
        &runtime.pool,
        &entry.vault.settings,
        entry.vault.vault_index,
    )
    .await
    .map_err(|error| FleetPositionSweepVaultError::from_sqlx(&error))?;
    let Some(vault) = vault else {
        return Ok(FleetPositionSweepTaskOutcome::Superseded(
            "sweep vault is no longer active under an active policy".to_owned(),
        ));
    };
    if vault.id != entry.vault.id {
        return Ok(FleetPositionSweepTaskOutcome::Superseded(
            "active sweep vault identity changed during the full sweep".to_owned(),
        ));
    }
    // The sweep tracks optimizer-managed and fixed-main Kamino vaults. Only the
    // former enters fleet opportunity planning; the latter is observed solely
    // so its current reserve and idle balances remain authoritative for AUM.
    if !vault
        .route_modes
        .iter()
        .any(|mode| mode == SAME_MINT_ROUTE_MODE || mode == FIXED_KAMINO_MAIN_ROUTE_MODE)
    {
        return Ok(FleetPositionSweepTaskOutcome::Superseded(
            "active policy is no longer in a tracked Kamino route mode".to_owned(),
        ));
    }
    if !vault
        .delegated_signers
        .iter()
        .any(|signer| signer == STANDARD_POLICY_AUTHORITY)
    {
        return Ok(FleetPositionSweepTaskOutcome::Superseded(
            "active policy no longer contains the standard delegated signer".to_owned(),
        ));
    }
    if !vault
        .stable_mints
        .iter()
        .any(|mint| universe.enabled_mints.contains(mint))
        || !vault
            .kamino_liquidity_mints
            .iter()
            .any(|mint| universe.enabled_mints.contains(mint))
    {
        return Ok(FleetPositionSweepTaskOutcome::Superseded(
            "active policy is no longer in the enabled stable-mint cohort".to_owned(),
        ));
    }
    // Cohort predicate `cardinality(p.kamino_markets) > 0`. Checked before the
    // reserve intersection below so a policy that lost its markets mid-sweep is
    // separated from a policy whose markets have no shared-catalog reserve role,
    // which is a genuine invariant that the same empty result would otherwise
    // hide.
    if vault.kamino_markets.is_empty() {
        return Ok(FleetPositionSweepTaskOutcome::Superseded(
            "active policy no longer declares a Kamino market".to_owned(),
        ));
    }
    let reserves = universe
        .reserves
        .iter()
        .filter(|reserve| {
            vault.kamino_markets.contains(&reserve.market)
                && vault.stable_mints.contains(&reserve.liquidity_mint)
                && vault
                    .kamino_liquidity_mints
                    .contains(&reserve.liquidity_mint)
        })
        .map(|reserve| reserve.reserve.clone())
        .collect::<Vec<_>>();
    if reserves.is_empty() {
        return Err(FleetPositionSweepVaultError::invariant(
            "active policy has no role-validated shared-catalog reserve",
        ));
    }
    let current_reserves = runtime
        .client
        .current_positions(vault.id)
        .await
        .map_err(|error| FleetPositionSweepVaultError::from_orchestrator(&error))?
        .into_iter()
        .filter(|position| position.has_value || position.amount_raw > 0)
        .map(|position| position.reserve)
        .collect::<BTreeSet<_>>();
    if current_reserves
        .iter()
        .any(|reserve| !reserves.contains(reserve))
    {
        return Err(FleetPositionSweepVaultError::invariant(
            "a held current reserve is outside the active policy's role-validated stable universe",
        ));
    }

    let preview =
        load_chain_reconcile_preview_from_runtime(runtime, &vault, &reserves, None, None, false)
            .await
            .map_err(|error| FleetPositionSweepVaultError::from_chain_read(error.as_ref()))?;
    let universe_by_reserve = universe
        .reserves
        .iter()
        .map(|reserve| (reserve.reserve.as_str(), reserve))
        .collect::<BTreeMap<_, _>>();
    for position in &preview.positions {
        let reserve = universe_by_reserve
            .get(position.reserve.as_str())
            .ok_or_else(|| {
                FleetPositionSweepVaultError::invariant(
                    "chain obligation references a reserve without an active shared-catalog reserve role",
                )
            })?;
        if position.market != reserve.market
            || position.liquidity_mint != reserve.liquidity_mint
            || !vault.kamino_markets.contains(&position.market)
            || !vault.stable_mints.contains(&position.liquidity_mint)
            || !vault
                .kamino_liquidity_mints
                .contains(&position.liquidity_mint)
        {
            return Err(FleetPositionSweepVaultError::invariant(
                "chain position identity falls outside the active policy's stable reserve universe",
            ));
        }
    }

    let mut state = chain_preview_reconciled_state(&preview)
        .map_err(|error| FleetPositionSweepVaultError::invariant(error.to_string()))?;
    state.context = json!({
        "kind": "fleet_position_sweep",
        "cluster": universe.cluster,
        "sweep_id": sweep_id,
        "sweep_started_at": started_at,
        "catalog_revision_id": universe.catalog_revision_id,
        "catalog_source_slot": universe.catalog_source_slot,
        "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
        "signer_loaded": false,
        "transactions_sent": false,
    });
    match runtime.client.reconcile_vault(vault.id, state).await {
        Ok(_) => {
            let position = preview.positions.first().ok_or_else(|| {
                FleetPositionSweepVaultError::invariant(
                    "position sweep produced no tracked Kamino reserve",
                )
            })?;
            runtime
                .client
                .record_current_idle_token_balance(CurrentIdleTokenBalance {
                    vault_id: vault.id,
                    mint: position.liquidity_mint.clone(),
                    amount_raw: i64::try_from(position.vault_liquidity_amount_raw).map_err(
                        |_| {
                            FleetPositionSweepVaultError::invariant(
                                "idle vault balance does not fit Postgres BIGINT",
                            )
                        },
                    )?,
                    owner: vault.vault_pubkey.clone(),
                    token_account: position.vault_liquidity_ata.clone(),
                    observed_slot: preview.observed_slot,
                    observed_at: Utc::now(),
                    source_commitment: "finalized".to_owned(),
                    updated_at: Utc::now(),
                })
                .await
                .map_err(|error| FleetPositionSweepVaultError::from_orchestrator(&error))?;
            Ok(FleetPositionSweepTaskOutcome::Refreshed)
        }
        Err(OrchestratorError::StaleVaultObservation { .. }) => {
            Ok(FleetPositionSweepTaskOutcome::Stale)
        }
        Err(error) => Err(FleetPositionSweepVaultError::from_orchestrator(&error)),
    }
}

async fn reconcile_signed_route_submission(
    runtime: Arc<SameMintRouteRuntime>,
    lease: SignedRouteSubmissionLease,
) -> Result<bool, String> {
    if matches!(
        lease.submission.state,
        SignedRouteSubmissionState::ExpiryCheckPending
            | SignedRouteSubmissionState::EffectAmbiguous
    ) {
        let recovering_ambiguous =
            lease.submission.state == SignedRouteSubmissionState::EffectAmbiguous;
        let result = inspect_expired_route(&runtime, &lease).await;
        return match result {
            Ok(ExpiredRouteCheckOutcome::EffectAbsent { observed_slot }) => {
                let observed_block_height = lease
                    .submission
                    .expiry_observed_block_height
                    .ok_or_else(|| {
                        "expiry-check submission is missing observed block height".to_owned()
                    })?;
                runtime
                    .client
                    .advance_signed_route_submission(
                        &lease,
                        SignedRouteSubmissionAdvance::Expired {
                            checked_at: Utc::now(),
                            observed_block_height,
                            signature_history_absent: true,
                            effect_absence_proved: true,
                        },
                    )
                    .await
                    .map_err(|error| error.to_string())?;
                println!(
                    "{}",
                    json!({
                        "status": "fleet_expired_route_effect_absent",
                        "submissionId": lease.submission.id,
                        "effectCheckSlot": observed_slot,
                    })
                );
                Ok(true)
            }
            Ok(ExpiredRouteCheckOutcome::Confirmed { slot }) => {
                runtime
                    .client
                    .advance_signed_route_submission(
                        &lease,
                        SignedRouteSubmissionAdvance::Confirmed {
                            checked_at: Utc::now(),
                            confirmed_slot: slot,
                        },
                    )
                    .await
                    .map_err(|error| error.to_string())?;
                runtime
                    .client
                    .advance_signed_route_submission(
                        &lease,
                        SignedRouteSubmissionAdvance::ReconciliationPending,
                    )
                    .await
                    .map_err(|error| error.to_string())?;
                println!(
                    "{}",
                    json!({
                        "status": "fleet_expired_route_late_confirmation",
                        "submissionId": lease.submission.id,
                        "confirmedSlot": slot,
                    })
                );
                Ok(true)
            }
            Ok(ExpiredRouteCheckOutcome::ConfirmedFailure { slot, detail }) => runtime
                .client
                .advance_signed_route_submission(
                    &lease,
                    SignedRouteSubmissionAdvance::Failed {
                        checked_at: Utc::now(),
                        confirmed_slot: Some(slot),
                        error_detail: detail,
                    },
                )
                .await
                .map(|_| true)
                .map_err(|error| error.to_string()),
            Ok(ExpiredRouteCheckOutcome::SeenUnconfirmed { detail }) => runtime
                .client
                .advance_signed_route_submission(
                    &lease,
                    SignedRouteSubmissionAdvance::Deferred {
                        checked_at: Utc::now(),
                        next_poll_at: Utc::now() + ChronoDuration::seconds(2),
                        error_detail: Some(detail),
                    },
                )
                .await
                .map(|_| false)
                .map_err(|error| error.to_string()),
            Ok(ExpiredRouteCheckOutcome::EffectAmbiguous { detail }) => {
                if recovering_ambiguous {
                    runtime
                        .client
                        .advance_signed_route_submission(
                            &lease,
                            SignedRouteSubmissionAdvance::Deferred {
                                checked_at: Utc::now(),
                                next_poll_at: Utc::now() + ChronoDuration::seconds(30),
                                error_detail: Some(detail),
                            },
                        )
                        .await
                        .map(|_| false)
                        .map_err(|error| error.to_string())
                } else {
                    runtime
                        .client
                        .advance_signed_route_submission(
                            &lease,
                            SignedRouteSubmissionAdvance::EffectAmbiguous {
                                checked_at: Utc::now(),
                                error_detail: detail.clone(),
                            },
                        )
                        .await
                        .map_err(|error| error.to_string())?;
                    println!(
                        "{}",
                        json!({
                            "status": "fleet_expired_route_effect_quarantined",
                            "submissionId": lease.submission.id,
                            "sharedLaneReleased": true,
                            "vaultConflictRetained": true,
                            "automaticRecoverySeconds": 30,
                            "reason": detail,
                        })
                    );
                    Ok(true)
                }
            }
            Err(error) => {
                let detail = safe_same_mint_operational_error(error.as_ref());
                runtime
                    .client
                    .advance_signed_route_submission(
                        &lease,
                        SignedRouteSubmissionAdvance::Deferred {
                            checked_at: Utc::now(),
                            next_poll_at: deferred_reconciliation_poll_at(
                                &lease,
                                "expiry_effect_check",
                                &detail,
                            ),
                            error_detail: Some(detail.clone()),
                        },
                    )
                    .await
                    .map_err(|advance_error| {
                        format!("expiry effect-check defer failed after {detail}: {advance_error}")
                    })?;
                Ok(false)
            }
        };
    }
    let result = reconcile_same_mint_submission_effect(&runtime, &lease).await;
    match result {
        Ok(reconciled_slot) => runtime
            .client
            .advance_signed_route_submission(
                &lease,
                SignedRouteSubmissionAdvance::Reconciled { reconciled_slot },
            )
            .await
            .map(|_| true)
            .map_err(|error| error.to_string()),
        Err(error) => {
            let detail = safe_same_mint_operational_error(error.as_ref());
            runtime
                .client
                .advance_signed_route_submission(
                    &lease,
                    SignedRouteSubmissionAdvance::Deferred {
                        checked_at: Utc::now(),
                        next_poll_at: deferred_reconciliation_poll_at(
                            &lease,
                            "post_effect_reconciliation",
                            &detail,
                        ),
                        error_detail: Some(detail.clone()),
                    },
                )
                .await
                .map_err(|advance_error| {
                    format!("reconciliation defer failed after {detail}: {advance_error}")
                })?;
            Ok(false)
        }
    }
}

/// Submissions already reported as stalled by this worker.
static RECONCILIATION_STALLS: ReconciliationStallLatch = ReconciliationStallLatch::new();

/// Schedules the next attempt for a submission that could not reach a terminal
/// state, and reports a submission whose failure has outlived every transient
/// explanation. Reconciliation never gives up on a confirmed money movement, so
/// the backoff is what stops a permanently failing predicate from polling chain
/// state once a second forever.
fn deferred_reconciliation_poll_at(
    lease: &SignedRouteSubmissionLease,
    lane: &str,
    detail: &str,
) -> DateTime<Utc> {
    let attempt_count = lease.submission.confirmation_attempt_count;
    let delay_seconds = reconciliation_retry_delay_seconds(attempt_count);
    if reconciliation_is_stalled(attempt_count) && RECONCILIATION_STALLS.claim(lease.submission.id)
    {
        OperationalError::new(
            "fleet_reconciliation_stalled",
            "reconcile_fleet_rebalance_submission",
            "fleet route submission reconciliation has not reached a terminal state",
        )
        .retryable(true)
        .recovery_required(true)
        .emit();
        eprintln!(
            "{}",
            json!({
                "status": "fleet_reconciliation_stalled",
                "lane": lane,
                "submissionId": lease.submission.id,
                "decisionId": lease.submission.decision_id.map(DecisionId::as_i64),
                "submissionState": lease.submission.state.as_str(),
                "signature": lease.submission.transaction_signature,
                "attemptCount": attempt_count,
                "retryDelaySeconds": delay_seconds,
                "reason": detail,
            })
        );
    }
    Utc::now() + ChronoDuration::seconds(delay_seconds)
}

enum ExpiredRouteCheckOutcome {
    EffectAbsent { observed_slot: i64 },
    Confirmed { slot: i64 },
    ConfirmedFailure { slot: i64, detail: String },
    SeenUnconfirmed { detail: String },
    EffectAmbiguous { detail: String },
}

async fn inspect_expired_route(
    runtime: &SameMintRouteRuntime,
    lease: &SignedRouteSubmissionLease,
) -> Result<ExpiredRouteCheckOutcome, Box<dyn Error>> {
    let submission = &lease.submission;
    if !matches!(
        submission.state,
        SignedRouteSubmissionState::ExpiryCheckPending
            | SignedRouteSubmissionState::EffectAmbiguous
    ) {
        return Err("effect-absence verifier received a non-recovery submission".into());
    }
    let effect_check_slot = submission
        .effect_check_slot
        .ok_or("expiry-check submission is missing its finalized slot")?;
    let signature = Signature::from_str(&submission.transaction_signature)
        .map_err(|_| "expiry-check submission has an invalid signature")?;
    let statuses = runtime
        .rpc
        .get_signature_statuses_with_history(&[signature])?;
    if let Some(status) = statuses.value.into_iter().next().flatten() {
        let slot = i64::try_from(status.slot)?;
        if status.satisfies_commitment(CommitmentConfig::confirmed()) {
            return match status.err {
                Some(error) => Ok(ExpiredRouteCheckOutcome::ConfirmedFailure {
                    slot,
                    detail: safe_same_mint_operational_error(&format!(
                        "late_confirmed_transaction_error:{error:?}"
                    )),
                }),
                None => Ok(ExpiredRouteCheckOutcome::Confirmed { slot }),
            };
        }
        return Ok(ExpiredRouteCheckOutcome::SeenUnconfirmed {
            detail: safe_same_mint_operational_error(&format!(
                "late_signature_seen_below_confirmed_commitment_at_slot_{slot}:{:?}",
                status.err
            )),
        });
    }

    let opportunity = runtime
        .client
        .rebalance_opportunity(submission.opportunity_id)
        .await?
        .ok_or("expiry-check opportunity no longer exists")?;
    let vault = reconciliation_vault(runtime, &opportunity).await?;
    let minimum_slot = u64::try_from(effect_check_slot)?;
    let (source_kind, _) = validated_opportunity_route_source_contract(&opportunity)?;
    match source_kind {
        SameMintRouteSourceKind::ReservePosition => {
            let source_reserve = opportunity
                .source_reserve
                .as_deref()
                .ok_or("same-mint expiry check has no source reserve")?;
            let source_snapshot_id = opportunity
                .source_snapshot_id
                .ok_or("same-mint expiry check has no source snapshot")?;
            let rows = sqlx::query(
                r#"
                SELECT reserve, liquidity_mint, amount_raw
                FROM loyal_yield.vault_position_snapshot_positions
                WHERE snapshot_id = $1 AND reserve IN ($2, $3)
                "#,
            )
            .bind(source_snapshot_id.as_i64())
            .bind(source_reserve)
            .bind(&opportunity.target_reserve)
            .fetch_all(&runtime.pool)
            .await?;
            let pre_source = rows
                .iter()
                .find(|row| {
                    row.try_get::<String, _>("reserve").ok().as_deref() == Some(source_reserve)
                })
                .ok_or("source snapshot is missing the expiry-check source reserve")?;
            let pre_source_amount: i64 = pre_source.try_get("amount_raw")?;
            let pre_source_mint: String = pre_source.try_get("liquidity_mint")?;
            let pre_target_amount = rows
                .iter()
                .find(|row| {
                    row.try_get::<String, _>("reserve").ok().as_deref()
                        == Some(opportunity.target_reserve.as_str())
                })
                .map(|row| row.try_get::<i64, _>("amount_raw"))
                .transpose()?
                .unwrap_or_default();
            let preview = load_chain_reconcile_preview_from_runtime(
                runtime,
                &vault,
                &[
                    source_reserve.to_owned(),
                    opportunity.target_reserve.clone(),
                ],
                Some(minimum_slot),
                Some(opportunity.optimizer_epoch_id),
                false,
            )
            .await?;
            let source = chain_position_for_reserve(&preview, source_reserve)?;
            let target = chain_position_for_reserve(&preview, &opportunity.target_reserve)?;
            if preview.observed_slot < effect_check_slot
                || source.liquidity_mint != pre_source_mint
                || target.liquidity_mint != opportunity.liquidity_mint
                || i64::try_from(source.amount_raw)? != pre_source_amount
                || i64::try_from(target.amount_raw)? != pre_target_amount
            {
                return Ok(ExpiredRouteCheckOutcome::EffectAmbiguous {
                    detail: "expired_same_mint_route_effect_present_or_chain_state_ambiguous"
                        .to_owned(),
                });
            }
            Ok(ExpiredRouteCheckOutcome::EffectAbsent {
                observed_slot: preview.observed_slot,
            })
        }
        SameMintRouteSourceKind::IdleVaultUsdc => {
            let idle_token_account = Pubkey::from_str(&required_plan_string(
                &opportunity.execution_plan,
                "idle_token_account",
            )?)?;
            let liquidity_mint = Pubkey::from_str(&opportunity.liquidity_mint)?;
            let (idle_amount, idle_account_exists) = load_spl_token_account_amount_at_or_after(
                runtime.rpc.as_ref(),
                &idle_token_account,
                &liquidity_mint,
                Some(minimum_slot),
            )?;
            if !idle_account_exists || i64::try_from(idle_amount)? != opportunity.amount_raw {
                return Ok(ExpiredRouteCheckOutcome::EffectAmbiguous {
                    detail: "expired_idle_deposit_effect_present_or_chain_state_ambiguous"
                        .to_owned(),
                });
            }
            Ok(ExpiredRouteCheckOutcome::EffectAbsent {
                observed_slot: effect_check_slot,
            })
        }
    }
}

async fn reconcile_same_mint_submission_effect(
    runtime: &SameMintRouteRuntime,
    lease: &SignedRouteSubmissionLease,
) -> Result<i64, Box<dyn Error>> {
    let submission = &lease.submission;
    if submission.state != SignedRouteSubmissionState::ReconciliationPending {
        return Err("reconciler received a submission outside reconciliation_pending".into());
    }
    let confirmed_slot = submission
        .confirmed_slot
        .ok_or("reconciliation_pending submission is missing confirmed_slot")?;
    let decision_id = submission
        .decision_id
        .ok_or("reconciliation_pending submission is missing decision_id")?;
    let decision_state = load_decision_reconciliation_state(&runtime.pool, decision_id).await?;
    if decision_state.status == DecisionStatus::Confirmed {
        if decision_state.signature.as_deref() != Some(&submission.transaction_signature)
            || decision_state.confirmed_slot != Some(confirmed_slot)
            || decision_state.post_snapshot_id.is_none()
        {
            return Err("confirmed decision does not match its durable signed submission".into());
        }
        return Ok(decision_state.reconciled_slot.unwrap_or(confirmed_slot));
    }
    if decision_state.status != DecisionStatus::Confirming {
        return Err(format!(
            "decision {} is {}, expected confirming",
            decision_id.as_i64(),
            decision_state.status.as_str()
        )
        .into());
    }
    let opportunity = runtime
        .client
        .rebalance_opportunity(submission.opportunity_id)
        .await?
        .ok_or("signed submission opportunity no longer exists")?;
    if opportunity.decision_id != Some(decision_id) {
        return Err("signed submission, opportunity, and decision identities diverged".into());
    }
    match validated_opportunity_route_source_contract(&opportunity)?.0 {
        SameMintRouteSourceKind::ReservePosition => {
            reconcile_reserve_submission_effect(
                runtime,
                submission,
                decision_id,
                confirmed_slot,
                &opportunity,
            )
            .await
        }
        SameMintRouteSourceKind::IdleVaultUsdc => {
            reconcile_idle_submission_effect(runtime, decision_id, confirmed_slot, &opportunity)
                .await
        }
    }
}

async fn reconciliation_vault(
    runtime: &SameMintRouteRuntime,
    opportunity: &RebalanceOpportunityRecord,
) -> Result<SelectedVault, Box<dyn Error>> {
    let settings = required_plan_string(&opportunity.execution_plan, "settings")?;
    let vault_index = i16::try_from(required_plan_i64(
        &opportunity.execution_plan,
        "vault_index",
    )?)?;
    let vault = load_active_vault(&runtime.pool, &settings, vault_index)
        .await?
        .ok_or("reconciliation vault is no longer active")?;
    validate_vault_policy(&vault)?;
    if vault.id != opportunity.vault_id {
        return Err("reconciliation vault does not match opportunity vault".into());
    }
    Ok(vault)
}

async fn reconcile_reserve_submission_effect(
    runtime: &SameMintRouteRuntime,
    submission: &loyal_yield_orchestrator::fleet_orchestration::SignedRouteSubmissionRecord,
    decision_id: DecisionId,
    confirmed_slot: i64,
    opportunity: &RebalanceOpportunityRecord,
) -> Result<i64, Box<dyn Error>> {
    let decision =
        load_prepared_same_mint_decision(&runtime.pool, decision_id, DecisionStatus::Confirming)
            .await?;
    if opportunity.vault_id != decision.vault_id
        || opportunity.source_reserve.as_deref() != Some(&decision.source_reserve)
        || opportunity.target_reserve != decision.target_reserve
    {
        return Err("reserve opportunity and decision identities diverged".into());
    }
    let vault = reconciliation_vault(runtime, opportunity).await?;

    let current = runtime.client.current_positions(decision.vault_id).await?;
    if let Some((snapshot_id, observed_slot)) =
        post_effect_current_snapshot(&decision, &current, confirmed_slot)?
    {
        runtime
            .client
            .confirm_same_mint_rebalance(ConfirmSameMintRebalanceInput {
                decision_id,
                signature: submission.transaction_signature.clone(),
                submitted_slot: submission.submitted_slot,
                confirmed_slot,
                observed_at: Some(Utc::now()),
                post_snapshot_id: Some(snapshot_id),
            })
            .await?;
        return Ok(observed_slot);
    }

    let current_slot = current
        .iter()
        .map(|position| position.observed_slot)
        .max()
        .unwrap_or_default();
    let minimum_slot = confirmed_slot.max(current_slot.saturating_add(1));
    let post_preview = load_chain_reconcile_preview_from_runtime(
        runtime,
        &vault,
        &[
            decision.source_reserve.clone(),
            decision.target_reserve.clone(),
        ],
        Some(u64::try_from(minimum_slot)?),
        Some(opportunity.optimizer_epoch_id),
        false,
    )
    .await?;
    let post_state = chain_preview_reconciled_state(&post_preview)?;
    ensure_post_confirm_chain_reconcile_state(&decision, &post_state)?;
    let snapshot = runtime
        .client
        .reconcile_vault(decision.vault_id, post_state)
        .await?;
    runtime
        .client
        .confirm_same_mint_rebalance(ConfirmSameMintRebalanceInput {
            decision_id,
            signature: submission.transaction_signature.clone(),
            submitted_slot: submission.submitted_slot,
            confirmed_slot,
            observed_at: Some(Utc::now()),
            post_snapshot_id: Some(snapshot.id),
        })
        .await?;
    Ok(snapshot.observed_slot)
}

async fn reconcile_idle_submission_effect(
    runtime: &SameMintRouteRuntime,
    decision_id: DecisionId,
    confirmed_slot: i64,
    opportunity: &RebalanceOpportunityRecord,
) -> Result<i64, Box<dyn Error>> {
    if opportunity.source_reserve.is_some() {
        return Err("idle opportunity unexpectedly carries a source reserve".into());
    }
    let vault = reconciliation_vault(runtime, opportunity).await?;
    let idle_token_account =
        required_plan_string(&opportunity.execution_plan, "idle_token_account")?;
    let contract = IdleDepositRouteContract {
        confirmed_slot,
        liquidity_mint: &opportunity.liquidity_mint,
        idle_token_account: &idle_token_account,
        deposited_amount_raw: opportunity.amount_raw,
        baseline_idle_amount_raw: optional_plan_i64(
            &opportunity.execution_plan,
            "idle_vault_liquidity_amount_raw",
        ),
    };
    let current = runtime.client.current_positions(vault.id).await?;
    let current_idle = runtime
        .client
        .current_idle_token_balance(vault.id, &opportunity.liquidity_mint)
        .await?;
    // The projections usually observe the deposit before the reconciler runs.
    // Closing against them keeps the common path off the RPC preview entirely.
    if let (Some(target), Some(idle)) = (
        current
            .iter()
            .find(|position| position.reserve == opportunity.target_reserve),
        current_idle.as_ref(),
    ) {
        let projected = IdleDepositPostEffectObservation {
            observed_slot: target.observed_slot.min(idle.observed_slot),
            target_liquidity_mint: &target.liquidity_mint,
            vault_liquidity_ata: &idle.token_account,
            idle_amount_raw: idle.amount_raw,
        };
        if let IdleDepositPostEffectDecision::Reconcile(residual) =
            classify_idle_deposit_post_effect(contract, projected)
        {
            runtime
                .client
                .advance_decision(
                    decision_id,
                    DecisionAdvance::Confirm {
                        slot: Some(confirmed_slot),
                        post_snapshot_id: Some(target.snapshot_id),
                    },
                )
                .await?;
            println!(
                "{}",
                json!({
                    "status": "fleet_idle_deposit_reconciled",
                    "decisionId": decision_id.as_i64(),
                    "source": "current_state_projection",
                    "observedSlot": projected.observed_slot,
                    "idleResidualRaw": residual.idle_amount_raw,
                    "plannedResidualRaw": residual.planned_residual_raw,
                    "unexplainedIdleSurplusRaw": residual.unexplained_surplus_raw,
                })
            );
            return Ok(projected.observed_slot);
        }
    }

    let current_slot = current
        .iter()
        .map(|position| position.observed_slot)
        .chain(current_idle.iter().map(|balance| balance.observed_slot))
        .max()
        .unwrap_or_default();
    let minimum_slot = confirmed_slot.max(current_slot.saturating_add(1));
    let mut reserves = current
        .iter()
        .map(|position| position.reserve.clone())
        .collect::<Vec<_>>();
    push_unique_string(&mut reserves, opportunity.target_reserve.clone());
    let preview = load_chain_reconcile_preview_from_runtime(
        runtime,
        &vault,
        &reserves,
        Some(u64::try_from(minimum_slot)?),
        Some(opportunity.optimizer_epoch_id),
        false,
    )
    .await?;
    let target = chain_position_for_reserve(&preview, &opportunity.target_reserve)?;
    let observed = IdleDepositPostEffectObservation {
        observed_slot: preview.observed_slot,
        target_liquidity_mint: &target.liquidity_mint,
        vault_liquidity_ata: &target.vault_liquidity_ata,
        idle_amount_raw: i64::try_from(target.vault_liquidity_amount_raw)?,
    };
    let residual = match classify_idle_deposit_post_effect(contract, observed) {
        IdleDepositPostEffectDecision::Reconcile(residual) => residual,
        IdleDepositPostEffectDecision::ObservationPredatesConfirmation {
            observed_slot,
            confirmed_slot,
        } => {
            return Err(format!(
                "idle deposit chain preview slot {observed_slot} predates confirmed slot {confirmed_slot}"
            )
            .into())
        }
        IdleDepositPostEffectDecision::IdentityMismatch { field } => {
            return Err(format!(
                "idle deposit chain preview {} does not match the executed route",
                field.as_str()
            )
            .into())
        }
    };
    let snapshot = runtime
        .client
        .reconcile_vault(vault.id, chain_preview_reconciled_state(&preview)?)
        .await?;
    runtime
        .client
        .record_current_idle_token_balance(CurrentIdleTokenBalance {
            vault_id: vault.id,
            mint: opportunity.liquidity_mint.clone(),
            amount_raw: i64::try_from(target.vault_liquidity_amount_raw)?,
            owner: vault.vault_pubkey.clone(),
            token_account: idle_token_account,
            observed_slot: preview.observed_slot,
            observed_at: Utc::now(),
            source_commitment: "confirmed".to_owned(),
            updated_at: Utc::now(),
        })
        .await?;
    runtime
        .client
        .advance_decision(
            decision_id,
            DecisionAdvance::Confirm {
                slot: Some(confirmed_slot),
                post_snapshot_id: Some(snapshot.id),
            },
        )
        .await?;
    println!(
        "{}",
        json!({
            "status": "fleet_idle_deposit_reconciled",
            "decisionId": decision_id.as_i64(),
            "source": "chain_reconcile_preview",
            "observedSlot": preview.observed_slot,
            "idleResidualRaw": residual.idle_amount_raw,
            "plannedResidualRaw": residual.planned_residual_raw,
            "unexplainedIdleSurplusRaw": residual.unexplained_surplus_raw,
        })
    );
    Ok(snapshot.observed_slot)
}

struct DecisionReconciliationState {
    status: DecisionStatus,
    signature: Option<String>,
    confirmed_slot: Option<i64>,
    post_snapshot_id: Option<SnapshotId>,
    reconciled_slot: Option<i64>,
}

async fn load_decision_reconciliation_state(
    pool: &PgPool,
    decision_id: DecisionId,
) -> Result<DecisionReconciliationState, Box<dyn Error>> {
    let row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            decision.status::TEXT AS status,
            decision.signature,
            decision.confirmed_slot,
            decision.post_snapshot_id,
            snapshot.observed_slot AS reconciled_slot
        FROM loyal_yield.rebalance_decisions decision
        LEFT JOIN loyal_yield.vault_position_snapshots snapshot
          ON snapshot.id = decision.post_snapshot_id
        WHERE decision.id = $1
        "#,
    )
    .bind(decision_id.as_i64())
    .fetch_one(pool)
    .await?;
    let status_text: String = row.try_get("status")?;
    let status = DecisionStatus::parse(&status_text)
        .ok_or_else(|| format!("unknown decision status {status_text:?}"))?;
    Ok(DecisionReconciliationState {
        status,
        signature: row.try_get("signature")?,
        confirmed_slot: row.try_get("confirmed_slot")?,
        post_snapshot_id: row
            .try_get::<Option<i64>, _>("post_snapshot_id")?
            .map(SnapshotId),
        reconciled_slot: row.try_get("reconciled_slot")?,
    })
}

fn post_effect_current_snapshot(
    decision: &PreparedSameMintDecision,
    positions: &[loyal_yield_orchestrator::CurrentReservePosition],
    confirmed_slot: i64,
) -> Result<Option<(SnapshotId, i64)>, Box<dyn Error>> {
    let Some(source) = positions
        .iter()
        .find(|position| position.reserve == decision.source_reserve)
    else {
        return Ok(None);
    };
    let Some(target) = positions
        .iter()
        .find(|position| position.reserve == decision.target_reserve)
    else {
        return Ok(None);
    };
    if source.observed_slot < confirmed_slot
        || target.observed_slot < confirmed_slot
        || source.snapshot_id != target.snapshot_id
    {
        return Ok(None);
    }
    if source.liquidity_mint != decision.liquidity_mint
        || target.liquidity_mint != decision.liquidity_mint
    {
        return Err("post-confirm current positions changed liquidity mint".into());
    }
    if source.amount_raw != 0 || target.amount_raw <= 0 {
        return Ok(None);
    }
    Ok(Some((source.snapshot_id, source.observed_slot)))
}

fn same_mint_request_from_opportunity(
    lease: &RebalanceOpportunityLease,
    rpc_url: &str,
    claim_kind: RebalanceOpportunityClaimKind,
) -> Result<SameMintRouteExecutionRequest, Box<dyn Error>> {
    let opportunity = &lease.opportunity;
    let plan = &opportunity.execution_plan;
    let settings = required_plan_string(plan, "settings")?;
    let vault_index = i16::try_from(required_plan_i64(plan, "vault_index")?)?;
    let (source_kind, source_evidence) = validated_opportunity_route_source_contract(opportunity)?;
    // `source_observed_*` is generic planner evidence. It is idle-account
    // evidence only for an idle-vault source; reserve-position routes are
    // fenced by their immutable source snapshot instead.
    Ok(SameMintRouteExecutionRequest {
        mode: match claim_kind {
            RebalanceOpportunityClaimKind::Revalidate => SameMintRouteExecutionMode::Revalidate,
            RebalanceOpportunityClaimKind::Execute => SameMintRouteExecutionMode::Execute,
        },
        opportunity_id: opportunity.id,
        optimizer_epoch_id: opportunity.optimizer_epoch_id,
        optimizer_market_slot: required_plan_i64(plan, "optimizer_market_slot")?,
        lease_owner: lease.owner.clone(),
        fencing_token: lease.fencing_token,
        source_kind,
        settings,
        vault_index,
        source_reserve: opportunity.source_reserve.clone(),
        target_reserve: opportunity.target_reserve.clone(),
        expected_source_snapshot_id: opportunity.source_snapshot_id.map(SnapshotId::as_i64),
        expected_idle_token_account: source_evidence.expected_idle_token_account,
        expected_idle_observed_slot: source_evidence.expected_idle_observed_slot,
        expected_idle_observed_at: source_evidence.expected_idle_observed_at,
        expected_liquidity_mint: opportunity.liquidity_mint.clone(),
        expected_amount_raw: opportunity.amount_raw,
        expected_route_amount_semantics: required_plan_string(plan, "route_amount_semantics")?,
        expected_source_apy_bps: opportunity.source_apy_bps,
        expected_observed_target_apy_bps: required_plan_i64(plan, "observed_target_apy_bps")?,
        expected_target_apy_bps: opportunity.target_apy_bps,
        expected_edge_bps: opportunity.estimated_edge_bps,
        principal_usd_micros: opportunity.principal_usd_micros,
        confidence_ppm: u32::try_from(required_plan_i64(plan, "confidence_ppm")?)?,
        expected_service_millis: u64::try_from(required_plan_i64(
            plan,
            "expected_service_millis",
        )?)?,
        holding_horizon_seconds: u64::try_from(required_plan_i64(
            plan,
            "holding_horizon_seconds",
        )?)?,
        estimated_execution_cost_usd_micros: required_plan_i64(
            plan,
            "estimated_execution_cost_usd_micros",
        )?,
        expected_cost_lamports: opportunity.estimated_cost_lamports,
        expected_route_fee_payer: (claim_kind == RebalanceOpportunityClaimKind::Execute)
            .then(|| optional_plan_string(plan, "route_fee_payer"))
            .flatten(),
        cluster: opportunity.cluster.clone(),
        rpc_url: rpc_url.to_owned(),
    })
}

fn validated_opportunity_route_source_contract(
    opportunity: &RebalanceOpportunityRecord,
) -> Result<(SameMintRouteSourceKind, FleetRouteSourceEvidence), Box<dyn Error>> {
    let source_kind = validate_fleet_route_kind_binding(&opportunity.execution_plan)?;
    let source_evidence =
        project_fleet_route_source_evidence(source_kind, &opportunity.execution_plan)?;
    validate_fleet_route_source_evidence(
        source_kind,
        opportunity.source_reserve.as_deref(),
        opportunity.source_snapshot_id.map(SnapshotId::as_i64),
        &source_evidence,
    )?;
    Ok((source_kind, source_evidence))
}

fn required_plan_string(plan: &Value, field: &str) -> Result<String, Box<dyn Error>> {
    plan.get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.trim().is_empty())
        .map(ToOwned::to_owned)
        .ok_or_else(|| format!("opportunity execution_plan.{field} is required").into())
}

fn optional_plan_string(plan: &Value, field: &str) -> Option<String> {
    plan.get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.trim().is_empty())
        .map(ToOwned::to_owned)
}

fn required_plan_i64(plan: &Value, field: &str) -> Result<i64, Box<dyn Error>> {
    plan.get(field)
        .and_then(Value::as_i64)
        .ok_or_else(|| format!("opportunity execution_plan.{field} is required").into())
}

fn optional_plan_i64(plan: &Value, field: &str) -> Option<i64> {
    plan.get(field).and_then(Value::as_i64)
}

fn request_conflict_account_keys(lease: &RebalanceOpportunityLease) -> Result<Vec<String>, String> {
    let Some(values) = lease
        .opportunity
        .execution_plan
        .get("conflict_account_keys")
        .and_then(Value::as_array)
    else {
        return Ok(Vec::new());
    };
    let mut keys = values
        .iter()
        .map(|value| {
            value
                .as_str()
                .filter(|key| !key.trim().is_empty())
                .map(ToOwned::to_owned)
                .ok_or_else(|| "semantic conflict keys must be nonempty strings".to_owned())
        })
        .collect::<Result<Vec<_>, _>>()?;
    keys.sort();
    keys.dedup();
    Ok(keys)
}

fn fleet_worker_retry_result(
    lease: RebalanceOpportunityLease,
    request: Option<&SameMintRouteExecutionRequest>,
    error: String,
) -> FleetWorkerTaskResult {
    let reason = safe_same_mint_operational_error(&error);
    let outcome = request.map_or_else(
        || {
            let plan = &lease.opportunity.execution_plan;
            SameMintRouteExecutionOutcome {
                // Building the in-process request is pure and performs no RPC
                // or database work. A failure here is durable route-schema or
                // identity corruption, not a transient dependency outage; a
                // retry would hot-loop the same poison row every two seconds.
                state: SameMintRouteExecutionState::Terminal,
                opportunity_id: lease.opportunity.id,
                source_kind: if lease.opportunity.source_reserve.is_some() {
                    SameMintRouteSourceKind::ReservePosition
                } else {
                    SameMintRouteSourceKind::IdleVaultUsdc
                },
                settings: optional_plan_string(plan, "settings").unwrap_or_default(),
                vault_index: optional_plan_i64(plan, "vault_index")
                    .and_then(|value| i16::try_from(value).ok())
                    .unwrap_or_default(),
                source_reserve: lease.opportunity.source_reserve.clone(),
                target_reserve: lease.opportunity.target_reserve.clone(),
                writes_decision: false,
                sends_transactions: false,
                reason: Some(reason.clone()),
                route_fingerprint: lease.opportunity.route_fingerprint.clone(),
                requirements_fingerprint: lease.opportunity.requirements_fingerprint.clone(),
                provisioning_request_id: None,
                readiness_evidence: None,
                writable_account_keys: Vec::new(),
                conflict_account_keys: Vec::new(),
            }
        },
        |request| request.outcome(SameMintRouteExecutionState::Retry, Some(reason.clone())),
    );
    FleetWorkerTaskResult { lease, outcome }
}

async fn finish_fleet_worker_task(
    client: &NeonSqlClient,
    result: FleetWorkerTaskResult,
) -> Result<(), Box<dyn Error>> {
    let FleetWorkerTaskResult { lease, outcome } = result;
    if lease.claim_kind == RebalanceOpportunityClaimKind::Execute
        && matches!(
            outcome.state,
            SameMintRouteExecutionState::SubmissionQueued | SameMintRouteExecutionState::Executed
        )
    {
        let current = client
            .rebalance_opportunity(lease.opportunity.id)
            .await?
            .ok_or("executed opportunity disappeared")?;
        let lease_identity = FleetWorkerCompletionIdentity {
            opportunity_id: lease.opportunity.id,
            route_fingerprint: lease.opportunity.route_fingerprint.as_deref(),
            requirements_fingerprint: lease.opportunity.requirements_fingerprint.as_deref(),
        };
        let outcome_identity = FleetWorkerCompletionIdentity {
            opportunity_id: outcome.opportunity_id,
            route_fingerprint: outcome.route_fingerprint.as_deref(),
            requirements_fingerprint: outcome.requirements_fingerprint.as_deref(),
        };
        let current_identity = FleetWorkerCompletionIdentity {
            opportunity_id: current.id,
            route_fingerprint: current.route_fingerprint.as_deref(),
            requirements_fingerprint: current.requirements_fingerprint.as_deref(),
        };
        validate_fleet_worker_completion(
            lease_identity,
            outcome_identity,
            current_identity,
            current.state,
            current.decision_id.is_some(),
        )
        .map_err(|error| -> Box<dyn Error> { error.into() })?;
        return Ok(());
    }

    let (next_state, available_at, reason, provisioning_request_id) =
        match (lease.claim_kind, outcome.state) {
            (RebalanceOpportunityClaimKind::Revalidate, SameMintRouteExecutionState::Ready) => {
                (RebalanceOpportunityState::Ready, None, None, None)
            }
            (_, SameMintRouteExecutionState::WaitingAlt) => (
                RebalanceOpportunityState::WaitingAlt,
                None,
                outcome.reason.clone(),
                outcome.provisioning_request_id,
            ),
            (RebalanceOpportunityClaimKind::Revalidate, SameMintRouteExecutionState::Retry) => (
                RebalanceOpportunityState::Revalidate,
                Some(Utc::now() + ChronoDuration::seconds(2)),
                outcome.reason.clone(),
                None,
            ),
            (RebalanceOpportunityClaimKind::Execute, SameMintRouteExecutionState::Retry)
                if outcome.reason.as_deref().is_some_and(|reason| {
                    reason.contains("fee_payer_reselection_required")
                        || reason.contains("target capacity telemetry changed")
                }) =>
            {
                (
                    RebalanceOpportunityState::Revalidate,
                    Some(Utc::now() + ChronoDuration::milliseconds(250)),
                    outcome.reason.clone(),
                    None,
                )
            }
            (RebalanceOpportunityClaimKind::Execute, SameMintRouteExecutionState::Retry)
                if outcome
                    .reason
                    .as_deref()
                    .is_some_and(|reason| reason.contains("target capacity")) =>
            {
                (
                    RebalanceOpportunityState::Revalidate,
                    Some(Utc::now() + ChronoDuration::seconds(2)),
                    outcome.reason.clone(),
                    None,
                )
            }
            (RebalanceOpportunityClaimKind::Execute, SameMintRouteExecutionState::Retry) => (
                RebalanceOpportunityState::Ready,
                Some(Utc::now() + ChronoDuration::seconds(2)),
                outcome.reason.clone(),
                None,
            ),
            (_, SameMintRouteExecutionState::Stale) => (
                RebalanceOpportunityState::Stale,
                None,
                outcome.reason.clone(),
                None,
            ),
            (_, SameMintRouteExecutionState::Terminal) => (
                RebalanceOpportunityState::Failed,
                None,
                Some(
                    outcome
                        .reason
                        .clone()
                        .unwrap_or_else(|| "route preflight failed terminally".to_owned()),
                ),
                None,
            ),
            (RebalanceOpportunityClaimKind::Execute, SameMintRouteExecutionState::Ready) => (
                RebalanceOpportunityState::Ready,
                Some(Utc::now() + ChronoDuration::seconds(1)),
                Some("execute lane returned ready without execution".to_owned()),
                None,
            ),
            (RebalanceOpportunityClaimKind::Revalidate, SameMintRouteExecutionState::Executed) => {
                return Err("revalidation lane unexpectedly executed a route".into());
            }
            (
                RebalanceOpportunityClaimKind::Revalidate,
                SameMintRouteExecutionState::SubmissionQueued,
            ) => return Err("revalidation lane unexpectedly queued a signed route".into()),
            (
                RebalanceOpportunityClaimKind::Execute,
                SameMintRouteExecutionState::SubmissionQueued,
            ) => unreachable!("queued routes return after decision linkage validation"),
            (RebalanceOpportunityClaimKind::Execute, SameMintRouteExecutionState::Executed) => {
                unreachable!("executed queue routes return after decision linkage validation")
            }
        };
    let mut execution_plan = lease.opportunity.execution_plan.clone();
    let object = execution_plan
        .as_object_mut()
        .ok_or("opportunity execution plan is not an object")?;
    if !outcome.writable_account_keys.is_empty() {
        object.insert(
            "exact_writable_account_keys".to_owned(),
            json!(outcome.writable_account_keys),
        );
    }
    if !outcome.conflict_account_keys.is_empty() {
        object.insert(
            "conflict_account_keys".to_owned(),
            json!(outcome.conflict_account_keys),
        );
    }
    if let Some(readiness) = outcome.readiness_evidence.clone() {
        if let Some(fee_payer) = readiness.get("feePayer").and_then(Value::as_str) {
            object.insert("route_fee_payer".to_owned(), json!(fee_payer));
        }
        object.insert("alt_readiness".to_owned(), readiness);
    }
    client
        .advance_rebalance_opportunity(
            lease.opportunity.id,
            &lease,
            RebalanceOpportunityAdvance {
                next_state,
                available_at,
                decision_id: None,
                reason,
                route_fingerprint: outcome.route_fingerprint,
                requirements_fingerprint: outcome.requirements_fingerprint,
                execution_plan: Some(execution_plan),
                provisioning_request_id,
            },
        )
        .await?;
    Ok(())
}

async fn emit_fleet_worker_health(
    client: &NeonSqlClient,
    options: &FleetWorkerOptions,
    delegated_signer: &str,
    keypool_state: &str,
    mounted_fee_payer_pubkeys: &BTreeSet<String>,
    claimed: u64,
    completed: u64,
    failed: u64,
    lifetime_fenced: u64,
    outbox_acknowledged: u64,
    fused_execute_permits: u64,
    fused_execute_promotions: u64,
    wakeup_listener_connected: bool,
) -> Result<(), Box<dyn Error>> {
    let status = client.fleet_orchestration_status(&options.cluster).await?;
    let observed_at = Utc::now();
    let stage_health = fleet_stage_health_report(
        &status,
        options.poll_interval_milliseconds,
        FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS,
        observed_at,
    )
    .ok();
    let enabled_shards = client
        .enabled_route_fee_payer_shards(&options.cluster)
        .await;
    let authority_status = client
        .route_fee_payer_authority_status(&options.cluster, delegated_signer)
        .await;
    let enabled_shard_count = enabled_shards.as_ref().map_or(0, Vec::len);
    let database_authority_conflict_count = enabled_shards.as_ref().map_or(0, |shards| {
        shards
            .iter()
            .filter(|shard| !shard.database_authority_separation_passes)
            .count()
    });
    let policy_key_conflict_count = enabled_shards.as_ref().map_or(0, |shards| {
        shards
            .iter()
            .filter(|shard| shard.fee_payer == delegated_signer)
            .count()
    });
    let exact_mounted_shard_count = enabled_shards.as_ref().map_or(0, |shards| {
        shards
            .iter()
            .filter(|shard| mounted_fee_payer_pubkeys.contains(&shard.fee_payer))
            .count()
    });
    println!(
        "{}",
        serde_json::to_string(&json!({
            "status": "fleet_worker_healthy",
            "lane": options.claim_kind.as_str(),
            "cluster": options.cluster,
            "owner": options.owner,
            "policySigner": delegated_signer,
            "feePayerSharding": {
                "policyFallbackAvailable": true,
                "keypoolState": keypool_state,
                "mountedKeyCount": mounted_fee_payer_pubkeys.len(),
                "registryAvailable": enabled_shards.is_ok(),
                "enabledShardCount": enabled_shard_count,
                "exactMountedConfiguredShardCount": exact_mounted_shard_count,
                "databaseAltAuthorityConflictCount": database_authority_conflict_count,
                "policyKeyConflictCount": policy_key_conflict_count,
                "authoritySeparationPasses": database_authority_conflict_count == 0
                    && policy_key_conflict_count == 0
                    && authority_status.as_ref().is_ok_and(|status| status.policy_authority_and_payer_match()),
                "assignment": "ranked_rendezvous_cluster_vault_pubkey",
                "maximumCandidatesPerRoute": MAX_FEE_PAYER_SHARD_CANDIDATES,
                "eligibleRouteClass": "mature_queue_same_mint_only",
                "manifestBinding": "revalidate_payer_then_execute_exact",
                "authorityProof": {
                    "delegatedPolicySigner": delegated_signer,
                    "databaseProofAvailable": authority_status.is_ok(),
                    "reusableFamilyCount": authority_status.as_ref().ok().map(|status| status.reusable_family_count),
                    "reusableFamilyPolicyMismatchCount": authority_status.as_ref().ok().map(|status| status.reusable_family_policy_mismatch_count),
                    "reusableTableCount": authority_status.as_ref().ok().map(|status| status.reusable_table_count),
                    "reusableTablePolicyMismatchCount": authority_status.as_ref().ok().map(|status| status.reusable_table_policy_mismatch_count),
                    "policyIsReusableAltAuthorityAndPayer": authority_status.as_ref().is_ok_and(|status| status.policy_authority_and_payer_match()),
                    "setupFarmAndRentPayer": delegated_signer,
                    "feeOnlyShardHasPolicyAuthority": false,
                    "feeOnlyShardHasReusableAltAuthority": false,
                    "feeOnlyShardMayFundSetupFarmOrRent": false,
                },
            },
            "concurrency": options.concurrency,
            "fusedExecuteConcurrency": options.fused_execute_concurrency,
            "claimed": claimed,
            "completed": completed,
            "failed": failed,
            "lifetimeFenced": lifetime_fenced,
            "altWakeupsAcknowledged": outbox_acknowledged,
            "fusedExecutePermits": fused_execute_permits,
            "fusedExecutePromotions": fused_execute_promotions,
            "wakeupListenerConnected": wakeup_listener_connected,
            "durableRecoveryPollMilliseconds": options.poll_interval_milliseconds,
            "healthObservationIntervalMilliseconds": FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS,
            "queue": status,
            "stageHealth": stage_health,
            "observedAt": observed_at,
        }))?
    );
    Ok(())
}

/// Runs a queue-planned route entirely inside the current process. The
/// persistent worker supplies typed evidence directly; argv and child-process
/// stdout are not part of this contract.
pub async fn execute_same_mint_route_in_process(
    request: SameMintRouteExecutionRequest,
) -> SameMintRouteExecutionOutcome {
    let options = match request.as_cli_options() {
        Ok(options) => options,
        Err(reason) => {
            return request.outcome(SameMintRouteExecutionState::Terminal, Some(reason));
        }
    };
    let database_url = match env::var("NEON_DATABASE_URL") {
        Ok(value) => value,
        Err(_) => {
            return request.outcome(
                SameMintRouteExecutionState::Terminal,
                Some("NEON_DATABASE_URL must be set".to_owned()),
            )
        }
    };
    let client = match NeonSqlClient::connect(NeonSqlConfig::new(database_url)).await {
        Ok(value) => value,
        Err(error) => {
            return request.outcome(
                SameMintRouteExecutionState::Retry,
                Some(safe_same_mint_operational_error(&error)),
            )
        }
    };
    let runtime =
        match SameMintRouteRuntime::new(&options.rpc_url, &options.cluster, client, true).await {
            Ok(value) => value,
            Err(error) => {
                return request.outcome(
                    SameMintRouteExecutionState::Retry,
                    Some(safe_same_mint_operational_error(error.as_ref())),
                )
            }
        };
    execute_same_mint_route_with_runtime(request, &runtime, None).await
}

async fn execute_same_mint_route_with_runtime(
    request: SameMintRouteExecutionRequest,
    runtime: &SameMintRouteRuntime,
    fused_lease_state: Option<&FusedExecutionLeaseState>,
) -> SameMintRouteExecutionOutcome {
    let options = match request.as_cli_options() {
        Ok(options) => options,
        Err(reason) => {
            return request.outcome(SameMintRouteExecutionState::Terminal, Some(reason));
        }
    };
    match run_with_runtime(options, runtime, fused_lease_state).await {
        Ok(Some(result)) => request.outcome_from_run(result),
        Ok(None) => request.outcome(
            SameMintRouteExecutionState::Terminal,
            Some("same-mint in-process request completed outside the route boundary".to_owned()),
        ),
        Err(error) => {
            let reason = safe_same_mint_operational_error(error.as_ref());
            request.outcome(classify_in_process_execution_error(&reason), Some(reason))
        }
    }
}

fn classify_in_process_execution_error(reason: &str) -> SameMintRouteExecutionState {
    let reason = reason.to_ascii_lowercase();
    if reason.starts_with(CURRENT_MARKET_EPOCH_STALE_PREFIX) {
        SameMintRouteExecutionState::Stale
    } else if reason.contains("complete reusable alt coverage")
        || reason.contains("reusable alt coverage") && reason.contains("incomplete")
        || reason.contains("lookup-table coverage") && reason.contains("missing")
    {
        SameMintRouteExecutionState::WaitingAlt
    } else if [
        "active decision",
        "blockhash",
        "connection",
        "database",
        "deadlock",
        "fee_payer_reselection_required",
        "insufficient funds",
        "insufficient lamports",
        "lease",
        "rate limit",
        "route_funding_required",
        "rpc",
        "serialization",
        "stale rpc",
        "target capacity",
        "temporar",
        "timeout",
    ]
    .iter()
    .any(|retryable| reason.contains(retryable))
    {
        SameMintRouteExecutionState::Retry
    } else {
        SameMintRouteExecutionState::Terminal
    }
}

fn classify_route_resolution_blocker(reason: &str) -> SameMintRouteExecutionState {
    if reason.starts_with("route_funding_required:") {
        SameMintRouteExecutionState::Retry
    } else if reason.starts_with("route_simulation_failed:") {
        if reason.contains("simulation_rpc_failed:") {
            SameMintRouteExecutionState::Retry
        } else {
            SameMintRouteExecutionState::Terminal
        }
    } else {
        classify_in_process_execution_error(reason)
    }
}

/// Re-read the durable queue lease instead of trusting the lease object that
/// originally woke the worker. Queue identity is deliberately absent for the
/// legacy CLI/admin paths; a partially populated identity fails closed.
async fn require_current_opportunity_fence(
    client: &NeonSqlClient,
    options: &CliOptions,
    vault: &SelectedVault,
    expected_fingerprints: Option<(&str, &str)>,
) -> Result<Option<RebalanceOpportunityRecord>, Box<dyn Error>> {
    let (opportunity_id, owner, fencing_token) = match (
        options.opportunity_id,
        options.opportunity_lease_owner.as_deref(),
        options.opportunity_fencing_token,
    ) {
        (None, None, None) => return Ok(None),
        (Some(opportunity_id), Some(owner), Some(fencing_token)) => {
            (opportunity_id, owner, fencing_token)
        }
        _ => {
            return Err(
                "queue route execution has a partial opportunity lease identity and is fenced"
                    .into(),
            )
        }
    };
    let current = client
        .rebalance_opportunity(opportunity_id)
        .await?
        .ok_or_else(|| format!("rebalance opportunity {opportunity_id} no longer exists"))?;
    let claim_kind = if options.execute {
        RebalanceOpportunityClaimKind::Execute
    } else if options.prepare_only {
        RebalanceOpportunityClaimKind::Revalidate
    } else {
        return Err("queue opportunity identity is only valid for execute or revalidate".into());
    };
    let lease = RebalanceOpportunityLease {
        opportunity: current.clone(),
        claim_kind,
        owner: owner.to_owned(),
        fencing_token,
        expires_at: current
            .lease_expires_at
            .ok_or("rebalance opportunity is missing its lease expiry")?,
    };
    let current = client.validate_rebalance_opportunity_lease(&lease).await?;
    if current.expires_at <= Utc::now() {
        return Err(format!("rebalance opportunity {opportunity_id} has expired").into());
    }

    let expected_target = options
        .idle_vault_deposit_reserve
        .as_deref()
        .or(options.target_reserve.as_deref())
        .ok_or("queue route is missing its target reserve")?;
    let expected_amount = options
        .expected_amount_raw
        .ok_or("queue route is missing its expected amount")?;
    let expected_mint = options
        .expected_liquidity_mint
        .as_deref()
        .ok_or("queue route is missing its expected liquidity mint")?;
    let expected_source_apy = options
        .expected_source_apy_bps
        .ok_or("queue route is missing its expected source APY")?;
    let expected_target_apy = options
        .expected_target_apy_bps
        .ok_or("queue route is missing its expected target APY")?;
    let expected_edge = options
        .expected_edge_bps
        .ok_or("queue route is missing its expected edge")?;
    let evidence_matches = current.cluster == options.cluster
        && current.vault_id == vault.id
        && current.source_snapshot_id.map(SnapshotId::as_i64)
            == options.expected_source_snapshot_id
        && current.source_reserve.as_deref() == options.source_reserve.as_deref()
        && current.target_reserve == expected_target
        && current.liquidity_mint == expected_mint
        && current.amount_raw == expected_amount
        && current.source_apy_bps == expected_source_apy
        && current.target_apy_bps == expected_target_apy
        && current.estimated_edge_bps == expected_edge;
    if !evidence_matches {
        return Err(format!(
            "rebalance opportunity {opportunity_id} evidence changed while leased; worker is fenced"
        )
        .into());
    }
    if let Some((route_fingerprint, requirements_fingerprint)) = expected_fingerprints {
        if current.route_fingerprint.as_deref() != Some(route_fingerprint)
            || current.requirements_fingerprint.as_deref() != Some(requirements_fingerprint)
        {
            return Err(format!(
                "rebalance opportunity {opportunity_id} exact route/requirements fingerprints changed while leased; worker is fenced"
            )
            .into());
        }
    }
    Ok(Some(current))
}

async fn run_with_options(
    options: CliOptions,
) -> Result<Option<InProcessRouteResult>, Box<dyn Error>> {
    let database_url =
        env::var("NEON_DATABASE_URL").map_err(|_| "NEON_DATABASE_URL must be set")?;
    let client = NeonSqlClient::from_pool(connect(&database_url).await?);
    let require_current_market = options.opportunity_id.is_some();
    let runtime = SameMintRouteRuntime::new(
        &options.rpc_url,
        &options.cluster,
        client,
        require_current_market,
    )
    .await?;
    run_with_runtime(options, &runtime, None).await
}

/// Queue work must not reuse its durable APYs as if they were a fresh market
/// observation. Read the same immutable Timescale snapshot shape as the
/// planner, preserve the planner's already-admitted capacity haircut, and
/// recompute the route edge from current source/target APYs. Legacy admin and
/// dry-run CLI paths have no opportunity identity and retain their existing
/// non-mutating behavior.
fn require_current_market_epoch_identity(
    optimizer_epoch_id: i64,
    fingerprint: &str,
    expires_at: DateTime<Utc>,
    expected_optimizer_epoch_id: i64,
) -> Result<(), Box<dyn Error>> {
    if optimizer_epoch_id != expected_optimizer_epoch_id {
        return Err(format!(
            "{CURRENT_MARKET_EPOCH_STALE_PREFIX} leased optimizer epoch {expected_optimizer_epoch_id} was superseded by current epoch {optimizer_epoch_id} ({fingerprint})"
        )
        .into());
    }
    require_market_epoch_lifetime(fingerprint, expires_at)
}

fn require_market_epoch_lifetime(
    fingerprint: &str,
    expires_at: DateTime<Utc>,
) -> Result<(), Box<dyn Error>> {
    let minimum_usable_until =
        Utc::now() + ChronoDuration::seconds(MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS);
    if expires_at < minimum_usable_until {
        return Err(format!(
            "{CURRENT_MARKET_EPOCH_STALE_PREFIX} market evidence {fingerprint} expires at {expires_at}, before minimum signing lifetime {minimum_usable_until}"
        )
        .into());
    }
    Ok(())
}

fn require_current_route_market_epoch(
    current: &CurrentRouteMarketEconomics,
    expected_optimizer_epoch_id: i64,
) -> Result<(), Box<dyn Error>> {
    require_current_market_epoch_identity(
        current.optimizer_epoch_id,
        &current.optimizer_epoch_fingerprint,
        current.optimizer_epoch_expires_at,
        expected_optimizer_epoch_id,
    )?;
    require_market_epoch_lifetime(
        &current.fresh_market_fingerprint,
        current.fresh_market_expires_at,
    )
}

async fn load_current_route_market_economics(
    runtime: &SameMintRouteRuntime,
    options: &CliOptions,
    vault: &SelectedVault,
    reserve_move: &ReserveMove,
) -> Result<Option<CurrentRouteMarketEconomics>, Box<dyn Error>> {
    if options.opportunity_id.is_none() {
        return Ok(None);
    }
    let liquidity_mint = options
        .expected_liquidity_mint
        .as_ref()
        .ok_or("queue route is missing its expected liquidity mint")?;
    let enabled_mints = enabled_stable_mints_from_env()?;
    if !enabled_mints.iter().any(|mint| mint == liquidity_mint) {
        return Err(format!(
            "{CURRENT_MARKET_EPOCH_STALE_PREFIX} route mint {liquidity_mint} is no longer in the planner's enabled universe"
        )
        .into());
    }
    let config = FleetObservationConfig {
        cluster: options.cluster.clone(),
        stablecoin_valuations: code_owned_stablecoin_valuations(&enabled_mints)?,
        enabled_mints,
        ..FleetObservationConfig::default()
    };
    let optimizer_epoch_id = options
        .optimizer_epoch_id
        .ok_or("queue route is missing its optimizer epoch")?;
    let bound_epoch = runtime
        .client
        .optimizer_epoch(&options.cluster, optimizer_epoch_id)
        .await?
        .ok_or_else(|| {
            format!(
                "{CURRENT_MARKET_EPOCH_STALE_PREFIX} bound optimizer epoch {optimizer_epoch_id} does not exist"
            )
        })?;
    let planned_epoch: ImmutableMarketEpoch = serde_json::from_value(bound_epoch.market_state.clone())
        .map_err(|error| {
            format!(
                "{CURRENT_MARKET_EPOCH_STALE_PREFIX} bound optimizer epoch {optimizer_epoch_id} has invalid market evidence: {error}"
            )
        })?;
    if bound_epoch.epoch_key != planned_epoch.fingerprint
        || bound_epoch.expires_at != planned_epoch.optimizer_envelope_expires_at()
    {
        return Err(format!(
            "{CURRENT_MARKET_EPOCH_STALE_PREFIX} bound optimizer epoch {optimizer_epoch_id} disagrees with its immutable market evidence"
        )
        .into());
    }
    require_current_market_epoch_identity(
        bound_epoch.id,
        &bound_epoch.epoch_key,
        bound_epoch.expires_at,
        optimizer_epoch_id,
    )?;
    let planned_mint_expires_at = planned_epoch
        .mint_expires_at(liquidity_mint)
        .ok_or_else(|| {
            format!(
                "{CURRENT_MARKET_EPOCH_STALE_PREFIX} bound optimizer epoch {optimizer_epoch_id} has no lifetime for route mint {liquidity_mint}"
            )
        })?;
    require_market_epoch_lifetime(&planned_epoch.fingerprint, planned_mint_expires_at)?;
    let topology_convergence_started = Instant::now();
    let (epoch, fresh_mint_expires_at, material_frontier_disposition) = loop {
        require_market_epoch_lifetime(&planned_epoch.fingerprint, planned_mint_expires_at)?;
        let epoch = runtime.current_market_epoch(&config).await?;
        let Some(fresh_mint_expires_at) = epoch.mint_expires_at(liquidity_mint) else {
            if topology_convergence_started.elapsed()
                < Duration::from_secs(CURRENT_MARKET_TOPOLOGY_CONVERGENCE_TIMEOUT_SECONDS)
            {
                tokio::time::sleep(Duration::from_millis(
                    CURRENT_MARKET_TOPOLOGY_CONVERGENCE_POLL_MILLISECONDS,
                ))
                .await;
                continue;
            }
            return Err(format!(
                "{CURRENT_MARKET_EPOCH_STALE_PREFIX} current market evidence has no lifetime for route mint {liquidity_mint} after bounded topology convergence"
            )
            .into());
        };
        require_market_epoch_lifetime(&epoch.fingerprint, fresh_mint_expires_at)?;
        let material_frontier_disposition = planned_epoch
            .material_market_frontier_for_mint(liquidity_mint)
            .disposition_against(&epoch.material_market_frontier_for_mint(liquidity_mint));
        if material_frontier_disposition.allows_current_route_revalidation() {
            break (epoch, fresh_mint_expires_at, material_frontier_disposition);
        }
        if material_frontier_disposition.requires_current_route_topology_convergence()
            && topology_convergence_started.elapsed()
                < Duration::from_secs(CURRENT_MARKET_TOPOLOGY_CONVERGENCE_TIMEOUT_SECONDS)
        {
            tokio::time::sleep(Duration::from_millis(
                CURRENT_MARKET_TOPOLOGY_CONVERGENCE_POLL_MILLISECONDS,
            ))
            .await;
            continue;
        }
        return Err(format!(
            "{CURRENT_MARKET_EPOCH_STALE_PREFIX} bound optimizer epoch {optimizer_epoch_id} has a materially superseded market frontier ({material_frontier_disposition:?}) after bounded topology convergence"
        )
        .into());
    };
    let target = epoch
        .reserves
        .iter()
        .find(|reserve| {
            reserve.reserve == reserve_move.target_reserve
                && reserve.liquidity_mint == *liquidity_mint
        })
        .ok_or_else(|| {
            format!(
                "{CURRENT_MARKET_EPOCH_STALE_PREFIX} current full-universe market epoch is missing target reserve {}",
                reserve_move.target_reserve
            )
        })?;
    if !target.target_eligible {
        return Err(format!(
            "{CURRENT_MARKET_EPOCH_STALE_PREFIX} current target reserve {} is no longer eligible",
            reserve_move.target_reserve
        )
        .into());
    }
    let source_apy_bps = if options.idle_vault_deposit_amount_raw.is_some() {
        0
    } else {
        epoch
            .reserves
            .iter()
            .find(|reserve| {
                reserve.reserve == reserve_move.source_reserve
                    && reserve.liquidity_mint == *liquidity_mint
            })
            .map(|reserve| reserve.supply_apy_bps)
            .ok_or_else(|| {
                format!(
                    "{CURRENT_MARKET_EPOCH_STALE_PREFIX} current full-universe market epoch is missing source reserve {}",
                    reserve_move.source_reserve
                )
            })?
    };
    let durable_observed_target_apy_bps = options
        .expected_observed_target_apy_bps
        .ok_or("queue route is missing its raw observed target APY")?;
    let durable_capacity_adjusted_target_apy_bps = options
        .expected_target_apy_bps
        .ok_or("queue route is missing its capacity-adjusted target APY")?;
    if durable_capacity_adjusted_target_apy_bps > durable_observed_target_apy_bps {
        return Err("queue route has invalid durable target-capacity evidence".into());
    }
    let opportunity_id = options
        .opportunity_id
        .ok_or("queue route is missing its opportunity identity")?;
    let principal_usd_micros = options
        .principal_usd_micros
        .ok_or("queue route is missing its normalized principal")?;
    let capacity_observation = TargetCapacityObservation {
        cluster: options.cluster.clone(),
        target_reserve: reserve_move.target_reserve.clone(),
        liquidity_mint: liquidity_mint.clone(),
        observed_supply_usd_micros: target.total_supply_usd_micros,
        observed_slot: target.slot,
        maximum_inflight_usd_micros: maximum_target_inflight_usd_micros(
            target.total_supply_usd_micros,
        ),
    };
    let capacity_projection = runtime
        .client
        .observe_target_capacity(capacity_observation)
        .await?;
    let projected_inflow_usd_micros = capacity_projection
        .committed_inflow_usd_micros
        .checked_add(principal_usd_micros)
        .ok_or("target capacity projection overflowed")?;
    if projected_inflow_usd_micros > capacity_projection.observation.maximum_inflight_usd_micros {
        return Err(format!(
            "current target capacity is exhausted: requested {}, committed {}, maximum {} USD micros",
            principal_usd_micros,
            capacity_projection.committed_inflow_usd_micros,
            capacity_projection.observation.maximum_inflight_usd_micros
        )
        .into());
    }
    let projected_target_apy_bps = projected_target_apy_bps(
        target.supply_apy_bps,
        target.total_supply_usd_micros,
        projected_inflow_usd_micros,
    )?;
    let economic_opportunity = OpportunityInput {
        opportunity_id,
        optimizer_epoch_id,
        vault_id: vault.id.as_i64(),
        tenant_id: vault.authority.clone(),
        source_snapshot_id: options
            .expected_source_snapshot_id
            .unwrap_or(opportunity_id)
            .max(1),
        observed_slot: options.optimizer_market_slot.unwrap_or(1).max(1),
        mint: liquidity_mint.clone(),
        source_reserve: if options.idle_vault_deposit_amount_raw.is_some() {
            "idle-vault-usdc".to_owned()
        } else {
            reserve_move.source_reserve.clone()
        },
        target_reserve: reserve_move.target_reserve.clone(),
        notional_usd_micros: principal_usd_micros,
        source_net_apy_bps: source_apy_bps,
        target_net_apy_bps: target.supply_apy_bps,
        confidence_ppm: options
            .confidence_ppm
            .ok_or("queue route is missing its confidence")?,
        expected_service_millis: options
            .expected_service_millis
            .ok_or("queue route is missing its expected service time")?,
        holding_horizon_seconds: options
            .holding_horizon_seconds
            .ok_or("queue route is missing its holding horizon")?,
        estimated_execution_cost_usd_micros: options
            .estimated_execution_cost_usd_micros
            .ok_or("queue route is missing its estimated execution cost")?,
        age_seconds: 0,
        fairness_credit: 0,
        writable_conflict_keys: Vec::new(),
    };
    let economic_policy = EconomicPolicy::default();
    let fee_policy = RouteFeePolicy::default();
    let economics = evaluate_fresh_route_economics(FreshRouteEconomicsInput {
        opportunity: economic_opportunity.clone(),
        // Validate current projected dilution through the same economic gate.
        // The original durable evidence was checked above; it is not reused as
        // if it described today's outstanding inflow.
        durable_observed_target_apy_bps: target.supply_apy_bps,
        durable_capacity_adjusted_target_apy_bps: projected_target_apy_bps,
        current_source_apy_bps: source_apy_bps,
        current_observed_target_apy_bps: target.supply_apy_bps,
        economic_policy: economic_policy.clone(),
        fee_policy,
    })
    .map_err(|reason| {
        format!(
            "current immutable market snapshot {} makes route economically ineligible: {reason:?}",
            epoch.fingerprint
        )
    })?;
    Ok(Some(CurrentRouteMarketEconomics {
        optimizer_epoch_id,
        optimizer_epoch_fingerprint: bound_epoch.epoch_key,
        optimizer_epoch_expires_at: bound_epoch.expires_at,
        fresh_market_fingerprint: epoch.fingerprint,
        fresh_market_expires_at: fresh_mint_expires_at,
        material_frontier_disposition: format!("{material_frontier_disposition:?}"),
        source_apy_bps,
        capacity_adjusted_target_apy_bps: economics.current_capacity_adjusted_target_apy_bps,
        edge_bps: economics.score.capacity_adjusted_net_edge_bps,
        fee_cap_lamports: economics.fee_budget.cap_lamports,
        capacity_reservation: TargetCapacityReservationInput {
            projection: capacity_projection,
            principal_usd_micros,
            economic_opportunity,
            current_observed_target_apy_bps: target.supply_apy_bps,
            economic_policy,
            fee_policy,
        },
    }))
}

async fn run_with_runtime(
    mut options: CliOptions,
    runtime: &SameMintRouteRuntime,
    fused_lease_state: Option<&FusedExecutionLeaseState>,
) -> Result<Option<InProcessRouteResult>, Box<dyn Error>> {
    let rpc = runtime.rpc.clone();
    let pool = runtime.pool.clone();
    let client = runtime.client.clone();
    let reserve_move = if let Some(reserve) = &options.idle_vault_deposit_reserve {
        ReserveMove {
            source_reserve: reserve.clone(),
            target_reserve: reserve.clone(),
        }
    } else {
        ReserveMove::from_options(&options)?
    };
    if let Some(amount_raw) = options.e2e_deposit_amount_raw {
        run_lifecycle_e2e_flow(&options, amount_raw)?;
        return Ok(None);
    }

    if options.update_policy {
        let default_authority = solana_testing_keypair_from_env()?.pubkey();
        let default_delegated_signer = policy_keypair_from_env()?.pubkey();
        let vault = if options.update_active_policy {
            match load_active_vault(&pool, &options.settings, options.vault_index).await? {
                Some(vault) => vault,
                None => load_policy_target_vault(
                    &pool,
                    &options.settings,
                    options.vault_index,
                    default_authority,
                    default_delegated_signer,
                )
                .await?
                .ok_or("no managed vault found for settings and vault index")?,
            }
        } else {
            load_policy_target_vault(
                &pool,
                &options.settings,
                options.vault_index,
                default_authority,
                default_delegated_signer,
            )
            .await?
            .ok_or("no managed vault found for settings and vault index")?
        };
        validate_vault_policy(&vault)?;
        run_policy_update_flow(&options, &client, &vault).await?;
        return Ok(None);
    }

    let vault = load_active_vault(&pool, &options.settings, options.vault_index)
        .await?
        .ok_or("no active managed vault found for settings and vault index")?;
    validate_vault_policy(&vault)?;
    let current_market =
        load_current_route_market_economics(runtime, &options, &vault, &reserve_move).await?;
    if let Some(current) = current_market.as_ref() {
        options.current_economic_fee_cap_lamports = Some(
            current
                .fee_cap_lamports
                .min(options.expected_cost_lamports.unwrap_or(i64::MAX)),
        );
    }
    let reconcile_reserves = reconcile_reserves_for_move(&options, &reserve_move);

    let requires_chain_preview = options.reconcile_from_chain
        || options.initial_deposit_amount_raw.is_some()
        || options.idle_vault_deposit_amount_raw.is_some()
        || options.full_withdraw_main_usdc
        || options.full_withdraw_reserve.is_some()
        || options.setup_obligation_reserve.is_some()
        || options.reconcile_current_positions;
    let optimizer_min_context_slot = options
        .optimizer_market_slot
        .map(u64::try_from)
        .transpose()?;
    let chain_preview = if requires_chain_preview {
        Some(
            load_chain_reconcile_preview_from_runtime(
                runtime,
                &vault,
                &reconcile_reserves,
                optimizer_min_context_slot,
                options.optimizer_epoch_id,
                true,
            )
            .await?,
        )
    } else {
        None
    };
    let policy_preflight = if let Some(preview) = &chain_preview {
        Some(load_policy_account_preflight_from_runtime(
            runtime,
            &vault,
            preview,
            &reserve_move,
            options.optimizer_epoch_id,
        )?)
    } else {
        None
    };
    if options.reconcile_current_positions {
        run_reconcile_current_positions_flow(
            &options,
            &client,
            &vault,
            chain_preview
                .as_ref()
                .ok_or("reconcile current positions requires chain preview")?,
        )
        .await?;
        return Ok(None);
    }
    if let Some(amount_raw) = options.idle_vault_deposit_amount_raw {
        let deposit_reserve = options
            .idle_vault_deposit_reserve
            .clone()
            .ok_or("idle vault deposit reserve is required")?;
        let result = run_idle_vault_deposit_flow(
            &mut options,
            &client,
            &vault,
            current_market.as_ref(),
            chain_preview
                .as_ref()
                .ok_or("idle vault deposit requires chain preview")?,
            policy_preflight.as_ref(),
            &deposit_reserve,
            amount_raw,
            fused_lease_state,
        )
        .await?;
        return Ok(result);
    }
    if let Some(amount_raw) = options.initial_deposit_amount_raw {
        let deposit_reserve = options
            .initial_deposit_reserve
            .clone()
            .unwrap_or_else(|| KAMINO_MAIN_USDC_RESERVE.to_string());
        run_initial_reserve_deposit_flow(
            &options,
            &client,
            &vault,
            chain_preview
                .as_ref()
                .ok_or("initial deposit requires chain preview")?,
            policy_preflight.as_ref(),
            &deposit_reserve,
            amount_raw,
        )
        .await?;
        return Ok(None);
    }
    if let Some(setup_reserve) = &options.setup_obligation_reserve {
        run_setup_obligation_flow(
            &options,
            &client,
            &vault,
            chain_preview
                .as_ref()
                .ok_or("setup obligation requires chain preview")?,
            setup_reserve,
            policy_preflight.as_ref(),
        )
        .await?;
        return Ok(None);
    }
    if options.full_withdraw_main_usdc || options.full_withdraw_reserve.is_some() {
        let withdraw_reserve = full_withdraw_reserve(&options);
        run_full_reserve_withdraw_flow(
            &options,
            &client,
            &vault,
            chain_preview
                .as_ref()
                .ok_or("full reserve withdraw requires chain preview")?,
            policy_preflight.as_ref(),
            &withdraw_reserve,
        )
        .await?;
        return Ok(None);
    }
    if options.route_runtime_active() {
        if let Some(reason) = execution_preflight_blocker(
            chain_preview.as_ref(),
            policy_preflight.as_ref(),
            &reserve_move,
            None,
        ) {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "execution_preflight_blocked",
                    "reason": reason.clone(),
                    "writesDecision": false,
                    "writesCurrentPositions": false,
                    "picksUpExecution": false,
                    "sendsTransactions": false,
                    "direction": options.direction.as_str(),
                    "vault": vault_json(&vault),
                    "requiredReserves": required_reserves_json(&reserve_move),
                    "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
                    "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
                    "targetObligationSetup": chain_preview.as_ref().and_then(|preview| target_obligation_setup_json(preview, &reserve_move, &vault, policy_preflight.as_ref())),
                    "missingObligationSetup": Value::Null,
                }))?
            );
            return Err(format!(
                "same-mint execution preflight blocked before DB writes: {reason}"
            )
            .into());
        }
    }
    let mut db_positions = load_position_summaries(&client, vault.id).await?;
    let user_position_seed = if options.seed_from_user_position {
        load_user_position_seed_preview(
            &pool,
            &vault,
            &reserve_move,
            chain_preview.as_ref(),
            options.direction,
        )
        .await?
    } else {
        None
    };
    let mut reconciled_snapshot_id = None;
    let should_write_current_positions_from_chain = writes_current_positions_from_chain(&options);
    let should_write_current_positions_from_user_seed =
        writes_current_positions_from_user_seed(&options);
    if should_write_current_positions_from_chain {
        let preview = chain_preview
            .as_ref()
            .ok_or("--execute requires --reconcile-from-chain")?;
        let state = chain_preview_reconciled_state(preview)?;
        let snapshot = client.reconcile_vault(vault.id, state).await?;
        reconciled_snapshot_id = Some(snapshot.id);
        db_positions = load_position_summaries(&client, vault.id).await?;
    } else if should_write_current_positions_from_user_seed {
        let seed = user_position_seed
            .as_ref()
            .ok_or("no active user_yield_positions row found for selected vault")?;
        let target_market = target_market_for_seed(
            seed,
            &reserve_move,
            chain_preview.as_ref(),
            options.direction,
        )?;
        let state = user_position_seed_reconciled_state(seed, &reserve_move, &target_market)?;
        let snapshot = client.reconcile_vault(vault.id, state).await?;
        reconciled_snapshot_id = Some(snapshot.id);
        db_positions = load_position_summaries(&client, vault.id).await?;
    }

    let using_chain_preview_positions =
        uses_chain_preview_positions(&options, chain_preview.is_some());
    let using_seed_preview_positions =
        !options.execute && user_position_seed.is_some() && !using_chain_preview_positions;
    let current_positions_source = if should_write_current_positions_from_chain {
        "vault_reserve_positions_current_after_chain_reconcile"
    } else if should_write_current_positions_from_user_seed {
        "vault_reserve_positions_current_after_user_position_seed"
    } else if using_chain_preview_positions {
        "chain_reconcile_preview"
    } else if using_seed_preview_positions {
        "user_yield_positions_seed_preview"
    } else {
        "neon_current_positions"
    };
    let pre_reconcile_positions = if using_chain_preview_positions {
        chain_preview
            .as_ref()
            .map(|preview| preview_position_summaries(preview, options.expected_source_snapshot_id))
            .unwrap_or_default()
    } else if using_seed_preview_positions {
        let seed = user_position_seed
            .as_ref()
            .expect("using seed preview implies seed exists");
        seed.positions.clone()
    } else {
        db_positions.clone()
    };
    let active_decision = load_active_decision(&pool, vault.id).await?;

    let pre_reconcile_input = match build_same_mint_input(
        &options,
        &reserve_move,
        vault.id,
        &pre_reconcile_positions,
        active_decision,
        current_market.as_ref(),
    ) {
        Ok(value) => value,
        Err(blocker) => {
            let blocker_error = format!(
                "same-mint execution prerequisite failed before DB command write: {blocker:?}"
            );
            let report = blocker_report(
                &options,
                &reserve_move,
                &vault,
                &db_positions,
                chain_preview.as_ref(),
                policy_preflight.as_ref(),
                user_position_seed.as_ref(),
                reconciled_snapshot_id,
                blocker,
            );
            println!("{}", serde_json::to_string_pretty(&report)?);
            if options.route_runtime_active() {
                return Err(blocker_error.into());
            }
            return Ok(None);
        }
    };
    let route_fee_payer = if let Some(preview) = chain_preview.as_ref() {
        Some(
            select_same_mint_route_fee_payer(runtime, &options, &vault, preview, &reserve_move)
                .await?,
        )
    } else {
        None
    };
    let (route_execution, route_build_error) = if let Some(preview) = &chain_preview {
        match build_route_execution_plan(
            Some(&rpc),
            &vault,
            preview,
            &reserve_move,
            &pre_reconcile_input,
            policy_preflight.as_ref(),
            route_fee_payer
                .as_ref()
                .expect("chain preview implies route fee payer"),
        ) {
            Ok(plan) => (Some(plan), None),
            Err(error) if !options.execute => (
                None,
                Some(safe_same_mint_operational_error_with_context(
                    "route_plan_build_failed",
                    error.as_ref(),
                )),
            ),
            Err(error) => return Err(error),
        }
    } else {
        (None, None)
    };
    let inline_missing_obligation_setup = route_execution
        .as_ref()
        .and_then(|plan| plan.preview.missing_obligation_setup.as_ref())
        .map(inline_missing_obligation_setup_json);
    let mut execution_preflight_blockers = execution_preflight_blockers(
        chain_preview.as_ref(),
        policy_preflight.as_ref(),
        &reserve_move,
        route_execution.as_ref(),
    );
    if let Some(error) = &route_build_error {
        execution_preflight_blockers.push(error.clone());
    }
    let mut route_lookup_table_resolution: Option<RuntimeLookupTableResolution> = None;
    let mut route_lookup_table_evidence: Option<Value> = None;
    let mut provisioning_request_id = None;
    if let Some(route_execution) = &route_execution {
        let mut transaction_instructions = route_execution.pre_instructions.clone();
        transaction_instructions.extend(route_execution.instructions.iter().cloned());
        if let Err(error) =
            guard_lookup_table_mutations(&transaction_instructions, "route execution")
        {
            execution_preflight_blockers.push(safe_same_mint_operational_error(&error));
        }
        let fee_payer = Pubkey::from_str(&route_execution.preview.fee_payer)?;
        let delegated_signer = policy_keypair_from_env()?;
        let fee_payer_signer = same_mint_route_fee_payer_from_env(&options, fee_payer)?;
        if fee_payer_signer.pubkey() != fee_payer {
            return Err(format!(
                "runtime lookup-table fee payer {} does not match prepared fee payer {}",
                fee_payer_signer.pubkey(),
                fee_payer
            )
            .into());
        }
        let transaction_signers = same_mint_route_signers(&fee_payer_signer, &delegated_signer);
        let lookup_table_scope = same_mint_route_lookup_table_scope_for_reserves(
            &vault,
            &reserve_move.source_reserve,
            &reserve_move.target_reserve,
        );
        if options.route_runtime_active() {
            require_current_opportunity_fence(&client, &options, &vault, None).await?;
        }
        if options.opportunity_id.is_some() {
            require_current_route_market_epoch(
                current_market
                    .as_ref()
                    .ok_or("queue route is missing its current full-universe market epoch")?,
                options
                    .optimizer_epoch_id
                    .ok_or("queue route is missing its optimizer epoch")?,
            )?;
        }
        let mut resolution = resolve_route_lookup_tables(
            &client,
            &rpc,
            &options,
            &vault,
            &reserve_move.source_reserve,
            &reserve_move.target_reserve,
            "same_mint_kamino",
            &lookup_table_scope,
            fee_payer,
            &transaction_instructions,
            &route_execution.lookup_table_manifest,
            &transaction_signers,
        )
        .await?;
        let serializes_policy_setup_funding =
            route_execution.preview.missing_obligation_setup.is_some()
                || route_execution.preview.source_farm_setup_required
                || route_execution.preview.target_farm_setup_required;
        apply_policy_setup_funding_serialization(
            &mut resolution,
            &route_execution.preview.signer,
            serializes_policy_setup_funding,
        );
        if let Some(fields) = resolution.evidence.as_object_mut() {
            fields.insert(
                "routeSteps".to_owned(),
                json!(&route_execution.preview.route_steps),
            );
            fields.insert(
                "missingObligationSetup".to_owned(),
                route_execution
                    .preview
                    .missing_obligation_setup
                    .as_ref()
                    .map(inline_missing_obligation_setup_json)
                    .unwrap_or(Value::Null),
            );
        }
        if options.route_runtime_active() {
            require_current_opportunity_fence(
                &client,
                &options,
                &vault,
                options.execute.then_some((
                    resolution.route_fingerprint.as_str(),
                    resolution.requirements_fingerprint.as_str(),
                )),
            )
            .await?;
            let acquire_route_lease = (options.execute || options.fused_execute)
                && resolution.blocker.is_none()
                && execution_preflight_blockers.is_empty();
            provisioning_request_id = persist_route_lookup_table_resolution(
                &client,
                &options,
                &vault,
                &reserve_move.source_reserve,
                &reserve_move.target_reserve,
                "same_mint_kamino",
                &route_execution.lookup_table_manifest,
                &resolution,
                acquire_route_lease,
                true,
            )
            .await?;
        }
        if let Some(blocker) = &resolution.blocker {
            execution_preflight_blockers.push(format!(
                "lookup-table resolver blocked route execution: {blocker}"
            ));
        }
        route_lookup_table_evidence = Some(resolution.evidence.clone());
        route_lookup_table_resolution = Some(resolution);
    }
    let execution_preflight_blocker_reason = execution_preflight_blockers.first().cloned();
    let would_execute_route =
        route_execution.is_some() && execution_preflight_blocker_reason.is_none();
    if options.route_runtime_active() {
        if let Some(reason) = &execution_preflight_blocker_reason {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "execution_preflight_blocked",
                    "reason": reason,
                    "writesDecision": false,
                    "writesCurrentPositions": options.reconcile_from_chain,
                    "picksUpExecution": false,
                    "sendsTransactions": false,
                    "direction": options.direction.as_str(),
                    "vault": vault_json(&vault),
                    "requiredReserves": required_reserves_json(&reserve_move),
                    "currentPositions": db_positions.iter().map(position_json).collect::<Vec<_>>(),
                    "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
                    "userPositionSeed": user_position_seed.as_ref().map(user_position_seed_preview_json),
                    "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
                    "sameMintInput": same_mint_input_json(&pre_reconcile_input),
                    "routeExecution": route_execution.as_ref().map(route_execution_preview_json),
                    "lookupTableResolution": route_lookup_table_evidence.clone(),
                    "targetObligationSetup": chain_preview.as_ref().and_then(|preview| target_obligation_setup_json(preview, &reserve_move, &vault, policy_preflight.as_ref())),
                    "missingObligationSetup": inline_missing_obligation_setup.clone(),
                }))?
            );
            return Ok(Some(in_process_route_result(
                classify_in_process_execution_error(reason),
                Some(reason.clone()),
                route_lookup_table_resolution.as_ref(),
                provisioning_request_id,
            )));
        }
    }

    if options.fused_execute && would_execute_route {
        let resolution = route_lookup_table_resolution
            .as_ref()
            .ok_or("fused same-mint route is missing exact lookup-table resolution")?;
        let current = require_current_opportunity_fence(&client, &options, &vault, None)
            .await?
            .ok_or("fused same-mint route is missing its durable opportunity")?;
        let lease_owner = current
            .lease_owner
            .clone()
            .ok_or("fused same-mint route is missing its revalidation lease owner")?;
        let lease_expires_at = current
            .lease_expires_at
            .ok_or("fused same-mint route is missing its revalidation lease expiry")?;
        let revalidation_lease = RebalanceOpportunityLease {
            opportunity: current.clone(),
            claim_kind: RebalanceOpportunityClaimKind::Revalidate,
            owner: lease_owner,
            fencing_token: current.fencing_token,
            expires_at: lease_expires_at,
        };
        let mut execution_plan = current.execution_plan.clone();
        let fields = execution_plan
            .as_object_mut()
            .ok_or("fused same-mint opportunity execution plan is not an object")?;
        fields.insert(
            "exact_writable_account_keys".to_owned(),
            json!(resolution.writable_account_keys),
        );
        fields.insert(
            "conflict_account_keys".to_owned(),
            json!(resolution.conflict_account_keys),
        );
        fields.insert(
            "route_fee_payer".to_owned(),
            json!(route_execution
                .as_ref()
                .map(|plan| plan.preview.fee_payer.as_str())),
        );
        fields.insert("alt_readiness".to_owned(), resolution.evidence.clone());

        let promotion = client
            .try_promote_revalidation_lease_to_execute(
                &revalidation_lease,
                &resolution.route_fingerprint,
                &resolution.requirements_fingerprint,
                &execution_plan,
                &resolution.conflict_account_keys,
            )
            .await;
        let promoted = match promotion {
            Ok(promoted) => promoted,
            Err(error) => {
                release_route_resolution_lease(&client, resolution).await;
                return Err(error.into());
            }
        };
        if let Some(promoted) = promoted {
            let state = fused_lease_state
                .ok_or("fused same-mint execution requires worker-owned promotion state")?;
            *state
                .lock()
                .map_err(|_| "fused same-mint promotion state lock was poisoned")? =
                Some(promoted.clone());
            options.execute = true;
            options.prepare_only = false;
            options.fused_execute = false;
            options.opportunity_fencing_token = Some(promoted.fencing_token);
        } else {
            // A semantic lock was not immediately available. Drop only the
            // short-lived ALT route-resolution lease and publish normal
            // durable `ready`; no prepared transaction crosses this boundary.
            release_route_resolution_lease(&client, resolution).await;
        }
    }

    if !options.execute {
        let report_status = if options.prepare_only {
            "ready"
        } else {
            "dry_run"
        };
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": report_status,
                "writesDecision": false,
                "persistsReadiness": options.prepare_only,
                "provisioningRequestId": provisioning_request_id,
                "wouldWriteDecision": execution_preflight_blocker_reason.is_none(),
                "wouldBuildRoute": route_execution.is_some(),
                "wouldExecuteRoute": would_execute_route,
                "executionPreflightBlocker": execution_preflight_blocker_reason,
                "executionPreflightBlockers": execution_preflight_blockers,
                "wouldReconcileCurrentPositions": options.reconcile_from_chain,
                "wouldSeedCurrentPositions": options.seed_from_user_position,
                "reconciledSnapshotId": reconciled_snapshot_id.map(SnapshotId::as_i64),
                "currentPositionsSource": current_positions_source,
                "direction": options.direction.as_str(),
                "vault": vault_json(&vault),
                "requiredReserves": required_reserves_json(&reserve_move),
                "currentPositions": db_positions.iter().map(position_json).collect::<Vec<_>>(),
                "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
                "userPositionSeed": user_position_seed.as_ref().map(user_position_seed_preview_json),
                "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
                "sameMintInput": same_mint_input_json(&pre_reconcile_input),
                "routeBuildError": route_build_error,
                "routeExecution": route_execution.as_ref().map(route_execution_preview_json),
                "lookupTableResolution": route_lookup_table_evidence,
                "targetObligationSetup": chain_preview.as_ref().and_then(|preview| target_obligation_setup_json(preview, &reserve_move, &vault, policy_preflight.as_ref())),
                "missingObligationSetup": inline_missing_obligation_setup.clone(),
                "executionPlan": {
                    "kind": "same_mint",
                    "routeSteps": route_execution.as_ref().map(|plan| plan.preview.route_steps.clone()).unwrap_or_else(|| vec![KAMINO_WITHDRAW_ROUTE_STEP, KAMINO_DEPOSIT_ROUTE_STEP]),
                    "policyExecutions": route_execution.as_ref().map(|plan| plan.preview.route_steps.len()).unwrap_or(1)
                }
            }))?
        );
        return if options.prepare_only {
            Ok(Some(in_process_route_result(
                SameMintRouteExecutionState::Ready,
                None,
                route_lookup_table_resolution.as_ref(),
                provisioning_request_id,
            )))
        } else {
            Ok(None)
        };
    }

    let executable_lookup_tables = route_lookup_table_resolution
        .as_ref()
        .ok_or("same-mint execute route is missing exact lookup-table resolution")?;
    let current_opportunity = require_current_opportunity_fence(
        &client,
        &options,
        &vault,
        Some((
            executable_lookup_tables.route_fingerprint.as_str(),
            executable_lookup_tables.requirements_fingerprint.as_str(),
        )),
    )
    .await?;
    if let Some(current) = current_opportunity {
        let current_market = current_market
            .as_ref()
            .ok_or("queue route is missing its target capacity reservation")?;
        require_current_route_market_epoch(current_market, current.optimizer_epoch_id)?;
        let executable_route = route_execution
            .as_ref()
            .ok_or("same-mint queue handoff is missing its exact route plan")?;
        let handoff = prepare_queue_signed_route_handoff(
            &client,
            Some(runtime),
            &options,
            current,
            current_market,
            executable_lookup_tables,
            executable_route.preview.fee_payer_selection.mature_route
                && executable_route.preview.missing_obligation_setup.is_none()
                && !executable_route.preview.source_farm_setup_required
                && !executable_route.preview.target_farm_setup_required,
            executable_route
                .preview
                .fee_payer_selection
                .observed_balance_lamports,
            executable_route
                .preview
                .fee_payer_selection
                .observed_balance_slot,
            executable_route
                .preview
                .fee_payer_selection
                .observed_balance_at,
        )
        .await?;
        let (prepared, submission) = client
            .prepare_same_mint_rebalance_with_signed_submission(
                pre_reconcile_input.clone(),
                &handoff.lease,
                current_market.capacity_reservation.clone(),
                handoff.submission,
            )
            .await?;
        let decision_id = prepared
            .decision_id
            .filter(|_| prepared.status == DecisionStatus::Planned)
            .ok_or("atomic same-mint fleet handoff did not create a planned decision")?;
        if submission.decision_id != Some(decision_id) {
            return Err("atomic same-mint fleet handoff returned an unlinked submission".into());
        }
        release_route_resolution_lease(&client, executable_lookup_tables).await;
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "submission_queued",
                "writesDecision": true,
                "persistsSignedBytes": true,
                "atomicSignedDecisionHandoff": true,
                "sendsTransactions": false,
                "opportunityId": options.opportunity_id,
                "decisionId": decision_id.as_i64(),
                "submissionId": submission.id,
                "signature": submission.transaction_signature,
            }))?
        );
        return Ok(Some(in_process_route_result(
            SameMintRouteExecutionState::SubmissionQueued,
            None,
            route_lookup_table_resolution.as_ref(),
            provisioning_request_id,
        )));
    }
    let prepared = client
        .prepare_same_mint_rebalance(pre_reconcile_input.clone())
        .await?;
    if prepared.status == DecisionStatus::Planned {
        let decision_id = prepared
            .decision_id
            .ok_or("planned same-mint rebalance result did not include decision id")?;
        let predecision_lookup_tables = route_lookup_table_resolution
            .as_ref()
            .ok_or("planned same-mint route is missing predecision lookup-table resolution")?;
        let execution_decision =
            match load_prepared_same_mint_decision(&pool, decision_id, DecisionStatus::Planned)
                .await
            {
                Ok(value) => value,
                Err(error) => {
                    let reason = same_mint_decision_failure_reason("decision_load_failed", &error);
                    let _ = client
                        .advance_decision(
                            decision_id,
                            DecisionAdvance::Fail {
                                reason: reason.clone(),
                            },
                        )
                        .await;
                    release_route_resolution_lease(&client, predecision_lookup_tables).await;
                    return Err(format!(
                        "same-mint route execution failed after decision {}: {reason}",
                        decision_id.as_i64()
                    )
                    .into());
                }
            };
        if let Err(error) = validate_execution_decision_route(&execution_decision, &reserve_move) {
            let reason =
                same_mint_decision_failure_reason("decision_route_validation_failed", &error);
            let _ = client
                .advance_decision(
                    decision_id,
                    DecisionAdvance::Fail {
                        reason: reason.clone(),
                    },
                )
                .await;
            release_route_resolution_lease(&client, predecision_lookup_tables).await;
            return Err(format!(
                "same-mint route execution failed after decision {}: {reason}",
                decision_id.as_i64()
            )
            .into());
        }
        let execution_input = same_mint_input_from_decision(&execution_decision);
        let chain_reconcile = chain_preview
            .as_ref()
            .ok_or("--execute requires --reconcile-from-chain route execution plan")?;
        let route_execution = match build_route_execution_plan(
            Some(&rpc),
            &vault,
            chain_reconcile,
            &reserve_move,
            &execution_input,
            policy_preflight.as_ref(),
            route_fee_payer
                .as_ref()
                .ok_or("route execution is missing its prepared fee payer")?,
        ) {
            Ok(value) => value,
            Err(error) => {
                let reason =
                    same_mint_decision_failure_reason("route_plan_build_failed", error.as_ref());
                let _ = client
                    .advance_decision(
                        decision_id,
                        DecisionAdvance::Fail {
                            reason: reason.clone(),
                        },
                    )
                    .await;
                release_route_resolution_lease(&client, predecision_lookup_tables).await;
                return Err(format!(
                    "same-mint route execution failed after decision {}: {reason}",
                    decision_id.as_i64()
                )
                .into());
            }
        };
        let execution = match execute_prepared_same_mint_route(
            &client,
            &options,
            &vault,
            &execution_decision,
            &route_execution,
            predecision_lookup_tables,
        )
        .await
        {
            Ok(value) => value,
            Err(error) => {
                let reason =
                    same_mint_decision_failure_reason("route_execution_failed", error.as_ref());
                let _ = client
                    .advance_decision(
                        decision_id,
                        DecisionAdvance::Fail {
                            reason: reason.clone(),
                        },
                    )
                    .await;
                return Err(format!(
                    "same-mint route execution failed after decision {}: {reason}",
                    decision_id.as_i64()
                )
                .into());
            }
        };
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "executed",
                "writesDecision": true,
                "picksUpExecution": true,
                "sendsTransactions": true,
                "wouldReconcileCurrentPositions": options.reconcile_from_chain,
                "wouldSeedCurrentPositions": options.seed_from_user_position,
                "reconciledSnapshotId": reconciled_snapshot_id.map(SnapshotId::as_i64),
                "currentPositionsSource": current_positions_source,
                "direction": options.direction.as_str(),
                "vault": vault_json(&vault),
                "requiredReserves": required_reserves_json(&reserve_move),
                "currentPositions": db_positions.iter().map(position_json).collect::<Vec<_>>(),
                "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
                "userPositionSeed": user_position_seed.as_ref().map(user_position_seed_preview_json),
                "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
                "sameMintInput": same_mint_input_json(&pre_reconcile_input),
                "preparedDecision": same_mint_result_json(&prepared),
                "executionDecision": prepared_same_mint_decision_json(&execution_decision),
                "routeExecution": route_execution_preview_json(&route_execution),
                "missingObligationSetup": route_execution.preview.missing_obligation_setup.as_ref().map(inline_missing_obligation_setup_json),
                "executionPickup": {
                    "decisionId": decision_id.as_i64(),
                    "source": "loyal_yield.rebalance_decisions",
                    "signature": execution.signature,
                    "submittedSlot": execution.submitted_slot,
                    "confirmedSlot": execution.confirmed_slot,
                    "simulationUnitsConsumed": execution.simulation_units_consumed,
                    "transaction": transaction_packet_json(&execution.transaction_packet),
                    "lookupTableResolution": execution.lookup_table_resolution,
                    "finalStatus": execution.confirmed.status.as_str(),
                },
                "confirmedDecision": same_mint_result_json(&execution.confirmed),
                "executionPlan": {
                    "kind": "same_mint",
                    "routeSteps": route_execution.preview.route_steps.clone(),
                    "policyExecutions": route_execution.preview.route_steps.len()
                }
            }))?
        );
        return Ok(Some(in_process_route_result(
            SameMintRouteExecutionState::Executed,
            None,
            route_lookup_table_resolution.as_ref(),
            provisioning_request_id,
        )));
    }

    if let Some(resolution) = route_lookup_table_resolution.as_ref() {
        release_route_resolution_lease(&client, resolution).await;
    }

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "prepare_same_mint_rebalance_did_not_plan",
            "writesDecision": prepared.decision_id.is_some(),
            "picksUpExecution": false,
            "sendsTransactions": false,
            "wouldReconcileCurrentPositions": options.reconcile_from_chain,
            "wouldSeedCurrentPositions": options.seed_from_user_position,
            "reconciledSnapshotId": reconciled_snapshot_id.map(SnapshotId::as_i64),
            "currentPositionsSource": current_positions_source,
            "direction": options.direction.as_str(),
            "vault": vault_json(&vault),
            "requiredReserves": required_reserves_json(&reserve_move),
            "currentPositions": db_positions.iter().map(position_json).collect::<Vec<_>>(),
            "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
            "userPositionSeed": user_position_seed.as_ref().map(user_position_seed_preview_json),
            "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
            "sameMintInput": same_mint_input_json(&pre_reconcile_input),
            "preparedDecision": same_mint_result_json(&prepared),
            "routeExecution": route_execution.as_ref().map(route_execution_preview_json),
            "missingObligationSetup": inline_missing_obligation_setup.clone(),
        }))?
    );
    Err("same-mint rebalance was not planned".into())
}

fn run_lifecycle_e2e_flow(options: &CliOptions, amount_raw: u64) -> Result<(), Box<dyn Error>> {
    let phase_specs = lifecycle_e2e_phase_specs(amount_raw);
    let mut phase_results = Vec::new();
    for spec in phase_specs {
        let phase = LifecyclePhaseCommand {
            name: spec.name,
            args: lifecycle_phase_args(options, &spec.args),
        };
        let result = run_lifecycle_phase(&phase, options)?;
        let success = result
            .get("process")
            .and_then(|process| process.get("success"))
            .and_then(Value::as_bool)
            .unwrap_or(false);
        phase_results.push(result);
        if options.execute && !success {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "lifecycle_e2e_phase_failed",
                    "writesDecision": options.execute,
                    "sendsTransactions": options.execute,
                    "execute": options.execute,
                    "settings": options.settings,
                    "vaultIndex": options.vault_index,
                    "depositAmountRaw": amount_raw.to_string(),
                    "phases": phase_results,
                }))?
            );
            return Err("same-mint lifecycle E2E phase failed".into());
        }
    }
    let all_phase_processes_succeeded = phase_results.iter().all(|result| {
        result
            .get("process")
            .and_then(|process| process.get("success"))
            .and_then(Value::as_bool)
            .unwrap_or(false)
    });

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": if options.execute { "lifecycle_e2e_executed" } else { "lifecycle_e2e_dry_run" },
            "writesDecision": options.execute,
            "sendsTransactions": options.execute,
            "execute": options.execute,
            "allPhaseProcessesSucceeded": all_phase_processes_succeeded,
            "settings": options.settings,
            "vaultIndex": options.vault_index,
            "depositAmountRaw": amount_raw.to_string(),
            "phaseOrder": [
                "policy_update",
                "initial_main_usdc_deposit",
                "move_main_to_prime",
                "move_prime_to_main",
                "full_main_usdc_withdraw"
            ],
            "phases": phase_results,
        }))?
    );
    Ok(())
}

fn validate_same_mint_rpc_genesis(cluster: &str, observed: Hash) -> Result<(), String> {
    validate_rpc_genesis_hash(cluster, observed)
        .map_err(|error| format!("refusing same-mint route work against mismatched RPC: {error}"))
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct LifecyclePhaseCommand {
    name: &'static str,
    args: Vec<String>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct LifecyclePhaseSpec {
    name: &'static str,
    args: Vec<String>,
}

fn lifecycle_e2e_phase_specs(amount_raw: u64) -> Vec<LifecyclePhaseSpec> {
    vec![
        LifecyclePhaseSpec {
            name: "policy_update",
            args: vec!["--update-policy".to_owned()],
        },
        LifecyclePhaseSpec {
            name: "initial_main_usdc_deposit",
            args: vec!["--deposit-main-usdc".to_owned(), amount_raw.to_string()],
        },
        LifecyclePhaseSpec {
            name: "move_main_to_prime",
            args: vec![
                "--direction".to_owned(),
                "main-to-prime".to_owned(),
                "--reconcile-from-chain".to_owned(),
            ],
        },
        LifecyclePhaseSpec {
            name: "move_prime_to_main",
            args: vec![
                "--direction".to_owned(),
                "prime-to-main".to_owned(),
                "--reconcile-from-chain".to_owned(),
            ],
        },
        LifecyclePhaseSpec {
            name: "full_main_usdc_withdraw",
            args: vec!["--full-withdraw-main-usdc".to_owned()],
        },
    ]
}

fn lifecycle_phase_args(options: &CliOptions, phase_args: &[String]) -> Vec<String> {
    let mut args = vec![
        "--settings".to_owned(),
        options.settings.clone(),
        "--vault-index".to_owned(),
        options.vault_index.to_string(),
        "--cluster".to_owned(),
        options.cluster.clone(),
    ];
    args.extend(phase_args.iter().cloned());
    if options.seed_from_user_position {
        args.push("--seed-from-user-position".to_owned());
    }
    if options.execute {
        args.push("--execute".to_owned());
    }
    args
}

fn run_lifecycle_phase(
    phase: &LifecyclePhaseCommand,
    options: &CliOptions,
) -> Result<Value, Box<dyn Error>> {
    let exe = env::current_exe()?;
    let output = Command::new(exe)
        .args(&phase.args)
        .env("SOLANA_RPC_URL", &options.rpc_url)
        .output()?;
    let stdout = String::from_utf8_lossy(&output.stdout).trim().to_owned();
    let stderr = String::from_utf8_lossy(&output.stderr).trim().to_owned();
    let parsed_stdout = if stdout.is_empty() {
        Value::Null
    } else {
        serde_json::from_str(&stdout).unwrap_or_else(|_| json!({ "raw": stdout }))
    };
    Ok(json!({
        "name": phase.name,
        "args": phase.args,
        "process": {
            "success": output.status.success(),
            "code": output.status.code(),
        },
        "stdout": parsed_stdout,
        "stderr": if stderr.is_empty() { Value::Null } else { json!(stderr) },
    }))
}

async fn run_policy_update_flow(
    options: &CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
) -> Result<(), Box<dyn Error>> {
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let settings = Pubkey::from_str(&vault.settings)?;
    let authority = Pubkey::from_str(&vault.authority)?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let policy = Pubkey::from_str(&vault.policy_account)?;
    let policy_seed = u64::try_from(vault.policy_seed).map_err(|_| "policy_seed must be >= 0")?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault_index {} must fit u8 for Squads account index",
            vault.vault_index
        )
    })?;
    if vault.threshold != 1 {
        return Err(format!(
            "policy update script only supports threshold 1, got {}",
            vault.threshold
        )
        .into());
    }

    let authority_signer = solana_testing_keypair_from_env()?;
    if authority_signer.pubkey() != authority {
        return Err(format!(
            "SOLANA_TESTING_PK pubkey {} does not match policy authority {}",
            authority_signer.pubkey(),
            authority
        )
        .into());
    }
    let policy_lookup_table_scope = format!(
        "same_mint_policy:{}:{}:{}",
        vault.settings, vault.vault_index, vault.policy_account
    );
    // Legacy exact-scope ALTs are deliberately absent from measurement. The
    // best-case packet estimator remains useful, while every actual send is
    // compiled by the reusable resolver below.
    let lookup_table_accounts = Vec::new();
    let delegated_signer = policy_keypair_from_env()?;
    let db_delegated_signer_matches = vault
        .delegated_signers
        .iter()
        .any(|signer| signer == &delegated_signer.pubkey().to_string());

    let final_universe = same_mint_usdc_policy_universe()?;
    let swap_lanes = Vec::new();
    let context = LoyalActionContext {
        settings,
        authority,
        delegated_signer: delegated_signer.pubkey(),
        account_index,
        vault: vault_pubkey,
    };

    let existing_policy_account =
        rpc.get_account_with_commitment(&policy, CommitmentConfig::confirmed())?;
    let policy_exists = if let Some(account) = existing_policy_account.value.as_ref() {
        if account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
            return Err(format!(
                "policy account {} is owned by {}, expected {}",
                policy, account.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID
            )
            .into());
        }
        true
    } else {
        false
    };

    let all_in_one_setup = if policy_exists {
        update_all_in_one_market_mint_yield_route_action(
            context,
            final_universe.clone(),
            swap_lanes.clone(),
            policy,
            account_index,
        )?
    } else {
        let setup = YieldRouteActionBuilder::new(context, final_universe.clone())
            .topology(RouteTopology::AllInOne)
            .swap_lanes(swap_lanes.clone())
            .seeds(YieldRouteActionSeeds {
                withdraw: policy_seed,
                ..YieldRouteActionSeeds::default()
            })
            .build()?;
        if setup.accounts.withdraw != policy {
            return Err(format!(
                "policy seed {} derives {}, but DB policy_account is {}",
                policy_seed, setup.accounts.withdraw, policy
            )
            .into());
        }
        setup
    };
    let existing_decoded = existing_policy_account
        .value
        .as_ref()
        .and_then(|account| decode_squads_policy_account(&account.data).ok());
    let all_in_one_instruction = all_in_one_setup
        .instructions
        .first()
        .ok_or(if policy_exists {
            "policy update did not produce an instruction"
        } else {
            "policy create did not produce an instruction"
        })?
        .clone();
    let all_in_one_transaction = build_policy_transaction(
        &rpc,
        authority_signer.pubkey(),
        all_in_one_instruction.clone(),
        &lookup_table_accounts,
        &authority_signer,
        if policy_exists {
            "policy all-in-one update measurement"
        } else {
            "policy all-in-one create measurement"
        },
        None,
    )?;
    let all_in_one_preview = policy_operation_preview_json(
        if policy_exists {
            "all_in_one_update_attempt"
        } else {
            "all_in_one_create_attempt"
        },
        vault,
        settings,
        policy,
        vault_pubkey,
        authority_signer.pubkey(),
        delegated_signer.pubkey(),
        db_delegated_signer_matches,
        &final_universe,
        &swap_lanes,
        &all_in_one_setup,
        &all_in_one_transaction,
        existing_decoded.as_ref(),
    )?;
    let all_in_one_best_case_fits = all_in_one_transaction
        .best_case_single_lookup_table_packet
        .as_ref()
        .map(|packet| packet.fits_packet_data_size)
        .unwrap_or(
            all_in_one_transaction
                .transaction_packet
                .fits_packet_data_size,
        );

    if all_in_one_best_case_fits {
        let policy_instructions = vec![all_in_one_instruction.clone()];
        let policy_manifest = policy_lookup_table_manifest(
            authority_signer.pubkey(),
            &policy_instructions,
            vault,
            &[&all_in_one_setup],
            &[policy],
        )?;
        let policy_lookup_table_phase = prepare_route_lookup_table_phase(
            client,
            &rpc,
            options,
            vault,
            "policy",
            "policy",
            "policy_update_all_in_one",
            policy_lookup_table_scope.clone(),
            authority_signer.pubkey(),
            policy_instructions,
            policy_manifest,
            &[&authority_signer],
            options.execute,
        )
        .await?;
        let lookup_table_provisioning = policy_lookup_table_phase.resolution.evidence.clone();
        let policy_transaction = build_policy_transaction(
            &rpc,
            authority_signer.pubkey(),
            all_in_one_instruction,
            &lookup_table_accounts,
            &authority_signer,
            if policy_exists {
                "policy update"
            } else {
                "policy create"
            },
            None,
        )?;
        let policy_preview = policy_operation_preview_json(
            if policy_exists { "update" } else { "create" },
            vault,
            settings,
            policy,
            vault_pubkey,
            authority_signer.pubkey(),
            delegated_signer.pubkey(),
            db_delegated_signer_matches,
            &final_universe,
            &swap_lanes,
            &all_in_one_setup,
            &policy_transaction,
            existing_decoded.as_ref(),
        )?;

        if !options.execute {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "policy_update_dry_run",
                    "writesDecision": false,
                    "sendsTransactions": false,
                    "fallbackRequired": false,
                    "lookupTableProvisioning": lookup_table_provisioning.clone(),
                    "policyAllInOneAttempt": all_in_one_preview,
                    "policyCreate": if policy_exists { None } else { Some(policy_preview.clone()) },
                    "policyUpdate": if policy_exists { Some(policy_preview.clone()) } else { None },
                    "policyFinalizeUpdate": Value::Null,
                }))?
            );
            return Ok(());
        }

        let submitted_policy = submit_route_lookup_table_phase(
            client,
            &rpc,
            options,
            vault,
            &policy_lookup_table_phase,
            &[&authority_signer],
            &format!("policy-update:{policy}"),
        )
        .await?;
        let submitted_slot = u64::try_from(submitted_policy.submitted_slot)?;
        let signature = submitted_policy.signature.clone();
        let confirmed_slot = u64::try_from(submitted_policy.confirmed_slot)?;
        let create_signature = if policy_exists {
            None
        } else {
            Some(signature.clone())
        };
        let create_submitted_slot = if policy_exists {
            None
        } else {
            Some(i64::try_from(submitted_slot)?)
        };
        let create_confirmed_slot = if policy_exists {
            None
        } else {
            Some(i64::try_from(confirmed_slot)?)
        };
        let policy_swap_lanes = policy_swap_lanes_json(&all_in_one_setup, &swap_lanes)?;
        let stored = client
            .record_policy_match(PolicyMatchInput {
                signature: signature.clone(),
                slot: confirmed_slot,
                settings: settings.to_string(),
                authority: authority.to_string(),
                policy_seed,
                policy_account: policy.to_string(),
                vault_index: account_index,
                vault_pubkey: vault_pubkey.to_string(),
                delegated_signers: vec![delegated_signer.pubkey().to_string()],
                threshold: 1,
                route_modes: vec![SAME_MINT_ROUTE_MODE.to_owned()],
                stable_mints: pubkeys_json(&final_universe.stable_mints),
                kamino_markets: pubkeys_json(&final_universe.kamino_markets),
                kamino_liquidity_mints: pubkeys_json(&final_universe.kamino_liquidity_mints),
                universe_preset: Some(KAMINO_STABLE_UNIVERSE_PRESET.to_owned()),
                risk_profile: Some(SAFE_RISK_PROFILE.to_owned()),
                swap_lanes: policy_swap_lanes.clone(),
            })
            .await?;
        let updated_account = rpc.get_account(&policy)?;
        let updated_decoded =
            decode_squads_policy_account(&updated_account.data).map_err(|error| {
                format!("failed to decode updated Squads policy account {policy}: {error}")
            })?;
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": if policy_exists { "policy_updated" } else { "policy_created" },
                "writesDecision": false,
                "sendsTransactions": true,
                "fallbackRequired": false,
                "lookupTableProvisioning": lookup_table_provisioning,
                "lookupTableResolution": submitted_policy.lookup_table_resolution,
                "signature": signature,
                "submittedSlot": i64::try_from(submitted_slot)?,
                "confirmedSlot": i64::try_from(confirmed_slot)?,
                "createSignature": create_signature,
                "createSubmittedSlot": create_submitted_slot,
                "createConfirmedSlot": create_confirmed_slot,
                "policyAllInOneAttempt": all_in_one_preview,
                "policyCreate": if policy_exists { None } else { Some(policy_preview.clone()) },
                "policyUpdate": if policy_exists { Some(policy_preview.clone()) } else { None },
                "policyFinalizeUpdate": Value::Null,
                "storedPolicyMatch": {
                    "policyId": stored.policy.id.as_i64(),
                    "vaultId": stored.vault.id.as_i64(),
                    "vaultActive": stored.vault.active,
                    "activePolicyId": stored.vault.active_policy_id.as_i64(),
                    "setupPolicyId": Value::Null,
                    "policyActive": stored.policy.active,
                },
                "updatedPolicyDecoded": decoded_policy_account_json(&updated_decoded),
                "decodedAllowsInitObligation": updated_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP)),
                "decodedAllowsRefreshObligation": updated_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP)),
            }))?
        );
        return Ok(());
    }

    let setup_policy_seed = vault
        .setup_policy_seed
        .unwrap_or_else(|| vault.policy_seed.saturating_add(1));
    let setup_policy_seed_u64 =
        u64::try_from(setup_policy_seed).map_err(|_| "setup_policy_seed must be >= 0")?;
    let setup_policy = vault
        .setup_policy_account
        .as_deref()
        .map(Pubkey::from_str)
        .transpose()?
        .unwrap_or_else(|| derive_action_account(&settings, setup_policy_seed_u64).0);
    let route_setup = if policy_exists {
        update_same_mint_market_mint_yield_route_action(
            context,
            final_universe.clone(),
            policy,
            account_index,
        )?
    } else {
        let setup = create_same_mint_market_mint_yield_route_action(
            context,
            final_universe.clone(),
            policy_seed,
        )?;
        if setup.accounts.withdraw != policy {
            return Err(format!(
                "route policy seed {} derives {}, but DB policy_account is {}",
                policy_seed, setup.accounts.withdraw, policy
            )
            .into());
        }
        setup
    };
    let setup_existing_account =
        rpc.get_account_with_commitment(&setup_policy, CommitmentConfig::confirmed())?;
    let setup_policy_exists = if let Some(account) = setup_existing_account.value.as_ref() {
        if account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
            return Err(format!(
                "setup policy account {} is owned by {}, expected {}",
                setup_policy, account.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID
            )
            .into());
        }
        true
    } else {
        false
    };
    let setup_existing_decoded = setup_existing_account
        .value
        .as_ref()
        .and_then(|account| decode_squads_policy_account(&account.data).ok());
    let setup_policy_setup = if setup_policy_exists {
        update_init_obligation_yield_route_action(
            context,
            final_universe.clone(),
            setup_policy,
            account_index,
        )?
    } else {
        let setup = create_init_obligation_yield_route_action(
            context,
            final_universe.clone(),
            setup_policy_seed_u64,
        )?;
        if setup.accounts.withdraw != setup_policy {
            return Err(format!(
                "setup policy seed {} derives {}, but expected setup policy {}",
                setup_policy_seed, setup.accounts.withdraw, setup_policy
            )
            .into());
        }
        setup
    };
    let route_instruction = route_setup
        .instructions
        .first()
        .ok_or("fallback route policy instruction was not built")?
        .clone();
    let setup_instruction = setup_policy_setup
        .instructions
        .first()
        .ok_or("fallback setup policy instruction was not built")?
        .clone();
    let route_policy_instructions = vec![route_instruction.clone()];
    let setup_policy_instructions = vec![setup_instruction.clone()];
    let route_policy_manifest = policy_lookup_table_manifest(
        authority_signer.pubkey(),
        &route_policy_instructions,
        vault,
        &[&route_setup],
        &[policy],
    )?;
    let setup_policy_manifest = policy_lookup_table_manifest(
        authority_signer.pubkey(),
        &setup_policy_instructions,
        vault,
        &[&setup_policy_setup],
        &[setup_policy],
    )?;
    let setup_policy_requires_landed_route_create = !policy_exists && !setup_policy_exists;
    let route_policy_lookup_table_phase = prepare_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        "policy",
        "policy",
        "route_policy_update",
        policy_lookup_table_scope.clone(),
        authority_signer.pubkey(),
        route_policy_instructions,
        route_policy_manifest,
        &[&authority_signer],
        options.execute,
    )
    .await?;
    let setup_policy_lookup_table_phase = prepare_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        "policy",
        "setup_policy",
        "setup_policy_update",
        policy_lookup_table_scope.clone(),
        authority_signer.pubkey(),
        setup_policy_instructions,
        setup_policy_manifest,
        &[&authority_signer],
        options.execute && !setup_policy_requires_landed_route_create,
    )
    .await?;
    if options.execute && setup_policy_requires_landed_route_create {
        setup_policy_lookup_table_phase
            .resolution
            .require_deferred_simulation_coverage()
            .map_err(|error| {
                format!(
                    "setup-policy ALT coverage is incomplete before route-policy creation: {error}"
                )
            })?;
    }
    let lookup_table_provisioning = json!({
        "mode": "active_reusable_resolver",
        "routePolicy": route_policy_lookup_table_phase.resolution.evidence.clone(),
        "setupPolicy": setup_policy_lookup_table_phase.resolution.evidence.clone(),
        "setupSimulationDeferredUntilRouteCreateLands": setup_policy_requires_landed_route_create,
    });
    let setup_policy_simulation_skip_reason = setup_policy_requires_landed_route_create.then(|| {
        "setup policy create uses the next Squads policy seed and must be simulated after the route policy create lands".to_owned()
    });
    let route_policy_transaction = build_policy_transaction(
        &rpc,
        authority_signer.pubkey(),
        route_instruction,
        &lookup_table_accounts,
        &authority_signer,
        if policy_exists {
            "route policy fallback update"
        } else {
            "route policy fallback create"
        },
        None,
    )?;
    let mut setup_policy_transaction = build_policy_transaction(
        &rpc,
        authority_signer.pubkey(),
        setup_instruction.clone(),
        &lookup_table_accounts,
        &authority_signer,
        if setup_policy_exists {
            "setup policy fallback update"
        } else {
            "setup policy fallback create"
        },
        setup_policy_simulation_skip_reason,
    )?;
    let route_policy_preview = policy_operation_preview_json(
        if policy_exists { "update" } else { "create" },
        vault,
        settings,
        policy,
        vault_pubkey,
        authority_signer.pubkey(),
        delegated_signer.pubkey(),
        db_delegated_signer_matches,
        &final_universe,
        &swap_lanes,
        &route_setup,
        &route_policy_transaction,
        existing_decoded.as_ref(),
    )?;
    let mut setup_policy_preview = setup_policy_operation_preview_json(
        if setup_policy_exists {
            "update"
        } else {
            "create"
        },
        vault,
        settings,
        setup_policy,
        setup_policy_seed,
        vault_pubkey,
        authority_signer.pubkey(),
        delegated_signer.pubkey(),
        db_delegated_signer_matches,
        &final_universe,
        &setup_policy_setup,
        &setup_policy_transaction,
        setup_existing_decoded.as_ref(),
    )?;

    if !options.execute {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "policy_update_dry_run",
                "writesDecision": false,
                "sendsTransactions": false,
                "fallbackRequired": true,
                "fallbackReason": "all_safe_one_policy_exceeds_packet_limit",
                "lookupTableProvisioning": lookup_table_provisioning.clone(),
                "policyAllInOneAttempt": all_in_one_preview,
                "policyCreate": if policy_exists { None } else { Some(route_policy_preview.clone()) },
                "policyUpdate": if policy_exists { Some(route_policy_preview.clone()) } else { None },
                "setupPolicyCreate": if setup_policy_exists { None } else { Some(setup_policy_preview.clone()) },
                "setupPolicyUpdate": if setup_policy_exists { Some(setup_policy_preview.clone()) } else { None },
                "policyFinalizeUpdate": Value::Null,
            }))?
        );
        return Ok(());
    }

    if let Some(error) = route_policy_transaction.simulation_error.clone() {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "policy_update_simulation_failed",
                "writesDecision": false,
                "sendsTransactions": false,
                "fallbackRequired": true,
                "lookupTableProvisioning": lookup_table_provisioning.clone(),
                "policyAllInOneAttempt": all_in_one_preview,
                "policyCreate": if policy_exists { None } else { Some(route_policy_preview.clone()) },
                "policyUpdate": if policy_exists { Some(route_policy_preview.clone()) } else { None },
                "setupPolicyCreate": if setup_policy_exists { None } else { Some(setup_policy_preview.clone()) },
                "setupPolicyUpdate": if setup_policy_exists { Some(setup_policy_preview.clone()) } else { None },
                "policyFinalizeUpdate": Value::Null,
            }))?
        );
        return Err(format!("fallback route policy simulation failed: {error}").into());
    }
    if let Some(error) = setup_policy_transaction.simulation_error.clone() {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "policy_update_simulation_failed",
                "writesDecision": false,
                "sendsTransactions": false,
                "fallbackRequired": true,
                "lookupTableProvisioning": lookup_table_provisioning.clone(),
                "policyAllInOneAttempt": all_in_one_preview,
                "policyCreate": if policy_exists { None } else { Some(route_policy_preview.clone()) },
                "policyUpdate": if policy_exists { Some(route_policy_preview.clone()) } else { None },
                "setupPolicyCreate": if setup_policy_exists { None } else { Some(setup_policy_preview.clone()) },
                "setupPolicyUpdate": if setup_policy_exists { Some(setup_policy_preview.clone()) } else { None },
                "policyFinalizeUpdate": Value::Null,
            }))?
        );
        return Err(format!("fallback setup policy simulation failed: {error}").into());
    }

    let submitted_route_policy = submit_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        &route_policy_lookup_table_phase,
        &[&authority_signer],
        &format!("route-policy-update:{policy}"),
    )
    .await?;
    let route_submitted_slot = u64::try_from(submitted_route_policy.submitted_slot)?;
    let route_signature = submitted_route_policy.signature.clone();
    let route_confirmed_slot = u64::try_from(submitted_route_policy.confirmed_slot)?;
    let mut active_setup_lookup_table_phase = setup_policy_lookup_table_phase;
    if setup_policy_requires_landed_route_create {
        setup_policy_transaction = build_policy_transaction(
            &rpc,
            authority_signer.pubkey(),
            setup_instruction.clone(),
            &lookup_table_accounts,
            &authority_signer,
            "setup policy fallback create",
            None,
        )?;
        setup_policy_preview = setup_policy_operation_preview_json(
            "create",
            vault,
            settings,
            setup_policy,
            setup_policy_seed,
            vault_pubkey,
            authority_signer.pubkey(),
            delegated_signer.pubkey(),
            db_delegated_signer_matches,
            &final_universe,
            &setup_policy_setup,
            &setup_policy_transaction,
            setup_existing_decoded.as_ref(),
        )?;
        if let Some(error) = setup_policy_transaction.simulation_error.clone() {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "policy_update_simulation_failed",
                    "writesDecision": false,
                    "sendsTransactions": true,
                    "fallbackRequired": true,
                    "lookupTableProvisioning": lookup_table_provisioning.clone(),
                    "routeSignature": route_signature,
                    "routeSubmittedSlot": i64::try_from(route_submitted_slot)?,
                    "routeConfirmedSlot": i64::try_from(route_confirmed_slot)?,
                    "policyAllInOneAttempt": all_in_one_preview,
                    "policyCreate": Some(route_policy_preview.clone()),
                    "policyUpdate": Value::Null,
                    "setupPolicyCreate": Some(setup_policy_preview.clone()),
                    "setupPolicyUpdate": Value::Null,
                    "policyFinalizeUpdate": Value::Null,
                }))?
            );
            return Err(format!(
                "fallback setup policy simulation failed after route policy create landed: {error}"
            )
            .into());
        }
        let setup_policy_manifest = policy_lookup_table_manifest(
            authority_signer.pubkey(),
            std::slice::from_ref(&setup_instruction),
            vault,
            &[&setup_policy_setup],
            &[setup_policy],
        )?;
        active_setup_lookup_table_phase = prepare_route_lookup_table_phase(
            client,
            &rpc,
            options,
            vault,
            "policy",
            "setup_policy",
            "setup_policy_update",
            policy_lookup_table_scope.clone(),
            authority_signer.pubkey(),
            vec![setup_instruction.clone()],
            setup_policy_manifest,
            &[&authority_signer],
            true,
        )
        .await?;
    }
    let submitted_setup_policy = submit_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        &active_setup_lookup_table_phase,
        &[&authority_signer],
        &format!("setup-policy-update:{setup_policy}"),
    )
    .await?;
    let setup_submitted_slot = u64::try_from(submitted_setup_policy.submitted_slot)?;
    let setup_signature = submitted_setup_policy.signature.clone();
    let setup_confirmed_slot = u64::try_from(submitted_setup_policy.confirmed_slot)?;
    let create_signature = if policy_exists {
        None
    } else {
        Some(route_signature.clone())
    };
    let create_submitted_slot = if policy_exists {
        None
    } else {
        Some(i64::try_from(route_submitted_slot)?)
    };
    let create_confirmed_slot = if policy_exists {
        None
    } else {
        Some(i64::try_from(route_confirmed_slot)?)
    };
    let setup_create_signature = if setup_policy_exists {
        None
    } else {
        Some(setup_signature.clone())
    };
    let setup_create_submitted_slot = if setup_policy_exists {
        None
    } else {
        Some(i64::try_from(setup_submitted_slot)?)
    };
    let setup_create_confirmed_slot = if setup_policy_exists {
        None
    } else {
        Some(i64::try_from(setup_confirmed_slot)?)
    };
    let policy_swap_lanes = policy_swap_lanes_json(&route_setup, &swap_lanes)?;
    let (stored, stored_setup_policy) = client
        .record_route_and_setup_policy_match(
            PolicyMatchInput {
                signature: route_signature.clone(),
                slot: route_confirmed_slot,
                settings: settings.to_string(),
                authority: authority.to_string(),
                policy_seed,
                policy_account: policy.to_string(),
                vault_index: account_index,
                vault_pubkey: vault_pubkey.to_string(),
                delegated_signers: vec![delegated_signer.pubkey().to_string()],
                threshold: 1,
                route_modes: vec![SAME_MINT_ROUTE_MODE.to_owned()],
                stable_mints: pubkeys_json(&final_universe.stable_mints),
                kamino_markets: pubkeys_json(&final_universe.kamino_markets),
                kamino_liquidity_mints: pubkeys_json(&final_universe.kamino_liquidity_mints),
                universe_preset: Some(KAMINO_STABLE_UNIVERSE_PRESET.to_owned()),
                risk_profile: Some(SAFE_RISK_PROFILE.to_owned()),
                swap_lanes: policy_swap_lanes.clone(),
            },
            PolicyMatchInput {
                signature: setup_signature.clone(),
                slot: setup_confirmed_slot,
                settings: settings.to_string(),
                authority: authority.to_string(),
                policy_seed: setup_policy_seed_u64,
                policy_account: setup_policy.to_string(),
                vault_index: account_index,
                vault_pubkey: vault_pubkey.to_string(),
                delegated_signers: vec![delegated_signer.pubkey().to_string()],
                threshold: 1,
                route_modes: vec![format!("{SAME_MINT_ROUTE_MODE}_setup")],
                stable_mints: pubkeys_json(&final_universe.stable_mints),
                kamino_markets: pubkeys_json(&final_universe.kamino_markets),
                kamino_liquidity_mints: pubkeys_json(&final_universe.kamino_liquidity_mints),
                universe_preset: Some(KAMINO_STABLE_UNIVERSE_PRESET.to_owned()),
                risk_profile: Some(SAFE_RISK_PROFILE.to_owned()),
                swap_lanes: Value::Array(vec![]),
            },
        )
        .await?;
    let updated_route_account = rpc.get_account(&policy)?;
    let updated_route_decoded =
        decode_squads_policy_account(&updated_route_account.data).map_err(|error| {
            format!("failed to decode updated route policy account {policy}: {error}")
        })?;
    let updated_setup_account = rpc.get_account(&setup_policy)?;
    let updated_setup_decoded =
        decode_squads_policy_account(&updated_setup_account.data).map_err(|error| {
            format!("failed to decode updated setup policy account {setup_policy}: {error}")
        })?;
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": if policy_exists || setup_policy_exists { "policy_fallback_updated" } else { "policy_fallback_created" },
            "writesDecision": false,
            "sendsTransactions": true,
            "fallbackRequired": true,
            "fallbackReason": "all_safe_one_policy_exceeds_packet_limit",
            "lookupTableProvisioning": lookup_table_provisioning,
            "lookupTableResolution": {
                "routePolicy": submitted_route_policy.lookup_table_resolution,
                "setupPolicy": submitted_setup_policy.lookup_table_resolution,
            },
            "signature": route_signature,
            "submittedSlot": i64::try_from(route_submitted_slot)?,
            "confirmedSlot": i64::try_from(route_confirmed_slot)?,
            "createSignature": create_signature,
            "createSubmittedSlot": create_submitted_slot,
            "createConfirmedSlot": create_confirmed_slot,
            "setupSignature": setup_signature,
            "setupSubmittedSlot": i64::try_from(setup_submitted_slot)?,
            "setupConfirmedSlot": i64::try_from(setup_confirmed_slot)?,
            "setupCreateSignature": setup_create_signature,
            "setupCreateSubmittedSlot": setup_create_submitted_slot,
            "setupCreateConfirmedSlot": setup_create_confirmed_slot,
            "policyAllInOneAttempt": all_in_one_preview,
            "policyCreate": if policy_exists { None } else { Some(route_policy_preview.clone()) },
            "policyUpdate": if policy_exists { Some(route_policy_preview.clone()) } else { None },
            "setupPolicyCreate": if setup_policy_exists { None } else { Some(setup_policy_preview.clone()) },
            "setupPolicyUpdate": if setup_policy_exists { Some(setup_policy_preview.clone()) } else { None },
            "policyFinalizeUpdate": Value::Null,
            "storedPolicyMatch": {
                "policyId": stored.policy.id.as_i64(),
                "setupPolicyId": stored_setup_policy.id.as_i64(),
                "vaultId": stored.vault.id.as_i64(),
                "vaultActive": stored.vault.active,
                "activePolicyId": stored.vault.active_policy_id.as_i64(),
                "activePolicyRemainsRoutePolicy": stored.vault.active_policy_id == stored.policy.id,
                "policyActive": stored.policy.active,
                "setupPolicyActive": stored_setup_policy.active,
            },
            "updatedPolicyDecoded": decoded_policy_account_json(&updated_route_decoded),
            "updatedSetupPolicyDecoded": decoded_policy_account_json(&updated_setup_decoded),
            "decodedAllowsInitObligation": updated_setup_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP)),
            "decodedRouteAllowsInitObligation": updated_route_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP)),
            "decodedAllowsRefreshObligation": updated_route_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP))
                || updated_setup_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP)),
        }))?
    );
    Ok(())
}

fn build_missing_obligation_setup_dry_run(
    options: &CliOptions,
    vault: &SelectedVault,
    target: &ChainPositionSummary,
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<MissingObligationSetupDryRun, Box<dyn Error>> {
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let delegated_signer = policy_keypair_from_env()?;
    let admin_fee_payer = if options.optimization_cycle {
        None
    } else {
        Some(solana_testing_keypair_from_env()?)
    };
    let fee_payer: &dyn Signer = admin_fee_payer
        .as_ref()
        .map(|keypair| keypair as &dyn Signer)
        .unwrap_or(&delegated_signer);
    build_missing_obligation_setup_dry_run_with_signers(
        &rpc,
        &[],
        vault,
        target,
        policy_preflight,
        fee_payer,
        &delegated_signer,
    )
}

fn missing_obligation_setup_vault_rent_top_up(
    rpc: &RpcClient,
    vault_pubkey: Pubkey,
    fee_payer: &dyn Signer,
) -> Result<(Option<MissingObligationSetupFunding>, Vec<Instruction>), Box<dyn Error>> {
    missing_obligation_setup_vault_rent_top_up_for_payer(rpc, vault_pubkey, fee_payer.pubkey())
}

fn missing_obligation_setup_vault_rent_top_up_for_payer(
    rpc: &RpcClient,
    vault_pubkey: Pubkey,
    fee_payer: Pubkey,
) -> Result<(Option<MissingObligationSetupFunding>, Vec<Instruction>), Box<dyn Error>> {
    let required_vault_lamports =
        rpc.get_minimum_balance_for_rent_exemption(std::mem::size_of::<Obligation>() + 8)?;
    if required_vault_lamports > MAX_KAMINO_OBLIGATION_RENT_LAMPORTS {
        return Err(format!(
            "route_setup_rent_cap_exceeded: RPC requires {required_vault_lamports} lamports for a Kamino obligation, above the {MAX_KAMINO_OBLIGATION_RENT_LAMPORTS} lamport cap"
        )
        .into());
    }
    let vault_lamports_before = rpc.get_balance(&vault_pubkey)?;
    let payer_lamports_before = rpc.get_balance(&fee_payer)?;
    if vault_lamports_before >= required_vault_lamports || fee_payer == vault_pubkey {
        return Ok((None, Vec::new()));
    }

    let lamports = required_vault_lamports.saturating_sub(vault_lamports_before);
    if payer_lamports_before < lamports {
        return Err(format!(
            "route_funding_required: missing-obligation rent payer {fee_payer} has {payer_lamports_before} lamports but vault {vault_pubkey} requires a {lamports} lamport top-up"
        )
        .into());
    }
    let transfer = system_instruction::transfer(&fee_payer, &vault_pubkey, lamports);
    Ok((
        Some(MissingObligationSetupFunding {
            payer: fee_payer.to_string(),
            vault: vault_pubkey.to_string(),
            lamports,
            vault_lamports_before,
            payer_lamports_before,
            required_vault_lamports,
        }),
        vec![transfer],
    ))
}

fn build_missing_obligation_setup_dry_run_with_signers(
    rpc: &RpcClient,
    lookup_table_accounts: &[AddressLookupTableAccount],
    vault: &SelectedVault,
    target: &ChainPositionSummary,
    policy_preflight: Option<&PolicyAccountPreflight>,
    fee_payer: &dyn Signer,
    delegated_signer: &dyn Signer,
) -> Result<MissingObligationSetupDryRun, Box<dyn Error>> {
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault_index {} must fit u8 for Squads account index",
            vault.vault_index
        )
    })?;
    let (policy, instruction_constraint_index) =
        resolve_init_obligation_policy(Some(rpc), vault, target, policy_preflight)?;
    let (vault_rent_top_up, setup_pre_instructions) =
        missing_obligation_setup_vault_rent_top_up(rpc, vault_pubkey, fee_payer)?;
    let route_policy = Pubkey::from_str(&vault.policy_account)?;
    let policy_source = if policy == route_policy {
        "route_policy"
    } else {
        "setup_policy"
    };

    let instruction_plan = init_obligation_execution_instructions(
        policy,
        account_index,
        vault_pubkey,
        target,
        instruction_constraint_index,
        delegated_signer,
        &setup_pre_instructions,
    )?;
    let (instructions, lookup_table_requirements) = instruction_plan.into_parts();
    let transaction_signers = same_mint_route_signers(fee_payer, delegated_signer);
    let init_execution = build_signed_transaction(
        rpc,
        fee_payer.pubkey(),
        &instructions,
        lookup_table_accounts,
        &transaction_signers,
        "init-obligation setup execution",
        None,
    )?;

    Ok(MissingObligationSetupDryRun {
        policy_account: policy.to_string(),
        policy_source,
        instruction_constraint_index,
        vault_rent_top_up,
        instructions,
        lookup_table_requirements,
        init_execution,
    })
}

async fn execute_missing_obligation_setup_with_reusable_alts(
    options: &CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
    _preview: &ChainReconcilePreview,
    target: &ChainPositionSummary,
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<(MissingObligationSetupSubmitResult, Value), Box<dyn Error>> {
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let delegated_signer = policy_keypair_from_env()?;
    let admin_fee_payer = if options.optimization_cycle {
        None
    } else {
        Some(solana_testing_keypair_from_env()?)
    };
    let fee_payer: &dyn Signer = admin_fee_payer
        .as_ref()
        .map(|keypair| keypair as &dyn Signer)
        .unwrap_or(&delegated_signer);
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault_index {} must fit u8 for Squads account index",
            vault.vault_index
        )
    })?;
    let (policy, instruction_constraint_index) =
        resolve_init_obligation_policy(Some(&rpc), vault, target, policy_preflight)?;
    let (vault_rent_top_up, setup_pre_instructions) =
        missing_obligation_setup_vault_rent_top_up(&rpc, vault_pubkey, fee_payer)?;
    let route_policy = Pubkey::from_str(&vault.policy_account)?;
    let policy_source = if policy == route_policy {
        "route_policy"
    } else {
        "setup_policy"
    };
    let instruction_plan = init_obligation_execution_instructions(
        policy,
        account_index,
        vault_pubkey,
        target,
        instruction_constraint_index,
        &delegated_signer,
        &setup_pre_instructions,
    )?;
    let (instructions, lookup_table_requirements) = instruction_plan.into_parts();
    let manifest = route_lookup_table_manifest(
        fee_payer.pubkey(),
        &instructions,
        vault,
        &lookup_table_requirements,
        &[],
    )?;
    let signers = same_mint_route_signers(fee_payer, &delegated_signer);
    let scope =
        same_mint_route_lookup_table_scope_for_reserves(vault, &target.reserve, &target.reserve);
    let phase = prepare_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        &target.reserve,
        &target.reserve,
        "kamino_obligation_setup",
        scope,
        fee_payer.pubkey(),
        instructions,
        manifest,
        &signers,
        true,
    )
    .await?;
    let submitted = submit_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        &phase,
        &signers,
        &format!("kamino-obligation-setup:{}", target.obligation),
    )
    .await?;
    let lookup_table_resolution = submitted.lookup_table_resolution.clone();
    Ok((
        MissingObligationSetupSubmitResult {
            policy_account: policy.to_string(),
            policy_source,
            instruction_constraint_index,
            vault_rent_top_up,
            init_signature: submitted.signature,
            init_submitted_slot: submitted.submitted_slot,
            init_confirmed_slot: submitted.confirmed_slot,
            init_simulation_units_consumed: submitted.simulation_units_consumed,
            init_transaction_packet: submitted.transaction_packet,
        },
        lookup_table_resolution,
    ))
}

async fn run_setup_obligation_flow(
    options: &CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    setup_reserve: &str,
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<(), Box<dyn Error>> {
    let target = chain_position_for_reserve(preview, setup_reserve)?;
    if target.obligation_exists {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "setup_obligation_reserve_skipped_existing",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "execute": options.execute,
                "vault": vault_json(vault),
                "target": {
                    "reserve": target.reserve,
                    "market": target.market,
                    "liquidityMint": target.liquidity_mint,
                    "obligation": target.obligation,
                    "obligationExists": true,
                },
                "chainReconcile": chain_reconcile_preview_json(preview),
            }))?
        );
        return Ok(());
    }

    if !options.execute {
        let dry_run =
            build_missing_obligation_setup_dry_run(options, vault, target, policy_preflight)?;
        let delegated_signer = policy_keypair_from_env()?;
        let admin_fee_payer = if options.optimization_cycle {
            None
        } else {
            Some(solana_testing_keypair_from_env()?)
        };
        let fee_payer: &dyn Signer = admin_fee_payer
            .as_ref()
            .map(|keypair| keypair as &dyn Signer)
            .unwrap_or(&delegated_signer);
        let rpc = RpcClient::new_with_commitment(
            options.rpc_url.to_owned(),
            CommitmentConfig::confirmed(),
        );
        let manifest = route_lookup_table_manifest(
            fee_payer.pubkey(),
            &dry_run.instructions,
            vault,
            &dry_run.lookup_table_requirements,
            &[],
        )?;
        let signers = same_mint_route_signers(fee_payer, &delegated_signer);
        let lookup_table_phase = prepare_route_lookup_table_phase(
            client,
            &rpc,
            options,
            vault,
            setup_reserve,
            setup_reserve,
            "kamino_obligation_setup",
            same_mint_route_lookup_table_scope_for_reserves(vault, setup_reserve, setup_reserve),
            fee_payer.pubkey(),
            dry_run.instructions.clone(),
            manifest,
            &signers,
            false,
        )
        .await?;
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "setup_obligation_reserve_dry_run",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "execute": false,
                "vault": vault_json(vault),
                "target": {
                    "reserve": target.reserve,
                    "market": target.market,
                    "liquidityMint": target.liquidity_mint,
                    "obligation": target.obligation,
                    "obligationExists": false,
                },
                "chainReconcile": chain_reconcile_preview_json(preview),
                "missingObligationSetup": missing_obligation_setup_dry_run_json(target, &dry_run),
                "lookupTableResolution": lookup_table_phase.resolution.evidence,
            }))?
        );
        return Ok(());
    }

    let (result, lookup_table_resolution) = execute_missing_obligation_setup_with_reusable_alts(
        options,
        client,
        vault,
        preview,
        target,
        policy_preflight,
    )
    .await?;
    let post_preview = load_chain_reconcile_preview(
        &options.rpc_url,
        vault,
        &preview
            .positions
            .iter()
            .map(|position| position.reserve.clone())
            .collect::<Vec<_>>(),
    )?;
    let post_target = chain_position_for_reserve(&post_preview, setup_reserve)?;
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "setup_obligation_reserve_executed",
            "writesDecision": false,
            "writesCurrentPositions": false,
            "sendsTransactions": true,
            "execute": true,
            "vault": vault_json(vault),
            "target": {
                "reserve": post_target.reserve,
                "market": post_target.market,
                "liquidityMint": post_target.liquidity_mint,
                "obligation": post_target.obligation,
                "obligationExists": post_target.obligation_exists,
            },
            "setup": missing_obligation_setup_submit_result_json(target, &result),
            "lookupTableResolution": lookup_table_resolution,
            "postChainReconcile": chain_reconcile_preview_json(&post_preview),
        }))?
    );
    Ok(())
}

fn init_obligation_execution_instructions(
    policy: Pubkey,
    account_index: u8,
    vault_pubkey: Pubkey,
    target: &ChainPositionSummary,
    instruction_constraint_index: u8,
    delegated_signer: &dyn Signer,
    setup_pre_instructions: &[Instruction],
) -> Result<YieldRouteInstructionPlan, Box<dyn Error>> {
    let init_instruction = kamino_init_obligation_instruction(vault_pubkey, target)?;
    guard_lookup_table_mutations(
        std::slice::from_ref(init_instruction.instruction()),
        "raw init-obligation policy inner instruction",
    )?;
    let (init_instruction, mut init_requirements) = init_instruction.into_parts();
    let mut transaction_accounts = Vec::new();
    let init_compiled =
        compile_squads_inner_instruction(&mut transaction_accounts, init_instruction);
    let outer_instruction = execute_program_interaction_policy_instruction(
        policy,
        delegated_signer.pubkey(),
        account_index,
        vec![init_compiled],
        vec![instruction_constraint_index],
        transaction_accounts,
    );
    init_requirements.add_policy(policy);
    let mut outer_requirements = YieldRouteLookupTableRequirements::default();
    outer_requirements.add_vault_account(vault_pubkey);
    outer_requirements.add_policy(policy);
    let mut plan = YieldRouteInstructionPlan::with_outer_context(outer_requirements);
    for instruction in setup_pre_instructions {
        plan.push_outer_instruction(instruction.clone());
    }
    plan.push(YieldRouteInstruction::new(
        outer_instruction,
        init_requirements,
    ))?;
    Ok(plan)
}

async fn run_initial_reserve_deposit_flow(
    options: &CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
    initial_preview: &ChainReconcilePreview,
    policy_preflight: Option<&PolicyAccountPreflight>,
    deposit_reserve: &str,
    amount_raw: u64,
) -> Result<(), Box<dyn Error>> {
    if amount_raw == 0 {
        return Err("initial deposit amount must be greater than 0".into());
    }

    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let wallet_signer = solana_testing_keypair_from_env()?;
    let delegated_signer = policy_keypair_from_env()?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault index {} does not fit Squads account index",
            vault.vault_index
        )
    })?;
    let deposit_position = chain_position_for_reserve(initial_preview, deposit_reserve)?;
    let mut active_preview = initial_preview.clone();
    let mut reloaded_policy_preflight: Option<PolicyAccountPreflight> = None;
    let mut missing_obligation_setup_result: Option<Value> = None;
    let mut missing_obligation_setup_dry_run =
        if !options.execute && !deposit_position.obligation_exists {
            Some(
                build_missing_obligation_setup_dry_run(
                    options,
                    vault,
                    deposit_position,
                    policy_preflight,
                )
                .map(|dry_run| missing_obligation_setup_dry_run_json(deposit_position, &dry_run))
                .unwrap_or_else(|error| {
                    json!({
                        "targetObligation": deposit_position.obligation,
                        "targetReserve": deposit_position.reserve,
                        "targetMarket": deposit_position.market,
                        "error": safe_same_mint_operational_error(error.as_ref()),
                    })
                }),
            )
        } else {
            None
        };
    if !options.execute && !deposit_position.obligation_exists {
        if let Ok(setup_dry_run) = build_missing_obligation_setup_dry_run(
            options,
            vault,
            deposit_position,
            policy_preflight,
        ) {
            let setup_manifest = route_lookup_table_manifest(
                wallet_signer.pubkey(),
                &setup_dry_run.instructions,
                vault,
                &setup_dry_run.lookup_table_requirements,
                &[],
            )?;
            let setup_signers: Vec<&dyn Signer> = vec![&wallet_signer, &delegated_signer];
            let setup_phase = prepare_route_lookup_table_phase(
                client,
                &rpc,
                options,
                vault,
                deposit_reserve,
                deposit_reserve,
                "kamino_obligation_setup",
                same_mint_route_lookup_table_scope_for_reserves(
                    vault,
                    deposit_reserve,
                    deposit_reserve,
                ),
                wallet_signer.pubkey(),
                setup_dry_run.instructions,
                setup_manifest,
                &setup_signers,
                false,
            )
            .await?;
            if let Some(Value::Object(fields)) = missing_obligation_setup_dry_run.as_mut() {
                fields.insert(
                    "lookupTableResolution".to_owned(),
                    setup_phase.resolution.evidence,
                );
            }
        }
    }
    let wallet_usdc_ata =
        derive_associated_token_address(&wallet_signer.pubkey(), &USDC_MINT, &spl_token::ID);
    let vault_usdc_ata = derive_associated_token_address(&vault_pubkey, &USDC_MINT, &spl_token::ID);
    let (wallet_usdc_amount_raw, wallet_usdc_account_exists) =
        load_spl_token_account_amount(&rpc, &wallet_usdc_ata, &USDC_MINT)?;
    let funding_needed_raw = amount_raw.saturating_sub(deposit_position.vault_liquidity_amount_raw);
    let mut blockers = Vec::new();
    if !wallet_usdc_account_exists {
        blockers.push(format!(
            "wallet USDC ATA {} does not exist for {}",
            wallet_usdc_ata,
            wallet_signer.pubkey()
        ));
    }
    if wallet_usdc_amount_raw < funding_needed_raw {
        blockers.push(format!(
            "wallet USDC balance {} is below needed funding amount {}",
            wallet_usdc_amount_raw, funding_needed_raw
        ));
    }
    if !deposit_position.obligation_exists && !options.execute {
        blockers.push(format!(
            "deposit obligation {} is missing for reserve {}; run missing-obligation setup before policy deposit",
            deposit_position.obligation, deposit_position.reserve
        ));
    }
    if options.execute && blockers.iter().any(|reason| reason.contains("wallet USDC")) {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "initial_deposit_preflight_blocked",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "preflightBlockers": blockers,
                "missingObligationSetup": Value::Null,
            }))?
        );
        return Err("initial reserve deposit preflight blocked before setup".into());
    }
    if options.execute && !deposit_position.obligation_exists {
        let (setup_result, lookup_table_resolution) =
            execute_missing_obligation_setup_with_reusable_alts(
                options,
                client,
                vault,
                initial_preview,
                deposit_position,
                policy_preflight,
            )
            .await?;
        let mut setup_result_json =
            missing_obligation_setup_submit_result_json(deposit_position, &setup_result);
        if let Value::Object(fields) = &mut setup_result_json {
            fields.insert("lookupTableResolution".to_owned(), lookup_table_resolution);
        }
        missing_obligation_setup_result = Some(setup_result_json);
        active_preview =
            load_chain_reconcile_preview(&options.rpc_url, vault, &[deposit_reserve.to_owned()])?;
        reloaded_policy_preflight = Some(load_policy_account_preflight(
            &options.rpc_url,
            vault,
            &active_preview,
            &ReserveMove {
                source_reserve: deposit_reserve.to_owned(),
                target_reserve: deposit_reserve.to_owned(),
            },
        )?);
        let active_deposit = chain_position_for_reserve(&active_preview, deposit_reserve)?;
        if !active_deposit.obligation_exists {
            return Err(format!(
                "deposit obligation {} is still missing after setup execution",
                active_deposit.obligation
            )
            .into());
        }
    }
    let active_policy_preflight = reloaded_policy_preflight.as_ref().or(policy_preflight);
    let active_deposit_position = chain_position_for_reserve(&active_preview, deposit_reserve)?;
    let funding_creates_missing_vault_usdc_ata =
        !active_deposit_position.vault_liquidity_token_account_exists;

    let mut funding_instructions = vec![create_associated_token_account_idempotent_instruction(
        wallet_signer.pubkey(),
        vault_pubkey,
        USDC_MINT,
        spl_token::ID,
    )];
    if funding_needed_raw > 0 {
        funding_instructions.push(spl_token::instruction::transfer_checked(
            &spl_token::ID,
            &wallet_usdc_ata,
            &USDC_MINT,
            &vault_usdc_ata,
            &wallet_signer.pubkey(),
            &[],
            funding_needed_raw,
            6,
        )?);
    }
    let funding_skip_reason = if blockers.iter().any(|reason| reason.contains("wallet USDC")) {
        Some("funding simulation skipped because wallet USDC preflight failed".to_owned())
    } else {
        None
    };
    let funding_transaction = build_signed_transaction(
        &rpc,
        wallet_signer.pubkey(),
        &funding_instructions,
        &[],
        &[&wallet_signer],
        "initial reserve funding",
        funding_skip_reason,
    )?;

    let policy_plan = match build_initial_reserve_deposit_policy_plan(
        vault,
        &active_preview,
        active_policy_preflight,
        deposit_reserve,
        amount_raw,
        wallet_signer.pubkey(),
        delegated_signer.pubkey(),
        account_index,
    ) {
        Ok(plan) => Some(plan),
        Err(error) => {
            blockers.push(safe_same_mint_operational_error(error.as_ref()));
            None
        }
    };
    let dry_run_policy_transaction = if let Some(plan) = policy_plan.as_ref() {
        let policy_simulation_skip_reason =
            if deposit_position.vault_liquidity_amount_raw >= amount_raw {
                None
            } else {
                Some(
                "policy deposit simulation requires the wallet funding transaction to land first"
                    .to_owned(),
            )
            };
        let mut policy_instructions = plan.pre_instructions.clone();
        policy_instructions.push(plan.instruction.clone());
        Some(build_signed_transaction(
            &rpc,
            wallet_signer.pubkey(),
            &policy_instructions,
            &[],
            &[&wallet_signer, &delegated_signer],
            "initial reserve policy deposit",
            policy_simulation_skip_reason,
        )?)
    } else {
        None
    };
    let pre_funding_lookup_table_phase = if let Some(plan) = policy_plan.as_ref() {
        let mut instructions = plan.pre_instructions.clone();
        instructions.push(plan.instruction.clone());
        let manifest = route_lookup_table_manifest(
            wallet_signer.pubkey(),
            &instructions,
            vault,
            &plan.lookup_table_requirements,
            &[],
        )?;
        let signers: Vec<&dyn Signer> = vec![&wallet_signer, &delegated_signer];
        Some(
            prepare_route_lookup_table_phase(
                client,
                &rpc,
                options,
                vault,
                deposit_reserve,
                deposit_reserve,
                "initial_reserve_deposit",
                same_mint_route_lookup_table_scope_for_reserves(
                    vault,
                    deposit_reserve,
                    deposit_reserve,
                ),
                wallet_signer.pubkey(),
                instructions,
                manifest,
                &signers,
                false,
            )
            .await?,
        )
    } else {
        None
    };

    if !options.execute {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "initial_deposit_dry_run",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "deposit": {
                    "reserve": &deposit_position.reserve,
                    "market": &deposit_position.market,
                    "liquidityMint": USDC_MINT.to_string(),
                    "amountRaw": amount_raw.to_string(),
                },
                "wallet": {
                    "signer": wallet_signer.pubkey().to_string(),
                    "usdcAta": wallet_usdc_ata.to_string(),
                    "usdcAtaExists": wallet_usdc_account_exists,
                    "usdcAmountRaw": wallet_usdc_amount_raw.to_string(),
                },
                "vault": vault_json(vault),
                "vaultUsdcAta": vault_usdc_ata.to_string(),
                "chainReconcile": chain_reconcile_preview_json(initial_preview),
                "activeChainReconcile": chain_reconcile_preview_json(&active_preview),
                "policyPreflight": policy_route_preflight_json(vault, &ReserveMove {
                    source_reserve: deposit_reserve.to_owned(),
                    target_reserve: deposit_reserve.to_owned(),
                }, active_policy_preflight),
                "preflightBlockers": blockers,
                "missingObligationSetup": missing_obligation_setup_dry_run,
                "fundingTransaction": policy_transaction_json(&funding_transaction),
                "policyDeposit": policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
                "policyDepositTransaction": dry_run_policy_transaction.as_ref().map(policy_transaction_json),
                "lookupTableResolution": pre_funding_lookup_table_phase.as_ref().map(|phase| phase.resolution.evidence.clone()),
            }))?
        );
        return Ok(());
    }

    if let Some(error) = &funding_transaction.simulation_error {
        return Err(format!("initial reserve funding simulation failed: {error}").into());
    }
    if let Some(phase) = pre_funding_lookup_table_phase.as_ref() {
        phase
            .resolution
            .require_missing_token_account_deferred_simulation_coverage(
                funding_creates_missing_vault_usdc_ata,
            )
            .map_err(|error| {
                format!(
                    "initial reserve deposit ALT coverage is incomplete before wallet funding: {error}"
                )
            })?;
    }

    if !blockers.is_empty() {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "initial_deposit_preflight_blocked",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "preflightBlockers": blockers,
                "missingObligationSetup": missing_obligation_setup_result.clone(),
                "fundingTransaction": policy_transaction_json(&funding_transaction),
                "policyDeposit": policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
            }))?
        );
        return Err("initial reserve deposit preflight blocked before live submit".into());
    }
    let funding_submitted_slot = i64::try_from(rpc.get_slot()?)?;
    let funding_signature = rpc.send_and_confirm_transaction(&funding_transaction.transaction)?;
    let funding_confirmed_slot = i64::try_from(rpc.get_slot()?)?;

    let funded_preview =
        load_chain_reconcile_preview(&options.rpc_url, vault, &[deposit_reserve.to_owned()])?;
    let funded_deposit_position = chain_position_for_reserve(&funded_preview, deposit_reserve)?;
    if funded_deposit_position.vault_liquidity_amount_raw < amount_raw {
        return Err(format!(
            "vault USDC ATA {} has {} after funding, below requested deposit {}",
            funded_deposit_position.vault_liquidity_ata,
            funded_deposit_position.vault_liquidity_amount_raw,
            amount_raw
        )
        .into());
    }

    let policy_plan = build_initial_reserve_deposit_policy_plan(
        vault,
        &funded_preview,
        active_policy_preflight,
        deposit_reserve,
        amount_raw,
        wallet_signer.pubkey(),
        delegated_signer.pubkey(),
        account_index,
    )?;
    let mut policy_instructions = policy_plan.pre_instructions.clone();
    policy_instructions.push(policy_plan.instruction.clone());
    let policy_manifest = route_lookup_table_manifest(
        wallet_signer.pubkey(),
        &policy_instructions,
        vault,
        &policy_plan.lookup_table_requirements,
        &[],
    )?;
    let policy_signers: Vec<&dyn Signer> = vec![&wallet_signer, &delegated_signer];
    let policy_lookup_table_phase = prepare_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        deposit_reserve,
        deposit_reserve,
        "initial_reserve_deposit",
        same_mint_route_lookup_table_scope_for_reserves(vault, deposit_reserve, deposit_reserve),
        wallet_signer.pubkey(),
        policy_instructions,
        policy_manifest,
        &policy_signers,
        true,
    )
    .await?;
    let submitted_policy = submit_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        &policy_lookup_table_phase,
        &policy_signers,
        &format!("initial-reserve-deposit:{}", deposit_reserve),
    )
    .await
    .map_err(|error| {
        format!(
            "initial reserve policy deposit failed after funding tx {}: {error}",
            funding_signature
        )
    })?;
    let policy_submitted_slot = submitted_policy.submitted_slot;
    let policy_signature = submitted_policy.signature.clone();
    let policy_confirmed_slot = submitted_policy.confirmed_slot;
    let post_preview =
        load_chain_reconcile_preview(&options.rpc_url, vault, &[deposit_reserve.to_owned()])?;
    let snapshot = client
        .reconcile_vault(vault.id, chain_preview_reconciled_state(&post_preview)?)
        .await?;
    let result = InitialDepositSubmitResult {
        funding_signature: Some(funding_signature.to_string()),
        funding_submitted_slot: Some(funding_submitted_slot),
        funding_confirmed_slot: Some(funding_confirmed_slot),
        funding_simulation_units_consumed: funding_transaction.simulation_units_consumed,
        funding_transaction_packet: funding_transaction.transaction_packet,
        policy_signature: Some(policy_signature),
        policy_submitted_slot: Some(policy_submitted_slot),
        policy_confirmed_slot: Some(policy_confirmed_slot),
        policy_simulation_units_consumed: submitted_policy.simulation_units_consumed,
        policy_transaction_packet: submitted_policy.transaction_packet,
        reconciled_snapshot_id: Some(snapshot.id),
        post_chain_preview: Some(post_preview),
    };

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "initial_deposit_executed",
            "writesDecision": false,
            "writesCurrentPositions": true,
            "sendsTransactions": true,
            "deposit": {
                "reserve": deposit_reserve,
                "market": &funded_deposit_position.market,
                "liquidityMint": USDC_MINT.to_string(),
                "amountRaw": amount_raw.to_string(),
            },
            "wallet": {
                "signer": wallet_signer.pubkey().to_string(),
                "usdcAta": wallet_usdc_ata.to_string(),
            },
            "vault": vault_json(vault),
            "vaultUsdcAta": vault_usdc_ata.to_string(),
            "missingObligationSetup": missing_obligation_setup_result,
            "fundingTransaction": {
                "signature": result.funding_signature,
                "submittedSlot": result.funding_submitted_slot,
                "confirmedSlot": result.funding_confirmed_slot,
                "simulationUnitsConsumed": result.funding_simulation_units_consumed,
                "transaction": transaction_packet_json(&result.funding_transaction_packet),
            },
            "policyDeposit": initial_deposit_policy_preview_json(&policy_plan.preview),
            "policyDepositTransaction": {
                "signature": result.policy_signature,
                "submittedSlot": result.policy_submitted_slot,
                "confirmedSlot": result.policy_confirmed_slot,
                "simulationUnitsConsumed": result.policy_simulation_units_consumed,
                "transaction": transaction_packet_json(&result.policy_transaction_packet),
            },
            "lookupTableResolution": submitted_policy.lookup_table_resolution,
            "reconciledSnapshotId": result.reconciled_snapshot_id.map(SnapshotId::as_i64),
            "postChainReconcile": result.post_chain_preview.as_ref().map(chain_reconcile_preview_json),
        }))?
    );

    Ok(())
}

async fn run_idle_vault_deposit_flow(
    options: &mut CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
    current_market: Option<&CurrentRouteMarketEconomics>,
    initial_preview: &ChainReconcilePreview,
    policy_preflight: Option<&PolicyAccountPreflight>,
    deposit_reserve: &str,
    amount_raw: u64,
    fused_lease_state: Option<&FusedExecutionLeaseState>,
) -> Result<Option<InProcessRouteResult>, Box<dyn Error>> {
    if amount_raw == 0 {
        return Err("idle vault deposit amount must be greater than 0".into());
    }

    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let signer = policy_keypair_from_env()?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault index {} does not fit Squads account index",
            vault.vault_index
        )
    })?;
    let deposit_position = chain_position_for_reserve(initial_preview, deposit_reserve)?;
    let vault_usdc_ata = derive_associated_token_address(&vault_pubkey, &USDC_MINT, &spl_token::ID);
    let db_idle = client
        .current_idle_token_balance(vault.id, &USDC_MINT.to_string())
        .await?;
    let amount_i64 = i64::try_from(amount_raw)
        .map_err(|_| "idle vault deposit amount does not fit Postgres BIGINT")?;
    let live_idle_amount_i64 = i64::try_from(deposit_position.vault_liquidity_amount_raw)
        .map_err(|_| "live idle vault USDC amount does not fit Postgres BIGINT")?;
    let mut active_preview = initial_preview.clone();
    let mut reloaded_policy_preflight: Option<PolicyAccountPreflight> = None;
    let mut setup_obligation_before_deposit = false;
    let mut setup_obligation_policy: Option<String> = None;
    let mut setup_obligation_policy_source: Option<String> = None;
    let mut setup_obligation_vault_rent_top_up_lamports: i64 = 0;
    let mut missing_obligation_setup_plan: Option<MissingObligationSetupDryRun> = None;
    let mut missing_obligation_setup_dry_run: Option<Value> = None;
    let mut missing_obligation_setup_result: Option<Value> = None;

    let mut blockers: Vec<IdleVaultDepositBlocker> = Vec::new();
    if deposit_position.liquidity_mint != USDC_MINT.to_string() {
        blockers.push(IdleVaultDepositBlocker::safety(format!(
            "target reserve {} liquidity mint {} is not USDC {}",
            deposit_position.reserve, deposit_position.liquidity_mint, USDC_MINT
        )));
    }
    if deposit_position.vault_liquidity_ata != vault_usdc_ata.to_string() {
        blockers.push(IdleVaultDepositBlocker::safety(format!(
            "chain preview vault liquidity ATA {} does not match derived vault USDC ATA {}",
            deposit_position.vault_liquidity_ata, vault_usdc_ata
        )));
    }
    if !deposit_position.vault_liquidity_token_account_exists {
        blockers.push(IdleVaultDepositBlocker::safety(format!(
            "vault idle USDC ATA {} does not exist",
            vault_usdc_ata
        )));
    }
    if deposit_position.vault_liquidity_amount_raw < amount_raw {
        blockers.push(IdleVaultDepositBlocker::source_stale(format!(
            "live vault idle USDC balance {} is below planned deposit amount {}",
            deposit_position.vault_liquidity_amount_raw, amount_raw
        )));
    }
    if !deposit_position.obligation_exists {
        match build_missing_obligation_setup_dry_run_with_signers(
            &rpc,
            &[],
            vault,
            deposit_position,
            policy_preflight,
            &signer,
            &signer,
        ) {
            Ok(dry_run) => {
                setup_obligation_before_deposit = true;
                setup_obligation_policy = Some(dry_run.policy_account.clone());
                setup_obligation_policy_source = Some(dry_run.policy_source.to_owned());
                setup_obligation_vault_rent_top_up_lamports = dry_run
                    .vault_rent_top_up
                    .as_ref()
                    .map(|funding| i64::try_from(funding.lamports))
                    .transpose()
                    .map_err(|_| "setup obligation rent top-up lamports do not fit BIGINT")?
                    .unwrap_or(0);
                missing_obligation_setup_dry_run = Some(missing_obligation_setup_dry_run_json(
                    deposit_position,
                    &dry_run,
                ));
                missing_obligation_setup_plan = Some(dry_run);
            }
            Err(error) => blockers.push(IdleVaultDepositBlocker::route_preflight(
                safe_same_mint_operational_error_with_context(
                    "idle_vault_init_obligation_plan_failed",
                    error.as_ref(),
                ),
            )),
        }
    }

    match db_idle.as_ref() {
        Some(balance) => {
            if balance.mint != USDC_MINT.to_string() {
                blockers.push(IdleVaultDepositBlocker::safety(format!(
                    "DB idle mint {} does not match USDC {}",
                    balance.mint, USDC_MINT
                )));
            }
            if balance.token_account != vault_usdc_ata.to_string() {
                blockers.push(IdleVaultDepositBlocker::safety(format!(
                    "DB idle token account {} does not match vault USDC ATA {}",
                    balance.token_account, vault_usdc_ata
                )));
            }
            if balance.amount_raw != amount_i64 {
                blockers.push(IdleVaultDepositBlocker::source_stale(format!(
                    "DB idle amount {} does not match planned amount {}",
                    balance.amount_raw, amount_i64
                )));
            }
            if balance.amount_raw > live_idle_amount_i64 {
                blockers.push(IdleVaultDepositBlocker::source_stale(format!(
                    "DB idle amount {} is above live vault ATA balance {}",
                    balance.amount_raw, live_idle_amount_i64
                )));
            }
            if let Some(expected_account) = &options.expected_idle_token_account {
                if balance.token_account != *expected_account {
                    let reason = format!(
                        "expected idle token account {} does not match DB row {}",
                        expected_account, balance.token_account
                    );
                    if balance.token_account == vault_usdc_ata.to_string() {
                        blockers.push(IdleVaultDepositBlocker::source_stale(reason));
                    } else {
                        blockers.push(IdleVaultDepositBlocker::safety(reason));
                    }
                }
            }
            if let Some(expected_slot) = options.expected_idle_observed_slot {
                if balance.observed_slot != expected_slot {
                    blockers.push(IdleVaultDepositBlocker::source_stale(format!(
                        "expected idle observed slot {} does not match DB row {}",
                        expected_slot, balance.observed_slot
                    )));
                }
            }
            if let Some(expected_at) = options.expected_idle_observed_at {
                if balance.observed_at != expected_at {
                    blockers.push(IdleVaultDepositBlocker::source_stale(format!(
                        "expected idle observed at {} does not match DB row {}",
                        expected_at.to_rfc3339(),
                        balance.observed_at.to_rfc3339()
                    )));
                }
            }
        }
        None => blockers.push(IdleVaultDepositBlocker::safety(format!(
            "missing loyal_yield.vault_idle_token_balances_current row for vault {} USDC",
            vault.id.as_i64()
        ))),
    }

    if let Some(expected_account) = &options.expected_idle_token_account {
        if expected_account != &vault_usdc_ata.to_string() {
            blockers.push(IdleVaultDepositBlocker::safety(format!(
                "expected idle token account {} does not match derived vault USDC ATA {}",
                expected_account, vault_usdc_ata
            )));
        }
    }
    if let Some(expected_mint) = &options.expected_liquidity_mint {
        if expected_mint != &USDC_MINT.to_string() {
            blockers.push(IdleVaultDepositBlocker::safety(format!(
                "expected liquidity mint {} does not match USDC {}",
                expected_mint, USDC_MINT
            )));
        }
    }
    if let Some(expected_amount) = options.expected_amount_raw {
        if expected_amount != amount_i64 {
            blockers.push(IdleVaultDepositBlocker::safety(format!(
                "expected amount {} does not match requested idle deposit amount {}",
                expected_amount, amount_i64
            )));
        }
    }
    if let Some(expected_edge) = options.expected_edge_bps {
        if expected_edge <= 0 {
            blockers.push(IdleVaultDepositBlocker::safety(format!(
                "expected idle deposit edge {} must be positive",
                expected_edge
            )));
        }
    }

    let atomic_queue_setup = setup_obligation_before_deposit && options.opportunity_id.is_some();
    let mut predicted_deposit_preview = initial_preview.clone();
    if setup_obligation_before_deposit {
        let predicted_position = predicted_deposit_preview
            .positions
            .iter_mut()
            .find(|position| position.reserve == deposit_reserve)
            .ok_or("idle deposit target disappeared from predicted post-setup preview")?;
        predicted_position.obligation_exists = true;
    }
    let missing_obligation_setup_retry = !deposit_position.obligation_exists
        && missing_obligation_setup_plan.is_none()
        && blockers
            .iter()
            .any(|blocker| blocker.kind == IdleVaultDepositBlockerKind::Retry);
    let dry_run_policy_plan = if missing_obligation_setup_retry {
        None
    } else {
        match build_initial_reserve_deposit_policy_plan(
            vault,
            &predicted_deposit_preview,
            policy_preflight,
            deposit_reserve,
            amount_raw,
            signer.pubkey(),
            signer.pubkey(),
            account_index,
        ) {
            Ok(plan) => Some(plan),
            Err(error) => {
                blockers.push(IdleVaultDepositBlocker::safety(
                    safe_same_mint_operational_error(error.as_ref()),
                ));
                None
            }
        }
    };
    let dry_run_policy_transaction: Option<PolicyTransactionBuild> = None;
    let mut setup_lookup_table_phase: Option<RouteLookupTablePhase> = None;
    let mut deposit_lookup_table_phase: Option<RouteLookupTablePhase> = None;
    let transaction_signers = vec![&signer as &dyn Signer];

    if options.route_runtime_active() {
        require_current_opportunity_fence(client, options, vault, None).await?;
    }

    if !atomic_queue_setup {
        if let Some(setup) = missing_obligation_setup_plan.as_ref() {
            let manifest = route_lookup_table_manifest(
                signer.pubkey(),
                &setup.instructions,
                vault,
                &setup.lookup_table_requirements,
                &[],
            )?;
            let scope = format!(
                "idle_vault_deposit_setup:{}:{}:{}",
                vault.settings, vault.vault_index, deposit_reserve
            );
            let resolution = resolve_route_lookup_tables(
                client,
                &rpc,
                options,
                vault,
                deposit_reserve,
                deposit_reserve,
                "idle_vault_deposit_setup",
                &scope,
                signer.pubkey(),
                &setup.instructions,
                &manifest,
                &transaction_signers,
            )
            .await?;
            if let Some(blocker) = resolution.blocker.as_ref() {
                blockers.push(IdleVaultDepositBlocker::route_resolution(
                    "idle setup route resolver blocked",
                    blocker,
                ));
            }
            setup_lookup_table_phase = Some(RouteLookupTablePhase {
                route_kind: "idle_vault_deposit_setup",
                scope,
                source_reserve: deposit_reserve.to_owned(),
                target_reserve: deposit_reserve.to_owned(),
                instructions: setup.instructions.clone(),
                manifest,
                resolution,
            });
        }
    }

    if let Some(plan) = dry_run_policy_plan.as_ref() {
        let mut instructions = Vec::new();
        let mut requirements = plan.lookup_table_requirements.clone();
        if atomic_queue_setup {
            let setup = missing_obligation_setup_plan
                .as_ref()
                .ok_or("atomic idle setup plan disappeared")?;
            instructions.extend(setup.instructions.iter().cloned());
            requirements.merge(&setup.lookup_table_requirements)?;
        }
        instructions.extend(plan.pre_instructions.iter().cloned());
        instructions.push(plan.instruction.clone());
        let manifest =
            route_lookup_table_manifest(signer.pubkey(), &instructions, vault, &requirements, &[])?;
        let scope = format!(
            "idle_vault_deposit:{}:{}:{}",
            vault.settings, vault.vault_index, deposit_reserve
        );
        let route_kind = if atomic_queue_setup {
            "idle_vault_deposit_atomic_setup"
        } else {
            "idle_vault_deposit"
        };
        let serializes_policy_setup_funding = atomic_queue_setup
            || plan
                .preview
                .route_steps
                .contains(&KAMINO_INIT_OBLIGATION_FARM_ROUTE_STEP);
        let mut resolution = resolve_route_lookup_tables(
            client,
            &rpc,
            options,
            vault,
            deposit_reserve,
            deposit_reserve,
            route_kind,
            &scope,
            signer.pubkey(),
            &instructions,
            &manifest,
            &transaction_signers,
        )
        .await?;
        apply_policy_setup_funding_serialization(
            &mut resolution,
            &plan.preview.signer,
            serializes_policy_setup_funding,
        );
        if let Some(blocker) = resolution.blocker.as_ref() {
            blockers.push(IdleVaultDepositBlocker::route_resolution(
                "idle deposit route resolver blocked",
                blocker,
            ));
        }
        deposit_lookup_table_phase = Some(RouteLookupTablePhase {
            route_kind,
            scope,
            source_reserve: deposit_reserve.to_owned(),
            target_reserve: deposit_reserve.to_owned(),
            instructions,
            manifest,
            resolution,
        });
    }

    let mut setup_provisioning_request_id = None;
    let mut deposit_provisioning_request_id = None;
    if options.route_runtime_active() {
        let exact_fingerprints = idle_route_fingerprints(
            setup_lookup_table_phase.as_ref(),
            deposit_lookup_table_phase.as_ref(),
        );
        require_current_opportunity_fence(
            client,
            options,
            vault,
            if options.execute {
                exact_fingerprints
                    .as_ref()
                    .map(|(route, requirements)| (route.as_str(), requirements.as_str()))
            } else {
                None
            },
        )
        .await?;
        let acquire_leases = blockers.is_empty();
        if let Some(phase) = setup_lookup_table_phase.as_ref() {
            setup_provisioning_request_id = persist_route_lookup_table_resolution(
                client,
                options,
                vault,
                &phase.source_reserve,
                &phase.target_reserve,
                phase.route_kind,
                &phase.manifest,
                &phase.resolution,
                (options.execute || options.fused_execute) && acquire_leases,
                true,
            )
            .await?;
        }
        if let Some(phase) = deposit_lookup_table_phase.as_ref() {
            deposit_provisioning_request_id = persist_route_lookup_table_resolution(
                client,
                options,
                vault,
                &phase.source_reserve,
                &phase.target_reserve,
                phase.route_kind,
                &phase.manifest,
                &phase.resolution,
                (options.execute || options.fused_execute) && acquire_leases,
                true,
            )
            .await?;
        }
    }
    let idle_lookup_table_evidence = json!({
        "setup": setup_lookup_table_phase
            .as_ref()
            .map(|phase| json!({
                "provisioningRequestId": setup_provisioning_request_id,
                "resolution": phase.resolution.evidence.clone(),
            })),
        "deposit": deposit_lookup_table_phase
            .as_ref()
            .map(|phase| json!({
                "provisioningRequestId": deposit_provisioning_request_id,
                "resolution": phase.resolution.evidence.clone(),
        })),
    });

    if options.fused_execute && blockers.is_empty() {
        let exact_fingerprints = idle_route_fingerprints(
            setup_lookup_table_phase.as_ref(),
            deposit_lookup_table_phase.as_ref(),
        )
        .ok_or("fused idle vault route did not produce exact route fingerprints")?;
        let current = require_current_opportunity_fence(client, options, vault, None)
            .await?
            .ok_or("fused idle vault route is missing its durable opportunity")?;
        let lease_owner = current
            .lease_owner
            .clone()
            .ok_or("fused idle vault route is missing its revalidation lease owner")?;
        let lease_expires_at = current
            .lease_expires_at
            .ok_or("fused idle vault route is missing its revalidation lease expiry")?;
        let revalidation_lease = RebalanceOpportunityLease {
            opportunity: current.clone(),
            claim_kind: RebalanceOpportunityClaimKind::Revalidate,
            owner: lease_owner,
            fencing_token: current.fencing_token,
            expires_at: lease_expires_at,
        };
        let ready_evidence = idle_in_process_route_result(
            SameMintRouteExecutionState::Ready,
            None,
            setup_lookup_table_phase.as_ref(),
            setup_provisioning_request_id,
            deposit_lookup_table_phase.as_ref(),
            deposit_provisioning_request_id,
        );
        let conflict_account_keys = ready_evidence.conflict_account_keys.clone();
        let mut execution_plan = current.execution_plan.clone();
        let fields = execution_plan
            .as_object_mut()
            .ok_or("fused idle vault opportunity execution plan is not an object")?;
        fields.insert(
            "exact_writable_account_keys".to_owned(),
            json!(ready_evidence.writable_account_keys),
        );
        fields.insert(
            "conflict_account_keys".to_owned(),
            json!(&conflict_account_keys),
        );
        fields.insert(
            "alt_readiness".to_owned(),
            ready_evidence.readiness_evidence.unwrap_or(Value::Null),
        );
        let promotion = client
            .try_promote_revalidation_lease_to_execute(
                &revalidation_lease,
                &exact_fingerprints.0,
                &exact_fingerprints.1,
                &execution_plan,
                &conflict_account_keys,
            )
            .await;
        let promoted = match promotion {
            Ok(promoted) => promoted,
            Err(error) => {
                release_idle_lookup_table_phase_leases(
                    client,
                    setup_lookup_table_phase.as_ref(),
                    deposit_lookup_table_phase.as_ref(),
                )
                .await;
                return Err(error.into());
            }
        };
        if let Some(promoted) = promoted {
            let state = fused_lease_state
                .ok_or("fused idle vault execution requires worker-owned promotion state")?;
            *state
                .lock()
                .map_err(|_| "fused idle vault promotion state lock was poisoned")? =
                Some(promoted.clone());
            options.execute = true;
            options.prepare_only = false;
            options.fused_execute = false;
            options.opportunity_fencing_token = Some(promoted.fencing_token);
        } else {
            release_idle_lookup_table_phase_leases(
                client,
                setup_lookup_table_phase.as_ref(),
                deposit_lookup_table_phase.as_ref(),
            )
            .await;
        }
    }

    let idle_decision_input = if options.execute {
        Some(IdleVaultDepositDecisionInput {
            target_reserve: deposit_reserve.to_owned(),
            target_market: Some(deposit_position.market.clone()),
            liquidity_mint: USDC_MINT.to_string(),
            amount_raw: amount_i64,
            idle_token_account: vault_usdc_ata.to_string(),
            idle_observed_slot: options.expected_idle_observed_slot.ok_or(
                "--deposit-idle-vault-reserve --execute requires --expected-idle-observed-slot",
            )?,
            idle_observed_at: options.expected_idle_observed_at.ok_or(
                "--deposit-idle-vault-reserve --execute requires --expected-idle-observed-at",
            )?,
            target_apy_bps: current_market
                .map(|market| market.capacity_adjusted_target_apy_bps)
                .or(options.expected_target_apy_bps)
                .ok_or(
                    "--deposit-idle-vault-reserve --execute requires --expected-target-apy-bps",
                )?,
            estimated_edge_bps: current_market
                .map(|market| market.edge_bps)
                .or(options.expected_edge_bps)
                .ok_or("--deposit-idle-vault-reserve --execute requires --expected-edge-bps")?,
            estimated_cost_lamports: options.expected_cost_lamports.unwrap_or_default(),
            setup_obligation_before_deposit,
            setup_obligation_policy: setup_obligation_policy.clone(),
            setup_obligation_policy_source: setup_obligation_policy_source.clone(),
            setup_obligation_vault_rent_top_up_lamports,
        })
    } else {
        None
    };

    if options.prepare_only {
        let blocker_reason = (!blockers.is_empty())
            .then(|| idle_vault_deposit_blocker_messages(&blockers).join("; "));
        let state = idle_vault_deposit_blocker_state(&blockers);
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": match state {
                    SameMintRouteExecutionState::Ready => "ready",
                    SameMintRouteExecutionState::WaitingAlt => "idle_vault_deposit_lookup_table_deferred",
                    SameMintRouteExecutionState::Retry => "idle_vault_deposit_revalidate_retry",
                    SameMintRouteExecutionState::Stale => "idle_vault_deposit_epoch_stale",
                    SameMintRouteExecutionState::Terminal => "idle_vault_deposit_preflight_blocked",
                    SameMintRouteExecutionState::SubmissionQueued => "idle_vault_deposit_submission_queued",
                    SameMintRouteExecutionState::Executed => "idle_vault_deposit_executed",
                },
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "reason": blocker_reason,
                "deposit": idle_vault_deposit_request_json(vault, deposit_reserve, &deposit_position, amount_raw, db_idle.as_ref(), options),
                "preflightBlockers": idle_vault_deposit_blocker_messages(&blockers),
                "lookupTableResolution": idle_lookup_table_evidence.clone(),
            }))?
        );
        return Ok(Some(idle_in_process_route_result(
            state,
            blocker_reason,
            setup_lookup_table_phase.as_ref(),
            setup_provisioning_request_id,
            deposit_lookup_table_phase.as_ref(),
            deposit_provisioning_request_id,
        )));
    }

    if !options.execute {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "idle_vault_deposit_dry_run",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "deposit": idle_vault_deposit_request_json(vault, deposit_reserve, &deposit_position, amount_raw, db_idle.as_ref(), options),
                "vault": vault_json(vault),
                "vaultUsdcAta": vault_usdc_ata.to_string(),
                "chainReconcile": chain_reconcile_preview_json(initial_preview),
                "policyPreflight": policy_route_preflight_json(vault, &ReserveMove {
                    source_reserve: deposit_reserve.to_owned(),
                    target_reserve: deposit_reserve.to_owned(),
                }, policy_preflight),
                "preflightBlockers": idle_vault_deposit_blocker_messages(&blockers),
                "setupObligationBeforeDeposit": setup_obligation_before_deposit,
                "missingObligationSetup": missing_obligation_setup_dry_run,
                "policyDepositRequiresSetup": setup_obligation_before_deposit,
                "policyDeposit": dry_run_policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
                "policyDepositTransaction": dry_run_policy_transaction.as_ref().map(policy_transaction_json),
                "lookupTableResolution": idle_lookup_table_evidence.clone(),
                "postConfirmReconcileReserves": idle_deposit_post_reconcile_reserves(options, deposit_reserve),
            }))?
        );
        return Ok(None);
    }

    if idle_vault_deposit_requires_lookup_table_provisioning(&blockers) {
        let preflight_blockers = idle_vault_deposit_blocker_messages(&blockers);
        let blocker_reason = format!(
            "idle vault deposit lookup-table provisioning deferred: {}",
            preflight_blockers.join("; ")
        );
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "idle_vault_deposit_lookup_table_deferred",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "retry": "next_monitor_cycle_after_provisioning",
                "deposit": idle_vault_deposit_request_json(vault, deposit_reserve, &deposit_position, amount_raw, db_idle.as_ref(), options),
                "preflightBlockers": preflight_blockers,
                "setupObligationBeforeDeposit": setup_obligation_before_deposit,
                "missingObligationSetup": missing_obligation_setup_dry_run,
                "policyDeposit": dry_run_policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
                "policyDepositTransaction": dry_run_policy_transaction.as_ref().map(policy_transaction_json),
                "lookupTableResolution": idle_lookup_table_evidence.clone(),
            }))?
        );
        if options.opportunity_id.is_some() {
            return Ok(Some(idle_in_process_route_result(
                SameMintRouteExecutionState::WaitingAlt,
                Some(blocker_reason),
                setup_lookup_table_phase.as_ref(),
                setup_provisioning_request_id,
                deposit_lookup_table_phase.as_ref(),
                deposit_provisioning_request_id,
            )));
        }
        return Err(blocker_reason.into());
    }

    if idle_vault_deposit_has_only_source_sync_blockers(&blockers) {
        let synced_idle = record_live_idle_vault_balance(
            client,
            vault,
            &vault_usdc_ata.to_string(),
            initial_preview,
            deposit_position,
        )
        .await?;
        let preflight_blockers = idle_vault_deposit_blocker_messages(&blockers);
        let source_sync_reasons = idle_vault_deposit_source_sync_reasons(&blockers);
        if let Some(sync_conflict) = live_idle_vault_balance_sync_conflict(
            &synced_idle,
            vault,
            &vault_usdc_ata.to_string(),
            initial_preview,
            live_idle_amount_i64,
        ) {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "idle_vault_deposit_stale_source_reconcile_conflict",
                    "writesDecision": false,
                    "writesCurrentPositions": false,
                    "writesCurrentIdleBalance": false,
                    "attemptedCurrentIdleBalanceWrite": true,
                    "sendsTransactions": false,
                    "deposit": idle_vault_deposit_request_json(vault, deposit_reserve, deposit_position, amount_raw, db_idle.as_ref(), options),
                    "preflightBlockers": preflight_blockers,
                    "sourceSyncReasons": source_sync_reasons,
                    "syncConflict": sync_conflict,
                    "syncedIdleBalance": idle_balance_json(&synced_idle),
                    "setupObligationBeforeDeposit": setup_obligation_before_deposit,
                    "missingObligationSetup": missing_obligation_setup_dry_run,
                    "policyDeposit": dry_run_policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
                    "policyDepositTransaction": dry_run_policy_transaction.as_ref().map(policy_transaction_json),
                    "lookupTableResolution": idle_lookup_table_evidence.clone(),
                }))?
            );
            return if options.opportunity_id.is_some() {
                Ok(Some(idle_in_process_route_result(
                    SameMintRouteExecutionState::Retry,
                    Some(
                        "idle vault source reconciliation conflicted with a newer observation"
                            .to_owned(),
                    ),
                    setup_lookup_table_phase.as_ref(),
                    setup_provisioning_request_id,
                    deposit_lookup_table_phase.as_ref(),
                    deposit_provisioning_request_id,
                )))
            } else {
                Ok(None)
            };
        }
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "idle_vault_deposit_stale_source_reconciled",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "writesCurrentIdleBalance": true,
                "sendsTransactions": false,
                "deposit": idle_vault_deposit_request_json(vault, deposit_reserve, deposit_position, amount_raw, db_idle.as_ref(), options),
                "preflightBlockers": preflight_blockers,
                "sourceSyncReasons": source_sync_reasons,
                "syncedIdleBalance": idle_balance_json(&synced_idle),
                "setupObligationBeforeDeposit": setup_obligation_before_deposit,
                "missingObligationSetup": missing_obligation_setup_dry_run,
                "policyDeposit": dry_run_policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
                "policyDepositTransaction": dry_run_policy_transaction.as_ref().map(policy_transaction_json),
                "lookupTableResolution": idle_lookup_table_evidence.clone(),
            }))?
        );
        return if options.opportunity_id.is_some() {
            Ok(Some(idle_in_process_route_result(
                SameMintRouteExecutionState::Retry,
                Some(
                    "idle vault source evidence was refreshed; re-plan before execution".to_owned(),
                ),
                setup_lookup_table_phase.as_ref(),
                setup_provisioning_request_id,
                deposit_lookup_table_phase.as_ref(),
                deposit_provisioning_request_id,
            )))
        } else {
            Ok(None)
        };
    }

    if idle_vault_deposit_blocker_state(&blockers) == SameMintRouteExecutionState::Retry {
        let preflight_blockers = idle_vault_deposit_blocker_messages(&blockers);
        let blocker_reason = format!(
            "idle vault deposit retryable preflight blocked: {}",
            preflight_blockers.join("; ")
        );
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "idle_vault_deposit_revalidate_retry",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "deposit": idle_vault_deposit_request_json(vault, deposit_reserve, &deposit_position, amount_raw, db_idle.as_ref(), options),
                "preflightBlockers": preflight_blockers,
                "setupObligationBeforeDeposit": setup_obligation_before_deposit,
                "missingObligationSetup": missing_obligation_setup_dry_run,
                "policyDeposit": dry_run_policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
                "policyDepositTransaction": dry_run_policy_transaction.as_ref().map(policy_transaction_json),
                "lookupTableResolution": idle_lookup_table_evidence.clone(),
            }))?
        );
        if options.opportunity_id.is_some() {
            return Ok(Some(idle_in_process_route_result(
                SameMintRouteExecutionState::Retry,
                Some(blocker_reason),
                setup_lookup_table_phase.as_ref(),
                setup_provisioning_request_id,
                deposit_lookup_table_phase.as_ref(),
                deposit_provisioning_request_id,
            )));
        }
        return Err(blocker_reason.into());
    }

    if !blockers.is_empty() {
        let preflight_blockers = idle_vault_deposit_blocker_messages(&blockers);
        let blocker_reason = format!(
            "idle vault deposit preflight blocked: {}",
            preflight_blockers.join("; ")
        );
        let mut blocked_decision = None;
        let mut blocked_decision_skip_reason = None;
        if options.opportunity_id.is_some() {
            return Ok(Some(idle_in_process_route_result(
                SameMintRouteExecutionState::Terminal,
                Some(blocker_reason),
                setup_lookup_table_phase.as_ref(),
                setup_provisioning_request_id,
                deposit_lookup_table_phase.as_ref(),
                deposit_provisioning_request_id,
            )));
        }
        if let Some(input) = idle_decision_input.clone() {
            let planned = client
                .record_idle_vault_deposit_decision(vault.id, input)
                .await?;
            match planned.status {
                PlanOutcomeStatus::Planned(decision) => {
                    let decision = if decision.status.is_terminal() {
                        decision
                    } else {
                        client
                            .advance_decision(
                                decision.id,
                                DecisionAdvance::Fail {
                                    reason: blocker_reason.clone(),
                                },
                            )
                            .await?
                    };
                    blocked_decision = Some(decision);
                }
                PlanOutcomeStatus::Skipped { reason } => {
                    blocked_decision_skip_reason = Some(reason.decision_reason().as_str());
                }
            }
        }
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "idle_vault_deposit_preflight_blocked",
                "writesDecision": blocked_decision.is_some(),
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "deposit": idle_vault_deposit_request_json(vault, deposit_reserve, &deposit_position, amount_raw, db_idle.as_ref(), options),
                "preflightBlockers": preflight_blockers,
                "decisionId": blocked_decision.as_ref().map(|decision| decision.id.as_i64()),
                "blockedDecision": blocked_decision.as_ref().map(idle_vault_deposit_decision_json),
                "blockedDecisionSkipReason": blocked_decision_skip_reason,
                "setupObligationBeforeDeposit": setup_obligation_before_deposit,
                "missingObligationSetup": missing_obligation_setup_dry_run,
                "policyDeposit": dry_run_policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
                "policyDepositTransaction": dry_run_policy_transaction.as_ref().map(policy_transaction_json),
                "lookupTableResolution": idle_lookup_table_evidence.clone(),
            }))?
        );
        return Err(blocker_reason.into());
    }

    let idle_decision_input =
        idle_decision_input.ok_or("idle vault deposit decision input was not built")?;
    let exact_idle_fingerprints = idle_route_fingerprints(
        setup_lookup_table_phase.as_ref(),
        deposit_lookup_table_phase.as_ref(),
    )
    .ok_or("idle vault deposit did not produce exact route fingerprints")?;
    let current_opportunity = require_current_opportunity_fence(
        client,
        options,
        vault,
        Some((
            exact_idle_fingerprints.0.as_str(),
            exact_idle_fingerprints.1.as_str(),
        )),
    )
    .await?;
    let queue_handoff = if let Some(current) = current_opportunity {
        let current_market =
            current_market.ok_or("queue idle route is missing its target capacity reservation")?;
        require_current_route_market_epoch(current_market, current.optimizer_epoch_id)?;
        let phase = deposit_lookup_table_phase
            .as_ref()
            .ok_or("queue idle deposit is missing its atomic transaction phase")?;
        Some(
            prepare_queue_signed_route_handoff(
                client,
                None,
                options,
                current,
                current_market,
                &phase.resolution,
                fee_only_shard_allowed_for_scope(FleetRouteFeePayerScope::IdleVault),
                None,
                None,
                None,
            )
            .await?,
        )
    } else {
        None
    };
    if let Some(handoff) = queue_handoff {
        let (planned, submission) = client
            .record_idle_vault_deposit_decision_with_signed_submission(
                vault.id,
                idle_decision_input.clone(),
                &handoff.lease,
                current_market
                    .ok_or("queue idle route is missing its target capacity reservation")?
                    .capacity_reservation
                    .clone(),
                handoff.submission,
            )
            .await?;
        let decision = match planned.status {
            PlanOutcomeStatus::Planned(decision) if decision.status == DecisionStatus::Planned => {
                decision
            }
            _ => return Err("atomic idle fleet handoff did not create a planned decision".into()),
        };
        if submission.decision_id != Some(decision.id) {
            return Err("atomic idle fleet handoff returned an unlinked submission".into());
        }
        release_idle_lookup_table_phase_leases(
            client,
            setup_lookup_table_phase.as_ref(),
            deposit_lookup_table_phase.as_ref(),
        )
        .await;
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "idle_vault_deposit_submission_queued",
                "writesDecision": true,
                "persistsSignedBytes": true,
                "atomicSignedDecisionHandoff": true,
                "sendsTransactions": false,
                "opportunityId": options.opportunity_id,
                "decisionId": decision.id.as_i64(),
                "submissionId": submission.id,
                "signature": submission.transaction_signature,
                "atomicSetup": atomic_queue_setup,
            }))?
        );
        return Ok(Some(idle_in_process_route_result(
            SameMintRouteExecutionState::SubmissionQueued,
            None,
            setup_lookup_table_phase.as_ref(),
            setup_provisioning_request_id,
            deposit_lookup_table_phase.as_ref(),
            deposit_provisioning_request_id,
        )));
    }
    let planned = client
        .record_idle_vault_deposit_decision(vault.id, idle_decision_input)
        .await?;
    let decision = match planned.status {
        PlanOutcomeStatus::Planned(decision) => decision,
        PlanOutcomeStatus::Skipped { reason } => {
            release_idle_lookup_table_phase_leases(
                client,
                setup_lookup_table_phase.as_ref(),
                deposit_lookup_table_phase.as_ref(),
            )
            .await;
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "idle_vault_deposit_not_planned",
                    "writesDecision": planned.decision_id.is_some(),
                    "sendsTransactions": false,
                    "skipReason": reason.decision_reason().as_str(),
                    "decisionId": planned.decision_id.map(|id| id.as_i64()),
                }))?
            );
            return Err("idle vault deposit was not planned".into());
        }
    };

    if decision.status.is_terminal() {
        release_idle_lookup_table_phase_leases(
            client,
            setup_lookup_table_phase.as_ref(),
            deposit_lookup_table_phase.as_ref(),
        )
        .await;
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "idle_vault_deposit_not_planned",
                "writesDecision": false,
                "sendsTransactions": false,
                "skipReason": "matched_terminal_idle_vault_deposit_decision",
                "decisionId": decision.id.as_i64(),
                "decisionStatus": decision.status.as_str(),
                "decision": idle_vault_deposit_decision_json(&decision),
            }))?
        );
        return Err("idle vault deposit was not planned because a terminal matching decision already exists".into());
    }

    if let Some(opportunity_id) = options.opportunity_id {
        let semantic_key = route_submission_semantic_key(opportunity_id);
        let submission = client
            .signed_route_submission_by_semantic_key(&semantic_key)
            .await?
            .ok_or("decision-linked idle queue submission disappeared")?;
        if submission.decision_id != Some(decision.id) {
            return Err(format!(
                "signed idle queue submission {} is not linked to decision {}",
                submission.id,
                decision.id.as_i64()
            )
            .into());
        }
        release_idle_lookup_table_phase_leases(
            client,
            setup_lookup_table_phase.as_ref(),
            deposit_lookup_table_phase.as_ref(),
        )
        .await;
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "idle_vault_deposit_submission_queued",
                "writesDecision": true,
                "persistsSignedBytes": true,
                "sendsTransactions": false,
                "opportunityId": opportunity_id,
                "decisionId": decision.id.as_i64(),
                "submissionId": submission.id,
                "signature": submission.transaction_signature,
                "atomicSetup": atomic_queue_setup,
            }))?
        );
        return Ok(Some(idle_in_process_route_result(
            SameMintRouteExecutionState::SubmissionQueued,
            None,
            setup_lookup_table_phase.as_ref(),
            setup_provisioning_request_id,
            deposit_lookup_table_phase.as_ref(),
            deposit_provisioning_request_id,
        )));
    }

    if setup_obligation_before_deposit && !atomic_queue_setup {
        let setup_phase = setup_lookup_table_phase
            .as_ref()
            .ok_or("idle deposit setup lookup-table phase was not prepared")?;
        let prepared_lease_reference = format!(
            "idle-decision:{}:setup:{}",
            decision.id.as_i64(),
            setup_phase.resolution.requirements_fingerprint
        );
        let setup_result = match async {
            require_current_opportunity_fence(
                client,
                options,
                vault,
                Some((
                    exact_idle_fingerprints.0.as_str(),
                    exact_idle_fingerprints.1.as_str(),
                )),
            )
            .await?;
            let mut presend = resolve_route_lookup_tables_immediately_before_send(
                client,
                &rpc,
                options,
                vault,
                setup_phase,
                &setup_phase.instructions,
                &setup_phase.manifest,
                &[&signer],
                &prepared_lease_reference,
            )
            .await?;
            let transaction = presend
                .selected_transaction
                .take()
                .ok_or("idle setup resolver did not return a signed transaction")?;
            let transaction_packet = presend
                .selected_transaction_packet
                .take()
                .ok_or("idle setup resolver did not return packet evidence")?;
            require_current_opportunity_fence(
                client,
                options,
                vault,
                Some((
                    exact_idle_fingerprints.0.as_str(),
                    exact_idle_fingerprints.1.as_str(),
                )),
            )
            .await?;
            let submitted_slot = i64::try_from(rpc.get_slot()?)?;
            let signature = rpc.send_and_confirm_transaction(&transaction)?.to_string();
            let confirmed_slot = i64::try_from(rpc.get_slot()?)?;
            let setup_plan = missing_obligation_setup_plan
                .take()
                .ok_or("idle setup metadata was not prepared")?;
            Ok::<_, Box<dyn Error>>(MissingObligationSetupSubmitResult {
                policy_account: setup_plan.policy_account,
                policy_source: setup_plan.policy_source,
                instruction_constraint_index: setup_plan.instruction_constraint_index,
                vault_rent_top_up: setup_plan.vault_rent_top_up,
                init_signature: signature,
                init_submitted_slot: submitted_slot,
                init_confirmed_slot: confirmed_slot,
                init_simulation_units_consumed: presend.selected_simulation_units_consumed,
                init_transaction_packet: transaction_packet,
            })
        }
        .await
        {
            Ok(result) => result,
            Err(error) => {
                release_route_lookup_table_phase_leases(
                    client,
                    setup_phase,
                    Some(&prepared_lease_reference),
                )
                .await;
                if let Some(deposit_phase) = deposit_lookup_table_phase.as_ref() {
                    release_route_lookup_table_phase_leases(client, deposit_phase, None).await;
                }
                let reason = safe_same_mint_operational_error_with_context(
                    "idle_vault_init_obligation_setup_failed",
                    error.as_ref(),
                );
                client
                    .advance_decision(
                        decision.id,
                        DecisionAdvance::Fail {
                            reason: reason.clone(),
                        },
                    )
                    .await?;
                return Err(reason.into());
            }
        };
        release_route_lookup_table_phase_leases(
            client,
            setup_phase,
            Some(&prepared_lease_reference),
        )
        .await;
        missing_obligation_setup_result = Some(missing_obligation_setup_submit_result_json(
            deposit_position,
            &setup_result,
        ));
        active_preview = match load_chain_reconcile_preview(
            &options.rpc_url,
            vault,
            &[deposit_reserve.to_owned()],
        ) {
            Ok(preview) => preview,
            Err(error) => {
                let reason = safe_same_mint_operational_error_with_context(
                    "idle_vault_init_obligation_chain_reload_failed",
                    error.as_ref(),
                );
                if let Some(deposit_phase) = deposit_lookup_table_phase.as_ref() {
                    release_route_lookup_table_phase_leases(client, deposit_phase, None).await;
                }
                client
                    .advance_decision(
                        decision.id,
                        DecisionAdvance::Fail {
                            reason: reason.clone(),
                        },
                    )
                    .await?;
                return Err(reason.into());
            }
        };
        reloaded_policy_preflight = match load_policy_account_preflight(
            &options.rpc_url,
            vault,
            &active_preview,
            &ReserveMove {
                source_reserve: deposit_reserve.to_owned(),
                target_reserve: deposit_reserve.to_owned(),
            },
        ) {
            Ok(preflight) => Some(preflight),
            Err(error) => {
                let reason = safe_same_mint_operational_error_with_context(
                    "idle_vault_init_obligation_policy_reload_failed",
                    error.as_ref(),
                );
                if let Some(deposit_phase) = deposit_lookup_table_phase.as_ref() {
                    release_route_lookup_table_phase_leases(client, deposit_phase, None).await;
                }
                client
                    .advance_decision(
                        decision.id,
                        DecisionAdvance::Fail {
                            reason: reason.clone(),
                        },
                    )
                    .await?;
                return Err(reason.into());
            }
        };
        let active_deposit = match chain_position_for_reserve(&active_preview, deposit_reserve) {
            Ok(position) => position,
            Err(error) => {
                let reason = safe_same_mint_operational_error_with_context(
                    "idle_vault_init_obligation_target_reload_failed",
                    error.as_ref(),
                );
                if let Some(deposit_phase) = deposit_lookup_table_phase.as_ref() {
                    release_route_lookup_table_phase_leases(client, deposit_phase, None).await;
                }
                client
                    .advance_decision(
                        decision.id,
                        DecisionAdvance::Fail {
                            reason: reason.clone(),
                        },
                    )
                    .await?;
                return Err(reason.into());
            }
        };
        if !active_deposit.obligation_exists {
            let reason = format!(
                "deposit obligation {} is still missing after idle vault init-obligation setup",
                active_deposit.obligation
            );
            if let Some(deposit_phase) = deposit_lookup_table_phase.as_ref() {
                release_route_lookup_table_phase_leases(client, deposit_phase, None).await;
            }
            client
                .advance_decision(
                    decision.id,
                    DecisionAdvance::Fail {
                        reason: reason.clone(),
                    },
                )
                .await?;
            return Err(reason.into());
        }
    }
    if atomic_queue_setup {
        // The queue path keeps setup + deposit in one vault-local atomic v0
        // transaction. This removes an intermediate ambiguous-send boundary
        // and lets the same durable signed-submission lifecycle cover first
        // deposits as well as already-initialized obligations.
        active_preview = predicted_deposit_preview.clone();
        missing_obligation_setup_result = Some(json!({
            "mode": "atomic_with_deposit",
            "broadcastedSeparately": false,
        }));
    }
    let active_policy_preflight = reloaded_policy_preflight.as_ref().or(policy_preflight);
    let active_deposit_position = chain_position_for_reserve(&active_preview, deposit_reserve)?;
    let policy_plan = match build_initial_reserve_deposit_policy_plan(
        vault,
        &active_preview,
        active_policy_preflight,
        deposit_reserve,
        amount_raw,
        signer.pubkey(),
        signer.pubkey(),
        account_index,
    ) {
        Ok(plan) => plan,
        Err(error) => {
            let reason = safe_same_mint_operational_error_with_context(
                "idle_vault_policy_deposit_plan_failed",
                error.as_ref(),
            );
            if let Some(deposit_phase) = deposit_lookup_table_phase.as_ref() {
                release_route_lookup_table_phase_leases(client, deposit_phase, None).await;
            }
            client
                .advance_decision(
                    decision.id,
                    DecisionAdvance::Fail {
                        reason: reason.clone(),
                    },
                )
                .await?;
            return Err(reason.into());
        }
    };
    let mut policy_instructions = policy_plan.pre_instructions.clone();
    policy_instructions.push(policy_plan.instruction.clone());
    let mut actual_deposit_requirements = policy_plan.lookup_table_requirements.clone();
    if atomic_queue_setup {
        let setup = missing_obligation_setup_plan
            .as_ref()
            .ok_or("atomic idle setup plan disappeared before final build")?;
        let mut combined = setup.instructions.clone();
        combined.extend(policy_instructions);
        policy_instructions = combined;
        actual_deposit_requirements.merge(&setup.lookup_table_requirements)?;
    }
    let deposit_phase = deposit_lookup_table_phase
        .as_ref()
        .ok_or("idle deposit lookup-table phase was not prepared")?;
    let actual_deposit_manifest = route_lookup_table_manifest(
        signer.pubkey(),
        &policy_instructions,
        vault,
        &actual_deposit_requirements,
        &[],
    )?;
    let prepared_lease_reference = format!(
        "idle-decision:{}:deposit:{}",
        decision.id.as_i64(),
        deposit_phase.resolution.requirements_fingerprint
    );
    require_current_opportunity_fence(
        client,
        options,
        vault,
        Some((
            exact_idle_fingerprints.0.as_str(),
            exact_idle_fingerprints.1.as_str(),
        )),
    )
    .await?;
    let mut presend_lookup_tables = match resolve_route_lookup_tables_immediately_before_send(
        client,
        &rpc,
        options,
        vault,
        deposit_phase,
        &policy_instructions,
        &actual_deposit_manifest,
        &[&signer],
        &prepared_lease_reference,
    )
    .await
    {
        Ok(resolution) => resolution,
        Err(error) => {
            release_route_lookup_table_phase_leases(
                client,
                deposit_phase,
                Some(&prepared_lease_reference),
            )
            .await;
            let reason = safe_same_mint_operational_error_with_context(
                "idle_vault_policy_deposit_lookup_table_presend_failed",
                error.as_ref(),
            );
            client
                .advance_decision(
                    decision.id,
                    DecisionAdvance::Fail {
                        reason: reason.clone(),
                    },
                )
                .await?;
            return Err(reason.into());
        }
    };
    let policy_transaction = presend_lookup_tables
        .selected_transaction
        .take()
        .ok_or("idle deposit resolver did not return a signed transaction")?;
    let policy_transaction_packet = presend_lookup_tables
        .selected_transaction_packet
        .take()
        .ok_or("idle deposit resolver did not return packet evidence")?;
    let policy_simulation_units_consumed = presend_lookup_tables.selected_simulation_units_consumed;
    let final_lookup_table_evidence = presend_lookup_tables.evidence.clone();
    require_current_opportunity_fence(
        client,
        options,
        vault,
        Some((
            exact_idle_fingerprints.0.as_str(),
            exact_idle_fingerprints.1.as_str(),
        )),
    )
    .await?;
    client
        .advance_decision(decision.id, DecisionAdvance::StartSimulation)
        .await?;
    client
        .advance_decision(decision.id, DecisionAdvance::SimulationReady)
        .await?;

    require_current_opportunity_fence(
        client,
        options,
        vault,
        Some((
            exact_idle_fingerprints.0.as_str(),
            exact_idle_fingerprints.1.as_str(),
        )),
    )
    .await?;
    let submitted_slot = i64::try_from(rpc.get_slot()?)?;
    let signature = match rpc.send_and_confirm_transaction(&policy_transaction) {
        Ok(signature) => signature,
        Err(error) => {
            release_route_lookup_table_phase_leases(
                client,
                deposit_phase,
                Some(&prepared_lease_reference),
            )
            .await;
            let reason = safe_same_mint_operational_error_with_context(
                "idle_vault_policy_deposit_submission_failed",
                &error,
            );
            client
                .advance_decision(
                    decision.id,
                    DecisionAdvance::Fail {
                        reason: reason.clone(),
                    },
                )
                .await?;
            return Err(reason.into());
        }
    };
    let confirmed_slot = i64::try_from(rpc.get_slot()?)?;
    release_route_lookup_table_phase_leases(client, deposit_phase, Some(&prepared_lease_reference))
        .await;
    let signature = signature.to_string();
    client
        .advance_decision(
            decision.id,
            DecisionAdvance::Submit {
                signature: signature.clone(),
                slot: Some(submitted_slot),
            },
        )
        .await?;
    client
        .advance_decision(decision.id, DecisionAdvance::StartConfirmation)
        .await?;

    let post_confirm = async {
        let post_reconcile_reserves =
            idle_deposit_post_reconcile_reserves(options, deposit_reserve);
        let post_preview = load_chain_reconcile_preview_with_min_context(
            &options.rpc_url,
            vault,
            &post_reconcile_reserves,
            Some(u64::try_from(confirmed_slot)?),
        )?;
        let post_reconcile_state = chain_preview_reconciled_state(&post_preview)?;
        let post_snapshot = client
            .reconcile_vault(vault.id, post_reconcile_state)
            .await?;
        let post_deposit_position = chain_position_for_reserve(&post_preview, deposit_reserve)?;
        let idle_after = client
            .record_current_idle_token_balance(CurrentIdleTokenBalance {
                vault_id: vault.id,
                mint: USDC_MINT.to_string(),
                amount_raw: i64::try_from(post_deposit_position.vault_liquidity_amount_raw)?,
                owner: vault.vault_pubkey.clone(),
                token_account: vault_usdc_ata.to_string(),
                observed_slot: post_preview.observed_slot,
                observed_at: Utc::now(),
                source_commitment: "confirmed".to_owned(),
                updated_at: Utc::now(),
            })
            .await?;
        let confirmed = client
            .advance_decision(
                decision.id,
                DecisionAdvance::Confirm {
                    slot: Some(confirmed_slot),
                    post_snapshot_id: Some(post_snapshot.id),
                },
            )
            .await?;
        Ok::<_, Box<dyn Error>>((
            post_reconcile_reserves,
            post_preview,
            post_snapshot,
            idle_after,
            confirmed,
        ))
    }
    .await;
    let (post_reconcile_reserves, post_preview, post_snapshot, idle_after, confirmed) =
        match post_confirm {
            Ok(value) => value,
            Err(error) => {
                let reason = safe_same_mint_operational_error_with_context(
                    "idle_vault_policy_deposit_confirmed_reconcile_failed",
                    error.as_ref(),
                );
                client
                    .advance_decision(
                        decision.id,
                        DecisionAdvance::Fail {
                            reason: reason.clone(),
                        },
                    )
                    .await?;
                return Err(reason.into());
            }
        };
    let repair = repair_idle_vault_deposit_partial_pull_history(
        client,
        vault,
        &confirmed,
        deposit_reserve,
        &active_deposit_position.market,
        &signature,
        confirmed_slot,
        amount_i64,
    )
    .await?;

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "idle_vault_deposit_executed",
            "writesDecision": true,
            "writesCurrentPositions": true,
            "sendsTransactions": true,
            "deposit": idle_vault_deposit_request_json(vault, deposit_reserve, active_deposit_position, amount_raw, db_idle.as_ref(), options),
            "vault": vault_json(vault),
            "vaultUsdcAta": vault_usdc_ata.to_string(),
            "setupObligationBeforeDeposit": setup_obligation_before_deposit,
            "missingObligationSetup": missing_obligation_setup_result,
            "preparedDecision": idle_vault_deposit_decision_json(&decision),
            "confirmedDecision": idle_vault_deposit_decision_json(&confirmed),
            "policyDeposit": initial_deposit_policy_preview_json(&policy_plan.preview),
            "policyDepositTransaction": {
                "signature": signature,
                "submittedSlot": submitted_slot,
                "confirmedSlot": confirmed_slot,
                "simulationUnitsConsumed": policy_simulation_units_consumed,
                "transaction": transaction_packet_json(&policy_transaction_packet),
            },
            "lookupTableResolution": final_lookup_table_evidence,
            "reconciledSnapshotId": post_snapshot.id.as_i64(),
            "postConfirmReconcileReserves": post_reconcile_reserves,
            "postChainReconcile": chain_reconcile_preview_json(&post_preview),
            "idleVaultBalanceAfter": idle_balance_json(&idle_after),
            "partialPullRepair": repair,
        }))?
    );

    if options.opportunity_id.is_some() {
        Ok(Some(idle_in_process_route_result(
            SameMintRouteExecutionState::Executed,
            None,
            setup_lookup_table_phase.as_ref(),
            setup_provisioning_request_id,
            deposit_lookup_table_phase.as_ref(),
            deposit_provisioning_request_id,
        )))
    } else {
        Ok(None)
    }
}

fn idle_deposit_post_reconcile_reserves(
    options: &CliOptions,
    deposit_reserve: &str,
) -> Vec<String> {
    let mut reserves = Vec::new();
    push_unique_string(&mut reserves, deposit_reserve.to_owned());
    for reserve in &options.reconcile_reserves {
        push_unique_string(&mut reserves, reserve.clone());
    }
    reserves
}

fn idle_vault_deposit_blocker_messages(blockers: &[IdleVaultDepositBlocker]) -> Vec<String> {
    blockers
        .iter()
        .map(|blocker| blocker.message.clone())
        .collect()
}

fn idle_vault_deposit_source_sync_reasons(blockers: &[IdleVaultDepositBlocker]) -> Vec<String> {
    blockers
        .iter()
        .filter(|blocker| blocker.kind == IdleVaultDepositBlockerKind::SourceStale)
        .map(|blocker| blocker.message.clone())
        .collect()
}

fn idle_vault_deposit_has_only_source_sync_blockers(blockers: &[IdleVaultDepositBlocker]) -> bool {
    !blockers.is_empty()
        && blockers
            .iter()
            .all(|blocker| blocker.kind == IdleVaultDepositBlockerKind::SourceStale)
}

fn idle_vault_deposit_blocker_state(
    blockers: &[IdleVaultDepositBlocker],
) -> SameMintRouteExecutionState {
    if blockers.is_empty() {
        SameMintRouteExecutionState::Ready
    } else if blockers
        .iter()
        .any(|blocker| blocker.kind == IdleVaultDepositBlockerKind::Safety)
    {
        SameMintRouteExecutionState::Terminal
    } else if blockers
        .iter()
        .any(|blocker| blocker.kind == IdleVaultDepositBlockerKind::LookupTable)
    {
        SameMintRouteExecutionState::WaitingAlt
    } else {
        SameMintRouteExecutionState::Retry
    }
}

fn idle_vault_deposit_requires_lookup_table_provisioning(
    blockers: &[IdleVaultDepositBlocker],
) -> bool {
    idle_vault_deposit_blocker_state(blockers) == SameMintRouteExecutionState::WaitingAlt
}

async fn record_live_idle_vault_balance(
    client: &NeonSqlClient,
    vault: &SelectedVault,
    vault_usdc_ata: &str,
    preview: &ChainReconcilePreview,
    deposit_position: &ChainPositionSummary,
) -> Result<CurrentIdleTokenBalance, Box<dyn Error>> {
    let now = Utc::now();
    Ok(client
        .record_current_idle_token_balance(CurrentIdleTokenBalance {
            vault_id: vault.id,
            mint: USDC_MINT.to_string(),
            amount_raw: i64::try_from(deposit_position.vault_liquidity_amount_raw)
                .map_err(|_| "live idle vault USDC amount does not fit Postgres BIGINT")?,
            owner: vault.vault_pubkey.clone(),
            token_account: vault_usdc_ata.to_owned(),
            observed_slot: preview.observed_slot,
            observed_at: now,
            source_commitment: "confirmed".to_owned(),
            updated_at: now,
        })
        .await?)
}

fn live_idle_vault_balance_sync_conflict(
    balance: &CurrentIdleTokenBalance,
    vault: &SelectedVault,
    vault_usdc_ata: &str,
    preview: &ChainReconcilePreview,
    expected_amount_raw: i64,
) -> Option<String> {
    if balance.mint != USDC_MINT.to_string() {
        return Some(format!(
            "DB returned idle mint {}, expected USDC {}",
            balance.mint, USDC_MINT
        ));
    }
    if balance.amount_raw != expected_amount_raw {
        return Some(format!(
            "DB returned idle amount {}, expected live RPC amount {}",
            balance.amount_raw, expected_amount_raw
        ));
    }
    if balance.owner != vault.vault_pubkey {
        return Some(format!(
            "DB returned idle owner {}, expected vault {}",
            balance.owner, vault.vault_pubkey
        ));
    }
    if balance.token_account != vault_usdc_ata {
        return Some(format!(
            "DB returned idle token account {}, expected vault USDC ATA {}",
            balance.token_account, vault_usdc_ata
        ));
    }
    if balance.observed_slot < preview.observed_slot {
        return Some(format!(
            "DB returned idle observed slot {}, expected at least live RPC slot {}",
            balance.observed_slot, preview.observed_slot
        ));
    }
    if balance.source_commitment != "confirmed" {
        return Some(format!(
            "DB returned idle source commitment {}, expected confirmed",
            balance.source_commitment
        ));
    }
    None
}

fn idle_vault_deposit_request_json(
    vault: &SelectedVault,
    deposit_reserve: &str,
    deposit_position: &ChainPositionSummary,
    amount_raw: u64,
    db_idle: Option<&CurrentIdleTokenBalance>,
    options: &CliOptions,
) -> Value {
    json!({
        "kind": "idle_vault_deposit",
        "sourceKind": "idle_vault",
        "reserve": deposit_reserve,
        "market": deposit_position.market,
        "liquidityMint": USDC_MINT.to_string(),
        "amountRaw": amount_raw.to_string(),
        "idleVaultLiquidityAmountRaw": amount_raw.to_string(),
        "idleTokenAccount": deposit_position.vault_liquidity_ata,
        "liveIdleAmountRaw": deposit_position.vault_liquidity_amount_raw.to_string(),
        "dbIdle": db_idle.map(idle_balance_json),
        "expected": {
            "idleTokenAccount": options.expected_idle_token_account,
            "idleObservedSlot": options.expected_idle_observed_slot,
            "idleObservedAt": options.expected_idle_observed_at.map(|value| value.to_rfc3339()),
            "liquidityMint": options.expected_liquidity_mint,
            "amountRaw": options.expected_amount_raw,
            "targetApyBps": options.expected_target_apy_bps,
            "edgeBps": options.expected_edge_bps,
        },
        "vaultId": vault.id.as_i64(),
    })
}

fn idle_balance_json(balance: &CurrentIdleTokenBalance) -> Value {
    json!({
        "vaultId": balance.vault_id.as_i64(),
        "mint": balance.mint,
        "amountRaw": balance.amount_raw.to_string(),
        "owner": balance.owner,
        "tokenAccount": balance.token_account,
        "observedSlot": balance.observed_slot,
        "observedAt": balance.observed_at,
        "sourceCommitment": balance.source_commitment,
        "updatedAt": balance.updated_at,
    })
}

fn idle_vault_deposit_decision_json(decision: &RebalanceDecision) -> Value {
    json!({
        "id": decision.id.as_i64(),
        "vaultId": decision.vault_id.as_i64(),
        "status": decision.status.as_str(),
        "decisionReason": decision.decision_reason.as_str(),
        "sourceReserve": decision.source_reserve,
        "targetReserve": decision.target_reserve,
        "liquidityMint": decision.liquidity_mint,
        "amountRaw": decision.amount_raw.map(|amount| amount.to_string()),
        "sourceApyBps": decision.source_apy_bps,
        "targetApyBps": decision.target_apy_bps,
        "estimatedEdgeBps": decision.estimated_edge_bps,
        "signature": decision.signature,
        "submittedSlot": decision.submitted_slot,
        "confirmedSlot": decision.confirmed_slot,
        "postSnapshotId": decision.post_snapshot_id.map(SnapshotId::as_i64),
        "executionPlan": decision.execution_plan,
    })
}

async fn repair_idle_vault_deposit_partial_pull_history(
    client: &NeonSqlClient,
    vault: &SelectedVault,
    decision: &RebalanceDecision,
    target_reserve: &str,
    target_market: &str,
    deposit_signature: &str,
    confirmed_slot: i64,
    planned_amount_raw: i64,
) -> Result<Value, Box<dyn Error>> {
    let mut tx = client.pool().begin().await?;
    let app_tables_exist: bool = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT to_regclass('loyal_yield.user_yield_position_deposits') IS NOT NULL
           AND to_regclass('loyal_yield.user_yield_positions') IS NOT NULL
           AND to_regclass('loyal_yield.user_yield_position_holding_events') IS NOT NULL
        "#,
    )
    .fetch_one(&mut *tx)
    .await?;

    let target_row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, wallet, token_mint, vault_token_ata
        FROM loyal_yield.balance_sweep_targets
        WHERE settings = $1
          AND vault_index = $2
          AND vault_pubkey = $3
          AND token_mint = $4
        ORDER BY active DESC, last_seen_at DESC, id DESC
        LIMIT 1
        "#,
    )
    .bind(&vault.settings)
    .bind(vault.vault_index)
    .bind(&vault.vault_pubkey)
    .bind(USDC_MINT.to_string())
    .fetch_optional(&mut *tx)
    .await?;
    let Some(target_row) = target_row else {
        tx.commit().await?;
        return Ok(json!({
            "matchedPartialPullCount": 0,
            "matchedPartialPullAmountRaw": "0",
            "balanceSweepTargetFound": false,
            "appHistoryRepair": "skipped_no_balance_sweep_target",
        }));
    };
    let target_id: i64 = target_row.try_get("id")?;
    let wallet: String = target_row.try_get("wallet")?;
    let vault_token_ata: String = target_row.try_get("vault_token_ata")?;

    let execution_rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, amount_raw, signature
        FROM loyal_yield.balance_sweep_executions
        WHERE target_id = $1
          AND token_mint = $2
          AND COALESCE(destination_token_ata, destination_vault_ata) = $3
          AND decoded_evidence->>'status' = 'partial_executed_pull_top_up_blocked'
          AND decoded_evidence->>'idleVaultDepositDecisionId' IS NULL
        ORDER BY slot ASC, id ASC
        FOR UPDATE
        "#,
    )
    .bind(target_id)
    .bind(USDC_MINT.to_string())
    .bind(&vault_token_ata)
    .fetch_all(&mut *tx)
    .await?;

    let mut matched_ids = Vec::new();
    let mut matched_signatures = Vec::new();
    let mut matched_amount_raw = 0_i64;
    for row in execution_rows {
        let amount: i64 = row.try_get("amount_raw")?;
        if matched_amount_raw + amount > planned_amount_raw {
            break;
        }
        matched_amount_raw += amount;
        matched_ids.push(row.try_get::<i64, _>("id")?);
        matched_signatures.push(row.try_get::<String, _>("signature")?);
        if matched_amount_raw == planned_amount_raw {
            break;
        }
    }

    if matched_ids.is_empty() {
        tx.commit().await?;
        return Ok(json!({
            "matchedPartialPullCount": 0,
            "matchedPartialPullAmountRaw": "0",
            "balanceSweepTargetFound": true,
            "appHistoryRepair": "skipped_no_matching_partial_pull",
        }));
    }

    loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.balance_sweep_executions
        SET
            decoded_evidence = COALESCE(decoded_evidence, '{}'::jsonb)
              || jsonb_build_object(
                    'previousStatus', decoded_evidence->>'status',
                    'status', 'partial_executed_pull_idle_vault_deposited',
                    'idleVaultDepositDecisionId', $2::text,
                    'kaminoDepositSignature', $3,
                    'kaminoDepositSlot', $4::text,
                    'idleVaultDepositAmountRaw', $5::text
                 ),
            decoded_at = now()
        WHERE id = ANY($1)
        "#,
    )
    .bind(&matched_ids)
    .bind(decision.id.as_i64())
    .bind(deposit_signature)
    .bind(confirmed_slot)
    .bind(planned_amount_raw)
    .execute(&mut *tx)
    .await?;

    let mut app_history_repair = json!("skipped_app_tables_missing");
    if app_tables_exist {
        app_history_repair = repair_idle_vault_deposit_app_history_in_tx(
            &mut tx,
            vault,
            target_reserve,
            target_market,
            &wallet,
            deposit_signature,
            confirmed_slot,
            matched_amount_raw,
            decision,
        )
        .await?;
    }

    tx.commit().await?;
    Ok(json!({
        "matchedPartialPullCount": matched_ids.len(),
        "matchedPartialPullIds": matched_ids,
        "matchedPartialPullSignatures": matched_signatures,
        "matchedPartialPullAmountRaw": matched_amount_raw.to_string(),
        "plannedAmountRaw": planned_amount_raw.to_string(),
        "balanceSweepTargetFound": true,
        "appHistoryRepair": app_history_repair,
    }))
}

async fn repair_idle_vault_deposit_app_history_in_tx(
    tx: &mut loyal_yield_orchestrator::sqlx::Transaction<
        '_,
        loyal_yield_orchestrator::sqlx::Postgres,
    >,
    vault: &SelectedVault,
    target_reserve: &str,
    target_market: &str,
    wallet: &str,
    deposit_signature: &str,
    confirmed_slot: i64,
    principal_delta_raw: i64,
    decision: &RebalanceDecision,
) -> Result<Value, Box<dyn Error>> {
    let deposit_row = loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.user_yield_position_deposits (
            deposit_signature,
            policy_signature,
            confirmed_slot,
            wallet_address,
            smart_account_address,
            settings,
            vault_index,
            vault_pubkey,
            policy_id,
            policy_account,
            policy_seed,
            target_reserve,
            market,
            liquidity_mint,
            target_supply_apy_bps,
            deposit_mint,
            principal_amount_raw,
            confirmed_at,
            created_at
        )
        VALUES ($1, $1, $2, $3, $4, $5, $6, $4, $7, $8, $7, $9, $10, $11, $12, $11, $13, now(), now())
        ON CONFLICT (deposit_signature) DO NOTHING
        RETURNING id
        "#,
    )
    .bind(deposit_signature)
    .bind(confirmed_slot)
    .bind(wallet)
    .bind(&vault.vault_pubkey)
    .bind(&vault.settings)
    .bind(vault.vault_index)
    .bind(vault.policy_seed)
    .bind(&vault.policy_account)
    .bind(target_reserve)
    .bind(target_market)
    .bind(USDC_MINT.to_string())
    .bind(decision.target_apy_bps)
    .bind(principal_delta_raw)
    .fetch_optional(&mut **tx)
    .await?;

    let Some(deposit_row) = deposit_row else {
        return Ok(json!({
            "status": "duplicate_deposit_signature",
            "depositSignature": deposit_signature,
        }));
    };
    let deposit_id: i64 = deposit_row.try_get("id")?;
    let existing = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, current_amount_raw, principal_amount_raw, current_reserve, current_liquidity_mint
        FROM loyal_yield.user_yield_positions
        WHERE settings = $1
          AND vault_index = $2
          AND wallet_address = $3
          AND status::text = 'active'
        ORDER BY updated_at DESC, id DESC
        LIMIT 1
        FOR UPDATE
        "#,
    )
    .bind(&vault.settings)
    .bind(vault.vault_index)
    .bind(wallet)
    .fetch_optional(&mut **tx)
    .await?;

    let observed_current_amount = decision.amount_raw.unwrap_or(principal_delta_raw);
    let (position_id, event_type, next_amount_raw, next_principal_raw, holding_delta_raw) =
        if let Some(existing) = existing {
            let position_id: i64 = existing.try_get("id")?;
            let current_amount_raw: i64 = existing.try_get("current_amount_raw")?;
            let principal_amount_raw: i64 = existing.try_get("principal_amount_raw")?;
            let current_reserve: String = existing.try_get("current_reserve")?;
            let current_liquidity_mint: String = existing.try_get("current_liquidity_mint")?;
            let same_current_holding = current_reserve == target_reserve
                && current_liquidity_mint == USDC_MINT.to_string();
            let next_amount_raw = if same_current_holding {
                observed_current_amount
            } else {
                current_amount_raw
            };
            let next_principal_raw = principal_amount_raw + principal_delta_raw;
            let holding_delta_raw = if same_current_holding {
                Some(next_amount_raw - current_amount_raw)
            } else {
                None
            };
            loyal_yield_orchestrator::sqlx::query(
                r#"
                UPDATE loyal_yield.user_yield_positions
                SET
                    deposit_mint = $2,
                    initial_liquidity_mint = $2,
                    initial_market = $3,
                    last_confirmed_slot = $4,
                    last_deposit_signature = $5,
                    policy_account = $6,
                    policy_id = $7,
                    policy_seed = $7,
                    principal_amount_raw = $8,
                    smart_account_address = $9,
                    status = 'active'::loyal_yield.yield_position_status,
                    updated_at = now(),
                    vault_pubkey = $9,
                    wallet_address = $10
                WHERE id = $1
                "#,
            )
            .bind(position_id)
            .bind(USDC_MINT.to_string())
            .bind(target_market)
            .bind(confirmed_slot)
            .bind(deposit_signature)
            .bind(&vault.policy_account)
            .bind(vault.policy_seed)
            .bind(next_principal_raw)
            .bind(&vault.vault_pubkey)
            .bind(wallet)
            .execute(&mut **tx)
            .await?;
            (
                position_id,
                "deposit_top_up",
                next_amount_raw,
                next_principal_raw,
                holding_delta_raw,
            )
        } else {
            let row = loyal_yield_orchestrator::sqlx::query(
                r#"
                INSERT INTO loyal_yield.user_yield_positions (
                    wallet_address,
                    smart_account_address,
                    settings,
                    vault_index,
                    vault_pubkey,
                    policy_id,
                    policy_account,
                    policy_seed,
                    initial_reserve,
                    initial_market,
                    initial_liquidity_mint,
                    initial_supply_apy_bps,
                    deposit_mint,
                    principal_amount_raw,
                    current_reserve,
                    current_market,
                    current_liquidity_mint,
                    current_amount_raw,
                    current_observed_slot,
                    current_observed_at,
                    first_deposit_signature,
                    last_deposit_signature,
                    last_confirmed_slot,
                    status,
                    created_at,
                    updated_at
                )
                VALUES ($1, $2, $3, $4, $2, $5, $6, $5, $7, $8, $9, $10, $9, $11, $7, $8, $9, $12, $13, now(), $14, $14, $13, 'active'::loyal_yield.yield_position_status, now(), now())
                RETURNING id
                "#,
            )
            .bind(wallet)
            .bind(&vault.vault_pubkey)
            .bind(&vault.settings)
            .bind(vault.vault_index)
            .bind(vault.policy_seed)
            .bind(&vault.policy_account)
            .bind(target_reserve)
            .bind(target_market)
            .bind(USDC_MINT.to_string())
            .bind(decision.target_apy_bps)
            .bind(principal_delta_raw)
            .bind(observed_current_amount)
            .bind(confirmed_slot)
            .bind(deposit_signature)
            .fetch_one(&mut **tx)
            .await?;
            (
                row.try_get("id")?,
                "deposit_initialized",
                observed_current_amount,
                principal_delta_raw,
                Some(principal_delta_raw),
            )
        };

    let event_row = loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.user_yield_position_holding_events (
            position_id,
            event_type,
            reserve,
            market,
            liquidity_mint,
            amount_raw,
            principal_delta_raw,
            holding_delta_raw,
            observed_slot,
            observed_at,
            source_signature,
            source_deposit_id,
            source_rebalance_decision_id,
            created_at
        )
        VALUES ($1, $2::text::loyal_yield.user_yield_holding_event_type, $3, $4, $5, $6, $7, $8, $9, now(), $10, $11, $12, now())
        RETURNING id
        "#,
    )
    .bind(position_id)
    .bind(event_type)
    .bind(target_reserve)
    .bind(target_market)
    .bind(USDC_MINT.to_string())
    .bind(next_amount_raw)
    .bind(principal_delta_raw)
    .bind(holding_delta_raw)
    .bind(confirmed_slot)
    .bind(deposit_signature)
    .bind(deposit_id)
    .bind(decision.id.as_i64())
    .fetch_one(&mut **tx)
    .await?;
    let event_id: i64 = event_row.try_get("id")?;

    loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.user_yield_positions
        SET
            current_amount_raw = $2,
            current_liquidity_mint = $3,
            current_market = $4,
            current_observed_at = now(),
            current_observed_slot = $5,
            current_reserve = $6,
            last_holding_event_id = $7,
            last_confirmed_slot = $5,
            last_deposit_signature = $8,
            principal_amount_raw = $9,
            status = 'active'::loyal_yield.yield_position_status,
            updated_at = now()
        WHERE id = $1
        "#,
    )
    .bind(position_id)
    .bind(next_amount_raw)
    .bind(USDC_MINT.to_string())
    .bind(target_market)
    .bind(confirmed_slot)
    .bind(target_reserve)
    .bind(event_id)
    .bind(deposit_signature)
    .bind(next_principal_raw)
    .execute(&mut **tx)
    .await?;

    Ok(json!({
        "status": "repaired",
        "positionId": position_id,
        "depositId": deposit_id,
        "holdingEventId": event_id,
        "principalDeltaRaw": principal_delta_raw.to_string(),
        "nextPrincipalRaw": next_principal_raw.to_string(),
        "nextAmountRaw": next_amount_raw.to_string(),
    }))
}

async fn deactivate_vault_policy_after_full_withdraw(
    client: &NeonSqlClient,
    vault: &SelectedVault,
) -> Result<Value, Box<dyn Error>> {
    let mut tx = client.pool().begin().await?;
    let policy_row = loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.route_policies
        SET active = false, last_seen_at = now()
        WHERE policy_account = $1
        RETURNING id, active
        "#,
    )
    .bind(&vault.policy_account)
    .fetch_one(&mut *tx)
    .await?;
    let setup_policy_row = if let Some(setup_policy_account) = vault.setup_policy_account.as_ref() {
        Some(
            loyal_yield_orchestrator::sqlx::query(
                r#"
                UPDATE loyal_yield.route_policies
                SET active = false, last_seen_at = now()
                WHERE policy_account = $1
                RETURNING id, active
                "#,
            )
            .bind(setup_policy_account)
            .fetch_one(&mut *tx)
            .await?,
        )
    } else {
        None
    };
    let vault_row = loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.managed_vaults
        SET active = false, last_seen_at = now()
        WHERE id = $1
        RETURNING id, active
        "#,
    )
    .bind(vault.id.as_i64())
    .fetch_one(&mut *tx)
    .await?;
    tx.commit().await?;

    Ok(json!({
        "policyId": policy_row.try_get::<i64, _>("id")?,
        "policyActive": policy_row.try_get::<bool, _>("active")?,
        "setupPolicyId": match setup_policy_row.as_ref() {
            Some(row) => Value::from(row.try_get::<i64, _>("id")?),
            None => Value::Null,
        },
        "setupPolicyActive": match setup_policy_row.as_ref() {
            Some(row) => Value::from(row.try_get::<bool, _>("active")?),
            None => Value::Null,
        },
        "vaultId": vault_row.try_get::<i64, _>("id")?,
        "vaultActive": vault_row.try_get::<bool, _>("active")?,
    }))
}

async fn run_reconcile_current_positions_flow(
    options: &CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
) -> Result<(), Box<dyn Error>> {
    let snapshot = client
        .reconcile_vault(vault.id, chain_preview_reconciled_state(preview)?)
        .await?;
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "current_positions_reconciled",
            "writesDecision": false,
            "writesCurrentPositions": true,
            "sendsTransactions": false,
            "execute": options.execute,
            "vault": vault_json(vault),
            "requestedReserves": options.reconcile_reserves,
            "reconciledReserveCount": preview.positions.len(),
            "reconciledSnapshotId": snapshot.id.as_i64(),
            "chainReconcile": chain_reconcile_preview_json(preview),
        }))?
    );
    Ok(())
}

async fn run_full_reserve_withdraw_flow(
    options: &CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    policy_preflight: Option<&PolicyAccountPreflight>,
    withdraw_reserve: &str,
) -> Result<(), Box<dyn Error>> {
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    // Preview-only builds intentionally use no legacy lookup tables. Every
    // submitted phase is recompiled through the reusable resolver.
    let lookup_table_accounts = Vec::new();
    let signer = policy_keypair_from_env()?;
    let authority_signer = solana_testing_keypair_from_env()?;
    let authority_pubkey = Pubkey::from_str(&vault.authority)?;
    if authority_signer.pubkey() != authority_pubkey {
        return Err(format!(
            "SOLANA_TESTING_PK pubkey {} does not match policy authority {}",
            authority_signer.pubkey(),
            authority_pubkey
        )
        .into());
    }
    let settings_pubkey = Pubkey::from_str(&vault.settings)?;
    let policy_account_pubkey = Pubkey::from_str(&vault.policy_account)?;
    let setup_policy_account_pubkey = vault
        .setup_policy_account
        .as_deref()
        .map(Pubkey::from_str)
        .transpose()?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault index {} does not fit Squads account index",
            vault.vault_index
        )
    })?;
    let withdraw = chain_position_for_reserve(preview, withdraw_reserve)?;
    let withdraw_obligation_pubkey = Pubkey::from_str(&withdraw.obligation)?;
    let withdraw_reserve_pubkey = Pubkey::from_str(&withdraw.reserve)?;
    let withdraw_market_pubkey = Pubkey::from_str(&withdraw.market)?;
    let wallet_usdc_ata =
        derive_associated_token_address(&authority_signer.pubkey(), &USDC_MINT, &spl_token::ID);
    let vault_usdc_ata = derive_associated_token_address(&vault_pubkey, &USDC_MINT, &spl_token::ID);
    let authority_account_before = load_account_proof(&rpc, &authority_signer.pubkey())?;
    let (wallet_usdc_before_raw, wallet_usdc_before_exists) =
        load_spl_token_account_amount(&rpc, &wallet_usdc_ata, &USDC_MINT)?;
    let vault_usdc_ata_before = load_account_proof(&rpc, &vault_usdc_ata)?;
    let policy_account_before = load_account_proof(&rpc, &policy_account_pubkey)?;
    let setup_policy_account_before = setup_policy_account_pubkey
        .as_ref()
        .map(|pubkey| load_account_proof(&rpc, pubkey))
        .transpose()?;
    let vault_account_before = load_account_proof(&rpc, &vault_pubkey)?;
    let obligation_before = load_obligation_account_proof(
        &rpc,
        &withdraw_obligation_pubkey,
        &vault_pubkey,
        &withdraw_market_pubkey,
        &withdraw_reserve_pubkey,
    )?;

    let mut blockers = Vec::new();
    if !withdraw.obligation_exists {
        blockers.push(format!(
            "withdraw obligation account {} does not exist for reserve {}",
            withdraw.obligation, withdraw.reserve
        ));
    }
    if withdraw.amount_raw == 0 {
        blockers.push(format!(
            "withdraw obligation account {} has zero deposited amount for reserve {}",
            withdraw.obligation, withdraw.reserve
        ));
    }
    if !withdraw.vault_liquidity_token_account_exists {
        blockers.push(format!(
            "vault USDC ATA {} does not exist",
            withdraw.vault_liquidity_ata
        ));
    }
    if !policy_account_before.exists {
        blockers.push(format!(
            "policy account {} does not exist",
            vault.policy_account
        ));
    }

    let policy_plan = match build_full_main_usdc_withdraw_policy_plan(
        vault,
        preview,
        policy_preflight,
        signer.pubkey(),
        account_index,
        withdraw_reserve,
    ) {
        Ok(plan) => Some(plan),
        Err(error) => {
            blockers.push(safe_same_mint_operational_error(error.as_ref()));
            None
        }
    };
    let withdraw_transaction = if let Some(plan) = policy_plan.as_ref() {
        let mut instructions = plan.pre_instructions.clone();
        instructions.push(plan.instruction.clone());
        Some(build_signed_transaction(
            &rpc,
            signer.pubkey(),
            &instructions,
            &lookup_table_accounts,
            &[&signer],
            "full reserve USDC policy withdraw",
            if blockers.is_empty() {
                None
            } else {
                Some("withdraw simulation skipped because preflight blockers exist".to_owned())
            },
        )?)
    } else {
        None
    };
    let withdraw_lookup_table_phase = if let Some(plan) = policy_plan.as_ref() {
        let mut instructions = plan.pre_instructions.clone();
        instructions.push(plan.instruction.clone());
        let manifest = route_lookup_table_manifest(
            signer.pubkey(),
            &instructions,
            vault,
            &plan.lookup_table_requirements,
            &[],
        )?;
        Some(
            prepare_route_lookup_table_phase(
                client,
                &rpc,
                options,
                vault,
                withdraw_reserve,
                withdraw_reserve,
                "full_reserve_withdraw",
                same_mint_route_lookup_table_scope_for_reserves(
                    vault,
                    withdraw_reserve,
                    withdraw_reserve,
                ),
                signer.pubkey(),
                instructions,
                manifest,
                &[&signer],
                options.execute && blockers.is_empty(),
            )
            .await?,
        )
    } else {
        None
    };
    let wallet_recovery_transaction = Some(build_vault_usdc_recovery_transaction(
        &rpc,
        &lookup_table_accounts,
        settings_pubkey,
        &authority_signer,
        vault_pubkey,
        account_index,
        wallet_usdc_ata,
        vault_usdc_ata,
        withdraw.amount_raw,
        Some("wallet recovery simulation requires the Kamino withdraw to land first".to_owned()),
    )?);
    let policy_close_instruction = remove_policy_instruction(
        settings_pubkey,
        authority_signer.pubkey(),
        policy_account_pubkey,
    );
    let policy_close_transaction = Some(build_policy_transaction(
        &rpc,
        authority_signer.pubkey(),
        policy_close_instruction.clone(),
        &lookup_table_accounts,
        &authority_signer,
        "full withdraw policy close",
        if blockers.is_empty() {
            None
        } else {
            Some("policy close simulation skipped because preflight blockers exist".to_owned())
        },
    )?);
    let setup_policy_close_transaction =
        if let (Some(setup_policy_pubkey), Some(setup_policy_before)) = (
            setup_policy_account_pubkey.as_ref(),
            setup_policy_account_before.as_ref(),
        ) {
            if setup_policy_before.exists {
                let setup_policy_close_instruction = remove_policy_instruction(
                    settings_pubkey,
                    authority_signer.pubkey(),
                    *setup_policy_pubkey,
                );
                Some(build_policy_transaction(
                    &rpc,
                    authority_signer.pubkey(),
                    setup_policy_close_instruction.clone(),
                    &lookup_table_accounts,
                    &authority_signer,
                    "full withdraw setup policy close",
                    if blockers.is_empty() {
                        Some(
                            "setup policy close simulation waits until the route policy close lands"
                                .to_owned(),
                        )
                    } else {
                        Some(
                            "setup policy close simulation skipped because preflight blockers exist"
                                .to_owned(),
                        )
                    },
                )?)
            } else {
                None
            }
        } else {
            None
        };
    dedup_strings_in_place(&mut blockers);

    let preflight_recovery_plan = vault_usdc_recovery_instructions(
        settings_pubkey,
        authority_signer.pubkey(),
        vault_pubkey,
        account_index,
        wallet_usdc_ata,
        vault_usdc_ata,
        withdraw.amount_raw,
    )?;
    let preflight_recovery_manifest = route_lookup_table_manifest(
        authority_signer.pubkey(),
        preflight_recovery_plan.instructions(),
        vault,
        preflight_recovery_plan.lookup_table_requirements(),
        &[wallet_usdc_ata],
    )?;
    let (preflight_recovery_instructions, _) = preflight_recovery_plan.into_parts();
    let preflight_recovery_phase = prepare_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        withdraw_reserve,
        withdraw_reserve,
        "full_withdraw_wallet_recovery",
        same_mint_route_lookup_table_scope_for_reserves(vault, withdraw_reserve, withdraw_reserve),
        authority_signer.pubkey(),
        preflight_recovery_instructions,
        preflight_recovery_manifest,
        &[&authority_signer],
        false,
    )
    .await?;
    let preflight_policy_close_instructions = vec![policy_close_instruction.clone()];
    let preflight_policy_close_manifest = policy_lookup_table_manifest(
        authority_signer.pubkey(),
        &preflight_policy_close_instructions,
        vault,
        &[],
        &[policy_account_pubkey],
    )?;
    let preflight_policy_close_phase = prepare_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        withdraw_reserve,
        withdraw_reserve,
        "full_withdraw_policy_close",
        same_mint_route_lookup_table_scope_for_reserves(vault, withdraw_reserve, withdraw_reserve),
        authority_signer.pubkey(),
        preflight_policy_close_instructions,
        preflight_policy_close_manifest,
        &[&authority_signer],
        options.execute && blockers.is_empty(),
    )
    .await?;
    let preflight_setup_policy_close_phase = if let Some(setup_policy_pubkey) =
        setup_policy_account_pubkey.filter(|_| {
            setup_policy_account_before
                .as_ref()
                .is_some_and(|account| account.exists)
        }) {
        let instructions = vec![remove_policy_instruction(
            settings_pubkey,
            authority_signer.pubkey(),
            setup_policy_pubkey,
        )];
        let manifest = policy_lookup_table_manifest(
            authority_signer.pubkey(),
            &instructions,
            vault,
            &[],
            &[setup_policy_pubkey],
        )?;
        Some(
            prepare_route_lookup_table_phase(
                client,
                &rpc,
                options,
                vault,
                withdraw_reserve,
                withdraw_reserve,
                "full_withdraw_setup_policy_close",
                same_mint_route_lookup_table_scope_for_reserves(
                    vault,
                    withdraw_reserve,
                    withdraw_reserve,
                ),
                authority_signer.pubkey(),
                instructions,
                manifest,
                &[&authority_signer],
                options.execute && blockers.is_empty(),
            )
            .await?,
        )
    } else {
        None
    };

    if !options.execute {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "full_withdraw_reserve_dry_run",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "withdraw": {
                    "reserve": withdraw.reserve,
                    "market": withdraw.market,
                    "liquidityMint": USDC_MINT.to_string(),
                    "amountRaw": withdraw.amount_raw.to_string(),
                    "amountSemantics": "kamino_obligation_collateral_deposited_amount",
                },
                "vault": vault_json(vault),
                "chainReconcile": chain_reconcile_preview_json(preview),
                "policyPreflight": policy_route_preflight_json(vault, &ReserveMove {
                    source_reserve: KAMINO_MAIN_USDC_RESERVE.to_string(),
                    target_reserve: KAMINO_PRIME_USDC_RESERVE.to_owned(),
                }, policy_preflight),
                "preflightBlockers": blockers,
                "rentCleanupProof": {
                    "vaultBefore": account_proof_json(&vault_account_before),
                    "authorityBefore": account_proof_json(&authority_account_before),
                    "vaultUsdcAtaBefore": account_proof_json(&vault_usdc_ata_before),
                    "policyBefore": account_proof_json(&policy_account_before),
                    "setupPolicyBefore": setup_policy_account_before.as_ref().map(account_proof_json),
                    "withdrawObligationBefore": obligation_account_proof_json(&obligation_before),
                    "afterAvailable": false,
                    "expectedRefundRecipient": vault.vault_pubkey,
                },
                "walletRecovery": {
                    "wallet": authority_signer.pubkey().to_string(),
                    "walletUsdcAta": wallet_usdc_ata.to_string(),
                        "walletUsdcBeforeRaw": wallet_usdc_before_raw.to_string(),
                        "walletUsdcBeforeExists": wallet_usdc_before_exists,
                        "estimatedTransferAmountRaw": withdraw.amount_raw.to_string(),
                        "cleanupSigner": authority_signer.pubkey().to_string(),
                    },
                "policyWithdraw": policy_plan.as_ref().map(|plan| full_withdraw_policy_preview_json(&plan.preview)),
                "policyWithdrawTransaction": withdraw_transaction.as_ref().map(policy_transaction_json),
                "lookupTableResolution": withdraw_lookup_table_phase.as_ref().map(|phase| phase.resolution.evidence.clone()),
                "walletRecoveryTransaction": wallet_recovery_transaction.as_ref().map(policy_transaction_json),
                "policyClose": {
                        "policyAccount": vault.policy_account,
                        "settings": vault.settings,
                        "authority": authority_signer.pubkey().to_string(),
                        "kind": "squads_execute_settings_transaction_sync_policy_remove",
                    },
                "policyCloseTransaction": policy_close_transaction.as_ref().map(policy_transaction_json),
                "setupPolicyClose": setup_policy_account_before.as_ref().map(|account| json!({
                    "policyAccount": vault.setup_policy_account,
                    "settings": vault.settings,
                    "authority": authority_signer.pubkey().to_string(),
                    "kind": "squads_execute_settings_transaction_sync_policy_remove",
                    "policyExists": account.exists,
                })),
                "setupPolicyCloseTransaction": setup_policy_close_transaction.as_ref().map(policy_transaction_json),
                "cleanupLookupTableResolution": {
                    "walletRecovery": preflight_recovery_phase.resolution.evidence.clone(),
                    "policyClose": preflight_policy_close_phase.resolution.evidence.clone(),
                    "setupPolicyClose": preflight_setup_policy_close_phase.as_ref().map(|phase| phase.resolution.evidence.clone()),
                },
            }))?
        );
        return Ok(());
    }

    if !blockers.is_empty() {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "full_withdraw_reserve_preflight_blocked",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "preflightBlockers": blockers,
                "rentCleanupProof": {
                    "vaultBefore": account_proof_json(&vault_account_before),
                    "authorityBefore": account_proof_json(&authority_account_before),
                    "vaultUsdcAtaBefore": account_proof_json(&vault_usdc_ata_before),
                    "policyBefore": account_proof_json(&policy_account_before),
                    "setupPolicyBefore": setup_policy_account_before.as_ref().map(account_proof_json),
                    "withdrawObligationBefore": obligation_account_proof_json(&obligation_before),
                },
                "policyWithdraw": policy_plan.as_ref().map(|plan| full_withdraw_policy_preview_json(&plan.preview)),
                "policyWithdrawTransaction": withdraw_transaction.as_ref().map(policy_transaction_json),
                "lookupTableResolution": withdraw_lookup_table_phase.as_ref().map(|phase| phase.resolution.evidence.clone()),
                "walletRecoveryTransaction": wallet_recovery_transaction.as_ref().map(policy_transaction_json),
                "policyCloseTransaction": policy_close_transaction.as_ref().map(policy_transaction_json),
                "setupPolicyCloseTransaction": setup_policy_close_transaction.as_ref().map(policy_transaction_json),
                "cleanupLookupTableResolution": {
                    "walletRecovery": preflight_recovery_phase.resolution.evidence.clone(),
                    "policyClose": preflight_policy_close_phase.resolution.evidence.clone(),
                    "setupPolicyClose": preflight_setup_policy_close_phase.as_ref().map(|phase| phase.resolution.evidence.clone()),
                },
            }))?
        );
        return Err("full reserve withdraw preflight blocked before live submit".into());
    }
    preflight_recovery_phase
        .resolution
        .require_deferred_simulation_coverage()
        .map_err(|error| {
            format!(
                "full-withdraw cleanup ALT coverage is incomplete before reserve withdrawal: {error}"
            )
        })?;
    let policy_plan = policy_plan.ok_or("full withdraw plan was not built")?;
    let withdraw_lookup_table_phase = withdraw_lookup_table_phase
        .as_ref()
        .ok_or("full withdraw lookup-table phase was not built")?;
    let submitted_withdraw = submit_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        withdraw_lookup_table_phase,
        &[&signer],
        &format!("full-reserve-withdraw:{}", withdraw_reserve),
    )
    .await?;
    let submitted_slot = submitted_withdraw.submitted_slot;
    let signature = submitted_withdraw.signature.clone();
    let confirmed_slot = submitted_withdraw.confirmed_slot;
    let (vault_usdc_after_withdraw_raw, vault_usdc_after_withdraw_exists) =
        load_spl_token_account_amount(&rpc, &vault_usdc_ata, &USDC_MINT)?;
    if !vault_usdc_after_withdraw_exists {
        return Err(format!(
            "vault USDC ATA {} is missing after Kamino withdraw",
            vault_usdc_ata
        )
        .into());
    }
    let wallet_recovery_plan = vault_usdc_recovery_instructions(
        settings_pubkey,
        authority_signer.pubkey(),
        vault_pubkey,
        account_index,
        wallet_usdc_ata,
        vault_usdc_ata,
        vault_usdc_after_withdraw_raw,
    )?;
    let wallet_recovery_manifest = route_lookup_table_manifest(
        authority_signer.pubkey(),
        wallet_recovery_plan.instructions(),
        vault,
        wallet_recovery_plan.lookup_table_requirements(),
        &[wallet_usdc_ata],
    )?;
    let (wallet_recovery_instructions, _) = wallet_recovery_plan.into_parts();
    let wallet_recovery_phase = prepare_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        withdraw_reserve,
        withdraw_reserve,
        "full_withdraw_wallet_recovery",
        same_mint_route_lookup_table_scope_for_reserves(vault, withdraw_reserve, withdraw_reserve),
        authority_signer.pubkey(),
        wallet_recovery_instructions,
        wallet_recovery_manifest,
        &[&authority_signer],
        true,
    )
    .await?;
    if wallet_recovery_phase.resolution.route_fingerprint
        != preflight_recovery_phase.resolution.route_fingerprint
        || wallet_recovery_phase.resolution.requirements_fingerprint
            != preflight_recovery_phase.resolution.requirements_fingerprint
    {
        return Err(
            "full-withdraw wallet-recovery ALT requirements changed after reserve withdrawal"
                .into(),
        );
    }
    let submitted_wallet_recovery = submit_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        &wallet_recovery_phase,
        &[&authority_signer],
        &format!("full-withdraw-wallet-recovery:{}", withdraw_reserve),
    )
    .await?;
    let wallet_recovery_submitted_slot = submitted_wallet_recovery.submitted_slot;
    let wallet_recovery_signature = submitted_wallet_recovery.signature.clone();
    let wallet_recovery_confirmed_slot = submitted_wallet_recovery.confirmed_slot;

    let submitted_policy_close = submit_route_lookup_table_phase(
        client,
        &rpc,
        options,
        vault,
        &preflight_policy_close_phase,
        &[&authority_signer],
        &format!("full-withdraw-policy-close:{}", vault.policy_account),
    )
    .await?;
    let policy_close_submitted_slot = submitted_policy_close.submitted_slot;
    let policy_close_signature = submitted_policy_close.signature.clone();
    let policy_close_confirmed_slot = submitted_policy_close.confirmed_slot;
    let setup_policy_close_result =
        if let Some(setup_policy_phase) = preflight_setup_policy_close_phase.as_ref() {
            Some(
                submit_route_lookup_table_phase(
                    client,
                    &rpc,
                    options,
                    vault,
                    setup_policy_phase,
                    &[&authority_signer],
                    &format!(
                        "full-withdraw-setup-policy-close:{}",
                        vault.setup_policy_account.as_deref().unwrap_or("unknown")
                    ),
                )
                .await?,
            )
        } else {
            None
        };

    let post_preview = load_chain_reconcile_preview(
        &options.rpc_url,
        vault,
        &preview
            .positions
            .iter()
            .map(|position| position.reserve.clone())
            .collect::<Vec<_>>(),
    )?;
    let vault_account_after = load_account_proof(&rpc, &vault_pubkey)?;
    let obligation_after = load_obligation_account_proof(
        &rpc,
        &withdraw_obligation_pubkey,
        &vault_pubkey,
        &withdraw_market_pubkey,
        &withdraw_reserve_pubkey,
    )?;
    let authority_account_after = load_account_proof(&rpc, &authority_signer.pubkey())?;
    let (wallet_usdc_after_raw, wallet_usdc_after_exists) =
        load_spl_token_account_amount(&rpc, &wallet_usdc_ata, &USDC_MINT)?;
    let vault_usdc_ata_after = load_account_proof(&rpc, &vault_usdc_ata)?;
    let policy_account_after = load_account_proof(&rpc, &policy_account_pubkey)?;
    let setup_policy_account_after = setup_policy_account_pubkey
        .as_ref()
        .map(|pubkey| load_account_proof(&rpc, pubkey))
        .transpose()?;
    let snapshot = client
        .reconcile_vault(vault.id, chain_preview_reconciled_state(&post_preview)?)
        .await?;
    let inactive = deactivate_vault_policy_after_full_withdraw(client, vault).await?;

    let rent_refund_lamports =
        i128::from(vault_account_after.lamports) - i128::from(vault_account_before.lamports);
    let authority_lamports_delta = i128::from(authority_account_after.lamports)
        - i128::from(authority_account_before.lamports);
    let closed_obligation_lamports = i128::from(obligation_before.account.lamports);
    let closed_policy_lamports = i128::from(policy_account_before.lamports);
    let closed_setup_policy_lamports = setup_policy_account_before
        .as_ref()
        .map(|account| i128::from(account.lamports))
        .unwrap_or(0);
    let closed_vault_usdc_ata_lamports = i128::from(vault_usdc_ata_before.lamports);
    let wallet_usdc_delta = i128::from(wallet_usdc_after_raw) - i128::from(wallet_usdc_before_raw);
    let all_tracked_positions_zero = post_preview
        .positions
        .iter()
        .all(|position| position.amount_raw == 0);
    let all_tracked_obligations_closed = post_preview
        .positions
        .iter()
        .all(|position| !position.obligation_exists);
    let policy_withdraw_transaction_json = json!({
        "signature": signature.to_string(),
        "submittedSlot": submitted_slot,
        "confirmedSlot": confirmed_slot,
        "simulationUnitsConsumed": submitted_withdraw.simulation_units_consumed,
        "transaction": transaction_packet_json(&submitted_withdraw.transaction_packet),
    });
    let wallet_recovery_json = json!({
        "wallet": authority_signer.pubkey().to_string(),
        "cleanupSigner": authority_signer.pubkey().to_string(),
        "walletUsdcAta": wallet_usdc_ata.to_string(),
        "walletUsdcBeforeRaw": wallet_usdc_before_raw.to_string(),
        "walletUsdcBeforeExists": wallet_usdc_before_exists,
        "walletUsdcAfterRaw": wallet_usdc_after_raw.to_string(),
        "walletUsdcAfterExists": wallet_usdc_after_exists,
        "walletUsdcDeltaRaw": wallet_usdc_delta.to_string(),
        "vaultUsdcAfterWithdrawRaw": vault_usdc_after_withdraw_raw.to_string(),
        "vaultUsdcAtaClosed": vault_usdc_ata_before.exists && !vault_usdc_ata_after.exists,
    });
    let wallet_recovery_transaction_json = json!({
        "signature": wallet_recovery_signature.to_string(),
        "submittedSlot": wallet_recovery_submitted_slot,
        "confirmedSlot": wallet_recovery_confirmed_slot,
        "simulationUnitsConsumed": submitted_wallet_recovery.simulation_units_consumed,
        "transaction": transaction_packet_json(&submitted_wallet_recovery.transaction_packet),
        "lookupTableResolution": submitted_wallet_recovery.lookup_table_resolution,
    });
    let policy_close_json = json!({
        "policyAccount": vault.policy_account,
        "settings": vault.settings,
        "authority": authority_signer.pubkey().to_string(),
        "kind": "squads_execute_settings_transaction_sync_policy_remove",
        "policyClosed": policy_account_before.exists && !policy_account_after.exists,
    });
    let policy_close_transaction_json = json!({
        "signature": policy_close_signature.to_string(),
        "submittedSlot": policy_close_submitted_slot,
        "confirmedSlot": policy_close_confirmed_slot,
        "simulationUnitsConsumed": submitted_policy_close.simulation_units_consumed,
        "transaction": transaction_packet_json(&submitted_policy_close.transaction_packet),
        "lookupTableResolution": submitted_policy_close.lookup_table_resolution,
    });
    let setup_policy_close_json = match setup_policy_account_before.as_ref() {
        Some(before) => json!({
            "policyAccount": vault.setup_policy_account,
            "settings": vault.settings,
            "authority": authority_signer.pubkey().to_string(),
            "kind": "squads_execute_settings_transaction_sync_policy_remove",
            "policyClosed": setup_policy_account_after
                .as_ref()
                .map(|after| before.exists && !after.exists)
                .unwrap_or(false),
        }),
        None => Value::Null,
    };
    let setup_policy_close_transaction_json = match setup_policy_close_result.as_ref() {
        Some(submitted) => json!({
            "signature": submitted.signature,
            "submittedSlot": submitted.submitted_slot,
            "confirmedSlot": submitted.confirmed_slot,
            "simulationUnitsConsumed": submitted.simulation_units_consumed,
            "transaction": transaction_packet_json(&submitted.transaction_packet),
            "lookupTableResolution": submitted.lookup_table_resolution,
        }),
        None => Value::Null,
    };
    let position_cleanup_proof_json = json!({
        "allTrackedPositionsZero": all_tracked_positions_zero,
        "allTrackedObligationsClosed": all_tracked_obligations_closed,
        "inactiveRows": inactive,
    });
    let rent_cleanup_proof_json = json!({
        "vaultBefore": account_proof_json(&vault_account_before),
        "vaultAfter": account_proof_json(&vault_account_after),
        "authorityBefore": account_proof_json(&authority_account_before),
        "authorityAfter": account_proof_json(&authority_account_after),
        "authorityLamportsDelta": authority_lamports_delta.to_string(),
        "vaultUsdcAtaBefore": account_proof_json(&vault_usdc_ata_before),
        "vaultUsdcAtaAfter": account_proof_json(&vault_usdc_ata_after),
        "policyBefore": account_proof_json(&policy_account_before),
        "policyAfter": account_proof_json(&policy_account_after),
        "policyClosed": policy_account_before.exists && !policy_account_after.exists,
        "setupPolicyBefore": setup_policy_account_before.as_ref().map(account_proof_json),
        "setupPolicyAfter": setup_policy_account_after.as_ref().map(account_proof_json),
        "setupPolicyClosed": setup_policy_account_before
            .as_ref()
            .zip(setup_policy_account_after.as_ref())
            .map(|(before, after)| before.exists && !after.exists),
        "withdrawObligationBefore": obligation_account_proof_json(&obligation_before),
        "withdrawObligationAfter": obligation_account_proof_json(&obligation_after),
        "withdrawObligationClosed": obligation_before.account.exists && !obligation_after.account.exists,
        "rentRefundLamports": rent_refund_lamports.to_string(),
        "closedObligationLamports": closed_obligation_lamports.to_string(),
        "closedPolicyLamports": closed_policy_lamports.to_string(),
        "closedSetupPolicyLamports": closed_setup_policy_lamports.to_string(),
        "closedVaultUsdcAtaLamports": closed_vault_usdc_ata_lamports.to_string(),
        "refundRecipient": vault.vault_pubkey,
        "refundAtLeastClosedObligationLamports": rent_refund_lamports >= closed_obligation_lamports,
    });

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "full_withdraw_reserve_executed",
            "writesDecision": false,
            "writesCurrentPositions": true,
            "sendsTransactions": true,
            "withdraw": {
                "reserve": withdraw.reserve,
                "market": withdraw.market,
                "liquidityMint": USDC_MINT.to_string(),
                "amountRaw": withdraw.amount_raw.to_string(),
                "amountSemantics": "kamino_obligation_collateral_deposited_amount",
            },
            "vault": vault_json(vault),
            "policyWithdraw": full_withdraw_policy_preview_json(&policy_plan.preview),
            "policyWithdrawTransaction": policy_withdraw_transaction_json,
            "lookupTableResolution": submitted_withdraw.lookup_table_resolution,
            "walletRecovery": wallet_recovery_json,
            "walletRecoveryTransaction": wallet_recovery_transaction_json,
            "policyClose": policy_close_json,
            "policyCloseTransaction": policy_close_transaction_json,
            "setupPolicyClose": setup_policy_close_json,
            "setupPolicyCloseTransaction": setup_policy_close_transaction_json,
            "reconciledSnapshotId": snapshot.id.as_i64(),
            "postChainReconcile": chain_reconcile_preview_json(&post_preview),
            "positionCleanupProof": position_cleanup_proof_json,
            "rentCleanupProof": rent_cleanup_proof_json,
        }))?
    );

    Ok(())
}

fn build_vault_usdc_recovery_transaction(
    rpc: &RpcClient,
    lookup_table_accounts: &[AddressLookupTableAccount],
    settings: Pubkey,
    authority_signer: &dyn Signer,
    vault_pubkey: Pubkey,
    account_index: u8,
    wallet_usdc_ata: Pubkey,
    vault_usdc_ata: Pubkey,
    amount_raw: u64,
    simulation_skip_reason: Option<String>,
) -> Result<PolicyTransactionBuild, Box<dyn Error>> {
    let instruction_plan = vault_usdc_recovery_instructions(
        settings,
        authority_signer.pubkey(),
        vault_pubkey,
        account_index,
        wallet_usdc_ata,
        vault_usdc_ata,
        amount_raw,
    )?;

    build_signed_transaction(
        rpc,
        authority_signer.pubkey(),
        instruction_plan.instructions(),
        lookup_table_accounts,
        &[authority_signer],
        "full withdraw vault USDC recovery",
        simulation_skip_reason,
    )
}

#[allow(clippy::too_many_arguments)]
fn vault_usdc_recovery_instructions(
    settings: Pubkey,
    authority: Pubkey,
    vault_pubkey: Pubkey,
    account_index: u8,
    wallet_usdc_ata: Pubkey,
    vault_usdc_ata: Pubkey,
    amount_raw: u64,
) -> Result<YieldRouteInstructionPlan, Box<dyn Error>> {
    let mut inner_instructions = Vec::new();
    if amount_raw > 0 {
        inner_instructions.push(spl_token::instruction::transfer_checked(
            &spl_token::ID,
            &vault_usdc_ata,
            &USDC_MINT,
            &wallet_usdc_ata,
            &vault_pubkey,
            &[],
            amount_raw,
            6,
        )?);
    }
    inner_instructions.push(spl_token::instruction::close_account(
        &spl_token::ID,
        &vault_usdc_ata,
        &authority,
        &vault_pubkey,
        &[],
    )?);
    guard_lookup_table_mutations(
        &inner_instructions,
        "raw full-withdraw wallet-recovery inner instructions",
    )?;

    let mut transaction_accounts = Vec::new();
    let compiled_instructions = inner_instructions
        .into_iter()
        .map(|instruction| compile_squads_inner_instruction(&mut transaction_accounts, instruction))
        .collect::<Vec<_>>();
    let recovery_instruction = execute_sync_transaction_instruction(
        settings,
        authority,
        account_index,
        compiled_instructions,
        transaction_accounts,
    );
    let mut requirements = YieldRouteLookupTableRequirements::new(settings, vault_pubkey);
    requirements.add_vault_token_account(wallet_usdc_ata);
    requirements.add_vault_token_account(vault_usdc_ata);
    requirements.add_shared_liquidity_mint(USDC_MINT);
    requirements.add_infrastructure_accounts([
        spl_token::ID,
        ASSOCIATED_TOKEN_PROGRAM_ID,
        system_program::ID,
    ]);
    let mut plan = YieldRouteInstructionPlan::with_outer_context(requirements);
    plan.push_outer_instruction(create_associated_token_account_idempotent_instruction(
        authority,
        authority,
        USDC_MINT,
        spl_token::ID,
    ));
    plan.push_outer_instruction(recovery_instruction);
    Ok(plan)
}

fn build_policy_transaction(
    rpc: &RpcClient,
    payer: Pubkey,
    instruction: Instruction,
    lookup_table_accounts: &[AddressLookupTableAccount],
    signer: &dyn Signer,
    operation_label: &str,
    simulation_skip_reason: Option<String>,
) -> Result<PolicyTransactionBuild, Box<dyn Error>> {
    build_signed_transaction(
        rpc,
        payer,
        &[instruction],
        lookup_table_accounts,
        &[signer],
        operation_label,
        simulation_skip_reason,
    )
}

fn build_signed_transaction(
    rpc: &RpcClient,
    payer: Pubkey,
    instructions: &[Instruction],
    lookup_table_accounts: &[AddressLookupTableAccount],
    signers: &[&dyn Signer],
    operation_label: &str,
    simulation_skip_reason: Option<String>,
) -> Result<PolicyTransactionBuild, Box<dyn Error>> {
    guard_lookup_table_mutations(instructions, operation_label)?;
    let (blockhash, _last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::confirmed())?;
    let transaction = compile_versioned_transaction(
        payer,
        instructions,
        lookup_table_accounts,
        blockhash,
        signers,
    )?;
    let transaction_packet = transaction_packet_summary(&transaction, lookup_table_accounts)?;
    let best_case_single_lookup_table_packet =
        best_case_single_lookup_table_packet_summary(payer, instructions, blockhash, signers)?;
    let packet_error = if transaction_packet.fits_packet_data_size {
        None
    } else {
        Some(format!(
            "{operation_label} transaction is too large for one packet: {} > {} bytes",
            transaction_packet.packet_size_bytes, transaction_packet.packet_data_size_bytes
        ))
    };
    let simulation_skipped_reason = if let Some(reason) = simulation_skip_reason {
        Some(reason)
    } else if !transaction_packet.fits_packet_data_size {
        Some(format!(
            "serialized v0 transaction is {} bytes; Solana packet limit is {} bytes",
            transaction_packet.packet_size_bytes, transaction_packet.packet_data_size_bytes
        ))
    } else {
        None
    };
    let simulation = if simulation_skipped_reason.is_none() {
        Some(rpc.simulate_transaction(&transaction)?)
    } else {
        None
    };
    let simulation_error = simulation
        .as_ref()
        .and_then(|simulation| {
            simulation
                .value
                .err
                .as_ref()
                .map(|error| format!("{error:?}"))
        })
        .or(packet_error);
    let simulation_logs = simulation
        .as_ref()
        .map(|simulation| json!(simulation.value.logs))
        .unwrap_or(Value::Null);
    let simulation_units_consumed = simulation
        .as_ref()
        .and_then(|simulation| simulation.value.units_consumed);

    Ok(PolicyTransactionBuild {
        transaction,
        transaction_packet,
        best_case_single_lookup_table_packet,
        simulation_error,
        simulation_logs,
        simulation_skipped_reason,
        simulation_units_consumed,
    })
}

fn guard_lookup_table_mutations(
    instructions: &[Instruction],
    operation_label: &str,
) -> Result<(), Box<dyn Error>> {
    for instruction in instructions {
        if let Some(kind) = lookup_table_mutation_kind(instruction) {
            return Err(format!(
                "{operation_label} rejected Address Lookup Table {kind} instruction outside explicit provisioning mode"
            )
            .into());
        }
    }
    Ok(())
}

fn lookup_table_mutation_kind(instruction: &Instruction) -> Option<&'static str> {
    if instruction.program_id != address_lookup_table_program::id() {
        return None;
    }
    match bincode::deserialize::<address_lookup_table_instruction::ProgramInstruction>(
        &instruction.data,
    ) {
        Ok(address_lookup_table_instruction::ProgramInstruction::CreateLookupTable { .. }) => {
            Some("create")
        }
        Ok(address_lookup_table_instruction::ProgramInstruction::ExtendLookupTable { .. }) => {
            Some("extend")
        }
        Ok(address_lookup_table_instruction::ProgramInstruction::FreezeLookupTable) => {
            Some("freeze")
        }
        Ok(address_lookup_table_instruction::ProgramInstruction::DeactivateLookupTable) => {
            Some("deactivate")
        }
        Ok(address_lookup_table_instruction::ProgramInstruction::CloseLookupTable) => Some("close"),
        Err(_) => Some("unknown"),
    }
}

#[allow(clippy::too_many_arguments)]
fn policy_operation_preview_json(
    operation: &str,
    vault: &SelectedVault,
    settings: Pubkey,
    policy: Pubkey,
    vault_pubkey: Pubkey,
    authority_signer: Pubkey,
    delegated_signer: Pubkey,
    db_delegated_signer_matches: bool,
    universe: &YieldRouteUniverse,
    swap_lanes: &[SwapLane],
    setup: &YieldRouteActionSetup,
    transaction: &PolicyTransactionBuild,
    existing_decoded: Option<&DecodedPolicyAccount>,
) -> Result<Value, Box<dyn Error>> {
    let same_mint_route = setup.same_mint_route()?;
    let jupiter_route = setup.jupiter_route().ok();
    let loyal_hub_route = setup.loyal_hub_route().ok();
    Ok(json!({
        "operation": operation,
        "policyAccount": policy.to_string(),
        "settings": settings.to_string(),
        "vaultIndex": vault.vault_index,
        "vaultPubkey": vault_pubkey.to_string(),
        "authoritySigner": authority_signer.to_string(),
        "delegatedSigner": delegated_signer.to_string(),
        "dbDelegatedSignerMatches": db_delegated_signer_matches,
        "dbDelegatedSigners": vault.delegated_signers.clone(),
        "transaction": policy_transaction_packet_json(transaction),
        "simulationSkippedReason": transaction.simulation_skipped_reason.clone(),
        "constraintCount": setup.spec.constraint_count,
        "instructionCount": setup.spec.instruction_count,
        "stableMints": pubkeys_json(&universe.stable_mints),
        "kaminoMarkets": pubkeys_json(&universe.kamino_markets),
        "kaminoLiquidityMints": pubkeys_json(&universe.kamino_liquidity_mints),
        "templateStableMints": vault.stable_mints.clone(),
        "templateKaminoMarkets": vault.kamino_markets.clone(),
        "templateKaminoLiquidityMints": vault.kamino_liquidity_mints.clone(),
        "swapLanes": swap_lanes_json(swap_lanes),
        "storedSwapLanes": policy_swap_lanes_json(setup, swap_lanes)?,
        "sameMintConstraintIndexes": same_mint_route.instruction_constraint_indexes(),
        "jupiterConstraintIndexes": jupiter_route.as_ref().map(|route| route.instruction_constraint_indexes().to_vec()),
        "loyalHubConstraintIndexes": loyal_hub_route.as_ref().map(|route| route.instruction_constraint_indexes().to_vec()),
        "existingPolicyDecoded": existing_decoded.map(decoded_policy_account_json),
        "simulationError": transaction.simulation_error.clone(),
        "simulationLogs": transaction.simulation_logs.clone(),
        "simulationUnitsConsumed": transaction.simulation_units_consumed,
    }))
}

#[allow(clippy::too_many_arguments)]
fn setup_policy_operation_preview_json(
    operation: &str,
    vault: &SelectedVault,
    settings: Pubkey,
    policy: Pubkey,
    policy_seed: i64,
    vault_pubkey: Pubkey,
    authority_signer: Pubkey,
    delegated_signer: Pubkey,
    db_delegated_signer_matches: bool,
    universe: &YieldRouteUniverse,
    setup: &YieldRouteActionSetup,
    transaction: &PolicyTransactionBuild,
    existing_decoded: Option<&DecodedPolicyAccount>,
) -> Result<Value, Box<dyn Error>> {
    Ok(json!({
        "operation": operation,
        "policyAccount": policy.to_string(),
        "policySeed": policy_seed,
        "settings": settings.to_string(),
        "vaultIndex": vault.vault_index,
        "vaultPubkey": vault_pubkey.to_string(),
        "authoritySigner": authority_signer.to_string(),
        "delegatedSigner": delegated_signer.to_string(),
        "dbDelegatedSignerMatches": db_delegated_signer_matches,
        "dbDelegatedSigners": vault.delegated_signers.clone(),
        "transaction": policy_transaction_packet_json(transaction),
        "simulationSkippedReason": transaction.simulation_skipped_reason.clone(),
        "constraintCount": setup.spec.constraint_count,
        "instructionCount": setup.spec.instruction_count,
        "stableMints": pubkeys_json(&universe.stable_mints),
        "kaminoMarkets": pubkeys_json(&universe.kamino_markets),
        "kaminoLiquidityMints": pubkeys_json(&universe.kamino_liquidity_mints),
        "templateStableMints": vault.stable_mints.clone(),
        "templateKaminoMarkets": vault.kamino_markets.clone(),
        "templateKaminoLiquidityMints": vault.kamino_liquidity_mints.clone(),
        "initObligationConstraintIndex": setup.spec.constraint_count.saturating_sub(1),
        "existingPolicyDecoded": existing_decoded.map(decoded_policy_account_json),
        "simulationError": transaction.simulation_error.clone(),
        "simulationLogs": transaction.simulation_logs.clone(),
        "simulationUnitsConsumed": transaction.simulation_units_consumed,
    }))
}

#[cfg(test)]
fn missing_lookup_table_addresses(
    required_addresses: &[Pubkey],
    lookup_table_accounts: &[AddressLookupTableAccount],
) -> Vec<Pubkey> {
    let present = lookup_table_accounts
        .iter()
        .flat_map(|account| account.addresses.iter().copied())
        .collect::<BTreeSet<_>>();
    required_addresses
        .iter()
        .copied()
        .filter(|address| !present.contains(address))
        .collect()
}

#[allow(clippy::too_many_arguments)]
async fn resolve_and_compile_reusable_lookup_table_bundle(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    options: &CliOptions,
    vault: &SelectedVault,
    required_addresses: &BTreeSet<String>,
    observed_slot: i64,
    observed_slot_u64: u64,
    fee_payer: Pubkey,
    instructions: &[Instruction],
    blockhash: Hash,
    signers: &[&dyn Signer],
    fee_budget: Option<TransactionFeeBudget>,
) -> Result<CompiledLookupTableBundle, Box<dyn Error>> {
    let reusable_resolution = client
        .resolve_reusable_lookup_table_bundle(
            &options.cluster,
            vault.id,
            required_addresses.clone(),
            observed_slot,
            LOOKUP_TABLE_RESOLVER_EXACT_SEARCH_LIMIT,
        )
        .await?;
    let (reusable_candidates, reusable_accounts, mut reusable_failures) =
        verify_reusable_lookup_table_candidates(rpc, reusable_resolution.tables, observed_slot_u64);
    let (reusable_tables, reusable_missing_after_rpc) = minimal_verified_table_bundle(
        required_addresses,
        &reusable_candidates,
        LOOKUP_TABLE_RESOLVER_EXACT_SEARCH_LIMIT,
    )?;
    let reusable_missing = reusable_resolution
        .missing_addresses
        .union(&reusable_missing_after_rpc)
        .cloned()
        .collect::<BTreeSet<_>>();
    let mut reusable = compile_lookup_table_bundle(
        rpc,
        reusable_tables,
        reusable_missing,
        required_addresses.clone(),
        reusable_accounts,
        fee_payer,
        instructions,
        blockhash,
        signers,
        fee_budget,
    );
    reusable
        .verification_failures
        .append(&mut reusable_failures);
    Ok(reusable)
}

fn unavailable_reusable_lookup_table_bundle(
    required_addresses: &BTreeSet<String>,
    code: &'static str,
    reason: impl Into<String>,
    is_error: bool,
) -> CompiledLookupTableBundle {
    let reason = reason.into();
    CompiledLookupTableBundle {
        domain: ResolvedLookupTableBundle {
            tables: Vec::new(),
            required_addresses: required_addresses.clone(),
            missing_addresses: required_addresses.clone(),
            packet_fits: false,
            simulation_succeeded: false,
        },
        transaction: None,
        transaction_packet: None,
        simulation_units_consumed: None,
        compute_unit_limit: None,
        priority_fee_micro_lamports: None,
        compiled_fee_lamports: None,
        simulation_error: is_error.then(|| reason.clone()),
        verification_failures: vec![json!({
            "stage": "resolver",
            "code": code,
            "reason": reason,
        })],
    }
}

fn safe_lookup_table_resolution_error(error: &dyn std::fmt::Display) -> String {
    safe_same_mint_operational_error(error)
}

fn safe_same_mint_operational_error(error: &dyn std::fmt::Display) -> String {
    redacted_external_error(&error.to_string())
}

fn safe_same_mint_operational_error_with_context(
    context: &str,
    error: &dyn std::fmt::Display,
) -> String {
    redacted_external_error(&format!("{context}: {error}"))
}

fn same_mint_fatal_error_payload(error: &dyn std::fmt::Display) -> Value {
    json!({
        "event": "same_mint_route_worker_fatal",
        "error": safe_same_mint_operational_error(error),
    })
}

fn same_mint_decision_failure_reason(stable_code: &str, error: &dyn std::fmt::Display) -> String {
    safe_same_mint_operational_error_with_context(stable_code, error)
}

fn same_mint_readiness_rpc_failure(error: &dyn std::fmt::Display) -> String {
    safe_same_mint_operational_error_with_context("simulation_rpc_failed", error)
}

async fn resolve_route_lookup_tables(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    options: &CliOptions,
    vault: &SelectedVault,
    source_reserve: &str,
    target_reserve: &str,
    route_kind: &str,
    scope: &str,
    fee_payer: Pubkey,
    instructions: &[Instruction],
    manifest: &LookupTableManifest,
    signers: &[&dyn Signer],
) -> Result<RuntimeLookupTableResolution, Box<dyn Error>> {
    guard_lookup_table_mutations(instructions, "route lookup-table resolution")?;
    manifest.validate_against_instructions(fee_payer, instructions)?;

    let observed_slot_u64 = rpc.get_slot()?;
    let observed_slot = i64::try_from(observed_slot_u64)?;
    let required_addresses = manifest
        .lookup_eligible_addresses()
        .into_iter()
        .map(|address| address.to_string())
        .collect::<BTreeSet<_>>();
    let writable_account_keys = exact_writable_account_keys(fee_payer, instructions);
    let conflict_account_keys = semantic_route_conflict_keys(vault);
    let fee_budget = fleet_transaction_fee_budget(rpc, options, &writable_account_keys)?;
    let requirements_fingerprint = control_plane_lookup_table_manifest_hash(manifest);
    let route_fingerprint = stable_fingerprint(&[
        route_kind,
        &options.cluster,
        &vault.id.as_i64().to_string(),
        source_reserve,
        target_reserve,
    ]);
    let rollout = client
        .effective_lookup_table_rollout(&options.cluster, vault.id)
        .await?;
    let shared_catalog_validation = client
        .validate_shared_market_catalog_route(
            &options.cluster,
            shared_market_manifest_addresses(manifest),
        )
        .await?;
    let shared_catalog_covered =
        shared_catalog_validation.state == SharedMarketCatalogRouteValidationState::Covered;
    let (blockhash, last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::confirmed())?;
    let mut reusable_resolution_error_code = None;
    let (mut reusable, reusable_resolution_state) =
        match resolve_and_compile_reusable_lookup_table_bundle(
            client,
            rpc,
            options,
            vault,
            &required_addresses,
            observed_slot,
            observed_slot_u64,
            fee_payer,
            instructions,
            blockhash,
            signers,
            fee_budget,
        )
        .await
        {
            Ok(bundle) => (bundle, "resolved"),
            Err(error) => {
                let detail = safe_lookup_table_resolution_error(error.as_ref());
                reusable_resolution_error_code = Some("reusable_resolution_failed");
                let bundle = unavailable_reusable_lookup_table_bundle(
                    &required_addresses,
                    "reusable_resolution_failed",
                    detail,
                    true,
                );
                (bundle, "failed")
            }
        };
    let mut reusable_evidence = compiled_lookup_table_bundle_json(&reusable);
    if let Some(fields) = reusable_evidence.as_object_mut() {
        fields.insert(
            "resolutionState".to_owned(),
            json!(reusable_resolution_state),
        );
        if let Some(code) = reusable_resolution_error_code {
            fields.insert("resolutionErrorCode".to_owned(), json!(code));
        }
    }
    let reusable_table_ids = reusable
        .domain
        .tables
        .iter()
        .map(|table| table.table_id)
        .collect::<Vec<_>>();
    let reusable_missing_addresses = reusable.domain.missing_addresses.clone();
    let reusable_ready = reusable.domain.ready() && shared_catalog_covered;
    let reusable_compiled_message_size = reusable
        .transaction_packet
        .as_ref()
        .map(|packet| packet.packet_size_bytes);
    let reusable_packet_fits = reusable
        .transaction_packet
        .as_ref()
        .map(|packet| packet.fits_packet_data_size);
    let reusable_simulation_units_consumed = reusable.simulation_units_consumed;
    let reusable_simulation_error = reusable.simulation_error.clone();
    let blocker = if reusable_runtime_enabled(&rollout)
        && shared_catalog_covered
        && reusable.domain.missing_addresses.is_empty()
        && reusable_simulation_error.is_some()
    {
        reusable_simulation_error
            .as_deref()
            .map(route_simulation_blocker)
    } else {
        reusable_runtime_blocker(&rollout, shared_catalog_covered, reusable_ready)
    };
    let selection_kind = if blocker.is_none() {
        LookupTableSelectionKind::Reusable
    } else {
        LookupTableSelectionKind::Blocked
    };
    let selected_bundle = blocker.is_none().then(|| reusable.domain.clone());
    let selected_table_ids = selected_bundle
        .as_ref()
        .map(|bundle| bundle.tables.iter().map(|table| table.table_id).collect())
        .unwrap_or_else(Vec::new);
    let (active_binding_fingerprint, active_binding_id) = if selected_bundle.is_some() {
        active_lookup_table_binding_fingerprint(client, vault.id, &selected_table_ids).await?
    } else {
        (stable_fingerprint(&["reusable", "no-active-binding"]), None)
    };
    let selection_fingerprint = selected_bundle.as_ref().map(|bundle| {
        let mut parts = vec![
            requirements_fingerprint.clone(),
            active_binding_fingerprint.clone(),
            "reusable".to_owned(),
            format!(
                "shared-catalog:{}:{}",
                shared_catalog_validation
                    .catalog_revision_id
                    .map_or_else(|| "none".to_owned(), |id| id.to_string()),
                shared_catalog_validation
                    .desired_set_hash
                    .as_deref()
                    .unwrap_or("none")
            ),
        ];
        for table in &bundle.tables {
            parts.push(format!(
                "{}:{}:{}:{}:{}",
                table.table_id,
                table.table_address,
                table.mutation_epoch,
                table.usable_prefix_len,
                table.address_hash
            ));
        }
        stable_fingerprint_owned(&parts)
    });
    let route_lease_reference = selection_fingerprint
        .as_ref()
        .map(|fingerprint| format!("route-resolution:{fingerprint}"));

    let selected = blocker.is_none().then_some(&mut reusable);
    let (
        selected_transaction,
        selected_transaction_packet,
        selected_simulation_units_consumed,
        selected_compiled_fee_lamports,
    ) = if let Some(selected) = selected {
        (
            selected.transaction.take(),
            selected.transaction_packet.take(),
            selected.simulation_units_consumed,
            selected.compiled_fee_lamports,
        )
    } else {
        (None, None, None, None)
    };

    let evidence = json!({
        "mode": "active_reusable_resolver",
        "cluster": options.cluster,
        "scope": scope,
        "feePayer": fee_payer.to_string(),
        "routeFingerprint": route_fingerprint,
        "requirementsFingerprint": requirements_fingerprint,
        "observedSlot": observed_slot,
        "rollout": {
            "mode": rollout.rollout_mode.as_str(),
            "forceLegacy": rollout.force_legacy,
            "globalReason": rollout.global.as_ref().and_then(|control| control.reason.clone()),
            "vaultReason": rollout.vault.as_ref().and_then(|control| control.reason.clone()),
        },
        "selection": {
            "kind": selection_kind.as_str(),
            "blocker": blocker,
            "fingerprint": selection_fingerprint,
            "activeBindingFingerprint": active_binding_fingerprint,
            "tableIds": selected_table_ids,
        },
        "requiredAddresses": required_addresses.iter().cloned().collect::<Vec<_>>(),
        "writableAccountKeys": writable_account_keys,
        "conflictAccountKeys": conflict_account_keys,
        "reusable": reusable_evidence,
        "sharedMarketCatalog": shared_market_catalog_validation_json(&shared_catalog_validation),
        "typedManifest": lookup_table_manifest_json(manifest),
    });
    Ok(RuntimeLookupTableResolution {
        rollout,
        route_fingerprint,
        requirements_fingerprint,
        selection_fingerprint,
        route_lease_reference,
        active_binding_fingerprint,
        active_binding_id,
        selection_kind,
        blocker,
        selected_bundle,
        selected_transaction,
        selected_transaction_packet,
        selected_simulation_units_consumed,
        selected_compiled_fee_lamports,
        recent_blockhash: blockhash,
        last_valid_block_height: i64::try_from(last_valid_block_height)?,
        reusable_table_ids,
        required_addresses,
        writable_account_keys,
        conflict_account_keys,
        reusable_missing_addresses,
        reusable_ready,
        reusable_compiled_message_size,
        reusable_packet_fits,
        reusable_simulation_units_consumed,
        reusable_simulation_error,
        shared_catalog_covered,
        observed_slot,
        evidence,
    })
}

fn exact_writable_account_keys(fee_payer: Pubkey, instructions: &[Instruction]) -> Vec<String> {
    let mut writable = BTreeSet::from([fee_payer.to_string()]);
    for instruction in instructions {
        for account in &instruction.accounts {
            if account.is_writable {
                writable.insert(account.pubkey.to_string());
            }
        }
    }
    writable.into_iter().collect()
}

fn semantic_route_conflict_keys(vault: &SelectedVault) -> Vec<String> {
    let lane = vault.id.as_i64().rem_euclid(FLEET_SHARED_WRITE_LANE_COUNT);
    let mut keys = vec![
        format!("vault-write:{}", vault.vault_pubkey),
        format!("fleet-shared-write-lane:{lane:02}"),
    ];
    keys.sort_unstable();
    keys
}

fn fleet_transaction_fee_budget(
    rpc: &RpcClient,
    options: &CliOptions,
    writable_account_keys: &[String],
) -> Result<Option<TransactionFeeBudget>, String> {
    if options.opportunity_id.is_none() {
        return Ok(None);
    }
    let expected_cost_lamports = options
        .expected_cost_lamports
        .ok_or_else(|| "fleet opportunity is missing its durable fee cap".to_owned())?;
    let current_economic_fee_cap_lamports = options
        .current_economic_fee_cap_lamports
        .ok_or_else(|| "fleet opportunity is missing its fresh economic fee cap".to_owned())?;
    let effective_cost_lamports = expected_cost_lamports.min(current_economic_fee_cap_lamports);
    let max_total_fee_lamports = u64::try_from(effective_cost_lamports)
        .map_err(|_| "fleet opportunity has a negative effective fee cap".to_owned())?;
    if max_total_fee_lamports == 0 {
        return Err("fleet opportunity effective fee cap is zero".to_owned());
    }
    let addresses = writable_account_keys
        .iter()
        .filter_map(|key| Pubkey::from_str(key).ok())
        .take(128)
        .collect::<Vec<_>>();
    let mut recent = rpc
        .get_recent_prioritization_fees(&addresses)
        .map_err(|error| {
            format!(
                "recent prioritization fee sampling failed: {}",
                same_mint_readiness_rpc_failure(&error)
            )
        })?
        .into_iter()
        .map(|fee| fee.prioritization_fee)
        .collect::<Vec<_>>();
    recent.sort_unstable();
    let recent_priority_fee_micro_lamports = if recent.is_empty() {
        0
    } else {
        let index = (recent.len() * 75).div_ceil(100).saturating_sub(1);
        recent[index]
    };
    Ok(Some(TransactionFeeBudget {
        max_total_fee_lamports,
        recent_priority_fee_micro_lamports,
    }))
}

async fn prepare_queue_signed_route_handoff(
    client: &NeonSqlClient,
    route_runtime: Option<&SameMintRouteRuntime>,
    options: &CliOptions,
    current: RebalanceOpportunityRecord,
    current_market: &CurrentRouteMarketEconomics,
    resolution: &RuntimeLookupTableResolution,
    fee_only_shard_allowed: bool,
    observed_fee_payer_balance_lamports: Option<i64>,
    observed_fee_payer_balance_slot: Option<i64>,
    observed_fee_payer_balance_at: Option<DateTime<Utc>>,
) -> Result<QueueSignedRouteHandoff, Box<dyn Error>> {
    require_current_route_market_epoch(current_market, current.optimizer_epoch_id)?;
    if options.optimizer_epoch_id != Some(current_market.optimizer_epoch_id) {
        return Err(format!(
            "{CURRENT_MARKET_EPOCH_STALE_PREFIX} route options and current durable optimizer epoch diverged before signed persistence"
        )
        .into());
    }
    let (owner, fencing_token, expires_at) = match (
        options.opportunity_lease_owner.as_deref(),
        options.opportunity_fencing_token,
        current.lease_expires_at,
    ) {
        (Some(owner), Some(fencing_token), Some(expires_at)) => {
            (owner.to_owned(), fencing_token, expires_at)
        }
        _ => return Err("queue signing requires a complete live execute lease".into()),
    };
    let lease = RebalanceOpportunityLease {
        opportunity: current.clone(),
        claim_kind: RebalanceOpportunityClaimKind::Execute,
        owner: owner.clone(),
        fencing_token,
        expires_at,
    };
    client.validate_rebalance_opportunity_lease(&lease).await?;
    if resolution.writable_account_keys.is_empty() || resolution.conflict_account_keys.is_empty() {
        return Err("signed route has incomplete writable/conflict evidence".into());
    }
    let transaction = resolution
        .selected_transaction
        .as_ref()
        .ok_or("queue signing requires an exact selected transaction")?;
    let actual_fee_lamports = i64::try_from(
        resolution
            .selected_compiled_fee_lamports
            .ok_or("signed route is missing its simulation-verified compiled fee")?,
    )?;
    if actual_fee_lamports > current.estimated_cost_lamports {
        return Err(format!(
            "compiled route fee {actual_fee_lamports} lamports exceeds economic cap {}",
            current.estimated_cost_lamports
        )
        .into());
    }
    let signed_transaction = bincode::serialize(transaction)?;
    let signed_transaction_hash = format!("{:x}", Sha256::digest(&signed_transaction));
    let message_hash = format!(
        "{:x}",
        Sha256::digest(bincode::serialize(&transaction.message)?)
    );
    let transaction_signature = transaction
        .signatures
        .first()
        .ok_or("signed route transaction has no signature")?
        .to_string();
    let fee_payer = transaction
        .message
        .static_account_keys()
        .first()
        .ok_or("signed route transaction has no fee payer")?
        .to_string();
    let policy_fee_payer = policy_keypair_from_env()?.pubkey().to_string();
    let signer_count = usize::from(transaction.message.header().num_required_signatures);
    let static_signers = transaction
        .message
        .static_account_keys()
        .iter()
        .take(signer_count)
        .map(ToString::to_string)
        .collect::<BTreeSet<_>>();
    if !static_signers.contains(&policy_fee_payer) {
        return Err("fleet route is missing POLICY_KEYPAIR as delegated policy signer".into());
    }
    let (fee_payer_kind, selected_fee_payer_observation) = if fee_payer == policy_fee_payer {
        (RouteFeePayerKind::Policy, None)
    } else {
        if !fee_only_shard_allowed {
            return Err(
                "fee-only route payer is forbidden for setup, farm, rent, or idle work".into(),
            );
        }
        let fee_payer_pubkey = Pubkey::from_str(&fee_payer)?;
        if !route_fee_payer_keypairs_from_env()?
            .iter()
            .any(|keypair| keypair.pubkey() == fee_payer_pubkey)
        {
            return Err("fleet route fee payer has no exact mounted shard keypair".into());
        }
        let lamports = u64::try_from(
            observed_fee_payer_balance_lamports
                .ok_or("fleet route shard is missing its batched balance observation")?,
        )?;
        let context_slot = u64::try_from(
            observed_fee_payer_balance_slot
                .ok_or("fleet route shard is missing its balance observation context slot")?,
        )?;
        (
            RouteFeePayerKind::FeeOnlyShard,
            Some(FeePayerBalanceObservation {
                lamports,
                context_slot,
                observed_at: observed_fee_payer_balance_at
                    .ok_or("fleet route shard is missing its balance observation time")?,
            }),
        )
    };
    client
        .acquire_route_account_conflict_leases(
            &lease,
            &resolution.conflict_account_keys,
            expires_at,
        )
        .await?;
    let selection_fingerprint = resolution
        .selection_fingerprint
        .clone()
        .ok_or("signed route has no ALT selection fingerprint")?;
    let selected_bundle = resolution
        .selected_bundle
        .as_ref()
        .ok_or("signed route has no selected ALT bundle")?;
    let mutation_epochs = json!({
        "optimizerEpoch": {
            "id": current_market.optimizer_epoch_id,
            "fingerprint": current_market.optimizer_epoch_fingerprint,
            "expiresAt": current_market.optimizer_epoch_expires_at,
        },
        "marketRevalidation": {
            "fingerprint": current_market.fresh_market_fingerprint,
            "expiresAt": current_market.fresh_market_expires_at,
            "materialFrontierDisposition": current_market.material_frontier_disposition,
        },
        "tables": selected_bundle.tables.iter().map(|table| json!({
            "tableId": table.table_id,
            "tableAddress": table.table_address,
            "mutationEpoch": table.mutation_epoch,
            "usablePrefixLen": table.usable_prefix_len,
            "addressHash": table.address_hash,
        })).collect::<Vec<_>>()
    });
    let semantic_key = route_submission_semantic_key(current.id);
    client
        .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
            cluster: current.cluster.clone(),
            lease_kind: LookupTableUsageLeaseKind::PreparedTransaction,
            reference_key: semantic_key.clone(),
            route_lookup_table_ids: resolution.selected_table_ids(),
            vault_id: Some(current.vault_id),
            binding_id: resolution.active_binding_id,
            route_fingerprint: current.route_fingerprint.clone(),
            requirements_fingerprint: current.requirements_fingerprint.clone(),
            expires_at: Utc::now() + ChronoDuration::minutes(LOOKUP_TABLE_PREPARED_LEASE_MINUTES),
        })
        .await?;
    // Keep the balance snapshot that selected the shard all the way through
    // compilation, but refresh it immediately before locked DB admission when
    // build/lease work consumed most of the two-second admission window. The
    // refresh is normally avoided; if needed it targets only the bound payer
    // and preserves the optimizer's minimum context slot.
    let admission_fee_payer_observation = match selected_fee_payer_observation {
        Some(observation) if observation.observed_at < Utc::now() - ChronoDuration::seconds(1) => {
            let runtime = route_runtime
                .ok_or("fleet route shard admission refresh is missing its persistent runtime")?;
            let fee_payer_pubkey = Pubkey::from_str(&fee_payer)?;
            load_cached_fee_payer_balances(
                runtime,
                &[fee_payer_pubkey],
                options.optimizer_epoch_id,
                options
                    .optimizer_market_slot
                    .map(u64::try_from)
                    .transpose()?,
            )?
            .remove(&fee_payer_pubkey)
            .ok_or("fleet route shard payer is not a funded system account at admission")?
            .into()
        }
        observation => observation,
    };
    let (fee_payer_balance_lamports, fee_payer_balance_slot, fee_payer_balance_observed_at) =
        admission_fee_payer_observation
            .map(|observation| {
                Ok::<_, Box<dyn Error>>((
                    Some(i64::try_from(observation.lamports)?),
                    Some(i64::try_from(observation.context_slot)?),
                    Some(observation.observed_at),
                ))
            })
            .transpose()?
            .unwrap_or((None, None, None));
    Ok(QueueSignedRouteHandoff {
        lease,
        submission: SignedRouteSubmissionInput {
            cluster: current.cluster,
            semantic_key,
            opportunity_id: current.id,
            decision_id: None,
            signed_transaction,
            signed_transaction_hash,
            message_hash,
            transaction_signature,
            recent_blockhash: resolution.recent_blockhash.to_string(),
            last_valid_block_height: resolution.last_valid_block_height,
            source_snapshot_id: current.source_snapshot_id,
            optimizer_epoch_id: current_market.optimizer_epoch_id,
            alt_requirements_fingerprint: resolution.requirements_fingerprint.clone(),
            alt_selection_fingerprint: selection_fingerprint,
            alt_mutation_epochs: mutation_epochs,
            fee_payer,
            fee_payer_kind,
            fee_payer_balance_lamports,
            fee_payer_balance_slot,
            fee_payer_balance_observed_at,
            compiled_fee_lamports: actual_fee_lamports,
            writable_account_keys: resolution.writable_account_keys.clone(),
            conflict_account_keys: resolution.conflict_account_keys.clone(),
            executor_owner: owner,
            executor_fencing_token: fencing_token,
        },
    })
}

fn route_submission_semantic_key(opportunity_id: i64) -> String {
    format!("fleet-opportunity:{opportunity_id}")
}

fn verify_reusable_lookup_table_candidates(
    rpc: &RpcClient,
    raw_candidates: Vec<ResolverTableCandidate>,
    observed_slot: u64,
) -> (
    Vec<ResolverTableCandidate>,
    BTreeMap<i64, AddressLookupTableAccount>,
    Vec<Value>,
) {
    let table_keys = raw_candidates
        .iter()
        .filter_map(|candidate| Pubkey::from_str(&candidate.table_address).ok())
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect::<Vec<_>>();
    let fetched = get_multiple_accounts_batched(rpc, &table_keys, Some(observed_slot));
    let fetched_error = fetched.as_ref().err().map(|error| {
        safe_same_mint_operational_error_with_context("rpc_lookup_table_batch_load_failed", error)
    });
    let mut account_by_key = BTreeMap::new();
    if let Ok((values, _)) = fetched {
        for (key, (account, _)) in table_keys.into_iter().zip(values) {
            account_by_key.insert(key, account);
        }
    }
    let mut candidates = Vec::new();
    let mut accounts = BTreeMap::new();
    let mut failures = Vec::new();
    for mut candidate in raw_candidates {
        let table_id = candidate.table_id;
        let verified = match Pubkey::from_str(&candidate.table_address) {
            Ok(table_key) => match fetched_error.as_deref() {
                Some(error) => Err(error.to_owned()),
                None => verify_lookup_table_candidate_account(
                    &mut candidate,
                    observed_slot,
                    account_by_key.get(&table_key).and_then(Option::as_ref),
                ),
            },
            Err(error) => Err(format!("table address is not a public key: {error}")),
        };
        match verified {
            Ok(account) => {
                accounts.insert(table_id, account);
            }
            Err(reason) => {
                candidate.rpc_verified = false;
                candidate.usable = false;
                failures.push(json!({
                    "tableId": table_id,
                    "tableAddress": candidate.table_address,
                    "reason": safe_same_mint_operational_error(&reason),
                }));
            }
        }
        candidates.push(candidate);
    }
    (candidates, accounts, failures)
}

fn verify_lookup_table_candidate_account(
    candidate: &mut ResolverTableCandidate,
    observed_slot: u64,
    account: Option<&Account>,
) -> Result<AddressLookupTableAccount, String> {
    if !candidate.persisted_prefix_verified || !candidate.usable {
        return Err("persisted lookup-table prefix is not verified/usable".to_owned());
    }
    let table_key = Pubkey::from_str(&candidate.table_address)
        .map_err(|error| format!("table address is not a public key: {error}"))?;
    let account = account.ok_or_else(|| "lookup-table account is missing on RPC".to_owned())?;
    if account.owner != address_lookup_table_program::id() {
        return Err(format!(
            "lookup-table owner {} does not match {}",
            account.owner,
            address_lookup_table_program::id()
        ));
    }
    let table = AddressLookupTable::deserialize(&account.data)
        .map_err(|error| format!("lookup-table account decode failed: {error}"))?;
    let expected_authority = Pubkey::from_str(&candidate.expected_authority)
        .map_err(|error| format!("expected authority is not a public key: {error}"))?;
    if table.meta.authority != Some(expected_authority) {
        return Err(format!(
            "lookup-table authority {:?} does not match expected {}",
            table.meta.authority, expected_authority
        ));
    }
    if table.meta.deactivation_slot != u64::MAX {
        return Err(format!(
            "lookup table is deactivating/deactivated at slot {}",
            table.meta.deactivation_slot
        ));
    }
    if usize::from(candidate.usable_prefix_len) != candidate.ordered_usable_prefix.len() {
        return Err("persisted usable prefix length does not match ordered prefix".to_owned());
    }
    if !candidate
        .ordered_durable_addresses
        .starts_with(&candidate.ordered_usable_prefix)
    {
        return Err("durable lookup-table membership does not extend the usable prefix".to_owned());
    }
    let chain_addresses = table.addresses.iter().copied().collect::<Vec<_>>();
    let usable_prefix_len = if observed_slot > table.meta.last_extended_slot {
        chain_addresses.len()
    } else if observed_slot == table.meta.last_extended_slot {
        usize::from(table.meta.last_extended_slot_start_index)
    } else {
        0
    };
    let expected_prefix = candidate
        .ordered_usable_prefix
        .iter()
        .map(|address| {
            Pubkey::from_str(address)
                .map_err(|error| format!("persisted prefix address is invalid: {error}"))
        })
        .collect::<Result<Vec<_>, _>>()?;
    if expected_prefix.len() > usable_prefix_len {
        return Err(format!(
            "persisted prefix has {} addresses but only {} are warm at observed slot {}",
            expected_prefix.len(),
            usable_prefix_len,
            observed_slot
        ));
    }
    if chain_addresses.get(..expected_prefix.len()) != Some(expected_prefix.as_slice()) {
        return Err("RPC ordered prefix differs from persisted ordered usable prefix".to_owned());
    }
    let chain_address_strings = chain_addresses
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let persisted_full_matches =
        ordered_lookup_table_address_hash(&chain_address_strings) == candidate.address_hash;
    let anticipated_durable_full_matches =
        chain_address_strings == candidate.ordered_durable_addresses;
    if !persisted_full_matches && !anticipated_durable_full_matches {
        return Err(
            "RPC full ordered membership matches neither persisted state nor the exact durable pending suffix"
                .to_owned(),
        );
    }
    candidate.rpc_verified = true;
    candidate.usable = true;
    candidate.addresses = candidate.ordered_usable_prefix.iter().cloned().collect();
    Ok(AddressLookupTableAccount {
        key: table_key,
        addresses: expected_prefix,
    })
}

#[allow(clippy::too_many_arguments)]
fn compile_budgeted_fleet_transaction(
    rpc: &RpcClient,
    fee_payer: Pubkey,
    instructions: &[Instruction],
    lookup_table_accounts: &[AddressLookupTableAccount],
    blockhash: Hash,
    signers: &[&dyn Signer],
    measured_units: u64,
    budget: TransactionFeeBudget,
) -> Result<BudgetedFleetTransaction, String> {
    let padded_units = measured_units
        .saturating_mul(115)
        .div_ceil(100)
        .saturating_add(10_000)
        .clamp(100_000, 1_400_000);
    let compute_unit_limit = u32::try_from(padded_units)
        .map_err(|_| "measured compute limit exceeds Solana's u32 instruction field".to_owned())?;

    // Reserve the base signature/message fee first. The economic opportunity
    // cap is a hard ceiling; congestion may raise priority bidding only inside
    // the remaining budget.
    let baseline = compile_versioned_transaction(
        fee_payer,
        instructions,
        lookup_table_accounts,
        blockhash,
        signers,
    )
    .map_err(|error| format!("baseline fee compilation failed: {error}"))?;
    let base_fee = versioned_message_fee(rpc, &baseline.message)
        .map_err(|error| format!("baseline fee lookup failed: {error}"))?;
    let priority_budget_lamports = budget.max_total_fee_lamports.saturating_sub(base_fee);
    let capped_micro_lamports = u64::try_from(
        u128::from(priority_budget_lamports)
            .saturating_mul(1_000_000)
            .checked_div(u128::from(compute_unit_limit))
            .unwrap_or_default(),
    )
    .unwrap_or(u64::MAX);
    let priority_fee_micro_lamports = budget
        .recent_priority_fee_micro_lamports
        .min(capped_micro_lamports);

    #[allow(deprecated)]
    let mut budgeted_instructions = vec![
        ComputeBudgetInstruction::set_compute_unit_limit(compute_unit_limit),
        ComputeBudgetInstruction::set_compute_unit_price(priority_fee_micro_lamports),
    ];
    budgeted_instructions.extend_from_slice(instructions);
    let transaction = compile_versioned_transaction(
        fee_payer,
        &budgeted_instructions,
        lookup_table_accounts,
        blockhash,
        signers,
    )
    .map_err(|error| format!("budgeted v0 compilation failed: {error}"))?;
    let packet = transaction_packet_summary(&transaction, lookup_table_accounts)
        .map_err(|error| format!("budgeted packet measurement failed: {error}"))?;
    if !packet.fits_packet_data_size {
        return Err(format!(
            "budgeted v0 transaction is {} bytes; packet limit is {}",
            packet.packet_size_bytes, packet.packet_data_size_bytes
        ));
    }
    let simulation = rpc
        .simulate_transaction(&transaction)
        .map_err(|error| same_mint_readiness_rpc_failure(&error))?;
    if let Some(error) = simulation.value.err {
        return Err(format!(
            "budgeted simulation failed: {error:?}; logs: {}",
            simulation.value.logs.unwrap_or_default().join(" | ")
        ));
    }
    let simulation_units_consumed = simulation.value.units_consumed.unwrap_or(measured_units);
    let compiled_fee_lamports = versioned_message_fee(rpc, &transaction.message)
        .map_err(|error| format!("budgeted fee lookup failed: {error}"))?;
    if compiled_fee_lamports > budget.max_total_fee_lamports {
        return Err(format!(
            "budgeted route fee {compiled_fee_lamports} exceeds economic cap {}",
            budget.max_total_fee_lamports
        ));
    }
    Ok(BudgetedFleetTransaction {
        transaction,
        packet,
        simulation_units_consumed,
        compute_unit_limit,
        priority_fee_micro_lamports,
        compiled_fee_lamports,
    })
}

fn versioned_message_fee(rpc: &RpcClient, message: &VersionedMessage) -> Result<u64, String> {
    match message {
        VersionedMessage::Legacy(message) => rpc.get_fee_for_message(message),
        VersionedMessage::V0(message) => rpc.get_fee_for_message(message),
    }
    .map_err(|error| error.to_string())
}

#[allow(clippy::too_many_arguments)]
fn compile_lookup_table_bundle(
    rpc: &RpcClient,
    mut tables: Vec<ResolverTableCandidate>,
    mut missing_addresses: BTreeSet<String>,
    required_addresses: BTreeSet<String>,
    account_by_table_id: BTreeMap<i64, AddressLookupTableAccount>,
    fee_payer: Pubkey,
    instructions: &[Instruction],
    blockhash: Hash,
    signers: &[&dyn Signer],
    fee_budget: Option<TransactionFeeBudget>,
) -> CompiledLookupTableBundle {
    let mut lookup_table_accounts = tables
        .iter()
        .filter_map(|table| account_by_table_id.get(&table.table_id).cloned())
        .collect::<Vec<_>>();
    let mut transaction = None;
    let mut transaction_packet = None;
    let mut simulation_units_consumed = None;
    let mut compute_unit_limit = None;
    let mut priority_fee_micro_lamports = None;
    let mut compiled_fee_lamports = None;
    let mut simulation_error = None;
    let mut verification_failures = Vec::new();

    if missing_addresses.is_empty() {
        match compile_versioned_transaction(
            fee_payer,
            instructions,
            &lookup_table_accounts,
            blockhash,
            signers,
        ) {
            Ok(mut compiled) => {
                let contributing = contributing_lookup_table_keys(&compiled);
                tables.retain(|table| {
                    Pubkey::from_str(&table.table_address)
                        .is_ok_and(|address| contributing.contains(&address))
                });
                lookup_table_accounts.retain(|account| contributing.contains(&account.key));
                match compile_versioned_transaction(
                    fee_payer,
                    instructions,
                    &lookup_table_accounts,
                    blockhash,
                    signers,
                ) {
                    Ok(recompiled) => compiled = recompiled,
                    Err(error) => {
                        simulation_error = Some(format!(
                        "v0 recompilation after zero-contribution table removal failed: {error}"
                    ))
                    }
                }
                let covered = loaded_lookup_table_addresses(&compiled, &lookup_table_accounts);
                missing_addresses = required_addresses.difference(&covered).cloned().collect();
                if missing_addresses.is_empty() && simulation_error.is_none() {
                    match transaction_packet_summary(&compiled, &lookup_table_accounts) {
                        Ok(mut packet) => {
                            if packet.fits_packet_data_size {
                                match rpc.simulate_transaction(&compiled) {
                                    Ok(simulation) => {
                                        simulation_units_consumed = simulation.value.units_consumed;
                                        if let Some(error) = simulation.value.err {
                                            simulation_error = Some(format!(
                                                "simulation failed: {error:?}; logs: {}",
                                                simulation
                                                    .value
                                                    .logs
                                                    .unwrap_or_default()
                                                    .join(" | ")
                                            ));
                                        }
                                    }
                                    Err(error) => {
                                        simulation_error =
                                            Some(same_mint_readiness_rpc_failure(&error));
                                    }
                                }
                                if simulation_error.is_none() {
                                    match (fee_budget, simulation_units_consumed) {
                                        (Some(budget), Some(units_consumed)) => {
                                            match compile_budgeted_fleet_transaction(
                                                rpc,
                                                fee_payer,
                                                instructions,
                                                &lookup_table_accounts,
                                                blockhash,
                                                signers,
                                                units_consumed,
                                                budget,
                                            ) {
                                                Ok(budgeted) => {
                                                    compiled = budgeted.transaction;
                                                    packet = budgeted.packet;
                                                    simulation_units_consumed =
                                                        Some(budgeted.simulation_units_consumed);
                                                    compute_unit_limit =
                                                        Some(budgeted.compute_unit_limit);
                                                    priority_fee_micro_lamports =
                                                        Some(budgeted.priority_fee_micro_lamports);
                                                    compiled_fee_lamports =
                                                        Some(budgeted.compiled_fee_lamports);
                                                }
                                                Err(error) => simulation_error = Some(error),
                                            }
                                        }
                                        (Some(_), None) => {
                                            simulation_error = Some(
                                                "fleet simulation omitted unitsConsumed; refusing to sign without a measured compute budget"
                                                    .to_owned(),
                                            );
                                        }
                                        (None, _) => {
                                            compiled_fee_lamports =
                                                versioned_message_fee(rpc, &compiled.message).ok();
                                        }
                                    }
                                }
                            } else {
                                simulation_error = Some(format!(
                                    "serialized v0 transaction is {} bytes; packet limit is {}",
                                    packet.packet_size_bytes, packet.packet_data_size_bytes
                                ));
                            }
                            transaction_packet = Some(packet);
                            transaction = Some(compiled);
                        }
                        Err(error) => {
                            simulation_error =
                                Some(format!("transaction packet measurement failed: {error}"));
                        }
                    }
                }
            }
            Err(error) => {
                simulation_error = Some(format!("v0 compilation failed: {error}"));
            }
        }
    }
    if let Some(error) = simulation_error.as_ref() {
        verification_failures.push(json!({ "reason": error }));
    }
    let packet_fits = transaction_packet
        .as_ref()
        .is_some_and(|packet| packet.fits_packet_data_size);
    let simulation_succeeded = transaction.is_some()
        && packet_fits
        && simulation_error.is_none()
        && missing_addresses.is_empty();
    CompiledLookupTableBundle {
        domain: ResolvedLookupTableBundle {
            tables,
            required_addresses,
            missing_addresses,
            packet_fits,
            simulation_succeeded,
        },
        transaction,
        transaction_packet,
        simulation_units_consumed,
        compute_unit_limit,
        priority_fee_micro_lamports,
        compiled_fee_lamports,
        simulation_error,
        verification_failures,
    }
}

fn contributing_lookup_table_keys(transaction: &VersionedTransaction) -> BTreeSet<Pubkey> {
    match &transaction.message {
        VersionedMessage::V0(message) => message
            .address_table_lookups
            .iter()
            .map(|lookup| lookup.account_key)
            .collect(),
        VersionedMessage::Legacy(_) => BTreeSet::new(),
    }
}

fn loaded_lookup_table_addresses(
    transaction: &VersionedTransaction,
    lookup_table_accounts: &[AddressLookupTableAccount],
) -> BTreeSet<String> {
    let VersionedMessage::V0(message) = &transaction.message else {
        return BTreeSet::new();
    };
    let accounts = lookup_table_accounts
        .iter()
        .map(|account| (account.key, account))
        .collect::<BTreeMap<_, _>>();
    message
        .address_table_lookups
        .iter()
        .filter_map(|lookup| {
            accounts
                .get(&lookup.account_key)
                .copied()
                .map(|account| (lookup, account))
        })
        .flat_map(|(lookup, account)| {
            lookup
                .writable_indexes
                .iter()
                .chain(lookup.readonly_indexes.iter())
                .filter_map(|index| account.addresses.get(usize::from(*index)))
        })
        .map(ToString::to_string)
        .collect()
}

fn compiled_lookup_table_bundle_json(bundle: &CompiledLookupTableBundle) -> Value {
    json!({
        "kind": "reusable",
        "ready": bundle.domain.ready(),
        "packetFits": bundle.domain.packet_fits,
        "simulationSucceeded": bundle.domain.simulation_succeeded,
        "simulationUnitsConsumed": bundle.simulation_units_consumed,
        "computeUnitLimit": bundle.compute_unit_limit,
        "priorityFeeMicroLamports": bundle.priority_fee_micro_lamports,
        "compiledFeeLamports": bundle.compiled_fee_lamports,
        "simulationError": bundle.simulation_error,
        "requiredAddressCount": bundle.domain.required_addresses.len(),
        "missingAddresses": bundle.domain.missing_addresses.iter().cloned().collect::<Vec<_>>(),
        "tables": bundle.domain.tables.iter().map(|table| json!({
            "tableId": table.table_id,
            "tableAddress": table.table_address,
            "familyId": table.family_id,
            "allocationKind": table.allocation_kind.map(|kind| kind.as_str()),
            "generation": table.generation,
            "shardIndex": table.shard_index,
            "usablePrefixLength": table.usable_prefix_len,
            "mutationEpoch": table.mutation_epoch,
            "addressHash": table.address_hash,
            "lastVerifiedSlot": table.last_verified_slot,
            "persistedPrefixVerified": table.persisted_prefix_verified,
            "rpcVerified": table.rpc_verified,
            "contributesToCompiledMessage": true,
        })).collect::<Vec<_>>(),
        "transaction": bundle.transaction_packet.as_ref().map(transaction_packet_json),
        "verificationFailures": bundle.verification_failures,
    })
}

fn shared_market_catalog_validation_json(validation: &SharedMarketCatalogRouteValidation) -> Value {
    json!({
        "state": validation.state.as_str(),
        "catalogRevisionId": validation.catalog_revision_id,
        "catalogRevision": validation.catalog_revision,
        "desiredSetHash": validation.desired_set_hash,
        "readinessState": validation.readiness_state.map(|state| state.as_str()),
        "targetGeneration": validation.target_generation,
        "activeGeneration": validation.active_generation,
        "routeMissingAddresses": validation.route_missing_addresses,
        "semanticMismatchAddresses": validation.semantic_mismatch_addresses,
        "activeMissingAddresses": validation.active_missing_addresses,
        "activeExtraAddresses": validation.active_extra_addresses,
    })
}

async fn active_lookup_table_binding_fingerprint(
    client: &NeonSqlClient,
    vault_id: VaultId,
    selected_table_ids: &[i64],
) -> Result<(String, Option<i64>), Box<dyn Error>> {
    if selected_table_ids.is_empty() {
        return Ok((
            stable_fingerprint(&["reusable", "no-selected-tables"]),
            None,
        ));
    }
    let rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, route_lookup_table_id, manifest_id, binding_ordinal,
               lifecycle_state, active_from_slot, active_until_slot
        FROM loyal_yield.lookup_table_vault_bindings
        WHERE vault_id = $1 AND lifecycle_state = 'active'
          AND route_lookup_table_id = ANY($2)
        ORDER BY route_lookup_table_id, binding_ordinal, id
        "#,
    )
    .bind(vault_id.as_i64())
    .bind(selected_table_ids)
    .fetch_all(client.pool())
    .await?;
    let mut parts = vec![format!("vault:{}", vault_id.as_i64())];
    let mut binding_ids = Vec::new();
    for row in rows {
        let id: i64 = row.try_get("id")?;
        binding_ids.push(id);
        parts.push(format!(
            "{}:{}:{}:{}:{}:{:?}:{:?}",
            id,
            row.try_get::<i64, _>("route_lookup_table_id")?,
            row.try_get::<i64, _>("manifest_id")?,
            row.try_get::<i32, _>("binding_ordinal")?,
            row.try_get::<String, _>("lifecycle_state")?,
            row.try_get::<Option<i64>, _>("active_from_slot")?,
            row.try_get::<Option<i64>, _>("active_until_slot")?,
        ));
    }
    let binding_id = (binding_ids.len() == 1).then_some(binding_ids[0]);
    Ok((stable_fingerprint_owned(&parts), binding_id))
}

async fn persist_route_lookup_table_resolution(
    client: &NeonSqlClient,
    options: &CliOptions,
    vault: &SelectedVault,
    source_reserve: &str,
    target_reserve: &str,
    route_kind: &str,
    manifest: &LookupTableManifest,
    resolution: &RuntimeLookupTableResolution,
    acquire_route_lease: bool,
    request_provisioning: bool,
) -> Result<Option<i64>, Box<dyn Error>> {
    // A read-only inspection must not register readiness, provisioning demand, or
    // usage leases. Returning no provisioning request id is honest here: nothing
    // was requested, so nothing can be waited on.
    if options.read_only {
        return Ok(None);
    }
    let selected_table_ids = resolution.selected_table_ids();
    let selected_table_count = i32::try_from(selected_table_ids.len())?;
    let required_count = i32::try_from(resolution.required_addresses.len())?;
    let reusable_missing_count = i32::try_from(resolution.reusable_missing_addresses.len())?;
    let packet_size = resolution
        .reusable_compiled_message_size
        .map(i32::try_from)
        .transpose()?;
    let simulation_state = if resolution.reusable_compiled_message_size.is_none() {
        LookupTableSimulationState::NotRun
    } else if resolution.reusable_simulation_error.is_none() {
        LookupTableSimulationState::Succeeded
    } else {
        LookupTableSimulationState::Failed
    };
    let shared_family_id = resolution.selected_bundle.as_ref().and_then(|bundle| {
        bundle
            .tables
            .iter()
            .find(|table| table.allocation_kind == Some(LookupTableAllocationKind::SharedMarket))
            .and_then(|table| table.family_id)
    });
    client
        .upsert_lookup_table_readiness(LookupTableReadinessRecord {
            cluster: options.cluster.clone(),
            vault_id: vault.id,
            route_fingerprint: resolution.route_fingerprint.clone(),
            requirements_fingerprint: resolution.requirements_fingerprint.clone(),
            route_kind: route_kind.to_owned(),
            source_reserve: Some(source_reserve.to_owned()),
            target_reserve: Some(target_reserve.to_owned()),
            manifest_id: None,
            shared_family_id,
            vault_binding_id: resolution.active_binding_id,
            readiness_state: if resolution.reusable_ready {
                LookupTableReadinessStatus::Ready
            } else {
                LookupTableReadinessStatus::Incomplete
            },
            required_address_count: required_count,
            covered_address_count: required_count - reusable_missing_count,
            missing_addresses: json!(resolution
                .reusable_missing_addresses
                .iter()
                .cloned()
                .collect::<Vec<_>>()),
            legacy_table_ids: Vec::new(),
            reusable_table_ids: resolution.reusable_table_ids.clone(),
            compiled_message_size: packet_size,
            packet_limit: Some(i32::try_from(PACKET_DATA_SIZE)?),
            observed_slot: Some(resolution.observed_slot),
            observed_at: Utc::now(),
            selection_kind: Some(resolution.selection_kind),
            fallback_reason: resolution.blocker.clone(),
            rollout_mode: Some(resolution.rollout.rollout_mode),
            selected_table_ids: selected_table_ids.clone(),
            selected_table_count: Some(selected_table_count),
            packet_fits: resolution.reusable_packet_fits,
            simulation_state: Some(simulation_state),
            simulation_units_consumed: resolution
                .reusable_simulation_units_consumed
                .map(i64::try_from)
                .transpose()?,
            simulation_error: resolution.reusable_simulation_error.clone(),
            updated_at: Utc::now(),
        })
        .await?;

    let missing_vault_addresses = vault_manifest_addresses(manifest)
        .into_iter()
        .map(|address| address.address)
        .filter(|address| resolution.reusable_missing_addresses.contains(address))
        .collect::<BTreeSet<_>>();
    let provisioning_request_id = if request_provisioning
        && reusable_runtime_enabled(&resolution.rollout)
        && resolution.shared_catalog_covered
        && !missing_vault_addresses.is_empty()
    {
        Some(
            client
                .upsert_lookup_table_provisioning_request(LookupTableProvisioningRequestUpsert {
                    cluster: options.cluster.clone(),
                    vault_id: vault.id,
                    route_fingerprint: resolution.route_fingerprint.clone(),
                    requirements_fingerprint: resolution.requirements_fingerprint.clone(),
                    shared_manifest_id: None,
                    vault_manifest_id: None,
                    desired_shared_hash: Some(shared_market_manifest_hash(manifest)),
                    desired_vault_hash: Some(vault_manifest_hash(manifest)),
                    shared_addresses: shared_market_manifest_addresses(manifest),
                    vault_addresses: vault_manifest_addresses(manifest),
                })
                .await?
                .id,
        )
    } else {
        None
    };

    if acquire_route_lease {
        resolution.require_ready()?;
        client
            .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
                cluster: options.cluster.clone(),
                lease_kind: LookupTableUsageLeaseKind::RouteResolution,
                reference_key: resolution
                    .route_lease_reference
                    .clone()
                    .ok_or("route lookup-table lease reference is missing")?,
                route_lookup_table_ids: selected_table_ids,
                vault_id: Some(vault.id),
                binding_id: resolution.active_binding_id,
                route_fingerprint: Some(resolution.route_fingerprint.clone()),
                requirements_fingerprint: Some(resolution.requirements_fingerprint.clone()),
                expires_at: Utc::now() + ChronoDuration::minutes(LOOKUP_TABLE_ROUTE_LEASE_MINUTES),
            })
            .await?;
    }
    Ok(provisioning_request_id)
}

#[allow(clippy::too_many_arguments)]
async fn prepare_route_lookup_table_phase(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    options: &CliOptions,
    vault: &SelectedVault,
    source_reserve: &str,
    target_reserve: &str,
    route_kind: &'static str,
    scope: String,
    fee_payer: Pubkey,
    instructions: Vec<Instruction>,
    manifest: LookupTableManifest,
    signers: &[&dyn Signer],
    acquire_route_lease: bool,
) -> Result<RouteLookupTablePhase, Box<dyn Error>> {
    let resolution = resolve_route_lookup_tables(
        client,
        rpc,
        options,
        vault,
        source_reserve,
        target_reserve,
        route_kind,
        &scope,
        fee_payer,
        &instructions,
        &manifest,
        signers,
    )
    .await?;
    persist_route_lookup_table_resolution(
        client,
        options,
        vault,
        source_reserve,
        target_reserve,
        route_kind,
        &manifest,
        &resolution,
        acquire_route_lease,
        true,
    )
    .await?;
    Ok(RouteLookupTablePhase {
        route_kind,
        scope,
        source_reserve: source_reserve.to_owned(),
        target_reserve: target_reserve.to_owned(),
        instructions,
        manifest,
        resolution,
    })
}

async fn submit_route_lookup_table_phase(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    options: &CliOptions,
    vault: &SelectedVault,
    phase: &RouteLookupTablePhase,
    signers: &[&dyn Signer],
    prepared_reference_prefix: &str,
) -> Result<SubmittedLookupTablePhase, Box<dyn Error>> {
    let selection_fingerprint = phase
        .resolution
        .selection_fingerprint
        .as_deref()
        .ok_or("predecision lookup-table selection fingerprint is missing")?;
    let prepared_lease_reference = format!("{prepared_reference_prefix}:{selection_fingerprint}");
    let result = async {
        let mut presend = resolve_route_lookup_tables_immediately_before_send(
            client,
            rpc,
            options,
            vault,
            phase,
            &phase.instructions,
            &phase.manifest,
            signers,
            &prepared_lease_reference,
        )
        .await?;
        let transaction = presend
            .selected_transaction
            .take()
            .ok_or("pre-send lookup-table resolution is missing the compiled transaction")?;
        let transaction_packet = presend
            .selected_transaction_packet
            .take()
            .ok_or("pre-send lookup-table resolution is missing packet evidence")?;
        let simulation_units_consumed = presend.selected_simulation_units_consumed;
        let lookup_table_resolution = presend.evidence.clone();
        let submitted_slot = i64::try_from(rpc.get_slot()?)?;
        let signature = rpc.send_and_confirm_transaction(&transaction)?.to_string();
        let confirmed_slot = i64::try_from(rpc.get_slot()?)?;
        Ok::<_, Box<dyn Error>>(SubmittedLookupTablePhase {
            signature,
            submitted_slot,
            confirmed_slot,
            simulation_units_consumed,
            transaction_packet,
            lookup_table_resolution,
        })
    }
    .await;
    release_route_lookup_table_phase_leases(client, phase, Some(&prepared_lease_reference)).await;
    result
}

fn ensure_lookup_table_resolution_unchanged(
    predecision: &RuntimeLookupTableResolution,
    presend: &RuntimeLookupTableResolution,
) -> Result<(), Box<dyn Error>> {
    predecision.require_ready()?;
    presend.require_ready()?;
    if predecision.route_fingerprint != presend.route_fingerprint
        || predecision.requirements_fingerprint != presend.requirements_fingerprint
        || predecision.selection_kind != presend.selection_kind
        || predecision.selection_fingerprint != presend.selection_fingerprint
        || predecision.active_binding_fingerprint != presend.active_binding_fingerprint
        || predecision.selected_table_ids() != presend.selected_table_ids()
    {
        return Err(
            "lookup-table selection, usable prefix, mutation epoch, or active binding changed between predecision and pre-send"
                .into(),
        );
    }
    Ok(())
}

async fn resolve_route_lookup_tables_immediately_before_send(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    options: &CliOptions,
    vault: &SelectedVault,
    predecision: &RouteLookupTablePhase,
    instructions: &[Instruction],
    manifest: &LookupTableManifest,
    signers: &[&dyn Signer],
    prepared_lease_reference: &str,
) -> Result<RuntimeLookupTableResolution, Box<dyn Error>> {
    let presend = resolve_route_lookup_tables(
        client,
        rpc,
        options,
        vault,
        &predecision.source_reserve,
        &predecision.target_reserve,
        predecision.route_kind,
        &predecision.scope,
        signers
            .first()
            .ok_or("route pre-send signer set is empty")?
            .pubkey(),
        instructions,
        manifest,
        signers,
    )
    .await?;
    persist_route_lookup_table_resolution(
        client,
        options,
        vault,
        &predecision.source_reserve,
        &predecision.target_reserve,
        predecision.route_kind,
        manifest,
        &presend,
        false,
        false,
    )
    .await?;
    ensure_lookup_table_resolution_unchanged(&predecision.resolution, &presend)?;
    let selected_table_ids = presend.selected_table_ids();
    let route_lease_reference = predecision
        .resolution
        .route_lease_reference
        .as_deref()
        .ok_or("predecision route lease reference is missing")?;
    let expires_at = Utc::now() + ChronoDuration::minutes(LOOKUP_TABLE_PREPARED_LEASE_MINUTES);
    client
        .validate_lookup_table_usage_leases(
            LookupTableUsageLeaseKind::RouteResolution,
            route_lease_reference,
            &selected_table_ids,
            &presend.requirements_fingerprint,
            expires_at,
        )
        .await?;
    client
        .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
            cluster: options.cluster.clone(),
            lease_kind: LookupTableUsageLeaseKind::PreparedTransaction,
            reference_key: prepared_lease_reference.to_owned(),
            route_lookup_table_ids: selected_table_ids.clone(),
            vault_id: Some(vault.id),
            binding_id: presend.active_binding_id,
            route_fingerprint: Some(presend.route_fingerprint.clone()),
            requirements_fingerprint: Some(presend.requirements_fingerprint.clone()),
            expires_at,
        })
        .await?;
    client
        .validate_lookup_table_usage_leases(
            LookupTableUsageLeaseKind::PreparedTransaction,
            prepared_lease_reference,
            &selected_table_ids,
            &presend.requirements_fingerprint,
            Utc::now() + ChronoDuration::minutes(4),
        )
        .await?;
    if presend.selection_kind == LookupTableSelectionKind::Reusable {
        let (binding_fingerprint, _) =
            active_lookup_table_binding_fingerprint(client, vault.id, &selected_table_ids).await?;
        if binding_fingerprint != presend.active_binding_fingerprint {
            return Err(
                "active reusable lookup-table binding changed immediately before send".into(),
            );
        }
    }
    presend.require_ready()?;
    Ok(presend)
}

async fn release_route_lookup_table_phase_leases(
    client: &NeonSqlClient,
    phase: &RouteLookupTablePhase,
    prepared_lease_reference: Option<&str>,
) {
    if let Some(reference) = prepared_lease_reference {
        let _ = client
            .release_lookup_table_usage_leases(
                LookupTableUsageLeaseKind::PreparedTransaction,
                reference,
            )
            .await;
    }
    release_route_resolution_lease(client, &phase.resolution).await;
}

async fn release_idle_lookup_table_phase_leases(
    client: &NeonSqlClient,
    setup: Option<&RouteLookupTablePhase>,
    deposit: Option<&RouteLookupTablePhase>,
) {
    if let Some(phase) = setup {
        release_route_lookup_table_phase_leases(client, phase, None).await;
    }
    if let Some(phase) = deposit {
        release_route_lookup_table_phase_leases(client, phase, None).await;
    }
}

fn stable_fingerprint(parts: &[&str]) -> String {
    let parts = parts
        .iter()
        .map(|part| (*part).to_owned())
        .collect::<Vec<_>>();
    stable_fingerprint_owned(&parts)
}

fn stable_fingerprint_owned(parts: &[String]) -> String {
    let mut hasher = Sha256::new();
    for part in parts {
        hasher.update((part.len() as u64).to_le_bytes());
        hasher.update(part.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

fn ordered_lookup_table_address_hash(addresses: &[String]) -> String {
    stable_fingerprint_owned(addresses)
}

fn same_mint_route_lookup_table_scope_for_reserves(
    vault: &SelectedVault,
    source_reserve: &str,
    target_reserve: &str,
) -> String {
    format!(
        "same_mint_kamino:{}:{}:{}:{}",
        vault.settings, vault.vault_index, source_reserve, target_reserve
    )
}

fn compile_versioned_transaction(
    payer: Pubkey,
    instructions: &[Instruction],
    lookup_table_accounts: &[AddressLookupTableAccount],
    blockhash: Hash,
    signers: &[&dyn Signer],
) -> Result<VersionedTransaction, Box<dyn Error>> {
    let message = v0::Message::try_compile(&payer, instructions, lookup_table_accounts, blockhash)?;
    Ok(VersionedTransaction::try_new(
        VersionedMessage::V0(message),
        signers,
    )?)
}

fn best_case_single_lookup_table_packet_summary(
    payer: Pubkey,
    instructions: &[Instruction],
    blockhash: Hash,
    signers: &[&dyn Signer],
) -> Result<Option<TransactionPacketSummary>, Box<dyn Error>> {
    let lookup_addresses = best_case_lookup_table_addresses(payer, instructions);
    if lookup_addresses.is_empty() {
        return Ok(None);
    }
    let lookup_table_accounts = vec![AddressLookupTableAccount {
        key: Pubkey::new_from_array([42; 32]),
        addresses: lookup_addresses,
    }];
    let transaction = compile_versioned_transaction(
        payer,
        instructions,
        &lookup_table_accounts,
        blockhash,
        signers,
    )?;
    let mut summary = transaction_packet_summary(&transaction, &lookup_table_accounts)?;
    for (summary, account) in summary
        .lookup_table_accounts
        .iter_mut()
        .zip(lookup_table_accounts.iter())
    {
        summary.addresses = Some(pubkeys_json(&account.addresses));
    }
    Ok(Some(summary))
}

fn best_case_lookup_table_addresses(payer: Pubkey, instructions: &[Instruction]) -> Vec<Pubkey> {
    compiler_lookup_eligible_addresses(payer, instructions)
}

fn transaction_packet_summary(
    transaction: &VersionedTransaction,
    lookup_table_accounts: &[AddressLookupTableAccount],
) -> Result<TransactionPacketSummary, Box<dyn Error>> {
    let packet_size_bytes = bincode::serialize(transaction)?.len();
    let VersionedMessage::V0(message) = &transaction.message else {
        return Err("expected v0 transaction message".into());
    };
    let signer_count = usize::from(message.header.num_required_signatures);
    Ok(TransactionPacketSummary {
        version: "v0",
        fee_payer: message
            .account_keys
            .first()
            .map(ToString::to_string)
            .unwrap_or_default(),
        signer_pubkeys: message
            .account_keys
            .iter()
            .take(signer_count)
            .map(ToString::to_string)
            .collect(),
        packet_size_bytes,
        packet_data_size_bytes: PACKET_DATA_SIZE,
        fits_packet_data_size: packet_size_bytes <= PACKET_DATA_SIZE,
        static_account_key_count: message.account_keys.len(),
        address_table_lookup_count: message.address_table_lookups.len(),
        loaded_writable_address_count: message
            .address_table_lookups
            .iter()
            .map(|lookup| lookup.writable_indexes.len())
            .sum(),
        loaded_readonly_address_count: message
            .address_table_lookups
            .iter()
            .map(|lookup| lookup.readonly_indexes.len())
            .sum(),
        compiled_instruction_count: message.instructions.len(),
        instruction_data_bytes: message
            .instructions
            .iter()
            .map(|instruction| instruction.data.len())
            .sum(),
        lookup_table_accounts: lookup_table_accounts
            .iter()
            .map(|account| LookupTableAccountSummary {
                account: account.key.to_string(),
                address_count: account.addresses.len(),
                addresses: None,
            })
            .collect(),
    })
}

fn policy_transaction_packet_json(transaction: &PolicyTransactionBuild) -> Value {
    let mut value = transaction_packet_json(&transaction.transaction_packet);
    if let Value::Object(ref mut object) = value {
        object.insert(
            "bestCaseSingleLookupTable".to_owned(),
            transaction
                .best_case_single_lookup_table_packet
                .as_ref()
                .map(transaction_packet_json)
                .unwrap_or(Value::Null),
        );
    }
    value
}

fn transaction_packet_json(summary: &TransactionPacketSummary) -> Value {
    json!({
        "version": summary.version,
        "feePayer": summary.fee_payer,
        "signerPubkeys": summary.signer_pubkeys,
        "packetSizeBytes": summary.packet_size_bytes,
        "packetDataSizeBytes": summary.packet_data_size_bytes,
        "fitsPacketDataSize": summary.fits_packet_data_size,
        "lookupTableCount": summary.lookup_table_accounts.len(),
        "lookupTableAddressCount": summary.lookup_table_accounts.iter().map(|account| account.address_count).sum::<usize>(),
        "staticAccountKeyCount": summary.static_account_key_count,
        "addressTableLookupCount": summary.address_table_lookup_count,
        "loadedWritableAddressCount": summary.loaded_writable_address_count,
        "loadedReadonlyAddressCount": summary.loaded_readonly_address_count,
        "compiledInstructionCount": summary.compiled_instruction_count,
        "instructionDataBytes": summary.instruction_data_bytes,
        "instructionDataExceedsPacketLimit": summary.instruction_data_bytes > summary.packet_data_size_bytes,
        "lookupTables": summary.lookup_table_accounts.iter().map(|account| {
            let mut value = json!({
                "account": account.account,
                "addressCount": account.address_count,
            });
            if let (Value::Object(object), Some(addresses)) = (&mut value, &account.addresses) {
                object.insert("addresses".to_owned(), json!(addresses));
            }
            value
        }).collect::<Vec<_>>(),
    })
}

fn policy_transaction_json(transaction: &PolicyTransactionBuild) -> Value {
    let obligation_stale = policy_transaction_has_klend_obligation_stale(transaction);
    json!({
        "transaction": policy_transaction_packet_json(transaction),
        "simulationError": transaction.simulation_error,
        "simulationSkippedReason": transaction.simulation_skipped_reason,
        "simulationUnitsConsumed": transaction.simulation_units_consumed,
        "simulationLogs": transaction.simulation_logs,
        "klendObligationStale": obligation_stale,
        "requiresRefreshObligationPolicy": false,
        "refreshObligationPolicyNote": obligation_stale.then_some(
            "KLend deposit/withdraw needs a fresh obligation; the script now emits refresh_obligation as a public pre-instruction before protected value movement"
        ),
    })
}

fn policy_transaction_has_klend_obligation_stale(transaction: &PolicyTransactionBuild) -> bool {
    simulation_indicates_klend_obligation_stale(
        transaction.simulation_error.as_deref(),
        &transaction.simulation_logs,
    )
}

fn simulation_indicates_klend_obligation_stale(
    simulation_error: Option<&str>,
    simulation_logs: &Value,
) -> bool {
    simulation_error.is_some_and(|error| error.contains("Custom(6017)") || error.contains("0x1781"))
        || json_logs_contain(simulation_logs, "ObligationStale")
        || json_logs_contain(simulation_logs, "Obligation is stale and must be refreshed")
}

fn json_logs_contain(value: &Value, needle: &str) -> bool {
    match value {
        Value::Array(items) => items
            .iter()
            .any(|item| item.as_str().is_some_and(|log| log.contains(needle))),
        Value::String(log) => log.contains(needle),
        _ => false,
    }
}

async fn load_position_summaries(
    client: &NeonSqlClient,
    vault_id: VaultId,
) -> Result<Vec<PositionSummary>, Box<dyn Error>> {
    let current_positions = client.current_positions(vault_id).await?;
    Ok(current_positions
        .into_iter()
        .map(|position| PositionSummary {
            reserve: position.reserve,
            liquidity_mint: position.liquidity_mint,
            amount_raw: position.amount_raw,
            has_value: position.has_value,
            snapshot_id: position.snapshot_id,
            supply_apy_bps: position.supply_apy_bps,
            planning_metadata: position.planning_metadata,
        })
        .collect())
}

async fn load_prepared_same_mint_decision(
    pool: &PgPool,
    decision_id: DecisionId,
    expected_status: DecisionStatus,
) -> Result<PreparedSameMintDecision, Box<dyn Error>> {
    let row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            id,
            vault_id,
            source_snapshot_id,
            status::text AS status,
            source_reserve,
            target_reserve,
            liquidity_mint,
            source_liquidity_mint,
            target_liquidity_mint,
            amount_raw,
            source_apy_bps,
            target_apy_bps,
            estimated_edge_bps,
            estimated_cost_lamports,
            execution_plan,
            idempotency_key
        FROM loyal_yield.rebalance_decisions
        WHERE id = $1
        "#,
    )
    .bind(decision_id.as_i64())
    .fetch_one(pool)
    .await?;

    let status: String = row.try_get("status")?;
    if DecisionStatus::parse(&status) != Some(expected_status) {
        return Err(format!(
            "decision {} is {}, expected {}",
            decision_id.as_i64(),
            status,
            expected_status.as_str(),
        )
        .into());
    }
    let execution_plan: Value = row.try_get("execution_plan")?;
    let kind = execution_plan
        .get("kind")
        .and_then(Value::as_str)
        .unwrap_or_default();
    if kind != "same_mint" {
        return Err(format!(
            "decision {} execution_plan.kind is {kind:?}, expected same_mint",
            decision_id.as_i64()
        )
        .into());
    }

    let decision = PreparedSameMintDecision {
        id: DecisionId(row.try_get("id")?),
        vault_id: VaultId(row.try_get("vault_id")?),
        source_snapshot_id: SnapshotId(required_i64_column(&row, "source_snapshot_id")?),
        source_reserve: required_string_column(&row, "source_reserve")?,
        target_reserve: required_string_column(&row, "target_reserve")?,
        liquidity_mint: required_string_column(&row, "liquidity_mint")?,
        source_liquidity_mint: required_string_column(&row, "source_liquidity_mint")?,
        target_liquidity_mint: required_string_column(&row, "target_liquidity_mint")?,
        amount_raw: required_i64_column(&row, "amount_raw")?,
        source_apy_bps: required_i64_column(&row, "source_apy_bps")?,
        target_apy_bps: required_i64_column(&row, "target_apy_bps")?,
        estimated_edge_bps: required_i64_column(&row, "estimated_edge_bps")?,
        estimated_cost_lamports: row.try_get("estimated_cost_lamports")?,
        execution_plan,
        idempotency_key: row.try_get("idempotency_key")?,
    };
    validate_prepared_decision_plan_fields(&decision)?;
    Ok(decision)
}

fn required_string_column(
    row: &loyal_yield_orchestrator::sqlx::postgres::PgRow,
    column: &'static str,
) -> Result<String, Box<dyn Error>> {
    row.try_get::<Option<String>, _>(column)?
        .ok_or_else(|| format!("prepared same-mint decision is missing {column}").into())
}

fn required_i64_column(
    row: &loyal_yield_orchestrator::sqlx::postgres::PgRow,
    column: &'static str,
) -> Result<i64, Box<dyn Error>> {
    row.try_get::<Option<i64>, _>(column)?
        .ok_or_else(|| format!("prepared same-mint decision is missing {column}").into())
}

fn validate_prepared_decision_plan_fields(
    decision: &PreparedSameMintDecision,
) -> Result<(), Box<dyn Error>> {
    require_plan_string(decision, "source_reserve", &decision.source_reserve)?;
    require_plan_string(decision, "target_reserve", &decision.target_reserve)?;
    require_plan_string(decision, "liquidity_mint", &decision.liquidity_mint)?;
    require_optional_plan_string(
        decision,
        "source_liquidity_mint",
        &decision.source_liquidity_mint,
    )?;
    require_optional_plan_string(
        decision,
        "target_liquidity_mint",
        &decision.target_liquidity_mint,
    )?;
    if decision.source_liquidity_mint != decision.liquidity_mint {
        return Err(format!(
            "decision {} source_liquidity_mint {} does not match liquidity_mint {}",
            decision.id, decision.source_liquidity_mint, decision.liquidity_mint
        )
        .into());
    }
    if decision.target_liquidity_mint != decision.liquidity_mint {
        return Err(format!(
            "decision {} target_liquidity_mint {} does not match liquidity_mint {}",
            decision.id, decision.target_liquidity_mint, decision.liquidity_mint
        )
        .into());
    }
    require_plan_i64(decision, "amount_raw", decision.amount_raw)?;
    require_plan_string(
        decision,
        "route_amount_semantics",
        ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
    )?;
    require_plan_i64(
        decision,
        "redeemable_source_liquidity_amount_raw",
        decision.amount_raw,
    )?;
    if decision.source_snapshot_id.as_i64() <= 0 {
        return Err(format!(
            "decision {} source_snapshot_id {} is not a persisted snapshot",
            decision.id,
            decision.source_snapshot_id.as_i64()
        )
        .into());
    }
    if decision.idempotency_key.trim().is_empty() {
        return Err(format!("decision {} idempotency_key is empty", decision.id).into());
    }
    Ok(())
}

fn require_plan_string(
    decision: &PreparedSameMintDecision,
    field: &'static str,
    expected: &str,
) -> Result<(), Box<dyn Error>> {
    let actual = decision
        .execution_plan
        .get(field)
        .and_then(Value::as_str)
        .ok_or_else(|| format!("decision {} execution_plan.{field} is missing", decision.id))?;
    if actual != expected {
        return Err(format!(
            "decision {} execution_plan.{field} {actual} does not match row value {expected}",
            decision.id
        )
        .into());
    }
    Ok(())
}

fn require_optional_plan_string(
    decision: &PreparedSameMintDecision,
    field: &'static str,
    expected: &str,
) -> Result<(), Box<dyn Error>> {
    let Some(actual) = decision.execution_plan.get(field).and_then(Value::as_str) else {
        return Ok(());
    };
    if actual != expected {
        return Err(format!(
            "decision {} execution_plan.{field} {actual} does not match row value {expected}",
            decision.id
        )
        .into());
    }
    Ok(())
}

fn require_plan_i64(
    decision: &PreparedSameMintDecision,
    field: &'static str,
    expected: i64,
) -> Result<(), Box<dyn Error>> {
    let actual = decision
        .execution_plan
        .get(field)
        .and_then(Value::as_i64)
        .ok_or_else(|| format!("decision {} execution_plan.{field} is missing", decision.id))?;
    if actual != expected {
        return Err(format!(
            "decision {} execution_plan.{field} {actual} does not match row value {expected}",
            decision.id
        )
        .into());
    }
    Ok(())
}

fn plan_i64(plan: &Value, field: &'static str) -> Option<i64> {
    let value = plan.get(field)?;
    value
        .as_i64()
        .or_else(|| value.as_u64().and_then(|amount| i64::try_from(amount).ok()))
        .or_else(|| value.as_str().and_then(|amount| amount.parse::<i64>().ok()))
}

fn validate_execution_decision_route(
    decision: &PreparedSameMintDecision,
    reserve_move: &ReserveMove,
) -> Result<(), Box<dyn Error>> {
    if decision.source_reserve != reserve_move.source_reserve {
        return Err(format!(
            "persisted decision source reserve {} does not match requested source reserve {}",
            decision.source_reserve, reserve_move.source_reserve
        )
        .into());
    }
    if decision.target_reserve != reserve_move.target_reserve {
        return Err(format!(
            "persisted decision target reserve {} does not match requested target reserve {}",
            decision.target_reserve, reserve_move.target_reserve
        )
        .into());
    }
    Ok(())
}

fn same_mint_input_from_decision(decision: &PreparedSameMintDecision) -> SameMintRebalanceInput {
    SameMintRebalanceInput {
        vault_id: Some(decision.vault_id),
        settings: None,
        vault_index: None,
        source_reserve: decision.source_reserve.clone(),
        target_reserve: decision.target_reserve.clone(),
        liquidity_mint: decision.liquidity_mint.clone(),
        amount_raw: decision.amount_raw,
        route_amount_semantics: ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned(),
        source_amount_semantics: decision
            .execution_plan
            .get("source_amount_semantics")
            .and_then(Value::as_str)
            .map(ToOwned::to_owned),
        source_collateral_amount_raw: plan_i64(
            &decision.execution_plan,
            "source_collateral_amount_raw",
        ),
        redeemable_source_liquidity_amount_raw: plan_i64(
            &decision.execution_plan,
            "redeemable_source_liquidity_amount_raw",
        ),
        idle_vault_liquidity_amount_raw: plan_i64(
            &decision.execution_plan,
            "idle_vault_liquidity_amount_raw",
        ),
        expected_source_snapshot_id: decision.source_snapshot_id,
        source_apy_bps: decision.source_apy_bps,
        target_apy_bps: decision.target_apy_bps,
        estimated_edge_bps: decision.estimated_edge_bps,
        estimated_cost_lamports: decision.estimated_cost_lamports,
        dry_run: false,
    }
}

async fn load_user_position_seed_preview(
    pool: &PgPool,
    vault: &SelectedVault,
    reserve_move: &ReserveMove,
    chain_preview: Option<&ChainReconcilePreview>,
    direction: Direction,
) -> Result<Option<UserPositionSeedPreview>, Box<dyn Error>> {
    let rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            id,
            current_reserve,
            current_market,
            current_liquidity_mint,
            current_amount_raw,
            current_observed_slot,
            current_observed_at
        FROM loyal_yield.user_yield_positions
        WHERE settings = $1
          AND vault_index = $2
          AND vault_pubkey = $3
          AND status::text = 'active'
        ORDER BY current_observed_at DESC NULLS LAST, id DESC
        "#,
    )
    .bind(&vault.settings)
    .bind(vault.vault_index)
    .bind(&vault.vault_pubkey)
    .fetch_all(pool)
    .await?;

    if rows.is_empty() {
        return Ok(None);
    }

    let rows = rows
        .into_iter()
        .map(|row| {
            Ok(UserPositionSeedRow {
                id: row.try_get("id")?,
                current_reserve: row.try_get("current_reserve")?,
                current_market: row.try_get("current_market")?,
                current_liquidity_mint: row.try_get("current_liquidity_mint")?,
                current_amount_raw: row.try_get("current_amount_raw")?,
                current_observed_slot: row.try_get("current_observed_slot")?,
                current_observed_at: row.try_get("current_observed_at")?,
            })
        })
        .collect::<Result<Vec<_>, loyal_yield_orchestrator::sqlx::Error>>()?;

    let source_reserve = reserve_move.source_reserve.clone();
    let target_reserve = reserve_move.target_reserve.clone();
    let source_row = rows
        .iter()
        .find(|row| row.current_reserve == source_reserve);
    if source_row.is_none() {
        return Ok(Some(UserPositionSeedPreview {
            source: "user_yield_positions".to_owned(),
            rows,
            positions: Vec::new(),
        }));
    }
    let source_row = source_row.expect("checked some");
    let expected_source_market = chain_preview
        .and_then(|preview| chain_position_for_reserve(preview, &reserve_move.source_reserve).ok())
        .map(|position| position.market.clone())
        .or_else(|| {
            if reserve_move.source_reserve == direction.source_reserve() {
                Some(direction.source_market().to_owned())
            } else {
                None
            }
        });
    if let Some(expected_source_market) = expected_source_market {
        if source_row.current_market != expected_source_market {
            return Err(format!(
                "user_yield_positions row {} has market {}, expected {} for reserve {}",
                source_row.id,
                source_row.current_market,
                expected_source_market,
                source_row.current_reserve
            )
            .into());
        }
    }

    let target_amount = rows
        .iter()
        .find(|row| {
            row.current_reserve == target_reserve
                && row.current_liquidity_mint == source_row.current_liquidity_mint
        })
        .map(|row| row.current_amount_raw)
        .unwrap_or_default();
    let liquidity_mint = source_row.current_liquidity_mint.clone();
    let positions = vec![
        PositionSummary {
            reserve: source_reserve,
            liquidity_mint: liquidity_mint.clone(),
            amount_raw: source_row.current_amount_raw,
            has_value: source_row.current_amount_raw > 0,
            snapshot_id: SnapshotId(0),
            supply_apy_bps: None,
            planning_metadata: json!({
                "source": "user_yield_positions",
                "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                "redeemable_source_liquidity_amount_raw": source_row.current_amount_raw.to_string(),
            }),
        },
        PositionSummary {
            reserve: target_reserve,
            liquidity_mint,
            amount_raw: target_amount,
            has_value: target_amount > 0,
            snapshot_id: SnapshotId(0),
            supply_apy_bps: None,
            planning_metadata: json!({
                "source": "user_yield_positions",
                "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                "redeemable_source_liquidity_amount_raw": target_amount.to_string(),
            }),
        },
    ];

    Ok(Some(UserPositionSeedPreview {
        source: "user_yield_positions".to_owned(),
        rows,
        positions,
    }))
}

fn user_position_seed_reconciled_state(
    seed: &UserPositionSeedPreview,
    reserve_move: &ReserveMove,
    target_market: &str,
) -> Result<ReconciledVaultState, Box<dyn Error>> {
    let source_reserve = reserve_move.source_reserve.clone();
    let target_reserve = reserve_move.target_reserve.clone();
    let source_row = seed
        .rows
        .iter()
        .find(|row| row.current_reserve == source_reserve)
        .ok_or_else(|| {
            format!("user_yield_positions seed has no active source reserve {source_reserve}")
        })?;
    let source_amount = amount_i64_to_u64(source_row.current_amount_raw, "source amount")?;

    let target_row = seed.rows.iter().find(|row| {
        row.current_reserve == target_reserve
            && row.current_liquidity_mint == source_row.current_liquidity_mint
    });
    let target_amount = target_row
        .map(|row| amount_i64_to_u64(row.current_amount_raw, "target amount"))
        .transpose()?
        .unwrap_or_default();

    Ok(ReconciledVaultState {
        observed_slot: source_row.current_observed_slot,
        observed_at: source_row.current_observed_at,
        chain_slot: Some(source_row.current_observed_slot),
        lock_attempt_id: None,
        context: json!({
            "kind": "same_mint_user_position_seed",
            "source": seed.source,
            "source_position_id": source_row.id,
            "source_reserve": source_row.current_reserve,
            "target_reserve": target_reserve,
            "amount_raw": source_row.current_amount_raw.to_string(),
        }),
        positions: vec![
            ReconciledReservePosition {
                reserve: source_row.current_reserve.clone(),
                market: Some(source_row.current_market.clone()),
                liquidity_mint: source_row.current_liquidity_mint.clone(),
                amount_raw: source_amount,
                supply_apy_bps: None,
                borrow_apy_bps: None,
                planning_metadata: json!({
                    "source": seed.source,
                    "user_yield_position_id": source_row.id,
                    "seed_role": "source",
                    "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                    "redeemable_source_liquidity_amount_raw": source_amount.to_string(),
                }),
            },
            ReconciledReservePosition {
                reserve: target_reserve,
                market: Some(target_market.to_owned()),
                liquidity_mint: source_row.current_liquidity_mint.clone(),
                amount_raw: target_amount,
                supply_apy_bps: None,
                borrow_apy_bps: None,
                planning_metadata: json!({
                    "source": seed.source,
                    "user_yield_position_id": target_row.map(|row| row.id),
                    "seed_role": "target",
                    "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                    "redeemable_source_liquidity_amount_raw": target_amount.to_string(),
                }),
            },
        ],
    })
}

fn chain_preview_reconciled_state(
    preview: &ChainReconcilePreview,
) -> Result<ReconciledVaultState, Box<dyn Error>> {
    Ok(ReconciledVaultState {
        observed_slot: preview.observed_slot,
        observed_at: None,
        chain_slot: Some(preview.observed_slot),
        lock_attempt_id: None,
        context: json!({
            "kind": "same_mint_chain_reconcile_preview",
            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
        }),
        positions: preview
            .positions
            .iter()
            .map(|position| ReconciledReservePosition {
                reserve: position.reserve.clone(),
                market: Some(position.market.clone()),
                liquidity_mint: position.liquidity_mint.clone(),
                amount_raw: position.amount_raw,
                supply_apy_bps: None,
                borrow_apy_bps: None,
                planning_metadata: json!({
                    "source": "chain_reconcile_preview",
                    "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
                    "source_collateral_amount_raw": position.amount_raw.to_string(),
                    "redeemable_source_liquidity_amount_raw": position.redeemable_liquidity_amount_raw.to_string(),
                    "redeemable_liquidity_amount_raw": position.redeemable_liquidity_amount_raw.to_string(),
                    "obligation": position.obligation,
                    "obligation_exists": position.obligation_exists,
                    "vault_liquidity_ata": position.vault_liquidity_ata,
                    "vault_liquidity_token_account_exists": position.vault_liquidity_token_account_exists,
                    "idle_vault_liquidity_amount_raw": position.vault_liquidity_amount_raw.to_string(),
                    "vault_liquidity_amount_raw": position.vault_liquidity_amount_raw.to_string(),
                }),
            })
            .collect(),
    })
}

fn ensure_post_confirm_chain_reconcile_state(
    decision: &PreparedSameMintDecision,
    state: &ReconciledVaultState,
) -> Result<(), Box<dyn Error>> {
    let mut saw_source = false;
    let mut saw_target = false;

    for position in &state.positions {
        if position.reserve == decision.source_reserve {
            saw_source = true;
            if position.liquidity_mint != decision.liquidity_mint {
                return Err(format!(
                    "post-confirm source reserve liquidity mint {} does not match decision mint {}",
                    position.liquidity_mint, decision.liquidity_mint
                )
                .into());
            }
            if position.amount_raw != 0 {
                return Err(format!(
                    "post-confirm source reserve {} remains nonzero in chain reconcile: {}",
                    decision.source_reserve, position.amount_raw
                )
                .into());
            }
        } else if position.reserve == decision.target_reserve {
            saw_target = true;
            if position.liquidity_mint != decision.liquidity_mint {
                return Err(format!(
                    "post-confirm target reserve liquidity mint {} does not match decision mint {}",
                    position.liquidity_mint, decision.liquidity_mint
                )
                .into());
            }
            if position.amount_raw == 0 {
                return Err(format!(
                    "post-confirm target reserve {} is zero in chain reconcile",
                    decision.target_reserve
                )
                .into());
            }
        }
    }

    if !saw_source || !saw_target {
        return Err("post-confirm chain reconcile requires source and target positions".into());
    }

    Ok(())
}

fn target_market_for_seed(
    seed: &UserPositionSeedPreview,
    reserve_move: &ReserveMove,
    chain_preview: Option<&ChainReconcilePreview>,
    direction: Direction,
) -> Result<String, Box<dyn Error>> {
    let source_liquidity_mint = seed
        .rows
        .iter()
        .find(|row| row.current_reserve == reserve_move.source_reserve)
        .map(|row| row.current_liquidity_mint.as_str());
    if let Some(row) = seed.rows.iter().find(|row| {
        row.current_reserve == reserve_move.target_reserve
            && source_liquidity_mint.is_some_and(|mint| row.current_liquidity_mint == mint)
    }) {
        return Ok(row.current_market.clone());
    }
    if let Some(preview) = chain_preview {
        return Ok(
            chain_position_for_reserve(preview, &reserve_move.target_reserve)?
                .market
                .clone(),
        );
    }
    if reserve_move.target_reserve == direction.target_reserve() {
        return Ok(direction.target_market().to_owned());
    }
    Err(format!(
        "--seed-from-user-position with target reserve {} requires --reconcile-from-chain or an existing target row to determine the target market",
        reserve_move.target_reserve
    )
    .into())
}

fn amount_i64_to_u64(amount: i64, field: &str) -> Result<u64, Box<dyn Error>> {
    if amount < 0 {
        return Err(format!("{field} {amount} cannot be negative").into());
    }
    Ok(amount as u64)
}

fn load_chain_reconcile_preview(
    rpc_url: &str,
    vault: &SelectedVault,
    reserves: &[String],
) -> Result<ChainReconcilePreview, Box<dyn Error>> {
    load_chain_reconcile_preview_with_min_context(rpc_url, vault, reserves, None)
}

fn load_chain_reconcile_preview_with_min_context(
    rpc_url: &str,
    vault: &SelectedVault,
    reserves: &[String],
    min_context_slot: Option<u64>,
) -> Result<ChainReconcilePreview, Box<dyn Error>> {
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    load_chain_reconcile_preview_from_rpc(&rpc, vault, reserves, min_context_slot)
}

fn get_multiple_accounts_batched(
    rpc: &RpcClient,
    pubkeys: &[Pubkey],
    min_context_slot: Option<u64>,
) -> Result<(Vec<(Option<Account>, u64)>, usize), Box<dyn Error>> {
    let mut values = Vec::with_capacity(pubkeys.len());
    let mut requests = 0usize;
    for chunk in pubkeys.chunks(RPC_MULTIPLE_ACCOUNTS_LIMIT) {
        let response = rpc.get_multiple_accounts_with_config(
            chunk,
            RpcAccountInfoConfig {
                encoding: Some(UiAccountEncoding::Base64),
                commitment: Some(CommitmentConfig::confirmed()),
                min_context_slot,
                ..RpcAccountInfoConfig::default()
            },
        )?;
        if response.value.len() != chunk.len() {
            // An upstream response that violates the RPC contract, not evidence
            // about the accounts themselves. Typed as transient so callers class
            // it with the provider faults it belongs to.
            return Err(TransientChainReadError(format!(
                "getMultipleAccounts returned {} values for {} requested accounts",
                response.value.len(),
                chunk.len()
            ))
            .into());
        }
        let context_slot = response.context.slot;
        if let Some(minimum) = min_context_slot {
            if context_slot < minimum {
                return Err(TransientChainReadError(format!(
                    "getMultipleAccounts context slot {context_slot} is older than required minContextSlot {minimum}"
                ))
                .into());
            }
        }
        values.extend(
            response
                .value
                .into_iter()
                .map(|account| (account, context_slot)),
        );
        requests = requests.saturating_add(1);
    }
    Ok((values, requests))
}

async fn load_cached_reserve_summaries(
    runtime: &SameMintRouteRuntime,
    reserves: &[Pubkey],
    min_context_slot: Option<u64>,
    optimizer_epoch_id: Option<i64>,
    evidence: &mut FleetRpcAccountReadEvidence,
) -> Result<BTreeMap<Pubkey, (KaminoReserveSummary, u64)>, Box<dyn Error>> {
    let mut loaded = BTreeMap::new();
    let requested = reserves.iter().copied().collect::<BTreeSet<_>>();
    while loaded.len() < requested.len() {
        let mut leader_claims = Vec::new();
        let mut wait_for = None;
        {
            let mut state = runtime
                .rpc_cache
                .reserve_summaries
                .state
                .lock()
                .map_err(|_| "shared reserve-summary cache lock was poisoned")?;
            purge_ttl_cache(
                &mut state.values,
                SHARED_RESERVE_CACHE_TTL,
                SHARED_RESERVE_CACHE_MAX_ENTRIES,
            );
            for reserve in &requested {
                if loaded.contains_key(reserve) {
                    continue;
                }
                let cache_key = ReserveSummaryCacheKey {
                    reserve: *reserve,
                    optimizer_epoch_id,
                };
                if let Some(entry) = state.values.get(&cache_key).filter(|entry| {
                    entry.is_fresh_for(
                        optimizer_epoch_id,
                        min_context_slot,
                        SHARED_RESERVE_CACHE_TTL,
                    )
                }) {
                    loaded.insert(*reserve, (entry.value.clone(), entry.context_slot));
                    evidence.reserve_cache_hits = evidence.reserve_cache_hits.saturating_add(1);
                    continue;
                }

                let key = ReserveSummaryFlightKey {
                    reserve: *reserve,
                    optimizer_epoch_id,
                    min_context_slot,
                };
                if let Some(flight) = state.in_flight.get(&key) {
                    if wait_for.is_none() {
                        wait_for = Some(flight.clone());
                    }
                } else {
                    let flight = Arc::new(ReserveSummaryFlight::default());
                    state.in_flight.insert(key, flight.clone());
                    leader_claims.push((key, flight));
                }
            }
        }

        if loaded.len() == requested.len() {
            break;
        }

        if !leader_claims.is_empty() {
            let missing = leader_claims
                .iter()
                .map(|(key, _)| key.reserve)
                .collect::<Vec<_>>();
            let fetched = match catch_unwind(AssertUnwindSafe(
                || -> Result<(Vec<(Pubkey, KaminoReserveSummary, u64)>, usize), Box<dyn Error>> {
                    let (accounts, requests) = get_multiple_accounts_batched(
                        runtime.rpc.as_ref(),
                        &missing,
                        min_context_slot,
                    )?;
                    let mut fetched = Vec::with_capacity(missing.len());
                    for (reserve, (account, context_slot)) in missing.iter().copied().zip(accounts)
                    {
                        let account = account
                            .ok_or_else(|| format!("reserve account {reserve} does not exist"))?;
                        let summary = decode_kamino_reserve_summary(&reserve, &account)?;
                        fetched.push((reserve, summary, context_slot));
                    }
                    Ok((fetched, requests))
                },
            )) {
                Ok(result) => result,
                Err(_) => Err("shared reserve-summary RPC batch panicked".into()),
            };

            let observed_at = Utc::now();
            let fetched_at = Instant::now();
            let mut state = runtime
                .rpc_cache
                .reserve_summaries
                .state
                .lock()
                .map_err(|_| "shared reserve-summary cache lock was poisoned")?;
            for (key, flight) in &leader_claims {
                if state
                    .in_flight
                    .get(key)
                    .is_some_and(|current| Arc::ptr_eq(current, flight))
                {
                    state.in_flight.remove(key);
                }
            }
            if let Ok((values, _)) = &fetched {
                for (reserve, summary, context_slot) in values {
                    state.values.insert(
                        ReserveSummaryCacheKey {
                            reserve: *reserve,
                            optimizer_epoch_id,
                        },
                        CachedRpcValue {
                            value: summary.clone(),
                            context_slot: *context_slot,
                            optimizer_epoch_id,
                            observed_at,
                            fetched_at,
                        },
                    );
                    loaded.insert(*reserve, (summary.clone(), *context_slot));
                }
                purge_ttl_cache(
                    &mut state.values,
                    SHARED_RESERVE_CACHE_TTL,
                    SHARED_RESERVE_CACHE_MAX_ENTRIES,
                );
            }
            drop(state);
            for (_, flight) in leader_claims {
                flight.complete();
            }
            let (_, requests) = fetched?;
            evidence.reserve_batch_requests =
                evidence.reserve_batch_requests.saturating_add(requests);
            continue;
        }

        if let Some(flight) = wait_for {
            flight.wait().await;
            continue;
        }

        return Err("shared reserve-summary cache made no progress".into());
    }
    Ok(loaded)
}

fn cached_policy_account(
    runtime: &SameMintRouteRuntime,
    policy_account: &Pubkey,
    min_context_slot: Option<u64>,
    optimizer_epoch_id: Option<i64>,
) -> Result<Option<DecodedPolicyAccount>, Box<dyn Error>> {
    let mut cache = runtime
        .rpc_cache
        .policy_accounts
        .lock()
        .map_err(|_| "policy RPC cache lock was poisoned")?;
    purge_ttl_cache(
        &mut cache,
        POLICY_ACCOUNT_CACHE_TTL,
        POLICY_ACCOUNT_CACHE_MAX_ENTRIES,
    );
    Ok(cache
        .get(policy_account)
        .filter(|entry| {
            entry.is_fresh_for(
                optimizer_epoch_id,
                min_context_slot,
                POLICY_ACCOUNT_CACHE_TTL,
            )
        })
        .map(|entry| entry.value.clone()))
}

fn cache_policy_account(
    runtime: &SameMintRouteRuntime,
    policy_account: Pubkey,
    account: &Account,
    context_slot: u64,
    optimizer_epoch_id: Option<i64>,
) -> Result<(), Box<dyn Error>> {
    if account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID || account.executable {
        return Err(format!(
            "policy account {policy_account} must be a non-executable account owned by {}",
            SQUADS_SMART_ACCOUNT_PROGRAM_ID
        )
        .into());
    }
    let decoded = decode_squads_policy_account(&account.data).map_err(|error| {
        format!("failed to decode Squads policy account {policy_account}: {error}")
    })?;
    let mut cache = runtime
        .rpc_cache
        .policy_accounts
        .lock()
        .map_err(|_| "policy RPC cache lock was poisoned")?;
    purge_ttl_cache(
        &mut cache,
        POLICY_ACCOUNT_CACHE_TTL,
        POLICY_ACCOUNT_CACHE_MAX_ENTRIES,
    );
    cache.insert(
        policy_account,
        CachedRpcValue {
            value: decoded,
            context_slot,
            optimizer_epoch_id,
            observed_at: Utc::now(),
            fetched_at: Instant::now(),
        },
    );
    purge_ttl_cache(
        &mut cache,
        POLICY_ACCOUNT_CACHE_TTL,
        POLICY_ACCOUNT_CACHE_MAX_ENTRIES,
    );
    Ok(())
}

async fn load_chain_reconcile_preview_from_runtime(
    runtime: &SameMintRouteRuntime,
    vault: &SelectedVault,
    reserves: &[String],
    min_context_slot: Option<u64>,
    optimizer_epoch_id: Option<i64>,
    include_policy: bool,
) -> Result<ChainReconcilePreview, Box<dyn Error>> {
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let (vault_user_metadata, _) = user_metadata(&KLEND_PROGRAM_ID, &vault_pubkey);
    let policy_account = include_policy
        .then(|| Pubkey::from_str(&vault.policy_account))
        .transpose()?;
    let mut evidence = FleetRpcAccountReadEvidence::default();
    let policy_is_cached = match policy_account {
        Some(policy) => {
            cached_policy_account(runtime, &policy, min_context_slot, optimizer_epoch_id)?.is_some()
        }
        None => false,
    };
    evidence.policy_cache_hit = policy_is_cached;

    let mut reserve_pubkeys = Vec::with_capacity(reserves.len());
    for reserve in reserves {
        let pubkey = Pubkey::from_str(reserve)
            .map_err(|_| format!("reconcile reserve {reserve} must be a public key"))?;
        if !reserve_pubkeys.contains(&pubkey) {
            reserve_pubkeys.push(pubkey);
        }
    }
    let mut positions = Vec::with_capacity(reserve_pubkeys.len());
    let mut processed = BTreeSet::new();
    let mut observed_slots = Vec::new();
    let mut vault_user_metadata_exists = false;
    let mut first_round = true;

    while processed.len() < reserve_pubkeys.len() {
        let round_reserves = reserve_pubkeys
            .iter()
            .copied()
            .filter(|reserve| !processed.contains(reserve))
            .collect::<Vec<_>>();
        let summaries = load_cached_reserve_summaries(
            runtime,
            &round_reserves,
            min_context_slot,
            optimizer_epoch_id,
            &mut evidence,
        )
        .await?;

        let mut derived = Vec::with_capacity(round_reserves.len());
        let mut account_keys = BTreeSet::new();
        if first_round {
            account_keys.insert(vault_user_metadata);
            if let Some(policy) = policy_account.filter(|_| !policy_is_cached) {
                account_keys.insert(policy);
            }
        }
        for reserve in &round_reserves {
            let summary = summaries
                .get(reserve)
                .ok_or("shared reserve batch omitted a requested reserve")?
                .0
                .clone();
            let vault_liquidity_ata = derive_associated_token_address(
                &vault_pubkey,
                &summary.liquidity_mint,
                &spl_token::ID,
            );
            let (obligation_account, _) = obligation(
                &KLEND_PROGRAM_ID,
                0,
                0,
                &vault_pubkey,
                &summary.market,
                &Pubkey::default(),
                &Pubkey::default(),
            );
            let farm_user_state = summary
                .collateral_farm
                .map(|farm| farms_user_state(&farm, &obligation_account).0);
            // Re-read the reserve in the same RPC response as its dependent
            // obligation and token accounts. The cache is only an address-
            // derivation accelerator; mutable exchange-rate evidence comes
            // from this coherent batch.
            account_keys.insert(*reserve);
            account_keys.insert(vault_liquidity_ata);
            account_keys.insert(obligation_account);
            if let Some(farm_user_state) = farm_user_state {
                account_keys.insert(farm_user_state);
            }
            derived.push((
                *reserve,
                summary,
                vault_liquidity_ata,
                obligation_account,
                farm_user_state,
            ));
        }
        let account_keys = account_keys.into_iter().collect::<Vec<_>>();
        let (account_values, requests) =
            get_multiple_accounts_batched(runtime.rpc.as_ref(), &account_keys, min_context_slot)?;
        if requests != 1 {
            return Err(
                "chain reconciliation dependent accounts exceed one getMultipleAccounts context"
                    .into(),
            );
        }
        evidence.vault_batch_requests = evidence.vault_batch_requests.saturating_add(requests);
        let mut accounts = BTreeMap::new();
        for (key, (account, slot)) in account_keys.into_iter().zip(account_values) {
            observed_slots.push(slot);
            accounts.insert(key, (account, slot));
        }
        if first_round {
            vault_user_metadata_exists = account_exists_with_owner_from_account(
                accounts
                    .get(&vault_user_metadata)
                    .and_then(|(account, _)| account.as_ref()),
                &vault_user_metadata,
                &KLEND_PROGRAM_ID,
            )?;
            if let Some(policy) = policy_account.filter(|_| !policy_is_cached) {
                let (account, context_slot) = accounts
                    .get(&policy)
                    .ok_or("policy account was omitted from the vault account batch")?;
                let account = account
                    .as_ref()
                    .ok_or_else(|| format!("policy account {policy} does not exist"))?;
                cache_policy_account(runtime, policy, account, *context_slot, optimizer_epoch_id)?;
            }
        }

        let mut refreshed_reserves = Vec::with_capacity(derived.len());
        let mut identity_drift = None;
        for (reserve, cached_summary, _, _, _) in &mut derived {
            let (account, context_slot) = accounts
                .get(reserve)
                .ok_or("dependent account batch omitted its reserve")?;
            let account = account
                .as_ref()
                .ok_or_else(|| format!("reserve account {reserve} does not exist"))?;
            let refreshed_summary = decode_kamino_reserve_summary(reserve, account)?;
            if !cached_summary.derivation_identity_matches(&refreshed_summary) {
                identity_drift = Some(*reserve);
            }
            *cached_summary = refreshed_summary.clone();
            refreshed_reserves.push((*reserve, refreshed_summary, *context_slot));
        }
        {
            let observed_at = Utc::now();
            let fetched_at = Instant::now();
            let mut state = runtime
                .rpc_cache
                .reserve_summaries
                .state
                .lock()
                .map_err(|_| "shared reserve-summary cache lock was poisoned")?;
            purge_ttl_cache(
                &mut state.values,
                SHARED_RESERVE_CACHE_TTL,
                SHARED_RESERVE_CACHE_MAX_ENTRIES,
            );
            for (reserve, summary, context_slot) in refreshed_reserves {
                state.values.insert(
                    ReserveSummaryCacheKey {
                        reserve,
                        optimizer_epoch_id,
                    },
                    CachedRpcValue {
                        value: summary,
                        context_slot,
                        optimizer_epoch_id,
                        observed_at,
                        fetched_at,
                    },
                );
            }
            purge_ttl_cache(
                &mut state.values,
                SHARED_RESERVE_CACHE_TTL,
                SHARED_RESERVE_CACHE_MAX_ENTRIES,
            );
        }
        if let Some(reserve) = identity_drift {
            // The refreshed summary is already cached above, so the next attempt
            // derives from it and succeeds. Typed so the fleet sweep does not
            // report a self-clearing cache generation change as an invariant.
            return Err(TransientChainReadError(format!(
                "reserve {reserve} address-derivation identity changed during coherent account read; retry with the refreshed cache"
            ))
            .into());
        }

        for (reserve, summary, vault_liquidity_ata, obligation_account, farm_user_state) in derived
        {
            let (vault_liquidity_amount_raw, vault_liquidity_token_account_exists) =
                decode_spl_token_account_amount(
                    accounts
                        .get(&vault_liquidity_ata)
                        .and_then(|(account, _)| account.as_ref()),
                    &vault_liquidity_ata,
                    &summary.liquidity_mint,
                )?;
            let obligation_summary = decode_kamino_obligation_summary(
                accounts
                    .get(&obligation_account)
                    .and_then(|(account, _)| account.as_ref()),
                &obligation_account,
                &vault_pubkey,
                &summary.market,
                &reserve,
            )?;
            append_missing_obligation_reserve_pubkeys(
                &mut reserve_pubkeys,
                &obligation_summary,
                &obligation_account,
            )?;
            let collateral_farm_user_state_exists = match farm_user_state {
                Some(farm_user_state) => account_exists_with_owner_from_account(
                    accounts
                        .get(&farm_user_state)
                        .and_then(|(account, _)| account.as_ref()),
                    &farm_user_state,
                    &FARMS_PROGRAM_ID,
                )?,
                None => false,
            };
            let redeemable_liquidity_amount_raw = collateral_to_redeemable_liquidity_amount(
                summary.collateral_total_supply,
                &summary.total_liquidity_scaled,
                obligation_summary.reserve_deposited_amount_raw,
            )?;
            positions.push(ChainPositionSummary {
                reserve: reserve.to_string(),
                market: summary.market.to_string(),
                liquidity_mint: summary.liquidity_mint.to_string(),
                liquidity_token_program: summary.liquidity_token_program.to_string(),
                reserve_liquidity_supply: summary.liquidity_supply.to_string(),
                collateral_mint: summary.collateral_mint.to_string(),
                reserve_collateral_supply: summary.collateral_supply.to_string(),
                collateral_farm: summary.collateral_farm.map(|farm| farm.to_string()),
                collateral_farm_user_state: farm_user_state.map(|state| state.to_string()),
                collateral_farm_user_state_exists,
                pyth_oracle: summary.pyth_oracle.map(|oracle| oracle.to_string()),
                switchboard_price_oracle: summary
                    .switchboard_price_oracle
                    .map(|oracle| oracle.to_string()),
                switchboard_twap_oracle: summary
                    .switchboard_twap_oracle
                    .map(|oracle| oracle.to_string()),
                scope_prices: summary.scope_prices.map(|account| account.to_string()),
                obligation: obligation_account.to_string(),
                obligation_exists: obligation_summary.exists,
                obligation_deposit_reserves: obligation_summary.deposit_reserves,
                obligation_borrow_reserves: obligation_summary.borrow_reserves,
                amount_raw: obligation_summary.reserve_deposited_amount_raw,
                redeemable_liquidity_amount_raw,
                vault_liquidity_ata: vault_liquidity_ata.to_string(),
                vault_liquidity_token_account_exists,
                vault_liquidity_amount_raw,
            });
            processed.insert(reserve);
        }
        first_round = false;
    }

    let observed_slot = observed_slots
        .into_iter()
        .min()
        .or(min_context_slot)
        .ok_or("batched chain reconcile returned no account context slot")?;
    Ok(ChainReconcilePreview {
        observed_slot: i64::try_from(observed_slot)?,
        vault_user_metadata: vault_user_metadata.to_string(),
        vault_user_metadata_exists,
        positions,
        rpc_account_reads: evidence,
    })
}

fn load_chain_reconcile_preview_from_rpc(
    rpc: &RpcClient,
    vault: &SelectedVault,
    reserves: &[String],
    min_context_slot: Option<u64>,
) -> Result<ChainReconcilePreview, Box<dyn Error>> {
    let observed_slot = i64::try_from(match min_context_slot {
        Some(slot) => slot,
        None => rpc.get_slot()?,
    })?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let (vault_user_metadata, _) = user_metadata(&KLEND_PROGRAM_ID, &vault_pubkey);
    let vault_user_metadata_exists = account_exists_with_owner_at_or_after(
        &rpc,
        &vault_user_metadata,
        &KLEND_PROGRAM_ID,
        min_context_slot,
    )?;
    let mut reserve_pubkeys = Vec::with_capacity(reserves.len());
    for reserve in reserves {
        let pubkey = Pubkey::from_str(reserve)
            .map_err(|_| format!("reconcile reserve {reserve} must be a public key"))?;
        if !reserve_pubkeys.iter().any(|existing| existing == &pubkey) {
            reserve_pubkeys.push(pubkey);
        }
    }
    let mut positions = Vec::with_capacity(reserve_pubkeys.len());

    let mut reserve_index = 0;
    while reserve_index < reserve_pubkeys.len() {
        let reserve = reserve_pubkeys[reserve_index];
        reserve_index += 1;
        let reserve_summary =
            load_kamino_reserve_summary_at_or_after(&rpc, &reserve, min_context_slot)?;
        let vault_liquidity_ata = derive_associated_token_address(
            &vault_pubkey,
            &reserve_summary.liquidity_mint,
            &spl_token::ID,
        );
        let (vault_liquidity_amount_raw, vault_liquidity_token_account_exists) =
            load_spl_token_account_amount_at_or_after(
                &rpc,
                &vault_liquidity_ata,
                &reserve_summary.liquidity_mint,
                min_context_slot,
            )?;

        let collateral_mint = reserve_summary.collateral_mint;
        let (obligation_account, _) = obligation(
            &KLEND_PROGRAM_ID,
            0,
            0,
            &vault_pubkey,
            &reserve_summary.market,
            &Pubkey::default(),
            &Pubkey::default(),
        );
        let obligation_summary = load_kamino_obligation_summary_at_or_after(
            &rpc,
            &obligation_account,
            &vault_pubkey,
            &reserve_summary.market,
            &reserve,
            min_context_slot,
        )?;
        append_missing_obligation_reserve_pubkeys(
            &mut reserve_pubkeys,
            &obligation_summary,
            &obligation_account,
        )?;
        let (collateral_farm_user_state, collateral_farm_user_state_exists) =
            if let Some(collateral_farm) = reserve_summary.collateral_farm {
                let (farm_user_state, _) = farms_user_state(&collateral_farm, &obligation_account);
                let exists = account_exists_with_owner_at_or_after(
                    &rpc,
                    &farm_user_state,
                    &FARMS_PROGRAM_ID,
                    min_context_slot,
                )?;
                (Some(farm_user_state.to_string()), exists)
            } else {
                (None, false)
            };

        let redeemable_liquidity_amount_raw = collateral_to_redeemable_liquidity_amount(
            reserve_summary.collateral_total_supply,
            &reserve_summary.total_liquidity_scaled,
            obligation_summary.reserve_deposited_amount_raw,
        )?;

        positions.push(ChainPositionSummary {
            reserve: reserve.to_string(),
            market: reserve_summary.market.to_string(),
            liquidity_mint: reserve_summary.liquidity_mint.to_string(),
            liquidity_token_program: reserve_summary.liquidity_token_program.to_string(),
            reserve_liquidity_supply: reserve_summary.liquidity_supply.to_string(),
            collateral_mint: collateral_mint.to_string(),
            reserve_collateral_supply: reserve_summary.collateral_supply.to_string(),
            collateral_farm: reserve_summary.collateral_farm.map(|farm| farm.to_string()),
            collateral_farm_user_state,
            collateral_farm_user_state_exists,
            pyth_oracle: reserve_summary.pyth_oracle.map(|oracle| oracle.to_string()),
            switchboard_price_oracle: reserve_summary
                .switchboard_price_oracle
                .map(|oracle| oracle.to_string()),
            switchboard_twap_oracle: reserve_summary
                .switchboard_twap_oracle
                .map(|oracle| oracle.to_string()),
            scope_prices: reserve_summary
                .scope_prices
                .map(|account| account.to_string()),
            obligation: obligation_account.to_string(),
            obligation_exists: obligation_summary.exists,
            obligation_deposit_reserves: obligation_summary.deposit_reserves,
            obligation_borrow_reserves: obligation_summary.borrow_reserves,
            amount_raw: obligation_summary.reserve_deposited_amount_raw,
            redeemable_liquidity_amount_raw,
            vault_liquidity_ata: vault_liquidity_ata.to_string(),
            vault_liquidity_token_account_exists,
            vault_liquidity_amount_raw,
        });
    }

    Ok(ChainReconcilePreview {
        observed_slot,
        vault_user_metadata: vault_user_metadata.to_string(),
        vault_user_metadata_exists,
        positions,
        rpc_account_reads: FleetRpcAccountReadEvidence::default(),
    })
}

fn append_missing_obligation_reserve_pubkeys(
    reserve_pubkeys: &mut Vec<Pubkey>,
    obligation_summary: &KaminoObligationSummary,
    obligation_account: &Pubkey,
) -> Result<(), Box<dyn Error>> {
    for reserve in obligation_summary
        .deposit_reserves
        .iter()
        .chain(obligation_summary.borrow_reserves.iter())
    {
        let pubkey = Pubkey::from_str(reserve).map_err(|error| {
            format!(
                "invalid reserve {reserve} referenced by obligation {obligation_account}: {error}"
            )
        })?;
        if !reserve_pubkeys.iter().any(|existing| existing == &pubkey) {
            reserve_pubkeys.push(pubkey);
        }
    }

    Ok(())
}

fn load_policy_account_preflight(
    rpc_url: &str,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    reserve_move: &ReserveMove,
) -> Result<PolicyAccountPreflight, Box<dyn Error>> {
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    load_policy_account_preflight_from_rpc(&rpc, vault, preview, reserve_move)
}

fn load_policy_account_preflight_from_rpc(
    rpc: &RpcClient,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    reserve_move: &ReserveMove,
) -> Result<PolicyAccountPreflight, Box<dyn Error>> {
    let source = chain_position_for_reserve(preview, &reserve_move.source_reserve)?;
    let target = chain_position_for_reserve(preview, &reserve_move.target_reserve)?;
    let policy_account = Pubkey::from_str(&vault.policy_account)?;
    let account = rpc.get_account(&policy_account)?;
    let decoded = decode_squads_policy_account(&account.data).map_err(|error| {
        format!(
            "failed to decode Squads policy account {}: {error}",
            vault.policy_account
        )
    })?;

    Ok(PolicyAccountPreflight {
        policy_account: vault.policy_account.clone(),
        source_market: source.market.clone(),
        target_market: target.market.clone(),
        decoded,
    })
}

fn load_policy_account_preflight_from_runtime(
    runtime: &SameMintRouteRuntime,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    reserve_move: &ReserveMove,
    optimizer_epoch_id: Option<i64>,
) -> Result<PolicyAccountPreflight, Box<dyn Error>> {
    let source = chain_position_for_reserve(preview, &reserve_move.source_reserve)?;
    let target = chain_position_for_reserve(preview, &reserve_move.target_reserve)?;
    let policy_account = Pubkey::from_str(&vault.policy_account)?;
    let min_context_slot = Some(u64::try_from(preview.observed_slot)?);
    let decoded = if let Some(decoded) = cached_policy_account(
        runtime,
        &policy_account,
        min_context_slot,
        optimizer_epoch_id,
    )? {
        decoded
    } else {
        let (accounts, _) = get_multiple_accounts_batched(
            runtime.rpc.as_ref(),
            &[policy_account],
            min_context_slot,
        )?;
        let (account, context_slot) = accounts
            .into_iter()
            .next()
            .ok_or("policy account batch returned no value")?;
        let account = account
            .as_ref()
            .ok_or_else(|| format!("policy account {policy_account} does not exist"))?;
        cache_policy_account(
            runtime,
            policy_account,
            account,
            context_slot,
            optimizer_epoch_id,
        )?;
        cached_policy_account(
            runtime,
            &policy_account,
            min_context_slot,
            optimizer_epoch_id,
        )?
        .ok_or("policy cache fill did not retain the decoded account")?
    };

    Ok(PolicyAccountPreflight {
        policy_account: vault.policy_account.clone(),
        source_market: source.market.clone(),
        target_market: target.market.clone(),
        decoded,
    })
}

fn decode_squads_policy_account(data: &[u8]) -> Result<DecodedPolicyAccount, String> {
    let mut cursor = PolicyCursor::new(data);
    let discriminator = cursor.read_array::<8>()?;
    if discriminator != SQUADS_POLICY_ACCOUNT_DISCRIMINATOR {
        return Err("account discriminator is not a Squads Policy account".to_owned());
    }
    cursor.skip(PUBKEY_LEN)?;
    cursor.skip(8)?;
    cursor.skip(1)?;
    cursor.skip(8)?;
    cursor.skip(8)?;

    let signer_count = cursor.read_u32_len("policy signer count", 32)?;
    let mut delegated_signers = Vec::with_capacity(signer_count);
    for _ in 0..signer_count {
        delegated_signers.push(cursor.read_pubkey()?.to_string());
        cursor.skip(1)?;
    }
    let threshold = cursor.read_u16()?;
    cursor.skip(4)?;

    let policy_state_tag = cursor.read_u8()?;
    if policy_state_tag != 3 {
        return Err(format!(
            "unsupported policy state tag {policy_state_tag}; expected ProgramInteraction (3)"
        ));
    }
    let layout = PolicyAccountLayout::ProgramInteractionPolicyState;
    let account_index = cursor.read_u8()?;
    let legacy_cursor = cursor.clone();
    let constraints = match read_legacy_program_interaction_instruction_constraints(cursor) {
        Ok(constraints) => constraints,
        Err(legacy_error) => {
            let mut compact_cursor = legacy_cursor;
            read_compact_program_interaction_instruction_constraints(&mut compact_cursor)
                .map_err(|compact_error| {
                    format!(
                        "failed to decode ProgramInteraction policy as legacy ({legacy_error}) or compact ({compact_error})"
                    )
                })?
        }
    };

    Ok(summarize_policy_account(
        layout,
        delegated_signers,
        threshold,
        account_index,
        constraints,
    ))
}

fn read_legacy_program_interaction_instruction_constraints(
    mut cursor: PolicyCursor<'_>,
) -> Result<Vec<PolicyInstructionConstraint>, String> {
    let len = cursor.read_u32_len("program interaction instruction constraint count", 128)?;
    read_program_interaction_instruction_constraints(&mut cursor, len)
}

fn summarize_policy_account(
    layout: PolicyAccountLayout,
    delegated_signers: Vec<String>,
    threshold: u16,
    account_index: u8,
    constraints: Vec<PolicyInstructionConstraint>,
) -> DecodedPolicyAccount {
    let mut kamino_markets = Vec::new();
    let mut kamino_liquidity_mints = Vec::new();
    let mut instructions = Vec::with_capacity(constraints.len());
    let instruction_count = constraints.len();

    for constraint in &constraints {
        let discriminator = instruction_discriminator(&constraint);
        let route_step = kamino_route_step(&constraint, discriminator.as_deref());
        let markets = if let Some(step) = route_step {
            let account_index = match step {
                KAMINO_WITHDRAW_ROUTE_STEP | KAMINO_DEPOSIT_ROUTE_STEP => 2,
                KAMINO_INIT_OBLIGATION_ROUTE_STEP => 3,
                KAMINO_REFRESH_OBLIGATION_ROUTE_STEP => 0,
                _ => 1,
            };
            pubkeys_for_account(&constraint, account_index).unwrap_or_default()
        } else if constraint.program_id == KLEND_PROGRAM_ID {
            let mut markets = pubkeys_for_account(&constraint, 1).unwrap_or_default();
            markets.extend(pubkeys_for_account(&constraint, 2).unwrap_or_default());
            unique_pubkeys(markets)
        } else {
            Vec::new()
        }
        .into_iter()
        .map(|pubkey| pubkey.to_string())
        .collect::<Vec<_>>();
        let liquidity_mints = if route_step == Some(KAMINO_WITHDRAW_ROUTE_STEP)
            || route_step == Some(KAMINO_DEPOSIT_ROUTE_STEP)
            || (route_step.is_none() && constraint.program_id == KLEND_PROGRAM_ID)
        {
            let mut liquidity_mints = pubkeys_for_account(&constraint, 5).unwrap_or_default();
            liquidity_mints.extend(account_data_pubkeys_for_account(
                &constraint,
                5,
                SPL_TOKEN_ACCOUNT_MINT_OFFSET as u64,
                Some(spl_token::ID),
            ));
            unique_pubkeys(liquidity_mints)
        } else {
            Vec::new()
        }
        .into_iter()
        .map(|pubkey| pubkey.to_string())
        .collect::<Vec<_>>();

        extend_unique_strings(&mut kamino_markets, &markets);
        extend_unique_strings(&mut kamino_liquidity_mints, &liquidity_mints);

        instructions.push(DecodedPolicyInstructionSummary {
            program_id: constraint.program_id.to_string(),
            route_step,
            data_discriminator: discriminator,
            markets,
            liquidity_mints,
            account_constraints: decoded_policy_account_constraint_summaries(
                &constraint.account_constraints,
            ),
        });
    }

    DecodedPolicyAccount {
        layout,
        delegated_signers,
        threshold,
        account_index,
        instruction_count,
        kamino_markets,
        kamino_liquidity_mints,
        constraints,
        instructions,
    }
}

fn decoded_policy_account_constraint_summaries(
    constraints: &[PolicyAccountConstraint],
) -> Vec<DecodedPolicyAccountConstraintSummary> {
    constraints
        .iter()
        .map(|constraint| DecodedPolicyAccountConstraintSummary {
            account_index: constraint.account_index,
            kind: if constraint.pubkeys.is_empty() {
                "account_data"
            } else {
                "pubkey"
            },
            pubkeys: constraint.pubkeys.iter().map(ToString::to_string).collect(),
            owner: constraint.owner.map(|owner| owner.to_string()),
            data_constraints: constraint
                .data_constraints
                .iter()
                .map(decoded_policy_data_constraint_summary)
                .collect(),
        })
        .collect()
}

fn decoded_policy_data_constraint_summary(
    constraint: &PolicyDataConstraint,
) -> DecodedPolicyDataConstraintSummary {
    DecodedPolicyDataConstraintSummary {
        data_offset: constraint.data_offset,
        operator: constraint.operator.as_str(),
        value: constraint.data_value.to_json(),
    }
}

fn read_program_interaction_instruction_constraints(
    cursor: &mut PolicyCursor<'_>,
    len: usize,
) -> Result<Vec<PolicyInstructionConstraint>, String> {
    let mut constraints = Vec::with_capacity(len);
    for _ in 0..len {
        let program_id = cursor.read_pubkey()?;
        let account_constraint_count =
            cursor.read_u32_len("program interaction account constraint count", 128)?;
        let account_constraints =
            read_program_interaction_account_constraints(cursor, account_constraint_count)?;
        let data_constraint_count =
            cursor.read_u32_len("program interaction data constraint count", 128)?;
        let data_constraints = read_policy_data_constraints(cursor, data_constraint_count)?;
        constraints.push(PolicyInstructionConstraint {
            program_id,
            account_constraints,
            data_constraints,
        });
    }
    Ok(constraints)
}

fn read_compact_program_interaction_instruction_constraints(
    cursor: &mut PolicyCursor<'_>,
) -> Result<Vec<PolicyInstructionConstraint>, String> {
    let pubkey_table_len = cursor.read_u8()? as usize;
    if pubkey_table_len > 240 {
        return Err(format!(
            "program interaction pubkey table length {pubkey_table_len} exceeds maximum 240"
        ));
    }
    let pubkey_table = (0..pubkey_table_len)
        .map(|_| cursor.read_pubkey())
        .collect::<Result<Vec<_>, _>>()?;
    let instruction_count = cursor.read_u8()? as usize;
    if instruction_count > 128 {
        return Err(format!(
            "program interaction instruction constraint count {instruction_count} exceeds maximum 128"
        ));
    }
    let mut constraints = Vec::with_capacity(instruction_count);
    for _ in 0..instruction_count {
        let program_id = compact_pubkey(&pubkey_table, cursor.read_u8()?)?;
        let account_constraint_count = cursor.read_u8()? as usize;
        if account_constraint_count > 128 {
            return Err(format!(
                "program interaction account constraint count {account_constraint_count} exceeds maximum 128"
            ));
        }
        let mut account_constraints = Vec::with_capacity(account_constraint_count);
        for _ in 0..account_constraint_count {
            let account_index = cursor.read_u8()?;
            let (pubkeys, data_constraints) = match cursor.read_u8()? {
                0 => {
                    let len = cursor.read_u8()? as usize;
                    if len > 128 {
                        return Err(format!(
                            "program interaction pubkey account constraint {len} exceeds maximum 128"
                        ));
                    }
                    let mut pubkeys = Vec::with_capacity(len);
                    for _ in 0..len {
                        pubkeys.push(compact_pubkey(&pubkey_table, cursor.read_u8()?)?);
                    }
                    (pubkeys, Vec::new())
                }
                1 => {
                    let len = cursor.read_u8()? as usize;
                    if len > 128 {
                        return Err(format!(
                            "program interaction account data constraint count {len} exceeds maximum 128"
                        ));
                    }
                    (Vec::new(), read_policy_data_constraints(cursor, len)?)
                }
                tag => {
                    return Err(format!(
                        "unknown compact program interaction account constraint kind {tag}"
                    ))
                }
            };
            let owner = match cursor.read_u8()? {
                0 => None,
                1 => Some(compact_pubkey(&pubkey_table, cursor.read_u8()?)?),
                tag => return Err(format!("invalid compact pubkey option tag {tag}")),
            };
            account_constraints.push(PolicyAccountConstraint {
                account_index,
                pubkeys,
                data_constraints,
                owner,
            });
        }
        let data_constraint_count = cursor.read_u8()? as usize;
        if data_constraint_count > 128 {
            return Err(format!(
                "program interaction data constraint count {data_constraint_count} exceeds maximum 128"
            ));
        }
        let data_constraints = read_policy_data_constraints(cursor, data_constraint_count)?;
        constraints.push(PolicyInstructionConstraint {
            program_id,
            account_constraints,
            data_constraints,
        });
    }
    Ok(constraints)
}

fn compact_pubkey(pubkey_table: &[Pubkey], index: u8) -> Result<Pubkey, String> {
    pubkey_table
        .get(index as usize)
        .copied()
        .ok_or_else(|| format!("compact pubkey table index {index} is out of bounds"))
}

fn read_program_interaction_account_constraints(
    cursor: &mut PolicyCursor<'_>,
    len: usize,
) -> Result<Vec<PolicyAccountConstraint>, String> {
    let mut constraints = Vec::with_capacity(len);
    for _ in 0..len {
        let account_index = cursor.read_u8()?;
        let (pubkeys, data_constraints) = match cursor.read_u8()? {
            0 => (
                cursor.read_pubkey_vec_u32("program interaction pubkey account constraint", 128)?,
                Vec::new(),
            ),
            1 => (Vec::new(), {
                let len = cursor
                    .read_u32_len("program interaction account data constraint count", 128)?;
                read_policy_data_constraints(cursor, len)?
            }),
            tag => {
                return Err(format!(
                    "unknown program interaction account constraint kind {tag}"
                ))
            }
        };
        let owner = cursor.read_option_pubkey()?;
        constraints.push(PolicyAccountConstraint {
            account_index,
            pubkeys,
            data_constraints,
            owner,
        });
    }
    Ok(constraints)
}

fn read_policy_data_constraints(
    cursor: &mut PolicyCursor<'_>,
    len: usize,
) -> Result<Vec<PolicyDataConstraint>, String> {
    let mut constraints = Vec::with_capacity(len);
    for _ in 0..len {
        constraints.push(PolicyDataConstraint {
            data_offset: cursor.read_u64()?,
            data_value: match cursor.read_u8()? {
                0 => PolicyDataValue::U8(cursor.read_u8()?),
                1 => PolicyDataValue::U16Le(cursor.read_u16()?),
                2 => PolicyDataValue::U32Le(cursor.read_u32()?),
                3 => PolicyDataValue::U64Le(cursor.read_u64()?),
                4 => PolicyDataValue::U128Le(cursor.read_u128()?),
                5 => PolicyDataValue::U8Slice(cursor.read_vec_u8("data u8 slice", 256)?),
                tag => return Err(format!("unknown data value kind {tag}")),
            },
            operator: match cursor.read_u8()? {
                0 => PolicyDataOperator::Equals,
                1 => PolicyDataOperator::NotEquals,
                2 => PolicyDataOperator::GreaterThan,
                3 => PolicyDataOperator::GreaterThanOrEqualTo,
                4 => PolicyDataOperator::LessThan,
                5 => PolicyDataOperator::LessThanOrEqualTo,
                tag => return Err(format!("unknown data operator {tag}")),
            },
        });
    }
    Ok(constraints)
}

fn instruction_discriminator(constraint: &PolicyInstructionConstraint) -> Option<Vec<u8>> {
    constraint
        .data_constraints
        .iter()
        .find_map(|data_constraint| {
            if data_constraint.data_offset == 0
                && data_constraint.operator == PolicyDataOperator::Equals
                && matches!(data_constraint.data_value, PolicyDataValue::U8Slice(_))
            {
                if let PolicyDataValue::U8Slice(value) = &data_constraint.data_value {
                    return Some(value.clone());
                }
            }
            None
        })
}

fn kamino_route_step(
    constraint: &PolicyInstructionConstraint,
    discriminator: Option<&[u8]>,
) -> Option<&'static str> {
    if constraint.program_id != KLEND_PROGRAM_ID {
        return None;
    }
    match discriminator {
        Some(value)
            if value
                .starts_with(&WITHDRAW_OBLIGATION_COLLATERAL_AND_REDEEM_RESERVE_COLLATERAL_V2) =>
        {
            Some(KAMINO_WITHDRAW_ROUTE_STEP)
        }
        Some(value)
            if value.starts_with(&DEPOSIT_RESERVE_LIQUIDITY_AND_OBLIGATION_COLLATERAL_V2) =>
        {
            Some(KAMINO_DEPOSIT_ROUTE_STEP)
        }
        Some(value) if value.starts_with(&INIT_OBLIGATION) => {
            Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP)
        }
        Some(value) if value.starts_with(&REFRESH_OBLIGATION) => {
            Some(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP)
        }
        _ => None,
    }
}

fn pubkeys_for_account(
    constraint: &PolicyInstructionConstraint,
    account_index: u8,
) -> Option<Vec<Pubkey>> {
    constraint
        .account_constraints
        .iter()
        .find(|constraint| constraint.account_index == account_index)
        .map(|constraint| constraint.pubkeys.clone())
}

fn account_data_pubkeys_for_account(
    constraint: &PolicyInstructionConstraint,
    account_index: u8,
    data_offset: u64,
    owner: Option<Pubkey>,
) -> Vec<Pubkey> {
    constraint
        .account_constraints
        .iter()
        .filter(|constraint| constraint.account_index == account_index && constraint.owner == owner)
        .flat_map(|constraint| {
            constraint
                .data_constraints
                .iter()
                .filter_map(move |data_constraint| {
                    if data_constraint.data_offset == data_offset
                        && data_constraint.operator == PolicyDataOperator::Equals
                    {
                        if let PolicyDataValue::U8Slice(value) = &data_constraint.data_value {
                            return value.as_slice().try_into().ok().map(Pubkey::new_from_array);
                        }
                    }
                    None
                })
        })
        .collect()
}

fn unique_pubkeys(pubkeys: Vec<Pubkey>) -> Vec<Pubkey> {
    let mut unique = Vec::new();
    for pubkey in pubkeys {
        if !unique.contains(&pubkey) {
            unique.push(pubkey);
        }
    }
    unique
}

fn extend_unique_strings(target: &mut Vec<String>, values: &[String]) {
    for value in values {
        if !target.contains(value) {
            target.push(value.clone());
        }
    }
}

#[derive(Clone)]
struct PolicyCursor<'a> {
    data: &'a [u8],
    offset: usize,
}

impl<'a> PolicyCursor<'a> {
    fn new(data: &'a [u8]) -> Self {
        Self { data, offset: 0 }
    }

    fn remaining(&self) -> usize {
        self.data.len().saturating_sub(self.offset)
    }

    fn skip(&mut self, len: usize) -> Result<(), String> {
        self.take(len).map(|_| ())
    }

    fn take(&mut self, len: usize) -> Result<&'a [u8], String> {
        if self.remaining() < len {
            return Err(format!(
                "truncated policy account data at offset {}, need {len} bytes",
                self.offset
            ));
        }
        let start = self.offset;
        self.offset += len;
        Ok(&self.data[start..self.offset])
    }

    fn read_array<const N: usize>(&mut self) -> Result<[u8; N], String> {
        self.take(N)?
            .try_into()
            .map_err(|_| "slice length mismatch".to_owned())
    }

    fn read_u8(&mut self) -> Result<u8, String> {
        Ok(self.take(1)?[0])
    }

    fn read_u16(&mut self) -> Result<u16, String> {
        Ok(u16::from_le_bytes(self.read_array()?))
    }

    fn read_u32(&mut self) -> Result<u32, String> {
        Ok(u32::from_le_bytes(self.read_array()?))
    }

    fn read_u64(&mut self) -> Result<u64, String> {
        Ok(u64::from_le_bytes(self.read_array()?))
    }

    fn read_u128(&mut self) -> Result<u128, String> {
        Ok(u128::from_le_bytes(self.read_array()?))
    }

    fn read_u32_len(&mut self, label: &str, max: usize) -> Result<usize, String> {
        let len = self.read_u32()? as usize;
        if len > max {
            return Err(format!("{label} {len} exceeds maximum {max}"));
        }
        Ok(len)
    }

    fn read_pubkey(&mut self) -> Result<Pubkey, String> {
        Ok(Pubkey::new_from_array(self.read_array()?))
    }

    fn read_vec_u8(&mut self, label: &str, max: usize) -> Result<Vec<u8>, String> {
        let len = self.read_u32_len(label, max)?;
        Ok(self.take(len)?.to_vec())
    }

    fn read_pubkey_vec_u32(&mut self, label: &str, max: usize) -> Result<Vec<Pubkey>, String> {
        let len = self.read_u32_len(label, max)?;
        (0..len).map(|_| self.read_pubkey()).collect()
    }

    fn read_option_pubkey(&mut self) -> Result<Option<Pubkey>, String> {
        match self.read_u8()? {
            0 => Ok(None),
            1 => self.read_pubkey().map(Some),
            tag => Err(format!("invalid pubkey option tag {tag}")),
        }
    }
}

fn chain_position_for_reserve<'a>(
    preview: &'a ChainReconcilePreview,
    reserve: &str,
) -> Result<&'a ChainPositionSummary, Box<dyn Error>> {
    preview
        .positions
        .iter()
        .find(|position| position.reserve == reserve)
        .ok_or_else(|| format!("chain preview missing required reserve {reserve}").into())
}

fn push_obligation_refresh_position<'a>(
    preview: &'a ChainReconcilePreview,
    seen: &mut BTreeSet<String>,
    positions: &mut Vec<&'a ChainPositionSummary>,
    reserve: &str,
    context: &str,
) -> Result<(), Box<dyn Error>> {
    let reserve = Pubkey::from_str(reserve)
        .map_err(|error| format!("invalid obligation refresh reserve {reserve}: {error}"))?
        .to_string();
    if !seen.insert(reserve.clone()) {
        return Ok(());
    }

    let position = chain_position_for_reserve(preview, &reserve).map_err(|_| {
        format!(
            "missing_obligation_refresh_reserve_metadata reserve {reserve} referenced by {context}; chain preview lacks metadata needed to build Kamino RefreshReserve"
        )
    })?;
    positions.push(position);
    Ok(())
}

fn obligation_refresh_positions_for_route<'a>(
    preview: &'a ChainReconcilePreview,
    source: &'a ChainPositionSummary,
    target: &'a ChainPositionSummary,
) -> Result<Vec<&'a ChainPositionSummary>, Box<dyn Error>> {
    let mut seen = BTreeSet::new();
    let mut positions = Vec::new();

    push_obligation_refresh_position(
        preview,
        &mut seen,
        &mut positions,
        &source.reserve,
        "selected source reserve",
    )?;
    push_obligation_refresh_position(
        preview,
        &mut seen,
        &mut positions,
        &target.reserve,
        "selected target reserve",
    )?;

    let source_deposit_context =
        format!("source obligation {} deposit reserves", source.obligation);
    for reserve in &source.obligation_deposit_reserves {
        push_obligation_refresh_position(
            preview,
            &mut seen,
            &mut positions,
            reserve,
            &source_deposit_context,
        )?;
    }
    let source_borrow_context = format!("source obligation {} borrow reserves", source.obligation);
    for reserve in &source.obligation_borrow_reserves {
        push_obligation_refresh_position(
            preview,
            &mut seen,
            &mut positions,
            reserve,
            &source_borrow_context,
        )?;
    }
    let target_deposit_context =
        format!("target obligation {} deposit reserves", target.obligation);
    for reserve in &target.obligation_deposit_reserves {
        push_obligation_refresh_position(
            preview,
            &mut seen,
            &mut positions,
            reserve,
            &target_deposit_context,
        )?;
    }
    let target_borrow_context = format!("target obligation {} borrow reserves", target.obligation);
    for reserve in &target.obligation_borrow_reserves {
        push_obligation_refresh_position(
            preview,
            &mut seen,
            &mut positions,
            reserve,
            &target_borrow_context,
        )?;
    }

    Ok(positions)
}

fn execution_preflight_blocker(
    chain_preview: Option<&ChainReconcilePreview>,
    policy_preflight: Option<&PolicyAccountPreflight>,
    reserve_move: &ReserveMove,
    route_execution: Option<&RouteExecutionPlan>,
) -> Option<String> {
    execution_preflight_blockers(
        chain_preview,
        policy_preflight,
        reserve_move,
        route_execution,
    )
    .into_iter()
    .next()
}

fn execution_preflight_blockers(
    chain_preview: Option<&ChainReconcilePreview>,
    policy_preflight: Option<&PolicyAccountPreflight>,
    reserve_move: &ReserveMove,
    route_execution: Option<&RouteExecutionPlan>,
) -> Vec<String> {
    let Some(chain_preview) = chain_preview else {
        return vec!["--execute requires --reconcile-from-chain".to_owned()];
    };

    let mut blockers = Vec::new();
    match chain_position_for_reserve(chain_preview, &reserve_move.source_reserve) {
        Ok(source) => {
            if !source.obligation_exists {
                blockers.push(format!(
                    "source obligation account {} does not exist",
                    source.obligation
                ));
            }
            if source.amount_raw == 0 {
                blockers.push(format!(
                    "source obligation account {} has zero deposited amount for reserve {}",
                    source.obligation, source.reserve
                ));
            }
            if !source.vault_liquidity_token_account_exists {
                blockers.push(format!(
                    "vault liquidity token account {} does not exist",
                    source.vault_liquidity_ata
                ));
            }
        }
        Err(error) => blockers.push(safe_same_mint_operational_error(error.as_ref())),
    }
    match chain_position_for_reserve(chain_preview, &reserve_move.target_reserve) {
        Ok(target) => {
            if !target.obligation_exists {
                match route_execution {
                    Some(plan) if plan.preview.missing_obligation_setup.is_some() => {}
                    Some(_) => blockers.push(format!(
                        "target obligation account {} does not exist and no inline init_obligation route step is planned before same-mint deposit",
                        target.obligation
                    )),
                    None => {}
                }
            }
        }
        Err(error) => blockers.push(safe_same_mint_operational_error(error.as_ref())),
    }
    if let Some(policy_preflight) = policy_preflight {
        let mut missing = Vec::new();
        if !policy_preflight.allows_required_route_steps() {
            missing.push("required same-mint KLend route steps");
        }
        if decoded_route_instruction_constraint_indexes(&policy_preflight.decoded).is_err() {
            missing.push("usable same-mint instruction constraint indexes");
        }
        if !policy_preflight.allows_required_markets() {
            missing.push("both required markets");
        }
        if !missing.is_empty() {
            blockers.push(format!(
                "decoded policy account does not allow {}",
                missing.join(" and ")
            ));
        }
    }
    if let Some(validation) =
        route_execution.and_then(|plan| plan.preview.policy_constraint_validation.as_ref())
    {
        if !validation.matches {
            blockers.push(format!(
                "decoded policy account constraints do not match built KLend v2 route: {}",
                validation.failures.join("; ")
            ));
        }
    }
    blockers
}

fn writes_current_positions_from_chain(options: &CliOptions) -> bool {
    // Queue execution treats the persisted source snapshot as the immutable
    // decision identity and validates it against a fresh chain preview. A
    // pre-decision reconcile would manufacture a successor snapshot that no
    // longer matches the fenced opportunity; projection happens only after
    // confirmation. Legacy/admin CLI execution retains its existing write.
    options.execute && options.reconcile_from_chain && options.opportunity_id.is_none()
}

fn writes_current_positions_from_user_seed(options: &CliOptions) -> bool {
    options.execute && options.seed_from_user_position
}

fn uses_chain_preview_positions(options: &CliOptions, has_chain_preview: bool) -> bool {
    has_chain_preview
        && options.reconcile_from_chain
        && (!options.execute || options.opportunity_id.is_some())
}

fn load_cached_fee_payer_balances(
    runtime: &SameMintRouteRuntime,
    fee_payers: &[Pubkey],
    optimizer_epoch_id: Option<i64>,
    min_context_slot: Option<u64>,
) -> Result<BTreeMap<Pubkey, FeePayerBalanceObservation>, Box<dyn Error>> {
    let mut observations = BTreeMap::new();
    let mut missing = Vec::new();
    {
        let mut cache = runtime
            .rpc_cache
            .fee_payer_balances
            .lock()
            .map_err(|_| "fee-payer balance RPC cache lock was poisoned")?;
        purge_ttl_cache(
            &mut cache,
            FEE_PAYER_BALANCE_CACHE_TTL,
            FEE_PAYER_BALANCE_CACHE_MAX_ENTRIES,
        );
        for fee_payer in fee_payers {
            if let Some(entry) = cache.get(fee_payer).filter(|entry| {
                entry.is_fresh_for(
                    optimizer_epoch_id,
                    min_context_slot,
                    FEE_PAYER_BALANCE_CACHE_TTL,
                )
            }) {
                if let Some(lamports) = entry.value {
                    observations.insert(
                        *fee_payer,
                        FeePayerBalanceObservation {
                            lamports,
                            context_slot: entry.context_slot,
                            observed_at: entry.observed_at,
                        },
                    );
                }
            } else {
                missing.push(*fee_payer);
            }
        }
    }
    if !missing.is_empty() {
        let (accounts, _) =
            get_multiple_accounts_batched(runtime.rpc.as_ref(), &missing, min_context_slot)?;
        let observed_at = Utc::now();
        let fetched_at = Instant::now();
        let mut fetched = Vec::with_capacity(missing.len());
        for (fee_payer, (account, context_slot)) in missing.into_iter().zip(accounts) {
            let balance = account.and_then(|account| {
                (account.owner == system_program::ID && !account.executable)
                    .then_some(account.lamports)
            });
            if let Some(lamports) = balance {
                observations.insert(
                    fee_payer,
                    FeePayerBalanceObservation {
                        lamports,
                        context_slot,
                        observed_at,
                    },
                );
            }
            fetched.push((fee_payer, balance, context_slot));
        }
        let mut cache = runtime
            .rpc_cache
            .fee_payer_balances
            .lock()
            .map_err(|_| "fee-payer balance RPC cache lock was poisoned")?;
        purge_ttl_cache(
            &mut cache,
            FEE_PAYER_BALANCE_CACHE_TTL,
            FEE_PAYER_BALANCE_CACHE_MAX_ENTRIES,
        );
        for (fee_payer, balance, context_slot) in fetched {
            cache.insert(
                fee_payer,
                CachedRpcValue {
                    value: balance,
                    context_slot,
                    optimizer_epoch_id,
                    observed_at,
                    fetched_at,
                },
            );
        }
        purge_ttl_cache(
            &mut cache,
            FEE_PAYER_BALANCE_CACHE_TTL,
            FEE_PAYER_BALANCE_CACHE_MAX_ENTRIES,
        );
    }
    Ok(observations)
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum FleetRouteFeePayerScope {
    MatureSameMint,
    ObligationSetup,
    IdleVault,
    FarmInit,
}

fn fee_only_shard_allowed_for_scope(scope: FleetRouteFeePayerScope) -> bool {
    scope == FleetRouteFeePayerScope::MatureSameMint
}

fn same_mint_route_fee_payer_scope(
    preview: &ChainReconcilePreview,
    reserve_move: &ReserveMove,
) -> Result<FleetRouteFeePayerScope, Box<dyn Error>> {
    let source = chain_position_for_reserve(preview, &reserve_move.source_reserve)?;
    let target = chain_position_for_reserve(preview, &reserve_move.target_reserve)?;
    if !source.obligation_exists || !target.obligation_exists {
        return Ok(FleetRouteFeePayerScope::ObligationSetup);
    }
    let farm_is_ready = |position: &ChainPositionSummary| {
        position.collateral_farm.is_none() || position.collateral_farm_user_state_exists
    };
    if !farm_is_ready(source) || !farm_is_ready(target) {
        return Ok(FleetRouteFeePayerScope::FarmInit);
    }
    Ok(FleetRouteFeePayerScope::MatureSameMint)
}

fn fee_payer_rendezvous_score(cluster: &str, vault_pubkey: &str, fee_payer: &Pubkey) -> [u8; 32] {
    let mut hasher = Sha256::new();
    hasher.update(cluster.as_bytes());
    hasher.update([0]);
    hasher.update(vault_pubkey.as_bytes());
    hasher.update([0]);
    hasher.update(fee_payer.as_ref());
    hasher.finalize().into()
}

fn bounded_ranked_fee_payer_pubkeys(
    cluster: &str,
    vault_pubkey: &str,
    mut fee_payers: Vec<Pubkey>,
) -> Vec<Pubkey> {
    fee_payers.sort_by(|left, right| {
        fee_payer_rendezvous_score(cluster, vault_pubkey, right)
            .cmp(&fee_payer_rendezvous_score(cluster, vault_pubkey, left))
            .then_with(|| left.to_string().cmp(&right.to_string()))
    });
    fee_payers.truncate(MAX_FEE_PAYER_SHARD_CANDIDATES);
    fee_payers
}

async fn select_same_mint_route_fee_payer(
    runtime: &SameMintRouteRuntime,
    options: &CliOptions,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    reserve_move: &ReserveMove,
) -> Result<RouteFeePayerSelection, Box<dyn Error>> {
    let policy_pubkey = policy_keypair_from_env()?.pubkey();
    let route_scope = same_mint_route_fee_payer_scope(preview, reserve_move)?;
    let mature_route = fee_only_shard_allowed_for_scope(route_scope);
    let expected_fee_payer = options
        .expected_route_fee_payer
        .as_deref()
        .map(Pubkey::from_str)
        .transpose()?;
    let expected_shard = expected_fee_payer.filter(|pubkey| *pubkey != policy_pubkey);
    let policy_fallback = |reason: &str| RouteFeePayerSelection {
        pubkey: policy_pubkey,
        kind: RouteFeePayerKind::Policy,
        reason: reason.to_owned(),
        mature_route,
        observed_balance_lamports: None,
        observed_balance_slot: None,
        observed_balance_at: None,
        shard: None,
    };
    if !options.optimization_cycle {
        return Ok(RouteFeePayerSelection {
            pubkey: solana_testing_keypair_from_env()?.pubkey(),
            kind: RouteFeePayerKind::Policy,
            reason: "admin_testing_fee_payer".to_owned(),
            mature_route,
            observed_balance_lamports: None,
            observed_balance_slot: None,
            observed_balance_at: None,
            shard: None,
        });
    }
    // Only the durable fleet queue receives fee-only shards. Legacy/admin
    // optimization remains a single-POLICY fallback path.
    if options.opportunity_id.is_none() {
        return Ok(policy_fallback("not_queue_backed"));
    }
    if options.execute && expected_fee_payer.is_none() {
        return Ok(policy_fallback("legacy_unbound_ready_route"));
    }
    if expected_fee_payer == Some(policy_pubkey) {
        return Ok(policy_fallback("durable_revalidation_policy_binding"));
    }
    if !fee_only_shard_allowed_for_scope(route_scope) {
        if expected_shard.is_some() {
            return Err(
                "fee_payer_reselection_required: bound shard route is no longer mature".into(),
            );
        }
        return Ok(policy_fallback("route_requires_obligation_or_farm_setup"));
    }
    let fee_payer_keypairs = match route_fee_payer_keypairs_from_env() {
        Ok(keypairs) if !keypairs.is_empty() => keypairs,
        Ok(_) if expected_shard.is_some() => {
            return Err(
                "fee_payer_reselection_required: bound shard keypool is unconfigured".into(),
            );
        }
        Ok(_) => return Ok(policy_fallback("fee_payer_keypool_unconfigured")),
        Err(_) if expected_shard.is_some() => {
            return Err("fee_payer_reselection_required: bound shard keypool is invalid".into());
        }
        Err(_) => return Ok(policy_fallback("fee_payer_keypool_invalid")),
    };
    let policy_pubkey_string = policy_pubkey.to_string();
    let (enabled_shards, authority_status) = tokio::join!(
        runtime
            .client
            .enabled_route_fee_payer_shards(&options.cluster),
        runtime
            .client
            .route_fee_payer_authority_status(&options.cluster, &policy_pubkey_string),
    );
    if !authority_status
        .as_ref()
        .is_ok_and(|status| status.policy_authority_and_payer_match())
    {
        if expected_shard.is_some() {
            return Err(
                "fee_payer_reselection_required: reusable ALT authority proof failed".into(),
            );
        }
        return Ok(policy_fallback("policy_alt_authority_proof_failed"));
    }
    let enabled_shards = match enabled_shards {
        Ok(shards) => shards,
        Err(_) if expected_shard.is_some() => {
            return Err("fee_payer_reselection_required: shard registry unavailable".into());
        }
        Err(_) => return Ok(policy_fallback("fee_payer_registry_unavailable")),
    };
    let mounted_pubkeys = fee_payer_keypairs
        .iter()
        .map(Signer::pubkey)
        .collect::<BTreeSet<_>>();
    let eligible = enabled_shards
        .into_iter()
        .filter_map(|shard| {
            let pubkey = Pubkey::from_str(&shard.fee_payer).ok()?;
            (shard.database_authority_separation_passes
                && pubkey != policy_pubkey
                && expected_shard.is_none_or(|expected| expected == pubkey)
                && mounted_pubkeys.contains(&pubkey))
            .then_some((pubkey, shard))
        })
        .collect::<Vec<_>>();
    if eligible.is_empty() {
        if expected_shard.is_some() {
            return Err("fee_payer_reselection_required: bound shard is no longer eligible".into());
        }
        return Ok(policy_fallback("no_exact_registry_keypair_match"));
    }
    let ranked_pubkeys = bounded_ranked_fee_payer_pubkeys(
        &options.cluster,
        &vault.vault_pubkey,
        eligible.iter().map(|(pubkey, _)| *pubkey).collect(),
    );
    let mut eligible_by_pubkey = eligible.into_iter().collect::<BTreeMap<_, _>>();
    let candidates = ranked_pubkeys
        .into_iter()
        .filter_map(|pubkey| {
            eligible_by_pubkey
                .remove(&pubkey)
                .map(|shard| (pubkey, shard))
        })
        .collect::<Vec<_>>();
    let candidate_pubkeys = candidates
        .iter()
        .map(|(pubkey, _)| *pubkey)
        .collect::<Vec<_>>();
    let candidate_balances = match load_cached_fee_payer_balances(
        runtime,
        &candidate_pubkeys,
        options.optimizer_epoch_id,
        options
            .optimizer_market_slot
            .map(u64::try_from)
            .transpose()?,
    ) {
        Ok(balances) => balances,
        Err(_) if expected_shard.is_some() => {
            return Err("bound fee-payer shard balance RPC is temporarily unavailable".into());
        }
        Err(_) => return Ok(policy_fallback("ranked_shard_balance_rpc_unavailable")),
    };
    for (assigned_pubkey, assigned_shard) in candidates {
        let preflight_fee_lamports = options
            .expected_cost_lamports
            .unwrap_or(assigned_shard.maximum_transaction_fee_lamports)
            .max(0);
        if preflight_fee_lamports > assigned_shard.maximum_transaction_fee_lamports
            || assigned_shard
                .current_window_reserved_lamports
                .checked_add(preflight_fee_lamports)
                .is_none_or(|spend| spend > assigned_shard.maximum_window_spend_lamports)
        {
            continue;
        }
        let Some(balance_observation) = candidate_balances.get(&assigned_pubkey) else {
            continue;
        };
        let observed_balance_lamports = i64::try_from(balance_observation.lamports)?;
        if observed_balance_lamports < assigned_shard.minimum_balance_lamports
            || observed_balance_lamports.saturating_sub(preflight_fee_lamports)
                < assigned_shard.minimum_balance_lamports
            || observed_balance_lamports > assigned_shard.maximum_balance_lamports
        {
            continue;
        }
        return Ok(RouteFeePayerSelection {
            pubkey: assigned_pubkey,
            kind: RouteFeePayerKind::FeeOnlyShard,
            reason: "ranked_rendezvous_mature_route_shard".to_owned(),
            mature_route,
            observed_balance_lamports: Some(observed_balance_lamports),
            observed_balance_slot: Some(i64::try_from(balance_observation.context_slot)?),
            observed_balance_at: Some(balance_observation.observed_at),
            shard: Some(assigned_shard),
        });
    }
    if expected_shard.is_some() {
        Err("fee_payer_reselection_required: bound shard is no longer healthy".into())
    } else {
        Ok(policy_fallback("no_healthy_ranked_shard"))
    }
}

fn same_mint_route_fee_payer_from_env(
    options: &CliOptions,
    expected_fee_payer: Pubkey,
) -> Result<Keypair, Box<dyn Error>> {
    if !options.optimization_cycle {
        let keypair = solana_testing_keypair_from_env()?;
        return (keypair.pubkey() == expected_fee_payer)
            .then_some(keypair)
            .ok_or_else(|| "admin fee payer does not match prepared route".into());
    }
    let policy = policy_keypair_from_env()?;
    if policy.pubkey() == expected_fee_payer {
        return Ok(policy);
    }
    route_fee_payer_keypairs_from_env()?
        .into_iter()
        .find(|keypair| keypair.pubkey() == expected_fee_payer)
        .ok_or_else(|| "prepared fee-only route payer is not mounted".into())
}

fn same_mint_route_signers<'a>(
    fee_payer: &'a dyn Signer,
    delegated_signer: &'a dyn Signer,
) -> Vec<&'a dyn Signer> {
    if fee_payer.pubkey() == delegated_signer.pubkey() {
        vec![fee_payer]
    } else {
        vec![fee_payer, delegated_signer]
    }
}

fn build_program_interaction_policy_execution_instruction(
    policy: Pubkey,
    signer_pubkey: Pubkey,
    account_index: u8,
    instruction: YieldRouteInstruction,
    instruction_constraint_index: u8,
) -> Result<(YieldRouteInstruction, usize, usize, usize), Box<dyn Error>> {
    guard_lookup_table_mutations(
        std::slice::from_ref(instruction.instruction()),
        "raw Squads program-interaction inner instruction",
    )?;
    let (instruction, mut requirements) = instruction.into_parts();
    let mut transaction_accounts = Vec::new();
    let compiled_instruction =
        compile_squads_inner_instruction(&mut transaction_accounts, instruction);
    let outer_instruction = execute_program_interaction_policy_instruction(
        policy,
        signer_pubkey,
        account_index,
        vec![compiled_instruction],
        vec![instruction_constraint_index],
        transaction_accounts.clone(),
    );
    requirements.add_policy(policy);
    Ok((
        YieldRouteInstruction::new(outer_instruction.clone(), requirements),
        1,
        transaction_accounts.len(),
        outer_instruction.accounts.len(),
    ))
}

fn planned_source_collateral_amount(
    input: &SameMintRebalanceInput,
    source: &ChainPositionSummary,
) -> Result<u64, Box<dyn Error>> {
    let Some(source_collateral_amount_raw) = input.source_collateral_amount_raw else {
        return Err(
            "planned same-mint route is missing source_collateral_amount_raw for Kamino withdraw"
                .into(),
        );
    };
    let source_collateral_amount =
        amount_i64_to_u64(source_collateral_amount_raw, "source collateral amount")?;
    if source_collateral_amount == 0 {
        return Err("source collateral amount must be greater than 0".into());
    }
    if source.amount_raw != source_collateral_amount {
        return Err(format!(
            "chain source reserve {} collateral amount {} does not match planned source_collateral_amount_raw {}",
            source.reserve, source.amount_raw, source_collateral_amount
        )
        .into());
    }
    Ok(source_collateral_amount)
}

fn same_mint_outer_lookup_table_requirements(
    vault: &SelectedVault,
) -> Result<YieldRouteLookupTableRequirements, Box<dyn Error>> {
    let mut requirements = YieldRouteLookupTableRequirements::new(
        Pubkey::from_str(&vault.settings)?,
        Pubkey::from_str(&vault.vault_pubkey)?,
    );
    requirements.add_policy(Pubkey::from_str(&vault.policy_account)?);
    if let Some(setup_policy_account) = vault.setup_policy_account.as_deref() {
        requirements.add_policy(Pubkey::from_str(setup_policy_account)?);
    }
    requirements.add_infrastructure_accounts([
        spl_token::ID,
        ASSOCIATED_TOKEN_PROGRAM_ID,
        solana_sdk::sysvar::instructions::id(),
        solana_sdk::sysvar::rent::id(),
        system_program::ID,
        Pubkey::default(),
    ]);

    Ok(requirements)
}

fn route_lookup_table_manifest(
    fee_payer: Pubkey,
    instructions: &[Instruction],
    vault: &SelectedVault,
    builder_requirements: &YieldRouteLookupTableRequirements,
    extra_vault_token_accounts: &[Pubkey],
) -> Result<LookupTableManifest, Box<dyn Error>> {
    let mut requirements = same_mint_outer_lookup_table_requirements(vault)?;
    requirements.merge(builder_requirements)?;
    for address in extra_vault_token_accounts {
        requirements.add_vault_token_account(*address);
    }
    requirements
        .manifest(fee_payer, instructions)
        .map_err(|error| format!("route ALT manifest is invalid: {error}").into())
}

fn policy_lookup_table_manifest(
    fee_payer: Pubkey,
    instructions: &[Instruction],
    vault: &SelectedVault,
    action_setups: &[&YieldRouteActionSetup],
    extra_policy_accounts: &[Pubkey],
) -> Result<LookupTableManifest, Box<dyn Error>> {
    let mut requirements = YieldRouteLookupTableRequirements::new(
        Pubkey::from_str(&vault.settings)?,
        Pubkey::from_str(&vault.vault_pubkey)?,
    );
    requirements.add_policy(Pubkey::from_str(&vault.policy_account)?);
    if let Some(setup_policy) = vault.setup_policy_account.as_deref() {
        requirements.add_policy(Pubkey::from_str(setup_policy)?);
    }
    for setup in action_setups {
        requirements.merge(setup.lookup_table_requirements())?;
    }
    for policy in extra_policy_accounts {
        requirements.add_policy(*policy);
    }
    requirements.add_infrastructure_accounts([
        system_program::ID,
        spl_token::ID,
        ASSOCIATED_TOKEN_PROGRAM_ID,
        solana_sdk::sysvar::instructions::id(),
        solana_sdk::sysvar::rent::id(),
        Pubkey::default(),
    ]);
    requirements
        .manifest(fee_payer, instructions)
        .map_err(|error| format!("policy ALT manifest is invalid: {error}").into())
}

fn build_route_execution_plan(
    rpc: Option<&RpcClient>,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    reserve_move: &ReserveMove,
    input: &SameMintRebalanceInput,
    policy_preflight: Option<&PolicyAccountPreflight>,
    fee_payer_selection: &RouteFeePayerSelection,
) -> Result<RouteExecutionPlan, Box<dyn Error>> {
    let fee_payer = fee_payer_selection.pubkey;
    let policy_account = Pubkey::from_str(&vault.policy_account)?;
    let signer_pubkey = policy_keypair_from_env()?.pubkey();
    if let Some(policy_preflight) = policy_preflight {
        if !policy_preflight
            .decoded
            .delegated_signers
            .iter()
            .any(|signer| signer == &signer_pubkey.to_string())
        {
            return Err(format!(
                "decoded policy account {} does not allow POLICY_KEYPAIR signer {}",
                vault.policy_account, signer_pubkey
            )
            .into());
        }
    }
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault index {} does not fit Squads account index",
            vault.vault_index
        )
    })?;
    let route_liquidity_amount = amount_i64_to_u64(input.amount_raw, "route liquidity amount")?;
    let source = chain_position_for_reserve(preview, &reserve_move.source_reserve)?;
    let target = chain_position_for_reserve(preview, &reserve_move.target_reserve)?;
    let source_collateral_amount = planned_source_collateral_amount(input, source)?;
    if input.redeemable_source_liquidity_amount_raw != Some(input.amount_raw) {
        return Err(format!(
            "planned redeemable_source_liquidity_amount_raw {:?} does not match route amount {}",
            input.redeemable_source_liquidity_amount_raw, input.amount_raw
        )
        .into());
    }
    if source.redeemable_liquidity_amount_raw != route_liquidity_amount {
        return Err(format!(
            "chain source reserve {} redeemable liquidity amount {} does not match planned route amount {}",
            source.reserve, source.redeemable_liquidity_amount_raw, route_liquidity_amount
        )
        .into());
    }
    if !vault
        .stable_mints
        .iter()
        .any(|mint| mint == &input.liquidity_mint)
    {
        return Err(format!(
            "selected policy {} does not allow stable mint {}",
            vault.policy_account, input.liquidity_mint
        )
        .into());
    }
    if !vault
        .kamino_liquidity_mints
        .iter()
        .any(|mint| mint == &input.liquidity_mint)
    {
        return Err(format!(
            "selected policy {} does not allow Kamino liquidity mint {}",
            vault.policy_account, input.liquidity_mint
        )
        .into());
    }
    if source.liquidity_mint != input.liquidity_mint {
        return Err(format!(
            "source reserve {} liquidity mint {} does not match planned mint {}",
            source.reserve, source.liquidity_mint, input.liquidity_mint
        )
        .into());
    }
    if target.liquidity_mint != input.liquidity_mint {
        return Err(format!(
            "target reserve {} liquidity mint {} does not match planned mint {}",
            target.reserve, target.liquidity_mint, input.liquidity_mint
        )
        .into());
    }
    let planned_liquidity_mint = Pubkey::from_str(&input.liquidity_mint)?;
    let vault_liquidity_ata =
        derive_associated_token_address(&vault_pubkey, &planned_liquidity_mint, &spl_token::ID);

    let refresh_positions = obligation_refresh_positions_for_route(preview, source, target)?;
    let refresh_reserves = refresh_positions
        .iter()
        .map(|position| position.reserve.clone())
        .collect::<Vec<_>>();
    let source_farm_init_instruction =
        kamino_init_obligation_collateral_farm_instruction(fee_payer, vault_pubkey, source)?;
    let target_farm_init_instruction =
        kamino_init_obligation_collateral_farm_instruction(fee_payer, vault_pubkey, target)?;
    let source_farm_setup_required = source_farm_init_instruction.is_some();
    let target_farm_setup_required = target_farm_init_instruction.is_some();
    if fee_payer_selection.kind == RouteFeePayerKind::Policy
        && (source_farm_setup_required || target_farm_setup_required)
    {
        let rpc = rpc.ok_or("farm setup funding preflight requires an RPC client")?;
        if rpc.get_balance(&fee_payer)? == 0 {
            return Err(format!(
                "route_funding_required: farm setup payer {fee_payer} has no lamports"
            )
            .into());
        }
    }
    let source_refresh_instruction = kamino_refresh_obligation_instruction(source)?;
    let target_refresh_instruction = kamino_refresh_obligation_instruction(target)?;
    let source_instruction = kamino_withdraw_instruction(
        vault_pubkey,
        source,
        vault_liquidity_ata,
        source_collateral_amount,
    )?;
    let target_instruction = kamino_deposit_to_obligation_instruction(
        vault_pubkey,
        target,
        vault_liquidity_ata,
        route_liquidity_amount,
    )?;
    let source_instruction_program = source_instruction.instruction().program_id.to_string();
    let target_instruction_program = target_instruction.instruction().program_id.to_string();
    let source_instruction_discriminator = source_instruction.instruction().data[..8].to_vec();
    let target_instruction_discriminator = target_instruction.instruction().data[..8].to_vec();
    let instruction_constraint_indexes =
        route_instruction_constraint_indexes(vault, policy_preflight)?;
    let withdraw_instruction_constraint_index = instruction_constraint_indexes
        .first()
        .copied()
        .ok_or("route policy is missing withdraw instruction constraint index")?;
    let deposit_instruction_constraint_index = instruction_constraint_indexes
        .get(1)
        .copied()
        .ok_or("route policy is missing deposit instruction constraint index")?;
    let policy_constraint_validation = policy_preflight.map(|policy_preflight| {
        let route = [
            (KAMINO_WITHDRAW_ROUTE_STEP, source_instruction.instruction()),
            (KAMINO_DEPOSIT_ROUTE_STEP, target_instruction.instruction()),
        ];
        validate_route_policy_constraints(
            &policy_preflight.decoded,
            &instruction_constraint_indexes,
            &route,
        )
    });

    let (
        withdraw_outer_instruction,
        withdraw_inner_count,
        withdraw_transaction_account_count,
        withdraw_outer_account_count,
    ) = build_program_interaction_policy_execution_instruction(
        policy_account,
        signer_pubkey,
        account_index,
        source_instruction,
        withdraw_instruction_constraint_index,
    )?;
    let (
        deposit_outer_instruction,
        deposit_inner_count,
        deposit_transaction_account_count,
        deposit_outer_account_count,
    ) = build_program_interaction_policy_execution_instruction(
        policy_account,
        signer_pubkey,
        account_index,
        target_instruction,
        deposit_instruction_constraint_index,
    )?;

    let mut routed_pre_instructions = refresh_positions
        .iter()
        .map(|position| kamino_refresh_reserve_instruction(position))
        .collect::<Result<Vec<_>, _>>()?;
    routed_pre_instructions.extend(source_farm_init_instruction);
    routed_pre_instructions.push(source_refresh_instruction);

    let mut routed_protected_and_public_instructions = vec![withdraw_outer_instruction];
    let mut route_steps = vec![KAMINO_WITHDRAW_ROUTE_STEP];
    let mut inner_instruction_count = withdraw_inner_count;
    let mut transaction_account_count = withdraw_transaction_account_count;
    let mut outer_account_count = withdraw_outer_account_count;
    let mut missing_obligation_setup = None;
    let mut setup_policy_account = None;
    let mut init_instruction_constraint_index = None;
    let mut setup_instruction_program = None;
    let mut setup_instruction_discriminator = None;

    if target.obligation_exists {
        routed_pre_instructions.extend(target_farm_init_instruction);
        routed_pre_instructions.push(target_refresh_instruction);
        routed_protected_and_public_instructions.push(
            kamino_refresh_obligation_for_reserves_instruction(target, &[target.reserve.as_str()])?,
        );
        route_steps.push(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP);
    } else {
        let rpc = rpc.ok_or(
            "inline target obligation setup requires an RPC client for exact rent funding",
        )?;
        let (init_policy, init_index) =
            resolve_init_obligation_policy(Some(rpc), vault, target, policy_preflight)?;
        let route_policy = Pubkey::from_str(&vault.policy_account)?;
        let policy_source = if init_policy == route_policy {
            "route_policy"
        } else {
            "setup_policy"
        };
        let (vault_rent_top_up, rent_top_up_instructions) =
            missing_obligation_setup_vault_rent_top_up_for_payer(rpc, vault_pubkey, fee_payer)?;
        for instruction in rent_top_up_instructions {
            routed_protected_and_public_instructions.push(YieldRouteInstruction::new(
                instruction,
                YieldRouteLookupTableRequirements::default(),
            ));
        }
        let init_instruction = kamino_init_obligation_instruction(vault_pubkey, target)?;
        setup_instruction_program = Some(init_instruction.instruction().program_id.to_string());
        setup_instruction_discriminator = Some(init_instruction.instruction().data[..8].to_vec());
        let (
            init_outer_instruction,
            init_inner_count,
            init_transaction_account_count,
            init_outer_account_count,
        ) = build_program_interaction_policy_execution_instruction(
            init_policy,
            signer_pubkey,
            account_index,
            init_instruction,
            init_index,
        )?;
        routed_protected_and_public_instructions.push(init_outer_instruction);
        routed_protected_and_public_instructions.extend(target_farm_init_instruction);
        routed_protected_and_public_instructions.push(target_refresh_instruction);
        if vault_rent_top_up.is_some() {
            route_steps.push(SYSTEM_TRANSFER_VAULT_RENT_TOP_UP_ROUTE_STEP);
        }
        route_steps.push(KAMINO_INIT_OBLIGATION_ROUTE_STEP);
        route_steps.push(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP);
        inner_instruction_count += init_inner_count;
        transaction_account_count += init_transaction_account_count;
        outer_account_count += init_outer_account_count;
        if policy_source == "setup_policy" {
            setup_policy_account = Some(init_policy.to_string());
        }
        init_instruction_constraint_index = Some(init_index);
        missing_obligation_setup = Some(InlineMissingObligationSetupPreview {
            target_obligation: target.obligation.clone(),
            target_reserve: target.reserve.clone(),
            target_market: target.market.clone(),
            policy_account: init_policy.to_string(),
            policy_source,
            instruction_constraint_index: init_index,
            vault_rent_top_up,
        });
    }

    routed_protected_and_public_instructions.push(deposit_outer_instruction);
    route_steps.push(KAMINO_DEPOSIT_ROUTE_STEP);
    inner_instruction_count += deposit_inner_count;
    transaction_account_count += deposit_transaction_account_count;
    outer_account_count += deposit_outer_account_count;

    if fee_payer_selection.kind == RouteFeePayerKind::FeeOnlyShard
        && (!fee_payer_selection.mature_route
            || source_farm_setup_required
            || target_farm_setup_required
            || missing_obligation_setup.is_some())
    {
        return Err(
            "fee-only route payer cannot fund obligation, farm, setup, or rent work".into(),
        );
    }

    let mut instruction_plan = YieldRouteInstructionPlan::with_outer_context(
        same_mint_outer_lookup_table_requirements(vault)?,
    );
    let mut pre_instructions = Vec::with_capacity(routed_pre_instructions.len());
    for routed_instruction in routed_pre_instructions {
        pre_instructions.push(routed_instruction.instruction().clone());
        instruction_plan.push(routed_instruction)?;
    }
    let mut protected_and_public_instructions =
        Vec::with_capacity(routed_protected_and_public_instructions.len());
    for routed_instruction in routed_protected_and_public_instructions {
        protected_and_public_instructions.push(routed_instruction.instruction().clone());
        instruction_plan.push(routed_instruction)?;
    }
    let lookup_table_manifest = instruction_plan
        .manifest(fee_payer)
        .map_err(|error| format!("same-mint route ALT manifest is invalid: {error}"))?;

    Ok(RouteExecutionPlan {
        pre_instructions,
        instructions: protected_and_public_instructions,
        lookup_table_manifest,
        preview: RouteExecutionPreview {
            policy_account: policy_account.to_string(),
            setup_policy_account,
            fee_payer: fee_payer.to_string(),
            fee_payer_kind: fee_payer_selection.kind,
            fee_payer_selection: fee_payer_selection.clone(),
            signer: signer_pubkey.to_string(),
            account_index,
            instruction_constraint_indexes,
            init_instruction_constraint_index,
            policy_constraint_validation,
            missing_obligation_setup,
            source_farm_setup_required,
            target_farm_setup_required,
            setup_instruction_program,
            setup_instruction_discriminator,
            route_steps,
            refresh_reserves,
            inner_instruction_count,
            transaction_account_count,
            outer_account_count,
            source_instruction_program,
            target_instruction_program,
            source_instruction_discriminator,
            target_instruction_discriminator,
        },
    })
}

fn build_initial_reserve_deposit_policy_plan(
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    policy_preflight: Option<&PolicyAccountPreflight>,
    deposit_reserve: &str,
    amount: u64,
    payer_pubkey: Pubkey,
    signer_pubkey: Pubkey,
    account_index: u8,
) -> Result<InitialDepositPolicyPlan, Box<dyn Error>> {
    let policy_account = Pubkey::from_str(&vault.policy_account)?;
    if let Some(policy_preflight) = policy_preflight {
        if !policy_preflight
            .decoded
            .delegated_signers
            .iter()
            .any(|signer| signer == &signer_pubkey.to_string())
        {
            return Err(format!(
                "decoded policy account {} does not allow POLICY_KEYPAIR signer {}",
                vault.policy_account, signer_pubkey
            )
            .into());
        }
    }
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let deposit = chain_position_for_reserve(preview, deposit_reserve)?;
    if !deposit.obligation_exists {
        return Err(format!(
            "deposit obligation {} is missing for reserve {}; run the missing-obligation setup transaction before policy deposit",
            deposit.obligation, deposit.reserve
        )
        .into());
    }
    let vault_liquidity_ata =
        derive_associated_token_address(&vault_pubkey, &USDC_MINT, &spl_token::ID);
    let reserve_refresh_instruction = kamino_refresh_reserve_instruction(deposit)?;
    let farm_init_instruction =
        kamino_init_obligation_collateral_farm_instruction(payer_pubkey, vault_pubkey, deposit)?;
    let refresh_instruction = kamino_refresh_obligation_instruction(deposit)?;
    let deposit_instruction = kamino_deposit_to_obligation_instruction(
        vault_pubkey,
        deposit,
        vault_liquidity_ata,
        amount,
    )?;
    guard_lookup_table_mutations(
        std::slice::from_ref(deposit_instruction.instruction()),
        "raw initial-deposit policy inner instruction",
    )?;
    let instruction_constraint_indexes =
        initial_deposit_instruction_constraint_indexes(policy_preflight)?;
    let policy_constraint_validation = policy_preflight.map(|policy_preflight| {
        let route = [(KAMINO_DEPOSIT_ROUTE_STEP, deposit_instruction.instruction())];
        validate_route_policy_constraints(
            &policy_preflight.decoded,
            &instruction_constraint_indexes,
            &route,
        )
    });
    if let Some(validation) = policy_constraint_validation.as_ref() {
        if !validation.matches {
            return Err(format!(
                "decoded policy account constraints do not match built initial reserve deposit: {}",
                validation.failures.join("; ")
            )
            .into());
        }
    }

    let deposit_instruction_program = deposit_instruction.instruction().program_id.to_string();
    let deposit_instruction_discriminator = deposit_instruction.instruction().data[..8].to_vec();
    let setup_instruction_program = farm_init_instruction
        .as_ref()
        .map(|instruction| instruction.instruction().program_id.to_string());
    let setup_instruction_discriminator = farm_init_instruction
        .as_ref()
        .map(|instruction| instruction.instruction().data[..8].to_vec());
    let has_farm_init = farm_init_instruction.is_some();
    let mut routed_pre_instructions = vec![reserve_refresh_instruction];
    if let Some(farm_init_instruction) = farm_init_instruction {
        routed_pre_instructions.push(farm_init_instruction);
    }
    routed_pre_instructions.push(refresh_instruction);

    let (deposit_instruction, mut deposit_requirements) = deposit_instruction.into_parts();
    let mut transaction_accounts = Vec::new();
    let deposit_compiled =
        compile_squads_inner_instruction(&mut transaction_accounts, deposit_instruction);
    let compiled_instructions = vec![deposit_compiled];
    let outer_instruction = execute_program_interaction_policy_instruction(
        policy_account,
        signer_pubkey,
        account_index,
        compiled_instructions.clone(),
        instruction_constraint_indexes.clone(),
        transaction_accounts.clone(),
    );
    deposit_requirements.add_policy(policy_account);
    let mut instruction_plan = YieldRouteInstructionPlan::with_outer_context(
        same_mint_outer_lookup_table_requirements(vault)?,
    );
    for instruction in routed_pre_instructions {
        instruction_plan.push(instruction)?;
    }
    instruction_plan.push(YieldRouteInstruction::new(
        outer_instruction.clone(),
        deposit_requirements,
    ))?;
    let (mut planned_instructions, lookup_table_requirements) = instruction_plan.into_parts();
    let instruction = planned_instructions
        .pop()
        .ok_or("initial-deposit route plan omitted policy instruction")?;

    Ok(InitialDepositPolicyPlan {
        pre_instructions: planned_instructions,
        instruction,
        lookup_table_requirements,
        preview: InitialDepositPolicyPreview {
            policy_account: policy_account.to_string(),
            signer: signer_pubkey.to_string(),
            account_index,
            instruction_constraint_indexes,
            policy_constraint_validation,
            setup_instruction_program,
            setup_instruction_discriminator,
            route_steps: if has_farm_init {
                vec![
                    KAMINO_INIT_OBLIGATION_FARM_ROUTE_STEP,
                    KAMINO_DEPOSIT_ROUTE_STEP,
                ]
            } else {
                vec![KAMINO_DEPOSIT_ROUTE_STEP]
            },
            inner_instruction_count: compiled_instructions.len(),
            transaction_account_count: transaction_accounts.len(),
            outer_account_count: outer_instruction.accounts.len(),
            deposit_instruction_program,
            deposit_instruction_discriminator,
        },
    })
}

fn build_full_main_usdc_withdraw_policy_plan(
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    policy_preflight: Option<&PolicyAccountPreflight>,
    signer_pubkey: Pubkey,
    account_index: u8,
    withdraw_reserve: &str,
) -> Result<FullWithdrawPolicyPlan, Box<dyn Error>> {
    let policy_account = Pubkey::from_str(&vault.policy_account)?;
    if let Some(policy_preflight) = policy_preflight {
        if !policy_preflight
            .decoded
            .delegated_signers
            .iter()
            .any(|signer| signer == &signer_pubkey.to_string())
        {
            return Err(format!(
                "decoded policy account {} does not allow POLICY_KEYPAIR signer {}",
                vault.policy_account, signer_pubkey
            )
            .into());
        }
    }
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let withdraw = chain_position_for_reserve(preview, withdraw_reserve)?;
    if withdraw.amount_raw == 0 {
        return Err(format!(
            "withdraw obligation account {} has zero deposited amount for reserve {}",
            withdraw.obligation, withdraw.reserve
        )
        .into());
    }
    let vault_liquidity_ata =
        derive_associated_token_address(&vault_pubkey, &USDC_MINT, &spl_token::ID);
    let reserve_refresh_instruction = kamino_refresh_reserve_instruction(withdraw)?;
    let refresh_instruction = kamino_refresh_obligation_instruction(withdraw)?;
    let withdraw_instruction = kamino_withdraw_instruction(
        vault_pubkey,
        withdraw,
        vault_liquidity_ata,
        withdraw.amount_raw,
    )?;
    guard_lookup_table_mutations(
        std::slice::from_ref(withdraw_instruction.instruction()),
        "raw full-withdraw policy inner instruction",
    )?;
    let instruction_constraint_indexes =
        full_withdraw_instruction_constraint_indexes(policy_preflight)?;
    let policy_constraint_validation = policy_preflight.map(|policy_preflight| {
        validate_route_policy_constraints(
            &policy_preflight.decoded,
            &instruction_constraint_indexes,
            &[(
                KAMINO_WITHDRAW_ROUTE_STEP,
                withdraw_instruction.instruction(),
            )],
        )
    });
    if let Some(validation) = policy_constraint_validation.as_ref() {
        if !validation.matches {
            return Err(format!(
                "decoded policy account constraints do not match built full reserve withdraw: {}",
                validation.failures.join("; ")
            )
            .into());
        }
    }

    let withdraw_instruction_program = withdraw_instruction.instruction().program_id.to_string();
    let withdraw_instruction_discriminator = withdraw_instruction.instruction().data[..8].to_vec();
    let (withdraw_instruction, mut withdraw_requirements) = withdraw_instruction.into_parts();
    let mut transaction_accounts = Vec::new();
    let withdraw_compiled =
        compile_squads_inner_instruction(&mut transaction_accounts, withdraw_instruction);
    let compiled_instructions = vec![withdraw_compiled];
    let outer_instruction = execute_program_interaction_policy_instruction(
        policy_account,
        signer_pubkey,
        account_index,
        compiled_instructions.clone(),
        instruction_constraint_indexes.clone(),
        transaction_accounts.clone(),
    );
    withdraw_requirements.add_policy(policy_account);
    let mut instruction_plan = YieldRouteInstructionPlan::with_outer_context(
        same_mint_outer_lookup_table_requirements(vault)?,
    );
    instruction_plan.push(reserve_refresh_instruction)?;
    instruction_plan.push(refresh_instruction)?;
    instruction_plan.push(YieldRouteInstruction::new(
        outer_instruction.clone(),
        withdraw_requirements,
    ))?;
    let (mut planned_instructions, lookup_table_requirements) = instruction_plan.into_parts();
    let instruction = planned_instructions
        .pop()
        .ok_or("full-withdraw route plan omitted policy instruction")?;

    Ok(FullWithdrawPolicyPlan {
        pre_instructions: planned_instructions,
        instruction,
        lookup_table_requirements,
        preview: FullWithdrawPolicyPreview {
            policy_account: policy_account.to_string(),
            signer: signer_pubkey.to_string(),
            account_index,
            instruction_constraint_indexes,
            policy_constraint_validation,
            route_steps: vec![KAMINO_WITHDRAW_ROUTE_STEP],
            inner_instruction_count: compiled_instructions.len(),
            transaction_account_count: transaction_accounts.len(),
            outer_account_count: outer_instruction.accounts.len(),
            withdraw_instruction_program,
            withdraw_instruction_discriminator,
        },
    })
}

async fn execute_prepared_same_mint_route(
    client: &NeonSqlClient,
    options: &CliOptions,
    vault: &SelectedVault,
    decision: &PreparedSameMintDecision,
    route_execution: &RouteExecutionPlan,
    predecision_lookup_tables: &RuntimeLookupTableResolution,
) -> Result<RouteExecutionSubmitResult, Box<dyn Error>> {
    let prepared_lease_reference = format!(
        "same-mint-decision:{}:{}",
        decision.id.as_i64(),
        predecision_lookup_tables
            .selection_fingerprint
            .as_deref()
            .ok_or("predecision lookup-table selection fingerprint is missing")?
    );
    let result = execute_prepared_same_mint_route_inner(
        client,
        options,
        vault,
        decision,
        route_execution,
        predecision_lookup_tables,
        &prepared_lease_reference,
    )
    .await;
    let _ = client
        .release_lookup_table_usage_leases(
            LookupTableUsageLeaseKind::PreparedTransaction,
            &prepared_lease_reference,
        )
        .await;
    release_route_resolution_lease(client, predecision_lookup_tables).await;
    result
}

async fn execute_prepared_same_mint_route_inner(
    client: &NeonSqlClient,
    options: &CliOptions,
    vault: &SelectedVault,
    decision: &PreparedSameMintDecision,
    route_execution: &RouteExecutionPlan,
    predecision_lookup_tables: &RuntimeLookupTableResolution,
    prepared_lease_reference: &str,
) -> Result<RouteExecutionSubmitResult, Box<dyn Error>> {
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let signer = policy_keypair_from_env()?;
    let expected_signer = Pubkey::from_str(&route_execution.preview.signer)?;
    if signer.pubkey() != expected_signer {
        return Err(format!(
            "POLICY_KEYPAIR pubkey {} does not match delegated signer {}",
            signer.pubkey(),
            expected_signer
        )
        .into());
    }
    let expected_fee_payer = Pubkey::from_str(&route_execution.preview.fee_payer)?;
    let fee_payer = same_mint_route_fee_payer_from_env(options, expected_fee_payer)?;
    if fee_payer.pubkey() != expected_fee_payer {
        return Err(format!(
            "route fee payer {} does not match prepared route fee payer {}",
            fee_payer.pubkey(),
            expected_fee_payer
        )
        .into());
    }
    let mut transaction_instructions = route_execution.pre_instructions.clone();
    transaction_instructions.extend(route_execution.instructions.iter().cloned());
    guard_lookup_table_mutations(&transaction_instructions, "route execution")?;
    let transaction_signers = same_mint_route_signers(&fee_payer, &signer);
    let lookup_table_scope = same_mint_route_lookup_table_scope_for_reserves(
        vault,
        &decision.source_reserve,
        &decision.target_reserve,
    );
    require_current_opportunity_fence(
        client,
        options,
        vault,
        Some((
            predecision_lookup_tables.route_fingerprint.as_str(),
            predecision_lookup_tables.requirements_fingerprint.as_str(),
        )),
    )
    .await?;
    let mut presend_lookup_tables = resolve_route_lookup_tables(
        client,
        &rpc,
        options,
        vault,
        &decision.source_reserve,
        &decision.target_reserve,
        "same_mint_kamino",
        &lookup_table_scope,
        fee_payer.pubkey(),
        &transaction_instructions,
        &route_execution.lookup_table_manifest,
        &transaction_signers,
    )
    .await?;
    persist_route_lookup_table_resolution(
        client,
        options,
        vault,
        &decision.source_reserve,
        &decision.target_reserve,
        "same_mint_kamino",
        &route_execution.lookup_table_manifest,
        &presend_lookup_tables,
        false,
        false,
    )
    .await?;
    ensure_lookup_table_resolution_unchanged(predecision_lookup_tables, &presend_lookup_tables)?;
    let selected_table_ids = presend_lookup_tables.selected_table_ids();
    let route_lease_reference = predecision_lookup_tables
        .route_lease_reference
        .as_deref()
        .ok_or("predecision route lookup-table lease reference is missing")?;
    let prepared_lease_expires_at =
        Utc::now() + ChronoDuration::minutes(LOOKUP_TABLE_PREPARED_LEASE_MINUTES);
    client
        .validate_lookup_table_usage_leases(
            LookupTableUsageLeaseKind::RouteResolution,
            route_lease_reference,
            &selected_table_ids,
            &presend_lookup_tables.requirements_fingerprint,
            prepared_lease_expires_at,
        )
        .await?;
    client
        .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
            cluster: options.cluster.clone(),
            lease_kind: LookupTableUsageLeaseKind::PreparedTransaction,
            reference_key: prepared_lease_reference.to_owned(),
            route_lookup_table_ids: selected_table_ids.clone(),
            vault_id: Some(vault.id),
            binding_id: presend_lookup_tables.active_binding_id,
            route_fingerprint: Some(presend_lookup_tables.route_fingerprint.clone()),
            requirements_fingerprint: Some(presend_lookup_tables.requirements_fingerprint.clone()),
            expires_at: prepared_lease_expires_at,
        })
        .await?;
    client
        .validate_lookup_table_usage_leases(
            LookupTableUsageLeaseKind::PreparedTransaction,
            prepared_lease_reference,
            &selected_table_ids,
            &presend_lookup_tables.requirements_fingerprint,
            Utc::now() + ChronoDuration::minutes(4),
        )
        .await?;
    if presend_lookup_tables.selection_kind == LookupTableSelectionKind::Reusable {
        let (binding_fingerprint_now, _) =
            active_lookup_table_binding_fingerprint(client, vault.id, &selected_table_ids).await?;
        if binding_fingerprint_now != presend_lookup_tables.active_binding_fingerprint {
            return Err(
                "active reusable lookup-table binding changed immediately before send".into(),
            );
        }
    }
    presend_lookup_tables.require_ready()?;
    let lookup_table_resolution = presend_lookup_tables.evidence.clone();
    let transaction = presend_lookup_tables
        .selected_transaction
        .take()
        .ok_or("pre-send lookup-table resolution did not return a signed transaction")?;
    let transaction_packet = presend_lookup_tables
        .selected_transaction_packet
        .take()
        .ok_or("pre-send lookup-table resolution did not return packet evidence")?;
    let simulation_units_consumed = presend_lookup_tables.selected_simulation_units_consumed;

    require_current_opportunity_fence(
        client,
        options,
        vault,
        Some((
            presend_lookup_tables.route_fingerprint.as_str(),
            presend_lookup_tables.requirements_fingerprint.as_str(),
        )),
    )
    .await?;
    client
        .advance_decision(decision.id, DecisionAdvance::StartSimulation)
        .await?;
    client
        .advance_decision(decision.id, DecisionAdvance::SimulationReady)
        .await?;

    require_current_opportunity_fence(
        client,
        options,
        vault,
        Some((
            presend_lookup_tables.route_fingerprint.as_str(),
            presend_lookup_tables.requirements_fingerprint.as_str(),
        )),
    )
    .await?;
    let submitted_slot = i64::try_from(rpc.get_slot()?)?;
    let signature = rpc.send_and_confirm_transaction(&transaction)?;
    let confirmed_slot = i64::try_from(rpc.get_slot()?)?;
    let signature = signature.to_string();
    client
        .advance_decision(
            decision.id,
            DecisionAdvance::Submit {
                signature: signature.clone(),
                slot: Some(submitted_slot),
            },
        )
        .await?;
    client
        .advance_decision(decision.id, DecisionAdvance::StartConfirmation)
        .await?;
    let post_reconcile_reserves = vec![
        decision.source_reserve.clone(),
        decision.target_reserve.clone(),
    ];
    let post_reconcile_preview = load_chain_reconcile_preview_with_min_context(
        &options.rpc_url,
        vault,
        &post_reconcile_reserves,
        Some(u64::try_from(confirmed_slot)?),
    )?;
    let post_reconcile_state = chain_preview_reconciled_state(&post_reconcile_preview)?;
    ensure_post_confirm_chain_reconcile_state(decision, &post_reconcile_state)?;
    let post_snapshot = client
        .reconcile_vault(decision.vault_id, post_reconcile_state)
        .await?;
    let confirmed = client
        .confirm_same_mint_rebalance(ConfirmSameMintRebalanceInput {
            decision_id: decision.id,
            signature: signature.clone(),
            submitted_slot: Some(submitted_slot),
            confirmed_slot,
            observed_at: Some(Utc::now()),
            post_snapshot_id: Some(post_snapshot.id),
        })
        .await?;

    Ok(RouteExecutionSubmitResult {
        signature,
        submitted_slot,
        confirmed_slot,
        simulation_units_consumed,
        transaction_packet,
        lookup_table_resolution,
        confirmed,
    })
}

async fn release_route_resolution_lease(
    client: &NeonSqlClient,
    resolution: &RuntimeLookupTableResolution,
) {
    if let Some(reference) = resolution.route_lease_reference.as_deref() {
        let _ = client
            .release_lookup_table_usage_leases(
                LookupTableUsageLeaseKind::RouteResolution,
                reference,
            )
            .await;
    }
}

fn validate_route_policy_constraints(
    decoded: &DecodedPolicyAccount,
    instruction_constraint_indexes: &[u8],
    route: &[(&'static str, &Instruction)],
) -> PolicyConstraintValidation {
    let mut failures = Vec::new();
    if instruction_constraint_indexes.len() != route.len() {
        failures.push(format!(
            "expected {} instruction constraint indexes, got {}",
            route.len(),
            instruction_constraint_indexes.len()
        ));
    }

    for (position, (route_step, instruction)) in route.iter().enumerate() {
        let Some(index) = instruction_constraint_indexes.get(position).copied() else {
            continue;
        };
        let Some(constraint) = decoded.constraints.get(index as usize) else {
            failures.push(format!(
                "{route_step} uses missing policy instruction constraint index {index}"
            ));
            continue;
        };
        failures.extend(validate_instruction_against_policy_constraint(
            route_step,
            constraint,
            instruction,
        ));
    }

    PolicyConstraintValidation {
        matches: failures.is_empty(),
        failures,
    }
}

fn validate_instruction_against_policy_constraint(
    route_step: &str,
    constraint: &PolicyInstructionConstraint,
    instruction: &Instruction,
) -> Vec<String> {
    let mut failures = Vec::new();
    if instruction.program_id != constraint.program_id {
        failures.push(format!(
            "{route_step} program id {} does not match policy program id {}",
            instruction.program_id, constraint.program_id
        ));
    }

    for account_constraint in &constraint.account_constraints {
        let Some(account_meta) = instruction
            .accounts
            .get(account_constraint.account_index as usize)
        else {
            failures.push(format!(
                "{route_step} policy account index {} is out of bounds for built instruction with {} accounts",
                account_constraint.account_index,
                instruction.accounts.len()
            ));
            continue;
        };
        if !account_constraint.pubkeys.is_empty()
            && !account_constraint.pubkeys.contains(&account_meta.pubkey)
        {
            failures.push(format!(
                "{route_step} policy account index {} expects one of [{}], built instruction has {}",
                account_constraint.account_index,
                account_constraint
                    .pubkeys
                    .iter()
                    .map(ToString::to_string)
                    .collect::<Vec<_>>()
                    .join(", "),
                account_meta.pubkey
            ));
        }
    }

    for data_constraint in &constraint.data_constraints {
        if let Err(reason) = policy_data_constraint_matches(data_constraint, &instruction.data) {
            failures.push(format!("{route_step} data constraint mismatch: {reason}"));
        }
    }

    failures
}

fn policy_data_constraint_matches(
    constraint: &PolicyDataConstraint,
    data: &[u8],
) -> Result<(), String> {
    let offset = usize::try_from(constraint.data_offset)
        .map_err(|_| format!("offset {} does not fit usize", constraint.data_offset))?;
    let passed = match &constraint.data_value {
        PolicyDataValue::U8(expected) => compare_policy_values(
            *data
                .get(offset)
                .ok_or_else(|| format!("data too short for u8 at offset {offset}"))?,
            *expected,
            constraint.operator,
        ),
        PolicyDataValue::U16Le(expected) => compare_policy_values(
            read_le_array::<2>(data, offset).map(u16::from_le_bytes)?,
            *expected,
            constraint.operator,
        ),
        PolicyDataValue::U32Le(expected) => compare_policy_values(
            read_le_array::<4>(data, offset).map(u32::from_le_bytes)?,
            *expected,
            constraint.operator,
        ),
        PolicyDataValue::U64Le(expected) => compare_policy_values(
            read_le_array::<8>(data, offset).map(u64::from_le_bytes)?,
            *expected,
            constraint.operator,
        ),
        PolicyDataValue::U128Le(expected) => compare_policy_values(
            read_le_array::<16>(data, offset).map(u128::from_le_bytes)?,
            *expected,
            constraint.operator,
        ),
        PolicyDataValue::U8Slice(expected) => {
            let actual = data
                .get(offset..offset + expected.len())
                .ok_or_else(|| format!("data too short for byte slice at offset {offset}"))?;
            match constraint.operator {
                PolicyDataOperator::Equals => actual == expected.as_slice(),
                PolicyDataOperator::NotEquals => actual != expected.as_slice(),
                other => {
                    return Err(format!(
                        "unsupported byte-slice operator {}",
                        other.as_str()
                    ))
                }
            }
        }
    };

    if passed {
        Ok(())
    } else {
        Err(format!(
            "operator {} failed at offset {}",
            constraint.operator.as_str(),
            constraint.data_offset
        ))
    }
}

fn read_le_array<const N: usize>(data: &[u8], offset: usize) -> Result<[u8; N], String> {
    data.get(offset..offset + N)
        .ok_or_else(|| format!("data too short for {N} bytes at offset {offset}"))?
        .try_into()
        .map_err(|_| format!("failed to read {N} bytes at offset {offset}"))
}

fn compare_policy_values<T: PartialOrd + PartialEq>(
    actual: T,
    expected: T,
    operator: PolicyDataOperator,
) -> bool {
    match operator {
        PolicyDataOperator::Equals => actual == expected,
        PolicyDataOperator::NotEquals => actual != expected,
        PolicyDataOperator::GreaterThan => actual > expected,
        PolicyDataOperator::GreaterThanOrEqualTo => actual >= expected,
        PolicyDataOperator::LessThan => actual < expected,
        PolicyDataOperator::LessThanOrEqualTo => actual <= expected,
    }
}

fn kamino_position_instruction_requirements(
    position: &ChainPositionSummary,
) -> Result<YieldRouteLookupTableRequirements, Box<dyn Error>> {
    let market = Pubkey::from_str(&position.market)?;
    let mut reserve_accounts = KaminoReserveLookupTableAccounts::new(
        market,
        Pubkey::from_str(&position.reserve)?,
        Pubkey::from_str(&position.liquidity_mint)?,
    );
    reserve_accounts.market_authorities =
        vec![lending_market_authority(&KLEND_PROGRAM_ID, &market).0];
    reserve_accounts.liquidity_supply = Some(Pubkey::from_str(&position.reserve_liquidity_supply)?);
    reserve_accounts.collateral_mint = Some(Pubkey::from_str(&position.collateral_mint)?);
    reserve_accounts.collateral_supply =
        Some(Pubkey::from_str(&position.reserve_collateral_supply)?);
    reserve_accounts.oracles = [
        position.pyth_oracle.as_deref(),
        position.switchboard_price_oracle.as_deref(),
        position.switchboard_twap_oracle.as_deref(),
    ]
    .into_iter()
    .flatten()
    .map(Pubkey::from_str)
    .collect::<Result<Vec<_>, _>>()?;
    reserve_accounts.scope_prices = position
        .scope_prices
        .as_deref()
        .map(Pubkey::from_str)
        .transpose()?;
    reserve_accounts.reserve_farm_state = position
        .collateral_farm
        .as_deref()
        .map(Pubkey::from_str)
        .transpose()?;
    reserve_accounts.obligation_reserves = position
        .obligation_deposit_reserves
        .iter()
        .chain(position.obligation_borrow_reserves.iter())
        .map(|reserve| Pubkey::from_str(reserve))
        .collect::<Result<Vec<_>, _>>()?;
    reserve_accounts.infrastructure = vec![
        KLEND_PROGRAM_ID,
        FARMS_PROGRAM_ID,
        Pubkey::from_str(&position.liquidity_token_program)?,
        solana_sdk::sysvar::instructions::id(),
        solana_sdk::sysvar::rent::id(),
        system_program::ID,
        Pubkey::default(),
    ];

    let mut requirements = YieldRouteLookupTableRequirements::default();
    requirements.add_kamino_reserve(reserve_accounts);
    requirements.add_obligation(Pubkey::from_str(&position.obligation)?);
    requirements.add_vault_token_account(Pubkey::from_str(&position.vault_liquidity_ata)?);
    if let Some(farm_user_state) = position.collateral_farm_user_state.as_deref() {
        requirements.add_farm_user_state(Pubkey::from_str(farm_user_state)?);
    }
    Ok(requirements)
}

fn kamino_withdraw_instruction(
    vault: Pubkey,
    source: &ChainPositionSummary,
    vault_liquidity_ata: Pubkey,
    amount: u64,
) -> Result<YieldRouteInstruction, Box<dyn Error>> {
    let reserve = Pubkey::from_str(&source.reserve)?;
    let market = Pubkey::from_str(&source.market)?;
    let liquidity_mint = Pubkey::from_str(&source.liquidity_mint)?;
    let collateral_mint = Pubkey::from_str(&source.collateral_mint)?;
    let reserve_liquidity_supply = Pubkey::from_str(&source.reserve_liquidity_supply)?;
    let reserve_collateral_supply = Pubkey::from_str(&source.reserve_collateral_supply)?;
    let liquidity_token_program = Pubkey::from_str(&source.liquidity_token_program)?;
    let (obligation_farm_user_state, reserve_farm_state) = collateral_farm_accounts(source)?;
    let (lending_market_authority, _) = lending_market_authority(&KLEND_PROGRAM_ID, &market);
    let (obligation_account, _) = obligation(
        &KLEND_PROGRAM_ID,
        0,
        0,
        &vault,
        &market,
        &Pubkey::default(),
        &Pubkey::default(),
    );

    let instruction = withdraw_obligation_collateral_and_redeem_reserve_collateral_v2(
        WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts {
            owner: vault,
            obligation: obligation_account,
            lending_market: market,
            lending_market_authority,
            withdraw_reserve: reserve,
            reserve_liquidity_mint: liquidity_mint,
            reserve_source_collateral: reserve_collateral_supply,
            reserve_collateral_mint: collateral_mint,
            reserve_liquidity_supply,
            user_destination_liquidity: vault_liquidity_ata,
            placeholder_user_destination_collateral: None,
            liquidity_token_program,
            obligation_farm_user_state,
            reserve_farm_state,
        },
        amount,
    );
    let mut requirements = kamino_position_instruction_requirements(source)?;
    requirements.add_vault_account(vault);
    requirements.add_obligation(obligation_account);
    requirements.add_vault_token_account(vault_liquidity_ata);
    Ok(YieldRouteInstruction::new(instruction, requirements))
}

fn kamino_deposit_to_obligation_instruction(
    vault: Pubkey,
    target: &ChainPositionSummary,
    vault_liquidity_ata: Pubkey,
    amount: u64,
) -> Result<YieldRouteInstruction, Box<dyn Error>> {
    let reserve = Pubkey::from_str(&target.reserve)?;
    let market = Pubkey::from_str(&target.market)?;
    let liquidity_mint = Pubkey::from_str(&target.liquidity_mint)?;
    let collateral_mint = Pubkey::from_str(&target.collateral_mint)?;
    let reserve_liquidity_supply = Pubkey::from_str(&target.reserve_liquidity_supply)?;
    let reserve_collateral_supply = Pubkey::from_str(&target.reserve_collateral_supply)?;
    let liquidity_token_program = Pubkey::from_str(&target.liquidity_token_program)?;
    let (obligation_farm_user_state, reserve_farm_state) = collateral_farm_accounts(target)?;
    let (lending_market_authority, _) = lending_market_authority(&KLEND_PROGRAM_ID, &market);
    let (obligation_account, _) = obligation(
        &KLEND_PROGRAM_ID,
        0,
        0,
        &vault,
        &market,
        &Pubkey::default(),
        &Pubkey::default(),
    );

    let instruction = deposit_reserve_liquidity_and_obligation_collateral_v2(
        DepositReserveLiquidityAndObligationCollateralV2Accounts {
            owner: vault,
            obligation: obligation_account,
            lending_market: market,
            lending_market_authority,
            reserve,
            reserve_liquidity_mint: liquidity_mint,
            reserve_liquidity_supply,
            reserve_collateral_mint: collateral_mint,
            reserve_destination_deposit_collateral: reserve_collateral_supply,
            user_source_liquidity: vault_liquidity_ata,
            placeholder_user_destination_collateral: None,
            liquidity_token_program,
            obligation_farm_user_state,
            reserve_farm_state,
        },
        amount,
    );
    let mut requirements = kamino_position_instruction_requirements(target)?;
    requirements.add_vault_account(vault);
    requirements.add_obligation(obligation_account);
    requirements.add_vault_token_account(vault_liquidity_ata);
    Ok(YieldRouteInstruction::new(instruction, requirements))
}

fn kamino_refresh_reserve_instruction(
    position: &ChainPositionSummary,
) -> Result<YieldRouteInstruction, Box<dyn Error>> {
    let instruction = refresh_reserve(RefreshReserveAccounts {
        reserve: Pubkey::from_str(&position.reserve)?,
        lending_market: Pubkey::from_str(&position.market)?,
        pyth_oracle: optional_pubkey_from_string(position.pyth_oracle.as_deref())?,
        switchboard_price_oracle: optional_pubkey_from_string(
            position.switchboard_price_oracle.as_deref(),
        )?,
        switchboard_twap_oracle: optional_pubkey_from_string(
            position.switchboard_twap_oracle.as_deref(),
        )?,
        scope_prices: optional_pubkey_from_string(position.scope_prices.as_deref())?,
    });
    Ok(YieldRouteInstruction::new(
        instruction,
        kamino_position_instruction_requirements(position)?,
    ))
}

fn kamino_init_obligation_collateral_farm_instruction(
    payer: Pubkey,
    owner: Pubkey,
    position: &ChainPositionSummary,
) -> Result<Option<YieldRouteInstruction>, Box<dyn Error>> {
    let Some(reserve_farm_state) = &position.collateral_farm else {
        return Ok(None);
    };
    if position.collateral_farm_user_state_exists {
        return Ok(None);
    }
    let obligation_farm = position
        .collateral_farm_user_state
        .as_deref()
        .ok_or("collateral farm state was present without a derived farm user state")?;
    let lending_market = Pubkey::from_str(&position.market)?;
    let reserve_farm_state = Pubkey::from_str(reserve_farm_state)?;
    let obligation = derive_kamino_vanilla_obligation(owner, lending_market);
    if Pubkey::from_str(&position.obligation)? != obligation {
        return Err(format!(
            "chain preview obligation {} does not match derived vanilla obligation {}",
            position.obligation, obligation
        )
        .into());
    }
    let derived_obligation_farm =
        derive_kamino_obligation_farm_user_state(reserve_farm_state, obligation);
    if Pubkey::from_str(obligation_farm)? != derived_obligation_farm {
        return Err(format!(
            "chain preview collateral farm user state {obligation_farm} does not match derived farm user state {derived_obligation_farm}"
        )
        .into());
    }

    let instruction = kamino_init_obligation_farm_instruction(KaminoInitObligationFarm {
        payer,
        owner,
        lending_market,
        reserve: Pubkey::from_str(&position.reserve)?,
        reserve_farm_state,
    });
    let mut requirements = kamino_position_instruction_requirements(position)?;
    requirements.add_vault_account(owner);
    requirements.add_obligation(obligation);
    requirements.add_kamino_farm(reserve_farm_state, derived_obligation_farm);
    requirements.add_infrastructure_accounts([FARMS_PROGRAM_ID, system_program::ID]);
    Ok(Some(YieldRouteInstruction::new(instruction, requirements)))
}

fn optional_pubkey_from_string(value: Option<&str>) -> Result<Option<Pubkey>, Box<dyn Error>> {
    value
        .map(Pubkey::from_str)
        .transpose()
        .map_err(|error| error.into())
}

fn collateral_farm_accounts(
    position: &ChainPositionSummary,
) -> Result<(Option<Pubkey>, Option<Pubkey>), Box<dyn Error>> {
    let Some(collateral_farm) = &position.collateral_farm else {
        return Ok((None, None));
    };
    let reserve_farm_state = Pubkey::from_str(collateral_farm)?;
    let obligation_account = Pubkey::from_str(&position.obligation)?;
    let (obligation_farm_user_state, _) =
        farms_user_state(&reserve_farm_state, &obligation_account);
    Ok((Some(obligation_farm_user_state), Some(reserve_farm_state)))
}

fn kamino_refresh_obligation_instruction(
    position: &ChainPositionSummary,
) -> Result<YieldRouteInstruction, Box<dyn Error>> {
    let remaining_reserves = position
        .obligation_deposit_reserves
        .iter()
        .chain(position.obligation_borrow_reserves.iter())
        .map(String::as_str)
        .collect::<Vec<_>>();

    kamino_refresh_obligation_for_reserves_instruction(position, &remaining_reserves)
}

fn kamino_refresh_obligation_for_reserves_instruction(
    position: &ChainPositionSummary,
    reserves: &[&str],
) -> Result<YieldRouteInstruction, Box<dyn Error>> {
    let lending_market = Pubkey::from_str(&position.market)?;
    let obligation = Pubkey::from_str(&position.obligation)?;
    let remaining_accounts = reserves
        .iter()
        .map(|reserve| {
            Pubkey::from_str(reserve)
                .map(|pubkey| AccountMeta::new(pubkey, false))
                .map_err(|error| format!("invalid obligation reserve {reserve}: {error}"))
        })
        .collect::<Result<Vec<_>, _>>()?;

    let instruction = refresh_obligation(
        RefreshObligationAccounts {
            lending_market,
            obligation,
        },
        remaining_accounts,
    );
    let mut requirements = kamino_position_instruction_requirements(position)?;
    requirements.add_obligation(obligation);
    for reserve in reserves {
        requirements.add_shared_reserve(Pubkey::from_str(reserve)?);
    }
    Ok(YieldRouteInstruction::new(instruction, requirements))
}

fn kamino_init_obligation_instruction(
    vault: Pubkey,
    target: &ChainPositionSummary,
) -> Result<YieldRouteInstruction, Box<dyn Error>> {
    let market = Pubkey::from_str(&target.market)?;
    let seed1 = Pubkey::default();
    let seed2 = Pubkey::default();
    let (obligation_account, _) =
        obligation(&KLEND_PROGRAM_ID, 0, 0, &vault, &market, &seed1, &seed2);
    let (owner_user_metadata, _) = user_metadata(&KLEND_PROGRAM_ID, &vault);

    let instruction = init_obligation(
        InitObligationAccounts {
            obligation_owner: vault,
            fee_payer: vault,
            obligation: obligation_account,
            lending_market: market,
            seed1_account: seed1,
            seed2_account: seed2,
            owner_user_metadata,
        },
        InitObligationArgs { tag: 0, id: 0 },
    );
    let mut requirements = kamino_position_instruction_requirements(target)?;
    requirements.add_vault_account(vault);
    requirements.add_obligation(obligation_account);
    requirements.add_metadata(owner_user_metadata);
    requirements.add_infrastructure_accounts([
        system_program::ID,
        solana_sdk::sysvar::rent::id(),
        Pubkey::default(),
    ]);
    Ok(YieldRouteInstruction::new(instruction, requirements))
}

fn route_instruction_constraint_indexes(
    vault: &SelectedVault,
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<Vec<u8>, Box<dyn Error>> {
    if let Some(policy_preflight) = policy_preflight {
        return decoded_route_instruction_constraint_indexes(&policy_preflight.decoded);
    }

    let _ = vault;
    Err("same-mint route requires decoded policy account indexes".into())
}

fn decoded_route_instruction_constraint_indexes(
    decoded: &DecodedPolicyAccount,
) -> Result<Vec<u8>, Box<dyn Error>> {
    let withdraw =
        decoded_instruction_index(decoded, KAMINO_WITHDRAW_ROUTE_STEP, "Kamino withdraw route")?;
    let deposit =
        decoded_instruction_index(decoded, KAMINO_DEPOSIT_ROUTE_STEP, "Kamino deposit route")?;
    let mut indexes = Vec::new();
    indexes.push(u8::try_from(withdraw)?);
    indexes.push(u8::try_from(deposit)?);
    Ok(indexes)
}

fn resolve_init_obligation_policy(
    rpc: Option<&RpcClient>,
    vault: &SelectedVault,
    target: &ChainPositionSummary,
    route_policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<(Pubkey, u8), Box<dyn Error>> {
    if let Some(preflight) = route_policy_preflight {
        if let Ok(index) = init_obligation_instruction_constraint_index(Some(preflight), target) {
            return Ok((Pubkey::from_str(&preflight.policy_account)?, index));
        }
    }

    let setup_policy_account = vault.setup_policy_account.as_deref().ok_or_else(|| {
        format!(
            "target obligation {} is missing, active policy {} has no authorized init_obligation path for target market {}, and no setup_policy_id is recorded for vault {}",
            target.obligation, vault.policy_account, target.market, vault.id
        )
    })?;
    let rpc = rpc.ok_or(
        "setup policy account decode requires an RPC client when target init is not in route policy",
    )?;
    let setup_policy = Pubkey::from_str(setup_policy_account)?;
    let account = rpc.get_account(&setup_policy)?;
    let decoded = decode_squads_policy_account(&account.data).map_err(|error| {
        format!("failed to decode setup policy account {setup_policy}: {error}")
    })?;
    let setup_preflight = PolicyAccountPreflight {
        policy_account: setup_policy_account.to_owned(),
        source_market: target.market.clone(),
        target_market: target.market.clone(),
        decoded,
    };
    let index = init_obligation_instruction_constraint_index(Some(&setup_preflight), target)?;
    Ok((setup_policy, index))
}

fn init_obligation_instruction_constraint_index(
    policy_preflight: Option<&PolicyAccountPreflight>,
    target: &ChainPositionSummary,
) -> Result<u8, Box<dyn Error>> {
    let Some(policy_preflight) = policy_preflight else {
        return Err("init obligation setup requires decoded policy account indexes".into());
    };
    let index = policy_preflight
        .decoded
        .instructions
        .iter()
        .position(|instruction| {
            instruction.route_step == Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP)
                && instruction
                    .markets
                    .iter()
                    .any(|market| market == &target.market)
        })
        .ok_or_else(|| {
            format!(
                "decoded policy account has no market-scoped init_obligation constraint for target market {}",
                target.market
            )
        })?;
    if index >= policy_preflight.decoded.instruction_count {
        return Err(format!(
            "decoded init_obligation index {index} exceeds policy instruction count {}",
            policy_preflight.decoded.instruction_count
        )
        .into());
    }
    Ok(u8::try_from(index)?)
}

fn initial_deposit_instruction_constraint_indexes(
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<Vec<u8>, Box<dyn Error>> {
    let Some(policy_preflight) = policy_preflight else {
        return Err("initial deposit requires decoded policy account indexes".into());
    };
    let decoded = &policy_preflight.decoded;
    let deposit =
        decoded_instruction_index(decoded, KAMINO_DEPOSIT_ROUTE_STEP, "Kamino deposit route")?;
    Ok(vec![u8::try_from(deposit)?])
}

fn full_withdraw_instruction_constraint_indexes(
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<Vec<u8>, Box<dyn Error>> {
    let Some(policy_preflight) = policy_preflight else {
        return Err("full withdraw requires decoded policy account indexes".into());
    };
    let decoded = &policy_preflight.decoded;
    let withdraw =
        decoded_instruction_index(decoded, KAMINO_WITHDRAW_ROUTE_STEP, "Kamino withdraw route")?;
    Ok(vec![u8::try_from(withdraw)?])
}

fn decoded_instruction_index(
    decoded: &DecodedPolicyAccount,
    route_step: &'static str,
    label: &'static str,
) -> Result<usize, Box<dyn Error>> {
    let index = decoded
        .instructions
        .iter()
        .position(|instruction| instruction.route_step == Some(route_step))
        .ok_or_else(|| format!("decoded policy account has no {label} constraint"))?;
    if index >= decoded.instruction_count {
        return Err(format!(
            "decoded {label} index {index} exceeds policy instruction count {}",
            decoded.instruction_count
        )
        .into());
    }
    Ok(index)
}

fn preview_position_summaries(
    preview: &ChainReconcilePreview,
    expected_source_snapshot_id: Option<i64>,
) -> Vec<PositionSummary> {
    preview
        .positions
        .iter()
        .map(|position| PositionSummary {
            reserve: position.reserve.clone(),
            liquidity_mint: position.liquidity_mint.clone(),
            amount_raw: i64::try_from(position.amount_raw).unwrap_or(i64::MAX),
            has_value: position.amount_raw > 0,
            // A prepare-only queue pass must retain the immutable DB snapshot
            // it is revalidating. The live chain preview refreshes amount and
            // account evidence, but it does not itself create a DB snapshot.
            snapshot_id: SnapshotId(expected_source_snapshot_id.unwrap_or_default()),
            supply_apy_bps: None,
            planning_metadata: json!({
                "source": "chain_reconcile_preview",
                "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
                "source_collateral_amount_raw": position.amount_raw.to_string(),
                "redeemable_source_liquidity_amount_raw": position.redeemable_liquidity_amount_raw.to_string(),
                "redeemable_liquidity_amount_raw": position.redeemable_liquidity_amount_raw.to_string(),
                "obligation": position.obligation,
                "obligation_exists": position.obligation_exists,
                "vault_liquidity_ata": position.vault_liquidity_ata,
                "vault_liquidity_token_account_exists": position.vault_liquidity_token_account_exists,
                "idle_vault_liquidity_amount_raw": position.vault_liquidity_amount_raw.to_string(),
                "vault_liquidity_amount_raw": position.vault_liquidity_amount_raw.to_string(),
            }),
        })
        .collect()
}

#[derive(Clone, Debug)]
struct KaminoReserveSummary {
    market: Pubkey,
    liquidity_mint: Pubkey,
    liquidity_token_program: Pubkey,
    liquidity_supply: Pubkey,
    collateral_mint: Pubkey,
    collateral_supply: Pubkey,
    collateral_farm: Option<Pubkey>,
    pyth_oracle: Option<Pubkey>,
    switchboard_price_oracle: Option<Pubkey>,
    switchboard_twap_oracle: Option<Pubkey>,
    scope_prices: Option<Pubkey>,
    collateral_total_supply: u64,
    total_liquidity_scaled: BigUint,
}

impl KaminoReserveSummary {
    fn derivation_identity_matches(&self, other: &Self) -> bool {
        self.market == other.market
            && self.liquidity_mint == other.liquidity_mint
            && self.liquidity_token_program == other.liquidity_token_program
            && self.liquidity_supply == other.liquidity_supply
            && self.collateral_mint == other.collateral_mint
            && self.collateral_supply == other.collateral_supply
            && self.collateral_farm == other.collateral_farm
            && self.pyth_oracle == other.pyth_oracle
            && self.switchboard_price_oracle == other.switchboard_price_oracle
            && self.switchboard_twap_oracle == other.switchboard_twap_oracle
            && self.scope_prices == other.scope_prices
    }
}

fn load_kamino_reserve_summary(
    rpc: &RpcClient,
    reserve: &Pubkey,
) -> Result<KaminoReserveSummary, Box<dyn Error>> {
    load_kamino_reserve_summary_at_or_after(rpc, reserve, None)
}

fn load_kamino_reserve_summary_at_or_after(
    rpc: &RpcClient,
    reserve: &Pubkey,
    min_context_slot: Option<u64>,
) -> Result<KaminoReserveSummary, Box<dyn Error>> {
    let response = rpc.get_account_with_config(
        reserve,
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::confirmed()),
            min_context_slot,
            ..RpcAccountInfoConfig::default()
        },
    )?;
    let account = response
        .value
        .ok_or_else(|| format!("reserve account {reserve} does not exist"))?;
    decode_kamino_reserve_summary(reserve, &account)
}

fn decode_kamino_reserve_summary(
    reserve: &Pubkey,
    account: &Account,
) -> Result<KaminoReserveSummary, Box<dyn Error>> {
    if account.owner != KLEND_PROGRAM_ID {
        return Err(format!(
            "reserve {reserve} is owned by {}, expected live Kamino lend program {}",
            account.owner, KLEND_PROGRAM_ID
        )
        .into());
    }
    let reserve_state = from_account_data::<Reserve>(&account.data)?;
    Ok(KaminoReserveSummary {
        market: reserve_state.lending_market,
        liquidity_mint: reserve_state.liquidity.mint_pubkey,
        liquidity_token_program: reserve_state.liquidity.token_program,
        liquidity_supply: reserve_state.liquidity.supply_vault,
        collateral_mint: reserve_state.collateral.mint_pubkey,
        collateral_supply: reserve_state.collateral.supply_vault,
        collateral_total_supply: reserve_state.collateral.mint_total_supply,
        total_liquidity_scaled: reserve_total_liquidity_scaled(&reserve_state)?,
        collateral_farm: if reserve_state.farm_collateral == Pubkey::default() {
            None
        } else {
            Some(reserve_state.farm_collateral)
        },
        pyth_oracle: non_default_pubkey(reserve_state.config.token_info.pyth_configuration.price),
        switchboard_price_oracle: non_default_pubkey(
            reserve_state
                .config
                .token_info
                .switchboard_configuration
                .price_aggregator,
        ),
        switchboard_twap_oracle: non_default_pubkey(
            reserve_state
                .config
                .token_info
                .switchboard_configuration
                .twap_aggregator,
        ),
        scope_prices: non_default_pubkey(
            reserve_state
                .config
                .token_info
                .scope_configuration
                .price_feed,
        ),
    })
}

fn reserve_total_liquidity_scaled(reserve: &Reserve) -> Result<BigUint, Box<dyn Error>> {
    let scale = BigUint::from(1_u128 << 60);
    let mut total = BigUint::from(reserve.liquidity.total_available_amount) * &scale;
    total += BigUint::from(u128::from(reserve.liquidity.borrowed_amount_sf));
    subtract_scaled_fraction(
        &mut total,
        u128::from(reserve.liquidity.accumulated_protocol_fees_sf),
        "accumulated protocol fees",
    )?;
    subtract_scaled_fraction(
        &mut total,
        u128::from(reserve.liquidity.accumulated_referrer_fees_sf),
        "accumulated referrer fees",
    )?;
    subtract_scaled_fraction(
        &mut total,
        u128::from(reserve.liquidity.pending_referrer_fees_sf),
        "pending referrer fees",
    )?;
    Ok(total)
}

fn subtract_scaled_fraction(
    total: &mut BigUint,
    amount: u128,
    label: &'static str,
) -> Result<(), Box<dyn Error>> {
    let amount = BigUint::from(amount);
    if (&*total) < &amount {
        return Err(format!("reserve total liquidity underflow subtracting {label}").into());
    }
    *total -= amount;
    Ok(())
}

fn collateral_to_redeemable_liquidity_amount(
    collateral_total_supply: u64,
    total_liquidity_scaled: &BigUint,
    collateral_amount: u64,
) -> Result<u64, Box<dyn Error>> {
    if collateral_amount == 0 {
        return Ok(0);
    }
    if collateral_total_supply == 0 || total_liquidity_scaled.is_zero() {
        return Ok(collateral_amount);
    }

    let scale = BigUint::from(1_u128 << 60);
    let numerator = BigUint::from(collateral_amount) * total_liquidity_scaled;
    let denominator = BigUint::from(collateral_total_supply) * scale;
    (numerator / denominator)
        .to_u64()
        .ok_or_else(|| "redeemable liquidity amount does not fit u64".into())
}

fn non_default_pubkey(pubkey: Pubkey) -> Option<Pubkey> {
    if pubkey == Pubkey::default() {
        None
    } else {
        Some(pubkey)
    }
}

struct KaminoObligationSummary {
    exists: bool,
    reserve_deposited_amount_raw: u64,
    deposit_reserves: Vec<String>,
    borrow_reserves: Vec<String>,
}

fn load_kamino_obligation_summary(
    rpc: &RpcClient,
    obligation_account: &Pubkey,
    expected_owner: &Pubkey,
    expected_market: &Pubkey,
    reserve: &Pubkey,
) -> Result<KaminoObligationSummary, Box<dyn Error>> {
    load_kamino_obligation_summary_at_or_after(
        rpc,
        obligation_account,
        expected_owner,
        expected_market,
        reserve,
        None,
    )
}

fn load_kamino_obligation_summary_at_or_after(
    rpc: &RpcClient,
    obligation_account: &Pubkey,
    expected_owner: &Pubkey,
    expected_market: &Pubkey,
    reserve: &Pubkey,
    min_context_slot: Option<u64>,
) -> Result<KaminoObligationSummary, Box<dyn Error>> {
    let response = rpc.get_account_with_config(
        obligation_account,
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::confirmed()),
            min_context_slot,
            ..RpcAccountInfoConfig::default()
        },
    )?;
    decode_kamino_obligation_summary(
        response.value.as_ref(),
        obligation_account,
        expected_owner,
        expected_market,
        reserve,
    )
}

fn decode_kamino_obligation_summary(
    account: Option<&Account>,
    obligation_account: &Pubkey,
    expected_owner: &Pubkey,
    expected_market: &Pubkey,
    reserve: &Pubkey,
) -> Result<KaminoObligationSummary, Box<dyn Error>> {
    let Some(account) = account else {
        return Ok(KaminoObligationSummary {
            exists: false,
            reserve_deposited_amount_raw: 0,
            deposit_reserves: Vec::new(),
            borrow_reserves: Vec::new(),
        });
    };
    if account.owner != KLEND_PROGRAM_ID {
        return Err(format!(
            "obligation account {obligation_account} is owned by {}, expected {}",
            account.owner, KLEND_PROGRAM_ID
        )
        .into());
    }
    let obligation_state = from_account_data::<Obligation>(&account.data)?;
    if obligation_state.owner != *expected_owner {
        return Err(format!(
            "obligation account {obligation_account} owner {} does not match vault {}",
            obligation_state.owner, expected_owner
        )
        .into());
    }
    if obligation_state.lending_market != *expected_market {
        return Err(format!(
            "obligation account {obligation_account} market {} does not match reserve market {}",
            obligation_state.lending_market, expected_market
        )
        .into());
    }

    let amount = obligation_state
        .deposits
        .iter()
        .find(|deposit| deposit.deposit_reserve == *reserve)
        .map(|deposit| deposit.deposited_amount)
        .unwrap_or_default();
    let deposit_reserves = obligation_state
        .deposits
        .iter()
        .filter(|deposit| deposit.deposit_reserve != Pubkey::default())
        .map(|deposit| deposit.deposit_reserve.to_string())
        .collect();
    let borrow_reserves = obligation_state
        .borrows
        .iter()
        .filter(|borrow| borrow.borrow_reserve != Pubkey::default())
        .map(|borrow| borrow.borrow_reserve.to_string())
        .collect();

    Ok(KaminoObligationSummary {
        exists: true,
        reserve_deposited_amount_raw: amount,
        deposit_reserves,
        borrow_reserves,
    })
}

fn pubkey_from_account_data(
    data: &[u8],
    offset: usize,
    field: &'static str,
) -> Result<Pubkey, Box<dyn Error>> {
    let bytes = data
        .get(offset..offset + PUBKEY_LEN)
        .ok_or_else(|| format!("account data too short for {field} at offset {offset}"))?;
    Ok(Pubkey::new_from_array(bytes.try_into()?))
}

fn derive_associated_token_address(
    owner: &Pubkey,
    mint: &Pubkey,
    token_program: &Pubkey,
) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), token_program.as_ref(), mint.as_ref()],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

fn create_associated_token_account_idempotent_instruction(
    funding_address: Pubkey,
    wallet_address: Pubkey,
    token_mint_address: Pubkey,
    token_program_id: Pubkey,
) -> Instruction {
    let associated_account_address =
        derive_associated_token_address(&wallet_address, &token_mint_address, &token_program_id);
    Instruction {
        program_id: ASSOCIATED_TOKEN_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(funding_address, true),
            AccountMeta::new(associated_account_address, false),
            AccountMeta::new_readonly(wallet_address, false),
            AccountMeta::new_readonly(token_mint_address, false),
            AccountMeta::new_readonly(system_program::ID, false),
            AccountMeta::new_readonly(token_program_id, false),
        ],
        data: vec![1],
    }
}

fn load_spl_token_account_amount(
    rpc: &RpcClient,
    token_account: &Pubkey,
    expected_mint: &Pubkey,
) -> Result<(u64, bool), Box<dyn Error>> {
    load_spl_token_account_amount_at_or_after(rpc, token_account, expected_mint, None)
}

fn load_spl_token_account_amount_at_or_after(
    rpc: &RpcClient,
    token_account: &Pubkey,
    expected_mint: &Pubkey,
    min_context_slot: Option<u64>,
) -> Result<(u64, bool), Box<dyn Error>> {
    let response = rpc.get_account_with_config(
        token_account,
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::confirmed()),
            min_context_slot,
            ..RpcAccountInfoConfig::default()
        },
    )?;
    decode_spl_token_account_amount(response.value.as_ref(), token_account, expected_mint)
}

fn decode_spl_token_account_amount(
    account: Option<&Account>,
    token_account: &Pubkey,
    expected_mint: &Pubkey,
) -> Result<(u64, bool), Box<dyn Error>> {
    let Some(account) = account else {
        return Ok((0, false));
    };
    if account.owner != spl_token::ID {
        return Err(format!(
            "token account {token_account} is owned by {}, expected {}",
            account.owner,
            spl_token::ID
        )
        .into());
    }
    let mint = pubkey_from_account_data(
        &account.data,
        SPL_TOKEN_ACCOUNT_MINT_OFFSET,
        "token account mint",
    )?;
    if mint != *expected_mint {
        return Err(format!(
            "token account {token_account} mint {mint} does not match expected {expected_mint}"
        )
        .into());
    }
    let amount_bytes = account
        .data
        .get(SPL_TOKEN_ACCOUNT_AMOUNT_OFFSET..SPL_TOKEN_ACCOUNT_AMOUNT_OFFSET + 8)
        .ok_or_else(|| {
            format!(
                "token account data too short for amount at offset {SPL_TOKEN_ACCOUNT_AMOUNT_OFFSET}"
            )
        })?;
    Ok((u64::from_le_bytes(amount_bytes.try_into()?), true))
}

fn load_account_proof(rpc: &RpcClient, pubkey: &Pubkey) -> Result<AccountProof, Box<dyn Error>> {
    let response = rpc.get_account_with_commitment(pubkey, CommitmentConfig::confirmed())?;
    let Some(account) = response.value else {
        return Ok(AccountProof {
            pubkey: pubkey.to_string(),
            exists: false,
            lamports: 0,
            owner: None,
        });
    };
    Ok(AccountProof {
        pubkey: pubkey.to_string(),
        exists: true,
        lamports: account.lamports,
        owner: Some(account.owner.to_string()),
    })
}

fn load_obligation_account_proof(
    rpc: &RpcClient,
    obligation_account: &Pubkey,
    expected_owner: &Pubkey,
    expected_market: &Pubkey,
    reserve: &Pubkey,
) -> Result<ObligationAccountProof, Box<dyn Error>> {
    let response =
        rpc.get_account_with_commitment(obligation_account, CommitmentConfig::confirmed())?;
    let Some(account) = response.value else {
        return Ok(ObligationAccountProof {
            account: AccountProof {
                pubkey: obligation_account.to_string(),
                exists: false,
                lamports: 0,
                owner: None,
            },
            owner: None,
            lending_market: None,
            active_deposit_count: None,
            active_borrow_count: None,
            reserve_deposited_amount_raw: None,
        });
    };
    if account.owner != KLEND_PROGRAM_ID {
        return Err(format!(
            "obligation account {obligation_account} is owned by {}, expected {}",
            account.owner, KLEND_PROGRAM_ID
        )
        .into());
    }
    let obligation_state = from_account_data::<Obligation>(&account.data)?;
    if obligation_state.owner != *expected_owner {
        return Err(format!(
            "obligation account {obligation_account} owner {} does not match vault {}",
            obligation_state.owner, expected_owner
        )
        .into());
    }
    if obligation_state.lending_market != *expected_market {
        return Err(format!(
            "obligation account {obligation_account} market {} does not match expected {}",
            obligation_state.lending_market, expected_market
        )
        .into());
    }
    let reserve_deposited_amount_raw = obligation_state
        .deposits
        .iter()
        .find(|deposit| deposit.deposit_reserve == *reserve)
        .map(|deposit| deposit.deposited_amount);
    Ok(ObligationAccountProof {
        account: AccountProof {
            pubkey: obligation_account.to_string(),
            exists: true,
            lamports: account.lamports,
            owner: Some(account.owner.to_string()),
        },
        owner: Some(obligation_state.owner.to_string()),
        lending_market: Some(obligation_state.lending_market.to_string()),
        active_deposit_count: Some(obligation_state.num_deposits()),
        active_borrow_count: Some(obligation_state.num_borrows()),
        reserve_deposited_amount_raw,
    })
}

fn account_exists_with_owner(
    rpc: &RpcClient,
    pubkey: &Pubkey,
    expected_owner: &Pubkey,
) -> Result<bool, Box<dyn Error>> {
    account_exists_with_owner_at_or_after(rpc, pubkey, expected_owner, None)
}

fn account_exists_with_owner_at_or_after(
    rpc: &RpcClient,
    pubkey: &Pubkey,
    expected_owner: &Pubkey,
    min_context_slot: Option<u64>,
) -> Result<bool, Box<dyn Error>> {
    let response = rpc.get_account_with_config(
        pubkey,
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::confirmed()),
            min_context_slot,
            ..RpcAccountInfoConfig::default()
        },
    )?;
    account_exists_with_owner_from_account(response.value.as_ref(), pubkey, expected_owner)
}

fn account_exists_with_owner_from_account(
    account: Option<&Account>,
    pubkey: &Pubkey,
    expected_owner: &Pubkey,
) -> Result<bool, Box<dyn Error>> {
    let Some(account) = account else {
        return Ok(false);
    };
    if account.owner != *expected_owner {
        return Err(format!(
            "account {pubkey} is owned by {}, expected {}",
            account.owner, expected_owner
        )
        .into());
    }
    Ok(true)
}

fn dedup_strings_in_place(values: &mut Vec<String>) {
    let mut deduped = Vec::with_capacity(values.len());
    for value in values.drain(..) {
        if !deduped.contains(&value) {
            deduped.push(value);
        }
    }
    *values = deduped;
}

async fn connect(database_url: &str) -> Result<PgPool, loyal_yield_orchestrator::sqlx::Error> {
    let options = PgConnectOptions::from_str(database_url)?.statement_cache_capacity(0);
    PgPoolOptions::new()
        .max_connections(2)
        .connect_with(options)
        .await
}

async fn load_active_vault(
    pool: &PgPool,
    settings: &str,
    vault_index: i16,
) -> Result<Option<SelectedVault>, loyal_yield_orchestrator::sqlx::Error> {
    let row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            v.id,
            v.settings,
            p.authority,
            p.policy_seed,
            v.vault_index,
            v.vault_pubkey,
            p.policy_account,
            sp.policy_account AS setup_policy_account,
            sp.policy_seed AS setup_policy_seed,
            p.delegated_signers,
            p.threshold,
            p.route_modes,
            p.stable_mints,
            p.kamino_markets,
            p.kamino_liquidity_mints,
            p.swap_lanes
        FROM loyal_yield.managed_vaults v
        JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
        LEFT JOIN loyal_yield.route_policies sp ON sp.id = v.setup_policy_id
          AND sp.active = true
        WHERE v.settings = $1
          AND v.vault_index = $2
          AND v.active = true
          AND p.active = true
        "#,
    )
    .bind(settings)
    .bind(vault_index)
    .fetch_optional(pool)
    .await?;

    row.map(|row| {
        Ok(SelectedVault {
            id: VaultId(row.try_get::<i64, _>("id")?),
            settings: row.try_get("settings")?,
            authority: row.try_get("authority")?,
            policy_seed: row.try_get("policy_seed")?,
            vault_index: row.try_get("vault_index")?,
            vault_pubkey: row.try_get("vault_pubkey")?,
            policy_account: row.try_get("policy_account")?,
            setup_policy_account: row.try_get("setup_policy_account")?,
            setup_policy_seed: row.try_get("setup_policy_seed")?,
            delegated_signers: row.try_get("delegated_signers")?,
            threshold: row.try_get("threshold")?,
            route_modes: row.try_get("route_modes")?,
            stable_mints: row.try_get("stable_mints")?,
            kamino_markets: row.try_get("kamino_markets")?,
            kamino_liquidity_mints: row.try_get("kamino_liquidity_mints")?,
            swap_lanes: row.try_get("swap_lanes")?,
        })
    })
    .transpose()
}

async fn load_policy_target_vault(
    pool: &PgPool,
    settings: &str,
    vault_index: i16,
    default_authority: Pubkey,
    default_delegated_signer: Pubkey,
) -> Result<Option<SelectedVault>, Box<dyn Error>> {
    let row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            v.id,
            v.settings,
            v.vault_index,
            v.vault_pubkey,
            seed_cursor.max_policy_seed,
            route_template.authority,
            route_template.delegated_signers,
            route_template.threshold,
            route_template.route_modes,
            route_template.stable_mints,
            route_template.kamino_markets,
            route_template.kamino_liquidity_mints,
            route_template.swap_lanes
        FROM loyal_yield.managed_vaults v
        LEFT JOIN LATERAL (
            SELECT max(policy_seed) AS max_policy_seed
            FROM loyal_yield.route_policies
            WHERE settings = v.settings
              AND vault_index = v.vault_index
        ) seed_cursor ON TRUE
        LEFT JOIN LATERAL (
            SELECT
                authority,
                delegated_signers,
                threshold,
                route_modes,
                stable_mints,
                kamino_markets,
                kamino_liquidity_mints,
                swap_lanes
            FROM loyal_yield.route_policies
            WHERE settings = v.settings
              AND vault_index = v.vault_index
              AND $3 = ANY(route_modes)
            ORDER BY active DESC, last_seen_slot DESC, policy_seed DESC, id DESC
            LIMIT 1
        ) route_template ON TRUE
        WHERE v.settings = $1
          AND v.vault_index = $2
        "#,
    )
    .bind(settings)
    .bind(vault_index)
    .bind(SAME_MINT_ROUTE_MODE)
    .fetch_optional(pool)
    .await?;

    let Some(row) = row else {
        return Ok(None);
    };

    let settings: String = row.try_get("settings")?;
    let settings_pubkey = Pubkey::from_str(&settings)?;
    let max_policy_seed: Option<i64> = row.try_get("max_policy_seed")?;
    let policy_seed = max_policy_seed
        .map(|seed| seed.saturating_add(1))
        .unwrap_or(i64::try_from(YIELD_ROUTE_WITHDRAW_ACTION_SEED)?);
    let policy_account = derive_action_account(&settings_pubkey, u64::try_from(policy_seed)?).0;
    let authority = row
        .try_get::<Option<String>, _>("authority")?
        .unwrap_or_else(|| default_authority.to_string());
    let delegated_signers = row
        .try_get::<Option<Vec<String>>, _>("delegated_signers")?
        .unwrap_or_else(|| vec![default_delegated_signer.to_string()]);
    let threshold = row.try_get::<Option<i32>, _>("threshold")?.unwrap_or(1);
    let route_modes = row
        .try_get::<Option<Vec<String>>, _>("route_modes")?
        .unwrap_or_else(|| vec![SAME_MINT_ROUTE_MODE.to_owned()]);
    let stable_mints = row
        .try_get::<Option<Vec<String>>, _>("stable_mints")?
        .unwrap_or_else(|| vec![USDC_MINT.to_string()]);
    let kamino_markets = row
        .try_get::<Option<Vec<String>>, _>("kamino_markets")?
        .unwrap_or_else(|| {
            vec![
                KAMINO_MAIN_MARKET.to_owned(),
                KAMINO_PRIME_MARKET.to_owned(),
            ]
        });
    let kamino_liquidity_mints = row
        .try_get::<Option<Vec<String>>, _>("kamino_liquidity_mints")?
        .unwrap_or_else(|| vec![USDC_MINT.to_string()]);
    let swap_lanes = row
        .try_get::<Option<Value>, _>("swap_lanes")?
        .unwrap_or_else(|| Value::Array(vec![]));

    Ok(Some(SelectedVault {
        id: VaultId(row.try_get::<i64, _>("id")?),
        settings,
        authority,
        policy_seed,
        vault_index: row.try_get("vault_index")?,
        vault_pubkey: row.try_get("vault_pubkey")?,
        policy_account: policy_account.to_string(),
        setup_policy_account: None,
        setup_policy_seed: None,
        delegated_signers,
        threshold,
        route_modes,
        stable_mints,
        kamino_markets,
        kamino_liquidity_mints,
        swap_lanes,
    }))
}

async fn load_active_decision(
    pool: &PgPool,
    vault_id: VaultId,
) -> Result<Option<(i64, String)>, loyal_yield_orchestrator::sqlx::Error> {
    let row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, status::text AS status
        FROM loyal_yield.rebalance_decisions
        WHERE vault_id = $1
          AND status = ANY($2::loyal_yield.decision_status[])
        ORDER BY id DESC
        LIMIT 1
        "#,
    )
    .bind(vault_id.as_i64())
    .bind(&["planned", "simulating", "ready", "submitted", "confirming"])
    .fetch_optional(pool)
    .await?;

    row.map(|row| Ok((row.try_get("id")?, row.try_get("status")?)))
        .transpose()
}

fn validate_alt_cluster(cluster: &str) -> Result<(), String> {
    if matches!(cluster, "mainnet-beta" | "devnet" | "testnet" | "localnet") {
        Ok(())
    } else {
        Err(format!(
            "YIELD_ALT_CLUSTER/--cluster must be mainnet-beta, devnet, testnet, or localnet; got {cluster:?}"
        ))
    }
}

fn parse_args(args: impl IntoIterator<Item = String>) -> Result<CliOptions, String> {
    let mut settings = None;
    let mut vault_index = None;
    let mut direction = Direction::MainToPrime;
    let mut source_reserve = None;
    let mut target_reserve = None;
    let mut update_policy = false;
    let mut update_active_policy = false;
    let mut initial_deposit_reserve = None;
    let mut initial_deposit_amount_raw = None;
    let mut idle_vault_deposit_reserve = None;
    let mut idle_vault_deposit_amount_raw = None;
    let mut full_withdraw_main_usdc = false;
    let mut full_withdraw_reserve = None;
    let mut setup_obligation_reserve = None;
    let mut e2e_deposit_amount_raw = None;
    let mut execute = false;
    let mut prepare_only = false;
    let mut read_only = false;
    let mut optimization_cycle = false;
    let mut reconcile_from_chain = false;
    let mut reconcile_current_positions = false;
    let mut reconcile_reserves = Vec::new();
    let mut seed_from_user_position = false;
    let mut expected_source_snapshot_id = None;
    let mut expected_liquidity_mint = None;
    let mut expected_amount_raw = None;
    let mut expected_route_amount_semantics = None;
    let mut expected_idle_token_account = None;
    let mut expected_idle_observed_slot = None;
    let mut expected_idle_observed_at = None;
    let mut expected_source_apy_bps = None;
    let expected_observed_target_apy_bps = None;
    let mut expected_target_apy_bps = None;
    let mut expected_edge_bps = None;
    let principal_usd_micros = None;
    let confidence_ppm = None;
    let expected_service_millis = None;
    let holding_horizon_seconds = None;
    let estimated_execution_cost_usd_micros = None;
    let mut cluster = env::var("YIELD_ALT_CLUSTER").ok();
    let mut rpc_url = env::var("SOLANA_RPC_URL").unwrap_or_else(|_| DEFAULT_SOLANA_RPC_URL.into());
    let mut iter = args.into_iter();

    while let Some(arg) = iter.next() {
        match arg.as_str() {
            "--settings" => {
                settings = Some(
                    iter.next()
                        .ok_or("--settings requires a settings public key")?,
                );
            }
            "--vault-index" => {
                let raw = iter.next().ok_or("--vault-index requires a value")?;
                vault_index = Some(
                    raw.parse::<i16>()
                        .map_err(|_| "--vault-index must be an i16")?,
                );
            }
            "--direction" => {
                let raw = iter.next().ok_or("--direction requires a value")?;
                direction = Direction::parse(&raw)
                    .ok_or("--direction must be main-to-prime or prime-to-main")?;
            }
            "--source-reserve" => {
                source_reserve = Some(
                    iter.next()
                        .ok_or("--source-reserve requires a public key")?,
                );
            }
            "--target-reserve" => {
                target_reserve = Some(
                    iter.next()
                        .ok_or("--target-reserve requires a public key")?,
                );
            }
            "--update-policy" => update_policy = true,
            "--update-active-policy" => update_active_policy = true,
            "--full-withdraw-main-usdc" => full_withdraw_main_usdc = true,
            "--full-withdraw-reserve" => {
                let raw = iter
                    .next()
                    .ok_or("--full-withdraw-reserve requires a reserve public key")?;
                Pubkey::from_str(&raw)
                    .map_err(|_| "--full-withdraw-reserve must be a public key")?;
                full_withdraw_reserve = Some(raw);
            }
            "--setup-obligation-reserve" => {
                let raw = iter
                    .next()
                    .ok_or("--setup-obligation-reserve requires a reserve public key")?;
                Pubkey::from_str(&raw)
                    .map_err(|_| "--setup-obligation-reserve must be a public key")?;
                setup_obligation_reserve = Some(raw);
            }
            "--e2e-main-prime-main" => {
                let raw = iter
                    .next()
                    .ok_or("--e2e-main-prime-main requires an amount in raw USDC units")?;
                let amount = raw
                    .parse::<u64>()
                    .map_err(|_| "--e2e-main-prime-main amount must be a u64")?;
                if amount == 0 {
                    return Err("--e2e-main-prime-main amount must be greater than 0".to_owned());
                }
                e2e_deposit_amount_raw = Some(amount);
            }
            "--deposit-main-usdc" => {
                if initial_deposit_amount_raw.is_some() {
                    return Err("choose only one initial deposit mode".to_owned());
                }
                let raw = iter
                    .next()
                    .ok_or("--deposit-main-usdc requires an amount in raw USDC units")?;
                let amount = raw
                    .parse::<u64>()
                    .map_err(|_| "--deposit-main-usdc amount must be a u64")?;
                if amount == 0 {
                    return Err("--deposit-main-usdc amount must be greater than 0".to_owned());
                }
                initial_deposit_reserve = Some(KAMINO_MAIN_USDC_RESERVE.to_string());
                initial_deposit_amount_raw = Some(amount);
            }
            "--deposit-reserve" => {
                if initial_deposit_amount_raw.is_some() {
                    return Err("choose only one initial deposit mode".to_owned());
                }
                let reserve = iter
                    .next()
                    .ok_or("--deposit-reserve requires a reserve public key")?;
                Pubkey::from_str(&reserve)
                    .map_err(|_| "--deposit-reserve reserve must be a public key")?;
                let raw = iter
                    .next()
                    .ok_or("--deposit-reserve requires an amount in raw USDC units")?;
                let amount = raw
                    .parse::<u64>()
                    .map_err(|_| "--deposit-reserve amount must be a u64")?;
                if amount == 0 {
                    return Err("--deposit-reserve amount must be greater than 0".to_owned());
                }
                initial_deposit_reserve = Some(reserve);
                initial_deposit_amount_raw = Some(amount);
            }
            "--deposit-idle-vault-reserve" => {
                if idle_vault_deposit_amount_raw.is_some() {
                    return Err("choose only one idle vault deposit mode".to_owned());
                }
                let reserve = iter
                    .next()
                    .ok_or("--deposit-idle-vault-reserve requires a reserve public key")?;
                Pubkey::from_str(&reserve)
                    .map_err(|_| "--deposit-idle-vault-reserve reserve must be a public key")?;
                let raw = iter
                    .next()
                    .ok_or("--deposit-idle-vault-reserve requires an amount in raw USDC units")?;
                let amount = raw
                    .parse::<u64>()
                    .map_err(|_| "--deposit-idle-vault-reserve amount must be a u64")?;
                if amount == 0 {
                    return Err(
                        "--deposit-idle-vault-reserve amount must be greater than 0".to_owned()
                    );
                }
                idle_vault_deposit_reserve = Some(reserve);
                idle_vault_deposit_amount_raw = Some(amount);
            }
            "--execute" => execute = true,
            "--prepare-only" => prepare_only = true,
            "--read-only" => read_only = true,
            "--optimization-cycle" => optimization_cycle = true,
            "--reconcile-from-chain" => reconcile_from_chain = true,
            "--reconcile-current-positions" => reconcile_current_positions = true,
            "--reconcile-reserve" => {
                let raw = iter
                    .next()
                    .ok_or("--reconcile-reserve requires a reserve public key")?;
                Pubkey::from_str(&raw).map_err(|_| "--reconcile-reserve must be a public key")?;
                if !reconcile_reserves.iter().any(|reserve| reserve == &raw) {
                    reconcile_reserves.push(raw);
                }
            }
            "--seed-from-user-position" => seed_from_user_position = true,
            "--expected-source-snapshot-id" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-source-snapshot-id requires a value")?;
                expected_source_snapshot_id = Some(
                    raw.parse::<i64>()
                        .map_err(|_| "--expected-source-snapshot-id must be an i64")?,
                );
            }
            "--expected-liquidity-mint" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-liquidity-mint requires a mint public key")?;
                Pubkey::from_str(&raw)
                    .map_err(|_| "--expected-liquidity-mint must be a public key")?;
                expected_liquidity_mint = Some(raw);
            }
            "--expected-amount-raw" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-amount-raw requires a value")?;
                let amount = raw
                    .parse::<i64>()
                    .map_err(|_| "--expected-amount-raw must be an i64")?;
                if amount <= 0 {
                    return Err("--expected-amount-raw must be greater than 0".to_owned());
                }
                expected_amount_raw = Some(amount);
            }
            "--expected-route-amount-semantics" => {
                expected_route_amount_semantics = Some(
                    iter.next()
                        .ok_or("--expected-route-amount-semantics requires a value")?,
                );
            }
            "--expected-idle-token-account" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-idle-token-account requires a token account public key")?;
                Pubkey::from_str(&raw)
                    .map_err(|_| "--expected-idle-token-account must be a public key")?;
                expected_idle_token_account = Some(raw);
            }
            "--expected-idle-observed-slot" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-idle-observed-slot requires a value")?;
                expected_idle_observed_slot = Some(
                    raw.parse::<i64>()
                        .map_err(|_| "--expected-idle-observed-slot must be an i64")?,
                );
            }
            "--expected-idle-observed-at" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-idle-observed-at requires an RFC3339 timestamp")?;
                let parsed = DateTime::parse_from_rfc3339(&raw)
                    .map_err(|_| "--expected-idle-observed-at must be an RFC3339 timestamp")?;
                expected_idle_observed_at = Some(parsed.with_timezone(&Utc));
            }
            "--expected-source-apy-bps" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-source-apy-bps requires a value")?;
                expected_source_apy_bps = Some(
                    raw.parse::<i64>()
                        .map_err(|_| "--expected-source-apy-bps must be an i64")?,
                );
            }
            "--expected-target-apy-bps" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-target-apy-bps requires a value")?;
                expected_target_apy_bps = Some(
                    raw.parse::<i64>()
                        .map_err(|_| "--expected-target-apy-bps must be an i64")?,
                );
            }
            "--expected-edge-bps" => {
                let raw = iter.next().ok_or("--expected-edge-bps requires a value")?;
                expected_edge_bps = Some(
                    raw.parse::<i64>()
                        .map_err(|_| "--expected-edge-bps must be an i64")?,
                );
            }
            "--rpc-url" => {
                rpc_url = iter.next().ok_or("--rpc-url requires a value")?;
            }
            "--cluster" => {
                cluster = Some(iter.next().ok_or("--cluster requires a value")?);
            }
            "--help" | "-h" => return Err("help".to_owned()),
            _ => return Err(format!("unknown argument: {arg}")),
        }
    }
    if full_withdraw_main_usdc && full_withdraw_reserve.is_some() {
        return Err(
            "--full-withdraw-main-usdc and --full-withdraw-reserve are aliases; choose one"
                .to_owned(),
        );
    }
    let full_withdraw_requested = full_withdraw_main_usdc || full_withdraw_reserve.is_some();
    if initial_deposit_amount_raw.is_some() && idle_vault_deposit_amount_raw.is_some() {
        return Err(
            "--deposit-idle-vault-reserve cannot be combined with --deposit-main-usdc/--deposit-reserve"
                .to_owned(),
        );
    }
    let selected_special_modes = [
        update_policy,
        initial_deposit_amount_raw.is_some(),
        idle_vault_deposit_amount_raw.is_some(),
        full_withdraw_requested,
        setup_obligation_reserve.is_some(),
        reconcile_current_positions,
        e2e_deposit_amount_raw.is_some(),
    ]
    .into_iter()
    .filter(|selected| *selected)
    .count();
    if selected_special_modes > 1 {
        return Err(
            "--update-policy, --deposit-main-usdc/--deposit-reserve, --deposit-idle-vault-reserve, --setup-obligation-reserve, --full-withdraw-reserve, --reconcile-current-positions, and --e2e-main-prime-main are mutually exclusive"
                .to_owned(),
        );
    }
    if update_active_policy && !update_policy {
        return Err("--update-active-policy requires --update-policy".to_owned());
    }
    if read_only && (execute || prepare_only) {
        return Err(
            "--read-only cannot be combined with --execute or --prepare-only, which must persist"
                .to_owned(),
        );
    }
    if source_reserve.is_some() != target_reserve.is_some() {
        return Err("--source-reserve and --target-reserve must be provided together".to_owned());
    }
    if reconcile_current_positions && !reconcile_from_chain {
        return Err("--reconcile-current-positions requires --reconcile-from-chain".to_owned());
    }
    if reconcile_current_positions && reconcile_reserves.is_empty() {
        return Err(
            "--reconcile-current-positions requires at least one --reconcile-reserve".to_owned(),
        );
    }
    if reconcile_current_positions && (execute || seed_from_user_position) {
        return Err("--reconcile-current-positions cannot be combined with --execute or --seed-from-user-position".to_owned());
    }
    if prepare_only && execute {
        return Err("--prepare-only cannot be combined with --execute".to_owned());
    }
    if prepare_only && !optimization_cycle {
        return Err("--prepare-only requires --optimization-cycle".to_owned());
    }
    if idle_vault_deposit_amount_raw.is_some() {
        if !reconcile_from_chain {
            return Err("--deposit-idle-vault-reserve requires --reconcile-from-chain".to_owned());
        }
        if seed_from_user_position {
            return Err(
                "--deposit-idle-vault-reserve cannot use --seed-from-user-position".to_owned(),
            );
        }
        if execute
            && (expected_idle_token_account.is_none()
                || expected_idle_observed_slot.is_none()
                || expected_idle_observed_at.is_none()
                || expected_liquidity_mint.is_none()
                || expected_amount_raw.is_none()
                || expected_target_apy_bps.is_none()
                || expected_edge_bps.is_none())
        {
            return Err(
                "--deposit-idle-vault-reserve --execute requires --expected-idle-token-account, --expected-idle-observed-slot, --expected-idle-observed-at, --expected-liquidity-mint, --expected-amount-raw, --expected-target-apy-bps, and --expected-edge-bps"
                    .to_owned(),
            );
        }
    }
    if optimization_cycle {
        if !execute && !prepare_only {
            return Err("--optimization-cycle requires --execute or --prepare-only".to_owned());
        }
        if !reconcile_from_chain {
            return Err("--optimization-cycle requires --reconcile-from-chain".to_owned());
        }
        if source_reserve.is_none() || target_reserve.is_none() {
            return Err(
                "--optimization-cycle requires explicit --source-reserve and --target-reserve"
                    .to_owned(),
            );
        }
        if selected_special_modes != 0 || update_active_policy {
            return Err(
                "--optimization-cycle cannot be combined with setup/admin modes".to_owned(),
            );
        }
        if seed_from_user_position {
            return Err("--optimization-cycle cannot use --seed-from-user-position".to_owned());
        }
        if expected_source_snapshot_id.is_none()
            || expected_liquidity_mint.is_none()
            || expected_amount_raw.is_none()
            || expected_route_amount_semantics.is_none()
            || expected_source_apy_bps.is_none()
            || expected_target_apy_bps.is_none()
            || expected_edge_bps.is_none()
        {
            return Err(
                "--optimization-cycle requires --expected-source-snapshot-id, --expected-liquidity-mint, --expected-amount-raw, --expected-route-amount-semantics, --expected-source-apy-bps, --expected-target-apy-bps, and --expected-edge-bps"
                    .to_owned(),
            );
        }
    }
    let cluster = cluster.ok_or("YIELD_ALT_CLUSTER or --cluster is required")?;
    validate_alt_cluster(&cluster)?;
    Ok(CliOptions {
        settings: settings.ok_or("--settings is required")?,
        vault_index: vault_index.ok_or("--vault-index is required")?,
        direction,
        source_reserve,
        target_reserve,
        update_policy,
        update_active_policy,
        initial_deposit_reserve,
        initial_deposit_amount_raw,
        idle_vault_deposit_reserve,
        idle_vault_deposit_amount_raw,
        full_withdraw_main_usdc,
        full_withdraw_reserve,
        setup_obligation_reserve,
        e2e_deposit_amount_raw,
        execute,
        prepare_only,
        read_only,
        fused_execute: false,
        optimization_cycle,
        reconcile_from_chain,
        reconcile_current_positions,
        reconcile_reserves,
        seed_from_user_position,
        expected_source_snapshot_id,
        expected_liquidity_mint,
        expected_amount_raw,
        expected_route_amount_semantics,
        expected_idle_token_account,
        expected_idle_observed_slot,
        expected_idle_observed_at,
        expected_source_apy_bps,
        expected_observed_target_apy_bps,
        expected_target_apy_bps,
        expected_edge_bps,
        principal_usd_micros,
        confidence_ppm,
        expected_service_millis,
        holding_horizon_seconds,
        estimated_execution_cost_usd_micros,
        expected_cost_lamports: None,
        current_economic_fee_cap_lamports: None,
        expected_route_fee_payer: None,
        optimizer_epoch_id: None,
        optimizer_market_slot: None,
        opportunity_id: None,
        opportunity_lease_owner: None,
        opportunity_fencing_token: None,
        cluster,
        rpc_url,
    })
}

fn validate_vault_policy(vault: &SelectedVault) -> Result<(), Box<dyn Error>> {
    if !vault
        .route_modes
        .iter()
        .any(|mode| mode == SAME_MINT_ROUTE_MODE)
    {
        return Err(format!(
            "selected policy {} does not allow {SAME_MINT_ROUTE_MODE}",
            vault.policy_account
        )
        .into());
    }
    Ok(())
}

fn build_same_mint_input(
    options: &CliOptions,
    reserve_move: &ReserveMove,
    vault_id: VaultId,
    positions: &[PositionSummary],
    active_decision: Option<(i64, String)>,
    current_market: Option<&CurrentRouteMarketEconomics>,
) -> Result<SameMintRebalanceInput, PlanBlocker> {
    if let Some((decision_id, status)) = active_decision {
        return Err(PlanBlocker::ActiveDecision {
            decision_id,
            status,
        });
    }
    if positions.is_empty() {
        return Err(PlanBlocker::MissingCurrentPosition);
    }

    let source_reserve = reserve_move.source_reserve.clone();
    let target_reserve = reserve_move.target_reserve.clone();
    let source = positions
        .iter()
        .find(|position| position.reserve == source_reserve)
        .ok_or_else(|| PlanBlocker::MissingSourceReserve(source_reserve.clone()))?;
    let target = positions
        .iter()
        .find(|position| position.reserve == target_reserve)
        .ok_or_else(|| PlanBlocker::MissingTargetReserve(target_reserve.clone()))?;

    let liquidity_mint = source.liquidity_mint.clone();
    if target.liquidity_mint != liquidity_mint {
        return Err(PlanBlocker::TargetMintMismatch {
            actual: target.liquidity_mint.clone(),
            expected: liquidity_mint,
        });
    }
    if source.amount_raw <= 0 || !source.has_value {
        return Err(PlanBlocker::SourceHasNoValue);
    }
    let evidence =
        route_amount_evidence_from_metadata(source.amount_raw, &source.planning_metadata)
            .ok_or_else(|| PlanBlocker::UnsupportedAmountSemantics {
                reserve: source.reserve.clone(),
                amount_semantics: source
                    .planning_metadata
                    .get("amount_semantics")
                    .and_then(Value::as_str)
                    .map(ToOwned::to_owned),
            })?;

    // A queue route revalidates against fresh market economics before this
    // point, but its durable decision must retain the economics identity that
    // the planner published. The capacity reservation and signed handoff carry
    // the fresh admission evidence separately. Mixing those two identities
    // makes the database's execute-opportunity trigger reject every ordinary
    // APY tick after revalidation.
    let queue_route = options.opportunity_id.is_some();
    let source_apy_bps = if queue_route {
        options.expected_source_apy_bps.ok_or_else(|| {
            PlanBlocker::MonitorPlanDrift(
                "queue route is missing its published source APY".to_owned(),
            )
        })?
    } else {
        current_market
            .map(|market| market.source_apy_bps)
            .or(source.supply_apy_bps)
            .unwrap_or_default()
    };
    let target_apy_bps = if queue_route {
        options.expected_target_apy_bps.ok_or_else(|| {
            PlanBlocker::MonitorPlanDrift(
                "queue route is missing its published target APY".to_owned(),
            )
        })?
    } else {
        current_market
            .map(|market| market.capacity_adjusted_target_apy_bps)
            .or(target.supply_apy_bps)
            .unwrap_or_default()
    };
    let input = SameMintRebalanceInput {
        vault_id: Some(vault_id),
        settings: None,
        vault_index: None,
        source_reserve,
        target_reserve,
        liquidity_mint,
        amount_raw: evidence.amount_raw,
        route_amount_semantics: evidence.route_amount_semantics,
        source_amount_semantics: evidence.source_amount_semantics,
        source_collateral_amount_raw: evidence.source_collateral_amount_raw,
        redeemable_source_liquidity_amount_raw: evidence.redeemable_source_liquidity_amount_raw,
        idle_vault_liquidity_amount_raw: evidence.idle_vault_liquidity_amount_raw,
        expected_source_snapshot_id: source.snapshot_id,
        source_apy_bps,
        target_apy_bps,
        estimated_edge_bps: if queue_route {
            options.expected_edge_bps.ok_or_else(|| {
                PlanBlocker::MonitorPlanDrift(
                    "queue route is missing its published edge".to_owned(),
                )
            })?
        } else {
            current_market
                .map(|market| market.edge_bps)
                .unwrap_or(target_apy_bps - source_apy_bps)
        },
        estimated_cost_lamports: options.expected_cost_lamports.unwrap_or_default(),
        dry_run: !options.execute,
    };
    validate_monitor_expectations(options, &input)?;
    Ok(input)
}

fn validate_monitor_expectations(
    options: &CliOptions,
    input: &SameMintRebalanceInput,
) -> Result<(), PlanBlocker> {
    if let Some(expected) = options.expected_source_snapshot_id {
        let actual = input.expected_source_snapshot_id.as_i64();
        let accepted_fresh_chain_snapshot =
            options.execute && options.reconcile_from_chain && actual > expected;
        if actual != expected && !accepted_fresh_chain_snapshot {
            return Err(PlanBlocker::MonitorPlanDrift(format!(
                "expected source snapshot {expected}, got {}",
                input.expected_source_snapshot_id.as_i64()
            )));
        }
    }
    if let Some(expected) = &options.expected_liquidity_mint {
        if input.liquidity_mint != *expected {
            return Err(PlanBlocker::MonitorPlanDrift(format!(
                "expected liquidity_mint {expected}, got {}",
                input.liquidity_mint
            )));
        }
    }
    if let Some(expected) = options.expected_amount_raw {
        let positive_queue_accrual = options.opportunity_id.is_some()
            && options.reconcile_from_chain
            && options.idle_vault_deposit_amount_raw.is_none()
            && input.amount_raw > expected
            && i128::from(input.amount_raw - expected) * 1_000_000
                <= i128::from(expected) * i128::from(MAX_QUEUE_POSITIVE_AMOUNT_DRIFT_PPM);
        if input.amount_raw != expected && !positive_queue_accrual {
            return Err(PlanBlocker::MonitorPlanDrift(format!(
                "expected route amount_raw {expected}, got {}",
                input.amount_raw
            )));
        }
    }
    if let Some(expected) = &options.expected_route_amount_semantics {
        if input.route_amount_semantics != *expected {
            return Err(PlanBlocker::MonitorPlanDrift(format!(
                "expected route_amount_semantics {expected}, got {}",
                input.route_amount_semantics
            )));
        }
    }
    // Queue routes have already recomputed the complete economic gate from one
    // fresh immutable market epoch. Their durable APYs describe the planned
    // route/capacity policy, not an equality constraint on ordinary market
    // ticks. Legacy monitor CLI calls still retain their exact drift contract.
    if options.current_economic_fee_cap_lamports.is_none() {
        if let Some(expected) = options.expected_source_apy_bps {
            if input.source_apy_bps != expected {
                return Err(PlanBlocker::MonitorPlanDrift(format!(
                    "expected source_apy_bps {expected}, got {}",
                    input.source_apy_bps
                )));
            }
        }
        if let Some(expected) = options.expected_target_apy_bps {
            if input.target_apy_bps != expected {
                return Err(PlanBlocker::MonitorPlanDrift(format!(
                    "expected target_apy_bps {expected}, got {}",
                    input.target_apy_bps
                )));
            }
        }
        if let Some(expected) = options.expected_edge_bps {
            if input.estimated_edge_bps != expected {
                return Err(PlanBlocker::MonitorPlanDrift(format!(
                    "expected estimated_edge_bps {expected}, got {}",
                    input.estimated_edge_bps
                )));
            }
        }
    }
    Ok(())
}

fn blocker_report(
    options: &CliOptions,
    reserve_move: &ReserveMove,
    vault: &SelectedVault,
    positions: &[PositionSummary],
    chain_preview: Option<&ChainReconcilePreview>,
    policy_preflight: Option<&PolicyAccountPreflight>,
    user_position_seed: Option<&UserPositionSeedPreview>,
    reconciled_snapshot_id: Option<SnapshotId>,
    blocker: PlanBlocker,
) -> Value {
    json!({
        "status": "blocked_before_decision_write",
        "reason": blocker_reason(&blocker),
        "executeRequested": options.execute,
        "writesDecision": false,
        "wouldReconcileCurrentPositions": options.reconcile_from_chain,
        "reconciledSnapshotId": reconciled_snapshot_id.map(SnapshotId::as_i64),
        "direction": options.direction.as_str(),
        "vault": vault_json(vault),
        "requiredReserves": required_reserves_json(reserve_move),
        "currentPositions": positions.iter().map(position_json).collect::<Vec<_>>(),
        "chainReconcile": chain_preview.map(chain_reconcile_preview_json),
        "userPositionSeed": user_position_seed.map(user_position_seed_preview_json),
        "policyPreflight": policy_route_preflight_json(vault, reserve_move, policy_preflight),
    })
}

fn blocker_reason(blocker: &PlanBlocker) -> Value {
    match blocker {
        PlanBlocker::MissingCurrentPosition => json!("missing_current_positions"),
        PlanBlocker::MissingSourceReserve(reserve) => json!({
            "kind": "missing_source_reserve",
            "reserve": reserve,
        }),
        PlanBlocker::MissingTargetReserve(reserve) => json!({
            "kind": "missing_target_reserve",
            "reserve": reserve,
        }),
        PlanBlocker::SourceHasNoValue => json!("source_reserve_has_no_value"),
        PlanBlocker::TargetMintMismatch { actual, expected } => json!({
            "kind": "target_liquidity_mint_mismatch",
            "actual": actual,
            "expected": expected,
        }),
        PlanBlocker::UnsupportedAmountSemantics {
            reserve,
            amount_semantics,
        } => json!({
            "kind": "unsupported_amount_semantics",
            "reserve": reserve,
            "amountSemantics": amount_semantics,
            "expectedRouteAmountSemantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
        }),
        PlanBlocker::MonitorPlanDrift(reason) => json!({
            "kind": "monitor_plan_drift",
            "reason": reason,
        }),
        PlanBlocker::ActiveDecision {
            decision_id,
            status,
        } => json!({
            "kind": "active_decision_exists",
            "decisionId": decision_id,
            "status": status,
        }),
    }
}

fn vault_json(vault: &SelectedVault) -> Value {
    json!({
        "id": vault.id.as_i64(),
        "settings": vault.settings,
        "vaultIndex": vault.vault_index,
        "vaultPubkey": vault.vault_pubkey,
        "policyAccount": vault.policy_account,
        "setupPolicyAccount": vault.setup_policy_account,
        "setupPolicySeed": vault.setup_policy_seed,
        "delegatedSigners": vault.delegated_signers,
        "routeModes": vault.route_modes,
        "kaminoMarkets": vault.kamino_markets,
        "kaminoLiquidityMints": vault.kamino_liquidity_mints,
    })
}

fn required_reserves_json(reserve_move: &ReserveMove) -> Value {
    json!({
        "sourceReserve": reserve_move.source_reserve,
        "targetReserve": reserve_move.target_reserve,
    })
}

fn position_json(position: &PositionSummary) -> Value {
    json!({
        "reserve": position.reserve,
        "liquidityMint": position.liquidity_mint,
        "amountRaw": position.amount_raw.to_string(),
        "hasValue": position.has_value,
        "snapshotId": position.snapshot_id.as_i64(),
        "supplyApyBps": position.supply_apy_bps,
        "planningMetadata": position.planning_metadata,
    })
}

fn same_mint_result_json(result: &SameMintRebalanceResult) -> Value {
    json!({
        "vaultId": result.vault_id.as_i64(),
        "decisionId": result.decision_id.map(|id| id.as_i64()),
        "status": result.status.as_str(),
        "sourceReserve": result.source_reserve,
        "targetReserve": result.target_reserve,
        "liquidityMint": result.liquidity_mint,
        "amountRaw": result.amount_raw.to_string(),
        "signature": result.signature,
        "confirmedSlot": result.confirmed_slot,
        "skipReason": result.skip_reason.map(|reason| reason.decision_reason().as_str()),
        "errorReason": result.error_reason,
        "dryRun": result.dry_run,
        "executionPreview": result.execution_preview.as_ref().map(|preview| json!({
            "kind": preview.kind,
            "sourceReserve": preview.source_reserve,
            "targetReserve": preview.target_reserve,
            "liquidityMint": preview.liquidity_mint,
            "amountRaw": preview.amount_raw.to_string(),
            "routeAmountSemantics": preview.route_amount_semantics,
            "sourceAmountSemantics": preview.source_amount_semantics,
            "sourceCollateralAmountRaw": preview.source_collateral_amount_raw.map(|amount| amount.to_string()),
            "redeemableSourceLiquidityAmountRaw": preview.redeemable_source_liquidity_amount_raw.map(|amount| amount.to_string()),
            "idleVaultLiquidityAmountRaw": preview.idle_vault_liquidity_amount_raw.map(|amount| amount.to_string()),
            "policyExecutions": preview.policy_executions,
            "routeSteps": preview.route_steps,
        })),
    })
}

fn prepared_same_mint_decision_json(decision: &PreparedSameMintDecision) -> Value {
    json!({
        "source": "loyal_yield.rebalance_decisions",
        "id": decision.id.as_i64(),
        "vaultId": decision.vault_id.as_i64(),
        "sourceSnapshotId": decision.source_snapshot_id.as_i64(),
        "sourceReserve": decision.source_reserve,
        "targetReserve": decision.target_reserve,
        "liquidityMint": decision.liquidity_mint,
        "sourceLiquidityMint": decision.source_liquidity_mint,
        "targetLiquidityMint": decision.target_liquidity_mint,
        "amountRaw": decision.amount_raw.to_string(),
        "routeAmountSemantics": decision.execution_plan.get("route_amount_semantics").and_then(Value::as_str),
        "sourceAmountSemantics": decision.execution_plan.get("source_amount_semantics").and_then(Value::as_str),
        "sourceCollateralAmountRaw": plan_i64(&decision.execution_plan, "source_collateral_amount_raw").map(|amount| amount.to_string()),
        "redeemableSourceLiquidityAmountRaw": plan_i64(&decision.execution_plan, "redeemable_source_liquidity_amount_raw").map(|amount| amount.to_string()),
        "idleVaultLiquidityAmountRaw": plan_i64(&decision.execution_plan, "idle_vault_liquidity_amount_raw").map(|amount| amount.to_string()),
        "sourceApyBps": decision.source_apy_bps,
        "targetApyBps": decision.target_apy_bps,
        "estimatedEdgeBps": decision.estimated_edge_bps,
        "estimatedCostLamports": decision.estimated_cost_lamports,
        "executionPlan": decision.execution_plan,
        "idempotencyKey": decision.idempotency_key,
    })
}

fn chain_reconcile_preview_json(preview: &ChainReconcilePreview) -> Value {
    json!({
        "observedSlot": preview.observed_slot,
        "vaultUserMetadata": preview.vault_user_metadata,
        "vaultUserMetadataExists": preview.vault_user_metadata_exists,
        "rpcAccountReads": preview.rpc_account_reads,
        "positions": preview.positions.iter().map(chain_position_json).collect::<Vec<_>>(),
    })
}

fn target_obligation_setup_json(
    preview: &ChainReconcilePreview,
    reserve_move: &ReserveMove,
    vault: &SelectedVault,
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Option<Value> {
    let target = chain_position_for_reserve(preview, &reserve_move.target_reserve).ok()?;
    let needed = !target.obligation_exists;
    let init_constraint_index = policy_preflight.and_then(|preflight| {
        init_obligation_instruction_constraint_index(Some(preflight), target).ok()
    });
    let decoded_route_policy_allows_init = init_constraint_index.is_some();
    let decoded_route_policy_allows_refresh = policy_preflight
        .map(PolicyAccountPreflight::allows_refresh_obligation)
        .unwrap_or(false);
    let setup_policy_available = vault.setup_policy_account.is_some();
    let (policy_shape, init_policy_source, init_policy_account) =
        if decoded_route_policy_allows_init {
            (
                "route_policy_with_market_scoped_init_obligation",
                Some("route_policy"),
                Some(vault.policy_account.as_str()),
            )
        } else if setup_policy_available {
            (
                "route_policy_plus_setup_policy_market_scoped_init_obligation",
                Some("setup_policy"),
                vault.setup_policy_account.as_deref(),
            )
        } else {
            (
                "route_policy_without_authorized_init_obligation",
                None,
                None,
            )
        };
    let required_before_same_mint_execution = if !needed {
        Vec::<&str>::new()
    } else if decoded_route_policy_allows_init {
        vec![
            "execute route-policy withdraw in the same transaction",
            "execute the target-market init_obligation constraint from the route policy in the same transaction",
            "refresh the newly initialized target obligation before the protected deposit instruction",
        ]
    } else if setup_policy_available {
        vec![
            "execute route-policy withdraw in the same transaction",
            "execute the target-market init_obligation constraint from the setup policy in the same transaction",
            "refresh the newly initialized target obligation before the protected deposit instruction",
        ]
    } else {
        vec!["block execution because no authorized init_obligation policy path is recorded"]
    };

    Some(json!({
        "needed": needed,
        "targetObligation": target.obligation,
        "targetReserve": target.reserve,
        "targetMarket": target.market,
        "vaultUserMetadata": preview.vault_user_metadata,
        "vaultUserMetadataExists": preview.vault_user_metadata_exists,
        "policyShape": policy_shape,
        "initPolicySource": init_policy_source,
        "initPolicyAccount": init_policy_account,
        "setupPolicyAccount": vault.setup_policy_account,
        "setupPolicySeed": vault.setup_policy_seed,
        "decodedRoutePolicyAllowsInitObligation": decoded_route_policy_allows_init,
        "initObligationInstructionConstraintIndex": init_constraint_index,
        "decodedRoutePolicyAllowsRefreshObligation": decoded_route_policy_allows_refresh,
        "requiredBeforeSameMintExecution": required_before_same_mint_execution,
    }))
}

fn missing_obligation_setup_dry_run_json(
    target: &ChainPositionSummary,
    dry_run: &MissingObligationSetupDryRun,
) -> Value {
    json!({
        "targetObligation": target.obligation,
        "targetReserve": target.reserve,
        "targetMarket": target.market,
        "policyAccount": dry_run.policy_account,
        "policySource": dry_run.policy_source,
        "instructionConstraintIndex": dry_run.instruction_constraint_index,
        "vaultRentTopUp": dry_run.vault_rent_top_up.as_ref().map(missing_obligation_setup_funding_json),
        "initExecution": policy_transaction_json(&dry_run.init_execution),
    })
}

fn missing_obligation_setup_submit_result_json(
    target: &ChainPositionSummary,
    result: &MissingObligationSetupSubmitResult,
) -> Value {
    json!({
        "targetObligation": target.obligation,
        "targetReserve": target.reserve,
        "targetMarket": target.market,
        "policyAccount": result.policy_account,
        "policySource": result.policy_source,
        "instructionConstraintIndex": result.instruction_constraint_index,
        "vaultRentTopUp": result.vault_rent_top_up.as_ref().map(missing_obligation_setup_funding_json),
        "initExecution": {
            "signature": result.init_signature,
            "submittedSlot": result.init_submitted_slot,
            "confirmedSlot": result.init_confirmed_slot,
            "simulationUnitsConsumed": result.init_simulation_units_consumed,
            "transaction": transaction_packet_json(&result.init_transaction_packet),
        },
    })
}

fn missing_obligation_setup_funding_json(funding: &MissingObligationSetupFunding) -> Value {
    json!({
        "payer": funding.payer,
        "vault": funding.vault,
        "lamports": funding.lamports,
        "vaultLamportsBefore": funding.vault_lamports_before,
        "payerLamportsBefore": funding.payer_lamports_before,
        "requiredVaultLamports": funding.required_vault_lamports,
    })
}

fn inline_missing_obligation_setup_json(setup: &InlineMissingObligationSetupPreview) -> Value {
    let route_order = if setup.vault_rent_top_up.is_some() {
        vec![
            KAMINO_WITHDRAW_ROUTE_STEP,
            SYSTEM_TRANSFER_VAULT_RENT_TOP_UP_ROUTE_STEP,
            KAMINO_INIT_OBLIGATION_ROUTE_STEP,
            KAMINO_DEPOSIT_ROUTE_STEP,
        ]
    } else {
        vec![
            KAMINO_WITHDRAW_ROUTE_STEP,
            KAMINO_INIT_OBLIGATION_ROUTE_STEP,
            KAMINO_DEPOSIT_ROUTE_STEP,
        ]
    };
    json!({
        "executionMode": "inline_route_transaction",
        "targetObligation": setup.target_obligation,
        "targetReserve": setup.target_reserve,
        "targetMarket": setup.target_market,
        "policyAccount": setup.policy_account,
        "policySource": setup.policy_source,
        "instructionConstraintIndex": setup.instruction_constraint_index,
        "vaultRentTopUp": setup.vault_rent_top_up.as_ref().map(missing_obligation_setup_funding_json),
        "routeOrder": route_order,
    })
}

fn lookup_table_manifest_hash(hash_input: &[u8]) -> String {
    format!("{:x}", Sha256::digest(hash_input))
}

fn lookup_table_manifest_json(manifest: &LookupTableManifest) -> Value {
    json!({
        "version": 1,
        "canonicalHash": lookup_table_manifest_hash(&manifest.canonical_hash_input()),
        "sharedMarketHash": lookup_table_manifest_hash(&manifest.shared_market_hash_input()),
        "vaultHash": lookup_table_manifest_hash(&manifest.vault_hash_input()),
        "compilerUniverseAddressCount": manifest.must_remain_static().len()
            + manifest.shared_market().len()
            + manifest.vault().len(),
        "lookupEligibleAddressCount": manifest.shared_market().len() + manifest.vault().len(),
        "mustRemainStatic": manifest.must_remain_static().iter().map(|requirement| json!({
            "address": requirement.address.to_string(),
            "access": requirement.access.as_str(),
            "reasons": requirement.reasons.iter().map(|reason| reason.as_str()).collect::<Vec<_>>(),
        })).collect::<Vec<_>>(),
        "sharedMarket": manifest.shared_market().iter().map(|requirement| json!({
            "address": requirement.address.to_string(),
            "access": requirement.access.as_str(),
            "roles": requirement.roles.iter().map(|role| role.as_str()).collect::<Vec<_>>(),
        })).collect::<Vec<_>>(),
        "vault": manifest.vault().iter().map(|requirement| json!({
            "address": requirement.address.to_string(),
            "access": requirement.access.as_str(),
            "roles": requirement.roles.iter().map(|role| role.as_str()).collect::<Vec<_>>(),
        })).collect::<Vec<_>>(),
    })
}

fn route_execution_preview_json(plan: &RouteExecutionPlan) -> Value {
    let preview = &plan.preview;
    json!({
        "kind": "squads_program_interaction_same_mint",
        "policyAccount": preview.policy_account,
        "setupPolicyAccount": preview.setup_policy_account,
        "feePayer": preview.fee_payer,
        "feePayerKind": preview.fee_payer_kind.as_str(),
        "feePayerSelection": route_fee_payer_selection_json(&preview.fee_payer_selection),
        "feePayerAuthorityProof": {
            "delegatedPolicySigner": preview.signer,
            "reusableAltAuthorityAndPayer": preview.signer,
            "setupFarmAndRentPayer": if preview.fee_payer_kind == RouteFeePayerKind::FeeOnlyShard {
                preview.signer.clone()
            } else {
                preview.fee_payer.clone()
            },
            "routeFeePayer": preview.fee_payer,
            "routeFeeOnly": preview.fee_payer_kind == RouteFeePayerKind::FeeOnlyShard,
        },
        "signer": preview.signer,
        "accountIndex": preview.account_index,
        "instructionConstraintIndexes": preview.instruction_constraint_indexes,
        "initInstructionConstraintIndex": preview.init_instruction_constraint_index,
        "policyConstraintValidation": preview.policy_constraint_validation.as_ref().map(policy_constraint_validation_json),
        "missingObligationSetup": preview.missing_obligation_setup.as_ref().map(inline_missing_obligation_setup_json),
        "sourceFarmSetupRequired": preview.source_farm_setup_required,
        "targetFarmSetupRequired": preview.target_farm_setup_required,
        "innerInstructionCount": preview.inner_instruction_count,
        "transactionAccountCount": preview.transaction_account_count,
        "outerAccountCount": preview.outer_account_count,
        "setupInstructionProgram": preview.setup_instruction_program,
        "setupInstructionDiscriminator": preview.setup_instruction_discriminator,
        "sourceInstructionProgram": preview.source_instruction_program,
        "targetInstructionProgram": preview.target_instruction_program,
        "sourceInstructionDiscriminator": preview.source_instruction_discriminator,
        "targetInstructionDiscriminator": preview.target_instruction_discriminator,
        "routeSteps": &preview.route_steps,
        "refreshReserves": &preview.refresh_reserves,
        "lookupTableManifest": lookup_table_manifest_json(&plan.lookup_table_manifest),
    })
}

fn route_fee_payer_selection_json(selection: &RouteFeePayerSelection) -> Value {
    json!({
        "feePayer": selection.pubkey.to_string(),
        "kind": selection.kind.as_str(),
        "reason": selection.reason,
        "matureRoute": selection.mature_route,
        "observedBalanceLamports": selection.observed_balance_lamports,
        "observedBalanceSlot": selection.observed_balance_slot,
        "observedBalanceAt": selection.observed_balance_at,
        "durableBudget": selection.shard.as_ref().map(|shard| json!({
            "minimumBalanceLamports": shard.minimum_balance_lamports,
            "maximumBalanceLamports": shard.maximum_balance_lamports,
            "rollingWindowSeconds": shard.rolling_window_seconds,
            "maximumWindowSpendLamports": shard.maximum_window_spend_lamports,
            "maximumTransactionFeeLamports": shard.maximum_transaction_fee_lamports,
            "currentWindowReservedLamports": shard.current_window_reserved_lamports,
        })),
    })
}

fn initial_deposit_policy_preview_json(preview: &InitialDepositPolicyPreview) -> Value {
    json!({
        "kind": "squads_program_interaction_initial_main_usdc_deposit",
        "policyAccount": preview.policy_account,
        "signer": preview.signer,
        "accountIndex": preview.account_index,
        "instructionConstraintIndexes": preview.instruction_constraint_indexes,
        "policyConstraintValidation": preview.policy_constraint_validation.as_ref().map(policy_constraint_validation_json),
        "innerInstructionCount": preview.inner_instruction_count,
        "transactionAccountCount": preview.transaction_account_count,
        "outerAccountCount": preview.outer_account_count,
        "setupInstructionProgram": preview.setup_instruction_program,
        "setupInstructionDiscriminator": preview.setup_instruction_discriminator,
        "depositInstructionProgram": preview.deposit_instruction_program,
        "depositInstructionDiscriminator": preview.deposit_instruction_discriminator,
        "routeSteps": &preview.route_steps,
    })
}

fn full_withdraw_policy_preview_json(preview: &FullWithdrawPolicyPreview) -> Value {
    json!({
        "kind": "squads_program_interaction_full_reserve_withdraw",
        "policyAccount": preview.policy_account,
        "signer": preview.signer,
        "accountIndex": preview.account_index,
        "instructionConstraintIndexes": preview.instruction_constraint_indexes,
        "policyConstraintValidation": preview.policy_constraint_validation.as_ref().map(policy_constraint_validation_json),
        "innerInstructionCount": preview.inner_instruction_count,
        "transactionAccountCount": preview.transaction_account_count,
        "outerAccountCount": preview.outer_account_count,
        "withdrawInstructionProgram": preview.withdraw_instruction_program,
        "withdrawInstructionDiscriminator": preview.withdraw_instruction_discriminator,
        "routeSteps": &preview.route_steps,
    })
}

fn policy_constraint_validation_json(validation: &PolicyConstraintValidation) -> Value {
    json!({
        "matches": validation.matches,
        "failures": validation.failures,
    })
}

fn account_proof_json(proof: &AccountProof) -> Value {
    json!({
        "pubkey": proof.pubkey,
        "exists": proof.exists,
        "lamports": proof.lamports.to_string(),
        "owner": proof.owner,
    })
}

fn obligation_account_proof_json(proof: &ObligationAccountProof) -> Value {
    json!({
        "account": account_proof_json(&proof.account),
        "owner": proof.owner,
        "lendingMarket": proof.lending_market,
        "activeDepositCount": proof.active_deposit_count,
        "activeBorrowCount": proof.active_borrow_count,
        "reserveDepositedAmountRaw": proof.reserve_deposited_amount_raw.map(|amount| amount.to_string()),
    })
}

fn chain_position_json(position: &ChainPositionSummary) -> Value {
    json!({
        "reserve": position.reserve,
        "market": position.market,
        "liquidityMint": position.liquidity_mint,
        "liquidityTokenProgram": position.liquidity_token_program,
        "reserveLiquiditySupply": position.reserve_liquidity_supply,
        "collateralMint": position.collateral_mint,
        "reserveCollateralSupply": position.reserve_collateral_supply,
        "collateralFarm": position.collateral_farm,
        "collateralFarmUserState": position.collateral_farm_user_state,
        "collateralFarmUserStateExists": position.collateral_farm_user_state_exists,
        "pythOracle": position.pyth_oracle,
        "switchboardPriceOracle": position.switchboard_price_oracle,
        "switchboardTwapOracle": position.switchboard_twap_oracle,
        "scopePrices": position.scope_prices,
        "obligation": position.obligation,
        "obligationExists": position.obligation_exists,
        "obligationDepositReserves": position.obligation_deposit_reserves,
        "obligationBorrowReserves": position.obligation_borrow_reserves,
        "amountRaw": position.amount_raw.to_string(),
        "hasValue": position.amount_raw > 0,
        "sourceCollateralAmountRaw": position.amount_raw.to_string(),
        "redeemableSourceLiquidityAmountRaw": position.redeemable_liquidity_amount_raw.to_string(),
        "vaultLiquidityAta": position.vault_liquidity_ata,
        "vaultLiquidityTokenAccountExists": position.vault_liquidity_token_account_exists,
        "vaultLiquidityAmountRaw": position.vault_liquidity_amount_raw.to_string(),
        "amountSemantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
    })
}

fn user_position_seed_preview_json(preview: &UserPositionSeedPreview) -> Value {
    json!({
        "source": preview.source,
        "rows": preview.rows.iter().map(user_position_seed_row_json).collect::<Vec<_>>(),
        "positions": preview.positions.iter().map(position_json).collect::<Vec<_>>(),
        "amountSemantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
        "dryRunOnly": true,
    })
}

fn user_position_seed_row_json(row: &UserPositionSeedRow) -> Value {
    json!({
        "id": row.id,
        "currentReserve": row.current_reserve,
        "currentMarket": row.current_market,
        "currentLiquidityMint": row.current_liquidity_mint,
        "currentAmountRaw": row.current_amount_raw.to_string(),
        "currentObservedSlot": row.current_observed_slot,
        "currentObservedAt": row.current_observed_at,
    })
}

fn policy_account_preflight_json(preflight: &PolicyAccountPreflight) -> Value {
    json!({
        "method": "decoded_squads_policy_account",
        "policyAccount": preflight.policy_account,
        "sourceMarket": preflight.source_market,
        "targetMarket": preflight.target_market,
        "sourceMarketPresent": preflight.decoded.kamino_markets.iter().any(|market| market == &preflight.source_market),
        "targetMarketPresent": preflight.decoded.kamino_markets.iter().any(|market| market == &preflight.target_market),
        "decodedAllowsRequiredMarkets": preflight.allows_required_markets(),
        "decodedAllowsRequiredRouteSteps": preflight.allows_required_route_steps(),
        "decodedAllowsInitObligation": preflight.allows_init_obligation(),
        "decodedAllowsRefreshObligation": preflight.allows_refresh_obligation(),
        "decodedPolicyAccount": decoded_policy_account_json(&preflight.decoded),
    })
}

fn policy_route_preflight_json(
    vault: &SelectedVault,
    reserve_move: &ReserveMove,
    policy_account: Option<&PolicyAccountPreflight>,
) -> Value {
    let source_market = policy_account
        .map(|preflight| preflight.source_market.clone())
        .or_else(|| market_hint_for_reserve(&reserve_move.source_reserve).map(str::to_owned));
    let target_market = policy_account
        .map(|preflight| preflight.target_market.clone())
        .or_else(|| market_hint_for_reserve(&reserve_move.target_reserve).map(str::to_owned));
    let neon_allows_required_markets =
        source_market
            .as_ref()
            .zip(target_market.as_ref())
            .map(|(source_market, target_market)| {
                vault
                    .kamino_markets
                    .iter()
                    .any(|market| market == source_market)
                    && vault
                        .kamino_markets
                        .iter()
                        .any(|market| market == target_market)
            });
    json!({
        "method": "neon_route_policy_with_decoded_policy_account",
        "policyAccount": vault.policy_account,
        "sourceMarket": source_market,
        "targetMarket": target_market,
        "neonAllowsRequiredMarkets": neon_allows_required_markets,
        "neonAllowedLiquidityMints": vault.kamino_liquidity_mints,
        "neonRouteModes": vault.route_modes,
        "policyAccountDecode": policy_account.map(policy_account_preflight_json),
    })
}

fn market_hint_for_reserve(reserve: &str) -> Option<&'static str> {
    if reserve == KAMINO_MAIN_USDC_RESERVE.to_string() {
        Some(KAMINO_MAIN_MARKET)
    } else if reserve == KAMINO_PRIME_USDC_RESERVE {
        Some(KAMINO_PRIME_MARKET)
    } else {
        None
    }
}

fn same_mint_usdc_policy_universe() -> Result<YieldRouteUniverse, Box<dyn Error>> {
    Ok(YieldRouteUniverse::new(
        vec![USDC_MINT],
        vec![
            Pubkey::from_str(KAMINO_MAIN_MARKET)?,
            Pubkey::from_str(KAMINO_PRIME_MARKET)?,
            Pubkey::from_str(KAMINO_MAPLE_MARKET)?,
            Pubkey::from_str(KAMINO_ONRE_MARKET)?,
            Pubkey::from_str(KAMINO_ETHENA_MARKET)?,
        ],
        vec![USDC_MINT],
    ))
}

fn pubkeys_json(pubkeys: &[Pubkey]) -> Vec<String> {
    pubkeys.iter().map(Pubkey::to_string).collect()
}

fn swap_lanes_json(swap_lanes: &[SwapLane]) -> Vec<Value> {
    swap_lanes
        .iter()
        .map(|lane| match lane {
            SwapLane::Jupiter(contract) => json!({
                "kind": "jupiter",
                "programId": contract.program_id.to_string(),
                "exactInDiscriminator": contract.exact_in_discriminator,
                "maxSlippageBps": contract.max_slippage_bps,
            }),
            SwapLane::LoyalHub {
                hub_authorizer,
                max_fee_bps,
            } => json!({
                "kind": "loyal_hub",
                "hubAuthorizer": hub_authorizer.to_string(),
                "maxFeeBps": max_fee_bps,
            }),
        })
        .collect()
}

fn policy_swap_lanes_json(
    setup: &YieldRouteActionSetup,
    swap_lanes: &[SwapLane],
) -> Result<Value, Box<dyn Error>> {
    let action_account = setup.accounts.withdraw.to_string();
    let deposit_index = u8::try_from(1 + swap_lanes.len())?;
    let lanes = swap_lanes
        .iter()
        .enumerate()
        .map(|(offset, lane)| -> Result<Value, Box<dyn Error>> {
            let swap_index = u8::try_from(1 + offset)?;
            Ok(match lane {
                SwapLane::Jupiter(contract) => json!({
                    "lane": "jupiter",
                    "programId": contract.program_id.to_string(),
                    "exactInDiscriminator": contract.exact_in_discriminator,
                    "maxSlippageBps": contract.max_slippage_bps,
                    "actionAccount": action_account.clone(),
                    "instructionConstraintIndexes": [0_u8, swap_index, deposit_index],
                }),
                SwapLane::LoyalHub {
                    hub_authorizer,
                    max_fee_bps,
                } => json!({
                    "lane": "loyal_hub",
                    "hubAuthorizer": hub_authorizer.to_string(),
                    "maxFeeBps": max_fee_bps,
                    "actionAccount": action_account.clone(),
                    "instructionConstraintIndexes": [0_u8, swap_index, deposit_index],
                }),
            })
        })
        .collect::<Result<Vec<_>, Box<dyn Error>>>()?;
    Ok(Value::Array(lanes))
}

fn decoded_policy_account_json(decoded: &DecodedPolicyAccount) -> Value {
    json!({
        "layout": decoded.layout.as_str(),
        "delegatedSigners": decoded.delegated_signers,
        "threshold": decoded.threshold,
        "accountIndex": decoded.account_index,
        "instructionCount": decoded.instruction_count,
        "kaminoMarkets": decoded.kamino_markets,
        "kaminoLiquidityMints": decoded.kamino_liquidity_mints,
        "instructions": decoded.instructions.iter().map(decoded_policy_instruction_json).collect::<Vec<_>>(),
    })
}

fn decoded_policy_instruction_json(instruction: &DecodedPolicyInstructionSummary) -> Value {
    json!({
        "programId": instruction.program_id,
        "routeStep": instruction.route_step,
        "dataDiscriminator": instruction.data_discriminator,
        "markets": instruction.markets,
        "liquidityMints": instruction.liquidity_mints,
        "accountConstraints": instruction.account_constraints.iter().map(decoded_policy_account_constraint_json).collect::<Vec<_>>(),
    })
}

fn decoded_policy_account_constraint_json(
    constraint: &DecodedPolicyAccountConstraintSummary,
) -> Value {
    json!({
        "accountIndex": constraint.account_index,
        "kind": constraint.kind,
        "pubkeys": constraint.pubkeys,
        "owner": constraint.owner,
        "dataConstraints": constraint.data_constraints.iter().map(decoded_policy_data_constraint_json).collect::<Vec<_>>(),
    })
}

fn decoded_policy_data_constraint_json(constraint: &DecodedPolicyDataConstraintSummary) -> Value {
    json!({
        "dataOffset": constraint.data_offset,
        "operator": constraint.operator,
        "value": constraint.value,
    })
}

fn same_mint_input_json(input: &SameMintRebalanceInput) -> Value {
    json!({
        "vaultId": input.vault_id.map(VaultId::as_i64),
        "sourceReserve": input.source_reserve,
        "targetReserve": input.target_reserve,
        "liquidityMint": input.liquidity_mint,
        "amountRaw": input.amount_raw.to_string(),
        "routeAmountSemantics": input.route_amount_semantics,
        "sourceAmountSemantics": input.source_amount_semantics,
        "sourceCollateralAmountRaw": input.source_collateral_amount_raw.map(|amount| amount.to_string()),
        "redeemableSourceLiquidityAmountRaw": input.redeemable_source_liquidity_amount_raw.map(|amount| amount.to_string()),
        "idleVaultLiquidityAmountRaw": input.idle_vault_liquidity_amount_raw.map(|amount| amount.to_string()),
        "sourceSnapshotId": input.expected_source_snapshot_id.as_i64(),
        "sourceApyBps": input.source_apy_bps,
        "targetApyBps": input.target_apy_bps,
        "estimatedEdgeBps": input.estimated_edge_bps,
        "estimatedCostLamports": input.estimated_cost_lamports,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    const CREDENTIAL_BEARING_RPC_ERROR: &str = "sendTransaction failed with HTTP 401 Unauthorized at https://user:password@example.test/private/path?api-key=query-secret access_token=header-secret";

    fn fleet_worker_completion_identity<'a>(
        opportunity_id: i64,
        route_fingerprint: Option<&'a str>,
        requirements_fingerprint: Option<&'a str>,
    ) -> FleetWorkerCompletionIdentity<'a> {
        FleetWorkerCompletionIdentity {
            opportunity_id,
            route_fingerprint,
            requirements_fingerprint,
        }
    }

    #[test]
    fn fleet_worker_completion_accepts_exact_decision_and_completed_states() {
        let exact = fleet_worker_completion_identity(405_569, Some("route-v1"), Some("req-v1"));

        for state in [
            RebalanceOpportunityState::DecisionCreated,
            RebalanceOpportunityState::Completed,
        ] {
            assert!(
                validate_fleet_worker_completion(exact, exact, exact, state, true).is_ok(),
                "expected exact {} state to be accepted",
                state.as_str()
            );
        }
    }

    #[test]
    fn fleet_worker_completion_rejects_identity_drift_and_unrelated_states() {
        let exact = fleet_worker_completion_identity(405_569, Some("route-v1"), Some("req-v1"));

        let divergent_identities = [
            fleet_worker_completion_identity(405_570, Some("route-v1"), Some("req-v1")),
            fleet_worker_completion_identity(405_569, Some("route-v2"), Some("req-v1")),
            fleet_worker_completion_identity(405_569, Some("route-v1"), Some("req-v2")),
            fleet_worker_completion_identity(405_569, None, Some("req-v1")),
            fleet_worker_completion_identity(405_569, Some("route-v1"), None),
        ];
        for outcome in divergent_identities {
            assert!(
                validate_fleet_worker_completion(
                    exact,
                    outcome,
                    exact,
                    RebalanceOpportunityState::Completed,
                    true,
                )
                .is_err(),
                "expected divergent worker outcome identity to be rejected: {outcome:?}"
            );
        }
        for current in divergent_identities {
            assert!(
                validate_fleet_worker_completion(
                    exact,
                    exact,
                    current,
                    RebalanceOpportunityState::Completed,
                    true,
                )
                .is_err(),
                "expected divergent completed identity to be rejected: {current:?}"
            );
        }
        for state in [
            RebalanceOpportunityState::DecisionCreated,
            RebalanceOpportunityState::Completed,
        ] {
            assert!(validate_fleet_worker_completion(exact, exact, exact, state, false).is_err());
        }

        for state in [
            RebalanceOpportunityState::WaitingAlt,
            RebalanceOpportunityState::Revalidate,
            RebalanceOpportunityState::Ready,
            RebalanceOpportunityState::Leased,
            RebalanceOpportunityState::Stale,
            RebalanceOpportunityState::Superseded,
            RebalanceOpportunityState::Failed,
            RebalanceOpportunityState::Cancelled,
        ] {
            assert!(
                validate_fleet_worker_completion(exact, exact, exact, state, true).is_err(),
                "expected unrelated {} state to be rejected",
                state.as_str()
            );
        }
    }

    #[tokio::test]
    async fn ready_fleet_worker_task_preempts_ready_health_tick() {
        let mut tasks = JoinSet::new();
        tasks.spawn(async { 7_u8 });
        tokio::task::yield_now().await;
        let mut health_interval = tokio::time::interval(Duration::from_secs(1));

        match next_fleet_worker_wakeup(&mut tasks, &mut health_interval).await {
            FleetWorkerWakeup::Task(Some(Ok(7))) => {}
            wakeup => panic!("ready task did not preempt ready health tick: {wakeup:?}"),
        }
    }

    fn assert_safe_operational_error(value: &str) {
        assert!(value.contains("sendTransaction"));
        assert!(value.contains("HTTP 401 Unauthorized"));
        assert!(value.len() <= 512);
        for secret in [
            "user",
            "password",
            "private/path",
            "api-key",
            "query-secret",
            "access_token",
            "header-secret",
        ] {
            assert!(
                !value.contains(secret),
                "operational error leaked {secret}: {value}"
            );
        }
    }

    #[test]
    fn idle_lookup_table_blocker_requires_predecision_provisioning_defer() {
        let blockers = vec![
            IdleVaultDepositBlocker::source_stale("source snapshot changed"),
            IdleVaultDepositBlocker::route_resolution(
                "idle deposit route resolver blocked",
                "reusable-only runtime requires complete reusable ALT coverage and simulation",
            ),
        ];

        assert!(idle_vault_deposit_requires_lookup_table_provisioning(
            &blockers
        ));
        assert!(!idle_vault_deposit_has_only_source_sync_blockers(&blockers));
        assert!(!idle_vault_deposit_requires_lookup_table_provisioning(&[
            IdleVaultDepositBlocker::source_stale("source snapshot changed"),
            IdleVaultDepositBlocker::safety("unrelated safety blocker"),
        ]));
    }

    #[test]
    fn same_mint_fatal_log_payload_never_contains_rpc_credentials() {
        let payload = same_mint_fatal_error_payload(&CREDENTIAL_BEARING_RPC_ERROR);
        assert_eq!(payload["event"], "same_mint_route_worker_fatal");
        assert_safe_operational_error(payload["error"].as_str().unwrap());
    }

    #[test]
    fn same_mint_readiness_blocker_never_contains_rpc_credentials() {
        let blocker = same_mint_readiness_rpc_failure(&CREDENTIAL_BEARING_RPC_ERROR);
        assert!(blocker.starts_with("simulation_rpc_failed:"));
        assert_safe_operational_error(&blocker);
    }

    #[test]
    fn missing_token_account_simulation_error_recognizes_anchor_and_rpc_forms() {
        for error in [
            "InstructionError(2, Custom(3012))",
            "AnchorError caused by account: user_source_liquidity. Error Code: AccountNotInitialized",
            "Error processing Instruction 2: custom program error: 0xbc4",
            "account not initialized",
        ] {
            assert!(
                is_account_not_initialized_simulation_error(error),
                "expected missing-account error: {error}"
            );
        }
        assert!(!is_account_not_initialized_simulation_error(
            "InstructionError(2, Custom(6001))"
        ));
    }

    #[test]
    fn same_mint_persisted_failure_reason_never_contains_rpc_credentials() {
        let reason = same_mint_decision_failure_reason(
            "route_execution_failed",
            &CREDENTIAL_BEARING_RPC_ERROR,
        );
        assert!(reason.starts_with("route_execution_failed:"));
        assert_safe_operational_error(&reason);
    }

    #[test]
    fn same_mint_startup_rejects_default_mainnet_rpc_for_explicit_devnet() {
        let mainnet_genesis =
            Hash::from_str("5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d").unwrap();

        let error = validate_same_mint_rpc_genesis("devnet", mainnet_genesis).unwrap_err();

        assert!(error.contains("mismatched RPC"));
        assert!(validate_same_mint_rpc_genesis("mainnet-beta", mainnet_genesis).is_ok());
    }

    #[test]
    fn fleet_source_observation_evidence_is_not_relabelled_across_source_kinds() {
        let observed_at = "2026-07-16T03:11:11Z";
        let idle_account = Pubkey::new_unique().to_string();
        let generic_source_plan = json!({
            "source_kind": "reserve_position",
            "source_observed_slot": 433_191_369,
            "source_observed_at": observed_at,
            "idle_token_account": null,
        });

        let reserve = project_fleet_route_source_evidence(
            SameMintRouteSourceKind::ReservePosition,
            &generic_source_plan,
        )
        .unwrap();
        assert_eq!(reserve.expected_idle_token_account, None);
        assert_eq!(reserve.expected_idle_observed_slot, None);
        assert_eq!(reserve.expected_idle_observed_at, None);
        assert!(validate_fleet_route_source_evidence(
            SameMintRouteSourceKind::ReservePosition,
            Some(&Pubkey::new_unique().to_string()),
            Some(1),
            &reserve,
        )
        .is_ok());

        let idle_plan = json!({
            "source_kind": "idle_vault_usdc",
            "source_observed_slot": 433_191_369,
            "source_observed_at": observed_at,
            "idle_token_account": idle_account,
        });
        let idle =
            project_fleet_route_source_evidence(SameMintRouteSourceKind::IdleVaultUsdc, &idle_plan)
                .unwrap();
        assert_eq!(
            idle.expected_idle_token_account.as_deref(),
            Some(idle_account.as_str())
        );
        assert_eq!(idle.expected_idle_observed_slot, Some(433_191_369));
        assert_eq!(
            idle.expected_idle_observed_at.unwrap().to_rfc3339(),
            "2026-07-16T03:11:11+00:00"
        );
        assert!(validate_fleet_route_source_evidence(
            SameMintRouteSourceKind::IdleVaultUsdc,
            None,
            None,
            &idle,
        )
        .is_ok());

        let corrupt_reserve = project_fleet_route_source_evidence(
            SameMintRouteSourceKind::ReservePosition,
            &idle_plan,
        )
        .unwrap();
        assert_eq!(
            corrupt_reserve.expected_idle_token_account.as_deref(),
            Some(idle_account.as_str())
        );
        assert_eq!(corrupt_reserve.expected_idle_observed_slot, None);
        assert_eq!(corrupt_reserve.expected_idle_observed_at, None);
        assert_eq!(
            validate_fleet_route_source_evidence(
                SameMintRouteSourceKind::ReservePosition,
                Some(&Pubkey::new_unique().to_string()),
                Some(2),
                &corrupt_reserve,
            )
            .unwrap_err(),
            "same-mint reserve-position request cannot carry idle-vault evidence"
        );
    }

    fn test_chain_position(
        collateral_amount: u64,
        redeemable_liquidity_amount: u64,
    ) -> ChainPositionSummary {
        ChainPositionSummary {
            reserve: "source-reserve".to_owned(),
            market: "market".to_owned(),
            liquidity_mint: "mint".to_owned(),
            liquidity_token_program: "token-program".to_owned(),
            reserve_liquidity_supply: "liquidity-supply".to_owned(),
            collateral_mint: "collateral-mint".to_owned(),
            reserve_collateral_supply: "collateral-supply".to_owned(),
            collateral_farm: None,
            collateral_farm_user_state: None,
            collateral_farm_user_state_exists: false,
            pyth_oracle: None,
            switchboard_price_oracle: None,
            switchboard_twap_oracle: None,
            scope_prices: None,
            obligation: "obligation".to_owned(),
            obligation_exists: true,
            obligation_deposit_reserves: Vec::new(),
            obligation_borrow_reserves: Vec::new(),
            amount_raw: collateral_amount,
            redeemable_liquidity_amount_raw: redeemable_liquidity_amount,
            vault_liquidity_ata: "vault-liquidity-ata".to_owned(),
            vault_liquidity_token_account_exists: true,
            vault_liquidity_amount_raw: 0,
        }
    }

    fn test_same_mint_input(
        route_liquidity_amount: i64,
        source_collateral_amount: Option<i64>,
    ) -> SameMintRebalanceInput {
        SameMintRebalanceInput {
            vault_id: None,
            settings: None,
            vault_index: None,
            source_reserve: "source-reserve".to_owned(),
            target_reserve: "target-reserve".to_owned(),
            liquidity_mint: "mint".to_owned(),
            amount_raw: route_liquidity_amount,
            route_amount_semantics: ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned(),
            source_amount_semantics: Some(AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED.to_owned()),
            source_collateral_amount_raw: source_collateral_amount,
            redeemable_source_liquidity_amount_raw: Some(route_liquidity_amount),
            idle_vault_liquidity_amount_raw: Some(0),
            expected_source_snapshot_id: SnapshotId(1),
            source_apy_bps: 100,
            target_apy_bps: 200,
            estimated_edge_bps: 100,
            estimated_cost_lamports: 5_000,
            dry_run: false,
        }
    }

    fn test_cli_options(
        execute: bool,
        reconcile_from_chain: bool,
        expected_source_snapshot_id: Option<i64>,
    ) -> CliOptions {
        CliOptions {
            settings: "settings".to_owned(),
            vault_index: 1,
            direction: Direction::MainToPrime,
            source_reserve: Some("source-reserve".to_owned()),
            target_reserve: Some("target-reserve".to_owned()),
            update_policy: false,
            update_active_policy: false,
            initial_deposit_reserve: None,
            initial_deposit_amount_raw: None,
            idle_vault_deposit_reserve: None,
            idle_vault_deposit_amount_raw: None,
            full_withdraw_main_usdc: false,
            full_withdraw_reserve: None,
            setup_obligation_reserve: None,
            e2e_deposit_amount_raw: None,
            execute,
            prepare_only: false,
            read_only: false,
            fused_execute: false,
            optimization_cycle: true,
            reconcile_from_chain,
            reconcile_current_positions: false,
            reconcile_reserves: Vec::new(),
            seed_from_user_position: false,
            expected_source_snapshot_id,
            expected_liquidity_mint: Some("mint".to_owned()),
            expected_amount_raw: Some(480_000_000),
            expected_route_amount_semantics: Some(
                ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned(),
            ),
            expected_idle_token_account: None,
            expected_idle_observed_slot: None,
            expected_idle_observed_at: None,
            expected_source_apy_bps: Some(100),
            expected_observed_target_apy_bps: None,
            expected_target_apy_bps: Some(200),
            expected_edge_bps: Some(100),
            principal_usd_micros: None,
            confidence_ppm: None,
            expected_service_millis: None,
            holding_horizon_seconds: None,
            estimated_execution_cost_usd_micros: None,
            expected_cost_lamports: Some(5_000),
            current_economic_fee_cap_lamports: None,
            expected_route_fee_payer: None,
            optimizer_epoch_id: None,
            optimizer_market_slot: None,
            opportunity_id: None,
            opportunity_lease_owner: None,
            opportunity_fencing_token: None,
            cluster: "localnet".to_owned(),
            rpc_url: "http://localhost:8899".to_owned(),
        }
    }

    #[test]
    fn reusable_runtime_rejects_every_legacy_rollout_state() {
        let rollout = |rollout_mode, force_legacy| EffectiveLookupTableRollout {
            rollout_mode,
            force_legacy,
            global: None,
            vault: None,
        };

        assert!(reusable_runtime_blocker(
            &rollout(LookupTableRolloutMode::ReusableOnly, false),
            true,
            true,
        )
        .is_none());
        assert!(reusable_runtime_blocker(
            &rollout(LookupTableRolloutMode::ReusableOnly, false),
            true,
            false,
        )
        .is_some_and(|blocker| blocker.contains("complete reusable ALT coverage")));
        assert!(reusable_runtime_blocker(
            &rollout(LookupTableRolloutMode::ReusableOnly, false),
            false,
            true,
        )
        .is_some_and(|blocker| blocker.contains("shared_market_catalog_drift")));

        for mode in [
            LookupTableRolloutMode::Legacy,
            LookupTableRolloutMode::Shadow,
            LookupTableRolloutMode::PreferReusable,
        ] {
            assert!(reusable_runtime_blocker(&rollout(mode, false), true, true)
                .is_some_and(|blocker| blocker.contains("legacy ALT resolution has been removed")));
        }

        assert!(reusable_runtime_blocker(
            &rollout(LookupTableRolloutMode::ReusableOnly, true),
            true,
            true,
        )
        .is_some_and(|blocker| blocker.contains("force-legacy is a fail-closed stop")));
    }

    #[test]
    fn legacy_lookup_table_cli_argument_is_rejected() {
        let error = parse_args([
            "--lookup-table".to_owned(),
            Pubkey::new_unique().to_string(),
        ])
        .unwrap_err();

        assert_eq!(error, "unknown argument: --lookup-table");
    }

    #[test]
    fn remaining_lane_manifests_cover_policy_setup_deposit_and_cleanup_accounts() {
        let settings = Pubkey::new_unique();
        let authority = Pubkey::new_unique();
        let delegated = solana_sdk::signature::Keypair::new();
        let vault_pubkey = Pubkey::new_unique();
        let policy = Pubkey::new_unique();
        let reserve = Pubkey::new_unique();
        let market = Pubkey::new_unique();
        let liquidity_supply = Pubkey::new_unique();
        let collateral_mint = Pubkey::new_unique();
        let collateral_supply = Pubkey::new_unique();
        let metadata = user_metadata(&KLEND_PROGRAM_ID, &vault_pubkey).0;
        let obligation = obligation(
            &KLEND_PROGRAM_ID,
            0,
            0,
            &vault_pubkey,
            &market,
            &Pubkey::default(),
            &Pubkey::default(),
        )
        .0;
        let vault_ata = Pubkey::new_unique();
        let wallet_ata = derive_associated_token_address(&authority, &USDC_MINT, &spl_token::ID);
        let selected_vault = SelectedVault {
            id: VaultId(1),
            settings: settings.to_string(),
            authority: authority.to_string(),
            policy_seed: 7,
            vault_index: 0,
            vault_pubkey: vault_pubkey.to_string(),
            policy_account: policy.to_string(),
            setup_policy_account: None,
            setup_policy_seed: None,
            delegated_signers: vec![delegated.pubkey().to_string()],
            threshold: 1,
            route_modes: vec![SAME_MINT_ROUTE_MODE.to_owned()],
            stable_mints: vec![USDC_MINT.to_string()],
            kamino_markets: vec![market.to_string()],
            kamino_liquidity_mints: vec![USDC_MINT.to_string()],
            swap_lanes: Value::Array(Vec::new()),
        };
        let preview = ChainReconcilePreview {
            observed_slot: 42,
            vault_user_metadata: metadata.to_string(),
            vault_user_metadata_exists: true,
            rpc_account_reads: FleetRpcAccountReadEvidence::default(),
            positions: vec![ChainPositionSummary {
                reserve: reserve.to_string(),
                market: market.to_string(),
                liquidity_mint: USDC_MINT.to_string(),
                liquidity_token_program: spl_token::ID.to_string(),
                reserve_liquidity_supply: liquidity_supply.to_string(),
                collateral_mint: collateral_mint.to_string(),
                reserve_collateral_supply: collateral_supply.to_string(),
                collateral_farm: None,
                collateral_farm_user_state: None,
                collateral_farm_user_state_exists: false,
                pyth_oracle: None,
                switchboard_price_oracle: None,
                switchboard_twap_oracle: None,
                scope_prices: None,
                obligation: obligation.to_string(),
                obligation_exists: false,
                obligation_deposit_reserves: Vec::new(),
                obligation_borrow_reserves: Vec::new(),
                amount_raw: 1,
                redeemable_liquidity_amount_raw: 1,
                vault_liquidity_ata: vault_ata.to_string(),
                vault_liquidity_token_account_exists: true,
                vault_liquidity_amount_raw: 1,
            }],
        };
        let target = &preview.positions[0];

        let policy_instruction = remove_policy_instruction(settings, authority, policy);
        let policy_manifest = policy_lookup_table_manifest(
            authority,
            std::slice::from_ref(&policy_instruction),
            &selected_vault,
            &[],
            &[policy],
        )
        .expect("policy operation should have complete typed provenance");
        assert!(policy_manifest
            .vault()
            .iter()
            .any(|requirement| requirement.address == settings));
        assert!(policy_manifest
            .vault()
            .iter()
            .any(|requirement| requirement.address == policy));

        let setup_plan = init_obligation_execution_instructions(
            policy,
            0,
            vault_pubkey,
            target,
            0,
            &delegated,
            &[],
        )
        .expect("setup instructions should build");
        let setup_manifest = route_lookup_table_manifest(
            delegated.pubkey(),
            setup_plan.instructions(),
            &selected_vault,
            setup_plan.lookup_table_requirements(),
            &[],
        )
        .expect("setup operation should have complete typed provenance");
        assert!(setup_manifest
            .vault()
            .iter()
            .any(|requirement| requirement.address == obligation));

        let deposit_inner =
            kamino_deposit_to_obligation_instruction(vault_pubkey, target, vault_ata, 1)
                .expect("deposit instruction should build");
        let (deposit_outer, _, _, _) = build_program_interaction_policy_execution_instruction(
            policy,
            delegated.pubkey(),
            0,
            deposit_inner,
            0,
        )
        .expect("deposit policy wrapper should build");
        let deposit_manifest = route_lookup_table_manifest(
            delegated.pubkey(),
            std::slice::from_ref(deposit_outer.instruction()),
            &selected_vault,
            deposit_outer.lookup_table_requirements(),
            &[],
        )
        .expect("deposit operation should have complete typed provenance");
        assert!(deposit_manifest
            .shared_market()
            .iter()
            .any(|requirement| requirement.address == reserve));

        let recovery_plan = vault_usdc_recovery_instructions(
            settings,
            authority,
            vault_pubkey,
            0,
            wallet_ata,
            vault_ata,
            1,
        )
        .expect("cleanup instructions should build");
        let recovery_manifest = route_lookup_table_manifest(
            authority,
            recovery_plan.instructions(),
            &selected_vault,
            recovery_plan.lookup_table_requirements(),
            &[wallet_ata],
        )
        .expect("cleanup operation should have complete typed provenance");
        assert!(recovery_manifest
            .vault()
            .iter()
            .any(|requirement| requirement.address == wallet_ata));
        assert!(recovery_manifest
            .vault()
            .iter()
            .any(|requirement| requirement.address == vault_ata));
    }

    #[test]
    fn accepts_explicit_alt_cluster() {
        let options = parse_args(vec![
            "--settings".to_owned(),
            Pubkey::new_unique().to_string(),
            "--vault-index".to_owned(),
            "1".to_owned(),
            "--update-policy".to_owned(),
            "--cluster".to_owned(),
            "devnet".to_owned(),
        ])
        .expect("explicit ALT cluster should parse");

        assert_eq!(options.cluster, "devnet");
    }

    #[test]
    fn rejects_invalid_alt_cluster() {
        let error = parse_args(vec![
            "--settings".to_owned(),
            Pubkey::new_unique().to_string(),
            "--vault-index".to_owned(),
            "1".to_owned(),
            "--update-policy".to_owned(),
            "--cluster".to_owned(),
            "https://api.mainnet-beta.solana.com".to_owned(),
        ])
        .unwrap_err();

        assert!(error.contains("must be mainnet-beta, devnet, testnet, or localnet"));
    }

    #[test]
    fn live_route_execution_still_writes_current_positions_from_chain() {
        let options = test_cli_options(true, true, Some(1));
        let mut seed_options = test_cli_options(true, false, Some(1));
        seed_options.seed_from_user_position = true;

        assert!(writes_current_positions_from_chain(&options));
        assert!(writes_current_positions_from_user_seed(&seed_options));
        assert!(!uses_chain_preview_positions(&options, true));
    }

    #[test]
    fn rejects_all_lookup_table_mutations_in_route_transactions() {
        let variants = [
            (
                "create",
                address_lookup_table_instruction::ProgramInstruction::CreateLookupTable {
                    recent_slot: 42,
                    bump_seed: 1,
                },
            ),
            (
                "extend",
                address_lookup_table_instruction::ProgramInstruction::ExtendLookupTable {
                    new_addresses: vec![Pubkey::new_unique()],
                },
            ),
            (
                "freeze",
                address_lookup_table_instruction::ProgramInstruction::FreezeLookupTable,
            ),
            (
                "deactivate",
                address_lookup_table_instruction::ProgramInstruction::DeactivateLookupTable,
            ),
            (
                "close",
                address_lookup_table_instruction::ProgramInstruction::CloseLookupTable,
            ),
        ];

        for (kind, variant) in variants {
            let instruction = Instruction {
                program_id: address_lookup_table_program::id(),
                accounts: Vec::new(),
                data: bincode::serialize(&variant).expect("ALT variant should serialize"),
            };
            let error = guard_lookup_table_mutations(&[instruction], "route execution")
                .unwrap_err()
                .to_string();
            assert!(
                error.contains(&format!("Address Lookup Table {kind} instruction")),
                "unexpected error for {kind}: {error}"
            );
        }

        let malformed = Instruction {
            program_id: address_lookup_table_program::id(),
            accounts: Vec::new(),
            data: vec![0xff, 0x01, 0x02],
        };
        let error = guard_lookup_table_mutations(&[malformed.clone()], "route execution")
            .unwrap_err()
            .to_string();
        assert!(error.contains("Address Lookup Table unknown instruction"));

        let error = build_program_interaction_policy_execution_instruction(
            Pubkey::new_unique(),
            Pubkey::new_unique(),
            0,
            YieldRouteInstruction::new(malformed, YieldRouteLookupTableRequirements::default()),
            0,
        )
        .unwrap_err()
        .to_string();
        assert!(
            error.contains("raw Squads program-interaction inner instruction rejected")
                && error.contains("Address Lookup Table unknown instruction"),
            "unexpected nested-mutation error: {error}"
        );
    }

    #[test]
    fn compiler_lookup_eligibility_keeps_all_required_static_keys_out_of_alts() {
        let payer = Pubkey::new_unique();
        let signer = Pubkey::new_unique();
        let nonce = Pubkey::new_unique();
        let invoked_program = Pubkey::new_unique();
        let lookup_eligible = Pubkey::new_unique();
        let nonce_instruction = Instruction {
            program_id: system_program::ID,
            accounts: vec![
                AccountMeta::new(nonce, false),
                AccountMeta::new_readonly(signer, true),
            ],
            data: vec![4, 0, 0, 0],
        };
        let route_instruction = Instruction {
            program_id: invoked_program,
            accounts: vec![
                AccountMeta::new_readonly(invoked_program, false),
                AccountMeta::new_readonly(signer, true),
                AccountMeta::new(lookup_eligible, false),
            ],
            data: Vec::new(),
        };

        assert_eq!(
            best_case_lookup_table_addresses(payer, &[nonce_instruction, route_instruction],),
            vec![lookup_eligible]
        );
    }

    #[test]
    fn reusable_alt_compiler_evidence_covers_missing_obligation_and_farm_route_variants() {
        struct Variant {
            name: &'static str,
            missing_obligation: bool,
            collateral_farm: bool,
        }
        let variants = [
            Variant {
                name: "existing_obligation_no_farm",
                missing_obligation: false,
                collateral_farm: false,
            },
            Variant {
                name: "missing_obligation_no_farm",
                missing_obligation: true,
                collateral_farm: false,
            },
            Variant {
                name: "missing_obligation_with_farm",
                missing_obligation: true,
                collateral_farm: true,
            },
        ];

        for variant in variants {
            let payer_keypair = solana_sdk::signature::Keypair::new();
            let signer_keypair = solana_sdk::signature::Keypair::new();
            let payer = payer_keypair.pubkey();
            let signer = signer_keypair.pubkey();
            let market = Pubkey::new_unique();
            let reserve = Pubkey::new_unique();
            let vault = Pubkey::new_unique();
            let obligation = Pubkey::new_unique();
            let farm_state = Pubkey::new_unique();
            let farm_user_state = Pubkey::new_unique();
            let mut requirements = YieldRouteLookupTableRequirements::default();
            requirements.add_vault_account(vault);
            let mut reserve_accounts =
                KaminoReserveLookupTableAccounts::new(market, reserve, Pubkey::new_unique());
            if variant.collateral_farm {
                reserve_accounts.reserve_farm_state = Some(farm_state);
            }
            requirements.add_kamino_reserve(reserve_accounts);
            let mut instructions = vec![Instruction {
                program_id: KLEND_PROGRAM_ID,
                accounts: vec![
                    AccountMeta::new_readonly(signer, true),
                    AccountMeta::new_readonly(market, false),
                    AccountMeta::new(reserve, false),
                    AccountMeta::new(vault, false),
                ],
                data: vec![1],
            }];
            if variant.missing_obligation {
                requirements.add_obligation(obligation);
                instructions.push(Instruction {
                    program_id: KLEND_PROGRAM_ID,
                    accounts: vec![
                        AccountMeta::new_readonly(signer, true),
                        AccountMeta::new(vault, false),
                        AccountMeta::new(obligation, false),
                        AccountMeta::new_readonly(market, false),
                    ],
                    data: vec![2],
                });
            }
            if variant.collateral_farm {
                requirements.add_farm_user_state(farm_user_state);
                instructions.push(Instruction {
                    program_id: FARMS_PROGRAM_ID,
                    accounts: vec![
                        AccountMeta::new_readonly(signer, true),
                        AccountMeta::new_readonly(farm_state, false),
                        AccountMeta::new(farm_user_state, false),
                        AccountMeta::new_readonly(reserve, false),
                        AccountMeta::new(vault, false),
                    ],
                    data: vec![3],
                });
            }

            let manifest = requirements
                .manifest(payer, &instructions)
                .unwrap_or_else(|error| panic!("{} manifest failed: {error}", variant.name));
            let lookup_table_accounts = vec![
                AddressLookupTableAccount {
                    key: Pubkey::new_unique(),
                    addresses: manifest
                        .shared_market()
                        .iter()
                        .map(|requirement| requirement.address)
                        .collect(),
                },
                AddressLookupTableAccount {
                    key: Pubkey::new_unique(),
                    addresses: manifest
                        .vault()
                        .iter()
                        .map(|requirement| requirement.address)
                        .collect(),
                },
            ];
            let message = v0::Message::try_compile(
                &payer,
                &instructions,
                &lookup_table_accounts,
                Hash::new_unique(),
            )
            .unwrap_or_else(|error| panic!("{} v0 compile failed: {error}", variant.name));
            let loaded_count = message
                .address_table_lookups
                .iter()
                .map(|lookup| lookup.writable_indexes.len() + lookup.readonly_indexes.len())
                .sum::<usize>();
            assert_eq!(
                loaded_count,
                manifest.lookup_eligible_addresses().len(),
                "{} did not load the complete typed manifest",
                variant.name
            );
            assert_eq!(
                message.address_table_lookups.len(),
                2,
                "{} did not contribute both shared and vault tables",
                variant.name
            );
            assert!(
                message.address_table_lookups.iter().all(|lookup| {
                    !lookup.writable_indexes.is_empty() || !lookup.readonly_indexes.is_empty()
                }),
                "{} retained a zero-contribution table",
                variant.name
            );
            assert!(
                message.account_keys.len() + loaded_count <= 256,
                "{} exceeds the v0 unique-key limit",
                variant.name
            );
            assert!(
                message.account_keys.contains(&payer),
                "{} lost payer",
                variant.name
            );
            assert!(
                message.account_keys.contains(&signer),
                "{} lost signer",
                variant.name
            );
            assert!(
                message.account_keys.contains(&KLEND_PROGRAM_ID),
                "{} moved invoked KLend program into an ALT",
                variant.name
            );
            if variant.collateral_farm {
                assert!(
                    message.account_keys.contains(&FARMS_PROGRAM_ID),
                    "{} moved invoked farms program into an ALT",
                    variant.name
                );
            }
            let transaction = VersionedTransaction::try_new(
                VersionedMessage::V0(message),
                &[&payer_keypair, &signer_keypair],
            )
            .unwrap_or_else(|error| panic!("{} signing failed: {error}", variant.name));
            let loaded = loaded_lookup_table_addresses(&transaction, &lookup_table_accounts);
            let required = manifest
                .lookup_eligible_addresses()
                .into_iter()
                .map(|address| address.to_string())
                .collect::<BTreeSet<_>>();
            assert_eq!(
                loaded, required,
                "{} exact ALT coverage drifted",
                variant.name
            );
            let packet = transaction_packet_summary(&transaction, &lookup_table_accounts)
                .unwrap_or_else(|error| {
                    panic!("{} packet measurement failed: {error}", variant.name)
                });
            assert!(
                packet.fits_packet_data_size && packet.packet_size_bytes < 1232,
                "{} packet is {} bytes",
                variant.name,
                packet.packet_size_bytes
            );
        }
    }

    #[test]
    fn reusable_alt_ordered_hash_detects_reordering() {
        let first = Pubkey::new_unique().to_string();
        let second = Pubkey::new_unique().to_string();
        let third = Pubkey::new_unique().to_string();

        let ordered_hash =
            ordered_lookup_table_address_hash(&[first.clone(), second.clone(), third.clone()]);
        let shuffled_hash = ordered_lookup_table_address_hash(&[third, first, second]);

        assert_ne!(ordered_hash, shuffled_hash);
    }

    #[test]
    fn lookup_table_missing_addresses_include_only_uncovered_required_keys() {
        let covered_first = Pubkey::new_unique();
        let covered_second = Pubkey::new_unique();
        let missing = Pubkey::new_unique();
        let lookup_table_accounts = vec![AddressLookupTableAccount {
            key: Pubkey::new_unique(),
            addresses: vec![covered_second, covered_first, Pubkey::new_unique()],
        }];

        let missing_addresses = missing_lookup_table_addresses(
            &[covered_first, missing, covered_second],
            &lookup_table_accounts,
        );

        assert_eq!(missing_addresses, vec![missing]);
    }

    #[test]
    fn collateral_conversion_can_produce_distinct_redeemable_liquidity() {
        let scale = BigUint::from(1_u128 << 60);
        let total_liquidity_scaled = BigUint::from(1_200_000_000_u64) * scale;

        let redeemable = collateral_to_redeemable_liquidity_amount(
            1_000_000_000,
            &total_liquidity_scaled,
            500_000_000,
        )
        .expect("conversion should fit");

        assert_eq!(redeemable, 600_000_000);
    }

    #[test]
    fn source_collateral_validation_rejects_route_liquidity_as_withdraw_amount() {
        let source = test_chain_position(404_323_479, 480_000_000);
        let input = test_same_mint_input(480_000_000, Some(480_000_000));

        let error = planned_source_collateral_amount(&input, &source).unwrap_err();

        assert!(
            error
                .to_string()
                .contains("does not match planned source_collateral_amount_raw"),
            "unexpected error: {error}"
        );
    }

    #[test]
    fn source_collateral_validation_accepts_distinct_collateral_and_liquidity() {
        let source = test_chain_position(404_323_479, 480_000_000);
        let input = test_same_mint_input(480_000_000, Some(404_323_479));

        let amount = planned_source_collateral_amount(&input, &source)
            .expect("matching source collateral should pass");

        assert_eq!(amount, 404_323_479);
    }

    #[test]
    fn monitor_expectations_accept_newer_execute_chain_snapshot() {
        let options = test_cli_options(true, true, Some(1));
        let mut input = test_same_mint_input(480_000_000, Some(404_323_479));
        input.expected_source_snapshot_id = SnapshotId(2);

        validate_monitor_expectations(&options, &input)
            .expect("execute plus chain reconcile should accept its fresh snapshot");
    }

    #[test]
    fn monitor_expectations_reject_snapshot_drift_without_chain_reconcile() {
        let options = test_cli_options(true, false, Some(1));
        let mut input = test_same_mint_input(480_000_000, Some(404_323_479));
        input.expected_source_snapshot_id = SnapshotId(2);

        let error = validate_monitor_expectations(&options, &input).unwrap_err();

        assert_eq!(
            blocker_reason(&error),
            json!({
                "kind": "monitor_plan_drift",
                "reason": "expected source snapshot 1, got 2",
            })
        );
    }
}

fn print_help() {
    println!(
         "Usage: same-mint-reserve-swap --settings <PUBKEY> --vault-index <N> --cluster <mainnet-beta|devnet|testnet|localnet> [--e2e-main-prime-main <AMOUNT_RAW>] [--update-policy] [--update-active-policy] [--deposit-main-usdc <AMOUNT_RAW> | --deposit-reserve <RESERVE> <AMOUNT_RAW> | --deposit-idle-vault-reserve <RESERVE> <AMOUNT_RAW>] [--setup-obligation-reserve <RESERVE>] [--full-withdraw-main-usdc | --full-withdraw-reserve <RESERVE>] [--direction main-to-prime|prime-to-main | --source-reserve <PUBKEY> --target-reserve <PUBKEY>] [--optimization-cycle] [--reconcile-from-chain] [--seed-from-user-position] [--rpc-url <URL>] [--execute | --prepare-only | --read-only]\n\n\
         Dry-run is the default, and still records lookup-table readiness and provisioning demand so the readiness wait loop can make progress; add --read-only to suppress every database write for pure inspection. Reads NEON_DATABASE_URL, optionally SOLANA_RPC_URL, and requires YIELD_ALT_CLUSTER or --cluster. E2E mode runs policy update, initial Main USDC deposit, Main -> Prime move, Prime -> Main move, and full Main withdrawal as child invocations of this same binary. Policy update mode uses SOLANA_TESTING_PK for the settings authority and POLICY_KEYPAIR as the delegated policy signer. By default --update-policy targets a fresh next policy seed; add --update-active-policy to intentionally update the currently active DB policy instead. Policy create/update, obligation setup, initial/idle policy deposits, same-mint moves, full withdrawal, wallet recovery, and policy cleanup all use the same Neon rollout, typed-manifest, readiness, usage-lease, fresh-RPC, exact-v0, and immediate pre-send resolver path. The wallet-to-vault funding transaction is deliberately ALT-free. Add --setup-obligation-reserve <reserve> as a setup/admin-only mode to execute the decoded target-market init_obligation constraint from the route or setup policy. Add --optimization-cycle for same-mint route work; it requires explicit source/target reserves plus --reconcile-from-chain and either --execute or --prepare-only. --prepare-only builds and simulates the exact route and persists reusable readiness/provisioning demand without creating a rebalance decision, acquiring a route lease, or sending a transaction. --execute uses POLICY_KEYPAIR as fee payer and delegated signer, requires reusable_only rollout state with force_legacy disabled, fresh-verifies every selected reusable ALT against RPC, compiles and simulates the exact v0 transaction, and fails before the decision or send when readiness or leases are invalid. Missing reusable coverage records an idempotent provisioning request for the dedicated provisioner; this route process never creates or extends ALTs. Legacy, shadow, prefer_reusable, and force_legacy control states fail closed because legacy ALT resolution has been removed. Add --deposit-idle-vault-reserve for router-owned USDC already inside the vault; execute mode requires expected idle token account, observed slot/time, mint, amount, target APY, and edge, uses POLICY_KEYPAIR as fee payer/delegated signer for target obligation setup when needed and for deposit, and does not read SOLANA_TESTING_PK. Initial deposit mode uses SOLANA_TESTING_PK as the funding wallet and POLICY_KEYPAIR for the policy deposit; --deposit-reserve allows choosing a non-Main Safe USDC reserve when Main is already the APY winner. Full withdraw mode uses POLICY_KEYPAIR for the policy withdraw, then SOLANA_TESTING_PK authority cleanup to recover vault USDC, close the route policy plus setup policy when present, and report rent cleanup proof. Run through:\n\
         op run --env-file=.env.1password -- bun run same-mint:swap -- --settings <PUBKEY> --vault-index 1 --reconcile-from-chain --seed-from-user-position"
    );
}
