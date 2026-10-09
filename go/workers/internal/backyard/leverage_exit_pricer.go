package backyard

import (
	"context"
	"encoding/binary"
	"math"
	"math/big"
)

// priceLeverageExitCycles prices the B2 1.75x exit cycles on cost-only
// copies of the projected accounts: while one safe release cannot fund the
// payoff, release to the ceiling -> swap everything idle to debt -> repay
// the swap's slippage MINIMUM (never the whole debt). It returns the cycle
// legs, the post-cycle accounts (obligation, reserves and custodies
// patched), debt cash left, the accepted payoff window and actual cycle count.
// Every leg uses that window (at least 7 + 3N steps), including the final
// release. Zero cycles returns the inputs as-is.
// The legs' costs are left to the caller's concurrent cost reads.
func priceLeverageExitCycles(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, route RuntimeRoute, s Snapshot, accounts []ConfirmedAccount, slot int64, blockhash LatestBlockhash, cash uint64) ([]phase3BridgeExitCost, []ConfirmedAccount, uint64, int64, int, *KaminoPayoffBound, error) {
	return priceLeverageExitCyclesWithInitialRepay(ctx, rpc, client, m, route, s, accounts, slot, blockhash, cash, false)
}

func priceLeverageExitCyclesWithInitialRepay(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, route RuntimeRoute, s Snapshot, accounts []ConfirmedAccount, slot int64, blockhash LatestBlockhash, cash uint64, initialRepay bool) ([]phase3BridgeExitCost, []ConfirmedAccount, uint64, int64, int, *KaminoPayoffBound, error) {
	for steps := int64(7); steps <= kaminoPayoffMaxWindowSteps && steps <= int64(7+3*leverageExitMaxCycles); steps += 3 {
		legs, post, remainingCash, cycles, first, err := priceLeverageExitCyclesAtWindowWithInitialRepay(ctx, rpc, client, m, route, s, accounts, slot, blockhash, cash, steps, initialRepay)
		if err != nil {
			return nil, nil, 0, 0, 0, nil, err
		}
		if steps >= 7+3*cycles {
			return legs, post, remainingCash, steps, int(cycles), first, nil
		}
		// Discard the entire attempt. A longer horizon must reprice every
		// release and quote from the original accounts and cash, not its poststate.
	}
	return nil, nil, 0, 0, 0, nil, budgetHold("leverage_exit_cycles_exceeded")
}

// Every leg in one attempt uses the same complete-exit horizon. A cycle
// outside it returns only the required count; no partial plan can escape.
func priceLeverageExitCyclesAtWindow(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, route RuntimeRoute, s Snapshot, accounts []ConfirmedAccount, slot int64, blockhash LatestBlockhash, cash uint64, steps int64) ([]phase3BridgeExitCost, []ConfirmedAccount, uint64, int64, *KaminoPayoffBound, error) {
	return priceLeverageExitCyclesAtWindowWithInitialRepay(ctx, rpc, client, m, route, s, accounts, slot, blockhash, cash, steps, false)
}

