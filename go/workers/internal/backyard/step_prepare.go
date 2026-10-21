package backyard

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

const hardLTVDeleverReason = "hard_ltv_delever"

const (
	// stepDownMarker is the amount of every down step's decision but a
	// release's (which carries its equity, stable): prepare sizes the repay
	// from the same snapshot (stepDownRepay), so re-deciding on a refreshed
	// snapshot keeps the decision equal.
	stepDownMarker  = 1
	stepSwapLossBPS = 100
	// stepWithdrawMarginBPS keeps a down step's withdrawal this far below the
	// obligation's own borrow limit, which KLend checks right after it.
	stepWithdrawMarginBPS = 300
)

type StepKind uint8

const (
	notStep StepKind = iota
	stepUp
	stepDown
	stepMove
	stepRelease
	stepExit
)

func stepKind(d Decision) StepKind {
	if !earnHeldLane(d.StrategyKey) {
		return notStep
	}
	switch d.Action {
	case OpenRouteStep:
		switch d.Reason {
		case leverageUpReason, termKeepAliveReason:
			return stepUp
		}
	case DeleverRouteStep:
		switch d.Reason {
		case hardLTVDeleverReason, debtZeroReason, leverageDownPartialReason, rotateRepayReason:
			return stepDown
		case overflowMoveReason:
			return stepMove
		case exitWithdrawReason, releaseReason, rotateWithdrawReason:
			return stepRelease
		}
	}
	return notStep
}

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

func stepDownRepay(s Snapshot, d Decision) (uint64, bool) {
	switch d.Reason {
	case rotateRepayReason:
		return uint64(max(min(d.AmountRaw, s.PositionDebtRaw), 0)), false
	case debtZeroReason:
		return uint64(max(s.PositionDebtRaw, s.PayoffDebtRaw)), true
	case hardLTVDeleverReason:
		if ltv := hardLTVStepLTVBPS(s); ltv > 0 {
			return uint64(max(stepDebtToLTVRaw(s, ltv), 0)), false
		}
		return uint64(max(s.PositionDebtRaw, s.PayoffDebtRaw)), true
	case leverageDownPartialReason:
		return uint64(max(stepDebtToLTVRaw(s, heldLevelLTVBPS(s)), 0)), false
	}
	return 0, false
}

type stepState struct {
	route                                      RuntimeRoute
	position                                   KaminoPosition
	collateral                                 decodedKaminoReserve
	obligation                                 kamino.Obligation
	slot, collateralBalance, debtBalance, cash uint64
	topology                                   []string
}

func (st stepState) heldDebt() uint64 {
	if sharedUSDCDebt(st.route.Lane) {
		return 0
	}
	return st.debtBalance
}

func readStepState(v *Vault, accounts []ConfirmedAccount, route RuntimeRoute, slot int64) (stepState, error) {
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
	if st.collateralBalance, err = vaultCustodyRaw(v, accounts, route.CollateralCustody, route.Kamino.CollateralMint, route.CollateralTokenProgram); err != nil {
		return stepState{}, err
	}
	if st.debtBalance, err = vaultCustodyRaw(v, accounts, route.DebtCustody, route.Kamino.DebtMint, route.DebtTokenProgram); err != nil {
		return stepState{}, err
	}
	st.topology = obligationTopology(obligation)
	return st, nil
}

type legCap struct {
	raw    uint64
	capped bool
}

func stepLegCap(route RuntimeRoute) legCap {
	if key, _ := kaminoPolicyLeg(route, kaminoLegDeposit); key.leg != 0 {
		return legCap{raw: bridgeCapRaw, capped: true}
	}
	return legCap{}
}

func (c legCap) clamp(raw uint64) uint64 {
	if c.capped {
		return min(raw, c.raw)
	}
	return raw
}

func (c legCap) exceeded(raw uint64) bool { return c.capped && raw > c.raw }

func (c legCap) headroom() legCap {
	if !c.capped {
		return c
	}
	return legCap{raw: c.raw - c.raw/10, capped: true}
}

func (st stepState) stepWithdrawReceipts() (uint64, error) {
	ceiling, ok := kaminoBorrowLimitBPS(st.obligation)
	if !ok || ceiling <= stepWithdrawMarginBPS {
		return 0, budgetHold("step_withdraw_limit_unavailable")
	}
	receipts, _, err := withdrawExcessAtLTV(st.position, uint64(ceiling-stepWithdrawMarginBPS))
	if err != nil {
		return 0, roomHold("step_no_withdraw_room", err)
	}
	return stepLegCap(st.route).clamp(receipts), nil
}

