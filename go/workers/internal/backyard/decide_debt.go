package backyard

import (
	"fmt"
)

// Journaled reasons of the top-up tranche (plan B3). Admissions and the
// selector fence key on these exact reasons, never on a balance.
const (
	topupSwapReason       = "topup_usdc_requires_collateral"
	topupAllocationReason = "topup_voltr_idle"
	topupDepositReason    = "topup_collateral_deposit"
	// A step's close moves its conversion's minimum output; the excess the
	// conversion delivers stays beside the position. Borrowed-token excess
	// converts to collateral, which the top-up deposit then joins.
	stepResidueReason = "step_residue_to_collateral"
)

const onreONycUSDC = "OnRe/ONyc/USDC"

// A top-up allocation below this is not worth its fees; the cash waits in
// Voltr for the next deposit. ponytail: fixed $10 floor, derive it from the
// measured leg costs if small deposits matter.
const topupMinimumRaw int64 = 10_000_000

// debtTopupLane: the top-up also runs beside a debt-bearing position where
// debt custody is not bridge cash (AUTO/PYUSD). On OnRe the debt is Squads
// USDC, so Squads cash beside debt is borrowed cash the leverage loop swaps.
func debtTopupLane(lane string) bool {
	return earnActiveLane(lane) && !sharedUSDCDebt(lane)
}

// laneSwap reads the edge table the builder signs from, so the planner never
// picks a swap the lane's policy lacks (AUTO and OnRe have no USDC->debt edge).
func laneSwap(lane string, action Action) bool {
	if _, hastra := hastraConversionLeg(action, lane); hastra {
		return true
	}
	if action == SwapDebtToUSDCStep && !debtToUSDCConvertible(lane) {
		return false
	}
	_, _, _, _, err := jupiterEdgeForRoute(action, lane)
	return err == nil
}

// topupStep is the plan B3 sequence beside a funded position: convert a
// step's borrowed-token excess (stepResidueReason), swap Squads cash to
// collateral, deposit that collateral, then move idle Voltr cash into Squads.
// No top-up leg borrows or repays. It runs before any borrow,
// so a later borrow levers the whole collateral. Every withdrawal, hard-LTV,
// unwind and report rule has already run.
func topupStep(s Snapshot, hard int64, d func(Action, string, int64) Decision) (Decision, bool) {
	if !s.HasPosition || s.PositionCollateralRaw <= 0 || hard <= TargetLTVBPS ||
		(s.PositionDebtRaw != 0 && (!debtTopupLane(s.RouteLane) || s.PositionDebtValueRaw <= 0)) ||
		s.WithdrawalDemandRaw != 0 || s.Unwind || s.CutoverDrain || s.UnwindRefreshRequired || s.VoltrStrategyIdleRaw != 0 {
		return Decision{}, false
	}
	// An overflow lane takes no new capital: top-ups go to its source lane,
	// whose next overflow tranche moves them on. Its own residue and
	// collateral still redeposit above and below.
	_, overflow := overflowSourceOf(s.RouteLane)
	if s.PositionDebtRaw > 0 && s.DebtIdleRaw > partialWithdrawalDustRaw {
		return d(SwapDebtToCollateralStep, stepResidueReason, s.DebtIdleRaw), true
	}
	if s.SquadsIdleRaw > 0 && !overflow {
		return d(SwapStableToCollateralStep, topupSwapReason, s.SquadsIdleRaw), true
	}
	// Swapped collateral joins the existing obligation. No borrow comes with
	// it: the loan-to-value can only go down.
	if s.CollateralIdleRaw > 0 {
		if s.MinimumCollateralDepositRaw <= 0 || s.CollateralIdleRaw < s.MinimumCollateralDepositRaw {
			return Decision{}, false
		}
		return d(OpenRouteStep, topupDepositReason, s.CollateralIdleRaw), true
	}
	// Top up before any borrow, so idle cash never waits for a full close and
	// reopen, and a later borrow levers the whole collateral at once.
	amount := min(s.VoltrIdleRaw-DefaultSelectorPolicy().IdleBufferRaw, s.TopupDepositRoomRaw, int64(strategyTwoBridgeLegCapRaw))
	if amount < topupMinimumRaw || overflow {
		return Decision{}, false
	}
	return d(VoltrAllocateToSquads, topupAllocationReason, amount), true
}

