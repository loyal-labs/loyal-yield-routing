package backyard

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// The hard-LTV down step's reason. A leverage_up or term_keepalive borrow is
// an up step; debt_zero, leverage_down_partial and (beside debt)
// withdrawal_release are down steps; overflow_move is a move.
const hardLTVDeleverReason = "hard_ltv_delever"

const (
	// stepDownMarker is the amount of every down step's decision but a
	// release's (which carries its equity, stable): prepare sizes the repay
	// from the same snapshot (stepDownRepay), so re-deciding on a refreshed
	// snapshot keeps the decision equal.
	stepDownMarker = 1
	// stepSwapLossBPS is the margin a down step converts over the debt it
	// means to repay: the swap's slippage and the prices' drift.
	stepSwapLossBPS = 100
	// stepWithdrawMarginBPS keeps a down step's withdrawal this far below the
	// obligation's own borrow limit, which KLend checks right after it.
	stepWithdrawMarginBPS = 300
)

// stepDecision reports whether d is an atomic step of s's lane, and whether
// it is an up step. An overflow move is a step that is not up.
func stepDecision(s Snapshot, d Decision) (up bool, ok bool) {
	if !earnHeldLane(d.StrategyKey) {
		return false, false
	}
	switch d.Action {
	case OpenRouteStep:
		return true, d.Reason == leverageUpReason || d.Reason == termKeepAliveReason
	case DeleverRouteStep:
		switch d.Reason {
		case hardLTVDeleverReason, debtZeroReason, leverageDownPartialReason, overflowMoveReason:
			return false, true
		case releaseReason:
			// Beside no debt a release is one withdrawal.
			return false, s.PositionDebtRaw > 0
		}
	}
	return false, false
}

// stepDebtToLTVRaw is the debt (raw) a down step repays to bring s to ltvBPS:
// repaying R of value with R of collateral leaves (D-R)/(C-R) = t, so
// R = (D - tC)/(1 - t). Zero when s is at or under it.
func stepDebtToLTVRaw(s Snapshot, ltvBPS int64) int64 {
	if s.PositionDebtRaw <= 0 || s.PositionDebtValueRaw <= 0 || ltvBPS <= 0 || ltvBPS >= 10_000 {
		return 0
	}
	value := new(big.Int).Mul(big.NewInt(s.PositionDebtValueRaw), big.NewInt(10_000))
	value.Sub(value, new(big.Int).Mul(big.NewInt(s.PositionCollateralValueRaw), big.NewInt(ltvBPS)))
	if value.Sign() <= 0 {
		return 0
	}
	value.Quo(value, big.NewInt(10_000-ltvBPS))
	raw := value.Mul(value, big.NewInt(s.PositionDebtRaw)).Quo(value, big.NewInt(s.PositionDebtValueRaw))
	if !raw.IsInt64() {
		return s.PositionDebtRaw
	}
	return min(raw.Int64(), s.PositionDebtRaw)
}

// stepDownRepay is the debt a down step of reason repays, from the snapshot
// it was decided on: to the stored level's LTV after hard LTV, to 1.5x for
// the partial down move, all of it (full, sized on the payoff bound) for
// debt 0. A release repays its debt share (planDownStep).
func stepDownRepay(s Snapshot, reason string) (uint64, bool) {
	switch reason {
	case debtZeroReason:
		return uint64(max(s.PositionDebtRaw, s.PayoffDebtRaw)), true
	case hardLTVDeleverReason:
		return uint64(max(stepDebtToLTVRaw(s, hardLTVTargetBPS(s)), 0)), false
	case leverageDownPartialReason:
		return uint64(max(stepDebtToLTVRaw(s, leverageLevelLTVBPS(1.5)), 0)), false
	}
	return 0, false
}

