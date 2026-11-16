-- One organisation allowance, shared by the vault and retail API keys.
CREATE TABLE loyal_yield.jupiter_request_budget (
    scope text PRIMARY KEY CHECK (scope = 'loyal'),
    cooldown_until timestamptz NOT NULL DEFAULT 'epoch',
    request_at timestamptz NOT NULL DEFAULT 'epoch',
    execution_at timestamptz NOT NULL DEFAULT 'epoch',
    valuation_at timestamptz NOT NULL DEFAULT 'epoch',
    background_at timestamptz NOT NULL DEFAULT 'epoch'
);
INSERT INTO loyal_yield.jupiter_request_budget (scope) VALUES ('loyal');
