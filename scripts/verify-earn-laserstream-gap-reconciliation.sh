#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
fixture_root="$script_dir/fixtures/earn-laserstream-gap-reconciliation"
scratch_dir="$(mktemp -d "/tmp/earn-laserstream-gap-verify.XXXXXX")"
data_dir="$scratch_dir/postgres"
socket_dir="$scratch_dir/socket"
postgres_log="$scratch_dir/postgres.log"
database_name="earn_laserstream_gap_verify"
port="$((58432 + RANDOM % 700))"
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

for command_name in cargo jq pg_config; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done
pg_bindir="$(pg_config --bindir)"
for postgres_command in initdb pg_ctl psql; do
  [[ -x "$pg_bindir/$postgres_command" ]] || fail "$postgres_command is required"
done

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

psql_verify() {
  "$pg_bindir/psql" -X --set=ON_ERROR_STOP=1 \
    --host="$socket_dir" --port="$port" --username="$(id -un)" \
    --dbname="$database_name" "$@"
}

cd "$repo_root"
NEON_DATABASE_URL="$database_url" NO_DNA=1 \
  cargo run --quiet -p loyal-yield-orchestrator --bin yield-migrations -- --apply

first_report="$scratch_dir/first.json"
second_report="$scratch_dir/second.json"
request_output="$scratch_dir/request.json"

scripts/reconcile-earn-laserstream-gap.sh \
  --postgres-url "$database_url" \
  --consumer-name earn-gap-e2e \
  --watch-set "$fixture_root/watch-set.json" \
  --history-fixture "$fixture_root/history.json" \
  --from-slot 100 \
  --to-slot 150 >"$first_report"

jq -e '
  .accountsScanned == 2 and
  .successfulSignaturesInRange == 3 and
  .plannedUpdates == 3 and
  .insertedJobs == 3 and
  .existingJobs == 0 and
  .dryRun == false
' "$first_report" >/dev/null || fail "first gap scan report was not complete"

scripts/reconcile-earn-laserstream-gap.sh \
  --postgres-url "$database_url" \
  --consumer-name earn-gap-e2e \
  --watch-set "$fixture_root/watch-set.json" \
  --history-fixture "$fixture_root/history.json" \
  --from-slot 100 \
  --to-slot 150 >"$second_report"

jq -e '
  .plannedUpdates == 3 and
  .insertedJobs == 0 and
  .existingJobs == 3
' "$second_report" >/dev/null || fail "rerun was not idempotent"

cargo run --quiet -p balance-sweep-ata-monitor \
  --bin smart-account-laserstream-e2e -- \
  --postgres-url "$database_url" \
  --stream-name earn-gap-e2e \
  --watch-set "$fixture_root/watch-set.json" \
  --events "$fixture_root/empty.ndjson" \
  --chain-fixtures "$fixture_root/chain.json" \
  --request-output "$request_output"

job_state="$(psql_verify -A -t --command="
  SELECT COUNT(*)::TEXT || ':' ||
         COUNT(*) FILTER (WHERE completed_at IS NOT NULL)::TEXT || ':' ||
         COUNT(*) FILTER (WHERE completed_at IS NULL)::TEXT
  FROM loyal_yield.earn_reconciliation_jobs
  WHERE consumer_name = 'earn-gap-e2e'
" | tr -d '[:space:]')"
[[ "$job_state" == "3:3:0" ]] || fail "expected 3 completed jobs, got $job_state"

cursor_slot="$(psql_verify -A -t --command="
  SELECT durable_slot
  FROM loyal_yield.laserstream_replay_cursors
  WHERE consumer_name = 'earn-gap-e2e'
" | tr -d '[:space:]')"
[[ "$cursor_slot" == "120" ]] || fail "expected durable cursor 120, got $cursor_slot"