// stepState is what a step is sized from: the tick's batch for its lane.
type stepState struct {
	route                          RuntimeRoute
	position                       KaminoPosition
	collateral                     decodedKaminoReserve
	obligation                     kamino.Obligation
	heldCollateral, heldDebt, slot uint64
	// The custody balances the step's effects start from. They equal the
	// held amounts except on a shared-USDC-debt lane, whose debt custody is
	// the Squads bridge account: its cash is the bridge's, never a step's.
	collateralBalance, debtBalance uint64
	// topology is the obligation's reserves before the step, as its first
	// refresh names them.
	topology []string
}

func readStepState(accounts []ConfirmedAccount, route RuntimeRoute, slot int64) (stepState, error) {
	position, err := observeKaminoFromFixedAccounts(slot, accounts, route.Kamino)
	if err != nil {
		return stepState{}, err
	}
	collateral, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return stepState{}, err
	}
	obligation, err := kamino.DecodeObligation(chainAccount(accountAt(accounts, route.Kamino.Obligation), route.Kamino.Obligation))
	if err != nil {
		return stepState{}, err
	}
	st := stepState{route: route, position: position, collateral: collateral, obligation: obligation, slot: uint64(slot)}
	if st.heldCollateral, err = stepCustodyRaw(accounts, route.CollateralCustody, route.Kamino.CollateralMint); err != nil {
		return stepState{}, err
	}
	if st.debtBalance, err = stepCustodyRaw(accounts, route.DebtCustody, route.Kamino.DebtMint); err != nil {
		return stepState{}, err
	}
	st.collateralBalance, st.heldDebt = st.heldCollateral, st.debtBalance
	if sharedUSDCDebt(route.Lane) {
		st.heldDebt = 0
	}
	if position.CollateralDepositedRaw > 0 {
		st.topology = append(st.topology, route.Kamino.CollateralReserve)
	}
	if position.DebtRaw > 0 {
		st.topology = append(st.topology, route.Kamino.DebtReserve)
	}
	return st, nil
}

// stepCustodyRaw is the vault's balance in its custody of mint.
func stepCustodyRaw(accounts []ConfirmedAccount, address, mint string) (uint64, error) {
	account := accountAt(accounts, address)
	custody, err := DecodeTokenCustody(account.Owner, account.Data, mustKey(mint), mustKey(bridgeVault))
	if err != nil {
		return 0, fmt.Errorf("custody %s: %w", address, err)
	}
	return custody.Raw, nil
}

// stepLegCap is the most a KLend leg of route may carry: a split policy caps
// every leg at the vault cap; the AUTO and basic policies leave it free.
func stepLegCap(route RuntimeRoute) uint64 {
	if key, _ := kaminoPolicyLeg(route, kaminoLegDeposit); key.leg != 0 {
		return bridgeCapRaw
	}
	return math.MaxUint64
}

// stepWithdrawReceipts is the most a down step may withdraw: the
// obligation's own borrow limit (its allowed borrow value over its deposits,
// read back to plain debt through its borrow factors, elevation group
// included) less stepWithdrawMarginBPS, as receipts at the batch's prices.
func (st stepState) stepWithdrawReceipts() (uint64, error) {
	o := st.obligation
	deposited, allowed := littleInt(o.DepositedValueSF[:]), littleInt(o.AllowedBorrowValueSF[:])
	adjusted, plain := littleInt(o.BorrowFactorAdjustedDebtValueSF[:]), littleInt(o.BorrowedAssetsMarketValueSF[:])
	if deposited.Sign() <= 0 || adjusted.Sign() <= 0 || plain.Sign() <= 0 || allowed.Sign() <= 0 {
		return 0, budgetHold("step_withdraw_limit_unavailable")
	}
	ceiling := new(big.Int).Mul(allowed, plain)
	ceiling.Mul(ceiling, big.NewInt(10_000)).Quo(ceiling, new(big.Int).Mul(deposited, adjusted))
	if !ceiling.IsInt64() || ceiling.Int64() <= stepWithdrawMarginBPS {
		return 0, budgetHold("step_withdraw_limit_unavailable")
	}
	receipts, _, err := withdrawExcessAtLTV(st.position, uint64(ceiling.Int64()-stepWithdrawMarginBPS))
	if err != nil {
		// Only outside cash can repay now: the target's no_room, counted on
		// /metrics where its alert lives.
		return 0, &BudgetHold{Reason: holdNoRoom, Details: map[string]string{"cause": "step_no_withdraw_room"}, cause: err}
	}
	return min(receipts, stepLegCap(st.route)), nil
}

