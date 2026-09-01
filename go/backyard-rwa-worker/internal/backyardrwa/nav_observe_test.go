package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"math/big"
	"strings"
	"testing"
)

func tokenAccountFixture(t *testing.T, address, mint, authority string, raw uint64) ConfirmedAccount {
	t.Helper()
	mintKey, err := decodeBase58PublicKey(mint)
	if err != nil {
		t.Fatal(err)
	}
	authorityKey, err := decodeBase58PublicKey(authority)
	if err != nil {
		t.Fatal(err)
	}
	return ConfirmedAccount{
		Address: address, Owner: bridgeTokenProgram, Lamports: 1,
		Data: custodyFixture(mintKey, authorityKey, raw, false),
	}
}

func strategyReceiptFixture(t *testing.T, positionRaw uint64) ConfirmedAccount {
	t.Helper()
	data := make([]byte, strategyReceiptLength)
	copy(data[:8], strategyReceiptDiscriminator[:])
	putKey(t, data[8:40], bridgeVoltrVault)
	putKey(t, data[40:72], bridgeStrategy)
	putKey(t, data[72:104], bridgeAdaptorProgram)
	binary.LittleEndian.PutUint64(data[104:112], positionRaw)
	binary.LittleEndian.PutUint64(data[112:120], 1_700_000_000)
	data[120], data[121], data[122] = 1, 254, 253
	return ConfirmedAccount{Address: bridgeStrategyReceipt, Owner: bridgeVoltrProgram, Lamports: 1, Data: data}
}

func putScaledFraction(dst []byte, value *big.Int) {
	for index := range dst {
		dst[index] = 0
	}
	bytes := value.Bytes()
	for index := range bytes {
		dst[index] = bytes[len(bytes)-1-index]
	}
}

func reserveFixture(t *testing.T, address, mint string, slot int64, priceSF *big.Int, liquidityRaw, collateralSupply uint64) ConfirmedAccount {
	t.Helper()
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, kaminoReserveLength)
	copy(data[:8], kaminoReserveDiscriminator[:])
	binary.LittleEndian.PutUint64(data[8:16], 1)
	binary.LittleEndian.PutUint64(data[16:24], uint64(slot))
	data[25] = kaminoRequiredPriceStatus
	putKey(t, data[32:64], config.Market)
	putKey(t, data[128:160], mint)
	binary.LittleEndian.PutUint64(data[224:232], liquidityRaw)
	putScaledFraction(data[248:264], priceSF)
	binary.LittleEndian.PutUint64(data[2592:2600], collateralSupply)
	return ConfirmedAccount{Address: address, Owner: kaminoProgram, Lamports: 1, Data: data}
}

func obligationFixture(t *testing.T, slot int64, collateralReceiptRaw, debtRaw uint64) ConfirmedAccount {
	t.Helper()
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, kaminoObligationLength)
	copy(data[:8], kaminoObligationDiscriminator[:])
	binary.LittleEndian.PutUint64(data[16:24], uint64(slot))
	data[25] = kaminoRequiredPriceStatus
	putKey(t, data[32:64], config.Market)
	putKey(t, data[64:96], bridgeVault)
	if collateralReceiptRaw > 0 {
		putKey(t, data[96:128], config.CollateralReserve)
		binary.LittleEndian.PutUint64(data[128:136], collateralReceiptRaw)
	}
	if debtRaw > 0 {
		putKey(t, data[1208:1240], config.DebtReserve)
		putScaledFraction(data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(debtRaw), 60))
	}
	return ConfirmedAccount{Address: config.Obligation, Owner: config.Program, Lamports: 1, Data: data}
}

