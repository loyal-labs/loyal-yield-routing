\set ON_ERROR_STOP on

\if :{?unsafe_only}
\else

INSERT INTO loyal_yield.lookup_table_families
    (cluster, logical_name, kind, desired_state, planner_version,
     catalog_version, active_generation, provisioning_authority, payer,
     hard_capacity, largest_atomic_expansion, safety_margin,
     allocation_high_water)
VALUES
    ('reusable_alt_inflight_local', 'fixture-vault-shards', 'vault_shards',
     'active', 'inflight-verifier-v1', 'inflight-verifier-catalog-v1', 0,
     'fixture-authority', 'fixture-authority', 64, 8, 4, 52)
RETURNING id AS fixture_family_id
\gset

-- Planner fixture: older no-operation binding plus newer completed canonical
-- binding. The canonical table is missing one desired address. An unrelated
-- binding owns a queued verify operation on the same physical table; the
-- planner must not mistake that operation for canonical binding work.
INSERT INTO loyal_yield.route_policies
    (settings, authority, policy_seed, policy_account, vault_index,
     vault_pubkey, threshold, last_seen_slot, last_seen_signature)
VALUES
    ('planner-settings', 'planner-authority', 0, 'planner-policy', 1,
     'planner-vault', 1, 1, 'planner-signature')
RETURNING id AS planner_policy_id
\gset

INSERT INTO loyal_yield.managed_vaults
    (settings, vault_index, vault_pubkey, active_policy_id)
VALUES
    ('planner-settings', 1, 'planner-vault', :planner_policy_id)
RETURNING id AS planner_vault_id
\gset

INSERT INTO loyal_yield.lookup_table_manifests
    (family_id, subject_kind, subject_key, vault_id, desired_set_hash,
     address_count, source_slot, planner_version, catalog_version)
VALUES
    (:fixture_family_id, 'vault', 'planner-duplicate', :planner_vault_id,
     'planner-desired-hash', 2, 100, 'inflight-verifier-v1',
     'inflight-verifier-catalog-v1')
RETURNING id AS planner_manifest_id
\gset

INSERT INTO loyal_yield.lookup_table_manifest_addresses
    (manifest_id, address, ordinal, semantic_class, account_role, is_writable)
VALUES
    (:planner_manifest_id, 'planner-address-a', 0, 'vault', 'planner_a', TRUE),
    (:planner_manifest_id, 'planner-address-b', 1, 'vault', 'planner_b', FALSE);

UPDATE loyal_yield.lookup_table_manifests
SET sealed_at = now()
WHERE id = :planner_manifest_id;

INSERT INTO loyal_yield.lookup_table_vault_desired_heads
    (family_id, vault_id, binding_ordinal, manifest_id, desired_revision)
VALUES
    (:fixture_family_id, :planner_vault_id, 0, :planner_manifest_id, 1);

INSERT INTO loyal_yield.route_lookup_tables
    (cluster, scope, table_address, authority, payer, status, durable,
     address_count, address_hash, addresses, family_id, allocation_kind,
     generation, shard_ordinal, desired_state, accepting_allocations,
     allocation_high_water, reserved_address_count, usable_address_count,
     last_extended_slot, last_extended_start_index, last_verified_slot,
     last_verified_at, mutation_epoch, created_at)
VALUES
    ('reusable_alt_inflight_local', 'planner-stale-table',
     'planner-stale-table-address', 'fixture-authority', 'fixture-authority',
     'warming', TRUE, 0, '', '[]'::jsonb, :fixture_family_id, 'vault_shard',
     0, 10, 'preparing', TRUE, 52, 0, 0, NULL, NULL, NULL, NULL, 0,
     now() - interval '20 minutes')
RETURNING id AS planner_stale_table_id
\gset

INSERT INTO loyal_yield.route_lookup_tables
    (cluster, scope, table_address, authority, payer, status, durable,
     address_count, address_hash, addresses, family_id, allocation_kind,
     generation, shard_ordinal, desired_state, accepting_allocations,
     allocation_high_water, reserved_address_count, usable_address_count,
     last_extended_slot, last_extended_start_index, last_verified_slot,
     last_verified_at, mutation_epoch, created_at)
