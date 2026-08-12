-- Reusable Address Lookup Table control plane.
--
-- This migration is additive. Existing exact-route lookup-table rows remain
-- valid and continue to be resolved through the migration-0008 columns.

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_families (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    logical_name TEXT NOT NULL,
    kind TEXT NOT NULL,
    desired_state TEXT NOT NULL DEFAULT 'active',
    planner_version TEXT NOT NULL,
    catalog_version TEXT NOT NULL,
    active_generation INTEGER,
    previous_generation INTEGER,
    provisioning_authority TEXT NOT NULL,
    payer TEXT NOT NULL,
    hard_capacity INTEGER NOT NULL DEFAULT 256,
    largest_atomic_expansion INTEGER NOT NULL,
    safety_margin INTEGER NOT NULL,
    allocation_high_water INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_families_cluster_logical_name_unique
        UNIQUE (cluster, logical_name),
    CONSTRAINT lookup_table_families_kind_check
        CHECK (kind IN ('shared_market', 'vault_shards')),
    CONSTRAINT lookup_table_families_desired_state_check
        CHECK (desired_state IN ('active', 'paused', 'retiring', 'retired')),
    CONSTRAINT lookup_table_families_generation_check
        CHECK (
            (active_generation IS NULL OR active_generation >= 0)
            AND (previous_generation IS NULL OR previous_generation >= 0)
            AND (
                active_generation IS NULL
                OR previous_generation IS NULL
                OR active_generation <> previous_generation
            )
        ),
    CONSTRAINT lookup_table_families_capacity_check CHECK (
        hard_capacity BETWEEN 1 AND 256
        AND largest_atomic_expansion > 0
        AND safety_margin > 0
        AND largest_atomic_expansion + safety_margin < hard_capacity
        AND allocation_high_water =
            hard_capacity - largest_atomic_expansion - safety_margin
    )
);

ALTER TABLE loyal_yield.lookup_table_families
    ADD COLUMN IF NOT EXISTS rollback_until TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS largest_atomic_expansion INTEGER,
    ADD COLUMN IF NOT EXISTS safety_margin INTEGER;

-- Upgrade rows created by an earlier draft without changing their durable
-- high-water mark. A high-water equal to hard capacity was never safe for an
-- atomic expansion and intentionally fails the constraint below.
UPDATE loyal_yield.lookup_table_families
SET safety_margin = COALESCE(safety_margin, 1),
    largest_atomic_expansion = COALESCE(
        largest_atomic_expansion,
        hard_capacity - allocation_high_water - 1
    )
WHERE largest_atomic_expansion IS NULL OR safety_margin IS NULL;

ALTER TABLE loyal_yield.lookup_table_families
    ALTER COLUMN largest_atomic_expansion SET NOT NULL,
    ALTER COLUMN safety_margin SET NOT NULL,
    DROP CONSTRAINT IF EXISTS lookup_table_families_capacity_check,
    ADD CONSTRAINT lookup_table_families_capacity_check CHECK (
        hard_capacity BETWEEN 1 AND 256
        AND largest_atomic_expansion > 0
        AND safety_margin > 0
        AND largest_atomic_expansion + safety_margin < hard_capacity
        AND allocation_high_water =
            hard_capacity - largest_atomic_expansion - safety_margin
    );

-- A generation is the rollover unit inside a family. Having two active
-- families of the same semantic kind would make manifest resolution depend on
-- row ordering instead of durable desired state.
CREATE UNIQUE INDEX IF NOT EXISTS lookup_table_families_one_active_kind_idx
    ON loyal_yield.lookup_table_families (cluster, kind)
    WHERE desired_state = 'active';

