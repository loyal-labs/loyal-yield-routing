CREATE TABLE IF NOT EXISTS loyal_yield.multiply_route_states (
    route_key TEXT PRIMARY KEY,
    vault_id BIGINT NOT NULL UNIQUE REFERENCES loyal_yield.managed_vaults(id),
    state JSONB NOT NULL,
    state_version BIGINT NOT NULL DEFAULT 1 CHECK (state_version > 0),
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    fencing_token BIGINT NOT NULL DEFAULT 0 CHECK (fencing_token >= 0),
    pending_signed_wire BYTEA,
    pending_signed_wire_sha256 TEXT,
    pending_transaction_signature TEXT,
    pending_recent_blockhash TEXT,
    pending_last_valid_block_height BIGINT,
    pending_broadcast_intent_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (jsonb_typeof(state) = 'object'),
    CHECK ((state ->> 'schemaVersion')::INTEGER = 3),
    CHECK (state -> 'metadata' ->> 'routeId' = route_key),
    CHECK ((state -> 'metadata' ->> 'vaultId')::BIGINT = vault_id),
    CHECK ((state -> 'metadata' ->> 'generation')::BIGINT = state_version),
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    CHECK (
        (pending_signed_wire IS NULL
            AND pending_signed_wire_sha256 IS NULL
            AND pending_transaction_signature IS NULL
            AND pending_recent_blockhash IS NULL
            AND pending_last_valid_block_height IS NULL
            AND pending_broadcast_intent_at IS NULL)
        OR
        (pending_signed_wire IS NOT NULL
            AND octet_length(pending_signed_wire) > 0
            AND pending_signed_wire_sha256 ~ '^[0-9a-f]{64}$'
            AND length(pending_transaction_signature) > 0
            AND length(pending_recent_blockhash) > 0
            AND pending_last_valid_block_height > 0)
    )
);

CREATE INDEX IF NOT EXISTS multiply_route_states_runnable_idx
    ON loyal_yield.multiply_route_states (lease_expires_at, updated_at);
