-- Durable cross-mint movements.
--
-- The existing opportunity is immutable economic intent, the decision is the
-- one active movement, and submissions are append-only signed transaction
-- evidence.  There is deliberately no saga table and no separately mutable
-- phase column: custody plus reconciled leg evidence is the state machine.

ALTER TABLE loyal_yield.rebalance_opportunities
    ADD COLUMN IF NOT EXISTS source_liquidity_mint TEXT,
    ADD COLUMN IF NOT EXISTS target_liquidity_mint TEXT;

CREATE OR REPLACE FUNCTION loyal_yield.derive_opportunity_mint_identity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.source_liquidity_mint := COALESCE(
        NEW.source_liquidity_mint,
        NULLIF(NEW.execution_plan ->> 'source_liquidity_mint', ''),
        NEW.liquidity_mint
    );
    NEW.target_liquidity_mint := COALESCE(
        NEW.target_liquidity_mint,
        NULLIF(NEW.execution_plan ->> 'target_liquidity_mint', ''),
        NEW.liquidity_mint
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_opportunity_derives_mint_identity
    ON loyal_yield.rebalance_opportunities;
CREATE TRIGGER rebalance_opportunity_derives_mint_identity
BEFORE INSERT OR UPDATE OF liquidity_mint, execution_plan,
    source_liquidity_mint, target_liquidity_mint
ON loyal_yield.rebalance_opportunities
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.derive_opportunity_mint_identity();

UPDATE loyal_yield.rebalance_opportunities
SET source_liquidity_mint = COALESCE(
        source_liquidity_mint,
        NULLIF(execution_plan ->> 'source_liquidity_mint', ''),
        liquidity_mint
    ),
    target_liquidity_mint = COALESCE(
        target_liquidity_mint,
        NULLIF(execution_plan ->> 'target_liquidity_mint', ''),
        liquidity_mint
    )
WHERE source_liquidity_mint IS NULL
   OR target_liquidity_mint IS NULL;

ALTER TABLE loyal_yield.rebalance_opportunities
    ALTER COLUMN source_liquidity_mint SET NOT NULL,
    ALTER COLUMN target_liquidity_mint SET NOT NULL;

ALTER TABLE loyal_yield.rebalance_opportunities
    DROP CONSTRAINT IF EXISTS rebalance_opportunities_movement_identity_check;
ALTER TABLE loyal_yield.rebalance_opportunities
    ADD CONSTRAINT rebalance_opportunities_movement_identity_check CHECK (
        NULLIF(btrim(source_liquidity_mint), '') IS NOT NULL
        AND NULLIF(btrim(target_liquidity_mint), '') IS NOT NULL
        AND (
            execution_plan ->> 'kind' IS DISTINCT FROM 'cross_mint_jupiter'
            OR (
                source_reserve IS NOT NULL
                AND source_liquidity_mint <> target_liquidity_mint
            )
        )
    );

CREATE OR REPLACE FUNCTION loyal_yield.guard_activated_cross_mint_opportunity_intent()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.decision_id IS NOT NULL
       AND OLD.execution_plan ->> 'kind' = 'cross_mint_jupiter'
       AND (
           NEW.cluster IS DISTINCT FROM OLD.cluster
           OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
           OR NEW.vault_id IS DISTINCT FROM OLD.vault_id
           OR NEW.source_snapshot_id IS DISTINCT FROM OLD.source_snapshot_id
           OR NEW.optimizer_epoch_id IS DISTINCT FROM OLD.optimizer_epoch_id
           OR NEW.route_fingerprint IS DISTINCT FROM OLD.route_fingerprint
           OR NEW.requirements_fingerprint IS DISTINCT FROM OLD.requirements_fingerprint
           OR NEW.source_reserve IS DISTINCT FROM OLD.source_reserve
           OR NEW.target_reserve IS DISTINCT FROM OLD.target_reserve
           OR NEW.liquidity_mint IS DISTINCT FROM OLD.liquidity_mint
           OR NEW.source_liquidity_mint IS DISTINCT FROM OLD.source_liquidity_mint
           OR NEW.target_liquidity_mint IS DISTINCT FROM OLD.target_liquidity_mint
           OR NEW.amount_raw IS DISTINCT FROM OLD.amount_raw
           OR NEW.principal_usd_micros IS DISTINCT FROM OLD.principal_usd_micros
           OR NEW.source_apy_bps IS DISTINCT FROM OLD.source_apy_bps
           OR NEW.target_apy_bps IS DISTINCT FROM OLD.target_apy_bps
           OR NEW.estimated_edge_bps IS DISTINCT FROM OLD.estimated_edge_bps
           OR NEW.estimated_cost_lamports IS DISTINCT FROM OLD.estimated_cost_lamports
           OR NEW.annual_yield_gain_usd_micros IS DISTINCT FROM OLD.annual_yield_gain_usd_micros
           OR NEW.expected_net_gain_usd_micros IS DISTINCT FROM OLD.expected_net_gain_usd_micros
           OR NEW.economic_priority IS DISTINCT FROM OLD.economic_priority
           OR NEW.scheduler_priority_anchor IS DISTINCT FROM OLD.scheduler_priority_anchor
           OR NEW.priority_version IS DISTINCT FROM OLD.priority_version
           OR NEW.execution_plan IS DISTINCT FROM OLD.execution_plan
           OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
       )
    THEN
        RAISE EXCEPTION 'activated cross-mint opportunity intent and economics are immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_opportunity_guards_activated_cross_mint_intent
    ON loyal_yield.rebalance_opportunities;
CREATE TRIGGER rebalance_opportunity_guards_activated_cross_mint_intent
BEFORE UPDATE ON loyal_yield.rebalance_opportunities
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_activated_cross_mint_opportunity_intent();

ALTER TABLE loyal_yield.rebalance_decisions
    ADD COLUMN IF NOT EXISTS movement_route TEXT NOT NULL DEFAULT 'same_mint',
    ADD COLUMN IF NOT EXISTS active_target_reserve TEXT,
    ADD COLUMN IF NOT EXISTS custody_mint TEXT,
    ADD COLUMN IF NOT EXISTS custody_amount_raw BIGINT,
    ADD COLUMN IF NOT EXISTS custody_account TEXT,
    ADD COLUMN IF NOT EXISTS custody_observed_balance_raw BIGINT,
    ADD COLUMN IF NOT EXISTS custody_reconciled_slot BIGINT,
    ADD COLUMN IF NOT EXISTS custody_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS continuation_available_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS continuation_lease_owner TEXT,
    ADD COLUMN IF NOT EXISTS continuation_lease_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS continuation_fencing_token BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS continuation_attempt_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cross_mint_activation_control_generation BIGINT,
    ADD COLUMN IF NOT EXISTS continuation_control_generation BIGINT,
    ADD COLUMN IF NOT EXISTS terminal_outcome TEXT,
    ADD COLUMN IF NOT EXISTS terminal_evidence JSONB,
    ADD COLUMN IF NOT EXISTS terminal_reason TEXT,
    ADD COLUMN IF NOT EXISTS terminal_observed_slot BIGINT;

CREATE OR REPLACE FUNCTION loyal_yield.derive_movement_identity_and_initial_custody()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.movement_route := CASE
        WHEN NEW.source_liquidity_mint IS NOT NULL
         AND NEW.target_liquidity_mint IS NOT NULL
         AND NEW.source_liquidity_mint <> NEW.target_liquidity_mint
        THEN 'cross_mint_jupiter'
        ELSE 'same_mint'
    END;
    NEW.active_target_reserve := COALESCE(
        NEW.active_target_reserve,
        NEW.target_reserve
    );
    IF NEW.movement_route = 'cross_mint_jupiter' AND TG_OP = 'INSERT' THEN
        NEW.custody_mint := COALESCE(
            NEW.custody_mint,
            NEW.source_liquidity_mint
        );
        NEW.custody_amount_raw := COALESCE(
            NEW.custody_amount_raw,
            NEW.amount_raw
        );
        NEW.custody_account := COALESCE(
            NEW.custody_account,
            NEW.source_reserve
        );
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_decision_derives_movement_identity
    ON loyal_yield.rebalance_decisions;
CREATE TRIGGER rebalance_decision_derives_movement_identity
BEFORE INSERT OR UPDATE OF source_liquidity_mint, target_liquidity_mint,
    target_reserve, movement_route
ON loyal_yield.rebalance_decisions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.derive_movement_identity_and_initial_custody();

UPDATE loyal_yield.rebalance_decisions
SET movement_route = CASE
        WHEN source_liquidity_mint IS NOT NULL
         AND target_liquidity_mint IS NOT NULL
         AND source_liquidity_mint <> target_liquidity_mint
        THEN 'cross_mint_jupiter'
        ELSE 'same_mint'
    END,
    active_target_reserve = COALESCE(active_target_reserve, target_reserve),
    custody_mint = COALESCE(custody_mint, source_liquidity_mint, liquidity_mint),
    custody_amount_raw = COALESCE(custody_amount_raw, amount_raw),
    custody_account = COALESCE(custody_account, source_reserve, target_reserve)
WHERE active_target_reserve IS NULL
   OR custody_mint IS NULL
   OR custody_amount_raw IS NULL
   OR custody_account IS NULL;

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
                'closed_by_user', 'manual_intervention'
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
                terminal_outcome IN ('closed_by_user', 'manual_intervention')
                AND terminal_evidence IS NOT NULL
                AND terminal_evidence <> '{}'::jsonb
                AND NULLIF(btrim(terminal_reason), '') IS NOT NULL
                AND terminal_observed_slot IS NOT NULL
            )
        )
    );

