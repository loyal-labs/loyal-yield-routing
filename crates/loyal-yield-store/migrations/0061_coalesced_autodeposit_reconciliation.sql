CREATE TABLE IF NOT EXISTS loyal_yield.autodeposit_reconciliation_requests (
    target_id BIGINT NOT NULL
        REFERENCES loyal_yield.balance_sweep_targets(id) ON DELETE CASCADE,
    requested_slot BIGINT NOT NULL CHECK (requested_slot >= 0),
    processed_slot BIGINT NOT NULL DEFAULT 0 CHECK (processed_slot >= 0),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    claim_owner TEXT,
    claim_expires_at TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (target_id),
    CHECK (processed_slot <= requested_slot),
    CHECK (
        (claim_owner IS NULL AND claim_expires_at IS NULL)
        OR (claim_owner IS NOT NULL AND claim_expires_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS autodeposit_reconciliation_requests_ready_idx
    ON loyal_yield.autodeposit_reconciliation_requests
        (next_attempt_at, requested_slot, target_id)
    WHERE processed_slot < requested_slot;

COMMENT ON TABLE loyal_yield.autodeposit_reconciliation_requests IS
    'One bounded high-water reconciliation request per Autodeposit target; contains no raw chain history.';
