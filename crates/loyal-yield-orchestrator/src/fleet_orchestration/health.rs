use std::collections::BTreeMap;

use chrono::{DateTime, Duration as ChronoDuration, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use thiserror::Error;

use super::queue::FleetOrchestrationStatus;

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum FleetWorkerRole {
    Planner,
    Revalidator,
    Executor,
    Confirmer,
    Reconciler,
    PriorityProvisioner,
}

impl FleetWorkerRole {
    pub const ALL: [Self; 6] = [
        Self::Planner,
        Self::Revalidator,
        Self::Executor,
        Self::Confirmer,
        Self::Reconciler,
        Self::PriorityProvisioner,
    ];

    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Planner => "planner",
            Self::Revalidator => "revalidator",
            Self::Executor => "executor",
            Self::Confirmer => "confirmer",
            Self::Reconciler => "reconciler",
            Self::PriorityProvisioner => "priority_provisioner",
        }
    }

    pub const fn owning_binary(self) -> &'static str {
        match self {
            Self::Planner => "fleet-opportunity-planner",
            Self::Revalidator | Self::Executor | Self::Reconciler => "same-mint-reserve-swap",
            Self::Confirmer => "fleet-route-confirmer",
            Self::PriorityProvisioner => "route-lookup-table-provisioner",
        }
    }

    pub const fn command_prefix(self) -> &'static str {
        match self {
            Self::Planner => "/usr/local/bin/fleet-opportunity-planner",
            Self::Revalidator => "/usr/local/bin/same-mint-reserve-swap --fleet-worker revalidate",
            Self::Executor => "/usr/local/bin/same-mint-reserve-swap --fleet-worker execute",
            Self::Confirmer => "/usr/local/bin/fleet-route-confirmer --execute",
            Self::Reconciler => "/usr/local/bin/same-mint-reserve-swap --fleet-reconciler",
            Self::PriorityProvisioner => "/usr/local/bin/route-lookup-table-provisioner",
        }
    }

    pub const fn local_probe_argv(self) -> &'static [&'static str] {
        match self {
            Self::Planner => &["--role-probe"],
            Self::Revalidator => &["--fleet-worker", "revalidate", "--role-probe"],
            Self::Executor => &["--fleet-worker", "execute", "--role-probe"],
            Self::Confirmer => &["--role-probe"],
            Self::Reconciler => &["--fleet-reconciler", "--role-probe"],
            Self::PriorityProvisioner => &["--role-probe"],
        }
    }
}

