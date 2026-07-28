#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

for command in initdb pg_ctl createdb psql cargo rg git; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "FAIL environment: missing required command $command" >&2
    exit 1
  fi
done

VERIFY_TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/reusable-alt-inflight-verify.XXXXXX")"
VERIFY_PGDATA="$VERIFY_TMP_DIR/postgres"
VERIFY_LOG="$VERIFY_TMP_DIR/postgres.log"
VERIFY_PORT=$((54000 + ($$ % 1000)))
VERIFY_DATABASE="reusable_alt_inflight_$$_local"
VERIFY_DATABASE_URL="postgresql://postgres@127.0.0.1:${VERIFY_PORT}/${VERIFY_DATABASE}"
VERIFY_SERVER_STARTED=0

cleanup() {
  if [[ "$VERIFY_SERVER_STARTED" == "1" ]]; then
    pg_ctl -D "$VERIFY_PGDATA" -m fast stop >/dev/null 2>&1 || true
  fi
  case "$VERIFY_TMP_DIR" in
    "${TMPDIR:-/tmp}"/reusable-alt-inflight-verify.*)
      rm -rf -- "$VERIFY_TMP_DIR"
      ;;
    *)
      echo "refusing unexpected verifier cleanup path: $VERIFY_TMP_DIR" >&2
      ;;
  esac
}
trap cleanup EXIT

initdb -D "$VERIFY_PGDATA" -A trust -U postgres >/dev/null
pg_ctl -D "$VERIFY_PGDATA" \
  -l "$VERIFY_LOG" \
  -o "-h 127.0.0.1 -p $VERIFY_PORT" \
  start >/dev/null
VERIFY_SERVER_STARTED=1
createdb -h 127.0.0.1 -p "$VERIFY_PORT" -U postgres "$VERIFY_DATABASE"
echo "PASS isolated_database_started"

NEON_DATABASE_URL="$VERIFY_DATABASE_URL" NO_DNA=1 \
  cargo run -q -p loyal-yield-orchestrator --bin yield-migrations --
psql "$VERIFY_DATABASE_URL" -v ON_ERROR_STOP=1 -c \
  "DROP INDEX loyal_yield.lookup_table_vault_bindings_one_inflight_idx" \
  >/dev/null
psql "$VERIFY_DATABASE_URL" \
  -f scripts/verify-reusable-alt-inflight-binding-fixture.sql \
  >/dev/null

duplicate_groups="$(psql "$VERIFY_DATABASE_URL" -Atqc "
  SELECT count(*)
  FROM (
      SELECT 1
      FROM loyal_yield.lookup_table_vault_bindings
      WHERE lifecycle_state IN ('preparing', 'warming')
      GROUP BY vault_id, family_id, binding_ordinal
      HAVING count(*) > 1
  ) duplicate
")"
if [[ "$duplicate_groups" != "2" ]]; then
  echo "FAIL duplicate_reproduction: expected 2 fixture groups, found $duplicate_groups" >&2
  exit 1
fi
diagnostic="$(psql "$VERIFY_DATABASE_URL" -Atqc "
  SELECT format(
      'vault %s has multiple in-flight bindings for manifest %s',
      binding.vault_id,
      binding.manifest_id
  )
  FROM loyal_yield.lookup_table_vault_bindings binding
  JOIN loyal_yield.lookup_table_manifests manifest
    ON manifest.id = binding.manifest_id
  WHERE manifest.subject_key = 'planner-duplicate'
    AND binding.lifecycle_state IN ('preparing', 'warming')
  GROUP BY binding.vault_id, binding.manifest_id
  HAVING count(*) > 1
")"
if [[ "$diagnostic" != vault\ *" has multiple in-flight bindings for manifest "* ]]; then
  echo "FAIL duplicate_reproduction: diagnostic did not match production invariant" >&2
  exit 1
fi
echo "PASS duplicate_failure_reproduced: $diagnostic"

REUSABLE_ALT_INFLIGHT_VERIFY_ISOLATED=1 \
  NEON_DATABASE_URL="$VERIFY_DATABASE_URL" \
  NO_DNA=1 \
  cargo run -q -p loyal-yield-orchestrator \
    --bin verify-reusable-alt-inflight-binding-repair

