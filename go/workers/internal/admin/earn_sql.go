package admin

const earnActiveHoldingsCTE = `
  WITH active_positions AS (
    SELECT
      position.id AS position_id,
      position.wallet_address,
      position.settings,
      position.vault_index,
      position.vault_pubkey,
      position.current_reserve,
      position.current_amount_raw,
      position.principal_amount_raw,
      position.current_observed_at,
      position.deposit_mint,
      vault.id AS vault_id
    FROM loyal_yield.user_yield_positions AS position
    LEFT JOIN loyal_yield.managed_vaults AS vault
      ON vault.settings = position.settings
      AND vault.vault_index = position.vault_index
      AND vault.vault_pubkey = position.vault_pubkey
      AND vault.active = true
    WHERE position.status = 'active'
  ),
  reserve_rows AS (
    SELECT
      active.position_id,
      reserve.amount_raw,
      COALESCE(
        reserve.planning_metadata->>'amountSemantics',
        reserve.planning_metadata->>'amount_semantics'
      ) AS amount_semantics,
      COALESCE(
        reserve.planning_metadata->>'redeemable_liquidity_amount_raw',
        reserve.planning_metadata->>'redeemable_source_liquidity_amount_raw'
      ) AS redeemable_amount_raw_text
    FROM active_positions AS active
    CROSS JOIN LATERAL (
      SELECT
        reserve.amount_raw,
        reserve.planning_metadata,
        reserve.liquidity_mint,
        reserve.vault_id
      FROM loyal_yield.vault_reserve_positions_current AS reserve
      WHERE reserve.vault_id = active.vault_id
        AND reserve.liquidity_mint = active.deposit_mint
      OFFSET 0
    ) AS reserve
  ),
  normalized_reserve_by_position AS (
    SELECT
      position_id,
      COALESCE(SUM(
        CASE
          WHEN amount_semantics IN (
            'kamino_redeemable_liquidity',
            'redeemable_liquidity_amount'
          )
            THEN amount_raw
          WHEN amount_semantics = 'kamino_obligation_collateral_deposited_amount'
            AND redeemable_amount_raw_text ~ '^[0-9]+$'
            THEN redeemable_amount_raw_text::bigint
          ELSE 0::bigint
        END
      ), 0)::bigint AS normalized_reserve_raw,
      COALESCE(SUM(
        CASE
          WHEN amount_semantics = 'kamino_obligation_collateral_deposited_amount'
            THEN amount_raw
          ELSE 0::bigint
        END
      ), 0)::bigint AS collateral_stored_amount_raw,
      COALESCE(SUM(
        CASE
          WHEN amount_semantics = 'kamino_obligation_collateral_deposited_amount'
            AND (
              redeemable_amount_raw_text IS NULL
              OR redeemable_amount_raw_text !~ '^[0-9]+$'
            )
            THEN amount_raw
          WHEN amount_semantics IS NULL
            OR amount_semantics NOT IN (
              'kamino_redeemable_liquidity',
              'redeemable_liquidity_amount',
              'kamino_obligation_collateral_deposited_amount'
            )
            THEN amount_raw
          ELSE 0::bigint
        END
      ), 0)::bigint AS excluded_reserve_raw,
      COUNT(*) FILTER (
        WHERE amount_semantics IN (
          'kamino_redeemable_liquidity',
          'redeemable_liquidity_amount'
        )
      )::bigint AS redeemable_reserve_rows,
      COUNT(*) FILTER (
        WHERE amount_semantics = 'kamino_obligation_collateral_deposited_amount'
      )::bigint AS collateral_reserve_rows,
      COUNT(*) FILTER (
        WHERE amount_semantics = 'kamino_obligation_collateral_deposited_amount'
          AND (
            redeemable_amount_raw_text IS NULL
            OR redeemable_amount_raw_text !~ '^[0-9]+$'
          )
      )::bigint AS missing_redeemable_metadata_rows,
      COUNT(*) FILTER (
        WHERE amount_semantics IS NULL
          OR amount_semantics NOT IN (
            'kamino_redeemable_liquidity',
            'redeemable_liquidity_amount',
            'kamino_obligation_collateral_deposited_amount'
          )
      )::bigint AS unknown_reserve_semantics_rows
    FROM reserve_rows
    GROUP BY position_id
  ),
  idle_by_position AS (
    SELECT
      active.position_id,
      COALESCE(SUM(idle.amount_raw), 0)::bigint AS idle_raw
    FROM active_positions AS active
    INNER JOIN loyal_yield.vault_idle_token_balances_current AS idle
      ON idle.vault_id = active.vault_id
      AND idle.mint = active.deposit_mint
    GROUP BY active.position_id
  ),
  normalized_active_positions AS (
    SELECT
      active.position_id,
      active.wallet_address,
      active.settings,
      active.deposit_mint,
      active.current_reserve,
      active.current_amount_raw,
      active.principal_amount_raw,
      active.current_observed_at,
      COALESCE(reserve.normalized_reserve_raw, 0::bigint)
        AS normalized_reserve_raw,
      COALESCE(idle.idle_raw, 0::bigint) AS idle_raw,
      COALESCE(reserve.normalized_reserve_raw, 0::bigint)
        + COALESCE(idle.idle_raw, 0::bigint) AS normalized_aum_raw,
      COALESCE(reserve.collateral_stored_amount_raw, 0::bigint)
        AS collateral_stored_amount_raw,
      COALESCE(reserve.excluded_reserve_raw, 0::bigint)
        AS excluded_reserve_raw,
      COALESCE(reserve.redeemable_reserve_rows, 0::bigint)
        AS redeemable_reserve_rows,
      COALESCE(reserve.collateral_reserve_rows, 0::bigint)
        AS collateral_reserve_rows,
      COALESCE(reserve.missing_redeemable_metadata_rows, 0::bigint)
        AS missing_redeemable_metadata_rows,
      COALESCE(reserve.unknown_reserve_semantics_rows, 0::bigint)
        AS unknown_reserve_semantics_rows,
      CASE WHEN active.vault_id IS NULL THEN 1::bigint ELSE 0::bigint END
        AS missing_managed_vault_rows,
      COALESCE(reserve.normalized_reserve_raw, 0::bigint)
        + COALESCE(idle.idle_raw, 0::bigint)
        - active.current_amount_raw AS current_pointer_delta_raw
    FROM active_positions AS active
    LEFT JOIN normalized_reserve_by_position AS reserve
      ON reserve.position_id = active.position_id
    LEFT JOIN idle_by_position AS idle
      ON idle.position_id = active.position_id
  )
`

