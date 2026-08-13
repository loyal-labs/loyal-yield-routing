\set ON_ERROR_STOP on

SET synchronous_commit = off;
SET session_replication_role = replica;

INSERT INTO loyal_yield.rebalance_decisions (
    id, vault_id, source_snapshot_id, status, source_reserve,
    target_reserve, liquidity_mint, source_liquidity_mint,
    target_liquidity_mint, amount_raw, source_apy_bps, target_apy_bps,
    estimated_edge_bps, estimated_cost_lamports, decision_reason,
    execution_plan, idempotency_key, signature, submitted_slot,
    confirmed_slot, created_at, updated_at
)
SELECT value,
       ((value * 4 - 1) % 1000) + 1,
       ((value * 4 - 1) % 1000) + 1,
       'confirmed'::loyal_yield.decision_status,
       'local-source-reserve', 'local-target-reserve', 'local-mint',
       'local-mint', 'local-mint', 1000000 + value, 400, 650, 250, 5000,
       'target_supply_apy_exceeds_source'::loyal_yield.decision_reason,
       '{"kind":"same_mint","source":"local-reproduction"}'::jsonb,
       'local-decision-' || value,
       'local-signature-' || value,
       1000000 + value,
       1000001 + value,
       clock_timestamp() - ((value % 3600) * interval '1 second'),
       clock_timestamp()
FROM generate_series(
    COALESCE((SELECT max(id) + 1 FROM loyal_yield.rebalance_decisions), 1),
    floor(:target_rows::numeric / 4)::bigint
) value;

INSERT INTO loyal_yield.rebalance_opportunities (
    id, cluster, idempotency_key, vault_id, source_snapshot_id,
    optimizer_epoch_id, route_fingerprint, requirements_fingerprint,
    source_reserve, target_reserve, liquidity_mint, amount_raw,
    principal_usd_micros, source_apy_bps, target_apy_bps,
    estimated_edge_bps, estimated_cost_lamports,
    annual_yield_gain_usd_micros, expected_net_gain_usd_micros,
    economic_priority, scheduler_priority_anchor, priority_version,
    opportunity_state, execution_plan, available_at, expires_at,
    lease_kind, lease_owner, lease_expires_at, fencing_token,
    attempt_count, decision_id, terminal_reason, state_entered_at,
    ready_at, waiting_alt_at, created_at, updated_at, rediscovery_key,
    attempt_generation
)
SELECT value,
       :'cluster',
       'local-opportunity-' || value,
       ((value - 1) % 1000) + 1,
       ((value - 1) % 1000) + 1,
       1,
       'route-' || (value % 64),
       'requirements-' || (value % 64),
       'local-source-reserve',
       'local-target-reserve',
       'local-mint',
       1000000 + value,
       1000000 + (value % 100000),
       400,
       650,
       250,
       5000,
       250000 + (value % 10000),
       200000 + (value % 10000),
       1000000 + (value % 100000),
       1000000 + (value % 100000),
       'local-v1',
       CASE
           WHEN value % 4 = 0 AND (value / 4) % 20 IN (1, 2, 3, 4)
               THEN 'decision_created'
           WHEN value % 4 = 0 THEN 'completed'
           WHEN value % 20 = 1 THEN 'ready'
           WHEN value % 20 = 2 THEN 'waiting_alt'
           WHEN value % 20 = 3 THEN 'revalidate'
           WHEN value % 20 = 5 THEN 'failed'
           WHEN value % 20 = 6 THEN 'cancelled'
           WHEN value % 20 = 7 THEN 'superseded'
           ELSE 'stale'
       END,
       '{"kind":"same_mint","source":"local-reproduction"}'::jsonb,
       clock_timestamp() - ((value % 60) * interval '1 second'),
       clock_timestamp() + interval '7 days',
       NULL,
       NULL,
       NULL,
       0,
       value % 3,
       CASE WHEN value % 4 = 0 THEN value / 4 ELSE NULL END,
       CASE
           WHEN value % 4 = 0 AND (value / 4) % 20 NOT IN (1, 2, 3, 4)
               THEN 'mock-chain-reconciled'
           WHEN value % 20 >= 4 THEN 'historical'
           ELSE NULL
       END,
       clock_timestamp() - ((value % 3600) * interval '1 second'),
       CASE WHEN value % 20 = 1 THEN clock_timestamp() ELSE NULL END,
       CASE WHEN value % 20 = 2 THEN clock_timestamp() ELSE NULL END,
       clock_timestamp() - ((value % 3600) * interval '1 second'),
       clock_timestamp(),
       'local-opportunity-' || value,
       1
FROM generate_series(
    COALESCE((SELECT max(id) + 1 FROM loyal_yield.rebalance_opportunities), 1),
    :target_rows
) value;

