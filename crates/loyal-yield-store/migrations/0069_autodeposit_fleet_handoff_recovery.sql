CREATE OR REPLACE FUNCTION loyal_yield.finalize_fleet_handoff_autodeposit(
    p_claim_token TEXT,
    p_execution_id BIGINT,
    p_scheduled_slot_id BIGINT,
    p_lease_token TEXT,
    p_decision_id BIGINT
)
RETURNS TEXT
LANGUAGE plpgsql
AS $$
DECLARE
    claim_row RECORD;
    pull_row RECORD;
    execution_row RECORD;
    slot_row RECORD;
    decision_row RECORD;
    snapshot_position_row RECORD;
    position_row RECORD;
    plan JSONB;
    plan_amount_raw BIGINT;
    plan_wallet TEXT;
    plan_vault_pubkey TEXT;
    plan_settings TEXT;
    plan_vault_index SMALLINT;
    plan_policy_seed BIGINT;
    plan_policy_account TEXT;
    plan_liquidity_mint TEXT;
    v_candidate_count BIGINT;
    v_position_count BIGINT;
    v_deposit_id BIGINT;
    v_position_id BIGINT;
    v_holding_event_id BIGINT;
    deposit_inserted BOOLEAN := FALSE;
    v_handoff_is_current BOOLEAN;
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

    IF claim_row.status = 'executed' THEN
        IF claim_row.execution_id IS DISTINCT FROM p_execution_id
           OR NOT EXISTS (
                SELECT 1
                FROM loyal_yield.balance_sweep_executions AS execution
                WHERE execution.id = p_execution_id
                  AND execution.completed_at IS NOT NULL
                  AND execution.decoded_evidence ->> 'recoverySource' = 'fleet_idle_handoff'
                  AND (execution.decoded_evidence ->> 'fleetDecisionId')::BIGINT = p_decision_id
                  AND execution.yield_deposit_id IS NOT NULL
                  AND execution.yield_position_id IS NOT NULL
           )
           OR NOT EXISTS (
                SELECT 1
                FROM loyal_yield.balance_sweep_scheduled_slots AS scheduled
                WHERE scheduled.id = p_scheduled_slot_id
                  AND scheduled.status = 'executed'
                  AND scheduled.execution_id = p_execution_id
           ) THEN
            RAISE EXCEPTION
                'completed autodeposit claim % has inconsistent Fleet handoff evidence',
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

    SELECT attempt.*
    INTO pull_row
    FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
    WHERE attempt.claim_token = p_claim_token
      AND attempt.operation_kind = 'pull'
      AND attempt.attempt_state = 'confirmed'
      AND attempt.confirmed_slot IS NOT NULL
    ORDER BY attempt.attempt_number DESC
    LIMIT 1;

    IF NOT FOUND OR EXISTS (
        SELECT 1
        FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
        WHERE attempt.claim_token = p_claim_token
          AND attempt.operation_kind = 'top_up'
    ) THEN
        RAISE EXCEPTION
            'autodeposit claim % is not a confirmed pull with no direct top-up',
            p_claim_token;
    END IF;

    SELECT *
    INTO execution_row
    FROM loyal_yield.balance_sweep_executions
    WHERE id = p_execution_id
    FOR UPDATE;
    IF NOT FOUND
       OR execution_row.target_id IS DISTINCT FROM claim_row.target_id
       OR execution_row.signature IS DISTINCT FROM pull_row.signature
       OR execution_row.slot IS DISTINCT FROM pull_row.confirmed_slot
       OR execution_row.amount_raw IS DISTINCT FROM claim_row.amount_raw THEN
        RAISE EXCEPTION
            'autodeposit execution % does not match confirmed pull for claim %',
            p_execution_id,
            p_claim_token;
    END IF;

    SELECT *
    INTO slot_row
    FROM loyal_yield.balance_sweep_scheduled_slots
    WHERE id = p_scheduled_slot_id
    FOR UPDATE;
    IF NOT FOUND
       OR slot_row.claim_token IS DISTINCT FROM p_claim_token
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
    plan_wallet := plan #>> '{target,wallet}';
    plan_vault_pubkey := plan #>> '{target,vaultPubkey}';
    plan_settings := plan #>> '{target,settings}';
    plan_vault_index := (plan #>> '{target,vaultIndex}')::SMALLINT;
    plan_policy_seed := (plan #>> '{target,routePolicySeed}')::BIGINT;
    plan_policy_account := plan #>> '{target,routePolicyAccount}';
    plan_liquidity_mint := plan ->> 'liquidityMint';

    IF plan_amount_raw IS DISTINCT FROM claim_row.amount_raw
       OR plan_wallet IS NULL
       OR plan_vault_pubkey IS NULL
       OR plan_settings IS NULL
       OR plan_policy_account IS NULL
       OR plan_liquidity_mint IS NULL THEN
        RAISE EXCEPTION 'autodeposit claim % has invalid immutable plan', p_claim_token;
    END IF;

    -- This recovery is rare and correctness matters more than concurrent decision
    -- throughput. Freeze Fleet decision transitions until uniqueness is checked and
    -- the selected decision is locked, so a second confirmation cannot race adoption.
    LOCK TABLE loyal_yield.rebalance_decisions IN SHARE ROW EXCLUSIVE MODE;

    SELECT count(*)
    INTO v_candidate_count
    FROM loyal_yield.rebalance_decisions AS candidate
    JOIN loyal_yield.managed_vaults AS vault
      ON vault.id = candidate.vault_id
    JOIN loyal_yield.vault_position_snapshots AS snapshot
      ON snapshot.id = candidate.post_snapshot_id
    JOIN loyal_yield.vault_position_snapshot_positions AS snapshot_position
      ON snapshot_position.snapshot_id = snapshot.id
     AND snapshot_position.reserve = candidate.target_reserve
    WHERE vault.settings = plan_settings
      AND vault.vault_index = plan_vault_index
      AND vault.vault_pubkey = plan_vault_pubkey
      AND candidate.status = 'confirmed'
      AND candidate.decision_reason = 'idle_vault_liquidity_available'
      AND candidate.execution_plan ->> 'kind' = 'idle_vault_deposit'
      AND candidate.amount_raw = plan_amount_raw
      AND COALESCE(candidate.target_liquidity_mint, candidate.liquidity_mint) = plan_liquidity_mint
      AND candidate.signature IS NOT NULL
      AND candidate.confirmed_slot > pull_row.confirmed_slot
      AND candidate.execution_plan ->> 'idle_token_account' =
          COALESCE(execution_row.destination_token_ata, execution_row.destination_vault_ata)
      AND candidate.execution_plan ->> 'idle_observed_slot' ~ '^[0-9]+$'
      AND (candidate.execution_plan ->> 'idle_observed_slot')::BIGINT >=
          pull_row.confirmed_slot
      AND snapshot.observed_slot >= candidate.confirmed_slot
      AND snapshot_position.liquidity_mint = plan_liquidity_mint;

    IF v_candidate_count <> 1 THEN
        RAISE EXCEPTION USING
            ERRCODE = 'P2187',
            MESSAGE = format(
                'autodeposit claim %s has %s matching confirmed Fleet handoffs, expected exactly one',
                p_claim_token,
                v_candidate_count
            );
    END IF;

    SELECT candidate.*
    INTO decision_row
    FROM loyal_yield.rebalance_decisions AS candidate
    JOIN loyal_yield.managed_vaults AS vault
      ON vault.id = candidate.vault_id
    JOIN loyal_yield.vault_position_snapshots AS snapshot
      ON snapshot.id = candidate.post_snapshot_id
    JOIN loyal_yield.vault_position_snapshot_positions AS snapshot_position
      ON snapshot_position.snapshot_id = snapshot.id
     AND snapshot_position.reserve = candidate.target_reserve
    WHERE candidate.id = p_decision_id
      AND vault.settings = plan_settings
      AND vault.vault_index = plan_vault_index
      AND vault.vault_pubkey = plan_vault_pubkey
      AND candidate.status = 'confirmed'
      AND candidate.decision_reason = 'idle_vault_liquidity_available'
      AND candidate.execution_plan ->> 'kind' = 'idle_vault_deposit'
      AND candidate.amount_raw = plan_amount_raw
      AND COALESCE(candidate.target_liquidity_mint, candidate.liquidity_mint) = plan_liquidity_mint
      AND candidate.signature IS NOT NULL
      AND candidate.confirmed_slot > pull_row.confirmed_slot
      AND candidate.execution_plan ->> 'idle_token_account' =
          COALESCE(execution_row.destination_token_ata, execution_row.destination_vault_ata)
      AND candidate.execution_plan ->> 'idle_observed_slot' ~ '^[0-9]+$'
      AND (candidate.execution_plan ->> 'idle_observed_slot')::BIGINT >=
          pull_row.confirmed_slot
      AND snapshot.observed_slot >= candidate.confirmed_slot
      AND snapshot_position.liquidity_mint = plan_liquidity_mint
    FOR UPDATE OF candidate;

    IF NOT FOUND THEN
        RAISE EXCEPTION USING
            ERRCODE = 'P2187',
            MESSAGE = format(
                'Fleet decision %s is not the unique confirmed handoff for claim %s',
                p_decision_id,
                p_claim_token
            );
    END IF;

    SELECT snapshot_position.*, snapshot.observed_slot, snapshot.observed_at
    INTO snapshot_position_row
    FROM loyal_yield.vault_position_snapshots AS snapshot
    JOIN loyal_yield.vault_position_snapshot_positions AS snapshot_position
      ON snapshot_position.snapshot_id = snapshot.id
    WHERE snapshot.id = decision_row.post_snapshot_id
      AND snapshot_position.reserve = decision_row.target_reserve
      AND snapshot_position.liquidity_mint = plan_liquidity_mint;

    SELECT count(*)
    INTO v_position_count
    FROM loyal_yield.user_yield_positions AS position
    WHERE position.settings = plan_settings
      AND position.vault_index = plan_vault_index
      AND position.wallet_address = plan_wallet
      AND position.vault_pubkey = plan_vault_pubkey
      AND position.status = 'active';
    IF v_position_count <> 1 THEN
        RAISE EXCEPTION
            'autodeposit claim % has % active yield positions, expected exactly one for Fleet adoption',
            p_claim_token,
            v_position_count;
    END IF;

    SELECT *
    INTO position_row
    FROM loyal_yield.user_yield_positions AS position
    WHERE position.settings = plan_settings
      AND position.vault_index = plan_vault_index
      AND position.wallet_address = plan_wallet
      AND position.vault_pubkey = plan_vault_pubkey
      AND position.status = 'active'
    FOR UPDATE;

    v_handoff_is_current :=
        snapshot_position_row.observed_slot >= position_row.current_observed_slot;

    INSERT INTO loyal_yield.user_yield_position_deposits (
        deposit_signature, policy_signature, confirmed_slot, wallet_address,
        smart_account_address, settings, vault_index, vault_pubkey, policy_id,
        policy_account, policy_seed, target_reserve, market, liquidity_mint,
        target_supply_apy_bps, deposit_mint, principal_amount_raw,
        balance_sweep_execution_id, balance_sweep_scheduled_slot_id,
        confirmed_at, created_at
    ) VALUES (
        decision_row.signature, decision_row.signature, decision_row.confirmed_slot,
        plan_wallet, plan_vault_pubkey, plan_settings, plan_vault_index,
        plan_vault_pubkey, plan_policy_seed, plan_policy_account, plan_policy_seed,
        decision_row.target_reserve, snapshot_position_row.market,
        snapshot_position_row.liquidity_mint, snapshot_position_row.supply_apy_bps,
        snapshot_position_row.liquidity_mint, plan_amount_raw, p_execution_id,
        p_scheduled_slot_id, now(), now()
    )
    ON CONFLICT (deposit_signature) DO NOTHING
    RETURNING id INTO v_deposit_id;
    deposit_inserted := FOUND;

    IF NOT deposit_inserted THEN
        SELECT deposit.id
        INTO v_deposit_id
        FROM loyal_yield.user_yield_position_deposits AS deposit
        WHERE deposit.deposit_signature = decision_row.signature
          AND deposit.settings = plan_settings
          AND deposit.vault_index = plan_vault_index
          AND deposit.principal_amount_raw = plan_amount_raw
          AND deposit.target_reserve = decision_row.target_reserve
          AND deposit.balance_sweep_execution_id = p_execution_id
          AND deposit.balance_sweep_scheduled_slot_id = p_scheduled_slot_id
        FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION
                'Fleet signature % is already bound to different accounting evidence',
                decision_row.signature;
        END IF;
    END IF;

    v_holding_delta_raw := CASE
        WHEN v_handoff_is_current
         AND position_row.current_reserve = decision_row.target_reserve
         AND position_row.current_liquidity_mint = snapshot_position_row.liquidity_mint
        THEN snapshot_position_row.amount_raw - position_row.current_amount_raw
        ELSE NULL
    END;

    UPDATE loyal_yield.user_yield_positions AS position
    SET last_confirmed_slot = GREATEST(position.last_confirmed_slot, decision_row.confirmed_slot),
        last_deposit_signature = CASE
            WHEN decision_row.confirmed_slot >= COALESCE(position.last_confirmed_slot, -1)
            THEN decision_row.signature
            ELSE position.last_deposit_signature
        END,
        principal_amount_raw = position.principal_amount_raw +
            CASE WHEN deposit_inserted THEN plan_amount_raw ELSE 0 END,
        current_reserve = CASE WHEN v_handoff_is_current
            THEN decision_row.target_reserve ELSE position.current_reserve END,
        current_market = CASE WHEN v_handoff_is_current
            THEN snapshot_position_row.market ELSE position.current_market END,
        current_liquidity_mint = CASE WHEN v_handoff_is_current
            THEN snapshot_position_row.liquidity_mint ELSE position.current_liquidity_mint END,
        current_amount_raw = CASE WHEN v_handoff_is_current
            THEN snapshot_position_row.amount_raw ELSE position.current_amount_raw END,
        current_observed_slot = CASE WHEN v_handoff_is_current
            THEN snapshot_position_row.observed_slot ELSE position.current_observed_slot END,
        current_observed_at = CASE WHEN v_handoff_is_current
            THEN snapshot_position_row.observed_at ELSE position.current_observed_at END,
        last_rebalance_decision_id = CASE WHEN v_handoff_is_current
            THEN decision_row.id ELSE position.last_rebalance_decision_id END,
        updated_at = now()
    WHERE position.id = position_row.id
    RETURNING position.id INTO v_position_id;

    SELECT event.id
    INTO v_holding_event_id
    FROM loyal_yield.user_yield_position_holding_events AS event
    WHERE event.source_signature = decision_row.signature
    FOR UPDATE;
    IF FOUND THEN
        IF NOT EXISTS (
            SELECT 1
            FROM loyal_yield.user_yield_position_holding_events AS event
            WHERE event.id = v_holding_event_id
              AND event.position_id = v_position_id
              AND event.source_deposit_id = v_deposit_id
              AND event.source_rebalance_decision_id = decision_row.id
              AND event.source_snapshot_id = decision_row.post_snapshot_id
        ) THEN
            RAISE EXCEPTION
                'holding event for Fleet signature % belongs to different evidence',
                decision_row.signature;
        END IF;
    ELSE
        INSERT INTO loyal_yield.user_yield_position_holding_events (
            position_id, event_type, reserve, market, liquidity_mint, amount_raw,
            principal_delta_raw, holding_delta_raw, observed_slot, observed_at,
            source_signature, source_deposit_id, source_rebalance_decision_id,
            source_snapshot_id, created_at
        ) VALUES (
            v_position_id, 'deposit_top_up', decision_row.target_reserve,
            snapshot_position_row.market, snapshot_position_row.liquidity_mint,
            snapshot_position_row.amount_raw,
            CASE WHEN deposit_inserted THEN plan_amount_raw ELSE 0 END,
            v_holding_delta_raw, snapshot_position_row.observed_slot,
            snapshot_position_row.observed_at, decision_row.signature, v_deposit_id,
            decision_row.id, decision_row.post_snapshot_id, now()
        )
        RETURNING id INTO v_holding_event_id;
    END IF;

    UPDATE loyal_yield.user_yield_positions
    SET last_holding_event_id = CASE WHEN v_handoff_is_current
            THEN v_holding_event_id ELSE last_holding_event_id END,
        updated_at = now()
    WHERE id = v_position_id;

    UPDATE loyal_yield.balance_sweep_executions
    SET scheduled_slot_id = p_scheduled_slot_id,
        yield_deposit_id = v_deposit_id,
        yield_position_id = v_position_id,
        kamino_deposit_signature = decision_row.signature,
        completed_at = now(),
        completion_failure_code = NULL,
        decoded_evidence = COALESCE(decoded_evidence, '{}'::jsonb) ||
            jsonb_build_object(
                'status', 'executed',
                'recoverySource', 'fleet_idle_handoff',
                'fleetDecisionId', decision_row.id::text,
                'kaminoDepositSignature', decision_row.signature,
                'kaminoDepositSlot', decision_row.confirmed_slot::text,
                'postSnapshotId', decision_row.post_snapshot_id::text
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
                'claim_owned_by_another_executor: autodeposit claim %s lease expired during Fleet handoff finalization',
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

COMMENT ON FUNCTION loyal_yield.finalize_fleet_handoff_autodeposit(TEXT, BIGINT, BIGINT, TEXT, BIGINT) IS
    'Atomically adopts one confirmed Fleet idle-vault deposit that consumed a legacy confirmed Autodeposit pull.';
