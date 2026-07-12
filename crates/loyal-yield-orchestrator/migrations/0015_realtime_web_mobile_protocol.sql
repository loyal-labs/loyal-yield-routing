-- Live autodeposit transactions read executions before targets. Acquire the
-- two DDL locks in that same order so the forward migration waits cleanly
-- instead of deadlocking with a request/claim cycle.
LOCK TABLE loyal_yield.balance_sweep_executions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE loyal_yield.balance_sweep_targets IN ACCESS EXCLUSIVE MODE;

ALTER TABLE loyal_yield.realtime_events
    ADD COLUMN IF NOT EXISTS schema_version SMALLINT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS earn_vault_address TEXT,
    ADD COLUMN IF NOT EXISTS failure_code TEXT,
    ADD COLUMN IF NOT EXISTS deliverable BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE loyal_yield.balance_sweep_targets
    ADD COLUMN IF NOT EXISTS cluster TEXT;

ALTER TABLE loyal_yield.balance_sweep_targets
    DROP CONSTRAINT IF EXISTS balance_sweep_targets_cluster_check,
    ADD CONSTRAINT balance_sweep_targets_cluster_check
        CHECK (cluster IS NULL OR cluster IN ('mainnet-beta', 'devnet')) NOT VALID;
ALTER TABLE loyal_yield.balance_sweep_targets
    VALIDATE CONSTRAINT balance_sweep_targets_cluster_check;

ALTER TABLE loyal_yield.realtime_events
    DROP CONSTRAINT IF EXISTS realtime_events_schema_version_check,
    ADD CONSTRAINT realtime_events_schema_version_check
        CHECK (schema_version = 1) NOT VALID,
    DROP CONSTRAINT IF EXISTS realtime_events_failure_code_check,
    ADD CONSTRAINT realtime_events_failure_code_check
        CHECK (
            failure_code IS NULL
            OR failure_code ~ '^[a-z0-9_]{1,64}$'
        ) NOT VALID,
    DROP CONSTRAINT IF EXISTS realtime_events_deliverable_identity_check,
    ADD CONSTRAINT realtime_events_deliverable_identity_check
        CHECK (
            NOT deliverable
            OR NOT loyal_yield.realtime_private_scope_requires_identity(scope, event_type)
            OR (
                solana_env IN ('mainnet-beta', 'devnet')
                AND wallet_address IS NOT NULL
                AND settings_pda IS NOT NULL
                AND earn_vault_address IS NOT NULL
            )
        ) NOT VALID;

