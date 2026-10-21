-- One selected claim per Autodeposit target: a second claimer's insert conflicts here and returns noop target_has_selected_claim.
SET LOCAL lock_timeout = '5s';

LOCK TABLE loyal_yield.balance_sweep_lot_claims IN SHARE MODE;

DO $$
DECLARE
    doubled TEXT;
BEGIN
    SELECT string_agg(target_id::text, ', ' ORDER BY target_id) INTO doubled
    FROM (
        SELECT target_id FROM loyal_yield.balance_sweep_lot_claims
        WHERE status = 'selected'
        GROUP BY target_id HAVING count(*) > 1
    ) AS conflicting;
    IF doubled IS NOT NULL THEN
        RAISE EXCEPTION 'Autodeposit targets hold more than one selected claim: %', doubled;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS balance_sweep_lot_claims_one_selected_uidx
    ON loyal_yield.balance_sweep_lot_claims (target_id)
    WHERE status = 'selected';
