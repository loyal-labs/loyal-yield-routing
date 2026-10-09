package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// leverage175Accounts turns the controlled OnRe fixture into a 1.75x
// position: collateral value C, debt D = (1.75-1)/1.75 * C (LTV 42.9%).
// Fixture prices: ONyc (9 dp) $1, USDC debt (6 dp) $2 per raw-unit math, so
// debt raw = collateral value in USDC-6 / 2.
func leverage175Fixture(t *testing.T) (Observation, RouteManifest, *chain.Client, *jupiterClient, []ConfirmedAccount, RuntimeRoute) {
	t.Helper()
	o, m, rpc, client, accounts := usdcReturnFixtureForLane(t, onreONycUSDC)
	route, _ := runtimeRoute(onreONycUSDC)
	c := accountAt(accounts, route.Kamino.CollateralReserve).Data
	c[kaminoLoanToValueOffset], c[kaminoReserveConfigOffset+17] = 66, 75
	// Align the Kamino debt price ($1) with the Jupiter mock's ONyc->USDC
	// rate (1 ONyc = 1 USDC).
	putScaledFraction(accountAt(accounts, route.Kamino.DebtReserve).Data[248:264], new(big.Int).Lsh(big.NewInt(1), 60))
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.Market).Data[kaminoGlobalBorrowValueOffset:], 1<<40)
	// 100 ONyc (1e11 raw at 9 dp) = $100; debt 42.857% = 42_857_142 USDC-6.
	obligation := accountAt(accounts, route.Kamino.Obligation).Data
	binary.LittleEndian.PutUint64(obligation[128:136], 100_000_000_000)
	debt := uint64(42_857_142)
	putScaledFraction(obligation[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(debt), 60))
	supply := accountAt(accounts, route.Kamino.CollateralReserve).Data
	binary.LittleEndian.PutUint64(supply[224:232], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(supply[2592:2600], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralLiquiditySupply).Data[64:72], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 0)
	// The debt reserve has lent at least our debt.
	putScaledFraction(accountAt(accounts, route.Kamino.DebtReserve).Data[232:248], new(big.Int).Lsh(new(big.Int).SetUint64(debt), 60))
	o.Snapshot.PilotActive, o.Snapshot.HasPosition = true, true
	o.Snapshot.PositionCollateralRaw, o.Snapshot.PositionCollateralValueRaw = 100_000_000_000, 100_000_000
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw = int64(debt), int64(debt)
	o.Snapshot.SquadsIdleRaw, o.Snapshot.LTVBPS, o.Snapshot.LiquidationThresholdBPS = 0, 4285, 7500
	return o, m, rpc, client, accounts, route
}

