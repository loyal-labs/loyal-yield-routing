ALTER TYPE loyal_yield.balance_sweep_surplus_classification
    ADD VALUE IF NOT EXISTS 'initial_surplus';

CREATE OR REPLACE VIEW loyal_yield.pending_balance_sweep_surplus_lots AS
SELECT
    id,
    target_id,
    source_event_id,
    source_signature,
    classification::text AS classification,
    original_amount_raw,
    remaining_amount_raw,
    eligible_after,
    status::text AS status,
    confidence,
    reason,
    created_at,
    updated_at
FROM loyal_yield.balance_sweep_surplus_lots
WHERE status = 'open'
  AND remaining_amount_raw > 0;
