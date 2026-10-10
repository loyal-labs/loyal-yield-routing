package backyard

import (
	"context"
	"encoding/binary"
	"math"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// The simulation's poststate is used only for complete exit costing. Each
// producer validates its own current transition before entering this function;
// no projected account replaces a current build, RPC read or send prestate.
func pricePhase3ProjectedPositionReturn(ctx context.Context, rpc *chain.Client, client *jupiter.Client, m RouteManifest, o Observation, d Decision, request any, effects ExpectedEffects, current ValuedTransactionCost, projection phase3KaminoProjection) (phase3BridgeAdmission, error) {
	s := o.Snapshot
	route, err := runtimeRoute(s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	bound, err := decodeKaminoPayoffWindow(projection.Accounts, route, projection.Slot, 3)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	position, err := decodeKaminoObligation(accountAt(projection.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || position.collateralDepositedRaw == 0 || position.collateralDepositedRaw > math.MaxInt64 {
		return phase3BridgeAdmission{}, budgetHold("projected_return_position_unavailable")
	}
	s.PositionCollateralRaw = int64(position.collateralDepositedRaw)
	var cash uint64
	for _, row := range []struct{ address, mint string }{{route.CollateralCustody, route.Kamino.CollateralMint}, {route.DebtCustody, route.Kamino.DebtMint}} {
		a := accountAt(projection.Accounts, row.address)
		mint, _ := decodeBase58PublicKey(row.mint)
		owner, _ := decodeBase58PublicKey(bridgeVault)
		balance, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Executable || a.Lamports == 0 || balance.Raw > math.MaxInt64 {
			return phase3BridgeAdmission{}, budgetHold("projected_return_custody_unavailable")
		}
		if row.address == route.CollateralCustody {
			s.CollateralIdleRaw, s.PrimeIdleRaw = int64(balance.Raw), int64(balance.Raw)
		} else {
			cash = balance.Raw
		}
	}
	var blockhash LatestBlockhash
	switch r := request.(type) {
	case KaminoPrimeUSDCRequest:
		blockhash = LatestBlockhash{Blockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}
	case JupiterSwapRequest:
		blockhash = LatestBlockhash{Blockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}
	case BridgeBuildRequest:
		// NAV, admitted partial bridge legs and the plan B3 top-up allocation
		// price the remaining position; this cost-only helper grants no
		// permission to execute that exit.
		topup := r.Action == VoltrAllocateToSquads && d.Reason == topupAllocationReason
		if (r.Action != ReportNAV && r.Action != StageSquadsToVoltr && r.Action != VoltrRestoreIdle && !topup) || !leverageLane(s.RouteLane) {
			return phase3BridgeAdmission{}, budgetHold("invalid_projected_return_request")
		}
		blockhash = LatestBlockhash{Blockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}
	default:
		return phase3BridgeAdmission{}, budgetHold("invalid_projected_return_request")
	}
	remaining := uint64(s.PositionCollateralRaw)
	reserve, err := decodeKaminoReserve(accountAt(projection.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	accounts := append([]ConfirmedAccount(nil), projection.Accounts...)
	var release *KaminoExecutionEvidence
	var funding *JupiterExecutionEvidence
	var releaseCost, fundingCost ValuedTransactionCost
	upperCash := cash
	// B2 1.75x: when one release cannot fund the payoff, price the exit
	// cycles (release -> swap -> partial repay) on cost-only account copies
	// first; the payoff branch below then prices the final release from the
	// post-cycle state over the longer 7 + 3N window.
	var cycles []phase3BridgeExitCost
	var firstPayoff *KaminoPayoffBound
	windowSteps := int64(7)
	if cash < bound.UpperDebtRaw && leverageLane(s.RouteLane) && leverageExitAccountsMayNeedCycles(accounts, route, s) {
		var cycleCash uint64
		cycles, accounts, cycleCash, windowSteps, firstPayoff, err = priceLeverageExitCycles(ctx, rpc, client, m, route, s, accounts, projection.Slot, blockhash, cash)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		if len(cycles) > 0 {
			cash, upperCash = cycleCash, cycleCash
			projection.Accounts = accounts
			accounts = append([]ConfirmedAccount(nil), accounts...)
			if position, err = decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino); err != nil {
				return phase3BridgeAdmission{}, err
			}
			remaining = position.collateralDepositedRaw
			if reserve, err = decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino); err != nil {
				return phase3BridgeAdmission{}, err
			}
			s.CollateralIdleRaw, s.PrimeIdleRaw = 0, 0
		}
	}
	if cash < bound.UpperDebtRaw {
		// Borrow -> NAV -> release -> NAV -> swap -> NAV -> payoff. Combine
		// existing residue with the safe release; never require a dust-only swap.
		limit, err := m.decodeKaminoRepaymentReleaseForMode(projection.Accounts, route, projection.Slot, windowSteps, true)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		bound = limit.Payoff
		if limit.LiquidityRaw > uint64(math.MaxInt64-s.CollateralIdleRaw) {
			return phase3BridgeAdmission{}, budgetHold("borrow_release_buffer_overflow")
		}
		req, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, limit.ReceiptRaw, blockhash, s.RouteLane)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		req.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
		effects, err := exactKaminoTokenEffects(projection.Accounts, source, destination, limit.LiquidityRaw)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		release = &KaminoExecutionEvidence{req, effects}
		buffer := uint64(s.CollateralIdleRaw) + limit.LiquidityRaw
		quote, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapCollateralToDebtStep, StrategyKey: s.RouteLane, AmountRaw: int64(buffer)}, buffer, cash, projection.Slot)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		// Validate enforceable wire minimum against all interest windows using
		// a cost-only view of the post-release custody, not optimistic output.
		projected := append([]ConfirmedAccount(nil), projection.Accounts...)
		for i, a := range projected {
			if a.Address == route.CollateralCustody {
				projected[i].Data = append([]byte(nil), a.Data...)
				binary.LittleEndian.PutUint64(projected[i].Data[64:72], buffer)
			}
		}
		check := quote.Request
		check.FullPayoffFunding = true
		if _, _, err = validatePayoffFundingAccounts(m, check, quote.ExpectedEffects, bound, projected, route); err != nil {
			return phase3BridgeAdmission{}, err
		}
		funding = &quote
		upper, err := withdrawalUSDCExitEstimate(quote.Request.QuotedOutputRaw)
		if err != nil || upper > math.MaxInt64-cash {
			return phase3BridgeAdmission{}, budgetHold("borrow_funding_output_overflow")
		}
		upperCash += upper
		remaining = limit.RemainingReceiptRaw
		reserve, err = projectKaminoReleaseReserve(reserve, limit.ReceiptRaw, limit.LiquidityRaw)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		for i, a := range accounts {
			if a.Address == route.CollateralCustody || a.Address == route.CollateralLiquiditySupply {
				accounts[i].Data = append([]byte(nil), a.Data...)
				amount := uint64(0)
				if a.Address == route.CollateralLiquiditySupply {
					amount = effects.Accounts[0].AfterRaw
				}
				binary.LittleEndian.PutUint64(accounts[i].Data[64:72], amount)
			}
		}
	}
	payoff, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegRepay, bound.UpperDebtRaw, blockhash, s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	payoff.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	payoffAccounts := append([]ConfirmedAccount(nil), accounts...)
	for i, a := range payoffAccounts {
		if a.Address == route.DebtCustody {
			payoffAccounts[i].Data = append([]byte(nil), a.Data...)
			binary.LittleEndian.PutUint64(payoffAccounts[i].Data[64:72], upperCash)
		}
	}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	payoffEffects, err := boundedKaminoRepaymentEffects(payoffAccounts, source, destination, bound.ObservedDebtRaw, bound.UpperDebtRaw)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	withdrawal, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, remaining, blockhash, s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	withdrawal.ObligationReserves = []string{route.Kamino.CollateralReserve}
	policies := map[string]string{payoff.Policy: payoff.PolicyAccountDataSHA256, withdrawal.Policy: withdrawal.PolicyAccountDataSHA256}
	if release != nil {
		policies[release.Request.Policy] = release.Request.PolicyAccountDataSHA256
	}
	var addresses []string
	for address := range policies {
		addresses = append(addresses, address)
	}
	amount, err := reserve.redeemLiquidityRaw(remaining)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	source, destination = kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	withdrawalEffects, err := exactKaminoTokenEffects(accounts, source, destination, amount)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	post := o
	post.Snapshot.PositionDebtRaw, post.Snapshot.PositionDebtValueRaw, post.Snapshot.PayoffDebtRaw = 0, 0, 0
	post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = s.CollateralIdleRaw, s.PrimeIdleRaw
	post.Snapshot.PositionCollateralRaw = int64(remaining)
	setDebtCashRaw(&post.Snapshot, int64(upperCash-bound.ObservedDebtRaw))
	if funding != nil {
		post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = 0, 0
	}
	// The exit's shape is fixed now. Its leg costs, the policy check and the
	// tail are reads that feed nothing back into it: read them all at once.
	var payoffCost ValuedTransactionCost
	var tail phase3BridgeAdmission
	reads := exitLegCostReads(rpc, m, cycles)
	if release != nil {
		reads = append(reads, func(ctx context.Context) (err error) {
			releaseCost, err = observePhase3KnownBuildCost(ctx, rpc, release.Request, release.ExpectedEffects)
			return err
		}, func(ctx context.Context) (err error) {
			fundingCost, err = m.observePhase3KnownBuildCost(ctx, rpc, funding.Request, funding.ExpectedEffects)
			return err
		})
	}
	reads = append(reads, func(ctx context.Context) (err error) {
		payoffCost, err = observePhase3KnownBuildCost(ctx, rpc, payoff, payoffEffects)
		return err
	}, func(ctx context.Context) error {
		_, rows, err := confirmedAccounts(ctx, rpc, addresses, projection.Slot)
		if err != nil {
			return err
		}
		for address, hash := range policies {
			a := accountAt(rows, address)
			if a.Owner != squads.ProgramID.String() || a.Executable || a.Lamports == 0 || sha256Bytes(a.Data) != hash {
				return budgetHold("borrow_exit_policy_drift")
			}
		}
		return nil
	}, func(ctx context.Context) (err error) {
		tail, err = observePhase3WithdrawalAdmission(ctx, rpc, client, m, post, Decision{Action: DeleverRouteStep, StrategyKey: s.RouteLane}, KaminoExecutionEvidence{withdrawal, withdrawalEffects})
		return err
	})
	if err := concurrentReads(ctx, reads...); err != nil {
		return phase3BridgeAdmission{}, err
	}
	if len(tail.Exit) == 0 || tail.Exit[0].Action != ReportNAV {
		return tail, budgetHold("borrow_exit_nav_unavailable")
	}
	plan := tail
	plan.Snapshot, plan.Decision, plan.CurrentCost = o.Snapshot, d, current
	plan.Input, err = exitLegInput(request, effects)
	if err != nil {
		return plan, err
	}
	plan.Payoff = &bound
	if len(cycles) > 0 {
		// The exit bound is the CURRENT position's payoff window: the first
		// cycle's, not the final post-cycle one.
		plan.Payoff = firstPayoff
	}
	plan.PayoffRepayment, err = exitLegInput(payoff, payoffEffects)
	if err != nil {
		return plan, err
	}
	nav := phase3BridgeExitCost{Action: ReportNAV, Cost: tail.Exit[0].Cost, Template: tail.Exit[0].Template}
	prefix := []phase3BridgeExitCost{nav}
	for _, step := range cycles {
		prefix = append(prefix, step)
		if step.Action == DeleverRouteStep || step.Action == SwapCollateralToDebtStep {
			prefix = append(prefix, nav)
		}
	}
	if funding != nil {
		releaseInput, err := exitLegInput(release.Request, release.ExpectedEffects)
		if err != nil {
			return plan, err
		}
		input, err := exitLegInput(funding.Request, funding.ExpectedEffects)
		if err != nil {
			return plan, err
		}
		prefix = append(prefix, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: release.Request.AmountRaw, Cost: releaseCost, Template: releaseInput}, nav, phase3BridgeExitCost{Action: SwapCollateralToDebtStep, Amount: funding.Request.AmountRaw, Cost: fundingCost, Template: input}, nav)
	}
	prefix = append(prefix, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: payoff.AmountRaw, Cost: payoffCost, Template: plan.PayoffRepayment}, nav, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: withdrawal.AmountRaw, Cost: tail.CurrentCost, Template: tail.Input})
	plan.Exit = append(prefix, tail.Exit...)
	plan.ExitAfterMicros = 0
	plan.ValidThroughSlot = min(current.ValidThroughSlot, tail.ValidThroughSlot, projection.Slot+observationLagSlots())
	for _, step := range plan.Exit {
		plan.ExitAfterMicros, err = budgetSum(plan.ExitAfterMicros, step.Cost.TotalMicros)
		if err != nil {
			return plan, err
		}
		plan.ValidThroughSlot = min(plan.ValidThroughSlot, step.Cost.ValidThroughSlot)
	}
	if projection.Slot > plan.ValidThroughSlot {
		return plan, budgetHold("stale_borrow_exit_admission")
	}
	return plan, nil
}