ALTER TABLE loyal_yield.rebalance_decisions
    DROP CONSTRAINT IF EXISTS rebalance_decisions_continuation_lease_check;
ALTER TABLE loyal_yield.rebalance_decisions
    ADD CONSTRAINT rebalance_decisions_continuation_lease_check CHECK (
        (
            continuation_lease_owner IS NULL
            AND continuation_lease_expires_at IS NULL
        ) OR (
            movement_route = 'cross_mint_jupiter'
            AND status = 'confirming'::loyal_yield.decision_status
            AND terminal_outcome IS NULL
            AND NULLIF(btrim(continuation_lease_owner), '') IS NOT NULL
            AND continuation_lease_expires_at IS NOT NULL
        )
    );

ALTER TABLE loyal_yield.rebalance_decisions
    DROP CONSTRAINT IF EXISTS rebalance_decisions_cross_mint_identity_check;
ALTER TABLE loyal_yield.rebalance_decisions
    ADD CONSTRAINT rebalance_decisions_cross_mint_identity_check CHECK (
        movement_route <> 'cross_mint_jupiter'
        OR (
            source_reserve IS NOT NULL
            AND target_reserve IS NOT NULL
            AND active_target_reserve IS NOT NULL
            AND source_liquidity_mint IS NOT NULL
            AND target_liquidity_mint IS NOT NULL
            AND source_liquidity_mint <> target_liquidity_mint
            AND amount_raw > 0
            AND custody_mint IS NOT NULL
            AND custody_amount_raw IS NOT NULL
            AND custody_account IS NOT NULL
            AND cross_mint_activation_control_generation IS NOT NULL
            AND status IN (
                'planned'::loyal_yield.decision_status,
                'confirming'::loyal_yield.decision_status,
                'confirmed'::loyal_yield.decision_status,
                'failed'::loyal_yield.decision_status,
                'abandoned'::loyal_yield.decision_status
            )
        )
    );

CREATE INDEX IF NOT EXISTS rebalance_decisions_cross_mint_continuation_idx
    ON loyal_yield.rebalance_decisions (
        continuation_available_at,
        continuation_lease_expires_at,
        created_at,
        id
    )
    WHERE movement_route = 'cross_mint_jupiter'
      AND status = 'confirming'
      AND terminal_outcome IS NULL;

ALTER TABLE loyal_yield.signed_route_submissions
    ADD COLUMN IF NOT EXISTS movement_leg TEXT NOT NULL DEFAULT 'route',
    ADD COLUMN IF NOT EXISTS leg_purpose TEXT NOT NULL DEFAULT 'optimize_yield',
    ADD COLUMN IF NOT EXISTS leg_generation BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS required_commitment TEXT NOT NULL DEFAULT 'confirmed',
    ADD COLUMN IF NOT EXISTS policy_account TEXT,
    ADD COLUMN IF NOT EXISTS expected_effect JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS expected_balance_anchors JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS reconciled_effect JSONB,
    ADD COLUMN IF NOT EXISTS reconciled_balance_anchors JSONB,
    ADD COLUMN IF NOT EXISTS finalized_slot BIGINT,
    ADD COLUMN IF NOT EXISTS finalized_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS effect_debit_mint TEXT,
    ADD COLUMN IF NOT EXISTS effect_debit_account TEXT,
    ADD COLUMN IF NOT EXISTS effect_debit_amount_raw BIGINT,
    ADD COLUMN IF NOT EXISTS effect_credit_mint TEXT,
    ADD COLUMN IF NOT EXISTS effect_credit_account TEXT,
    ADD COLUMN IF NOT EXISTS effect_credit_amount_raw BIGINT;

ALTER TABLE loyal_yield.signed_route_submissions
    DROP CONSTRAINT IF EXISTS signed_route_submissions_movement_leg_check;
ALTER TABLE loyal_yield.signed_route_submissions
    ADD CONSTRAINT signed_route_submissions_movement_leg_check CHECK (
        movement_leg IN ('route', 'withdraw', 'swap', 'deposit')
        AND leg_purpose IN ('optimize_yield', 'recover_source', 'fallback_target')
        AND leg_generation > 0
        AND required_commitment IN ('confirmed', 'finalized')
        AND jsonb_typeof(expected_effect) = 'object'
        AND jsonb_typeof(expected_balance_anchors) = 'object'
        AND (
            reconciled_balance_anchors IS NULL
            OR jsonb_typeof(reconciled_balance_anchors) = 'object'
        )
        AND (reconciled_effect IS NULL OR jsonb_typeof(reconciled_effect) = 'object')
        AND (finalized_slot IS NULL OR finalized_slot >= 0)
        AND (effect_debit_amount_raw IS NULL OR effect_debit_amount_raw >= 0)
        AND (effect_credit_amount_raw IS NULL OR effect_credit_amount_raw >= 0)
        AND (
            reconciled_effect IS NULL
            OR (
                finalized_slot IS NOT NULL
                AND finalized_at IS NOT NULL
                AND (
                    effect_debit_amount_raw IS NOT NULL
                    OR effect_credit_amount_raw IS NOT NULL
                )
            )
        )
    );

