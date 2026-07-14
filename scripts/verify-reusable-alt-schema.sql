\set ON_ERROR_STOP on

DO $reusable_alt_verifier$
DECLARE
    missing_relations TEXT[];
    missing_columns TEXT[];
    inconsistent_reservations BIGINT;
    invalid_bindings BIGINT;
    invalid_membership BIGINT;
    invalid_operations BIGINT;
    invalid_usage_leases BIGINT;
    invalid_accounting BIGINT;
    invalid_provisioning_requests BIGINT;
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM loyal_yield.schema_migrations
        WHERE version = 17
          AND name = 'reusable_route_lookup_tables'
    ) THEN
        RAISE EXCEPTION 'migration 17 reusable_route_lookup_tables is not recorded';
    END IF;

    SELECT array_agg(required_relation ORDER BY required_relation)
    INTO missing_relations
    FROM unnest(ARRAY[
        'lookup_table_families',
        'lookup_table_manifests',
        'lookup_table_manifest_addresses',
        'lookup_table_vault_desired_heads',
        'lookup_table_vault_bindings',
        'lookup_table_usage_leases',
        'lookup_table_provisioning_requests',
        'lookup_table_provisioning_request_addresses',
        'lookup_table_operations',
        'lookup_table_operation_addresses',
        'lookup_table_addresses',
        'lookup_table_route_readiness_current',
        'lookup_table_rollout_controls'
    ]) AS required_relation
    WHERE to_regclass('loyal_yield.' || required_relation) IS NULL;

    IF missing_relations IS NOT NULL THEN
        RAISE EXCEPTION 'missing reusable ALT relations: %', missing_relations;
    END IF;

    SELECT array_agg(required_column ORDER BY required_column)
    INTO missing_columns
    FROM unnest(ARRAY[
        'family_id',
        'allocation_kind',
        'generation',
        'shard_ordinal',
        'desired_state',
        'accepting_allocations',
        'allocation_high_water',
        'reserved_address_count',
        'usable_address_count',
        'last_extended_start_index',
        'last_verified_slot',
        'last_verified_at',
        'mutation_epoch',
        'rollback_until'
    ]) AS required_column
    WHERE NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'loyal_yield'
          AND table_name = 'route_lookup_tables'
          AND column_name = required_column
    );

    IF missing_columns IS NOT NULL THEN
        RAISE EXCEPTION 'missing reusable ALT physical columns: %', missing_columns;
    END IF;

    SELECT array_agg(required_table || '.' || required_column ORDER BY required_table, required_column)
    INTO missing_columns
    FROM (VALUES
        ('lookup_table_families', 'rollback_until'),
        ('lookup_table_families', 'hard_capacity'),
        ('lookup_table_families', 'largest_atomic_expansion'),
        ('lookup_table_families', 'safety_margin'),
        ('lookup_table_families', 'allocation_high_water'),
        ('lookup_table_vault_bindings', 'rollback_until'),
        ('lookup_table_vault_bindings', 'desired_head_revision'),
        ('lookup_table_vault_desired_heads', 'manifest_id'),
        ('lookup_table_vault_desired_heads', 'desired_revision'),
        ('lookup_table_operations', 'estimated_fee_lamports'),
        ('lookup_table_operations', 'estimated_rent_lamports'),
        ('lookup_table_operations', 'actual_fee_lamports'),
        ('lookup_table_operations', 'actual_rent_lamports'),
        ('lookup_table_operations', 'reclaimed_rent_lamports'),
        ('lookup_table_route_readiness_current', 'selection_kind'),
        ('lookup_table_route_readiness_current', 'fallback_reason'),
        ('lookup_table_route_readiness_current', 'rollout_mode'),
        ('lookup_table_route_readiness_current', 'selected_table_ids'),
        ('lookup_table_route_readiness_current', 'selected_table_count'),
        ('lookup_table_route_readiness_current', 'packet_fits'),
        ('lookup_table_route_readiness_current', 'simulation_state'),
        ('lookup_table_route_readiness_current', 'simulation_units_consumed'),
        ('lookup_table_route_readiness_current', 'simulation_error'),
        ('lookup_table_usage_leases', 'lease_kind'),
        ('lookup_table_usage_leases', 'reference_key'),
        ('lookup_table_usage_leases', 'route_lookup_table_id'),
        ('lookup_table_usage_leases', 'expires_at'),
        ('lookup_table_provisioning_requests', 'requirements_fingerprint'),
        ('lookup_table_provisioning_requests', 'request_status'),
        ('lookup_table_provisioning_requests', 'lease_expires_at'),
        ('lookup_table_provisioning_requests', 'fencing_token'),
        ('lookup_table_provisioning_requests', 'desired_shared_address_count'),
        ('lookup_table_provisioning_requests', 'desired_vault_address_count'),
        ('lookup_table_provisioning_requests', 'sealed_at'),
        ('lookup_table_provisioning_request_addresses', 'request_id'),
        ('lookup_table_provisioning_request_addresses', 'address'),
        ('lookup_table_provisioning_request_addresses', 'semantic_class'),
        ('lookup_table_provisioning_request_addresses', 'ordinal'),
        ('lookup_table_provisioning_request_addresses', 'account_role'),
        ('lookup_table_provisioning_request_addresses', 'is_writable')
    ) AS required(required_table, required_column)
    WHERE NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'loyal_yield'
          AND table_name = required_table
          AND column_name = required_column
    );

    IF missing_columns IS NOT NULL THEN
        RAISE EXCEPTION 'missing reusable ALT control-plane columns: %', missing_columns;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM loyal_yield.lookup_table_families
        WHERE hard_capacity NOT BETWEEN 1 AND 256
           OR largest_atomic_expansion <= 0
           OR safety_margin <= 0
           OR largest_atomic_expansion + safety_margin >= hard_capacity
           OR allocation_high_water
                <> hard_capacity - largest_atomic_expansion - safety_margin
    ) THEN
        RAISE EXCEPTION 'lookup-table family durable capacity formula invariant failed';
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'loyal_yield'
          AND tablename = 'lookup_table_families'
          AND indexname = 'lookup_table_families_one_active_kind_idx'
    ) OR EXISTS (
        SELECT 1
        FROM loyal_yield.lookup_table_families
        WHERE desired_state = 'active'
        GROUP BY cluster, kind
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'lookup-table family active kind is not deterministic';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM loyal_yield.route_lookup_tables
        WHERE address_count > 256
           OR COALESCE(usable_address_count, 0) > address_count
           OR COALESCE(reserved_address_count, 0) > COALESCE(allocation_high_water, 256)
    ) THEN
        RAISE EXCEPTION 'physical lookup-table capacity invariant failed';
    END IF;

    SELECT count(*)
    INTO inconsistent_reservations
    FROM loyal_yield.route_lookup_tables route_table
    LEFT JOIN (
        SELECT route_lookup_table_id,
               sum(reserved_capacity)::INTEGER AS expected_reserved
        FROM (
            SELECT route_lookup_table_id, vault_id, family_id, binding_ordinal,
                   max(reserved_capacity) AS reserved_capacity
            FROM loyal_yield.lookup_table_vault_bindings
            WHERE lifecycle_state IN (
                'preparing', 'warming', 'active', 'standby', 'retiring'
            )
            GROUP BY route_lookup_table_id, vault_id, family_id, binding_ordinal
        ) live_heads
        GROUP BY route_lookup_table_id
    ) binding_totals
      ON binding_totals.route_lookup_table_id = route_table.id
    WHERE route_table.family_id IS NOT NULL
      AND route_table.reserved_address_count
          <> COALESCE(binding_totals.expected_reserved, 0);

    IF inconsistent_reservations <> 0 THEN
        RAISE EXCEPTION 'reservation accounting mismatch on % table(s)', inconsistent_reservations;
    END IF;

    SELECT count(*)
    INTO invalid_bindings
    FROM loyal_yield.lookup_table_vault_bindings binding
    JOIN loyal_yield.route_lookup_tables route_table
      ON route_table.id = binding.route_lookup_table_id
    JOIN loyal_yield.lookup_table_manifests manifest
      ON manifest.id = binding.manifest_id
    WHERE route_table.family_id <> binding.family_id
       OR manifest.family_id <> binding.family_id
       OR manifest.vault_id <> binding.vault_id
       OR manifest.subject_kind <> 'vault'
       OR (
           binding.allocation_mode = 'packed_shard'
           AND route_table.allocation_kind <> 'vault_shard'
       )
       OR (
           binding.allocation_mode = 'dedicated'
           AND route_table.allocation_kind <> 'dedicated_vault'
       )
       OR (
           binding.lifecycle_state IN ('preparing', 'warming')
           AND NOT EXISTS (
               SELECT 1
               FROM loyal_yield.lookup_table_vault_desired_heads desired
               WHERE desired.family_id = binding.family_id
                 AND desired.vault_id = binding.vault_id
                 AND desired.binding_ordinal = binding.binding_ordinal
                 AND desired.manifest_id = binding.manifest_id
                 AND desired.desired_revision = binding.desired_head_revision
           )
       );

    SELECT invalid_bindings + count(*)
    INTO invalid_bindings
    FROM loyal_yield.lookup_table_vault_desired_heads desired
    JOIN loyal_yield.lookup_table_manifests manifest ON manifest.id = desired.manifest_id
    WHERE manifest.family_id <> desired.family_id
       OR manifest.vault_id <> desired.vault_id
       OR manifest.subject_kind <> 'vault'
       OR manifest.sealed_at IS NULL;

    IF invalid_bindings <> 0 THEN
        RAISE EXCEPTION 'family/manifest mismatch on % vault binding(s)', invalid_bindings;
    END IF;

    SELECT count(*)
    INTO invalid_membership
    FROM loyal_yield.route_lookup_tables route_table
    WHERE route_table.family_id IS NOT NULL
      AND route_table.last_verified_at IS NOT NULL
      AND route_table.address_count <> (
          SELECT count(*)
          FROM loyal_yield.lookup_table_addresses address
          WHERE address.route_lookup_table_id = route_table.id
      );

    IF invalid_membership <> 0 THEN
        RAISE EXCEPTION 'confirmed membership mismatch on % table(s)', invalid_membership;
    END IF;

    SELECT count(*)
    INTO invalid_operations
    FROM loyal_yield.lookup_table_operations
    WHERE operation_kind IN ('create', 'extend', 'rollover', 'deactivate', 'close')
      AND operation_state IN (
        'signed', 'submitted', 'confirmed', 'finalized', 'reconciled', 'complete'
    )
      AND (
          transaction_signature IS NULL
          OR message_hash IS NULL
          OR recent_blockhash IS NULL
          OR last_valid_block_height IS NULL
      );

    IF invalid_operations <> 0 THEN
        RAISE EXCEPTION 'signed operation metadata missing on % operation(s)', invalid_operations;
    END IF;

    SELECT count(*)
    INTO invalid_usage_leases
    FROM loyal_yield.lookup_table_usage_leases usage_lease
    LEFT JOIN loyal_yield.lookup_table_vault_bindings binding
      ON binding.id = usage_lease.binding_id
    WHERE usage_lease.binding_id IS NOT NULL
      AND (
          binding.route_lookup_table_id <> usage_lease.route_lookup_table_id
          OR binding.vault_id IS DISTINCT FROM usage_lease.vault_id
      );

    IF invalid_usage_leases <> 0 THEN
        RAISE EXCEPTION 'usage lease binding mismatch on % lease(s)', invalid_usage_leases;
    END IF;

    SELECT count(*)
    INTO invalid_accounting
    FROM loyal_yield.lookup_table_operations
    WHERE (
        operation_kind IN ('create', 'extend', 'rollover', 'deactivate', 'close')
        AND operation_state IN ('confirmed', 'finalized', 'reconciled', 'complete')
        AND actual_fee_lamports IS NULL
    ) OR (
        operation_kind IN ('create', 'extend', 'rollover')
        AND operation_state IN ('reconciled', 'complete')
        AND actual_rent_lamports IS NULL
    ) OR (
        operation_kind = 'close'
        AND operation_state IN ('reconciled', 'complete')
        AND reclaimed_rent_lamports IS NULL
    );

    IF invalid_accounting <> 0 THEN
        RAISE EXCEPTION 'lamport accounting missing on % operation(s)', invalid_accounting;
    END IF;

    SELECT count(*)
    INTO invalid_provisioning_requests
    FROM loyal_yield.lookup_table_provisioning_requests
    WHERE (shared_manifest_id IS NULL AND NULLIF(desired_shared_hash, '') IS NULL)
       OR (vault_manifest_id IS NULL AND NULLIF(desired_vault_hash, '') IS NULL)
       OR (
           request_status = 'planning'
           AND (lease_owner IS NULL OR lease_expires_at IS NULL OR sealed_at IS NULL)
       )
       OR (request_status = 'satisfied' AND satisfied_at IS NULL);

    SELECT invalid_provisioning_requests + count(*)
    INTO invalid_provisioning_requests
    FROM loyal_yield.lookup_table_provisioning_requests request
    WHERE request.sealed_at IS NOT NULL
      AND (
          (
              request.desired_shared_address_count <> (
                  SELECT count(*)
                  FROM loyal_yield.lookup_table_provisioning_request_addresses address
                  WHERE address.request_id = request.id
                    AND address.semantic_class = 'shared_market'
              )
          )
          OR (
              request.desired_vault_address_count <> (
                  SELECT count(*)
                  FROM loyal_yield.lookup_table_provisioning_request_addresses address
                  WHERE address.request_id = request.id
                    AND address.semantic_class = 'vault'
              )
          )
      );

    IF invalid_provisioning_requests <> 0 THEN
        RAISE EXCEPTION 'invalid provisioning lifecycle on % request(s)', invalid_provisioning_requests;
    END IF;
