-- The Backyard worker no longer has a `submitted` status: a signed row goes
-- from broadcast_intent straight to confirmed, and the binary since the
-- one-writer cutover has not written `submitted`. Drop it from the status
-- list and drop the check that required submitted_at on it. No row is
-- changed; the constraint is re-created NOT VALID and validated so
-- deployment still proves no stored row carries the status.
--
-- Apply with or before the binary that removes the status: the previous
-- binary never writes it.
SET LOCAL lock_timeout = '5s';

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_status_check;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_status_check CHECK (status IN (
        'prepared',
        'signed_persisted',
        'broadcast_intent',
        'confirmed',
        'reconciliation_pending',
        'reconciled',
        'expired',
        'manual_recovery',
        'held',
        'decided',
        'built',
        'simulated',
        'signed',
        'reconciling',
        'failed'
    )) NOT VALID;

ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_status_check;

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_backyard_submission_evidence;
