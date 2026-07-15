-- Durable, value-prioritized fleet orchestration queue.
--
-- PostgreSQL rows are the source of truth. NOTIFY is only a low-latency hint;
-- workers must recover by polling the indexed queue after startup/reconnect.

CREATE TABLE IF NOT EXISTS loyal_yield.optimizer_epochs (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    epoch_key TEXT NOT NULL,
    market_slot BIGINT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    market_state JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT optimizer_epochs_identity_unique UNIQUE (cluster, epoch_key),
    CONSTRAINT optimizer_epochs_evidence_check CHECK (
        market_slot >= 0
        AND expires_at > observed_at
        AND jsonb_typeof(market_state) = 'object'
    )
);

CREATE OR REPLACE FUNCTION loyal_yield.guard_optimizer_epoch_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'optimizer epochs are immutable';
END;
$$;

DROP TRIGGER IF EXISTS optimizer_epochs_immutable
    ON loyal_yield.optimizer_epochs;
CREATE TRIGGER optimizer_epochs_immutable
BEFORE UPDATE OR DELETE ON loyal_yield.optimizer_epochs
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_optimizer_epoch_mutation();

-- One durable watermark per fleet. A dirty-vault pass may reuse the last full
-- fleet ranking only when it was complete (no deferred contenders) and the
-- low-churn material market frontier still matches; every scoped pass adds the
-- current durable non-released capacity reservations. Otherwise the planner
-- immediately falls back to a full, capacity-aware sweep.
CREATE TABLE IF NOT EXISTS loyal_yield.fleet_planning_state (
    cluster TEXT PRIMARY KEY,
    full_sweep_started_at TIMESTAMPTZ NOT NULL,
    full_sweep_completed_at TIMESTAMPTZ NOT NULL,
    optimizer_epoch_key TEXT NOT NULL,
    optimizer_epoch_expires_at TIMESTAMPTZ NOT NULL,
    complete_frontier BOOLEAN NOT NULL,
    observed_vault_count BIGINT NOT NULL,
    opportunity_count BIGINT NOT NULL,
    selected_count BIGINT NOT NULL,
    deferred_count BIGINT NOT NULL,
    generation BIGINT NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT fleet_planning_state_evidence_check CHECK (
        full_sweep_completed_at >= full_sweep_started_at
        AND optimizer_epoch_expires_at > full_sweep_started_at
        AND observed_vault_count >= 0
        AND opportunity_count >= 0
        AND selected_count >= 0
        AND deferred_count >= 0
        AND generation > 0
    )
);

-- Source projection tables predate multi-cluster orchestration and do not
-- carry a cluster column. Planners therefore register their explicit cluster
-- before the initial full sweep; source triggers fan out only to this durable
-- registry and never guess mainnet from a process-local default.
CREATE TABLE IF NOT EXISTS loyal_yield.fleet_planning_clusters (
    cluster TEXT PRIMARY KEY,
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT fleet_planning_clusters_name_check CHECK (
        NULLIF(btrim(cluster), '') IS NOT NULL
    )
);

-- Coalesced hints are the durable source of truth. `generation` changes on
-- every producer write so a worker cannot acknowledge a newer hint that
-- arrived while it held the row lease.
CREATE TABLE IF NOT EXISTS loyal_yield.fleet_planning_dirty_vaults (
    cluster TEXT NOT NULL,
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id) ON DELETE CASCADE,
    reasons TEXT[] NOT NULL,
    maximum_observed_slot BIGINT,
    first_dirty_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_dirty_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    generation BIGINT NOT NULL DEFAULT 1,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, vault_id),
    CONSTRAINT fleet_planning_dirty_vaults_evidence_check CHECK (
        cardinality(reasons) > 0
        AND NOT ('' = ANY(reasons))
        AND (maximum_observed_slot IS NULL OR maximum_observed_slot >= 0)
        AND last_dirty_at >= first_dirty_at
        AND fencing_token >= 0
        AND generation > 0
        AND attempt_count >= 0
        AND (
            lease_owner IS NULL
            OR lease_expires_at IS NOT NULL
        )
    )
);

CREATE INDEX IF NOT EXISTS fleet_planning_dirty_vaults_ready_idx
    ON loyal_yield.fleet_planning_dirty_vaults
        (cluster, available_at, last_dirty_at, vault_id)
    INCLUDE (lease_expires_at, generation)
    WHERE lease_owner IS NULL OR lease_expires_at IS NOT NULL;

CREATE OR REPLACE FUNCTION loyal_yield.enqueue_fleet_planning_dirty_vault(
    p_vault_id BIGINT,
    p_reason TEXT,
    p_observed_slot BIGINT DEFAULT NULL,
    p_available_at TIMESTAMPTZ DEFAULT now(),
    p_cluster TEXT DEFAULT NULL
)
RETURNS VOID
LANGUAGE plpgsql
AS $$
BEGIN
    IF p_vault_id IS NULL
       OR NULLIF(btrim(p_reason), '') IS NULL
       OR NULLIF(btrim(p_cluster), '') IS NULL
       OR p_available_at IS NULL
       OR (p_observed_slot IS NOT NULL AND p_observed_slot < 0)
    THEN
        RAISE EXCEPTION 'fleet planning dirty hint requires vault, reason, cluster, availability, and a nonnegative optional slot';
    END IF;

    INSERT INTO loyal_yield.fleet_planning_dirty_vaults
        (cluster, vault_id, reasons, maximum_observed_slot, available_at)
    VALUES
        (p_cluster, p_vault_id, ARRAY[btrim(p_reason)], p_observed_slot, p_available_at)
    ON CONFLICT (cluster, vault_id) DO UPDATE
    SET reasons = ARRAY(
            SELECT DISTINCT reason
            FROM unnest(
                loyal_yield.fleet_planning_dirty_vaults.reasons
                || EXCLUDED.reasons
            ) AS reason
            ORDER BY reason
        ),
        maximum_observed_slot = CASE
            WHEN loyal_yield.fleet_planning_dirty_vaults.maximum_observed_slot IS NULL
                THEN EXCLUDED.maximum_observed_slot
            WHEN EXCLUDED.maximum_observed_slot IS NULL
                THEN loyal_yield.fleet_planning_dirty_vaults.maximum_observed_slot
            ELSE GREATEST(
                loyal_yield.fleet_planning_dirty_vaults.maximum_observed_slot,
                EXCLUDED.maximum_observed_slot
            )
        END,
        last_dirty_at = clock_timestamp(),
        available_at = LEAST(
            loyal_yield.fleet_planning_dirty_vaults.available_at,
            EXCLUDED.available_at
        ),
        generation = loyal_yield.fleet_planning_dirty_vaults.generation + 1,
        updated_at = now();

    -- Delivery is deliberately lossy; the indexed row above is authoritative.
    PERFORM pg_notify(
        'loyal_yield_fleet_planner_wakeup',
        json_build_object(
            'cluster', p_cluster,
            'vault_id', p_vault_id,
            'reason', btrim(p_reason)
        )::text
    );
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.enqueue_fleet_planning_dirty_vault_for_registered_clusters(
    p_vault_id BIGINT,
    p_reason TEXT,
    p_observed_slot BIGINT DEFAULT NULL,
    p_available_at TIMESTAMPTZ DEFAULT now()
)
RETURNS VOID
LANGUAGE plpgsql
AS $$
DECLARE
    registered_cluster RECORD;
