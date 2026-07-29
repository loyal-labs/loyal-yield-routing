#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
migration="$repo_root/crates/loyal-yield-orchestrator/migrations/0033_idle_vault_decision_lookup_index.sql"
runner="$repo_root/crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs"
index_name="rebalance_decisions_idle_signature_id_idx"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

require_literal() {
  local file="$1"
  local literal="$2"
  local description="$3"
  rg --fixed-strings --quiet "$literal" "$file" ||
    fail "$description"
}

command -v initdb >/dev/null || fail "initdb is required"
command -v pg_ctl >/dev/null || fail "pg_ctl is required"
command -v psql >/dev/null || fail "psql is required"

[[ -f "$migration" ]] || fail "migration 0033 is missing"
require_literal "$migration" \
  "CREATE INDEX CONCURRENTLY IF NOT EXISTS $index_name" \
  "migration must create the idle-vault index concurrently"
require_literal "$migration" \
  "ON loyal_yield.rebalance_decisions (signature, id DESC)" \
  "migration must index signature followed by descending id"
require_literal "$migration" \
  "WHERE execution_plan->>'kind' = 'idle_vault_deposit';" \
  "migration must use the admin query's idle-vault partial predicate"

require_literal "$runner" \
  'version: 33,' \
  "yield-migrations must register migration 33"
require_literal "$runner" \
  'name: "idle_vault_decision_lookup_index",' \
  "yield-migrations must register the migration name"
require_literal "$runner" \
  'include_str!("../../migrations/0033_idle_vault_decision_lookup_index.sql")' \
  "yield-migrations must embed migration 33"
require_literal "$runner" \
  "DROP INDEX CONCURRENTLY loyal_yield.$index_name" \
  "yield-migrations must remove an invalid concurrent-build remnant"
require_literal "$runner" \
  "SELECT indisready, indisvalid" \
  "yield-migrations must inspect PostgreSQL index readiness and validity"
require_literal "$runner" \
  "requires migration 32 reusable_alt_inflight_binding_uniqueness" \
  "migration 33 must fail closed until PR #24's migration 32 is recorded"

if [[ -n "${ADMIN_REBALANCE_DATA_FILE:-}" ]]; then
  [[ -f "$ADMIN_REBALANCE_DATA_FILE" ]] ||
    fail "ADMIN_REBALANCE_DATA_FILE does not exist"
  require_literal "$ADMIN_REBALANCE_DATA_FILE" \
    "WHERE idle.signature = deposit.deposit_signature" \
    "admin query signature join no longer matches the index"
  require_literal "$ADMIN_REBALANCE_DATA_FILE" \
    "AND idle.execution_plan->>'kind' = 'idle_vault_deposit'" \
    "admin query predicate no longer matches the partial index"
  require_literal "$ADMIN_REBALANCE_DATA_FILE" \
    "ORDER BY idle.id DESC" \
    "admin query ordering no longer matches the index"
  require_literal "$ADMIN_REBALANCE_DATA_FILE" \
    "LIMIT 1" \
    "admin query no longer selects the latest matching decision"
else
  echo "NOTE: ADMIN_REBALANCE_DATA_FILE not set; external admin source check skipped"
fi

scratch_dir="$(mktemp -d "${TMPDIR:-/tmp}/ask-1928-index-verifier.XXXXXX")"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
mkdir -p "$socket_dir"
port="$((55432 + RANDOM % 1000))"
server_started=0

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    pg_ctl -D "$data_dir" -m fast -w stop >/dev/null
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

initdb -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
pg_ctl -D "$data_dir" \
  -o "-F -k '$socket_dir' -p $port -c allow_system_table_mods=on" \
  -w start >/dev/null
server_started=1

psql_args=(
  -X
  --set=ON_ERROR_STOP=1
  --host="$socket_dir"
  --port="$port"
  --username="$(id -un)"
  --dbname=postgres
)

psql "${psql_args[@]}" >/dev/null <<'SQL'
CREATE SCHEMA loyal_yield;

CREATE TABLE loyal_yield.schema_migrations (
  version BIGINT PRIMARY KEY,
  name TEXT NOT NULL
);

CREATE TABLE loyal_yield.rebalance_decisions (
  id BIGSERIAL PRIMARY KEY,
  signature TEXT,
  execution_plan JSONB NOT NULL
);

CREATE TABLE loyal_yield.user_yield_position_deposits (
  id BIGSERIAL PRIMARY KEY,
  deposit_signature TEXT NOT NULL
);

INSERT INTO loyal_yield.rebalance_decisions (signature, execution_plan)
SELECT
  'deposit-signature-' || decision_id,
  '{"kind":"idle_vault_deposit"}'::jsonb
FROM generate_series(1, 320) AS decision_id;

INSERT INTO loyal_yield.rebalance_decisions (signature, execution_plan)
SELECT
  'other-signature-' || decision_id,
  '{"kind":"same_mint_rebalance"}'::jsonb
FROM generate_series(1, 3301) AS decision_id;

INSERT INTO loyal_yield.user_yield_position_deposits (deposit_signature)
SELECT 'deposit-signature-' || deposit_id
FROM generate_series(1, 19620) AS deposit_id;

