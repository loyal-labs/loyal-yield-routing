package backyard

import (
	"encoding/binary"
	"math"
	"math/big"
	"testing"
)

func pairCapacityFixture(t *testing.T) (RuntimeRoute, []ConfirmedAccount, KaminoPosition) {
	t.Helper()
	route, _ := runtimeRoute(SelectedRouteID)
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	c := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 77, one, 100, 100)
	d := reserveFixture(t, route.Kamino.DebtReserve, bridgeUSDC, 77, one, 1000, 1000)
	for _, a := range []*ConfirmedAccount{&c, &d} {
		putKey(t, a.Data[32:64], route.Kamino.Market)
		binary.LittleEndian.PutUint64(a.Data[kaminoReserveConfigOffset+160:], 10_000)
		binary.LittleEndian.PutUint64(a.Data[kaminoReserveConfigOffset+168:], 10_000)
		binary.LittleEndian.PutUint64(a.Data[kaminoOutsideBorrowLimitOffset:], 10_000)
		binary.LittleEndian.PutUint64(a.Data[kaminoBorrowFactorOffset:], 100)
		a.Data[kaminoLoanToValueOffset] = 80
	}
	o := obligationFixture(t, 77, 0, 0)
	o.Address = route.Kamino.Obligation
	putKey(t, o.Data[32:64], route.Kamino.Market)
	return route, []ConfirmedAccount{c, d, o, clockFixture()}, KaminoPosition{EntryCapacityRaw: 6600}
}

// A B2 lane's entry equity is its collateral deposit room, independent of
// debt room; unknown borrowing evidence fails closed and a full deposit limit
// closes entry. (The deleted one-pass pair arithmetic's protocol caps now bound
// the borrow: TestCapacitySizedDebtRoomBoundaries.)
func TestPairCapacityIsDepositRoomAndFailsClosedOnUnknownEvidence(t *testing.T) {
	t.Parallel()
	route, accounts, _ := pairCapacityFixture(t)
	position := leverageTestPosition(0, 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, budgetClockAddress).Data[:8], 77)
	if got, err := kaminoPairEntryCapacity(position, nil, route); err == nil || got != 0 {
		t.Fatal("unknown borrowing evidence became capacity", got, err)
	}
	collateral, err := decodeKaminoReserve(accounts[0], route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	deposited, err := ceilScaledBigFraction(collateral.totalLiquiditySF)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accounts[0].Data[kaminoReserveConfigOffset+160:], deposited+10_000)
	got, err := kaminoPairEntryCapacity(position, accounts, route)
	if err != nil || got != 9_900 {
		t.Fatal("deposit room is not the entry ceiling", got, err)
	}
	binary.LittleEndian.PutUint64(accounts[1].Data[kaminoOutsideBorrowLimitOffset:], 0)
	if closedDebt, err := kaminoPairEntryCapacity(position, accounts, route); err != nil || closedDebt != got {
		t.Fatal("closed debt room changed entry equity", closedDebt, err)
	}
	binary.LittleEndian.PutUint64(accounts[0].Data[kaminoReserveConfigOffset+160:], deposited)
	if full, err := kaminoPairEntryCapacity(position, accounts, route); err != nil || full != 0 {
		t.Fatal("full deposit limit still advertised capacity", full, err)
	}
}

func TestNetBorrowHeadroom(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name              string
		capacity, current int64
		start, interval   uint64
		now               int64
		want              uint64
		fail              bool
	}{
		{"disabled", 0, 0, 0, 0, 100, math.MaxUint64, false},
		{"open", 100, 40, 10, 100, 100, 60, false},
		{"closed", 100, 100, 10, 100, 100, 0, false},
		{"over_limit", 100, 110, 10, 100, 100, 0, false},
		{"net_repayments", 100, -20, 10, 100, 100, 120, false},
		{"boundary", 100, 40, 10, 100, 110, 100, false},
		{"reset", 100, 40, 10, 100, 111, 100, false},
		{"future", 100, 40, 101, 100, 100, 0, true},
		{"negative_cap", -1, 40, 10, 100, 100, 0, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			data := make([]byte, 32)
			for i, n := range []uint64{uint64(row.capacity), uint64(row.current), row.start, row.interval} {
				binary.LittleEndian.PutUint64(data[i*8:], n)
			}
			got, err := kaminoNetBorrowHeadroom(data, row.now)
			if got != row.want || (err != nil) != row.fail {
				t.Fatalf("headroom=%d want=%d err=%v", got, row.want, err)
			}
		})
	}
}
