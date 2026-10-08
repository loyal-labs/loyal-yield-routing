ALTER TABLE loyal_yield.multiply_route_states
    DROP CONSTRAINT IF EXISTS multiply_route_states_schema_v6_or_v7;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM loyal_yield.multiply_route_states
        WHERE state ->> 'goal' = 'move'
           OR (
                state -> 'position' ->> 'kind' = 'active'
                AND state -> 'position' ->> 'strategyKey' <> 'syrup_usdc_usdc'
           )
    ) THEN
        RAISE EXCEPTION 'Earn MAX v3 requires every route to be outside Move and the PYUSD strategy';
    END IF;
END $$;

UPDATE loyal_yield.multiply_route_states
SET state = jsonb_set(
        state - 'frontend' - 'targetStrategyKey',
        '{schemaVersion}',
        '8'::jsonb
    ),
    updated_at = now()
WHERE (state ->> 'schemaVersion')::INTEGER IN (6, 7)
   OR state ? 'frontend'
   OR state ? 'targetStrategyKey';

ALTER TABLE loyal_yield.multiply_route_states
    ADD CONSTRAINT multiply_route_states_schema_v8
    CHECK (
        (state ->> 'schemaVersion')::INTEGER = 8
        AND NOT (state ? 'frontend')
        AND NOT (state ? 'targetStrategyKey')
        AND state ->> 'goal' <> 'move'
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
            AND engine_version = 'earn_max_v1'
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
            AND engine_version = 'earn_max_v1'
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
                OR (action IN ('deposit_claim_asset', 'claim') AND engine_version = 'earn_max_v1')
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
                OR (action IN ('deposit_claim_asset', 'claim') AND engine_version = 'earn_max_v1')
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
