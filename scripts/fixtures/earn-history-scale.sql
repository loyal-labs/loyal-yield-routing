-- Local-only synthetic scale fixture. No wallet records or production data.
\set ON_ERROR_STOP on
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE SCHEMA kamino;
CREATE TABLE kamino.reserve_updates (
  event_id bigint NOT NULL,
  reserve text NOT NULL,
  observed_at timestamptz NOT NULL,
  supply_apy double precision NOT NULL,
  reserve_last_update_stale boolean NOT NULL,
  padding text NOT NULL,
  payload text NOT NULL DEFAULT repeat('y', 4000)
);
-- Preserve realistic physical row width instead of compressing repeated padding.
ALTER TABLE kamino.reserve_updates ALTER COLUMN padding SET STORAGE PLAIN;
ALTER TABLE kamino.reserve_updates ALTER COLUMN payload SET STORAGE EXTERNAL;
SELECT create_hypertable('kamino.reserve_updates', 'observed_at', chunk_time_interval => INTERVAL '1 day');
CREATE INDEX reserve_updates_reserve_time_idx ON kamino.reserve_updates (reserve, observed_at DESC);
CREATE TABLE kamino.reserve_apy_backfill (
  reserve text NOT NULL, observed_at timestamptz NOT NULL, supply_apy double precision NOT NULL
);
CREATE INDEX reserve_apy_backfill_reserve_time_idx ON kamino.reserve_apy_backfill (reserve, observed_at DESC);
-- 140k observations/day, two held reserves plus two unrelated reserves.
-- Each held reserve has 35k observations/day, with an intraday rate change.
CREATE PROCEDURE public.seed_earn_history(days integer) LANGUAGE plpgsql AS $$
BEGIN
  FOR day IN 0..days-1 LOOP
    INSERT INTO kamino.reserve_updates (event_id, reserve, observed_at, supply_apy, reserve_last_update_stale, padding)
    SELECT day::bigint * 140000 + n + 1, repeat(chr(65 + n % 4), 44),
      '2026-07-08T00:00Z'::timestamptz + day * interval '1 day' + (n / 4) * interval '2.468571 seconds',
      CASE WHEN n / 4 < 17500 THEN 0.03 ELSE 0.09 END,
      false, repeat('x', 1400)
    FROM generate_series(0, 139999) n;
    COMMIT;
    RAISE NOTICE 'seeded day %', day + 1;
  END LOOP;
END $$;
