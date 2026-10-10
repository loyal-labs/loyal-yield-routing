package backyard

import (
	"context"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

func initializationSnapshotReady(s Snapshot) bool {
	return snapshotInitializationReady(s, selectorLane)
}

// initializationSnapshotReadyOnManifest is the identical initializer snapshot
// readiness with the lane authority explicit: the candidate AUTO lane is
// admitted only while the manifest's reviewed binding resolves, and every
// freshness, flat-state, pause, unwind, withdrawal, capacity and LTV
// condition is shared verbatim with the installed form.
func (m RouteManifest) initializationSnapshotReady(s Snapshot) bool {
	return snapshotInitializationReady(s, m.selectorEntryLaneAllowed)
}

func snapshotInitializationReady(s Snapshot, laneAllowed func(string) bool) bool {
	if s.ObservationID == "" || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || s.RouteKind != RouteKind || !s.Fresh {
		return false
	}
	if !s.InitializationPolicyReady || !laneAllowed(s.RouteLane) || !s.ObligationPresenceKnown || s.ObligationPresent {
		return false
	}
	if s.HasPosition || s.PositionCollateralValueRaw != 0 || s.PositionDebtValueRaw != 0 || s.CollateralIdleValueRaw != 0 || s.StrategyNAVRaw != 0 || s.PositionCollateralRaw != 0 || s.PositionDebtRaw != 0 || s.CollateralIdleRaw != 0 || s.DebtIdleRaw != 0 || s.SquadsIdleRaw != 0 || s.VoltrStrategyIdleRaw != 0 {
		return false
	}
	if s.Nonterminal != "" || s.HasAmbiguousSubmission || s.ManualReason != "" || s.Unwind || s.CutoverDrain || s.SelectorEntryPaused || s.WithdrawalDemandRaw != 0 {
		return false
	}
	return s.SelectorEntryEquityRaw > 0 && s.SelectorEntryEquityRaw <= min(s.VoltrIdleRaw, s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw) && s.PolicyReady && s.ExitBuildable && s.CapacityRaw > 0 && s.PolicyLimitRaw > 0 && s.MaxTargetLTVEntryRaw > 0 && min(s.LiquidationThresholdBPS-1500, 6000) > TargetLTVBPS
}

// This prepares only an empty account before allocating user principal. Actual
// rent, installed policy, metadata, market and native funding are rechecked by
// the existing prestate gate at admission and immediately before signing/send.
func prepareKaminoInitialization(ctx context.Context, rpc *chain.Client, manifest RouteManifest, decision Decision, observe func(context.Context) (Observation, error)) (Observation, KaminoInitializationRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// The decision is validated through the same manifest authority that
	// produced it: the embedded form keeps every installed lane and refuses the
	// candidate outright, while the manifest form admits the reviewed AUTO
	// binding exactly as DecideOnManifest does.
	if rpc == nil || observe == nil || decision.Action != InitializeKaminoObligation || manifest.validateDecision(decision) != nil {
		return Observation{}, KaminoInitializationRequest{}, budgetHold("invalid_initializer_preparation")
	}
	o, err := observe(ctx)
	if err != nil {
		return o, KaminoInitializationRequest{}, err
	}
	if !decisionsEqual(manifest.DecideOnManifest(o.Snapshot), decision) || !manifest.initializationSnapshotReady(o.Snapshot) {
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
	r, err := manifest.initializationRequest(decision.StrategyKey, blockhash, rent, 1)
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
	if _, err = manifest.validateKaminoInitializationPrestate(ctx, rpc, r, max(o.Snapshot.Slot, fee.Slot)); err != nil {
		return o, r, err
	}
	return o, r, nil
}
