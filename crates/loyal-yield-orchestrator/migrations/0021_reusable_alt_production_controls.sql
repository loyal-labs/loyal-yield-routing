-- Cluster-scoped production controls for the reusable ALT control plane.
--
-- A missing row means provisioning is enabled. Operators mutate this row only
-- through the provisioner's fenced admin commands. The monotonic control epoch
-- gives every watcher a durable value to report and makes state changes
-- auditable without relying on process-local flags.

CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_provisioner_controls (
    cluster TEXT PRIMARY KEY,
    paused BOOLEAN NOT NULL DEFAULT FALSE,
    reason TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    control_epoch BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_provisioner_controls_epoch_check
        CHECK (control_epoch >= 0),
    CONSTRAINT lookup_table_provisioner_controls_text_check
        CHECK (
            length(btrim(cluster)) > 0
            AND length(btrim(reason)) > 0
            AND length(btrim(updated_by)) > 0
        )
);

-- A worker must durably acquire one exact signed-transaction permit before it
-- may cross the network broadcast boundary. Granting a permit and changing the
-- cluster pause both lock the provisioner-control row in short transactions.
-- Therefore a pause either prevents the grant or observes the already-granted
-- mutation as durable in-flight work. No database transaction is held while
-- the RPC send is in progress.
CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_provisioner_broadcast_permits (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL
        REFERENCES loyal_yield.lookup_table_provisioner_controls(cluster),
    operation_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_operations(id),
    fencing_token BIGINT NOT NULL,
    control_epoch BIGINT NOT NULL,
    transaction_signature TEXT NOT NULL,
    message_hash TEXT NOT NULL,
    permit_state TEXT NOT NULL DEFAULT 'granted',
    resolution_detail TEXT,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_provisioner_broadcast_permits_identity_unique
        UNIQUE (operation_id, fencing_token),
    CONSTRAINT lookup_table_provisioner_broadcast_permits_identity_check
        CHECK (
            length(btrim(cluster)) > 0
            AND fencing_token > 0
            AND control_epoch >= 0
            AND length(btrim(transaction_signature)) > 0
            AND length(btrim(message_hash)) > 0
        ),
    CONSTRAINT lookup_table_provisioner_broadcast_permits_state_check
        CHECK (
            permit_state IN (
                'granted', 'submitted', 'needs_reconcile', 'expired',
                'reconciled', 'failed'
            )
            AND ((permit_state = 'granted' AND resolved_at IS NULL)
                 OR (permit_state <> 'granted' AND resolved_at IS NOT NULL))
        )
);

CREATE UNIQUE INDEX IF NOT EXISTS lookup_table_provisioner_broadcast_permits_active_idx
    ON loyal_yield.lookup_table_provisioner_broadcast_permits (operation_id)
    WHERE resolved_at IS NULL;

CREATE INDEX IF NOT EXISTS lookup_table_provisioner_broadcast_permits_cluster_active_idx
    ON loyal_yield.lookup_table_provisioner_broadcast_permits
        (cluster, granted_at, id)
    WHERE resolved_at IS NULL;

