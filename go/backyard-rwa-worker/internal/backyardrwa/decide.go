package backyardrwa

import "fmt"

// Decide resolves the already-frozen lane carried by the confirmed
// observation. It does not choose a lane; observations for any lane outside
// the two manifest-approved routes fail closed.
func Decide(s Snapshot) Decision {
	if s.RouteLane == SelectedRouteID {
		if s.CollateralIdleRaw >= 0 {
			s.PrimeIdleRaw = s.CollateralIdleRaw
		}
		// Any genuine Voltr withdrawal receipt drains the entire Phase 2 lane.
		// Keeping the receipt demand live until the selected position is flat
		// prevents new risk and gives terminal reconciliation one stable state.
		if s.WithdrawalDemandRaw > 0 {
			s.CutoverDrain = true
			if s.StrategyNAVRaw > s.WithdrawalDemandRaw {
				s.WithdrawalDemandRaw = s.StrategyNAVRaw
			}
			// The custom adaptor withdraw consumes the complete strategy custody,
			// even when Voltr's outer instruction carries a smaller amount. Never
			// execute it with more than one transaction cap staged.
			if s.VoltrStrategyIdleRaw > Phase2TransactionCapRaw {
				return Decision{Action: HoldManualRecovery, Reason: "voltr_restore_actual_effect_exceeds_cap", AmountRaw: 0,
					IdempotencyKey: fmt.Sprintf("%s:%s", s.ObservationID, "voltr_restore_actual_effect_exceeds_cap"), StrategyKey: SelectedRouteID}
			}
			// The finalized restore incident can leave Voltr's reported strategy
			// NAV stale even though every strategy custody is flat. A one-raw
			// allocation is the minimum installed-policy operation that consumes a
			// zero-NAV report; the withdrawal path below immediately stages and
			// restores that exact residue before declaring terminal custody.
			if !s.HasPosition && s.StrategyNAVRaw == 0 && s.PriorReportedNAVRaw > 0 &&
				s.SquadsIdleRaw == 0 && s.VoltrStrategyIdleRaw == 0 && s.VoltrIdleRaw > 0 {
				return Decision{Action: VoltrAllocateToSquads, Reason: "terminal_nav_reconciliation_probe", AmountRaw: 1,
					IdempotencyKey: fmt.Sprintf("%s:%s", s.ObservationID, "terminal_nav_reconciliation_probe"), StrategyKey: SelectedRouteID}
			}
			if s.VoltrStrategyIdleRaw > 0 {
				return Decision{Action: VoltrRestoreIdle, Reason: "withdrawal_staged", AmountRaw: s.VoltrStrategyIdleRaw,
					IdempotencyKey: fmt.Sprintf("%s:%s:%d", s.ObservationID, "withdrawal_staged", s.VoltrStrategyIdleRaw), StrategyKey: SelectedRouteID}
			}
			if s.SquadsIdleRaw > 0 && !s.HasPosition && s.PrimeIdleRaw == 0 {
				return Decision{Action: StageSquadsToVoltr, Reason: "withdrawal_terminal_residue", AmountRaw: s.SquadsIdleRaw,
					IdempotencyKey: fmt.Sprintf("%s:%s:%d", s.ObservationID, "withdrawal_terminal_residue", s.SquadsIdleRaw), StrategyKey: SelectedRouteID}
			}
			if s.WithdrawalDemandRaw <= s.VoltrIdleRaw && s.StrategyNAVRaw == 0 && s.PriorReportedNAVRaw == 0 &&
				!s.CapitalMutated && !s.PostMutationNAVRequired {
				return Decision{Action: Hold, Reason: "withdrawal_covered_terminal_nav_current", AmountRaw: 0,
					IdempotencyKey: fmt.Sprintf("%s:%s", s.ObservationID, "withdrawal_covered_terminal_nav_current"), StrategyKey: SelectedRouteID}
			}
		}
		decision := decideFixed(s)
		decision = neutralizeRouteAction(decision)
		if decision.AmountRaw > Phase2TransactionCapRaw {
			decision.AmountRaw = Phase2TransactionCapRaw
			decision.IdempotencyKey = fmt.Sprintf("%s:%s:%d:%s", s.ObservationID, decision.Action, decision.AmountRaw, decision.Reason)
		}
		decision.StrategyKey = SelectedRouteID
		return decision
	}
	if s.RouteLane != "" && s.RouteLane != RouteID {
		return Decision{Action: HoldManualRecovery, Reason: "unsupported_runtime_lane", AmountRaw: 0,
			IdempotencyKey: fmt.Sprintf("%s:%s", s.ObservationID, "unsupported_runtime_lane"), StrategyKey: s.RouteLane}
	}
	if s.CutoverDrain && s.WithdrawalDemandRaw == 0 {
		s.WithdrawalDemandRaw = s.StrategyNAVRaw
		if s.WithdrawalDemandRaw == 0 {
			s.WithdrawalDemandRaw = 1
		}
	}
	decision := decideFixed(s)
	decision.StrategyKey = RouteID
	return decision
}

