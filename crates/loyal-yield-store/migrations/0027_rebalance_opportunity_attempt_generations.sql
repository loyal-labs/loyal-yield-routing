-- Immutable, concurrency-safe retry generations for durable fleet routes.
--
-- The economic identity remains stable when a source snapshot and optimizer
-- epoch are unchanged. A terminal attempt is never reopened: a new generation
-- is inserted only after the previous route has authoritative no-effect
-- evidence. The unique generation index plus the planner's per-vault lock make
-- concurrent rediscovery converge on exactly one retry row.

ALTER TABLE loyal_yield.rebalance_opportunities
    ADD COLUMN IF NOT EXISTS rediscovery_key TEXT,
    ADD COLUMN IF NOT EXISTS attempt_generation BIGINT NOT NULL DEFAULT 1;

UPDATE loyal_yield.rebalance_opportunities
SET rediscovery_key = idempotency_key
WHERE rediscovery_key IS NULL;

ALTER TABLE loyal_yield.rebalance_opportunities
    ALTER COLUMN rediscovery_key SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS rebalance_opportunities_rediscovery_generation_uidx
    ON loyal_yield.rebalance_opportunities
        (rediscovery_key, attempt_generation);

CREATE INDEX IF NOT EXISTS rebalance_opportunities_vault_rediscovery_latest_idx
    ON loyal_yield.rebalance_opportunities
        (cluster, vault_id, rediscovery_key, attempt_generation DESC, id DESC);

CREATE OR REPLACE FUNCTION loyal_yield.guard_rebalance_opportunity_attempt_identity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.rediscovery_key := COALESCE(NEW.rediscovery_key, NEW.idempotency_key);
        IF NULLIF(btrim(NEW.rediscovery_key), '') IS NULL
           OR NEW.attempt_generation <= 0
        THEN
            RAISE EXCEPTION
                'rebalance opportunity attempt identity must be nonempty and positive';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.rediscovery_key IS DISTINCT FROM OLD.rediscovery_key
       OR NEW.attempt_generation IS DISTINCT FROM OLD.attempt_generation
    THEN
        RAISE EXCEPTION
            'rebalance opportunity attempt identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_opportunity_attempt_identity_immutable
    ON loyal_yield.rebalance_opportunities;
CREATE TRIGGER rebalance_opportunity_attempt_identity_immutable
BEFORE INSERT OR UPDATE OF idempotency_key, rediscovery_key, attempt_generation
ON loyal_yield.rebalance_opportunities
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_rebalance_opportunity_attempt_identity();

-- Terminal failure is a hint, not proof. The planner immediately re-observes
-- the vault, while the atomic publisher independently checks terminal signed
-- evidence, released capacity, and released conflict leases before generating
-- a retry.
CREATE OR REPLACE FUNCTION loyal_yield.enqueue_terminal_route_retry_check()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.opportunity_state = 'failed'
       AND OLD.opportunity_state IS DISTINCT FROM NEW.opportunity_state
    THEN
        PERFORM loyal_yield.enqueue_fleet_planning_dirty_vault(
            NEW.vault_id,
            'terminal_no_effect_retry_check',
            NULL,
            clock_timestamp(),
            NEW.cluster
        );
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS rebalance_opportunity_terminal_retry_wakeup
    ON loyal_yield.rebalance_opportunities;
CREATE TRIGGER rebalance_opportunity_terminal_retry_wakeup
AFTER UPDATE OF opportunity_state
ON loyal_yield.rebalance_opportunities
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.enqueue_terminal_route_retry_check();

COMMENT ON COLUMN loyal_yield.rebalance_opportunities.rediscovery_key IS
    'Stable economic/source identity shared by immutable retry attempts.';
COMMENT ON COLUMN loyal_yield.rebalance_opportunities.attempt_generation IS
    'Monotonic retry generation; a prior terminal attempt is never reopened.';
