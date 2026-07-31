#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
trigger_source="$repo_root/crates/balance-sweep-autodeposit-trigger/src/main.rs"
projector_bin="$repo_root/target/debug/balance-sweep-ata-projector"
trigger_bin="$repo_root/target/debug/balance-sweep-autodeposit-trigger"
target_count="${BALANCE_SWEEP_DEADLOCK_TARGETS:-1000}"
round_count="${BALANCE_SWEEP_DEADLOCK_ROUNDS:-5}"
usdc_mint="EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

for command_name in cargo initdb pg_ctl psql rg; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

case "$target_count" in
  ''|*[!0-9]*) fail "BALANCE_SWEEP_DEADLOCK_TARGETS must be a positive integer" ;;
esac
case "$round_count" in
  ''|*[!0-9]*) fail "BALANCE_SWEEP_DEADLOCK_ROUNDS must be a positive integer" ;;
esac
[[ "$target_count" -gt 1 ]] || fail "BALANCE_SWEEP_DEADLOCK_TARGETS must be greater than 1"
[[ "$round_count" -gt 0 ]] || fail "BALANCE_SWEEP_DEADLOCK_ROUNDS must be greater than 0"

deplete_function="$({
  awk '
    /^async fn deplete_lots_newest_first/ { capture = 1 }
    capture { print }
    capture && /^}$/ { exit }
  ' "$trigger_source"
})"
printf '%s\n' "$deplete_function" | rg --fixed-strings --quiet "FOR UPDATE OF lot" ||
  fail "deplete_lots_newest_first must lock only the lot alias"
if printf '%s\n' "$deplete_function" | rg --quiet '^[[:space:]]*FOR UPDATE[[:space:]]*$'; then
  fail "deplete_lots_newest_first still contains an unscoped FOR UPDATE"
fi

echo "Building the two production worker binaries"
cargo build --quiet \
  -p balance-sweep-ata-projector \
  -p balance-sweep-autodeposit-trigger

scratch_dir="$(mktemp -d "${TMPDIR:-/tmp}/balance-sweep-deadlock.XXXXXX")"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
mkdir -p "$socket_dir"
port="$((55432 + RANDOM % 1000))"
server_started=0

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    pg_ctl -D "$data_dir" -m fast -w stop >/dev/null
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

initdb -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
pg_ctl -D "$data_dir" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1 -c deadlock_timeout=100ms -c lock_timeout=10s -c log_lock_waits=on" \
  -w start >/dev/null
server_started=1

psql_args=(
  -X
  --set=ON_ERROR_STOP=1
  --host="$socket_dir"
  --port="$port"
  --username="$(id -un)"
  --dbname=postgres
)
database_url="postgresql://$(id -un)@127.0.0.1:$port/postgres"

psql "${psql_args[@]}" >/dev/null <<'SQL'
CREATE SCHEMA loyal_yield;
CREATE SCHEMA loyal_prod;

CREATE TYPE loyal_yield.balance_sweep_surplus_lot_status AS ENUM (
  'open', 'selected', 'consumed', 'depleted', 'suppressed'
);

