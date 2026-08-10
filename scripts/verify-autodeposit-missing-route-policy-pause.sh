#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
trigger_source="$repo_root/crates/balance-sweep-autodeposit-trigger/src/main.rs"
trigger_bin="$repo_root/target/debug/balance-sweep-autodeposit-trigger"
usdc_mint="EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

for command_name in cargo initdb pg_ctl psql rg; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

rg --quiet "pause_targets_without_active_earn_route_policy_once" "$trigger_source" ||
  fail "the worker does not pause targets with missing route policies"
if rg --quiet '"autodeposit_route_policy_missing"' "$trigger_source"; then
  fail "the old repeating missing-route-policy alert is still present"
fi

echo "Building the production autodeposit worker"
cargo build --quiet -p balance-sweep-autodeposit-trigger

scratch_dir="$(mktemp -d "/tmp/autodeposit-missing-policy.XXXXXX")"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
mkdir -p "$socket_dir"
port="$((56432 + RANDOM % 800))"
server_started=0
server_log="$scratch_dir/postgres.log"

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    pg_ctl -D "$data_dir" -m fast -w stop >/dev/null
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

initdb -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
if ! pg_ctl -D "$data_dir" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1" \
  -l "$server_log" -w start >/dev/null; then
  tail -40 "$server_log" >&2
  fail "temporary PostgreSQL server did not start"
fi
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

psql "${psql_args[@]}" --set=usdc_mint="$usdc_mint" >/dev/null <<'SQL'
CREATE SCHEMA loyal_yield;

CREATE TYPE loyal_yield.balance_sweep_surplus_classification AS ENUM (
  'earn_withdrawal', 'simple_inbound', 'complex_defi', 'unknown', 'explicit_redeposit'
);
CREATE TYPE loyal_yield.balance_sweep_surplus_lot_status AS ENUM (
  'open', 'selected', 'consumed', 'depleted', 'suppressed'
);
CREATE TYPE loyal_yield.balance_sweep_scheduled_slot_status AS ENUM (
  'scheduled', 'requested', 'selected', 'executed', 'failed', 'released', 'canceled'
);

