CREATE OR REPLACE FUNCTION pg_temp.repair_autodeposit_fleet_handoff(
    p_claim_token TEXT,
    p_execution_id BIGINT,
    p_scheduled_slot_id BIGINT,
    p_decision_id BIGINT,
    p_expected_pull_signature TEXT,
    p_expected_pull_slot BIGINT,
    p_expected_fleet_signature TEXT,
    p_expected_fleet_slot BIGINT,
    p_expected_liquidity_mint TEXT,
    p_expected_amount_raw BIGINT,
    p_expected_wallet_token_ata TEXT,
    p_expected_vault_token_ata TEXT,
    p_expected_fleet_target_reserve TEXT,
    p_apply BOOLEAN
)
RETURNS JSONB
LANGUAGE plpgsql
AS $$
DECLARE
    claim_row RECORD;
    slot_row RECORD;
    execution_row RECORD;
    pull_attempt_row RECORD;
    decision_row RECORD;
    deposit_row RECORD;
    holding_row RECORD;
    holding_event_id BIGINT;
    position_row RECORD;
    plan JSONB;
    plan_amount_raw BIGINT;
    plan_settings TEXT;
    plan_vault_index SMALLINT;
    plan_vault_pubkey TEXT;
    plan_wallet TEXT;
    plan_liquidity_mint TEXT;
    plan_target_id BIGINT;
    plan_managed_vault_id BIGINT;
    plan_wallet_token_ata TEXT;
    plan_vault_token_ata TEXT;
    pull_attempt_count BIGINT;
    holding_count BIGINT;
    affected_rows BIGINT;
