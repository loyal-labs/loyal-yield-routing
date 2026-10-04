-- Desired scheduling intent has its own revision; chain slots and setup
-- generations must not acknowledge an off-chain floor/pause change.
ALTER TABLE loyal_yield.balance_sweep_targets
    ADD COLUMN desired_revision BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN applied_desired_revision BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN applied_scheduling_eligible BOOLEAN NOT NULL DEFAULT FALSE,
    ADD CONSTRAINT balance_sweep_target_desired_revision_check
        CHECK (desired_revision > 0 AND applied_desired_revision >= 0
               AND applied_desired_revision <= desired_revision);

CREATE TABLE loyal_yield.autodeposit_desired_control_requests (
    target_id BIGINT PRIMARY KEY REFERENCES loyal_yield.balance_sweep_targets(id) ON DELETE CASCADE,
    requested_revision BIGINT NOT NULL CHECK (requested_revision > 0),
    processed_revision BIGINT NOT NULL DEFAULT 0,
    requested_generation BIGINT NOT NULL DEFAULT 1,
    processed_generation BIGINT NOT NULL DEFAULT 0,
    claim_owner TEXT,
    claim_expires_at TIMESTAMPTZ,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_error TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK (processed_revision >= 0 AND processed_revision <= requested_revision),
    CHECK (requested_generation > 0 AND processed_generation >= 0
           AND processed_generation <= requested_generation),
    CHECK ((claim_owner IS NULL) = (claim_expires_at IS NULL)),
    CHECK (claim_owner IS NULL OR length(claim_owner) > 0)
);

CREATE INDEX autodeposit_desired_control_pending_idx
    ON loyal_yield.autodeposit_desired_control_requests(next_attempt_at, target_id)
    WHERE requested_generation > processed_generation;

CREATE FUNCTION loyal_yield.advance_autodeposit_desired_revision()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.desired_revision := 1;
        NEW.applied_desired_revision := 0;
        NEW.applied_scheduling_eligible := FALSE;
    ELSE
        IF NEW.desired_active IS DISTINCT FROM OLD.desired_active
            OR NEW.wallet_balance_floor_raw IS DISTINCT FROM OLD.wallet_balance_floor_raw
            OR NEW.max_amount_per_period IS DISTINCT FROM OLD.max_amount_per_period
            OR NEW.cluster IS DISTINCT FROM OLD.cluster
            OR NEW.settings IS DISTINCT FROM OLD.settings
            OR NEW.authority IS DISTINCT FROM OLD.authority
            OR NEW.policy_seed IS DISTINCT FROM OLD.policy_seed
            OR NEW.policy_account IS DISTINCT FROM OLD.policy_account
            OR NEW.vault_index IS DISTINCT FROM OLD.vault_index
            OR NEW.vault_pubkey IS DISTINCT FROM OLD.vault_pubkey
            OR NEW.wallet IS DISTINCT FROM OLD.wallet
            OR NEW.token_mint IS DISTINCT FROM OLD.token_mint
            OR NEW.wallet_token_ata IS DISTINCT FROM OLD.wallet_token_ata
            OR NEW.wallet_usdc_ata IS DISTINCT FROM OLD.wallet_usdc_ata
            OR NEW.vault_token_ata IS DISTINCT FROM OLD.vault_token_ata
            OR NEW.vault_usdc_ata IS DISTINCT FROM OLD.vault_usdc_ata
            OR NEW.subscription_authority IS DISTINCT FROM OLD.subscription_authority
            OR NEW.recurring_delegation IS DISTINCT FROM OLD.recurring_delegation
            OR NEW.recurring_delegation_nonce IS DISTINCT FROM OLD.recurring_delegation_nonce
            OR NEW.recurring_delegation_expiry_timestamp IS DISTINCT FROM OLD.recurring_delegation_expiry_timestamp
            OR NEW.period_length_seconds IS DISTINCT FROM OLD.period_length_seconds
            OR NEW.start_timestamp IS DISTINCT FROM OLD.start_timestamp
            OR NEW.setup_generation IS DISTINCT FROM OLD.setup_generation THEN
            NEW.desired_revision := OLD.desired_revision + 1;
        ELSE
            NEW.desired_revision := OLD.desired_revision;
        END IF;
        IF NEW.applied_desired_revision < OLD.applied_desired_revision THEN
            RAISE EXCEPTION 'Autodeposit applied desired revision cannot regress';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER balance_sweep_target_desired_revision
BEFORE INSERT OR UPDATE ON loyal_yield.balance_sweep_targets
FOR EACH ROW EXECUTE FUNCTION loyal_yield.advance_autodeposit_desired_revision();

CREATE FUNCTION loyal_yield.enqueue_autodeposit_desired_control()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.desired_revision = OLD.desired_revision
        AND NEW.chain_status IS NOT DISTINCT FROM OLD.chain_status THEN
        RETURN NEW;
    END IF;
    INSERT INTO loyal_yield.autodeposit_desired_control_requests
        (target_id, requested_revision)
    VALUES (NEW.id, NEW.desired_revision)
    ON CONFLICT (target_id) DO UPDATE SET
        requested_revision = EXCLUDED.requested_revision,
        requested_generation = loyal_yield.autodeposit_desired_control_requests.requested_generation + 1,
        requested_at = clock_timestamp(),
        next_attempt_at = clock_timestamp(),
        updated_at = clock_timestamp();
    -- An in-flight proof retains its owner. Its captured generation may be
    -- acknowledged only; a newer revision/generation remains pending.
    RETURN NEW;
END;
$$;

CREATE TRIGGER balance_sweep_target_desired_control_request
AFTER INSERT OR UPDATE ON loyal_yield.balance_sweep_targets
FOR EACH ROW EXECUTE FUNCTION loyal_yield.enqueue_autodeposit_desired_control();

-- Deliberately reconcile existing known intent after branch activation. NULL
-- floors remain unknown, and historical lost desired intent is never guessed.
INSERT INTO loyal_yield.autodeposit_desired_control_requests(target_id, requested_revision)
SELECT id, desired_revision FROM loyal_yield.balance_sweep_targets
WHERE chain_status <> 'closed';

COMMENT ON COLUMN loyal_yield.balance_sweep_targets.applied_scheduling_eligible IS
    'Last engine-applied scheduling eligibility; does not imply chain or creator proof.';
COMMENT ON TABLE loyal_yield.autodeposit_desired_control_requests IS
    'Family-owned desired-intent and eligibility coalescing. Engine rebaseline and exact generation acknowledgement commit together.';
