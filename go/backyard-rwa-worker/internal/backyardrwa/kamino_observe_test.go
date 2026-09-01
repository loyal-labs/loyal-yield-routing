package backyardrwa

import (
	"encoding/binary"
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
	oracle := bridgeVault
	putKey(t, reserve[5200:5232], oracle)
	decoded, err := decodeKaminoReserve(ConfirmedAccount{Address: c.CollateralReserve, Owner: c.Program, Lamports: 1, Data: reserve}, c.CollateralMint, c)
	if err != nil || len(uniqueNonzero(decoded.oracles)) != 1 || uniqueNonzero(decoded.oracles)[0] != oracle {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	reserve[128] ^= 1
	if _, err := decodeKaminoReserve(ConfirmedAccount{Address: c.CollateralReserve, Owner: c.Program, Lamports: 1, Data: reserve}, c.CollateralMint, c); err == nil {
		t.Fatal("reserve mint drift accepted")
	}
}

func TestKaminoRefreshFailsClosedOnSlotOrPriceDrift(t *testing.T) {
	o := decodedKaminoObligation{refreshedSlot: 10, priceStatus: kaminoRequiredPriceStatus}
	r := decodedKaminoReserve{refreshedSlot: 11, priceStatus: kaminoRequiredPriceStatus}
	if err := validateKaminoRefresh(o, r); err == nil || !strings.Contains(err.Error(), "incoherent") {
		t.Fatalf("refresh drift accepted: %v", err)
	}
	if err := validateKaminoRefresh(decodedKaminoObligation{refreshedSlot: 10, priceStatus: 1}, decodedKaminoReserve{refreshedSlot: 10, priceStatus: kaminoRequiredPriceStatus}); err == nil {
		t.Fatal("invalid price status accepted")
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
