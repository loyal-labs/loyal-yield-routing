//! Planner-to-executor source evidence contract for fleet routes.
//!
//! Planner freshness fields describe whichever source kind was observed. They
//! become idle-account fences only for idle-vault work; reserve-position work
//! is fenced by its immutable source snapshot.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use thiserror::Error;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum FleetRouteSourceKind {
    ReservePosition,
    IdleVaultUsdc,
}

impl FleetRouteSourceKind {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::ReservePosition => "reserve_position",
            Self::IdleVaultUsdc => "idle_vault_usdc",
        }
    }

    pub const fn route_kind(self) -> &'static str {
        match self {
            Self::ReservePosition => "same_mint",
            Self::IdleVaultUsdc => "idle_vault_deposit",
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetRouteSourceEvidence {
    pub expected_idle_token_account: Option<String>,
    pub expected_idle_observed_slot: Option<i64>,
    pub expected_idle_observed_at: Option<DateTime<Utc>>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetRouteSourceContractFixture {
    pub source_kind: FleetRouteSourceKind,
    pub source_reserve: Option<String>,
    pub source_snapshot_id: Option<i64>,
    pub execution_plan: Value,
    pub projected_evidence: FleetRouteSourceEvidence,
    pub validation_error: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct FleetRouteSourceContractFixtures {
    pub reserve_position: FleetRouteSourceContractFixture,
    pub idle_vault_usdc: FleetRouteSourceContractFixture,
    pub contaminated_reserve: FleetRouteSourceContractFixture,
    pub mismatched_route_kind: FleetRouteSourceContractFixture,
}

#[derive(Debug, Error)]
pub enum FleetRouteSourceEvidenceError {
    #[error("invalid execution_plan.source_observed_at: {0}")]
    InvalidObservedAt(String),
}

/// Parses the planner-owned source discriminator and proves that its route
/// discriminator selects the same execution and reconciliation path.
pub fn validate_fleet_route_kind_binding(plan: &Value) -> Result<FleetRouteSourceKind, String> {
    let source_kind_label = optional_plan_string(plan, "source_kind")
        .ok_or_else(|| "fleet execution_plan.source_kind is required".to_owned())?;
    let source_kind = match source_kind_label.as_str() {
        "reserve_position" => FleetRouteSourceKind::ReservePosition,
        "idle_vault_usdc" => FleetRouteSourceKind::IdleVaultUsdc,
        other => {
            return Err(format!(
                "unsupported fleet execution_plan.source_kind {other:?}"
            ));
        }
    };
    let route_kind = optional_plan_string(plan, "kind")
        .ok_or_else(|| "fleet execution_plan.kind is required".to_owned())?;
    let expected_route_kind = source_kind.route_kind();
    if route_kind != expected_route_kind {
        return Err(format!(
            "fleet execution_plan.kind {route_kind:?} does not match source_kind {source_kind_label:?}; expected {expected_route_kind:?}"
        ));
    }
    Ok(source_kind)
}

pub fn project_fleet_route_source_evidence(
    source_kind: FleetRouteSourceKind,
    plan: &Value,
) -> Result<FleetRouteSourceEvidence, FleetRouteSourceEvidenceError> {
    let idle_token_account = optional_plan_string(plan, "idle_token_account");
    if source_kind == FleetRouteSourceKind::ReservePosition {
        // Preserve explicit cross-kind contamination so validation fails
        // closed. Generic source slot/time must not be relabelled as idle
        // evidence for a reserve-position request.
        return Ok(FleetRouteSourceEvidence {
            expected_idle_token_account: idle_token_account,
            expected_idle_observed_slot: None,
            expected_idle_observed_at: None,
        });
    }
    Ok(FleetRouteSourceEvidence {
        expected_idle_token_account: idle_token_account,
        expected_idle_observed_slot: optional_plan_i64(plan, "source_observed_slot"),
        expected_idle_observed_at: optional_plan_datetime(plan, "source_observed_at")?,
    })
}

pub fn validate_fleet_route_source_evidence(
    source_kind: FleetRouteSourceKind,
    source_reserve: Option<&str>,
    source_snapshot_id: Option<i64>,
    evidence: &FleetRouteSourceEvidence,
) -> Result<(), String> {
    match source_kind {
        FleetRouteSourceKind::ReservePosition => {
            if source_reserve.is_none_or(|reserve| reserve.trim().is_empty()) {
                return Err(
                    "same-mint reserve-position request requires a source reserve".to_owned(),
                );
            }
            if source_snapshot_id.is_none_or(|id| id <= 0) {
                return Err(
                    "same-mint reserve-position request requires a positive source snapshot"
                        .to_owned(),
                );
            }
            if evidence.expected_idle_token_account.is_some()
                || evidence.expected_idle_observed_slot.is_some()
                || evidence.expected_idle_observed_at.is_some()
            {
                return Err(
                    "same-mint reserve-position request cannot carry idle-vault evidence"
                        .to_owned(),
                );
            }
        }
        FleetRouteSourceKind::IdleVaultUsdc => {
            if source_reserve.is_some() || source_snapshot_id.is_some() {
                return Err(
                    "idle-vault request must not carry a reserve source or source snapshot"
                        .to_owned(),
                );
            }
            if evidence
                .expected_idle_token_account
                .as_deref()
                .is_none_or(|account| account.trim().is_empty())
            {
                return Err(
                    "idle-vault request requires the observed idle token account".to_owned(),
                );
            }
            if evidence
                .expected_idle_observed_slot
                .is_none_or(|slot| slot <= 0)
                || evidence.expected_idle_observed_at.is_none()
            {
                return Err(
                    "idle-vault request requires positive observed slot and observed time"
                        .to_owned(),
                );
            }
        }
    }
    Ok(())
}

/// Code-owned, deterministic raw fixtures for schema-v1 runtime evidence.
/// Consumers must recompute the contract from these fields; no PASS boolean is
/// serialized.
pub fn deterministic_fleet_route_source_contract_fixtures(
) -> Result<FleetRouteSourceContractFixtures, FleetRouteSourceEvidenceError> {
    const OBSERVED_AT: &str = "2026-07-16T03:11:11Z";
    const OBSERVED_SLOT: i64 = 433_191_369;
    const SOURCE_RESERVE: &str = "So11111111111111111111111111111111111111112";
    const IDLE_TOKEN_ACCOUNT: &str = "11111111111111111111111111111111";

    let reserve_plan = json!({
        "kind": "same_mint",
        "source_kind": "reserve_position",
        "source_observed_slot": OBSERVED_SLOT,
        "source_observed_at": OBSERVED_AT,
        "idle_token_account": null,
    });
    let idle_plan = json!({
        "kind": "idle_vault_deposit",
        "source_kind": "idle_vault_usdc",
        "source_observed_slot": OBSERVED_SLOT,
        "source_observed_at": OBSERVED_AT,
        "idle_token_account": IDLE_TOKEN_ACCOUNT,
    });
    let contaminated_reserve_plan = idle_plan
        .as_object()
        .cloned()
        .map(|mut plan| {
            plan.insert("kind".to_owned(), json!("same_mint"));
            plan.insert("source_kind".to_owned(), json!("reserve_position"));
            Value::Object(plan)
        })
        .unwrap_or_else(|| json!({}));
    let mismatched_route_kind_plan = json!({
        "kind": "idle_vault_deposit",
        "source_kind": "reserve_position",
        "source_observed_slot": OBSERVED_SLOT,
        "source_observed_at": OBSERVED_AT,
        "idle_token_account": null,
    });

    Ok(FleetRouteSourceContractFixtures {
        reserve_position: contract_fixture(
            FleetRouteSourceKind::ReservePosition,
            Some(SOURCE_RESERVE),
            Some(73_001),
            reserve_plan,
        )?,
        idle_vault_usdc: contract_fixture(
            FleetRouteSourceKind::IdleVaultUsdc,
            None,
            None,
            idle_plan,
        )?,
        contaminated_reserve: contract_fixture(
            FleetRouteSourceKind::ReservePosition,
            Some(SOURCE_RESERVE),
            Some(73_002),
            contaminated_reserve_plan,
        )?,
        mismatched_route_kind: contract_fixture(
            FleetRouteSourceKind::ReservePosition,
            Some(SOURCE_RESERVE),
            Some(73_003),
            mismatched_route_kind_plan,
        )?,
    })
}

fn contract_fixture(
    source_kind: FleetRouteSourceKind,
    source_reserve: Option<&str>,
    source_snapshot_id: Option<i64>,
    execution_plan: Value,
) -> Result<FleetRouteSourceContractFixture, FleetRouteSourceEvidenceError> {
    let projected_evidence = project_fleet_route_source_evidence(source_kind, &execution_plan)?;
    let validation_error = validate_fleet_route_kind_binding(&execution_plan)
        .err()
        .or_else(|| {
            validate_fleet_route_source_evidence(
                source_kind,
                source_reserve,
                source_snapshot_id,
                &projected_evidence,
            )
            .err()
        });
    Ok(FleetRouteSourceContractFixture {
        source_kind,
        source_reserve: source_reserve.map(ToOwned::to_owned),
        source_snapshot_id,
        execution_plan,
        projected_evidence,
        validation_error,
    })
}

fn optional_plan_string(plan: &Value, field: &str) -> Option<String> {
    plan.get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.trim().is_empty())
        .map(ToOwned::to_owned)
}

fn optional_plan_i64(plan: &Value, field: &str) -> Option<i64> {
    plan.get(field).and_then(Value::as_i64)
}

fn optional_plan_datetime(
    plan: &Value,
    field: &str,
) -> Result<Option<DateTime<Utc>>, FleetRouteSourceEvidenceError> {
    optional_plan_string(plan, field)
        .map(|value| {
            DateTime::parse_from_rfc3339(&value)
                .map(|value| value.with_timezone(&Utc))
                .map_err(|error| {
                    FleetRouteSourceEvidenceError::InvalidObservedAt(error.to_string())
                })
        })
        .transpose()
}