const earnCombinedHoldingsQuery = earnActiveHoldingsCTE + `
    ,
    holdings_by_mint AS (
      SELECT deposit_mint, COUNT(*)::text AS active_position_count,
        COALESCE(SUM(normalized_aum_raw), 0)::text AS active_aum_raw,
        COALESCE(SUM(excluded_reserve_raw), 0)::text AS active_excluded_reserve_raw,
        COALESCE(SUM(normalized_reserve_raw), 0)::text AS active_reserve_raw,
        COALESCE(SUM(idle_raw), 0)::text AS active_idle_raw,
        COALESCE(SUM(principal_amount_raw), 0)::text AS active_principal_raw,
        COALESCE(SUM(current_amount_raw), 0)::text AS active_stored_current_pointer_raw,
        COALESCE(SUM(current_pointer_delta_raw), 0)::text AS current_pointer_delta_raw
      FROM normalized_active_positions GROUP BY deposit_mint
    ),
    latest_rebalance_by_mint AS (
      SELECT liquidity_mint AS deposit_mint, MAX(updated_at) AS latest_rebalance_at
      FROM loyal_yield.rebalance_decisions
      WHERE status = 'confirmed' AND execution_plan->>'kind' = 'same_mint'
        AND liquidity_mint IS NOT NULL
      GROUP BY liquidity_mint
    ),
    mint_summary AS (
      SELECT COALESCE(holdings.deposit_mint, rebalance.deposit_mint) AS deposit_mint,
        COALESCE(holdings.active_position_count, '0') AS active_position_count,
        COALESCE(holdings.active_aum_raw, '0') AS active_aum_raw,
        COALESCE(holdings.active_excluded_reserve_raw, '0') AS active_excluded_reserve_raw,
        COALESCE(holdings.active_reserve_raw, '0') AS active_reserve_raw,
        COALESCE(holdings.active_idle_raw, '0') AS active_idle_raw,
        COALESCE(holdings.active_principal_raw, '0') AS active_principal_raw,
        COALESCE(holdings.active_stored_current_pointer_raw, '0') AS active_stored_current_pointer_raw,
        COALESCE(holdings.current_pointer_delta_raw, '0') AS current_pointer_delta_raw,
        rebalance.latest_rebalance_at
      FROM holdings_by_mint AS holdings
      FULL OUTER JOIN latest_rebalance_by_mint AS rebalance
        ON rebalance.deposit_mint = holdings.deposit_mint
    ),
    headline AS (
      SELECT COALESCE(SUM(normalized_aum_raw), 0)::text AS active_aum_raw,
        COALESCE(SUM(normalized_reserve_raw), 0)::text AS active_reserve_raw,
        COALESCE(SUM(idle_raw), 0)::text AS active_idle_raw,
        COALESCE(SUM(current_amount_raw), 0)::text AS active_stored_current_pointer_raw,
        COALESCE(SUM(current_pointer_delta_raw), 0)::text AS active_current_pointer_delta_raw,
        COALESCE(SUM(principal_amount_raw), 0)::text AS active_principal_raw,
        COALESCE(SUM(collateral_stored_amount_raw), 0)::text AS active_collateral_stored_amount_raw,
        COALESCE(SUM(excluded_reserve_raw), 0)::text AS active_excluded_reserve_raw,
        COALESCE(SUM(redeemable_reserve_rows), 0)::text AS active_redeemable_reserve_rows,
        COALESCE(SUM(collateral_reserve_rows), 0)::text AS active_collateral_reserve_rows,
        COALESCE(SUM(missing_redeemable_metadata_rows), 0)::text AS active_missing_redeemable_metadata_rows,
        COALESCE(SUM(unknown_reserve_semantics_rows), 0)::text AS active_unknown_reserve_semantics_rows,
        COALESCE(SUM(missing_managed_vault_rows), 0)::text AS active_missing_managed_vault_rows,
        MAX(current_observed_at) AS latest_position_observed_at,
        COUNT(DISTINCT COALESCE(NULLIF(wallet_address, ''), settings))::text AS unique_earn_users,
        (SELECT COUNT(DISTINCT policy_account)::text FROM loyal_yield.route_policies WHERE active = true) AS unique_earn_policies,
        (SELECT COUNT(DISTINCT policy.policy_account)::text FROM loyal_yield.balance_sweep_policies AS policy
          INNER JOIN loyal_yield.balance_sweep_targets AS target ON target.balance_sweep_policy_id = policy.id
          WHERE policy.active = true AND target.desired_active = true AND target.chain_status = 'active') AS active_autodeposit_policies
      FROM normalized_active_positions
    ),
    top_positions AS (
      SELECT wallet_address, settings, deposit_mint, current_reserve,
        normalized_aum_raw::text, normalized_reserve_raw::text, idle_raw::text,
        current_amount_raw::text AS stored_current_pointer_raw,
        current_pointer_delta_raw::text, principal_amount_raw::text,
        collateral_stored_amount_raw::text, excluded_reserve_raw::text,
        redeemable_reserve_rows::text, collateral_reserve_rows::text,
        missing_redeemable_metadata_rows::text, unknown_reserve_semantics_rows::text,
        missing_managed_vault_rows::text, current_observed_at
      FROM normalized_active_positions
      ORDER BY normalized_aum_raw DESC, current_observed_at DESC LIMIT 25
    )
    SELECT (SELECT row_to_json(headline) FROM headline) AS headline,
      COALESCE((SELECT json_agg(mint_summary ORDER BY deposit_mint) FROM mint_summary), '[]'::json) AS mint_summaries,
      COALESCE((SELECT json_agg(top_positions ORDER BY normalized_aum_raw DESC, current_observed_at DESC) FROM top_positions), '[]'::json) AS top_positions
  `

