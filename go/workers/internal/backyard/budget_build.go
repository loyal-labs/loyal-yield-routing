package backyard

import (
	"context"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// observePhase3KnownBuildCost revalues the exact executable principal and
// unsigned-message fee before the production builder can load a signer.
// It measures cost without selecting a deployment budget. The locked durable
// admission/build/send boundaries enforce the current authorized caps. Account setup
// and the remaining exit graph still require independently bounded funding.
// Only the typed initializer admits its exact native rent in this gate.
// The production wrapper supplies the embedded reviewed manifest exactly once;
// every existing build/send/reconcile caller keeps this behavior. The
// manifest-aware form below serves only the internal candidate AUTO source
// path, whose retained legs compile against the SAME reviewed manifest that
// produced them.
func observePhase3KnownBuildCost(ctx context.Context, rpc *chain.Client, view *View, request any, effects ExpectedEffects) (ValuedTransactionCost, error) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return ValuedTransactionCost{}, err
	}
	return manifest.observePhase3KnownBuildCost(ctx, rpc, view, request, effects)
}

func (m RouteManifest) observePhase3KnownBuildCost(ctx context.Context, rpc *chain.Client, view *View, request any, effects ExpectedEffects) (ValuedTransactionCost, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	debit, err := m.measureExecutableDebit(request, effects)
	if err != nil {
		return ValuedTransactionCost{}, err
	}
	var message []byte
	var setupLamports uint64
	lane := RouteID
	switch r := request.(type) {
	case BridgeBuildRequest:
		message, err = CompileBridgeMessage(r)
	case KaminoPrimeUSDCRequest:
		message, err = compileKaminoMessageForDelegate(r, mustKey(bridgeDelegate))
		lane = r.RouteLane
	case KaminoInitializationRequest:
		message, err = m.compileKaminoInitializationMessage(r)
		lane, setupLamports = r.RouteLane, r.RentLamports
	case JupiterSwapRequest:
		message, err = compileJupiterMessageForDelegate(r, mustKey(bridgeDelegate))
		lane = r.RouteLane
	default:
		return ValuedTransactionCost{}, budgetHold("unmapped_economic_action")
	}
	if err != nil {
		return ValuedTransactionCost{}, err
	}
	if rpc == nil {
		return ValuedTransactionCost{}, budgetHold("build_valuation_unavailable")
	}
	slot, err := m.validateRequestPrestate(ctx, rpc, view, request, effects)
	if err != nil {
		return ValuedTransactionCost{}, err
	}
	var initializerPrestateSlot int64
	if _, ok := request.(KaminoInitializationRequest); ok {
		initializerPrestateSlot = slot
	}
	// Prices and the exact-message fee are independent reads. Preserve each
	// source slot and expiry, then validate all three against one final slot.
	var fee MessageFeeObservation
	var token, sol BudgetPrice
	var feeErr, tokenErr, solErr error
	var reads sync.WaitGroup
	reads.Add(2)
	go func() { defer reads.Done(); fee, feeErr = observeMessageFee(ctx, rpc, message, slot) }()
	go func() { defer reads.Done(); sol, solErr = ObserveNativeSOLBudgetPrice(ctx, rpc, view, slot) }()
	if debit.Raw > 0 {
		reads.Add(1)
		go func() {
			defer reads.Done()
			token, tokenErr = ObserveBudgetTokenPrice(ctx, rpc, view, lane, debit, slot)
		}()
	}
	reads.Wait()
	if feeErr != nil {
		return ValuedTransactionCost{}, feeErr
	}
	if r, ok := request.(KaminoInitializationRequest); ok && fee.Lamports > r.MaximumFeeLamports {
		return ValuedTransactionCost{}, budgetHold("initializer_fee_changed")
	}
	if tokenErr != nil {
		// Keep the cause (RPC read, stale reserve price whose refresh
		// simulation failed, unbound mint) visible in the hold's details.
		return ValuedTransactionCost{}, &BudgetHold{Reason: "build_token_valuation_unavailable", Details: map[string]string{"mint": debit.Mint, "cause": sanitizedHoldCause(tokenErr)}}
	}
	if solErr != nil {
		return ValuedTransactionCost{}, budgetHold("build_native_valuation_unavailable")
	}
	slot, err = view.slot(ctx)
	if err != nil {
		return ValuedTransactionCost{}, budgetHold("build_valuation_unavailable")
	}
	// The chain is at least at the newest slot any input was read at.
	slot = max(slot, fee.Slot, sol.ObservedSlot, token.ObservedSlot)
	if initializerPrestateSlot > 0 && slot-initializerPrestateSlot > observationLagSlots() {
		return ValuedTransactionCost{}, budgetHold("initializer_prestate_expired")
	}
	cost, err := ValueTransactionCost(message, debit, fee, setupLamports, token, sol, slot)
	if err != nil {
		return cost, err
	}
	if initializerPrestateSlot > 0 {
		cost.ValidThroughSlot = min(cost.ValidThroughSlot, initializerPrestateSlot+observationLagSlots())
	}
	return cost, nil
}

// phase3ReadFanout bounds the independent reads one pricer runs at once; each
// cost read already fans out to its fee and price reads.
const phase3ReadFanout = 4

// concurrentReads runs independent read-only steps at once, at most
// phase3ReadFanout in flight, and returns the first error in step order: the
// error the serial sequence would have stopped on. A failure cancels only the
// steps after it, which the serial sequence would never have reached; the
// steps before it finish, so the reported error is never a cancellation it
// caused. Each step writes only its own results.
func concurrentReads(ctx context.Context, steps ...func(context.Context) error) error {
	errs := make([]error, len(steps))
	contexts := make([]context.Context, len(steps))
	cancels := make([]context.CancelFunc, len(steps))
	for i := range steps {
		contexts[i], cancels[i] = context.WithCancel(ctx)
		defer cancels[i]()
	}
	slots := make(chan struct{}, phase3ReadFanout)
	var reads sync.WaitGroup
	for i, step := range steps {
		reads.Add(1)
		go func() {
			defer reads.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			if errs[i] = step(contexts[i]); errs[i] != nil {
				for _, cancel := range cancels[i+1:] {
					cancel()
				}
			}
		}()
	}
	reads.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// exitLegCostReads reads each cost-only leg's cost from its own template.
func exitLegCostReads(rpc *chain.Client, view *View, m RouteManifest, legs []phase3BridgeExitCost) []func(context.Context) error {
	reads := make([]func(context.Context) error, len(legs))
	for i := range legs {
		reads[i] = func(ctx context.Context) error {
			request, effects, _, err := legs[i].Template.decodeWithManifest(m)
			if err == nil {
				legs[i].Cost, err = m.observePhase3KnownBuildCost(ctx, rpc, view, request, effects)
			}
			return err
		}
	}
	return reads
}
