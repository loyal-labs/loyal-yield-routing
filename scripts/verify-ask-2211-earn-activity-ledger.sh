#!/usr/bin/env bash
set -u

routing_root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
app_root="${2:-${LOYAL_APP_DIR:-}}"
failures=0

pass() {
  printf 'PASS: %s\n' "$1"
}

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

run_check() {
  local description="$1"
  shift
  if "$@"; then
    pass "$description"
  else
    fail "$description"
  fi
}

require_match() {
  local description="$1"
  local pattern="$2"
  shift 2
  if rg --quiet --glob '!**/node_modules/**' "$pattern" "$@"; then
    pass "$description"
  else
    fail "$description"
  fi
}

reject_match() {
  local description="$1"
  local pattern="$2"
  shift 2
  if rg --quiet --glob '!**/node_modules/**' "$pattern" "$@"; then
    fail "$description"
  else
    pass "$description"
  fi
}

[[ -e "$routing_root/.git" ]] || {
  printf 'FAIL: routing root is not a Git worktree\n' >&2
  exit 1
}
[[ -n "$app_root" && -e "$app_root/.git" ]] || {
  printf 'FAIL: Loyal App worktree is required\n' >&2
  exit 1
}

for command_name in bun cargo git initdb pg_config pg_ctl rg; do
  command -v "$command_name" >/dev/null || {
    printf 'FAIL: %s is required\n' "$command_name" >&2
    exit 1
  }
done

migration="$routing_root/crates/loyal-yield-store/migrations/0066_earn_activity_events.sql"
store="$routing_root/crates/loyal-yield-store/src/store.rs"
db_test="$routing_root/crates/loyal-yield-store/tests/earn_activity_events_db.rs"
app_schema="$app_root/apps/web/src/lib/yield-optimization/yield-neon-client.server.ts"
app_repository="$app_root/apps/web/src/lib/yield-optimization/earn-activity-repository.server.ts"
app_repository_test="$app_root/apps/web/src/lib/yield-optimization/earn-activity-repository.server.test.ts"
activity_route="$app_root/apps/web/src/app/api/smart-accounts/earn-transactions/route.ts"
formatter="$app_root/apps/web/src/app/api/smart-accounts/earn-transactions/formatter.ts"
formatter_test="$app_root/apps/web/src/app/api/smart-accounts/earn-transactions/formatter.test.ts"
legacy_repository="$app_root/apps/web/src/lib/yield-optimization/earn-autodeposit-repository.server.ts"

printf '== Durable activity contract\n'
for required_file in "$migration" "$db_test" "$app_repository" "$app_repository_test"; do
  if [[ -f "$required_file" ]]; then
    pass "required artifact exists: $required_file"
  else
    fail "required artifact exists: $required_file"
  fi
done

require_match "migration creates append-only Earn activity events" \
  'CREATE TABLE loyal_yield\.earn_activity_events' "$migration"
require_match "migration enforces an idempotency key" \
  'UNIQUE.*idempotency|CREATE UNIQUE INDEX.*earn_activity' "$migration"
for event_type in \
  autodeposit_created \
  autodeposit_closed \
  autoswap_created \
  autoswap_closed; do
  require_match "activity contract includes $event_type" "$event_type" \
    "$migration" "$store" "$db_test" "$formatter"
done

require_match "routing persists lifecycle events inside store transactions" \
  'insert_earn_activity_event' "$store"
require_match "database test covers replay idempotency" \
  'replay.*idempotent|idempotent.*replay' "$db_test"
require_match "database test covers atomic rollback" \
  'atomic.*rollback|rollback.*atomic' "$db_test"
require_match "database test preserves setup after close" \
  'setup.*close.*preserv|preserv.*setup.*close' "$db_test"

printf '== Activity read model\n'
require_match "web schema maps the activity ledger" \
  'earnActivityEvents.*earn_activity_events|earn_activity_events.*earnActivityEvents' "$app_schema"
require_match "Activity route reads lifecycle events from the ledger" \
  'findEarnActivityEventsForVault' "$activity_route" "$app_repository"
reject_match "Activity route no longer rebuilds Autodeposit history from targets" \
  'findEarnAutodepositHistoryEvents' "$activity_route"
reject_match "legacy target repository no longer owns user Activity history" \
  'buildEarnAutodepositTargetHistoryEvents|policySignature && target\.policyConfirmedSlot' \
  "$legacy_repository"
require_match "formatter supports Autoswap lifecycle rows" \
  'autoswap_created|autoswap_closed' "$formatter" "$formatter_test"
reject_match "user Activity repository does not return snapshot reconciliation" \
  'snapshot_reconciled' "$app_repository"

printf '== Focused behavior checks\n'
scratch_dir="$(mktemp -d /private/tmp/ask-2211-activity-ledger.XXXXXX)"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
postgres_log="$scratch_dir/postgres.log"
database_name="ask_2211_activity_${$}"
port=$((57100 + ($$ % 300)))
started=false
cleanup() {
  if [[ "$started" == true ]]; then
    "$(pg_config --bindir)/pg_ctl" -D "$data_dir" -m fast stop >/dev/null 2>&1 || true
  fi
  rm -rf -- "$scratch_dir"
}
trap cleanup EXIT

mkdir -p "$socket_dir"
if "$(pg_config --bindir)/initdb" -D "$data_dir" -A trust --no-locale >/dev/null && \
  "$(pg_config --bindir)/pg_ctl" -D "$data_dir" -l "$postgres_log" \
    -o "-p $port -h 127.0.0.1 -k $socket_dir" start >/dev/null; then
  started=true
  "$(pg_config --bindir)/createdb" -h 127.0.0.1 -p "$port" "$database_name"
  database_url="postgresql://127.0.0.1:$port/$database_name"
  run_check "all routing migrations apply" \
    bash -lc "cd '$routing_root' && NEON_DATABASE_URL='$database_url' NO_DNA=1 cargo run -q -p loyal-yield-orchestrator --bin yield-migrations -- --apply"
  run_check "append-only lifecycle database behavior passes" \
    bash -lc "cd '$routing_root' && ASK_2211_ACTIVITY_VERIFY_DATABASE_URL='$database_url' cargo test -p loyal-yield-store --test earn_activity_events_db -- --ignored --nocapture"
else
  fail "disposable PostgreSQL starts"
fi

run_check "routing store and monitor compile" \
  cargo check --manifest-path "$routing_root/Cargo.toml" \
    -p loyal-yield-store -p balance-sweep-ata-monitor
run_check "routing formatting passes" \
  cargo fmt --manifest-path "$routing_root/Cargo.toml" --all -- --check
run_check "Activity repository and formatter tests pass" \
  bash -lc "cd '$app_root' && bun test '$app_repository_test' '$formatter_test'"
run_check "web TypeScript passes" \
  bash -lc "cd '$app_root' && bunx tsc --noEmit --pretty false -p apps/web/tsconfig.json"
run_check "changed web files pass scoped lint" \
  bash -lc "cd '$app_root' && env ESLINT_USE_FLAT_CONFIG=false node_modules/.bin/eslint '$app_schema' '$app_repository' '$app_repository_test' '$activity_route' '$formatter' '$formatter_test'"
run_check "routing diff is cleanly applicable" git -C "$routing_root" diff --check
run_check "app diff is cleanly applicable" git -C "$app_root" diff --check

if ((failures > 0)); then
  printf 'FAIL: ASK-2211 append-only Earn activity ledger (%d checks failed)\n' "$failures" >&2
  exit 1
fi

printf 'PASS: ASK-2211 append-only Earn activity ledger\n'
