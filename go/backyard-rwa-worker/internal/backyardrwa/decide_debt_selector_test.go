package backyardrwa

import "testing"

// The AUTO lane plans through decideNonUSDC: bridge sizing stays in admitted
// entry-equity USDC units, debt legs stay in raw PYUSD units, and the selector
// authority gates only new allocation — never safety legs or tranche
// completion. These snapshots reproduce the live 370333-idle/250000-admitted
// allocation failure and the lifecycle holds around it.
func TestAUTOSelectorNonUSDCLifecycle(t *testing.T) {
	const lane = "AUTO/AUTO/PYUSD"
	ready := func(s Snapshot) Snapshot {
		s.RouteLane = lane
		s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 500_000, 500_000, 500_000
		return s
	}
	check := func(t *testing.T, s Snapshot, action Action, amount int64, reason string) {
		t.Helper()
		got := Decide(s)
		if got.Action != action || got.AmountRaw != amount || got.Reason != reason || got.Validate() != nil {
			t.Fatalf("wanted %s %d %q, got %+v", action, amount, reason, got)
		}
	}

	t.Run("pilot allocation spends the admitted equity, not full idle", func(t *testing.T) {
		s := ready(base())
		s.PilotActive = true
		s.VoltrIdleRaw = 370_333
		s.SelectorEntryEquityRaw = 250_000
		check(t, s, VoltrAllocateToSquads, 250_000, "eligible_voltr_idle")

		// Without an admission the same idle is unallocatable.
		s.SelectorEntryEquityRaw = 0
		check(t, s, Hold, 0, "selector_entry_amount_requires_fresh_quote")

		// An admission above the capped, reviewed capacity is stale.
		s.SelectorEntryEquityRaw = 550_000
		check(t, s, Hold, 0, "selector_entry_amount_requires_fresh_quote")

		// Idle below the admission is never topped up partially.
		s.SelectorEntryEquityRaw = 250_000
		s.VoltrIdleRaw = 100_000
		check(t, s, Hold, 0, "selector_entry_amount_requires_fresh_quote")
	})

	t.Run("non-pilot allocation keeps full idle sizing", func(t *testing.T) {
		s := ready(base())
		s.VoltrIdleRaw = 370_333
		check(t, s, VoltrAllocateToSquads, 370_333, "eligible_voltr_idle")
	})

	t.Run("paused entry authority holds before any allocation", func(t *testing.T) {
		s := ready(base())
		s.PilotActive = true
		s.VoltrIdleRaw = 370_333
		s.SelectorEntryEquityRaw = 250_000
		s.SelectorEntryPaused = true
		check(t, s, Hold, 0, "selector_entry_requires_fresh_admission")
	})

	t.Run("already funded tranche completes without reallocation", func(t *testing.T) {
		s := ready(base())
		s.PilotActive = true
		s.SelectorEntryEquityRaw = 250_000
		s.SquadsIdleRaw = 250_000
		check(t, s, SwapStableToCollateralStep, 250_000, "usdc_requires_collateral")
	})

	t.Run("capacity loss returns flat bridge cash to Voltr in full", func(t *testing.T) {
		s := base()
		s.RouteLane = lane
		s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 100, 500_000, 500_000
		s.SquadsIdleRaw = 250_000
		check(t, s, StageSquadsToVoltr, 250_000, "entry_capacity_changed_return_cash")

		// Every reviewed bound gone, not just reduced below the cash.
		s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 0, 0, 0
		check(t, s, StageSquadsToVoltr, 250_000, "entry_capacity_changed_return_cash")
	})

	t.Run("capacity loss never bypasses the unattributed debt hold", func(t *testing.T) {
		s := ready(base())
		s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 100, 100, 100
		s.SquadsIdleRaw = 250_000
		check(t, s, StageSquadsToVoltr, 250_000, "entry_capacity_changed_return_cash")

		// Idle PYUSD custody is distinct: it keeps its manual-recovery hold and
		// no bridge allocation or staging may run alongside it.
		s.DebtIdleRaw = 5
		check(t, s, HoldManualRecovery, 0, "unattributed_idle_debt_before_entry")
	})

	t.Run("no-withdrawal unwind drains in full and completes", func(t *testing.T) {
		s := ready(base())
		s.Unwind = true // no WithdrawalDemandRaw and no cutover drain
		s.SquadsIdleRaw = 76
		check(t, s, StageSquadsToVoltr, 76, "withdrawal_terminal_residue")

		// Staged custody still restores ahead of the terminal report.
		s.SquadsIdleRaw, s.VoltrStrategyIdleRaw = 0, 76
		s.StagedAmountKnown, s.StagedAmountRaw = true, 76
		s.CapitalMutated = true
		check(t, s, VoltrRestoreIdle, 76, "withdrawal_staged")

		s.VoltrStrategyIdleRaw = 0
		check(t, s, ReportNAV, 0, "withdrawal_terminal_nav_due")

		// Flat and reported, the unwind ends instead of reallocating.
		s.CapitalMutated, s.PriorReportedNAVRaw = false, 0
		check(t, s, Hold, 0, "unwind_complete")

		// Even an admitted pilot equity cannot allocate during an unwind.
		s.VoltrIdleRaw, s.PilotActive, s.SelectorEntryEquityRaw = 370_333, true, 250_000
		check(t, s, Hold, 0, "unwind_complete")
	})

	t.Run("staged custody restores before the nav report", func(t *testing.T) {
		s := ready(base())
		s.VoltrStrategyIdleRaw = 76
		s.StagedAmountKnown, s.StagedAmountRaw = true, 76
		s.PostMutationNAVRequired = true
		check(t, s, VoltrRestoreIdle, 76, "withdrawal_staged")
	})

	t.Run("unwind refresh waits behind hard ltv but ahead of ordinary legs", func(t *testing.T) {
		s := ready(base())
		s.Unwind, s.UnwindRefreshRequired = true, true
		s.HasPosition = true
		s.PositionCollateralRaw, s.PositionDebtRaw = 100_000_000, 40
		s.PositionCollateralValueRaw, s.PositionDebtValueRaw = 100_000, 2_000
		s.LTVBPS = 6000 // hard := min(8000-1500, 6000)
		s.DebtIdleRaw = 5
		check(t, s, DeleverRouteStep, 5, "hard_ltv_repay")

		// Below hard LTV the stale-unwind hold precedes every ordinary leg.
		s.LTVBPS = 4000
		check(t, s, Hold, 0, "unwind_requires_fresh_admission")

		s.DebtIdleRaw = 0
		s.HasPosition = false
		s.PositionDebtRaw = 0
		s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.PositionDebtValueRaw = 0, 0, 0
		s.VoltrStrategyIdleRaw = 76
		s.StagedAmountKnown, s.StagedAmountRaw = true, 76
		check(t, s, Hold, 0, "unwind_requires_fresh_admission")

		s.VoltrStrategyIdleRaw, s.PostMutationNAVRequired = 0, true
		check(t, s, Hold, 0, "unwind_requires_fresh_admission")
	})
}
