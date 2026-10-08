CREATE OR REPLACE FUNCTION loyal_yield.finalize_confirmed_autodeposit(
    p_claim_token TEXT,
    p_execution_id BIGINT,
    p_scheduled_slot_id BIGINT,
    p_lease_token TEXT,
    p_post_confirm_position_amount_raw BIGINT,
    p_post_confirm_observed_slot BIGINT
)
RETURNS TEXT
LANGUAGE plpgsql
AS $$
DECLARE
    claim_row RECORD;
    attempt_row RECORD;
    execution_row RECORD;
    slot_row RECORD;
    position_row RECORD;
    plan JSONB;
    plan_amount_raw BIGINT;
    plan_reserve TEXT;
    plan_market TEXT;
    plan_liquidity_mint TEXT;
    plan_wallet TEXT;
    plan_vault_pubkey TEXT;
    plan_settings TEXT;
    plan_vault_index SMALLINT;
    plan_policy_seed BIGINT;
    plan_policy_account TEXT;
    v_deposit_id BIGINT;
    v_position_id BIGINT;
    v_holding_event_id BIGINT;
    deposit_inserted BOOLEAN := FALSE;
    position_existed BOOLEAN := FALSE;
    v_event_type TEXT;
    v_holding_delta_raw BIGINT;
    affected_rows BIGINT;
