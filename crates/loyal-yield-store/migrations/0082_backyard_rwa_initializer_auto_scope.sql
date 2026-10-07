-- Expand the initializer journal scope with the reviewed candidate AUTO lane.
-- This schema change grants no policy, signing, budget, or deposit authority:
-- it only allows journal rows for the one additional reviewed initializer
-- lane. Every condition, reason, engine, action and lane restriction that
-- migration 0079 created stays exactly as it was; the constraint is re-created
-- NOT VALID and validated in the same statement pair so deployment still
-- proves existing rows satisfy it.
ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_backyard_initializer_scope;

ALTER TABLE loyal_yield.multiply_operations
 ADD CONSTRAINT multiply_operations_backyard_initializer_scope CHECK (
  action <> 'INITIALIZE_KAMINO_OBLIGATION' OR
  (strategy_key IS NOT NULL AND strategy_key IN (
   'Prime/PRIME/USDC','Maple/syrupUSDC/USDC','OnRe/ONyc/USDC','AUTO/AUTO/PYUSD'
  ))
 ) NOT VALID;
ALTER TABLE loyal_yield.multiply_operations VALIDATE CONSTRAINT multiply_operations_backyard_initializer_scope;
