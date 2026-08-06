-- Durable, horizontally claimable confirmation work for exact signed routes.

ALTER TABLE loyal_yield.signed_route_submissions
    ADD COLUMN IF NOT EXISTS confirmation_available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS confirmation_lease_owner TEXT,
    ADD COLUMN IF NOT EXISTS confirmation_lease_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS confirmation_fencing_token BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS confirmation_attempt_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS broadcast_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_broadcast_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_status_checked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS expiry_observed_block_height BIGINT,
    ADD COLUMN IF NOT EXISTS effect_check_slot BIGINT;

ALTER TABLE loyal_yield.route_account_conflict_leases
    ADD COLUMN IF NOT EXISTS submission_id BIGINT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'loyal_yield.route_account_conflict_leases'::regclass
          AND conname = 'route_account_conflict_leases_submission_id_fkey'
    ) THEN
        ALTER TABLE loyal_yield.route_account_conflict_leases
            ADD CONSTRAINT route_account_conflict_leases_submission_id_fkey
            FOREIGN KEY (submission_id)
            REFERENCES loyal_yield.signed_route_submissions(id);
    END IF;
END $$;

ALTER TABLE loyal_yield.signed_route_submissions
    DROP CONSTRAINT IF EXISTS signed_route_submissions_confirmation_lease_check;

ALTER TABLE loyal_yield.signed_route_submissions
    ADD CONSTRAINT signed_route_submissions_confirmation_lease_check CHECK (
        confirmation_fencing_token >= 0
        AND confirmation_attempt_count >= 0
        AND broadcast_count >= 0
        AND (
            expiry_observed_block_height IS NULL
            OR expiry_observed_block_height >= 0
        )
        AND (effect_check_slot IS NULL OR effect_check_slot >= 0)
        AND (
            (
                confirmation_lease_owner IS NULL
                AND confirmation_lease_expires_at IS NULL
            )
            OR (
                NULLIF(btrim(confirmation_lease_owner), '') IS NOT NULL
                AND confirmation_lease_expires_at IS NOT NULL
                AND submission_state IN (
                    'signed', 'submitted', 'confirmed', 'reconciliation_pending',
                    'expiry_check_pending', 'effect_ambiguous'
                )
            )
        )
    );

CREATE INDEX IF NOT EXISTS signed_route_submissions_confirmation_queue_idx
    ON loyal_yield.signed_route_submissions (
        cluster,
        confirmation_available_at,
        confirmation_lease_expires_at,
        created_at,
        id
    )
    WHERE decision_id IS NOT NULL
      AND submission_state IN ('signed', 'submitted', 'confirmed');

CREATE INDEX IF NOT EXISTS signed_route_submissions_reconciliation_queue_idx
    ON loyal_yield.signed_route_submissions (
        cluster,
        confirmation_available_at,
        confirmation_lease_expires_at,
        created_at,
        id
    )
    WHERE decision_id IS NOT NULL
      AND submission_state IN (
          'reconciliation_pending', 'expiry_check_pending', 'effect_ambiguous'
      );

DROP INDEX IF EXISTS loyal_yield.signed_route_submissions_opportunity_fence_uidx;

CREATE UNIQUE INDEX IF NOT EXISTS signed_route_submissions_one_nonterminal_opportunity_idx
    ON loyal_yield.signed_route_submissions (opportunity_id)
    WHERE submission_state NOT IN ('reconciled', 'expired', 'failed');

CREATE INDEX IF NOT EXISTS route_account_conflict_leases_submission_idx
    ON loyal_yield.route_account_conflict_leases
        (submission_id, expires_at, writable_account_key)
    WHERE submission_id IS NOT NULL;

