package backyard

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// A manual or ambiguous signed fee operation can already have changed both
// the published price and treasury custody. Its predecessor must not be reused.
const unresolvedFeeSQL = `EXISTS (SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1
	AND status='manual_recovery' AND signed_wire IS NOT NULL AND (action='PAY_PERFORMANCE_FEE' OR expected_effects #> '{expectedEffects,performanceFee}' IS NOT NULL))`

func checkFeeCheckpoint(ctx context.Context, tx pgx.Tx, route string, e *performanceFeeEntry) error {
	if route != looping.RouteKey || e == nil || !e.valid() {
		return fmt.Errorf("invalid Looping fee journal entry")
	}
	var raw []byte
	var unresolved bool
	if err := tx.QueryRow(ctx, `SELECT state->'performanceFee',`+unresolvedFeeSQL+` FROM loyal_yield.multiply_route_states WHERE route_key=$1 FOR UPDATE`, route).Scan(&raw, &unresolved); err != nil {
		return err
	}
	var current performanceFeeState
	if unresolved || json.Unmarshal(raw, &current) != nil || current != e.Before {
		return staleHold("looping_fee_checkpoint_changed")
	}
	if r := e.Resolution; r.Signature != "" {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM `+resolutionTable+` WHERE signature=$1 AND finalized_slot=$2 AND principal_usdc_raw=$3 AND effective_lp_raw=$4 AND applied_operation_id IS NULL)`, r.Signature, r.Slot, r.Principal, r.Supply).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return staleHold("looping_fee_resolution_changed")
		}
	}
	return nil
}

func saveFeeCheckpoint(ctx context.Context, tx pgx.Tx, id string, e *performanceFeeEntry) error {
	var route string
	var planned []byte
	var action Action
	if err := tx.QueryRow(ctx, `SELECT route_key,action,expected_effects #> '{expectedEffects,performanceFee}' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&route, &action, &planned); err != nil {
		return err
	}
	var intent performanceFeeEntry
	if json.Unmarshal(planned, &intent) != nil || intent != *e || (action == PayPerformanceFee) != (e.Paid > 0) {
		return fmt.Errorf("finalized Looping fee differs from its journal")
	}
	if err := checkFeeCheckpoint(ctx, tx, route, e); err != nil {
		return err
	}
	if r := e.Resolution; r.Signature != "" {
		tag, err := tx.Exec(ctx, `UPDATE `+resolutionTable+` SET applied_operation_id=$2 WHERE signature=$1 AND applied_operation_id IS NULL`, r.Signature, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return staleHold("looping_fee_resolution_changed")
		}
	}
	raw, err := json.Marshal(e.After)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{performanceFee}',$2::jsonb),updated_at=clock_timestamp() WHERE route_key=$1`, route, string(raw))
	return err
}
