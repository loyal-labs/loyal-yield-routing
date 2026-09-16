package backyardrwa

import "fmt"

// custodyDiscipline fails closed when the strategy custody balance is not
// exactly the amount this worker last staged into Voltr for the route. Voltr
// sweeps the entire custody on restore and books that balance into its own
// total value, so an unexplained balance is manual recovery and never an
// amount the planner may infer from a custody read.
func custodyDiscipline(s Snapshot) (Decision, bool) {
	if s.Nonterminal != "" || s.VoltrStrategyIdleRaw <= 0 {
		return Decision{}, false
	}
	if !s.StagedAmountKnown || s.StagedAmountRaw != s.VoltrStrategyIdleRaw {
		strategyKey := s.RouteLane
		if strategyKey == "" {
			strategyKey = RouteID
		}
		return Decision{Action: HoldManualRecovery, Reason: "custody_mismatch", AmountRaw: 0,
			IdempotencyKey: fmt.Sprintf("%s:%s:%d", s.ObservationID, "custody_mismatch", s.VoltrStrategyIdleRaw),
			StrategyKey:    strategyKey}, true
	}
	return Decision{}, false
}

// custodyResidueHold blocks every accounting refresh while a strategy custody
// balance exists. A report moves no capital, so it must observe custody empty;
// a residue has to be restored (or recovered manually) instead of being
// silently reported as current book.
func custodyResidueHold(s Snapshot) (Decision, bool) {
	if s.VoltrStrategyIdleRaw == 0 {
		return Decision{}, false
	}
	strategyKey := s.RouteLane
	if strategyKey == "" {
		strategyKey = RouteID
	}
	return Decision{Action: HoldManualRecovery, Reason: "custody_residue", AmountRaw: 0,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d", s.ObservationID, "custody_residue", s.VoltrStrategyIdleRaw),
		StrategyKey:    strategyKey}, true
}

// installedDecisionLane reports whether a confirmed observation frozen on
// this lane may be resolved by Decide: the pinned legacy Prime route plus the
// basic policy lanes. The remaining runtimeRoute catalog entries stay
// observation-only, so they keep failing closed here instead of reusing the
// legacy Prime decision path.
func installedDecisionLane(lane string) bool {
	if lane == RouteID {
		return true
	}
	route, err := runtimeRoute(lane)
	return err == nil && route.BasicPolicy
}

// Decide resolves the already-frozen lane carried by the confirmed
// observation. It does not choose a lane; observations for any lane outside
// the registered routes fail closed. Registration does not enable the live
// selection manifest or replace policy/exit/admission checks.
func Decide(s Snapshot) Decision {
	if hold, blocked := custodyDiscipline(s); blocked {
		return hold
	}
	if hold, blocked := bridgeMonitorHold(s); blocked {
		return hold
	}
	if route, err := runtimeRoute(s.RouteLane); err == nil && route.Kamino.DebtMint != bridgeUSDC && len(route.KaminoPolicies) == 4 {
		return decideNonUSDC(s)
	}

	if s.RouteLane != "" && !installedDecisionLane(s.RouteLane) {
		return Decision{Action: HoldManualRecovery, Reason: "unsupported_runtime_lane", AmountRaw: 0,
			IdempotencyKey: fmt.Sprintf("%s:%s", s.ObservationID, "unsupported_runtime_lane"), StrategyKey: s.RouteLane}
	}
	// Legacy snapshots are normalized here; typed lanes use collateral directly.
	if s.RouteLane == "" || s.RouteLane == RouteID {
		s.CollateralIdleRaw = s.PrimeIdleRaw
	}
	decision := decideUSDC(s)
	decision.StrategyKey = s.RouteLane
	if decision.StrategyKey == "" {
		decision.StrategyKey = RouteID
	}
	if s.RouteLane == "" || s.RouteLane == RouteID {
		decision.Action = legacyUSDCAction(decision.Action)
	}
	if !s.PilotActive && s.RouteLane == SelectedRouteID && decision.AmountRaw > Phase2TransactionCapRaw {
		// Restore consumes all staged custody; never conceal its actual effect.
		if decision.Action == VoltrRestoreIdle {
			decision.Action, decision.Reason, decision.AmountRaw = HoldManualRecovery, "voltr_restore_actual_effect_exceeds_cap", 0
		} else if decision.Action == StageSquadsToVoltr {
			decision.Action, decision.Reason, decision.AmountRaw = HoldManualRecovery, "full_custody_exit_exceeds_transaction_cap", 0
		} else {
			decision.AmountRaw = Phase2TransactionCapRaw
		}
		decision.IdempotencyKey = fmt.Sprintf("%s:%s:%d:%s", s.ObservationID, decision.Action, decision.AmountRaw, decision.Reason)
	}
	return decision
}

