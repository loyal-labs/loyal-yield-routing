package backyardrwa

import "fmt"

func Decide(s Snapshot) Decision {
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
			return decision(DeleverPrimeUSDCStep, "hard_ltv", 0)
		}
	}
	if s.WithdrawalDemandRaw > 0 {
		shortfall := s.WithdrawalDemandRaw - s.VoltrIdleRaw
		if shortfall <= 0 {
			if s.CapitalMutated || s.LastReportAgeSeconds >= 60 {
				return decision(ReportNAV, "withdrawal_covered_nav_due", 0)
			}
			return decision(Hold, "withdrawal_covered", 0)
		}
		if s.VoltrStrategyIdleRaw >= shortfall {
			return decision(VoltrRestoreIdle, "withdrawal_staged", shortfall)
		}
		remaining := shortfall - s.VoltrStrategyIdleRaw
		if s.SquadsIdleRaw >= remaining {
			return decision(StageSquadsToVoltr, "withdrawal_demand", remaining)
		}
		if !s.HasPosition {
			return decision(HoldManualRecovery, "withdrawal_conservation_shortfall", remaining-s.SquadsIdleRaw)
		}
		return decision(DeleverPrimeUSDCStep, "withdrawal_shortfall", remaining-s.SquadsIdleRaw)
	}
	if s.CapitalMutated || s.LastReportAgeSeconds >= 60 {
		return decision(ReportNAV, "nav_due", 0)
	}
	if s.VoltrIdleRaw > 0 {
		return decision(VoltrAllocateToSquads, "eligible_voltr_idle", s.VoltrIdleRaw)
	}
	if s.LTVBPS < TargetLTVBPS && s.NetAPYBPS > 0 && s.CapacityRaw > 0 && s.PolicyReady && s.ExitBuildable {
		if s.LiquidationThresholdBPS <= 0 || hard <= TargetLTVBPS {
			return decision(HoldManualRecovery, "invalid_entry_ltv", 0)
		}
		amount := min(s.SquadsIdleRaw, s.CapacityRaw)
		amount = min(amount, s.MaxTargetLTVEntryRaw)
		if s.PolicyLimitRaw > 0 {
			amount = min(amount, s.PolicyLimitRaw)
		}
		if amount > 0 {
			return decision(OpenPrimeUSDCStep, "positive_apy_entry", amount)
		}
	}
	return decision(Hold, "no_eligible_action", 0)
}
