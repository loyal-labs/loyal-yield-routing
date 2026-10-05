-- Operator-only, explicit bounded history range; use psql without --single-transaction.
-- psql "$TIMESCALEDB_URL" -X -v ON_ERROR_STOP=1 \
--   -v history_start=2026-07-08T00:00:00Z -v history_end=2026-09-04T00:00:00Z \
--   -f scripts/backfill-earn-rate-history.sql
\set ON_ERROR_STOP on
SET application_name = 'earn-rate-history-backfill';
SET lock_timeout = '2s';
SET statement_timeout = '10s';
-- The first call validates/registers the window and copies one batch.
SELECT kamino.backfill_reserve_earn_rates_batch(:'history_start', :'history_end');
-- Each generated call runs in its own transaction. Resume with identical bounds.
SELECT format(
    'SELECT kamino.backfill_reserve_earn_rates_batch(%L, %L);',
    :'history_start', :'history_end'
)
FROM generate_series(1, (
    SELECT ceil(extract(epoch FROM (end_at - completed_until)) / 3600)::integer
    FROM kamino.reserve_earn_rate_backfills
    WHERE start_at = :'history_start'::timestamptz AND end_at = :'history_end'::timestamptz
))
\gexec
SELECT start_at, end_at, completed_until, completed_until = end_at AS completed
FROM kamino.reserve_earn_rate_backfills
WHERE start_at = :'history_start'::timestamptz AND end_at = :'history_end'::timestamptz;
