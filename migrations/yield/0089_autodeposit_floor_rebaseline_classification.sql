-- Retained Apps floor-rebaseline CTE depends on this classification.
-- Event-id allocation remains governed by the bounded ranges in 0069.
ALTER TYPE loyal_yield.balance_sweep_surplus_classification
    ADD VALUE IF NOT EXISTS 'floor_rebaseline';
