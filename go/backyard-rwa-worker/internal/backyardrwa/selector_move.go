package backyardrwa

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

// A complete move includes the existing source exit and destination entry.
// Its evidence authorizes no transaction; runtime still reserves and rebuilds
// each leg against actual balances. A destination is reselected after unwind.
func observeSelectorMove(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, lane string, requestedEquity, idleBuffer uint64) (MoveQuote, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var empty MoveQuote
	if o.ObservedAt.IsZero() || !freshAt(time.Now().UTC(), o.ObservedAt, 30*time.Second) || requestedEquity == 0 || requestedEquity > uint64(PilotWorkingTrancheCapRaw) {
		return empty, budgetHold("invalid_selector_move")
	}
	source, err := observeSelectorSource(ctx, rpc, client, m, o)
	if err != nil {
		return empty, err
	}
	if source.MinimumIdleRaw <= idleBuffer {
		return empty, budgetHold("selector_move_has_no_entry_cash")
	}
	equity := min(requestedEquity, source.MinimumIdleRaw-idleBuffer)
	if equity == 0 {
		return empty, budgetHold("selector_move_has_no_entry_cash")
	}
	destination, err := observeSelectorDestination(ctx, rpc, client, m, lane, equity, o.Snapshot.Slot)
	if err != nil {
		return empty, err
	}
	return composeSelectorMove(ctx, rpc, o, source, destination)
}

func composeSelectorMove(ctx context.Context, rpc *RPCClient, o Observation, source selectorSourceQuote, destination selectorDestinationQuote) (MoveQuote, error) {
	s := o.Snapshot
	q := MoveQuote{SourceLane: source.Lane, DestinationLane: destination.Lane, ObservationID: s.ObservationID, ObservedAt: o.ObservedAt, SampleSlot: s.Slot,
		BorrowReceiveRaw: destination.BorrowReceiveRaw, BorrowFeeRaw: destination.BorrowFeeRaw, MinimumIdleRaw: source.MinimumIdleRaw,
		ValidThroughSlot: min(source.Recipe.ValidThroughSlot, destination.Recipe.ValidThroughSlot)}
	if rpc == nil || !s.PilotActive || source.Lane != s.RouteLane || source.ObservationID != s.ObservationID || !selectorLane(destination.Lane) || destination.EquityRaw == 0 || destination.EquityRaw > uint64(PilotWorkingTrancheCapRaw) || destination.EquityRaw > source.MinimumIdleRaw || source.MinimumIdleRaw > math.MaxInt64 || !sha256Pattern.MatchString(source.Recipe.EvidenceID) || !sha256Pattern.MatchString(destination.Recipe.EvidenceID) || !q.currentAtSlot(s.Slot) || o.ObservedAt.IsZero() {
		return q, budgetHold("invalid_selector_move")
	}
	q.EquityRaw = int64(destination.EquityRaw)
	var err error
	q.CostRaw, err = budgetSum(source.Recipe.CostRaw, destination.Recipe.CostRaw)
	if err != nil || source.Recipe.CostRaw < 0 || destination.Recipe.CostRaw <= 0 || q.CostRaw >= q.EquityRaw || !q.validBorrow() {
		return q, budgetHold("selector_move_cost_exceeds_equity")
	}
	floor := max(s.Slot, destination.AccountSlot)
	for _, recipe := range []selectorRecipe{source.Recipe, destination.Recipe} {
		for _, cost := range recipe.Costs {
			floor = max(floor, cost.ObservationSlot)
		}
	}
	floor, err = selectorSwapObservationFloor(destination.PayoffSwap.Request, s.Slot, floor)
	if err != nil {
		return q, err
	}
	if floor > q.ValidThroughSlot {
		return q, budgetHold("selector_recipe_observation_expired")
	}
	// Fees debit the delegate; account setup debits the vault. Future rent
	// refunds are deliberately not spendable until the closing step reconciles.
	if source.Recipe.NetworkLamports > math.MaxUint64-destination.Recipe.NetworkLamports || source.Recipe.SetupLamports > math.MaxUint64-destination.Recipe.SetupLamports {
		return q, budgetHold("selector_move_native_overflow")
	}
	slot, accounts, err := rpc.GetMultipleAccounts(ctx, []string{bridgeDelegate, bridgeVault}, floor)
	if err != nil {
		return q, err
	}
	for _, funding := range []struct {
		address string
		amount  uint64
	}{{bridgeDelegate, source.Recipe.NetworkLamports + destination.Recipe.NetworkLamports}, {bridgeVault, source.Recipe.SetupLamports + destination.Recipe.SetupLamports}} {
		a := accountAt(accounts, funding.address)
		if a.Owner != "11111111111111111111111111111111" || a.Executable || len(a.Data) != 0 || a.Lamports < funding.amount {
			return q, budgetHold("selector_move_native_funding_unavailable")
		}
	}
	current, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return q, err
	}
	if current < slot || current > q.ValidThroughSlot || !freshAt(time.Now().UTC(), q.ObservedAt, 30*time.Second) {
		return q, budgetHold("selector_recipe_observation_expired")
	}
	raw, err := json.Marshal(struct {
		Quote                MoveQuote
		Source               selectorSourceQuote
		Destination          selectorDestinationQuote
		NativeSlot           int64
		NativeAccountsSHA256 string
	}{q, source, destination, slot, hashConfirmedAccounts(accounts)})
	if err != nil {
		return q, err
	}
	q.EvidenceID = sha256Bytes(raw)
	return q, nil
}
