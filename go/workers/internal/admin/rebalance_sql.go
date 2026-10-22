package admin

const rebalanceSQLAuditRowsCTE = `
  WITH audit_rows AS (
    SELECT
      'decision'::text AS record_type,
      ('decision:' || decision.id::text) AS id,
      decision.id::bigint AS sort_id,
      1::integer AS sort_source,
      decision.created_at AS event_at,
      decision.created_at,
      decision.updated_at,
      CASE
        WHEN decision.execution_plan->>'kind' IN (
          'same_mint',
          'cross_mint_jupiter'
        ) THEN 'rebalance'
        WHEN decision.execution_plan->>'kind' = 'idle_vault_deposit' THEN 'deposit'
        ELSE 'needs_review'
      END::text AS lane,
      CASE
        WHEN decision.execution_plan->>'kind' IN (
          'same_mint',
          'cross_mint_jupiter'
        ) THEN 'rebalance'
        WHEN decision.execution_plan->>'kind' = 'idle_vault_deposit' THEN 'idle_vault_deposit'
        ELSE 'needs_review'
      END::text AS movement_source,
      decision.execution_plan->>'kind' AS execution_kind,
      decision.liquidity_mint,
      decision.source_liquidity_mint,
      decision.target_liquidity_mint,
      decision.status::text AS status,
      decision.abandon_reason,
      decision.amount_raw,
      decision.confirmed_slot,
      decision.submitted_slot,
      decision.decision_reason::text,
      decision.estimated_edge_bps,
      decision.source_apy_bps,
      decision.source_reserve,
      decision.target_apy_bps,
      decision.target_reserve,
      decision.signature,
      NULL::text AS secondary_signature,
      decision.vault_id::text AS vault_id,
      vault.vault_index::text AS vault_index,
      vault.vault_pubkey,
      (
        decision.execution_plan->>'kind' = 'idle_vault_deposit'
        AND NOT EXISTS (
          SELECT 1
          FROM loyal_yield.user_yield_position_deposits AS linked_deposit
          WHERE linked_deposit.deposit_signature = decision.signature
        )
      ) AS is_idle_fallback
    FROM loyal_yield.rebalance_decisions AS decision
    LEFT JOIN loyal_yield.managed_vaults AS vault
      ON vault.id = decision.vault_id

    UNION ALL

    SELECT
      'yield_deposit'::text AS record_type,
      ('deposit:' || deposit.id::text) AS id,
      deposit.id::bigint AS sort_id,
      2::integer AS sort_source,
      deposit.confirmed_at AS event_at,
      deposit.created_at,
      deposit.confirmed_at AS updated_at,
      'deposit'::text AS lane,
      CASE
        WHEN execution.id IS NOT NULL THEN 'autodeposit'
        WHEN idle_decision.id IS NOT NULL THEN 'idle_vault_deposit'
        ELSE 'manual_deposit'
      END::text AS movement_source,
      CASE
        WHEN execution.id IS NOT NULL THEN 'autodeposit'
        WHEN idle_decision.id IS NOT NULL THEN 'idle_vault_deposit'
        ELSE 'manual_deposit'
      END::text AS execution_kind,
      deposit.liquidity_mint,
      NULL::text AS source_liquidity_mint,
      NULL::text AS target_liquidity_mint,
      'confirmed'::text AS status,
      NULL::text AS abandon_reason,
      deposit.principal_amount_raw AS amount_raw,
      deposit.confirmed_slot,
      NULL::bigint AS submitted_slot,
      CASE
        WHEN execution.id IS NOT NULL THEN 'autodeposit_completed'
        WHEN idle_decision.id IS NOT NULL THEN 'idle_vault_deposit_completed'
        ELSE 'user_deposit_confirmed'
      END::text AS decision_reason,
      NULL::bigint AS estimated_edge_bps,
      NULL::bigint AS source_apy_bps,
      NULL::text AS source_reserve,
      deposit.target_supply_apy_bps AS target_apy_bps,
      deposit.target_reserve,
      COALESCE(execution.kamino_deposit_signature, deposit.deposit_signature) AS signature,
      execution.signature AS secondary_signature,
      vault.id::text AS vault_id,
      deposit.vault_index::text AS vault_index,
      deposit.vault_pubkey,
      false AS is_idle_fallback
    FROM loyal_yield.user_yield_position_deposits AS deposit
    LEFT JOIN loyal_yield.balance_sweep_executions AS execution
      ON execution.id = deposit.balance_sweep_execution_id
    LEFT JOIN LATERAL (
      SELECT idle.id
      FROM loyal_yield.rebalance_decisions AS idle
      WHERE idle.signature = deposit.deposit_signature
        AND idle.execution_plan->>'kind' = 'idle_vault_deposit'
      ORDER BY idle.id DESC
      LIMIT 1
    ) AS idle_decision ON true
    LEFT JOIN loyal_yield.managed_vaults AS vault
      ON vault.settings = deposit.settings
      AND vault.vault_index = deposit.vault_index
      AND vault.vault_pubkey = deposit.vault_pubkey

    UNION ALL

    SELECT
      'execution'::text AS record_type,
      ('execution:' || execution.id::text) AS id,
      execution.id::bigint AS sort_id,
      3::integer AS sort_source,
      COALESCE(execution.completed_at, execution.received_at, execution.inserted_at) AS event_at,
      execution.inserted_at AS created_at,
      COALESCE(execution.completed_at, execution.received_at, execution.inserted_at) AS updated_at,
      'deposit'::text AS lane,
      'autodeposit'::text AS movement_source,
      'autodeposit'::text AS execution_kind,
      execution.token_mint AS liquidity_mint,
      NULL::text AS source_liquidity_mint,
      NULL::text AS target_liquidity_mint,
      'failed'::text AS status,
      execution.completion_failure_code AS abandon_reason,
      execution.amount_raw,
      execution.slot AS confirmed_slot,
      NULL::bigint AS submitted_slot,
      NULL::text AS decision_reason,
      NULL::bigint AS estimated_edge_bps,
      NULL::bigint AS source_apy_bps,
      NULL::text AS source_reserve,
      NULL::bigint AS target_apy_bps,
      NULL::text AS target_reserve,
      COALESCE(execution.kamino_deposit_signature, execution.signature) AS signature,
      CASE
        WHEN execution.kamino_deposit_signature IS NOT NULL THEN execution.signature
        ELSE NULL::text
      END AS secondary_signature,
      vault.id::text AS vault_id,
      target.vault_index::text AS vault_index,
      target.vault_pubkey,
      false AS is_idle_fallback
    FROM loyal_yield.balance_sweep_executions AS execution
    INNER JOIN loyal_yield.balance_sweep_targets AS target
      ON target.id = execution.target_id
    LEFT JOIN loyal_yield.managed_vaults AS vault
      ON vault.settings = target.settings
      AND vault.vault_index = target.vault_index
      AND vault.vault_pubkey = target.vault_pubkey
    WHERE execution.completion_failure_code IS NOT NULL
  )
`

