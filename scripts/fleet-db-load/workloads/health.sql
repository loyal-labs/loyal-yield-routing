SELECT payload
FROM loyal_yield.fleet_orchestration_health_snapshots
WHERE cluster = 'localnet'
  AND refreshed_at >= now() - interval '30 seconds';
