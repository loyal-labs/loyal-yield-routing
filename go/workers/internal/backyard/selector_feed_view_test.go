package backyard

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"
)

// The selector feed prices a lane only from its reserves as Backyard's account
// view holds them, observed at the view slot's chain time. A lane whose
// reserve is absent from the view is left out, and a view whose chain time is
// older than the market bound prices nothing.
func TestReserveFeedPricesLanesOnlyFromCurrentViewReserves(t *testing.T) {
	t.Parallel()
	route, now, body := nativeFixture(t)
	native, err := parseNativeYields([]byte(body), []RuntimeRoute{route}, now, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const slot = 500
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	debt := reserveFixture(t, route.Kamino.DebtReserve, route.Kamino.DebtMint, slot, one, 100_000_000, 100)
	putScaledFraction(debt.Data[128+104:128+120], new(big.Int).Lsh(big.NewInt(50_000_000), 60))
	curve := debt.Data[4856+64:]
	for i := range 11 { // KLend pads its eleven points with the terminal one
		point := [][2]uint32{{0, 0}, {9000, 364}, {10_000, 1075}}[min(i, 2)]
		binary.LittleEndian.PutUint32(curve[i*8:], point[0])
		binary.LittleEndian.PutUint32(curve[i*8+4:], point[1])
	}
	// Collateral is borrowed too: 20 of 100 on the same curve, so suppliers
	// earn kamino.Reserve.Rates' supply APY.
	collateral := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, slot, one, 80, 100)
	putScaledFraction(collateral.Data[128+104:128+120], new(big.Int).Lsh(big.NewInt(20), 60))
	copy(collateral.Data[4856+64:4856+64+88], debt.Data[4856+64:4856+64+88])
	usdc := reserveFixture(t, kaminoDebtReserve, bridgeUSDC, slot, one, 100, 100)
	putKey(t, debt.Data[32:64], route.Kamino.Market)
	putKey(t, collateral.Data[32:64], route.Kamino.Market)
	feed := func(chainTime time.Time, debt ConfirmedAccount, slotsPerSecond float64) []LaneEconomics {
		t.Helper()
		clock := ConfirmedAccount{Address: budgetClockAddress, Owner: budgetClockAddress, Lamports: 1, Data: make([]byte, 40)}
		binary.LittleEndian.PutUint64(clock.Data[32:], uint64(chainTime.Unix()))
		view := &View{through: slot, advanced: make(chan struct{}), accounts: map[string]viewAccount{}}
		for _, account := range []ConfirmedAccount{clock, usdc, debt, collateral} {
			view.accounts[account.Address] = viewAccount{ConfirmedAccount: account, slot: slot}
		}
		reserves, err := readReserveEconomics(context.Background(), view, []RuntimeRoute{route}, func(context.Context) (float64, error) {
			if slotsPerSecond == 0 {
				return 0, errors.New("no performance samples")
			}
			return slotsPerSecond, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return combineEconomics([]RuntimeRoute{route}, reserves, native, now, DefaultSelectorPolicy())
	}
	chainTime := now.Add(-time.Second)
	// Legacy basis accrues the curve times the measured slot rate over 2: the
	// 364 bps at 9000 interpolated to 20% utilization, on 20% borrowed.
	for _, slotsPerSecond := range []float64{2.5, 4.6} {
		supplyAPY := math.Expm1(364.0 * 2000 / 9000 / 10_000 * 0.2 * slotsPerSecond / 2)
		if got := feed(chainTime, debt, slotsPerSecond); len(got) != 1 || !got[0].ObservedAt.Equal(chainTime) || got[0].DebtBorrowRaw != 50_000_000 || math.Abs(got[0].SupplyAPY-supplyAPY) > 1e-12 {
			t.Fatalf("lane at %v slots/s = %+v, want one lane observed at %s with supply APY %v", slotsPerSecond, got, chainTime, supplyAPY)
		}
	}
	if got := feed(chainTime, debt, 0); len(got) != 0 {
		t.Fatalf("Legacy lane priced without a measured slot rate: %+v", got)
	}
	if got := feed(chainTime, ConfirmedAccount{Address: debt.Address}, 4.6); len(got) != 0 {
		t.Fatalf("lane priced without its debt reserve: %+v", got)
	}
	if got := feed(now.Add(-2*time.Minute), debt, 4.6); len(got) != 0 {
		t.Fatalf("lane priced from a view two minutes behind: %+v", got)
	}
}