CREATE TABLE IF NOT EXISTS loyal_yield.realtime_configuration (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    solana_env TEXT NOT NULL CHECK (solana_env IN ('mainnet-beta', 'devnet')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Derive the database environment only when existing target data has exactly
-- one known cluster. A mixed or uninitialized branch is left unconfigured and
-- private triggers skip emission instead of creating a cross-cluster row.
INSERT INTO loyal_yield.realtime_configuration (singleton, solana_env)
SELECT TRUE, MIN(cluster)
FROM loyal_yield.balance_sweep_targets
WHERE NULLIF(cluster, '') IS NOT NULL
HAVING COUNT(DISTINCT cluster) = 1
ON CONFLICT (singleton) DO UPDATE
SET solana_env = EXCLUDED.solana_env,
    updated_at = now();

UPDATE loyal_yield.balance_sweep_targets AS target
SET cluster = config.solana_env
FROM loyal_yield.realtime_configuration AS config
WHERE config.singleton
  AND NULLIF(target.cluster, '') IS NULL;

UPDATE loyal_yield.realtime_events AS event
SET wallet_address = COALESCE(event.wallet_address, target.wallet),
    settings_pda = COALESCE(event.settings_pda, target.settings),
    earn_vault_address = COALESCE(event.earn_vault_address, target.vault_pubkey),
    smart_account_address = COALESCE(
        event.smart_account_address,
        event.vault_pubkey,
        target.vault_pubkey
    ),
    vault_pubkey = COALESCE(event.vault_pubkey, target.vault_pubkey),
    solana_env = COALESCE(event.solana_env, NULLIF(target.cluster, ''))
FROM loyal_yield.balance_sweep_targets AS target
WHERE event.target_id = target.id;

DO $$
BEGIN
    IF to_regclass('loyal_yield.user_yield_positions') IS NOT NULL THEN
        EXECUTE $sql$
            UPDATE loyal_yield.realtime_events AS event
            SET wallet_address = COALESCE(event.wallet_address, position.wallet_address),
                settings_pda = COALESCE(event.settings_pda, position.settings),
                earn_vault_address = COALESCE(event.earn_vault_address, position.vault_pubkey),
                smart_account_address = COALESCE(
                    event.smart_account_address,
                    position.smart_account_address,
                    position.vault_pubkey
                ),
                vault_pubkey = COALESCE(event.vault_pubkey, position.vault_pubkey),
                solana_env = COALESCE(event.solana_env, target.cluster, config.solana_env)
            FROM loyal_yield.user_yield_positions AS position
            LEFT JOIN loyal_yield.balance_sweep_targets AS target
              ON target.settings = position.settings
             AND target.wallet = position.wallet_address
             AND target.vault_pubkey = position.vault_pubkey
            LEFT JOIN loyal_yield.realtime_configuration AS config
              ON config.singleton
            WHERE event.source_table = 'user_yield_positions'
              AND event.source_id = position.id::text
        $sql$;
    END IF;

    IF to_regclass('loyal_yield.user_yield_position_holding_events') IS NOT NULL
       AND to_regclass('loyal_yield.user_yield_positions') IS NOT NULL THEN
        EXECUTE $sql$
            UPDATE loyal_yield.realtime_events AS event
            SET wallet_address = COALESCE(event.wallet_address, position.wallet_address),
                settings_pda = COALESCE(event.settings_pda, position.settings),
                earn_vault_address = COALESCE(event.earn_vault_address, position.vault_pubkey),
                smart_account_address = COALESCE(
                    event.smart_account_address,
                    position.smart_account_address,
                    position.vault_pubkey
                ),
                vault_pubkey = COALESCE(event.vault_pubkey, position.vault_pubkey),
                solana_env = COALESCE(event.solana_env, target.cluster, config.solana_env)
            FROM loyal_yield.user_yield_position_holding_events AS holding
            JOIN loyal_yield.user_yield_positions AS position
              ON position.id = holding.position_id
            LEFT JOIN loyal_yield.balance_sweep_targets AS target
              ON target.settings = position.settings
             AND target.wallet = position.wallet_address
             AND target.vault_pubkey = position.vault_pubkey
            LEFT JOIN loyal_yield.realtime_configuration AS config
              ON config.singleton
            WHERE event.source_table = 'user_yield_position_holding_events'
              AND event.source_id = holding.id::text
        $sql$;
    END IF;
END $$;

-- Old smart_account_address values are intentionally not promoted into the
-- new Earn-vault identity: historical producers used that name ambiguously.
-- Rows without an authoritative target/position join remain non-deliverable
-- and therefore require a client resync instead of risking a false match.
UPDATE loyal_yield.realtime_events AS event
SET solana_env = COALESCE(event.solana_env, config.solana_env)
FROM loyal_yield.realtime_configuration AS config
WHERE config.singleton
  AND event.solana_env IS NULL;

UPDATE loyal_yield.realtime_events
SET deliverable = (
    wallet_address IS NOT NULL
    AND settings_pda IS NOT NULL
    AND earn_vault_address IS NOT NULL
    AND solana_env IN ('mainnet-beta', 'devnet')
    AND event_type NOT IN (
        'autodeposit_slot_changed',
        'earn.autodeposit.sweep_requested',
        'earn.autodeposit.sweep_selected',
        'earn.autodeposit.sweep_executed'
    )
)
WHERE deliverable IS DISTINCT FROM (
    wallet_address IS NOT NULL
    AND settings_pda IS NOT NULL
    AND earn_vault_address IS NOT NULL
    AND solana_env IN ('mainnet-beta', 'devnet')
    AND event_type NOT IN (
        'autodeposit_slot_changed',
        'earn.autodeposit.sweep_requested',
        'earn.autodeposit.sweep_selected',
        'earn.autodeposit.sweep_executed'
    )
);

CREATE INDEX IF NOT EXISTS realtime_events_private_replay_idx
    ON loyal_yield.realtime_events (
        solana_env,
        wallet_address,
        settings_pda,
        earn_vault_address,
        scope,
        id
    )
    WHERE deliverable = TRUE;

CREATE INDEX IF NOT EXISTS realtime_events_retention_idx
    ON loyal_yield.realtime_events (created_at, id);

CREATE INDEX IF NOT EXISTS realtime_events_autodeposit_latency_idx
    ON loyal_yield.realtime_events (scheduled_slot_id, created_at)
    WHERE deliverable = TRUE
      AND event_type = 'earn.autodeposit.execution.changed';

ALTER TABLE loyal_yield.realtime_events
    VALIDATE CONSTRAINT realtime_events_schema_version_check;
ALTER TABLE loyal_yield.realtime_events
    VALIDATE CONSTRAINT realtime_events_failure_code_check;
ALTER TABLE loyal_yield.realtime_events
    VALIDATE CONSTRAINT realtime_events_deliverable_identity_check;

ALTER TABLE loyal_yield.balance_sweep_executions
    ADD COLUMN IF NOT EXISTS scheduled_slot_id BIGINT,
    ADD COLUMN IF NOT EXISTS yield_deposit_id BIGINT,
    ADD COLUMN IF NOT EXISTS yield_position_id BIGINT,
    ADD COLUMN IF NOT EXISTS kamino_deposit_signature TEXT,
    ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS completion_failure_code TEXT;

ALTER TABLE loyal_yield.balance_sweep_executions
    DROP CONSTRAINT IF EXISTS balance_sweep_executions_completion_failure_code_check,
    ADD CONSTRAINT balance_sweep_executions_completion_failure_code_check
        CHECK (
            completion_failure_code IS NULL
            OR completion_failure_code ~ '^[a-z0-9_]{1,64}$'
        ) NOT VALID;
ALTER TABLE loyal_yield.balance_sweep_executions
    VALIDATE CONSTRAINT balance_sweep_executions_completion_failure_code_check;

CREATE INDEX IF NOT EXISTS balance_sweep_executions_scheduled_slot_idx
    ON loyal_yield.balance_sweep_executions (scheduled_slot_id)
    WHERE scheduled_slot_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS balance_sweep_executions_completion_idx
    ON loyal_yield.balance_sweep_executions (completed_at, id);

DO $$
BEGIN
    IF to_regclass('loyal_yield.user_yield_position_deposits') IS NOT NULL THEN
        EXECUTE 'ALTER TABLE loyal_yield.user_yield_position_deposits
            ADD COLUMN IF NOT EXISTS balance_sweep_execution_id BIGINT,
            ADD COLUMN IF NOT EXISTS balance_sweep_scheduled_slot_id BIGINT';
        EXECUTE 'CREATE INDEX IF NOT EXISTS user_yield_position_deposits_sweep_execution_idx
            ON loyal_yield.user_yield_position_deposits (balance_sweep_execution_id)
            WHERE balance_sweep_execution_id IS NOT NULL';
    END IF;
END $$;

CREATE OR REPLACE FUNCTION loyal_yield.emit_realtime_event(
    p_event_type TEXT,
    p_scope TEXT,
    p_reason TEXT,
    p_solana_env TEXT DEFAULT NULL,
    p_wallet_address TEXT DEFAULT NULL,
    p_settings_pda TEXT DEFAULT NULL,
    p_smart_account_address TEXT DEFAULT NULL,
    p_vault_pubkey TEXT DEFAULT NULL,
    p_target_id BIGINT DEFAULT NULL,
    p_scheduled_slot_id BIGINT DEFAULT NULL,
    p_execution_id BIGINT DEFAULT NULL,
    p_source_table TEXT DEFAULT NULL,
    p_source_id TEXT DEFAULT NULL,
    p_payload JSONB DEFAULT '{}'::jsonb
)
RETURNS BIGINT
LANGUAGE plpgsql
AS $$
DECLARE
    inserted_event_id BIGINT;
    resolved_earn_vault_address TEXT;
    resolved_failure_code TEXT;
BEGIN
    resolved_earn_vault_address := COALESCE(
        NULLIF(p_smart_account_address, ''),
        NULLIF(p_vault_pubkey, '')
    );
    resolved_failure_code := NULLIF(COALESCE(p_payload, '{}'::jsonb)->>'failureCode', '');

    IF resolved_failure_code IS NOT NULL
       AND resolved_failure_code !~ '^[a-z0-9_]{1,64}$' THEN
        RAISE EXCEPTION 'invalid realtime failure code' USING ERRCODE = '23514';
    END IF;

    IF loyal_yield.realtime_private_scope_requires_identity(p_scope, p_event_type)
       AND (
           NULLIF(p_solana_env, '') IS NULL
           OR p_solana_env NOT IN ('mainnet-beta', 'devnet')
           OR NULLIF(p_wallet_address, '') IS NULL
           OR NULLIF(p_settings_pda, '') IS NULL
           OR resolved_earn_vault_address IS NULL
       ) THEN
        RAISE EXCEPTION
            'private realtime event %.% requires exact wallet, settings, earn vault, and cluster identity',
            p_scope,
            p_event_type
            USING ERRCODE = '23514';
    END IF;

    INSERT INTO loyal_yield.realtime_events (
        schema_version,
        event_type,
        scope,
        reason,
        solana_env,
        wallet_address,
        settings_pda,
        smart_account_address,
        earn_vault_address,
        vault_pubkey,
        target_id,
        scheduled_slot_id,
        execution_id,
        source_table,
        source_id,
        payload,
        failure_code,
        deliverable
    )
    VALUES (
        1,
        p_event_type,
        p_scope,
        p_reason,
        p_solana_env,
        p_wallet_address,
        p_settings_pda,
        resolved_earn_vault_address,
        resolved_earn_vault_address,
        COALESCE(NULLIF(p_vault_pubkey, ''), resolved_earn_vault_address),
        p_target_id,
        p_scheduled_slot_id,
        p_execution_id,
        p_source_table,
        p_source_id,
        COALESCE(p_payload, '{}'::jsonb),
        resolved_failure_code,
        TRUE
    )
    RETURNING id INTO inserted_event_id;

    PERFORM pg_notify(
        'loyal_yield_realtime',
        json_build_object('event_id', inserted_event_id)::text
    );

    RETURN inserted_event_id;
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.emit_autodeposit_scheduled_slot_realtime_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_row RECORD;
    event_state TEXT;
    safe_failure_code TEXT;
BEGIN
    -- Slot linkage and bookkeeping updates are not progress transitions. Emitting
    -- here only when the state changes keeps each canonical state unique while
    -- the later pull_confirmed event introduces the execution id.
    IF TG_OP = 'UPDATE'
       AND NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;

    SELECT
        target.settings,
        target.wallet,
        target.vault_pubkey,
        COALESCE(NULLIF(target.cluster, ''), config.solana_env) AS solana_env
    INTO target_row
    FROM loyal_yield.balance_sweep_targets AS target
    LEFT JOIN loyal_yield.realtime_configuration AS config
      ON config.singleton
    WHERE target.id = NEW.target_id;

    IF target_row.solana_env IS NULL THEN
        RETURN NEW;
    END IF;

    event_state := CASE NEW.status::text
        WHEN 'scheduled' THEN 'scheduled'
        WHEN 'requested' THEN 'requested'
        WHEN 'selected' THEN 'selected'
        WHEN 'failed' THEN 'failed'
        WHEN 'canceled' THEN 'canceled'
        WHEN 'released' THEN 'released'
        ELSE NULL
    END;
    IF event_state IS NULL THEN
        RETURN NEW;
    END IF;

    safe_failure_code := CASE
        WHEN NEW.status::text <> 'failed' THEN NULL
        WHEN COALESCE(NEW.last_error, '') ILIKE '%route policy%' THEN 'route_policy_missing'
        WHEN COALESCE(NEW.last_error, '') ILIKE '%timed out%' THEN 'request_timeout'
        WHEN COALESCE(NEW.last_error, '') ILIKE '%stale%' THEN 'stale_claim'
        WHEN COALESCE(NEW.last_error, '') ILIKE '%pull%' THEN 'pull_failed'
        ELSE 'execution_failed'
    END;

    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.autodeposit.execution.changed',
        p_scope => 'autodeposit',
        p_reason => event_state,
        p_solana_env => target_row.solana_env,
        p_wallet_address => target_row.wallet,
        p_settings_pda => target_row.settings,
        p_smart_account_address => target_row.vault_pubkey,
        p_vault_pubkey => target_row.vault_pubkey,
        p_target_id => NEW.target_id,
        p_scheduled_slot_id => NEW.id,
        p_execution_id => NEW.execution_id,
        p_source_table => 'balance_sweep_scheduled_slots',
        p_source_id => NEW.id::text,
        p_payload => jsonb_strip_nulls(jsonb_build_object(
            'requestSource', NEW.request_source,
            'failureCode', safe_failure_code
        ))
    );

    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.emit_autodeposit_execution_realtime_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_row RECORD;
    resolved_scheduled_slot_id BIGINT;
BEGIN
    SELECT
        target.settings,
        target.wallet,
        target.vault_pubkey,
        COALESCE(NULLIF(target.cluster, ''), config.solana_env) AS solana_env
    INTO target_row
    FROM loyal_yield.balance_sweep_targets AS target
    LEFT JOIN loyal_yield.realtime_configuration AS config
      ON config.singleton
    WHERE target.id = NEW.target_id;

    SELECT slot.id
    INTO resolved_scheduled_slot_id
    FROM loyal_yield.balance_sweep_scheduled_slots AS slot
    LEFT JOIN loyal_yield.balance_sweep_lot_claims AS claim
      ON claim.claim_token = slot.claim_token
    WHERE slot.execution_id = NEW.id
       OR (
          slot.target_id = NEW.target_id
          AND slot.status = 'selected'
          AND claim.status = 'selected'
          AND claim.execution_id IS NULL
       )
    ORDER BY
        CASE WHEN slot.execution_id = NEW.id THEN 0 ELSE 1 END,
        slot.updated_at DESC,
        slot.id DESC
    LIMIT 1;

    UPDATE loyal_yield.balance_sweep_executions
    SET scheduled_slot_id = resolved_scheduled_slot_id
    WHERE id = NEW.id;

    IF target_row.solana_env IS NULL OR resolved_scheduled_slot_id IS NULL THEN
        RETURN NEW;
    END IF;

    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.autodeposit.execution.changed',
        p_scope => 'autodeposit',
        p_reason => 'pull_confirmed',
        p_solana_env => target_row.solana_env,
        p_wallet_address => target_row.wallet,
        p_settings_pda => target_row.settings,
        p_smart_account_address => target_row.vault_pubkey,
        p_vault_pubkey => target_row.vault_pubkey,
        p_target_id => NEW.target_id,
        p_scheduled_slot_id => resolved_scheduled_slot_id,
        p_execution_id => NEW.id,
        p_source_table => 'balance_sweep_executions',
        p_source_id => NEW.id::text,
        p_payload => '{}'::jsonb
    );

    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.transaction.recorded',
        p_scope => 'earn',
        p_reason => 'autodeposit_pull_confirmed',
        p_solana_env => target_row.solana_env,
        p_wallet_address => target_row.wallet,
        p_settings_pda => target_row.settings,
        p_smart_account_address => target_row.vault_pubkey,
        p_vault_pubkey => target_row.vault_pubkey,
        p_target_id => NEW.target_id,
        p_scheduled_slot_id => resolved_scheduled_slot_id,
        p_execution_id => NEW.id,
        p_source_table => 'balance_sweep_executions',
        p_source_id => NEW.id::text,
        p_payload => '{}'::jsonb
    );

    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.mark_autodeposit_execution_completed(
    p_execution_id BIGINT,
    p_scheduled_slot_id BIGINT,
    p_kamino_deposit_signature TEXT
)
RETURNS BOOLEAN
LANGUAGE plpgsql
AS $$
DECLARE
    execution_row RECORD;
    deposit_row RECORD;
    target_row RECORD;