// Step 4: the pricer plans one cycle at 1.75x (release -> swap -> partial
// repay of the swap's MINIMUM) before the final release -> payoff, over the
// longer 7+3 window, and the repay never pays the whole debt.
func TestLeverageExitPricerPricesOneCycleAt175x(t *testing.T) {
	o, m, rpc, client, _, route := leverage175Fixture(t)
	_, rows, err := confirmedAccounts(context.Background(), rpc, payoffWindowAddresses(route, route.Kamino.Market), 42)
	if err != nil {
		t.Fatal(err)
	}
	need, err := leverageExitNeedsCycles(context.Background(), rpc, client, m, route, o.Snapshot, rows)
	if err != nil || !need {
		t.Fatalf("1.75x needs cycles: %v %v", need, err)
	}
	legs, post, cash, steps, first, err := priceLeverageExitCycles(context.Background(), rpc, client, m, route, o.Snapshot, rows, 42, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(legs) != 3 || legs[0].Action != DeleverRouteStep || legs[1].Action != SwapCollateralToDebtStep || legs[2].Action != DeleverRouteStep || steps != 10 || cash != 0 || first == nil {
		t.Fatalf("cycle legs %d steps %d cash %d", len(legs), steps, cash)
	}
	repayRequest, repayEffects, _, err := legs[2].Template.decode()
	if err != nil {
		t.Fatal(err)
	}
	swapRequest, _, _, _ := legs[1].Template.decode()
	repay, swap := repayRequest.(KaminoPrimeUSDCRequest), swapRequest.(JupiterSwapRequest)
	if repay.AmountRaw != swap.MinimumOutputRaw || repay.AmountRaw >= first.ObservedDebtRaw || repay.FullPayoff || repayEffects.Repayment == nil || repayEffects.Repayment.MaximumDebitRaw != repay.AmountRaw {
		t.Fatalf("partial repay %d vs swap minimum %d / debt %d", repay.AmountRaw, swap.MinimumOutputRaw, first.ObservedDebtRaw)
	}
	// Post-cycle: less collateral, less debt, lower LTV; one release now pays off.
	before, _ := decodeKaminoObligation(accountAt(rows, route.Kamino.Obligation), route.Kamino)
	after, _ := decodeKaminoObligation(accountAt(post, route.Kamino.Obligation), route.Kamino)
	if after.collateralDepositedRaw >= before.collateralDepositedRaw || after.debtRaw >= before.debtRaw || after.debtRaw == 0 {
		t.Fatalf("projection %+v -> %+v", before, after)
	}
	s := o.Snapshot
	need, err = leverageExitNeedsCycles(context.Background(), rpc, client, m, route, s, post)
	if err != nil || need {
		t.Fatalf("post-cycle still needs cycles: %v %v", need, err)
	}
	// The observed accounts were never mutated.
	orig, _ := decodeKaminoObligation(accountAt(rows, route.Kamino.Obligation), route.Kamino)
	if orig.debtRaw != 42_857_142 {
		t.Fatal("pricer mutated observed accounts")
	}
}

// The admission-level pricer: a NAV at 1.75x reserves the complete
// multi-cycle exit (cycle legs + final release -> swap -> payoff ->
// withdraw -> return), binding the FIRST cycle's payoff window and release
// for build/send revalidation.
func TestLeverageExitAdmissionReservesTheMultiCycleExit(t *testing.T) {
	o, m, rpc, client, _, route := leverage175Fixture(t)
	_, rows, err := confirmedAccounts(context.Background(), rpc, payoffWindowAddresses(route, route.Kamino.Market), 42)
	if err != nil {
		t.Fatal(err)
	}
	d := Decision{Action: ReportNAV, StrategyKey: route.Lane, Reason: "nav_due"}
	nav := BridgeBuildRequest{Action: ReportNAV, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings, Report: BridgeReport{Sequence: 42, ObservedSlot: 42, SnapshotDigest: o.Snapshot.ReportSnapshotDigest}, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
	effects, _, _, err := bridgeExpectedEffects(d, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	effects.Kind, effects.ReturnData = "bridge", expectedAdaptorReturnData(0)
	plan, err, ok := priceLeverageExitFromCurrent(context.Background(), rpc, client, m, o, d, nav, effects, route, rows)
	if !ok || err != nil {
		t.Fatalf("ok=%t err=%v", ok, err)
	}
	var actions []Action
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
	}
	cycleRepays, swaps := 0, 0
	for _, step := range plan.Exit {
		if step.Action == SwapCollateralToDebtStep {
			swaps++
		}
		if step.Action == DeleverRouteStep && step.Template != nil {
			if r, _, _, err := step.Template.decode(); err == nil {
				if k, ok := r.(KaminoPrimeUSDCRequest); ok {
					if _, leg, _ := kaminoPrimeUSDCInstruction(k); leg == kaminoLegRepay && !k.FullPayoff && step.Amount < uint64(o.Snapshot.PositionDebtRaw) && plan.Payoff != nil && step.Amount < plan.Payoff.ObservedDebtRaw {
						cycleRepays++
					}
				}
			}
		}
	}
	if cycleRepays < 1 || swaps < 2 || plan.ExitAfterMicros <= 0 || plan.Payoff == nil || plan.Payoff.ObservedDebtRaw != 42_857_142 || plan.BorrowRelease == nil || plan.PayoffRepayment == nil || plan.PayoffWithdrawal == nil {
		t.Fatalf("multi-cycle exit incomplete: %v payoff %+v", actions, plan.Payoff)
	}
	var total int64
	for _, step := range plan.Exit {
		total += step.Cost.TotalMicros
	}
	if total != plan.ExitAfterMicros {
		t.Fatal("exit cost total")
	}
	// A 1.5x position keeps the installed single-release pricing.
	o15, m15, rpc15, client15, accounts15, _ := leverage175Fixture(t)
	putScaledFraction(accountAt(accounts15, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(33_333_333), 60))
	o15.Snapshot.PositionDebtRaw, o15.Snapshot.PositionDebtValueRaw, o15.Snapshot.LTVBPS = 33_333_333, 33_333_333, 3333
	_, rows15, _ := confirmedAccounts(context.Background(), rpc15, payoffWindowAddresses(route, route.Kamino.Market), 42)
	if need, err := leverageExitNeedsCycles(context.Background(), rpc15, client15, m15, route, o15.Snapshot, rows15); err != nil || need {
		t.Fatalf("1.5x needs cycles: %v %v", need, err)
	}
}

// Every Kamino wire of an exit cycle (release and partial repay, both over
// {collateral, debt}) is an existing topology: compiled by the real
// compiler, it passes the persisted-wire gate on AUTO and OnRe.
func TestExitCycleKaminoWiresPassThePersistedWireGate(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{81}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	for lane, manifest := range map[string]RouteManifest{autoAUTOPYUSD.Lane: autoFixtureManifest(t), onreONycUSDC: basicPolicyFixtureManifest(t)} {
		route, _ := runtimeRoute(lane)
		for _, leg := range []kaminoPrimeUSDCLeg{kaminoLegWithdraw, kaminoLegRepay} {
			request, err := manifest.kaminoPacketForRoute(DeleverRouteStep, leg, 386_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, lane)
			if err != nil {
				t.Fatal(err)
			}
			request.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
			message, err := manifest.compileKaminoMessage(request, delegate)
			if err != nil {
				t.Fatal(err)
			}
			if err := signedTestBuildResult(t, key, message).validateForDelegate(delegate); err != nil {
				t.Fatalf("%s leg %d: %v", lane, leg, err)
			}
		}
	}
}

// Review: a 1.5x release/NAV admission never needs Jupiter for the cycle
// check. The live-shaped 1.5x AUTO position (2463.48 AUTO @ 1.0209, 837.77
// PYUSD) and the OnRe fixture at 1.5x take the installed path even when the
// Jupiter client fails.
func TestLeverageExitPreCheckSkipsQuotesAt15x(t *testing.T) {
	broken, _ := newJupiterClient("https://jupiter.invalid", nil)
	live := base()
	live.RouteLane, live.StrategyKey, live.PilotActive, live.HasPosition = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, true, true
	live.PositionCollateralRaw, live.PositionCollateralValueRaw = 2_463_480_000, 2_514_967_000
	live.PositionDebtRaw, live.PositionDebtValueRaw = 837_770_000, 837_770_000
	if leverageExitMayNeedCycles(live) {
		t.Fatal("live 1.5x flagged for cycles")
	}
	if need, err := leverageExitNeedsCycles(context.Background(), nil, broken, RouteManifest{}, autoAUTOPYUSD, live, nil); need || err != nil {
		t.Fatalf("1.5x touched RPC/Jupiter: %v %v", need, err)
	}
	o, m, rpc, _, accounts, route := leverage175Fixture(t)
	putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(33_333_333), 60))
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.LTVBPS = 33_333_333, 33_333_333, 3333
	_, rows, _ := confirmedAccounts(context.Background(), rpc, payoffWindowAddresses(route, route.Kamino.Market), 42)
	d := Decision{Action: ReportNAV, StrategyKey: route.Lane, Reason: "nav_due"}
	if _, err, ok := priceLeverageExitFromCurrent(context.Background(), rpc, broken, m, o, d, BridgeBuildRequest{}, ExpectedEffects{}, route, rows); ok || err != nil {
		t.Fatalf("1.5x NAV left the installed path: ok=%t err=%v", ok, err)
	}
	if _, err, ok := priceLeverageExitAfterRelease(context.Background(), rpc, broken, m, o, d, KaminoPrimeUSDCRequest{RouteLane: route.Lane}, ExpectedEffects{}, KaminoReleaseBound{}, rows); ok || err != nil {
		t.Fatalf("1.5x release left the installed path: ok=%t err=%v", ok, err)
	}
	if leverageExitAccountsMayNeedCycles(rows, route, o.Snapshot) {
		t.Fatal("1.5x accounts flagged for cycles")
	}
	// 1.75x still takes the quote check, on snapshot and on accounts.
	s := leverageSnapshot(1.75)
	if !leverageExitMayNeedCycles(s) {
		t.Fatal("1.75x skipped the cycle check")
	}
	o175, _, rpc175, _, _, _ := leverage175Fixture(t)
	_, rows175, _ := confirmedAccounts(context.Background(), rpc175, payoffWindowAddresses(route, route.Kamino.Market), 42)
	if !leverageExitAccountsMayNeedCycles(rows175, route, o175.Snapshot) {
		t.Fatal("1.75x accounts skipped the cycle check")
	}
}

