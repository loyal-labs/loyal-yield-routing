\set candidate random(1, :opportunity_rows)
WITH candidate AS (
    SELECT id
    FROM loyal_yield.rebalance_opportunities
    WHERE cluster = 'localnet'
      AND opportunity_state IN ('ready', 'revalidate')
      AND available_at <= clock_timestamp()
      AND expires_at > clock_timestamp()
      AND id >= :candidate
    ORDER BY scheduler_priority_anchor DESC, economic_priority DESC, created_at, id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
), wrapped_candidate AS (
    SELECT id FROM candidate
    UNION ALL
    SELECT id
    FROM loyal_yield.rebalance_opportunities
    WHERE cluster = 'localnet'
      AND opportunity_state IN ('ready', 'revalidate')
      AND available_at <= clock_timestamp()
      AND expires_at > clock_timestamp()
    ORDER BY id
    LIMIT 1
)
UPDATE loyal_yield.rebalance_opportunities opportunity
SET attempt_count = opportunity.attempt_count + 1,
    updated_at = clock_timestamp()
FROM (SELECT id FROM wrapped_candidate LIMIT 1) selected
WHERE opportunity.id = selected.id;
