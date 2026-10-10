package backyard

import (
	"context"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// initializationSnapshotReady reports a flat, entry-authorized lane whose
// obligation is known absent and is created through an installed initializer
// policy. A lane whose obligation pre-exists has no initializer: its absence
// holds.
func initializationSnapshotReady(s Snapshot) bool {
	if s.ObservationID == "" || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || s.RouteKind != RouteKind || !s.Fresh {
		return false
	}
	if !earnInitializerLane(s.RouteLane) || !s.ObligationPresenceKnown || s.ObligationPresent {
		return false
	}
	if s.HasPosition || s.PositionCollateralValueRaw != 0 || s.PositionDebtValueRaw != 0 || s.CollateralIdleValueRaw != 0 || s.StrategyNAVRaw != 0 || s.PositionCollateralRaw != 0 || s.PositionDebtRaw != 0 || s.CollateralIdleRaw != 0 || s.DebtIdleRaw != 0 || s.SquadsIdleRaw != 0 || s.VoltrStrategyIdleRaw != 0 {
		return false
	}
	if s.Nonterminal != "" || s.HasAmbiguousSubmission || s.ManualReason != "" || s.Unwind || s.CutoverDrain || s.SelectorEntryPaused || s.WithdrawalDemandRaw != 0 {
		return false
	}
	return s.SelectorEntryEquityRaw > 0 && s.SelectorEntryEquityRaw <= min(s.VoltrIdleRaw, s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw) && s.CapacityRaw > 0 && s.PolicyLimitRaw > 0 && s.MaxTargetLTVEntryRaw > 0 && min(s.LiquidationThresholdBPS-1500, 6000) > TargetLTVBPS
}

// This prepares only an empty account before allocating user principal.
func prepareKaminoInitialization(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, decision Decision, observe func(context.Context) (Observation, error)) (Observation, KaminoInitializationRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if rpc == nil || observe == nil || decision.Action != InitializeKaminoObligation || decision.Validate() != nil {
		return Observation{}, KaminoInitializationRequest{}, budgetHold("invalid_initializer_preparation")
	}
	o, err := observe(ctx)
	if err != nil {
		return o, KaminoInitializationRequest{}, err
	}
	if !decisionsEqual(Decide(o.Snapshot), decision) || !initializationSnapshotReady(o.Snapshot) {
		return o, KaminoInitializationRequest{}, budgetHold("initializer_decision_changed")
	}
	if err = manifest.validateBindings(); err != nil {
		return o, KaminoInitializationRequest{}, err
	}
	rent, err := rpc.RentExempt(ctx, kamino.ObligationSize)
	if err != nil {
		return o, KaminoInitializationRequest{}, budgetHold("initializer_rent_unavailable")
	}
	blockhash, err := latestBlockhash(ctx, rpc)
	if err != nil {
		return o, KaminoInitializationRequest{}, err
	}
	// MaximumFeeLamports is not part of the wire; one is a compile-only
	// placeholder, replaced by the RPC fee for these exact message bytes.
	if o.policies, err = observeInstalledPolicies(ctx, rpc, o.Snapshot.Slot); err != nil {
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
	fee, err := observeMessageFee(ctx, rpc, message, o.Snapshot.Slot)
	if err != nil {
		return o, r, err
	}
	r.MaximumFeeLamports = fee.Lamports
	if _, err = manifest.validateKaminoInitializationPrestate(ctx, view, r, o.Snapshot.Slot); err != nil {
		return o, r, err
	}
	return o, r, nil
}
