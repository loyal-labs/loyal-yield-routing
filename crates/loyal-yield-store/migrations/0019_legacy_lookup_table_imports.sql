-- Audited import state for exact-route lookup tables created before the
-- reusable ALT control plane. Legacy tables stay outside reusable families;
-- this migration only classifies and records successful RPC reverification.

ALTER TABLE loyal_yield.route_lookup_tables
    ADD COLUMN IF NOT EXISTS legacy_kind TEXT;

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_legacy_import_runs (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    rpc_genesis_hash TEXT NOT NULL,
    verified_slot BIGINT NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL,
    legacy_kind TEXT NOT NULL,
    expected_table_count INTEGER NOT NULL,
    verified_table_count INTEGER NOT NULL,
    import_fingerprint TEXT NOT NULL,
    reason TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_legacy_import_runs_identity_unique
        UNIQUE (cluster, import_fingerprint),
    CONSTRAINT lookup_table_legacy_import_runs_kind_check
        CHECK (legacy_kind IN ('legacy_route', 'legacy_mixed')),
    CONSTRAINT lookup_table_legacy_import_runs_count_check
        CHECK (
            expected_table_count > 0
            AND verified_table_count = expected_table_count
        ),
    CONSTRAINT lookup_table_legacy_import_runs_slot_check
        CHECK (verified_slot >= 0),
    CONSTRAINT lookup_table_legacy_import_runs_text_check
        CHECK (
            length(rpc_genesis_hash) > 0
            AND length(import_fingerprint) = 64
            AND import_fingerprint ~ '^[0-9a-f]{64}$'
            AND length(btrim(reason)) > 0
            AND length(btrim(updated_by)) > 0
        )
);

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_legacy_import_evidence (
    id BIGSERIAL PRIMARY KEY,
    import_run_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_legacy_import_runs(id),
    route_lookup_table_id BIGINT NOT NULL
        REFERENCES loyal_yield.route_lookup_tables(id),
    table_address TEXT NOT NULL,
    scope TEXT NOT NULL,
    legacy_kind TEXT NOT NULL,
    expected_authority TEXT NOT NULL,
    observed_authority TEXT NOT NULL,
    observed_owner TEXT NOT NULL,
    observed_deactivation_slot TEXT NOT NULL,
    observed_last_extended_slot BIGINT NOT NULL,
    observed_last_extended_start_index INTEGER NOT NULL,
    address_count INTEGER NOT NULL,
    address_hash TEXT NOT NULL,
    addresses JSONB NOT NULL,
    verified_slot BIGINT NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_legacy_import_evidence_run_table_unique
        UNIQUE (import_run_id, route_lookup_table_id),
    CONSTRAINT lookup_table_legacy_import_evidence_kind_check
        CHECK (legacy_kind IN ('legacy_route', 'legacy_mixed')),
    CONSTRAINT lookup_table_legacy_import_evidence_count_check
        CHECK (
            address_count BETWEEN 0 AND 256
            AND jsonb_typeof(addresses) = 'array'
            AND jsonb_array_length(addresses) = address_count
            AND observed_last_extended_start_index <= address_count
        ),
    CONSTRAINT lookup_table_legacy_import_evidence_slot_check
        CHECK (
            observed_last_extended_slot >= 0
            AND observed_last_extended_start_index BETWEEN 0 AND 255
            AND verified_slot > observed_last_extended_slot
        ),
    CONSTRAINT lookup_table_legacy_import_evidence_hash_check
        CHECK (
            address_hash ~ '^[0-9a-f]{64}$'
            AND observed_authority = expected_authority
            AND observed_owner = 'AddressLookupTab1e1111111111111111111111111'
            AND observed_deactivation_slot = '18446744073709551615'
        )
);

