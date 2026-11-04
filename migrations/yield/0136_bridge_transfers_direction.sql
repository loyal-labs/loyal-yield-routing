ALTER TABLE loyal_yield.bridge_transfers
    DROP CONSTRAINT bridge_transfers_evm_address_check,
    DROP CONSTRAINT bridge_transfers_origin_tx_check,
    DROP CONSTRAINT bridge_transfers_deposit_address_check;

ALTER TABLE loyal_yield.bridge_transfers RENAME COLUMN evm_address TO counterparty;

ALTER TABLE loyal_yield.bridge_transfers
    ADD COLUMN direction text NOT NULL DEFAULT 'in' CHECK (direction IN ('in', 'out')),
    ADD CONSTRAINT bridge_transfers_counterparty_check CHECK (CASE WHEN chain = 'tron'
        THEN counterparty ~ '^T[1-9A-HJ-NP-Za-km-z]{33}$'
        ELSE counterparty ~ '^0x[0-9a-f]{40}$' END);

ALTER TABLE loyal_yield.bridge_transfers
    ADD CONSTRAINT bridge_transfers_origin_tx_check CHECK (CASE
        WHEN direction = 'out' THEN origin_tx ~ '^[1-9A-HJ-NP-Za-km-z]{64,88}$'
        WHEN chain = 'tron' THEN origin_tx ~ '^[0-9a-f]{64}$'
        ELSE origin_tx ~ '^0x[0-9a-f]{64}$' END),
    ADD CONSTRAINT bridge_transfers_deposit_address_check CHECK ((direction = 'in' AND chain = 'tron') = (deposit_address IS NOT NULL));