ALTER TABLE loyal_yield.route_lookup_tables
    ADD COLUMN IF NOT EXISTS family_id BIGINT,
    ADD COLUMN IF NOT EXISTS allocation_kind TEXT,
    ADD COLUMN IF NOT EXISTS generation INTEGER,
    ADD COLUMN IF NOT EXISTS shard_ordinal INTEGER,
    ADD COLUMN IF NOT EXISTS desired_state TEXT,
    ADD COLUMN IF NOT EXISTS accepting_allocations BOOLEAN,
    ADD COLUMN IF NOT EXISTS allocation_high_water INTEGER,
    ADD COLUMN IF NOT EXISTS reserved_address_count INTEGER,
    ADD COLUMN IF NOT EXISTS usable_address_count INTEGER,
    ADD COLUMN IF NOT EXISTS last_extended_start_index INTEGER,
    ADD COLUMN IF NOT EXISTS last_verified_slot BIGINT,
    ADD COLUMN IF NOT EXISTS last_verified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS mutation_epoch BIGINT,
    ADD COLUMN IF NOT EXISTS rollback_until TIMESTAMPTZ;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.route_lookup_tables'::regclass
          AND conname = 'route_lookup_tables_family_id_fkey'
    ) THEN
        ALTER TABLE loyal_yield.route_lookup_tables
            ADD CONSTRAINT route_lookup_tables_family_id_fkey
            FOREIGN KEY (family_id)
            REFERENCES loyal_yield.lookup_table_families(id);
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.route_lookup_tables'::regclass
          AND conname = 'route_lookup_tables_allocation_kind_check'
    ) THEN
        ALTER TABLE loyal_yield.route_lookup_tables
            ADD CONSTRAINT route_lookup_tables_allocation_kind_check
            CHECK (
                allocation_kind IS NULL
                OR allocation_kind IN ('shared_market', 'vault_shard', 'dedicated_vault')
            );
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.route_lookup_tables'::regclass
          AND conname = 'route_lookup_tables_desired_state_check'
    ) THEN
        ALTER TABLE loyal_yield.route_lookup_tables
            ADD CONSTRAINT route_lookup_tables_desired_state_check
            CHECK (
                desired_state IS NULL
                OR desired_state IN (
                    'preparing',
                    'warming',
                    'active',
                    'standby',
                    'retiring',
                    'deactivated',
                    'closed',
                    'failed'
                )
            );
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.route_lookup_tables'::regclass
          AND conname = 'route_lookup_tables_v2_capacity_check'
    ) THEN
        ALTER TABLE loyal_yield.route_lookup_tables
            ADD CONSTRAINT route_lookup_tables_v2_capacity_check
            CHECK (
                (allocation_high_water IS NULL OR allocation_high_water BETWEEN 1 AND 256)
                AND (reserved_address_count IS NULL OR reserved_address_count BETWEEN 0 AND 256)
                AND (usable_address_count IS NULL OR usable_address_count BETWEEN 0 AND 256)
                AND (
                    allocation_high_water IS NULL
                    OR reserved_address_count IS NULL
                    OR reserved_address_count <= allocation_high_water
                )
                AND (
                    usable_address_count IS NULL
                    OR usable_address_count <= address_count
                )
                AND (
                    last_extended_start_index IS NULL
                    OR last_extended_start_index BETWEEN 0 AND 255
                )
            );
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.route_lookup_tables'::regclass
          AND conname = 'route_lookup_tables_v2_metadata_check'
    ) THEN
        ALTER TABLE loyal_yield.route_lookup_tables
            ADD CONSTRAINT route_lookup_tables_v2_metadata_check
            CHECK (
                (
                    family_id IS NULL
                    AND allocation_kind IS NULL
                    AND generation IS NULL
                    AND shard_ordinal IS NULL
                    AND desired_state IS NULL
                    AND accepting_allocations IS NULL
                    AND allocation_high_water IS NULL
                    AND reserved_address_count IS NULL
                    AND usable_address_count IS NULL
                    AND mutation_epoch IS NULL
                )
                OR (
                    family_id IS NOT NULL
                    AND allocation_kind IS NOT NULL
                    AND generation IS NOT NULL
                    AND generation >= 0
                    AND shard_ordinal IS NOT NULL
                    AND shard_ordinal >= 0
                    AND desired_state IS NOT NULL
                    AND accepting_allocations IS NOT NULL
                    AND allocation_high_water IS NOT NULL
                    AND reserved_address_count IS NOT NULL
                    AND usable_address_count IS NOT NULL
                    AND mutation_epoch IS NOT NULL
                    AND mutation_epoch >= 0
                )
            );
    END IF;
END;
$$;

CREATE UNIQUE INDEX IF NOT EXISTS route_lookup_tables_unique_family_generation_shard_idx
    ON loyal_yield.route_lookup_tables (family_id, generation, shard_ordinal)
    WHERE family_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS route_lookup_tables_family_lifecycle_idx
    ON loyal_yield.route_lookup_tables (
        family_id,
        desired_state,
        accepting_allocations,
        generation,
        shard_ordinal
    )
    WHERE family_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_manifests (
    id BIGSERIAL PRIMARY KEY,
    family_id BIGINT NOT NULL REFERENCES loyal_yield.lookup_table_families(id),
    subject_kind TEXT NOT NULL,
    subject_key TEXT NOT NULL,
    vault_id BIGINT REFERENCES loyal_yield.managed_vaults(id),
    desired_set_hash TEXT NOT NULL,
    address_count INTEGER NOT NULL,
    source_slot BIGINT,
    planner_version TEXT NOT NULL,
    catalog_version TEXT NOT NULL,
    sealed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_manifests_identity_unique
        UNIQUE (family_id, subject_kind, subject_key, desired_set_hash),
    CONSTRAINT lookup_table_manifests_subject_kind_check
        CHECK (subject_kind IN ('shared_market', 'vault')),
    CONSTRAINT lookup_table_manifests_subject_vault_check
        CHECK (
            (subject_kind = 'shared_market' AND vault_id IS NULL)
            OR (subject_kind = 'vault' AND vault_id IS NOT NULL)
        ),
    CONSTRAINT lookup_table_manifests_address_count_check
        CHECK (address_count >= 0),
    CONSTRAINT lookup_table_manifests_source_slot_check
        CHECK (source_slot IS NULL OR source_slot >= 0)
);

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_manifest_addresses (
    manifest_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_manifests(id),
    address TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    semantic_class TEXT NOT NULL,
    account_role TEXT NOT NULL,
    is_writable BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (manifest_id, address),
    CONSTRAINT lookup_table_manifest_addresses_manifest_ordinal_unique
        UNIQUE (manifest_id, ordinal),
    CONSTRAINT lookup_table_manifest_addresses_ordinal_check
        CHECK (ordinal >= 0),
    CONSTRAINT lookup_table_manifest_addresses_semantic_class_check
        CHECK (semantic_class IN ('shared_market', 'vault'))
);

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_manifest_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'lookup-table manifests are immutable';
    END IF;

    IF OLD.sealed_at IS NOT NULL THEN
        RAISE EXCEPTION 'sealed lookup-table manifests are immutable';
    END IF;

    IF NEW.family_id IS DISTINCT FROM OLD.family_id
        OR NEW.subject_kind IS DISTINCT FROM OLD.subject_kind
        OR NEW.subject_key IS DISTINCT FROM OLD.subject_key
        OR NEW.vault_id IS DISTINCT FROM OLD.vault_id
        OR NEW.desired_set_hash IS DISTINCT FROM OLD.desired_set_hash
        OR NEW.address_count IS DISTINCT FROM OLD.address_count
        OR NEW.source_slot IS DISTINCT FROM OLD.source_slot
        OR NEW.planner_version IS DISTINCT FROM OLD.planner_version
        OR NEW.catalog_version IS DISTINCT FROM OLD.catalog_version
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'lookup-table manifest content is immutable';
    END IF;

    IF NEW.sealed_at IS NULL THEN
        RAISE EXCEPTION 'lookup-table manifest updates may only seal the manifest';
    END IF;

    IF (
        SELECT count(*)
        FROM loyal_yield.lookup_table_manifest_addresses
        WHERE manifest_id = OLD.id
    ) <> NEW.address_count THEN
        RAISE EXCEPTION 'lookup-table manifest address count does not match its rows';
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_manifests_immutable
    ON loyal_yield.lookup_table_manifests;
