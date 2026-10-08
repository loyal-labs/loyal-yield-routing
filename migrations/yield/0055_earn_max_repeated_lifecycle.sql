UPDATE loyal_yield.multiply_route_states route
SET
    state_version = route.state_version + 1,
    state = jsonb_set(
        jsonb_set(
            route.state,
            '{generation}',
            to_jsonb(route.state_version + 1)
        ),
        '{frontend}',
        jsonb_build_object(
            'generation', route.state_version + 1,
            'status', CASE route.state ->> 'goal'
                WHEN 'idle' THEN 'idle'
                WHEN 'deploy' THEN 'deploying'
                WHEN 'move' THEN 'moving'
                WHEN 'withdraw' THEN 'withdrawing'
                WHEN 'claimed' THEN 'claimed'
                WHEN 'manual_recovery' THEN 'manual_recovery'
            END,
            'strategyKey', CASE
                WHEN route.state #>> '{position,kind}' = 'active'
                THEN route.state #> '{position,strategyKey}'
                ELSE 'null'::jsonb
            END,
            'claimAmountRaw', CASE
                WHEN route.state #>> '{position,kind}' = 'idle'
                THEN (route.state #>> '{position,claim,amountRaw}')::BIGINT
                ELSE 0
            END,
            'collateralAmountRaw', CASE
                WHEN route.state #>> '{position,kind}' = 'active'
                THEN (route.state #>> '{position,collateral,amountRaw}')::BIGINT
                ELSE 0
            END,
            'debtAmountRaw', CASE
                WHEN route.state #>> '{position,kind}' = 'active'
                THEN (route.state #>> '{position,debt,amountRaw}')::BIGINT
                ELSE 0
            END,
            'withdrawalStatus', COALESCE(
                route.state #> '{withdrawal,status}',
                'null'::jsonb
            ),
            'observedSlot', (route.state ->> 'observedSlot')::BIGINT
        )
    ),
    updated_at = now()
WHERE route.state -> 'frontend' IS DISTINCT FROM jsonb_build_object(
    'generation', route.state_version,
    'status', CASE route.state ->> 'goal'
        WHEN 'idle' THEN 'idle'
        WHEN 'deploy' THEN 'deploying'
        WHEN 'move' THEN 'moving'
        WHEN 'withdraw' THEN 'withdrawing'
        WHEN 'claimed' THEN 'claimed'
        WHEN 'manual_recovery' THEN 'manual_recovery'
    END,
    'strategyKey', CASE
        WHEN route.state #>> '{position,kind}' = 'active'
        THEN route.state #> '{position,strategyKey}'
        ELSE 'null'::jsonb
    END,
    'claimAmountRaw', CASE
        WHEN route.state #>> '{position,kind}' = 'idle'
        THEN (route.state #>> '{position,claim,amountRaw}')::BIGINT
        ELSE 0
    END,
    'collateralAmountRaw', CASE
        WHEN route.state #>> '{position,kind}' = 'active'
        THEN (route.state #>> '{position,collateral,amountRaw}')::BIGINT
        ELSE 0
    END,
    'debtAmountRaw', CASE
        WHEN route.state #>> '{position,kind}' = 'active'
        THEN (route.state #>> '{position,debt,amountRaw}')::BIGINT
        ELSE 0
    END,
    'withdrawalStatus', COALESCE(
        route.state #> '{withdrawal,status}',
        'null'::jsonb
    ),
    'observedSlot', (route.state ->> 'observedSlot')::BIGINT
);