ALTER TABLE loyal_yield.signed_route_submissions
    DROP CONSTRAINT IF EXISTS signed_route_submissions_cross_mint_leg_check;
ALTER TABLE loyal_yield.signed_route_submissions
    ADD CONSTRAINT signed_route_submissions_cross_mint_leg_check CHECK (
        movement_leg = 'route'
        OR (
            required_commitment = 'finalized'
            AND policy_account IS NOT NULL
            AND movement_leg IN ('withdraw', 'swap', 'deposit')
            AND (
                submission_state <> 'reconciliation_pending'
                OR (finalized_slot IS NOT NULL AND finalized_at IS NOT NULL)
            )
            AND (
                movement_leg = 'deposit'
                OR leg_purpose = 'optimize_yield'
            )
        )
    );

CREATE UNIQUE INDEX IF NOT EXISTS signed_route_submissions_movement_leg_generation_uidx
    ON loyal_yield.signed_route_submissions
        (decision_id, movement_leg, leg_generation)
    WHERE decision_id IS NOT NULL AND movement_leg <> 'route';

CREATE INDEX IF NOT EXISTS signed_route_submissions_movement_history_idx
    ON loyal_yield.signed_route_submissions
        (decision_id, created_at, id)
    WHERE decision_id IS NOT NULL AND movement_leg <> 'route';

-- Preserve the fused same-mint handoff verbatim. Cross-mint decisions are
-- activated by the explicit movement API because target capacity must become
-- movement-owned before the first withdrawal is published.
CREATE OR REPLACE FUNCTION loyal_yield.link_rebalance_decision_to_execute_opportunity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    leased_opportunity loyal_yield.rebalance_opportunities%ROWTYPE;
    linked_submission_count INTEGER;
    amount_matches BOOLEAN;
BEGIN
    IF NEW.source_liquidity_mint IS DISTINCT FROM NEW.target_liquidity_mint THEN
        RETURN NEW;
    END IF;

    SELECT opportunity.*
    INTO leased_opportunity
    FROM loyal_yield.rebalance_opportunities opportunity
    JOIN loyal_yield.optimizer_epochs epoch
      ON epoch.id = opportunity.optimizer_epoch_id
     AND epoch.cluster = opportunity.cluster
    WHERE opportunity.vault_id = NEW.vault_id
      AND opportunity.opportunity_state = 'leased'
      AND opportunity.lease_kind = 'execute'
      AND opportunity.lease_expires_at > clock_timestamp()
      AND opportunity.expires_at > clock_timestamp()
      AND epoch.expires_at > clock_timestamp()
    FOR UPDATE OF opportunity;

    IF NOT FOUND THEN
        RETURN NEW;
    END IF;

    amount_matches := NEW.amount_raw IS NOT DISTINCT FROM leased_opportunity.amount_raw
        OR (
            leased_opportunity.execution_plan->>'kind' = 'same_mint'
            AND NEW.amount_raw IS NOT NULL
            AND NEW.amount_raw > leased_opportunity.amount_raw
            AND (NEW.amount_raw - leased_opportunity.amount_raw)::NUMERIC * 1000000
                <= leased_opportunity.amount_raw::NUMERIC * 100
        );

    IF NEW.source_snapshot_id IS DISTINCT FROM leased_opportunity.source_snapshot_id
       OR NEW.source_reserve IS DISTINCT FROM leased_opportunity.source_reserve
       OR NEW.target_reserve IS DISTINCT FROM leased_opportunity.target_reserve
       OR NEW.liquidity_mint IS DISTINCT FROM leased_opportunity.liquidity_mint
       OR NOT amount_matches
       OR NEW.source_apy_bps IS DISTINCT FROM leased_opportunity.source_apy_bps
       OR NEW.target_apy_bps IS DISTINCT FROM leased_opportunity.target_apy_bps
       OR NEW.estimated_edge_bps IS DISTINCT FROM leased_opportunity.estimated_edge_bps
       OR NEW.estimated_cost_lamports IS DISTINCT FROM leased_opportunity.estimated_cost_lamports
       OR NEW.execution_plan->>'kind'
            IS DISTINCT FROM leased_opportunity.execution_plan->>'kind'
       OR (
            leased_opportunity.execution_plan->>'kind' = 'same_mint'
            AND NEW.execution_plan->>'route_amount_semantics'
                IS DISTINCT FROM leased_opportunity.execution_plan->>'route_amount_semantics'
       )
    THEN
        RAISE EXCEPTION 'rebalance decision does not match the leased execute opportunity';
    END IF;

    UPDATE loyal_yield.rebalance_opportunities
    SET opportunity_state = 'decision_created',
        decision_id = NEW.id,
        lease_kind = NULL,
        lease_owner = NULL,
        lease_expires_at = NULL,
        terminal_reason = NULL,
        updated_at = now()
    WHERE id = leased_opportunity.id;

    UPDATE loyal_yield.signed_route_submissions
    SET decision_id = NEW.id,
        updated_at = now()
    WHERE opportunity_id = leased_opportunity.id
      AND executor_fencing_token = leased_opportunity.fencing_token
      AND submission_state IN ('signed', 'submitted')
      AND decision_id IS NULL;

    GET DIAGNOSTICS linked_submission_count = ROW_COUNT;
    IF linked_submission_count <> 1 THEN
        RAISE EXCEPTION
            'leased execute opportunity must have exactly one persisted signed submission for its fence';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE IF NOT EXISTS loyal_yield.cross_mint_movement_controls (
    cluster TEXT PRIMARY KEY,
    start_new_movements BOOLEAN NOT NULL DEFAULT FALSE,
    continue_or_recover_existing BOOLEAN NOT NULL DEFAULT TRUE,
    generation BIGINT NOT NULL DEFAULT 0,
    updated_by TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT cross_mint_movement_controls_check CHECK (
        NULLIF(btrim(cluster), '') IS NOT NULL
        AND generation >= 0
        AND (updated_by IS NULL OR NULLIF(btrim(updated_by), '') IS NOT NULL)
    )
);

CREATE OR REPLACE FUNCTION loyal_yield.serialize_cross_mint_control_change()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(
        'loyal-yield-cross-mint-control:' || NEW.cluster,
        0
    ));
    IF TG_OP = 'UPDATE' AND NEW.generation <= OLD.generation THEN
        RAISE EXCEPTION 'cross-mint control updates must advance generation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS cross_mint_control_change_is_serialized
    ON loyal_yield.cross_mint_movement_controls;
CREATE TRIGGER cross_mint_control_change_is_serialized
BEFORE INSERT OR UPDATE ON loyal_yield.cross_mint_movement_controls
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.serialize_cross_mint_control_change();

