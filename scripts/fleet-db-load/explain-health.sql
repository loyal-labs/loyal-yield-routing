\set ON_ERROR_STOP on

EXPLAIN (ANALYZE, BUFFERS, WAL, FORMAT JSON)
SELECT *
FROM loyal_yield.fleet_orchestration_status
WHERE cluster = :'cluster'
ORDER BY opportunity_state NULLS LAST;
