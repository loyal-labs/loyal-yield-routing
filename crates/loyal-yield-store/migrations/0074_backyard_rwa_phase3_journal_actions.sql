-- Extend journal vocabulary, not on-chain authority. Runtime construction,
-- admission, signer and final-send gates remain independent. The five pending
-- farm-repair lanes are NOT enabled for normal lifecycle actions by this file.
ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_action_check;
ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_action_check CHECK (action IN (
        'request_withdrawal', 'cancel_withdrawal', 'deposit_claim_asset',
        'swap_claim_to_collateral', 'deposit_collateral', 'borrow_debt',
        'swap_debt_to_collateral', 'withdraw_collateral', 'swap_collateral_to_debt',
        'repay_debt', 'withdraw_remaining_collateral', 'swap_collateral_to_claim', 'claim',
        'HOLD', 'RECOVER_TRANSACTION', 'VOLTR_ALLOCATE_TO_SQUADS',
        'SWAP_USDC_TO_PRIME_STEP', 'SWAP_PRIME_TO_USDC_STEP',
        'OPEN_PRIME_USDC_STEP', 'DELEVER_PRIME_USDC_STEP',
        'SWAP_STABLE_TO_COLLATERAL_STEP', 'SWAP_COLLATERAL_TO_STABLE_STEP',
        'SWAP_DEBT_TO_COLLATERAL_STEP', 'SWAP_COLLATERAL_TO_DEBT_STEP',
        'SWAP_USDC_TO_DEBT_STEP', 'SWAP_DEBT_TO_USDC_STEP',
        'OPEN_ROUTE_STEP', 'DELEVER_ROUTE_STEP',
        'STAGE_SQUADS_TO_VOLTR', 'VOLTR_RESTORE_IDLE', 'REPORT_NAV', 'HOLD_MANUAL_RECOVERY',
        'POLICY_SETUP_PREFUND', 'POLICY_SETUP_CREATE'
    )) NOT VALID;
ALTER TABLE loyal_yield.multiply_operations VALIDATE CONSTRAINT multiply_operations_action_check;

ALTER TABLE loyal_yield.multiply_operations DROP CONSTRAINT IF EXISTS multiply_operations_backyard_action_scope;
ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_backyard_action_scope CHECK (
        (engine_version = 'backyard_rwa_v1') = (action IN (
            'HOLD', 'RECOVER_TRANSACTION', 'VOLTR_ALLOCATE_TO_SQUADS',
            'SWAP_USDC_TO_PRIME_STEP', 'SWAP_PRIME_TO_USDC_STEP',
            'OPEN_PRIME_USDC_STEP', 'DELEVER_PRIME_USDC_STEP',
            'SWAP_STABLE_TO_COLLATERAL_STEP', 'SWAP_COLLATERAL_TO_STABLE_STEP',
            'SWAP_DEBT_TO_COLLATERAL_STEP', 'SWAP_COLLATERAL_TO_DEBT_STEP',
            'SWAP_USDC_TO_DEBT_STEP', 'SWAP_DEBT_TO_USDC_STEP',
            'OPEN_ROUTE_STEP', 'DELEVER_ROUTE_STEP',
            'STAGE_SQUADS_TO_VOLTR', 'VOLTR_RESTORE_IDLE', 'REPORT_NAV', 'HOLD_MANUAL_RECOVERY',
            'POLICY_SETUP_PREFUND', 'POLICY_SETUP_CREATE'
        ))
    ) NOT VALID;
ALTER TABLE loyal_yield.multiply_operations VALIDATE CONSTRAINT multiply_operations_backyard_action_scope;

ALTER TABLE loyal_yield.multiply_operations DROP CONSTRAINT IF EXISTS multiply_operations_backyard_phase2_strategy_scope;
ALTER TABLE loyal_yield.multiply_operations DROP CONSTRAINT IF EXISTS multiply_operations_backyard_phase3_strategy_scope;
ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_backyard_phase3_strategy_scope CHECK (
        action NOT IN ('SWAP_STABLE_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_STABLE_STEP','OPEN_ROUTE_STEP','DELEVER_ROUTE_STEP',
                       'SWAP_DEBT_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_DEBT_STEP','SWAP_USDC_TO_DEBT_STEP','SWAP_DEBT_TO_USDC_STEP')
        OR (strategy_key IS NOT NULL AND strategy_key IN (
            'Prime/PRIME/USDC','Prime/PRIME/PYUSD','Prime/PRIME/USDS',
            'Maple/syrupUSDC/USDC','AUTO/AUTO/PYUSD','Ethena/USDe/PYUSD'
        ))
    ) NOT VALID;
ALTER TABLE loyal_yield.multiply_operations VALIDATE CONSTRAINT multiply_operations_backyard_phase3_strategy_scope;

ALTER TABLE loyal_yield.multiply_operations DROP CONSTRAINT IF EXISTS multiply_operations_backyard_setup_scope;
ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_backyard_setup_scope CHECK (
        action NOT IN ('POLICY_SETUP_PREFUND','POLICY_SETUP_CREATE')
        OR (strategy_key IS NOT NULL AND strategy_key = 'OnRe/ONyc/USDC')
    ) NOT VALID;
ALTER TABLE loyal_yield.multiply_operations VALIDATE CONSTRAINT multiply_operations_backyard_setup_scope;
