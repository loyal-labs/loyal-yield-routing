CREATE SCHEMA IF NOT EXISTS loyal_yield;

DO $$
BEGIN
    CREATE TYPE loyal_yield.decision_status AS ENUM (
        'planned',
        'simulating',
        'ready',
        'submitted',
        'confirming',
        'confirmed',
        'failed',
        'abandoned',
        'skipped'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

ALTER TYPE loyal_yield.decision_status ADD VALUE IF NOT EXISTS 'skipped';

DO $$
BEGIN
    CREATE TYPE loyal_yield.decision_reason AS ENUM (
        'target_supply_apy_exceeds_source',
        'active_decision',
        'no_value_source',
        'cross_mint_only',
        'no_same_mint_edge'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

ALTER TYPE loyal_yield.decision_reason ADD VALUE IF NOT EXISTS 'active_decision';
ALTER TYPE loyal_yield.decision_reason ADD VALUE IF NOT EXISTS 'no_value_source';
ALTER TYPE loyal_yield.decision_reason ADD VALUE IF NOT EXISTS 'cross_mint_only';
ALTER TYPE loyal_yield.decision_reason ADD VALUE IF NOT EXISTS 'no_same_mint_edge';

CREATE TABLE IF NOT EXISTS loyal_yield.route_policies (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    settings TEXT NOT NULL,
    authority TEXT NOT NULL,
    policy_seed BIGINT NOT NULL,
    policy_account TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    delegated_signers TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    threshold INTEGER NOT NULL,
    route_modes TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    stable_mints TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    kamino_markets TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    kamino_liquidity_mints TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    universe_preset TEXT,
    risk_profile TEXT,
    swap_lanes JSONB NOT NULL DEFAULT '[]'::jsonb,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_slot BIGINT NOT NULL,
    last_seen_signature TEXT NOT NULL,
    UNIQUE (cluster, policy_account)
);

CREATE INDEX IF NOT EXISTS route_policies_active_idx
    ON loyal_yield.route_policies (cluster, active, settings, vault_index);

CREATE TABLE IF NOT EXISTS loyal_yield.managed_vaults (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    active_policy_id BIGINT NOT NULL REFERENCES loyal_yield.route_policies(id),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (cluster, settings, vault_index, vault_pubkey)
);

CREATE INDEX IF NOT EXISTS managed_vaults_active_idx
    ON loyal_yield.managed_vaults (cluster, active, active_policy_id);

CREATE TABLE IF NOT EXISTS loyal_yield.vault_position_snapshots (
    id BIGSERIAL PRIMARY KEY,
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id),
    policy_id BIGINT NOT NULL REFERENCES loyal_yield.route_policies(id),
    observed_slot BIGINT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    chain_slot BIGINT,
    lock_attempt_id BIGINT,
    is_current BOOLEAN NOT NULL DEFAULT TRUE,
    context JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS vault_position_snapshots_latest_idx
    ON loyal_yield.vault_position_snapshots (vault_id, observed_slot DESC, observed_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS vault_position_snapshots_one_current_idx
    ON loyal_yield.vault_position_snapshots (vault_id)
    WHERE is_current;

CREATE TABLE IF NOT EXISTS loyal_yield.vault_position_snapshot_positions (
    id BIGSERIAL PRIMARY KEY,
    snapshot_id BIGINT NOT NULL REFERENCES loyal_yield.vault_position_snapshots(id) ON DELETE CASCADE,
    reserve TEXT NOT NULL,
    market TEXT,
    liquidity_mint TEXT NOT NULL,
    amount_raw BIGINT NOT NULL,
    supply_apy_bps BIGINT,
    borrow_apy_bps BIGINT,
    has_value BOOLEAN NOT NULL,
    planning_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (snapshot_id, reserve)
);

CREATE INDEX IF NOT EXISTS vault_position_snapshot_positions_value_idx
    ON loyal_yield.vault_position_snapshot_positions (snapshot_id, has_value, liquidity_mint);

CREATE TABLE IF NOT EXISTS loyal_yield.vault_reserve_positions_current (
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id) ON DELETE CASCADE,
    reserve TEXT NOT NULL,
    market TEXT,
    liquidity_mint TEXT NOT NULL,
    amount_raw BIGINT NOT NULL,
    has_value BOOLEAN NOT NULL,
    supply_apy_bps BIGINT,
    borrow_apy_bps BIGINT,
    snapshot_id BIGINT NOT NULL REFERENCES loyal_yield.vault_position_snapshots(id),
    observed_slot BIGINT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    planning_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (vault_id, reserve)
);

CREATE TABLE IF NOT EXISTS loyal_yield.rebalance_decisions (
    id BIGSERIAL PRIMARY KEY,
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id),
    source_snapshot_id BIGINT REFERENCES loyal_yield.vault_position_snapshots(id),
    status loyal_yield.decision_status NOT NULL DEFAULT 'planned',
    source_reserve TEXT,
    target_reserve TEXT,
    liquidity_mint TEXT,
    amount_raw BIGINT,
    source_apy_bps BIGINT,
    target_apy_bps BIGINT,
    estimated_edge_bps BIGINT,
    estimated_cost_lamports BIGINT NOT NULL DEFAULT 0,
    decision_reason loyal_yield.decision_reason NOT NULL,
    abandon_reason TEXT,
    idempotency_key TEXT NOT NULL,
    signature TEXT,
    submitted_slot BIGINT,
    confirmed_slot BIGINT,
    preflight_chain_slot BIGINT,
    post_snapshot_id BIGINT REFERENCES loyal_yield.vault_position_snapshots(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (idempotency_key)
);

ALTER TABLE loyal_yield.rebalance_decisions
    ALTER COLUMN source_snapshot_id DROP NOT NULL,
    ALTER COLUMN source_reserve DROP NOT NULL,
    ALTER COLUMN target_reserve DROP NOT NULL,
    ALTER COLUMN liquidity_mint DROP NOT NULL,
    ALTER COLUMN amount_raw DROP NOT NULL;

CREATE INDEX IF NOT EXISTS rebalance_decisions_vault_status_idx
    ON loyal_yield.rebalance_decisions (vault_id, status, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS rebalance_decisions_one_active_per_vault_idx
    ON loyal_yield.rebalance_decisions (vault_id)
    WHERE status IN ('planned', 'simulating', 'ready', 'submitted', 'confirming');
