use std::{
    collections::{BTreeMap, BTreeSet},
    time::Duration,
};

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use sqlx::{PgPool, Postgres, Row, Transaction};

use crate::OrchestratorError;

/// The complete reusable-ALT semantic alert contract. Keep this list small and
/// stable: downstream paging rules use these exact strings as routing keys.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LookupTableAlertCondition {
    ReadinessRegression,
    MissingCoverage,
    OperationBacklog,
    CapacityHeadroom,
    AuthorityPrefixDrift,
    ProvisioningBudget,
    OrphanedTables,
    FallbackUse,
    CleanupAnomalies,
}

impl LookupTableAlertCondition {
    pub const ALL: [Self; 9] = [
        Self::ReadinessRegression,
        Self::MissingCoverage,
        Self::OperationBacklog,
        Self::CapacityHeadroom,
        Self::AuthorityPrefixDrift,
        Self::ProvisioningBudget,
        Self::OrphanedTables,
        Self::FallbackUse,
        Self::CleanupAnomalies,
    ];

    pub const fn as_str(self) -> &'static str {
        match self {
            Self::ReadinessRegression => "readiness_regression",
            Self::MissingCoverage => "missing_coverage",
            Self::OperationBacklog => "operation_backlog",
            Self::CapacityHeadroom => "capacity_headroom",
            Self::AuthorityPrefixDrift => "authority_prefix_drift",
            Self::ProvisioningBudget => "provisioning_budget",
            Self::OrphanedTables => "orphaned_tables",
            Self::FallbackUse => "fallback_use",
            Self::CleanupAnomalies => "cleanup_anomalies",
        }
    }

    pub fn parse(value: &str) -> Option<Self> {
        Self::ALL
            .into_iter()
            .find(|condition| condition.as_str() == value)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LookupTableAlertSeverity {
    Info,
    Warning,
    Critical,
}

impl LookupTableAlertSeverity {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Info => "info",
            Self::Warning => "warning",
            Self::Critical => "critical",
        }
    }

    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "info" => Some(Self::Info),
            "warning" => Some(Self::Warning),
            "critical" => Some(Self::Critical),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct LookupTableAlertRuleRecord {
    pub condition: LookupTableAlertCondition,
    pub rule_version: i64,
    pub enabled: bool,
    pub severity: LookupTableAlertSeverity,
    pub description: String,
    pub configuration: Value,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LookupTableAlertThresholds {
    pub missing_coverage_grace: Duration,
    pub operation_backlog_age: Duration,
    pub operation_backlog_depth: i64,
    pub capacity_headroom: i64,
    pub budget_max_lamports: Option<i64>,
    pub budget_window: Duration,
    pub budget_alert_percent: i64,
    pub cleanup_grace: Duration,
}

impl Default for LookupTableAlertThresholds {
    fn default() -> Self {
        Self {
            missing_coverage_grace: Duration::from_secs(300),
            operation_backlog_age: Duration::from_secs(600),
            operation_backlog_depth: 25,
            capacity_headroom: 16,
            budget_max_lamports: None,
            budget_window: Duration::from_secs(86_400),
            budget_alert_percent: 90,
            cleanup_grace: Duration::from_secs(86_400),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct LookupTablePhysicalExpectation {
    pub table_id: i64,
    pub table_address: String,
    pub expected_authority: String,
    pub expected_addresses: Vec<String>,
    pub desired_state: String,
    pub mutation_epoch: i64,
    pub registry_authority_matches: bool,
    pub has_inflight_operation: bool,
    pub orphaned: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct LookupTableRpcAudit {
    pub finalized_slot: u64,
    pub authority_prefix_drift_table_ids: Vec<i64>,
    pub absent_orphan_table_ids: Vec<i64>,
    pub evidence: Vec<Value>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct LookupTableAlertSnapshot {
    pub cluster: String,
    pub policy_pubkey: String,
    pub shared_head_count: i64,
    pub healthy_shared_head_count: i64,
    pub missing_coverage_count: i64,
    pub oldest_missing_coverage_seconds: i64,
    pub operation_backlog_count: i64,
    pub oldest_operation_seconds: i64,
    pub permanent_operation_failure_count: i64,
    pub low_headroom_table_count: i64,
    pub minimum_headroom: Option<i64>,
    pub open_physical_drift_count: i64,
    pub budget_used_lamports: i64,
    pub budget_exhaustion_count: i64,
    pub orphaned_table_ids: Vec<i64>,
    pub fallback_use_count: i64,
    pub cleanup_anomaly_count: i64,
    pub cleanup_anomaly_table_ids: Vec<i64>,
    pub physical_expectations: Vec<LookupTablePhysicalExpectation>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct LookupTableAlertObservation {
    pub condition: LookupTableAlertCondition,
    pub active: bool,
    pub severity: LookupTableAlertSeverity,
    pub fingerprint: String,
    pub summary: String,
    pub details: Value,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LookupTableAlertEventKind {
    Open,
    Reminder,
    Resolved,
    Test,
}

impl LookupTableAlertEventKind {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Open => "open",
            Self::Reminder => "reminder",
            Self::Resolved => "resolved",
            Self::Test => "test",
        }
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct LookupTableAlertTransition {
    pub incident_id: Option<i64>,
    pub event_kind: Option<LookupTableAlertEventKind>,
    pub revision: Option<i64>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct LeasedLookupTableAlertDelivery {
    pub id: i64,
    pub fencing_token: i64,
    pub attempt_count: i32,
    pub max_attempts: i32,
    pub payload: Value,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct ExistingIncident {
    id: i64,
    status: String,
    fingerprint: String,
    revision: i64,
    last_observed_at: DateTime<Utc>,
    last_notified_at: DateTime<Utc>,
    first_observed_at: DateTime<Utc>,
    opened_at: DateTime<Utc>,
    occurrence_count: i64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum PlannedTransition {
    None,
    Open(i64),
    Reminder(i64),
    Resolved(i64),
}

impl PlannedTransition {
    fn event_kind(self) -> Option<LookupTableAlertEventKind> {
        match self {
            Self::None => None,
            Self::Open(_) => Some(LookupTableAlertEventKind::Open),
            Self::Reminder(_) => Some(LookupTableAlertEventKind::Reminder),
            Self::Resolved(_) => Some(LookupTableAlertEventKind::Resolved),
        }
    }

    fn revision(self) -> Option<i64> {
        match self {
            Self::None => None,
            Self::Open(revision) | Self::Reminder(revision) | Self::Resolved(revision) => {
                Some(revision)
            }
        }
    }
}

fn plan_transition(
    existing: Option<&ExistingIncident>,
    observation: &LookupTableAlertObservation,
    observed_at: DateTime<Utc>,
    reminder_interval: chrono::Duration,
) -> PlannedTransition {
    match (existing, observation.active) {
        (None, false) => PlannedTransition::None,
        (None, true) => PlannedTransition::Open(1),
        (Some(incident), false) if incident.status == "open" => {
            PlannedTransition::Resolved(incident.revision + 1)
        }
        (Some(_), false) => PlannedTransition::None,
        (Some(incident), true) if incident.status == "resolved" => {
            PlannedTransition::Open(incident.revision + 1)
        }
        (Some(incident), true)
            if incident.fingerprint != observation.fingerprint
                || observed_at - incident.last_notified_at >= reminder_interval =>
        {
            PlannedTransition::Reminder(incident.revision + 1)
        }
        (Some(_), true) => PlannedTransition::None,
    }
}

pub fn lookup_table_alert_fingerprint(
    condition: LookupTableAlertCondition,
    stable_details: &Value,
) -> String {
    let mut hasher = Sha256::new();
    hasher.update(condition.as_str().as_bytes());
    hasher.update([0]);
    hasher.update(
        serde_json::to_vec(&canonicalize_json(stable_details))
            .expect("serializing serde_json::Value cannot fail"),
    );
    format!("{:x}", hasher.finalize())
}

fn canonicalize_json(value: &Value) -> Value {
    match value {
        Value::Array(values) => Value::Array(values.iter().map(canonicalize_json).collect()),
        Value::Object(values) => {
            let sorted = values
                .iter()
                .map(|(key, value)| (key.clone(), canonicalize_json(value)))
                .collect::<BTreeMap<_, _>>();
            Value::Object(sorted.into_iter().collect())
        }
        _ => value.clone(),
    }
}

fn observation(
    condition: LookupTableAlertCondition,
    active: bool,
    severity: LookupTableAlertSeverity,
    summary: impl Into<String>,
    stable_details: Value,
) -> LookupTableAlertObservation {
    observation_with_details(
        condition,
        active,
        severity,
        summary,
        stable_details.clone(),
        stable_details,
    )
}

fn observation_with_details(
    condition: LookupTableAlertCondition,
    active: bool,
    severity: LookupTableAlertSeverity,
    summary: impl Into<String>,
    fingerprint_details: Value,
    details: Value,
) -> LookupTableAlertObservation {
    LookupTableAlertObservation {
        condition,
        active,
        severity,
        fingerprint: lookup_table_alert_fingerprint(condition, &fingerprint_details),
        summary: summary.into(),
        details,
    }
}

/// Produces one observation for every contract condition on every scan. Healthy
/// observations are necessary: they drive durable resolution notifications.
pub fn evaluate_lookup_table_alerts(
    snapshot: &LookupTableAlertSnapshot,
    rpc_audit: &LookupTableRpcAudit,
    thresholds: &LookupTableAlertThresholds,
) -> Vec<LookupTableAlertObservation> {
    let readiness_active =
        snapshot.shared_head_count != 1 || snapshot.healthy_shared_head_count != 1;
    let missing_coverage_active = snapshot.missing_coverage_count > 0;
    let operation_backlog_active = snapshot.permanent_operation_failure_count > 0
        || snapshot.operation_backlog_count >= thresholds.operation_backlog_depth
        || (snapshot.operation_backlog_count > 0
            && snapshot.oldest_operation_seconds
                >= duration_seconds_i64(thresholds.operation_backlog_age));
    let capacity_active = snapshot.low_headroom_table_count > 0;
    let authority_drift_active = snapshot.open_physical_drift_count > 0
        || !rpc_audit.authority_prefix_drift_table_ids.is_empty();
    let budget_percent = thresholds.budget_max_lamports.map(|limit| {
        if limit <= 0 {
            100
        } else {
            snapshot.budget_used_lamports.saturating_mul(100) / limit
        }
    });
    let budget_active = snapshot.budget_exhaustion_count > 0
        || budget_percent.is_some_and(|percent| percent >= thresholds.budget_alert_percent);
    let orphaned_active = !snapshot.orphaned_table_ids.is_empty();
    let fallback_active = snapshot.fallback_use_count > 0;
    let cleanup_active =
        snapshot.cleanup_anomaly_count > 0 || !rpc_audit.absent_orphan_table_ids.is_empty();

    vec![
        observation(
            LookupTableAlertCondition::ReadinessRegression,
            readiness_active,
            LookupTableAlertSeverity::Critical,
            if readiness_active {
                "durable shared-market ALT readiness regressed"
            } else {
                "durable shared-market ALT is ready"
            },
            json!({
                "sharedHeadCount": snapshot.shared_head_count,
                "healthySharedHeadCount": snapshot.healthy_shared_head_count,
            }),
        ),
        observation_with_details(
            LookupTableAlertCondition::MissingCoverage,
            missing_coverage_active,
            LookupTableAlertSeverity::Warning,
            if missing_coverage_active {
                "route coverage remained missing beyond its provisioning grace period"
            } else {
                "no route coverage is overdue"
            },
            json!({
                "count": snapshot.missing_coverage_count,
                "graceSeconds": duration_seconds_i64(thresholds.missing_coverage_grace),
            }),
            json!({
                "count": snapshot.missing_coverage_count,
                "oldestSeconds": snapshot.oldest_missing_coverage_seconds,
                "graceSeconds": duration_seconds_i64(thresholds.missing_coverage_grace),
            }),
        ),
        observation_with_details(
            LookupTableAlertCondition::OperationBacklog,
            operation_backlog_active,
            if snapshot.permanent_operation_failure_count > 0 {
                LookupTableAlertSeverity::Critical
            } else {
                LookupTableAlertSeverity::Warning
            },
            if operation_backlog_active {
                "reusable-ALT provisioning operations are backlogged or terminally failed"
            } else {
                "reusable-ALT operation queue is healthy"
            },
            json!({
                "depth": snapshot.operation_backlog_count,
                "permanentFailures": snapshot.permanent_operation_failure_count,
                "depthThreshold": thresholds.operation_backlog_depth,
                "ageThresholdSeconds": duration_seconds_i64(thresholds.operation_backlog_age),
            }),
            json!({
                "depth": snapshot.operation_backlog_count,
                "oldestSeconds": snapshot.oldest_operation_seconds,
                "permanentFailures": snapshot.permanent_operation_failure_count,
                "depthThreshold": thresholds.operation_backlog_depth,
                "ageThresholdSeconds": duration_seconds_i64(thresholds.operation_backlog_age),
            }),
        ),
        observation(
            LookupTableAlertCondition::CapacityHeadroom,
            capacity_active,
            LookupTableAlertSeverity::Warning,
            if capacity_active {
                "one or more packed vault ALTs are below reserved expansion headroom"
            } else {
                "packed vault ALT headroom is healthy"
            },
            json!({
                "tableCount": snapshot.low_headroom_table_count,
                "minimumHeadroom": snapshot.minimum_headroom,
                "threshold": thresholds.capacity_headroom,
            }),
        ),
        observation(
            LookupTableAlertCondition::AuthorityPrefixDrift,
            authority_drift_active,
            LookupTableAlertSeverity::Critical,
            if authority_drift_active {
                "finalized reusable-ALT authority or ordered address prefix drifted"
            } else {
                "finalized reusable-ALT authority and ordered prefixes match"
            },
            json!({
                "durableOpenDriftCount": snapshot.open_physical_drift_count,
                "rpcDriftTableIds": rpc_audit.authority_prefix_drift_table_ids,
                "rpcEvidence": rpc_audit.evidence,
            }),
        ),
        observation(
            LookupTableAlertCondition::ProvisioningBudget,
            budget_active,
            if snapshot.budget_exhaustion_count > 0 {
                LookupTableAlertSeverity::Critical
            } else {
                LookupTableAlertSeverity::Warning
            },
            if budget_active {
                "reusable-ALT provisioning budget is near its limit or exhausted"
            } else {
                "reusable-ALT provisioning budget is healthy"
            },
            json!({
                "usedLamports": snapshot.budget_used_lamports,
                "maxLamports": thresholds.budget_max_lamports,
                "usedPercent": budget_percent,
                "alertPercent": thresholds.budget_alert_percent,
                "exhaustionCount": snapshot.budget_exhaustion_count,
                "windowSeconds": duration_seconds_i64(thresholds.budget_window),
            }),
        ),
        observation(
            LookupTableAlertCondition::OrphanedTables,
            orphaned_active,
            LookupTableAlertSeverity::Warning,
            if orphaned_active {
                "reusable ALTs exist without a live family, binding, lease, or operation reference"
            } else {
                "no reusable ALTs are orphaned"
            },
            json!({"tableIds": snapshot.orphaned_table_ids}),
        ),
        observation(
            LookupTableAlertCondition::FallbackUse,
            fallback_active,
            LookupTableAlertSeverity::Critical,
            if fallback_active {
                "legacy fallback or a non-reusable-only rollout control is active"
            } else {
                "routing is reusable-only without fallback use"
            },
            json!({"count": snapshot.fallback_use_count}),
        ),
        observation(
            LookupTableAlertCondition::CleanupAnomalies,
            cleanup_active,
            LookupTableAlertSeverity::Warning,
            if cleanup_active {
                "legacy ALT cleanup, close refund, or physical cleanup evidence is anomalous"
            } else {
                "legacy ALT cleanup and refunds are healthy"
            },
            json!({
                "count": snapshot.cleanup_anomaly_count,
                "tableIds": snapshot.cleanup_anomaly_table_ids,
                "absentOrphanTableIds": rpc_audit.absent_orphan_table_ids,
            }),
        ),
    ]
}

fn duration_seconds_i64(duration: Duration) -> i64 {
    i64::try_from(duration.as_secs()).unwrap_or(i64::MAX)
}

pub async fn load_lookup_table_alert_rules(
    pool: &PgPool,
) -> Result<Vec<LookupTableAlertRuleRecord>, OrchestratorError> {
    let rows = sqlx::query(
        r#"
        SELECT rule_key, rule_version, enabled, severity, description, configuration
        FROM loyal_yield.lookup_table_alert_rules
        ORDER BY rule_key
        "#,
    )
    .fetch_all(pool)
    .await?;
    let mut by_condition = BTreeMap::new();
    for row in rows {
        let rule_key: String = row.try_get("rule_key")?;
        let condition = LookupTableAlertCondition::parse(&rule_key).ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "unknown reusable ALT alert rule {rule_key:?}"
            ))
        })?;
        let severity_value: String = row.try_get("severity")?;
        let severity = LookupTableAlertSeverity::parse(&severity_value).ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "unknown reusable ALT alert severity {severity_value:?}"
            ))
        })?;
        let rule = LookupTableAlertRuleRecord {
            condition,
            rule_version: row.try_get("rule_version")?,
            enabled: row.try_get("enabled")?,
            severity,
            description: row.try_get("description")?,
            configuration: row.try_get("configuration")?,
        };
        if by_condition.insert(condition, rule).is_some() {
            return Err(OrchestratorError::StoreInvariant(format!(
                "duplicate reusable ALT alert rule {}",
                condition.as_str()
            )));
        }
    }
    if by_condition.len() != LookupTableAlertCondition::ALL.len()
        || LookupTableAlertCondition::ALL
            .iter()
            .any(|condition| !by_condition.contains_key(condition))
    {
        return Err(OrchestratorError::StoreInvariant(
            "reusable ALT alert rule catalog is not the exact nine-condition contract".to_owned(),
        ));
    }
    Ok(LookupTableAlertCondition::ALL
        .into_iter()
        .map(|condition| {
            by_condition
                .remove(&condition)
                .expect("exact catalog checked above")
        })
        .collect())
}

pub fn apply_lookup_table_alert_rule(
    mut observation: LookupTableAlertObservation,
    rule: &LookupTableAlertRuleRecord,
) -> Result<LookupTableAlertObservation, OrchestratorError> {
    if observation.condition != rule.condition {
        return Err(OrchestratorError::StoreInvariant(format!(
            "alert observation {} was paired with rule {}",
            observation.condition.as_str(),
            rule.condition.as_str()
        )));
    }
    let evaluation_fingerprint = observation.fingerprint.clone();
    let mut details = observation.details.as_object().cloned().ok_or_else(|| {
        OrchestratorError::StoreInvariant("alert observation details must be an object".to_owned())
    })?;
    details.insert("ruleVersion".to_owned(), json!(rule.rule_version));
    details.insert("ruleEnabled".to_owned(), json!(rule.enabled));
    observation.details = Value::Object(details);
    observation.fingerprint = lookup_table_alert_fingerprint(
        observation.condition,
        &json!({
            "evaluationFingerprint": evaluation_fingerprint,
            "ruleVersion": rule.rule_version,
            "enabled": rule.enabled,
            "severity": rule.severity.as_str(),
        }),
    );
    if rule.enabled {
        observation.severity = observation.severity.max(rule.severity);
    } else {
        observation.active = false;
        observation.severity = LookupTableAlertSeverity::Info;
        observation.summary = format!(
            "{} reusable ALT alert rule is disabled",
            observation.condition.as_str()
        );
    }
    Ok(observation)
}

/// Loads semantic control-plane state only. Physical authority/order checks are
/// intentionally returned as expectations and must be completed against a
/// finalized RPC snapshot by the signerless monitor.
pub async fn load_lookup_table_alert_snapshot(
    pool: &PgPool,
    cluster: &str,
    policy_pubkey: &str,
    thresholds: &LookupTableAlertThresholds,
) -> Result<LookupTableAlertSnapshot, OrchestratorError> {
    let missing_grace = duration_seconds_i64(thresholds.missing_coverage_grace);
    let capacity_headroom = thresholds.capacity_headroom;
    let budget_window = duration_seconds_i64(thresholds.budget_window);
    let cleanup_grace = duration_seconds_i64(thresholds.cleanup_grace);

    let row = sqlx::query(
        r#"
        WITH policy_families AS (
            SELECT *
            FROM loyal_yield.lookup_table_families
            WHERE cluster = $1
              AND provisioning_authority = $2
        ),
        shared_health AS (
            SELECT head.family_id,
                   head.readiness_state,
                   head.target_generation,
                   family.active_generation,
                   revision.address_count AS desired_address_count,
                   COALESCE(sum(route_table.usable_address_count) FILTER (
                       WHERE route_table.generation = family.active_generation
                         AND route_table.desired_state = 'active'
                   ), 0) AS usable_address_count,
                   count(route_table.id) FILTER (
                       WHERE route_table.generation = family.active_generation
                         AND route_table.desired_state = 'active'
                   ) AS active_table_count
            FROM loyal_yield.lookup_table_shared_market_catalog_heads head
            JOIN policy_families family ON family.id = head.family_id
            JOIN loyal_yield.lookup_table_shared_market_catalog_revisions revision
              ON revision.id = head.catalog_revision_id
            LEFT JOIN loyal_yield.route_lookup_tables route_table
              ON route_table.family_id = family.id
            WHERE family.kind = 'shared_market'
              AND family.desired_state = 'active'
            GROUP BY head.family_id, head.readiness_state, head.target_generation,
                     family.active_generation, revision.address_count
        ),
        overdue_coverage AS (
            SELECT readiness.vault_id::TEXT || ':' || readiness.route_fingerprint AS identity,
                   readiness.updated_at AS since
            FROM loyal_yield.lookup_table_route_readiness_current readiness
            WHERE readiness.cluster = $1
              AND readiness.updated_at <= now() - make_interval(secs => $3)
              AND (
                  readiness.selection_kind = 'blocked'
                  OR readiness.readiness_state IN ('incomplete', 'failed')
                  OR readiness.covered_address_count < readiness.required_address_count
              )
            UNION ALL
            SELECT request.vault_id::TEXT || ':' || request.requirements_fingerprint,
                   request.requested_at
            FROM loyal_yield.lookup_table_provisioning_requests request
            WHERE request.cluster = $1
              AND request.request_status IN ('requested', 'planning', 'queued', 'failed')
              AND request.requested_at <= now() - make_interval(secs => $3)
        ),
        operation_health AS (
            SELECT operation.*
            FROM loyal_yield.lookup_table_operations operation
            JOIN policy_families family ON family.id = operation.family_id
        ),
        low_headroom AS (
            SELECT route_table.id,
                   route_table.allocation_high_water - route_table.reserved_address_count AS headroom
            FROM loyal_yield.route_lookup_tables route_table
            JOIN policy_families family ON family.id = route_table.family_id
            WHERE family.kind = 'vault_shards'
              AND family.desired_state = 'active'
              AND route_table.generation = family.active_generation
              AND route_table.desired_state IN ('warming', 'active')
              AND route_table.accepting_allocations = TRUE
              AND route_table.allocation_high_water - route_table.reserved_address_count < $4
        ),
        orphaned AS (
            SELECT route_table.id
            FROM loyal_yield.route_lookup_tables route_table
            JOIN policy_families family ON family.id = route_table.family_id
            WHERE route_table.desired_state NOT IN ('closed', 'failed')
              AND NOT (
                  route_table.generation = family.active_generation
                  AND (
                      family.kind = 'shared_market'
                      OR route_table.accepting_allocations = TRUE
                      OR EXISTS (
                          SELECT 1 FROM loyal_yield.lookup_table_vault_bindings binding
                          WHERE binding.route_lookup_table_id = route_table.id
                            AND binding.lifecycle_state IN (
                                'preparing', 'warming', 'active', 'standby', 'retiring'
                            )
                      )
                  )
              )
              AND NOT (
                  route_table.generation = family.previous_generation
                  AND COALESCE(route_table.rollback_until, family.rollback_until) > now()
              )
              AND NOT EXISTS (
                  SELECT 1 FROM loyal_yield.lookup_table_operations operation
                  WHERE operation.route_lookup_table_id = route_table.id
                    AND operation.operation_state NOT IN (
                        'complete', 'permanent_failure', 'cancelled'
                    )
              )
              AND NOT EXISTS (
                  SELECT 1 FROM loyal_yield.lookup_table_usage_leases lease
                  WHERE lease.route_lookup_table_id = route_table.id
                    AND lease.released_at IS NULL
                    AND lease.expires_at > now()
              )
        ),
        rollout_fallback AS (
            SELECT control.id
            FROM loyal_yield.lookup_table_rollout_controls control
            WHERE control.cluster = $1
              AND (control.force_legacy OR control.rollout_mode <> 'reusable_only')
        ),
        cleanup_anomalies AS (
            SELECT route_table.id
            FROM loyal_yield.route_lookup_tables route_table
            WHERE route_table.cluster = $1
              AND route_table.authority = $2
              AND route_table.family_id IS NULL
              AND route_table.legacy_import_run_id IS NOT NULL
              AND (
                  -- Retirement deliberately flips durable to false. Keep the
                  -- complete imported lifecycle observable so an unfinished
                  -- deactivate/close cannot disappear from semantic alerts.
                  (
                      route_table.status IN (
                          'active', 'warming', 'usable', 'retiring', 'deactivated'
                      )
                      AND route_table.updated_at <= now() - make_interval(secs => $6)
                  )
                  OR (
                      route_table.status IN ('retiring', 'deactivated', 'closed')
                      AND route_table.durable <> FALSE
                  )
                  OR (
                      route_table.status IN ('deactivated', 'closed')
                      AND (
                          route_table.deactivated_slot IS NULL
                          OR COALESCE(length(btrim(route_table.deactivate_signature)), 0) = 0
                      )
                  )
                  OR (
                      route_table.status = 'closed'
                      AND (
                          COALESCE(length(btrim(route_table.closed_signature)), 0) = 0
                          OR route_table.reclaimed_lamports IS NULL
                          OR route_table.reclaimed_lamports <= 0
                          OR route_table.close_recipient IS DISTINCT FROM $2
                      )
                  )
                  OR (
                      route_table.durable = FALSE
                      AND route_table.status NOT IN ('retiring', 'deactivated', 'closed')
                  )
                  OR route_table.status NOT IN (
                      'active', 'warming', 'usable', 'retiring', 'deactivated', 'closed'
                  )
              )
            UNION
            SELECT route_table.id
            FROM loyal_yield.lookup_table_operations operation
            JOIN policy_families family ON family.id = operation.family_id
            JOIN loyal_yield.route_lookup_tables route_table
              ON route_table.id = operation.route_lookup_table_id
            WHERE operation.operation_kind IN ('deactivate', 'close')
              AND operation.operation_state = 'permanent_failure'
        )
        SELECT
            (SELECT count(*) FROM shared_health) AS shared_head_count,
            (SELECT count(*) FROM shared_health
             WHERE readiness_state = 'active'
               AND target_generation = active_generation
               AND active_table_count > 0
               AND usable_address_count = desired_address_count) AS healthy_shared_head_count,
            (SELECT count(DISTINCT identity) FROM overdue_coverage) AS missing_coverage_count,
            COALESCE((SELECT floor(extract(epoch FROM now() - min(since)))::BIGINT
                      FROM overdue_coverage), 0) AS oldest_missing_coverage_seconds,
            (SELECT count(*) FROM operation_health
             WHERE operation_state NOT IN ('complete', 'permanent_failure', 'cancelled'))
                AS operation_backlog_count,
            COALESCE((SELECT floor(extract(epoch FROM now() - min(created_at)))::BIGINT
                      FROM operation_health
                      WHERE operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')), 0)
                AS oldest_operation_seconds,
            (SELECT count(*) FROM operation_health
             WHERE operation_state = 'permanent_failure') AS permanent_operation_failure_count,
            (SELECT count(*) FROM low_headroom) AS low_headroom_table_count,
            (SELECT min(headroom)::BIGINT FROM low_headroom) AS minimum_headroom,
            (SELECT count(*)
             FROM loyal_yield.lookup_table_shared_market_physical_drifts drift
             JOIN policy_families family ON family.id = drift.family_id
             WHERE drift.resolution_state = 'open') AS open_physical_drift_count,
            COALESCE((SELECT sum(reservation.reserved_lamports)
                      FROM loyal_yield.lookup_table_cluster_budget_reservations reservation
                      JOIN operation_health operation ON operation.id = reservation.operation_id
                      WHERE reservation.cluster = $1
                        AND reservation.reserved_at >= now() - make_interval(secs => $5)
                        AND operation.operation_state <> 'cancelled'), 0)::BIGINT
                AS budget_used_lamports,
            (SELECT count(*) FROM operation_health
             WHERE error_code ILIKE '%budget%'
               AND updated_at >= now() - make_interval(secs => $5))
                AS budget_exhaustion_count,
            COALESCE((SELECT jsonb_agg(id ORDER BY id) FROM orphaned), '[]'::jsonb)
                AS orphaned_table_ids,
            (
                (SELECT count(*) FROM rollout_fallback)
                + (SELECT count(*)
                   FROM loyal_yield.lookup_table_route_readiness_current readiness
                   WHERE readiness.cluster = $1
                     AND readiness.selection_kind = 'legacy')
                + CASE WHEN NOT EXISTS (
                    SELECT 1 FROM loyal_yield.lookup_table_rollout_controls control
                    WHERE control.cluster = $1
                      AND control.vault_id IS NULL
                      AND control.rollout_mode = 'reusable_only'
                      AND control.force_legacy = FALSE
                  ) THEN 1 ELSE 0 END
            )::BIGINT AS fallback_use_count,
            (SELECT count(*) FROM cleanup_anomalies) AS cleanup_anomaly_count,
            COALESCE((SELECT jsonb_agg(id ORDER BY id) FROM cleanup_anomalies), '[]'::jsonb)
                AS cleanup_anomaly_table_ids
        "#,
    )
    .bind(cluster)
    .bind(policy_pubkey)
    .bind(missing_grace)
    .bind(capacity_headroom)
    .bind(budget_window)
    .bind(cleanup_grace)
    .fetch_one(pool)
    .await?;

    let expectation_rows = sqlx::query(
        r#"
        SELECT route_table.id,
               route_table.table_address,
               family.provisioning_authority AS expected_authority,
               route_table.addresses,
               route_table.desired_state,
               route_table.mutation_epoch,
               route_table.authority = family.provisioning_authority
                   AS registry_authority_matches,
               EXISTS (
                   SELECT 1 FROM loyal_yield.lookup_table_operations operation
                   WHERE operation.route_lookup_table_id = route_table.id
                     AND operation.operation_state NOT IN (
                         'complete', 'permanent_failure', 'cancelled'
                     )
               ) AS has_inflight_operation,
               (
                   route_table.desired_state NOT IN ('closed', 'failed')
                   AND NOT (
                       route_table.generation = family.active_generation
                       AND (
                           family.kind = 'shared_market'
                           OR route_table.accepting_allocations = TRUE
                           OR EXISTS (
                               SELECT 1 FROM loyal_yield.lookup_table_vault_bindings binding
                               WHERE binding.route_lookup_table_id = route_table.id
                                 AND binding.lifecycle_state IN (
                                     'preparing', 'warming', 'active', 'standby', 'retiring'
                                 )
                           )
                       )
                   )
                   AND NOT (
                       route_table.generation = family.previous_generation
                       AND COALESCE(route_table.rollback_until, family.rollback_until) > now()
                   )
                   AND NOT EXISTS (
                       SELECT 1 FROM loyal_yield.lookup_table_operations operation
                       WHERE operation.route_lookup_table_id = route_table.id
                         AND operation.operation_state NOT IN (
                             'complete', 'permanent_failure', 'cancelled'
                         )
                   )
                   AND NOT EXISTS (
                       SELECT 1 FROM loyal_yield.lookup_table_usage_leases lease
                       WHERE lease.route_lookup_table_id = route_table.id
                         AND lease.released_at IS NULL
                         AND lease.expires_at > now()
                   )
               ) AS orphaned
        FROM loyal_yield.route_lookup_tables route_table
        JOIN loyal_yield.lookup_table_families family ON family.id = route_table.family_id
        WHERE family.cluster = $1
          AND family.provisioning_authority = $2
          AND route_table.desired_state NOT IN ('closed', 'failed')
        ORDER BY route_table.id
        "#,
    )
    .bind(cluster)
    .bind(policy_pubkey)
    .fetch_all(pool)
    .await?;

    let mut physical_expectations = Vec::with_capacity(expectation_rows.len());
    for row in expectation_rows {
        let addresses: Value = row.try_get("addresses")?;
        let expected_addresses =
            serde_json::from_value::<Vec<String>>(addresses).map_err(|error| {
                OrchestratorError::StoreInvariant(format!(
                    "reusable ALT registry contains non-string ordered addresses: {error}"
                ))
            })?;
        physical_expectations.push(LookupTablePhysicalExpectation {
            table_id: row.try_get("id")?,
            table_address: row.try_get("table_address")?,
            expected_authority: row.try_get("expected_authority")?,
            expected_addresses,
            desired_state: row.try_get("desired_state")?,
            mutation_epoch: row.try_get("mutation_epoch")?,
            registry_authority_matches: row.try_get("registry_authority_matches")?,
            has_inflight_operation: row.try_get("has_inflight_operation")?,
            orphaned: row.try_get("orphaned")?,
        });
    }

    Ok(LookupTableAlertSnapshot {
        cluster: cluster.to_owned(),
        policy_pubkey: policy_pubkey.to_owned(),
        shared_head_count: row.try_get("shared_head_count")?,
        healthy_shared_head_count: row.try_get("healthy_shared_head_count")?,
        missing_coverage_count: row.try_get("missing_coverage_count")?,
        oldest_missing_coverage_seconds: row.try_get("oldest_missing_coverage_seconds")?,
        operation_backlog_count: row.try_get("operation_backlog_count")?,
        oldest_operation_seconds: row.try_get("oldest_operation_seconds")?,
        permanent_operation_failure_count: row.try_get("permanent_operation_failure_count")?,
        low_headroom_table_count: row.try_get("low_headroom_table_count")?,
        minimum_headroom: row.try_get("minimum_headroom")?,
        open_physical_drift_count: row.try_get("open_physical_drift_count")?,
        budget_used_lamports: row.try_get("budget_used_lamports")?,
        budget_exhaustion_count: row.try_get("budget_exhaustion_count")?,
        orphaned_table_ids: json_i64_array(row.try_get("orphaned_table_ids")?)?,
        fallback_use_count: row.try_get("fallback_use_count")?,
        cleanup_anomaly_count: row.try_get("cleanup_anomaly_count")?,
        cleanup_anomaly_table_ids: json_i64_array(row.try_get("cleanup_anomaly_table_ids")?)?,
        physical_expectations,
    })
}

fn json_i64_array(value: Value) -> Result<Vec<i64>, OrchestratorError> {
    serde_json::from_value(value).map_err(|error| {
        OrchestratorError::StoreInvariant(format!(
            "reusable ALT alert aggregate is not an integer array: {error}"
        ))
    })
}

#[allow(clippy::too_many_arguments)]
pub async fn record_lookup_table_alert_observation(
    pool: &PgPool,
    cluster: &str,
    policy_pubkey: &str,
    scope_key: &str,
    observation: &LookupTableAlertObservation,
    observed_at: DateTime<Utc>,
    reminder_interval: Duration,
    delivery_max_attempts: i32,
) -> Result<LookupTableAlertTransition, OrchestratorError> {
    let mut tx = pool.begin().await?;
    // PostgreSQL TEXT rejects NUL bytes. JSON gives this advisory-lock
    // identity an unambiguous, escaped tuple representation even when an
    // operator-supplied scope contains separators.
    let incident_lock_key = json!([
        "reusable-alt-alert",
        cluster,
        policy_pubkey,
        observation.condition.as_str(),
        scope_key,
    ])
    .to_string();
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")
        .bind(incident_lock_key)
        .execute(&mut *tx)
        .await?;
    let existing = load_incident_for_update(
        &mut tx,
        cluster,
        policy_pubkey,
        observation.condition,
        scope_key,
    )
    .await?;
    let observed_at = existing
        .as_ref()
        .map(|incident| observed_at.max(incident.last_observed_at))
        .unwrap_or(observed_at);
    let reminder_interval = chrono::Duration::from_std(reminder_interval).map_err(|error| {
        OrchestratorError::StoreInvariant(format!("alert reminder interval is invalid: {error}"))
    })?;
    let planned = plan_transition(
        existing.as_ref(),
        observation,
        observed_at,
        reminder_interval,
    );

    if existing.is_none() && !observation.active {
        tx.commit().await?;
        return Ok(LookupTableAlertTransition {
            incident_id: None,
            event_kind: None,
            revision: None,
        });
    }
    if existing
        .as_ref()
        .is_some_and(|incident| incident.status == "resolved")
        && !observation.active
    {
        let incident_id = existing.as_ref().map(|incident| incident.id);
        tx.commit().await?;
        return Ok(LookupTableAlertTransition {
            incident_id,
            event_kind: None,
            revision: None,
        });
    }

    let incident = persist_incident(
        &mut tx,
        cluster,
        policy_pubkey,
        scope_key,
        observation,
        observed_at,
        existing.as_ref(),
        planned,
    )
    .await?;

    if let Some(event_kind) = planned.event_kind() {
        let payload = incident_webhook_payload(
            incident.id,
            cluster,
            policy_pubkey,
            scope_key,
            observation,
            event_kind,
            incident.revision,
            incident.first_observed_at,
            observed_at,
        );
        let idempotency_key = format!(
            "incident:{}:revision:{}:{}",
            incident.id,
            incident.revision,
            event_kind.as_str()
        );
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_alert_deliveries
                (incident_id, incident_revision, alert_condition, event_kind,
                 idempotency_key, cluster, policy_pubkey, payload, max_attempts,
                 next_attempt_at)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
            ON CONFLICT (idempotency_key) DO NOTHING
            "#,
        )
        .bind(incident.id)
        .bind(incident.revision)
        .bind(observation.condition.as_str())
        .bind(event_kind.as_str())
        .bind(idempotency_key)
        .bind(cluster)
        .bind(policy_pubkey)
        .bind(payload)
        .bind(delivery_max_attempts)
        .execute(&mut *tx)
        .await?;
    }

    tx.commit().await?;
    Ok(LookupTableAlertTransition {
        incident_id: Some(incident.id),
        event_kind: planned.event_kind(),
        revision: planned.revision(),
    })
}

async fn load_incident_for_update(
    tx: &mut Transaction<'_, Postgres>,
    cluster: &str,
    policy_pubkey: &str,
    condition: LookupTableAlertCondition,
    scope_key: &str,
) -> Result<Option<ExistingIncident>, OrchestratorError> {
    let row = sqlx::query(
        r#"
        SELECT id, incident_status, fingerprint, revision, last_observed_at, last_notified_at,
               first_observed_at, opened_at, occurrence_count
        FROM loyal_yield.lookup_table_alert_incidents
        WHERE cluster = $1
          AND policy_pubkey = $2
          AND alert_condition = $3
          AND scope_key = $4
        FOR UPDATE
        "#,
    )
    .bind(cluster)
    .bind(policy_pubkey)
    .bind(condition.as_str())
    .bind(scope_key)
    .fetch_optional(&mut **tx)
    .await?;

    row.map(|row| {
        Ok(ExistingIncident {
            id: row.try_get("id")?,
            status: row.try_get("incident_status")?,
            fingerprint: row.try_get("fingerprint")?,
            revision: row.try_get("revision")?,
            last_observed_at: row.try_get("last_observed_at")?,
            last_notified_at: row.try_get("last_notified_at")?,
            first_observed_at: row.try_get("first_observed_at")?,
            opened_at: row.try_get("opened_at")?,
            occurrence_count: row.try_get("occurrence_count")?,
        })
    })
    .transpose()
}

#[allow(clippy::too_many_arguments)]
async fn persist_incident(
    tx: &mut Transaction<'_, Postgres>,
    cluster: &str,
    policy_pubkey: &str,
    scope_key: &str,
    observation: &LookupTableAlertObservation,
    observed_at: DateTime<Utc>,
    existing: Option<&ExistingIncident>,
    planned: PlannedTransition,
) -> Result<ExistingIncident, OrchestratorError> {
    if existing.is_none() {
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_alert_incidents
                (cluster, policy_pubkey, alert_condition, scope_key,
                 incident_status, severity, fingerprint, summary, details,
                 first_observed_at, opened_at, last_observed_at,
                 last_notified_at, occurrence_count, revision)
            VALUES ($1, $2, $3, $4, 'open', $5, $6, $7, $8,
                    $9, $9, $9, $9, 1, 1)
            RETURNING id, incident_status, fingerprint, revision,
                      last_observed_at, last_notified_at, first_observed_at, opened_at,
                      occurrence_count
            "#,
        )
        .bind(cluster)
        .bind(policy_pubkey)
        .bind(observation.condition.as_str())
        .bind(scope_key)
        .bind(observation.severity.as_str())
        .bind(&observation.fingerprint)
        .bind(&observation.summary)
        .bind(&observation.details)
        .bind(observed_at)
        .fetch_one(&mut **tx)
        .await?;
        return incident_from_row(&row);
    }

    let existing = existing.expect("checked above");
    let resolving = matches!(planned, PlannedTransition::Resolved(_));
    let reopening = matches!(planned, PlannedTransition::Open(_));
    let notifying = planned.event_kind().is_some();
    let revision = planned.revision().unwrap_or(existing.revision);
    let status = if resolving { "resolved" } else { "open" };
    let severity = if resolving {
        LookupTableAlertSeverity::Info
    } else {
        observation.severity
    };
    let occurrence_count = if observation.active {
        existing.occurrence_count.saturating_add(1)
    } else {
        existing.occurrence_count
    };
    let opened_at = if reopening {
        observed_at
    } else {
        existing.opened_at
    };
    let resolved_at = resolving.then_some(observed_at);
    let last_notified_at = if notifying {
        observed_at
    } else {
        existing.last_notified_at
    };

    let row = sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_alert_incidents
        SET incident_status = $2,
            severity = $3,
            fingerprint = $4,
            summary = $5,
            details = $6,
            opened_at = $7,
            last_observed_at = $8,
            last_notified_at = $9,
            occurrence_count = $10,
            revision = $11,
            resolved_at = $12,
            updated_at = now()
        WHERE id = $1
        RETURNING id, incident_status, fingerprint, revision,
                  last_observed_at, last_notified_at, first_observed_at, opened_at,
                  occurrence_count
        "#,
    )
    .bind(existing.id)
    .bind(status)
    .bind(severity.as_str())
    .bind(&observation.fingerprint)
    .bind(&observation.summary)
    .bind(&observation.details)
    .bind(opened_at)
    .bind(observed_at)
    .bind(last_notified_at)
    .bind(occurrence_count)
    .bind(revision)
    .bind(resolved_at)
    .fetch_one(&mut **tx)
    .await?;
    incident_from_row(&row)
}

fn incident_from_row(row: &sqlx::postgres::PgRow) -> Result<ExistingIncident, OrchestratorError> {
    Ok(ExistingIncident {
        id: row.try_get("id")?,
        status: row.try_get("incident_status")?,
        fingerprint: row.try_get("fingerprint")?,
        revision: row.try_get("revision")?,
        last_observed_at: row.try_get("last_observed_at")?,
        last_notified_at: row.try_get("last_notified_at")?,
        first_observed_at: row.try_get("first_observed_at")?,
        opened_at: row.try_get("opened_at")?,
        occurrence_count: row.try_get("occurrence_count")?,
    })
}

#[allow(clippy::too_many_arguments)]
fn incident_webhook_payload(
    incident_id: i64,
    cluster: &str,
    policy_pubkey: &str,
    scope_key: &str,
    observation: &LookupTableAlertObservation,
    event_kind: LookupTableAlertEventKind,
    revision: i64,
    first_observed_at: DateTime<Utc>,
    observed_at: DateTime<Utc>,
) -> Value {
    json!({
        "schemaVersion": 1,
        "source": "loyal-yield-routing",
        "subsystem": "reusable_alts",
        "event": event_kind.as_str(),
        "condition": observation.condition.as_str(),
        "severity": if event_kind == LookupTableAlertEventKind::Resolved {
            LookupTableAlertSeverity::Info.as_str()
        } else {
            observation.severity.as_str()
        },
        "cluster": cluster,
        "policyPubkey": policy_pubkey,
        "scope": scope_key,
        "incidentId": incident_id,
        "revision": revision,
        "fingerprint": observation.fingerprint,
        "summary": observation.summary,
        "details": observation.details,
        "firstObservedAt": first_observed_at,
        "observedAt": observed_at,
    })
}

pub async fn enqueue_lookup_table_test_alerts(
    pool: &PgPool,
    cluster: &str,
    policy_pubkey: &str,
    test_id: &str,
    rules: &[LookupTableAlertRuleRecord],
    max_attempts: i32,
    observed_at: DateTime<Utc>,
) -> Result<Vec<i64>, OrchestratorError> {
    let by_condition = rules
        .iter()
        .map(|rule| (rule.condition, rule))
        .collect::<BTreeMap<_, _>>();
    if by_condition.len() != LookupTableAlertCondition::ALL.len()
        || LookupTableAlertCondition::ALL
            .iter()
            .any(|condition| !by_condition.contains_key(condition))
    {
        return Err(OrchestratorError::StoreInvariant(
            "test-alert delivery requires the exact nine-rule catalog".to_owned(),
        ));
    }

    let mut tx = pool.begin().await?;
    let mut delivery_ids = Vec::with_capacity(LookupTableAlertCondition::ALL.len());
    for condition in LookupTableAlertCondition::ALL {
        let rule = by_condition
            .get(&condition)
            .expect("exact catalog checked above");
        let payload = json!({
            "schemaVersion": 1,
            "source": "loyal-yield-routing",
            "subsystem": "reusable_alts",
            "event": "test",
            "condition": condition.as_str(),
            "severity": rule.severity.as_str(),
            "cluster": cluster,
            "policyPubkey": policy_pubkey,
            "testId": test_id,
            "ruleVersion": rule.rule_version,
            "ruleEnabled": rule.enabled,
            "summary": format!("reusable ALT {} alert delivery test", condition.as_str()),
            "observedAt": observed_at,
            "mutatesLiveIncidentState": false,
            "mutatesRouteDemand": false,
            "mutatesLookupTables": false,
        });
        let idempotency_key = format!(
            "test:{cluster}:{policy_pubkey}:{test_id}:{}",
            condition.as_str()
        );
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_alert_deliveries
                (incident_id, incident_revision, alert_condition, event_kind,
                 idempotency_key, cluster, policy_pubkey, payload, max_attempts,
                 next_attempt_at)
            VALUES (NULL, NULL, $1, 'test', $2, $3, $4, $5, $6, now())
            ON CONFLICT (idempotency_key) DO UPDATE
            SET idempotency_key = EXCLUDED.idempotency_key
            RETURNING id
            "#,
        )
        .bind(condition.as_str())
        .bind(idempotency_key)
        .bind(cluster)
        .bind(policy_pubkey)
        .bind(payload)
        .bind(max_attempts)
        .fetch_one(&mut *tx)
        .await?;
        delivery_ids.push(row.try_get("id")?);
    }
    tx.commit().await?;
    Ok(delivery_ids)
}

pub async fn lease_lookup_table_alert_deliveries(
    pool: &PgPool,
    worker_id: &str,
    limit: i64,
    lease_duration: Duration,
) -> Result<Vec<LeasedLookupTableAlertDelivery>, OrchestratorError> {
    let lease_seconds = duration_seconds_i64(lease_duration);
    sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_alert_deliveries
        SET delivery_state = 'dead_letter',
            lease_owner = NULL,
            lease_expires_at = NULL,
            last_error = COALESCE(last_error, 'delivery lease expired at attempt limit'),
            updated_at = now()
        WHERE delivery_state = 'leased'
          AND lease_expires_at <= now()
          AND attempt_count >= max_attempts
        "#,
    )
    .execute(pool)
    .await?;
    let rows = sqlx::query(
        r#"
        WITH candidates AS (
            SELECT id
            FROM loyal_yield.lookup_table_alert_deliveries
            WHERE (
                delivery_state IN ('pending', 'retry_wait')
                AND next_attempt_at <= now()
            ) OR (
                delivery_state = 'leased'
                AND lease_expires_at <= now()
                AND attempt_count < max_attempts
            )
            ORDER BY next_attempt_at, id
            LIMIT $2
            FOR UPDATE SKIP LOCKED
        )
        UPDATE loyal_yield.lookup_table_alert_deliveries delivery
        SET delivery_state = 'leased',
            lease_owner = $1,
            lease_expires_at = now() + make_interval(secs => $3),
            fencing_token = delivery.fencing_token + 1,
            attempt_count = delivery.attempt_count + 1,
            updated_at = now()
        FROM candidates
        WHERE delivery.id = candidates.id
        RETURNING delivery.id, delivery.fencing_token, delivery.attempt_count,
                  delivery.max_attempts, delivery.payload
        "#,
    )
    .bind(worker_id)
    .bind(limit)
    .bind(lease_seconds)
    .fetch_all(pool)
    .await?;

    rows.into_iter()
        .map(|row| {
            Ok(LeasedLookupTableAlertDelivery {
                id: row.try_get("id")?,
                fencing_token: row.try_get("fencing_token")?,
                attempt_count: row.try_get("attempt_count")?,
                max_attempts: row.try_get("max_attempts")?,
                payload: row.try_get("payload")?,
            })
        })
        .collect()
}