BEGIN
    FOR registered_cluster IN
        SELECT cluster
        FROM loyal_yield.fleet_planning_clusters
        ORDER BY cluster
    LOOP
        PERFORM loyal_yield.enqueue_fleet_planning_dirty_vault(
            p_vault_id,
            p_reason,
            p_observed_slot,
            p_available_at,
            registered_cluster.cluster
        );
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.mark_fleet_position_dirty()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    affected_vault_id BIGINT;
    affected_slot BIGINT;
BEGIN
    affected_vault_id := CASE WHEN TG_OP = 'DELETE' THEN OLD.vault_id ELSE NEW.vault_id END;
    affected_slot := CASE WHEN TG_OP = 'DELETE' THEN OLD.observed_slot ELSE NEW.observed_slot END;
    PERFORM loyal_yield.enqueue_fleet_planning_dirty_vault_for_registered_clusters(
        affected_vault_id,
        CASE WHEN TG_TABLE_NAME = 'vault_idle_token_balances_current'
            THEN 'idle_balance' ELSE 'reserve_position' END,
        affected_slot
    );
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$;

DROP TRIGGER IF EXISTS vault_reserve_position_fleet_planning_dirty
    ON loyal_yield.vault_reserve_positions_current;
CREATE TRIGGER vault_reserve_position_fleet_planning_dirty
AFTER INSERT OR UPDATE OR DELETE
ON loyal_yield.vault_reserve_positions_current
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.mark_fleet_position_dirty();

DROP TRIGGER IF EXISTS vault_idle_balance_fleet_planning_dirty
    ON loyal_yield.vault_idle_token_balances_current;
CREATE TRIGGER vault_idle_balance_fleet_planning_dirty
AFTER INSERT OR UPDATE OR DELETE
ON loyal_yield.vault_idle_token_balances_current
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.mark_fleet_position_dirty();