type downStepPlan struct {
	withdrawReceiptsRaw, convertCollateralRaw uint64
}

func planDownStep(s Snapshot, d Decision, st stepState, repayRaw uint64) (downStepPlan, error) {
	limit, err := st.stepWithdrawReceipts()
	if err != nil {
		return downStepPlan{}, err
	}
	if d.Reason == releaseReason {
		receipts := uint64(max(partialWithdrawalReleaseReceipts(s, d.AmountRaw), 0))
		receipts = min(receipts, limit)
		liquidity, err := st.collateral.redeemLiquidityRaw(receipts)
		if err != nil || liquidity == 0 || s.PositionCollateralValueRaw <= 0 {
			return downStepPlan{}, budgetHoldFrom(err, "release_leg_unavailable")
		}
		convert := new(big.Int).Mul(new(big.Int).SetUint64(liquidity), big.NewInt(s.PositionDebtValueRaw))
		convert.Mul(convert, big.NewInt(10_000+stepSwapLossBPS))
		convert.Quo(convert, new(big.Int).Mul(big.NewInt(s.PositionCollateralValueRaw), big.NewInt(10_000)))
		if !convert.IsUint64() || convert.Sign() <= 0 {
			return downStepPlan{}, budgetHold("release_leg_unavailable")
		}
		return downStepPlan{receipts, min(convert.Uint64(), liquidity+st.collateralBalance)}, nil
	}
	short := repayRaw
	if held := st.heldDebt(); held < repayRaw {
		short -= held
	}
	p := st.position
	need, err := valueBetweenTokenRaw(short, p.DebtDecimals, p.CollateralDecimals, p.DebtPriceSF, p.CollateralPriceSF, true)
	if err != nil {
		return downStepPlan{}, err
	}
	margined := new(big.Int).Mul(new(big.Int).SetUint64(need), big.NewInt(10_000+stepSwapLossBPS))
	margined.Quo(margined, big.NewInt(10_000))
	if !margined.IsUint64() {
		return downStepPlan{}, budgetHold("step_amounts_invalid")
	}
	if need = margined.Uint64(); need > st.collateralBalance {
		need -= st.collateralBalance
	}
	receipts, err := payoffReceiptsForLiquidity(st.collateral.totalLiquiditySF, st.collateral.collateralMintSupply, need)
	if err != nil {
		return downStepPlan{}, err
	}
	receipts = min(receipts, limit, p.CollateralDepositedRaw-1)
	liquidity, err := st.collateral.redeemLiquidityRaw(receipts)
	if err != nil || liquidity == 0 {
		return downStepPlan{}, budgetHoldFrom(err, "step_no_withdraw_room")
	}
	return downStepPlan{receipts, liquidity + st.collateralBalance}, nil
}