CREATE TRIGGER lookup_table_manifests_immutable
    BEFORE UPDATE OR DELETE ON loyal_yield.lookup_table_manifests
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_manifest_update();

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_manifest_address_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    manifest_sealed_at TIMESTAMPTZ;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        SELECT sealed_at
        INTO manifest_sealed_at
        FROM loyal_yield.lookup_table_manifests
        WHERE id = OLD.manifest_id
        FOR SHARE;

        IF manifest_sealed_at IS NOT NULL THEN
            RAISE EXCEPTION 'sealed lookup-table manifest addresses are immutable';
        END IF;
    END IF;

    IF TG_OP <> 'DELETE' THEN
        SELECT sealed_at
        INTO manifest_sealed_at
        FROM loyal_yield.lookup_table_manifests
        WHERE id = NEW.manifest_id
        FOR SHARE;

        IF manifest_sealed_at IS NOT NULL THEN
            RAISE EXCEPTION 'sealed lookup-table manifest addresses are immutable';
        END IF;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_manifest_addresses_immutable
    ON loyal_yield.lookup_table_manifest_addresses;
CREATE TRIGGER lookup_table_manifest_addresses_immutable
    BEFORE INSERT OR UPDATE OR DELETE
    ON loyal_yield.lookup_table_manifest_addresses
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_manifest_address_mutation();

-- The immutable manifest id is not itself a desired-state revision: a cohort
-- cancellation may intentionally make an older aggregate hash current again.
-- This pointer gives every vault head an explicit monotonic revision that a
-- binding must match before it may warm or activate.
CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_vault_desired_heads (
    family_id BIGINT NOT NULL REFERENCES loyal_yield.lookup_table_families(id),
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id),
    binding_ordinal INTEGER NOT NULL DEFAULT 0,
    manifest_id BIGINT NOT NULL REFERENCES loyal_yield.lookup_table_manifests(id),
    desired_revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (family_id, vault_id, binding_ordinal),
    CONSTRAINT lookup_table_vault_desired_heads_ordinal_check
        CHECK (binding_ordinal >= 0),
    CONSTRAINT lookup_table_vault_desired_heads_revision_check
        CHECK (desired_revision > 0)
);

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_vault_bindings (
    id BIGSERIAL PRIMARY KEY,
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id),
    family_id BIGINT NOT NULL REFERENCES loyal_yield.lookup_table_families(id),
    route_lookup_table_id BIGINT NOT NULL
        REFERENCES loyal_yield.route_lookup_tables(id),
    manifest_id BIGINT NOT NULL REFERENCES loyal_yield.lookup_table_manifests(id),
    binding_ordinal INTEGER NOT NULL DEFAULT 0,
    desired_head_revision BIGINT NOT NULL,
    allocation_mode TEXT NOT NULL,
    reserved_capacity INTEGER NOT NULL,
    predecessor_binding_id BIGINT
        REFERENCES loyal_yield.lookup_table_vault_bindings(id),
    lifecycle_state TEXT NOT NULL DEFAULT 'preparing',
    active_from_slot BIGINT,
    active_until_slot BIGINT,
    activated_at TIMESTAMPTZ,
    deactivated_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_vault_bindings_allocation_mode_check
        CHECK (allocation_mode IN ('packed_shard', 'dedicated')),
    CONSTRAINT lookup_table_vault_bindings_lifecycle_state_check
        CHECK (
            lifecycle_state IN (
                'preparing',
                'warming',
                'active',
                'standby',
                'retiring',
                'retired',
                'failed'
            )
        ),
    CONSTRAINT lookup_table_vault_bindings_capacity_check
        CHECK (reserved_capacity BETWEEN 1 AND 256),
    CONSTRAINT lookup_table_vault_bindings_ordinal_check
        CHECK (binding_ordinal >= 0),
    CONSTRAINT lookup_table_vault_bindings_desired_revision_check
        CHECK (desired_head_revision > 0),
    CONSTRAINT lookup_table_vault_bindings_activation_interval_check
        CHECK (
            (active_from_slot IS NULL OR active_from_slot >= 0)
            AND (active_until_slot IS NULL OR active_until_slot >= 0)
            AND (
                active_from_slot IS NULL
                OR active_until_slot IS NULL
                OR active_until_slot >= active_from_slot
            )
            AND (
                activated_at IS NULL
                OR deactivated_at IS NULL
                OR deactivated_at >= activated_at
            )
        ),
    CONSTRAINT lookup_table_vault_bindings_predecessor_check
        CHECK (predecessor_binding_id IS NULL OR predecessor_binding_id <> id)
);

ALTER TABLE loyal_yield.lookup_table_vault_bindings
    ADD COLUMN IF NOT EXISTS rollback_until TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS desired_head_revision BIGINT;

-- Upgrade draft rows deterministically. The newest nonterminal binding becomes
-- the initial desired head; the explicit newest-binding activation fence below
-- prevents another revision-1 draft row from displacing it.
INSERT INTO loyal_yield.lookup_table_vault_desired_heads
    (family_id, vault_id, binding_ordinal, manifest_id, desired_revision)
