-- Expose the reserve inputs needed to evaluate RWA loop capacity, leverage,
-- and post-entry borrow cost. The event table already retains the complete
-- decoded snapshot as JSONB, so this migration adds no duplicate storage.

CREATE OR REPLACE VIEW kamino.latest_reserve_updates AS
SELECT DISTINCT ON (reserve)
    event_id,
    observed_at,
    slot,
    source,
    source_commitment,
    reserve,
    market,
    market_name,
    symbol,
    liquidity_mint,
    supply_apy,
    borrow_apy,
    utilization,
    total_supply_usd_estimate,
    total_borrow_usd_estimate,
    reserve_last_update_stale,
    diff_changed,
    changed_fields,
    diff_summary,
    record,
    NULLIF(snapshot ->> 'reserve_status', '')::SMALLINT AS reserve_status,
    NULLIF(snapshot ->> 'emergency_mode', '')::BOOLEAN AS emergency_mode,
    NULLIF(snapshot ->> 'loan_to_value_pct', '')::SMALLINT AS loan_to_value_pct,
    NULLIF(snapshot ->> 'liquidation_threshold_pct', '')::SMALLINT
        AS liquidation_threshold_pct,
    NULLIF(snapshot ->> 'borrow_factor_pct', '')::NUMERIC(20, 0) AS borrow_factor_pct,
    NULLIF(snapshot ->> 'deposit_limit', '')::NUMERIC(20, 0) AS deposit_limit,
    NULLIF(snapshot ->> 'borrow_limit', '')::NUMERIC(20, 0) AS borrow_limit,
    NULLIF(snapshot ->> 'utilization_limit_block_borrowing_above_pct', '')::SMALLINT
        AS utilization_limit_block_borrowing_above_pct,
    NULLIF(snapshot ->> 'disable_usage_as_coll_outside_emode', '')::BOOLEAN
        AS disable_usage_as_coll_outside_emode,
    NULLIF(snapshot ->> 'borrow_limit_outside_elevation_group', '')::NUMERIC(20, 0)
        AS borrow_limit_outside_elevation_group,
    NULLIF(snapshot ->> 'borrowed_amount_outside_elevation_group', '')::NUMERIC(20, 0)
        AS borrowed_amount_outside_elevation_group,
    NULLIF(snapshot ->> 'origination_fee_sf', '')::NUMERIC(20, 0) AS origination_fee_sf,
    NULLIF(snapshot ->> 'flash_loan_fee_sf', '')::NUMERIC(20, 0) AS flash_loan_fee_sf,
    snapshot -> 'borrow_rate_curve' AS borrow_rate_curve,
    snapshot -> 'deposit_withdrawal_cap' AS deposit_withdrawal_cap,
    snapshot -> 'debt_withdrawal_cap' AS debt_withdrawal_cap
FROM kamino.reserve_updates
ORDER BY reserve, event_id DESC;

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
    state.market_price_last_updated_ts,
    NULLIF(state.snapshot ->> 'reserve_status', '')::SMALLINT AS reserve_status,
    NULLIF(state.snapshot ->> 'emergency_mode', '')::BOOLEAN AS emergency_mode,
    NULLIF(state.snapshot ->> 'loan_to_value_pct', '')::SMALLINT AS loan_to_value_pct,
    NULLIF(state.snapshot ->> 'liquidation_threshold_pct', '')::SMALLINT
        AS liquidation_threshold_pct,
    NULLIF(state.snapshot ->> 'borrow_factor_pct', '')::NUMERIC(20, 0)
        AS borrow_factor_pct,
    NULLIF(state.snapshot ->> 'deposit_limit', '')::NUMERIC(20, 0) AS deposit_limit,
    NULLIF(state.snapshot ->> 'borrow_limit', '')::NUMERIC(20, 0) AS borrow_limit,
    NULLIF(state.snapshot ->> 'utilization_limit_block_borrowing_above_pct', '')::SMALLINT
        AS utilization_limit_block_borrowing_above_pct,
    NULLIF(state.snapshot ->> 'disable_usage_as_coll_outside_emode', '')::BOOLEAN
        AS disable_usage_as_coll_outside_emode,
    NULLIF(state.snapshot ->> 'borrow_limit_outside_elevation_group', '')::NUMERIC(20, 0)
        AS borrow_limit_outside_elevation_group,
    NULLIF(state.snapshot ->> 'borrowed_amount_outside_elevation_group', '')::NUMERIC(20, 0)
        AS borrowed_amount_outside_elevation_group,
    NULLIF(state.snapshot ->> 'origination_fee_sf', '')::NUMERIC(20, 0)
        AS origination_fee_sf,
    NULLIF(state.snapshot ->> 'flash_loan_fee_sf', '')::NUMERIC(20, 0) AS flash_loan_fee_sf,
    state.snapshot -> 'borrow_rate_curve' AS borrow_rate_curve,
    state.snapshot -> 'deposit_withdrawal_cap' AS deposit_withdrawal_cap,
    state.snapshot -> 'debt_withdrawal_cap' AS debt_withdrawal_cap
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
   OR (
        observation_floor.state_valid
    AND observation_floor.floor_slot > verification.verified_slot
    AND observation_floor.floor_slot - verification.verified_slot
        <= kamino.confirmed_verification_slot_tolerance()
   );
