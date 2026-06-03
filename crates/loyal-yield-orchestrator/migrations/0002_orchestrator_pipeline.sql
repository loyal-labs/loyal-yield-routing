DO $$
BEGIN
    CREATE TYPE loyal_yield.worker_job_status AS ENUM (
        'pending',
        'leased',
        'succeeded',
        'failed',
        'dead'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    CREATE TYPE loyal_yield.rebalance_attempt_status AS ENUM (
        'building',
        'simulated',
        'ready',
        'batched',
        'failed',
        'expired'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    CREATE TYPE loyal_yield.rebalance_batch_status AS ENUM (
        'building',
        'signed',
        'submitted',
        'confirming',
        'confirmed',
        'failed',
        'expired'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS loyal_yield.worker_cursors (
    worker_kind TEXT NOT NULL,
    cluster TEXT NOT NULL,
    partition_key TEXT NOT NULL DEFAULT '',
    cursor JSONB NOT NULL,
    observed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (worker_kind, cluster, partition_key)
);

CREATE TABLE IF NOT EXISTS loyal_yield.reserve_targets_current (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    strategy TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    target_reserve TEXT NOT NULL,
    target_market TEXT,
    target_supply_apy_bps BIGINT NOT NULL,
    observed_slot BIGINT,
    observed_at TIMESTAMPTZ NOT NULL,
    source_cursor JSONB NOT NULL DEFAULT '{}'::jsonb,
    filters JSONB NOT NULL DEFAULT '{}'::jsonb,
    target_epoch TEXT NOT NULL,
    stale BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (cluster, strategy, liquidity_mint)
);

CREATE INDEX IF NOT EXISTS reserve_targets_current_cluster_idx
    ON loyal_yield.reserve_targets_current (cluster, strategy, stale, updated_at DESC);

CREATE TABLE IF NOT EXISTS loyal_yield.reserve_target_snapshots (
    id BIGSERIAL PRIMARY KEY,
    target_id BIGINT REFERENCES loyal_yield.reserve_targets_current(id) ON DELETE SET NULL,
    cluster TEXT NOT NULL,
    strategy TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    target_reserve TEXT NOT NULL,
    target_market TEXT,
    target_supply_apy_bps BIGINT NOT NULL,
    previous_target_reserve TEXT,
    observed_slot BIGINT,
    observed_at TIMESTAMPTZ NOT NULL,
    source_cursor JSONB NOT NULL DEFAULT '{}'::jsonb,
    reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS reserve_target_snapshots_lookup_idx
    ON loyal_yield.reserve_target_snapshots (cluster, strategy, liquidity_mint, created_at DESC);

CREATE TABLE IF NOT EXISTS loyal_yield.vault_reconcile_jobs (
    id BIGSERIAL PRIMARY KEY,
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id) ON DELETE CASCADE,
    target_id BIGINT REFERENCES loyal_yield.reserve_targets_current(id) ON DELETE SET NULL,
    cluster TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    target_reserve TEXT NOT NULL,
    target_epoch TEXT NOT NULL,
    status loyal_yield.worker_job_status NOT NULL DEFAULT 'pending',
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error_code TEXT,
    last_error_message TEXT,
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (idempotency_key)
);

CREATE INDEX IF NOT EXISTS vault_reconcile_jobs_claim_idx
    ON loyal_yield.vault_reconcile_jobs (status, next_attempt_at, created_at)
    WHERE status IN ('pending', 'leased', 'failed');

CREATE INDEX IF NOT EXISTS vault_reconcile_jobs_vault_idx
    ON loyal_yield.vault_reconcile_jobs (vault_id, status, created_at DESC);

CREATE TABLE IF NOT EXISTS loyal_yield.rebalance_attempts (
    id BIGSERIAL PRIMARY KEY,
    decision_id BIGINT NOT NULL REFERENCES loyal_yield.rebalance_decisions(id) ON DELETE CASCADE,
    status loyal_yield.rebalance_attempt_status NOT NULL DEFAULT 'building',
    attempt_kind TEXT NOT NULL DEFAULT 'same_mint',
    worker_id TEXT,
    simulation_slot BIGINT,
    compute_units BIGINT,
    logs_hash TEXT,
    error_code TEXT,
    error_message TEXT,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (idempotency_key)
);

CREATE INDEX IF NOT EXISTS rebalance_attempts_decision_idx
    ON loyal_yield.rebalance_attempts (decision_id, created_at DESC);

CREATE TABLE IF NOT EXISTS loyal_yield.rebalance_batches (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    status loyal_yield.rebalance_batch_status NOT NULL DEFAULT 'building',
    signer TEXT NOT NULL,
    fee_payer TEXT NOT NULL,
    transaction_version TEXT NOT NULL DEFAULT 'legacy',
    signature TEXT,
    recent_blockhash TEXT,
    last_valid_block_height BIGINT,
    signed_transaction BYTEA,
    submitted_at TIMESTAMPTZ,
    confirmed_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    error_code TEXT,
    error_message TEXT,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (idempotency_key)
);

CREATE INDEX IF NOT EXISTS rebalance_batches_status_idx
    ON loyal_yield.rebalance_batches (status, updated_at)
    WHERE status IN ('signed', 'submitted', 'confirming');

CREATE TABLE IF NOT EXISTS loyal_yield.rebalance_batch_decisions (
    batch_id BIGINT NOT NULL REFERENCES loyal_yield.rebalance_batches(id) ON DELETE CASCADE,
    decision_id BIGINT NOT NULL REFERENCES loyal_yield.rebalance_decisions(id) ON DELETE CASCADE,
    attempt_id BIGINT REFERENCES loyal_yield.rebalance_attempts(id) ON DELETE SET NULL,
    position SMALLINT NOT NULL,
    status TEXT NOT NULL DEFAULT 'included',
    error_code TEXT,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (batch_id, decision_id),
    UNIQUE (batch_id, position)
);

CREATE INDEX IF NOT EXISTS rebalance_batch_decisions_decision_idx
    ON loyal_yield.rebalance_batch_decisions (decision_id, created_at DESC);

CREATE TABLE IF NOT EXISTS loyal_yield.solana_account_cache (
    cluster TEXT NOT NULL,
    pubkey TEXT NOT NULL,
    owner TEXT NOT NULL,
    lamports BIGINT NOT NULL,
    data_hash TEXT NOT NULL,
    data_bytes BYTEA,
    decoded_json JSONB,
    observed_slot BIGINT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    cache_class TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, pubkey, cache_class)
);

CREATE INDEX IF NOT EXISTS solana_account_cache_expiry_idx
    ON loyal_yield.solana_account_cache (expires_at);

CREATE TABLE IF NOT EXISTS loyal_yield.worker_events (
    id BIGSERIAL PRIMARY KEY,
    worker_kind TEXT NOT NULL,
    worker_id TEXT,
    cluster TEXT,
    vault_id BIGINT,
    decision_id BIGINT,
    attempt_id BIGINT,
    batch_id BIGINT,
    signature TEXT,
    event_type TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS worker_events_lookup_idx
    ON loyal_yield.worker_events (worker_kind, created_at DESC);
