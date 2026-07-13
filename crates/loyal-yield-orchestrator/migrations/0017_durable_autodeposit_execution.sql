BEGIN;

-- A prepared pull is inserted before it is broadcast. Do not publish the
-- historical "pull confirmed" realtime event until chain evidence advances the
-- execution out of pull_confirmation_pending.
DROP TRIGGER IF EXISTS balance_sweep_executions_realtime_event
    ON loyal_yield.balance_sweep_executions;

ALTER TABLE loyal_yield.balance_sweep_executions
    ADD COLUMN IF NOT EXISTS lifecycle_state TEXT,
    ADD COLUMN IF NOT EXISTS claim_token TEXT,
    ADD COLUMN IF NOT EXISTS requested_amount_raw BIGINT,
    ADD COLUMN IF NOT EXISTS confirmed_pull_amount_raw BIGINT,
    ADD COLUMN IF NOT EXISTS reserved_amount_raw BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS top_up_reserve TEXT,
    ADD COLUMN IF NOT EXISTS top_up_market TEXT,
    ADD COLUMN IF NOT EXISTS top_up_liquidity_mint TEXT,
    ADD COLUMN IF NOT EXISTS top_up_policy_account TEXT,
    ADD COLUMN IF NOT EXISTS top_up_policy_seed BIGINT,
    ADD COLUMN IF NOT EXISTS top_up_route_modes TEXT[],
    ADD COLUMN IF NOT EXISTS active_attempt_kind TEXT;

ALTER TABLE loyal_yield.balance_sweep_executions
    ALTER COLUMN slot DROP NOT NULL;

CREATE TABLE IF NOT EXISTS loyal_yield.vault_operation_leases (
    cluster TEXT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    owner_token TEXT NOT NULL,
    fence BIGINT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    blocking_operation_kind TEXT,
    blocking_signature TEXT,
    blocking_blockhash TEXT,
    blocking_last_valid_block_height BIGINT,
    blocking_signed_transaction_base64 TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, vault_pubkey),
    CHECK (fence > 0),
    CHECK (
        (blocking_signature IS NULL
         AND blocking_operation_kind IS NULL
         AND blocking_blockhash IS NULL
         AND blocking_last_valid_block_height IS NULL
         AND blocking_signed_transaction_base64 IS NULL)
        OR
        (blocking_signature IS NOT NULL
         AND blocking_operation_kind IS NOT NULL
         AND blocking_blockhash IS NOT NULL
         AND blocking_last_valid_block_height IS NOT NULL
         AND blocking_signed_transaction_base64 IS NOT NULL)
    )
);

CREATE TABLE IF NOT EXISTS loyal_yield.balance_sweep_execution_attempts (
    id BIGSERIAL PRIMARY KEY,
    execution_id BIGINT NOT NULL
        REFERENCES loyal_yield.balance_sweep_executions(id) ON DELETE CASCADE,
    operation_kind TEXT NOT NULL
        CHECK (operation_kind IN ('pull', 'top_up')),
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    signature TEXT NOT NULL,
    blockhash TEXT,
    last_valid_block_height BIGINT,
    signed_transaction_base64 TEXT,
    classification TEXT NOT NULL
        CHECK (
            classification IN (
                'prepared',
                'landed',
                'failed',
                'expired_not_landed',
                'unknown'
            )
        ),
    prepared_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    broadcast_at TIMESTAMPTZ,
    classified_at TIMESTAMPTZ,
    confirmed_slot BIGINT,
    chain_error JSONB,
    evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    lease_owner_token TEXT,
    lease_fence BIGINT,
    UNIQUE (execution_id, operation_kind, attempt_number),
    UNIQUE (id, execution_id),
    UNIQUE (signature)
);

ALTER TABLE loyal_yield.balance_sweep_executions
    ADD COLUMN IF NOT EXISTS successful_top_up_attempt_id BIGINT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'balance_sweep_executions_successful_top_up_attempt_fkey'
          AND conrelid = 'loyal_yield.balance_sweep_executions'::regclass
    ) THEN
        ALTER TABLE loyal_yield.balance_sweep_executions
            ADD CONSTRAINT balance_sweep_executions_successful_top_up_attempt_fkey
            FOREIGN KEY (successful_top_up_attempt_id, id)
            REFERENCES loyal_yield.balance_sweep_execution_attempts(id, execution_id);
    END IF;
END $$;

