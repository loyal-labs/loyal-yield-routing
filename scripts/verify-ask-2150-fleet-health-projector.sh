#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scratch_dir="$(mktemp -d "${TMPDIR:-/tmp}/ask-2150-fleet-verify.XXXXXX")"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
port="$((57432 + RANDOM % 1000))"
server_started=0

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    pg_ctl -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

for command_name in cargo initdb pg_ctl psql rg; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

mkdir -p "$socket_dir"
initdb -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
pg_ctl -D "$data_dir" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1" \
  -w start >/dev/null
server_started=1

psql -X --set=ON_ERROR_STOP=1 \
  --host="$socket_dir" --port="$port" --username="$(id -un)" \
  --dbname=postgres --command='CREATE DATABASE fleet_verify_ask_2150' >/dev/null

database_url="postgresql://$(id -un)@127.0.0.1:${port}/fleet_verify_ask_2150"

echo "== Apply migrations to disposable fleet_verify database"
NEON_DATABASE_URL="$database_url" \
  cargo run --quiet -p loyal-yield-orchestrator --bin yield-migrations -- --apply

echo "== Run advisory-lock database contract"
FLEET_VERIFY_DATABASE_URL="$database_url" \
  cargo test -p loyal-yield-store --test fleet_health_projection_advisory_lock -- --nocapture

projector="$repo_root/crates/loyal-yield-orchestrator/src/bin/fleet-health-projector.rs"
queue="$repo_root/crates/loyal-yield-store/src/fleet_orchestration/queue.rs"
render="$repo_root/render.yaml"
release_verifier="$repo_root/scripts/verify-cross-mint-render-release.ts"

echo "== Check runtime and deployment wiring"
rg --quiet 'pg_try_advisory_xact_lock' "$queue" ||
  fail "store refresh does not use pg_try_advisory_xact_lock"
rg --quiet 'FleetHealthSnapshotProjection::Busy' "$projector" ||
  fail "projector does not handle Busy as a normal outcome"
rg --quiet 'FleetHealthSnapshotProjection::NotDue' "$projector" ||
  fail "projector does not handle NotDue as a normal outcome"
if rg --quiet 'claim_fleet_health_projection_lease|FleetHealthProjectionLease' "$projector" "$queue"; then
  fail "runtime still depends on the TTL lease protocol"
fi
if sed '/^#\[cfg(test)\]/,$d' "$projector" |
    rg --quiet -- '--lease-seconds|std::process::id\(\)'; then
  fail "projector runtime still contains lease TTL or PID-derived ownership"
fi
if rg --quiet -- '--lease-seconds' "$render" "$release_verifier"; then
  fail "runtime/config still contains lease TTL or PID-derived ownership"
fi

echo "== Compile and formatting checks"
cargo test -p loyal-yield-orchestrator --bin fleet-health-projector
cargo check -p loyal-yield-orchestrator --bin fleet-health-projector
cargo fmt --all -- --check
git -C "$repo_root" diff --check

echo "PASS: ASK-2150 fleet health projector advisory-lock verifier"
