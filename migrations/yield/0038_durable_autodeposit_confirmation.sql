CREATE TABLE IF NOT EXISTS loyal_yield.balance_sweep_transaction_attempts (
    id BIGSERIAL PRIMARY KEY,
    claim_token TEXT NOT NULL
        REFERENCES loyal_yield.balance_sweep_lot_claims(claim_token) ON DELETE RESTRICT,
    target_id BIGINT NOT NULL
        REFERENCES loyal_yield.balance_sweep_targets(id) ON DELETE CASCADE,
    scheduled_slot_id BIGINT
        REFERENCES loyal_yield.balance_sweep_scheduled_slots(id) ON DELETE RESTRICT,
    execution_id BIGINT
        REFERENCES loyal_yield.balance_sweep_executions(id) ON DELETE RESTRICT,
    operation_kind TEXT NOT NULL,
    attempt_number INTEGER NOT NULL DEFAULT 1,
    amount_raw BIGINT NOT NULL,
    source_pre_balance_raw BIGINT NOT NULL,
    destination_pre_balance_raw BIGINT NOT NULL,
    signature TEXT NOT NULL UNIQUE,
    signed_transaction_base64 TEXT NOT NULL,
    signed_transaction_sha256 TEXT NOT NULL,
    recent_blockhash TEXT NOT NULL,
    last_valid_block_height BIGINT NOT NULL,
    attempt_state TEXT NOT NULL DEFAULT 'prepared',
    broadcast_count INTEGER NOT NULL DEFAULT 0,
    last_broadcast_at TIMESTAMPTZ,
    last_status_checked_at TIMESTAMPTZ,
    confirmed_slot BIGINT,
    error_detail TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT balance_sweep_transaction_attempts_operation_check CHECK (
        operation_kind IN ('pull', 'top_up')
    ),
    CONSTRAINT balance_sweep_transaction_attempts_state_check CHECK (
        attempt_state IN (
            'prepared', 'submitted', 'confirmed', 'failed',
            'expired', 'unknown', 'ambiguous'
        )
    ),
    CONSTRAINT balance_sweep_transaction_attempts_wire_check CHECK (
        NULLIF(btrim(signature), '') IS NOT NULL
        AND NULLIF(btrim(signed_transaction_base64), '') IS NOT NULL
        AND signed_transaction_sha256 ~ '^[0-9a-f]{64}$'
        AND NULLIF(btrim(recent_blockhash), '') IS NOT NULL
        AND last_valid_block_height >= 0
        AND attempt_number > 0
        AND amount_raw > 0
        AND source_pre_balance_raw >= amount_raw
        AND destination_pre_balance_raw >= 0
        AND broadcast_count >= 0
        AND (confirmed_slot IS NULL OR confirmed_slot >= 0)
    ),
    UNIQUE (claim_token, operation_kind, attempt_number)
);

CREATE UNIQUE INDEX IF NOT EXISTS balance_sweep_transaction_attempts_active_uidx
    ON loyal_yield.balance_sweep_transaction_attempts (claim_token, operation_kind)
    WHERE attempt_state IN (
        'prepared', 'submitted', 'confirmed', 'unknown', 'ambiguous'
    );

CREATE INDEX IF NOT EXISTS balance_sweep_transaction_attempts_recovery_idx
    ON loyal_yield.balance_sweep_transaction_attempts
        (attempt_state, updated_at, id)
    WHERE attempt_state IN (
        'prepared', 'submitted', 'confirmed', 'unknown', 'ambiguous'
    );

CREATE INDEX IF NOT EXISTS balance_sweep_transaction_attempts_execution_idx
    ON loyal_yield.balance_sweep_transaction_attempts
        (execution_id, operation_kind, attempt_number DESC)
    WHERE execution_id IS NOT NULL;

CREATE OR REPLACE FUNCTION loyal_yield.guard_balance_sweep_attempt_wire()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.claim_token IS DISTINCT FROM OLD.claim_token
       OR NEW.target_id IS DISTINCT FROM OLD.target_id
       OR NEW.scheduled_slot_id IS DISTINCT FROM OLD.scheduled_slot_id
       OR NEW.execution_id IS DISTINCT FROM OLD.execution_id
       OR NEW.operation_kind IS DISTINCT FROM OLD.operation_kind
       OR NEW.attempt_number IS DISTINCT FROM OLD.attempt_number
       OR NEW.amount_raw IS DISTINCT FROM OLD.amount_raw
       OR NEW.source_pre_balance_raw IS DISTINCT FROM OLD.source_pre_balance_raw
       OR NEW.destination_pre_balance_raw IS DISTINCT FROM OLD.destination_pre_balance_raw
       OR NEW.signature IS DISTINCT FROM OLD.signature
       OR NEW.signed_transaction_base64 IS DISTINCT FROM OLD.signed_transaction_base64
       OR NEW.signed_transaction_sha256 IS DISTINCT FROM OLD.signed_transaction_sha256
       OR NEW.recent_blockhash IS DISTINCT FROM OLD.recent_blockhash
       OR NEW.last_valid_block_height IS DISTINCT FROM OLD.last_valid_block_height THEN
        RAISE EXCEPTION 'autodeposit signed-attempt wire identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS guard_balance_sweep_attempt_wire
    ON loyal_yield.balance_sweep_transaction_attempts;
CREATE TRIGGER guard_balance_sweep_attempt_wire
BEFORE UPDATE ON loyal_yield.balance_sweep_transaction_attempts
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_balance_sweep_attempt_wire();

COMMENT ON TABLE loyal_yield.balance_sweep_transaction_attempts IS
    'Exact signed autodeposit transactions persisted before first broadcast and retained until signature reconciliation is conclusive.';
