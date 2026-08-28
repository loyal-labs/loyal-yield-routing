#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module_root="$repo_root/go/laserstream-worker"
go_bin="${GO_BIN:-$(command -v go || true)}"

if [[ -z "$go_bin" || ! -x "$go_bin" ]]; then
  echo "Go 1.25.1 is required; set GO_BIN to the pinned go executable" >&2
  exit 1
fi
if [[ "$($go_bin env GOVERSION)" != "go1.25.1" ]]; then
  echo "expected Go go1.25.1" >&2
  exit 1
fi

postgres_bin="${POSTGRES_BIN:-}"
if [[ -z "$postgres_bin" ]]; then
  if command -v initdb >/dev/null 2>&1; then
    postgres_bin="$(dirname "$(command -v initdb)")"
  elif [[ -x /opt/homebrew/opt/postgresql@17/bin/initdb ]]; then
    postgres_bin=/opt/homebrew/opt/postgresql@17/bin
  else
    echo "PostgreSQL tools are required; set POSTGRES_BIN" >&2
    exit 1
  fi
fi
for executable in initdb pg_ctl createdb psql pg_isready; do
  if [[ ! -x "$postgres_bin/$executable" ]]; then
    echo "missing $postgres_bin/$executable" >&2
    exit 1
  fi
done

free_port() {
  python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    print(sock.getsockname()[1])
PY
}

scratch_dir="$(mktemp -d /tmp/go-laserstream-handoff-e2e.XXXXXX)"
data_dir="$scratch_dir/postgres"
log_file="$scratch_dir/postgres.log"
port="$(free_port)"
postgres_started=0
timescale_container=""
cleanup() {
  if [[ -n "$timescale_container" ]]; then
    podman rm -f "$timescale_container" >/dev/null 2>&1 || true
  fi
  if [[ "$postgres_started" == 1 ]]; then
    "$postgres_bin/pg_ctl" -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

"$postgres_bin/initdb" -D "$data_dir" -A trust -U postgres --no-locale -E UTF8 >/dev/null
"$postgres_bin/pg_ctl" -D "$data_dir" -l "$log_file" \
  -o "-h 127.0.0.1 -p $port -k $scratch_dir -F" -w start >/dev/null
postgres_started=1
"$postgres_bin/createdb" -h 127.0.0.1 -p "$port" -U postgres laserstream_handoff_e2e
database_url="postgresql://postgres@127.0.0.1:$port/laserstream_handoff_e2e?sslmode=disable"

timescale_url="${TEST_TIMESCALE_DATABASE_URL:-}"
if [[ -z "$timescale_url" ]]; then
  if ! command -v podman >/dev/null 2>&1; then
    echo "Podman is required for the isolated TimescaleDB E2E, or set TEST_TIMESCALE_DATABASE_URL" >&2
    exit 1
  fi
  timescale_port="$(free_port)"
  timescale_container="go-laserstream-timescale-$RANDOM-$$"
  podman run --detach --rm --name "$timescale_container" \
    --env POSTGRES_PASSWORD=postgres \
    --publish "127.0.0.1:$timescale_port:5432" \
    docker.io/timescale/timescaledb:2.20.3-pg17 >/dev/null
  for _ in $(seq 1 60); do
    if "$postgres_bin/pg_isready" -h 127.0.0.1 -p "$timescale_port" -U postgres >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  "$postgres_bin/pg_isready" -h 127.0.0.1 -p "$timescale_port" -U postgres >/dev/null
  timescale_url="postgresql://postgres:postgres@127.0.0.1:$timescale_port/postgres?sslmode=disable"
elif [[ "${ALLOW_TEST_DATABASE_RESET:-}" != "1" ]]; then
  echo "refusing external TEST_TIMESCALE_DATABASE_URL without ALLOW_TEST_DATABASE_RESET=1" >&2
  exit 1
fi

for migration in "$repo_root"/crates/loyal-timescale-migrations/migrations/*.sql; do
  "$postgres_bin/psql" "$timescale_url" -X -v ON_ERROR_STOP=1 -f "$migration" >/dev/null
done

cd "$module_root"
TEST_DATABASE_URL="$database_url" TEST_TIMESCALE_DATABASE_URL="$timescale_url" \
  "$go_bin" test -race ./... -count=1
TEST_DATABASE_URL="$database_url" \
  "$go_bin" test ./internal/stream -run 'E2E$' -count=1 -v

echo "PASS: combined Go LaserStream handoff and real-schema persistence are gap-free and idempotent"
