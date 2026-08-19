#!/usr/bin/env bash
set -euo pipefail

verifier_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
routing_root=""
app_root=""
scratch_dir="$(mktemp -d "/tmp/smart-account-laserstream-verifier.XXXXXX")"
data_dir="$scratch_dir/postgres"
socket_dir="$scratch_dir/socket"
request_log="$scratch_dir/subscribe-request.json"
postgres_log="$scratch_dir/postgres.log"
database_name="smart_account_laserstream_verify"
port="$((57432 + RANDOM % 1000))"
server_started=0
vault_a="Config1111111111111111111111111111111111111"
vault_b="BPFLoaderUpgradeab1e11111111111111111111111"
balance_only="11111111111111111111111111111111"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

pass_condition() {
  echo "PASS: $*"
}

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    "$pg_bindir/pg_ctl" -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

while [[ $# -gt 0 ]]; do
  case "$1" in
    --routing-root)
      routing_root="${2:-}"
      shift 2
      ;;
    --app-root)
      app_root="${2:-}"
      shift 2
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n "$routing_root" ]] || fail "--routing-root is required"
[[ -n "$app_root" ]] || fail "--app-root is required"
routing_root="$(cd "$routing_root" && pwd)"
app_root="$(cd "$app_root" && pwd)"
[[ "$routing_root" != "$verifier_root" ]] || fail "routing implementation must be a separate worktree"
[[ "$app_root" != "$verifier_root" ]] || fail "app implementation must be a separate worktree"

if [[ -x /opt/homebrew/opt/postgresql@17/bin/postgres ]]; then
  pg_bindir=/opt/homebrew/opt/postgresql@17/bin
else
  pg_bindir="$(pg_config --bindir)"
fi

for command_name in cargo bun jq rg git; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done
for postgres_command in initdb pg_ctl psql; do
  [[ -x "$pg_bindir/$postgres_command" ]] || fail "$postgres_command is required"
done

e2e_source="$routing_root/crates/balance-sweep-ata-monitor/src/bin/smart-account-laserstream-e2e.rs"
consumer_source="$app_root/apps/web/src/lib/yield-optimization/earn-reconciliation-job.server.ts"
consumer_test="$app_root/apps/web/src/lib/yield-optimization/earn-reconciliation-job.server.test.ts"
consumer_worker="$app_root/apps/web/scripts/earn-reconciliation-worker.ts"
[[ -f "$e2e_source" ]] || fail "production-backed E2E binary is missing"
[[ -f "$consumer_source" ]] || fail "targeted loyal-app job consumer is missing"
[[ -f "$consumer_test" ]] || fail "targeted loyal-app consumer test is missing"
[[ -f "$consumer_worker" ]] || fail "long-lived loyal-app job worker is missing"
rg --quiet --fixed-strings 'processEarnReconciliationJob' "$consumer_worker" ||
  fail "long-lived loyal-app worker does not invoke the durable-job consumer"
if jq -e '.crons[]?.path | select(. == "/api/cron/earn-deposit-reconcile" or . == "/api/cron/earn-cleanup-reconcile")' \
  "$app_root/apps/web/vercel.json" >/dev/null; then
  fail "superseded Earn reconciliation cron schedule is still enabled"
fi

for channel in \
  balance_sweep_wallet_atas \
  earn_policy_accounts \
  earn_vault_accounts \
  earn_idle_token_accounts \
  earn_obligations; do
  rg --quiet --fixed-strings "$channel" \
    "$routing_root/crates/balance-sweep-ata-monitor/src" ||
    fail "missing production channel: $channel"
done
if rg --quiet --fixed-strings 'earn_smart_account_transactions' \
  "$routing_root/crates/balance-sweep-ata-monitor/src"; then
  fail "redundant smart-account transaction subscription is still present"
fi
if rg --quiet --fixed-strings 'earn_reserves' \
  "$routing_root/crates/balance-sweep-ata-monitor/src"; then
  fail "shared reserve updates must not be an Earn subscription trigger"
fi
new_crate_diff="$(git -C "$routing_root" diff --unified=0 origin/main -- crates)"
if rg --quiet 'smart_account_chain_events|catalog_generation' <<<"$new_crate_diff"; then
  fail "implementation contains the rejected generic event-sourcing layers"
fi
for production_wiring in \
  load_earn_subscription_targets \
  spawn_with_updates \
  persist_normalized_earn_update \
  'handle.write('; do
  rg --quiet --fixed-strings "$production_wiring" \
    "$routing_root/crates/balance-sweep-ata-monitor/src/main.rs" \
    "$routing_root/crates/balance-sweep-ata-monitor/src/lib.rs" ||
    fail "production monitor is not wired through $production_wiring"
done
if rg --quiet --fixed-strings 'watch_set: None' \
  "$routing_root/crates/balance-sweep-ata-monitor/src/main.rs"; then
  fail "production monitor still disables the Earn watch set"
fi
pass_condition "minimal production channel surface"