VALUES
    ('reusable_alt_inflight_local', 'planner-canonical-table',
     'planner-canonical-table-address', 'fixture-authority',
     'fixture-authority', 'usable', TRUE, 1, 'planner-address-hash',
     '["planner-address-a"]'::jsonb, :fixture_family_id, 'vault_shard',
     0, 11, 'active', TRUE, 52, 0, 1, 101, 0, 120, now(), 0,
     now() - interval '10 minutes')
RETURNING id AS planner_canonical_table_id
\gset

INSERT INTO loyal_yield.lookup_table_vault_bindings
    (vault_id, family_id, route_lookup_table_id, manifest_id,
     binding_ordinal, desired_head_revision, allocation_mode,
     reserved_capacity, lifecycle_state, created_at, updated_at)
VALUES
    (:planner_vault_id, :fixture_family_id, :planner_stale_table_id,
     :planner_manifest_id, 0, 1, 'packed_shard', 2, 'preparing',
     now() - interval '20 minutes', now() - interval '20 minutes')
RETURNING id AS planner_stale_binding_id
\gset

INSERT INTO loyal_yield.lookup_table_vault_bindings
    (vault_id, family_id, route_lookup_table_id, manifest_id,
     binding_ordinal, desired_head_revision, allocation_mode,
     reserved_capacity, lifecycle_state, created_at, updated_at)
VALUES
    (:planner_vault_id, :fixture_family_id, :planner_canonical_table_id,
     :planner_manifest_id, 0, 1, 'packed_shard', 2, 'preparing',
     now() - interval '10 minutes', now() - interval '10 minutes')
RETURNING id AS planner_canonical_binding_id
\gset

INSERT INTO loyal_yield.lookup_table_operations
    (idempotency_key, family_id, route_lookup_table_id, manifest_id,
     binding_id, operation_kind, operation_state, operation_context,
     mutation_epoch, transaction_signature, message_hash, recent_blockhash,
     last_valid_block_height, submitted_slot, submitted_at, confirmed_slot,
     confirmed_at, finalized_slot, finalized_at, reconciled_slot,
     reconciled_at, completed_at)
VALUES
    ('planner-canonical-complete', :fixture_family_id,
     :planner_canonical_table_id, :planner_manifest_id,
     :planner_canonical_binding_id, 'extend', 'complete',
     '{"fixture":"planner-canonical"}'::jsonb, 0,
     'planner-canonical-signature', 'planner-canonical-message',
     'planner-canonical-blockhash', 1000, 100, now(), 101, now(), 102, now(),
     102, now(), now())
RETURNING id AS planner_complete_operation_id
\gset

INSERT INTO loyal_yield.lookup_table_operation_addresses
    (operation_id, address, ordinal)
VALUES
    (:planner_complete_operation_id, 'planner-address-a', 0);

INSERT INTO loyal_yield.lookup_table_addresses
    (route_lookup_table_id, address, ordinal, added_operation_id, added_slot,
     usable_after_slot, last_verified_slot, last_verified_at)
VALUES
    (:planner_canonical_table_id, 'planner-address-a', 0,
     :planner_complete_operation_id, 101, 102, 120, now());

INSERT INTO loyal_yield.route_policies
    (settings, authority, policy_seed, policy_account, vault_index,
     vault_pubkey, threshold, last_seen_slot, last_seen_signature)
VALUES
    ('unrelated-settings', 'unrelated-authority', 0, 'unrelated-policy', 2,
     'unrelated-vault', 1, 1, 'unrelated-signature')
RETURNING id AS unrelated_policy_id
\gset

INSERT INTO loyal_yield.managed_vaults
    (settings, vault_index, vault_pubkey, active_policy_id)
VALUES
    ('unrelated-settings', 2, 'unrelated-vault', :unrelated_policy_id)
RETURNING id AS unrelated_vault_id
\gset

INSERT INTO loyal_yield.lookup_table_manifests
    (family_id, subject_kind, subject_key, vault_id, desired_set_hash,
     address_count, source_slot, planner_version, catalog_version)
VALUES
    (:fixture_family_id, 'vault', 'unrelated-binding', :unrelated_vault_id,
     'unrelated-hash', 1, 100, 'inflight-verifier-v1',
     'inflight-verifier-catalog-v1')
RETURNING id AS unrelated_manifest_id
\gset

INSERT INTO loyal_yield.lookup_table_manifest_addresses
    (manifest_id, address, ordinal, semantic_class, account_role, is_writable)
