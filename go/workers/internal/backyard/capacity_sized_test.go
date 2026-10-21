package backyard

import (
	"encoding/binary"
	"math"
	"math/big"
	"testing"
)

func TestCapacitySizedEntryRetainsCollateralRoom(t *testing.T) {
	t.Parallel()
	route, accounts, position := pairCapacityFixture(t)
	route.Lane = onreONycUSDC
	binary.LittleEndian.PutUint64(accountAt(accounts, budgetClockAddress).Data[:8], 77)
	// $100k eligible collateral, $10k new principal room. Fixture prices are par.
	position = leverageTestPosition(0, 0)
	position.EntryCapacityRaw = 20_000_000_000
	for _, a := range accounts[:2] {
		binary.LittleEndian.PutUint64(a.Data[kaminoReserveConfigOffset+160:], 101_010_102_000)
		binary.LittleEndian.PutUint64(a.Data[kaminoReserveConfigOffset+168:], 200_000_000_000)
		binary.LittleEndian.PutUint64(a.Data[kaminoOutsideBorrowLimitOffset:], 200_000_000_000)
	}
	binary.LittleEndian.PutUint64(accounts[1].Data[224:], 10_000_000_000)
	got, err := kaminoPairEntryCapacityDebtRaw(position, accounts, route)
	if err != nil || got < 100_000_000_000 {
		t.Fatalf("equity clipped by borrowing: %d %v", got, err)
	}
	binary.LittleEndian.PutUint64(accounts[1].Data[224:], 0)
	zero, err := kaminoPairEntryCapacityDebtRaw(position, accounts, route)
	if err != nil || zero != got {
		t.Fatalf("zero debt room changed equity: %d %v", zero, err)
	}
}

func TestCapacitySizedActualCarry(t *testing.T) {
	t.Parallel()
	s := leverageSnapshot(1.5)
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw = 136_000_000, 36_000_000
	s.PositionDebtRaw, s.LTVBPS = 36_000_000, 2647
	m := LaneEconomics{Lane: s.RouteLane, NativeAPY: .10, SupplyAPY: .01, CurrentBorrowAPY: .05,
		BorrowCurve: []BorrowCurvePoint{{0, 500}, {10_000, 500}}, DebtSupplyRaw: 1e12, DebtBorrowRaw: 1e11}
	got, ok := currentPositionAPY(s, []LaneEconomics{m})
	want := int64(math.Round(performanceFeeForecast(1.36*.11-.36*.05, rwa.AdminPerformanceFeeBPS) * 10_000))
	if !ok || got.APYBPS != want || got.Level != 1.36 {
		t.Fatalf("carry=%+v want=%d", got, want)
	}
}

