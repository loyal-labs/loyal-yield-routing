-- Backyard's lanes are its earnLanes registry (go/workers/internal/backyard
-- route_catalog.go), the one list. The journal keeps strategy_key as plain
-- text and no longer repeats the lane list in CHECK constraints (recreated in
-- 0072, 0074, 0079, 0082 and 0102 each time a lane was added).
--
-- Only drops constraints: no row changes, no table rewrite, nothing the
-- running binary reads. Safe to apply before or after the binary that stops
-- listing lanes; apply it before (with the release's other pre-deploy
-- migrations).
ALTER TABLE loyal_yield.multiply_operations DROP CONSTRAINT IF EXISTS multiply_operations_backyard_phase3_strategy_scope;
ALTER TABLE loyal_yield.multiply_operations DROP CONSTRAINT IF EXISTS multiply_operations_backyard_initializer_scope;
ALTER TABLE loyal_yield.multiply_operations DROP CONSTRAINT IF EXISTS multiply_operations_backyard_setup_scope;
