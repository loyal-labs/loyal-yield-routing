WITH selected AS (
    SELECT id
    FROM loyal_yield.rebalance_opportunities
    WHERE cluster = 'localnet'
      AND opportunity_state = 'ready'
    ORDER BY scheduler_priority_anchor DESC, economic_priority DESC, created_at, id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
UPDATE loyal_yield.rebalance_opportunities opportunity
SET attempt_count = opportunity.attempt_count + 1,
    updated_at = clock_timestamp()
FROM selected
WHERE opportunity.id = selected.id;
