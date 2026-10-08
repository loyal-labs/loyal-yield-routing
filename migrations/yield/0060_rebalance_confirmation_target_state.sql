-- Migration 0059 renamed balance_sweep_targets.active to desired_active.
-- Replace the migration 0018 realtime trigger function so a confirmed
-- rebalance no longer aborts while resolving its private event identity.
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
        ORDER BY
            (candidate.chain_status <> 'closed') DESC,
            candidate.chain_observation_slot DESC,
            candidate.last_seen_at DESC,
            candidate.id DESC
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