mkdir -p "$socket_dir"
"$pg_bindir/initdb" -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
if ! "$pg_bindir/pg_ctl" -D "$data_dir" -l "$postgres_log" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1" \
  -w start >/dev/null; then
  tail -80 "$postgres_log" >&2 || true
  fail "isolated PostgreSQL failed to start"
fi
server_started=1

"$pg_bindir/psql" -X --set=ON_ERROR_STOP=1 \
  --host="$socket_dir" --port="$port" --username="$(id -un)" \
  --dbname=postgres --command="CREATE DATABASE $database_name" >/dev/null
database_url="postgresql://$(id -un)@127.0.0.1:${port}/${database_name}"

echo "== Apply production Yield migrations"
(
  cd "$routing_root"
  NEON_DATABASE_URL="$database_url" NO_DNA=1 \
    cargo run --quiet -p loyal-yield-orchestrator --bin yield-migrations -- --apply
  NEON_DATABASE_URL="$database_url" NO_DNA=1 \
    cargo run --quiet -p loyal-yield-orchestrator --bin yield-migrations -- --check
)

echo "== Exercise production watch-list discovery"
"$pg_bindir/psql" -X --set=ON_ERROR_STOP=1 \
  --host="$socket_dir" --port="$port" --username="$(id -un)" \
  --dbname="$database_name" <<'SQL' >/dev/null
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE app_users (
  id UUID PRIMARY KEY,
  subject_address TEXT NOT NULL
);
CREATE TABLE app_user_smart_accounts (
  user_id UUID NOT NULL,
  solana_env TEXT NOT NULL,
  settings_pda TEXT NOT NULL,
  state TEXT NOT NULL
);
CREATE TABLE loyal_yield.earn_deposit_onboarding_attempts (
  wallet_address TEXT NOT NULL,
  settings TEXT NOT NULL,
  vault_index SMALLINT NOT NULL,
  vault_pubkey TEXT NOT NULL,
  policy_account TEXT,
  setup_policy_account TEXT,
  market TEXT,
  status TEXT NOT NULL
);
CREATE TABLE loyal_yield.user_yield_positions (
  wallet_address TEXT NOT NULL,
  settings TEXT NOT NULL,
  vault_index SMALLINT NOT NULL,
  vault_pubkey TEXT NOT NULL,
  policy_account TEXT,
  current_market TEXT,
  status TEXT NOT NULL
);
INSERT INTO app_users (id, subject_address)
VALUES ('00000000-0000-0000-0000-000000000001', '11111111111111111111111111111111');
INSERT INTO app_user_smart_accounts (user_id, solana_env, settings_pda, state)
VALUES (
  '00000000-0000-0000-0000-000000000001',
  'mainnet',
  'Config1111111111111111111111111111111111111',
  'ready'
);
SQL
(
  cd "$routing_root"
  NO_DNA=1 cargo run --quiet -p balance-sweep-ata-monitor \
    --bin smart-account-laserstream-e2e -- \
    --postgres-url "$database_url" \
    --stream-name earn-watch-list-verification \
    --environment mainnet \
    --events /dev/null \
    --request-output "$scratch_dir/database-watch-request.json"
)
jq -e '
  (.accounts.earn_vault_accounts | length) == 1 and
  (.accounts.earn_idle_token_accounts | length) > 0 and
  (.accounts.earn_obligations | length) > 0 and
  .transactions == {}
' "$scratch_dir/database-watch-request.json" >/dev/null ||
  fail "production database watch-list discovery did not produce Earn bindings"
pass_condition "production database watch-list discovery"

run_fixture() {
  local events_file="$1"
  (
    cd "$routing_root"
    NO_DNA=1 cargo run --quiet -p balance-sweep-ata-monitor \
      --bin smart-account-laserstream-e2e -- \
      --postgres-url "$database_url" \
      --stream-name earn-smart-account-verification \
      --watch-set "$verifier_root/fixtures/watch-set.json" \
      --events "$events_file" \
      --request-output "$request_log"
  )
}

sql_scalar() {
  "$pg_bindir/psql" -X -A -t --set=ON_ERROR_STOP=1 \
    --host="$socket_dir" --port="$port" --username="$(id -un)" \
    --dbname="$database_name" --command="$1" | tr -d '[:space:]'
}

assert_scalar() {
  local expected="$1"
  local sql="$2"
  local description="$3"
  local actual
  actual="$(sql_scalar "$sql")"
  [[ "$actual" == "$expected" ]] ||
    fail "$description: expected '$expected', got '$actual'"
  pass_condition "$description"
}

echo "== Process simulated chain activity"
run_fixture "$verifier_root/fixtures/phase-1.ndjson"

jq -e '
  .request_count == 1 and
  .commitment == "confirmed" and
  (.accounts | keys | sort) == ([
    "balance_sweep_wallet_atas",
    "earn_idle_token_accounts",
    "earn_obligations",
    "earn_policy_accounts",
    "earn_vault_accounts"
  ] | sort) and
  .transactions == {} and
  ([.accounts[] | length] | all(. > 0)) and
  ([.accounts[] | . == (sort | unique)] | all) and
  (.accounts.earn_reserves == null)
' "$request_log" >/dev/null || fail "captured SubscribeRequest shape is unsafe or incomplete"
pass_condition "captured multi-channel SubscribeRequest"