VALUES
    (:unrelated_manifest_id, 'planner-address-a', 0, 'vault',
     'unrelated_a', FALSE);

UPDATE loyal_yield.lookup_table_manifests
SET sealed_at = now()
WHERE id = :unrelated_manifest_id;

INSERT INTO loyal_yield.lookup_table_vault_bindings
    (vault_id, family_id, route_lookup_table_id, manifest_id,
     binding_ordinal, desired_head_revision, allocation_mode,
     reserved_capacity, lifecycle_state, active_from_slot, activated_at)
VALUES
    (:unrelated_vault_id, :fixture_family_id, :planner_canonical_table_id,
     :unrelated_manifest_id, 0, 1, 'packed_shard', 1, 'active', 102, now())
RETURNING id AS unrelated_binding_id
\gset

INSERT INTO loyal_yield.lookup_table_operations
    (idempotency_key, family_id, route_lookup_table_id, manifest_id,
     binding_id, operation_kind, operation_state, operation_context,
     mutation_epoch)
VALUES
    ('unrelated-pending-verify', :fixture_family_id,
     :planner_canonical_table_id, :unrelated_manifest_id,
     :unrelated_binding_id, 'verify', 'queued',
     '{"fixture":"unrelated"}'::jsonb, 0)
RETURNING id AS unrelated_operation_id
\gset

-- Guarded SQL repair fixture.
INSERT INTO loyal_yield.route_policies
    (settings, authority, policy_seed, policy_account, vault_index,
     vault_pubkey, threshold, last_seen_slot, last_seen_signature)
VALUES
    ('repair-settings', 'repair-authority', 0, 'repair-policy', 3,
     'repair-vault', 1, 1, 'repair-signature')
RETURNING id AS repair_policy_id
\gset

INSERT INTO loyal_yield.managed_vaults
    (settings, vault_index, vault_pubkey, active_policy_id)
VALUES
    ('repair-settings', 3, 'repair-vault', :repair_policy_id)
RETURNING id AS repair_vault_id
\gset

INSERT INTO loyal_yield.lookup_table_manifests
    (family_id, subject_kind, subject_key, vault_id, desired_set_hash,
     address_count, source_slot, planner_version, catalog_version)
VALUES
    (:fixture_family_id, 'vault', 'sql-repair-duplicate', :repair_vault_id,
     'repair-hash', 1, 200, 'inflight-verifier-v1',
     'inflight-verifier-catalog-v1')
RETURNING id AS repair_manifest_id
\gset

INSERT INTO loyal_yield.lookup_table_manifest_addresses
    (manifest_id, address, ordinal, semantic_class, account_role, is_writable)
VALUES
    (:repair_manifest_id, 'repair-address', 0, 'vault', 'repair', TRUE);

UPDATE loyal_yield.lookup_table_manifests
SET sealed_at = now()
WHERE id = :repair_manifest_id;

INSERT INTO loyal_yield.lookup_table_vault_desired_heads
    (family_id, vault_id, binding_ordinal, manifest_id, desired_revision)
VALUES
    (:fixture_family_id, :repair_vault_id, 0, :repair_manifest_id, 1);

INSERT INTO loyal_yield.route_lookup_tables
    (cluster, scope, table_address, authority, payer, status, durable,
     address_count, address_hash, addresses, family_id, allocation_kind,
     generation, shard_ordinal, desired_state, accepting_allocations,
     allocation_high_water, reserved_address_count, usable_address_count,
     last_extended_slot, last_extended_start_index, last_verified_slot,
     last_verified_at, mutation_epoch, created_at)
VALUES
    ('reusable_alt_inflight_local', 'repair-stale-table',
     'repair-stale-table-address', 'fixture-authority', 'fixture-authority',
     'warming', TRUE, 0, '', '[]'::jsonb, :fixture_family_id, 'vault_shard',
     0, 20, 'preparing', TRUE, 52, 0, 0, NULL, NULL, NULL, NULL, 0,
     now() - interval '20 minutes')
RETURNING id AS repair_stale_table_id
\gset

INSERT INTO loyal_yield.route_lookup_tables
    (cluster, scope, table_address, authority, payer, status, durable,
     address_count, address_hash, addresses, family_id, allocation_kind,
     generation, shard_ordinal, desired_state, accepting_allocations,
     allocation_high_water, reserved_address_count, usable_address_count,
     last_extended_slot, last_extended_start_index, last_verified_slot,
     last_verified_at, mutation_epoch, created_at)
