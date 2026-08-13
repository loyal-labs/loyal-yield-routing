\set vault_id random(1, 1000)
UPDATE loyal_yield.vault_reserve_positions_current
SET amount_raw = amount_raw + 1,
    observed_slot = observed_slot + 1,
    observed_at = clock_timestamp(),
    planning_metadata = jsonb_build_object(
        'source', 'local-mock-chain',
        'updatedAt', clock_timestamp()
    )
WHERE vault_id = :vault_id
  AND reserve = 'local-source-reserve';
