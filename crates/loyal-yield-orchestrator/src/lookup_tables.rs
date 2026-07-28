use crate::{NeonSqlClient, OrchestratorError, VaultId, STANDARD_POLICY_AUTHORITY};
use chrono::{DateTime, Utc};
pub use loyal_actions::{
    compiler_lookup_eligible_addresses, LookupTableAccountAccess, LookupTableAccountProvenance,
    LookupTableManifest, LookupTableManifestError, MustRemainStatic, MustRemainStaticReason,
    SharedMarket, SharedMarketRole, Vault, VaultRole,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use solana_sdk::{
    address_lookup_table::{
        instruction::derive_lookup_table_address, program as address_lookup_table_program,
    },
    pubkey::Pubkey,
};
use sqlx::{Postgres, QueryBuilder, Row};
use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
    str::FromStr,
    sync::atomic::{AtomicU64, Ordering},
};
use thiserror::Error;

pub const LOOKUP_TABLE_HARD_CAPACITY: u16 = 256;
pub const SHARED_MARKET_LOGICAL_CATALOG_MAX_ADDRESSES: usize = 10_000;
const LOOKUP_TABLE_DB_CONCURRENCY_MAX_ATTEMPTS: usize = 3;
const LOOKUP_TABLE_DB_CONCURRENCY_RETRY_BASE_MILLIS: u64 = 50;
static LOOKUP_TABLE_ROLLOUT_LOCK_ACQUISITIONS: AtomicU64 = AtomicU64::new(0);

/// Process-local telemetry for proving that normal demand-driven readiness
/// does not acquire the cluster-wide rollout administration fence.
///
/// This counter intentionally measures successful lock acquisitions only. It
/// is not a distributed metric and must not be used to coordinate work.
pub fn lookup_table_rollout_lock_acquisition_count() -> u64 {
    LOOKUP_TABLE_ROLLOUT_LOCK_ACQUISITIONS.load(Ordering::Relaxed)
}

async fn acquire_lookup_table_rollout_lock(
    tx: &mut sqlx::Transaction<'_, Postgres>,
    cluster: &str,
) -> Result<(), sqlx::Error> {
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-rollout:' || $1, 0))")
        .bind(cluster)
        .execute(&mut **tx)
        .await?;
    LOOKUP_TABLE_ROLLOUT_LOCK_ACQUISITIONS.fetch_add(1, Ordering::Relaxed);
    Ok(())
}

async fn acquire_lookup_table_readiness_vault_lock(
    tx: &mut sqlx::Transaction<'_, Postgres>,
    cluster: &str,
    vault_id: VaultId,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-readiness:' || $1 || ':' || $2::TEXT, 0))",
    )
    .bind(cluster)
    .bind(vault_id.as_i64())
    .execute(&mut **tx)
    .await?;
    Ok(())
}

#[derive(Debug, Error, Clone, PartialEq, Eq)]
pub enum LookupTableDomainError {
    #[error("unknown {kind} value {value:?}")]
    UnknownEnumValue { kind: &'static str, value: String },
    #[error("invalid {kind} transition from {from} to {to}")]
    InvalidTransition {
        kind: &'static str,
        from: &'static str,
        to: &'static str,
    },
    #[error("lookup-table hard capacity must be between 1 and 256, got {0}")]
    InvalidHardCapacity(u16),
    #[error("lookup-table high-water mark must be positive and no greater than hard capacity")]
    InvalidHighWaterMark,
    #[error("lookup-table safety margin plus largest atomic expansion exhausts capacity")]
    InvalidCapacityReserve,
    #[error("vault manifest requires {required} distinct addresses, exceeding hard capacity {hard_capacity}")]
    ManifestExceedsHardCapacity { required: usize, hard_capacity: u16 },
    #[error("resolver received {actual} candidates, exceeding its exact-search limit {limit}")]
    TooManyResolverCandidates { actual: usize, limit: usize },
    #[error("lookup-table lease fencing token must be positive")]
    InvalidFencingToken,
    #[error("shared-market cohort {cohort_key:?} was supplied with conflicting address sets")]
    ConflictingSharedMarketCohort { cohort_key: String },
    #[error("shared-market catalog requires {actual} shards, exceeding INTEGER ordinals")]
    SharedMarketShardCountOverflow { actual: usize },
}

macro_rules! string_enum {
    (
        $(#[$meta:meta])*
        pub enum $name:ident, $kind:literal {
            $($variant:ident => $value:literal),+ $(,)?
        }
    ) => {
        $(#[$meta])*
        #[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize, Deserialize)]
        #[serde(rename_all = "snake_case")]
        pub enum $name {
            $($variant),+
        }

        impl $name {
            pub const fn as_str(self) -> &'static str {
                match self {
                    $(Self::$variant => $value),+
                }
            }
        }

        impl fmt::Display for $name {
            fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
                formatter.write_str(self.as_str())
            }
        }

        impl FromStr for $name {
            type Err = LookupTableDomainError;

            fn from_str(value: &str) -> Result<Self, Self::Err> {
                match value {
                    $($value => Ok(Self::$variant)),+,
                    _ => Err(LookupTableDomainError::UnknownEnumValue {
                        kind: $kind,
                        value: value.to_owned(),
                    }),
                }
            }
        }
    };
}

string_enum! {
    pub enum LookupTableFamilyKind, "lookup-table family kind" {
        SharedMarket => "shared_market",
        VaultShards => "vault_shards"
    }
}

string_enum! {
    pub enum LookupTableFamilyState, "lookup-table family state" {
        Active => "active",
        Paused => "paused",
        Retiring => "retiring",
        Retired => "retired"
    }
}

impl LookupTableFamilyState {
    pub fn can_transition_to(self, next: Self) -> bool {
        self == next
            || matches!(
                (self, next),
                (Self::Active, Self::Paused | Self::Retiring)
                    | (Self::Paused, Self::Active | Self::Retiring)
                    | (Self::Retiring, Self::Active | Self::Retired)
            )
    }

    pub fn transition_to(self, next: Self) -> Result<Self, LookupTableDomainError> {
        transition(self, next, "lookup-table family state")
    }
}

string_enum! {
    pub enum LookupTableAllocationKind, "lookup-table allocation kind" {
        SharedMarket => "shared_market",
        VaultShard => "vault_shard",
        DedicatedVault => "dedicated_vault"
    }
}

string_enum! {
    /// Structured classification for tables created before reusable families.
    /// These values must never be promoted into reusable allocation kinds.
    pub enum LegacyLookupTableKind, "legacy lookup-table kind" {
        LegacyRoute => "legacy_route",
        LegacyMixed => "legacy_mixed"
    }
}

string_enum! {
    pub enum LookupTableLifecycle, "lookup-table lifecycle" {
        Preparing => "preparing",
        Warming => "warming",
        Active => "active",
        Standby => "standby",
        Retiring => "retiring",
        Deactivated => "deactivated",
        Closed => "closed",
        Failed => "failed"
    }
}

impl LookupTableLifecycle {
    pub fn can_transition_to(self, next: Self) -> bool {
        self == next
            || matches!(
                (self, next),
                (Self::Preparing, Self::Warming | Self::Failed | Self::Closed)
                    | (Self::Warming, Self::Active | Self::Failed | Self::Retiring)
                    | (Self::Active, Self::Standby | Self::Retiring | Self::Failed)
                    | (Self::Standby, Self::Active | Self::Retiring | Self::Failed)
                    | (
                        Self::Retiring,
                        Self::Active | Self::Deactivated | Self::Failed
                    )
                    | (Self::Deactivated, Self::Closed | Self::Failed)
                    | (
                        Self::Failed,
                        Self::Preparing | Self::Warming | Self::Retiring | Self::Closed
                    )
            )
    }

    pub fn transition_to(self, next: Self) -> Result<Self, LookupTableDomainError> {
        transition(self, next, "lookup-table lifecycle")
    }

    pub const fn may_resolve(self) -> bool {
        matches!(self, Self::Active | Self::Standby)
    }
}

string_enum! {
    pub enum LookupTableAllocationAcceptance, "lookup-table allocation acceptance" {
        Accepting => "accepting",
        Sealed => "sealed",
        Paused => "paused"
    }
}

string_enum! {
    pub enum LookupTableManifestSubject, "lookup-table manifest subject" {
        SharedMarket => "shared_market",
        Vault => "vault"
    }
}

string_enum! {
    pub enum LookupTableBindingMode, "lookup-table binding mode" {
        PackedShard => "packed_shard",
        Dedicated => "dedicated"
    }
}

string_enum! {
    pub enum LookupTableBindingLifecycle, "lookup-table binding lifecycle" {
        Preparing => "preparing",
        Warming => "warming",
        Active => "active",
        Standby => "standby",
        Retiring => "retiring",
        Retired => "retired",
        Failed => "failed"
    }
}

impl LookupTableBindingLifecycle {
    pub fn can_transition_to(self, next: Self) -> bool {
        self == next
            || matches!(
                (self, next),
                (
                    Self::Preparing,
                    Self::Warming | Self::Failed | Self::Retired
                ) | (Self::Warming, Self::Active | Self::Failed | Self::Retiring)
                    | (Self::Active, Self::Standby | Self::Retiring | Self::Failed)
                    | (Self::Standby, Self::Active | Self::Retiring | Self::Failed)
                    | (Self::Retiring, Self::Active | Self::Retired | Self::Failed)
                    | (
                        Self::Failed,
                        Self::Preparing | Self::Retiring | Self::Retired
                    )
            )
    }

    pub fn transition_to(self, next: Self) -> Result<Self, LookupTableDomainError> {
        transition(self, next, "lookup-table binding lifecycle")
    }

    pub const fn may_resolve(self) -> bool {
        matches!(self, Self::Active | Self::Standby)
    }
}

string_enum! {
    pub enum LookupTableOperationKind, "lookup-table operation kind" {
        Create => "create",
        Extend => "extend",
        Verify => "verify",
        Rollover => "rollover",
        Deactivate => "deactivate",
        Close => "close"
    }
}

string_enum! {
    pub enum LookupTableOperationStatus, "lookup-table operation status" {
        Queued => "queued",
        Leased => "leased",
        Signed => "signed",
        Submitted => "submitted",
        Confirmed => "confirmed",
        Finalized => "finalized",
        Reconciled => "reconciled",
        Complete => "complete",
        RetryWait => "retry_wait",
        NeedsReconcile => "needs_reconcile",
        PermanentFailure => "permanent_failure",
        Cancelled => "cancelled"
    }
}

string_enum! {
    pub enum LegacyLookupTableCleanupAttemptState, "legacy lookup-table cleanup attempt state" {
        Prepared => "prepared",
        Signed => "signed",
        Submitted => "submitted",
        NeedsReconcile => "needs_reconcile",
        Expired => "expired",
        Complete => "complete",
        PermanentFailure => "permanent_failure"
    }
}

impl LookupTableOperationStatus {
    pub fn can_transition_to(self, next: Self) -> bool {
        self == next
            || matches!(
                (self, next),
                (Self::Queued, Self::Leased | Self::Cancelled)
                    | (
                        Self::Leased,
                        Self::Signed
                            | Self::RetryWait
                            | Self::NeedsReconcile
                            | Self::PermanentFailure
                            | Self::Cancelled
                    )
                    | (
                        Self::Signed,
                        Self::Submitted
                            | Self::RetryWait
                            | Self::NeedsReconcile
                            | Self::PermanentFailure
                            | Self::Cancelled
                    )
                    | (
                        Self::Submitted,
                        Self::Confirmed
                            | Self::RetryWait
                            | Self::NeedsReconcile
                            | Self::PermanentFailure
                    )
                    | (
                        Self::Confirmed,
                        Self::Finalized | Self::NeedsReconcile | Self::PermanentFailure
                    )
                    | (
                        Self::Finalized,
                        Self::Reconciled | Self::NeedsReconcile | Self::PermanentFailure
                    )
                    | (
                        Self::Reconciled,
                        Self::Complete | Self::NeedsReconcile | Self::PermanentFailure
                    )
                    | (
                        Self::RetryWait,
                        Self::Queued
                            | Self::Leased
                            | Self::NeedsReconcile
                            | Self::PermanentFailure
                            | Self::Cancelled
                    )
                    | (
                        Self::NeedsReconcile,
                        Self::Queued
                            | Self::Leased
                            | Self::Confirmed
                            | Self::Finalized
                            | Self::Reconciled
                            | Self::Complete
                            | Self::RetryWait
                            | Self::PermanentFailure
                            | Self::Cancelled
                    )
            )
    }

    pub fn transition_to(self, next: Self) -> Result<Self, LookupTableDomainError> {
        transition(self, next, "lookup-table operation status")
    }

    pub const fn is_terminal(self) -> bool {
        matches!(
            self,
            Self::Complete | Self::PermanentFailure | Self::Cancelled
        )
    }
}

string_enum! {
    pub enum LookupTableRolloutMode, "lookup-table rollout mode" {
        Legacy => "legacy",
        Shadow => "shadow",
        PreferReusable => "prefer_reusable",
        ReusableOnly => "reusable_only"
    }
}

string_enum! {
    pub enum LookupTableReadinessStatus, "lookup-table readiness status" {
        Unknown => "unknown",
        Incomplete => "incomplete",
        Ready => "ready",
        Failed => "failed"
    }
}

string_enum! {
    pub enum LookupTableSelectionKind, "lookup-table selection kind" {
        Legacy => "legacy",
        Reusable => "reusable",
        Blocked => "blocked"
    }
}

string_enum! {
    pub enum LookupTableSimulationState, "lookup-table simulation state" {
        NotRun => "not_run",
        Succeeded => "succeeded",
        Failed => "failed"
    }
}

string_enum! {
    pub enum LookupTableUsageLeaseKind, "lookup-table usage lease kind" {
        RouteResolution => "route_resolution",
        PreparedTransaction => "prepared_transaction"
    }
}

string_enum! {
    pub enum LookupTableProvisioningRequestStatus, "lookup-table provisioning request status" {
        Requested => "requested",
        Planning => "planning",
        Queued => "queued",
        Satisfied => "satisfied",
        Failed => "failed",
        Cancelled => "cancelled"
    }
}

string_enum! {
    pub enum SharedMarketCatalogReadiness, "shared-market catalog readiness" {
        Pending => "pending",
        Provisioning => "provisioning",
        Active => "active",
        Failed => "failed"
    }
}

string_enum! {
pub enum SharedMarketCatalogRouteValidationState, "shared-market catalog route validation state" {
        Covered => "covered",
        MissingHead => "missing_head",
        Drift => "drift"
    }
}

string_enum! {
    pub enum SharedMarketPhysicalDriftResolution, "shared-market physical drift resolution" {
        Open => "open",
        Resolved => "resolved"
    }
}

impl LookupTableProvisioningRequestStatus {
    pub fn can_transition_to(self, next: Self) -> bool {
        self == next
            || matches!(
                (self, next),
                (Self::Requested, Self::Planning | Self::Cancelled)
                    | (
                        Self::Planning,
                        Self::Queued | Self::Failed | Self::Requested | Self::Cancelled
                    )
                    | (
                        Self::Queued,
                        Self::Planning | Self::Satisfied | Self::Failed | Self::Cancelled
                    )
                    | (
                        Self::Failed,
                        Self::Requested | Self::Planning | Self::Cancelled
                    )
                    | (Self::Satisfied, Self::Requested)
            )
    }
}

fn transition<T>(current: T, next: T, kind: &'static str) -> Result<T, LookupTableDomainError>
where
    T: Copy + fmt::Display + TransitionState,
{
    if current.can_transition_to(next) {
        Ok(next)
    } else {
        Err(LookupTableDomainError::InvalidTransition {
            kind,
            from: current.as_static_str(),
            to: next.as_static_str(),
        })
    }
}

trait TransitionState: Copy {
    fn can_transition_to(self, next: Self) -> bool;
    fn as_static_str(self) -> &'static str;
}

macro_rules! transition_state {
    ($type:ty) => {
        impl TransitionState for $type {
            fn can_transition_to(self, next: Self) -> bool {
                <$type>::can_transition_to(self, next)
            }

            fn as_static_str(self) -> &'static str {
                self.as_str()
            }
        }
    };
}

transition_state!(LookupTableFamilyState);
transition_state!(LookupTableLifecycle);
transition_state!(LookupTableBindingLifecycle);
transition_state!(LookupTableOperationStatus);

pub fn lookup_table_manifest_hash(manifest: &LookupTableManifest) -> String {
    hash_manifest_input(&manifest.canonical_hash_input())
}

pub fn shared_market_manifest_hash(manifest: &LookupTableManifest) -> String {
    hash_manifest_input(&manifest.shared_market_hash_input())
}

pub fn vault_manifest_hash(manifest: &LookupTableManifest) -> String {
    hash_manifest_input(&manifest.vault_hash_input())
}

fn hash_manifest_input(input: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(input);
    format!("{:x}", hasher.finalize())
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SharedMarketRouteCohort {
    pub cohort_key: String,
    pub addresses: BTreeSet<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SharedMarketShardPlan {
    pub shard_ordinal: i32,
    pub addresses: Vec<String>,
}

/// Packs the authoritative append-stable catalog order into deterministic
/// physical shards. Existing full shard prefixes never move when a later
/// catalog revision only appends addresses; only the final shard can extend
/// before a new ordinal is allocated.
pub fn append_pack_shared_market_shards(
    ordered_addresses: &[String],
    shard_capacity: u16,
) -> Result<Vec<SharedMarketShardPlan>, LookupTableDomainError> {
    if shard_capacity == 0 || shard_capacity > LOOKUP_TABLE_HARD_CAPACITY {
        return Err(LookupTableDomainError::InvalidHardCapacity(shard_capacity));
    }
    let shard_count = ordered_addresses
        .len()
        .div_ceil(usize::from(shard_capacity));
    if shard_count > i32::MAX as usize {
        return Err(LookupTableDomainError::SharedMarketShardCountOverflow {
            actual: shard_count,
        });
    }
    Ok(ordered_addresses
        .chunks(usize::from(shard_capacity))
        .enumerate()
        .map(|(shard_ordinal, addresses)| SharedMarketShardPlan {
            shard_ordinal: shard_ordinal as i32,
            addresses: addresses.to_vec(),
        })
        .collect())
}

/// Deterministically clusters frequently co-occurring shared accounts without
/// ever truncating the universe. A route may reference more than one shard.
pub fn plan_shared_market_shards(
    cohorts: &[SharedMarketRouteCohort],
    shard_capacity: u16,
) -> Result<Vec<SharedMarketShardPlan>, LookupTableDomainError> {
    if shard_capacity == 0 || shard_capacity > LOOKUP_TABLE_HARD_CAPACITY {
        return Err(LookupTableDomainError::InvalidHardCapacity(shard_capacity));
    }
    let mut canonical_cohorts = BTreeMap::<String, BTreeSet<String>>::new();
    for cohort in cohorts {
        match canonical_cohorts.get(&cohort.cohort_key) {
            Some(existing) if existing != &cohort.addresses => {
                return Err(LookupTableDomainError::ConflictingSharedMarketCohort {
                    cohort_key: cohort.cohort_key.clone(),
                });
            }
            Some(_) => {}
            None => {
                canonical_cohorts.insert(cohort.cohort_key.clone(), cohort.addresses.clone());
            }
        }
    }

    let universe = canonical_cohorts
        .values()
        .flat_map(|addresses| addresses.iter().cloned())
        .collect::<BTreeSet<_>>();
    let mut pair_weights = BTreeMap::<(String, String), usize>::new();
    let mut weighted_degree = universe
        .iter()
        .cloned()
        .map(|address| (address, 0usize))
        .collect::<BTreeMap<_, _>>();
    for addresses in canonical_cohorts.values() {
        let addresses = addresses.iter().collect::<Vec<_>>();
        for left_index in 0..addresses.len() {
            for right_index in (left_index + 1)..addresses.len() {
                let left = addresses[left_index];
                let right = addresses[right_index];
                *pair_weights
                    .entry((left.clone(), right.clone()))
                    .or_default() += 1;
                *weighted_degree.entry(left.clone()).or_default() += 1;
                *weighted_degree.entry(right.clone()).or_default() += 1;
            }
        }
    }

    let mut unassigned = universe;
    let mut shards = Vec::new();
    while !unassigned.is_empty() {
        let seed = unassigned
            .iter()
            .max_by(|left, right| {
                weighted_degree[*left]
                    .cmp(&weighted_degree[*right])
                    .then_with(|| right.cmp(left))
            })
            .expect("nonempty set has a seed")
            .clone();
        unassigned.remove(&seed);
        let mut shard = vec![seed];
        while shard.len() < usize::from(shard_capacity) && !unassigned.is_empty() {
            let next = unassigned
                .iter()
                .max_by(|left, right| {
                    shared_market_connection_weight(left, &shard, &pair_weights)
                        .cmp(&shared_market_connection_weight(
                            right,
                            &shard,
                            &pair_weights,
                        ))
                        .then_with(|| weighted_degree[*left].cmp(&weighted_degree[*right]))
                        .then_with(|| right.cmp(left))
                })
                .expect("nonempty set has a next account")
                .clone();
            unassigned.remove(&next);
            shard.push(next);
        }
        shard.sort();
        shards.push(SharedMarketShardPlan {
            shard_ordinal: shards.len() as i32,
            addresses: shard,
        });
    }
    Ok(shards)
}

fn shared_market_connection_weight(
    address: &str,
    shard: &[String],
    pair_weights: &BTreeMap<(String, String), usize>,
) -> usize {
    shard
        .iter()
        .map(|member| {
            let pair = if address < member.as_str() {
                (address.to_owned(), member.clone())
            } else {
                (member.clone(), address.to_owned())
            };
            pair_weights.get(&pair).copied().unwrap_or_default()
        })
        .sum()
}

fn next_shared_market_mutation(
    table_exists: bool,
    desired: &[String],
    confirmed: &[String],
    pending: &[String],
    max_extension_addresses: usize,
) -> Option<(LookupTableOperationKind, Vec<String>)> {
    if !pending.is_empty() {
        return None;
    }
    if !ordered_prefix_matches(confirmed, desired) {
        return None;
    }
    let missing = desired
        .iter()
        .skip(confirmed.len())
        .take(max_extension_addresses)
        .cloned()
        .collect::<Vec<_>>();
    if missing.is_empty() {
        return None;
    }
    Some((
        if table_exists {
            LookupTableOperationKind::Extend
        } else {
            LookupTableOperationKind::Create
        },
        missing,
    ))
}

fn ordered_prefix_matches(prefix: &[String], desired: &[String]) -> bool {
    prefix.len() <= desired.len()
        && prefix
            .iter()
            .zip(desired.iter())
            .all(|(actual, expected)| actual == expected)
}

fn ordered_confirmed_and_pending_match(
    confirmed: &[String],
    pending: &[String],
    desired: &[String],
) -> bool {
    if confirmed.len().saturating_add(pending.len()) > desired.len() {
        return false;
    }
    confirmed
        .iter()
        .chain(pending.iter())
        .zip(desired.iter())
        .all(|(actual, expected)| actual == expected)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct PackedShardPolicy {
    pub hard_capacity: u16,
    pub largest_atomic_expansion: u16,
    pub safety_margin: u16,
    pub per_vault_growth_reservation: u16,
    pub max_vault_cohort: u16,
}

impl PackedShardPolicy {
    pub fn validate(self) -> Result<Self, LookupTableDomainError> {
        if self.hard_capacity == 0 || self.hard_capacity > LOOKUP_TABLE_HARD_CAPACITY {
            return Err(LookupTableDomainError::InvalidHardCapacity(
                self.hard_capacity,
            ));
        }
        if self.max_vault_cohort == 0 {
            return Err(LookupTableDomainError::InvalidHighWaterMark);
        }
        if self.largest_atomic_expansion == 0
            || self.safety_margin == 0
            || self
                .largest_atomic_expansion
                .saturating_add(self.safety_margin)
                >= self.hard_capacity
        {
            return Err(LookupTableDomainError::InvalidCapacityReserve);
        }
        Ok(self)
    }

    pub fn high_water_mark(self) -> Result<u16, LookupTableDomainError> {
        self.validate()?;
        Ok(self
            .hard_capacity
            .saturating_sub(self.largest_atomic_expansion)
            .saturating_sub(self.safety_margin))
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PackedShardCandidate {
    pub table_id: i64,
    pub family_id: i64,
    pub generation: i32,
    pub shard_index: i32,
    pub confirmed_addresses: BTreeSet<String>,
    pub pending_addresses: BTreeSet<String>,
    /// Sum of complete-manifest-plus-growth promises for live bindings.
    pub reserved_address_count: u16,
    pub allocation_high_water: u16,
    pub bound_vault_count: u16,
    pub acceptance: LookupTableAllocationAcceptance,
    pub lifecycle: LookupTableLifecycle,
}

impl PackedShardCandidate {
    pub fn occupied_distinct_count(&self) -> usize {
        self.confirmed_addresses
            .union(&self.pending_addresses)
            .count()
    }

    pub fn contains_manifest(&self, addresses: &BTreeSet<String>) -> bool {
        addresses
            .iter()
            .all(|address| self.confirmed_addresses.contains(address))
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PackedVaultAllocationRequest {
    pub vault_id: VaultId,
    pub manifest_id: i64,
    pub desired_addresses: BTreeSet<String>,
    pub current_table_id: Option<i64>,
    pub current_reserved_capacity: Option<u16>,
    pub next_generation: i32,
    pub next_shard_index: i32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PackedVaultAllocation {
    KeepExisting {
        table_id: i64,
    },
    ReserveExistingShard {
        table_id: i64,
        family_id: i64,
        missing_addresses: Vec<String>,
        reserved_capacity: u16,
        reservation_delta: u16,
        projected_occupied: u16,
        projected_capacity_commitment: u16,
    },
    PrepareNewShard {
        generation: i32,
        shard_index: i32,
        desired_addresses: Vec<String>,
        reserved_capacity: u16,
        allocation_high_water: u16,
        dedicated: bool,
    },
}

#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord)]
struct PackedShardScore {
    new_address_count: usize,
    projected_residual_headroom: u16,
    bound_vault_count: u16,
    generation: i32,
    shard_index: i32,
    table_id: i64,
}

pub fn allocate_packed_vault_manifest(
    request: &PackedVaultAllocationRequest,
    candidates: &[PackedShardCandidate],
    policy: PackedShardPolicy,
) -> Result<PackedVaultAllocation, LookupTableDomainError> {
    let policy = policy.validate()?;
    let high_water = policy.high_water_mark()?;
    if request.desired_addresses.len() > usize::from(policy.hard_capacity) {
        return Err(LookupTableDomainError::ManifestExceedsHardCapacity {
            required: request.desired_addresses.len(),
            hard_capacity: policy.hard_capacity,
        });
    }
    let required_reserved_capacity = request
        .desired_addresses
        .len()
        .saturating_add(usize::from(policy.per_vault_growth_reservation));
    if required_reserved_capacity > usize::from(policy.hard_capacity) {
        return Err(LookupTableDomainError::ManifestExceedsHardCapacity {
            required: required_reserved_capacity,
            hard_capacity: policy.hard_capacity,
        });
    }

    if let Some(current_table_id) = request.current_table_id {
        if let Some(current) = candidates.iter().find(|table| {
            table.table_id == current_table_id
                && table.lifecycle.may_resolve()
                && table.contains_manifest(&request.desired_addresses)
                && usize::from(request.current_reserved_capacity.unwrap_or_default())
                    >= required_reserved_capacity
        }) {
            return Ok(PackedVaultAllocation::KeepExisting {
                table_id: current.table_id,
            });
        }
    }

    let mut eligible = candidates
        .iter()
        .filter_map(|candidate| {
            let already_bound = request.current_table_id == Some(candidate.table_id);
            let projected_bound_vault_count = candidate
                .bound_vault_count
                .checked_add(u16::from(!already_bound))?;
            if candidate.acceptance != LookupTableAllocationAcceptance::Accepting
                || !matches!(
                    candidate.lifecycle,
                    LookupTableLifecycle::Preparing
                        | LookupTableLifecycle::Warming
                        | LookupTableLifecycle::Active
                )
                || projected_bound_vault_count > policy.max_vault_cohort
            {
                return None;
            }

            let occupied = candidate
                .confirmed_addresses
                .union(&candidate.pending_addresses)
                .cloned()
                .collect::<BTreeSet<_>>();
            let missing_addresses = request
                .desired_addresses
                .difference(&occupied)
                .cloned()
                .collect::<Vec<_>>();
            let projected_occupied = occupied.len().checked_add(missing_addresses.len())?;
            let reserved_capacity = required_reserved_capacity;
            let prior_binding_reservation = if already_bound {
                usize::from(request.current_reserved_capacity.unwrap_or_default())
            } else {
                0
            };
            let reservation_delta = reserved_capacity.saturating_sub(prior_binding_reservation);
            let projected_reservation_floor =
                usize::from(candidate.reserved_address_count).checked_add(reservation_delta)?;
            let projected_commitment = projected_occupied.max(projected_reservation_floor);
            let candidate_high_water = high_water.min(candidate.allocation_high_water);
            if projected_occupied > usize::from(policy.hard_capacity)
                || projected_commitment > usize::from(candidate_high_water)
            {
                return None;
            }

            let projected_occupied = u16::try_from(projected_occupied).ok()?;
            let projected_capacity_commitment = u16::try_from(projected_commitment).ok()?;
            let projected_residual_headroom =
                candidate_high_water.checked_sub(projected_capacity_commitment)?;
            let reserved_capacity = u16::try_from(reserved_capacity).ok()?;
            let reservation_delta = u16::try_from(reservation_delta).ok()?;
            Some((
                PackedShardScore {
                    new_address_count: missing_addresses.len(),
                    projected_residual_headroom,
                    bound_vault_count: projected_bound_vault_count,
                    generation: candidate.generation,
                    shard_index: candidate.shard_index,
                    table_id: candidate.table_id,
                },
                candidate,
                missing_addresses,
                reserved_capacity,
                reservation_delta,
                projected_occupied,
                projected_capacity_commitment,
            ))
        })
        .collect::<Vec<_>>();
    eligible.sort_by(|left, right| left.0.cmp(&right.0));

    if let Some((
        _,
        candidate,
        missing_addresses,
        reserved_capacity,
        reservation_delta,
        projected_occupied,
        projected_commitment,
    )) = eligible.into_iter().next()
    {
        return Ok(PackedVaultAllocation::ReserveExistingShard {
            table_id: candidate.table_id,
            family_id: candidate.family_id,
            missing_addresses,
            reserved_capacity,
            reservation_delta,
            projected_occupied,
            projected_capacity_commitment: projected_commitment,
        });
    }

    let new_shard_commitment = required_reserved_capacity;
    let dedicated = new_shard_commitment > usize::from(high_water);
    if new_shard_commitment > usize::from(policy.hard_capacity) {
        return Err(LookupTableDomainError::ManifestExceedsHardCapacity {
            required: new_shard_commitment,
            hard_capacity: policy.hard_capacity,
        });
    }
    Ok(PackedVaultAllocation::PrepareNewShard {
        generation: request.next_generation,
        shard_index: request.next_shard_index,
        desired_addresses: request.desired_addresses.iter().cloned().collect(),
        reserved_capacity: u16::try_from(new_shard_commitment).map_err(|_| {
            LookupTableDomainError::ManifestExceedsHardCapacity {
                required: new_shard_commitment,
                hard_capacity: policy.hard_capacity,
            }
        })?,
        allocation_high_water: if dedicated {
            policy.hard_capacity
        } else {
            high_water
        },
        dedicated,
    })
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LookupTableOperationLease {
    pub owner: String,
    pub fencing_token: i64,
    pub leased_until: DateTime<Utc>,
}

impl LookupTableOperationLease {
    pub fn new(
        owner: impl Into<String>,
        fencing_token: i64,
        leased_until: DateTime<Utc>,
    ) -> Result<Self, LookupTableDomainError> {
        if fencing_token <= 0 {
            return Err(LookupTableDomainError::InvalidFencingToken);
        }
        Ok(Self {
            owner: owner.into(),
            fencing_token,
            leased_until,
        })
    }

    pub fn authorizes(&self, owner: &str, fencing_token: i64, now: DateTime<Utc>) -> bool {
        self.owner == owner && self.fencing_token == fencing_token && now < self.leased_until
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LookupTableOperationIntent {
    pub cluster: String,
    pub family_id: i64,
    pub table_id: Option<i64>,
    pub kind: LookupTableOperationKind,
    pub generation: i32,
    pub shard_index: i32,
    pub mutation_epoch: i64,
    pub desired_address_hash: String,
    pub addresses: Vec<String>,
}

impl LookupTableOperationIntent {
    pub fn idempotency_key(&self) -> String {
        operation_idempotency_key(self)
    }
}

pub fn operation_idempotency_key(intent: &LookupTableOperationIntent) -> String {
    let mut addresses = intent.addresses.clone();
    addresses.sort();
    addresses.dedup();
    let mut hasher = Sha256::new();
    for value in [
        intent.cluster.as_str(),
        &intent.family_id.to_string(),
        &intent.table_id.unwrap_or_default().to_string(),
        intent.kind.as_str(),
        &intent.generation.to_string(),
        &intent.shard_index.to_string(),
        &intent.mutation_epoch.to_string(),
        intent.desired_address_hash.as_str(),
    ] {
        hasher.update((value.len() as u64).to_le_bytes());
        hasher.update(value.as_bytes());
    }
    for address in addresses {
        hasher.update((address.len() as u64).to_le_bytes());
        hasher.update(address.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

fn terminal_operation_successor_idempotency_key(
    predecessor_idempotency_key: &str,
    predecessor_id: i64,
    attempt_generation: i64,
) -> String {
    let mut hasher = Sha256::new();
    for value in [
        "loyal-reusable-alt-terminal-successor",
        predecessor_idempotency_key,
        &predecessor_id.to_string(),
        &attempt_generation.to_string(),
    ] {
        hasher.update((value.len() as u64).to_le_bytes());
        hasher.update(value.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

string_enum! {
    pub enum LookupTableSignatureState, "lookup-table signature state" {
        Unknown => "unknown",
        NotFound => "not_found",
        Processed => "processed",
        Confirmed => "confirmed",
        Finalized => "finalized",
        Failed => "failed"
    }
}

string_enum! {
    pub enum LookupTableChainState, "lookup-table chain state" {
        Missing => "missing",
        PrefixMatches => "prefix_matches",
        ExactMatch => "exact_match",
        AuthorityDrift => "authority_drift",
        PrefixDrift => "prefix_drift",
        LifecycleDrift => "lifecycle_drift"
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableReconciliationObservation {
    pub operation_kind: LookupTableOperationKind,
    pub persisted_status: LookupTableOperationStatus,
    pub signature_state: LookupTableSignatureState,
    pub chain_state: LookupTableChainState,
    /// True only when the physical ALT state was loaded at finalized commitment.
    pub chain_observed_finalized: bool,
    pub blockhash_expired: bool,
    pub usable_after_slot_reached: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum LookupTableReconciliationDecision {
    WaitForSignature,
    WaitForFinalization,
    WaitForUsableSlot,
    AdvanceTo(LookupTableOperationStatus),
    MarkCompleteFromChain,
    RetryWithFreshTransaction,
    NeedsManualReconcile { reason: &'static str },
    PermanentFailure { reason: &'static str },
}

pub fn reconcile_lookup_table_operation(
    observation: &LookupTableReconciliationObservation,
) -> LookupTableReconciliationDecision {
    use LookupTableChainState as Chain;
    use LookupTableOperationStatus as Status;
    use LookupTableReconciliationDecision as Decision;
    use LookupTableSignatureState as Signature;

    if observation.signature_state == Signature::Finalized && !observation.chain_observed_finalized
    {
        return Decision::WaitForFinalization;
    }

    if matches!(
        observation.chain_state,
        Chain::AuthorityDrift | Chain::PrefixDrift | Chain::LifecycleDrift
    ) {
        return Decision::NeedsManualReconcile {
            reason:
                "on-chain lookup table does not match its durable authority, prefix, or lifecycle",
        };
    }
    if observation.signature_state == Signature::Failed {
        return Decision::PermanentFailure {
            reason: "the persisted lookup-table transaction failed on chain",
        };
    }

    let mutation_is_present = match observation.operation_kind {
        LookupTableOperationKind::Create => {
            matches!(
                observation.chain_state,
                Chain::PrefixMatches | Chain::ExactMatch
            )
        }
        LookupTableOperationKind::Extend | LookupTableOperationKind::Rollover => {
            matches!(
                observation.chain_state,
                Chain::PrefixMatches | Chain::ExactMatch
            )
        }
        LookupTableOperationKind::Verify => observation.chain_state == Chain::ExactMatch,
        LookupTableOperationKind::Deactivate | LookupTableOperationKind::Close => {
            observation.chain_state == Chain::ExactMatch
        }
    };

    if mutation_is_present {
        if observation.operation_kind != LookupTableOperationKind::Verify {
            match observation.signature_state {
                Signature::Unknown => {
                    return Decision::NeedsManualReconcile {
                        reason: "unsigned lookup-table mutation appeared on chain outside the durable transaction boundary",
                    }
                }
                Signature::Processed | Signature::Confirmed => {
                    return Decision::WaitForFinalization;
                }
                Signature::NotFound => {
                    return Decision::NeedsManualReconcile {
                        reason: "lookup-table mutation exists but its persisted signature was not found, so spend cannot be attributed safely",
                    }
                }
                Signature::Finalized => {}
                Signature::Failed => unreachable!("failed signatures return above"),
            }
        }
        if !observation.chain_observed_finalized {
            return Decision::WaitForFinalization;
        }
        if matches!(
            observation.operation_kind,
            LookupTableOperationKind::Create
                | LookupTableOperationKind::Extend
                | LookupTableOperationKind::Rollover
        ) && !observation.usable_after_slot_reached
        {
            return Decision::WaitForUsableSlot;
        }
        return if matches!(
            observation.persisted_status,
            Status::Reconciled | Status::Complete
        ) {
            Decision::MarkCompleteFromChain
        } else {
            Decision::AdvanceTo(Status::Reconciled)
        };
    }

    // Verify is a read-only reconciliation operation: it never has a signed
    // transaction whose signature could later appear. A missing table must
    // therefore surface as drift instead of waiting forever for a signature
    // that cannot exist.
    if observation.operation_kind == LookupTableOperationKind::Verify {
        return Decision::NeedsManualReconcile {
            reason: "lookup-table verification did not find the expected finalized table state",
        };
    }

    match observation.signature_state {
        Signature::Processed | Signature::Confirmed => Decision::WaitForFinalization,
        Signature::Unknown => Decision::WaitForSignature,
        Signature::NotFound if !observation.blockhash_expired => Decision::WaitForSignature,
        Signature::NotFound => Decision::RetryWithFreshTransaction,
        Signature::Finalized => Decision::NeedsManualReconcile {
            reason: "transaction finalized but expected lookup-table state is absent",
        },
        Signature::Failed => unreachable!("failed signatures return above"),
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolverTableCandidate {
    pub table_id: i64,
    pub table_address: String,
    pub expected_authority: String,
    pub family_id: Option<i64>,
    pub allocation_kind: Option<LookupTableAllocationKind>,
    pub generation: i32,
    pub shard_index: i32,
    pub ordered_usable_prefix: Vec<String>,
    /// Exact durable full membership followed by the exact append-only suffix
    /// from nonterminal create/extend operations. Runtime RPC verification may
    /// accept either the persisted full membership or this anticipated full
    /// membership, but compilation uses only `ordered_usable_prefix`.
    pub ordered_durable_addresses: Vec<String>,
    pub addresses: BTreeSet<String>,
    pub usable_prefix_len: u16,
    pub address_hash: String,
    pub mutation_epoch: i64,
    pub last_verified_slot: Option<i64>,
    pub lifecycle: LookupTableLifecycle,
    /// The normalized DB prefix matched the last persisted verification.
    pub persisted_prefix_verified: bool,
    /// Set only after a fresh finalized/confirmed RPC reload at execution time.
    pub rpc_verified: bool,
    pub usable: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedLookupTableBundle {
    pub tables: Vec<ResolverTableCandidate>,
    pub required_addresses: BTreeSet<String>,
    pub missing_addresses: BTreeSet<String>,
    pub packet_fits: bool,
    pub simulation_succeeded: bool,
}

impl ResolvedLookupTableBundle {
    pub fn ready(&self) -> bool {
        self.missing_addresses.is_empty()
            && self.packet_fits
            && self.simulation_succeeded
            && self
                .tables
                .iter()
                .all(|table| table.rpc_verified && table.usable && table.lifecycle.may_resolve())
    }
}

pub fn minimal_verified_table_bundle(
    required_addresses: &BTreeSet<String>,
    candidates: &[ResolverTableCandidate],
    exact_search_limit: usize,
) -> Result<(Vec<ResolverTableCandidate>, BTreeSet<String>), LookupTableDomainError> {
    let (candidates, missing) =
        persisted_relevant_table_candidates(required_addresses, candidates, exact_search_limit)?;

    // Shared-market shards are an exact, disjoint partition of one logical
    // catalog. Every shard that intersects this route is therefore a mandatory
    // contributor, not an exponential subset-search candidate. Keep the
    // bounded exact search only for potentially overlapping vault candidates.
    let (mandatory_shared, optional): (Vec<_>, Vec<_>) =
        candidates.into_iter().partition(|candidate| {
            candidate.allocation_kind == Some(LookupTableAllocationKind::SharedMarket)
        });
    let mandatory_covered = mandatory_shared
        .iter()
        .flat_map(|candidate| candidate.addresses.iter().cloned())
        .collect::<BTreeSet<_>>();
    let remaining_required = required_addresses
        .difference(&mandatory_covered)
        .cloned()
        .collect::<BTreeSet<_>>();
    if remaining_required.is_empty() {
        let mut selected = mandatory_shared;
        selected.sort_by(resolver_candidate_identity_order);
        return Ok((selected, BTreeSet::new()));
    }

    let mut best: Option<(Vec<usize>, usize)> = None;
    let mut selected = Vec::new();
    search_table_subsets(0, &optional, &remaining_required, &mut selected, &mut best);
    if let Some((indexes, _)) = best {
        let mut selected = mandatory_shared;
        selected.extend(indexes.into_iter().map(|index| optional[index].clone()));
        selected.sort_by(resolver_candidate_identity_order);
        return Ok((selected, BTreeSet::new()));
    }

    Ok((Vec::new(), missing))
}

fn resolver_candidate_identity_order(
    left: &ResolverTableCandidate,
    right: &ResolverTableCandidate,
) -> std::cmp::Ordering {
    left.table_address
        .cmp(&right.table_address)
        .then_with(|| left.table_id.cmp(&right.table_id))
}

/// Returns every persisted-eligible candidate that can contribute to this
/// route. Runtime must RPC-verify this set before exact minimization;
/// preselecting a single persisted bundle would make one drifted overlap hide a
/// healthy alternative. Only non-shared candidates consume the exponential
/// exact-search bound; disjoint shared shards are mandatory contributors.
pub fn persisted_relevant_table_candidates(
    required_addresses: &BTreeSet<String>,
    candidates: &[ResolverTableCandidate],
    exact_search_limit: usize,
) -> Result<(Vec<ResolverTableCandidate>, BTreeSet<String>), LookupTableDomainError> {
    let mut candidates = candidates
        .iter()
        .filter(|candidate| {
            candidate.persisted_prefix_verified
                && candidate.usable
                && candidate.lifecycle.may_resolve()
                && candidate
                    .addresses
                    .intersection(required_addresses)
                    .next()
                    .is_some()
        })
        .cloned()
        .collect::<Vec<_>>();
    candidates.sort_by(resolver_candidate_identity_order);
    let exact_candidate_count = candidates
        .iter()
        .filter(|candidate| {
            candidate.allocation_kind != Some(LookupTableAllocationKind::SharedMarket)
        })
        .count();
    if exact_candidate_count > exact_search_limit {
        return Err(LookupTableDomainError::TooManyResolverCandidates {
            actual: exact_candidate_count,
            limit: exact_search_limit,
        });
    }

    let covered = candidates
        .iter()
        .flat_map(|candidate| candidate.addresses.iter().cloned())
        .collect::<BTreeSet<_>>();
    let missing = required_addresses
        .difference(&covered)
        .cloned()
        .collect::<BTreeSet<_>>();
    Ok((candidates, missing))
}

fn search_table_subsets(
    next_index: usize,
    candidates: &[ResolverTableCandidate],
    required_addresses: &BTreeSet<String>,
    selected: &mut Vec<usize>,
    best: &mut Option<(Vec<usize>, usize)>,
) {
    if let Some((best_indexes, _)) = best {
        if selected.len() >= best_indexes.len() {
            return;
        }
    }
    let covered = selected
        .iter()
        .flat_map(|index| candidates[*index].addresses.iter().cloned())
        .collect::<BTreeSet<_>>();
    if required_addresses.is_subset(&covered) {
        let total_table_entries = selected
            .iter()
            .map(|index| candidates[*index].addresses.len())
            .sum();
        let replace = best.as_ref().is_none_or(|(best_indexes, best_entries)| {
            selected.len() < best_indexes.len()
                || (selected.len() == best_indexes.len()
                    && (total_table_entries < *best_entries
                        || (total_table_entries == *best_entries
                            && selected.as_slice() < best_indexes.as_slice())))
        });
        if replace {
            *best = Some((selected.clone(), total_table_entries));
        }
        return;
    }
    if next_index >= candidates.len() {
        return;
    }

    selected.push(next_index);
    search_table_subsets(
        next_index + 1,
        candidates,
        required_addresses,
        selected,
        best,
    );
    selected.pop();
    search_table_subsets(
        next_index + 1,
        candidates,
        required_addresses,
        selected,
        best,
    );
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableFamilyRecord {
    pub id: i64,
    pub cluster: String,
    pub logical_name: String,
    pub kind: LookupTableFamilyKind,
    pub desired_state: LookupTableFamilyState,
    pub planner_version: String,
    pub catalog_version: String,
    pub active_generation: Option<i32>,
    pub previous_generation: Option<i32>,
    pub rollback_until: Option<DateTime<Utc>>,
    pub provisioning_authority: String,
    pub payer: String,
    pub hard_capacity: i32,
    pub largest_atomic_expansion: i32,
    pub safety_margin: i32,
    pub allocation_high_water: i32,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableFamilyUpsert {
    pub cluster: String,
    pub logical_name: String,
    pub kind: LookupTableFamilyKind,
    pub desired_state: LookupTableFamilyState,
    pub planner_version: String,
    pub catalog_version: String,
    pub active_generation: Option<i32>,
    pub previous_generation: Option<i32>,
    pub rollback_until: Option<DateTime<Utc>>,
    pub provisioning_authority: String,
    pub payer: String,
    pub hard_capacity: i32,
    pub largest_atomic_expansion: i32,
    pub safety_margin: i32,
    pub allocation_high_water: i32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReusableLookupTableRecord {
    pub id: i64,
    pub cluster: String,
    pub scope: String,
    pub table_address: String,
    pub authority: String,
    pub payer: String,
    pub legacy_status: String,
    pub address_count: i32,
    pub address_hash: String,
    pub family_id: i64,
    pub allocation_kind: LookupTableAllocationKind,
    pub generation: i32,
    pub shard_ordinal: i32,
    pub desired_state: LookupTableLifecycle,
    pub accepting_allocations: bool,
    pub allocation_high_water: i32,
    pub reserved_address_count: i32,
    pub usable_address_count: i32,
    pub last_extended_start_index: Option<i32>,
    pub last_verified_slot: Option<i64>,
    pub last_verified_at: Option<DateTime<Utc>>,
    pub mutation_epoch: i64,
    pub rollback_until: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReusableLookupTableInsert {
    pub cluster: String,
    pub scope: String,
    pub table_address: String,
    pub authority: String,
    pub payer: String,
    pub family_id: i64,
    pub allocation_kind: LookupTableAllocationKind,
    pub generation: i32,
    pub shard_ordinal: i32,
    pub desired_state: LookupTableLifecycle,
    pub accepting_allocations: bool,
    pub allocation_high_water: i32,
    pub mutation_epoch: i64,
    pub create_signature: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableManifestAddressRecord {
    pub address: String,
    pub ordinal: i32,
    pub semantic_class: LookupTableManifestSubject,
    /// Stable comma-separated canonical role set.
    pub account_role: String,
    pub is_writable: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableManifestWrite {
    pub family_id: i64,
    pub subject_kind: LookupTableManifestSubject,
    pub subject_key: String,
    pub vault_id: Option<VaultId>,
    pub desired_set_hash: String,
    pub source_slot: Option<i64>,
    pub planner_version: String,
    pub catalog_version: String,
    pub addresses: Vec<LookupTableManifestAddressRecord>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableManifestRecord {
    pub id: i64,
    pub family_id: i64,
    pub subject_kind: LookupTableManifestSubject,
    pub subject_key: String,
    pub vault_id: Option<VaultId>,
    pub desired_set_hash: String,
    pub address_count: i32,
    pub source_slot: Option<i64>,
    pub planner_version: String,
    pub catalog_version: String,
    pub sealed_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    pub addresses: Vec<LookupTableManifestAddressRecord>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct SharedMarketCatalogUpsert {
    pub cluster: String,
    pub catalog_version: String,
    pub desired_set_hash: String,
    pub enabled_mints_hash: String,
    pub reserve_set_hash: String,
    pub addresses: Vec<LookupTableManifestAddressRecord>,
    pub source_slot: Option<i64>,
    pub source_observed_at: Option<DateTime<Utc>>,
    pub source_metadata: Value,
    pub reason: String,
    pub updated_by: String,
}

#[derive(Debug, Clone, PartialEq)]
pub struct SharedMarketCatalogHeadRecord {
    pub family_id: i64,
    pub catalog_revision_id: i64,
    pub catalog_revision: i64,
    pub manifest_id: i64,
    pub cluster: String,
    pub catalog_version: String,
    pub desired_set_hash: String,
    pub enabled_mints_hash: String,
    pub reserve_set_hash: String,
    pub address_count: i32,
    pub source_slot: Option<i64>,
    pub source_observed_at: Option<DateTime<Utc>>,
    pub source_metadata: Value,
    pub reason: String,
    pub updated_by: String,
    pub active_generation: Option<i32>,
    pub target_generation: Option<i32>,
    pub readiness_state: SharedMarketCatalogReadiness,
    pub activated_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub addresses: Vec<LookupTableManifestAddressRecord>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SharedMarketCatalogRouteValidation {
    pub state: SharedMarketCatalogRouteValidationState,
    pub catalog_revision_id: Option<i64>,
    pub catalog_revision: Option<i64>,
    pub desired_set_hash: Option<String>,
    pub readiness_state: Option<SharedMarketCatalogReadiness>,
    pub target_generation: Option<i32>,
    pub active_generation: Option<i32>,
    pub route_missing_addresses: Vec<String>,
    pub semantic_mismatch_addresses: Vec<String>,
    pub active_missing_addresses: Vec<String>,
    pub active_extra_addresses: Vec<String>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct SharedMarketCatalogPlanPolicy {
    pub shared_shard_capacity: u16,
    pub max_extension_addresses: usize,
    pub operation_context: Value,
    pub estimated_fee_lamports: Option<i64>,
    pub estimated_rent_lamports: Option<i64>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct SharedMarketCatalogPlan {
    pub catalog: SharedMarketCatalogHeadRecord,
    pub shared_target_generation: i32,
    pub shared_operations: Vec<LookupTableOperationRecord>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SharedMarketPhysicalDriftReport {
    pub cluster: String,
    pub catalog_revision_id: i64,
    pub family_id: i64,
    pub route_lookup_table_id: i64,
    pub expected_mutation_epoch: i64,
    pub expected_table_address: String,
    pub expected_authority: String,
    pub observed_slot: i64,
    pub observed_table_present: bool,
    pub observed_authority: Option<String>,
    pub observed_active: bool,
    pub observed_last_extended_slot: Option<i64>,
    pub observed_warm: bool,
    /// Exact finalized on-chain order. Empty only when the table is absent.
    pub observed_addresses: Vec<String>,
    pub reason: String,
    pub reported_by: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SharedMarketPhysicalDriftRecord {
    pub id: i64,
    pub evidence_hash: String,
    pub cluster: String,
    pub family_id: i64,
    pub catalog_revision_id: i64,
    pub route_lookup_table_id: i64,
    pub expected_mutation_epoch: i64,
    pub expected_table_address: String,
    pub expected_authority: String,
    pub observed_slot: i64,
    pub observed_table_present: bool,
    pub observed_authority: Option<String>,
    pub observed_active: bool,
    pub observed_last_extended_slot: Option<i64>,
    pub observed_warm: bool,
    pub observed_address_hash: String,
    pub observed_addresses: Vec<String>,
    pub reason: String,
    pub reported_by: String,
    pub resolution_state: SharedMarketPhysicalDriftResolution,
    pub resolution_target_generation: Option<i32>,
    pub resolved_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
}

/// Signerless production-connected exercise of the real reusable-ALT drift
/// and demand-request store paths. The implementation rolls every exercised
/// control-plane mutation back and persists only an immutable PASS audit row.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTablePrecutoverProbe {
    pub probe_token: String,
    /// Exact durable paused-control epoch observed before finalized RPC and
    /// rechecked under a row lock by the rollback-only database exercise.
    pub provisioner_control_epoch: i64,
    /// Exact finalized on-chain bundle after owner/authority/lifecycle/warmth
    /// validation by the provisioner.
    pub finalized_observation: FinalizedSharedTableObservation,
    /// Deliberately mismatched observation passed through the production drift
    /// reporter inside the rollback-only transaction.
    pub drift_report: SharedMarketPhysicalDriftReport,
    /// Typed missing-vault request passed twice through the production request
    /// upsert inside that same transaction.
    pub provisioning_request: LookupTableProvisioningRequestUpsert,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LookupTablePrecutoverProbeRecord {
    pub id: i64,
    pub probe_token: String,
    pub cluster: String,
    pub vault_id: VaultId,
    pub catalog_revision_id: i64,
    pub shared_manifest_id: i64,
    pub route_lookup_table_id: i64,
    pub shared_table_address: String,
    pub shared_authority: String,
    pub shared_mutation_epoch: i64,
    pub provisioner_control_epoch: i64,
    pub requirements_fingerprint: String,
    pub finalized_slot: i64,
    pub finalized_last_extended_slot: i64,
    pub finalized_address_hash: String,
    pub finalized_address_count: i32,
    pub shared_table_bundle_hash: String,
    pub shared_table_count: i32,
    pub finalized_bundle_address_count: i32,
    pub shared_tables: Vec<LookupTablePrecutoverProbeSharedTableRecord>,
    pub finalized_shared_exact: bool,
    pub synthetic_drift_evidence_hash: String,
    pub drift_signal_count: i32,
    pub drift_provisioning_request_count: i32,
    pub duplicate_request_attempt_count: i32,
    pub distinct_request_count: i32,
    pub decision_count: i32,
    pub binding_count: i32,
    pub operation_count: i32,
    pub rollback_residue_count: i32,
    pub catalog_head_restored: bool,
    pub signer_loaded: bool,
    pub transactions_sent: bool,
    pub result: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LookupTablePrecutoverProbeSharedTableRecord {
    pub probe_run_id: i64,
    pub shard_ordinal: i32,
    pub route_lookup_table_id: i64,
    pub shared_table_address: String,
    pub shared_authority: String,
    pub shared_mutation_epoch: i64,
    pub finalized_slot: i64,
    pub finalized_last_extended_slot: i64,
    pub finalized_address_hash: String,
    pub finalized_address_count: i32,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LookupTableClusterBudgetPolicy {
    pub max_lamports: i64,
    pub rolling_window_seconds: i64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LookupTableClusterBudgetReservation {
    pub approved: bool,
    pub replayed: bool,
    pub reservation_id: Option<i64>,
    pub cluster: String,
    pub operation_id: i64,
    pub fencing_token: i64,
    pub estimated_fee_lamports: i64,
    pub estimated_rent_lamports: i64,
    pub requested_lamports: i64,
    pub spent_lamports: i64,
    pub reserved_lamports: i64,
    pub charged_lamports: i64,
    pub remaining_lamports: i64,
    pub window_ends_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LegacyLookupTableCleanupBudgetReservation {
    pub approved: bool,
    pub replayed: bool,
    pub reservation_id: Option<i64>,
    pub cluster: String,
    pub legacy_cleanup_attempt_id: i64,
    pub estimated_fee_lamports: i64,
    pub estimated_rent_lamports: i64,
    pub requested_lamports: i64,
    pub spent_lamports: i64,
    pub reserved_lamports: i64,
    pub charged_lamports: i64,
    pub remaining_lamports: i64,
    pub window_ends_at: DateTime<Utc>,
}

/// Exact DB evidence that a caller must verify against finalized RPC before
/// reusable-only cutover. Passing this value back fences every mutable field
/// used to authorize the cutover.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ReusableOnlyCutoverPreflight {
    pub cluster: String,
    pub catalog_revision_id: i64,
    pub catalog_revision: i64,
    pub manifest_id: i64,
    pub manifest_hash: String,
    pub ordered_address_hash: String,
    pub ordered_addresses: Vec<String>,
    pub shared_family_id: i64,
    pub active_generation: i32,
    pub target_generation: i32,
    pub shared_table_bundle_hash: String,
    pub shared_tables: Vec<ReusableOnlyCutoverSharedTable>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ReusableOnlyCutoverSharedTable {
    pub table_id: i64,
    pub shard_ordinal: i32,
    pub table_address: String,
    pub authority: String,
    pub mutation_epoch: i64,
    pub last_extended_slot: i64,
    pub last_verified_slot: i64,
    pub ordered_address_hash: String,
    pub address_count: i32,
    pub usable_address_count: i32,
    pub ordered_addresses: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct FinalizedSharedTableShardObservation {
    pub table_id: i64,
    pub shard_ordinal: i32,
    pub table_address: String,
    pub authority: String,
    pub mutation_epoch: i64,
    pub last_extended_slot: i64,
    pub ordered_address_hash: String,
    pub address_count: i32,
    pub ordered_addresses: Vec<String>,
}

/// Exact finalized RPC evidence for the active shared physical bundle. The
/// caller obtains this only after checking every shard's genesis, owner,
/// authority, lifecycle, warmth, and ordered membership. Cutover rechecks the
/// complete ordered bundle against the concurrently locked database preflight.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct FinalizedSharedTableObservation {
    pub cluster: String,
    pub observed_slot: i64,
    pub shared_table_bundle_hash: String,
    pub shared_tables: Vec<FinalizedSharedTableShardObservation>,
}

fn shared_table_bundle_hash_from_parts(
    tables: impl IntoIterator<Item = (i64, i32, String, String, i64, i64, String, i32)>,
) -> String {
    let mut parts = vec!["loyal-reusable-shared-table-bundle-v1".to_owned()];
    for (
        table_id,
        shard_ordinal,
        table_address,
        authority,
        mutation_epoch,
        last_extended_slot,
        ordered_address_hash,
        address_count,
    ) in tables
    {
        parts.extend([
            table_id.to_string(),
            shard_ordinal.to_string(),
            table_address,
            authority,
            mutation_epoch.to_string(),
            last_extended_slot.to_string(),
            ordered_address_hash,
            address_count.to_string(),
        ]);
    }
    hash_length_prefixed_values(parts.iter().map(String::as_str))
}

pub fn reusable_only_cutover_shared_table_bundle_hash(
    tables: &[ReusableOnlyCutoverSharedTable],
) -> String {
    shared_table_bundle_hash_from_parts(tables.iter().map(|table| {
        (
            table.table_id,
            table.shard_ordinal,
            table.table_address.clone(),
            table.authority.clone(),
            table.mutation_epoch,
            table.last_extended_slot,
            table.ordered_address_hash.clone(),
            table.address_count,
        )
    }))
}

pub fn finalized_shared_table_bundle_hash(
    tables: &[FinalizedSharedTableShardObservation],
) -> String {
    shared_table_bundle_hash_from_parts(tables.iter().map(|table| {
        (
            table.table_id,
            table.shard_ordinal,
            table.table_address.clone(),
            table.authority.clone(),
            table.mutation_epoch,
            table.last_extended_slot,
            table.ordered_address_hash.clone(),
            table.address_count,
        )
    }))
}

fn validate_finalized_shared_table_observation(
    observation: &FinalizedSharedTableObservation,
) -> Result<Vec<String>, OrchestratorError> {
    if observation.cluster.trim().is_empty()
        || observation.observed_slot < 0
        || observation.shared_tables.is_empty()
        || !is_sha256_hex(&observation.shared_table_bundle_hash)
        || finalized_shared_table_bundle_hash(&observation.shared_tables)
            != observation.shared_table_bundle_hash
    {
        return Err(OrchestratorError::StoreInvariant(
            "finalized shared-table bundle identity is malformed".to_owned(),
        ));
    }
    let mut table_ids = BTreeSet::new();
    let mut table_addresses = BTreeSet::new();
    let mut shared_addresses = BTreeSet::new();
    let mut flattened = Vec::new();
    for (ordinal, table) in observation.shared_tables.iter().enumerate() {
        if table.table_id <= 0
            || table.shard_ordinal != i32::try_from(ordinal).unwrap_or(-1)
            || table.mutation_epoch < 0
            || table.last_extended_slot < 0
            || table.last_extended_slot >= observation.observed_slot
            || table.address_count <= 0
            || table.address_count > i32::from(LOOKUP_TABLE_HARD_CAPACITY)
            || usize::try_from(table.address_count).ok() != Some(table.ordered_addresses.len())
            || !is_sha256_hex(&table.ordered_address_hash)
            || ordered_address_hash(&table.ordered_addresses) != table.ordered_address_hash
            || Pubkey::from_str(&table.table_address).is_err()
            || Pubkey::from_str(&table.authority).is_err()
            || !table_ids.insert(table.table_id)
            || !table_addresses.insert(table.table_address.as_str())
            || table.ordered_addresses.iter().any(|address| {
                Pubkey::from_str(address).is_err() || !shared_addresses.insert(address.as_str())
            })
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "finalized shared-table shard {ordinal} is malformed, duplicated, or not warm"
            )));
        }
        flattened.extend(table.ordered_addresses.iter().cloned());
    }
    Ok(flattened)
}

fn validate_finalized_shared_tables_against_preflight(
    preflight: &ReusableOnlyCutoverPreflight,
    observation: &FinalizedSharedTableObservation,
) -> Result<Vec<String>, OrchestratorError> {
    let flattened = validate_finalized_shared_table_observation(observation)?;
    let tables_match = preflight.shared_tables.len() == observation.shared_tables.len()
        && preflight
            .shared_tables
            .iter()
            .zip(&observation.shared_tables)
            .all(|(expected, observed)| {
                expected.table_id == observed.table_id
                    && expected.shard_ordinal == observed.shard_ordinal
                    && expected.table_address == observed.table_address
                    && expected.authority == observed.authority
                    && expected.mutation_epoch == observed.mutation_epoch
                    && expected.last_extended_slot == observed.last_extended_slot
                    && expected.last_verified_slot <= observation.observed_slot
                    && expected.ordered_address_hash == observed.ordered_address_hash
                    && expected.address_count == observed.address_count
                    && expected.usable_address_count == observed.address_count
                    && expected.ordered_addresses == observed.ordered_addresses
            });
    if preflight.cluster != observation.cluster
        || preflight.shared_table_bundle_hash != observation.shared_table_bundle_hash
        || preflight.ordered_address_hash != ordered_address_hash(&flattened)
        || preflight.ordered_addresses != flattened
        || !tables_match
    {
        return Err(OrchestratorError::StoreInvariant(
            "finalized shared-table bundle does not match the locked reusable-only preflight"
                .to_owned(),
        ));
    }
    Ok(flattened)
}

pub fn shared_market_manifest_addresses(
    manifest: &LookupTableManifest,
) -> Vec<LookupTableManifestAddressRecord> {
    let mut rows = manifest
        .shared_market()
        .iter()
        .map(|account| {
            let account_role = account
                .roles
                .iter()
                .map(|role| role.as_str())
                .collect::<Vec<_>>()
                .join(",");
            (account.address.to_string(), account.access, account_role)
        })
        .collect::<Vec<_>>();
    rows.sort_by(|left, right| left.0.cmp(&right.0));
    rows.into_iter()
        .enumerate()
        .map(
            |(ordinal, (address, access, account_role))| LookupTableManifestAddressRecord {
                address,
                ordinal: ordinal as i32,
                semantic_class: LookupTableManifestSubject::SharedMarket,
                account_role,
                is_writable: access == LookupTableAccountAccess::Writable,
            },
        )
        .collect()
}

pub fn vault_manifest_addresses(
    manifest: &LookupTableManifest,
) -> Vec<LookupTableManifestAddressRecord> {
    let mut rows = manifest
        .vault()
        .iter()
        .map(|account| {
            let account_role = account
                .roles
                .iter()
                .map(|role| role.as_str())
                .collect::<Vec<_>>()
                .join(",");
            (account.address.to_string(), account.access, account_role)
        })
        .collect::<Vec<_>>();
    rows.sort_by(|left, right| left.0.cmp(&right.0));
    rows.into_iter()
        .enumerate()
        .map(
            |(ordinal, (address, access, account_role))| LookupTableManifestAddressRecord {
                address,
                ordinal: ordinal as i32,
                semantic_class: LookupTableManifestSubject::Vault,
                account_role,
                is_writable: access == LookupTableAccountAccess::Writable,
            },
        )
        .collect()
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableVaultBindingRecord {
    pub id: i64,
    pub vault_id: VaultId,
    pub family_id: i64,
    pub route_lookup_table_id: i64,
    pub manifest_id: i64,
    pub binding_ordinal: i32,
    pub desired_head_revision: i64,
    pub allocation_mode: LookupTableBindingMode,
    pub reserved_capacity: i32,
    pub predecessor_binding_id: Option<i64>,
    pub lifecycle_state: LookupTableBindingLifecycle,
    pub active_from_slot: Option<i64>,
    pub active_until_slot: Option<i64>,
    pub activated_at: Option<DateTime<Utc>>,
    pub deactivated_at: Option<DateTime<Utc>>,
    pub rollback_until: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableVaultBindingInsert {
    pub vault_id: VaultId,
    pub family_id: i64,
    pub route_lookup_table_id: i64,
    pub manifest_id: i64,
    pub binding_ordinal: i32,
    pub allocation_mode: LookupTableBindingMode,
    pub reserved_capacity: i32,
    pub predecessor_binding_id: Option<i64>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct LookupTableOperationEnqueue {
    pub idempotency_key: String,
    pub family_id: i64,
    pub route_lookup_table_id: Option<i64>,
    pub manifest_id: Option<i64>,
    pub binding_id: Option<i64>,
    pub operation_kind: LookupTableOperationKind,
    pub target_generation: Option<i32>,
    pub target_shard_ordinal: Option<i32>,
    pub operation_context: Value,
    pub mutation_epoch: i64,
    pub estimated_fee_lamports: Option<i64>,
    pub estimated_rent_lamports: Option<i64>,
    pub addresses: Vec<String>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct LookupTableOperationRecord {
    pub id: i64,
    pub idempotency_key: String,
    pub family_id: i64,
    pub route_lookup_table_id: Option<i64>,
    pub manifest_id: Option<i64>,
    pub binding_id: Option<i64>,
    pub operation_kind: LookupTableOperationKind,
    pub operation_state: LookupTableOperationStatus,
    pub target_generation: Option<i32>,
    pub target_shard_ordinal: Option<i32>,
    pub operation_context: Value,
    pub mutation_epoch: i64,
    pub lease_owner: Option<String>,
    pub lease_expires_at: Option<DateTime<Utc>>,
    pub fencing_token: i64,
    pub transaction_signature: Option<String>,
    pub message_hash: Option<String>,
    pub recent_blockhash: Option<String>,
    pub last_valid_block_height: Option<i64>,
    pub attempt_count: i32,
    pub next_attempt_at: Option<DateTime<Utc>>,
    pub error_code: Option<String>,
    pub error_detail: Option<String>,
    pub submitted_slot: Option<i64>,
    pub confirmed_slot: Option<i64>,
    pub finalized_slot: Option<i64>,
    pub reconciled_slot: Option<i64>,
    pub estimated_fee_lamports: Option<i64>,
    pub estimated_rent_lamports: Option<i64>,
    pub actual_fee_lamports: Option<i64>,
    pub actual_rent_lamports: Option<i64>,
    pub reclaimed_rent_lamports: Option<i64>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LookupTableTerminalAccountState {
    Missing,
    NonLookupTable,
    ActiveLookupTable,
}

impl LookupTableTerminalAccountState {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Missing => "missing",
            Self::NonLookupTable => "non_lookup_table",
            Self::ActiveLookupTable => "active_lookup_table",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum LookupTableTerminalNoEffectEvidence {
    Unsigned,
    FinalizedFailedSignature {
        transaction_signature: String,
        failed_slot: i64,
    },
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableTerminalChainEvidence {
    pub observed_slot: i64,
    pub account_state: LookupTableTerminalAccountState,
    pub account_owner: Option<String>,
    pub authority: Option<String>,
    pub last_extended_slot: Option<i64>,
    pub ordered_addresses: Vec<String>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct LookupTableTerminalRepairCandidate {
    pub operation: LookupTableOperationRecord,
    pub operation_addresses: Vec<String>,
    pub unresolved_terminal_siblings: Vec<LookupTableOperationRecord>,
    pub physical_table: ReusableLookupTableRecord,
    pub persisted_membership: Vec<LookupTableMembershipAddress>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableTerminalSiblingEvidence {
    pub operation_id: i64,
    pub no_effect: LookupTableTerminalNoEffectEvidence,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableTerminalRepairRequest {
    pub cluster: String,
    pub operation_id: i64,
    pub expected_control_epoch: i64,
    pub expected_policy_authority: String,
    pub chain: LookupTableTerminalChainEvidence,
    pub no_effect: LookupTableTerminalNoEffectEvidence,
    pub sibling_no_effect: Vec<LookupTableTerminalSiblingEvidence>,
    pub reason: String,
    pub updated_by: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct LookupTableTerminalRepairResult {
    pub repair_id: i64,
    pub repair_kind: String,
    pub root_operation_id: i64,
    pub route_lookup_table_id: i64,
    pub successor_operation_id: Option<i64>,
    pub superseded_operation_ids: Vec<i64>,
    pub failed_binding_ids: Vec<i64>,
    pub requeued_request_ids: Vec<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct LookupTableTerminalNoEffectAudit {
    evidence: &'static str,
    signature: Option<String>,
    signature_slot: Option<i64>,
}

fn validate_lookup_table_terminal_no_effect(
    operation: &LookupTableOperationRecord,
    evidence: &LookupTableTerminalNoEffectEvidence,
    finalized_observed_slot: i64,
) -> Result<LookupTableTerminalNoEffectAudit, OrchestratorError> {
    match evidence {
        LookupTableTerminalNoEffectEvidence::Unsigned => {
            if operation.transaction_signature.is_some()
                || operation.message_hash.is_some()
                || operation.recent_blockhash.is_some()
                || operation.last_valid_block_height.is_some()
            {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "unsigned terminal ALT repair evidence conflicts with signed operation {}",
                    operation.id
                )));
            }
            Ok(LookupTableTerminalNoEffectAudit {
                evidence: "unsigned",
                signature: None,
                signature_slot: None,
            })
        }
        LookupTableTerminalNoEffectEvidence::FinalizedFailedSignature {
            transaction_signature,
            failed_slot,
        } => {
            if *failed_slot < 0
                || *failed_slot > finalized_observed_slot
                || operation.transaction_signature.as_deref()
                    != Some(transaction_signature.as_str())
            {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "failed-signature ALT repair evidence conflicts with durable operation {} or finalized slot",
                    operation.id
                )));
            }
            Ok(LookupTableTerminalNoEffectAudit {
                evidence: "finalized_failed_signature",
                signature: Some(transaction_signature.clone()),
                signature_slot: Some(*failed_slot),
            })
        }
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct LeasedLookupTableOperation {
    pub operation: LookupTableOperationRecord,
    pub addresses: Vec<String>,
    pub physical_table: Option<ReusableLookupTableRecord>,
    pub persisted_membership: Vec<LookupTableMembershipAddress>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SignedLookupTableTransaction {
    pub transaction_signature: String,
    pub message_hash: String,
    pub recent_blockhash: String,
    pub last_valid_block_height: i64,
    pub estimated_fee_lamports: i64,
    pub estimated_rent_lamports: i64,
    pub estimated_reclaimed_rent_lamports: i64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableActualAccounting {
    pub actual_fee_lamports: i64,
    pub actual_rent_lamports: i64,
    pub reclaimed_rent_lamports: i64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableOperationAdvance {
    pub expected_state: LookupTableOperationStatus,
    pub next_state: LookupTableOperationStatus,
    pub observed_slot: Option<i64>,
    pub error_code: Option<String>,
    pub error_detail: Option<String>,
    pub actual_fee_lamports: Option<i64>,
    pub actual_rent_lamports: Option<i64>,
    pub reclaimed_rent_lamports: Option<i64>,
}

/// Returns the deterministic spend/reclaim amounts persisted with the signed
/// transaction. Callers may promote them to actual accounting only after
/// finalized signature evidence and an exact expected chain effect.
pub fn persisted_lookup_table_success_accounting(
    operation: &LookupTableOperationRecord,
) -> Result<LookupTableActualAccounting, OrchestratorError> {
    if operation.transaction_signature.is_none()
        || operation.message_hash.is_none()
        || operation.recent_blockhash.is_none()
        || operation.last_valid_block_height.is_none()
    {
        return Err(OrchestratorError::StoreInvariant(format!(
            "lookup-table operation {} has no durable signed identity",
            operation.id
        )));
    }
    let actual_fee_lamports = operation.estimated_fee_lamports.ok_or_else(|| {
        OrchestratorError::StoreInvariant(format!(
            "lookup-table operation {} lacks a persisted fee estimate",
            operation.id
        ))
    })?;
    let actual_rent_lamports = operation.estimated_rent_lamports.ok_or_else(|| {
        OrchestratorError::StoreInvariant(format!(
            "lookup-table operation {} lacks a persisted rent estimate",
            operation.id
        ))
    })?;
    let reclaimed_rent_lamports = operation
        .operation_context
        .get("signedExpectedReclaimedRentLamports")
        .and_then(Value::as_i64)
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "lookup-table operation {} lacks a persisted reclaim estimate",
                operation.id
            ))
        })?;
    if actual_fee_lamports < 0 || actual_rent_lamports < 0 || reclaimed_rent_lamports < 0 {
        return Err(OrchestratorError::StoreInvariant(format!(
            "lookup-table operation {} has invalid persisted accounting",
            operation.id
        )));
    }
    Ok(LookupTableActualAccounting {
        actual_fee_lamports,
        actual_rent_lamports,
        reclaimed_rent_lamports,
    })
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableMembershipAddress {
    pub address: String,
    pub ordinal: i32,
    pub added_operation_id: Option<i64>,
    pub added_slot: i64,
    pub usable_after_slot: i64,
    pub last_verified_slot: i64,
    pub last_verified_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReusableLookupTableResolution {
    pub tables: Vec<ResolverTableCandidate>,
    pub required_addresses: BTreeSet<String>,
    pub missing_addresses: BTreeSet<String>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct LookupTableReadinessRecord {
    pub cluster: String,
    pub vault_id: VaultId,
    pub route_fingerprint: String,
    pub requirements_fingerprint: String,
    pub route_kind: String,
    pub source_reserve: Option<String>,
    pub target_reserve: Option<String>,
    pub manifest_id: Option<i64>,
    pub shared_family_id: Option<i64>,
    pub vault_binding_id: Option<i64>,
    pub readiness_state: LookupTableReadinessStatus,
    pub required_address_count: i32,
    pub covered_address_count: i32,
    pub missing_addresses: Value,
    pub legacy_table_ids: Vec<i64>,
    pub reusable_table_ids: Vec<i64>,
    pub compiled_message_size: Option<i32>,
    pub packet_limit: Option<i32>,
    pub observed_slot: Option<i64>,
    pub observed_at: DateTime<Utc>,
    pub selection_kind: Option<LookupTableSelectionKind>,
    pub fallback_reason: Option<String>,
    pub rollout_mode: Option<LookupTableRolloutMode>,
    pub selected_table_ids: Vec<i64>,
    pub selected_table_count: Option<i32>,
    pub packet_fits: Option<bool>,
    pub simulation_state: Option<LookupTableSimulationState>,
    pub simulation_units_consumed: Option<i64>,
    pub simulation_error: Option<String>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableRolloutControl {
    pub id: i64,
    pub cluster: String,
    pub vault_id: Option<VaultId>,
    pub rollout_mode: LookupTableRolloutMode,
    pub force_legacy: bool,
    pub reason: Option<String>,
    pub updated_by: String,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LookupTableProvisionerControlRecord {
    pub cluster: String,
    pub paused: bool,
    pub reason: String,
    pub updated_by: String,
    pub control_epoch: i64,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LookupTableProvisionerBroadcastPermitRecord {
    pub id: i64,
    pub cluster: String,
    pub operation_id: i64,
    pub fencing_token: i64,
    pub control_epoch: i64,
    pub transaction_signature: String,
    pub message_hash: String,
    pub permit_state: String,
    pub resolution_detail: Option<String>,
    pub granted_at: DateTime<Utc>,
    pub resolved_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

/// Result of the short, durable handoff immediately before broadcast. A grant
/// is committed before the caller performs network I/O. A pause result retains
/// the signed identity for reconciliation and never authorizes a send.
#[derive(Debug)]
pub enum LookupTableProvisionerBroadcastPermitResult {
    Paused {
        control: LookupTableProvisionerControlRecord,
        operation: LookupTableOperationRecord,
    },
    Fenced {
        control: LookupTableProvisionerControlRecord,
        operation: LookupTableOperationRecord,
        error_code: String,
        error_detail: String,
    },
    Granted {
        control: LookupTableProvisionerControlRecord,
        operation: LookupTableOperationRecord,
        permit: LookupTableProvisionerBroadcastPermitRecord,
    },
}

#[derive(Debug)]
pub enum LookupTableSharedMarketOperationFenceResult {
    Current,
    Cancelled {
        operation: LookupTableOperationRecord,
        reason: String,
    },
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum LookupTableProvisionerBroadcastResolution {
    Submitted {
        observed_slot: i64,
    },
    NeedsReconcile {
        observed_slot: Option<i64>,
        error_code: String,
        error_detail: String,
    },
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EffectiveLookupTableRollout {
    pub rollout_mode: LookupTableRolloutMode,
    pub force_legacy: bool,
    pub global: Option<LookupTableRolloutControl>,
    pub vault: Option<LookupTableRolloutControl>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReusableOnlyCutoverResult {
    pub cluster: String,
    pub catalog_revision_id: i64,
    pub shared_family_id: i64,
    pub shared_generation: i32,
    pub vault_family_id: i64,
    pub aligned_vault_control_count: i64,
    pub provisioner_control_epoch: i64,
    pub finalized_observed_slot: i64,
    pub finalized_address_hash: String,
    pub finalized_address_count: i32,
    pub global_control: LookupTableRolloutControl,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableUsageLeaseRecord {
    pub id: i64,
    pub cluster: String,
    pub lease_kind: LookupTableUsageLeaseKind,
    pub reference_key: String,
    pub route_lookup_table_id: i64,
    pub vault_id: Option<VaultId>,
    pub binding_id: Option<i64>,
    pub route_fingerprint: Option<String>,
    pub requirements_fingerprint: Option<String>,
    pub expires_at: DateTime<Utc>,
    pub released_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableUsageLeaseBundle {
    pub cluster: String,
    pub lease_kind: LookupTableUsageLeaseKind,
    pub reference_key: String,
    pub route_lookup_table_ids: Vec<i64>,
    pub vault_id: Option<VaultId>,
    pub binding_id: Option<i64>,
    pub route_fingerprint: Option<String>,
    pub requirements_fingerprint: Option<String>,
    pub expires_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableProvisioningRequestRecord {
    pub id: i64,
    pub cluster: String,
    pub vault_id: VaultId,
    pub route_fingerprint: String,
    pub requirements_fingerprint: String,
    pub shared_manifest_id: Option<i64>,
    pub vault_manifest_id: Option<i64>,
    pub desired_shared_hash: Option<String>,
    pub desired_vault_hash: Option<String>,
    pub desired_shared_address_count: i32,
    pub desired_vault_address_count: i32,
    pub sealed_at: Option<DateTime<Utc>>,
    pub request_status: LookupTableProvisioningRequestStatus,
    pub lease_owner: Option<String>,
    pub lease_expires_at: Option<DateTime<Utc>>,
    pub fencing_token: i64,
    pub attempt_count: i32,
    pub next_attempt_at: Option<DateTime<Utc>>,
    pub error_code: Option<String>,
    pub error_detail: Option<String>,
    pub requested_at: DateTime<Utc>,
    pub satisfied_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableProvisioningRequestUpsert {
    pub cluster: String,
    pub vault_id: VaultId,
    pub route_fingerprint: String,
    pub requirements_fingerprint: String,
    pub shared_manifest_id: Option<i64>,
    pub vault_manifest_id: Option<i64>,
    pub desired_shared_hash: Option<String>,
    pub desired_vault_hash: Option<String>,
    /// Immutable, canonically ordered compiler output. Required when the
    /// corresponding manifest id is not already known.
    pub shared_addresses: Vec<LookupTableManifestAddressRecord>,
    /// Immutable, canonically ordered compiler output. Required when the
    /// corresponding manifest id is not already known.
    pub vault_addresses: Vec<LookupTableManifestAddressRecord>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableCleanupProtection {
    pub table_id: i64,
    pub family_id: i64,
    pub cluster: String,
    pub table_address: String,
    pub expected_authority: String,
    pub address_count: i32,
    pub address_hash: String,
    pub mutation_epoch: i64,
    pub desired_state: LookupTableLifecycle,
    pub accepting_allocations: bool,
    pub can_deactivate: bool,
    pub can_close: bool,
    pub protection_reasons: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LegacyLookupTableRetirementRequest {
    pub cluster: String,
    pub table_address: String,
    pub expected_authority: String,
    pub expected_address_hash: String,
    pub expected_address_count: i32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LegacyLookupTableRetirement {
    pub table_id: i64,
    pub cluster: String,
    pub table_address: String,
    pub authority: String,
    pub address_hash: String,
    pub address_count: i32,
    pub previous_status: String,
    pub status: String,
    pub durable: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LegacyLookupTableCleanupProtection {
    pub table_id: i64,
    pub cluster: String,
    pub table_address: String,
    pub import_run_id: Option<i64>,
    pub legacy_kind: Option<LegacyLookupTableKind>,
    pub status: String,
    pub durable: bool,
    pub family_id: Option<i64>,
    pub expected_authority: String,
    pub address_count: i32,
    pub address_hash: String,
    pub ordered_addresses: Vec<String>,
    pub last_verified_slot: Option<i64>,
    pub zero_reference: bool,
    pub nonselectable: bool,
    pub can_deactivate: bool,
    pub can_close: bool,
    pub authorization_token: String,
    pub protection_reasons: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct VerifiedLegacyLookupTableCleanup {
    pub cluster: String,
    pub table_address: String,
    pub expected_authorization_token: String,
    pub operation_kind: LookupTableOperationKind,
    pub transaction_signature: String,
    pub observed_slot: i64,
    pub close_recipient: Option<String>,
    pub reclaimed_lamports: Option<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LegacyLookupTableCleanupAttemptPrepare {
    pub cluster: String,
    pub table_address: String,
    pub expected_authorization_token: String,
    pub operation_kind: LookupTableOperationKind,
    pub expected_authority: String,
    pub expected_address_count: i32,
    pub expected_address_hash: String,
    pub close_recipient: Option<String>,
    pub expected_reclaimed_lamports: Option<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SignedLegacyLookupTableCleanupAttempt {
    pub transaction_signature: String,
    pub message_hash: String,
    pub recent_blockhash: String,
    pub last_valid_block_height: i64,
    pub estimated_fee_lamports: i64,
    pub recipient_balance_before: Option<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FinalizedLegacyLookupTableCleanupAttempt {
    pub transaction_signature: String,
    pub finalized_slot: i64,
    pub recipient_balance_before: Option<i64>,
    pub recipient_balance_after: Option<i64>,
    pub actual_reclaimed_lamports: Option<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LegacyLookupTableCleanupAttemptRecord {
    pub id: i64,
    pub route_lookup_table_id: i64,
    pub cluster: String,
    pub table_address: String,
    pub operation_kind: LookupTableOperationKind,
    pub attempt_number: i32,
    pub authorization_token: String,
    pub expected_authority: String,
    pub expected_address_count: i32,
    pub expected_address_hash: String,
    pub close_recipient: Option<String>,
    pub expected_reclaimed_lamports: Option<i64>,
    pub attempt_state: LegacyLookupTableCleanupAttemptState,
    pub transaction_signature: Option<String>,
    pub message_hash: Option<String>,
    pub recent_blockhash: Option<String>,
    pub last_valid_block_height: Option<i64>,
    pub estimated_fee_lamports: Option<i64>,
    pub recipient_balance_before: Option<i64>,
    pub submitted_at: Option<DateTime<Utc>>,
    pub finalized_slot: Option<i64>,
    pub recipient_balance_after: Option<i64>,
    pub actual_reclaimed_lamports: Option<i64>,
    pub error_code: Option<String>,
    pub error_detail: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

/// Short-lived cleanup authorization. Retirement plus database triggers form
/// the durable nonblocking fence while the chain transaction finalizes.
pub struct LegacyLookupTableCleanupAuthorization {
    client: NeonSqlClient,
    protection: LegacyLookupTableCleanupProtection,
    operation_kind: LookupTableOperationKind,
}

impl LegacyLookupTableCleanupAuthorization {
    pub fn protection(&self) -> &LegacyLookupTableCleanupProtection {
        &self.protection
    }

    pub async fn record_finalized(
        self,
        input: VerifiedLegacyLookupTableCleanup,
    ) -> Result<(), OrchestratorError> {
        if input.cluster != self.protection.cluster
            || input.table_address != self.protection.table_address
            || input.expected_authorization_token != self.protection.authorization_token
            || input.operation_kind != self.operation_kind
            || input.transaction_signature.trim().is_empty()
            || input.observed_slot < 0
        {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup finalized evidence does not match its fenced authorization"
                    .to_owned(),
            ));
        }
        match self.operation_kind {
            LookupTableOperationKind::Deactivate => {
                if input.close_recipient.is_some() || input.reclaimed_lamports.is_some() {
                    return Err(OrchestratorError::StoreInvariant(
                        "legacy deactivation must not record close refund evidence".to_owned(),
                    ));
                }
            }
            LookupTableOperationKind::Close => {
                if input.close_recipient.as_deref()
                    != Some(self.protection.expected_authority.as_str())
                    || input.reclaimed_lamports.is_none_or(|value| value <= 0)
                {
                    return Err(OrchestratorError::StoreInvariant(
                        "legacy close must refund positive rent to the policy authority".to_owned(),
                    ));
                }
            }
            _ => {
                return Err(OrchestratorError::StoreInvariant(
                    "legacy cleanup authorization only supports deactivate or close".to_owned(),
                ));
            }
        }
        self.client
            .record_verified_legacy_lookup_table_cleanup(&self.protection, input)
            .await
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LegacyLookupTableImportSource {
    pub id: i64,
    pub cluster: String,
    pub scope: String,
    pub table_address: String,
    pub authority: String,
    pub status: String,
    pub durable: bool,
    pub address_count: i32,
    pub address_hash: String,
    pub addresses: Vec<String>,
    pub legacy_kind: Option<LegacyLookupTableKind>,
    pub legacy_import_run_id: Option<i64>,
    pub last_extended_slot: Option<i64>,
    pub last_extended_start_index: Option<i32>,
    pub last_verified_slot: Option<i64>,
    pub last_verified_at: Option<DateTime<Utc>>,
}

impl LegacyLookupTableImportSource {
    fn familyless_import_identity_is_valid(&self) -> bool {
        usize::try_from(self.address_count).ok() == Some(self.addresses.len())
            && self.addresses.len() <= usize::from(LOOKUP_TABLE_HARD_CAPACITY)
            && is_sha256_hex(&self.address_hash)
            && ordered_address_hash(&self.addresses) == self.address_hash
            && Pubkey::from_str(&self.table_address).is_ok()
            && Pubkey::from_str(&self.authority).is_ok()
            && self.legacy_import_run_id.is_some()
    }
}

/// Complete immutable import identity plus mutable cleanup evidence. Cleanup
/// inventory is sourced from these rows even after the physical ALT is closed
/// and therefore absent from finalized RPC account scans.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ImportedLegacyLookupTableCleanupRecord {
    pub source: LegacyLookupTableImportSource,
    pub import_fingerprint: String,
    pub import_verified_slot: i64,
    pub deactivated_slot: Option<i64>,
    pub deactivate_signature: Option<String>,
    pub closed_signature: Option<String>,
    pub close_recipient: Option<String>,
    pub reclaimed_lamports: Option<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct VerifiedLegacyLookupTableImport {
    pub source: LegacyLookupTableImportSource,
    pub legacy_kind: LegacyLookupTableKind,
    pub observed_owner: String,
    pub observed_authority: String,
    /// Stored as text because an active Solana ALT uses `u64::MAX`, which does
    /// not fit PostgreSQL BIGINT.
    pub observed_deactivation_slot: String,
    pub observed_last_extended_slot: i64,
    pub observed_last_extended_start_index: i32,
    pub observed_address_count: i32,
    pub observed_address_hash: String,
    pub observed_addresses: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LegacyLookupTableFleetImportRequest {
    pub cluster: String,
    pub rpc_genesis_hash: String,
    pub verified_slot: i64,
    pub verified_at: DateTime<Utc>,
    pub import_fingerprint: String,
    pub reason: String,
    pub updated_by: String,
    pub expected_table_count: i32,
    pub tables: Vec<VerifiedLegacyLookupTableImport>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LegacyLookupTableFleetImportResult {
    pub import_run_id: i64,
    pub cluster: String,
    pub legacy_kind: LegacyLookupTableKind,
    pub verified_slot: i64,
    pub verified_at: DateTime<Utc>,
    pub imported_table_count: i32,
    pub import_fingerprint: String,
    pub replayed: bool,
}

#[derive(Debug, Clone, PartialEq)]
pub struct AtomicVaultAllocationRequest {
    pub cluster: String,
    pub family_id: i64,
    pub vault_id: VaultId,
    pub manifest_id: i64,
    pub binding_ordinal: i32,
    pub desired_addresses: BTreeSet<String>,
    pub policy: PackedShardPolicy,
    pub next_generation: i32,
    pub next_shard_ordinal: i32,
    pub operation_context: Value,
    pub estimated_fee_lamports: Option<i64>,
    pub estimated_rent_lamports: Option<i64>,
    pub max_extension_addresses: usize,
}

#[derive(Debug, Clone, PartialEq)]
pub enum AtomicVaultAllocationResult {
    NotRequired,
    Existing {
        binding: LookupTableVaultBindingRecord,
    },
    BindingReserved {
        allocation: PackedVaultAllocation,
        binding: LookupTableVaultBindingRecord,
        operations: Vec<LookupTableOperationRecord>,
    },
    CreateQueued {
        allocation: PackedVaultAllocation,
        binding: LookupTableVaultBindingRecord,
        operations: Vec<LookupTableOperationRecord>,
    },
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum ActiveVaultBindingDisposition {
    Ready,
    Verify {
        table: ReusableLookupTableRecord,
        persisted_addresses: Vec<String>,
    },
    Relocate,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct ClusterBudgetUsage {
    spent_lamports: i64,
    reserved_lamports: i64,
    charged_lamports: i64,
    subject_reserved_lamports: i64,
    subject_actual_lamports: i64,
    window_ends_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, PartialEq)]
struct PreparedNewShardReservation {
    table_address: String,
    operation_context: Value,
    allocation_kind: LookupTableAllocationKind,
    binding_mode: LookupTableBindingMode,
    generation: i32,
    shard_ordinal: i32,
    reserved_capacity: u16,
    allocation_high_water: u16,
    accepting_allocations: bool,
    scope: String,
    operation_kind: LookupTableOperationKind,
}

fn prepare_new_shard_reservation(
    family: &LookupTableFamilyRecord,
    allocation: &PackedVaultAllocation,
    mut operation_context: Value,
    occupied_table_addresses: &BTreeSet<String>,
) -> Result<PreparedNewShardReservation, OrchestratorError> {
    let PackedVaultAllocation::PrepareNewShard {
        generation,
        shard_index,
        reserved_capacity,
        allocation_high_water,
        dedicated,
        ..
    } = allocation
    else {
        return Err(OrchestratorError::StoreInvariant(
            "new-shard reservation requires PrepareNewShard allocation".to_owned(),
        ));
    };
    let table_address = reserve_derived_lookup_table_address(
        &family.provisioning_authority,
        &mut operation_context,
        occupied_table_addresses,
    )?;
    let context = operation_context.as_object_mut().ok_or_else(|| {
        OrchestratorError::StoreInvariant("operation context must be a JSON object".to_owned())
    })?;
    context.insert("dedicated".to_owned(), Value::from(*dedicated));
    let allocation_kind = if *dedicated {
        LookupTableAllocationKind::DedicatedVault
    } else {
        LookupTableAllocationKind::VaultShard
    };
    Ok(PreparedNewShardReservation {
        table_address,
        operation_context,
        allocation_kind,
        binding_mode: if *dedicated {
            LookupTableBindingMode::Dedicated
        } else {
            LookupTableBindingMode::PackedShard
        },
        generation: *generation,
        shard_ordinal: *shard_index,
        reserved_capacity: *reserved_capacity,
        allocation_high_water: *allocation_high_water,
        accepting_allocations: !*dedicated,
        scope: format!(
            "reusable:{}:g{}:s{}",
            family.logical_name, generation, shard_index
        ),
        operation_kind: LookupTableOperationKind::Create,
    })
}

fn reserve_derived_lookup_table_address(
    authority: &str,
    operation_context: &mut Value,
    occupied_table_addresses: &BTreeSet<String>,
) -> Result<String, OrchestratorError> {
    let base_recent_slot = operation_context
        .get("recent_slot")
        .or_else(|| operation_context.get("recentSlot"))
        .and_then(Value::as_u64)
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "new lookup-table reservation requires operation_context.recent_slot".to_owned(),
            )
        })?;
    let authority = Pubkey::from_str(authority).map_err(|error| {
        OrchestratorError::StoreInvariant(format!(
            "lookup-table family has invalid provisioning authority: {error}"
        ))
    })?;
    let (table_address, selected_recent_slot) = (0_u64..=255)
        .filter_map(|offset| base_recent_slot.checked_sub(offset))
        .map(|slot| {
            (
                derive_lookup_table_address(&authority, slot).0.to_string(),
                slot,
            )
        })
        .find(|(address, _)| !occupied_table_addresses.contains(address))
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "could not reserve a unique lookup-table address in the durable recent-slot window"
                    .to_owned(),
            )
        })?;
    let context = operation_context.as_object_mut().ok_or_else(|| {
        OrchestratorError::StoreInvariant("operation context must be a JSON object".to_owned())
    })?;
    context.insert("recent_slot".to_owned(), Value::from(selected_recent_slot));
    context.remove("recentSlot");
    Ok(table_address)
}

async fn reserve_shared_lookup_table_in_tx(
    tx: &mut sqlx::PgConnection,
    family: &LookupTableFamilyRecord,
    generation: i32,
    shard_ordinal: i32,
    mut operation_context: Value,
) -> Result<(ReusableLookupTableRecord, Value), OrchestratorError> {
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")
        .bind(&family.provisioning_authority)
        .execute(&mut *tx)
        .await?;
    let occupied_table_addresses = sqlx::query_scalar::<_, String>(
        "SELECT table_address FROM loyal_yield.route_lookup_tables WHERE authority = $1",
    )
    .bind(&family.provisioning_authority)
    .fetch_all(&mut *tx)
    .await?
    .into_iter()
    .collect::<BTreeSet<_>>();
    let table_address = reserve_derived_lookup_table_address(
        &family.provisioning_authority,
        &mut operation_context,
        &occupied_table_addresses,
    )?;
    let scope = format!(
        "reusable:{}:g{}:s{}",
        family.logical_name, generation, shard_ordinal
    );
    let row = sqlx::query(
        r#"
        INSERT INTO loyal_yield.route_lookup_tables
            (cluster, scope, table_address, authority, payer, status, durable,
             address_count, address_hash, addresses, family_id, allocation_kind,
             generation, shard_ordinal, desired_state, accepting_allocations,
             allocation_high_water, reserved_address_count, usable_address_count,
             mutation_epoch)
        VALUES ($1, $2, $3, $4, $5, 'warming', TRUE,
                0, '', '[]'::jsonb, $6, 'shared_market', $7, $8,
                'preparing', FALSE, $9, 0, 0, 0)
        RETURNING *
        "#,
    )
    .bind(&family.cluster)
    .bind(scope)
    .bind(table_address)
    .bind(&family.provisioning_authority)
    .bind(&family.payer)
    .bind(family.id)
    .bind(generation)
    .bind(shard_ordinal)
    .bind(family.allocation_high_water)
    .fetch_one(&mut *tx)
    .await?;
    Ok((reusable_lookup_table_from_row(&row)?, operation_context))
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableBindingHeadFlip {
    pub active: LookupTableVaultBindingRecord,
    pub predecessor: Option<LookupTableVaultBindingRecord>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct LookupTableProvisioningPlanPolicy {
    pub vault_policy: PackedShardPolicy,
    pub shared_shard_capacity: u16,
    pub max_extension_addresses: usize,
    pub operation_context: Value,
    pub estimated_fee_lamports: Option<i64>,
    pub estimated_rent_lamports: Option<i64>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct LookupTableProvisioningPlan {
    pub request: LookupTableProvisioningRequestRecord,
    pub shared_target_generation: i32,
    pub shared_operations: Vec<LookupTableOperationRecord>,
    pub vault_allocation: AtomicVaultAllocationResult,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableRollbackFinalization {
    pub family_id: i64,
    pub cleared_previous_generation: Option<i32>,
    pub retired_binding_ids: Vec<i64>,
    pub retiring_table_ids: Vec<i64>,
    pub released_reserved_capacity: i32,
}

fn provisioning_request_is_satisfied(
    shared_ready: bool,
    shared_operation_count: usize,
    pending_operation_count: i64,
    vault_allocation: &AtomicVaultAllocationResult,
) -> bool {
    shared_ready
        && shared_operation_count == 0
        && pending_operation_count == 0
        && matches!(
            vault_allocation,
            AtomicVaultAllocationResult::Existing { .. } | AtomicVaultAllocationResult::NotRequired
        )
}

fn terminal_provisioning_operation<'a>(
    shared_operations: &'a [LookupTableOperationRecord],
    vault_allocation: &'a AtomicVaultAllocationResult,
) -> Option<&'a LookupTableOperationRecord> {
    let vault_operations = match vault_allocation {
        AtomicVaultAllocationResult::BindingReserved { operations, .. }
        | AtomicVaultAllocationResult::CreateQueued { operations, .. } => operations.as_slice(),
        AtomicVaultAllocationResult::Existing { .. } | AtomicVaultAllocationResult::NotRequired => {
            &[]
        }
    };
    shared_operations
        .iter()
        .chain(vault_operations.iter())
        .find(|operation| {
            matches!(
                operation.operation_state,
                LookupTableOperationStatus::PermanentFailure
                    | LookupTableOperationStatus::Cancelled
            )
        })
}

fn validate_lookup_table_family_bootstrap(
    family: &LookupTableFamilyRecord,
    input: &LookupTableFamilyUpsert,
) -> Result<(), OrchestratorError> {
    if family.kind != input.kind
        || family.planner_version != input.planner_version
        || family.catalog_version != input.catalog_version
        || family.provisioning_authority != input.provisioning_authority
        || family.payer != input.payer
        || family.hard_capacity != input.hard_capacity
        || family.largest_atomic_expansion != input.largest_atomic_expansion
        || family.safety_margin != input.safety_margin
        || family.allocation_high_water != input.allocation_high_water
    {
        return Err(OrchestratorError::StoreInvariant(format!(
            "lookup-table family {} bootstrap configuration conflicts with the existing family",
            family.id
        )));
    }
    Ok(())
}

async fn classify_active_vault_binding_in_connection(
    tx: &mut sqlx::PgConnection,
    family: &LookupTableFamilyRecord,
    binding: &LookupTableVaultBindingRecord,
    manifest_addresses: &BTreeSet<String>,
) -> Result<ActiveVaultBindingDisposition, OrchestratorError> {
    let Some(table_row) = sqlx::query(
        r#"
        SELECT * FROM loyal_yield.route_lookup_tables
        WHERE id = $1 AND family_id = $2
        FOR UPDATE
        "#,
    )
    .bind(binding.route_lookup_table_id)
    .bind(family.id)
    .fetch_optional(&mut *tx)
    .await?
    else {
        return Ok(ActiveVaultBindingDisposition::Relocate);
    };
    let table = reusable_lookup_table_from_row(&table_row)?;
    let durable: bool = table_row.try_get("durable")?;
    let expected_allocation_kind = match binding.allocation_mode {
        LookupTableBindingMode::PackedShard => LookupTableAllocationKind::VaultShard,
        LookupTableBindingMode::Dedicated => LookupTableAllocationKind::DedicatedVault,
    };
    if table.cluster != family.cluster
        || table.authority != family.provisioning_authority
        || table.payer != family.payer
        || table.allocation_kind != expected_allocation_kind
        || family.active_generation != Some(table.generation)
        || table.desired_state != LookupTableLifecycle::Active
        || !durable
        || binding.reserved_capacity < i32::try_from(manifest_addresses.len()).unwrap_or(i32::MAX)
    {
        return Ok(ActiveVaultBindingDisposition::Relocate);
    }

    let membership_rows = sqlx::query(
        r#"
        SELECT address, ordinal, added_operation_id, added_slot,
               usable_after_slot, last_verified_slot, last_verified_at
        FROM loyal_yield.lookup_table_addresses
        WHERE route_lookup_table_id = $1 ORDER BY ordinal
        "#,
    )
    .bind(table.id)
    .fetch_all(&mut *tx)
    .await?;
    let membership = membership_rows
        .iter()
        .map(|row| {
            Ok(LookupTableMembershipAddress {
                address: row.try_get("address")?,
                ordinal: row.try_get("ordinal")?,
                added_operation_id: row.try_get("added_operation_id")?,
                added_slot: row.try_get("added_slot")?,
                usable_after_slot: row.try_get("usable_after_slot")?,
                last_verified_slot: row.try_get("last_verified_slot")?,
                last_verified_at: row.try_get("last_verified_at")?,
            })
        })
        .collect::<Result<Vec<_>, OrchestratorError>>()?;
    let persisted_addresses = membership
        .iter()
        .map(|entry| entry.address.clone())
        .collect::<Vec<_>>();
    let persisted_set = persisted_addresses.iter().cloned().collect::<BTreeSet<_>>();
    let structurally_complete = membership.len()
        == usize::try_from(table.address_count).unwrap_or(usize::MAX)
        && membership
            .iter()
            .enumerate()
            .all(|(ordinal, entry)| entry.ordinal == ordinal as i32)
        && ordered_address_hash(&persisted_addresses) == table.address_hash
        && manifest_addresses.is_subset(&persisted_set);
    if !structurally_complete {
        return Ok(ActiveVaultBindingDisposition::Relocate);
    }

    let pending_operation_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.lookup_table_operations
        WHERE route_lookup_table_id = $1
          AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
        "#,
    )
    .bind(table.id)
    .fetch_one(&mut *tx)
    .await?;
    let fully_verified = table.legacy_status == "usable"
        && table.usable_address_count == table.address_count
        && table.last_verified_slot.is_some_and(|slot| {
            validate_membership(&membership, slot).is_ok()
                && membership.iter().all(|entry| {
                    entry.usable_after_slot <= slot && entry.last_verified_slot >= slot
                })
        })
        && pending_operation_count == 0;
    Ok(if fully_verified {
        ActiveVaultBindingDisposition::Ready
    } else {
        ActiveVaultBindingDisposition::Verify {
            table,
            persisted_addresses,
        }
    })
}

async fn load_cluster_budget_usage_in_connection(
    tx: &mut sqlx::PgConnection,
    cluster: &str,
    subject_kind: &str,
    subject_id: i64,
    now: DateTime<Utc>,
) -> Result<ClusterBudgetUsage, OrchestratorError> {
    let row = sqlx::query(
        r#"
        WITH v2_per_operation AS (
            SELECT 'operation'::TEXT AS subject_kind,
                   operation.id AS subject_id,
                   COALESCE(sum(reservation.reserved_lamports), 0)::BIGINT
                       AS reserved_lamports,
                   (COALESCE(operation.actual_fee_lamports, 0)
                    + COALESCE(operation.actual_rent_lamports, 0))::BIGINT
                       AS actual_lamports,
                   min(reservation.reserved_until) AS window_ends_at
            FROM loyal_yield.lookup_table_cluster_budget_reservations reservation
            JOIN loyal_yield.lookup_table_operations operation
              ON operation.id = reservation.operation_id
            WHERE reservation.cluster = $1
              AND reservation.reserved_until > $4
              AND operation.operation_state <> 'cancelled'
            GROUP BY operation.id, operation.actual_fee_lamports,
                     operation.actual_rent_lamports
        ),
        legacy_per_attempt AS (
            SELECT 'legacy_cleanup'::TEXT AS subject_kind,
                   attempt.id AS subject_id,
                   COALESCE(sum(reservation.reserved_lamports), 0)::BIGINT
                       AS reserved_lamports,
                   0::BIGINT AS actual_lamports,
                   min(reservation.reserved_until) AS window_ends_at
            FROM loyal_yield.lookup_table_legacy_cleanup_budget_reservations reservation
            JOIN loyal_yield.lookup_table_legacy_cleanup_attempts attempt
              ON attempt.id = reservation.legacy_cleanup_attempt_id
            WHERE reservation.cluster = $1
              AND reservation.reserved_until > $4
            GROUP BY attempt.id
        ),
        per_subject AS (
            SELECT * FROM v2_per_operation
            UNION ALL
            SELECT * FROM legacy_per_attempt
        )
        SELECT COALESCE(sum(actual_lamports), 0)::BIGINT AS spent_lamports,
               COALESCE(sum(GREATEST(reserved_lamports - actual_lamports, 0)), 0)::BIGINT
                   AS reserved_lamports,
               COALESCE(sum(GREATEST(reserved_lamports, actual_lamports)), 0)::BIGINT
                   AS charged_lamports,
               COALESCE(max(reserved_lamports) FILTER (
                   WHERE subject_kind = $2 AND subject_id = $3
               ), 0)::BIGINT AS subject_reserved_lamports,
               COALESCE(max(actual_lamports) FILTER (
                   WHERE subject_kind = $2 AND subject_id = $3
               ), 0)::BIGINT AS subject_actual_lamports,
               min(window_ends_at) AS window_ends_at
        FROM per_subject
        "#,
    )
    .bind(cluster)
    .bind(subject_kind)
    .bind(subject_id)
    .bind(now)
    .fetch_one(&mut *tx)
    .await?;
    Ok(ClusterBudgetUsage {
        spent_lamports: row.try_get("spent_lamports")?,
        reserved_lamports: row.try_get("reserved_lamports")?,
        charged_lamports: row.try_get("charged_lamports")?,
        subject_reserved_lamports: row.try_get("subject_reserved_lamports")?,
        subject_actual_lamports: row.try_get("subject_actual_lamports")?,
        window_ends_at: row.try_get("window_ends_at")?,
    })
}

impl NeonSqlClient {
    pub async fn upsert_lookup_table_family(
        &self,
        input: LookupTableFamilyUpsert,
    ) -> Result<LookupTableFamilyRecord, OrchestratorError> {
        self.create_or_validate_lookup_table_family(input).await
    }

    /// Bootstraps immutable family identity/configuration without ever
    /// rewriting live generation pointers or lifecycle state on a retry.
    pub async fn create_or_validate_lookup_table_family(
        &self,
        input: LookupTableFamilyUpsert,
    ) -> Result<LookupTableFamilyRecord, OrchestratorError> {
        if !(1..=i32::from(LOOKUP_TABLE_HARD_CAPACITY)).contains(&input.hard_capacity)
            || input.largest_atomic_expansion <= 0
            || input.safety_margin <= 0
            || input
                .largest_atomic_expansion
                .checked_add(input.safety_margin)
                .is_none_or(|reserve| reserve >= input.hard_capacity)
            || input.allocation_high_water
                != input
                    .hard_capacity
                    .saturating_sub(input.largest_atomic_expansion)
                    .saturating_sub(input.safety_margin)
        {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table family capacity configuration must satisfy high_water = hard_capacity - largest_atomic_expansion - safety_margin"
                    .to_owned(),
            ));
        }
        if input.payer != input.provisioning_authority {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table family provisioning authority and payer must match".to_owned(),
            ));
        }
        let inserted = sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_families
                (cluster, logical_name, kind, desired_state, planner_version,
                 catalog_version, active_generation, previous_generation, rollback_until,
                 provisioning_authority, payer, hard_capacity,
                 largest_atomic_expansion, safety_margin, allocation_high_water)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
            ON CONFLICT (cluster, logical_name) DO NOTHING
            RETURNING *
            "#,
        )
        .bind(&input.cluster)
        .bind(&input.logical_name)
        .bind(input.kind.as_str())
        .bind(input.desired_state.as_str())
        .bind(&input.planner_version)
        .bind(&input.catalog_version)
        .bind(input.active_generation)
        .bind(input.previous_generation)
        .bind(input.rollback_until)
        .bind(&input.provisioning_authority)
        .bind(&input.payer)
        .bind(input.hard_capacity)
        .bind(input.largest_atomic_expansion)
        .bind(input.safety_margin)
        .bind(input.allocation_high_water)
        .fetch_optional(self.pool())
        .await?;
        let row = match inserted {
            Some(row) => row,
            None => sqlx::query(
                "SELECT * FROM loyal_yield.lookup_table_families WHERE cluster = $1 AND logical_name = $2",
            )
            .bind(&input.cluster)
            .bind(&input.logical_name)
            .fetch_one(self.pool())
            .await?,
        };
        let family = lookup_table_family_from_row(&row)?;
        validate_lookup_table_family_bootstrap(&family, &input)?;
        Ok(family)
    }

    pub async fn lookup_table_family(
        &self,
        cluster: &str,
        logical_name: &str,
    ) -> Result<Option<LookupTableFamilyRecord>, OrchestratorError> {
        let row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_families WHERE cluster = $1 AND logical_name = $2",
        )
        .bind(cluster)
        .bind(logical_name)
        .fetch_optional(self.pool())
        .await?;
        row.as_ref().map(lookup_table_family_from_row).transpose()
    }

    /// Loads a family by durable identity without filtering lifecycle. Signed
    /// operation reconciliation must remain possible while a family is paused,
    /// retiring, or retired.
    pub async fn lookup_table_family_by_id(
        &self,
        family_id: i64,
    ) -> Result<Option<LookupTableFamilyRecord>, OrchestratorError> {
        let row = sqlx::query("SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1")
            .bind(family_id)
            .fetch_optional(self.pool())
            .await?;
        row.as_ref().map(lookup_table_family_from_row).transpose()
    }

    pub async fn active_lookup_table_families(
        &self,
        cluster: &str,
    ) -> Result<Vec<LookupTableFamilyRecord>, OrchestratorError> {
        let rows = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_families WHERE cluster = $1 AND desired_state = 'active' ORDER BY kind, logical_name",
        )
        .bind(cluster)
        .fetch_all(self.pool())
        .await?;
        rows.iter().map(lookup_table_family_from_row).collect()
    }

    pub async fn insert_reusable_lookup_table(
        &self,
        input: ReusableLookupTableInsert,
    ) -> Result<ReusableLookupTableRecord, OrchestratorError> {
        if input.allocation_kind == LookupTableAllocationKind::DedicatedVault
            && input.accepting_allocations
        {
            return Err(OrchestratorError::StoreInvariant(
                "dedicated lookup tables can never accept additional allocations".to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.route_lookup_tables
                (cluster, scope, table_address, authority, payer, status, durable,
                 address_count, address_hash, addresses, create_signature,
                 family_id, allocation_kind, generation, shard_ordinal,
                 desired_state, accepting_allocations, allocation_high_water,
                 reserved_address_count, usable_address_count, mutation_epoch)
            VALUES
                ($1, $2, $3, $4, $5, 'warming', TRUE,
                 0, '', '[]'::jsonb, $6,
                 $7, $8, $9, $10, $11, $12, $13, 0, 0, $14)
            ON CONFLICT (table_address) DO NOTHING
            RETURNING *
            "#,
        )
        .bind(input.cluster)
        .bind(input.scope)
        .bind(&input.table_address)
        .bind(input.authority)
        .bind(input.payer)
        .bind(input.create_signature)
        .bind(input.family_id)
        .bind(input.allocation_kind.as_str())
        .bind(input.generation)
        .bind(input.shard_ordinal)
        .bind(input.desired_state.as_str())
        .bind(input.accepting_allocations)
        .bind(input.allocation_high_water)
        .bind(input.mutation_epoch)
        .fetch_optional(self.pool())
        .await?;

        let row = match row {
            Some(row) => row,
            None => sqlx::query(
                "SELECT * FROM loyal_yield.route_lookup_tables WHERE table_address = $1 AND family_id IS NOT NULL",
            )
            .bind(&input.table_address)
            .fetch_one(self.pool())
            .await?,
        };
        reusable_lookup_table_from_row(&row)
    }

    pub async fn reusable_lookup_table(
        &self,
        id: i64,
    ) -> Result<Option<ReusableLookupTableRecord>, OrchestratorError> {
        let row = sqlx::query(
            "SELECT * FROM loyal_yield.route_lookup_tables WHERE id = $1 AND family_id IS NOT NULL",
        )
        .bind(id)
        .fetch_optional(self.pool())
        .await?;
        row.as_ref().map(reusable_lookup_table_from_row).transpose()
    }

    /// Database-native v2 retirement inventory. The cleanup command uses this
    /// separately from the immutable imported-legacy fleet and only enqueues
    /// provisioner-owned lifecycle operations for these registered tables.
    pub async fn registered_lookup_table_cleanup_inventory(
        &self,
        cluster: &str,
    ) -> Result<Vec<ReusableLookupTableRecord>, OrchestratorError> {
        let rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.route_lookup_tables
            WHERE cluster = $1 AND family_id IS NOT NULL
              AND desired_state IN ('retiring', 'deactivated', 'closed')
            ORDER BY id
            "#,
        )
        .bind(cluster)
        .fetch_all(self.pool())
        .await?;
        rows.iter().map(reusable_lookup_table_from_row).collect()
    }

    pub async fn persist_lookup_table_manifest(
        &self,
        mut input: LookupTableManifestWrite,
    ) -> Result<LookupTableManifestRecord, OrchestratorError> {
        input.addresses.sort_by_key(|address| address.ordinal);
        validate_manifest_write(&input)?;
        let mut tx = self.pool().begin().await?;
        let inserted_id = sqlx::query_scalar::<_, i64>(
            r#"
            INSERT INTO loyal_yield.lookup_table_manifests
                (family_id, subject_kind, subject_key, vault_id, desired_set_hash,
                 address_count, source_slot, planner_version, catalog_version)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
            ON CONFLICT (family_id, subject_kind, subject_key, desired_set_hash) DO NOTHING
            RETURNING id
            "#,
        )
        .bind(input.family_id)
        .bind(input.subject_kind.as_str())
        .bind(&input.subject_key)
        .bind(input.vault_id.map(VaultId::as_i64))
        .bind(&input.desired_set_hash)
        .bind(input.addresses.len() as i32)
        .bind(input.source_slot)
        .bind(&input.planner_version)
        .bind(&input.catalog_version)
        .fetch_optional(&mut *tx)
        .await?;

        let manifest_id = if let Some(manifest_id) = inserted_id {
            if !input.addresses.is_empty() {
                let mut query = QueryBuilder::<Postgres>::new(
                    "INSERT INTO loyal_yield.lookup_table_manifest_addresses (manifest_id, address, ordinal, semantic_class, account_role, is_writable) ",
                );
                query.push_values(&input.addresses, |mut row, address| {
                    row.push_bind(manifest_id)
                        .push_bind(&address.address)
                        .push_bind(address.ordinal)
                        .push_bind(address.semantic_class.as_str())
                        .push_bind(&address.account_role)
                        .push_bind(address.is_writable);
                });
                query.build().execute(&mut *tx).await?;
            }
            sqlx::query(
                "UPDATE loyal_yield.lookup_table_manifests SET sealed_at = now() WHERE id = $1",
            )
            .bind(manifest_id)
            .execute(&mut *tx)
            .await?;
            manifest_id
        } else {
            sqlx::query_scalar::<_, i64>(
                r#"
                SELECT id FROM loyal_yield.lookup_table_manifests
                WHERE family_id = $1 AND subject_kind = $2
                  AND subject_key = $3 AND desired_set_hash = $4
                "#,
            )
            .bind(input.family_id)
            .bind(input.subject_kind.as_str())
            .bind(&input.subject_key)
            .bind(&input.desired_set_hash)
            .fetch_one(&mut *tx)
            .await?
        };
        tx.commit().await?;
        let persisted = self
            .lookup_table_manifest(manifest_id)
            .await?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(format!(
                    "lookup-table manifest {manifest_id} disappeared after persistence"
                ))
            })?;
        if persisted.addresses != input.addresses {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table manifest {} idempotency collision has different addresses",
                persisted.id
            )));
        }
        Ok(persisted)
    }

    pub async fn lookup_table_manifest(
        &self,
        manifest_id: i64,
    ) -> Result<Option<LookupTableManifestRecord>, OrchestratorError> {
        let Some(row) =
            sqlx::query("SELECT * FROM loyal_yield.lookup_table_manifests WHERE id = $1")
                .bind(manifest_id)
                .fetch_optional(self.pool())
                .await?
        else {
            return Ok(None);
        };
        let address_rows = sqlx::query(
            r#"
            SELECT address, ordinal, semantic_class, account_role, is_writable
            FROM loyal_yield.lookup_table_manifest_addresses
            WHERE manifest_id = $1 ORDER BY ordinal
            "#,
        )
        .bind(manifest_id)
        .fetch_all(self.pool())
        .await?;
        lookup_table_manifest_from_rows(&row, &address_rows).map(Some)
    }

    /// Publishes a vault-independent shared-market catalog revision and moves
    /// the single durable family head atomically. Route requests never call
    /// this method and therefore cannot grow shared desired state implicitly.
    pub async fn upsert_shared_market_catalog(
        &self,
        mut input: SharedMarketCatalogUpsert,
    ) -> Result<SharedMarketCatalogHeadRecord, OrchestratorError> {
        input.addresses.sort_by_key(|address| address.ordinal);
        validate_logical_shared_market_catalog_addresses(&input.addresses)?;
        if input.addresses.is_empty()
            || input.cluster.trim().is_empty()
            || input.catalog_version.trim().is_empty()
            || input.reason.trim().is_empty()
            || input.updated_by.trim().is_empty()
            || input.source_slot.is_some_and(|slot| slot < 0)
            || !input.source_metadata.is_object()
            || !is_sha256_hex(&input.desired_set_hash)
            || !is_sha256_hex(&input.enabled_mints_hash)
            || !is_sha256_hex(&input.reserve_set_hash)
            || lookup_table_manifest_address_records_hash(&input.addresses)
                != input.desired_set_hash
        {
            return Err(OrchestratorError::StoreInvariant(
                "shared-market catalog metadata or canonical address hash is invalid".to_owned(),
            ));
        }

        let mut tx = self.pool().begin().await?;
        let family_rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_families
            WHERE cluster = $1 AND kind = 'shared_market' AND desired_state = 'active'
            ORDER BY logical_name, id
            FOR UPDATE
            "#,
        )
        .bind(&input.cluster)
        .fetch_all(&mut *tx)
        .await?;
        if family_rows.len() != 1 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "cluster {:?} requires exactly one active shared_market lookup-table family, found {}",
                input.cluster,
                family_rows.len()
            )));
        }
        let family = lookup_table_family_from_row(&family_rows[0])?;
        if family.catalog_version != input.catalog_version {
            return Err(OrchestratorError::StoreInvariant(format!(
                "shared-market catalog does not match family {} version",
                family.id
            )));
        }

        let current = load_shared_market_catalog_head_in_connection(
            &mut tx,
            &input.cluster,
            SharedMarketCatalogHeadLock::Update,
        )
        .await?;
        if let Some(current) = current.as_ref() {
            if current.catalog_version == input.catalog_version
                && current.desired_set_hash == input.desired_set_hash
            {
                if current.addresses != input.addresses {
                    return Err(OrchestratorError::StoreInvariant(format!(
                        "shared-market catalog head {} has conflicting normalized identity",
                        current.catalog_revision_id
                    )));
                }
                if current.enabled_mints_hash == input.enabled_mints_hash
                    && current.reserve_set_hash == input.reserve_set_hash
                {
                    tx.commit().await?;
                    return Ok(current.clone());
                }
            }
            if current.source_slot.is_some() && input.source_slot < current.source_slot {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "shared-market catalog source slot regressed from {:?} to {:?}",
                    current.source_slot, input.source_slot
                )));
            }
        }
        let in_flight_shared_mutation_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*)::BIGINT
            FROM loyal_yield.lookup_table_operations
            WHERE family_id = $1
              AND operation_kind IN ('create', 'extend', 'rollover')
              AND operation_state NOT IN (
                  'complete', 'permanent_failure', 'cancelled'
              )
            "#,
        )
        .bind(family.id)
        .fetch_one(&mut *tx)
        .await?;
        let active_shared_permit_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*)::BIGINT
            FROM loyal_yield.lookup_table_provisioner_broadcast_permits permit
            JOIN loyal_yield.lookup_table_operations operation
              ON operation.id = permit.operation_id
            WHERE operation.family_id = $1 AND permit.resolved_at IS NULL
            "#,
        )
        .bind(family.id)
        .fetch_one(&mut *tx)
        .await?;
        if current.is_some()
            && (in_flight_shared_mutation_count != 0 || active_shared_permit_count != 0)
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "shared-market catalog publication requires a drained family; found {in_flight_shared_mutation_count} nonterminal mutation(s) and {active_shared_permit_count} active broadcast permit(s)"
            )));
        }

        let manifest = persist_lookup_table_manifest_in_tx(
            &mut tx,
            LookupTableManifestWrite {
                family_id: family.id,
                subject_kind: LookupTableManifestSubject::SharedMarket,
                subject_key: format!("shared-market-catalog:{}", input.catalog_version),
                vault_id: None,
                desired_set_hash: input.desired_set_hash.clone(),
                source_slot: input.source_slot,
                planner_version: family.planner_version.clone(),
                catalog_version: family.catalog_version.clone(),
                addresses: input.addresses.clone(),
            },
        )
        .await?;
        // A new monotonic audit revision may intentionally point back to an
        // older immutable manifest (A -> B -> A rollback). Manifest identity
        // is content-addressed; only catalog revision/head identity advances.
        let catalog_revision = current
            .as_ref()
            .map_or(1, |head| head.catalog_revision.saturating_add(1));
        let catalog_revision_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.lookup_table_shared_market_catalog_revisions
                (family_id, manifest_id, catalog_revision, catalog_version,
                 desired_set_hash, enabled_mints_hash, reserve_set_hash,
                 address_count, source_slot, source_observed_at,
                 source_metadata, reason, updated_by)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
            RETURNING id
            "#,
        )
        .bind(family.id)
        .bind(manifest.id)
        .bind(catalog_revision)
        .bind(&input.catalog_version)
        .bind(&input.desired_set_hash)
        .bind(&input.enabled_mints_hash)
        .bind(&input.reserve_set_hash)
        .bind(i32::try_from(input.addresses.len()).map_err(|_| {
            OrchestratorError::StoreInvariant(
                "shared-market catalog address count exceeds PostgreSQL INTEGER".to_owned(),
            )
        })?)
        .bind(manifest.source_slot)
        .bind(input.source_observed_at)
        .bind(&input.source_metadata)
        .bind(&input.reason)
        .bind(&input.updated_by)
        .fetch_one(&mut *tx)
        .await?;
        if current.is_some() {
            sqlx::query(
                r#"
                UPDATE loyal_yield.lookup_table_shared_market_catalog_heads
                SET catalog_revision_id = $2, target_generation = NULL,
                    readiness_state = 'pending', activated_at = NULL,
                    updated_at = now()
                WHERE family_id = $1
                "#,
            )
            .bind(family.id)
            .bind(catalog_revision_id)
            .execute(&mut *tx)
            .await?;
        } else {
            sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_shared_market_catalog_heads
                    (family_id, catalog_revision_id)
                VALUES ($1, $2)
                "#,
            )
            .bind(family.id)
            .bind(catalog_revision_id)
            .execute(&mut *tx)
            .await?;
        }
        cancel_superseded_unsigned_shared_market_operations_in_connection(
            &mut tx,
            family.id,
            manifest.id,
        )
        .await?;
        let head = load_shared_market_catalog_head_in_connection(
            &mut tx,
            &input.cluster,
            SharedMarketCatalogHeadLock::None,
        )
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "shared-market catalog head disappeared after publication".to_owned(),
            )
        })?;
        tx.commit().await?;
        Ok(head)
    }

    pub async fn shared_market_catalog_head(
        &self,
        cluster: &str,
    ) -> Result<Option<SharedMarketCatalogHeadRecord>, OrchestratorError> {
        let mut connection = self.pool().acquire().await?;
        load_shared_market_catalog_head_from_connection(
            &mut connection,
            cluster,
            SharedMarketCatalogHeadLock::None,
        )
        .await
    }

    /// Read-only runtime fence. `Covered` means both the route's typed shared
    /// requirements are catalog subsets and the active physical generation is
    /// the exact current catalog (no append-only remnants from an older head).
    pub async fn validate_shared_market_catalog_route(
        &self,
        cluster: &str,
        mut route_addresses: Vec<LookupTableManifestAddressRecord>,
    ) -> Result<SharedMarketCatalogRouteValidation, OrchestratorError> {
        route_addresses.sort_by_key(|address| address.ordinal);
        validate_request_addresses(&route_addresses, LookupTableManifestSubject::SharedMarket)?;
        let mut tx = self.pool().begin().await?;
        let Some(catalog) = load_shared_market_catalog_head_in_connection(
            &mut tx,
            cluster,
            SharedMarketCatalogHeadLock::None,
        )
        .await?
        else {
            tx.commit().await?;
            return Ok(SharedMarketCatalogRouteValidation {
                state: SharedMarketCatalogRouteValidationState::MissingHead,
                catalog_revision_id: None,
                catalog_revision: None,
                desired_set_hash: None,
                readiness_state: None,
                target_generation: None,
                active_generation: None,
                route_missing_addresses: route_addresses
                    .into_iter()
                    .map(|row| row.address)
                    .collect(),
                semantic_mismatch_addresses: Vec::new(),
                active_missing_addresses: Vec::new(),
                active_extra_addresses: Vec::new(),
            });
        };
        let (route_missing_addresses, semantic_mismatch_addresses) =
            shared_market_route_catalog_drift(&route_addresses, &catalog.addresses);
        let physical = shared_market_catalog_generation_evidence_in_connection(
            &mut tx,
            catalog.family_id,
            catalog.active_generation,
            &catalog.addresses,
        )
        .await?;
        let covered = route_missing_addresses.is_empty()
            && semantic_mismatch_addresses.is_empty()
            && catalog.readiness_state == SharedMarketCatalogReadiness::Active
            && catalog.target_generation == catalog.active_generation
            && physical.ready;
        let validation = SharedMarketCatalogRouteValidation {
            state: if covered {
                SharedMarketCatalogRouteValidationState::Covered
            } else {
                SharedMarketCatalogRouteValidationState::Drift
            },
            catalog_revision_id: Some(catalog.catalog_revision_id),
            catalog_revision: Some(catalog.catalog_revision),
            desired_set_hash: Some(catalog.desired_set_hash),
            readiness_state: Some(catalog.readiness_state),
            target_generation: catalog.target_generation,
            active_generation: catalog.active_generation,
            route_missing_addresses,
            semantic_mismatch_addresses,
            active_missing_addresses: physical.missing_addresses,
            active_extra_addresses: physical.extra_addresses,
        };
        tx.commit().await?;
        Ok(validation)
    }

    /// Persists finalized RPC drift against the exact current catalog/table
    /// fence. The catalog planner treats every open report as a mandatory
    /// generation rollover, even when the normalized DB membership still
    /// appears exact.
    pub async fn report_shared_market_physical_drift(
        &self,
        input: SharedMarketPhysicalDriftReport,
    ) -> Result<SharedMarketPhysicalDriftRecord, OrchestratorError> {
        let observed_hash = validate_shared_market_physical_drift_report(&input)?;
        let mut tx = self.pool().begin().await?;
        let drift =
            report_shared_market_physical_drift_in_tx(&mut tx, &input, &observed_hash).await?;
        tx.commit().await?;
        Ok(drift)
    }

    pub async fn insert_lookup_table_vault_binding(
        &self,
        input: LookupTableVaultBindingInsert,
    ) -> Result<LookupTableVaultBindingRecord, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        let family_kind: Option<String> = sqlx::query_scalar(
            "SELECT kind FROM loyal_yield.lookup_table_families WHERE id = $1 FOR UPDATE",
        )
        .bind(input.family_id)
        .fetch_optional(&mut *tx)
        .await?;
        if family_kind.as_deref() != Some(LookupTableFamilyKind::VaultShards.as_str()) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table family {} is not a vault-shards family",
                input.family_id
            )));
        }
        let desired_head_revision = upsert_vault_desired_head_in_tx(
            &mut tx,
            input.family_id,
            input.vault_id,
            input.binding_ordinal,
            input.manifest_id,
        )
        .await?;
        supersede_stale_vault_binding_revisions_in_tx(
            &mut tx,
            input.family_id,
            input.vault_id,
            input.binding_ordinal,
            input.manifest_id,
            desired_head_revision,
        )
        .await?;
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_vault_bindings
                (vault_id, family_id, route_lookup_table_id, manifest_id,
                 binding_ordinal, desired_head_revision, allocation_mode,
                 reserved_capacity, predecessor_binding_id, lifecycle_state)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'preparing')
            RETURNING *
            "#,
        )
        .bind(input.vault_id.as_i64())
        .bind(input.family_id)
        .bind(input.route_lookup_table_id)
        .bind(input.manifest_id)
        .bind(input.binding_ordinal)
        .bind(desired_head_revision)
        .bind(input.allocation_mode.as_str())
        .bind(input.reserved_capacity)
        .bind(input.predecessor_binding_id)
        .fetch_one(&mut *tx)
        .await?;
        let binding = lookup_table_binding_from_row(&row)?;
        tx.commit().await?;
        Ok(binding)
    }

    pub async fn lookup_table_vault_bindings(
        &self,
        vault_id: VaultId,
        family_id: i64,
    ) -> Result<Vec<LookupTableVaultBindingRecord>, OrchestratorError> {
        let rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_vault_bindings
            WHERE vault_id = $1 AND family_id = $2
            ORDER BY binding_ordinal, created_at DESC
            "#,
        )
        .bind(vault_id.as_i64())
        .bind(family_id)
        .fetch_all(self.pool())
        .await?;
        rows.iter().map(lookup_table_binding_from_row).collect()
    }

    pub async fn transition_lookup_table_vault_binding(
        &self,
        binding_id: i64,
        expected: LookupTableBindingLifecycle,
        next: LookupTableBindingLifecycle,
        observed_slot: Option<i64>,
    ) -> Result<LookupTableVaultBindingRecord, OrchestratorError> {
        expected.transition_to(next).map_err(domain_store_error)?;
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_vault_bindings
            SET lifecycle_state = $3,
                active_from_slot = CASE WHEN $3 = 'active' THEN COALESCE(active_from_slot, $4) ELSE active_from_slot END,
                active_until_slot = CASE WHEN $3 = 'retired' THEN COALESCE(active_until_slot, $4) ELSE active_until_slot END,
                activated_at = CASE WHEN $3 = 'active' THEN COALESCE(activated_at, now()) ELSE activated_at END,
                deactivated_at = CASE WHEN $3 = 'retired' THEN COALESCE(deactivated_at, now()) ELSE deactivated_at END,
                updated_at = now()
            WHERE id = $1 AND lifecycle_state = $2
            RETURNING *
            "#,
        )
        .bind(binding_id)
        .bind(expected.as_str())
        .bind(next.as_str())
        .bind(observed_slot)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| stale_store_update("lookup-table binding", binding_id))?;
        lookup_table_binding_from_row(&row)
    }
}

#[cfg(test)]
mod reusable_alt_tests {
    use super::*;
    use chrono::Duration;

    fn addresses(values: &[&str]) -> BTreeSet<String> {
        values.iter().map(|value| (*value).to_owned()).collect()
    }

    fn ordered_addresses(values: &[&str]) -> Vec<String> {
        values.iter().map(|value| (*value).to_owned()).collect()
    }

    fn packed_policy() -> PackedShardPolicy {
        PackedShardPolicy {
            hard_capacity: 16,
            largest_atomic_expansion: 2,
            safety_margin: 2,
            per_vault_growth_reservation: 2,
            max_vault_cohort: 2,
        }
    }

    fn packed_candidate() -> PackedShardCandidate {
        PackedShardCandidate {
            table_id: 10,
            family_id: 3,
            generation: 2,
            shard_index: 0,
            confirmed_addresses: addresses(&["a", "b"]),
            pending_addresses: addresses(&["c"]),
            reserved_address_count: 5,
            allocation_high_water: 12,
            bound_vault_count: 1,
            acceptance: LookupTableAllocationAcceptance::Accepting,
            lifecycle: LookupTableLifecycle::Active,
        }
    }

    fn allocation_request() -> PackedVaultAllocationRequest {
        PackedVaultAllocationRequest {
            vault_id: VaultId(7),
            manifest_id: 8,
            desired_addresses: addresses(&["b", "c", "d"]),
            current_table_id: None,
            current_reserved_capacity: None,
            next_generation: 3,
            next_shard_index: 1,
        }
    }

    #[test]
    fn reusable_alt_shared_planner_exact_capacity_and_one_over_never_truncate() {
        let exact = vec![SharedMarketRouteCohort {
            cohort_key: "exact".to_owned(),
            addresses: addresses(&["a", "b", "c", "d"]),
        }];
        let exact_plan = plan_shared_market_shards(&exact, 4).unwrap();
        assert_eq!(exact_plan.len(), 1);
        assert_eq!(exact_plan[0].addresses.len(), 4);

        let one_over = vec![SharedMarketRouteCohort {
            cohort_key: "one-over".to_owned(),
            addresses: addresses(&["a", "b", "c", "d", "e"]),
        }];
        let plan = plan_shared_market_shards(&one_over, 4).unwrap();
        assert_eq!(plan.len(), 2);
        let planned = plan
            .iter()
            .flat_map(|shard| shard.addresses.iter().cloned())
            .collect::<BTreeSet<_>>();
        assert_eq!(planned, one_over[0].addresses);
    }

    #[test]
    fn reusable_alt_shared_append_pack_preserves_full_prefixes_and_extends_only_tail() {
        let first = ordered_addresses(&["a", "b", "c", "d", "e"]);
        let first_plan = append_pack_shared_market_shards(&first, 4).unwrap();
        assert_eq!(
            first_plan,
            vec![
                SharedMarketShardPlan {
                    shard_ordinal: 0,
                    addresses: ordered_addresses(&["a", "b", "c", "d"]),
                },
                SharedMarketShardPlan {
                    shard_ordinal: 1,
                    addresses: ordered_addresses(&["e"]),
                },
            ]
        );

        let appended = ordered_addresses(&["a", "b", "c", "d", "e", "f", "g", "h", "i"]);
        let appended_plan = append_pack_shared_market_shards(&appended, 4).unwrap();
        assert_eq!(appended_plan[0], first_plan[0]);
        assert!(appended_plan[1]
            .addresses
            .starts_with(&first_plan[1].addresses));
        assert_eq!(
            appended_plan[1].addresses,
            ordered_addresses(&["e", "f", "g", "h"])
        );
        assert_eq!(appended_plan[2].addresses, ordered_addresses(&["i"]));
        assert_eq!(
            appended_plan
                .iter()
                .flat_map(|shard| shard.addresses.iter().cloned())
                .collect::<Vec<_>>(),
            appended
        );
    }

    #[test]
    fn reusable_alt_shared_append_pack_splits_production_catalog_without_reducing_headroom() {
        let catalog = (0..237)
            .map(|index| format!("address-{index:03}"))
            .collect::<Vec<_>>();
        let plan = append_pack_shared_market_shards(&catalog, 219).unwrap();
        assert_eq!(plan.len(), 2);
        assert_eq!(plan[0].shard_ordinal, 0);
        assert_eq!(plan[0].addresses.len(), 219);
        assert_eq!(plan[1].shard_ordinal, 1);
        assert_eq!(plan[1].addresses.len(), 18);
        assert_eq!(
            plan.iter()
                .flat_map(|shard| shard.addresses.iter().cloned())
                .collect::<Vec<_>>(),
            catalog
        );
    }

    #[test]
    fn reusable_alt_shared_planner_clusters_overlap_and_is_order_independent() {
        let cohorts = vec![
            SharedMarketRouteCohort {
                cohort_key: "route-b".to_owned(),
                addresses: addresses(&["a", "b", "d"]),
            },
            SharedMarketRouteCohort {
                cohort_key: "route-a".to_owned(),
                addresses: addresses(&["a", "b", "c"]),
            },
            SharedMarketRouteCohort {
                cohort_key: "route-c".to_owned(),
                addresses: addresses(&["e", "f"]),
            },
        ];
        let mut reversed = cohorts.clone();
        reversed.reverse();
        let first = plan_shared_market_shards(&cohorts, 3).unwrap();
        let second = plan_shared_market_shards(&reversed, 3).unwrap();
        assert_eq!(first, second);
        assert!(first
            .iter()
            .any(|shard| shard.addresses.contains(&"a".to_owned())
                && shard.addresses.contains(&"b".to_owned())));
    }

    #[test]
    fn reusable_alt_rollover_target_continues_with_bounded_suffix_extensions() {
        let desired = ordered_addresses(&["a", "b", "c", "d"]);
        let confirmed = ordered_addresses(&["a", "b"]);
        let (kind, missing) =
            next_shared_market_mutation(true, &desired, &confirmed, &[], 1).unwrap();
        assert_eq!(kind, LookupTableOperationKind::Extend);
        assert_eq!(missing, vec!["c".to_owned()]);
        assert!(next_shared_market_mutation(
            true,
            &desired,
            &confirmed,
            &ordered_addresses(&["c"]),
            1,
        )
        .is_none());
        let (kind, _) = next_shared_market_mutation(false, &desired, &[], &[], 1).unwrap();
        assert_eq!(kind, LookupTableOperationKind::Create);
    }

    #[test]
    fn reusable_alt_shared_order_requires_an_exact_prefix() {
        let desired = ordered_addresses(&["a", "b", "c"]);
        assert!(ordered_confirmed_and_pending_match(
            &ordered_addresses(&["a"]),
            &ordered_addresses(&["b"]),
            &desired,
        ));
        assert!(!ordered_confirmed_and_pending_match(
            &ordered_addresses(&["b", "c"]),
            &[],
            &desired,
        ));
        assert!(!ordered_confirmed_and_pending_match(
            &ordered_addresses(&["a", "c"]),
            &[],
            &desired,
        ));
        assert!(next_shared_market_mutation(
            true,
            &desired,
            &ordered_addresses(&["b", "c"]),
            &[],
            1,
        )
        .is_none());
    }

    #[test]
    fn reusable_alt_shared_insertion_before_existing_prefix_forces_replacement() {
        let original = ordered_addresses(&["b", "c"]);
        let inserted_before = ordered_addresses(&["a", "b", "c"]);
        let removed_or_reordered = ordered_addresses(&["c"]);
        assert!(!ordered_confirmed_and_pending_match(
            &original,
            &[],
            &inserted_before,
        ));
        assert!(!ordered_confirmed_and_pending_match(
            &original,
            &[],
            &removed_or_reordered,
        ));
    }

    #[test]
    fn reusable_alt_shared_append_stable_catalog_extends_same_generation() {
        let confirmed = ordered_addresses(&["b", "c"]);
        let append_stable_catalog = ordered_addresses(&["b", "c", "a"]);
        let (kind, missing) =
            next_shared_market_mutation(true, &append_stable_catalog, &confirmed, &[], 20)
                .expect("append-stable catalog should extend its durable physical prefix");
        assert_eq!(kind, LookupTableOperationKind::Extend);
        assert_eq!(missing, vec!["a".to_owned()]);
    }

    #[test]
    fn reusable_alt_allocator_counts_pending_and_full_binding_reservation_once() {
        let allocation = allocate_packed_vault_manifest(
            &allocation_request(),
            &[packed_candidate()],
            packed_policy(),
        )
        .unwrap();
        assert_eq!(
            allocation,
            PackedVaultAllocation::ReserveExistingShard {
                table_id: 10,
                family_id: 3,
                missing_addresses: vec!["d".to_owned()],
                reserved_capacity: 5,
                reservation_delta: 5,
                projected_occupied: 4,
                projected_capacity_commitment: 10,
            }
        );

        let mut expanding = allocation_request();
        expanding.desired_addresses = addresses(&["a", "b", "c", "d"]);
        expanding.current_table_id = Some(10);
        expanding.current_reserved_capacity = Some(4);
        let allocation =
            allocate_packed_vault_manifest(&expanding, &[packed_candidate()], packed_policy())
                .unwrap();
        assert!(matches!(
            allocation,
            PackedVaultAllocation::ReserveExistingShard {
                reservation_delta: 2,
                projected_capacity_commitment: 7,
                ..
            }
        ));

        let mut retry = allocation_request();
        retry.current_table_id = Some(10);
        retry.current_reserved_capacity = Some(5);
        let allocation =
            allocate_packed_vault_manifest(&retry, &[packed_candidate()], packed_policy()).unwrap();
        assert!(matches!(
            allocation,
            PackedVaultAllocation::ReserveExistingShard {
                table_id: 10,
                reservation_delta: 0,
                ..
            }
        ));
    }

    #[test]
    fn reusable_alt_allocator_best_fits_equal_overlap_into_fuller_shard() {
        let fuller = packed_candidate();
        let mut emptier = fuller.clone();
        emptier.table_id = 20;
        emptier.shard_index = 1;
        emptier.reserved_address_count = 2;
        emptier.bound_vault_count = 0;

        let allocation = allocate_packed_vault_manifest(
            &allocation_request(),
            &[emptier, fuller],
            packed_policy(),
        )
        .unwrap();

        assert!(matches!(
            allocation,
            PackedVaultAllocation::ReserveExistingShard {
                table_id: 10,
                projected_capacity_commitment: 10,
                ..
            }
        ));
    }

    #[test]
    fn reusable_alt_allocator_never_partially_places_and_respects_cohort_cap() {
        let request = allocation_request();
        let mut full = packed_candidate();
        full.reserved_address_count = 10;
        let allocation =
            allocate_packed_vault_manifest(&request, &[full], packed_policy()).unwrap();
        assert!(matches!(
            allocation,
            PackedVaultAllocation::PrepareNewShard {
                ref desired_addresses,
                dedicated: false,
                ..
            } if desired_addresses == &vec!["b".to_owned(), "c".to_owned(), "d".to_owned()]
        ));

        let mut cohort_full = packed_candidate();
        cohort_full.bound_vault_count = 2;
        assert!(matches!(
            allocate_packed_vault_manifest(&request, &[cohort_full], packed_policy()).unwrap(),
            PackedVaultAllocation::PrepareNewShard { .. }
        ));
    }

    #[test]
    fn reusable_alt_allocator_rejects_manifest_plus_growth_over_hard_capacity() {
        let mut request = allocation_request();
        request.desired_addresses = (0..15).map(|index| format!("a{index:02}")).collect();
        assert!(matches!(
            allocate_packed_vault_manifest(&request, &[], packed_policy()),
            Err(LookupTableDomainError::ManifestExceedsHardCapacity {
                required: 17,
                hard_capacity: 16
            })
        ));
    }

    #[test]
    fn reusable_alt_state_transitions_reject_illegal_skips() {
        assert_eq!(
            LookupTableLifecycle::Warming
                .transition_to(LookupTableLifecycle::Active)
                .unwrap(),
            LookupTableLifecycle::Active
        );
        assert!(LookupTableLifecycle::Closed
            .transition_to(LookupTableLifecycle::Active)
            .is_err());
        assert!(LookupTableOperationStatus::Queued
            .transition_to(LookupTableOperationStatus::Submitted)
            .is_err());
        assert!(LookupTableBindingLifecycle::Active
            .transition_to(LookupTableBindingLifecycle::Standby)
            .is_ok());
    }

    #[test]
    fn reusable_alt_operation_idempotency_is_address_order_independent() {
        let mut first = LookupTableOperationIntent {
            cluster: "mainnet-beta".to_owned(),
            family_id: 3,
            table_id: Some(10),
            kind: LookupTableOperationKind::Extend,
            generation: 2,
            shard_index: 0,
            mutation_epoch: 0,
            desired_address_hash: "manifest".to_owned(),
            addresses: vec!["b".to_owned(), "a".to_owned(), "a".to_owned()],
        };
        let first_key = first.idempotency_key();
        first.addresses.reverse();
        assert_eq!(first_key, first.idempotency_key());
        first.desired_address_hash = "changed".to_owned();
        assert_ne!(first_key, first.idempotency_key());
        first.desired_address_hash = "manifest".to_owned();
        first.mutation_epoch = 1;
        assert_ne!(first_key, first.idempotency_key());
    }

    #[test]
    fn reusable_alt_fencing_rejects_stale_owner_token_and_expiry() {
        let now = Utc::now();
        let lease =
            LookupTableOperationLease::new("worker-a", 4, now + Duration::seconds(30)).unwrap();
        assert!(lease.authorizes("worker-a", 4, now));
        assert!(!lease.authorizes("worker-b", 4, now));
        assert!(!lease.authorizes("worker-a", 3, now));
        assert!(!lease.authorizes("worker-a", 4, now + Duration::seconds(31)));
    }

    #[test]
    fn reusable_alt_crash_recovery_uses_finalized_chain_effect_without_blind_replay() {
        let observation = LookupTableReconciliationObservation {
            operation_kind: LookupTableOperationKind::Extend,
            persisted_status: LookupTableOperationStatus::Signed,
            signature_state: LookupTableSignatureState::Finalized,
            chain_state: LookupTableChainState::ExactMatch,
            chain_observed_finalized: true,
            blockhash_expired: true,
            usable_after_slot_reached: true,
        };
        assert_eq!(
            reconcile_lookup_table_operation(&observation),
            LookupTableReconciliationDecision::AdvanceTo(LookupTableOperationStatus::Reconciled)
        );
        let mut absent = observation.clone();
        absent.signature_state = LookupTableSignatureState::NotFound;
        absent.chain_state = LookupTableChainState::Missing;
        assert_eq!(
            reconcile_lookup_table_operation(&absent),
            LookupTableReconciliationDecision::RetryWithFreshTransaction
        );
        let unattributed = LookupTableReconciliationObservation {
            signature_state: LookupTableSignatureState::NotFound,
            chain_state: LookupTableChainState::ExactMatch,
            ..observation
        };
        assert_eq!(
            reconcile_lookup_table_operation(&unattributed),
            LookupTableReconciliationDecision::NeedsManualReconcile {
                reason: "lookup-table mutation exists but its persisted signature was not found, so spend cannot be attributed safely",
            }
        );
    }

    #[test]
    fn reusable_alt_verify_missing_state_never_waits_for_an_impossible_signature() {
        let observation = LookupTableReconciliationObservation {
            operation_kind: LookupTableOperationKind::Verify,
            persisted_status: LookupTableOperationStatus::Leased,
            signature_state: LookupTableSignatureState::Unknown,
            chain_state: LookupTableChainState::Missing,
            chain_observed_finalized: true,
            blockhash_expired: false,
            usable_after_slot_reached: true,
        };

        assert_eq!(
            reconcile_lookup_table_operation(&observation),
            LookupTableReconciliationDecision::NeedsManualReconcile {
                reason: "lookup-table verification did not find the expected finalized table state",
            }
        );
    }

    #[test]
    fn reusable_alt_finalized_signature_waits_for_a_nonstale_physical_context() {
        let stale_physical_observation = LookupTableReconciliationObservation {
            operation_kind: LookupTableOperationKind::Extend,
            persisted_status: LookupTableOperationStatus::Submitted,
            signature_state: LookupTableSignatureState::Finalized,
            chain_state: LookupTableChainState::Missing,
            chain_observed_finalized: false,
            blockhash_expired: true,
            usable_after_slot_reached: true,
        };
        assert_eq!(
            reconcile_lookup_table_operation(&stale_physical_observation),
            LookupTableReconciliationDecision::WaitForFinalization
        );
        let stale_prefix = LookupTableReconciliationObservation {
            chain_state: LookupTableChainState::PrefixDrift,
            ..stale_physical_observation.clone()
        };
        assert_eq!(
            reconcile_lookup_table_operation(&stale_prefix),
            LookupTableReconciliationDecision::WaitForFinalization
        );
        let caught_up = LookupTableReconciliationObservation {
            chain_state: LookupTableChainState::ExactMatch,
            chain_observed_finalized: true,
            ..stale_physical_observation
        };
        assert_eq!(
            reconcile_lookup_table_operation(&caught_up),
            LookupTableReconciliationDecision::AdvanceTo(LookupTableOperationStatus::Reconciled)
        );
    }

    #[test]
    fn reusable_alt_unsigned_chain_mutation_requires_manual_reconcile() {
        let observation = LookupTableReconciliationObservation {
            operation_kind: LookupTableOperationKind::Create,
            persisted_status: LookupTableOperationStatus::Leased,
            signature_state: LookupTableSignatureState::Unknown,
            chain_state: LookupTableChainState::ExactMatch,
            chain_observed_finalized: true,
            blockhash_expired: false,
            usable_after_slot_reached: true,
        };
        assert_eq!(
            reconcile_lookup_table_operation(&observation),
            LookupTableReconciliationDecision::NeedsManualReconcile {
                reason: "unsigned lookup-table mutation appeared on chain outside the durable transaction boundary",
            }
        );

        let verify = LookupTableReconciliationObservation {
            operation_kind: LookupTableOperationKind::Verify,
            ..observation
        };
        assert_eq!(
            reconcile_lookup_table_operation(&verify),
            LookupTableReconciliationDecision::AdvanceTo(LookupTableOperationStatus::Reconciled)
        );
    }

    fn resolver_candidate(id: i64, values: &[&str], rpc_verified: bool) -> ResolverTableCandidate {
        let ordered = values
            .iter()
            .map(|value| (*value).to_owned())
            .collect::<Vec<_>>();
        ResolverTableCandidate {
            table_id: id,
            table_address: format!("table-{id}"),
            expected_authority: "authority".to_owned(),
            family_id: Some(3),
            allocation_kind: Some(LookupTableAllocationKind::VaultShard),
            generation: 1,
            shard_index: id as i32,
            ordered_usable_prefix: ordered.clone(),
            ordered_durable_addresses: ordered.clone(),
            addresses: ordered.iter().cloned().collect(),
            usable_prefix_len: ordered.len() as u16,
            address_hash: ordered_address_hash(&ordered),
            mutation_epoch: 2,
            last_verified_slot: Some(100),
            lifecycle: LookupTableLifecycle::Active,
            persisted_prefix_verified: true,
            rpc_verified,
            usable: true,
        }
    }

    #[test]
    fn reusable_alt_resolver_requires_fresh_rpc_and_selects_minimal_bundle() {
        let required = addresses(&["a", "b"]);
        let candidates = vec![
            resolver_candidate(1, &["a"], false),
            resolver_candidate(2, &["b"], false),
            resolver_candidate(3, &["a", "b"], false),
        ];
        let (selected, missing) = minimal_verified_table_bundle(&required, &candidates, 8).unwrap();
        assert!(missing.is_empty());
        assert_eq!(selected.len(), 1);
        assert_eq!(selected[0].table_id, 3);
        let not_rpc_verified = ResolvedLookupTableBundle {
            tables: selected,
            required_addresses: required,
            missing_addresses: BTreeSet::new(),
            packet_fits: true,
            simulation_succeeded: true,
        };
        assert!(!not_rpc_verified.ready());
    }

    #[test]
    fn reusable_alt_resolver_selects_only_contributing_shared_shards() {
        let required = addresses(&["shared-a", "shared-z", "vault"]);
        let mut first_shared = resolver_candidate(1, &["shared-a", "unused-a"], true);
        first_shared.allocation_kind = Some(LookupTableAllocationKind::SharedMarket);
        let mut second_shared = resolver_candidate(2, &["shared-z", "unused-z"], true);
        second_shared.allocation_kind = Some(LookupTableAllocationKind::SharedMarket);
        let mut irrelevant_shared = resolver_candidate(3, &["unused-only"], true);
        irrelevant_shared.allocation_kind = Some(LookupTableAllocationKind::SharedMarket);
        let vault = resolver_candidate(4, &["vault"], true);
        let (selected, missing) = minimal_verified_table_bundle(
            &required,
            &[first_shared, second_shared, irrelevant_shared, vault],
            8,
        )
        .unwrap();
        assert!(missing.is_empty());
        assert_eq!(
            selected
                .iter()
                .map(|table| table.table_id)
                .collect::<BTreeSet<_>>(),
            BTreeSet::from([1, 2, 4])
        );
    }

    #[test]
    fn reusable_alt_resolver_does_not_apply_exponential_bound_to_disjoint_shared_shards() {
        let mut required = BTreeSet::new();
        let mut candidates = Vec::new();
        for index in 0..21 {
            let address = format!("shared-{index}");
            required.insert(address.clone());
            let mut candidate = resolver_candidate(i64::from(index), &[address.as_str()], true);
            candidate.allocation_kind = Some(LookupTableAllocationKind::SharedMarket);
            candidates.push(candidate);
        }
        required.insert("vault".to_owned());
        candidates.push(resolver_candidate(100, &["vault"], true));

        let (selected, missing) = minimal_verified_table_bundle(&required, &candidates, 1).unwrap();
        assert!(missing.is_empty());
        assert_eq!(selected.len(), 22);
        assert_eq!(
            selected
                .iter()
                .filter(|candidate| {
                    candidate.allocation_kind == Some(LookupTableAllocationKind::SharedMarket)
                })
                .count(),
            21
        );
    }

    #[test]
    fn reusable_alt_resolver_bounds_only_route_relevant_candidates() {
        let required = addresses(&["a", "b"]);
        let mut candidates = (0..20)
            .map(|index| resolver_candidate(index, &[&format!("irrelevant-{index}")], true))
            .collect::<Vec<_>>();
        candidates.push(resolver_candidate(100, &["a"], true));
        candidates.push(resolver_candidate(101, &["b"], true));
        let (relevant, missing) =
            persisted_relevant_table_candidates(&required, &candidates, 2).unwrap();
        assert_eq!(relevant.len(), 2);
        assert!(missing.is_empty());

        candidates.push(resolver_candidate(102, &["a", "b"], true));
        assert!(matches!(
            persisted_relevant_table_candidates(&required, &candidates, 2),
            Err(LookupTableDomainError::TooManyResolverCandidates {
                actual: 3,
                limit: 2
            })
        ));
    }

    #[test]
    fn reusable_alt_membership_rejects_noncontiguous_or_nonprefix_warmup() {
        let now = Utc::now();
        let invalid = vec![
            LookupTableMembershipAddress {
                address: "a".to_owned(),
                ordinal: 0,
                added_operation_id: None,
                added_slot: 10,
                usable_after_slot: 12,
                last_verified_slot: 12,
                last_verified_at: now,
            },
            LookupTableMembershipAddress {
                address: "b".to_owned(),
                ordinal: 2,
                added_operation_id: None,
                added_slot: 10,
                usable_after_slot: 10,
                last_verified_slot: 12,
                last_verified_at: now,
            },
        ];
        assert!(validate_membership(&invalid, 11).is_err());
    }

    #[test]
    fn reusable_alt_manifest_rejects_duplicate_addresses_before_db_write() {
        let input = LookupTableManifestWrite {
            family_id: 1,
            subject_kind: LookupTableManifestSubject::SharedMarket,
            subject_key: "stable".to_owned(),
            vault_id: None,
            desired_set_hash: "hash".to_owned(),
            source_slot: Some(10),
            planner_version: "planner".to_owned(),
            catalog_version: "catalog".to_owned(),
            addresses: vec![
                LookupTableManifestAddressRecord {
                    address: "duplicate".to_owned(),
                    ordinal: 0,
                    semantic_class: LookupTableManifestSubject::SharedMarket,
                    account_role: "reserve".to_owned(),
                    is_writable: false,
                },
                LookupTableManifestAddressRecord {
                    address: "duplicate".to_owned(),
                    ordinal: 1,
                    semantic_class: LookupTableManifestSubject::SharedMarket,
                    account_role: "oracle".to_owned(),
                    is_writable: false,
                },
            ],
        };
        assert!(validate_manifest_write(&input).is_err());
    }

    #[test]
    fn reusable_alt_concurrent_reservation_snapshots_allow_only_one_last_slot_winner() {
        let request = allocation_request();
        let first =
            allocate_packed_vault_manifest(&request, &[packed_candidate()], packed_policy())
                .unwrap();
        assert!(matches!(
            first,
            PackedVaultAllocation::ReserveExistingShard { table_id: 10, .. }
        ));

        let mut after_first_commit = packed_candidate();
        after_first_commit.reserved_address_count = 10;
        after_first_commit.bound_vault_count = 2;
        after_first_commit.pending_addresses.insert("d".to_owned());
        let second = allocate_packed_vault_manifest(
            &PackedVaultAllocationRequest {
                vault_id: VaultId(9),
                manifest_id: 10,
                desired_addresses: addresses(&["e", "f", "g"]),
                current_table_id: None,
                current_reserved_capacity: None,
                next_generation: 3,
                next_shard_index: 1,
            },
            &[after_first_commit],
            packed_policy(),
        )
        .unwrap();
        assert!(matches!(
            second,
            PackedVaultAllocation::PrepareNewShard { .. }
        ));
    }

    #[test]
    fn reusable_alt_relocation_keeps_complete_manifest_and_prepares_successor() {
        let mut full_current = packed_candidate();
        full_current.reserved_address_count = 12;
        let mut successor = packed_candidate();
        successor.table_id = 20;
        successor.shard_index = 1;
        successor.confirmed_addresses.clear();
        successor.pending_addresses.clear();
        successor.reserved_address_count = 5;
        let request = PackedVaultAllocationRequest {
            vault_id: VaultId(7),
            manifest_id: 11,
            desired_addresses: addresses(&["a", "b", "c", "d"]),
            current_table_id: Some(10),
            current_reserved_capacity: Some(5),
            next_generation: 3,
            next_shard_index: 2,
        };
        let allocation =
            allocate_packed_vault_manifest(&request, &[full_current, successor], packed_policy())
                .unwrap();
        assert!(matches!(
            allocation,
            PackedVaultAllocation::ReserveExistingShard {
                table_id: 20,
                ref missing_addresses,
                ..
            } if missing_addresses == &vec!["a".to_owned(), "b".to_owned(), "c".to_owned(), "d".to_owned()]
        ));
    }

    #[test]
    fn reusable_alt_active_packed_head_expands_in_place_with_headroom() {
        let mut active = packed_candidate();
        active.confirmed_addresses = addresses(&["a", "b"]);
        active.pending_addresses.clear();
        active.reserved_address_count = 4;
        let allocation = allocate_packed_vault_manifest(
            &PackedVaultAllocationRequest {
                vault_id: VaultId(7),
                manifest_id: 12,
                desired_addresses: addresses(&["a", "b", "c"]),
                current_table_id: Some(active.table_id),
                current_reserved_capacity: Some(4),
                next_generation: 3,
                next_shard_index: 2,
            },
            &[active],
            packed_policy(),
        )
        .unwrap();
        assert!(matches!(
            allocation,
            PackedVaultAllocation::ReserveExistingShard {
                table_id: 10,
                ref missing_addresses,
                reservation_delta: 1,
                ..
            } if missing_addresses == &vec!["c".to_owned()]
        ));
    }

    #[test]
    fn reusable_alt_manifest_change_never_grows_the_rollback_predecessor() {
        let mut predecessor = packed_candidate();
        predecessor.acceptance = LookupTableAllocationAcceptance::Sealed;
        predecessor.confirmed_addresses = addresses(&["a", "b", "c"]);
        predecessor.pending_addresses.clear();
        let mut successor = packed_candidate();
        successor.table_id = 20;
        successor.shard_index = 1;
        successor.confirmed_addresses.clear();
        successor.pending_addresses.clear();
        successor.reserved_address_count = 0;
        successor.bound_vault_count = 0;
        let allocation = allocate_packed_vault_manifest(
            &PackedVaultAllocationRequest {
                vault_id: VaultId(7),
                manifest_id: 99,
                desired_addresses: addresses(&["a", "b", "c", "d"]),
                current_table_id: None,
                current_reserved_capacity: None,
                next_generation: 2,
                next_shard_index: 2,
            },
            &[predecessor, successor],
            packed_policy(),
        )
        .unwrap();
        assert!(matches!(
            allocation,
            PackedVaultAllocation::ReserveExistingShard {
                table_id: 20,
                ref missing_addresses,
                ..
            } if missing_addresses == &vec!["a".to_owned(), "b".to_owned(), "c".to_owned(), "d".to_owned()]
        ));
    }

    #[test]
    fn reusable_alt_prepare_new_shard_reserves_binding_and_unique_create_identity() {
        let now = Utc::now();
        let authority = Pubkey::new_unique();
        let family = LookupTableFamilyRecord {
            id: 3,
            cluster: "mainnet-beta".to_owned(),
            logical_name: "vault-shards".to_owned(),
            kind: LookupTableFamilyKind::VaultShards,
            desired_state: LookupTableFamilyState::Active,
            planner_version: "planner-v1".to_owned(),
            catalog_version: "catalog-v1".to_owned(),
            active_generation: Some(2),
            previous_generation: Some(1),
            rollback_until: Some(now + Duration::minutes(5)),
            provisioning_authority: authority.to_string(),
            payer: authority.to_string(),
            hard_capacity: 256,
            largest_atomic_expansion: 24,
            safety_margin: 8,
            allocation_high_water: 224,
            created_at: now,
            updated_at: now,
        };
        let allocation = PackedVaultAllocation::PrepareNewShard {
            generation: 2,
            shard_index: 4,
            desired_addresses: vec![Pubkey::new_unique().to_string()],
            reserved_capacity: 17,
            allocation_high_water: 224,
            dedicated: false,
        };
        let first_address = derive_lookup_table_address(&authority, 100).0.to_string();
        let reservation = prepare_new_shard_reservation(
            &family,
            &allocation,
            serde_json::json!({"recent_slot": 100}),
            &BTreeSet::from([first_address]),
        )
        .unwrap();
        assert_eq!(
            reservation.table_address,
            derive_lookup_table_address(&authority, 99).0.to_string()
        );
        assert_eq!(
            reservation.binding_mode,
            LookupTableBindingMode::PackedShard
        );
        assert_eq!(reservation.reserved_capacity, 17);
        assert_eq!(reservation.operation_kind, LookupTableOperationKind::Create);
        assert_eq!(reservation.operation_context["recent_slot"], 99);
    }

    #[test]
    fn reusable_alt_zero_class_is_satisfied_without_a_physical_table() {
        assert!(plan_shared_market_shards(
            &[SharedMarketRouteCohort {
                cohort_key: "empty".to_owned(),
                addresses: BTreeSet::new(),
            }],
            224,
        )
        .unwrap()
        .is_empty());
        assert!(provisioning_request_is_satisfied(
            true,
            0,
            0,
            &AtomicVaultAllocationResult::NotRequired,
        ));
        assert!(!provisioning_request_is_satisfied(
            true,
            0,
            1,
            &AtomicVaultAllocationResult::NotRequired,
        ));
    }

    #[test]
    fn reusable_alt_family_bootstrap_retry_ignores_live_pointers_and_rejects_drift() {
        let now = Utc::now();
        let authority = Pubkey::new_unique().to_string();
        let family = LookupTableFamilyRecord {
            id: 5,
            cluster: "mainnet-beta".to_owned(),
            logical_name: "shared".to_owned(),
            kind: LookupTableFamilyKind::SharedMarket,
            desired_state: LookupTableFamilyState::Retiring,
            planner_version: "planner-v1".to_owned(),
            catalog_version: "catalog-v1".to_owned(),
            active_generation: Some(9),
            previous_generation: Some(8),
            rollback_until: Some(now + Duration::minutes(5)),
            provisioning_authority: authority.clone(),
            payer: authority.clone(),
            hard_capacity: 256,
            largest_atomic_expansion: 24,
            safety_margin: 8,
            allocation_high_water: 224,
            created_at: now,
            updated_at: now,
        };
        let input = LookupTableFamilyUpsert {
            cluster: family.cluster.clone(),
            logical_name: family.logical_name.clone(),
            kind: family.kind,
            desired_state: LookupTableFamilyState::Active,
            planner_version: family.planner_version.clone(),
            catalog_version: family.catalog_version.clone(),
            active_generation: Some(1),
            previous_generation: None,
            rollback_until: None,
            provisioning_authority: authority.clone(),
            payer: authority,
            hard_capacity: 256,
            largest_atomic_expansion: 24,
            safety_margin: 8,
            allocation_high_water: 224,
        };
        assert!(validate_lookup_table_family_bootstrap(&family, &input).is_ok());
        let mut conflicting = input.clone();
        conflicting.provisioning_authority = Pubkey::new_unique().to_string();
        assert!(validate_lookup_table_family_bootstrap(&family, &conflicting).is_err());
        let mut conflicting = input;
        conflicting.catalog_version = "catalog-v2".to_owned();
        assert!(validate_lookup_table_family_bootstrap(&family, &conflicting).is_err());
    }
}

impl NeonSqlClient {
    /// Shares immutable family policy and locks candidate rows, re-runs
    /// allocation from durable reservations, and writes the binding plus one
    /// exact-transaction outbox operation before releasing the transaction.
    pub async fn allocate_vault_binding_and_queue_operation(
        &self,
        request: AtomicVaultAllocationRequest,
    ) -> Result<AtomicVaultAllocationResult, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        let result = self
            .allocate_vault_binding_and_queue_operation_in_connection(&mut *tx, request)
            .await?;
        tx.commit().await?;
        Ok(result)
    }

    async fn allocate_vault_binding_and_queue_operation_in_connection(
        &self,
        tx: &mut sqlx::PgConnection,
        request: AtomicVaultAllocationRequest,
    ) -> Result<AtomicVaultAllocationResult, OrchestratorError> {
        if request.max_extension_addresses == 0 {
            return Err(OrchestratorError::StoreInvariant(
                "max extension addresses must be positive".to_owned(),
            ));
        }
        let family_row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 AND cluster = $2 FOR SHARE",
        )
        .bind(request.family_id)
        .bind(&request.cluster)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("vault-shards family", request.family_id))?;
        let family = lookup_table_family_from_row(&family_row)?;
        if family.kind != LookupTableFamilyKind::VaultShards
            || family.desired_state != LookupTableFamilyState::Active
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table family {} is not an active vault-shards family",
                family.id
            )));
        }
        let durable_hard_capacity = u16::try_from(family.hard_capacity).map_err(|_| {
            OrchestratorError::StoreInvariant(format!(
                "lookup-table family {} has invalid durable hard capacity",
                family.id
            ))
        })?;
        let durable_largest_expansion =
            u16::try_from(family.largest_atomic_expansion).map_err(|_| {
                OrchestratorError::StoreInvariant(format!(
                    "lookup-table family {} has invalid durable largest expansion",
                    family.id
                ))
            })?;
        let durable_safety_margin = u16::try_from(family.safety_margin).map_err(|_| {
            OrchestratorError::StoreInvariant(format!(
                "lookup-table family {} has invalid durable safety margin",
                family.id
            ))
        })?;
        if request.policy.hard_capacity != durable_hard_capacity
            || request.policy.largest_atomic_expansion != durable_largest_expansion
            || request.policy.safety_margin != durable_safety_margin
            || request
                .policy
                .high_water_mark()
                .map_err(domain_store_error)?
                != u16::try_from(family.allocation_high_water).map_err(|_| {
                    OrchestratorError::StoreInvariant(format!(
                        "lookup-table family {} has invalid durable high-water mark",
                        family.id
                    ))
                })?
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table family {} planner capacity policy drifted from durable configuration",
                family.id
            )));
        }
        let manifest_row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_manifests
            WHERE id = $1 AND family_id = $2 AND vault_id = $3
              AND subject_kind = 'vault' AND sealed_at IS NOT NULL
            FOR SHARE
            "#,
        )
        .bind(request.manifest_id)
        .bind(request.family_id)
        .bind(request.vault_id.as_i64())
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("sealed vault manifest", request.manifest_id))?;
        let desired_set_hash: String = manifest_row.try_get("desired_set_hash")?;
        let manifest_addresses = sqlx::query_scalar::<_, String>(
            "SELECT address FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id = $1 ORDER BY ordinal",
        )
        .bind(request.manifest_id)
        .fetch_all(&mut *tx)
        .await?
        .into_iter()
        .collect::<BTreeSet<_>>();
        if manifest_addresses != request.desired_addresses {
            return Err(OrchestratorError::StoreInvariant(
                "allocator request does not exactly match the sealed vault manifest".to_owned(),
            ));
        }
        let required_single_binding_capacity = request
            .desired_addresses
            .len()
            .saturating_add(usize::from(request.policy.per_vault_growth_reservation));
        if required_single_binding_capacity > usize::from(durable_hard_capacity) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "vault manifest requires {required_single_binding_capacity} addresses including growth reserve, exceeding the single-binding ALT capacity {durable_hard_capacity}; multi-binding partitioning is not yet atomic across route readiness and usage leases"
            )));
        }
        let desired_head_revision = upsert_vault_desired_head_in_tx(
            tx,
            request.family_id,
            request.vault_id,
            request.binding_ordinal,
            request.manifest_id,
        )
        .await?;
        supersede_stale_vault_binding_revisions_in_tx(
            tx,
            request.family_id,
            request.vault_id,
            request.binding_ordinal,
            request.manifest_id,
            desired_head_revision,
        )
        .await?;
        if request.desired_addresses.is_empty() {
            return Ok(AtomicVaultAllocationResult::NotRequired);
        }

        let active_binding_row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_vault_bindings
            WHERE vault_id = $1 AND family_id = $2 AND binding_ordinal = $3
              AND lifecycle_state = 'active'
            FOR UPDATE
            "#,
        )
        .bind(request.vault_id.as_i64())
        .bind(request.family_id)
        .bind(request.binding_ordinal)
        .fetch_optional(&mut *tx)
        .await?;
        let active_binding = active_binding_row
            .as_ref()
            .map(lookup_table_binding_from_row)
            .transpose()?;
        let mut relocate_active_table_id = None;
        if let Some(binding) = &active_binding {
            if binding.manifest_id == request.manifest_id
                && binding.desired_head_revision == desired_head_revision
            {
                match classify_active_vault_binding_in_connection(
                    tx,
                    &family,
                    binding,
                    &request.desired_addresses,
                )
                .await?
                {
                    ActiveVaultBindingDisposition::Ready => {
                        return Ok(AtomicVaultAllocationResult::Existing {
                            binding: binding.clone(),
                        });
                    }
                    ActiveVaultBindingDisposition::Verify {
                        table,
                        persisted_addresses,
                    } => {
                        if let Some(operation) = terminal_lookup_table_binding_operation_in_tx(
                            tx,
                            binding.id,
                            request.manifest_id,
                            table.id,
                        )
                        .await?
                        {
                            return Ok(AtomicVaultAllocationResult::BindingReserved {
                                allocation: PackedVaultAllocation::KeepExisting {
                                    table_id: table.id,
                                },
                                binding: binding.clone(),
                                operations: vec![operation],
                            });
                        }
                        let pending_rows = sqlx::query(
                            r#"
                            SELECT * FROM loyal_yield.lookup_table_operations
                            WHERE route_lookup_table_id = $1
                              AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
                            ORDER BY created_at, id
                            "#,
                        )
                        .bind(table.id)
                        .fetch_all(&mut *tx)
                        .await?;
                        let mut operations = pending_rows
                            .iter()
                            .map(lookup_table_operation_from_row)
                            .collect::<Result<Vec<_>, _>>()?;
                        if operations.is_empty() {
                            let verification_key = hash_length_prefixed_values([
                                "vault-active-physical-verify",
                                request.cluster.as_str(),
                                &table.id.to_string(),
                                &table.mutation_epoch.to_string(),
                                &binding.id.to_string(),
                                &binding.desired_head_revision.to_string(),
                                &table.last_verified_slot.unwrap_or(-1).to_string(),
                                &ordered_address_hash(&persisted_addresses),
                            ]);
                            operations.push(
                                enqueue_lookup_table_operation_in_tx(
                                    &mut *tx,
                                    &LookupTableOperationEnqueue {
                                        idempotency_key: verification_key,
                                        family_id: request.family_id,
                                        route_lookup_table_id: Some(table.id),
                                        manifest_id: Some(request.manifest_id),
                                        binding_id: Some(binding.id),
                                        operation_kind: LookupTableOperationKind::Verify,
                                        target_generation: None,
                                        target_shard_ordinal: None,
                                        operation_context: request.operation_context.clone(),
                                        mutation_epoch: table.mutation_epoch,
                                        estimated_fee_lamports: Some(0),
                                        estimated_rent_lamports: Some(0),
                                        addresses: Vec::new(),
                                    },
                                )
                                .await?,
                            );
                        }
                        return Ok(AtomicVaultAllocationResult::BindingReserved {
                            allocation: PackedVaultAllocation::KeepExisting { table_id: table.id },
                            binding: binding.clone(),
                            operations,
                        });
                    }
                    ActiveVaultBindingDisposition::Relocate => {
                        relocate_active_table_id = Some(binding.route_lookup_table_id);
                    }
                }
            }
        }
        let in_flight_binding_rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_vault_bindings
            WHERE vault_id = $1 AND family_id = $2 AND binding_ordinal = $3
              AND manifest_id = $4 AND lifecycle_state IN ('preparing', 'warming')
              AND desired_head_revision = $5
            ORDER BY id DESC
            FOR UPDATE
            "#,
        )
        .bind(request.vault_id.as_i64())
        .bind(request.family_id)
        .bind(request.binding_ordinal)
        .bind(request.manifest_id)
        .bind(desired_head_revision)
        .fetch_all(&mut *tx)
        .await?;
        if in_flight_binding_rows.len() > 1 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "vault {} has multiple in-flight bindings for manifest {}",
                request.vault_id.as_i64(),
                request.manifest_id
            )));
        }
        let current_reservation = in_flight_binding_rows
            .first()
            .map(lookup_table_binding_from_row)
            .transpose()?;
        if let Some(binding) = &current_reservation {
            if let Some(operation) = terminal_lookup_table_binding_operation_in_tx(
                tx,
                binding.id,
                request.manifest_id,
                binding.route_lookup_table_id,
            )
            .await?
            {
                return Ok(AtomicVaultAllocationResult::BindingReserved {
                    allocation: PackedVaultAllocation::KeepExisting {
                        table_id: binding.route_lookup_table_id,
                    },
                    binding: binding.clone(),
                    operations: vec![operation],
                });
            }
        }

        let physical_rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.route_lookup_tables
            WHERE family_id = $1
              AND allocation_kind IN ('vault_shard', 'dedicated_vault')
              AND desired_state IN ('preparing', 'warming', 'active')
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.lookup_table_operations failed_create
                  WHERE failed_create.route_lookup_table_id = route_lookup_tables.id
                    AND failed_create.operation_kind IN ('create', 'rollover')
                    AND failed_create.operation_state = 'permanent_failure'
                    AND NOT EXISTS (
                        SELECT 1
                        FROM loyal_yield.lookup_table_terminal_repair_operations repaired
                        WHERE repaired.operation_id = failed_create.id
                    )
              )
            ORDER BY generation, shard_ordinal, id
            FOR UPDATE
            "#,
        )
        .bind(request.family_id)
        .fetch_all(&mut *tx)
        .await?;
        let physical = physical_rows
            .iter()
            .map(reusable_lookup_table_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let table_ids = physical.iter().map(|table| table.id).collect::<Vec<_>>();
        let mut confirmed = BTreeMap::<i64, BTreeSet<String>>::new();
        let mut pending = BTreeMap::<i64, BTreeSet<String>>::new();
        let mut bound_counts = BTreeMap::<i64, u16>::new();
        if !table_ids.is_empty() {
            for row in sqlx::query(
                "SELECT route_lookup_table_id, address FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id = ANY($1)",
            )
            .bind(&table_ids)
            .fetch_all(&mut *tx)
            .await?
            {
                confirmed
                    .entry(row.try_get("route_lookup_table_id")?)
                    .or_default()
                    .insert(row.try_get("address")?);
            }
            for row in sqlx::query(
                r#"
                SELECT operation.route_lookup_table_id, address.address
                FROM loyal_yield.lookup_table_operations operation
                JOIN loyal_yield.lookup_table_operation_addresses address
                  ON address.operation_id = operation.id
                WHERE operation.route_lookup_table_id = ANY($1)
                  AND operation.operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
                "#,
            )
            .bind(&table_ids)
            .fetch_all(&mut *tx)
            .await?
            {
                pending
                    .entry(row.try_get("route_lookup_table_id")?)
                    .or_default()
                    .insert(row.try_get("address")?);
            }
            for row in sqlx::query(
                r#"
                SELECT route_lookup_table_id, count(DISTINCT vault_id)::bigint AS bound_count
                FROM loyal_yield.lookup_table_vault_bindings
                WHERE route_lookup_table_id = ANY($1)
                  AND lifecycle_state IN ('preparing', 'warming', 'active', 'standby', 'retiring')
                GROUP BY route_lookup_table_id
                "#,
            )
            .bind(&table_ids)
            .fetch_all(&mut *tx)
            .await?
            {
                let count: i64 = row.try_get("bound_count")?;
                bound_counts.insert(
                    row.try_get("route_lookup_table_id")?,
                    u16::try_from(count).map_err(|_| {
                        OrchestratorError::StoreInvariant(
                            "vault-shard bound count exceeds u16".to_owned(),
                        )
                    })?,
                );
            }
        }
        let candidates = physical
            .iter()
            .map(|table| PackedShardCandidate {
                table_id: table.id,
                family_id: table.family_id,
                generation: table.generation,
                shard_index: table.shard_ordinal,
                confirmed_addresses: confirmed.remove(&table.id).unwrap_or_default(),
                pending_addresses: pending.remove(&table.id).unwrap_or_default(),
                reserved_address_count: table.reserved_address_count as u16,
                allocation_high_water: table.allocation_high_water as u16,
                bound_vault_count: bound_counts.remove(&table.id).unwrap_or_default(),
                acceptance: if table.accepting_allocations
                    && (table.allocation_kind == LookupTableAllocationKind::VaultShard
                        || current_reservation
                            .as_ref()
                            .is_some_and(|binding| binding.route_lookup_table_id == table.id))
                {
                    LookupTableAllocationAcceptance::Accepting
                } else {
                    LookupTableAllocationAcceptance::Sealed
                },
                lifecycle: if relocate_active_table_id == Some(table.id) {
                    LookupTableLifecycle::Failed
                } else {
                    table.desired_state
                },
            })
            .collect::<Vec<_>>();
        let allocation_request = PackedVaultAllocationRequest {
            vault_id: request.vault_id,
            manifest_id: request.manifest_id,
            desired_addresses: request.desired_addresses.clone(),
            current_table_id: current_reservation
                .as_ref()
                .or(active_binding.as_ref())
                .map(|binding| binding.route_lookup_table_id),
            current_reserved_capacity: current_reservation
                .as_ref()
                .or(active_binding.as_ref())
                .and_then(|binding| u16::try_from(binding.reserved_capacity).ok()),
            next_generation: request.next_generation,
            next_shard_index: request.next_shard_ordinal,
        };
        let allocation =
            allocate_packed_vault_manifest(&allocation_request, &candidates, request.policy)
                .map_err(domain_store_error)?;

        if let PackedVaultAllocation::PrepareNewShard {
            desired_addresses, ..
        } = &allocation
        {
            // ALT addresses are derived only from authority + recent slot. Lock
            // the authority across families so two concurrent planners cannot
            // reserve the same derived address before either transaction commits.
            sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")
                .bind(&family.provisioning_authority)
                .execute(&mut *tx)
                .await?;
            let occupied_table_addresses = sqlx::query_scalar::<_, String>(
                "SELECT table_address FROM loyal_yield.route_lookup_tables WHERE authority = $1",
            )
            .bind(&family.provisioning_authority)
            .fetch_all(&mut *tx)
            .await?
            .into_iter()
            .collect::<BTreeSet<_>>();
            let reservation = prepare_new_shard_reservation(
                &family,
                &allocation,
                request.operation_context,
                &occupied_table_addresses,
            )?;
            let table_row = sqlx::query(
                r#"
                INSERT INTO loyal_yield.route_lookup_tables
                    (cluster, scope, table_address, authority, payer, status, durable,
                     address_count, address_hash, addresses, family_id, allocation_kind,
                     generation, shard_ordinal, desired_state, accepting_allocations,
                     allocation_high_water, reserved_address_count, usable_address_count,
                     mutation_epoch)
                VALUES ($1, $2, $3, $4, $5, 'warming', TRUE,
                        0, '', '[]'::jsonb, $6, $7, $8, $9, 'preparing', $10,
                        $11, 0, 0, 0)
                RETURNING *
                "#,
            )
            .bind(&request.cluster)
            .bind(&reservation.scope)
            .bind(&reservation.table_address)
            .bind(&family.provisioning_authority)
            .bind(&family.payer)
            .bind(request.family_id)
            .bind(reservation.allocation_kind.as_str())
            .bind(reservation.generation)
            .bind(reservation.shard_ordinal)
            .bind(reservation.accepting_allocations)
            .bind(i32::from(reservation.allocation_high_water))
            .fetch_one(&mut *tx)
            .await?;
            let table = reusable_lookup_table_from_row(&table_row)?;
            let binding_row = sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_vault_bindings
                    (vault_id, family_id, route_lookup_table_id, manifest_id,
                     binding_ordinal, desired_head_revision, allocation_mode,
                     reserved_capacity, predecessor_binding_id, lifecycle_state)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'preparing')
                RETURNING *
                "#,
            )
            .bind(request.vault_id.as_i64())
            .bind(request.family_id)
            .bind(table.id)
            .bind(request.manifest_id)
            .bind(request.binding_ordinal)
            .bind(desired_head_revision)
            .bind(reservation.binding_mode.as_str())
            .bind(i32::from(reservation.reserved_capacity))
            .bind(active_binding.as_ref().map(|binding| binding.id))
            .fetch_one(&mut *tx)
            .await?;
            let binding = lookup_table_binding_from_row(&binding_row)?;
            let chunk = desired_addresses
                .iter()
                .take(request.max_extension_addresses)
                .cloned()
                .collect::<Vec<_>>();
            let intent = LookupTableOperationIntent {
                cluster: request.cluster.clone(),
                family_id: request.family_id,
                table_id: Some(table.id),
                kind: reservation.operation_kind,
                generation: reservation.generation,
                shard_index: reservation.shard_ordinal,
                mutation_epoch: table.mutation_epoch,
                desired_address_hash: desired_set_hash.clone(),
                addresses: chunk.clone(),
            };
            let operation = enqueue_lookup_table_operation_in_tx(
                &mut *tx,
                &LookupTableOperationEnqueue {
                    idempotency_key: intent.idempotency_key(),
                    family_id: request.family_id,
                    route_lookup_table_id: Some(table.id),
                    manifest_id: Some(request.manifest_id),
                    binding_id: Some(binding.id),
                    operation_kind: reservation.operation_kind,
                    target_generation: Some(reservation.generation),
                    target_shard_ordinal: Some(reservation.shard_ordinal),
                    operation_context: reservation.operation_context,
                    mutation_epoch: 0,
                    estimated_fee_lamports: request.estimated_fee_lamports,
                    estimated_rent_lamports: request.estimated_rent_lamports,
                    addresses: chunk,
                },
            )
            .await?;
            return Ok(AtomicVaultAllocationResult::CreateQueued {
                allocation,
                binding,
                operations: vec![operation],
            });
        }

        let (table_id, reserved_capacity, missing_addresses) = match &allocation {
            PackedVaultAllocation::KeepExisting { table_id } => (
                *table_id,
                request.desired_addresses.len() as u16
                    + request.policy.per_vault_growth_reservation,
                Vec::new(),
            ),
            PackedVaultAllocation::ReserveExistingShard {
                table_id,
                reserved_capacity,
                missing_addresses,
                ..
            } => (*table_id, *reserved_capacity, missing_addresses.clone()),
            PackedVaultAllocation::PrepareNewShard { .. } => unreachable!(),
        };
        let table = physical
            .iter()
            .find(|table| table.id == table_id)
            .ok_or_else(|| stale_store_update("allocated lookup table", table_id))?;
        let allocation_mode = if table.allocation_kind == LookupTableAllocationKind::DedicatedVault
        {
            LookupTableBindingMode::Dedicated
        } else {
            LookupTableBindingMode::PackedShard
        };
        let existing_binding_row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_vault_bindings
            WHERE vault_id = $1 AND family_id = $2 AND route_lookup_table_id = $3
              AND manifest_id = $4 AND binding_ordinal = $5
              AND desired_head_revision = $6
              AND lifecycle_state NOT IN ('retired', 'failed')
            ORDER BY id DESC LIMIT 1
            "#,
        )
        .bind(request.vault_id.as_i64())
        .bind(request.family_id)
        .bind(table_id)
        .bind(request.manifest_id)
        .bind(request.binding_ordinal)
        .bind(desired_head_revision)
        .fetch_optional(&mut *tx)
        .await?;
        let binding = if let Some(row) = existing_binding_row {
            lookup_table_binding_from_row(&row)?
        } else {
            let row = sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_vault_bindings
                    (vault_id, family_id, route_lookup_table_id, manifest_id,
                     binding_ordinal, desired_head_revision, allocation_mode,
                     reserved_capacity, predecessor_binding_id, lifecycle_state)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'preparing')
                RETURNING *
                "#,
            )
            .bind(request.vault_id.as_i64())
            .bind(request.family_id)
            .bind(table_id)
            .bind(request.manifest_id)
            .bind(request.binding_ordinal)
            .bind(desired_head_revision)
            .bind(allocation_mode.as_str())
            .bind(i32::from(reserved_capacity))
            .bind(active_binding.as_ref().map(|binding| binding.id))
            .fetch_one(&mut *tx)
            .await?;
            lookup_table_binding_from_row(&row)?
        };
        let pending_operation_rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_operations
            WHERE route_lookup_table_id = $1
              AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
            ORDER BY created_at, id
            "#,
        )
        .bind(table_id)
        .fetch_all(&mut *tx)
        .await?;
        let mut operations = pending_operation_rows
            .iter()
            .map(lookup_table_operation_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        if operations.is_empty() && !missing_addresses.is_empty() {
            let chunk = missing_addresses
                .iter()
                .take(request.max_extension_addresses)
                .cloned()
                .collect::<Vec<_>>();
            let intent = LookupTableOperationIntent {
                cluster: request.cluster,
                family_id: request.family_id,
                table_id: Some(table_id),
                kind: LookupTableOperationKind::Extend,
                generation: table.generation,
                shard_index: table.shard_ordinal,
                mutation_epoch: table.mutation_epoch,
                desired_address_hash: desired_set_hash,
                addresses: chunk.clone(),
            };
            operations.push(
                enqueue_lookup_table_operation_in_tx(
                    &mut *tx,
                    &LookupTableOperationEnqueue {
                        idempotency_key: intent.idempotency_key(),
                        family_id: request.family_id,
                        route_lookup_table_id: Some(table_id),
                        manifest_id: Some(request.manifest_id),
                        binding_id: Some(binding.id),
                        operation_kind: LookupTableOperationKind::Extend,
                        target_generation: None,
                        target_shard_ordinal: None,
                        operation_context: request.operation_context,
                        mutation_epoch: table.mutation_epoch,
                        estimated_fee_lamports: request.estimated_fee_lamports,
                        estimated_rent_lamports: request.estimated_rent_lamports,
                        addresses: chunk,
                    },
                )
                .await?,
            );
        }
        Ok(AtomicVaultAllocationResult::BindingReserved {
            allocation,
            binding,
            operations,
        })
    }

    /// Plans the current catalog head without requiring a vault request. The
    /// expected immutable revision id fences a stale catalog derivation.
    pub async fn plan_shared_market_catalog_head(
        &self,
        cluster: &str,
        expected_catalog_revision_id: i64,
        policy: SharedMarketCatalogPlanPolicy,
    ) -> Result<SharedMarketCatalogPlan, OrchestratorError> {
        if policy.max_extension_addresses == 0 {
            return Err(OrchestratorError::StoreInvariant(
                "max extension addresses must be positive".to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        let catalog = load_shared_market_catalog_head_in_connection(
            &mut tx,
            cluster,
            SharedMarketCatalogHeadLock::Update,
        )
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "cluster {cluster:?} has no shared-market catalog head"
            ))
        })?;
        if catalog.catalog_revision_id != expected_catalog_revision_id {
            return Err(OrchestratorError::StoreInvariant(format!(
                "shared-market catalog head changed from revision id {expected_catalog_revision_id} to {}",
                catalog.catalog_revision_id
            )));
        }
        let (shared_target_generation, shared_operations) = self
            .plan_shared_market_operations_in_connection(
                &mut tx,
                cluster,
                &catalog,
                policy.shared_shard_capacity,
                policy.max_extension_addresses,
                policy.operation_context,
                policy.estimated_fee_lamports,
                policy.estimated_rent_lamports,
            )
            .await?;
        update_shared_market_catalog_plan_state_in_connection(
            &mut tx,
            &catalog,
            shared_target_generation,
        )
        .await?;
        let catalog = load_shared_market_catalog_head_in_connection(
            &mut tx,
            cluster,
            SharedMarketCatalogHeadLock::None,
        )
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "shared-market catalog head disappeared after planning".to_owned(),
            )
        })?;
        tx.commit().await?;
        Ok(SharedMarketCatalogPlan {
            catalog,
            shared_target_generation,
            shared_operations,
        })
    }

    /// Reconciles the current catalog readiness and atomically activates a
    /// fully materialized target generation. Incomplete work remains in the
    /// provisioning state for the generic operation worker to continue.
    pub async fn reconcile_shared_market_catalog_head(
        &self,
        cluster: &str,
        expected_catalog_revision_id: i64,
        policy: SharedMarketCatalogPlanPolicy,
        rollback_until: DateTime<Utc>,
    ) -> Result<SharedMarketCatalogHeadRecord, OrchestratorError> {
        if rollback_until <= Utc::now() || policy.max_extension_addresses == 0 {
            return Err(OrchestratorError::StoreInvariant(
                "shared-market catalog reconciliation requires a future rollback deadline and positive extension size"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        let catalog = load_shared_market_catalog_head_in_connection(
            &mut tx,
            cluster,
            SharedMarketCatalogHeadLock::Update,
        )
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "cluster {cluster:?} has no shared-market catalog head"
            ))
        })?;
        if catalog.catalog_revision_id != expected_catalog_revision_id {
            return Err(OrchestratorError::StoreInvariant(format!(
                "shared-market catalog head changed from revision id {expected_catalog_revision_id} to {}",
                catalog.catalog_revision_id
            )));
        }
        let (planned_target_generation, _) = self
            .plan_shared_market_operations_in_connection(
                &mut tx,
                cluster,
                &catalog,
                policy.shared_shard_capacity,
                policy.max_extension_addresses,
                policy.operation_context,
                policy.estimated_fee_lamports,
                policy.estimated_rent_lamports,
            )
            .await?;
        update_shared_market_catalog_plan_state_in_connection(
            &mut tx,
            &catalog,
            planned_target_generation,
        )
        .await?;
        let catalog = load_shared_market_catalog_head_in_connection(
            &mut tx,
            cluster,
            SharedMarketCatalogHeadLock::None,
        )
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "shared-market catalog head disappeared during reconciliation".to_owned(),
            )
        })?;
        let target_generation = catalog.target_generation.ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "shared-market catalog revision {} has not been planned",
                catalog.catalog_revision_id
            ))
        })?;
        let has_permanent_failure: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1 FROM loyal_yield.lookup_table_operations
                WHERE family_id = $1 AND manifest_id = $2
                  AND operation_state = 'permanent_failure'
            )
            "#,
        )
        .bind(catalog.family_id)
        .bind(catalog.manifest_id)
        .fetch_one(&mut *tx)
        .await?;
        if has_permanent_failure {
            sqlx::query(
                r#"
                UPDATE loyal_yield.lookup_table_shared_market_catalog_heads
                SET readiness_state = 'failed', activated_at = NULL, updated_at = now()
                WHERE family_id = $1 AND catalog_revision_id = $2
                "#,
            )
            .bind(catalog.family_id)
            .bind(catalog.catalog_revision_id)
            .execute(&mut *tx)
            .await?;
        } else {
            let evidence = shared_market_catalog_generation_evidence_in_connection(
                &mut tx,
                catalog.family_id,
                Some(target_generation),
                &catalog.addresses,
            )
            .await?;
            if evidence.ready {
                if catalog.active_generation != Some(target_generation) {
                    activate_shared_market_catalog_generation_in_connection(
                        &mut tx,
                        catalog.family_id,
                        target_generation,
                        rollback_until,
                    )
                    .await?;
                }
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.lookup_table_shared_market_physical_drifts
                    SET resolution_state = 'resolved',
                        resolution_target_generation = $3,
                        resolved_at = now()
                    WHERE family_id = $1 AND catalog_revision_id = $2
                      AND resolution_state = 'open'
                    "#,
                )
                .bind(catalog.family_id)
                .bind(catalog.catalog_revision_id)
                .bind(target_generation)
                .execute(&mut *tx)
                .await?;
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.lookup_table_shared_market_catalog_heads
                    SET readiness_state = 'active', activated_at = COALESCE(activated_at, now()),
                        updated_at = now()
                    WHERE family_id = $1 AND catalog_revision_id = $2
                      AND target_generation = $3
                    "#,
                )
                .bind(catalog.family_id)
                .bind(catalog.catalog_revision_id)
                .bind(target_generation)
                .execute(&mut *tx)
                .await?;
            } else {
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.lookup_table_shared_market_catalog_heads
                    SET readiness_state = 'provisioning', activated_at = NULL,
                        updated_at = now()
                    WHERE family_id = $1 AND catalog_revision_id = $2
                      AND target_generation = $3
                    "#,
                )
                .bind(catalog.family_id)
                .bind(catalog.catalog_revision_id)
                .bind(target_generation)
                .execute(&mut *tx)
                .await?;
            }
        }
        let catalog = load_shared_market_catalog_head_in_connection(
            &mut tx,
            cluster,
            SharedMarketCatalogHeadLock::None,
        )
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "shared-market catalog head disappeared after reconciliation".to_owned(),
            )
        })?;
        tx.commit().await?;
        Ok(catalog)
    }

    pub async fn plan_lookup_table_provisioning_request(
        &self,
        cluster: &str,
        request_id: i64,
        lease: &LookupTableOperationLease,
        policy: LookupTableProvisioningPlanPolicy,
    ) -> Result<LookupTableProvisioningPlan, OrchestratorError> {
        if policy.max_extension_addresses == 0 {
            return Err(OrchestratorError::StoreInvariant(
                "max extension addresses must be positive".to_owned(),
            ));
        }

        for attempt in 1..=LOOKUP_TABLE_DB_CONCURRENCY_MAX_ATTEMPTS {
            match self
                .plan_lookup_table_provisioning_request_once(
                    cluster,
                    request_id,
                    lease,
                    policy.clone(),
                )
                .await
            {
                Ok(plan) => return Ok(plan),
                Err(error) => {
                    let Some(sqlstate) = retryable_lookup_table_database_conflict(&error) else {
                        return Err(error);
                    };
                    if attempt == LOOKUP_TABLE_DB_CONCURRENCY_MAX_ATTEMPTS {
                        return Err(error);
                    }
                    log_lookup_table_database_retry(
                        "plan_lookup_table_provisioning_request",
                        sqlstate,
                        attempt,
                    );
                    sleep_for_lookup_table_database_retry(attempt).await;
                }
            }
        }
        unreachable!("bounded lookup-table database retry returns on its final attempt")
    }

    async fn plan_lookup_table_provisioning_request_once(
        &self,
        cluster: &str,
        request_id: i64,
        lease: &LookupTableOperationLease,
        policy: LookupTableProvisioningPlanPolicy,
    ) -> Result<LookupTableProvisioningPlan, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        // Normal demand-driven planning must not take the cluster rollout
        // lock: unrelated vault families and physical tables need to make
        // progress independently. The shared head is consumed optimistically;
        // vault-family/table locks remain canonical, while the rollout lock is
        // reserved for catalog publication, cutover, pause, and retirement.
        let request_row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_provisioning_requests
            WHERE id = $1 AND cluster = $2 AND request_status = 'planning'
              AND lease_owner = $3 AND fencing_token = $4
              AND lease_expires_at > now()
            FOR UPDATE
            "#,
        )
        .bind(request_id)
        .bind(cluster)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "provisioning request {request_id} lease is stale, expired, or fenced"
            ))
        })?;
        let request = lookup_table_provisioning_request_from_row(&request_row)?;

        if request.sealed_at.is_none() {
            return Err(OrchestratorError::StoreInvariant(format!(
                "provisioning request {request_id} is not sealed"
            )));
        }
        let source_slot = policy
            .operation_context
            .get("recent_slot")
            .or_else(|| policy.operation_context.get("recentSlot"))
            .and_then(Value::as_u64)
            .and_then(|slot| i64::try_from(slot).ok());
        let shared_manifest = resolve_or_persist_request_manifest_in_tx(
            &mut *tx,
            cluster,
            &request,
            LookupTableManifestSubject::SharedMarket,
            source_slot,
        )
        .await?;
        let shared_manifest_id = shared_manifest.id;
        let catalog = load_shared_market_catalog_head_in_connection(
            &mut tx,
            cluster,
            SharedMarketCatalogHeadLock::None,
        )
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "cluster {cluster:?} has no authoritative shared-market catalog head"
            ))
        })?;
        if shared_manifest.family_id != catalog.family_id {
            return Err(OrchestratorError::StoreInvariant(format!(
                "route shared manifest {} does not belong to catalog family {}",
                shared_manifest.id, catalog.family_id
            )));
        }
        let (route_missing, semantic_mismatches) =
            shared_market_route_catalog_drift(&shared_manifest.addresses, &catalog.addresses);
        if !route_missing.is_empty() || !semantic_mismatches.is_empty() {
            return Err(OrchestratorError::StoreInvariant(format!(
                "route shared manifest {} drifted from catalog revision {} ({} missing, {} semantic mismatch)",
                shared_manifest.id,
                catalog.catalog_revision_id,
                route_missing.len(),
                semantic_mismatches.len()
            )));
        }
        // Shared-family/head mutation belongs to the dedicated catalog
        // reconciler. A normal vault request consumes this snapshot and locks
        // only vault-family/table rows, allowing unrelated cold vaults to plan
        // independently while a staging shared revision warms.
        let shared_target_generation = catalog
            .target_generation
            .or(catalog.active_generation)
            .unwrap_or_default();
        let shared_operations = Vec::new();

        // Shared drift is fenced above before deriving or allocating any vault
        // desired state. A catalog/code mismatch cannot consume shard capacity.
        let vault_manifest = resolve_or_persist_request_manifest_in_tx(
            &mut *tx,
            cluster,
            &request,
            LookupTableManifestSubject::Vault,
            source_slot,
        )
        .await?;
        let vault_manifest_id = vault_manifest.id;
        sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_provisioning_requests
            SET shared_manifest_id = $2, vault_manifest_id = $3, updated_at = now()
            WHERE id = $1
            "#,
        )
        .bind(request_id)
        .bind(shared_manifest_id)
        .bind(vault_manifest_id)
        .execute(&mut *tx)
        .await?;

        let vault_family_id = vault_manifest.family_id;
        let vault_addresses = sqlx::query_scalar::<_, String>(
            "SELECT address FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id = $1 ORDER BY ordinal",
        )
        .bind(vault_manifest_id)
        .fetch_all(&mut *tx)
        .await?
        .into_iter()
        .collect::<BTreeSet<_>>();
        let vault_family_row =
            sqlx::query("SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 FOR SHARE")
                .bind(vault_family_id)
                .fetch_one(&mut *tx)
                .await?;
        let vault_family = lookup_table_family_from_row(&vault_family_row)?;
        let next_generation = vault_family.active_generation.unwrap_or_default();
        let next_shard_ordinal = sqlx::query_scalar::<_, Option<i32>>(
            "SELECT max(shard_ordinal) FROM loyal_yield.route_lookup_tables WHERE family_id = $1 AND generation = $2",
        )
        .bind(vault_family_id)
        .bind(next_generation)
        .fetch_one(&mut *tx)
        .await?
        .map_or(0, |ordinal| ordinal.saturating_add(1));
        let vault_allocation = self
            .allocate_vault_binding_and_queue_operation_in_connection(
                &mut *tx,
                AtomicVaultAllocationRequest {
                    cluster: cluster.to_owned(),
                    family_id: vault_family_id,
                    vault_id: request.vault_id,
                    manifest_id: vault_manifest_id,
                    binding_ordinal: 0,
                    desired_addresses: vault_addresses,
                    policy: policy.vault_policy,
                    next_generation,
                    next_shard_ordinal,
                    operation_context: policy.operation_context,
                    estimated_fee_lamports: policy.estimated_fee_lamports,
                    estimated_rent_lamports: policy.estimated_rent_lamports,
                    max_extension_addresses: policy.max_extension_addresses,
                },
            )
            .await?;
        let pending_operation_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*)
            FROM loyal_yield.lookup_table_operations
            WHERE manifest_id IN ($1, $2)
              AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
            "#,
        )
        .bind(catalog.manifest_id)
        .bind(vault_manifest_id)
        .fetch_one(&mut *tx)
        .await?;
        let refreshed_catalog = load_shared_market_catalog_head_in_connection(
            &mut tx,
            cluster,
            SharedMarketCatalogHeadLock::None,
        )
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "shared-market catalog head disappeared during route planning".to_owned(),
            )
        })?;
        if refreshed_catalog.catalog_revision_id != catalog.catalog_revision_id {
            return Err(OrchestratorError::StoreInvariant(format!(
                "shared-market catalog head changed from revision {} to {} during vault request planning",
                catalog.catalog_revision_id, refreshed_catalog.catalog_revision_id
            )));
        }
        let shared_evidence = shared_market_catalog_generation_evidence_in_connection(
            &mut tx,
            refreshed_catalog.family_id,
            refreshed_catalog.active_generation,
            &refreshed_catalog.addresses,
        )
        .await?;
        let shared_ready = refreshed_catalog.readiness_state
            == SharedMarketCatalogReadiness::Active
            && refreshed_catalog.target_generation == refreshed_catalog.active_generation
            && shared_evidence.ready;
        let request_satisfied = provisioning_request_is_satisfied(
            shared_ready,
            shared_operations.len(),
            pending_operation_count,
            &vault_allocation,
        );
        let terminal_operation =
            terminal_provisioning_operation(&shared_operations, &vault_allocation);
        let terminal_error_detail = terminal_operation.map(|operation| {
            format!(
                "lookup-table operation {} reached terminal state {} and requires operator repair",
                operation.id, operation.operation_state
            )
        });
        let queued_retry_at = Utc::now() + chrono::Duration::seconds(5);
        let request_row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_provisioning_requests
            SET request_status = CASE
                    WHEN $5::BOOLEAN THEN 'failed'
                    WHEN $4::BOOLEAN THEN 'satisfied'
                    ELSE 'queued'
                END,
                satisfied_at = CASE WHEN $4 AND NOT $5 THEN now() ELSE satisfied_at END,
                next_attempt_at = CASE
                    WHEN $5 OR $4 THEN NULL
                    ELSE $7
                END,
                lease_owner = NULL, lease_expires_at = NULL,
                error_code = CASE WHEN $5 THEN 'terminal_lookup_table_operation' ELSE NULL END,
                error_detail = CASE WHEN $5 THEN $6 ELSE NULL END,
                updated_at = now()
            WHERE id = $1 AND request_status = 'planning'
              AND lease_owner = $2 AND fencing_token = $3
            RETURNING *
            "#,
        )
        .bind(request_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(request_satisfied)
        .bind(terminal_operation.is_some())
        .bind(terminal_error_detail)
        .bind(queued_retry_at)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "provisioning request {request_id} lost fencing during planning"
            ))
        })?;
        let request = lookup_table_provisioning_request_from_row(&request_row)?;
        tx.commit().await?;
        Ok(LookupTableProvisioningPlan {
            request,
            shared_target_generation,
            shared_operations,
            vault_allocation,
        })
    }

    #[allow(clippy::too_many_arguments)]
    async fn plan_shared_market_operations_in_connection(
        &self,
        tx: &mut sqlx::PgConnection,
        cluster: &str,
        catalog: &SharedMarketCatalogHeadRecord,
        requested_shard_capacity: u16,
        max_extension_addresses: usize,
        operation_context: Value,
        estimated_fee_lamports: Option<i64>,
        estimated_rent_lamports: Option<i64>,
    ) -> Result<(i32, Vec<LookupTableOperationRecord>), OrchestratorError> {
        let family_id = catalog.family_id;
        let family_row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 AND cluster = $2 FOR UPDATE",
        )
        .bind(family_id)
        .bind(cluster)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("shared-market family", family_id))?;
        let family = lookup_table_family_from_row(&family_row)?;
        if family.kind != LookupTableFamilyKind::SharedMarket
            || family.desired_state != LookupTableFamilyState::Active
            || family.id != catalog.family_id
            || family.catalog_version != catalog.catalog_version
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table family {family_id} is not an active shared-market family"
            )));
        }
        let shard_capacity = requested_shard_capacity
            .min(family.allocation_high_water as u16)
            .min(family.hard_capacity as u16);
        if catalog.addresses.is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "shared-market catalog head is empty".to_owned(),
            ));
        }
        // The logical catalog can exceed one physical ALT. Preserve its
        // append-stable order and fill deterministic shard ordinals in that
        // exact order. A later append therefore extends only the final shard
        // (and then allocates the next ordinal) instead of relocating an
        // already-active prefix.
        let ordered_addresses = catalog
            .addresses
            .iter()
            .map(|row| row.address.clone())
            .collect::<Vec<_>>();
        let shard_plan = append_pack_shared_market_shards(&ordered_addresses, shard_capacity)
            .map_err(domain_store_error)?;
        // Signed work keeps its identity and may only be reconciled; the
        // before-sign and before-broadcast head fences prevent it from
        // mutating a newer catalog revision.
        cancel_superseded_unsigned_shared_market_operations_in_connection(
            tx,
            family_id,
            catalog.manifest_id,
        )
        .await?;

        let active_generation = family.active_generation.unwrap_or_default();
        let mut planning_state =
            load_shared_market_generation_planning_state(tx, family_id, active_generation).await?;
        let has_open_physical_drift: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.lookup_table_shared_market_physical_drifts drift
                JOIN loyal_yield.route_lookup_tables route_table
                  ON route_table.id = drift.route_lookup_table_id
                WHERE drift.family_id = $1
                  AND drift.catalog_revision_id = $2
                  AND drift.resolution_state = 'open'
                  AND route_table.generation = $3
            )
            "#,
        )
        .bind(family_id)
        .bind(catalog.catalog_revision_id)
        .bind(active_generation)
        .fetch_one(&mut *tx)
        .await?;
        let requires_rollover = has_open_physical_drift
            || !shared_market_generation_is_order_compatible(&planning_state, &shard_plan);
        let mut target_generation = active_generation;
        if requires_rollover {
            let max_generation: i32 = sqlx::query_scalar(
                r#"
                SELECT COALESCE(max(generation), -1)::INTEGER
                FROM loyal_yield.route_lookup_tables
                WHERE family_id = $1 AND generation IS NOT NULL
                "#,
            )
            .bind(family_id)
            .fetch_one(&mut *tx)
            .await?;
            let planned_candidate = catalog
                .target_generation
                .filter(|generation| *generation != active_generation);
            if let Some(candidate_generation) = planned_candidate {
                let candidate_state = load_shared_market_generation_planning_state(
                    tx,
                    family_id,
                    candidate_generation,
                )
                .await?;
                if candidate_state.all_table_count
                    == i64::try_from(candidate_state.physical.len()).unwrap_or(-1)
                    && shared_market_generation_is_order_compatible(&candidate_state, &shard_plan)
                {
                    target_generation = candidate_generation;
                    planning_state = candidate_state;
                } else {
                    target_generation = max_generation.checked_add(1).ok_or_else(|| {
                        OrchestratorError::StoreInvariant(
                            "shared-market generation counter overflowed".to_owned(),
                        )
                    })?;
                    planning_state = SharedMarketGenerationPlanningState::empty();
                }
            } else {
                // A newly published head deliberately skips every old partial
                // successor. Reusing active+1 can append an obsolete catalog
                // after an A -> B head change, so allocate strictly above the
                // complete historical generation range.
                target_generation = max_generation.checked_add(1).ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "shared-market generation counter overflowed".to_owned(),
                    )
                })?;
                planning_state = SharedMarketGenerationPlanningState::empty();
            }
        }
        let mut operations = Vec::new();
        for shard in shard_plan {
            let existing_table = planning_state
                .physical
                .iter()
                .find(|table| table.shard_ordinal == shard.shard_ordinal)
                .cloned();
            let confirmed_addresses = existing_table
                .as_ref()
                .and_then(|table| planning_state.confirmed.get(&table.shard_ordinal))
                .cloned()
                .unwrap_or_default();
            let pending_addresses = existing_table
                .as_ref()
                .and_then(|table| planning_state.pending.get(&table.shard_ordinal))
                .cloned()
                .unwrap_or_default();
            let nonterminal_operation_count = existing_table
                .as_ref()
                .and_then(|table| {
                    planning_state
                        .nonterminal_operation_count
                        .get(&table.shard_ordinal)
                })
                .copied()
                .unwrap_or_default();
            if nonterminal_operation_count != 0 {
                continue;
            }
            let Some((kind, missing)) = next_shared_market_mutation(
                existing_table.is_some(),
                &shard.addresses,
                &confirmed_addresses,
                &pending_addresses,
                max_extension_addresses,
            ) else {
                continue;
            };
            let (table, operation_context) = if let Some(table) = existing_table {
                debug_assert_eq!(kind, LookupTableOperationKind::Extend);
                (table, operation_context.clone())
            } else {
                debug_assert_eq!(kind, LookupTableOperationKind::Create);
                let (table, create_context) = reserve_shared_lookup_table_in_tx(
                    tx,
                    &family,
                    target_generation,
                    shard.shard_ordinal,
                    operation_context.clone(),
                )
                .await?;
                (table, create_context)
            };
            let shard_hash = ordered_address_hash(&shard.addresses);
            let intent = LookupTableOperationIntent {
                cluster: cluster.to_owned(),
                family_id,
                table_id: Some(table.id),
                kind,
                generation: target_generation,
                shard_index: shard.shard_ordinal,
                mutation_epoch: table.mutation_epoch,
                desired_address_hash: shard_hash,
                addresses: missing.clone(),
            };
            operations.push(
                enqueue_lookup_table_operation_in_tx(
                    &mut *tx,
                    &LookupTableOperationEnqueue {
                        idempotency_key: intent.idempotency_key(),
                        family_id,
                        route_lookup_table_id: Some(table.id),
                        manifest_id: Some(catalog.manifest_id),
                        binding_id: None,
                        operation_kind: kind,
                        target_generation: (kind == LookupTableOperationKind::Create)
                            .then_some(target_generation),
                        target_shard_ordinal: (kind == LookupTableOperationKind::Create)
                            .then_some(shard.shard_ordinal),
                        operation_context,
                        mutation_epoch: table.mutation_epoch,
                        estimated_fee_lamports,
                        estimated_rent_lamports,
                        addresses: missing,
                    },
                )
                .await?,
            );
        }
        Ok((target_generation, operations))
    }

    pub async fn flip_lookup_table_binding_head(
        &self,
        binding_id: i64,
        observed_slot: i64,
        rollback_until: DateTime<Utc>,
    ) -> Result<LookupTableBindingHeadFlip, OrchestratorError> {
        if observed_slot < 0 || rollback_until <= Utc::now() {
            return Err(OrchestratorError::StoreInvariant(
                "binding activation requires a nonnegative observed slot and a future rollback deadline"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        // Read immutable foreign keys first, then acquire locks in the same
        // family-first order as allocation/finalization. Every field is
        // rechecked after the binding lock so a direct caller cannot race a
        // manifest/table substitution into activation.
        let identity_row = sqlx::query(
            r#"
            SELECT family_id, route_lookup_table_id, manifest_id, vault_id,
                   binding_ordinal
            FROM loyal_yield.lookup_table_vault_bindings WHERE id = $1
            "#,
        )
        .bind(binding_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("lookup-table binding", binding_id))?;
        let identity_family_id: i64 = identity_row.try_get("family_id")?;
        let identity_table_id: i64 = identity_row.try_get("route_lookup_table_id")?;
        let identity_manifest_id: i64 = identity_row.try_get("manifest_id")?;
        let identity_vault_id: i64 = identity_row.try_get("vault_id")?;
        let identity_binding_ordinal: i32 = identity_row.try_get("binding_ordinal")?;

        let family_row =
            sqlx::query("SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 FOR SHARE")
                .bind(identity_family_id)
                .fetch_optional(&mut *tx)
                .await?
                .ok_or_else(|| stale_store_update("lookup-table family", identity_family_id))?;
        let family = lookup_table_family_from_row(&family_row)?;
        if family.kind != LookupTableFamilyKind::VaultShards
            || family.desired_state != LookupTableFamilyState::Active
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} does not belong to an active vault-shards family"
            )));
        }

        let manifest_row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_manifests
            WHERE id = $1 AND family_id = $2 AND vault_id = $3
              AND subject_kind = 'vault' AND sealed_at IS NOT NULL
            FOR SHARE
            "#,
        )
        .bind(identity_manifest_id)
        .bind(identity_family_id)
        .bind(identity_vault_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("sealed vault manifest", identity_manifest_id))?;
        let manifest_address_count: i32 = manifest_row.try_get("address_count")?;

        // Lock the entire logical head in deterministic id order. This avoids
        // two candidate activators locking A then B/B then A and lets us make a
        // single desired-revision decision for all contenders.
        let head_rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_vault_bindings
            WHERE vault_id = $1 AND family_id = $2 AND binding_ordinal = $3
              AND lifecycle_state IN ('preparing', 'warming', 'active')
            ORDER BY id
            FOR UPDATE
            "#,
        )
        .bind(identity_vault_id)
        .bind(identity_family_id)
        .bind(identity_binding_ordinal)
        .fetch_all(&mut *tx)
        .await?;
        let candidate = head_rows
            .iter()
            .find(|row| row.try_get::<i64, _>("id").ok() == Some(binding_id))
            .map(lookup_table_binding_from_row)
            .transpose()?
            .ok_or_else(|| stale_store_update("preparing lookup-table binding", binding_id))?;
        if candidate.family_id != identity_family_id
            || candidate.route_lookup_table_id != identity_table_id
            || candidate.manifest_id != identity_manifest_id
            || candidate.vault_id.as_i64() != identity_vault_id
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} identity changed while activation was being fenced"
            )));
        }
        if !matches!(
            candidate.lifecycle_state,
            LookupTableBindingLifecycle::Preparing | LookupTableBindingLifecycle::Warming
        ) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} is not preparing or warming"
            )));
        }

        let desired_row = sqlx::query(
            r#"
            SELECT manifest_id, desired_revision
            FROM loyal_yield.lookup_table_vault_desired_heads
            WHERE family_id = $1 AND vault_id = $2 AND binding_ordinal = $3
            FOR UPDATE
            "#,
        )
        .bind(candidate.family_id)
        .bind(candidate.vault_id.as_i64())
        .bind(candidate.binding_ordinal)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} has no durable desired-head revision"
            ))
        })?;
        let desired_manifest_id: i64 = desired_row.try_get("manifest_id")?;
        let desired_revision: i64 = desired_row.try_get("desired_revision")?;
        if candidate.manifest_id != desired_manifest_id
            || candidate.desired_head_revision != desired_revision
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} was superseded by desired head revision {desired_revision}"
            )));
        }
        let newest_desired_candidate_id = head_rows
            .iter()
            .filter_map(|row| {
                let lifecycle = row.try_get::<String, _>("lifecycle_state").ok()?;
                let manifest_id = row.try_get::<i64, _>("manifest_id").ok()?;
                let revision = row.try_get::<i64, _>("desired_head_revision").ok()?;
                matches!(lifecycle.as_str(), "preparing" | "warming")
                    .then(|| row.try_get::<i64, _>("id").ok())
                    .flatten()
                    .filter(|_| manifest_id == desired_manifest_id && revision == desired_revision)
            })
            .max();
        if newest_desired_candidate_id != Some(candidate.id) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} is not the newest candidate for desired head revision {desired_revision}"
            )));
        }

        let predecessor = head_rows
            .iter()
            .filter(|row| {
                row.try_get::<String, _>("lifecycle_state").ok().as_deref() == Some("active")
                    && row.try_get::<i64, _>("id").ok() != Some(candidate.id)
            })
            .map(lookup_table_binding_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        if predecessor.len() > 1
            || predecessor
                .first()
                .is_some_and(|binding| binding.id >= candidate.id)
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} does not monotonically succeed the active head"
            )));
        }
        let predecessor = predecessor.into_iter().next();

        let mut affected_table_ids = vec![candidate.route_lookup_table_id];
        if let Some(predecessor) = &predecessor {
            affected_table_ids.push(predecessor.route_lookup_table_id);
        }
        affected_table_ids.sort_unstable();
        affected_table_ids.dedup();

        let table_rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.route_lookup_tables
            WHERE id = ANY($1) AND family_id = $2
            ORDER BY id
            FOR UPDATE
            "#,
        )
        .bind(&affected_table_ids)
        .bind(identity_family_id)
        .fetch_all(&mut *tx)
        .await?;
        if table_rows.len() != affected_table_ids.len() {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} head references a missing physical table"
            )));
        }
        let table_row = table_rows
            .iter()
            .find(|row| row.try_get::<i64, _>("id").ok() == Some(identity_table_id))
            .ok_or_else(|| stale_store_update("reusable lookup table", identity_table_id))?;
        let table = reusable_lookup_table_from_row(&table_row)?;
        let durable: bool = table_row.try_get("durable")?;
        // Publishing a logical binding on an unchanged packed table does not
        // invalidate routes for other vaults that share the physical ALT.
        // Fence only leases that belong to this vault/head, while treating
        // malformed unscoped vault-table leases as ambiguous and fail-closed.
        // Physical mutation, generation replacement, rollback, and cleanup
        // retain their table-wide lease fences elsewhere in this module.
        let predecessor_binding_id = predecessor.as_ref().map(|binding| binding.id);
        let conflicting_usage_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*) FROM loyal_yield.lookup_table_usage_leases
            WHERE route_lookup_table_id = ANY($1)
              AND released_at IS NULL AND expires_at > now()
              AND (
                  vault_id = $2
                  OR binding_id = $3
                  OR vault_id IS NULL
                  OR binding_id IS NULL
              )
            "#,
        )
        .bind(&affected_table_ids)
        .bind(candidate.vault_id.as_i64())
        .bind(predecessor_binding_id)
        .fetch_one(&mut *tx)
        .await?;
        if conflicting_usage_count != 0 {
            return Err(OrchestratorError::LookupTableBindingActivationDeferred { binding_id });
        }
        let expected_allocation_kind = match candidate.allocation_mode {
            LookupTableBindingMode::PackedShard => LookupTableAllocationKind::VaultShard,
            LookupTableBindingMode::Dedicated => LookupTableAllocationKind::DedicatedVault,
        };
        if table.cluster != family.cluster
            || table.allocation_kind != expected_allocation_kind
            || family.active_generation != Some(table.generation)
            || table.desired_state != LookupTableLifecycle::Active
            || table.legacy_status != "usable"
            || !durable
            || table.last_verified_slot.is_none()
            || table
                .last_verified_slot
                .is_some_and(|slot| slot > observed_slot)
            || table.usable_address_count != table.address_count
            || candidate.reserved_capacity < manifest_address_count
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} physical table is not verified, fully usable, and active in the family head"
            )));
        }

        let membership_rows = sqlx::query(
            r#"
            SELECT address, ordinal, added_operation_id, added_slot,
                   usable_after_slot, last_verified_slot, last_verified_at
            FROM loyal_yield.lookup_table_addresses
            WHERE route_lookup_table_id = $1 ORDER BY ordinal
            "#,
        )
        .bind(identity_table_id)
        .fetch_all(&mut *tx)
        .await?;
        let membership = membership_rows
            .iter()
            .map(|row| {
                Ok(LookupTableMembershipAddress {
                    address: row.try_get("address")?,
                    ordinal: row.try_get("ordinal")?,
                    added_operation_id: row.try_get("added_operation_id")?,
                    added_slot: row.try_get("added_slot")?,
                    usable_after_slot: row.try_get("usable_after_slot")?,
                    last_verified_slot: row.try_get("last_verified_slot")?,
                    last_verified_at: row.try_get("last_verified_at")?,
                })
            })
            .collect::<Result<Vec<_>, OrchestratorError>>()?;
        validate_membership(&membership, observed_slot)?;
        let ordered_membership = membership
            .iter()
            .map(|entry| entry.address.clone())
            .collect::<Vec<_>>();
        if membership.len() != table.address_count as usize
            || membership
                .iter()
                .any(|entry| entry.usable_after_slot > observed_slot)
            || ordered_address_hash(&ordered_membership) != table.address_hash
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} physical membership is not the exact verified usable prefix"
            )));
        }
        let manifest_rows = sqlx::query(
            r#"
            SELECT address, ordinal
            FROM loyal_yield.lookup_table_manifest_addresses
            WHERE manifest_id = $1 ORDER BY ordinal
            "#,
        )
        .bind(identity_manifest_id)
        .fetch_all(&mut *tx)
        .await?;
        let usable_addresses = ordered_membership.into_iter().collect::<BTreeSet<_>>();
        if manifest_rows.len() != manifest_address_count as usize
            || manifest_rows.iter().enumerate().any(|(ordinal, row)| {
                row.try_get::<i32, _>("ordinal").ok() != Some(ordinal as i32)
                    || row
                        .try_get::<String, _>("address")
                        .ok()
                        .is_none_or(|address| !usable_addresses.contains(&address))
            })
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} sealed manifest is not exactly covered by the usable prefix"
            )));
        }
        let pending_operation_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*) FROM loyal_yield.lookup_table_operations
            WHERE route_lookup_table_id = $1
              AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
            "#,
        )
        .bind(identity_table_id)
        .fetch_one(&mut *tx)
        .await?;
        if pending_operation_count != 0 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {binding_id} physical table still has a conflicting operation"
            )));
        }
        // Release every older in-flight reservation before publishing the new
        // head. Its operation/signature rows remain intact for reconciliation,
        // but the failed binding can never be warmed or activated later.
        sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_vault_bindings
            SET lifecycle_state = 'failed',
                deactivated_at = COALESCE(deactivated_at, now()),
                updated_at = now()
            WHERE vault_id = $1 AND family_id = $2 AND binding_ordinal = $3
              AND id <> $4 AND lifecycle_state IN ('preparing', 'warming')
            "#,
        )
        .bind(candidate.vault_id.as_i64())
        .bind(candidate.family_id)
        .bind(candidate.binding_ordinal)
        .bind(candidate.id)
        .execute(&mut *tx)
        .await?;
        let predecessor = if let Some(predecessor) = predecessor {
            let row = sqlx::query(
                r#"
                UPDATE loyal_yield.lookup_table_vault_bindings
                SET lifecycle_state = 'standby', active_until_slot = $2,
                    rollback_until = $3, updated_at = now()
                WHERE id = $1 AND lifecycle_state = 'active'
                RETURNING *
                "#,
            )
            .bind(predecessor.id)
            .bind(observed_slot)
            .bind(rollback_until)
            .fetch_one(&mut *tx)
            .await?;
            Some(lookup_table_binding_from_row(&row)?)
        } else {
            None
        };
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_vault_bindings
            SET lifecycle_state = 'active', active_from_slot = COALESCE(active_from_slot, $2),
                activated_at = COALESCE(activated_at, now()),
                predecessor_binding_id = $3,
                rollback_until = $4, updated_at = now()
            WHERE id = $1 AND lifecycle_state IN ('preparing', 'warming')
            RETURNING *
            "#,
        )
        .bind(candidate.id)
        .bind(observed_slot)
        .bind(predecessor.as_ref().map(|binding| binding.id))
        .bind(rollback_until)
        .fetch_one(&mut *tx)
        .await?;
        let active = lookup_table_binding_from_row(&row)?;
        tx.commit().await?;
        Ok(LookupTableBindingHeadFlip {
            active,
            predecessor,
        })
    }

    pub async fn activate_lookup_table_family_generation(
        &self,
        family_id: i64,
        target_generation: i32,
        rollback_until: DateTime<Utc>,
    ) -> Result<LookupTableFamilyRecord, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        let family_row =
            sqlx::query("SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 FOR UPDATE")
                .bind(family_id)
                .fetch_optional(&mut *tx)
                .await?
                .ok_or_else(|| stale_store_update("lookup-table family", family_id))?;
        let family = lookup_table_family_from_row(&family_row)?;
        let mut affected_generations = vec![target_generation];
        if let Some(active_generation) = family.active_generation {
            affected_generations.push(active_generation);
        }
        affected_generations.sort_unstable();
        affected_generations.dedup();
        let affected_rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.route_lookup_tables
            WHERE family_id = $1 AND generation = ANY($2)
            ORDER BY id
            FOR UPDATE
            "#,
        )
        .bind(family_id)
        .bind(&affected_generations)
        .fetch_all(&mut *tx)
        .await?;
        let target_rows = affected_rows
            .iter()
            .filter(|row| row.try_get::<i32, _>("generation").ok() == Some(target_generation))
            .collect::<Vec<_>>();
        if target_rows.is_empty()
            || target_rows.iter().any(|row| {
                !matches!(
                    row.try_get::<Option<String>, _>("desired_state")
                        .ok()
                        .flatten()
                        .as_deref(),
                    Some("active" | "standby")
                ) || row
                    .try_get::<Option<i32>, _>("usable_address_count")
                    .ok()
                    .flatten()
                    != row.try_get::<i32, _>("address_count").ok()
            })
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "family {family_id} generation {target_generation} is not fully active and usable"
            )));
        }
        let affected_table_ids = affected_rows
            .iter()
            .map(|row| row.try_get::<i64, _>("id"))
            .collect::<Result<Vec<_>, _>>()?;
        let live_usage_count: i64 = if affected_table_ids.is_empty() {
            0
        } else {
            sqlx::query_scalar(
                r#"
                SELECT count(*) FROM loyal_yield.lookup_table_usage_leases
                WHERE route_lookup_table_id = ANY($1)
                  AND released_at IS NULL AND expires_at > now()
                "#,
            )
            .bind(&affected_table_ids)
            .fetch_one(&mut *tx)
            .await?
        };
        if live_usage_count != 0 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "family {family_id} generation pointer has an unexpired usage lease"
            )));
        }
        if let Some(current_generation) = family.active_generation {
            if current_generation != target_generation {
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.route_lookup_tables
                    SET desired_state = 'standby', accepting_allocations = FALSE,
                        rollback_until = $3, updated_at = now()
                    WHERE family_id = $1 AND generation = $2 AND desired_state = 'active'
                    "#,
                )
                .bind(family_id)
                .bind(current_generation)
                .bind(rollback_until)
                .execute(&mut *tx)
                .await?;
            }
        }
        sqlx::query(
            r#"
            UPDATE loyal_yield.route_lookup_tables
            SET desired_state = 'active', status = 'usable',
                rollback_until = $3, updated_at = now()
            WHERE family_id = $1 AND generation = $2
            "#,
        )
        .bind(family_id)
        .bind(target_generation)
        .bind(rollback_until)
        .execute(&mut *tx)
        .await?;
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_families
            SET previous_generation = CASE
                    WHEN active_generation IS DISTINCT FROM $2 THEN active_generation
                    ELSE previous_generation END,
                active_generation = $2,
                rollback_until = $3,
                updated_at = now()
            WHERE id = $1
            RETURNING *
            "#,
        )
        .bind(family_id)
        .bind(target_generation)
        .bind(rollback_until)
        .fetch_one(&mut *tx)
        .await?;
        let family = lookup_table_family_from_row(&row)?;
        tx.commit().await?;
        Ok(family)
    }

    pub async fn rollback_lookup_table_family_generation(
        &self,
        family_id: i64,
    ) -> Result<LookupTableFamilyRecord, OrchestratorError> {
        let family_row =
            sqlx::query("SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1")
                .bind(family_id)
                .fetch_optional(self.pool())
                .await?
                .ok_or_else(|| stale_store_update("lookup-table family", family_id))?;
        let family = lookup_table_family_from_row(&family_row)?;
        if !family
            .rollback_until
            .is_some_and(|rollback_until| rollback_until > Utc::now())
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "family {family_id} rollback window is not active"
            )));
        }
        let previous = family.previous_generation.ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "family {family_id} has no previous generation"
            ))
        })?;
        self.activate_lookup_table_family_generation(
            family_id,
            previous,
            family.rollback_until.expect("checked above"),
        )
        .await
    }

    pub async fn rollback_lookup_table_binding_head(
        &self,
        active_binding_id: i64,
        observed_slot: i64,
    ) -> Result<LookupTableBindingHeadFlip, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        let active_row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_vault_bindings WHERE id = $1 AND lifecycle_state = 'active' FOR UPDATE",
        )
        .bind(active_binding_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("active lookup-table binding", active_binding_id))?;
        let active = lookup_table_binding_from_row(&active_row)?;
        if !active
            .rollback_until
            .is_some_and(|rollback_until| rollback_until > Utc::now())
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {active_binding_id} rollback window is not active"
            )));
        }
        let predecessor_id = active.predecessor_binding_id.ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "binding {active_binding_id} has no predecessor"
            ))
        })?;
        let predecessor_row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_vault_bindings WHERE id = $1 AND lifecycle_state = 'standby' FOR UPDATE",
        )
        .bind(predecessor_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("standby predecessor binding", predecessor_id))?;
        let predecessor_candidate = lookup_table_binding_from_row(&predecessor_row)?;
        if predecessor_candidate.vault_id != active.vault_id
            || predecessor_candidate.family_id != active.family_id
            || predecessor_candidate.binding_ordinal != active.binding_ordinal
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {predecessor_id} is not a predecessor in the same vault/family head"
            )));
        }
        let mut affected_table_ids = vec![
            active.route_lookup_table_id,
            predecessor_candidate.route_lookup_table_id,
        ];
        affected_table_ids.sort_unstable();
        affected_table_ids.dedup();
        let locked_table_ids = sqlx::query_scalar::<_, i64>(
            r#"
            SELECT id FROM loyal_yield.route_lookup_tables
            WHERE id = ANY($1)
            ORDER BY id
            FOR UPDATE
            "#,
        )
        .bind(&affected_table_ids)
        .fetch_all(&mut *tx)
        .await?;
        if locked_table_ids != affected_table_ids {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {active_binding_id} rollback references a missing physical table"
            )));
        }
        let live_usage_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*) FROM loyal_yield.lookup_table_usage_leases
            WHERE route_lookup_table_id = ANY($1)
              AND released_at IS NULL AND expires_at > now()
            "#,
        )
        .bind(&affected_table_ids)
        .fetch_one(&mut *tx)
        .await?;
        if live_usage_count != 0 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "binding {active_binding_id} rollback has an unexpired usage lease"
            )));
        }
        let restored_desired_revision = upsert_vault_desired_head_in_tx(
            &mut tx,
            active.family_id,
            active.vault_id,
            active.binding_ordinal,
            predecessor_candidate.manifest_id,
        )
        .await?;
        supersede_stale_vault_binding_revisions_in_tx(
            &mut tx,
            active.family_id,
            active.vault_id,
            active.binding_ordinal,
            predecessor_candidate.manifest_id,
            restored_desired_revision,
        )
        .await?;
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_vault_bindings
            SET lifecycle_state = 'standby', active_until_slot = $2, updated_at = now()
            WHERE id = $1 AND lifecycle_state = 'active'
            RETURNING *
            "#,
        )
        .bind(active_binding_id)
        .bind(observed_slot)
        .fetch_one(&mut *tx)
        .await?;
        let demoted = lookup_table_binding_from_row(&row)?;
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_vault_bindings
            SET lifecycle_state = 'active', active_until_slot = NULL,
                active_from_slot = $2, activated_at = now(),
                desired_head_revision = $3, updated_at = now()
            WHERE id = $1 AND lifecycle_state = 'standby'
            RETURNING *
            "#,
        )
        .bind(predecessor_id)
        .bind(observed_slot)
        .bind(restored_desired_revision)
        .fetch_one(&mut *tx)
        .await?;
        let restored = lookup_table_binding_from_row(&row)?;
        tx.commit().await?;
        Ok(LookupTableBindingHeadFlip {
            active: restored,
            predecessor: Some(demoted),
        })
    }

    /// Finalizes an explicitly recorded rollback window after it expires.
    /// This releases standby binding reservations and makes the previous
    /// generation eligible for the separately fenced cleanup workflow.
    pub async fn finalize_expired_lookup_table_rollbacks(
        &self,
        family_id: i64,
    ) -> Result<LookupTableRollbackFinalization, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        let family_row =
            sqlx::query("SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 FOR UPDATE")
                .bind(family_id)
                .fetch_optional(&mut *tx)
                .await?
                .ok_or_else(|| stale_store_update("lookup-table family", family_id))?;
        let family = lookup_table_family_from_row(&family_row)?;
        let now = Utc::now();
        if family.rollback_until.is_some_and(|until| until > now) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "family {family_id} rollback window is still active"
            )));
        }
        let standby_rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_vault_bindings
            WHERE family_id = $1 AND lifecycle_state = 'standby'
            ORDER BY id FOR UPDATE
            "#,
        )
        .bind(family_id)
        .fetch_all(&mut *tx)
        .await?;
        if standby_rows.iter().any(|row| {
            row.try_get::<Option<DateTime<Utc>>, _>("rollback_until")
                .ok()
                .flatten()
                .is_none_or(|until| until > now)
        }) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "family {family_id} has a standby binding without an expired rollback window"
            )));
        }
        let previous_generation = family.previous_generation;
        if previous_generation.is_some() && family.rollback_until.is_none() {
            return Err(OrchestratorError::StoreInvariant(format!(
                "family {family_id} previous generation has no explicit rollback deadline"
            )));
        }
        let previous_table_rows = if let Some(previous_generation) = previous_generation {
            sqlx::query(
                r#"
                SELECT * FROM loyal_yield.route_lookup_tables
                WHERE family_id = $1 AND generation = $2
                ORDER BY id FOR UPDATE
                "#,
            )
            .bind(family_id)
            .bind(previous_generation)
            .fetch_all(&mut *tx)
            .await?
        } else {
            Vec::new()
        };
        let previous_table_ids = previous_table_rows
            .iter()
            .map(|row| row.try_get::<i64, _>("id"))
            .collect::<Result<Vec<_>, _>>()?;
        if previous_generation.is_some()
            && (previous_table_rows.is_empty()
                || previous_table_rows.iter().any(|row| {
                    row.try_get::<Option<String>, _>("desired_state")
                        .ok()
                        .flatten()
                        .as_deref()
                        != Some("standby")
                }))
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "family {family_id} previous generation is not entirely standby"
            )));
        }
        if !previous_table_ids.is_empty() {
            let usage_count: i64 = sqlx::query_scalar(
                r#"
                SELECT count(*) FROM loyal_yield.lookup_table_usage_leases
                WHERE route_lookup_table_id = ANY($1)
                  AND released_at IS NULL AND expires_at > now()
                "#,
            )
            .bind(&previous_table_ids)
            .fetch_one(&mut *tx)
            .await?;
            let pending_count: i64 = sqlx::query_scalar(
                r#"
                SELECT count(*) FROM loyal_yield.lookup_table_operations
                WHERE route_lookup_table_id = ANY($1)
                  AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
                "#,
            )
            .bind(&previous_table_ids)
            .fetch_one(&mut *tx)
            .await?;
            if usage_count != 0 || pending_count != 0 {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "family {family_id} previous generation still has live leases or operations"
                )));
            }
        }

        let affected_table_ids = standby_rows
            .iter()
            .map(|row| row.try_get::<i64, _>("route_lookup_table_id"))
            .collect::<Result<BTreeSet<_>, _>>()?
            .into_iter()
            .collect::<Vec<_>>();
        let reserved_before: i64 = if affected_table_ids.is_empty() {
            0
        } else {
            sqlx::query_scalar(
                r#"
                SELECT COALESCE(sum(reserved_address_count), 0)::BIGINT
                FROM loyal_yield.route_lookup_tables WHERE id = ANY($1)
                "#,
            )
            .bind(&affected_table_ids)
            .fetch_one(&mut *tx)
            .await?
        };
        let retired_binding_ids = if standby_rows.is_empty() {
            Vec::new()
        } else {
            sqlx::query_scalar::<_, i64>(
                r#"
                UPDATE loyal_yield.lookup_table_vault_bindings
                SET lifecycle_state = 'retired',
                    deactivated_at = COALESCE(deactivated_at, now()),
                    rollback_until = NULL, updated_at = now()
                WHERE family_id = $1 AND lifecycle_state = 'standby'
                  AND rollback_until <= now()
                RETURNING id
                "#,
            )
            .bind(family_id)
            .fetch_all(&mut *tx)
            .await?
        };
        // A table that just lost an old binding head is sealed immediately.
        // If no other binding/lease/operation references it, a current-generation
        // vault shard is also safe to retire; shared-market heads remain protected.
        if !affected_table_ids.is_empty() {
            sqlx::query(
                r#"
                UPDATE loyal_yield.route_lookup_tables
                SET accepting_allocations = FALSE, updated_at = now()
                WHERE id = ANY($1)
                  AND allocation_kind IN ('vault_shard', 'dedicated_vault')
                "#,
            )
            .bind(&affected_table_ids)
            .execute(&mut *tx)
            .await?;
        }

        if !previous_table_ids.is_empty() {
            let live_binding_count: i64 = sqlx::query_scalar(
                r#"
                SELECT count(*) FROM loyal_yield.lookup_table_vault_bindings
                WHERE route_lookup_table_id = ANY($1)
                  AND lifecycle_state IN ('preparing', 'warming', 'active', 'standby', 'retiring')
                "#,
            )
            .bind(&previous_table_ids)
            .fetch_one(&mut *tx)
            .await?;
            if live_binding_count != 0 {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "family {family_id} previous generation still has live bindings"
                )));
            }
        }

        let previous_retiring_table_ids = if let Some(previous_generation) = previous_generation {
            sqlx::query_scalar::<_, i64>(
                r#"
                UPDATE loyal_yield.route_lookup_tables
                SET desired_state = 'retiring', accepting_allocations = FALSE,
                    rollback_until = NULL, updated_at = now()
                WHERE family_id = $1 AND generation = $2
                  AND desired_state = 'standby'
                RETURNING id
                "#,
            )
            .bind(family_id)
            .bind(previous_generation)
            .fetch_all(&mut *tx)
            .await?
        } else {
            Vec::new()
        };
        let current_zero_reference_table_ids = if affected_table_ids.is_empty() {
            Vec::new()
        } else {
            sqlx::query_scalar::<_, i64>(
                r#"
                UPDATE loyal_yield.route_lookup_tables route_table
                SET desired_state = 'retiring', accepting_allocations = FALSE,
                    rollback_until = NULL, updated_at = now()
                WHERE route_table.id = ANY($1)
                  AND route_table.allocation_kind IN ('vault_shard', 'dedicated_vault')
                  AND route_table.desired_state IN ('active', 'standby')
                  AND NOT EXISTS (
                      SELECT 1 FROM loyal_yield.lookup_table_vault_bindings binding
                      WHERE binding.route_lookup_table_id = route_table.id
                        AND binding.lifecycle_state IN (
                            'preparing', 'warming', 'active', 'standby', 'retiring'
                        )
                  )
                  AND NOT EXISTS (
                      SELECT 1 FROM loyal_yield.lookup_table_usage_leases usage
                      WHERE usage.route_lookup_table_id = route_table.id
                        AND usage.released_at IS NULL AND usage.expires_at > now()
                  )
                  AND NOT EXISTS (
                      SELECT 1 FROM loyal_yield.lookup_table_operations operation
                      WHERE operation.route_lookup_table_id = route_table.id
                        AND operation.operation_state NOT IN (
                            'complete', 'permanent_failure', 'cancelled'
                        )
                  )
                RETURNING route_table.id
                "#,
            )
            .bind(&affected_table_ids)
            .fetch_all(&mut *tx)
            .await?
        };
        let mut retiring_table_ids = previous_retiring_table_ids
            .into_iter()
            .chain(current_zero_reference_table_ids)
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect::<Vec<_>>();
        retiring_table_ids.sort_unstable();
        if previous_generation.is_some() {
            sqlx::query(
                r#"
                UPDATE loyal_yield.lookup_table_families
                SET previous_generation = NULL, rollback_until = NULL, updated_at = now()
                WHERE id = $1
                "#,
            )
            .bind(family_id)
            .execute(&mut *tx)
            .await?;
        }
        let reserved_after: i64 = if affected_table_ids.is_empty() {
            0
        } else {
            sqlx::query_scalar(
                r#"
                SELECT COALESCE(sum(reserved_address_count), 0)::BIGINT
                FROM loyal_yield.route_lookup_tables WHERE id = ANY($1)
                "#,
            )
            .bind(&affected_table_ids)
            .fetch_one(&mut *tx)
            .await?
        };
        let released_reserved_capacity =
            i32::try_from(reserved_before - reserved_after).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "released lookup-table reservation exceeds i32".to_owned(),
                )
            })?;
        if retired_binding_ids.is_empty() && retiring_table_ids.is_empty() {
            return Err(OrchestratorError::StoreInvariant(format!(
                "family {family_id} has no expired rollback references to finalize"
            )));
        }
        tx.commit().await?;
        Ok(LookupTableRollbackFinalization {
            family_id,
            cleared_previous_generation: previous_generation,
            retired_binding_ids,
            retiring_table_ids,
            released_reserved_capacity,
        })
    }
}

impl NeonSqlClient {
    pub async fn upsert_lookup_table_usage_leases(
        &self,
        mut bundle: LookupTableUsageLeaseBundle,
    ) -> Result<Vec<LookupTableUsageLeaseRecord>, OrchestratorError> {
        bundle.route_lookup_table_ids.sort();
        bundle.route_lookup_table_ids.dedup();
        if bundle.route_lookup_table_ids.is_empty() || bundle.expires_at <= Utc::now() {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table usage lease bundle must be nonempty and unexpired".to_owned(),
            ));
        }

        for attempt in 1..=LOOKUP_TABLE_DB_CONCURRENCY_MAX_ATTEMPTS {
            match self
                .upsert_lookup_table_usage_leases_once(bundle.clone())
                .await
            {
                Ok(leases) => return Ok(leases),
                Err(error) => {
                    let Some(sqlstate) = retryable_lookup_table_database_conflict(&error) else {
                        return Err(error);
                    };
                    if attempt == LOOKUP_TABLE_DB_CONCURRENCY_MAX_ATTEMPTS {
                        return Err(error);
                    }
                    log_lookup_table_database_retry(
                        "upsert_lookup_table_usage_leases",
                        sqlstate,
                        attempt,
                    );
                    sleep_for_lookup_table_database_retry(attempt).await;
                }
            }
        }
        unreachable!("bounded lookup-table database retry returns on its final attempt")
    }

    async fn upsert_lookup_table_usage_leases_once(
        &self,
        bundle: LookupTableUsageLeaseBundle,
    ) -> Result<Vec<LookupTableUsageLeaseRecord>, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        // Route persistence is intentionally independent across physical
        // tables. Lock the selected rows in canonical id order so cleanup or
        // mutation of one table cannot race a new lease without serializing
        // unrelated routes across the cluster.
        let locked_tables = sqlx::query(
            r#"
            SELECT id, family_id, allocation_kind, desired_state, status
            FROM loyal_yield.route_lookup_tables
            WHERE id = ANY($1) AND cluster = $2
            ORDER BY id
            FOR SHARE
            "#,
        )
        .bind(&bundle.route_lookup_table_ids)
        .bind(&bundle.cluster)
        .fetch_all(&mut *tx)
        .await?;
        if locked_tables.len() != bundle.route_lookup_table_ids.len() {
            return Err(OrchestratorError::StoreInvariant(
                "usage lease selection contains a missing or cross-cluster lookup table".to_owned(),
            ));
        }
        for row in &locked_tables {
            let family_id: Option<i64> = row.try_get("family_id")?;
            let selectable = if family_id.is_some() {
                row.try_get::<Option<String>, _>("desired_state")?
                    .as_deref()
                    == Some("active")
            } else {
                matches!(
                    row.try_get::<String, _>("status")?.as_str(),
                    "active" | "warming" | "usable"
                )
            };
            if !selectable {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "lookup table {} stopped accepting new usage leases",
                    row.try_get::<i64, _>("id")?
                )));
            }
        }
        let vault_table_ids = locked_tables
            .iter()
            .filter_map(|row| {
                matches!(
                    row.try_get::<Option<String>, _>("allocation_kind")
                        .ok()
                        .flatten()
                        .as_deref(),
                    Some("vault_shard" | "dedicated_vault")
                )
                .then(|| row.try_get::<i64, _>("id").ok())
                .flatten()
            })
            .collect::<Vec<_>>();
        if !vault_table_ids.is_empty() {
            let (Some(vault_id), Some(binding_id)) = (bundle.vault_id, bundle.binding_id) else {
                return Err(OrchestratorError::StoreInvariant(
                    "vault lookup-table usage lease requires its active vault binding".to_owned(),
                ));
            };
            let active_bound_table_ids = sqlx::query_scalar::<_, i64>(
                r#"
                SELECT route_lookup_table_id
                FROM loyal_yield.lookup_table_vault_bindings
                WHERE id = $1 AND vault_id = $2 AND lifecycle_state = 'active'
                  AND route_lookup_table_id = ANY($3)
                ORDER BY route_lookup_table_id
                FOR SHARE
                "#,
            )
            .bind(binding_id)
            .bind(vault_id.as_i64())
            .bind(&vault_table_ids)
            .fetch_all(&mut *tx)
            .await?;
            if active_bound_table_ids != vault_table_ids {
                return Err(OrchestratorError::StoreInvariant(
                    "vault lookup-table usage lease binding is no longer the active head"
                        .to_owned(),
                ));
            }
        }
        let mutation_operation_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*) FROM loyal_yield.lookup_table_operations
            WHERE route_lookup_table_id = ANY($1)
              AND operation_kind IN ('create', 'extend', 'rollover', 'deactivate', 'close')
              AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
            "#,
        )
        .bind(&bundle.route_lookup_table_ids)
        .fetch_one(&mut *tx)
        .await?;
        if mutation_operation_count != 0 {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table usage lease races with a nonterminal mutation operation".to_owned(),
            ));
        }
        for table_id in &bundle.route_lookup_table_ids {
            sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_usage_leases
                    (cluster, lease_kind, reference_key, route_lookup_table_id,
                     vault_id, binding_id, route_fingerprint,
                     requirements_fingerprint, expires_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
                ON CONFLICT (lease_kind, reference_key, route_lookup_table_id) DO UPDATE SET
                    cluster = EXCLUDED.cluster,
                    vault_id = EXCLUDED.vault_id,
                    binding_id = EXCLUDED.binding_id,
                    route_fingerprint = EXCLUDED.route_fingerprint,
                    requirements_fingerprint = EXCLUDED.requirements_fingerprint,
                    expires_at = EXCLUDED.expires_at,
                    released_at = NULL,
                    updated_at = now()
                "#,
            )
            .bind(&bundle.cluster)
            .bind(bundle.lease_kind.as_str())
            .bind(&bundle.reference_key)
            .bind(*table_id)
            .bind(bundle.vault_id.map(VaultId::as_i64))
            .bind(bundle.binding_id)
            .bind(&bundle.route_fingerprint)
            .bind(&bundle.requirements_fingerprint)
            .bind(bundle.expires_at)
            .execute(&mut *tx)
            .await?;
        }
        let rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_usage_leases
            WHERE lease_kind = $1 AND reference_key = $2 AND released_at IS NULL
            ORDER BY route_lookup_table_id
            "#,
        )
        .bind(bundle.lease_kind.as_str())
        .bind(&bundle.reference_key)
        .fetch_all(&mut *tx)
        .await?;
        tx.commit().await?;
        rows.iter().map(lookup_table_usage_lease_from_row).collect()
    }

    pub async fn validate_lookup_table_usage_leases(
        &self,
        lease_kind: LookupTableUsageLeaseKind,
        reference_key: &str,
        expected_table_ids: &[i64],
        requirements_fingerprint: &str,
        valid_through: DateTime<Utc>,
    ) -> Result<(), OrchestratorError> {
        let mut expected = expected_table_ids.to_vec();
        expected.sort();
        expected.dedup();
        let rows = sqlx::query(
            r#"
            SELECT route_lookup_table_id, requirements_fingerprint
            FROM loyal_yield.lookup_table_usage_leases
            WHERE lease_kind = $1 AND reference_key = $2
              AND released_at IS NULL AND expires_at >= $3
            ORDER BY route_lookup_table_id
            "#,
        )
        .bind(lease_kind.as_str())
        .bind(reference_key)
        .bind(valid_through)
        .fetch_all(self.pool())
        .await?;
        let actual = rows
            .iter()
            .map(|row| row.try_get::<i64, _>("route_lookup_table_id"))
            .collect::<Result<Vec<_>, _>>()?;
        let fingerprints_match = rows.iter().all(|row| {
            row.try_get::<Option<String>, _>("requirements_fingerprint")
                .ok()
                .flatten()
                .as_deref()
                == Some(requirements_fingerprint)
        });
        if actual != expected || !fingerprints_match {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table usage lease {reference_key:?} expired or selection/fingerprint changed"
            )));
        }
        Ok(())
    }

    pub async fn release_lookup_table_usage_leases(
        &self,
        lease_kind: LookupTableUsageLeaseKind,
        reference_key: &str,
    ) -> Result<u64, OrchestratorError> {
        let result = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_usage_leases
            SET released_at = COALESCE(released_at, now()), updated_at = now()
            WHERE lease_kind = $1 AND reference_key = $2 AND released_at IS NULL
            "#,
        )
        .bind(lease_kind.as_str())
        .bind(reference_key)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected())
    }

    pub async fn lookup_table_has_active_usage_lease(
        &self,
        route_lookup_table_id: i64,
    ) -> Result<bool, OrchestratorError> {
        Ok(sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.lookup_table_usage_leases
                WHERE route_lookup_table_id = $1
                  AND released_at IS NULL
                  AND expires_at > now()
            )
            "#,
        )
        .bind(route_lookup_table_id)
        .fetch_one(self.pool())
        .await?)
    }

    /// Returns the complete durable legacy fleet that must be verified as one
    /// unit. Import intentionally never selects reusable-family rows.
    pub async fn legacy_lookup_tables_for_import(
        &self,
        cluster: &str,
    ) -> Result<Vec<LegacyLookupTableImportSource>, OrchestratorError> {
        let rows = sqlx::query(
            r#"
            SELECT id, cluster, scope, table_address, authority, status, durable,
                   address_count, address_hash, addresses, legacy_kind,
                   legacy_import_run_id, last_extended_slot,
                   last_extended_start_index, last_verified_slot, last_verified_at
            FROM loyal_yield.route_lookup_tables
            WHERE cluster = $1
              AND family_id IS NULL
              AND durable = TRUE
              AND status IN ('active', 'warming', 'usable')
            ORDER BY id
            "#,
        )
        .bind(cluster)
        .fetch_all(self.pool())
        .await?;
        rows.iter()
            .map(legacy_lookup_table_import_source_from_row)
            .collect()
    }

    /// Returns the immutable imported legacy fleet that cleanup must account
    /// for, including rows whose physical ALT has already been closed. This is
    /// deliberately a database inventory rather than an ALT-program scan: the
    /// import run and per-table evidence are the durable boundary, while RPC is
    /// used later to verify the current finalized lifecycle of every member.
    pub async fn imported_legacy_lookup_table_cleanup_fleet(
        &self,
        cluster: &str,
    ) -> Result<Vec<ImportedLegacyLookupTableCleanupRecord>, OrchestratorError> {
        if cluster.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup fleet requires a cluster".to_owned(),
            ));
        }
        let expected_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*)
            FROM loyal_yield.route_lookup_tables
            WHERE cluster = $1 AND family_id IS NULL
              AND legacy_import_run_id IS NOT NULL
            "#,
        )
        .bind(cluster)
        .fetch_one(self.pool())
        .await?;
        let rows = sqlx::query(
            r#"
            SELECT table_record.id, table_record.cluster, table_record.scope,
                   table_record.table_address, table_record.authority,
                   table_record.status, table_record.durable,
                   table_record.address_count, table_record.address_hash,
                   table_record.addresses, table_record.legacy_kind,
                   table_record.legacy_import_run_id,
                   table_record.last_extended_slot,
                   table_record.last_extended_start_index,
                   table_record.last_verified_slot,
                   table_record.last_verified_at,
                   table_record.deactivated_slot,
                   table_record.deactivate_signature,
                   table_record.closed_signature,
                   table_record.close_recipient,
                   table_record.reclaimed_lamports,
                   import_run.cluster AS import_cluster,
                   import_run.legacy_kind AS import_legacy_kind,
                   import_run.import_fingerprint,
                   import_run.verified_slot AS import_verified_slot,
                   evidence.table_address AS evidence_table_address,
                   evidence.scope AS evidence_scope,
                   evidence.expected_authority AS evidence_expected_authority,
                   evidence.observed_authority AS evidence_observed_authority,
                   evidence.observed_owner AS evidence_observed_owner,
                   evidence.address_count AS evidence_address_count,
                   evidence.address_hash AS evidence_address_hash,
                   evidence.addresses AS evidence_addresses,
                   evidence.verified_slot AS evidence_verified_slot
            FROM loyal_yield.route_lookup_tables table_record
            JOIN loyal_yield.lookup_table_legacy_import_runs import_run
              ON import_run.id = table_record.legacy_import_run_id
            JOIN loyal_yield.lookup_table_legacy_import_evidence evidence
              ON evidence.import_run_id = table_record.legacy_import_run_id
             AND evidence.route_lookup_table_id = table_record.id
            WHERE table_record.cluster = $1
              AND table_record.family_id IS NULL
              AND table_record.legacy_import_run_id IS NOT NULL
            ORDER BY table_record.id
            "#,
        )
        .bind(cluster)
        .fetch_all(self.pool())
        .await?;
        if i64::try_from(rows.len()).ok() != Some(expected_count) {
            return Err(OrchestratorError::StoreInvariant(
                "imported legacy cleanup fleet is missing immutable import evidence".to_owned(),
            ));
        }
        rows.iter()
            .map(|row| {
                let source = legacy_lookup_table_import_source_from_row(row)?;
                let evidence_addresses =
                    serde_json::from_value::<Vec<String>>(row.try_get("evidence_addresses")?)
                        .map_err(|error| {
                            OrchestratorError::StoreInvariant(format!(
                                "legacy cleanup import evidence addresses are invalid: {error}"
                            ))
                        })?;
                let import_fingerprint: String = row.try_get("import_fingerprint")?;
                let import_verified_slot: i64 = row.try_get("import_verified_slot")?;
                let legacy_kind = source.legacy_kind.ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "imported legacy cleanup row has no legacy kind".to_owned(),
                    )
                })?;
                let identity_matches = source.cluster == cluster
                    && source.familyless_import_identity_is_valid()
                    && row.try_get::<String, _>("import_cluster")? == cluster
                    && row.try_get::<String, _>("import_legacy_kind")? == legacy_kind.as_str()
                    && is_sha256_hex(&import_fingerprint)
                    && import_verified_slot >= 0
                    && row.try_get::<String, _>("evidence_table_address")? == source.table_address
                    && row.try_get::<String, _>("evidence_scope")? == source.scope
                    && row.try_get::<String, _>("evidence_expected_authority")? == source.authority
                    && row.try_get::<String, _>("evidence_observed_authority")? == source.authority
                    && row.try_get::<String, _>("evidence_observed_owner")?
                        == address_lookup_table_program::id().to_string()
                    && row.try_get::<i32, _>("evidence_address_count")? == source.address_count
                    && row.try_get::<String, _>("evidence_address_hash")? == source.address_hash
                    && evidence_addresses == source.addresses
                    && row.try_get::<i64, _>("evidence_verified_slot")? == import_verified_slot;
                if !identity_matches {
                    return Err(OrchestratorError::StoreInvariant(format!(
                        "imported legacy cleanup identity drifted for {}",
                        source.table_address
                    )));
                }
                Ok(ImportedLegacyLookupTableCleanupRecord {
                    source,
                    import_fingerprint,
                    import_verified_slot,
                    deactivated_slot: row.try_get("deactivated_slot")?,
                    deactivate_signature: row.try_get("deactivate_signature")?,
                    closed_signature: row.try_get("closed_signature")?,
                    close_recipient: row.try_get("close_recipient")?,
                    reclaimed_lamports: row.try_get("reclaimed_lamports")?,
                })
            })
            .collect()
    }

    /// Persists a successful fleet verification in one transaction. The RPC
    /// phase happens before this method is called; this transaction locks and
    /// rechecks the entire eligible fleet so a concurrent registry change
    /// causes zero import writes.
    pub async fn import_verified_legacy_lookup_table_fleet(
        &self,
        input: LegacyLookupTableFleetImportRequest,
    ) -> Result<LegacyLookupTableFleetImportResult, OrchestratorError> {
        validate_legacy_lookup_table_fleet_import(&input)?;
        let canonical_fingerprint = legacy_lookup_table_import_fingerprint(
            &input.cluster,
            &input.rpc_genesis_hash,
            input.verified_slot,
            &input.tables,
        );
        if input.import_fingerprint != canonical_fingerprint {
            return Err(OrchestratorError::StoreInvariant(
                "legacy lookup-table import fingerprint is not canonical".to_owned(),
            ));
        }
        let legacy_kind = input.tables[0].legacy_kind;
        let imported_table_count = i32::try_from(input.tables.len()).map_err(|_| {
            OrchestratorError::StoreInvariant(
                "legacy lookup-table fleet size does not fit PostgreSQL INTEGER".to_owned(),
            )
        })?;
        let mut tx = self.pool().begin().await?;
        sqlx::query("SET TRANSACTION ISOLATION LEVEL SERIALIZABLE")
            .execute(&mut *tx)
            .await?;
        sqlx::query("LOCK TABLE loyal_yield.route_lookup_tables IN SHARE ROW EXCLUSIVE MODE")
            .execute(&mut *tx)
            .await?;
        sqlx::query(
            "SELECT pg_advisory_xact_lock(hashtextextended('legacy-alt-import:' || $1, 0))",
        )
        .bind(&input.cluster)
        .execute(&mut *tx)
        .await?;

        let locked_rows = sqlx::query(
            r#"
            SELECT id, cluster, scope, table_address, authority, status, durable,
                   address_count, address_hash, addresses, legacy_kind,
                   legacy_import_run_id, last_extended_slot,
                   last_extended_start_index, last_verified_slot, last_verified_at
            FROM loyal_yield.route_lookup_tables
            WHERE cluster = $1
              AND family_id IS NULL
              AND durable = TRUE
              AND status IN ('active', 'warming', 'usable')
            ORDER BY id
            FOR UPDATE
            "#,
        )
        .bind(&input.cluster)
        .fetch_all(&mut *tx)
        .await?;
        let locked_sources = locked_rows
            .iter()
            .map(legacy_lookup_table_import_source_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let expected_sources = input
            .tables
            .iter()
            .map(|table| table.source.clone())
            .collect::<Vec<_>>();
        if locked_sources != expected_sources {
            return Err(OrchestratorError::StoreInvariant(
                "legacy lookup-table fleet changed after RPC verification".to_owned(),
            ));
        }

        let inserted_import_run_id = sqlx::query_scalar::<_, i64>(
            r#"
            INSERT INTO loyal_yield.lookup_table_legacy_import_runs
                (cluster, rpc_genesis_hash, verified_slot, verified_at,
                 legacy_kind, expected_table_count, verified_table_count,
                 import_fingerprint, reason, updated_by)
            VALUES ($1, $2, $3, $4, $5, $6, $6, $7, $8, $9)
            ON CONFLICT (cluster, import_fingerprint) DO NOTHING
            RETURNING id
            "#,
        )
        .bind(&input.cluster)
        .bind(&input.rpc_genesis_hash)
        .bind(input.verified_slot)
        .bind(input.verified_at)
        .bind(legacy_kind.as_str())
        .bind(imported_table_count)
        .bind(&input.import_fingerprint)
        .bind(&input.reason)
        .bind(&input.updated_by)
        .fetch_optional(&mut *tx)
        .await?;
        let (import_run_id, effective_verified_at, replayed) = match inserted_import_run_id {
            Some(id) => (id, input.verified_at, false),
            None => {
                let existing = sqlx::query(
                    r#"
                    SELECT id, rpc_genesis_hash, verified_slot, verified_at,
                           legacy_kind, expected_table_count, verified_table_count
                    FROM loyal_yield.lookup_table_legacy_import_runs
                    WHERE cluster = $1 AND import_fingerprint = $2
                    "#,
                )
                .bind(&input.cluster)
                .bind(&input.import_fingerprint)
                .fetch_one(&mut *tx)
                .await?;
                let id: i64 = existing.try_get("id")?;
                let existing_verified_at: DateTime<Utc> = existing.try_get("verified_at")?;
                let exact_run_matches = existing.try_get::<String, _>("rpc_genesis_hash")?
                    == input.rpc_genesis_hash
                    && existing.try_get::<i64, _>("verified_slot")? == input.verified_slot
                    && existing.try_get::<String, _>("legacy_kind")? == legacy_kind.as_str()
                    && existing.try_get::<i32, _>("expected_table_count")? == imported_table_count
                    && existing.try_get::<i32, _>("verified_table_count")? == imported_table_count;
                let registry_matches_existing_run = locked_sources.iter().all(|source| {
                    source.legacy_import_run_id == Some(id)
                        && source.last_verified_slot == Some(input.verified_slot)
                        && source.last_verified_at == Some(existing_verified_at)
                        && source.legacy_kind == Some(legacy_kind)
                });
                if !exact_run_matches || !registry_matches_existing_run {
                    return Err(OrchestratorError::StoreInvariant(
                        "legacy lookup-table import fingerprint conflicts with existing evidence"
                            .to_owned(),
                    ));
                }
                (id, existing_verified_at, true)
            }
        };

        // The pre-reusable writer stored a set-style digest (sorted Base58
        // strings separated by NUL bytes). Accepting that digest is confined
        // to the finalized-RPC import boundary. Before immutable evidence is
        // inserted, normalize every such row to the reusable-v2 ordered
        // digest derived from the exact RPC membership. The fleet lock plus
        // this CAS makes the normalization all-or-nothing with the import.
        if !replayed {
            for table in &input.tables {
                if table.source.address_hash == table.observed_address_hash {
                    continue;
                }
                let addresses = serde_json::to_value(&table.source.addresses).map_err(|error| {
                    OrchestratorError::StoreInvariant(format!(
                        "legacy lookup-table address normalization could not be encoded: {error}"
                    ))
                })?;
                let normalized = sqlx::query(
                    r#"
                    UPDATE loyal_yield.route_lookup_tables
                    SET address_hash = $2,
                        updated_at = now()
                    WHERE id = $1
                      AND cluster = $3
                      AND scope = $4
                      AND table_address = $5
                      AND family_id IS NULL
                      AND durable = TRUE
                      AND status = $6
                      AND authority = $7
                      AND address_count = $8
                      AND address_hash = $9
                      AND addresses = $10
                      AND legacy_kind IS NULL
                      AND legacy_import_run_id IS NULL
                    "#,
                )
                .bind(table.source.id)
                .bind(&table.observed_address_hash)
                .bind(&table.source.cluster)
                .bind(&table.source.scope)
                .bind(&table.source.table_address)
                .bind(&table.source.status)
                .bind(&table.source.authority)
                .bind(table.source.address_count)
                .bind(&table.source.address_hash)
                .bind(&addresses)
                .execute(&mut *tx)
                .await?;
                if normalized.rows_affected() != 1 {
                    return Err(stale_store_update(
                        "legacy lookup-table historical hash normalization target",
                        table.source.id,
                    ));
                }
            }
        }

        for table in &input.tables {
            let addresses = serde_json::to_value(&table.source.addresses).map_err(|error| {
                OrchestratorError::StoreInvariant(format!(
                    "legacy lookup-table address evidence could not be encoded: {error}"
                ))
            })?;
            let observed_addresses =
                serde_json::to_value(&table.observed_addresses).map_err(|error| {
                    OrchestratorError::StoreInvariant(format!(
                        "observed legacy lookup-table addresses could not be encoded: {error}"
                    ))
                })?;
            if replayed {
                let evidence_matches: bool = sqlx::query_scalar(
                    r#"
                    SELECT EXISTS (
                        SELECT 1
                        FROM loyal_yield.lookup_table_legacy_import_evidence
                        WHERE import_run_id = $1
                          AND route_lookup_table_id = $2
                          AND table_address = $3
                          AND scope = $4
                          AND legacy_kind = $5
                          AND expected_authority = $6
                          AND observed_authority = $7
                          AND observed_owner = $8
                          AND observed_deactivation_slot = $9
                          AND observed_last_extended_slot = $10
                          AND observed_last_extended_start_index = $11
                          AND address_count = $12
                          AND address_hash = $13
                          AND addresses = $14
                          AND verified_slot = $15
                          AND verified_at = $16
                    )
                    "#,
                )
                .bind(import_run_id)
                .bind(table.source.id)
                .bind(&table.source.table_address)
                .bind(&table.source.scope)
                .bind(table.legacy_kind.as_str())
                .bind(&table.source.authority)
                .bind(&table.observed_authority)
                .bind(&table.observed_owner)
                .bind(&table.observed_deactivation_slot)
                .bind(table.observed_last_extended_slot)
                .bind(table.observed_last_extended_start_index)
                .bind(table.observed_address_count)
                .bind(&table.observed_address_hash)
                .bind(&observed_addresses)
                .bind(input.verified_slot)
                .bind(effective_verified_at)
                .fetch_one(&mut *tx)
                .await?;
                if !evidence_matches {
                    return Err(OrchestratorError::StoreInvariant(
                        "legacy lookup-table import fingerprint resolved to different evidence"
                            .to_owned(),
                    ));
                }
                continue;
            }
            sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_legacy_import_evidence
                    (import_run_id, route_lookup_table_id, table_address, scope,
                     legacy_kind, expected_authority, observed_authority,
                     observed_owner, observed_deactivation_slot,
                     observed_last_extended_slot,
                     observed_last_extended_start_index, address_count,
                     address_hash, addresses, verified_slot, verified_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
                        $12, $13, $14, $15, $16)
                ON CONFLICT (import_run_id, route_lookup_table_id) DO NOTHING
                "#,
            )
            .bind(import_run_id)
            .bind(table.source.id)
            .bind(&table.source.table_address)
            .bind(&table.source.scope)
            .bind(table.legacy_kind.as_str())
            .bind(&table.source.authority)
            .bind(&table.observed_authority)
            .bind(&table.observed_owner)
            .bind(&table.observed_deactivation_slot)
            .bind(table.observed_last_extended_slot)
            .bind(table.observed_last_extended_start_index)
            .bind(table.observed_address_count)
            .bind(&table.observed_address_hash)
            .bind(&observed_addresses)
            .bind(input.verified_slot)
            .bind(effective_verified_at)
            .execute(&mut *tx)
            .await?;

            let updated = sqlx::query(
                r#"
                UPDATE loyal_yield.route_lookup_tables
                SET legacy_kind = $2,
                    legacy_import_run_id = $3,
                    last_extended_slot = $4,
                    last_extended_start_index = $5,
                    last_verified_slot = $6,
                    last_verified_at = $7,
                    updated_at = now()
                WHERE id = $1
                  AND family_id IS NULL
                  AND durable = TRUE
                  AND status = $8
                  AND authority = $9
                  AND address_count = $10
                  AND address_hash = $11
                  AND addresses = $12
                  AND (legacy_kind IS NULL OR legacy_kind = $2)
                "#,
            )
            .bind(table.source.id)
            .bind(table.legacy_kind.as_str())
            .bind(import_run_id)
            .bind(table.observed_last_extended_slot)
            .bind(table.observed_last_extended_start_index)
            .bind(input.verified_slot)
            .bind(effective_verified_at)
            .bind(&table.source.status)
            .bind(&table.source.authority)
            .bind(table.source.address_count)
            .bind(&table.observed_address_hash)
            .bind(&addresses)
            .execute(&mut *tx)
            .await?;
            if updated.rows_affected() != 1 {
                return Err(stale_store_update(
                    "legacy lookup-table import target",
                    table.source.id,
                ));
            }
        }

        let evidence_count: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM loyal_yield.lookup_table_legacy_import_evidence WHERE import_run_id = $1",
        )
        .bind(import_run_id)
        .fetch_one(&mut *tx)
        .await?;
        if evidence_count != i64::from(imported_table_count) {
            return Err(OrchestratorError::StoreInvariant(
                "legacy lookup-table import audit evidence is incomplete".to_owned(),
            ));
        }
        tx.commit().await?;
        Ok(LegacyLookupTableFleetImportResult {
            import_run_id,
            cluster: input.cluster,
            legacy_kind,
            verified_slot: input.verified_slot,
            verified_at: effective_verified_at,
            imported_table_count,
            import_fingerprint: input.import_fingerprint,
            replayed,
        })
    }

    /// Explicitly removes an imported legacy table from the durable resolver
    /// set. The row remains as audit history for the existing cleanup scanner.
    pub async fn retire_legacy_route_lookup_table(
        &self,
        input: LegacyLookupTableRetirementRequest,
    ) -> Result<LegacyLookupTableRetirement, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        acquire_lookup_table_rollout_lock(&mut tx, &input.cluster).await?;
        let row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.route_lookup_tables
            WHERE cluster = $1 AND table_address = $2
            FOR UPDATE
            "#,
        )
        .bind(&input.cluster)
        .bind(&input.table_address)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "legacy lookup-table retirement target was not found".to_owned(),
            )
        })?;
        let table_id: i64 = row.try_get("id")?;
        let family_id: Option<i64> = row.try_get("family_id")?;
        let previous_status: String = row.try_get("status")?;
        let authority: String = row.try_get("authority")?;
        let address_hash: String = row.try_get("address_hash")?;
        let address_count: i32 = row.try_get("address_count")?;
        let durable: bool = row.try_get("durable")?;
        if family_id.is_some()
            || !durable
            || !matches!(previous_status.as_str(), "active" | "warming" | "usable")
            || authority != input.expected_authority
            || address_hash != input.expected_address_hash
            || address_count != input.expected_address_count
        {
            return Err(OrchestratorError::StoreInvariant(
                "legacy lookup-table retirement metadata or lifecycle changed".to_owned(),
            ));
        }
        let has_usage_lease: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1 FROM loyal_yield.lookup_table_usage_leases
                WHERE route_lookup_table_id = $1
                  AND released_at IS NULL AND expires_at > now()
            )
            "#,
        )
        .bind(table_id)
        .fetch_one(&mut *tx)
        .await?;
        let global_rollout = sqlx::query(
            r#"
            SELECT rollout_mode, force_legacy, updated_at
            FROM loyal_yield.lookup_table_rollout_controls
            WHERE cluster = $1 AND vault_id IS NULL
            FOR SHARE
            "#,
        )
        .bind(&input.cluster)
        .fetch_optional(&mut *tx)
        .await?;
        let cluster_reusable_only = global_rollout.as_ref().is_some_and(|row| {
            row.try_get::<String, _>("rollout_mode").ok().as_deref() == Some("reusable_only")
                && row.try_get::<bool, _>("force_legacy").ok() == Some(false)
        });
        let reusable_only_cutover_at = global_rollout
            .as_ref()
            .and_then(|row| row.try_get::<DateTime<Utc>, _>("updated_at").ok());
        let active_overrides = sqlx::query(
            r#"
            SELECT control.rollout_mode, control.force_legacy
            FROM loyal_yield.lookup_table_rollout_controls control
            JOIN loyal_yield.managed_vaults vault ON vault.id = control.vault_id
            WHERE control.cluster = $1 AND vault.active = TRUE
            FOR SHARE OF control, vault
            "#,
        )
        .bind(&input.cluster)
        .fetch_all(&mut *tx)
        .await?;
        let unsafe_active_override_count = active_overrides
            .iter()
            .filter(|row| {
                row.try_get::<String, _>("rollout_mode").ok().as_deref() != Some("reusable_only")
                    || row.try_get::<bool, _>("force_legacy").ok() != Some(false)
            })
            .count();
        let readiness_rows = sqlx::query(
            r#"
            SELECT selection_kind, legacy_table_ids, selected_table_ids, updated_at
            FROM loyal_yield.lookup_table_route_readiness_current
            WHERE cluster = $1
              AND ($2 = ANY(legacy_table_ids) OR $2 = ANY(selected_table_ids))
            FOR UPDATE
            "#,
        )
        .bind(&input.cluster)
        .bind(table_id)
        .fetch_all(&mut *tx)
        .await?;
        let has_selected_legacy_reference = readiness_rows.iter().any(|row| {
            let post_cutover = reusable_only_cutover_at.is_none_or(|cutover_at| {
                row.try_get::<DateTime<Utc>, _>("updated_at")
                    .is_ok_and(|updated_at| updated_at >= cutover_at)
            });
            post_cutover
                && (row
                    .try_get::<Option<String>, _>("selection_kind")
                    .ok()
                    .flatten()
                    .as_deref()
                    == Some("legacy")
                    || row
                        .try_get::<Vec<i64>, _>("selected_table_ids")
                        .ok()
                        .is_some_and(|ids| ids.contains(&table_id)))
        });
        if has_usage_lease
            || !cluster_reusable_only
            || unsafe_active_override_count != 0
            || has_selected_legacy_reference
        {
            return Err(OrchestratorError::StoreInvariant(
                "legacy lookup table requires cluster-wide reusable-only rollout with no live lease or selected legacy reference"
                    .to_owned(),
            ));
        }
        sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_route_readiness_current
            SET legacy_table_ids = array_remove(legacy_table_ids, $2),
                selected_table_ids = array_remove(selected_table_ids, $2),
                selected_table_count = cardinality(array_remove(selected_table_ids, $2)),
                selection_kind = CASE
                    WHEN selection_kind = 'legacy' THEN 'blocked'
                    ELSE selection_kind
                END,
                fallback_reason = CASE
                    WHEN selection_kind = 'legacy' THEN 'legacy_table_retired'
                    ELSE fallback_reason
                END,
                updated_at = now()
            WHERE cluster = $1
              AND ($2 = ANY(legacy_table_ids) OR $2 = ANY(selected_table_ids))
            "#,
        )
        .bind(&input.cluster)
        .bind(table_id)
        .execute(&mut *tx)
        .await?;
        let updated = sqlx::query(
            r#"
            UPDATE loyal_yield.route_lookup_tables
            SET status = 'retiring', durable = FALSE, updated_at = now()
            WHERE id = $1 AND family_id IS NULL AND durable = TRUE
              AND status = $2 AND authority = $3
              AND address_hash = $4 AND address_count = $5
            RETURNING id, cluster, table_address, authority, address_hash,
                      address_count, status, durable
            "#,
        )
        .bind(table_id)
        .bind(&previous_status)
        .bind(&input.expected_authority)
        .bind(&input.expected_address_hash)
        .bind(input.expected_address_count)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("legacy lookup table", table_id))?;
        let retirement = LegacyLookupTableRetirement {
            table_id: updated.try_get("id")?,
            cluster: updated.try_get("cluster")?,
            table_address: updated.try_get("table_address")?,
            authority: updated.try_get("authority")?,
            address_hash: updated.try_get("address_hash")?,
            address_count: updated.try_get("address_count")?,
            previous_status,
            status: updated.try_get("status")?,
            durable: updated.try_get("durable")?,
        };
        tx.commit().await?;
        Ok(retirement)
    }

    /// Exercises the production drift reporter and provisioning-request
    /// upsert against production-connected state without loading a signer or
    /// committing demand. All exercised writes share one repeatable-read
    /// transaction, are rolled back to a savepoint, and only the immutable
    /// PASS audit row commits while the exact paused control row remains
    /// locked.
    pub async fn run_lookup_table_precutover_probe(
        &self,
        mut input: LookupTablePrecutoverProbe,
    ) -> Result<LookupTablePrecutoverProbeRecord, OrchestratorError> {
        let finalized_addresses =
            validate_finalized_shared_table_observation(&input.finalized_observation)?;
        let drift_target = input
            .finalized_observation
            .shared_tables
            .iter()
            .find(|table| table.table_id == input.drift_report.route_lookup_table_id)
            .cloned();
        if !is_sha256_hex(&input.probe_token)
            || input.provisioner_control_epoch < 0
            || input.provisioning_request.vault_id.as_i64() <= 0
            || !is_sha256_hex(&input.provisioning_request.requirements_fingerprint)
            || input.drift_report.observed_slot != input.finalized_observation.observed_slot
            || !input.drift_report.observed_table_present
            || !input.drift_report.observed_active
            || !input.drift_report.observed_warm
            || input.drift_report.observed_authority.is_none()
            || input.drift_report.observed_last_extended_slot.is_none()
            || input.drift_report.reason
                != format!("precutover-probe-synthetic-drift:{}", input.probe_token)
            || input.drift_report.reported_by != "route-lookup-table-provisioner:precutover-probe"
            || input.provisioning_request.cluster != input.drift_report.cluster
            || input.provisioning_request.shared_manifest_id.is_none()
            || input.provisioning_request.vault_manifest_id.is_some()
            || !input.provisioning_request.shared_addresses.is_empty()
            || input.provisioning_request.vault_addresses.len() != 1
        {
            return Err(OrchestratorError::StoreInvariant(
                "pre-cutover probe input is malformed or not signerless synthetic demand"
                    .to_owned(),
            ));
        }
        let Some(drift_target) = drift_target else {
            return Err(OrchestratorError::StoreInvariant(
                "pre-cutover probe drift target is absent from the finalized shared bundle"
                    .to_owned(),
            ));
        };
        if input.drift_report.expected_mutation_epoch != drift_target.mutation_epoch
            || input.drift_report.expected_table_address != drift_target.table_address
            || input.drift_report.expected_authority != drift_target.authority
            || input.drift_report.observed_authority.as_deref()
                != Some(drift_target.authority.as_str())
            || input.drift_report.observed_last_extended_slot
                != Some(drift_target.last_extended_slot)
            || drift_target.ordered_addresses.len()
                != input.drift_report.observed_addresses.len() + 1
            || drift_target.ordered_addresses[..input.drift_report.observed_addresses.len()]
                != input.drift_report.observed_addresses
        {
            return Err(OrchestratorError::StoreInvariant(
                "pre-cutover probe synthetic drift must remove exactly the final address of its fenced shared shard".to_owned(),
            ));
        }
        let observed_hash = validate_shared_market_physical_drift_report(&input.drift_report)?;
        validate_lookup_table_provisioning_request(&mut input.provisioning_request)?;
        let finalized_address_hash = drift_target.ordered_address_hash.clone();
        let finalized_address_count = drift_target.address_count;
        let finalized_bundle_address_count =
            i32::try_from(finalized_addresses.len()).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "pre-cutover probe finalized bundle address count exceeds PostgreSQL INTEGER"
                        .to_owned(),
                )
            })?;
        let shared_manifest_id = input
            .provisioning_request
            .shared_manifest_id
            .expect("validated above");
        let finalized_last_extended_slot = input
            .drift_report
            .observed_last_extended_slot
            .expect("validated signerless finalized observation above");
        if finalized_last_extended_slot >= input.finalized_observation.observed_slot {
            return Err(OrchestratorError::StoreInvariant(
                "pre-cutover probe finalized shared table is not warm".to_owned(),
            ));
        }

        let mut tx = self.pool().begin().await?;
        sqlx::query("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ")
            .execute(&mut *tx)
            .await?;
        let control_row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster = $1 FOR UPDATE",
        )
        .bind(&input.drift_report.cluster)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "pre-cutover probe requires a durable cluster pause".to_owned(),
            )
        })?;
        let control = lookup_table_provisioner_control_from_row(&control_row)?;
        if !control.paused || control.control_epoch != input.provisioner_control_epoch {
            return Err(OrchestratorError::StoreInvariant(
                "pre-cutover probe durable pause epoch changed after finalized RPC verification"
                    .to_owned(),
            ));
        }
        let active_broadcast_permit_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*)::BIGINT
            FROM loyal_yield.lookup_table_provisioner_broadcast_permits
            WHERE cluster = $1 AND resolved_at IS NULL
            "#,
        )
        .bind(&input.drift_report.cluster)
        .fetch_one(&mut *tx)
        .await?;
        let in_flight_mutation_count = lookup_table_in_flight_mutation_count_in_connection(
            &mut tx,
            &input.drift_report.cluster,
        )
        .await?;
        if active_broadcast_permit_count != 0 || in_flight_mutation_count != 0 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "pre-cutover probe requires a drained durable pause; active permits={active_broadcast_permit_count}, in-flight mutations={in_flight_mutation_count}"
            )));
        }
        let catalog_before = load_shared_market_catalog_head_in_connection(
            &mut tx,
            &input.drift_report.cluster,
            SharedMarketCatalogHeadLock::Update,
        )
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "pre-cutover probe requires an active shared-market catalog head".to_owned(),
            )
        })?;
        if catalog_before.catalog_revision_id != input.drift_report.catalog_revision_id
            || catalog_before.family_id != input.drift_report.family_id
            || catalog_before.manifest_id != shared_manifest_id
            || catalog_before.readiness_state != SharedMarketCatalogReadiness::Active
            || catalog_before.active_generation != catalog_before.target_generation
            || input
                .provisioning_request
                .desired_shared_hash
                .as_deref()
                .is_some_and(|hash| hash != catalog_before.desired_set_hash)
        {
            return Err(OrchestratorError::StoreInvariant(
                "pre-cutover probe lost its exact active shared catalog fence".to_owned(),
            ));
        }
        let catalog_addresses = catalog_before
            .addresses
            .iter()
            .map(|address| address.address.clone())
            .collect::<Vec<_>>();
        let current_preflight = load_reusable_only_cutover_preflight_in_connection(
            &mut tx,
            &input.drift_report.cluster,
        )
        .await?;
        let locked_finalized_addresses = validate_finalized_shared_tables_against_preflight(
            &current_preflight,
            &input.finalized_observation,
        )?;
        if current_preflight.catalog_revision_id != catalog_before.catalog_revision_id
            || current_preflight.manifest_id != shared_manifest_id
            || catalog_addresses != finalized_addresses
            || locked_finalized_addresses != finalized_addresses
        {
            return Err(OrchestratorError::StoreInvariant(
                "pre-cutover probe finalized shared bundle does not match exact DB catalog/table order"
                    .to_owned(),
            ));
        }
        let vault_active: bool = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM loyal_yield.managed_vaults WHERE id = $1 AND active)",
        )
        .bind(input.provisioning_request.vault_id.as_i64())
        .fetch_one(&mut *tx)
        .await?;
        if !vault_active {
            return Err(OrchestratorError::StoreInvariant(
                "pre-cutover probe vault is missing or inactive".to_owned(),
            ));
        }
        let existing_probe_request_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*)::BIGINT
            FROM loyal_yield.lookup_table_provisioning_requests
            WHERE cluster = $1 AND vault_id = $2 AND requirements_fingerprint = $3
            "#,
        )
        .bind(&input.provisioning_request.cluster)
        .bind(input.provisioning_request.vault_id.as_i64())
        .bind(&input.provisioning_request.requirements_fingerprint)
        .fetch_one(&mut *tx)
        .await?;
        if existing_probe_request_count != 0 {
            return Err(OrchestratorError::StoreInvariant(
                "pre-cutover probe requirements fingerprint is not fresh".to_owned(),
            ));
        }

        sqlx::query("SAVEPOINT lookup_table_precutover_exercise")
            .execute(&mut *tx)
            .await?;
        let before = lookup_table_probe_counts(&mut tx).await?;
        let drift =
            report_shared_market_physical_drift_in_tx(&mut tx, &input.drift_report, &observed_hash)
                .await?;
        let after_drift = lookup_table_probe_counts(&mut tx).await?;
        let first_request =
            upsert_lookup_table_provisioning_request_in_tx(&mut tx, &input.provisioning_request)
                .await?;
        let second_request =
            upsert_lookup_table_provisioning_request_in_tx(&mut tx, &input.provisioning_request)
                .await?;
        let after_requests = lookup_table_probe_counts(&mut tx).await?;
        let drift_signal_count = after_requests.drifts - before.drifts;
        let drift_provisioning_request_count = after_drift.requests - before.requests;
        let distinct_request_count = after_requests.requests - before.requests;
        let decision_count = after_requests.decisions - before.decisions;
        let binding_count = after_requests.bindings - before.bindings;
        let operation_count = after_requests.operations - before.operations;
        let in_tx_passed = first_request.id == second_request.id
            && drift_signal_count == 1
            && drift_provisioning_request_count == 0
            && distinct_request_count == 1
            && decision_count == 0
            && binding_count == 0
            && operation_count == 0;
        if !in_tx_passed {
            return Err(OrchestratorError::StoreInvariant(format!(
                "pre-cutover probe transaction failed invariants: drift={drift_signal_count}, drift_requests={drift_provisioning_request_count}, requests={distinct_request_count}, decisions={decision_count}, bindings={binding_count}, operations={operation_count}, duplicate_same_id={}",
                first_request.id == second_request.id,
            )));
        }

        sqlx::query("ROLLBACK TO SAVEPOINT lookup_table_precutover_exercise")
            .execute(&mut *tx)
            .await?;
        sqlx::query("RELEASE SAVEPOINT lookup_table_precutover_exercise")
            .execute(&mut *tx)
            .await?;

        let rollback_residue_count: i64 = sqlx::query_scalar(
            r#"
            SELECT
                (SELECT count(*) FROM loyal_yield.lookup_table_shared_market_physical_drifts
                 WHERE evidence_hash = $1)
              + (SELECT count(*) FROM loyal_yield.lookup_table_provisioning_requests
                 WHERE cluster = $2 AND vault_id = $3 AND requirements_fingerprint = $4)
            "#,
        )
        .bind(&drift.evidence_hash)
        .bind(&input.provisioning_request.cluster)
        .bind(input.provisioning_request.vault_id.as_i64())
        .bind(&input.provisioning_request.requirements_fingerprint)
        .fetch_one(&mut *tx)
        .await?;
        let catalog_after = load_shared_market_catalog_head_in_connection(
            &mut tx,
            &input.drift_report.cluster,
            SharedMarketCatalogHeadLock::Update,
        )
        .await?;
        let catalog_head_restored = catalog_after.as_ref() == Some(&catalog_before);
        if rollback_residue_count != 0 || !catalog_head_restored {
            return Err(OrchestratorError::StoreInvariant(format!(
                "pre-cutover probe rollback verification failed: residue={rollback_residue_count}, catalog_head_restored={catalog_head_restored}"
            )));
        }

        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_precutover_probe_runs
                (probe_token, cluster, vault_id, catalog_revision_id,
                 shared_manifest_id, route_lookup_table_id,
                 shared_table_address, shared_authority, shared_mutation_epoch,
                 provisioner_control_epoch, requirements_fingerprint,
                 finalized_slot, finalized_last_extended_slot,
                 finalized_address_hash, finalized_address_count,
                 shared_table_bundle_hash, shared_table_count,
                 finalized_bundle_address_count,
                 finalized_shared_exact, synthetic_drift_evidence_hash,
                 drift_signal_count, drift_provisioning_request_count,
                 duplicate_request_attempt_count, distinct_request_count,
                 decision_count, binding_count, operation_count,
                 rollback_residue_count, catalog_head_restored,
                 signer_loaded, transactions_sent, result)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
                    $11, $12, $13, $14, $15, $16, $17, $18, TRUE,
                    $19, $20, $21, 2, $22, $23, $24, $25, $26, $27,
                    FALSE, FALSE, 'pass')
            RETURNING *
            "#,
        )
        .bind(&input.probe_token)
        .bind(&input.drift_report.cluster)
        .bind(input.provisioning_request.vault_id.as_i64())
        .bind(input.drift_report.catalog_revision_id)
        .bind(shared_manifest_id)
        .bind(input.drift_report.route_lookup_table_id)
        .bind(&input.drift_report.expected_table_address)
        .bind(&input.drift_report.expected_authority)
        .bind(input.drift_report.expected_mutation_epoch)
        .bind(input.provisioner_control_epoch)
        .bind(&input.provisioning_request.requirements_fingerprint)
        .bind(input.finalized_observation.observed_slot)
        .bind(finalized_last_extended_slot)
        .bind(&finalized_address_hash)
        .bind(finalized_address_count)
        .bind(&input.finalized_observation.shared_table_bundle_hash)
        .bind(
            i32::try_from(input.finalized_observation.shared_tables.len()).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "probe shared table count exceeds INTEGER".to_owned(),
                )
            })?,
        )
        .bind(finalized_bundle_address_count)
        .bind(&drift.evidence_hash)
        .bind(i32::try_from(drift_signal_count).map_err(|_| {
            OrchestratorError::StoreInvariant("probe drift count exceeds INTEGER".to_owned())
        })?)
        .bind(
            i32::try_from(drift_provisioning_request_count).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "probe drift request count exceeds INTEGER".to_owned(),
                )
            })?,
        )
        .bind(i32::try_from(distinct_request_count).map_err(|_| {
            OrchestratorError::StoreInvariant("probe request count exceeds INTEGER".to_owned())
        })?)
        .bind(i32::try_from(decision_count).map_err(|_| {
            OrchestratorError::StoreInvariant("probe decision count exceeds INTEGER".to_owned())
        })?)
        .bind(i32::try_from(binding_count).map_err(|_| {
            OrchestratorError::StoreInvariant("probe binding count exceeds INTEGER".to_owned())
        })?)
        .bind(i32::try_from(operation_count).map_err(|_| {
            OrchestratorError::StoreInvariant("probe operation count exceeds INTEGER".to_owned())
        })?)
        .bind(i32::try_from(rollback_residue_count).map_err(|_| {
            OrchestratorError::StoreInvariant("probe residue count exceeds INTEGER".to_owned())
        })?)
        .bind(catalog_head_restored)
        .fetch_one(&mut *tx)
        .await?;
        let probe_run_id: i64 = row.try_get("id")?;
        let mut child_insert = QueryBuilder::<Postgres>::new(
            "INSERT INTO loyal_yield.lookup_table_precutover_probe_shared_tables (probe_run_id, shard_ordinal, route_lookup_table_id, shared_table_address, shared_authority, shared_mutation_epoch, finalized_slot, finalized_last_extended_slot, finalized_address_hash, finalized_address_count) ",
        );
        child_insert.push_values(
            &input.finalized_observation.shared_tables,
            |mut row, table| {
                row.push_bind(probe_run_id)
                    .push_bind(table.shard_ordinal)
                    .push_bind(table.table_id)
                    .push_bind(&table.table_address)
                    .push_bind(&table.authority)
                    .push_bind(table.mutation_epoch)
                    .push_bind(input.finalized_observation.observed_slot)
                    .push_bind(table.last_extended_slot)
                    .push_bind(&table.ordered_address_hash)
                    .push_bind(table.address_count);
            },
        );
        child_insert.build().execute(&mut *tx).await?;
        let audit = lookup_table_precutover_probe_from_row_in_connection(&mut tx, &row).await?;
        tx.commit().await?;
        Ok(audit)
    }

    pub async fn upsert_lookup_table_provisioning_request(
        &self,
        mut input: LookupTableProvisioningRequestUpsert,
    ) -> Result<LookupTableProvisioningRequestRecord, OrchestratorError> {
        validate_lookup_table_provisioning_request(&mut input)?;
        let mut tx = self.pool().begin().await?;
        let request = upsert_lookup_table_provisioning_request_in_tx(&mut tx, &input).await?;
        tx.commit().await?;
        Ok(request)
    }

    pub async fn lease_next_lookup_table_provisioning_request(
        &self,
        cluster: &str,
        owner: &str,
        lease_expires_at: DateTime<Utc>,
    ) -> Result<Option<LookupTableProvisioningRequestRecord>, OrchestratorError> {
        let row = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT request.id
                FROM loyal_yield.lookup_table_provisioning_requests request
                LEFT JOIN LATERAL (
                    SELECT
                        COALESCE(sum(opportunity.annual_yield_gain_usd_micros), 0)::NUMERIC
                            AS aggregate_annual_yield,
                        COALESCE(sum(opportunity.economic_priority), 0)::NUMERIC
                            AS aggregate_priority,
                        count(*)::BIGINT AS consumer_count
                    FROM loyal_yield.lookup_table_provisioning_request_consumers consumer
                    JOIN loyal_yield.rebalance_opportunities opportunity
                      ON opportunity.id = consumer.opportunity_id
                    WHERE consumer.provisioning_request_id = request.id
                      AND opportunity.opportunity_state = 'waiting_alt'
                      AND opportunity.expires_at > now()
                ) live_priority ON TRUE
                WHERE request.cluster = $1
                  AND request.request_status IN ('requested', 'queued', 'failed', 'planning')
                  AND (
                      (request.request_status = 'failed'
                       AND request.next_attempt_at IS NOT NULL
                       AND request.next_attempt_at <= now())
                      OR
                      (request.request_status <> 'failed'
                       AND (request.next_attempt_at IS NULL OR request.next_attempt_at <= now()))
                  )
                  AND (
                      request.request_status <> 'planning'
                      OR request.lease_expires_at <= now()
                  )
                -- Rank live aggregate yield unlocked per still-required
                -- address. Stored priority is observability only and cannot
                -- go stale enough to waste the physical ALT mutation lane.
                ORDER BY
                    live_priority.aggregate_annual_yield
                        / GREATEST(
                            1,
                            request.desired_shared_address_count
                                + request.desired_vault_address_count
                        ) DESC,
                    live_priority.aggregate_priority DESC,
                    live_priority.consumer_count DESC,
                    request.updated_at,
                    request.requested_at,
                    request.id
                FOR UPDATE OF request SKIP LOCKED LIMIT 1
            )
            UPDATE loyal_yield.lookup_table_provisioning_requests request
            SET request_status = 'planning', lease_owner = $2, lease_expires_at = $3,
                fencing_token = request.fencing_token + 1,
                attempt_count = request.attempt_count + 1,
                updated_at = now()
            FROM candidate WHERE request.id = candidate.id
            RETURNING request.*
            "#,
        )
        .bind(cluster)
        .bind(owner)
        .bind(lease_expires_at)
        .fetch_optional(self.pool())
        .await?;
        row.as_ref()
            .map(lookup_table_provisioning_request_from_row)
            .transpose()
    }

    pub async fn advance_lookup_table_provisioning_request(
        &self,
        request_id: i64,
        lease: &LookupTableOperationLease,
        next_status: LookupTableProvisioningRequestStatus,
        next_attempt_at: Option<DateTime<Utc>>,
        error_code: Option<&str>,
        error_detail: Option<&str>,
    ) -> Result<LookupTableProvisioningRequestRecord, OrchestratorError> {
        if !LookupTableProvisioningRequestStatus::Planning.can_transition_to(next_status) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "invalid provisioning request transition planning -> {next_status}"
            )));
        }
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_provisioning_requests
            SET request_status = $4, next_attempt_at = $5,
                error_code = $6, error_detail = $7,
                satisfied_at = CASE WHEN $4 = 'satisfied' THEN COALESCE(satisfied_at, now()) ELSE satisfied_at END,
                lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
            WHERE id = $1 AND request_status = 'planning'
              AND lease_owner = $2 AND fencing_token = $3
              AND lease_expires_at > now()
            RETURNING *
            "#,
        )
        .bind(request_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(next_status.as_str())
        .bind(next_attempt_at)
        .bind(error_code)
        .bind(error_detail)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "provisioning request {request_id} lease is stale, expired, or fenced"
            ))
        })?;
        lookup_table_provisioning_request_from_row(&row)
    }

    pub async fn lookup_table_cleanup_protection(
        &self,
        cluster: &str,
        table_address: &str,
    ) -> Result<Option<LookupTableCleanupProtection>, OrchestratorError> {
        self.lookup_table_cleanup_protection_excluding(cluster, table_address, None)
            .await
    }

    /// Returns exact imported legacy evidence plus a fresh, row-locked
    /// zero-reference/nonselectable decision. Imported legacy ALTs never enter
    /// reusable families and must use this API instead of the v2 cleanup path.
    pub async fn legacy_lookup_table_cleanup_protection(
        &self,
        cluster: &str,
        table_address: &str,
    ) -> Result<Option<LegacyLookupTableCleanupProtection>, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        let row = sqlx::query(
            r#"
            SELECT route_table.id, route_table.cluster, route_table.table_address,
                   route_table.legacy_import_run_id, route_table.legacy_kind,
                   route_table.status, route_table.durable, route_table.family_id,
                   route_table.authority, route_table.address_count,
                   route_table.address_hash, route_table.addresses,
                   route_table.last_verified_slot,
                   evidence.import_run_id AS evidence_import_run_id,
                   evidence.legacy_kind AS evidence_legacy_kind,
                   evidence.expected_authority AS evidence_authority,
                   evidence.address_count AS evidence_address_count,
                   evidence.address_hash AS evidence_address_hash,
                   evidence.addresses AS evidence_addresses,
                   EXISTS (
                       SELECT 1 FROM loyal_yield.lookup_table_usage_leases usage
                       WHERE usage.route_lookup_table_id = route_table.id
                         AND usage.released_at IS NULL AND usage.expires_at > now()
                   ) AS has_usage_lease,
                   EXISTS (
                       SELECT 1 FROM loyal_yield.lookup_table_route_readiness_current readiness
                       WHERE readiness.cluster = route_table.cluster
                         AND (route_table.id = ANY(readiness.legacy_table_ids)
                              OR route_table.id = ANY(readiness.selected_table_ids))
                   ) AS has_readiness_reference,
                   EXISTS (
                       SELECT 1 FROM loyal_yield.lookup_table_operations operation
                       WHERE operation.route_lookup_table_id = route_table.id
                         AND operation.operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
                   ) AS has_pending_operation,
                   COALESCE((
                       SELECT control.rollout_mode = 'reusable_only' AND NOT control.force_legacy
                       FROM loyal_yield.lookup_table_rollout_controls control
                       WHERE control.cluster = route_table.cluster AND control.vault_id IS NULL
                   ), FALSE) AS cluster_reusable_only,
                   EXISTS (
                       SELECT 1 FROM loyal_yield.lookup_table_rollout_controls control
                       JOIN loyal_yield.managed_vaults vault ON vault.id = control.vault_id
                       WHERE control.cluster = route_table.cluster AND vault.active = TRUE
                         AND (control.rollout_mode <> 'reusable_only' OR control.force_legacy)
                   ) AS has_unsafe_override
            FROM loyal_yield.route_lookup_tables route_table
            LEFT JOIN loyal_yield.lookup_table_legacy_import_evidence evidence
              ON evidence.import_run_id = route_table.legacy_import_run_id
             AND evidence.route_lookup_table_id = route_table.id
            WHERE route_table.cluster = $1 AND route_table.table_address = $2
            FOR UPDATE OF route_table
            "#,
        )
        .bind(cluster)
        .bind(table_address)
        .fetch_optional(&mut *tx)
        .await?;
        let Some(row) = row else {
            tx.commit().await?;
            return Ok(None);
        };
        let table_id: i64 = row.try_get("id")?;
        let import_run_id: Option<i64> = row.try_get("legacy_import_run_id")?;
        let legacy_kind_raw: Option<String> = row.try_get("legacy_kind")?;
        let legacy_kind = legacy_kind_raw
            .clone()
            .map(|value| parse_store_enum("legacy lookup-table kind", value))
            .transpose()?;
        let status: String = row.try_get("status")?;
        let durable: bool = row.try_get("durable")?;
        let family_id: Option<i64> = row.try_get("family_id")?;
        let authority: String = row.try_get("authority")?;
        let address_count: i32 = row.try_get("address_count")?;
        let address_hash: String = row.try_get("address_hash")?;
        let ordered_addresses = serde_json::from_value::<Vec<String>>(row.try_get("addresses")?)
            .map_err(|error| {
                OrchestratorError::StoreInvariant(format!(
                    "legacy lookup-table registry addresses are invalid: {error}"
                ))
            })?;
        let evidence_exact = import_run_id.is_some()
            && row.try_get::<Option<i64>, _>("evidence_import_run_id")? == import_run_id
            && row.try_get::<Option<String>, _>("evidence_legacy_kind")? == legacy_kind_raw
            && row
                .try_get::<Option<String>, _>("evidence_authority")?
                .as_deref()
                == Some(authority.as_str())
            && row.try_get::<Option<i32>, _>("evidence_address_count")? == Some(address_count)
            && row
                .try_get::<Option<String>, _>("evidence_address_hash")?
                .as_deref()
                == Some(address_hash.as_str())
            && row.try_get::<Option<Value>, _>("evidence_addresses")?
                == Some(serde_json::to_value(&ordered_addresses).map_err(|error| {
                    OrchestratorError::StoreInvariant(format!(
                        "legacy lookup-table evidence cannot be compared: {error}"
                    ))
                })?);
        let has_usage_lease: bool = row.try_get("has_usage_lease")?;
        let has_readiness_reference: bool = row.try_get("has_readiness_reference")?;
        let has_pending_operation: bool = row.try_get("has_pending_operation")?;
        let cluster_reusable_only: bool = row.try_get("cluster_reusable_only")?;
        let has_unsafe_override: bool = row.try_get("has_unsafe_override")?;
        let nonselectable = !durable && !matches!(status.as_str(), "active" | "warming" | "usable");
        let zero_reference = !has_usage_lease && !has_readiness_reference && !has_pending_operation;
        let mut reasons = Vec::new();
        if family_id.is_some() {
            reasons.push("belongs_to_reusable_family".to_owned());
        }
        if !evidence_exact {
            reasons.push("missing_or_mismatched_import_evidence".to_owned());
        }
        if !nonselectable {
            reasons.push("legacy_table_still_selectable".to_owned());
        }
        if !cluster_reusable_only || has_unsafe_override {
            reasons.push("rollout_not_fully_reusable_only".to_owned());
        }
        if has_usage_lease {
            reasons.push("unexpired_usage_lease".to_owned());
        }
        if has_readiness_reference {
            reasons.push("readiness_reference".to_owned());
        }
        if has_pending_operation {
            reasons.push("pending_operation".to_owned());
        }
        let can_deactivate = reasons.is_empty() && status == "retiring";
        let can_close = reasons.is_empty() && status == "deactivated";
        if !can_deactivate && !can_close && reasons.is_empty() {
            reasons.push(
                if status == "closed" {
                    "already_closed"
                } else {
                    "legacy_lifecycle_not_actionable"
                }
                .to_owned(),
            );
        }
        let token_values = vec![
            cluster.to_owned(),
            table_id.to_string(),
            table_address.to_owned(),
            import_run_id.unwrap_or_default().to_string(),
            legacy_kind_raw.unwrap_or_default(),
            status.clone(),
            durable.to_string(),
            authority.clone(),
            address_count.to_string(),
            address_hash.clone(),
            zero_reference.to_string(),
            nonselectable.to_string(),
            cluster_reusable_only.to_string(),
            has_unsafe_override.to_string(),
        ];
        let authorization_token = hash_length_prefixed_values(
            token_values
                .iter()
                .map(String::as_str)
                .chain(ordered_addresses.iter().map(String::as_str)),
        );
        let protection = LegacyLookupTableCleanupProtection {
            table_id,
            cluster: cluster.to_owned(),
            table_address: table_address.to_owned(),
            import_run_id,
            legacy_kind,
            status,
            durable,
            family_id,
            expected_authority: authority,
            address_count,
            address_hash,
            ordered_addresses,
            last_verified_slot: row.try_get("last_verified_slot")?,
            zero_reference,
            nonselectable,
            can_deactivate,
            can_close,
            authorization_token,
            protection_reasons: reasons,
        };
        tx.commit().await?;
        Ok(Some(protection))
    }

    pub async fn prepare_legacy_lookup_table_cleanup_attempt(
        &self,
        input: LegacyLookupTableCleanupAttemptPrepare,
    ) -> Result<LegacyLookupTableCleanupAttemptRecord, OrchestratorError> {
        if input.cluster.trim().is_empty()
            || input.table_address.trim().is_empty()
            || input.expected_authority.trim().is_empty()
            || !is_sha256_hex(&input.expected_authorization_token)
            || !is_sha256_hex(&input.expected_address_hash)
            || !(0..=i32::from(LOOKUP_TABLE_HARD_CAPACITY)).contains(&input.expected_address_count)
        {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup attempt requires complete immutable identity fences".to_owned(),
            ));
        }
        match input.operation_kind {
            LookupTableOperationKind::Deactivate => {
                if input.close_recipient.is_some() || input.expected_reclaimed_lamports.is_some() {
                    return Err(OrchestratorError::StoreInvariant(
                        "legacy deactivation attempt cannot contain refund fields".to_owned(),
                    ));
                }
            }
            LookupTableOperationKind::Close => {
                if input.close_recipient.as_deref() != Some(input.expected_authority.as_str())
                    || input
                        .expected_reclaimed_lamports
                        .is_none_or(|value| value <= 0)
                {
                    return Err(OrchestratorError::StoreInvariant(
                        "legacy close attempt must refund positive rent to its expected authority"
                            .to_owned(),
                    ));
                }
            }
            _ => {
                return Err(OrchestratorError::StoreInvariant(
                    "legacy cleanup attempts only support deactivate or close".to_owned(),
                ));
            }
        }
        let protection = self
            .legacy_lookup_table_cleanup_protection(&input.cluster, &input.table_address)
            .await?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "legacy cleanup attempt target was not found".to_owned(),
                )
            })?;
        let authorized = match input.operation_kind {
            LookupTableOperationKind::Deactivate => protection.can_deactivate,
            LookupTableOperationKind::Close => protection.can_close,
            _ => false,
        };
        if !authorized
            || protection.authorization_token != input.expected_authorization_token
            || protection.expected_authority != input.expected_authority
            || protection.address_count != input.expected_address_count
            || protection.address_hash != input.expected_address_hash
        {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup attempt authorization is stale or not actionable".to_owned(),
            ));
        }

        let mut tx = self.pool().begin().await?;
        sqlx::query(
            "SELECT pg_advisory_xact_lock(hashtextextended('legacy-alt-cleanup:' || $1 || ':' || $2, 0))",
        )
        .bind(&input.cluster)
        .bind(&input.table_address)
        .execute(&mut *tx)
        .await?;
        let route_row = sqlx::query(
            r#"
            SELECT id, status, durable, family_id, legacy_import_run_id,
                   authority, address_count, address_hash
            FROM loyal_yield.route_lookup_tables
            WHERE cluster = $1 AND table_address = $2
            FOR UPDATE
            "#,
        )
        .bind(&input.cluster)
        .bind(&input.table_address)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "legacy cleanup attempt target disappeared".to_owned(),
            )
        })?;
        let table_id: i64 = route_row.try_get("id")?;
        let expected_status = match input.operation_kind {
            LookupTableOperationKind::Deactivate => "retiring",
            LookupTableOperationKind::Close => "deactivated",
            _ => unreachable!("validated above"),
        };
        if route_row.try_get::<String, _>("status")? != expected_status
            || route_row.try_get::<bool, _>("durable")?
            || route_row.try_get::<Option<i64>, _>("family_id")?.is_some()
            || route_row
                .try_get::<Option<i64>, _>("legacy_import_run_id")?
                .is_none()
            || route_row.try_get::<String, _>("authority")? != input.expected_authority
            || route_row.try_get::<i32, _>("address_count")? != input.expected_address_count
            || route_row.try_get::<String, _>("address_hash")? != input.expected_address_hash
        {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup attempt lost its exact retired registry fence".to_owned(),
            ));
        }
        if let Some(row) = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_legacy_cleanup_attempts
            WHERE route_lookup_table_id = $1 AND operation_kind = $2
              AND attempt_state IN ('prepared', 'signed', 'submitted', 'needs_reconcile')
            ORDER BY attempt_number DESC
            LIMIT 1
            FOR UPDATE
            "#,
        )
        .bind(table_id)
        .bind(input.operation_kind.as_str())
        .fetch_optional(&mut *tx)
        .await?
        {
            let existing = legacy_lookup_table_cleanup_attempt_from_row(&row)?;
            if existing.authorization_token != input.expected_authorization_token
                || existing.expected_authority != input.expected_authority
                || existing.expected_address_count != input.expected_address_count
                || existing.expected_address_hash != input.expected_address_hash
                || existing.close_recipient != input.close_recipient
                || existing.expected_reclaimed_lamports != input.expected_reclaimed_lamports
            {
                return Err(OrchestratorError::StoreInvariant(
                    "active legacy cleanup attempt conflicts with the requested immutable fences"
                        .to_owned(),
                ));
            }
            tx.commit().await?;
            return Ok(existing);
        }
        let attempt_number: i32 = sqlx::query_scalar(
            r#"
            SELECT COALESCE(max(attempt_number), 0) + 1
            FROM loyal_yield.lookup_table_legacy_cleanup_attempts
            WHERE route_lookup_table_id = $1 AND operation_kind = $2
            "#,
        )
        .bind(table_id)
        .bind(input.operation_kind.as_str())
        .fetch_one(&mut *tx)
        .await?;
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_legacy_cleanup_attempts
                (route_lookup_table_id, cluster, table_address, operation_kind,
                 attempt_number, authorization_token, expected_authority,
                 expected_address_count, expected_address_hash, close_recipient,
                 expected_reclaimed_lamports, attempt_state)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'prepared')
            RETURNING *
            "#,
        )
        .bind(table_id)
        .bind(&input.cluster)
        .bind(&input.table_address)
        .bind(input.operation_kind.as_str())
        .bind(attempt_number)
        .bind(&input.expected_authorization_token)
        .bind(&input.expected_authority)
        .bind(input.expected_address_count)
        .bind(&input.expected_address_hash)
        .bind(&input.close_recipient)
        .bind(input.expected_reclaimed_lamports)
        .fetch_one(&mut *tx)
        .await?;
        let attempt = legacy_lookup_table_cleanup_attempt_from_row(&row)?;
        tx.commit().await?;
        Ok(attempt)
    }

    /// Reserves the simulated worst-case fee and rent for a familyless legacy
    /// cleanup attempt before any signer is invoked. Legacy and reusable-v2
    /// reservations serialize on the same cluster advisory lock and are summed
    /// by the same rolling-window accounting query.
    pub async fn reserve_legacy_lookup_table_cleanup_budget(
        &self,
        cluster: &str,
        legacy_cleanup_attempt_id: i64,
        policy: LookupTableClusterBudgetPolicy,
        estimated_fee_lamports: i64,
        estimated_rent_lamports: i64,
    ) -> Result<LegacyLookupTableCleanupBudgetReservation, OrchestratorError> {
        if cluster.trim().is_empty()
            || legacy_cleanup_attempt_id <= 0
            || policy.max_lamports <= 0
            || !(1..=31_536_000).contains(&policy.rolling_window_seconds)
            || estimated_fee_lamports < 0
            || estimated_rent_lamports < 0
        {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup cluster budget requires a positive attempt, limit/window, and nonnegative simulated accounting"
                    .to_owned(),
            ));
        }
        let requested_lamports = estimated_fee_lamports
            .checked_add(estimated_rent_lamports)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "legacy cleanup cluster budget request overflowed lamports".to_owned(),
                )
            })?;
        if requested_lamports <= 0 {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup cluster budget requires a positive simulated reservation"
                    .to_owned(),
            ));
        }

        let mut tx = self.pool().begin().await?;
        sqlx::query(
            "SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-budget:' || $1, 0))",
        )
        .bind(cluster)
        .execute(&mut *tx)
        .await?;
        let now: DateTime<Utc> = sqlx::query_scalar("SELECT clock_timestamp()")
            .fetch_one(&mut *tx)
            .await?;
        let attempt_row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_legacy_cleanup_attempts WHERE id = $1 FOR UPDATE",
        )
        .bind(legacy_cleanup_attempt_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            stale_store_update(
                "legacy cleanup budget attempt",
                legacy_cleanup_attempt_id,
            )
        })?;
        let attempt = legacy_lookup_table_cleanup_attempt_from_row(&attempt_row)?;
        if attempt.cluster != cluster
            || attempt.attempt_state != LegacyLookupTableCleanupAttemptState::Prepared
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "legacy cleanup attempt {legacy_cleanup_attempt_id} is not a prepared attempt in cluster {cluster}"
            )));
        }

        let existing = sqlx::query(
            r#"
            SELECT id, estimated_fee_lamports, estimated_rent_lamports,
                   reserved_lamports, reserved_until
            FROM loyal_yield.lookup_table_legacy_cleanup_budget_reservations
            WHERE legacy_cleanup_attempt_id = $1
            "#,
        )
        .bind(legacy_cleanup_attempt_id)
        .fetch_optional(&mut *tx)
        .await?;
        if let Some(existing) = existing {
            let reserved_until: DateTime<Utc> = existing.try_get("reserved_until")?;
            if existing.try_get::<i64, _>("estimated_fee_lamports")? != estimated_fee_lamports
                || existing.try_get::<i64, _>("estimated_rent_lamports")? != estimated_rent_lamports
                || existing.try_get::<i64, _>("reserved_lamports")? != requested_lamports
            {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "legacy cleanup attempt {legacy_cleanup_attempt_id} budget fence was replayed with different accounting"
                )));
            }
            if reserved_until <= now {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "legacy cleanup attempt {legacy_cleanup_attempt_id} budget reservation expired before signing"
                )));
            }
            let usage = load_cluster_budget_usage_in_connection(
                &mut tx,
                cluster,
                "legacy_cleanup",
                legacy_cleanup_attempt_id,
                now,
            )
            .await?;
            let result = LegacyLookupTableCleanupBudgetReservation {
                approved: true,
                replayed: true,
                reservation_id: Some(existing.try_get("id")?),
                cluster: cluster.to_owned(),
                legacy_cleanup_attempt_id,
                estimated_fee_lamports,
                estimated_rent_lamports,
                requested_lamports,
                spent_lamports: usage.spent_lamports,
                reserved_lamports: usage.reserved_lamports,
                charged_lamports: usage.charged_lamports,
                remaining_lamports: policy.max_lamports.saturating_sub(usage.charged_lamports),
                window_ends_at: reserved_until,
            };
            tx.commit().await?;
            return Ok(result);
        }

        let usage = load_cluster_budget_usage_in_connection(
            &mut tx,
            cluster,
            "legacy_cleanup",
            legacy_cleanup_attempt_id,
            now,
        )
        .await?;
        let current_subject_charge = usage
            .subject_reserved_lamports
            .max(usage.subject_actual_lamports);
        let prospective_subject_charge = usage
            .subject_reserved_lamports
            .checked_add(requested_lamports)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "legacy cleanup cluster budget subject reservation overflowed".to_owned(),
                )
            })?
            .max(usage.subject_actual_lamports);
        let prospective_charge = usage
            .charged_lamports
            .checked_add(prospective_subject_charge - current_subject_charge)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "legacy cleanup cluster budget total overflowed".to_owned(),
                )
            })?;
        let requested_window_end: DateTime<Utc> = sqlx::query_scalar(
            "SELECT $1::timestamptz + ($2::double precision * interval '1 second')",
        )
        .bind(now)
        .bind(policy.rolling_window_seconds)
        .fetch_one(&mut *tx)
        .await?;
        if prospective_charge > policy.max_lamports {
            let result = LegacyLookupTableCleanupBudgetReservation {
                approved: false,
                replayed: false,
                reservation_id: None,
                cluster: cluster.to_owned(),
                legacy_cleanup_attempt_id,
                estimated_fee_lamports,
                estimated_rent_lamports,
                requested_lamports,
                spent_lamports: usage.spent_lamports,
                reserved_lamports: usage.reserved_lamports,
                charged_lamports: usage.charged_lamports,
                remaining_lamports: policy.max_lamports.saturating_sub(usage.charged_lamports),
                window_ends_at: usage.window_ends_at.unwrap_or(requested_window_end),
            };
            tx.commit().await?;
            return Ok(result);
        }

        let reservation_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.lookup_table_legacy_cleanup_budget_reservations
                (legacy_cleanup_attempt_id, cluster, estimated_fee_lamports,
                 estimated_rent_lamports, reserved_lamports, reserved_at,
                 reserved_until)
            VALUES ($1, $2, $3, $4, $5, $6, $7)
            RETURNING id
            "#,
        )
        .bind(legacy_cleanup_attempt_id)
        .bind(cluster)
        .bind(estimated_fee_lamports)
        .bind(estimated_rent_lamports)
        .bind(requested_lamports)
        .bind(now)
        .bind(requested_window_end)
        .fetch_one(&mut *tx)
        .await?;
        let usage = load_cluster_budget_usage_in_connection(
            &mut tx,
            cluster,
            "legacy_cleanup",
            legacy_cleanup_attempt_id,
            now,
        )
        .await?;
        let result = LegacyLookupTableCleanupBudgetReservation {
            approved: true,
            replayed: false,
            reservation_id: Some(reservation_id),
            cluster: cluster.to_owned(),
            legacy_cleanup_attempt_id,
            estimated_fee_lamports,
            estimated_rent_lamports,
            requested_lamports,
            spent_lamports: usage.spent_lamports,
            reserved_lamports: usage.reserved_lamports,
            charged_lamports: usage.charged_lamports,
            remaining_lamports: policy.max_lamports.saturating_sub(usage.charged_lamports),
            window_ends_at: usage.window_ends_at.unwrap_or(requested_window_end),
        };
        tx.commit().await?;
        Ok(result)
    }

    pub async fn persist_signed_legacy_lookup_table_cleanup_attempt(
        &self,
        attempt_id: i64,
        input: SignedLegacyLookupTableCleanupAttempt,
    ) -> Result<LegacyLookupTableCleanupAttemptRecord, OrchestratorError> {
        if attempt_id <= 0
            || input.transaction_signature.trim().is_empty()
            || !is_sha256_hex(&input.message_hash)
            || input.recent_blockhash.trim().is_empty()
            || input.last_valid_block_height < 0
            || input.estimated_fee_lamports < 0
            || input
                .recipient_balance_before
                .is_some_and(|value| value < 0)
        {
            return Err(OrchestratorError::StoreInvariant(
                "signed legacy cleanup attempt metadata is incomplete".to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_legacy_cleanup_attempts
            SET attempt_state = 'signed', transaction_signature = $2,
                message_hash = $3, recent_blockhash = $4,
                last_valid_block_height = $5, estimated_fee_lamports = $6,
                recipient_balance_before = $7, error_code = NULL,
                error_detail = NULL, updated_at = now()
            WHERE id = $1 AND attempt_state = 'prepared'
              AND transaction_signature IS NULL
            RETURNING *
            "#,
        )
        .bind(attempt_id)
        .bind(&input.transaction_signature)
        .bind(&input.message_hash)
        .bind(&input.recent_blockhash)
        .bind(input.last_valid_block_height)
        .bind(input.estimated_fee_lamports)
        .bind(input.recipient_balance_before)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| stale_store_update("legacy cleanup prepared attempt", attempt_id))?;
        legacy_lookup_table_cleanup_attempt_from_row(&row)
    }

    pub async fn mark_legacy_lookup_table_cleanup_attempt_submitted(
        &self,
        attempt_id: i64,
        expected_signature: &str,
    ) -> Result<LegacyLookupTableCleanupAttemptRecord, OrchestratorError> {
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_legacy_cleanup_attempts
            SET attempt_state = 'submitted', submitted_at = COALESCE(submitted_at, now()),
                error_code = NULL, error_detail = NULL, updated_at = now()
            WHERE id = $1 AND attempt_state = 'signed' AND transaction_signature = $2
            RETURNING *
            "#,
        )
        .bind(attempt_id)
        .bind(expected_signature)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| stale_store_update("legacy cleanup signed attempt", attempt_id))?;
        legacy_lookup_table_cleanup_attempt_from_row(&row)
    }

    pub async fn mark_legacy_lookup_table_cleanup_attempt_needs_reconcile(
        &self,
        attempt_id: i64,
        expected_signature: &str,
        error_code: &str,
        error_detail: &str,
    ) -> Result<LegacyLookupTableCleanupAttemptRecord, OrchestratorError> {
        if error_code.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup reconciliation error code is required".to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_legacy_cleanup_attempts
            SET attempt_state = 'needs_reconcile', error_code = $3,
                error_detail = $4, updated_at = now()
            WHERE id = $1 AND attempt_state IN ('signed', 'submitted', 'needs_reconcile')
              AND transaction_signature = $2
            RETURNING *
            "#,
        )
        .bind(attempt_id)
        .bind(expected_signature)
        .bind(error_code)
        .bind(error_detail.chars().take(500).collect::<String>())
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| stale_store_update("legacy cleanup reconciling attempt", attempt_id))?;
        legacy_lookup_table_cleanup_attempt_from_row(&row)
    }

    pub async fn expire_unobserved_legacy_lookup_table_cleanup_attempt(
        &self,
        attempt_id: i64,
        expected_signature: &str,
        observed_block_height: i64,
    ) -> Result<LegacyLookupTableCleanupAttemptRecord, OrchestratorError> {
        if observed_block_height < 0 {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup expiry height must not be negative".to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_legacy_cleanup_attempts
            SET attempt_state = 'expired', error_code = 'signed_transaction_expired_unobserved',
                error_detail = 'persisted signature was absent after blockhash expiry and chain state was unchanged',
                updated_at = now()
            WHERE id = $1
              AND attempt_state IN ('signed', 'submitted', 'needs_reconcile')
              AND transaction_signature = $2
              AND last_valid_block_height < $3
            RETURNING *
            "#,
        )
        .bind(attempt_id)
        .bind(expected_signature)
        .bind(observed_block_height)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| stale_store_update("legacy cleanup expiring attempt", attempt_id))?;
        legacy_lookup_table_cleanup_attempt_from_row(&row)
    }

    pub async fn fail_legacy_lookup_table_cleanup_attempt_permanently(
        &self,
        attempt_id: i64,
        expected_signature: &str,
        error_detail: &str,
    ) -> Result<LegacyLookupTableCleanupAttemptRecord, OrchestratorError> {
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_legacy_cleanup_attempts
            SET attempt_state = 'permanent_failure', error_code = 'finalized_transaction_failed',
                error_detail = $3, updated_at = now()
            WHERE id = $1
              AND attempt_state IN ('signed', 'submitted', 'needs_reconcile')
              AND transaction_signature = $2
            RETURNING *
            "#,
        )
        .bind(attempt_id)
        .bind(expected_signature)
        .bind(error_detail.chars().take(500).collect::<String>())
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| stale_store_update("legacy cleanup failed attempt", attempt_id))?;
        legacy_lookup_table_cleanup_attempt_from_row(&row)
    }

    pub async fn complete_legacy_lookup_table_cleanup_attempt(
        &self,
        attempt_id: i64,
        input: FinalizedLegacyLookupTableCleanupAttempt,
    ) -> Result<LegacyLookupTableCleanupAttemptRecord, OrchestratorError> {
        if attempt_id <= 0
            || input.transaction_signature.trim().is_empty()
            || input.finalized_slot < 0
            || input
                .recipient_balance_before
                .is_some_and(|value| value < 0)
            || input.recipient_balance_after.is_some_and(|value| value < 0)
            || input
                .actual_reclaimed_lamports
                .is_some_and(|value| value <= 0)
        {
            return Err(OrchestratorError::StoreInvariant(
                "finalized legacy cleanup attempt evidence is incomplete".to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        let row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_legacy_cleanup_attempts WHERE id = $1 FOR UPDATE",
        )
        .bind(attempt_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("legacy cleanup attempt", attempt_id))?;
        let attempt = legacy_lookup_table_cleanup_attempt_from_row(&row)?;
        if attempt.attempt_state == LegacyLookupTableCleanupAttemptState::Complete {
            if attempt.transaction_signature.as_deref() == Some(&input.transaction_signature)
                && attempt.finalized_slot == Some(input.finalized_slot)
                && attempt.recipient_balance_before == input.recipient_balance_before
                && attempt.recipient_balance_after == input.recipient_balance_after
                && attempt.actual_reclaimed_lamports == input.actual_reclaimed_lamports
            {
                tx.commit().await?;
                return Ok(attempt);
            }
            return Err(OrchestratorError::StoreInvariant(
                "completed legacy cleanup evidence cannot be changed".to_owned(),
            ));
        }
        if !matches!(
            attempt.attempt_state,
            LegacyLookupTableCleanupAttemptState::Signed
                | LegacyLookupTableCleanupAttemptState::Submitted
                | LegacyLookupTableCleanupAttemptState::NeedsReconcile
        ) || attempt.transaction_signature.as_deref() != Some(&input.transaction_signature)
        {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup finalization does not match a durable signed attempt".to_owned(),
            ));
        }
        let (expected_status, updated) = match attempt.operation_kind {
            LookupTableOperationKind::Deactivate => {
                if input.recipient_balance_before.is_some()
                    || input.recipient_balance_after.is_some()
                    || input.actual_reclaimed_lamports.is_some()
                {
                    return Err(OrchestratorError::StoreInvariant(
                        "legacy deactivation cannot finalize refund evidence".to_owned(),
                    ));
                }
                (
                    "retiring",
                    sqlx::query(
                        r#"
                        UPDATE loyal_yield.route_lookup_tables
                        SET status = 'deactivated', deactivated_slot = $2,
                            deactivate_signature = $3, updated_at = now()
                        WHERE id = $1 AND status = 'retiring' AND durable = FALSE
                          AND family_id IS NULL AND authority = $4
                          AND address_count = $5 AND address_hash = $6
                        "#,
                    )
                    .bind(attempt.route_lookup_table_id)
                    .bind(input.finalized_slot)
                    .bind(&input.transaction_signature)
                    .bind(&attempt.expected_authority)
                    .bind(attempt.expected_address_count)
                    .bind(&attempt.expected_address_hash)
                    .execute(&mut *tx)
                    .await?,
                )
            }
            LookupTableOperationKind::Close => {
                let before = input.recipient_balance_before.ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "legacy close finalization lacks recipient balance before".to_owned(),
                    )
                })?;
                let after = input.recipient_balance_after.ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "legacy close finalization lacks recipient balance after".to_owned(),
                    )
                })?;
                let reclaimed = input.actual_reclaimed_lamports.ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "legacy close finalization lacks reclaimed rent".to_owned(),
                    )
                })?;
                let expected_reclaimed = attempt.expected_reclaimed_lamports.ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "legacy close attempt lacks expected reclaimed rent".to_owned(),
                    )
                })?;
                let fee = attempt.estimated_fee_lamports.ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "legacy close signed attempt lacks estimated fee".to_owned(),
                    )
                })?;
                let expected_after = before
                    .checked_add(expected_reclaimed)
                    .and_then(|balance| balance.checked_sub(fee))
                    .ok_or_else(|| {
                        OrchestratorError::StoreInvariant(
                            "legacy close transaction balance proof overflowed".to_owned(),
                        )
                    })?;
                if reclaimed != expected_reclaimed || after != expected_after {
                    return Err(OrchestratorError::StoreInvariant(
                        "legacy close transaction balances do not prove the exact refund"
                            .to_owned(),
                    ));
                }
                (
                    "deactivated",
                    sqlx::query(
                        r#"
                        UPDATE loyal_yield.route_lookup_tables
                        SET status = 'closed', closed_signature = $2,
                            close_recipient = $3, reclaimed_lamports = $4,
                            updated_at = now()
                        WHERE id = $1 AND status = 'deactivated' AND durable = FALSE
                          AND family_id IS NULL AND authority = $5
                          AND address_count = $6 AND address_hash = $7
                        "#,
                    )
                    .bind(attempt.route_lookup_table_id)
                    .bind(&input.transaction_signature)
                    .bind(&attempt.close_recipient)
                    .bind(reclaimed)
                    .bind(&attempt.expected_authority)
                    .bind(attempt.expected_address_count)
                    .bind(&attempt.expected_address_hash)
                    .execute(&mut *tx)
                    .await?,
                )
            }
            _ => unreachable!("attempt table restricts operation kind"),
        };
        if updated.rows_affected() != 1 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "legacy cleanup {expected_status} registry row changed before recovery finalization"
            )));
        }
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_legacy_cleanup_attempts
            SET attempt_state = 'complete', finalized_slot = $2,
                recipient_balance_before = $3, recipient_balance_after = $4,
                actual_reclaimed_lamports = $5,
                error_code = NULL, error_detail = NULL, updated_at = now()
            WHERE id = $1
              AND attempt_state IN ('signed', 'submitted', 'needs_reconcile')
              AND transaction_signature = $6
            RETURNING *
            "#,
        )
        .bind(attempt_id)
        .bind(input.finalized_slot)
        .bind(input.recipient_balance_before)
        .bind(input.recipient_balance_after)
        .bind(input.actual_reclaimed_lamports)
        .bind(&input.transaction_signature)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("legacy cleanup finalizing attempt", attempt_id))?;
        let attempt = legacy_lookup_table_cleanup_attempt_from_row(&row)?;
        tx.commit().await?;
        Ok(attempt)
    }

    pub async fn pending_legacy_lookup_table_cleanup_attempts(
        &self,
        cluster: &str,
    ) -> Result<Vec<LegacyLookupTableCleanupAttemptRecord>, OrchestratorError> {
        let rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_legacy_cleanup_attempts
            WHERE cluster = $1
              AND attempt_state IN ('prepared', 'signed', 'submitted', 'needs_reconcile')
            ORDER BY created_at, id
            "#,
        )
        .bind(cluster)
        .fetch_all(self.pool())
        .await?;
        rows.iter()
            .map(legacy_lookup_table_cleanup_attempt_from_row)
            .collect()
    }

    pub async fn cumulative_legacy_lookup_table_refunds(
        &self,
        cluster: &str,
    ) -> Result<i64, OrchestratorError> {
        Ok(sqlx::query_scalar(
            r#"
            SELECT COALESCE(sum(actual_reclaimed_lamports), 0)::BIGINT
            FROM loyal_yield.lookup_table_legacy_cleanup_attempts
            WHERE cluster = $1 AND operation_kind = 'close' AND attempt_state = 'complete'
            "#,
        )
        .bind(cluster)
        .fetch_one(self.pool())
        .await?)
    }

    /// Performs the short, immediate pre-sign authorization read. Retirement
    /// triggers durably reject every new reference and rollout reversal, so no
    /// database transaction or advisory lock is held across chain RPC.
    pub async fn begin_legacy_lookup_table_cleanup_authorization(
        &self,
        cluster: &str,
        table_address: &str,
        expected_authorization_token: &str,
        operation_kind: LookupTableOperationKind,
    ) -> Result<LegacyLookupTableCleanupAuthorization, OrchestratorError> {
        let protection = self
            .legacy_lookup_table_cleanup_protection(cluster, table_address)
            .await?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "legacy cleanup authorization target was not found".to_owned(),
                )
            })?;
        let authorized = match operation_kind {
            LookupTableOperationKind::Deactivate => protection.can_deactivate,
            LookupTableOperationKind::Close => protection.can_close,
            _ => false,
        };
        if protection.authorization_token != expected_authorization_token || !authorized {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup authorization token is stale or not actionable".to_owned(),
            ));
        }
        Ok(LegacyLookupTableCleanupAuthorization {
            client: self.clone(),
            protection,
            operation_kind,
        })
    }

    async fn record_verified_legacy_lookup_table_cleanup(
        &self,
        expected: &LegacyLookupTableCleanupProtection,
        input: VerifiedLegacyLookupTableCleanup,
    ) -> Result<(), OrchestratorError> {
        let current = self
            .legacy_lookup_table_cleanup_protection(&input.cluster, &input.table_address)
            .await?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "legacy cleanup finalized target disappeared".to_owned(),
                )
            })?;
        let actionable = match input.operation_kind {
            LookupTableOperationKind::Deactivate => current.can_deactivate,
            LookupTableOperationKind::Close => current.can_close,
            _ => false,
        };
        if current.authorization_token != input.expected_authorization_token
            || current.authorization_token != expected.authorization_token
            || !actionable
        {
            return Err(OrchestratorError::StoreInvariant(
                "legacy cleanup authorization changed before finalized record".to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        let (expected_status, updated) = match input.operation_kind {
            LookupTableOperationKind::Deactivate => (
                "retiring",
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.route_lookup_tables
                    SET status = 'deactivated', deactivated_slot = $2,
                        deactivate_signature = $3, updated_at = now()
                    WHERE id = $1 AND status = 'retiring' AND durable = FALSE
                      AND family_id IS NULL AND legacy_import_run_id = $4
                      AND authority = $5 AND address_count = $6 AND address_hash = $7
                    "#,
                )
                .bind(current.table_id)
                .bind(input.observed_slot)
                .bind(&input.transaction_signature)
                .bind(current.import_run_id)
                .bind(&current.expected_authority)
                .bind(current.address_count)
                .bind(&current.address_hash)
                .execute(&mut *tx)
                .await?,
            ),
            LookupTableOperationKind::Close => (
                "deactivated",
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.route_lookup_tables
                    SET status = 'closed', closed_signature = $2,
                        close_recipient = $3, reclaimed_lamports = $4,
                        updated_at = now()
                    WHERE id = $1 AND status = 'deactivated' AND durable = FALSE
                      AND family_id IS NULL AND legacy_import_run_id = $5
                      AND authority = $6 AND address_count = $7 AND address_hash = $8
                    "#,
                )
                .bind(current.table_id)
                .bind(&input.transaction_signature)
                .bind(&input.close_recipient)
                .bind(input.reclaimed_lamports)
                .bind(current.import_run_id)
                .bind(&current.expected_authority)
                .bind(current.address_count)
                .bind(&current.address_hash)
                .execute(&mut *tx)
                .await?,
            ),
            _ => unreachable!("validated by authorization"),
        };
        if updated.rows_affected() != 1 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "legacy cleanup {expected_status} row changed before finalized record"
            )));
        }
        tx.commit().await?;
        Ok(())
    }

    /// Re-reads cleanup blockers while excluding the operation whose signer is
    /// being authorized. Usage-lease acquisition locks the same physical row
    /// and rejects every nonterminal cleanup operation, closing the read/sign
    /// race without holding a database transaction across RPC work.
    pub async fn lookup_table_cleanup_protection_for_operation(
        &self,
        cluster: &str,
        table_address: &str,
        operation_id: i64,
    ) -> Result<Option<LookupTableCleanupProtection>, OrchestratorError> {
        self.lookup_table_cleanup_protection_excluding(cluster, table_address, Some(operation_id))
            .await
    }

    async fn lookup_table_cleanup_protection_excluding(
        &self,
        cluster: &str,
        table_address: &str,
        excluding_operation_id: Option<i64>,
    ) -> Result<Option<LookupTableCleanupProtection>, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        let locked_table_id = sqlx::query_scalar::<_, i64>(
            r#"
            SELECT id FROM loyal_yield.route_lookup_tables
            WHERE cluster = $1 AND table_address = $2
            FOR UPDATE
            "#,
        )
        .bind(cluster)
        .bind(table_address)
        .fetch_optional(&mut *tx)
        .await?;
        if locked_table_id.is_none() {
            tx.commit().await?;
            return Ok(None);
        }
        let row = sqlx::query(
            r#"
            SELECT route_table.id, route_table.family_id, route_table.cluster,
                   route_table.table_address,
                   route_table.authority, route_table.address_count,
                   route_table.address_hash, route_table.generation,
                   route_table.mutation_epoch,
                   route_table.allocation_kind,
                   route_table.accepting_allocations, route_table.desired_state,
                   route_table.last_verified_slot, route_table.rollback_until,
                   family.active_generation, family.previous_generation,
                   family.rollback_until AS family_rollback_until,
                   EXISTS (
                       SELECT 1 FROM loyal_yield.lookup_table_vault_bindings binding
                       WHERE binding.route_lookup_table_id = route_table.id
                         AND binding.lifecycle_state IN ('preparing', 'warming', 'active', 'standby', 'retiring')
                   ) AS has_live_binding,
                   EXISTS (
                       SELECT 1 FROM loyal_yield.lookup_table_vault_bindings binding
                       WHERE binding.route_lookup_table_id = route_table.id
                         AND binding.rollback_until > now()
                   ) AS has_binding_rollback,
                   EXISTS (
                       SELECT 1 FROM loyal_yield.lookup_table_usage_leases usage
                       WHERE usage.route_lookup_table_id = route_table.id
                         AND usage.released_at IS NULL AND usage.expires_at > now()
                   ) AS has_usage_lease,
                   EXISTS (
                       SELECT 1 FROM loyal_yield.lookup_table_operations operation
                       WHERE operation.route_lookup_table_id = route_table.id
                         AND ($3::BIGINT IS NULL OR operation.id <> $3)
                         AND operation.operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
                   ) AS has_pending_operation
            FROM loyal_yield.route_lookup_tables route_table
            JOIN loyal_yield.lookup_table_families family ON family.id = route_table.family_id
            WHERE route_table.cluster = $1 AND route_table.table_address = $2
            "#,
        )
        .bind(cluster)
        .bind(table_address)
        .bind(excluding_operation_id)
        .fetch_optional(&mut *tx)
        .await?;
        let Some(row) = row else {
            tx.commit().await?;
            return Ok(None);
        };
        let generation: Option<i32> = row.try_get("active_generation")?;
        let physical_generation: Option<i32> = row.try_get("generation")?;
        let previous_generation: Option<i32> = row.try_get("previous_generation")?;
        let desired_state_raw: String = row.try_get("desired_state")?;
        let has_live_binding: bool = row.try_get("has_live_binding")?;
        let safe_retiring_current_vault_shard = generation == physical_generation
            && matches!(
                row.try_get::<Option<String>, _>("allocation_kind")?
                    .as_deref(),
                Some("vault_shard" | "dedicated_vault")
            )
            && !row.try_get::<bool, _>("accepting_allocations")?
            && !has_live_binding
            && matches!(desired_state_raw.as_str(), "retiring" | "deactivated");
        let mut reasons = Vec::new();
        if generation == physical_generation && !safe_retiring_current_vault_shard {
            reasons.push("active_family_generation".to_owned());
        }
        if previous_generation == physical_generation {
            reasons.push("previous_family_generation".to_owned());
        }
        if excluding_operation_id.is_none() && row.try_get::<bool, _>("accepting_allocations")? {
            reasons.push("accepting_allocations".to_owned());
        }
        if has_live_binding {
            reasons.push("live_binding".to_owned());
        }
        if row.try_get::<bool, _>("has_usage_lease")? {
            reasons.push("unexpired_usage_lease".to_owned());
        }
        if row.try_get::<bool, _>("has_pending_operation")? {
            reasons.push("pending_operation".to_owned());
        }
        let now = Utc::now();
        if row
            .try_get::<Option<DateTime<Utc>>, _>("rollback_until")?
            .is_some_and(|until| until > now)
            || row
                .try_get::<Option<DateTime<Utc>>, _>("family_rollback_until")?
                .is_some_and(|until| until > now)
            || row.try_get::<bool, _>("has_binding_rollback")?
        {
            reasons.push("rollback_window".to_owned());
        }
        if desired_state_raw == "failed"
            || row
                .try_get::<Option<i64>, _>("last_verified_slot")?
                .is_none()
        {
            reasons.push("verification_or_drift_unresolved".to_owned());
        }
        let desired_state = parse_store_enum("lookup-table lifecycle", desired_state_raw)?;
        let can_deactivate = reasons.is_empty()
            && matches!(
                desired_state,
                LookupTableLifecycle::Active
                    | LookupTableLifecycle::Standby
                    | LookupTableLifecycle::Retiring
            );
        let can_close = reasons.is_empty() && desired_state == LookupTableLifecycle::Deactivated;
        if desired_state == LookupTableLifecycle::Closed {
            reasons.push("already_closed".to_owned());
        } else if !can_deactivate && !can_close && reasons.is_empty() {
            reasons.push("physical_lifecycle_not_actionable".to_owned());
        }
        let protection = LookupTableCleanupProtection {
            table_id: row.try_get("id")?,
            family_id: row.try_get::<Option<i64>, _>("family_id")?.ok_or_else(|| {
                OrchestratorError::StoreInvariant("registered v2 ALT lacks family".to_owned())
            })?,
            cluster: row.try_get("cluster")?,
            table_address: row.try_get("table_address")?,
            expected_authority: row.try_get("authority")?,
            address_count: row.try_get("address_count")?,
            address_hash: row.try_get("address_hash")?,
            mutation_epoch: row
                .try_get::<Option<i64>, _>("mutation_epoch")?
                .ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "registered v2 ALT lacks mutation epoch".to_owned(),
                    )
                })?,
            desired_state,
            accepting_allocations: row.try_get("accepting_allocations")?,
            can_deactivate,
            can_close,
            protection_reasons: reasons,
        };
        tx.commit().await?;
        Ok(Some(protection))
    }
}

fn legacy_lookup_table_import_source_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LegacyLookupTableImportSource, OrchestratorError> {
    let legacy_kind = row
        .try_get::<Option<String>, _>("legacy_kind")?
        .map(|value| parse_store_enum("legacy lookup-table kind", value))
        .transpose()?;
    let addresses =
        serde_json::from_value::<Vec<String>>(row.try_get("addresses")?).map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "legacy lookup-table address list is invalid: {error}"
            ))
        })?;
    Ok(LegacyLookupTableImportSource {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        scope: row.try_get("scope")?,
        table_address: row.try_get("table_address")?,
        authority: row.try_get("authority")?,
        status: row.try_get("status")?,
        durable: row.try_get("durable")?,
        address_count: row.try_get("address_count")?,
        address_hash: row.try_get("address_hash")?,
        addresses,
        legacy_kind,
        legacy_import_run_id: row.try_get("legacy_import_run_id")?,
        last_extended_slot: row.try_get("last_extended_slot")?,
        last_extended_start_index: row.try_get("last_extended_start_index")?,
        last_verified_slot: row.try_get("last_verified_slot")?,
        last_verified_at: row.try_get("last_verified_at")?,
    })
}

pub fn legacy_lookup_table_import_fingerprint(
    cluster: &str,
    rpc_genesis_hash: &str,
    verified_slot: i64,
    tables: &[VerifiedLegacyLookupTableImport],
) -> String {
    let mut parts = vec![
        cluster.to_owned(),
        rpc_genesis_hash.to_owned(),
        verified_slot.to_string(),
    ];
    for table in tables {
        parts.extend([
            table.source.id.to_string(),
            table.source.scope.clone(),
            table.source.table_address.clone(),
            table.source.authority.clone(),
            table.legacy_kind.as_str().to_owned(),
            table.observed_owner.clone(),
            table.observed_authority.clone(),
            table.observed_deactivation_slot.clone(),
            table.observed_last_extended_slot.to_string(),
            table.observed_last_extended_start_index.to_string(),
            table.observed_address_count.to_string(),
            table.observed_address_hash.clone(),
        ]);
        parts.extend(table.observed_addresses.iter().cloned());
    }
    ordered_address_hash(&parts)
}

fn validate_legacy_lookup_table_fleet_import(
    input: &LegacyLookupTableFleetImportRequest,
) -> Result<(), OrchestratorError> {
    if input.cluster.trim().is_empty()
        || input.rpc_genesis_hash.trim().is_empty()
        || input.verified_slot < 0
        || input.reason.trim().is_empty()
        || input.updated_by.trim().is_empty()
        || input.import_fingerprint.len() != 64
        || !input
            .import_fingerprint
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit())
        || input.tables.is_empty()
        || input.expected_table_count <= 0
        || usize::try_from(input.expected_table_count).ok() != Some(input.tables.len())
    {
        return Err(OrchestratorError::StoreInvariant(
            "legacy lookup-table fleet import metadata is incomplete".to_owned(),
        ));
    }
    let expected_kind = input.tables[0].legacy_kind;
    let mut table_ids = BTreeSet::new();
    let mut table_addresses = BTreeSet::new();
    let mut previous_id = None;
    for table in &input.tables {
        let source = &table.source;
        let address_count = usize::try_from(source.address_count).ok();
        let canonical_address_hash = ordered_address_hash(&source.addresses);
        let historical_preimport_hash = source.legacy_kind.is_none()
            && source.legacy_import_run_id.is_none()
            && historical_legacy_lookup_table_address_hash(&source.addresses)
                == source.address_hash;
        let persisted_address_hash_is_valid =
            source.address_hash == canonical_address_hash || historical_preimport_hash;
        let existing_kind_matches = source
            .legacy_kind
            .is_none_or(|legacy_kind| legacy_kind == table.legacy_kind);
        if table.legacy_kind != expected_kind
            || source.cluster != input.cluster
            || !source.durable
            || !matches!(source.status.as_str(), "active" | "warming" | "usable")
            || address_count != Some(source.addresses.len())
            || source.addresses.len() > usize::from(LOOKUP_TABLE_HARD_CAPACITY)
            || !is_sha256_hex(&source.address_hash)
            || !persisted_address_hash_is_valid
            || Pubkey::from_str(&source.table_address).is_err()
            || Pubkey::from_str(&source.authority).is_err()
            || table.observed_authority != source.authority
            || table.observed_owner != address_lookup_table_program::id().to_string()
            || table.observed_deactivation_slot != u64::MAX.to_string()
            || table.observed_last_extended_slot < 0
            || !(0..=255).contains(&table.observed_last_extended_start_index)
            || table.observed_last_extended_start_index > table.observed_address_count
            || table.observed_address_count != source.address_count
            || table.observed_address_hash != canonical_address_hash
            || ordered_address_hash(&table.observed_addresses) != table.observed_address_hash
            || table.observed_addresses != source.addresses
            || input.verified_slot <= table.observed_last_extended_slot
            || source
                .last_verified_slot
                .is_some_and(|last_verified_slot| input.verified_slot < last_verified_slot)
            || !existing_kind_matches
            || !table_ids.insert(source.id)
            || !table_addresses.insert(&source.table_address)
            || previous_id.is_some_and(|id| source.id <= id)
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "legacy lookup-table import evidence is invalid for table {}",
                source.id
            )));
        }
        previous_id = Some(source.id);
    }
    Ok(())
}

fn parse_store_enum<T>(kind: &'static str, value: String) -> Result<T, OrchestratorError>
where
    T: FromStr<Err = LookupTableDomainError>,
{
    value.parse().map_err(|error| {
        OrchestratorError::StoreInvariant(format!(
            "invalid persisted {kind} value {value:?}: {error}"
        ))
    })
}

fn domain_store_error(error: LookupTableDomainError) -> OrchestratorError {
    OrchestratorError::StoreInvariant(error.to_string())
}

fn retryable_lookup_table_database_conflict(error: &OrchestratorError) -> Option<&'static str> {
    let OrchestratorError::Sqlx(sqlx::Error::Database(database)) = error else {
        return None;
    };
    match database.code().as_deref() {
        Some("40P01") => Some("40P01"),
        Some("40001") => Some("40001"),
        Some("55P03") => Some("55P03"),
        _ => None,
    }
}

fn log_lookup_table_database_retry(
    operation: &'static str,
    sqlstate: &'static str,
    attempt: usize,
) {
    eprintln!(
        "{}",
        serde_json::json!({
            "event": "lookup_table_database_concurrency_retry",
            "operation": operation,
            "sqlstate": sqlstate,
            "attempt": attempt,
            "nextAttempt": attempt + 1,
            "maxAttempts": LOOKUP_TABLE_DB_CONCURRENCY_MAX_ATTEMPTS,
        })
    );
}

async fn sleep_for_lookup_table_database_retry(attempt: usize) {
    let delay_millis = LOOKUP_TABLE_DB_CONCURRENCY_RETRY_BASE_MILLIS
        .saturating_mul(u64::try_from(attempt).unwrap_or(u64::MAX));
    tokio::time::sleep(std::time::Duration::from_millis(delay_millis)).await;
}

fn stale_store_update(kind: &str, id: i64) -> OrchestratorError {
    OrchestratorError::StoreInvariant(format!("stale or missing {kind} {id}"))
}

fn stale_fenced_operation(id: i64) -> OrchestratorError {
    OrchestratorError::StoreInvariant(format!(
        "lookup-table operation {id} lease is stale, expired, or fenced"
    ))
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum SharedMarketCatalogHeadLock {
    None,
    Update,
}

impl SharedMarketCatalogHeadLock {
    const fn clause(self) -> &'static str {
        match self {
            Self::None => "",
            Self::Update => " FOR UPDATE OF head",
        }
    }
}

async fn load_shared_market_catalog_head_in_connection(
    tx: &mut sqlx::PgConnection,
    cluster: &str,
    lock: SharedMarketCatalogHeadLock,
) -> Result<Option<SharedMarketCatalogHeadRecord>, OrchestratorError> {
    load_shared_market_catalog_head_from_connection(tx, cluster, lock).await
}

async fn load_shared_market_catalog_head_from_connection(
    connection: &mut sqlx::PgConnection,
    cluster: &str,
    lock: SharedMarketCatalogHeadLock,
) -> Result<Option<SharedMarketCatalogHeadRecord>, OrchestratorError> {
    if lock == SharedMarketCatalogHeadLock::Update {
        // Shared catalog mutations use one global lock order everywhere:
        // family -> head -> physical table -> operation/permit. Acquiring the
        // family separately avoids relying on a join executor's row-lock order
        // and prevents publish/plan/permit deadlocks.
        sqlx::query(
            r#"
            SELECT id
            FROM loyal_yield.lookup_table_families
            WHERE cluster = $1 AND kind = 'shared_market'
              AND desired_state = 'active'
            ORDER BY id
            FOR UPDATE
            "#,
        )
        .bind(cluster)
        .fetch_all(&mut *connection)
        .await?;
    }
    let sql = format!(
        r#"
        SELECT family.id AS family_id, family.cluster,
               family.active_generation,
               head.catalog_revision_id, head.target_generation,
               head.readiness_state, head.activated_at,
               head.created_at AS head_created_at,
               head.updated_at AS head_updated_at,
               revision.catalog_revision, revision.manifest_id,
               revision.catalog_version, revision.desired_set_hash,
               revision.enabled_mints_hash, revision.reserve_set_hash,
               revision.address_count, revision.source_slot,
               revision.source_observed_at, revision.source_metadata,
               revision.reason, revision.updated_by
        FROM loyal_yield.lookup_table_families family
        JOIN loyal_yield.lookup_table_shared_market_catalog_heads head
          ON head.family_id = family.id
        JOIN loyal_yield.lookup_table_shared_market_catalog_revisions revision
          ON revision.id = head.catalog_revision_id
         AND revision.family_id = family.id
        JOIN loyal_yield.lookup_table_manifests manifest
          ON manifest.id = revision.manifest_id
         AND manifest.family_id = family.id
         AND manifest.subject_kind = 'shared_market'
         AND manifest.sealed_at IS NOT NULL
        WHERE family.cluster = $1 AND family.kind = 'shared_market'
          AND family.desired_state = 'active'
        {}
        "#,
        lock.clause()
    );
    let rows = sqlx::query(&sql)
        .bind(cluster)
        .fetch_all(&mut *connection)
        .await?;
    if rows.len() > 1 {
        return Err(OrchestratorError::StoreInvariant(format!(
            "cluster {cluster:?} has multiple active shared-market catalog heads"
        )));
    }
    let Some(row) = rows.first() else {
        return Ok(None);
    };
    let manifest_id: i64 = row.try_get("manifest_id")?;
    let address_rows = sqlx::query(
        r#"
        SELECT address, ordinal, semantic_class, account_role, is_writable
        FROM loyal_yield.lookup_table_manifest_addresses
        WHERE manifest_id = $1 ORDER BY ordinal
        "#,
    )
    .bind(manifest_id)
    .fetch_all(&mut *connection)
    .await?;
    let addresses = address_rows
        .iter()
        .map(lookup_table_manifest_address_from_row)
        .collect::<Result<Vec<_>, _>>()?;
    let address_count: i32 = row.try_get("address_count")?;
    if usize::try_from(address_count).ok() != Some(addresses.len()) {
        return Err(OrchestratorError::StoreInvariant(format!(
            "shared-market catalog manifest {manifest_id} address count drifted"
        )));
    }
    Ok(Some(SharedMarketCatalogHeadRecord {
        family_id: row.try_get("family_id")?,
        catalog_revision_id: row.try_get("catalog_revision_id")?,
        catalog_revision: row.try_get("catalog_revision")?,
        manifest_id,
        cluster: row.try_get("cluster")?,
        catalog_version: row.try_get("catalog_version")?,
        desired_set_hash: row.try_get("desired_set_hash")?,
        enabled_mints_hash: row.try_get("enabled_mints_hash")?,
        reserve_set_hash: row.try_get("reserve_set_hash")?,
        address_count,
        source_slot: row.try_get("source_slot")?,
        source_observed_at: row.try_get("source_observed_at")?,
        source_metadata: row.try_get("source_metadata")?,
        reason: row.try_get("reason")?,
        updated_by: row.try_get("updated_by")?,
        active_generation: row.try_get("active_generation")?,
        target_generation: row.try_get("target_generation")?,
        readiness_state: row
            .try_get::<String, _>("readiness_state")?
            .parse()
            .map_err(domain_store_error)?,
        activated_at: row.try_get("activated_at")?,
        created_at: row.try_get("head_created_at")?,
        updated_at: row.try_get("head_updated_at")?,
        addresses,
    }))
}

fn shared_market_route_catalog_drift(
    route_addresses: &[LookupTableManifestAddressRecord],
    catalog_addresses: &[LookupTableManifestAddressRecord],
) -> (Vec<String>, Vec<String>) {
    let catalog = catalog_addresses
        .iter()
        .map(|row| (row.address.as_str(), row))
        .collect::<BTreeMap<_, _>>();
    let mut missing = Vec::new();
    let mut semantic_mismatches = Vec::new();
    for route in route_addresses {
        let Some(catalog_row) = catalog.get(route.address.as_str()) else {
            missing.push(route.address.clone());
            continue;
        };
        let route_roles = route
            .account_role
            .split(',')
            .filter(|role| !role.is_empty())
            .collect::<BTreeSet<_>>();
        let catalog_roles = catalog_row
            .account_role
            .split(',')
            .filter(|role| !role.is_empty())
            .collect::<BTreeSet<_>>();
        if !route_roles.is_subset(&catalog_roles) || (route.is_writable && !catalog_row.is_writable)
        {
            semantic_mismatches.push(route.address.clone());
        }
    }
    (missing, semantic_mismatches)
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct SharedMarketCatalogGenerationEvidence {
    ready: bool,
    missing_addresses: Vec<String>,
    extra_addresses: Vec<String>,
}

#[derive(Debug, Default)]
struct SharedMarketGenerationPlanningState {
    physical: Vec<ReusableLookupTableRecord>,
    confirmed: BTreeMap<i32, Vec<String>>,
    pending: BTreeMap<i32, Vec<String>>,
    nonterminal_operation_count: BTreeMap<i32, i64>,
    all_table_count: i64,
}

impl SharedMarketGenerationPlanningState {
    fn empty() -> Self {
        Self::default()
    }
}

async fn cancel_superseded_unsigned_shared_market_operations_in_connection(
    tx: &mut sqlx::PgConnection,
    family_id: i64,
    current_manifest_id: i64,
) -> Result<u64, OrchestratorError> {
    let cancelled = sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_operations
        SET operation_state = 'cancelled', next_attempt_at = NULL,
            error_code = 'superseded_shared_market_catalog',
            error_detail = 'unsigned shared-market operation belongs to a superseded catalog manifest',
            lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
        WHERE family_id = $1
          AND manifest_id IS DISTINCT FROM $2
          AND operation_kind IN ('create', 'extend', 'rollover')
          AND (
              operation_state IN ('queued', 'retry_wait')
              OR (
                  operation_state = 'leased'
                  AND lease_expires_at <= now()
              )
          )
          AND transaction_signature IS NULL
          AND message_hash IS NULL
          AND recent_blockhash IS NULL
          AND last_valid_block_height IS NULL
        "#,
    )
    .bind(family_id)
    .bind(current_manifest_id)
    .execute(&mut *tx)
    .await?;
    Ok(cancelled.rows_affected())
}

async fn load_shared_market_generation_planning_state(
    tx: &mut sqlx::PgConnection,
    family_id: i64,
    generation: i32,
) -> Result<SharedMarketGenerationPlanningState, OrchestratorError> {
    let all_table_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)::BIGINT
        FROM loyal_yield.route_lookup_tables
        WHERE family_id = $1 AND generation = $2
          AND allocation_kind = 'shared_market'
        "#,
    )
    .bind(family_id)
    .bind(generation)
    .fetch_one(&mut *tx)
    .await?;
    let rows = sqlx::query(
        r#"
        SELECT * FROM loyal_yield.route_lookup_tables
        WHERE family_id = $1 AND generation = $2
          AND allocation_kind = 'shared_market'
          AND desired_state NOT IN ('deactivated', 'closed', 'failed')
        ORDER BY shard_ordinal FOR UPDATE
        "#,
    )
    .bind(family_id)
    .bind(generation)
    .fetch_all(&mut *tx)
    .await?;
    let physical = rows
        .iter()
        .map(reusable_lookup_table_from_row)
        .collect::<Result<Vec<_>, _>>()?;
    let mut confirmed = BTreeMap::new();
    let mut pending = BTreeMap::new();
    let mut nonterminal_operation_count = BTreeMap::new();
    for table in &physical {
        confirmed.insert(
            table.shard_ordinal,
            sqlx::query_scalar::<_, String>(
                r#"
                SELECT address
                FROM loyal_yield.lookup_table_addresses
                WHERE route_lookup_table_id = $1
                ORDER BY ordinal
                "#,
            )
            .bind(table.id)
            .fetch_all(&mut *tx)
            .await?,
        );
        pending.insert(
            table.shard_ordinal,
            sqlx::query_scalar::<_, String>(
                r#"
                SELECT address.address
                FROM loyal_yield.lookup_table_operations operation
                JOIN loyal_yield.lookup_table_operation_addresses address
                  ON address.operation_id = operation.id
                WHERE operation.route_lookup_table_id = $1
                  AND operation.operation_state NOT IN (
                      'complete', 'permanent_failure', 'cancelled'
                  )
                ORDER BY operation.created_at, operation.id, address.ordinal
                "#,
            )
            .bind(table.id)
            .fetch_all(&mut *tx)
            .await?,
        );
        nonterminal_operation_count.insert(
            table.shard_ordinal,
            sqlx::query_scalar::<_, i64>(
                r#"
                SELECT count(*)::BIGINT
                FROM loyal_yield.lookup_table_operations
                WHERE route_lookup_table_id = $1
                  AND operation_state NOT IN (
                      'complete', 'permanent_failure', 'cancelled'
                  )
                "#,
            )
            .bind(table.id)
            .fetch_one(&mut *tx)
            .await?,
        );
    }
    Ok(SharedMarketGenerationPlanningState {
        physical,
        confirmed,
        pending,
        nonterminal_operation_count,
        all_table_count,
    })
}

fn shared_market_generation_is_order_compatible(
    state: &SharedMarketGenerationPlanningState,
    shard_plan: &[SharedMarketShardPlan],
) -> bool {
    state.physical.len() <= shard_plan.len()
        && state.physical.iter().all(|table| {
            let Some(desired) = shard_plan
                .iter()
                .find(|shard| shard.shard_ordinal == table.shard_ordinal)
                .map(|shard| shard.addresses.as_slice())
            else {
                return false;
            };
            let confirmed = state
                .confirmed
                .get(&table.shard_ordinal)
                .map(Vec::as_slice)
                .unwrap_or_default();
            let pending = state
                .pending
                .get(&table.shard_ordinal)
                .map(Vec::as_slice)
                .unwrap_or_default();
            ordered_confirmed_and_pending_match(confirmed, pending, desired)
        })
}

async fn shared_market_operation_head_fence_detail(
    tx: &mut sqlx::PgConnection,
    catalog: &SharedMarketCatalogHeadRecord,
    operation: &LookupTableOperationRecord,
    table: &ReusableLookupTableRecord,
) -> Result<Option<String>, OrchestratorError> {
    if !matches!(
        operation.operation_kind,
        LookupTableOperationKind::Create
            | LookupTableOperationKind::Extend
            | LookupTableOperationKind::Rollover
    ) {
        return Ok(None);
    }
    if operation.family_id != catalog.family_id
        || operation.manifest_id != Some(catalog.manifest_id)
        || operation.route_lookup_table_id != Some(table.id)
        || table.family_id != catalog.family_id
        || table.cluster != catalog.cluster
        || table.allocation_kind != LookupTableAllocationKind::SharedMarket
        || catalog.target_generation != Some(table.generation)
        || operation.mutation_epoch != table.mutation_epoch
        || (matches!(
            operation.operation_kind,
            LookupTableOperationKind::Create | LookupTableOperationKind::Rollover
        ) && (operation.target_generation != Some(table.generation)
            || operation.target_shard_ordinal != Some(table.shard_ordinal)))
    {
        return Ok(Some(
            "shared-market operation lost its current catalog, generation, table, or mutation-epoch identity"
                .to_owned(),
        ));
    }
    let confirmed = sqlx::query_scalar::<_, String>(
        r#"
        SELECT address
        FROM loyal_yield.lookup_table_addresses
        WHERE route_lookup_table_id = $1
        ORDER BY ordinal
        "#,
    )
    .bind(table.id)
    .fetch_all(&mut *tx)
    .await?;
    let operation_addresses = sqlx::query_scalar::<_, String>(
        r#"
        SELECT address
        FROM loyal_yield.lookup_table_operation_addresses
        WHERE operation_id = $1
        ORDER BY ordinal
        "#,
    )
    .bind(operation.id)
    .fetch_all(&mut *tx)
    .await?;
    let catalog_addresses = catalog
        .addresses
        .iter()
        .map(|address| address.address.clone())
        .collect::<Vec<_>>();
    let shard_capacity = u16::try_from(table.allocation_high_water).map_err(|_| {
        OrchestratorError::StoreInvariant(format!(
            "shared-market table {} has an invalid allocation high-water",
            table.id
        ))
    })?;
    let desired_plan = append_pack_shared_market_shards(&catalog_addresses, shard_capacity)
        .map_err(domain_store_error)?;
    let Some(desired) = desired_plan
        .iter()
        .find(|shard| shard.shard_ordinal == table.shard_ordinal)
        .map(|shard| shard.addresses.as_slice())
    else {
        return Ok(Some(
            "shared-market operation targets a shard absent from the current catalog plan"
                .to_owned(),
        ));
    };
    let older_nonterminal_exists: bool = sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM loyal_yield.lookup_table_operations predecessor
            WHERE predecessor.route_lookup_table_id = $1
              AND predecessor.id <> $2
              AND predecessor.operation_state NOT IN (
                  'complete', 'permanent_failure', 'cancelled'
              )
              AND (predecessor.created_at, predecessor.id) < ($3, $2)
        )
        "#,
    )
    .bind(table.id)
    .bind(operation.id)
    .bind(operation.created_at)
    .fetch_one(&mut *tx)
    .await?;
    let table_count_matches = usize::try_from(table.address_count).ok() == Some(confirmed.len());
    if older_nonterminal_exists
        || operation_addresses.is_empty()
        || !table_count_matches
        || !ordered_prefix_matches(&confirmed, desired)
        || !ordered_confirmed_and_pending_match(&confirmed, &operation_addresses, desired)
    {
        return Ok(Some(
            "shared-market operation is not the unique next ordered suffix of the current physical prefix"
                .to_owned(),
        ));
    }
    Ok(None)
}

async fn shared_market_catalog_generation_evidence_in_connection(
    tx: &mut sqlx::PgConnection,
    family_id: i64,
    generation: Option<i32>,
    catalog_addresses: &[LookupTableManifestAddressRecord],
) -> Result<SharedMarketCatalogGenerationEvidence, OrchestratorError> {
    let desired_ordered = catalog_addresses
        .iter()
        .map(|row| row.address.clone())
        .collect::<Vec<_>>();
    let desired = desired_ordered.iter().cloned().collect::<BTreeSet<_>>();
    let Some(generation) = generation else {
        return Ok(SharedMarketCatalogGenerationEvidence {
            ready: false,
            missing_addresses: desired.into_iter().collect(),
            extra_addresses: Vec::new(),
        });
    };
    let allocation_high_water: i32 = sqlx::query_scalar(
        "SELECT allocation_high_water FROM loyal_yield.lookup_table_families WHERE id = $1",
    )
    .bind(family_id)
    .fetch_one(&mut *tx)
    .await?;
    let shard_capacity = u16::try_from(allocation_high_water).map_err(|_| {
        OrchestratorError::StoreInvariant(format!(
            "shared-market family {family_id} has an invalid allocation high-water"
        ))
    })?;
    let shard_plan = append_pack_shared_market_shards(&desired_ordered, shard_capacity)
        .map_err(domain_store_error)?;
    let table_rows = sqlx::query(
        r#"
        SELECT id, desired_state, address_count, usable_address_count,
               last_verified_slot, shard_ordinal, allocation_high_water
        FROM loyal_yield.route_lookup_tables
        WHERE family_id = $1 AND generation = $2
          AND allocation_kind = 'shared_market'
          AND desired_state NOT IN ('deactivated', 'closed', 'failed')
        ORDER BY shard_ordinal, id
        "#,
    )
    .bind(family_id)
    .bind(generation)
    .fetch_all(&mut *tx)
    .await?;
    let table_ids = table_rows
        .iter()
        .map(|row| row.try_get::<i64, _>("id"))
        .collect::<Result<Vec<_>, _>>()?;
    let membership_rows = if table_ids.is_empty() {
        Vec::new()
    } else {
        sqlx::query(
            r#"
            SELECT address.route_lookup_table_id, address.ordinal, address.address
            FROM loyal_yield.lookup_table_addresses address
            JOIN loyal_yield.route_lookup_tables route_table
              ON route_table.id = address.route_lookup_table_id
            WHERE address.route_lookup_table_id = ANY($1)
            ORDER BY route_table.shard_ordinal, address.ordinal
            "#,
        )
        .bind(&table_ids)
        .fetch_all(&mut *tx)
        .await?
    };
    let mut memberships = BTreeMap::<i64, Vec<(i32, String)>>::new();
    for row in membership_rows {
        memberships
            .entry(row.try_get("route_lookup_table_id")?)
            .or_default()
            .push((row.try_get("ordinal")?, row.try_get("address")?));
    }
    let physical_ordered = table_rows
        .iter()
        .flat_map(|row| {
            row.try_get::<i64, _>("id")
                .ok()
                .and_then(|id| memberships.get(&id))
                .into_iter()
                .flatten()
                .map(|(_, address)| address.clone())
        })
        .collect::<Vec<_>>();
    let physical = physical_ordered.iter().cloned().collect::<BTreeSet<_>>();
    let pending_operation_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.lookup_table_operations
        WHERE family_id = $1
          AND (
              target_generation = $2
              OR route_lookup_table_id = ANY($3)
          )
          AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
        "#,
    )
    .bind(family_id)
    .bind(generation)
    .bind(&table_ids)
    .fetch_one(&mut *tx)
    .await?;
    let rows_ready = !table_rows.is_empty()
        && table_rows.len() == shard_plan.len()
        && table_rows.iter().zip(&shard_plan).all(|(row, shard)| {
            let table_id = row.try_get::<i64, _>("id").ok();
            let membership = table_id
                .and_then(|id| memberships.get(&id))
                .map(Vec::as_slice)
                .unwrap_or_default();
            matches!(
                row.try_get::<Option<String>, _>("desired_state")
                    .ok()
                    .flatten()
                    .as_deref(),
                Some("active" | "standby")
            ) && row
                .try_get::<Option<i32>, _>("usable_address_count")
                .ok()
                .flatten()
                == row.try_get::<i32, _>("address_count").ok()
                && row
                    .try_get::<Option<i64>, _>("last_verified_slot")
                    .ok()
                    .flatten()
                    .is_some()
                && row
                    .try_get::<Option<i32>, _>("shard_ordinal")
                    .ok()
                    .flatten()
                    == Some(shard.shard_ordinal)
                && row
                    .try_get::<Option<i32>, _>("allocation_high_water")
                    .ok()
                    .flatten()
                    == Some(allocation_high_water)
                && row.try_get::<i32, _>("address_count").ok()
                    == i32::try_from(shard.addresses.len()).ok()
                && membership.len() == shard.addresses.len()
                && membership
                    .iter()
                    .enumerate()
                    .all(|(ordinal, (stored_ordinal, _))| {
                        i32::try_from(ordinal).ok() == Some(*stored_ordinal)
                    })
                && membership
                    .iter()
                    .map(|(_, address)| address)
                    .eq(shard.addresses.iter())
        });
    Ok(SharedMarketCatalogGenerationEvidence {
        ready: rows_ready && pending_operation_count == 0 && physical_ordered == desired_ordered,
        missing_addresses: desired.difference(&physical).cloned().collect(),
        extra_addresses: physical.difference(&desired).cloned().collect(),
    })
}

async fn load_reusable_only_cutover_preflight_in_connection(
    tx: &mut sqlx::PgConnection,
    cluster: &str,
) -> Result<ReusableOnlyCutoverPreflight, OrchestratorError> {
    let catalog = load_shared_market_catalog_head_in_connection(
        tx,
        cluster,
        SharedMarketCatalogHeadLock::Update,
    )
    .await?
    .ok_or_else(|| {
        OrchestratorError::StoreInvariant(format!(
            "cluster {cluster:?} has no authoritative shared-market catalog head"
        ))
    })?;
    let active_generation = catalog.active_generation.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "shared-market catalog has no active generation".to_owned(),
        )
    })?;
    let target_generation = catalog.target_generation.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "shared-market catalog has no target generation".to_owned(),
        )
    })?;
    if catalog.readiness_state != SharedMarketCatalogReadiness::Active
        || active_generation != target_generation
    {
        return Err(OrchestratorError::StoreInvariant(
            "shared-market catalog is not active on its target generation".to_owned(),
        ));
    }
    let rows = sqlx::query(
        r#"
        SELECT * FROM loyal_yield.route_lookup_tables
        WHERE family_id = $1 AND generation = $2
          AND allocation_kind = 'shared_market'
          AND desired_state = 'active'
        ORDER BY shard_ordinal, id FOR UPDATE
        "#,
    )
    .bind(catalog.family_id)
    .bind(active_generation)
    .fetch_all(&mut *tx)
    .await?;
    let family_high_water: i32 = sqlx::query_scalar(
        "SELECT allocation_high_water FROM loyal_yield.lookup_table_families WHERE id = $1",
    )
    .bind(catalog.family_id)
    .fetch_one(&mut *tx)
    .await?;
    let expected_addresses = catalog
        .addresses
        .iter()
        .map(|row| row.address.clone())
        .collect::<Vec<_>>();
    let shard_capacity = u16::try_from(family_high_water).map_err(|_| {
        OrchestratorError::StoreInvariant(
            "shared-market family has an invalid allocation high-water".to_owned(),
        )
    })?;
    let shard_plan = append_pack_shared_market_shards(&expected_addresses, shard_capacity)
        .map_err(domain_store_error)?;
    if rows.is_empty() || rows.len() != shard_plan.len() {
        return Err(OrchestratorError::StoreInvariant(format!(
            "shared-market cutover requires {} exact active physical ALT shard(s), found {}",
            shard_plan.len(),
            rows.len()
        )));
    }
    let mut shared_tables = Vec::with_capacity(rows.len());
    for (row, shard) in rows.iter().zip(&shard_plan) {
        let table = reusable_lookup_table_from_row(row)?;
        let durable: bool = row.try_get("durable")?;
        let last_extended_slot: Option<i64> = row.try_get("last_extended_slot")?;
        let ordered_addresses = sqlx::query_scalar::<_, String>(
            "SELECT address FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id = $1 ORDER BY ordinal",
        )
        .bind(table.id)
        .fetch_all(&mut *tx)
        .await?;
        let ordered_hash = ordered_address_hash(&ordered_addresses);
        if !durable
            || table.legacy_status != "usable"
            || table.shard_ordinal != shard.shard_ordinal
            || table.allocation_high_water != family_high_water
            || last_extended_slot.is_none()
            || table.last_verified_slot.is_none()
            || table.address_count != table.usable_address_count
            || table.address_count != i32::try_from(ordered_addresses.len()).unwrap_or(-1)
            || table.address_hash != ordered_hash
            || ordered_addresses != shard.addresses
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "shared-market cutover physical ALT shard {} is not durable, exact, warm, and verified",
                shard.shard_ordinal
            )));
        }
        shared_tables.push(ReusableOnlyCutoverSharedTable {
            table_id: table.id,
            shard_ordinal: table.shard_ordinal,
            table_address: table.table_address,
            authority: table.authority,
            mutation_epoch: table.mutation_epoch,
            last_extended_slot: last_extended_slot.unwrap_or_default(),
            last_verified_slot: table.last_verified_slot.unwrap_or_default(),
            ordered_address_hash: ordered_hash,
            address_count: table.address_count,
            usable_address_count: table.usable_address_count,
            ordered_addresses,
        });
    }
    let shared_table_bundle_hash = reusable_only_cutover_shared_table_bundle_hash(&shared_tables);
    Ok(ReusableOnlyCutoverPreflight {
        cluster: cluster.to_owned(),
        catalog_revision_id: catalog.catalog_revision_id,
        catalog_revision: catalog.catalog_revision,
        manifest_id: catalog.manifest_id,
        manifest_hash: catalog.desired_set_hash,
        ordered_address_hash: ordered_address_hash(&expected_addresses),
        ordered_addresses: expected_addresses,
        shared_family_id: catalog.family_id,
        active_generation,
        target_generation,
        shared_table_bundle_hash,
        shared_tables,
    })
}

async fn lookup_table_in_flight_mutation_count_in_connection(
    tx: &mut sqlx::PgConnection,
    cluster: &str,
) -> Result<i64, OrchestratorError> {
    Ok(sqlx::query_scalar(
        r#"
        SELECT count(*)::BIGINT
        FROM loyal_yield.lookup_table_operations operation
        JOIN loyal_yield.lookup_table_families family
          ON family.id = operation.family_id
        WHERE family.cluster = $1
          AND (
              operation.operation_state IN (
                  'leased', 'signed', 'submitted', 'confirmed', 'finalized',
                  'reconciled', 'needs_reconcile'
              )
              OR (
                  operation.operation_state = 'retry_wait'
                  AND operation.transaction_signature IS NOT NULL
              )
          )
        "#,
    )
    .bind(cluster)
    .fetch_one(&mut *tx)
    .await?)
}

async fn update_shared_market_catalog_plan_state_in_connection(
    tx: &mut sqlx::PgConnection,
    catalog: &SharedMarketCatalogHeadRecord,
    target_generation: i32,
) -> Result<(), OrchestratorError> {
    let evidence = shared_market_catalog_generation_evidence_in_connection(
        tx,
        catalog.family_id,
        Some(target_generation),
        &catalog.addresses,
    )
    .await?;
    let active = catalog.active_generation == Some(target_generation) && evidence.ready;
    let updated = sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_shared_market_catalog_heads
        SET target_generation = $3,
            readiness_state = CASE
                WHEN readiness_state = 'failed' THEN 'failed'
                WHEN $4 THEN 'active'
                ELSE 'provisioning'
            END,
            activated_at = CASE
                WHEN $4 THEN COALESCE(activated_at, now())
                ELSE NULL
            END,
            updated_at = now()
        WHERE family_id = $1 AND catalog_revision_id = $2
        "#,
    )
    .bind(catalog.family_id)
    .bind(catalog.catalog_revision_id)
    .bind(target_generation)
    .bind(active)
    .execute(&mut *tx)
    .await?;
    if updated.rows_affected() != 1 {
        return Err(OrchestratorError::StoreInvariant(format!(
            "shared-market catalog revision {} lost its head fence during planning",
            catalog.catalog_revision_id
        )));
    }
    Ok(())
}

async fn activate_shared_market_catalog_generation_in_connection(
    tx: &mut sqlx::PgConnection,
    family_id: i64,
    target_generation: i32,
    rollback_until: DateTime<Utc>,
) -> Result<(), OrchestratorError> {
    let family_row =
        sqlx::query("SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 FOR UPDATE")
            .bind(family_id)
            .fetch_optional(&mut *tx)
            .await?
            .ok_or_else(|| stale_store_update("shared-market family", family_id))?;
    let family = lookup_table_family_from_row(&family_row)?;
    if family.kind != LookupTableFamilyKind::SharedMarket {
        return Err(OrchestratorError::StoreInvariant(format!(
            "lookup-table family {family_id} is not shared-market"
        )));
    }
    let mut affected_generations = vec![target_generation];
    if let Some(active_generation) = family.active_generation {
        affected_generations.push(active_generation);
    }
    affected_generations.sort_unstable();
    affected_generations.dedup();
    let affected_rows = sqlx::query(
        r#"
        SELECT id FROM loyal_yield.route_lookup_tables
        WHERE family_id = $1 AND generation = ANY($2)
        ORDER BY id FOR UPDATE
        "#,
    )
    .bind(family_id)
    .bind(&affected_generations)
    .fetch_all(&mut *tx)
    .await?;
    let affected_table_ids = affected_rows
        .iter()
        .map(|row| row.try_get::<i64, _>("id"))
        .collect::<Result<Vec<_>, _>>()?;
    let live_usage_count: i64 = if affected_table_ids.is_empty() {
        0
    } else {
        sqlx::query_scalar(
            r#"
            SELECT count(*) FROM loyal_yield.lookup_table_usage_leases
            WHERE route_lookup_table_id = ANY($1)
              AND released_at IS NULL AND expires_at > now()
            "#,
        )
        .bind(&affected_table_ids)
        .fetch_one(&mut *tx)
        .await?
    };
    if live_usage_count != 0 {
        return Err(OrchestratorError::StoreInvariant(format!(
            "shared-market family {family_id} generation activation has an unexpired usage lease"
        )));
    }
    if let Some(current_generation) = family.active_generation {
        if current_generation != target_generation {
            sqlx::query(
                r#"
                UPDATE loyal_yield.route_lookup_tables
                SET desired_state = 'standby', accepting_allocations = FALSE,
                    rollback_until = $3, updated_at = now()
                WHERE family_id = $1 AND generation = $2
                  AND allocation_kind = 'shared_market'
                  AND desired_state = 'active'
                "#,
            )
            .bind(family_id)
            .bind(current_generation)
            .bind(rollback_until)
            .execute(&mut *tx)
            .await?;
        }
    }
    sqlx::query(
        r#"
        UPDATE loyal_yield.route_lookup_tables
        SET desired_state = 'active', status = 'usable',
            rollback_until = $3, updated_at = now()
        WHERE family_id = $1 AND generation = $2
          AND allocation_kind = 'shared_market'
        "#,
    )
    .bind(family_id)
    .bind(target_generation)
    .bind(rollback_until)
    .execute(&mut *tx)
    .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_families
        SET previous_generation = CASE
                WHEN active_generation IS DISTINCT FROM $2 THEN active_generation
                ELSE previous_generation END,
            active_generation = $2, rollback_until = $3, updated_at = now()
        WHERE id = $1
        "#,
    )
    .bind(family_id)
    .bind(target_generation)
    .bind(rollback_until)
    .execute(&mut *tx)
    .await?;
    Ok(())
}

async fn upsert_vault_desired_head_in_tx(
    tx: &mut sqlx::PgConnection,
    family_id: i64,
    vault_id: VaultId,
    binding_ordinal: i32,
    manifest_id: i64,
) -> Result<i64, OrchestratorError> {
    let desired_revision = sqlx::query_scalar::<_, i64>(
        r#"
        INSERT INTO loyal_yield.lookup_table_vault_desired_heads
            (family_id, vault_id, binding_ordinal, manifest_id, desired_revision)
        VALUES ($1, $2, $3, $4, 1)
        ON CONFLICT (family_id, vault_id, binding_ordinal) DO UPDATE SET
            manifest_id = EXCLUDED.manifest_id,
            desired_revision = CASE
                WHEN lookup_table_vault_desired_heads.manifest_id = EXCLUDED.manifest_id
                THEN lookup_table_vault_desired_heads.desired_revision
                ELSE lookup_table_vault_desired_heads.desired_revision + 1
            END,
            updated_at = CASE
                WHEN lookup_table_vault_desired_heads.manifest_id = EXCLUDED.manifest_id
                THEN lookup_table_vault_desired_heads.updated_at
                ELSE now()
            END
        RETURNING desired_revision
        "#,
    )
    .bind(family_id)
    .bind(vault_id.as_i64())
    .bind(binding_ordinal)
    .bind(manifest_id)
    .fetch_one(&mut *tx)
    .await?;
    Ok(desired_revision)
}

async fn supersede_stale_vault_binding_revisions_in_tx(
    tx: &mut sqlx::PgConnection,
    family_id: i64,
    vault_id: VaultId,
    binding_ordinal: i32,
    manifest_id: i64,
    desired_head_revision: i64,
) -> Result<(), OrchestratorError> {
    // Operations retain their immutable binding/signature attribution for
    // reconciliation, but a superseded binding immediately stops reserving
    // capacity and can never return to a warm/active lifecycle.
    sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_vault_bindings
        SET lifecycle_state = 'failed',
            deactivated_at = COALESCE(deactivated_at, now()),
            updated_at = now()
        WHERE family_id = $1 AND vault_id = $2 AND binding_ordinal = $3
          AND lifecycle_state IN ('preparing', 'warming')
          AND (manifest_id <> $4 OR desired_head_revision <> $5)
        "#,
    )
    .bind(family_id)
    .bind(vault_id.as_i64())
    .bind(binding_ordinal)
    .bind(manifest_id)
    .bind(desired_head_revision)
    .execute(&mut *tx)
    .await?;
    Ok(())
}

async fn resolve_or_persist_request_manifest_in_tx(
    tx: &mut sqlx::PgConnection,
    cluster: &str,
    request: &LookupTableProvisioningRequestRecord,
    subject: LookupTableManifestSubject,
    source_slot: Option<i64>,
) -> Result<LookupTableManifestRecord, OrchestratorError> {
    if subject == LookupTableManifestSubject::Vault {
        return resolve_or_persist_vault_aggregate_manifest_in_tx(
            tx,
            cluster,
            request,
            source_slot,
        )
        .await;
    }
    let (manifest_id, expected_hash, expected_count, family_kind) = match subject {
        LookupTableManifestSubject::SharedMarket => (
            request.shared_manifest_id,
            request.desired_shared_hash.as_deref(),
            request.desired_shared_address_count,
            LookupTableFamilyKind::SharedMarket,
        ),
        LookupTableManifestSubject::Vault => (
            request.vault_manifest_id,
            request.desired_vault_hash.as_deref(),
            request.desired_vault_address_count,
            LookupTableFamilyKind::VaultShards,
        ),
    };
    if let Some(manifest_id) = manifest_id {
        let row = sqlx::query(
            r#"
            SELECT manifest.*
            FROM loyal_yield.lookup_table_manifests manifest
            JOIN loyal_yield.lookup_table_families family ON family.id = manifest.family_id
            WHERE manifest.id = $1 AND family.cluster = $2 AND family.kind = $3
              AND manifest.subject_kind = $4 AND manifest.sealed_at IS NOT NULL
              AND ($4 <> 'vault' OR manifest.vault_id = $5)
            FOR SHARE OF manifest
            "#,
        )
        .bind(manifest_id)
        .bind(cluster)
        .bind(family_kind.as_str())
        .bind(subject.as_str())
        .bind(request.vault_id.as_i64())
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("sealed lookup-table manifest", manifest_id))?;
        let address_rows = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id = $1 ORDER BY ordinal",
        )
        .bind(manifest_id)
        .fetch_all(&mut *tx)
        .await?;
        let manifest = lookup_table_manifest_from_rows(&row, &address_rows)?;
        if expected_hash.is_some_and(|hash| hash != manifest.desired_set_hash)
            || expected_count != 0 && expected_count != manifest.address_count
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "provisioning request manifest {manifest_id} does not match its sealed request identity"
            )));
        }
        return Ok(manifest);
    }

    let expected_hash = expected_hash
        .filter(|hash| !hash.is_empty())
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "sealed provisioning request {} lacks {} manifest hash",
                request.id,
                subject.as_str()
            ))
        })?;
    let family_rows = sqlx::query(
        r#"
        SELECT * FROM loyal_yield.lookup_table_families
        WHERE cluster = $1 AND kind = $2 AND desired_state = 'active'
        ORDER BY logical_name, id
        FOR SHARE
        "#,
    )
    .bind(cluster)
    .bind(family_kind.as_str())
    .fetch_all(&mut *tx)
    .await?;
    if family_rows.len() != 1 {
        return Err(OrchestratorError::StoreInvariant(format!(
            "cluster {cluster:?} requires exactly one active {} lookup-table family, found {}",
            family_kind.as_str(),
            family_rows.len()
        )));
    }
    let family = lookup_table_family_from_row(&family_rows[0])?;
    let address_rows = sqlx::query(
        r#"
        SELECT address, semantic_class, ordinal, account_role, is_writable
        FROM loyal_yield.lookup_table_provisioning_request_addresses
        WHERE request_id = $1 AND semantic_class = $2
        ORDER BY ordinal
        "#,
    )
    .bind(request.id)
    .bind(subject.as_str())
    .fetch_all(&mut *tx)
    .await?;
    let addresses = address_rows
        .iter()
        .map(lookup_table_manifest_address_from_row)
        .collect::<Result<Vec<_>, _>>()?;
    if addresses.len() != expected_count as usize {
        return Err(OrchestratorError::StoreInvariant(format!(
            "sealed provisioning request {} {} address count mismatch",
            request.id,
            subject.as_str()
        )));
    }
    let write = LookupTableManifestWrite {
        family_id: family.id,
        subject_kind: subject,
        subject_key: match subject {
            LookupTableManifestSubject::SharedMarket => {
                format!("route:{}", request.requirements_fingerprint)
            }
            LookupTableManifestSubject::Vault => format!("vault:{}", request.vault_id.as_i64()),
        },
        vault_id: (subject == LookupTableManifestSubject::Vault).then_some(request.vault_id),
        desired_set_hash: expected_hash.to_owned(),
        source_slot,
        planner_version: family.planner_version,
        catalog_version: family.catalog_version,
        addresses,
    };
    persist_lookup_table_manifest_in_tx(tx, write).await
}

async fn resolve_or_persist_vault_aggregate_manifest_in_tx(
    tx: &mut sqlx::PgConnection,
    cluster: &str,
    request: &LookupTableProvisioningRequestRecord,
    source_slot: Option<i64>,
) -> Result<LookupTableManifestRecord, OrchestratorError> {
    // Aggregate revisions serialize per vault, not per family. Different
    // vaults can therefore seal demand concurrently while two cohorts for the
    // same vault cannot publish competing partial desired sets.
    let family_rows = sqlx::query(
        r#"
        SELECT * FROM loyal_yield.lookup_table_families
        WHERE cluster = $1 AND kind = 'vault_shards' AND desired_state = 'active'
        ORDER BY logical_name, id
        FOR SHARE
        "#,
    )
    .bind(cluster)
    .fetch_all(&mut *tx)
    .await?;
    if family_rows.len() != 1 {
        return Err(OrchestratorError::StoreInvariant(format!(
            "cluster {cluster:?} requires exactly one active vault_shards lookup-table family, found {}",
            family_rows.len()
        )));
    }
    let family = lookup_table_family_from_row(&family_rows[0])?;
    sqlx::query(
        r#"
        SELECT pg_advisory_xact_lock(
            hashtextextended(
                'reusable-alt-vault-manifest:' || $1::TEXT || ':' || $2::TEXT,
                0
            )
        )
        "#,
    )
    .bind(family.id)
    .bind(request.vault_id.as_i64())
    .execute(&mut *tx)
    .await?;

    let request_cohort_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_provisioning_request_addresses
        WHERE request_id = $1 AND semantic_class = 'vault'
        "#,
    )
    .bind(request.id)
    .fetch_one(&mut *tx)
    .await?;
    if request_cohort_count != i64::from(request.desired_vault_address_count) {
        return Err(OrchestratorError::StoreInvariant(format!(
            "sealed provisioning request {} vault cohort address count mismatch",
            request.id
        )));
    }

    let cohort_rows = sqlx::query(
        r#"
        SELECT address.address, address.account_role, address.is_writable
        FROM loyal_yield.lookup_table_provisioning_requests request
        JOIN loyal_yield.lookup_table_provisioning_request_addresses address
          ON address.request_id = request.id
        WHERE request.cluster = $1 AND request.vault_id = $2
          AND request.sealed_at IS NOT NULL
          AND request.request_status <> 'cancelled'
          AND address.semantic_class = 'vault'
        ORDER BY address.address, request.id, address.ordinal
        "#,
    )
    .bind(cluster)
    .bind(request.vault_id.as_i64())
    .fetch_all(&mut *tx)
    .await?;
    let mut aggregate = BTreeMap::<String, (BTreeSet<String>, bool)>::new();
    for row in cohort_rows {
        let address: String = row.try_get("address")?;
        let roles: String = row.try_get("account_role")?;
        let is_writable: bool = row.try_get("is_writable")?;
        let entry = aggregate.entry(address).or_default();
        for role in roles
            .split(',')
            .map(str::trim)
            .filter(|role| !role.is_empty())
        {
            entry.0.insert(role.to_owned());
        }
        entry.1 |= is_writable;
    }
    let addresses = aggregate
        .into_iter()
        .enumerate()
        .map(
            |(ordinal, (address, (roles, is_writable)))| LookupTableManifestAddressRecord {
                address,
                ordinal: ordinal as i32,
                semantic_class: LookupTableManifestSubject::Vault,
                account_role: roles.into_iter().collect::<Vec<_>>().join(","),
                is_writable,
            },
        )
        .collect::<Vec<_>>();
    let desired_set_hash = lookup_table_manifest_address_records_hash(&addresses);
    persist_lookup_table_manifest_in_tx(
        tx,
        LookupTableManifestWrite {
            family_id: family.id,
            subject_kind: LookupTableManifestSubject::Vault,
            subject_key: format!("vault:{}:aggregate", request.vault_id.as_i64()),
            vault_id: Some(request.vault_id),
            desired_set_hash,
            source_slot,
            planner_version: family.planner_version,
            catalog_version: family.catalog_version,
            addresses,
        },
    )
    .await
}

async fn persist_lookup_table_manifest_in_tx(
    tx: &mut sqlx::PgConnection,
    mut input: LookupTableManifestWrite,
) -> Result<LookupTableManifestRecord, OrchestratorError> {
    input.addresses.sort_by_key(|address| address.ordinal);
    validate_manifest_write(&input)?;
    let inserted_id = sqlx::query_scalar::<_, i64>(
        r#"
        INSERT INTO loyal_yield.lookup_table_manifests
            (family_id, subject_kind, subject_key, vault_id, desired_set_hash,
             address_count, source_slot, planner_version, catalog_version)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
        ON CONFLICT (family_id, subject_kind, subject_key, desired_set_hash) DO NOTHING
        RETURNING id
        "#,
    )
    .bind(input.family_id)
    .bind(input.subject_kind.as_str())
    .bind(&input.subject_key)
    .bind(input.vault_id.map(VaultId::as_i64))
    .bind(&input.desired_set_hash)
    .bind(input.addresses.len() as i32)
    .bind(input.source_slot)
    .bind(&input.planner_version)
    .bind(&input.catalog_version)
    .fetch_optional(&mut *tx)
    .await?;
    let manifest_id = if let Some(manifest_id) = inserted_id {
        if !input.addresses.is_empty() {
            let mut query = QueryBuilder::<Postgres>::new(
                "INSERT INTO loyal_yield.lookup_table_manifest_addresses (manifest_id, address, ordinal, semantic_class, account_role, is_writable) ",
            );
            query.push_values(input.addresses.iter(), |mut row, address| {
                row.push_bind(manifest_id)
                    .push_bind(&address.address)
                    .push_bind(address.ordinal)
                    .push_bind(address.semantic_class.as_str())
                    .push_bind(&address.account_role)
                    .push_bind(address.is_writable);
            });
            query.build().execute(&mut *tx).await?;
        }
        sqlx::query(
            "UPDATE loyal_yield.lookup_table_manifests SET sealed_at = now() WHERE id = $1",
        )
        .bind(manifest_id)
        .execute(&mut *tx)
        .await?;
        manifest_id
    } else {
        sqlx::query_scalar::<_, i64>(
            r#"
            SELECT id FROM loyal_yield.lookup_table_manifests
            WHERE family_id = $1 AND subject_kind = $2
              AND subject_key = $3 AND desired_set_hash = $4
            "#,
        )
        .bind(input.family_id)
        .bind(input.subject_kind.as_str())
        .bind(&input.subject_key)
        .bind(&input.desired_set_hash)
        .fetch_one(&mut *tx)
        .await?
    };
    let row = sqlx::query("SELECT * FROM loyal_yield.lookup_table_manifests WHERE id = $1")
        .bind(manifest_id)
        .fetch_one(&mut *tx)
        .await?;
    let address_rows = sqlx::query(
        "SELECT * FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id = $1 ORDER BY ordinal",
    )
    .bind(manifest_id)
    .fetch_all(&mut *tx)
    .await?;
    let manifest = lookup_table_manifest_from_rows(&row, &address_rows)?;
    if manifest.addresses != input.addresses {
        return Err(OrchestratorError::StoreInvariant(format!(
            "lookup-table manifest {} idempotency collision has different addresses",
            manifest.id
        )));
    }
    Ok(manifest)
}

fn validate_manifest_write(input: &LookupTableManifestWrite) -> Result<(), OrchestratorError> {
    let vault_shape_is_valid = matches!(
        (input.subject_kind, input.vault_id),
        (LookupTableManifestSubject::SharedMarket, None)
            | (LookupTableManifestSubject::Vault, Some(_))
    );
    if !vault_shape_is_valid {
        return Err(OrchestratorError::StoreInvariant(
            "lookup-table manifest subject/vault shape is invalid".to_owned(),
        ));
    }
    let maximum_address_count = match input.subject_kind {
        LookupTableManifestSubject::SharedMarket => SHARED_MARKET_LOGICAL_CATALOG_MAX_ADDRESSES,
        LookupTableManifestSubject::Vault => usize::from(LOOKUP_TABLE_HARD_CAPACITY),
    };
    if input.addresses.len() > maximum_address_count {
        return Err(OrchestratorError::StoreInvariant(format!(
            "lookup-table {} manifest exceeds its logical address limit of {maximum_address_count}",
            input.subject_kind.as_str()
        )));
    }
    let mut seen = BTreeSet::new();
    for (expected_ordinal, address) in input.addresses.iter().enumerate() {
        if address.ordinal != expected_ordinal as i32
            || address.semantic_class != input.subject_kind
            || !seen.insert(&address.address)
        {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table manifest addresses must be unique, contiguous, and match the subject class"
                    .to_owned(),
            ));
        }
    }
    Ok(())
}

fn validate_request_addresses(
    addresses: &[LookupTableManifestAddressRecord],
    expected_class: LookupTableManifestSubject,
) -> Result<(), OrchestratorError> {
    validate_typed_addresses(
        addresses,
        expected_class,
        usize::from(LOOKUP_TABLE_HARD_CAPACITY),
        "provisioning request",
    )
}

fn validate_logical_shared_market_catalog_addresses(
    addresses: &[LookupTableManifestAddressRecord],
) -> Result<(), OrchestratorError> {
    validate_typed_addresses(
        addresses,
        LookupTableManifestSubject::SharedMarket,
        SHARED_MARKET_LOGICAL_CATALOG_MAX_ADDRESSES,
        "logical shared-market catalog",
    )
}

fn validate_typed_addresses(
    addresses: &[LookupTableManifestAddressRecord],
    expected_class: LookupTableManifestSubject,
    maximum_address_count: usize,
    context: &str,
) -> Result<(), OrchestratorError> {
    if addresses.len() > maximum_address_count {
        return Err(OrchestratorError::StoreInvariant(format!(
            "{context} {} class exceeds its address limit of {maximum_address_count}",
            expected_class.as_str(),
        )));
    }
    let mut seen = BTreeSet::new();
    for (expected_ordinal, address) in addresses.iter().enumerate() {
        if address.ordinal != expected_ordinal as i32
            || address.semantic_class != expected_class
            || address.account_role.is_empty()
            || !seen.insert(&address.address)
            || Pubkey::from_str(&address.address).is_err()
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "{context} {} addresses must be valid pubkeys, unique, contiguous, typed, and role-labelled",
                expected_class.as_str()
            )));
        }
    }
    Ok(())
}

fn validate_membership(
    addresses: &[LookupTableMembershipAddress],
    observed_slot: i64,
) -> Result<(), OrchestratorError> {
    if addresses.len() > usize::from(LOOKUP_TABLE_HARD_CAPACITY) || observed_slot < 0 {
        return Err(OrchestratorError::StoreInvariant(
            "lookup-table membership exceeds capacity or has an invalid observed slot".to_owned(),
        ));
    }
    let mut seen = BTreeSet::new();
    let mut encountered_unusable = false;
    for (expected_ordinal, address) in addresses.iter().enumerate() {
        let currently_usable = address.usable_after_slot <= observed_slot;
        encountered_unusable |= !currently_usable;
        if address.ordinal != expected_ordinal as i32
            || !seen.insert(&address.address)
            || address.added_slot < 0
            || address.usable_after_slot < address.added_slot
            || address.last_verified_slot < address.added_slot
            || (encountered_unusable && currently_usable)
        {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table membership must be unique, contiguous, slot-valid, and have a usable prefix"
                    .to_owned(),
            ));
        }
    }
    Ok(())
}

fn ordered_address_hash(addresses: &[String]) -> String {
    let mut hasher = Sha256::new();
    for address in addresses {
        hasher.update((address.len() as u64).to_le_bytes());
        hasher.update(address.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

/// Frozen digest used by the exact-scope ALT writer before reusable-v2.
///
/// This is intentionally exposed only so the audited legacy importer can
/// recognize pre-import rows. Imported registry state, immutable evidence,
/// reusable tables, and cleanup all use the canonical ordered digest above.
pub fn historical_legacy_lookup_table_address_hash(addresses: &[String]) -> String {
    let mut ordered = addresses.to_vec();
    ordered.sort();
    let mut hasher = Sha256::new();
    for address in ordered {
        hasher.update(address.as_bytes());
        hasher.update([0]);
    }
    format!("{:x}", hasher.finalize())
}

fn hash_length_prefixed_values<'a>(values: impl IntoIterator<Item = &'a str>) -> String {
    let mut hasher = Sha256::new();
    for value in values {
        hasher.update((value.len() as u64).to_le_bytes());
        hasher.update(value.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

async fn terminal_lookup_table_binding_operation_in_tx(
    tx: &mut sqlx::PgConnection,
    binding_id: i64,
    manifest_id: i64,
    route_lookup_table_id: i64,
) -> Result<Option<LookupTableOperationRecord>, OrchestratorError> {
    let row = sqlx::query(
        r#"
        SELECT * FROM loyal_yield.lookup_table_operations
        WHERE binding_id = $1
          AND manifest_id = $2
          AND route_lookup_table_id = $3
          AND operation_state IN ('permanent_failure', 'cancelled')
          AND NOT EXISTS (
              SELECT 1
              FROM loyal_yield.lookup_table_terminal_repair_operations repaired
              WHERE repaired.operation_id = lookup_table_operations.id
          )
        ORDER BY updated_at DESC, id DESC
        LIMIT 1
        "#,
    )
    .bind(binding_id)
    .bind(manifest_id)
    .bind(route_lookup_table_id)
    .fetch_optional(&mut *tx)
    .await?;
    row.as_ref()
        .map(lookup_table_operation_from_row)
        .transpose()
}

pub fn lookup_table_manifest_address_records_hash(
    addresses: &[LookupTableManifestAddressRecord],
) -> String {
    let mut hasher = Sha256::new();
    for address in addresses {
        for value in [
            address.address.as_str(),
            address.semantic_class.as_str(),
            address.account_role.as_str(),
        ] {
            hasher.update((value.len() as u64).to_le_bytes());
            hasher.update(value.as_bytes());
        }
        hasher.update(address.ordinal.to_le_bytes());
        hasher.update([u8::from(address.is_writable)]);
    }
    format!("{:x}", hasher.finalize())
}

fn is_sha256_hex(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
}

async fn enqueue_lookup_table_operation_in_tx(
    tx: &mut sqlx::PgConnection,
    input: &LookupTableOperationEnqueue,
) -> Result<LookupTableOperationRecord, OrchestratorError> {
    if matches!(
        input.operation_kind,
        LookupTableOperationKind::Create | LookupTableOperationKind::Rollover
    ) && input.route_lookup_table_id.is_none()
    {
        return Err(OrchestratorError::StoreInvariant(
            "create/rollover operation requires an atomically pre-reserved physical table"
                .to_owned(),
        ));
    }
    let mut addresses = input.addresses.clone();
    addresses.sort();
    addresses.dedup();
    let inserted_id = sqlx::query_scalar::<_, i64>(
        r#"
        INSERT INTO loyal_yield.lookup_table_operations
            (idempotency_key, family_id, route_lookup_table_id, manifest_id,
             binding_id, operation_kind, operation_state, target_generation,
             target_shard_ordinal, operation_context, mutation_epoch,
             estimated_fee_lamports, estimated_rent_lamports)
        VALUES ($1, $2, $3, $4, $5, $6, 'queued', $7, $8, $9, $10, $11, $12)
        ON CONFLICT (idempotency_key) DO NOTHING
        RETURNING id
        "#,
    )
    .bind(&input.idempotency_key)
    .bind(input.family_id)
    .bind(input.route_lookup_table_id)
    .bind(input.manifest_id)
    .bind(input.binding_id)
    .bind(input.operation_kind.as_str())
    .bind(input.target_generation)
    .bind(input.target_shard_ordinal)
    .bind(&input.operation_context)
    .bind(input.mutation_epoch)
    .bind(input.estimated_fee_lamports)
    .bind(input.estimated_rent_lamports)
    .fetch_optional(&mut *tx)
    .await?;
    let operation_id = if let Some(operation_id) = inserted_id {
        if !addresses.is_empty() {
            let mut query = QueryBuilder::<Postgres>::new(
                "INSERT INTO loyal_yield.lookup_table_operation_addresses (operation_id, address, ordinal) ",
            );
            query.push_values(
                addresses.iter().enumerate(),
                |mut row, (ordinal, address)| {
                    row.push_bind(operation_id)
                        .push_bind(address)
                        .push_bind(ordinal as i32);
                },
            );
            query.build().execute(&mut *tx).await?;
        }
        operation_id
    } else {
        sqlx::query_scalar::<_, i64>(
            "SELECT id FROM loyal_yield.lookup_table_operations WHERE idempotency_key = $1",
        )
        .bind(&input.idempotency_key)
        .fetch_one(&mut *tx)
        .await?
    };
    let row = sqlx::query("SELECT * FROM loyal_yield.lookup_table_operations WHERE id = $1")
        .bind(operation_id)
        .fetch_one(&mut *tx)
        .await?;
    let operation = lookup_table_operation_from_row(&row)?;
    let persisted_addresses = sqlx::query_scalar::<_, String>(
        "SELECT address FROM loyal_yield.lookup_table_operation_addresses WHERE operation_id = $1 ORDER BY ordinal",
    )
    .bind(operation_id)
    .fetch_all(&mut *tx)
    .await?;
    let planner_context_is_refreshable = input.manifest_id.is_some()
        && matches!(
            input.operation_kind,
            LookupTableOperationKind::Create
                | LookupTableOperationKind::Extend
                | LookupTableOperationKind::Rollover
        );
    if operation.family_id != input.family_id
        || operation.route_lookup_table_id != input.route_lookup_table_id
        || operation.manifest_id != input.manifest_id
        || operation.binding_id != input.binding_id
        || operation.operation_kind != input.operation_kind
        || operation.target_generation != input.target_generation
        || operation.target_shard_ordinal != input.target_shard_ordinal
        || (!planner_context_is_refreshable
            && operation.operation_context != input.operation_context)
        || operation.mutation_epoch != input.mutation_epoch
        || persisted_addresses != addresses
    {
        return Err(OrchestratorError::StoreInvariant(format!(
            "lookup-table operation idempotency collision for {}",
            input.idempotency_key
        )));
    }
    Ok(operation)
}

fn lookup_table_family_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTableFamilyRecord, OrchestratorError> {
    Ok(LookupTableFamilyRecord {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        logical_name: row.try_get("logical_name")?,
        kind: parse_store_enum("lookup-table family kind", row.try_get("kind")?)?,
        desired_state: parse_store_enum(
            "lookup-table family state",
            row.try_get("desired_state")?,
        )?,
        planner_version: row.try_get("planner_version")?,
        catalog_version: row.try_get("catalog_version")?,
        active_generation: row.try_get("active_generation")?,
        previous_generation: row.try_get("previous_generation")?,
        rollback_until: row.try_get("rollback_until")?,
        provisioning_authority: row.try_get("provisioning_authority")?,
        payer: row.try_get("payer")?,
        hard_capacity: row.try_get("hard_capacity")?,
        largest_atomic_expansion: row.try_get("largest_atomic_expansion")?,
        safety_margin: row.try_get("safety_margin")?,
        allocation_high_water: row.try_get("allocation_high_water")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn reusable_lookup_table_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<ReusableLookupTableRecord, OrchestratorError> {
    let allocation_kind = row
        .try_get::<Option<String>, _>("allocation_kind")?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant("reusable ALT lacks allocation_kind".to_owned())
        })?;
    let desired_state = row
        .try_get::<Option<String>, _>("desired_state")?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant("reusable ALT lacks desired_state".to_owned())
        })?;
    Ok(ReusableLookupTableRecord {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        scope: row.try_get("scope")?,
        table_address: row.try_get("table_address")?,
        authority: row.try_get("authority")?,
        payer: row.try_get("payer")?,
        legacy_status: row.try_get("status")?,
        address_count: row.try_get("address_count")?,
        address_hash: row.try_get("address_hash")?,
        family_id: row.try_get::<Option<i64>, _>("family_id")?.ok_or_else(|| {
            OrchestratorError::StoreInvariant("reusable ALT lacks family_id".to_owned())
        })?,
        allocation_kind: parse_store_enum("lookup-table allocation kind", allocation_kind)?,
        generation: row
            .try_get::<Option<i32>, _>("generation")?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant("reusable ALT lacks generation".to_owned())
            })?,
        shard_ordinal: row
            .try_get::<Option<i32>, _>("shard_ordinal")?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant("reusable ALT lacks shard_ordinal".to_owned())
            })?,
        desired_state: parse_store_enum("lookup-table lifecycle", desired_state)?,
        accepting_allocations: row
            .try_get::<Option<bool>, _>("accepting_allocations")?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "reusable ALT lacks accepting_allocations".to_owned(),
                )
            })?,
        allocation_high_water: row
            .try_get::<Option<i32>, _>("allocation_high_water")?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "reusable ALT lacks allocation_high_water".to_owned(),
                )
            })?,
        reserved_address_count: row
            .try_get::<Option<i32>, _>("reserved_address_count")?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "reusable ALT lacks reserved_address_count".to_owned(),
                )
            })?,
        usable_address_count: row
            .try_get::<Option<i32>, _>("usable_address_count")?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "reusable ALT lacks usable_address_count".to_owned(),
                )
            })?,
        last_extended_start_index: row.try_get("last_extended_start_index")?,
        last_verified_slot: row.try_get("last_verified_slot")?,
        last_verified_at: row.try_get("last_verified_at")?,
        mutation_epoch: row
            .try_get::<Option<i64>, _>("mutation_epoch")?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant("reusable ALT lacks mutation_epoch".to_owned())
            })?,
        rollback_until: row.try_get("rollback_until")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn lookup_table_manifest_from_rows(
    row: &sqlx::postgres::PgRow,
    address_rows: &[sqlx::postgres::PgRow],
) -> Result<LookupTableManifestRecord, OrchestratorError> {
    let addresses = address_rows
        .iter()
        .map(lookup_table_manifest_address_from_row)
        .collect::<Result<Vec<_>, OrchestratorError>>()?;
    let expected_count: i32 = row.try_get("address_count")?;
    if addresses.len() != expected_count as usize {
        return Err(OrchestratorError::StoreInvariant(format!(
            "sealed lookup-table manifest address count mismatch: expected {expected_count}, got {}",
            addresses.len()
        )));
    }
    Ok(LookupTableManifestRecord {
        id: row.try_get("id")?,
        family_id: row.try_get("family_id")?,
        subject_kind: parse_store_enum(
            "lookup-table manifest subject",
            row.try_get("subject_kind")?,
        )?,
        subject_key: row.try_get("subject_key")?,
        vault_id: row.try_get::<Option<i64>, _>("vault_id")?.map(VaultId),
        desired_set_hash: row.try_get("desired_set_hash")?,
        address_count: expected_count,
        source_slot: row.try_get("source_slot")?,
        planner_version: row.try_get("planner_version")?,
        catalog_version: row.try_get("catalog_version")?,
        sealed_at: row.try_get("sealed_at")?,
        created_at: row.try_get("created_at")?,
        addresses,
    })
}

fn lookup_table_manifest_address_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTableManifestAddressRecord, OrchestratorError> {
    Ok(LookupTableManifestAddressRecord {
        address: row.try_get("address")?,
        ordinal: row.try_get("ordinal")?,
        semantic_class: parse_store_enum(
            "lookup-table manifest subject",
            row.try_get("semantic_class")?,
        )?,
        account_role: row.try_get("account_role")?,
        is_writable: row.try_get("is_writable")?,
    })
}

fn shared_market_physical_drift_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<SharedMarketPhysicalDriftRecord, OrchestratorError> {
    let observed_addresses = serde_json::from_value::<Vec<String>>(
        row.try_get("observed_addresses")?,
    )
    .map_err(|error| {
        OrchestratorError::StoreInvariant(format!(
            "shared-market physical drift address evidence is invalid: {error}"
        ))
    })?;
    Ok(SharedMarketPhysicalDriftRecord {
        id: row.try_get("id")?,
        evidence_hash: row.try_get("evidence_hash")?,
        cluster: row.try_get("cluster")?,
        family_id: row.try_get("family_id")?,
        catalog_revision_id: row.try_get("catalog_revision_id")?,
        route_lookup_table_id: row.try_get("route_lookup_table_id")?,
        expected_mutation_epoch: row.try_get("expected_mutation_epoch")?,
        expected_table_address: row.try_get("expected_table_address")?,
        expected_authority: row.try_get("expected_authority")?,
        observed_slot: row.try_get("observed_slot")?,
        observed_table_present: row.try_get("observed_table_present")?,
        observed_authority: row.try_get("observed_authority")?,
        observed_active: row.try_get("observed_active")?,
        observed_last_extended_slot: row.try_get("observed_last_extended_slot")?,
        observed_warm: row.try_get("observed_warm")?,
        observed_address_hash: row.try_get("observed_address_hash")?,
        observed_addresses,
        reason: row.try_get("reason")?,
        reported_by: row.try_get("reported_by")?,
        resolution_state: parse_store_enum(
            "shared-market physical drift resolution",
            row.try_get::<String, _>("resolution_state")?,
        )?,
        resolution_target_generation: row.try_get("resolution_target_generation")?,
        resolved_at: row.try_get("resolved_at")?,
        created_at: row.try_get("created_at")?,
    })
}

fn lookup_table_binding_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTableVaultBindingRecord, OrchestratorError> {
    Ok(LookupTableVaultBindingRecord {
        id: row.try_get("id")?,
        vault_id: VaultId(row.try_get("vault_id")?),
        family_id: row.try_get("family_id")?,
        route_lookup_table_id: row.try_get("route_lookup_table_id")?,
        manifest_id: row.try_get("manifest_id")?,
        binding_ordinal: row.try_get("binding_ordinal")?,
        desired_head_revision: row.try_get("desired_head_revision")?,
        allocation_mode: parse_store_enum(
            "lookup-table binding mode",
            row.try_get("allocation_mode")?,
        )?,
        reserved_capacity: row.try_get("reserved_capacity")?,
        predecessor_binding_id: row.try_get("predecessor_binding_id")?,
        lifecycle_state: parse_store_enum(
            "lookup-table binding lifecycle",
            row.try_get("lifecycle_state")?,
        )?,
        active_from_slot: row.try_get("active_from_slot")?,
        active_until_slot: row.try_get("active_until_slot")?,
        activated_at: row.try_get("activated_at")?,
        deactivated_at: row.try_get("deactivated_at")?,
        rollback_until: row.try_get("rollback_until")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn lookup_table_operation_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTableOperationRecord, OrchestratorError> {
    Ok(LookupTableOperationRecord {
        id: row.try_get("id")?,
        idempotency_key: row.try_get("idempotency_key")?,
        family_id: row.try_get("family_id")?,
        route_lookup_table_id: row.try_get("route_lookup_table_id")?,
        manifest_id: row.try_get("manifest_id")?,
        binding_id: row.try_get("binding_id")?,
        operation_kind: parse_store_enum(
            "lookup-table operation kind",
            row.try_get("operation_kind")?,
        )?,
        operation_state: parse_store_enum(
            "lookup-table operation state",
            row.try_get("operation_state")?,
        )?,
        target_generation: row.try_get("target_generation")?,
        target_shard_ordinal: row.try_get("target_shard_ordinal")?,
        operation_context: row.try_get("operation_context")?,
        mutation_epoch: row.try_get("mutation_epoch")?,
        lease_owner: row.try_get("lease_owner")?,
        lease_expires_at: row.try_get("lease_expires_at")?,
        fencing_token: row.try_get("fencing_token")?,
        transaction_signature: row.try_get("transaction_signature")?,
        message_hash: row.try_get("message_hash")?,
        recent_blockhash: row.try_get("recent_blockhash")?,
        last_valid_block_height: row.try_get("last_valid_block_height")?,
        attempt_count: row.try_get("attempt_count")?,
        next_attempt_at: row.try_get("next_attempt_at")?,
        error_code: row.try_get("error_code")?,
        error_detail: row.try_get("error_detail")?,
        submitted_slot: row.try_get("submitted_slot")?,
        confirmed_slot: row.try_get("confirmed_slot")?,
        finalized_slot: row.try_get("finalized_slot")?,
        reconciled_slot: row.try_get("reconciled_slot")?,
        estimated_fee_lamports: row.try_get("estimated_fee_lamports")?,
        estimated_rent_lamports: row.try_get("estimated_rent_lamports")?,
        actual_fee_lamports: row.try_get("actual_fee_lamports")?,
        actual_rent_lamports: row.try_get("actual_rent_lamports")?,
        reclaimed_rent_lamports: row.try_get("reclaimed_rent_lamports")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn lookup_table_readiness_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTableReadinessRecord, OrchestratorError> {
    Ok(LookupTableReadinessRecord {
        cluster: row.try_get("cluster")?,
        vault_id: VaultId(row.try_get("vault_id")?),
        route_fingerprint: row.try_get("route_fingerprint")?,
        requirements_fingerprint: row.try_get("requirements_fingerprint")?,
        route_kind: row.try_get("route_kind")?,
        source_reserve: row.try_get("source_reserve")?,
        target_reserve: row.try_get("target_reserve")?,
        manifest_id: row.try_get("manifest_id")?,
        shared_family_id: row.try_get("shared_family_id")?,
        vault_binding_id: row.try_get("vault_binding_id")?,
        readiness_state: parse_store_enum(
            "lookup-table readiness state",
            row.try_get("readiness_state")?,
        )?,
        required_address_count: row.try_get("required_address_count")?,
        covered_address_count: row.try_get("covered_address_count")?,
        missing_addresses: row.try_get("missing_addresses")?,
        legacy_table_ids: row.try_get("legacy_table_ids")?,
        reusable_table_ids: row.try_get("reusable_table_ids")?,
        compiled_message_size: row.try_get("compiled_message_size")?,
        packet_limit: row.try_get("packet_limit")?,
        observed_slot: row.try_get("observed_slot")?,
        observed_at: row.try_get("observed_at")?,
        selection_kind: row
            .try_get::<Option<String>, _>("selection_kind")?
            .map(|value| parse_store_enum("lookup-table selection kind", value))
            .transpose()?,
        fallback_reason: row.try_get("fallback_reason")?,
        rollout_mode: row
            .try_get::<Option<String>, _>("rollout_mode")?
            .map(|value| parse_store_enum("lookup-table rollout mode", value))
            .transpose()?,
        selected_table_ids: row.try_get("selected_table_ids")?,
        selected_table_count: row.try_get("selected_table_count")?,
        packet_fits: row.try_get("packet_fits")?,
        simulation_state: row
            .try_get::<Option<String>, _>("simulation_state")?
            .map(|value| parse_store_enum("lookup-table simulation state", value))
            .transpose()?,
        simulation_units_consumed: row.try_get("simulation_units_consumed")?,
        simulation_error: row.try_get("simulation_error")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn lookup_table_rollout_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTableRolloutControl, OrchestratorError> {
    Ok(LookupTableRolloutControl {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        vault_id: row.try_get::<Option<i64>, _>("vault_id")?.map(VaultId),
        rollout_mode: parse_store_enum("lookup-table rollout mode", row.try_get("rollout_mode")?)?,
        force_legacy: row.try_get("force_legacy")?,
        reason: row.try_get("reason")?,
        updated_by: row.try_get("updated_by")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn lookup_table_provisioner_control_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTableProvisionerControlRecord, OrchestratorError> {
    Ok(LookupTableProvisionerControlRecord {
        cluster: row.try_get("cluster")?,
        paused: row.try_get("paused")?,
        reason: row.try_get("reason")?,
        updated_by: row.try_get("updated_by")?,
        control_epoch: row.try_get("control_epoch")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn lookup_table_provisioner_broadcast_permit_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTableProvisionerBroadcastPermitRecord, OrchestratorError> {
    Ok(LookupTableProvisionerBroadcastPermitRecord {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        operation_id: row.try_get("operation_id")?,
        fencing_token: row.try_get("fencing_token")?,
        control_epoch: row.try_get("control_epoch")?,
        transaction_signature: row.try_get("transaction_signature")?,
        message_hash: row.try_get("message_hash")?,
        permit_state: row.try_get("permit_state")?,
        resolution_detail: row.try_get("resolution_detail")?,
        granted_at: row.try_get("granted_at")?,
        resolved_at: row.try_get("resolved_at")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn legacy_lookup_table_cleanup_attempt_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LegacyLookupTableCleanupAttemptRecord, OrchestratorError> {
    Ok(LegacyLookupTableCleanupAttemptRecord {
        id: row.try_get("id")?,
        route_lookup_table_id: row.try_get("route_lookup_table_id")?,
        cluster: row.try_get("cluster")?,
        table_address: row.try_get("table_address")?,
        operation_kind: parse_store_enum(
            "legacy lookup-table cleanup operation kind",
            row.try_get("operation_kind")?,
        )?,
        attempt_number: row.try_get("attempt_number")?,
        authorization_token: row.try_get("authorization_token")?,
        expected_authority: row.try_get("expected_authority")?,
        expected_address_count: row.try_get("expected_address_count")?,
        expected_address_hash: row.try_get("expected_address_hash")?,
        close_recipient: row.try_get("close_recipient")?,
        expected_reclaimed_lamports: row.try_get("expected_reclaimed_lamports")?,
        attempt_state: parse_store_enum(
            "legacy lookup-table cleanup attempt state",
            row.try_get("attempt_state")?,
        )?,
        transaction_signature: row.try_get("transaction_signature")?,
        message_hash: row.try_get("message_hash")?,
        recent_blockhash: row.try_get("recent_blockhash")?,
        last_valid_block_height: row.try_get("last_valid_block_height")?,
        estimated_fee_lamports: row.try_get("estimated_fee_lamports")?,
        recipient_balance_before: row.try_get("recipient_balance_before")?,
        submitted_at: row.try_get("submitted_at")?,
        finalized_slot: row.try_get("finalized_slot")?,
        recipient_balance_after: row.try_get("recipient_balance_after")?,
        actual_reclaimed_lamports: row.try_get("actual_reclaimed_lamports")?,
        error_code: row.try_get("error_code")?,
        error_detail: row.try_get("error_detail")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn lookup_table_usage_lease_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTableUsageLeaseRecord, OrchestratorError> {
    Ok(LookupTableUsageLeaseRecord {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        lease_kind: parse_store_enum("lookup-table usage lease kind", row.try_get("lease_kind")?)?,
        reference_key: row.try_get("reference_key")?,
        route_lookup_table_id: row.try_get("route_lookup_table_id")?,
        vault_id: row.try_get::<Option<i64>, _>("vault_id")?.map(VaultId),
        binding_id: row.try_get("binding_id")?,
        route_fingerprint: row.try_get("route_fingerprint")?,
        requirements_fingerprint: row.try_get("requirements_fingerprint")?,
        expires_at: row.try_get("expires_at")?,
        released_at: row.try_get("released_at")?,
    })
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct LookupTableProbeCounts {
    drifts: i64,
    requests: i64,
    decisions: i64,
    bindings: i64,
    operations: i64,
}

async fn lookup_table_probe_counts(
    tx: &mut sqlx::PgConnection,
) -> Result<LookupTableProbeCounts, OrchestratorError> {
    let row = sqlx::query(
        r#"
        SELECT
            (SELECT count(*)::BIGINT FROM loyal_yield.lookup_table_shared_market_physical_drifts) AS drifts,
            (SELECT count(*)::BIGINT FROM loyal_yield.lookup_table_provisioning_requests) AS requests,
            (SELECT count(*)::BIGINT FROM loyal_yield.rebalance_decisions) AS decisions,
            (SELECT count(*)::BIGINT FROM loyal_yield.lookup_table_vault_bindings) AS bindings,
            (SELECT count(*)::BIGINT FROM loyal_yield.lookup_table_operations) AS operations
        "#,
    )
    .fetch_one(&mut *tx)
    .await?;
    Ok(LookupTableProbeCounts {
        drifts: row.try_get("drifts")?,
        requests: row.try_get("requests")?,
        decisions: row.try_get("decisions")?,
        bindings: row.try_get("bindings")?,
        operations: row.try_get("operations")?,
    })
}

fn lookup_table_precutover_probe_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTablePrecutoverProbeRecord, OrchestratorError> {
    Ok(LookupTablePrecutoverProbeRecord {
        id: row.try_get("id")?,
        probe_token: row.try_get("probe_token")?,
        cluster: row.try_get("cluster")?,
        vault_id: VaultId(row.try_get("vault_id")?),
        catalog_revision_id: row.try_get("catalog_revision_id")?,
        shared_manifest_id: row.try_get("shared_manifest_id")?,
        route_lookup_table_id: row.try_get("route_lookup_table_id")?,
        shared_table_address: row.try_get("shared_table_address")?,
        shared_authority: row.try_get("shared_authority")?,
        shared_mutation_epoch: row.try_get("shared_mutation_epoch")?,
        provisioner_control_epoch: row.try_get("provisioner_control_epoch")?,
        requirements_fingerprint: row.try_get("requirements_fingerprint")?,
        finalized_slot: row.try_get("finalized_slot")?,
        finalized_last_extended_slot: row.try_get("finalized_last_extended_slot")?,
        finalized_address_hash: row.try_get("finalized_address_hash")?,
        finalized_address_count: row.try_get("finalized_address_count")?,
        shared_table_bundle_hash: row.try_get("shared_table_bundle_hash")?,
        shared_table_count: row.try_get("shared_table_count")?,
        finalized_bundle_address_count: row.try_get("finalized_bundle_address_count")?,
        shared_tables: Vec::new(),
        finalized_shared_exact: row.try_get("finalized_shared_exact")?,
        synthetic_drift_evidence_hash: row.try_get("synthetic_drift_evidence_hash")?,
        drift_signal_count: row.try_get("drift_signal_count")?,
        drift_provisioning_request_count: row.try_get("drift_provisioning_request_count")?,
        duplicate_request_attempt_count: row.try_get("duplicate_request_attempt_count")?,
        distinct_request_count: row.try_get("distinct_request_count")?,
        decision_count: row.try_get("decision_count")?,
        binding_count: row.try_get("binding_count")?,
        operation_count: row.try_get("operation_count")?,
        rollback_residue_count: row.try_get("rollback_residue_count")?,
        catalog_head_restored: row.try_get("catalog_head_restored")?,
        signer_loaded: row.try_get("signer_loaded")?,
        transactions_sent: row.try_get("transactions_sent")?,
        result: row.try_get("result")?,
        created_at: row.try_get("created_at")?,
    })
}

fn lookup_table_precutover_probe_shared_table_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTablePrecutoverProbeSharedTableRecord, OrchestratorError> {
    Ok(LookupTablePrecutoverProbeSharedTableRecord {
        probe_run_id: row.try_get("probe_run_id")?,
        shard_ordinal: row.try_get("shard_ordinal")?,
        route_lookup_table_id: row.try_get("route_lookup_table_id")?,
        shared_table_address: row.try_get("shared_table_address")?,
        shared_authority: row.try_get("shared_authority")?,
        shared_mutation_epoch: row.try_get("shared_mutation_epoch")?,
        finalized_slot: row.try_get("finalized_slot")?,
        finalized_last_extended_slot: row.try_get("finalized_last_extended_slot")?,
        finalized_address_hash: row.try_get("finalized_address_hash")?,
        finalized_address_count: row.try_get("finalized_address_count")?,
    })
}

async fn lookup_table_precutover_probe_from_row_in_connection(
    tx: &mut sqlx::PgConnection,
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTablePrecutoverProbeRecord, OrchestratorError> {
    let mut record = lookup_table_precutover_probe_from_row(row)?;
    let child_rows = sqlx::query(
        r#"
        SELECT *
        FROM loyal_yield.lookup_table_precutover_probe_shared_tables
        WHERE probe_run_id = $1
        ORDER BY shard_ordinal
        "#,
    )
    .bind(record.id)
    .fetch_all(&mut *tx)
    .await?;
    record.shared_tables = child_rows
        .iter()
        .map(lookup_table_precutover_probe_shared_table_from_row)
        .collect::<Result<Vec<_>, _>>()?;
    if i32::try_from(record.shared_tables.len()).ok() != Some(record.shared_table_count) {
        return Err(OrchestratorError::StoreInvariant(format!(
            "pre-cutover probe {} shared-table evidence count drifted",
            record.id
        )));
    }
    Ok(record)
}

fn validate_shared_market_physical_drift_report(
    input: &SharedMarketPhysicalDriftReport,
) -> Result<String, OrchestratorError> {
    if input.cluster.trim().is_empty()
        || input.reason.trim().is_empty()
        || input.reported_by.trim().is_empty()
        || input.observed_slot < 0
        || input.expected_mutation_epoch < 0
        || input
            .observed_last_extended_slot
            .is_some_and(|slot| slot < 0)
        || input.observed_addresses.len() > usize::from(LOOKUP_TABLE_HARD_CAPACITY)
        || (!input.observed_table_present
            && (input.observed_authority.is_some()
                || input.observed_active
                || input.observed_last_extended_slot.is_some()
                || input.observed_warm
                || !input.observed_addresses.is_empty()))
        || (input.observed_warm && input.observed_last_extended_slot.is_none())
    {
        return Err(OrchestratorError::StoreInvariant(
            "shared-market physical drift report is malformed".to_owned(),
        ));
    }
    let mut observed_seen = BTreeSet::new();
    if input
        .observed_addresses
        .iter()
        .any(|address| !observed_seen.insert(address) || Pubkey::from_str(address).is_err())
    {
        return Err(OrchestratorError::StoreInvariant(
            "shared-market physical drift addresses must be valid, unique pubkeys in finalized order"
                .to_owned(),
        ));
    }
    Ok(ordered_address_hash(&input.observed_addresses))
}

fn validate_lookup_table_provisioning_request(
    input: &mut LookupTableProvisioningRequestUpsert,
) -> Result<(), OrchestratorError> {
    if input.shared_manifest_id.is_none()
        && input
            .desired_shared_hash
            .as_deref()
            .unwrap_or_default()
            .is_empty()
        || input.vault_manifest_id.is_none()
            && input
                .desired_vault_hash
                .as_deref()
                .unwrap_or_default()
                .is_empty()
    {
        return Err(OrchestratorError::StoreInvariant(
            "provisioning request requires shared and vault manifest identity".to_owned(),
        ));
    }
    input
        .shared_addresses
        .sort_by_key(|address| address.ordinal);
    input.vault_addresses.sort_by_key(|address| address.ordinal);
    validate_request_addresses(
        &input.shared_addresses,
        LookupTableManifestSubject::SharedMarket,
    )?;
    validate_request_addresses(&input.vault_addresses, LookupTableManifestSubject::Vault)?;
    let shared_set = input
        .shared_addresses
        .iter()
        .map(|address| &address.address)
        .collect::<BTreeSet<_>>();
    if input
        .vault_addresses
        .iter()
        .any(|address| shared_set.contains(&address.address))
    {
        return Err(OrchestratorError::StoreInvariant(
            "provisioning request address classes must be disjoint".to_owned(),
        ));
    }
    Ok(())
}

async fn report_shared_market_physical_drift_in_tx(
    tx: &mut sqlx::PgConnection,
    input: &SharedMarketPhysicalDriftReport,
    observed_hash: &str,
) -> Result<SharedMarketPhysicalDriftRecord, OrchestratorError> {
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended('shared-alt-drift:' || $1, 0))")
        .bind(&input.cluster)
        .execute(&mut *tx)
        .await?;
    let catalog = load_shared_market_catalog_head_in_connection(
        &mut *tx,
        &input.cluster,
        SharedMarketCatalogHeadLock::Update,
    )
    .await?
    .ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "shared-market physical drift has no current catalog head".to_owned(),
        )
    })?;
    if catalog.catalog_revision_id != input.catalog_revision_id
        || catalog.family_id != input.family_id
        || catalog.readiness_state != SharedMarketCatalogReadiness::Active
        || catalog.active_generation != catalog.target_generation
    {
        return Err(OrchestratorError::StoreInvariant(
            "shared-market physical drift report lost its active catalog fence".to_owned(),
        ));
    }
    let table_row = sqlx::query(
        r#"
        SELECT * FROM loyal_yield.route_lookup_tables
        WHERE id = $1 AND family_id = $2 AND generation = $3
          AND allocation_kind = 'shared_market'
        FOR UPDATE
        "#,
    )
    .bind(input.route_lookup_table_id)
    .bind(input.family_id)
    .bind(catalog.active_generation)
    .fetch_optional(&mut *tx)
    .await?
    .ok_or_else(|| {
        stale_store_update("shared-market physical table", input.route_lookup_table_id)
    })?;
    let table = reusable_lookup_table_from_row(&table_row)?;
    let expected_last_extended_slot: Option<i64> = table_row.try_get("last_extended_slot")?;
    if table.cluster != input.cluster
        || table.table_address != input.expected_table_address
        || table.authority != input.expected_authority
        || table.mutation_epoch != input.expected_mutation_epoch
    {
        return Err(OrchestratorError::StoreInvariant(
            "shared-market physical drift report lost its table/mutation fence".to_owned(),
        ));
    }
    let expected_addresses = sqlx::query_scalar::<_, String>(
        r#"
        SELECT address FROM loyal_yield.lookup_table_addresses
        WHERE route_lookup_table_id = $1 ORDER BY ordinal
        "#,
    )
    .bind(table.id)
    .fetch_all(&mut *tx)
    .await?;
    let catalog_addresses = catalog
        .addresses
        .iter()
        .map(|row| row.address.clone())
        .collect::<Vec<_>>();
    let shard_capacity = u16::try_from(table.allocation_high_water).map_err(|_| {
        OrchestratorError::StoreInvariant(format!(
            "shared-market table {} has an invalid allocation high-water",
            table.id
        ))
    })?;
    let shard_plan = append_pack_shared_market_shards(&catalog_addresses, shard_capacity)
        .map_err(domain_store_error)?;
    let planned_addresses = shard_plan
        .iter()
        .find(|shard| shard.shard_ordinal == table.shard_ordinal)
        .map(|shard| shard.addresses.as_slice());
    let is_drift = !input.observed_table_present
        || input.observed_authority.as_deref() != Some(table.authority.as_str())
        || !input.observed_active
        || !input.observed_warm
        || input.observed_last_extended_slot != expected_last_extended_slot
        || input.observed_addresses != expected_addresses
        || planned_addresses != Some(expected_addresses.as_slice());
    if !is_drift {
        return Err(OrchestratorError::StoreInvariant(
            "shared-market physical drift report matches the exact active catalog".to_owned(),
        ));
    }
    let observed_present = input.observed_table_present.to_string();
    let observed_active = input.observed_active.to_string();
    let observed_last_extended_slot = input
        .observed_last_extended_slot
        .map(|slot| slot.to_string())
        .unwrap_or_default();
    let observed_warm = input.observed_warm.to_string();
    let observed_slot = input.observed_slot.to_string();
    let expected_epoch = input.expected_mutation_epoch.to_string();
    let evidence_hash = hash_length_prefixed_values(
        [
            input.cluster.as_str(),
            &input.catalog_revision_id.to_string(),
            &input.family_id.to_string(),
            &input.route_lookup_table_id.to_string(),
            expected_epoch.as_str(),
            input.expected_table_address.as_str(),
            input.expected_authority.as_str(),
            observed_slot.as_str(),
            observed_present.as_str(),
            input.observed_authority.as_deref().unwrap_or(""),
            observed_active.as_str(),
            observed_last_extended_slot.as_str(),
            observed_warm.as_str(),
            observed_hash,
            input.reason.as_str(),
            input.reported_by.as_str(),
        ]
        .into_iter()
        .chain(input.observed_addresses.iter().map(String::as_str)),
    );
    let observed_json = serde_json::to_value(&input.observed_addresses).map_err(|error| {
        OrchestratorError::StoreInvariant(format!(
            "shared-market physical drift addresses cannot be serialized: {error}"
        ))
    })?;
    let inserted_id = sqlx::query_scalar::<_, i64>(
        r#"
        INSERT INTO loyal_yield.lookup_table_shared_market_physical_drifts
            (evidence_hash, cluster, family_id, catalog_revision_id,
             route_lookup_table_id, expected_mutation_epoch,
             expected_table_address, expected_authority, observed_slot,
             observed_table_present, observed_authority, observed_active,
             observed_last_extended_slot, observed_warm,
             observed_address_hash, observed_addresses, reason, reported_by)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
                $13, $14, $15, $16, $17, $18)
        ON CONFLICT (evidence_hash) DO NOTHING
        RETURNING id
        "#,
    )
    .bind(&evidence_hash)
    .bind(&input.cluster)
    .bind(input.family_id)
    .bind(input.catalog_revision_id)
    .bind(input.route_lookup_table_id)
    .bind(input.expected_mutation_epoch)
    .bind(&input.expected_table_address)
    .bind(&input.expected_authority)
    .bind(input.observed_slot)
    .bind(input.observed_table_present)
    .bind(&input.observed_authority)
    .bind(input.observed_active)
    .bind(input.observed_last_extended_slot)
    .bind(input.observed_warm)
    .bind(observed_hash)
    .bind(observed_json)
    .bind(&input.reason)
    .bind(&input.reported_by)
    .fetch_optional(&mut *tx)
    .await?;
    let drift_id = if let Some(id) = inserted_id {
        id
    } else {
        sqlx::query_scalar::<_, i64>(
            "SELECT id FROM loyal_yield.lookup_table_shared_market_physical_drifts WHERE evidence_hash = $1",
        )
        .bind(&evidence_hash)
        .fetch_one(&mut *tx)
        .await?
    };
    sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_shared_market_catalog_heads
        SET readiness_state = 'provisioning', activated_at = NULL,
            updated_at = now()
        WHERE family_id = $1 AND catalog_revision_id = $2
          AND target_generation IS NOT NULL
        "#,
    )
    .bind(input.family_id)
    .bind(input.catalog_revision_id)
    .execute(&mut *tx)
    .await?;
    let row = sqlx::query(
        "SELECT * FROM loyal_yield.lookup_table_shared_market_physical_drifts WHERE id = $1",
    )
    .bind(drift_id)
    .fetch_one(&mut *tx)
    .await?;
    shared_market_physical_drift_from_row(&row)
}

async fn upsert_lookup_table_provisioning_request_in_tx(
    tx: &mut sqlx::PgConnection,
    input: &LookupTableProvisioningRequestUpsert,
) -> Result<LookupTableProvisioningRequestRecord, OrchestratorError> {
    let inserted_id = sqlx::query_scalar::<_, i64>(
        r#"
        INSERT INTO loyal_yield.lookup_table_provisioning_requests
            (cluster, vault_id, route_fingerprint, requirements_fingerprint,
             shared_manifest_id, vault_manifest_id, desired_shared_hash,
             desired_vault_hash, desired_shared_address_count,
             desired_vault_address_count, request_status)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'requested')
        ON CONFLICT (cluster, vault_id, requirements_fingerprint) DO NOTHING
        RETURNING id
        "#,
    )
    .bind(&input.cluster)
    .bind(input.vault_id.as_i64())
    .bind(&input.route_fingerprint)
    .bind(&input.requirements_fingerprint)
    .bind(input.shared_manifest_id)
    .bind(input.vault_manifest_id)
    .bind(&input.desired_shared_hash)
    .bind(&input.desired_vault_hash)
    .bind(input.shared_addresses.len() as i32)
    .bind(input.vault_addresses.len() as i32)
    .fetch_optional(&mut *tx)
    .await?;
    let request_id = if let Some(request_id) = inserted_id {
        let addresses = input
            .shared_addresses
            .iter()
            .chain(input.vault_addresses.iter())
            .collect::<Vec<_>>();
        if !addresses.is_empty() {
            let mut query = QueryBuilder::<Postgres>::new(
                "INSERT INTO loyal_yield.lookup_table_provisioning_request_addresses (request_id, address, semantic_class, ordinal, account_role, is_writable) ",
            );
            query.push_values(addresses, |mut row, address| {
                row.push_bind(request_id)
                    .push_bind(&address.address)
                    .push_bind(address.semantic_class.as_str())
                    .push_bind(address.ordinal)
                    .push_bind(&address.account_role)
                    .push_bind(address.is_writable);
            });
            query.build().execute(&mut *tx).await?;
        }
        sqlx::query(
            "UPDATE loyal_yield.lookup_table_provisioning_requests SET sealed_at = now(), updated_at = now() WHERE id = $1",
        )
        .bind(request_id)
        .execute(&mut *tx)
        .await?;
        request_id
    } else {
        let existing_row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_provisioning_requests
            WHERE cluster = $1 AND vault_id = $2 AND requirements_fingerprint = $3
            FOR UPDATE
            "#,
        )
        .bind(&input.cluster)
        .bind(input.vault_id.as_i64())
        .bind(&input.requirements_fingerprint)
        .fetch_one(&mut *tx)
        .await?;
        let existing = lookup_table_provisioning_request_from_row(&existing_row)?;
        // `requirements_fingerprint` is the durable identity. Multiple route
        // shapes can require the exact same immutable address set; retain the
        // first route fingerprint as audit provenance without allocating twice.
        if existing.shared_manifest_id != input.shared_manifest_id
            && input.shared_manifest_id.is_some()
            || existing.vault_manifest_id != input.vault_manifest_id
                && input.vault_manifest_id.is_some()
            || existing.desired_shared_hash != input.desired_shared_hash
                && input.desired_shared_hash.is_some()
            || existing.desired_vault_hash != input.desired_vault_hash
                && input.desired_vault_hash.is_some()
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "sealed provisioning request {} idempotency collision has different content",
                existing.id
            )));
        }
        let persisted_rows = sqlx::query(
            r#"
            SELECT address, semantic_class, ordinal, account_role, is_writable
            FROM loyal_yield.lookup_table_provisioning_request_addresses
            WHERE request_id = $1
            ORDER BY semantic_class, ordinal
            "#,
        )
        .bind(existing.id)
        .fetch_all(&mut *tx)
        .await?;
        let persisted = persisted_rows
            .iter()
            .map(lookup_table_manifest_address_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let mut expected = input
            .shared_addresses
            .iter()
            .chain(input.vault_addresses.iter())
            .cloned()
            .collect::<Vec<_>>();
        expected.sort_by_key(|address| (address.semantic_class, address.ordinal));
        if !expected.is_empty() && persisted != expected {
            return Err(OrchestratorError::StoreInvariant(format!(
                "sealed provisioning request {} idempotency collision has different addresses",
                existing.id
            )));
        }
        let terminal_operation_failure = existing.request_status
            == LookupTableProvisioningRequestStatus::Failed
            && existing.error_code.as_deref() == Some("terminal_lookup_table_operation");
        if !terminal_operation_failure
            && matches!(
                existing.request_status,
                LookupTableProvisioningRequestStatus::Failed
                    | LookupTableProvisioningRequestStatus::Cancelled
                    | LookupTableProvisioningRequestStatus::Satisfied
            )
        {
            sqlx::query(
                r#"
                UPDATE loyal_yield.lookup_table_provisioning_requests
                SET request_status = 'requested', requested_at = now(),
                    lease_owner = NULL, lease_expires_at = NULL,
                    next_attempt_at = NULL, error_code = NULL, error_detail = NULL,
                    satisfied_at = NULL,
                    updated_at = now()
                WHERE id = $1
                "#,
            )
            .bind(existing.id)
            .execute(&mut *tx)
            .await?;
        }
        existing.id
    };
    let row =
        sqlx::query("SELECT * FROM loyal_yield.lookup_table_provisioning_requests WHERE id = $1")
            .bind(request_id)
            .fetch_one(&mut *tx)
            .await?;
    lookup_table_provisioning_request_from_row(&row)
}

fn lookup_table_provisioning_request_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<LookupTableProvisioningRequestRecord, OrchestratorError> {
    Ok(LookupTableProvisioningRequestRecord {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        vault_id: VaultId(row.try_get("vault_id")?),
        route_fingerprint: row.try_get("route_fingerprint")?,
        requirements_fingerprint: row.try_get("requirements_fingerprint")?,
        shared_manifest_id: row.try_get("shared_manifest_id")?,
        vault_manifest_id: row.try_get("vault_manifest_id")?,
        desired_shared_hash: row.try_get("desired_shared_hash")?,
        desired_vault_hash: row.try_get("desired_vault_hash")?,
        desired_shared_address_count: row.try_get("desired_shared_address_count")?,
        desired_vault_address_count: row.try_get("desired_vault_address_count")?,
        sealed_at: row.try_get("sealed_at")?,
        request_status: parse_store_enum(
            "lookup-table provisioning request status",
            row.try_get("request_status")?,
        )?,
        lease_owner: row.try_get("lease_owner")?,
        lease_expires_at: row.try_get("lease_expires_at")?,
        fencing_token: row.try_get("fencing_token")?,
        attempt_count: row.try_get("attempt_count")?,
        next_attempt_at: row.try_get("next_attempt_at")?,
        error_code: row.try_get("error_code")?,
        error_detail: row.try_get("error_detail")?,
        requested_at: row.try_get("requested_at")?,
        satisfied_at: row.try_get("satisfied_at")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

impl NeonSqlClient {
    pub async fn enqueue_lookup_table_operation(
        &self,
        mut input: LookupTableOperationEnqueue,
    ) -> Result<LookupTableOperationRecord, OrchestratorError> {
        input.addresses.sort();
        input.addresses.dedup();
        if matches!(
            input.operation_kind,
            LookupTableOperationKind::Create | LookupTableOperationKind::Rollover
        ) && (input.target_generation.is_none()
            || input.target_shard_ordinal.is_none()
            || input.route_lookup_table_id.is_none())
        {
            return Err(OrchestratorError::StoreInvariant(
                "create/rollover lookup-table operation requires an atomically pre-reserved table, target generation, and shard"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        Self::validate_cleanup_enqueue_in_tx(&mut *tx, &input).await?;
        let inserted_id = sqlx::query_scalar::<_, i64>(
            r#"
            INSERT INTO loyal_yield.lookup_table_operations
                (idempotency_key, family_id, route_lookup_table_id, manifest_id,
                 binding_id, operation_kind, operation_state, target_generation,
                 target_shard_ordinal, operation_context, mutation_epoch,
                 estimated_fee_lamports, estimated_rent_lamports)
            VALUES ($1, $2, $3, $4, $5, $6, 'queued', $7, $8, $9, $10, $11, $12)
            ON CONFLICT (idempotency_key) DO NOTHING
            RETURNING id
            "#,
        )
        .bind(&input.idempotency_key)
        .bind(input.family_id)
        .bind(input.route_lookup_table_id)
        .bind(input.manifest_id)
        .bind(input.binding_id)
        .bind(input.operation_kind.as_str())
        .bind(input.target_generation)
        .bind(input.target_shard_ordinal)
        .bind(&input.operation_context)
        .bind(input.mutation_epoch)
        .bind(input.estimated_fee_lamports)
        .bind(input.estimated_rent_lamports)
        .fetch_optional(&mut *tx)
        .await?;
        let operation_id = if let Some(operation_id) = inserted_id {
            if !input.addresses.is_empty() {
                let mut query = QueryBuilder::<Postgres>::new(
                    "INSERT INTO loyal_yield.lookup_table_operation_addresses (operation_id, address, ordinal) ",
                );
                query.push_values(
                    input.addresses.iter().enumerate(),
                    |mut row, (ordinal, address)| {
                        row.push_bind(operation_id)
                            .push_bind(address)
                            .push_bind(ordinal as i32);
                    },
                );
                query.build().execute(&mut *tx).await?;
            }
            operation_id
        } else {
            sqlx::query_scalar::<_, i64>(
                "SELECT id FROM loyal_yield.lookup_table_operations WHERE idempotency_key = $1",
            )
            .bind(&input.idempotency_key)
            .fetch_one(&mut *tx)
            .await?
        };
        tx.commit().await?;
        let (operation, addresses) = self
            .lookup_table_operation_with_addresses(operation_id)
            .await?
            .ok_or_else(|| stale_store_update("lookup-table operation", operation_id))?;
        let planner_context_is_refreshable = input.manifest_id.is_some()
            && matches!(
                input.operation_kind,
                LookupTableOperationKind::Create
                    | LookupTableOperationKind::Extend
                    | LookupTableOperationKind::Rollover
            );
        if operation.family_id != input.family_id
            || operation.operation_kind != input.operation_kind
            || operation.route_lookup_table_id != input.route_lookup_table_id
            || operation.manifest_id != input.manifest_id
            || operation.binding_id != input.binding_id
            || operation.target_generation != input.target_generation
            || operation.target_shard_ordinal != input.target_shard_ordinal
            || (!planner_context_is_refreshable
                && operation.operation_context != input.operation_context)
            || operation.mutation_epoch != input.mutation_epoch
            || (!planner_context_is_refreshable
                && operation.estimated_fee_lamports != input.estimated_fee_lamports)
            || (!planner_context_is_refreshable
                && operation.estimated_rent_lamports != input.estimated_rent_lamports)
            || addresses != input.addresses
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table operation idempotency collision for {}",
                input.idempotency_key
            )));
        }
        Ok(operation)
    }

    pub async fn lookup_table_operation_with_addresses(
        &self,
        operation_id: i64,
    ) -> Result<Option<(LookupTableOperationRecord, Vec<String>)>, OrchestratorError> {
        let Some(row) =
            sqlx::query("SELECT * FROM loyal_yield.lookup_table_operations WHERE id = $1")
                .bind(operation_id)
                .fetch_optional(self.pool())
                .await?
        else {
            return Ok(None);
        };
        let operation = lookup_table_operation_from_row(&row)?;
        let addresses = sqlx::query_scalar::<_, String>(
            "SELECT address FROM loyal_yield.lookup_table_operation_addresses WHERE operation_id = $1 ORDER BY ordinal",
        )
        .bind(operation_id)
        .fetch_all(self.pool())
        .await?;
        Ok(Some((operation, addresses)))
    }

    /// Returns unresolved terminal reusable-ALT dependencies in deterministic
    /// repair order. Failed create/rollover roots precede failed suffixes so a
    /// phantom table is quarantined once instead of retrying every dependent
    /// extension independently.
    pub async fn lookup_table_terminal_repair_candidates(
        &self,
        cluster: &str,
        limit: i64,
    ) -> Result<Vec<LookupTableTerminalRepairCandidate>, OrchestratorError> {
        if cluster.trim().is_empty() || !(1..=100).contains(&limit) {
            return Err(OrchestratorError::StoreInvariant(
                "terminal ALT repair requires a cluster and a limit between 1 and 100".to_owned(),
            ));
        }
        let operation_ids = sqlx::query_scalar::<_, i64>(
            r#"
            WITH unresolved AS (
                SELECT
                    operation.id,
                    operation.operation_kind,
                    operation.attempt_generation,
                    route_table.id AS table_id,
                    row_number() OVER (
                        PARTITION BY route_table.id
                        ORDER BY
                          CASE WHEN operation.operation_kind IN ('create', 'rollover') THEN 0 ELSE 1 END,
                          operation.attempt_generation,
                          operation.id
                    ) AS table_rank
                FROM loyal_yield.lookup_table_operations operation
                JOIN loyal_yield.lookup_table_families family
                  ON family.id = operation.family_id
                JOIN loyal_yield.route_lookup_tables route_table
                  ON route_table.id = operation.route_lookup_table_id
                WHERE family.cluster = $1
                  AND operation.operation_state = 'permanent_failure'
                  AND operation.operation_kind IN ('create', 'rollover', 'extend')
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.lookup_table_terminal_repair_operations repaired
                      WHERE repaired.operation_id = operation.id
                  )
            )
            SELECT id
            FROM unresolved
            WHERE table_rank = 1
            ORDER BY
              CASE WHEN operation_kind IN ('create', 'rollover') THEN 0 ELSE 1 END,
              table_id,
              attempt_generation,
              id
            LIMIT $2
            "#,
        )
        .bind(cluster)
        .bind(limit)
        .fetch_all(self.pool())
        .await?;
        let mut candidates = Vec::with_capacity(operation_ids.len());
        for operation_id in operation_ids {
            let Some((operation, operation_addresses)) = self
                .lookup_table_operation_with_addresses(operation_id)
                .await?
            else {
                return Err(stale_store_update(
                    "terminal lookup-table operation",
                    operation_id,
                ));
            };
            let table_id = operation.route_lookup_table_id.ok_or_else(|| {
                OrchestratorError::StoreInvariant(format!(
                    "terminal lookup-table operation {operation_id} has no physical table"
                ))
            })?;
            let physical_table = self
                .reusable_lookup_table(table_id)
                .await?
                .ok_or_else(|| stale_store_update("reusable lookup table", table_id))?;
            let persisted_membership = self.lookup_table_membership(table_id).await?;
            let sibling_rows = sqlx::query(
                r#"
                SELECT operation.*
                FROM loyal_yield.lookup_table_operations operation
                WHERE operation.route_lookup_table_id = $1
                  AND operation.id <> $2
                  AND operation.operation_state = 'permanent_failure'
                  AND operation.operation_kind = 'extend'
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.lookup_table_terminal_repair_operations repaired
                      WHERE repaired.operation_id = operation.id
                  )
                ORDER BY operation.mutation_epoch, operation.attempt_generation, operation.id
                "#,
            )
            .bind(table_id)
            .bind(operation_id)
            .fetch_all(self.pool())
            .await?;
            let unresolved_terminal_siblings = sibling_rows
                .iter()
                .map(lookup_table_operation_from_row)
                .collect::<Result<Vec<_>, _>>()?;
            candidates.push(LookupTableTerminalRepairCandidate {
                operation,
                operation_addresses,
                unresolved_terminal_siblings,
                physical_table,
                persisted_membership,
            });
        }
        Ok(candidates)
    }

    /// Repairs one terminal reusable-ALT dependency under a durable cluster
    /// pause and exact finalized evidence. This method never mutates a terminal
    /// operation. It either quarantines the empty phantom table and requeues
    /// affected sealed requests, or inserts one immutable successor operation
    /// for the exact failed suffix.
    pub async fn repair_terminal_lookup_table_operation(
        &self,
        input: LookupTableTerminalRepairRequest,
    ) -> Result<LookupTableTerminalRepairResult, OrchestratorError> {
        if input.cluster.trim().is_empty()
            || input.operation_id <= 0
            || input.expected_control_epoch < 0
            || input.chain.observed_slot < 0
            || input.reason.trim().is_empty()
            || input.updated_by.trim().is_empty()
            || Pubkey::from_str(&input.expected_policy_authority).is_err()
            || input.expected_policy_authority != STANDARD_POLICY_AUTHORITY
            || input.chain.ordered_addresses.len() > usize::from(LOOKUP_TABLE_HARD_CAPACITY)
        {
            return Err(OrchestratorError::StoreInvariant(
                "terminal ALT repair request is malformed".to_owned(),
            ));
        }
        let mut unique_addresses = BTreeSet::new();
        if input.chain.ordered_addresses.iter().any(|address| {
            Pubkey::from_str(address).is_err() || !unique_addresses.insert(address.as_str())
        }) {
            return Err(OrchestratorError::StoreInvariant(
                "terminal ALT repair finalized addresses are malformed or duplicated".to_owned(),
            ));
        }

        let mut tx = self.pool().begin().await?;
        let control_row = sqlx::query(
            r#"
            SELECT paused, control_epoch
            FROM loyal_yield.lookup_table_provisioner_controls
            WHERE cluster = $1
            FOR UPDATE
            "#,
        )
        .bind(&input.cluster)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "terminal ALT repair requires an existing durable provisioner control".to_owned(),
            )
        })?;
        let paused: bool = control_row.try_get("paused")?;
        let control_epoch: i64 = control_row.try_get("control_epoch")?;
        if !paused || control_epoch != input.expected_control_epoch {
            return Err(OrchestratorError::StoreInvariant(format!(
                "terminal ALT repair requires paused control epoch {}; observed paused={paused}, epoch={control_epoch}",
                input.expected_control_epoch
            )));
        }
        let active_permits: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*)
            FROM loyal_yield.lookup_table_provisioner_broadcast_permits
            WHERE cluster = $1 AND resolved_at IS NULL
            "#,
        )
        .bind(&input.cluster)
        .fetch_one(&mut *tx)
        .await?;
        if active_permits != 0 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "terminal ALT repair requires zero unresolved broadcast permits; found {active_permits}"
            )));
        }

        let identity_row = sqlx::query(
            r#"
            SELECT family_id, route_lookup_table_id
            FROM loyal_yield.lookup_table_operations
            WHERE id = $1
            "#,
        )
        .bind(input.operation_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("terminal lookup-table operation", input.operation_id))?;
        let family_id: i64 = identity_row.try_get("family_id")?;
        let table_id: i64 = identity_row
            .try_get::<Option<i64>, _>("route_lookup_table_id")?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "terminal lookup-table repair operation has no physical table".to_owned(),
                )
            })?;

        // All control-plane mutations use family -> table -> operation locks.
        let family_row =
            sqlx::query("SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 FOR SHARE")
                .bind(family_id)
                .fetch_optional(&mut *tx)
                .await?
                .ok_or_else(|| stale_store_update("lookup-table family", family_id))?;
        let family = lookup_table_family_from_row(&family_row)?;
        if family.cluster != input.cluster
            || family.provisioning_authority != input.expected_policy_authority
            || family.payer != input.expected_policy_authority
        {
            return Err(OrchestratorError::StoreInvariant(
                "terminal ALT repair cluster or standard policy identity drifted".to_owned(),
            ));
        }
        let table_row = sqlx::query(
            "SELECT * FROM loyal_yield.route_lookup_tables WHERE id = $1 AND family_id = $2 FOR UPDATE",
        )
        .bind(table_id)
        .bind(family_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("reusable lookup table", table_id))?;
        let table = reusable_lookup_table_from_row(&table_row)?;
        if table.cluster != input.cluster
            || table.authority != input.expected_policy_authority
            || table.payer != input.expected_policy_authority
        {
            return Err(OrchestratorError::StoreInvariant(
                "terminal ALT repair physical table identity drifted".to_owned(),
            ));
        }
        let operation_row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_operations WHERE id = $1 FOR UPDATE",
        )
        .bind(input.operation_id)
        .fetch_one(&mut *tx)
        .await?;
        let operation = lookup_table_operation_from_row(&operation_row)?;
        if operation.operation_state != LookupTableOperationStatus::PermanentFailure
            || operation.route_lookup_table_id != Some(table.id)
            || operation.family_id != family.id
            || operation.mutation_epoch != table.mutation_epoch
        {
            return Err(OrchestratorError::StoreInvariant(
                "terminal ALT repair target is no longer the exact failed table mutation"
                    .to_owned(),
            ));
        }
        let already_repaired: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.lookup_table_terminal_repair_operations
                WHERE operation_id = $1
            )
            "#,
        )
        .bind(operation.id)
        .fetch_one(&mut *tx)
        .await?;
        if already_repaired {
            return Err(OrchestratorError::StoreInvariant(format!(
                "terminal lookup-table operation {} already has repair lineage",
                operation.id
            )));
        }

        let membership_rows = sqlx::query(
            r#"
            SELECT address, ordinal, added_operation_id, added_slot,
                   usable_after_slot, last_verified_slot, last_verified_at
            FROM loyal_yield.lookup_table_addresses
            WHERE route_lookup_table_id = $1
            ORDER BY ordinal
            FOR UPDATE
            "#,
        )
        .bind(table.id)
        .fetch_all(&mut *tx)
        .await?;
        let membership = membership_rows
            .iter()
            .map(|row| {
                Ok(LookupTableMembershipAddress {
                    address: row.try_get("address")?,
                    ordinal: row.try_get("ordinal")?,
                    added_operation_id: row.try_get("added_operation_id")?,
                    added_slot: row.try_get("added_slot")?,
                    usable_after_slot: row.try_get("usable_after_slot")?,
                    last_verified_slot: row.try_get("last_verified_slot")?,
                    last_verified_at: row.try_get("last_verified_at")?,
                })
            })
            .collect::<Result<Vec<_>, OrchestratorError>>()?;
        let persisted_addresses = membership
            .iter()
            .map(|address| address.address.clone())
            .collect::<Vec<_>>();
        if table
            .last_verified_slot
            .is_some_and(|slot| slot > input.chain.observed_slot)
        {
            return Err(OrchestratorError::StoreInvariant(
                "terminal ALT repair finalized observation predates durable table evidence"
                    .to_owned(),
            ));
        }

        let root_no_effect = validate_lookup_table_terminal_no_effect(
            &operation,
            &input.no_effect,
            input.chain.observed_slot,
        )?;

        let active_usage_leases: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*)
            FROM loyal_yield.lookup_table_usage_leases
            WHERE route_lookup_table_id = $1
              AND released_at IS NULL
              AND expires_at > now()
            "#,
        )
        .bind(table.id)
        .fetch_one(&mut *tx)
        .await?;
        if active_usage_leases != 0 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "terminal ALT repair requires zero active usage leases; found {active_usage_leases}"
            )));
        }

        let operation_rows = sqlx::query(
            r#"
            SELECT *
            FROM loyal_yield.lookup_table_operations
            WHERE route_lookup_table_id = $1
            ORDER BY id
            FOR UPDATE
            "#,
        )
        .bind(table.id)
        .fetch_all(&mut *tx)
        .await?;
        let table_operations = operation_rows
            .iter()
            .map(lookup_table_operation_from_row)
            .collect::<Result<Vec<_>, _>>()?;

        let is_phantom = matches!(
            operation.operation_kind,
            LookupTableOperationKind::Create | LookupTableOperationKind::Rollover
        );
        let repair_kind = if is_phantom {
            if !matches!(
                input.chain.account_state,
                LookupTableTerminalAccountState::Missing
                    | LookupTableTerminalAccountState::NonLookupTable
            ) || !input.chain.ordered_addresses.is_empty()
                || input.chain.authority.is_some()
                || input.chain.last_extended_slot.is_some()
                || (input.chain.account_state == LookupTableTerminalAccountState::Missing
                    && input.chain.account_owner.is_some())
                || (input.chain.account_state == LookupTableTerminalAccountState::NonLookupTable
                    && input.chain.account_owner.as_deref()
                        == Some(address_lookup_table_program::id().to_string().as_str()))
                || !membership.is_empty()
                || table.address_count != 0
                || table.usable_address_count != 0
                || !matches!(
                    table.desired_state,
                    LookupTableLifecycle::Preparing
                        | LookupTableLifecycle::Warming
                        | LookupTableLifecycle::Failed
                )
            {
                return Err(OrchestratorError::StoreInvariant(
                    "phantom ALT repair requires a finalized missing/non-ALT account and an empty durable table"
                        .to_owned(),
                ));
            }
            "quarantine_phantom"
        } else if operation.operation_kind == LookupTableOperationKind::Extend {
            if input.chain.account_state != LookupTableTerminalAccountState::ActiveLookupTable
                || input.chain.account_owner.as_deref()
                    != Some(address_lookup_table_program::id().to_string().as_str())
                || input.chain.authority.as_deref() != Some(table.authority.as_str())
                || !input
                    .chain
                    .last_extended_slot
                    .is_some_and(|slot| slot >= 0 && slot < input.chain.observed_slot)
                || input.chain.ordered_addresses != persisted_addresses
                || i32::try_from(persisted_addresses.len()).ok() != Some(table.address_count)
                || table.usable_address_count != table.address_count
                || ordered_address_hash(&persisted_addresses) != table.address_hash
                || matches!(
                    table.desired_state,
                    LookupTableLifecycle::Failed
                        | LookupTableLifecycle::Deactivated
                        | LookupTableLifecycle::Closed
                )
            {
                return Err(OrchestratorError::StoreInvariant(
                    "failed suffix repair requires the exact active finalized ALT prefix"
                        .to_owned(),
                ));
            }
            "retry_suffix"
        } else {
            return Err(OrchestratorError::StoreInvariant(
                "only failed create, rollover, or extend operations are repairable".to_owned(),
            ));
        };

        let binding_rows = sqlx::query(
            r#"
            SELECT id, manifest_id, lifecycle_state
            FROM loyal_yield.lookup_table_vault_bindings
            WHERE route_lookup_table_id = $1
            ORDER BY id
            FOR UPDATE
            "#,
        )
        .bind(table.id)
        .fetch_all(&mut *tx)
        .await?;
        if is_phantom
            && binding_rows.iter().any(|row| {
                row.try_get::<String, _>("lifecycle_state")
                    .is_ok_and(|state| matches!(state.as_str(), "active" | "standby" | "retiring"))
            })
        {
            return Err(OrchestratorError::StoreInvariant(
                "phantom ALT repair refuses a table with live/rollback bindings".to_owned(),
            ));
        }

        let repaired_operation_ids = sqlx::query_scalar::<_, i64>(
            r#"
            SELECT repaired.operation_id
            FROM loyal_yield.lookup_table_terminal_repair_operations repaired
            JOIN loyal_yield.lookup_table_operations operation
              ON operation.id = repaired.operation_id
            WHERE operation.route_lookup_table_id = $1
            "#,
        )
        .bind(table.id)
        .fetch_all(&mut *tx)
        .await?
        .into_iter()
        .collect::<BTreeSet<_>>();
        let mut sibling_evidence = BTreeMap::new();
        for evidence in &input.sibling_no_effect {
            if evidence.operation_id <= 0
                || evidence.operation_id == operation.id
                || sibling_evidence
                    .insert(evidence.operation_id, evidence.no_effect.clone())
                    .is_some()
            {
                return Err(OrchestratorError::StoreInvariant(
                    "terminal ALT sibling evidence is duplicated or targets the repair root"
                        .to_owned(),
                ));
            }
        }
        let mut superseded_operation_evidence = Vec::new();
        for dependency in &table_operations {
            if dependency.id == operation.id {
                continue;
            }
            // Append-only repair lineage makes terminal ancestors historical,
            // not blockers. If a successor later fails, a new repair may use
            // it as the root without reopening or re-auditing any ancestor.
            if repaired_operation_ids.contains(&dependency.id) {
                continue;
            }
            match dependency.operation_state {
                LookupTableOperationStatus::Complete | LookupTableOperationStatus::Cancelled => {}
                LookupTableOperationStatus::PermanentFailure
                    if dependency.operation_kind == LookupTableOperationKind::Extend =>
                {
                    let evidence = sibling_evidence.remove(&dependency.id).ok_or_else(|| {
                        OrchestratorError::StoreInvariant(format!(
                            "unresolved terminal ALT sibling {} lacks individual no-effect evidence",
                            dependency.id
                        ))
                    })?;
                    let audit = validate_lookup_table_terminal_no_effect(
                        dependency,
                        &evidence,
                        input.chain.observed_slot,
                    )?;
                    superseded_operation_evidence.push((dependency.id, audit));
                }
                LookupTableOperationStatus::Queued | LookupTableOperationStatus::RetryWait
                    if is_phantom
                        && dependency.transaction_signature.is_none()
                        && dependency.message_hash.is_none()
                        && dependency.recent_blockhash.is_none() =>
                {
                    sqlx::query(
                        r#"
                        UPDATE loyal_yield.lookup_table_operations
                        SET operation_state = 'cancelled',
                            error_code = 'phantom_table_quarantined',
                            error_detail = 'superseded by fenced terminal ALT repair',
                            lease_owner = NULL,
                            lease_expires_at = NULL,
                            next_attempt_at = NULL,
                            updated_at = now()
                        WHERE id = $1 AND operation_state IN ('queued', 'retry_wait')
                        "#,
                    )
                    .bind(dependency.id)
                    .execute(&mut *tx)
                    .await?;
                    superseded_operation_evidence.push((
                        dependency.id,
                        LookupTableTerminalNoEffectAudit {
                            evidence: "unsigned",
                            signature: None,
                            signature_slot: None,
                        },
                    ));
                }
                _ => {
                    return Err(OrchestratorError::StoreInvariant(format!(
                        "terminal ALT repair found unresolved operation {} in state {}",
                        dependency.id, dependency.operation_state
                    )));
                }
            }
        }
        if !sibling_evidence.is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "terminal ALT sibling evidence references no unresolved same-table dependency"
                    .to_owned(),
            ));
        }
        let superseded_operation_ids = superseded_operation_evidence
            .iter()
            .map(|(operation_id, _)| *operation_id)
            .collect::<Vec<_>>();

        let operation_addresses = sqlx::query_scalar::<_, String>(
            r#"
            SELECT address
            FROM loyal_yield.lookup_table_operation_addresses
            WHERE operation_id = $1
            ORDER BY ordinal
            "#,
        )
        .bind(operation.id)
        .fetch_all(&mut *tx)
        .await?;
        if !is_phantom {
            let persisted = persisted_addresses.iter().collect::<BTreeSet<_>>();
            let mut suffix = BTreeSet::new();
            if operation_addresses.is_empty()
                || operation_addresses.iter().any(|address| {
                    Pubkey::from_str(address).is_err()
                        || persisted.contains(address)
                        || !suffix.insert(address)
                })
                || persisted_addresses
                    .len()
                    .saturating_add(operation_addresses.len())
                    > usize::from(LOOKUP_TABLE_HARD_CAPACITY)
            {
                return Err(OrchestratorError::StoreInvariant(
                    "failed ALT suffix is empty, malformed, duplicated, or exceeds capacity"
                        .to_owned(),
                ));
            }
        }
        let attempt_generation: i64 = operation_row.try_get("attempt_generation")?;
        let mut successor_operation_id = None;
        if !is_phantom {
            let next_generation = attempt_generation.checked_add(1).ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "lookup-table operation attempt generation overflow".to_owned(),
                )
            })?;
            let successor_key = terminal_operation_successor_idempotency_key(
                &operation.idempotency_key,
                operation.id,
                next_generation,
            );
            let mut successor_context = operation.operation_context.clone();
            let context = successor_context.as_object_mut().ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "lookup-table operation context must be an object".to_owned(),
                )
            })?;
            context.insert(
                "terminalRepairPredecessorId".to_owned(),
                Value::from(operation.id),
            );
            context.insert(
                "terminalRepairAttemptGeneration".to_owned(),
                Value::from(next_generation),
            );
            let successor_id: i64 = sqlx::query_scalar(
                r#"
                INSERT INTO loyal_yield.lookup_table_operations
                    (idempotency_key, family_id, route_lookup_table_id, manifest_id,
                     binding_id, operation_kind, operation_state, target_generation,
                     target_shard_ordinal, operation_context, mutation_epoch,
                     estimated_fee_lamports, estimated_rent_lamports,
                     attempt_generation, retry_of_operation_id)
                VALUES ($1, $2, $3, $4, $5, $6, 'queued', $7, $8, $9, $10,
                        $11, $12, $13, $14)
                RETURNING id
                "#,
            )
            .bind(successor_key)
            .bind(operation.family_id)
            .bind(operation.route_lookup_table_id)
            .bind(operation.manifest_id)
            .bind(operation.binding_id)
            .bind(operation.operation_kind.as_str())
            .bind(operation.target_generation)
            .bind(operation.target_shard_ordinal)
            .bind(successor_context)
            .bind(operation.mutation_epoch)
            .bind(operation.estimated_fee_lamports)
            .bind(operation.estimated_rent_lamports)
            .bind(next_generation)
            .bind(operation.id)
            .fetch_one(&mut *tx)
            .await?;
            if !operation_addresses.is_empty() {
                let mut query = QueryBuilder::<Postgres>::new(
                    "INSERT INTO loyal_yield.lookup_table_operation_addresses (operation_id, address, ordinal) ",
                );
                query.push_values(
                    operation_addresses.iter().enumerate(),
                    |mut row, (ordinal, address)| {
                        row.push_bind(successor_id)
                            .push_bind(address)
                            .push_bind(i32::try_from(ordinal).unwrap_or(i32::MAX));
                    },
                );
                query.build().execute(&mut *tx).await?;
            }
            successor_operation_id = Some(successor_id);
        }

        let manifest_ids = binding_rows
            .iter()
            .map(|row| row.try_get::<i64, _>("manifest_id"))
            .collect::<Result<Vec<_>, _>>()?;
        let mut affected_operation_ids = vec![operation.id];
        affected_operation_ids.extend(superseded_operation_ids.iter().copied());
        let mut request_ids = sqlx::query_scalar::<_, i64>(
            r#"
            SELECT DISTINCT (operation_context->>'request_id')::BIGINT
            FROM loyal_yield.lookup_table_operations
            WHERE id = ANY($1)
              AND operation_context->>'request_id' ~ '^[0-9]{1,18}$'
            "#,
        )
        .bind(&affected_operation_ids)
        .fetch_all(&mut *tx)
        .await?;
        if is_phantom && !manifest_ids.is_empty() {
            request_ids.extend(
                sqlx::query_scalar::<_, i64>(
                    r#"
                    SELECT id
                    FROM loyal_yield.lookup_table_provisioning_requests
                    WHERE vault_manifest_id = ANY($1)
                      AND request_status NOT IN ('satisfied', 'cancelled')
                    "#,
                )
                .bind(&manifest_ids)
                .fetch_all(&mut *tx)
                .await?,
            );
        }
        request_ids.sort_unstable();
        request_ids.dedup();
        // Always re-enter through the normal planner lease. For a suffix, the
        // planner observes the queued immutable successor; for a phantom it
        // allocates the request onto a healthy packed shard (or a fresh shard
        // only when no existing shard fits).
        let next_request_state = "requested";
        let requeued_request_ids = if request_ids.is_empty() {
            Vec::new()
        } else {
            sqlx::query_scalar::<_, i64>(
                r#"
                UPDATE loyal_yield.lookup_table_provisioning_requests
                SET request_status = $2,
                    lease_owner = NULL,
                    lease_expires_at = NULL,
                    fencing_token = fencing_token + 1,
                    attempt_count = 0,
                    next_attempt_at = now(),
                    error_code = NULL,
                    error_detail = NULL,
                    satisfied_at = NULL,
                    updated_at = now()
                WHERE id = ANY($1)
                  AND request_status NOT IN ('satisfied', 'cancelled')
                RETURNING id
                "#,
            )
            .bind(&request_ids)
            .bind(next_request_state)
            .fetch_all(&mut *tx)
            .await?
        };

        let failed_binding_ids = if is_phantom {
            sqlx::query_scalar::<_, i64>(
                r#"
                UPDATE loyal_yield.lookup_table_vault_bindings
                SET lifecycle_state = 'failed', updated_at = now()
                WHERE route_lookup_table_id = $1
                  AND lifecycle_state IN ('preparing', 'warming')
                RETURNING id
                "#,
            )
            .bind(table.id)
            .fetch_all(&mut *tx)
            .await?
        } else {
            Vec::new()
        };
        if is_phantom {
            sqlx::query(
                r#"
                UPDATE loyal_yield.route_lookup_tables
                SET desired_state = 'failed',
                    accepting_allocations = FALSE,
                    status = 'failed',
                    notes = concat_ws(E'\n', NULLIF(notes, ''), $2),
                    updated_at = now()
                WHERE id = $1
                "#,
            )
            .bind(table.id)
            .bind(format!(
                "fenced terminal repair quarantined phantom operation {}",
                operation.id
            ))
            .execute(&mut *tx)
            .await?;
        }

        let finalized_address_hash = ordered_address_hash(&input.chain.ordered_addresses);
        let repair_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.lookup_table_terminal_repairs
                (cluster, repair_kind, route_lookup_table_id, root_operation_id,
                 successor_operation_id, expected_control_epoch,
                 expected_mutation_epoch, finalized_observed_slot,
                 finalized_account_state, finalized_account_owner,
                 finalized_authority, finalized_last_extended_slot,
                 finalized_address_hash,
                 finalized_address_count, no_effect_evidence,
                 no_effect_signature, no_effect_signature_slot, reason, updated_by)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
                    $13, $14, $15, $16, $17, $18, $19)
            RETURNING id
            "#,
        )
        .bind(&input.cluster)
        .bind(repair_kind)
        .bind(table.id)
        .bind(operation.id)
        .bind(successor_operation_id)
        .bind(input.expected_control_epoch)
        .bind(table.mutation_epoch)
        .bind(input.chain.observed_slot)
        .bind(input.chain.account_state.as_str())
        .bind(&input.chain.account_owner)
        .bind(&input.chain.authority)
        .bind(input.chain.last_extended_slot)
        .bind(finalized_address_hash)
        .bind(
            i32::try_from(input.chain.ordered_addresses.len()).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "finalized terminal repair address count overflow".to_owned(),
                )
            })?,
        )
        .bind(root_no_effect.evidence)
        .bind(root_no_effect.signature)
        .bind(root_no_effect.signature_slot)
        .bind(input.reason.chars().take(500).collect::<String>())
        .bind(input.updated_by.chars().take(128).collect::<String>())
        .fetch_one(&mut *tx)
        .await?;
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_terminal_repair_operations
                (repair_id, operation_id, disposition)
            VALUES ($1, $2, 'root')
            "#,
        )
        .bind(repair_id)
        .bind(operation.id)
        .execute(&mut *tx)
        .await?;
        for (operation_id, evidence) in &superseded_operation_evidence {
            sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_terminal_repair_operations
                    (repair_id, operation_id, disposition, no_effect_evidence,
                     no_effect_signature, no_effect_signature_slot)
                VALUES ($1, $2, 'superseded_dependency', $3, $4, $5)
                "#,
            )
            .bind(repair_id)
            .bind(operation_id)
            .bind(evidence.evidence)
            .bind(&evidence.signature)
            .bind(evidence.signature_slot)
            .execute(&mut *tx)
            .await?;
        }
        for request_id in &requeued_request_ids {
            sqlx::query(
                "INSERT INTO loyal_yield.lookup_table_terminal_repair_requests (repair_id, request_id) VALUES ($1, $2)",
            )
            .bind(repair_id)
            .bind(request_id)
            .execute(&mut *tx)
            .await?;
        }
        for binding_id in &failed_binding_ids {
            sqlx::query(
                "INSERT INTO loyal_yield.lookup_table_terminal_repair_bindings (repair_id, binding_id) VALUES ($1, $2)",
            )
            .bind(repair_id)
            .bind(binding_id)
            .execute(&mut *tx)
            .await?;
        }
        tx.commit().await?;
        Ok(LookupTableTerminalRepairResult {
            repair_id,
            repair_kind: repair_kind.to_owned(),
            root_operation_id: operation.id,
            route_lookup_table_id: table.id,
            successor_operation_id,
            superseded_operation_ids,
            failed_binding_ids,
            requeued_request_ids,
        })
    }

    pub async fn lease_next_lookup_table_operation(
        &self,
        cluster: &str,
        owner: &str,
        lease_expires_at: DateTime<Utc>,
        reconcile_only: bool,
    ) -> Result<Option<LeasedLookupTableOperation>, OrchestratorError> {
        let row = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT operation.id
                FROM loyal_yield.lookup_table_operations operation
                JOIN loyal_yield.lookup_table_families family ON family.id = operation.family_id
                LEFT JOIN loyal_yield.lookup_table_provisioning_requests priority_request
                  ON priority_request.id = CASE
                      WHEN operation.operation_context->>'request_id' ~ '^[0-9]{1,18}$'
                          THEN (operation.operation_context->>'request_id')::BIGINT
                      ELSE NULL
                  END
                LEFT JOIN LATERAL (
                    SELECT
                        COALESCE(sum(opportunity.annual_yield_gain_usd_micros), 0)::NUMERIC
                            AS aggregate_annual_yield,
                        COALESCE(sum(opportunity.economic_priority), 0)::NUMERIC
                            AS aggregate_priority,
                        count(*)::BIGINT AS consumer_count
                    FROM loyal_yield.lookup_table_provisioning_request_consumers consumer
                    JOIN loyal_yield.rebalance_opportunities opportunity
                      ON opportunity.id = consumer.opportunity_id
                    WHERE consumer.provisioning_request_id = priority_request.id
                      AND opportunity.opportunity_state = 'waiting_alt'
                      AND opportunity.expires_at > now()
                ) live_priority ON priority_request.id IS NOT NULL
                WHERE family.cluster = $1
                  AND operation.operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
                  AND (
                      NOT $4 OR operation.operation_state IN (
                          'signed', 'submitted', 'confirmed', 'finalized',
                          'reconciled', 'needs_reconcile'
                      ) OR (
                          operation.operation_state = 'leased'
                          AND operation.transaction_signature IS NOT NULL
                      ) OR (
                          operation.operation_kind = 'verify'
                          AND operation.operation_state IN ('queued', 'retry_wait', 'leased')
                      )
                  )
                  AND (
                      operation.lease_expires_at IS NULL
                      OR operation.lease_expires_at <= now()
                  )
                  AND (
                      operation.next_attempt_at IS NULL
                      OR operation.next_attempt_at <= now()
                  )
                  AND (
                      $4
                      OR operation.operation_kind = 'verify'
                      OR operation.transaction_signature IS NOT NULL
                      OR operation.route_lookup_table_id IS NULL
                      OR NOT EXISTS (
                          SELECT 1
                          FROM loyal_yield.lookup_table_usage_leases usage
                          WHERE usage.route_lookup_table_id = operation.route_lookup_table_id
                            AND usage.released_at IS NULL
                            AND usage.expires_at > now()
                      )
                  )
                  AND (
                      operation.route_lookup_table_id IS NULL
                      OR NOT EXISTS (
                          SELECT 1
                          FROM loyal_yield.lookup_table_operations predecessor
                          WHERE predecessor.route_lookup_table_id = operation.route_lookup_table_id
                            AND predecessor.id <> operation.id
                            AND predecessor.operation_state NOT IN (
                                'complete', 'permanent_failure', 'cancelled'
                            )
                            AND (predecessor.created_at, predecessor.id)
                                < (operation.created_at, operation.id)
                      )
                  )
                ORDER BY
                    CASE operation.operation_state
                        WHEN 'needs_reconcile' THEN 0
                        WHEN 'signed' THEN 1
                        WHEN 'submitted' THEN 2
                        WHEN 'confirmed' THEN 3
                        WHEN 'finalized' THEN 4
                        WHEN 'reconciled' THEN 5
                        WHEN 'leased' THEN 6
                        WHEN 'retry_wait' THEN 7
                        ELSE 8
                    END,
                    -- Shared-market mutations unlock every dependent route,
                    -- so they are the fleet-wide prerequisite ahead of the
                    -- per-request economic order used for vault shards.
                    CASE WHEN family.kind = 'shared_market' THEN 0 ELSE 1 END,
                    COALESCE(live_priority.aggregate_annual_yield, 0)
                        / GREATEST(
                            1,
                            COALESCE(priority_request.desired_shared_address_count, 0)
                                + COALESCE(priority_request.desired_vault_address_count, 0)
                        ) DESC,
                    COALESCE(live_priority.aggregate_priority, 0) DESC,
                    COALESCE(live_priority.consumer_count, 0) DESC,
                    operation.created_at,
                    operation.id
                FOR UPDATE OF operation SKIP LOCKED
                LIMIT 1
            )
            UPDATE loyal_yield.lookup_table_operations operation
            SET operation_state = CASE
                    WHEN operation.operation_state IN ('queued', 'retry_wait', 'leased')
                        THEN 'leased'
                    ELSE operation.operation_state
                END,
                lease_owner = $2,
                lease_expires_at = $3,
                fencing_token = operation.fencing_token + 1,
                attempt_count = operation.attempt_count + 1,
                updated_at = now()
            FROM candidate
            WHERE operation.id = candidate.id
            RETURNING operation.*
            "#,
        )
        .bind(cluster)
        .bind(owner)
        .bind(lease_expires_at)
        .bind(reconcile_only)
        .fetch_optional(self.pool())
        .await?;
        let Some(row) = row else {
            return Ok(None);
        };
        let operation = lookup_table_operation_from_row(&row)?;
        let addresses = sqlx::query_scalar::<_, String>(
            "SELECT address FROM loyal_yield.lookup_table_operation_addresses WHERE operation_id = $1 ORDER BY ordinal",
        )
        .bind(operation.id)
        .fetch_all(self.pool())
        .await?;
        let physical_table = match operation.route_lookup_table_id {
            Some(table_id) => self.reusable_lookup_table(table_id).await?,
            None => None,
        };
        let persisted_membership = match operation.route_lookup_table_id {
            Some(table_id) => self.lookup_table_membership(table_id).await?,
            None => Vec::new(),
        };
        Ok(Some(LeasedLookupTableOperation {
            operation,
            addresses,
            physical_table,
            persisted_membership,
        }))
    }

    pub async fn lookup_table_membership(
        &self,
        table_id: i64,
    ) -> Result<Vec<LookupTableMembershipAddress>, OrchestratorError> {
        let rows = sqlx::query(
            r#"
            SELECT address, ordinal, added_operation_id, added_slot,
                   usable_after_slot, last_verified_slot, last_verified_at
            FROM loyal_yield.lookup_table_addresses
            WHERE route_lookup_table_id = $1 ORDER BY ordinal
            "#,
        )
        .bind(table_id)
        .fetch_all(self.pool())
        .await?;
        rows.iter()
            .map(|row| {
                Ok(LookupTableMembershipAddress {
                    address: row.try_get("address")?,
                    ordinal: row.try_get("ordinal")?,
                    added_operation_id: row.try_get("added_operation_id")?,
                    added_slot: row.try_get("added_slot")?,
                    usable_after_slot: row.try_get("usable_after_slot")?,
                    last_verified_slot: row.try_get("last_verified_slot")?,
                    last_verified_at: row.try_get("last_verified_at")?,
                })
            })
            .collect()
    }

    /// Rekeys an unsigned create/rollover reservation after its persisted
    /// recent slot has aged out of SlotHashes. IDs and binding references stay
    /// stable; only the derived table address and durable recent-slot context
    /// change under the operation fence.
    pub async fn refresh_leased_lookup_table_create_reservation(
        &self,
        operation_id: i64,
        lease: &LookupTableOperationLease,
        fresh_finalized_slot: u64,
    ) -> Result<LeasedLookupTableOperation, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        let operation_row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_operations
            WHERE id = $1 AND operation_state = 'leased'
              AND lease_owner = $2 AND fencing_token = $3
              AND lease_expires_at > now()
            FOR UPDATE
            "#,
        )
        .bind(operation_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        let operation = lookup_table_operation_from_row(&operation_row)?;
        if !matches!(
            operation.operation_kind,
            LookupTableOperationKind::Create | LookupTableOperationKind::Rollover
        ) || operation.transaction_signature.is_some()
            || operation.message_hash.is_some()
            || operation.recent_blockhash.is_some()
            || operation.last_valid_block_height.is_some()
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table operation {operation_id} is not an unsigned leased create/rollover"
            )));
        }
        let table_id = operation.route_lookup_table_id.ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "lookup-table operation {operation_id} has no pre-reserved physical table"
            ))
        })?;
        let table_row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.route_lookup_tables
            WHERE id = $1 AND family_id = $2
            FOR UPDATE
            "#,
        )
        .bind(table_id)
        .bind(operation.family_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("reusable lookup table", table_id))?;
        let create_signature: Option<String> = table_row.try_get("create_signature")?;
        let table = reusable_lookup_table_from_row(&table_row)?;
        let membership_count: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id = $1",
        )
        .bind(table_id)
        .fetch_one(&mut *tx)
        .await?;
        if !matches!(
            table.desired_state,
            LookupTableLifecycle::Preparing | LookupTableLifecycle::Warming
        ) || table.address_count != 0
            || table.usable_address_count != 0
            || table.mutation_epoch != operation.mutation_epoch
            || create_signature.is_some()
            || membership_count != 0
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table operation {operation_id} physical reservation is no longer empty and rekeyable"
            )));
        }

        sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")
            .bind(&table.authority)
            .execute(&mut *tx)
            .await?;
        // Include the current address so refresh always produces a new key.
        let occupied_table_addresses = sqlx::query_scalar::<_, String>(
            "SELECT table_address FROM loyal_yield.route_lookup_tables WHERE authority = $1",
        )
        .bind(&table.authority)
        .fetch_all(&mut *tx)
        .await?
        .into_iter()
        .collect::<BTreeSet<_>>();
        let mut operation_context = operation.operation_context.clone();
        let context = operation_context.as_object_mut().ok_or_else(|| {
            OrchestratorError::StoreInvariant("operation context must be a JSON object".to_owned())
        })?;
        context.insert("recent_slot".to_owned(), Value::from(fresh_finalized_slot));
        context.remove("recentSlot");
        let new_table_address = reserve_derived_lookup_table_address(
            &table.authority,
            &mut operation_context,
            &occupied_table_addresses,
        )?;
        let updated_table_row = sqlx::query(
            r#"
            UPDATE loyal_yield.route_lookup_tables
            SET table_address = $3, updated_at = now()
            WHERE id = $1 AND table_address = $2
              AND desired_state IN ('preparing', 'warming')
              AND address_count = 0 AND usable_address_count = 0
              AND mutation_epoch = $4 AND create_signature IS NULL
            RETURNING *
            "#,
        )
        .bind(table_id)
        .bind(&table.table_address)
        .bind(&new_table_address)
        .bind(operation.mutation_epoch)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("rekeyable lookup table", table_id))?;
        let updated_table = reusable_lookup_table_from_row(&updated_table_row)?;
        let updated_operation_row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_operations
            SET operation_context = $4, updated_at = now()
            WHERE id = $1 AND operation_state = 'leased'
              AND lease_owner = $2 AND fencing_token = $3
              AND lease_expires_at > now()
              AND transaction_signature IS NULL AND message_hash IS NULL
              AND recent_blockhash IS NULL AND last_valid_block_height IS NULL
            RETURNING *
            "#,
        )
        .bind(operation_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(&operation_context)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        let updated_operation = lookup_table_operation_from_row(&updated_operation_row)?;
        let addresses = sqlx::query_scalar::<_, String>(
            "SELECT address FROM loyal_yield.lookup_table_operation_addresses WHERE operation_id = $1 ORDER BY ordinal",
        )
        .bind(operation_id)
        .fetch_all(&mut *tx)
        .await?;
        let persisted_membership = sqlx::query(
            r#"
            SELECT address, ordinal, added_operation_id, added_slot,
                   usable_after_slot, last_verified_slot, last_verified_at
            FROM loyal_yield.lookup_table_addresses
            WHERE route_lookup_table_id = $1 ORDER BY ordinal
            "#,
        )
        .bind(table_id)
        .fetch_all(&mut *tx)
        .await?
        .iter()
        .map(|row| {
            Ok(LookupTableMembershipAddress {
                address: row.try_get("address")?,
                ordinal: row.try_get("ordinal")?,
                added_operation_id: row.try_get("added_operation_id")?,
                added_slot: row.try_get("added_slot")?,
                usable_after_slot: row.try_get("usable_after_slot")?,
                last_verified_slot: row.try_get("last_verified_slot")?,
                last_verified_at: row.try_get("last_verified_at")?,
            })
        })
        .collect::<Result<Vec<_>, OrchestratorError>>()?;
        tx.commit().await?;
        Ok(LeasedLookupTableOperation {
            operation: updated_operation,
            addresses,
            physical_table: Some(updated_table),
            persisted_membership,
        })
    }

    /// Atomically reserves a cluster-wide rolling-window budget for one
    /// simulated provisioning attempt. The `(operation_id, fencing_token)`
    /// identity makes retries in the same lease idempotent while a new lease
    /// conservatively reserves a new attempt. No process-local counter is used.
    pub async fn reserve_lookup_table_cluster_budget(
        &self,
        cluster: &str,
        operation_id: i64,
        lease: &LookupTableOperationLease,
        policy: LookupTableClusterBudgetPolicy,
        estimated_fee_lamports: i64,
        estimated_rent_lamports: i64,
    ) -> Result<LookupTableClusterBudgetReservation, OrchestratorError> {
        if cluster.trim().is_empty()
            || policy.max_lamports <= 0
            || !(1..=31_536_000).contains(&policy.rolling_window_seconds)
            || estimated_fee_lamports < 0
            || estimated_rent_lamports < 0
        {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table cluster budget requires positive limit/window and nonnegative simulated accounting"
                    .to_owned(),
            ));
        }
        let requested_lamports = estimated_fee_lamports
            .checked_add(estimated_rent_lamports)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "lookup-table cluster budget request overflowed lamports".to_owned(),
                )
            })?;
        let mut tx = self.pool().begin().await?;
        sqlx::query(
            "SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-budget:' || $1, 0))",
        )
        .bind(cluster)
        .execute(&mut *tx)
        .await?;
        let now: DateTime<Utc> = sqlx::query_scalar("SELECT clock_timestamp()")
            .fetch_one(&mut *tx)
            .await?;
        let operation_row = sqlx::query(
            r#"
            SELECT operation.*, family.cluster AS family_cluster
            FROM loyal_yield.lookup_table_operations operation
            JOIN loyal_yield.lookup_table_families family
              ON family.id = operation.family_id
            WHERE operation.id = $1
            FOR UPDATE OF operation
            "#,
        )
        .bind(operation_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("lookup-table operation", operation_id))?;
        let operation = lookup_table_operation_from_row(&operation_row)?;
        if operation_row.try_get::<String, _>("family_cluster")? != cluster
            || operation.operation_state != LookupTableOperationStatus::Leased
            || !matches!(
                operation.operation_kind,
                LookupTableOperationKind::Create
                    | LookupTableOperationKind::Extend
                    | LookupTableOperationKind::Rollover
                    | LookupTableOperationKind::Deactivate
                    | LookupTableOperationKind::Close
            )
            || operation.lease_owner.as_deref() != Some(lease.owner.as_str())
            || operation.fencing_token != lease.fencing_token
            || operation.lease_expires_at.is_none_or(|until| until <= now)
        {
            return Err(stale_fenced_operation(operation_id));
        }

        let existing = sqlx::query(
            r#"
            SELECT id, lease_owner, estimated_fee_lamports,
                   estimated_rent_lamports, reserved_lamports, reserved_until
            FROM loyal_yield.lookup_table_cluster_budget_reservations
            WHERE operation_id = $1 AND fencing_token = $2
            "#,
        )
        .bind(operation_id)
        .bind(lease.fencing_token)
        .fetch_optional(&mut *tx)
        .await?;
        if let Some(existing) = existing {
            if existing.try_get::<String, _>("lease_owner")? != lease.owner
                || existing.try_get::<i64, _>("estimated_fee_lamports")? != estimated_fee_lamports
                || existing.try_get::<i64, _>("estimated_rent_lamports")? != estimated_rent_lamports
                || existing.try_get::<i64, _>("reserved_lamports")? != requested_lamports
            {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "lookup-table operation {operation_id} budget fence was replayed with different accounting"
                )));
            }
            let usage = load_cluster_budget_usage_in_connection(
                &mut tx,
                cluster,
                "operation",
                operation_id,
                now,
            )
            .await?;
            let window_ends_at: DateTime<Utc> = existing.try_get("reserved_until")?;
            let result = LookupTableClusterBudgetReservation {
                approved: true,
                replayed: true,
                reservation_id: Some(existing.try_get("id")?),
                cluster: cluster.to_owned(),
                operation_id,
                fencing_token: lease.fencing_token,
                estimated_fee_lamports,
                estimated_rent_lamports,
                requested_lamports,
                spent_lamports: usage.spent_lamports,
                reserved_lamports: usage.reserved_lamports,
                charged_lamports: usage.charged_lamports,
                remaining_lamports: policy.max_lamports.saturating_sub(usage.charged_lamports),
                window_ends_at,
            };
            tx.commit().await?;
            return Ok(result);
        }

        let usage = load_cluster_budget_usage_in_connection(
            &mut tx,
            cluster,
            "operation",
            operation_id,
            now,
        )
        .await?;
        let current_operation_charge = usage
            .subject_reserved_lamports
            .max(usage.subject_actual_lamports);
        let prospective_operation_charge = usage
            .subject_reserved_lamports
            .checked_add(requested_lamports)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "lookup-table cluster budget operation reservation overflowed".to_owned(),
                )
            })?
            .max(usage.subject_actual_lamports);
        let prospective_charge = usage
            .charged_lamports
            .checked_add(prospective_operation_charge - current_operation_charge)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "lookup-table cluster budget total overflowed".to_owned(),
                )
            })?;
        let requested_window_end: DateTime<Utc> = sqlx::query_scalar(
            "SELECT $1::timestamptz + ($2::double precision * interval '1 second')",
        )
        .bind(now)
        .bind(policy.rolling_window_seconds)
        .fetch_one(&mut *tx)
        .await?;
        if prospective_charge > policy.max_lamports {
            let result = LookupTableClusterBudgetReservation {
                approved: false,
                replayed: false,
                reservation_id: None,
                cluster: cluster.to_owned(),
                operation_id,
                fencing_token: lease.fencing_token,
                estimated_fee_lamports,
                estimated_rent_lamports,
                requested_lamports,
                spent_lamports: usage.spent_lamports,
                reserved_lamports: usage.reserved_lamports,
                charged_lamports: usage.charged_lamports,
                remaining_lamports: policy.max_lamports.saturating_sub(usage.charged_lamports),
                window_ends_at: usage.window_ends_at.unwrap_or(requested_window_end),
            };
            tx.commit().await?;
            return Ok(result);
        }
        let reservation_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.lookup_table_cluster_budget_reservations
                (cluster, operation_id, fencing_token, lease_owner,
                 estimated_fee_lamports, estimated_rent_lamports,
                 reserved_lamports, reserved_at, reserved_until)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
            RETURNING id
            "#,
        )
        .bind(cluster)
        .bind(operation_id)
        .bind(lease.fencing_token)
        .bind(&lease.owner)
        .bind(estimated_fee_lamports)
        .bind(estimated_rent_lamports)
        .bind(requested_lamports)
        .bind(now)
        .bind(requested_window_end)
        .fetch_one(&mut *tx)
        .await?;
        let usage = load_cluster_budget_usage_in_connection(
            &mut tx,
            cluster,
            "operation",
            operation_id,
            now,
        )
        .await?;
        let result = LookupTableClusterBudgetReservation {
            approved: true,
            replayed: false,
            reservation_id: Some(reservation_id),
            cluster: cluster.to_owned(),
            operation_id,
            fencing_token: lease.fencing_token,
            estimated_fee_lamports,
            estimated_rent_lamports,
            requested_lamports,
            spent_lamports: usage.spent_lamports,
            reserved_lamports: usage.reserved_lamports,
            charged_lamports: usage.charged_lamports,
            remaining_lamports: policy.max_lamports.saturating_sub(usage.charged_lamports),
            window_ends_at: usage.window_ends_at.unwrap_or(requested_window_end),
        };
        tx.commit().await?;
        Ok(result)
    }

    pub async fn persist_signed_lookup_table_transaction(
        &self,
        operation_id: i64,
        lease: &LookupTableOperationLease,
        signed: SignedLookupTableTransaction,
    ) -> Result<LookupTableOperationRecord, OrchestratorError> {
        if signed.last_valid_block_height < 0
            || signed.estimated_fee_lamports < 0
            || signed.estimated_rent_lamports < 0
            || signed.estimated_reclaimed_rent_lamports < 0
        {
            return Err(OrchestratorError::StoreInvariant(
                "signed lookup-table transaction accounting must be nonnegative".to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_operations
            SET operation_state = 'signed',
                transaction_signature = $4,
                message_hash = $5,
                recent_blockhash = $6,
                last_valid_block_height = $7,
                estimated_fee_lamports = $8,
                estimated_rent_lamports = $9,
                operation_context = jsonb_set(
                    operation_context,
                    '{signedExpectedReclaimedRentLamports}',
                    to_jsonb($10::BIGINT),
                    TRUE
                ),
                error_code = NULL,
                error_detail = NULL,
                updated_at = now()
            WHERE id = $1
              AND operation_state = 'leased'
              AND lease_owner = $2
              AND fencing_token = $3
              AND lease_expires_at > now()
            RETURNING *
            "#,
        )
        .bind(operation_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(signed.transaction_signature)
        .bind(signed.message_hash)
        .bind(signed.recent_blockhash)
        .bind(signed.last_valid_block_height)
        .bind(signed.estimated_fee_lamports)
        .bind(signed.estimated_rent_lamports)
        .bind(signed.estimated_reclaimed_rent_lamports)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        lookup_table_operation_from_row(&row)
    }

    async fn validate_cleanup_enqueue_in_tx(
        tx: &mut sqlx::PgConnection,
        input: &LookupTableOperationEnqueue,
    ) -> Result<(), OrchestratorError> {
        if !matches!(
            input.operation_kind,
            LookupTableOperationKind::Deactivate | LookupTableOperationKind::Close
        ) {
            return Ok(());
        }
        let table_id = input.route_lookup_table_id.ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "cleanup operation requires a registered physical lookup table".to_owned(),
            )
        })?;

        // Family first, then physical table: generation activation uses the same
        // lock order. Usage-lease acquisition locks the physical row, so either a
        // route lease or cleanup intent wins atomically.
        let family_row =
            sqlx::query("SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 FOR SHARE")
                .bind(input.family_id)
                .fetch_optional(&mut *tx)
                .await?
                .ok_or_else(|| stale_store_update("lookup-table family", input.family_id))?;
        let family = lookup_table_family_from_row(&family_row)?;
        let table_row = sqlx::query(
            r#"
        SELECT * FROM loyal_yield.route_lookup_tables
        WHERE id = $1 AND family_id = $2
        FOR UPDATE
        "#,
        )
        .bind(table_id)
        .bind(input.family_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("reusable lookup table", table_id))?;
        let table = reusable_lookup_table_from_row(&table_row)?;

        let context_string = |field: &str| {
            input
                .operation_context
                .get(field)
                .and_then(Value::as_str)
                .ok_or_else(|| {
                    OrchestratorError::StoreInvariant(format!(
                        "cleanup operation context lacks {field}"
                    ))
                })
        };
        let expected_authority = context_string("expectedAuthority")?;
        let expected_hash = context_string("expectedAddressHash")?;
        let expected_epoch = input
            .operation_context
            .get("expectedMutationEpoch")
            .and_then(Value::as_i64)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "cleanup operation context lacks expectedMutationEpoch".to_owned(),
                )
            })?;
        let expected_address_count = input
            .operation_context
            .get("expectedAddressCount")
            .and_then(Value::as_i64)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "cleanup operation context lacks expectedAddressCount".to_owned(),
                )
            })?;
        if input.mutation_epoch != expected_epoch
            || table.mutation_epoch != expected_epoch
            || table.authority != expected_authority
            || table.address_hash != expected_hash
            || i64::from(table.address_count) != expected_address_count
            || input
                .operation_context
                .get("cluster")
                .and_then(Value::as_str)
                .is_some_and(|cluster| cluster != table.cluster)
            || input
                .operation_context
                .get("table")
                .and_then(Value::as_str)
                .is_some_and(|address| address != table.table_address)
        {
            return Err(OrchestratorError::StoreInvariant(
                "cleanup operation metadata changed after preview".to_owned(),
            ));
        }

        let now = Utc::now();
        let lifecycle_allowed = match input.operation_kind {
            LookupTableOperationKind::Deactivate => matches!(
                table.desired_state,
                LookupTableLifecycle::Active
                    | LookupTableLifecycle::Standby
                    | LookupTableLifecycle::Retiring
            ),
            LookupTableOperationKind::Close => {
                table.desired_state == LookupTableLifecycle::Deactivated
            }
            _ => unreachable!(),
        };
        let rollback_active = table.rollback_until.is_some_and(|until| until > now)
            || family.rollback_until.is_some_and(|until| until > now);
        let has_live_binding: bool = sqlx::query_scalar(
            r#"
        SELECT EXISTS (
            SELECT 1 FROM loyal_yield.lookup_table_vault_bindings
            WHERE route_lookup_table_id = $1
              AND lifecycle_state IN ('preparing', 'warming', 'active', 'standby', 'retiring')
        )
        "#,
        )
        .bind(table_id)
        .fetch_one(&mut *tx)
        .await?;
        let safe_retiring_current_vault_shard = family.active_generation == Some(table.generation)
            && matches!(
                table.allocation_kind,
                LookupTableAllocationKind::VaultShard | LookupTableAllocationKind::DedicatedVault
            )
            && !table.accepting_allocations
            && !has_live_binding
            && matches!(
                table.desired_state,
                LookupTableLifecycle::Retiring | LookupTableLifecycle::Deactivated
            );
        let is_family_head = family.previous_generation == Some(table.generation)
            || (family.active_generation == Some(table.generation)
                && !safe_retiring_current_vault_shard);
        let has_binding_rollback: bool = sqlx::query_scalar(
            r#"
        SELECT EXISTS (
            SELECT 1 FROM loyal_yield.lookup_table_vault_bindings
            WHERE route_lookup_table_id = $1 AND rollback_until > now()
        )
        "#,
        )
        .bind(table_id)
        .fetch_one(&mut *tx)
        .await?;
        let has_usage_lease: bool = sqlx::query_scalar(
            r#"
        SELECT EXISTS (
            SELECT 1 FROM loyal_yield.lookup_table_usage_leases
            WHERE route_lookup_table_id = $1
              AND released_at IS NULL AND expires_at > now()
        )
        "#,
        )
        .bind(table_id)
        .fetch_one(&mut *tx)
        .await?;
        let has_other_operation: bool = sqlx::query_scalar(
            r#"
        SELECT EXISTS (
            SELECT 1 FROM loyal_yield.lookup_table_operations
            WHERE route_lookup_table_id = $1
              AND idempotency_key <> $2
              AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
        )
        "#,
        )
        .bind(table_id)
        .bind(&input.idempotency_key)
        .fetch_one(&mut *tx)
        .await?;
        if !lifecycle_allowed
            || table.accepting_allocations
            || table.last_verified_slot.is_none()
            || is_family_head
            || rollback_active
            || has_live_binding
            || has_binding_rollback
            || has_usage_lease
            || has_other_operation
        {
            return Err(OrchestratorError::StoreInvariant(
                "lookup table became protected before cleanup enqueue".to_owned(),
            ));
        }
        Ok(())
    }

    pub async fn advance_lookup_table_operation(
        &self,
        operation_id: i64,
        lease: &LookupTableOperationLease,
        advance: LookupTableOperationAdvance,
    ) -> Result<LookupTableOperationRecord, OrchestratorError> {
        advance
            .expected_state
            .transition_to(advance.next_state)
            .map_err(domain_store_error)?;
        let terminal = advance.next_state.is_terminal();
        let mut tx = self.pool().begin().await?;
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_operations
            SET operation_state = $4,
                submitted_slot = CASE WHEN $4 = 'submitted' THEN COALESCE($5, submitted_slot) ELSE submitted_slot END,
                submitted_at = CASE WHEN $4 = 'submitted' THEN COALESCE(submitted_at, now()) ELSE submitted_at END,
                confirmed_slot = CASE WHEN $4 = 'confirmed' THEN COALESCE($5, confirmed_slot) ELSE confirmed_slot END,
                confirmed_at = CASE WHEN $4 = 'confirmed' THEN COALESCE(confirmed_at, now()) ELSE confirmed_at END,
                finalized_slot = CASE WHEN $4 = 'finalized' THEN COALESCE($5, finalized_slot) ELSE finalized_slot END,
                finalized_at = CASE WHEN $4 = 'finalized' THEN COALESCE(finalized_at, now()) ELSE finalized_at END,
                reconciled_slot = CASE WHEN $4 = 'reconciled' THEN COALESCE($5, reconciled_slot) ELSE reconciled_slot END,
                reconciled_at = CASE WHEN $4 = 'reconciled' THEN COALESCE(reconciled_at, now()) ELSE reconciled_at END,
                completed_at = CASE WHEN $4 = 'complete' THEN COALESCE(completed_at, now()) ELSE completed_at END,
                error_code = $6,
                error_detail = $7,
                actual_fee_lamports = COALESCE($10, actual_fee_lamports),
                actual_rent_lamports = COALESCE($11, actual_rent_lamports),
                reclaimed_rent_lamports = COALESCE($12, reclaimed_rent_lamports),
                lease_owner = CASE WHEN $8 THEN NULL ELSE lease_owner END,
                lease_expires_at = CASE WHEN $8 THEN NULL ELSE lease_expires_at END,
                updated_at = now()
            WHERE id = $1
              AND operation_state = $9
              AND lease_owner = $2
              AND fencing_token = $3
              AND lease_expires_at > now()
            RETURNING *
            "#,
        )
        .bind(operation_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(advance.next_state.as_str())
        .bind(advance.observed_slot)
        .bind(advance.error_code)
        .bind(advance.error_detail)
        .bind(terminal)
        .bind(advance.expected_state.as_str())
        .bind(advance.actual_fee_lamports)
        .bind(advance.actual_rent_lamports)
        .bind(advance.reclaimed_rent_lamports)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        let operation = lookup_table_operation_from_row(&row)?;
        let permit_state = match advance.next_state {
            LookupTableOperationStatus::Submitted => "submitted",
            LookupTableOperationStatus::NeedsReconcile => "needs_reconcile",
            LookupTableOperationStatus::PermanentFailure
            | LookupTableOperationStatus::Cancelled => "failed",
            _ => "reconciled",
        };
        sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_provisioner_broadcast_permits
            SET permit_state = $2,
                resolution_detail = COALESCE($3, 'operation advanced through durable reconciliation'),
                resolved_at = now(), updated_at = now()
            WHERE operation_id = $1 AND resolved_at IS NULL
              AND transaction_signature = $4
            "#,
        )
        .bind(operation_id)
        .bind(permit_state)
        .bind(operation.error_detail.as_deref())
        .bind(operation.transaction_signature.as_deref().unwrap_or_default())
        .execute(&mut *tx)
        .await?;
        tx.commit().await?;
        Ok(operation)
    }

    pub async fn retry_lookup_table_operation(
        &self,
        operation_id: i64,
        lease: &LookupTableOperationLease,
        expected_state: LookupTableOperationStatus,
        next_attempt_at: DateTime<Utc>,
        error_code: &str,
        error_detail: &str,
    ) -> Result<LookupTableOperationRecord, OrchestratorError> {
        expected_state
            .transition_to(LookupTableOperationStatus::RetryWait)
            .map_err(domain_store_error)?;
        let mut tx = self.pool().begin().await?;
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_operations
            SET operation_state = 'retry_wait',
                next_attempt_at = $4,
                error_code = $5,
                error_detail = $6,
                operation_context = jsonb_set(
                    operation_context,
                    '{attempt_history}',
                    COALESCE(operation_context->'attempt_history', '[]'::jsonb)
                    || jsonb_build_array(jsonb_strip_nulls(jsonb_build_object(
                        'fencingToken', fencing_token,
                        'transactionSignature', transaction_signature,
                        'messageHash', message_hash,
                        'recentBlockhash', recent_blockhash,
                        'lastValidBlockHeight', last_valid_block_height,
                        'estimatedFeeLamports', estimated_fee_lamports,
                        'estimatedRentLamports', estimated_rent_lamports,
                        'estimatedReclaimedRentLamports', operation_context->'signedExpectedReclaimedRentLamports',
                        'submittedSlot', submitted_slot,
                        'submittedAt', submitted_at,
                        'confirmedSlot', confirmed_slot,
                        'confirmedAt', confirmed_at,
                        'finalizedSlot', finalized_slot,
                        'finalizedAt', finalized_at,
                        'reconciledSlot', reconciled_slot,
                        'reconciledAt', reconciled_at,
                        'archivedAt', now()
                    ))),
                    TRUE
                ) - 'signedExpectedReclaimedRentLamports',
                transaction_signature = NULL,
                message_hash = NULL,
                recent_blockhash = NULL,
                last_valid_block_height = NULL,
                estimated_fee_lamports = NULL,
                estimated_rent_lamports = NULL,
                submitted_slot = NULL,
                submitted_at = NULL,
                confirmed_slot = NULL,
                confirmed_at = NULL,
                finalized_slot = NULL,
                finalized_at = NULL,
                reconciled_slot = NULL,
                reconciled_at = NULL,
                completed_at = NULL,
                lease_owner = NULL,
                lease_expires_at = NULL,
                updated_at = now()
            WHERE id = $1 AND operation_state = $7
              AND lease_owner = $2 AND fencing_token = $3
              AND lease_expires_at > now()
            RETURNING *
            "#,
        )
        .bind(operation_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(next_attempt_at)
        .bind(error_code)
        .bind(error_detail)
        .bind(expected_state.as_str())
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        let operation = lookup_table_operation_from_row(&row)?;
        sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_provisioner_broadcast_permits
            SET permit_state = 'expired',
                resolution_detail = $2,
                resolved_at = now(), updated_at = now()
            WHERE operation_id = $1 AND resolved_at IS NULL
            "#,
        )
        .bind(operation_id)
        .bind(error_detail.chars().take(500).collect::<String>())
        .execute(&mut *tx)
        .await?;
        tx.commit().await?;
        Ok(operation)
    }

    /// Records one worker attempt without ever crossing the durable signing
    /// boundary backwards. Once an operation has a signed identity (or has
    /// reached a signed-or-later state), its transaction metadata is retained
    /// and reconciliation is mandatory. Only an unsigned leased attempt may
    /// return to the retry queue or become permanently failed.
    pub async fn record_lookup_table_operation_attempt_failure(
        &self,
        operation_id: i64,
        lease: &LookupTableOperationLease,
        retry_at: DateTime<Utc>,
        max_attempts: i32,
        code: &str,
        redacted_detail: &str,
    ) -> Result<LookupTableOperationRecord, OrchestratorError> {
        if max_attempts <= 0 || code.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table operation failure policy requires positive max attempts and a code"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        let row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_operations WHERE id = $1 FOR UPDATE",
        )
        .bind(operation_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("lookup-table operation", operation_id))?;
        let operation = lookup_table_operation_from_row(&row)?;
        if operation.operation_state.is_terminal() {
            tx.commit().await?;
            return Ok(operation);
        }
        if operation.lease_owner.as_deref() != Some(lease.owner.as_str())
            || operation.fencing_token != lease.fencing_token
            || operation
                .lease_expires_at
                .is_none_or(|expires_at| expires_at <= Utc::now())
        {
            return Err(stale_fenced_operation(operation_id));
        }

        let signed_or_later = matches!(
            operation.operation_state,
            LookupTableOperationStatus::Signed
                | LookupTableOperationStatus::Submitted
                | LookupTableOperationStatus::Confirmed
                | LookupTableOperationStatus::Finalized
                | LookupTableOperationStatus::Reconciled
        ) || operation.transaction_signature.is_some()
            || operation.message_hash.is_some()
            || operation.recent_blockhash.is_some()
            || operation.last_valid_block_height.is_some();
        if !signed_or_later && operation.operation_state != LookupTableOperationStatus::Leased {
            return Err(OrchestratorError::StoreInvariant(format!(
                "unsigned lookup-table operation {operation_id} is not in the leased attempt state"
            )));
        }
        let next_state = if signed_or_later {
            LookupTableOperationStatus::NeedsReconcile
        } else if operation.attempt_count < max_attempts {
            LookupTableOperationStatus::RetryWait
        } else {
            LookupTableOperationStatus::PermanentFailure
        };
        let bounded_detail = redacted_detail.chars().take(500).collect::<String>();
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_operations
            SET operation_state = $4,
                next_attempt_at = CASE
                    WHEN $4 IN ('retry_wait', 'needs_reconcile') THEN $5
                    ELSE NULL
                END,
                error_code = $6,
                error_detail = $7,
                lease_owner = NULL,
                lease_expires_at = NULL,
                updated_at = now()
            WHERE id = $1 AND lease_owner = $2 AND fencing_token = $3
              AND lease_expires_at > now()
            RETURNING *
            "#,
        )
        .bind(operation_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(next_state.as_str())
        .bind(retry_at)
        .bind(code)
        .bind(&bounded_detail)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        let operation = lookup_table_operation_from_row(&row)?;
        if signed_or_later {
            let permit_state = if next_state == LookupTableOperationStatus::PermanentFailure {
                "failed"
            } else {
                "needs_reconcile"
            };
            sqlx::query(
                r#"
                UPDATE loyal_yield.lookup_table_provisioner_broadcast_permits
                SET permit_state = $2, resolution_detail = $3,
                    resolved_at = now(), updated_at = now()
                WHERE operation_id = $1 AND resolved_at IS NULL
                "#,
            )
            .bind(operation_id)
            .bind(permit_state)
            .bind(&bounded_detail)
            .execute(&mut *tx)
            .await?;
        }
        tx.commit().await?;
        Ok(operation)
    }

    /// Defers an unsigned operation because a prerequisite gate prevented an
    /// attempt. Leasing increments `attempt_count`; this fenced transition
    /// restores that single increment so policy pauses and unavailable
    /// prerequisites cannot exhaust the execution failure budget.
    pub async fn defer_unsigned_lookup_table_operation_without_attempt(
        &self,
        operation_id: i64,
        lease: &LookupTableOperationLease,
        retry_at: DateTime<Utc>,
        code: &str,
        detail: &str,
    ) -> Result<LookupTableOperationRecord, OrchestratorError> {
        if code.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table operation deferral requires an error code".to_owned(),
            ));
        }
        let bounded_detail = detail.chars().take(500).collect::<String>();
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_operations
            SET operation_state = 'retry_wait',
                next_attempt_at = $4,
                error_code = $5,
                error_detail = $6,
                attempt_count = attempt_count - 1,
                lease_owner = NULL,
                lease_expires_at = NULL,
                updated_at = now()
            WHERE id = $1 AND operation_state = 'leased'
              AND lease_owner = $2 AND fencing_token = $3
              AND lease_expires_at > now()
              AND attempt_count > 0
              AND transaction_signature IS NULL
              AND message_hash IS NULL
              AND recent_blockhash IS NULL
              AND last_valid_block_height IS NULL
              AND NOT (operation_context ? 'signedExpectedReclaimedRentLamports')
            RETURNING *
            "#,
        )
        .bind(operation_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(retry_at)
        .bind(code)
        .bind(bounded_detail)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "lookup-table operation {operation_id} is signed, stale, or not an unsigned leased attempt"
            ))
        })?;
        lookup_table_operation_from_row(&row)
    }

    /// Releases a reconciliation lease while preserving the exact signed
    /// identity and state. The lease TTL is crash fencing, not a normal poll
    /// interval; known wait decisions schedule a short bounded retry instead
    /// of idling until the lease expires.
    pub async fn defer_lookup_table_reconciliation_poll(
        &self,
        operation_id: i64,
        lease: &LookupTableOperationLease,
        next_attempt_at: DateTime<Utc>,
        detail: &str,
    ) -> Result<LookupTableOperationRecord, OrchestratorError> {
        let bounded_detail = detail.chars().take(500).collect::<String>();
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_operations
            SET next_attempt_at = $4,
                error_detail = $5,
                lease_owner = NULL,
                lease_expires_at = NULL,
                updated_at = now()
            WHERE id = $1
              AND operation_state IN (
                  'signed', 'submitted', 'confirmed', 'finalized',
                  'reconciled', 'needs_reconcile'
              )
              AND lease_owner = $2 AND fencing_token = $3
              AND lease_expires_at > now()
              AND transaction_signature IS NOT NULL
              AND message_hash IS NOT NULL
              AND recent_blockhash IS NOT NULL
              AND last_valid_block_height IS NOT NULL
            RETURNING *
            "#,
        )
        .bind(operation_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(next_attempt_at)
        .bind(bounded_detail)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        lookup_table_operation_from_row(&row)
    }

    pub async fn replace_confirmed_lookup_table_membership(
        &self,
        table_id: i64,
        expected_mutation_epoch: i64,
        new_mutation_epoch: i64,
        observed_slot: i64,
        observed_last_extended_slot: i64,
        mut addresses: Vec<LookupTableMembershipAddress>,
    ) -> Result<ReusableLookupTableRecord, OrchestratorError> {
        addresses.sort_by_key(|address| address.ordinal);
        validate_membership(&addresses, observed_slot)?;
        if observed_last_extended_slot < 0 || observed_last_extended_slot >= observed_slot {
            return Err(OrchestratorError::StoreInvariant(
                "confirmed lookup-table membership requires a warm finalized last-extended slot"
                    .to_owned(),
            ));
        }
        if new_mutation_epoch <= expected_mutation_epoch {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table membership mutation epoch must advance".to_owned(),
            ));
        }
        let usable_address_count = addresses
            .iter()
            .take_while(|address| address.usable_after_slot <= observed_slot)
            .count() as i32;
        let address_strings = addresses
            .iter()
            .map(|address| address.address.clone())
            .collect::<Vec<_>>();
        let address_hash = ordered_address_hash(&address_strings);
        let addresses_json = serde_json::to_value(&address_strings).map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "could not serialize lookup-table membership: {error}"
            ))
        })?;
        let last_verified_at = addresses
            .iter()
            .map(|address| address.last_verified_at)
            .max()
            .unwrap_or_else(Utc::now);

        let mut tx = self.pool().begin().await?;
        let locked = sqlx::query_scalar::<_, i64>(
            r#"
            SELECT id FROM loyal_yield.route_lookup_tables
            WHERE id = $1 AND family_id IS NOT NULL AND mutation_epoch = $2
            FOR UPDATE
            "#,
        )
        .bind(table_id)
        .bind(expected_mutation_epoch)
        .fetch_optional(&mut *tx)
        .await?;
        if locked.is_none() {
            return Err(stale_store_update("reusable lookup table", table_id));
        }
        sqlx::query(
            "DELETE FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id = $1",
        )
        .bind(table_id)
        .execute(&mut *tx)
        .await?;
        if !addresses.is_empty() {
            let mut query = QueryBuilder::<Postgres>::new(
                "INSERT INTO loyal_yield.lookup_table_addresses (route_lookup_table_id, address, ordinal, added_operation_id, added_slot, usable_after_slot, last_verified_slot, last_verified_at) ",
            );
            query.push_values(&addresses, |mut row, address| {
                row.push_bind(table_id)
                    .push_bind(&address.address)
                    .push_bind(address.ordinal)
                    .push_bind(address.added_operation_id)
                    .push_bind(address.added_slot)
                    .push_bind(address.usable_after_slot)
                    .push_bind(address.last_verified_slot)
                    .push_bind(address.last_verified_at);
            });
            query.build().execute(&mut *tx).await?;
        }
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.route_lookup_tables
            SET address_count = $3,
                address_hash = $4,
                addresses = $5,
                usable_address_count = $6,
                last_verified_slot = $7,
                last_verified_at = $8,
                mutation_epoch = $9,
                last_extended_slot = $10,
                updated_at = now()
            WHERE id = $1 AND mutation_epoch = $2
              AND (last_extended_slot IS NULL OR last_extended_slot <= $10)
            RETURNING *
            "#,
        )
        .bind(table_id)
        .bind(expected_mutation_epoch)
        .bind(addresses.len() as i32)
        .bind(address_hash)
        .bind(addresses_json)
        .bind(usable_address_count)
        .bind(observed_slot)
        .bind(last_verified_at)
        .bind(new_mutation_epoch)
        .bind(observed_last_extended_slot)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("reusable lookup table", table_id))?;
        tx.commit().await?;
        reusable_lookup_table_from_row(&row)
    }

    pub async fn mark_reusable_lookup_table_verification(
        &self,
        table_id: i64,
        expected_mutation_epoch: i64,
        expected_state: LookupTableLifecycle,
        next_state: LookupTableLifecycle,
        accepting_allocations: bool,
        usable_address_count: i32,
        verified_slot: i64,
    ) -> Result<ReusableLookupTableRecord, OrchestratorError> {
        expected_state
            .transition_to(next_state)
            .map_err(domain_store_error)?;
        let legacy_status = match next_state {
            LookupTableLifecycle::Preparing | LookupTableLifecycle::Warming => "warming",
            LookupTableLifecycle::Active
            | LookupTableLifecycle::Standby
            | LookupTableLifecycle::Retiring => "usable",
            LookupTableLifecycle::Deactivated => "deactivated",
            LookupTableLifecycle::Closed => "closed",
            LookupTableLifecycle::Failed => "failed",
        };
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.route_lookup_tables
            SET desired_state = $4,
                status = $5,
                accepting_allocations = CASE
                    WHEN allocation_kind = 'dedicated_vault'
                      OR $4 IN ('retiring', 'deactivated', 'closed', 'failed')
                    THEN FALSE
                    ELSE $6
                END,
                usable_address_count = $7,
                last_verified_slot = $8,
                last_verified_at = now(),
                updated_at = now()
            WHERE id = $1 AND mutation_epoch = $2 AND desired_state = $3
              AND $7 BETWEEN 0 AND address_count
            RETURNING *
            "#,
        )
        .bind(table_id)
        .bind(expected_mutation_epoch)
        .bind(expected_state.as_str())
        .bind(next_state.as_str())
        .bind(legacy_status)
        .bind(accepting_allocations)
        .bind(usable_address_count)
        .bind(verified_slot)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| stale_store_update("reusable lookup table", table_id))?;
        reusable_lookup_table_from_row(&row)
    }

    pub async fn resolve_reusable_lookup_table_bundle(
        &self,
        cluster: &str,
        vault_id: VaultId,
        required_addresses: BTreeSet<String>,
        observed_slot: i64,
        exact_search_limit: usize,
    ) -> Result<ReusableLookupTableResolution, OrchestratorError> {
        let table_rows = sqlx::query(
            r#"
            SELECT DISTINCT route_table.*
            FROM loyal_yield.route_lookup_tables route_table
            JOIN loyal_yield.lookup_table_families family
              ON family.id = route_table.family_id
            LEFT JOIN loyal_yield.lookup_table_vault_bindings binding
              ON binding.route_lookup_table_id = route_table.id
             AND binding.vault_id = $2
             AND binding.lifecycle_state = 'active'
            WHERE family.cluster = $1
              AND family.desired_state = 'active'
              AND route_table.generation = family.active_generation
              AND route_table.desired_state = 'active'
              AND (
                    (family.kind = 'shared_market'
                     AND route_table.allocation_kind = 'shared_market')
                 OR (family.kind = 'vault_shards' AND binding.id IS NOT NULL)
              )
            ORDER BY route_table.generation DESC, route_table.shard_ordinal, route_table.id
            "#,
        )
        .bind(cluster)
        .bind(vault_id.as_i64())
        .fetch_all(self.pool())
        .await?;
        let physical_tables = table_rows
            .iter()
            .map(reusable_lookup_table_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let ids = physical_tables
            .iter()
            .map(|table| table.id)
            .collect::<Vec<_>>();
        let membership_rows = if ids.is_empty() {
            Vec::new()
        } else {
            sqlx::query(
                r#"
                SELECT route_lookup_table_id, address, ordinal, usable_after_slot
                FROM loyal_yield.lookup_table_addresses
                WHERE route_lookup_table_id = ANY($1)
                ORDER BY route_lookup_table_id, ordinal
                "#,
            )
            .bind(&ids)
            .fetch_all(self.pool())
            .await?
        };
        let pending_rows = if ids.is_empty() {
            Vec::new()
        } else {
            sqlx::query(
                r#"
                SELECT operation.route_lookup_table_id, address.address
                FROM loyal_yield.lookup_table_operations operation
                JOIN loyal_yield.lookup_table_operation_addresses address
                  ON address.operation_id = operation.id
                WHERE operation.route_lookup_table_id = ANY($1)
                  AND operation.operation_kind IN ('create', 'extend', 'rollover')
                  AND operation.operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
                ORDER BY operation.route_lookup_table_id, operation.created_at,
                         operation.id, address.ordinal
                "#,
            )
            .bind(&ids)
            .fetch_all(self.pool())
            .await?
        };
        let mut memberships = BTreeMap::<i64, Vec<(i32, String, i64)>>::new();
        for row in membership_rows {
            memberships
                .entry(row.try_get("route_lookup_table_id")?)
                .or_default()
                .push((
                    row.try_get("ordinal")?,
                    row.try_get("address")?,
                    row.try_get("usable_after_slot")?,
                ));
        }
        let mut pending_suffixes = BTreeMap::<i64, Vec<String>>::new();
        for row in pending_rows {
            pending_suffixes
                .entry(row.try_get("route_lookup_table_id")?)
                .or_default()
                .push(row.try_get("address")?);
        }
        let candidates = physical_tables
            .into_iter()
            .map(|table| {
                let membership = memberships.remove(&table.id).unwrap_or_default();
                let ordered_persisted = membership
                    .iter()
                    .map(|(_, address, _)| address.clone())
                    .collect::<Vec<_>>();
                let ordered_usable_prefix = membership
                    .iter()
                    .take_while(|(_, _, usable_after_slot)| *usable_after_slot <= observed_slot)
                    .map(|(_, address, _)| address.clone())
                    .collect::<Vec<_>>();
                let persisted_prefix_verified = membership.len() == table.address_count as usize
                    && membership
                        .iter()
                        .enumerate()
                        .all(|(ordinal, (persisted, _, _))| ordinal as i32 == *persisted)
                    && ordered_usable_prefix.len() == table.usable_address_count as usize
                    && ordered_address_hash(&ordered_persisted) == table.address_hash
                    && table.last_verified_slot.is_some();
                let mut ordered_durable_addresses = ordered_persisted;
                let mut durable_seen = ordered_durable_addresses
                    .iter()
                    .cloned()
                    .collect::<BTreeSet<_>>();
                for address in pending_suffixes.remove(&table.id).unwrap_or_default() {
                    if durable_seen.insert(address.clone()) {
                        ordered_durable_addresses.push(address);
                    }
                }
                ResolverTableCandidate {
                    table_id: table.id,
                    table_address: table.table_address,
                    expected_authority: table.authority,
                    family_id: Some(table.family_id),
                    allocation_kind: Some(table.allocation_kind),
                    generation: table.generation,
                    shard_index: table.shard_ordinal,
                    addresses: ordered_usable_prefix.iter().cloned().collect(),
                    usable_prefix_len: table.usable_address_count as u16,
                    ordered_usable_prefix,
                    ordered_durable_addresses,
                    address_hash: table.address_hash,
                    mutation_epoch: table.mutation_epoch,
                    last_verified_slot: table.last_verified_slot,
                    lifecycle: table.desired_state,
                    persisted_prefix_verified,
                    rpc_verified: false,
                    usable: persisted_prefix_verified,
                }
            })
            .collect::<Vec<_>>();
        let (tables, missing_addresses) = persisted_relevant_table_candidates(
            &required_addresses,
            &candidates,
            exact_search_limit,
        )
        .map_err(domain_store_error)?;
        Ok(ReusableLookupTableResolution {
            tables,
            required_addresses,
            missing_addresses,
        })
    }

    pub async fn upsert_lookup_table_readiness(
        &self,
        mut input: LookupTableReadinessRecord,
    ) -> Result<LookupTableReadinessRecord, OrchestratorError> {
        input.selected_table_ids.sort();
        input.selected_table_ids.dedup();
        if input.selected_table_count != Some(input.selected_table_ids.len() as i32) {
            return Err(OrchestratorError::StoreInvariant(
                "readiness selected table count does not match its unique table ids".to_owned(),
            ));
        }

        for attempt in 1..=LOOKUP_TABLE_DB_CONCURRENCY_MAX_ATTEMPTS {
            match self.upsert_lookup_table_readiness_once(input.clone()).await {
                Ok(readiness) => return Ok(readiness),
                Err(error) => {
                    let Some(sqlstate) = retryable_lookup_table_database_conflict(&error) else {
                        return Err(error);
                    };
                    if attempt == LOOKUP_TABLE_DB_CONCURRENCY_MAX_ATTEMPTS {
                        return Err(error);
                    }
                    log_lookup_table_database_retry(
                        "upsert_lookup_table_readiness",
                        sqlstate,
                        attempt,
                    );
                    sleep_for_lookup_table_database_retry(attempt).await;
                }
            }
        }
        unreachable!("bounded lookup-table database retry returns on its final attempt")
    }

    async fn upsert_lookup_table_readiness_once(
        &self,
        input: LookupTableReadinessRecord,
    ) -> Result<LookupTableReadinessRecord, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        // One vault can publish several overlapping route variants at once.
        // Serialize that vault's readiness transactions before they acquire
        // physical-table and readiness-row locks in different combinations.
        acquire_lookup_table_readiness_vault_lock(&mut tx, &input.cluster, input.vault_id).await?;
        if !input.selected_table_ids.is_empty() {
            // Readiness writes are a fleet hot path. Serialize only against
            // lifecycle changes to the selected physical tables, in canonical
            // id order, instead of taking the cluster-wide rollout lock.
            let selected_rows = sqlx::query(
                r#"
                SELECT id, cluster, status, durable, family_id, desired_state
                FROM loyal_yield.route_lookup_tables
                WHERE id = ANY($1)
                ORDER BY id
                FOR SHARE
                "#,
            )
            .bind(&input.selected_table_ids)
            .fetch_all(&mut *tx)
            .await?;
            if selected_rows.len() != input.selected_table_ids.len() {
                return Err(OrchestratorError::StoreInvariant(
                    "readiness selected a missing lookup table".to_owned(),
                ));
            }
            for row in &selected_rows {
                let family_id: Option<i64> = row.try_get("family_id")?;
                let is_legacy = family_id.is_none();
                let selectable = row.try_get::<String, _>("cluster")? == input.cluster
                    && if is_legacy {
                        row.try_get::<bool, _>("durable")?
                            && matches!(
                                row.try_get::<String, _>("status")?.as_str(),
                                "active" | "warming" | "usable"
                            )
                    } else {
                        matches!(
                            row.try_get::<Option<String>, _>("desired_state")?
                                .as_deref(),
                            Some("active")
                        )
                    };
                let selection_kind_matches = match input.selection_kind {
                    Some(LookupTableSelectionKind::Legacy) => is_legacy,
                    Some(LookupTableSelectionKind::Reusable) => !is_legacy,
                    Some(LookupTableSelectionKind::Blocked) | None => true,
                };
                if !selectable || !selection_kind_matches {
                    return Err(OrchestratorError::StoreInvariant(format!(
                        "readiness selected lookup table {} after it became non-selectable or changed class",
                        row.try_get::<i64, _>("id")?
                    )));
                }
            }
        }
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_route_readiness_current
                (cluster, vault_id, route_fingerprint, requirements_fingerprint,
                 route_kind, source_reserve, target_reserve, manifest_id,
                 shared_family_id, vault_binding_id, readiness_state,
                 required_address_count, covered_address_count, missing_addresses,
                 legacy_table_ids, reusable_table_ids, compiled_message_size,
                 packet_limit, observed_slot, observed_at, selection_kind,
                 fallback_reason, rollout_mode, selected_table_ids,
                 selected_table_count, packet_fits, simulation_state,
                 simulation_units_consumed, simulation_error)
            VALUES
                ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
                 $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23,
                 $24, $25, $26, $27, $28, $29)
            ON CONFLICT (cluster, vault_id, route_fingerprint, requirements_fingerprint)
            DO UPDATE SET
                route_kind = EXCLUDED.route_kind,
                source_reserve = EXCLUDED.source_reserve,
                target_reserve = EXCLUDED.target_reserve,
                manifest_id = EXCLUDED.manifest_id,
                shared_family_id = EXCLUDED.shared_family_id,
                vault_binding_id = EXCLUDED.vault_binding_id,
                readiness_state = EXCLUDED.readiness_state,
                required_address_count = EXCLUDED.required_address_count,
                covered_address_count = EXCLUDED.covered_address_count,
                missing_addresses = EXCLUDED.missing_addresses,
                legacy_table_ids = EXCLUDED.legacy_table_ids,
                reusable_table_ids = EXCLUDED.reusable_table_ids,
                compiled_message_size = EXCLUDED.compiled_message_size,
                packet_limit = EXCLUDED.packet_limit,
                observed_slot = EXCLUDED.observed_slot,
                observed_at = EXCLUDED.observed_at,
                selection_kind = EXCLUDED.selection_kind,
                fallback_reason = EXCLUDED.fallback_reason,
                rollout_mode = EXCLUDED.rollout_mode,
                selected_table_ids = EXCLUDED.selected_table_ids,
                selected_table_count = EXCLUDED.selected_table_count,
                packet_fits = EXCLUDED.packet_fits,
                simulation_state = EXCLUDED.simulation_state,
                simulation_units_consumed = EXCLUDED.simulation_units_consumed,
                simulation_error = EXCLUDED.simulation_error,
                updated_at = now()
            RETURNING *
            "#,
        )
        .bind(input.cluster)
        .bind(input.vault_id.as_i64())
        .bind(input.route_fingerprint)
        .bind(input.requirements_fingerprint)
        .bind(input.route_kind)
        .bind(input.source_reserve)
        .bind(input.target_reserve)
        .bind(input.manifest_id)
        .bind(input.shared_family_id)
        .bind(input.vault_binding_id)
        .bind(input.readiness_state.as_str())
        .bind(input.required_address_count)
        .bind(input.covered_address_count)
        .bind(input.missing_addresses)
        .bind(input.legacy_table_ids)
        .bind(input.reusable_table_ids)
        .bind(input.compiled_message_size)
        .bind(input.packet_limit)
        .bind(input.observed_slot)
        .bind(input.observed_at)
        .bind(input.selection_kind.map(LookupTableSelectionKind::as_str))
        .bind(input.fallback_reason)
        .bind(input.rollout_mode.map(LookupTableRolloutMode::as_str))
        .bind(input.selected_table_ids)
        .bind(input.selected_table_count)
        .bind(input.packet_fits)
        .bind(
            input
                .simulation_state
                .map(LookupTableSimulationState::as_str),
        )
        .bind(input.simulation_units_consumed)
        .bind(input.simulation_error)
        .fetch_one(&mut *tx)
        .await?;
        let readiness = lookup_table_readiness_from_row(&row)?;
        tx.commit().await?;
        Ok(readiness)
    }

    pub async fn lookup_table_readiness(
        &self,
        cluster: &str,
        vault_id: VaultId,
        route_fingerprint: &str,
        requirements_fingerprint: &str,
    ) -> Result<Option<LookupTableReadinessRecord>, OrchestratorError> {
        let row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_route_readiness_current
            WHERE cluster = $1 AND vault_id = $2 AND route_fingerprint = $3
              AND requirements_fingerprint = $4
            "#,
        )
        .bind(cluster)
        .bind(vault_id.as_i64())
        .bind(route_fingerprint)
        .bind(requirements_fingerprint)
        .fetch_optional(self.pool())
        .await?;
        row.as_ref()
            .map(lookup_table_readiness_from_row)
            .transpose()
    }

    pub async fn reusable_only_cutover_preflight(
        &self,
        cluster: &str,
    ) -> Result<ReusableOnlyCutoverPreflight, OrchestratorError> {
        if cluster.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "reusable-only cutover preflight requires a cluster".to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        let evidence = load_reusable_only_cutover_preflight_in_connection(&mut tx, cluster).await?;
        tx.commit().await?;
        Ok(evidence)
    }

    /// Performs the direct reusable-only cutover as one cluster-fenced write.
    /// It intentionally proves only the durable shared catalog and demand
    /// provisioning infrastructure, never fleet-wide vault coverage.
    pub async fn activate_reusable_only_cutover(
        &self,
        expected_preflight: &ReusableOnlyCutoverPreflight,
        finalized_observation: &FinalizedSharedTableObservation,
        reason: &str,
        updated_by: &str,
    ) -> Result<ReusableOnlyCutoverResult, OrchestratorError> {
        let cluster = expected_preflight.cluster.as_str();
        if cluster.trim().is_empty() || reason.trim().is_empty() || updated_by.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "reusable-only cutover requires valid cluster, operator, and exact finalized shared-table evidence"
                    .to_owned(),
            ));
        }
        let finalized_addresses =
            validate_finalized_shared_table_observation(finalized_observation)?;
        let mut tx = self.pool().begin().await?;
        acquire_lookup_table_rollout_lock(&mut tx, cluster).await?;
        let provisioner_control_row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster = $1 FOR UPDATE",
        )
        .bind(cluster)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "reusable-only cutover requires a durable provisioner pause".to_owned(),
            )
        })?;
        let provisioner_control =
            lookup_table_provisioner_control_from_row(&provisioner_control_row)?;
        if !provisioner_control.paused {
            return Err(OrchestratorError::StoreInvariant(
                "reusable-only cutover requires the durable provisioner pause to remain active"
                    .to_owned(),
            ));
        }
        let active_broadcast_permit_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*)::BIGINT
            FROM loyal_yield.lookup_table_provisioner_broadcast_permits
            WHERE cluster = $1 AND resolved_at IS NULL
            "#,
        )
        .bind(cluster)
        .fetch_one(&mut *tx)
        .await?;
        let in_flight_mutation_count =
            lookup_table_in_flight_mutation_count_in_connection(&mut tx, cluster).await?;
        if active_broadcast_permit_count != 0 || in_flight_mutation_count != 0 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "reusable-only cutover requires a drained pause; active permits={active_broadcast_permit_count}, in-flight mutations={in_flight_mutation_count}"
            )));
        }
        let current_preflight =
            load_reusable_only_cutover_preflight_in_connection(&mut tx, cluster).await?;
        if current_preflight != *expected_preflight {
            return Err(OrchestratorError::StoreInvariant(
                "reusable-only cutover preflight evidence changed after finalized RPC verification"
                    .to_owned(),
            ));
        }
        validate_finalized_shared_tables_against_preflight(
            &current_preflight,
            finalized_observation,
        )?;
        let probe_row = sqlx::query(
            r#"
            SELECT *
            FROM loyal_yield.lookup_table_precutover_probe_runs
            WHERE cluster = $1
            ORDER BY created_at DESC, id DESC
            LIMIT 1
            FOR SHARE
            "#,
        )
        .bind(cluster)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "reusable-only cutover requires an immutable successful pre-cutover probe"
                    .to_owned(),
            )
        })?;
        let probe =
            lookup_table_precutover_probe_from_row_in_connection(&mut tx, &probe_row).await?;
        let probe_tables_match = probe.shared_tables.len()
            == finalized_observation.shared_tables.len()
            && probe
                .shared_tables
                .iter()
                .zip(&finalized_observation.shared_tables)
                .all(|(persisted, observed)| {
                    persisted.shard_ordinal == observed.shard_ordinal
                        && persisted.route_lookup_table_id == observed.table_id
                        && persisted.shared_table_address == observed.table_address
                        && persisted.shared_authority == observed.authority
                        && persisted.shared_mutation_epoch == observed.mutation_epoch
                        && persisted.finalized_slot <= finalized_observation.observed_slot
                        && persisted.finalized_last_extended_slot == observed.last_extended_slot
                        && persisted.finalized_address_hash == observed.ordered_address_hash
                        && persisted.finalized_address_count == observed.address_count
                });
        let probe_target = finalized_observation
            .shared_tables
            .iter()
            .find(|table| table.table_id == probe.route_lookup_table_id);
        if probe.result != "pass"
            || probe.provisioner_control_epoch != provisioner_control.control_epoch
            || probe.catalog_revision_id != current_preflight.catalog_revision_id
            || probe.shared_manifest_id != current_preflight.manifest_id
            || probe.shared_table_bundle_hash != current_preflight.shared_table_bundle_hash
            || probe.shared_table_count
                != i32::try_from(current_preflight.shared_tables.len()).unwrap_or(-1)
            || probe.finalized_bundle_address_count
                != i32::try_from(current_preflight.ordered_addresses.len()).unwrap_or(-1)
            || !probe_tables_match
            || probe_target.is_none_or(|target| {
                probe.shared_table_address != target.table_address
                    || probe.shared_authority != target.authority
                    || probe.shared_mutation_epoch != target.mutation_epoch
                    || probe.finalized_address_hash != target.ordered_address_hash
                    || probe.finalized_address_count != target.address_count
                    || probe.finalized_last_extended_slot != target.last_extended_slot
            })
            || probe.finalized_slot > finalized_observation.observed_slot
            || !probe.finalized_shared_exact
        {
            return Err(OrchestratorError::StoreInvariant(
                "reusable-only cutover pre-cutover probe is absent, stale, or does not match the exact finalized shared-table evidence"
                    .to_owned(),
            ));
        }
        let mutation_after_probe_count: i64 = sqlx::query_scalar(
            r#"
            SELECT
                (SELECT count(*)::BIGINT
                 FROM loyal_yield.lookup_table_operations operation
                 JOIN loyal_yield.lookup_table_families family
                   ON family.id = operation.family_id
                 WHERE family.cluster = $1 AND operation.updated_at > $2)
              + (SELECT count(*)::BIGINT
                 FROM loyal_yield.lookup_table_provisioner_broadcast_permits permit
                 WHERE permit.cluster = $1 AND permit.updated_at > $2)
            "#,
        )
        .bind(cluster)
        .bind(probe.created_at)
        .fetch_one(&mut *tx)
        .await?;
        if mutation_after_probe_count != 0 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "reusable-only cutover observed {mutation_after_probe_count} ALT operation or permit mutation(s) after the successful probe"
            )));
        }
        let catalog = load_shared_market_catalog_head_in_connection(
            &mut tx,
            cluster,
            SharedMarketCatalogHeadLock::Update,
        )
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "cluster {cluster:?} has no authoritative shared-market catalog head"
            ))
        })?;
        let shared_generation = catalog.target_generation.ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "shared-market catalog head has no planned target generation".to_owned(),
            )
        })?;
        let shared_evidence = shared_market_catalog_generation_evidence_in_connection(
            &mut tx,
            catalog.family_id,
            catalog.active_generation,
            &catalog.addresses,
        )
        .await?;
        if catalog.readiness_state != SharedMarketCatalogReadiness::Active
            || catalog.active_generation != Some(shared_generation)
            || !shared_evidence.ready
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "shared-market catalog revision {} is not exact, active, and reusable-only ready",
                catalog.catalog_revision_id
            )));
        }
        let vault_family_rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_families
            WHERE cluster = $1 AND kind = 'vault_shards' AND desired_state = 'active'
            ORDER BY logical_name, id FOR UPDATE
            "#,
        )
        .bind(cluster)
        .fetch_all(&mut *tx)
        .await?;
        if vault_family_rows.len() != 1 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "cluster {cluster:?} requires exactly one active vault-shards family before cutover, found {}",
                vault_family_rows.len()
            )));
        }
        let vault_family = lookup_table_family_from_row(&vault_family_rows[0])?;
        sqlx::query(
            "SELECT id FROM loyal_yield.lookup_table_rollout_controls WHERE cluster = $1 ORDER BY id FOR UPDATE",
        )
        .bind(cluster)
        .fetch_all(&mut *tx)
        .await?;
        let global_row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_rollout_controls
                (cluster, vault_id, rollout_mode, force_legacy, reason, updated_by)
            VALUES ($1, NULL, 'reusable_only', FALSE, $2, $3)
            ON CONFLICT (cluster) WHERE vault_id IS NULL DO UPDATE SET
                rollout_mode = 'reusable_only', force_legacy = FALSE,
                reason = EXCLUDED.reason, updated_by = EXCLUDED.updated_by,
                updated_at = now()
            RETURNING *
            "#,
        )
        .bind(cluster)
        .bind(reason)
        .bind(updated_by)
        .fetch_one(&mut *tx)
        .await?;
        let global_control = lookup_table_rollout_from_row(&global_row)?;
        let aligned = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_rollout_controls
            SET rollout_mode = 'reusable_only', force_legacy = FALSE,
                reason = $2, updated_by = $3, updated_at = now()
            WHERE cluster = $1 AND vault_id IS NOT NULL
              AND (rollout_mode <> 'reusable_only' OR force_legacy
                   OR reason IS DISTINCT FROM $2 OR updated_by <> $3)
            "#,
        )
        .bind(cluster)
        .bind(reason)
        .bind(updated_by)
        .execute(&mut *tx)
        .await?;
        let hidden_override_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*) FROM loyal_yield.lookup_table_rollout_controls
            WHERE cluster = $1
              AND (rollout_mode <> 'reusable_only' OR force_legacy)
            "#,
        )
        .bind(cluster)
        .fetch_one(&mut *tx)
        .await?;
        if hidden_override_count != 0 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "reusable-only cutover left {hidden_override_count} hidden rollout override(s)"
            )));
        }
        let result = ReusableOnlyCutoverResult {
            cluster: cluster.to_owned(),
            catalog_revision_id: catalog.catalog_revision_id,
            shared_family_id: catalog.family_id,
            shared_generation,
            vault_family_id: vault_family.id,
            aligned_vault_control_count: i64::try_from(aligned.rows_affected()).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "aligned vault rollout control count exceeds i64".to_owned(),
                )
            })?,
            provisioner_control_epoch: provisioner_control.control_epoch,
            finalized_observed_slot: finalized_observation.observed_slot,
            finalized_address_hash: current_preflight.ordered_address_hash,
            finalized_address_count: i32::try_from(finalized_addresses.len()).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "cutover finalized bundle address count exceeds INTEGER".to_owned(),
                )
            })?,
            global_control,
        };
        tx.commit().await?;
        Ok(result)
    }

    pub async fn lookup_table_provisioner_control(
        &self,
        cluster: &str,
    ) -> Result<Option<LookupTableProvisionerControlRecord>, OrchestratorError> {
        let row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster = $1",
        )
        .bind(cluster)
        .fetch_optional(self.pool())
        .await?;
        row.as_ref()
            .map(lookup_table_provisioner_control_from_row)
            .transpose()
    }

    /// Rechecks the exact shared catalog/table suffix in a short transaction
    /// immediately before simulation and signing. A superseded unsigned lease
    /// is cancelled without consuming RPC budget; signed identities are never
    /// accepted by this gate and remain reconciliation-only.
    pub async fn fence_leased_shared_market_operation_before_signing(
        &self,
        cluster: &str,
        operation_id: i64,
        lease: &LookupTableOperationLease,
    ) -> Result<LookupTableSharedMarketOperationFenceResult, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        let catalog = load_shared_market_catalog_head_in_connection(
            &mut tx,
            cluster,
            SharedMarketCatalogHeadLock::Update,
        )
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "cluster {cluster:?} has no current shared-market catalog head"
            ))
        })?;
        let operation_row = sqlx::query(
            r#"
            SELECT operation.*
            FROM loyal_yield.lookup_table_operations operation
            JOIN loyal_yield.lookup_table_families family
              ON family.id = operation.family_id
            WHERE operation.id = $1 AND family.id = $2
              AND family.cluster = $3 AND family.kind = 'shared_market'
              AND operation.operation_state = 'leased'
              AND operation.lease_owner = $4
              AND operation.fencing_token = $5
              AND operation.lease_expires_at > now()
              AND operation.transaction_signature IS NULL
              AND operation.message_hash IS NULL
              AND operation.recent_blockhash IS NULL
              AND operation.last_valid_block_height IS NULL
            FOR UPDATE OF operation
            "#,
        )
        .bind(operation_id)
        .bind(catalog.family_id)
        .bind(cluster)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        let operation = lookup_table_operation_from_row(&operation_row)?;
        if !matches!(
            operation.operation_kind,
            LookupTableOperationKind::Create
                | LookupTableOperationKind::Extend
                | LookupTableOperationKind::Rollover
        ) {
            tx.commit().await?;
            return Ok(LookupTableSharedMarketOperationFenceResult::Current);
        }
        let table_id = operation.route_lookup_table_id.ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "shared-market operation {operation_id} has no physical table"
            ))
        })?;
        let table_row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.route_lookup_tables
            WHERE id = $1 AND family_id = $2 AND cluster = $3
            FOR UPDATE
            "#,
        )
        .bind(table_id)
        .bind(catalog.family_id)
        .bind(cluster)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("shared-market physical table", table_id))?;
        let table = reusable_lookup_table_from_row(&table_row)?;
        let Some(reason) =
            shared_market_operation_head_fence_detail(&mut tx, &catalog, &operation, &table)
                .await?
        else {
            tx.commit().await?;
            return Ok(LookupTableSharedMarketOperationFenceResult::Current);
        };
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_operations
            SET operation_state = 'cancelled', next_attempt_at = NULL,
                error_code = 'stale_shared_market_catalog_before_signing',
                error_detail = $4, lease_owner = NULL,
                lease_expires_at = NULL, updated_at = now()
            WHERE id = $1 AND operation_state = 'leased'
              AND lease_owner = $2 AND fencing_token = $3
              AND lease_expires_at > now()
              AND transaction_signature IS NULL
              AND message_hash IS NULL
              AND recent_blockhash IS NULL
              AND last_valid_block_height IS NULL
            RETURNING *
            "#,
        )
        .bind(operation_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(&reason)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        let operation = lookup_table_operation_from_row(&row)?;
        tx.commit().await?;
        Ok(LookupTableSharedMarketOperationFenceResult::Cancelled { operation, reason })
    }

    /// Atomically grants one durable permit for an already-persisted signed
    /// identity. This transaction commits before the caller performs RPC I/O.
    /// Pause administration locks the same control row, so a pause either wins
    /// and prevents the grant or observes an unresolved permit as in-flight.
    pub async fn grant_lookup_table_provisioner_broadcast_permit(
        &self,
        cluster: &str,
        operation_id: i64,
        lease: &LookupTableOperationLease,
        retry_at: DateTime<Utc>,
    ) -> Result<LookupTableProvisionerBroadcastPermitResult, OrchestratorError> {
        if cluster.trim().is_empty() || operation_id <= 0 {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table broadcast permit requires a cluster and positive operation id"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_provisioner_controls
                (cluster, paused, reason, updated_by, control_epoch)
            VALUES ($1, FALSE, 'implicit unpaused provisioner control', $2, 0)
            ON CONFLICT (cluster) DO NOTHING
            "#,
        )
        .bind(cluster)
        .bind(&lease.owner)
        .execute(&mut *tx)
        .await?;
        let control_row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster = $1 FOR UPDATE",
        )
        .bind(cluster)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "lookup-table broadcast permit lost its provisioner control row".to_owned(),
            )
        })?;
        let control = lookup_table_provisioner_control_from_row(&control_row)?;
        let operation_family_row = sqlx::query(
            r#"
            SELECT family.id, family.kind
            FROM loyal_yield.lookup_table_operations operation
            JOIN loyal_yield.lookup_table_families family
              ON family.id = operation.family_id
            WHERE operation.id = $1 AND family.cluster = $2
            "#,
        )
        .bind(operation_id)
        .bind(cluster)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("lookup-table operation", operation_id))?;
        let family_id: i64 = operation_family_row.try_get("id")?;
        let family_kind: LookupTableFamilyKind = parse_store_enum(
            "lookup-table family kind",
            operation_family_row.try_get("kind")?,
        )?;
        let shared_catalog = if family_kind == LookupTableFamilyKind::SharedMarket {
            Some(
                load_shared_market_catalog_head_in_connection(
                    &mut tx,
                    cluster,
                    SharedMarketCatalogHeadLock::Update,
                )
                .await?
                .ok_or_else(|| {
                    OrchestratorError::StoreInvariant(format!(
                        "cluster {cluster:?} has no current shared-market catalog head"
                    ))
                })?,
            )
        } else {
            None
        };
        let family_row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 AND cluster = $2 FOR SHARE",
        )
        .bind(family_id)
        .bind(cluster)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("lookup-table family", family_id))?;
        let family = lookup_table_family_from_row(&family_row)?;
        if family.kind != family_kind {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table operation {operation_id} family kind changed during permit fencing"
            )));
        }
        let operation_row = sqlx::query(
            r#"
            SELECT operation.*
            FROM loyal_yield.lookup_table_operations operation
            JOIN loyal_yield.lookup_table_families family
              ON family.id = operation.family_id
            WHERE operation.id = $1 AND family.cluster = $2
            FOR UPDATE OF operation
            "#,
        )
        .bind(operation_id)
        .bind(cluster)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("lookup-table operation", operation_id))?;
        let operation = lookup_table_operation_from_row(&operation_row)?;
        if operation.operation_state != LookupTableOperationStatus::Signed
            || operation.lease_owner.as_deref() != Some(lease.owner.as_str())
            || operation.fencing_token != lease.fencing_token
            || operation
                .lease_expires_at
                .is_none_or(|expires_at| expires_at <= Utc::now())
            || operation.transaction_signature.is_none()
            || operation.message_hash.is_none()
            || operation.recent_blockhash.is_none()
            || operation.last_valid_block_height.is_none()
        {
            return Err(stale_fenced_operation(operation_id));
        }
        let operation_signature = operation
            .transaction_signature
            .clone()
            .ok_or_else(|| stale_fenced_operation(operation_id))?;
        let operation_message_hash = operation
            .message_hash
            .clone()
            .ok_or_else(|| stale_fenced_operation(operation_id))?;
        if control.paused {
            let bounded_reason = control.reason.chars().take(500).collect::<String>();
            let operation_row = sqlx::query(
                r#"
                UPDATE loyal_yield.lookup_table_operations
                SET operation_state = 'needs_reconcile',
                    next_attempt_at = $4,
                    error_code = 'cluster_provisioner_paused_before_broadcast',
                    error_detail = $5,
                    operation_context = jsonb_set(
                        operation_context,
                        '{lastBroadcastPauseFence}',
                        jsonb_build_object(
                            'controlEpoch', $6::BIGINT,
                            'updatedBy', $7::TEXT,
                            'observedAt', now()
                        ),
                        TRUE
                    ),
                    lease_owner = NULL,
                    lease_expires_at = NULL,
                    updated_at = now()
                WHERE id = $1 AND operation_state = 'signed'
                  AND lease_owner = $2 AND fencing_token = $3
                  AND lease_expires_at > now()
                  AND transaction_signature IS NOT NULL
                  AND message_hash IS NOT NULL
                  AND recent_blockhash IS NOT NULL
                  AND last_valid_block_height IS NOT NULL
                RETURNING *
                "#,
            )
            .bind(operation_id)
            .bind(&lease.owner)
            .bind(lease.fencing_token)
            .bind(retry_at)
            .bind(bounded_reason)
            .bind(control.control_epoch)
            .bind(&control.updated_by)
            .fetch_optional(&mut *tx)
            .await?
            .ok_or_else(|| stale_fenced_operation(operation_id))?;
            let operation = lookup_table_operation_from_row(&operation_row)?;
            tx.commit().await?;
            return Ok(LookupTableProvisionerBroadcastPermitResult::Paused { control, operation });
        }
        let mut fence = None::<(&str, String)>;
        if matches!(
            operation.operation_kind,
            LookupTableOperationKind::Create
                | LookupTableOperationKind::Extend
                | LookupTableOperationKind::Rollover
                | LookupTableOperationKind::Deactivate
                | LookupTableOperationKind::Close
        ) {
            let table_id = operation.route_lookup_table_id.ok_or_else(|| {
                OrchestratorError::StoreInvariant(format!(
                    "mutating lookup-table operation {operation_id} has no physical table"
                ))
            })?;
            let table_row = sqlx::query(
                r#"
                SELECT * FROM loyal_yield.route_lookup_tables
                WHERE id = $1 FOR UPDATE
                "#,
            )
            .bind(table_id)
            .fetch_optional(&mut *tx)
            .await?
            .ok_or_else(|| stale_store_update("reusable lookup table", table_id))?;
            let table = reusable_lookup_table_from_row(&table_row)?;
            if operation.family_id != family.id
                || operation.route_lookup_table_id != Some(table.id)
                || table.family_id != family.id
                || table.cluster != cluster
                || table.authority != family.provisioning_authority
                || table.payer != family.payer
                || operation.mutation_epoch != table.mutation_epoch
            {
                fence = Some((
                    "lookup_table_identity_changed_before_broadcast",
                    "mutating operation lost its cluster, family, authority, payer, table, or mutation-epoch identity"
                        .to_owned(),
                ));
            } else if sqlx::query_scalar::<_, bool>(
                r#"
                SELECT EXISTS (
                    SELECT 1
                    FROM loyal_yield.lookup_table_usage_leases
                    WHERE route_lookup_table_id = $1
                      AND released_at IS NULL
                      AND expires_at > now()
                )
                "#,
            )
            .bind(table.id)
            .fetch_one(&mut *tx)
            .await?
            {
                fence = Some((
                    "lookup_table_usage_lease_active_before_broadcast",
                    "mutating operation is blocked by an active route lookup-table usage lease"
                        .to_owned(),
                ));
            } else if let Some(catalog) = shared_catalog.as_ref() {
                if let Some(detail) =
                    shared_market_operation_head_fence_detail(&mut tx, catalog, &operation, &table)
                        .await?
                {
                    fence = Some(("stale_shared_market_catalog_before_broadcast", detail));
                }
            }
        }
        if let Some((error_code, error_detail)) = fence {
            let operation_row = sqlx::query(
                r#"
                UPDATE loyal_yield.lookup_table_operations
                SET operation_state = 'needs_reconcile', next_attempt_at = $4,
                    error_code = $5, error_detail = $6,
                    lease_owner = NULL, lease_expires_at = NULL,
                    updated_at = now()
                WHERE id = $1 AND operation_state = 'signed'
                  AND lease_owner = $2 AND fencing_token = $3
                  AND lease_expires_at > now()
                  AND transaction_signature IS NOT NULL
                  AND message_hash IS NOT NULL
                  AND recent_blockhash IS NOT NULL
                  AND last_valid_block_height IS NOT NULL
                RETURNING *
                "#,
            )
            .bind(operation_id)
            .bind(&lease.owner)
            .bind(lease.fencing_token)
            .bind(retry_at)
            .bind(error_code)
            .bind(&error_detail)
            .fetch_optional(&mut *tx)
            .await?
            .ok_or_else(|| stale_fenced_operation(operation_id))?;
            let operation = lookup_table_operation_from_row(&operation_row)?;
            tx.commit().await?;
            return Ok(LookupTableProvisionerBroadcastPermitResult::Fenced {
                control,
                operation,
                error_code: error_code.to_owned(),
                error_detail,
            });
        }
        let permit_row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_provisioner_broadcast_permits
                (cluster, operation_id, fencing_token, control_epoch,
                 transaction_signature, message_hash)
            VALUES ($1, $2, $3, $4, $5, $6)
            ON CONFLICT (operation_id, fencing_token) DO NOTHING
            RETURNING *
            "#,
        )
        .bind(cluster)
        .bind(operation_id)
        .bind(lease.fencing_token)
        .bind(control.control_epoch)
        .bind(&operation_signature)
        .bind(&operation_message_hash)
        .fetch_optional(&mut *tx)
        .await?;
        let permit_row = match permit_row {
            Some(row) => row,
            None => sqlx::query(
                r#"
                SELECT *
                FROM loyal_yield.lookup_table_provisioner_broadcast_permits
                WHERE operation_id = $1 AND fencing_token = $2
                FOR UPDATE
                "#,
            )
            .bind(operation_id)
            .bind(lease.fencing_token)
            .fetch_optional(&mut *tx)
            .await?
            .ok_or_else(|| stale_fenced_operation(operation_id))?,
        };
        let permit = lookup_table_provisioner_broadcast_permit_from_row(&permit_row)?;
        if permit.cluster != cluster
            || permit.control_epoch != control.control_epoch
            || permit.transaction_signature != operation_signature
            || permit.message_hash != operation_message_hash
            || permit.permit_state != "granted"
            || permit.resolved_at.is_some()
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table operation {operation_id} has conflicting or consumed broadcast-permit identity"
            )));
        }
        tx.commit().await?;
        Ok(LookupTableProvisionerBroadcastPermitResult::Granted {
            control,
            operation,
            permit,
        })
    }

    /// Resolves the durable permit and hands it to the operation state machine
    /// in one short transaction after the RPC send attempt returns. If the
    /// process crashes before this handoff, the unresolved permit remains a
    /// cutover blocker and reconciliation inspects the persisted signature.
    pub async fn resolve_lookup_table_provisioner_broadcast_permit(
        &self,
        permit_id: i64,
        operation_id: i64,
        lease: &LookupTableOperationLease,
        resolution: LookupTableProvisionerBroadcastResolution,
    ) -> Result<LookupTableOperationRecord, OrchestratorError> {
        if permit_id <= 0 || operation_id <= 0 {
            return Err(OrchestratorError::StoreInvariant(
                "broadcast-permit resolution requires positive identities".to_owned(),
            ));
        }
        let (next_state, observed_slot, error_code, error_detail, permit_state) = match resolution {
            LookupTableProvisionerBroadcastResolution::Submitted { observed_slot } => (
                LookupTableOperationStatus::Submitted,
                Some(observed_slot),
                None,
                None,
                "submitted",
            ),
            LookupTableProvisionerBroadcastResolution::NeedsReconcile {
                observed_slot,
                error_code,
                error_detail,
            } => (
                LookupTableOperationStatus::NeedsReconcile,
                observed_slot,
                Some(error_code),
                Some(error_detail),
                "needs_reconcile",
            ),
        };
        LookupTableOperationStatus::Signed
            .transition_to(next_state)
            .map_err(domain_store_error)?;
        let mut tx = self.pool().begin().await?;
        let permit_row = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_provisioner_broadcast_permits
            WHERE id = $1 AND operation_id = $2 FOR UPDATE
            "#,
        )
        .bind(permit_id)
        .bind(operation_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_store_update("lookup-table broadcast permit", permit_id))?;
        let permit = lookup_table_provisioner_broadcast_permit_from_row(&permit_row)?;
        if permit.fencing_token != lease.fencing_token
            || permit.permit_state != "granted"
            || permit.resolved_at.is_some()
        {
            return Err(stale_store_update(
                "active lookup-table broadcast permit",
                permit_id,
            ));
        }
        let bounded_detail =
            error_detail.map(|detail| detail.chars().take(500).collect::<String>());
        let operation_row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_operations
            SET operation_state = $4,
                submitted_slot = CASE WHEN $4 = 'submitted' THEN COALESCE($5, submitted_slot) ELSE submitted_slot END,
                submitted_at = CASE WHEN $4 = 'submitted' THEN COALESCE(submitted_at, now()) ELSE submitted_at END,
                error_code = $6,
                error_detail = $7,
                next_attempt_at = now() + interval '2 seconds',
                lease_owner = NULL,
                lease_expires_at = NULL,
                updated_at = now()
            WHERE id = $1 AND operation_state = 'signed'
              AND lease_owner = $2 AND fencing_token = $3
              AND lease_expires_at > now()
              AND transaction_signature = $8 AND message_hash = $9
            RETURNING *
            "#,
        )
        .bind(operation_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(next_state.as_str())
        .bind(observed_slot)
        .bind(error_code.as_deref())
        .bind(bounded_detail.as_deref())
        .bind(&permit.transaction_signature)
        .bind(&permit.message_hash)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_provisioner_broadcast_permits
            SET permit_state = $2, resolution_detail = $3,
                resolved_at = now(), updated_at = now()
            WHERE id = $1 AND permit_state = 'granted' AND resolved_at IS NULL
            "#,
        )
        .bind(permit_id)
        .bind(permit_state)
        .bind(bounded_detail)
        .execute(&mut *tx)
        .await?;
        let operation = lookup_table_operation_from_row(&operation_row)?;
        tx.commit().await?;
        Ok(operation)
    }

    pub async fn set_lookup_table_provisioner_pause(
        &self,
        cluster: &str,
        paused: bool,
        reason: &str,
        updated_by: &str,
    ) -> Result<LookupTableProvisionerControlRecord, OrchestratorError> {
        if cluster.trim().is_empty() || reason.trim().is_empty() || updated_by.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "provisioner pause control requires non-empty cluster, reason, and updated_by"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_provisioner_controls
                (cluster, paused, reason, updated_by, control_epoch)
            VALUES ($1, FALSE, 'provisioner control initialized', $2, 0)
            ON CONFLICT (cluster) DO NOTHING
            "#,
        )
        .bind(cluster)
        .bind(updated_by)
        .execute(&mut *tx)
        .await?;
        sqlx::query(
            "SELECT cluster FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster = $1 FOR UPDATE",
        )
        .bind(cluster)
        .fetch_one(&mut *tx)
        .await?;
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_provisioner_controls
            SET paused = $2, reason = $3, updated_by = $4,
                control_epoch = control_epoch + 1, updated_at = now()
            WHERE cluster = $1
            RETURNING *
            "#,
        )
        .bind(cluster)
        .bind(paused)
        .bind(reason)
        .bind(updated_by)
        .fetch_one(&mut *tx)
        .await?;
        let control = lookup_table_provisioner_control_from_row(&row)?;
        tx.commit().await?;
        Ok(control)
    }

    pub async fn upsert_lookup_table_rollout_control(
        &self,
        cluster: &str,
        vault_id: Option<VaultId>,
        rollout_mode: LookupTableRolloutMode,
        force_legacy: bool,
        reason: Option<&str>,
        updated_by: &str,
    ) -> Result<LookupTableRolloutControl, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        acquire_lookup_table_rollout_lock(&mut tx, cluster).await?;
        let row = if let Some(vault_id) = vault_id {
            sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_rollout_controls
                    (cluster, vault_id, rollout_mode, force_legacy, reason, updated_by)
                VALUES ($1, $2, $3, $4, $5, $6)
                ON CONFLICT (cluster, vault_id) WHERE vault_id IS NOT NULL DO UPDATE SET
                    rollout_mode = EXCLUDED.rollout_mode,
                    force_legacy = EXCLUDED.force_legacy,
                    reason = EXCLUDED.reason,
                    updated_by = EXCLUDED.updated_by,
                    updated_at = now()
                RETURNING *
                "#,
            )
            .bind(cluster)
            .bind(vault_id.as_i64())
            .bind(rollout_mode.as_str())
            .bind(force_legacy)
            .bind(reason)
            .bind(updated_by)
            .fetch_one(&mut *tx)
            .await?
        } else {
            sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_rollout_controls
                    (cluster, vault_id, rollout_mode, force_legacy, reason, updated_by)
                VALUES ($1, NULL, $2, $3, $4, $5)
                ON CONFLICT (cluster) WHERE vault_id IS NULL DO UPDATE SET
                    rollout_mode = EXCLUDED.rollout_mode,
                    force_legacy = EXCLUDED.force_legacy,
                    reason = EXCLUDED.reason,
                    updated_by = EXCLUDED.updated_by,
                    updated_at = now()
                RETURNING *
                "#,
            )
            .bind(cluster)
            .bind(rollout_mode.as_str())
            .bind(force_legacy)
            .bind(reason)
            .bind(updated_by)
            .fetch_one(&mut *tx)
            .await?
        };
        let control = lookup_table_rollout_from_row(&row)?;
        tx.commit().await?;
        Ok(control)
    }

    /// Toggles the global emergency kill switch without changing the stored
    /// rollout mode. Creating the first global control defaults to legacy.
    pub async fn set_lookup_table_force_legacy(
        &self,
        cluster: &str,
        force_legacy: bool,
        reason: Option<&str>,
        updated_by: &str,
    ) -> Result<LookupTableRolloutControl, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        acquire_lookup_table_rollout_lock(&mut tx, cluster).await?;
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_rollout_controls
                (cluster, vault_id, rollout_mode, force_legacy, reason, updated_by)
            VALUES ($1, NULL, 'legacy', $2, $3, $4)
            ON CONFLICT (cluster) WHERE vault_id IS NULL DO UPDATE SET
                force_legacy = EXCLUDED.force_legacy,
                reason = EXCLUDED.reason,
                updated_by = EXCLUDED.updated_by,
                updated_at = now()
            RETURNING *
            "#,
        )
        .bind(cluster)
        .bind(force_legacy)
        .bind(reason)
        .bind(updated_by)
        .fetch_one(&mut *tx)
        .await?;
        let control = lookup_table_rollout_from_row(&row)?;
        tx.commit().await?;
        Ok(control)
    }

    /// Changes a rollout mode without implicitly clearing an active force bit.
    pub async fn set_lookup_table_rollout_mode(
        &self,
        cluster: &str,
        vault_id: Option<VaultId>,
        rollout_mode: LookupTableRolloutMode,
        reason: Option<&str>,
        updated_by: &str,
    ) -> Result<LookupTableRolloutControl, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        acquire_lookup_table_rollout_lock(&mut tx, cluster).await?;
        let row = if let Some(vault_id) = vault_id {
            sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_rollout_controls
                    (cluster, vault_id, rollout_mode, force_legacy, reason, updated_by)
                VALUES ($1, $2, $3, FALSE, $4, $5)
                ON CONFLICT (cluster, vault_id) WHERE vault_id IS NOT NULL DO UPDATE SET
                    rollout_mode = EXCLUDED.rollout_mode,
                    reason = EXCLUDED.reason,
                    updated_by = EXCLUDED.updated_by,
                    updated_at = now()
                RETURNING *
                "#,
            )
            .bind(cluster)
            .bind(vault_id.as_i64())
            .bind(rollout_mode.as_str())
            .bind(reason)
            .bind(updated_by)
            .fetch_one(&mut *tx)
            .await?
        } else {
            sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_rollout_controls
                    (cluster, vault_id, rollout_mode, force_legacy, reason, updated_by)
                VALUES ($1, NULL, $2, FALSE, $3, $4)
                ON CONFLICT (cluster) WHERE vault_id IS NULL DO UPDATE SET
                    rollout_mode = EXCLUDED.rollout_mode,
                    reason = EXCLUDED.reason,
                    updated_by = EXCLUDED.updated_by,
                    updated_at = now()
                RETURNING *
                "#,
            )
            .bind(cluster)
            .bind(rollout_mode.as_str())
            .bind(reason)
            .bind(updated_by)
            .fetch_one(&mut *tx)
            .await?
        };
        let control = lookup_table_rollout_from_row(&row)?;
        tx.commit().await?;
        Ok(control)
    }

    pub async fn effective_lookup_table_rollout(
        &self,
        cluster: &str,
        vault_id: VaultId,
    ) -> Result<EffectiveLookupTableRollout, OrchestratorError> {
        let rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.lookup_table_rollout_controls
            WHERE cluster = $1 AND (vault_id IS NULL OR vault_id = $2)
            ORDER BY vault_id NULLS FIRST
            "#,
        )
        .bind(cluster)
        .bind(vault_id.as_i64())
        .fetch_all(self.pool())
        .await?;
        let mut global = None;
        let mut vault = None;
        for row in &rows {
            let control = lookup_table_rollout_from_row(row)?;
            if control.vault_id.is_some() {
                vault = Some(control);
            } else {
                global = Some(control);
            }
        }
        let rollout_mode = vault
            .as_ref()
            .or(global.as_ref())
            .map(|control| control.rollout_mode)
            .unwrap_or(LookupTableRolloutMode::Legacy);
        let force_legacy = global.as_ref().is_some_and(|control| control.force_legacy)
            || vault.as_ref().is_some_and(|control| control.force_legacy);
        Ok(EffectiveLookupTableRollout {
            rollout_mode,
            force_legacy,
            global,
            vault,
        })
    }

    pub async fn lookup_table_control_plane_snapshot(
        &self,
        cluster: &str,
    ) -> Result<Value, OrchestratorError> {
        let snapshot = sqlx::query_scalar::<_, Value>(
            r#"
            SELECT jsonb_build_object(
                'shared_market_catalog', COALESCE((
                    SELECT jsonb_build_object(
                        'family_id', family.id,
                        'family_logical_name', family.logical_name,
                        'catalog_revision_id', revision.id,
                        'catalog_revision', revision.catalog_revision,
                        'catalog_version', revision.catalog_version,
                        'desired_set_hash', revision.desired_set_hash,
                        'enabled_mints_hash', revision.enabled_mints_hash,
                        'reserve_set_hash', revision.reserve_set_hash,
                        'address_count', revision.address_count,
                        'source_slot', revision.source_slot,
                        'source_observed_at', revision.source_observed_at,
                        'target_generation', head.target_generation,
                        'active_generation', family.active_generation,
                        'readiness_state', head.readiness_state,
                        'activated_at', head.activated_at,
                        'expected_authority', family.provisioning_authority,
                        'payer', family.payer,
                        'physical_table_count', (
                            SELECT count(*)
                            FROM loyal_yield.route_lookup_tables route_table
                            WHERE route_table.family_id = family.id
                              AND route_table.generation = family.active_generation
                              AND route_table.allocation_kind = 'shared_market'
                              AND route_table.desired_state NOT IN ('deactivated', 'closed', 'failed')
                        ),
                        'physical_address_count', COALESCE((
                            SELECT sum(route_table.address_count)
                            FROM loyal_yield.route_lookup_tables route_table
                            WHERE route_table.family_id = family.id
                              AND route_table.generation = family.active_generation
                              AND route_table.allocation_kind = 'shared_market'
                              AND route_table.desired_state NOT IN ('deactivated', 'closed', 'failed')
                        ), 0),
                        'usable_address_count', COALESCE((
                            SELECT sum(route_table.usable_address_count)
                            FROM loyal_yield.route_lookup_tables route_table
                            WHERE route_table.family_id = family.id
                              AND route_table.generation = family.active_generation
                              AND route_table.allocation_kind = 'shared_market'
                              AND route_table.desired_state NOT IN ('deactivated', 'closed', 'failed')
                        ), 0),
                        'last_verified_slot', (
                            SELECT min(route_table.last_verified_slot)
                            FROM loyal_yield.route_lookup_tables route_table
                            WHERE route_table.family_id = family.id
                              AND route_table.generation = family.active_generation
                              AND route_table.allocation_kind = 'shared_market'
                              AND route_table.desired_state NOT IN ('deactivated', 'closed', 'failed')
                        )
                    )
                    FROM loyal_yield.lookup_table_shared_market_catalog_heads head
                    JOIN loyal_yield.lookup_table_shared_market_catalog_revisions revision
                      ON revision.id = head.catalog_revision_id
                    JOIN loyal_yield.lookup_table_families family
                      ON family.id = head.family_id
                    WHERE family.cluster = $1
                ), '{}'::jsonb),
                'provisioning_requests', COALESCE((
                    SELECT jsonb_build_object(
                        'depth', count(*) FILTER (
                            WHERE request.request_status IN ('requested', 'planning', 'queued', 'failed')
                        ),
                        'oldest_requested_at', min(request.requested_at) FILTER (
                            WHERE request.request_status IN ('requested', 'planning', 'queued', 'failed')
                        ),
                        'oldest_age_seconds', COALESCE(floor(extract(epoch FROM now() - (
                            min(request.requested_at) FILTER (
                                WHERE request.request_status IN ('requested', 'planning', 'queued', 'failed')
                            )
                        )))::BIGINT, 0),
                        'max_attempt_count', COALESCE(max(request.attempt_count), 0),
                        'by_status', COALESCE((
                            SELECT jsonb_object_agg(request_status, status_count)
                            FROM (
                                SELECT request_status, count(*) AS status_count
                                FROM loyal_yield.lookup_table_provisioning_requests
                                WHERE cluster = $1
                                GROUP BY request_status
                            ) grouped_requests
                        ), '{}'::jsonb)
                    )
                    FROM loyal_yield.lookup_table_provisioning_requests request
                    WHERE request.cluster = $1
                ), '{}'::jsonb),
                'readiness', jsonb_build_object(
                    'active_vault_count', (
                        SELECT count(*) FROM loyal_yield.managed_vaults vault
                        WHERE vault.active = TRUE
                    ),
                    'ready_active_vault_count', (
                        SELECT count(*) FROM loyal_yield.managed_vaults vault
                        WHERE vault.active = TRUE AND EXISTS (
                            SELECT 1
                            FROM loyal_yield.lookup_table_route_readiness_current active_readiness
                            WHERE active_readiness.cluster = $1
                              AND active_readiness.vault_id = vault.id
                        ) AND NOT EXISTS (
                            SELECT 1
                            FROM loyal_yield.lookup_table_route_readiness_current blocked_readiness
                            WHERE blocked_readiness.cluster = $1
                              AND blocked_readiness.vault_id = vault.id
                              AND NOT (
                                  blocked_readiness.readiness_state = 'ready'
                                  AND blocked_readiness.selection_kind = 'reusable'
                                  AND blocked_readiness.covered_address_count = blocked_readiness.required_address_count
                                  AND blocked_readiness.missing_addresses = '[]'::jsonb
                              )
                        )
                    ),
                    'ready_active_vault_percent', CASE
                        WHEN (SELECT count(*) FROM loyal_yield.managed_vaults vault WHERE vault.active = TRUE) = 0
                        THEN 0
                        ELSE round(100.0 * (
                            SELECT count(*) FROM loyal_yield.managed_vaults vault
                            WHERE vault.active = TRUE AND EXISTS (
                                SELECT 1
                                FROM loyal_yield.lookup_table_route_readiness_current active_readiness
                                WHERE active_readiness.cluster = $1
                                  AND active_readiness.vault_id = vault.id
                            ) AND NOT EXISTS (
                                SELECT 1
                                FROM loyal_yield.lookup_table_route_readiness_current blocked_readiness
                                WHERE blocked_readiness.cluster = $1
                                  AND blocked_readiness.vault_id = vault.id
                                  AND NOT (
                                      blocked_readiness.readiness_state = 'ready'
                                      AND blocked_readiness.selection_kind = 'reusable'
                                      AND blocked_readiness.covered_address_count = blocked_readiness.required_address_count
                                      AND blocked_readiness.missing_addresses = '[]'::jsonb
                                  )
                            )
                        ) / (SELECT count(*) FROM loyal_yield.managed_vaults vault WHERE vault.active = TRUE), 2)
                    END,
                    'vault_count', count(DISTINCT readiness.vault_id),
                    'ready_vault_count', count(DISTINCT readiness.vault_id)
                        FILTER (WHERE readiness.readiness_state = 'ready'
                                  AND readiness.selection_kind = 'reusable'
                                  AND readiness.covered_address_count = readiness.required_address_count
                                  AND readiness.missing_addresses = '[]'::jsonb),
                    'ready_percent', CASE WHEN count(DISTINCT readiness.vault_id) = 0 THEN 0
                        ELSE round(100.0 * count(DISTINCT readiness.vault_id)
                            FILTER (WHERE readiness.readiness_state = 'ready'
                                      AND readiness.selection_kind = 'reusable'
                                      AND readiness.covered_address_count = readiness.required_address_count
                                      AND readiness.missing_addresses = '[]'::jsonb)
                            / count(DISTINCT readiness.vault_id), 2) END,
                    'by_state', COALESCE((
                        SELECT jsonb_object_agg(state, state_count)
                        FROM (
                            SELECT readiness_state AS state, count(*) AS state_count
                            FROM loyal_yield.lookup_table_route_readiness_current
                            WHERE cluster = $1 GROUP BY readiness_state
                        ) grouped_readiness
                    ), '{}'::jsonb),
                    'legacy_fallback_count', count(*) FILTER (
                        WHERE readiness.selection_kind = 'legacy'
                          AND readiness.fallback_reason IS NOT NULL
                    ),
                    'fallback_by_reason', COALESCE((
                        SELECT jsonb_object_agg(fallback_reason, fallback_count)
                        FROM (
                            SELECT fallback_reason, count(*) AS fallback_count
                            FROM loyal_yield.lookup_table_route_readiness_current
                            WHERE cluster = $1 AND fallback_reason IS NOT NULL
                            GROUP BY fallback_reason
                        ) grouped_fallbacks
                    ), '{}'::jsonb),
                    'blocker_count', count(*) FILTER (
                        WHERE readiness.selection_kind = 'blocked'
                           OR readiness.readiness_state IN ('incomplete', 'failed')
                    )
                ),
                'blockers', COALESCE((
                    SELECT jsonb_agg(blocker ORDER BY blocker_updated_at DESC)
                    FROM (
                        SELECT readiness.updated_at AS blocker_updated_at,
                               jsonb_build_object(
                                   'vault_id', readiness.vault_id,
                                   'route_fingerprint', readiness.route_fingerprint,
                                   'requirements_fingerprint', readiness.requirements_fingerprint,
                                   'readiness_state', readiness.readiness_state,
                                   'selection_kind', readiness.selection_kind,
                                   'fallback_reason', readiness.fallback_reason,
                                   'missing_addresses', readiness.missing_addresses,
                                   'compiled_message_size', readiness.compiled_message_size,
                                   'selected_table_count', readiness.selected_table_count,
                                   'packet_fits', readiness.packet_fits,
                                   'updated_at', readiness.updated_at
                               ) AS blocker
                        FROM loyal_yield.lookup_table_route_readiness_current readiness
                        WHERE readiness.cluster = $1
                          AND (readiness.selection_kind = 'blocked'
                               OR readiness.readiness_state IN ('incomplete', 'failed'))
                        ORDER BY readiness.updated_at DESC
                        LIMIT 100
                    ) recent_blockers
                ), '[]'::jsonb),
                'recent_compilations', COALESCE((
                    SELECT jsonb_agg(compilation ORDER BY compilation_updated_at DESC)
                    FROM (
                        SELECT readiness.updated_at AS compilation_updated_at,
                               jsonb_build_object(
                                   'vault_id', readiness.vault_id,
                                   'route_fingerprint', readiness.route_fingerprint,
                                   'selection_kind', readiness.selection_kind,
                                   'selected_table_ids', readiness.selected_table_ids,
                                   'selected_table_count', readiness.selected_table_count,
                                   'compiled_message_size', readiness.compiled_message_size,
                                   'packet_limit', readiness.packet_limit,
                                   'packet_fits', readiness.packet_fits,
                                   'simulation_state', readiness.simulation_state,
                                   'simulation_units_consumed', readiness.simulation_units_consumed,
                                   'simulation_error', readiness.simulation_error,
                                   'observed_slot', readiness.observed_slot,
                                   'updated_at', readiness.updated_at
                               ) AS compilation
                        FROM loyal_yield.lookup_table_route_readiness_current readiness
                        WHERE readiness.cluster = $1
                          AND readiness.compiled_message_size IS NOT NULL
                        ORDER BY readiness.updated_at DESC
                        LIMIT 100
                    ) recent_compilation_rows
                ), '[]'::jsonb),
                'queue', COALESCE((
                    SELECT jsonb_build_object(
                        'depth', count(*) FILTER (WHERE operation.operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')),
                        'oldest_created_at', min(operation.created_at) FILTER (WHERE operation.operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')),
                        'oldest_age_seconds', COALESCE(floor(extract(epoch FROM now() - (
                            min(operation.created_at) FILTER (WHERE operation.operation_state NOT IN ('complete', 'permanent_failure', 'cancelled'))
                        )))::BIGINT, 0),
                        'max_attempt_count', COALESCE(max(operation.attempt_count), 0),
                        'permanent_failures', count(*) FILTER (
                            WHERE operation.operation_state = 'permanent_failure'
                              AND NOT EXISTS (
                                  SELECT 1
                                  FROM loyal_yield.lookup_table_terminal_repair_operations repaired
                                  WHERE repaired.operation_id = operation.id
                              )
                        ),
                        'needs_reconcile', count(*) FILTER (WHERE operation.operation_state = 'needs_reconcile')
                    )
                    FROM loyal_yield.lookup_table_operations operation
                    JOIN loyal_yield.lookup_table_families family ON family.id = operation.family_id
                    WHERE family.cluster = $1
                ), '{}'::jsonb),
                'terminal_failures', COALESCE((
                    SELECT jsonb_agg(failure ORDER BY failure_updated_at DESC)
                    FROM (
                        SELECT operation.updated_at AS failure_updated_at,
                               jsonb_build_object(
                                   'operation_id', operation.id,
                                   'table_id', operation.route_lookup_table_id,
                                   'table_address', route_table.table_address,
                                   'operation_kind', operation.operation_kind,
                                   'error_code', operation.error_code,
                                   'error_detail', regexp_replace(
                                       left(COALESCE(operation.error_detail, ''), 500),
                                       '(postgres(ql)?|https?)://[^[:space:]]+',
                                       '[redacted-url]', 'gi'
                                   ),
                                   'attempt_count', operation.attempt_count,
                                   'updated_at', operation.updated_at
                               ) AS failure
                        FROM loyal_yield.lookup_table_operations operation
                        JOIN loyal_yield.lookup_table_families family ON family.id = operation.family_id
                        LEFT JOIN loyal_yield.route_lookup_tables route_table
                          ON route_table.id = operation.route_lookup_table_id
                        WHERE family.cluster = $1
                          AND operation.operation_state = 'permanent_failure'
                          AND NOT EXISTS (
                              SELECT 1
                              FROM loyal_yield.lookup_table_terminal_repair_operations repaired
                              WHERE repaired.operation_id = operation.id
                          )
                        ORDER BY operation.updated_at DESC
                        LIMIT 100
                    ) recent_terminal_failures
                ), '[]'::jsonb),
                'drift', COALESCE((
                    SELECT jsonb_agg(drift_record ORDER BY drift_updated_at DESC)
                    FROM (
                        SELECT operation.updated_at AS drift_updated_at,
                               jsonb_build_object(
                                   'operation_id', operation.id,
                                   'operation_state', operation.operation_state,
                                   'operation_kind', operation.operation_kind,
                                   'table_id', route_table.id,
                                   'table_address', route_table.table_address,
                                   'expected_authority', route_table.authority,
                                   'address_hash', route_table.address_hash,
                                   'mutation_epoch', route_table.mutation_epoch,
                                   'error_code', operation.error_code,
                                   'error_detail', regexp_replace(
                                       left(COALESCE(operation.error_detail, ''), 500),
                                       '(postgres(ql)?|https?)://[^[:space:]]+',
                                       '[redacted-url]', 'gi'
                                   ),
                                   'attempt_count', operation.attempt_count,
                                   'updated_at', operation.updated_at
                               ) AS drift_record
                        FROM loyal_yield.lookup_table_operations operation
                        JOIN loyal_yield.lookup_table_families family ON family.id = operation.family_id
                        LEFT JOIN loyal_yield.route_lookup_tables route_table
                          ON route_table.id = operation.route_lookup_table_id
                        WHERE family.cluster = $1
                          AND operation.operation_state IN ('needs_reconcile', 'permanent_failure')
                          AND (
                              operation.operation_state <> 'permanent_failure'
                              OR NOT EXISTS (
                                  SELECT 1
                                  FROM loyal_yield.lookup_table_terminal_repair_operations repaired
                                  WHERE repaired.operation_id = operation.id
                              )
                          )
                        ORDER BY operation.updated_at DESC
                        LIMIT 100
                    ) recent_drift
                ), '[]'::jsonb),
                'tables', COALESCE((
                    SELECT jsonb_agg(jsonb_build_object(
                        'id', route_table.id,
                        'address', route_table.table_address,
                        'family_id', route_table.family_id,
                        'family_logical_name', family.logical_name,
                        'family_kind', family.kind,
                        'expected_authority', route_table.authority,
                        'generation', route_table.generation,
                        'shard_ordinal', route_table.shard_ordinal,
                        'state', route_table.desired_state,
                        'address_count', route_table.address_count,
                        'address_hash', route_table.address_hash,
                        'mutation_epoch', route_table.mutation_epoch,
                        'last_verified_slot', route_table.last_verified_slot,
                        'last_verified_at', route_table.last_verified_at,
                        'usable_address_count', route_table.usable_address_count,
                        'reserved_address_count', route_table.reserved_address_count,
                        'allocation_high_water', route_table.allocation_high_water,
                        'headroom', route_table.allocation_high_water - route_table.reserved_address_count,
                        'fragmentation', GREATEST(route_table.reserved_address_count - route_table.address_count, 0),
                        'bound_vault_count', (
                            SELECT count(DISTINCT binding.vault_id)
                            FROM loyal_yield.lookup_table_vault_bindings binding
                            WHERE binding.route_lookup_table_id = route_table.id
                              AND binding.lifecycle_state IN ('preparing', 'warming', 'active', 'standby', 'retiring')
                        ),
                        'reclaimed_lamports', route_table.reclaimed_lamports,
                        'drifted', route_table.desired_state = 'failed'
                    ) ORDER BY route_table.family_id, route_table.generation, route_table.shard_ordinal)
                    FROM loyal_yield.route_lookup_tables route_table
                    JOIN loyal_yield.lookup_table_families family ON family.id = route_table.family_id
                    WHERE family.cluster = $1
                ), '[]'::jsonb),
                'rollout_controls', COALESCE((
                    SELECT jsonb_agg(jsonb_build_object(
                        'vault_id', vault_id,
                        'mode', rollout_mode,
                        'force_legacy', force_legacy,
                        'reason', reason,
                        'updated_by', updated_by,
                        'updated_at', updated_at
                    ) ORDER BY vault_id NULLS FIRST)
                    FROM loyal_yield.lookup_table_rollout_controls
                    WHERE cluster = $1
                ), '[]'::jsonb),
                'lamports', COALESCE((
                    SELECT jsonb_build_object(
                        'estimated_fee', COALESCE(sum(estimated_fee_lamports), 0),
                        'estimated_rent', COALESCE(sum(estimated_rent_lamports), 0),
                        'actual_fee', COALESCE(sum(actual_fee_lamports), 0),
                        'actual_rent', COALESCE(sum(actual_rent_lamports), 0),
                        'reclaimed_rent', COALESCE(sum(reclaimed_rent_lamports), 0)
                    )
                    FROM loyal_yield.lookup_table_operations operation
                    JOIN loyal_yield.lookup_table_families family ON family.id = operation.family_id
                    WHERE family.cluster = $1
                ), '{}'::jsonb)
            )
            FROM loyal_yield.lookup_table_route_readiness_current readiness
            WHERE readiness.cluster = $1
            "#,
        )
        .bind(cluster)
        .fetch_one(self.pool())
        .await?;
        Ok(snapshot)
    }
}
