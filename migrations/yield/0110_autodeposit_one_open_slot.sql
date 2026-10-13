-- One open (scheduled or requested) slot per Autodeposit target. Lots filed
-- onto "the oldest scheduled slot" while every drain, claim and release left
-- or inserted another, so production held 625,297 scheduled slots without an
-- open lot and 653 targets with more than one open slot, and lots stranded on
-- failed or released slots waited for a repair scan to requeue them.
--
-- These rows are bookkeeping, not progress: the slot realtime trigger would
-- otherwise publish a "canceled" autodeposit event for each of them. It is
-- disabled for this transaction only and enabled again before commit, so no
-- other session ever writes a slot without it.
ALTER TABLE loyal_yield.balance_sweep_scheduled_slots
    DISABLE TRIGGER balance_sweep_scheduled_slots_realtime_event;

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

-- 2. A target left with several open slots keeps one: its requested slot, else
--    its oldest. The others' open lots move onto it and they are canceled. The
--    kept slot is due when the latest of them was (a batch due date never
--    moves earlier) and keeps the oldest idle block.
CREATE TEMP TABLE merged_open_slots ON COMMIT DROP AS
SELECT slot.id, slot.target_id, slot.eligible_after,
       first_value(slot.id) OVER (
           PARTITION BY slot.target_id
           ORDER BY slot.status = 'requested' DESC, slot.id ASC) AS kept_id
FROM loyal_yield.balance_sweep_scheduled_slots AS slot
WHERE slot.status IN ('scheduled', 'requested')
  AND slot.target_id IN (
      SELECT target_id FROM loyal_yield.balance_sweep_scheduled_slots
      WHERE status IN ('scheduled', 'requested')
      GROUP BY target_id HAVING count(*) > 1);

UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
SET scheduled_slot_id = merged.kept_id, updated_at = now()
FROM merged_open_slots AS merged
WHERE lot.scheduled_slot_id = merged.id
  AND merged.id <> merged.kept_id
  AND lot.status = 'open';

UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
SET status = 'canceled', last_error = 'merged into slot ' || merged.kept_id, updated_at = now()
FROM merged_open_slots AS merged
WHERE slot.id = merged.id AND merged.id <> merged.kept_id;

UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
SET eligible_after = due.eligible_after, updated_at = now()
FROM (SELECT kept_id, max(eligible_after) AS eligible_after FROM merged_open_slots GROUP BY kept_id) AS due
WHERE slot.id = due.kept_id AND slot.eligible_after < due.eligible_after;

-- 3. Open lots stranded on a failed or released slot are still owed: they
--    move onto their target's open slot, created where the target has none.
--    The schedule-repair scan that requeued them is deleted.
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
    WHERE slot.target_id = stranded.target_id AND slot.status IN ('scheduled', 'requested'));

UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
SET scheduled_slot_id = open_slot.id, updated_at = now()
FROM loyal_yield.balance_sweep_scheduled_slots AS stranded_slot,
     loyal_yield.balance_sweep_scheduled_slots AS open_slot
WHERE stranded_slot.id = lot.scheduled_slot_id
  AND stranded_slot.status IN ('failed', 'released')
  AND lot.status = 'open' AND lot.remaining_amount_raw > 0
  AND open_slot.target_id = lot.target_id
  AND open_slot.status IN ('scheduled', 'requested');

-- 4. The idle block recorded in last_error becomes the column.
UPDATE loyal_yield.balance_sweep_scheduled_slots
SET idle_blocked_since = to_timestamp(substring(last_error FROM 'idle_blocked_since=([0-9]{1,12})')::bigint)
WHERE status IN ('scheduled', 'requested')
  AND last_error ~ '^existing idle vault balance must drain before direct autodeposit: [0-9]+ \[idle_blocked_since=[0-9]{1,12}; idle_deferrals=[0-9]{1,7}\]$';

CREATE UNIQUE INDEX balance_sweep_scheduled_slots_one_open_uidx
    ON loyal_yield.balance_sweep_scheduled_slots (target_id)
    WHERE status IN ('scheduled', 'requested');

ALTER TABLE loyal_yield.balance_sweep_scheduled_slots
    ENABLE TRIGGER balance_sweep_scheduled_slots_realtime_event;
