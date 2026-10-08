#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../go/workers"
export GOTOOLCHAIN=local
export GOENV=off
cache_dir="${WORKERS_V2_CACHE_DIR:-${TMPDIR:-/tmp}/loyal-workers-v2-cache}"
export GOMODCACHE="${WORKERS_V2_GOMODCACHE:-$cache_dir/gomod}"
export GOCACHE="${WORKERS_V2_GOCACHE:-$cache_dir/gobuild}"
if test -n "$(gofmt -l cmd internal)"; then
  echo '{"gate":"format","verdict":"FAIL"}'
  exit 1
fi
go vet ./...
go test -race -count=1 ./...
go build ./cmd/loyal-observer ./cmd/loyal-engine ./cmd/loyal-evidence ./cmd/loyal-migrate
echo '{"gate":"offline","verdict":"PASS","scope":"offline package behavior and binaries; durable acceptance remains separate"}'
