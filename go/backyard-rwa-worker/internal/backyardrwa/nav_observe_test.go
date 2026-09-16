package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"math/big"
	"strings"
	"testing"
)

func TestCrossDecimalValuationUsesPricesAndConservativeRounding(t *testing.T) {
	var one, two [16]byte
	binary.LittleEndian.PutUint64(one[:8], uint64(1)<<60)
	binary.LittleEndian.PutUint64(two[:8], uint64(2)<<60)
	for _, tc := range []struct {
		raw          uint64
		from, to     uint8
		price, quote [16]byte
		liability    bool
		want         uint64
	}{
		{1_000_000_000, 9, 6, one, one, false, 1_000_000},
		{1_000_000, 6, 9, one, one, false, 1_000_000_000},
		{1_000_000_000, 9, 6, one, two, false, 500_000},
		{1, 9, 6, one, one, false, 0},
		{1, 9, 6, one, one, true, 1},
	} {
		got, err := valueBetweenTokenRaw(tc.raw, tc.from, tc.to, tc.price, tc.quote, tc.liability)
		if err != nil || got != tc.want {
			t.Fatalf("cross-decimal value: got %d, want %d, err %v", got, tc.want, err)
		}
	}
}

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
	return strategyReceiptWithCustodyFixture(t, positionRaw, 0)
}

func strategyReceiptWithCustodyFixture(t *testing.T, positionRaw, custodyTrackedRaw uint64) ConfirmedAccount {
	t.Helper()
	data := make([]byte, strategyReceiptLength)
	copy(data[:8], strategyReceiptDiscriminator[:])
	putKey(t, data[8:40], bridgeVoltrVault)
	putKey(t, data[40:72], bridgeStrategy)
	putKey(t, data[72:104], bridgeAdaptorProgram)
	binary.LittleEndian.PutUint64(data[104:112], positionRaw)
	binary.LittleEndian.PutUint64(data[112:120], 1_700_000_000)
	data[120], data[121], data[122] = 2, 254, 253
	binary.LittleEndian.PutUint64(data[128:136], custodyTrackedRaw)
	return ConfirmedAccount{Address: bridgeStrategyReceipt, Owner: bridgeVoltrProgram, Lamports: 1, Data: data}
}

// kaminoFixtureUnix is the chain-time every fixture reserve publishes its
// oracle price at; route-level fixtures set the Clock sysvar to the same
// instant so the freshness gate sees a fresh batch.
const kaminoFixtureUnix = int64(1_700_000_000)

func marketFixture(t *testing.T, address string) ConfirmedAccount {
	t.Helper()
	data := make([]byte, kaminoMarketLength)
	copy(data[:8], kaminoMarketDiscriminator[:])
	return ConfirmedAccount{Address: address, Owner: kaminoProgram, Lamports: 1, Data: data}
}

// clockFixture publishes kaminoFixtureUnix as the batch's chain time so the
// reserve oracle freshness gate sees a fresh observation.
func clockFixture() ConfirmedAccount {
	data := make([]byte, 40)
	binary.LittleEndian.PutUint64(data[32:40], uint64(kaminoFixtureUnix))
	return ConfirmedAccount{Address: budgetClockAddress, Owner: "Sysvar1111111111111111111111111111111111111", Data: data}
}

func voltrVaultFixture(t *testing.T, totalValueRaw uint64) ConfirmedAccount {
	t.Helper()
	data := make([]byte, 928)
	copy(data[:8], voltrVaultDiscriminator[:])
	putKey(t, data[104:136], bridgeUSDC)
	putKey(t, data[136:168], bridgeIdleATA)
	putKey(t, data[272:304], bridgeLPMint)
	putKey(t, data[368:400], bridgeVault)
	putKey(t, data[400:432], bridgeSettingsSigner)
	binary.LittleEndian.PutUint64(data[168:176], totalValueRaw)
	binary.LittleEndian.PutUint64(data[448:456], 0)
	binary.LittleEndian.PutUint64(data[456:464], 600)
	binary.LittleEndian.PutUint64(data[616:624], 1_000)
	return ConfirmedAccount{Address: bridgeVoltrVault, Owner: bridgeVoltrProgram, Lamports: 1, Data: data}
}

