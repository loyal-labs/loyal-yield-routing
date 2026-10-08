-- Phase 2 keeps one canonical route row and journal while adding the
-- route-neutral action vocabulary used by the frozen Maple representative.
-- Existing PRIME action rows remain valid for Phase 1 compatibility.

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
        'OPEN_ROUTE_STEP', 'DELEVER_ROUTE_STEP',
        'STAGE_SQUADS_TO_VOLTR', 'VOLTR_RESTORE_IDLE', 'REPORT_NAV', 'HOLD_MANUAL_RECOVERY'
    )) NOT VALID;

ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_action_check;

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_backyard_action_scope;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_backyard_action_scope CHECK (
        (
            engine_version = 'backyard_rwa_v1'
            AND action IN (
                'HOLD', 'RECOVER_TRANSACTION', 'VOLTR_ALLOCATE_TO_SQUADS',
                'SWAP_USDC_TO_PRIME_STEP', 'SWAP_PRIME_TO_USDC_STEP',
                'OPEN_PRIME_USDC_STEP', 'DELEVER_PRIME_USDC_STEP',
                'SWAP_STABLE_TO_COLLATERAL_STEP', 'SWAP_COLLATERAL_TO_STABLE_STEP',
                'OPEN_ROUTE_STEP', 'DELEVER_ROUTE_STEP',
                'STAGE_SQUADS_TO_VOLTR', 'VOLTR_RESTORE_IDLE', 'REPORT_NAV', 'HOLD_MANUAL_RECOVERY'
            )
        )
        OR
        (
            engine_version <> 'backyard_rwa_v1'
            AND action NOT IN (
                'HOLD', 'RECOVER_TRANSACTION', 'VOLTR_ALLOCATE_TO_SQUADS',
                'SWAP_USDC_TO_PRIME_STEP', 'SWAP_PRIME_TO_USDC_STEP',
                'OPEN_PRIME_USDC_STEP', 'DELEVER_PRIME_USDC_STEP',
                'SWAP_STABLE_TO_COLLATERAL_STEP', 'SWAP_COLLATERAL_TO_STABLE_STEP',
                'OPEN_ROUTE_STEP', 'DELEVER_ROUTE_STEP',
                'STAGE_SQUADS_TO_VOLTR', 'VOLTR_RESTORE_IDLE', 'REPORT_NAV', 'HOLD_MANUAL_RECOVERY'
            )
        )
    ) NOT VALID;

ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_backyard_action_scope;

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_backyard_phase2_strategy_scope;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_backyard_phase2_strategy_scope CHECK (
        action NOT IN (
            'SWAP_STABLE_TO_COLLATERAL_STEP', 'SWAP_COLLATERAL_TO_STABLE_STEP',
            'OPEN_ROUTE_STEP', 'DELEVER_ROUTE_STEP'
        )
        OR strategy_key = 'Maple/syrupUSDC/USDC'
    ) NOT VALID;

ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_backyard_phase2_strategy_scope;
