CREATE TABLE loyal_yield.multiply_operations (
    operation_id TEXT PRIMARY KEY,
    route_key TEXT NOT NULL REFERENCES loyal_yield.multiply_route_states(route_key) ON DELETE CASCADE,
    cycle BIGINT NOT NULL CHECK (cycle > 0),
    engine_version TEXT NOT NULL CHECK (engine_version IN ('canary_migrated', 'linus_v1')),
    action TEXT NOT NULL CHECK (action IN (
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
    )),
    strategy_key TEXT,
    status TEXT NOT NULL CHECK (status IN (
        'prepared',
        'signed_persisted',
        'broadcast_intent',
        'confirmed',
        'reconciliation_pending',
        'reconciled',
        'expired',
        'manual_recovery'
    )),
    idempotency_key TEXT NOT NULL UNIQUE,
    expected_effects JSONB NOT NULL CHECK (jsonb_typeof(expected_effects) = 'object'),
    policy_account TEXT,
    policy_data_sha256 TEXT CHECK (policy_data_sha256 IS NULL OR policy_data_sha256 ~ '^[0-9a-f]{64}$'),
    message_sha256 TEXT CHECK (message_sha256 IS NULL OR message_sha256 ~ '^[0-9a-f]{64}$'),
    signed_wire BYTEA,
    signed_wire_sha256 TEXT CHECK (signed_wire_sha256 IS NULL OR signed_wire_sha256 ~ '^[0-9a-f]{64}$'),
    transaction_signature TEXT,
    recent_blockhash TEXT,
    last_valid_block_height BIGINT CHECK (last_valid_block_height IS NULL OR last_valid_block_height > 0),
    broadcast_intent_at TIMESTAMPTZ,
    confirmed_slot BIGINT CHECK (confirmed_slot IS NULL OR confirmed_slot > 0),
    reconciliation_sha256 TEXT CHECK (reconciliation_sha256 IS NULL OR reconciliation_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
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
            AND (action <> 'deposit_claim_asset' OR engine_version = 'canary_migrated')
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
            AND action = 'deposit_claim_asset'
            AND engine_version = 'linus_v1'
            AND signed_wire IS NULL
            AND signed_wire_sha256 IS NOT NULL
            AND transaction_signature IS NOT NULL
            AND recent_blockhash IS NOT NULL
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
    )
);

CREATE UNIQUE INDEX multiply_operations_one_nonterminal_per_route
    ON loyal_yield.multiply_operations(route_key)
    WHERE status IN ('prepared', 'signed_persisted', 'broadcast_intent', 'confirmed', 'reconciliation_pending');

CREATE UNIQUE INDEX multiply_operations_transaction_signature_unique
    ON loyal_yield.multiply_operations(transaction_signature)
    WHERE transaction_signature IS NOT NULL;

CREATE INDEX multiply_operations_route_cycle_idx
    ON loyal_yield.multiply_operations(route_key, cycle, created_at, operation_id);

INSERT INTO loyal_yield.multiply_operations (
    operation_id,
    route_key,
    cycle,
    engine_version,
    action,
    strategy_key,
    status,
    idempotency_key,
    expected_effects,
    policy_account,
    policy_data_sha256,
    message_sha256,
    signed_wire,
    signed_wire_sha256,
    transaction_signature,
    recent_blockhash,
    last_valid_block_height,
    broadcast_intent_at,
    confirmed_slot,
    reconciliation_sha256,
    created_at,
    updated_at
)
SELECT
    route.route_key || ':' || (receipt ->> 'idempotencyKey'),
    route.route_key,
    COALESCE((receipt -> 'plan' ->> 'optimizerEpoch')::BIGINT, (route.state ->> 'cycle')::BIGINT, 1),
    'canary_migrated',
    CASE
        WHEN receipt ->> 'idempotencyKey' LIKE '%user-deposit%' THEN 'deposit_claim_asset'
        WHEN receipt ->> 'idempotencyKey' LIKE '%initial-swap%' THEN 'swap_claim_to_collateral'
        WHEN receipt ->> 'idempotencyKey' LIKE '%initial-deposit%' OR receipt ->> 'idempotencyKey' LIKE '%loop-deposit%' THEN 'deposit_collateral'
        WHEN receipt ->> 'idempotencyKey' LIKE '%borrow%' THEN 'borrow_debt'
        WHEN receipt ->> 'idempotencyKey' LIKE '%debt-swap%' THEN 'swap_debt_to_collateral'
        WHEN receipt ->> 'idempotencyKey' LIKE '%reverse-swap%' THEN 'swap_collateral_to_debt'
        WHEN receipt ->> 'idempotencyKey' LIKE '%repay%' THEN 'repay_debt'
        WHEN receipt ->> 'idempotencyKey' LIKE '%withdraw-remainder%' THEN 'withdraw_remaining_collateral'
        WHEN receipt ->> 'idempotencyKey' LIKE '%withdraw%' THEN 'withdraw_collateral'
        WHEN receipt ->> 'idempotencyKey' LIKE '%close-to-claim%' THEN 'swap_collateral_to_claim'
        WHEN receipt ->> 'idempotencyKey' LIKE '%user-claim%' THEN 'claim'
        ELSE 'withdraw_collateral'
    END,
    'canary_migrated',
    CASE receipt ->> 'state'
        WHEN 'reconciled' THEN 'reconciled'
        WHEN 'expired' THEN 'expired'
        WHEN 'ambiguous' THEN 'manual_recovery'
        WHEN 'failed' THEN 'manual_recovery'
        WHEN 'signed_persisted' THEN 'signed_persisted'
        WHEN 'broadcast_intent' THEN 'broadcast_intent'
        WHEN 'confirmed' THEN 'confirmed'
        WHEN 'reconciliation_pending' THEN 'reconciliation_pending'
        ELSE 'manual_recovery'
    END,
    receipt ->> 'idempotencyKey',
    COALESCE(receipt -> 'plan', '{}'::JSONB),
    receipt -> 'policyEvidence' ->> 'policyPubkey',
    receipt -> 'policyEvidence' ->> 'deployedPolicySha256',
    receipt ->> 'messageHash',
    NULL,
    receipt ->> 'signedMessageHash',
    receipt ->> 'transactionSignature',
    receipt ->> 'recentBlockhash',
    (receipt ->> 'lastValidBlockHeight')::BIGINT,
    (receipt ->> 'broadcastIntentAt')::TIMESTAMPTZ,
    COALESCE((receipt ->> 'reconciledSlot')::BIGINT, (receipt ->> 'observedSlot')::BIGINT),
    receipt ->> 'reconciliationEvidenceHash',
    COALESCE((receipt ->> 'createdAt')::TIMESTAMPTZ, route.updated_at),
    COALESCE((receipt ->> 'updatedAt')::TIMESTAMPTZ, route.updated_at)