BEGIN
    SELECT * INTO execution_row
    FROM loyal_yield.balance_sweep_executions
    WHERE id = p_execution_id
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'unknown balance sweep execution' USING ERRCODE = '23503';
    END IF;
    IF execution_row.completed_at IS NOT NULL THEN
        RETURN FALSE;
    END IF;

    SELECT deposit.id, holding.position_id
    INTO deposit_row
    FROM loyal_yield.user_yield_position_deposits AS deposit
    JOIN loyal_yield.user_yield_position_holding_events AS holding
      ON holding.source_deposit_id = deposit.id
     AND holding.source_signature = deposit.deposit_signature
    WHERE deposit.deposit_signature = p_kamino_deposit_signature
    ORDER BY holding.id DESC
    LIMIT 1;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'yield deposit and position persistence must exist before completion'
            USING ERRCODE = '23514';
    END IF;

    UPDATE loyal_yield.user_yield_position_deposits
    SET balance_sweep_execution_id = p_execution_id,
        balance_sweep_scheduled_slot_id = p_scheduled_slot_id
    WHERE id = deposit_row.id
      AND (
          balance_sweep_execution_id IS NULL
          OR balance_sweep_execution_id = p_execution_id
      );
    IF NOT FOUND THEN
        RAISE EXCEPTION 'yield deposit is linked to another sweep execution'
            USING ERRCODE = '23514';
    END IF;

    UPDATE loyal_yield.balance_sweep_executions
    SET scheduled_slot_id = p_scheduled_slot_id,
        yield_deposit_id = deposit_row.id,
        yield_position_id = deposit_row.position_id,
        kamino_deposit_signature = p_kamino_deposit_signature,
        completed_at = now(),
        completion_failure_code = NULL
    WHERE id = p_execution_id;

    SELECT
        target.settings,
        target.wallet,
        target.vault_pubkey,
        COALESCE(NULLIF(target.cluster, ''), config.solana_env) AS solana_env
    INTO target_row
    FROM loyal_yield.balance_sweep_targets AS target
    LEFT JOIN loyal_yield.realtime_configuration AS config
      ON config.singleton
    WHERE target.id = execution_row.target_id;

    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.autodeposit.execution.changed',
        p_scope => 'autodeposit',
        p_reason => 'completed',
        p_solana_env => target_row.solana_env,
        p_wallet_address => target_row.wallet,
        p_settings_pda => target_row.settings,
        p_smart_account_address => target_row.vault_pubkey,
        p_vault_pubkey => target_row.vault_pubkey,
        p_target_id => execution_row.target_id,
        p_scheduled_slot_id => p_scheduled_slot_id,
        p_execution_id => p_execution_id,
        p_source_table => 'balance_sweep_executions',
        p_source_id => p_execution_id::text,
        p_payload => '{}'::jsonb
    );

    RETURN TRUE;
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.mark_autodeposit_execution_failed(
    p_execution_id BIGINT,
    p_scheduled_slot_id BIGINT,
    p_failure_code TEXT
)
RETURNS BOOLEAN
LANGUAGE plpgsql
AS $$
DECLARE
    execution_row RECORD;
    target_row RECORD;
