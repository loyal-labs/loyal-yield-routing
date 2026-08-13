\set ON_ERROR_STOP on

SELECT json_build_object(
    'targetOpportunityRows', :target_rows::bigint,
    'actualRows', json_build_object(
        'rebalanceOpportunities', (
            SELECT count(*) FROM loyal_yield.rebalance_opportunities
        ),
        'rebalanceDecisions', (
            SELECT count(*) FROM loyal_yield.rebalance_decisions
        ),
        'signedRouteSubmissions', (
            SELECT count(*) FROM loyal_yield.signed_route_submissions
        ),
        'orchestrationOutbox', (
            SELECT count(*) FROM loyal_yield.orchestration_outbox
        ),
        'managedVaults', (
            SELECT count(*) FROM loyal_yield.managed_vaults
        )
    ),
    'relationBytes', json_build_object(
        'rebalanceOpportunities', pg_total_relation_size(
            'loyal_yield.rebalance_opportunities'
        ),
        'rebalanceDecisions', pg_total_relation_size(
            'loyal_yield.rebalance_decisions'
        ),
        'signedRouteSubmissions', pg_total_relation_size(
            'loyal_yield.signed_route_submissions'
        ),
        'orchestrationOutbox', pg_total_relation_size(
            'loyal_yield.orchestration_outbox'
        )
    ),
    'databaseBytes', pg_database_size(current_database()),
    'statusRows', (
        SELECT count(*)
        FROM loyal_yield.fleet_orchestration_status
        WHERE cluster = :'cluster'
    )
);
