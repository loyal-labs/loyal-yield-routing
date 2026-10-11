package backyard

import (
	"context"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// leverageStepCost is the expected cost of one leverage up step, in USDC
// micros at central observed prices: what the vault's capital loses by
// taking the step, from the same evidence the selector's recipe pricer uses.
// Execution bounds (swap minOut, recipe limits) stay on the legs themselves;
// none of them is a cost.
//
// The step is eventually repaid, so its unwind is charged at entry: release
// the increment's collateral, sell it back to debt, repay. An up step that
// only pays for its way in would look profitable while the level it buys is
// never left for free, and the spread rule's down moves sell exactly this
// increment.
type leverageStepCost struct {
	BorrowFeeMicros     int64 `json:"borrowFeeMicros"`
	EntrySwapLossMicros int64 `json:"entrySwapLossMicros"`
	ExitSwapLossMicros  int64 `json:"exitSwapLossMicros"`
	NetworkMicros       int64 `json:"networkMicros"`
	TotalMicros         int64 `json:"totalMicros"`
}

// leverageStepPricer prices the step that borrows borrowRaw more debt. An
// error is an unknown cost, never zero.
type leverageStepPricer func(borrowRaw uint64) (leverageStepCost, error)

// leverageStepLegs is one step's observed evidence: the swap quotes in both
// directions at the step's size, and the central values and network fees of
// its six transactions (borrow, swap, deposit; release, swap, repay).
type leverageStepLegs struct {
	BorrowFeeMicros int64
	// Entry: BorrowRaw debt in, its quoted collateral out.
	EntryInMicros, EntryOutMicros int64
	// Exit: the entry's quoted collateral in, its quoted debt out.
	ExitInMicros, ExitOutMicros int64
	NetworkMicros               int64
}

// cost is the step's expected cost. A swap that quotes above the central
// price is not a credit: the recipe pricer floors each swap's expected loss
// at zero, and so does this.
func (l leverageStepLegs) cost() (leverageStepCost, error) {
	out := leverageStepCost{BorrowFeeMicros: l.BorrowFeeMicros, NetworkMicros: l.NetworkMicros,
		EntrySwapLossMicros: max(l.EntryInMicros-l.EntryOutMicros, 0), ExitSwapLossMicros: max(l.ExitInMicros-l.ExitOutMicros, 0)}
	if l.BorrowFeeMicros < 0 || l.NetworkMicros <= 0 || l.EntryInMicros <= 0 || l.EntryOutMicros <= 0 || l.ExitInMicros <= 0 || l.ExitOutMicros <= 0 {
		return out, budgetHold("leverage_cost_unavailable")
	}
	var err error
	out.TotalMicros, err = budgetSum(out.BorrowFeeMicros, out.EntrySwapLossMicros, out.ExitSwapLossMicros, out.NetworkMicros)
	if err != nil {
		return out, budgetHoldFrom(err, "leverage_cost_unavailable")
	}
	return out, nil
}

// productionLeverageStepPricer quotes the working lane's step live: Jupiter
// debt->collateral at the step's size and collateral->debt at the quoted
// output, the reserves' and SOL's observed prices from the account view, the
// KLend origination fee at the reserve's rate, and each of the six compiled
// transactions' fee (chain.MessageFee).
func productionLeverageStepPricer(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, s Snapshot) leverageStepPricer {
	return func(borrowRaw uint64) (leverageStepCost, error) {
		legs, err := observeLeverageStepLegs(ctx, rpc, view, client, m, s, borrowRaw)
		if err != nil {
			return leverageStepCost{}, budgetHoldFrom(err, "leverage_cost_unavailable")
		}
		return legs.cost()
	}
}

func observeLeverageStepLegs(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, s Snapshot, borrowRaw uint64) (leverageStepLegs, error) {
	var out leverageStepLegs
	route, err := runtimeRoute(s.RouteLane)
	if err != nil || rpc == nil || view == nil || client == nil || borrowRaw == 0 || s.Slot <= 0 {
		return out, budgetHold("leverage_cost_unavailable")
	}
	fee, err := kaminoBorrowFeeAtRate(s.BorrowFeeRate, borrowRaw)
	if err != nil {
		return out, err
	}
	var policies installedPolicies
	var blockhash LatestBlockhash
	if err := concurrentReads(ctx, func(ctx context.Context) (err error) {
		policies, err = observeInstalledPolicies(ctx, rpc, s.Slot)
		return err
	}, func(ctx context.Context) (err error) {
		blockhash, err = latestBlockhash(ctx, rpc)
		return err
	}); err != nil {
		return out, err
	}
	entry, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, policies, Decision{Action: SwapDebtToCollateralStep, StrategyKey: route.Lane, AmountRaw: int64(borrowRaw)}, borrowRaw, 0, s.Slot)
	if err != nil {
		return out, err
	}
	collateralRaw := entry.Request.QuotedOutputRaw
	exit, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, policies, Decision{Action: SwapCollateralToDebtStep, StrategyKey: route.Lane, AmountRaw: int64(collateralRaw)}, collateralRaw, 0, s.Slot)
	if err != nil {
		return out, err
	}
	debt := ExecutableDebit{Source: route.DebtCustody, Mint: route.Kamino.DebtMint, TokenProgram: route.DebtTokenProgram, Raw: borrowRaw}
	collateral := ExecutableDebit{Source: route.CollateralCustody, Mint: route.Kamino.CollateralMint, TokenProgram: route.CollateralTokenProgram, Raw: collateralRaw}
	var debtPrice, collateralPrice, sol BudgetPrice
	if err := concurrentReads(ctx, func(ctx context.Context) (err error) {
		debtPrice, err = ObserveBudgetTokenPrice(ctx, rpc, view, route.Lane, debt, s.Slot)
		return err
	}, func(ctx context.Context) (err error) {
		collateralPrice, err = ObserveBudgetTokenPrice(ctx, rpc, view, route.Lane, collateral, s.Slot)
		return err
	}, func(ctx context.Context) (err error) {
		sol, err = ObserveNativeSOLBudgetPrice(ctx, rpc, view, s.Slot)
		return err
	}); err != nil {
		return out, err
	}
	slot := max(debtPrice.ObservedSlot, collateralPrice.ObservedSlot, sol.ObservedSlot)
	central := func(p BudgetPrice, d ExecutableDebit, raw uint64) (int64, error) {
		if raw == 0 {
			return 0, nil
		}
		return selectorMidpointPriceValue(p, raw, d.Mint, d.TokenProgram, slot)
	}
	for _, v := range []struct {
		into  *int64
		price BudgetPrice
		debit ExecutableDebit
		raw   uint64
	}{
		{&out.BorrowFeeMicros, debtPrice, debt, fee},
		{&out.EntryInMicros, debtPrice, debt, borrowRaw},
		{&out.EntryOutMicros, collateralPrice, collateral, collateralRaw},
		{&out.ExitInMicros, collateralPrice, collateral, collateralRaw},
		{&out.ExitOutMicros, debtPrice, debt, exit.Request.QuotedOutputRaw},
	} {
		if *v.into, err = central(v.price, v.debit, v.raw); err != nil {
			return out, err
		}
	}
	// The six transactions' fees. Each Kamino leg refreshes both reserves of
	// the levered obligation, as the worker's own legs do.
	var messages [][]byte
	for _, leg := range []struct {
		action Action
		leg    kaminoPrimeUSDCLeg
		amount uint64
	}{{OpenRouteStep, kaminoLegBorrow, borrowRaw}, {OpenRouteStep, kaminoLegDeposit, entry.Request.MinimumOutputRaw}, {DeleverRouteStep, kaminoLegWithdraw, collateralRaw}, {DeleverRouteStep, kaminoLegRepay, exit.Request.MinimumOutputRaw}} {
		r, err := m.kaminoPacketForRoute(policies, leg.action, leg.leg, leg.amount, blockhash, route.Lane)
		if err != nil {
			return out, err
		}
		r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		message, err := compileKaminoMessageForDelegate(r, mustKey(bridgeDelegate))
		if err != nil {
			return out, err
		}
		messages = append(messages, message)
	}
	for _, swap := range []JupiterSwapRequest{entry.Request, exit.Request} {
		message, err := compileJupiterMessageForDelegate(swap, mustKey(bridgeDelegate))
		if err != nil {
			return out, err
		}
		messages = append(messages, message)
	}
	for _, message := range messages {
		cost, err := ValueTransactionCost(message, ExecutableDebit{}, 0, BudgetPrice{}, sol, slot)
		if err != nil {
			return out, err
		}
		if out.NetworkMicros, err = budgetSum(out.NetworkMicros, cost.NetworkFeeMicros); err != nil {
			return out, err
		}
	}
	return out, nil
}
