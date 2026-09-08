package backyardrwa

import (
	"context"
	"encoding/binary"
	"math/big"
	"strings"
	"testing"
)

func TestKaminoAccruedDebtUsesUnroundedFractionAndFullRateLimbs(t *testing.T) {
	// KLend accrue_interest floors amountSF * newRate / oldRate before
	// converting Fraction to ceil raw. SDK 7.3.9 decodes these same offsets.
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	for _, row := range []struct {
		name                    string
		amount, former, current *big.Int
		want                    uint64
	}{
		{"unchanged", new(big.Int).Mul(big.NewInt(7), one), one, one, 7},
		{"accrued", new(big.Int).Mul(big.NewInt(7), one), one, new(big.Int).Rsh(new(big.Int).Mul(big.NewInt(5), one), 2), 9},
		{"round_only_after_ratio", new(big.Int).Quo(new(big.Int).Mul(big.NewInt(6), one), big.NewInt(5)), one, new(big.Int).Rsh(new(big.Int).Mul(big.NewInt(5), one), 2), 2},
		{"upper_limbs", new(big.Int).Mul(big.NewInt(7), one), new(big.Int).Lsh(big.NewInt(1), 100), new(big.Int).Lsh(big.NewInt(1), 101), 14},
		{"unchanged_high_rate_skips_multiplication", one, new(big.Int).Lsh(big.NewInt(1), 240), new(big.Int).Lsh(big.NewInt(1), 240), 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			o := decodedKaminoObligation{}
			r := decodedKaminoReserve{}
			putScaledFraction(o.debtAmountSF[:], row.amount)
			putScaledFraction(o.cumulativeBorrowRate[:], row.former)
			putScaledFraction(r.cumulativeBorrowRate[:], row.current)
			got, err := o.debtAtReserveRate(r)
			if err != nil || got != row.want {
				t.Fatalf("debt=%d want=%d err=%v", got, row.want, err)
			}
		})
	}
	for _, row := range []struct {
		name                    string
		amount, former, current *big.Int
	}{
		{"missing_rate", one, big.NewInt(0), one},
		{"regressed_rate", one, one, new(big.Int).Rsh(new(big.Int).Set(one), 1)},
		{"u256_product_overflow", new(big.Int).Lsh(big.NewInt(1), 100), one, new(big.Int).Lsh(big.NewInt(1), 200)},
		{"fraction_overflow", new(big.Int).Lsh(big.NewInt(1), 127), one, new(big.Int).Lsh(new(big.Int).Set(one), 1)},
		{"raw_overflow", new(big.Int).Lsh(big.NewInt(1), 124), one, one},
	} {
		t.Run(row.name, func(t *testing.T) {
			o := decodedKaminoObligation{}
			r := decodedKaminoReserve{}
			putScaledFraction(o.debtAmountSF[:], row.amount)
			putScaledFraction(o.cumulativeBorrowRate[:], row.former)
			putScaledFraction(r.cumulativeBorrowRate[:], row.current)
			if _, err := o.debtAtReserveRate(r); err == nil {
				t.Fatal("invalid interest state accepted")
			}
		})
	}
	if got, err := (decodedKaminoObligation{}).debtAtReserveRate(decodedKaminoReserve{}); err != nil || got != 0 {
		t.Fatal("empty debt requires no rate", err)
	}
}

func TestAccruedDebtFlowsThroughObservationNAVAndRepayment(t *testing.T) {
	route, accounts := nonUSDCDebtNAVFixture(t)
	reserve := accountAt(accounts, route.Kamino.DebtReserve)
	putKey(t, reserve.Data[5112:5144], kaminoScopePrices)
	// Stored seven debt units at rate 1 become 8.75 at rate 1.25; raw
	// liabilities ceil to nine. The reference USD price remains unchanged.
	putScaledFraction(reserve.Data[296:328], new(big.Int).Lsh(big.NewInt(5), 58))
	for i := 328; i < 344; i++ {
		reserve.Data[i] = 255
	} // BigFraction padding is NOT part of the rate.
	position, err := observeKaminoFromFixedAccounts(context.Background(), func(_ context.Context, _ []string, slot int64) (int64, []ConfirmedAccount, error) {
		return slot, []ConfirmedAccount{{Address: kaminoScopePrices, Lamports: 1, Data: []byte{1}}}, nil
	}, 77, append(append([]ConfirmedAccount{}, accounts...), clockFixture()), route.Kamino)
	if err != nil || position.DebtRaw != 9 {
		t.Fatalf("position=%+v err=%v", position, err)
	}
	ltv, err := observedLTVBPS(position)
	if err != nil || ltv != 6000 {
		t.Fatal("interest omitted from risk", ltv, err)
	}
	nav, err := ComputeRouteNAVForRoute(77, accounts, readyWorkerManifest(t), nil, route)
	if err != nil || nav.PositionDebtValue != 18 || nav.StrategyNAVRaw != 45 {
		t.Fatal("interest omitted from NAV", nav, err)
	}
	leg, wire, effect, err := selectKaminoLeg(Decision{Action: DeleverRouteStep, Reason: "withdrawal_repay_debt", AmountRaw: 9, StrategyKey: route.Lane}, position)
	if err != nil || leg != kaminoLegRepay || wire != 9 || effect != 9 {
		t.Fatal("repayment used unaccrued debt", wire, effect, err)
	}
	for _, offset := range []int{296, 1240} {
		_, bad := nonUSDCDebtNAVFixture(t)
		address := route.Kamino.DebtReserve
		if offset == 1240 {
			address = route.Kamino.Obligation
		}
		data := accountAt(bad, address).Data
		clear(data[offset : offset+32])
		if _, err := ComputeRouteNAVForRoute(77, bad, readyWorkerManifest(t), nil, route); err == nil {
			t.Fatal("missing rate became valid NAV", offset)
		}
	}
}

func TestTargetBorrowRespectsNineDecimalCollateral(t *testing.T) {
	p := KaminoPosition{CollateralDepositedRaw: 1_000_000_000, RedeemablePrimeRaw: 1_000_000_000, CollateralDecimals: 9, DebtDecimals: 6}
	binary.LittleEndian.PutUint64(p.CollateralPriceSF[:8], uint64(1)<<60)
	binary.LittleEndian.PutUint64(p.DebtPriceSF[:8], uint64(1)<<60)
	got, err := p.targetLTVBorrowRaw()
	if err != nil || got != 500_000 {
		t.Fatalf("nine-decimal collateral allowed debt=%d err=%v", got, err)
	}
	p.CollateralDecimals = 19
	if _, err = p.targetLTVBorrowRaw(); err == nil {
		t.Fatal("unsupported decimal scale accepted")
	}
}

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
	binary.LittleEndian.PutUint64(reserve[272:280], 9)
	reserve[kaminoReserveConfigOffset+645] = 90
	oracle := bridgeVault
	putKey(t, reserve[5224:5256], oracle)
	decoded, err := decodeKaminoReserve(ConfirmedAccount{Address: c.CollateralReserve, Owner: c.Program, Lamports: 1, Data: reserve}, c.CollateralMint, c)
	if err != nil || decoded.mintDecimals != 9 || decoded.utilizationLimitPct != 90 || len(uniqueNonzero(decoded.oracles)) != 1 || uniqueNonzero(decoded.oracles)[0] != oracle {
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
