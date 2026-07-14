use crate::{NeonSqlClient, OrchestratorError, VaultId};
use chrono::{DateTime, Utc};
pub use loyal_actions::{
    compiler_lookup_eligible_addresses, LookupTableAccountAccess, LookupTableAccountProvenance,
    LookupTableManifest, LookupTableManifestError, MustRemainStatic, MustRemainStaticReason,
    SharedMarket, SharedMarketRole, Vault, VaultRole,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use solana_sdk::{address_lookup_table::instruction::derive_lookup_table_address, pubkey::Pubkey};
use sqlx::{Postgres, QueryBuilder, Row};
use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
    str::FromStr,
};
use thiserror::Error;

pub const LOOKUP_TABLE_HARD_CAPACITY: u16 = 256;

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
    desired: &BTreeSet<String>,
    confirmed: &BTreeSet<String>,
    pending: &BTreeSet<String>,
    max_extension_addresses: usize,
) -> Option<(LookupTableOperationKind, Vec<String>)> {
    if !pending.is_empty() {
        return None;
    }
    let occupied = confirmed.union(pending).cloned().collect::<BTreeSet<_>>();
    let missing = desired
        .difference(&occupied)
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
    projected_capacity_commitment: u16,
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
            let reserved_capacity = u16::try_from(reserved_capacity).ok()?;
            let reservation_delta = u16::try_from(reservation_delta).ok()?;
            Some((
                PackedShardScore {
                    new_address_count: missing_addresses.len(),
                    projected_capacity_commitment,
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

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum LookupTableBundleKind {
    Legacy,
    Reusable,
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
    pub kind: LookupTableBundleKind,
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

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolverSelectionInput {
    pub rollout_mode: LookupTableRolloutMode,
    pub force_legacy: bool,
    pub legacy: Option<ResolvedLookupTableBundle>,
    pub reusable: Option<ResolvedLookupTableBundle>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ResolverSelection {
    Execute(ResolvedLookupTableBundle),
    Shadow {
        execute: ResolvedLookupTableBundle,
        reusable_evidence: Option<ResolvedLookupTableBundle>,
    },
    Blocked {
        reason: &'static str,
    },
}

pub fn select_lookup_table_bundle(input: ResolverSelectionInput) -> ResolverSelection {
    let complete_legacy = input.legacy.filter(ResolvedLookupTableBundle::ready);
    let complete_reusable = input
        .reusable
        .clone()
        .filter(ResolvedLookupTableBundle::ready);

    if input.force_legacy {
        return complete_legacy.map_or(
            ResolverSelection::Blocked {
                reason: "global force-legacy is active but complete legacy coverage is unavailable",
            },
            ResolverSelection::Execute,
        );
    }

    match input.rollout_mode {
        LookupTableRolloutMode::Legacy => complete_legacy.map_or(
            ResolverSelection::Blocked {
                reason: "legacy rollout mode requires complete legacy coverage",
            },
            ResolverSelection::Execute,
        ),
        LookupTableRolloutMode::Shadow => complete_legacy.map_or(
            ResolverSelection::Blocked {
                reason: "shadow rollout mode requires complete authoritative legacy coverage",
            },
            |execute| ResolverSelection::Shadow {
                execute,
                reusable_evidence: input.reusable,
            },
        ),
        LookupTableRolloutMode::PreferReusable => {
            if let Some(reusable) = complete_reusable {
                ResolverSelection::Execute(reusable)
            } else {
                complete_legacy.map_or(
                    ResolverSelection::Blocked {
                        reason: "neither reusable nor legacy coverage is complete",
                    },
                    ResolverSelection::Execute,
                )
            }
        }
        LookupTableRolloutMode::ReusableOnly => complete_reusable.map_or(
            ResolverSelection::Blocked {
                reason: "reusable-only rollout mode requires complete reusable coverage",
            },
            ResolverSelection::Execute,
        ),
    }
}

pub fn minimal_verified_table_bundle(
    required_addresses: &BTreeSet<String>,
    candidates: &[ResolverTableCandidate],
    exact_search_limit: usize,
) -> Result<(Vec<ResolverTableCandidate>, BTreeSet<String>), LookupTableDomainError> {
    let (candidates, missing) =
        persisted_relevant_table_candidates(required_addresses, candidates, exact_search_limit)?;

    let mut best: Option<(Vec<usize>, usize)> = None;
    let mut selected = Vec::new();
    search_table_subsets(0, &candidates, required_addresses, &mut selected, &mut best);
    if let Some((indexes, _)) = best {
        return Ok((
            indexes
                .into_iter()
                .map(|index| candidates[index].clone())
                .collect(),
            BTreeSet::new(),
        ));
    }

    Ok((Vec::new(), missing))
}

/// Returns every persisted-eligible candidate that can contribute to this
/// route. Runtime must RPC-verify this bounded set before exact minimization;
/// preselecting a single persisted bundle would make one drifted overlap hide a
/// healthy alternative.
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
    candidates.sort_by(|left, right| {
        left.table_address
            .cmp(&right.table_address)
            .then_with(|| left.table_id.cmp(&right.table_id))
    });
    if candidates.len() > exact_search_limit {
        return Err(LookupTableDomainError::TooManyResolverCandidates {
            actual: candidates.len(),
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

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EffectiveLookupTableRollout {
    pub rollout_mode: LookupTableRolloutMode,
    pub force_legacy: bool,
    pub global: Option<LookupTableRolloutControl>,
    pub vault: Option<LookupTableRolloutControl>,
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

impl NeonSqlClient {
    /// Returns true when an ALT manager identity is already entrusted with
    /// route execution or pays for a durable legacy route table.
    pub async fn lookup_table_manager_identity_overlaps_control_plane(
        &self,
        manager: &str,
    ) -> Result<bool, OrchestratorError> {
        Ok(sqlx::query_scalar::<_, bool>(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.route_policies
                WHERE active = TRUE
                  AND ($1 = authority OR $1 = ANY(delegated_signers))
                UNION ALL
                SELECT 1
                FROM loyal_yield.balance_sweep_targets
                WHERE active = TRUE
                  AND ($1 = authority OR $1 = wallet OR $1 = ANY(delegated_signers))
                UNION ALL
                SELECT 1
                FROM loyal_yield.route_lookup_tables
                WHERE family_id IS NULL AND durable = TRUE AND payer = $1
            )
            "#,
        )
        .bind(manager)
        .fetch_one(self.pool())
        .await?)
    }

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
        if self
            .lookup_table_manager_identity_overlaps_control_plane(&input.provisioning_authority)
            .await?
            || (input.payer != input.provisioning_authority
                && self
                    .lookup_table_manager_identity_overlaps_control_plane(&input.payer)
                    .await?)
        {
            return Err(OrchestratorError::StoreInvariant(
                "ALT manager authority/payer overlaps a durable route signer, authority, wallet, or legacy route payer"
                    .to_owned(),
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
        let desired = addresses(&["a", "b", "c", "d"]);
        let confirmed = addresses(&["a", "b"]);
        let (kind, missing) =
            next_shared_market_mutation(true, &desired, &confirmed, &BTreeSet::new(), 1).unwrap();
        assert_eq!(kind, LookupTableOperationKind::Extend);
        assert_eq!(missing, vec!["c".to_owned()]);
        assert!(
            next_shared_market_mutation(true, &desired, &confirmed, &addresses(&["c"]), 1,)
                .is_none()
        );
        let (kind, _) =
            next_shared_market_mutation(false, &desired, &BTreeSet::new(), &BTreeSet::new(), 1)
                .unwrap();
        assert_eq!(kind, LookupTableOperationKind::Create);
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
            desired_address_hash: "manifest".to_owned(),
            addresses: vec!["b".to_owned(), "a".to_owned(), "a".to_owned()],
        };
        let first_key = first.idempotency_key();
        first.addresses.reverse();
        assert_eq!(first_key, first.idempotency_key());
        first.desired_address_hash = "changed".to_owned();
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

    fn bundle(kind: LookupTableBundleKind, ready: bool) -> ResolvedLookupTableBundle {
        ResolvedLookupTableBundle {
            kind,
            tables: vec![resolver_candidate(1, &["a"], ready)],
            required_addresses: addresses(&["a"]),
            missing_addresses: BTreeSet::new(),
            packet_fits: true,
            simulation_succeeded: true,
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
            kind: LookupTableBundleKind::Reusable,
            tables: selected,
            required_addresses: required,
            missing_addresses: BTreeSet::new(),
            packet_fits: true,
            simulation_succeeded: true,
        };
        assert!(!not_rpc_verified.ready());
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
    fn reusable_alt_rollout_force_legacy_and_prefer_fallback_are_deterministic() {
        let legacy = bundle(LookupTableBundleKind::Legacy, true);
        let reusable = bundle(LookupTableBundleKind::Reusable, true);
        let selected = select_lookup_table_bundle(ResolverSelectionInput {
            rollout_mode: LookupTableRolloutMode::ReusableOnly,
            force_legacy: true,
            legacy: Some(legacy.clone()),
            reusable: Some(reusable),
        });
        assert!(matches!(
            selected,
            ResolverSelection::Execute(ResolvedLookupTableBundle {
                kind: LookupTableBundleKind::Legacy,
                ..
            })
        ));

        let selected = select_lookup_table_bundle(ResolverSelectionInput {
            rollout_mode: LookupTableRolloutMode::PreferReusable,
            force_legacy: false,
            legacy: Some(legacy),
            reusable: Some(bundle(LookupTableBundleKind::Reusable, false)),
        });
        assert!(matches!(
            selected,
            ResolverSelection::Execute(ResolvedLookupTableBundle {
                kind: LookupTableBundleKind::Legacy,
                ..
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
    /// Locks the family and candidate rows, re-runs allocation from durable
    /// reservations, and writes the binding plus one exact-transaction outbox
    /// operation before releasing the transaction.
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
            "SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 AND cluster = $2 FOR UPDATE",
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
        if let Some(binding) = &active_binding {
            if binding.manifest_id == request.manifest_id
                && binding.desired_head_revision == desired_head_revision
            {
                return Ok(AtomicVaultAllocationResult::Existing {
                    binding: binding.clone(),
                });
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

        let physical_rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.route_lookup_tables
            WHERE family_id = $1
              AND allocation_kind IN ('vault_shard', 'dedicated_vault')
              AND desired_state IN ('preparing', 'warming', 'active')
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
                lifecycle: table.desired_state,
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
        let mut tx = self.pool().begin().await?;
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
        let vault_manifest = resolve_or_persist_request_manifest_in_tx(
            &mut *tx,
            cluster,
            &request,
            LookupTableManifestSubject::Vault,
            source_slot,
        )
        .await?;
        let shared_manifest_id = shared_manifest.id;
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

        let shared_family_id = shared_manifest.family_id;
        let (shared_target_generation, shared_operations) = self
            .plan_shared_market_operations_in_connection(
                &mut *tx,
                cluster,
                shared_family_id,
                shared_manifest_id,
                policy.shared_shard_capacity,
                policy.max_extension_addresses,
                policy.operation_context.clone(),
                policy.estimated_fee_lamports,
                policy.estimated_rent_lamports,
            )
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
            sqlx::query("SELECT * FROM loyal_yield.lookup_table_families WHERE id = $1 FOR UPDATE")
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
        .bind(shared_manifest_id)
        .bind(vault_manifest_id)
        .fetch_one(&mut *tx)
        .await?;
        let shared_ready: bool = sqlx::query_scalar(
            r#"
            SELECT CASE WHEN $3 = 0 THEN TRUE ELSE
                COALESCE(family.active_generation = $2, FALSE)
                AND EXISTS (
                    SELECT 1 FROM loyal_yield.route_lookup_tables route_table
                    WHERE route_table.family_id = $1 AND route_table.generation = $2
                      AND route_table.allocation_kind = 'shared_market'
                )
                AND COALESCE((
                    SELECT bool_and(
                        route_table.desired_state = 'active'
                        AND route_table.usable_address_count = route_table.address_count
                        AND route_table.last_verified_slot IS NOT NULL
                    )
                    FROM loyal_yield.route_lookup_tables route_table
                    WHERE route_table.family_id = $1 AND route_table.generation = $2
                      AND route_table.allocation_kind = 'shared_market'
                ), FALSE)
            END
            FROM loyal_yield.lookup_table_families family
            WHERE family.id = $1
            "#,
        )
        .bind(shared_family_id)
        .bind(shared_target_generation)
        .bind(shared_manifest.address_count)
        .fetch_one(&mut *tx)
        .await?;
        let request_satisfied = provisioning_request_is_satisfied(
            shared_ready,
            shared_operations.len(),
            pending_operation_count,
            &vault_allocation,
        );
        let request_row = sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_provisioning_requests
            SET request_status = CASE WHEN $4 THEN 'satisfied' ELSE 'queued' END,
                satisfied_at = CASE WHEN $4 THEN now() ELSE satisfied_at END,
                lease_owner = NULL,
                lease_expires_at = NULL, error_code = NULL, error_detail = NULL,
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
        family_id: i64,
        manifest_id: i64,
        requested_shard_capacity: u16,
        max_extension_addresses: usize,
        operation_context: Value,
        estimated_fee_lamports: Option<i64>,
        estimated_rent_lamports: Option<i64>,
    ) -> Result<(i32, Vec<LookupTableOperationRecord>), OrchestratorError> {
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
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "lookup-table family {family_id} is not an active shared-market family"
            )));
        }
        let shard_capacity = requested_shard_capacity
            .min(family.allocation_high_water as u16)
            .min(family.hard_capacity as u16);
        let cohort_rows = sqlx::query(
            r#"
            SELECT DISTINCT ON (manifest.subject_key) manifest.id, manifest.subject_key
            FROM loyal_yield.lookup_table_manifests manifest
            JOIN loyal_yield.lookup_table_provisioning_requests request
              ON request.shared_manifest_id = manifest.id
            WHERE manifest.family_id = $1 AND manifest.subject_kind = 'shared_market'
              AND manifest.sealed_at IS NOT NULL
              AND manifest.planner_version = $2 AND manifest.catalog_version = $3
              AND request.sealed_at IS NOT NULL
              AND request.request_status <> 'cancelled'
            ORDER BY manifest.subject_key, manifest.created_at DESC, manifest.id DESC
            "#,
        )
        .bind(family_id)
        .bind(&family.planner_version)
        .bind(&family.catalog_version)
        .fetch_all(&mut *tx)
        .await?;
        let mut cohorts = Vec::new();
        let mut referenced_manifest_is_current = false;
        for row in cohort_rows {
            let cohort_manifest_id: i64 = row.try_get("id")?;
            referenced_manifest_is_current |= cohort_manifest_id == manifest_id;
            let cohort_addresses = sqlx::query_scalar::<_, String>(
                "SELECT address FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id = $1 ORDER BY ordinal",
            )
            .bind(cohort_manifest_id)
            .fetch_all(&mut *tx)
            .await?
            .into_iter()
            .collect();
            cohorts.push(SharedMarketRouteCohort {
                cohort_key: row.try_get("subject_key")?,
                addresses: cohort_addresses,
            });
        }
        if cohorts.is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "shared-market family has no current sealed manifest cohorts".to_owned(),
            ));
        }
        if !referenced_manifest_is_current {
            return Err(OrchestratorError::StoreInvariant(format!(
                "shared-market manifest {manifest_id} is not current for the family planner/catalog"
            )));
        }
        let shard_plan =
            plan_shared_market_shards(&cohorts, shard_capacity).map_err(domain_store_error)?;
        let active_generation = family.active_generation.unwrap_or_default();
        let physical_rows = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.route_lookup_tables
            WHERE family_id = $1 AND generation = $2
              AND allocation_kind = 'shared_market'
              AND desired_state NOT IN ('deactivated', 'closed', 'failed')
            ORDER BY shard_ordinal FOR UPDATE
            "#,
        )
        .bind(family_id)
        .bind(active_generation)
        .fetch_all(&mut *tx)
        .await?;
        let mut physical = physical_rows
            .iter()
            .map(reusable_lookup_table_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let mut confirmed = BTreeMap::<i32, BTreeSet<String>>::new();
        let mut pending = BTreeMap::<i32, BTreeSet<String>>::new();
        for table in &physical {
            confirmed.insert(
                table.shard_ordinal,
                sqlx::query_scalar::<_, String>(
                    "SELECT address FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id = $1 ORDER BY ordinal",
                )
                .bind(table.id)
                .fetch_all(&mut *tx)
                .await?
                .into_iter()
                .collect(),
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
                      AND operation.operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
                    ORDER BY address.ordinal
                    "#,
                )
                .bind(table.id)
                .fetch_all(&mut *tx)
                .await?
                .into_iter()
                .collect(),
            );
        }
        let requires_rollover = physical.len() > shard_plan.len()
            || physical.iter().any(|table| {
                let desired = shard_plan
                    .get(table.shard_ordinal as usize)
                    .map(|shard| shard.addresses.iter().cloned().collect::<BTreeSet<_>>())
                    .unwrap_or_default();
                !confirmed
                    .get(&table.shard_ordinal)
                    .cloned()
                    .unwrap_or_default()
                    .is_subset(&desired)
            });
        let target_generation = if requires_rollover {
            active_generation.saturating_add(1)
        } else {
            active_generation
        };
        if requires_rollover {
            let target_rows = sqlx::query(
                r#"
                SELECT * FROM loyal_yield.route_lookup_tables
                WHERE family_id = $1 AND generation = $2
                  AND allocation_kind = 'shared_market'
                  AND desired_state NOT IN ('deactivated', 'closed', 'failed')
                ORDER BY shard_ordinal FOR UPDATE
                "#,
            )
            .bind(family_id)
            .bind(target_generation)
            .fetch_all(&mut *tx)
            .await?;
            physical = target_rows
                .iter()
                .map(reusable_lookup_table_from_row)
                .collect::<Result<Vec<_>, _>>()?;
            confirmed.clear();
            pending.clear();
            for table in &physical {
                confirmed.insert(
                    table.shard_ordinal,
                    sqlx::query_scalar::<_, String>(
                        "SELECT address FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id = $1 ORDER BY ordinal",
                    )
                    .bind(table.id)
                    .fetch_all(&mut *tx)
                    .await?
                    .into_iter()
                    .collect(),
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
                          AND operation.operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
                        ORDER BY address.ordinal
                        "#,
                    )
                    .bind(table.id)
                    .fetch_all(&mut *tx)
                    .await?
                    .into_iter()
                    .collect(),
                );
            }
        }
        let mut operations = Vec::new();
        for shard in shard_plan {
            let existing_table = physical
                .iter()
                .find(|table| table.shard_ordinal == shard.shard_ordinal)
                .cloned();
            let desired = shard.addresses.iter().cloned().collect::<BTreeSet<_>>();
            let confirmed_addresses = existing_table
                .as_ref()
                .and_then(|table| confirmed.get(&table.shard_ordinal))
                .cloned()
                .unwrap_or_default();
            let pending_addresses = existing_table
                .as_ref()
                .and_then(|table| pending.get(&table.shard_ordinal))
                .cloned()
                .unwrap_or_default();
            let Some((kind, missing)) = next_shared_market_mutation(
                existing_table.is_some(),
                &desired,
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
                        manifest_id: Some(manifest_id),
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
                "binding {binding_id} head has an unexpired usage lease"
            )));
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
        let mut tx = self.pool().begin().await?;
        let locked_tables = sqlx::query(
            r#"
            SELECT id, family_id, allocation_kind, desired_state, status
            FROM loyal_yield.route_lookup_tables
            WHERE id = ANY($1) AND cluster = $2
            ORDER BY id
            FOR UPDATE
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
        let cleanup_operation_count: i64 = sqlx::query_scalar(
            r#"
            SELECT count(*) FROM loyal_yield.lookup_table_operations
            WHERE route_lookup_table_id = ANY($1)
              AND operation_kind IN ('deactivate', 'close')
              AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
            "#,
        )
        .bind(&bundle.route_lookup_table_ids)
        .fetch_one(&mut *tx)
        .await?;
        if cleanup_operation_count != 0 {
            return Err(OrchestratorError::StoreInvariant(
                "lookup-table usage lease races with a pending cleanup operation".to_owned(),
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

    /// Explicitly removes an imported legacy table from the durable resolver
    /// set. The row remains as audit history for the existing cleanup scanner.
    pub async fn retire_legacy_route_lookup_table(
        &self,
        input: LegacyLookupTableRetirementRequest,
    ) -> Result<LegacyLookupTableRetirement, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        sqlx::query(
            "SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-rollout:' || $1, 0))",
        )
        .bind(&input.cluster)
        .execute(&mut *tx)
        .await?;
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
            SELECT rollout_mode, force_legacy
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
            SELECT selection_kind, legacy_table_ids, selected_table_ids
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
            row.try_get::<Option<String>, _>("selection_kind")
                .ok()
                .flatten()
                .as_deref()
                == Some("legacy")
                || row
                    .try_get::<Vec<i64>, _>("selected_table_ids")
                    .ok()
                    .is_some_and(|ids| ids.contains(&table_id))
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
            SET legacy_table_ids = array_remove(legacy_table_ids, $2), updated_at = now()
            WHERE cluster = $1 AND $2 = ANY(legacy_table_ids)
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

    pub async fn upsert_lookup_table_provisioning_request(
        &self,
        mut input: LookupTableProvisioningRequestUpsert,
    ) -> Result<LookupTableProvisioningRequestRecord, OrchestratorError> {
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
            input.shared_manifest_id.is_none(),
        )?;
        validate_request_addresses(
            &input.vault_addresses,
            LookupTableManifestSubject::Vault,
            input.vault_manifest_id.is_none(),
        )?;
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

        let mut tx = self.pool().begin().await?;
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
            // `requirements_fingerprint` is the durable identity. Multiple
            // route shapes can require the exact same immutable address set;
            // retain the first route fingerprint as audit provenance without
            // making it own a second physical allocation.
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
            if matches!(
                existing.request_status,
                LookupTableProvisioningRequestStatus::Failed
                    | LookupTableProvisioningRequestStatus::Cancelled
            ) {
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.lookup_table_provisioning_requests
                    SET request_status = 'requested', requested_at = now(),
                        lease_owner = NULL, lease_expires_at = NULL,
                        next_attempt_at = NULL, error_code = NULL, error_detail = NULL,
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
        let row = sqlx::query(
            "SELECT * FROM loyal_yield.lookup_table_provisioning_requests WHERE id = $1",
        )
        .bind(request_id)
        .fetch_one(&mut *tx)
        .await?;
        let request = lookup_table_provisioning_request_from_row(&row)?;
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
                SELECT id FROM loyal_yield.lookup_table_provisioning_requests
                WHERE cluster = $1
                  AND request_status IN ('requested', 'queued', 'failed', 'planning')
                  AND (next_attempt_at IS NULL OR next_attempt_at <= now())
                  AND (request_status <> 'planning' OR lease_expires_at <= now())
                ORDER BY requested_at, id
                FOR UPDATE SKIP LOCKED LIMIT 1
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

fn stale_store_update(kind: &str, id: i64) -> OrchestratorError {
    OrchestratorError::StoreInvariant(format!("stale or missing {kind} {id}"))
}

fn stale_fenced_operation(id: i64) -> OrchestratorError {
    OrchestratorError::StoreInvariant(format!(
        "lookup-table operation {id} lease is stale, expired, or fenced"
    ))
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
    // The family row is the serialization point for aggregate revisions. Two
    // disjoint route cohorts planned concurrently therefore cannot publish
    // competing partial desired sets.
    let family_rows = sqlx::query(
        r#"
        SELECT * FROM loyal_yield.lookup_table_families
        WHERE cluster = $1 AND kind = 'vault_shards' AND desired_state = 'active'
        ORDER BY logical_name, id
        FOR UPDATE
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
    let desired_set_hash = manifest_address_records_hash(&addresses);
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
    if input.addresses.len() > usize::from(LOOKUP_TABLE_HARD_CAPACITY) {
        return Err(OrchestratorError::StoreInvariant(
            "lookup-table manifest contains more than 256 addresses".to_owned(),
        ));
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
    _manifest_will_be_derived: bool,
) -> Result<(), OrchestratorError> {
    if addresses.len() > usize::from(LOOKUP_TABLE_HARD_CAPACITY) {
        return Err(OrchestratorError::StoreInvariant(format!(
            "provisioning request {} class exceeds lookup-table hard capacity",
            expected_class.as_str()
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
                "provisioning request {} addresses must be valid pubkeys, unique, contiguous, typed, and role-labelled",
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

fn manifest_address_records_hash(addresses: &[LookupTableManifestAddressRecord]) -> String {
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
    if operation.family_id != input.family_id
        || operation.route_lookup_table_id != input.route_lookup_table_id
        || operation.manifest_id != input.manifest_id
        || operation.binding_id != input.binding_id
        || operation.operation_kind != input.operation_kind
        || operation.target_generation != input.target_generation
        || operation.target_shard_ordinal != input.target_shard_ordinal
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
        if operation.family_id != input.family_id
            || operation.operation_kind != input.operation_kind
            || operation.route_lookup_table_id != input.route_lookup_table_id
            || operation.manifest_id != input.manifest_id
            || operation.binding_id != input.binding_id
            || operation.target_generation != input.target_generation
            || operation.target_shard_ordinal != input.target_shard_ordinal
            || operation.mutation_epoch != input.mutation_epoch
            || operation.estimated_fee_lamports != input.estimated_fee_lamports
            || operation.estimated_rent_lamports != input.estimated_rent_lamports
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
                WHERE family.cluster = $1
                  AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
                  AND (
                      NOT $4 OR operation_state IN (
                          'signed', 'submitted', 'confirmed', 'finalized',
                          'reconciled', 'needs_reconcile'
                      ) OR (
                          operation_state = 'leased'
                          AND transaction_signature IS NOT NULL
                      ) OR (
                          operation_kind = 'verify'
                          AND operation_state IN ('queued', 'retry_wait', 'leased')
                      )
                  )
                  AND (lease_expires_at IS NULL OR lease_expires_at <= now())
                  AND (next_attempt_at IS NULL OR next_attempt_at <= now())
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
                    operation.created_at,
                    operation.id
                FOR UPDATE SKIP LOCKED
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
        if input.mutation_epoch != expected_epoch
            || table.mutation_epoch != expected_epoch
            || table.authority != expected_authority
            || table.address_hash != expected_hash
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
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        lookup_table_operation_from_row(&row)
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
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        lookup_table_operation_from_row(&row)
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
        .bind(bounded_detail)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| stale_fenced_operation(operation_id))?;
        let operation = lookup_table_operation_from_row(&row)?;
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

    pub async fn replace_confirmed_lookup_table_membership(
        &self,
        table_id: i64,
        expected_mutation_epoch: i64,
        new_mutation_epoch: i64,
        observed_slot: i64,
        mut addresses: Vec<LookupTableMembershipAddress>,
    ) -> Result<ReusableLookupTableRecord, OrchestratorError> {
        addresses.sort_by_key(|address| address.ordinal);
        validate_membership(&addresses, observed_slot)?;
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
                updated_at = now()
            WHERE id = $1 AND mutation_epoch = $2
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
        .fetch_one(&mut *tx)
        .await?;
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
        let mut tx = self.pool().begin().await?;
        if !input.selected_table_ids.is_empty() {
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
        sqlx::query(
            "SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-rollout:' || $1, 0))",
        )
        .bind(cluster)
        .execute(&mut *tx)
        .await?;
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
        sqlx::query(
            "SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-rollout:' || $1, 0))",
        )
        .bind(cluster)
        .execute(&mut *tx)
        .await?;
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
        sqlx::query(
            "SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-rollout:' || $1, 0))",
        )
        .bind(cluster)
        .execute(&mut *tx)
        .await?;
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
                        'permanent_failures', count(*) FILTER (WHERE operation.operation_state = 'permanent_failure'),
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