-- Recover the real claim and slot identifiers before adding uniqueness guards.
UPDATE loyal_yield.balance_sweep_executions AS execution
SET claim_token = claim.claim_token
FROM loyal_yield.balance_sweep_lot_claims AS claim
WHERE claim.execution_id = execution.id
  AND execution.claim_token IS NULL;

UPDATE loyal_yield.balance_sweep_executions AS execution
SET scheduled_slot_id = slot.id
FROM loyal_yield.balance_sweep_scheduled_slots AS slot
WHERE slot.execution_id = execution.id
  AND execution.scheduled_slot_id IS NULL;

-- Backfill rows that have complete, independently verifiable application
-- linkage. This is deliberately one transaction so no historical row is ever
-- exposed as a new pull candidate between DDL and classification.
WITH completed_linkage AS (
    SELECT
        execution.id AS execution_id,
        deposit.id AS deposit_id,
        holding.position_id,
        deposit.deposit_signature,
        deposit.confirmed_at,
        deposit.confirmed_slot
    FROM loyal_yield.balance_sweep_executions AS execution
    JOIN loyal_yield.user_yield_position_deposits AS deposit
      ON deposit.deposit_signature = COALESCE(
          execution.kamino_deposit_signature,
          execution.decoded_evidence->>'kaminoDepositSignature'
      )
    JOIN LATERAL (
        SELECT event.position_id
        FROM loyal_yield.user_yield_position_holding_events AS event
        WHERE event.source_deposit_id = deposit.id
          AND event.source_signature = deposit.deposit_signature
        ORDER BY event.id DESC
        LIMIT 1
    ) AS holding ON TRUE
    WHERE (
        execution.completed_at IS NOT NULL
        OR execution.decoded_evidence->>'status' = 'executed'
      )
      AND (
          deposit.balance_sweep_execution_id IS NULL
          OR deposit.balance_sweep_execution_id = execution.id
      )
      AND (
          deposit.balance_sweep_scheduled_slot_id IS NULL
          OR deposit.balance_sweep_scheduled_slot_id = execution.scheduled_slot_id
      )
)
UPDATE loyal_yield.balance_sweep_executions AS execution
SET
    yield_deposit_id = COALESCE(execution.yield_deposit_id, linkage.deposit_id),
    yield_position_id = COALESCE(execution.yield_position_id, linkage.position_id),
    kamino_deposit_signature = COALESCE(
        execution.kamino_deposit_signature,
        linkage.deposit_signature
    ),
    completed_at = COALESCE(execution.completed_at, linkage.confirmed_at),
    requested_amount_raw = COALESCE(execution.requested_amount_raw, execution.amount_raw),
    confirmed_pull_amount_raw = COALESCE(
        execution.confirmed_pull_amount_raw,
        execution.amount_raw
    ),
    lifecycle_state = 'completed',
    reserved_amount_raw = 0,
    active_attempt_kind = NULL,
    completion_failure_code = NULL
FROM completed_linkage AS linkage
WHERE execution.id = linkage.execution_id;

UPDATE loyal_yield.user_yield_position_deposits AS deposit
SET
    balance_sweep_execution_id = execution.id,
    balance_sweep_scheduled_slot_id = execution.scheduled_slot_id
FROM loyal_yield.balance_sweep_executions AS execution
WHERE execution.lifecycle_state = 'completed'
  AND execution.yield_deposit_id = deposit.id
  AND (
      deposit.balance_sweep_execution_id IS NULL
      OR deposit.balance_sweep_execution_id = execution.id
  )
  AND (
      deposit.balance_sweep_scheduled_slot_id IS NULL
      OR deposit.balance_sweep_scheduled_slot_id = execution.scheduled_slot_id
  );

-- Historical partials and any row without complete linkage require explicit
-- chain reconciliation. They are never exposed as deposit_pending by DDL.
UPDATE loyal_yield.balance_sweep_executions
SET
    requested_amount_raw = COALESCE(requested_amount_raw, amount_raw),
    confirmed_pull_amount_raw = COALESCE(confirmed_pull_amount_raw, amount_raw),
    lifecycle_state = 'needs_reconciliation',
    reserved_amount_raw = CASE
        WHEN decoded_evidence->>'status' = 'partial_executed_pull_top_up_blocked'
        THEN amount_raw
        ELSE 0
    END,
    active_attempt_kind = NULL
WHERE lifecycle_state IS NULL;

