UPDATE loyal_yield.rebalance_decisions
SET execution_plan = jsonb_set(
    jsonb_set(
      jsonb_set(
        jsonb_set(
          execution_plan,
          '{observed_slot}',
          COALESCE(execution_plan->'observed_slot', execution_plan->'idle_observed_slot', 'null'::jsonb),
          true
        ),
        '{observed_at}',
        COALESCE(execution_plan->'observed_at', execution_plan->'idle_observed_at', 'null'::jsonb),
        true
      ),
      '{target_supply_apy_bps}',
      COALESCE(execution_plan->'target_supply_apy_bps', execution_plan->'target_apy_bps', 'null'::jsonb),
      true
    ),
    '{edge_bps}',
    COALESCE(execution_plan->'edge_bps', execution_plan->'estimated_edge_bps', 'null'::jsonb),
    true
  )
WHERE execution_plan->>'kind' = 'idle_vault_deposit'
  AND (
    NOT execution_plan ? 'observed_slot'
    OR NOT execution_plan ? 'observed_at'
    OR NOT execution_plan ? 'target_supply_apy_bps'
    OR NOT execution_plan ? 'edge_bps'
  );
