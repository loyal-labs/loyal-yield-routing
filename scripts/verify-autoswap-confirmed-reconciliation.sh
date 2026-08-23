#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'FAIL_AUTOSWAP_CONFIRMED_RECONCILIATION %s\n' "$*" >&2
  exit 1
}

routing_root=""
app_root=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --routing-root)
      routing_root="${2:-}"
      shift 2
      ;;
    --app-root)
      app_root="${2:-}"
      shift 2
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n "$routing_root" && -e "$routing_root/.git" ]] ||
  fail "--routing-root must point to a Git worktree"
[[ -n "$app_root" && -e "$app_root/.git" ]] ||
  fail "--app-root must point to a Git worktree"
[[ -x "$routing_root/scripts/verify-autoswap-local-e2e.sh" ]] ||
  fail "local Autoswap lifecycle verifier is missing"
[[ -f "$routing_root/crates/loyal-yield-store/tests/autoswap_confirmed_reconciliation_db.rs" ]] ||
  fail "confirmed reconciliation database contract is missing"

for command_name in bun cargo git initdb pg_config pg_ctl solana-test-validator; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

printf '== Confirmed reconciliation database contract\n'
scratch_dir="$(mktemp -d /private/tmp/ask-2192-autoswap-confirmed.XXXXXX)"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
postgres_log="$scratch_dir/postgres.log"
database_name="ask_2192_autoswap_confirmed_${$}"
port=$((56600 + ($$ % 300)))
started=false
cleanup() {
  if [[ "$started" == true ]]; then
    "$(pg_config --bindir)/pg_ctl" -D "$data_dir" -m fast stop >/dev/null 2>&1 || true
  fi
  rm -rf -- "$scratch_dir"
}
trap cleanup EXIT
mkdir -p "$socket_dir"
"$(pg_config --bindir)/initdb" -D "$data_dir" -A trust --no-locale >/dev/null
"$(pg_config --bindir)/pg_ctl" -D "$data_dir" -l "$postgres_log" \
  -o "-p $port -h 127.0.0.1 -k $socket_dir" start >/dev/null
started=true
"$(pg_config --bindir)/createdb" -h 127.0.0.1 -p "$port" "$database_name"
database_url="postgresql://127.0.0.1:$port/$database_name"
(
  cd "$routing_root"
  NEON_DATABASE_URL="$database_url" NO_DNA=1 \
    cargo run -q -p loyal-yield-orchestrator --bin yield-migrations -- --apply
  ASK_2192_VERIFY_DATABASE_URL="$database_url" \
    cargo test -p loyal-yield-store --test autoswap_confirmed_reconciliation_db \
      -- --ignored --nocapture
)

printf '== Disposable confirmed-chain browser lifecycle\n'
bash "$routing_root/scripts/verify-autoswap-local-e2e.sh" \
  --app-root "$app_root" \
  --policy-commitment confirmed

printf '== Focused routing checks\n'
(
  cd "$routing_root"
  cargo test -p loyal-yield-store autoswap_confirmed -- --nocapture
  cargo test -p balance-sweep-ata-monitor autoswap -- --nocapture
  cargo check -p loyal-yield-store -p balance-sweep-ata-monitor -p loyal-yield-realtime
  cargo fmt --all -- --check
  git diff --check
)

printf '== Focused web contracts\n'
(
  cd "$app_root"
  bun test apps/web/src/lib/yield-optimization/earn-cross-mint-policy-index.test.ts \
    apps/web/src/lib/yield-optimization/earn-cross-mint-repository.server.test.ts
  bun run --cwd apps/web lint -- \
    --file src/lib/yield-optimization/earn-cross-mint-policy-index.shared.ts \
    --file src/lib/yield-optimization/earn-cross-mint-repository.server.ts \
    --file src/lib/yield-optimization/earn-cross-mint-policy-index.test.ts \
    --file src/lib/yield-optimization/earn-cross-mint-repository.server.test.ts \
    --file src/components/wallet-workspace/facelift/autoswap-pane.tsx
  git diff --check
)

printf 'PASS_AUTOSWAP_CONFIRMED_RECONCILIATION\n'