repair_stale_id="$(psql "$VERIFY_DATABASE_URL" -Atqc "
  SELECT binding.id
  FROM loyal_yield.lookup_table_vault_bindings binding
  JOIN loyal_yield.lookup_table_manifests manifest
    ON manifest.id = binding.manifest_id
  WHERE manifest.subject_key = 'sql-repair-duplicate'
    AND binding.lifecycle_state IN ('preparing', 'warming')
  ORDER BY binding.created_at, binding.id
  LIMIT 1
")"
canonical_before="$(psql "$VERIFY_DATABASE_URL" -Atqc "
  SELECT concat_ws(
      '|',
      binding.id,
      operation.id,
      operation.transaction_signature,
      operation.finalized_slot,
      operation.reconciled_slot,
      operation.completed_at
  )
  FROM loyal_yield.lookup_table_vault_bindings binding
  JOIN loyal_yield.lookup_table_manifests manifest
    ON manifest.id = binding.manifest_id
  JOIN loyal_yield.lookup_table_operations operation
    ON operation.binding_id = binding.id
  WHERE manifest.subject_key = 'sql-repair-duplicate'
    AND operation.operation_state = 'complete'
")"

first_repair_output="$(psql "$VERIFY_DATABASE_URL" \
  -v expected_group_count=1 \
  -v expected_stale_binding_ids="$repair_stale_id" \
  -f scripts/repair-reusable-alt-inflight-bindings.sql \
  -At)"
if ! rg -q '^1$' <<<"$first_repair_output"; then
  echo "FAIL guarded_sql_repair: first run did not update exactly one row" >&2
  exit 1
fi
second_repair_output="$(psql "$VERIFY_DATABASE_URL" \
  -v expected_group_count=1 \
  -v expected_stale_binding_ids="$repair_stale_id" \
  -f scripts/repair-reusable-alt-inflight-bindings.sql \
  -At)"
if ! rg -q '^0$' <<<"$second_repair_output"; then
  echo "FAIL guarded_sql_repair: second run was not idempotent" >&2
  exit 1
fi
canonical_after="$(psql "$VERIFY_DATABASE_URL" -Atqc "
  SELECT concat_ws(
      '|',
      binding.id,
      operation.id,
      operation.transaction_signature,
      operation.finalized_slot,
      operation.reconciled_slot,
      operation.completed_at
  )
  FROM loyal_yield.lookup_table_vault_bindings binding
  JOIN loyal_yield.lookup_table_manifests manifest
    ON manifest.id = binding.manifest_id
  JOIN loyal_yield.lookup_table_operations operation
    ON operation.binding_id = binding.id
  WHERE manifest.subject_key = 'sql-repair-duplicate'
    AND operation.operation_state = 'complete'
")"
repair_state="$(psql "$VERIFY_DATABASE_URL" -Atqc "
  SELECT lifecycle_state
  FROM loyal_yield.lookup_table_vault_bindings
  WHERE id = $repair_stale_id
")"
if [[ "$repair_state" != "failed" || "$canonical_before" != "$canonical_after" ]]; then
  echo "FAIL guarded_sql_repair: stale state or canonical evidence changed incorrectly" >&2
  exit 1
fi
echo "PASS guarded_sql_repair_exact_and_idempotent"

psql "$VERIFY_DATABASE_URL" \
  -v unsafe_only=1 \
  -f scripts/verify-reusable-alt-inflight-binding-fixture.sql \
  >/dev/null
unsafe_stale_id="$(psql "$VERIFY_DATABASE_URL" -Atqc "
  SELECT binding.id
  FROM loyal_yield.lookup_table_vault_bindings binding
  JOIN loyal_yield.lookup_table_manifests manifest
    ON manifest.id = binding.manifest_id
  WHERE manifest.subject_key = 'unsafe-duplicate'
  ORDER BY binding.created_at, binding.id
  LIMIT 1
")"
if psql "$VERIFY_DATABASE_URL" \
  -v expected_group_count=1 \
  -v expected_stale_binding_ids="$unsafe_stale_id" \
  -f scripts/repair-reusable-alt-inflight-bindings.sql \
  >/dev/null 2>&1; then
  echo "FAIL guarded_sql_repair: unsafe operation-owning stale binding was accepted" >&2
  exit 1
fi
unsafe_states="$(psql "$VERIFY_DATABASE_URL" -Atqc "
  SELECT string_agg(
      binding.lifecycle_state,
      ','
      ORDER BY binding.created_at, binding.id
  )
  FROM loyal_yield.lookup_table_vault_bindings binding
  JOIN loyal_yield.lookup_table_manifests manifest
    ON manifest.id = binding.manifest_id
  WHERE manifest.subject_key = 'unsafe-duplicate'
