-- Hourly Loyal Earn fleet allocation samples: raw liquidity per Kamino
-- reserve plus zero-return idle capital, with fleet completeness counts.
-- Written by loyal-app's /api/cron/earn-reserve-share-prices alongside the
-- share prices in 0084; read to weight realized Earn APY by the allocation
-- held at the time. Recorded forward only; there is no backfill.

-- Both runners execute this batch in one implicit transaction. Bound DDL
-- waits; SET LOCAL does not change settings for subsequent worker queries.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS loyal_yield.earn_fleet_allocations_hourly (
    cluster TEXT NOT NULL,
    observed_hour TIMESTAMPTZ NOT NULL,
    -- When the fleet was sampled; the allocation applies from this instant.
    observed_at TIMESTAMPTZ NOT NULL,
    -- {"<reserve>": "<redeemable liquidity raw>"} summed over included vaults.
    reserve_amounts JSONB NOT NULL
        CHECK (jsonb_typeof(reserve_amounts) = 'object'),
    idle_amount_raw NUMERIC(39, 0) NOT NULL CHECK (idle_amount_raw >= 0),
    -- Every active Earn vault is counted in exactly one of the four groups
    -- below. Only included vaults contribute to reserve_amounts and
    -- idle_amount_raw.
    vaults_total INTEGER NOT NULL,
    vaults_included INTEGER NOT NULL,
    -- No complete snapshot exists yet, so the capital is unknown.
    vaults_missing INTEGER NOT NULL,
    -- Unknown amount units or unknown idle balance.
    vaults_invalid INTEGER NOT NULL,
    -- Funded, and the latest complete snapshot is too old to trust.
    vaults_stale INTEGER NOT NULL,
    -- Last-known raw amounts of invalid and stale vaults. Units are
    -- unverified; use only to bound how much capital a sample leaves out.
    excluded_amount_raw NUMERIC(39, 0) NOT NULL
        CHECK (excluded_amount_raw >= 0),
    -- Snapshot clocks of the included funded vaults; NULL when none is funded.
    oldest_source_at TIMESTAMPTZ,
    newest_source_at TIMESTAMPTZ,
    calc_version SMALLINT NOT NULL CHECK (calc_version > 0),
    PRIMARY KEY (cluster, observed_hour),
    CONSTRAINT earn_fleet_allocations_hourly_vault_counts_check CHECK (
        vaults_included >= 0 AND vaults_missing >= 0
        AND vaults_invalid >= 0 AND vaults_stale >= 0
        AND vaults_total =
            vaults_included + vaults_missing + vaults_invalid + vaults_stale
    )
);