-- Replacement generations are authorized only by this write-once receipt.
-- The receipt binds the exact signed submission to a finalized signature-
-- history scan and unchanged aggregate token-account anchors. Transition API
-- booleans are deliberately insufficient to create a replacement generation.
CREATE TABLE IF NOT EXISTS loyal_yield.cross_mint_no_effect_receipts (
    submission_id BIGINT PRIMARY KEY REFERENCES
        loyal_yield.signed_route_submissions(id) ON DELETE RESTRICT,
    decision_id BIGINT NOT NULL REFERENCES
        loyal_yield.rebalance_decisions(id) ON DELETE RESTRICT,
    movement_leg TEXT NOT NULL,
    leg_generation BIGINT NOT NULL,
    transaction_signature TEXT NOT NULL,
    observed_block_height BIGINT NOT NULL,
    signature_history_checked_through_slot BIGINT NOT NULL,
    effect_check_slot BIGINT NOT NULL,
    expected_balance_anchors JSONB NOT NULL,
    observed_balance_anchors JSONB NOT NULL,
    signature_history_evidence JSONB NOT NULL,
    evidence_hash TEXT NOT NULL UNIQUE,
    observed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT cross_mint_no_effect_receipts_check CHECK (
        movement_leg IN ('withdraw', 'swap', 'deposit')
        AND leg_generation > 0
        AND NULLIF(btrim(transaction_signature), '') IS NOT NULL
        AND observed_block_height >= 0
        AND signature_history_checked_through_slot >= 0
        AND effect_check_slot >= 0
        AND signature_history_checked_through_slot >= effect_check_slot
        AND jsonb_typeof(expected_balance_anchors) = 'object'
        AND expected_balance_anchors <> '{}'::jsonb
        AND jsonb_typeof(observed_balance_anchors) = 'object'
        AND observed_balance_anchors = expected_balance_anchors
        AND jsonb_typeof(signature_history_evidence) = 'object'
        AND signature_history_evidence <> '{}'::jsonb
        AND evidence_hash ~ '^[0-9a-f]{64}$'
    )
);

CREATE OR REPLACE FUNCTION loyal_yield.verify_cross_mint_no_effect_receipt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    submission_decision_id BIGINT;
    submission_movement_leg TEXT;
    submission_leg_generation BIGINT;
    submission_transaction_signature TEXT;
    submission_expected_balance_anchors JSONB;
    submission_last_valid_block_height BIGINT;
    submission_broadcast_count INTEGER;
    submission_state TEXT;
    submission_expiry_observed_block_height BIGINT;
    submission_effect_check_slot BIGINT;
    movement_route TEXT;
BEGIN
    SELECT route.decision_id, route.movement_leg, route.leg_generation,
           route.transaction_signature, route.expected_balance_anchors,
           route.last_valid_block_height, route.broadcast_count,
           route.submission_state, route.expiry_observed_block_height,
           route.effect_check_slot, decision.movement_route
    INTO submission_decision_id, submission_movement_leg,
         submission_leg_generation, submission_transaction_signature,
         submission_expected_balance_anchors,
         submission_last_valid_block_height, submission_broadcast_count,
         submission_state, submission_expiry_observed_block_height,
         submission_effect_check_slot, movement_route
    FROM loyal_yield.signed_route_submissions route
    JOIN loyal_yield.rebalance_decisions decision
      ON decision.id = route.decision_id
    WHERE route.id = NEW.submission_id
    FOR KEY SHARE OF route, decision;

    IF NOT FOUND
       OR movement_route <> 'cross_mint_jupiter'
       OR submission_decision_id IS DISTINCT FROM NEW.decision_id
       OR submission_movement_leg IS DISTINCT FROM NEW.movement_leg
       OR submission_leg_generation IS DISTINCT FROM NEW.leg_generation
       OR submission_transaction_signature IS DISTINCT FROM NEW.transaction_signature
       OR submission_expected_balance_anchors IS DISTINCT FROM NEW.expected_balance_anchors
       OR submission_last_valid_block_height >= NEW.observed_block_height
       OR (
            submission_broadcast_count = 0
            AND submission_state NOT IN ('signed', 'submitted')
       )
       OR (
            submission_broadcast_count > 0
            AND (
                submission_state NOT IN (
                    'expiry_check_pending', 'effect_ambiguous'
                )
                OR submission_expiry_observed_block_height
                    IS DISTINCT FROM NEW.observed_block_height
                OR submission_effect_check_slot
                    IS DISTINCT FROM NEW.effect_check_slot
            )
       )
    THEN
        RAISE EXCEPTION
            'cross-mint no-effect receipt is stale or does not bind the exact expired submission';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS cross_mint_no_effect_receipt_verifies_submission
    ON loyal_yield.cross_mint_no_effect_receipts;
CREATE TRIGGER cross_mint_no_effect_receipt_verifies_submission
BEFORE INSERT ON loyal_yield.cross_mint_no_effect_receipts
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.verify_cross_mint_no_effect_receipt();

CREATE OR REPLACE FUNCTION loyal_yield.reject_cross_mint_no_effect_receipt_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'cross-mint no-effect receipts are immutable';
END;
$$;

DROP TRIGGER IF EXISTS cross_mint_no_effect_receipt_is_immutable
    ON loyal_yield.cross_mint_no_effect_receipts;
CREATE TRIGGER cross_mint_no_effect_receipt_is_immutable
BEFORE UPDATE OR DELETE ON loyal_yield.cross_mint_no_effect_receipts
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.reject_cross_mint_no_effect_receipt_mutation();

-- Capacity belongs to the movement. The initial submission remains optional
-- audit linkage for legacy rows; later cross-mint legs do not replace it.
ALTER TABLE loyal_yield.target_capacity_reservations
    DROP CONSTRAINT IF EXISTS target_capacity_reservations_link_check;
ALTER TABLE loyal_yield.target_capacity_reservations
    ADD CONSTRAINT target_capacity_reservations_link_check CHECK (
        (decision_id IS NULL AND signed_submission_id IS NULL)
        OR decision_id IS NOT NULL
    );

CREATE OR REPLACE FUNCTION loyal_yield.require_target_capacity_reservation_link()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    reservation_decision_id BIGINT;
    reservation_submission_id BIGINT;
    reservation_opportunity_id BIGINT;
    submission_decision_id BIGINT;
    submission_opportunity_id BIGINT;
    movement_route TEXT;
BEGIN
    -- This is a deferred constraint trigger. Read the committed-in-transaction
    -- row instead of NEW: same-mint publication inserts the submission before
    -- attaching its decision, so the INSERT event's NEW.decision_id is stale
    -- by the time the constraint is checked at COMMIT.
    SELECT reservation.decision_id, reservation.signed_submission_id,
           reservation.opportunity_id, submission.decision_id,
           submission.opportunity_id, decision.movement_route
    INTO reservation_decision_id, reservation_submission_id,
         reservation_opportunity_id, submission_decision_id,
         submission_opportunity_id, movement_route
    FROM loyal_yield.signed_route_submissions submission
    JOIN loyal_yield.target_capacity_reservations reservation
      ON reservation.opportunity_id = submission.opportunity_id
    JOIN loyal_yield.rebalance_decisions decision
      ON decision.id = submission.decision_id
    WHERE submission.id = NEW.id;

    IF NOT FOUND
       OR reservation_opportunity_id IS DISTINCT FROM submission_opportunity_id
       OR reservation_decision_id IS DISTINCT FROM submission_decision_id
       OR (
           movement_route = 'same_mint'
           AND reservation_submission_id IS DISTINCT FROM NEW.id
       )
    THEN
        RAISE EXCEPTION
            'signed submission and movement-owned target capacity diverged: submission %, opportunity %, decision %, reservation opportunity %, reservation decision %, reservation submission %, movement route %',
            NEW.id, submission_opportunity_id, submission_decision_id,
            reservation_opportunity_id, reservation_decision_id,
            reservation_submission_id, movement_route;
    END IF;
    RETURN NEW;
