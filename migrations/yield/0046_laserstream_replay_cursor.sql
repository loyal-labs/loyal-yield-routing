CREATE TABLE IF NOT EXISTS loyal_yield.laserstream_replay_cursors (
    consumer_name TEXT PRIMARY KEY,
    durable_slot BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
