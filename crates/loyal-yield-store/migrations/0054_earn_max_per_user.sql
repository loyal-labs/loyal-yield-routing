ALTER TABLE loyal_yield.multiply_route_states
    ADD COLUMN IF NOT EXISTS settings TEXT,
    ADD COLUMN IF NOT EXISTS vault_index SMALLINT,
    ADD COLUMN IF NOT EXISTS vault TEXT;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'loyal_yield'
          AND table_name = 'multiply_route_states'
          AND column_name = 'vault_id'
    ) THEN
        UPDATE loyal_yield.multiply_route_states route
        SET
            settings = managed.settings,
            vault_index = managed.vault_index,
            vault = managed.vault_pubkey
        FROM loyal_yield.managed_vaults managed
        WHERE managed.id = route.vault_id;
    END IF;
END $$;

ALTER TABLE loyal_yield.multiply_route_states
    ALTER COLUMN settings SET NOT NULL,
    ALTER COLUMN vault_index SET NOT NULL,
    ALTER COLUMN vault SET NOT NULL;

DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    FOR constraint_name IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.multiply_route_states'::regclass
          AND (
            confrelid = 'loyal_yield.managed_vaults'::regclass
            OR conname IN (
                'multiply_route_states_vault_id_key',
                'multiply_route_states_vault_identity',
                'multiply_route_states_schema_v4'
            )
          )
    LOOP
        EXECUTE format(
            'ALTER TABLE loyal_yield.multiply_route_states DROP CONSTRAINT %I',
            constraint_name
        );
    END LOOP;
END $$;

UPDATE loyal_yield.multiply_route_states
SET
    state_version = state_version + 1,
    state = (state - 'vaultId') || jsonb_build_object(
        'schemaVersion', 5,
        'engineVersion', 'earn_max_v1',
        'settings', settings,
        'vaultIndex', vault_index,
        'vault', vault,
        'generation', state_version + 1
    ),
    updated_at = now()
WHERE (state ->> 'schemaVersion') IS DISTINCT FROM '5'
   OR (state ->> 'engineVersion') IS DISTINCT FROM 'earn_max_v1'
   OR (state ->> 'settings') IS DISTINCT FROM settings
   OR (state ->> 'vaultIndex') IS DISTINCT FROM vault_index::TEXT
   OR (state ->> 'vault') IS DISTINCT FROM vault;

ALTER TABLE loyal_yield.multiply_route_states
    DROP COLUMN IF EXISTS vault_id;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'loyal_yield.multiply_route_states'::regclass
          AND conname = 'multiply_route_states_settings_vault_unique'
    ) THEN
        ALTER TABLE loyal_yield.multiply_route_states
            ADD CONSTRAINT multiply_route_states_settings_vault_unique UNIQUE (settings, vault_index);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'loyal_yield.multiply_route_states'::regclass
          AND conname = 'multiply_route_states_vault_unique'
    ) THEN
        ALTER TABLE loyal_yield.multiply_route_states
            ADD CONSTRAINT multiply_route_states_vault_unique UNIQUE (vault);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'loyal_yield.multiply_route_states'::regclass
          AND conname = 'multiply_route_states_vault_index_range'
    ) THEN
        ALTER TABLE loyal_yield.multiply_route_states
            ADD CONSTRAINT multiply_route_states_vault_index_range CHECK (vault_index BETWEEN 0 AND 255);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'loyal_yield.multiply_route_states'::regclass
          AND conname = 'multiply_route_states_schema_v5'
    ) THEN
        ALTER TABLE loyal_yield.multiply_route_states
            ADD CONSTRAINT multiply_route_states_schema_v5 CHECK ((state ->> 'schemaVersion')::INTEGER = 5);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'loyal_yield.multiply_route_states'::regclass
          AND conname = 'multiply_route_states_settings_identity'
    ) THEN
        ALTER TABLE loyal_yield.multiply_route_states
            ADD CONSTRAINT multiply_route_states_settings_identity CHECK (state ->> 'settings' = settings);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'loyal_yield.multiply_route_states'::regclass
          AND conname = 'multiply_route_states_vault_index_identity'
    ) THEN
        ALTER TABLE loyal_yield.multiply_route_states
            ADD CONSTRAINT multiply_route_states_vault_index_identity CHECK ((state ->> 'vaultIndex')::SMALLINT = vault_index);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'loyal_yield.multiply_route_states'::regclass
          AND conname = 'multiply_route_states_vault_identity'
    ) THEN
        ALTER TABLE loyal_yield.multiply_route_states
            ADD CONSTRAINT multiply_route_states_vault_identity CHECK (state ->> 'vault' = vault);
    END IF;
END $$;

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_engine_version_check;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_engine_version_check CHECK (
        engine_version IN ('canary_migrated', 'linus_v1', 'earn_max_v1')
    );

CREATE TABLE IF NOT EXISTS loyal_yield.earn_max_policy_sets (
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL CHECK (vault_index BETWEEN 0 AND 255),
    vault TEXT NOT NULL,
    manifest_version TEXT NOT NULL,
    manifest_sha256 TEXT NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    status TEXT NOT NULL CHECK (status IN ('incomplete', 'ready', 'removed')),
    policy_accounts JSONB NOT NULL CHECK (jsonb_typeof(policy_accounts) = 'array'),
    observed_signature TEXT NOT NULL,
    observed_slot BIGINT NOT NULL CHECK (observed_slot > 0),
    observed_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (settings, vault_index),
    UNIQUE (vault)
);

CREATE TABLE IF NOT EXISTS loyal_yield.multiply_position_snapshots (
    id BIGSERIAL PRIMARY KEY,
    route_key TEXT NOT NULL REFERENCES loyal_yield.multiply_route_states(route_key) ON DELETE CASCADE,
    generation BIGINT NOT NULL CHECK (generation > 0),
    observed_slot BIGINT NOT NULL CHECK (observed_slot > 0),
    observed_at TIMESTAMPTZ NOT NULL,
    strategy_key TEXT,
    claim_raw NUMERIC(78, 0) NOT NULL CHECK (claim_raw >= 0),
    collateral_raw NUMERIC(78, 0) NOT NULL CHECK (collateral_raw >= 0),
    debt_raw NUMERIC(78, 0) NOT NULL CHECK (debt_raw >= 0),
    equity_usd_micros NUMERIC(78, 0),
    collateral_value_usd_micros NUMERIC(78, 0),
    debt_value_usd_micros NUMERIC(78, 0),
    leverage_bps BIGINT,
    ltv_bps BIGINT,
    health_factor_ppm BIGINT,
    supply_apy_bps BIGINT,
    borrow_apy_bps BIGINT,
    forecast_apy_bps BIGINT,
    valuation_source TEXT,
    valuation_slot BIGINT,
    valuation_observed_at TIMESTAMPTZ,
    coverage_start_at TIMESTAMPTZ,
    UNIQUE (route_key, generation)
);

CREATE INDEX IF NOT EXISTS multiply_position_snapshots_route_observed_idx
    ON loyal_yield.multiply_position_snapshots (route_key, observed_at, id);
