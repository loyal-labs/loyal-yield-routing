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