const rebalanceSQLActiveReserveRoutes = `
      WITH reserve_rows AS (
        SELECT
          reserve.reserve AS current_reserve,
          reserve.liquidity_mint,
          reserve.observed_at,
          vault.id AS vault_id,
          reserve.amount_raw,
          COALESCE(
            reserve.planning_metadata->>'amountSemantics',
            reserve.planning_metadata->>'amount_semantics'
          ) AS amount_semantics,
          COALESCE(
            reserve.planning_metadata->>'redeemable_liquidity_amount_raw',
            reserve.planning_metadata->>'redeemable_source_liquidity_amount_raw'
          ) AS redeemable_amount_raw_text
        FROM loyal_yield.managed_vaults AS vault
        INNER JOIN loyal_yield.vault_reserve_positions_current AS reserve
          ON reserve.vault_id = vault.id
        WHERE vault.active = true
          AND vault.vault_index = 1
          AND reserve.has_value = true
          AND reserve.amount_raw > 0
      ),
      normalized_reserve_rows AS (
        SELECT
          current_reserve,
          liquidity_mint,
          observed_at,
          vault_id,
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
          END AS normalized_amount_raw
        FROM reserve_rows
      )
      SELECT
        current_reserve,
        liquidity_mint,
        COUNT(DISTINCT vault_id)::text AS position_count,
        COALESCE(SUM(normalized_amount_raw), 0)::text AS active_aum_raw,
        MAX(observed_at) AS latest_observed_at
      FROM normalized_reserve_rows
      WHERE normalized_amount_raw > 0
      GROUP BY current_reserve, liquidity_mint
      ORDER BY COALESCE(SUM(normalized_amount_raw), 0) DESC
`

const rebalanceSQLRecentDecisions = `
      WITH ranked_decisions AS MATERIALIZED (
        SELECT
          decision.*,
          ROW_NUMBER() OVER (
            PARTITION BY decision.execution_plan->>'kind'
            ORDER BY decision.created_at DESC, decision.id DESC
          ) AS route_mode_rank
        FROM loyal_yield.rebalance_decisions AS decision
        WHERE decision.execution_plan->>'kind' IN (
          'same_mint',
          'cross_mint_jupiter'
        )
      )
      SELECT
        id::text,
        status::text,
        source_reserve,
        target_reserve,
        liquidity_mint,
        source_liquidity_mint,
        target_liquidity_mint,
        amount_raw::text,
        source_apy_bps::text,
        target_apy_bps::text,
        estimated_edge_bps::text,
        decision_reason::text,
        abandon_reason,
        signature,
        confirmed_slot::text,
        created_at,
        updated_at,
        execution_plan->>'kind' AS execution_kind,
        execution_plan->>'kind' AS route_mode
      FROM ranked_decisions
      WHERE route_mode_rank <= 25
      ORDER BY created_at DESC, id DESC
`

const rebalanceSQLActivity = `
      WITH bounds AS (
        SELECT
          now() - interval '72 hours' AS window_started_at,
          date_bin(
            interval '2 hours',
            now() - interval '72 hours',
            timestamptz '1970-01-01 00:00:00+00'
          ) AS started_at,
          date_bin(
            interval '2 hours',
            now(),
            timestamptz '1970-01-01 00:00:00+00'
          ) AS ended_at
      ),
      route_modes AS (
        SELECT *
        FROM (VALUES ('same_mint'), ('cross_mint_jupiter')) AS modes(route_mode)
      ),
      buckets AS (
        SELECT
          route_mode.route_mode,
          generate_series(
            (SELECT started_at FROM bounds),
            (SELECT ended_at FROM bounds),
            interval '2 hours'
          ) AS bucket_started_at
        FROM route_modes AS route_mode
      ),
      decision_activity AS (
        SELECT
          decision.execution_plan->>'kind' AS route_mode,
          date_bin(
            interval '2 hours',
            decision.updated_at,
            timestamptz '1970-01-01 00:00:00+00'
          ) AS bucket_started_at,
          COUNT(*) FILTER (
            WHERE decision.status = 'confirmed'
          )::bigint AS confirmed,
          COUNT(*) FILTER (
            WHERE decision.status IN ('confirmed', 'failed')
          )::bigint AS terminal_attempts,
          COUNT(*) FILTER (
            WHERE decision.status = 'failed'
          )::bigint AS failed_decisions
        FROM loyal_yield.rebalance_decisions AS decision
        WHERE decision.execution_plan->>'kind' IN (
            'same_mint',
            'cross_mint_jupiter'
          )
          AND decision.updated_at >= (SELECT window_started_at FROM bounds)
        GROUP BY 1, 2
      ),
      recent_failed_opportunities AS MATERIALIZED (
        SELECT
          opportunity.execution_plan->>'kind' AS route_mode,
          opportunity.state_entered_at
        FROM loyal_yield.rebalance_opportunities AS opportunity
        WHERE opportunity.execution_plan->>'kind' IN (
            'same_mint',
            'cross_mint_jupiter'
          )
          AND opportunity.opportunity_state = 'failed'
          AND opportunity.state_entered_at >= (
            SELECT window_started_at FROM bounds
          )
      ),
      opportunity_activity AS (
        SELECT
          opportunity.route_mode,
          date_bin(
            interval '2 hours',
            opportunity.state_entered_at,
            timestamptz '1970-01-01 00:00:00+00'
          ) AS bucket_started_at,
          COUNT(*)::bigint AS failed_opportunities
        FROM recent_failed_opportunities AS opportunity
        GROUP BY 1, 2
      ),
      recent_claim_opportunities AS MATERIALIZED (
        SELECT
          opportunity.execution_plan->>'kind' AS route_mode,
          opportunity.created_at,
          opportunity.attempt_count
        FROM loyal_yield.rebalance_opportunities AS opportunity
        WHERE opportunity.execution_plan->>'kind' IN (
            'same_mint',
            'cross_mint_jupiter'
          )
          AND opportunity.created_at >= (SELECT window_started_at FROM bounds)
      ),
      recent_submissions AS MATERIALIZED (
        SELECT
          decision.execution_plan->>'kind' AS route_mode,
          submission.submission_state,
          submission.submission_state_entered_at,
          submission.movement_leg,
          submission.finalized_at,
          submission.compiled_fee_lamports
        FROM loyal_yield.signed_route_submissions AS submission
        INNER JOIN loyal_yield.rebalance_decisions AS decision
          ON decision.id = submission.decision_id
        WHERE decision.execution_plan->>'kind' IN (
            'same_mint',
            'cross_mint_jupiter'
          )
          AND (
            submission.submission_state_entered_at >= (
              SELECT window_started_at FROM bounds
            )
            OR submission.finalized_at >= (SELECT window_started_at FROM bounds)
          )
          AND (
            submission.submission_state = 'expired'
            OR (
              submission.movement_leg = 'swap'
              AND submission.finalized_at IS NOT NULL
              AND decision.execution_plan->>'kind' = 'cross_mint_jupiter'
            )
          )
      ),
      submission_activity AS (
        SELECT
          submission.route_mode,
          date_bin(
            interval '2 hours',
            submission.submission_state_entered_at,
            timestamptz '1970-01-01 00:00:00+00'
          ) AS bucket_started_at,
          COUNT(*)::bigint AS expired_submissions
        FROM recent_submissions AS submission
        WHERE submission.submission_state = 'expired'
          AND submission.submission_state_entered_at >= (
            SELECT window_started_at FROM bounds
          )
        GROUP BY 1, 2
      ),
      opportunity_claims AS (
        SELECT
          opportunity.route_mode,
          date_bin(
            interval '2 hours',
            opportunity.created_at,
            timestamptz '1970-01-01 00:00:00+00'
          ) AS bucket_started_at,
          COALESCE(SUM(opportunity.attempt_count), 0)::bigint AS fleet_claims
        FROM recent_claim_opportunities AS opportunity
        WHERE opportunity.created_at >= (SELECT window_started_at FROM bounds)
        GROUP BY 1, 2
      ),
      swap_fee_activity AS (
        SELECT
          'cross_mint_jupiter'::text AS route_mode,
          date_bin(
            interval '2 hours',
            submission.finalized_at,
            timestamptz '1970-01-01 00:00:00+00'
          ) AS bucket_started_at,
          COUNT(*)::bigint AS finalized_swap_legs,
          COALESCE(SUM(submission.compiled_fee_lamports), 0)::bigint
            AS swap_fee_lamports,
          COALESCE(MAX(submission.compiled_fee_lamports), 0)::bigint
            AS max_swap_fee_lamports
        FROM recent_submissions AS submission
        WHERE submission.route_mode = 'cross_mint_jupiter'
          AND submission.movement_leg = 'swap'
          AND submission.finalized_at IS NOT NULL
          AND submission.finalized_at >= (
            SELECT window_started_at FROM bounds
          )
        GROUP BY 1, 2
      )
      SELECT
        bucket.route_mode,
        bucket.bucket_started_at,
        COALESCE(decision.confirmed, 0)::text AS confirmed,
        COALESCE(decision.terminal_attempts, 0)::text AS terminal_attempts,
        COALESCE(decision.failed_decisions, 0)::text AS failed_decisions,
        COALESCE(opportunity.failed_opportunities, 0)::text
          AS failed_opportunities,
        COALESCE(submission.expired_submissions, 0)::text
          AS expired_submissions,
        COALESCE(claim.fleet_claims, 0)::text AS fleet_claims,
        COALESCE(swap_fee.finalized_swap_legs, 0)::text
          AS finalized_swap_legs,
        COALESCE(swap_fee.swap_fee_lamports, 0)::text
          AS swap_fee_lamports,
        COALESCE(swap_fee.max_swap_fee_lamports, 0)::text
          AS max_swap_fee_lamports
      FROM buckets AS bucket
      LEFT JOIN decision_activity AS decision
        USING (route_mode, bucket_started_at)
      LEFT JOIN opportunity_activity AS opportunity
        USING (route_mode, bucket_started_at)
      LEFT JOIN submission_activity AS submission
        USING (route_mode, bucket_started_at)
      LEFT JOIN opportunity_claims AS claim
        USING (route_mode, bucket_started_at)
      LEFT JOIN swap_fee_activity AS swap_fee
        USING (route_mode, bucket_started_at)
      ORDER BY bucket.route_mode ASC, bucket.bucket_started_at ASC
`

