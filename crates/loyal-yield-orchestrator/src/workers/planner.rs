use crate::domain::{draft_same_mint_decision, PlannedDecision};
use crate::{CurrentReservePosition, PlannerConfig, ReserveScore, ReserveTarget, SkipReason};

#[derive(Debug, Default, Clone, Copy)]
pub struct PlannerWorker;

impl PlannerWorker {
    pub fn reserve_scores_for_target(target: &ReserveTarget) -> Vec<ReserveScore> {
        vec![ReserveScore {
            reserve: target.target_reserve.clone(),
            supply_apy_bps: target.target_supply_apy_bps,
            borrow_apy_bps: None,
        }]
    }

    pub fn draft_for_target(
        positions: &[CurrentReservePosition],
        target: &ReserveTarget,
        config: PlannerConfig,
    ) -> Result<PlannedDecision, SkipReason> {
        let reserve_scores = Self::reserve_scores_for_target(target);
        draft_same_mint_decision(positions, &reserve_scores, config)
    }

    pub fn target_is_stale(target: &ReserveTarget) -> bool {
        target.stale
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{SnapshotId, VaultId};
    use chrono::Utc;
    use serde_json::json;

    fn position(reserve: &str, amount_raw: i64, apy: i64) -> CurrentReservePosition {
        CurrentReservePosition {
            vault_id: VaultId(1),
            reserve: reserve.to_owned(),
            market: Some("market".to_owned()),
            liquidity_mint: "USDC".to_owned(),
            amount_raw,
            has_value: amount_raw > 0,
            supply_apy_bps: Some(apy),
            borrow_apy_bps: None,
            snapshot_id: SnapshotId(1),
            observed_slot: 1,
            observed_at: Utc::now(),
            planning_metadata: json!({}),
        }
    }

    fn target() -> ReserveTarget {
        ReserveTarget {
            id: 1,
            cluster: "mainnet".to_owned(),
            strategy: "same_mint_max_apy_v1".to_owned(),
            liquidity_mint: "USDC".to_owned(),
            target_reserve: "reserve-b".to_owned(),
            target_market: Some("market".to_owned()),
            target_supply_apy_bps: 250,
            target_epoch: "epoch".to_owned(),
            stale: false,
        }
    }

    #[test]
    fn planner_worker_drafts_move_to_target() {
        let planned = PlannerWorker::draft_for_target(
            &[
                position("reserve-a", 1_000, 100),
                position("reserve-b", 0, 250),
            ],
            &target(),
            PlannerConfig {
                min_edge_bps: 10,
                estimated_cost_lamports: 0,
            },
        )
        .expect("draft same-mint move");

        assert_eq!(planned.source_reserve, "reserve-a");
        assert_eq!(planned.target_reserve, "reserve-b");
        assert_eq!(planned.estimated_edge_bps, 150);
    }

    #[test]
    fn planner_worker_respects_stale_target_flag() {
        let mut target = target();
        target.stale = true;

        assert!(PlannerWorker::target_is_stale(&target));
    }
}
