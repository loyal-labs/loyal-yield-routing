#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module_root="$repo_root/go/laserstream-worker"
go_bin="${GO_BIN:-$(command -v go || true)}"

if [[ -z "$go_bin" || ! -x "$go_bin" ]]; then
  echo "Go 1.25.1 is required; set GO_BIN to the pinned go executable" >&2
  exit 1
fi

actual_version="$($go_bin env GOVERSION)"
if [[ "$actual_version" != "go1.25.1" ]]; then
  echo "expected Go go1.25.1, found $actual_version" >&2
  exit 1
fi

postgres_bin="${POSTGRES_BIN:-}"
if [[ -z "$postgres_bin" ]]; then
  if command -v initdb >/dev/null 2>&1; then
    postgres_bin="$(dirname "$(command -v initdb)")"
  elif [[ -x /opt/homebrew/opt/postgresql@17/bin/initdb ]]; then
    postgres_bin=/opt/homebrew/opt/postgresql@17/bin
  else
    echo "PostgreSQL 17 tools are required; set POSTGRES_BIN" >&2
    exit 1
  fi
fi
for executable in initdb pg_ctl createdb; do
  if [[ ! -x "$postgres_bin/$executable" ]]; then
    echo "missing $postgres_bin/$executable" >&2
    exit 1
  fi
done

scratch_dir="$(mktemp -d /tmp/go-laserstream-handoff-e2e.XXXXXX)"
data_dir="$scratch_dir/postgres"
log_file="$scratch_dir/postgres.log"
port="$(python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    print(sock.getsockname()[1])
PY
)"
postgres_started=0
cleanup() {
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

cd "$module_root"
"$go_bin" test -race ./... -count=1
TEST_DATABASE_URL="$database_url" \
  "$go_bin" test ./internal/stream -run 'E2E$' -count=1 -v

echo "PASS: combined Go LaserStream handoff is gap-free and PostgreSQL-idempotent"
