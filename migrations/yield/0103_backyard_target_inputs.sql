-- Backyard computes its target every tick from the chain, the rates and three
-- stored inputs: the withdrawal queue (Voltr's own receipts), the leverage
-- level each lane should sit at, and the operator's lane exits. These two
-- tables hold the last two. Nothing else about a move is stored: no unwind
-- intent, tranche, partial-withdrawal record, working lane, selector entry or
-- debt-clear authority.

-- One row per lane: the level the selector's economics chose (1, 1.5 or
-- 1.75) and whether the up step toward it is worth its cost at the last
-- sample. Written by the selector sample loop.
CREATE TABLE IF NOT EXISTS loyal_yield.backyard_lane_levels (
    route_key text NOT NULL REFERENCES loyal_yield.multiply_route_states(route_key),
    lane text NOT NULL,
    level numeric NOT NULL CHECK (level IN (1, 1.5, 1.75)),
    up_approved boolean NOT NULL DEFAULT false,
    decided_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (route_key, lane)
);

-- One row per lane the operator exits: its target debt is 0, and with
-- equity_zero its collateral returns to Voltr as well. Written and cleared
-- only by `loyal-engine backyard exit-lane`.
CREATE TABLE IF NOT EXISTS loyal_yield.backyard_lane_exits (
    route_key text NOT NULL REFERENCES loyal_yield.multiply_route_states(route_key),
    lane text NOT NULL,
    equity_zero boolean NOT NULL DEFAULT false,
    requested_by text NOT NULL CHECK (length(requested_by) BETWEEN 1 AND 256),
    requested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (route_key, lane)
);

-- The level each lane sits at moves to its own row; its approval does not
-- (the next selector sample decides it again). Additive only: the binary
-- running while this applies ignores both tables, and a second run never
-- overwrites a level the new selector has written. The move machines' old
-- route-state keys are left in place for that binary; 0104 removes them once
-- the new binary is live.
INSERT INTO loyal_yield.backyard_lane_levels (route_key, lane, level)
SELECT s.route_key, t.key, (t.value->>'level')::numeric
FROM loyal_yield.multiply_route_states s,
     jsonb_each(CASE WHEN jsonb_typeof(s.state->'leverageTargets') = 'object' THEN s.state->'leverageTargets' ELSE '{}'::jsonb END) t
WHERE (t.value->>'level')::numeric IN (1, 1.5, 1.75)
ON CONFLICT (route_key, lane) DO NOTHING;
