package backyard

import (
	"context"
	"encoding/json"
	"math"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// A flat vault's source is its cash: the selector only enters a lane with a
// flat vault (held lanes keep their capital), so the source quote prices no
// exit. It reserves no budget and asserts no prospective balance.
type selectorSourceQuote struct {
	Lane           string             `json:"lane"`
	ObservationID  string             `json:"observationId"`
	MinimumIdleRaw uint64             `json:"minimumIdleRaw"`
	Recipe         selectorRecipe     `json:"recipe"`
	ExitBound      *selectorExitBound `json:"exitBound,omitempty"`
}

func observeSelectorSource(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, o Observation) (selectorSourceQuote, error) {
	return observeReviewedSelectorSource(ctx, rpc, view, client, m, o, false)
}

// observeAutoSelectorSource is the source producer for the catalog
// (non-USDC debt) lanes, through an explicit reviewed manifest.
func observeAutoSelectorSource(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, o Observation) (selectorSourceQuote, error) {
	return observeReviewedSelectorSource(ctx, rpc, view, client, m, o, true)
}

// selectorSourceLaneAuthorized admits a basic registry lane through the plain
// producer, and a catalog registry lane (non-USDC debt) only through the
// candidate producer over a manifest whose activation selects that lane.
func selectorSourceLaneAuthorized(m RouteManifest, lane string, candidate bool) bool {
	if basicLane(lane) {
		return true
	}
	if !candidate || !catalogJupiterRoute(lane) {
		return false
	}
	active, err := m.activeRuntimeRoute()
	return err == nil && active.Lane == lane
}

// observeReviewedSelectorSource is a flat vault's cash, Voltr idle and the
// Squads cash an entry already allocated: the guaranteed idle a move enters
// with. A vault that holds a lane has no selector source.
func observeReviewedSelectorSource(_ context.Context, rpc *chain.Client, _ *View, client *jupiter.Client, m RouteManifest, o Observation, candidate bool) (selectorSourceQuote, error) {
	s := o.Snapshot
	out := selectorSourceQuote{Lane: s.RouteLane, ObservationID: s.ObservationID}
	if rpc == nil || client == nil || !selectorSourceLaneAuthorized(m, s.RouteLane, candidate) || s.RouteLane != s.StrategyKey || !s.Fresh || s.ObservationID == "" || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.ManualReason != "" || s.WithdrawalDemandRaw != 0 || s.VoltrIdleRaw < 0 || s.VoltrStrategyIdleRaw != 0 || s.SquadsIdleRaw < 0 || laneExposure(s) {
		return out, budgetHold("selector_source_unavailable")
	}
	out.MinimumIdleRaw = uint64(s.VoltrIdleRaw) + uint64(s.SquadsIdleRaw)
	out.Recipe.ValidThroughSlot = s.Slot + observationLagSlots()
	raw, err := json.Marshal(struct {
		Kind     string
		Snapshot Snapshot
	}{"OBSERVED_IDLE_NO_EXIT", s})
	if err != nil {
		return out, err
	}
	out.Recipe.EvidenceID = sha256Bytes(raw)
	return out, nil
}