// planDownStep sizes a down step from decision: the receipts it withdraws
// and the collateral it converts. A release frees its equity E (the
// decision amount) at the position's own level: it withdraws the collateral
// share C*E/Eq and converts only the debt share of it, so the rest waits as
// collateral for the withdrawal's exit legs. Every other down step repays
// the decision amount of debt: it converts the withdrawal and the held
// collateral, which covers that debt less the held debt, plus
// stepSwapLossBPS.
func planDownStep(s Snapshot, d Decision, st stepState, repayRaw uint64) (uint64, uint64, error) {
	limit, err := st.stepWithdrawReceipts()
	if err != nil {
		return 0, 0, err
	}
	if d.Reason == releaseReason {
		receipts := uint64(max(partialWithdrawalReleaseReceipts(s, d.AmountRaw), 0))
		receipts = min(receipts, limit)
		liquidity, err := st.collateral.redeemLiquidityRaw(receipts)
		if err != nil || liquidity == 0 || s.PositionCollateralValueRaw <= 0 {
			return 0, 0, budgetHoldFrom(err, "release_leg_unavailable")
		}
		convert := new(big.Int).Mul(new(big.Int).SetUint64(liquidity), big.NewInt(s.PositionDebtValueRaw))
		convert.Mul(convert, big.NewInt(10_000+stepSwapLossBPS))
		convert.Quo(convert, new(big.Int).Mul(big.NewInt(s.PositionCollateralValueRaw), big.NewInt(10_000)))
		if !convert.IsUint64() || convert.Sign() <= 0 {
			return 0, 0, budgetHold("release_leg_unavailable")
		}
		return receipts, min(convert.Uint64(), liquidity+st.heldCollateral), nil
	}
	// Held debt shortens what the conversion must cover, and held collateral
	// what the withdrawal must free; when held custody alone covers it the
	// step still converts the whole amount, repaying a little more.
	short := repayRaw
	if st.heldDebt < repayRaw {
		short -= st.heldDebt
	}
	p := st.position
	need, err := valueBetweenTokenRaw(short, p.DebtDecimals, p.CollateralDecimals, p.DebtPriceSF, p.CollateralPriceSF, true)
	if err != nil {
		return 0, 0, err
	}
	margined := new(big.Int).Mul(new(big.Int).SetUint64(need), big.NewInt(10_000+stepSwapLossBPS))
	margined.Quo(margined, big.NewInt(10_000))
	if !margined.IsUint64() {
		return 0, 0, budgetHold("step_amounts_invalid")
	}
	if need = margined.Uint64(); need > st.heldCollateral {
		need -= st.heldCollateral
	}
	receipts, err := payoffReceiptsForLiquidity(st.collateral.totalLiquiditySF, st.collateral.collateralMintSupply, need)
	if err != nil {
		return 0, 0, err
	}
	receipts = min(receipts, limit, p.CollateralDepositedRaw-1)
	liquidity, err := st.collateral.redeemLiquidityRaw(receipts)
	if err != nil || liquidity == 0 {
		return 0, 0, budgetHoldFrom(err, "step_no_withdraw_room")
	}
	return receipts, liquidity + st.heldCollateral, nil
}