CREATE OR REPLACE FUNCTION loyal_yield.mark_managed_vault_fleet_planning_dirty()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM loyal_yield.enqueue_fleet_planning_dirty_vault_for_registered_clusters(
        NEW.id,
        'vault_or_policy_binding',
        NULL
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS managed_vault_fleet_planning_dirty
    ON loyal_yield.managed_vaults;
CREATE TRIGGER managed_vault_fleet_planning_dirty
AFTER INSERT OR UPDATE OF active, active_policy_id
ON loyal_yield.managed_vaults
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.mark_managed_vault_fleet_planning_dirty();

CREATE OR REPLACE FUNCTION loyal_yield.mark_policy_fleet_planning_dirty()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    affected_vault RECORD;
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.active IS NOT DISTINCT FROM OLD.active
       AND NEW.delegated_signers IS NOT DISTINCT FROM OLD.delegated_signers
       AND NEW.route_modes IS NOT DISTINCT FROM OLD.route_modes
       AND NEW.stable_mints IS NOT DISTINCT FROM OLD.stable_mints
       AND NEW.kamino_markets IS NOT DISTINCT FROM OLD.kamino_markets
       AND NEW.kamino_liquidity_mints IS NOT DISTINCT FROM OLD.kamino_liquidity_mints
    THEN
        RETURN NEW;
    END IF;

    FOR affected_vault IN
        SELECT vault.id
        FROM loyal_yield.managed_vaults vault
        WHERE vault.active_policy_id = NEW.id
    LOOP
        PERFORM loyal_yield.enqueue_fleet_planning_dirty_vault_for_registered_clusters(
            affected_vault.id,
            'policy',
            NEW.last_seen_slot
        );
    END LOOP;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS route_policy_fleet_planning_dirty
    ON loyal_yield.route_policies;
CREATE TRIGGER route_policy_fleet_planning_dirty
AFTER INSERT OR UPDATE OF
    active, delegated_signers, route_modes, stable_mints,
    kamino_markets, kamino_liquidity_mints
ON loyal_yield.route_policies
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.mark_policy_fleet_planning_dirty();

-- A confirmed route is ineligible during cooldown, then becomes dirty exactly
-- when the default five-minute planner cooldown expires. An earlier balance or
-- policy event pulls `available_at` forward through the coalescing upsert.
CREATE OR REPLACE FUNCTION loyal_yield.schedule_fleet_cooldown_dirty()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.status::TEXT = 'confirmed'
       AND (TG_OP = 'INSERT' OR OLD.status::TEXT <> 'confirmed')
    THEN
        PERFORM loyal_yield.enqueue_fleet_planning_dirty_vault_for_registered_clusters(
            NEW.vault_id,
            'cooldown_elapsed',
            NEW.confirmed_slot,
            NEW.updated_at + INTERVAL '5 minutes'
        );
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_decision_fleet_cooldown_dirty
    ON loyal_yield.rebalance_decisions;
CREATE TRIGGER rebalance_decision_fleet_cooldown_dirty
AFTER INSERT OR UPDATE OF status
ON loyal_yield.rebalance_decisions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.schedule_fleet_cooldown_dirty();

CREATE TABLE IF NOT EXISTS loyal_yield.rebalance_opportunities (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    vault_id BIGINT NOT NULL REFERENCES loyal_yield.managed_vaults(id),
    source_snapshot_id BIGINT REFERENCES loyal_yield.vault_position_snapshots(id),
    optimizer_epoch_id BIGINT NOT NULL REFERENCES loyal_yield.optimizer_epochs(id),
    route_fingerprint TEXT,
    requirements_fingerprint TEXT,
    source_reserve TEXT,
    target_reserve TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    amount_raw BIGINT NOT NULL,
    principal_usd_micros BIGINT NOT NULL,
    source_apy_bps BIGINT NOT NULL,
    target_apy_bps BIGINT NOT NULL,
    estimated_edge_bps BIGINT NOT NULL,
    estimated_cost_lamports BIGINT NOT NULL DEFAULT 0,
    annual_yield_gain_usd_micros BIGINT NOT NULL,
    expected_net_gain_usd_micros BIGINT NOT NULL,
    economic_priority BIGINT NOT NULL,
    -- Ordering by economic_priority + age_seconds is equivalent to ordering
    -- by this immutable-at-write anchor because the current epoch second is
    -- the same additive constant for every candidate. Persisting the anchor
    -- keeps unbounded aging/fairness indexable instead of sorting the entire
    -- runnable fleet on every claim.
    scheduler_priority_anchor NUMERIC(30, 0) NOT NULL,
    priority_version TEXT NOT NULL,
    opportunity_state TEXT NOT NULL,
    execution_plan JSONB NOT NULL DEFAULT '{}'::jsonb,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    lease_kind TEXT,
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    decision_id BIGINT UNIQUE REFERENCES loyal_yield.rebalance_decisions(id),
    terminal_reason TEXT,
    state_entered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at TIMESTAMPTZ,
    waiting_alt_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT rebalance_opportunities_state_check CHECK (
        opportunity_state IN (
            'waiting_alt', 'revalidate', 'ready', 'leased',
            'decision_created', 'completed', 'stale', 'superseded', 'failed', 'cancelled'
        )
    ),
    CONSTRAINT rebalance_opportunities_value_check CHECK (
        amount_raw > 0
        AND principal_usd_micros > 0
        AND estimated_edge_bps > 0
        AND estimated_cost_lamports >= 0
        AND annual_yield_gain_usd_micros > 0
        AND expected_net_gain_usd_micros > 0
        AND economic_priority > 0
        AND jsonb_typeof(execution_plan) = 'object'
    ),
    CONSTRAINT rebalance_opportunities_exact_route_check CHECK (
        NOT (
            opportunity_state IN ('waiting_alt', 'ready')
            OR (opportunity_state = 'leased' AND lease_kind = 'execute')
        )
        OR (
            NULLIF(btrim(route_fingerprint), '') IS NOT NULL
            AND NULLIF(btrim(requirements_fingerprint), '') IS NOT NULL
        )
    ),
    CONSTRAINT rebalance_opportunities_time_check CHECK (
        expires_at > created_at
        AND available_at < expires_at
        AND (lease_expires_at IS NULL OR lease_expires_at > created_at)
    ),
    CONSTRAINT rebalance_opportunities_lease_check CHECK (
        fencing_token >= 0
        AND attempt_count >= 0
        AND (
            opportunity_state <> 'leased'
            OR (
                lease_kind IN ('execute', 'revalidate')
                AND lease_owner IS NOT NULL
                AND lease_expires_at IS NOT NULL
            )
        )
        AND (
            opportunity_state = 'leased'
            OR (lease_kind IS NULL AND lease_owner IS NULL AND lease_expires_at IS NULL)
        )
    ),
    CONSTRAINT rebalance_opportunities_decision_check CHECK (
        (opportunity_state IN ('decision_created', 'completed') AND decision_id IS NOT NULL)
        OR (opportunity_state = 'failed')
        OR (
            opportunity_state NOT IN ('decision_created', 'completed', 'failed')
            AND decision_id IS NULL
        )
    )
);

ALTER TABLE loyal_yield.rebalance_opportunities
    ADD COLUMN IF NOT EXISTS scheduler_priority_anchor NUMERIC(30, 0);

UPDATE loyal_yield.rebalance_opportunities
SET scheduler_priority_anchor =
        economic_priority::NUMERIC - floor(EXTRACT(EPOCH FROM created_at))
WHERE scheduler_priority_anchor IS DISTINCT FROM
      economic_priority::NUMERIC - floor(EXTRACT(EPOCH FROM created_at));

ALTER TABLE loyal_yield.rebalance_opportunities
    ALTER COLUMN scheduler_priority_anchor SET NOT NULL;

CREATE OR REPLACE FUNCTION loyal_yield.stamp_rebalance_opportunity_scheduler_priority()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.scheduler_priority_anchor :=
        NEW.economic_priority::NUMERIC
        - floor(EXTRACT(EPOCH FROM NEW.created_at));
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_opportunity_scheduler_priority
    ON loyal_yield.rebalance_opportunities;
CREATE TRIGGER rebalance_opportunity_scheduler_priority
BEFORE INSERT OR UPDATE OF economic_priority, created_at, scheduler_priority_anchor
ON loyal_yield.rebalance_opportunities
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.stamp_rebalance_opportunity_scheduler_priority();

CREATE OR REPLACE FUNCTION loyal_yield.stamp_rebalance_opportunity_state_entry()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT'
       OR NEW.opportunity_state IS DISTINCT FROM OLD.opportunity_state
    THEN
        NEW.state_entered_at := clock_timestamp();
        IF NEW.opportunity_state = 'ready' THEN
            NEW.ready_at := COALESCE(NEW.ready_at, NEW.state_entered_at);
        ELSIF NEW.opportunity_state = 'waiting_alt' THEN
            NEW.waiting_alt_at := COALESCE(
                NEW.waiting_alt_at,
                NEW.state_entered_at
            );
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_opportunity_state_entry
    ON loyal_yield.rebalance_opportunities;
CREATE TRIGGER rebalance_opportunity_state_entry
BEFORE INSERT OR UPDATE OF opportunity_state
ON loyal_yield.rebalance_opportunities
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.stamp_rebalance_opportunity_state_entry();

-- One current scheduling intent per vault. Keep this uniqueness slot outside
-- the high-churn queue table. A partial unique index containing waiting_alt
-- rows is rewritten for every ready -> leased transition and makes an ALT-cold
-- cohort a direct tax on otherwise runnable claims.
CREATE TABLE IF NOT EXISTS loyal_yield.active_rebalance_opportunity_slots (
    vault_id BIGINT PRIMARY KEY
        REFERENCES loyal_yield.managed_vaults(id) ON DELETE CASCADE,
    cluster TEXT NOT NULL,
    opportunity_id BIGINT NOT NULL UNIQUE
        REFERENCES loyal_yield.rebalance_opportunities(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION loyal_yield.sync_active_rebalance_opportunity_slot()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    old_active BOOLEAN;
    new_active BOOLEAN;
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM loyal_yield.active_rebalance_opportunity_slots
        WHERE opportunity_id = OLD.id;
        RETURN OLD;
    END IF;

    new_active := NEW.opportunity_state IN (
        'waiting_alt', 'revalidate', 'ready', 'leased'
    );
    IF TG_OP = 'INSERT' THEN
        IF new_active THEN
            INSERT INTO loyal_yield.active_rebalance_opportunity_slots
                (vault_id, cluster, opportunity_id)
            VALUES (NEW.vault_id, NEW.cluster, NEW.id);
        END IF;
        RETURN NEW;
    END IF;

    old_active := OLD.opportunity_state IN (
        'waiting_alt', 'revalidate', 'ready', 'leased'
    );
    IF old_active AND new_active
       AND OLD.vault_id = NEW.vault_id
       AND OLD.cluster = NEW.cluster
    THEN
        -- The common ready -> leased transition performs no slot-table I/O.
        RETURN NEW;
    END IF;

    IF old_active THEN
        DELETE FROM loyal_yield.active_rebalance_opportunity_slots
        WHERE opportunity_id = OLD.id;
    END IF;
    IF new_active THEN
        INSERT INTO loyal_yield.active_rebalance_opportunity_slots
            (vault_id, cluster, opportunity_id)
        VALUES (NEW.vault_id, NEW.cluster, NEW.id);
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_opportunity_active_slot
    ON loyal_yield.rebalance_opportunities;
CREATE TRIGGER rebalance_opportunity_active_slot
AFTER INSERT OR UPDATE OF opportunity_state, cluster, vault_id OR DELETE
ON loyal_yield.rebalance_opportunities
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.sync_active_rebalance_opportunity_slot();

INSERT INTO loyal_yield.active_rebalance_opportunity_slots
    (vault_id, cluster, opportunity_id)
SELECT opportunity.vault_id, opportunity.cluster, opportunity.id
FROM loyal_yield.rebalance_opportunities opportunity
WHERE opportunity.opportunity_state IN (
    'waiting_alt', 'revalidate', 'ready', 'leased'
)
ON CONFLICT (vault_id) DO NOTHING;

DROP INDEX IF EXISTS loyal_yield.rebalance_opportunities_one_active_vault_idx;

-- The priority/age anchor lets SKIP LOCKED consumers reach the most valuable
-- executable work while guaranteeing eventual progress. Active leases must
-- not remain in this hot index: otherwise every subsequent claim walks past
-- the whole in-flight wave before it reaches the next runnable row.
DROP INDEX IF EXISTS loyal_yield.rebalance_opportunities_ready_priority_idx;
CREATE INDEX IF NOT EXISTS rebalance_opportunities_ready_priority_idx
    ON loyal_yield.rebalance_opportunities
        (
            cluster,
            opportunity_state,
            scheduler_priority_anchor DESC,
            economic_priority DESC,
            created_at,
            id
        )
    INCLUDE (available_at, expires_at)
    WHERE opportunity_state IN ('ready', 'revalidate');

DROP INDEX IF EXISTS loyal_yield.rebalance_opportunities_expired_lease_idx;
CREATE INDEX IF NOT EXISTS rebalance_opportunities_expired_lease_idx
    ON loyal_yield.rebalance_opportunities
        (cluster, lease_kind, lease_expires_at, id)
    INCLUDE (
        scheduler_priority_anchor,
        economic_priority,
        created_at,
        available_at,
        expires_at
    )
    WHERE opportunity_state = 'leased';

CREATE INDEX IF NOT EXISTS rebalance_opportunities_status_idx
    ON loyal_yield.rebalance_opportunities
        (cluster, opportunity_state, created_at, id)
    INCLUDE (
        principal_usd_micros,
        annual_yield_gain_usd_micros,
        lease_expires_at
    );

ALTER TABLE loyal_yield.lookup_table_provisioning_requests
    ADD COLUMN IF NOT EXISTS economic_priority BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS active_consumer_count INTEGER NOT NULL DEFAULT 0;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'loyal_yield.lookup_table_provisioning_requests'::regclass
          AND conname = 'lookup_table_provisioning_requests_consumer_priority_check'
    ) THEN
        ALTER TABLE loyal_yield.lookup_table_provisioning_requests
            ADD CONSTRAINT lookup_table_provisioning_requests_consumer_priority_check
            CHECK (economic_priority >= 0 AND active_consumer_count >= 0);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_provisioning_request_consumers (
    opportunity_id BIGINT PRIMARY KEY
        REFERENCES loyal_yield.rebalance_opportunities(id),
    provisioning_request_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_provisioning_requests(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS loyal_yield.orchestration_outbox (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    event_kind TEXT NOT NULL,
    aggregate_kind TEXT NOT NULL,
    aggregate_id BIGINT NOT NULL,
    dedupe_key TEXT NOT NULL UNIQUE,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    processed_at TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT orchestration_outbox_payload_check CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT orchestration_outbox_lease_check CHECK (
        fencing_token >= 0 AND attempt_count >= 0
        AND (
            lease_owner IS NULL
            OR (lease_expires_at IS NOT NULL AND processed_at IS NULL)
        )
    )
);

CREATE INDEX IF NOT EXISTS orchestration_outbox_pending_idx
    ON loyal_yield.orchestration_outbox
        (cluster, available_at, created_at, id)
    WHERE processed_at IS NULL;

CREATE TABLE IF NOT EXISTS loyal_yield.signed_route_submissions (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    semantic_key TEXT NOT NULL UNIQUE,
    opportunity_id BIGINT NOT NULL REFERENCES loyal_yield.rebalance_opportunities(id),
    decision_id BIGINT REFERENCES loyal_yield.rebalance_decisions(id),
    signed_transaction BYTEA NOT NULL,
    signed_transaction_hash TEXT NOT NULL,
    message_hash TEXT NOT NULL,
    transaction_signature TEXT NOT NULL UNIQUE,
    recent_blockhash TEXT NOT NULL,
    last_valid_block_height BIGINT NOT NULL,
    source_snapshot_id BIGINT REFERENCES loyal_yield.vault_position_snapshots(id),
    optimizer_epoch_id BIGINT NOT NULL REFERENCES loyal_yield.optimizer_epochs(id),
    alt_requirements_fingerprint TEXT NOT NULL,
    alt_selection_fingerprint TEXT NOT NULL,
    alt_mutation_epochs JSONB NOT NULL,
    fee_payer TEXT NOT NULL,
    compiled_fee_lamports BIGINT NOT NULL,
    writable_account_keys TEXT[] NOT NULL,
    conflict_account_keys TEXT[] NOT NULL,
    executor_owner TEXT NOT NULL,
    executor_fencing_token BIGINT NOT NULL,
    submission_state TEXT NOT NULL DEFAULT 'signed',
    submission_state_entered_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    submitted_slot BIGINT,
    submitted_at TIMESTAMPTZ,
    confirmed_slot BIGINT,
    confirmed_at TIMESTAMPTZ,
    reconciled_slot BIGINT,
    reconciled_at TIMESTAMPTZ,
    error_detail TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT signed_route_submissions_state_check CHECK (
        submission_state IN (
            'signed', 'submitted', 'confirmed', 'reconciliation_pending',
            'expiry_check_pending', 'effect_ambiguous',
            'reconciled', 'expired', 'failed'
        )
    ),
    CONSTRAINT signed_route_submissions_evidence_check CHECK (
        octet_length(signed_transaction) > 0
        AND NULLIF(btrim(signed_transaction_hash), '') IS NOT NULL
        AND NULLIF(btrim(message_hash), '') IS NOT NULL
        AND last_valid_block_height >= 0
        AND compiled_fee_lamports >= 0
        AND executor_fencing_token >= 0
        AND cardinality(writable_account_keys) > 0
        AND fee_payer = ANY(writable_account_keys)
        AND cardinality(conflict_account_keys) >= 2
        AND jsonb_typeof(alt_mutation_epochs) = 'object'
    )
);

COMMENT ON COLUMN loyal_yield.signed_route_submissions.writable_account_keys IS
    'Complete exact writable pubkeys from the compiled transaction; immutable audit evidence.';
COMMENT ON COLUMN loyal_yield.signed_route_submissions.conflict_account_keys IS
    'Vault-exclusive semantic key plus bounded fleet shared-write lane held through reconciliation.';
COMMENT ON COLUMN loyal_yield.signed_route_submissions.submission_state_entered_at IS
    'Timestamp of the current submission state, changed only when submission_state changes; used for stage-stall health.';

CREATE OR REPLACE FUNCTION loyal_yield.set_signed_route_submission_state_entered_at()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.submission_state IS DISTINCT FROM OLD.submission_state THEN
        NEW.submission_state_entered_at := clock_timestamp();
    ELSE
        NEW.submission_state_entered_at := OLD.submission_state_entered_at;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS signed_route_submission_state_entered_at
    ON loyal_yield.signed_route_submissions;
CREATE TRIGGER signed_route_submission_state_entered_at
BEFORE UPDATE ON loyal_yield.signed_route_submissions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.set_signed_route_submission_state_entered_at();

CREATE INDEX IF NOT EXISTS signed_route_submissions_state_idx
    ON loyal_yield.signed_route_submissions
        (cluster, submission_state, created_at, id);

CREATE UNIQUE INDEX IF NOT EXISTS signed_route_submissions_opportunity_fence_uidx
    ON loyal_yield.signed_route_submissions
        (opportunity_id, executor_fencing_token);

CREATE TABLE IF NOT EXISTS loyal_yield.route_account_conflict_leases (
    cluster TEXT NOT NULL,
    writable_account_key TEXT NOT NULL,
    opportunity_id BIGINT NOT NULL REFERENCES loyal_yield.rebalance_opportunities(id),
    lease_owner TEXT NOT NULL,
    fencing_token BIGINT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, writable_account_key),
    CONSTRAINT route_account_conflict_leases_fence_check CHECK (
        fencing_token >= 0
        AND expires_at > created_at
        AND NULLIF(btrim(writable_account_key), '') IS NOT NULL
    )
);

COMMENT ON TABLE loyal_yield.route_account_conflict_leases IS
    'Semantic vault and bounded shared-write lane ownership; exact transaction writables live on signed_route_submissions.';

CREATE INDEX IF NOT EXISTS route_account_conflict_leases_opportunity_idx
    ON loyal_yield.route_account_conflict_leases
        (opportunity_id, expires_at, writable_account_key);

CREATE OR REPLACE VIEW loyal_yield.fleet_orchestration_status AS
WITH clusters AS (
    SELECT cluster FROM loyal_yield.fleet_planning_clusters
    UNION
    SELECT cluster FROM loyal_yield.fleet_planning_state
    UNION
    SELECT cluster FROM loyal_yield.optimizer_epochs
    UNION
    SELECT cluster FROM loyal_yield.rebalance_opportunities
    UNION
    SELECT cluster FROM loyal_yield.orchestration_outbox
    UNION
    SELECT cluster FROM loyal_yield.signed_route_submissions
), latest_market_epoch AS (
    SELECT DISTINCT ON (cluster)
           cluster,
           id,
           epoch_key,
           market_slot,
           observed_at,
           expires_at
    FROM loyal_yield.optimizer_epochs
    ORDER BY cluster, observed_at DESC, id DESC
), opportunity_status AS (
    SELECT cluster,
           opportunity_state,
           count(*)::BIGINT AS opportunity_count,
           COALESCE(sum(principal_usd_micros), 0)::BIGINT AS principal_usd_micros,
           COALESCE(sum(annual_yield_gain_usd_micros), 0)::BIGINT
               AS annual_yield_gain_usd_micros,
           min(created_at) AS oldest_created_at,
           min(state_entered_at) AS oldest_state_entered_at,
           count(*) FILTER (
               WHERE opportunity_state = 'leased' AND lease_expires_at <= now()
           )::BIGINT AS expired_lease_count
    FROM loyal_yield.rebalance_opportunities
    GROUP BY cluster, opportunity_state
), queue_status AS (
    SELECT cluster,
           count(*) FILTER (
               WHERE opportunity_state = 'waiting_alt'
           )::BIGINT AS waiting_alt_opportunity_count,
           COALESCE(sum(principal_usd_micros) FILTER (
               WHERE opportunity_state = 'waiting_alt'
           ), 0)::BIGINT AS waiting_alt_principal_usd_micros,
           COALESCE((sum(annual_yield_gain_usd_micros) FILTER (
               WHERE opportunity_state = 'waiting_alt'
           ) / 8760)::BIGINT, 0)::BIGINT
               AS waiting_alt_yield_gain_usd_micros_per_hour,
           min(state_entered_at) FILTER (
               WHERE opportunity_state = 'waiting_alt'
           ) AS oldest_waiting_alt_state_entered_at,
           count(*) FILTER (
               WHERE opportunity_state = 'ready'
           )::BIGINT AS ready_opportunity_count,
           COALESCE(sum(principal_usd_micros) FILTER (
               WHERE opportunity_state = 'ready'
           ), 0)::BIGINT AS ready_principal_usd_micros,
           COALESCE((sum(annual_yield_gain_usd_micros) FILTER (
               WHERE opportunity_state = 'ready'
           ) / 8760)::BIGINT, 0)::BIGINT
               AS ready_yield_gain_usd_micros_per_hour,
           min(state_entered_at) FILTER (
               WHERE opportunity_state = 'ready'
           ) AS oldest_ready_state_entered_at
    FROM loyal_yield.rebalance_opportunities
    GROUP BY cluster
), outbox_status AS (
    SELECT cluster,
           count(*) FILTER (WHERE processed_at IS NULL)::BIGINT AS pending_outbox_count
    FROM loyal_yield.orchestration_outbox
    GROUP BY cluster
), submission_status AS (
    SELECT cluster,
           count(*) FILTER (
               WHERE submission_state NOT IN ('reconciled', 'expired', 'failed')
           )::BIGINT AS pending_submission_count,
           COALESCE(sum(compiled_fee_lamports) FILTER (
               WHERE submission_state NOT IN ('reconciled', 'expired', 'failed')
           ), 0)::BIGINT AS pending_compiled_fee_lamports,
           count(*) FILTER (
               WHERE submission_state = 'expiry_check_pending'
           )::BIGINT AS expiry_check_pending_count,
           count(*) FILTER (
               WHERE submission_state = 'effect_ambiguous'
           )::BIGINT AS effect_ambiguous_count,
           min(created_at) FILTER (
               WHERE submission_state NOT IN ('reconciled', 'expired', 'failed')
           ) AS oldest_pending_submission_at,
           count(*) FILTER (
               WHERE submission_state = 'signed'
           )::BIGINT AS sender_submission_count,
           min(submission_state_entered_at) FILTER (
               WHERE submission_state = 'signed'
           ) AS oldest_sender_state_entered_at,
           count(*) FILTER (
               WHERE submission_state IN ('submitted', 'confirmed')
           )::BIGINT AS confirmer_submission_count,
           min(submission_state_entered_at) FILTER (
               WHERE submission_state IN ('submitted', 'confirmed')
           ) AS oldest_confirmer_state_entered_at,
           count(*) FILTER (
               WHERE submission_state IN (
                   'reconciliation_pending',
                   'expiry_check_pending',
                   'effect_ambiguous'
               )
           )::BIGINT AS reconciler_submission_count,
           min(submission_state_entered_at) FILTER (
               WHERE submission_state IN (
                   'reconciliation_pending',
                   'expiry_check_pending',
                   'effect_ambiguous'
               )
           ) AS oldest_reconciler_state_entered_at
    FROM loyal_yield.signed_route_submissions
    GROUP BY cluster
), submission_lifecycle AS (
    SELECT opportunity_id,
           min(submitted_at) FILTER (
               WHERE submitted_at IS NOT NULL
           ) AS first_submitted_at,
           min(confirmed_at) FILTER (
               WHERE confirmed_at IS NOT NULL
           ) AS first_confirmed_at,
           COALESCE(sum(compiled_fee_lamports), 0)::BIGINT
               AS compiled_fee_lamports
    FROM loyal_yield.signed_route_submissions
    GROUP BY opportunity_id
), current_epoch_unlock AS (
    -- Queue admission already requires positive annual and expected net gain.
    -- Grouping by the latest immutable market epoch makes the denominator
    -- explicit and prevents a lifetime aggregate from hiding a new slowdown.
    SELECT epoch.cluster,
           count(opportunity.id)::BIGINT AS current_epoch_opportunity_count,
           COALESCE(sum(opportunity.principal_usd_micros), 0)::BIGINT
               AS current_epoch_principal_usd_micros,
           COALESCE((sum(opportunity.annual_yield_gain_usd_micros) / 8760)::BIGINT, 0)::BIGINT
               AS current_epoch_recoverable_yield_usd_micros_per_hour,
           COALESCE(floor(
               1000000::NUMERIC
               * COALESCE(sum(opportunity.annual_yield_gain_usd_micros) FILTER (
                   WHERE lifecycle.first_submitted_at
                       <= opportunity.created_at + interval '10 seconds'
               ), 0)
               / NULLIF(sum(opportunity.annual_yield_gain_usd_micros), 0)
           )::BIGINT, 0)::BIGINT
               AS current_epoch_submitted_within_10s_yield_ppm,
           COALESCE(floor(
               1000000::NUMERIC
               * COALESCE(sum(opportunity.annual_yield_gain_usd_micros) FILTER (
                   WHERE lifecycle.first_submitted_at
                       <= opportunity.created_at + interval '2 minutes'
               ), 0)
               / NULLIF(sum(opportunity.annual_yield_gain_usd_micros), 0)
           )::BIGINT, 0)::BIGINT
               AS current_epoch_submitted_within_2m_yield_ppm,
           COALESCE(floor(
               1000000::NUMERIC
               * COALESCE(sum(opportunity.annual_yield_gain_usd_micros) FILTER (
                   WHERE lifecycle.first_submitted_at
                       <= opportunity.created_at + interval '10 minutes'
               ), 0)
               / NULLIF(sum(opportunity.annual_yield_gain_usd_micros), 0)
           )::BIGINT, 0)::BIGINT
               AS current_epoch_submitted_within_10m_yield_ppm,
           COALESCE(floor(
               1000000::NUMERIC
               * COALESCE(sum(opportunity.annual_yield_gain_usd_micros) FILTER (
                   WHERE lifecycle.first_confirmed_at
                       <= opportunity.created_at + interval '30 seconds'
               ), 0)
               / NULLIF(sum(opportunity.annual_yield_gain_usd_micros), 0)
           )::BIGINT, 0)::BIGINT
               AS current_epoch_confirmed_within_30s_yield_ppm,
           ceil(percentile_cont(0.95) WITHIN GROUP (
               ORDER BY GREATEST(
                   extract(epoch FROM (
                       lifecycle.first_submitted_at - opportunity.created_at
                   )) * 1000,
                   0
               )
           ) FILTER (
               WHERE lifecycle.first_submitted_at IS NOT NULL
           ))::BIGINT AS current_epoch_submission_p95_milliseconds,
           ceil(percentile_cont(0.95) WITHIN GROUP (
               ORDER BY GREATEST(
                   extract(epoch FROM (
                       lifecycle.first_confirmed_at - opportunity.created_at
                   )) * 1000,
                   0
               )
           ) FILTER (
               WHERE lifecycle.first_confirmed_at IS NOT NULL
           ))::BIGINT AS current_epoch_confirmation_p95_milliseconds,
           COALESCE(sum(lifecycle.compiled_fee_lamports), 0)::BIGINT
               AS current_epoch_compiled_fee_lamports
    FROM latest_market_epoch epoch
    LEFT JOIN loyal_yield.rebalance_opportunities opportunity
      ON opportunity.cluster = epoch.cluster
     AND opportunity.optimizer_epoch_id = epoch.id
    LEFT JOIN submission_lifecycle lifecycle
      ON lifecycle.opportunity_id = opportunity.id
    GROUP BY epoch.cluster
)
SELECT cluster.cluster,
       opportunity.opportunity_state,
       COALESCE(opportunity.opportunity_count, 0)::BIGINT AS opportunity_count,
       COALESCE(opportunity.principal_usd_micros, 0) AS principal_usd_micros,
       COALESCE(opportunity.annual_yield_gain_usd_micros, 0)
           AS annual_yield_gain_usd_micros,
       COALESCE((opportunity.annual_yield_gain_usd_micros / 8760)::BIGINT, 0)::BIGINT
           AS yield_gain_usd_micros_per_hour,
       opportunity.oldest_created_at,
       opportunity.oldest_state_entered_at,
       CASE
           WHEN opportunity.oldest_created_at IS NULL THEN NULL
           ELSE extract(epoch FROM now() - opportunity.oldest_created_at)::BIGINT
       END AS oldest_age_seconds,
       CASE
           WHEN opportunity.oldest_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - opportunity.oldest_state_entered_at
           )::BIGINT
       END AS oldest_state_age_seconds,
       COALESCE(opportunity.expired_lease_count, 0)::BIGINT AS expired_lease_count,
       COALESCE(outbox.pending_outbox_count, 0)::BIGINT AS pending_outbox_count,
       COALESCE(submission.pending_submission_count, 0)::BIGINT
           AS pending_submission_count,
       COALESCE(submission.pending_compiled_fee_lamports, 0)::BIGINT
           AS pending_compiled_fee_lamports,
       COALESCE(submission.expiry_check_pending_count, 0)::BIGINT
           AS expiry_check_pending_count,
       COALESCE(submission.effect_ambiguous_count, 0)::BIGINT
           AS effect_ambiguous_count,
       submission.oldest_pending_submission_at,
       CASE
           WHEN submission.oldest_pending_submission_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - submission.oldest_pending_submission_at
           )::BIGINT
       END AS oldest_pending_submission_age_seconds
       , COALESCE(submission.sender_submission_count, 0)::BIGINT
           AS sender_submission_count
       , submission.oldest_sender_state_entered_at
       , CASE
           WHEN submission.oldest_sender_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - submission.oldest_sender_state_entered_at
           )::BIGINT
         END AS oldest_sender_state_age_seconds
       , COALESCE(submission.confirmer_submission_count, 0)::BIGINT
           AS confirmer_submission_count
       , submission.oldest_confirmer_state_entered_at
       , CASE
           WHEN submission.oldest_confirmer_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - submission.oldest_confirmer_state_entered_at
           )::BIGINT
         END AS oldest_confirmer_state_age_seconds
       , COALESCE(submission.reconciler_submission_count, 0)::BIGINT
           AS reconciler_submission_count
       , submission.oldest_reconciler_state_entered_at
       , CASE
           WHEN submission.oldest_reconciler_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - submission.oldest_reconciler_state_entered_at
           )::BIGINT
         END AS oldest_reconciler_state_age_seconds
       , planning_cluster.registered_at AS planner_registered_at
       , planning_cluster.last_seen_at AS planner_last_seen_at
       , CASE
           WHEN planning_cluster.last_seen_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - planning_cluster.last_seen_at
           )::BIGINT
         END AS planner_last_seen_age_seconds
       , planning.full_sweep_started_at
       , planning.full_sweep_completed_at
       , CASE
           WHEN planning.full_sweep_completed_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - planning.full_sweep_completed_at
           )::BIGINT
         END AS full_sweep_age_seconds
       , planning.optimizer_epoch_key AS planned_optimizer_epoch_key
       , planning.optimizer_epoch_expires_at AS planned_optimizer_epoch_expires_at
       , planning.complete_frontier
       , planning.observed_vault_count
       , planning.opportunity_count AS planned_opportunity_count
       , planning.selected_count AS planned_selected_count
       , planning.deferred_count AS planned_deferred_count
       , planning.generation AS planning_generation
       , latest_epoch.id AS latest_market_epoch_id
       , latest_epoch.epoch_key AS latest_market_epoch_key
       , latest_epoch.market_slot AS latest_market_slot
       , latest_epoch.observed_at AS latest_market_observed_at
       , latest_epoch.expires_at AS latest_market_expires_at
       , CASE
           WHEN latest_epoch.observed_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - latest_epoch.observed_at
           )::BIGINT
         END AS latest_market_epoch_age_seconds
       , CASE
           WHEN latest_epoch.expires_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM latest_epoch.expires_at - now()
           )::BIGINT
         END AS latest_market_epoch_expires_in_seconds
       , CASE
           WHEN latest_epoch.expires_at IS NULL THEN NULL
           ELSE latest_epoch.expires_at <= now()
         END AS latest_market_epoch_expired
       , CASE
           WHEN planning.optimizer_epoch_key IS NULL
             OR latest_epoch.epoch_key IS NULL THEN NULL
           ELSE planning.optimizer_epoch_key = latest_epoch.epoch_key
         END AS planner_epoch_matches_latest
       , COALESCE(queue.waiting_alt_opportunity_count, 0)::BIGINT
           AS waiting_alt_opportunity_count
       , COALESCE(queue.waiting_alt_principal_usd_micros, 0)::BIGINT
           AS waiting_alt_principal_usd_micros
       , COALESCE(queue.waiting_alt_yield_gain_usd_micros_per_hour, 0)::BIGINT
           AS waiting_alt_yield_gain_usd_micros_per_hour
       , queue.oldest_waiting_alt_state_entered_at
       , CASE
           WHEN queue.oldest_waiting_alt_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - queue.oldest_waiting_alt_state_entered_at
           )::BIGINT
         END AS oldest_waiting_alt_state_age_seconds
       , COALESCE(queue.ready_opportunity_count, 0)::BIGINT
           AS ready_opportunity_count
       , COALESCE(queue.ready_principal_usd_micros, 0)::BIGINT
           AS ready_principal_usd_micros
       , COALESCE(queue.ready_yield_gain_usd_micros_per_hour, 0)::BIGINT
           AS ready_yield_gain_usd_micros_per_hour
       , queue.oldest_ready_state_entered_at
       , CASE
           WHEN queue.oldest_ready_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - queue.oldest_ready_state_entered_at
           )::BIGINT
         END AS oldest_ready_state_age_seconds
       , COALESCE(unlock.current_epoch_opportunity_count, 0)::BIGINT
           AS current_epoch_opportunity_count
       , COALESCE(unlock.current_epoch_principal_usd_micros, 0)::BIGINT
           AS current_epoch_principal_usd_micros
       , COALESCE(unlock.current_epoch_recoverable_yield_usd_micros_per_hour, 0)::BIGINT
           AS current_epoch_recoverable_yield_usd_micros_per_hour
       , COALESCE(unlock.current_epoch_submitted_within_10s_yield_ppm, 0)::BIGINT
           AS current_epoch_submitted_within_10s_yield_ppm
       , COALESCE(unlock.current_epoch_submitted_within_2m_yield_ppm, 0)::BIGINT
           AS current_epoch_submitted_within_2m_yield_ppm
       , COALESCE(unlock.current_epoch_submitted_within_10m_yield_ppm, 0)::BIGINT
           AS current_epoch_submitted_within_10m_yield_ppm
       , COALESCE(unlock.current_epoch_confirmed_within_30s_yield_ppm, 0)::BIGINT
           AS current_epoch_confirmed_within_30s_yield_ppm
       , unlock.current_epoch_submission_p95_milliseconds
       , unlock.current_epoch_confirmation_p95_milliseconds
       , COALESCE(unlock.current_epoch_compiled_fee_lamports, 0)::BIGINT
           AS current_epoch_compiled_fee_lamports
