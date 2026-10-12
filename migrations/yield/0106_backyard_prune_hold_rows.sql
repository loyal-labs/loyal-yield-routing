-- A Backyard hold is not a journal row. The one-writer binary reports why a
-- tick did not move the vault on /metrics (loyal_backyard_hold{lane,reason})
-- and in one log line when it changes; only transactions get rows. The
-- previous binaries wrote a HOLD row on every holding tick (131,522 rows,
-- about 149 MB on 2026-10-12, nearly all debt_reserve_utilization_blocks_borrow).
--
-- Nothing reads them: no foreign key references multiply_operations, the
-- worker's journal reads (the decision epoch, NAV cadence, the reconciled
-- bridge journal, capital recovery, the in-flight count) filter by other
-- actions or statuses, and the observer and the Rust services do not read
-- Backyard rows.
--
-- HOLD_MANUAL_RECOVERY and HOLD_CLEARED rows stay: they are the history of
-- operator stops and clears (a few rows), and the previous binary derives its
-- latch from the pair when the latch row is cleared, so removing only one
-- side would re-arm a stop on a rollback.
--
-- Apply after the one-writer binary is live: the previous binary keeps
-- writing a HOLD row every tick until it stops.
DELETE FROM loyal_yield.multiply_operations
WHERE engine_version = 'backyard_rwa_v1'
  AND action = 'HOLD'
  AND status = 'held';