SELECT DISTINCT ON (family_id, vault_id, binding_ordinal)
       family_id, vault_id, binding_ordinal, manifest_id, 1
FROM loyal_yield.lookup_table_vault_bindings
WHERE lifecycle_state IN ('preparing', 'warming', 'active')
ORDER BY family_id, vault_id, binding_ordinal, id DESC
ON CONFLICT (family_id, vault_id, binding_ordinal) DO NOTHING;

UPDATE loyal_yield.lookup_table_vault_bindings
SET desired_head_revision = 1
WHERE desired_head_revision IS NULL;

ALTER TABLE loyal_yield.lookup_table_vault_bindings
    ALTER COLUMN desired_head_revision SET NOT NULL,
    DROP CONSTRAINT IF EXISTS lookup_table_vault_bindings_desired_revision_check,
    ADD CONSTRAINT lookup_table_vault_bindings_desired_revision_check
        CHECK (desired_head_revision > 0);

CREATE UNIQUE INDEX IF NOT EXISTS lookup_table_vault_bindings_one_active_idx
    ON loyal_yield.lookup_table_vault_bindings (
        vault_id,
        family_id,
        binding_ordinal
    )
    WHERE lifecycle_state = 'active';

CREATE INDEX IF NOT EXISTS lookup_table_vault_bindings_table_state_idx
    ON loyal_yield.lookup_table_vault_bindings (
        route_lookup_table_id,
        lifecycle_state
    );

CREATE OR REPLACE FUNCTION loyal_yield.account_lookup_table_binding_reservation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    expected_allocation_kind TEXT;
    affected_table_id BIGINT;
    recomputed_reservation BIGINT;
BEGIN
    IF TG_OP <> 'DELETE' THEN
        expected_allocation_kind := CASE NEW.allocation_mode
            WHEN 'packed_shard' THEN 'vault_shard'
            WHEN 'dedicated' THEN 'dedicated_vault'
        END;

        IF NOT EXISTS (
            SELECT 1
            FROM loyal_yield.route_lookup_tables route_table
            WHERE route_table.id = NEW.route_lookup_table_id
              AND route_table.family_id = NEW.family_id
              AND route_table.allocation_kind = expected_allocation_kind
        ) THEN
            RAISE EXCEPTION 'vault binding does not match its physical table family/allocation';
        END IF;

        IF NOT EXISTS (
            SELECT 1
            FROM loyal_yield.lookup_table_manifests manifest
            WHERE manifest.id = NEW.manifest_id
              AND manifest.family_id = NEW.family_id
              AND manifest.vault_id = NEW.vault_id
              AND manifest.subject_kind = 'vault'
              AND manifest.sealed_at IS NOT NULL
              AND manifest.address_count <= NEW.reserved_capacity
        ) THEN
            RAISE EXCEPTION 'vault binding does not match its vault manifest';
        END IF;

        IF NOT EXISTS (
            SELECT 1
            FROM loyal_yield.lookup_table_families family
            WHERE family.id = NEW.family_id
              AND family.kind = 'vault_shards'
        ) THEN
            RAISE EXCEPTION 'vault binding family must be a vault-shards family';
        END IF;

        IF NEW.lifecycle_state IN ('preparing', 'warming')
           AND NOT EXISTS (
               SELECT 1
               FROM loyal_yield.lookup_table_vault_desired_heads desired
               WHERE desired.family_id = NEW.family_id
                 AND desired.vault_id = NEW.vault_id
                 AND desired.binding_ordinal = NEW.binding_ordinal
                 AND desired.manifest_id = NEW.manifest_id
                 AND desired.desired_revision = NEW.desired_head_revision
           ) THEN
            RAISE EXCEPTION 'vault binding is not the durable desired head revision';
        END IF;
    END IF;

    -- Recompute commitments from live heads rather than applying per-row
    -- deltas. Two manifest versions for the same vault/head may coexist on the
    -- same table during warmup; they consume the maximum promise, not the sum.
    -- A relocation still reserves both physical tables independently until the
    -- old head leaves its rollback window.
    FOR affected_table_id IN
        SELECT DISTINCT table_id
        FROM unnest(ARRAY[
            CASE WHEN TG_OP <> 'INSERT' THEN OLD.route_lookup_table_id END,
            CASE WHEN TG_OP <> 'DELETE' THEN NEW.route_lookup_table_id END
        ]) AS affected(table_id)
        WHERE table_id IS NOT NULL
    LOOP
        SELECT COALESCE(sum(head.reserved_capacity), 0)
        INTO recomputed_reservation
        FROM (
            SELECT max(binding.reserved_capacity) AS reserved_capacity
            FROM loyal_yield.lookup_table_vault_bindings binding
            WHERE binding.route_lookup_table_id = affected_table_id
              AND binding.lifecycle_state IN (
                  'preparing', 'warming', 'active', 'standby', 'retiring'
              )
            GROUP BY binding.vault_id, binding.family_id, binding.binding_ordinal
        ) head;

        UPDATE loyal_yield.route_lookup_tables
        SET reserved_address_count = recomputed_reservation::INTEGER,
            updated_at = now()
        WHERE id = affected_table_id
          AND recomputed_reservation <= allocation_high_water
          AND recomputed_reservation <= 256;

        IF NOT FOUND THEN
            RAISE EXCEPTION 'lookup-table allocation capacity exceeded';
        END IF;
    END LOOP;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_vault_bindings_reservation_accounting
    ON loyal_yield.lookup_table_vault_bindings;
