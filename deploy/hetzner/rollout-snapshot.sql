BEGIN READ ONLY;
SET LOCAL statement_timeout = 10000;
SELECT json_build_object(
  'observed_at', state->'observation'->>'observedAt',
  'observed_slot', state->'observation'->'observedSlot',
  'position', json_build_object(
    'collateral', state->'observation'->'positionCollateralRaw',
    'debt', state->'observation'->'positionDebtRaw',
    'status', state->'observation'->'routeStatus'),
  'targets', (SELECT COALESCE(jsonb_agg(jsonb_build_object('lane', lane, 'level', level) ORDER BY lane), '[]')
    FROM loyal_yield.backyard_lane_levels t WHERE route_key = :'route'),
  'exits', (SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY lane), '[]')
    FROM loyal_yield.backyard_lane_exits t WHERE route_key = :'route'),
  'permissions', (SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY action), '[]')
    FROM loyal_yield.vault_release_controls t WHERE vault = :'vault'),
  'recovery', (SELECT count(*) FROM loyal_yield.backyard_manual_recovery_latches
    WHERE route_key = :'route' AND cleared_at IS NULL),
  'failed_or_held', (SELECT count(*) FROM loyal_yield.multiply_operations
    WHERE route_key = :'route' AND updated_at >= to_timestamp(:'since'::double precision)
      AND status IN ('failed', 'held', 'manual_recovery')),
  'inflight', (SELECT count(*) FROM loyal_yield.multiply_operations
    WHERE route_key = :'route'
      AND status IN ('decided', 'built', 'simulated', 'signed', 'broadcast_intent', 'confirmed', 'reconciling'))
) FROM loyal_yield.multiply_route_states WHERE route_key = :'route';
COMMIT;
