-- Apply only after the binary that computes Backyard's target every tick is
-- live: it reads none of these route-state keys. They belonged to the move
-- machines that binary deleted (unwind intents, tranches, partial-withdrawal
-- records, the working lane, selector entries and debt-clear authorities);
-- the lane levels moved to loyal_yield.backyard_lane_levels in 0103.
UPDATE loyal_yield.multiply_route_states
SET state = state - 'leverageTargets' - 'leverageTarget' - 'selectorUnwind' - 'partialWithdrawals' - 'partialWithdrawal'
    - 'tranche' - 'workingLane' - 'debtClearAuthority' - 'debtClearReceipts' - 'selectorEntry' - 'selectorEntryPaused'
WHERE state ?| ARRAY['leverageTargets','leverageTarget','selectorUnwind','partialWithdrawals','partialWithdrawal',
    'tranche','workingLane','debtClearAuthority','debtClearReceipts','selectorEntry','selectorEntryPaused'];
