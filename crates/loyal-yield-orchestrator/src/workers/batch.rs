use std::collections::HashSet;

use crate::pipeline::{BatchPlan, ReadyAttempt};

#[derive(Debug, Clone, Copy)]
pub struct BatchWorker {
    max_decisions_per_batch: usize,
}

impl BatchWorker {
    pub fn new(max_decisions_per_batch: usize) -> Self {
        Self {
            max_decisions_per_batch: max_decisions_per_batch.max(1),
        }
    }

    pub fn pack(&self, attempts: &[ReadyAttempt]) -> Vec<BatchPlan> {
        let mut plans = Vec::new();
        let mut current = Vec::new();
        let mut current_vaults = HashSet::new();

        for attempt in attempts {
            let vault_id = attempt.decision.vault_id;
            let would_exceed = current.len() >= self.max_decisions_per_batch;
            let duplicate_vault = current_vaults.contains(&vault_id);
            if !current.is_empty() && (would_exceed || duplicate_vault) {
                plans.push(BatchPlan { attempts: current });
                current = Vec::new();
                current_vaults = HashSet::new();
            }

            current_vaults.insert(vault_id);
            current.push(attempt.clone());
        }

        if !current.is_empty() {
            plans.push(BatchPlan { attempts: current });
        }

        plans
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pipeline::{DecisionWorkItem, ReadyAttempt};
    use crate::{DecisionId, VaultId};

    fn attempt(id: i64, vault: i64) -> ReadyAttempt {
        ReadyAttempt {
            attempt_id: id,
            decision: DecisionWorkItem {
                decision_id: DecisionId(id),
                vault_id: VaultId(vault),
                cluster: "mainnet".to_owned(),
                liquidity_mint: "USDC".to_owned(),
                source_reserve: "source".to_owned(),
                target_reserve: "target".to_owned(),
                amount_raw: 1,
            },
            estimated_compute_units: None,
        }
    }

    #[test]
    fn batch_worker_respects_max_size() {
        let plans = BatchWorker::new(2).pack(&[attempt(1, 1), attempt(2, 2), attempt(3, 3)]);

        assert_eq!(plans.len(), 2);
        assert_eq!(plans[0].attempts.len(), 2);
        assert_eq!(plans[1].attempts.len(), 1);
    }

    #[test]
    fn batch_worker_splits_duplicate_vaults() {
        let plans = BatchWorker::new(3).pack(&[attempt(1, 1), attempt(2, 1), attempt(3, 2)]);

        assert_eq!(plans.len(), 2);
        assert_eq!(plans[0].attempts.len(), 1);
        assert_eq!(plans[1].attempts.len(), 2);
    }
}
