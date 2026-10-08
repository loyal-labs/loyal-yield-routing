CREATE OR REPLACE FUNCTION loyal_yield.emit_autodeposit_configuration_realtime_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    event_reason TEXT;
    resolved_solana_env TEXT;
BEGIN
    event_reason := CASE
        WHEN NEW.lifecycle_status = 'active'
             AND (
                 TG_OP = 'INSERT'
                 OR OLD.lifecycle_status IS DISTINCT FROM NEW.lifecycle_status
             ) THEN 'allowance_created'
        WHEN TG_OP = 'UPDATE'
             AND NEW.lifecycle_status = 'closed'
             AND OLD.lifecycle_status IS DISTINCT FROM NEW.lifecycle_status
            THEN 'allowance_removed'
        WHEN TG_OP = 'UPDATE'
             AND NEW.lifecycle_status = 'active'
             AND NEW.active IS DISTINCT FROM OLD.active
            THEN CASE WHEN NEW.active THEN 'allowance_resumed' ELSE 'allowance_paused' END
        WHEN TG_OP = 'UPDATE'
             AND NEW.wallet_balance_floor_raw IS DISTINCT FROM OLD.wallet_balance_floor_raw
            THEN 'allowance_updated'
        ELSE NULL
    END;

    IF event_reason IS NULL THEN
        RETURN NEW;
    END IF;

    resolved_solana_env := NULLIF(NEW.cluster, '');
    IF resolved_solana_env IS NULL THEN
        SELECT config.solana_env
        INTO resolved_solana_env
        FROM loyal_yield.realtime_configuration AS config
        WHERE config.singleton;
    END IF;

    IF resolved_solana_env IS NULL THEN
        RETURN NEW;
    END IF;

    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.autodeposit.configuration.changed',
        p_scope => 'autodeposit',
        p_reason => event_reason,
        p_solana_env => resolved_solana_env,
        p_wallet_address => NEW.wallet,
        p_settings_pda => NEW.settings,
        p_smart_account_address => NEW.vault_pubkey,
        p_vault_pubkey => NEW.vault_pubkey,
        p_target_id => NEW.id,
        p_source_table => 'balance_sweep_targets',
        p_source_id => NEW.id::text,
        p_payload => '{}'::jsonb
    );

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS balance_sweep_targets_configuration_realtime_event
    ON loyal_yield.balance_sweep_targets;

CREATE TRIGGER balance_sweep_targets_configuration_realtime_event
AFTER INSERT OR UPDATE ON loyal_yield.balance_sweep_targets
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.emit_autodeposit_configuration_realtime_event();

CREATE OR REPLACE FUNCTION loyal_yield.emit_rebalance_confirmation_realtime_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    identity_row RECORD;
BEGIN
    IF NEW.status::text <> 'confirmed'
       OR NEW.post_snapshot_id IS NULL THEN
        RETURN NEW;
    END IF;

    IF TG_OP = 'UPDATE'
       AND OLD.status::text = 'confirmed'
       AND OLD.post_snapshot_id IS NOT NULL THEN
        RETURN NEW;
    END IF;

    SELECT
        policy.authority AS wallet_address,
        vault.settings,
        vault.vault_pubkey,
        COALESCE(NULLIF(target.cluster, ''), config.solana_env) AS solana_env
    INTO identity_row
    FROM loyal_yield.managed_vaults AS vault
    JOIN loyal_yield.route_policies AS policy
      ON policy.id = vault.active_policy_id
    LEFT JOIN LATERAL (
        SELECT candidate.cluster
        FROM loyal_yield.balance_sweep_targets AS candidate
        WHERE candidate.settings = vault.settings
          AND candidate.wallet = policy.authority
          AND candidate.vault_pubkey = vault.vault_pubkey
        ORDER BY candidate.active DESC, candidate.last_seen_at DESC, candidate.id DESC
        LIMIT 1
    ) AS target ON TRUE
    LEFT JOIN loyal_yield.realtime_configuration AS config
      ON config.singleton
    WHERE vault.id = NEW.vault_id;

    IF identity_row.wallet_address IS NULL
       OR identity_row.solana_env IS NULL THEN
        RETURN NEW;
    END IF;

    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.rebalance.confirmed',
        p_scope => 'earn',
        p_reason => 'rebalance_confirmed',
        p_solana_env => identity_row.solana_env,
        p_wallet_address => identity_row.wallet_address,
        p_settings_pda => identity_row.settings,
        p_smart_account_address => identity_row.vault_pubkey,
        p_vault_pubkey => identity_row.vault_pubkey,
        p_source_table => 'rebalance_decisions',
        p_source_id => NEW.id::text,
        p_payload => '{}'::jsonb
    );

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_decisions_confirmation_realtime_event
    ON loyal_yield.rebalance_decisions;

CREATE TRIGGER rebalance_decisions_confirmation_realtime_event
AFTER INSERT OR UPDATE ON loyal_yield.rebalance_decisions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.emit_rebalance_confirmation_realtime_event();
