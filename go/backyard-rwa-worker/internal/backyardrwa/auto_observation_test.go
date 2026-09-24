package backyardrwa

// Doc-14 candidate observation proof: a test-local coherent AUTO confirmed
// batch drives the production confirmed-route observer through its existing
// injectable runtime boundary. Nothing on the observation path is mocked: the
// real Kamino obligation/reserve/market decoders, the route NAV calculator,
// the pinned custody decoders, the report ticket, the receipt fence, and the
// candidate policy readiness gate all decode the same synthetic images, exactly
// as one confirmed RPC batch would deliver them.
//
// The combined candidate policy and the four masked bridge policy pins come
// from the doc-13 readiness fixture, so the pin digests and the account bytes
// can never drift apart across the two files. Legacy PRIME/USDC identities
// stay in the batch because the candidate address set still requests them: the
// legacy USDC reference reserve is decoded for NAV and entry-capacity parity,
// and the rest are inert for AUTO.
//
// B-facing fixture surface (report to coordinator; no shared mutable state):
//
//	func autoObservationBatch(t *testing.T, slot int64, mutate func([]ConfirmedAccount)) (RouteManifest, RuntimeRoute, []ConfirmedAccount)
//
// The default image carries an owned position (10e9 nine-decimal receipt
// tokens, 7e6 raw six-decimal PYUSD debt, 3_000 idle collateral, 9e6 idle
// debt); flat and adversarial variants are produced by mutating the returned
// batch, never the manifest or any global.

import (
	"context"
	"encoding/binary"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

const (
	autoFixtureDepositReceiptRaw = uint64(10_000_000_000)
	autoFixtureDebtRaw           = uint64(7_000_000)
	autoFixtureCollateralIdleRaw = uint64(3_000)
	autoFixtureDebtIdleRaw       = uint64(9_000_000)
	// AUTO collateral pool: the position owns half of a 20e9-receipt supply
	// backed by 40e9 liquidity tokens, so one receipt always redeems two
	// collateral tokens and no balance exceeds the pool that backs it.
	autoFixturePoolReceiptSupply = uint64(20_000_000_000)
	autoFixturePoolLiquidity     = uint64(40_000_000_000)
	// PYUSD pool: 100e6 tokens of which the position borrowed 7e6, with the
	// f-token supply equal to the pool for a 1:1 exchange rate.
	autoFixtureDebtPoolLiquidity = uint64(100_000_000)
)

// autoObservationClock publishes the slot and chain-time fields the
// deposit-rounding and payoff windows read; clockFixture leaves the slot at
// zero, which those windows must reject.
func autoObservationClock(slot int64) ConfirmedAccount {
	data := make([]byte, 40)
	binary.LittleEndian.PutUint64(data[0:8], uint64(slot))
	binary.LittleEndian.PutUint64(data[32:40], uint64(kaminoFixtureUnix))
	return ConfirmedAccount{Address: budgetClockAddress, Owner: "Sysvar1111111111111111111111111111111111111", Data: data}
}

// putKaminoBorrowCurve writes a valid 11-point utilisation/rate curve so the
// deposit-rounding and payoff windows see a decodable maximum borrow rate.
func putKaminoBorrowCurve(data []byte) {
	config := data[kaminoReserveConfigOffset:]
	for point := 0; point < 11; point++ {
		offset := 64 + point*8
		binary.LittleEndian.PutUint32(config[offset:offset+4], uint32(point*1_000))
		binary.LittleEndian.PutUint32(config[offset+4:offset+8], uint32(100*(point+1)))
	}
}

// kaminoReserveImage extends the shared reserve fixture with the fields the
// route observer validates beyond plain NAV: the reserve's own market, its
// mint decimals, one configured oracle, an 80% liquidation threshold, a valid
// borrow curve, and the borrowed-liquidity scaled fraction so the pool's
// available + borrowed accounting is internally consistent with the balances
// the obligation and the supply vaults report.
func kaminoReserveImage(t *testing.T, market, address, mint string, slot int64, priceSF *big.Int, availableRaw, borrowedRaw, receiptSupplyRaw uint64, decimals uint8) ConfirmedAccount {
	t.Helper()
	reserve := reserveFixture(t, address, mint, slot, priceSF, availableRaw, receiptSupplyRaw)
	putKey(t, reserve.Data[32:64], market)
	binary.LittleEndian.PutUint64(reserve.Data[272:280], uint64(decimals))
	putScaledFraction(reserve.Data[232:248], new(big.Int).Lsh(new(big.Int).SetUint64(borrowedRaw), 60))
	putKey(t, reserve.Data[5112:5144], kaminoPrimeMint)
	reserve.Data[kaminoReserveConfigOffset+17] = 80
	putKaminoBorrowCurve(reserve.Data)
	return reserve
}

// kaminoObligationImage is the route-parameterized obligation fixture; the
// shared obligationFixture pins the legacy PRIME/USDC market, which the AUTO
// decoders must refuse.
func kaminoObligationImage(t *testing.T, route RuntimeRoute, slot int64, collateralReceiptRaw, debtRaw uint64) ConfirmedAccount {
	t.Helper()
	data := make([]byte, kaminoObligationLength)
	copy(data[:8], kaminoObligationDiscriminator[:])
	binary.LittleEndian.PutUint64(data[16:24], uint64(slot))
	data[25] = kaminoRequiredPriceStatus
	putKey(t, data[32:64], route.Kamino.Market)
	putKey(t, data[64:96], route.Kamino.Vault)
	if collateralReceiptRaw > 0 {
		putKey(t, data[96:128], route.Kamino.CollateralReserve)
		binary.LittleEndian.PutUint64(data[128:136], collateralReceiptRaw)
	}
	if debtRaw > 0 {
		putKey(t, data[1208:1240], route.Kamino.DebtReserve)
		putScaledFraction(data[1240:1272], new(big.Int).Lsh(big.NewInt(1), 60))
		putScaledFraction(data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(debtRaw), 60))
	}
	return ConfirmedAccount{Address: route.Kamino.Obligation, Owner: route.Kamino.Program, Lamports: 1, Data: data}
}