ALTER TABLE loyal_yield.route_lookup_tables
    ADD COLUMN IF NOT EXISTS legacy_import_run_id BIGINT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.route_lookup_tables'::regclass
          AND conname = 'route_lookup_tables_legacy_import_run_id_fkey'
    ) THEN
        ALTER TABLE loyal_yield.route_lookup_tables
            ADD CONSTRAINT route_lookup_tables_legacy_import_run_id_fkey
            FOREIGN KEY (legacy_import_run_id)
            REFERENCES loyal_yield.lookup_table_legacy_import_runs(id);
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.route_lookup_tables'::regclass
          AND conname = 'route_lookup_tables_legacy_import_check'
    ) THEN
        ALTER TABLE loyal_yield.route_lookup_tables
            ADD CONSTRAINT route_lookup_tables_legacy_import_check
            CHECK (
                (
                    family_id IS NOT NULL
                    AND legacy_kind IS NULL
                    AND legacy_import_run_id IS NULL
                )
                OR (
                    family_id IS NULL
                    AND (
                        (
                            legacy_kind IS NULL
                            AND legacy_import_run_id IS NULL
                        )
                        OR (
                            legacy_kind IN ('legacy_route', 'legacy_mixed')
                            AND legacy_import_run_id IS NOT NULL
                            AND last_verified_slot IS NOT NULL
                            AND last_verified_at IS NOT NULL
                        )
                    )
                )
            );
    END IF;
END;
$$;

CREATE INDEX IF NOT EXISTS route_lookup_tables_legacy_import_idx
    ON loyal_yield.route_lookup_tables (
        cluster,
        legacy_kind,
        last_verified_at DESC
    )
    WHERE family_id IS NULL;

CREATE INDEX IF NOT EXISTS lookup_table_legacy_import_evidence_table_idx
    ON loyal_yield.lookup_table_legacy_import_evidence (
        route_lookup_table_id,
        verified_at DESC
    );

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_legacy_registry_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.legacy_kind IS NOT NULL
       AND NEW.legacy_kind IS DISTINCT FROM OLD.legacy_kind
    THEN
        RAISE EXCEPTION 'legacy lookup-table classification is immutable';
    END IF;

    IF OLD.legacy_import_run_id IS NOT NULL
       AND (
           NEW.cluster IS DISTINCT FROM OLD.cluster
           OR NEW.scope IS DISTINCT FROM OLD.scope
           OR NEW.table_address IS DISTINCT FROM OLD.table_address
           OR NEW.authority IS DISTINCT FROM OLD.authority
           OR NEW.address_count IS DISTINCT FROM OLD.address_count
           OR NEW.address_hash IS DISTINCT FROM OLD.address_hash
           OR NEW.addresses IS DISTINCT FROM OLD.addresses
           OR NEW.last_extended_slot IS DISTINCT FROM OLD.last_extended_slot
           OR NEW.last_extended_start_index IS DISTINCT FROM OLD.last_extended_start_index
       )
    THEN
        RAISE EXCEPTION 'verified legacy lookup-table registry evidence is immutable';
    END IF;

    IF NEW.legacy_import_run_id IS NOT NULL AND NOT EXISTS (
        SELECT 1
        FROM loyal_yield.lookup_table_legacy_import_evidence evidence
        JOIN loyal_yield.lookup_table_legacy_import_runs import_run
          ON import_run.id = evidence.import_run_id
        WHERE evidence.import_run_id = NEW.legacy_import_run_id
          AND evidence.route_lookup_table_id = NEW.id
          AND evidence.table_address = NEW.table_address
          AND evidence.scope = NEW.scope
          AND evidence.legacy_kind = NEW.legacy_kind
          AND evidence.expected_authority = NEW.authority
          AND evidence.address_count = NEW.address_count
          AND evidence.address_hash = NEW.address_hash
          AND evidence.addresses = NEW.addresses
          AND evidence.observed_last_extended_slot = NEW.last_extended_slot
          AND evidence.observed_last_extended_start_index = NEW.last_extended_start_index
          AND evidence.verified_slot = NEW.last_verified_slot
          AND evidence.verified_at = NEW.last_verified_at
          AND import_run.cluster = NEW.cluster
    ) THEN
        RAISE EXCEPTION 'legacy lookup-table registry pointer lacks exact immutable evidence';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS route_lookup_tables_legacy_kind_immutable
    ON loyal_yield.route_lookup_tables;
