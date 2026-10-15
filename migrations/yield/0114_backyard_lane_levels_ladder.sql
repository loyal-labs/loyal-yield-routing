-- The leverage ladder grows past 1.75x (owner decision 2026-10-14): a lane's
-- stored level is one of 1, 1.5, 1.75, 2, 2.5 or 3 (leverageLadder,
-- go/workers/internal/backyard/leverage_level.go), and each lane climbs no
-- higher than its own cap. Additive: every stored row stays valid, and the
-- old binary never writes a level it did not know. Apply before the binary
-- that writes the new levels.
SET LOCAL lock_timeout = '5s';

ALTER TABLE loyal_yield.backyard_lane_levels
    DROP CONSTRAINT backyard_lane_levels_level_check,
    ADD CONSTRAINT backyard_lane_levels_level_check CHECK (level IN (1, 1.5, 1.75, 2, 2.5, 3));
