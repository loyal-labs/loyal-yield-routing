CREATE TABLE IF NOT EXISTS kamino.reserve_current_states (
    reserve TEXT PRIMARY KEY,
    state_event_id BIGINT NOT NULL CHECK (state_event_id > 0),
    account_data_hash TEXT NOT NULL,
    state_slot BIGINT NOT NULL CHECK (state_slot >= 0),
    state_observed_at TIMESTAMPTZ NOT NULL,
    state_source TEXT NOT NULL CHECK (
        state_source IN ('http_snapshot', 'http_confirmed_refresh')
    ),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE SEQUENCE IF NOT EXISTS kamino.reserve_confirmed_observation_id_seq AS BIGINT;

CREATE TABLE IF NOT EXISTS kamino.reserve_confirmed_observation_floors (
    reserve TEXT PRIMARY KEY,
    floor_slot BIGINT NOT NULL CHECK (floor_slot >= 0),
    observation_id BIGINT NOT NULL DEFAULT nextval(
        'kamino.reserve_confirmed_observation_id_seq'::regclass
    ) CHECK (observation_id > 0),
    account_data_hash TEXT,
    state_valid BOOLEAN NOT NULL,
    source TEXT NOT NULL CHECK (
        source IN (
            'http_snapshot', 'http_confirmed_refresh',
            'laserstream_grpc', 'websocket'
        )
    ),
    source_rank SMALLINT NOT NULL CHECK (source_rank IN (1, 2)),
    observed_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (state_valid AND account_data_hash IS NOT NULL)
        OR (NOT state_valid AND account_data_hash IS NULL)
    ),
    CHECK (
        (source_rank = 1 AND source IN ('laserstream_grpc', 'websocket'))
        OR (
            source_rank = 2
            AND source IN ('http_snapshot', 'http_confirmed_refresh')
        )
    )
);

CREATE TABLE IF NOT EXISTS kamino.reserve_confirmed_verifications (
    reserve TEXT PRIMARY KEY,
    state_event_id BIGINT NOT NULL,
    account_data_hash TEXT NOT NULL,
    verified_slot BIGINT NOT NULL CHECK (verified_slot >= 0),
    verified_at TIMESTAMPTZ NOT NULL,
    commitment TEXT NOT NULL CHECK (commitment = 'confirmed'),
    verification_source TEXT NOT NULL CHECK (
        verification_source IN ('http_snapshot', 'http_confirmed_refresh')
    ),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

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
   );