CREATE TRIGGER route_lookup_tables_legacy_kind_immutable
    BEFORE UPDATE ON loyal_yield.route_lookup_tables
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_legacy_registry_update();

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_legacy_import_evidence_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    expected_count INTEGER;
BEGIN
    SELECT expected_table_count
    INTO expected_count
    FROM loyal_yield.lookup_table_legacy_import_runs
    WHERE id = NEW.import_run_id
    FOR UPDATE;

    IF expected_count IS NULL THEN
        RAISE EXCEPTION 'legacy lookup-table import evidence has no audit run';
    END IF;

    IF (
        SELECT count(*)
        FROM loyal_yield.lookup_table_legacy_import_evidence
        WHERE import_run_id = NEW.import_run_id
    ) >= expected_count THEN
        RAISE EXCEPTION 'legacy lookup-table import evidence exceeds the approved fleet size';
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM loyal_yield.lookup_table_legacy_import_runs import_run
        JOIN loyal_yield.route_lookup_tables route_table
          ON route_table.id = NEW.route_lookup_table_id
        WHERE import_run.id = NEW.import_run_id
          AND import_run.cluster = route_table.cluster
          AND import_run.legacy_kind = NEW.legacy_kind
          AND import_run.verified_slot = NEW.verified_slot
          AND import_run.verified_at = NEW.verified_at
          AND route_table.family_id IS NULL
          AND route_table.table_address = NEW.table_address
          AND route_table.scope = NEW.scope
          AND route_table.authority = NEW.expected_authority
          AND route_table.address_count = NEW.address_count
          AND route_table.address_hash = NEW.address_hash
          AND route_table.addresses = NEW.addresses
          AND (route_table.legacy_kind IS NULL OR route_table.legacy_kind = NEW.legacy_kind)
    ) THEN
        RAISE EXCEPTION 'legacy lookup-table import evidence does not match its run and registry row';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_legacy_import_evidence_consistent
    ON loyal_yield.lookup_table_legacy_import_evidence;
CREATE TRIGGER lookup_table_legacy_import_evidence_consistent
    BEFORE INSERT ON loyal_yield.lookup_table_legacy_import_evidence
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_legacy_import_evidence_insert();

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_legacy_import_audit_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'legacy lookup-table import audit evidence is immutable';
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_legacy_import_runs_immutable
    ON loyal_yield.lookup_table_legacy_import_runs;
CREATE TRIGGER lookup_table_legacy_import_runs_immutable
    BEFORE UPDATE OR DELETE ON loyal_yield.lookup_table_legacy_import_runs
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_legacy_import_audit_mutation();

DROP TRIGGER IF EXISTS lookup_table_legacy_import_evidence_immutable
    ON loyal_yield.lookup_table_legacy_import_evidence;
CREATE TRIGGER lookup_table_legacy_import_evidence_immutable
    BEFORE UPDATE OR DELETE ON loyal_yield.lookup_table_legacy_import_evidence
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_legacy_import_audit_mutation();

-- Retirement is the durable, nonblocking cleanup fence. Once an imported
-- legacy table is nonselectable, no current control-plane reference may be
-- created for it while the chain mutation is in flight.
CREATE OR REPLACE FUNCTION loyal_yield.guard_retired_legacy_lookup_table_reference()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    referenced_ids BIGINT[];
    referenced_cluster TEXT;
    active_reference BOOLEAN;