-- A pre-cutover probe executes the real drift and demand-request store paths
-- in one transaction, observes their effects, and rolls them back to a
-- savepoint while retaining the paused control-row lock. Only this immutable
-- PASS summary commits. It proves the finalized
-- shared table was exact, drift did not manufacture vault demand, duplicate
-- demand sealed exactly one request, and no decision, binding, operation,
-- signer load, transaction send, or rollback residue occurred.
CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_precutover_probe_runs (
    id BIGSERIAL PRIMARY KEY,
    probe_token TEXT NOT NULL UNIQUE,
    cluster TEXT NOT NULL,
    vault_id BIGINT NOT NULL
        REFERENCES loyal_yield.managed_vaults(id),
    catalog_revision_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_shared_market_catalog_revisions(id),
    shared_manifest_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_manifests(id),
    route_lookup_table_id BIGINT NOT NULL
        REFERENCES loyal_yield.route_lookup_tables(id),
    shared_table_address TEXT NOT NULL,
    shared_authority TEXT NOT NULL,
    shared_mutation_epoch BIGINT NOT NULL,
    provisioner_control_epoch BIGINT NOT NULL,
    requirements_fingerprint TEXT NOT NULL UNIQUE,
    finalized_slot BIGINT NOT NULL,
    finalized_last_extended_slot BIGINT NOT NULL,
    finalized_address_hash TEXT NOT NULL,
    finalized_address_count INTEGER NOT NULL,
    finalized_shared_exact BOOLEAN NOT NULL,
    synthetic_drift_evidence_hash TEXT NOT NULL,
    drift_signal_count INTEGER NOT NULL,
    drift_provisioning_request_count INTEGER NOT NULL,
    duplicate_request_attempt_count INTEGER NOT NULL,
    distinct_request_count INTEGER NOT NULL,
    decision_count INTEGER NOT NULL,
    binding_count INTEGER NOT NULL,
    operation_count INTEGER NOT NULL,
    rollback_residue_count INTEGER NOT NULL,
    catalog_head_restored BOOLEAN NOT NULL,
    signer_loaded BOOLEAN NOT NULL,
    transactions_sent BOOLEAN NOT NULL,
    result TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_precutover_probe_identity_check
        CHECK (
            probe_token ~ '^[0-9a-f]{64}$'
            AND requirements_fingerprint ~ '^[0-9a-f]{64}$'
            AND finalized_address_hash ~ '^[0-9a-f]{64}$'
            AND synthetic_drift_evidence_hash ~ '^[0-9a-f]{64}$'
            AND length(btrim(shared_table_address)) > 0
            AND length(btrim(shared_authority)) > 0
            AND shared_mutation_epoch >= 0
            AND provisioner_control_epoch >= 0
            AND finalized_slot >= 0
            AND finalized_last_extended_slot >= 0
            AND finalized_last_extended_slot < finalized_slot
            AND finalized_address_count BETWEEN 1 AND 256
        ),
    CONSTRAINT lookup_table_precutover_probe_pass_check
        CHECK (
            result = 'pass'
            AND finalized_shared_exact
            AND drift_signal_count = 1
            AND drift_provisioning_request_count = 0
            AND duplicate_request_attempt_count = 2
            AND distinct_request_count = 1
            AND decision_count = 0
            AND binding_count = 0
            AND operation_count = 0
            AND rollback_residue_count = 0
            AND catalog_head_restored
            AND NOT signer_loaded
            AND NOT transactions_sent
        )
);

CREATE INDEX IF NOT EXISTS lookup_table_precutover_probe_runs_cluster_idx
    ON loyal_yield.lookup_table_precutover_probe_runs (cluster, created_at DESC, id DESC);

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_precutover_probe_run_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'pre-cutover probe audit rows are immutable';
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_precutover_probe_runs_immutable
    ON loyal_yield.lookup_table_precutover_probe_runs;
CREATE TRIGGER lookup_table_precutover_probe_runs_immutable
    BEFORE UPDATE OR DELETE ON loyal_yield.lookup_table_precutover_probe_runs
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_precutover_probe_run_mutation();

-- Durable rule identity/configuration. The catalog is seeded with the complete
-- nine-condition contract. Rules may be deliberately enabled or disabled, but
-- every configuration change must advance its version exactly once.
CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_alert_rules (
    rule_key TEXT PRIMARY KEY,
    rule_version BIGINT NOT NULL DEFAULT 1,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    severity TEXT NOT NULL,
    description TEXT NOT NULL,
    configuration JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_alert_rules_key_check CHECK (
        rule_key IN (
            'readiness_regression',
            'missing_coverage',
            'operation_backlog',
            'capacity_headroom',
            'authority_prefix_drift',
            'provisioning_budget',
            'orphaned_tables',
            'fallback_use',
            'cleanup_anomalies'
        )
    ),
    CONSTRAINT lookup_table_alert_rules_version_check
        CHECK (rule_version > 0),
    CONSTRAINT lookup_table_alert_rules_severity_check
        CHECK (severity IN ('warning', 'critical')),
    CONSTRAINT lookup_table_alert_rules_configuration_check
        CHECK (jsonb_typeof(configuration) = 'object'),
    CONSTRAINT lookup_table_alert_rules_text_check
        CHECK (length(btrim(description)) > 0 AND updated_at >= created_at)
);

