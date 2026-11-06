CREATE TABLE loyal_yield.earn_max_withdrawal_consents (
    consent_id TEXT PRIMARY KEY CHECK (consent_id ~ '^[0-9a-f]{64}$'),
    authority TEXT NOT NULL,
    vault TEXT NOT NULL,
    request_signature TEXT NOT NULL,
    request_instruction INTEGER NOT NULL CHECK (request_instruction >= 0),
    version BIGINT NOT NULL CHECK (version > 0),
    owner TEXT NOT NULL,
    nonce TEXT NOT NULL UNIQUE CHECK (nonce ~ '^[0-9a-f]{64}$'),
    expires_at TIMESTAMPTZ NOT NULL,
    canonical_message BYTEA NOT NULL CHECK (octet_length(canonical_message) BETWEEN 1 AND 8192),
    owner_signature BYTEA NOT NULL CHECK (octet_length(owner_signature) = 64),
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (authority, request_signature, request_instruction, version)
);

CREATE INDEX earn_max_withdrawal_consents_vault_authority
    ON loyal_yield.earn_max_withdrawal_consents (vault, authority, expires_at);
