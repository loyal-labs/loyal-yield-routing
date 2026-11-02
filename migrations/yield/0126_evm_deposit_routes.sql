-- The 0x address each Solana wallet receives EVM deposits at. privy_autobridge is a
-- Privy deposit account that bridges every arrival by itself; relay_signed is the
-- user's Privy embedded EVM wallet, bridged by a Relay transaction the user signs.
CREATE TABLE loyal_yield.evm_deposit_routes (
    solana_wallet text PRIMARY KEY,
    app_user_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('privy_autobridge', 'relay_signed')),
    evm_address text NOT NULL UNIQUE CHECK (evm_address ~ '^0x[0-9a-f]{40}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
