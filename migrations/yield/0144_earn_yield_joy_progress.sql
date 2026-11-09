CREATE TABLE loyal_yield.earn_yield_joy_progress (
    cluster text PRIMARY KEY,
    cycle_started_at timestamptz NOT NULL,
    last_wallet text NOT NULL DEFAULT '',
    completed boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now()
);
