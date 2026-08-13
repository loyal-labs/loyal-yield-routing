SELECT *
FROM loyal_yield.fleet_orchestration_status
WHERE cluster = 'localnet'
ORDER BY opportunity_state NULLS LAST;
