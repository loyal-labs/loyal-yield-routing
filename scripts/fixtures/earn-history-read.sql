-- Full 57-day holding history, with two reserve switches and two cash flows.
-- The application also performs a carry-in lookup for non-midnight boundaries;
-- this fixture starts at midnight, where every reserve has an observation.
WITH held_days AS (
  SELECT '2026-07-09T00:00Z'::timestamptz + d * interval '1 day' AS start_at,
    '2026-07-10T00:00Z'::timestamptz + d * interval '1 day' AS end_at,
    repeat(CASE WHEN d >= 23 AND d < 44 THEN 'B' ELSE 'A' END, 44) AS reserve,
    CASE WHEN d >= 40 THEN 125 WHEN d >= 20 THEN 150 ELSE 100 END AS principal
  FROM generate_series(0, 56) d
), integrated AS (
  SELECT d.start_at, d.principal,
    sum(s.supply_apy * extract(epoch FROM s.next_at - s.observed_at)) AS rate_seconds
  FROM held_days d CROSS JOIN LATERAL (
    SELECT observed_at, supply_apy,
      lead(observed_at, 1, d.end_at) OVER (ORDER BY observed_at) AS next_at
    FROM kamino.reserve_updates
    WHERE reserve = d.reserve AND observed_at >= d.start_at AND observed_at < d.end_at
      AND NOT reserve_last_update_stale AND supply_apy >= 0 AND supply_apy < 0.5
  ) s GROUP BY d.start_at, d.principal
)
SELECT sum(principal * rate_seconds / (365 * 86400)) AS lifetime_earned_usd,
  count(*) AS days FROM integrated;
