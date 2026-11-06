CREATE TABLE loyal_yield.vault_release_controls (
    vault TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('optional_addition','ordinary_funding','official_deposit','claim_attempt')),
    state TEXT NOT NULL CHECK (state IN ('open','held')),
    version BIGINT NOT NULL CHECK (version > 0),
    reason TEXT NOT NULL CHECK (reason <> ''),
    operator TEXT NOT NULL CHECK (operator <> ''),
    allowed_owners TEXT[] CHECK (allowed_owners IS NULL OR action <> 'optional_addition'),
    max_amount_raw BIGINT CHECK (max_amount_raw IS NULL OR (max_amount_raw >= 0 AND action <> 'optional_addition')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (vault, action)
);

CREATE TABLE loyal_yield.vault_release_control_history (
    vault TEXT NOT NULL,
    action TEXT NOT NULL,
    state TEXT NOT NULL,
    version BIGINT NOT NULL,
    reason TEXT NOT NULL,
    operator TEXT NOT NULL,
    allowed_owners TEXT[],
    max_amount_raw BIGINT,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (vault, action, version)
);

CREATE FUNCTION loyal_yield.vault_release_control_history_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'vault_release_control_history is append-only';
END $$;

CREATE TRIGGER vault_release_control_history_append_only
    BEFORE UPDATE OR DELETE ON loyal_yield.vault_release_control_history
    FOR EACH ROW EXECUTE FUNCTION loyal_yield.vault_release_control_history_append_only();

ALTER TABLE loyal_yield.multiply_operations
    ADD COLUMN release_control_vault TEXT,
    ADD COLUMN release_control_action TEXT CHECK (release_control_action IN ('optional_addition','ordinary_funding')),
    ADD COLUMN release_control_version BIGINT CHECK (release_control_version > 0),
    ADD CONSTRAINT multiply_operations_release_control_complete CHECK ((release_control_vault IS NULL) = (release_control_action IS NULL) AND (release_control_action IS NULL) = (release_control_version IS NULL));

ALTER TABLE loyal_yield.earn_max_withdrawal_claim_attempts
    ADD COLUMN release_control_vault TEXT,
    ADD COLUMN release_control_version BIGINT CHECK (release_control_version > 0),
    ADD CONSTRAINT earn_max_withdrawal_claim_attempt_release_control CHECK ((release_control_vault IS NULL) = (release_control_version IS NULL));

-- Certified NAV the worker published on chain and last confirmed: the deposit
-- quote compares the on-chain report fields against this row.
CREATE TABLE loyal_yield.backyard_confirmed_nav_publications (
    vault TEXT PRIMARY KEY,
    operation_id TEXT NOT NULL REFERENCES loyal_yield.multiply_operations(operation_id),
    locked_profit_raw NUMERIC(20,0) NOT NULL CHECK (locked_profit_raw >= 0),
    last_report_unix BIGINT NOT NULL CHECK (last_report_unix > 0),
    total_value_raw NUMERIC(20,0) NOT NULL CHECK (total_value_raw >= 0),
    confirmed_slot BIGINT NOT NULL CHECK (confirmed_slot > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Latest worker certificate of full-coverage NAV, valid for 30 s and 32 slots.
CREATE TABLE loyal_yield.backyard_certified_nav (
    vault TEXT PRIMARY KEY,
    manifest_sha256 TEXT NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    total_nav_raw NUMERIC(20,0) NOT NULL CHECK (total_nav_raw >= 0),
    certified_slot BIGINT NOT NULL CHECK (certified_slot > 0),
    max_slot BIGINT NOT NULL CHECK (max_slot >= certified_slot),
    valid_until TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE loyal_yield.earn_max_deposit_quotes (
    certificate_id TEXT PRIMARY KEY CHECK (certificate_id ~ '^[0-9a-f]{64}$'),
    vault TEXT NOT NULL,
    authority TEXT NOT NULL,
    amount_raw NUMERIC(20,0) NOT NULL CHECK (amount_raw > 0),
    lp_min_raw NUMERIC(20,0) NOT NULL,
    lp_max_raw NUMERIC(20,0) NOT NULL CHECK (lp_max_raw >= lp_min_raw),
    certified_slot BIGINT NOT NULL,
    not_after_slot BIGINT NOT NULL CHECK (not_after_slot >= certified_slot),
    release_control_version BIGINT NOT NULL CHECK (release_control_version > 0),
    expires_at TIMESTAMPTZ NOT NULL,
    quote JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX earn_max_deposit_quotes_vault ON loyal_yield.earn_max_deposit_quotes(vault, expires_at);
