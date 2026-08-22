CREATE TABLE loyal_yield.autodeposit_vault_configs (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL CHECK (cluster IN ('mainnet-beta', 'devnet')),
    settings TEXT NOT NULL,
    wallet TEXT NOT NULL,
    vault_index SMALLINT NOT NULL CHECK (vault_index = 1),
    vault_pubkey TEXT NOT NULL,
    desired_active BOOLEAN NOT NULL DEFAULT TRUE,
    wallet_balance_floor_raw BIGINT NOT NULL CHECK (wallet_balance_floor_raw >= 0),
    expected_policy_account TEXT NOT NULL,
    expected_subscription_authority TEXT NOT NULL,
    expected_recurring_delegation TEXT NOT NULL,
    observation_start_slot BIGINT NOT NULL CHECK (observation_start_slot >= 0),
    generation BIGINT NOT NULL DEFAULT 1 CHECK (generation > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (cluster, settings, wallet, vault_index)
);

CREATE TABLE loyal_yield.autodeposit_chain_projections (
    config_id BIGINT PRIMARY KEY REFERENCES loyal_yield.autodeposit_vault_configs(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('pending', 'active', 'closed', 'inconsistent')),
    policy_valid BOOLEAN NOT NULL,
    subscription_authority_valid BOOLEAN NOT NULL,
    recurring_delegation_valid BOOLEAN NOT NULL,
    token_delegate_valid BOOLEAN NOT NULL,
    observation_complete BOOLEAN NOT NULL,
    observation_slot BIGINT NOT NULL CHECK (observation_slot >= 0),
    bootstrap_generation BIGINT,
    reason TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION loyal_yield.effective_autodeposit_active(p_config_id BIGINT)
RETURNS BOOLEAN
LANGUAGE sql
STABLE
AS $$
    SELECT config.desired_active AND projection.status = 'active'
    FROM loyal_yield.autodeposit_vault_configs AS config
    JOIN loyal_yield.autodeposit_chain_projections AS projection
      ON projection.config_id = config.id
    WHERE config.id = p_config_id
$$;

CREATE OR REPLACE FUNCTION loyal_yield.notify_autodeposit_watch_set_changed()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    UPDATE loyal_yield.balance_sweep_targets
    SET wallet_balance_floor_raw = NEW.wallet_balance_floor_raw,
        active = COALESCE(loyal_yield.effective_autodeposit_active(NEW.id), FALSE),
        last_seen_at = now()
    WHERE settings = NEW.settings
      AND wallet = NEW.wallet
      AND vault_index = NEW.vault_index;
    PERFORM pg_notify('loyal_yield_autodeposit_watch', NEW.id::text);
    RETURN NEW;
END;
$$;

CREATE TRIGGER autodeposit_vault_configs_watch_set_changed
AFTER INSERT OR UPDATE OF expected_policy_account, expected_subscription_authority,
    expected_recurring_delegation, observation_start_slot
ON loyal_yield.autodeposit_vault_configs
FOR EACH ROW EXECUTE FUNCTION loyal_yield.notify_autodeposit_watch_set_changed();

CREATE OR REPLACE FUNCTION loyal_yield.emit_autodeposit_projection_changed()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    config_row loyal_yield.autodeposit_vault_configs%ROWTYPE;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.status = OLD.status
       AND NEW.observation_slot = OLD.observation_slot THEN
        RETURN NEW;
    END IF;

    SELECT * INTO config_row
    FROM loyal_yield.autodeposit_vault_configs
    WHERE id = NEW.config_id;

    UPDATE loyal_yield.balance_sweep_targets
    SET wallet_balance_floor_raw = config_row.wallet_balance_floor_raw,
        active = config_row.desired_active AND NEW.status = 'active',
        last_seen_at = now()
    WHERE settings = config_row.settings
      AND wallet = config_row.wallet
      AND vault_index = config_row.vault_index;

    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.autodeposit.changed',
        p_scope => 'earn',
        p_reason => NEW.status,
        p_solana_env => config_row.cluster,
        p_wallet_address => config_row.wallet,
        p_settings_pda => config_row.settings,
        p_smart_account_address => config_row.vault_pubkey,
        p_vault_pubkey => config_row.vault_pubkey,
        p_source_table => 'autodeposit_chain_projections',
        p_source_id => NEW.config_id::text,
        p_payload => jsonb_build_object(
            'status', NEW.status,
            'desiredActive', config_row.desired_active,
            'generation', config_row.generation,
            'observationSlot', NEW.observation_slot
        )
    );
    RETURN NEW;
END;
$$;

CREATE TRIGGER autodeposit_chain_projection_changed
AFTER INSERT OR UPDATE ON loyal_yield.autodeposit_chain_projections
FOR EACH ROW EXECUTE FUNCTION loyal_yield.emit_autodeposit_projection_changed();

COMMENT ON TABLE loyal_yield.autodeposit_vault_configs IS
    'Authenticated user intent and exact accounts to observe; never inferred from chain activity.';
COMMENT ON TABLE loyal_yield.autodeposit_chain_projections IS
    'Current finalized on-chain Autodeposit snapshot; never written by the client.';
