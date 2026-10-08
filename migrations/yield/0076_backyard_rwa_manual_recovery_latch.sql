-- A HOLD_MANUAL_RECOVERY decision is a durable route-level stop, not a one-tick
-- journal note. This row is what makes the stop survive restarts and healthy
-- batches: every worker tick re-reads it before observing, re-records the hold,
-- and executes nothing until an operator clears it with an explicit reason.
-- Nothing in the worker clears it automatically.

-- The clear command is a terminal journal fact, but it still uses the shared
-- multiply_operations table.  Migration 0074 has already validated these
-- constraints on existing databases, so extend both action vocabularies in a
-- follow-up migration rather than relying on a fresh-schema definition.
ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_action_check;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_action_check CHECK (action IN (
        'request_withdrawal', 'cancel_withdrawal', 'deposit_claim_asset',
        'swap_claim_to_collateral', 'deposit_collateral', 'borrow_debt',
        'swap_debt_to_collateral', 'withdraw_collateral', 'swap_collateral_to_debt',
        'repay_debt', 'withdraw_remaining_collateral', 'swap_collateral_to_claim', 'claim',
        'HOLD', 'RECOVER_TRANSACTION', 'VOLTR_ALLOCATE_TO_SQUADS',
        'SWAP_USDC_TO_PRIME_STEP', 'SWAP_PRIME_TO_USDC_STEP',
        'OPEN_PRIME_USDC_STEP', 'DELEVER_PRIME_USDC_STEP',
        'SWAP_STABLE_TO_COLLATERAL_STEP', 'SWAP_COLLATERAL_TO_STABLE_STEP',
        'SWAP_DEBT_TO_COLLATERAL_STEP', 'SWAP_COLLATERAL_TO_DEBT_STEP',
        'SWAP_USDC_TO_DEBT_STEP', 'SWAP_DEBT_TO_USDC_STEP',
        'OPEN_ROUTE_STEP', 'DELEVER_ROUTE_STEP',
        'STAGE_SQUADS_TO_VOLTR', 'VOLTR_RESTORE_IDLE', 'REPORT_NAV',
        'HOLD_MANUAL_RECOVERY', 'HOLD_CLEARED',
        'POLICY_SETUP_PREFUND', 'POLICY_SETUP_CREATE'
    )) NOT VALID;
ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_action_check;

ALTER TABLE loyal_yield.multiply_operations
    DROP CONSTRAINT IF EXISTS multiply_operations_backyard_action_scope;

ALTER TABLE loyal_yield.multiply_operations
    ADD CONSTRAINT multiply_operations_backyard_action_scope CHECK (
        (engine_version = 'backyard_rwa_v1') = (action IN (
            'HOLD', 'RECOVER_TRANSACTION', 'VOLTR_ALLOCATE_TO_SQUADS',
            'SWAP_USDC_TO_PRIME_STEP', 'SWAP_PRIME_TO_USDC_STEP',
            'OPEN_PRIME_USDC_STEP', 'DELEVER_PRIME_USDC_STEP',
            'SWAP_STABLE_TO_COLLATERAL_STEP', 'SWAP_COLLATERAL_TO_STABLE_STEP',
            'SWAP_DEBT_TO_COLLATERAL_STEP', 'SWAP_COLLATERAL_TO_DEBT_STEP',
            'SWAP_USDC_TO_DEBT_STEP', 'SWAP_DEBT_TO_USDC_STEP',
            'OPEN_ROUTE_STEP', 'DELEVER_ROUTE_STEP',
            'STAGE_SQUADS_TO_VOLTR', 'VOLTR_RESTORE_IDLE', 'REPORT_NAV',
            'HOLD_MANUAL_RECOVERY', 'HOLD_CLEARED',
            'POLICY_SETUP_PREFUND', 'POLICY_SETUP_CREATE'
        ))
    ) NOT VALID;
ALTER TABLE loyal_yield.multiply_operations
    VALIDATE CONSTRAINT multiply_operations_backyard_action_scope;

CREATE TABLE IF NOT EXISTS loyal_yield.backyard_manual_recovery_latches (
    route_key text PRIMARY KEY REFERENCES loyal_yield.multiply_route_states(route_key),
    reason text NOT NULL,
    observation_id text NOT NULL,
    observation_slot bigint NOT NULL,
    latched_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    cleared_at timestamptz,
    cleared_reason text,
    CHECK ((cleared_at IS NULL) = (cleared_reason IS NULL))
);

-- Backfill the durable stop for terminal manual-recovery decisions that were
-- recorded before this latch existed. A later HOLD_CLEARED is the only fact
-- that authorizes resumption; operation_id breaks ties for rows sharing a
-- timestamp. Re-running the migration preserves an active latch and may only
-- re-arm a row that was already cleared.
INSERT INTO loyal_yield.backyard_manual_recovery_latches
    (route_key, reason, observation_id, observation_slot, latched_at)
SELECT pending.route_key,
       COALESCE(
           NULLIF(pending.recovery_reason, ''),
           NULLIF(pending.expected_effects -> 'decision' ->> 'reason', ''),
           'legacy_manual_recovery'
       ),
       COALESCE(
           NULLIF(pending.expected_effects -> 'decision' ->> 'observationId', ''),
           pending.operation_id
       ),
       GREATEST(
           COALESCE(
               CASE
                   WHEN pending.expected_effects -> 'decision' ->> 'observationSlot' ~ '^[0-9]+$'
                   THEN (pending.expected_effects -> 'decision' ->> 'observationSlot')::bigint
               END,
               CASE WHEN pending.confirmed_slot > 0 THEN pending.confirmed_slot END,
               1
           ),
           1
       ),
       pending.updated_at
FROM (
    SELECT DISTINCT ON (hold.route_key)
           hold.route_key,
           hold.operation_id,
           hold.recovery_reason,
           hold.expected_effects,
           hold.confirmed_slot,
           hold.updated_at
    FROM loyal_yield.multiply_operations AS hold
    WHERE hold.action = 'HOLD_MANUAL_RECOVERY'
      AND hold.status = 'manual_recovery'
      AND NOT EXISTS (
          SELECT 1
          FROM loyal_yield.multiply_operations AS cleared
          WHERE cleared.route_key = hold.route_key
            AND cleared.action = 'HOLD_CLEARED'
            AND (cleared.updated_at, cleared.operation_id) > (hold.updated_at, hold.operation_id)
      )
    ORDER BY hold.route_key, hold.updated_at DESC, hold.operation_id DESC
) AS pending
ON CONFLICT (route_key) DO UPDATE
SET reason = EXCLUDED.reason,
    observation_id = EXCLUDED.observation_id,
    observation_slot = EXCLUDED.observation_slot,
    latched_at = EXCLUDED.latched_at,
    cleared_at = NULL,
    cleared_reason = NULL
WHERE loyal_yield.backyard_manual_recovery_latches.cleared_at IS NOT NULL;