// Step 5 admission: the sized 1.75x -> 1.5x release is priced from its
// poststate (complete remaining exit), never as a payoff funding.
func TestDownPartialReleaseAdmissionPricesFromItsPoststate(t *testing.T) {
	o, m, rpc, client, _, route := leverage175Fixture(t)
	o.Snapshot.LeverageTargetLevel = 1.5
	_, rows, _ := confirmedAccounts(context.Background(), rpc, payoffWindowAddresses(route, route.Kamino.Market), 42)
	bound, err := m.decodeKaminoRepaymentReleaseForMode(rows, route, 42, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, receipts, ok := leverageDownPartialStepAt(o.Snapshot, true)
	if !ok || uint64(receipts) > bound.ReceiptRaw {
		t.Fatalf("partial release %d vs safe %d", receipts, bound.ReceiptRaw)
	}
	d := Decision{Action: DeleverRouteStep, StrategyKey: route.Lane, Reason: leverageDownPartialReleaseReason, AmountRaw: receipts}
	r, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, uint64(receipts), LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	r.RepaymentRelease, r.PilotRepaymentRelease = true, true
	reserve, _ := decodeKaminoReserve(accountAt(rows, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	liquidity, _ := reserve.redeemLiquidityRaw(uint64(receipts))
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	effects, err := exactKaminoTokenEffects(rows, source, destination, liquidity)
	if err != nil {
		t.Fatal(err)
	}
	plan, err, ok := priceLeverageExitAfterRelease(context.Background(), rpc, client, m, o, d, r, effects, bound, rows)
	if !ok || err != nil || plan.ExitAfterMicros <= 0 || plan.PayoffRepayment == nil || plan.Payoff == nil {
		t.Fatalf("ok=%t err=%v plan=%+v", ok, err, plan.Payoff)
	}
}

// Partial withdrawal: the leveraged release leg is admitted with the
// complete exit of the remaining position priced from its poststate; the
// debt-free position prices its withdraw -> swap -> return tail.
func TestPartialWithdrawalReleaseAdmission(t *testing.T) {
	for _, level := range []string{"1.5x", "1x"} {
		o, m, rpc, client, accounts, route := leverage175Fixture(t)
		debt := uint64(33_333_333)
		if level == "1x" {
			debt = 0
		}
		putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(debt), 60))
		o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.LTVBPS = int64(debt), int64(debt), int64(debt*10_000/100_000_000)
		o.Snapshot.LeverageTargetLevel = 1.5
		if debt == 0 {
			o.Snapshot.LeverageTargetLevel = 1
		}
		// $10 of the fixture's $66.67 (1.5x) / $100 (1x) equity; the
		// remainder stays above the $50 minimum.
		o.Snapshot.WithdrawalDemandRaw, o.Snapshot.VoltrIdleRaw, o.Snapshot.PayoffDebtRaw = 10_000_000, 0, int64(debt)+100
		// The fixture's snapshot equity (value units): C $100 - D.
		o.Snapshot.StrategyNAVRaw, o.Snapshot.TotalVaultNAVRaw = 100_000_000-int64(debt), 100_000_000-int64(debt)
		o.Snapshot.CollateralIdleValueRaw = 0
		d := Decide(o.Snapshot)
		if d.Reason != partialReleaseReason {
			t.Fatalf("%s: %+v", level, d)
		}
		r, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, uint64(d.AmountRaw), LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		r.ObligationReserves = []string{route.Kamino.CollateralReserve}
		if debt > 0 {
			r.ObligationReserves = append(r.ObligationReserves, route.Kamino.DebtReserve)
			r.RepaymentRelease, r.PilotRepaymentRelease = true, true
		}
		reserve, _ := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
		liquidity, _ := reserve.redeemLiquidityRaw(uint64(d.AmountRaw))
		source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
		effects, err := exactKaminoTokenEffects(accounts, source, destination, liquidity)
		if err != nil {
			t.Fatal(err)
		}
		plan, err, ok := admitPartialWithdrawalLeg(context.Background(), rpc, client, m, o, d, r, effects)
		if !ok || err != nil || plan.ExitAfterMicros <= 0 || plan.PayoffWithdrawal == nil || len(plan.Exit) == 0 {
			t.Fatalf("%s: ok=%t err=%v", level, ok, err)
		}
		if debt > 0 && plan.PayoffRepayment == nil {
			t.Fatalf("%s: remaining debt payoff not priced", level)
		}
		// A changed decision is refused.
		bad := d
		bad.AmountRaw++
		if _, err, ok := admitPartialWithdrawalLeg(context.Background(), rpc, client, m, o, bad, r, effects); !ok || err == nil {
			t.Fatalf("%s: drifted decision admitted", level)
		}
	}
}

// Every Kamino wire of a partial withdrawal is an existing topology: the
// leveraged release ({collateral, debt}), the debt-free withdraw
// ({collateral}) and the partial repay ({collateral, debt}), compiled by the
// real compiler, pass the persisted-wire gate on AUTO and OnRe.
func TestPartialWithdrawalKaminoWiresPassThePersistedWireGate(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{91}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	for lane, manifest := range map[string]RouteManifest{autoAUTOPYUSD.Lane: autoFixtureManifest(t), onreONycUSDC: basicPolicyFixtureManifest(t)} {
		route, _ := runtimeRoute(lane)
		both := []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		for _, c := range []struct {
			leg      kaminoPrimeUSDCLeg
			reserves []string
		}{{kaminoLegWithdraw, both}, {kaminoLegWithdraw, []string{route.Kamino.CollateralReserve}}, {kaminoLegRepay, both}} {
			request, err := manifest.kaminoPacketForRoute(DeleverRouteStep, c.leg, 176_750_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, lane)
			if err != nil {
				t.Fatal(err)
			}
			request.ObligationReserves = c.reserves
			message, err := manifest.compileKaminoMessage(request, delegate)
			if err != nil {
				t.Fatal(err)
			}
			if err := signedTestBuildResult(t, key, message).validateForDelegate(delegate); err != nil {
				t.Fatalf("%s leg %d %v: %v", lane, c.leg, c.reserves, err)
			}
		}
	}
}

// The collateral->USDC swap leg of a partial withdrawal (after the release)
// is admitted with the remaining position's complete exit priced.
func TestPartialWithdrawalSwapLegAdmission(t *testing.T) {
	o, m, rpc, client, accounts, route := leverage175Fixture(t)
	debt := uint64(33_333_333)
	putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(debt), 60))
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.Obligation).Data[128:136], 85_000_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 15_000_000_000)
	o.Snapshot.PositionCollateralRaw, o.Snapshot.PositionCollateralValueRaw = 85_000_000_000, 85_000_000
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.PayoffDebtRaw, o.Snapshot.LTVBPS = int64(debt), int64(debt), int64(debt)+100, 3921
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.CollateralIdleValueRaw = 15_000_000_000, 15_000_000_000, 15_000_000
	o.Snapshot.LeverageTargetLevel, o.Snapshot.WithdrawalDemandRaw = 1.5, 10_000_000
	d := Decide(o.Snapshot)
	if d.Reason != partialSwapToUSDCReason || d.Action != SwapCollateralToStableStep {
		t.Fatalf("decision %+v", d)
	}
	e, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, uint64(d.AmountRaw), 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	plan, err, ok := admitPartialWithdrawalLeg(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects)
	if !ok || err != nil || plan.ExitAfterMicros <= 0 || plan.PayoffRepayment == nil {
		t.Fatalf("ok=%t err=%v", ok, err)
	}
}

