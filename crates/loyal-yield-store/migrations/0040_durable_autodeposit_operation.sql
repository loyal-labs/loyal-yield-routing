ALTER TABLE loyal_yield.balance_sweep_lot_claims
    ADD COLUMN IF NOT EXISTS autodeposit_executor_lease_token TEXT,
    ADD COLUMN IF NOT EXISTS autodeposit_executor_lease_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS autodeposit_deposit_plan JSONB;

ALTER TABLE loyal_yield.balance_sweep_lot_claims
    DROP CONSTRAINT IF EXISTS balance_sweep_lot_claims_autodeposit_lease_check;
ALTER TABLE loyal_yield.balance_sweep_lot_claims
    ADD CONSTRAINT balance_sweep_lot_claims_autodeposit_lease_check CHECK (
        (autodeposit_executor_lease_token IS NULL) =
            (autodeposit_executor_lease_expires_at IS NULL)
        AND (
            autodeposit_deposit_plan IS NULL
            OR jsonb_typeof(autodeposit_deposit_plan) = 'object'
        )
    );

CREATE INDEX IF NOT EXISTS balance_sweep_lot_claims_autodeposit_recovery_idx
    ON loyal_yield.balance_sweep_lot_claims
        (status, autodeposit_executor_lease_expires_at, updated_at)
    WHERE status = 'selected';

CREATE OR REPLACE FUNCTION loyal_yield.guard_balance_sweep_claim_deposit_plan()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.autodeposit_deposit_plan IS NOT NULL
       AND NEW.autodeposit_deposit_plan IS DISTINCT FROM OLD.autodeposit_deposit_plan THEN
        RAISE EXCEPTION 'autodeposit deposit plan is immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS guard_balance_sweep_claim_deposit_plan
    ON loyal_yield.balance_sweep_lot_claims;
CREATE TRIGGER guard_balance_sweep_claim_deposit_plan
BEFORE UPDATE OF autodeposit_deposit_plan
    ON loyal_yield.balance_sweep_lot_claims
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_balance_sweep_claim_deposit_plan();

COMMENT ON COLUMN loyal_yield.balance_sweep_lot_claims.autodeposit_deposit_plan IS
    'Immutable direct Kamino deposit intent stored before the autodeposit pull is broadcast.';
