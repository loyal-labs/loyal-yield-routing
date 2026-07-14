-- Durable shared-market ALT bundles.
--
-- A shared-market catalog is one logical, append-ordered manifest. Its total
-- address count may exceed one physical ALT's allocation high-water mark. The
-- planner deterministically partitions that manifest into contiguous physical
-- shards, while every physical route_lookup_tables row remains bounded by its
-- family's per-table high-water and Solana's 256-address hard limit.

ALTER TABLE loyal_yield.lookup_table_shared_market_catalog_revisions
    DROP CONSTRAINT IF EXISTS lookup_table_shared_catalog_address_count_check;

ALTER TABLE loyal_yield.lookup_table_shared_market_catalog_revisions
    ADD CONSTRAINT lookup_table_shared_catalog_address_count_check
        CHECK (address_count > 0);

-- Replace migration 0020's insert guard without changing that migration's
-- recorded bytes. Logical catalog size is no longer constrained by one
-- physical table; the sealed manifest and its typed address rows remain exact.
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

-- The singular table fields from migration 0021 remain the selected synthetic
-- drift target. These bundle fields are the authoritative cutover evidence for
-- every physical shared table in the active generation.
ALTER TABLE loyal_yield.lookup_table_precutover_probe_runs
    ADD COLUMN IF NOT EXISTS shared_table_bundle_hash TEXT,
    ADD COLUMN IF NOT EXISTS shared_table_count INTEGER,
    ADD COLUMN IF NOT EXISTS finalized_bundle_address_count INTEGER;

-- This is the exact framing used by Rust's hash_length_prefixed_values:
-- every UTF-8 value is prefixed by its byte length encoded as an unsigned
-- 64-bit little-endian integer, then the complete byte stream is SHA-256'd.
-- PostgreSQL BIGINT is signed, but every TEXT value is necessarily far below
-- its positive maximum, so int8send plus a byte reversal is exact here.
CREATE OR REPLACE FUNCTION loyal_yield.hash_length_prefixed_text(input_values TEXT[])
RETURNS TEXT
LANGUAGE plpgsql
IMMUTABLE
STRICT
PARALLEL SAFE
AS $$
DECLARE
    input_value TEXT;
    value_bytes BYTEA;
    length_big_endian BYTEA;
    payload BYTEA := decode('', 'hex');
BEGIN
    FOREACH input_value IN ARRAY input_values LOOP
        IF input_value IS NULL THEN
            RAISE EXCEPTION 'length-prefixed hash values cannot contain NULL';
        END IF;

        value_bytes := convert_to(input_value, 'UTF8');
        length_big_endian := int8send(octet_length(value_bytes)::BIGINT);
        payload := payload
            || substring(length_big_endian FROM 8 FOR 1)
            || substring(length_big_endian FROM 7 FOR 1)
            || substring(length_big_endian FROM 6 FOR 1)
            || substring(length_big_endian FROM 5 FOR 1)
            || substring(length_big_endian FROM 4 FOR 1)
            || substring(length_big_endian FROM 3 FOR 1)
            || substring(length_big_endian FROM 2 FOR 1)
            || substring(length_big_endian FROM 1 FOR 1)
            || value_bytes;
    END LOOP;

    RETURN encode(sha256(payload), 'hex');
END;
$$;

-- Temporarily remove migration 0021's parent immutability trigger only for
-- this transactional schema backfill; a migration failure rolls the drop
-- back. Historical probes receive their real one-table bundle hash only after
-- their immutable child snapshot has been materialized below.
DROP TRIGGER IF EXISTS lookup_table_precutover_probe_runs_immutable
    ON loyal_yield.lookup_table_precutover_probe_runs;

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_precutover_probe_shared_tables (
    probe_run_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_precutover_probe_runs(id),
    shard_ordinal INTEGER NOT NULL,
    route_lookup_table_id BIGINT NOT NULL
        REFERENCES loyal_yield.route_lookup_tables(id),
    shared_table_address TEXT NOT NULL,
    shared_authority TEXT NOT NULL,
    shared_mutation_epoch BIGINT NOT NULL,
    finalized_slot BIGINT NOT NULL,
    finalized_last_extended_slot BIGINT NOT NULL,
    finalized_address_hash TEXT NOT NULL,
    finalized_address_count INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (probe_run_id, shard_ordinal),
    CONSTRAINT lookup_table_precutover_probe_shared_table_id_unique
        UNIQUE (probe_run_id, route_lookup_table_id),
    CONSTRAINT lookup_table_precutover_probe_shared_table_address_unique
        UNIQUE (probe_run_id, shared_table_address),
    CONSTRAINT lookup_table_precutover_probe_shared_table_identity_check CHECK (
        shard_ordinal >= 0
        AND length(btrim(shared_table_address)) > 0
        AND length(btrim(shared_authority)) > 0
        AND shared_mutation_epoch >= 0
        AND finalized_slot >= 0
        AND finalized_last_extended_slot >= 0
        AND finalized_last_extended_slot < finalized_slot
        AND finalized_address_hash ~ '^[0-9a-f]{64}$'
        AND finalized_address_count BETWEEN 1 AND 256
    )
);