// prepareStepFromTickObservation builds decision's atomic step from the
// tick's own batch: it sizes the open leg (the capacity-sized borrow, or the
// down step's withdrawal), quotes the conversion at the route size the step
// leaves, and closes with the held custody plus the conversion's guaranteed
// output. A down step that repays the whole debt is sized on the payoff's
// upper bound and requoted once when the first quote falls short of it.
func prepareStepFromTickObservation(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, client *jupiter.Client, decision Decision, observation Observation) (Observation, StepExecutionEvidence, error) {
	up, ok := stepDecision(observation.Snapshot, decision)
	if !ok || rpc == nil || view == nil || observation.routeBatch == nil || decision.AmountRaw <= 0 {
		return Observation{}, StepExecutionEvidence{}, fmt.Errorf("invalid step evidence request")
	}
	prepareStart, s, accounts := time.Now(), observation.Snapshot, observation.routeBatch.Accounts
	route, err := runtimeRoute(decision.StrategyKey)
	if err != nil {
		return Observation{}, StepExecutionEvidence{}, err
	}
	if observation.policies, err = observeInstalledPolicies(ctx, rpc, s.Slot); err != nil {
		return Observation{}, StepExecutionEvidence{}, err
	}
	st, err := readStepState(accounts, route, s.Slot)
	if err != nil {
		return Observation{}, StepExecutionEvidence{}, err
	}
	if decision.Reason == overflowMoveReason {
		evidence, err := assembleMove(ctx, rpc, manifest, observation.policies, accounts, st, s, decision)
		logStage("prepare_step", prepareStart)
		return observation, evidence, err
	}
	if up {
		borrow := uint64(decision.AmountRaw)
		if decision.Reason == termKeepAliveReason {
			err = validateTermKeepAliveBorrow(st.position, accounts, route, borrow)
		} else if capped, capErr := capacitySizedBorrow(st.position, accounts, route, int64(leverageUpLevel(s)*100)); capErr != nil || borrow > capped || borrow < leverageMinimumBorrowRaw {
			err = budgetHoldFrom(capErr, "borrow_capacity_shrank")
		} else {
			// A split policy caps each KLend leg: the borrow, and the deposit
			// of its conversion, stay a tenth under the cap.
			borrow, err = stepUpBorrowWithinCap(st, borrow)
		}
		if err != nil {
			return Observation{}, StepExecutionEvidence{}, err
		}
		// The conversion also takes the lane's held borrowed token, the
		// excess a previous step's conversion left.
		evidence, _, err := assembleStep(ctx, rpc, view, client, manifest, observation.policies, st, true, borrow, borrow+st.heldDebt, s.Slot)
		logStage("prepare_step", prepareStart)
		return observation, evidence, err
	}
	repay, full := stepDownRepay(s, decision.Reason)
	// Under a split policy's leg cap a repay leaves a tenth for the
	// conversion's excess; the step then repeats while debt remains.
	if limit := stepLegCap(route); repay > limit-limit/10 {
		repay, full = limit-limit/10, false
	}
	if full {
		bound, _, err := observeRawFullPayoff(ctx, view, route, s.Slot)
		if err != nil {
			return Observation{}, StepExecutionEvidence{}, err
		}
		repay = max(repay, bound.UpperDebtRaw)
	}
	receipts, convert, err := planDownStep(s, decision, st, repay)
	if err != nil {
		return Observation{}, StepExecutionEvidence{}, err
	}
	evidence, guaranteed, err := assembleStep(ctx, rpc, view, client, manifest, observation.policies, st, false, receipts, convert, s.Slot)
	if err == nil && full && st.heldDebt+guaranteed < repay {
		// Scale the conversion by its own quote to cover the payoff, within
		// the withdrawal limit, and quote once more.
		scaled := new(big.Int).Mul(new(big.Int).SetUint64(convert), new(big.Int).SetUint64(repay-st.heldDebt))
		scaled.Mul(scaled, big.NewInt(10_001)).Quo(scaled, new(big.Int).Mul(new(big.Int).SetUint64(guaranteed), big.NewInt(10_000)))
		if scaled.IsUint64() && scaled.Uint64() > st.heldCollateral {
			var more uint64
			if more, err = payoffReceiptsForLiquidity(st.collateral.totalLiquiditySF, st.collateral.collateralMintSupply, scaled.Uint64()-st.heldCollateral); err == nil {
				limit, _ := st.stepWithdrawReceipts()
				if more = min(more, limit, st.position.CollateralDepositedRaw-1); more > receipts {
					liquidity, redeemErr := st.collateral.redeemLiquidityRaw(more)
					if redeemErr == nil && liquidity > 0 {
						evidence, _, err = assembleStep(ctx, rpc, view, client, manifest, observation.policies, st, false, more, liquidity+st.heldCollateral, s.Slot)
					}
				}
			}
		}
	}
	logStage("prepare_step", prepareStart)
	return observation, evidence, err
}

