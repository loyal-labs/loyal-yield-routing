#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

routing_root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
[[ -e "$routing_root/.git" ]] || fail "routing root is not a Git worktree"

for command_name in cargo createdb initdb pg_config pg_ctl; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

migration="$routing_root/crates/loyal-yield-store/migrations/0069_autodeposit_event_id_ranges.sql"
db_test="$routing_root/crates/loyal-yield-store/tests/autodeposit_event_id_ranges_db.rs"
[[ -f "$migration" ]] || fail "migration 0069 is missing"
[[ -f "$db_test" ]] || fail "event ID range database test is missing"

scratch_dir="$(mktemp -d /private/tmp/autodeposit-event-ids.XXXXXX)"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
postgres_log="$scratch_dir/postgres.log"
database_name="autodeposit_event_ids_${$}"
port=$((57000 + ($$ % 500)))
started=false
cleanup() {
  if [[ "$started" == true ]]; then
    "$(pg_config --bindir)/pg_ctl" -D "$data_dir" -m fast stop >/dev/null 2>&1 || true
  fi
  if [[ "$scratch_dir" == /private/tmp/autodeposit-event-ids.* ]]; then
    rm -r -- "$scratch_dir"
  fi
}
trap cleanup EXIT

mkdir -p "$socket_dir"
"$(pg_config --bindir)/initdb" -D "$data_dir" -A trust --no-locale >/dev/null
"$(pg_config --bindir)/pg_ctl" -D "$data_dir" -l "$postgres_log" \
  -o "-p $port -h 127.0.0.1 -k $socket_dir" start >/dev/null
started=true
"$(pg_config --bindir)/createdb" -h 127.0.0.1 -p "$port" "$database_name"
database_url="postgresql://127.0.0.1:$port/$database_name"

(
  cd "$routing_root"
  NEON_DATABASE_URL="$database_url" NO_DNA=1 \
    cargo run -q -p loyal-yield-orchestrator --bin yield-migrations -- --apply
  ASK_2211_VERIFY_DATABASE_URL="$database_url" \
    cargo test -p loyal-yield-store --test autodeposit_event_id_ranges_db \
      -- --ignored --nocapture
  cargo fmt --all -- --check
  git diff --check
)

printf 'PASS: synthetic Autodeposit event ID ranges do not collide\n'
