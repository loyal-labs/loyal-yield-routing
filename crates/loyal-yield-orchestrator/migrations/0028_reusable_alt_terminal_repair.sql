-- Fenced repair lineage for terminal reusable-ALT operations.
--
-- Terminal operations remain immutable evidence. A repair either quarantines
-- an empty phantom physical table or inserts one new immutable successor for
-- a failed suffix after finalized no-effect proof. The repair audit and its
-- dependency links are append-only so historical failures remain inspectable
-- without continuing to poison the unresolved-failure alert surface.

ALTER TABLE loyal_yield.lookup_table_operations
    ADD COLUMN IF NOT EXISTS attempt_generation BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS retry_of_operation_id BIGINT
        REFERENCES loyal_yield.lookup_table_operations(id);

CREATE UNIQUE INDEX IF NOT EXISTS lookup_table_operations_retry_of_uidx
    ON loyal_yield.lookup_table_operations (retry_of_operation_id)
    WHERE retry_of_operation_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS lookup_table_operations_retry_lineage_idx
    ON loyal_yield.lookup_table_operations
        (route_lookup_table_id, attempt_generation, id);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.lookup_table_operations'::regclass
          AND conname = 'lookup_table_operations_attempt_generation_check'
    ) THEN
        ALTER TABLE loyal_yield.lookup_table_operations
            ADD CONSTRAINT lookup_table_operations_attempt_generation_check
            CHECK (
                attempt_generation > 0
                AND (
                    (retry_of_operation_id IS NULL AND attempt_generation = 1)
                    OR (retry_of_operation_id IS NOT NULL AND attempt_generation > 1)
                )
            );
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_operation_retry_lineage()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    predecessor loyal_yield.lookup_table_operations%ROWTYPE;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.attempt_generation IS DISTINCT FROM OLD.attempt_generation
           OR NEW.retry_of_operation_id IS DISTINCT FROM OLD.retry_of_operation_id
        THEN
            RAISE EXCEPTION 'lookup-table operation retry lineage is immutable';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.retry_of_operation_id IS NULL THEN
        IF NEW.attempt_generation <> 1 THEN
            RAISE EXCEPTION 'root lookup-table operation attempt generation must equal one';
        END IF;
        RETURN NEW;
    END IF;

    SELECT * INTO predecessor
    FROM loyal_yield.lookup_table_operations
    WHERE id = NEW.retry_of_operation_id
    FOR SHARE;

    IF NOT FOUND
       OR predecessor.operation_state <> 'permanent_failure'
       OR NEW.attempt_generation <> predecessor.attempt_generation + 1
       OR NEW.family_id IS DISTINCT FROM predecessor.family_id
       OR NEW.route_lookup_table_id IS DISTINCT FROM predecessor.route_lookup_table_id
       OR NEW.manifest_id IS DISTINCT FROM predecessor.manifest_id
       OR NEW.binding_id IS DISTINCT FROM predecessor.binding_id
       OR NEW.operation_kind IS DISTINCT FROM predecessor.operation_kind
       OR NEW.target_generation IS DISTINCT FROM predecessor.target_generation
       OR NEW.target_shard_ordinal IS DISTINCT FROM predecessor.target_shard_ordinal
       OR NEW.mutation_epoch IS DISTINCT FROM predecessor.mutation_epoch
    THEN
        RAISE EXCEPTION 'invalid lookup-table operation retry successor';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_operation_retry_lineage_guard
    ON loyal_yield.lookup_table_operations;
