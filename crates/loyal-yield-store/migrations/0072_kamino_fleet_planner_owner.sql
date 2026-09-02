-- Single-writer fencing for the first fixed-route Kamino fleet planner cohort.
-- Market snapshots remain process-local; optimizer_epochs and the existing
-- opportunity queue remain the durable handoff and recovery source of truth.

CREATE TABLE IF NOT EXISTS loyal_yield.kamino_fleet_planner_owners (
    cluster TEXT NOT NULL,
    cohort TEXT NOT NULL,
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    last_confirmed_slot BIGINT,
    last_snapshot_hash TEXT,
    last_decision_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (cluster, cohort),
    CONSTRAINT kamino_fleet_planner_owners_identity_check CHECK (
        NULLIF(btrim(cluster), '') IS NOT NULL
        AND NULLIF(btrim(cohort), '') IS NOT NULL
        AND fencing_token >= 0
        AND (last_confirmed_slot IS NULL OR last_confirmed_slot > 0)
        AND (last_snapshot_hash IS NULL OR NULLIF(btrim(last_snapshot_hash), '') IS NOT NULL)
        AND (
            (lease_owner IS NULL AND lease_expires_at IS NULL)
            OR (NULLIF(btrim(lease_owner), '') IS NOT NULL AND lease_expires_at IS NOT NULL)
        )
    )
);

CREATE INDEX IF NOT EXISTS kamino_fleet_planner_owners_expired_lease_idx
    ON loyal_yield.kamino_fleet_planner_owners (lease_expires_at)
    WHERE lease_owner IS NOT NULL;

COMMENT ON TABLE loyal_yield.kamino_fleet_planner_owners IS
    'Fenced single-writer ownership and observability watermark for the fixed-route Kamino fleet planner. This row is not executable market evidence.';
