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
    invalid_legacy_imports BIGINT;
    invalid_legacy_import_runs BIGINT;
    invalid_shared_catalogs BIGINT;
    invalid_shared_physical_drifts BIGINT;
    invalid_budget_reservations BIGINT;
    invalid_legacy_cleanup_attempts BIGINT;
    invalid_legacy_cleanup_budget_reservations BIGINT;
    invalid_broadcast_permits BIGINT;
    invalid_precutover_probes BIGINT;
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM loyal_yield.schema_migrations
        WHERE version = 20
          AND name = 'demand_driven_shared_market_catalog'
    ) THEN
        RAISE EXCEPTION 'migration 20 demand_driven_shared_market_catalog is not recorded';
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM loyal_yield.schema_migrations
        WHERE version = 21
          AND name = 'reusable_alt_production_controls'
    ) THEN
        RAISE EXCEPTION 'migration 21 reusable_alt_production_controls is not recorded';
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM loyal_yield.schema_migrations
        WHERE version = 22
          AND name = 'shared_market_alt_bundles'
    ) THEN
        RAISE EXCEPTION 'migration 22 shared_market_alt_bundles is not recorded';
    END IF;

    SELECT array_agg(required_relation ORDER BY required_relation)
    INTO missing_relations
    FROM unnest(ARRAY[
        'lookup_table_families',
        'lookup_table_legacy_import_runs',
        'lookup_table_legacy_import_evidence',
        'lookup_table_shared_market_catalog_revisions',
        'lookup_table_shared_market_catalog_heads',
        'lookup_table_shared_market_physical_drifts',
        'lookup_table_cluster_budget_reservations',
        'lookup_table_legacy_cleanup_attempts',
        'lookup_table_legacy_cleanup_budget_reservations',
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
        'lookup_table_rollout_controls',
        'lookup_table_provisioner_controls',
        'lookup_table_provisioner_broadcast_permits',
        'lookup_table_precutover_probe_runs',
        'lookup_table_precutover_probe_shared_tables'
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
        'rollback_until',
        'legacy_kind',
        'legacy_import_run_id'
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
        ('lookup_table_legacy_import_runs', 'rpc_genesis_hash'),
        ('lookup_table_legacy_import_runs', 'verified_slot'),
        ('lookup_table_legacy_import_runs', 'verified_at'),
        ('lookup_table_legacy_import_runs', 'legacy_kind'),
        ('lookup_table_legacy_import_runs', 'expected_table_count'),
        ('lookup_table_legacy_import_runs', 'verified_table_count'),
        ('lookup_table_legacy_import_runs', 'import_fingerprint'),
        ('lookup_table_legacy_import_runs', 'reason'),
        ('lookup_table_legacy_import_runs', 'updated_by'),
        ('lookup_table_legacy_import_evidence', 'import_run_id'),
        ('lookup_table_legacy_import_evidence', 'route_lookup_table_id'),
        ('lookup_table_legacy_import_evidence', 'legacy_kind'),
        ('lookup_table_legacy_import_evidence', 'observed_authority'),
        ('lookup_table_legacy_import_evidence', 'observed_owner'),
        ('lookup_table_legacy_import_evidence', 'observed_deactivation_slot'),
        ('lookup_table_legacy_import_evidence', 'observed_last_extended_slot'),
        ('lookup_table_legacy_import_evidence', 'observed_last_extended_start_index'),
        ('lookup_table_legacy_import_evidence', 'address_count'),
        ('lookup_table_legacy_import_evidence', 'address_hash'),
        ('lookup_table_legacy_import_evidence', 'addresses'),
        ('lookup_table_legacy_import_evidence', 'verified_slot'),
        ('lookup_table_legacy_import_evidence', 'verified_at'),
        ('lookup_table_shared_market_catalog_revisions', 'family_id'),
        ('lookup_table_shared_market_catalog_revisions', 'manifest_id'),
        ('lookup_table_shared_market_catalog_revisions', 'catalog_revision'),
        ('lookup_table_shared_market_catalog_revisions', 'catalog_version'),
        ('lookup_table_shared_market_catalog_revisions', 'desired_set_hash'),
        ('lookup_table_shared_market_catalog_revisions', 'enabled_mints_hash'),
        ('lookup_table_shared_market_catalog_revisions', 'reserve_set_hash'),
        ('lookup_table_shared_market_catalog_revisions', 'address_count'),
        ('lookup_table_shared_market_catalog_revisions', 'source_slot'),
        ('lookup_table_shared_market_catalog_revisions', 'source_observed_at'),
        ('lookup_table_shared_market_catalog_revisions', 'source_metadata'),
        ('lookup_table_shared_market_catalog_revisions', 'reason'),
        ('lookup_table_shared_market_catalog_revisions', 'updated_by'),
        ('lookup_table_shared_market_catalog_heads', 'family_id'),
        ('lookup_table_shared_market_catalog_heads', 'catalog_revision_id'),
        ('lookup_table_shared_market_catalog_heads', 'target_generation'),
        ('lookup_table_shared_market_catalog_heads', 'readiness_state'),
        ('lookup_table_shared_market_catalog_heads', 'activated_at'),
        ('lookup_table_shared_market_physical_drifts', 'evidence_hash'),
        ('lookup_table_shared_market_physical_drifts', 'cluster'),
        ('lookup_table_shared_market_physical_drifts', 'family_id'),
        ('lookup_table_shared_market_physical_drifts', 'catalog_revision_id'),
        ('lookup_table_shared_market_physical_drifts', 'route_lookup_table_id'),
        ('lookup_table_shared_market_physical_drifts', 'expected_mutation_epoch'),
        ('lookup_table_shared_market_physical_drifts', 'expected_table_address'),
        ('lookup_table_shared_market_physical_drifts', 'expected_authority'),
        ('lookup_table_shared_market_physical_drifts', 'observed_slot'),
        ('lookup_table_shared_market_physical_drifts', 'observed_table_present'),
        ('lookup_table_shared_market_physical_drifts', 'observed_authority'),
        ('lookup_table_shared_market_physical_drifts', 'observed_active'),
        ('lookup_table_shared_market_physical_drifts', 'observed_address_hash'),
        ('lookup_table_shared_market_physical_drifts', 'observed_addresses'),
        ('lookup_table_shared_market_physical_drifts', 'reason'),
        ('lookup_table_shared_market_physical_drifts', 'reported_by'),
        ('lookup_table_shared_market_physical_drifts', 'resolution_state'),
        ('lookup_table_shared_market_physical_drifts', 'resolution_target_generation'),
        ('lookup_table_shared_market_physical_drifts', 'resolved_at'),
        ('lookup_table_cluster_budget_reservations', 'cluster'),
        ('lookup_table_cluster_budget_reservations', 'operation_id'),
        ('lookup_table_cluster_budget_reservations', 'fencing_token'),
        ('lookup_table_cluster_budget_reservations', 'lease_owner'),
        ('lookup_table_cluster_budget_reservations', 'estimated_fee_lamports'),
        ('lookup_table_cluster_budget_reservations', 'estimated_rent_lamports'),
        ('lookup_table_cluster_budget_reservations', 'reserved_lamports'),
        ('lookup_table_cluster_budget_reservations', 'reserved_at'),
        ('lookup_table_cluster_budget_reservations', 'reserved_until'),
        ('lookup_table_legacy_cleanup_attempts', 'route_lookup_table_id'),
        ('lookup_table_legacy_cleanup_attempts', 'cluster'),
        ('lookup_table_legacy_cleanup_attempts', 'table_address'),
        ('lookup_table_legacy_cleanup_attempts', 'operation_kind'),
        ('lookup_table_legacy_cleanup_attempts', 'attempt_state'),
        ('lookup_table_legacy_cleanup_attempts', 'transaction_signature'),
        ('lookup_table_legacy_cleanup_attempts', 'estimated_fee_lamports'),
        ('lookup_table_legacy_cleanup_attempts', 'finalized_slot'),
        ('lookup_table_legacy_cleanup_attempts', 'actual_reclaimed_lamports'),
        ('lookup_table_legacy_cleanup_budget_reservations', 'legacy_cleanup_attempt_id'),
        ('lookup_table_legacy_cleanup_budget_reservations', 'cluster'),
        ('lookup_table_legacy_cleanup_budget_reservations', 'estimated_fee_lamports'),
        ('lookup_table_legacy_cleanup_budget_reservations', 'estimated_rent_lamports'),
        ('lookup_table_legacy_cleanup_budget_reservations', 'reserved_lamports'),
        ('lookup_table_legacy_cleanup_budget_reservations', 'reserved_at'),
        ('lookup_table_legacy_cleanup_budget_reservations', 'reserved_until'),
        ('lookup_table_provisioner_controls', 'paused'),
        ('lookup_table_provisioner_controls', 'control_epoch'),
        ('lookup_table_provisioner_broadcast_permits', 'operation_id'),
        ('lookup_table_provisioner_broadcast_permits', 'fencing_token'),
        ('lookup_table_provisioner_broadcast_permits', 'control_epoch'),
        ('lookup_table_provisioner_broadcast_permits', 'transaction_signature'),
        ('lookup_table_provisioner_broadcast_permits', 'message_hash'),
        ('lookup_table_provisioner_broadcast_permits', 'permit_state'),
        ('lookup_table_provisioner_broadcast_permits', 'granted_at'),
        ('lookup_table_provisioner_broadcast_permits', 'resolved_at'),
        ('lookup_table_precutover_probe_runs', 'provisioner_control_epoch'),
        ('lookup_table_precutover_probe_runs', 'finalized_slot'),
        ('lookup_table_precutover_probe_runs', 'finalized_address_hash'),
        ('lookup_table_precutover_probe_runs', 'finalized_address_count'),
        ('lookup_table_precutover_probe_runs', 'shared_table_bundle_hash'),
        ('lookup_table_precutover_probe_runs', 'shared_table_count'),
        ('lookup_table_precutover_probe_runs', 'finalized_bundle_address_count'),
        ('lookup_table_precutover_probe_shared_tables', 'probe_run_id'),
        ('lookup_table_precutover_probe_shared_tables', 'shard_ordinal'),
        ('lookup_table_precutover_probe_shared_tables', 'route_lookup_table_id'),
        ('lookup_table_precutover_probe_shared_tables', 'shared_table_address'),
        ('lookup_table_precutover_probe_shared_tables', 'shared_authority'),
        ('lookup_table_precutover_probe_shared_tables', 'shared_mutation_epoch'),
        ('lookup_table_precutover_probe_shared_tables', 'finalized_slot'),
        ('lookup_table_precutover_probe_shared_tables', 'finalized_last_extended_slot'),
        ('lookup_table_precutover_probe_shared_tables', 'finalized_address_hash'),
        ('lookup_table_precutover_probe_shared_tables', 'finalized_address_count'),
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
        ('lookup_table_provisioning_requests', 'error_code'),
        ('lookup_table_provisioning_requests', 'error_detail'),
        ('lookup_table_provisioning_requests', 'requested_at'),
        ('lookup_table_provisioning_requests', 'satisfied_at'),
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

    IF NOT EXISTS (
        SELECT 1
        FROM pg_indexes
        WHERE schemaname = 'loyal_yield'
          AND tablename = 'lookup_table_vault_bindings'
          AND indexname = 'lookup_table_vault_bindings_one_inflight_idx'
    ) OR EXISTS (
        SELECT 1
        FROM loyal_yield.lookup_table_vault_bindings
        WHERE lifecycle_state IN ('preparing', 'warming')
        GROUP BY vault_id, family_id, binding_ordinal
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'lookup-table in-flight binding head is not deterministic';
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
    INTO invalid_legacy_imports
    FROM loyal_yield.route_lookup_tables route_table
    WHERE (
        route_table.family_id IS NOT NULL
        AND (route_table.legacy_kind IS NOT NULL OR route_table.legacy_import_run_id IS NOT NULL)
    ) OR (
        route_table.legacy_import_run_id IS NOT NULL
        AND NOT EXISTS (
            SELECT 1
            FROM loyal_yield.lookup_table_legacy_import_evidence evidence
            JOIN loyal_yield.lookup_table_legacy_import_runs import_run
              ON import_run.id = evidence.import_run_id
            WHERE evidence.import_run_id = route_table.legacy_import_run_id
              AND evidence.route_lookup_table_id = route_table.id
              AND evidence.table_address = route_table.table_address
              AND evidence.scope = route_table.scope
              AND evidence.legacy_kind = route_table.legacy_kind
              AND evidence.expected_authority = route_table.authority
              AND evidence.address_count = route_table.address_count
              AND evidence.address_hash = route_table.address_hash
              AND evidence.addresses = route_table.addresses
              AND evidence.observed_last_extended_slot = route_table.last_extended_slot
              AND evidence.observed_last_extended_start_index = route_table.last_extended_start_index
              AND evidence.verified_slot = route_table.last_verified_slot
              AND evidence.verified_at = route_table.last_verified_at
              AND import_run.cluster = route_table.cluster
        )
    );

    SELECT count(*)
    INTO invalid_legacy_import_runs
    FROM loyal_yield.lookup_table_legacy_import_runs import_run
    WHERE import_run.expected_table_count <> (
        SELECT count(*)
        FROM loyal_yield.lookup_table_legacy_import_evidence evidence
        WHERE evidence.import_run_id = import_run.id
    );

    IF invalid_legacy_imports <> 0 OR invalid_legacy_import_runs <> 0 THEN
        RAISE EXCEPTION
            'legacy lookup-table import invariant failed for % table(s) and % run(s)',
            invalid_legacy_imports,
            invalid_legacy_import_runs;
    END IF;

    SELECT count(*)
    INTO invalid_shared_catalogs
    FROM loyal_yield.lookup_table_shared_market_catalog_heads head
    JOIN loyal_yield.lookup_table_shared_market_catalog_revisions revision
      ON revision.id = head.catalog_revision_id
    JOIN loyal_yield.lookup_table_families family ON family.id = head.family_id
    JOIN loyal_yield.lookup_table_manifests manifest ON manifest.id = revision.manifest_id
    WHERE revision.family_id <> head.family_id
       OR family.kind <> 'shared_market'
       OR family.desired_state <> 'active'
       OR manifest.family_id <> head.family_id
       OR manifest.subject_kind <> 'shared_market'
       OR manifest.sealed_at IS NULL
       OR manifest.catalog_version <> revision.catalog_version
       OR manifest.desired_set_hash <> revision.desired_set_hash
       OR manifest.address_count <> revision.address_count
       OR length(btrim(revision.reason)) = 0
       OR length(btrim(revision.updated_by)) = 0
       OR jsonb_typeof(revision.source_metadata) <> 'object'
       OR revision.address_count <> (
           SELECT count(*)
           FROM loyal_yield.lookup_table_manifest_addresses address
           WHERE address.manifest_id = revision.manifest_id
             AND address.semantic_class = 'shared_market'
       )
       OR (
           head.readiness_state = 'active'
           AND (
               head.target_generation IS DISTINCT FROM family.active_generation
               OR head.activated_at IS NULL
               OR (
                   revision.address_count + family.allocation_high_water - 1
               ) / family.allocation_high_water <> (
                   SELECT count(*)
                   FROM loyal_yield.route_lookup_tables route_table
                   WHERE route_table.family_id = family.id
                     AND route_table.generation = family.active_generation
                     AND route_table.allocation_kind = 'shared_market'
                     AND route_table.desired_state = 'active'
                     AND route_table.usable_address_count = route_table.address_count
                     AND route_table.last_verified_slot IS NOT NULL
               )
               OR EXISTS (
                   SELECT 1
                   FROM loyal_yield.route_lookup_tables route_table
                   WHERE route_table.family_id = family.id
                     AND route_table.generation = family.active_generation
                     AND route_table.allocation_kind = 'shared_market'
                     AND (
                         route_table.desired_state <> 'active'
                         OR route_table.address_count > family.allocation_high_water
                         OR route_table.usable_address_count <> route_table.address_count
                         OR route_table.last_verified_slot IS NULL
                     )
               )
               OR revision.address_count <> (
                   SELECT count(*)
                   FROM loyal_yield.route_lookup_tables route_table
                   JOIN loyal_yield.lookup_table_addresses membership
                     ON membership.route_lookup_table_id = route_table.id
                   WHERE route_table.family_id = family.id
                     AND route_table.generation = family.active_generation
                     AND route_table.allocation_kind = 'shared_market'
               )
               OR revision.address_count <> (
                   SELECT count(DISTINCT membership.address)
                   FROM loyal_yield.route_lookup_tables route_table
                   JOIN loyal_yield.lookup_table_addresses membership
                     ON membership.route_lookup_table_id = route_table.id
                   WHERE route_table.family_id = family.id
                     AND route_table.generation = family.active_generation
                     AND route_table.allocation_kind = 'shared_market'
               )
               OR EXISTS (
                   SELECT address.address
                   FROM loyal_yield.lookup_table_manifest_addresses address
                   WHERE address.manifest_id = revision.manifest_id
                   EXCEPT
                   SELECT membership.address
                   FROM loyal_yield.route_lookup_tables route_table
                   JOIN loyal_yield.lookup_table_addresses membership
                     ON membership.route_lookup_table_id = route_table.id
                   WHERE route_table.family_id = family.id
                     AND route_table.generation = family.active_generation
                     AND route_table.allocation_kind = 'shared_market'
               )
               OR EXISTS (
                   SELECT membership.address
                   FROM loyal_yield.route_lookup_tables route_table
                   JOIN loyal_yield.lookup_table_addresses membership
                     ON membership.route_lookup_table_id = route_table.id
                   WHERE route_table.family_id = family.id
                     AND route_table.generation = family.active_generation
                     AND route_table.allocation_kind = 'shared_market'
                   EXCEPT
                   SELECT address.address
                   FROM loyal_yield.lookup_table_manifest_addresses address
                   WHERE address.manifest_id = revision.manifest_id
               )
               OR EXISTS (
                   WITH expected AS (
                       SELECT (
                                  address.ordinal
                                  / family.allocation_high_water
                              )::INTEGER AS shard_ordinal,
                              (
                                  address.ordinal
                                  % family.allocation_high_water
                              )::INTEGER AS physical_ordinal,
                              address.address
                       FROM loyal_yield.lookup_table_manifest_addresses address
                       WHERE address.manifest_id = revision.manifest_id
                         AND address.semantic_class = 'shared_market'
                   ), observed AS (
                       SELECT route_table.shard_ordinal,
                              membership.ordinal AS physical_ordinal,
                              membership.address
                       FROM loyal_yield.route_lookup_tables route_table
                       JOIN loyal_yield.lookup_table_addresses membership
                         ON membership.route_lookup_table_id = route_table.id
                       WHERE route_table.family_id = family.id
                         AND route_table.generation = family.active_generation
                         AND route_table.allocation_kind = 'shared_market'
                   )
                   SELECT 1
                   FROM expected
                   FULL JOIN observed
                     USING (shard_ordinal, physical_ordinal)
                   WHERE expected.address IS DISTINCT FROM observed.address
               )
           )
       );

    IF invalid_shared_catalogs <> 0 THEN
        RAISE EXCEPTION
            'invalid authoritative shared-market catalog head(s): %',
            invalid_shared_catalogs;
    END IF;

    SELECT count(*)
    INTO invalid_shared_physical_drifts
    FROM loyal_yield.lookup_table_shared_market_physical_drifts drift
    JOIN loyal_yield.lookup_table_families family ON family.id = drift.family_id
    JOIN loyal_yield.lookup_table_shared_market_catalog_revisions revision
      ON revision.id = drift.catalog_revision_id
    JOIN loyal_yield.route_lookup_tables route_table
      ON route_table.id = drift.route_lookup_table_id
    WHERE family.cluster <> drift.cluster
       OR family.kind <> 'shared_market'
       OR revision.family_id <> drift.family_id
       OR route_table.family_id <> drift.family_id
       OR route_table.table_address <> drift.expected_table_address
       OR route_table.authority <> drift.expected_authority
       OR drift.expected_mutation_epoch < 0
       OR drift.observed_slot < 0
       OR drift.evidence_hash !~ '^[0-9a-f]{64}$'
       OR drift.observed_address_hash !~ '^[0-9a-f]{64}$'
       OR length(btrim(drift.reason)) = 0
       OR length(btrim(drift.reported_by)) = 0
       OR jsonb_typeof(drift.observed_addresses) <> 'array'
       OR jsonb_array_length(drift.observed_addresses) > 256
       OR (
           NOT drift.observed_table_present
           AND (
               drift.observed_authority IS NOT NULL
               OR drift.observed_active
               OR jsonb_array_length(drift.observed_addresses) <> 0
           )
       )
       OR (
           drift.resolution_state = 'open'
           AND (
               drift.resolution_target_generation IS NOT NULL
               OR drift.resolved_at IS NOT NULL
               OR NOT EXISTS (
                   SELECT 1
                   FROM loyal_yield.lookup_table_shared_market_catalog_heads head
                   WHERE head.family_id = drift.family_id
                     AND head.catalog_revision_id = drift.catalog_revision_id
                     AND head.readiness_state <> 'active'
               )
           )
       )
       OR (
           drift.resolution_state = 'resolved'
           AND (
               drift.resolution_target_generation IS NULL
               OR drift.resolved_at IS NULL
           )
       );

    IF invalid_shared_physical_drifts <> 0 THEN
        RAISE EXCEPTION 'invalid shared-market physical drift evidence row(s): %',
            invalid_shared_physical_drifts;
    END IF;

    SELECT count(*)
    INTO invalid_budget_reservations
    FROM loyal_yield.lookup_table_cluster_budget_reservations reservation
    JOIN loyal_yield.lookup_table_operations operation
      ON operation.id = reservation.operation_id
    JOIN loyal_yield.lookup_table_families family ON family.id = operation.family_id
    WHERE family.cluster <> reservation.cluster
       OR reservation.fencing_token <= 0
       OR length(btrim(reservation.lease_owner)) = 0
       OR reservation.reserved_lamports
            <> reservation.estimated_fee_lamports + reservation.estimated_rent_lamports
       OR reservation.reserved_lamports < 0
       OR reservation.estimated_fee_lamports < 0
       OR reservation.estimated_rent_lamports < 0
       OR reservation.reserved_until <= reservation.reserved_at
       OR reservation.fencing_token > operation.fencing_token;

    IF invalid_budget_reservations <> 0 THEN
        RAISE EXCEPTION 'invalid durable cluster budget reservation row(s): %',
            invalid_budget_reservations;
    END IF;

    SELECT count(*)
    INTO invalid_legacy_cleanup_budget_reservations
    FROM loyal_yield.lookup_table_legacy_cleanup_budget_reservations reservation
    JOIN loyal_yield.lookup_table_legacy_cleanup_attempts attempt
      ON attempt.id = reservation.legacy_cleanup_attempt_id
    WHERE attempt.cluster <> reservation.cluster
       OR reservation.reserved_lamports
            <> reservation.estimated_fee_lamports + reservation.estimated_rent_lamports
       OR reservation.reserved_lamports <= 0
       OR reservation.estimated_fee_lamports < 0
       OR reservation.estimated_rent_lamports < 0
       OR reservation.reserved_until <= reservation.reserved_at;

    IF invalid_legacy_cleanup_budget_reservations <> 0 THEN
        RAISE EXCEPTION 'invalid legacy cleanup cluster budget reservation row(s): %',
            invalid_legacy_cleanup_budget_reservations;
    END IF;

    SELECT count(*)
    INTO invalid_legacy_cleanup_attempts
    FROM loyal_yield.lookup_table_legacy_cleanup_attempts attempt
    JOIN loyal_yield.route_lookup_tables route_table
      ON route_table.id = attempt.route_lookup_table_id
    WHERE route_table.family_id IS NOT NULL
       OR route_table.legacy_import_run_id IS NULL
       OR route_table.cluster <> attempt.cluster
       OR route_table.table_address <> attempt.table_address
       OR (
           attempt.attempt_state IN ('signed', 'submitted', 'needs_reconcile', 'complete')
           AND NOT EXISTS (
               SELECT 1
               FROM loyal_yield.lookup_table_legacy_cleanup_budget_reservations reservation
               WHERE reservation.legacy_cleanup_attempt_id = attempt.id
                 AND reservation.cluster = attempt.cluster
                 AND reservation.estimated_fee_lamports = attempt.estimated_fee_lamports
                 AND reservation.reserved_lamports =
                     attempt.estimated_fee_lamports + reservation.estimated_rent_lamports
           )
       );

    IF invalid_legacy_cleanup_attempts <> 0 THEN
        RAISE EXCEPTION 'invalid durable legacy cleanup attempt row(s): %',
            invalid_legacy_cleanup_attempts;
    END IF;

    SELECT count(*)
    INTO invalid_broadcast_permits
    FROM loyal_yield.lookup_table_provisioner_broadcast_permits permit
    JOIN loyal_yield.lookup_table_operations operation
      ON operation.id = permit.operation_id
    JOIN loyal_yield.lookup_table_families family ON family.id = operation.family_id
    WHERE family.cluster <> permit.cluster
       OR permit.fencing_token > operation.fencing_token
       OR permit.control_epoch < 0
       OR (
           permit.resolved_at IS NULL
           AND (
               permit.permit_state <> 'granted'
               OR operation.operation_state <> 'signed'
               OR permit.transaction_signature <> operation.transaction_signature
               OR permit.message_hash <> operation.message_hash
           )
       )
       OR (permit.resolved_at IS NOT NULL AND permit.permit_state = 'granted');

    IF invalid_broadcast_permits <> 0 THEN
        RAISE EXCEPTION 'invalid durable broadcast permit row(s): %',
            invalid_broadcast_permits;
    END IF;

    SELECT count(*)
    INTO invalid_precutover_probes
    FROM loyal_yield.lookup_table_precutover_probe_runs probe
    LEFT JOIN loyal_yield.lookup_table_shared_market_catalog_revisions revision
      ON revision.id = probe.catalog_revision_id
    LEFT JOIN loyal_yield.lookup_table_manifests manifest
      ON manifest.id = probe.shared_manifest_id
    WHERE revision.id IS NULL
       OR manifest.id IS NULL
       OR revision.manifest_id <> probe.shared_manifest_id
       OR manifest.family_id <> revision.family_id
       OR probe.provisioner_control_epoch < 0
       OR probe.result <> 'pass'
       OR probe.shared_table_bundle_hash !~ '^[0-9a-f]{64}$'
       OR probe.shared_table_bundle_hash IS DISTINCT FROM (
           SELECT loyal_yield.hash_length_prefixed_text(
               ARRAY['loyal-reusable-shared-table-bundle-v1']::TEXT[]
               || COALESCE(
                   array_agg(
                       bundle_field.field_value
                       ORDER BY shared.shard_ordinal,
                                bundle_field.field_ordinal
                   ),
                   ARRAY[]::TEXT[]
               )
           )
           FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
           CROSS JOIN LATERAL (
               VALUES
                   (0, shared.route_lookup_table_id::TEXT),
                   (1, shared.shard_ordinal::TEXT),
                   (2, shared.shared_table_address),
                   (3, shared.shared_authority),
                   (4, shared.shared_mutation_epoch::TEXT),
                   (5, shared.finalized_last_extended_slot::TEXT),
                   (6, shared.finalized_address_hash),
                   (7, shared.finalized_address_count::TEXT)
           ) AS bundle_field(field_ordinal, field_value)
           WHERE shared.probe_run_id = probe.id
       )
       OR probe.shared_table_count <> (
           SELECT count(*)
           FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
           WHERE shared.probe_run_id = probe.id
       )
       OR probe.finalized_bundle_address_count <> revision.address_count
       OR probe.finalized_bundle_address_count <> (
           SELECT COALESCE(sum(shared.finalized_address_count), 0)
           FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
           WHERE shared.probe_run_id = probe.id
       )
       OR NOT EXISTS (
           SELECT 1
           FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
           WHERE shared.probe_run_id = probe.id
             AND shared.route_lookup_table_id = probe.route_lookup_table_id
             AND shared.shared_table_address = probe.shared_table_address
             AND shared.shared_authority = probe.shared_authority
             AND shared.shared_mutation_epoch = probe.shared_mutation_epoch
             AND shared.finalized_slot = probe.finalized_slot
             AND shared.finalized_last_extended_slot = probe.finalized_last_extended_slot
             AND shared.finalized_address_hash = probe.finalized_address_hash
             AND shared.finalized_address_count = probe.finalized_address_count
       )
       OR EXISTS (
           SELECT 1
           FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
           LEFT JOIN loyal_yield.route_lookup_tables route_table
             ON route_table.id = shared.route_lookup_table_id
           JOIN loyal_yield.lookup_table_families family
             ON family.id = revision.family_id
           WHERE shared.probe_run_id = probe.id
             AND (
                 shared.shard_ordinal < 0
                 OR shared.finalized_slot <> probe.finalized_slot
                 OR shared.finalized_slot <= shared.finalized_last_extended_slot
                 OR shared.finalized_address_hash !~ '^[0-9a-f]{64}$'
                 OR shared.finalized_address_count NOT BETWEEN 1 AND 256
                 OR route_table.id IS NULL
                 OR family.cluster <> probe.cluster
                 OR route_table.cluster <> probe.cluster
                 OR route_table.family_id <> revision.family_id
                 OR route_table.allocation_kind <> 'shared_market'
                 OR route_table.shard_ordinal <> shared.shard_ordinal
                 OR route_table.table_address <> shared.shared_table_address
                 OR route_table.authority <> shared.shared_authority
                 OR route_table.mutation_epoch < shared.shared_mutation_epoch
                 OR route_table.address_count < shared.finalized_address_count
                 OR shared.finalized_address_count > family.allocation_high_water
             )
       )
       OR EXISTS (
           WITH expected AS (
               SELECT (
                          address.ordinal / family.allocation_high_water
                      )::INTEGER AS shard_ordinal,
                      count(*)::INTEGER AS finalized_address_count,
                      loyal_yield.hash_length_prefixed_text(
                          array_agg(address.address ORDER BY address.ordinal)
                      ) AS finalized_address_hash
               FROM loyal_yield.lookup_table_manifest_addresses address
               JOIN loyal_yield.lookup_table_families family
                 ON family.id = manifest.family_id
               WHERE address.manifest_id = probe.shared_manifest_id
                 AND address.semantic_class = 'shared_market'
               GROUP BY (
                   address.ordinal / family.allocation_high_water
               )::INTEGER
           ), observed AS (
               SELECT shared.shard_ordinal,
                      shared.finalized_address_count,
                      shared.finalized_address_hash
               FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
               WHERE shared.probe_run_id = probe.id
           )
           SELECT 1
           FROM expected
           FULL JOIN observed
             USING (shard_ordinal)
           WHERE expected.finalized_address_count
                     IS DISTINCT FROM observed.finalized_address_count
              OR expected.finalized_address_hash
                     IS DISTINCT FROM observed.finalized_address_hash
       );

    IF invalid_precutover_probes <> 0 THEN
        RAISE EXCEPTION 'invalid immutable pre-cutover probe row(s): %',
            invalid_precutover_probes;
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
       OR (request_status = 'satisfied' AND satisfied_at IS NULL)
       OR (error_code IS NOT NULL AND length(btrim(error_code)) = 0)
       OR (error_detail IS NOT NULL AND length(btrim(error_detail)) = 0)
       OR (
           request_status = 'failed'
           AND (NULLIF(btrim(error_code), '') IS NULL OR NULLIF(btrim(error_detail), '') IS NULL)
       );

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
    'verifiedLegacyTables', (
        SELECT count(*)
        FROM loyal_yield.route_lookup_tables
        WHERE family_id IS NULL AND legacy_import_run_id IS NOT NULL
    ),
    'legacyImportRuns', (
        SELECT count(*) FROM loyal_yield.lookup_table_legacy_import_runs
    ),
    'sharedMarketCatalogHeads', (
        SELECT count(*) FROM loyal_yield.lookup_table_shared_market_catalog_heads
    ),
    'sharedMarketCatalog', (
        SELECT json_build_object(
            'heads', count(*),
            'pending', count(*) FILTER (WHERE readiness_state = 'pending'),
            'provisioning', count(*) FILTER (WHERE readiness_state = 'provisioning'),
            'active', count(*) FILTER (WHERE readiness_state = 'active'),
            'failed', count(*) FILTER (WHERE readiness_state = 'failed'),
            'revisionsWithReason', (
                SELECT count(*)
                FROM loyal_yield.lookup_table_shared_market_catalog_revisions
                WHERE NULLIF(btrim(reason), '') IS NOT NULL
            )
        )
        FROM loyal_yield.lookup_table_shared_market_catalog_heads
    ),
    'sharedPhysicalDrift', (
        SELECT json_build_object(
            'total', count(*),
            'open', count(*) FILTER (WHERE resolution_state = 'open'),
            'resolved', count(*) FILTER (WHERE resolution_state = 'resolved')
        )
        FROM loyal_yield.lookup_table_shared_market_physical_drifts
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
    'provisioningRequests', (
        SELECT json_build_object(
            'total', count(*),
            'requested', count(*) FILTER (WHERE request_status = 'requested'),
            'planning', count(*) FILTER (WHERE request_status = 'planning'),
            'queued', count(*) FILTER (WHERE request_status = 'queued'),
            'satisfied', count(*) FILTER (WHERE request_status = 'satisfied'),
            'failed', count(*) FILTER (WHERE request_status = 'failed'),
            'cancelled', count(*) FILTER (WHERE request_status = 'cancelled'),
            'failedWithReason', count(*) FILTER (
                WHERE request_status = 'failed'
                  AND NULLIF(btrim(error_code), '') IS NOT NULL
                  AND NULLIF(btrim(error_detail), '') IS NOT NULL
            )
        )
        FROM loyal_yield.lookup_table_provisioning_requests
    ),
    'pendingOperations', (
        SELECT count(*)
        FROM loyal_yield.lookup_table_operations
        WHERE operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
    ),
    'clusterBudgetReservations', (
        SELECT json_build_object(
            'total', count(*),
            'active', count(*) FILTER (WHERE reserved_until > now()),
            'activeReservedLamports', COALESCE(
                sum(reserved_lamports) FILTER (WHERE reserved_until > now()),
                0
            )
        )
        FROM loyal_yield.lookup_table_cluster_budget_reservations
    ),
    'legacyCleanupBudgetReservations', (
        SELECT json_build_object(
            'total', count(*),
            'active', count(*) FILTER (WHERE reserved_until > now()),
            'activeReservedLamports', COALESCE(
                sum(reserved_lamports) FILTER (WHERE reserved_until > now()),
                0
            )
        )
        FROM loyal_yield.lookup_table_legacy_cleanup_budget_reservations
    ),
    'broadcastPermits', (
        SELECT json_build_object(
            'total', count(*),
            'active', count(*) FILTER (WHERE resolved_at IS NULL)
        )
        FROM loyal_yield.lookup_table_provisioner_broadcast_permits
    ),
    'precutoverProbeRuns', (
        SELECT count(*) FROM loyal_yield.lookup_table_precutover_probe_runs
    ),
    'precutoverProbeSharedTables', (
        SELECT count(*)
        FROM loyal_yield.lookup_table_precutover_probe_shared_tables
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
