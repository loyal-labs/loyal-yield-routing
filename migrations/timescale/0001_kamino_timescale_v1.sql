CREATE EXTENSION IF NOT EXISTS timescaledb;

CREATE SCHEMA IF NOT EXISTS kamino;

CREATE SEQUENCE IF NOT EXISTS kamino.reserve_update_event_id_seq;

CREATE TABLE IF NOT EXISTS kamino.reserve_updates (
    event_id BIGINT NOT NULL DEFAULT nextval('kamino.reserve_update_event_id_seq'),
    observed_at TIMESTAMPTZ NOT NULL,
    slot BIGINT NOT NULL,
    kind TEXT NOT NULL,
    source TEXT NOT NULL,
    source_commitment TEXT NOT NULL DEFAULT 'confirmed',
    reserve TEXT NOT NULL,
    market TEXT,
    market_name TEXT,
    symbol TEXT,
    liquidity_mint TEXT NOT NULL,
    mint_decimals INTEGER NOT NULL,
    reserve_last_update_slot BIGINT NOT NULL,
    reserve_last_update_stale BOOLEAN NOT NULL,
    reserve_price_status SMALLINT NOT NULL,
    available_amount DOUBLE PRECISION NOT NULL,
    borrowed_amount DOUBLE PRECISION NOT NULL,
    borrowed_amount_sf TEXT NOT NULL,
    total_supply_amount DOUBLE PRECISION NOT NULL,
    market_price_usd DOUBLE PRECISION NOT NULL,
    market_price_last_updated_ts BIGINT NOT NULL,
    cumulative_borrow_rate_bsf TEXT NOT NULL,
    total_supply_usd_estimate DOUBLE PRECISION NOT NULL,
    total_borrow_usd_estimate DOUBLE PRECISION NOT NULL,
    utilization DOUBLE PRECISION NOT NULL,
    borrow_apr DOUBLE PRECISION NOT NULL,
    supply_apr DOUBLE PRECISION NOT NULL,
    borrow_apy DOUBLE PRECISION NOT NULL,
    supply_apy DOUBLE PRECISION NOT NULL,
    protocol_take_rate_pct SMALLINT NOT NULL,
    host_fixed_interest_rate_bps INTEGER NOT NULL,
    diff_changed BOOLEAN NOT NULL,
    changed_fields TEXT[] NOT NULL DEFAULT '{}',
    diff_summary TEXT NOT NULL,
    diff JSONB NOT NULL,
    target JSONB NOT NULL,
    snapshot JSONB NOT NULL,
    record JSONB NOT NULL,
    raw_account_data_base64 TEXT,
    api_supply_apy DOUBLE PRECISION,
    api_borrow_apy DOUBLE PRECISION,
    api_total_supply_usd DOUBLE PRECISION,
    api_total_borrow_usd DOUBLE PRECISION,
    account_data_hash TEXT,
    received_at TIMESTAMPTZ,
    decoded_at TIMESTAMPTZ,
    inserted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    receive_to_decode_ms BIGINT,
    decode_to_insert_ms BIGINT
);

SELECT create_hypertable(
    'kamino.reserve_updates',
    'observed_at',
    if_not_exists => TRUE,
    chunk_time_interval => INTERVAL '1 day'
);

CREATE INDEX IF NOT EXISTS reserve_updates_time_desc_idx
    ON kamino.reserve_updates (observed_at DESC);
CREATE INDEX IF NOT EXISTS reserve_updates_event_id_idx
    ON kamino.reserve_updates (event_id);
CREATE INDEX IF NOT EXISTS reserve_updates_reserve_event_id_idx
    ON kamino.reserve_updates (reserve, event_id DESC);
CREATE INDEX IF NOT EXISTS reserve_updates_commitment_event_id_idx
    ON kamino.reserve_updates (source_commitment, event_id DESC);
CREATE INDEX IF NOT EXISTS reserve_updates_reserve_time_idx
    ON kamino.reserve_updates (reserve, observed_at DESC);
CREATE INDEX IF NOT EXISTS reserve_updates_symbol_time_idx
    ON kamino.reserve_updates (symbol, observed_at DESC);
CREATE INDEX IF NOT EXISTS reserve_updates_market_time_idx
    ON kamino.reserve_updates (market, observed_at DESC);
CREATE INDEX IF NOT EXISTS reserve_updates_slot_desc_idx
    ON kamino.reserve_updates (slot DESC);
CREATE INDEX IF NOT EXISTS reserve_updates_supply_apy_time_idx
    ON kamino.reserve_updates (supply_apy DESC, observed_at DESC);
CREATE INDEX IF NOT EXISTS reserve_updates_changed_fields_gin_idx
    ON kamino.reserve_updates USING GIN (changed_fields);
CREATE INDEX IF NOT EXISTS reserve_updates_diff_jsonb_gin_idx
    ON kamino.reserve_updates USING GIN (diff jsonb_path_ops);

