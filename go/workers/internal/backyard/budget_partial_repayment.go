package backyard

import (
	"context"
	"errors"
	"math"
	"math/big"
	"time"
)

// Partial repayment has the same recovery budget as any other exit. Its
// simulated poststate prices a complete remaining exit, never settled NAV.
func observePhase3PartialRepaymentAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, e KaminoExecutionEvidence) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, r := o.Snapshot, e.Request
	_, leg, err := kaminoPrimeUSDCInstruction(r)
	if err != nil || rpc == nil || client == nil || !s.PilotActive || !partialRepaymentLane(s.RouteLane, d.Reason) || !s.Fresh || s.Slot <= 0 || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.RouteKind != RouteKind || s.RouteLane != s.StrategyKey || s.RouteLane != d.StrategyKey || s.RouteLane != r.RouteLane || !s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionDebtRaw <= 1 || d.Action != DeleverRouteStep || r.Action != d.Action || !partialRepaymentDecision(s, d) || !decisionsEqual(m.DecideOnManifest(s), d) || d.AmountRaw <= 0 || r.AmountRaw != partialRepaymentWireRaw(s, d) || r.AmountRaw == 0 || r.AmountRaw >= uint64(s.PositionDebtRaw) || uint64(debtCashRaw(s)) < r.AmountRaw || leg != kaminoLegRepay || r.FullPayoff || r.RepaymentRelease {
		return phase3BridgeAdmission{}, budgetHold("partial_repayment_admission_unavailable")
	}
	if autoEmergencyPartialRepayment(s, d) {
		route := autoAUTOPYUSD
		if len(r.ObligationReserves) != 2 || r.ObligationReserves[0] != route.Kamino.CollateralReserve || r.ObligationReserves[1] != route.Kamino.DebtReserve {
			return phase3BridgeAdmission{}, budgetHold("auto_partial_repayment_identity_changed")
		}
		proof, err := verifyDebtClearEmergency(m, o, d, "partial-repayment-admission", time.Now().UTC())
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		if proof == nil {
			return phase3BridgeAdmission{}, budgetHold("auto_partial_repayment_fresh_risk_required")
		}
	}
	current, err := observePhase3KnownBuildCost(ctx, rpc, r, e.ExpectedEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	before, accounts, err := observeKaminoPayoffWindow(ctx, rpc, route, max(s.Slot, current.ObservationSlot), 1)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if !sameAccruingDebt(before, s.PositionDebtRaw) {
		return phase3BridgeAdmission{}, budgetHold("partial_repayment_prestate_changed")
	}
	for _, effect := range e.ExpectedEffects.Accounts {
		a := accountAt(accounts, effect.Address)
		mint, _ := decodeBase58PublicKey(effect.Mint)
		owner, _ := decodeBase58PublicKey(effect.Authority)
		c, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Executable || a.Lamports == 0 || a.Owner != effect.Owner || c.Raw != effect.BeforeRaw {
			return phase3BridgeAdmission{}, budgetHold("partial_repayment_prestate_changed")
		}
	}
	if autoEmergencyPartialRepayment(s, d) {
		before.ObservedSlot, accounts, err = observeAutoPartialRepaymentPrestate(ctx, rpc, m, s, before.ObservedSlot)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
	}
	message, err := CompileKaminoMessage(r)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	addresses := depositProjectionAddresses(route)
	addresses = append(addresses, route.Kamino.DebtReserve, route.DebtLiquiditySupply)
	projection, err := rpc.simulatePhase3EntryProjection(ctx, message, addresses, before.ObservedSlot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if _, err = validatePartialRepaymentProjection(r, e.ExpectedEffects, s, projection); err != nil {
		return phase3BridgeAdmission{}, err
	}
	if autoEmergencyPartialRepayment(s, d) {
		if err := validateAutoPartialRepaymentPrincipal(accounts, projection.Accounts, r); err != nil {
			return phase3BridgeAdmission{}, err
		}
	}
	plan, err := pricePhase3ProjectedPositionReturn(ctx, rpc, client, m, o, d, r, e.ExpectedEffects, current, projection)
	if err == nil {
		plan.RepaymentProjection = &projection
	}
	return plan, err
}

func validatePartialRepaymentProjection(r KaminoPrimeUSDCRequest, e ExpectedEffects, s Snapshot, p phase3KaminoProjection) (KaminoPayoffBound, error) {
	message, err := CompileKaminoMessage(r)
	_, leg, legErr := kaminoPrimeUSDCInstruction(r)
	if err != nil || legErr != nil || leg != kaminoLegRepay || r.Action != DeleverRouteStep || r.FullPayoff || r.RepaymentRelease || !s.PilotActive || !(selectorLane(r.RouteLane) || leverageLane(r.RouteLane)) || r.RouteLane != s.RouteLane || p.MessageSHA256 != sha256Bytes(message) || p.UnitsConsumed == 0 || p.Slot < s.Slot || p.Slot-s.Slot > observationLagSlots() || r.AmountRaw == 0 || s.PositionDebtRaw <= 0 || r.AmountRaw >= uint64(s.PositionDebtRaw) || e.Repayment == nil || e.Repayment.MinimumDebitRaw != r.AmountRaw || e.Repayment.MaximumDebitRaw != r.AmountRaw {
		return KaminoPayoffBound{}, budgetHold("partial_repayment_projection_identity_mismatch")
	}
	if _, err = MeasureExecutableDebit(r, e); err != nil {
		return KaminoPayoffBound{}, err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return KaminoPayoffBound{}, err
	}
	if debtCashRaw(s) < 0 || len(e.Accounts) != 2 || e.Accounts[0].BeforeRaw != uint64(debtCashRaw(s)) {
		return KaminoPayoffBound{}, budgetHold("partial_repayment_projection_cash_mismatch")
	}
	for _, effect := range e.Accounts {
		a := accountAt(p.Accounts, effect.Address)
		mint, _ := decodeBase58PublicKey(effect.Mint)
		owner, _ := decodeBase58PublicKey(effect.Authority)
		c, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Executable || a.Lamports == 0 || a.Owner != effect.Owner || c.Raw != effect.AfterRaw {
			return KaminoPayoffBound{}, budgetHold("partial_repayment_projection_custody_mismatch")
		}
	}
	o, err := decodeKaminoObligation(accountAt(p.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || s.PositionCollateralRaw <= 0 || o.collateralDepositedRaw != uint64(s.PositionCollateralRaw) || o.debtRaw == 0 || o.debtRaw >= uint64(s.PositionDebtRaw) || o.debtRaw < uint64(s.PositionDebtRaw)-r.AmountRaw {
		return KaminoPayoffBound{}, budgetHold("partial_repayment_projection_position_mismatch")
	}
	a := accountAt(p.Accounts, route.CollateralCustody)
	mint, _ := decodeBase58PublicKey(route.Kamino.CollateralMint)
	owner, _ := decodeBase58PublicKey(bridgeVault)
	c, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
	if err != nil || a.Executable || a.Lamports == 0 || s.CollateralIdleRaw < 0 || c.Raw != uint64(s.CollateralIdleRaw) {
		return KaminoPayoffBound{}, budgetHold("partial_repayment_projection_collateral_changed")
	}
	return decodeKaminoPayoffWindow(p.Accounts, route, p.Slot, 3)
}

// B2 1.75x exit: exit_partial_repay repays one cycle's funding inside a
// withdrawal, unwind or down move. It uses the same measured admission as
// the hard-LTV partial repay (projection, complete remaining exit), but it
// is not a risk reduction, so it writes no unwind intent of its own.
const exitPartialRepayReason = "exit_partial_repay"

func partialRepaymentReason(reason string) bool {
	return reason == "hard_ltv_partial_repay" || reason == exitPartialRepayReason
}

// hard_ltv_partial_repay keeps its installed selector-lane scope; the exit
// cycle runs on the B2 leverage lanes (AUTO and OnRe).
func partialRepaymentLane(lane, reason string) bool {
	if reason == "hard_ltv_repay" {
		return lane == autoAUTOPYUSD.Lane
	}
	if reason == exitPartialRepayReason {
		return leverageLane(lane)
	}
	return selectorLane(lane)
}

// partialRepaymentWireRaw: hard_ltv_partial_repay carries its exact amount;
// exit_partial_repay carries the (stable) debt cash and its exact wire is
// sized from the same snapshot by exitPartialRepayWireRaw.
func partialRepaymentWireRaw(s Snapshot, d Decision) uint64 {
	if d.Reason == exitPartialRepayReason {
		return uint64(max(exitPartialRepayWireRaw(s), 0))
	}
	return uint64(max(d.AmountRaw, 0))
}

// AUTO shares a reason between full and partial repayments. Classification is
// lane/action/amount-aware; it is never risk authority on its own.
func autoEmergencyPartialRepayment(s Snapshot, d Decision) bool {
	return s.RouteLane == autoAUTOPYUSD.Lane && d.StrategyKey == s.RouteLane && d.Action == DeleverRouteStep && d.Reason == "hard_ltv_repay" && d.AmountRaw > 0 && d.AmountRaw < s.PositionDebtRaw
}

func partialRepaymentDecision(s Snapshot, d Decision) bool {
	return partialRepaymentReason(d.Reason) || autoEmergencyPartialRepayment(s, d)
}

// Current execution evidence, never a replacement principal origin. Risk that
// has recovered needs a separate continuation authority; none is granted here.
func observeAutoPartialRepaymentPrestate(ctx context.Context, rpc *RPCClient, m RouteManifest, s Snapshot, slot int64) (int64, []ConfirmedAccount, error) {
	riskSlot, _, accounts, err := observeAutoPartialRepaymentPrestateSlots(ctx, rpc, m, s, slot)
	return riskSlot, accounts, err
}

func observeAutoPartialRepaymentPrestateSlots(ctx context.Context, rpc *RPCClient, m RouteManifest, s Snapshot, slot int64) (int64, int64, []ConfirmedAccount, error) {
	pin := reviewedTopupKaminoIdentity()
	addresses := payoffWindowAddresses(autoAUTOPYUSD, autoAUTOPYUSD.Kamino.Market)
	for _, lane := range selectorObservationLanes(m) {
		route, err := runtimeRoute(lane)
		if err != nil {
			return 0, 0, nil, err
		}
		addresses = append(addresses, route.Kamino.Obligation, route.CollateralCustody)
	}
	addresses = append(addresses, pin.program, pin.programData)
	observed, accounts, err := rpc.GetMultipleAccounts(ctx, uniqueNonzero(addresses), slot)
	if err != nil {
		return 0, 0, nil, err
	}
	if err := validateTopupKaminoCapability(accounts, observed); err != nil {
		return 0, 0, nil, err
	}
	if observed < slot {
		return 0, 0, nil, budgetHold("auto_partial_repayment_prestate_slot_changed")
	}
	riskSlot, riskAccounts, source := observed, accounts, "confirmed"
	position, err := observeKaminoFromFixedAccounts(ctx, rpc.GetMultipleAccounts, riskSlot, riskAccounts, autoAUTOPYUSD.Kamino)
	if errors.Is(err, errKaminoReserveStale) {
		// Closed reserve-only refresh establishes current risk, not raw principal.
		riskAddresses := uniqueNonzero(addresses[:len(addresses)-2])
		riskSlot, riskAccounts, err = rpc.simulateRouteValuationRefresh(ctx, autoAUTOPYUSD, riskAddresses, observed)
		if err != nil {
			return 0, 0, nil, err
		}
		source = routeRefreshValuationSource
		position, err = observeKaminoFromFixedAccounts(ctx, rpc.GetMultipleAccounts, riskSlot, riskAccounts, autoAUTOPYUSD.Kamino)
	}
	if err != nil {
		return 0, 0, nil, err
	}
	if position.DebtRaw > math.MaxInt64 || position.CollateralDepositedRaw != uint64(s.PositionCollateralRaw) {
		return 0, 0, nil, budgetHold("auto_partial_repayment_position_changed")
	}
	ltv, err := observedLTVBPS(position)
	if err != nil {
		return 0, 0, nil, err
	}
	if riskSlot < observed {
		return 0, 0, nil, budgetHold("auto_partial_repayment_prestate_slot_changed")
	}
	s.PositionDebtRaw, s.LTVBPS, s.LiquidationThresholdBPS = int64(position.DebtRaw), ltv, position.LiquidationThresholdBPS
	s.Slot, s.ValuationSlot, s.ValuationSource = riskSlot, riskSlot, source
	o := Observation{Snapshot: s, ObservedAt: time.Now().UTC(), ValuationSource: source, ValuationSlot: riskSlot,
		routeBatch: &routeObservationBatch{Slot: riskSlot, ObservationID: s.ObservationID, ManifestSHA256: m.SHA256, Accounts: riskAccounts}}
	proof, err := verifyDebtClearRiskBatch(m, o, "partial-repayment-prestate", time.Now().UTC())
	if err != nil {
		return 0, 0, nil, err
	}
	if proof == nil {
		return 0, 0, nil, budgetHold("auto_partial_repayment_fresh_risk_required")
	}
	return riskSlot, observed, accounts, nil
}

// Reviewed a087609 KLend ObligationLiquidity::accrue_interest floors A*R1/R0
// once in this compiled refresh. calculate_repay settles exactly amount<<60
// for a strict partial repay; ObligationLiquidity::repay subtracts that SF.
func validateAutoPartialRepaymentPrincipal(before, after []ConfirmedAccount, r KaminoPrimeUSDCRequest) error {
	route := autoAUTOPYUSD
	if r.RouteLane != route.Lane || r.FullPayoff || r.RepaymentRelease || r.AmountRaw == 0 || len(r.ObligationReserves) != 2 || r.ObligationReserves[0] != route.Kamino.CollateralReserve || r.ObligationReserves[1] != route.Kamino.DebtReserve {
		return budgetHold("auto_partial_repayment_identity_changed")
	}
	old, err := decodeKaminoObligation(accountAt(before, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return err
	}
	next, err := decodeKaminoObligation(accountAt(after, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return err
	}
	reserve, err := decodeKaminoReserve(accountAt(after, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return err
	}
	oldRate, newRate := littleInt(old.cumulativeBorrowRate[:]), littleInt(reserve.cumulativeBorrowRate[:])
	if oldRate.Sign() <= 0 || newRate.Cmp(oldRate) < 0 || next.cumulativeBorrowRate != reserve.cumulativeBorrowRate {
		return budgetHold("auto_partial_repayment_rate_changed")
	}
	want := new(big.Int).Mul(littleInt(old.debtAmountSF[:]), newRate)
	if want.BitLen() > 256 {
		return budgetHold("auto_partial_repayment_rate_changed")
	}
	want.Quo(want, oldRate)
	debit := new(big.Int).Lsh(new(big.Int).SetUint64(r.AmountRaw), 60)
	// Strictly below actual principal, not merely its rounded-up raw value.
	if debit.Cmp(littleInt(old.debtAmountSF[:])) >= 0 {
		return budgetHold("auto_partial_repayment_full_close_forbidden")
	}
	want.Sub(want, debit)
	marker, err := topupBorrowMarker(accountAt(before, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return err
	}
	nextMarker, err := topupBorrowMarker(accountAt(after, route.Kamino.Obligation), route.Kamino)
	if err != nil || marker != nextMarker || old.collateralDepositedRaw != next.collateralDepositedRaw || want.Sign() <= 0 || littleInt(next.debtAmountSF[:]).Cmp(want) != 0 {
		return budgetHold("auto_partial_repayment_principal_changed")
	}
	// post_repay_obligation_invariants uses market min-net-value. Require
	// strictly more, priced in SF (no raw-debt dust or rounding allowance).
	market := accountAt(after, route.Kamino.Market)
	if _, err := decodeKaminoMarketEmergency(market, route.Kamino); err != nil {
		return err
	}
	if len(market.Data) < kaminoMinRemainingValueOffset+16 {
		return budgetHold("auto_partial_repayment_minimum_unavailable")
	}
	value := new(big.Int).Mul(want, littleInt(reserve.marketPriceSF[:]))
	scale := new(big.Int).Lsh(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(reserve.mintDecimals)), nil), 60)
	value.Quo(value, scale)
	if value.Sign() <= 0 || value.Cmp(littleInt(market.Data[kaminoMinRemainingValueOffset:kaminoMinRemainingValueOffset+16])) <= 0 {
		return budgetHold("auto_partial_repayment_residual_too_small")
	}
	return nil
}