CREATE TABLE loyal_yield.projection_offsets (
  consumer_name TEXT PRIMARY KEY,
  last_event_id BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE loyal_yield.balance_sweep_targets (
  id BIGINT PRIMARY KEY,
  active BOOLEAN NOT NULL DEFAULT true,
  lifecycle_status TEXT NOT NULL DEFAULT 'active',
  wallet_balance_floor_raw BIGINT,
  token_mint TEXT NOT NULL
);

CREATE TABLE loyal_yield.balance_sweep_wallet_balances_current (
  target_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_targets(id) ON DELETE CASCADE,
  wallet TEXT NOT NULL,
  wallet_usdc_ata TEXT,
  wallet_token_ata TEXT NOT NULL,
  amount_raw BIGINT NOT NULL,
  owner TEXT,
  mint TEXT NOT NULL,
  observed_slot BIGINT NOT NULL,
  observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  source TEXT NOT NULL,
  source_commitment TEXT NOT NULL,
  txn_signature TEXT,
  account_data_hash TEXT,
  raw_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (target_id, mint)
);

CREATE TABLE loyal_yield.balance_sweep_wallet_balance_events (
  event_id BIGINT PRIMARY KEY,
  target_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_targets(id) ON DELETE CASCADE,
  wallet TEXT NOT NULL,
  wallet_usdc_ata TEXT,
  wallet_token_ata TEXT NOT NULL,
  mint TEXT NOT NULL,
  previous_amount_raw BIGINT,
  amount_raw BIGINT NOT NULL,
  delta_amount_raw BIGINT,
  observed_slot BIGINT NOT NULL,
  observed_at TIMESTAMPTZ NOT NULL,
  source TEXT NOT NULL,
  source_commitment TEXT NOT NULL,
  txn_signature TEXT,
  account_data_hash TEXT,
  raw_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
  projected_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE loyal_yield.balance_sweep_surplus_lots (
  id BIGSERIAL PRIMARY KEY,
  target_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_targets(id) ON DELETE CASCADE,
  source_event_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_wallet_balance_events(event_id) ON DELETE CASCADE,
  source_signature TEXT,
  original_amount_raw BIGINT NOT NULL,
  remaining_amount_raw BIGINT NOT NULL,
  classification TEXT NOT NULL,
  eligible_after TIMESTAMPTZ NOT NULL,
  status loyal_yield.balance_sweep_surplus_lot_status NOT NULL DEFAULT 'open',
  confidence TEXT NOT NULL,
  reason TEXT NOT NULL,
  scheduled_slot_id BIGINT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_event_id)
);

CREATE INDEX balance_sweep_surplus_lots_target_open_idx
  ON loyal_yield.balance_sweep_surplus_lots (target_id, status, created_at DESC, id DESC);

CREATE TABLE loyal_prod.balance_sweep_wallet_ata_observations (
  event_id BIGINT PRIMARY KEY,
  cluster TEXT NOT NULL,
  target_id BIGINT NOT NULL,
  wallet TEXT NOT NULL,
  wallet_usdc_ata TEXT NOT NULL,
  vault_pubkey TEXT NOT NULL,
  vault_usdc_ata TEXT NOT NULL,
  amount_raw BIGINT NOT NULL,
  owner TEXT,
  mint TEXT NOT NULL,
  slot BIGINT NOT NULL,
  observed_at TIMESTAMPTZ NOT NULL,
  source TEXT NOT NULL,
  source_commitment TEXT NOT NULL,
  txn_signature TEXT,
  account_data_hash TEXT NOT NULL,
  raw_account_data_base64 TEXT NOT NULL DEFAULT '',
  raw_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
  received_at TIMESTAMPTZ NOT NULL,
  inserted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
SQL

reset_tables() {
  psql "${psql_args[@]}" >/dev/null <<'SQL'
TRUNCATE TABLE
  loyal_yield.balance_sweep_surplus_lots,
  loyal_yield.balance_sweep_wallet_balances_current,
  loyal_yield.balance_sweep_wallet_balance_events,
  loyal_yield.balance_sweep_targets,
  loyal_yield.projection_offsets,
  loyal_prod.balance_sweep_wallet_ata_observations
RESTART IDENTITY CASCADE;
SQL
}

seed_probe() {
  reset_tables
  psql "${psql_args[@]}" --set=usdc_mint="$usdc_mint" >/dev/null <<'SQL'
INSERT INTO loyal_yield.balance_sweep_targets (id, token_mint)
VALUES (1, :'usdc_mint'), (2, :'usdc_mint');

INSERT INTO loyal_yield.balance_sweep_wallet_balance_events (
  event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata, mint,
  amount_raw, delta_amount_raw, observed_slot, observed_at, source,
  source_commitment, account_data_hash
)
VALUES
  (1, 1, 'wallet-1', 'ata-1', 'ata-1', :'usdc_mint', 100, 100, 1, now(), 'probe', 'finalized', 'hash-1'),
  (2, 2, 'wallet-2', 'ata-2', 'ata-2', :'usdc_mint', 100, 100, 2, now(), 'probe', 'finalized', 'hash-2');

INSERT INTO loyal_yield.balance_sweep_surplus_lots (
  target_id, source_event_id, original_amount_raw, remaining_amount_raw,
  classification, eligible_after, confidence, reason
)
VALUES
  (1, 1, 100, 100, 'simple_inbound', now(), 'verified', 'deadlock probe'),
  (2, 2, 100, 100, 'simple_inbound', now(), 'verified', 'deadlock probe');
SQL
}

# The trigger-side SQL is written to a temporary file because psql variables cannot
# safely splice a locking clause into a parsed SQL statement.
run_sql_probe() {
  local mode="$1"
  local expected_result="$2"
  local probe_name="$3"
  local trigger_sql="$scratch_dir/${probe_name}-trigger.sql"

  if [[ "$mode" == "legacy" ]]; then
    sed 's/__LOCK_CLAUSE__/FOR UPDATE/' >"$trigger_sql" <<'SQL'
BEGIN;
SELECT lot.id
FROM loyal_yield.balance_sweep_surplus_lots AS lot
JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
  ON event.event_id = lot.source_event_id
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = lot.target_id
WHERE lot.target_id = 2
  AND event.mint = target.token_mint
  AND target.token_mint = :'usdc_mint'
  AND lot.status = 'open'
__LOCK_CLAUSE__;
SELECT pg_sleep(0.4);
SELECT lot.id
FROM loyal_yield.balance_sweep_surplus_lots AS lot
JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
  ON event.event_id = lot.source_event_id
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = lot.target_id
WHERE lot.target_id = 1
  AND event.mint = target.token_mint
  AND target.token_mint = :'usdc_mint'
  AND lot.status = 'open'
__LOCK_CLAUSE__;
COMMIT;
SQL
  else
    sed 's/__LOCK_CLAUSE__/FOR UPDATE OF lot/' >"$trigger_sql" <<'SQL'
BEGIN;
SELECT lot.id
FROM loyal_yield.balance_sweep_surplus_lots AS lot
JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
  ON event.event_id = lot.source_event_id
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = lot.target_id
WHERE lot.target_id = 2
  AND event.mint = target.token_mint
  AND target.token_mint = :'usdc_mint'
  AND lot.status = 'open'
__LOCK_CLAUSE__;
SELECT pg_sleep(0.4);
SELECT lot.id
FROM loyal_yield.balance_sweep_surplus_lots AS lot
JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
  ON event.event_id = lot.source_event_id
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = lot.target_id
WHERE lot.target_id = 1
  AND event.mint = target.token_mint
  AND target.token_mint = :'usdc_mint'
  AND lot.status = 'open'
__LOCK_CLAUSE__;
COMMIT;
SQL
  fi

  seed_probe
  local projector_log="$scratch_dir/${probe_name}-projector.log"
  local trigger_log="$scratch_dir/${probe_name}-trigger.log"

  psql "${psql_args[@]}" --set=usdc_mint="$usdc_mint" >"$projector_log" 2>&1 <<'SQL' &
BEGIN;
INSERT INTO loyal_yield.balance_sweep_wallet_balance_events (
  event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata, mint,
  amount_raw, observed_slot, observed_at, source, source_commitment, account_data_hash
)
VALUES (101, 1, 'wallet-1', 'ata-1', 'ata-1', :'usdc_mint', 50, 101, now(), 'projector', 'finalized', 'projector-1');
SELECT pg_sleep(0.4);
INSERT INTO loyal_yield.balance_sweep_wallet_balance_events (
  event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata, mint,
  amount_raw, observed_slot, observed_at, source, source_commitment, account_data_hash
)
VALUES (102, 2, 'wallet-2', 'ata-2', 'ata-2', :'usdc_mint', 50, 102, now(), 'projector', 'finalized', 'projector-2');
COMMIT;
SQL
  local projector_pid=$!

  sleep 0.08
  psql "${psql_args[@]}" --set=usdc_mint="$usdc_mint" --file="$trigger_sql" >"$trigger_log" 2>&1 &
  local trigger_pid=$!

  set +e
  wait "$projector_pid"
  local projector_status=$?
  wait "$trigger_pid"
  local trigger_status=$?
  set -e

  if [[ "$expected_result" == "deadlock" ]]; then
    rg --quiet "deadlock detected|40P01" "$projector_log" "$trigger_log" ||
      fail "legacy control did not reproduce a deadlock"
    if [[ "$projector_status" -eq 0 && "$trigger_status" -eq 0 ]]; then
      fail "legacy control reported success despite the expected deadlock"
    fi
    echo "PASS: legacy unscoped-lock control reproduced a deadlock"
    return
  fi

  if [[ "$projector_status" -ne 0 || "$trigger_status" -ne 0 ]]; then
    tail -40 "$projector_log" >&2
    tail -40 "$trigger_log" >&2
    fail "fixed-lock interleaving failed"
  fi
  rg --quiet "deadlock detected|40P01" "$projector_log" "$trigger_log" &&
    fail "fixed-lock interleaving encountered a deadlock"
  echo "PASS: scoped lot lock completed the deterministic interleaving"
}

database_deadlocks() {
  psql "${psql_args[@]}" --tuples-only --no-align --command="
    SELECT deadlocks
    FROM pg_stat_database
    WHERE datname = current_database();
  "
}

run_sql_probe legacy deadlock legacy-control
deadlocks_after_legacy="$(database_deadlocks)"
run_sql_probe fixed success fixed-control
deadlocks_after_fixed="$(database_deadlocks)"
[[ "$deadlocks_after_fixed" == "$deadlocks_after_legacy" ]] ||
  fail "fixed deterministic probe incremented pg_stat_database.deadlocks"

seed_load() {
  reset_tables
  psql "${psql_args[@]}" \
    --set=target_count="$target_count" \
    --set=usdc_mint="$usdc_mint" >/dev/null <<'SQL'
INSERT INTO loyal_yield.balance_sweep_targets (
  id, active, lifecycle_status, wallet_balance_floor_raw, token_mint
)
SELECT target_id, true, 'active', 0, :'usdc_mint'
FROM generate_series(1, :target_count::bigint) AS target_id;

INSERT INTO loyal_yield.balance_sweep_wallet_balance_events (
  event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata, mint,
  previous_amount_raw, amount_raw, delta_amount_raw, observed_slot, observed_at,
  source, source_commitment, txn_signature, account_data_hash, raw_evidence
)
SELECT
  target_id,
  target_id,
  'wallet-' || target_id,
  'ata-' || target_id,
  'ata-' || target_id,
  :'usdc_mint',
  NULL,
  100,
  100,
  target_id,
  now() - interval '2 minutes',
  'production-load-fixture',
  'finalized',
  'source-signature-' || target_id,
  'source-hash-' || target_id,
  '{}'::jsonb
FROM generate_series(1, :target_count::bigint) AS target_id;

INSERT INTO loyal_yield.balance_sweep_surplus_lots (
  target_id, source_event_id, source_signature, original_amount_raw,
  remaining_amount_raw, classification, eligible_after, status, confidence, reason
)
SELECT
  target_id,
  target_id,
  'source-signature-' || target_id,
  100,
  100,
  'simple_inbound',
  now() - interval '1 minute',
  'open',
  'verified',
  'production-load-fixture'
FROM generate_series(1, :target_count::bigint) AS target_id;

INSERT INTO loyal_yield.balance_sweep_wallet_balance_events (
  event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata, mint,
  previous_amount_raw, amount_raw, delta_amount_raw, observed_slot, observed_at,
  source, source_commitment, txn_signature, account_data_hash, raw_evidence
)
SELECT
  :target_count::bigint + sequence_id,
  :target_count::bigint - sequence_id + 1,
  'wallet-' || (:target_count::bigint - sequence_id + 1),
  'ata-' || (:target_count::bigint - sequence_id + 1),
  'ata-' || (:target_count::bigint - sequence_id + 1),
  :'usdc_mint',
  100,
  0,
  -100,
  :target_count::bigint + sequence_id,
  now() - interval '1 minute',
  'production-load-fixture',
  'finalized',
  'outflow-signature-' || sequence_id,
  'outflow-hash-' || sequence_id,
  '{}'::jsonb
FROM generate_series(1, :target_count::bigint) AS sequence_id;

INSERT INTO loyal_yield.projection_offsets (consumer_name, last_event_id)
VALUES ('balance_sweep_autodeposit_trigger', :target_count::bigint);

INSERT INTO loyal_prod.balance_sweep_wallet_ata_observations (
  event_id, cluster, target_id, wallet, wallet_usdc_ata, vault_pubkey,
  vault_usdc_ata, amount_raw, owner, mint, slot, observed_at, source,
  source_commitment, txn_signature, account_data_hash, raw_account_data_base64,
  raw_evidence, received_at
)
SELECT
  (2 * :target_count::bigint) + target_id,
  'mainnet-beta',
  target_id,
  'wallet-' || target_id,
  'ata-' || target_id,
  'vault-' || target_id,
  'vault-ata-' || target_id,
  50,
  'owner-' || target_id,
  :'usdc_mint',
  (2 * :target_count::bigint) + target_id,
  now(),
  'laserstream',
  'finalized',
  'projector-signature-' || target_id,
  'projector-hash-' || target_id,
  '',
  '{}'::jsonb,
  now()
FROM generate_series(1, :target_count::bigint) AS target_id;
SQL
}

deadlocks_before_workers="$(database_deadlocks)"
round=1
while [[ "$round" -le "$round_count" ]]; do
  seed_load
  projector_log="$scratch_dir/round-${round}-projector.log"
  trigger_log="$scratch_dir/round-${round}-trigger.log"

  "$projector_bin" \
    --timescaledb-url "$database_url" \
    --postgres-url "$database_url" \
    --ata-stream production \
    --batch-limit "$target_count" \
    --once >"$projector_log" 2>&1 &
  projector_pid=$!

  "$trigger_bin" \
    --postgres-url "$database_url" \
    --batch-limit "$target_count" \
    --disable-realtime-listen \
    --once >"$trigger_log" 2>&1 &
  trigger_pid=$!

  set +e
  wait "$projector_pid"
  projector_status=$?
  wait "$trigger_pid"
  trigger_status=$?
  set -e

  if [[ "$projector_status" -ne 0 || "$trigger_status" -ne 0 ]]; then
    tail -60 "$projector_log" >&2
    tail -60 "$trigger_log" >&2
    fail "production-load round $round failed"
  fi
  rg --quiet "deadlock detected|40P01" "$projector_log" "$trigger_log" &&
    fail "production-load round $round encountered a deadlock"

  projected_count="$(psql "${psql_args[@]}" --tuples-only --no-align --command="
    SELECT COUNT(*)
    FROM loyal_yield.balance_sweep_wallet_balance_events
    WHERE event_id > (2 * $target_count);
  ")"
  depleted_count="$(psql "${psql_args[@]}" --tuples-only --no-align --command="
    SELECT COUNT(*)
    FROM loyal_yield.balance_sweep_surplus_lots
    WHERE status = 'depleted' AND remaining_amount_raw = 0;
  ")"
  [[ "$projected_count" == "$target_count" ]] ||
    fail "round $round projected $projected_count of $target_count observations"
  [[ "$depleted_count" == "$target_count" ]] ||
    fail "round $round depleted $depleted_count of $target_count lots"

  echo "PASS: production-load round $round/$round_count projected and depleted $target_count targets"
  round=$((round + 1))
done

deadlocks_after_workers="$(database_deadlocks)"
[[ "$deadlocks_after_workers" == "$deadlocks_before_workers" ]] ||
  fail "worker load incremented pg_stat_database.deadlocks ($deadlocks_before_workers -> $deadlocks_after_workers)"

echo "PASS: balance-sweep deadlock verification"
echo "  deterministic legacy deadlock: reproduced"
echo "  deterministic scoped lock:     no deadlock"
echo "  actual worker rounds:           $round_count"
echo "  targets per worker round:       $target_count"
echo "  worker deadlock delta:           0"