END
$reusable_alt_verifier$;

SELECT json_build_object(
    'status', 'reusable_alt_schema_ready',
    'families', (SELECT count(*) FROM loyal_yield.lookup_table_families),
    'physicalTables', (
        SELECT count(*)
        FROM loyal_yield.route_lookup_tables
        WHERE family_id IS NOT NULL
    ),
    'manifests', (SELECT count(*) FROM loyal_yield.lookup_table_manifests),
    'bindings', (SELECT count(*) FROM loyal_yield.lookup_table_vault_bindings),
    'desiredVaultHeads', (SELECT count(*) FROM loyal_yield.lookup_table_vault_desired_heads),
    'activeUsageLeases', (
        SELECT count(*)
        FROM loyal_yield.lookup_table_usage_leases
        WHERE released_at IS NULL AND expires_at > now()
    ),
    'pendingProvisioningRequests', (
        SELECT count(*)
        FROM loyal_yield.lookup_table_provisioning_requests
        WHERE request_status IN ('requested', 'planning', 'queued', 'failed')
    ),
    'pendingOperations', (
        SELECT count(*)
        FROM loyal_yield.lookup_table_operations
        WHERE operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
    ),
    'lamports', (
        SELECT json_build_object(
            'estimatedFees', COALESCE(sum(estimated_fee_lamports), 0),
            'estimatedRent', COALESCE(sum(estimated_rent_lamports), 0),
            'actualFees', COALESCE(sum(actual_fee_lamports), 0),
            'actualRent', COALESCE(sum(actual_rent_lamports), 0),
            'reclaimedRent', COALESCE(sum(reclaimed_rent_lamports), 0)
        )
        FROM loyal_yield.lookup_table_operations
    )
) AS reusable_alt_schema_verification;
