#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../go/workers"
export GOTOOLCHAIN=local
export GOENV=off
cache_dir="${WORKERS_V2_CACHE_DIR:-${TMPDIR:-/tmp}/loyal-workers-v2-cache}"
export GOMODCACHE="${WORKERS_V2_GOMODCACHE:-$cache_dir/gomod}"
export GOCACHE="${WORKERS_V2_GOCACHE:-$cache_dir/gobuild}"
bash ../../scripts/check-go-ts-refs.sh
mapfile -t packages < <(python3 -c 'import json,os; print("\n".join(json.loads(os.environ.get("WORKERS_V2_PACKAGES", "[\"./...\"]"))))')
[[ ${#packages[@]} -gt 0 ]] || { echo 'No Go packages selected' >&2; exit 1; }
mapfile -t directories < <(go list -f '{{.Dir}}' "${packages[@]}")
[[ ${#directories[@]} -gt 0 ]] || exit 1
if test -n "$(gofmt -l "${directories[@]}")"; then
  echo '{"gate":"format","verdict":"FAIL"}'
  exit 1
fi
go vet "${packages[@]}"
race=
[[ ${WORKERS_V2_RACE:-} == 1 ]] && race=-race
# Bridge and webapp tests share a disposable database schema.
go test $race -p 1 -count=1 "${packages[@]}"
python3 ../../scripts/ci-db-tests.py
echo '{"gate":"offline","verdict":"PASS","scope":"offline package behavior and binaries; durable acceptance remains separate"}'