INSERT INTO loyal_yield.lookup_table_alert_rules
    (rule_key, severity, description, configuration)
VALUES
    ('readiness_regression', 'critical',
     'durable shared-market ALT readiness regressed', '{}'::jsonb),
    ('missing_coverage', 'warning',
     'route coverage remained missing beyond its provisioning grace period',
     '{"threshold":"missing_coverage_grace"}'::jsonb),
    ('operation_backlog', 'warning',
     'reusable-ALT provisioning operations are backlogged or terminally failed',
     '{"thresholds":["operation_backlog_age","operation_backlog_depth"]}'::jsonb),
    ('capacity_headroom', 'warning',
     'packed vault ALT capacity is below reserved expansion headroom',
     '{"threshold":"capacity_headroom"}'::jsonb),
    ('authority_prefix_drift', 'critical',
     'finalized ALT authority or ordered address prefix drifted', '{}'::jsonb),
    ('provisioning_budget', 'warning',
     'reusable-ALT provisioning budget is near its limit or exhausted',
     '{"thresholds":["budget_max_lamports","budget_alert_percent","budget_window"]}'::jsonb),
    ('orphaned_tables', 'warning',
     'reusable ALTs exist without a live control-plane reference', '{}'::jsonb),
    ('fallback_use', 'critical',
     'legacy fallback or a non-reusable-only rollout control is active', '{}'::jsonb),
    ('cleanup_anomalies', 'warning',
     'legacy ALT cleanup, close refund, or physical evidence is anomalous',
     '{"threshold":"cleanup_grace"}'::jsonb)
ON CONFLICT (rule_key) DO NOTHING;

CREATE INDEX IF NOT EXISTS lookup_table_alert_rules_enabled_idx
    ON loyal_yield.lookup_table_alert_rules (enabled, rule_key);

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_alert_rule_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reusable ALT alert rules cannot be deleted';
    END IF;
    IF NEW.rule_key IS DISTINCT FROM OLD.rule_key
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'reusable ALT alert rule identity is immutable';
    END IF;
    IF NEW.enabled IS DISTINCT FROM OLD.enabled
       OR NEW.severity IS DISTINCT FROM OLD.severity
       OR NEW.description IS DISTINCT FROM OLD.description
       OR NEW.configuration IS DISTINCT FROM OLD.configuration
    THEN
        IF NEW.rule_version <> OLD.rule_version + 1
           OR NEW.updated_at <= OLD.updated_at
        THEN
            RAISE EXCEPTION 'reusable ALT alert rule changes require one version advance';
        END IF;
    ELSIF NEW.rule_version IS DISTINCT FROM OLD.rule_version
          OR NEW.updated_at IS DISTINCT FROM OLD.updated_at
    THEN
        RAISE EXCEPTION 'reusable ALT alert rule metadata cannot advance without a change';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_alert_rules_guard
    ON loyal_yield.lookup_table_alert_rules;
CREATE TRIGGER lookup_table_alert_rules_guard
    BEFORE UPDATE OR DELETE ON loyal_yield.lookup_table_alert_rules
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_alert_rule_mutation();

