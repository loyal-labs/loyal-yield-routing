package backyard

import (
	"fmt"
	"math/big"
)

// Journaled reasons of the top-up tranche (plan B3). Admissions and the
// selector fence key on these exact reasons, never on a balance.
const (
	debtResidueSwapReason = "debt_residue_to_usdc"
	topupSwapReason       = "topup_usdc_requires_collateral"
	topupAllocationReason = "topup_voltr_idle"
	topupDepositReason    = "topup_collateral_deposit"
)

// onreONycUSDC is the one USDC-debt lane with the plan B3 top-up (B4).
const onreONycUSDC = "OnRe/ONyc/USDC"

// A top-up allocation below this is not worth its fees; the cash waits in
// Voltr for the next deposit. ponytail: fixed $10 floor, derive it from the
// measured leg costs if small deposits matter.
const topupMinimumRaw int64 = 10_000_000

// topupStep adds collateral without borrowing. Debt-bearing AUTO uses only
// receipt-bound inventory; fresh loan and execution authority stay in admission.
// Withdrawal, hard-LTV, unwind and report rules run before this planner.
func topupStep(s Snapshot, hard int64, d func(Action, string, int64) Decision) (Decision, bool) {
	if !s.HasPosition || s.PositionCollateralRaw <= 0 || !s.PolicyReady || !s.ExitBuildable || hard <= TargetLTVBPS ||
		s.WithdrawalDemandRaw != 0 || s.Unwind || s.CutoverDrain || s.UnwindRefreshRequired || s.VoltrStrategyIdleRaw != 0 {
		return Decision{}, false
	}
	if s.PositionDebtRaw > 0 {
		if s.RouteLane != autoAUTOPYUSD.Lane || s.PositionDebtValueRaw <= 0 || !s.PilotActive || s.LTVBPS >= hard || s.DebtIdleRaw != 0 {
			return Decision{}, false
		}
		carry := int64(0)
		if t := s.TopupTranche; t != nil {
			if t.validate() != nil || t.LastSlot > s.Slot {
				return d(Hold, "topup_inventory_binding_invalid", 0), true
			}
			if t.Stage == topupTrancheOrdinary || t.Stage == topupTrancheHandoff {
				return Decision{}, false
			}
			carry = int64(t.CollateralRemainingRaw)
			if s.SquadsIdleRaw != int64(t.USDCRemainingRaw) || s.CollateralIdleRaw != carry || s.PrimeIdleRaw != carry {
				return d(Hold, "topup_inventory_custody_changed", 0), true
			}
			if t.Stage == topupTrancheAllocated {
				return d(SwapStableToCollateralStep, topupSwapReason, s.SquadsIdleRaw), true
			}
			if t.Stage == topupTrancheCollateral {
				if s.MinimumCollateralDepositRaw <= 0 || carry < s.MinimumCollateralDepositRaw {
					return d(Hold, "topup_collateral_below_deposit_window", 0), true
				}
				return d(OpenRouteStep, topupDepositReason, carry), true
			}
		}
		if s.SquadsIdleRaw != 0 || s.CollateralIdleRaw != carry || s.PrimeIdleRaw != carry {
			return Decision{}, false
		}
	} else {
		if s.DebtIdleRaw > 0 {
			return d(SwapDebtToUSDCStep, debtResidueSwapReason, s.DebtIdleRaw), true
		}
		if s.SquadsIdleRaw > 0 {
			if s.CollateralIdleRaw != 0 {
				return d(Hold, "topup_cash_beside_collateral_residue", 0), true
			}
			return d(SwapStableToCollateralStep, topupSwapReason, s.SquadsIdleRaw), true
		}
		if s.CollateralIdleRaw > 0 {
			if s.MinimumCollateralDepositRaw <= 0 || s.CollateralIdleRaw < s.MinimumCollateralDepositRaw {
				return Decision{}, false
			}
			return d(OpenRouteStep, topupDepositReason, s.CollateralIdleRaw), true
		}
	}
	if !s.PilotActive {
		return Decision{}, false
	}
	amount := min(s.VoltrIdleRaw-DefaultSelectorPolicy().IdleBufferRaw, workingTrancheCap(s), s.TopupDepositRoomRaw, int64(strategyTwoBridgeLegCapRaw))
	if amount < topupMinimumRaw {
		return Decision{}, false
	}
	return d(VoltrAllocateToSquads, topupAllocationReason, amount), true
}

