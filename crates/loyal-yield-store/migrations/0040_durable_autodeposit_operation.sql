CREATE TABLE IF NOT EXISTS loyal_yield.balance_sweep_autodeposit_operations (
    claim_token TEXT PRIMARY KEY
        REFERENCES loyal_yield.balance_sweep_lot_claims(claim_token) ON DELETE RESTRICT,
    target_id BIGINT NOT NULL
        REFERENCES loyal_yield.balance_sweep_targets(id) ON DELETE RESTRICT,
    scheduled_slot_id BIGINT NOT NULL
        REFERENCES loyal_yield.balance_sweep_scheduled_slots(id) ON DELETE RESTRICT,
    state TEXT NOT NULL DEFAULT 'prepared',
    amount_raw BIGINT NOT NULL,
    managed_vault_id BIGINT NOT NULL
        REFERENCES loyal_yield.managed_vaults(id) ON DELETE RESTRICT,
    settings TEXT NOT NULL,
    vault_index INTEGER NOT NULL,
    wallet TEXT NOT NULL,
    wallet_token_ata TEXT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    vault_token_ata TEXT NOT NULL,
    token_mint TEXT NOT NULL,
    reserve TEXT NOT NULL,
    market TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    route_policy_account TEXT NOT NULL,
    route_policy_seed BIGINT NOT NULL,
    preflight_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    pull_signature TEXT,
    pull_confirmed_slot BIGINT,
    execution_id BIGINT
        REFERENCES loyal_yield.balance_sweep_executions(id) ON DELETE RESTRICT,
    deposit_source_balance_raw BIGINT,
    deposit_lease_token TEXT,
    deposit_lease_expires_at TIMESTAMPTZ,
    deposit_signature TEXT,
    deposit_confirmed_slot BIGINT,
    error_detail TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT balance_sweep_autodeposit_operations_state_check CHECK (
        state IN (
            'prepared', 'pull_confirmed', 'deposit_pending',
            'completed', 'ambiguous'
        )
    ),
    CONSTRAINT balance_sweep_autodeposit_operations_identity_check CHECK (
        amount_raw > 0
        AND vault_index >= 0
        AND NULLIF(btrim(settings), '') IS NOT NULL
        AND NULLIF(btrim(wallet), '') IS NOT NULL
        AND NULLIF(btrim(wallet_token_ata), '') IS NOT NULL
        AND NULLIF(btrim(vault_pubkey), '') IS NOT NULL
        AND NULLIF(btrim(vault_token_ata), '') IS NOT NULL
        AND NULLIF(btrim(token_mint), '') IS NOT NULL
        AND NULLIF(btrim(reserve), '') IS NOT NULL
        AND NULLIF(btrim(market), '') IS NOT NULL
        AND NULLIF(btrim(liquidity_mint), '') IS NOT NULL
        AND NULLIF(btrim(route_policy_account), '') IS NOT NULL
    ),
    CONSTRAINT balance_sweep_autodeposit_operations_progress_check CHECK (
        (pull_signature IS NULL) = (pull_confirmed_slot IS NULL)
        AND (deposit_signature IS NULL) = (deposit_confirmed_slot IS NULL)
        AND (deposit_lease_token IS NULL) = (deposit_lease_expires_at IS NULL)
        AND (deposit_source_balance_raw IS NULL OR deposit_source_balance_raw >= amount_raw)
        AND (state = 'prepared' OR pull_signature IS NOT NULL)
        AND (state <> 'completed' OR (
            execution_id IS NOT NULL
            AND pull_signature IS NOT NULL
            AND deposit_signature IS NOT NULL
        ))
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS balance_sweep_autodeposit_operations_slot_uidx
    ON loyal_yield.balance_sweep_autodeposit_operations (scheduled_slot_id);

CREATE INDEX IF NOT EXISTS balance_sweep_autodeposit_operations_recovery_idx
    ON loyal_yield.balance_sweep_autodeposit_operations (state, updated_at, claim_token)
    WHERE state IN ('pull_confirmed', 'deposit_pending', 'ambiguous');

CREATE OR REPLACE FUNCTION loyal_yield.guard_balance_sweep_autodeposit_operation_identity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.claim_token IS DISTINCT FROM OLD.claim_token
       OR NEW.target_id IS DISTINCT FROM OLD.target_id
       OR NEW.scheduled_slot_id IS DISTINCT FROM OLD.scheduled_slot_id
       OR NEW.amount_raw IS DISTINCT FROM OLD.amount_raw
       OR NEW.managed_vault_id IS DISTINCT FROM OLD.managed_vault_id
       OR NEW.settings IS DISTINCT FROM OLD.settings
       OR NEW.vault_index IS DISTINCT FROM OLD.vault_index
       OR NEW.wallet IS DISTINCT FROM OLD.wallet
       OR NEW.wallet_token_ata IS DISTINCT FROM OLD.wallet_token_ata
       OR NEW.vault_pubkey IS DISTINCT FROM OLD.vault_pubkey
       OR NEW.vault_token_ata IS DISTINCT FROM OLD.vault_token_ata
       OR NEW.token_mint IS DISTINCT FROM OLD.token_mint
       OR NEW.reserve IS DISTINCT FROM OLD.reserve
       OR NEW.market IS DISTINCT FROM OLD.market
       OR NEW.liquidity_mint IS DISTINCT FROM OLD.liquidity_mint
       OR NEW.route_policy_account IS DISTINCT FROM OLD.route_policy_account
       OR NEW.route_policy_seed IS DISTINCT FROM OLD.route_policy_seed
       OR NEW.preflight_evidence IS DISTINCT FROM OLD.preflight_evidence THEN
        RAISE EXCEPTION 'durable autodeposit operation identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS guard_balance_sweep_autodeposit_operation_identity
    ON loyal_yield.balance_sweep_autodeposit_operations;
CREATE TRIGGER guard_balance_sweep_autodeposit_operation_identity
BEFORE UPDATE ON loyal_yield.balance_sweep_autodeposit_operations
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_balance_sweep_autodeposit_operation_identity();

COMMENT ON TABLE loyal_yield.balance_sweep_autodeposit_operations IS
    'One durable owner for the autodeposit pull and mandatory Kamino deposit transactions.';
