-- Minimal parent shape for withdrawal regressions, not a schema migration.
-- Lifecycle checks are supplied only by the unmodified production 0113 SQL.
CREATE SCHEMA loyal_yield;
CREATE TABLE loyal_yield.multiply_operations (
    operation_id TEXT PRIMARY KEY,
    route_key TEXT,
    status TEXT,
    signed_wire BYTEA,
    recovery_reason TEXT,
    transaction_signature TEXT,
    signed_wire_sha256 TEXT,
    expected_effects JSONB,
    engine_version TEXT DEFAULT 'backyard_rwa_v1',
    action TEXT DEFAULT 'DELEVER_ROUTE_STEP',
    strategy_key TEXT,
    message_sha256 TEXT,
    recent_blockhash TEXT,
    last_valid_block_height BIGINT,
    simulation_slot BIGINT,
    simulation_result JSONB,
    broadcast_intent_at TIMESTAMPTZ,
    confirmed_slot BIGINT,
    confirmation_status TEXT,
    reconciliation_sha256 TEXT,
    reconciled_effects JSONB,
    updated_at TIMESTAMPTZ DEFAULT now()
);