BEGIN
    IF p_failure_code !~ '^[a-z0-9_]{1,64}$' THEN
        RAISE EXCEPTION 'invalid autodeposit failure code' USING ERRCODE = '23514';
    END IF;

    UPDATE loyal_yield.balance_sweep_executions
    SET scheduled_slot_id = p_scheduled_slot_id,
        completion_failure_code = p_failure_code
    WHERE id = p_execution_id
      AND completed_at IS NULL
      AND completion_failure_code IS NULL
    RETURNING * INTO execution_row;
    IF NOT FOUND THEN
        RETURN FALSE;
    END IF;

    SELECT
        target.settings,
        target.wallet,
        target.vault_pubkey,
        COALESCE(NULLIF(target.cluster, ''), config.solana_env) AS solana_env
    INTO target_row
    FROM loyal_yield.balance_sweep_targets AS target
    LEFT JOIN loyal_yield.realtime_configuration AS config
      ON config.singleton
    WHERE target.id = execution_row.target_id;

    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.autodeposit.execution.changed',
        p_scope => 'autodeposit',
        p_reason => 'failed',
        p_solana_env => target_row.solana_env,
        p_wallet_address => target_row.wallet,
        p_settings_pda => target_row.settings,
        p_smart_account_address => target_row.vault_pubkey,
        p_vault_pubkey => target_row.vault_pubkey,
        p_target_id => execution_row.target_id,
        p_scheduled_slot_id => p_scheduled_slot_id,
        p_execution_id => p_execution_id,
        p_source_table => 'balance_sweep_executions',
        p_source_id => p_execution_id::text,
        p_payload => jsonb_build_object('failureCode', p_failure_code)
    );

    RETURN TRUE;
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.emit_user_yield_position_realtime_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    event_reason TEXT;
    resolved_solana_env TEXT;
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.principal_amount_raw IS NOT DISTINCT FROM OLD.principal_amount_raw
       AND NEW.current_reserve IS NOT DISTINCT FROM OLD.current_reserve
       AND NEW.current_liquidity_mint IS NOT DISTINCT FROM OLD.current_liquidity_mint
       AND NEW.current_amount_raw IS NOT DISTINCT FROM OLD.current_amount_raw
       AND NEW.last_holding_event_id IS NOT DISTINCT FROM OLD.last_holding_event_id
       AND NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;

    SELECT COALESCE(NULLIF(target.cluster, ''), config.solana_env)
    INTO resolved_solana_env
    FROM loyal_yield.realtime_configuration AS config
    LEFT JOIN loyal_yield.balance_sweep_targets AS target
      ON target.settings = NEW.settings
     AND target.wallet = NEW.wallet_address
     AND target.vault_pubkey = NEW.vault_pubkey
    WHERE config.singleton
    LIMIT 1;
    IF resolved_solana_env IS NULL THEN
        RETURN NEW;
    END IF;

    event_reason := CASE TG_OP WHEN 'INSERT' THEN 'position_created' ELSE 'position_updated' END;
    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.position.changed',
        p_scope => 'earn',
        p_reason => event_reason,
        p_solana_env => resolved_solana_env,
        p_wallet_address => NEW.wallet_address,
        p_settings_pda => NEW.settings,
        p_smart_account_address => NEW.vault_pubkey,
        p_vault_pubkey => NEW.vault_pubkey,
        p_source_table => 'user_yield_positions',
        p_source_id => NEW.id::text,
        p_payload => '{}'::jsonb
    );
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.emit_user_yield_holding_event_realtime_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    position_row RECORD;
BEGIN
    SELECT
        position.wallet_address,
        position.settings,
        position.vault_pubkey,
        COALESCE(NULLIF(target.cluster, ''), config.solana_env) AS solana_env
    INTO position_row
    FROM loyal_yield.user_yield_positions AS position
    LEFT JOIN loyal_yield.balance_sweep_targets AS target
      ON target.settings = position.settings
     AND target.wallet = position.wallet_address
     AND target.vault_pubkey = position.vault_pubkey
    LEFT JOIN loyal_yield.realtime_configuration AS config
      ON config.singleton
    WHERE position.id = NEW.position_id;
    IF position_row.solana_env IS NULL THEN
        RETURN NEW;
    END IF;

    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.transaction.recorded',
        p_scope => 'earn',
        p_reason => 'holding_event_' || NEW.event_type::text,
        p_solana_env => position_row.solana_env,
        p_wallet_address => position_row.wallet_address,
        p_settings_pda => position_row.settings,
        p_smart_account_address => position_row.vault_pubkey,
        p_vault_pubkey => position_row.vault_pubkey,
        p_source_table => 'user_yield_position_holding_events',
        p_source_id => NEW.id::text,
        p_payload => '{}'::jsonb
    );
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.emit_earn_onboarding_realtime_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    event_reason TEXT;
    resolved_solana_env TEXT;
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.status IS NOT DISTINCT FROM OLD.status
       AND NEW.last_error_code IS NOT DISTINCT FROM OLD.last_error_code
       AND NEW.deposit_signature IS NOT DISTINCT FROM OLD.deposit_signature THEN
        RETURN NEW;
    END IF;

    SELECT COALESCE(NULLIF(target.cluster, ''), config.solana_env)
    INTO resolved_solana_env
    FROM loyal_yield.realtime_configuration AS config
    LEFT JOIN loyal_yield.balance_sweep_targets AS target
      ON target.settings = NEW.settings
     AND target.wallet = NEW.wallet_address
     AND target.vault_pubkey = NEW.vault_pubkey
    WHERE config.singleton
    LIMIT 1;
    IF resolved_solana_env IS NULL THEN
        RETURN NEW;
    END IF;

    event_reason := CASE
        WHEN TG_OP = 'INSERT' THEN 'onboarding_started'
        WHEN NEW.status IS DISTINCT FROM OLD.status THEN 'onboarding_status_changed'
        WHEN NEW.last_error_code IS DISTINCT FROM OLD.last_error_code THEN 'onboarding_error_changed'
        ELSE 'onboarding_updated'
    END;
    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.onboarding.changed',
        p_scope => 'earn',
        p_reason => event_reason,
        p_solana_env => resolved_solana_env,
        p_wallet_address => NEW.wallet_address,
        p_settings_pda => NEW.settings,
        p_smart_account_address => NEW.vault_pubkey,
        p_vault_pubkey => NEW.vault_pubkey,
        p_source_table => 'earn_deposit_onboarding_attempts',
        p_source_id => NEW.id::text,
        p_payload => jsonb_strip_nulls(jsonb_build_object(
            'failureCode', CASE
                WHEN NULLIF(NEW.last_error_code, '') IS NULL THEN NULL
                WHEN NEW.last_error_code ~ '^[a-z0-9_]{1,64}$' THEN NEW.last_error_code
                ELSE 'onboarding_failed'
            END
        ))
    );
    RETURN NEW;
END;
$$;