// assembleStep is one step of st's lane: Open borrows (up) or withdraws
// (down) openRaw, the conversion converts convertRaw at the route size the
// step's other accounts leave, and Close deposits or repays the held
// destination custody plus the conversion's guaranteed output, which it
// returns. Every leg carries the conversion's blockhash.
func assembleStep(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, policies installedPolicies, st stepState, up bool, openRaw, convertRaw uint64, slot int64) (StepExecutionEvidence, uint64, error) {
	route := st.route
	lane := route.Lane
	action, conversion, openLeg, closeLeg := DeleverRouteStep, SwapCollateralToDebtStep, kaminoLegWithdraw, kaminoLegRepay
	sourceHeld, destinationHeld, openLiquidity := st.heldCollateral, st.heldDebt, uint64(0)
	if up {
		action, conversion, openLeg, closeLeg = OpenRouteStep, SwapDebtToCollateralStep, kaminoLegBorrow, kaminoLegDeposit
		sourceHeld, destinationHeld, openLiquidity = st.heldDebt, st.heldCollateral, openRaw
	} else {
		liquidity, err := st.collateral.redeemLiquidityRaw(openRaw)
		if err != nil {
			return StepExecutionEvidence{}, 0, err
		}
		openLiquidity = liquidity
	}
	if openRaw > stepLegCap(route) {
		return StepExecutionEvidence{}, 0, budgetHold("step_over_policy_cap")
	}
	if openRaw == 0 || convertRaw == 0 || convertRaw > math.MaxInt64 || sourceHeld > math.MaxUint64-openLiquidity || convertRaw > sourceHeld+openLiquidity {
		return StepExecutionEvidence{}, 0, budgetHold("step_amounts_invalid")
	}
	// The route size: the key budget the two KLend legs and the swap's fixed
	// accounts leave.
	placeholder := LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 1}
	open, err := manifest.kaminoPacketForRoute(policies, action, openLeg, openRaw, placeholder, lane)
	if err != nil {
		return StepExecutionEvidence{}, 0, err
	}
	open.ObligationReserves = st.topology
	closing, err := manifest.kaminoPacketForRoute(policies, action, closeLeg, 1, placeholder, lane)
	if err != nil {
		return StepExecutionEvidence{}, 0, err
	}
	request := StepRequest{Open: open, Close: closing}
	decision := Decision{Action: conversion, AmountRaw: int64(convertRaw), StrategyKey: lane}
	var conversionRequest any
	if conversionVenueHastra(decision) {
		e, err := prepareHastraQuoteEvidence(ctx, rpc, view, policies, decision, sourceHeld+openLiquidity, destinationHeld, slot)
		if err != nil {
			return StepExecutionEvidence{}, 0, err
		}
		request.Hastra, conversionRequest = &e.Request, e.Request
	} else {
		maxAccounts, err := stepSwapMaxAccounts(request, conversion)
		if err != nil {
			return StepExecutionEvidence{}, 0, err
		}
		e, err := prepareJupiterQuoteEvidenceAt(ctx, rpc, client, policies, decision, sourceHeld+openLiquidity, destinationHeld, slot, maxAccounts)
		if err != nil {
			return StepExecutionEvidence{}, 0, err
		}
		request.Swap, conversionRequest = &e.Request, e.Request
	}
	guaranteed, err := stepConversionOutput(conversionRequest)
	if err != nil {
		return StepExecutionEvidence{}, 0, err
	}
	closeRaw, err := stepCloseAmount(destinationHeld, conversionRequest)
	if err != nil {
		return StepExecutionEvidence{}, 0, err
	}
	if closeRaw > stepLegCap(route) {
		return StepExecutionEvidence{}, 0, budgetHold("step_over_policy_cap")
	}
	hash, height := stepConversionBlockhash(conversionRequest)
	blockhash := LatestBlockhash{Blockhash: hash, LastValidBlockHeight: height}
	if request.Open, err = manifest.kaminoPacketForRoute(policies, action, openLeg, openRaw, blockhash, lane); err != nil {
		return StepExecutionEvidence{}, 0, err
	}
	request.Open.ObligationReserves = st.topology
	if request.Close, err = manifest.kaminoPacketForRoute(policies, action, closeLeg, closeRaw, blockhash, lane); err != nil {
		return StepExecutionEvidence{}, 0, err
	}
	if _, err := compileStepMessageForDelegate(request, mustKey(bridgeDelegate)); err != nil {
		return StepExecutionEvidence{}, 0, budgetHoldFrom(err, "step_does_not_compile")
	}
	effect := func(custody, mint, program string, before, after uint64) ExpectedAccountEffect {
		return ExpectedAccountEffect{Address: custody, Owner: program, Mint: mint, Authority: bridgeVault, BeforeRaw: before, AfterRaw: after}
	}
	// Up: the borrow and the held debt convert exactly, and the deposit takes
	// the held collateral plus the minimum output. Down: the withdrawal less
	// the conversion stays as collateral, and the repay takes the held debt
	// plus the minimum output. Conversion excess stays beside them.
	collateral := effect(route.CollateralCustody, route.Kamino.CollateralMint, route.CollateralTokenProgram, st.collateralBalance, st.collateralBalance-st.heldCollateral)
	debt := effect(route.DebtCustody, route.Kamino.DebtMint, route.DebtTokenProgram, st.debtBalance, st.debtBalance+openLiquidity-convertRaw)
	if !up {
		collateral.AfterRaw = st.collateralBalance + openLiquidity - convertRaw
		debt.AfterRaw = st.debtBalance - st.heldDebt
	}
	return StepExecutionEvidence{Request: request, ExpectedEffects: stepEffects(up, collateral, debt)}, guaranteed, nil
}

