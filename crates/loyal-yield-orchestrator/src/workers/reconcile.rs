use chrono::{DateTime, Utc};
use serde_json::{json, Value};

use crate::{ReconciledReservePosition, ReconciledVaultState, VaultReconcileJob};

#[derive(Debug, Default, Clone, Copy)]
pub struct ReconcileWorker;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ObservedReservePosition {
    pub reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
    pub amount_raw: u64,
    pub supply_apy_bps: Option<i64>,
    pub borrow_apy_bps: Option<i64>,
    pub account_metadata: Value,
}

impl ReconcileWorker {
    pub fn build_state(
        job: &VaultReconcileJob,
        observed_slot: i64,
        chain_slot: Option<i64>,
        observed_at: Option<DateTime<Utc>>,
        positions: Vec<ObservedReservePosition>,
    ) -> ReconciledVaultState {
        let reconciled_positions = positions
            .into_iter()
            .map(|position| ReconciledReservePosition {
                reserve: position.reserve,
                market: position.market,
                liquidity_mint: position.liquidity_mint,
                amount_raw: position.amount_raw,
                supply_apy_bps: position.supply_apy_bps,
                borrow_apy_bps: position.borrow_apy_bps,
                planning_metadata: position.account_metadata,
            })
            .collect();

        ReconciledVaultState {
            observed_slot,
            observed_at,
            chain_slot,
            lock_attempt_id: Some(job.id),
            context: json!({
                "target_reserve": job.target_reserve,
                "target_epoch": job.target_epoch,
                "liquidity_mint": job.liquidity_mint
            }),
            positions: reconciled_positions,
        }
    }

    pub fn has_target_candidate(
        job: &VaultReconcileJob,
        positions: &[ObservedReservePosition],
    ) -> bool {
        positions.iter().any(|position| {
            position.reserve == job.target_reserve && position.liquidity_mint == job.liquidity_mint
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{VaultId, VaultReconcileJob};

    fn job() -> VaultReconcileJob {
        VaultReconcileJob {
            id: 9,
            vault_id: VaultId(4),
            target_id: Some(7),
            cluster: "mainnet".to_owned(),
            liquidity_mint: "USDC".to_owned(),
            target_reserve: "reserve-b".to_owned(),
            target_epoch: "epoch".to_owned(),
            attempt_count: 1,
        }
    }

    #[test]
    fn reconcile_worker_preserves_job_context_in_snapshot() {
        let state = ReconcileWorker::build_state(
            &job(),
            55,
            Some(56),
            None,
            vec![ObservedReservePosition {
                reserve: "reserve-b".to_owned(),
                market: Some("market".to_owned()),
                liquidity_mint: "USDC".to_owned(),
                amount_raw: 0,
                supply_apy_bps: Some(120),
                borrow_apy_bps: None,
                account_metadata: json!({"collateral": "vault-collateral"}),
            }],
        );

        assert_eq!(state.observed_slot, 55);
        assert_eq!(state.chain_slot, Some(56));
        assert_eq!(state.lock_attempt_id, Some(9));
        assert_eq!(state.context["target_reserve"], "reserve-b");
        assert_eq!(
            state.positions[0].planning_metadata["collateral"],
            "vault-collateral"
        );
    }

    #[test]
    fn reconcile_worker_detects_target_candidate() {
        let positions = vec![ObservedReservePosition {
            reserve: "reserve-b".to_owned(),
            market: None,
            liquidity_mint: "USDC".to_owned(),
            amount_raw: 0,
            supply_apy_bps: None,
            borrow_apy_bps: None,
            account_metadata: json!({}),
        }];

        assert!(ReconcileWorker::has_target_candidate(&job(), &positions));
    }
}