-- Semantic alert incidents are durable state, not process-local counters.
-- There is exactly one incident identity per condition/scope. Repeated scans
-- update occurrence evidence in place; open/reminder/resolved notifications
-- advance a monotonic revision and atomically enqueue one delivery row.
CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_alert_incidents (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    policy_pubkey TEXT NOT NULL,
    alert_condition TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    incident_status TEXT NOT NULL,
    severity TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    summary TEXT NOT NULL,
    details JSONB NOT NULL,
    first_observed_at TIMESTAMPTZ NOT NULL,
    opened_at TIMESTAMPTZ NOT NULL,
    last_observed_at TIMESTAMPTZ NOT NULL,
    last_notified_at TIMESTAMPTZ NOT NULL,
    occurrence_count BIGINT NOT NULL,
    revision BIGINT NOT NULL,
    resolved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_alert_incidents_identity_unique
        UNIQUE (cluster, policy_pubkey, alert_condition, scope_key),
    CONSTRAINT lookup_table_alert_incidents_id_condition_unique
        UNIQUE (id, alert_condition),
    CONSTRAINT lookup_table_alert_incidents_rule_fkey
        FOREIGN KEY (alert_condition)
        REFERENCES loyal_yield.lookup_table_alert_rules(rule_key),
    CONSTRAINT lookup_table_alert_incidents_condition_check CHECK (
        alert_condition IN (
            'readiness_regression',
            'missing_coverage',
            'operation_backlog',
            'capacity_headroom',
            'authority_prefix_drift',
            'provisioning_budget',
            'orphaned_tables',
            'fallback_use',
            'cleanup_anomalies'
        )
    ),
    CONSTRAINT lookup_table_alert_incidents_status_check
        CHECK (incident_status IN ('open', 'resolved')),
    CONSTRAINT lookup_table_alert_incidents_severity_check
        CHECK (severity IN ('info', 'warning', 'critical')),
    CONSTRAINT lookup_table_alert_incidents_fingerprint_check
        CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    CONSTRAINT lookup_table_alert_incidents_details_check
        CHECK (jsonb_typeof(details) = 'object'),
    CONSTRAINT lookup_table_alert_incidents_text_check CHECK (
        length(btrim(cluster)) > 0
        AND length(btrim(policy_pubkey)) > 0
        AND length(btrim(scope_key)) > 0
        AND length(btrim(summary)) > 0
    ),
    CONSTRAINT lookup_table_alert_incidents_counter_check
        CHECK (occurrence_count > 0 AND revision > 0),
    CONSTRAINT lookup_table_alert_incidents_time_check CHECK (
        opened_at >= first_observed_at
        AND last_observed_at >= opened_at
        AND last_notified_at >= first_observed_at
        AND updated_at >= created_at
        AND (
            (incident_status = 'open' AND resolved_at IS NULL)
            OR
            (incident_status = 'resolved'
             AND resolved_at IS NOT NULL
             AND resolved_at >= opened_at)
        )
    )
);

CREATE INDEX IF NOT EXISTS lookup_table_alert_incidents_open_idx
    ON loyal_yield.lookup_table_alert_incidents
        (cluster, policy_pubkey, alert_condition, last_observed_at DESC)
    WHERE incident_status = 'open';

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_alert_incident_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reusable ALT alert incidents cannot be deleted';
    END IF;
    IF NEW.cluster IS DISTINCT FROM OLD.cluster
       OR NEW.policy_pubkey IS DISTINCT FROM OLD.policy_pubkey
       OR NEW.alert_condition IS DISTINCT FROM OLD.alert_condition
       OR NEW.scope_key IS DISTINCT FROM OLD.scope_key
       OR NEW.first_observed_at IS DISTINCT FROM OLD.first_observed_at
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'reusable ALT alert incident identity is immutable';
    END IF;
    IF NEW.revision < OLD.revision
       OR NEW.occurrence_count < OLD.occurrence_count
       OR NEW.updated_at < OLD.updated_at
    THEN
        RAISE EXCEPTION 'reusable ALT alert incident counters must be monotonic';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_alert_incidents_guard
    ON loyal_yield.lookup_table_alert_incidents;
CREATE TRIGGER lookup_table_alert_incidents_guard
    BEFORE UPDATE OR DELETE ON loyal_yield.lookup_table_alert_incidents
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_alert_incident_mutation();

