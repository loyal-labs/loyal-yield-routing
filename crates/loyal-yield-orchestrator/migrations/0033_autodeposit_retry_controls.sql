-- Autodeposit surplus lots had no notion of how many times they had already
-- failed. A permanently broken target (for example a vault whose Squads route
-- policy was closed on chain while Neon still lists it active) therefore
-- retried on every worker tick forever: the failure path restored the lot with
-- a fixed delay and the residual mover re-scheduled it with an
-- `eligible_after` that was already in the past.
--
-- Track attempts per lot so the failure path can back off and the residual
-- mover can dead-letter a lot instead of rescheduling it forever.

ALTER TABLE loyal_yield.balance_sweep_surplus_lots
    ADD COLUMN IF NOT EXISTS autodeposit_attempt_count INTEGER NOT NULL DEFAULT 0;

ALTER TABLE loyal_yield.balance_sweep_surplus_lots
    ADD CONSTRAINT balance_sweep_surplus_lots_attempt_count_non_negative
    CHECK (autodeposit_attempt_count >= 0)
    NOT VALID;

ALTER TABLE loyal_yield.balance_sweep_surplus_lots
    VALIDATE CONSTRAINT balance_sweep_surplus_lots_attempt_count_non_negative;

-- Suppressed lots are terminal, so the open-lot scans should skip them without
-- walking the dead-lettered backlog of a permanently broken target.
CREATE INDEX IF NOT EXISTS balance_sweep_surplus_lots_open_attempts_idx
    ON loyal_yield.balance_sweep_surplus_lots (target_id, eligible_after)
    WHERE status = 'open';
