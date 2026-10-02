package backyardrwa

import (
	"context"
	"encoding/binary"
	"math/big"
)

// Partial withdrawals (AUTO and OnRe). A withdrawal shortfall S = demand -
// Voltr idle - withdrawal cash already in flight frees only E = S + buffer
// of equity and keeps the position at its level: release the collateral
// share C*E/Eq, swap the debt share to debt and repay it (LTV back to the
// level), convert the rest to USDC and stage it to Voltr. A shortfall of
// >= 90% of equity, or a remainder below the minimum, keeps the installed
// full exit. The decision is stateless: each leg is chosen from the
// observed custody, so a restart resumes the same chain.
const (
	partialWithdrawalBufferBPS       int64 = 100        // 1% of S ...
	partialWithdrawalMinimumBuffer   int64 = 1_000_000  // ... at least $1
	partialWithdrawalFullExitBPS     int64 = 9_000      // E >= 90% equity -> full exit
	partialWithdrawalMinRemainingRaw int64 = 50_000_000 // keep >= $50 of equity
	// Collateral or cash below this is rounding residue, not a leg.
	partialWithdrawalDustRaw int64 = 1_000

	partialReleaseReason    = "withdrawal_partial_release"
	partialSwapToDebtReason = "withdrawal_partial_swap_to_debt"
	partialSwapToUSDCReason = "withdrawal_partial_swap_to_usdc"
	partialStageReason      = "withdrawal_partial_stage"
	partialDebtToUSDCReason = "withdrawal_partial_debt_to_usdc"
	// At most this many release rounds (each keeps LTV under the release
	// ceiling); a shortfall under 90% of equity at 1.5x needs <= 6. More
	// takes the full exit.
	partialWithdrawalMaxRounds = 6
)

// partialWithdrawalTargetLTVBPS is the LTV the position keeps: the stored
// level's (1.5x = 33.33%); a leveraged position with no stored level keeps
// 1.5x. A 1x target with debt is a down move, handled by the full chain.
func partialWithdrawalTargetLTVBPS(s Snapshot) (int64, bool) {
	if s.PositionDebtRaw <= 0 {
		return 0, true
	}
	if s.LeverageTargetLevel == 1 {
		return 0, false
	}
	if s.PartialWithdrawalOperationID != "" {
		return s.PartialWithdrawalLTVBPS, true
	}
	// Only capture with nothing in flight. Legacy in-flight positions keep
	// their old discrete target; new releases persist this actual ratio.
	if s.CollateralIdleRaw <= partialWithdrawalDustRaw && debtCashRaw(s) <= partialWithdrawalDustRaw && s.SquadsIdleRaw <= partialWithdrawalDustRaw {
		if s.PositionCollateralValueRaw <= s.PositionDebtValueRaw || s.PositionDebtValueRaw <= 0 {
			return 0, false
		}
		ratio := new(big.Int).Mul(big.NewInt(s.PositionDebtValueRaw), big.NewInt(10_000))
		ratio.Quo(ratio, big.NewInt(s.PositionCollateralValueRaw))
		if !ratio.IsInt64() || ratio.Int64() > leverageMaxLTVBPS {
			return 0, false
		}
		return ratio.Int64(), true
	}
	level := s.LeverageTargetLevel
	if level == 0 {
		level = 1.5
	}
	return leverageLevelLTVBPS(level), level >= 1.5
}

