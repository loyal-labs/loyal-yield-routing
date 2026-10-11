-- Admit journal rows for AUTO's overflow lane, AUTO/AUTO/wYLDS (AUTO
-- collateral levered with wYLDS debt through its own obligation). This schema
-- change grants no policy, signing, budget, or deposit authority. It
-- re-creates the two lane-listing scopes exactly as 0079 and 0082 left them,
-- with the one lane added: the route-neutral lifecycle actions (its Hastra
-- conversions journal as the same debt/collateral swap steps) and the
-- obligation initializer. Each constraint is re-created NOT VALID and
-- validated in the same statement pair so deployment still proves existing
-- rows satisfy it.
ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_backyard_phase3_strategy_scope;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_backyard_phase3_strategy_scope CHECK (
        action NOT IN ('SWAP_STABLE_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_STABLE_STEP','OPEN_ROUTE_STEP','DELEVER_ROUTE_STEP',
                       'SWAP_DEBT_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_DEBT_STEP','SWAP_USDC_TO_DEBT_STEP','SWAP_DEBT_TO_USDC_STEP')
        OR (strategy_key IS NOT NULL AND strategy_key IN (
            'Prime/PRIME/USDC','Prime/PRIME/PYUSD','Prime/PRIME/USDS',
            'Maple/syrupUSDC/USDC','AUTO/AUTO/PYUSD','Ethena/USDe/PYUSD','OnRe/ONyc/USDC',
            'AUTO/AUTO/wYLDS'
        ))
    ) NOT VALID;
ALTER TABLE loyal_yield.multiply_operations VALIDATE CONSTRAINT multiply_operations_backyard_phase3_strategy_scope;

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_backyard_initializer_scope;

ALTER TABLE loyal_yield.multiply_operations
 ADD CONSTRAINT multiply_operations_backyard_initializer_scope CHECK (
  action <> 'INITIALIZE_KAMINO_OBLIGATION' OR
  (strategy_key IS NOT NULL AND strategy_key IN (
   'Prime/PRIME/USDC','Maple/syrupUSDC/USDC','OnRe/ONyc/USDC','AUTO/AUTO/PYUSD',
   'AUTO/AUTO/wYLDS'
  ))
 ) NOT VALID;
ALTER TABLE loyal_yield.multiply_operations VALIDATE CONSTRAINT multiply_operations_backyard_initializer_scope;