-- The delivery outbox is independent from incident state so a worker crash or
-- webhook outage cannot lose the transition. Test deliveries deliberately
-- have no incident and therefore cannot manufacture route-demand evidence.
CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_alert_deliveries (
    id BIGSERIAL PRIMARY KEY,
    incident_id BIGINT,
    incident_revision BIGINT,
    alert_condition TEXT NOT NULL,
    event_kind TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    cluster TEXT NOT NULL,
    policy_pubkey TEXT NOT NULL,
    payload JSONB NOT NULL,
    delivery_state TEXT NOT NULL DEFAULT 'pending',
    delivered_via TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    http_status INTEGER,
    last_error TEXT,
    delivered_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_alert_deliveries_idempotency_key_unique
        UNIQUE (idempotency_key),
    CONSTRAINT lookup_table_alert_deliveries_incident_revision_unique
        UNIQUE (incident_id, incident_revision, event_kind),
    CONSTRAINT lookup_table_alert_deliveries_rule_fkey
        FOREIGN KEY (alert_condition)
        REFERENCES loyal_yield.lookup_table_alert_rules(rule_key),
    CONSTRAINT lookup_table_alert_deliveries_incident_fkey
        FOREIGN KEY (incident_id, alert_condition)
        REFERENCES loyal_yield.lookup_table_alert_incidents(id, alert_condition),
    CONSTRAINT lookup_table_alert_deliveries_event_check
        CHECK (event_kind IN ('open', 'reminder', 'resolved', 'test')),
    CONSTRAINT lookup_table_alert_deliveries_state_check CHECK (
        delivery_state IN (
            'pending', 'leased', 'retry_wait', 'delivered', 'dead_letter'
        )
    ),
    CONSTRAINT lookup_table_alert_deliveries_channel_check
        CHECK (delivered_via IS NULL OR delivered_via IN ('webhook', 'render_failure')),
    CONSTRAINT lookup_table_alert_deliveries_payload_check
        CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT lookup_table_alert_deliveries_identity_check CHECK (
        (
            event_kind = 'test'
            AND incident_id IS NULL
            AND incident_revision IS NULL
        )
        OR
        (
            event_kind <> 'test'
            AND incident_id IS NOT NULL
            AND incident_revision IS NOT NULL
            AND incident_revision > 0
        )
    ),
    CONSTRAINT lookup_table_alert_deliveries_counter_check CHECK (
        attempt_count >= 0
        AND max_attempts BETWEEN 1 AND 100
        AND fencing_token >= 0
    ),
    CONSTRAINT lookup_table_alert_deliveries_lease_check CHECK (
        (
            delivery_state = 'leased'
            AND lease_owner IS NOT NULL
            AND lease_expires_at IS NOT NULL
        )
        OR
        (
            delivery_state <> 'leased'
            AND lease_owner IS NULL
            AND lease_expires_at IS NULL
        )
    ),
    CONSTRAINT lookup_table_alert_deliveries_completion_check CHECK (
        (
            delivery_state = 'delivered'
            AND delivered_at IS NOT NULL
            AND delivered_via IS NOT NULL
        )
        OR
        (
            delivery_state <> 'delivered'
            AND delivered_at IS NULL
            AND delivered_via IS NULL
        )
    ),
    CONSTRAINT lookup_table_alert_deliveries_http_check
        CHECK (http_status IS NULL OR http_status BETWEEN 100 AND 599),
    CONSTRAINT lookup_table_alert_deliveries_text_check CHECK (
        length(btrim(idempotency_key)) > 0
        AND length(btrim(cluster)) > 0
        AND length(btrim(policy_pubkey)) > 0
    )
);

CREATE INDEX IF NOT EXISTS lookup_table_alert_deliveries_work_idx
    ON loyal_yield.lookup_table_alert_deliveries
        (delivery_state, next_attempt_at, lease_expires_at, id)
    WHERE delivery_state IN ('pending', 'leased', 'retry_wait');

CREATE INDEX IF NOT EXISTS lookup_table_alert_deliveries_incident_idx
    ON loyal_yield.lookup_table_alert_deliveries
        (incident_id, incident_revision, id);