FROM clusters cluster
LEFT JOIN loyal_yield.fleet_planning_clusters planning_cluster USING (cluster)
LEFT JOIN loyal_yield.fleet_planning_state planning USING (cluster)
LEFT JOIN latest_market_epoch latest_epoch USING (cluster)
LEFT JOIN opportunity_status opportunity USING (cluster)
LEFT JOIN queue_status queue USING (cluster)
LEFT JOIN outbox_status outbox USING (cluster)
LEFT JOIN submission_status submission USING (cluster)
LEFT JOIN current_epoch_unlock unlock USING (cluster);

CREATE INDEX IF NOT EXISTS lookup_table_provisioning_request_consumers_request_idx
    ON loyal_yield.lookup_table_provisioning_request_consumers
        (provisioning_request_id, opportunity_id);

CREATE INDEX IF NOT EXISTS lookup_table_provisioning_requests_priority_queue_idx
    ON loyal_yield.lookup_table_provisioning_requests (
        cluster,
        request_status,
        economic_priority DESC,
        next_attempt_at,
        lease_expires_at,
        requested_at,
        id
    )
    WHERE request_status IN ('requested', 'planning', 'queued', 'failed');

CREATE OR REPLACE FUNCTION loyal_yield.refresh_lookup_table_request_consumer_priority(
    refreshed_request_id BIGINT
)
RETURNS VOID
LANGUAGE plpgsql
AS $$
DECLARE
    next_priority BIGINT;
    next_count INTEGER;