-- Backfill the one-table shape before enabling the deferred bundle guard.
INSERT INTO loyal_yield.lookup_table_precutover_probe_shared_tables
    (probe_run_id, shard_ordinal, route_lookup_table_id,
     shared_table_address, shared_authority, shared_mutation_epoch,
     finalized_slot, finalized_last_extended_slot,
     finalized_address_hash, finalized_address_count)
SELECT probe.id,
       0,
       probe.route_lookup_table_id,
       probe.shared_table_address,
       probe.shared_authority,
       probe.shared_mutation_epoch,
       probe.finalized_slot,
       probe.finalized_last_extended_slot,
       probe.finalized_address_hash,
       probe.finalized_address_count
FROM loyal_yield.lookup_table_precutover_probe_runs probe
ON CONFLICT (probe_run_id, shard_ordinal) DO NOTHING;

CREATE INDEX IF NOT EXISTS lookup_table_precutover_probe_shared_tables_route_idx
    ON loyal_yield.lookup_table_precutover_probe_shared_tables
        (route_lookup_table_id, probe_run_id);

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_precutover_probe_shared_table_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'pre-cutover probe shared-table audit rows are immutable';
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_precutover_probe_shared_tables_immutable
    ON loyal_yield.lookup_table_precutover_probe_shared_tables;
CREATE TRIGGER lookup_table_precutover_probe_shared_tables_immutable
    BEFORE UPDATE OR DELETE
    ON loyal_yield.lookup_table_precutover_probe_shared_tables
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_precutover_probe_shared_table_mutation();

-- Backfill every parent from the immutable child snapshot. The field order is
-- part of the Rust/SQL contract: table id, shard ordinal, table address,
-- authority, mutation epoch, last-extended slot, ordered-address hash, count.
WITH child_summaries AS (
    SELECT shared.probe_run_id,
           count(*)::INTEGER AS shared_table_count,
           sum(shared.finalized_address_count)::INTEGER
               AS finalized_bundle_address_count
    FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
    GROUP BY shared.probe_run_id
), bundle_hashes AS (
    SELECT shared.probe_run_id,
           loyal_yield.hash_length_prefixed_text(
               ARRAY['loyal-reusable-shared-table-bundle-v1']::TEXT[]
               || array_agg(
                    part.value
                    ORDER BY shared.shard_ordinal, part.field_ordinal
               )
           ) AS shared_table_bundle_hash
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
    ) AS part(field_ordinal, value)
    GROUP BY shared.probe_run_id
)
UPDATE loyal_yield.lookup_table_precutover_probe_runs probe
SET shared_table_bundle_hash = bundle_hash.shared_table_bundle_hash,
    shared_table_count = summary.shared_table_count,
    finalized_bundle_address_count = summary.finalized_bundle_address_count
FROM child_summaries summary
JOIN bundle_hashes bundle_hash
  ON bundle_hash.probe_run_id = summary.probe_run_id
WHERE probe.id = summary.probe_run_id;

ALTER TABLE loyal_yield.lookup_table_precutover_probe_runs
    ALTER COLUMN shared_table_bundle_hash SET NOT NULL,
    ALTER COLUMN shared_table_count SET NOT NULL,
    ALTER COLUMN finalized_bundle_address_count SET NOT NULL,
    DROP CONSTRAINT IF EXISTS lookup_table_precutover_probe_bundle_check,
    ADD CONSTRAINT lookup_table_precutover_probe_bundle_check CHECK (
        shared_table_bundle_hash ~ '^[0-9a-f]{64}$'
        AND shared_table_count > 0
        AND finalized_bundle_address_count >= shared_table_count
        AND finalized_bundle_address_count::BIGINT
            <= shared_table_count::BIGINT * 256
    );