assert_scalar "2" \
  "SELECT count(*) FROM loyal_yield.earn_reconciliation_jobs" \
  "exactly two affected-vault jobs"
assert_scalar "100" \
  "SELECT highest_trigger_slot FROM loyal_yield.earn_reconciliation_jobs WHERE vault_pubkey = '$vault_a'" \
  "policy creation queued vault A"
assert_scalar "110" \
  "SELECT highest_trigger_slot FROM loyal_yield.earn_reconciliation_jobs WHERE vault_pubkey = '$vault_b'" \
  "out-of-order vault B signals coalesced at highest slot"
assert_scalar "sig-deposit-b" \
  "SELECT latest_signature FROM loyal_yield.earn_reconciliation_jobs WHERE vault_pubkey = '$vault_b'" \
  "highest-slot deposit signature retained"
assert_scalar "0" \
  "SELECT count(*) FROM loyal_yield.earn_reconciliation_jobs WHERE vault_pubkey = '$balance_only'" \
  "balance-sweep-only update isolated from Earn"
assert_scalar "4" \
  "SELECT count(*) FROM loyal_yield.earn_reconciliation_receipts" \
  "four unique Earn receipts before policy closure"
assert_scalar "110" \
  "SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name = 'earn-smart-account-verification'" \
  "cursor follows durable coalesced jobs"

echo "== Prove transaction rollback before cursor advance"
before_state="$(sql_scalar "SELECT count(*) || ':' || COALESCE(max(highest_trigger_slot), 0) FROM loyal_yield.earn_reconciliation_jobs")"
if (
  export SMART_ACCOUNT_E2E_FAIL_BEFORE_COMMIT_EVENT_KEY=policy-a-closed
  run_fixture "$verifier_root/fixtures/phase-2.ndjson"
); then
  fail "fault-injected fixture unexpectedly succeeded"
fi
assert_scalar "$before_state" \
  "SELECT count(*) || ':' || COALESCE(max(highest_trigger_slot), 0) FROM loyal_yield.earn_reconciliation_jobs" \
  "failed transaction left jobs unchanged"
assert_scalar "4" \
  "SELECT count(*) FROM loyal_yield.earn_reconciliation_receipts" \
  "failed transaction left receipts unchanged"
assert_scalar "110" \
  "SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name = 'earn-smart-account-verification'" \
  "failed transaction did not advance cursor"

echo "== Retry policy closure and replay every event"
run_fixture "$verifier_root/fixtures/phase-2.ndjson"
run_fixture "$verifier_root/fixtures/phase-1.ndjson"
assert_scalar "120" \
  "SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name = 'earn-smart-account-verification'" \
  "older replay did not lower durable cursor"
assert_scalar "110" \
  "SELECT highest_trigger_slot FROM loyal_yield.earn_reconciliation_jobs WHERE vault_pubkey = '$vault_b'" \
  "older replay did not lower vault B high-water slot"
assert_scalar "sig-deposit-b" \
  "SELECT latest_signature FROM loyal_yield.earn_reconciliation_jobs WHERE vault_pubkey = '$vault_b'" \
  "older replay did not replace vault B signature"
run_fixture "$verifier_root/fixtures/phase-2.ndjson"

assert_scalar "2" \
  "SELECT count(*) FROM loyal_yield.earn_reconciliation_jobs" \
  "replay did not duplicate jobs"
assert_scalar "5" \
  "SELECT count(*) FROM loyal_yield.earn_reconciliation_receipts" \
  "replay did not duplicate receipts"
assert_scalar "120" \
  "SELECT highest_trigger_slot FROM loyal_yield.earn_reconciliation_jobs WHERE vault_pubkey = '$vault_a'" \
  "policy deletion advanced vault A"
assert_scalar "sig-policy-close-a" \
  "SELECT latest_signature FROM loyal_yield.earn_reconciliation_jobs WHERE vault_pubkey = '$vault_a'" \
  "policy deletion signature retained"
assert_scalar "120" \
  "SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name = 'earn-smart-account-verification'" \
  "durable cursor remained monotonic after restart replay"
assert_scalar "0" \
  "SELECT count(*) FROM loyal_yield.balance_sweep_wallet_balance_events" \
  "Earn fixture path created no balance-sweep wallet events"
assert_scalar "0" \
  "SELECT count(*) FROM loyal_yield.balance_sweep_surplus_lots" \
  "Earn fixture path created no balance-sweep lots"

echo "== Run focused production checks"
(
  cd "$routing_root"
  NO_DNA=1 cargo fmt --all -- --check
  NO_DNA=1 cargo test -p balance-sweep-ata-monitor
  NO_DNA=1 cargo check -p balance-sweep-ata-monitor -p loyal-yield-store \
    -p loyal-yield-orchestrator --bin yield-migrations
  git diff --check
)
(
  cd "$app_root/apps/web"
  bun test src/lib/yield-optimization/earn-reconciliation-job.server.test.ts
  git diff --check
)

echo "PASS: smart-account LaserStream produces durable idempotent per-vault reconciliation jobs"
