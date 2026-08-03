-- Reproduction and acceptance scenario for migration 0006.
--
-- Run against a throwaway database with migrations 0001-0006 applied:
--
--   createdb loyal_timescale_scratch
--   for f in crates/loyal-timescale-migrations/migrations/000*.sql; do
--     psql -v ON_ERROR_STOP=1 -d loyal_timescale_scratch -f "$f"
--   done
--   psql -v ON_ERROR_STOP=1 -d loyal_timescale_scratch \
--     -f crates/loyal-timescale-migrations/fixtures/0006_verification_slot_tolerance_scenario.sql
--
-- Expected: visible = 1, 1, 1, 0, 0, 0.
-- Against the 0005 view the second case returns 0, which is the livelock this
-- migration removes: a confirmed verification is evicted by the first newer
-- LaserStream observation and the next HTTP read loses the same race.

\set ON_ERROR_STOP on

INSERT INTO kamino.reserve_updates (
  event_id, observed_at, slot, kind, source, source_commitment, reserve, market,
  market_name, symbol, liquidity_mint, mint_decimals, reserve_last_update_slot,
  reserve_last_update_stale, reserve_price_status, available_amount, borrowed_amount,
  borrowed_amount_sf, total_supply_amount, market_price_usd, market_price_last_updated_ts,
  cumulative_borrow_rate_bsf, total_supply_usd_estimate, total_borrow_usd_estimate,
  utilization, borrow_apr, supply_apr, borrow_apy, supply_apy, protocol_take_rate_pct,
  host_fixed_interest_rate_bps, diff_changed, changed_fields, diff_summary, diff, target,
  snapshot, record, account_data_hash
) VALUES (
  1, now(), 1000, 'state', 'http_snapshot', 'confirmed', 'RSV', 'MKT',
  'm', 'USDC', 'MINT', 6, 1000, false, 0, 1, 1, '1', 2, 1.0, 0, '0', 2, 1, 0.5,
  0.05, 0.04, 0.05, 0.04, 0, 0, false, '{}', '', '{}', '{}', '{}', '{}', 'HASH_A'
);

INSERT INTO kamino.reserve_current_states (
  reserve, state_event_id, account_data_hash, state_slot, state_observed_at, state_source
)
SELECT 'RSV', 1, 'HASH_A', 1000, observed_at, 'http_snapshot'
FROM kamino.reserve_updates WHERE event_id = 1;

INSERT INTO kamino.reserve_confirmed_verifications (
  reserve, state_event_id, account_data_hash, verified_slot, verified_at, commitment,
  verification_source
) VALUES ('RSV', 1, 'HASH_A', 1000, now(), 'confirmed', 'http_snapshot');

INSERT INTO kamino.reserve_confirmed_observation_floors (
  reserve, floor_slot, account_data_hash, state_valid, source, source_rank, observed_at
) VALUES ('RSV', 1000, 'HASH_A', true, 'laserstream_grpc', 1, now());

SELECT 'baseline (floor == verified, hash matches)' AS scenario,
       count(*) AS visible FROM kamino.latest_verified_reserve_updates;

-- LaserStream observes a newer state: the floor advances and the account data
-- hash changes. Under 0005 this alone evicted the reserve permanently.
UPDATE kamino.reserve_confirmed_observation_floors
SET floor_slot = 1010, account_data_hash = 'HASH_B' WHERE reserve = 'RSV';
SELECT 'floor +10 slots, hash changed (the livelock)' AS scenario,
       count(*) AS visible FROM kamino.latest_verified_reserve_updates;

UPDATE kamino.reserve_confirmed_observation_floors SET floor_slot = 1150 WHERE reserve = 'RSV';
SELECT 'floor +150 slots (at tolerance bound)' AS scenario,
       count(*) AS visible FROM kamino.latest_verified_reserve_updates;

-- Past the tolerance the verification is genuinely stale and must drop out, so
-- staleness stays bounded rather than merely being tolerated.
UPDATE kamino.reserve_confirmed_observation_floors SET floor_slot = 1151 WHERE reserve = 'RSV';
SELECT 'floor +151 slots (past tolerance)' AS scenario,
       count(*) AS visible FROM kamino.latest_verified_reserve_updates;

-- The tolerance must not swallow the equal-slot fence. Two monitors disagreeing
-- about the same slot is a conflict, not staleness, so it stays fail-closed
-- even though the slot difference is zero and trivially within tolerance.
UPDATE kamino.reserve_confirmed_observation_floors
SET floor_slot = 1000, account_data_hash = 'HASH_B', state_valid = true WHERE reserve = 'RSV';
SELECT 'equal slot, conflicting hash (fence must hold)' AS scenario,
       count(*) AS visible FROM kamino.latest_verified_reserve_updates;

-- An invalid floor is written when the stream saw a wrong-owner or undecodable
-- reserve account, which fences routability immediately by contract. Sitting
-- inside the slot tolerance must not grant it another window of visibility.
UPDATE kamino.reserve_confirmed_observation_floors
SET floor_slot = 1010, account_data_hash = NULL, state_valid = false WHERE reserve = 'RSV';
SELECT 'invalid floor inside tolerance (must fence)' AS scenario,
       count(*) AS visible FROM kamino.latest_verified_reserve_updates;
