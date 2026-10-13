-- One scheduled slot per Autodeposit target. Lots were filed onto "the
-- oldest scheduled slot" while every drain, claim and release left or
-- inserted another, so production held 625,297 scheduled slots without an
-- open lot and targets with several scheduled slots. A requested slot is one
-- being executed now ("Deposit now"), not a second place for lots: it is left
-- as it is, its due date included.
SET LOCAL lock_timeout = '5s';

-- These rows are bookkeeping, not progress. The slot realtime trigger would
-- publish a "canceled" autodeposit event for each of them, and the wakeup
-- trigger runs for every status change (it notifies only on 'requested',
-- which nothing here writes). Both are disabled for this transaction only and
-- enabled again before commit, so no other session ever writes a slot
-- without them.
ALTER TABLE loyal_yield.balance_sweep_scheduled_slots
    DISABLE TRIGGER balance_sweep_scheduled_slots_realtime_event,
    DISABLE TRIGGER balance_sweep_scheduled_slots_autodeposit_wakeup;

-- When idle vault custody first blocked the slot's pull; cleared when a claim
-- selects the slot. A drained slot is never reused, so the time never carries
-- over to new lots. Replaces the "[idle_blocked_since=...]" text in last_error.
ALTER TABLE loyal_yield.balance_sweep_scheduled_slots
    ADD COLUMN idle_blocked_since TIMESTAMPTZ;

CREATE TEMP TABLE slot_open_lots ON COMMIT DROP AS
SELECT DISTINCT scheduled_slot_id AS slot_id
FROM loyal_yield.balance_sweep_surplus_lots
WHERE status = 'open' AND remaining_amount_raw > 0 AND scheduled_slot_id IS NOT NULL;
CREATE UNIQUE INDEX ON slot_open_lots (slot_id);
ANALYZE slot_open_lots;

-- 1. Scheduled slots with no open lot, no attempt and no execution held
--    nothing to deposit.
UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
SET status = 'canceled', last_error = 'no open lot', updated_at = now()
WHERE slot.status = 'scheduled'
  AND slot.execution_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM slot_open_lots AS open WHERE open.slot_id = slot.id)
  AND NOT EXISTS (
      SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
      WHERE attempt.scheduled_slot_id = slot.id)
  AND NOT EXISTS (
      SELECT 1 FROM loyal_yield.balance_sweep_executions AS execution
      WHERE execution.scheduled_slot_id = slot.id);

-- 2. A target left with several scheduled slots keeps its oldest. The others'
--    open lots move onto it and they are canceled; the kept slot is due when
--    the latest of them was (a batch due date never moves earlier).
CREATE TEMP TABLE merged_scheduled_slots ON COMMIT DROP AS
SELECT slot.id, slot.eligible_after,
       min(slot.id) OVER (PARTITION BY slot.target_id) AS kept_id
FROM loyal_yield.balance_sweep_scheduled_slots AS slot
WHERE slot.status = 'scheduled'
  AND slot.target_id IN (
      SELECT target_id FROM loyal_yield.balance_sweep_scheduled_slots
      WHERE status = 'scheduled'
      GROUP BY target_id HAVING count(*) > 1);

UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
SET scheduled_slot_id = merged.kept_id, updated_at = now()
FROM merged_scheduled_slots AS merged
WHERE lot.scheduled_slot_id = merged.id
  AND merged.id <> merged.kept_id
  AND lot.status = 'open';

UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
SET status = 'canceled', last_error = 'merged into slot ' || merged.kept_id, updated_at = now()
FROM merged_scheduled_slots AS merged
WHERE slot.id = merged.id AND merged.id <> merged.kept_id;

UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
SET eligible_after = due.eligible_after, updated_at = now()
FROM (SELECT kept_id, max(eligible_after) AS eligible_after FROM merged_scheduled_slots GROUP BY kept_id) AS due
WHERE slot.id = due.kept_id AND slot.eligible_after < due.eligible_after;

-- 3. Open lots stranded on a failed or released slot (2 in production, no
--    attempt, execution or claim behind them) are still owed: they move onto
--    their target's scheduled slot, created where the target has none. The
--    schedule-repair scan that requeued them is deleted.
CREATE TEMP TABLE stranded_lot_targets ON COMMIT DROP AS
SELECT lot.target_id, max(lot.eligible_after) AS eligible_after
FROM loyal_yield.balance_sweep_surplus_lots AS lot
JOIN loyal_yield.balance_sweep_scheduled_slots AS slot ON slot.id = lot.scheduled_slot_id
WHERE slot.status IN ('failed', 'released')
  AND lot.status = 'open' AND lot.remaining_amount_raw > 0
GROUP BY lot.target_id;

INSERT INTO loyal_yield.balance_sweep_scheduled_slots (target_id, token_mint, eligible_after, status)
SELECT stranded.target_id, target.token_mint, stranded.eligible_after, 'scheduled'
FROM stranded_lot_targets AS stranded
JOIN loyal_yield.balance_sweep_targets AS target ON target.id = stranded.target_id
WHERE NOT EXISTS (
    SELECT 1 FROM loyal_yield.balance_sweep_scheduled_slots AS slot
    WHERE slot.target_id = stranded.target_id AND slot.status = 'scheduled');

UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
SET scheduled_slot_id = scheduled.id, updated_at = now()
FROM loyal_yield.balance_sweep_scheduled_slots AS stranded_slot,
     loyal_yield.balance_sweep_scheduled_slots AS scheduled
WHERE stranded_slot.id = lot.scheduled_slot_id
  AND stranded_slot.status IN ('failed', 'released')
  AND lot.status = 'open' AND lot.remaining_amount_raw > 0
  AND scheduled.target_id = lot.target_id
  AND scheduled.status = 'scheduled';

-- 4. The idle block recorded in last_error becomes the column.
UPDATE loyal_yield.balance_sweep_scheduled_slots
SET idle_blocked_since = to_timestamp(substring(last_error FROM 'idle_blocked_since=([0-9]{1,12})')::bigint)
WHERE status IN ('scheduled', 'requested')
  AND last_error ~ '^existing idle vault balance must drain before direct autodeposit: [0-9]+ \[idle_blocked_since=[0-9]{1,12}; idle_deferrals=[0-9]{1,7}\]$';

CREATE UNIQUE INDEX balance_sweep_scheduled_slots_one_scheduled_uidx
    ON loyal_yield.balance_sweep_scheduled_slots (target_id)
    WHERE status = 'scheduled';

ALTER TABLE loyal_yield.balance_sweep_scheduled_slots
    ENABLE TRIGGER balance_sweep_scheduled_slots_realtime_event,
    ENABLE TRIGGER balance_sweep_scheduled_slots_autodeposit_wakeup;
