-- Backyard RWA reuses the existing route state, operation journal, and snapshots.
ALTER TABLE loyal_yield.multiply_operations DROP CONSTRAINT IF EXISTS multiply_operations_engine_version_check;
ALTER TABLE loyal_yield.multiply_operations ADD CONSTRAINT multiply_operations_engine_version_check CHECK (engine_version IN ('canary_migrated', 'linus_v1', 'earn_max_v1', 'backyard_rwa_v1'));

ALTER TABLE loyal_yield.multiply_operations DROP CONSTRAINT IF EXISTS multiply_operations_action_check;
ALTER TABLE loyal_yield.multiply_operations ADD CONSTRAINT multiply_operations_action_check CHECK (action IN ('deposit_claim_asset','swap_claim_to_collateral','deposit_collateral','borrow_debt','swap_debt_to_collateral','withdraw_collateral','swap_collateral_to_debt','repay_debt','withdraw_remaining_collateral','swap_collateral_to_claim','claim','HOLD','RECOVER_TRANSACTION','VOLTR_ALLOCATE_TO_SQUADS','OPEN_PRIME_USDC_STEP','DELEVER_PRIME_USDC_STEP','STAGE_SQUADS_TO_VOLTR','VOLTR_RESTORE_IDLE','REPORT_NAV','HOLD_MANUAL_RECOVERY'));
ALTER TABLE loyal_yield.multiply_operations ADD CONSTRAINT multiply_operations_backyard_action_scope CHECK (
    (engine_version = 'backyard_rwa_v1' AND action IN ('HOLD','RECOVER_TRANSACTION','VOLTR_ALLOCATE_TO_SQUADS','OPEN_PRIME_USDC_STEP','DELEVER_PRIME_USDC_STEP','STAGE_SQUADS_TO_VOLTR','VOLTR_RESTORE_IDLE','REPORT_NAV','HOLD_MANUAL_RECOVERY'))
    OR
    (engine_version <> 'backyard_rwa_v1' AND action NOT IN ('HOLD','RECOVER_TRANSACTION','VOLTR_ALLOCATE_TO_SQUADS','OPEN_PRIME_USDC_STEP','DELEVER_PRIME_USDC_STEP','STAGE_SQUADS_TO_VOLTR','VOLTR_RESTORE_IDLE','REPORT_NAV','HOLD_MANUAL_RECOVERY'))
);

ALTER TABLE loyal_yield.multiply_operations DROP CONSTRAINT IF EXISTS multiply_operations_status_check;
ALTER TABLE loyal_yield.multiply_operations ADD CONSTRAINT multiply_operations_status_check CHECK (status IN ('prepared','signed_persisted','broadcast_intent','confirmed','reconciliation_pending','reconciled','expired','manual_recovery','held','decided','built','simulated','signed','submitted','reconciling','failed'));

ALTER TABLE loyal_yield.multiply_operations
    ADD COLUMN IF NOT EXISTS simulation_slot BIGINT CHECK (simulation_slot IS NULL OR simulation_slot > 0),
    ADD COLUMN IF NOT EXISTS simulation_result JSONB CHECK (simulation_result IS NULL OR jsonb_typeof(simulation_result) = 'object'),
    ADD COLUMN IF NOT EXISTS submitted_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS confirmation_status TEXT CHECK (confirmation_status IS NULL OR confirmation_status IN ('confirmed', 'finalized')),
    ADD COLUMN IF NOT EXISTS reconciled_effects JSONB CHECK (reconciled_effects IS NULL OR jsonb_typeof(reconciled_effects) = 'object'),
    ADD COLUMN IF NOT EXISTS recovery_reason TEXT CHECK (recovery_reason IS NULL OR length(recovery_reason) BETWEEN 1 AND 512);

