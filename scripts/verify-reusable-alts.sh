#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "checking reusable ALT diff integrity"
git diff --check
git diff --exit-code -- crates/loyal-yield-orchestrator/migrations/0008_route_lookup_tables.sql

echo "checking reusable ALT formatting and compilation"
NO_DNA=1 cargo fmt --all -- --check
NO_DNA=1 cargo check -p loyal-actions -p loyal-yield-orchestrator --all-targets

echo "checking that ALT mutation callsites stay inside audited worker boundaries"
mutation_callers="$({ rg -l \
  'create_lookup_table\(|extend_lookup_table\(|freeze_lookup_table\(|deactivate_lookup_table\(|close_lookup_table\(' \
  crates/loyal-yield-orchestrator/src || true; } | sort -u)"
unexpected_mutation_callers="$({ printf '%s\n' "$mutation_callers"; } | rg -v \
  '^crates/loyal-yield-orchestrator/src/bin/(route-lookup-table-provisioner|route-lookup-table-cleanup)\.rs$' || true)"
if [[ -n "$unexpected_mutation_callers" ]]; then
  echo "unexpected ALT mutation callsite outside provisioner/cleanup boundary:" >&2
  printf '%s\n' "$unexpected_mutation_callers" >&2
  exit 1
fi

echo "running full reusable ALT implementation tests"
NO_DNA=1 cargo test -p loyal-actions
NO_DNA=1 cargo test -p loyal-yield-orchestrator
NO_DNA=1 bun run verify:reusable-alts:routes
NO_DNA=1 bun run yield:migrate -- --help

if [[ "${RUN_REUSABLE_ALT_DATABASE_CHECKS:-0}" != "1" ]]; then
  echo "database checks are mandatory; set RUN_REUSABLE_ALT_DATABASE_CHECKS=1 on an isolated disposable branch" >&2
  exit 1
fi

if [[ "${YIELD_ALT_VERIFICATION_DATABASE_KIND:-}" != "isolated" ]]; then
  echo "refusing migration apply: YIELD_ALT_VERIFICATION_DATABASE_KIND must equal isolated" >&2
  exit 1
fi

if [[ -z "${NEON_DATABASE_URL:-}" ]]; then
  echo "NEON_DATABASE_URL must be injected for isolated database verification" >&2
  exit 1
fi

echo "applying and checking migration 0017 on the isolated database"
NO_DNA=1 bun run yield:migrate
NO_DNA=1 bun run yield:migrate:check
NO_DNA=1 bun run verify:reusable-alts:schema
REUSABLE_ALT_DB_VERIFY_ISOLATED=1 NO_DNA=1 bun run verify:reusable-alts:db

echo "reapplying migration runner to prove idempotency"
NO_DNA=1 bun run yield:migrate
NO_DNA=1 bun run yield:migrate:check
NO_DNA=1 bun run verify:reusable-alts:schema
REUSABLE_ALT_DB_VERIFY_ISOLATED=1 NO_DNA=1 bun run verify:reusable-alts:db
