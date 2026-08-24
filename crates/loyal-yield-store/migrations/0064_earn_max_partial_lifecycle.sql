ALTER TABLE loyal_yield.multiply_route_states
    DROP CONSTRAINT IF EXISTS multiply_route_states_schema_v5_or_v6;

ALTER TABLE loyal_yield.multiply_route_states
    DROP CONSTRAINT IF EXISTS multiply_route_states_schema_v6_or_v7;

UPDATE loyal_yield.multiply_route_states
SET state = CASE
        WHEN state -> 'withdrawal' IS NULL OR state -> 'withdrawal' = 'null'::jsonb THEN
            jsonb_set(state, '{schemaVersion}', '7'::jsonb)
        ELSE
            jsonb_set(
                jsonb_set(state, '{schemaVersion}', '7'::jsonb) #- '{withdrawal,claimableAt}',
                '{withdrawal,readyBy}',
                COALESCE(
                    state #> '{withdrawal,readyBy}',
                    state #> '{withdrawal,claimableAt}'
                ),
                true
            )
    END,
    state_version = GREATEST(
        state_version,
        COALESCE((state ->> 'generation')::BIGINT, state_version)
    ),
    updated_at = now()
WHERE (state ->> 'schemaVersion')::INTEGER IN (5, 6);

ALTER TABLE loyal_yield.multiply_route_states
    ADD CONSTRAINT multiply_route_states_schema_v6_or_v7
    CHECK ((state ->> 'schemaVersion')::INTEGER IN (6, 7));

ALTER TABLE loyal_yield.multiply_operations
    ADD COLUMN IF NOT EXISTS source_instruction_index INTEGER
    CHECK (source_instruction_index IS NULL OR source_instruction_index >= 0);

DROP INDEX IF EXISTS loyal_yield.multiply_operations_transaction_signature_unique;

CREATE UNIQUE INDEX multiply_operations_transaction_signature_unique
    ON loyal_yield.multiply_operations(transaction_signature)
    WHERE transaction_signature IS NOT NULL AND source_instruction_index IS NULL;

CREATE UNIQUE INDEX multiply_operations_source_instruction_unique
    ON loyal_yield.multiply_operations(transaction_signature, source_instruction_index)
    WHERE transaction_signature IS NOT NULL AND source_instruction_index IS NOT NULL;

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_action_check;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_action_check CHECK (action IN (
        'request_withdrawal',
        'cancel_withdrawal',
        'deposit_claim_asset',
        'swap_claim_to_collateral',
        'deposit_collateral',
        'borrow_debt',
        'swap_debt_to_collateral',
        'withdraw_collateral',
        'swap_collateral_to_debt',
        'repay_debt',
        'withdraw_remaining_collateral',
        'swap_collateral_to_claim',
        'claim'
    ));

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