// The same single-loop lifecycle as the retained routes, with debt custody
// explicitly distinct from bridge USDC. No raw-unit cap or stablecoin peg is
// assumed here: admission must price the executable transaction and its exit.
func decideNonUSDC(s Snapshot, initializationReady func(Snapshot) bool) Decision {
	d := func(action Action, reason string, amount int64) Decision {
		return Decision{Action: action, Reason: reason, AmountRaw: amount, StrategyKey: s.RouteLane,
			IdempotencyKey: fmt.Sprintf("%s:%s:%s:%d:%s", s.ObservationID, s.RouteLane, action, amount, reason)}
	}
	if s.Nonterminal != "" {
		reason := "resume_nonterminal_operation"
		if s.HasAmbiguousSubmission {
			reason = "recover_ambiguous_submission"
		}
		return d(RecoverTransaction, reason, 0)
	}
	if s.ManualReason != "" || s.RouteKind != RouteKind || !s.Fresh || s.ObservationID == "" || s.Slot <= 0 {
		return d(HoldManualRecovery, "invalid_or_incoherent_snapshot", 0)
	}
	for _, value := range []int64{s.WithdrawalDemandRaw, s.SquadsIdleRaw, s.CollateralIdleRaw, s.CollateralIdleValueRaw, s.MinimumCollateralDepositRaw, s.DebtIdleRaw, s.PayoffDebtRaw,
		s.VoltrStrategyIdleRaw, s.VoltrIdleRaw, s.PositionCollateralRaw, s.PositionDebtRaw,
		s.PositionCollateralValueRaw, s.PositionDebtValueRaw, s.StrategyNAVRaw, s.PriorReportedNAVRaw,
		s.LTVBPS, s.LiquidationThresholdBPS, s.LastReportAgeSeconds, s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw} {
		if value < 0 {
			return d(HoldManualRecovery, "invalid_or_incoherent_snapshot", 0)
		}
	}
	if s.LiquidationThresholdBPS > 10000 {
		return d(HoldManualRecovery, "invalid_or_incoherent_snapshot", 0)
	}
	if s.HasPosition != (s.PositionCollateralRaw > 0 || s.PositionDebtRaw > 0) {
		return d(HoldManualRecovery, "position_presence_mismatch", 0)
	}
	hard := min(s.LiquidationThresholdBPS-1500, int64(6000))
	if s.HasPosition && (s.LiquidationThresholdBPS <= 1500 || hard <= TargetLTVBPS) {
		return d(HoldManualRecovery, "invalid_hard_ltv", 0)
	}
	// Hard LTV: one down step back to the stored level, before anything else.
	if s.HasPosition && s.LTVBPS >= hard {
		if s.PositionDebtRaw > 0 {
			return d(DeleverRouteStep, hardLTVDeleverReason, stepDownMarker)
		}
		return d(HoldManualRecovery, "hard_ltv_without_debt", 0)
	}
	// An admitted unwind whose sizing evidence went stale re-admits before any
	// further leg; only the recovery holds and the hard-LTV safety above it
	// may preempt.
	if s.UnwindRefreshRequired {
		return d(Hold, "unwind_requires_fresh_admission", 0)
	}
	// Term debt with an unknowable maturity: nothing plans on it.
	if termDebtUnknown(s) {
		return d(Hold, "term_debt_maturity_unknown", 0)
	}
	// Finish a journal-explained stage even if deposits or claims have since
	// changed demand. Reporting cannot reconcile cash still in this custody.
	if s.VoltrStrategyIdleRaw > 0 {
		return d(VoltrRestoreIdle, "withdrawal_staged", s.VoltrStrategyIdleRaw)
	}
	if s.PostMutationNAVRequired && !withdrawalIdleUnderfunded(s) {
		// M4: the unwind legs below stay admissible when idle cannot cover the
		// pending withdrawal queue.
		if hold, blocked := custodyResidueHold(s); blocked {
			return hold
		}
		return d(ReportNAV, "post_mutation_nav_due", 0)
	}
	// Voltr idle already pays a plain withdrawal: report if due, never unwind.
	// Mirrors the USDC lane's withdrawal_covered (live 2026-09-28: a $5 claim
	// against $1,295 idle started a full AUTO unwind).
	if partialWithdrawalInFlight(s) {
		if action, reason, amount, ok := partialWithdrawalStep(s); ok {
			return d(action, reason, amount)
		}
	}
	// Tranche source half: free E with the same debt-preserving partial
	// legs, into Voltr idle reserved for the destination.
	if trancheSourceHalf(s) {
		if action, reason, amount, ok := partialWithdrawalStep(s); ok {
			return d(action, reason, amount)
		}
		return d(Hold, trancheSourceHoldReason(s), 0)
	}
	// Overflow source half: release E as collateral for the overflow lane,
	// then hold it in the shared custody until the handoff.
	if overflowSourceHalf(s) {
		if action, reason, amount, ok := partialWithdrawalStep(s); ok {
			return d(action, reason, amount)
		}
		return d(Hold, overflowSourceHoldReason(s), 0)
	}
	if s.WithdrawalDemandRaw > 0 && !s.Unwind && !s.CutoverDrain && s.WithdrawalDemandRaw <= s.VoltrIdleRaw {
		if s.CapitalMutated || s.LastReportAgeSeconds >= 60 {
			if hold, blocked := custodyResidueHold(s); blocked {
				return hold
			}
			return d(ReportNAV, "withdrawal_covered_nav_due", 0)
		}
		return d(Hold, "withdrawal_covered", 0)
	}
	// A requested canary drain stays a drain even when Voltr idle already covers
	// the withdrawal, and an admitted unwind is "a full exit, independent of the
	// user's claim amount", so it drains with no demand at all. Flat means every
	// debt/collateral/bridge custody is cleared.
	if s.CutoverDrain || s.Unwind || s.WithdrawalDemandRaw > 0 {
		// Partial withdrawal: free only the shortfall and keep the level.
		if action, reason, amount, ok := partialWithdrawalStep(s); ok {
			return d(action, reason, amount)
		}
		// Withdrawal size and partial-planner exclusions are not full-exit authority.
		// Explicit unwind/cutover flows retain their separate admission gates.
		if !s.Unwind && !s.CutoverDrain && s.PositionDebtRaw > 0 {
			return d(Hold, "withdrawal_full_exit_unproven", 0)
		}
		// A drain repays the whole debt with down steps first.
		if s.PositionDebtRaw > 0 {
			return d(DeleverRouteStep, withdrawalDeleverReason, stepDownMarker)
		}
		if s.PositionCollateralRaw > 0 {
			return d(DeleverRouteStep, "withdrawal_withdraw_collateral", 0)
		}
		if s.CollateralIdleRaw > 0 {
			return d(SwapCollateralToStableStep, "withdrawal_convert_collateral_to_usdc", s.CollateralIdleRaw)
		}
		if s.DebtIdleRaw > 0 {
			if !debtToUSDCConvertible(s.RouteLane) {
				return d(SwapDebtToCollateralStep, "withdrawal_stake_debt_residue", s.DebtIdleRaw)
			}
			return d(SwapDebtToUSDCStep, "withdrawal_convert_debt_residue_to_usdc", s.DebtIdleRaw)
		}
		if s.SquadsIdleRaw > 0 {
			return d(StageSquadsToVoltr, "withdrawal_terminal_residue", s.SquadsIdleRaw)
		}
		if s.CapitalMutated || s.PriorReportedNAVRaw != 0 || s.LastReportAgeSeconds >= 60 {
			if hold, blocked := custodyResidueHold(s); blocked {
				return hold
			}
			return d(ReportNAV, "withdrawal_terminal_nav_due", 0)
		}
		if s.StrategyNAVRaw != 0 {
			return d(HoldManualRecovery, "flat_custody_nonzero_nav", 0)
		}
		if s.WithdrawalDemandRaw > s.VoltrIdleRaw {
			return d(HoldManualRecovery, "withdrawal_conservation_shortfall", s.WithdrawalDemandRaw-s.VoltrIdleRaw)
		}
		if s.Unwind {
			return d(Hold, "unwind_complete", 0)
		}
		return d(Hold, "canary_flat_nav_current", 0)
	}
	// S1/S2 mirror the fixed lane: an unexplained drift holds, a reconciled
	// mutation reports only beyond the drift tolerance.
	if hold, drifted := unexplainedNAVDriftHold(s); drifted {
		return hold
	}
	if capitalMutationReports(s) || (scheduledNAVReportDue(s) && !admittedEntryAllocationReady(s)) {
		if hold, blocked := custodyResidueHold(s); blocked {
			return hold
		}
		return d(ReportNAV, "nav_due", 0)
	}
	// B2 down move to 1x: repay the position before any top-up or borrow.
	if action, reason, amount, ok := leverageDownStep(s); ok {
		return d(action, reason, amount)
	}
	if action, reason, amount, ok := leverageDownPartialStep(s); ok {
		return d(action, reason, amount)
	}
	// Plan B3 top-up tranche beside a funded position. It adds to the current
	// loop, so it needs no new-lane selector entry authority.
	if decision, ok := topupStep(s, hard, d); ok {
		return decision
	}
	// Returning flat working cash is an exit. It does not need a usable entry
	// market, an obligation account, or an entry LTV threshold. Unlike the USDC
	// flat predicate, idle debt custody disqualifies the return: unattributed
	// PYUSD residue keeps its own manual-recovery hold below and is never
	// cleared or carried alongside a bridge-cash stage.
	if !s.HasPosition && s.PositionCollateralRaw == 0 && s.PositionDebtRaw == 0 && s.CollateralIdleRaw == 0 && s.DebtIdleRaw == 0 && s.SquadsIdleRaw > 0 &&
		(s.CapacityRaw < s.SquadsIdleRaw || s.PolicyLimitRaw < s.SquadsIdleRaw || s.MaxTargetLTVEntryRaw < s.SquadsIdleRaw || s.LiquidationThresholdBPS <= 0 || hard <= TargetLTVBPS) {
		return d(StageSquadsToVoltr, "entry_capacity_changed_return_cash", s.SquadsIdleRaw)
	}
	// Term debt: borrow again before maturity (a leverage_up borrow due
	// the same tick restarts the term itself).
	if action, reason, amount, ok := termKeepAliveStep(s, hard); ok {
		return d(action, reason, amount)
	}
	// B2: borrow only toward the stored level target (never an entry quote).
	if action, reason, amount, ok := leverageBorrowStep(s, hard); ok {
		return d(action, reason, amount)
	}
	// The pause blocks new entries; an overflow lane takes over its source's
	// freed collateral under the overflow tranche's authority.
	if s.SelectorEntryPaused && !overflowDestinationEntry(s) {
		return d(Hold, "selector_entry_requires_fresh_admission", 0)
	}
	// Same prerequisite as the fixed lane: a deposit into a missing obligation
	// is refused, so no allocation, swap, or deposit is constructed. Reports and
	// withdrawal legs above stay live.
	if hold, absent := obligationPrerequisiteHold(s, initializationReady); absent {
		return hold
	}
	if hard <= TargetLTVBPS {
		return d(HoldManualRecovery, "invalid_entry_ltv", 0)
	}
	if s.CollateralIdleRaw > 0 && s.MinimumCollateralDepositRaw == 0 {
		return d(Hold, "deposit_rounding_window_unavailable", 0)
	}
	depositReady := s.CollateralIdleRaw > 0 && s.CollateralIdleRaw >= s.MinimumCollateralDepositRaw
	// A levered position moves only by atomic steps; residue beside it waits
	// for the next step to sweep it.
	if s.PositionDebtRaw > 0 {
		return d(Hold, "single_loop_position_ready", 0)
	}
	if depositReady {
		return d(OpenRouteStep, "collateral_ready", s.CollateralIdleRaw)
	}
	if s.PositionCollateralRaw > 0 {
		if earnActiveLane(s.RouteLane) {
			action, reason, amount := leverageDebtFreeStep(s)
			return d(action, reason, amount)
		}
		// An exit-only lane is never levered.
		return d(Hold, "single_loop_position_ready", 0)
	}
	if s.CollateralIdleRaw > 0 {
		return d(Hold, "collateral_below_deposit_rounding_window", 0)
	}
	if s.DebtIdleRaw > 0 {
		return d(HoldManualRecovery, "unattributed_idle_debt_before_entry", 0)
	}
	if s.CapacityRaw <= 0 || s.PolicyLimitRaw <= 0 || s.MaxTargetLTVEntryRaw <= 0 {
		return d(Hold, "insufficient_reviewed_entry_capacity", 0)
	}
	// These entry bounds are explicitly bridge-USDC units supplied by the
	// observation/valuation path, never raw PYUSD debt amounts.
	limit := min(s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw)
	if s.SquadsIdleRaw > 0 {
		return d(SwapStableToCollateralStep, "usdc_requires_collateral", min(s.SquadsIdleRaw, limit))
	}
	if s.VoltrIdleRaw > 0 {
		// The allocation is sized only by the admitted selector entry equity,
		// which must still fit the reviewed capacity above. A missing or
		// oversized admission holds for a fresh quote instead of spending
		// unadmitted idle.
		if s.SelectorEntryEquityRaw <= 0 || s.SelectorEntryEquityRaw > min(s.VoltrIdleRaw, limit) {
			return d(Hold, "selector_entry_amount_requires_fresh_quote", 0)
		}
		return d(VoltrAllocateToSquads, "eligible_voltr_idle", s.SelectorEntryEquityRaw)
	}
	return d(Hold, "no_eligible_action", 0)
}
