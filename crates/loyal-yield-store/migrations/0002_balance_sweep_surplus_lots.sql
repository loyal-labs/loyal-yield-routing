ALTER TABLE loyal_yield.balance_sweep_targets
    ADD COLUMN IF NOT EXISTS lifecycle_status TEXT NOT NULL DEFAULT 'active',
    ADD COLUMN IF NOT EXISTS wallet_balance_floor_raw BIGINT,
    ADD COLUMN IF NOT EXISTS recurring_delegation TEXT,
    ADD COLUMN IF NOT EXISTS period_length_seconds BIGINT,
    ADD COLUMN IF NOT EXISTS start_timestamp BIGINT;

ALTER TABLE loyal_yield.balance_sweep_wallet_balances_current
    ADD COLUMN IF NOT EXISTS txn_signature TEXT;

CREATE TABLE IF NOT EXISTS loyal_yield.balance_sweep_wallet_balance_events (
    event_id BIGINT PRIMARY KEY,
    target_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_targets(id) ON DELETE CASCADE,
    wallet TEXT NOT NULL,
    wallet_usdc_ata TEXT NOT NULL,
    previous_amount_raw BIGINT,
    amount_raw BIGINT NOT NULL,
    delta_amount_raw BIGINT,
    observed_slot BIGINT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    source TEXT NOT NULL,
    source_commitment TEXT NOT NULL,
    txn_signature TEXT,
    account_data_hash TEXT,
    raw_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    projected_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS balance_sweep_wallet_balance_events_target_event_idx
    ON loyal_yield.balance_sweep_wallet_balance_events (target_id, event_id DESC);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_balance_events_target_slot_idx
    ON loyal_yield.balance_sweep_wallet_balance_events (target_id, observed_slot DESC);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_balance_events_txn_signature_idx
    ON loyal_yield.balance_sweep_wallet_balance_events (txn_signature)
    WHERE txn_signature IS NOT NULL;

DO $$
BEGIN
    CREATE TYPE loyal_yield.balance_sweep_surplus_classification AS ENUM (
        'earn_withdrawal',
        'simple_inbound',
        'complex_defi',
        'unknown',
        'explicit_redeposit'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    CREATE TYPE loyal_yield.balance_sweep_surplus_lot_status AS ENUM (
        'open',
        'selected',
        'consumed',
        'depleted',
        'suppressed'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS loyal_yield.balance_sweep_surplus_lots (
    id BIGSERIAL PRIMARY KEY,
    target_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_targets(id) ON DELETE CASCADE,
    source_event_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_wallet_balance_events(event_id) ON DELETE CASCADE,
    source_signature TEXT,
    original_amount_raw BIGINT NOT NULL CHECK (original_amount_raw > 0),
    remaining_amount_raw BIGINT NOT NULL CHECK (remaining_amount_raw >= 0),
    classification loyal_yield.balance_sweep_surplus_classification NOT NULL,
    eligible_after TIMESTAMPTZ NOT NULL,
    status loyal_yield.balance_sweep_surplus_lot_status NOT NULL DEFAULT 'open',
    confidence TEXT NOT NULL DEFAULT 'unknown',
    reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_event_id)
);

CREATE INDEX IF NOT EXISTS balance_sweep_surplus_lots_target_status_eligible_idx
    ON loyal_yield.balance_sweep_surplus_lots (target_id, status, eligible_after, id);
CREATE INDEX IF NOT EXISTS balance_sweep_surplus_lots_source_signature_idx
    ON loyal_yield.balance_sweep_surplus_lots (source_signature)
    WHERE source_signature IS NOT NULL;

DO $$
BEGIN
    CREATE TYPE loyal_yield.balance_sweep_lot_claim_status AS ENUM (
        'selected',
        'executed',
        'released',
        'failed'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS loyal_yield.balance_sweep_lot_claims (
    claim_token TEXT PRIMARY KEY,
    target_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_targets(id) ON DELETE CASCADE,
    amount_raw BIGINT NOT NULL CHECK (amount_raw > 0),
    status loyal_yield.balance_sweep_lot_claim_status NOT NULL DEFAULT 'selected',
    execution_id BIGINT REFERENCES loyal_yield.balance_sweep_executions(id) ON DELETE SET NULL,
    stale_check_event_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS balance_sweep_lot_claims_target_status_idx
    ON loyal_yield.balance_sweep_lot_claims (target_id, status, created_at DESC);

CREATE TABLE IF NOT EXISTS loyal_yield.balance_sweep_lot_claim_items (
    claim_token TEXT NOT NULL REFERENCES loyal_yield.balance_sweep_lot_claims(claim_token) ON DELETE CASCADE,
    lot_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_surplus_lots(id) ON DELETE RESTRICT,
    amount_raw BIGINT NOT NULL CHECK (amount_raw > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (claim_token, lot_id)
);

CREATE INDEX IF NOT EXISTS balance_sweep_lot_claim_items_lot_idx
    ON loyal_yield.balance_sweep_lot_claim_items (lot_id, created_at DESC);

CREATE TABLE IF NOT EXISTS loyal_yield.balance_sweep_execution_lots (
    execution_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_executions(id) ON DELETE CASCADE,
    lot_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_surplus_lots(id) ON DELETE RESTRICT,
    amount_raw BIGINT NOT NULL CHECK (amount_raw > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (execution_id, lot_id)
);

CREATE INDEX IF NOT EXISTS balance_sweep_execution_lots_lot_idx
    ON loyal_yield.balance_sweep_execution_lots (lot_id, created_at DESC);
