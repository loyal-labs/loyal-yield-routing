package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Renewal observes the same source again before replacing an expired interest
// envelope. It never adds exit spending or makes a new destination choice.
// Renewal authority resolves through the explicit reviewed manifest, the same
// way the commit, decode and merge paths already do: the candidate AUTO
// source renews exactly while that manifest's reviewed binding resolves.
func (d *Database) refreshSelectorUnwind(ctx context.Context, rpc *RPCClient, manifest RouteManifest, observe func(context.Context) (Observation, error)) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var version int64
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT state_version,state->'selectorUnwind' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, productionRouteKey).Scan(&version, &raw); err != nil {
		return err
	}
	var previous UnwindIntent
	if json.Unmarshal(raw, &previous) != nil || manifest.validateUnwindIntent(previous) != nil {
		return budgetHold("unwind_refresh_intent_unavailable")
	}
	o, err := observe(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errConfirmedObservationUnavailable, err)
	}
	s := o.Snapshot
	if !s.PilotActive || !s.Unwind || !s.UnwindRefreshRequired || s.RouteLane != previous.SourceLane || s.PositionCollateralRaw > previous.MaxCollateralRaw || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission {
		return budgetHold("unwind_refresh_source_changed")
	}
	// These flags select the exit forecast only. Original observation remains
	// intact for locked acceptance; actual withdrawal demand is never rewritten.
	forecast := o
	forecast.Snapshot.Unwind = false
	forecast.Snapshot.UnwindRefreshRequired = false
	forecast.Snapshot.WithdrawalDemandRaw = 0
	source, err := observeSelectorSource(ctx, rpc, productionJupiterClient(), manifest, forecast)
	if err != nil {
		return fmt.Errorf("%w: %w", errConfirmedObservationUnavailable, err)
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errConfirmedObservationUnavailable, err)
	}
	return d.renewSelectorUnwindOnManifest(ctx, manifest, productionRouteKey, version, previous, o, source, slot)
}

// renewSelectorUnwind keeps the pre-manifest renewal signature for legacy
// callers and tests: renewal authority resolves through the installed
// embedded manifest, exactly as the embedded entry fence does.
func (d *Database) renewSelectorUnwind(ctx context.Context, routeKey string, expectedVersion int64, previous UnwindIntent, o Observation, source selectorSourceQuote, confirmedSlot int64) error {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return err
	}
	return d.renewSelectorUnwindOnManifest(ctx, manifest, routeKey, expectedVersion, previous, o, source, confirmedSlot)
}

func (d *Database) renewSelectorUnwindOnManifest(ctx context.Context, manifest RouteManifest, routeKey string, expectedVersion int64, previous UnwindIntent, o Observation, source selectorSourceQuote, confirmedSlot int64) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	s := o.Snapshot
	floor := s.Slot
	if len(source.Recipe.Costs) == 0 {
		return budgetHold("unwind_refresh_evidence_unavailable")
	}
	for _, cost := range source.Recipe.Costs {
		if cost.ObservationSlot < s.Slot || cost.TotalMicros <= 0 {
			return budgetHold("unwind_refresh_evidence_unavailable")
		}
		floor = max(floor, cost.ObservationSlot)
	}
	current := func() bool {
		return freshAt(time.Now().UTC(), o.ObservedAt, 30*time.Second) && s.Slot > 0 && confirmedSlot >= floor && confirmedSlot <= source.Recipe.ValidThroughSlot && source.Recipe.ValidThroughSlot-s.Slot <= budgetMaxObservationLagSlots
	}
	if manifest.validateUnwindIntent(previous) != nil || !current() || !s.Fresh || !s.PilotActive || !s.Unwind || !s.UnwindRefreshRequired || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.CutoverDrain || s.RouteLane != previous.SourceLane || source.Lane != s.RouteLane || source.ObservationID != s.ObservationID || s.ObservationID == "" || source.ExitBound == nil || !sha256Pattern.MatchString(source.Recipe.EvidenceID) || s.PositionCollateralRaw < 0 || s.PositionCollateralRaw > previous.MaxCollateralRaw || source.ExitBound.MaxCollateralRaw != s.PositionCollateralRaw || s.PositionDebtRaw <= previous.MaxDebtRaw || source.ExitBound.MaxDebtRaw < s.PositionDebtRaw {
		return budgetHold("unwind_refresh_evidence_unavailable")
	}
	next := previous
	next.ObservationID, next.EvidenceID, next.CreatedAt = s.ObservationID, source.Recipe.EvidenceID, time.Now().UTC()
	next.MaxCollateralRaw, next.MaxDebtRaw, next.CostBoundRaw = source.ExitBound.MaxCollateralRaw, source.ExitBound.MaxDebtRaw, source.ExitBound.GrossMicros
	if err := manifest.validateUnwindIntent(next); err != nil {
		return err
	}
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	if lease.RouteKey != routeKey {
		return fmt.Errorf("unwind_route_lease_mismatch")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout = '25ms'`); err != nil {
		return err
	}
	var version int64
	var raw []byte
	if err = tx.QueryRow(ctx, RouteStateForUpdate, routeKey, lease.Owner, lease.FencingToken).Scan(&version, &raw); err != nil {
		return err
	}
	if version != expectedVersion {
		return budgetHold("unwind_refresh_state_changed")
	}
	var state struct {
		Budget     Phase3Budget    `json:"phase3"`
		Activation json.RawMessage `json:"pilotBudgetActivation"`
		Unwind     *UnwindIntent   `json:"selectorUnwind"`
	}
	if json.Unmarshal(raw, &state) != nil || state.Unwind == nil || !sameUnwindIntent(*state.Unwind, previous) {
		return budgetHold("unwind_refresh_intent_changed")
	}
	if err = state.Budget.validate(); err != nil {
		return err
	}
	if state.Budget.Pilot == nil {
		return budgetHold("unwind_refresh_requires_pilot")
	}
	if _, err = validatePersistedPilotActivation(state.Budget, state.Activation, version); err != nil {
		return err
	}
	if state.Budget.Closed || state.Budget.GoalID != next.BudgetScope || len(state.Budget.Reservations) != 0 || state.Budget.Families[next.BudgetFamily].ExitMicros < next.CostBoundRaw {
		return budgetHold("unwind_requires_existing_exit_reservation")
	}
	var blocked bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`)) OR EXISTS(SELECT 1 FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key=$1 AND cleared_at IS NULL) OR EXISTS (`+manualRecoveryDerivedLatchSQL+`)`, routeKey).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return budgetHold("unwind_refresh_recovery_first")
	}
	if err = tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, routeKey).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return budgetHold("unwind_refresh_recovery_first")
	}
	if !current() {
		return budgetHold("unwind_refresh_evidence_expired")
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return d.writeUnwindTx(ctx, tx, routeKey, version, encoded)
}
