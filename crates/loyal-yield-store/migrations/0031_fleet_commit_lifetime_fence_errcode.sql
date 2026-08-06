-- Give the commit-time lifetime fences a dedicated SQLSTATE.
--
-- The fences in 0029 raise with the default plpgsql SQLSTATE (P0001), which is
-- shared by every other RAISE EXCEPTION in this schema. Workers therefore could
-- not tell an expected lifetime rejection from a genuine persistence fault, and
-- emitted an identical recovery-required operational error for both. That made
-- routine end-of-epoch backpressure page on every occurrence.
--
-- LY001 and LY002 mark only the lifetime rejections. The reactivation guard in
-- require_signed_route_commit_lifetime keeps the default SQLSTATE because it
-- reports a real invariant violation that must stay loud.
--
-- Function bodies are otherwise unchanged from 0029; the triggers are not
-- redefined because CREATE OR REPLACE FUNCTION keeps them bound.

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
            'active rebalance opportunity cannot commit without reciprocal optimizer-epoch identity and minimum usable lifetime'
            USING ERRCODE = 'LY001';
    END IF;
    RETURN NULL;
END;
$$;

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
            'signed route handoff cannot commit without reciprocal opportunity/optimizer identity and minimum usable lifetime'
            USING ERRCODE = 'LY002';
    END IF;
    RETURN NULL;
END;
$$;

COMMENT ON FUNCTION loyal_yield.require_rebalance_opportunity_commit_lifetime() IS
    'Deferred DB-clock fence for newly visible active fleet opportunity work. Raises SQLSTATE LY001 on lifetime rejection.';
COMMENT ON FUNCTION loyal_yield.require_signed_route_commit_lifetime() IS
    'Deferred DB-clock fence for atomic signed fleet decision handoff. Raises SQLSTATE LY002 on lifetime rejection.';
