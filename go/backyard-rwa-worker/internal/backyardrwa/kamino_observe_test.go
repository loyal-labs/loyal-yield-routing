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
	oracle := bridgeVault
	putKey(t, reserve[5224:5256], oracle)
	decoded, err := decodeKaminoReserve(ConfirmedAccount{Address: c.CollateralReserve, Owner: c.Program, Lamports: 1, Data: reserve}, c.CollateralMint, c)
	if err != nil || len(uniqueNonzero(decoded.oracles)) != 1 || uniqueNonzero(decoded.oracles)[0] != oracle {
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

func TestKaminoRefreshAcceptsIndependentCurrentReservesAndRejectsOlderOrStaleState(t *testing.T) {
	o := decodedKaminoObligation{refreshedSlot: 10, hasPosition: true}
	if err := validateKaminoRefresh(o,
		decodedKaminoReserve{refreshedSlot: 11, priceStatus: kaminoRequiredPriceStatus},
		decodedKaminoReserve{refreshedSlot: 12, priceStatus: 0},
	); err != nil {
		t.Fatalf("independently refreshed reserves rejected: %v", err)
	}
	if err := validateKaminoRefresh(o, decodedKaminoReserve{refreshedSlot: 9}); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("reserve older than obligation accepted: %v", err)
	}
	if err := validateKaminoRefresh(o, decodedKaminoReserve{refreshedSlot: 11, stale: 1}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale reserve accepted: %v", err)
	}
	if err := validateKaminoRefresh(decodedKaminoObligation{refreshedSlot: 10, stale: 1, hasPosition: true}, decodedKaminoReserve{refreshedSlot: 11}); err == nil || !strings.Contains(err.Error(), "obligation") {
		t.Fatalf("stale obligation accepted: %v", err)
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
