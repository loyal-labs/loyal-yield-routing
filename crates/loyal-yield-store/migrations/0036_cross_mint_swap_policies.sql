-- One catalog row per immutable generalized swap policy account.
ALTER TABLE loyal_yield.route_policies
    ADD COLUMN IF NOT EXISTS cluster TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS source_commitment TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS finalized_eligible BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE loyal_yield.rebalance_decisions
    ADD COLUMN IF NOT EXISTS cross_mint_preflight_certification JSONB;

ALTER TABLE loyal_yield.rebalance_decisions
    DROP CONSTRAINT IF EXISTS rebalance_decisions_cross_mint_preflight_certification_check;

ALTER TABLE loyal_yield.rebalance_decisions
    ADD CONSTRAINT rebalance_decisions_cross_mint_preflight_certification_check CHECK (
        cross_mint_preflight_certification IS NULL
        OR (
            movement_route = 'cross_mint_jupiter'
            AND jsonb_typeof(cross_mint_preflight_certification) = 'object'
            AND cross_mint_preflight_certification <> '{}'::jsonb
        )
    );

COMMENT ON COLUMN loyal_yield.rebalance_decisions.cross_mint_preflight_certification IS
    'Fresh Jupiter build, finalized policy readback, packet-fit, and simulation evidence admitted atomically before withdrawal.';

ALTER TABLE loyal_yield.route_policies
    DROP CONSTRAINT IF EXISTS route_policies_source_commitment_check;

ALTER TABLE loyal_yield.route_policies
    ADD CONSTRAINT route_policies_source_commitment_check CHECK (
        source_commitment IN ('unknown', 'processed', 'confirmed', 'finalized')
    );

ALTER TABLE loyal_yield.route_policies
    DROP CONSTRAINT IF EXISTS route_policies_finalized_eligible_check;

ALTER TABLE loyal_yield.route_policies
    ADD CONSTRAINT route_policies_finalized_eligible_check CHECK (
        NOT finalized_eligible
        OR (active AND source_commitment = 'finalized')
    );

CREATE TABLE IF NOT EXISTS loyal_yield.cross_mint_swap_policies (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    settings TEXT NOT NULL,
    authority TEXT NOT NULL,
    policy_seed BIGINT,
    policy_account TEXT NOT NULL,
    vault_index SMALLINT,
    vault_pubkey TEXT,
    delegated_signer TEXT,
    source_shard TEXT,
    max_slippage_bps INTEGER,
    daily_source_mint_spending_cap BIGINT,
    manifest_fingerprint TEXT,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    start_eligible BOOLEAN NOT NULL DEFAULT FALSE,
    last_mutation TEXT NOT NULL,
    source_commitment TEXT NOT NULL,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_slot BIGINT NOT NULL,
    last_seen_signature TEXT NOT NULL,
    UNIQUE (cluster, policy_account),
    CONSTRAINT cross_mint_swap_policies_identity_check CHECK (
        cluster <> ''
        AND settings <> ''
        AND authority <> ''
        AND policy_account <> ''
        AND (vault_pubkey IS NULL OR vault_pubkey <> '')
        AND (delegated_signer IS NULL OR delegated_signer <> '')
        AND (manifest_fingerprint IS NULL OR (
            manifest_fingerprint = btrim(manifest_fingerprint)
            AND manifest_fingerprint <> ''
        ))
        AND last_seen_signature <> ''
    ),
    CONSTRAINT cross_mint_swap_policies_shape_check CHECK (
        (
            last_mutation = 'remove'
            AND NOT active
            AND NOT start_eligible
        ) OR (
            vault_index IS NOT NULL
            AND vault_pubkey IS NOT NULL
            AND delegated_signer IS NOT NULL
            AND source_shard IN ('classic', 'token_2022')
            AND max_slippage_bps BETWEEN 1 AND 10000
            AND daily_source_mint_spending_cap > 0
            AND manifest_fingerprint IS NOT NULL
        )
    ),
    CONSTRAINT cross_mint_swap_policies_observation_check CHECK (
        last_mutation IN ('create', 'update', 'remove', 'ambiguous')
        AND source_commitment IN ('processed', 'confirmed', 'finalized')
        AND last_seen_slot >= 0
        AND (
            NOT start_eligible OR (
                active
                AND source_commitment = 'finalized'
                AND last_mutation IN ('create', 'update')
            )
        )
    )
);