/// A startup-safe binary-presence probe. It is deliberately static: invoking
/// it must happen before configuration, signer, database, or RPC setup.
pub fn fleet_worker_role_probe(role: FleetWorkerRole) -> Value {
    json!({
        "schemaVersion": 1,
        "event": "fleet_worker_role_probe",
        "status": "pass",
        "role": role.as_str(),
        "owningBinary": role.owning_binary(),
        "commandPrefix": role.command_prefix(),
        "probeArgv": role.local_probe_argv(),
        "networkAccessed": false,
        "secretsLoaded": false,
        "databaseMutated": false,
        "transactionSent": false,
    })
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum FleetStuckStage {
    MarketEpoch,
    Ready,
    WaitingAlt,
    Sender,
    Confirmer,
    Reconciler,
}

impl FleetStuckStage {
    pub const ALL: [Self; 6] = [
        Self::MarketEpoch,
        Self::Ready,
        Self::WaitingAlt,
        Self::Sender,
        Self::Confirmer,
        Self::Reconciler,
    ];

    pub const fn as_str(self) -> &'static str {
        match self {
            Self::MarketEpoch => "market_epoch",
            Self::Ready => "ready",
            Self::WaitingAlt => "waiting_alt",
            Self::Sender => "sender",
            Self::Confirmer => "confirmer",
            Self::Reconciler => "reconciler",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetStageHealthPolicy {
    pub recovery_poll_interval_milliseconds: u64,
    pub planner_stale_after_milliseconds: u64,
    pub ready_stuck_after_milliseconds: u64,
    pub waiting_alt_stuck_after_milliseconds: u64,
    pub sender_stuck_after_milliseconds: u64,
    pub confirmer_stuck_after_milliseconds: u64,
    pub reconciler_stuck_after_milliseconds: u64,
}

impl FleetStageHealthPolicy {
    /// Feedback thresholds mirror the verifier's first-response SLOs. The
    /// durable recovery poll bounds missed-notification recovery; the distinct
    /// health observation cadence bounds when a crossed threshold is emitted.
    pub fn for_recovery_poll(
        recovery_poll_interval_milliseconds: u64,
    ) -> Result<Self, FleetHealthError> {
        if recovery_poll_interval_milliseconds == 0
            || recovery_poll_interval_milliseconds > i64::MAX as u64
        {
            return Err(FleetHealthError::InvalidRecoveryPoll);
        }
        let at_least_poll =
            |milliseconds: u64| milliseconds.max(recovery_poll_interval_milliseconds);
        Ok(Self {
            recovery_poll_interval_milliseconds,
            planner_stale_after_milliseconds: at_least_poll(
                recovery_poll_interval_milliseconds.saturating_mul(2),
            ),
            ready_stuck_after_milliseconds: at_least_poll(10_000),
            waiting_alt_stuck_after_milliseconds: at_least_poll(120_000),
            sender_stuck_after_milliseconds: at_least_poll(10_000),
            confirmer_stuck_after_milliseconds: at_least_poll(30_000),
            reconciler_stuck_after_milliseconds: at_least_poll(30_000),
        })
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FleetStageStatusSource {
    pub cluster: String,
    pub planner_registered_at: Option<DateTime<Utc>>,
    pub planner_last_seen_at: Option<DateTime<Utc>>,
    pub latest_market_epoch_id: Option<i64>,
    pub latest_market_expires_at: Option<DateTime<Utc>>,
    pub ready_count: i64,
    pub oldest_ready_state_entered_at: Option<DateTime<Utc>>,
    pub waiting_alt_count: i64,
    pub oldest_waiting_alt_state_entered_at: Option<DateTime<Utc>>,
    pub sender_count: i64,
    pub oldest_sender_state_entered_at: Option<DateTime<Utc>>,
    pub confirmer_count: i64,
    pub oldest_confirmer_state_entered_at: Option<DateTime<Utc>>,
    pub reconciler_count: i64,
    pub oldest_reconciler_state_entered_at: Option<DateTime<Utc>>,
}

impl FleetStageStatusSource {
    pub fn from_status_rows(rows: &[FleetOrchestrationStatus]) -> Result<Self, FleetHealthError> {
        let first = rows.first().ok_or(FleetHealthError::EmptyStatus)?;
        if rows.iter().any(|row| row.cluster != first.cluster) {
            return Err(FleetHealthError::MixedClusters);
        }
        Ok(Self {
            cluster: first.cluster.clone(),
            planner_registered_at: first.planner_registered_at,
            planner_last_seen_at: first.planner_last_seen_at,
            latest_market_epoch_id: first.latest_market_epoch_id,
            latest_market_expires_at: first.latest_market_expires_at,
            ready_count: first.ready_opportunity_count,
            oldest_ready_state_entered_at: first.oldest_ready_state_entered_at,
            waiting_alt_count: first.waiting_alt_opportunity_count,
            oldest_waiting_alt_state_entered_at: first.oldest_waiting_alt_state_entered_at,
            sender_count: first.sender_submission_count,
            oldest_sender_state_entered_at: first.oldest_sender_state_entered_at,
            confirmer_count: first.confirmer_submission_count,
            oldest_confirmer_state_entered_at: first.oldest_confirmer_state_entered_at,
            reconciler_count: first.reconciler_submission_count,
            oldest_reconciler_state_entered_at: first.oldest_reconciler_state_entered_at,
        })
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetStuckStageSignal {
    pub stage: FleetStuckStage,
    pub active_item_count: u64,
    pub stuck_since: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetStuckStageDetection {
    pub stage: FleetStuckStage,
    pub active_item_count: u64,
    pub stuck_since: DateTime<Utc>,
    pub detected_at: DateTime<Utc>,
    pub detection_milliseconds: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetStageHealthReport {
    pub cluster: String,
    pub observed_at: DateTime<Utc>,
    pub recovery_poll_interval_milliseconds: u64,
    pub health_observation_interval_milliseconds: u64,
    pub signals: Vec<FleetStuckStageSignal>,
    pub stuck_stages: Vec<FleetStuckStageDetection>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetStuckStageFixtureEvidence {
    pub recovery_poll_interval_milliseconds: u64,
    pub health_observation_interval_milliseconds: u64,
    pub detection_milliseconds: BTreeMap<String, u64>,
    pub exact_stage_set: bool,
    pub detected_within_one_health_observation: bool,
    pub healthy_control_clear: bool,
    pub passed: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Error)]
pub enum FleetHealthError {
    #[error("fleet stage health requires a positive representable recovery poll")]
    InvalidRecoveryPoll,
    #[error("fleet stage health requires a positive representable observation interval")]
    InvalidHealthObservationInterval,
    #[error("fleet stage health requires at least one status row")]
    EmptyStatus,
    #[error("fleet stage health rows must describe one cluster")]
    MixedClusters,
}

fn duration_milliseconds(milliseconds: u64) -> ChronoDuration {
    ChronoDuration::milliseconds(i64::try_from(milliseconds).unwrap_or(i64::MAX))
}

fn deadline(
    count: i64,
    entered_at: Option<DateTime<Utc>>,
    threshold_milliseconds: u64,
    observed_at: DateTime<Utc>,
) -> Option<DateTime<Utc>> {
    if count <= 0 {
        return None;
    }
    Some(
        entered_at
            .map(|entered_at| entered_at + duration_milliseconds(threshold_milliseconds))
            // A positive backlog without its required state-entry timestamp is
            // itself an immediately visible health invariant violation.
            .unwrap_or(observed_at),
    )
}

fn minimum_timestamp(
    left: Option<DateTime<Utc>>,
    right: Option<DateTime<Utc>>,
) -> Option<DateTime<Utc>> {
    match (left, right) {
        (Some(left), Some(right)) => Some(left.min(right)),
        (Some(value), None) | (None, Some(value)) => Some(value),
        (None, None) => None,
    }
}

pub fn fleet_stuck_stage_signals(
    source: &FleetStageStatusSource,
    policy: FleetStageHealthPolicy,
    observed_at: DateTime<Utc>,
) -> Vec<FleetStuckStageSignal> {
    let expired_market = source
        .latest_market_expires_at
        .filter(|expires_at| *expires_at <= observed_at);
    let missing_market = source
        .latest_market_epoch_id
        .is_none()
        .then(|| {
            source.planner_registered_at.map(|registered_at| {
                registered_at + duration_milliseconds(policy.planner_stale_after_milliseconds)
            })
        })
        .flatten();
    let stale_planner = source
        .planner_last_seen_at
        .or(source.planner_registered_at)
        .map(|last_seen_at| {
            last_seen_at + duration_milliseconds(policy.planner_stale_after_milliseconds)
        })
        .filter(|stale_at| *stale_at <= observed_at);
    let market_stuck_since = minimum_timestamp(
        expired_market,
        minimum_timestamp(missing_market, stale_planner),
    );

    [
        FleetStuckStageSignal {
            stage: FleetStuckStage::MarketEpoch,
            active_item_count: u64::from(market_stuck_since.is_some()),
            stuck_since: market_stuck_since,
        },
        FleetStuckStageSignal {
            stage: FleetStuckStage::Ready,
            active_item_count: u64::try_from(source.ready_count.max(0)).unwrap_or(u64::MAX),
            stuck_since: deadline(
                source.ready_count,
                source.oldest_ready_state_entered_at,
                policy.ready_stuck_after_milliseconds,
                observed_at,
            ),
        },
        FleetStuckStageSignal {
            stage: FleetStuckStage::WaitingAlt,
            active_item_count: u64::try_from(source.waiting_alt_count.max(0)).unwrap_or(u64::MAX),
            stuck_since: deadline(
                source.waiting_alt_count,
                source.oldest_waiting_alt_state_entered_at,
                policy.waiting_alt_stuck_after_milliseconds,
                observed_at,
            ),
        },
        FleetStuckStageSignal {
            stage: FleetStuckStage::Sender,
            active_item_count: u64::try_from(source.sender_count.max(0)).unwrap_or(u64::MAX),
            stuck_since: deadline(
                source.sender_count,
                source.oldest_sender_state_entered_at,
                policy.sender_stuck_after_milliseconds,
                observed_at,
            ),
        },
        FleetStuckStageSignal {
            stage: FleetStuckStage::Confirmer,
            active_item_count: u64::try_from(source.confirmer_count.max(0)).unwrap_or(u64::MAX),
            stuck_since: deadline(
                source.confirmer_count,
                source.oldest_confirmer_state_entered_at,
                policy.confirmer_stuck_after_milliseconds,
                observed_at,
            ),
        },
        FleetStuckStageSignal {
            stage: FleetStuckStage::Reconciler,
            active_item_count: u64::try_from(source.reconciler_count.max(0)).unwrap_or(u64::MAX),
            stuck_since: deadline(
                source.reconciler_count,
                source.oldest_reconciler_state_entered_at,
                policy.reconciler_stuck_after_milliseconds,
                observed_at,
            ),
        },
    ]
    .into_iter()
    .collect()
}

pub fn detect_stuck_stages(
    signals: &[FleetStuckStageSignal],
    observed_at: DateTime<Utc>,
) -> Vec<FleetStuckStageDetection> {
    signals
        .iter()
        .filter_map(|signal| {
            let stuck_since = signal.stuck_since.filter(|value| *value <= observed_at)?;
            let detection_milliseconds = observed_at
                .signed_duration_since(stuck_since)
                .num_milliseconds()
                .max(0) as u64;
            Some(FleetStuckStageDetection {
                stage: signal.stage,
                active_item_count: signal.active_item_count,
                stuck_since,
                detected_at: observed_at,
                detection_milliseconds,
            })
        })
        .collect()
}

pub fn fleet_stage_health_report(
    rows: &[FleetOrchestrationStatus],
    recovery_poll_interval_milliseconds: u64,
    health_observation_interval_milliseconds: u64,
    observed_at: DateTime<Utc>,
) -> Result<FleetStageHealthReport, FleetHealthError> {
    let policy = FleetStageHealthPolicy::for_recovery_poll(recovery_poll_interval_milliseconds)?;
    if health_observation_interval_milliseconds == 0
        || health_observation_interval_milliseconds > i64::MAX as u64
    {
        return Err(FleetHealthError::InvalidHealthObservationInterval);
    }
    let source = FleetStageStatusSource::from_status_rows(rows)?;
    let signals = fleet_stuck_stage_signals(&source, policy, observed_at);
    let stuck_stages = detect_stuck_stages(&signals, observed_at);
    Ok(FleetStageHealthReport {
        cluster: source.cluster,
        observed_at,
        recovery_poll_interval_milliseconds,
        health_observation_interval_milliseconds,
        signals,
        stuck_stages,
    })
}

/// Controlled functional fixture used by the executable verifier. Every stage
/// crosses its own SLO halfway between two health observations, while a
/// matching healthy snapshot remains below every threshold. The fixture keeps
/// the faster durable recovery poll distinct from health emission cadence.
pub fn functional_stuck_stage_fixture() -> FleetStuckStageFixtureEvidence {
    let recovery_poll_interval_milliseconds = 250;
    let health_observation_interval_milliseconds = 1_000;
    let policy = FleetStageHealthPolicy::for_recovery_poll(recovery_poll_interval_milliseconds)
        .expect("fixture recovery poll is valid");
    let observed_at = DateTime::<Utc>::from_timestamp(2_000_000_000, 0)
        .expect("fixture timestamp is representable");
    let detection_delay = health_observation_interval_milliseconds / 2;
    let before = |threshold: u64| {
        observed_at - duration_milliseconds(threshold.saturating_add(detection_delay))
    };
    let source = FleetStageStatusSource {
        cluster: "fixture".to_owned(),
        planner_registered_at: Some(observed_at),
        planner_last_seen_at: Some(observed_at),
        latest_market_epoch_id: Some(1),
        latest_market_expires_at: Some(observed_at - duration_milliseconds(detection_delay)),
        ready_count: 1,
        oldest_ready_state_entered_at: Some(before(policy.ready_stuck_after_milliseconds)),
        waiting_alt_count: 1,
        oldest_waiting_alt_state_entered_at: Some(before(
            policy.waiting_alt_stuck_after_milliseconds,
        )),
        sender_count: 1,
        oldest_sender_state_entered_at: Some(before(policy.sender_stuck_after_milliseconds)),
        confirmer_count: 1,
        oldest_confirmer_state_entered_at: Some(before(policy.confirmer_stuck_after_milliseconds)),
        reconciler_count: 1,
        oldest_reconciler_state_entered_at: Some(before(
            policy.reconciler_stuck_after_milliseconds,
        )),
    };
    let signals = fleet_stuck_stage_signals(&source, policy, observed_at);
    let detections = detect_stuck_stages(&signals, observed_at);
    let detection_milliseconds = detections
        .iter()
        .map(|detection| {
            (
                detection.stage.as_str().to_owned(),
                detection.detection_milliseconds,
            )
        })
        .collect::<BTreeMap<_, _>>();
    let exact_stage_set = detection_milliseconds.len() == FleetStuckStage::ALL.len()
        && FleetStuckStage::ALL
            .iter()
            .all(|stage| detection_milliseconds.contains_key(stage.as_str()));
    let detected_within_one_health_observation = detection_milliseconds
        .values()
        .all(|milliseconds| *milliseconds <= health_observation_interval_milliseconds);

    let just_healthy =
        |threshold: u64| observed_at - duration_milliseconds(threshold.saturating_sub(1));
    let healthy_source = FleetStageStatusSource {
        latest_market_expires_at: Some(observed_at + duration_milliseconds(1)),
        oldest_ready_state_entered_at: Some(just_healthy(policy.ready_stuck_after_milliseconds)),
        oldest_waiting_alt_state_entered_at: Some(just_healthy(
            policy.waiting_alt_stuck_after_milliseconds,
        )),
        oldest_sender_state_entered_at: Some(just_healthy(policy.sender_stuck_after_milliseconds)),
        oldest_confirmer_state_entered_at: Some(just_healthy(
            policy.confirmer_stuck_after_milliseconds,
        )),
        oldest_reconciler_state_entered_at: Some(just_healthy(
            policy.reconciler_stuck_after_milliseconds,
        )),
        ..source
    };
    let healthy_signals = fleet_stuck_stage_signals(&healthy_source, policy, observed_at);
    let healthy_control_clear = detect_stuck_stages(&healthy_signals, observed_at).is_empty();
    FleetStuckStageFixtureEvidence {
        recovery_poll_interval_milliseconds,
        health_observation_interval_milliseconds,
        detection_milliseconds,
        exact_stage_set,
        detected_within_one_health_observation,
        healthy_control_clear,
        passed: exact_stage_set && detected_within_one_health_observation && healthy_control_clear,
    }
}