VALUES
    ('reusable_alt_inflight_local', 'repair-canonical-table',
     'repair-canonical-table-address', 'fixture-authority',
     'fixture-authority', 'usable', TRUE, 1, 'repair-address-hash',
     '["repair-address"]'::jsonb, :fixture_family_id, 'vault_shard',
     0, 21, 'active', TRUE, 52, 0, 1, 201, 0, 220, now(), 0,
     now() - interval '10 minutes')
RETURNING id AS repair_canonical_table_id
\gset

INSERT INTO loyal_yield.lookup_table_vault_bindings
    (vault_id, family_id, route_lookup_table_id, manifest_id,
     binding_ordinal, desired_head_revision, allocation_mode,
     reserved_capacity, lifecycle_state, created_at, updated_at)
VALUES
    (:repair_vault_id, :fixture_family_id, :repair_stale_table_id,
     :repair_manifest_id, 0, 1, 'packed_shard', 1, 'preparing',
     now() - interval '20 minutes', now() - interval '20 minutes')
RETURNING id AS repair_stale_binding_id
\gset

INSERT INTO loyal_yield.lookup_table_vault_bindings
    (vault_id, family_id, route_lookup_table_id, manifest_id,
     binding_ordinal, desired_head_revision, allocation_mode,
     reserved_capacity, lifecycle_state, created_at, updated_at)
VALUES
    (:repair_vault_id, :fixture_family_id, :repair_canonical_table_id,
     :repair_manifest_id, 0, 1, 'packed_shard', 1, 'preparing',
     now() - interval '10 minutes', now() - interval '10 minutes')
RETURNING id AS repair_canonical_binding_id
\gset

INSERT INTO loyal_yield.lookup_table_operations
    (idempotency_key, family_id, route_lookup_table_id, manifest_id,
     binding_id, operation_kind, operation_state, operation_context,
     mutation_epoch, transaction_signature, message_hash, recent_blockhash,
     last_valid_block_height, submitted_slot, submitted_at, confirmed_slot,
     confirmed_at, finalized_slot, finalized_at, reconciled_slot,
     reconciled_at, completed_at)
VALUES
    ('repair-canonical-complete', :fixture_family_id,
     :repair_canonical_table_id, :repair_manifest_id,
     :repair_canonical_binding_id, 'extend', 'complete',
     '{"fixture":"repair-canonical"}'::jsonb, 0,
     'repair-canonical-signature', 'repair-canonical-message',
     'repair-canonical-blockhash', 2000, 200, now(), 201, now(), 202, now(),
     202, now(), now());

-- Repaired terminal successor fixture. The old desired head remains in-flight
-- with an append-only repaired permanent failure and a completed successor.
-- Advancing to the new manifest must retire this old binding and continue
-- planning; the historical repaired failure must not poison the new request.
INSERT INTO loyal_yield.route_policies
    (settings, authority, policy_seed, policy_account, vault_index,
     vault_pubkey, threshold, last_seen_slot, last_seen_signature)
VALUES
    ('repaired-terminal-settings', 'repaired-terminal-authority', 0,
     'repaired-terminal-policy', 5, 'repaired-terminal-vault', 1, 1,
     'repaired-terminal-signature')
RETURNING id AS repaired_terminal_policy_id
\gset

INSERT INTO loyal_yield.managed_vaults
    (settings, vault_index, vault_pubkey, active_policy_id)
VALUES
    ('repaired-terminal-settings', 5, 'repaired-terminal-vault',
     :repaired_terminal_policy_id)
RETURNING id AS repaired_terminal_vault_id
\gset

INSERT INTO loyal_yield.lookup_table_manifests
    (family_id, subject_kind, subject_key, vault_id, desired_set_hash,
     address_count, source_slot, planner_version, catalog_version)
VALUES
    (:fixture_family_id, 'vault', 'repaired-terminal-old',
     :repaired_terminal_vault_id, 'repaired-terminal-old-hash', 1, 400,
     'inflight-verifier-v1', 'inflight-verifier-catalog-v1')
RETURNING id AS repaired_terminal_old_manifest_id
\gset

