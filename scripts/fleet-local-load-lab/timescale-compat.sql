\set ON_ERROR_STOP on

-- The planner needs only the catalog and verified-reserve view for an empty
-- local market. This deliberately installs no Timescale extension and reads
-- no external data.
CREATE SCHEMA IF NOT EXISTS kamino;

CREATE TABLE IF NOT EXISTS kamino.supported_reserves (
    market TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    reserve TEXT NOT NULL,
    market_name TEXT,
    symbol TEXT,
    risk_baskets TEXT[] NOT NULL DEFAULT '{}',
    source TEXT NOT NULL DEFAULT 'local-e2e',
    active BOOLEAN NOT NULL DEFAULT TRUE,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (market, liquidity_mint)
);

ALTER TABLE kamino.supported_reserves
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE OR REPLACE VIEW kamino.latest_verified_reserve_updates AS
SELECT
    NULL::BIGINT AS event_id,
    NULL::TEXT AS account_data_hash,
    NULL::TIMESTAMPTZ AS observed_at,
    NULL::BIGINT AS slot,
    NULL::TIMESTAMPTZ AS verified_at,
    NULL::BIGINT AS verified_slot,
    NULL::TEXT AS verification_commitment,
    NULL::TEXT AS verification_source,
    NULL::TEXT AS reserve,
    NULL::TEXT AS market,
    NULL::TEXT AS market_name,
    NULL::TEXT AS liquidity_mint,
    NULL::TEXT AS symbol,
    NULL::INTEGER AS mint_decimals,
    NULL::BIGINT AS reserve_last_update_slot,
    NULL::BOOLEAN AS reserve_last_update_stale,
    NULL::SMALLINT AS reserve_price_status,
    NULL::DOUBLE PRECISION AS available_amount,
    NULL::DOUBLE PRECISION AS borrowed_amount,
    NULL::DOUBLE PRECISION AS total_supply_amount,
    NULL::DOUBLE PRECISION AS market_price_usd,
    NULL::BIGINT AS market_price_last_updated_ts,
    NULL::DOUBLE PRECISION AS utilization,
    NULL::DOUBLE PRECISION AS borrow_apy,
    NULL::DOUBLE PRECISION AS supply_apy,
    NULL::DOUBLE PRECISION AS total_supply_usd_estimate,
    NULL::DOUBLE PRECISION AS total_borrow_usd_estimate,
    NULL::BOOLEAN AS diff_changed,
    NULL::TEXT[] AS changed_fields,
    NULL::TEXT AS diff_summary
WHERE FALSE;
