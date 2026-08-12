#!/usr/bin/env bash
set -euo pipefail

cutoff="${SAME_MINT_FIX_CUTOFF:-2026-06-18T00:00:00Z}"
service_id="${SAME_MINT_RENDER_SERVICE_ID:-srv-d8n7gqbbc2fs73emk610}"

if [[ "${RUN_LOCAL_CHECKS:-0}" == "1" ]]; then
  NO_DNA=1 cargo fmt --check
  NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin same-mint-yield-monitor
  NO_DNA=1 cargo check -p loyal-fleet-worker --bin same-mint-reserve-swap
fi

if [[ -z "${NEON_DATABASE_URL:-}" ]]; then
  echo "NEON_DATABASE_URL is required for DB guardrail verification" >&2
  exit 1
fi

psql "$NEON_DATABASE_URL" \
  -v ON_ERROR_STOP=1 \
  -v fix_cutoff="$cutoff" \
  -f docs/same-mint-worker-fix-guardrail.sql

if [[ "${RUN_MONITOR_DRY_RUN:-0}" == "1" ]]; then
  cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor -- --once --all-active-vaults
fi

if [[ "${RUN_RENDER_CHECKS:-0}" == "1" ]]; then
  render services --output json | rg -F "$service_id" >/dev/null
  render logs --resource "$service_id" --since 30m --text execute | rg -F "execute: false" >/dev/null
fi
