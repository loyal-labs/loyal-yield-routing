CREATE OR REPLACE FUNCTION loyal_yield.verify_cross_mint_no_effect_receipt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    submission_decision_id BIGINT;
    submission_movement_leg TEXT;
    submission_leg_generation BIGINT;
    submission_transaction_signature TEXT;
    submission_expected_balance_anchors JSONB;
    submission_last_valid_block_height BIGINT;
    submission_broadcast_count INTEGER;
    submission_state TEXT;
    submission_expiry_observed_block_height BIGINT;
    submission_effect_check_slot BIGINT;
    movement_route TEXT;
BEGIN
    SELECT route.decision_id, route.movement_leg, route.leg_generation,
           route.transaction_signature, route.expected_balance_anchors,
           route.last_valid_block_height, route.broadcast_count,
           route.submission_state, route.expiry_observed_block_height,
           route.effect_check_slot, decision.movement_route
    INTO submission_decision_id, submission_movement_leg,
         submission_leg_generation, submission_transaction_signature,
         submission_expected_balance_anchors,
         submission_last_valid_block_height, submission_broadcast_count,
         submission_state, submission_expiry_observed_block_height,
         submission_effect_check_slot, movement_route
    FROM loyal_yield.signed_route_submissions route
    JOIN loyal_yield.rebalance_decisions decision
      ON decision.id = route.decision_id
    WHERE route.id = NEW.submission_id
    FOR KEY SHARE OF route, decision;

    IF NOT FOUND
       OR movement_route <> 'cross_mint_jupiter'
       OR submission_decision_id IS DISTINCT FROM NEW.decision_id
       OR submission_movement_leg IS DISTINCT FROM NEW.movement_leg
       OR submission_leg_generation IS DISTINCT FROM NEW.leg_generation
       OR submission_transaction_signature IS DISTINCT FROM NEW.transaction_signature
       OR submission_expected_balance_anchors IS DISTINCT FROM NEW.expected_balance_anchors
       OR submission_last_valid_block_height >= NEW.observed_block_height
       OR NOT (
            (
                submission_broadcast_count = 0
                AND submission_state IN ('signed', 'submitted')
            )
            OR (
                submission_state IN (
                    'expiry_check_pending', 'effect_ambiguous'
                )
                AND submission_expiry_observed_block_height
                    IS NOT DISTINCT FROM NEW.observed_block_height
                AND submission_effect_check_slot
                    IS NOT DISTINCT FROM NEW.effect_check_slot
            )
       )
    THEN
        RAISE EXCEPTION
            'cross-mint no-effect receipt is stale or does not bind the exact expired submission';
    END IF;
    RETURN NEW;
END;
$$;
