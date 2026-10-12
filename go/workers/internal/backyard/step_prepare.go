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

// The down steps' reasons. A leverage_up or term_keepalive borrow is an up
// step; a partial withdrawal's withdrawal_partial_release is a down step
// except on an overflow source, whose release repays nothing.
const (
	hardLTVDeleverReason      = "hard_ltv_delever"
	leverageDownReason        = "leverage_down"
	leverageDownPartialReason = "leverage_down_partial"
	withdrawalDeleverReason   = "withdrawal_delever"
)

const (
	// stepSwapLossBPS is the margin a down step converts over the debt it
	// means to repay: the swap's slippage and the prices' drift.
	stepSwapLossBPS = 100
	// stepWithdrawMarginBPS keeps a down step's withdrawal this far below the
	// obligation's own borrow limit, which KLend checks right after it.
	stepWithdrawMarginBPS = 300
)

// stepDecision reports whether d is an atomic step of s's lane, and whether
// it is an up step.
func stepDecision(s Snapshot, d Decision) (up bool, ok bool) {
	if !earnHeldLane(d.StrategyKey) {
		return false, false
	}
	switch d.Action {
	case OpenRouteStep:
		return true, d.Reason == leverageUpReason || d.Reason == termKeepAliveReason
	case DeleverRouteStep:
		switch d.Reason {
		case hardLTVDeleverReason, leverageDownReason, leverageDownPartialReason, withdrawalDeleverReason:
			return false, true
		case partialReleaseReason:
			return false, !overflowSourceHalf(s)
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

// stepState is what a step is sized from: the tick's batch for its lane.
type stepState struct {
	route                          RuntimeRoute
	position                       KaminoPosition
	collateral                     decodedKaminoReserve
	obligation                     kamino.Obligation
	heldCollateral, heldDebt, slot uint64
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
	if st.heldDebt, err = stepCustodyRaw(accounts, route.DebtCustody, route.Kamino.DebtMint); err != nil {
		return stepState{}, err
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
		return 0, budgetHoldFrom(err, "step_no_withdraw_room")
	}
	return min(receipts, stepLegCap(st.route)), nil
}

// planDownStep sizes a down step from decision: the receipts it withdraws
// and the collateral it converts. A partial withdrawal frees its equity E
// (the decision amount) at the position's own level: it withdraws the
// collateral share C*E/Eq and converts only the debt share of it, so the
// rest waits as collateral for the withdrawal's exit legs. Every other down
// step repays the decision amount of debt: it converts the withdrawal and
// the held collateral, which covers that debt less the held debt, plus
// stepSwapLossBPS.
func planDownStep(s Snapshot, d Decision, st stepState, repayRaw uint64) (uint64, uint64, error) {
	limit, err := st.stepWithdrawReceipts()
	if err != nil {
		return 0, 0, err
	}
	if d.Reason == partialReleaseReason {
		receipts := uint64(max(partialWithdrawalReleaseReceipts(s, d.AmountRaw), 0))
		receipts = min(receipts, limit)
		liquidity, err := st.collateral.redeemLiquidityRaw(receipts)
		if err != nil || liquidity == 0 || s.PositionCollateralValueRaw <= 0 {
			return 0, 0, budgetHoldFrom(err, "partial_withdrawal_leg_unavailable")
		}
		convert := new(big.Int).Mul(new(big.Int).SetUint64(liquidity), big.NewInt(s.PositionDebtValueRaw))
		convert.Mul(convert, big.NewInt(10_000+stepSwapLossBPS))
		convert.Quo(convert, new(big.Int).Mul(big.NewInt(s.PositionCollateralValueRaw), big.NewInt(10_000)))
		if !convert.IsUint64() || convert.Sign() <= 0 {
			return 0, 0, budgetHold("partial_withdrawal_leg_unavailable")
		}
		return receipts, min(convert.Uint64(), liquidity+st.heldCollateral), nil
	}
	if repayRaw <= st.heldDebt {
		return 0, 0, budgetHold("step_down_nothing_to_convert")
	}
	p := st.position
	need, err := valueBetweenTokenRaw(repayRaw-st.heldDebt, p.DebtDecimals, p.CollateralDecimals, p.DebtPriceSF, p.CollateralPriceSF, true)
	if err != nil {
		return 0, 0, err
	}
	margined := new(big.Int).Mul(new(big.Int).SetUint64(need), big.NewInt(10_000+stepSwapLossBPS))
	margined.Quo(margined, big.NewInt(10_000))
	if !margined.IsUint64() {
		return 0, 0, budgetHold("step_amounts_invalid")
	}
	if need = margined.Uint64(); need <= st.heldCollateral {
		return 0, 0, budgetHold("step_down_nothing_to_convert")
	}
	receipts, err := payoffReceiptsForLiquidity(st.collateral.totalLiquiditySF, st.collateral.collateralMintSupply, need-st.heldCollateral)
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
	if up {
		borrow := uint64(decision.AmountRaw)
		if decision.Reason == termKeepAliveReason {
			err = validateTermKeepAliveBorrow(st.position, accounts, route, borrow)
		} else if capped, capErr := capacitySizedBorrow(st.position, accounts, route, int64(leverageUpLevel(s)*100)); capErr != nil || borrow > capped || borrow < leverageMinimumBorrowRaw {
			err = budgetHoldFrom(capErr, "borrow_capacity_shrank")
		}
		if err != nil {
			return Observation{}, StepExecutionEvidence{}, err
		}
		evidence, _, err := assembleStep(ctx, rpc, view, client, manifest, observation.policies, st, true, min(borrow, stepLegCap(route)), borrow, s.Slot)
		logStage("prepare_step", prepareStart)
		return observation, evidence, err
	}
	repay, full := uint64(decision.AmountRaw), uint64(decision.AmountRaw) >= uint64(s.PositionDebtRaw)
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
	collateral := effect(route.CollateralCustody, route.Kamino.CollateralMint, route.CollateralTokenProgram, st.heldCollateral, 0)
	debt := effect(route.DebtCustody, route.Kamino.DebtMint, route.DebtTokenProgram, st.heldDebt, st.heldDebt)
	if !up {
		collateral.AfterRaw = st.heldCollateral + openLiquidity - convertRaw
		debt.AfterRaw = 0
	}
	return StepExecutionEvidence{Request: request, ExpectedEffects: stepEffects(up, collateral, debt)}, guaranteed, nil
}
