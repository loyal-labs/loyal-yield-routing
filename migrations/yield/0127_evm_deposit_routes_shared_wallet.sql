-- A Privy user's embedded EVM wallet serves every external Solana wallet they link;
-- each Relay bridge sends to the signed-in wallet. A Privy deposit account still
-- belongs to exactly one Solana wallet.
ALTER TABLE loyal_yield.evm_deposit_routes DROP CONSTRAINT evm_deposit_routes_evm_address_key;
CREATE UNIQUE INDEX evm_deposit_routes_autobridge_address
    ON loyal_yield.evm_deposit_routes (evm_address) WHERE kind = 'privy_autobridge';
