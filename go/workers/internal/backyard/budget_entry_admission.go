package backyard

import "context"

// A prospective reverse quote is not evidence of current custody. Validate only
// the persisted current entry against one fresh account batch, also at final
// send. No entry is allowed to adopt an unaccounted open position or balance.
func validateEntrySwap(ctx context.Context, view *View, request JupiterSwapRequest, effects ExpectedEffects, slot int64) (int64, error) {
	if selectorLane(request.RouteLane) && (len(effects.Accounts) == 0 || effects.Accounts[0].BeforeRaw != request.AmountRaw) {
		return 0, budgetHold("entry_swap_must_consume_working_cash")
	}
	if !request.EntryReturnReserved || request.FullPayoffFunding || request.Action != SwapStableToCollateralStep || len(effects.Accounts) != 2 {
		return 0, budgetHold("entry_swap_intent_mismatch")
	}
	if _, err := MeasureExecutableDebit(request, effects); err != nil {
		return 0, err
	}
	if effects.Accounts[1].AfterRaw != request.MinimumOutputRaw || effects.Accounts[1].MinimumAfterRaw == nil || *effects.Accounts[1].MinimumAfterRaw != request.MinimumOutputRaw {
		return 0, budgetHold("entry_swap_output_mismatch")
	}
	route, err := runtimeRoute(request.RouteLane)
	if err != nil {
		return 0, err
	}
	sourceMint, destinationMint, source, destination, err := jupiterEdgeForRoute(request.Action, request.RouteLane)
	if err != nil || source != bridgeSquadsATA || sourceMint != bridgeUSDC || destination != route.CollateralCustody {
		return 0, budgetHold("entry_swap_custody_mismatch")
	}
	observed, accounts, err := view.read(ctx, []string{source, destination, route.DebtCustody, route.Kamino.Obligation}, slot)
	if err != nil {
		return 0, budgetHold("entry_swap_state_unavailable")
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	// A top-up swap sits beside a funded position, debt-free or (AUTO) with
	// debt; every other entry swap requires a flat obligation. Debt custody
	// stays empty either way (checked below).
	if err != nil || (obligation.debtRaw != 0 && !(request.TopupReturnReserved && debtTopupLane(request.RouteLane))) ||
		(obligation.collateralDepositedRaw == 0) != !request.TopupReturnReserved {
		return 0, budgetHold("entry_swap_position_changed")
	}
	for i, identity := range []struct{ address, mint string }{{source, sourceMint}, {destination, destinationMint}, {route.DebtCustody, route.Kamino.DebtMint}} {
		a := accountAt(accounts, identity.address)
		mint, _ := decodeBase58PublicKey(identity.mint)
		authority, _ := decodeBase58PublicKey(bridgeVault)
		custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, authority)
		if err != nil || a.Executable || a.Lamports == 0 {
			return 0, budgetHold("entry_swap_custody_changed")
		}
		if i == 2 {
			if identity.address == source && identity.mint == sourceMint {
				continue
			}
			if custody.Raw != 0 {
				return 0, budgetHold("entry_swap_debt_custody_changed")
			}
			continue
		}
		e := effects.Accounts[i]
		if e.Address != identity.address || e.Mint != identity.mint || e.Owner != a.Owner || e.Authority != bridgeVault || e.BeforeRaw != custody.Raw || (i == 1 && custody.Raw != 0) {
			return 0, budgetHold("entry_swap_custody_changed")
		}
	}
	return observed, nil
}
