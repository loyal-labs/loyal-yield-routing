-- Demand-driven reusable ALT shared-market catalog.
--
-- The stable shared-market universe is a vault-independent desired-state
-- head. Route requests may consume subsets of this catalog, but may never
-- grow it implicitly. Vault manifests remain demand-driven.

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_shared_market_catalog_revisions (
    id BIGSERIAL PRIMARY KEY,
    family_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_families(id),
    manifest_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_manifests(id),
    catalog_revision BIGINT NOT NULL,
    catalog_version TEXT NOT NULL,
    desired_set_hash TEXT NOT NULL,
    enabled_mints_hash TEXT NOT NULL,
    reserve_set_hash TEXT NOT NULL,
    address_count INTEGER NOT NULL,
    source_slot BIGINT,
    source_observed_at TIMESTAMPTZ,
    source_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    reason TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_shared_catalog_revision_unique
        UNIQUE (family_id, catalog_revision),
    CONSTRAINT lookup_table_shared_catalog_revision_check
        CHECK (catalog_revision > 0),
    CONSTRAINT lookup_table_shared_catalog_address_count_check
        CHECK (address_count BETWEEN 1 AND 256),
    CONSTRAINT lookup_table_shared_catalog_source_slot_check
        CHECK (source_slot IS NULL OR source_slot >= 0),
    CONSTRAINT lookup_table_shared_catalog_metadata_check
        CHECK (
            jsonb_typeof(source_metadata) = 'object'
            AND length(btrim(catalog_version)) > 0
            AND desired_set_hash ~ '^[0-9a-f]{64}$'
            AND enabled_mints_hash ~ '^[0-9a-f]{64}$'
            AND reserve_set_hash ~ '^[0-9a-f]{64}$'
            AND length(btrim(reason)) > 0
            AND length(btrim(updated_by)) > 0
        )
);

-- A monotonic catalog head may intentionally roll back to a previously
-- published immutable manifest. Revision identity remains unique while the
-- same sealed manifest can therefore appear in multiple audit revisions.
ALTER TABLE loyal_yield.lookup_table_shared_market_catalog_revisions
    DROP CONSTRAINT IF EXISTS lookup_table_shared_catalog_manifest_unique;

CREATE INDEX IF NOT EXISTS lookup_table_shared_catalog_manifest_idx
    ON loyal_yield.lookup_table_shared_market_catalog_revisions (manifest_id);

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_shared_market_catalog_heads (
    family_id BIGINT PRIMARY KEY
        REFERENCES loyal_yield.lookup_table_families(id),
    catalog_revision_id BIGINT NOT NULL UNIQUE
        REFERENCES loyal_yield.lookup_table_shared_market_catalog_revisions(id),
    target_generation INTEGER,
    readiness_state TEXT NOT NULL DEFAULT 'pending',
    activated_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_shared_catalog_head_generation_check
        CHECK (target_generation IS NULL OR target_generation >= 0),
    CONSTRAINT lookup_table_shared_catalog_head_readiness_check
        CHECK (readiness_state IN ('pending', 'provisioning', 'active', 'failed')),
    CONSTRAINT lookup_table_shared_catalog_head_lifecycle_check
        CHECK (
            (readiness_state = 'pending' AND target_generation IS NULL AND activated_at IS NULL)
            OR (readiness_state = 'provisioning' AND target_generation IS NOT NULL AND activated_at IS NULL)
            OR (readiness_state = 'active' AND target_generation IS NOT NULL AND activated_at IS NOT NULL)
            OR (readiness_state = 'failed' AND target_generation IS NOT NULL)
        )
);

CREATE INDEX IF NOT EXISTS lookup_table_shared_catalog_revision_family_idx
    ON loyal_yield.lookup_table_shared_market_catalog_revisions
        (family_id, catalog_revision DESC);

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_shared_catalog_revision_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM loyal_yield.lookup_table_families family
        JOIN loyal_yield.lookup_table_manifests manifest
          ON manifest.id = NEW.manifest_id
        WHERE family.id = NEW.family_id
          AND family.kind = 'shared_market'
          AND family.desired_state = 'active'
          AND family.catalog_version = NEW.catalog_version
          AND manifest.family_id = family.id
          AND manifest.subject_kind = 'shared_market'
          AND manifest.vault_id IS NULL
          AND manifest.sealed_at IS NOT NULL
          AND manifest.catalog_version = NEW.catalog_version
          AND manifest.desired_set_hash = NEW.desired_set_hash
          AND manifest.address_count = NEW.address_count
          AND manifest.address_count <= family.allocation_high_water
          AND manifest.source_slot IS NOT DISTINCT FROM NEW.source_slot
          AND manifest.address_count = (
              SELECT count(*)
              FROM loyal_yield.lookup_table_manifest_addresses address
              WHERE address.manifest_id = manifest.id
                AND address.semantic_class = 'shared_market'
          )
    ) THEN
        RAISE EXCEPTION 'shared-market catalog revision does not match its sealed family manifest';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_shared_catalog_revision_consistent
    ON loyal_yield.lookup_table_shared_market_catalog_revisions;