func voltrLPMintFixture(t *testing.T, supplyRaw uint64) ConfirmedAccount {
	t.Helper()
	data := make([]byte, voltrLPMintLength)
	putKey(t, data[4:36], bridgeSettingsSigner)
	binary.LittleEndian.PutUint64(data[36:44], supplyRaw)
	data[44] = 9
	return ConfirmedAccount{Address: bridgeLPMint, Owner: bridgeTokenProgram, Lamports: 1, Data: data}
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
	binary.LittleEndian.PutUint64(data[272:280], 6)
	binary.LittleEndian.PutUint64(data[264:272], uint64(kaminoFixtureUnix))
	putScaledFraction(data[296:328], new(big.Int).Lsh(big.NewInt(1), 60))
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
		putScaledFraction(data[1240:1272], new(big.Int).Lsh(big.NewInt(1), 60))
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
		marketFixture(t, kaminoMarket),
		voltrVaultFixture(t, 49),
		voltrLPMintFixture(t, 1_000),
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
	// Strategy NAV excludes the 5 USDC sitting in the strategy custody ATA
	// (Voltr books that balance itself): 6 Squads + 4 PRIME + 30 - 7 = 33.
	if got.StrategyNAVRaw != 33 || got.VaultIdleRaw != 11 || got.TotalVaultNAVRaw != 44 ||
		got.PrimeIdleValueRaw != 4 || got.PositionCollateralValue != 30 || got.PositionDebtValue != 7 ||
		got.PriorReportedNAVRaw != 42 || got.Custodies.StrategyUSDCraw != 5 ||
		got.Receipt.CustodyTrackedRaw != 0 || got.Voltr.TotalValueRaw != 49 || got.LPSupplyRaw != 1_000 {
		t.Fatalf("unexpected route NAV: %+v", got)
	}
	if got.Report.Sequence != 77 || got.Report.ObservedSlot != 77 || got.Report.NAVAfterRaw != 33 ||
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

func nonUSDCDebtNAVFixture(t *testing.T) (RuntimeRoute, []ConfirmedAccount) {
	t.Helper()
	route, err := runtimeRoute(RouteID)
	if err != nil {
		t.Fatal(err)
	}
	// Controlled account images only; this does not install a runtime binding.
	route.Kamino.DebtMint = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"
	route.Kamino.DebtReserve = mapleSyrupUSDCUSDC.Kamino.DebtReserve
	route.DebtCustody = "J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn"
	route.DebtTokenProgram = token2022Program
	accounts := routeNAVFixture(t, 77)
	// 9-decimal collateral: 3,000 raw idle and 20,000 raw redeemable,
	// priced at 1.5 USDC/token; asset conversion must floor in micro-USDC.
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 3_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.CollateralReserve).Data[272:280], 9)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.Obligation).Data[128:136], 10_000)
	putKey(t, accountAt(accounts, route.Kamino.Obligation).Data[1208:1240], route.Kamino.DebtReserve)
	// Deliberately depeg the debt asset to 2 USDC. Equal mint decimals
	// must not turn its liability or idle balance into USDC at par.
	debt := reserveFixture(t, route.Kamino.DebtReserve, route.Kamino.DebtMint, 77,
		new(big.Int).Lsh(big.NewInt(2), 60), 100, 100)
	custody := tokenAccountFixture(t, route.DebtCustody, route.Kamino.DebtMint, bridgeVault, 9)
	custody.Owner = token2022Program
	return route, append(accounts, debt, custody)
}

