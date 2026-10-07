-- Extend the finalized failed-broadcast proof branch to the restore lane.
-- Migration 0081's terminal branch still gates every finalized failure on the
-- retained receipt, signed-wire identity and booked fee evidence; only the
-- action predicate widens so a VOLTR_RESTORE_IDLE broadcast whose atomic
-- rollback was refused for the report slot may reach the same terminal state
-- under recovery_reason = 'adaptor_report_slot_refused'. Every other
-- lifecycle clause, reason and proof predicate stays exactly as 0081 wrote it.
-- The constraint is re-created NOT VALID and validated in the same statement
-- pair so deployment still proves existing rows satisfy it.

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
                    OR (
                        action IS NOT NULL AND (
                            action = 'REPORT_NAV'
                            OR (action = 'VOLTR_RESTORE_IDLE'
                                AND recovery_reason = 'adaptor_report_slot_refused')
                        )
                        AND confirmation_status IS NOT NULL AND confirmation_status = 'finalized'
                        AND confirmed_slot IS NOT NULL AND confirmed_slot > 0
                        AND signed_wire IS NOT NULL AND octet_length(signed_wire) > 65
                        AND signed_wire_sha256 IS NOT NULL
                        AND message_sha256 IS NOT NULL
                        AND transaction_signature IS NOT NULL
                        AND recent_blockhash IS NOT NULL
                        AND last_valid_block_height IS NOT NULL
                        AND simulation_slot IS NOT NULL AND simulation_result IS NOT NULL
                        AND broadcast_intent_at IS NOT NULL
                        AND reconciliation_sha256 IS NOT NULL AND reconciliation_sha256 ~ '^[0-9a-f]{64}$'
                        AND COALESCE(
                            reconciled_effects->>'schema' = 'backyard-finalized-failure/v1'
                            AND reconciled_effects->>'signature' = transaction_signature
                            AND reconciled_effects->>'signedWireSha256' = signed_wire_sha256
                            AND reconciled_effects->>'messageSha256' = message_sha256
                            AND (reconciled_effects->>'slot')::bigint = confirmed_slot
                            AND reconciled_effects->>'reason' = recovery_reason
                            AND recovery_reason IN ('adaptor_report_slot_refused',
                                'adaptor_report_ticket_replayed','report_expired_at_landing',
                                'squads_spending_limit_exceeded')
                            AND reconciled_effects->>'atomicNoCapitalMovement' = 'true'
                            AND (reconciled_effects->>'feeLamports')::bigint > 0
                            AND (reconciled_effects->>'bookedFeeMicros')::bigint > 0
                            AND expected_effects->'phase3'->>'goalId' = '01a06b6c-8023-72b1-ad5d-c97c0662820e'
                            AND expected_effects->'phase3'->>'signedWireSha256' = signed_wire_sha256
                            AND COALESCE(expected_effects->'phase3'->>'reservationReleased','false') = 'false'
                            AND (expected_effects->'phase3'->>'bookedSpentMicros')::bigint =
                                (reconciled_effects->>'bookedFeeMicros')::bigint
                            AND (expected_effects->'phase3'->>'pilotAuthorityId' IS NULL OR
                                (expected_effects->'phase3'->>'bookedExecutionCostMicros')::bigint =
                                (reconciled_effects->>'bookedFeeMicros')::bigint)
                            AND (reconciled_effects->'receipt'->>'slot')::bigint = confirmed_slot
                            AND reconciled_effects->'receipt'->'transaction'->>1 = 'base64'
                            AND reconciled_effects->'receipt'->'transaction'->>0 =
                                replace(encode(signed_wire,'base64'), E'\n', '')
                            AND jsonb_typeof(reconciled_effects->'receipt'->'meta'->'err') = 'object'
                            AND (reconciled_effects->'receipt'->'meta'->>'fee')::bigint =
                                (reconciled_effects->>'feeLamports')::bigint,
                            FALSE
                        )
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
