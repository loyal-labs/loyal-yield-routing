-- Bounded-tolerance admission for confirmed reserve verifications.
--
-- The confirmed-verification protocol required an HTTP confirmed read to
-- either strictly lead the LaserStream observation floor or to match the
-- floor's account data hash exactly. Neither branch can be made reliable:
-- LaserStream advances floor_slot continuously, so on an active reserve the
-- floor routinely overtakes verified_slot before the HTTP verifier commits,
-- and by then the account data hash has already moved too. The reserve is
-- then evicted from latest_verified_reserve_updates and cannot re-enter,
-- because the next HTTP read loses the same race. This is a livelock rather
-- than lag: the busier the reserve, the more permanently it is excluded, and
-- no verifier cadence fixes it (production already refreshes every second).
--
-- A confirmed verification is still required, and it must still match
-- reserve_current_states exactly, so the authenticity guarantee is unchanged.
-- What changes is that a verification may trail the floor by a bounded slot
-- margin instead of having to win a race it cannot win. Staleness stays
-- bounded here and again downstream by the planner's economic slot lag gate.

CREATE OR REPLACE FUNCTION kamino.confirmed_verification_slot_tolerance()
RETURNS BIGINT
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    -- ~60s at mainnet slot times. Comfortably above the confirmed-refresh
    -- round trip (--confirmed-refresh-timeout-secs defaults to 20) and an
    -- order of magnitude below the planner's 1500-slot economic lag bound,
    -- so this never becomes the widest staleness window in the pipeline.
    SELECT 150::BIGINT
$$;

CREATE OR REPLACE VIEW kamino.latest_verified_reserve_updates AS
SELECT
    state.event_id,
    state.observed_at,
    state.slot,
    state.source,
    state.source_commitment,
    state.reserve,
    state.market,
    state.market_name,
    state.symbol,
    state.liquidity_mint,
    state.mint_decimals,
    state.market_price_usd,
    state.supply_apy,
    state.borrow_apy,
    state.utilization,
    state.total_supply_usd_estimate,
    state.total_borrow_usd_estimate,
    state.reserve_last_update_stale,
    state.diff_changed,
    state.changed_fields,
    state.diff_summary,
    state.account_data_hash,
    verification.verified_slot,
    verification.verified_at,
    verification.commitment AS verification_commitment,
    verification.verification_source,
    state.reserve_last_update_slot,
    state.reserve_price_status,
    state.available_amount,
    state.borrowed_amount,
    state.total_supply_amount,
    state.market_price_last_updated_ts
FROM kamino.reserve_current_states current_state
JOIN kamino.reserve_confirmed_verifications verification
  ON verification.reserve = current_state.reserve
 AND verification.state_event_id = current_state.state_event_id
 AND verification.account_data_hash = current_state.account_data_hash
 AND verification.verified_slot >= current_state.state_slot
JOIN kamino.reserve_updates state
  ON state.reserve = current_state.reserve
 AND state.event_id = current_state.state_event_id
 AND state.account_data_hash = current_state.account_data_hash
 AND state.slot = current_state.state_slot
 AND state.observed_at = current_state.state_observed_at
 AND state.source = current_state.state_source
 AND state.source_commitment = 'confirmed'
LEFT JOIN kamino.reserve_confirmed_observation_floors observation_floor
  ON observation_floor.reserve = current_state.reserve
WHERE observation_floor.reserve IS NULL
   OR verification.verified_slot > observation_floor.floor_slot
   OR (
        observation_floor.state_valid
    AND observation_floor.account_data_hash = current_state.account_data_hash
   )
   -- Trailing the floor is expected on an active reserve; only unbounded
   -- trailing means the verification has actually gone stale. Strictly
   -- trailing only: at an equal slot the hash branch above is the
   -- overlapping-monitor fence and must keep deciding on its own.
   --
   -- state_valid is required. An invalid floor is written when the stream saw a
   -- wrong-owner or undecodable reserve account, which fences routability
   -- immediately by contract; tolerating it would keep an unroutable reserve
   -- visible for the whole window. Tolerance covers ordinary hash drift on a
   -- valid floor, never a floor that says the account itself is unusable.
   OR (
        observation_floor.state_valid
    AND observation_floor.floor_slot > verification.verified_slot
    AND observation_floor.floor_slot - verification.verified_slot
        <= kamino.confirmed_verification_slot_tolerance()
   );