BEGIN
    SELECT COALESCE(max(opportunity.economic_priority), 0), count(*)::INTEGER
    INTO next_priority, next_count
    FROM loyal_yield.lookup_table_provisioning_request_consumers consumer
    JOIN loyal_yield.rebalance_opportunities opportunity
      ON opportunity.id = consumer.opportunity_id
    WHERE consumer.provisioning_request_id = refreshed_request_id
      AND opportunity.opportunity_state = 'waiting_alt';

    UPDATE loyal_yield.lookup_table_provisioning_requests
    SET economic_priority = next_priority,
        active_consumer_count = next_count,
        updated_at = now()
    WHERE id = refreshed_request_id
      AND (
          economic_priority IS DISTINCT FROM next_priority
          OR active_consumer_count IS DISTINCT FROM next_count
      );
END;
$$;

CREATE OR REPLACE FUNCTION loyal_yield.refresh_lookup_table_request_priority_for_consumer()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        PERFORM loyal_yield.refresh_lookup_table_request_consumer_priority(
            OLD.provisioning_request_id
        );
    END IF;
    IF TG_OP <> 'DELETE'
       AND (TG_OP = 'INSERT'
            OR NEW.provisioning_request_id IS DISTINCT FROM OLD.provisioning_request_id)
    THEN
        PERFORM loyal_yield.refresh_lookup_table_request_consumer_priority(
            NEW.provisioning_request_id
        );
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_request_consumer_priority
    ON loyal_yield.lookup_table_provisioning_request_consumers;