const rebalanceSQLAutodepositTimeSeries = `
      WITH params AS (
        SELECT now() AT TIME ZONE 'UTC' AS utc_now
      ),
      ranges AS (
        SELECT
          '2d'::text AS range_key,
          2::integer AS bucket_hours,
          interval '2 hours' AS bucket_size,
          date_trunc('day', utc_now) - interval '1 day' AS started_at,
          date_bin(
            interval '2 hours',
            utc_now,
            timestamp '1970-01-01 00:00:00'
          ) AS ended_at
        FROM params

        UNION ALL

        SELECT
          '7d'::text AS range_key,
          6::integer AS bucket_hours,
          interval '6 hours' AS bucket_size,
          date_trunc('day', utc_now) - interval '6 days' AS started_at,
          date_bin(
            interval '6 hours',
            utc_now,
            timestamp '1970-01-01 00:00:00'
          ) AS ended_at
        FROM params

        UNION ALL

        SELECT
          '30d'::text AS range_key,
          24::integer AS bucket_hours,
          interval '1 day' AS bucket_size,
          date_trunc('day', utc_now) - interval '29 days' AS started_at,
          date_trunc('day', utc_now) AS ended_at
        FROM params
      ),
      buckets AS (
        SELECT
          range.range_key,
          range.bucket_hours,
          range.bucket_size,
          generate_series(
            range.started_at,
            range.ended_at,
            range.bucket_size
          ) AS bucket_started_at
        FROM ranges AS range
      ),
      window_bounds AS (
        SELECT MIN(started_at) AS started_at FROM ranges
      ),
      execution_events AS (
        SELECT
          execution.inserted_at AS event_at,
          CASE
            WHEN execution.decoded_evidence->>'status' = 'executed'
              AND execution.completion_failure_code IS NULL
              THEN 1
            ELSE 0
          END::bigint AS successful,
          CASE
            WHEN execution.decoded_evidence->>'status' = 'executed'
              AND execution.completion_failure_code IS NULL
              THEN execution.amount_raw
            ELSE 0
          END::bigint AS deposited_amount_raw,
          0::bigint AS post_pull_kamino_top_up
        FROM loyal_yield.balance_sweep_executions AS execution
        WHERE execution.inserted_at >= (
          SELECT started_at AT TIME ZONE 'UTC'
          FROM window_bounds
        )
          AND execution.inserted_at <= now()
          AND execution.decoded_evidence->>'status' = 'executed'
          AND execution.completion_failure_code IS NULL

        UNION ALL

        SELECT
          COALESCE(execution.completed_at, execution.inserted_at) AS event_at,
          0::bigint AS successful,
          0::bigint AS deposited_amount_raw,
          1::bigint AS post_pull_kamino_top_up
        FROM loyal_yield.balance_sweep_executions AS execution
        WHERE COALESCE(execution.completed_at, execution.inserted_at) >= (
          SELECT started_at AT TIME ZONE 'UTC'
          FROM window_bounds
        )
          AND COALESCE(execution.completed_at, execution.inserted_at) <= now()
          AND (
            execution.completion_failure_code IS NOT NULL
            OR (
              execution.decoded_evidence->>'status' LIKE 'partial_executed%'
              AND execution.decoded_evidence->>'status'
                <> 'partial_executed_idle_vault_deposited'
            )
          )
      ),
      execution_base AS (
        SELECT
          date_bin(
            interval '2 hours',
            event.event_at AT TIME ZONE 'UTC',
            timestamp '1970-01-01 00:00:00'
          ) AS base_bucket,
          SUM(event.successful)::bigint AS successful,
          SUM(event.deposited_amount_raw)::bigint AS deposited_amount_raw,
          SUM(event.post_pull_kamino_top_up)::bigint
            AS post_pull_kamino_top_up
        FROM execution_events AS event
        GROUP BY 1
      ),
      execution_activity AS (
        SELECT
          range.range_key,
          date_bin(
            range.bucket_size,
            base.base_bucket,
            timestamp '1970-01-01 00:00:00'
          ) AS bucket_started_at,
          SUM(base.successful)::bigint AS successful,
          SUM(base.deposited_amount_raw)::bigint AS deposited_amount_raw,
          SUM(base.post_pull_kamino_top_up)::bigint
            AS post_pull_kamino_top_up
        FROM execution_base AS base
        CROSS JOIN ranges AS range
        WHERE base.base_bucket >= range.started_at
          AND base.base_bucket < range.ended_at + range.bucket_size
        GROUP BY 1, 2
      ),
      pre_pull_claims AS (
        SELECT
          claim.updated_at AS event_at,
          CASE
            WHEN claim.claim_token ~ '^autodeposit-trigger:[0-9]+:[0-9]+:'
              THEN split_part(claim.claim_token, ':', 3)::bigint
          END AS scheduled_slot_id
        FROM loyal_yield.balance_sweep_lot_claims AS claim
        WHERE claim.updated_at >= (
          SELECT started_at AT TIME ZONE 'UTC'
          FROM window_bounds
        )
          AND claim.updated_at <= now()
          AND claim.status::text IN ('released', 'failed')
          AND claim.execution_id IS NULL
      ),
      scheduled_slots AS MATERIALIZED (
        SELECT
          slot.id,
          CASE
            WHEN strpos(lower(slot.last_error), 'accountnotfound') > 0 THEN 1
            WHEN strpos(lower(slot.last_error), 'insufficientfundsforrent') > 0
              THEN 2
            WHEN strpos(lower(slot.last_error), 'owner does not match') > 0
              OR strpos(lower(slot.last_error), 'missing token delegate') > 0
              THEN 3
            WHEN strpos(lower(slot.last_error), 'unable to confirm transaction') > 0
              OR strpos(lower(slot.last_error), 'timed out') > 0
              OR strpos(lower(slot.last_error), 'blockhashnotfound') > 0
              THEN 4
            WHEN slot.last_error IS NULL OR slot.last_error = '' THEN 5
            ELSE 6
          END::smallint AS cause
        FROM loyal_yield.balance_sweep_scheduled_slots AS slot
      ),
      pre_pull_claim_buckets AS MATERIALIZED (
        SELECT
          date_bin(
            interval '2 hours',
            claim.event_at AT TIME ZONE 'UTC',
            timestamp '1970-01-01 00:00:00'
          ) AS base_bucket,
          claim.scheduled_slot_id,
          COUNT(*)::bigint AS event_count
        FROM pre_pull_claims AS claim
        GROUP BY 1, 2
      ),
      pre_pull_base AS (
        SELECT
          claim.base_bucket,
          COALESCE(SUM(claim.event_count) FILTER (
            WHERE slot.cause = 1
          ), 0)::bigint AS account_not_found,
          COALESCE(SUM(claim.event_count) FILTER (
            WHERE slot.cause = 2
          ), 0)::bigint AS insufficient_rent,
          COALESCE(SUM(claim.event_count) FILTER (
            WHERE slot.cause = 3
          ), 0)::bigint AS missing_token_delegate,
          COALESCE(SUM(claim.event_count) FILTER (
            WHERE slot.cause = 4
          ), 0)::bigint AS confirmation_or_timeout,
          COALESCE(SUM(claim.event_count) FILTER (
            WHERE slot.cause = 5 OR slot.id IS NULL
          ), 0)::bigint AS no_linked_error,
          COALESCE(SUM(claim.event_count) FILTER (
            WHERE slot.cause = 6
          ), 0)::bigint AS other_pre_pull
        FROM pre_pull_claim_buckets AS claim
        LEFT JOIN scheduled_slots AS slot
          ON slot.id = claim.scheduled_slot_id
        GROUP BY claim.base_bucket
      ),
      pre_pull_failure_activity AS (
        SELECT
          range.range_key,
          date_bin(
            range.bucket_size,
            base.base_bucket,
            timestamp '1970-01-01 00:00:00'
          ) AS bucket_started_at,
          SUM(base.account_not_found)::bigint AS account_not_found,
          SUM(base.insufficient_rent)::bigint AS insufficient_rent,
          SUM(base.missing_token_delegate)::bigint AS missing_token_delegate,
          SUM(base.confirmation_or_timeout)::bigint AS confirmation_or_timeout,
          SUM(base.no_linked_error)::bigint AS no_linked_error,
          SUM(base.other_pre_pull)::bigint AS other_pre_pull
        FROM pre_pull_base AS base
        CROSS JOIN ranges AS range
        WHERE base.base_bucket >= range.started_at
          AND base.base_bucket < range.ended_at + range.bucket_size
        GROUP BY 1, 2
      )
      SELECT
        bucket.range_key,
        bucket.bucket_hours::text AS bucket_hours,
        bucket.bucket_started_at AT TIME ZONE 'UTC' AS bucket_started_at,
        COALESCE(execution.successful, 0)::text AS successful,
        COALESCE(execution.deposited_amount_raw, 0)::text
          AS deposited_amount_raw,
        COALESCE(failure.account_not_found, 0)::text
          AS account_not_found,
        COALESCE(failure.insufficient_rent, 0)::text
          AS insufficient_rent,
        COALESCE(failure.missing_token_delegate, 0)::text
          AS missing_token_delegate,
        COALESCE(failure.confirmation_or_timeout, 0)::text
          AS confirmation_or_timeout,
        COALESCE(failure.no_linked_error, 0)::text
          AS no_linked_error,
        COALESCE(failure.other_pre_pull, 0)::text AS other_pre_pull,
        COALESCE(execution.post_pull_kamino_top_up, 0)::text
          AS post_pull_kamino_top_up
      FROM buckets AS bucket
      LEFT JOIN execution_activity AS execution
        USING (range_key, bucket_started_at)
      LEFT JOIN pre_pull_failure_activity AS failure
        USING (range_key, bucket_started_at)
      ORDER BY
        CASE bucket.range_key
          WHEN '2d' THEN 1
          WHEN '7d' THEN 2
          ELSE 3
        END,
        bucket.bucket_started_at ASC
`

