UPDATE loyal_yield.fleet_planning_clusters
SET last_seen_at = clock_timestamp()
WHERE cluster = 'localnet';

UPDATE loyal_yield.fleet_planning_state
SET updated_at = clock_timestamp()
WHERE cluster = 'localnet';