CREATE TRIGGER lookup_table_request_consumer_priority
AFTER INSERT OR UPDATE OR DELETE
ON loyal_yield.lookup_table_provisioning_request_consumers
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.refresh_lookup_table_request_priority_for_consumer();

CREATE OR REPLACE FUNCTION loyal_yield.refresh_lookup_table_request_priority_for_opportunity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    refreshed_request_id BIGINT;
BEGIN
    IF NEW.opportunity_state IS NOT DISTINCT FROM OLD.opportunity_state
       AND NEW.economic_priority IS NOT DISTINCT FROM OLD.economic_priority
    THEN
        RETURN NEW;
    END IF;

    SELECT consumer.provisioning_request_id
    INTO refreshed_request_id
    FROM loyal_yield.lookup_table_provisioning_request_consumers consumer
    WHERE consumer.opportunity_id = NEW.id;

    IF refreshed_request_id IS NOT NULL THEN
        PERFORM loyal_yield.refresh_lookup_table_request_consumer_priority(
            refreshed_request_id
        );
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_opportunity_request_priority
    ON loyal_yield.rebalance_opportunities;

-- Do not synchronously refresh request priority from an opportunity UPDATE.
-- ALT satisfaction holds request -> opportunity, so the reverse trigger order
-- can deadlock normal queue transitions. Provisioner claims derive live
-- aggregate consumer value; the stored columns remain a best-effort snapshot
-- refreshed when consumer links themselves change.