// capacityBorrowGateFixture is a borrow request whose raw reserves are
// refreshed at slot 42, the clock's slot, with room, fee receiver and prices
// that pass the final borrow gate.
func capacityBorrowGateFixture(t *testing.T, sourceDebt uint64) ([]ConfirmedAccount, RuntimeRoute, KaminoLegRequest, ExpectedEffects) {
	t.Helper()
	_, m, _, _, accounts, route := leverage175Fixture(t)
	putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(sourceDebt), 60))
	debt := accountAt(accounts, route.Kamino.DebtReserve).Data
	collateral := accountAt(accounts, route.Kamino.CollateralReserve).Data
	binary.LittleEndian.PutUint64(debt[kaminoOutsideBorrowLimitOffset:], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(debt[kaminoReserveConfigOffset+168:], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(collateral[kaminoReserveConfigOffset+160:], 10_000_000_000_000)
	binary.LittleEndian.PutUint64(debt[224:], 1_000_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtLiquiditySupply).Data[64:], 1_000_000_000)
	binary.LittleEndian.PutUint64(debt[kaminoReserveConfigOffset+40:], 0)
	binary.LittleEndian.PutUint64(debt[kaminoBorrowFactorOffset:], 100)
	putKey(t, debt[192:224], route.DebtFeeReceiver)
	fee := accountAt(accounts, route.DebtLiquiditySupply)
	fee.Address, fee.Data = route.DebtFeeReceiver, append([]byte(nil), fee.Data...)
	binary.LittleEndian.PutUint64(fee.Data[64:], 0)
	accounts = append(accounts, fee)
	clock := clockFixture()
	binary.LittleEndian.PutUint64(clock.Data[:8], 42)
	binary.LittleEndian.PutUint64(clock.Data[32:], 1000)
	accounts = append(accounts, clock)
	for _, address := range []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve} {
		binary.LittleEndian.PutUint64(accountAt(accounts, address).Data[kaminoMarketPriceLastUpdatedTSOffset:], 1000)
	}
	r, err := m.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegBorrow, 10_000_000, LatestBlockhash{Blockhash: backyardSettings, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	effects, err := kaminoBorrowEffects(accounts, route, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	return accounts, route, r, effects
}

func TestCapacitySizedDebtRoomBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func([]ConfirmedAccount)
		want   uint64
		fail   bool
	}{
		{"available", func([]ConfirmedAccount) {}, 1000, false},
		{"zero_available", func(a []ConfirmedAccount) { binary.LittleEndian.PutUint64(a[1].Data[224:], 0) }, 0, false},
		{"queued", func(a []ConfirmedAccount) { binary.LittleEndian.PutUint64(a[1].Data[kaminoQueuedCollateralOffset:], 1) }, 0, false},
		{"outside_limit", func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(a[1].Data[kaminoOutsideBorrowLimitOffset:], 100)
		}, 100, false},
		{"elevation", func(a []ConfirmedAccount) { a[2].Data[kaminoObligationElevationGroupOffset] = 1 }, 0, false},
		// Moved from the deleted one-pass pair-capacity table: the same
		// protocol caps now bound the B2 borrow.
		{"outside_limit_closed", func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(a[1].Data[kaminoOutsideBorrowLimitOffset:], 0)
		}, 0, false},
		{"borrow_limit", func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(a[1].Data[kaminoReserveConfigOffset+168:], 50)
		}, 50, false},
		// One fixed-point ulp below the 10% boundary: strict.
		{"utilization_boundary", func(a []ConfirmedAccount) { a[1].Data[kaminoReserveConfigOffset+645] = 10 }, 99, false},
		{"cross_collateral_disabled", func(a []ConfirmedAccount) { a[0].Data[kaminoDisableCrossCollateralOffset] = 1 }, 0, false},
		{"referrer", func(a []ConfirmedAccount) { a[2].Data[2288] = 1 }, 0, false},
		{"weighted_debt", func(a []ConfirmedAccount) { binary.LittleEndian.PutUint64(a[1].Data[kaminoBorrowFactorOffset:], 200) }, 0, false},
		{"unknown_clock", func(a []ConfirmedAccount) { a[3].Data = nil }, 0, true},
		{"stale_oracle", func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(a[1].Data[kaminoMarketPriceLastUpdatedTSOffset:], 1)
		}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route, a, _ := pairCapacityFixture(t)
			binary.LittleEndian.PutUint64(a[3].Data[:8], 77)
			tc.mutate(a)
			got, err := kaminoAdditionalDebtRoom(a, route)
			if got != tc.want || (err != nil) != tc.fail {
				t.Fatalf("room=%d err=%v", got, err)
			}
		})
	}
	for _, room := range []uint64{0, 1, 2, 10_000_000, 1<<63 - 1} {
		rate := (uint64(1) << 60) / 100
		receive := kaminoReceiveWithinRoom(room, rate)
		if receive == 0 {
			continue
		}
		fee, err := kaminoBorrowFeeAtRate(rate, receive)
		if err != nil || receive > room || fee > room-receive {
			t.Fatalf("fee-inclusive room overflow: %d %d %v", room, receive, err)
		}
	}
}