// partialWithdrawalStep returns the next partial-withdrawal leg, or ok=false
// for the installed full chain. Callers run it inside the withdrawal branch,
// after hard LTV, recovery and the covered/staged checks.
func partialWithdrawalStep(s Snapshot) (Action, string, int64, bool) {
	if !leverageLane(s.RouteLane) || !s.PilotActive || s.Unwind || s.CutoverDrain || (s.WithdrawalDemandRaw <= 0 && !partialWithdrawalInFlight(s)) || !s.HasPosition ||
		s.PositionCollateralRaw <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionDebtValueRaw < 0 || s.PositionDebtRaw < 0 {
		return "", "", 0, false
	}
	target, ok := partialWithdrawalTargetLTVBPS(s)
	if !ok {
		return "", "", 0, false
	}
	usdc := s.SquadsIdleRaw // withdrawal proceeds waiting to stage
	debtCash := int64(0)
	if sharedUSDCDebt(s.RouteLane) {
		debtCash = s.SquadsIdleRaw
	} else {
		debtCash = s.DebtIdleRaw
	}
	equity := s.PositionCollateralValueRaw - s.PositionDebtValueRaw
	inflight := usdc + s.CollateralIdleValueRaw
	if !sharedUSDCDebt(s.RouteLane) {
		inflight += s.DebtIdleRaw
	}
	shortfall := s.WithdrawalDemandRaw - s.VoltrIdleRaw
	if equity <= 0 || (shortfall <= 0 && !partialWithdrawalInFlight(s)) {
		return "", "", 0, false
	}
	// Full-exit fallbacks: the partial would take (nearly) everything or
	// leave less than the minimum. Judged on the whole shortfall, before any
	// leg, so an in-flight chain never flips to a full exit midway.
	total := equity + inflight
	if !partialWithdrawalInFlight(s) && (shortfall*10_000 >= total*partialWithdrawalFullExitBPS || total-shortfall < partialWithdrawalMinRemainingRaw) {
		return "", "", 0, false
	}
	// Bounded rounds: a shortfall a capped release cannot free within
	// partialWithdrawalMaxRounds rounds takes the full exit. Judged against
	// the whole vault position so the answer is stable mid-chain.
	if !partialWithdrawalInFlight(s) && !partialWithdrawalFitsRounds(s, shortfall, total) {
		return "", "", 0, false
	}
	overTarget := s.PositionDebtRaw > 0 && s.LTVBPS > target+leverageUpNearBPS
	// Decision amounts are stable across observe -> prepare (custody
	// balances or the USDC target); value-dependent raw sizes (receipts,
	// the exact repay) are computed in prepare and rechecked at admission.
	switch {
	case overTarget && debtCash > partialWithdrawalDustRaw:
		// Repay back to the level: R = D - target*C (never the whole debt);
		// the exact wire is exitPartialRepayWireRaw at prepare time.
		if min(debtCash, partialWithdrawalRepayRaw(s, target), s.PositionDebtRaw-exitCycleResidualFloor(s.PositionDebtRaw)) > 0 {
			return DeleverRouteStep, exitPartialRepayReason, debtCash, true
		}
	case overTarget && s.CollateralIdleRaw > partialWithdrawalDustRaw:
		if sharedUSDCDebt(s.RouteLane) {
			// USDC debt: one swap; the repay then the stage split the USDC.
			return SwapCollateralToStableStep, partialSwapToUSDCReason, s.CollateralIdleRaw, true
		}
		// The decision carries the idle collateral (stable); prepare sizes
		// the debt share with partialWithdrawalDebtSwapRaw.
		if partialWithdrawalDebtSwapRaw(s, target) > 0 {
			return SwapCollateralToDebtStep, partialSwapToDebtReason, s.CollateralIdleRaw, true
		}
	}
	if s.CollateralIdleRaw > partialWithdrawalDustRaw {
		return SwapCollateralToStableStep, partialSwapToUSDCReason, s.CollateralIdleRaw, true
	}
	if !sharedUSDCDebt(s.RouteLane) && s.DebtIdleRaw > partialWithdrawalDustRaw {
		// Leftover debt cash after the repay: convert, like the residue leg.
		return SwapDebtToUSDCStep, partialDebtToUSDCReason, s.DebtIdleRaw, true
	}
	if usdc > partialWithdrawalDustRaw && !overTarget {
		return StageSquadsToVoltr, partialStageReason, usdc, true
	}
	if inflight > partialWithdrawalDustRaw {
		return "", "", 0, false
	}
	if shortfall <= 0 {
		return "", "", 0, false
	}
	// Nothing in flight: release the collateral share for E = S + buffer.
	// The decision carries E (USDC raw, stable); prepare converts it to
	// receipts with partialWithdrawalReleaseReceipts, capped at the safe
	// release (a larger E runs further rounds).
	free := shortfall + max(shortfall*partialWithdrawalBufferBPS/10_000, partialWithdrawalMinimumBuffer)
	if partialWithdrawalReleaseReceipts(s, free) <= 0 {
		return "", "", 0, false
	}
	return DeleverRouteStep, partialReleaseReason, free, true
}

// partialWithdrawalReleaseReceipts converts the USDC target E into receipts
// at the snapshot's values: the collateral share C*E/Eq, capped so C - R >=
// D / (ceiling - 5 pts). The builder's safe-release bound is the hard cap.
func partialWithdrawalReleaseReceipts(s Snapshot, free int64) int64 {
	equity := s.PositionCollateralValueRaw - s.PositionDebtValueRaw
	if free <= 0 || equity <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionCollateralRaw <= 0 {
		return 0
	}
	release := new(big.Int).Mul(big.NewInt(free), big.NewInt(s.PositionCollateralValueRaw))
	release.Quo(release, big.NewInt(equity))
	if !release.IsInt64() {
		return 0
	}
	releaseValue := release.Int64()
	if s.PositionDebtValueRaw > 0 {
		limit := s.PositionCollateralValueRaw - s.PositionDebtValueRaw*10_000/(leverageExitReleaseCeilingBPS-500)
		if limit <= 0 {
			return 0
		}
		releaseValue = min(releaseValue, limit)
	}
	receipts := new(big.Int).Mul(big.NewInt(s.PositionCollateralRaw), big.NewInt(releaseValue))
	receipts.Quo(receipts, big.NewInt(s.PositionCollateralValueRaw))
	if !receipts.IsInt64() || receipts.Sign() <= 0 || receipts.Int64() >= s.PositionCollateralRaw {
		return 0
	}
	return receipts.Int64()
}

