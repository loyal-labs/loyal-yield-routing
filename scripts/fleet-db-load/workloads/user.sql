\set vault_id random(1, 1000)
UPDATE loyal_yield.managed_vaults
SET last_seen_at = clock_timestamp()
WHERE id = :vault_id;

INSERT INTO loyal_yield.orchestration_outbox (
    cluster, event_kind, aggregate_kind, aggregate_id, dedupe_key, payload
)
VALUES (
    'localnet',
    'local_user_load',
    'managed_vault',
    :vault_id,
    'local-user-' || md5(
        clock_timestamp()::text || random()::text || (:client_id)::text
    ),
    jsonb_build_object(
        'source', 'local-user-emulator', 'vaultId', (:vault_id)::bigint
    )
)
ON CONFLICT (dedupe_key) DO UPDATE
SET updated_at = clock_timestamp();
