CREATE TABLE IF NOT EXISTS loyal_yield.earn_reconciliation_jobs (
    id BIGSERIAL PRIMARY KEY,
    environment TEXT NOT NULL,
    settings TEXT NOT NULL,
    wallet TEXT NOT NULL,
    vault_index INTEGER NOT NULL,
    vault_pubkey TEXT NOT NULL,
    highest_trigger_slot BIGINT NOT NULL,
    latest_signature TEXT,
    trigger_kinds TEXT[] NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'queued',
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (environment, vault_pubkey),
    CONSTRAINT earn_reconciliation_jobs_status_check CHECK (
        status IN ('queued', 'leased', 'retryable', 'completed', 'skipped')
    )
);

CREATE TABLE IF NOT EXISTS loyal_yield.earn_reconciliation_receipts (
    consumer_name TEXT NOT NULL,
    event_key TEXT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    filter_name TEXT NOT NULL,
    event_kind TEXT NOT NULL,
    trigger_slot BIGINT NOT NULL,
    signature TEXT,
    account_pubkey TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (consumer_name, event_key, vault_pubkey)
);

CREATE TABLE IF NOT EXISTS loyal_yield.laserstream_replay_cursors (
    consumer_name TEXT PRIMARY KEY,
    durable_slot BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS earn_reconciliation_jobs_status_idx
    ON loyal_yield.earn_reconciliation_jobs (status, available_at, id);
CREATE INDEX IF NOT EXISTS earn_reconciliation_receipts_vault_idx
    ON loyal_yield.earn_reconciliation_receipts (vault_pubkey, trigger_slot);
