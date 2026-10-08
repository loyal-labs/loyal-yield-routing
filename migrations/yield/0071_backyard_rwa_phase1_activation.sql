-- 0070 is already live and immutable. Phase 1 adds the two serialized
-- Jupiter mutations and converts the existing settings/vault owner row in
-- place. The conversion is pinned to independently hashed production bytes;
-- operations, snapshots, and the canonical route key remain untouched.

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
                'STAGE_SQUADS_TO_VOLTR', 'VOLTR_RESTORE_IDLE', 'REPORT_NAV', 'HOLD_MANUAL_RECOVERY'
            )
        )
    ) NOT VALID;

ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_backyard_action_scope;

-- 0070 admitted a not-yet-created route key. Production already has one
-- historical row for the exact settings/vault identity, so replace only the
-- two route-key predicates before converting that row in place.
ALTER TABLE loyal_yield.multiply_route_states
    DROP CONSTRAINT IF EXISTS multiply_route_states_schema_v8_v9_or_backyard_v1;

ALTER TABLE loyal_yield.multiply_route_states
    ADD CONSTRAINT multiply_route_states_schema_v8_v9_or_backyard_v1 CHECK (
        (
            (state ->> 'schemaVersion')::INTEGER = 8
            AND state ->> 'engineVersion' = 'earn_max_v1'
        )
        OR
        (
            (state ->> 'schemaVersion')::INTEGER = 9
            AND state ->> 'engineVersion' = 'earn_max_v2'
        )
        OR
        (
            (state ->> 'schemaVersion')::INTEGER = 10
            AND state ->> 'engineVersion' = 'backyard_rwa_v1'
            AND state ->> 'routeKind' = 'backyard_rwa_v1'
            AND route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
        )
    ) NOT VALID;

ALTER TABLE loyal_yield.multiply_route_states
    VALIDATE CONSTRAINT multiply_route_states_schema_v8_v9_or_backyard_v1;

ALTER TABLE loyal_yield.multiply_route_states
    DROP CONSTRAINT IF EXISTS multiply_route_states_backyard_kind;

ALTER TABLE loyal_yield.multiply_route_states
    ADD CONSTRAINT multiply_route_states_backyard_kind CHECK (
        (state ->> 'engineVersion') IS DISTINCT FROM 'backyard_rwa_v1'
        OR (
            state ->> 'routeKind' = 'backyard_rwa_v1'
            AND route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
        )
    ) NOT VALID;

ALTER TABLE loyal_yield.multiply_route_states
    VALIDATE CONSTRAINT multiply_route_states_backyard_kind;

DO $$
DECLARE
    canonical_route_key CONSTANT TEXT := 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh';
    canonical_settings CONSTANT TEXT := '5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6';
    canonical_vault CONSTANT TEXT := 'ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh';
    prestate_sha256 CONSTANT TEXT := '6e6d0e852bec3b64d92b7a33a8cdd96ecb6270e400b3c0713535cb389599102e';
    poststate_sha256 CONSTANT TEXT := 'f8f33dae4b171fe1eedd3038f2bf2dc440a0aa044e6cbb7f9aac4933ee107ff8';
    route_row loyal_yield.multiply_route_states%ROWTYPE;
    route_key_count BIGINT;
    settings_vault_count BIGINT;
    vault_count BIGINT;
    nonterminal_count BIGINT;
    state_sha256 TEXT;
    affected_rows BIGINT;