func prepareStepFromTickObservation(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, client *jupiter.Client, decision Decision, observation Observation) (Observation, StepExecutionEvidence, error) {
	kind := stepKind(decision)
	if kind == notStep || rpc == nil || view == nil || observation.routeBatch == nil || decision.AmountRaw <= 0 {
		return Observation{}, StepExecutionEvidence{}, fmt.Errorf("invalid step evidence request")
	}
	prepareStart, s, accounts, v := time.Now(), observation.Snapshot, observation.routeBatch.Accounts, manifest.vault
	route, err := v.route(decision.StrategyKey)
	if err != nil {
		return Observation{}, StepExecutionEvidence{}, err
	}
	if observation.policies, err = observeInstalledPolicies(ctx, rpc, v, s.Slot); err != nil {
		return Observation{}, StepExecutionEvidence{}, err
	}
	st, err := readStepState(v, accounts, route, s.Slot)
	if err != nil {
		return Observation{}, StepExecutionEvidence{}, err
	}
	switch kind {
	case stepMove:
		evidence, err := assembleMove(ctx, rpc, manifest, observation.policies, accounts, st, s, decision)
		logStage("prepare_step", prepareStart)
		return observation, evidence, err
	case stepRelease:
		evidence, _, err := releaseLeg(ctx, rpc, view, client, manifest, observation.policies, accounts, st, s, decision)
		logStage("prepare_step", prepareStart)
		return observation, evidence, err
	case stepUp:
		borrow := uint64(decision.AmountRaw)
		if decision.Reason != termKeepAliveReason {
			if capped, capErr := capacitySizedBorrow(st.position, accounts, route, leverageUpLevel(s)); capErr != nil || borrow > capped || borrow < leverageMinimumBorrowRaw {
				err = budgetHoldFrom(capErr, "borrow_capacity_shrank")
			}
		}
		if err == nil {
			borrow, err = stepUpBorrowWithinCap(st, borrow)
		}
		if err != nil {
			return Observation{}, StepExecutionEvidence{}, err
		}
		evidence, _, err := assembleStep(ctx, rpc, view, client, manifest, observation.policies, st, stepUp, borrow, borrow+st.heldDebt(), s.Slot)
		logStage("prepare_step", prepareStart)
		return observation, evidence, err
	}
	repay, full := stepDownRepay(s, decision)
	if headroom := stepLegCap(route).headroom(); headroom.exceeded(repay) {
		repay, full = headroom.raw, false
	}
	plan, err := planDownStep(s, decision, st, repay)
	if err != nil {
		return Observation{}, StepExecutionEvidence{}, err
	}
	receipts, convert := plan.withdrawReceiptsRaw, plan.convertCollateralRaw
	evidence, guaranteed, err := assembleStep(ctx, rpc, view, client, manifest, observation.policies, st, stepDown, receipts, convert, s.Slot)
	if err == nil && full && st.heldDebt()+guaranteed < repay {
		scaled := new(big.Int).Mul(new(big.Int).SetUint64(convert), new(big.Int).SetUint64(repay-st.heldDebt()))
		scaled.Mul(scaled, big.NewInt(10_001)).Quo(scaled, new(big.Int).Mul(new(big.Int).SetUint64(guaranteed), big.NewInt(10_000)))
		if scaled.IsUint64() && scaled.Uint64() > st.collateralBalance {
			var more uint64
			if more, err = payoffReceiptsForLiquidity(st.collateral.totalLiquiditySF, st.collateral.collateralMintSupply, scaled.Uint64()-st.collateralBalance); err == nil {
				limit, _ := st.stepWithdrawReceipts()
				if more = min(more, limit, st.position.CollateralDepositedRaw-1); more > receipts {
					liquidity, redeemErr := st.collateral.redeemLiquidityRaw(more)
					if redeemErr == nil && liquidity > 0 {
						evidence, _, err = assembleStep(ctx, rpc, view, client, manifest, observation.policies, st, stepDown, more, liquidity+st.collateralBalance, s.Slot)
					}
				}
			}
		}
	}
	logStage("prepare_step", prepareStart)
	return observation, evidence, err
}

func releaseLeg(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, policies installedPolicies, accounts []ConfirmedAccount, st stepState, s Snapshot, decision Decision) (StepExecutionEvidence, uint64, error) {
	if decision.Reason == releaseReason && st.position.DebtRaw > 0 {
		plan, err := planDownStep(s, decision, st, 0)
		if err != nil {
			return StepExecutionEvidence{}, 0, err
		}
		liquidity, err := st.collateral.redeemLiquidityRaw(plan.withdrawReceiptsRaw)
		if err != nil {
			return StepExecutionEvidence{}, 0, err
		}
		evidence, _, err := assembleStep(ctx, rpc, view, client, manifest, policies, st, stepDown, plan.withdrawReceiptsRaw, plan.convertCollateralRaw, s.Slot)
		return evidence, liquidity + st.collateralBalance - plan.convertCollateralRaw, err
	}
	receipts := st.position.CollateralDepositedRaw
	switch decision.Reason {
	case releaseReason:
		receipts = uint64(max(partialWithdrawalReleaseReceipts(s, decision.AmountRaw), 0))
	case rotateWithdrawReason:
		receipts = min(uint64(max(decision.AmountRaw, 0)), receipts)
	}
	receipts = stepLegCap(st.route).clamp(receipts)
	liquidity, err := st.collateral.redeemLiquidityRaw(receipts)
	if err != nil || receipts == 0 || liquidity == 0 {
		return StepExecutionEvidence{}, 0, budgetHoldFrom(err, "release_leg_unavailable")
	}
	if st.cash, err = vaultCustodyRaw(manifest.vault, accounts, manifest.vault.cash, fleet.USDCMint, bridgeTokenProgram); err != nil {
		return StepExecutionEvidence{}, 0, err
	}
	evidence, _, err := assembleStep(ctx, rpc, view, client, manifest, policies, st, stepExit, receipts, liquidity+st.collateralBalance, s.Slot)
	return evidence, 0, err
}

