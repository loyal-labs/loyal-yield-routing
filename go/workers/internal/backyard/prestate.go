package backyard

import (
	"context"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// validateBuildPrestate runs before a builder loads its signer.
func validateBuildPrestate(ctx context.Context, rpc *chain.Client, view *View, request any, effects ExpectedEffects) error {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return err
	}
	_, err = manifest.validateRequestPrestate(ctx, rpc, view, request, effects)
	return err
}

// validateRequestPrestate re-reads the chain state a request was built
// against, at build and again at send: the effect graph matches the request,
// the initializer's obligation is still absent, a borrow still fits capacity
// and the loop LTV, a repayment release and the leverage, entry and funding
// swaps still see their custody, and a Hastra conversion's quoted stake price
// is still the program's and lands fresh. It returns the view slot of those
// reads.
func (m RouteManifest) validateRequestPrestate(ctx context.Context, rpc *chain.Client, view *View, request any, effects ExpectedEffects) (int64, error) {
	if r, ok := request.(StepRequest); ok {
		// The step's legs, amounts and minimum output are the wire's; the
		// simulation and the chain judge them. Only a Hastra conversion's
		// quoted price is re-read, so a moved price holds before signing.
		if rpc == nil {
			return 0, budgetHold("prestate_unavailable")
		}
		slot, err := view.slot(ctx)
		if err != nil {
			return 0, budgetHoldFrom(err, "prestate_unavailable")
		}
		if r.Hastra != nil {
			return validateHastraPrice(ctx, view, *r.Hastra, slot)
		}
		return slot, nil
	}
	if _, err := m.measureExecutableDebit(request, effects); err != nil {
		return 0, err
	}
	if rpc == nil {
		return 0, budgetHold("prestate_unavailable")
	}
	slot, err := view.slot(ctx)
	if err != nil {
		return 0, budgetHoldFrom(err, "prestate_unavailable")
	}
	if r, ok := request.(KaminoInitializationRequest); ok {
		// Basic lanes keep the exact public absent-only prestate path —
		// including validated expiry recovery — with no manifest identity
		// re-checks. Other initializer lanes revalidate through the
		// manifest-aware prestate.
		if !basicLane(r.RouteLane) {
			return m.validateKaminoInitializationPrestate(ctx, view, r, slot)
		}
		return validateKaminoInitializationPrestate(ctx, view, r, slot)
	}
	if r, ok := request.(KaminoPrimeUSDCRequest); ok && effects.Kind == "kamino-borrow" {
		observed, err := validateBorrowRequest(ctx, rpc, view, r, effects, slot)
		if err != nil {
			return 0, err
		}
		slot = max(slot, observed)
	}
	if r, ok := request.(JupiterSwapRequest); ok {
		if r.EntryReturnReserved {
			observed, err := validateEntrySwap(ctx, view, r, effects, slot)
			if err != nil {
				return 0, err
			}
			slot = max(slot, observed)
		}
	}
	if r, ok := request.(HastraConversionRequest); ok {
		observed, err := validateHastraPrice(ctx, view, r, slot)
		if err != nil {
			return 0, err
		}
		slot = max(slot, observed)
	}
	return slot, nil
}