INSERT INTO loyal_yield.lookup_table_manifest_addresses
    (manifest_id, address, ordinal, semantic_class, account_role, is_writable)
VALUES
    (:repaired_terminal_old_manifest_id, 'repaired-terminal-address-a', 0,
     'vault', 'repaired_terminal_a', TRUE);

UPDATE loyal_yield.lookup_table_manifests
SET sealed_at = now()
WHERE id = :repaired_terminal_old_manifest_id;

INSERT INTO loyal_yield.lookup_table_manifests
    (family_id, subject_kind, subject_key, vault_id, desired_set_hash,
     address_count, source_slot, planner_version, catalog_version)
VALUES
    (:fixture_family_id, 'vault', 'repaired-terminal-new',
     :repaired_terminal_vault_id, 'repaired-terminal-new-hash', 2, 401,
     'inflight-verifier-v1', 'inflight-verifier-catalog-v1')
RETURNING id AS repaired_terminal_new_manifest_id
\gset

INSERT INTO loyal_yield.lookup_table_manifest_addresses
    (manifest_id, address, ordinal, semantic_class, account_role, is_writable)
VALUES
    (:repaired_terminal_new_manifest_id, 'repaired-terminal-address-a', 0,
     'vault', 'repaired_terminal_a', TRUE),
    (:repaired_terminal_new_manifest_id, 'repaired-terminal-address-b', 1,
     'vault', 'repaired_terminal_b', FALSE);

UPDATE loyal_yield.lookup_table_manifests
SET sealed_at = now()
WHERE id = :repaired_terminal_new_manifest_id;

INSERT INTO loyal_yield.lookup_table_vault_desired_heads
    (family_id, vault_id, binding_ordinal, manifest_id, desired_revision)
VALUES
    (:fixture_family_id, :repaired_terminal_vault_id, 0,
     :repaired_terminal_old_manifest_id, 1);

INSERT INTO loyal_yield.route_lookup_tables
    (cluster, scope, table_address, authority, payer, status, durable,
     address_count, address_hash, addresses, family_id, allocation_kind,
     generation, shard_ordinal, desired_state, accepting_allocations,
     allocation_high_water, reserved_address_count, usable_address_count,
     last_extended_slot, last_extended_start_index, last_verified_slot,
     last_verified_at, mutation_epoch, created_at)
VALUES
    ('reusable_alt_inflight_local', 'repaired-terminal-table',
     'repaired-terminal-table-address', 'fixture-authority',
     'fixture-authority', 'usable', TRUE, 1, 'repaired-terminal-address-hash',
     '["repaired-terminal-address-a"]'::jsonb, :fixture_family_id,
     'vault_shard', 0, 32, 'active', TRUE, 52, 0, 1, 401, 0, 420, now(), 0,
     now() - interval '10 minutes')
RETURNING id AS repaired_terminal_table_id
\gset

INSERT INTO loyal_yield.lookup_table_vault_bindings
    (vault_id, family_id, route_lookup_table_id, manifest_id,
     binding_ordinal, desired_head_revision, allocation_mode,
     reserved_capacity, lifecycle_state, created_at, updated_at)
VALUES
    (:repaired_terminal_vault_id, :fixture_family_id,
     :repaired_terminal_table_id, :repaired_terminal_old_manifest_id, 0, 1,
     'packed_shard', 2, 'preparing', now() - interval '10 minutes',
     now() - interval '10 minutes')
RETURNING id AS repaired_terminal_binding_id
\gset

INSERT INTO loyal_yield.lookup_table_operations
    (idempotency_key, family_id, route_lookup_table_id, manifest_id,
     binding_id, operation_kind, operation_state, operation_context,
     mutation_epoch, error_code, error_detail)
VALUES
    ('repaired-terminal-root', :fixture_family_id, :repaired_terminal_table_id,
     :repaired_terminal_old_manifest_id, :repaired_terminal_binding_id,
     'extend', 'permanent_failure', '{"fixture":"repaired-terminal-root"}'::jsonb,
     0, 'synthetic_terminal_failure', 'synthetic no-effect failure')
RETURNING id AS repaired_terminal_root_operation_id
\gset

