#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

for command_name in initdb pg_ctl createdb psql rg; do
  command -v "$command_name" >/dev/null || {
    echo "FAIL: $command_name is required" >&2
    exit 1
  }
done

baseline_migration="crates/loyal-yield-store/migrations/0045_atomic_autodeposit_finalization.sql"
fix_migration="crates/loyal-yield-store/migrations/0047_unambiguous_autodeposit_finalization.sql"
[[ -f "$baseline_migration" ]] || {
  echo "FAIL: missing migration 0045 atomic autodeposit finalization" >&2
  exit 1
}
[[ -f "$fix_migration" ]] || {
  echo "FAIL: missing migration 0047 unambiguous autodeposit finalization" >&2
  exit 1
}

rg -q 'version: 47,' crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs || {
  echo "FAIL: production migration runner does not register migration 47" >&2
  exit 1
}
rg -q 'finalize_confirmed_autodeposit' scripts/execute-autodeposit-policy.ts || {
  echo "FAIL: TypeScript executor does not call the atomic finalizer" >&2
  exit 1
}
rg -q '"--read-only"' scripts/execute-autodeposit-policy.ts || {
  echo "FAIL: chain reconciliation is not invoked in read-only mode" >&2
  exit 1
}

reconcile_body="$(sed -n '/async fn run_reconcile_current_positions_flow/,/^async fn /p' crates/loyal-fleet-worker/src/lib.rs)"
if grep -q 'apply_observed_patch' <<<"$reconcile_body"; then
  echo "FAIL: chain reconciliation still writes current positions" >&2
  exit 1
fi
grep -q 'writesCurrentPositions.*false' <<<"$reconcile_body" || {
  echo "FAIL: chain reconciliation does not declare itself read-only" >&2
  exit 1
}

rg -q 'AUTODEPOSIT_YIELD_PERSISTENCE_FAILED_EXIT_CODE: i32 = 21' \
  crates/balance-sweep-autodeposit-trigger/src/lib.rs || {
  echo "FAIL: deterministic post-confirm persistence failures are not exit 21" >&2
  exit 1
}
rg -q 'yield_persistence_failed' crates/balance-sweep-autodeposit-trigger/src || {
  echo "FAIL: trigger does not map the existing yield persistence alert" >&2
  exit 1
}

runtime_tmp_root="${AUTODEPOSIT_FINALIZER_RUNTIME_TMPDIR:-/tmp}"
scratch_dir="$(mktemp -d "$runtime_tmp_root/autodeposit-finalizer.XXXXXX")"
data_dir="$scratch_dir/postgres"
socket_dir="$scratch_dir/socket"
mkdir -p "$socket_dir"
port="$((59900 + RANDOM % 80))"
server_started=0

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    pg_ctl -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  if [[ "$scratch_dir" == "$runtime_tmp_root/autodeposit-finalizer."* ]]; then
    rm -rf "$scratch_dir"
  fi
}
trap cleanup EXIT

initdb -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
pg_ctl -D "$data_dir" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1" \
  -w start >/dev/null
server_started=1
createdb -h "$socket_dir" -p "$port" autodeposit_finalizer
database_url="postgresql://$(id -un)@127.0.0.1:$port/autodeposit_finalizer"

psql "$database_url" -X -v ON_ERROR_STOP=1 <<'SQL' >/dev/null
CREATE SCHEMA loyal_yield;
CREATE TYPE loyal_yield.balance_sweep_lot_claim_status AS ENUM ('selected', 'executed', 'released');
CREATE TYPE loyal_yield.balance_sweep_scheduled_slot_status AS ENUM ('scheduled', 'requested', 'selected', 'executed', 'failed');
CREATE TYPE loyal_yield.yield_position_status AS ENUM ('active', 'closed');
CREATE TYPE loyal_yield.user_yield_holding_event_type AS ENUM ('deposit_initialized', 'deposit_top_up', 'rebalance', 'withdrawal');

