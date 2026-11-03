CREATE TABLE loyal_yield.earn_max_cash_flows (
    signature text NOT NULL,
    wallet text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('deposit', 'claim')),
    amount_raw bigint NOT NULL CHECK (amount_raw >= 0),
    slot bigint NOT NULL CHECK (slot > 0),
    transaction_index bigint NOT NULL CHECK (transaction_index >= 0),
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (signature, wallet, kind)
);

CREATE INDEX earn_max_cash_flows_wallet ON loyal_yield.earn_max_cash_flows (wallet, slot, transaction_index);
