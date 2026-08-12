-- Durable execution-time target-capacity admission.
--
-- Planner projections are advisory: by the time a route is signed, another
-- executor may have admitted flow into the same reserve. These rows serialize
-- only that reserve's small admission frontier and keep landed-but-not-yet-
-- reflected inflow reserved until market telemetry crosses the movement slot.

CREATE TABLE IF NOT EXISTS loyal_yield.target_capacity_frontiers (
    cluster TEXT NOT NULL,
    target_reserve TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    observed_supply_usd_micros BIGINT NOT NULL,
    observed_slot BIGINT NOT NULL,
    maximum_inflight_usd_micros BIGINT NOT NULL,
    -- Telemetry freshness is independent from reservation churn. A build
    -- fences the market observation, while reservations admitted from the
    -- same observation advance only the audit generation below.
    telemetry_version BIGINT NOT NULL DEFAULT 0,
    reservation_generation BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, target_reserve, liquidity_mint),
    CONSTRAINT target_capacity_frontiers_identity_check CHECK (
        NULLIF(btrim(cluster), '') IS NOT NULL
        AND NULLIF(btrim(target_reserve), '') IS NOT NULL
        AND NULLIF(btrim(liquidity_mint), '') IS NOT NULL
    ),
    CONSTRAINT target_capacity_frontiers_value_check CHECK (
        observed_supply_usd_micros >= 0
        AND observed_slot >= 0
        AND maximum_inflight_usd_micros > 0
        AND telemetry_version >= 0
        AND reservation_generation >= 0
    )
);

