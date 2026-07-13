\set ON_ERROR_STOP on

-- Run only against an isolated disposable PostgreSQL database after migrations
-- 1-17 have been applied. Every fixture and successful write is rolled back.
BEGIN;

DO $$
DECLARE
    v_position_id BIGINT;
    v_fault_step TEXT;
    v_error_message TEXT;
    v_result RECORD;
    v_position RECORD;
    v_event RECORD;
    v_deposit RECORD;
BEGIN
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
        'ask1731-fault-wallet',
        'ask1731-fault-vault',
        'ask1731-fault-settings',
        99,
        'ask1731-fault-vault',
        7,
        'ask1731-fault-policy',
        7,
        'ask1731-fault-reserve',
        'ask1731-fault-market',
        'ask1731-fault-mint',
        NULL,
        'ask1731-fault-mint',
        1000,
        'ask1731-fault-reserve',
        'ask1731-fault-market',
        'ask1731-fault-mint',
        1000,
        6000,
        now(),
        'ask1731-existing-deposit',
        'ask1731-existing-deposit',
        6000,
        'active',
        now(),
        now()
    )
    RETURNING id INTO v_position_id;

    FOREACH v_fault_step IN ARRAY ARRAY[
        'after_deposit_insert',
        'after_existing_position_update',
        'after_holding_event_insert',
        'after_final_linkage_update'
    ] LOOP
        PERFORM set_config(
            'loyal_yield.test_autodeposit_persistence_fault',
            v_fault_step,
            true
        );
        BEGIN
            PERFORM *
            FROM loyal_yield.record_durable_autodeposit_yield_deposit(
                100,
                9100001,
                'ask1731-fault-deposit',
                7000,
                'ask1731-fault-mint',
                'ask1731-fault-market',
                NULL,
                7000,
                'ask1731-fault-deposit',
                9200001,
                'ask1731-fault-wallet',
                'ask1731-fault-vault',
                'ask1731-fault-settings',
                99,
                7,
                'ask1731-fault-policy',
                'ask1731-fault-reserve'
            );
            RAISE EXCEPTION 'expected fault injection % was not raised', v_fault_step;
        EXCEPTION WHEN OTHERS THEN
            GET STACKED DIAGNOSTICS v_error_message = MESSAGE_TEXT;
            IF v_error_message NOT LIKE
                'autodeposit persistence fault injection:%' THEN
                RAISE;
            END IF;
        END;
        PERFORM set_config(
            'loyal_yield.test_autodeposit_persistence_fault',
            '',
            true
        );

        IF EXISTS (
            SELECT 1
            FROM loyal_yield.user_yield_position_deposits
            WHERE deposit_signature = 'ask1731-fault-deposit'
        ) THEN
            RAISE EXCEPTION 'deposit survived rollback at %', v_fault_step;
        END IF;
        IF EXISTS (
            SELECT 1
            FROM loyal_yield.user_yield_position_holding_events
            WHERE source_signature = 'ask1731-fault-deposit'
        ) THEN
            RAISE EXCEPTION 'holding event survived rollback at %', v_fault_step;
        END IF;
        SELECT position.*
        INTO v_position
        FROM loyal_yield.user_yield_positions AS position
        WHERE position.id = v_position_id;
        IF v_position.principal_amount_raw <> 1000
           OR v_position.current_amount_raw <> 1000
           OR v_position.last_deposit_signature <> 'ask1731-existing-deposit'
           OR v_position.last_holding_event_id IS NOT NULL THEN
            RAISE EXCEPTION 'position survived partial write at %', v_fault_step;
        END IF;
    END LOOP;

    SELECT *
    INTO v_result
    FROM loyal_yield.record_durable_autodeposit_yield_deposit(
        100,
        9100001,
        'ask1731-fault-deposit',
        7000,
        'ask1731-fault-mint',
        'ask1731-fault-market',
        NULL,
        7000,
        'ask1731-fault-deposit',
        9200001,
        'ask1731-fault-wallet',
        'ask1731-fault-vault',
        'ask1731-fault-settings',
        99,
        7,
        'ask1731-fault-policy',
        'ask1731-fault-reserve'
    );
    IF v_result.result_status <> 'inserted'
       OR v_result.result_position_id <> v_position_id THEN
        RAISE EXCEPTION 'successful atomic persistence returned wrong linkage';
    END IF;

    SELECT position.*
    INTO v_position
    FROM loyal_yield.user_yield_positions AS position
    WHERE position.id = v_position_id;
    SELECT event.*
    INTO v_event
    FROM loyal_yield.user_yield_position_holding_events AS event
    WHERE event.source_signature = 'ask1731-fault-deposit';
    SELECT deposit.*
    INTO v_deposit
    FROM loyal_yield.user_yield_position_deposits AS deposit
    WHERE deposit.deposit_signature = 'ask1731-fault-deposit';

    IF v_position.principal_amount_raw <> 1100
       OR v_position.current_amount_raw <> 1100
       OR v_position.last_deposit_signature <> 'ask1731-fault-deposit'
       OR v_position.last_holding_event_id IS DISTINCT FROM v_event.id
       OR v_event.amount_raw <> 1100
       OR v_event.principal_delta_raw <> 100
       OR v_event.observed_slot <> 7000
       OR v_event.source_deposit_id IS DISTINCT FROM v_deposit.id
       OR v_deposit.balance_sweep_execution_id <> 9100001
       OR v_deposit.balance_sweep_scheduled_slot_id <> 9200001 THEN
        RAISE EXCEPTION 'successful atomic persistence has mismatched evidence';
    END IF;

    SELECT *
    INTO v_result
    FROM loyal_yield.record_durable_autodeposit_yield_deposit(
        100,
        9100001,
        'ask1731-fault-deposit',
        7000,
        'ask1731-fault-mint',
        'ask1731-fault-market',
        NULL,
        7000,
        'ask1731-fault-deposit',
        9200001,
        'ask1731-fault-wallet',
        'ask1731-fault-vault',
        'ask1731-fault-settings',
        99,
        7,
        'ask1731-fault-policy',
        'ask1731-fault-reserve'
    );
    IF v_result.result_status <> 'duplicate'
       OR (SELECT COUNT(*) FROM loyal_yield.user_yield_position_deposits
           WHERE deposit_signature = 'ask1731-fault-deposit') <> 1
       OR (SELECT COUNT(*) FROM loyal_yield.user_yield_position_holding_events
           WHERE source_signature = 'ask1731-fault-deposit') <> 1 THEN
        RAISE EXCEPTION 'idempotent persistence retry duplicated application evidence';
    END IF;
END;
$$;

ROLLBACK;
