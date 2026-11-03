-- Relay open deposit addresses that pay USDC on Solana to a wallet. A wallet
-- gets a new address before its order deadline, so it can own several rows.
CREATE TABLE loyal_yield.relay_deposit_addresses (
    address text PRIMARY KEY CHECK (address ~ '^T[1-9A-HJ-NP-Za-km-z]{33}$'),
    solana_wallet text NOT NULL,
    app_user_id uuid NOT NULL,
    chain text NOT NULL CHECK (chain = 'tron'),
    asset text NOT NULL CHECK (asset = 'usdt'),
    quote_request_id text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX relay_deposit_addresses_wallet_created_at
    ON loyal_yield.relay_deposit_addresses (solana_wallet, chain, asset, created_at DESC);
