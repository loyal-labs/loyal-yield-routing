CREATE TABLE IF NOT EXISTS loyal_yield.earn_reconciliation_jobs (
    id BIGSERIAL PRIMARY KEY,
    consumer_name TEXT NOT NULL,
    event_key TEXT NOT NULL,
    durable_slot BIGINT NOT NULL CHECK (durable_slot >= 0),
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL CHECK (vault_index BETWEEN 0 AND 255),
    vault_pubkey TEXT NOT NULL,
    event_payload JSONB NOT NULL,
    vault_payload JSONB NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    claim_owner TEXT,
    claim_expires_at TIMESTAMPTZ,
    last_error TEXT,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (consumer_name, event_key, settings, vault_index, vault_pubkey),
    CHECK (
        (claim_owner IS NULL AND claim_expires_at IS NULL)
        OR (claim_owner IS NOT NULL AND claim_expires_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS earn_reconciliation_jobs_ready_idx
    ON loyal_yield.earn_reconciliation_jobs
        (consumer_name, next_attempt_at, durable_slot, id)
    WHERE completed_at IS NULL;
