SET LOCAL lock_timeout = '5s';

INSERT INTO loyal_yield.laserstream_replay_cursors AS cursor (consumer_name, durable_slot)
SELECT 'earn-smart-account:mainnet-beta' || substr(consumer_name, length('earn-smart-account:mainnet') + 1), durable_slot
FROM loyal_yield.laserstream_replay_cursors
WHERE consumer_name IN ('earn-smart-account:mainnet', 'earn-smart-account:mainnet:watch-observation')
ON CONFLICT (consumer_name) DO UPDATE
SET durable_slot = GREATEST(cursor.durable_slot, EXCLUDED.durable_slot), updated_at = now();

DELETE FROM loyal_yield.laserstream_replay_cursors
WHERE consumer_name IN ('earn-smart-account:mainnet', 'earn-smart-account:mainnet:watch-observation');

UPDATE loyal_yield.earn_reconciliation_jobs
SET consumer_name = 'earn-smart-account:mainnet-beta' || substr(consumer_name, length('earn-smart-account:mainnet') + 1)
WHERE completed_at IS NULL
  AND consumer_name IN ('earn-smart-account:mainnet', 'earn-smart-account:mainnet:watch-observation');
