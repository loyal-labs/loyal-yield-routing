ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_check;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_check CHECK (
        (status = 'prepared'
            AND signed_wire IS NULL
            AND transaction_signature IS NULL)
        OR
        (status = 'signed_persisted'
            AND signed_wire IS NOT NULL
            AND signed_wire_sha256 IS NOT NULL
            AND transaction_signature IS NOT NULL
            AND recent_blockhash IS NOT NULL
            AND last_valid_block_height IS NOT NULL
            AND broadcast_intent_at IS NULL)
        OR
        (status IN ('broadcast_intent', 'confirmed', 'reconciliation_pending')
            AND signed_wire IS NOT NULL
            AND signed_wire_sha256 IS NOT NULL
            AND transaction_signature IS NOT NULL
            AND recent_blockhash IS NOT NULL
            AND last_valid_block_height IS NOT NULL
            AND broadcast_intent_at IS NOT NULL)
        OR
        (status = 'reconciled'
            AND NOT (
                (action = 'deposit_claim_asset' AND engine_version = 'linus_v1')
                OR
                (action IN ('deposit_claim_asset', 'claim') AND engine_version = 'earn_max_v1')
            )
            AND signed_wire IS NULL
            AND signed_wire_sha256 IS NOT NULL
            AND transaction_signature IS NOT NULL
            AND recent_blockhash IS NOT NULL
            AND last_valid_block_height IS NOT NULL
            AND broadcast_intent_at IS NOT NULL
            AND confirmed_slot IS NOT NULL
            AND reconciliation_sha256 IS NOT NULL)
        OR
        (status = 'reconciled'
            AND (
                (action = 'deposit_claim_asset' AND engine_version = 'linus_v1')
                OR
                (action IN ('deposit_claim_asset', 'claim') AND engine_version = 'earn_max_v1')
            )
            AND signed_wire IS NULL
            AND signed_wire_sha256 IS NOT NULL
            AND transaction_signature IS NOT NULL
            AND recent_blockhash IS NOT NULL
            AND last_valid_block_height IS NULL
            AND broadcast_intent_at IS NULL
            AND confirmed_slot IS NOT NULL
            AND reconciliation_sha256 IS NOT NULL
            AND policy_account IS NULL
            AND policy_data_sha256 IS NULL)
        OR
        (status = 'expired'
            AND signed_wire IS NULL
            AND signed_wire_sha256 IS NOT NULL
            AND transaction_signature IS NOT NULL
            AND recent_blockhash IS NOT NULL
            AND last_valid_block_height IS NOT NULL
            AND confirmed_slot IS NULL)
        OR
        status = 'manual_recovery'
    ) NOT VALID;

ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_check;