func partialWithdrawalRestoreFixture(t *testing.T, lane string, debt int64) (Observation, Decision, BridgeExecutionEvidence, RouteManifest, *chain.Client, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, m, rpc, client, accounts, route := leverage175Fixture(t)
	if lane == autoAUTOPYUSD.Lane {
		m, route, accounts = autoObservationBatch(t, 42, func(batch []ConfirmedAccount) {
			autoSourceReleasedBatch(autoAUTOPYUSD)(batch)
			// Scale the coherent 2:1 reserve and collateral position together:
			// the retained AUTO equity remains above the partial-flow minimum.
			reserve := accountAt(batch, autoAUTOPYUSD.Kamino.CollateralReserve).Data
			binary.LittleEndian.PutUint64(reserve[224:232], 3*autoFixturePoolLiquidity)
			binary.LittleEndian.PutUint64(reserve[2592:2600], 3*autoFixturePoolReceiptSupply)
			binary.LittleEndian.PutUint64(accountAt(batch, autoAUTOPYUSD.CollateralLiquiditySupply).Data[64:72], 3*autoFixturePoolLiquidity)
			obligation := kaminoObligationImage(t, autoAUTOPYUSD, 42, 3*autoFixtureDepositReceiptRaw, uint64(debt))
			copy(accountAt(batch, autoAUTOPYUSD.Kamino.Obligation).Data, obligation.Data)
			binary.LittleEndian.PutUint64(accountAt(batch, bridgeSquadsATA).Data[64:72], 0)
			binary.LittleEndian.PutUint64(accountAt(batch, bridgeIdleATA).Data[64:72], 0)
			binary.LittleEndian.PutUint64(accountAt(batch, bridgeStrategyATA).Data[64:72], 11_000_000)
		})
		installed := installedAutoFixtureBinding(t)
		m.RuntimeBindings.AutoPolicy = &installed
		upsertConfirmedAccount(&accounts, installedAutoPolicyAccount(t))
		// The last report still includes the $11 waiting in staged custody;
		// staging has not yet changed Voltr's book or consumed its ticket.
		upsertConfirmedAccount(&accounts, strategyReceiptFixture(t, 94_000_000))
		upsertConfirmedAccount(&accounts, voltrVaultFixture(t, 94_000_000))
		binary.LittleEndian.PutUint16(accountAt(accounts, bridgeVoltrVault).Data[514:516], uint16(approvedAdminPerformanceFeeBPS))
		upsertConfirmedAccount(&accounts, exactReportTicketAccount(t, 40))
		var err error
		o, _, err = autoObservationForAccounts(m, 42, accounts)(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		o.Snapshot.PilotActive = true
		// Controlled counterparts of journal/identity enrichment; keep all
		// production monitors armed rather than disabling their checks.
		o.Snapshot.JournalSequenceKnown, o.Snapshot.JournalReconciledSequenceRaw = true, 40
		o.Snapshot.JournalArmedNAVKnown, o.Snapshot.JournalArmedNAVRaw = true, o.Snapshot.PriorReportedNAVRaw
		o.Snapshot.CapitalMutated, o.Snapshot.ProgramIdentityKnown = true, true
		o.Snapshot.VoltrProgramDeploySlot, o.Snapshot.AdaptorProgramDeploySlot = voltrProgramDeploySlot, adaptorProgramDeploySlot
		rpc, client = autoCleanupRPC(t, 42, accounts), autoCandidateJupiter(t, route)
	}
	putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(debt), 60))
	s := &o.Snapshot
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw = debt, debt, debt+100
	s.LTVBPS, s.LeverageTargetLevel = debt*10_000/s.PositionCollateralValueRaw, 1.5
	s.PartialWithdrawalOperationID, s.PartialWithdrawalLTVBPS = strings.Repeat("a", 64), s.LTVBPS
	s.WithdrawalDemandRaw, s.VoltrIdleRaw = 10_000_000, 0
	s.VoltrStrategyIdleRaw, s.StagedAmountRaw, s.StagedAmountKnown, s.StageTransient = 11_000_000, 11_000_000, true, true
	s.CollateralIdleRaw, s.PrimeIdleRaw, s.DebtIdleRaw, s.SquadsIdleRaw = 0, 0, 0, 0
	s.StrategyNAVRaw, s.TotalVaultNAVRaw = s.PositionCollateralValueRaw-debt, s.PositionCollateralValueRaw-debt
	d := m.DecideOnManifest(*s)
	if d.Action != VoltrRestoreIdle || d.Reason != "withdrawal_staged" {
		t.Fatalf("restore decision: %+v", d)
	}
	r := bridgeTestRequest(d.Action, uint64(d.AmountRaw))
	r.Report = BridgeReport{Sequence: uint64(s.Slot), ObservedSlot: uint64(s.Slot), NAVAfterRaw: uint64(s.StrategyNAVRaw), SnapshotDigest: s.ReportSnapshotDigest}
	effects, _, _, err := bridgeExpectedEffects(d, uint64(s.VoltrIdleRaw), uint64(s.VoltrStrategyIdleRaw), uint64(s.SquadsIdleRaw))
	if err != nil {
		t.Fatal(err)
	}
	effects.Kind, effects.ReturnData = "bridge", expectedAdaptorReturnData(r.Report.NAVAfterRaw)
	return o, d, BridgeExecutionEvidence{r, effects}, m, rpc, client, accounts
}