func routeNAVFixture(t *testing.T, slot int64) []ConfirmedAccount {
	t.Helper()
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	oneAndHalf := new(big.Int).Mul(big.NewInt(3), new(big.Int).Lsh(big.NewInt(1), 59))
	adaptor := exactAdaptorConfigAccount(t)
	return []ConfirmedAccount{
		adaptor,
		strategyReceiptFixture(t, 42),
		tokenAccountFixture(t, bridgeIdleATA, bridgeUSDC, bridgeIdleAuthority, 11),
		tokenAccountFixture(t, bridgeStrategyATA, bridgeUSDC, bridgeStrategyAuth, 5),
		tokenAccountFixture(t, bridgeSquadsATA, bridgeUSDC, bridgeVault, 6),
		tokenAccountFixture(t, kaminoPrimeCustody, kaminoPrimeMint, bridgeVault, 3),
		obligationFixture(t, slot, 10, 7),
		reserveFixture(t, kaminoCollateralReserve, kaminoPrimeMint, slot, oneAndHalf, 200, 100),
		reserveFixture(t, kaminoDebtReserve, kaminoUSDCMint, slot, one, 100, 100),
	}
}

func TestComputeRouteNAVValuesConfirmedCustodyAndPositionConservatively(t *testing.T) {
	manifest := readyWorkerManifest(t)
	accounts := routeNAVFixture(t, 77)
	got, err := ComputeRouteNAV(77, accounts, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	// PRIME idle: floor(3 * 1.5) = 4. Position: 10 receipt tokens
	// redeem 20 PRIME, worth 30 USDC. Debt is conservatively 7 USDC.
	// Strategy NAV = 5 strategy + 6 Squads + 4 PRIME + 30 - 7 = 38.
	if got.StrategyNAVRaw != 38 || got.VaultIdleRaw != 11 || got.TotalVaultNAVRaw != 49 ||
		got.PrimeIdleValueRaw != 4 || got.PositionCollateralValue != 30 || got.PositionDebtValue != 7 ||
		got.PriorReportedNAVRaw != 42 {
		t.Fatalf("unexpected route NAV: %+v", got)
	}
	if got.Report.Sequence != 77 || got.Report.ObservedSlot != 77 || got.Report.NAVAfterRaw != 38 ||
		!sha256Pattern.MatchString(got.Report.SnapshotDigest) || got.Report.SnapshotDigest != got.SnapshotDigest {
		t.Fatalf("invalid ReportV1 inputs: %+v", got.Report)
	}
	reversed := append([]ConfirmedAccount(nil), accounts...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	again, err := ComputeRouteNAV(77, reversed, manifest, nil)
	if err != nil || again.SnapshotDigest != got.SnapshotDigest {
		t.Fatalf("NAV digest depends on RPC account order: %+v err=%v", again, err)
	}
}

func TestComputeRouteNAVPoststateOverridesOnlyCustody(t *testing.T) {
	manifest := readyWorkerManifest(t)
	accounts := routeNAVFixture(t, 77)
	post := RouteNAVCustodies{VoltrIdleRaw: 7, StrategyUSDCraw: 5, SquadsUSDCraw: 10, SquadsPRIMEraw: 3}
	got, err := ComputeRouteNAV(77, accounts, manifest, &post)
	if err != nil {
		t.Fatal(err)
	}
	if got.StrategyNAVRaw != 42 || got.TotalVaultNAVRaw != 49 || got.Report.NAVAfterRaw != 42 {
		t.Fatalf("allocation poststate NAV is not conserved: %+v", got)
	}
}

func TestComputeRouteNAVRejectsStalePricesUnknownCustodyAndUnsupportedPosition(t *testing.T) {
	manifest := readyWorkerManifest(t)
	t.Run("stale reserve", func(t *testing.T) {
		accounts := routeNAVFixture(t, 77)
		accountAt(accounts, kaminoCollateralReserve).Data[24] = 1
		if _, err := ComputeRouteNAV(77, accounts, manifest, nil); err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("stale reserve accepted: %v", err)
		}
	})
	t.Run("invalid zero price", func(t *testing.T) {
		accounts := routeNAVFixture(t, 77)
		for index := 248; index < 264; index++ {
			accountAt(accounts, kaminoCollateralReserve).Data[index] = 0
		}
		if _, err := ComputeRouteNAV(77, accounts, manifest, nil); err == nil || !strings.Contains(err.Error(), "price") {
			t.Fatalf("zero reserve price accepted: %v", err)
		}
	})
	t.Run("unknown custody", func(t *testing.T) {
		accounts := append(routeNAVFixture(t, 77), ConfirmedAccount{Address: bridgeSettings, Owner: bridgeTokenProgram, Lamports: 1, Data: make([]byte, 165)})
		if _, err := ComputeRouteNAV(77, accounts, manifest, nil); err == nil || !strings.Contains(err.Error(), "unsupported custody") {
			t.Fatalf("unknown custody accepted: %v", err)
		}
	})
	t.Run("unsupported obligation reserve", func(t *testing.T) {
		accounts := routeNAVFixture(t, 77)
		obligation := accountAt(accounts, kaminoPrimeUSDCObligation)
		putKey(t, obligation.Data[232:264], bridgeSettings)
		binary.LittleEndian.PutUint64(obligation.Data[264:272], 1)
		if _, err := ComputeRouteNAV(77, accounts, manifest, nil); err == nil || !strings.Contains(err.Error(), "unsupported Kamino collateral") {
			t.Fatalf("unsupported active position accepted: %v", err)
		}
	})
}

func TestComputeRouteNAVRejectsOverflowNegativeNAVAndReceiptDrift(t *testing.T) {
	manifest := readyWorkerManifest(t)
	t.Run("valuation overflow", func(t *testing.T) {
		accounts := routeNAVFixture(t, 77)
		prime := accountAt(accounts, kaminoPrimeCustody)
		binary.LittleEndian.PutUint64(prime.Data[64:72], math.MaxUint64)
		if _, err := ComputeRouteNAV(77, accounts, manifest, nil); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("overflow accepted: %v", err)
		}
	})
	t.Run("negative NAV", func(t *testing.T) {
		accounts := routeNAVFixture(t, 77)
		accounts[6] = obligationFixture(t, 77, 10, 100)
		if _, err := ComputeRouteNAV(77, accounts, manifest, nil); err == nil || !strings.Contains(err.Error(), "underflow") {
			t.Fatalf("negative NAV accepted: %v", err)
		}
	})
	t.Run("receipt binding", func(t *testing.T) {
		accounts := routeNAVFixture(t, 77)
		accountAt(accounts, bridgeStrategyReceipt).Data[40] ^= 1
		if _, err := ComputeRouteNAV(77, accounts, manifest, nil); err == nil || !strings.Contains(err.Error(), "receipt") {
			t.Fatalf("drifted strategy receipt accepted: %v", err)
		}
	})
}