CREATE TRIGGER lookup_table_vault_bindings_reservation_accounting
    AFTER INSERT OR UPDATE OR DELETE
    ON loyal_yield.lookup_table_vault_bindings
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.account_lookup_table_binding_reservation();

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_usage_leases (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    lease_kind TEXT NOT NULL,
    reference_key TEXT NOT NULL,
    route_lookup_table_id BIGINT NOT NULL
        REFERENCES loyal_yield.route_lookup_tables(id),
    vault_id BIGINT REFERENCES loyal_yield.managed_vaults(id),
    binding_id BIGINT REFERENCES loyal_yield.lookup_table_vault_bindings(id),
    route_fingerprint TEXT,
    requirements_fingerprint TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    released_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_usage_leases_reference_unique
        UNIQUE (lease_kind, reference_key, route_lookup_table_id),
    CONSTRAINT lookup_table_usage_leases_kind_check
        CHECK (lease_kind IN ('route_resolution', 'prepared_transaction')),
    CONSTRAINT lookup_table_usage_leases_interval_check
        CHECK (
            expires_at > created_at
            AND (released_at IS NULL OR released_at >= created_at)
        )
);

CREATE INDEX IF NOT EXISTS lookup_table_usage_leases_active_table_idx
    ON loyal_yield.lookup_table_usage_leases (
        route_lookup_table_id,
        expires_at,
        lease_kind
    )
    WHERE released_at IS NULL;

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_provisioning_requests (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id),
    route_fingerprint TEXT NOT NULL,
    requirements_fingerprint TEXT NOT NULL,
    shared_manifest_id BIGINT REFERENCES loyal_yield.lookup_table_manifests(id),
    vault_manifest_id BIGINT REFERENCES loyal_yield.lookup_table_manifests(id),
    desired_shared_hash TEXT,
    desired_vault_hash TEXT,
    desired_shared_address_count INTEGER NOT NULL DEFAULT 0,
    desired_vault_address_count INTEGER NOT NULL DEFAULT 0,
    sealed_at TIMESTAMPTZ,
    request_status TEXT NOT NULL DEFAULT 'requested',
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ,
    error_code TEXT,
    error_detail TEXT,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    satisfied_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_provisioning_requests_identity_unique
        UNIQUE (cluster, vault_id, requirements_fingerprint),
    CONSTRAINT lookup_table_provisioning_requests_status_check
        CHECK (
            request_status IN (
                'requested', 'planning', 'queued', 'satisfied', 'failed', 'cancelled'
            )
        ),
    CONSTRAINT lookup_table_provisioning_requests_lease_check
        CHECK (
            fencing_token >= 0
            AND attempt_count >= 0
            AND (
                request_status <> 'planning'
                OR (
                    lease_owner IS NOT NULL
                    AND lease_expires_at IS NOT NULL
                    AND sealed_at IS NOT NULL
                )
            )
            AND (
                request_status <> 'satisfied'
                OR satisfied_at IS NOT NULL
            )
        ),
    CONSTRAINT lookup_table_provisioning_requests_desired_check
        CHECK (
            (shared_manifest_id IS NOT NULL OR NULLIF(desired_shared_hash, '') IS NOT NULL)
            AND (vault_manifest_id IS NOT NULL OR NULLIF(desired_vault_hash, '') IS NOT NULL)
            AND desired_shared_address_count >= 0
            AND desired_vault_address_count >= 0
        )
);

CREATE INDEX IF NOT EXISTS lookup_table_provisioning_requests_work_queue_idx
    ON loyal_yield.lookup_table_provisioning_requests (
        request_status,
        next_attempt_at,
        lease_expires_at,
        requested_at
    )
    WHERE request_status IN ('requested', 'planning', 'queued', 'failed');

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_provisioning_request_addresses (
    request_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_provisioning_requests(id),
    address TEXT NOT NULL,
    semantic_class TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    account_role TEXT NOT NULL,
    is_writable BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id, address),
    CONSTRAINT lookup_table_request_addresses_class_ordinal_unique
        UNIQUE (request_id, semantic_class, ordinal),
    CONSTRAINT lookup_table_provisioning_request_addresses_class_check
        CHECK (semantic_class IN ('shared_market', 'vault')),
    CONSTRAINT lookup_table_provisioning_request_addresses_ordinal_check
        CHECK (ordinal >= 0)
);

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_provisioning_request_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    shared_address_count BIGINT;
    vault_address_count BIGINT;