-- Preserve every historical signature as an auditable attempt. Historical
-- signed bytes/blockhashes are intentionally unknown and are never rebuilt.
INSERT INTO loyal_yield.balance_sweep_execution_attempts (
    execution_id,
    operation_kind,
    attempt_number,
    signature,
    classification,
    prepared_at,
    classified_at,
    confirmed_slot,
    evidence
)
SELECT
    execution.id,
    'pull',
    1,
    execution.signature,
    'landed',
    COALESCE(execution.received_at, execution.inserted_at),
    COALESCE(execution.decoded_at, execution.received_at, execution.inserted_at),
    execution.slot,
    jsonb_build_object('source', 'legacy_balance_sweep_execution_backfill')
FROM loyal_yield.balance_sweep_executions AS execution
ON CONFLICT (signature) DO NOTHING;

INSERT INTO loyal_yield.balance_sweep_execution_attempts (
    execution_id,
    operation_kind,
    attempt_number,
    signature,
    classification,
    prepared_at,
    classified_at,
    confirmed_slot,
    evidence
)
SELECT
    execution.id,
    'top_up',
    1,
    execution.kamino_deposit_signature,
    'landed',
    execution.completed_at,
    execution.completed_at,
    deposit.confirmed_slot,
    jsonb_build_object('source', 'legacy_completed_autodeposit_backfill')
FROM loyal_yield.balance_sweep_executions AS execution
JOIN loyal_yield.user_yield_position_deposits AS deposit
  ON deposit.id = execution.yield_deposit_id
WHERE execution.lifecycle_state = 'completed'
  AND execution.kamino_deposit_signature IS NOT NULL
ON CONFLICT (signature) DO NOTHING;

UPDATE loyal_yield.balance_sweep_executions AS execution
SET successful_top_up_attempt_id = attempt.id
FROM loyal_yield.balance_sweep_execution_attempts AS attempt
WHERE attempt.execution_id = execution.id
  AND attempt.operation_kind = 'top_up'
  AND attempt.classification = 'landed'
  AND execution.successful_top_up_attempt_id IS NULL;

-- Keep the DDL/application rollout order safe for an older worker image that
-- may insert a confirmed pull while the new image is rolling out. The durable
-- lifecycle remains authoritative; this trigger maps legacy evidence into it
-- conservatively and reserves the confirmed amount for reconciliation.
CREATE OR REPLACE FUNCTION loyal_yield.initialize_durable_balance_sweep_execution()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.lifecycle_state := COALESCE(NEW.lifecycle_state, 'needs_reconciliation');
        NEW.requested_amount_raw := COALESCE(NEW.requested_amount_raw, NEW.amount_raw);
        IF NEW.lifecycle_state = 'needs_reconciliation' THEN
            NEW.confirmed_pull_amount_raw := COALESCE(
                NEW.confirmed_pull_amount_raw,
                NEW.amount_raw
            );
            NEW.reserved_amount_raw := GREATEST(
                COALESCE(NEW.reserved_amount_raw, 0),
                NEW.confirmed_pull_amount_raw
            );
        END IF;
    END IF;

    IF NEW.completed_at IS NOT NULL THEN
        NEW.lifecycle_state := 'completed';
        NEW.reserved_amount_raw := 0;
        NEW.active_attempt_kind := NULL;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS balance_sweep_executions_durable_lifecycle_compat
    ON loyal_yield.balance_sweep_executions;
CREATE TRIGGER balance_sweep_executions_durable_lifecycle_compat
BEFORE INSERT OR UPDATE OF completed_at ON loyal_yield.balance_sweep_executions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.initialize_durable_balance_sweep_execution();

CREATE OR REPLACE FUNCTION loyal_yield.capture_legacy_balance_sweep_execution_attempt()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.lifecycle_state = 'needs_reconciliation' AND NEW.slot IS NOT NULL THEN
        INSERT INTO loyal_yield.balance_sweep_execution_attempts (
            execution_id,
            operation_kind,
            attempt_number,
            signature,
            classification,
            prepared_at,
            classified_at,
            confirmed_slot,
            evidence
        ) VALUES (
            NEW.id,
            'pull',
            1,
            NEW.signature,
            'landed',
            COALESCE(NEW.received_at, NEW.inserted_at),
            COALESCE(NEW.decoded_at, NEW.received_at, NEW.inserted_at),
            NEW.slot,
            jsonb_build_object('source', 'legacy_worker_compatibility_trigger')
        )
        ON CONFLICT (signature) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS balance_sweep_executions_legacy_attempt_compat
    ON loyal_yield.balance_sweep_executions;
CREATE TRIGGER balance_sweep_executions_legacy_attempt_compat
AFTER INSERT ON loyal_yield.balance_sweep_executions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.capture_legacy_balance_sweep_execution_attempt();