const rebalanceSQLOptimizationVolume = `
      WITH confirmed_daily AS (
        SELECT
          (decision.updated_at AT TIME ZONE 'UTC')::date AS date,
          COUNT(*)::bigint AS confirmed_count,
          COALESCE(SUM(decision.amount_raw), 0)::bigint AS daily_amount_raw
        FROM loyal_yield.rebalance_decisions AS decision
        WHERE decision.status = 'confirmed'
          AND decision.signature IS NOT NULL
          AND decision.amount_raw IS NOT NULL
        GROUP BY 1
      ),
      bounds AS (
        SELECT
          COALESCE(
            MIN(date),
            (now() AT TIME ZONE 'UTC')::date
          ) AS started_on,
          (now() AT TIME ZONE 'UTC')::date AS ended_on
        FROM confirmed_daily
      ),
      days AS (
        SELECT generate_series(
          (SELECT started_on FROM bounds),
          (SELECT ended_on FROM bounds),
          interval '1 day'
        )::date AS date
      ),
      daily AS (
        SELECT
          day.date,
          COALESCE(confirmed.confirmed_count, 0)::bigint AS confirmed_count,
          COALESCE(confirmed.daily_amount_raw, 0)::bigint AS daily_amount_raw
        FROM days AS day
        LEFT JOIN confirmed_daily AS confirmed USING (date)
      ),
      cumulative AS (
        SELECT
          date,
          confirmed_count,
          daily_amount_raw,
          SUM(daily_amount_raw) OVER (ORDER BY date ASC) AS cumulative_amount_raw
        FROM daily
      )
      SELECT
        to_char(date, 'YYYY-MM-DD') AS date,
        confirmed_count::text AS confirmed_count,
        daily_amount_raw::text AS daily_amount_raw,
        cumulative_amount_raw::text AS cumulative_amount_raw
      FROM cumulative
      WHERE date > (now() AT TIME ZONE 'UTC')::date - 90
      ORDER BY date ASC
`