/// Leases only the requested outbox rows. Safe alert verification uses this
/// path so an older production backlog cannot be mistaken for delivery of the
/// nine test-rule identities and is never consumed as a side effect.
pub async fn lease_lookup_table_alert_deliveries_by_ids(
    pool: &PgPool,
    worker_id: &str,
    delivery_ids: &[i64],
    lease_duration: Duration,
) -> Result<Vec<LeasedLookupTableAlertDelivery>, OrchestratorError> {
    if delivery_ids.is_empty() {
        return Ok(Vec::new());
    }
    let unique_ids = delivery_ids.iter().copied().collect::<BTreeSet<_>>();
    if unique_ids.len() != delivery_ids.len() || unique_ids.iter().any(|id| *id <= 0) {
        return Err(OrchestratorError::StoreInvariant(
            "targeted reusable ALT alert delivery IDs must be unique and positive".to_owned(),
        ));
    }
    let delivery_ids = unique_ids.into_iter().collect::<Vec<_>>();
    let lease_seconds = duration_seconds_i64(lease_duration);
    sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_alert_deliveries
        SET delivery_state = 'dead_letter',
            lease_owner = NULL,
            lease_expires_at = NULL,
            last_error = COALESCE(last_error, 'delivery lease expired at attempt limit'),
            updated_at = now()
        WHERE id = ANY($1::BIGINT[])
          AND delivery_state = 'leased'
          AND lease_expires_at <= now()
          AND attempt_count >= max_attempts
        "#,
    )
    .bind(&delivery_ids)
    .execute(pool)
    .await?;
    let rows = sqlx::query(
        r#"
        WITH candidates AS (
            SELECT id
            FROM loyal_yield.lookup_table_alert_deliveries
            WHERE id = ANY($2::BIGINT[])
              AND (
                  (
                      delivery_state IN ('pending', 'retry_wait')
                      AND next_attempt_at <= now()
                  )
                  OR (
                      delivery_state = 'leased'
                      AND lease_expires_at <= now()
                      AND attempt_count < max_attempts
                  )
              )
            ORDER BY id
            FOR UPDATE SKIP LOCKED
        )
        UPDATE loyal_yield.lookup_table_alert_deliveries delivery
        SET delivery_state = 'leased',
            lease_owner = $1,
            lease_expires_at = now() + make_interval(secs => $3),
            fencing_token = delivery.fencing_token + 1,
            attempt_count = delivery.attempt_count + 1,
            updated_at = now()
        FROM candidates
        WHERE delivery.id = candidates.id
        RETURNING delivery.id, delivery.fencing_token, delivery.attempt_count,
                  delivery.max_attempts, delivery.payload
        "#,
    )
    .bind(worker_id)
    .bind(&delivery_ids)
    .bind(lease_seconds)
    .fetch_all(pool)
    .await?;

    rows.into_iter()
        .map(|row| {
            Ok(LeasedLookupTableAlertDelivery {
                id: row.try_get("id")?,
                fencing_token: row.try_get("fencing_token")?,
                attempt_count: row.try_get("attempt_count")?,
                max_attempts: row.try_get("max_attempts")?,
                payload: row.try_get("payload")?,
            })
        })
        .collect()
}