const earnSmallMetricsTemplate = `
    WITH bounds AS (
      SELECT
        ((date_trunc('day', now() AT TIME ZONE 'UTC')::date + 1) - 30)::date AS start_day,
        (date_trunc('day', now() AT TIME ZONE 'UTC')::date + 1)::date AS end_day
    ),
    stable_mints(liquidity_mint) AS (
      VALUES __MINTS__
    ),
    days AS (
      SELECT generate_series(
        (SELECT start_day FROM bounds),
        (SELECT end_day FROM bounds) - 1,
        interval '1 day'
      )::date AS day
    ),
    confirmed_flow_events AS (
      SELECT
        (deposit.confirmed_at AT TIME ZONE 'UTC')::date AS day,
        'deposit'::text AS direction,
        'earn_deposit'::text AS source,
        deposit.id AS source_id,
        deposit.deposit_mint AS liquidity_mint,
        deposit.principal_amount_raw AS amount_raw
      FROM loyal_yield.user_yield_position_deposits AS deposit
      WHERE deposit.confirmed_at >= (SELECT start_day FROM bounds)
        AND deposit.confirmed_at < (SELECT end_day FROM bounds)
      UNION ALL
      SELECT
        (withdrawal.confirmed_at AT TIME ZONE 'UTC')::date AS day,
        'withdrawal'::text AS direction,
        'earn_withdrawal'::text AS source,
        withdrawal.id AS source_id,
        withdrawal.liquidity_mint,
        withdrawal.withdrawn_amount_raw AS amount_raw
      FROM loyal_yield.user_yield_position_withdrawals AS withdrawal
      WHERE withdrawal.confirmed_at >= (SELECT start_day FROM bounds)
        AND withdrawal.confirmed_at < (SELECT end_day FROM bounds)
    ),
    flow_by_day AS (
      SELECT
        day,
        liquidity_mint,
        SUM(amount_raw) FILTER (WHERE direction = 'deposit') AS deposited_raw,
        SUM(amount_raw) FILTER (WHERE direction = 'withdrawal') AS withdrawn_raw
      FROM confirmed_flow_events
      GROUP BY day, liquidity_mint
    ),
    flow_rows AS (
      SELECT
        to_char(days.day, 'YYYY-MM-DD') AS day,
        stable_mints.liquidity_mint,
        COALESCE(flow_by_day.deposited_raw, 0)::text AS deposited_raw,
        COALESCE(flow_by_day.withdrawn_raw, 0)::text AS withdrawn_raw
      FROM days
      CROSS JOIN stable_mints
      LEFT JOIN flow_by_day
        ON flow_by_day.day = days.day
        AND flow_by_day.liquidity_mint = stable_mints.liquidity_mint
    ),
    flow_json AS (
      SELECT COALESCE(
        json_agg(flow_rows ORDER BY flow_rows.day, flow_rows.liquidity_mint),
        '[]'::json
      ) AS value
      FROM flow_rows
    ),
    target_observations AS MATERIALIZED (
      SELECT
        target.last_seen_at,
        CASE
          WHEN policy.active = true
            AND target.desired_active = true
            AND target.chain_status = 'active'
            THEN 'active'
          WHEN policy.active = true
            AND target.desired_active = false
            AND target.chain_status = 'active'
            THEN 'paused'
          WHEN target.chain_status = 'pending'
            THEN 'pending'
          WHEN target.chain_status = 'inconsistent'
            THEN 'inconsistent'
          ELSE 'closed'
        END AS status
      FROM loyal_yield.balance_sweep_targets AS target
      LEFT JOIN loyal_yield.balance_sweep_policies AS policy
        ON policy.id = target.balance_sweep_policy_id
    ),
    status_json AS (
      SELECT COALESCE(
        json_agg(status_rows ORDER BY status_rows.status),
        '[]'::json
      ) AS value
      FROM (
        SELECT status, COUNT(*)::text AS total
        FROM target_observations
        GROUP BY status
      ) AS status_rows
    ),
    scheduled AS (
      SELECT
        (COUNT(*) FILTER (
          WHERE lot.status = 'open' AND lot.remaining_amount_raw > 0
        ))::text AS open_lot_count,
        COALESCE(SUM(lot.remaining_amount_raw) FILTER (
          WHERE lot.status = 'open' AND lot.remaining_amount_raw > 0
        ), 0)::text AS open_amount_raw,
        (COUNT(*) FILTER (
          WHERE lot.status = 'open'
            AND lot.remaining_amount_raw > 0
            AND lot.eligible_after <= now()
        ))::text AS eligible_lot_count,
        COALESCE(SUM(lot.remaining_amount_raw) FILTER (
          WHERE lot.status = 'open'
            AND lot.remaining_amount_raw > 0
            AND lot.eligible_after <= now()
        ), 0)::text AS eligible_amount_raw
      FROM loyal_yield.balance_sweep_surplus_lots AS lot
      INNER JOIN loyal_yield.balance_sweep_targets AS target
        ON target.id = lot.target_id
      LEFT JOIN loyal_yield.balance_sweep_policies AS policy
        ON policy.id = target.balance_sweep_policy_id
      WHERE policy.active = true
        AND target.desired_active = true
        AND target.chain_status = 'active'
    ),
    executions AS (
      SELECT
        COUNT(*)::text AS count,
        COALESCE(SUM(amount_raw), 0)::text AS amount_raw,
        (COUNT(*) FILTER (
          WHERE received_at >= now() - interval '30 days'
        ))::text AS count_30d,
        COALESCE(SUM(amount_raw) FILTER (
          WHERE received_at >= now() - interval '30 days'
        ), 0)::text AS amount_30d_raw,
        MAX(received_at) AS latest_received_at
      FROM loyal_yield.balance_sweep_executions
    ),
    wallet_balance_event_freshness AS (
      SELECT
        (
          SELECT observed_at
          FROM loyal_yield.balance_sweep_wallet_balance_events
          ORDER BY observed_at DESC
          LIMIT 1
        ) AS latest_wallet_balance_event_observed_at,
        (
          SELECT projected_at
          FROM loyal_yield.balance_sweep_wallet_balance_events
          ORDER BY projected_at DESC
          LIMIT 1
        ) AS latest_wallet_balance_projected_at
    ),
    freshness AS (
      SELECT
        NULL::timestamptz AS latest_position_observed_at,
        (SELECT MAX(last_seen_at)
         FROM target_observations
         WHERE status = 'active') AS latest_target_seen_at,
        (SELECT latest_received_at FROM executions)
          AS latest_sweep_execution_received_at,
        (SELECT MAX(observed_at)
         FROM loyal_yield.balance_sweep_wallet_balances_current)
          AS latest_wallet_balance_observed_at,
        wallet_balance_event_freshness.latest_wallet_balance_event_observed_at,
        wallet_balance_event_freshness.latest_wallet_balance_projected_at
      FROM wallet_balance_event_freshness
    )
    SELECT
      (SELECT value FROM flow_json) AS flow,
      (SELECT value FROM status_json) AS status,
      (SELECT row_to_json(scheduled) FROM scheduled) AS scheduled,
      (SELECT row_to_json(executions) FROM executions) AS executions,
      (SELECT row_to_json(freshness) FROM freshness) AS freshness
  `
