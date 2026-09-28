-- A route latch generation fences a stale worker tick from re-arming a hold
-- after an operator has committed HOLD_CLEARED. The clear and its journal fact
-- advance this value in one transaction; new holds preserve the current value.
ALTER TABLE loyal_yield.backyard_manual_recovery_latches
    ADD COLUMN IF NOT EXISTS generation BIGINT NOT NULL DEFAULT 0;

ALTER TABLE loyal_yield.backyard_manual_recovery_latches
    DROP CONSTRAINT IF EXISTS backyard_manual_recovery_latches_generation_check;

ALTER TABLE loyal_yield.backyard_manual_recovery_latches
    ADD CONSTRAINT backyard_manual_recovery_latches_generation_check
    CHECK (generation >= 0);