INSERT INTO loyal_yield.signed_route_submissions (
    id, cluster, semantic_key, opportunity_id, decision_id,
    signed_transaction, signed_transaction_hash, message_hash,
    transaction_signature, recent_blockhash, last_valid_block_height,
    source_snapshot_id, optimizer_epoch_id, alt_requirements_fingerprint,
    alt_selection_fingerprint, alt_mutation_epochs, fee_payer,
    compiled_fee_lamports, writable_account_keys, conflict_account_keys,
    executor_owner, executor_fencing_token, submission_state,
    submission_state_entered_at, submitted_slot, submitted_at,
    confirmed_slot, confirmed_at, reconciled_slot, reconciled_at,
    created_at, updated_at, fee_payer_kind
)
SELECT value,
       :'cluster',
       'local-submission-' || value,
       value * 4,
       value,
       decode('00', 'hex'),
       'local-tx-hash-' || value,
       'local-message-hash-' || value,
       'local-transaction-signature-' || value,
       'local-blockhash-' || value,
       2000000 + value,
       (((value * 4) - 1) % 1000) + 1,
       1,
       'requirements-' || ((value * 4) % 64),
       'selection-' || (value % 64),
       '{"tables":[],"source":"local-reproduction"}'::jsonb,
       'local-delegated-signer',
       5000,
       ARRAY['local-delegated-signer', 'local-vault-' || (((value * 4) - 1) % 1000 + 1)],
       ARRAY['vault:' || (((value * 4) - 1) % 1000 + 1), 'payer:local-delegated-signer'],
       'local-executor',
       value,
       CASE
           WHEN value % 20 = 1 THEN 'submitted'
           WHEN value % 20 = 2 THEN 'confirmed'
           WHEN value % 20 = 3 THEN 'reconciliation_pending'
           WHEN value % 20 = 4 THEN 'expiry_check_pending'
           ELSE 'reconciled'
       END,
       clock_timestamp() - ((value % 3600) * interval '1 second'),
       1000000 + value,
       clock_timestamp() - ((value % 1800) * interval '1 second'),
       1000001 + value,
       clock_timestamp() - ((value % 900) * interval '1 second'),
       CASE WHEN value % 20 NOT IN (1, 2, 3, 4) THEN 1000002 + value ELSE NULL END,
       CASE
           WHEN value % 20 NOT IN (1, 2, 3, 4)
           THEN clock_timestamp() - ((value % 600) * interval '1 second')
           ELSE NULL
       END,
       clock_timestamp() - ((value % 3600) * interval '1 second'),
       clock_timestamp(),
       'policy'
FROM generate_series(
    COALESCE((SELECT max(id) + 1 FROM loyal_yield.signed_route_submissions), 1),
    floor(:target_rows::numeric / 4)::bigint
) value;

INSERT INTO loyal_yield.orchestration_outbox (
    id, cluster, event_kind, aggregate_kind, aggregate_id, dedupe_key,
    payload, available_at, fencing_token, attempt_count, processed_at,
    created_at, updated_at
)
SELECT value,
       :'cluster',
       'opportunity_state_changed',
       'rebalance_opportunity',
       value * 2,
       'local-outbox-' || value,
       jsonb_build_object('source', 'local-reproduction', 'sequence', value),
       clock_timestamp() - ((value % 60) * interval '1 second'),
       0,
       value % 3,
       CASE WHEN value % 10 = 0 THEN NULL ELSE clock_timestamp() END,
       clock_timestamp() - ((value % 3600) * interval '1 second'),
       clock_timestamp()
FROM generate_series(
    COALESCE((SELECT max(id) + 1 FROM loyal_yield.orchestration_outbox), 1),
    floor(:target_rows::numeric / 2)::bigint
) value;

UPDATE loyal_yield.fleet_planning_state
SET opportunity_count = :target_rows,
    selected_count = floor(:target_rows::numeric * 0.15)::bigint,
    deferred_count = floor(:target_rows::numeric * 0.05)::bigint,
    generation = generation + 1,
    updated_at = clock_timestamp()
WHERE cluster = :'cluster';

SELECT setval(
    'loyal_yield.rebalance_decisions_id_seq',
    GREATEST((SELECT COALESCE(max(id), 1) FROM loyal_yield.rebalance_decisions), 1),
    true
);
SELECT setval(
    'loyal_yield.rebalance_opportunities_id_seq',
    GREATEST((SELECT COALESCE(max(id), 1) FROM loyal_yield.rebalance_opportunities), 1),
    true
);
SELECT setval(
    'loyal_yield.signed_route_submissions_id_seq',
    GREATEST((SELECT COALESCE(max(id), 1) FROM loyal_yield.signed_route_submissions), 1),
    true
);
SELECT setval(
    'loyal_yield.orchestration_outbox_id_seq',
    GREATEST((SELECT COALESCE(max(id), 1) FROM loyal_yield.orchestration_outbox), 1),
    true
);

SET session_replication_role = origin;
SET synchronous_commit = on;

SELECT 1 / CASE WHEN
    (SELECT count(*) FROM loyal_yield.rebalance_opportunities) = :target_rows
    AND (SELECT count(*) FROM loyal_yield.signed_route_submissions)
        = floor(:target_rows::numeric / 4)::bigint
    AND (SELECT count(*) FROM loyal_yield.orchestration_outbox)
        = floor(:target_rows::numeric / 2)::bigint
    AND NOT EXISTS (
        SELECT 1
        FROM loyal_yield.signed_route_submissions submission
        JOIN loyal_yield.rebalance_opportunities opportunity
          ON opportunity.id = submission.opportunity_id
        WHERE submission.submission_state IN (
            'signed', 'submitted', 'confirmed', 'reconciliation_pending',
            'expiry_check_pending', 'effect_ambiguous'
        )
          AND opportunity.opportunity_state <> 'decision_created'
    )
THEN 1 ELSE 0 END AS fixture_validation;
