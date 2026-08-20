-- Admin monitoring reads exact lifetime opportunity counts by active vault and
-- route mode. Keep the count inputs in a narrow partial index so the query does
-- not repeatedly read the JSON-heavy opportunity heap.
CREATE INDEX CONCURRENTLY IF NOT EXISTS rebalance_opportunities_same_mint_admin_frequency_idx
    ON loyal_yield.rebalance_opportunities (
        cluster,
        vault_id,
        created_at
    )
    WHERE source_reserve IS NOT NULL
      AND target_reserve IS NOT NULL
      AND (execution_plan ->> 'kind') = 'same_mint';

CREATE INDEX CONCURRENTLY IF NOT EXISTS rebalance_opportunities_cross_mint_admin_frequency_idx
    ON loyal_yield.rebalance_opportunities (
        cluster,
        vault_id,
        created_at
    )
    WHERE source_reserve IS NOT NULL
      AND target_reserve IS NOT NULL
      AND (execution_plan ->> 'kind') = 'cross_mint_jupiter';

-- The 72-hour activity query has two independent clocks. Narrow partial
-- indexes let each bounded branch read only its own recent rows before they
-- are combined, instead of evaluating an OR over the full opportunity heap.
CREATE INDEX CONCURRENTLY IF NOT EXISTS rebalance_opportunities_failed_kind_state_entered_idx
    ON loyal_yield.rebalance_opportunities (
        state_entered_at,
        ((execution_plan ->> 'kind'))
    )
    WHERE opportunity_state = 'failed'
      AND (execution_plan ->> 'kind') IN ('same_mint', 'cross_mint_jupiter');

CREATE INDEX CONCURRENTLY IF NOT EXISTS rebalance_opportunities_kind_created_at_idx
    ON loyal_yield.rebalance_opportunities (
        created_at,
        ((execution_plan ->> 'kind'))
    )
    INCLUDE (attempt_count)
    WHERE (execution_plan ->> 'kind') IN ('same_mint', 'cross_mint_jupiter');

-- Pre-pull monitoring reads only unresolved released/failed claims in a
-- bounded time window. Keep that operational subset ordered by its clock.
CREATE INDEX CONCURRENTLY IF NOT EXISTS balance_sweep_lot_claims_pre_pull_updated_idx
    ON loyal_yield.balance_sweep_lot_claims (updated_at)
    INCLUDE (claim_token, status, execution_id)
    WHERE execution_id IS NULL
      AND status IN ('released', 'failed');

-- Admin Earn freshness reports independent exact maxima. These indexes turn
-- each MAX into a one-row backward index-only lookup.
CREATE INDEX CONCURRENTLY IF NOT EXISTS balance_sweep_wallet_balance_events_observed_at_idx
    ON loyal_yield.balance_sweep_wallet_balance_events (observed_at);

CREATE INDEX CONCURRENTLY IF NOT EXISTS balance_sweep_wallet_balance_events_projected_at_idx
    ON loyal_yield.balance_sweep_wallet_balance_events (projected_at);

-- Earn holdings joins each active position to the current reserve row by its
-- stable `(vault_id, liquidity_mint)` identity. Without this lookup the admin
-- request scans the complete current-position table on every navigation.
CREATE INDEX CONCURRENTLY IF NOT EXISTS vault_reserve_positions_admin_holdings_idx
    ON loyal_yield.vault_reserve_positions_current (vault_id, liquidity_mint);