CREATE TRIGGER lookup_table_shared_catalog_revision_consistent
    BEFORE INSERT ON loyal_yield.lookup_table_shared_market_catalog_revisions
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_shared_catalog_revision_insert();

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_shared_catalog_revision_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'shared-market catalog revisions are immutable';
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_shared_catalog_revisions_immutable
    ON loyal_yield.lookup_table_shared_market_catalog_revisions;
CREATE TRIGGER lookup_table_shared_catalog_revisions_immutable
    BEFORE UPDATE OR DELETE
    ON loyal_yield.lookup_table_shared_market_catalog_revisions
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_shared_catalog_revision_mutation();

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_shared_catalog_head_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    new_family_id BIGINT;
    new_revision BIGINT;
    old_revision BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'shared-market catalog heads cannot be deleted';
    END IF;

    SELECT family_id, catalog_revision
    INTO new_family_id, new_revision
    FROM loyal_yield.lookup_table_shared_market_catalog_revisions
    WHERE id = NEW.catalog_revision_id;

    IF new_family_id IS DISTINCT FROM NEW.family_id THEN
        RAISE EXCEPTION 'shared-market catalog head points across families';
    END IF;

    IF TG_OP = 'INSERT' THEN
        IF new_revision <> 1 THEN
            RAISE EXCEPTION 'first shared-market catalog head revision must be 1';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.family_id IS DISTINCT FROM OLD.family_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'shared-market catalog head identity is immutable';
    END IF;

    IF NEW.catalog_revision_id IS DISTINCT FROM OLD.catalog_revision_id THEN
        SELECT catalog_revision
        INTO old_revision
        FROM loyal_yield.lookup_table_shared_market_catalog_revisions
        WHERE id = OLD.catalog_revision_id;
        IF new_revision <> old_revision + 1
           OR NEW.target_generation IS NOT NULL
           OR NEW.readiness_state <> 'pending'
           OR NEW.activated_at IS NOT NULL
        THEN
            RAISE EXCEPTION 'shared-market catalog head advancement must be monotonic and reset readiness';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_shared_catalog_head_consistent
    ON loyal_yield.lookup_table_shared_market_catalog_heads;
CREATE TRIGGER lookup_table_shared_catalog_head_consistent
    BEFORE INSERT OR UPDATE OR DELETE
    ON loyal_yield.lookup_table_shared_market_catalog_heads
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_shared_catalog_head_mutation();

