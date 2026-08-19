CREATE OR REPLACE VIEW loyal_yield.fleet_orchestration_status AS
WITH clusters AS (
    -- Fleet planners explicitly register before producing any durable work.
    -- Keeping this table authoritative avoids rediscovering one cluster by
    -- sorting every historical epoch, opportunity, outbox, and submission.
    SELECT cluster
    FROM loyal_yield.fleet_planning_clusters
), latest_market_epoch AS (
    SELECT cluster.cluster,
           latest.id,
           latest.epoch_key,
           latest.market_slot,
           latest.observed_at,
           latest.expires_at
    FROM clusters cluster
    LEFT JOIN LATERAL (
        SELECT epoch.id,
               epoch.epoch_key,
               epoch.market_slot,
               epoch.observed_at,
               epoch.expires_at
        FROM loyal_yield.optimizer_epochs epoch
        WHERE epoch.cluster = cluster.cluster
        ORDER BY epoch.observed_at DESC, epoch.id DESC
        LIMIT 1
    ) latest ON TRUE
), opportunity_aggregate AS (
    -- GROUPING SETS produces the per-state status rows and the cluster queue
    -- totals from one historical opportunity scan.
    SELECT cluster,
           opportunity_state,
           GROUPING(opportunity_state) AS is_cluster_total,
           count(*)::BIGINT AS opportunity_count,
           COALESCE(sum(principal_usd_micros), 0)::BIGINT
               AS principal_usd_micros,
           COALESCE(sum(annual_yield_gain_usd_micros), 0)::BIGINT
               AS annual_yield_gain_usd_micros,
           min(created_at) AS oldest_created_at,
           min(state_entered_at) AS oldest_state_entered_at,
           count(*) FILTER (
               WHERE opportunity_state = 'leased' AND lease_expires_at <= now()
           )::BIGINT AS expired_lease_count,
           count(*) FILTER (
               WHERE opportunity_state = 'waiting_alt'
           )::BIGINT AS waiting_alt_opportunity_count,
           COALESCE(sum(principal_usd_micros) FILTER (
               WHERE opportunity_state = 'waiting_alt'
           ), 0)::BIGINT AS waiting_alt_principal_usd_micros,
           COALESCE((sum(annual_yield_gain_usd_micros) FILTER (
               WHERE opportunity_state = 'waiting_alt'
           ) / 8760)::BIGINT, 0)::BIGINT
               AS waiting_alt_yield_gain_usd_micros_per_hour,
           min(state_entered_at) FILTER (
               WHERE opportunity_state = 'waiting_alt'
           ) AS oldest_waiting_alt_state_entered_at,
           count(*) FILTER (
               WHERE opportunity_state = 'ready'
           )::BIGINT AS ready_opportunity_count,
           COALESCE(sum(principal_usd_micros) FILTER (
               WHERE opportunity_state = 'ready'
           ), 0)::BIGINT AS ready_principal_usd_micros,
           COALESCE((sum(annual_yield_gain_usd_micros) FILTER (
               WHERE opportunity_state = 'ready'
           ) / 8760)::BIGINT, 0)::BIGINT
               AS ready_yield_gain_usd_micros_per_hour,
           min(state_entered_at) FILTER (
               WHERE opportunity_state = 'ready'
           ) AS oldest_ready_state_entered_at
    FROM loyal_yield.rebalance_opportunities
    GROUP BY GROUPING SETS ((cluster, opportunity_state), (cluster))
), opportunity_status AS (
    SELECT cluster,
           opportunity_state,
           opportunity_count,
           principal_usd_micros,
           annual_yield_gain_usd_micros,
           oldest_created_at,
           oldest_state_entered_at,
           expired_lease_count
    FROM opportunity_aggregate
    WHERE is_cluster_total = 0
), queue_status AS (
    SELECT cluster,
           waiting_alt_opportunity_count,
           waiting_alt_principal_usd_micros,
           waiting_alt_yield_gain_usd_micros_per_hour,
           oldest_waiting_alt_state_entered_at,
           ready_opportunity_count,
           ready_principal_usd_micros,
           ready_yield_gain_usd_micros_per_hour,
           oldest_ready_state_entered_at
    FROM opportunity_aggregate
    WHERE is_cluster_total = 1
), outbox_status AS (
    SELECT cluster,
           count(*)::BIGINT AS pending_outbox_count
    FROM loyal_yield.orchestration_outbox
    WHERE processed_at IS NULL
    GROUP BY cluster
), submission_status AS (
    SELECT cluster,
           count(*)::BIGINT AS pending_submission_count,
           COALESCE(sum(compiled_fee_lamports), 0)::BIGINT
               AS pending_compiled_fee_lamports,
           count(*) FILTER (
               WHERE submission_state = 'expiry_check_pending'
           )::BIGINT AS expiry_check_pending_count,
           count(*) FILTER (
               WHERE submission_state = 'effect_ambiguous'
           )::BIGINT AS effect_ambiguous_count,
           min(created_at) AS oldest_pending_submission_at,
           count(*) FILTER (
               WHERE submission_state = 'signed'
           )::BIGINT AS sender_submission_count,
           min(submission_state_entered_at) FILTER (
               WHERE submission_state = 'signed'
           ) AS oldest_sender_state_entered_at,
           count(*) FILTER (
               WHERE submission_state IN ('submitted', 'confirmed')
           )::BIGINT AS confirmer_submission_count,
           min(submission_state_entered_at) FILTER (
               WHERE submission_state IN ('submitted', 'confirmed')
           ) AS oldest_confirmer_state_entered_at,
           count(*) FILTER (
               WHERE submission_state IN (
                   'reconciliation_pending',
                   'expiry_check_pending',
                   'effect_ambiguous'
               )
           )::BIGINT AS reconciler_submission_count,
           min(submission_state_entered_at) FILTER (
               WHERE submission_state IN (
                   'reconciliation_pending',
                   'expiry_check_pending',
                   'effect_ambiguous'
               )
           ) AS oldest_reconciler_state_entered_at
    FROM loyal_yield.signed_route_submissions
    WHERE submission_state NOT IN ('reconciled', 'expired', 'failed')
    GROUP BY cluster
), current_epoch_opportunities AS MATERIALIZED (
    SELECT opportunity.id,
           opportunity.cluster,
           opportunity.created_at,
           opportunity.principal_usd_micros,
           opportunity.annual_yield_gain_usd_micros
    FROM latest_market_epoch epoch
    JOIN loyal_yield.rebalance_opportunities opportunity
      ON opportunity.optimizer_epoch_id = epoch.id
     AND opportunity.cluster = epoch.cluster
), submission_lifecycle AS (
    SELECT submission.opportunity_id,
           min(submission.submitted_at) FILTER (
               WHERE submission.submitted_at IS NOT NULL
           ) AS first_submitted_at,
           min(submission.confirmed_at) FILTER (
               WHERE submission.confirmed_at IS NOT NULL
           ) AS first_confirmed_at,
           COALESCE(sum(submission.compiled_fee_lamports), 0)::BIGINT
               AS compiled_fee_lamports
    FROM loyal_yield.signed_route_submissions submission
    JOIN current_epoch_opportunities opportunity
      ON opportunity.id = submission.opportunity_id
    GROUP BY submission.opportunity_id
), current_epoch_unlock AS (
    -- Queue admission already requires positive annual and expected net gain.
    -- Grouping by the latest immutable market epoch makes the denominator
    -- explicit and prevents a lifetime aggregate from hiding a new slowdown.
    SELECT epoch.cluster,
           count(opportunity.id)::BIGINT AS current_epoch_opportunity_count,
           COALESCE(sum(opportunity.principal_usd_micros), 0)::BIGINT
               AS current_epoch_principal_usd_micros,
           COALESCE((sum(opportunity.annual_yield_gain_usd_micros) / 8760)::BIGINT, 0)::BIGINT
               AS current_epoch_recoverable_yield_usd_micros_per_hour,
           COALESCE(floor(
               1000000::NUMERIC
               * COALESCE(sum(opportunity.annual_yield_gain_usd_micros) FILTER (
                   WHERE lifecycle.first_submitted_at
                       <= opportunity.created_at + interval '10 seconds'
               ), 0)
               / NULLIF(sum(opportunity.annual_yield_gain_usd_micros), 0)
           )::BIGINT, 0)::BIGINT
               AS current_epoch_submitted_within_10s_yield_ppm,
           COALESCE(floor(
               1000000::NUMERIC
               * COALESCE(sum(opportunity.annual_yield_gain_usd_micros) FILTER (
                   WHERE lifecycle.first_submitted_at
                       <= opportunity.created_at + interval '2 minutes'
               ), 0)
               / NULLIF(sum(opportunity.annual_yield_gain_usd_micros), 0)
           )::BIGINT, 0)::BIGINT
               AS current_epoch_submitted_within_2m_yield_ppm,
           COALESCE(floor(
               1000000::NUMERIC
               * COALESCE(sum(opportunity.annual_yield_gain_usd_micros) FILTER (
                   WHERE lifecycle.first_submitted_at
                       <= opportunity.created_at + interval '10 minutes'
               ), 0)
               / NULLIF(sum(opportunity.annual_yield_gain_usd_micros), 0)
           )::BIGINT, 0)::BIGINT
               AS current_epoch_submitted_within_10m_yield_ppm,
           COALESCE(floor(
               1000000::NUMERIC
               * COALESCE(sum(opportunity.annual_yield_gain_usd_micros) FILTER (
                   WHERE lifecycle.first_confirmed_at
                       <= opportunity.created_at + interval '30 seconds'
               ), 0)
               / NULLIF(sum(opportunity.annual_yield_gain_usd_micros), 0)
           )::BIGINT, 0)::BIGINT
               AS current_epoch_confirmed_within_30s_yield_ppm,
           ceil(percentile_cont(0.95) WITHIN GROUP (
               ORDER BY GREATEST(
                   extract(epoch FROM (
                       lifecycle.first_submitted_at - opportunity.created_at
                   )) * 1000,
                   0
               )
           ) FILTER (
               WHERE lifecycle.first_submitted_at IS NOT NULL
           ))::BIGINT AS current_epoch_submission_p95_milliseconds,
           ceil(percentile_cont(0.95) WITHIN GROUP (
               ORDER BY GREATEST(
                   extract(epoch FROM (
                       lifecycle.first_confirmed_at - opportunity.created_at
                   )) * 1000,
                   0
               )
           ) FILTER (
               WHERE lifecycle.first_confirmed_at IS NOT NULL
           ))::BIGINT AS current_epoch_confirmation_p95_milliseconds,
           COALESCE(sum(lifecycle.compiled_fee_lamports), 0)::BIGINT
               AS current_epoch_compiled_fee_lamports
    FROM latest_market_epoch epoch
    LEFT JOIN current_epoch_opportunities opportunity
      ON opportunity.cluster = epoch.cluster
    LEFT JOIN submission_lifecycle lifecycle
      ON lifecycle.opportunity_id = opportunity.id
    GROUP BY epoch.cluster
)
SELECT cluster.cluster,
       opportunity.opportunity_state,
       COALESCE(opportunity.opportunity_count, 0)::BIGINT AS opportunity_count,
       COALESCE(opportunity.principal_usd_micros, 0) AS principal_usd_micros,
       COALESCE(opportunity.annual_yield_gain_usd_micros, 0)
           AS annual_yield_gain_usd_micros,
       COALESCE((opportunity.annual_yield_gain_usd_micros / 8760)::BIGINT, 0)::BIGINT
           AS yield_gain_usd_micros_per_hour,
       opportunity.oldest_created_at,
       opportunity.oldest_state_entered_at,
       CASE
           WHEN opportunity.oldest_created_at IS NULL THEN NULL
           ELSE extract(epoch FROM now() - opportunity.oldest_created_at)::BIGINT
       END AS oldest_age_seconds,
       CASE
           WHEN opportunity.oldest_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - opportunity.oldest_state_entered_at
           )::BIGINT
       END AS oldest_state_age_seconds,
       COALESCE(opportunity.expired_lease_count, 0)::BIGINT AS expired_lease_count,
       COALESCE(outbox.pending_outbox_count, 0)::BIGINT AS pending_outbox_count,
       COALESCE(submission.pending_submission_count, 0)::BIGINT
           AS pending_submission_count,
       COALESCE(submission.pending_compiled_fee_lamports, 0)::BIGINT
           AS pending_compiled_fee_lamports,
       COALESCE(submission.expiry_check_pending_count, 0)::BIGINT
           AS expiry_check_pending_count,
       COALESCE(submission.effect_ambiguous_count, 0)::BIGINT
           AS effect_ambiguous_count,
       submission.oldest_pending_submission_at,
       CASE
           WHEN submission.oldest_pending_submission_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - submission.oldest_pending_submission_at
           )::BIGINT
       END AS oldest_pending_submission_age_seconds
       , COALESCE(submission.sender_submission_count, 0)::BIGINT
           AS sender_submission_count
       , submission.oldest_sender_state_entered_at
       , CASE
           WHEN submission.oldest_sender_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - submission.oldest_sender_state_entered_at
           )::BIGINT
         END AS oldest_sender_state_age_seconds
       , COALESCE(submission.confirmer_submission_count, 0)::BIGINT
           AS confirmer_submission_count
       , submission.oldest_confirmer_state_entered_at
       , CASE
           WHEN submission.oldest_confirmer_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - submission.oldest_confirmer_state_entered_at
           )::BIGINT
         END AS oldest_confirmer_state_age_seconds
       , COALESCE(submission.reconciler_submission_count, 0)::BIGINT
           AS reconciler_submission_count
       , submission.oldest_reconciler_state_entered_at
       , CASE
           WHEN submission.oldest_reconciler_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - submission.oldest_reconciler_state_entered_at
           )::BIGINT
         END AS oldest_reconciler_state_age_seconds
       , planning_cluster.registered_at AS planner_registered_at
       , planning_cluster.last_seen_at AS planner_last_seen_at
       , CASE
           WHEN planning_cluster.last_seen_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - planning_cluster.last_seen_at
           )::BIGINT
         END AS planner_last_seen_age_seconds
       , planning.full_sweep_started_at
       , planning.full_sweep_completed_at
       , CASE
           WHEN planning.full_sweep_completed_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - planning.full_sweep_completed_at
           )::BIGINT
         END AS full_sweep_age_seconds
       , planning.optimizer_epoch_key AS planned_optimizer_epoch_key
       , planning.optimizer_epoch_expires_at AS planned_optimizer_epoch_expires_at
       , planning.complete_frontier
       , planning.observed_vault_count
       , planning.opportunity_count AS planned_opportunity_count
       , planning.selected_count AS planned_selected_count
       , planning.deferred_count AS planned_deferred_count
       , planning.generation AS planning_generation
       , latest_epoch.id AS latest_market_epoch_id
       , latest_epoch.epoch_key AS latest_market_epoch_key
       , latest_epoch.market_slot AS latest_market_slot
       , latest_epoch.observed_at AS latest_market_observed_at
       , latest_epoch.expires_at AS latest_market_expires_at
       , CASE
           WHEN latest_epoch.observed_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - latest_epoch.observed_at
           )::BIGINT
         END AS latest_market_epoch_age_seconds
       , CASE
           WHEN latest_epoch.expires_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM latest_epoch.expires_at - now()
           )::BIGINT
         END AS latest_market_epoch_expires_in_seconds
       , CASE
           WHEN latest_epoch.expires_at IS NULL THEN NULL
           ELSE latest_epoch.expires_at <= now()
         END AS latest_market_epoch_expired
       , CASE
           WHEN planning.optimizer_epoch_key IS NULL
             OR latest_epoch.epoch_key IS NULL THEN NULL
           ELSE planning.optimizer_epoch_key = latest_epoch.epoch_key
         END AS planner_epoch_matches_latest
       , COALESCE(queue.waiting_alt_opportunity_count, 0)::BIGINT
           AS waiting_alt_opportunity_count
       , COALESCE(queue.waiting_alt_principal_usd_micros, 0)::BIGINT
           AS waiting_alt_principal_usd_micros
       , COALESCE(queue.waiting_alt_yield_gain_usd_micros_per_hour, 0)::BIGINT
           AS waiting_alt_yield_gain_usd_micros_per_hour
       , queue.oldest_waiting_alt_state_entered_at
       , CASE
           WHEN queue.oldest_waiting_alt_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - queue.oldest_waiting_alt_state_entered_at
           )::BIGINT
         END AS oldest_waiting_alt_state_age_seconds
       , COALESCE(queue.ready_opportunity_count, 0)::BIGINT
           AS ready_opportunity_count
       , COALESCE(queue.ready_principal_usd_micros, 0)::BIGINT
           AS ready_principal_usd_micros
       , COALESCE(queue.ready_yield_gain_usd_micros_per_hour, 0)::BIGINT
           AS ready_yield_gain_usd_micros_per_hour
       , queue.oldest_ready_state_entered_at
       , CASE
           WHEN queue.oldest_ready_state_entered_at IS NULL THEN NULL
           ELSE extract(
               epoch FROM now() - queue.oldest_ready_state_entered_at
           )::BIGINT
         END AS oldest_ready_state_age_seconds
       , COALESCE(unlock.current_epoch_opportunity_count, 0)::BIGINT
           AS current_epoch_opportunity_count
       , COALESCE(unlock.current_epoch_principal_usd_micros, 0)::BIGINT
           AS current_epoch_principal_usd_micros
       , COALESCE(unlock.current_epoch_recoverable_yield_usd_micros_per_hour, 0)::BIGINT
           AS current_epoch_recoverable_yield_usd_micros_per_hour
       , COALESCE(unlock.current_epoch_submitted_within_10s_yield_ppm, 0)::BIGINT
           AS current_epoch_submitted_within_10s_yield_ppm
       , COALESCE(unlock.current_epoch_submitted_within_2m_yield_ppm, 0)::BIGINT
           AS current_epoch_submitted_within_2m_yield_ppm
       , COALESCE(unlock.current_epoch_submitted_within_10m_yield_ppm, 0)::BIGINT
           AS current_epoch_submitted_within_10m_yield_ppm
       , COALESCE(unlock.current_epoch_confirmed_within_30s_yield_ppm, 0)::BIGINT
           AS current_epoch_confirmed_within_30s_yield_ppm
       , unlock.current_epoch_submission_p95_milliseconds
       , unlock.current_epoch_confirmation_p95_milliseconds
       , COALESCE(unlock.current_epoch_compiled_fee_lamports, 0)::BIGINT
           AS current_epoch_compiled_fee_lamports
FROM clusters cluster
LEFT JOIN loyal_yield.fleet_planning_clusters planning_cluster USING (cluster)
LEFT JOIN loyal_yield.fleet_planning_state planning USING (cluster)
LEFT JOIN latest_market_epoch latest_epoch USING (cluster)
LEFT JOIN opportunity_status opportunity USING (cluster)
LEFT JOIN queue_status queue USING (cluster)
LEFT JOIN outbox_status outbox USING (cluster)
LEFT JOIN submission_status submission USING (cluster)
LEFT JOIN current_epoch_unlock unlock USING (cluster);
