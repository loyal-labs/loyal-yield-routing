\set ON_ERROR_STOP on

SELECT jsonb_build_object(
    'cluster', 'localnet',
    'opportunities', jsonb_build_object(
        'total', count(*),
        'completed', count(*) FILTER (WHERE opportunity_state = 'completed'),
        'failed', count(*) FILTER (WHERE opportunity_state = 'failed'),
        'active', count(*) FILTER (
            WHERE opportunity_state NOT IN ('completed', 'failed', 'stale', 'superseded', 'cancelled')
        ),
        'attempts', COALESCE(sum(attempt_count), 0),
        'states', COALESCE(
            (SELECT jsonb_object_agg(opportunity_state, state_count)
             FROM (
                 SELECT opportunity_state, count(*) AS state_count
                 FROM loyal_yield.rebalance_opportunities
                 WHERE cluster = 'localnet'
                 GROUP BY opportunity_state
             ) grouped),
            '{}'::jsonb
        )
    )
) || jsonb_build_object(
    'decisions', jsonb_build_object(
        'total', (SELECT count(*) FROM loyal_yield.rebalance_decisions),
        'withSignature', (
            SELECT count(*) FROM loyal_yield.rebalance_decisions
            WHERE NULLIF(btrim(signature), '') IS NOT NULL
        ),
        'distinctSignatures', (
            SELECT count(DISTINCT signature) FROM loyal_yield.rebalance_decisions
            WHERE NULLIF(btrim(signature), '') IS NOT NULL
        ),
        'states', COALESCE(
            (SELECT jsonb_object_agg(status::text, state_count)
             FROM (
                 SELECT status, count(*) AS state_count
                 FROM loyal_yield.rebalance_decisions
                 GROUP BY status
             ) grouped),
            '{}'::jsonb
        )
    ),
    'submissions', jsonb_build_object(
        'total', (SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE cluster = 'localnet'),
        'reconciled', (
            SELECT count(*) FROM loyal_yield.signed_route_submissions
            WHERE cluster = 'localnet' AND submission_state = 'reconciled'
        ),
        'active', (
            SELECT count(*) FROM loyal_yield.signed_route_submissions
            WHERE cluster = 'localnet'
              AND submission_state NOT IN ('reconciled', 'expired', 'failed')
        ),
        'distinctSemanticKeys', (
            SELECT count(DISTINCT semantic_key) FROM loyal_yield.signed_route_submissions
            WHERE cluster = 'localnet'
        ),
        'distinctSignatures', (
            SELECT count(DISTINCT transaction_signature)
            FROM loyal_yield.signed_route_submissions
            WHERE cluster = 'localnet'
        ),
        'signatures', COALESCE(
            (SELECT jsonb_agg(transaction_signature ORDER BY transaction_signature)
             FROM loyal_yield.signed_route_submissions
             WHERE cluster = 'localnet'),
            '[]'::jsonb
        ),
        'states', COALESCE(
            (SELECT jsonb_object_agg(submission_state, state_count)
             FROM (
                 SELECT submission_state, count(*) AS state_count
                 FROM loyal_yield.signed_route_submissions
                 WHERE cluster = 'localnet'
                 GROUP BY submission_state
             ) grouped),
            '{}'::jsonb
        )
    ),
    'currentPositions', COALESCE(
        (SELECT jsonb_agg(
            jsonb_build_object(
                'vaultId', vault_id,
                'reserve', reserve,
                'market', market,
                'liquidityMint', liquidity_mint,
                'amountRaw', amount_raw::text,
                'hasValue', has_value
            ) ORDER BY vault_id, reserve
        ) FROM loyal_yield.vault_reserve_positions_current),
        '[]'::jsonb
    ),
    'lookupTables', jsonb_build_object(
        'operationsTotal', (SELECT count(*) FROM loyal_yield.lookup_table_operations),
        'operationsIncomplete', (
            SELECT count(*) FROM loyal_yield.lookup_table_operations
            WHERE operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
        ),
        'operationsPermanentFailure', (
            SELECT count(*) FROM loyal_yield.lookup_table_operations
            WHERE operation_state = 'permanent_failure'
        ),
        'activeUsageLeases', (
            SELECT count(*) FROM loyal_yield.lookup_table_usage_leases
            WHERE released_at IS NULL AND expires_at > now()
        )
    )
)
FROM loyal_yield.rebalance_opportunities
WHERE cluster = 'localnet';