func priceLeverageExitCyclesAtWindowWithInitialRepay(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, route RuntimeRoute, s Snapshot, accounts []ConfirmedAccount, slot int64, blockhash LatestBlockhash, cash uint64, steps int64, initialRepay bool) ([]phase3BridgeExitCost, []ConfirmedAccount, uint64, int64, *KaminoPayoffBound, error) {
	accounts = append([]ConfirmedAccount(nil), accounts...)
	var legs []phase3BridgeExitCost
	idle := uint64(max(s.CollateralIdleRaw, 0))
	cycles := 0
	var first *KaminoPayoffBound
	if initialRepay {
		// Hard risk may leave NO safe pre-repay release. Price owned cash
		// repayment first, retaining the original debt bound independently.
		if route.Lane != autoAUTOPYUSD.Lane || idle != 0 {
			return nil, nil, 0, 0, nil, budgetHold("emergency_topup_funding_cycle_invalid")
		}
		if err := validateEmergencyFundingStrictRepay(accounts, cash); err != nil {
			return nil, nil, 0, 0, nil, err
		}
		if steps < 10 {
			return nil, nil, 0, 1, nil, nil
		}
		bound, err := decodeKaminoPayoffWindow(accounts, route, slot, steps)
		if err != nil {
			return nil, nil, 0, 0, nil, err
		}
		first = &bound
		repay, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegRepay, cash, blockhash, route.Lane)
		if err != nil {
			return nil, nil, 0, 0, nil, err
		}
		repay.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
		effects, err := boundedKaminoRepaymentEffects(accounts, source, destination, cash, cash)
		if err != nil {
			return nil, nil, 0, 0, nil, err
		}
		input, err := exitLegInput(repay, effects)
		if err != nil {
			return nil, nil, 0, 0, nil, err
		}
		legs = append(legs, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: cash, Template: input})
		accounts, err = projectLeverageExitCycle(accounts, route, KaminoReleaseBound{}, nil, bound.ObservedDebtRaw, cash)
		if err != nil {
			return nil, nil, 0, 0, nil, err
		}
		cash, cycles = 0, 1
	}
	for {
		limit, err := m.decodeKaminoRepaymentReleaseForMode(accounts, route, slot, steps, s.PilotActive)
		if err != nil {
			return nil, nil, 0, 0, nil, err
		}
		// Does this release (plus idle) fund the full payoff? Probe-quote it.
		buffer := idle + limit.LiquidityRaw
		probe, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapCollateralToDebtStep, StrategyKey: route.Lane, AmountRaw: int64(buffer)}, buffer, cash, slot)
		if err != nil {
			return nil, nil, 0, 0, nil, err
		}
		if first == nil {
			payoff := limit.Payoff
			first = &payoff
		}
		if cash+probe.Request.MinimumOutputRaw >= limit.Payoff.UpperDebtRaw {
			return legs, accounts, cash, int64(cycles), first, nil
		}
		if cycles == leverageExitMaxCycles {
			return nil, nil, 0, 0, nil, budgetHold("leverage_exit_cycles_exceeded")
		}
		if int64(7+3*(cycles+1)) > steps {
			return nil, nil, 0, int64(cycles + 1), nil, nil
		}
		cycles++
		// A cycle: [release ->] [swap idle ->] partial repay. Debt cash that
		// is already on hand (after a cycle swap) is repaid first, which
		// frees the next release; otherwise release to the ceiling.
		var releaseEffects *ExpectedEffects
		var released KaminoReleaseBound
		if cash == 0 || idle > 0 {
			if cash == 0 {
				release, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, limit.ReceiptRaw, blockhash, route.Lane)
				if err != nil {
					return nil, nil, 0, 0, nil, err
				}
				release.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
				source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
				effects, err := exactKaminoTokenEffects(accounts, source, destination, limit.LiquidityRaw)
				if err != nil {
					return nil, nil, 0, 0, nil, err
				}
				input, err := exitLegInput(release, effects)
				if err != nil {
					return nil, nil, 0, 0, nil, err
				}
				legs = append(legs, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: release.AmountRaw, Template: input})
				releaseEffects, released = &effects, limit
				idle = buffer
			}
			// After a release the swap sells exactly the probed buffer: the
			// probe already is its quote.
			swap := probe
			if idle != buffer {
				if swap, err = prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapCollateralToDebtStep, StrategyKey: route.Lane, AmountRaw: int64(idle)}, idle, cash, slot); err != nil {
					return nil, nil, 0, 0, nil, err
				}
			}
			input, err := exitLegInput(swap.Request, swap.ExpectedEffects)
			if err != nil {
				return nil, nil, 0, 0, nil, err
			}
			legs = append(legs, phase3BridgeExitCost{Action: SwapCollateralToDebtStep, Amount: swap.Request.AmountRaw, Template: input})
			cash += swap.Request.MinimumOutputRaw
			idle = 0
		}
		// Partial repay of the guaranteed cash (never the whole debt).
		if cash >= limit.Payoff.ObservedDebtRaw {
			return nil, nil, 0, 0, nil, budgetHold("leverage_exit_cycle_would_pay_off")
		}
		repay, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegRepay, cash, blockhash, route.Lane)
		if err != nil {
			return nil, nil, 0, 0, nil, err
		}
		repay.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		repayAccounts := patchConfirmedTokenRaw(accounts, route.DebtCustody, cash)
		repaySource, repayDestination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
		repayEffects, err := boundedKaminoRepaymentEffects(repayAccounts, repaySource, repayDestination, cash, cash)
		if err != nil {
			return nil, nil, 0, 0, nil, err
		}
		input, err := exitLegInput(repay, repayEffects)
		if err != nil {
			return nil, nil, 0, 0, nil, err
		}
		legs = append(legs, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: cash, Template: input})
		// Project the post-cycle state (cost-only; never an RPC write).
		if accounts, err = projectLeverageExitCycle(accounts, route, released, releaseEffects, limit.Payoff.ObservedDebtRaw, cash); err != nil {
			return nil, nil, 0, 0, nil, err
		}
		cash = 0
	}
}