// autoObservationBatch builds one coherent confirmed AUTO candidate batch at
// the given slot and returns the candidate manifest (validated AUTO binding,
// AUTO selected lane), the resolved route, and the batch. The economics are
// internally consistent: the position's 10e9 receipts are half of a 20e9
// receipt supply backed by 40e9 pooled collateral tokens (2:1 redeemable), and
// its 7e6 PYUSD debt is part of a 100e6-token pool whose borrowed scaled
// fraction carries it. The liquidity supply vaults hold exactly those pooled
// balances under the lending market authority (token-2022 for the PYUSD
// vault), and the collateral reserve prices its token at 1.5 USDC with nine
// decimals while PYUSD carries six, so every value the observer reports has
// crossed an unequal-decimal price conversion, never a par assumption. mutate
// runs last and must adjust batch bytes only.
func autoObservationBatch(t *testing.T, slot int64, mutate func([]ConfirmedAccount)) (RouteManifest, RuntimeRoute, []ConfirmedAccount) {
	t.Helper()
	readiness := newAutoReadinessFixture(t)
	readiness.manifest.RuntimeActivation.SelectedLane = autoAUTOPYUSD.Lane
	route, err := readiness.manifest.activeRuntimeRoute()
	if err != nil || route.Lane != autoAUTOPYUSD.Lane {
		t.Fatalf("candidate manifest did not select the AUTO route: %v, %v", route, err)
	}
	peg := new(big.Int).Lsh(big.NewInt(1), 60)
	oneAndHalf := new(big.Int).Mul(big.NewInt(3), new(big.Int).Lsh(big.NewInt(1), 59))
	debtCustody := tokenAccountFixture(t, route.DebtCustody, route.Kamino.DebtMint, bridgeVault, autoFixtureDebtIdleRaw)
	debtCustody.Owner = token2022Program
	// Pooled PYUSD liquidity is what the reserve has NOT lent out: 93e6
	// available mirrors the reserve's available/borrowed split exactly.
	debtSupplyVault := tokenAccountFixture(t, route.DebtLiquiditySupply, route.Kamino.DebtMint, route.Kamino.MarketAuthority,
		autoFixtureDebtPoolLiquidity-autoFixtureDebtRaw)
	debtSupplyVault.Owner = token2022Program
	accounts := []ConfirmedAccount{
		exactAdaptorConfigAccount(t),
		strategyReceiptFixture(t, 42),
		tokenAccountFixture(t, bridgeIdleATA, bridgeUSDC, bridgeIdleAuthority, 11),
		tokenAccountFixture(t, bridgeStrategyATA, bridgeUSDC, bridgeStrategyAuth, 0),
		tokenAccountFixture(t, bridgeSquadsATA, bridgeUSDC, bridgeVault, 6),
		voltrVaultFixture(t, 53),
		voltrLPMintFixture(t, 1_000),
		exactReportTicketAccount(t, 4),
		autoObservationClock(slot),
		// One configured oracle account serves every reserve image.
		{Address: kaminoPrimeMint, Owner: kaminoProgram, Lamports: 1, Data: []byte{1}},
		// Legacy PRIME/USDC identities the candidate batch still requests.
		kaminoReserveImage(t, kaminoMarket, kaminoCollateralReserve, kaminoPrimeMint, slot, oneAndHalf, 200, 0, 100, 6),
		kaminoReserveImage(t, kaminoMarket, kaminoDebtReserve, kaminoUSDCMint, slot, peg, 100, 0, 100, 6),
		marketFixture(t, kaminoMarket),
		tokenAccountFixture(t, kaminoPrimeCustody, kaminoPrimeMint, bridgeVault, 0),
		obligationFixture(t, slot, 0, 0),
		tokenAccountFixture(t, kaminoPrimeLiquiditySupply, kaminoPrimeMint, kaminoPrimeMarketAuthority, 200),
		tokenAccountFixture(t, kaminoUSDCLiquiditySupply, bridgeUSDC, kaminoPrimeMarketAuthority, 100),
		// The candidate AUTO protocol identities themselves.
		kaminoReserveImage(t, route.Kamino.Market, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, slot, oneAndHalf,
			autoFixturePoolLiquidity, 0, autoFixturePoolReceiptSupply, 9),
		kaminoReserveImage(t, route.Kamino.Market, route.Kamino.DebtReserve, route.Kamino.DebtMint, slot, peg,
			autoFixtureDebtPoolLiquidity-autoFixtureDebtRaw, autoFixtureDebtRaw, autoFixtureDebtPoolLiquidity, 6),
		marketFixture(t, route.Kamino.Market),
		kaminoObligationImage(t, route, slot, autoFixtureDepositReceiptRaw, autoFixtureDebtRaw),
		tokenAccountFixture(t, route.CollateralCustody, route.Kamino.CollateralMint, bridgeVault, autoFixtureCollateralIdleRaw),
		debtCustody,
		tokenAccountFixture(t, route.CollateralLiquiditySupply, route.Kamino.CollateralMint, route.Kamino.MarketAuthority, autoFixturePoolLiquidity),
		debtSupplyVault,
		{Address: route.DebtFeeReceiver, Owner: "11111111111111111111111111111111", Lamports: 1},
	}
	accounts = readiness.accounts(t, route, accounts...)
	if mutate != nil {
		mutate(accounts)
	}
	return readiness.manifest, route, accounts
}

