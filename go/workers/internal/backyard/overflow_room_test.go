package backyard

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// overflowRoomBatch is the AUTO observation batch plus the overflow lane's
// own accounts: its debt reserve (the PYUSD fixture reserve re-minted as
// wYLDS, same market) and its absent obligation.
func overflowRoomBatch(t *testing.T, slot int64) []ConfirmedAccount {
	t.Helper()
	_, _, accounts := autoObservationBatch(t, slot, nil)
	debt := accountAt(accounts, autoAUTOPYUSD.Kamino.DebtReserve)
	debt.Address, debt.Data = autoAUTOWYLDS.Kamino.DebtReserve, append([]byte(nil), debt.Data...)
	putKey(t, debt.Data[128:160], autoAUTOWYLDS.Kamino.DebtMint)
	// Open the borrow and deposit limits, as capacityBorrowGateFixture does.
	binary.LittleEndian.PutUint64(debt.Data[kaminoOutsideBorrowLimitOffset:], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(debt.Data[kaminoReserveConfigOffset+168:], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, autoAUTOWYLDS.Kamino.CollateralReserve).Data[kaminoReserveConfigOffset+160:], 10_000_000_000_000)
	return append(accounts, debt, ConfirmedAccount{Address: autoAUTOWYLDS.Kamino.Obligation})
}

// Nobody but us borrows wYLDS, so its raw reserve sits far past the health
// age. The room is judged on the tick's refreshed batch, which refreshes the
// overflow lane's reserves with the valued lanes' (withOverflowRoutes); a
// stale batch leaves only this room unknown.
func TestOverflowRoomIsJudgedOnTheRefreshedBatch(t *testing.T) {
	t.Parallel()
	const slot = int64(77)
	fresh := overflowRoomBatch(t, slot)
	stale := overflowRoomBatch(t, slot)
	binary.LittleEndian.PutUint64(accountAt(stale, autoAUTOWYLDS.Kamino.DebtReserve).Data[16:24], uint64(slot-adaptorMaxReportAgeSlots-8))
	wantRaw, wantValue, err := overflowRoom(fresh, autoAUTOWYLDS)
	if err != nil || wantRaw == 0 || wantValue <= 0 {
		t.Fatalf("fresh fixture has no overflow room: %d %d %v", wantRaw, wantValue, err)
	}
	// The source lane (AUTO/PYUSD) cannot borrow: the only state in which
	// the overflow room is read.
	closed := Snapshot{HasPosition: true, BorrowCapacityKnown: true, BorrowUtilizationBlocked: true}

	s := Snapshot{BorrowCapacityKnown: true, AdditionalDebtRoomRaw: 1_000_000_000_000, OverflowRoomKnown: true}
	if err := applyOverflowRoom(rwa, &s, fresh, autoAUTOPYUSD); err != nil || s.OverflowRoomKnown {
		t.Fatalf("source with room: %+v %v", s, err)
	}
	s = closed
	if err := applyOverflowRoom(rwa, &s, fresh, autoAUTOPYUSD); err != nil || !s.OverflowRoomKnown || s.OverflowRoomRaw != wantRaw || s.OverflowRoomValueRaw != wantValue {
		t.Fatalf("room was not judged on the batch: %+v %v", s, err)
	}
	s = closed
	s.OverflowRoomKnown, s.OverflowRoomRaw = true, 1
	if err := applyOverflowRoom(rwa, &s, stale, autoAUTOPYUSD); !errors.Is(err, errKaminoReserveStale) || s.OverflowRoomKnown || s.OverflowRoomRaw != 0 {
		t.Fatalf("stale batch: %+v %v", s, err)
	}
	s = Snapshot{OverflowRoomKnown: true}
	if err := applyOverflowRoom(rwa, &s, fresh, autoAUTOWYLDS); err != nil || s.OverflowRoomKnown {
		t.Fatalf("non-source lane: %+v %v", s, err)
	}
	// The batch refreshes the overflow lane with its source even while the
	// overflow lane holds nothing.
	if got := withOverflowRoutes(rwa, []RuntimeRoute{autoAUTOPYUSD}); len(got) != 2 || got[1].Lane != autoAUTOWYLDS.Lane {
		t.Fatalf("overflow lane not refreshed with its source: %+v", got)
	}
	if got := withOverflowRoutes(rwa, []RuntimeRoute{autoAUTOPYUSD, autoAUTOWYLDS}); len(got) != 2 {
		t.Fatalf("overflow lane refreshed twice: %+v", got)
	}
}

// Kamino refusing to refresh an overflow lane nothing holds leaves only its
// room unknown: the held lanes are valued from their own capture and the
// refusal never counts toward the vault's refresh latch.
func TestUnheldOverflowRefreshRefusalNeverLatches(t *testing.T) {
	refreshSimulationFailures.Store(0)
	t.Cleanup(func() { refreshSimulationFailures.Store(0) })
	manifest, accounts := multiLaneFixture(t, nil)
	valued := fixtureRefresh(77, accounts)
	calls := 0
	for i := 0; i < refreshSimulationLatchAfter+1; i++ {
		o, _, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), manifest, routeObservationRuntime{state: testVaultState,
			read: fixtureBatchRuntime(77, accounts),
			refreshValuation: func(ctx context.Context, routes []RuntimeRoute, addresses []string, minSlot int64) (int64, []ConfirmedAccount, error) {
				calls++
				for _, r := range routes {
					if r.Lane == autoAUTOWYLDSLane {
						return 0, nil, &BudgetHold{Reason: holdStale, Details: map[string]string{"cause": "price_refresh_simulation_failed", "transactionError": `{"InstructionError":[0,{"Custom":6009}]}`}}
					}
				}
				return valued(ctx, routes, addresses, minSlot)
			},
			now: func() time.Time { return time.Unix(kaminoFixtureUnix, 0).UTC() },
		})
		if err != nil || o.Snapshot.ManualReason != "" || o.Snapshot.HeldLanes != autoAUTOPYUSD.Lane || o.Snapshot.OverflowRoomKnown {
			t.Fatalf("tick %d: %v %q %+v", i, err, o.Snapshot.ManualReason, o.Snapshot)
		}
	}
	if refreshSimulationFailures.Load() != 0 || calls != 2*(refreshSimulationLatchAfter+1) {
		t.Fatalf("an unheld overflow lane's refusal counted: failures=%d calls=%d", refreshSimulationFailures.Load(), calls)
	}
}