func exitLegInput(request any, effects ExpectedEffects) (*phase3BuildInput, error) {
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		return nil, err
	}
	return encodePhase3BuildInput(request, encoded)
}

// projectLeverageExitCycle applies one release and one partial repay to
// cost-only account copies: obligation receipts and debt, collateral reserve
// supply/liquidity, debt reserve available/borrowed, and both custodies
// (collateral swapped out, debt cash repaid).
func projectLeverageExitCycle(accounts []ConfirmedAccount, route RuntimeRoute, limit KaminoReleaseBound, release *ExpectedEffects, debtRaw, repayRaw uint64) ([]ConfirmedAccount, error) {
	out := append([]ConfirmedAccount(nil), accounts...)
	obligation, err := decodeKaminoObligation(accountAt(out, route.Kamino.Obligation), route.Kamino)
	if err != nil || obligation.collateralDepositedRaw <= limit.ReceiptRaw || debtRaw <= repayRaw {
		return nil, budgetHold("leverage_exit_projection_unavailable")
	}
	for i := range out {
		switch out[i].Address {
		case route.Kamino.Obligation, route.Kamino.CollateralReserve, route.Kamino.DebtReserve:
			out[i].Data = append([]byte(nil), out[i].Data...)
		}
	}
	o := accountAt(out, route.Kamino.Obligation).Data
	found := false
	for i := 0; i < 8; i++ {
		off := 96 + i*136
		if sameKey(o[off:off+32], route.Kamino.CollateralReserve) && binary.LittleEndian.Uint64(o[off+32:off+40]) > 0 {
			binary.LittleEndian.PutUint64(o[off+32:off+40], obligation.collateralDepositedRaw-limit.ReceiptRaw)
			found = true
		}
	}
	remainingDebt := debtRaw - repayRaw
	for i := 0; i < 5; i++ {
		off := 1208 + i*200
		if sameKey(o[off:off+32], route.Kamino.DebtReserve) && !allZero(o[off+88:off+104]) {
			// Debt SF at the reserve's cumulative rate: store it with the
			// obligation's rate set to the reserve's, so accrual restarts
			// from the projected (already accrued) amount.
			if err := putLittleFraction(o[off+88:off+104], new(big.Int).Lsh(new(big.Int).SetUint64(remainingDebt), 60)); err != nil {
				return nil, err
			}
			copy(o[off+32:off+64], accountAt(out, route.Kamino.DebtReserve).Data[296:328])
		}
	}
	if !found {
		return nil, budgetHold("leverage_exit_projection_unavailable")
	}
	c := accountAt(out, route.Kamino.CollateralReserve).Data
	available := binary.LittleEndian.Uint64(c[224:232])
	supply := binary.LittleEndian.Uint64(c[2592:2600])
	if available < limit.LiquidityRaw || supply <= limit.ReceiptRaw {
		return nil, budgetHold("leverage_exit_projection_unavailable")
	}
	binary.LittleEndian.PutUint64(c[224:232], available-limit.LiquidityRaw)
	binary.LittleEndian.PutUint64(c[2592:2600], supply-limit.ReceiptRaw)
	if release != nil {
		out = patchConfirmedTokenRaw(out, route.CollateralLiquiditySupply, release.Accounts[0].AfterRaw)
	}
	d := accountAt(out, route.Kamino.DebtReserve).Data
	borrowed := littleInt(d[232:248])
	repaidSF := new(big.Int).Lsh(new(big.Int).SetUint64(repayRaw), 60)
	if borrowed.Cmp(repaidSF) < 0 || binary.LittleEndian.Uint64(d[224:232]) > math.MaxUint64-repayRaw {
		return nil, budgetHold("leverage_exit_projection_unavailable")
	}
	if err := putLittleFraction(d[232:248], borrowed.Sub(borrowed, repaidSF)); err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint64(d[224:232], binary.LittleEndian.Uint64(d[224:232])+repayRaw)
	out = patchConfirmedTokenRaw(out, route.CollateralCustody, 0)
	return patchConfirmedTokenRaw(out, route.DebtCustody, 0), nil
}