CREATE OR REPLACE FUNCTION loyal_yield.notify_rebalance_opportunity_wakeup()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.opportunity_state NOT IN ('ready', 'revalidate') THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE'
       AND OLD.opportunity_state = NEW.opportunity_state
       AND OLD.available_at = NEW.available_at
    THEN
        RETURN NEW;
    END IF;

    PERFORM pg_notify(
        'loyal_yield_rebalance_wakeup',
        json_build_object(
            'cluster', NEW.cluster,
            'opportunity_id', NEW.id,
            'state', NEW.opportunity_state
        )::text
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_opportunity_wakeup
    ON loyal_yield.rebalance_opportunities;
CREATE TRIGGER rebalance_opportunity_wakeup
AFTER INSERT OR UPDATE OF opportunity_state, available_at
ON loyal_yield.rebalance_opportunities
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.notify_rebalance_opportunity_wakeup();

CREATE OR REPLACE FUNCTION loyal_yield.wake_rebalance_opportunities_for_satisfied_alt_request()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.request_status <> 'satisfied'
       OR OLD.request_status = 'satisfied'
    THEN
        RETURN NEW;
    END IF;

    -- Coverage completion is a wakeup hint, not economic admission. Keep the
    -- opportunity ALT-cold until a current planner wave selects the exact
    -- optimizer epoch and explicitly re-admits it.
    WITH affected AS (
        SELECT opportunity.id, opportunity.cluster
        FROM loyal_yield.rebalance_opportunities opportunity
        JOIN loyal_yield.lookup_table_provisioning_request_consumers consumer
          ON opportunity.id = consumer.opportunity_id
        WHERE consumer.provisioning_request_id = NEW.id
          AND opportunity.opportunity_state = 'waiting_alt'
          AND opportunity.expires_at > now()
    )
    INSERT INTO loyal_yield.orchestration_outbox
        (cluster, event_kind, aggregate_kind, aggregate_id, dedupe_key, payload)
    SELECT affected.cluster,
           'alt_satisfied',
           'rebalance_opportunity',
           affected.id,
           concat('alt_satisfied:', NEW.id, ':', NEW.fencing_token, ':', affected.id),
           jsonb_build_object(
               'opportunity_id', affected.id,
               'provisioning_request_id', NEW.id,
               'requirements_fingerprint', NEW.requirements_fingerprint,
               'planner_readmission_required', TRUE
           )
    FROM affected
    ON CONFLICT (dedupe_key) DO NOTHING;

    PERFORM loyal_yield.enqueue_fleet_planning_dirty_vault(
        opportunity.vault_id,
        'alt_ready',
        NULL,
        now(),
        opportunity.cluster
    )
    FROM loyal_yield.rebalance_opportunities opportunity
    JOIN loyal_yield.lookup_table_provisioning_request_consumers consumer
      ON opportunity.id = consumer.opportunity_id
    WHERE consumer.provisioning_request_id = NEW.id
      AND opportunity.opportunity_state = 'waiting_alt'
      AND opportunity.expires_at > now();

    PERFORM pg_notify(
        'loyal_yield_fleet_planner_wakeup',
        json_build_object(
            'cluster', NEW.cluster,
            'provisioning_request_id', NEW.id,
            'reason', 'alt_satisfied'
        )::text
    );

    RETURN NEW;
END;
$$;

-- A decision and its leased execution opportunity are one durable handoff.
-- Existing non-queued decision paths remain valid when no execute lease exists.
CREATE OR REPLACE FUNCTION loyal_yield.link_rebalance_decision_to_execute_opportunity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    leased_opportunity loyal_yield.rebalance_opportunities%ROWTYPE;
    linked_submission_count INTEGER;
BEGIN
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

    IF NEW.source_snapshot_id IS DISTINCT FROM leased_opportunity.source_snapshot_id
       OR NEW.source_reserve IS DISTINCT FROM leased_opportunity.source_reserve
       OR NEW.target_reserve IS DISTINCT FROM leased_opportunity.target_reserve
       OR NEW.liquidity_mint IS DISTINCT FROM leased_opportunity.liquidity_mint
       OR NEW.amount_raw IS DISTINCT FROM leased_opportunity.amount_raw
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

DROP TRIGGER IF EXISTS rebalance_decision_links_execute_opportunity
    ON loyal_yield.rebalance_decisions;
CREATE TRIGGER rebalance_decision_links_execute_opportunity
AFTER INSERT ON loyal_yield.rebalance_decisions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.link_rebalance_decision_to_execute_opportunity();

DROP TRIGGER IF EXISTS lookup_table_request_rebalance_wakeup
    ON loyal_yield.lookup_table_provisioning_requests;
CREATE TRIGGER lookup_table_request_rebalance_wakeup
AFTER UPDATE OF request_status
ON loyal_yield.lookup_table_provisioning_requests
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.wake_rebalance_opportunities_for_satisfied_alt_request();

-- Routine readiness and usage-lease writes must not take the rollout-wide
-- advisory lock. Lock only the selected physical rows, in canonical order.
-- Lifecycle/provisioner writers take FOR UPDATE on the same rows, so retirement
-- remains fenced without serializing unrelated reusable route traffic.
CREATE OR REPLACE FUNCTION loyal_yield.guard_retired_legacy_lookup_table_reference()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    referenced_ids BIGINT[];
    referenced_cluster TEXT;
    active_reference BOOLEAN;
BEGIN
    IF TG_TABLE_NAME = 'lookup_table_route_readiness_current' THEN
        referenced_ids := COALESCE(NEW.legacy_table_ids, '{}'::BIGINT[])
            || COALESCE(NEW.selected_table_ids, '{}'::BIGINT[]);
        referenced_cluster := NEW.cluster;
        active_reference := TRUE;
    ELSIF TG_TABLE_NAME = 'lookup_table_usage_leases' THEN
        referenced_ids := ARRAY[NEW.route_lookup_table_id]::BIGINT[];
        referenced_cluster := NEW.cluster;
        active_reference := NEW.released_at IS NULL AND NEW.expires_at > now();
    ELSE
        referenced_ids := ARRAY[NEW.route_lookup_table_id]::BIGINT[];
        SELECT cluster INTO referenced_cluster
        FROM loyal_yield.route_lookup_tables
        WHERE id = NEW.route_lookup_table_id;
        active_reference := NEW.operation_state NOT IN (
            'complete', 'permanent_failure', 'cancelled'
        );
    END IF;

    IF NOT active_reference THEN
        RETURN NEW;
    END IF;

    PERFORM route_table.id
    FROM loyal_yield.route_lookup_tables route_table
    WHERE route_table.id = ANY(referenced_ids)
    ORDER BY route_table.id
    FOR SHARE;

    IF EXISTS (
        SELECT 1
        FROM loyal_yield.route_lookup_tables route_table
        WHERE route_table.id = ANY(referenced_ids)
          AND route_table.family_id IS NULL
          AND route_table.legacy_import_run_id IS NOT NULL
          AND (
              route_table.durable = FALSE
              OR route_table.status NOT IN ('active', 'warming', 'usable')
          )
    ) THEN
        RAISE EXCEPTION 'retired imported legacy lookup table cannot acquire a new control-plane reference';
    END IF;
    RETURN NEW;
END;
$$;
