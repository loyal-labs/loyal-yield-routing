#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

for command_name in bun cargo initdb pg_ctl createdb; do
  command -v "$command_name" >/dev/null || {
    echo "FAIL: $command_name is required" >&2
    exit 1
  }
done

runtime_tmp_root="${AUTODEPOSIT_SELECTOR_RUNTIME_TMPDIR:-/tmp}"
scratch_dir="$(mktemp -d "$runtime_tmp_root/autodeposit-selector-runtime.XXXXXX")"
data_dir="$scratch_dir/postgres"
socket_dir="$scratch_dir/socket"
mkdir -p "$socket_dir"
port="$((59500 + RANDOM % 400))"
server_started=0

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    pg_ctl -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  if [[ "$scratch_dir" == "$runtime_tmp_root/autodeposit-selector-runtime."* ]]; then
    rm -rf "$scratch_dir"
  fi
}
trap cleanup EXIT

cargo build --locked \
  -p loyal-yield-orchestrator --bin yield-migrations \
  -p balance-sweep-autodeposit-trigger --bin balance-sweep-autodeposit-trigger

initdb -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
pg_ctl -D "$data_dir" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1" \
  -w start >/dev/null
server_started=1
createdb -h "$socket_dir" -p "$port" autodeposit_selector
database_url="postgresql://$(id -un)@127.0.0.1:$port/autodeposit_selector"

NEON_DATABASE_URL="$database_url" target/debug/yield-migrations --apply >/dev/null
AUTODEPOSIT_SELECTOR_DATABASE_URL="$database_url" \
AUTODEPOSIT_SELECTOR_TRIGGER_BINARY="$repo_root/target/debug/balance-sweep-autodeposit-trigger" \
  bun scripts/verify-autodeposit-release-selector.ts
