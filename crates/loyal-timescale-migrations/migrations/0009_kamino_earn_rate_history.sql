-- Only install the empty read model and synchronous maintenance at deploy time.
-- Historical copying is a separate, explicitly scheduled, resumable operation.
BEGIN;
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS kamino.reserve_earn_rates (
    observed_at TIMESTAMPTZ NOT NULL,
    event_id BIGINT NOT NULL,
    reserve TEXT NOT NULL,
    supply_apy DOUBLE PRECISION NOT NULL,
    reserve_last_update_stale BOOLEAN NOT NULL,
    PRIMARY KEY (observed_at, event_id)
);
SELECT create_hypertable(
    'kamino.reserve_earn_rates', 'observed_at',
    if_not_exists => TRUE, chunk_time_interval => INTERVAL '1 day'
);
CREATE INDEX IF NOT EXISTS reserve_earn_rates_history_idx
    ON kamino.reserve_earn_rates (reserve, observed_at DESC)
    INCLUDE (supply_apy)
    WHERE NOT reserve_last_update_stale AND supply_apy >= 0 AND supply_apy < 0.5;

-- Mirror invalid/stale records too, so corrections retain the source semantics.
-- The reader, not this projection, owns the eligibility rules.
CREATE OR REPLACE FUNCTION kamino.sync_reserve_earn_rate()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM kamino.reserve_earn_rates
        WHERE observed_at = OLD.observed_at AND event_id = OLD.event_id;
        RETURN OLD;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        DELETE FROM kamino.reserve_earn_rates
        WHERE observed_at = OLD.observed_at AND event_id = OLD.event_id
          AND (OLD.observed_at, OLD.event_id) IS DISTINCT FROM (NEW.observed_at, NEW.event_id);
    END IF;
    INSERT INTO kamino.reserve_earn_rates
        (observed_at, event_id, reserve, supply_apy, reserve_last_update_stale)
    VALUES (NEW.observed_at, NEW.event_id, NEW.reserve, NEW.supply_apy, NEW.reserve_last_update_stale)
    ON CONFLICT (observed_at, event_id) DO UPDATE SET
        reserve = EXCLUDED.reserve,
        supply_apy = EXCLUDED.supply_apy,
        reserve_last_update_stale = EXCLUDED.reserve_last_update_stale;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION kamino.sync_reserve_earn_rate() FROM PUBLIC;
DROP TRIGGER IF EXISTS sync_reserve_earn_rate ON kamino.reserve_updates;
CREATE TRIGGER sync_reserve_earn_rate
    AFTER INSERT OR UPDATE OR DELETE ON kamino.reserve_updates
    FOR EACH ROW EXECUTE FUNCTION kamino.sync_reserve_earn_rate();

CREATE TABLE IF NOT EXISTS kamino.reserve_earn_rate_backfills (
    start_at TIMESTAMPTZ NOT NULL,
    end_at TIMESTAMPTZ NOT NULL,
    completed_until TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (start_at, end_at),
    CHECK (start_at < end_at AND completed_until >= start_at AND completed_until <= end_at)
);

-- One one-hour batch and its checkpoint commit together in the caller's
-- transaction. Stop/retry never advances a checkpoint past uncommitted copies.
CREATE OR REPLACE FUNCTION kamino.backfill_reserve_earn_rates_batch(
    history_start TIMESTAMPTZ, history_end TIMESTAMPTZ
) RETURNS TIMESTAMPTZ LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    batch_start TIMESTAMPTZ;
    batch_end TIMESTAMPTZ;
BEGIN
    IF history_start IS NULL OR history_end IS NULL
       OR NOT isfinite(history_start) OR NOT isfinite(history_end)
       OR history_start >= history_end THEN
        RAISE EXCEPTION 'finite, ordered history bounds are required';
    END IF;
    INSERT INTO kamino.reserve_earn_rate_backfills(start_at, end_at, completed_until)
    VALUES (history_start, history_end, history_start)
    ON CONFLICT (start_at, end_at) DO NOTHING;
    SELECT completed_until INTO batch_start
    FROM kamino.reserve_earn_rate_backfills
    WHERE start_at = history_start AND end_at = history_end FOR UPDATE;
    batch_end := least(batch_start + interval '1 hour', history_end);
    IF batch_start = batch_end THEN RETURN batch_end; END IF;

    -- Lock source rows until the copy commits. A concurrent correction/delete
    -- either precedes this snapshot or waits, then its trigger wins. Never
    -- resurrect deleted rows or overwrite a newer live copy with old history.
    WITH source_rows AS MATERIALIZED (
        SELECT observed_at, event_id, reserve, supply_apy, reserve_last_update_stale
        FROM kamino.reserve_updates
        WHERE observed_at >= batch_start AND observed_at < batch_end
        FOR SHARE
    )
    INSERT INTO kamino.reserve_earn_rates
        (observed_at, event_id, reserve, supply_apy, reserve_last_update_stale)
    SELECT observed_at, event_id, reserve, supply_apy, reserve_last_update_stale FROM source_rows
    -- Keep each reserve's small records adjacent even when heap checks are
    -- required; otherwise interleaved monitor writes amplify history reads.
    ORDER BY reserve, observed_at, event_id
    ON CONFLICT (observed_at, event_id) DO NOTHING;

    UPDATE kamino.reserve_earn_rate_backfills
    SET completed_until = batch_end, updated_at = clock_timestamp()
    WHERE start_at = history_start AND end_at = history_end;
    RETURN batch_end;
END;
$$;
REVOKE ALL ON FUNCTION kamino.backfill_reserve_earn_rates_batch(TIMESTAMPTZ, TIMESTAMPTZ) FROM PUBLIC;
COMMIT;