// exitPartialRepayWireRaw is the exact partial repay prepared from the
// current snapshot: all debt cash, never the whole debt (residual floor),
// and inside a partial withdrawal only the debt share back to the level.
func exitPartialRepayWireRaw(s Snapshot) int64 {
	amount := min(debtCashRaw(s), s.PositionDebtRaw-exitCycleResidualFloor(s.PositionDebtRaw))
	if _, reason, _, ok := partialWithdrawalStep(s); ok && reason == exitPartialRepayReason {
		if target, ok := partialWithdrawalTargetLTVBPS(s); ok {
			amount = min(amount, partialWithdrawalRepayRaw(s, target))
		}
	}
	return max(amount, 0)
}

// partialWithdrawalRepayRaw is the debt (raw) to repay so the position
// returns to the target LTV: D - target*C, in debt raw units via the
// snapshot's debt value per raw unit.
func partialWithdrawalRepayRaw(s Snapshot, target int64) int64 {
	targetDebtValue := s.PositionCollateralValueRaw * target / 10_000
	excess := s.PositionDebtValueRaw - targetDebtValue
	if excess <= 0 || s.PositionDebtValueRaw <= 0 {
		return 0
	}
	raw := new(big.Int).Mul(big.NewInt(s.PositionDebtRaw), big.NewInt(excess))
	raw.Quo(raw, big.NewInt(s.PositionDebtValueRaw))
	if !raw.IsInt64() {
		return 0
	}
	return raw.Int64()
}