pub async fn complete_lookup_table_alert_delivery(
    pool: &PgPool,
    delivery_id: i64,
    fencing_token: i64,
    http_status: i32,
) -> Result<(), OrchestratorError> {
    let result = sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_alert_deliveries
        SET delivery_state = 'delivered',
            delivered_via = 'webhook',
            http_status = $3,
            delivered_at = now(),
            lease_owner = NULL,
            lease_expires_at = NULL,
            last_error = NULL,
            updated_at = now()
        WHERE id = $1
          AND fencing_token = $2
          AND delivery_state = 'leased'
        "#,
    )
    .bind(delivery_id)
    .bind(fencing_token)
    .bind(http_status)
    .execute(pool)
    .await?;
    require_fenced_delivery_update(result.rows_affected(), delivery_id, fencing_token)
}

/// Records the explicit Render process-failure delivery channel. The caller
/// must emit the payload as one sanitized JSON record and then terminate with
/// a nonzero exit code; marking this row is not permission to continue.
pub async fn complete_lookup_table_render_failure_delivery(
    pool: &PgPool,
    delivery_id: i64,
    fencing_token: i64,
) -> Result<(), OrchestratorError> {
    let result = sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_alert_deliveries
        SET delivery_state = 'delivered',
            delivered_via = 'render_failure',
            http_status = NULL,
            delivered_at = now(),
            lease_owner = NULL,
            lease_expires_at = NULL,
            last_error = NULL,
            updated_at = now()
        WHERE id = $1
          AND fencing_token = $2
          AND delivery_state = 'leased'
        "#,
    )
    .bind(delivery_id)
    .bind(fencing_token)
    .execute(pool)
    .await?;
    require_fenced_delivery_update(result.rows_affected(), delivery_id, fencing_token)
}

