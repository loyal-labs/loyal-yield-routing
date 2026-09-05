package backyardrwa

import "fmt"

// The same single-loop lifecycle as the retained routes, with debt custody
// explicitly distinct from bridge USDC. No raw-unit cap or stablecoin peg is
// assumed here: admission must price the executable transaction and its exit.
func decideNonUSDC(s Snapshot) Decision {
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
	for _, value := range []int64{s.WithdrawalDemandRaw, s.SquadsIdleRaw, s.CollateralIdleRaw, s.DebtIdleRaw, s.PayoffDebtRaw,
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
		if s.PositionDebtRaw > 0 && s.CollateralIdleRaw > 0 {
			return d(SwapCollateralToDebtStep, "hard_ltv_buffer_swap", s.CollateralIdleRaw)
		}
		if s.PositionDebtRaw > 0 && s.SquadsIdleRaw > 0 {
			return d(SwapUSDCToDebtStep, "hard_ltv_usdc_repayment_buffer", s.SquadsIdleRaw)
		}
		return d(HoldManualRecovery, "hard_ltv_without_repayment_buffer", 0)
	}
	if s.PostMutationNAVRequired {
		return d(ReportNAV, "post_mutation_nav_due", 0)
	}
	// A requested canary drain stays a drain even when Voltr idle already covers
	// the withdrawal. Flat means every debt/collateral/bridge custody is cleared.
	if s.CutoverDrain || s.WithdrawalDemandRaw > 0 {
		if s.VoltrStrategyIdleRaw > 0 {
			return d(VoltrRestoreIdle, "withdrawal_staged", s.VoltrStrategyIdleRaw)
		}
		if s.PositionDebtRaw > 0 {
			// A normal drain needs a full payoff. A principal-only or partial
			// buffer can fail the interest bound or KLend's residual-debt floor;
			// keep funding instead of repeatedly selecting that rejected repay.
			if s.DebtIdleRaw >= max(s.PositionDebtRaw, s.PayoffDebtRaw) {
				return d(DeleverRouteStep, "withdrawal_repay_debt", s.PositionDebtRaw)
			}
			if s.CollateralIdleRaw > 0 {
				return d(SwapCollateralToDebtStep, "withdrawal_swap_repayment_buffer", s.CollateralIdleRaw)
			}
			if s.SquadsIdleRaw > 0 {
				return d(SwapUSDCToDebtStep, "withdrawal_usdc_repayment_buffer", s.SquadsIdleRaw)
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
			return d(ReportNAV, "withdrawal_terminal_nav_due", 0)
		}
		if s.StrategyNAVRaw != 0 {
			return d(HoldManualRecovery, "flat_custody_nonzero_nav", 0)
		}
		if s.WithdrawalDemandRaw > s.VoltrIdleRaw {
			return d(HoldManualRecovery, "withdrawal_conservation_shortfall", s.WithdrawalDemandRaw-s.VoltrIdleRaw)
		}
		return d(Hold, "canary_flat_nav_current", 0)
	}
	if s.CapitalMutated || s.LastReportAgeSeconds >= 60 {
		return d(ReportNAV, "nav_due", 0)
	}
	if !s.PolicyReady || !s.ExitBuildable {
		return d(Hold, "policy_or_exit_not_ready", 0)
	}
	if hard <= TargetLTVBPS {
		return d(HoldManualRecovery, "invalid_entry_ltv", 0)
	}
	if s.PositionDebtRaw > 0 {
		if s.DebtIdleRaw > 0 {
			return d(SwapDebtToCollateralStep, "borrowed_debt_requires_collateral_buffer", s.DebtIdleRaw)
		}
		if s.CollateralIdleRaw > 0 {
			return d(OpenRouteStep, "single_loop_redeposit", s.CollateralIdleRaw)
		}
		return d(Hold, "single_loop_position_ready", 0)
	}
	if s.CollateralIdleRaw > 0 {
		return d(OpenRouteStep, "collateral_ready", s.CollateralIdleRaw)
	}
	if s.PositionCollateralRaw > 0 {
		if s.BorrowUtilizationBlocked {
			return d(Hold, "debt_reserve_utilization_blocks_borrow", 0)
		}
		return d(OpenRouteStep, "collateral_requires_borrow", 1)
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
		return d(VoltrAllocateToSquads, "eligible_voltr_idle", min(s.VoltrIdleRaw, limit))
	}
	return d(Hold, "no_eligible_action", 0)
}
