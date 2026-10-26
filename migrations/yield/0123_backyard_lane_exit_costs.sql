-- What a lane's collateral fetches when sold, against its Kamino mark. NAV
-- values held collateral at the mark minus this cost (exit_cost.go), and the
-- selector prices a rotation's release with the source lane's row. One row
-- per lane and source: the last exit quote at the held size, written by the
-- selector sampler, and the last realized sale, written at reconcile.
-- bps_micro is the cost in millionths of a basis point and is never zero.
-- Additive only: the running binary does not read the table.
CREATE TABLE IF NOT EXISTS loyal_yield.backyard_lane_exit_costs (
    lane text NOT NULL,
    source text NOT NULL CHECK (source IN ('quote', 'realized')),
    bps_micro bigint NOT NULL CHECK (bps_micro > 0 AND bps_micro < 10000000000),
    quoted_collateral_raw numeric(20, 0) NOT NULL CHECK (quoted_collateral_raw > 0),
    mark_sf numeric(40, 0) NOT NULL CHECK (mark_sf > 0),
    slot bigint NOT NULL CHECK (slot > 0),
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (lane, source)
);

-- The Oct 25 unwind's realized costs (hxtk-nav-loss-2026-10-25, M5): sold
-- quantity, the Kamino API's 08Z mark in USDC, the last sale's slot and time.
-- ONyc: 347,483.59 ONyc worth $402,857.96 at 1.159358 returned $744.19 less.
-- AUTO: 32,596.40 AUTO worth $33,509.69 at 1.028018 returned $14.34 less.
INSERT INTO loyal_yield.backyard_lane_exit_costs (lane, source, bps_micro, quoted_collateral_raw, mark_sf, slot, recorded_at) VALUES
    ('OnRe/ONyc/USDC', 'realized', 18472764, 347483590500000, 1336648769737984896, 461196099, '2026-10-25T08:51:55Z'),
    ('AUTO/AUTO/PYUSD', 'realized', 4279360, 32596395100, 1185224059322921615, 461196340, '2026-10-25T08:52:45Z')
ON CONFLICT (lane, source) DO NOTHING;