BEGIN
    IF OLD.sealed_at IS NOT NULL AND (
        NEW.cluster IS DISTINCT FROM OLD.cluster
        OR NEW.vault_id IS DISTINCT FROM OLD.vault_id
        OR NEW.route_fingerprint IS DISTINCT FROM OLD.route_fingerprint
        OR NEW.requirements_fingerprint IS DISTINCT FROM OLD.requirements_fingerprint
        OR (OLD.shared_manifest_id IS NOT NULL
            AND NEW.shared_manifest_id IS DISTINCT FROM OLD.shared_manifest_id)
        OR NEW.desired_shared_hash IS DISTINCT FROM OLD.desired_shared_hash
        OR NEW.desired_vault_hash IS DISTINCT FROM OLD.desired_vault_hash
        OR NEW.desired_shared_address_count IS DISTINCT FROM OLD.desired_shared_address_count
        OR NEW.desired_vault_address_count IS DISTINCT FROM OLD.desired_vault_address_count
        OR NEW.sealed_at IS DISTINCT FROM OLD.sealed_at
    ) THEN
        RAISE EXCEPTION 'sealed lookup-table provisioning request content is immutable';
    END IF;

    -- Shared links are exact immutable route cohorts. Vault links are derived
    -- snapshots of the current per-vault aggregate and may advance when another
    -- cohort is added, cancelled, or reactivated; the immutable request rows
    -- remain the audit source of truth.
    IF OLD.sealed_at IS NOT NULL
       AND OLD.shared_manifest_id IS NULL
       AND NEW.shared_manifest_id IS NOT NULL
       AND NOT EXISTS (
           SELECT 1
           FROM loyal_yield.lookup_table_manifests manifest
           JOIN loyal_yield.lookup_table_families family
             ON family.id = manifest.family_id
           WHERE manifest.id = NEW.shared_manifest_id
             AND family.cluster = OLD.cluster
             AND family.kind = 'shared_market'
             AND manifest.subject_kind = 'shared_market'
             AND manifest.vault_id IS NULL
             AND manifest.desired_set_hash = OLD.desired_shared_hash
             AND manifest.address_count = OLD.desired_shared_address_count
             AND manifest.sealed_at IS NOT NULL
       ) THEN
        RAISE EXCEPTION 'attached shared manifest does not match sealed provisioning request';
    END IF;

    IF OLD.sealed_at IS NOT NULL
       AND NEW.vault_manifest_id IS DISTINCT FROM OLD.vault_manifest_id
       AND NEW.vault_manifest_id IS NOT NULL
       AND NOT EXISTS (
           SELECT 1
           FROM loyal_yield.lookup_table_manifests manifest
           JOIN loyal_yield.lookup_table_families family
             ON family.id = manifest.family_id
           WHERE manifest.id = NEW.vault_manifest_id
             AND family.cluster = OLD.cluster
             AND family.kind = 'vault_shards'
             AND manifest.subject_kind = 'vault'
             AND manifest.vault_id = OLD.vault_id
             AND manifest.sealed_at IS NOT NULL
             AND NOT EXISTS (
                 SELECT 1
                 FROM loyal_yield.lookup_table_provisioning_request_addresses request_address
                 WHERE request_address.request_id = OLD.id
                   AND request_address.semantic_class = 'vault'
                   AND NOT EXISTS (
                       SELECT 1
                       FROM loyal_yield.lookup_table_manifest_addresses manifest_address
                       WHERE manifest_address.manifest_id = manifest.id
                         AND manifest_address.semantic_class = 'vault'
                         AND manifest_address.address = request_address.address
                         AND (
                             NOT request_address.is_writable
                             OR manifest_address.is_writable
                         )
                         AND regexp_split_to_array(
                                 manifest_address.account_role,
                                 ','
                             ) @> regexp_split_to_array(
                                 request_address.account_role,
                                 ','
                             )
                   )
             )
       ) THEN
        RAISE EXCEPTION 'attached vault aggregate does not cover sealed provisioning request cohort';
    END IF;

    IF OLD.sealed_at IS NULL AND NEW.sealed_at IS NOT NULL THEN
        SELECT
            count(*) FILTER (WHERE semantic_class = 'shared_market'),
            count(*) FILTER (WHERE semantic_class = 'vault')
        INTO shared_address_count, vault_address_count
        FROM loyal_yield.lookup_table_provisioning_request_addresses
        WHERE request_id = OLD.id;

        IF NEW.shared_manifest_id IS NULL
           AND shared_address_count <> NEW.desired_shared_address_count THEN
            RAISE EXCEPTION 'shared provisioning-request address count mismatch';
        END IF;
        IF NEW.vault_manifest_id IS NULL
           AND vault_address_count <> NEW.desired_vault_address_count THEN
            RAISE EXCEPTION 'vault provisioning-request address count mismatch';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_provisioning_requests_immutable
    ON loyal_yield.lookup_table_provisioning_requests;
CREATE TRIGGER lookup_table_provisioning_requests_immutable
    BEFORE UPDATE ON loyal_yield.lookup_table_provisioning_requests
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_provisioning_request_update();

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_provisioning_request_address_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    request_sealed_at TIMESTAMPTZ;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        SELECT sealed_at
        INTO request_sealed_at
        FROM loyal_yield.lookup_table_provisioning_requests
        WHERE id = OLD.request_id
        FOR SHARE;

        IF request_sealed_at IS NOT NULL THEN
            RAISE EXCEPTION 'sealed lookup-table provisioning request addresses are immutable';
        END IF;
    END IF;

    IF TG_OP <> 'DELETE' THEN
        SELECT sealed_at
        INTO request_sealed_at
        FROM loyal_yield.lookup_table_provisioning_requests
        WHERE id = NEW.request_id
        FOR SHARE;

        IF request_sealed_at IS NOT NULL THEN
            RAISE EXCEPTION 'sealed lookup-table provisioning request addresses are immutable';
        END IF;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_provisioning_request_addresses_immutable
    ON loyal_yield.lookup_table_provisioning_request_addresses;
