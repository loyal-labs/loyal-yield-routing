package backyardrwa

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

func TestPairCapacityConstrainsWholeLoopAllocation(t *testing.T) {
	for _, row := range []struct {
		name   string
		mutate func(c, d, o []byte)
		want   uint64
		fail   bool
	}{
		{name: "actual_available_liquidity", want: 2000},
		{name: "queued_liquidity_requires_new_admission", mutate: func(c, d, o []byte) {
			binary.LittleEndian.PutUint64(d[kaminoQueuedCollateralOffset:], 1)
		}},
		{name: "minimum_fee_exceeds_tiny_tranche_ltv", mutate: func(c, d, o []byte) {
			c[kaminoLoanToValueOffset] = 60
			binary.LittleEndian.PutUint64(c[kaminoReserveConfigOffset+160:], 106)
			binary.LittleEndian.PutUint64(d[kaminoReserveConfigOffset+40:], 1)
		}},
		{name: "outside_group_headroom", mutate: func(c, d, o []byte) {
			binary.LittleEndian.PutUint64(d[kaminoOutsideBorrowLimitOffset:], 900)
			binary.LittleEndian.PutUint64(d[kaminoOutsideBorrowCounterOffset:], 800)
		}, want: 200},
		{name: "outside_group_closed", mutate: func(c, d, o []byte) {
			binary.LittleEndian.PutUint64(d[kaminoOutsideBorrowLimitOffset:], 0)
		}},
		{name: "deposit_and_redeposit_share_cap", mutate: func(c, d, o []byte) {
			binary.LittleEndian.PutUint64(c[kaminoReserveConfigOffset+160:], 250)
		}, want: 100},
		{name: "global_borrow_headroom", mutate: func(c, d, o []byte) {
			binary.LittleEndian.PutUint64(d[kaminoReserveConfigOffset+168:], 50)
		}, want: 100},
		{name: "utilization_boundary_is_strict", mutate: func(c, d, o []byte) {
			d[kaminoReserveConfigOffset+645] = 10
		}, want: 198},
		{name: "cross_collateral_disabled", mutate: func(c, d, o []byte) {
			c[kaminoDisableCrossCollateralOffset] = 1
		}},
		{name: "borrow_factor_interim_ltv", mutate: func(c, d, o []byte) {
			binary.LittleEndian.PutUint64(d[kaminoBorrowFactorOffset:], 200)
		}},
		{name: "fee_interim_ltv", mutate: func(c, d, o []byte) {
			c[kaminoLoanToValueOffset] = 50
			binary.LittleEndian.PutUint64(d[kaminoReserveConfigOffset+40:], 1)
		}},
		{name: "fee_rounding_headroom", mutate: func(c, d, o []byte) {
			binary.LittleEndian.PutUint64(d[kaminoReserveConfigOffset+40:], (uint64(1)<<60)/100)
		}, want: 1978},
		{name: "group_requires_separate_admission", mutate: func(c, d, o []byte) {
			o[kaminoObligationElevationGroupOffset] = 1
		}},
		{name: "referrer_requires_separate_admission", mutate: func(c, d, o []byte) {
			o[2288] = 1
		}},
		{name: "factor_is_clamped_by_protocol", mutate: func(c, d, o []byte) {
			binary.LittleEndian.PutUint64(d[kaminoBorrowFactorOffset:], 99)
		}, want: 2000},
	} {
		t.Run(row.name, func(t *testing.T) {
			route, accounts, position := pairCapacityFixture(t)
			if row.mutate != nil {
				row.mutate(accounts[0].Data, accounts[1].Data, accounts[2].Data)
			}
			got, err := kaminoPairEntryCapacity(position, accounts, route)
			if (err != nil) != row.fail || got != row.want {
				t.Fatalf("capacity=%d want=%d err=%v", got, row.want, err)
			}
		})
	}
}

func TestPairCapacityPreservesUnknownAndClosedBoundaries(t *testing.T) {
	route, accounts, position := pairCapacityFixture(t)
	position.EntryCapacityRaw = 0
	if got, err := kaminoPairEntryCapacity(position, nil, route); err != nil || got != 0 {
		t.Fatal("cash fallback gained capacity", got, err)
	}
	position.EntryCapacityRaw = 6600
	accounts[2] = ConfirmedAccount{Address: route.Kamino.Obligation}
	if got, err := kaminoPairEntryCapacity(position, accounts, route); err != nil || got != 2000 {
		t.Fatal("closed account cannot advertise protocol capacity", got, err)
	}
	accounts[2] = ConfirmedAccount{}
	if _, err := kaminoPairEntryCapacity(position, accounts, route); err == nil {
		t.Fatal("missing account masquerades as closed")
	}
	accounts[0].Data = accounts[0].Data[:10]
	if _, err := kaminoPairEntryCapacity(position, accounts, route); err == nil {
		t.Fatal("truncated reserve admitted")
	}
}

func TestNetBorrowHeadroom(t *testing.T) {
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
