package backyardrwa

import (
	"context"
	"math"
	"time"
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
	if s.ObservationID == "" || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagSlots || s.RouteKind != RouteKind || !s.Fresh {
		return false
	}
	if !s.PilotActive || !s.InitializationPolicyReady || !laneAllowed(s.RouteLane) || !s.ObligationPresenceKnown || s.ObligationPresent {
		return false
	}
	if s.HasPosition || s.PositionCollateralValueRaw != 0 || s.PositionDebtValueRaw != 0 || s.CollateralIdleValueRaw != 0 || s.StrategyNAVRaw != 0 || s.PositionCollateralRaw != 0 || s.PositionDebtRaw != 0 || s.CollateralIdleRaw != 0 || s.DebtIdleRaw != 0 || s.SquadsIdleRaw != 0 || s.VoltrStrategyIdleRaw != 0 {
		return false
	}
	if s.Nonterminal != "" || s.HasAmbiguousSubmission || s.ManualReason != "" || s.Unwind || s.CutoverDrain || s.SelectorEntryPaused || s.WithdrawalDemandRaw != 0 {
		return false
	}
	return s.SelectorEntryEquityRaw > 0 && s.SelectorEntryEquityRaw <= min(s.VoltrIdleRaw, s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw, workingTrancheCap(s)) && s.PolicyReady && s.ExitBuildable && s.CapacityRaw > 0 && s.PolicyLimitRaw > 0 && s.MaxTargetLTVEntryRaw > 0 && min(s.LiquidationThresholdBPS-1500, 6000) > TargetLTVBPS
}

// This prepares only an empty account before allocating user principal. Actual
// rent, installed policy, metadata, market and native funding are rechecked by
// the existing prestate gate at admission and immediately before signing/send.
func prepareKaminoInitialization(ctx context.Context, rpc *RPCClient, manifest RouteManifest, decision Decision, observe func(context.Context) (Observation, error)) (Observation, KaminoInitializationRequest, error) {
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
	var rent uint64
	if err = rpc.call(ctx, "getMinimumBalanceForRentExemption", []any{kaminoObligationLength, map[string]string{"commitment": "confirmed"}}, &rent); err != nil || rent == 0 {
		return o, KaminoInitializationRequest{}, budgetHold("initializer_rent_unavailable")
	}
	blockhash, err := rpc.LatestBlockhash(ctx)
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
	fee, err := rpc.ObserveMessageFee(ctx, message, o.Snapshot.Slot)
	if err != nil {
		return o, r, err
	}
	r.MaximumFeeLamports = fee.Lamports
	if _, err = manifest.validateKaminoInitializationPrestate(ctx, rpc, r, max(o.Snapshot.Slot, fee.Slot)); err != nil {
		return o, r, err
	}
	return o, r, nil
}

func (d *Database) admitKaminoInitialization(ctx context.Context, rpc *RPCClient, manifest RouteManifest, id string, o Observation, decision Decision, r KaminoInitializationRequest) error {
	if !manifest.initializationSnapshotReady(o.Snapshot) || !decisionsEqual(manifest.DecideOnManifest(o.Snapshot), decision) || decision.StrategyKey != r.RouteLane {
		return budgetHold("initializer_decision_changed")
	}
	if err := manifest.validateBindings(); err != nil {
		return err
	}
	if err := manifest.validateInitializationRequest(r); err != nil {
		return err
	}
	effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}
	cost, err := manifest.observePhase3KnownBuildCost(ctx, rpc, r, effects)
	if err != nil {
		return err
	}
	raw, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		return err
	}
	input, err := encodePhase3BuildInput(r, raw)
	if err != nil {
		return err
	}
	// There is no token exposure or exit graph yet. Creation cannot consume an
	// outstanding exit reservation; the locked shared admission checks that too.
	plan := phase3BridgeAdmission{Snapshot: o.Snapshot, Decision: decision, Input: input, CurrentCost: cost, ValidThroughSlot: min(cost.ValidThroughSlot, o.Snapshot.Slot+budgetMaxObservationLagSlots)}
	return d.persistPhase3ExitAdmissionOnManifest(ctx, rpc, manifest, id, o, decision, plan)
}
