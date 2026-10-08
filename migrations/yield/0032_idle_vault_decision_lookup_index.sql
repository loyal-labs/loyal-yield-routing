CREATE INDEX CONCURRENTLY IF NOT EXISTS rebalance_decisions_idle_signature_id_idx
    ON loyal_yield.rebalance_decisions (signature, id DESC)
    WHERE execution_plan->>'kind' = 'idle_vault_deposit';
