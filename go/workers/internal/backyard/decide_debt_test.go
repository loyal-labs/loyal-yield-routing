package backyard

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestFixedAccountObservationPreservesDecimalsAndUSDCEntryCapacity(t *testing.T) {
	t.Parallel()
	route, accounts := nonUSDCDebtNAVFixture(t)
	// Feed the production fixed-account decoder a configured oracle, and 9/6
	// decimal assets. At 1.5/2 prices, 20,000 collateral raw units support
	// floor(20,000/1e9 * 1.5/2 * .5 * 1e6) = 7 debt raw units.
	putKey(t, accountAt(accounts, route.Kamino.CollateralReserve).Data[5112:5144], kaminoScopePrices)
	position, err := observeKaminoFromFixedAccounts(77, append(append([]ConfirmedAccount{}, accounts...), clockFixture()), route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	borrow, err := position.targetLTVBorrowRaw()
	if err != nil || borrow != 7 {
		t.Fatalf("lost decimal scale in real observation: borrow=%d err=%v", borrow, err)
	}
	ltv, err := observedLTVBPS(position)
	if err != nil || ltv != 4667 {
		t.Fatalf("LTV lost decimal scale or rounded risk down: %d %v", ltv, err)
	}
	if _, _, err = withdrawExcessForRepayment(position); err == nil {
		t.Fatal("released collateral above the unwind LTV")
	}
	position.EntryCapacityRaw = 123
	capacity, err := routeEntryCapacityUSDC(position, accounts, route)
	if err != nil || capacity != 246 {
		t.Fatalf("debt capacity inherited a USDC peg: %d %v", capacity, err)
	}
	position.DebtDecimals, position.EntryCapacityRaw = 9, 1234
	capacity, err = routeEntryCapacityUSDC(position, accounts, route)
	if err != nil || capacity != 2 {
		t.Fatalf("entry capacity did not floor across decimals: %d %v", capacity, err)
	}
	nav, err := ComputeRouteNAVForRoute(77, accounts, readyWorkerManifest(t), nil, route)
	if err != nil {
		t.Fatal(err)
	}
	s := base()
	s.Slot = 77
	if err = applyRouteNAVSnapshot(&s, nav, time.Unix(1_700_000_010, 0)); err != nil || s.DebtIdleRaw != 9 || s.CollateralIdleValueRaw != int64(nav.PrimeIdleValueRaw) {
		t.Fatal("debt custody lost before planning", err)
	}
	s.StrategyNAVRaw = int64(nav.StrategyNAVRaw)
	s.VoltrIdleRaw, s.VoltrStrategyIdleRaw, s.SquadsIdleRaw = int64(nav.Custodies.VoltrIdleRaw), int64(nav.Custodies.StrategyUSDCraw), int64(nav.Custodies.SquadsUSDCraw)
	s.HasPosition, s.PositionCollateralRaw, s.PositionDebtRaw = true, int64(position.CollateralDepositedRaw), 7
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw, s.LTVBPS = int64(nav.PositionCollateralValue), int64(nav.PositionDebtValue), ltv
	projection, err := newRouteObservationProjection(Observation{Snapshot: s, ObservedAt: time.Unix(1_700_000_010, 0)})
	if err != nil || projection.DebtIdleRaw != "9" || projection.CollateralIdleValueRaw != fmt.Sprint(nav.PrimeIdleValueRaw) {
		t.Fatal("debt custody lost from durable projection", err)
	}
	nav.Custodies.SquadsDebtRaw = math.MaxUint64
	if err = applyRouteNAVSnapshot(&s, nav, time.Unix(1_700_000_010, 0)); err == nil {
		t.Fatal("debt custody overflow accepted")
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, kaminoDebtReserve).Data[272:280], 9)
	if _, err = routeEntryCapacityUSDC(position, accounts, route); err == nil {
		t.Fatal("wrong USDC reference decimals accepted")
	}
}