CREATE TRIGGER lookup_table_operation_retry_lineage_guard
BEFORE INSERT OR UPDATE OF attempt_generation, retry_of_operation_id
ON loyal_yield.lookup_table_operations
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_lookup_table_operation_retry_lineage();

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_terminal_repairs (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    repair_kind TEXT NOT NULL,
    route_lookup_table_id BIGINT NOT NULL
        REFERENCES loyal_yield.route_lookup_tables(id),
    root_operation_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_operations(id),
    successor_operation_id BIGINT
        REFERENCES loyal_yield.lookup_table_operations(id),
    expected_control_epoch BIGINT NOT NULL,
    expected_mutation_epoch BIGINT NOT NULL,
    finalized_observed_slot BIGINT NOT NULL,
    finalized_account_state TEXT NOT NULL,
    finalized_account_owner TEXT,
    finalized_authority TEXT,
    finalized_last_extended_slot BIGINT,
    finalized_address_hash TEXT NOT NULL,
    finalized_address_count INTEGER NOT NULL,
    no_effect_evidence TEXT NOT NULL,
    no_effect_signature TEXT,
    no_effect_signature_slot BIGINT,
    reason TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_terminal_repairs_root_unique
        UNIQUE (root_operation_id),
    CONSTRAINT lookup_table_terminal_repairs_successor_unique
        UNIQUE (successor_operation_id),
    CONSTRAINT lookup_table_terminal_repairs_kind_check
        CHECK (repair_kind IN ('quarantine_phantom', 'retry_suffix')),
    CONSTRAINT lookup_table_terminal_repairs_account_state_check
        CHECK (finalized_account_state IN ('missing', 'non_lookup_table', 'active_lookup_table')),
    CONSTRAINT lookup_table_terminal_repairs_evidence_check
        CHECK (
            expected_control_epoch >= 0
            AND expected_mutation_epoch >= 0
            AND finalized_observed_slot >= 0
            AND (
                finalized_last_extended_slot IS NULL
                OR finalized_last_extended_slot BETWEEN 0 AND finalized_observed_slot - 1
            )
            AND finalized_address_count BETWEEN 0 AND 256
            AND length(finalized_address_hash) = 64
            AND (
                (finalized_account_state = 'missing'
                    AND finalized_account_owner IS NULL
                    AND finalized_authority IS NULL
                    AND finalized_last_extended_slot IS NULL
                    AND finalized_address_count = 0)
                OR
                (finalized_account_state = 'non_lookup_table'
                    AND length(btrim(finalized_account_owner)) > 0
                    AND finalized_authority IS NULL
                    AND finalized_last_extended_slot IS NULL
                    AND finalized_address_count = 0)
                OR
                (finalized_account_state = 'active_lookup_table'
                    AND length(btrim(finalized_account_owner)) > 0
                    AND length(btrim(finalized_authority)) > 0
                    AND finalized_last_extended_slot IS NOT NULL)
            )
            AND (
                (repair_kind = 'quarantine_phantom'
                    AND finalized_account_state IN ('missing', 'non_lookup_table')
                    AND successor_operation_id IS NULL)
                OR
                (repair_kind = 'retry_suffix'
                    AND finalized_account_state = 'active_lookup_table'
                    AND successor_operation_id IS NOT NULL)
            )
            AND no_effect_evidence IN ('unsigned', 'finalized_failed_signature')
            AND (
                (no_effect_evidence = 'unsigned'
                    AND no_effect_signature IS NULL
                    AND no_effect_signature_slot IS NULL)
                OR
                (no_effect_evidence = 'finalized_failed_signature'
                    AND length(btrim(no_effect_signature)) > 0
                    AND no_effect_signature_slot BETWEEN 0 AND finalized_observed_slot)
            )
            AND length(btrim(cluster)) > 0
            AND length(btrim(reason)) > 0
            AND length(btrim(updated_by)) > 0
        )
);

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_terminal_repair_operations (
    repair_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_terminal_repairs(id),
    operation_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_operations(id),
    disposition TEXT NOT NULL,
    no_effect_evidence TEXT,
    no_effect_signature TEXT,
    no_effect_signature_slot BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repair_id, operation_id),
    CONSTRAINT lookup_table_terminal_repair_operations_operation_unique
        UNIQUE (operation_id),
    CONSTRAINT lookup_table_terminal_repair_operations_disposition_check
        CHECK (disposition IN ('root', 'superseded_dependency')),
    CONSTRAINT lookup_table_terminal_repair_operations_evidence_check
        CHECK (
            (disposition = 'root'
                AND no_effect_evidence IS NULL
                AND no_effect_signature IS NULL
                AND no_effect_signature_slot IS NULL)
            OR
            (disposition = 'superseded_dependency'
                AND no_effect_evidence IN ('unsigned', 'finalized_failed_signature')
                AND (
                    (no_effect_evidence = 'unsigned'
                        AND no_effect_signature IS NULL
                        AND no_effect_signature_slot IS NULL)
                    OR
                    (no_effect_evidence = 'finalized_failed_signature'
                        AND length(btrim(no_effect_signature)) > 0
                        AND no_effect_signature_slot >= 0)
                ))
        )
);

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_terminal_repair_requests (
    repair_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_terminal_repairs(id),
    request_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_provisioning_requests(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repair_id, request_id)
);

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_terminal_repair_bindings (
    repair_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_terminal_repairs(id),
    binding_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_vault_bindings(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repair_id, binding_id)
);

CREATE OR REPLACE FUNCTION loyal_yield.reject_lookup_table_terminal_repair_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'lookup-table terminal repair audit is append-only';
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_terminal_repairs_append_only
    ON loyal_yield.lookup_table_terminal_repairs;
CREATE TRIGGER lookup_table_terminal_repairs_append_only
BEFORE UPDATE OR DELETE ON loyal_yield.lookup_table_terminal_repairs
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.reject_lookup_table_terminal_repair_mutation();

DROP TRIGGER IF EXISTS lookup_table_terminal_repair_operations_append_only
    ON loyal_yield.lookup_table_terminal_repair_operations;
CREATE TRIGGER lookup_table_terminal_repair_operations_append_only
BEFORE UPDATE OR DELETE ON loyal_yield.lookup_table_terminal_repair_operations
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.reject_lookup_table_terminal_repair_mutation();

DROP TRIGGER IF EXISTS lookup_table_terminal_repair_requests_append_only
    ON loyal_yield.lookup_table_terminal_repair_requests;
CREATE TRIGGER lookup_table_terminal_repair_requests_append_only
BEFORE UPDATE OR DELETE ON loyal_yield.lookup_table_terminal_repair_requests
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.reject_lookup_table_terminal_repair_mutation();

DROP TRIGGER IF EXISTS lookup_table_terminal_repair_bindings_append_only
    ON loyal_yield.lookup_table_terminal_repair_bindings;
CREATE TRIGGER lookup_table_terminal_repair_bindings_append_only
BEFORE UPDATE OR DELETE ON loyal_yield.lookup_table_terminal_repair_bindings
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.reject_lookup_table_terminal_repair_mutation();

COMMENT ON COLUMN loyal_yield.lookup_table_operations.attempt_generation IS
    'Immutable operation retry generation; terminal attempts are never reopened.';
COMMENT ON COLUMN loyal_yield.lookup_table_operations.retry_of_operation_id IS
    'Immediate permanent-failure predecessor for a fenced no-effect retry.';