-- Finalized RPC drift is durable input to the catalog planner. The evidence
-- is immutable and fenced to the exact catalog revision, physical row, and
-- mutation epoch that were observed. An open report forces a new generation;
-- it is resolved only after that replacement generation becomes active.
CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_shared_market_physical_drifts (
    id BIGSERIAL PRIMARY KEY,
    evidence_hash TEXT NOT NULL UNIQUE,
    cluster TEXT NOT NULL,
    family_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_families(id),
    catalog_revision_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_shared_market_catalog_revisions(id),
    route_lookup_table_id BIGINT NOT NULL
        REFERENCES loyal_yield.route_lookup_tables(id),
    expected_mutation_epoch BIGINT NOT NULL,
    expected_table_address TEXT NOT NULL,
    expected_authority TEXT NOT NULL,
    observed_slot BIGINT NOT NULL,
    observed_table_present BOOLEAN NOT NULL,
    observed_authority TEXT,
    observed_active BOOLEAN NOT NULL,
    observed_last_extended_slot BIGINT,
    observed_warm BOOLEAN NOT NULL,
    observed_address_hash TEXT NOT NULL,
    observed_addresses JSONB NOT NULL,
    reason TEXT NOT NULL,
    reported_by TEXT NOT NULL,
    resolution_state TEXT NOT NULL DEFAULT 'open',
    resolution_target_generation INTEGER,
    resolved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_shared_market_physical_drift_hash_check
        CHECK (evidence_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT lookup_table_shared_market_physical_drift_observation_check
        CHECK (
            expected_mutation_epoch >= 0
            AND observed_slot >= 0
            AND (observed_last_extended_slot IS NULL OR observed_last_extended_slot >= 0)
            AND (
                observed_table_present
                OR (observed_last_extended_slot IS NULL AND NOT observed_warm)
            )
            AND (NOT observed_warm OR observed_last_extended_slot IS NOT NULL)
            AND jsonb_typeof(observed_addresses) = 'array'
            AND jsonb_array_length(observed_addresses) BETWEEN 0 AND 256
            AND observed_address_hash ~ '^[0-9a-f]{64}$'
            AND length(btrim(expected_table_address)) > 0
            AND length(btrim(expected_authority)) > 0
            AND length(btrim(reason)) > 0
            AND length(btrim(reported_by)) > 0
        ),
    CONSTRAINT lookup_table_shared_market_physical_drift_resolution_check
        CHECK (
            (resolution_state = 'open'
             AND resolution_target_generation IS NULL
             AND resolved_at IS NULL)
            OR
            (resolution_state = 'resolved'
             AND resolution_target_generation IS NOT NULL
             AND resolution_target_generation >= 0
             AND resolved_at IS NOT NULL)
        )
);

CREATE INDEX IF NOT EXISTS lookup_table_shared_market_physical_drift_open_idx
    ON loyal_yield.lookup_table_shared_market_physical_drifts
        (family_id, catalog_revision_id, resolution_state, created_at)
    WHERE resolution_state = 'open';

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_shared_market_physical_drift_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'shared-market physical drift evidence cannot be deleted';
    END IF;
    IF NEW.evidence_hash IS DISTINCT FROM OLD.evidence_hash
       OR NEW.cluster IS DISTINCT FROM OLD.cluster
       OR NEW.family_id IS DISTINCT FROM OLD.family_id
       OR NEW.catalog_revision_id IS DISTINCT FROM OLD.catalog_revision_id
       OR NEW.route_lookup_table_id IS DISTINCT FROM OLD.route_lookup_table_id
       OR NEW.expected_mutation_epoch IS DISTINCT FROM OLD.expected_mutation_epoch
       OR NEW.expected_table_address IS DISTINCT FROM OLD.expected_table_address
       OR NEW.expected_authority IS DISTINCT FROM OLD.expected_authority
       OR NEW.observed_slot IS DISTINCT FROM OLD.observed_slot
       OR NEW.observed_table_present IS DISTINCT FROM OLD.observed_table_present
       OR NEW.observed_authority IS DISTINCT FROM OLD.observed_authority
       OR NEW.observed_active IS DISTINCT FROM OLD.observed_active
       OR NEW.observed_last_extended_slot IS DISTINCT FROM OLD.observed_last_extended_slot
       OR NEW.observed_warm IS DISTINCT FROM OLD.observed_warm
       OR NEW.observed_address_hash IS DISTINCT FROM OLD.observed_address_hash
       OR NEW.observed_addresses IS DISTINCT FROM OLD.observed_addresses
       OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.reported_by IS DISTINCT FROM OLD.reported_by
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'shared-market physical drift evidence is immutable';
    END IF;
    IF OLD.resolution_state = 'resolved'
       AND NEW IS DISTINCT FROM OLD
    THEN
        RAISE EXCEPTION 'resolved shared-market physical drift evidence is immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_shared_market_physical_drift_immutable
    ON loyal_yield.lookup_table_shared_market_physical_drifts;
CREATE TRIGGER lookup_table_shared_market_physical_drift_immutable
    BEFORE UPDATE OR DELETE
    ON loyal_yield.lookup_table_shared_market_physical_drifts
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_shared_market_physical_drift_mutation();

-- Every provisioning send reserves its worst-case fee plus rent in PostgreSQL
-- under the operation lease fencing token. Reservations survive process
-- restarts and overlap safely across Render instances. Cancelled operations
-- are excluded at read time; every other reservation remains conservative.
CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_cluster_budget_reservations (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    operation_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_operations(id),
    fencing_token BIGINT NOT NULL,
    lease_owner TEXT NOT NULL,
    estimated_fee_lamports BIGINT NOT NULL,
    estimated_rent_lamports BIGINT NOT NULL,
    reserved_lamports BIGINT NOT NULL,
    reserved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    reserved_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_cluster_budget_operation_fence_unique
        UNIQUE (operation_id, fencing_token),
    CONSTRAINT lookup_table_cluster_budget_amount_check
        CHECK (
            fencing_token > 0
            AND estimated_fee_lamports >= 0
            AND estimated_rent_lamports >= 0
            AND reserved_lamports = estimated_fee_lamports + estimated_rent_lamports
            AND reserved_until > reserved_at
            AND length(btrim(cluster)) > 0
            AND length(btrim(lease_owner)) > 0
        )
);

CREATE INDEX IF NOT EXISTS lookup_table_cluster_budget_active_idx
    ON loyal_yield.lookup_table_cluster_budget_reservations
        (cluster, reserved_until, operation_id);

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_cluster_budget_reservation_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'lookup-table cluster budget reservations are immutable';
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_cluster_budget_reservations_immutable
    ON loyal_yield.lookup_table_cluster_budget_reservations;
CREATE TRIGGER lookup_table_cluster_budget_reservations_immutable
    BEFORE UPDATE OR DELETE
    ON loyal_yield.lookup_table_cluster_budget_reservations
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_cluster_budget_reservation_mutation();
