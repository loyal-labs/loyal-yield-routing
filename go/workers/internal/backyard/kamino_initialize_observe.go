package backyard

import (
	"context"
	"encoding/binary"
	"math"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// validateKaminoInitializationPrestate is the manifest-aware form that also
// admits the AUTO lane once its request compiles; every absent-only check
// below is the exact installed check.
func (m RouteManifest) validateKaminoInitializationPrestate(ctx context.Context, view *View, r KaminoInitializationRequest, minimumSlot int64) (int64, error) {
	if err := m.validateInitializationRequest(r); err != nil {
		return 0, err
	}
	_, inner, err := initializerRouteForRequest(r)
	if err != nil {
		return 0, err
	}
	return observeKaminoInitializationPrestate(ctx, view, r, minimumSlot, selectorExitBound{}, false, inner)
}

// initializerRouteForRequest resolves the lane topology: basic lanes through
// the Multiply initializer, other initializer lanes (AUTO) through the checked
// route-aware core (its request already compiled by the caller).
func initializerRouteForRequest(r KaminoInitializationRequest) (RuntimeRoute, compiledInstruction, error) {
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return RuntimeRoute{}, compiledInstruction{}, err
	}
	if basicLane(route.Lane) {
		inner, err := kaminoMultiplyInitializer(route.Lane)
		return route, inner, err
	}
	inner, err := kaminoRouteInitializer(route)
	return route, inner, err
}

func observeKaminoInitializationPrestate(ctx context.Context, view *View, r KaminoInitializationRequest, minimumSlot int64, bound selectorExitBound, forecastExistingObligation bool, inner compiledInstruction) (int64, error) {
	if view == nil || minimumSlot <= 0 {
		return 0, budgetHold("initializer_prestate_unavailable")
	}
	addresses := []string{bridgeDelegate}
	for _, a := range inner.accounts {
		addresses = append(addresses, encodeBase58(a.key[:]))
	}
	route, _ := runtimeRoute(r.RouteLane)
	slot, accounts, err := view.read(ctx, addresses, minimumSlot, route.Kamino.Obligation)
	if err != nil {
		return 0, budgetHoldFrom(err, "initializer_prestate_unavailable")
	}
	for _, a := range []struct {
		address string
		minimum uint64
	}{{bridgeVault, r.RentLamports}, {bridgeDelegate, r.MaximumFeeLamports}} {
		account := accountAt(accounts, a.address)
		if account.Owner != "11111111111111111111111111111111" || account.Executable || len(account.Data) != 0 || account.Lamports < a.minimum {
			return 0, budgetHold("initializer_native_funding_unavailable")
		}
	}
	o := accountAt(accounts, route.Kamino.Obligation)
	if o.Address != route.Kamino.Obligation || o.Executable {
		return 0, budgetHold("initializer_obligation_already_present")
	}
	if !forecastExistingObligation {
		if o.Lamports != 0 || len(o.Data) != 0 {
			return 0, budgetHold("initializer_obligation_already_present")
		}
	} else {
		decoded, err := decodeKaminoObligation(o, route.Kamino)
		if err != nil || o.Lamports == 0 || len(o.Data) == 0 ||
			int64(decoded.collateralDepositedRaw) != bound.MaxCollateralRaw || decoded.debtRaw > uint64(bound.MaxDebtRaw) {
			return 0, budgetHold("initializer_reentry_obligation_unobserved")
		}
	}
	metadata := accountAt(accounts, encodeBase58(inner.accounts[6].key[:]))
	if decoded, err := kamino.DecodeUserMetadata(chainAccount(metadata, metadata.Address)); err != nil || !decoded.Referrer.IsZero() || decoded.Owner != kaminoKey(bridgeVault) {
		return 0, budgetHoldFrom(err, "initializer_metadata_unavailable")
	}
	if emergency, err := decodeKaminoMarketEmergency(accountAt(accounts, route.Kamino.Market), route.Kamino); err != nil || emergency {
		return 0, budgetHoldFrom(err, "initializer_market_unavailable")
	}
	// Each seed mint must exist under its route's own reviewed token program
	// and pass the execution-mint parser every admitted leg uses (extension
	// whitelist, transfer fee and transfer hook disabled). The parser's
	// decimals argument pins the observed decimals byte to the supported
	// scale, the same <=18 bound decodeKaminoReserve enforces elsewhere;
	// absolute decimals identity stays pinned by each lane's reserve reads.
	for _, mint := range []struct{ address, program string }{
		{route.Kamino.CollateralMint, route.CollateralTokenProgram},
		{route.Kamino.DebtMint, route.DebtTokenProgram},
	} {
		a := accountAt(accounts, mint.address)
		if a.Lamports == 0 || len(a.Data) < 82 || validateExecutionMint(a, mint.program, a.Data[44]) != nil {
			return 0, budgetHold("initializer_seed_mint_unavailable")
		}
	}
	rent := accountAt(accounts, "SysvarRent111111111111111111111111111111111")
	if rent.Owner != "Sysvar1111111111111111111111111111111111111" || rent.Executable || len(rent.Data) != 17 {
		return 0, budgetHold("initializer_rent_unavailable")
	}
	perByte := binary.LittleEndian.Uint64(rent.Data[:8])
	threshold := math.Float64frombits(binary.LittleEndian.Uint64(rent.Data[8:16]))
	if perByte == 0 || perByte > math.MaxUint64/(kamino.ObligationSize+128) || !finite(threshold) || threshold <= 0 {
		return 0, budgetHold("initializer_rent_invalid")
	}
	minimum := float64(perByte*(kamino.ObligationSize+128)) * threshold
	if !finite(minimum) || minimum >= float64(math.MaxInt64) || uint64(minimum) != r.RentLamports {
		return 0, budgetHold("initializer_rent_changed")
	}
	return slot, nil
}