// putLittleFraction writes a little-endian unsigned fraction into dst,
// zero-filling the rest (the Kamino scaled-fraction byte layout).
func putLittleFraction(dst []byte, value *big.Int) error {
	raw := value.Bytes()
	if value.Sign() < 0 || len(raw) > len(dst) {
		return budgetHold("leverage_exit_projection_overflow")
	}
	clear(dst)
	for i := range raw {
		dst[i] = raw[len(raw)-1-i]
	}
	return nil
}

// observePhase3ExitCycleSwapAdmission admits the swap of a 1.75x exit
// cycle's released collateral: it validates the current custody like the
// funding swap does, then prices the rest of the exit (partial repay, any
// further cycle, the final release -> payoff -> return) from the swap's
// guaranteed MINIMUM output over cost-only account copies.
func observePhase3ExitCycleSwapAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, e JupiterExecutionEvidence) (phase3BridgeAdmission, error) {
	s, r := o.Snapshot, e.Request
	cash := debtCashRaw(s)
	if rpc == nil || client == nil || !s.Fresh || !s.PilotActive || !leverageLane(s.RouteLane) || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission ||
		d.Action != SwapCollateralToDebtStep || d.Reason != exitCycleSwapReason || r.Action != d.Action || r.RouteLane != s.RouteLane || s.RouteLane != d.StrategyKey ||
		r.FullPayoffFunding || r.PositionReturnReserved || r.EntryReturnReserved || s.CollateralIdleRaw <= 0 || d.AmountRaw != s.CollateralIdleRaw || r.AmountRaw != uint64(d.AmountRaw) ||
		!s.HasPosition || s.PositionDebtRaw <= 1 || cash < 0 || !decisionsEqual(m.DecideOnManifest(s), d) || len(e.ExpectedEffects.Accounts) != 2 {
		return phase3BridgeAdmission{}, budgetHold("exit_cycle_swap_admission_unavailable")
	}
	route, err := runtimeRoute(s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	current, err := m.observePhase3KnownBuildCost(ctx, rpc, r, e.ExpectedEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	var additional []string
	if route.Lane == autoAUTOPYUSD.Lane {
		additional = append(additional, route.Kamino.Market)
	}
	bound, accounts, err := observeKaminoPayoffWindowAccounts(ctx, rpc, route, max(s.Slot, current.ObservationSlot), 3, additional...)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	for i, row := range []struct {
		address, mint string
		raw           uint64
	}{{route.CollateralCustody, route.Kamino.CollateralMint, uint64(s.CollateralIdleRaw)}, {route.DebtCustody, route.Kamino.DebtMint, uint64(cash)}} {
		a := accountAt(accounts, row.address)
		mint, _ := decodeBase58PublicKey(row.mint)
		owner, _ := decodeBase58PublicKey(bridgeVault)
		custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		effect := e.ExpectedEffects.Accounts[i]
		if err != nil || a.Executable || a.Lamports == 0 || custody.Raw != row.raw || effect.Address != row.address || effect.BeforeRaw != row.raw {
			return phase3BridgeAdmission{}, budgetHold("exit_cycle_swap_custody_changed")
		}
	}
	if !sameAccruingDebt(bound, s.PositionDebtRaw) {
		return phase3BridgeAdmission{}, budgetHold("exit_cycle_swap_debt_changed")
	}
	// Cost-only post-swap state: collateral custody empty, debt cash + the
	// enforced minimum. The projected pricer then prices the partial repay
	// as the next cycle and every later step.
	post := patchConfirmedTokenRaw(accounts, route.CollateralCustody, 0)
	post = patchConfirmedTokenRaw(post, route.DebtCustody, uint64(cash)+r.MinimumOutputRaw)
	projection := phase3KaminoProjection{Slot: bound.ObservedSlot, Accounts: post}
	plan, err := pricePhase3ProjectedPositionReturn(ctx, rpc, client, m, o, d, r, e.ExpectedEffects, current, projection)
	if err != nil {
		return plan, err
	}
	plan.Snapshot = s
	return plan, nil
}

// priceLeverageExitFromCurrent prices a multi-cycle exit for a current
// non-mutating step (a NAV report) from the observed accounts. ok=false:
// one release still funds the payoff, so the installed path prices it.
func priceLeverageExitFromCurrent(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, request any, effects ExpectedEffects, route RuntimeRoute, accounts []ConfirmedAccount) (phase3BridgeAdmission, error, bool) {
	need, err := leverageExitNeedsCycles(ctx, rpc, client, m, route, o.Snapshot, accounts)
	if err != nil || !need {
		return phase3BridgeAdmission{}, err, err != nil
	}
	current, err := m.observePhase3KnownBuildCost(ctx, rpc, request, effects)
	if err != nil {
		return phase3BridgeAdmission{}, err, true
	}
	slot := int64(0)
	if clock := accountAt(accounts, budgetClockAddress); len(clock.Data) == 40 {
		slot = int64(binary.LittleEndian.Uint64(clock.Data[:8]))
	}
	plan, err := pricePhase3ProjectedPositionReturn(ctx, rpc, client, m, o, d, request, effects, current, phase3KaminoProjection{Slot: max(slot, o.Snapshot.Slot), Accounts: accounts})
	if err == nil {
		plan.Snapshot = o.Snapshot
	}
	return plan, err, true
}

// priceLeverageExitAfterRelease prices the rest of a multi-cycle exit after
// the current repayment release, from its cost-only projected poststate.
func priceLeverageExitAfterRelease(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, r KaminoPrimeUSDCRequest, effects ExpectedEffects, bound KaminoReleaseBound, accounts []ConfirmedAccount) (phase3BridgeAdmission, error, bool) {
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err, true
	}
	// The 1.75x -> 1.5x partial release always prices from its poststate:
	// it is a de-levering step, never a payoff funding.
	if d.Reason != leverageDownPartialReleaseReason {
		need, err := leverageExitNeedsCycles(ctx, rpc, client, m, route, o.Snapshot, accounts)
		if err != nil || !need {
			return phase3BridgeAdmission{}, err, err != nil
		}
	}
	current, err := m.observePhase3KnownBuildCost(ctx, rpc, r, effects)
	if err != nil {
		return phase3BridgeAdmission{}, err, true
	}
	debit, err := m.measureExecutableDebit(r, effects)
	if err != nil {
		return phase3BridgeAdmission{}, err, true
	}
	release := KaminoReleaseBound{Payoff: bound.Payoff, ReceiptRaw: r.AmountRaw, LiquidityRaw: debit.Raw}
	// Receipts leave the obligation; the released liquidity lands in
	// collateral custody. No debt change yet.
	post, err := projectLeverageExitRelease(accounts, route, release, effects)
	if err != nil {
		return phase3BridgeAdmission{}, err, true
	}
	plan, err := pricePhase3ProjectedPositionReturn(ctx, rpc, client, m, o, d, r, effects, current, phase3KaminoProjection{Slot: bound.Payoff.ObservedSlot, Accounts: post})
	if err == nil {
		plan.Snapshot = o.Snapshot
	}
	return plan, err, true
}

// leverageExitNeedsCycles: one safe release plus idle collateral, swapped at
// its quote minimum, cannot fund the full payoff.
func leverageExitNeedsCycles(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, route RuntimeRoute, s Snapshot, accounts []ConfirmedAccount) (bool, error) {
	// Pure pre-check first (no RPC, no Jupiter): a position the value-level
	// planner clears with zero cycles, even with a 3% margin on the debt,
	// keeps the installed single-release path. Only 1.75x-like or borderline
	// positions pay for the quote-based check.
	if !leverageExitMayNeedCycles(s) || !leverageExitAccountsMayNeedCycles(accounts, route, s) {
		return false, nil
	}
	clock := accountAt(accounts, budgetClockAddress)
	if len(clock.Data) != 40 {
		return false, budgetHold("leverage_exit_clock_unavailable")
	}
	slot := int64(binary.LittleEndian.Uint64(clock.Data[:8]))
	limit, err := m.decodeKaminoRepaymentReleaseForMode(accounts, route, slot, 7, s.PilotActive)
	if err != nil {
		return false, err
	}
	cash := uint64(max(debtCashRaw(s), 0))
	buffer := uint64(max(s.CollateralIdleRaw, 0)) + limit.LiquidityRaw
	probe, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapCollateralToDebtStep, StrategyKey: route.Lane, AmountRaw: int64(buffer)}, buffer, cash, slot)
	if err != nil {
		return false, err
	}
	return cash+probe.Request.MinimumOutputRaw < limit.Payoff.UpperDebtRaw, nil
}