pub async fn fail_lookup_table_alert_delivery(
    pool: &PgPool,
    delivery: &LeasedLookupTableAlertDelivery,
    retry_delay: Duration,
    error: &str,
    http_status: Option<i32>,
) -> Result<(), OrchestratorError> {
    let terminal = delivery.attempt_count >= delivery.max_attempts;
    let state = if terminal {
        "dead_letter"
    } else {
        "retry_wait"
    };
    let retry_seconds = duration_seconds_i64(retry_delay);
    let bounded_error = error.chars().take(512).collect::<String>();
    let result = sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_alert_deliveries
        SET delivery_state = $3,
            next_attempt_at = CASE
                WHEN $3 = 'retry_wait' THEN now() + make_interval(secs => $4)
                ELSE next_attempt_at
            END,
            http_status = $5,
            last_error = $6,
            lease_owner = NULL,
            lease_expires_at = NULL,
            updated_at = now()
        WHERE id = $1
          AND fencing_token = $2
          AND delivery_state = 'leased'
        "#,
    )
    .bind(delivery.id)
    .bind(delivery.fencing_token)
    .bind(state)
    .bind(retry_seconds)
    .bind(http_status)
    .bind(bounded_error)
    .execute(pool)
    .await?;
    require_fenced_delivery_update(result.rows_affected(), delivery.id, delivery.fencing_token)
}