// stepUpBorrowWithinCap sizes an up step under a split policy's leg cap: the
// borrow, and the deposit its conversion makes beside the held collateral,
// stay a tenth under the cap, so an up step larger than the cap borrows the
// cap's share instead of holding.
func stepUpBorrowWithinCap(st stepState, borrow uint64) (uint64, error) {
	limit := stepLegCap(st.route)
	if limit == math.MaxUint64 {
		return borrow, nil
	}
	limit -= limit / 10
	borrow = min(borrow, limit)
	p := st.position
	if limit <= st.heldCollateral {
		return 0, budgetHold("step_over_policy_cap")
	}
	collateral, err := valueBetweenTokenRaw(borrow, p.DebtDecimals, p.CollateralDecimals, p.DebtPriceSF, p.CollateralPriceSF, true)
	if err != nil {
		return 0, err
	}
	if collateral > limit-st.heldCollateral {
		if borrow, err = valueBetweenTokenRaw(limit-st.heldCollateral, p.CollateralDecimals, p.DebtDecimals, p.CollateralPriceSF, p.DebtPriceSF, false); err != nil {
			return 0, err
		}
	}
	if borrow < leverageMinimumBorrowRaw {
		return 0, budgetHold("step_over_policy_cap")
	}
	return borrow, nil
}

