package backyard

import (
	"encoding/binary"
	"math/big"
	"testing"
)

func TestDepositReconcilesFromTheReceiptAndRejectsMalformedEffects(t *testing.T) {
	t.Parallel()
	_, _, _, manifest, _, _, accounts := fundingAdmissionFixture(t, 20_000)
	route := ethenaUSDePYUSD
	reserve := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 42, new(big.Int).Lsh(big.NewInt(1), 60), 1_100_000_000, 1_000_000_000)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(reserve.Data[272:280], 9)
	binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
	binary.LittleEndian.PutUint32(reserve.Data[28:32], 1000)
	reserve.Data[kaminoReserveConfigOffset+9] = 1
	for i := 0; i < 11; i++ {
		o := kaminoReserveConfigOffset + 64 + i*8
		if i > 0 {
			binary.LittleEndian.PutUint32(reserve.Data[o:], 10_000)
		}
		binary.LittleEndian.PutUint32(reserve.Data[o+4:], 7500)
	}
	accounts = append(accounts, reserve)
	r, err := manifest.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	effects, err := boundedKaminoDepositEffects(accounts, route, 42, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	// The reserve supply is shared: other depositors moved it before landing,
	// and KLend debited less than the signed maximum. The receipt is the truth.
	supply := effects.Accounts[1].Address
	receipt := receiptFor(effects, map[string][2]uint64{
		effects.Accounts[0].Address: {effects.Accounts[0].BeforeRaw, effects.Accounts[0].AfterRaw + 3},
		supply:                      {effects.Accounts[1].BeforeRaw + 7_000, effects.Accounts[1].AfterRaw + 7_000 - 3},
	})
	if _, _, err := ReconcileConfirmedTransaction(rwa, effects, receipt); err != nil {
		t.Fatal("a landed deposit beside third-party reserve moves failed reconciliation", err)
	}
	receipt = receiptFor(effects, map[string][2]uint64{effects.Accounts[0].Address: {effects.Accounts[0].BeforeRaw + 1, effects.Accounts[0].AfterRaw + 1}})
	if _, _, err := ReconcileConfirmedTransaction(rwa, effects, receipt); err != nil {
		t.Fatal("a transfer into our collateral custody before landing failed reconciliation", err)
	}
	receipt = receiptFor(effects, map[string][2]uint64{
		effects.Accounts[0].Address: {effects.Accounts[0].BeforeRaw, effects.Accounts[0].BeforeRaw},
		supply:                      {effects.Accounts[1].BeforeRaw, effects.Accounts[1].BeforeRaw},
	})
	if _, _, err := ReconcileConfirmedTransaction(rwa, effects, receipt); err == nil {
		t.Fatal("a deposit that moved nothing reconciled")
	}
}
