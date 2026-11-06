ALTER TABLE loyal_yield.backyard_withdrawal_cost_reservations
    ADD COLUMN participant_id TEXT,
    ADD COLUMN owner_class TEXT NOT NULL DEFAULT 'squads' CHECK (owner_class IN ('squads','vault_admin','unsupported')),
    ADD COLUMN receipt TEXT,
    ADD COLUMN wait_reason TEXT;

UPDATE loyal_yield.backyard_withdrawal_cost_reservations SET participant_id = consent_id;

ALTER TABLE loyal_yield.backyard_withdrawal_cost_reservations
    ALTER COLUMN participant_id SET NOT NULL,
    ALTER COLUMN owner_class DROP DEFAULT,
    ADD CONSTRAINT backyard_withdrawal_cost_participant_id CHECK (participant_id ~ '^[0-9a-f]{64}$'),
    DROP CONSTRAINT backyard_withdrawal_cost_reservations_pkey,
    ADD PRIMARY KEY (operation_id, participant_id),
    ALTER COLUMN consent_id DROP NOT NULL,
    ALTER COLUMN request_signature DROP NOT NULL,
    ALTER COLUMN request_instruction DROP NOT NULL,
    ADD CONSTRAINT backyard_withdrawal_cost_consented CHECK ((consent_id IS NOT NULL) = (owner_class = 'squads' AND wait_reason IS NULL)),
    ADD CONSTRAINT backyard_withdrawal_cost_consent_generation CHECK (
        (consent_id IS NOT NULL AND participant_id = consent_id AND request_signature IS NOT NULL AND request_instruction IS NOT NULL)
        OR (consent_id IS NULL AND request_signature IS NULL AND request_instruction IS NULL AND receipt IS NOT NULL)),
    ADD CONSTRAINT backyard_withdrawal_cost_unsupported_waits CHECK (owner_class <> 'unsupported' OR wait_reason IS NOT NULL),
    ADD CONSTRAINT backyard_withdrawal_cost_admin_admitted CHECK (owner_class <> 'vault_admin' OR wait_reason IS NULL);

ALTER TABLE loyal_yield.backyard_withdrawal_cost_operations
    ADD COLUMN actual_excess_cost_usdc BIGINT CHECK (actual_excess_cost_usdc >= 0);

UPDATE loyal_yield.backyard_withdrawal_cost_operations SET actual_excess_cost_usdc = 0 WHERE state = 'settled';

ALTER TABLE loyal_yield.backyard_withdrawal_cost_operations
    DROP COLUMN receipt_instruction,
    ADD CONSTRAINT backyard_withdrawal_cost_settled CHECK (state <> 'settled' OR (receipt_signature IS NOT NULL AND finalized_slot IS NOT NULL AND actual_cost_usdc IS NOT NULL AND actual_other_holders_usdc IS NOT NULL AND actual_incremental_cost_usdc IS NOT NULL AND actual_incremental_other_holders_usdc IS NOT NULL AND actual_fee_lamports IS NOT NULL AND actual_excess_cost_usdc IS NOT NULL)),
    ADD CONSTRAINT backyard_withdrawal_cost_excess_incremental CHECK (actual_excess_cost_usdc IS NULL OR actual_excess_cost_usdc <= actual_incremental_cost_usdc),
    ADD CONSTRAINT backyard_withdrawal_cost_signature_unique UNIQUE (operation_id, receipt_signature);
