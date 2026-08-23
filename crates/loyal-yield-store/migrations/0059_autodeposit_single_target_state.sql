ALTER TABLE loyal_yield.balance_sweep_targets
    RENAME COLUMN active TO desired_active;

ALTER TABLE loyal_yield.balance_sweep_targets
    RENAME COLUMN lifecycle_status TO chain_status;

ALTER TABLE loyal_yield.balance_sweep_targets
    ADD COLUMN IF NOT EXISTS chain_observation_slot BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS setup_generation BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS bootstrap_generation BIGINT,
    ADD COLUMN IF NOT EXISTS subscription_authority TEXT,
    ADD COLUMN IF NOT EXISTS recurring_delegation_nonce BIGINT,
    ADD COLUMN IF NOT EXISTS recurring_delegation_expiry_timestamp BIGINT,
    ADD COLUMN IF NOT EXISTS policy_signature TEXT,
    ADD COLUMN IF NOT EXISTS policy_confirmed_slot BIGINT,
    ADD COLUMN IF NOT EXISTS recurring_delegation_signature TEXT,
    ADD COLUMN IF NOT EXISTS recurring_delegation_confirmed_slot BIGINT,
    ADD COLUMN IF NOT EXISTS close_signature TEXT,
    ADD COLUMN IF NOT EXISTS close_slot BIGINT,
    ADD COLUMN IF NOT EXISTS closed_at TIMESTAMPTZ;

CREATE SEQUENCE IF NOT EXISTS loyal_yield.autodeposit_bootstrap_event_id_seq
    AS BIGINT START WITH -1 INCREMENT BY -1 MAXVALUE -1 MINVALUE -9223372036854775807;

UPDATE loyal_yield.balance_sweep_targets AS target
SET desired_active = config.desired_active,
    wallet_balance_floor_raw = config.wallet_balance_floor_raw,
    chain_status = COALESCE(projection.status, target.chain_status),
    chain_observation_slot = GREATEST(
        target.last_seen_slot,
        COALESCE(projection.observation_slot, 0)
    ),
    setup_generation = config.generation,
    bootstrap_generation = projection.bootstrap_generation,
    subscription_authority = config.expected_subscription_authority,
    recurring_delegation = COALESCE(
        target.recurring_delegation,
        config.expected_recurring_delegation
    )
FROM loyal_yield.autodeposit_vault_configs AS config
LEFT JOIN loyal_yield.autodeposit_chain_projections AS projection
  ON projection.config_id = config.id
WHERE target.settings = config.settings
  AND target.wallet = config.wallet
  AND target.vault_index = config.vault_index;

DROP TRIGGER IF EXISTS autodeposit_chain_projection_changed
    ON loyal_yield.autodeposit_chain_projections;
DROP TRIGGER IF EXISTS autodeposit_vault_configs_watch_set_changed
    ON loyal_yield.autodeposit_vault_configs;
DROP FUNCTION IF EXISTS loyal_yield.emit_autodeposit_projection_changed();
DROP FUNCTION IF EXISTS loyal_yield.notify_autodeposit_watch_set_changed();
DROP FUNCTION IF EXISTS loyal_yield.effective_autodeposit_active(BIGINT);
DROP TABLE loyal_yield.autodeposit_chain_projections;
DROP TABLE loyal_yield.autodeposit_vault_configs;

-- A wallet has one current Autodeposit target. Keep the newest observation as
-- current and retain older rows only as closed history before enforcing it.
WITH ranked_current AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY settings, wallet, vault_index, token_mint
               ORDER BY chain_observation_slot DESC, last_seen_slot DESC, id DESC
           ) AS rank
    FROM loyal_yield.balance_sweep_targets
    WHERE chain_status <> 'closed'
)
UPDATE loyal_yield.balance_sweep_targets AS target
SET chain_status = 'closed',
    desired_active = FALSE,
    closed_at = COALESCE(closed_at, now())
FROM ranked_current AS ranked
WHERE target.id = ranked.id
  AND ranked.rank > 1;

ALTER TABLE loyal_yield.balance_sweep_targets
    DROP CONSTRAINT IF EXISTS balance_sweep_targets_chain_status_check,
    ADD CONSTRAINT balance_sweep_targets_chain_status_check
      CHECK (chain_status IN ('pending', 'active', 'closed', 'inconsistent'));

DROP INDEX IF EXISTS loyal_yield.balance_sweep_targets_active_wallet_ata_idx;
DROP INDEX IF EXISTS loyal_yield.balance_sweep_targets_active_wallet_token_ata_idx;
CREATE INDEX balance_sweep_targets_effective_wallet_token_ata_idx
    ON loyal_yield.balance_sweep_targets
      (desired_active, chain_status, token_mint, wallet_token_ata);
CREATE UNIQUE INDEX balance_sweep_targets_one_current_wallet_idx
    ON loyal_yield.balance_sweep_targets
      (settings, wallet, vault_index, token_mint)
    WHERE chain_status <> 'closed';

CREATE OR REPLACE FUNCTION loyal_yield.emit_autodeposit_configuration_realtime_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    event_reason TEXT;
    resolved_solana_env TEXT;
BEGIN
    event_reason := CASE
        WHEN NEW.chain_status = 'active'
             AND (TG_OP = 'INSERT' OR OLD.chain_status IS DISTINCT FROM NEW.chain_status)
            THEN 'allowance_created'
        WHEN TG_OP = 'UPDATE'
             AND NEW.chain_status = 'closed'
             AND OLD.chain_status IS DISTINCT FROM NEW.chain_status
            THEN 'allowance_removed'
        WHEN TG_OP = 'UPDATE'
             AND NEW.chain_status = 'inconsistent'
             AND OLD.chain_status IS DISTINCT FROM NEW.chain_status
            THEN 'allowance_inconsistent'
        WHEN TG_OP = 'UPDATE'
             AND NEW.chain_status = 'active'
             AND NEW.desired_active IS DISTINCT FROM OLD.desired_active
            THEN CASE WHEN NEW.desired_active THEN 'allowance_resumed' ELSE 'allowance_paused' END
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
        SELECT config.solana_env INTO resolved_solana_env
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
        p_payload => jsonb_build_object(
            'chainStatus', NEW.chain_status,
            'desiredActive', NEW.desired_active,
            'observationSlot', NEW.chain_observation_slot,
            'setupGeneration', NEW.setup_generation
        )
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

COMMENT ON COLUMN loyal_yield.balance_sweep_targets.desired_active IS
    'Authenticated off-chain user scheduling intent. Chain observers never overwrite it.';
COMMENT ON COLUMN loyal_yield.balance_sweep_targets.chain_status IS
    'Finalized on-chain Autodeposit lifecycle projected by loyal-yield-routing.';
