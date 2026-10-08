-- Execute this single statement in autocommit, outside a transaction.
-- Both migration entrypoints use the shared bounded-session helper.
CREATE INDEX CONCURRENTLY IF NOT EXISTS vault_position_snapshots_complete_history_idx
    ON loyal_yield.vault_position_snapshots
        (vault_id, observed_at DESC, observed_slot DESC, id DESC)
    WHERE context->>'publication_scope' = 'complete_product_vault';
