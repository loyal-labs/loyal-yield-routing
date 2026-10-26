-- A route's current net APY, one row per Pacific day: the last value of the day
-- the selector sampler stored as state->'currentApy' (backyard current_apy.go),
-- written in the same statement. Earn MAX charts these rows as its APY history.
CREATE TABLE IF NOT EXISTS loyal_yield.multiply_route_current_apy_days (
    route_key text NOT NULL,
    day date NOT NULL,
    apy_bps bigint NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (route_key, day)
);
