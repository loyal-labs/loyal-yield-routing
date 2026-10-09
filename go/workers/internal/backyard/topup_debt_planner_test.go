package backyard

import "testing"

func TestDebtTopupPlannerUsesOnlyReceiptedInventory(t *testing.T) {
	s := liveTopupSnapshot()
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS = 1000, 1000, 3300
	s.LeverageTargetLevel = 1.5
	check := func(action Action, reason string, amount int64) {
		t.Helper()
		got := Decide(s)
		if got.Action != action || got.Reason != reason || got.AmountRaw != amount {
			t.Fatalf("want %s/%s/%d, got %+v", action, reason, amount, got)
		}
	}
	check(VoltrAllocateToSquads, topupAllocationReason, s.VoltrIdleRaw)
	loan, _ := topupLoanFixture(t)
	origin := sha256Bytes([]byte("planner allocation"))
	tranche := topupTranche{Generation: 1, Loan: loan, Lane: autoAUTOPYUSD.Lane, OriginOperationID: origin, LastOperationID: origin, LastSlot: s.Slot, LastEffectsSHA256: sha256Bytes([]byte("receipt")), AllocatedUSDCRaw: uint64(s.VoltrIdleRaw), USDCRemainingRaw: uint64(s.VoltrIdleRaw), CollateralRemainingRaw: 1, DepositQuantumRaw: 3, Stage: topupTrancheAllocated}
	s.TopupTranche = &tranche
	s.SquadsIdleRaw, s.VoltrIdleRaw = s.VoltrIdleRaw, 0
	s.CollateralIdleRaw, s.PrimeIdleRaw = 1, 1
	check(SwapStableToCollateralStep, topupSwapReason, s.SquadsIdleRaw)
	s.SquadsIdleRaw++
	if got := Decide(s); got.Action != Hold {
		t.Fatal("unowned cash selected", got)
	}
	s.SquadsIdleRaw = 0
	tranche.Stage, tranche.LastOperationID, tranche.USDCRemainingRaw, tranche.CollateralRemainingRaw = topupTrancheCollateral, sha256Bytes([]byte("swap")), 0, 101
	s.CollateralIdleRaw, s.PrimeIdleRaw, s.MinimumCollateralDepositRaw = 101, 101, 3
	check(OpenRouteStep, topupDepositReason, 101)
	tranche.Stage, tranche.LastOperationID, tranche.CollateralRemainingRaw = topupTrancheComplete, sha256Bytes([]byte("deposit")), 1
	s.CollateralIdleRaw, s.PrimeIdleRaw, s.VoltrIdleRaw = 1, 1, topupMinimumRaw
	check(VoltrAllocateToSquads, topupAllocationReason, topupMinimumRaw)
	s.TopupTranche = nil
	if got := Decide(s); got.Reason == topupAllocationReason {
		t.Fatal("unexplained carry adopted", got)
	}
}