fn require_fenced_delivery_update(
    rows_affected: u64,
    delivery_id: i64,
    fencing_token: i64,
) -> Result<(), OrchestratorError> {
    if rows_affected == 1 {
        Ok(())
    } else {
        Err(OrchestratorError::StoreInvariant(format!(
            "stale reusable ALT alert delivery lease for delivery {delivery_id} fence {fencing_token}"
        )))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn active_observation(fingerprint: &str) -> LookupTableAlertObservation {
        LookupTableAlertObservation {
            condition: LookupTableAlertCondition::MissingCoverage,
            active: true,
            severity: LookupTableAlertSeverity::Warning,
            fingerprint: fingerprint.to_owned(),
            summary: "coverage missing".to_owned(),
            details: json!({"count": 1}),
        }
    }

    fn existing(status: &str, fingerprint: &str, revision: i64) -> ExistingIncident {
        let now = DateTime::from_timestamp(1_700_000_000, 0).unwrap();
        ExistingIncident {
            id: 7,
            status: status.to_owned(),
            fingerprint: fingerprint.to_owned(),
            revision,
            last_observed_at: now,
            last_notified_at: now,
            first_observed_at: now,
            opened_at: now,
            occurrence_count: 1,
        }
    }

    #[test]
    fn alert_contract_has_exactly_the_nine_operator_conditions() {
        assert_eq!(
            LookupTableAlertCondition::ALL.map(LookupTableAlertCondition::as_str),
            [
                "readiness_regression",
                "missing_coverage",
                "operation_backlog",
                "capacity_headroom",
                "authority_prefix_drift",
                "provisioning_budget",
                "orphaned_tables",
                "fallback_use",
                "cleanup_anomalies",
            ]
        );
    }

    #[test]
    fn repeated_evidence_is_quiet_until_reminder_and_changed_evidence_reminds_immediately() {
        let incident = existing("open", "same", 3);
        let before_reminder = incident.last_notified_at + chrono::Duration::minutes(59);
        assert_eq!(
            plan_transition(
                Some(&incident),
                &active_observation("same"),
                before_reminder,
                chrono::Duration::hours(1),
            ),
            PlannedTransition::None
        );
        assert_eq!(
            plan_transition(
                Some(&incident),
                &active_observation("same"),
                incident.last_notified_at + chrono::Duration::hours(1),
                chrono::Duration::hours(1),
            ),
            PlannedTransition::Reminder(4)
        );
        assert_eq!(
            plan_transition(
                Some(&incident),
                &active_observation("changed"),
                before_reminder,
                chrono::Duration::hours(1),
            ),
            PlannedTransition::Reminder(4)
        );
    }

    #[test]
    fn incident_lifecycle_opens_resolves_and_reopens_monotonically() {
        let now = Utc::now();
        let active = active_observation("active");
        assert_eq!(
            plan_transition(None, &active, now, chrono::Duration::hours(1)),
            PlannedTransition::Open(1)
        );

        let open = existing("open", "active", 1);
        let mut healthy = active.clone();
        healthy.active = false;
        healthy.fingerprint = "healthy".to_owned();
        assert_eq!(
            plan_transition(Some(&open), &healthy, now, chrono::Duration::hours(1)),
            PlannedTransition::Resolved(2)
        );

        let resolved = existing("resolved", "healthy", 2);
        assert_eq!(
            plan_transition(Some(&resolved), &active, now, chrono::Duration::hours(1)),
            PlannedTransition::Open(3)
        );
    }

    #[test]
    fn fingerprint_is_stable_across_json_object_key_order() {
        let left = json!({"a": 1, "b": {"x": 2, "y": 3}});
        let right: Value = serde_json::from_str(r#"{"b":{"y":3,"x":2},"a":1}"#).unwrap();
        assert_eq!(
            lookup_table_alert_fingerprint(LookupTableAlertCondition::FallbackUse, &left),
            lookup_table_alert_fingerprint(LookupTableAlertCondition::FallbackUse, &right)
        );
    }

    #[test]
    fn evaluator_always_emits_one_observation_per_contract_condition() {
        let snapshot = LookupTableAlertSnapshot {
            cluster: "mainnet-beta".to_owned(),
            policy_pubkey: "policy".to_owned(),
            shared_head_count: 1,
            healthy_shared_head_count: 1,
            missing_coverage_count: 0,
            oldest_missing_coverage_seconds: 0,
            operation_backlog_count: 0,
            oldest_operation_seconds: 0,
            permanent_operation_failure_count: 0,
            low_headroom_table_count: 0,
            minimum_headroom: None,
            open_physical_drift_count: 0,
            budget_used_lamports: 0,
            budget_exhaustion_count: 0,
            orphaned_table_ids: Vec::new(),
            fallback_use_count: 0,
            cleanup_anomaly_count: 0,
            cleanup_anomaly_table_ids: Vec::new(),
            physical_expectations: Vec::new(),
        };
        let observations = evaluate_lookup_table_alerts(
            &snapshot,
            &LookupTableRpcAudit::default(),
            &LookupTableAlertThresholds::default(),
        );
        assert_eq!(observations.len(), 9);
        assert_eq!(
            observations
                .iter()
                .map(|observation| observation.condition)
                .collect::<Vec<_>>(),
            LookupTableAlertCondition::ALL
        );
        assert!(observations.iter().all(|observation| !observation.active));
    }

    #[test]
    fn durable_rule_version_and_enable_state_control_the_observation() {
        let observation = active_observation("evaluation");
        let enabled_rule = LookupTableAlertRuleRecord {
            condition: LookupTableAlertCondition::MissingCoverage,
            rule_version: 4,
            enabled: true,
            severity: LookupTableAlertSeverity::Critical,
            description: "test rule".to_owned(),
            configuration: json!({}),
        };
        let enabled = apply_lookup_table_alert_rule(observation.clone(), &enabled_rule).unwrap();
        assert!(enabled.active);
        assert_eq!(enabled.severity, LookupTableAlertSeverity::Critical);
        assert_eq!(enabled.details["ruleVersion"], 4);
        assert_eq!(enabled.details["ruleEnabled"], true);
        assert_ne!(enabled.fingerprint, observation.fingerprint);

        let disabled = apply_lookup_table_alert_rule(
            observation,
            &LookupTableAlertRuleRecord {
                rule_version: 5,
                enabled: false,
                ..enabled_rule
            },
        )
        .unwrap();
        assert!(!disabled.active);
        assert_eq!(disabled.severity, LookupTableAlertSeverity::Info);
        assert_eq!(disabled.details["ruleVersion"], 5);
        assert_eq!(disabled.details["ruleEnabled"], false);
        assert!(disabled.summary.contains("disabled"));
    }
}
