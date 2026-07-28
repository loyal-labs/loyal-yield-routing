-- Existing duplicates must be repaired with the guarded operator script before
-- this invariant is installed. Refuse to guess which binding owns chain state.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM loyal_yield.lookup_table_vault_bindings
        WHERE lifecycle_state IN ('preparing', 'warming')
        GROUP BY vault_id, family_id, binding_ordinal
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION
            'duplicate reusable ALT in-flight bindings remain; run the guarded repair before migration 32';
    END IF;
END;
$$;

CREATE UNIQUE INDEX IF NOT EXISTS lookup_table_vault_bindings_one_inflight_idx
    ON loyal_yield.lookup_table_vault_bindings (
        vault_id,
        family_id,
        binding_ordinal
    )
    WHERE lifecycle_state IN ('preparing', 'warming');
