ALTER TYPE loyal_yield.decision_reason ADD VALUE IF NOT EXISTS 'idle_vault_liquidity_available';

CREATE TABLE IF NOT EXISTS loyal_yield.vault_idle_token_balances_current (
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id),
    mint TEXT NOT NULL,
    amount_raw BIGINT NOT NULL,
    owner TEXT NOT NULL,
    token_account TEXT NOT NULL,
    observed_slot BIGINT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    source_commitment TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (vault_id, mint)
);

ALTER TABLE loyal_yield.vault_idle_token_balances_current
    ADD COLUMN IF NOT EXISTS vault_id BIGINT,
    ADD COLUMN IF NOT EXISTS mint TEXT,
    ADD COLUMN IF NOT EXISTS amount_raw BIGINT,
    ADD COLUMN IF NOT EXISTS owner TEXT,
    ADD COLUMN IF NOT EXISTS token_account TEXT,
    ADD COLUMN IF NOT EXISTS observed_slot BIGINT,
    ADD COLUMN IF NOT EXISTS observed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS source_commitment TEXT,
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;

ALTER TABLE loyal_yield.vault_idle_token_balances_current
    ALTER COLUMN vault_id SET NOT NULL,
    ALTER COLUMN mint SET NOT NULL,
    ALTER COLUMN amount_raw SET NOT NULL,
    ALTER COLUMN owner SET NOT NULL,
    ALTER COLUMN token_account SET NOT NULL,
    ALTER COLUMN observed_slot SET NOT NULL,
    ALTER COLUMN observed_at SET NOT NULL,
    ALTER COLUMN source_commitment SET NOT NULL,
    ALTER COLUMN updated_at SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'vault_idle_token_balances_current_pkey'
          AND conrelid = 'loyal_yield.vault_idle_token_balances_current'::regclass
    ) THEN
        ALTER TABLE loyal_yield.vault_idle_token_balances_current
            ADD CONSTRAINT vault_idle_token_balances_current_pkey PRIMARY KEY (vault_id, mint);
    END IF;
END $$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'vault_idle_token_balances_current_vault_id_fkey'
          AND conrelid = 'loyal_yield.vault_idle_token_balances_current'::regclass
    ) THEN
        ALTER TABLE loyal_yield.vault_idle_token_balances_current
            ADD CONSTRAINT vault_idle_token_balances_current_vault_id_fkey
            FOREIGN KEY (vault_id) REFERENCES loyal_yield.managed_vaults(id);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS vault_idle_token_balances_current_mint_idx
    ON loyal_yield.vault_idle_token_balances_current (mint);

ALTER TABLE loyal_yield.rebalance_decisions
    ALTER COLUMN source_reserve DROP NOT NULL;