")"
if [[ "$unsafe_states" != "preparing,preparing" ]]; then
  echo "FAIL guarded_sql_repair: unsafe repair did not roll back atomically" >&2
  exit 1
fi
echo "PASS guarded_sql_repair_unsafe_group_aborts"

REUSABLE_ALT_INFLIGHT_VERIFY_ISOLATED=1 \
  REUSABLE_ALT_INFLIGHT_VERIFY_SCENARIO=unsafe \
  NEON_DATABASE_URL="$VERIFY_DATABASE_URL" \
  NO_DNA=1 \
  cargo run -q -p loyal-yield-orchestrator \
    --bin verify-reusable-alt-inflight-binding-repair

# Turn the isolated unsafe group into a one-operation-owner signed group. The
# newer no-operation row must be failed while the signed row is returned to the
# normal operation reconciliation path.
psql "$VERIFY_DATABASE_URL" -v ON_ERROR_STOP=1 <<'SQL' >/dev/null
UPDATE loyal_yield.lookup_table_vault_bindings binding
SET lifecycle_state = 'failed',
    deactivated_at = now(),
    updated_at = now()
FROM loyal_yield.lookup_table_operations operation
WHERE operation.binding_id = binding.id
  AND operation.idempotency_key = 'unsafe-canonical-complete';

UPDATE loyal_yield.lookup_table_operations
SET operation_state = 'signed',
    transaction_signature = 'unsafe-stale-signed-signature',
    message_hash = 'unsafe-stale-signed-message',
    recent_blockhash = 'unsafe-stale-signed-blockhash',
    last_valid_block_height = 4000,
    updated_at = now()
WHERE idempotency_key = 'unsafe-stale-operation';

INSERT INTO loyal_yield.lookup_table_vault_bindings
    (vault_id, family_id, route_lookup_table_id, manifest_id,
     binding_ordinal, desired_head_revision, allocation_mode,
     reserved_capacity, lifecycle_state, created_at, updated_at)
SELECT stale.vault_id,
       stale.family_id,
       canonical.route_lookup_table_id,
       stale.manifest_id,
       stale.binding_ordinal,
       stale.desired_head_revision,
       stale.allocation_mode,
       stale.reserved_capacity,
       'preparing',
       now(),
       now()
FROM loyal_yield.lookup_table_vault_bindings stale
JOIN loyal_yield.lookup_table_operations stale_operation
  ON stale_operation.binding_id = stale.id
 AND stale_operation.idempotency_key = 'unsafe-stale-operation'
JOIN loyal_yield.lookup_table_operations canonical_operation
  ON canonical_operation.idempotency_key = 'unsafe-canonical-complete'
JOIN loyal_yield.lookup_table_vault_bindings canonical
  ON canonical.id = canonical_operation.binding_id;
SQL

REUSABLE_ALT_INFLIGHT_VERIFY_ISOLATED=1 \
  REUSABLE_ALT_INFLIGHT_VERIFY_SCENARIO=signed \
  NEON_DATABASE_URL="$VERIFY_DATABASE_URL" \
  NO_DNA=1 \
  cargo run -q -p loyal-yield-orchestrator \
    --bin verify-reusable-alt-inflight-binding-repair

psql "$VERIFY_DATABASE_URL" \
  -f crates/loyal-yield-orchestrator/migrations/0032_reusable_alt_inflight_binding_uniqueness.sql \
  >/dev/null

REUSABLE_ALT_INFLIGHT_VERIFY_ISOLATED=1 \
  REUSABLE_ALT_INFLIGHT_VERIFY_SCENARIO=repaired-terminal-successor \
  NEON_DATABASE_URL="$VERIFY_DATABASE_URL" \
  NO_DNA=1 \
  cargo run -q -p loyal-yield-orchestrator \
    --bin verify-reusable-alt-inflight-binding-repair

psql "$VERIFY_DATABASE_URL" -v ON_ERROR_STOP=1 <<'SQL' >/dev/null
DO $verify_inflight_unique$
DECLARE
    canonical loyal_yield.lookup_table_vault_bindings%ROWTYPE;