CREATE TRIGGER lookup_table_provisioning_request_addresses_immutable
    BEFORE INSERT OR UPDATE OR DELETE
    ON loyal_yield.lookup_table_provisioning_request_addresses
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_provisioning_request_address_mutation();

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_operations (
    id BIGSERIAL PRIMARY KEY,
    idempotency_key TEXT NOT NULL,
    family_id BIGINT NOT NULL REFERENCES loyal_yield.lookup_table_families(id),
    route_lookup_table_id BIGINT REFERENCES loyal_yield.route_lookup_tables(id),
    manifest_id BIGINT REFERENCES loyal_yield.lookup_table_manifests(id),
    binding_id BIGINT REFERENCES loyal_yield.lookup_table_vault_bindings(id),
    operation_kind TEXT NOT NULL,
    operation_state TEXT NOT NULL DEFAULT 'queued',
    target_generation INTEGER,
    target_shard_ordinal INTEGER,
    operation_context JSONB NOT NULL DEFAULT '{}'::jsonb,
    mutation_epoch BIGINT NOT NULL,
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    transaction_signature TEXT,
    message_hash TEXT,
    recent_blockhash TEXT,
    last_valid_block_height BIGINT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ,
    error_code TEXT,
    error_detail TEXT,
    submitted_slot BIGINT,
    submitted_at TIMESTAMPTZ,
    confirmed_slot BIGINT,
    confirmed_at TIMESTAMPTZ,
    finalized_slot BIGINT,
    finalized_at TIMESTAMPTZ,
    reconciled_slot BIGINT,
    reconciled_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_operations_idempotency_key_unique
        UNIQUE (idempotency_key),
    CONSTRAINT lookup_table_operations_kind_check
        CHECK (
            operation_kind IN (
                'create', 'extend', 'verify', 'rollover', 'deactivate', 'close'
            )
        ),
    CONSTRAINT lookup_table_operations_state_check
        CHECK (
            operation_state IN (
                'queued',
                'leased',
                'signed',
                'submitted',
                'confirmed',
                'finalized',
                'reconciled',
                'complete',
                'retry_wait',
                'needs_reconcile',
                'permanent_failure',
                'cancelled'
            )
        ),
    CONSTRAINT lookup_table_operations_target_check
        CHECK (
            (target_generation IS NULL OR target_generation >= 0)
            AND (target_shard_ordinal IS NULL OR target_shard_ordinal >= 0)
            AND (
                operation_kind NOT IN ('create', 'rollover')
                OR (
                    route_lookup_table_id IS NOT NULL
                    AND
                    target_generation IS NOT NULL
                    AND target_shard_ordinal IS NOT NULL
                )
            )
            AND (
                operation_kind NOT IN ('extend', 'verify', 'deactivate', 'close')
                OR route_lookup_table_id IS NOT NULL
            )
        ),
    CONSTRAINT lookup_table_operations_context_check
        CHECK (jsonb_typeof(operation_context) = 'object'),
    CONSTRAINT lookup_table_operations_lease_check
        CHECK (
            operation_state <> 'leased'
            OR (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
        ),
    CONSTRAINT lookup_table_operations_signed_metadata_check
        CHECK (
            operation_kind NOT IN ('create', 'extend', 'rollover', 'deactivate', 'close')
            OR operation_state NOT IN (
                'signed',
                'submitted',
                'confirmed',
                'finalized',
                'reconciled',
                'complete'
            )
            OR (
                transaction_signature IS NOT NULL
                AND message_hash IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
            )
        ),
    CONSTRAINT lookup_table_operations_counter_check
        CHECK (mutation_epoch >= 0 AND fencing_token >= 0 AND attempt_count >= 0),
    CONSTRAINT lookup_table_operations_slot_check
        CHECK (
            (last_valid_block_height IS NULL OR last_valid_block_height >= 0)
            AND (submitted_slot IS NULL OR submitted_slot >= 0)
            AND (confirmed_slot IS NULL OR confirmed_slot >= 0)
            AND (finalized_slot IS NULL OR finalized_slot >= 0)
            AND (reconciled_slot IS NULL OR reconciled_slot >= 0)
        )
);

ALTER TABLE loyal_yield.lookup_table_operations
    ADD COLUMN IF NOT EXISTS estimated_fee_lamports BIGINT,
    ADD COLUMN IF NOT EXISTS estimated_rent_lamports BIGINT,
    ADD COLUMN IF NOT EXISTS actual_fee_lamports BIGINT,
    ADD COLUMN IF NOT EXISTS actual_rent_lamports BIGINT,
    ADD COLUMN IF NOT EXISTS reclaimed_rent_lamports BIGINT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.lookup_table_operations'::regclass
          AND conname = 'lookup_table_operations_lamports_check'
    ) THEN
        ALTER TABLE loyal_yield.lookup_table_operations
            ADD CONSTRAINT lookup_table_operations_lamports_check
            CHECK (
                (estimated_fee_lamports IS NULL OR estimated_fee_lamports >= 0)
                AND (estimated_rent_lamports IS NULL OR estimated_rent_lamports >= 0)
                AND (actual_fee_lamports IS NULL OR actual_fee_lamports >= 0)
                AND (actual_rent_lamports IS NULL OR actual_rent_lamports >= 0)
                AND (reclaimed_rent_lamports IS NULL OR reclaimed_rent_lamports >= 0)
            );
    END IF;
END;
$$;

CREATE INDEX IF NOT EXISTS lookup_table_operations_work_queue_idx
    ON loyal_yield.lookup_table_operations (
        operation_state,
        next_attempt_at,
        lease_expires_at,
        created_at
    )
    WHERE operation_state IN ('queued', 'retry_wait', 'needs_reconcile', 'leased');

CREATE INDEX IF NOT EXISTS lookup_table_operations_table_state_idx
    ON loyal_yield.lookup_table_operations (route_lookup_table_id, operation_state);

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_operation_addresses (
    operation_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_operations(id),
    address TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (operation_id, address),
    CONSTRAINT lookup_table_operation_addresses_operation_ordinal_unique
        UNIQUE (operation_id, ordinal),
    CONSTRAINT lookup_table_operation_addresses_ordinal_check
        CHECK (ordinal >= 0)
);

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_addresses (
    route_lookup_table_id BIGINT NOT NULL
        REFERENCES loyal_yield.route_lookup_tables(id),
    address TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    added_operation_id BIGINT REFERENCES loyal_yield.lookup_table_operations(id),
    added_slot BIGINT NOT NULL,
    usable_after_slot BIGINT NOT NULL,
    last_verified_slot BIGINT NOT NULL,
    last_verified_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (route_lookup_table_id, address),
    CONSTRAINT lookup_table_addresses_table_ordinal_unique
        UNIQUE (route_lookup_table_id, ordinal),
    CONSTRAINT lookup_table_addresses_ordinal_check
        CHECK (ordinal BETWEEN 0 AND 255),
    CONSTRAINT lookup_table_addresses_slot_check
        CHECK (
            added_slot >= 0
            AND usable_after_slot >= added_slot
            AND last_verified_slot >= added_slot
        )
);