ALTER TABLE loyal_yield.balance_sweep_executions
    ALTER COLUMN lifecycle_state SET NOT NULL,
    ALTER COLUMN lifecycle_state SET DEFAULT 'needs_reconciliation',
    ALTER COLUMN requested_amount_raw SET NOT NULL,
    DROP CONSTRAINT IF EXISTS balance_sweep_executions_lifecycle_state_check,
    ADD CONSTRAINT balance_sweep_executions_lifecycle_state_check CHECK (
        lifecycle_state IN (
            'pull_confirmation_pending',
            'deposit_pending',
            'deposit_confirmation_pending',
            'deposit_confirmed',
            'completed',
            'needs_reconciliation'
        )
    ),
    DROP CONSTRAINT IF EXISTS balance_sweep_executions_active_attempt_kind_check,
    ADD CONSTRAINT balance_sweep_executions_active_attempt_kind_check CHECK (
        active_attempt_kind IS NULL
        OR active_attempt_kind IN ('pull', 'top_up')
    ),
    DROP CONSTRAINT IF EXISTS balance_sweep_executions_reserved_amount_raw_check,
    ADD CONSTRAINT balance_sweep_executions_reserved_amount_raw_check
        CHECK (reserved_amount_raw >= 0),
    DROP CONSTRAINT IF EXISTS balance_sweep_executions_claim_token_fkey,
    ADD CONSTRAINT balance_sweep_executions_claim_token_fkey
        FOREIGN KEY (claim_token)
        REFERENCES loyal_yield.balance_sweep_lot_claims(claim_token);

CREATE UNIQUE INDEX IF NOT EXISTS balance_sweep_executions_claim_token_uidx
    ON loyal_yield.balance_sweep_executions (claim_token)
    WHERE claim_token IS NOT NULL;

DROP INDEX IF EXISTS loyal_yield.balance_sweep_executions_scheduled_slot_idx;
CREATE UNIQUE INDEX balance_sweep_executions_scheduled_slot_uidx
    ON loyal_yield.balance_sweep_executions (scheduled_slot_id)
    WHERE scheduled_slot_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS balance_sweep_executions_kamino_signature_uidx
    ON loyal_yield.balance_sweep_executions (kamino_deposit_signature)
    WHERE kamino_deposit_signature IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS balance_sweep_execution_attempts_landed_operation_uidx
    ON loyal_yield.balance_sweep_execution_attempts (execution_id, operation_kind)
    WHERE classification = 'landed';

CREATE INDEX IF NOT EXISTS balance_sweep_executions_recovery_idx
    ON loyal_yield.balance_sweep_executions (lifecycle_state, inserted_at, id)
    WHERE lifecycle_state <> 'completed';

CREATE INDEX IF NOT EXISTS balance_sweep_executions_vault_reservation_idx
    ON loyal_yield.balance_sweep_executions (target_id, token_mint)
    WHERE reserved_amount_raw > 0;

CREATE UNIQUE INDEX IF NOT EXISTS user_yield_position_deposits_sweep_execution_uidx
    ON loyal_yield.user_yield_position_deposits (balance_sweep_execution_id)
    WHERE balance_sweep_execution_id IS NOT NULL;

CREATE OR REPLACE FUNCTION loyal_yield.reserved_autodeposit_amount_raw(
    p_cluster TEXT,
    p_vault_pubkey TEXT,
    p_token_mint TEXT
)
RETURNS BIGINT
LANGUAGE sql
STABLE
AS $$
    SELECT COALESCE(SUM(execution.reserved_amount_raw), 0)::BIGINT
    FROM loyal_yield.balance_sweep_executions AS execution
    JOIN loyal_yield.balance_sweep_targets AS target
      ON target.id = execution.target_id
    WHERE COALESCE(NULLIF(target.cluster, ''), 'mainnet-beta') = p_cluster
      AND target.vault_pubkey = p_vault_pubkey
      AND execution.token_mint = p_token_mint
      AND execution.lifecycle_state <> 'completed'
      AND execution.reserved_amount_raw > 0
$$;

CREATE TRIGGER balance_sweep_executions_realtime_event
AFTER UPDATE OF lifecycle_state ON loyal_yield.balance_sweep_executions
FOR EACH ROW
WHEN (
    NEW.lifecycle_state = 'deposit_pending'
    AND OLD.lifecycle_state IS DISTINCT FROM NEW.lifecycle_state
)
EXECUTE FUNCTION loyal_yield.emit_autodeposit_execution_realtime_event();

COMMIT;