CREATE OR REPLACE FUNCTION loyal_yield.notify_signed_route_confirmation_wakeup()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.decision_id IS NULL
       OR NEW.submission_state NOT IN ('signed', 'submitted', 'confirmed')
    THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE'
       AND OLD.decision_id IS NOT DISTINCT FROM NEW.decision_id
       AND OLD.submission_state IS NOT DISTINCT FROM NEW.submission_state
       AND OLD.confirmation_available_at IS NOT DISTINCT FROM NEW.confirmation_available_at
    THEN
        RETURN NEW;
    END IF;

    PERFORM pg_notify(
        'loyal_yield_route_confirmation_wakeup',
        json_build_object(
            'cluster', NEW.cluster,
            'submission_id', NEW.id,
            'state', NEW.submission_state
        )::text
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS signed_route_confirmation_wakeup
    ON loyal_yield.signed_route_submissions;
CREATE TRIGGER signed_route_confirmation_wakeup
AFTER INSERT OR UPDATE OF decision_id, submission_state, confirmation_available_at
ON loyal_yield.signed_route_submissions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.notify_signed_route_confirmation_wakeup();

CREATE OR REPLACE FUNCTION loyal_yield.notify_signed_route_reconciliation_wakeup()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.decision_id IS NULL
       OR NEW.submission_state NOT IN (
           'reconciliation_pending', 'expiry_check_pending', 'effect_ambiguous'
       )
    THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE'
       AND OLD.submission_state IS NOT DISTINCT FROM NEW.submission_state
       AND OLD.confirmation_available_at IS NOT DISTINCT FROM NEW.confirmation_available_at
    THEN
        RETURN NEW;
    END IF;

    PERFORM pg_notify(
        'loyal_yield_route_reconciliation_wakeup',
        json_build_object(
            'cluster', NEW.cluster,
            'submission_id', NEW.id,
            'state', NEW.submission_state
        )::text
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS signed_route_reconciliation_wakeup
    ON loyal_yield.signed_route_submissions;
CREATE TRIGGER signed_route_reconciliation_wakeup
AFTER INSERT OR UPDATE OF submission_state, confirmation_available_at
ON loyal_yield.signed_route_submissions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.notify_signed_route_reconciliation_wakeup();

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
       OR NEW.compiled_fee_lamports IS DISTINCT FROM OLD.compiled_fee_lamports
       OR NEW.writable_account_keys IS DISTINCT FROM OLD.writable_account_keys
       OR NEW.conflict_account_keys IS DISTINCT FROM OLD.conflict_account_keys
       OR NEW.executor_owner IS DISTINCT FROM OLD.executor_owner
       OR NEW.executor_fencing_token IS DISTINCT FROM OLD.executor_fencing_token
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (
            NEW.submission_state IS NOT DISTINCT FROM OLD.submission_state
            AND NEW.submission_state_entered_at IS DISTINCT FROM OLD.submission_state_entered_at
       )
       OR (
            OLD.decision_id IS NOT NULL
            AND NEW.decision_id IS DISTINCT FROM OLD.decision_id
       )
    THEN
        RAISE EXCEPTION 'signed route wire and identity evidence is immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS signed_route_submission_evidence_immutable
    ON loyal_yield.signed_route_submissions;
CREATE TRIGGER signed_route_submission_evidence_immutable
BEFORE UPDATE ON loyal_yield.signed_route_submissions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_signed_route_evidence_mutation();

CREATE OR REPLACE FUNCTION loyal_yield.require_signed_route_decision_link()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    current_decision_id BIGINT;
    current_opportunity_id BIGINT;
    opportunity_decision_id BIGINT;
    opportunity_vault_id BIGINT;
    decision_vault_id BIGINT;
BEGIN
    -- Query the end-of-transaction row instead of trusting the queued trigger
    -- event image: the atomic handoff inserts signed bytes first, then the
    -- decision trigger links them before this deferred constraint fires.
    SELECT decision_id, opportunity_id
    INTO current_decision_id, current_opportunity_id
    FROM loyal_yield.signed_route_submissions
    WHERE id = NEW.id;

    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    IF current_decision_id IS NULL THEN
        RAISE EXCEPTION
            'signed route submission must commit with a linked decision';
    END IF;

    SELECT opportunity.decision_id, opportunity.vault_id, decision.vault_id
    INTO opportunity_decision_id, opportunity_vault_id, decision_vault_id
    FROM loyal_yield.rebalance_opportunities opportunity
    JOIN loyal_yield.rebalance_decisions decision
      ON decision.id = current_decision_id
    WHERE opportunity.id = current_opportunity_id;

    IF NOT FOUND
       OR opportunity_decision_id IS DISTINCT FROM current_decision_id
       OR opportunity_vault_id IS DISTINCT FROM decision_vault_id
    THEN
        RAISE EXCEPTION
            'signed route submission, opportunity, and decision identities must be reciprocal';
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS signed_route_submission_requires_decision
    ON loyal_yield.signed_route_submissions;
CREATE CONSTRAINT TRIGGER signed_route_submission_requires_decision
AFTER INSERT OR UPDATE OF decision_id, submission_state
ON loyal_yield.signed_route_submissions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.require_signed_route_decision_link();

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
BEGIN
    IF NEW.submission_state IN ('reconciled', 'expired', 'failed')
       AND OLD.submission_state IS DISTINCT FROM NEW.submission_state
    THEN
        IF NEW.decision_id IS NULL THEN
            RAISE EXCEPTION 'terminal signed route submission has no decision';
        END IF;
        SELECT opportunity.decision_id, opportunity.vault_id,
               decision.vault_id, decision.status::text
        INTO linked_opportunity_decision_id, opportunity_vault_id,
             decision_vault_id, decision_state
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
    END IF;

    IF NEW.submission_state IN ('expired', 'failed')
       AND OLD.submission_state IS DISTINCT FROM NEW.submission_state
    THEN
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

    IF NEW.submission_state = 'reconciled'
       AND OLD.submission_state IS DISTINCT FROM NEW.submission_state
    THEN
        UPDATE loyal_yield.rebalance_opportunities
        SET opportunity_state = 'completed',
            terminal_reason = 'route_reconciled',
            updated_at = now()
        WHERE id = NEW.opportunity_id
          AND opportunity_state = 'decision_created'
          AND decision_id = NEW.decision_id;
    ELSIF NEW.submission_state IN ('expired', 'failed')
       AND OLD.submission_state IS DISTINCT FROM NEW.submission_state
    THEN
        UPDATE loyal_yield.rebalance_opportunities
        SET opportunity_state = 'failed',
            terminal_reason = COALESCE(
                NEW.error_detail,
                concat('signed_submission_', NEW.submission_state)
            ),
            updated_at = now()
        WHERE id = NEW.opportunity_id
          AND opportunity_state = 'decision_created'
          AND decision_id = NEW.decision_id;
    END IF;

    IF NEW.submission_state IN ('reconciled', 'expired', 'failed')
       AND OLD.submission_state IS DISTINCT FROM NEW.submission_state
    THEN
        GET DIAGNOSTICS opportunity_rows = ROW_COUNT;
        IF opportunity_rows <> 1
           AND NOT EXISTS (
               SELECT 1 FROM loyal_yield.rebalance_opportunities
               WHERE id = NEW.opportunity_id
                 AND decision_id = NEW.decision_id
                 AND opportunity_state = CASE
                     WHEN NEW.submission_state = 'reconciled' THEN 'completed'
                     ELSE 'failed'
                 END
           )
        THEN
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

    IF NEW.submission_state IN ('reconciled', 'expired', 'failed')
       AND OLD.submission_state IS DISTINCT FROM NEW.submission_state
    THEN
        DELETE FROM loyal_yield.route_account_conflict_leases
        WHERE submission_id = NEW.id;

        UPDATE loyal_yield.lookup_table_usage_leases
        SET released_at = COALESCE(released_at, now()),
            updated_at = now()
        WHERE lease_kind = 'prepared_transaction'
          AND reference_key = NEW.semantic_key
          AND released_at IS NULL;
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS signed_route_submission_finishes_terminal_state
    ON loyal_yield.signed_route_submissions;
CREATE TRIGGER signed_route_submission_finishes_terminal_state
AFTER UPDATE OF submission_state
ON loyal_yield.signed_route_submissions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.finish_terminal_route_submission();