// autoObservationForAccounts runs the production confirmed-route observer over
// one fixed batch through the injectable runtime boundary: no transport, no
// open withdrawal receipts, chain time equal to the fixture's Clock instant.
func autoObservationForAccounts(manifest RouteManifest, slot int64, accounts []ConfirmedAccount) func(context.Context) (Observation, []ConfirmedAccount, error) {
	accountsReader, finalizedReceipt := fixtureBatchRuntime(slot, accounts)
	return func(ctx context.Context) (Observation, []ConfirmedAccount, error) {
		return observeConfirmedRouteSnapshotWithAccounts(ctx, manifest, routeObservationRuntime{
			confirmedSlot: func(context.Context) (int64, error) { return slot, nil },
			receipts: func(_ context.Context, _ int64) (int64, []programAccount, error) {
				// No open withdrawal receipts, so the queue demand is zero.
				return slot, nil, nil
			},
			accounts:         accountsReader,
			finalizedReceipt: finalizedReceipt,
			now:              func() time.Time { return time.Unix(kaminoFixtureUnix, 0).UTC() },
		})
	}
}

func removeConfirmedAccount(accounts []ConfirmedAccount, address string) []ConfirmedAccount {
	for index := range accounts {
		if accounts[index].Address == address {
			return append(accounts[:index:index], accounts[index+1:]...)
		}
	}
	return accounts
}

