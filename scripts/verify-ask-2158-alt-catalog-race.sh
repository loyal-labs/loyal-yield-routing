#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scratch_dir="$(mktemp -d "${TMPDIR:-/tmp}/ask-2158-alt-catalog.XXXXXX")"
data_dir="$scratch_dir/postgres"
socket_dir="$scratch_dir/socket"
database_name="reusable_alt_ask_2158"
port="$((58432 + RANDOM % 1000))"
server_started=0

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    "$pg_bindir/pg_ctl" -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

if [[ -x /opt/homebrew/opt/postgresql@17/bin/postgres ]]; then
  pg_bindir=/opt/homebrew/opt/postgresql@17/bin
else
  pg_bindir="$(pg_config --bindir)"
fi

for command_name in cargo rg; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done
for postgres_command in initdb pg_ctl psql; do
  [[ -x "$pg_bindir/$postgres_command" ]] || fail "$postgres_command is required"
done

db_verifier="$repo_root/crates/loyal-yield-orchestrator/src/bin/verify-reusable-alt-db.rs"
provisioner="$repo_root/crates/loyal-yield-orchestrator/src/bin/route-lookup-table-provisioner.rs"

rg --quiet 'stale catalog snapshots are no-op and same-revision corruption stays fatal' \
  "$db_verifier" || fail "database verifier is missing the ASK-2158 behavioral proof"

reconcile_body="$(sed -n \
  '/^async fn reconcile_shared_market_catalog(/,/^async fn report_finalized_shared_drift_if_any(/p' \
  "$provisioner")"
rg --quiet 'reusable_only_cutover_preflight_if_current' <<<"$reconcile_body" ||
  fail "provisioner does not fence active-catalog preflight by revision"
rg --quiet 'reconcile_shared_market_catalog_head_if_current' <<<"$reconcile_body" ||
  fail "provisioner does not fence catalog reconciliation by revision"
if rg --quiet '\.reusable_only_cutover_preflight\(' <<<"$reconcile_body"; then
  fail "provisioner still uses strict preflight in the concurrent reconciliation path"
fi
if rg --quiet '\.reconcile_shared_market_catalog_head\(' <<<"$reconcile_body"; then
  fail "provisioner still turns a concurrent reconciliation revision change into an error"
fi

if rg -U --quiet \
  'retry_read_only_rpc[\s\S]{0,300}run_operation_batch|run_operation_batch[\s\S]{0,300}retry_read_only_rpc' \
  "$provisioner"; then
  fail "run_operation_batch is behind the read-only RPC retry boundary"
fi

mkdir -p "$socket_dir"
"$pg_bindir/initdb" -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
"$pg_bindir/pg_ctl" -D "$data_dir" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1" \
  -w start >/dev/null
server_started=1

"$pg_bindir/psql" -X --set=ON_ERROR_STOP=1 \
  --host="$socket_dir" --port="$port" --username="$(id -un)" \
  --dbname=postgres --command="CREATE DATABASE $database_name" >/dev/null

database_url="postgresql://$(id -un)@127.0.0.1:${port}/${database_name}"

cd "$repo_root"
echo "== Apply migrations to disposable ASK-2158 database"
NEON_DATABASE_URL="$database_url" NO_DNA=1 \
  cargo run --quiet -p loyal-yield-orchestrator --bin yield-migrations -- --apply

echo "== Run catalog revision-fence database proof"
db_log="$scratch_dir/database-verifier.log"
ASK_2158_ALT_CATALOG_VERIFY_ONLY=1 REUSABLE_ALT_DB_VERIFY_ISOLATED=1 \
  NEON_DATABASE_URL="$database_url" NO_DNA=1 \
  cargo run --quiet -p loyal-yield-orchestrator --bin verify-reusable-alt-db | tee "$db_log"
rg --quiet 'stale catalog snapshots are no-op and same-revision corruption stays fatal' \
  "$db_log" || fail "database verifier did not report the ASK-2158 proof"
rg --quiet '"result":"PASS"' "$db_log" ||
  rg --quiet '"result": "PASS"' "$db_log" ||
  fail "database verifier did not pass"

echo "== Preserve ASK-2143 RPC recovery and no-replay contract"
bash scripts/verify-ask-2143-alt-rpc-recovery.sh

echo "== Run focused formatting, compilation, and diff checks"
NO_DNA=1 cargo fmt --all -- --check
NO_DNA=1 cargo check -p loyal-route-lookup-tables -p loyal-yield-orchestrator \
  --bin route-lookup-table-provisioner --bin verify-reusable-alt-db
git diff --check

echo "PASS: ASK-2158 ALT catalog revision fence"