BEGIN
    SELECT
        count(*) FILTER (WHERE route_key = canonical_route_key),
        count(*) FILTER (WHERE settings = canonical_settings AND vault_index = 0),
        count(*) FILTER (WHERE vault = canonical_vault)
    INTO route_key_count, settings_vault_count, vault_count
    FROM loyal_yield.multiply_route_states;

    IF route_key_count <> 1 OR settings_vault_count <> 1 OR vault_count <> 1 THEN
        RAISE EXCEPTION 'Backyard Phase 1 canonical route cardinality drifted';
    END IF;

    SELECT *
    INTO route_row
    FROM loyal_yield.multiply_route_states
    WHERE route_key = canonical_route_key
    FOR UPDATE;

    IF route_row.settings IS DISTINCT FROM canonical_settings
        OR route_row.vault_index IS DISTINCT FROM 0
        OR route_row.vault IS DISTINCT FROM canonical_vault
    THEN
        RAISE EXCEPTION 'Backyard Phase 1 canonical route identity drifted';
    END IF;

    IF route_row.lease_owner IS NOT NULL OR route_row.lease_expires_at IS NOT NULL THEN
        RAISE EXCEPTION 'Backyard Phase 1 canonical route has a lease';
    END IF;

    SELECT count(*)
    INTO nonterminal_count
    FROM loyal_yield.multiply_operations
    WHERE route_key = canonical_route_key
      AND status IN (
          'prepared', 'signed_persisted', 'broadcast_intent', 'confirmed',
          'reconciliation_pending', 'decided', 'built', 'simulated',
          'signed', 'submitted', 'reconciling'
      );

    IF nonterminal_count <> 0 THEN
        RAISE EXCEPTION 'Backyard Phase 1 canonical route has a nonterminal operation';
    END IF;

    state_sha256 := encode(sha256(convert_to(route_row.state::text, 'UTF8')), 'hex');

    IF route_row.state_version = 817 AND state_sha256 = prestate_sha256 THEN
        IF route_row.fencing_token IS DISTINCT FROM 14480
            OR route_row.updated_at IS DISTINCT FROM TIMESTAMPTZ '2026-08-24 20:56:31.468439+00'
            OR route_row.state ->> 'schemaVersion' IS DISTINCT FROM '8'
            OR route_row.state ->> 'engineVersion' IS DISTINCT FROM 'earn_max_v1'
            OR route_row.state ->> 'routeKey' IS DISTINCT FROM canonical_route_key
            OR route_row.state ->> 'settings' IS DISTINCT FROM canonical_settings
            OR route_row.state ->> 'vaultIndex' IS DISTINCT FROM '0'
            OR route_row.state ->> 'vault' IS DISTINCT FROM canonical_vault
            OR route_row.state ->> 'generation' IS DISTINCT FROM '817'
            OR route_row.state ->> 'goal' IS DISTINCT FROM 'claimed'
            OR jsonb_typeof(route_row.state -> 'currentOperationId') IS DISTINCT FROM 'null'
            OR jsonb_typeof(route_row.state -> 'manualRecoveryReason') IS DISTINCT FROM 'null'
        THEN
            RAISE EXCEPTION 'Backyard Phase 1 canonical schema-8 prestate fields drifted';
        END IF;

        UPDATE loyal_yield.multiply_route_states
        SET state = state || jsonb_build_object(
                'schemaVersion', 10,
                'engineVersion', 'backyard_rwa_v1',
                'routeKind', 'backyard_rwa_v1',
                'generation', state_version + 1,
                'routeKey', route_key,
                'settings', settings,
                'vaultIndex', vault_index,
                'vault', vault
            ),
            state_version = state_version + 1,
            updated_at = clock_timestamp()
        WHERE route_key = canonical_route_key
          AND state_version = 817
          AND encode(sha256(convert_to(state::text, 'UTF8')), 'hex') = prestate_sha256;

        GET DIAGNOSTICS affected_rows = ROW_COUNT;
        IF affected_rows <> 1 THEN
            RAISE EXCEPTION 'Backyard Phase 1 canonical route conversion lost its exact prestate';
        END IF;
    ELSIF route_row.state_version = 818 AND state_sha256 = poststate_sha256 THEN
        -- Exact poststate is the only idempotent replay accepted.
        NULL;
    ELSE
        RAISE EXCEPTION 'Backyard Phase 1 canonical route is neither the approved prestate nor poststate';
    END IF;

    SELECT *
    INTO route_row
    FROM loyal_yield.multiply_route_states
    WHERE route_key = canonical_route_key;

    state_sha256 := encode(sha256(convert_to(route_row.state::text, 'UTF8')), 'hex');
    IF route_row.state_version IS DISTINCT FROM 818
        OR route_row.settings IS DISTINCT FROM canonical_settings
        OR route_row.vault_index IS DISTINCT FROM 0
        OR route_row.vault IS DISTINCT FROM canonical_vault
        OR route_row.lease_owner IS NOT NULL
        OR route_row.lease_expires_at IS NOT NULL
        OR route_row.fencing_token IS DISTINCT FROM 14480
        OR state_sha256 IS DISTINCT FROM poststate_sha256
        OR route_row.state ->> 'schemaVersion' IS DISTINCT FROM '10'
        OR route_row.state ->> 'engineVersion' IS DISTINCT FROM 'backyard_rwa_v1'
        OR route_row.state ->> 'routeKind' IS DISTINCT FROM 'backyard_rwa_v1'
        OR route_row.state ->> 'routeKey' IS DISTINCT FROM canonical_route_key
        OR route_row.state ->> 'settings' IS DISTINCT FROM canonical_settings
        OR route_row.state ->> 'vaultIndex' IS DISTINCT FROM '0'
        OR route_row.state ->> 'vault' IS DISTINCT FROM canonical_vault
        OR route_row.state ->> 'generation' IS DISTINCT FROM '818'
        OR route_row.state ->> 'goal' IS DISTINCT FROM 'claimed'
        OR jsonb_typeof(route_row.state -> 'currentOperationId') IS DISTINCT FROM 'null'
        OR jsonb_typeof(route_row.state -> 'manualRecoveryReason') IS DISTINCT FROM 'null'
    THEN
        RAISE EXCEPTION 'Backyard Phase 1 canonical route poststate readback failed';
    END IF;
END $$;
