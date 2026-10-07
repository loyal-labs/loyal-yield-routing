-- Retain setup admin wires before simulation RPC. Preserve every existing
-- lifecycle clause; only the two already-scoped setup actions may have a
-- complete signed-unsimulated Built row. This never makes Built sendable.

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
                AND message_sha256 IS NOT NULL
                AND simulation_result IS NULL
                AND (
                    (signed_wire IS NULL AND transaction_signature IS NULL)
                    OR (
                        action IS NOT NULL AND action IN ('POLICY_SETUP_PREFUND','POLICY_SETUP_CREATE')
                        AND strategy_key IS NOT NULL AND strategy_key = 'OnRe/ONyc/USDC'
                        AND signed_wire IS NOT NULL AND octet_length(signed_wire) > 65
                        AND signed_wire_sha256 IS NOT NULL
                        AND transaction_signature IS NOT NULL
                        AND recent_blockhash IS NOT NULL
                        AND last_valid_block_height IS NOT NULL AND last_valid_block_height > 0
                        AND simulation_slot IS NULL AND broadcast_intent_at IS NULL
                        AND COALESCE(expected_effects->'phase3'->>'goalId' = '01a06b6c-8023-72b1-ad5d-c97c0662820e', FALSE)
                        AND COALESCE(expected_effects->'phase3'->>'signedWireSha256' = signed_wire_sha256, FALSE)
                    )
                ))
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
