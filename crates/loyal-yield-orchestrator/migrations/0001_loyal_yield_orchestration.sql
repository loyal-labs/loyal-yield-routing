CREATE SCHEMA IF NOT EXISTS loyal_yield;

CREATE TABLE IF NOT EXISTS loyal_yield.route_policies (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    settings TEXT NOT NULL,
    authority TEXT NOT NULL,
    policy_seed BIGINT NOT NULL,
    policy_account TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    delegated_signers JSONB NOT NULL DEFAULT '[]'::jsonb,
    threshold INTEGER NOT NULL,
    route_modes JSONB NOT NULL DEFAULT '[]'::jsonb,
    stable_mints JSONB NOT NULL DEFAULT '[]'::jsonb,
    kamino_markets JSONB NOT NULL DEFAULT '[]'::jsonb,
    kamino_liquidity_mints JSONB NOT NULL DEFAULT '[]'::jsonb,
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
    amount_raw TEXT NOT NULL,
    supply_apy_bps BIGINT,
    borrow_apy_bps BIGINT,
    has_value BOOLEAN NOT NULL,
    planning_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (snapshot_id, reserve)
);

CREATE INDEX IF NOT EXISTS vault_position_snapshot_positions_value_idx
    ON loyal_yield.vault_position_snapshot_positions (snapshot_id, has_value, liquidity_mint);

CREATE TABLE IF NOT EXISTS loyal_yield.rebalance_attempts (
    id BIGSERIAL PRIMARY KEY,
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id),
    source_snapshot_id BIGINT NOT NULL REFERENCES loyal_yield.vault_position_snapshots(id),
    status TEXT NOT NULL,
    source_reserve TEXT NOT NULL,
    target_reserve TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    amount_raw TEXT NOT NULL,
    source_apy_bps BIGINT,
    target_apy_bps BIGINT,
    estimated_edge_bps BIGINT,
    estimated_cost_lamports BIGINT,
    decision_reason TEXT NOT NULL,
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

CREATE INDEX IF NOT EXISTS rebalance_attempts_vault_status_idx
    ON loyal_yield.rebalance_attempts (vault_id, status, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS rebalance_attempts_one_active_per_vault_idx
    ON loyal_yield.rebalance_attempts (vault_id)
    WHERE status IN ('planned', 'simulating', 'ready', 'submitted', 'confirming');

CREATE TABLE IF NOT EXISTS loyal_yield.rebalance_events (
    id BIGSERIAL PRIMARY KEY,
    attempt_id BIGINT REFERENCES loyal_yield.rebalance_attempts(id),
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id),
    event_type TEXT NOT NULL,
    from_status TEXT,
    to_status TEXT,
    reason TEXT,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS rebalance_events_attempt_idx
    ON loyal_yield.rebalance_events (attempt_id, created_at);
