-- Preserve pre-v2 rows as inert history. New route rows are schema 9 and are
-- admitted only from a confirmed earn-max-v2 three-policy projection.
ALTER TABLE loyal_yield.multiply_route_states
    DROP CONSTRAINT IF EXISTS multiply_route_states_schema_v8;

ALTER TABLE loyal_yield.multiply_route_states
    ADD CONSTRAINT multiply_route_states_schema_v8_or_v9 CHECK (
        (
            (state ->> 'schemaVersion')::INTEGER = 8
            AND state ->> 'engineVersion' = 'earn_max_v1'
        )
        OR
        (
            (state ->> 'schemaVersion')::INTEGER = 9
            AND state ->> 'engineVersion' = 'earn_max_v2'
        )
    );

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_engine_version_check;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_engine_version_check CHECK (
        engine_version IN ('canary_migrated', 'linus_v1', 'earn_max_v1', 'earn_max_v2')
    );

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_check;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_check CHECK (
        (status = 'prepared' AND signed_wire IS NULL AND transaction_signature IS NULL)
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
            AND action IN ('request_withdrawal', 'cancel_withdrawal')
            AND engine_version IN ('earn_max_v1', 'earn_max_v2')
            AND signed_wire IS NULL
            AND signed_wire_sha256 IS NULL
            AND transaction_signature IS NOT NULL
            AND source_instruction_index IS NOT NULL
            AND recent_blockhash IS NULL
            AND last_valid_block_height IS NULL
            AND broadcast_intent_at IS NULL
            AND confirmed_slot IS NOT NULL
            AND reconciliation_sha256 IS NOT NULL)
        OR
        (status = 'reconciled'
            AND action IN ('deposit_claim_asset', 'claim')
            AND engine_version IN ('earn_max_v1', 'earn_max_v2')
            AND signed_wire IS NULL
            AND signed_wire_sha256 IS NULL
            AND transaction_signature IS NOT NULL
            AND source_instruction_index IS NOT NULL
            AND recent_blockhash IS NULL
            AND last_valid_block_height IS NULL
            AND broadcast_intent_at IS NULL
            AND confirmed_slot IS NOT NULL
            AND reconciliation_sha256 IS NOT NULL
            AND policy_account IS NULL
            AND policy_data_sha256 IS NULL)
        OR
        (status = 'reconciled'
            AND NOT (
                action IN ('request_withdrawal', 'cancel_withdrawal')
                OR (action = 'deposit_claim_asset' AND engine_version = 'linus_v1')
                OR (action IN ('deposit_claim_asset', 'claim')
                    AND engine_version IN ('earn_max_v1', 'earn_max_v2'))
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
                OR (action IN ('deposit_claim_asset', 'claim')
                    AND engine_version IN ('earn_max_v1', 'earn_max_v2'))
            )
            AND signed_wire IS NULL
            AND signed_wire_sha256 IS NOT NULL
            AND transaction_signature IS NOT NULL
            AND source_instruction_index IS NULL
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
        OR status = 'manual_recovery'
    ) NOT VALID;

ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_check;

ALTER TABLE loyal_yield.multiply_position_snapshots
    DROP CONSTRAINT IF EXISTS multiply_position_snapshots_route_key_generation_key;

CREATE UNIQUE INDEX IF NOT EXISTS multiply_position_snapshots_route_slot_unique
    ON loyal_yield.multiply_position_snapshots (route_key, observed_slot);
