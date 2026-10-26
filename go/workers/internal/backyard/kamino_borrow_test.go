package backyard

import (
	"encoding/binary"
	"testing"
)

func TestBorrowFeesValueTheGrossDebitAndRejectWrongGraph(t *testing.T) {
	t.Parallel()
	_, _, _, m, _, _, accounts := fundingAdmissionFixture(t, 20_000)
	route := ethenaUSDePYUSD
	reserve := accountAt(accounts, route.Kamino.DebtReserve)
	putKey(t, reserve.Data[192:224], route.DebtFeeReceiver)
	binary.LittleEndian.PutUint64(reserve.Data[kaminoReserveConfigOffset+40:], 1<<52)
	fee := accountAt(accounts, route.DebtLiquiditySupply)
	fee.Address, fee.Data = route.DebtFeeReceiver, append([]byte(nil), fee.Data...)
	binary.LittleEndian.PutUint64(fee.Data[64:72], 5)
	accounts = append(accounts, fee)
	r, err := m.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegBorrow, 1000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	e, err := kaminoBorrowEffects(accounts, route, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	if e.Accounts[2].AfterRaw != 9 {
		t.Fatal("rounding omitted fee")
	}
	// Minimum, nearest-integer (including half-up), and zero fee semantics.
	for _, tc := range []struct{ rate, receive, want uint64 }{{0, 1, 0}, {1, 2, 1}, {1 << 52, 1000, 4}, {1 << 51, 1280, 3}} {
		binary.LittleEndian.PutUint64(reserve.Data[kaminoReserveConfigOffset+40:], tc.rate)
		got, err := kaminoBorrowFee(accounts, route, tc.receive)
		if err != nil || got != tc.want {
			t.Fatal("fee rounding mismatch", tc, got, err)
		}
	}
	binary.LittleEndian.PutUint64(reserve.Data[kaminoReserveConfigOffset+40:], 1)
	if _, err := kaminoBorrowFee(accounts, route, 1); err == nil {
		t.Fatal("borrow below minimum fee accepted")
	}
}