CREATE TABLE IF NOT EXISTS kamino.reserve_update_dedupe (
    dedupe_key TEXT PRIMARY KEY,
    event_id BIGINT NOT NULL,
    reserve TEXT NOT NULL,
    slot BIGINT NOT NULL,
    account_data_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS reserve_update_dedupe_event_id_idx
    ON kamino.reserve_update_dedupe (event_id);
CREATE INDEX IF NOT EXISTS reserve_update_dedupe_reserve_slot_idx
    ON kamino.reserve_update_dedupe (reserve, slot);

CREATE TABLE IF NOT EXISTS kamino.supported_reserves (
    market TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    reserve TEXT NOT NULL,
    market_name TEXT,
    symbol TEXT,
    risk_baskets TEXT[] NOT NULL DEFAULT '{}',
    source TEXT NOT NULL DEFAULT 'kamino-api',
    active BOOLEAN NOT NULL DEFAULT TRUE,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (market, liquidity_mint)
);

CREATE INDEX IF NOT EXISTS supported_reserves_active_reserve_idx
    ON kamino.supported_reserves (active, reserve);
CREATE INDEX IF NOT EXISTS supported_reserves_active_symbol_idx
    ON kamino.supported_reserves (active, symbol);

CREATE OR REPLACE FUNCTION kamino.notify_reserve_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
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

DROP TRIGGER IF EXISTS notify_reserve_update ON kamino.reserve_updates;
CREATE TRIGGER notify_reserve_update
AFTER INSERT ON kamino.reserve_updates
FOR EACH ROW
EXECUTE FUNCTION kamino.notify_reserve_update();

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
    record
FROM kamino.reserve_updates
ORDER BY reserve, event_id DESC;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'kamino'
          AND c.relname = 'reserve_updates_1m'
    ) THEN
        BEGIN
            EXECUTE $cagg$
                CREATE MATERIALIZED VIEW kamino.reserve_updates_1m
                WITH (timescaledb.continuous) AS
                SELECT
                    time_bucket('1 minute', observed_at) AS bucket,
                    reserve,
                    market,
                    symbol,
                    count(*) AS update_count,
                    avg(supply_apy) AS avg_supply_apy,
                    min(supply_apy) AS min_supply_apy,
                    max(supply_apy) AS max_supply_apy,
                    avg(borrow_apy) AS avg_borrow_apy,
                    avg(utilization) AS avg_utilization,
                    avg(total_supply_usd_estimate) AS avg_supply_usd,
                    avg(total_borrow_usd_estimate) AS avg_borrow_usd,
                    max(slot) AS max_slot,
                    max(observed_at) AS last_observed_at
                FROM kamino.reserve_updates
                GROUP BY bucket, reserve, market, symbol
                WITH NO DATA
            $cagg$;
        EXCEPTION
            WHEN others THEN
                RAISE NOTICE 'continuous aggregate was not created, using a compatibility view: %', SQLERRM;
                EXECUTE $view$
                    CREATE VIEW kamino.reserve_updates_1m AS
                    SELECT
                        time_bucket('1 minute', observed_at) AS bucket,
                        reserve,
                        market,
                        symbol,
                        count(*) AS update_count,
                        avg(supply_apy) AS avg_supply_apy,
                        min(supply_apy) AS min_supply_apy,
                        max(supply_apy) AS max_supply_apy,
                        avg(borrow_apy) AS avg_borrow_apy,
                        avg(utilization) AS avg_utilization,
                        avg(total_supply_usd_estimate) AS avg_supply_usd,
                        avg(total_borrow_usd_estimate) AS avg_borrow_usd,
                        max(slot) AS max_slot,
                        max(observed_at) AS last_observed_at
                    FROM kamino.reserve_updates
                    GROUP BY bucket, reserve, market, symbol
                $view$;
        END;
    END IF;
END $$;

DO $$
BEGIN
    PERFORM add_continuous_aggregate_policy(
        'kamino.reserve_updates_1m',
        start_offset => INTERVAL '2 days',
        end_offset => INTERVAL '1 minute',
        schedule_interval => INTERVAL '1 minute'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
    WHEN undefined_function THEN
        RAISE NOTICE 'continuous aggregate policy was not added: %', SQLERRM;
    WHEN invalid_parameter_value THEN
        RAISE NOTICE 'continuous aggregate policy was not added: %', SQLERRM;
    WHEN others THEN
        RAISE NOTICE 'continuous aggregate policy was not added: %', SQLERRM;
END $$;

DO $$
BEGIN
    ALTER TABLE kamino.reserve_updates SET (
        timescaledb.compress,
        timescaledb.compress_segmentby = 'reserve,symbol',
        timescaledb.compress_orderby = 'observed_at DESC'
    );
EXCEPTION
    WHEN others THEN
        RAISE NOTICE 'compression settings were not applied: %', SQLERRM;
END $$;

DO $$
BEGIN
    PERFORM add_compression_policy(
        'kamino.reserve_updates',
        compress_after => INTERVAL '7 days'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
    WHEN others THEN
        RAISE NOTICE 'compression policy was not added: %', SQLERRM;
END $$;
