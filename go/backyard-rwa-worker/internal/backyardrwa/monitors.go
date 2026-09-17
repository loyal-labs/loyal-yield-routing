package backyardrwa

import (
	"fmt"
	"math/big"
)

// bridgeMonitorHold evaluates the fail-closed Phase 2 monitors against one
// confirmed snapshot. Every monitor compares values decoded from a single
// coherent account batch against either this worker's own journal or a pinned
// constant; none of them plan or infer an amount. A violation stops the tick
// with HOLD_MANUAL_RECOVERY rather than a decode error, so an economic fault
// is visible as a durable journal decision instead of a crash loop.
//
// Monitors only gate new decisions: an in-flight operation resumes through the
// nonterminal path first, because its Squads sequence and custody state are
// explained by the operation that is still being reconciled.
func bridgeMonitorHold(s Snapshot) (Decision, bool) {
	if !s.MonitorsArmed || s.Nonterminal != "" {
		return Decision{}, false
	}
	// Receipt integrity. The strategy receipt is the account every other book
	// input is read from, so a confirmed batch in which that receipt is absent,
	// foreign-owned, or the wrong length holds durably before anything else is
	// compared, exactly like an unverified program identity.
	if s.StrategyReceiptIntegrityFault {
		return monitorHold(s, "strategy_receipt_integrity"), true
	}
	// M1: Voltr's own book identity, closed with the custody Voltr itself books
	// in the receipt (offset 128). A stage in flight moves Squads cash into the
	// strategy custody ATA without invoking Voltr, so tv, the receipt position,
	// and the tracked custody are all unchanged while the ATA is nonzero: the
	// observed ATA balance must therefore never appear in this identity.
	if uint64(s.VoltrTotalValueRaw) != uint64(s.VoltrIdleRaw)+uint64(s.PriorReportedNAVRaw)+uint64(s.VoltrReceiptCustodyTrackedRaw) {
		return monitorHold(s, "book_identity_mismatch"), true
	}
	// M3: custody discipline. At rest both custody views are empty. Between the
	// stage and restore legs of a journaled stage the ATA holds exactly the
	// staged amount while Voltr, which was never invoked, still books zero.
	if s.StageTransient {
		if !s.StagedAmountKnown || s.VoltrReceiptCustodyTrackedRaw != 0 || s.VoltrStrategyIdleRaw != s.StagedAmountRaw {
			return monitorHold(s, "custody_transient_mismatch"), true
		}
	} else if s.VoltrStrategyIdleRaw != 0 || s.VoltrReceiptCustodyTrackedRaw != 0 {
		return monitorHold(s, "custody_residue"), true
	}
	// M2: the receipt must still carry the NAV this worker armed. That armed NAV
	// is the adaptor return data of the latest reconciled ticket-consuming
	// operation of any kind, so a reconciled row without it is a durable hold,
	// never a silent disarm. The armed-versus-receipt comparison itself stays
	// deferred while a reconciled capital mutation awaits its own report, since
	// the receipt is then legitimately stale.
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
	// M7: fee discipline. Un-harvested LP fee accumulators stay a bounded share
	// of the supply Voltr prices deposits and claims against, and no
	// performance fee may be switched on until its bps terms are calibrated and
	// reviewed.
	if feeAccumulatorOutsideBound(uint64(s.FeeAccumulatorRaw), uint64(s.LPSupplyInclFeesRaw)) {
		return monitorHold(s, "fee_accumulator_anomaly"), true
	}
	if s.ManagerPerformanceFeeBPS != 0 || s.AdminPerformanceFeeBPS != 0 {
		return monitorHold(s, "performance_fee_enabled"), true
	}
	// M8: the report ticket may only ever be consumed by this worker's own
	// reconciled bridge operation. A reconciled journal sequence always takes
	// precedence. Only while the journal has no reconciled ticket-consuming
	// operation at all may an active validated pilot activation baseline
	// explain the ticket: the approved operator cleanup finished consuming it to reach the archived flat baseline, so an active baseline explains
	// exactly its own archived sequence — compared exactly, so a reset back to
	// zero after a consumed baseline holds like any other out-of-band move.
	// Without an active baseline, any nonzero consumed sequence holds. The
	// baseline is bookkeeping evidence, not a journal row, and carries no NAV.
	if !s.JournalSequenceKnown {
		switch {
		case s.PilotActive && s.PilotBaselineKnown:
			if s.TicketLastConsumedSequenceRaw != s.PilotBaselineTicketSequenceRaw {
				return monitorHold(s, "out_of_band_crank"), true
			}
		case s.TicketLastConsumedSequenceRaw != 0:
			return monitorHold(s, "out_of_band_crank"), true
		}
	} else if uint64(s.JournalReconciledSequenceRaw) != uint64(s.TicketLastConsumedSequenceRaw) {
		return monitorHold(s, "out_of_band_crank"), true
	}
	// M6: the pinned programs must still be the reviewed binaries. The watcher
	// fetches and hashes the full ProgramData at start and whenever the deploy
	// slot moves, so an absent, incoherent, or mismatched image stays
	// unverified, which is itself a durable hold.
	if !s.ProgramIdentityKnown {
		return monitorHold(s, "program_identity_unverified"), true
	}
	if s.VoltrProgramDeploySlot != voltrProgramDeploySlot || s.AdaptorProgramDeploySlot != adaptorProgramDeploySlot {
		return monitorHold(s, "program_identity_changed"), true
	}
	return Decision{}, false
}

