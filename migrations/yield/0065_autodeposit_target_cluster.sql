-- Policy-monitor projections created after setup confirmation moved to the
-- client were missing the environment identity. Repair existing rows from the
-- singleton realtime environment; new projections now persist cluster directly.
UPDATE loyal_yield.balance_sweep_targets AS target
SET cluster = config.solana_env
FROM loyal_yield.realtime_configuration AS config
WHERE config.singleton
  AND target.cluster IS NULL;