CREATE TABLE IF NOT EXISTS loyal_yield.target_capacity_reservations (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    target_reserve TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    opportunity_id BIGINT NOT NULL UNIQUE
        REFERENCES loyal_yield.rebalance_opportunities(id),
    decision_id BIGINT UNIQUE REFERENCES loyal_yield.rebalance_decisions(id),
    signed_submission_id BIGINT UNIQUE
        REFERENCES loyal_yield.signed_route_submissions(id),
    principal_usd_micros BIGINT NOT NULL,
    admitted_observed_supply_usd_micros BIGINT NOT NULL,
    admitted_observed_slot BIGINT NOT NULL,
    admitted_maximum_inflight_usd_micros BIGINT NOT NULL,
    admitted_telemetry_version BIGINT NOT NULL,
    reservation_generation BIGINT NOT NULL,
    admitted_observed_target_apy_bps BIGINT NOT NULL,
    admitted_projected_target_apy_bps BIGINT NOT NULL,
    admitted_source_apy_bps BIGINT NOT NULL,
    admitted_edge_bps BIGINT NOT NULL,
    admitted_net_holding_gain_usd_micros BIGINT NOT NULL,
    admitted_fee_cap_lamports BIGINT NOT NULL,
    reservation_fencing_token BIGINT NOT NULL,
    state_version BIGINT NOT NULL DEFAULT 1,
    reservation_state TEXT NOT NULL DEFAULT 'active',
    movement_slot BIGINT,
    released_at TIMESTAMPTZ,
    release_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (cluster, target_reserve, liquidity_mint)
        REFERENCES loyal_yield.target_capacity_frontiers
            (cluster, target_reserve, liquidity_mint),
    CONSTRAINT target_capacity_reservations_identity_check CHECK (
        NULLIF(btrim(cluster), '') IS NOT NULL
        AND NULLIF(btrim(target_reserve), '') IS NOT NULL
        AND NULLIF(btrim(liquidity_mint), '') IS NOT NULL
    ),
    CONSTRAINT target_capacity_reservations_value_check CHECK (
        principal_usd_micros > 0
        AND admitted_observed_supply_usd_micros >= 0
        AND admitted_observed_slot >= 0
        AND admitted_maximum_inflight_usd_micros > 0
        AND admitted_telemetry_version >= 0
        AND reservation_generation > 0
        AND admitted_projected_target_apy_bps <= admitted_observed_target_apy_bps
        AND admitted_edge_bps =
            admitted_projected_target_apy_bps - admitted_source_apy_bps
        AND admitted_edge_bps > 0
        AND admitted_net_holding_gain_usd_micros > 0
        AND admitted_fee_cap_lamports > 0
        AND reservation_fencing_token > 0
        AND state_version > 0
        AND (movement_slot IS NULL OR movement_slot >= 0)
    ),
    CONSTRAINT target_capacity_reservations_state_check CHECK (
        reservation_state IN ('active', 'awaiting_telemetry', 'released')
        AND (
            (reservation_state = 'active'
                AND movement_slot IS NULL
                AND released_at IS NULL
                AND release_reason IS NULL)
            OR
            (reservation_state = 'awaiting_telemetry'
                AND movement_slot IS NOT NULL
                AND released_at IS NULL
                AND release_reason IS NULL)
            OR
            (reservation_state = 'released'
                AND released_at IS NOT NULL
                AND NULLIF(btrim(release_reason), '') IS NOT NULL)
        )
    ),
    CONSTRAINT target_capacity_reservations_link_check CHECK (
        (decision_id IS NULL AND signed_submission_id IS NULL)
        OR (decision_id IS NOT NULL AND signed_submission_id IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS target_capacity_reservations_live_idx
    ON loyal_yield.target_capacity_reservations
        (cluster, target_reserve, liquidity_mint, reservation_state, id)
    WHERE reservation_state <> 'released';

-- The runtime normally performs this attachment explicitly in the same
-- transaction as the decision. The deferred constraint is a fail-closed guard
-- against any future fleet handoff forgetting the reciprocal capacity link.
CREATE OR REPLACE FUNCTION loyal_yield.require_target_capacity_reservation_link()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    reservation_decision_id BIGINT;
    reservation_submission_id BIGINT;
    submission_decision_id BIGINT;
    reservation_opportunity_id BIGINT;
BEGIN
    SELECT decision_id, signed_submission_id, opportunity_id
    INTO reservation_decision_id, reservation_submission_id,
         reservation_opportunity_id
    FROM loyal_yield.target_capacity_reservations
    WHERE opportunity_id = NEW.opportunity_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION
            'fleet signed submission requires a durable target-capacity reservation';
    END IF;

    SELECT decision_id
    INTO submission_decision_id
    FROM loyal_yield.signed_route_submissions
    WHERE id = NEW.id;

    IF reservation_opportunity_id IS DISTINCT FROM NEW.opportunity_id
       OR reservation_submission_id IS DISTINCT FROM NEW.id
       OR submission_decision_id IS NULL
       OR reservation_decision_id IS DISTINCT FROM submission_decision_id
    THEN
        RAISE EXCEPTION
            'signed submission and target-capacity reservation identities diverged';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS signed_route_submission_requires_target_capacity
    ON loyal_yield.signed_route_submissions;
CREATE CONSTRAINT TRIGGER signed_route_submission_requires_target_capacity
AFTER INSERT OR UPDATE OF decision_id
ON loyal_yield.signed_route_submissions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.require_target_capacity_reservation_link();

-- A successfully reconciled movement remains capacity-consuming until a
-- later target-market observation is strictly beyond the movement. Equal-slot
-- ordering is ambiguous without a transaction index. `reconciled_slot` proves
-- the observer is fresh enough to declare success, but the balance movement
-- happened at `confirmed_slot`; only that slot fences capacity reflection.
-- Proven terminal no-effect paths release immediately.
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
    confirmed_movement_slot BIGINT;
BEGIN
    IF OLD.submission_state IS NOT DISTINCT FROM NEW.submission_state
       OR NEW.submission_state NOT IN ('reconciled', 'expired', 'failed')
    THEN
        RETURN NEW;
    END IF;

    SELECT cluster, target_reserve, liquidity_mint
    INTO reservation_cluster, reservation_target_reserve,
         reservation_liquidity_mint
    FROM loyal_yield.target_capacity_reservations
    WHERE signed_submission_id = NEW.id;
    IF NOT FOUND THEN
        RAISE EXCEPTION
            'terminal signed route has no target-capacity reservation';
    END IF;

    -- Every writer takes the frontier before a reservation row. This matches
    -- telemetry admission and prevents target-local terminal/release updates
    -- from forming a row-lock cycle under load.
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

    IF NEW.submission_state = 'reconciled' THEN
        IF NEW.confirmed_slot IS NULL OR NEW.reconciled_slot IS NULL THEN
            RAISE EXCEPTION
                'reconciled route cannot fence capacity without movement slots';
        END IF;
        confirmed_movement_slot := NEW.confirmed_slot;
        IF frontier_observed_slot > confirmed_movement_slot THEN
            -- Telemetry may have advanced while reconciliation still held an
            -- active reservation. Do not strand the capacity waiting for a
            -- second market update that might never arrive.
            UPDATE loyal_yield.target_capacity_reservations
            SET reservation_state = 'released',
                movement_slot = confirmed_movement_slot,
                released_at = now(),
                release_reason = 'target_telemetry_already_reflected_movement',
                state_version = state_version + 1,
                updated_at = now()
            WHERE signed_submission_id = NEW.id
              AND reservation_state = 'active';
        ELSE
            UPDATE loyal_yield.target_capacity_reservations
            SET reservation_state = 'awaiting_telemetry',
                movement_slot = confirmed_movement_slot,
                state_version = state_version + 1,
                updated_at = now()
            WHERE signed_submission_id = NEW.id
              AND reservation_state = 'active';
        END IF;
    ELSE
        -- Expired routes have durable effect-absence proof and failed routes
        -- have a terminal chain result. In particular, signed-but-unsent
        -- failures release without waiting for telemetry.
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
            'terminal signed route must advance exactly one live target-capacity reservation';
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS signed_route_submission_advances_target_capacity
    ON loyal_yield.signed_route_submissions;
CREATE TRIGGER signed_route_submission_advances_target_capacity
AFTER UPDATE OF submission_state
ON loyal_yield.signed_route_submissions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.advance_target_capacity_from_submission();

COMMENT ON TABLE loyal_yield.target_capacity_frontiers IS
    'Per-target serialized telemetry fence and independent reservation audit generation';
COMMENT ON TABLE loyal_yield.target_capacity_reservations IS
    'Durable target inflow held from atomic signed-decision handoff until target telemetry reflects the movement';
