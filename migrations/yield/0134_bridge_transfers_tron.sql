ALTER TABLE loyal_yield.bridge_transfers
    DROP CONSTRAINT bridge_transfers_evm_address_check,
    DROP CONSTRAINT bridge_transfers_origin_tx_check,
    ADD COLUMN deposit_address text REFERENCES loyal_yield.relay_deposit_addresses (address),
    ADD CONSTRAINT bridge_transfers_evm_address_check CHECK (CASE WHEN chain = 'tron'
        THEN evm_address ~ '^T[1-9A-HJ-NP-Za-km-z]{33}$'
        ELSE evm_address ~ '^0x[0-9a-f]{40}$' END),
    ADD CONSTRAINT bridge_transfers_origin_tx_check CHECK (CASE WHEN chain = 'tron'
        THEN origin_tx ~ '^[0-9a-f]{64}$'
        ELSE origin_tx ~ '^0x[0-9a-f]{64}$' END),
    ADD CONSTRAINT bridge_transfers_deposit_address_check CHECK ((chain = 'tron') = (deposit_address IS NOT NULL));