func flattenAutoPosition(accounts []ConfirmedAccount) {
	obligation := accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation)
	for i := 96; i < 136; i++ {
		obligation.Data[i] = 0
	}
	for i := 1208; i < 1312; i++ {
		obligation.Data[i] = 0
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, autoAUTOPYUSD.CollateralCustody).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, autoAUTOPYUSD.DebtCustody).Data[64:72], 0)
}

// TestAutoCandidateObservationReportsOwnedPYUSDPosition proves the full
// production observer end to end on a coherent candidate AUTO batch: every
// reported amount keeps its raw units and its price-scaled USDC value, the
// unequal 9/6 decimal scales cross through the reserve prices, the payoff
// window bounds the PYUSD debt, and candidate readiness arms the route.
func TestAutoCandidateObservationReportsOwnedPYUSDPosition(t *testing.T) {
	manifest, route, accounts := autoObservationBatch(t, 77, nil)
	observation, batch, err := autoObservationForAccounts(manifest, 77, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) == 0 || observation.Snapshot.Slot != 77 || !observation.Snapshot.Fresh {
		t.Fatalf("owned AUTO batch did not observe fresh: %+v", observation.Snapshot)
	}
	if err := observation.Validate(); err != nil {
		t.Fatal(err)
	}
	if observation.Snapshot.RouteLane != route.Lane || observation.Snapshot.StrategyKey != route.Lane {
		t.Fatalf("observation lost the candidate lane identity: %+v", observation.Snapshot)
	}
	// Position view: exact raw amounts in their own token scales.
	if !observation.Snapshot.HasPosition || !observation.Snapshot.ObligationPresent || !observation.Snapshot.ObligationPresenceKnown {
		t.Fatalf("owned position was not reported as a present position: %+v", observation.Snapshot)
	}
	if observation.Snapshot.PositionCollateralRaw != int64(autoFixtureDepositReceiptRaw) || observation.Snapshot.PositionDebtRaw != int64(autoFixtureDebtRaw) {
		t.Fatalf("position raws lost their token scales: %+v", observation.Snapshot)
	}
	if observation.Snapshot.PrimeIdleRaw != int64(autoFixtureCollateralIdleRaw) || observation.Snapshot.DebtIdleRaw != int64(autoFixtureDebtIdleRaw) {
		t.Fatalf("idle custody raws lost their token scales: %+v", observation.Snapshot)
	}
	// NAV view: 3_000 nine-decimal tokens at 1.5 floor to 4 micro-USDC of idle
	// collateral; 10e9 receipts redeem 20e9 collateral tokens worth 30_000_000
	// micro-USDC; 7e6 PYUSD at par is 7_000_000; 9e6 idle PYUSD is 9_000_000.
	// Squads cash 6 + 4 + 30_000_000 - 7_000_000 + 9_000_000 = 32_000_010.
	if observation.Snapshot.CollateralIdleValueRaw != 4 || observation.Snapshot.PositionCollateralValueRaw != 30_000_000 || observation.Snapshot.PositionDebtValueRaw != 7_000_000 {
		t.Fatalf("unequal-decimal valuation drifted: %+v", observation.Snapshot)
	}
	if observation.Snapshot.StrategyNAVRaw != 32_000_010 || observation.Snapshot.TotalVaultNAVRaw != 32_000_021 || observation.Snapshot.PriorReportedNAVRaw != 42 {
		t.Fatalf("strategy NAV mispriced the PYUSD liability: %+v", observation.Snapshot)
	}
	// The liability survives into the planning inputs: a payoff window bound,
	// a nonzero LTV at the 80% threshold, and explicit custody parity.
	if observation.Snapshot.PayoffDebtRaw < int64(autoFixtureDebtRaw)+1 || observation.Snapshot.PayoffDebtRaw > 7_100_000 {
		t.Fatalf("payoff window did not bound the PYUSD debt: %d", observation.Snapshot.PayoffDebtRaw)
	}
	if observation.Snapshot.LTVBPS != 2_334 || observation.Snapshot.LiquidationThresholdBPS != 8_000 {
		t.Fatalf("LTV or threshold misread: %d/%d", observation.Snapshot.LTVBPS, observation.Snapshot.LiquidationThresholdBPS)
	}
	if observation.Snapshot.MinimumCollateralDepositRaw <= 0 {
		t.Fatalf("deposit rounding bound was not decoded from the AUTO reserve: %+v", observation.Snapshot)
	}
	if !observation.Snapshot.MonitorsArmed || !observation.Snapshot.PolicyReady || !observation.Snapshot.ExitBuildable {
		t.Fatalf("candidate policy readiness did not arm the route: %+v", observation.Snapshot)
	}
	if observation.Snapshot.BorrowUtilizationBlocked || observation.Snapshot.CapacityRaw != 0 || observation.Snapshot.TicketLastConsumedSequenceRaw != 4 {
		t.Fatalf("unexpected book facts on the healthy candidate batch: %+v", observation.Snapshot)
	}
	if observation.Snapshot.ValuationSource != "confirmed" || observation.Snapshot.ValuationSlot != 77 {
		t.Fatalf("valuation provenance drifted: %+v", observation.Snapshot)
	}
}