type fixtureNAVReader struct {
	confirmedSlot int64
	batchSlot     int64
	accounts      []ConfirmedAccount
	batchCalls    int
}

func (r *fixtureNAVReader) ConfirmedSlot(context.Context) (int64, error) { return r.confirmedSlot, nil }
func (r *fixtureNAVReader) GetMultipleAccounts(_ context.Context, addresses []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	r.batchCalls++
	if minimumSlot != r.confirmedSlot || strings.Join(addresses, ",") != strings.Join(pinnedRouteNAVAddresses(), ",") {
		return 0, nil, context.Canceled
	}
	return r.batchSlot, append([]ConfirmedAccount(nil), r.accounts...), nil
}

func TestObserveConfirmedRouteNAVUsesExactlyOneCoherentBatch(t *testing.T) {
	reader := &fixtureNAVReader{confirmedSlot: 76, batchSlot: 77, accounts: routeNAVFixture(t, 77)}
	got, err := ObserveConfirmedRouteNAV(context.Background(), reader, readyWorkerManifest(t))
	if err != nil || got.Slot != 77 || reader.batchCalls != 1 {
		t.Fatalf("single-batch observer failed: nav=%+v calls=%d err=%v", got, reader.batchCalls, err)
	}
	reader = &fixtureNAVReader{confirmedSlot: 78, batchSlot: 77, accounts: routeNAVFixture(t, 77)}
	if _, err := ObserveConfirmedRouteNAV(context.Background(), reader, readyWorkerManifest(t)); err == nil || !strings.Contains(err.Error(), "regressed") {
		t.Fatalf("mixed/regressed slot accepted: %v", err)
	}
}