// A partial stage is not pooled liquidity until the separately admitted restore.
// Exercise both the retained debt and retained debt-free collateral paths.
func TestPartialWithdrawalRestorePreservesPositionAndAdmission(t *testing.T) {
	for _, tc := range []struct {
		lane string
		debt int64
	}{{onreONycUSDC, 33_333_333}, {onreONycUSDC, 0}, {autoAUTOPYUSD.Lane, int64(autoFixtureDebtRaw)}} {
		t.Run(fmt.Sprintf("%s/%d", tc.lane, tc.debt), func(t *testing.T) {
			debt := tc.debt
			o, d, evidence, m, rpc, client, accounts := partialWithdrawalRestoreFixture(t, tc.lane, debt)
			route, _ := runtimeRoute(o.Snapshot.RouteLane)
			s, r, effects := &o.Snapshot, evidence.Request, evidence.ExpectedEffects
			before := append([]byte(nil), accountAt(accounts, route.Kamino.Obligation).Data...)
			plan, err, ok := admitPartialWithdrawalLeg(context.Background(), rpc, client, m, o, d, r, effects)
			if !ok || err != nil {
				t.Fatalf("restore not admitted: recognized=%t err=%v", ok, err)
			}
			current, currentEffects, _, err := plan.Input.decodeWithManifest(m)
			if err != nil || current != r || !reflect.DeepEqual(currentEffects, effects) || plan.Snapshot != *s {
				t.Fatalf("current restore or snapshot changed: %v", err)
			}
			if plan.ExitAfterMicros <= 0 || plan.PayoffWithdrawal == nil || (debt > 0 && plan.PayoffRepayment == nil) {
				t.Fatal("remaining position exit reserve was dropped")
			}
			if plan.ValidThroughSlot > s.Slot+adaptorMaxReportAgeSlots {
				t.Fatal("restore extended the adaptor report window")
			}
			if !bytes.Equal(before, accountAt(accounts, route.Kamino.Obligation).Data) {
				t.Fatal("restore mutated position accounts")
			}
			for _, e := range currentEffects.Accounts {
				switch e.Address {
				case bridgeStrategyATA:
					if e.BeforeRaw != uint64(s.StagedAmountRaw) || e.AfterRaw != 0 {
						t.Fatal("restore did not sweep exact staged custody")
					}
				case bridgeIdleATA:
					if e.AfterRaw-e.BeforeRaw != uint64(s.StagedAmountRaw) {
						t.Fatal("restore did not fund pooled idle")
					}
				case bridgeSquadsATA:
					if e.BeforeRaw != e.AfterRaw {
						t.Fatal("restore changed Squads cash")
					}
				default:
					t.Fatalf("restore touched position account %s", e.Address)
				}
			}
			// Run the production dispatcher, not a test copy of its branches.
			// All HTTP is controlled; a missing database stops before persistence.
			oldTransport := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "127.0.0.1" { // the fake chain node
					return oldTransport.RoundTrip(r)
				}
				request := r.Clone(r.Context())
				request.URL.Path = strings.TrimPrefix(request.URL.Path, "/swap/v1")
				return client.http.Transport.RoundTrip(request)
			})
			t.Cleanup(func() { http.DefaultTransport = oldTransport })
			runtime := productionTickRuntime(&Database{}, rpc, m, Credentials{})
			assertBudgetHold(t, runtime.admitBridge(context.Background(), "restore", o, d, BridgeExecutionEvidence{r, effects}), "bridge_admission_database_unavailable")
			// The cash-only guard remains closed to retained exposure.
			_, err = phase3BridgeTemplates(*s, d, BridgeExecutionEvidence{r, effects})
			assertBudgetHold(t, err, "complete_position_exit_admission_unavailable")
			badRequest := r
			badRequest.Report.NAVAfterRaw = 0
			_, err, _ = admitPartialWithdrawalLeg(context.Background(), nil, nil, m, o, d, badRequest, effects)
			assertBudgetHold(t, err, "partial_withdrawal_restore_mismatch")
			badRequest = r
			badRequest.AmountRaw--
			_, err, _ = admitPartialWithdrawalLeg(context.Background(), nil, nil, m, o, d, badRequest, effects)
			assertBudgetHold(t, err, "partial_withdrawal_restore_mismatch")
			badEffects := effects
			badEffects.Accounts = append([]ExpectedAccountEffect(nil), effects.Accounts...)
			badEffects.Accounts[0].AfterRaw++
			_, err, _ = admitPartialWithdrawalLeg(context.Background(), nil, nil, m, o, d, r, badEffects)
			assertBudgetHold(t, err, "partial_withdrawal_restore_mismatch")
			for name, mutate := range map[string]func(*Observation){
				"unknown stage":    func(o *Observation) { o.Snapshot.StagedAmountKnown = false },
				"mismatched stage": func(o *Observation) { o.Snapshot.StagedAmountRaw-- },
				"missing origin":   func(o *Observation) { o.Snapshot.PartialWithdrawalOperationID = "" },
			} {
				t.Run(name, func(t *testing.T) {
					bad := o
					mutate(&bad)
					_, err, ok := admitPartialWithdrawalLeg(context.Background(), nil, nil, m, bad, d, r, effects)
					if !ok || err == nil {
						t.Fatal("unproven staged restore accepted")
					}
				})
			}
		})
	}
}