// TestAutoCandidateObservationValuesFlatPositionWithoutLiability proves a
// closed AUTO position observes flat explicitly: the obligation is present,
// every collateral and debt figure is zero, and the NAV is cash only.
func TestAutoCandidateObservationValuesFlatPositionWithoutLiability(t *testing.T) {
	manifest, _, accounts := autoObservationBatch(t, 77, flattenAutoPosition)
	observation, _, err := autoObservationForAccounts(manifest, 77, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := observation.Snapshot
	if snapshot.HasPosition || snapshot.PositionDebtRaw != 0 || snapshot.PositionCollateralRaw != 0 || snapshot.PayoffDebtRaw != 0 {
		t.Fatalf("flat batch reported a liability: %+v", snapshot)
	}
	if !snapshot.ObligationPresent || !snapshot.ObligationPresenceKnown {
		t.Fatalf("flat batch did not carry explicit obligation presence: %+v", snapshot)
	}
	if snapshot.PositionDebtValueRaw != 0 || snapshot.PositionCollateralValueRaw != 0 || snapshot.LTVBPS != 0 || snapshot.DebtIdleRaw != 0 {
		t.Fatalf("flat batch kept position value: %+v", snapshot)
	}
	if snapshot.StrategyNAVRaw != 6 || snapshot.TotalVaultNAVRaw != 17 {
		t.Fatalf("cash-only NAV drifted: %+v", snapshot)
	}
	if !snapshot.MonitorsArmed || !snapshot.PolicyReady || !snapshot.ExitBuildable || !snapshot.Fresh {
		t.Fatalf("flat candidate batch did not arm: %+v", snapshot)
	}
}

// TestAutoCandidateObservationPricesAbovePegDebtThroughReservePrice proves a
// non-USDC debt asset is never silently interpreted as USDC: repricing PYUSD to
// 102/100 USDC moves the liability value, the idle debt value, the NAV, and
// the LTV exactly through the observed reserve price, on top of the 9/6
// decimal split every owned case already crosses. The price sits one ulp below
// the exact decimal 1.02, so the liability ceils to 7_140_000 while the idle
// asset floors to 9_179_999: the same price must round each side
// conservatively.
func TestAutoCandidateObservationPricesAbovePegDebtThroughReservePrice(t *testing.T) {
	abovePeg := func(accounts []ConfirmedAccount) {
		putScaledFraction(accountAt(accounts, autoAUTOPYUSD.Kamino.DebtReserve).Data[248:264],
			new(big.Int).Quo(new(big.Int).Mul(big.NewInt(102), new(big.Int).Lsh(big.NewInt(1), 60)), big.NewInt(100)))
	}
	manifest, _, accounts := autoObservationBatch(t, 77, abovePeg)
	observation, _, err := autoObservationForAccounts(manifest, 77, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := observation.Snapshot
	if snapshot.PositionDebtValueRaw != 7_140_000 || snapshot.StrategyNAVRaw != 32_040_009 || snapshot.TotalVaultNAVRaw != 32_040_020 {
		t.Fatalf("above-peg debt was not repriced through the reserve price: %+v", snapshot)
	}
	if snapshot.DebtIdleRaw != int64(autoFixtureDebtIdleRaw) {
		t.Fatalf("idle debt raw units were disturbed: %+v", snapshot)
	}
	if snapshot.LTVBPS != 2_380 || snapshot.PayoffDebtRaw < int64(autoFixtureDebtRaw)+1 {
		t.Fatalf("above-peg LTV or payoff bound drifted: %+v", snapshot)
	}
}

// TestAutoCandidateObservationKeepsLiabilityWhenPolicyIsNotReady proves the
// missing-or-wrong policy result can never look like a flat position: the
// readiness gate drops to false while every position fact survives intact.
func TestAutoCandidateObservationKeepsLiabilityWhenPolicyIsNotReady(t *testing.T) {
	for name, breakPolicy := range map[string]func(accounts []ConfirmedAccount){
		"missing": func(accounts []ConfirmedAccount) {
			// Handled by removal below; nothing to mutate in place.
		},
		"drifted": func(accounts []ConfirmedAccount) {
			accountAt(accounts, autoFixturePolicy).Data[100] ^= 0x40
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest, _, accounts := autoObservationBatch(t, 77, breakPolicy)
			if name == "missing" {
				accounts = removeConfirmedAccount(accounts, autoFixturePolicy)
			}
			observation, _, err := autoObservationForAccounts(manifest, 77, accounts)(context.Background())
			if err != nil {
				t.Fatalf("%s policy broke the observation instead of its readiness: %v", name, err)
			}
			snapshot := observation.Snapshot
			if snapshot.PolicyReady || snapshot.ExitBuildable {
				t.Fatalf("%s policy still reported the candidate ready: %+v", name, snapshot)
			}
			if !snapshot.HasPosition || snapshot.PositionDebtRaw != int64(autoFixtureDebtRaw) || snapshot.PositionDebtValueRaw != 7_000_000 {
				t.Fatalf("%s policy dropped the AUTO liability from the book: %+v", name, snapshot)
			}
			if snapshot.StrategyNAVRaw != 32_000_010 || !snapshot.MonitorsArmed || !snapshot.ObligationPresent {
				t.Fatalf("%s policy disturbed the coherent book: %+v", name, snapshot)
			}
		})
	}
}

// TestAutoCandidateForeignReserveIdentityFailsClosed proves a reserve account
// that does not bind the AUTO mint is an observation failure, never a decode
// the observer quietly reinterprets.
func TestAutoCandidateForeignReserveIdentityFailsClosed(t *testing.T) {
	foreign := func(accounts []ConfirmedAccount) {
		putKey(t, accountAt(accounts, autoAUTOPYUSD.Kamino.DebtReserve).Data[128:160], kaminoPrimeMint)
	}
	manifest, _, accounts := autoObservationBatch(t, 77, foreign)
	observation, _, err := autoObservationForAccounts(manifest, 77, accounts)(context.Background())
	if err == nil || !strings.Contains(err.Error(), "identity drifted") {
		t.Fatalf("a foreign AUTO debt reserve was not rejected: %+v, %v", observation.Snapshot, err)
	}
	if observation.Snapshot.Fresh || observation.Snapshot.MonitorsArmed {
		t.Fatalf("a foreign reserve produced a usable snapshot: %+v", observation.Snapshot)
	}
}

// TestAutoCandidateStaleValuationHoldsAndNeverDropsToCash proves the
// fail-closed health hold for a stale AUTO reserve, and that the cash-only
// fallback that USDC-debt lanes may use can never drop a non-USDC lane's
// position: the fallback refuses the route outright.
func TestAutoCandidateStaleValuationHoldsAndNeverDropsToCash(t *testing.T) {
	stale := func(accounts []ConfirmedAccount) {
		binary.LittleEndian.PutUint64(accountAt(accounts, autoAUTOPYUSD.Kamino.CollateralReserve).Data[16:24], uint64(77-kaminoMaxReserveAgeSlots-8))
	}
	manifest, route, accounts := autoObservationBatch(t, 77, stale)
	observation, _, err := autoObservationForAccounts(manifest, 77, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Snapshot.ManualReason != "kamino_stale" || observation.Snapshot.Fresh || observation.Snapshot.MonitorsArmed {
		t.Fatalf("a stale AUTO reserve did not become a health hold: %+v", observation.Snapshot)
	}
	if observation.Snapshot.PositionDebtRaw != 0 && observation.Snapshot.HasPosition {
		t.Fatalf("the health hold leaked position facts: %+v", observation.Snapshot)
	}
	// The same batch cannot fall back to cash-only accounting either: a
	// non-USDC debt lane with any reserve-health fault keeps its original error.
	reader, _ := fixtureBatchRuntime(77, accounts)
	position, fallbackErr := observeKaminoWithCashFallback(context.Background(), reader, 77, accounts, route)
	if !errors.Is(fallbackErr, errKaminoReserveStale) {
		t.Fatalf("the AUTO cash fallback accepted a stale reserve: %+v, %v", position, fallbackErr)
	}
	if position.HasPosition || position.DebtRaw != 0 || position.ObligationPresent {
		t.Fatalf("the AUTO cash fallback manufactured a flat position: %+v", position)
	}
}

// TestAutoCandidateDebtWithoutCollateralIsNeverFlat proves a liability with no
// redeemable collateral fails the observation outright: an uncollateralized
// AUTO debt can never be mispriced into a healthy flat or valued snapshot.
func TestAutoCandidateDebtWithoutCollateralIsNeverFlat(t *testing.T) {
	collateralDropped := func(accounts []ConfirmedAccount) {
		obligation := accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation)
		for i := 96; i < 136; i++ {
			obligation.Data[i] = 0
		}
	}
	manifest, _, accounts := autoObservationBatch(t, 77, collateralDropped)
	observation, _, err := autoObservationForAccounts(manifest, 77, accounts)(context.Background())
	if err == nil || !strings.Contains(err.Error(), "redeemable collateral") {
		t.Fatalf("an uncollateralized AUTO debt was not rejected: %+v, %v", observation.Snapshot, err)
	}
}

// TestAutoCandidateAbsentObligationStaysExplicit proves the explicit-absence
// contract: with the AUTO obligation account null in the batch (the production
// wire form of an absent optional account is an address-only image), the
// snapshot reports ObligationPresent=false and zero position instead of
// silently decoding a flat position from a missing account.
func TestAutoCandidateAbsentObligationStaysExplicit(t *testing.T) {
	manifest, _, accounts := autoObservationBatch(t, 77, nil)
	replaceAccount(accounts, autoAUTOPYUSD.Kamino.Obligation, ConfirmedAccount{Address: autoAUTOPYUSD.Kamino.Obligation})
	observation, _, err := autoObservationForAccounts(manifest, 77, accounts)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := observation.Snapshot
	if snapshot.ObligationPresent || snapshot.HasPosition || snapshot.PositionDebtRaw != 0 || snapshot.PositionCollateralRaw != 0 {
		t.Fatalf("an absent obligation was folded into a decoded position: %+v", snapshot)
	}
	if !snapshot.ObligationPresenceKnown || !snapshot.MonitorsArmed {
		t.Fatalf("absence was not carried as an explicit observation fact: %+v", snapshot)
	}
}

// An empty AUTO lane (no position, no AUTO or PYUSD custody) is valued as cash
// when its reserves are stale, like an empty USDC lane; any AUTO exposure
// keeps the health hold (TestAutoCandidateStaleValuationHoldsAndNeverDropsToCash).
func TestAutoEmptyLaneStaleReserveFallsBackToCash(t *testing.T) {
	stale := func(accounts []ConfirmedAccount) {
		flattenAutoPosition(accounts)
		binary.LittleEndian.PutUint64(accountAt(accounts, autoAUTOPYUSD.Kamino.CollateralReserve).Data[16:24], uint64(77-kaminoMaxReserveAgeSlots-8))
	}
	_, route, accounts := autoObservationBatch(t, 77, stale)
	reader, _ := fixtureBatchRuntime(77, accounts)
	position, err := observeKaminoWithCashFallback(context.Background(), reader, 77, accounts, route)
	if err != nil || position.HasPosition || position.DebtRaw != 0 || position.CollateralDepositedRaw != 0 || !position.BorrowUtilizationBlocked {
		t.Fatalf("empty stale AUTO lane did not fall back to cash: %+v, %v", position, err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 1)
	if _, err := observeKaminoWithCashFallback(context.Background(), reader, 77, accounts, route); !errors.Is(err, errKaminoReserveStale) {
		t.Fatalf("PYUSD custody was valued as cash on a stale reserve: %v", err)
	}
}