// projectLeverageExitRelease applies only a release to cost-only copies.
func projectLeverageExitRelease(accounts []ConfirmedAccount, route RuntimeRoute, release KaminoReleaseBound, effects ExpectedEffects) ([]ConfirmedAccount, error) {
	out := append([]ConfirmedAccount(nil), accounts...)
	obligation, err := decodeKaminoObligation(accountAt(out, route.Kamino.Obligation), route.Kamino)
	if err != nil || obligation.collateralDepositedRaw <= release.ReceiptRaw || len(effects.Accounts) != 2 {
		return nil, budgetHold("leverage_exit_projection_unavailable")
	}
	for i := range out {
		if out[i].Address == route.Kamino.Obligation || out[i].Address == route.Kamino.CollateralReserve {
			out[i].Data = append([]byte(nil), out[i].Data...)
		}
	}
	o := accountAt(out, route.Kamino.Obligation).Data
	for i := 0; i < 8; i++ {
		off := 96 + i*136
		if sameKey(o[off:off+32], route.Kamino.CollateralReserve) && binary.LittleEndian.Uint64(o[off+32:off+40]) > 0 {
			binary.LittleEndian.PutUint64(o[off+32:off+40], obligation.collateralDepositedRaw-release.ReceiptRaw)
		}
	}
	c := accountAt(out, route.Kamino.CollateralReserve).Data
	available, supply := binary.LittleEndian.Uint64(c[224:232]), binary.LittleEndian.Uint64(c[2592:2600])
	if available < release.LiquidityRaw || supply <= release.ReceiptRaw {
		return nil, budgetHold("leverage_exit_projection_unavailable")
	}
	binary.LittleEndian.PutUint64(c[224:232], available-release.LiquidityRaw)
	binary.LittleEndian.PutUint64(c[2592:2600], supply-release.ReceiptRaw)
	out = patchConfirmedTokenRaw(out, route.CollateralLiquiditySupply, effects.Accounts[0].AfterRaw)
	return patchConfirmedTokenRaw(out, route.CollateralCustody, effects.Accounts[1].AfterRaw), nil
}

