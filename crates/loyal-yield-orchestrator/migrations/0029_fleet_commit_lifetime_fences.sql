-- Commit-time lifetime fences for fleet opportunity publication and signed
-- decision handoff.
--
-- Runtime checks reject stale work early, but a process or network pause can
-- occur between the last statement and COMMIT. These deferred triggers use the
-- database wall clock while COMMIT is executing, so no newly-published active
-- opportunity or signed handoff becomes visible with less than sixty seconds
-- of immutable optimizer-epoch lifetime remaining. Terminal transitions and
-- cleanup remain legal after expiry.

CREATE OR REPLACE FUNCTION loyal_yield.require_rebalance_opportunity_commit_lifetime()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    current_state TEXT;
    lifetime_ready BOOLEAN;
BEGIN
    SELECT opportunity.opportunity_state,
           epoch.id IS NOT NULL
               AND opportunity.expires_at >= clock_timestamp() + interval '60 seconds'
               AND epoch.expires_at >= clock_timestamp() + interval '60 seconds'
    INTO current_state, lifetime_ready
    FROM loyal_yield.rebalance_opportunities opportunity
    LEFT JOIN loyal_yield.optimizer_epochs epoch
      ON epoch.id = opportunity.optimizer_epoch_id
     AND epoch.cluster = opportunity.cluster
    WHERE opportunity.id = NEW.id;

    -- The row may have been deleted or terminalized later in the transaction.
    -- Neither case publishes executable work.
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    IF current_state NOT IN ('waiting_alt', 'revalidate', 'ready', 'leased') THEN
        RETURN NULL;
    END IF;
    IF lifetime_ready IS DISTINCT FROM TRUE THEN
        RAISE EXCEPTION
            'active rebalance opportunity cannot commit without reciprocal optimizer-epoch identity and minimum usable lifetime';
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_opportunity_insert_commit_lifetime
    ON loyal_yield.rebalance_opportunities;
CREATE CONSTRAINT TRIGGER rebalance_opportunity_insert_commit_lifetime
AFTER INSERT ON loyal_yield.rebalance_opportunities
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.require_rebalance_opportunity_commit_lifetime();

DROP TRIGGER IF EXISTS rebalance_opportunity_transition_commit_lifetime
    ON loyal_yield.rebalance_opportunities;
-- Do not trigger on expires_at alone: active rows must be allowed to age and
-- the expiry sweeper must be able to stage an already-expired row before its
-- terminal transition. Inserts and state/epoch publication transitions are
-- the points at which new executable work becomes visible.
CREATE CONSTRAINT TRIGGER rebalance_opportunity_transition_commit_lifetime
AFTER UPDATE OF opportunity_state, optimizer_epoch_id, cluster
ON loyal_yield.rebalance_opportunities
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.require_rebalance_opportunity_commit_lifetime();

CREATE OR REPLACE FUNCTION loyal_yield.require_signed_route_commit_lifetime()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    current_state TEXT;
    lifetime_ready BOOLEAN;
BEGIN
    SELECT submission.submission_state,
           opportunity.id IS NOT NULL
               AND epoch.id IS NOT NULL
               AND opportunity.opportunity_state = 'decision_created'
               AND opportunity.decision_id = submission.decision_id
               AND opportunity.expires_at >= clock_timestamp() + interval '60 seconds'
               AND epoch.expires_at >= clock_timestamp() + interval '60 seconds'
    INTO current_state, lifetime_ready
    FROM loyal_yield.signed_route_submissions submission
    LEFT JOIN loyal_yield.rebalance_opportunities opportunity
      ON opportunity.id = submission.opportunity_id
     AND opportunity.cluster = submission.cluster
    LEFT JOIN loyal_yield.optimizer_epochs epoch
      ON epoch.id = opportunity.optimizer_epoch_id
     AND epoch.id = submission.optimizer_epoch_id
     AND epoch.cluster = opportunity.cluster
    WHERE submission.id = NEW.id;

    -- Deletion and terminalization are cleanup, not a newly-visible signed
    -- handoff. Submitted is included so an insert that advances within one
    -- transaction cannot bypass the commit fence.
    IF NOT FOUND OR current_state NOT IN ('signed', 'submitted') THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF OLD.submission_state IS DISTINCT FROM NEW.submission_state
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
    END IF;
    IF lifetime_ready IS DISTINCT FROM TRUE THEN
        RAISE EXCEPTION
            'signed route handoff cannot commit without reciprocal opportunity/optimizer identity and minimum usable lifetime';
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS signed_route_insert_commit_lifetime
    ON loyal_yield.signed_route_submissions;
CREATE CONSTRAINT TRIGGER signed_route_insert_commit_lifetime
AFTER INSERT ON loyal_yield.signed_route_submissions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.require_signed_route_commit_lifetime();

DROP TRIGGER IF EXISTS signed_route_decision_link_commit_lifetime
    ON loyal_yield.signed_route_submissions;
-- Recording an already-broadcast signed transaction as submitted must never be
-- rolled back merely because the remaining epoch lifetime dropped below the
-- signing threshold. Re-entering signed/submitted from any later or terminal
-- state is a new active handoff and must pass the fence again.
CREATE CONSTRAINT TRIGGER signed_route_decision_link_commit_lifetime
AFTER UPDATE OF decision_id, submission_state
ON loyal_yield.signed_route_submissions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
WHEN (
    OLD.decision_id IS DISTINCT FROM NEW.decision_id
    OR (
        OLD.submission_state IS DISTINCT FROM NEW.submission_state
        AND (
            (NEW.submission_state = 'signed' AND OLD.submission_state <> 'signed')
            OR (
                NEW.submission_state = 'submitted'
                AND OLD.submission_state NOT IN ('signed', 'submitted')
            )
        )
    )
)
EXECUTE FUNCTION loyal_yield.require_signed_route_commit_lifetime();

COMMENT ON FUNCTION loyal_yield.require_rebalance_opportunity_commit_lifetime() IS
    'Deferred DB-clock fence for newly visible active fleet opportunity work.';
COMMENT ON FUNCTION loyal_yield.require_signed_route_commit_lifetime() IS
    'Deferred DB-clock fence for atomic signed fleet decision handoff.';
