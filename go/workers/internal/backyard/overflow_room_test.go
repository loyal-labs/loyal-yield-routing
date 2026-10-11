package backyard

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
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
// age and the observation batch does not carry it (2026-10-11: every tick
// logged overflow_room_unknown while PYUSD was shut, first as kamino_stale,
// then as a missing account). The room is judged on the overflow lane's own
// refresh simulation; a failed refresh leaves only this room unknown.
func TestOverflowRoomIsJudgedOnTheOverflowLanesRefresh(t *testing.T) {
	t.Parallel()
	const slot = int64(77)
	fresh := overflowRoomBatch(t, slot)
	stale := overflowRoomBatch(t, slot)
	binary.LittleEndian.PutUint64(accountAt(stale, autoAUTOWYLDS.Kamino.DebtReserve).Data[16:24], uint64(slot-kaminoMaxReserveAgeSlots-8))
	if _, _, err := overflowRoom(stale, autoAUTOWYLDS); !errors.Is(err, errKaminoReserveStale) {
		t.Fatalf("the stale raw wYLDS reserve was not refused raw: %v", err)
	}
	wantRaw, wantValue, err := overflowRoom(fresh, autoAUTOWYLDS)
	if err != nil || wantRaw == 0 || wantValue <= 0 {
		t.Fatalf("fresh fixture has no overflow room: %d %d %v", wantRaw, wantValue, err)
	}
	want, err := overflowRoomAddresses(autoAUTOWYLDS)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("room comes from the overflow lane's own refresh", func(t *testing.T) {
		t.Parallel()
		calls := 0
		var s Snapshot
		err := applyOverflowRoom(context.Background(), &s, slot, autoAUTOPYUSD, func(_ context.Context, routes []RuntimeRoute, addresses []string, minSlot int64) (int64, []ConfirmedAccount, error) {
			calls++
			if len(routes) != 1 || routes[0].Lane != autoAUTOWYLDS.Lane || minSlot != slot || len(addresses) != len(want) {
				t.Fatalf("refresh was not the overflow lane's own: %v %v %d", routes, addresses, minSlot)
			}
			out := make([]ConfirmedAccount, 0, len(addresses))
			for _, address := range addresses {
				out = append(out, accountAt(fresh, address))
			}
			return slot + 1, out, nil
		})
		if err != nil || calls != 1 || !s.OverflowRoomKnown || s.OverflowRoomRaw != wantRaw || s.OverflowRoomValueRaw != wantValue {
			t.Fatalf("room was not judged on the refresh: calls=%d %+v %v", calls, s, err)
		}
	})

	t.Run("a stale refreshed reserve leaves the room unknown", func(t *testing.T) {
		t.Parallel()
		s := Snapshot{OverflowRoomKnown: true, OverflowRoomRaw: 1}
		err := applyOverflowRoom(context.Background(), &s, slot, autoAUTOPYUSD, func(_ context.Context, _ []RuntimeRoute, addresses []string, _ int64) (int64, []ConfirmedAccount, error) {
			out := make([]ConfirmedAccount, 0, len(addresses))
			for _, address := range addresses {
				out = append(out, accountAt(stale, address))
			}
			return slot, out, nil
		})
		if !errors.Is(err, errKaminoReserveStale) || s.OverflowRoomKnown || s.OverflowRoomRaw != 0 {
			t.Fatalf("stale capture: %+v %v", s, err)
		}
	})

	t.Run("failed or absent refresh leaves the room unknown with its cause", func(t *testing.T) {
		t.Parallel()
		s := Snapshot{OverflowRoomKnown: true, OverflowRoomRaw: 1}
		cause := budgetHold("price_refresh_simulation_unavailable")
		err := applyOverflowRoom(context.Background(), &s, slot, autoAUTOPYUSD, func(context.Context, []RuntimeRoute, []string, int64) (int64, []ConfirmedAccount, error) {
			return 0, nil, cause
		})
		if !errors.Is(err, cause) || s.OverflowRoomKnown || s.OverflowRoomRaw != 0 {
			t.Fatalf("failed refresh: %+v %v", s, err)
		}
		s = Snapshot{OverflowRoomKnown: true}
		if err := applyOverflowRoom(context.Background(), &s, slot, autoAUTOPYUSD, nil); err == nil || s.OverflowRoomKnown {
			t.Fatalf("absent refresh: %+v %v", s, err)
		}
	})

	t.Run("a lane without an overflow lane stamps nothing", func(t *testing.T) {
		t.Parallel()
		s := Snapshot{OverflowRoomKnown: true}
		if err := applyOverflowRoom(context.Background(), &s, slot, autoAUTOWYLDS, nil); err != nil || s.OverflowRoomKnown {
			t.Fatalf("non-source lane: %+v %v", s, err)
		}
	})
}
