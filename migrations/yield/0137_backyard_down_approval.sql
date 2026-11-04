ALTER TABLE loyal_yield.backyard_lane_levels
    ADD COLUMN down_approval jsonb NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(down_approval) = 'object');

-- Preserve the old Earn MAX target's meaning: stored 1 used to mean 1.5x.
-- A fresh, cost-approved decision must choose actual zero borrowing later.
-- Stop the old worker before applying this migration so it cannot write 1
-- with the old meaning again during the cutover.
UPDATE loyal_yield.backyard_lane_levels
SET level = 1.5, up_approved = false
WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
  AND level = 1;
