#!/usr/bin/env bash
# Fails when a Go comment cites a TypeScript line number (file.ts:N) or an
# orphaned (:N) / CL:N ref whose filename was trimmed away.
# Those refs rot: parity is proved by scripts/web/earn-tx-parity.ts and the
# Go tests, not by prose. Runs from any cwd inside the repository.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
cd "$root"

# Load-bearing refs stay here, one exclusion pathspec per entry, each with a
# one-line reason. Keep empty unless a parity harness needs the ref.
ALLOW=()

hits="$(git grep -nE '//.*(\.tsx?:[0-9]+|\(:[0-9]+|\bCL:[0-9]+)' -- 'go/*.go' ${ALLOW[@]+"${ALLOW[@]}"} || true)"
if test -n "$hits"; then
  echo "$hits"
  echo 'Go comments must not cite TypeScript line numbers (file.ts:N) (or an orphaned :N / CL:N ref): they rot. Delete the reference; parity is proved by scripts/web/earn-tx-parity.ts and the Go tests. Allowlist: scripts/check-go-ts-refs.sh.'
  echo '{"gate":"ts-line-refs","verdict":"FAIL"}'
  exit 1
fi
echo '{"gate":"ts-line-refs","verdict":"PASS"}'
