-- Isolated lease-only projection of deployed migrations 0051 and 0053.
-- managed_vaults is only a fixture FK anchor, not its production schema.
CREATE SCHEMA loyal_yield;
CREATE TABLE loyal_yield.managed_vaults (id BIGINT PRIMARY KEY);
CREATE TABLE loyal_yield.multiply_route_states (
    route_key TEXT PRIMARY KEY,
    vault_id BIGINT NOT NULL UNIQUE REFERENCES loyal_yield.managed_vaults(id),
    state JSONB NOT NULL,
    state_version BIGINT NOT NULL DEFAULT 1,
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT multiply_route_states_state_version_positive CHECK (state_version > 0),
    CONSTRAINT multiply_route_states_fencing_token_nonnegative CHECK (fencing_token >= 0),
    CONSTRAINT multiply_route_states_state_object CHECK (jsonb_typeof(state) = 'object'),
    CONSTRAINT multiply_route_states_lease_coherent CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);
INSERT INTO loyal_yield.managed_vaults VALUES (1);
INSERT INTO loyal_yield.multiply_route_states (route_key,vault_id,state)
VALUES ('rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh',1,'{}');

-- Handoff-only projection, not a replay of migrations 1..71+.
CREATE TABLE loyal_yield.multiply_operations (
 operation_id text PRIMARY KEY, route_key text NOT NULL REFERENCES loyal_yield.multiply_route_states,
 cycle bigint NOT NULL CHECK(cycle>0), engine_version text NOT NULL CHECK(engine_version='backyard_rwa_v1'),
 action text NOT NULL, status text NOT NULL CHECK(status IN ('decided','signed','broadcast_intent','submitted','confirmed','reconciling','reconciled','failed','manual_recovery')),
 idempotency_key text NOT NULL UNIQUE, strategy_key text, expected_effects jsonb NOT NULL CHECK(jsonb_typeof(expected_effects)='object'),
 signed_wire bytea, signed_wire_sha256 text CHECK(signed_wire_sha256 IS NULL OR signed_wire_sha256 ~ '^[0-9a-f]{64}$'),
 transaction_signature text, recent_blockhash text, last_valid_block_height bigint CHECK(last_valid_block_height>0),
 broadcast_intent_at timestamptz, confirmed_slot bigint, confirmation_status text, submitted_at timestamptz,
 recovery_reason text, created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK(status NOT IN ('signed','broadcast_intent','submitted') OR (signed_wire IS NOT NULL AND signed_wire_sha256 IS NOT NULL AND transaction_signature IS NOT NULL AND recent_blockhash IS NOT NULL AND last_valid_block_height IS NOT NULL)),
 CHECK(status NOT IN ('broadcast_intent','submitted') OR broadcast_intent_at IS NOT NULL),
 CHECK(status <> 'submitted' OR submitted_at IS NOT NULL)
);
CREATE UNIQUE INDEX multiply_operations_one_nonterminal_per_route ON loyal_yield.multiply_operations(route_key) WHERE status IN ('decided','signed','broadcast_intent','submitted','confirmed','reconciling');
CREATE UNIQUE INDEX multiply_operations_transaction_signature_unique ON loyal_yield.multiply_operations(transaction_signature) WHERE transaction_signature IS NOT NULL;