const rebalanceSQLLast30Days = `
      WITH bounds AS (
        SELECT
          date_trunc('day', now() AT TIME ZONE 'UTC') - interval '29 days'
            AS started_at,
          now() AT TIME ZONE 'UTC' AS ended_at
      ),
      days AS (
        SELECT
          route_mode.route_mode,
          generate_series(
            (SELECT started_at::date FROM bounds),
            (SELECT ended_at::date FROM bounds),
            interval '1 day'
          )::date AS date
        FROM (
          VALUES ('same_mint'), ('cross_mint_jupiter')
        ) AS route_mode(route_mode)
      ),
      daily AS (
        SELECT
          decision.execution_plan->>'kind' AS route_mode,
          (decision.updated_at AT TIME ZONE 'UTC')::date AS date,
          COUNT(*) FILTER (
            WHERE decision.status = 'confirmed'
          )::bigint AS confirmed,
          COUNT(*) FILTER (
            WHERE decision.status = 'failed'
          )::bigint AS failed
        FROM loyal_yield.rebalance_decisions AS decision
        WHERE decision.execution_plan->>'kind' IN (
            'same_mint',
            'cross_mint_jupiter'
          )
          AND decision.status IN ('confirmed', 'failed')
          AND decision.updated_at >= (
            SELECT started_at AT TIME ZONE 'UTC' FROM bounds
          )
          AND decision.updated_at < (
            SELECT ended_at AT TIME ZONE 'UTC' FROM bounds
          )
        GROUP BY 1, 2
      )
      SELECT
        day.route_mode,
        to_char(day.date, 'YYYY-MM-DD') AS date,
        COALESCE(daily.confirmed, 0)::text AS confirmed,
        COALESCE(daily.failed, 0)::text AS failed,
        (
          COALESCE(daily.confirmed, 0) + COALESCE(daily.failed, 0)
        )::text AS terminal_attempts
      FROM days AS day
      LEFT JOIN daily USING (route_mode, date)
      ORDER BY day.route_mode ASC, day.date ASC
`

const rebalanceSQLExecutedHistory = `
      WITH executed AS MATERIALIZED (
        SELECT
          decision.id,
          decision.updated_at AS executed_at,
          decision.amount_raw,
          decision.source_reserve,
          decision.target_reserve,
          decision.liquidity_mint,
          decision.source_liquidity_mint,
          decision.target_liquidity_mint,
          decision.execution_plan->>'kind' AS route_mode,
          decision.confirmed_slot,
          policy.authority
        FROM loyal_yield.rebalance_decisions AS decision
        INNER JOIN loyal_yield.managed_vaults AS vault
          ON vault.id = decision.vault_id
        INNER JOIN loyal_yield.route_policies AS policy
          ON policy.id = vault.active_policy_id
        WHERE decision.status = 'confirmed'
          AND decision.execution_plan->>'kind' IN (
            'same_mint',
            'cross_mint_jupiter'
          )
          AND decision.source_reserve IS NOT NULL
          AND decision.target_reserve IS NOT NULL
          AND decision.amount_raw IS NOT NULL
          AND decision.confirmed_slot IS NOT NULL
      ),
      swap_fees AS MATERIALIZED (
        SELECT
          submission.decision_id,
          COALESCE(SUM(submission.compiled_fee_lamports), 0)::bigint
            AS swap_fee_lamports
        FROM loyal_yield.signed_route_submissions AS submission
        INNER JOIN executed
          ON executed.id = submission.decision_id
        WHERE submission.movement_leg = 'swap'
          AND submission.finalized_at IS NOT NULL
        GROUP BY submission.decision_id
      ),
      executed_users AS MATERIALIZED (
        SELECT DISTINCT authority, route_mode FROM executed
      ),
      relevant_vaults AS MATERIALIZED (
        SELECT vault.id AS vault_id, policy.authority, executed_user.route_mode
        FROM loyal_yield.managed_vaults AS vault
        INNER JOIN loyal_yield.route_policies AS policy
          ON policy.id = vault.active_policy_id
        INNER JOIN executed_users AS executed_user
          ON executed_user.authority = policy.authority
      ),
      relevant_vault_ids AS MATERIALIZED (
        SELECT DISTINCT vault_id FROM relevant_vaults
      ),
      current_reserve_by_vault AS MATERIALIZED (
        SELECT
          position.vault_id,
          SUM(
            CASE
              WHEN COALESCE(
                position.planning_metadata->>'amountSemantics',
                position.planning_metadata->>'amount_semantics'
              ) IN (
                'kamino_redeemable_liquidity',
                'redeemable_liquidity_amount'
              )
                THEN position.amount_raw
              WHEN COALESCE(
                position.planning_metadata->>'amountSemantics',
                position.planning_metadata->>'amount_semantics'
              ) = 'kamino_obligation_collateral_deposited_amount'
                AND COALESCE(
                  position.planning_metadata->>'redeemable_liquidity_amount_raw',
                  position.planning_metadata->>'redeemable_source_liquidity_amount_raw'
                ) ~ '^[0-9]+$'
                THEN COALESCE(
                  position.planning_metadata->>'redeemable_liquidity_amount_raw',
                  position.planning_metadata->>'redeemable_source_liquidity_amount_raw'
                )::bigint
              ELSE 0::bigint
            END
          )::bigint AS amount_raw
        FROM relevant_vault_ids AS relevant
        CROSS JOIN LATERAL (
          SELECT position.*
          FROM loyal_yield.vault_reserve_positions_current AS position
          WHERE position.vault_id = relevant.vault_id
            AND position.has_value = true
            AND position.amount_raw > 0
          OFFSET 0
        ) AS position
        GROUP BY position.vault_id
      ),
      current_idle_by_vault AS MATERIALIZED (
        SELECT idle.vault_id, SUM(idle.amount_raw)::bigint AS amount_raw
        FROM relevant_vault_ids AS relevant
        CROSS JOIN LATERAL (
          SELECT idle.*
          FROM loyal_yield.vault_idle_token_balances_current AS idle
          WHERE idle.vault_id = relevant.vault_id
            AND idle.amount_raw > 0
          OFFSET 0
        ) AS idle
        GROUP BY idle.vault_id
      ),
      current_user_deposits AS MATERIALIZED (
        SELECT
          relevant.authority,
          relevant.route_mode,
          SUM(
            COALESCE(reserve.amount_raw, 0::bigint)
            + COALESCE(idle.amount_raw, 0::bigint)
          )::bigint AS current_deposit_raw
        FROM relevant_vaults AS relevant
        LEFT JOIN current_reserve_by_vault AS reserve
          ON reserve.vault_id = relevant.vault_id
        LEFT JOIN current_idle_by_vault AS idle
          ON idle.vault_id = relevant.vault_id
        GROUP BY relevant.authority, relevant.route_mode
      ),
      ranked_users AS MATERIALIZED (
        SELECT
          executed_user.authority,
          executed_user.route_mode,
          COALESCE(deposit_total.current_deposit_raw, 0::bigint)
            AS current_deposit_raw,
          ROW_NUMBER() OVER (
            PARTITION BY executed_user.route_mode
            ORDER BY
              COALESCE(deposit_total.current_deposit_raw, 0::bigint) ASC,
              executed_user.authority ASC
          )::integer AS user_rank
        FROM executed_users AS executed_user
        LEFT JOIN current_user_deposits AS deposit_total
          ON deposit_total.authority = executed_user.authority
          AND deposit_total.route_mode = executed_user.route_mode
      ),
      execution_rows AS MATERIALIZED (
        SELECT
          executed.id::text,
          executed.executed_at,
          executed.amount_raw::text,
          executed.source_reserve,
          executed.target_reserve,
          executed.liquidity_mint,
          executed.source_liquidity_mint,
          executed.target_liquidity_mint,
          executed.route_mode,
          COALESCE(swap_fee.swap_fee_lamports, 0)::text AS swap_fee_lamports,
          executed.confirmed_slot::text,
          executed.authority,
          ranked_users.current_deposit_raw::text,
          ranked_users.user_rank::text,
          ROW_NUMBER() OVER (
            PARTITION BY executed.route_mode, executed.liquidity_mint
            ORDER BY executed.executed_at DESC, executed.id DESC
          )::integer AS detail_rank
        FROM executed
        INNER JOIN ranked_users
          ON ranked_users.authority = executed.authority
          AND ranked_users.route_mode = executed.route_mode
        LEFT JOIN swap_fees AS swap_fee
          ON swap_fee.decision_id = executed.id
      ),
      chart_rows AS (
        SELECT
          date_trunc('hour', executed_at, 'UTC') AS bucket_at,
          route_mode,
          liquidity_mint,
          source_reserve,
          target_reserve,
          source_liquidity_mint,
          target_liquidity_mint,
          COUNT(*)::integer AS execution_count,
          COUNT(DISTINCT authority)::integer AS user_count,
          SUM(amount_raw::numeric)::text AS amount_raw,
          ROUND(AVG(current_deposit_raw::numeric))::text AS avg_deposit_raw,
          SUM(swap_fee_lamports::numeric)::text AS swap_fee_lamports
        FROM execution_rows
        WHERE executed_at >= now() - interval '90 days'
        GROUP BY
          date_trunc('hour', executed_at, 'UTC'),
          route_mode,
          liquidity_mint,
          source_reserve,
          target_reserve,
          source_liquidity_mint,
          target_liquidity_mint
      ),
      detail_rows AS (
        SELECT
          id,
          executed_at,
          amount_raw,
          source_reserve,
          target_reserve,
          liquidity_mint,
          source_liquidity_mint,
          target_liquidity_mint,
          route_mode,
          swap_fee_lamports,
          confirmed_slot,
          authority,
          current_deposit_raw::text AS current_deposit_raw,
          user_rank
        FROM execution_rows
        WHERE detail_rank <= $1
      ),
      summary_rows AS (
        SELECT
          route_mode,
          liquidity_mint,
          GROUPING(liquidity_mint) = 1 AS route_total,
          COUNT(*)::integer AS execution_count,
          COUNT(*) FILTER (
            WHERE executed_at >= now() - interval '30 days'
          )::integer AS execution_count_30d,
          COUNT(*) FILTER (
            WHERE executed_at >= now() - interval '7 days'
          )::integer AS execution_count_7d,
          COUNT(*) FILTER (
            WHERE current_deposit_raw::bigint = 0
          )::integer AS fully_withdrawn_count,
          COUNT(*) FILTER (
            WHERE current_deposit_raw::bigint = 0
              AND executed_at >= now() - interval '30 days'
          )::integer AS fully_withdrawn_count_30d,
          COUNT(*) FILTER (
            WHERE current_deposit_raw::bigint = 0
              AND executed_at >= now() - interval '7 days'
          )::integer AS fully_withdrawn_count_7d,
          COUNT(DISTINCT authority)::integer AS user_count,
          COUNT(DISTINCT authority) FILTER (
            WHERE executed_at >= now() - interval '30 days'
          )::integer AS user_count_30d,
          COUNT(DISTINCT authority) FILTER (
            WHERE executed_at >= now() - interval '7 days'
          )::integer AS user_count_7d,
          COALESCE(SUM(swap_fee_lamports::bigint), 0)::text AS swap_fee_lamports
        FROM execution_rows
        GROUP BY GROUPING SETS ((route_mode, liquidity_mint), (route_mode))
      )
      SELECT
        COALESCE(
          (
            SELECT json_agg(summary_row ORDER BY route_total, route_mode, liquidity_mint)
            FROM (
              SELECT
                route_mode,
                liquidity_mint,
                route_total,
                execution_count,
                execution_count_30d,
                execution_count_7d,
                fully_withdrawn_count,
                fully_withdrawn_count_30d,
                fully_withdrawn_count_7d,
                user_count,
                user_count_30d,
                user_count_7d,
                swap_fee_lamports
              FROM summary_rows
            ) AS summary_row
          ),
          '[]'::json
        ) AS summaries,
        COALESCE(
          (
            SELECT json_agg(
              json_build_array(
                chart_row.bucket_at,
                chart_row.route_mode,
                chart_row.liquidity_mint,
                chart_row.source_reserve,
                chart_row.target_reserve,
                chart_row.source_liquidity_mint,
                chart_row.target_liquidity_mint,
                chart_row.execution_count,
                chart_row.user_count,
                chart_row.amount_raw,
                chart_row.avg_deposit_raw,
                chart_row.swap_fee_lamports
              )
              ORDER BY
                chart_row.bucket_at ASC,
                chart_row.source_reserve ASC,
                chart_row.target_reserve ASC,
                chart_row.route_mode ASC
            )
            FROM chart_rows AS chart_row
          ),
          '[]'::json
        ) AS chart_points,
        COALESCE(
          (
            SELECT json_agg(
              json_build_array(
                detail_row.amount_raw,
                detail_row.authority,
                detail_row.confirmed_slot,
                detail_row.current_deposit_raw,
                detail_row.executed_at,
                detail_row.id,
                detail_row.liquidity_mint,
                detail_row.route_mode,
                detail_row.source_reserve,
                detail_row.source_liquidity_mint,
                detail_row.swap_fee_lamports,
                detail_row.target_reserve,
                detail_row.target_liquidity_mint,
                detail_row.user_rank
              )
              ORDER BY detail_row.executed_at DESC, detail_row.id DESC
            )
            FROM detail_rows AS detail_row
          ),
          '[]'::json
        ) AS details
`