// admitPartialWithdrawalLeg admits one leg of a partial withdrawal. Every
// leg prices the COMPLETE remaining exit of the whole position from the
// leg's cost-only poststate: the projected pricer (cycles if needed, final
// release -> swap -> payoff -> withdraw -> return) for a leveraged
// position, the post-payoff return for a debt-free one. ok=false: not a
// partial-withdrawal leg; the installed admission applies.
func admitPartialWithdrawalLeg(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, request any, effects ExpectedEffects) (phase3BridgeAdmission, error, bool) {
	switch d.Reason {
	case partialReleaseReason, partialSwapToDebtReason, partialSwapToUSDCReason, partialStageReason, partialDebtToUSDCReason:
	default:
		return phase3BridgeAdmission{}, nil, false
	}
	s := o.Snapshot
	// The decision amount is the stable driver; the request carries the wire
	// sized from the same snapshot, rechecked below.
	if want, reason, amount, ok := partialWithdrawalStep(s); !ok || want != d.Action || reason != d.Reason || amount != d.AmountRaw || !decisionsEqual(m.DecideOnManifest(s), d) {
		return phase3BridgeAdmission{}, budgetHold("partial_withdrawal_decision_changed"), true
	}
	route, err := runtimeRoute(s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err, true
	}
	if wire, sized := partialWithdrawalWireAmount(s, d); sized {
		var got uint64
		switch r := request.(type) {
		case KaminoPrimeUSDCRequest:
			got = r.AmountRaw
		case JupiterSwapRequest:
			got = r.AmountRaw
		}
		if wire <= 0 || got == 0 || got > uint64(wire) {
			return phase3BridgeAdmission{}, budgetHold("partial_withdrawal_wire_mismatch"), true
		}
	}
	current, err := m.observePhase3KnownBuildCost(ctx, rpc, request, effects)
	if err != nil {
		return phase3BridgeAdmission{}, err, true
	}
	var additional []string
	if route.Lane == autoAUTOPYUSD.Lane {
		additional = append(additional, route.Kamino.Market)
	}
	_, accounts, err := rpc.GetMultipleAccounts(ctx, payoffWindowAddresses(route, additional...), max(s.Slot, current.ObservationSlot))
	if err != nil {
		return phase3BridgeAdmission{}, err, true
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || obligation.collateralDepositedRaw != uint64(s.PositionCollateralRaw) {
		return phase3BridgeAdmission{}, budgetHold("partial_withdrawal_position_changed"), true
	}
	// The leg's measured effect on the custodies (cost-only poststate).
	post := append([]ConfirmedAccount(nil), accounts...)
	for _, e := range effects.Accounts {
		for i := range post {
			if post[i].Address == e.Address && len(post[i].Data) >= 72 && e.Mint != "" {
				post[i].Data = append([]byte(nil), post[i].Data...)
				binary.LittleEndian.PutUint64(post[i].Data[64:72], e.AfterRaw)
			}
		}
	}
	if d.Reason == partialReleaseReason {
		r, ok := request.(KaminoPrimeUSDCRequest)
		debit, err := m.measureExecutableDebit(request, effects)
		if !ok || err != nil || len(effects.Accounts) != 2 {
			return phase3BridgeAdmission{}, budgetHold("partial_withdrawal_release_mismatch"), true
		}
		if post, err = projectLeverageExitRelease(accounts, route, KaminoReleaseBound{ReceiptRaw: r.AmountRaw, LiquidityRaw: debit.Raw}, effects); err != nil {
			return phase3BridgeAdmission{}, err, true
		}
	}
	plan, err := pricePartialWithdrawalRemainder(ctx, rpc, client, m, o, d, request, effects, current, route, post)
	if err == nil {
		plan.Snapshot = s
	}
	return plan, err, true
}

// pricePartialWithdrawalRemainder prices the complete exit of whatever the
// poststate holds. A leveraged position goes through the projected pricer
// (which also counts idle collateral and debt cash); a debt-free one prices
// its withdraw -> swap -> return tail.
func pricePartialWithdrawalRemainder(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, request any, effects ExpectedEffects, current ValuedTransactionCost, route RuntimeRoute, post []ConfirmedAccount) (phase3BridgeAdmission, error) {
	slot := int64(0)
	if clock := accountAt(post, budgetClockAddress); len(clock.Data) == 40 {
		slot = int64(binary.LittleEndian.Uint64(clock.Data[:8]))
	}
	projection := phase3KaminoProjection{Slot: max(slot, o.Snapshot.Slot), Accounts: post}
	if o.Snapshot.PositionDebtRaw > 0 {
		return pricePhase3ProjectedPositionReturn(ctx, rpc, client, m, o, d, request, effects, current, projection)
	}
	return pricePhase3ProjectedDebtFreeReturn(ctx, rpc, client, m, o, d, request, effects, current, route, projection)
}

// pricePhase3ProjectedDebtFreeReturn prices a debt-free position's complete
// return from a cost-only poststate: withdraw every receipt -> NAV -> swap
// the collateral (and any idle) to USDC -> NAV -> stage -> restore -> NAV.
func pricePhase3ProjectedDebtFreeReturn(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, request any, effects ExpectedEffects, current ValuedTransactionCost, route RuntimeRoute, projection phase3KaminoProjection) (phase3BridgeAdmission, error) {
	obligation, err := decodeKaminoObligation(accountAt(projection.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || obligation.collateralDepositedRaw == 0 || obligation.debtRaw != 0 {
		return phase3BridgeAdmission{}, budgetHold("partial_withdrawal_remainder_unavailable")
	}
	reserve, err := decodeKaminoReserve(accountAt(projection.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	amount, err := reserve.redeemLiquidityRaw(obligation.collateralDepositedRaw)
	if err != nil || amount == 0 {
		return phase3BridgeAdmission{}, budgetHold("partial_withdrawal_remainder_unavailable")
	}
	blockhash, err := rpc.LatestBlockhash(ctx)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	withdrawal, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, obligation.collateralDepositedRaw, blockhash, route.Lane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	withdrawal.ObligationReserves = []string{route.Kamino.CollateralReserve}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	withdrawalEffects, err := exactKaminoTokenEffects(projection.Accounts, source, destination, amount)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	post := o
	post.Snapshot.HasPosition, post.Snapshot.PositionCollateralRaw = true, int64(obligation.collateralDepositedRaw)
	post.Snapshot.PositionDebtRaw, post.Snapshot.PositionDebtValueRaw = 0, 0
	idle := uint64(0)
	mint, _ := decodeBase58PublicKey(route.Kamino.CollateralMint)
	owner, _ := decodeBase58PublicKey(bridgeVault)
	if a := accountAt(projection.Accounts, route.CollateralCustody); len(a.Data) >= 72 {
		if custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner); err == nil {
			idle = custody.Raw
		}
	}
	post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = int64(idle), int64(idle)
	post.Snapshot.DebtIdleRaw = 0
	tail, err := observePhase3WithdrawalAdmission(ctx, rpc, client, m, post, Decision{Action: DeleverRouteStep, StrategyKey: route.Lane, Reason: "withdrawal_withdraw_collateral"}, KaminoExecutionEvidence{withdrawal, withdrawalEffects})
	if err != nil {
		return tail, err
	}
	if len(tail.Exit) == 0 || tail.Exit[0].Action != ReportNAV {
		return tail, budgetHold("partial_withdrawal_nav_unavailable")
	}
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		return tail, err
	}
	plan := tail
	plan.Snapshot, plan.Decision, plan.CurrentCost = o.Snapshot, d, current
	if plan.Input, err = encodePhase3BuildInput(request, encoded); err != nil {
		return plan, err
	}
	plan.PayoffWithdrawal = tail.Input
	plan.Exit = append([]phase3BridgeExitCost{{Action: ReportNAV, Cost: tail.Exit[0].Cost, Template: tail.Exit[0].Template}, {Action: DeleverRouteStep, Amount: withdrawal.AmountRaw, Cost: tail.CurrentCost, Template: tail.Input}}, tail.Exit...)
	plan.ExitAfterMicros = 0
	for _, step := range plan.Exit {
		if plan.ExitAfterMicros, err = budgetSum(plan.ExitAfterMicros, step.Cost.TotalMicros); err != nil {
			return plan, err
		}
	}
	plan.ValidThroughSlot = min(tail.ValidThroughSlot, current.ValidThroughSlot)
	return plan, nil
}

// partialWithdrawalFitsRounds: at the position's level (value units), each
// round releases up to C - D/(ceiling - 5 pts) and frees that times
// (1 - target LTV) of equity; the rounds shrink C and D proportionally. A
// debt-free position frees everything in one release.
func partialWithdrawalFitsRounds(s Snapshot, shortfall, total int64) bool {
	target, ok := partialWithdrawalTargetLTVBPS(s)
	if !ok {
		return false
	}
	if target == 0 {
		return true
	}
	need := shortfall + max(shortfall*partialWithdrawalBufferBPS/10_000, partialWithdrawalMinimumBuffer)
	// Per unit of equity at the level: C = 1/(1-t), D = t/(1-t).
	c := 10_000.0 / float64(10_000-target)
	d := float64(target) / float64(10_000-target)
	perRound := (c - d*10_000/float64(leverageExitReleaseCeilingBPS-500)) * float64(10_000-target) / 10_000
	if perRound <= 0 {
		return false
	}
	remaining := float64(total)
	freed := 0.0
	for round := 0; round < partialWithdrawalMaxRounds; round++ {
		step := remaining * perRound
		freed += step
		remaining -= step
		if freed >= float64(need) {
			return true
		}
	}
	return false
}

// partialWithdrawalDebtSwapRaw sizes the collateral swapped to debt: the
// repay's value plus 1% for swap loss, in collateral raw at the idle price.
func partialWithdrawalDebtSwapRaw(s Snapshot, target int64) int64 {
	repay := partialWithdrawalRepayRaw(s, target)
	if repay <= 0 || s.CollateralIdleValueRaw <= 0 || s.PositionDebtRaw <= 0 {
		return 0
	}
	value := new(big.Int).Mul(big.NewInt(repay), big.NewInt(s.PositionDebtValueRaw))
	value.Quo(value, big.NewInt(s.PositionDebtRaw))
	value.Mul(value, big.NewInt(10_000+leverageExitSwapLossBPS)).Quo(value, big.NewInt(10_000))
	raw := new(big.Int).Mul(big.NewInt(s.CollateralIdleRaw), value)
	raw.Quo(raw, big.NewInt(s.CollateralIdleValueRaw))
	if !raw.IsInt64() || raw.Sign() <= 0 {
		return 0
	}
	return min(raw.Int64(), s.CollateralIdleRaw)
}

// partialWithdrawalWireAmount maps a stable decision amount to the exact
// wire amount at prepare time, from the prepared snapshot. ok=false: the
// decision's own amount is the wire amount.
func partialWithdrawalWireAmount(s Snapshot, d Decision) (int64, bool) {
	switch d.Reason {
	case partialReleaseReason:
		return partialWithdrawalReleaseReceipts(s, d.AmountRaw), true
	case partialSwapToDebtReason:
		target, ok := partialWithdrawalTargetLTVBPS(s)
		if !ok {
			return 0, true
		}
		return partialWithdrawalDebtSwapRaw(s, target), true
	case exitPartialRepayReason:
		return exitPartialRepayWireRaw(s), true
	case leverageDownPartialReleaseReason:
		return leverageDownPartialReceipts(s), true
	}
	return 0, false
}
