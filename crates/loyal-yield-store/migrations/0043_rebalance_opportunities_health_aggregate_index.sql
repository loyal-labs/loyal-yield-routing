-- The health view reports lifetime counts and amounts for every durable state.
-- Keep those narrow aggregate inputs together so the refresh does not read the
-- JSON-heavy opportunity heap on every five-second cycle.
CREATE INDEX CONCURRENTLY IF NOT EXISTS rebalance_opportunities_health_aggregate_idx
    ON loyal_yield.rebalance_opportunities (cluster, opportunity_state)
    INCLUDE (
        principal_usd_micros,
        annual_yield_gain_usd_micros,
        created_at,
        state_entered_at,
        lease_expires_at
    );