func TestRouteNAVNormalizesNonUSDCDebtAndIncludesIdleDebt(t *testing.T) {
	route, accounts := nonUSDCDebtNAVFixture(t)
	got, err := ComputeRouteNAVForRoute(77, accounts, readyWorkerManifest(t), nil, route)
	if err != nil {
		t.Fatal(err)
	}
	// 6 USDC + floor(3*1.5) + 20*1.5 - 7*2 + 9*2, custody excluded.
	if got.StrategyNAVRaw != 44 || got.PositionDebtValue != 14 || got.DebtIdleValueRaw != 18 ||
		got.PositionCollateralValue != 30 || got.PrimeIdleValueRaw != 4 || got.TotalVaultNAVRaw != 55 {
		t.Fatalf("NAV mixed debt raw units with USDC or omitted idle debt: %+v", got)
	}
	post := got.Custodies
	post.SquadsDebtRaw--
	changed, err := ComputeRouteNAVForRoute(77, accounts, readyWorkerManifest(t), &post, route)
	if err != nil || changed.StrategyNAVRaw != 42 || changed.SnapshotDigest == got.SnapshotDigest {
		t.Fatalf("debt poststate did not affect valuation and its fingerprint: %+v, %v", changed, err)
	}
}

func TestNonUSDCDebtNAVRejectsIncompleteOrMismatchedInputs(t *testing.T) {
	for _, name := range []string{"missing reference", "missing custody", "wrong program", "wrong reference decimals", "future reference"} {
		t.Run(name, func(t *testing.T) {
			route, accounts := nonUSDCDebtNAVFixture(t)
			switch name {
			case "missing reference", "missing custody":
				address := kaminoDebtReserve
				if name == "missing custody" {
					address = route.DebtCustody
				}
				for i, a := range accounts {
					if a.Address == address {
						accounts = append(accounts[:i], accounts[i+1:]...)
						break
					}
				}
			case "wrong program":
				route.DebtTokenProgram = classicTokenProgram
			case "wrong reference decimals":
				binary.LittleEndian.PutUint64(accountAt(accounts, kaminoDebtReserve).Data[272:280], 9)
			case "future reference":
				binary.LittleEndian.PutUint64(accountAt(accounts, kaminoDebtReserve).Data[16:24], 78)
			}
			if _, err := ComputeRouteNAVForRoute(77, accounts, readyWorkerManifest(t), nil, route); err == nil {
				t.Fatal("unsafe non-USDC NAV input was accepted")
			}
		})
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
	// The custody override no longer enters the armed NAV: allocation reports
	// only external value, so Squads 10 + PRIME 4 + 30 - 7 = 37.
	if got.StrategyNAVRaw != 37 || got.TotalVaultNAVRaw != 44 || got.Report.NAVAfterRaw != 37 {
		t.Fatalf("allocation poststate NAV is not conserved: %+v", got)
	}
}

func TestComputeRouteNAVAcceptsRefreshMarkerAndRejectsInvalidInputs(t *testing.T) {
	manifest := readyWorkerManifest(t)
	t.Run("refresh marker", func(t *testing.T) {
		accounts := routeNAVFixture(t, 77)
		accountAt(accounts, kaminoCollateralReserve).Data[24] = 1
		if _, err := ComputeRouteNAV(77, accounts, manifest, nil); err != nil {
			t.Fatalf("transaction refresh marker rejected: %v", err)
		}
	})
	t.Run("zero refresh slot", func(t *testing.T) {
		accounts := routeNAVFixture(t, 77)
		binary.LittleEndian.PutUint64(accountAt(accounts, kaminoCollateralReserve).Data[16:24], 0)
		if _, err := ComputeRouteNAV(77, accounts, manifest, nil); err == nil || !strings.Contains(err.Error(), "reserve") {
			t.Fatalf("zero reserve refresh slot accepted: %v", err)
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

func TestStrategyTwoReceiptVersionAndTrackedCustody(t *testing.T) {
	a := strategyReceiptWithCustodyFixture(t, 500, 100)
	r, err := decodeStrategyReceipt(a)
	if err != nil || r.PositionValueRaw != 500 || r.CustodyTrackedRaw != 100 {
		t.Fatalf("version2 custody: %+v %v", r, err)
	}
	for _, version := range []byte{0, 1, 3, 255} {
		a.Data[120] = version
		if _, err := decodeStrategyReceipt(a); err == nil {
			t.Fatalf("unreviewed receipt version %d", version)
		}
	}
	a.Data[120] = 2
	a.Data[136] = 1
	if _, err := decodeStrategyReceipt(a); err == nil {
		t.Fatal("unknown reserved field accepted")
	}
}
