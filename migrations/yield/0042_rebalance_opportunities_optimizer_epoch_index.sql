CREATE INDEX CONCURRENTLY IF NOT EXISTS rebalance_opportunities_optimizer_epoch_idx
    ON loyal_yield.rebalance_opportunities (optimizer_epoch_id)
    INCLUDE (
        id,
        cluster,
        created_at,
        principal_usd_micros,
        annual_yield_gain_usd_micros
    );
