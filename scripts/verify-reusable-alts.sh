#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "checking reusable ALT diff integrity"
unmerged_paths="$(git diff --name-only --diff-filter=U)"
if [[ -n "$unmerged_paths" ]]; then
  echo "unmerged paths make reusable ALT verification invalid:" >&2
  printf '%s\n' "$unmerged_paths" >&2
  exit 1
fi
git diff --check HEAD --
git diff --exit-code HEAD -- crates/loyal-yield-orchestrator/migrations/0008_route_lookup_tables.sql
git diff --exit-code HEAD -- crates/loyal-yield-orchestrator/migrations/0017_reusable_route_lookup_tables.sql

for migration in \
  crates/loyal-yield-orchestrator/migrations/0018_earn_activity_realtime.sql \
  crates/loyal-yield-orchestrator/migrations/0019_legacy_lookup_table_imports.sql \
  crates/loyal-yield-orchestrator/migrations/0020_demand_driven_shared_market_catalog.sql \
  crates/loyal-yield-orchestrator/migrations/0021_reusable_alt_production_controls.sql; do
  if [[ ! -f "$migration" ]]; then
    echo "required ordered migration is missing: $migration" >&2
    exit 1
  fi
done

untracked_diff_errors=()
while IFS= read -r -d '' path; do
  [[ -f "$path" ]] || continue
  if LC_ALL=C grep -Iq . -- "$path"; then
    untracked_check="$(git diff --no-index --check -- /dev/null "$path" 2>/dev/null || true)"
    if [[ -n "$untracked_check" ]]; then
      untracked_diff_errors+=("$path")
    fi
  fi