BEGIN
    SELECT *
    INTO claim_row
    FROM loyal_yield.balance_sweep_lot_claims
    WHERE claim_token = p_claim_token
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'explicit Fleet repair: claim % does not exist', p_claim_token;
    END IF;

    SELECT *
    INTO slot_row
    FROM loyal_yield.balance_sweep_scheduled_slots
    WHERE id = p_scheduled_slot_id
    FOR UPDATE;
    IF NOT FOUND OR slot_row.claim_token IS DISTINCT FROM p_claim_token THEN
        RAISE EXCEPTION
            'explicit Fleet repair: scheduled slot % does not belong to claim %',
            p_scheduled_slot_id,
            p_claim_token;
    END IF;

    SELECT *
    INTO execution_row
    FROM loyal_yield.balance_sweep_executions
    WHERE id = p_execution_id
    FOR UPDATE;
    IF NOT FOUND
       OR execution_row.target_id IS DISTINCT FROM claim_row.target_id
       OR execution_row.amount_raw IS DISTINCT FROM claim_row.amount_raw
       OR execution_row.signature IS NULL
       OR execution_row.slot IS NULL THEN
        RAISE EXCEPTION
            'explicit Fleet repair: execution % does not match claim %',
            p_execution_id,
            p_claim_token;
    END IF;

    SELECT count(*)
    INTO pull_attempt_count
    FROM loyal_yield.balance_sweep_transaction_attempts
    WHERE claim_token = p_claim_token
      AND operation_kind = 'pull';

    SELECT attempt.*
    INTO pull_attempt_row
    FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
    WHERE attempt.claim_token = p_claim_token
      AND attempt.operation_kind = 'pull'
      AND attempt.attempt_state = 'confirmed'
      AND attempt.confirmed_slot IS NOT NULL
    ORDER BY attempt.attempt_number DESC
    LIMIT 1;
    IF pull_attempt_count > 0 AND NOT FOUND THEN
        RAISE EXCEPTION
            'explicit Fleet repair: claim % has pull attempts but none is confirmed',
            p_claim_token;
    END IF;
    IF pull_attempt_count > 0
       AND (
           pull_attempt_row.signature IS DISTINCT FROM execution_row.signature
           OR pull_attempt_row.confirmed_slot IS DISTINCT FROM execution_row.slot
           OR pull_attempt_row.scheduled_slot_id IS DISTINCT FROM p_scheduled_slot_id
           OR pull_attempt_row.target_id IS DISTINCT FROM claim_row.target_id
           OR (
               pull_attempt_row.execution_id IS NOT NULL
               AND pull_attempt_row.execution_id <> p_execution_id
           )
           OR pull_attempt_row.amount_raw IS DISTINCT FROM claim_row.amount_raw
       ) THEN
        RAISE EXCEPTION
            'explicit Fleet repair: confirmed pull attempt conflicts with execution %',
            p_execution_id;
    END IF;
    IF EXISTS (
        SELECT 1
        FROM loyal_yield.balance_sweep_transaction_attempts
        WHERE claim_token = p_claim_token
          AND operation_kind = 'top_up'
    ) THEN
        RAISE EXCEPTION
            'explicit Fleet repair: claim % has a direct top-up attempt',
            p_claim_token;
    END IF;

    plan := claim_row.autodeposit_deposit_plan;
    IF plan IS NULL OR jsonb_typeof(plan) <> 'object' THEN
        RAISE EXCEPTION
            'explicit Fleet repair: claim % has no immutable deposit plan',
            p_claim_token;
    END IF;
    plan_amount_raw := (plan ->> 'amountRaw')::BIGINT;
    plan_settings := plan #>> '{target,settings}';
    plan_vault_index := (plan #>> '{target,vaultIndex}')::SMALLINT;
    plan_vault_pubkey := plan #>> '{target,vaultPubkey}';
    plan_wallet := plan #>> '{target,wallet}';
    plan_liquidity_mint := plan ->> 'liquidityMint';
    plan_target_id := (plan #>> '{target,id}')::BIGINT;
    plan_managed_vault_id := (plan #>> '{target,managedVaultId}')::BIGINT;
    plan_wallet_token_ata := plan #>> '{target,walletTokenAta}';
    plan_vault_token_ata := plan #>> '{target,vaultTokenAta}';
    IF plan_amount_raw IS DISTINCT FROM claim_row.amount_raw
       OR plan_settings IS NULL
       OR plan_vault_index IS NULL
       OR plan_vault_pubkey IS NULL
       OR plan_wallet IS NULL
       OR plan_liquidity_mint IS NULL
       OR plan_target_id IS DISTINCT FROM claim_row.target_id
       OR slot_row.target_id IS DISTINCT FROM claim_row.target_id
       OR slot_row.token_mint IS DISTINCT FROM plan_liquidity_mint
       OR execution_row.token_mint IS DISTINCT FROM plan_liquidity_mint
       OR execution_row.source_token_ata IS DISTINCT FROM plan_wallet_token_ata
       OR COALESCE(execution_row.destination_token_ata, execution_row.destination_vault_ata)
            IS DISTINCT FROM plan_vault_token_ata THEN
        RAISE EXCEPTION
            'explicit Fleet repair: claim % has an invalid immutable deposit plan',
            p_claim_token;
    END IF;

    SELECT
        decision.*,
        vault.settings AS vault_settings,
        vault.vault_index AS managed_vault_index,
        vault.vault_pubkey AS managed_vault_pubkey,
        snapshot.observed_slot AS snapshot_observed_slot,
        snapshot_position.market AS snapshot_market,
        snapshot_position.liquidity_mint AS snapshot_liquidity_mint,
        snapshot_position.amount_raw AS snapshot_amount_raw
    INTO decision_row
    FROM loyal_yield.rebalance_decisions AS decision
    JOIN loyal_yield.managed_vaults AS vault
      ON vault.id = decision.vault_id
    JOIN loyal_yield.vault_position_snapshots AS snapshot
      ON snapshot.id = decision.post_snapshot_id
    JOIN loyal_yield.vault_position_snapshot_positions AS snapshot_position
      ON snapshot_position.snapshot_id = snapshot.id
     AND snapshot_position.reserve = decision.target_reserve
    WHERE decision.id = p_decision_id
    FOR UPDATE OF decision;
    IF NOT FOUND
       OR decision_row.status <> 'confirmed'
       OR decision_row.decision_reason <> 'idle_vault_liquidity_available'
       OR decision_row.execution_plan ->> 'kind' <> 'idle_vault_deposit'
       OR decision_row.amount_raw IS DISTINCT FROM plan_amount_raw
       OR COALESCE(decision_row.target_liquidity_mint, decision_row.liquidity_mint)
            IS DISTINCT FROM plan_liquidity_mint
       OR decision_row.signature IS NULL
       OR decision_row.confirmed_slot IS NULL
       OR decision_row.confirmed_slot <= execution_row.slot
       OR decision_row.vault_settings IS DISTINCT FROM plan_settings
       OR decision_row.managed_vault_index IS DISTINCT FROM plan_vault_index
       OR decision_row.managed_vault_pubkey IS DISTINCT FROM plan_vault_pubkey
       OR decision_row.vault_id IS DISTINCT FROM plan_managed_vault_id
       OR decision_row.execution_plan ->> 'idle_token_account'
            IS DISTINCT FROM COALESCE(
                execution_row.destination_token_ata,
                execution_row.destination_vault_ata
            )
       OR decision_row.execution_plan ->> 'idle_observed_slot' !~ '^[0-9]+$'
       OR (decision_row.execution_plan ->> 'idle_observed_slot')::BIGINT < execution_row.slot
       OR decision_row.execution_plan ->> 'idle_vault_liquidity_amount_raw' !~ '^[0-9]+$'
       OR (decision_row.execution_plan ->> 'idle_vault_liquidity_amount_raw')::BIGINT
            IS DISTINCT FROM plan_amount_raw
       OR decision_row.snapshot_observed_slot < decision_row.confirmed_slot
       OR decision_row.snapshot_liquidity_mint IS DISTINCT FROM plan_liquidity_mint THEN
        RAISE EXCEPTION
            'explicit Fleet repair: the four supplied identities do not describe one confirmed Autodeposit handoff';
    END IF;

    IF p_expected_pull_signature IS NOT NULL
       AND (
           execution_row.signature IS DISTINCT FROM p_expected_pull_signature
           OR execution_row.slot IS DISTINCT FROM p_expected_pull_slot
           OR decision_row.signature IS DISTINCT FROM p_expected_fleet_signature
           OR decision_row.confirmed_slot IS DISTINCT FROM p_expected_fleet_slot
           OR plan_liquidity_mint IS DISTINCT FROM p_expected_liquidity_mint
           OR plan_amount_raw IS DISTINCT FROM p_expected_amount_raw
           OR plan_wallet_token_ata IS DISTINCT FROM p_expected_wallet_token_ata
           OR plan_vault_token_ata IS DISTINCT FROM p_expected_vault_token_ata
           OR decision_row.target_reserve IS DISTINCT FROM p_expected_fleet_target_reserve
       ) THEN
        RAISE EXCEPTION
            'explicit Fleet repair: database evidence changed after chain verification';
    END IF;

    SELECT *
    INTO deposit_row
    FROM loyal_yield.user_yield_position_deposits
    WHERE deposit_signature = decision_row.signature
    FOR UPDATE;
    IF NOT FOUND
       OR deposit_row.confirmed_slot IS DISTINCT FROM decision_row.confirmed_slot
       OR deposit_row.settings IS DISTINCT FROM plan_settings
       OR deposit_row.vault_index IS DISTINCT FROM plan_vault_index
       OR deposit_row.vault_pubkey IS DISTINCT FROM plan_vault_pubkey
       OR deposit_row.wallet_address IS DISTINCT FROM plan_wallet
       OR deposit_row.principal_amount_raw IS DISTINCT FROM plan_amount_raw
       OR deposit_row.target_reserve IS DISTINCT FROM decision_row.target_reserve
       OR deposit_row.liquidity_mint IS DISTINCT FROM plan_liquidity_mint
       OR (
           deposit_row.balance_sweep_execution_id IS NOT NULL
           AND deposit_row.balance_sweep_execution_id <> p_execution_id
       )
       OR (
           deposit_row.balance_sweep_scheduled_slot_id IS NOT NULL
           AND deposit_row.balance_sweep_scheduled_slot_id <> p_scheduled_slot_id
       ) THEN
        RAISE EXCEPTION
            'explicit Fleet repair: Fleet decision % has no matching durable deposit',
            p_decision_id;
    END IF;

    SELECT count(*), max(id)
    INTO holding_count, holding_event_id
    FROM loyal_yield.user_yield_position_holding_events
    WHERE source_signature = decision_row.signature
      AND source_deposit_id = deposit_row.id
      AND source_rebalance_decision_id = decision_row.id
      AND source_snapshot_id = decision_row.post_snapshot_id;
    IF holding_count <> 1 THEN
        RAISE EXCEPTION
            'explicit Fleet repair: Fleet decision % has % matching holding events, expected one',
            p_decision_id,
            holding_count;
    END IF;

    SELECT *
    INTO holding_row
    FROM loyal_yield.user_yield_position_holding_events
    WHERE id = holding_event_id
    FOR UPDATE;
    IF holding_row.reserve IS DISTINCT FROM decision_row.target_reserve
       OR holding_row.liquidity_mint IS DISTINCT FROM plan_liquidity_mint
       OR holding_row.amount_raw IS DISTINCT FROM decision_row.snapshot_amount_raw
       OR holding_row.observed_slot IS DISTINCT FROM decision_row.snapshot_observed_slot THEN
        RAISE EXCEPTION
            'explicit Fleet repair: Fleet holding event conflicts with its confirmed snapshot';
    END IF;

    SELECT position.*
    INTO position_row
    FROM loyal_yield.user_yield_positions AS position
    JOIN loyal_yield.user_yield_position_holding_events AS holding
      ON holding.position_id = position.id
    WHERE holding.id = holding_event_id
    FOR UPDATE OF position;
    IF NOT FOUND
       OR position_row.settings IS DISTINCT FROM plan_settings
       OR position_row.vault_index IS DISTINCT FROM plan_vault_index
       OR position_row.vault_pubkey IS DISTINCT FROM plan_vault_pubkey
       OR position_row.wallet_address IS DISTINCT FROM plan_wallet
       OR position_row.status <> 'active' THEN
        RAISE EXCEPTION
            'explicit Fleet repair: Fleet holding history does not belong to the claim position';
    END IF;

    IF (claim_row.execution_id IS NOT NULL AND claim_row.execution_id <> p_execution_id)
       OR (slot_row.execution_id IS NOT NULL AND slot_row.execution_id <> p_execution_id)
       OR (
           execution_row.scheduled_slot_id IS NOT NULL
           AND execution_row.scheduled_slot_id <> p_scheduled_slot_id
       )
       OR (
           execution_row.yield_deposit_id IS NOT NULL
           AND execution_row.yield_deposit_id <> deposit_row.id
       )
       OR (
           execution_row.yield_position_id IS NOT NULL
           AND execution_row.yield_position_id <> position_row.id
       )
       OR (
           execution_row.kamino_deposit_signature IS NOT NULL
           AND execution_row.kamino_deposit_signature <> decision_row.signature
       )
       OR (
           execution_row.completed_at IS NOT NULL
           AND execution_row.kamino_deposit_signature IS DISTINCT FROM decision_row.signature
       )
       THEN
        RAISE EXCEPTION
            'explicit Fleet repair: conflicting durable linkage would be overwritten';
    END IF;

    IF (
           execution_row.decoded_evidence ? 'fleetDecisionId'
           AND execution_row.decoded_evidence ->> 'fleetDecisionId' <> decision_row.id::TEXT
       )
       OR (
           execution_row.decoded_evidence ? 'idleVaultDepositDecisionId'
           AND execution_row.decoded_evidence ->> 'idleVaultDepositDecisionId' <> decision_row.id::TEXT
       )
       OR (
           execution_row.decoded_evidence ? 'idleVaultLastDepositDecisionId'
           AND execution_row.decoded_evidence ->> 'idleVaultLastDepositDecisionId' <> decision_row.id::TEXT
       )
       OR (
           execution_row.decoded_evidence ? 'idleVaultLastDepositSignature'
           AND execution_row.decoded_evidence ->> 'idleVaultLastDepositSignature' <> decision_row.signature
       )
       OR (
           execution_row.decoded_evidence ? 'kaminoDepositSignature'
           AND execution_row.decoded_evidence ->> 'kaminoDepositSignature' <> decision_row.signature
       )
       OR (
           execution_row.decoded_evidence ? 'idleVaultLastDepositSlot'
           AND execution_row.decoded_evidence ->> 'idleVaultLastDepositSlot' <> decision_row.confirmed_slot::TEXT
       )
       OR (
           execution_row.decoded_evidence ? 'kaminoDepositSlot'
           AND execution_row.decoded_evidence ->> 'kaminoDepositSlot' <> decision_row.confirmed_slot::TEXT
       )
       OR (
           execution_row.decoded_evidence ? 'postSnapshotId'
           AND execution_row.decoded_evidence ->> 'postSnapshotId' <> decision_row.post_snapshot_id::TEXT
       )
       OR (
           execution_row.decoded_evidence ? 'idleVaultLastDepositAmountRaw'
           AND execution_row.decoded_evidence ->> 'idleVaultLastDepositAmountRaw' <> plan_amount_raw::TEXT
       )
       OR (
           execution_row.decoded_evidence ? 'idleVaultRecoveredAmountRaw'
           AND execution_row.decoded_evidence ->> 'idleVaultRecoveredAmountRaw' <> plan_amount_raw::TEXT
       )
       OR (
           execution_row.decoded_evidence ? 'idleVaultDepositAmountRaw'
           AND execution_row.decoded_evidence ->> 'idleVaultDepositAmountRaw' <> plan_amount_raw::TEXT
       )
       OR (
           execution_row.decoded_evidence ? 'recoverySource'
           AND execution_row.decoded_evidence ->> 'recoverySource' <> 'explicit_fleet_decision'
       )
       OR (
           execution_row.decoded_evidence ? 'status'
           AND execution_row.decoded_evidence ->> 'status' NOT IN (
               'partial_executed_pull_top_up_blocked',
               'partial_executed_pull_idle_vault_handoff',
               'partial_executed_pull_idle_vault_deposited',
               'executed'
           )
       ) THEN
        RAISE EXCEPTION
            'explicit Fleet repair: conflicting Fleet attribution would be overwritten';
    END IF;

    IF claim_row.status = 'executed' THEN
        IF claim_row.execution_id IS DISTINCT FROM p_execution_id
           OR slot_row.status <> 'executed'
           OR slot_row.execution_id IS DISTINCT FROM p_execution_id
           OR execution_row.yield_deposit_id IS DISTINCT FROM deposit_row.id
           OR execution_row.yield_position_id IS DISTINCT FROM position_row.id
           OR execution_row.kamino_deposit_signature IS DISTINCT FROM decision_row.signature
           OR execution_row.completed_at IS NULL
           OR deposit_row.balance_sweep_execution_id IS DISTINCT FROM p_execution_id
           OR deposit_row.balance_sweep_scheduled_slot_id IS DISTINCT FROM p_scheduled_slot_id THEN
            RAISE EXCEPTION
                'explicit Fleet repair: completed claim % has inconsistent linkage',
                p_claim_token;
        END IF;
        RETURN jsonb_build_object(
            'status', 'already_completed',
            'claimToken', p_claim_token,
            'executionId', p_execution_id,
            'scheduledSlotId', p_scheduled_slot_id,
            'decisionId', p_decision_id,
            'pullSignature', execution_row.signature,
            'pullSlot', execution_row.slot,
            'fleetSignature', decision_row.signature,
            'fleetSlot', decision_row.confirmed_slot,
            'liquidityMint', plan_liquidity_mint,
            'amountRaw', plan_amount_raw::TEXT,
            'walletTokenAta', plan_wallet_token_ata,
            'vaultTokenAta', plan_vault_token_ata,
            'fleetTargetReserve', decision_row.target_reserve
        );
    END IF;

    IF claim_row.status <> 'selected'
       OR slot_row.status <> 'selected'
       OR (
           claim_row.autodeposit_executor_lease_expires_at IS NOT NULL
           AND claim_row.autodeposit_executor_lease_expires_at > now()
       ) THEN
        RAISE EXCEPTION
            'explicit Fleet repair: claim % is not idle and selected',
            p_claim_token;
    END IF;

    IF NOT p_apply THEN
        RETURN jsonb_build_object(
            'status', 'ready',
            'claimToken', p_claim_token,
            'executionId', p_execution_id,
            'scheduledSlotId', p_scheduled_slot_id,
            'decisionId', p_decision_id,
            'pullSignature', execution_row.signature,
            'pullSlot', execution_row.slot,
            'fleetSignature', decision_row.signature,
            'fleetSlot', decision_row.confirmed_slot,
            'liquidityMint', plan_liquidity_mint,
            'amountRaw', plan_amount_raw::TEXT,
            'walletTokenAta', plan_wallet_token_ata,
            'vaultTokenAta', plan_vault_token_ata,
            'fleetTargetReserve', decision_row.target_reserve
        );
    END IF;

    UPDATE loyal_yield.user_yield_position_deposits
    SET balance_sweep_execution_id = p_execution_id,
        balance_sweep_scheduled_slot_id = p_scheduled_slot_id
    WHERE id = deposit_row.id
      AND (
          balance_sweep_execution_id IS NULL
          OR balance_sweep_execution_id = p_execution_id
      )
      AND (
          balance_sweep_scheduled_slot_id IS NULL
          OR balance_sweep_scheduled_slot_id = p_scheduled_slot_id
      );
    GET DIAGNOSTICS affected_rows = ROW_COUNT;
    IF affected_rows <> 1 THEN
        RAISE EXCEPTION
            'explicit Fleet repair: deposit is linked to another sweep operation';
    END IF;

    UPDATE loyal_yield.balance_sweep_executions
    SET scheduled_slot_id = p_scheduled_slot_id,
        yield_deposit_id = deposit_row.id,
        yield_position_id = position_row.id,
        kamino_deposit_signature = decision_row.signature,
        completed_at = COALESCE(completed_at, now()),
        completion_failure_code = NULL,
        decoded_evidence = COALESCE(decoded_evidence, '{}'::jsonb) || jsonb_build_object(
            'status', 'executed',
            'recoverySource', 'explicit_fleet_decision',
            'fleetDecisionId', decision_row.id::TEXT,
            'kaminoDepositSignature', decision_row.signature,
            'kaminoDepositSlot', decision_row.confirmed_slot::TEXT,
            'postSnapshotId', decision_row.post_snapshot_id::TEXT
        ),
        decoded_at = now()
    WHERE id = p_execution_id;

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
      AND status = 'selected';
    GET DIAGNOSTICS affected_rows = ROW_COUNT;
    IF affected_rows <> 1 THEN
        RAISE EXCEPTION
            'explicit Fleet repair: claim % changed before completion',
            p_claim_token;
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
            'explicit Fleet repair: scheduled slot % changed before completion',
            p_scheduled_slot_id;
    END IF;

    RETURN jsonb_build_object(
        'status', 'completed',
        'claimToken', p_claim_token,
        'executionId', p_execution_id,
        'scheduledSlotId', p_scheduled_slot_id,
        'decisionId', p_decision_id,
        'pullSignature', execution_row.signature,
        'pullSlot', execution_row.slot,
        'fleetSignature', decision_row.signature,
        'fleetSlot', decision_row.confirmed_slot,
        'liquidityMint', plan_liquidity_mint,
        'amountRaw', plan_amount_raw::TEXT,
        'walletTokenAta', plan_wallet_token_ata,
        'vaultTokenAta', plan_vault_token_ata,
        'fleetTargetReserve', decision_row.target_reserve
    );
END;
$$;

SELECT pg_temp.repair_autodeposit_fleet_handoff(
    :'claim_token',
    :'execution_id'::BIGINT,
    :'scheduled_slot_id'::BIGINT,
    :'decision_id'::BIGINT,
    NULLIF(:'expected_pull_signature', ''),
    NULLIF(:'expected_pull_slot', '')::BIGINT,
    NULLIF(:'expected_fleet_signature', ''),
    NULLIF(:'expected_fleet_slot', '')::BIGINT,
    NULLIF(:'expected_liquidity_mint', ''),
    NULLIF(:'expected_amount_raw', '')::BIGINT,
    NULLIF(:'expected_wallet_token_ata', ''),
    NULLIF(:'expected_vault_token_ata', ''),
    NULLIF(:'expected_fleet_target_reserve', ''),
    :'apply'::BOOLEAN
);