END;
$$;

-- Continuations are fenced by the movement, not by the expired optimizer
-- epoch. The initial same-mint/withdraw handoff keeps the original fence.
CREATE OR REPLACE FUNCTION loyal_yield.require_signed_route_commit_lifetime()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    current_state TEXT;
    current_leg TEXT;
    current_generation BIGINT;
    movement_route TEXT;
    lifetime_ready BOOLEAN;
BEGIN
    SELECT submission.submission_state, submission.movement_leg,
           submission.leg_generation, decision.movement_route,
           opportunity.id IS NOT NULL
               AND epoch.id IS NOT NULL
               AND opportunity.opportunity_state = 'decision_created'
               AND opportunity.decision_id = submission.decision_id
               AND opportunity.expires_at >= clock_timestamp() + interval '60 seconds'
               AND epoch.expires_at >= clock_timestamp() + interval '60 seconds'
    INTO current_state, current_leg, current_generation, movement_route,
         lifetime_ready
    FROM loyal_yield.signed_route_submissions submission
    LEFT JOIN loyal_yield.rebalance_decisions decision
      ON decision.id = submission.decision_id
    LEFT JOIN loyal_yield.rebalance_opportunities opportunity
      ON opportunity.id = submission.opportunity_id
     AND opportunity.cluster = submission.cluster
    LEFT JOIN loyal_yield.optimizer_epochs epoch
      ON epoch.id = opportunity.optimizer_epoch_id
     AND epoch.id = submission.optimizer_epoch_id
     AND epoch.cluster = opportunity.cluster
    WHERE submission.id = NEW.id;

    IF NOT FOUND OR current_state NOT IN ('signed', 'submitted') THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE'
       AND OLD.submission_state IS DISTINCT FROM NEW.submission_state
       AND (
           (NEW.submission_state = 'signed' AND OLD.submission_state <> 'signed')
           OR (
               NEW.submission_state = 'submitted'
               AND OLD.submission_state NOT IN ('signed', 'submitted')
           )
       )
    THEN
        RAISE EXCEPTION
            'signed route handoff cannot commit by reactivating a later or terminal submission state';
    END IF;

    IF movement_route = 'cross_mint_jupiter'
       AND NOT (current_leg = 'withdraw' AND current_generation = 1)
    THEN
        RETURN NULL;
    END IF;
    IF lifetime_ready IS DISTINCT FROM TRUE THEN
        RAISE EXCEPTION
            'signed route handoff cannot commit without reciprocal opportunity/optimizer identity and minimum usable lifetime'
            USING ERRCODE = 'LY002';
    END IF;
    RETURN NULL;
END;
$$;

-- Same-mint behavior is retained verbatim. Cross-mint intermediate legs only
-- release per-transaction resources and wake the durable continuation. Parent
-- completion happens solely for a reconciled deposit purpose.
CREATE OR REPLACE FUNCTION loyal_yield.finish_terminal_route_submission()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    decision_rows INTEGER;
    opportunity_rows INTEGER;
    linked_opportunity_decision_id BIGINT;
    opportunity_vault_id BIGINT;
    decision_vault_id BIGINT;
    decision_state TEXT;
    movement_route TEXT;
    movement_terminal_outcome TEXT;
BEGIN
    IF NEW.submission_state NOT IN ('reconciled', 'expired', 'failed')
       OR OLD.submission_state IS NOT DISTINCT FROM NEW.submission_state
    THEN
        RETURN NEW;
    END IF;
    IF NEW.decision_id IS NULL THEN
        RAISE EXCEPTION 'terminal signed route submission has no decision';
    END IF;

    SELECT opportunity.decision_id, opportunity.vault_id,
           decision.vault_id, decision.status::text,
           decision.movement_route, decision.terminal_outcome
    INTO linked_opportunity_decision_id, opportunity_vault_id,
         decision_vault_id, decision_state, movement_route,
         movement_terminal_outcome
    FROM loyal_yield.rebalance_opportunities opportunity
    JOIN loyal_yield.rebalance_decisions decision
      ON decision.id = NEW.decision_id
    WHERE opportunity.id = NEW.opportunity_id;
    IF NOT FOUND
       OR linked_opportunity_decision_id IS DISTINCT FROM NEW.decision_id
       OR opportunity_vault_id IS DISTINCT FROM decision_vault_id
    THEN
        RAISE EXCEPTION
            'terminal signed route identity diverged before lease release';
    END IF;

    IF movement_route = 'cross_mint_jupiter' THEN
        IF NEW.submission_state IN ('expired', 'failed') THEN
            UPDATE loyal_yield.rebalance_decisions
            SET continuation_available_at = now(),
                continuation_lease_owner = NULL,
                continuation_lease_expires_at = NULL,
                abandon_reason = NULL,
                updated_at = now()
            WHERE id = NEW.decision_id
              AND status = 'confirming'::loyal_yield.decision_status
              AND terminal_outcome IS NULL;
        ELSIF NEW.movement_leg = 'deposit' THEN
            IF movement_terminal_outcome IS NULL OR decision_state <> 'confirmed' THEN
                RAISE EXCEPTION
                    'reconciled cross-mint deposit must atomically terminalize its movement';
            END IF;
            UPDATE loyal_yield.rebalance_opportunities
            SET opportunity_state = 'completed',
                terminal_reason = movement_terminal_outcome,
                updated_at = now()
            WHERE id = NEW.opportunity_id
              AND opportunity_state = 'decision_created'
              AND decision_id = NEW.decision_id;
            GET DIAGNOSTICS opportunity_rows = ROW_COUNT;
            IF opportunity_rows <> 1 THEN
                RAISE EXCEPTION
                    'terminal cross-mint deposit did not complete one opportunity';
            END IF;
        ELSE
            UPDATE loyal_yield.rebalance_decisions
            SET continuation_available_at = now(),
                continuation_lease_owner = NULL,
                continuation_lease_expires_at = NULL,
                updated_at = now()
            WHERE id = NEW.decision_id
              AND status = 'confirming'::loyal_yield.decision_status
              AND terminal_outcome IS NULL;
        END IF;
    ELSE
        IF NEW.submission_state IN ('expired', 'failed') THEN
            UPDATE loyal_yield.rebalance_decisions
            SET status = 'failed'::loyal_yield.decision_status,
                abandon_reason = COALESCE(
                    NEW.error_detail,
                    concat('signed_submission_', NEW.submission_state)
                ),
                updated_at = now()
            WHERE id = NEW.decision_id
              AND status NOT IN (
                  'confirmed'::loyal_yield.decision_status,
                  'failed'::loyal_yield.decision_status,
                  'abandoned'::loyal_yield.decision_status,
                  'skipped'::loyal_yield.decision_status
              );
            GET DIAGNOSTICS decision_rows = ROW_COUNT;
            IF decision_rows <> 1
               AND NOT EXISTS (
                   SELECT 1 FROM loyal_yield.rebalance_decisions
                   WHERE id = NEW.decision_id
                     AND status = 'failed'::loyal_yield.decision_status
               )
            THEN
                RAISE EXCEPTION
                    'terminal signed route failed to terminalize its decision';
            END IF;
        END IF;

        UPDATE loyal_yield.rebalance_opportunities
        SET opportunity_state = CASE
                WHEN NEW.submission_state = 'reconciled' THEN 'completed'
                ELSE 'failed'
            END,
            terminal_reason = CASE
                WHEN NEW.submission_state = 'reconciled' THEN 'route_reconciled'
                ELSE COALESCE(
                    NEW.error_detail,
                    concat('signed_submission_', NEW.submission_state)
                )
            END,
            updated_at = now()
        WHERE id = NEW.opportunity_id
          AND opportunity_state = 'decision_created'
          AND decision_id = NEW.decision_id;
        GET DIAGNOSTICS opportunity_rows = ROW_COUNT;
        IF opportunity_rows <> 1 THEN
            RAISE EXCEPTION
                'terminal signed route failed to terminalize its opportunity';
        END IF;
        IF NEW.submission_state = 'reconciled'
           AND decision_state <> 'confirmed'
        THEN
            RAISE EXCEPTION
                'reconciled signed route requires a confirmed decision';
        END IF;
    END IF;

    DELETE FROM loyal_yield.route_account_conflict_leases
    WHERE submission_id = NEW.id;

    UPDATE loyal_yield.lookup_table_usage_leases
    SET released_at = COALESCE(released_at, now()),
        updated_at = now()
    WHERE lease_kind = 'prepared_transaction'
      AND reference_key = NEW.semantic_key
      AND released_at IS NULL;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.advance_target_capacity_from_submission()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    changed_rows INTEGER := 0;
    reservation_cluster TEXT;
    reservation_target_reserve TEXT;
    reservation_liquidity_mint TEXT;
    frontier_observed_slot BIGINT;
    terminal_movement_slot BIGINT;
    movement_route TEXT;
    movement_outcome TEXT;