psql_verify <<'SQL' >/dev/null
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
CREATE TABLE loyal_yield.user_yield_positions (
  wallet_address TEXT NOT NULL,
  settings TEXT NOT NULL,
  vault_index SMALLINT NOT NULL,
  vault_pubkey TEXT NOT NULL,
  policy_account TEXT,
  current_market TEXT,
  status TEXT NOT NULL
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

INSERT INTO app_users (id, subject_address) VALUES
  ('00000000-0000-0000-0000-000000000001', 'C1tmvPwE96hxVmnrx6Q4sfHiYvTpdDH1XwMPzm91NeJw'),
  ('00000000-0000-0000-0000-000000000002', 'Stake11111111111111111111111111111111111111');
INSERT INTO app_user_smart_accounts (user_id, solana_env, settings_pda, state) VALUES
  ('00000000-0000-0000-0000-000000000001', 'verification-a', '63HpDumPa3HiR2JD4DsvH3hzNAhSoU6YRhZA7fpPB2Bg', 'closed'),
  ('00000000-0000-0000-0000-000000000002', 'verification-b', '122nN975TU6UkDWoeeTc8pK4FbmGka6X9y2ggi7eZhXv', 'closed');
INSERT INTO loyal_yield.user_yield_positions (
  wallet_address, settings, vault_index, vault_pubkey,
  policy_account, current_market, status
) VALUES
  ('C1tmvPwE96hxVmnrx6Q4sfHiYvTpdDH1XwMPzm91NeJw', '63HpDumPa3HiR2JD4DsvH3hzNAhSoU6YRhZA7fpPB2Bg', 1, 'GfF666CzUDE1s4A3FqbJoP6iWVWvLB3ShXUiXpxjX771', NULL, NULL, 'closed'),
  ('Stake11111111111111111111111111111111111111', '122nN975TU6UkDWoeeTc8pK4FbmGka6X9y2ggi7eZhXv', 1, 'HzNg46dNAFj2Bfcrs8U4oLPvAZtBr2B9jB2LLUXeVLMd', NULL, NULL, 'closed');
INSERT INTO loyal_yield.earn_deposit_onboarding_attempts (
  wallet_address, settings, vault_index, vault_pubkey,
  policy_account, setup_policy_account, market, status
) VALUES
  ('C1tmvPwE96hxVmnrx6Q4sfHiYvTpdDH1XwMPzm91NeJw', '63HpDumPa3HiR2JD4DsvH3hzNAhSoU6YRhZA7fpPB2Bg', 1, 'GfF666CzUDE1s4A3FqbJoP6iWVWvLB3ShXUiXpxjX771', NULL, NULL, NULL, 'complete'),
  ('Stake11111111111111111111111111111111111111', '122nN975TU6UkDWoeeTc8pK4FbmGka6X9y2ggi7eZhXv', 1, 'HzNg46dNAFj2Bfcrs8U4oLPvAZtBr2B9jB2LLUXeVLMd', NULL, NULL, NULL, 'complete');
SQL

environment_report="$scratch_dir/environment.json"
scripts/reconcile-earn-laserstream-gap.sh \
  --postgres-url "$database_url" \
  --environment verification-a \
  --consumer-name earn-gap-environment-e2e \
  --history-fixture "$fixture_root/environment-history.json" \
  --from-slot 100 \
  --to-slot 150 >"$environment_report"

jq -e '
  .selectedVaults == 1 and
  .plannedUpdates == 1 and
  .candidateJobs == 1 and
  .insertedJobs == 1
' "$environment_report" >/dev/null || fail "historical target scan crossed environment boundary"

environment_jobs="$(psql_verify -A -t --command="
  SELECT COUNT(*)::TEXT || ':' ||
         COUNT(*) FILTER (
           WHERE settings = '63HpDumPa3HiR2JD4DsvH3hzNAhSoU6YRhZA7fpPB2Bg'
         )::TEXT || ':' ||
         COUNT(*) FILTER (
           WHERE settings = '122nN975TU6UkDWoeeTc8pK4FbmGka6X9y2ggi7eZhXv'
         )::TEXT
  FROM loyal_yield.earn_reconciliation_jobs
  WHERE consumer_name = 'earn-gap-environment-e2e'
" | tr -d '[:space:]')"
[[ "$environment_jobs" == "1:1:0" ]] ||
  fail "expected only verification-a job, got $environment_jobs"

echo "PASS: finalized gap scan is bounded, idempotent, and drains through canonical reconciliation"
