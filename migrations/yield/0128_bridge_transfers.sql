-- One Relay bridge from a relay_signed EVM wallet to its Solana wallet, keyed by
-- Relay's request id. loyal-web writes quoted and submitted; the status job
-- moves submitted and failed rows to filled or refunded.
CREATE TABLE loyal_yield.bridge_transfers (
    request_id text PRIMARY KEY,
    solana_wallet text NOT NULL,
    evm_address text NOT NULL CHECK (evm_address ~ '^0x[0-9a-f]{40}$'),
    chain text NOT NULL,
    asset text NOT NULL,
    amount_in_raw numeric NOT NULL CHECK (amount_in_raw > 0),
    amount_out_raw numeric NOT NULL CHECK (amount_out_raw > 0),
    state text NOT NULL DEFAULT 'quoted' CHECK (state IN ('quoted', 'submitted', 'filled', 'refunded', 'failed')),
    origin_tx text CHECK (origin_tx ~ '^0x[0-9a-f]{64}$'),
    destination_tx text,
    quoted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    submitted_at timestamptz,
    settled_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX bridge_transfers_wallet_quoted_at ON loyal_yield.bridge_transfers (solana_wallet, quoted_at DESC);