CREATE INDEX IF NOT EXISTS cross_mint_swap_policies_start_idx
    ON loyal_yield.cross_mint_swap_policies
        (cluster, settings, vault_index, vault_pubkey, last_seen_slot DESC, source_shard)
    WHERE active AND start_eligible AND source_commitment = 'finalized';

CREATE INDEX IF NOT EXISTS cross_mint_swap_policies_account_idx
    ON loyal_yield.cross_mint_swap_policies
        (cluster, policy_account, last_seen_slot DESC);

COMMENT ON TABLE loyal_yield.cross_mint_swap_policies IS
    'One finality-aware catalog row per immutable generalized Jupiter cross-mint policy account, including removal-only tombstones. Pair authorization is derived from source_shard and the canonical mint registry.';

COMMENT ON COLUMN loyal_yield.cross_mint_swap_policies.start_eligible IS
    'True only for a finalized active create/update observation; planners must use this instead of active alone.';

COMMENT ON COLUMN loyal_yield.cross_mint_swap_policies.daily_source_mint_spending_cap IS
    'Positive Squads daily spending limit for each source mint in the policy shard, in base units.';

ALTER TABLE loyal_yield.rebalance_decisions
    DROP CONSTRAINT IF EXISTS rebalance_decisions_movement_check;

ALTER TABLE loyal_yield.rebalance_decisions
    ADD CONSTRAINT rebalance_decisions_movement_check CHECK (
        movement_route IN ('same_mint', 'cross_mint_jupiter')
        AND (
            source_liquidity_mint IS NULL
            OR target_liquidity_mint IS NULL
            OR movement_route = CASE
                WHEN source_liquidity_mint = target_liquidity_mint
                    THEN 'same_mint'
                ELSE 'cross_mint_jupiter'
            END
        )
        AND custody_version >= 0
        AND continuation_fencing_token >= 0
        AND continuation_attempt_count >= 0
        AND (
            cross_mint_activation_control_generation IS NULL
            OR cross_mint_activation_control_generation >= 0
        )
        AND (
            continuation_control_generation IS NULL
            OR continuation_control_generation >= 0
        )
        AND (custody_amount_raw IS NULL OR custody_amount_raw >= 0)
        AND (
            custody_observed_balance_raw IS NULL
            OR custody_observed_balance_raw >= custody_amount_raw
        )
        AND (custody_reconciled_slot IS NULL OR custody_reconciled_slot >= 0)
        AND (
            terminal_outcome IS NULL OR terminal_outcome IN (
                'completed_target', 'recovered_source',
                'cancelled_before_withdraw', 'closed_by_user',
                'manual_intervention'
            )
        )
        AND (terminal_evidence IS NULL OR jsonb_typeof(terminal_evidence) = 'object')
        AND (terminal_observed_slot IS NULL OR terminal_observed_slot >= 0)
        AND (
            (
                terminal_outcome IS NULL
                AND terminal_evidence IS NULL
                AND terminal_reason IS NULL
                AND terminal_observed_slot IS NULL
            )
            OR (
                terminal_outcome IN ('completed_target', 'recovered_source')
                AND (
                    (
                        custody_amount_raw = 0
                        AND terminal_evidence IS NULL
                        AND terminal_reason IS NULL
                        AND terminal_observed_slot IS NULL
                    )
                    OR (
                        custody_amount_raw > 0
                        AND custody_observed_balance_raw IS NOT NULL
                        AND terminal_evidence->>'kind' =
                            'kamino_unmintable_rounding_dust'
                        AND terminal_reason =
                            'kamino_unmintable_rounding_dust'
                        AND terminal_observed_slot IS NOT NULL
                    )
                )
            )
            OR (
                terminal_outcome IN (
                    'cancelled_before_withdraw', 'closed_by_user',
                    'manual_intervention'
                )
                AND terminal_evidence IS NOT NULL
                AND terminal_evidence <> '{}'::jsonb
                AND NULLIF(btrim(terminal_reason), '') IS NOT NULL
                AND terminal_observed_slot IS NOT NULL
            )
        )
    );

