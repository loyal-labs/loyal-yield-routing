-- Read-only guardrail for the June 16 same-mint amount-semantics incident.
-- PASS condition: this query returns zero rows for planned/submitted/confirmed
-- same-mint decisions after the fix.
WITH same_mint_decisions AS (
    SELECT
        d.id,
        d.vault_id,
        d.status::text AS status,
        d.source_snapshot_id,
        d.source_reserve,
        d.target_reserve,
        d.liquidity_mint,
        d.amount_raw,
        d.execution_plan,
        source_current.amount_raw AS source_current_amount_raw,
        source_current.planning_metadata AS source_planning_metadata,
        CASE
            WHEN d.execution_plan->>'route_amount_semantics' IS NULL THEN 'missing_route_amount_semantics'
            WHEN d.execution_plan->>'route_amount_semantics' != 'redeemable_liquidity_amount' THEN 'unsupported_route_amount_semantics'
            WHEN d.execution_plan->>'source_amount_semantics' = 'kamino_obligation_collateral_deposited_amount'
                 AND d.execution_plan->>'redeemable_source_liquidity_amount_raw' IS NULL THEN 'collateral_source_without_redeemable_liquidity'
            WHEN source_current.planning_metadata->>'amount_semantics' = 'kamino_obligation_collateral_deposited_amount'
                 AND d.amount_raw = source_current.amount_raw THEN 'route_amount_matches_collateral_current_position'
            ELSE NULL
        END AS unsafe_reason
    FROM loyal_yield.rebalance_decisions d
    LEFT JOIN loyal_yield.vault_reserve_positions_current source_current
      ON source_current.vault_id = d.vault_id
     AND source_current.reserve = d.source_reserve
    WHERE d.status::text IN ('planned', 'simulating', 'ready', 'submitted', 'confirming', 'confirmed')
      AND (
          d.execution_plan->>'kind' = 'same_mint'
          OR (
              d.source_liquidity_mint IS NOT NULL
              AND d.source_liquidity_mint = d.target_liquidity_mint
              AND d.liquidity_mint IS NOT NULL
          )
      )
)
SELECT
    id,
    vault_id,
    status,
    source_snapshot_id,
    source_reserve,
    target_reserve,
    liquidity_mint,
    amount_raw,
    unsafe_reason,
    execution_plan->>'route_amount_semantics' AS route_amount_semantics,
    execution_plan->>'source_amount_semantics' AS source_amount_semantics,
    execution_plan->>'redeemable_source_liquidity_amount_raw' AS redeemable_source_liquidity_amount_raw,
    source_current_amount_raw,
    source_planning_metadata->>'amount_semantics' AS source_current_amount_semantics
FROM same_mint_decisions
WHERE unsafe_reason IS NOT NULL
ORDER BY id;