BEGIN
    IF OLD.submission_state IS NOT DISTINCT FROM NEW.submission_state
       OR NEW.submission_state NOT IN ('reconciled', 'expired', 'failed')
    THEN
        RETURN NEW;
    END IF;

    SELECT reservation.cluster, reservation.target_reserve,
           reservation.liquidity_mint, decision.movement_route,
           decision.terminal_outcome
    INTO reservation_cluster, reservation_target_reserve,
         reservation_liquidity_mint, movement_route, movement_outcome
    FROM loyal_yield.target_capacity_reservations reservation
    JOIN loyal_yield.rebalance_decisions decision
      ON decision.id = reservation.decision_id
    WHERE reservation.decision_id = NEW.decision_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION
            'terminal signed route has no movement-owned target capacity';
    END IF;

    IF movement_route = 'cross_mint_jupiter'
       AND (
           NEW.submission_state <> 'reconciled'
           OR NEW.movement_leg <> 'deposit'
       )
    THEN
        RETURN NEW;
    END IF;

    SELECT observed_slot
    INTO frontier_observed_slot
    FROM loyal_yield.target_capacity_frontiers
    WHERE cluster = reservation_cluster
      AND target_reserve = reservation_target_reserve
      AND liquidity_mint = reservation_liquidity_mint
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION
            'terminal signed route target-capacity frontier disappeared';
    END IF;

    IF NEW.submission_state = 'reconciled'
       AND movement_outcome = 'completed_target'
    THEN
        terminal_movement_slot := COALESCE(NEW.finalized_slot, NEW.confirmed_slot);
        IF terminal_movement_slot IS NULL OR NEW.reconciled_slot IS NULL THEN
            RAISE EXCEPTION
                'reconciled target deposit lacks movement slots';
        END IF;
        IF frontier_observed_slot > terminal_movement_slot THEN
            UPDATE loyal_yield.target_capacity_reservations
            SET reservation_state = 'released',
                movement_slot = terminal_movement_slot,
                released_at = now(),
                release_reason = 'target_telemetry_already_reflected_movement',
                state_version = state_version + 1,
                updated_at = now()
            WHERE decision_id = NEW.decision_id
              AND reservation_state = 'active';
        ELSE
            UPDATE loyal_yield.target_capacity_reservations
            SET reservation_state = 'awaiting_telemetry',
                movement_slot = terminal_movement_slot,
                state_version = state_version + 1,
                updated_at = now()
            WHERE decision_id = NEW.decision_id
              AND reservation_state = 'active';
        END IF;
    ELSIF movement_route = 'cross_mint_jupiter'
          AND movement_outcome IN (
              'recovered_source', 'closed_by_user', 'manual_intervention'
          )
    THEN
        UPDATE loyal_yield.target_capacity_reservations
        SET reservation_state = 'released',
            released_at = now(),
            release_reason = movement_outcome,
            state_version = state_version + 1,
            updated_at = now()
        WHERE decision_id = NEW.decision_id
          AND reservation_state <> 'released';
    ELSIF movement_route = 'same_mint' AND NEW.submission_state = 'reconciled' THEN
        terminal_movement_slot := NEW.confirmed_slot;
        IF terminal_movement_slot IS NULL OR NEW.reconciled_slot IS NULL THEN
            RAISE EXCEPTION
                'reconciled route cannot fence capacity without movement slots';
        END IF;
        IF frontier_observed_slot > terminal_movement_slot THEN
            UPDATE loyal_yield.target_capacity_reservations
            SET reservation_state = 'released',
                movement_slot = terminal_movement_slot,
                released_at = now(),
                release_reason = 'target_telemetry_already_reflected_movement',
                state_version = state_version + 1,
                updated_at = now()
            WHERE signed_submission_id = NEW.id
              AND reservation_state = 'active';
        ELSE
            UPDATE loyal_yield.target_capacity_reservations
            SET reservation_state = 'awaiting_telemetry',
                movement_slot = terminal_movement_slot,
                state_version = state_version + 1,
                updated_at = now()
            WHERE signed_submission_id = NEW.id
              AND reservation_state = 'active';
        END IF;
    ELSE
        UPDATE loyal_yield.target_capacity_reservations
        SET reservation_state = 'released',
            released_at = now(),
            release_reason = concat('signed_submission_', NEW.submission_state),
            state_version = state_version + 1,
            updated_at = now()
        WHERE signed_submission_id = NEW.id
          AND reservation_state <> 'released';
    END IF;

    GET DIAGNOSTICS changed_rows = ROW_COUNT;
    IF changed_rows <> 1 THEN
        RAISE EXCEPTION
            'terminal movement must advance exactly one live capacity reservation';
    END IF;
    RETURN NEW;
END;
$$;