const rebalanceSQLVaultFrequency = `
      WITH active_vaults AS MATERIALIZED (
        SELECT
          vault.id,
          policy.cluster,
          vault.settings,
          vault.vault_index,
          vault.vault_pubkey
        FROM loyal_yield.managed_vaults AS vault
        INNER JOIN loyal_yield.route_policies AS policy
          ON policy.id = vault.active_policy_id
        WHERE vault.active = true
          AND vault.vault_index = 1
      ),
      route_modes AS MATERIALIZED (
        SELECT *
        FROM (VALUES ('same_mint'), ('cross_mint_jupiter')) AS modes(route_mode)
      ),
      monitored_vault_modes AS MATERIALIZED (
        SELECT vault.*, route_mode.route_mode
        FROM active_vaults AS vault
        CROSS JOIN route_modes AS route_mode
        WHERE route_mode.route_mode = 'same_mint'
          OR EXISTS (
            SELECT 1
            FROM loyal_yield.cross_mint_vault_opt_ins AS opt_in
            WHERE opt_in.cluster = vault.cluster
              AND opt_in.settings = vault.settings
              AND opt_in.vault_index = vault.vault_index
              AND opt_in.vault_pubkey = vault.vault_pubkey
              AND opt_in.enabled = true
          )
      ),
      normalized_positions AS MATERIALIZED (
        SELECT
          position.vault_id,
          position.reserve,
          position.liquidity_mint,
          position.observed_at,
          CASE
            WHEN COALESCE(
              position.planning_metadata->>'amountSemantics',
              position.planning_metadata->>'amount_semantics'
            ) IN (
              'kamino_redeemable_liquidity',
              'redeemable_liquidity_amount'
            )
              THEN position.amount_raw
            WHEN COALESCE(
              position.planning_metadata->>'amountSemantics',
              position.planning_metadata->>'amount_semantics'
            ) = 'kamino_obligation_collateral_deposited_amount'
              AND COALESCE(
                position.planning_metadata->>'redeemable_liquidity_amount_raw',
                position.planning_metadata->>'redeemable_source_liquidity_amount_raw'
              ) ~ '^[0-9]+$'
              THEN COALESCE(
                position.planning_metadata->>'redeemable_liquidity_amount_raw',
                position.planning_metadata->>'redeemable_source_liquidity_amount_raw'
              )::bigint
            ELSE 0::bigint
          END AS normalized_amount_raw
        FROM active_vaults AS vault
        CROSS JOIN LATERAL (
          SELECT position.*
          FROM loyal_yield.vault_reserve_positions_current AS position
          WHERE position.vault_id = vault.id
            AND position.has_value = true
            AND position.amount_raw > 0
          OFFSET 0
        ) AS position
      ),
      positive_positions AS MATERIALIZED (
        SELECT *
        FROM normalized_positions
        WHERE normalized_amount_raw > 0
      ),
      position_totals AS MATERIALIZED (
        SELECT
          vault_id,
          SUM(normalized_amount_raw)::bigint AS amount_raw,
          COUNT(*)::integer AS position_count
        FROM positive_positions
        GROUP BY vault_id
      ),
      primary_positions AS MATERIALIZED (
        SELECT vault_id, reserve, liquidity_mint
        FROM (
          SELECT
            position.*,
            ROW_NUMBER() OVER (
              PARTITION BY position.vault_id
              ORDER BY
                position.normalized_amount_raw DESC,
                position.observed_at DESC NULLS LAST,
                position.reserve ASC
            ) AS priority
          FROM positive_positions AS position
        ) AS ranked
        WHERE priority = 1
      ),
      positive_idle AS MATERIALIZED (
        SELECT idle.*
        FROM active_vaults AS vault
        CROSS JOIN LATERAL (
          SELECT idle.*
          FROM loyal_yield.vault_idle_token_balances_current AS idle
          WHERE idle.vault_id = vault.id
            AND idle.amount_raw > 0
          OFFSET 0
        ) AS idle
      ),
      idle_totals AS MATERIALIZED (
        SELECT vault_id, SUM(amount_raw)::bigint AS amount_raw
        FROM positive_idle
        GROUP BY vault_id
      ),
      primary_idle AS MATERIALIZED (
        SELECT vault_id, mint
        FROM (
          SELECT
            idle.*,
            ROW_NUMBER() OVER (
              PARTITION BY idle.vault_id
              ORDER BY
                idle.amount_raw DESC,
                idle.observed_at DESC NULLS LAST,
                idle.mint ASC
            ) AS priority
          FROM positive_idle AS idle
        ) AS ranked
        WHERE priority = 1
      ),
      rebalance_counts AS MATERIALIZED (
        SELECT
          decision.vault_id,
          decision.execution_plan->>'kind' AS route_mode,
          COUNT(*)::integer AS all_count,
          COUNT(*) FILTER (
            WHERE decision.updated_at >= NOW() - INTERVAL '7 days'
          )::integer AS last_7d_count,
          COUNT(*) FILTER (
            WHERE decision.updated_at >= NOW() - INTERVAL '12 hours'
          )::integer AS last_12h_count,
          COUNT(*) FILTER (
            WHERE decision.updated_at >= NOW() - INTERVAL '2 hours'
          )::integer AS last_2h_count
        FROM loyal_yield.rebalance_decisions AS decision
        INNER JOIN active_vaults AS active_vault
          ON active_vault.id = decision.vault_id
        WHERE decision.status = 'confirmed'
          AND decision.execution_plan->>'kind' IN (
            'same_mint',
            'cross_mint_jupiter'
          )
          AND decision.source_reserve IS NOT NULL
          AND decision.target_reserve IS NOT NULL
        GROUP BY decision.vault_id, decision.execution_plan->>'kind'
      ),
      current_vaults_base AS MATERIALIZED (
        SELECT
          vault.id,
          vault.cluster,
          vault.vault_pubkey,
          vault.route_mode,
          primary_position.reserve AS current_reserve,
          COALESCE(primary_position.liquidity_mint, primary_idle.mint)
            AS liquidity_mint,
          COALESCE(position_total.amount_raw, 0::bigint)
            + COALESCE(idle_total.amount_raw, 0::bigint)
            AS current_deposit_raw,
          COALESCE(position_total.position_count, 0) AS position_count,
          COALESCE(rebalance.all_count, 0) AS all_count,
          COALESCE(rebalance.last_7d_count, 0) AS last_7d_count,
          COALESCE(rebalance.last_12h_count, 0) AS last_12h_count,
          COALESCE(rebalance.last_2h_count, 0) AS last_2h_count
        FROM monitored_vault_modes AS vault
        LEFT JOIN position_totals AS position_total
          ON position_total.vault_id = vault.id
        LEFT JOIN primary_positions AS primary_position
          ON primary_position.vault_id = vault.id
        LEFT JOIN idle_totals AS idle_total
          ON idle_total.vault_id = vault.id
        LEFT JOIN primary_idle
          ON primary_idle.vault_id = vault.id
        LEFT JOIN rebalance_counts AS rebalance
          ON rebalance.vault_id = vault.id
          AND rebalance.route_mode = vault.route_mode
        WHERE COALESCE(position_total.amount_raw, 0::bigint)
          + COALESCE(idle_total.amount_raw, 0::bigint) > 0
      ),
      current_vaults AS MATERIALIZED (
        SELECT
          base.*,
          CASE
            WHEN {{opportunityGate}}
              THEN CASE
                WHEN base.route_mode = 'same_mint' THEN EXISTS (
                  SELECT 1
                  FROM loyal_yield.rebalance_opportunities AS opportunity
                  WHERE opportunity.cluster = base.cluster
                    AND opportunity.vault_id = base.id
                    AND opportunity.source_reserve IS NOT NULL
                    AND opportunity.target_reserve IS NOT NULL
                    AND opportunity.execution_plan->>'kind' = 'same_mint'
                  LIMIT 1
                )
                WHEN base.route_mode = 'cross_mint_jupiter' THEN EXISTS (
                  SELECT 1
                  FROM loyal_yield.rebalance_opportunities AS opportunity
                  WHERE opportunity.cluster = base.cluster
                    AND opportunity.vault_id = base.id
                    AND opportunity.source_reserve IS NOT NULL
                    AND opportunity.target_reserve IS NOT NULL
                    AND opportunity.execution_plan->>'kind' = 'cross_mint_jupiter'
                  LIMIT 1
                )
                ELSE FALSE
              END
            ELSE FALSE
          END AS has_opportunity
        FROM current_vaults_base AS base
      ),
      ranked_vaults AS MATERIALIZED (
        SELECT
          current_vault.*,
          ROW_NUMBER() OVER (
            PARTITION BY current_vault.route_mode
            ORDER BY
              current_vault.current_deposit_raw ASC,
              current_vault.vault_pubkey ASC
          )::integer AS deposit_rank
        FROM current_vaults AS current_vault
      ),
      frequency_rows AS MATERIALIZED (
        SELECT
          ranked_vault.*,
          MAX(ranked_vault.deposit_rank) OVER (
            PARTITION BY ranked_vault.route_mode
          )::integer AS vault_count,
          ROW_NUMBER() OVER (
            PARTITION BY
              ranked_vault.route_mode,
              ranked_vault.liquidity_mint,
              ranked_vault.current_reserve
            ORDER BY ranked_vault.current_deposit_raw DESC, ranked_vault.vault_pubkey DESC
          )::integer AS detail_rank
        FROM ranked_vaults AS ranked_vault
      ),
      chart_rows AS (
        SELECT
          id::text AS vault_id,
          vault_pubkey,
          current_reserve,
          liquidity_mint,
          route_mode,
          current_deposit_raw::text AS current_deposit_raw,
          position_count,
          all_count,
          last_7d_count,
          last_12h_count,
          last_2h_count,
          deposit_rank
          ,vault_count
        FROM frequency_rows
      ),
      detail_rows AS (
        SELECT
          id::text AS vault_id,
          vault_pubkey,
          current_reserve,
          liquidity_mint,
          route_mode,
          current_deposit_raw::text AS current_deposit_raw,
          position_count,
          all_count,
          last_7d_count,
          last_12h_count,
          last_2h_count,
          deposit_rank
          ,vault_count
        FROM frequency_rows
        WHERE detail_rank <= $1
      ),
      summary_rows AS (
        SELECT
          route_mode,
          liquidity_mint,
          COUNT(*)::integer AS vault_count,
          COUNT(*) FILTER (
            WHERE {{eligible:all_count:opportunity}}
          )::integer AS eligible_count,
          COUNT(*) FILTER (
            WHERE {{eligible:last_7d_count}}
          )::integer AS eligible_count_7d,
          COUNT(*) FILTER (
            WHERE {{eligible:last_12h_count}}
          )::integer AS eligible_count_12h,
          COUNT(*) FILTER (
            WHERE {{eligible:last_2h_count}}
          )::integer AS eligible_count_2h,
          COUNT(*) FILTER (WHERE all_count > 0)::integer AS rebalanced_vault_count,
          COALESCE(SUM(position_count), 0)::integer AS position_count,
          COALESCE(SUM(all_count), 0)::integer AS rebalance_all_count,
          COALESCE(SUM(last_7d_count), 0)::integer AS rebalance_last_7d_count,
          COALESCE(SUM(last_12h_count), 0)::integer AS rebalance_last_12h_count,
          COALESCE(SUM(last_2h_count), 0)::integer AS rebalance_last_2h_count
        FROM current_vaults
        GROUP BY route_mode, liquidity_mint
      )
      SELECT
        COALESCE(
          (
            SELECT json_agg(summary_row ORDER BY route_mode, liquidity_mint)
            FROM summary_rows AS summary_row
          ),
          '[]'::json
        ) AS summaries,
        COALESCE(
          (
            SELECT json_agg(
              json_build_array(
                chart_row.all_count,
                chart_row.current_deposit_raw,
                chart_row.current_reserve,
                chart_row.deposit_rank,
                chart_row.last_12h_count,
                chart_row.last_2h_count,
                chart_row.last_7d_count,
                chart_row.liquidity_mint,
                chart_row.position_count,
                chart_row.route_mode,
                chart_row.vault_id,
                chart_row.vault_pubkey
              )
              ORDER BY chart_row.route_mode, chart_row.deposit_rank
            )
            FROM chart_rows AS chart_row
          ),
          '[]'::json
        ) AS chart_points,
        COALESCE(
          (
            SELECT json_agg(
              json_build_array(
                detail_row.all_count,
                detail_row.current_deposit_raw,
                detail_row.current_reserve,
                detail_row.deposit_rank,
                detail_row.last_12h_count,
                detail_row.last_2h_count,
                detail_row.last_7d_count,
                detail_row.liquidity_mint,
                detail_row.position_count,
                detail_row.route_mode,
                detail_row.vault_id,
                detail_row.vault_pubkey
              )
              ORDER BY detail_row.route_mode, detail_row.deposit_rank
            )
            FROM detail_rows AS detail_row
          ),
          '[]'::json
        ) AS details
`

