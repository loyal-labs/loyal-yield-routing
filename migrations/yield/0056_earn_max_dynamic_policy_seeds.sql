ALTER TABLE loyal_yield.earn_max_policy_sets
    ADD COLUMN IF NOT EXISTS policy_seed_base BIGINT;

UPDATE loyal_yield.earn_max_policy_sets policy
SET policy_seed_base = (
    SELECT MIN((entry ->> 'seed')::BIGINT)
    FROM jsonb_array_elements(policy.policy_accounts) entry
    WHERE entry ? 'seed'
)
WHERE policy.policy_seed_base IS NULL;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM loyal_yield.earn_max_policy_sets
        WHERE policy_seed_base IS NULL OR policy_seed_base <= 0
    ) THEN
        RAISE EXCEPTION 'Earn MAX policy projection has no positive policy seed base';
    END IF;
END
$$;

ALTER TABLE loyal_yield.earn_max_policy_sets
    ALTER COLUMN policy_seed_base SET NOT NULL;

ALTER TABLE loyal_yield.earn_max_policy_sets
    DROP CONSTRAINT IF EXISTS earn_max_policy_seed_base_positive;

ALTER TABLE loyal_yield.earn_max_policy_sets
    ADD CONSTRAINT earn_max_policy_seed_base_positive
    CHECK (policy_seed_base > 0);

ALTER TABLE loyal_yield.multiply_route_states
    DROP CONSTRAINT IF EXISTS multiply_route_states_schema_v5;

ALTER TABLE loyal_yield.multiply_route_states
    DROP CONSTRAINT IF EXISTS multiply_route_states_schema_v6;

ALTER TABLE loyal_yield.multiply_route_states
    DROP CONSTRAINT IF EXISTS multiply_route_states_schema_v5_or_v6;

UPDATE loyal_yield.multiply_route_states route
SET state = jsonb_set(
        jsonb_set(route.state, '{schemaVersion}', '6'::jsonb),
        '{policySeedBase}',
        to_jsonb(policy.policy_seed_base)
    ),
    updated_at = now()
FROM loyal_yield.earn_max_policy_sets policy
WHERE route.settings = policy.settings
  AND route.vault_index = policy.vault_index
  AND route.state ->> 'engineVersion' = 'earn_max_v1'
  AND (
      route.state ->> 'schemaVersion' IS DISTINCT FROM '6'
      OR route.state ->> 'policySeedBase' IS DISTINCT FROM policy.policy_seed_base::TEXT
  );

ALTER TABLE loyal_yield.multiply_route_states
    ADD CONSTRAINT multiply_route_states_schema_v5_or_v6
    CHECK ((state ->> 'schemaVersion')::INTEGER IN (5, 6));