// assembleMove is an overflow move of st's lane: it withdraws the receipts
// worth the decision's equity (within the live cap's LTV and the
// obligation's withdraw room) and deposits what they redeem, with any
// collateral already in the shared custody, into the overflow lane's
// obligation, in one transaction.
func assembleMove(ctx context.Context, rpc *chain.Client, manifest RouteManifest, policies installedPolicies, accounts []ConfirmedAccount, st stepState, s Snapshot, decision Decision) (StepExecutionEvidence, error) {
	route := st.route
	lane, ok := overflowLaneOf(route.Lane)
	if !ok {
		return StepExecutionEvidence{}, fmt.Errorf("lane %s has no overflow lane", route.Lane)
	}
	destination, err := runtimeRoute(lane)
	if err != nil {
		return StepExecutionEvidence{}, err
	}
	if destination.CollateralCustody != route.CollateralCustody || destination.Kamino.CollateralReserve != route.Kamino.CollateralReserve {
		return StepExecutionEvidence{}, fmt.Errorf("an overflow lane shares its source's collateral")
	}
	limit, err := st.stepWithdrawReceipts()
	if err != nil {
		return StepExecutionEvidence{}, err
	}
	receipts := min(uint64(max(overflowReleaseReceipts(s, decision.AmountRaw), 0)), limit, st.position.CollateralDepositedRaw-1)
	liquidity, err := st.collateral.redeemLiquidityRaw(receipts)
	if err != nil || receipts == 0 || liquidity == 0 {
		return StepExecutionEvidence{}, budgetHoldFrom(err, holdNoRoom)
	}
	deposit := st.heldCollateral + liquidity
	if receipts > stepLegCap(route) || deposit > stepLegCap(destination) {
		return StepExecutionEvidence{}, budgetHold("step_over_policy_cap")
	}
	// The overflow obligation's reserves before the deposit, as its refresh
	// names them.
	account := accountAt(accounts, destination.Kamino.Obligation)
	if account.Address != destination.Kamino.Obligation || account.Lamports == 0 {
		return StepExecutionEvidence{}, budgetHold("overflow_obligation_absent")
	}
	obligation, err := decodeKaminoObligation(account, destination.Kamino)
	if err != nil {
		return StepExecutionEvidence{}, err
	}
	topology := []string{}
	if obligation.collateralDepositedRaw > 0 {
		topology = append(topology, destination.Kamino.CollateralReserve)
	}
	if obligation.debtRaw > 0 {
		topology = append(topology, destination.Kamino.DebtReserve)
	}
	blockhash, err := latestBlockhash(ctx, rpc)
	if err != nil {
		return StepExecutionEvidence{}, err
	}
	open, err := manifest.kaminoPacketForRoute(policies, DeleverRouteStep, kaminoLegWithdraw, receipts, blockhash, route.Lane)
	if err != nil {
		return StepExecutionEvidence{}, err
	}
	open.ObligationReserves = st.topology
	closing, err := manifest.kaminoPacketForRoute(policies, DeleverRouteStep, kaminoLegDeposit, deposit, blockhash, destination.Lane)
	if err != nil {
		return StepExecutionEvidence{}, err
	}
	closing.ObligationReserves = topology
	request := StepRequest{Open: open, Close: closing}
	if _, err := compileStepMessageForDelegate(request, mustKey(bridgeDelegate)); err != nil {
		return StepExecutionEvidence{}, budgetHoldFrom(err, "step_does_not_compile")
	}
	// The shared custody gives up what it held: the withdrawal arrives and
	// leaves in the same transaction.
	after := st.collateralBalance - st.heldCollateral
	collateral := ExpectedAccountEffect{Address: route.CollateralCustody, Owner: route.CollateralTokenProgram, Mint: route.Kamino.CollateralMint, Authority: bridgeVault,
		BeforeRaw: st.collateralBalance, AfterRaw: after, MinimumAfterRaw: &after}
	return StepExecutionEvidence{Request: request, ExpectedEffects: ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "atomic-step", Conserved: false, Accounts: []ExpectedAccountEffect{collateral}}}, nil
}