// leverageExitMayNeedCycles runs the step-1 planner on snapshot values with
// a 3% debt margin (interest window, price moves). Unknown values stay on
// the quote-based check.
func leverageExitMayNeedCycles(s Snapshot) bool {
	if s.PositionCollateralValueRaw <= 0 || s.PositionDebtValueRaw < 0 {
		return true
	}
	return leverageExitOneReleaseMayNotCover(big.NewInt(s.PositionCollateralValueRaw), big.NewInt(max(s.CollateralIdleValueRaw, 0)),
		big.NewInt(s.PositionDebtValueRaw), big.NewInt(max(debtCashRaw(s), 0)))
}

// leverageExitOneReleaseMayNotCover is the pure pre-check: the first release
// is sized on the FULL obligation debt (debt cash in custody is not yet
// repaid, so it does not raise the release allowance: a post-borrow position
// holds its borrowed cash beside a larger debt); its proceeds plus idle
// collateral plus that cash must cover the debt with a 3% margin. Values
// are in one unit (debt raw). true = run the quote-based cycle check.
func leverageExitOneReleaseMayNotCover(collateral, idle, debt, cash *big.Int) bool {
	owed := new(big.Int).Quo(new(big.Int).Mul(debt, big.NewInt(103)), big.NewInt(100))
	if cash.Cmp(owed) >= 0 {
		return false
	}
	if debt.Sign() == 0 {
		return false
	}
	release := new(big.Int).Sub(collateral, new(big.Int).Quo(new(big.Int).Mul(debt, big.NewInt(10_000)), big.NewInt(leverageExitReleaseCeilingBPS)))
	if release.Sign() < 0 {
		release.SetInt64(0)
	}
	proceeds := new(big.Int).Add(release, idle)
	proceeds.Mul(proceeds, big.NewInt(10_000-leverageExitSwapLossBPS)).Quo(proceeds, big.NewInt(10_000))
	return proceeds.Add(proceeds, cash).Cmp(owed) < 0
}

