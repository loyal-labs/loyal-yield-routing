package backyardrwa

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"math/big"
	"testing"
)

// leverage175Accounts turns the controlled OnRe fixture into a 1.75x
// position: collateral value C, debt D = (1.75-1)/1.75 * C (LTV 42.9%).
// Fixture prices: ONyc (9 dp) $1, USDC debt (6 dp) $2 per raw-unit math, so
// debt raw = collateral value in USDC-6 / 2.
func leverage175Fixture(t *testing.T) (Observation, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount, RuntimeRoute) {
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
	_, rows, err := rpc.GetMultipleAccounts(context.Background(), payoffWindowAddresses(route, route.Kamino.Market), 42)
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
	_, rows, err := rpc.GetMultipleAccounts(context.Background(), payoffWindowAddresses(route, route.Kamino.Market), 42)
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
	_, rows15, _ := rpc15.GetMultipleAccounts(context.Background(), payoffWindowAddresses(route, route.Kamino.Market), 42)
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
	_, rows, _ := rpc.GetMultipleAccounts(context.Background(), payoffWindowAddresses(route, route.Kamino.Market), 42)
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
	_, rows175, _ := rpc175.GetMultipleAccounts(context.Background(), payoffWindowAddresses(route, route.Kamino.Market), 42)
	if !leverageExitAccountsMayNeedCycles(rows175, route, o175.Snapshot) {
		t.Fatal("1.75x accounts skipped the cycle check")
	}
}

// Step 5 admission: the sized 1.75x -> 1.5x release is priced from its
// poststate (complete remaining exit), never as a payoff funding.
func TestDownPartialReleaseAdmissionPricesFromItsPoststate(t *testing.T) {
	o, m, rpc, client, _, route := leverage175Fixture(t)
	o.Snapshot.LeverageTargetLevel = 1.5
	_, rows, _ := rpc.GetMultipleAccounts(context.Background(), payoffWindowAddresses(route, route.Kamino.Market), 42)
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
