-- Historical rows are queryable observations, not live monitor events. Keep
-- them out of the live notification channel and choose latest rows by event
-- time so a late import cannot replace a newer observation.

CREATE OR REPLACE FUNCTION kamino.notify_reserve_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.source IN ('substreams_backfill', 'kamino_api_history') THEN
        RETURN NEW;
    END IF;

    PERFORM pg_notify(
        'kamino_reserve_updates',
        json_build_object(
            'event_id', NEW.event_id,
            'observed_at', NEW.observed_at,
            'slot', NEW.slot,
            'reserve', NEW.reserve,
            'market', NEW.market,
            'symbol', NEW.symbol,
            'source', NEW.source,
            'source_commitment', NEW.source_commitment,
            'supply_apy', NEW.supply_apy,
            'borrow_apy', NEW.borrow_apy,
            'utilization', NEW.utilization,
            'diff_changed', NEW.diff_changed
        )::text
    );
    RETURN NEW;
END;
$$;

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
ORDER BY reserve, observed_at DESC, event_id DESC;
