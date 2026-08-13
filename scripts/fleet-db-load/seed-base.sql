\set ON_ERROR_STOP on

SET synchronous_commit = off;
SET session_replication_role = replica;

INSERT INTO loyal_yield.route_policies (
    id, settings, authority, policy_seed, policy_account, vault_index,
    vault_pubkey, delegated_signers, threshold, route_modes, stable_mints,
    kamino_markets, kamino_liquidity_mints, universe_preset, risk_profile,
    swap_lanes, active, last_seen_slot, last_seen_signature
)
VALUES (
    1, 'local-settings', 'local-authority', 1, 'local-policy', 0,
    'local-policy-vault', ARRAY['local-delegated-signer'], 1,
    ARRAY['same_mint'], ARRAY['local-mint'], ARRAY['local-market'],
    ARRAY['local-mint'], 'local', 'local', '[]'::jsonb, true, 1,
    'local-policy-observation'
);

INSERT INTO loyal_yield.managed_vaults (
    id, settings, vault_index, vault_pubkey, active_policy_id, active
)
SELECT value,
       'local-settings-' || value,
       (value % 32000)::smallint,
       'local-vault-' || value,
       1,
       true
FROM generate_series(1, 1000) value;

INSERT INTO loyal_yield.vault_position_snapshots (
    id, vault_id, policy_id, observed_slot, observed_at, chain_slot,
    is_current, context
)
SELECT value, value, 1, 1000000, clock_timestamp(), 1000000, true,
       jsonb_build_object('source', 'local-mock-chain')
FROM generate_series(1, 1000) value;

INSERT INTO loyal_yield.vault_position_snapshot_positions (
    snapshot_id, reserve, market, liquidity_mint, amount_raw,
    supply_apy_bps, borrow_apy_bps, has_value, planning_metadata
)
SELECT value, 'local-source-reserve', 'local-market', 'local-mint',
       1000000 + value, 400, 0, true,
       jsonb_build_object('source', 'local-mock-chain')
FROM generate_series(1, 1000) value;

INSERT INTO loyal_yield.vault_reserve_positions_current (
    vault_id, reserve, market, liquidity_mint, amount_raw, has_value,
    supply_apy_bps, borrow_apy_bps, snapshot_id, observed_slot,
    observed_at, planning_metadata
)
SELECT value, 'local-source-reserve', 'local-market', 'local-mint',
       1000000 + value, true, 400, 0, value, 1000000,
       clock_timestamp(), jsonb_build_object('source', 'local-mock-chain')
FROM generate_series(1, 1000) value;

INSERT INTO loyal_yield.optimizer_epochs (
    id, cluster, epoch_key, market_slot, observed_at, expires_at, market_state
)
VALUES (
    1, 'localnet', 'local-epoch', 1000000, clock_timestamp(),
    clock_timestamp() + interval '7 days',
    '{"source":"local-mock-chain"}'::jsonb
);

INSERT INTO loyal_yield.fleet_planning_clusters (cluster)
VALUES ('localnet');

INSERT INTO loyal_yield.fleet_planning_state (
    cluster, full_sweep_started_at, full_sweep_completed_at,
    optimizer_epoch_key, optimizer_epoch_expires_at, complete_frontier,
    observed_vault_count, opportunity_count, selected_count, deferred_count,
    generation
)
VALUES (
    'localnet', clock_timestamp(), clock_timestamp(), 'local-epoch',
    clock_timestamp() + interval '7 days', true, 1000, 0, 0, 0, 1
);

SELECT setval('loyal_yield.route_policies_id_seq', 1, true);
SELECT setval('loyal_yield.managed_vaults_id_seq', 1000, true);
SELECT setval('loyal_yield.vault_position_snapshots_id_seq', 1000, true);
SELECT setval('loyal_yield.optimizer_epochs_id_seq', 1, true);

SET session_replication_role = origin;
SET synchronous_commit = on;