BEGIN
    SELECT *
    INTO claim_row
    FROM loyal_yield.balance_sweep_lot_claims
    WHERE claim_token = p_claim_token
    FOR UPDATE;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'autodeposit claim % does not exist', p_claim_token;
    END IF;

    SELECT attempt.signature, attempt.confirmed_slot
    INTO attempt_row
    FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
    WHERE attempt.claim_token = p_claim_token
      AND attempt.execution_id = p_execution_id
      AND attempt.operation_kind = 'top_up'
      AND attempt.attempt_state = 'confirmed'
      AND attempt.confirmed_slot IS NOT NULL
    ORDER BY attempt.attempt_number DESC
    LIMIT 1;

    IF NOT FOUND THEN
        RAISE EXCEPTION
            'autodeposit claim % has no confirmed top-up for execution %',
            p_claim_token,
            p_execution_id;
    END IF;

    IF claim_row.status = 'executed' THEN
        IF claim_row.execution_id IS DISTINCT FROM p_execution_id
           OR NOT EXISTS (
                SELECT 1
                FROM loyal_yield.balance_sweep_executions AS execution
                JOIN loyal_yield.user_yield_position_deposits AS deposit
                  ON deposit.id = execution.yield_deposit_id
                JOIN loyal_yield.user_yield_positions AS position
                  ON position.id = execution.yield_position_id
                WHERE execution.id = p_execution_id
                  AND execution.kamino_deposit_signature = attempt_row.signature
                  AND execution.completed_at IS NOT NULL
                  AND deposit.deposit_signature = attempt_row.signature
                  AND position.last_deposit_signature = attempt_row.signature
                  AND EXISTS (
                      SELECT 1
                      FROM loyal_yield.user_yield_position_holding_events AS event
                      WHERE event.position_id = position.id
                        AND event.source_signature = attempt_row.signature
                  )
           )
           OR NOT EXISTS (
                SELECT 1
                FROM loyal_yield.balance_sweep_scheduled_slots AS slot
                WHERE slot.id = p_scheduled_slot_id
                  AND slot.claim_token = p_claim_token
                  AND slot.execution_id = p_execution_id
                  AND slot.status = 'executed'
           )
           OR NOT EXISTS (
                SELECT 1
                FROM loyal_yield.user_yield_position_deposits AS deposit
                WHERE deposit.deposit_signature = attempt_row.signature
                  AND deposit.balance_sweep_execution_id = p_execution_id
           ) THEN
            RAISE EXCEPTION
                'completed autodeposit claim % has inconsistent finalization evidence',
                p_claim_token;
        END IF;
        RETURN 'already_completed';
    END IF;

    IF claim_row.status <> 'selected'
       OR claim_row.autodeposit_executor_lease_token IS DISTINCT FROM p_lease_token
       OR claim_row.autodeposit_executor_lease_expires_at IS NULL
       OR claim_row.autodeposit_executor_lease_expires_at <= now() THEN
        RAISE EXCEPTION USING
            ERRCODE = '55P03',
            MESSAGE = format(
                'claim_owned_by_another_executor: autodeposit claim %s lease is not owned',
                p_claim_token
            );
    END IF;

    SELECT *
    INTO execution_row
    FROM loyal_yield.balance_sweep_executions
    WHERE id = p_execution_id
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'autodeposit execution % does not exist', p_execution_id;
    END IF;
    IF execution_row.kamino_deposit_signature IS NOT NULL
       AND execution_row.kamino_deposit_signature <> attempt_row.signature THEN
        RAISE EXCEPTION
            'autodeposit execution % belongs to top-up signature %, not %',
            p_execution_id,
            execution_row.kamino_deposit_signature,
            attempt_row.signature;
    END IF;

    SELECT *
    INTO slot_row
    FROM loyal_yield.balance_sweep_scheduled_slots
    WHERE id = p_scheduled_slot_id
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION
            'autodeposit scheduled slot % does not exist',
            p_scheduled_slot_id;
    END IF;
    IF slot_row.claim_token IS DISTINCT FROM p_claim_token
       OR slot_row.status <> 'selected' THEN
        RAISE EXCEPTION
            'autodeposit scheduled slot % is not selected for claim %',
            p_scheduled_slot_id,
            p_claim_token;
    END IF;

    plan := claim_row.autodeposit_deposit_plan;
    IF plan IS NULL OR jsonb_typeof(plan) <> 'object' THEN
        RAISE EXCEPTION 'autodeposit claim % has no immutable deposit plan', p_claim_token;
    END IF;

    plan_amount_raw := (plan ->> 'amountRaw')::BIGINT;
    plan_reserve := plan ->> 'reserve';
    plan_market := plan ->> 'market';
    plan_liquidity_mint := plan ->> 'liquidityMint';
    plan_wallet := plan #>> '{target,wallet}';
    plan_vault_pubkey := plan #>> '{target,vaultPubkey}';
    plan_settings := plan #>> '{target,settings}';
    plan_vault_index := (plan #>> '{target,vaultIndex}')::SMALLINT;
    plan_policy_seed := (plan #>> '{target,routePolicySeed}')::BIGINT;
    plan_policy_account := plan #>> '{target,routePolicyAccount}';

    IF plan_amount_raw <= 0
       OR plan_amount_raw <> claim_row.amount_raw
       OR plan_reserve IS NULL
       OR plan_liquidity_mint IS NULL
       OR plan_wallet IS NULL
       OR plan_vault_pubkey IS NULL
       OR plan_settings IS NULL
       OR plan_policy_account IS NULL
       OR p_post_confirm_position_amount_raw < 0
       OR p_post_confirm_observed_slot < attempt_row.confirmed_slot THEN
        RAISE EXCEPTION 'autodeposit claim % has invalid finalization evidence', p_claim_token;
    END IF;

    INSERT INTO loyal_yield.user_yield_position_deposits (
        deposit_signature,
        policy_signature,
        confirmed_slot,
        wallet_address,
        smart_account_address,
        settings,
        vault_index,
        vault_pubkey,
        policy_id,
        policy_account,
        policy_seed,
        target_reserve,
        market,
        liquidity_mint,
        target_supply_apy_bps,
        deposit_mint,
        principal_amount_raw,
        balance_sweep_execution_id,
        balance_sweep_scheduled_slot_id,
        confirmed_at,
        created_at
    ) VALUES (
        attempt_row.signature,
        attempt_row.signature,
        attempt_row.confirmed_slot,
        plan_wallet,
        plan_vault_pubkey,
        plan_settings,
        plan_vault_index,
        plan_vault_pubkey,
        plan_policy_seed,
        plan_policy_account,
        plan_policy_seed,
        plan_reserve,
        plan_market,
        plan_liquidity_mint,
        NULL,
        plan_liquidity_mint,
        plan_amount_raw,
        p_execution_id,
        p_scheduled_slot_id,
        now(),
        now()
    )
    ON CONFLICT (deposit_signature) DO NOTHING
    RETURNING id INTO v_deposit_id;
    deposit_inserted := FOUND;

    IF NOT deposit_inserted THEN
        SELECT deposit.id
        INTO v_deposit_id
        FROM loyal_yield.user_yield_position_deposits AS deposit
        WHERE deposit.deposit_signature = attempt_row.signature
          AND deposit.settings = plan_settings
          AND deposit.vault_index = plan_vault_index
          AND deposit.target_reserve = plan_reserve
          AND deposit.principal_amount_raw = plan_amount_raw
          AND (
              deposit.balance_sweep_execution_id IS NULL
              OR deposit.balance_sweep_execution_id = p_execution_id
          )
          AND (
              deposit.balance_sweep_scheduled_slot_id IS NULL
              OR deposit.balance_sweep_scheduled_slot_id = p_scheduled_slot_id
          )
        FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION
                'top-up signature % is already bound to different accounting evidence',
                attempt_row.signature;
        END IF;
        UPDATE loyal_yield.user_yield_position_deposits
        SET balance_sweep_execution_id = p_execution_id,
            balance_sweep_scheduled_slot_id = p_scheduled_slot_id
        WHERE id = v_deposit_id;
    END IF;

    SELECT *
    INTO position_row
    FROM loyal_yield.user_yield_positions AS position
    WHERE position.settings = plan_settings
      AND position.vault_index = plan_vault_index
      AND position.initial_reserve = plan_reserve
    FOR UPDATE;
    position_existed := FOUND;

    IF position_existed THEN
        IF position_row.wallet_address <> plan_wallet
           OR position_row.vault_pubkey <> plan_vault_pubkey THEN
            RAISE EXCEPTION
                'yield position identity conflicts with autodeposit claim %',
                p_claim_token;
        END IF;
        v_holding_delta_raw := CASE
            WHEN position_row.current_reserve = plan_reserve
             AND position_row.current_liquidity_mint = plan_liquidity_mint
            THEN p_post_confirm_position_amount_raw - position_row.current_amount_raw
            ELSE NULL
        END;
        UPDATE loyal_yield.user_yield_positions AS position
        SET deposit_mint = plan_liquidity_mint,
            last_confirmed_slot = attempt_row.confirmed_slot,
            last_deposit_signature = attempt_row.signature,
            policy_account = plan_policy_account,
            policy_id = plan_policy_seed,
            policy_seed = plan_policy_seed,
            principal_amount_raw = position.principal_amount_raw +
                CASE WHEN deposit_inserted THEN plan_amount_raw ELSE 0 END,
            smart_account_address = plan_vault_pubkey,
            vault_pubkey = plan_vault_pubkey,
            wallet_address = plan_wallet,
            current_reserve = plan_reserve,
            current_market = plan_market,
            current_liquidity_mint = plan_liquidity_mint,
            current_amount_raw = p_post_confirm_position_amount_raw,
            current_observed_slot = p_post_confirm_observed_slot,
            current_observed_at = now(),
            status = 'active',
            updated_at = now()
        WHERE position.id = position_row.id
        RETURNING position.id INTO v_position_id;
        v_event_type := 'deposit_top_up';
    ELSE
        INSERT INTO loyal_yield.user_yield_positions (
            wallet_address,
            smart_account_address,
            settings,
            vault_index,
            vault_pubkey,
            policy_id,
            policy_account,
            policy_seed,
            initial_reserve,
            initial_market,
            initial_liquidity_mint,
            initial_supply_apy_bps,
            deposit_mint,
            principal_amount_raw,
            current_reserve,
            current_market,
            current_liquidity_mint,
            current_amount_raw,
            current_observed_slot,
            current_observed_at,
            first_deposit_signature,
            last_deposit_signature,
            last_confirmed_slot,
            status,
            created_at,
            updated_at
        ) VALUES (
            plan_wallet,
            plan_vault_pubkey,
            plan_settings,
            plan_vault_index,
            plan_vault_pubkey,
            plan_policy_seed,
            plan_policy_account,
            plan_policy_seed,
            plan_reserve,
            plan_market,
            plan_liquidity_mint,
            NULL,
            plan_liquidity_mint,
            plan_amount_raw,
            plan_reserve,
            plan_market,
            plan_liquidity_mint,
            p_post_confirm_position_amount_raw,
            p_post_confirm_observed_slot,
            now(),
            attempt_row.signature,
            attempt_row.signature,
            attempt_row.confirmed_slot,
            'active',
            now(),
            now()
        )
        RETURNING id INTO v_position_id;
        v_event_type := 'deposit_initialized';
        v_holding_delta_raw := p_post_confirm_position_amount_raw;
    END IF;

    SELECT event.id
    INTO v_holding_event_id
    FROM loyal_yield.user_yield_position_holding_events AS event
    WHERE event.source_signature = attempt_row.signature
    FOR UPDATE;

    IF FOUND THEN
        IF NOT EXISTS (
            SELECT 1
            FROM loyal_yield.user_yield_position_holding_events AS event
            WHERE event.id = v_holding_event_id
              AND event.position_id = v_position_id
              AND event.source_deposit_id = v_deposit_id
        ) THEN
            RAISE EXCEPTION
                'holding event for top-up signature % belongs to different accounting evidence',
                attempt_row.signature;
        END IF;
    ELSE
        INSERT INTO loyal_yield.user_yield_position_holding_events (
            position_id,
            event_type,
            reserve,
            market,
            liquidity_mint,
            amount_raw,
            principal_delta_raw,
            holding_delta_raw,
            observed_slot,
            observed_at,
            source_signature,
            source_deposit_id,
            created_at
        ) VALUES (
            v_position_id,
            v_event_type::loyal_yield.user_yield_holding_event_type,
            plan_reserve,
            plan_market,
            plan_liquidity_mint,
            p_post_confirm_position_amount_raw,
            CASE WHEN deposit_inserted THEN plan_amount_raw ELSE 0 END,
            v_holding_delta_raw,
            p_post_confirm_observed_slot,
            now(),
            attempt_row.signature,
            v_deposit_id,
            now()
        )
        RETURNING id INTO v_holding_event_id;
    END IF;

    UPDATE loyal_yield.user_yield_positions
    SET last_holding_event_id = v_holding_event_id,
        updated_at = now()
    WHERE id = v_position_id;

    UPDATE loyal_yield.balance_sweep_executions
    SET yield_deposit_id = v_deposit_id,
        yield_position_id = v_position_id,
        kamino_deposit_signature = attempt_row.signature,
        completed_at = now(),
        completion_failure_code = NULL,
        decoded_evidence = COALESCE(decoded_evidence, '{}'::jsonb) ||
            jsonb_build_object(
                'status', 'executed',
                'kaminoDepositSignature', attempt_row.signature,
                'kaminoDepositSlot', attempt_row.confirmed_slot::text
            ),
        decoded_at = now()
    WHERE id = p_execution_id;
    GET DIAGNOSTICS affected_rows = ROW_COUNT;
    IF affected_rows <> 1 THEN
        RAISE EXCEPTION 'autodeposit execution % was not completed', p_execution_id;
    END IF;

    INSERT INTO loyal_yield.balance_sweep_execution_lots
        (execution_id, lot_id, amount_raw)
    SELECT p_execution_id, item.lot_id, item.amount_raw
    FROM loyal_yield.balance_sweep_lot_claim_items AS item
    WHERE item.claim_token = p_claim_token
    ON CONFLICT (execution_id, lot_id) DO NOTHING;

    UPDATE loyal_yield.balance_sweep_lot_claims
    SET status = 'executed',
        execution_id = p_execution_id,
        autodeposit_executor_lease_token = NULL,
        autodeposit_executor_lease_expires_at = NULL,
        updated_at = now()
    WHERE claim_token = p_claim_token
      AND status = 'selected'
      AND autodeposit_executor_lease_token = p_lease_token
      AND autodeposit_executor_lease_expires_at > now();
    GET DIAGNOSTICS affected_rows = ROW_COUNT;
    IF affected_rows <> 1 THEN
        RAISE EXCEPTION USING
            ERRCODE = '55P03',
            MESSAGE = format(
                'claim_owned_by_another_executor: autodeposit claim %s lease expired during finalization',
                p_claim_token
            );
    END IF;

    UPDATE loyal_yield.balance_sweep_scheduled_slots
    SET status = 'executed',
        execution_id = p_execution_id,
        updated_at = now()
    WHERE id = p_scheduled_slot_id
      AND claim_token = p_claim_token
      AND status = 'selected';
    GET DIAGNOSTICS affected_rows = ROW_COUNT;
    IF affected_rows <> 1 THEN
        RAISE EXCEPTION
            'autodeposit scheduled slot % was not completed',
            p_scheduled_slot_id;
    END IF;

    RETURN 'completed';
END;
$$;

COMMENT ON FUNCTION loyal_yield.finalize_confirmed_autodeposit(TEXT, BIGINT, BIGINT, TEXT, BIGINT, BIGINT) IS
    'Atomically publishes a confirmed autodeposit top-up into yield accounting and completes its durable claim.';
