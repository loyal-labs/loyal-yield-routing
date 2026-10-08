CREATE SCHEMA IF NOT EXISTS loyal_yield;

CREATE TABLE IF NOT EXISTS loyal_yield.schema_migrations (
    version BIGINT PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS loyal_yield.projection_offsets (
    consumer_name TEXT PRIMARY KEY,
    last_event_id BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

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
    UNIQUE (policy_account)
);

CREATE SEQUENCE IF NOT EXISTS loyal_yield.route_policies_id_seq AS BIGINT;

ALTER SEQUENCE loyal_yield.route_policies_id_seq
    OWNED BY loyal_yield.route_policies.id;

ALTER TABLE loyal_yield.route_policies
    ALTER COLUMN id SET DEFAULT nextval('loyal_yield.route_policies_id_seq'::regclass);

SELECT setval(
    'loyal_yield.route_policies_id_seq'::regclass,
    COALESCE((SELECT MAX(id) FROM loyal_yield.route_policies), 1),
    (SELECT MAX(id) IS NOT NULL FROM loyal_yield.route_policies)
);

CREATE INDEX IF NOT EXISTS route_policies_active_idx
    ON loyal_yield.route_policies (active, settings, vault_index);

CREATE TABLE IF NOT EXISTS loyal_yield.managed_vaults (
    id BIGSERIAL PRIMARY KEY,
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    active_policy_id BIGINT NOT NULL REFERENCES loyal_yield.route_policies(id),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (settings, vault_index, vault_pubkey)
);

CREATE INDEX IF NOT EXISTS managed_vaults_active_idx
    ON loyal_yield.managed_vaults (active, active_policy_id);

CREATE TABLE IF NOT EXISTS loyal_yield.balance_sweep_targets (
    id BIGSERIAL PRIMARY KEY,
    settings TEXT NOT NULL,
    authority TEXT NOT NULL,
    policy_seed BIGINT NOT NULL,
    policy_account TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    wallet TEXT NOT NULL,
    wallet_usdc_ata TEXT NOT NULL,
    vault_usdc_ata TEXT NOT NULL,
    delegated_signers TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    threshold INTEGER NOT NULL,
    max_amount_per_period BIGINT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_slot BIGINT NOT NULL,
    last_seen_signature TEXT NOT NULL,
    UNIQUE (policy_account)
);

CREATE INDEX IF NOT EXISTS balance_sweep_targets_active_wallet_ata_idx
    ON loyal_yield.balance_sweep_targets (active, wallet_usdc_ata);

CREATE INDEX IF NOT EXISTS balance_sweep_targets_wallet_idx
    ON loyal_yield.balance_sweep_targets (wallet, active);

CREATE TABLE IF NOT EXISTS loyal_yield.balance_sweep_wallet_balances_current (
    target_id BIGINT PRIMARY KEY REFERENCES loyal_yield.balance_sweep_targets(id) ON DELETE CASCADE,
    wallet TEXT NOT NULL,
    wallet_usdc_ata TEXT NOT NULL,
    amount_raw BIGINT NOT NULL,
    owner TEXT,
    mint TEXT NOT NULL,
    observed_slot BIGINT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    source TEXT NOT NULL,
    source_commitment TEXT NOT NULL,
    account_data_hash TEXT,
    raw_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS balance_sweep_wallet_balances_wallet_idx
    ON loyal_yield.balance_sweep_wallet_balances_current (wallet, updated_at DESC);

CREATE TABLE IF NOT EXISTS loyal_yield.balance_sweep_executions (
    id BIGSERIAL PRIMARY KEY,
    target_id BIGINT NOT NULL REFERENCES loyal_yield.balance_sweep_targets(id),
    signature TEXT NOT NULL,
    slot BIGINT NOT NULL,
    source_wallet_ata TEXT NOT NULL,
    destination_vault_ata TEXT NOT NULL,
    amount_raw BIGINT NOT NULL,
    source_pre_balance_raw BIGINT,
    source_post_balance_raw BIGINT,
    destination_pre_balance_raw BIGINT,
    destination_post_balance_raw BIGINT,
    source_commitment TEXT NOT NULL,
    raw_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    decoded_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    received_at TIMESTAMPTZ,
    decoded_at TIMESTAMPTZ,
    inserted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    dedupe_key TEXT NOT NULL UNIQUE
);

CREATE INDEX IF NOT EXISTS balance_sweep_executions_target_slot_idx
    ON loyal_yield.balance_sweep_executions (target_id, slot DESC, id DESC);

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
    source_liquidity_mint TEXT,
    target_liquidity_mint TEXT,
    amount_raw BIGINT,
    source_apy_bps BIGINT,
    target_apy_bps BIGINT,
    estimated_edge_bps BIGINT,
    estimated_cost_lamports BIGINT NOT NULL DEFAULT 0,
    decision_reason loyal_yield.decision_reason NOT NULL,
    execution_plan JSONB NOT NULL DEFAULT '{}'::jsonb,
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

ALTER TABLE loyal_yield.rebalance_decisions
    ADD COLUMN IF NOT EXISTS source_liquidity_mint TEXT,
    ADD COLUMN IF NOT EXISTS target_liquidity_mint TEXT,
    ADD COLUMN IF NOT EXISTS execution_plan JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE INDEX IF NOT EXISTS rebalance_decisions_vault_status_idx
    ON loyal_yield.rebalance_decisions (vault_id, status, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS rebalance_decisions_one_active_per_vault_idx
    ON loyal_yield.rebalance_decisions (vault_id)
    WHERE status IN ('planned', 'simulating', 'ready', 'submitted', 'confirming');