INSERT INTO loyal_yield.lookup_table_operations
    (idempotency_key, family_id, route_lookup_table_id, manifest_id,
     binding_id, operation_kind, operation_state, operation_context,
     mutation_epoch, transaction_signature, message_hash, recent_blockhash,
     last_valid_block_height, submitted_slot, submitted_at, confirmed_slot,
     confirmed_at, finalized_slot, finalized_at, reconciled_slot,
     reconciled_at, completed_at, attempt_generation, retry_of_operation_id)
VALUES
    ('repaired-terminal-successor', :fixture_family_id,
     :repaired_terminal_table_id, :repaired_terminal_old_manifest_id,
     :repaired_terminal_binding_id, 'extend', 'complete',
     '{"fixture":"repaired-terminal-successor"}'::jsonb, 0,
     'repaired-terminal-successor-signature',
     'repaired-terminal-successor-message',
     'repaired-terminal-successor-blockhash', 4000, 400, now(), 401, now(),
     402, now(), 402, now(), now(), 2, :repaired_terminal_root_operation_id)
RETURNING id AS repaired_terminal_successor_operation_id
\gset

INSERT INTO loyal_yield.lookup_table_terminal_repairs
    (cluster, repair_kind, route_lookup_table_id, root_operation_id,
     successor_operation_id, expected_control_epoch, expected_mutation_epoch,
     finalized_observed_slot, finalized_account_state, finalized_account_owner,
     finalized_authority, finalized_last_extended_slot, finalized_address_hash,
     finalized_address_count, no_effect_evidence, reason, updated_by)
VALUES
    ('reusable_alt_inflight_local', 'retry_suffix',
     :repaired_terminal_table_id, :repaired_terminal_root_operation_id,
     :repaired_terminal_successor_operation_id, 1, 0, 410,
     'active_lookup_table', 'synthetic-alt-owner', 'fixture-authority', 401,
     repeat('a', 64), 1, 'unsigned', 'synthetic repaired terminal verifier',
     'isolated-verifier')
RETURNING id AS repaired_terminal_repair_id
\gset

INSERT INTO loyal_yield.lookup_table_terminal_repair_operations
    (repair_id, operation_id, disposition)
VALUES
    (:repaired_terminal_repair_id, :repaired_terminal_root_operation_id, 'root');

\endif

-- Unsafe fixture. Both rows own operation evidence, so the repair must abort
-- without changing either binding.
\if :{?unsafe_only}

SELECT id AS fixture_family_id
FROM loyal_yield.lookup_table_families
WHERE cluster = 'reusable_alt_inflight_local'
  AND logical_name = 'fixture-vault-shards'
\gset

INSERT INTO loyal_yield.route_policies
    (settings, authority, policy_seed, policy_account, vault_index,
     vault_pubkey, threshold, last_seen_slot, last_seen_signature)
VALUES
    ('unsafe-settings', 'unsafe-authority', 0, 'unsafe-policy', 4,
     'unsafe-vault', 1, 1, 'unsafe-signature')
RETURNING id AS unsafe_policy_id
\gset

INSERT INTO loyal_yield.managed_vaults
    (settings, vault_index, vault_pubkey, active_policy_id)
VALUES
    ('unsafe-settings', 4, 'unsafe-vault', :unsafe_policy_id)
RETURNING id AS unsafe_vault_id
\gset

INSERT INTO loyal_yield.lookup_table_manifests
    (family_id, subject_kind, subject_key, vault_id, desired_set_hash,
     address_count, source_slot, planner_version, catalog_version)
VALUES
    (:fixture_family_id, 'vault', 'unsafe-duplicate', :unsafe_vault_id,
     'unsafe-hash', 1, 300, 'inflight-verifier-v1',
     'inflight-verifier-catalog-v1')
RETURNING id AS unsafe_manifest_id
\gset

INSERT INTO loyal_yield.lookup_table_manifest_addresses
    (manifest_id, address, ordinal, semantic_class, account_role, is_writable)
VALUES
    (:unsafe_manifest_id, 'unsafe-address', 0, 'vault', 'unsafe', TRUE);

UPDATE loyal_yield.lookup_table_manifests
SET sealed_at = now()
WHERE id = :unsafe_manifest_id;

INSERT INTO loyal_yield.lookup_table_vault_desired_heads
    (family_id, vault_id, binding_ordinal, manifest_id, desired_revision)
VALUES
    (:fixture_family_id, :unsafe_vault_id, 0, :unsafe_manifest_id, 1);

