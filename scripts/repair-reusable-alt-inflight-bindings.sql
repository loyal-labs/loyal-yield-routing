\set ON_ERROR_STOP on

-- Required psql variables:
--
--   expected_group_count
--   expected_stale_binding_ids
--
-- Example for one explicitly reviewed group:
--
--   psql "$NEON_DATABASE_URL" \
--     -v expected_group_count=1 \
--     -v expected_stale_binding_ids=596 \
--     -f scripts/repair-reusable-alt-inflight-bindings.sql
--
-- Review the candidate query immediately before execution. This transaction
-- intentionally fails closed if the complete duplicate set or any binding
-- safety predicate changed. A repeated run with the same expected IDs is
-- idempotent and reports zero updated rows.

\if :{?expected_group_count}
\else
  \echo expected_group_count is required
  \quit 3
\endif

\if :{?expected_stale_binding_ids}
\else
  \echo expected_stale_binding_ids is required
  \quit 3
\endif

BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TEMP TABLE expected_reusable_alt_stale_bindings (
    binding_id BIGINT PRIMARY KEY
) ON COMMIT DROP;

INSERT INTO expected_reusable_alt_stale_bindings (binding_id)
SELECT value::BIGINT
FROM unnest(string_to_array(:'expected_stale_binding_ids', ',')) AS input(value)
WHERE btrim(value) <> '';

SELECT set_config(
    'loyal_yield.reusable_alt_repair_expected_group_count',
    :'expected_group_count',
    TRUE
);

DO $validate_expected_input$
DECLARE
    expected_groups INTEGER := current_setting(
        'loyal_yield.reusable_alt_repair_expected_group_count'
    )::INTEGER;
    expected_ids INTEGER;
BEGIN
    SELECT count(*) INTO expected_ids
    FROM expected_reusable_alt_stale_bindings;

    IF expected_groups <= 0 OR expected_ids <> expected_groups THEN
        RAISE EXCEPTION
            'expected_group_count % does not match % distinct expected stale binding IDs',
            expected_groups,
            expected_ids;
    END IF;
END;
$validate_expected_input$;

-- Lock every expected desired head first and then every in-flight binding for
-- those keys. The order matches normal planner ownership and avoids observing
-- a group across two revisions.
SELECT head.family_id
FROM expected_reusable_alt_stale_bindings expected
JOIN loyal_yield.lookup_table_vault_bindings stale
  ON stale.id = expected.binding_id
JOIN loyal_yield.lookup_table_vault_desired_heads head
  ON head.family_id = stale.family_id
 AND head.vault_id = stale.vault_id
 AND head.binding_ordinal = stale.binding_ordinal
ORDER BY head.family_id, head.vault_id, head.binding_ordinal
FOR UPDATE OF head;

SELECT binding.id
FROM expected_reusable_alt_stale_bindings expected
JOIN loyal_yield.lookup_table_vault_bindings stale
  ON stale.id = expected.binding_id
JOIN loyal_yield.lookup_table_vault_bindings binding
  ON binding.family_id = stale.family_id
 AND binding.vault_id = stale.vault_id
 AND binding.binding_ordinal = stale.binding_ordinal
 AND binding.lifecycle_state IN ('preparing', 'warming')
ORDER BY binding.family_id, binding.vault_id, binding.binding_ordinal,
         binding.created_at, binding.id
FOR UPDATE OF binding;