CREATE INDEX IF NOT EXISTS lookup_table_addresses_address_idx
    ON loyal_yield.lookup_table_addresses (address, route_lookup_table_id);

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_route_readiness_current (
    cluster TEXT NOT NULL,
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id),
    route_fingerprint TEXT NOT NULL,
    requirements_fingerprint TEXT NOT NULL,
    route_kind TEXT NOT NULL,
    source_reserve TEXT,
    target_reserve TEXT,
    manifest_id BIGINT REFERENCES loyal_yield.lookup_table_manifests(id),
    shared_family_id BIGINT REFERENCES loyal_yield.lookup_table_families(id),
    vault_binding_id BIGINT REFERENCES loyal_yield.lookup_table_vault_bindings(id),
    readiness_state TEXT NOT NULL,
    required_address_count INTEGER NOT NULL,
    covered_address_count INTEGER NOT NULL,
    missing_addresses JSONB NOT NULL DEFAULT '[]'::jsonb,
    legacy_table_ids BIGINT[] NOT NULL DEFAULT '{}'::bigint[],
    reusable_table_ids BIGINT[] NOT NULL DEFAULT '{}'::bigint[],
    compiled_message_size INTEGER,
    packet_limit INTEGER,
    observed_slot BIGINT,
    observed_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, vault_id, route_fingerprint, requirements_fingerprint),
    CONSTRAINT lookup_table_route_readiness_state_check
        CHECK (readiness_state IN ('unknown', 'incomplete', 'ready', 'failed')),
    CONSTRAINT lookup_table_route_readiness_coverage_check
        CHECK (
            required_address_count >= 0
            AND covered_address_count BETWEEN 0 AND required_address_count
            AND jsonb_typeof(missing_addresses) = 'array'
        ),
    CONSTRAINT lookup_table_route_readiness_packet_check
        CHECK (
            (compiled_message_size IS NULL OR compiled_message_size >= 0)
            AND (packet_limit IS NULL OR packet_limit > 0)
        ),
    CONSTRAINT lookup_table_route_readiness_slot_check
        CHECK (observed_slot IS NULL OR observed_slot >= 0)
);

ALTER TABLE loyal_yield.lookup_table_route_readiness_current
    ADD COLUMN IF NOT EXISTS selection_kind TEXT,
    ADD COLUMN IF NOT EXISTS fallback_reason TEXT,
    ADD COLUMN IF NOT EXISTS rollout_mode TEXT,
    ADD COLUMN IF NOT EXISTS selected_table_ids BIGINT[] NOT NULL DEFAULT '{}'::bigint[],
    ADD COLUMN IF NOT EXISTS selected_table_count INTEGER,
    ADD COLUMN IF NOT EXISTS packet_fits BOOLEAN,
    ADD COLUMN IF NOT EXISTS simulation_state TEXT,
    ADD COLUMN IF NOT EXISTS simulation_units_consumed BIGINT,
    ADD COLUMN IF NOT EXISTS simulation_error TEXT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.lookup_table_route_readiness_current'::regclass
          AND conname = 'lookup_table_route_readiness_selection_check'
    ) THEN
        ALTER TABLE loyal_yield.lookup_table_route_readiness_current
            ADD CONSTRAINT lookup_table_route_readiness_selection_check
            CHECK (
                (selection_kind IS NULL OR selection_kind IN ('legacy', 'reusable', 'blocked'))
                AND (
                    rollout_mode IS NULL
                    OR rollout_mode IN ('legacy', 'shadow', 'prefer_reusable', 'reusable_only')
                )
                AND (selected_table_count IS NULL OR selected_table_count >= 0)
                AND (
                    selected_table_count IS NULL
                    OR selected_table_count = cardinality(selected_table_ids)
                )
            );
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.lookup_table_route_readiness_current'::regclass
          AND conname = 'lookup_table_route_readiness_simulation_check'
    ) THEN
        ALTER TABLE loyal_yield.lookup_table_route_readiness_current
            ADD CONSTRAINT lookup_table_route_readiness_simulation_check
            CHECK (
                (
                    simulation_state IS NULL
                    OR simulation_state IN ('not_run', 'succeeded', 'failed')
                )
                AND (
                    simulation_units_consumed IS NULL
                    OR simulation_units_consumed >= 0
                )
                AND (
                    packet_fits IS NULL
                    OR compiled_message_size IS NULL
                    OR packet_limit IS NULL
                    OR packet_fits = (compiled_message_size <= packet_limit)
                )
            );
    END IF;
END;
$$;

CREATE INDEX IF NOT EXISTS lookup_table_route_readiness_state_idx
    ON loyal_yield.lookup_table_route_readiness_current (
        cluster,
        readiness_state,
        updated_at DESC
    );

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_rollout_controls (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    vault_id BIGINT REFERENCES loyal_yield.managed_vaults(id),
    rollout_mode TEXT NOT NULL DEFAULT 'legacy',
    force_legacy BOOLEAN NOT NULL DEFAULT FALSE,
    reason TEXT,
    updated_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_rollout_controls_mode_check
        CHECK (rollout_mode IN ('legacy', 'shadow', 'prefer_reusable', 'reusable_only'))
);

CREATE UNIQUE INDEX IF NOT EXISTS lookup_table_rollout_controls_global_idx
    ON loyal_yield.lookup_table_rollout_controls (cluster)
    WHERE vault_id IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS lookup_table_rollout_controls_vault_idx
    ON loyal_yield.lookup_table_rollout_controls (cluster, vault_id)
    WHERE vault_id IS NOT NULL;