CREATE TABLE loyal_yield.route_policies (
  id BIGINT PRIMARY KEY,
  settings TEXT NOT NULL,
  authority TEXT NOT NULL,
  vault_index SMALLINT NOT NULL,
  vault_pubkey TEXT NOT NULL,
  route_modes TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
  active BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE loyal_yield.managed_vaults (
  id BIGINT PRIMARY KEY,
  settings TEXT NOT NULL,
  vault_index SMALLINT NOT NULL,
  vault_pubkey TEXT NOT NULL,
  active_policy_id BIGINT NOT NULL REFERENCES loyal_yield.route_policies(id),
  active BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE loyal_yield.balance_sweep_targets (
  id BIGINT PRIMARY KEY,
  settings TEXT NOT NULL,
  authority TEXT NOT NULL,
  vault_index SMALLINT NOT NULL,
  vault_pubkey TEXT NOT NULL,
  token_mint TEXT NOT NULL,
  wallet_balance_floor_raw BIGINT,
  active BOOLEAN NOT NULL DEFAULT TRUE,
  lifecycle_status TEXT NOT NULL DEFAULT 'active',
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE loyal_yield.balance_sweep_wallet_balance_events (
  event_id BIGINT PRIMARY KEY,
  target_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_targets(id),
  amount_raw BIGINT NOT NULL,
  delta_amount_raw BIGINT,
  observed_at TIMESTAMPTZ NOT NULL,
  txn_signature TEXT,
  mint TEXT NOT NULL
);

CREATE TABLE loyal_yield.projection_offsets (
  consumer_name TEXT PRIMARY KEY,
  last_event_id BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE loyal_yield.balance_sweep_scheduled_slots (
  id BIGSERIAL PRIMARY KEY,
  target_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_targets(id),
  token_mint TEXT NOT NULL,
  eligible_after TIMESTAMPTZ NOT NULL,
  status loyal_yield.balance_sweep_scheduled_slot_status NOT NULL DEFAULT 'scheduled',
  last_error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE loyal_yield.balance_sweep_surplus_lots (
  id BIGSERIAL PRIMARY KEY,
  target_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_targets(id),
  source_event_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_wallet_balance_events(event_id),
  source_signature TEXT,
  original_amount_raw BIGINT NOT NULL,
  remaining_amount_raw BIGINT NOT NULL,
  classification loyal_yield.balance_sweep_surplus_classification NOT NULL,
  eligible_after TIMESTAMPTZ NOT NULL,
  status loyal_yield.balance_sweep_surplus_lot_status NOT NULL DEFAULT 'open',
  confidence TEXT NOT NULL,
  reason TEXT NOT NULL,
  scheduled_slot_id BIGINT REFERENCES loyal_yield.balance_sweep_scheduled_slots(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_event_id)
);

INSERT INTO loyal_yield.balance_sweep_targets (
  id, settings, authority, vault_index, vault_pubkey, token_mint,
  wallet_balance_floor_raw, active, lifecycle_status
)
VALUES
  (0, 'settings-batch-blocker', 'authority-batch-blocker', 1, 'vault-batch-blocker', :'usdc_mint', 0, true, 'active'),
  (1, 'settings-missing', 'authority-missing', 1, 'vault-missing', :'usdc_mint', 0, true, 'active'),
  (2, 'settings-healthy', 'authority-healthy', 1, 'vault-healthy', :'usdc_mint', 0, true, 'active'),
  (3, 'settings-user-off', 'authority-user-off', 1, 'vault-user-off', :'usdc_mint', 0, false, 'active'),
  (4, 'settings-inflight', 'authority-inflight', 1, 'vault-inflight', :'usdc_mint', 0, true, 'active');

INSERT INTO loyal_yield.route_policies (
  id, settings, authority, vault_index, vault_pubkey, route_modes, active
)
VALUES (
  20, 'settings-healthy', 'authority-healthy', 1, 'vault-healthy',
  ARRAY['same_mint_kamino'], true
);
INSERT INTO loyal_yield.managed_vaults (
  id, settings, vault_index, vault_pubkey, active_policy_id, active
)
VALUES (20, 'settings-healthy', 1, 'vault-healthy', 20, true);

INSERT INTO loyal_yield.balance_sweep_wallet_balance_events (
  event_id, target_id, amount_raw, delta_amount_raw, observed_at, txn_signature, mint
)
VALUES
  (1, 1, 100, 100, now() - interval '2 hours', 'missing-old', :'usdc_mint'),
  (2, 4, 100, 100, now() - interval '2 hours', 'inflight-old', :'usdc_mint'),
  (101, 1, 200, 100, now(), 'missing-new', :'usdc_mint'),
  (102, 2, 200, 100, now(), 'healthy-new', :'usdc_mint'),
  (103, 3, 200, 100, now(), 'user-off-new', :'usdc_mint');

INSERT INTO loyal_yield.balance_sweep_scheduled_slots (
  id, target_id, token_mint, eligible_after, status, last_error
)
VALUES
  (11, 1, :'usdc_mint', now() - interval '1 hour', 'failed', 'old missing-policy failure'),
  (14, 4, :'usdc_mint', now() - interval '1 hour', 'selected', null);

INSERT INTO loyal_yield.balance_sweep_surplus_lots (
  id, target_id, source_event_id, original_amount_raw, remaining_amount_raw,
  classification, eligible_after, status, confidence, reason, scheduled_slot_id
)
VALUES
  (11, 1, 1, 100, 100, 'simple_inbound', now(), 'open', 'verified', 'old pending work', 11),
  (14, 4, 2, 100, 100, 'simple_inbound', now(), 'selected', 'verified', 'in-flight work', 14);

INSERT INTO loyal_yield.projection_offsets (consumer_name, last_event_id)
VALUES ('balance_sweep_autodeposit_trigger', 100);
SQL

run_worker() {
  local batch_limit="$1"
  "$trigger_bin" \
    --postgres-url "$database_url" \
    --batch-limit "$batch_limit" \
    --disable-realtime-listen \
    --once
}

scalar() {
  psql "${psql_args[@]}" --tuples-only --no-align --command="$1"
}

echo "Running the worker against the isolated database"
guard_log="$scratch_dir/guard-run.log"
run_worker 1 >"$guard_log" 2>&1

[[ "$(scalar "SELECT active || ':' || lifecycle_status FROM loyal_yield.balance_sweep_targets WHERE id = 0")" == "false:paused_missing_position" ]] ||
  fail "the bounded pause pass did not pause its first missing-policy target"
[[ "$(scalar "SELECT active || ':' || lifecycle_status FROM loyal_yield.balance_sweep_targets WHERE id = 1")" == "true:active" ]] ||
  fail "the projection guard scenario did not leave the second target for a later pause pass"
[[ "$(scalar "SELECT COUNT(*) FROM loyal_yield.balance_sweep_surplus_lots WHERE source_event_id = 101")" == "0" ]] ||
  fail "pending work was created before the bounded pause pass reached the target"

pause_log="$scratch_dir/pause-run.log"
run_worker 100 >"$pause_log" 2>&1

[[ "$(scalar "SELECT active || ':' || lifecycle_status FROM loyal_yield.balance_sweep_targets WHERE id = 1")" == "false:paused_missing_position" ]] ||
  fail "missing-policy target was not disabled"
[[ "$(scalar "SELECT status FROM loyal_yield.balance_sweep_surplus_lots WHERE id = 11")" == "suppressed" ]] ||
  fail "old pending work was not suppressed"
[[ "$(scalar "SELECT status FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = 11")" == "canceled" ]] ||
  fail "old failed slot was not canceled"
[[ "$(scalar "SELECT COUNT(*) FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id = 1 AND status = 'open'")" == "0" ]] ||
  fail "new pending work was created for the missing-policy target"
[[ "$(scalar "SELECT active || ':' || lifecycle_status FROM loyal_yield.balance_sweep_targets WHERE id = 2")" == "true:active" ]] ||
  fail "healthy target was paused"
[[ "$(scalar "SELECT COUNT(*) FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id = 2 AND status = 'open'")" == "1" ]] ||
  fail "healthy target did not create pending work"
[[ "$(scalar "SELECT active || ':' || lifecycle_status FROM loyal_yield.balance_sweep_targets WHERE id = 3")" == "false:active" ]] ||
  fail "user-disabled target was changed"
[[ "$(scalar "SELECT active || ':' || lifecycle_status FROM loyal_yield.balance_sweep_targets WHERE id = 4")" == "false:paused_missing_position" ]] ||
  fail "missing-policy target with in-flight work was not disabled"
[[ "$(scalar "SELECT status FROM loyal_yield.balance_sweep_surplus_lots WHERE id = 14")" == "selected" ]] ||
  fail "in-flight work was changed"

paused_at="$(scalar "SELECT last_seen_at::text FROM loyal_yield.balance_sweep_targets WHERE id = 1")"
psql "${psql_args[@]}" --set=usdc_mint="$usdc_mint" >/dev/null <<'SQL'
INSERT INTO loyal_yield.balance_sweep_wallet_balance_events (
  event_id, target_id, amount_raw, delta_amount_raw, observed_at, txn_signature, mint
)
VALUES (200, 1, 300, 100, now(), 'missing-after-pause', :'usdc_mint');
SQL

post_pause_log="$scratch_dir/post-pause-run.log"
run_worker 100 >"$post_pause_log" 2>&1

[[ "$(scalar "SELECT last_seen_at::text FROM loyal_yield.balance_sweep_targets WHERE id = 1")" == "$paused_at" ]] ||
  fail "the worker retried the already-paused target"
[[ "$(scalar "SELECT COUNT(*) FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id = 1 AND status = 'open'")" == "0" ]] ||
  fail "new money created work after automatic deposits were disabled"
[[ "$(scalar "SELECT last_event_id FROM loyal_yield.projection_offsets WHERE consumer_name = 'balance_sweep_autodeposit_trigger'")" == "200" ]] ||
  fail "the worker did not safely consume the post-pause wallet event"

echo "PASS: missing route policy disables automatic deposits before pending work is created"
echo "PASS: old pending work is cleared without changing in-flight work"
echo "PASS: later wallet activity does not retry the paused target"