CREATE TEMP TABLE reusable_alt_inflight_repair_candidates
ON COMMIT DROP AS
WITH ranked AS (
    SELECT binding.*,
           count(*) OVER (
               PARTITION BY binding.vault_id,
                            binding.family_id,
                            binding.binding_ordinal
           ) AS group_size,
           row_number() OVER (
               PARTITION BY binding.vault_id,
                            binding.family_id,
                            binding.binding_ordinal
               ORDER BY binding.created_at, binding.id
           ) AS oldest_rank,
           row_number() OVER (
               PARTITION BY binding.vault_id,
                            binding.family_id,
                            binding.binding_ordinal
               ORDER BY binding.created_at DESC, binding.id DESC
           ) AS newest_rank
    FROM loyal_yield.lookup_table_vault_bindings binding
    WHERE binding.lifecycle_state IN ('preparing', 'warming')
), pairs AS (
    SELECT stale.id AS stale_binding_id,
           canonical.id AS canonical_binding_id,
           stale.vault_id,
           stale.family_id,
           stale.binding_ordinal,
           stale.manifest_id,
           stale.desired_head_revision
    FROM ranked stale
    JOIN ranked canonical
      ON canonical.vault_id = stale.vault_id
     AND canonical.family_id = stale.family_id
     AND canonical.binding_ordinal = stale.binding_ordinal
     AND canonical.group_size = 2
     AND canonical.newest_rank = 1
    JOIN loyal_yield.lookup_table_vault_desired_heads head
      ON head.family_id = stale.family_id
     AND head.vault_id = stale.vault_id
     AND head.binding_ordinal = stale.binding_ordinal
    WHERE stale.group_size = 2
      AND stale.oldest_rank = 1
      AND stale.manifest_id = canonical.manifest_id
      AND stale.desired_head_revision = canonical.desired_head_revision
      AND head.manifest_id = canonical.manifest_id
      AND head.desired_revision = canonical.desired_head_revision
      AND NOT EXISTS (
          SELECT 1
          FROM loyal_yield.lookup_table_operations stale_operation
          WHERE stale_operation.binding_id = stale.id
      )
      AND EXISTS (
          SELECT 1
          FROM loyal_yield.lookup_table_operations canonical_operation
          WHERE canonical_operation.binding_id = canonical.id
      )
      AND NOT EXISTS (
          SELECT 1
          FROM loyal_yield.lookup_table_operations canonical_operation
          WHERE canonical_operation.binding_id = canonical.id
            AND (
                canonical_operation.manifest_id IS DISTINCT FROM canonical.manifest_id
                OR canonical_operation.route_lookup_table_id
                     IS DISTINCT FROM canonical.route_lookup_table_id
                OR canonical_operation.operation_state <> 'complete'
                OR canonical_operation.transaction_signature IS NULL
                OR canonical_operation.message_hash IS NULL
                OR canonical_operation.recent_blockhash IS NULL
                OR canonical_operation.last_valid_block_height IS NULL
                OR canonical_operation.finalized_slot IS NULL
                OR canonical_operation.finalized_at IS NULL
                OR canonical_operation.reconciled_slot IS NULL
                OR canonical_operation.reconciled_at IS NULL
                OR canonical_operation.completed_at IS NULL
            )
      )
)
SELECT * FROM pairs;

CREATE TEMP TABLE reusable_alt_inflight_repair_result (
    updated_count INTEGER NOT NULL
) ON COMMIT DROP;

DO $repair_reusable_alt_inflight_bindings$
DECLARE
    expected_groups INTEGER := current_setting(
        'loyal_yield.reusable_alt_repair_expected_group_count'
    )::INTEGER;
    duplicate_groups INTEGER;
    candidate_groups INTEGER;
    expected_current_or_repaired INTEGER;
    changed_rows INTEGER;