// withdrawalIdleUnderfunded is the M4 gate. When Voltr idle cannot cover the
// pending withdrawal request quotes, only the unwind and restore legs are
// admissible: a refresh-only or allocation tick would report a book that
// cannot pay the queue. This blocks a decision instead of holding the route,
// because the admissible legs remain safe to execute.
func withdrawalIdleUnderfunded(s Snapshot) bool {
	return s.MonitorsArmed && s.WithdrawalDemandRaw > s.VoltrIdleRaw
}

// unexplainedNAVDriftHold is S1. When the independently observed NAV has moved
// beyond the drift bound from the last reported NAV and no reconciled capital
// mutation explains the move, the book is unaccounted: the route stops instead
// of scheduling a refresh that would bless the drift. CapitalMutated is that
// explanation and is read from the reconciled journal (mutations newer than the
// last report), never from the NAV comparison, so the drift cannot explain
// itself.
func unexplainedNAVDriftHold(s Snapshot) (Decision, bool) {
	if !s.MonitorsArmed || s.PostMutationNAVRequired || s.CapitalMutated {
		return Decision{}, false
	}
	if navDriftOutsideTolerance(uint64(s.StrategyNAVRaw), uint64(s.PriorReportedNAVRaw)) {
		return monitorHold(s, "nav_drift_unexplained"), true
	}
	return Decision{}, false
}

// capitalMutationReports is S2. A reconciled capital mutation schedules a
// report only when the valuation moved beyond the same drift bound, so a
// sub-tolerance rounding move cannot churn a report on every tick.
func capitalMutationReports(s Snapshot) bool {
	return s.CapitalMutated && navDriftOutsideTolerance(uint64(s.StrategyNAVRaw), uint64(s.PriorReportedNAVRaw))
}

// navDriftOutsideTolerance compares the independently observed NAV with the
// last reported NAV using max(bps of reported, floor) raw units.
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

// feeAccumulatorOutsideBound bounds the un-harvested LP fee by a share of the
// LP supply Voltr actually prices against. big.Int keeps a hostile accumulator
// from wrapping the comparison.
func feeAccumulatorOutsideBound(fees, supply uint64) bool {
	if supply == 0 {
		return fees > 0
	}
	bound := new(big.Int).Mul(new(big.Int).SetUint64(supply), big.NewInt(feeAccumulatorMaxBPS))
	fee := new(big.Int).Mul(new(big.Int).SetUint64(fees), big.NewInt(10_000))
	return fee.Cmp(bound) > 0
}

func monitorHold(s Snapshot, reason string) Decision {
	strategyKey := s.RouteLane
	if strategyKey == "" {
		strategyKey = RouteID
	}
	return Decision{
		Action: HoldManualRecovery, Reason: reason, AmountRaw: 0, StrategyKey: strategyKey,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d", s.ObservationID, reason, s.Slot),
	}
}
