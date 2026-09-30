#!/usr/bin/env bash
# Create a private cluster; never attach to an existing database/service.
set -euo pipefail
umask 077
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/../.." && pwd)
cd "$repo_root"

# Re-exec with a minimal build environment. HOME locates the installed Rust
# toolchain/cache; runtime env below contains no inherited bindings or secrets.
if [[ ${1:-} != --isolated ]]; then
  exec env -i PATH="$PATH" HOME="$HOME" bash "$script_dir/run-scratch.sh" --isolated "$@"
fi
shift
if [[ $# -ne 1 ]]; then
  echo 'Usage: bash scripts/hetzner-realtime/run-scratch.sh <new-evidence-directory>' >&2
  exit 2
fi
output_dir=$1
mkdir -- "$output_dir" # refuse reuse/overwrite of another run
output_dir=$(cd -- "$output_dir" && pwd)

# libpq-only installs can expose initdb without its companion postgres binary.
pg_bin=$(dirname -- "$(command -v initdb)")
if [[ ! -x "$pg_bin/postgres" ]]; then
  pg_bin=/opt/homebrew/opt/postgresql@17/bin
fi
for binary in postgres initdb pg_ctl psql createdb; do
  [[ -x "$pg_bin/$binary" ]] || { echo "Missing PostgreSQL tool: $pg_bin/$binary" >&2; exit 2; }
done
run_path="$pg_bin:$PATH"
pg_tool() {
  env -i PATH="$run_path" PGPASSFILE=/dev/null LC_ALL=C "$pg_bin/$@"
}
task_pg=$(mktemp -d "${TMPDIR:-/tmp}/realtime-pg.XXXXXX")
verifier_pid=''
pg_started=false
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [[ -n "$verifier_pid" ]] && kill -0 "$verifier_pid" 2>/dev/null; then
    kill -TERM "$verifier_pid" 2>/dev/null || true
  fi
  if [[ "$pg_started" == true || -f "$task_pg/data/postmaster.pid" ]]; then
    pg_tool pg_ctl -D "$task_pg/data" -m fast -w stop >> "$output_dir/cleanup.log" 2>&1 || status=1
  fi
  if [[ -n "$verifier_pid" ]]; then wait "$verifier_pid" 2>/dev/null || true; fi
  if [[ -f "$task_pg/data/postmaster.pid" ]]; then status=1; fi
  cp "$task_pg/server.log" "$output_dir/postgres.log" 2>/dev/null || true
  if [[ $status -eq 0 ]]; then
    rm -rf -- "$task_pg"
    printf '{"postgres_stopped":true,"scratch_cluster_removed":true,"runtime_children_stopped":true}\n' > "$output_dir/cleanup.json"
    python3 "$script_dir/bind-evidence.py" bind "$output_dir" || status=1
  else
    if [[ ! -f "$task_pg/data/postmaster.pid" ]]; then
      rm -rf -- "$task_pg"
      printf '{"postgres_stopped":true,"scratch_cluster_removed":true,"run_failed":true}\n' > "$output_dir/cleanup.json"
    else
      printf '{"postgres_stopped":false,"scratch_cluster_removed":false,"run_failed":true}\n' > "$output_dir/cleanup.json"
      echo "Cleanup failed; scratch retained at $task_pg" >&2
    fi
  fi
  echo "Evidence directory: $output_dir" >&2
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

python3 "$script_dir/bind-evidence.py" snapshot "$output_dir"
# Ports reserved by OS at selection time; pg_ctl/listener fail closed on a race.
read -r db_port http_port < <(python3 - <<'PY'
import socket
with socket.socket() as a, socket.socket() as b:
    a.bind(('127.0.0.1',0)); b.bind(('127.0.0.1',0))
    print(a.getsockname()[1], b.getsockname()[1])
PY
)
pg_tool initdb -D "$task_pg/data" --auth-local=trust --auth-host=trust -U realtime_fixture > "$output_dir/initdb.log" 2>&1
pg_tool pg_ctl -D "$task_pg/data" -l "$task_pg/server.log" -o "-h 127.0.0.1 -p $db_port -k $task_pg" -w start > "$output_dir/postgres-start.log" 2>&1
pg_started=true
pg_tool createdb -h 127.0.0.1 -p "$db_port" -U realtime_fixture realtime_fixture
pg_tool createdb -h 127.0.0.1 -p "$db_port" -U realtime_fixture realtime_fixture_unmarked
pg_tool psql -Xw -h 127.0.0.1 -p "$db_port" -U realtime_fixture -d realtime_fixture -v ON_ERROR_STOP=1 -c "COMMENT ON DATABASE realtime_fixture IS 'loyal-realtime-disposable-fixture'" > "$output_dir/fixture-setup.log"
python3 "$script_dir/prepare-fixture.py" > "$output_dir/fixture.sql"
pg_tool psql -Xw -h 127.0.0.1 -p "$db_port" -U realtime_fixture -d realtime_fixture -v ON_ERROR_STOP=1 -f "$output_dir/fixture.sql" >> "$output_dir/fixture-setup.log"
cargo build -p loyal-yield-realtime --offline > "$output_dir/cargo-build.log" 2>&1
cargo test -p loyal-yield-realtime -p loyal-yield-realtime-core --offline > "$output_dir/cargo-tests.log" 2>&1
python3 "$script_dir/bind-evidence.py" binary "$output_dir"
python3 "$script_dir/verify-guards.py" > "$output_dir/guard-negative.log"
fixture_db="postgres://realtime_fixture@127.0.0.1:$db_port/realtime_fixture"
fixture_http="http://127.0.0.1:$http_port"
# Keep exact non-secret invocation inputs, including the chosen ephemeral ports.
python3 - "$fixture_db" "$fixture_http" > "$output_dir/inputs.json" <<'PY'
import json,sys
print(json.dumps({'REALTIME_FIXTURE_ONLY':'true','NEON_DATABASE_URL':sys.argv[1],
                  'REALTIME_URL':sys.argv[2], 'synthetic_authentication':'defined by verify-local.py',
                  'inherited_runtime_env':['PATH']},indent=2))
PY
env -i PATH="$run_path" REALTIME_FIXTURE_ONLY=true NEON_DATABASE_URL="$fixture_db" REALTIME_URL="$fixture_http" PGHOST=denied.fixture.invalid PGDATABASE=denied_fixture bun "$script_dir/verify-transport.ts" > "$output_dir/transport-identity.json"
env -i PATH="$run_path" REALTIME_FIXTURE_ONLY=true NEON_DATABASE_URL="${fixture_db}_unmarked" REALTIME_URL="$fixture_http" bun "$script_dir/verify-transport.ts" reject-unmarked > "$output_dir/transport-negative.json"
env -i PATH="$run_path" REALTIME_FIXTURE_ONLY=true NEON_DATABASE_URL="$fixture_db" REALTIME_URL="$fixture_http" REALTIME_FIXTURE_LOG_DIR="$output_dir" python3 "$script_dir/verify-local.py" > "$output_dir/runtime.json" 2> "$output_dir/runtime.log" &
verifier_pid=$!
wait "$verifier_pid"
verifier_pid=''
