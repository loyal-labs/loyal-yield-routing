-- The balance-sweep ATA monitor created this table at runtime
-- (ensure_earn_apy_hourly_snapshot_schema). The Go observer only writes it, so
-- the table is now owned by the registered migrations. IF NOT EXISTS keeps this
-- a no-op on databases where the Rust monitor already created it.
CREATE TABLE IF NOT EXISTS loyal_yield.earn_apy_hourly_snapshots (
    id BIGSERIAL PRIMARY KEY,
    strategy TEXT NOT NULL,
    risk_profile TEXT NOT NULL,
    fee_bps SMALLINT NOT NULL,
    sample_hour TIMESTAMPTZ NOT NULL,
    window_started_at TIMESTAMPTZ NOT NULL,
    window_ended_at TIMESTAMPTZ NOT NULL,
    generated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    loyal_apy_bps INTEGER NOT NULL,
    main_usdc_reserve_apy_bps INTEGER NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE UNIQUE INDEX IF NOT EXISTS earn_apy_hourly_snapshots_key_uidx
    ON loyal_yield.earn_apy_hourly_snapshots (strategy, risk_profile, fee_bps, sample_hour);

CREATE INDEX IF NOT EXISTS earn_apy_hourly_snapshots_latest_idx
    ON loyal_yield.earn_apy_hourly_snapshots (strategy, risk_profile, fee_bps, sample_hour DESC);
