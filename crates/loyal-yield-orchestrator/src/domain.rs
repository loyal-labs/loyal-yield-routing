use std::collections::HashMap;

use serde_json::{json, Value};

use crate::types::{
    CurrentReservePosition, DecisionAdvance, DecisionStatus, DecisionTransition, PlannerConfig,
    ReserveScore, SkipReason,
};
use crate::OrchestratorError;

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
    pub execution_plan: Value,
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

    let source = valued_positions
        .iter()
        .max_by_key(|position| position.amount_raw)
        .copied()
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
        amount_raw: source.amount_raw,
        source_apy_bps,
        target_apy_bps,
        estimated_edge_bps: target_apy_bps - source_apy_bps,
        execution_plan: json!({
            "kind": "same_mint",
            "source_reserve": source.reserve.clone(),
            "target_reserve": target.reserve.clone(),
            "liquidity_mint": source.liquidity_mint.clone(),
        }),
    })
}

fn score_for(position: &CurrentReservePosition, scored: &HashMap<&str, i64>) -> i64 {
    scored
        .get(position.reserve.as_str())
        .copied()
        .or(position.supply_apy_bps)
        .unwrap_or_default()
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