-- Familyless imported legacy ALTs cannot use the reusable-family operation
-- queue, but their deactivation/close sends still require the same durable
-- signed-identity-before-broadcast invariant. One row is one immutable signed
-- attempt. Expired, unobserved attempts are retained before a fresh attempt is
-- allowed, so crash recovery never loses or blindly replays a transaction.
CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_legacy_cleanup_attempts (
    id BIGSERIAL PRIMARY KEY,
    route_lookup_table_id BIGINT NOT NULL
        REFERENCES loyal_yield.route_lookup_tables(id),
    cluster TEXT NOT NULL,
    table_address TEXT NOT NULL,
    operation_kind TEXT NOT NULL,
    attempt_number INTEGER NOT NULL,
    authorization_token TEXT NOT NULL,
    expected_authority TEXT NOT NULL,
    expected_address_count INTEGER NOT NULL,
    expected_address_hash TEXT NOT NULL,
    close_recipient TEXT,
    expected_reclaimed_lamports BIGINT,
    attempt_state TEXT NOT NULL DEFAULT 'prepared',
    transaction_signature TEXT UNIQUE,
    message_hash TEXT,
    recent_blockhash TEXT,
    last_valid_block_height BIGINT,
    estimated_fee_lamports BIGINT,
    recipient_balance_before BIGINT,
    submitted_at TIMESTAMPTZ,
    finalized_slot BIGINT,
    recipient_balance_after BIGINT,
    actual_reclaimed_lamports BIGINT,
    error_code TEXT,
    error_detail TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_legacy_cleanup_attempt_identity_unique
        UNIQUE (route_lookup_table_id, operation_kind, attempt_number),
    CONSTRAINT lookup_table_legacy_cleanup_attempt_kind_check
        CHECK (operation_kind IN ('deactivate', 'close')),
    CONSTRAINT lookup_table_legacy_cleanup_attempt_state_check CHECK (
        attempt_state IN (
            'prepared', 'signed', 'submitted', 'needs_reconcile',
            'expired', 'complete', 'permanent_failure'
        )
    ),
    CONSTRAINT lookup_table_legacy_cleanup_attempt_identity_check CHECK (
        attempt_number > 0
        AND authorization_token ~ '^[0-9a-f]{64}$'
        AND expected_address_hash ~ '^[0-9a-f]{64}$'
        AND expected_address_count BETWEEN 0 AND 256
        AND length(btrim(cluster)) > 0
        AND length(btrim(table_address)) > 0
        AND length(btrim(expected_authority)) > 0
    ),
    CONSTRAINT lookup_table_legacy_cleanup_attempt_refund_shape_check CHECK (
        (
            operation_kind = 'deactivate'
            AND close_recipient IS NULL
            AND expected_reclaimed_lamports IS NULL
            AND recipient_balance_before IS NULL
            AND recipient_balance_after IS NULL
            AND actual_reclaimed_lamports IS NULL
        )
        OR
        (
            operation_kind = 'close'
            AND close_recipient = expected_authority
            AND expected_reclaimed_lamports > 0
            AND (
                actual_reclaimed_lamports IS NULL
                OR actual_reclaimed_lamports = expected_reclaimed_lamports
            )
        )
    ),
    CONSTRAINT lookup_table_legacy_cleanup_attempt_signed_shape_check CHECK (
        (
            attempt_state = 'prepared'
            AND transaction_signature IS NULL
            AND message_hash IS NULL
            AND recent_blockhash IS NULL
            AND last_valid_block_height IS NULL
        )
        OR
        attempt_state = 'permanent_failure'
        OR
        (
            attempt_state IN (
                'signed', 'submitted', 'needs_reconcile', 'expired', 'complete'
            )
            AND transaction_signature IS NOT NULL
            AND message_hash IS NOT NULL
            AND recent_blockhash IS NOT NULL
            AND last_valid_block_height >= 0
            AND estimated_fee_lamports >= 0
        )
    ),
    CONSTRAINT lookup_table_legacy_cleanup_attempt_completion_check CHECK (
        (
            attempt_state = 'complete'
            AND finalized_slot >= 0
            AND (
                operation_kind = 'deactivate'
                OR (
                    recipient_balance_before IS NOT NULL
                    AND recipient_balance_after IS NOT NULL
                    AND actual_reclaimed_lamports = expected_reclaimed_lamports
                )
            )
        )
        OR
        (attempt_state <> 'complete' AND finalized_slot IS NULL)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS lookup_table_legacy_cleanup_attempt_active_unique
    ON loyal_yield.lookup_table_legacy_cleanup_attempts
        (route_lookup_table_id, operation_kind)
    WHERE attempt_state IN ('prepared', 'signed', 'submitted', 'needs_reconcile');

CREATE INDEX IF NOT EXISTS lookup_table_legacy_cleanup_attempt_recovery_idx
    ON loyal_yield.lookup_table_legacy_cleanup_attempts
        (cluster, attempt_state, updated_at, id)
    WHERE attempt_state IN ('prepared', 'signed', 'submitted', 'needs_reconcile');

-- Familyless imported cleanup spends from the same logical cluster rolling
-- budget as reusable-family operations. A separate immutable reservation row
-- preserves the cleanup-attempt foreign key while both reservation paths take
-- the same cluster advisory lock and share one accounting query.
CREATE TABLE IF NOT EXISTS loyal_yield.lookup_table_legacy_cleanup_budget_reservations (
    id BIGSERIAL PRIMARY KEY,
    legacy_cleanup_attempt_id BIGINT NOT NULL
        REFERENCES loyal_yield.lookup_table_legacy_cleanup_attempts(id),
    cluster TEXT NOT NULL,
    estimated_fee_lamports BIGINT NOT NULL,
    estimated_rent_lamports BIGINT NOT NULL,
    reserved_lamports BIGINT NOT NULL,
    reserved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    reserved_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lookup_table_legacy_cleanup_budget_attempt_unique
        UNIQUE (legacy_cleanup_attempt_id),
    CONSTRAINT lookup_table_legacy_cleanup_budget_amount_check CHECK (
        estimated_fee_lamports >= 0
        AND estimated_rent_lamports >= 0
        AND reserved_lamports = estimated_fee_lamports + estimated_rent_lamports
        AND reserved_lamports > 0
        AND reserved_until > reserved_at
        AND length(btrim(cluster)) > 0
    )
);

CREATE INDEX IF NOT EXISTS lookup_table_legacy_cleanup_budget_active_idx
    ON loyal_yield.lookup_table_legacy_cleanup_budget_reservations
        (cluster, reserved_until, legacy_cleanup_attempt_id);

CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_legacy_cleanup_budget_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'legacy lookup-table cleanup budget reservations are immutable';
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_legacy_cleanup_budget_reservations_immutable
    ON loyal_yield.lookup_table_legacy_cleanup_budget_reservations;
CREATE TRIGGER lookup_table_legacy_cleanup_budget_reservations_immutable
    BEFORE UPDATE OR DELETE
    ON loyal_yield.lookup_table_legacy_cleanup_budget_reservations
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_legacy_cleanup_budget_mutation();

-- The attempt cannot cross the signing boundary unless its exact simulated fee
-- and rent were durably approved first. This protects the ordering even if a
-- future caller bypasses the cleanup CLI.
CREATE OR REPLACE FUNCTION loyal_yield.guard_lookup_table_legacy_cleanup_attempt_budget()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.attempt_state IN ('signed', 'submitted', 'needs_reconcile', 'complete')
       AND NOT EXISTS (
           SELECT 1
           FROM loyal_yield.lookup_table_legacy_cleanup_budget_reservations reservation
           WHERE reservation.legacy_cleanup_attempt_id = NEW.id
             AND reservation.cluster = NEW.cluster
             AND reservation.estimated_fee_lamports = NEW.estimated_fee_lamports
             AND reservation.reserved_lamports =
                 NEW.estimated_fee_lamports + reservation.estimated_rent_lamports
             AND (
                 OLD.attempt_state <> 'prepared'
                 OR reservation.reserved_until > clock_timestamp()
             )
       )
    THEN
        RAISE EXCEPTION 'legacy cleanup signing requires an exact durable cluster budget reservation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_legacy_cleanup_attempt_budget_guard
    ON loyal_yield.lookup_table_legacy_cleanup_attempts;
CREATE TRIGGER lookup_table_legacy_cleanup_attempt_budget_guard
    BEFORE UPDATE ON loyal_yield.lookup_table_legacy_cleanup_attempts
    FOR EACH ROW
    EXECUTE FUNCTION loyal_yield.guard_lookup_table_legacy_cleanup_attempt_budget();
