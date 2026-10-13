-- The fleet plans and admits moves from one fresh chain read: reserves and
-- obligations at a slot, plus the moves still in flight at that slot (claimed
-- opportunities and decisions with no route or deposit leg confirmed at or
-- before it). The target-capacity frontier and its reservations were a second
-- copy of reserve supply, advanced from stale telemetry and held in
-- awaiting_telemetry until that telemetry caught up with each landed move.
-- Nothing reads them any more: admission sums the in-flight moves under a
-- per-reserve advisory lock.
--
-- Apply together with the binary that stops writing them. The previous
-- binary's admissions write a frontier and a reservation in the same
-- transaction, so between this migration and the new binary they fail and
-- the opportunity stays unclaimed; nothing is signed. Its signed submissions
-- land and reconcile as before: the triggers dropped here only moved
-- reservations. The app and the Rust services do not read these tables.
SET LOCAL lock_timeout = '5s';
DROP TRIGGER IF EXISTS signed_route_submission_requires_target_capacity ON loyal_yield.signed_route_submissions;
DROP TRIGGER IF EXISTS signed_route_submission_advances_target_capacity ON loyal_yield.signed_route_submissions;
DROP FUNCTION IF EXISTS loyal_yield.require_target_capacity_reservation_link();
DROP FUNCTION IF EXISTS loyal_yield.advance_target_capacity_from_submission();
DROP TABLE IF EXISTS loyal_yield.target_capacity_reservations;
DROP TABLE IF EXISTS loyal_yield.target_capacity_frontiers;
