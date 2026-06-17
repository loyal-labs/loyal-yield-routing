use std::collections::HashMap;

use serde_json::{json, Value};

use crate::types::{
    CurrentReservePosition, DecisionAdvance, DecisionStatus, DecisionTransition, PlannerConfig,
    ReserveScore, SkipReason,
};
use crate::OrchestratorError;

pub const ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY: &str = "redeemable_liquidity_amount";
pub const AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED: &str =
    "kamino_obligation_collateral_deposited_amount";

#[derive(Debug, Clone)]
pub struct PlannedDecision {
    pub source_snapshot_id: crate::types::SnapshotId,
    pub source_reserve: String,
    pub target_reserve: String,
    pub liquidity_mint: Option<String>,
    pub source_liquidity_mint: String,
    pub target_liquidity_mint: String,
    pub amount_raw: i64,
    pub source_apy_bps: i64,
    pub target_apy_bps: i64,
    pub estimated_edge_bps: i64,
    pub route_amount_semantics: String,
    pub source_amount_semantics: Option<String>,
    pub source_collateral_amount_raw: Option<i64>,
    pub redeemable_source_liquidity_amount_raw: Option<i64>,
    pub idle_vault_liquidity_amount_raw: Option<i64>,
    pub execution_plan: Value,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RouteAmountEvidence {
    pub amount_raw: i64,
    pub route_amount_semantics: String,
    pub source_amount_semantics: Option<String>,
    pub source_collateral_amount_raw: Option<i64>,
    pub redeemable_source_liquidity_amount_raw: Option<i64>,
    pub idle_vault_liquidity_amount_raw: Option<i64>,
}

pub fn draft_same_mint_decision(
    positions: &[CurrentReservePosition],
    reserve_scores: &[ReserveScore],
    config: PlannerConfig,
) -> Result<PlannedDecision, SkipReason> {
    let mut scored = HashMap::new();
    for score in reserve_scores {
        scored.insert(score.reserve.as_str(), score.supply_apy_bps);
    }

    let valued_positions = positions
        .iter()
        .filter(|position| position.has_value && position.amount_raw > 0)
        .collect::<Vec<_>>();
    if valued_positions.is_empty() {
        return Err(SkipReason::NoValueSource);
    }

    let unsupported_value_source_exists = valued_positions
        .iter()
        .any(|position| route_amount_evidence(position).is_none());
    let routeable_positions = valued_positions
        .iter()
        .filter_map(|position| {
            route_amount_evidence(position).map(|evidence| (*position, evidence))
        })
        .collect::<Vec<_>>();
    if routeable_positions.is_empty() && unsupported_value_source_exists {
        return Err(SkipReason::UnsupportedAmountSemantics);
    }
    if routeable_positions.is_empty() {
        return Err(SkipReason::NoValueSource);
    }

    let (source, evidence) = routeable_positions
        .iter()
        .max_by_key(|(_, evidence)| evidence.amount_raw)
        .cloned()
        .expect("source exists");
    let source_apy_bps = score_for(source, &scored);

    let same_mint_targets = positions
        .iter()
        .filter(|position| {
            position.reserve != source.reserve && position.liquidity_mint == source.liquidity_mint
        })
        .collect::<Vec<_>>();
    if same_mint_targets.is_empty() {
        return Err(SkipReason::CrossMintOnly);
    }

    let mut target: Option<&CurrentReservePosition> = None;
    let mut best_edge = i64::MIN;
    for candidate in same_mint_targets {
        let candidate_apy = score_for(candidate, &scored);
        let edge = candidate_apy - source_apy_bps;
        if edge < config.min_edge_bps {
            continue;
        }
        if edge > best_edge {
            best_edge = edge;
            target = Some(candidate);
        }
    }

    let Some(target) = target else {
        return Err(SkipReason::NoSameMintEdge);
    };

    let target_apy_bps = score_for(target, &scored);
    Ok(PlannedDecision {
        source_snapshot_id: source.snapshot_id,
        source_reserve: source.reserve.clone(),
        target_reserve: target.reserve.clone(),
        liquidity_mint: Some(source.liquidity_mint.clone()),
        source_liquidity_mint: source.liquidity_mint.clone(),
        target_liquidity_mint: source.liquidity_mint.clone(),
        amount_raw: evidence.amount_raw,
        source_apy_bps,
        target_apy_bps,
        estimated_edge_bps: target_apy_bps - source_apy_bps,
        route_amount_semantics: evidence.route_amount_semantics.clone(),
        source_amount_semantics: evidence.source_amount_semantics.clone(),
        source_collateral_amount_raw: evidence.source_collateral_amount_raw,
        redeemable_source_liquidity_amount_raw: evidence.redeemable_source_liquidity_amount_raw,
        idle_vault_liquidity_amount_raw: evidence.idle_vault_liquidity_amount_raw,
        execution_plan: json!({
            "kind": "same_mint",
            "source_reserve": source.reserve.clone(),
            "target_reserve": target.reserve.clone(),
            "liquidity_mint": source.liquidity_mint.clone(),
            "amount_raw": evidence.amount_raw,
            "route_amount_semantics": evidence.route_amount_semantics,
            "source_amount_semantics": evidence.source_amount_semantics,
            "source_collateral_amount_raw": evidence.source_collateral_amount_raw,
            "redeemable_source_liquidity_amount_raw": evidence.redeemable_source_liquidity_amount_raw,
            "idle_vault_liquidity_amount_raw": evidence.idle_vault_liquidity_amount_raw,
        }),
    })
}

pub fn route_amount_evidence(position: &CurrentReservePosition) -> Option<RouteAmountEvidence> {
    route_amount_evidence_from_metadata(position.amount_raw, &position.planning_metadata)
}

pub fn route_amount_evidence_from_metadata(
    amount_raw: i64,
    metadata: &Value,
) -> Option<RouteAmountEvidence> {
    let source_amount_semantics = metadata
        .get("amount_semantics")
        .and_then(Value::as_str)
        .map(ToOwned::to_owned);
    let idle_vault_liquidity_amount_raw = metadata_i64(metadata, "idle_vault_liquidity_amount_raw")
        .or_else(|| metadata_i64(metadata, "vault_liquidity_amount_raw"));

    match source_amount_semantics.as_deref() {
        Some(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY) => Some(RouteAmountEvidence {
            amount_raw,
            route_amount_semantics: ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned(),
            source_amount_semantics,
            source_collateral_amount_raw: metadata_i64(metadata, "source_collateral_amount_raw"),
            redeemable_source_liquidity_amount_raw: Some(amount_raw),
            idle_vault_liquidity_amount_raw,
        }),
        Some(AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED) => {
            let redeemable_amount =
                metadata_i64(metadata, "redeemable_source_liquidity_amount_raw")
                    .or_else(|| metadata_i64(metadata, "redeemable_liquidity_amount_raw"))?;
            if redeemable_amount <= 0 {
                return None;
            }
            Some(RouteAmountEvidence {
                amount_raw: redeemable_amount,
                route_amount_semantics: ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned(),
                source_amount_semantics,
                source_collateral_amount_raw: Some(amount_raw),
                redeemable_source_liquidity_amount_raw: Some(redeemable_amount),
                idle_vault_liquidity_amount_raw,
            })
        }
        _ => None,
    }
}

fn metadata_i64(metadata: &Value, field: &str) -> Option<i64> {
    let value = metadata.get(field)?;
    value
        .as_i64()
        .or_else(|| value.as_u64().and_then(|amount| i64::try_from(amount).ok()))
        .or_else(|| value.as_str().and_then(|amount| amount.parse::<i64>().ok()))
}

fn score_for(position: &CurrentReservePosition, scored: &HashMap<&str, i64>) -> i64 {
    scored
        .get(position.reserve.as_str())
        .copied()
        .or(position.supply_apy_bps)
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::{SnapshotId, VaultId};
    use chrono::Utc;

    fn position(
        reserve: &str,
        amount_raw: i64,
        planning_metadata: Value,
    ) -> CurrentReservePosition {
        CurrentReservePosition {
            vault_id: VaultId(1),
            reserve: reserve.to_owned(),
            market: None,
            liquidity_mint: "USDC".to_owned(),
            amount_raw,
            has_value: amount_raw > 0,
            supply_apy_bps: None,
            borrow_apy_bps: None,
            snapshot_id: SnapshotId(7),
            observed_slot: 10,
            observed_at: Utc::now(),
            planning_metadata,
        }
    }

    fn reserve_scores() -> Vec<ReserveScore> {
        vec![
            ReserveScore {
                reserve: "source".to_owned(),
                supply_apy_bps: 100,
                borrow_apy_bps: None,
            },
            ReserveScore {
                reserve: "target".to_owned(),
                supply_apy_bps: 200,
                borrow_apy_bps: None,
            },
        ]
    }

    #[test]
    fn collateral_semantics_fail_closed_before_planning() {
        let positions = vec![
            position(
                "source",
                404_323_479,
                json!({
                    "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
                    "idle_vault_liquidity_amount_raw": "75_676_540".replace('_', ""),
                }),
            ),
            position(
                "target",
                0,
                json!({
                    "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                }),
            ),
        ];

        let result = draft_same_mint_decision(
            &positions,
            &reserve_scores(),
            PlannerConfig {
                min_edge_bps: 1,
                estimated_cost_lamports: 0,
            },
        );

        assert_eq!(result.unwrap_err(), SkipReason::UnsupportedAmountSemantics);
    }

    #[test]
    fn redeemable_liquidity_semantics_plan_route_amount() {
        let positions = vec![
            position(
                "source",
                480_000_000,
                json!({
                    "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                }),
            ),
            position(
                "target",
                0,
                json!({
                    "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                }),
            ),
        ];

        let planned = draft_same_mint_decision(
            &positions,
            &reserve_scores(),
            PlannerConfig {
                min_edge_bps: 1,
                estimated_cost_lamports: 0,
            },
        )
        .expect("redeemable liquidity source should plan");

        assert_eq!(planned.amount_raw, 480_000_000);
        assert_eq!(
            planned.route_amount_semantics,
            ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY
        );
        assert_eq!(
            planned
                .execution_plan
                .get("redeemable_source_liquidity_amount_raw")
                .and_then(Value::as_i64),
            Some(480_000_000)
        );
    }

    #[test]
    fn incident_collateral_shape_cannot_plan_404_as_usdc() {
        let positions = vec![
            position(
                "source",
                404_323_479,
                json!({
                    "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
                    "idle_vault_liquidity_amount_raw": 75_676_540,
                    "vault_liquidity_ata": "CBeayrtDtS18CduF36jRm1uFwoTiw3i9onoh3oniJUJb",
                }),
            ),
            position(
                "target",
                0,
                json!({
                    "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                }),
            ),
        ];

        let result = draft_same_mint_decision(
            &positions,
            &reserve_scores(),
            PlannerConfig {
                min_edge_bps: 1,
                estimated_cost_lamports: 0,
            },
        );

        assert!(matches!(
            result,
            Err(SkipReason::UnsupportedAmountSemantics)
        ));
    }
}

pub fn state_transition(
    current: DecisionStatus,
    advance: DecisionAdvance,
) -> Result<DecisionTransition, OrchestratorError> {
    if current.is_terminal() {
        let repeat = matches!(
            (current, &advance),
            (DecisionStatus::Confirmed, DecisionAdvance::Confirm { .. })
                | (DecisionStatus::Failed, DecisionAdvance::Fail { .. })
                | (DecisionStatus::Abandoned, DecisionAdvance::Abandon { .. })
        );
        if repeat {
            return Ok(DecisionTransition::idempotent(current));
        }
        return Err(OrchestratorError::TerminalDecision(current));
    }

    match (current, advance) {
        (DecisionStatus::Planned, DecisionAdvance::StartSimulation) => {
            Ok(DecisionTransition::simple(DecisionStatus::Simulating))
        }
        (DecisionStatus::Simulating, DecisionAdvance::SimulationReady) => {
            Ok(DecisionTransition::simple(DecisionStatus::Ready))
        }
        (DecisionStatus::Ready, DecisionAdvance::Submit { signature, slot }) => {
            Ok(DecisionTransition {
                status: DecisionStatus::Submitted,
                idempotent: false,
                signature: Some(signature.clone()),
                submitted_slot: slot,
                confirmed_slot: None,
                preflight_chain_slot: None,
                post_snapshot_id: None,
                abandon_reason: None,
                reason: Some("submitted".to_owned()),
                payload: json!({ "signature": signature, "slot": slot }),
            })
        }
        (DecisionStatus::Submitted, DecisionAdvance::StartConfirmation) => {
            Ok(DecisionTransition::simple(DecisionStatus::Confirming))
        }
        (
            DecisionStatus::Confirming | DecisionStatus::Submitted,
            DecisionAdvance::Confirm {
                slot,
                post_snapshot_id,
            },
        ) => Ok(DecisionTransition {
            status: DecisionStatus::Confirmed,
            idempotent: false,
            signature: None,
            submitted_slot: None,
            confirmed_slot: slot,
            preflight_chain_slot: None,
            post_snapshot_id,
            abandon_reason: None,
            reason: Some("confirmed".to_owned()),
            payload: json!({ "slot": slot, "post_snapshot_id": post_snapshot_id.map(|id| id.0) }),
        }),
        (_, DecisionAdvance::Fail { reason }) => Ok(DecisionTransition {
            status: DecisionStatus::Failed,
            idempotent: false,
            signature: None,
            submitted_slot: None,
            confirmed_slot: None,
            preflight_chain_slot: None,
            post_snapshot_id: None,
            abandon_reason: Some(reason.clone()),
            reason: Some(reason.clone()),
            payload: json!({ "reason": reason }),
        }),
        (_, DecisionAdvance::Abandon { reason }) => Ok(DecisionTransition {
            status: DecisionStatus::Abandoned,
            idempotent: false,
            signature: None,
            submitted_slot: None,
            confirmed_slot: None,
            preflight_chain_slot: None,
            post_snapshot_id: None,
            abandon_reason: Some(reason.clone()),
            reason: Some(reason.clone()),
            payload: json!({ "reason": reason }),
        }),
        (status, advance) => Err(OrchestratorError::InvalidDecisionTransition {
            from: status,
            advance,
        }),
    }
}