BEGIN
    IF TG_TABLE_NAME = 'lookup_table_route_readiness_current' THEN
        referenced_ids := COALESCE(NEW.legacy_table_ids, '{}'::BIGINT[])
            || COALESCE(NEW.selected_table_ids, '{}'::BIGINT[]);
        referenced_cluster := NEW.cluster;
        active_reference := TRUE;
    ELSIF TG_TABLE_NAME = 'lookup_table_usage_leases' THEN
        referenced_ids := ARRAY[NEW.route_lookup_table_id]::BIGINT[];
        referenced_cluster := NEW.cluster;
        active_reference := NEW.released_at IS NULL AND NEW.expires_at > now();
    ELSE
        referenced_ids := ARRAY[NEW.route_lookup_table_id]::BIGINT[];
        SELECT cluster INTO referenced_cluster
        FROM loyal_yield.route_lookup_tables
        WHERE id = NEW.route_lookup_table_id;
        active_reference := NEW.operation_state NOT IN (
            'complete', 'permanent_failure', 'cancelled'
        );
    END IF;

    IF referenced_cluster IS NOT NULL THEN
        PERFORM pg_advisory_xact_lock(
            hashtextextended('reusable-alt-rollout:' || referenced_cluster, 0)
        );
    END IF;

    IF NOT active_reference THEN
        RETURN NEW;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM loyal_yield.route_lookup_tables route_table
        WHERE route_table.id = ANY(referenced_ids)
          AND route_table.family_id IS NULL
          AND route_table.legacy_import_run_id IS NOT NULL
          AND (
              route_table.durable = FALSE
              OR route_table.status NOT IN ('active', 'warming', 'usable')
          )
    ) THEN
        RAISE EXCEPTION 'retired imported legacy lookup table cannot acquire a new control-plane reference';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_readiness_rejects_retired_legacy
    ON loyal_yield.lookup_table_route_readiness_current;
CREATE TRIGGER lookup_table_readiness_rejects_retired_legacy
    BEFORE INSERT OR UPDATE ON loyal_yield.lookup_table_route_readiness_current
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_retired_legacy_lookup_table_reference();

DROP TRIGGER IF EXISTS lookup_table_usage_rejects_retired_legacy
    ON loyal_yield.lookup_table_usage_leases;
CREATE TRIGGER lookup_table_usage_rejects_retired_legacy
    BEFORE INSERT OR UPDATE ON loyal_yield.lookup_table_usage_leases
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_retired_legacy_lookup_table_reference();

DROP TRIGGER IF EXISTS lookup_table_operation_rejects_retired_legacy
    ON loyal_yield.lookup_table_operations;
CREATE TRIGGER lookup_table_operation_rejects_retired_legacy
    BEFORE INSERT OR UPDATE ON loyal_yield.lookup_table_operations
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_retired_legacy_lookup_table_reference();

-- A rollout reversal would make a retired table selectable in policy even
-- though the account is being deactivated or closed. Block that reversal
-- durably instead of holding a PostgreSQL lock across RPC confirmation.
CREATE OR REPLACE FUNCTION loyal_yield.guard_rollout_during_legacy_cleanup()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    affected_cluster TEXT;
    unsafe_rollout BOOLEAN;
BEGIN
    IF TG_OP = 'DELETE' THEN
        affected_cluster := OLD.cluster;
        unsafe_rollout := TRUE;
    ELSE
        affected_cluster := NEW.cluster;
        unsafe_rollout := NEW.rollout_mode <> 'reusable_only' OR NEW.force_legacy;
    END IF;
    PERFORM pg_advisory_xact_lock(
        hashtextextended('reusable-alt-rollout:' || affected_cluster, 0)
    );
    IF unsafe_rollout AND EXISTS (
        SELECT 1
        FROM loyal_yield.route_lookup_tables route_table
        WHERE route_table.cluster = affected_cluster
          AND route_table.family_id IS NULL
          AND route_table.legacy_import_run_id IS NOT NULL
          AND route_table.status IN ('retiring', 'deactivated')
    ) THEN
        RAISE EXCEPTION 'rollout cannot leave reusable-only while imported legacy cleanup is active';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_rollout_fenced_by_legacy_cleanup
    ON loyal_yield.lookup_table_rollout_controls;
CREATE TRIGGER lookup_table_rollout_fenced_by_legacy_cleanup
    BEFORE INSERT OR UPDATE OR DELETE ON loyal_yield.lookup_table_rollout_controls
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_rollout_during_legacy_cleanup();
