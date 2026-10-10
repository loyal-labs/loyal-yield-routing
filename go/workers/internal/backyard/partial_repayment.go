package backyard

import (
	"context"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// observePartialRepaymentProjection is the debt-clear partial-repayment proof
// taken at bind: the repay leg is the planner's exact partial step, its
// custody prestate is current, and its simulated poststate leaves debt on the
// obligation.
func observePartialRepaymentProjection(ctx context.Context, rpc *chain.Client, view *View, m RouteManifest, s Snapshot, d Decision, r KaminoPrimeUSDCRequest, e ExpectedEffects) (phase3KaminoProjection, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, leg, err := kaminoPrimeUSDCInstruction(r)
	if err != nil || rpc == nil || !partialRepaymentLane(s.RouteLane, d.Reason) || !s.Fresh || s.Slot <= 0 || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.RouteKind != RouteKind || s.RouteLane != s.StrategyKey || s.RouteLane != d.StrategyKey || s.RouteLane != r.RouteLane || !s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionDebtRaw <= 1 || d.Action != DeleverRouteStep || r.Action != d.Action || !partialRepaymentReason(d.Reason) || !decisionsEqual(m.DecideOnManifest(s), d) || d.AmountRaw <= 0 || r.AmountRaw != partialRepaymentWireRaw(s, d) || r.AmountRaw == 0 || r.AmountRaw >= uint64(s.PositionDebtRaw) || uint64(debtCashRaw(s)) < r.AmountRaw || leg != kaminoLegRepay || r.FullPayoff || r.RepaymentRelease {
		return phase3KaminoProjection{}, budgetHold("partial_repayment_proof_unavailable")
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return phase3KaminoProjection{}, err
	}
	before, accounts, err := observeKaminoPayoffWindow(ctx, view, route, s.Slot, 1)
	if err != nil {
		return phase3KaminoProjection{}, err
	}
	if !sameAccruingDebt(before, s.PositionDebtRaw) {
		return phase3KaminoProjection{}, budgetHold("partial_repayment_prestate_changed")
	}
	for _, effect := range e.Accounts {
		a := accountAt(accounts, effect.Address)
		mint, _ := decodeBase58PublicKey(effect.Mint)
		owner, _ := decodeBase58PublicKey(effect.Authority)
		c, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Executable || a.Lamports == 0 || a.Owner != effect.Owner || c.Raw != effect.BeforeRaw {
			return phase3KaminoProjection{}, budgetHold("partial_repayment_prestate_changed")
		}
	}
	message, err := CompileKaminoMessage(r)
	if err != nil {
		return phase3KaminoProjection{}, err
	}
	addresses := depositProjectionAddresses(route)
	addresses = append(addresses, route.Kamino.DebtReserve, route.DebtLiquiditySupply)
	projection, err := simulatePhase3EntryProjection(ctx, rpc, message, addresses, before.ObservedSlot)
	if err != nil {
		return phase3KaminoProjection{}, err
	}
	if _, err = validatePartialRepaymentProjection(r, e, s, projection); err != nil {
		return phase3KaminoProjection{}, err
	}
	return projection, nil
}

func validatePartialRepaymentProjection(r KaminoPrimeUSDCRequest, e ExpectedEffects, s Snapshot, p phase3KaminoProjection) (KaminoPayoffBound, error) {
	message, err := CompileKaminoMessage(r)
	_, leg, legErr := kaminoPrimeUSDCInstruction(r)
	if err != nil || legErr != nil || leg != kaminoLegRepay || r.Action != DeleverRouteStep || r.FullPayoff || r.RepaymentRelease || !(selectorLane(r.RouteLane) || leverageLane(r.RouteLane)) || r.RouteLane != s.RouteLane || p.MessageSHA256 != sha256Bytes(message) || p.UnitsConsumed == 0 || p.Slot < s.Slot || p.Slot-s.Slot > observationLagSlots() || r.AmountRaw == 0 || s.PositionDebtRaw <= 0 || r.AmountRaw >= uint64(s.PositionDebtRaw) || e.Repayment == nil || e.Repayment.MinimumDebitRaw != r.AmountRaw || e.Repayment.MaximumDebitRaw != r.AmountRaw {
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
// withdrawal, unwind or down move. It takes the same partial-repayment proof
// as the hard-LTV partial repay, but it is not a risk reduction, so it writes
// no unwind intent of its own.
const exitPartialRepayReason = "exit_partial_repay"

func partialRepaymentReason(reason string) bool {
	return reason == "hard_ltv_partial_repay" || reason == exitPartialRepayReason
}

// hard_ltv_partial_repay keeps its installed selector-lane scope; the exit
// cycle runs on the B2 leverage lanes (AUTO and OnRe).
func partialRepaymentLane(lane, reason string) bool {
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