CREATE TRIGGER lookup_table_precutover_probe_runs_immutable
    BEFORE UPDATE OR DELETE ON loyal_yield.lookup_table_precutover_probe_runs
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_precutover_probe_run_mutation();

-- Validate the complete bundle only at commit so the parent PASS row and all
-- child shard rows can be inserted atomically in either statement order.
CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_precutover_probe_bundle_consistency()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    checked_probe_id BIGINT;
    probe loyal_yield.lookup_table_precutover_probe_runs%ROWTYPE;
    observed_child_count BIGINT;
    observed_distinct_ordinal_count BIGINT;
    observed_min_ordinal INTEGER;
    observed_max_ordinal INTEGER;
    observed_total_address_count BIGINT;
    recomputed_bundle_hash TEXT;
    valid_physical_identity_count BIGINT;
    invalid_partition BOOLEAN;
BEGIN
    checked_probe_id := COALESCE(
        (to_jsonb(NEW)->>'probe_run_id')::BIGINT,
        (to_jsonb(NEW)->>'id')::BIGINT
    );

    SELECT *
    INTO probe
    FROM loyal_yield.lookup_table_precutover_probe_runs
    WHERE id = checked_probe_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'pre-cutover probe bundle parent is missing';
    END IF;

    SELECT count(*),
           count(DISTINCT shard_ordinal),
           min(shard_ordinal),
           max(shard_ordinal),
           COALESCE(sum(finalized_address_count), 0)
    INTO observed_child_count,
         observed_distinct_ordinal_count,
         observed_min_ordinal,
         observed_max_ordinal,
         observed_total_address_count
    FROM loyal_yield.lookup_table_precutover_probe_shared_tables
    WHERE probe_run_id = checked_probe_id;

    IF observed_child_count <> probe.shared_table_count
       OR observed_distinct_ordinal_count <> probe.shared_table_count
       OR observed_min_ordinal <> 0
       OR observed_max_ordinal <> probe.shared_table_count - 1
       OR observed_total_address_count <> probe.finalized_bundle_address_count
    THEN
        RAISE EXCEPTION 'pre-cutover probe shared-table bundle count is inconsistent';
    END IF;

    SELECT loyal_yield.hash_length_prefixed_text(
               ARRAY['loyal-reusable-shared-table-bundle-v1']::TEXT[]
               || array_agg(
                    part.value
                    ORDER BY shared.shard_ordinal, part.field_ordinal
               )
           )
    INTO recomputed_bundle_hash
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
    ) AS part(field_ordinal, value)
    WHERE shared.probe_run_id = checked_probe_id;

    IF recomputed_bundle_hash IS DISTINCT FROM probe.shared_table_bundle_hash THEN
        RAISE EXCEPTION 'pre-cutover probe shared-table bundle hash is inconsistent';
    END IF;

    -- Count only rows that satisfy the complete positive identity. This makes
    -- an absent head/revision/family/manifest join fail closed instead of
    -- letting a NOT-EXISTS-style negative mismatch query return no errors.
    SELECT count(*)
    INTO valid_physical_identity_count
    FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
    JOIN loyal_yield.route_lookup_tables route_table
      ON route_table.id = shared.route_lookup_table_id
    JOIN loyal_yield.lookup_table_shared_market_catalog_revisions revision
      ON revision.id = probe.catalog_revision_id
    JOIN loyal_yield.lookup_table_manifests manifest
      ON manifest.id = probe.shared_manifest_id
    JOIN loyal_yield.lookup_table_families family
      ON family.id = revision.family_id
    JOIN loyal_yield.lookup_table_shared_market_catalog_heads head
      ON head.family_id = revision.family_id
    WHERE shared.probe_run_id = checked_probe_id
      AND family.cluster = probe.cluster
      AND family.kind = 'shared_market'
      AND family.desired_state = 'active'
      AND family.active_generation IS NOT NULL
      AND family.catalog_version = revision.catalog_version
      AND revision.manifest_id = probe.shared_manifest_id
      AND revision.address_count = probe.finalized_bundle_address_count
      AND manifest.family_id = revision.family_id
      AND manifest.subject_kind = 'shared_market'
      AND manifest.vault_id IS NULL
      AND manifest.sealed_at IS NOT NULL
      AND manifest.catalog_version = revision.catalog_version
      AND manifest.desired_set_hash = revision.desired_set_hash
      AND manifest.address_count = revision.address_count
      AND head.catalog_revision_id = probe.catalog_revision_id
      AND head.readiness_state = 'active'
      AND head.activated_at IS NOT NULL
      AND head.target_generation = family.active_generation
      AND route_table.cluster = probe.cluster
      AND route_table.cluster = family.cluster
      AND route_table.family_id = revision.family_id
      AND route_table.allocation_kind = 'shared_market'
      AND route_table.generation = head.target_generation
      AND route_table.shard_ordinal = shared.shard_ordinal
      AND route_table.durable
      AND route_table.desired_state = 'active'
      AND route_table.status = 'usable'
      AND route_table.table_address = shared.shared_table_address
      AND route_table.authority = shared.shared_authority
      AND route_table.mutation_epoch = shared.shared_mutation_epoch
      AND route_table.last_extended_slot = shared.finalized_last_extended_slot
      AND route_table.address_count = shared.finalized_address_count
      AND route_table.usable_address_count = shared.finalized_address_count
      AND route_table.address_hash = shared.finalized_address_hash
      AND route_table.allocation_high_water = family.allocation_high_water
      AND shared.finalized_address_count <= family.allocation_high_water
      AND shared.finalized_slot = probe.finalized_slot;

    IF valid_physical_identity_count <> probe.shared_table_count THEN
        RAISE EXCEPTION 'pre-cutover probe shared-table bundle identity is inconsistent';
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
        WHERE shared.probe_run_id = checked_probe_id
          AND shared.route_lookup_table_id = probe.route_lookup_table_id
          AND shared.shared_table_address = probe.shared_table_address
          AND shared.shared_authority = probe.shared_authority
          AND shared.shared_mutation_epoch = probe.shared_mutation_epoch
          AND shared.finalized_slot = probe.finalized_slot
          AND shared.finalized_last_extended_slot = probe.finalized_last_extended_slot
          AND shared.finalized_address_hash = probe.finalized_address_hash
          AND shared.finalized_address_count = probe.finalized_address_count
    ) THEN
        RAISE EXCEPTION 'pre-cutover probe synthetic drift target is outside the shared bundle';
    END IF;

    WITH expected AS (
        SELECT (address.ordinal / family.allocation_high_water)::INTEGER
                   AS shard_ordinal,
               (address.ordinal % family.allocation_high_water)::INTEGER
                   AS physical_ordinal,
               address.address
        FROM loyal_yield.lookup_table_manifest_addresses address
        JOIN loyal_yield.lookup_table_manifests manifest
          ON manifest.id = address.manifest_id
        JOIN loyal_yield.lookup_table_families family
          ON family.id = manifest.family_id
        WHERE manifest.id = probe.shared_manifest_id
          AND address.semantic_class = 'shared_market'
    ), observed AS (
        SELECT shared.shard_ordinal,
               membership.ordinal AS physical_ordinal,
               membership.address
        FROM loyal_yield.lookup_table_precutover_probe_shared_tables shared
        JOIN loyal_yield.lookup_table_addresses membership
          ON membership.route_lookup_table_id = shared.route_lookup_table_id
        WHERE shared.probe_run_id = checked_probe_id
    )
    SELECT EXISTS (
        SELECT 1
        FROM expected
        FULL JOIN observed
          USING (shard_ordinal, physical_ordinal)
        WHERE expected.address IS DISTINCT FROM observed.address
    )
    INTO invalid_partition;

    IF invalid_partition THEN
        RAISE EXCEPTION 'pre-cutover probe shared-table bundle is not the deterministic catalog partition';
    END IF;

    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_precutover_probe_bundle_consistent
    ON loyal_yield.lookup_table_precutover_probe_runs;
CREATE CONSTRAINT TRIGGER lookup_table_precutover_probe_bundle_consistent
    AFTER INSERT ON loyal_yield.lookup_table_precutover_probe_runs
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_precutover_probe_bundle_consistency();

DROP TRIGGER IF EXISTS lookup_table_precutover_probe_shared_tables_consistent
    ON loyal_yield.lookup_table_precutover_probe_shared_tables;
CREATE CONSTRAINT TRIGGER lookup_table_precutover_probe_shared_tables_consistent
    AFTER INSERT ON loyal_yield.lookup_table_precutover_probe_shared_tables
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_precutover_probe_bundle_consistency();