-- Replace the original unnamed lifecycle check so the existing journal can also
-- represent Backyard's persist-before-send states; no new journal is introduced.
ALTER TABLE loyal_yield.multiply_operations DROP CONSTRAINT IF EXISTS multiply_operations_check;
ALTER TABLE loyal_yield.multiply_operations ADD CONSTRAINT multiply_operations_backyard_lifecycle CHECK (
    (
        engine_version = 'backyard_rwa_v1'
        AND (
            (status = 'decided'
                AND signed_wire IS NULL
                AND transaction_signature IS NULL
                AND simulation_result IS NULL)
            OR
            (status = 'built'
                AND signed_wire IS NULL
                AND transaction_signature IS NULL
                AND message_sha256 IS NOT NULL
                AND simulation_result IS NULL)
            OR
            (status = 'simulated'
                AND signed_wire IS NULL
                AND transaction_signature IS NULL
                AND message_sha256 IS NOT NULL
                AND simulation_slot IS NOT NULL
                AND simulation_result IS NOT NULL)
            OR
            (status = 'signed'
                AND signed_wire IS NOT NULL
                AND signed_wire_sha256 IS NOT NULL
                AND transaction_signature IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
                AND simulation_slot IS NOT NULL
                AND simulation_result IS NOT NULL
                AND broadcast_intent_at IS NULL)
            OR
            (status IN ('broadcast_intent', 'submitted', 'confirmed', 'reconciling')
                AND signed_wire IS NOT NULL
                AND signed_wire_sha256 IS NOT NULL
                AND transaction_signature IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
                AND simulation_slot IS NOT NULL
                AND simulation_result IS NOT NULL
                AND broadcast_intent_at IS NOT NULL)
            OR
            (status = 'reconciled'
                AND signed_wire_sha256 IS NOT NULL
                AND transaction_signature IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
                AND broadcast_intent_at IS NOT NULL
                AND confirmed_slot IS NOT NULL
                AND confirmation_status IN ('confirmed', 'finalized')
                AND reconciliation_sha256 IS NOT NULL
                AND reconciled_effects IS NOT NULL)
            OR
            (status = 'failed' AND broadcast_intent_at IS NULL)
            OR
            (status = 'held'
                AND action = 'HOLD'
                AND signed_wire IS NULL
                AND transaction_signature IS NULL
                AND broadcast_intent_at IS NULL)
            OR
            (status = 'manual_recovery' AND recovery_reason IS NOT NULL)
        )
    )
    OR
    (
        engine_version <> 'backyard_rwa_v1'
        AND (
            (status = 'prepared'
                AND signed_wire IS NULL
                AND transaction_signature IS NULL)
            OR
            (status = 'signed_persisted'
                AND signed_wire IS NOT NULL
                AND signed_wire_sha256 IS NOT NULL
                AND transaction_signature IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
                AND broadcast_intent_at IS NULL)
            OR
            (status IN ('broadcast_intent', 'confirmed', 'reconciliation_pending')
                AND signed_wire IS NOT NULL
                AND signed_wire_sha256 IS NOT NULL
                AND transaction_signature IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
                AND broadcast_intent_at IS NOT NULL)
            OR
            (status = 'reconciled'
                AND (action <> 'deposit_claim_asset' OR engine_version = 'canary_migrated')
                AND signed_wire IS NULL
                AND signed_wire_sha256 IS NOT NULL
                AND transaction_signature IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
                AND broadcast_intent_at IS NOT NULL
                AND confirmed_slot IS NOT NULL
                AND reconciliation_sha256 IS NOT NULL)
            OR
            (status = 'reconciled'
                AND action = 'deposit_claim_asset'
                AND engine_version = 'linus_v1'
                AND signed_wire IS NULL
                AND signed_wire_sha256 IS NOT NULL
                AND transaction_signature IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
                AND broadcast_intent_at IS NULL
                AND confirmed_slot IS NOT NULL
                AND reconciliation_sha256 IS NOT NULL
                AND policy_account IS NULL
                AND policy_data_sha256 IS NULL)
            OR
            (status = 'expired'
                AND signed_wire IS NULL
                AND signed_wire_sha256 IS NOT NULL
                AND transaction_signature IS NOT NULL
                AND recent_blockhash IS NOT NULL
                AND last_valid_block_height IS NOT NULL
                AND confirmed_slot IS NULL)
            OR
            status = 'manual_recovery'
        )
    )
);

DROP INDEX IF EXISTS loyal_yield.multiply_operations_one_nonterminal_per_route;
CREATE UNIQUE INDEX multiply_operations_one_nonterminal_per_route ON loyal_yield.multiply_operations (route_key)
WHERE status IN ('prepared','signed_persisted','broadcast_intent','confirmed','reconciliation_pending','decided','built','simulated','signed','submitted','reconciling');

ALTER TABLE loyal_yield.multiply_route_states
    ADD CONSTRAINT multiply_route_states_backyard_kind CHECK (
        (state ->> 'engineVersion') IS DISTINCT FROM 'backyard_rwa_v1'
        OR state ->> 'routeKind' = 'backyard_rwa_v1'
    );

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_backyard_submission_evidence CHECK (
        engine_version <> 'backyard_rwa_v1'
        OR (status <> 'submitted' OR submitted_at IS NOT NULL)
    ),
    ADD CONSTRAINT multiply_operations_backyard_confirmation_evidence CHECK (
        engine_version <> 'backyard_rwa_v1'
        OR (status NOT IN ('confirmed','reconciling','reconciled')
            OR (confirmed_slot IS NOT NULL AND confirmation_status IN ('confirmed','finalized')))
    );