// obligationAbsentHoldReason marks an observed prerequisite, never a policy
// failure and never unexplained NAV drift: the lane's Kamino obligation account
// does not exist, so Kamino refuses the deposit entry planning would fund. It
// is a plain hold that clears by itself once the account exists.
const obligationAbsentHoldReason = "obligation_absent"

func obligationPrerequisiteHold(s Snapshot) (Decision, bool) {
	if !s.ObligationPresenceKnown || s.ObligationPresent {
		return Decision{}, false
	}
	strategyKey := s.RouteLane
	if strategyKey == "" {
		strategyKey = RouteID
	}
	return Decision{Action: Hold, Reason: obligationAbsentHoldReason, AmountRaw: 0, StrategyKey: strategyKey,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d", s.ObservationID, obligationAbsentHoldReason, 0)}, true
}

func decideUSDC(s Snapshot) Decision {
	decision := func(action Action, reason string, amount int64) Decision {
		return Decision{
			Action:    action,
			Reason:    reason,
			AmountRaw: amount,
			// ObservationID binds the economic state, while Slot remains audit
			// evidence. The same state re-read at a later slot must resolve to the
			// same durable decision instead of creating an endless journal stream.
			IdempotencyKey: fmt.Sprintf("%s:%s:%d:%s", s.ObservationID, action, amount, reason),
		}
	}
	if s.Nonterminal != "" {
		reason := "resume_nonterminal_operation"
		if s.HasAmbiguousSubmission {
			reason = "recover_ambiguous_submission"
		}
		return decision(RecoverTransaction, reason, 0)
	}
	if s.ManualReason != "" || s.RouteKind != RouteKind || !s.Fresh ||
		s.ObservationID == "" || s.Slot <= 0 || s.LastReportAgeSeconds < 0 ||
		s.WithdrawalDemandRaw < 0 || s.SquadsIdleRaw < 0 ||
		s.CollateralIdleRaw < 0 || s.PositionCollateralRaw < 0 || s.PositionDebtRaw < 0 ||
		s.PositionCollateralValueRaw < 0 || s.PositionDebtValueRaw < 0 || s.StrategyNAVRaw < 0 ||
		s.VoltrStrategyIdleRaw < 0 || s.VoltrIdleRaw < 0 ||
		s.LTVBPS < 0 || s.CapacityRaw < 0 || s.PolicyLimitRaw < 0 || s.MaxTargetLTVEntryRaw < 0 {
		return decision(HoldManualRecovery, "invalid_or_incoherent_snapshot", 0)
	}
	hard := s.LiquidationThresholdBPS - 1500
	if hard > 6000 {
		hard = 6000
	}
	if s.HasPosition {
		if s.LiquidationThresholdBPS <= 0 || hard <= TargetLTVBPS {
			return decision(HoldManualRecovery, "invalid_hard_ltv", 0)
		}
		if s.LTVBPS >= hard {
			if selectorLane(s.RouteLane) {
				payoff := max(s.PositionDebtRaw, s.PayoffDebtRaw)
				if s.PositionDebtRaw > 0 && debtCashRaw(s) >= payoff {
					return decision(DeleverRouteStep, "hard_ltv_repay", s.PositionDebtRaw)
				}
				if s.PositionDebtRaw > 0 {
					action, amount := payoffFundingSource(s, uint64(payoff))
					if amount > 0 {
						return decision(action, "hard_ltv_buffer_swap", amount)
					}
				}
				return decision(HoldManualRecovery, "hard_ltv_partial_repayment_requires_admission", 0)
			}
			if s.PositionDebtRaw > 0 && s.SquadsIdleRaw > 0 {
				return decision(DeleverRouteStep, "hard_ltv_repay", min(s.PositionDebtRaw, s.SquadsIdleRaw))
			}
			if s.PositionDebtRaw > 0 && s.CollateralIdleRaw > 0 {
				return decision(SwapCollateralToDebtStep, "hard_ltv_buffer_swap", s.CollateralIdleRaw)
			}
			return decision(HoldManualRecovery, "hard_ltv_without_repayment_buffer", s.PositionDebtRaw)
		}
	}
	// Finish a journal-explained stage even if deposits or claims have since
	// changed demand. Reporting cannot reconcile cash still in this custody.
	if s.VoltrStrategyIdleRaw > 0 {
		return decision(VoltrRestoreIdle, "withdrawal_staged", s.VoltrStrategyIdleRaw)
	}
	// A reconciled Jupiter/Kamino mutation must be accounted before any next
	// lifecycle leg, including a withdrawal unwind. Hard-LTV safety above is the
	// only action allowed to preempt this report.
	if s.PostMutationNAVRequired && !withdrawalIdleUnderfunded(s) {
		// M4: a pending withdrawal queue that Voltr idle cannot cover makes a
		// refresh-only tick inadmissible; the unwind legs below stay live.
		if hold, blocked := custodyResidueHold(s); blocked {
			return hold
		}
		return decision(ReportNAV, "post_mutation_nav_due", 0)
	}
	// A legacy position can sit at its maximum reviewed LTV, where withdrawing
	// collateral first is impossible. During the one-time Phase 2 cutover, use
	// newly deposited Voltr idle as the repayment buffer before attempting any
	// collateral release. Normal withdrawals retain their existing ordering.
	if s.CutoverDrain && s.PositionDebtRaw > 0 && s.SquadsIdleRaw == 0 && s.VoltrIdleRaw > 0 {
		return decision(VoltrAllocateToSquads, "phase2_cutover_fund_repayment", min(s.PositionDebtRaw, s.VoltrIdleRaw))
	}
	if s.WithdrawalDemandRaw > 0 || s.Unwind || s.CutoverDrain {
		shortfall := s.WithdrawalDemandRaw - s.VoltrIdleRaw
		if shortfall <= 0 && !s.Unwind && !s.CutoverDrain {
			if s.CapitalMutated || s.PostMutationNAVRequired || s.LastReportAgeSeconds >= 60 {
				if hold, blocked := custodyResidueHold(s); blocked {
					return hold
				}
				return decision(ReportNAV, "withdrawal_covered_nav_due", 0)
			}
			return decision(Hold, "withdrawal_covered", 0)
		}

		remaining := max(int64(0), shortfall-s.VoltrStrategyIdleRaw)
		// Fully flatten Kamino before any Squads USDC is staged to Voltr. The
		// single-loop borrowed PRIME is the repayment buffer.
		if s.PositionDebtRaw > 0 {
			if selectorLane(s.RouteLane) {
				if debtCashRaw(s) >= max(s.PositionDebtRaw, s.PayoffDebtRaw) {
					return decision(DeleverRouteStep, "withdrawal_repay_debt", s.PositionDebtRaw)
				}
				action, amount := payoffFundingSource(s, uint64(max(s.PositionDebtRaw, s.PayoffDebtRaw)))
				if amount > 0 {
					return decision(action, "withdrawal_swap_repayment_buffer", amount)
				}
				return decision(DeleverRouteStep, "withdrawal_release_repayment_collateral", 1)
			}
			if s.SquadsIdleRaw > 0 {
				return decision(DeleverRouteStep, "withdrawal_repay_debt", min(s.PositionDebtRaw, s.SquadsIdleRaw))
			}
			if s.CollateralIdleRaw > 0 {
				return decision(SwapCollateralToDebtStep, "withdrawal_swap_repayment_buffer", s.CollateralIdleRaw)
			}
			return decision(DeleverRouteStep, "withdrawal_release_repayment_collateral", 1)
		}
		if s.PositionCollateralRaw > 0 {
			amount := int64(0)
			reason := "withdrawal_withdraw_collateral"
			if s.CutoverDrain {
				amount = min(s.PositionCollateralRaw, Phase2TransactionCapRaw)
				reason = "phase2_cutover_withdraw_collateral"
			}
			return decision(DeleverRouteStep, reason, amount)
		}
		if s.CollateralIdleRaw > 0 {
			return decision(SwapCollateralToStableStep, "withdrawal_swap_withdrawn_prime", s.CollateralIdleRaw)
		}
		if (s.Unwind || s.CutoverDrain) && s.SquadsIdleRaw > 0 {
			return decision(StageSquadsToVoltr, "unwind_return_cash", s.SquadsIdleRaw)
		}
		if remaining == 0 {
			return decision(Hold, "unwind_complete", 0)
		}
		if s.SquadsIdleRaw >= remaining {
			if selectorLane(s.RouteLane) {
				remaining = s.SquadsIdleRaw
			} // Existing bridge admission returns full custody.
			return decision(StageSquadsToVoltr, "withdrawal_demand", remaining)
		}
		remaining -= s.SquadsIdleRaw
		if !s.HasPosition {
			return decision(HoldManualRecovery, "withdrawal_conservation_shortfall", remaining)
		}
		return decision(DeleverRouteStep, "withdrawal_shortfall", remaining)
	}
	// S1: an unexplained book drift stops the route instead of reporting it.
	if hold, drifted := unexplainedNAVDriftHold(s); drifted {
		return hold
	}
	// S2: a reconciled capital mutation reports only beyond the drift
	// tolerance; the post-mutation requirement and the aging cadence report
	// unconditionally.
	if capitalMutationReports(s) || s.PostMutationNAVRequired || s.LastReportAgeSeconds >= 60 {
		if hold, blocked := custodyResidueHold(s); blocked {
			return hold
		}
		return decision(ReportNAV, "nav_due", 0)
	}
	// Returning flat working cash is an exit. It does not need a usable
	// entry market, an obligation account, or an entry LTV threshold.
	if selectorLane(s.RouteLane) && !s.HasPosition && s.PositionCollateralRaw == 0 && s.PositionDebtRaw == 0 && s.CollateralIdleRaw == 0 && s.SquadsIdleRaw > 0 &&
		(s.CapacityRaw < s.SquadsIdleRaw || s.PolicyLimitRaw < s.SquadsIdleRaw || s.MaxTargetLTVEntryRaw < s.SquadsIdleRaw || s.LiquidationThresholdBPS <= 0 || hard <= TargetLTVBPS) {
		return decision(StageSquadsToVoltr, "entry_capacity_changed_return_cash", s.SquadsIdleRaw)
	}
	// Entry planning funds a Kamino deposit. Without the obligation account that
	// deposit is refused and the funded capital strands in custody, so no
	// allocation, swap, or deposit is constructed. Withdrawal and reporting legs
	// above stay live.
	if s.SelectorEntryPaused {
		return decision(Hold, "selector_entry_requires_fresh_admission", 0)
	}
	if hold, absent := obligationPrerequisiteHold(s); absent {
		return hold
	}
	if s.VoltrIdleRaw > 0 && !selectorLane(s.RouteLane) {
		return decision(VoltrAllocateToSquads, "eligible_voltr_idle", s.VoltrIdleRaw)
	}
	// Keep undeployed capital in Voltr. Squads cash is working cash for one
	// complete tranche, so a later deposit cannot be mistaken for borrowed cash.
	if selectorLane(s.RouteLane) && s.VoltrIdleRaw > 0 && !s.HasPosition && s.PositionCollateralRaw == 0 && s.PositionDebtRaw == 0 && s.SquadsIdleRaw == 0 && s.CollateralIdleRaw == 0 {
		if !s.PolicyReady || !s.ExitBuildable || s.CapacityRaw <= 0 || s.PolicyLimitRaw <= 0 || s.MaxTargetLTVEntryRaw <= 0 {
			return decision(Hold, "insufficient_reviewed_entry_capacity", 0)
		}
		return decision(VoltrAllocateToSquads, "eligible_voltr_idle", min(s.VoltrIdleRaw, s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw, workingTrancheCap(s)))
	}
	if (s.SquadsIdleRaw > 0 || s.CollateralIdleRaw > 0 || s.PositionCollateralRaw > 0) && s.PolicyReady && s.ExitBuildable &&
		(s.LiquidationThresholdBPS <= 0 || hard <= TargetLTVBPS) {
		return decision(HoldManualRecovery, "invalid_entry_ltv", 0)
	}
	if s.PositionDebtRaw > 0 {
		if s.SquadsIdleRaw > 0 && s.PolicyReady && s.ExitBuildable {
			return decision(SwapDebtToCollateralStep, "borrowed_usdc_requires_prime_buffer", s.SquadsIdleRaw)
		}
		if s.CollateralIdleRaw > 0 {
			return decision(OpenRouteStep, "single_loop_redeposit", s.CollateralIdleRaw)
		}
		return decision(Hold, "single_loop_position_ready", 0)
	}
	if selectorLane(s.RouteLane) && s.SquadsIdleRaw > 0 && (s.CollateralIdleRaw > 0 || s.PositionCollateralRaw > 0) {
		return decision(HoldManualRecovery, "entry_tranche_contains_unassigned_cash", 0)
	}
	// PRIME is the collateral asset. Fresh USDC is converted before the only
	// collateral deposit.
	if s.SquadsIdleRaw > 0 && s.PolicyReady && s.ExitBuildable {
		if s.CapacityRaw <= 0 || s.PolicyLimitRaw <= 0 || s.MaxTargetLTVEntryRaw <= 0 {
			if selectorLane(s.RouteLane) {
				return decision(StageSquadsToVoltr, "entry_capacity_changed_return_cash", s.SquadsIdleRaw)
			}
			return decision(Hold, "insufficient_reviewed_entry_capacity", 0)
		}
		amount := s.SquadsIdleRaw
		amount = min(amount, s.PolicyLimitRaw)
		amount = min(amount, s.CapacityRaw)
		amount = min(amount, s.MaxTargetLTVEntryRaw)
		if selectorLane(s.RouteLane) && amount != s.SquadsIdleRaw {
			return decision(StageSquadsToVoltr, "entry_capacity_changed_return_cash", s.SquadsIdleRaw)
		}
		return decision(SwapStableToCollateralStep, "usdc_requires_prime_collateral", amount)
	}
	if s.CollateralIdleRaw > 0 && s.PolicyReady && s.ExitBuildable {
		return decision(OpenRouteStep, "prime_collateral_ready", s.CollateralIdleRaw)
	}
	if s.PositionCollateralRaw > 0 && s.PositionDebtRaw == 0 && s.BorrowUtilizationBlocked {
		return decision(Hold, "debt_reserve_utilization_blocks_borrow", 0)
	}
	// A collateral-only intermediate state needs the borrow leg even though no
	// idle token amount drives that instruction. The builder computes its exact
	// amount from the refreshed reserve prices; AmountRaw=1 is only the durable
	// state-transition marker and is never used as the borrow wire amount.
	if s.PositionCollateralRaw > 0 && s.PositionDebtRaw == 0 && s.PolicyReady && s.ExitBuildable {
		return decision(OpenRouteStep, "prime_collateral_requires_borrow", 1)
	}
	return decision(Hold, "no_eligible_action", 0)
}
