-- Hourly Kamino reserve share prices (liquidity per collateral token) for
-- the reserves Loyal Earn uses. Written by loyal-app's
-- /api/cron/earn-reserve-share-prices; read to compute realized Earn APY.

CREATE TABLE IF NOT EXISTS loyal_yield.earn_reserve_share_prices (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    reserve TEXT NOT NULL,
    market TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    observed_hour TIMESTAMPTZ NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    slot BIGINT NOT NULL,
    share_price DOUBLE PRECISION NOT NULL CHECK (share_price > 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS earn_reserve_share_prices_hour_uidx
    ON loyal_yield.earn_reserve_share_prices (cluster, reserve, observed_hour);

CREATE INDEX IF NOT EXISTS earn_reserve_share_prices_observed_idx
    ON loyal_yield.earn_reserve_share_prices (cluster, observed_at);
