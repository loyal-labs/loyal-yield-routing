use std::error::Error;

use chrono::Utc;
use loyal_actions::autonomous_vaults::{
    embedded_backyard_voltr_route_bundle, BackyardVoltrStrategy,
};
use loyal_yield_orchestrator::{
    fleet_orchestration::{
        observe_backyard_voltr_confirmed_with_rpc, SignedRouteSubmissionLease,
        SignedRouteSubmissionState,
    },
    DecisionAdvance,
};
use serde_json::Value;

use super::{voltr, SameMintRouteRuntime};

pub(super) async fn effect_absent_at_or_after(
    runtime: &SameMintRouteRuntime,
    opportunity: &loyal_yield_orchestrator::fleet_orchestration::RebalanceOpportunityRecord,
    minimum_slot: u64,
) -> Result<(bool, i64), Box<dyn Error>> {
    if !voltr::is_voltr_plan(&opportunity.execution_plan) {
        return Err("Voltr absence proof received another route kind".into());
    }
    let bundle = embedded_backyard_voltr_route_bundle()?;
    let observed =
        observe_backyard_voltr_confirmed_with_rpc(runtime.rpc.as_ref(), minimum_slot, &bundle)?;
    let strategy = strategy(&opportunity.execution_plan)?;
    let position_raw = observed
        .positions
        .iter()
        .find(|position| position.strategy == strategy)
        .map(|position| position.value_raw)
        .ok_or("Voltr absence proof is missing its selected strategy")?;
    let absent = observed.vault_total_value_raw
        == required_u64(&opportunity.execution_plan, "pre_total_value_raw")?
        && observed.idle_raw == required_u64(&opportunity.execution_plan, "pre_idle_raw")?
        && position_raw == required_u64(&opportunity.execution_plan, "pre_position_raw")?;
    Ok((absent, i64::try_from(observed.context_slot)?))
}

/// Prove the route-specific effect while leaving durability, confirmation and
/// wakeup ownership in the generic fleet lifecycle.
pub(super) async fn reconcile_if_voltr(
    runtime: &SameMintRouteRuntime,
    lease: &SignedRouteSubmissionLease,
) -> Result<Option<i64>, Box<dyn Error>> {
    if lease.submission.state != SignedRouteSubmissionState::ReconciliationPending {
        return Ok(None);
    }
    let opportunity = runtime
        .client
        .rebalance_opportunity(lease.submission.opportunity_id)
        .await?
        .ok_or("Voltr signed submission opportunity no longer exists")?;
    if !voltr::is_voltr_plan(&opportunity.execution_plan) {
        return Ok(None);
    }
    let decision_id = lease
        .submission
        .decision_id
        .ok_or("Voltr reconciliation is missing decision_id")?;
    if opportunity.decision_id != Some(decision_id) {
        return Err("Voltr opportunity, submission, and decision diverged".into());
    }
    let confirmed_slot = lease
        .submission
        .confirmed_slot
        .ok_or("Voltr reconciliation is missing confirmed_slot")?;
    let minimum_slot = u64::try_from(confirmed_slot)?;
    let bundle = embedded_backyard_voltr_route_bundle()?;
    let observed =
        observe_backyard_voltr_confirmed_with_rpc(runtime.rpc.as_ref(), minimum_slot, &bundle)?;
    let plan = &opportunity.execution_plan;
    let strategy = strategy(plan)?;
    let operation = required_string(plan, "operation")?;
    let amount_raw = required_u64(plan, "amount_raw")?;
    let pre_total_value_raw = required_u64(plan, "pre_total_value_raw")?;
    let pre_idle_raw = required_u64(plan, "pre_idle_raw")?;
    let pre_position_raw = required_u64(plan, "pre_position_raw")?;
    if observed.context_slot < minimum_slot || observed.vault != bundle.vault {
        return Err("Voltr confirmed readback is stale or belongs to another vault".into());
    }
    let post_position_raw = observed
        .positions
        .iter()
        .find(|position| position.strategy == strategy)
        .map(|position| position.value_raw)
        .ok_or("Voltr confirmed readback is missing the selected strategy")?;
    let effect_matches = match operation.as_str() {
        "deposit" => {
            pre_idle_raw.checked_sub(amount_raw) == Some(observed.idle_raw)
                && post_position_raw >= pre_position_raw.saturating_add(amount_raw)
                && observed.vault_total_value_raw >= pre_total_value_raw
        }
        "withdraw" => {
            pre_idle_raw.checked_add(amount_raw) == Some(observed.idle_raw)
                && post_position_raw < pre_position_raw
                && observed.vault_total_value_raw >= pre_total_value_raw.saturating_sub(amount_raw)
        }
        _ => false,
    };
    if !effect_matches {
        return Err(
            "Voltr confirmed idle/strategy effect does not match the exact manager leg".into(),
        );
    }
    runtime
        .client
        .advance_decision(
            decision_id,
            DecisionAdvance::Confirm {
                slot: Some(confirmed_slot),
                post_snapshot_id: None,
            },
        )
        .await?;
    loyal_yield_orchestrator::sqlx::query(
        "SELECT loyal_yield.enqueue_fleet_planning_dirty_vault($1, $2, $3, $4, $5)",
    )
    .bind(opportunity.vault_id.as_i64())
    .bind("voltr_confirmed_manager_effect")
    .bind(i64::try_from(observed.context_slot)?)
    .bind(Utc::now())
    .bind(&opportunity.cluster)
    .execute(&runtime.pool)
    .await?;
    Ok(Some(i64::try_from(observed.context_slot)?))
}

fn required_string(plan: &Value, field: &str) -> Result<String, Box<dyn Error>> {
    plan.get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .map(ToOwned::to_owned)
        .ok_or_else(|| format!("Voltr reconciliation plan is missing {field}").into())
}

fn required_u64(plan: &Value, field: &str) -> Result<u64, Box<dyn Error>> {
    let value = plan
        .get(field)
        .and_then(Value::as_i64)
        .ok_or_else(|| format!("Voltr reconciliation plan is missing {field}"))?;
    u64::try_from(value).map_err(Into::into)
}

fn strategy(plan: &Value) -> Result<BackyardVoltrStrategy, Box<dyn Error>> {
    match required_string(plan, "strategy_id")?.as_str() {
        "main" => Ok(BackyardVoltrStrategy::Main),
        "onre" => Ok(BackyardVoltrStrategy::Onre),
        "prime" => Ok(BackyardVoltrStrategy::Prime),
        "maple" => Ok(BackyardVoltrStrategy::Maple),
        _ => Err("Voltr reconciliation strategy is not admitted".into()),
    }
}
