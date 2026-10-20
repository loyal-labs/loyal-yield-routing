package backyard

import (
	"context"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// initializationSnapshotReady reports a flat lane whose obligation is known
// absent and is created through an installed initializer policy, while the
// target enters it: the Voltr idle a current selector quote allocates and the
// vault's Squads cash (a rotation's release), all of which the lane takes. A
// lane whose obligation pre-exists has no initializer: its absence holds.
func initializationSnapshotReady(s Snapshot) bool {
	if s.ObservationID == "" || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || s.RouteKind != RouteKind || !s.Fresh {
		return false
	}
	if !earnInitializerLane(s.RouteLane) || !s.ObligationPresenceKnown || s.ObligationPresent {
		return false
	}
	if s.HasPosition || s.PositionCollateralValueRaw != 0 || s.PositionDebtValueRaw != 0 || s.CollateralIdleValueRaw != 0 || s.PositionCollateralRaw != 0 || s.PositionDebtRaw != 0 || s.CollateralIdleRaw != 0 || s.VoltrStrategyIdleRaw != 0 {
		return false
	}
	if s.Nonterminal != "" || s.HasAmbiguousSubmission || s.ManualReason != "" || withdrawalShortfallRaw(s) != 0 {
		return false
	}
	t := s.Target
	funding := t.EntryRaw + s.SquadsIdleRaw
	return t.Rule == ruleEntry && t.EntryRaw >= 0 && funding > 0 && t.EntryRaw <= s.VoltrIdleRaw && funding <= min(s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw) && s.CapacityRaw > 0 && s.PolicyLimitRaw > 0 && s.MaxTargetLTVEntryRaw > 0 && hardLTVBPS(s.LiquidationThresholdBPS, s.MaxLTVBPS) > laneCeilingLTVBPS(s.RouteLane)
}

// This prepares only an empty account before allocating user principal: the
// initializer the tick decided, from the tick's own observation.
func prepareKaminoInitialization(ctx context.Context, rpc *chain.Client, manifest RouteManifest, decision Decision, o Observation) (Observation, KaminoInitializationRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if rpc == nil || decision.Action != InitializeKaminoObligation || decision.Validate() != nil {
		return o, KaminoInitializationRequest{}, budgetHold("invalid_initializer_preparation")
	}
	rent, err := rpc.RentExempt(ctx, kamino.ObligationSize)
	if err != nil {
		return o, KaminoInitializationRequest{}, budgetHoldFrom(err, "initializer_rent_unavailable")
	}
	blockhash, err := latestBlockhash(ctx, rpc)
	if err != nil {
		return o, KaminoInitializationRequest{}, err
	}
	// MaximumFeeLamports is not part of the wire; one is a compile-only
	// placeholder, replaced by the RPC fee for these exact message bytes.
	if o.policies, err = observeInstalledPolicies(ctx, rpc, manifest.vault, o.Snapshot.Slot); err != nil {
		return o, KaminoInitializationRequest{}, err
	}
	r, err := manifest.initializationRequest(o.policies, decision.StrategyKey, blockhash, rent, 1)
	if err != nil {
		return o, r, err
	}
	message, err := manifest.compileKaminoInitializationMessage(r)
	if err != nil {
		return o, r, err
	}
	r.MaximumFeeLamports, err = chain.MessageFee(message)
	if err != nil {
		return o, r, budgetHoldFrom(err, "message_fee_unpriced")
	}
	return o, r, nil
}
