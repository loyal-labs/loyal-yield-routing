CREATE TABLE IF NOT EXISTS loyal_yield.fleet_health_projection_leases (
    cluster TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    fencing_token BIGINT NOT NULL CHECK (fencing_token > 0),
    lease_expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (NULLIF(btrim(cluster), '') IS NOT NULL),
    CHECK (NULLIF(btrim(owner), '') IS NOT NULL)
);

CREATE TABLE IF NOT EXISTS loyal_yield.fleet_orchestration_health_snapshots (
    cluster TEXT PRIMARY KEY,
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'array'),
    source_watermark JSONB NOT NULL CHECK (jsonb_typeof(source_watermark) = 'object'),
    refresh_started_at TIMESTAMPTZ NOT NULL,
    refreshed_at TIMESTAMPTZ NOT NULL,
    refresh_duration_milliseconds BIGINT NOT NULL
        CHECK (refresh_duration_milliseconds >= 0),
    refresh_owner TEXT NOT NULL,
    fencing_token BIGINT NOT NULL CHECK (fencing_token > 0),
    row_count BIGINT NOT NULL CHECK (row_count >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (NULLIF(btrim(cluster), '') IS NOT NULL),
    CHECK (NULLIF(btrim(refresh_owner), '') IS NOT NULL),
    CHECK (refreshed_at >= refresh_started_at)
);

-- Durable dirty rows are authoritative. NOTIFY is only an edge hint: an
-- update to an already-dirty vault must not create another wakeup. The retry
-- loop closes the rare delete-between-insert-and-update race without losing
-- the producer event.
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
DECLARE
    inserted_new BOOLEAN := FALSE;
BEGIN
    IF p_vault_id IS NULL
       OR NULLIF(btrim(p_reason), '') IS NULL
       OR NULLIF(btrim(p_cluster), '') IS NULL
       OR p_available_at IS NULL
       OR (p_observed_slot IS NOT NULL AND p_observed_slot < 0)
    THEN
        RAISE EXCEPTION 'fleet planning dirty hint requires vault, reason, cluster, availability, and a nonnegative optional slot';
    END IF;

    LOOP
        inserted_new := FALSE;
        INSERT INTO loyal_yield.fleet_planning_dirty_vaults
            (cluster, vault_id, reasons, maximum_observed_slot, available_at)
        VALUES
            (p_cluster, p_vault_id, ARRAY[btrim(p_reason)], p_observed_slot, p_available_at)
        ON CONFLICT (cluster, vault_id) DO NOTHING
        RETURNING TRUE INTO inserted_new;

        IF inserted_new THEN
            EXIT;
        END IF;

        UPDATE loyal_yield.fleet_planning_dirty_vaults
        SET reasons = ARRAY(
                SELECT DISTINCT reason
                FROM unnest(
                    loyal_yield.fleet_planning_dirty_vaults.reasons
                    || ARRAY[btrim(p_reason)]
                ) AS reason
                ORDER BY reason
            ),
            maximum_observed_slot = CASE
                WHEN loyal_yield.fleet_planning_dirty_vaults.maximum_observed_slot IS NULL
                    THEN p_observed_slot
                WHEN p_observed_slot IS NULL
                    THEN loyal_yield.fleet_planning_dirty_vaults.maximum_observed_slot
                ELSE GREATEST(
                    loyal_yield.fleet_planning_dirty_vaults.maximum_observed_slot,
                    p_observed_slot
                )
            END,
            last_dirty_at = clock_timestamp(),
            available_at = LEAST(
                loyal_yield.fleet_planning_dirty_vaults.available_at,
                p_available_at
            ),
            generation = loyal_yield.fleet_planning_dirty_vaults.generation + 1,
            updated_at = now()
        WHERE cluster = p_cluster
          AND vault_id = p_vault_id;

        IF FOUND THEN
            EXIT;
        END IF;
    END LOOP;

    IF inserted_new THEN
        PERFORM pg_notify(
            'loyal_yield_fleet_planner_wakeup',
            json_build_object(
                'cluster', p_cluster,
                'vault_id', p_vault_id,
                'reason', btrim(p_reason)
            )::text
        );
    END IF;
END;
$$;