BEGIN
    SELECT binding.* INTO canonical
    FROM loyal_yield.lookup_table_vault_bindings binding
    JOIN loyal_yield.lookup_table_manifests manifest
      ON manifest.id = binding.manifest_id
    WHERE manifest.subject_key = 'sql-repair-duplicate'
      AND binding.lifecycle_state IN ('preparing', 'warming');

    BEGIN
        INSERT INTO loyal_yield.lookup_table_vault_bindings
            (vault_id, family_id, route_lookup_table_id, manifest_id,
             binding_ordinal, desired_head_revision, allocation_mode,
             reserved_capacity, lifecycle_state)
        VALUES
            (canonical.vault_id, canonical.family_id,
             canonical.route_lookup_table_id, canonical.manifest_id,
             canonical.binding_ordinal, canonical.desired_head_revision,
             canonical.allocation_mode, canonical.reserved_capacity,
             'warming');
        RAISE EXCEPTION 'partial unique index accepted a second in-flight row';
    EXCEPTION
        WHEN unique_violation THEN NULL;
    END;

    INSERT INTO loyal_yield.lookup_table_vault_bindings
        (vault_id, family_id, route_lookup_table_id, manifest_id,
         binding_ordinal, desired_head_revision, allocation_mode,
         reserved_capacity, lifecycle_state)
    VALUES
        (canonical.vault_id, canonical.family_id,
         canonical.route_lookup_table_id, canonical.manifest_id,
         canonical.binding_ordinal, canonical.desired_head_revision,
         canonical.allocation_mode, canonical.reserved_capacity,
         'failed');
END;
$verify_inflight_unique$;
SQL
echo "PASS partial_unique_index_and_terminal_exclusion"

NEON_DATABASE_URL="$VERIFY_DATABASE_URL" NO_DNA=1 \
  cargo run -q -p loyal-yield-orchestrator --bin yield-migrations -- --check

# The focused fixtures intentionally omit unrelated reusable-ALT state. Verify
# the complete schema invariants on a second fresh disposable database.
VERIFY_SCHEMA_DATABASE="${VERIFY_DATABASE}_schema"
VERIFY_SCHEMA_DATABASE_URL="postgresql://postgres@127.0.0.1:${VERIFY_PORT}/${VERIFY_SCHEMA_DATABASE}"
createdb -h 127.0.0.1 -p "$VERIFY_PORT" -U postgres "$VERIFY_SCHEMA_DATABASE"
NEON_DATABASE_URL="$VERIFY_SCHEMA_DATABASE_URL" NO_DNA=1 \
  cargo run -q -p loyal-yield-orchestrator --bin yield-migrations --
psql "$VERIFY_SCHEMA_DATABASE_URL" \
  -f scripts/verify-reusable-alt-schema.sql \
  >/dev/null

if rg -n '\bDELETE\b' scripts/repair-reusable-alt-inflight-bindings.sql >/dev/null; then
  echo "FAIL repository_integrity: repair script contains DELETE" >&2
  exit 1
fi
for required_pattern in \
  'WHERE binding_id = $1' \
  'reconcile_in_flight_vault_bindings_in_tx' \
  'reload_in_flight_vault_binding_after_conflict_in_tx' \
  "ON CONFLICT (vault_id, family_id, binding_ordinal)" \
  'lookup_table_vault_bindings_one_inflight_idx'; do
  if ! rg -F -q "$required_pattern" \
    crates/loyal-yield-orchestrator/src/lookup_tables.rs \
    crates/loyal-yield-orchestrator/migrations/0032_reusable_alt_inflight_binding_uniqueness.sql; then
    echo "FAIL repository_integrity: missing $required_pattern" >&2
    exit 1
  fi
done

NO_DNA=1 cargo fmt --all -- --check
NO_DNA=1 cargo check -p loyal-yield-orchestrator \
  --lib \
  --bin yield-migrations \
  --bin route-lookup-table-provisioner \
  --bin verify-reusable-alt-inflight-binding-repair
git diff --check HEAD --

changed_text_files=()
while IFS= read -r path; do
  [[ -n "$path" && -f "$path" ]] && changed_text_files+=("$path")
done < <(git status --porcelain=v1 --untracked-files=all | cut -c4-)
if (( ${#changed_text_files[@]} > 0 )); then
  if rg -n -U -- \
    '-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----|postgres(ql)?://[^[:space:]'"'"']+:[^@[:space:]'"'"']+@|\p{Cyrillic}' \
    "${changed_text_files[@]}" >/dev/null; then
    echo "FAIL repository_integrity: secret-like or Cyrillic text found" >&2
    exit 1
  fi
fi

echo "PASS repository_integrity_without_tests"
echo "PASS reusable_alt_inflight_binding_repair"
