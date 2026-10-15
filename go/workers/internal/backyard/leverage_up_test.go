package backyard

import (
	"encoding/binary"
	"testing"
)

// Six-decimal collateral and debt at $1 each: value raw == amount raw.
func leverageTestPosition(collateral, debt uint64) KaminoPosition {
	p := KaminoPosition{CollateralDepositedRaw: collateral, RedeemablePrimeRaw: collateral, DebtRaw: debt, CollateralDecimals: 6, DebtDecimals: 6}
	binary.LittleEndian.PutUint64(p.CollateralPriceSF[:8], uint64(1)<<60)
	binary.LittleEndian.PutUint64(p.DebtPriceSF[:8], uint64(1)<<60)
	return p
}

func TestLeverageUpDecisionsAtEachLevel(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC} {
		at := func(level, target float64) Snapshot {
			s := leverageSnapshot(level)
			s.RouteLane, s.StrategyKey, s.LeverageTargetLevel = lane, lane, target
			s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 1, 1, 1
			if lane == onreONycUSDC {
				s.DebtIdleRaw = 0
			}
			return s
		}
		for _, tc := range []struct {
			level, target float64
			action        Action
			reason        string
			amount        int64
		}{
			{1, 1.5, OpenRouteStep, leverageUpReason, 500_000_000},
			{1, 1.75, OpenRouteStep, leverageUpReason, 500_000_000}, // one level per move
			{1, 1, Hold, holdAtTarget, 0},
			{1.5, 1.75, OpenRouteStep, leverageUpReason, 250_000_000},
			{1.5, 1.5, Hold, holdAtTarget, 0},
			{1.75, 1.75, Hold, holdAtTarget, 0},
		} {
			s := at(tc.level, tc.target)
			got := Decide(targeted(s))
			if got.Action != tc.action || got.Reason != tc.reason || got.AmountRaw != tc.amount {
				t.Fatalf("%s %.2fx target %.2fx: %+v", lane, tc.level, tc.target, got)
			}
			// Blocked borrowing never starts an up move.
			s.BorrowUtilizationBlocked = true
			if got := Decide(targeted(s)); got.Action == OpenRouteStep {
				t.Fatalf("%s %.2fx: blocked borrowing still borrowed: %+v", lane, tc.level, got)
			}
		}
		// Withdrawal, an operator exit and hard LTV keep their priority.
		for name, c := range map[string]struct {
			mutate func(*Snapshot)
			in     targetInputs
		}{
			"withdrawal": {func(s *Snapshot) { s.WithdrawalDemandRaw = 1 }, targetInputs{}},
			"exit":       {func(*Snapshot) {}, targetInputs{Exits: map[string]laneExit{lane: {EquityZero: true}}}},
			"hard ltv":   {func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS = 1, 1, 6_000 }, targetInputs{}},
		} {
			s := at(1, 1.5)
			c.mutate(&s)
			if got := Decide(targetedWith(s, c.in)); got.Reason == leverageUpReason {
				t.Fatalf("%s: %s did not preempt leverage_up", lane, name)
			}
		}
	}
	exitOnly := leverageSnapshot(1.5)
	exitOnly.RouteLane, exitOnly.StrategyKey, exitOnly.LeverageTargetLevel = ethenaUSDePYUSD.Lane, ethenaUSDePYUSD.Lane, 1.75
	if got := Decide(targeted(exitOnly)); got.Reason == leverageUpReason {
		t.Fatalf("exit-only Ethena levered up: %+v", got)
	}
}