done < <(git ls-files --others --exclude-standard -z)
if (( ${#untracked_diff_errors[@]} > 0 )); then
  echo "untracked text files fail diff integrity; matched content is intentionally suppressed:" >&2
  printf '%s\n' "${untracked_diff_errors[@]}" >&2
  exit 1
fi

if [[ "${REUSABLE_ALT_VERIFY_EXACT_COMMIT:-0}" == "1" ]]; then
  exact_commit_changes="$(git status --porcelain=v1 --untracked-files=all)"
  if [[ -n "$exact_commit_changes" ]]; then
    echo "exact-commit verification requires a clean checkout; changed paths:" >&2
    printf '%s\n' "$exact_commit_changes" >&2
    exit 1
  fi
  for migration in \
    crates/loyal-yield-orchestrator/migrations/0018_earn_activity_realtime.sql \
    crates/loyal-yield-orchestrator/migrations/0019_legacy_lookup_table_imports.sql \
    crates/loyal-yield-orchestrator/migrations/0020_demand_driven_shared_market_catalog.sql \
    crates/loyal-yield-orchestrator/migrations/0021_reusable_alt_production_controls.sql; do
    if ! git cat-file -e "HEAD:$migration"; then
      echo "exact commit does not contain required migration: $migration" >&2
      exit 1
    fi
  done
  echo "verifying exact commit $(git rev-parse --verify HEAD)"
else
  echo "working-tree iteration mode; this run cannot support final IMPLEMENTATION: PASS" >&2
fi

echo "checking tracked and untracked text for plaintext secret material"
plaintext_secret_pattern='-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----|postgres(ql)?://[^[:space:]'"'"']+:[^@[:space:]'"'"']+@|\[([[:space:]]*[0-9]{1,3}[[:space:]]*,){63,}[[:space:]]*[0-9]{1,3}[[:space:]]*\]|(POLICY_KEYPAIR|YIELD_ROUTER_KEYPAIR|SOLANA_TESTING_PK|DEPLOYMENT_PK)[[:space:]]*=[[:space:]]*["'"'"']?[1-9A-HJ-NP-Za-km-z]{80,}|(signed_transaction|serialized_transaction|transaction_bytes)[[:space:]]*[:=][[:space:]]*["'"'"'][A-Za-z0-9+/=_-]{100,}'
plaintext_secret_files=()
while IFS= read -r -d '' path; do
  [[ -f "$path" ]] || continue
  case "$path" in
    *.rs|*.sql|*.md|*.sh|*.toml|*.json|*.yml|*.yaml|*.ts|*.tsx|*.js|*.mjs|*.env|.env.*|Dockerfile*) ;;
    *) continue ;;
  esac
  if LC_ALL=C rg -q -U -- "$plaintext_secret_pattern" "$path"; then
    plaintext_secret_files+=("$path")
  fi
done < <(git ls-files --cached --others --exclude-standard -z)
if (( ${#plaintext_secret_files[@]} > 0 )); then
  echo "possible plaintext secret material found; matched content is intentionally suppressed:" >&2
  printf '%s\n' "${plaintext_secret_files[@]}" >&2
  exit 1
fi

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

echo "checking that the normal Earn runtime has no legacy ALT resolver dependency"
legacy_runtime_pattern='YIELD_ROUTE_LOOKUP_TABLES|legacy_lookup_tables_for_import\(|import_verified_legacy_lookup_table_fleet\(|retire_legacy_route_lookup_table\(|protected_legacy_route_lookup_table_addresses\(|LegacyLookupTableKind|LegacyLookupTableImport'
legacy_runtime_files="$({ rg -l -- "$legacy_runtime_pattern" \
  crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs \
  crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs \
  crates/loyal-yield-orchestrator/src/bin/same-mint-monitor-e2e.rs || true; } | sort -u)"
if [[ -n "$legacy_runtime_files" ]]; then
  echo "normal Earn runtime still depends on a legacy ALT inventory/resolver symbol:" >&2
  printf '%s\n' "$legacy_runtime_files" >&2
  exit 1
fi

for required_runtime_guard in \
  reusable_runtime_rejects_every_legacy_rollout_state \
  legacy_lookup_table_cli_argument_is_rejected; do
  if ! rg -q -- "$required_runtime_guard" \
    crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs; then
    echo "normal Earn runtime is missing required no-legacy regression guard: $required_runtime_guard" >&2
    exit 1
  fi
done

echo "checking durable budget, physical-drift, and cutover wiring"
for required_provisioner_call in \
  reserve_lookup_table_cluster_budget \
  report_shared_market_physical_drift \
  reusable_only_cutover_preflight \
  grant_lookup_table_provisioner_broadcast_permit \
  resolve_lookup_table_provisioner_broadcast_permit \
  activate_reusable_only_cutover; do
  if ! rg -q -- "$required_provisioner_call" \
    crates/loyal-yield-orchestrator/src/bin/route-lookup-table-provisioner.rs; then
    echo "provisioner is missing required durable safety wiring: $required_provisioner_call" >&2
    exit 1
  fi
done

if rg -q -- "with_lookup_table_provisioner_broadcast_fence" \
  crates/loyal-yield-orchestrator/src; then
  echo "provisioner still holds a database fence closure across broadcast" >&2
  exit 1
fi

echo "checking mandatory legacy-refund execution guards"
for required_cleanup_guard in \
  expected_fleet_count \
  expected_fleet_hash \
  imported_legacy_lookup_table_cleanup_fleet \
  registered_lookup_table_cleanup_inventory \
  enqueue_registered_cleanups \
  reserve_legacy_lookup_table_cleanup_budget \
  run_after_cleanup_budget_approval \
  get_multiple_accounts_with_commitment \
  next_before \
  RpcSendTransactionConfig \
  CommitmentLevel::Finalized \
  require_finalized_signature \
  minimum_net_recipient_increase_lamports \
  revalidate_cleanup_chain_evidence; do
  if ! rg -q -- "$required_cleanup_guard" \
    crates/loyal-yield-orchestrator/src/bin/route-lookup-table-cleanup.rs; then
    echo "legacy cleanup is missing mandatory refund guard: $required_cleanup_guard" >&2
    exit 1
  fi
done
if rg -q -- 'get_program_accounts|getProgramAccountsV2' \
  crates/loyal-yield-orchestrator/src/bin/route-lookup-table-cleanup.rs; then
  echo "legacy cleanup restored forbidden whole-program ALT discovery" >&2
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

echo "applying and checking reusable ALT migrations on the isolated database"
NO_DNA=1 bun run yield:migrate
NO_DNA=1 bun run yield:migrate:check
NO_DNA=1 bun run verify:reusable-alts:schema
REUSABLE_ALT_DB_VERIFY_ISOLATED=1 NO_DNA=1 bun run verify:reusable-alts:db

echo "running isolated durable alert and cleanup budget/crash regressions"
REUSABLE_ALT_ALERT_DB_VERIFY_ISOLATED=1 NO_DNA=1 \
  cargo test -p loyal-yield-orchestrator --test lookup_table_alerts_db \
    durable_incident_and_outbox_lifecycle_is_idempotent -- --ignored --exact
REUSABLE_ALT_CLEANUP_DB_VERIFY_ISOLATED=1 NO_DNA=1 \
  cargo test -p loyal-yield-orchestrator --test lookup_table_cleanup_db \
    cleanup_budget_and_crash_fences_share_v2_cluster_accounting -- --ignored --exact

echo "reapplying migration runner to prove idempotency"
NO_DNA=1 bun run yield:migrate
NO_DNA=1 bun run yield:migrate:check
NO_DNA=1 bun run verify:reusable-alts:schema
REUSABLE_ALT_DB_VERIFY_ISOLATED=1 NO_DNA=1 bun run verify:reusable-alts:db
