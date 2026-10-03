#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../go/workers"
export GOTOOLCHAIN=local
export GOENV=off
export GOMODCACHE="${WORKERS_V2_GOMODCACHE:-/Users/user/loyal/.cache/workers-v2/gomod}"
export GOCACHE="${WORKERS_V2_GOCACHE:-/Users/user/loyal/.cache/workers-v2/gobuild}"
if test -n "$(gofmt -l cmd internal)"; then
  echo '{"gate":"format","verdict":"FAIL"}'
  exit 1
fi
go vet ./...
go test -race ./...
go build ./cmd/loyal-observer ./cmd/loyal-engine ./cmd/loyal-evidence
echo '{"gate":"offline","verdict":"PASS","scope":"offline package behavior and binaries; durable acceptance remains separate"}'