VACUUM ANALYZE loyal_yield.rebalance_decisions;
VACUUM ANALYZE loyal_yield.user_yield_position_deposits;
SQL

audit_lookup_sql="
SELECT COALESCE(sum(idle_decision.id), 0)
FROM loyal_yield.user_yield_position_deposits AS deposit
LEFT JOIN LATERAL (
  SELECT idle.id
  FROM loyal_yield.rebalance_decisions AS idle
  WHERE idle.signature = deposit.deposit_signature
    AND idle.execution_plan->>'kind' = 'idle_vault_deposit'
  ORDER BY idle.id DESC
  LIMIT 1
) AS idle_decision ON true"

baseline_result="$(
  psql "${psql_args[@]}" --tuples-only --no-align \
    --command="$audit_lookup_sql"
)"
baseline_plan="$scratch_dir/baseline-plan.txt"
psql "${psql_args[@]}" --no-align --tuples-only >"$baseline_plan" <<SQL
SET jit = off;
EXPLAIN (ANALYZE, BUFFERS)
$audit_lookup_sql;
SQL

psql "${psql_args[@]}" >/dev/null <<SQL
CREATE INDEX CONCURRENTLY $index_name
    ON loyal_yield.rebalance_decisions (signature, id DESC)
    WHERE execution_plan->>'kind' = 'idle_vault_deposit';

UPDATE pg_index
SET indisready = false,
    indisvalid = false
WHERE indexrelid = 'loyal_yield.$index_name'::regclass;
SQL

invalid_state="$(
  psql "${psql_args[@]}" --tuples-only --no-align --command="
    SELECT indisready, indisvalid
    FROM pg_index
    WHERE indexrelid = 'loyal_yield.$index_name'::regclass;
  "
)"
[[ "$invalid_state" == "f|f" ]] ||
  fail "failed to simulate an interrupted concurrent index build"

ASK_1928_TEST_DATABASE_URL="postgresql://$(id -un)@127.0.0.1:$port/postgres" \
  cargo test \
    -p loyal-yield-orchestrator \
    --bin yield-migrations \
    tests::invalid_idle_vault_index_is_rebuilt_before_migration_success \
    -- --exact

psql "${psql_args[@]}" --command="ANALYZE loyal_yield.rebalance_decisions" >/dev/null

rebuilt_state="$(
  psql "${psql_args[@]}" --tuples-only --no-align --command="
    SELECT indisready, indisvalid
    FROM pg_index
    WHERE indexrelid = 'loyal_yield.$index_name'::regclass;
  "
)"
[[ "$rebuilt_state" == "t|t" ]] ||
  fail "rebuilt index is not ready and valid ($rebuilt_state)"

index_definition="$(
  psql "${psql_args[@]}" --tuples-only --no-align --command="
    SELECT indexdef
    FROM pg_indexes
    WHERE schemaname = 'loyal_yield'
      AND indexname = '$index_name';
  "
)"
[[ "$index_definition" == *"(signature, id DESC)"* ]] ||
  fail "catalog index key order is incorrect"
[[ "$index_definition" == *"execution_plan ->> 'kind'::text"* ]] ||
  fail "catalog partial predicate is incorrect"
[[ "$index_definition" == *"'idle_vault_deposit'::text"* ]] ||
  fail "catalog partial predicate value is incorrect"

indexed_result="$(
  psql "${psql_args[@]}" --tuples-only --no-align \
    --command="$audit_lookup_sql"
)"
[[ "$indexed_result" == "$baseline_result" ]] ||
  fail "lookup result changed after adding the index"

indexed_plan="$scratch_dir/indexed-plan.txt"
psql "${psql_args[@]}" --no-align --tuples-only >"$indexed_plan" <<SQL
SET jit = off;
EXPLAIN (ANALYZE, BUFFERS)
$audit_lookup_sql;
SQL

rg --fixed-strings --quiet "$index_name" "$indexed_plan" ||
  fail "PostgreSQL did not use $index_name"

extract_execution_ms() {
  local plan_file="$1"
  awk '/Execution Time:/ { print $(NF - 1) }' "$plan_file" | tail -1
}

baseline_ms="$(extract_execution_ms "$baseline_plan")"
indexed_ms="$(extract_execution_ms "$indexed_plan")"
[[ -n "$baseline_ms" && -n "$indexed_ms" ]] ||
  fail "could not read EXPLAIN ANALYZE execution times"

awk -v baseline="$baseline_ms" -v indexed="$indexed_ms" \
  'BEGIN { exit !(indexed * 10 <= baseline) }' ||
  fail "indexed query was not at least 10x faster ($baseline_ms ms -> $indexed_ms ms)"
awk -v indexed="$indexed_ms" \
  'BEGIN { exit !(indexed < 500) }' ||
  fail "indexed query exceeded 500 ms ($indexed_ms ms)"

speedup="$(
  awk -v baseline="$baseline_ms" -v indexed="$indexed_ms" \
    'BEGIN { printf "%.1f", baseline / indexed }'
)"

echo "Baseline execution: ${baseline_ms} ms"
echo "Indexed execution:  ${indexed_ms} ms"
echo "Measured speedup:    ${speedup}x"
echo "PASS: ASK-1928 idle-vault index verifier"
