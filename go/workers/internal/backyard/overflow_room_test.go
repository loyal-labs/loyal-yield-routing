package backyard

import (
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
	if err := applyOverflowRoom(&s, fresh, autoAUTOPYUSD); err != nil || s.OverflowRoomKnown {
		t.Fatalf("source with room: %+v %v", s, err)
	}
	s = closed
	if err := applyOverflowRoom(&s, fresh, autoAUTOPYUSD); err != nil || !s.OverflowRoomKnown || s.OverflowRoomRaw != wantRaw || s.OverflowRoomValueRaw != wantValue {
		t.Fatalf("room was not judged on the batch: %+v %v", s, err)
	}
	s = closed
	s.OverflowRoomKnown, s.OverflowRoomRaw = true, 1
	if err := applyOverflowRoom(&s, stale, autoAUTOPYUSD); !errors.Is(err, errKaminoReserveStale) || s.OverflowRoomKnown || s.OverflowRoomRaw != 0 {
		t.Fatalf("stale batch: %+v %v", s, err)
	}
	s = Snapshot{OverflowRoomKnown: true}
	if err := applyOverflowRoom(&s, fresh, autoAUTOWYLDS); err != nil || s.OverflowRoomKnown {
		t.Fatalf("non-source lane: %+v %v", s, err)
	}
	// The batch refreshes the overflow lane with its source even while the
	// overflow lane holds nothing.
	if got := withOverflowRoutes([]RuntimeRoute{autoAUTOPYUSD}); len(got) != 2 || got[1].Lane != autoAUTOWYLDS.Lane {
		t.Fatalf("overflow lane not refreshed with its source: %+v", got)
	}
	if got := withOverflowRoutes([]RuntimeRoute{autoAUTOPYUSD, autoAUTOWYLDS}); len(got) != 2 {
		t.Fatalf("overflow lane refreshed twice: %+v", got)
	}
}
