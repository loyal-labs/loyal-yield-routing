-- Allow a fused same-mint queue handoff to bind ordinary positive reserve
-- accrual observed by its final chain read. The source snapshot, collateral
-- shares, route identity, economics, signed bytes, and execute fence remain
-- exact; only the redeemable-liquidity amount may increase by at most 100 ppm.

CREATE OR REPLACE FUNCTION loyal_yield.link_rebalance_decision_to_execute_opportunity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    leased_opportunity loyal_yield.rebalance_opportunities%ROWTYPE;
    linked_submission_count INTEGER;
    amount_matches BOOLEAN;
BEGIN
    SELECT opportunity.*
    INTO leased_opportunity
    FROM loyal_yield.rebalance_opportunities opportunity
    JOIN loyal_yield.optimizer_epochs epoch
      ON epoch.id = opportunity.optimizer_epoch_id
     AND epoch.cluster = opportunity.cluster
    WHERE opportunity.vault_id = NEW.vault_id
      AND opportunity.opportunity_state = 'leased'
      AND opportunity.lease_kind = 'execute'
      AND opportunity.lease_expires_at > clock_timestamp()
      AND opportunity.expires_at > clock_timestamp()
      AND epoch.expires_at > clock_timestamp()
    FOR UPDATE OF opportunity;

    IF NOT FOUND THEN
        RETURN NEW;
    END IF;

    amount_matches := NEW.amount_raw IS NOT DISTINCT FROM leased_opportunity.amount_raw
        OR (
            leased_opportunity.execution_plan->>'kind' = 'same_mint'
            AND NEW.amount_raw IS NOT NULL
            AND NEW.amount_raw > leased_opportunity.amount_raw
            AND (NEW.amount_raw - leased_opportunity.amount_raw)::NUMERIC * 1000000
                <= leased_opportunity.amount_raw::NUMERIC * 100
        );

    IF NEW.source_snapshot_id IS DISTINCT FROM leased_opportunity.source_snapshot_id
       OR NEW.source_reserve IS DISTINCT FROM leased_opportunity.source_reserve
       OR NEW.target_reserve IS DISTINCT FROM leased_opportunity.target_reserve
       OR NEW.liquidity_mint IS DISTINCT FROM leased_opportunity.liquidity_mint
       OR NOT amount_matches
       OR NEW.source_apy_bps IS DISTINCT FROM leased_opportunity.source_apy_bps
       OR NEW.target_apy_bps IS DISTINCT FROM leased_opportunity.target_apy_bps
       OR NEW.estimated_edge_bps IS DISTINCT FROM leased_opportunity.estimated_edge_bps
       OR NEW.estimated_cost_lamports IS DISTINCT FROM leased_opportunity.estimated_cost_lamports
       OR NEW.execution_plan->>'kind'
            IS DISTINCT FROM leased_opportunity.execution_plan->>'kind'
       OR (
            leased_opportunity.execution_plan->>'kind' = 'same_mint'
            AND NEW.execution_plan->>'route_amount_semantics'
                IS DISTINCT FROM leased_opportunity.execution_plan->>'route_amount_semantics'
       )
    THEN
        RAISE EXCEPTION 'rebalance decision does not match the leased execute opportunity';
    END IF;

    UPDATE loyal_yield.rebalance_opportunities
    SET opportunity_state = 'decision_created',
        decision_id = NEW.id,
        lease_kind = NULL,
        lease_owner = NULL,
        lease_expires_at = NULL,
        terminal_reason = NULL,
        updated_at = now()
    WHERE id = leased_opportunity.id;

    UPDATE loyal_yield.signed_route_submissions
    SET decision_id = NEW.id,
        updated_at = now()
    WHERE opportunity_id = leased_opportunity.id
      AND executor_fencing_token = leased_opportunity.fencing_token
      AND submission_state IN ('signed', 'submitted')
      AND decision_id IS NULL;

    GET DIAGNOSTICS linked_submission_count = ROW_COUNT;
    IF linked_submission_count <> 1 THEN
        RAISE EXCEPTION
            'leased execute opportunity must have exactly one persisted signed submission for its fence';
    END IF;

    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION loyal_yield.link_rebalance_decision_to_execute_opportunity() IS
    'Atomically links one signed execute handoff; same-mint decisions may bind at most 100 ppm positive redeemable-liquidity accrual while every other route field remains exact.';
