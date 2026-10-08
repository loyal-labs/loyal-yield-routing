-- A signature that remains absent after its last valid block height cannot
-- land. Preserve its complete submission identity while allowing that exact
-- terminal outcome to release the one-nonterminal operation fence.

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_backyard_lifecycle;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_backyard_lifecycle CHECK (
        engine_version <> 'backyard_rwa_v1'
        OR
        (
            (status = 'decided'
                AND signed_wire IS NULL
                AND transaction_signature IS NULL
                AND simulation_result IS NULL)
            OR
            (status = 'built'
                AND signed_wire IS NULL
                AND transaction_signature IS NULL
                AND message_sha256 IS NOT NULL
                AND simulation_result IS NULL)
            OR
            (status = 'simulated'
                AND signed_wire IS NULL
                AND transaction_signature IS NULL
                AND message_sha256 IS NOT NULL
                AND simulation_slot IS NOT NULL
                AND simulation_result IS NOT NULL)
            OR
            (status = 'signed'
                AND signed_wire IS NOT NULL
                AND signed_wire_sha256 IS NOT NULL
                AND transaction_signature IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
                AND simulation_slot IS NOT NULL
                AND simulation_result IS NOT NULL
                AND broadcast_intent_at IS NULL)
            OR
            (status IN ('broadcast_intent', 'submitted', 'confirmed', 'reconciling')
                AND signed_wire IS NOT NULL
                AND signed_wire_sha256 IS NOT NULL
                AND transaction_signature IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
                AND simulation_slot IS NOT NULL
                AND simulation_result IS NOT NULL
                AND broadcast_intent_at IS NOT NULL)
            OR
            (status = 'reconciled'
                AND signed_wire_sha256 IS NOT NULL
                AND transaction_signature IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
                AND broadcast_intent_at IS NOT NULL
                AND confirmed_slot IS NOT NULL
                AND confirmation_status IN ('confirmed', 'finalized')
                AND reconciliation_sha256 IS NOT NULL
                AND reconciled_effects IS NOT NULL)
            OR
            (status = 'failed'
                AND (
                    broadcast_intent_at IS NULL
                    OR (
                        recovery_reason = 'signature_absent_after_blockhash_expiry'
                        AND signed_wire_sha256 IS NOT NULL
                        AND transaction_signature IS NOT NULL
                        AND recent_blockhash IS NOT NULL
                        AND last_valid_block_height IS NOT NULL
                        AND simulation_slot IS NOT NULL
                        AND simulation_result IS NOT NULL
                        AND broadcast_intent_at IS NOT NULL
                        AND confirmed_slot IS NULL
                    )
                ))
            OR
            (status = 'held'
                AND action = 'HOLD'
                AND signed_wire IS NULL
                AND transaction_signature IS NULL
                AND broadcast_intent_at IS NULL)
            OR
            (status = 'manual_recovery' AND recovery_reason IS NOT NULL)
        )
    ) NOT VALID;

ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_backyard_lifecycle;