COMMENT ON COLUMN loyal_yield.rebalance_decisions.terminal_outcome IS
    'Final custody outcome; cancelled_before_withdraw means activation reserved capacity but no signed withdrawal was ever admitted.';

CREATE OR REPLACE FUNCTION loyal_yield.verify_cross_mint_prewithdraw_cancellation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    signed_submission_count BIGINT;
BEGIN
    IF NEW.movement_route <> 'cross_mint_jupiter'
       OR NEW.terminal_outcome <> 'cancelled_before_withdraw'
    THEN
        RETURN NULL;
    END IF;

    SELECT count(*)::BIGINT
    INTO signed_submission_count
    FROM loyal_yield.signed_route_submissions
    WHERE decision_id = NEW.id;

    IF NEW.status <> 'abandoned'::loyal_yield.decision_status
       OR NEW.custody_version <> 0
       OR signed_submission_count <> 0
       OR NEW.custody_mint IS DISTINCT FROM NEW.source_liquidity_mint
       OR NEW.custody_amount_raw IS DISTINCT FROM NEW.amount_raw
       OR NEW.custody_account IS DISTINCT FROM NEW.source_reserve
       OR NEW.custody_observed_balance_raw IS NOT NULL
       OR NEW.custody_reconciled_slot IS NOT NULL
       OR NEW.terminal_evidence->>'kind'
            IS DISTINCT FROM 'start_authority_revoked_before_withdraw'
       OR NEW.terminal_reason
            IS DISTINCT FROM 'start_authority_revoked_before_withdraw'
       OR NEW.terminal_observed_slot IS NULL
    THEN
        RAISE EXCEPTION
            'cancelled cross-mint start does not prove untouched custody';
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_decision_verifies_cross_mint_custody
    ON loyal_yield.rebalance_decisions;
CREATE CONSTRAINT TRIGGER rebalance_decision_verifies_cross_mint_custody
AFTER INSERT OR UPDATE OF custody_mint, custody_amount_raw, custody_account,
    custody_observed_balance_raw,
    custody_reconciled_slot, custody_version, terminal_outcome,
    terminal_evidence, terminal_reason, terminal_observed_slot, status
ON loyal_yield.rebalance_decisions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
WHEN (NEW.terminal_outcome IS DISTINCT FROM 'cancelled_before_withdraw')
EXECUTE FUNCTION loyal_yield.verify_cross_mint_custody_projection();

DROP TRIGGER IF EXISTS rebalance_decision_verifies_prewithdraw_cancellation
    ON loyal_yield.rebalance_decisions;
CREATE CONSTRAINT TRIGGER rebalance_decision_verifies_prewithdraw_cancellation
AFTER INSERT OR UPDATE OF custody_mint, custody_amount_raw, custody_account,
    custody_observed_balance_raw,
    custody_reconciled_slot, custody_version, terminal_outcome,
    terminal_evidence, terminal_reason, terminal_observed_slot, status
ON loyal_yield.rebalance_decisions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
WHEN (NEW.terminal_outcome = 'cancelled_before_withdraw')
EXECUTE FUNCTION loyal_yield.verify_cross_mint_prewithdraw_cancellation();
