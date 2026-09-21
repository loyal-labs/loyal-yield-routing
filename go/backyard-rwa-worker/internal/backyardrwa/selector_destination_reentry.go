package backyardrwa

import (
	"context"
	"math"
	"time"
)

// A reentry forecast carries the completed source exit that precedes this
// entry inside one economic quote. It binds the currently funded same-lane
// position and custody residue to that exit's exact bounds. It authorizes no
// transaction, mutates no observed account, and creates no reservation.
type selectorReentryForecast struct {
	bound          selectorExitBound
	collateralIdle uint64
}

// Forecast-only same-lane reentry pricing. The ordinary destination quote
// refuses a funded obligation and collateral residue as not flat; this
// entrypoint prices the full unwind/recreate loop instead: the validated
// source exit is forecast to close the funded obligation, so the destination
// recipe always includes obligation-recreation rent and the exact initializer
// fee. Every flat execution admission/build/send prerequisite stays enforced
// where it already lives; nothing here relaxes one.
func observeSelectorReentryDestinationSize(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, source selectorSourceQuote, maximum uint64, clampCapacity bool) (selectorDestinationQuote, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s := o.Snapshot
	out := selectorDestinationQuote{Lane: s.RouteLane, EquityRaw: maximum}
	// Reentry prices a NEW funded allocation for the route lane, so the lane
	// must carry this manifest's funding authority — installed entry lanes
	// plus the candidate AUTO lane only under its reviewed binding — not
	// merely the broader decode/source-evidence authority that keeps deferred
	// installed lanes observable.
	if rpc == nil || client == nil || o.ObservedAt.IsZero() || !freshAt(time.Now().UTC(), o.ObservedAt, 30*time.Second) ||
		!s.PilotActive || !s.Fresh || !m.selectorEntryFundingLane(s.RouteLane, false) || s.RouteLane != s.StrategyKey ||
		s.ObservationID == "" || s.DebtIdleRaw != 0 || s.CollateralIdleRaw < 0 || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagSlots {
		return out, budgetHold("selector_reentry_destination_unavailable")
	}
	// Only a completed funded route observation — the lane's obligation present
	// with a positive position and settled NAV — may forecast reentry of its
	// own position. Flat or incomplete states belong to the ordinary paths.
	if !s.ObligationPresenceKnown || !s.ObligationPresent || !s.HasPosition || s.PositionCollateralRaw <= 0 || s.StrategyNAVRaw <= 0 {
		return out, budgetHold("selector_reentry_destination_unavailable")
	}
	// The exit bound is the only permit for a currently funded destination: it
	// must be the same lane's complete source quote from this exact observation
	// and must describe exactly the position that observation completed with.
	if source.Lane != s.RouteLane || source.ObservationID != s.ObservationID || source.ExitBound == nil ||
		source.ExitBound.MaxCollateralRaw < 0 || source.ExitBound.MaxDebtRaw < 0 ||
		source.ExitBound.MaxCollateralRaw != s.PositionCollateralRaw || source.ExitBound.MaxDebtRaw < s.PositionDebtRaw ||
		!sha256Pattern.MatchString(source.Recipe.EvidenceID) ||
		source.Recipe.ValidThroughSlot < s.Slot || source.Recipe.ValidThroughSlot-s.Slot > budgetMaxObservationLagSlots {
		return out, budgetHold("selector_reentry_exit_bound_unavailable")
	}
	if maximum == 0 || maximum > uint64(PilotWorkingTrancheCapRaw) || maximum > source.MinimumIdleRaw {
		return out, budgetHold("selector_reentry_equity_unavailable")
	}
	reentry := selectorReentryForecast{bound: *source.ExitBound, collateralIdle: uint64(s.CollateralIdleRaw)}
	// Installed lanes keep the public wrapper's selectorLane gate unchanged.
	// The candidate AUTO route lane dispatches to the shared authorized body
	// only after this function's funding predicate and exact source-exit
	// validation above both passed: the authorized form re-checks the same
	// reviewed binding and runs the identical entry graph with this forecast's
	// clampCapacity and reentry contract. An absent or drifted binding already
	// failed closed at the funding gate.
	if s.RouteLane == autoAUTOPYUSD.Lane {
		return observeSelectorDestinationForecastAuthorized(ctx, rpc, client, m, s.RouteLane, maximum, s.Slot, clampCapacity, &reentry)
	}
	return observeSelectorDestinationForecast(ctx, rpc, client, m, s.RouteLane, maximum, s.Slot, clampCapacity, &reentry)
}

// The reentry destination observes the actual funded lane batch and binds it
// to the source exit: exactly the bound collateral, debt within the payoff
// upper, the obligation present whenever the exit closes one, and no custody
// residue beyond the idle amount that exit swaps back. An already-flat
// destination belongs to the ordinary quote, not this forecast.
func selectorReentryDestinationAccounts(ctx context.Context, rpc *RPCClient, m RouteManifest, route RuntimeRoute, minimumSlot int64, bound selectorExitBound, collateralIdle uint64) (int64, []ConfirmedAccount, KaminoPosition, error) {
	var empty KaminoPosition
	slot, accounts, position, err := observeSelectorDestinationBatch(ctx, rpc, m, route, minimumSlot)
	if err != nil {
		return 0, nil, empty, err
	}
	custody, err := validateSelectorDestinationCommon(m, route, slot, accounts, position)
	if err != nil {
		return 0, nil, empty, err
	}
	if int64(position.CollateralDepositedRaw) != bound.MaxCollateralRaw ||
		position.DebtRaw > uint64(bound.MaxDebtRaw) ||
		(bound.MaxCollateralRaw > 0 || bound.MaxDebtRaw > 0) && !position.ObligationPresent ||
		custody != collateralIdle {
		return 0, nil, empty, budgetHold("selector_reentry_position_drifted")
	}
	if position.CollateralDepositedRaw == 0 && position.DebtRaw == 0 && custody == 0 {
		return 0, nil, empty, budgetHold("selector_reentry_destination_already_flat")
	}
	return slot, accounts, position, nil
}