// The real production admission must preserve the position's reserve and bind
// only the restore wire. Fixture rows represent already reconciled local history;
// no signer, chain simulation, or transaction submission is involved.
func TestPartialWithdrawalRestoreProductionAdmissionDB(t *testing.T) {
	for _, tc := range []struct {
		lane string
		debt int64
	}{{onreONycUSDC, 33_333_333}, {onreONycUSDC, 0}, {autoAUTOPYUSD.Lane, int64(autoFixtureDebtRaw)}} {
		t.Run(fmt.Sprintf("%s/%d", tc.lane, tc.debt), func(t *testing.T) {
			debt := tc.debt
			ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 60*time.Second)
			defer cancel()
			defer db.Close()
			o, d, evidence, m, rpc, client, accounts := partialWithdrawalRestoreFixture(t, tc.lane, debt)
			key := fmt.Sprintf("partial-restore-%d", time.Now().UnixNano())
			id, originID := key+"-restore", sha256Bytes([]byte(key+"-release"))
			o.Snapshot.PartialWithdrawalOperationID = originID
			d = m.DecideOnManifest(o.Snapshot)
			partial := partialWithdrawalState{Lane: d.StrategyKey, OperationID: originID, Generation: 1, LTVBPS: o.Snapshot.PartialWithdrawalLTVBPS}
			stateValue := planningPilotState(t)
			budget := stateValue["phase3"].(Phase3Budget)
			family := phase3BudgetFamilyForLane(d.StrategyKey)
			beforeReserve := int64(1_000_000_000)
			row := budget.Families[family]
			row.ExitMicros = beforeReserve
			budget.Families[family] = row
			stateValue["phase3"], stateValue["partialWithdrawal"] = budget, partial
			state, _ := json.Marshal(stateValue)
			if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2::jsonb,2)`, key, string(state)); err != nil {
				t.Fatal(err)
			}
			defer db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key)
			defer db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key=$1`, key)
			if _, err := db.AcquireRouteLease(ctx, key, "partial-restore-test", time.Minute); err != nil {
				t.Fatal(err)
			}
			insert := func(id string, action Action, reason, status string, slot int64, amount int64) {
				t.Helper()
				decision := d
				decision.Action, decision.Reason, decision.AmountRaw = action, reason, amount
				observed := o
				if status == "reconciled" {
					observed.Snapshot.Slot = slot
				}
				values := map[string]any{"decision": newDecisionEvidence(observed, decision, m.SHA256, *m.PolicyCatalog.SHA256), "partialWithdrawal": partial}
				if action == ReportNAV {
					values["expectedEffects"] = ExpectedEffects{ReturnData: expectedAdaptorReturnData(uint64(o.Snapshot.PriorReportedNAVRaw))}
				}
				envelope, _ := json.Marshal(values)
				if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,confirmed_slot,expected_effects) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb)`, id, key, status, action, d.StrategyKey, slot, string(envelope)); err != nil {
					t.Fatal(err)
				}
			}
			insert(originID, DeleverRouteStep, partialReleaseReason, "reconciled", 39, 11_000_000)
			insert(key+"-nav", ReportNAV, "post_mutation_nav_due", "reconciled", 40, 0)
			insert(key+"-stage", StageSquadsToVoltr, partialStageReason, "reconciled", 41, d.AmountRaw)
			origin, err := db.LoadPartialWithdrawal(ctx, key)
			if err != nil || origin == nil || *origin != partial {
				t.Fatalf("invalid partial origin: %v", err)
			}
			journal, err := db.ReconciledBridgeJournal(ctx, key)
			if err != nil || !journal.StagedAmountKnown || !journal.StageAfterTicket || journal.StagedAmountRaw != d.AmountRaw {
				t.Fatalf("invalid stage provenance: %+v %v", journal, err)
			}
			o.Snapshot.StagedAmountKnown, o.Snapshot.StagedAmountRaw, o.Snapshot.StageTransient = journal.StagedAmountKnown, journal.StagedAmountRaw, journal.StageAfterTicket
			insert(id, d.Action, d.Reason, "decided", 0, d.AmountRaw)
			oldTransport := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "127.0.0.1" { // the fake chain node
					return oldTransport.RoundTrip(r)
				}
				request := r.Clone(r.Context())
				request.URL.Path = strings.TrimPrefix(request.URL.Path, "/swap/v1")
				return client.http.Transport.RoundTrip(request)
			})
			t.Cleanup(func() { http.DefaultTransport = oldTransport })
			runtime := productionTickRuntime(db, rpc, m, Credentials{})
			readState := func() (string, string) {
				t.Helper()
				var state, operation string
				if err := db.pool.QueryRow(ctx, `SELECT s.state::text,o.expected_effects::text FROM loyal_yield.multiply_route_states s JOIN loyal_yield.multiply_operations o USING(route_key) WHERE o.operation_id=$1`, id).Scan(&state, &operation); err != nil {
					t.Fatal(err)
				}
				return state, operation
			}
			// A valid snapshot cannot substitute for the durable origin binding.
			if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=expected_effects-'partialWithdrawal' WHERE operation_id=$1`, id); err != nil {
				t.Fatal(err)
			}
			beforeState, beforeOperation := readState()
			assertBudgetHold(t, runtime.admitBridge(ctx, id, o, d, evidence), "partial_withdrawal_state_changed")
			if afterState, afterOperation := readState(); beforeState != afterState || beforeOperation != afterOperation {
				t.Fatal("bad provenance mutated admission or budget")
			}
			encodedPartial, _ := json.Marshal(partial)
			if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(expected_effects,'{partialWithdrawal}',$2::jsonb) WHERE operation_id=$1`, id, string(encodedPartial)); err != nil {
				t.Fatal(err)
			}
			beforeState, beforeOperation = readState()
			for _, known := range []bool{false, true} {
				bad := o
				bad.Snapshot.StagedAmountKnown = known
				if known {
					bad.Snapshot.StagedAmountRaw--
				}
				assertBudgetHold(t, runtime.admitBridge(ctx, id, bad, d, evidence), "partial_withdrawal_decision_changed")
			}
			if afterState, afterOperation := readState(); beforeState != afterState || beforeOperation != afterOperation {
				t.Fatal("bad stage mutated admission or budget")
			}
			route, _ := runtimeRoute(d.StrategyKey)
			positionBefore := append([]byte(nil), accountAt(accounts, route.Kamino.Obligation).Data...)
			if err = runtime.admitBridge(ctx, id, o, d, evidence); err != nil {
				t.Fatal(err)
			}
			var rawAuth, rawBudget []byte
			var hasWire, hasSend, hasDebtClear bool
			if err = db.pool.QueryRow(ctx, `SELECT o.expected_effects->'phase3',s.state->'phase3',o.signed_wire IS NOT NULL,o.broadcast_intent_at IS NOT NULL,s.state ? 'debtClearAuthority' FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states s USING(route_key) WHERE operation_id=$1`, id).Scan(&rawAuth, &rawBudget, &hasWire, &hasSend, &hasDebtClear); err != nil {
				t.Fatal(err)
			}
			var auth phase3OperationAuthorization
			var afterBudget Phase3Budget
			if json.Unmarshal(rawAuth, &auth) != nil || json.Unmarshal(rawBudget, &afterBudget) != nil || auth.BridgeAdmission == nil || auth.BuildInput == nil {
				t.Fatal("missing durable admission")
			}
			current, effects, _, err := auth.BuildInput.decodeWithManifest(m)
			if err != nil || current != evidence.Request || !reflect.DeepEqual(effects, evidence.ExpectedEffects) || auth.DebtClear != nil || hasWire || hasSend || hasDebtClear {
				t.Fatalf("admission changed current restore or granted debt-clear/send authority: %v", err)
			}
			plan, reservation := auth.BridgeAdmission, afterBudget.Reservations[id]
			if plan.Snapshot != o.Snapshot || plan.ExitAfterMicros <= 0 || plan.PayoffWithdrawal == nil || (debt > 0 && plan.PayoffRepayment == nil) || !bytes.Equal(positionBefore, accountAt(accounts, route.Kamino.Obligation).Data) {
				t.Fatal("remaining position or its reserved return changed")
			}
			if !reservation.Recovery || reservation.ExitBeforeMicros != beforeReserve || reservation.ExitAfterMicros != max(plan.ExitAfterMicros, beforeReserve-plan.CurrentCost.TotalMicros) || afterBudget.Families[family].ExitMicros != reservation.ExitAfterMicros {
				t.Fatal("remaining-position reserve was not retained")
			}
			if afterBudget.deploymentLimits() != budget.deploymentLimits() || afterBudget.Pilot == nil || *afterBudget.Pilot != *budget.Pilot {
				t.Fatal("restore widened limits or changed pilot authority")
			}
			if evidence.Request.Report.NAVAfterRaw == 0 || evidence.Request.Report.NAVAfterRaw != uint64(o.Snapshot.StrategyNAVRaw) {
				t.Fatal("retained position NAV lost")
			}
		})
	}
}