func decideFixed(s Snapshot) Decision {
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
		s.PrimeIdleRaw < 0 || s.PositionCollateralRaw < 0 || s.PositionDebtRaw < 0 ||
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
			if s.PositionDebtRaw > 0 && s.SquadsIdleRaw > 0 {
				return decision(DeleverPrimeUSDCStep, "hard_ltv_repay", min(s.PositionDebtRaw, s.SquadsIdleRaw))
			}
			if s.PositionDebtRaw > 0 && s.PrimeIdleRaw > 0 {
				return decision(SwapPrimeToUSDCStep, "hard_ltv_buffer_swap", s.PrimeIdleRaw)
			}
			return decision(HoldManualRecovery, "hard_ltv_without_repayment_buffer", s.PositionDebtRaw)
		}
	}
	// A reconciled Jupiter/Kamino mutation must be accounted before any next
	// lifecycle leg, including a withdrawal unwind. Hard-LTV safety above is the
	// only action allowed to preempt this report.
	if s.PostMutationNAVRequired {
		return decision(ReportNAV, "post_mutation_nav_due", 0)
	}
	// A legacy position can sit at its maximum reviewed LTV, where withdrawing
	// collateral first is impossible. During the one-time Phase 2 cutover, use
	// newly deposited Voltr idle as the repayment buffer before attempting any
	// collateral release. Normal withdrawals retain their existing ordering.
	if s.CutoverDrain && s.PositionDebtRaw > 0 && s.SquadsIdleRaw == 0 && s.VoltrIdleRaw > 0 {
		return decision(VoltrAllocateToSquads, "phase2_cutover_fund_repayment", min(s.PositionDebtRaw, s.VoltrIdleRaw))
	}
	if s.WithdrawalDemandRaw > 0 {
		shortfall := s.WithdrawalDemandRaw - s.VoltrIdleRaw
		if shortfall <= 0 {
			if s.CapitalMutated || s.PostMutationNAVRequired || s.LastReportAgeSeconds >= 60 {
				return decision(ReportNAV, "withdrawal_covered_nav_due", 0)
			}
			return decision(Hold, "withdrawal_covered", 0)
		}
		if s.VoltrStrategyIdleRaw > 0 {
			// Voltr sweeps the strategy custody after the adaptor validates the
			// requested liquidity. Request and reconcile that full staged balance.
			return decision(VoltrRestoreIdle, "withdrawal_staged", s.VoltrStrategyIdleRaw)
		}
		remaining := shortfall - s.VoltrStrategyIdleRaw
		// Fully flatten Kamino before any Squads USDC is staged to Voltr. The
		// single-loop borrowed PRIME is the repayment buffer.
		if s.PositionDebtRaw > 0 {
			if s.SquadsIdleRaw > 0 {
				return decision(DeleverPrimeUSDCStep, "withdrawal_repay_debt", min(s.PositionDebtRaw, s.SquadsIdleRaw))
			}
			if s.PrimeIdleRaw > 0 {
				return decision(SwapPrimeToUSDCStep, "withdrawal_swap_repayment_buffer", s.PrimeIdleRaw)
			}
			return decision(DeleverPrimeUSDCStep, "withdrawal_release_repayment_collateral", 1)
		}
		if s.PositionCollateralRaw > 0 {
			amount := int64(0)
			reason := "withdrawal_withdraw_collateral"
			if s.CutoverDrain {
				amount = min(s.PositionCollateralRaw, Phase2TransactionCapRaw)
				reason = "phase2_cutover_withdraw_collateral"
			}
			return decision(DeleverPrimeUSDCStep, reason, amount)
		}
		if s.PrimeIdleRaw > 0 {
			return decision(SwapPrimeToUSDCStep, "withdrawal_swap_withdrawn_prime", s.PrimeIdleRaw)
		}
		if s.SquadsIdleRaw >= remaining {
			return decision(StageSquadsToVoltr, "withdrawal_demand", remaining)
		}
		remaining -= s.SquadsIdleRaw
		if !s.HasPosition {
			return decision(HoldManualRecovery, "withdrawal_conservation_shortfall", remaining)
		}
		return decision(DeleverPrimeUSDCStep, "withdrawal_shortfall", remaining)
	}
	if s.CapitalMutated || s.PostMutationNAVRequired || s.LastReportAgeSeconds >= 60 {
		return decision(ReportNAV, "nav_due", 0)
	}
	if s.VoltrIdleRaw > 0 {
		return decision(VoltrAllocateToSquads, "eligible_voltr_idle", s.VoltrIdleRaw)
	}
	if (s.SquadsIdleRaw > 0 || s.PrimeIdleRaw > 0 || s.PositionCollateralRaw > 0) && s.PolicyReady && s.ExitBuildable &&
		(s.LiquidationThresholdBPS <= 0 || hard <= TargetLTVBPS) {
		return decision(HoldManualRecovery, "invalid_entry_ltv", 0)
	}
	if s.PositionDebtRaw > 0 {
		if s.SquadsIdleRaw > 0 && s.PolicyReady && s.ExitBuildable {
			return decision(SwapUSDCToPrimeStep, "borrowed_usdc_requires_prime_buffer", s.SquadsIdleRaw)
		}
		if s.PrimeIdleRaw > 0 {
			return decision(OpenPrimeUSDCStep, "single_loop_redeposit", s.PrimeIdleRaw)
		}
		return decision(Hold, "single_loop_position_ready", 0)
	}
	// PRIME is the collateral asset. Fresh USDC is converted before the only
	// collateral deposit.
	if s.SquadsIdleRaw > 0 && s.PolicyReady && s.ExitBuildable {
		if s.CapacityRaw <= 0 || s.PolicyLimitRaw <= 0 || s.MaxTargetLTVEntryRaw <= 0 {
			return decision(Hold, "insufficient_reviewed_entry_capacity", 0)
		}
		amount := s.SquadsIdleRaw
		amount = min(amount, s.PolicyLimitRaw)
		amount = min(amount, s.CapacityRaw)
		amount = min(amount, s.MaxTargetLTVEntryRaw)
		return decision(SwapUSDCToPrimeStep, "usdc_requires_prime_collateral", amount)
	}
	if s.PrimeIdleRaw > 0 && s.PolicyReady && s.ExitBuildable {
		return decision(OpenPrimeUSDCStep, "prime_collateral_ready", s.PrimeIdleRaw)
	}
	if s.PositionCollateralRaw > 0 && s.PositionDebtRaw == 0 && s.BorrowUtilizationBlocked {
		return decision(Hold, "debt_reserve_utilization_blocks_borrow", 0)
	}
	// A collateral-only intermediate state needs the borrow leg even though no
	// idle token amount drives that instruction. The builder computes its exact
	// amount from the refreshed reserve prices; AmountRaw=1 is only the durable
	// state-transition marker and is never used as the borrow wire amount.
	if s.PositionCollateralRaw > 0 && s.PositionDebtRaw == 0 && s.PolicyReady && s.ExitBuildable {
		return decision(OpenPrimeUSDCStep, "prime_collateral_requires_borrow", 1)
	}
	return decision(Hold, "no_eligible_action", 0)
}
