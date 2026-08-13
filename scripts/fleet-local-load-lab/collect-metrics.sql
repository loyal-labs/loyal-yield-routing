\set ON_ERROR_STOP on

SELECT json_build_object(
    'database', current_database(),
    'capturedAt', clock_timestamp(),
    'databaseBytes', pg_database_size(current_database()),
    'databaseStats', (
        SELECT row_to_json(stats)
        FROM (
            SELECT numbackends, xact_commit, xact_rollback, blks_read,
                   blks_hit, tup_returned, tup_fetched, tup_inserted,
                   tup_updated, tup_deleted, conflicts, deadlocks,
                   temp_files, temp_bytes
            FROM pg_stat_database
            WHERE datname = current_database()
        ) stats
    ),
    'locks', json_build_object(
        'total', (SELECT count(*) FROM pg_locks),
        'waiting', (SELECT count(*) FROM pg_locks WHERE NOT granted)
    ),
    'connections', json_build_object(
        'total', (SELECT count(*) FROM pg_stat_activity),
        'active', (
            SELECT count(*) FROM pg_stat_activity
            WHERE state = 'active' AND pid <> pg_backend_pid()
        ),
        'waiting', (
            SELECT count(*) FROM pg_stat_activity
            WHERE wait_event IS NOT NULL AND pid <> pg_backend_pid()
        )
    ),
    'rows', json_build_object(
        'opportunities', (SELECT count(*) FROM loyal_yield.rebalance_opportunities),
        'decisions', (SELECT count(*) FROM loyal_yield.rebalance_decisions),
        'submissions', (SELECT count(*) FROM loyal_yield.signed_route_submissions),
        'outbox', (SELECT count(*) FROM loyal_yield.orchestration_outbox),
        'syntheticOutbox', (
            SELECT count(*)
            FROM loyal_yield.orchestration_outbox
            WHERE event_kind = 'local_user_load'
        )
    ),
    'opportunityStates', (
        SELECT COALESCE(json_object_agg(opportunity_state, count), '{}'::json)
        FROM (
            SELECT opportunity_state, count(*)
            FROM loyal_yield.rebalance_opportunities
            GROUP BY opportunity_state
        ) states
    ),
    'submissionStates', (
        SELECT COALESCE(json_object_agg(submission_state, count), '{}'::json)
        FROM (
            SELECT submission_state, count(*)
            FROM loyal_yield.signed_route_submissions
            GROUP BY submission_state
        ) states
    ),
    'healthRows', (
        SELECT COALESCE(jsonb_array_length(payload), 0)
        FROM loyal_yield.fleet_orchestration_health_snapshots
        WHERE cluster = 'localnet'
    )
);
