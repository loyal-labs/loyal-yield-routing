-- Looping fee intent, exact wire and finalized checkpoint share the existing operation journal.
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
        'STAGE_SQUADS_TO_VOLTR', 'VOLTR_RESTORE_IDLE', 'REPORT_NAV',
        'HOLD_MANUAL_RECOVERY', 'HOLD_CLEARED',
        'POLICY_SETUP_PREFUND', 'POLICY_SETUP_CREATE', 'INITIALIZE_KAMINO_OBLIGATION', 'PAY_PERFORMANCE_FEE'
    )) NOT VALID;
ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_action_check;

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_backyard_action_scope;

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
            'STAGE_SQUADS_TO_VOLTR', 'VOLTR_RESTORE_IDLE', 'REPORT_NAV',
            'HOLD_MANUAL_RECOVERY', 'HOLD_CLEARED',
            'POLICY_SETUP_PREFUND', 'POLICY_SETUP_CREATE', 'INITIALIZE_KAMINO_OBLIGATION', 'PAY_PERFORMANCE_FEE'
        ))
    ) NOT VALID;
ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_backyard_action_scope;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_looping_fee_scope CHECK (
        action <> 'PAY_PERFORMANCE_FEE' OR route_key = 'backyard:6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY'
    );

CREATE UNIQUE INDEX backyard_looping_fee_finalized_version
    ON loyal_yield.multiply_operations ((expected_effects #>> '{expectedEffects,performanceFee,after,version}'))
    WHERE status='reconciled' AND expected_effects #> '{expectedEffects,performanceFee}' IS NOT NULL;

CREATE INDEX backyard_looping_fee_unresolved
    ON loyal_yield.multiply_operations (route_key)
    WHERE status='manual_recovery' AND signed_wire IS NOT NULL
      AND expected_effects #> '{expectedEffects,performanceFee}' IS NOT NULL;

CREATE INDEX backyard_looping_finalized_cash_movements
    ON loyal_yield.multiply_operations (route_key, confirmed_slot)
    WHERE status='reconciled' AND reconciled_effects ? 'loopingCashDelta';

ALTER TABLE loyal_yield.multiply_route_states
    ADD CONSTRAINT backyard_looping_fee_weekly_ceiling CHECK (
        NOT (state ? 'performanceFee') OR (
            route_key = 'backyard:6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY'
            AND COALESCE(jsonb_typeof(state #> '{performanceFee,weeklyCapRaw}') = 'number'
                AND (state #>> '{performanceFee,weeklyCapRaw}')::numeric BETWEEN 1 AND 10000000000
                AND (state #>> '{performanceFee,weeklyCapRaw}')::numeric = trunc((state #>> '{performanceFee,weeklyCapRaw}')::numeric), false)
        )
    );

CREATE TABLE loyal_yield.backyard_looping_fee_capital_resolutions (
    signature text PRIMARY KEY,
    finalized_slot bigint NOT NULL CHECK (finalized_slot > 0),
    principal_usdc_raw bigint NOT NULL CHECK (principal_usdc_raw BETWEEN 0 AND 1000000000000),
    effective_lp_raw bigint NOT NULL CHECK (effective_lp_raw > 0),
    approved_by text NOT NULL CHECK (length(approved_by) BETWEEN 1 AND 128),
    evidence text NOT NULL CHECK (length(evidence) BETWEEN 1 AND 1024),
    approved_version bigint NOT NULL CHECK (approved_version > 0),
    approved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    applied_operation_id text UNIQUE
);