const rebalanceSQLVaultOpportunityCounts = `
      WITH target_vault AS MATERIALIZED (
        SELECT vault.id, policy.cluster
        FROM loyal_yield.managed_vaults AS vault
        INNER JOIN loyal_yield.route_policies AS policy
          ON policy.id = vault.active_policy_id
        WHERE vault.id = $1::bigint
          AND vault.active = true
          AND vault.vault_index = 1
      )
      SELECT
        target.id::text AS vault_id,
        $2::text AS route_mode,
        COUNT(opportunity.id)::integer AS all_count,
        COUNT(opportunity.id) FILTER (
          WHERE opportunity.created_at >= NOW() - INTERVAL '7 days'
        )::integer AS last_7d_count,
        COUNT(opportunity.id) FILTER (
          WHERE opportunity.created_at >= NOW() - INTERVAL '12 hours'
        )::integer AS last_12h_count,
        COUNT(opportunity.id) FILTER (
          WHERE opportunity.created_at >= NOW() - INTERVAL '2 hours'
        )::integer AS last_2h_count
      FROM target_vault AS target
      LEFT JOIN loyal_yield.rebalance_opportunities AS opportunity
        ON opportunity.cluster = target.cluster
        AND opportunity.vault_id = target.id
        AND opportunity.source_reserve IS NOT NULL
        AND opportunity.target_reserve IS NOT NULL
        AND opportunity.execution_plan->>'kind' = $2
      GROUP BY target.id
`

