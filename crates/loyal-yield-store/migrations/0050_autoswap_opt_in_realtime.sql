-- Policy identity and risk settings are authoritative in
-- cross_mint_swap_policies. Keep the old columns nullable for a safe rolling
-- deploy, but new code neither reads nor writes them.
ALTER TABLE loyal_yield.cross_mint_vault_opt_ins
    ALTER COLUMN classic_policy_account DROP NOT NULL,
    ALTER COLUMN classic_policy_seed DROP NOT NULL,
    ALTER COLUMN token_2022_policy_account DROP NOT NULL,
    ALTER COLUMN token_2022_policy_seed DROP NOT NULL,
    ALTER COLUMN max_slippage_bps DROP NOT NULL,
    ALTER COLUMN daily_source_mint_spending_cap DROP NOT NULL;

COMMENT ON TABLE loyal_yield.cross_mint_vault_opt_ins IS
    'Per-vault Autoswap run/pause intent. Policy identity and risk settings live only in the finalized on-chain policy projection.';

COMMENT ON COLUMN loyal_yield.cross_mint_vault_opt_ins.classic_policy_account IS
    'Deprecated compatibility column; policy identity is read from cross_mint_swap_policies.';
COMMENT ON COLUMN loyal_yield.cross_mint_vault_opt_ins.classic_policy_seed IS
    'Deprecated compatibility column; policy identity is read from cross_mint_swap_policies.';
COMMENT ON COLUMN loyal_yield.cross_mint_vault_opt_ins.token_2022_policy_account IS
    'Deprecated compatibility column; policy identity is read from cross_mint_swap_policies.';
COMMENT ON COLUMN loyal_yield.cross_mint_vault_opt_ins.token_2022_policy_seed IS
    'Deprecated compatibility column; policy identity is read from cross_mint_swap_policies.';
COMMENT ON COLUMN loyal_yield.cross_mint_vault_opt_ins.max_slippage_bps IS
    'Deprecated compatibility column; risk settings are read from cross_mint_swap_policies.';
COMMENT ON COLUMN loyal_yield.cross_mint_vault_opt_ins.daily_source_mint_spending_cap IS
    'Deprecated compatibility column; risk settings are read from cross_mint_swap_policies.';

CREATE OR REPLACE FUNCTION loyal_yield.emit_cross_mint_opt_in_realtime_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    opt_in_row RECORD;
    wallet_address TEXT;
    event_reason TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        opt_in_row := OLD;
        event_reason := 'autoswap_removed';
    ELSE
        opt_in_row := NEW;
        event_reason := CASE
            WHEN TG_OP = 'INSERT' THEN 'autoswap_installed'
            WHEN NEW.enabled IS DISTINCT FROM OLD.enabled AND NEW.enabled
                THEN 'autoswap_resumed'
            WHEN NEW.enabled IS DISTINCT FROM OLD.enabled AND NOT NEW.enabled
                THEN 'autoswap_paused'
            ELSE NULL
        END;
    END IF;

    IF event_reason IS NULL THEN
        RETURN COALESCE(NEW, OLD);
    END IF;

    -- Fleet verifiers and local fixtures use isolated synthetic cluster names.
    -- Private realtime events are valid only for clusters served to clients.
    IF opt_in_row.cluster NOT IN ('mainnet-beta', 'devnet') THEN
        RETURN COALESCE(NEW, OLD);
    END IF;

    SELECT policy.authority
    INTO wallet_address
    FROM loyal_yield.cross_mint_swap_policies AS policy
    WHERE policy.cluster = opt_in_row.cluster
      AND policy.settings = opt_in_row.settings
      AND policy.vault_index = opt_in_row.vault_index
      AND policy.vault_pubkey = opt_in_row.vault_pubkey
      AND policy.authority <> ''
    ORDER BY policy.last_seen_slot DESC, policy.id DESC
    LIMIT 1;

    IF wallet_address IS NULL THEN
        RETURN COALESCE(NEW, OLD);
    END IF;

    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.autoswap.configuration.changed',
        p_scope => 'earn',
        p_reason => event_reason,
        p_solana_env => opt_in_row.cluster,
        p_wallet_address => wallet_address,
        p_settings_pda => opt_in_row.settings,
        p_smart_account_address => opt_in_row.vault_pubkey,
        p_vault_pubkey => opt_in_row.vault_pubkey,
        p_source_table => 'cross_mint_vault_opt_ins',
        p_source_id => concat_ws(
            ':',
            opt_in_row.cluster,
            opt_in_row.settings,
            opt_in_row.vault_index::text,
            opt_in_row.vault_pubkey
        ),
        p_payload => jsonb_build_object(
            'enabled', CASE WHEN TG_OP = 'DELETE' THEN false ELSE NEW.enabled END,
            'generation', opt_in_row.generation
        )
    );

    RETURN COALESCE(NEW, OLD);
END;
$$;

DROP TRIGGER IF EXISTS cross_mint_vault_opt_ins_realtime_event
    ON loyal_yield.cross_mint_vault_opt_ins;

CREATE TRIGGER cross_mint_vault_opt_ins_realtime_event
AFTER INSERT OR UPDATE OR DELETE ON loyal_yield.cross_mint_vault_opt_ins
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.emit_cross_mint_opt_in_realtime_event();
