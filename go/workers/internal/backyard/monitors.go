package backyard

import (
	"fmt"
	"math/big"
)

func bridgeMonitorHold(s Snapshot) (Decision, bool) {
	if !s.MonitorsArmed || s.Nonterminal != "" {
		return Decision{}, false
	}
	if s.StrategyReceiptIntegrityFault {
		return monitorHold(s, "strategy_receipt_integrity"), true
	}
	if s.ForeignInflowFault {
		return monitorHold(s, "foreign_inflow_unexplained"), true
	}
	// A stage in flight moves Squads cash into the strategy custody ATA without
	// invoking Voltr, so Voltr's total value, receipt and tracked custody do
	// not move: the ATA balance must stay out of this identity.
	book := new(big.Int).Add(big.NewInt(s.VoltrTotalValueRaw), big.NewInt(s.AcknowledgedForeignIdleRaw))
	assets := new(big.Int).Add(big.NewInt(s.VoltrIdleRaw), big.NewInt(s.PriorReportedNAVRaw))
	assets.Add(assets, big.NewInt(s.VoltrReceiptCustodyTrackedRaw))
	if s.VoltrTotalValueRaw < 0 || s.AcknowledgedForeignIdleRaw < 0 || s.VoltrIdleRaw < 0 ||
		s.PriorReportedNAVRaw < 0 || s.VoltrReceiptCustodyTrackedRaw < 0 || book.Cmp(assets) != 0 {
		return monitorHold(s, "book_identity_mismatch"), true
	}
	if s.StageTransient {
		if !s.StagedAmountKnown || s.VoltrReceiptCustodyTrackedRaw != 0 || s.ForeignCustodyRaw < 0 || s.VoltrStrategyIdleRaw < s.ForeignCustodyRaw || s.VoltrStrategyIdleRaw-s.ForeignCustodyRaw != s.StagedAmountRaw {
			return monitorHold(s, "custody_transient_mismatch"), true
		}
	} else if s.ForeignCustodyRaw < 0 || s.VoltrStrategyIdleRaw != s.ForeignCustodyRaw || (s.VoltrReceiptCustodyTrackedRaw != 0 && s.VoltrReceiptCustodyTrackedRaw != s.ForeignCustodyRaw) {
		return monitorHold(s, "custody_residue"), true
	}
	if s.JournalArmedNAVMalformed {
		return monitorHold(s, "armed_nav_malformed"), true
	}
	if s.JournalArmedNAVReturnDataMissing {
		return monitorHold(s, "armed_nav_missing"), true
	}
	if !s.PostMutationNAVRequired && s.JournalArmedNAVKnown &&
		(s.JournalArmedNAVRaw < 0 || uint64(s.JournalArmedNAVRaw) != uint64(s.PriorReportedNAVRaw)) {
		return monitorHold(s, "receipt_nav_mismatch"), true
	}
	if s.FeeAccumulatorRaw < 0 || s.LPSupplyInclFeesRaw < 0 || s.FeeAccumulatorRaw > s.LPSupplyInclFeesRaw {
		return monitorHold(s, "fee_accounting_invalid"), true
	}
	if !approvedVoltrFeeTerms(s) {
		return monitorHold(s, "voltr_fee_terms_unapproved"), true
	}
	if !s.JournalSequenceKnown {
		if s.TicketLastConsumedSequenceRaw != 0 {
			return monitorHold(s, "out_of_band_crank"), true
		}
	} else if uint64(s.JournalReconciledSequenceRaw) != uint64(s.TicketLastConsumedSequenceRaw) {
		return monitorHold(s, "out_of_band_crank"), true
	}
	if !s.ProgramIdentityKnown {
		return monitorHold(s, "program_identity_unverified"), true
	}
	if s.VoltrProgramDeploySlot != voltrProgramDeploySlot || s.AdaptorProgramDeploySlot != adaptorProgramDeploySlot {
		return monitorHold(s, "program_identity_changed"), true
	}
	return Decision{}, false
}

func withdrawalIdleUnderfunded(s Snapshot) bool {
	return s.MonitorsArmed && withdrawalShortfallRaw(s) > 0
}

func unexplainedNAVDriftHold(s Snapshot) (Decision, bool) {
	if !s.MonitorsArmed || s.PostMutationNAVRequired || s.CapitalMutated {
		return Decision{}, false
	}
	if navDriftOutsideBand(s) {
		return monitorHold(s, "nav_drift_unexplained"), true
	}
	return Decision{}, false
}

func capitalMutationReports(s Snapshot) bool {
	return s.CapitalMutated && navDriftOutsideBand(s)
}

const capitalMutationReportAgeSeconds int64 = 60

func scheduledNAVReportDue(s Snapshot) bool {
	return s.LastReportAgeSeconds >= int64(routineNAVReportInterval.Seconds()) ||
		(s.CapitalMutated && s.LastReportAgeSeconds >= capitalMutationReportAgeSeconds)
}

func trustfulReportDue(s Snapshot) bool {
	return navDriftOutsideBand(s) ||
		s.LastReportAgeSeconds >= int64(routineNAVReportInterval.Seconds())
}

func navDriftOutsideBand(s Snapshot) bool {
	return navDriftOutsideTolerance(uint64(s.StrategyNAVRaw)+uint64(s.ExitLiabilityRaw), uint64(s.PriorReportedNAVRaw)+uint64(s.JournalExitAtReportRaw))
}

func navDriftOutsideTolerance(observed, reported uint64) bool {
	bound := reported * uint64(navDriftToleranceBPS) / 10_000
	if bound < uint64(navDriftFloorRaw) {
		bound = uint64(navDriftFloorRaw)
	}
	if observed > reported {
		return observed-reported > bound
	}
	return reported-observed > bound
}

func approvedVoltrFeeTerms(s Snapshot) bool {
	return s.AdminPerformanceFeeBPS == s.ApprovedAdminPerformanceFeeBPS &&
		s.ManagerPerformanceFeeBPS == 0 && s.ManagerManagementFeeBPS == 0 && s.AdminManagementFeeBPS == 0 &&
		s.RedemptionFeeBPS == 0 && s.IssuanceFeeBPS == s.ApprovedIssuanceFeeBPS && s.ProtocolPerformanceFeeBPS == 0 && s.ProtocolManagementFeeBPS == 0
}

func feeAccumulatorNeedsWarning(fees, supply uint64) bool {
	if supply == 0 {
		return fees > 0
	}
	bound := new(big.Int).Mul(new(big.Int).SetUint64(supply), big.NewInt(feeAccumulatorWarningBPS))
	fee := new(big.Int).Mul(new(big.Int).SetUint64(fees), big.NewInt(10_000))
	return fee.Cmp(bound) > 0
}

func monitorHold(s Snapshot, reason string) Decision {
	return Decision{
		Action: HoldManualRecovery, Reason: reason, AmountRaw: 0, StrategyKey: s.RouteLane,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d", s.ObservationID, reason, s.Slot),
	}
}
