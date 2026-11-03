ALTER TABLE loyal_yield.bridge_transfers RENAME COLUMN evm_address TO origin_address;

ALTER TABLE loyal_yield.bridge_transfers
    DROP CONSTRAINT bridge_transfers_evm_address_check,
    DROP CONSTRAINT bridge_transfers_origin_tx_check,
    DROP CONSTRAINT bridge_transfers_amount_out_raw_check,
    ADD COLUMN deposit_address text REFERENCES loyal_yield.relay_deposit_addresses (address),
    ADD CONSTRAINT bridge_transfers_origin_address_check CHECK (CASE WHEN chain = 'tron'
        THEN origin_address ~ '^T[1-9A-HJ-NP-Za-km-z]{33}$'
        ELSE origin_address ~ '^0x[0-9a-f]{40}$' END),
    ADD CONSTRAINT bridge_transfers_origin_tx_check CHECK (CASE WHEN chain = 'tron'
        THEN origin_tx ~ '^[0-9a-f]{64}$'
        ELSE origin_tx ~ '^0x[0-9a-f]{64}$' END),
    ADD CONSTRAINT bridge_transfers_amount_out_raw_check CHECK (amount_out_raw >= 0),
    ADD CONSTRAINT bridge_transfers_deposit_address_check CHECK ((chain = 'tron') = (deposit_address IS NOT NULL));