INSERT INTO loyal_yield.route_lookup_tables
    (cluster, scope, table_address, authority, payer, status, durable,
     address_count, address_hash, addresses, family_id, allocation_kind,
     generation, shard_ordinal, desired_state, accepting_allocations,
     allocation_high_water, reserved_address_count, usable_address_count,
     last_verified_slot, last_verified_at, mutation_epoch, created_at)
VALUES
    ('reusable_alt_inflight_local', 'unsafe-stale-table',
     'unsafe-stale-table-address', 'fixture-authority', 'fixture-authority',
     'warming', TRUE, 0, '', '[]'::jsonb, :fixture_family_id, 'vault_shard',
     0, 30, 'preparing', TRUE, 52, 0, 0, NULL, NULL, 0,
     now() - interval '20 minutes')
RETURNING id AS unsafe_stale_table_id
\gset

INSERT INTO loyal_yield.route_lookup_tables
    (cluster, scope, table_address, authority, payer, status, durable,
     address_count, address_hash, addresses, family_id, allocation_kind,
     generation, shard_ordinal, desired_state, accepting_allocations,
     allocation_high_water, reserved_address_count, usable_address_count,
     last_verified_slot, last_verified_at, mutation_epoch, created_at)
VALUES
    ('reusable_alt_inflight_local', 'unsafe-canonical-table',
     'unsafe-canonical-table-address', 'fixture-authority',
     'fixture-authority', 'usable', TRUE, 1, 'unsafe-address-hash',
     '["unsafe-address"]'::jsonb, :fixture_family_id, 'vault_shard',
     0, 31, 'active', TRUE, 52, 0, 1, 320, now(), 0,
     now() - interval '10 minutes')
RETURNING id AS unsafe_canonical_table_id
\gset

INSERT INTO loyal_yield.lookup_table_vault_bindings
    (vault_id, family_id, route_lookup_table_id, manifest_id,
     binding_ordinal, desired_head_revision, allocation_mode,
     reserved_capacity, lifecycle_state, created_at, updated_at)
VALUES
    (:unsafe_vault_id, :fixture_family_id, :unsafe_stale_table_id,
     :unsafe_manifest_id, 0, 1, 'packed_shard', 1, 'preparing',
     now() - interval '20 minutes', now() - interval '20 minutes')
RETURNING id AS unsafe_stale_binding_id
\gset

INSERT INTO loyal_yield.lookup_table_vault_bindings
    (vault_id, family_id, route_lookup_table_id, manifest_id,
     binding_ordinal, desired_head_revision, allocation_mode,
     reserved_capacity, lifecycle_state, created_at, updated_at)
VALUES
    (:unsafe_vault_id, :fixture_family_id, :unsafe_canonical_table_id,
     :unsafe_manifest_id, 0, 1, 'packed_shard', 1, 'preparing',
     now() - interval '10 minutes', now() - interval '10 minutes')
RETURNING id AS unsafe_canonical_binding_id
\gset

INSERT INTO loyal_yield.lookup_table_operations
    (idempotency_key, family_id, route_lookup_table_id, manifest_id,
     binding_id, operation_kind, operation_state, operation_context,
     mutation_epoch)
VALUES
    ('unsafe-stale-operation', :fixture_family_id, :unsafe_stale_table_id,
     :unsafe_manifest_id, :unsafe_stale_binding_id, 'verify', 'queued',
     '{"fixture":"unsafe-stale"}'::jsonb, 0);

INSERT INTO loyal_yield.lookup_table_operations
    (idempotency_key, family_id, route_lookup_table_id, manifest_id,
     binding_id, operation_kind, operation_state, operation_context,
     mutation_epoch, transaction_signature, message_hash, recent_blockhash,
     last_valid_block_height, submitted_slot, submitted_at, confirmed_slot,
     confirmed_at, finalized_slot, finalized_at, reconciled_slot,
     reconciled_at, completed_at)
VALUES
    ('unsafe-canonical-complete', :fixture_family_id,
     :unsafe_canonical_table_id, :unsafe_manifest_id,
     :unsafe_canonical_binding_id, 'extend', 'complete',
     '{"fixture":"unsafe-canonical"}'::jsonb, 0,
     'unsafe-canonical-signature', 'unsafe-canonical-message',
     'unsafe-canonical-blockhash', 3000, 300, now(), 301, now(), 302, now(),
     302, now(), now());

\endif
