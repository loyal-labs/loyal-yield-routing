\set ON_ERROR_STOP on

-- Run only against an isolated disposable PostgreSQL database after migrations
-- 1-17 have been applied. Every fixture and successful write is rolled back.
BEGIN;

DO $$
DECLARE
    v_position_id BIGINT;
    v_lower_id_event_id BIGINT;
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

    -- A retry of the older signature after a later top-up must validate and
    -- return without rewinding the position read model to the older event.
    SELECT *
    INTO v_result
    FROM loyal_yield.record_durable_autodeposit_yield_deposit(
        50,
        9100003,
        'ask1731-newer-deposit',
        7100,
        'ask1731-fault-mint',
        'ask1731-fault-market',
        NULL,
        7100,
        'ask1731-newer-deposit',
        9200003,
        'ask1731-fault-wallet',
        'ask1731-fault-vault',
        'ask1731-fault-settings',
        99,
        7,
        'ask1731-fault-policy',
        'ask1731-fault-reserve'
    );
    SELECT event.*
    INTO v_event
    FROM loyal_yield.user_yield_position_holding_events AS event
    WHERE event.source_signature = 'ask1731-newer-deposit';
    IF v_result.result_status <> 'inserted'
       OR v_event.amount_raw <> 1150
       OR v_event.observed_slot <> 7100 THEN
        RAISE EXCEPTION 'newer holding-event fixture did not persist';
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
    SELECT position.*
    INTO v_position
    FROM loyal_yield.user_yield_positions AS position
    WHERE position.id = v_position_id;
    IF v_result.result_status <> 'duplicate'
       OR v_position.principal_amount_raw <> 1150
       OR v_position.current_amount_raw <> 1150
       OR v_position.current_observed_slot <> 7100
       OR v_position.last_deposit_signature <> 'ask1731-newer-deposit'
       OR v_position.last_holding_event_id IS DISTINCT FROM v_event.id THEN
        RAISE EXCEPTION 'older duplicate retry rewound newer holding evidence';
    END IF;

    -- Sequence IDs are allocation order, not application order. Allocate a
    -- future holding event first, create the autodeposit event second, then
    -- apply/link the lower-ID event last. Retrying the autodeposit must preserve
    -- the lower-ID current linkage.
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
        created_at
    ) VALUES (
        v_position_id,
        'snapshot_reconciled',
        'ask1731-fault-reserve',
        'ask1731-fault-market',
        'ask1731-fault-mint',
        1250,
        0,
        100,
        7300,
        now(),
        'ask1731-lower-id-later-event',
        now()
    )
    RETURNING id INTO v_lower_id_event_id;

    SELECT *
    INTO v_result
    FROM loyal_yield.record_durable_autodeposit_yield_deposit(
        25,
        9100005,
        'ask1731-higher-id-older-deposit',
        7200,
        'ask1731-fault-mint',
        'ask1731-fault-market',
        NULL,
        7200,
        'ask1731-higher-id-older-deposit',
        9200005,
        'ask1731-fault-wallet',
        'ask1731-fault-vault',
        'ask1731-fault-settings',
        99,
        7,
        'ask1731-fault-policy',
        'ask1731-fault-reserve'
    );
    SELECT event.*
    INTO v_event
    FROM loyal_yield.user_yield_position_holding_events AS event
    WHERE event.source_signature = 'ask1731-higher-id-older-deposit';
    IF v_result.result_status <> 'inserted'
       OR v_event.id <= v_lower_id_event_id THEN
        RAISE EXCEPTION 'lower-ID ordering fixture was not allocated as intended';
    END IF;

    UPDATE loyal_yield.user_yield_positions
    SET
        current_amount_raw = 1250,
        current_observed_at = now(),
        current_observed_slot = 7300,
        last_confirmed_slot = 7300,
        last_deposit_signature = 'ask1731-lower-id-later-deposit',
        last_holding_event_id = v_lower_id_event_id,
        updated_at = now()
    WHERE id = v_position_id;

    SELECT *
    INTO v_result
    FROM loyal_yield.record_durable_autodeposit_yield_deposit(
        25,
        9100005,
        'ask1731-higher-id-older-deposit',
        7200,
        'ask1731-fault-mint',
        'ask1731-fault-market',
        NULL,
        7200,
        'ask1731-higher-id-older-deposit',
        9200005,
        'ask1731-fault-wallet',
        'ask1731-fault-vault',
        'ask1731-fault-settings',
        99,
        7,
        'ask1731-fault-policy',
        'ask1731-fault-reserve'
    );
    SELECT position.*
    INTO v_position
    FROM loyal_yield.user_yield_positions AS position
    WHERE position.id = v_position_id;
    IF v_result.result_status <> 'duplicate'
       OR v_position.current_amount_raw <> 1250
       OR v_position.current_observed_slot <> 7300
       OR v_position.last_deposit_signature <>
           'ask1731-lower-id-later-deposit'
       OR v_position.last_holding_event_id IS DISTINCT FROM
           v_lower_id_event_id THEN
        RAISE EXCEPTION 'higher-ID duplicate rewound lower-ID current evidence';
    END IF;

    -- Model an old worker dying after it inserted the first position but before
    -- its holding event. Repair must not add the initial principal/current
    -- amount a second time when the deposit signature is already on the row.
    SELECT *
    INTO v_result
    FROM loyal_yield.record_durable_autodeposit_yield_deposit(
        100,
        9100002,
        'ask1731-initial-repair-deposit',
        8000,
        'ask1731-initial-repair-mint',
        'ask1731-initial-repair-market',
        NULL,
        8000,
        'ask1731-initial-repair-deposit',
        9200002,
        'ask1731-initial-repair-wallet',
        'ask1731-initial-repair-vault',
        'ask1731-initial-repair-settings',
        100,
        8,
        'ask1731-initial-repair-policy',
        'ask1731-initial-repair-reserve'
    );
    IF v_result.result_status <> 'inserted' THEN
        RAISE EXCEPTION 'initial repair fixture did not insert';
    END IF;
    v_position_id := v_result.result_position_id;
    UPDATE loyal_yield.user_yield_positions
    SET last_holding_event_id = NULL
    WHERE id = v_position_id;
    DELETE FROM loyal_yield.user_yield_position_holding_events
    WHERE source_signature = 'ask1731-initial-repair-deposit';

    SELECT *
    INTO v_result
    FROM loyal_yield.record_durable_autodeposit_yield_deposit(
        100,
        9100002,
        'ask1731-initial-repair-deposit',
        8000,
        'ask1731-initial-repair-mint',
        'ask1731-initial-repair-market',
        NULL,
        8000,
        'ask1731-initial-repair-deposit',
        9200002,
        'ask1731-initial-repair-wallet',
        'ask1731-initial-repair-vault',
        'ask1731-initial-repair-settings',
        100,
        8,
        'ask1731-initial-repair-policy',
        'ask1731-initial-repair-reserve'
    );
    SELECT position.*
    INTO v_position
    FROM loyal_yield.user_yield_positions AS position
    WHERE position.id = v_position_id;
    SELECT event.*
    INTO v_event
    FROM loyal_yield.user_yield_position_holding_events AS event
    WHERE event.source_signature = 'ask1731-initial-repair-deposit';
    IF v_result.result_status <> 'duplicate'
       OR v_position.principal_amount_raw <> 100
       OR v_position.current_amount_raw <> 100
       OR v_position.last_holding_event_id IS DISTINCT FROM v_event.id
       OR v_event.event_type <> 'deposit_initialized'
       OR v_event.amount_raw <> 100
       OR v_event.principal_delta_raw <> 100
       OR v_event.holding_delta_raw <> 100
       OR (SELECT COUNT(*) FROM loyal_yield.user_yield_position_holding_events
           WHERE source_signature = 'ask1731-initial-repair-deposit') <> 1 THEN
        RAISE EXCEPTION 'legacy initial-position repair duplicated amount evidence';
    END IF;

    -- Once later position evidence exists, the now-eventless old deposit is
    -- ambiguous: it may or may not have been applied by a legacy worker. It
    -- must fail closed instead of being added to principal/current again.
    UPDATE loyal_yield.user_yield_positions
    SET last_holding_event_id = NULL
    WHERE id = v_position_id;
    DELETE FROM loyal_yield.user_yield_position_holding_events
    WHERE source_signature = 'ask1731-initial-repair-deposit';
    SELECT *
    INTO v_result
    FROM loyal_yield.record_durable_autodeposit_yield_deposit(
        50,
        9100004,
        'ask1731-initial-repair-newer',
        8100,
        'ask1731-initial-repair-mint',
        'ask1731-initial-repair-market',
        NULL,
        8100,
        'ask1731-initial-repair-newer',
        9200004,
        'ask1731-initial-repair-wallet',
        'ask1731-initial-repair-vault',
        'ask1731-initial-repair-settings',
        100,
        8,
        'ask1731-initial-repair-policy',
        'ask1731-initial-repair-reserve'
    );
    IF v_result.result_status <> 'inserted' THEN
        RAISE EXCEPTION 'ambiguous-partial newer fixture did not insert';
    END IF;

    BEGIN
        PERFORM *
        FROM loyal_yield.record_durable_autodeposit_yield_deposit(
            100,
            9100002,
            'ask1731-initial-repair-deposit',
            8000,
            'ask1731-initial-repair-mint',
            'ask1731-initial-repair-market',
            NULL,
            8000,
            'ask1731-initial-repair-deposit',
            9200002,
            'ask1731-initial-repair-wallet',
            'ask1731-initial-repair-vault',
            'ask1731-initial-repair-settings',
            100,
            8,
            'ask1731-initial-repair-policy',
            'ask1731-initial-repair-reserve'
        );
        RAISE EXCEPTION 'ambiguous legacy partial was applied automatically';
    EXCEPTION WHEN OTHERS THEN
        GET STACKED DIAGNOSTICS v_error_message = MESSAGE_TEXT;
        IF v_error_message <>
            'Legacy autodeposit deposit without matching position evidence requires manual reconciliation' THEN
            RAISE;
        END IF;
    END;
    SELECT position.*
    INTO v_position
    FROM loyal_yield.user_yield_positions AS position
    WHERE position.id = v_position_id;
    SELECT event.*
    INTO v_event
    FROM loyal_yield.user_yield_position_holding_events AS event
    WHERE event.source_signature = 'ask1731-initial-repair-newer';
    IF v_position.principal_amount_raw <> 150
       OR v_position.current_amount_raw <> 150
       OR v_position.current_observed_slot <> 8100
       OR v_position.last_deposit_signature <>
           'ask1731-initial-repair-newer'
       OR v_position.last_holding_event_id IS DISTINCT FROM v_event.id
       OR EXISTS (
           SELECT 1
           FROM loyal_yield.user_yield_position_holding_events
           WHERE source_signature = 'ask1731-initial-repair-deposit'
       ) THEN
        RAISE EXCEPTION 'ambiguous legacy partial changed newer position evidence';
    END IF;
END;
$$;

ROLLBACK;