FROM loyal_yield.multiply_route_states route
CROSS JOIN LATERAL jsonb_array_elements(COALESCE(route.state -> 'submissionHistory', '[]'::JSONB)) receipt
WHERE receipt ->> 'idempotencyKey' IS NOT NULL
ON CONFLICT (idempotency_key) DO NOTHING;

DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    FOR constraint_name IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.multiply_route_states'::regclass
          AND contype = 'c'
    LOOP
        EXECUTE format('ALTER TABLE loyal_yield.multiply_route_states DROP CONSTRAINT %I', constraint_name);
    END LOOP;
END $$;

UPDATE loyal_yield.multiply_route_states route
SET
    state_version = route.state_version + 1,
    state = jsonb_build_object(
        'schemaVersion', 4,
        'engineVersion', 'linus_v1',
        'routeKey', route.route_key,
        'vaultId', route.vault_id,
        'generation', route.state_version + 1,
        'cycle', COALESCE((route.state ->> 'cycle')::BIGINT, 1),
        'goal', 'idle',
        'targetStrategyKey', NULL,
        'position', jsonb_build_object(
            'kind', 'idle',
            'claim', jsonb_build_object(
                'account', route.state -> 'currentIdle' ->> 'custodyAccount',
                'mint', route.state -> 'currentIdle' ->> 'liquidityMint',
                'tokenProgram', 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA',
                'amountRaw', COALESCE((route.state -> 'currentIdle' ->> 'amountRaw')::BIGINT, 0)
            )
        ),
        'deposit', NULL,
        'withdrawal', NULL,
        'currentOperationId', NULL,
        'manualRecoveryReason', NULL,
        'observedSlot', COALESCE((route.state -> 'currentIdle' ->> 'observedSlot')::BIGINT, 1),
        'observedAt', COALESCE(route.state -> 'currentIdle' ->> 'observedAt', route.updated_at::TEXT),
        'frontend', jsonb_build_object(
            'generation', route.state_version + 1,
            'strategyKey', NULL,
            'claimAmountRaw', COALESCE((route.state -> 'currentIdle' ->> 'amountRaw')::BIGINT, 0),
            'collateralAmountRaw', 0,
            'debtAmountRaw', 0,
            'withdrawalStatus', NULL,
            'status', 'idle',
            'observedSlot', COALESCE((route.state -> 'currentIdle' ->> 'observedSlot')::BIGINT, 1)
        )
    ),
    updated_at = now();

ALTER TABLE loyal_yield.multiply_route_states
    DROP COLUMN pending_signed_wire,
    DROP COLUMN pending_signed_wire_sha256,
    DROP COLUMN pending_transaction_signature,
    DROP COLUMN pending_recent_blockhash,
    DROP COLUMN pending_last_valid_block_height,
    DROP COLUMN pending_broadcast_intent_at;

ALTER TABLE loyal_yield.multiply_route_states
    ADD CONSTRAINT multiply_route_states_state_version_positive CHECK (state_version > 0),
    ADD CONSTRAINT multiply_route_states_fencing_token_nonnegative CHECK (fencing_token >= 0),
    ADD CONSTRAINT multiply_route_states_state_object CHECK (jsonb_typeof(state) = 'object'),
    ADD CONSTRAINT multiply_route_states_schema_v4 CHECK ((state ->> 'schemaVersion')::INTEGER = 4),
    ADD CONSTRAINT multiply_route_states_route_identity CHECK (state ->> 'routeKey' = route_key),
    ADD CONSTRAINT multiply_route_states_vault_identity CHECK ((state ->> 'vaultId')::BIGINT = vault_id),
    ADD CONSTRAINT multiply_route_states_generation_identity CHECK ((state ->> 'generation')::BIGINT = state_version),
    ADD CONSTRAINT multiply_route_states_lease_coherent CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL));
