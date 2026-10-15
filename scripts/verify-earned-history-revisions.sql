\set ON_ERROR_STOP on
-- Run only in a disposable database named earn_checkpoint_test.
DO $$ BEGIN IF current_database() <> 'earn_checkpoint_test' THEN RAISE EXCEPTION 'disposable database required'; END IF; END $$;
BEGIN;
CREATE SCHEMA loyal_yield;
CREATE TABLE loyal_yield.managed_vaults (id bigint PRIMARY KEY, settings text, vault_index smallint);
CREATE TABLE loyal_yield.user_yield_positions (id bigint PRIMARY KEY, settings text, vault_index smallint, vault_pubkey text, initial_reserve text, initial_market text, initial_liquidity_mint text, principal_amount_raw bigint);
CREATE TABLE loyal_yield.user_yield_position_deposits (id bigint PRIMARY KEY, settings text, vault_index smallint, confirmed_at timestamptz, principal_amount_raw bigint);
CREATE TABLE loyal_yield.user_yield_position_withdrawals (LIKE loyal_yield.user_yield_position_deposits INCLUDING ALL);
CREATE TABLE loyal_yield.user_yield_position_holding_events (id bigint PRIMARY KEY, position_id bigint, observed_at timestamptz, amount_raw bigint);
CREATE TABLE loyal_yield.vault_position_snapshots (id bigint PRIMARY KEY, vault_id bigint, observed_at timestamptz, observed_slot bigint, context jsonb, is_current boolean);
CREATE TABLE loyal_yield.vault_position_snapshot_positions (id bigint PRIMARY KEY, snapshot_id bigint, reserve text, liquidity_mint text, amount_raw bigint, has_value boolean);
CREATE TABLE loyal_yield.vault_idle_token_balances_current (vault_id bigint, mint text, observed_at timestamptz, amount_raw bigint);
\ir ../migrations/yield/0115_earn_history_day_revisions.sql
INSERT INTO loyal_yield.managed_vaults VALUES (1, 'wallet-a', 1), (2, 'wallet-b', 1);
INSERT INTO loyal_yield.user_yield_positions VALUES (1, 'wallet-a', 1, 'vault', 'reserve', 'market', 'USDC', 100);
INSERT INTO loyal_yield.user_yield_position_deposits VALUES (1, 'wallet-a', 1, '2026-01-01Z', 100);
INSERT INTO loyal_yield.user_yield_position_withdrawals VALUES (1, 'wallet-a', 1, '2026-01-01Z', 10);
INSERT INTO loyal_yield.user_yield_position_holding_events VALUES (1, 1, '2026-01-01Z', 90);
INSERT INTO loyal_yield.vault_position_snapshots VALUES (1, 1, '2026-01-01Z', 1, '{"publication_scope":"complete_product_vault"}', true);
INSERT INTO loyal_yield.vault_position_snapshot_positions VALUES (1, 1, 'reserve', 'USDC', 90, true);
DO $$ DECLARE before_revision bigint; BEGIN
 SELECT revision INTO before_revision FROM loyal_yield.earn_history_day_revisions WHERE settings='wallet-a' AND history_day='2026-01-01';
 ASSERT before_revision = 5, 'all five history sources must invalidate on insertion';
 UPDATE loyal_yield.vault_position_snapshots SET is_current=false;
 UPDATE loyal_yield.user_yield_positions SET principal_amount_raw=80;
 ASSERT (SELECT revision FROM loyal_yield.earn_history_day_revisions WHERE settings='wallet-a' AND history_day='2026-01-01') = before_revision, 'bookkeeping must not invalidate old calculation';
 UPDATE loyal_yield.vault_position_snapshot_positions SET amount_raw=80;
 ASSERT (SELECT revision FROM loyal_yield.earn_history_day_revisions WHERE settings='wallet-a' AND history_day='2026-01-01') = before_revision+1, 'in-place correction must invalidate';
 UPDATE loyal_yield.user_yield_position_deposits SET confirmed_at='2026-01-02Z';
 ASSERT (SELECT revision FROM loyal_yield.earn_history_day_revisions WHERE settings='wallet-a' AND history_day='2026-01-01') = before_revision+2, 'moving a timestamp invalidates its old day';
 ASSERT (SELECT revision FROM loyal_yield.earn_history_day_revisions WHERE settings='wallet-a' AND history_day='2026-01-02') = 1, 'moving a timestamp invalidates its new day';
 DELETE FROM loyal_yield.user_yield_position_holding_events;
 ASSERT (SELECT revision FROM loyal_yield.earn_history_day_revisions WHERE settings='wallet-a' AND history_day='2026-01-01') = before_revision+3, 'deletion invalidates';
 UPDATE loyal_yield.vault_position_snapshots SET vault_id=2;
 ASSERT EXISTS(SELECT 1 FROM loyal_yield.earn_history_day_revisions WHERE settings='wallet-b' AND history_day='2026-01-01'), 'scope move invalidates destination';
 UPDATE loyal_yield.user_yield_positions SET initial_reserve='corrected';
 ASSERT (SELECT revision FROM loyal_yield.earn_history_day_revisions WHERE settings='wallet-a' AND history_day='1970-01-01') = 3, 'initial metadata invalidates all history';
 INSERT INTO loyal_yield.vault_idle_token_balances_current VALUES (1, 'USDC', '2026-01-03Z', 100);
 UPDATE loyal_yield.vault_idle_token_balances_current SET observed_at='2026-01-04Z';
 ASSERT (SELECT revision FROM loyal_yield.earn_history_day_revisions WHERE settings='wallet-a' AND history_day='2026-01-03') = 2, 'idle seed changes invalidate old day';
 ASSERT (SELECT revision FROM loyal_yield.earn_history_day_revisions WHERE settings='wallet-a' AND history_day='2026-01-04') = 1, 'new day tracked independently';
 DELETE FROM loyal_yield.managed_vaults WHERE id=2;
 ASSERT (SELECT revision FROM loyal_yield.earn_history_day_revisions WHERE settings='wallet-b' AND history_day='1970-01-01') = 2, 'parent deletion invalidates even after cascades lose their scope';
END $$;
ROLLBACK;
\echo 'PASS: inserts, corrections, deletes, date and scope moves, metadata and bookkeeping isolation'
