-- Finalized evidence only. Confirmed transfers are replayed in memory until finality.
CREATE TABLE loyal_yield.backyard_foreign_inflow_cursors (
    route_key text NOT NULL,
    token_account text NOT NULL,
    signature text NOT NULL,
    slot bigint NOT NULL CHECK (slot > 0),
    balance_raw bigint NOT NULL CHECK (balance_raw >= 0),
    foreign_raw bigint NOT NULL CHECK (foreign_raw >= 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (route_key, token_account)
);

CREATE TABLE loyal_yield.backyard_foreign_inflows (
    route_key text NOT NULL,
    token_account text NOT NULL,
    signature text NOT NULL,
    slot bigint NOT NULL CHECK (slot > 0),
    foreign_before_raw bigint NOT NULL CHECK (foreign_before_raw >= 0),
    foreign_after_raw bigint NOT NULL CHECK (foreign_after_raw >= 0),
    evidence jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (route_key, token_account, signature)
);
