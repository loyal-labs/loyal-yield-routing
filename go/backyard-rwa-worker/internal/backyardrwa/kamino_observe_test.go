package backyardrwa

import (
	"encoding/binary"
	"math/big"
	"strings"
	"testing"
)

func TestDecodeKaminoPrimeUSDCRejectsTopologyAndDecodesOracles(t *testing.T) {
	c := KaminoObservationConfig{
		Program: kaminoProgram, Market: kaminoMarket, Obligation: bridgeSettings,
		CollateralReserve: kaminoCollateralReserve, DebtReserve: kaminoDebtReserve,
		Vault: bridgeVault, CollateralMint: kaminoPrimeMint, DebtMint: kaminoUSDCMint,
	}
	reserve := make([]byte, kaminoReserveLength)
	copy(reserve[:8], kaminoReserveDiscriminator[:])
	binary.LittleEndian.PutUint64(reserve[8:16], 1)
	binary.LittleEndian.PutUint64(reserve[16:24], 77)
	reserve[25] = kaminoRequiredPriceStatus
	putKey(t, reserve[32:64], c.Market)
	putKey(t, reserve[128:160], c.CollateralMint)
	binary.LittleEndian.PutUint64(reserve[224:232], 100)
	reserve[kaminoReserveConfigOffset+645] = 90
	oracle := bridgeVault
	putKey(t, reserve[5224:5256], oracle)
	decoded, err := decodeKaminoReserve(ConfirmedAccount{Address: c.CollateralReserve, Owner: c.Program, Lamports: 1, Data: reserve}, c.CollateralMint, c)
	if err != nil || decoded.utilizationLimitPct != 90 || len(uniqueNonzero(decoded.oracles)) != 1 || uniqueNonzero(decoded.oracles)[0] != oracle {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	reserve[128] ^= 1
	if _, err := decodeKaminoReserve(ConfirmedAccount{Address: c.CollateralReserve, Owner: c.Program, Lamports: 1, Data: reserve}, c.CollateralMint, c); err == nil {
		t.Fatal("reserve mint drift accepted")
	}
}

func TestKaminoObligationAcceptsTheFourLifecycleStates(t *testing.T) {
	c := KaminoObservationConfig{
		Program: kaminoProgram, Market: kaminoMarket, Obligation: bridgeSettings,
		CollateralReserve: kaminoCollateralReserve, DebtReserve: kaminoDebtReserve,
		Vault: bridgeVault, CollateralMint: kaminoPrimeMint, DebtMint: kaminoUSDCMint,
	}
	data := make([]byte, kaminoObligationLength)
	copy(data[:8], kaminoObligationDiscriminator[:])
	putKey(t, data[32:64], c.Market)
	putKey(t, data[64:96], c.Vault)
	putKey(t, data[96:128], c.CollateralReserve)
	binary.LittleEndian.PutUint64(data[128:136], 7)
	decoded, err := decodeKaminoObligation(ConfirmedAccount{Address: c.Obligation, Owner: c.Program, Lamports: 1, Data: data}, c)
	if err != nil || decoded.collateralDepositedRaw != 7 || decoded.debtRaw != 0 || !decoded.hasPosition {
		t.Fatalf("collateral-only state decoded=%+v err=%v", decoded, err)
	}
	putKey(t, data[1208:1240], c.DebtReserve)
	binary.LittleEndian.PutUint64(data[1296:1304], uint64(1)<<60)
	decoded, err = decodeKaminoObligation(ConfirmedAccount{Address: c.Obligation, Owner: c.Program, Lamports: 1, Data: data}, c)
	if err != nil || decoded.collateralDepositedRaw != 7 || decoded.debtRaw != 1 {
		t.Fatalf("complete state decoded=%+v err=%v", decoded, err)
	}
	binary.LittleEndian.PutUint64(data[128:136], 0)
	decoded, err = decodeKaminoObligation(ConfirmedAccount{Address: c.Obligation, Owner: c.Program, Lamports: 1, Data: data}, c)
	if err != nil || decoded.collateralDepositedRaw != 0 || decoded.debtRaw != 1 {
		t.Fatalf("debt-only state decoded=%+v err=%v", decoded, err)
	}
}

func TestKaminoCollateralExchangeRateUsesExactScaledFractionFloor(t *testing.T) {
	reserve := decodedKaminoReserve{totalLiquiditySF: new(big.Int).Lsh(big.NewInt(120), 60), collateralMintSupply: 100}
	got, err := reserve.redeemLiquidityRaw(25)
	if err != nil || got != 30 {
		t.Fatalf("redeemable=%d err=%v", got, err)
	}
}

func TestKaminoEntryCapacityBoundsOneRedepositAndBorrowHeadroom(t *testing.T) {
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	price := [16]byte{}
	putScaledFraction(price[:], one)
	collateral := decodedKaminoReserve{
		totalLiquiditySF: new(big.Int).Lsh(big.NewInt(100), 60),
		depositLimitRaw:  250,
		marketPriceSF:    price,
	}
	debt := decodedKaminoReserve{
		borrowedRaw:    20,
		borrowLimitRaw: 70,
		marketPriceSF:  price,
	}
	// Collateral permits an initial 100 (150 * 2/3); debt permits 100
	// (50 * 2), so the reviewed exact bound is 100.
	if got, err := entryCapacityDebtRaw(collateral, debt); err != nil || got != 100 {
		t.Fatalf("capacity=%d err=%v", got, err)
	}
	debt.borrowLimitRaw = 60
	if got, err := entryCapacityDebtRaw(collateral, debt); err != nil || got != 80 {
		t.Fatalf("debt-limited capacity=%d err=%v", got, err)
	}
}

func TestKaminoUtilizationGateCapsEntryAndBlocksBorrowAtBoundary(t *testing.T) {
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	total := new(big.Int).Mul(new(big.Int).Set(one), big.NewInt(100))
	borrowed := new(big.Int).Mul(new(big.Int).Set(one), big.NewInt(9368))
	borrowed.Quo(borrowed, big.NewInt(100))
	debt := decodedKaminoReserve{
		totalLiquiditySF: total, borrowedLiquiditySF: borrowed,
		borrowedRaw: 94, borrowLimitRaw: 1_000, utilizationLimitPct: 90,
	}
	if blocked, err := borrowingBlockedByUtilization(debt); err != nil || !blocked {
		t.Fatalf("93.68%% utilization was not blocked by the 90%% gate: blocked=%t err=%v", blocked, err)
	}

	debt.borrowedLiquiditySF = new(big.Int).Mul(new(big.Int).Set(one), big.NewInt(80))
	debt.borrowedRaw = 80
	if headroom, err := utilizationBorrowHeadroomRaw(debt); err != nil || headroom != 9 {
		t.Fatalf("utilization headroom=%d err=%v", headroom, err)
	}
	price := [16]byte{}
	putScaledFraction(price[:], one)
	collateral := decodedKaminoReserve{
		totalLiquiditySF: new(big.Int).Mul(new(big.Int).Set(one), big.NewInt(100)),
		depositLimitRaw:  250, marketPriceSF: price,
	}
	debt.marketPriceSF = price
	if capacity, err := entryCapacityDebtRaw(collateral, debt); err != nil || capacity != 18 {
		t.Fatalf("entry capacity did not include utilization headroom: capacity=%d err=%v", capacity, err)
	}

	debt.borrowedLiquiditySF = new(big.Int).Mul(new(big.Int).Set(one), big.NewInt(90))
	debt.borrowedRaw = 90
	if blocked, err := borrowingBlockedByUtilization(debt); err != nil || !blocked {
		t.Fatalf("exact utilization boundary admitted a borrow: blocked=%t err=%v", blocked, err)
	}
}

func TestKaminoRefreshAcceptsIndependentRefreshMarkersAndRejectsOlderState(t *testing.T) {
	o := decodedKaminoObligation{refreshedSlot: 10, stale: 1, hasPosition: true}
	if err := validateKaminoRefresh(o,
		decodedKaminoReserve{refreshedSlot: 11, stale: 1, priceStatus: kaminoRequiredPriceStatus},
		decodedKaminoReserve{refreshedSlot: 12, priceStatus: 0},
	); err != nil {
		t.Fatalf("independently refreshed reserves rejected: %v", err)
	}
	if err := validateKaminoRefresh(o, decodedKaminoReserve{refreshedSlot: 9}); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("reserve older than obligation accepted: %v", err)
	}
	if err := validateKaminoRefresh(o, decodedKaminoReserve{}); err == nil || !strings.Contains(err.Error(), "reserve") {
		t.Fatalf("zero-slot reserve accepted: %v", err)
	}
	if err := validateKaminoRefresh(decodedKaminoObligation{hasPosition: true}, decodedKaminoReserve{refreshedSlot: 11}); err == nil || !strings.Contains(err.Error(), "obligation") {
		t.Fatalf("zero-slot obligation accepted: %v", err)
	}
}

func putKey(t *testing.T, dst []byte, value string) {
	t.Helper()
	key, err := decodeBase58PublicKey(value)
	if err != nil {
		t.Fatal(err)
	}
	copy(dst, key[:])
}