CREATE TABLE loyal_yield.balance_sweep_lot_claims (
  claim_token text PRIMARY KEY,
  target_id bigint NOT NULL,
  amount_raw bigint NOT NULL,
  status loyal_yield.balance_sweep_lot_claim_status NOT NULL DEFAULT 'selected',
  execution_id bigint,
  autodeposit_executor_lease_token text,
  autodeposit_executor_lease_expires_at timestamptz,
  autodeposit_deposit_plan jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE loyal_yield.balance_sweep_lot_claim_items (
  claim_token text NOT NULL,
  lot_id bigint NOT NULL,
  amount_raw bigint NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (claim_token, lot_id)
);
CREATE TABLE loyal_yield.balance_sweep_executions (
  id bigint PRIMARY KEY,
  decoded_evidence jsonb NOT NULL DEFAULT '{}'::jsonb,
  decoded_at timestamptz,
  yield_deposit_id bigint,
  yield_position_id bigint,
  kamino_deposit_signature text,
  completed_at timestamptz,
  completion_failure_code text
);
CREATE TABLE loyal_yield.balance_sweep_execution_lots (
  execution_id bigint NOT NULL,
  lot_id bigint NOT NULL,
  amount_raw bigint NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (execution_id, lot_id)
);
CREATE TABLE loyal_yield.balance_sweep_scheduled_slots (
  id bigint PRIMARY KEY,
  status loyal_yield.balance_sweep_scheduled_slot_status NOT NULL DEFAULT 'selected',
  claim_token text,
  execution_id bigint,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE loyal_yield.balance_sweep_transaction_attempts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  claim_token text NOT NULL,
  execution_id bigint NOT NULL,
  operation_kind text NOT NULL,
  attempt_number integer NOT NULL,
  attempt_state text NOT NULL,
  signature text NOT NULL,
  confirmed_slot bigint
);
CREATE TABLE loyal_yield.user_yield_position_deposits (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  deposit_signature text NOT NULL UNIQUE,
  policy_signature text NOT NULL,
  confirmed_slot bigint NOT NULL,
  wallet_address text NOT NULL,
  smart_account_address text NOT NULL,
  settings text NOT NULL,
  vault_index smallint NOT NULL,
  vault_pubkey text NOT NULL,
  policy_id bigint NOT NULL,
  policy_account text NOT NULL,
  policy_seed bigint NOT NULL,
  target_reserve text NOT NULL,
  market text,
  liquidity_mint text NOT NULL,
  target_supply_apy_bps bigint,
  deposit_mint text NOT NULL,
  principal_amount_raw bigint NOT NULL,
  balance_sweep_execution_id bigint,
  balance_sweep_scheduled_slot_id bigint,
  confirmed_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL
);
CREATE TABLE loyal_yield.user_yield_positions (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  wallet_address text NOT NULL,
  smart_account_address text NOT NULL,
  settings text NOT NULL,
  vault_index smallint NOT NULL,
  vault_pubkey text NOT NULL,
  policy_id bigint NOT NULL,
  policy_account text NOT NULL,
  policy_seed bigint NOT NULL,
  initial_reserve text NOT NULL,
  initial_market text,
  initial_liquidity_mint text NOT NULL,
  initial_supply_apy_bps bigint,
  deposit_mint text NOT NULL,
  principal_amount_raw bigint NOT NULL,
  current_reserve text NOT NULL,
  current_market text,
  current_liquidity_mint text NOT NULL,
  current_amount_raw bigint NOT NULL,
  current_observed_slot bigint NOT NULL,
  current_observed_at timestamptz NOT NULL,
  first_deposit_signature text NOT NULL,
  last_deposit_signature text NOT NULL,
  last_confirmed_slot bigint NOT NULL,
  last_holding_event_id bigint,
  status loyal_yield.yield_position_status NOT NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX user_yield_positions_initial_uidx
  ON loyal_yield.user_yield_positions (settings, vault_index, initial_reserve);
CREATE TABLE loyal_yield.user_yield_position_holding_events (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  position_id bigint NOT NULL,
  event_type loyal_yield.user_yield_holding_event_type NOT NULL,
  reserve text NOT NULL,
  market text,
  liquidity_mint text NOT NULL,
  amount_raw bigint NOT NULL,
  principal_delta_raw bigint,
  holding_delta_raw bigint,
  observed_slot bigint NOT NULL,
  observed_at timestamptz NOT NULL,
  source_signature text,
  source_deposit_id bigint,
  created_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX user_yield_holding_source_signature_uidx
  ON loyal_yield.user_yield_position_holding_events (source_signature)
  WHERE source_signature IS NOT NULL;
SQL

psql "$database_url" -X -v ON_ERROR_STOP=1 -f "$baseline_migration" >/dev/null
psql "$database_url" -X -v ON_ERROR_STOP=1 -f "$fix_migration" >/dev/null

psql "$database_url" -X -v ON_ERROR_STOP=1 <<'SQL' >/dev/null
CREATE FUNCTION verifier_plan(amount_raw bigint, settings text, vault_index integer, reserve text)
RETURNS jsonb LANGUAGE sql IMMUTABLE AS $$
  SELECT jsonb_build_object(
    'amountRaw', amount_raw::text,
    'reserve', reserve,
    'market', 'market-1',
    'liquidityMint', 'liquidity-1',
    'target', jsonb_build_object(
      'wallet', 'wallet-1',
      'vaultPubkey', 'vault-1',
      'settings', settings,
      'vaultIndex', vault_index,
      'routePolicySeed', '7',
      'routePolicyAccount', 'policy-1'
    )
  )
$$;

INSERT INTO loyal_yield.user_yield_positions (
  wallet_address, smart_account_address, settings, vault_index, vault_pubkey,
  policy_id, policy_account, policy_seed, initial_reserve, initial_market,
  initial_liquidity_mint, deposit_mint, principal_amount_raw, current_reserve,
  current_market, current_liquidity_mint, current_amount_raw, current_observed_slot,
  current_observed_at, first_deposit_signature, last_deposit_signature,
  last_confirmed_slot, status, created_at, updated_at
) VALUES (
  'wallet-1', 'vault-1', 'settings-1', 1, 'vault-1', 7, 'policy-1', 7,
  'reserve-1', 'market-1', 'liquidity-1', 'liquidity-1', 100,
  'reserve-1', 'market-1', 'liquidity-1', 100, 1000, now(),
  'initial-signature', 'initial-signature', 1000, 'active', now(), now()
);

INSERT INTO loyal_yield.balance_sweep_lot_claims
  (claim_token, target_id, amount_raw, status, autodeposit_executor_lease_token,
   autodeposit_executor_lease_expires_at, autodeposit_deposit_plan)
VALUES ('claim-1', 1, 10, 'selected', 'lease-1', now() + interval '10 minutes', verifier_plan(10, 'settings-1', 1, 'reserve-1'));
INSERT INTO loyal_yield.balance_sweep_lot_claim_items VALUES ('claim-1', 101, 10, now());
INSERT INTO loyal_yield.balance_sweep_executions (id) VALUES (1);
INSERT INTO loyal_yield.balance_sweep_scheduled_slots (id, status, claim_token) VALUES (1, 'selected', 'claim-1');
INSERT INTO loyal_yield.balance_sweep_transaction_attempts
  (claim_token, execution_id, operation_kind, attempt_number, attempt_state, signature, confirmed_slot)
VALUES ('claim-1', 1, 'top_up', 1, 'confirmed', 'top-up-1', 1100);

SELECT loyal_yield.finalize_confirmed_autodeposit('claim-1', 1, 1, 'lease-1', 110, 1100);

DO $$
DECLARE snapshot jsonb;
BEGIN
  SELECT jsonb_build_object(
    'principal', (SELECT principal_amount_raw FROM loyal_yield.user_yield_positions WHERE settings = 'settings-1'),
    'current', (SELECT current_amount_raw FROM loyal_yield.user_yield_positions WHERE settings = 'settings-1'),
    'deposits', (SELECT count(*) FROM loyal_yield.user_yield_position_deposits WHERE deposit_signature = 'top-up-1'),
    'events', (SELECT count(*) FROM loyal_yield.user_yield_position_holding_events WHERE source_signature = 'top-up-1'),
    'lots', (SELECT count(*) FROM loyal_yield.balance_sweep_execution_lots WHERE execution_id = 1),
    'claim', (SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = 'claim-1'),
    'slot', (SELECT status::text FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = 1),
    'execution', (SELECT kamino_deposit_signature FROM loyal_yield.balance_sweep_executions WHERE id = 1)
  ) INTO snapshot;
  IF snapshot <> '{"principal": 110, "current": 110, "deposits": 1, "events": 1, "lots": 1, "claim": "executed", "slot": "executed", "execution": "top-up-1"}'::jsonb THEN
    RAISE EXCEPTION 'first finalization mismatch: %', snapshot;
  END IF;
END $$;

-- Replaying an already completed signature is a no-op.
SELECT loyal_yield.finalize_confirmed_autodeposit('claim-1', 1, 1, 'lease-1', 110, 1100);
DO $$
BEGIN
  IF (SELECT principal_amount_raw FROM loyal_yield.user_yield_positions WHERE settings = 'settings-1') <> 110
     OR (SELECT count(*) FROM loyal_yield.user_yield_position_deposits WHERE deposit_signature = 'top-up-1') <> 1
     OR (SELECT count(*) FROM loyal_yield.user_yield_position_holding_events WHERE source_signature = 'top-up-1') <> 1
     OR (SELECT count(*) FROM loyal_yield.balance_sweep_execution_lots WHERE execution_id = 1) <> 1 THEN
    RAISE EXCEPTION 'replay was not idempotent';
  END IF;
END $$;

-- A late failure must roll back every earlier accounting mutation.
INSERT INTO loyal_yield.balance_sweep_lot_claims
  (claim_token, target_id, amount_raw, status, autodeposit_executor_lease_token,
   autodeposit_executor_lease_expires_at, autodeposit_deposit_plan)
VALUES ('claim-2', 1, 5, 'selected', 'lease-2', now() + interval '10 minutes', verifier_plan(5, 'settings-1', 1, 'reserve-1'));
INSERT INTO loyal_yield.balance_sweep_lot_claim_items VALUES ('claim-2', 102, 5, now());
INSERT INTO loyal_yield.balance_sweep_executions (id) VALUES (2);
INSERT INTO loyal_yield.balance_sweep_scheduled_slots (id, status, claim_token) VALUES (2, 'selected', 'claim-2');
INSERT INTO loyal_yield.balance_sweep_transaction_attempts
  (claim_token, execution_id, operation_kind, attempt_number, attempt_state, signature, confirmed_slot)
VALUES ('claim-2', 2, 'top_up', 1, 'confirmed', 'top-up-2', 1200);
CREATE FUNCTION verifier_reject_execution_two() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id = 2 AND NEW.completed_at IS NOT NULL THEN
    RAISE EXCEPTION 'forced late finalization failure';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER verifier_reject_execution_two BEFORE UPDATE ON loyal_yield.balance_sweep_executions
FOR EACH ROW EXECUTE FUNCTION verifier_reject_execution_two();
DO $$
BEGIN
  BEGIN
    PERFORM loyal_yield.finalize_confirmed_autodeposit('claim-2', 2, 2, 'lease-2', 115, 1200);
    RAISE EXCEPTION 'forced failure unexpectedly succeeded';
  EXCEPTION WHEN OTHERS THEN
    IF SQLERRM = 'forced failure unexpectedly succeeded' THEN RAISE; END IF;
  END;
  IF (SELECT principal_amount_raw FROM loyal_yield.user_yield_positions WHERE settings = 'settings-1') <> 110
     OR EXISTS (SELECT 1 FROM loyal_yield.user_yield_position_deposits WHERE deposit_signature = 'top-up-2')
     OR EXISTS (SELECT 1 FROM loyal_yield.user_yield_position_holding_events WHERE source_signature = 'top-up-2')
     OR (SELECT status FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = 'claim-2') <> 'selected'
     OR (SELECT status FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = 2) <> 'selected' THEN
    RAISE EXCEPTION 'late failure leaked partial state';
  END IF;
END $$;
DROP TRIGGER verifier_reject_execution_two ON loyal_yield.balance_sweep_executions;

-- Recover the historical partial-write shape without double-counting principal.
INSERT INTO loyal_yield.balance_sweep_lot_claims
  (claim_token, target_id, amount_raw, status, autodeposit_executor_lease_token,
   autodeposit_executor_lease_expires_at, autodeposit_deposit_plan)
VALUES ('claim-3', 1, 10, 'selected', 'lease-3', now() + interval '10 minutes', verifier_plan(10, 'settings-1', 1, 'reserve-1'));
INSERT INTO loyal_yield.balance_sweep_lot_claim_items VALUES ('claim-3', 103, 10, now());
INSERT INTO loyal_yield.balance_sweep_executions (id) VALUES (3);
INSERT INTO loyal_yield.balance_sweep_scheduled_slots (id, status, claim_token) VALUES (3, 'selected', 'claim-3');
INSERT INTO loyal_yield.balance_sweep_transaction_attempts
  (claim_token, execution_id, operation_kind, attempt_number, attempt_state, signature, confirmed_slot)
VALUES ('claim-3', 3, 'top_up', 1, 'confirmed', 'top-up-3', 1300);
INSERT INTO loyal_yield.user_yield_position_deposits (
  deposit_signature, policy_signature, confirmed_slot, wallet_address,
  smart_account_address, settings, vault_index, vault_pubkey, policy_id,
  policy_account, policy_seed, target_reserve, market, liquidity_mint,
  deposit_mint, principal_amount_raw, confirmed_at, created_at
) VALUES (
  'top-up-3', 'top-up-3', 1300, 'wallet-1', 'vault-1', 'settings-1', 1,
  'vault-1', 7, 'policy-1', 7, 'reserve-1', 'market-1', 'liquidity-1',
  'liquidity-1', 10, now(), now()
);
UPDATE loyal_yield.user_yield_positions
SET principal_amount_raw = 120, current_amount_raw = 120,
    last_deposit_signature = 'top-up-3', last_confirmed_slot = 1300
WHERE settings = 'settings-1' AND vault_index = 1 AND initial_reserve = 'reserve-1';

INSERT INTO loyal_yield.user_yield_position_holding_events (
  position_id, event_type, reserve, market, liquidity_mint, amount_raw,
  principal_delta_raw, holding_delta_raw, observed_slot, observed_at,
  source_signature, source_deposit_id, created_at
)
SELECT position.id, 'deposit_top_up', 'reserve-1', 'market-1', 'liquidity-1',
       120, 10, 10, 1300, now(), 'top-up-3', deposit.id, now()
FROM loyal_yield.user_yield_positions AS position
JOIN loyal_yield.user_yield_position_deposits AS deposit
  ON deposit.deposit_signature = 'top-up-3'
WHERE position.settings = 'settings-1'
  AND position.vault_index = 1
  AND position.initial_reserve = 'reserve-1';

SELECT loyal_yield.finalize_confirmed_autodeposit('claim-3', 3, 3, 'lease-3', 120, 1300);
DO $$
BEGIN
  IF (SELECT principal_amount_raw FROM loyal_yield.user_yield_positions WHERE settings = 'settings-1') <> 120
     OR (SELECT count(*) FROM loyal_yield.user_yield_positions WHERE settings = 'settings-1') <> 1
     OR (SELECT count(*) FROM loyal_yield.user_yield_position_deposits WHERE deposit_signature = 'top-up-3') <> 1
     OR (SELECT count(*) FROM loyal_yield.user_yield_position_holding_events WHERE source_signature = 'top-up-3') <> 1
     OR (SELECT status FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = 'claim-3') <> 'executed' THEN
    RAISE EXCEPTION 'partial-state recovery mismatch';
  END IF;
END $$;

-- A competing executor must not mutate anything.
INSERT INTO loyal_yield.balance_sweep_lot_claims
  (claim_token, target_id, amount_raw, status, autodeposit_executor_lease_token,
   autodeposit_executor_lease_expires_at, autodeposit_deposit_plan)
VALUES ('claim-4', 1, 1, 'selected', 'winning-lease', now() + interval '10 minutes', verifier_plan(1, 'settings-1', 1, 'reserve-1'));
INSERT INTO loyal_yield.balance_sweep_executions (id) VALUES (4);
INSERT INTO loyal_yield.balance_sweep_scheduled_slots (id, status, claim_token) VALUES (4, 'selected', 'claim-4');
INSERT INTO loyal_yield.balance_sweep_transaction_attempts
  (claim_token, execution_id, operation_kind, attempt_number, attempt_state, signature, confirmed_slot)
VALUES ('claim-4', 4, 'top_up', 1, 'confirmed', 'top-up-4', 1400);
DO $$
BEGIN
  BEGIN
    PERFORM loyal_yield.finalize_confirmed_autodeposit('claim-4', 4, 4, 'losing-lease', 121, 1400);
    RAISE EXCEPTION 'losing lease unexpectedly succeeded';
  EXCEPTION WHEN SQLSTATE '55P03' THEN
    NULL;
  END;
  IF EXISTS (SELECT 1 FROM loyal_yield.user_yield_position_deposits WHERE deposit_signature = 'top-up-4') THEN
    RAISE EXCEPTION 'losing lease mutated accounting';
  END IF;
END $$;

-- A first autodeposit creates and links the complete accounting graph.
INSERT INTO loyal_yield.balance_sweep_lot_claims
  (claim_token, target_id, amount_raw, status, autodeposit_executor_lease_token,
   autodeposit_executor_lease_expires_at, autodeposit_deposit_plan)
VALUES ('claim-5', 2, 7, 'selected', 'lease-5', now() + interval '10 minutes', verifier_plan(7, 'settings-2', 1, 'reserve-2'));
INSERT INTO loyal_yield.balance_sweep_lot_claim_items VALUES ('claim-5', 105, 7, now());
INSERT INTO loyal_yield.balance_sweep_executions (id) VALUES (5);
INSERT INTO loyal_yield.balance_sweep_scheduled_slots (id, status, claim_token) VALUES (5, 'selected', 'claim-5');
INSERT INTO loyal_yield.balance_sweep_transaction_attempts
  (claim_token, execution_id, operation_kind, attempt_number, attempt_state, signature, confirmed_slot)
VALUES ('claim-5', 5, 'top_up', 1, 'confirmed', 'top-up-5', 1500);

SELECT loyal_yield.finalize_confirmed_autodeposit('claim-5', 5, 5, 'lease-5', 7, 1500);
DO $$
DECLARE snapshot jsonb;
BEGIN
  SELECT jsonb_build_object(
    'positions', (SELECT count(*) FROM loyal_yield.user_yield_positions WHERE settings = 'settings-2'),
    'principal', (SELECT principal_amount_raw FROM loyal_yield.user_yield_positions WHERE settings = 'settings-2'),
    'current', (SELECT current_amount_raw FROM loyal_yield.user_yield_positions WHERE settings = 'settings-2'),
    'deposits', (SELECT count(*) FROM loyal_yield.user_yield_position_deposits WHERE deposit_signature = 'top-up-5'),
    'events', (SELECT count(*) FROM loyal_yield.user_yield_position_holding_events WHERE source_signature = 'top-up-5'),
    'claim', (SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = 'claim-5'),
    'slot', (SELECT status::text FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = 5),
    'execution_complete', (SELECT completed_at IS NOT NULL FROM loyal_yield.balance_sweep_executions WHERE id = 5),
    'execution_linked', (SELECT yield_deposit_id IS NOT NULL AND yield_position_id IS NOT NULL FROM loyal_yield.balance_sweep_executions WHERE id = 5)
  ) INTO snapshot;
  IF snapshot <> '{"positions": 1, "principal": 7, "current": 7, "deposits": 1, "events": 1, "claim": "executed", "slot": "executed", "execution_complete": true, "execution_linked": true}'::jsonb THEN
    RAISE EXCEPTION 'new-position finalization mismatch: %', snapshot;
  END IF;
END $$;
SQL

echo PASS_AUTODEPOSIT_FINALIZATION_E2E
