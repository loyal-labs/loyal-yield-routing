-- Backyard runs a list of Voltr vaults. A vault's route row is keyed by the
-- Squads vault that is its book, 'backyard:<vault>'; rwa keeps the key it
-- has always had. 0071 pinned the backyard engine to that one key: this
-- admits either form, with the key bound to the row's own vault.

ALTER TABLE loyal_yield.multiply_route_states
    DROP CONSTRAINT IF EXISTS multiply_route_states_schema_v8_v9_or_backyard_v1;

ALTER TABLE loyal_yield.multiply_route_states
    ADD CONSTRAINT multiply_route_states_schema_v8_v9_or_backyard_v1 CHECK (
        (
            (state ->> 'schemaVersion')::INTEGER = 8
            AND state ->> 'engineVersion' = 'earn_max_v1'
        )
        OR
        (
            (state ->> 'schemaVersion')::INTEGER = 9
            AND state ->> 'engineVersion' = 'earn_max_v2'
        )
        OR
        (
            (state ->> 'schemaVersion')::INTEGER = 10
            AND state ->> 'engineVersion' = 'backyard_rwa_v1'
            AND state ->> 'routeKind' = 'backyard_rwa_v1'
            AND (route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh' OR route_key = 'backyard:' || vault)
        )
    ) NOT VALID;

ALTER TABLE loyal_yield.multiply_route_states
    VALIDATE CONSTRAINT multiply_route_states_schema_v8_v9_or_backyard_v1;

ALTER TABLE loyal_yield.multiply_route_states
    DROP CONSTRAINT IF EXISTS multiply_route_states_backyard_kind;

ALTER TABLE loyal_yield.multiply_route_states
    ADD CONSTRAINT multiply_route_states_backyard_kind CHECK (
        (state ->> 'engineVersion') IS DISTINCT FROM 'backyard_rwa_v1'
        OR (
            state ->> 'routeKind' = 'backyard_rwa_v1'
            AND (route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh' OR route_key = 'backyard:' || vault)
        )
    ) NOT VALID;

ALTER TABLE loyal_yield.multiply_route_states
    VALIDATE CONSTRAINT multiply_route_states_backyard_kind;

-- The looping vault's route row: Voltr vault GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1,
-- booked by Squads Settings 5YQ78… vault index 2.
INSERT INTO loyal_yield.multiply_route_states (route_key, settings, vault_index, vault, state, state_version)
VALUES (
    'backyard:6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY',
    '5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6',
    2,
    '6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY',
    jsonb_build_object(
        'schemaVersion', 10,
        'engineVersion', 'backyard_rwa_v1',
        'routeKind', 'backyard_rwa_v1',
        'routeKey', 'backyard:6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY',
        'settings', '5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6',
        'vaultIndex', 2,
        'vault', '6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY',
        'generation', 1
    ),
    1
)
ON CONFLICT (route_key) DO NOTHING;