BEGIN
    SELECT count(*) INTO duplicate_groups
    FROM (
        SELECT 1
        FROM loyal_yield.lookup_table_vault_bindings binding
        WHERE binding.lifecycle_state IN ('preparing', 'warming')
        GROUP BY binding.vault_id, binding.family_id, binding.binding_ordinal
        HAVING count(*) > 1
    ) duplicate;

    SELECT count(*) INTO candidate_groups
    FROM reusable_alt_inflight_repair_candidates;

    -- Every expected ID must either be a currently safe candidate or the
    -- already-failed stale half of the same still-canonical completed pair.
    SELECT count(*) INTO expected_current_or_repaired
    FROM expected_reusable_alt_stale_bindings expected
    JOIN loyal_yield.lookup_table_vault_bindings stale
      ON stale.id = expected.binding_id
    WHERE EXISTS (
        SELECT 1
        FROM reusable_alt_inflight_repair_candidates candidate
        WHERE candidate.stale_binding_id = stale.id
    ) OR (
        stale.lifecycle_state = 'failed'
        AND NOT EXISTS (
            SELECT 1
            FROM loyal_yield.lookup_table_operations stale_operation
            WHERE stale_operation.binding_id = stale.id
        )
        AND EXISTS (
            SELECT 1
            FROM loyal_yield.lookup_table_vault_bindings canonical
            JOIN loyal_yield.lookup_table_vault_desired_heads head
              ON head.family_id = canonical.family_id
             AND head.vault_id = canonical.vault_id
             AND head.binding_ordinal = canonical.binding_ordinal
            WHERE canonical.vault_id = stale.vault_id
              AND canonical.family_id = stale.family_id
              AND canonical.binding_ordinal = stale.binding_ordinal
              AND canonical.manifest_id = stale.manifest_id
              AND canonical.desired_head_revision = stale.desired_head_revision
              AND canonical.created_at > stale.created_at
              AND canonical.lifecycle_state IN ('preparing', 'warming')
              AND head.manifest_id = canonical.manifest_id
              AND head.desired_revision = canonical.desired_head_revision
              AND EXISTS (
                  SELECT 1
                  FROM loyal_yield.lookup_table_operations operation
                  WHERE operation.binding_id = canonical.id
              )
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.lookup_table_operations operation
                  WHERE operation.binding_id = canonical.id
                    AND (
                        operation.manifest_id IS DISTINCT FROM canonical.manifest_id
                        OR operation.route_lookup_table_id
                             IS DISTINCT FROM canonical.route_lookup_table_id
                        OR operation.operation_state <> 'complete'
                        OR operation.transaction_signature IS NULL
                        OR operation.message_hash IS NULL
                        OR operation.recent_blockhash IS NULL
                        OR operation.last_valid_block_height IS NULL
                        OR operation.finalized_slot IS NULL
                        OR operation.finalized_at IS NULL
                        OR operation.reconciled_slot IS NULL
                        OR operation.reconciled_at IS NULL
                        OR operation.completed_at IS NULL
                    )
              )
        )
    );

    IF expected_current_or_repaired <> expected_groups THEN
        RAISE EXCEPTION
            'expected stale binding set changed: expected %, validated %',
            expected_groups,
            expected_current_or_repaired;
    END IF;

    IF duplicate_groups <> candidate_groups
       OR candidate_groups NOT IN (0, expected_groups)
    THEN
        RAISE EXCEPTION
            'duplicate binding set changed: duplicates %, safe candidates %, expected %',
            duplicate_groups,
            candidate_groups,
            expected_groups;
    END IF;

    IF EXISTS (
        SELECT stale_binding_id
        FROM reusable_alt_inflight_repair_candidates
        EXCEPT
        SELECT binding_id
        FROM expected_reusable_alt_stale_bindings
    ) OR (
        candidate_groups = expected_groups
        AND EXISTS (
            SELECT binding_id
            FROM expected_reusable_alt_stale_bindings
            EXCEPT
            SELECT stale_binding_id
            FROM reusable_alt_inflight_repair_candidates
        )
    ) THEN
        RAISE EXCEPTION 'safe candidate IDs differ from the expected stale binding IDs';
    END IF;

    UPDATE loyal_yield.lookup_table_vault_bindings binding
    SET lifecycle_state = 'failed',
        deactivated_at = COALESCE(binding.deactivated_at, now()),
        updated_at = now()
    FROM reusable_alt_inflight_repair_candidates candidate
    WHERE binding.id = candidate.stale_binding_id
      AND binding.lifecycle_state IN ('preparing', 'warming');

    GET DIAGNOSTICS changed_rows = ROW_COUNT;
    IF changed_rows <> candidate_groups THEN
        RAISE EXCEPTION
            'repair row count changed: candidates %, updated %',
            candidate_groups,
            changed_rows;
    END IF;

    INSERT INTO reusable_alt_inflight_repair_result(updated_count)
    VALUES (changed_rows);
END;
$repair_reusable_alt_inflight_bindings$;

SELECT updated_count AS repaired_stale_binding_count
FROM reusable_alt_inflight_repair_result;

COMMIT;
