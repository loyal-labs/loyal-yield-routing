package backyard

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestCapacityReviewNonParBenchmarkDebtUnits(t *testing.T) {
	t.Parallel()
	m := LaneEconomics{Lane: autoAUTOPYUSD.Lane, NativeAPY: .12, BorrowCurve: []BorrowCurvePoint{{0, 0}, {10000, 1000}}, DebtSupplyRaw: 1_000_000_000, DebtBorrowRaw: 100_000_000}
	s := Snapshot{RouteLane: autoAUTOPYUSD.Lane, PositionDebtRaw: 50_000_000, BorrowCapacityKnown: true, BorrowDebtDecimals: 6}
	binary.LittleEndian.PutUint64(s.BorrowDebtPriceSF[:8], (uint64(1)<<60)/5*4)
	binary.LittleEndian.PutUint64(s.BorrowUSDCPriceSF[:8], uint64(1)<<60)
	got, ok := leverageSpread(m, 1.5, 100_000_000, true, s)
	apr, err := projectedBorrowAPR(m, 12_500_001)
	want := m.NativeAPY - math.Expm1(apr)
	if err != nil || !ok || math.Abs(got-want) > 1e-10 {
		t.Fatalf("mixed benchmark debt units: got %v want %v known=%v", got, want, ok)
	}
}

func TestCapacityReviewBenchmarkMissingPriceAndUSDC(t *testing.T) {
	t.Parallel()
	m := LaneEconomics{Lane: autoAUTOPYUSD.Lane, NativeAPY: .12, BorrowCurve: []BorrowCurvePoint{{0, 0}, {10000, 1000}}, DebtSupplyRaw: 1_000_000_000, DebtBorrowRaw: 100_000_000}
	if _, ok := leverageSpread(m, 1.5, 100_000_000, true); ok {
		t.Fatal("non-par benchmark silently assumed parity")
	}
	var watch leverageWatch
	watch.observe([]LaneEconomics{m}, m.Lane, 100_000_000, true)
	if len(watch.summary) != 1 || watch.summary[0].SpreadBPS != nil {
		t.Fatal("missing price persisted a numeric hypothetical spread")
	}
	if _, present := watch.summary[0].APYBPS["1.50"]; present {
		t.Fatal("missing price persisted leveraged benchmark")
	}
	if _, present := watch.summary[0].APYBPS["1.00"]; !present {
		t.Fatal("missing price removed unaffected 1x benchmark")
	}
	if _, ok := leverageLevelAPY(m, 1, 100_000_000, false); !ok {
		t.Fatal("debt-free benchmark needs no conversion")
	}
	s := Snapshot{RouteLane: autoAUTOPYUSD.Lane, HasPosition: true, PositionCollateralValueRaw: 140_000_000, PositionDebtValueRaw: 40_000_000, PositionDebtRaw: 50_000_000}
	m.CurrentBorrowAPY = .05
	if _, ok := currentPositionAPY(s, []LaneEconomics{m}); !ok {
		t.Fatal("missing new-entry price blocked held APY")
	}
	m.Lane = onreONycUSDC
	s.RouteLane = m.Lane
	got, ok := leverageSpread(m, 1.5, 100_000_000, true, s)
	apr, _ := projectedBorrowAPR(m, 0)
	if !ok || math.Abs(got-(m.NativeAPY-math.Expm1(apr))) > 1e-12 {
		t.Fatal("USDC benchmark parity changed", got)
	}
}