-- Custody columns are only a transactionally maintained query projection.
-- Finalized, reconciled submission effects remain authoritative. This
-- deferred verifier observes the end-of-transaction state regardless of
-- whether the writer updates the decision or submission first.
CREATE OR REPLACE FUNCTION loyal_yield.verify_cross_mint_custody_projection()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    movement_id BIGINT;
    movement loyal_yield.rebalance_decisions%ROWTYPE;
    reconciled_count BIGINT;
    latest_leg TEXT;
    latest_purpose TEXT;
    latest_credit_mint TEXT;
    latest_credit_account TEXT;
    latest_credit_amount BIGINT;
    latest_credit_observed_balance BIGINT;
    latest_debit_mint TEXT;
    latest_debit_account TEXT;
    latest_debit_amount BIGINT;
    latest_expected_debit_amount BIGINT;
    latest_debit_observed_balance BIGINT;
    latest_reconciled_slot BIGINT;
    latest_residual_amount BIGINT;
BEGIN
    movement_id := COALESCE(
        NULLIF(to_jsonb(NEW) ->> 'decision_id', '')::BIGINT,
        NULLIF(to_jsonb(NEW) ->> 'id', '')::BIGINT
    );
    IF movement_id IS NULL THEN
        RETURN NULL;
    END IF;

    SELECT * INTO movement
    FROM loyal_yield.rebalance_decisions
    WHERE id = movement_id;
    IF NOT FOUND OR movement.movement_route <> 'cross_mint_jupiter' THEN
        RETURN NULL;
    END IF;

    SELECT count(*)::BIGINT
    INTO reconciled_count
    FROM loyal_yield.signed_route_submissions
    WHERE decision_id = movement_id
      AND movement_leg <> 'route'
      AND submission_state = 'reconciled';
    IF reconciled_count IS DISTINCT FROM movement.custody_version THEN
        RAISE EXCEPTION
            'cross-mint custody version diverges from reconciled effect count';
    END IF;

    SELECT movement_leg, leg_purpose,
           effect_credit_mint, effect_credit_account,
           effect_credit_amount_raw,
           NULLIF(reconciled_balance_anchors #>> '{credit,amountRaw}', '')::BIGINT,
           effect_debit_mint, effect_debit_account,
           effect_debit_amount_raw,
           NULLIF(expected_effect #>> '{debit,amountRaw}', '')::BIGINT,
           NULLIF(reconciled_balance_anchors #>> '{debit,amountRaw}', '')::BIGINT,
           reconciled_slot
    INTO latest_leg, latest_purpose, latest_credit_mint,
         latest_credit_account, latest_credit_amount,
         latest_credit_observed_balance, latest_debit_mint,
         latest_debit_account, latest_debit_amount,
         latest_expected_debit_amount, latest_debit_observed_balance,
         latest_reconciled_slot
    FROM loyal_yield.signed_route_submissions
    WHERE decision_id = movement_id
      AND movement_leg <> 'route'
      AND submission_state = 'reconciled'
    ORDER BY reconciled_at DESC, id DESC
    LIMIT 1;

    IF NOT FOUND THEN
        IF movement.custody_mint IS DISTINCT FROM movement.source_liquidity_mint
           OR movement.custody_amount_raw IS DISTINCT FROM movement.amount_raw
           OR movement.custody_account IS DISTINCT FROM movement.source_reserve
           OR movement.custody_observed_balance_raw IS NOT NULL
           OR movement.custody_reconciled_slot IS NOT NULL
           OR NOT (
               movement.terminal_outcome IS NULL
               OR (
                   movement.terminal_outcome IN (
                       'closed_by_user', 'manual_intervention'
                   )
                   AND movement.status = 'abandoned'::loyal_yield.decision_status
               )
           )
        THEN
            RAISE EXCEPTION
                'initial cross-mint custody diverges from immutable movement intent';
        END IF;
        RETURN NULL;
    END IF;

    IF latest_leg IN ('withdraw', 'swap') THEN
        IF latest_credit_mint IS NULL
           OR latest_credit_account IS NULL
           OR latest_credit_amount IS NULL
           OR movement.custody_mint IS DISTINCT FROM latest_credit_mint
           OR movement.custody_account IS DISTINCT FROM latest_credit_account
           OR movement.custody_amount_raw IS DISTINCT FROM latest_credit_amount
           OR movement.custody_observed_balance_raw IS DISTINCT FROM latest_credit_observed_balance
           OR NOT (
               (
                   movement.terminal_outcome IS NULL
                   AND movement.status = 'confirming'::loyal_yield.decision_status
               )
               OR (
                   movement.terminal_outcome IN (
                       'closed_by_user', 'manual_intervention'
                   )
                   AND movement.status = 'abandoned'::loyal_yield.decision_status
               )
           )
        THEN
            RAISE EXCEPTION
                'cross-mint idle custody projection diverges from latest finalized credit';
        END IF;
    ELSIF latest_leg = 'deposit' THEN
        IF latest_debit_mint IS NULL
           OR latest_debit_account IS NULL
           OR latest_debit_amount IS NULL
           OR latest_expected_debit_amount IS NULL
           OR latest_debit_observed_balance IS NULL
           OR latest_reconciled_slot IS NULL
           OR latest_debit_amount <= 0
           OR latest_expected_debit_amount < latest_debit_amount
        THEN
            RAISE EXCEPTION
                'cross-mint terminal deposit lacks a bounded finalized debit';
        END IF;
        latest_residual_amount :=
            latest_expected_debit_amount - latest_debit_amount;
        IF movement.status <> 'confirmed'::loyal_yield.decision_status
           OR movement.custody_amount_raw IS DISTINCT FROM latest_residual_amount
           OR movement.custody_mint IS DISTINCT FROM latest_debit_mint
           OR movement.custody_reconciled_slot IS DISTINCT FROM latest_reconciled_slot
           OR (
               latest_purpose = 'recover_source'
               AND movement.terminal_outcome <> 'recovered_source'
           )
           OR (
               latest_purpose IN ('optimize_yield', 'fallback_target')
               AND movement.terminal_outcome <> 'completed_target'
           )
           OR (
               latest_residual_amount = 0
               AND (
                   movement.custody_observed_balance_raw IS NOT NULL
                   OR movement.custody_account IS DISTINCT FROM CASE
                       WHEN latest_purpose = 'recover_source'
                           THEN movement.source_reserve
                       ELSE movement.active_target_reserve
                   END
                   OR movement.terminal_evidence IS NOT NULL
                   OR movement.terminal_reason IS NOT NULL
                   OR movement.terminal_observed_slot IS NOT NULL
               )
           )
           OR (
               latest_residual_amount > 0
               AND (
                   movement.custody_account IS DISTINCT FROM latest_debit_account
                   OR movement.custody_observed_balance_raw
                       IS DISTINCT FROM latest_debit_observed_balance
                   OR movement.terminal_reason IS DISTINCT FROM
                       'kamino_unmintable_rounding_dust'
                   OR movement.terminal_observed_slot
                       IS DISTINCT FROM latest_reconciled_slot
                   OR movement.terminal_evidence->>'kind'
                       IS DISTINCT FROM 'kamino_unmintable_rounding_dust'
                   OR NULLIF(
                       movement.terminal_evidence->>'requestedAmountRaw', ''
                   )::BIGINT IS DISTINCT FROM latest_expected_debit_amount
                   OR NULLIF(
                       movement.terminal_evidence->>'depositedAmountRaw', ''
                   )::BIGINT IS DISTINCT FROM latest_debit_amount
                   OR NULLIF(
                       movement.terminal_evidence->>'residualAmountRaw', ''
                   )::BIGINT IS DISTINCT FROM latest_residual_amount
                   OR NULLIF(
                       movement.terminal_evidence->>'minimumDepositAmountRaw', ''
                   )::BIGINT IS NULL
                   OR NULLIF(
                       movement.terminal_evidence->>'minimumDepositAmountRaw', ''
                   )::BIGINT <= latest_residual_amount
                   OR NULLIF(
                       movement.terminal_evidence->>'finalizedPostBalanceRaw', ''
                   )::BIGINT IS DISTINCT FROM latest_debit_observed_balance
               )
           )
        THEN
            RAISE EXCEPTION
                'cross-mint terminal custody projection diverges from finalized deposit';
        END IF;
    ELSE
        RAISE EXCEPTION 'cross-mint reconciled history has an unknown latest leg';
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
EXECUTE FUNCTION loyal_yield.verify_cross_mint_custody_projection();

DROP TRIGGER IF EXISTS signed_submission_verifies_cross_mint_custody
    ON loyal_yield.signed_route_submissions;
CREATE CONSTRAINT TRIGGER signed_submission_verifies_cross_mint_custody
AFTER INSERT OR UPDATE OF submission_state, reconciled_effect,
    reconciled_balance_anchors, effect_debit_mint, effect_debit_account,
    effect_debit_amount_raw, effect_credit_mint, effect_credit_account,
    effect_credit_amount_raw, reconciled_slot
ON loyal_yield.signed_route_submissions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.verify_cross_mint_custody_projection();

CREATE OR REPLACE FUNCTION loyal_yield.guard_signed_route_evidence_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.cluster IS DISTINCT FROM OLD.cluster
       OR NEW.semantic_key IS DISTINCT FROM OLD.semantic_key
       OR NEW.opportunity_id IS DISTINCT FROM OLD.opportunity_id
       OR NEW.signed_transaction IS DISTINCT FROM OLD.signed_transaction
       OR NEW.signed_transaction_hash IS DISTINCT FROM OLD.signed_transaction_hash
       OR NEW.message_hash IS DISTINCT FROM OLD.message_hash
       OR NEW.transaction_signature IS DISTINCT FROM OLD.transaction_signature
       OR NEW.recent_blockhash IS DISTINCT FROM OLD.recent_blockhash
       OR NEW.last_valid_block_height IS DISTINCT FROM OLD.last_valid_block_height
       OR NEW.source_snapshot_id IS DISTINCT FROM OLD.source_snapshot_id
       OR NEW.optimizer_epoch_id IS DISTINCT FROM OLD.optimizer_epoch_id
       OR NEW.alt_requirements_fingerprint IS DISTINCT FROM OLD.alt_requirements_fingerprint
       OR NEW.alt_selection_fingerprint IS DISTINCT FROM OLD.alt_selection_fingerprint
       OR NEW.alt_mutation_epochs IS DISTINCT FROM OLD.alt_mutation_epochs
       OR NEW.fee_payer IS DISTINCT FROM OLD.fee_payer
       OR NEW.fee_payer_kind IS DISTINCT FROM OLD.fee_payer_kind
       OR NEW.compiled_fee_lamports IS DISTINCT FROM OLD.compiled_fee_lamports
       OR NEW.writable_account_keys IS DISTINCT FROM OLD.writable_account_keys
       OR NEW.conflict_account_keys IS DISTINCT FROM OLD.conflict_account_keys
       OR NEW.executor_owner IS DISTINCT FROM OLD.executor_owner
       OR NEW.executor_fencing_token IS DISTINCT FROM OLD.executor_fencing_token
       OR NEW.movement_leg IS DISTINCT FROM OLD.movement_leg
       OR NEW.leg_purpose IS DISTINCT FROM OLD.leg_purpose
       OR NEW.leg_generation IS DISTINCT FROM OLD.leg_generation
       OR NEW.required_commitment IS DISTINCT FROM OLD.required_commitment
       OR NEW.policy_account IS DISTINCT FROM OLD.policy_account
       OR NEW.expected_effect IS DISTINCT FROM OLD.expected_effect
       OR NEW.expected_balance_anchors IS DISTINCT FROM OLD.expected_balance_anchors
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (
            NEW.submission_state IS NOT DISTINCT FROM OLD.submission_state
            AND NEW.submission_state_entered_at IS DISTINCT FROM OLD.submission_state_entered_at
       )
       OR (OLD.decision_id IS NOT NULL AND NEW.decision_id IS DISTINCT FROM OLD.decision_id)
       OR (OLD.reconciled_effect IS NOT NULL AND NEW.reconciled_effect IS DISTINCT FROM OLD.reconciled_effect)
       OR (OLD.reconciled_balance_anchors IS NOT NULL AND NEW.reconciled_balance_anchors IS DISTINCT FROM OLD.reconciled_balance_anchors)
       OR (OLD.finalized_slot IS NOT NULL AND NEW.finalized_slot IS DISTINCT FROM OLD.finalized_slot)
       OR (OLD.finalized_at IS NOT NULL AND NEW.finalized_at IS DISTINCT FROM OLD.finalized_at)
       OR (OLD.effect_debit_mint IS NOT NULL AND NEW.effect_debit_mint IS DISTINCT FROM OLD.effect_debit_mint)
       OR (OLD.effect_debit_account IS NOT NULL AND NEW.effect_debit_account IS DISTINCT FROM OLD.effect_debit_account)
       OR (OLD.effect_debit_amount_raw IS NOT NULL AND NEW.effect_debit_amount_raw IS DISTINCT FROM OLD.effect_debit_amount_raw)
       OR (OLD.effect_credit_mint IS NOT NULL AND NEW.effect_credit_mint IS DISTINCT FROM OLD.effect_credit_mint)
       OR (OLD.effect_credit_account IS NOT NULL AND NEW.effect_credit_account IS DISTINCT FROM OLD.effect_credit_account)
       OR (OLD.effect_credit_amount_raw IS NOT NULL AND NEW.effect_credit_amount_raw IS DISTINCT FROM OLD.effect_credit_amount_raw)
    THEN
        RAISE EXCEPTION 'signed route wire, identity, and reconciled effect evidence is immutable';
    END IF;
    RETURN NEW;
END;
$$;

COMMENT ON TABLE loyal_yield.cross_mint_movement_controls IS
    'Independent fail-closed gate for new cross-mint withdrawals and fail-open recovery continuation.';
COMMENT ON TABLE loyal_yield.cross_mint_no_effect_receipts IS
    'Immutable finalized signature-history and unchanged-balance proof required before any replacement leg generation.';
COMMENT ON COLUMN loyal_yield.rebalance_decisions.custody_amount_raw IS
    'Movement-attributed amount only; never the aggregate balance of a vault ATA.';
COMMENT ON COLUMN loyal_yield.rebalance_decisions.custody_observed_balance_raw IS
    'Finalized aggregate balance anchor for the custody ATA; attribution remains custody_amount_raw.';
COMMENT ON COLUMN loyal_yield.signed_route_submissions.reconciled_effect IS
    'Write-once finalized token-balance delta receipt used to advance movement custody.';
