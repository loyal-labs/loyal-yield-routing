#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
observation_source="$repo_root/crates/loyal-yield-orchestrator/src/fleet_orchestration/observation.rs"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

for command_name in bun cargo createdb initdb perl pg_config pg_ctl psql; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

printf '== Source contract\n'
(cd "$repo_root" && bun scripts/verify-autodeposit-fleet-idle-ownership.ts)

scratch_dir="$(mktemp -d /private/tmp/autodeposit-fleet-idle.XXXXXX)"
data_dir="$scratch_dir/postgres"
socket_dir="$scratch_dir/socket"
postgres_log="$scratch_dir/postgres.log"
port=$((57400 + ($$ % 400)))
server_started=false
cleanup() {
  if [[ "$server_started" == true ]]; then
    "$(pg_config --bindir)/pg_ctl" -D "$data_dir" -m fast stop >/dev/null 2>&1 || true
  fi
  if [[ "$scratch_dir" == /private/tmp/autodeposit-fleet-idle.* ]]; then
    rm -rf -- "$scratch_dir"
  fi
}
trap cleanup EXIT

mkdir -p "$socket_dir"
"$(pg_config --bindir)/initdb" -D "$data_dir" -A trust --no-locale >/dev/null
"$(pg_config --bindir)/pg_ctl" -D "$data_dir" -l "$postgres_log" \
  -o "-p $port -h 127.0.0.1 -k $socket_dir" start >/dev/null
server_started=true

migration22_database="fleet_verify_autodeposit_idle_m22_${$}"
current_database="fleet_verify_autodeposit_idle_current_${$}"
"$(pg_config --bindir)/createdb" -h 127.0.0.1 -p "$port" "$migration22_database"
"$(pg_config --bindir)/createdb" -h 127.0.0.1 -p "$port" "$current_database"
migration22_url="postgresql://127.0.0.1:$port/$migration22_database"
current_url="postgresql://127.0.0.1:$port/$current_database"

printf '== Migration-22 fallback query\n'
migration_files="$(find "$repo_root/crates/loyal-yield-store/migrations" -maxdepth 1 -type f -name '*.sql' | sort | head -22)"
[[ "$(printf '%s\n' "$migration_files" | wc -l | tr -d ' ')" == 22 ]] || fail "missing migration in 0001-0022 sequence"
[[ "$(printf '%s\n' "$migration_files" | tail -1)" == *'/0022_'* ]] || fail "migration-22 boundary is not the 22nd migration"
for migration in $migration_files; do
  if [[ "$(basename "$migration")" == 0013_* ]]; then
    # Match yield-migrations' blank-database compatibility rewrite while
    # retaining the immutable migration bytes on disk.
    perl -pe "s/'loyal_yield\\.user_yield_positions'::regclass/to_regclass('loyal_yield.user_yield_positions')/g; s/'loyal_yield\\.user_yield_position_holding_events'::regclass/to_regclass('loyal_yield.user_yield_position_holding_events')/g; s/'loyal_yield\\.earn_deposit_onboarding_attempts'::regclass/to_regclass('loyal_yield.earn_deposit_onboarding_attempts')/g" \
      "$migration" | psql "$migration22_url" -X -1 -v ON_ERROR_STOP=1 -q
  else
    psql "$migration22_url" -X -1 -v ON_ERROR_STOP=1 -q -f "$migration"
  fi
done
fallback_query="$scratch_dir/fallback.sql"
perl -0777 -ne '
  if (/async fn load_fleet_sources_without_queue_schema\(.*?let row_result = crate::sqlx::query\(\s*r#"(.*?)"#,/s) {
    print $1;
  }
' "$observation_source" >"$fallback_query"
[[ -s "$fallback_query" ]] || fail "could not extract migration-22 fallback SQL"
{
  printf '%s\n' 'PREPARE migration22_fallback(TEXT, TEXT[], TEXT, TEXT[], BIGINT, TIMESTAMPTZ) AS'
  cat "$fallback_query"
  printf '%s\n' ';'
  printf '%s\n' "EXECUTE migration22_fallback('signer', ARRAY['mint'], 'same_mint_kamino', ARRAY['planned'], 0, now());"
} | psql "$migration22_url" -X -v ON_ERROR_STOP=1 -q >/dev/null
printf 'PASS: the real fallback query parses and executes on migrations 1-22\n'

printf '== Current-schema planner and ownership behavior\n'
(
  cd "$repo_root"
  NEON_DATABASE_URL="$current_url" NO_DNA=1 \
    cargo run -q -p loyal-yield-orchestrator --bin yield-migrations -- --apply
)
current_query="$scratch_dir/current.sql"
perl -0777 -ne '
  if (/async fn load_fleet_sources\(.*?let row_result = crate::sqlx::query\(\s*r#"(.*?)"#,/s) {
    print $1;
  }
' "$observation_source" >"$current_query"
[[ -s "$current_query" ]] || fail "could not extract current planner SQL"
{
  printf '%s\n' 'PREPARE current_planner(TEXT, TEXT[], TEXT, TEXT[], BIGINT, TIMESTAMPTZ, TEXT, BIGINT[], BOOLEAN) AS'
  cat "$current_query"
  printf '%s\n' ';'
  printf '%s\n' "EXECUTE current_planner('signer', ARRAY['mint'], 'same_mint_kamino', ARRAY['planned'], 0, now(), 'mainnet-beta', NULL, false);"
} | psql "$current_url" -X -v ON_ERROR_STOP=1 -q >/dev/null
(
  cd "$repo_root"
  AUTODEPOSIT_FLEET_IDLE_VERIFY_DATABASE_URL="$current_url" \
    cargo test -p loyal-yield-store --test autodeposit_fleet_idle_ownership_db \
      -- --ignored --nocapture
)
printf 'PASS: current planner SQL and active/terminal/top-up ownership behavior hold\n'

printf '== Focused compile and formatting\n'
(
  cd "$repo_root"
  bun build scripts/execute-autodeposit-policy.ts --target=bun \
    --outfile "$scratch_dir/execute-autodeposit-policy.js" >/dev/null
  cargo check -p loyal-yield-store -p loyal-yield-orchestrator -p loyal-fleet-worker
  cargo fmt --all -- --check
  git diff --check
)
printf 'PASS: Autodeposit idle ownership is migration-compatible and executable\n'