const rebalanceSQLAuditSummary = `
      SELECT
        COUNT(*) FILTER (
          WHERE audit.record_type = 'decision'
            AND audit.movement_source = 'rebalance'
            AND audit.status = 'confirmed'
        )::text AS completed_rebalances,
        COUNT(*) FILTER (
          WHERE (
            audit.record_type = 'yield_deposit'
            AND audit.status = 'confirmed'
          ) OR (
            audit.record_type = 'decision'
            AND audit.movement_source = 'idle_vault_deposit'
            AND audit.is_idle_fallback
            AND audit.status = 'confirmed'
          )
        )::text AS completed_deposits,
        COUNT(*) FILTER (
          WHERE audit.record_type = 'decision'
            AND audit.movement_source = 'rebalance'
            AND audit.status IN ('failed', 'abandoned')
        )::text AS rebalance_errors,
        COUNT(*) FILTER (
          WHERE (
            audit.record_type = 'decision'
            AND audit.movement_source = 'idle_vault_deposit'
            AND audit.status IN ('failed', 'abandoned')
          ) OR (
            audit.record_type = 'execution'
            AND audit.movement_source = 'autodeposit'
            AND audit.status = 'failed'
          )
        )::text AS deposit_errors,
        COUNT(*) FILTER (
          WHERE (
            audit.record_type = 'decision'
            AND audit.movement_source = 'rebalance'
            AND audit.status IN ('failed', 'abandoned')
          ) OR (
            audit.record_type = 'decision'
            AND audit.movement_source = 'idle_vault_deposit'
            AND audit.status IN ('failed', 'abandoned')
          ) OR (
            audit.record_type = 'execution'
            AND audit.movement_source = 'autodeposit'
            AND audit.status = 'failed'
          ) OR (
            audit.record_type = 'decision'
            AND (
              audit.movement_source = 'needs_review'
              OR audit.status NOT IN (
                'planned', 'simulating', 'ready', 'submitted', 'confirming',
                'confirmed', 'failed', 'abandoned', 'skipped'
              )
            )
          )
        )::text AS errors,
        COUNT(*) FILTER (
          WHERE audit.record_type = 'decision'
            AND (
              audit.movement_source = 'needs_review'
              OR audit.status NOT IN (
                'planned', 'simulating', 'ready', 'submitted', 'confirming',
                'confirmed', 'failed', 'abandoned', 'skipped'
              )
            )
        )::text AS needs_review,
        COUNT(*) FILTER (
          WHERE audit.record_type = 'decision'
            AND audit.status IN (
            'planned', 'simulating', 'ready', 'submitted', 'confirming'
          )
        )::text AS active,
        COUNT(*) FILTER (
          WHERE audit.record_type = 'decision'
            AND audit.status IN (
            'planned', 'simulating', 'ready', 'submitted', 'confirming'
          )
          AND audit.event_at < now() - interval '2 minutes'
        )::text AS stale_active
      FROM audit_rows AS audit
`