// leverageExitReleaseCeilingBPS: min(55%, maxLTV-5, hard-5) is 55% on both
// AUTO (78/80) and OnRe (66/75); the pre-check uses it with its own margin.
const leverageExitReleaseCeilingBPS int64 = 5500

// leverageExitAccountsMayNeedCycles is the same pure pre-check on the
// (possibly projected) accounts: collateral and idle collateral valued in
// debt units at the reserve prices, debt at the obligation, 3% margin.
// Undecodable accounts stay on the quote-based check.
func leverageExitAccountsMayNeedCycles(accounts []ConfirmedAccount, route RuntimeRoute, s Snapshot) bool {
	obligation, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return true
	}
	collateral, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return true
	}
	debt, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return true
	}
	redeemable, err := collateral.redeemLiquidityRaw(obligation.collateralDepositedRaw)
	if err != nil {
		return true
	}
	idle := uint64(0)
	cash := uint64(0)
	for _, row := range []struct{ address, mint string }{{route.CollateralCustody, route.Kamino.CollateralMint}, {route.DebtCustody, route.Kamino.DebtMint}} {
		a := accountAt(accounts, row.address)
		mint, _ := decodeBase58PublicKey(row.mint)
		owner, _ := decodeBase58PublicKey(bridgeVault)
		if custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner); err == nil {
			if row.address == route.CollateralCustody {
				idle = custody.Raw
			} else {
				cash = custody.Raw
			}
		}
	}
	if sharedUSDCDebt(route.Lane) {
		cash = uint64(max(debtCashRaw(s), 0))
	}
	value, err := valueBetweenTokenRaw(redeemable, collateral.mintDecimals, debt.mintDecimals, collateral.marketPriceSF, debt.marketPriceSF, false)
	if err != nil || value == 0 || value > math.MaxInt64 || obligation.debtRaw > math.MaxInt64/2 {
		return true
	}
	idleValue, err := valueBetweenTokenRaw(idle, collateral.mintDecimals, debt.mintDecimals, collateral.marketPriceSF, debt.marketPriceSF, false)
	if err != nil {
		return true
	}
	return leverageExitOneReleaseMayNotCover(new(big.Int).SetUint64(value), new(big.Int).SetUint64(idleValue),
		new(big.Int).SetUint64(obligation.debtRaw), new(big.Int).SetUint64(cash))
}
