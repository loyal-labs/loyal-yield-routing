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
			{1, 1.5, OpenRouteStep, leverageUpReason, 499_500_000},
			{1, 1.75, OpenRouteStep, leverageUpReason, 499_500_000}, // one level per move
			{1, 1, Hold, holdAtTarget, 0},
			{1.5, 1.75, OpenRouteStep, leverageUpReason, 249_250_000},
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

// Sizing lands each step on its level; receive+fee never takes the instant
// LTV above 50%; the loop result stays at or below 45%.
func TestLeverageUpSizingAndCaps(t *testing.T) {
	t.Parallel()
	noFee := func(uint64) (uint64, error) { return 0, nil }
	// 1x -> 1.5x on $1,000: borrow $500 (debt/equity 0.5).
	p := leverageTestPosition(1_000_000_000, 0)
	if got, err := p.leverageUpBorrowRaw(150); err != nil || got != 500_000_000 {
		t.Fatalf("1x->1.5x %d %v", got, err)
	}
	// 1.5x -> 1.75x: collateral $1,500, debt $500, equity $1,000: borrow 25%
	// of equity = $250; C'=1,750, D'=750, LTV 42.9%.
	p = leverageTestPosition(1_500_000_000, 500_000_000)
	got, err := p.leverageUpBorrowRaw(175)
	if err != nil || got != 250_000_000 {
		t.Fatalf("1.5x->1.75x %d %v", got, err)
	}
	// The 0.1% margin under 50% trims it to $249.25 (lands ~1.748x).
	got, err = leverageUpCapFee(p, got, noFee)
	if err != nil || got != 249_250_000 {
		t.Fatalf("1.5x->1.75x capped %d %v", got, err)
	}
	instant := leverageTestPosition(1_500_000_000, 500_000_000+got)
	if ltv, _ := observedLTVBPS(instant); ltv > TargetLTVBPS {
		t.Fatalf("instant LTV %d above 50%%", ltv)
	}
	after := leverageTestPosition(1_750_000_000, 750_000_000)
	if ltv, _ := observedLTVBPS(after); ltv > leverageMaxLTVBPS {
		t.Fatalf("post-move LTV %d above 45%%", ltv)
	}
	// Already at the level: refused.
	if _, err := leverageTestPosition(1_750_000_000, 750_000_000).leverageUpBorrowRaw(175); err == nil {
		t.Fatal("borrowed at the target level")
	}
	// The 1x->1.5x borrow sits at 50%: the fee shrinks receive so receive+fee
	// stays inside 50% (with the 0.1% margin).
	p = leverageTestPosition(1_000_000_000, 0)
	receive, _ := p.leverageUpBorrowRaw(150)
	capped, err := leverageUpCapFee(p, receive, func(r uint64) (uint64, error) { return r / 1000, nil })
	if err != nil || capped+capped/1000 > 500_000_000-500_000 {
		t.Fatalf("fee cap %d %v", capped, err)
	}
	// Tiny positions are refused, not dust-borrowed.
	if _, err := leverageUpCapFee(leverageTestPosition(10_000_000, 0), 5_000_000, noFee); err == nil {
		t.Fatal("dust borrow admitted")
	}
	for _, bad := range []int64{1, 100, 200} {
		if _, err := p.leverageUpBorrowRaw(bad); err == nil {
			t.Fatalf("level %d accepted", bad)
		}
	}
}