// Select without assuming equal token decimals or a stablecoin peg. Values
// already use the NAV's floor(asset)/ceil(liability) rounding; apply the existing
// two-sided pricing margin too. This is planning, never quote authorization.
func payoffFundingSource(s Snapshot, upperDebt uint64) (Action, int64) {
	if debtCashRaw(s) < 0 || s.PositionDebtRaw <= 0 || s.PositionDebtValueRaw <= 0 || uint64(debtCashRaw(s)) >= upperDebt {
		return "", 0
	}
	need := new(big.Int).SetUint64(upperDebt - uint64(debtCashRaw(s)))
	need.Mul(need, big.NewInt(s.PositionDebtValueRaw))
	need.Mul(need, big.NewInt(int64(10_000+budgetPriceMarginBPS)))
	for _, source := range []struct {
		action        Action
		amount, value int64
	}{
		{SwapCollateralToDebtStep, s.CollateralIdleRaw, s.CollateralIdleValueRaw},
		{SwapUSDCToDebtStep, s.SquadsIdleRaw, s.SquadsIdleRaw},
	} {
		if source.action == SwapUSDCToDebtStep && sharedUSDCDebt(s.RouteLane) {
			continue
		}
		if source.amount <= 0 || source.value <= 0 {
			continue
		}
		available := new(big.Int).Mul(big.NewInt(source.value), big.NewInt(s.PositionDebtRaw))
		available.Mul(available, big.NewInt(int64(10_000-budgetPriceMarginBPS)))
		if available.Cmp(need) >= 0 {
			return source.action, source.amount
		}
	}
	return "", 0
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
	if s.HasPosition && s.LTVBPS >= hard {
		if s.PositionDebtRaw > 0 && s.DebtIdleRaw > 0 {
			return d(DeleverRouteStep, "hard_ltv_repay", min(s.PositionDebtRaw, s.DebtIdleRaw))
		}
		entry := d(SwapStableToCollateralStep, topupRiskEntryReason, s.SquadsIdleRaw)
		if emergencyTopupEntryInventory(s, entry) {
			return entry
		}
		if s.PositionDebtRaw > 0 && s.CollateralIdleRaw > 0 {
			return d(SwapCollateralToDebtStep, "hard_ltv_buffer_swap", s.CollateralIdleRaw)
		}
		if s.PositionDebtRaw > 0 && s.SquadsIdleRaw > 0 {
			return d(SwapUSDCToDebtStep, "hard_ltv_usdc_repayment_buffer", s.SquadsIdleRaw)
		}
		return d(HoldManualRecovery, "hard_ltv_without_repayment_buffer", 0)
	}
	// An admitted unwind whose sizing evidence went stale re-admits before any
	// further leg; only the recovery holds and the hard-LTV safety above it
	// may preempt.
	if s.UnwindRefreshRequired {
		return d(Hold, "unwind_requires_fresh_admission", 0)
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
		// B2 1.75x exit: repay the cycle's funding before another release.
		if action, reason, amount, ok := exitCycleStep(s); ok {
			return d(action, reason, amount)
		}
		if s.PositionDebtRaw > 0 {
			// A normal drain needs a full payoff. A principal-only or partial
			// buffer can fail the interest bound or KLend's residual-debt floor;
			// keep funding instead of repeatedly selecting that rejected repay.
			if s.DebtIdleRaw >= max(s.PositionDebtRaw, s.PayoffDebtRaw) {
				return d(DeleverRouteStep, "withdrawal_repay_debt", s.PositionDebtRaw)
			}
			action, amount := payoffFundingSource(s, uint64(max(s.PositionDebtRaw, s.PayoffDebtRaw)))
			if action == SwapCollateralToDebtStep {
				return d(action, "withdrawal_swap_repayment_buffer", amount)
			}
			if action == SwapUSDCToDebtStep {
				return d(action, "withdrawal_usdc_repayment_buffer", amount)
			}
			// This is the existing builder's transition marker; it computes a
			// safe receipt withdrawal from current prices/LTV, not one raw unit.
			return d(DeleverRouteStep, "withdrawal_release_repayment_collateral", 1)
		}
		if s.PositionCollateralRaw > 0 {
			return d(DeleverRouteStep, "withdrawal_withdraw_collateral", 0)
		}
		if s.CollateralIdleRaw > 0 {
			return d(SwapCollateralToStableStep, "withdrawal_convert_collateral_to_usdc", s.CollateralIdleRaw)
		}
		if s.DebtIdleRaw > 0 {
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
	// A debt buffer larger than the whole debt can only be left over from an
	// exit whose demand went away (a borrow never receives more than it owes).
	// Pay the debt off with it: the funded full-payoff admission needs no
	// collateral release, so this also works at an LTV where no release is safe.
	if s.PositionDebtRaw > 0 && s.DebtIdleRaw > s.PositionDebtRaw && s.DebtIdleRaw >= s.PayoffDebtRaw {
		return d(DeleverRouteStep, "idle_debt_repay", s.PositionDebtRaw)
	}
	// B2 down move to 1x: repay the position before any top-up or borrow.
	if action, reason, amount, ok := leverageDownStep(s); ok {
		return d(action, reason, amount)
	}
	if action, reason, amount, ok := leverageDownPartialStep(s); ok {
		return d(action, reason, amount)
	}
	// Plan B3 top-up tranche beside a funded debt-free position. It adds to
	// the current loop, so it needs no new-lane selector entry authority.
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
	// B2: borrow only toward the stored level target (never an entry quote).
	if action, reason, amount, ok := leverageBorrowStep(s, hard); ok {
		return d(action, reason, amount)
	}
	// The pause blocks new entries; a B2 leveraged loop on its own lane still
	// swaps and redeposits its borrowed cash (no entry authority is needed).
	if s.SelectorEntryPaused && !leverageLoopInProgress(s) {
		return d(Hold, "selector_entry_requires_fresh_admission", 0)
	}
	// Same prerequisite as the fixed lane: a deposit into a missing obligation
	// is refused, so no allocation, swap, or deposit is constructed. Reports and
	// withdrawal legs above stay live.
	if hold, absent := obligationPrerequisiteHold(s, initializationReady); absent {
		return hold
	}
	if !s.PolicyReady || !s.ExitBuildable {
		return d(Hold, "policy_or_exit_not_ready", 0)
	}
	if hard <= TargetLTVBPS {
		return d(HoldManualRecovery, "invalid_entry_ltv", 0)
	}
	if s.CollateralIdleRaw > 0 && s.MinimumCollateralDepositRaw == 0 {
		return d(Hold, "deposit_rounding_window_unavailable", 0)
	}
	depositReady := s.CollateralIdleRaw > 0 && s.CollateralIdleRaw >= s.MinimumCollateralDepositRaw
	if s.PositionDebtRaw > 0 {
		if s.DebtIdleRaw > 0 {
			return d(SwapDebtToCollateralStep, "borrowed_debt_requires_collateral_buffer", s.DebtIdleRaw)
		}
		if depositReady {
			return d(OpenRouteStep, "single_loop_redeposit", s.CollateralIdleRaw)
		}
		return d(Hold, "single_loop_position_ready", 0)
	}
	if depositReady {
		return d(OpenRouteStep, "collateral_ready", s.CollateralIdleRaw)
	}
	if s.PositionCollateralRaw > 0 {
		if leverageLane(s.RouteLane) {
			action, reason, amount := leverageDebtFreeStep(s)
			return d(action, reason, amount)
		}
		if s.BorrowUtilizationBlocked {
			return d(Hold, "debt_reserve_utilization_blocks_borrow", 0)
		}
		return d(OpenRouteStep, "collateral_requires_borrow", 1)
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
		amount := min(s.VoltrIdleRaw, limit)
		if s.PilotActive {
			// A pilot allocation is sized only by the admitted selector entry
			// equity, and that authority must still fit the working tranche cap
			// and the reviewed capacity above. A missing or oversized admission
			// holds for a fresh quote instead of spending unadmitted idle.
			amount = min(amount, workingTrancheCap(s))
			if s.SelectorEntryEquityRaw <= 0 || s.SelectorEntryEquityRaw > amount {
				return d(Hold, "selector_entry_amount_requires_fresh_quote", 0)
			}
			amount = s.SelectorEntryEquityRaw
		}
		return d(VoltrAllocateToSquads, "eligible_voltr_idle", amount)
	}
	return d(Hold, "no_eligible_action", 0)
}