func assembleStep(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, policies installedPolicies, st stepState, kind StepKind, openRaw, convertRaw uint64, slot int64) (StepExecutionEvidence, uint64, error) {
	v := manifest.vault
	route := st.route
	lane := route.Lane
	up, exit := kind == stepUp, kind == stepExit
	conversion := map[StepKind]Action{stepUp: SwapDebtToCollateralStep, stepDown: SwapCollateralToDebtStep, stepExit: SwapCollateralToStableStep}[kind]
	action, openLeg, closeLeg := DeleverRouteStep, kaminoLegWithdraw, kaminoLegRepay
	sourceHeld, destinationHeld, openLiquidity := st.collateralBalance, st.heldDebt(), uint64(0)
	if up {
		action, openLeg, closeLeg = OpenRouteStep, kaminoLegBorrow, kaminoLegDeposit
		sourceHeld, destinationHeld, openLiquidity = st.heldDebt(), st.collateralBalance, openRaw
	} else {
		liquidity, err := st.collateral.redeemLiquidityRaw(openRaw)
		if err != nil {
			return StepExecutionEvidence{}, 0, err
		}
		openLiquidity = liquidity
	}
	if exit {
		destinationHeld = st.cash
	}
	if stepLegCap(route).exceeded(openRaw) {
		return StepExecutionEvidence{}, 0, budgetHold("step_over_policy_cap")
	}
	if openRaw == 0 || convertRaw == 0 || convertRaw > math.MaxInt64 || sourceHeld > math.MaxUint64-openLiquidity || convertRaw > sourceHeld+openLiquidity {
		return StepExecutionEvidence{}, 0, budgetHold("step_amounts_invalid")
	}
	placeholder := LatestBlockhash{Blockhash: v.squads, LastValidBlockHeight: 1}
	open, err := manifest.kaminoPacketForRoute(policies, action, openLeg, openRaw, placeholder, lane)
	if err != nil {
		return StepExecutionEvidence{}, 0, err
	}
	open.ObligationReserves = st.topology
	request := StepRequest{Open: open}
	var closing KaminoLegRequest
	if !exit {
		if closing, err = manifest.kaminoPacketForRoute(policies, action, closeLeg, 1, placeholder, lane); err != nil {
			return StepExecutionEvidence{}, 0, err
		}
		request.Close = &closing
	}
	decision := Decision{Action: conversion, AmountRaw: int64(convertRaw), StrategyKey: lane}
	var conversionRequest any
	if conversionVenueHastra(decision) {
		e, err := prepareHastraQuoteEvidence(ctx, v, rpc, view, policies, decision, sourceHeld+openLiquidity, destinationHeld, slot)
		if err != nil {
			return StepExecutionEvidence{}, 0, err
		}
		request.Hastra, conversionRequest = &e.Request, e.Request
	} else {
		maxAccounts, err := stepSwapMaxAccounts(v, request, conversion)
		if err != nil {
			return StepExecutionEvidence{}, 0, err
		}
		e, err := prepareJupiterQuoteEvidenceAt(ctx, v, rpc, client, policies, decision, sourceHeld+openLiquidity, destinationHeld, slot, maxAccounts)
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
	if stepLegCap(route).exceeded(closeRaw) && !exit {
		return StepExecutionEvidence{}, 0, budgetHold("step_over_policy_cap")
	}
	hash, height := stepConversionBlockhash(conversionRequest)
	blockhash := LatestBlockhash{Blockhash: hash, LastValidBlockHeight: height}
	if request.Open, err = manifest.kaminoPacketForRoute(policies, action, openLeg, openRaw, blockhash, lane); err != nil {
		return StepExecutionEvidence{}, 0, err
	}
	request.Open.ObligationReserves = st.topology
	if !exit {
		if closing, err = manifest.kaminoPacketForRoute(policies, action, closeLeg, closeRaw, blockhash, lane); err != nil {
			return StepExecutionEvidence{}, 0, err
		}
	}
	if _, err := compileStepMessageForDelegate(v, request, mustKey(backyardDelegate)); err != nil {
		return StepExecutionEvidence{}, 0, budgetHoldFrom(err, "step_does_not_compile")
	}
	effect := func(custody, mint, program string, before, after uint64) ExpectedAccountEffect {
		return ExpectedAccountEffect{Address: custody, Owner: program, Mint: mint, Authority: v.squads, BeforeRaw: before, AfterRaw: after}
	}
	collateral := effect(route.CollateralCustody, route.Kamino.CollateralMint, route.CollateralTokenProgram, st.collateralBalance, 0)
	debt := effect(route.DebtCustody, route.Kamino.DebtMint, route.DebtTokenProgram, st.debtBalance, st.debtBalance+openLiquidity-convertRaw)
	if !up {
		collateral.AfterRaw = st.collateralBalance + openLiquidity - convertRaw
		debt.AfterRaw = st.debtBalance - st.heldDebt()
	}
	if exit {
		debt = effect(v.cash, fleet.USDCMint, bridgeTokenProgram, st.cash, closeRaw)
	}
	return StepExecutionEvidence{Request: request, ExpectedEffects: stepEffects(up, collateral, debt)}, guaranteed, nil
}

func stepUpBorrowWithinCap(st stepState, borrow uint64) (uint64, error) {
	headroom := stepLegCap(st.route).headroom()
	if !headroom.capped {
		return borrow, nil
	}
	limit := headroom.raw
	borrow = min(borrow, limit)
	p := st.position
	if limit <= st.collateralBalance {
		return 0, budgetHold("step_over_policy_cap")
	}
	collateral, err := valueBetweenTokenRaw(borrow, p.DebtDecimals, p.CollateralDecimals, p.DebtPriceSF, p.CollateralPriceSF, true)
	if err != nil {
		return 0, err
	}
	if collateral > limit-st.collateralBalance {
		if borrow, err = valueBetweenTokenRaw(limit-st.collateralBalance, p.CollateralDecimals, p.DebtDecimals, p.CollateralPriceSF, p.DebtPriceSF, false); err != nil {
			return 0, err
		}
	}
	if borrow < leverageMinimumBorrowRaw {
		return 0, budgetHold("step_over_policy_cap")
	}
	return borrow, nil
}

func assembleMove(ctx context.Context, rpc *chain.Client, manifest RouteManifest, policies installedPolicies, accounts []ConfirmedAccount, st stepState, s Snapshot, decision Decision) (StepExecutionEvidence, error) {
	v := manifest.vault
	route := st.route
	lane, ok := overflowLaneOf(route.Lane)
	if !ok {
		return StepExecutionEvidence{}, fmt.Errorf("lane %s has no overflow lane", route.Lane)
	}
	destination, err := v.route(lane)
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
		return StepExecutionEvidence{}, roomHold("overflow_move_no_withdraw_room", err)
	}
	deposit := st.collateralBalance + liquidity
	if stepLegCap(route).exceeded(receipts) || stepLegCap(destination).exceeded(deposit) {
		return StepExecutionEvidence{}, budgetHold("step_over_policy_cap")
	}
	account := accountAt(accounts, destination.Kamino.Obligation)
	if account.Address != destination.Kamino.Obligation || account.Lamports == 0 {
		return StepExecutionEvidence{}, budgetHold("overflow_obligation_absent")
	}
	if _, err := decodeKaminoObligation(account, destination.Kamino); err != nil {
		return StepExecutionEvidence{}, err
	}
	topology, err := observedObligationTopology(accounts, destination)
	if err != nil {
		return StepExecutionEvidence{}, err
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
	request := StepRequest{Open: open, Close: &closing}
	if _, err := compileStepMessageForDelegate(v, request, mustKey(backyardDelegate)); err != nil {
		return StepExecutionEvidence{}, budgetHoldFrom(err, "step_does_not_compile")
	}
	return StepExecutionEvidence{Request: request, ExpectedEffects: moveEffects(route, st.collateralBalance)}, nil
}

func moveEffects(route RuntimeRoute, balance uint64) ExpectedEffects {
	after := uint64(0)
	collateral := ExpectedAccountEffect{Address: route.CollateralCustody, Owner: route.CollateralTokenProgram, Mint: route.Kamino.CollateralMint, Authority: route.Kamino.Vault,
		BeforeRaw: balance, AfterRaw: after, MinimumAfterRaw: &after}
	return ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: effectsAtomicStep, Conserved: false, Accounts: []ExpectedAccountEffect{collateral}}
}
