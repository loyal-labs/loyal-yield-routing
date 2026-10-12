package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"math/big"
	"reflect"
	"testing"
	"time"
)

// blockDebtUtilization puts the lane's debt reserve at its utilization
// limit: 90% configured, 95% borrowed, so KLend refuses new borrowing.
func blockDebtUtilization(t *testing.T, lane string) func([]ConfirmedAccount) {
	return func(accounts []ConfirmedAccount) {
		route, _ := runtimeRoute(lane)
		for i := range accounts {
			if accounts[i].Address == route.Kamino.DebtReserve {
				d := accounts[i].Data
				d[kaminoReserveConfigOffset+645] = 90
				putScaledFraction(d[232:248], new(big.Int).Lsh(big.NewInt(19_000_000_000), 60))
			}
		}
	}
}

func selectorRecipeActions(t *testing.T, q selectorDestinationQuote) []Action {
	t.Helper()
	var actions []Action
	for _, input := range q.Recipe.Inputs {
		r, _, _, err := input.decode()
		if err != nil {
			t.Fatal(err)
		}
		switch r := r.(type) {
		case BridgeBuildRequest:
			actions = append(actions, r.Action)
		case JupiterSwapRequest:
			actions = append(actions, r.Action)
		case KaminoPrimeUSDCRequest:
			actions = append(actions, r.Action)
		}
	}
	return actions
}

// (a) A blocked OnRe debt reserve prices a debt-free 1x entry: allocate ->
// swap -> deposit, exit = withdraw -> swap back -> stage -> restore. No
// borrow, no leverage swap, no redeposit.
func TestBlockedDestinationPricesAnUnleveredEntry(t *testing.T) {
	t.Parallel()
	m, rpc, client, _ := selectorDestinationFixtureForLane(t, onreONycUSDC, blockDebtUtilization(t, onreONycUSDC))
	q, err := observeSelectorDestinationForecast(context.Background(), rpc, fixtureView(t, rpc), client, m, capturedTestPolicies(), leveredTestMarket(onreONycUSDC), 1_000_000, 42, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{VoltrAllocateToSquads, ReportNAV, SwapStableToCollateralStep, ReportNAV, OpenRouteStep, ReportNAV,
		DeleverRouteStep, ReportNAV, SwapCollateralToStableStep, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV}
	if got := selectorRecipeActions(t, q); !reflect.DeepEqual(got, want) {
		t.Fatalf("1x recipe %v", got)
	}
	if !q.Unlevered || q.BorrowReceiveRaw != 0 || q.BorrowFeeRaw != 0 || q.PayoffUpperRaw != 0 || q.Recipe.CostRaw <= 0 || q.Recipe.CostRaw >= 1_000_000 || q.PayoffReturnAmountRaw == 0 {
		t.Fatalf("1x quote %+v", q)
	}
	// Every Kamino leg keeps an existing wire topology: flat deposit, and a
	// collateral-only withdrawal.
	for _, input := range q.Recipe.Inputs {
		r, _, _, _ := input.decode()
		if k, ok := r.(KaminoPrimeUSDCRequest); ok {
			_, leg, _ := kaminoPrimeUSDCInstruction(k)
			if (leg == kaminoLegDeposit && len(k.ObligationReserves) != 0) || (leg == kaminoLegWithdraw && len(k.ObligationReserves) != 1) || leg == kaminoLegBorrow || leg == kaminoLegRepay {
				t.Fatalf("unexpected Kamino leg %d reserves %v", leg, k.ObligationReserves)
			}
		}
	}
}

// (c) Not blocked: the same lane keeps the leveraged entry exactly; blocked
// Maple, a B2 lane since the registry, takes the 1x path like OnRe.
func TestUnblockedLaneKeepsTheLeveragedEntryAndBlockedMapleTakes1x(t *testing.T) {
	t.Parallel()
	m, rpc, client, _ := selectorDestinationFixtureForLane(t, onreONycUSDC, nil)
	q, err := observeSelectorDestinationForecast(context.Background(), rpc, fixtureView(t, rpc), client, m, capturedTestPolicies(), leveredTestMarket(onreONycUSDC), 100_000_000, 42, true)
	if err != nil {
		t.Fatal(err)
	}
	if q.Unlevered || q.BorrowReceiveRaw == 0 {
		t.Fatalf("unblocked OnRe lost its leveraged entry: %+v", q)
	}
	m, rpc, client, _ = selectorDestinationFixtureForLane(t, SelectedRouteID, blockDebtUtilization(t, SelectedRouteID))
	q, err = observeSelectorDestinationForecast(context.Background(), rpc, fixtureView(t, rpc), client, m, capturedTestPolicies(), leveredTestMarket(SelectedRouteID), 1_000_000, 42, true)
	if err != nil || !q.Unlevered || q.BorrowReceiveRaw != 0 {
		t.Fatalf("blocked Maple did not price the 1x entry: %+v %v", q, err)
	}
}

// Live shape 2026-09-29: funded debt-free AUTO at 1x (~$1,677, borrowing
// blocked), OnRe at 11.02% vs AUTO 9.48% at 1x, OnRe borrowing blocked too.
func unleveredSwitchFixture(cost int64) SelectorInput { return unleveredSwitchFixtureAt(cost, .1102) }

func unleveredSwitchFixtureAt(cost int64, onreAPY float64) SelectorInput {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s := base()
	s.RouteLane, s.StrategyKey = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 1_677_000_000, 1_677_000_000
	s.StrategyNAVRaw, s.PriorReportedNAVRaw, s.TotalVaultNAVRaw = 1_677_000_000, 1_677_000_000, 1_677_000_000
	s.BorrowUtilizationBlocked, s.LeverageTargetLevel = true, 1.5
	p := DefaultSelectorPolicy()
	curve := []BorrowCurvePoint{{0, 400}, {9000, 1200}, {10000, 10000}}
	auto := LaneEconomics{Lane: autoAUTOPYUSD.Lane, EvidenceID: "auto", ObservedAt: now, NativeObservedAt: now, NativeAPY: .0948, CurrentBorrowAPY: .12, BorrowCurve: curve, DebtSupplyRaw: 1e12, DebtBorrowRaw: .926e12, EntryCapacity: Capacity{Known: true}}
	onre := LaneEconomics{Lane: onreONycUSDC, EvidenceID: "onre", ObservedAt: now, NativeObservedAt: now, NativeAPY: onreAPY, CurrentBorrowAPY: .12, BorrowCurve: curve, DebtSupplyRaw: 1e12, DebtBorrowRaw: .926e12, EntryCapacity: Capacity{Known: true, Raw: 1_677_000_000}}
	q := MoveQuote{Unlevered: true, MinimumIdleRaw: 1_677_000_000, SourceLane: s.RouteLane, DestinationLane: onreONycUSDC, ObservationID: s.ObservationID, EquityRaw: 1_677_000_000, CostRaw: cost, ObservedAt: now, EvidenceID: "complete-1x-sequence", SampleSlot: s.Slot, ValidThroughSlot: s.Slot + 32,
		SourceExit: &selectorExitBound{MaxCollateralRaw: s.PositionCollateralRaw}}
	return SelectorInput{Now: now, Snapshot: s, Markets: []LaneEconomics{auto, onre}, Quotes: []MoveQuote{q}, Policy: p}
}

func selectUnlevered(in SelectorInput, previous SelectorState) SelectorResult {
	return SelectOpportunity(in, previous)
}

// A 1x quote carrying any borrow is invalid; 1x is AUTO/OnRe only.
func TestUnleveredQuoteShape(t *testing.T) {
	t.Parallel()
	q := unleveredSwitchFixture(1).Quotes[0]
	if !q.validBorrow() {
		t.Fatal("valid 1x quote refused")
	}
	for name, bad := range map[string]func(*MoveQuote){
		"borrow":    func(q *MoveQuote) { q.BorrowReceiveRaw = 1 },
		"fee":       func(q *MoveQuote) { q.BorrowFeeRaw = 1 },
		"exit-only": func(q *MoveQuote) { q.DestinationLane = ethenaUSDePYUSD.Lane },
		"no equity": func(q *MoveQuote) { q.EquityRaw = 0 },
	} {
		c := q
		bad(&c)
		if c.validBorrow() {
			t.Fatalf("%s: invalid 1x quote accepted", name)
		}
	}
}

// (d) A flat vault enters the selector's 1x lane: allocation, swap and
// deposit, then borrow blocked -> the step holds at target; when the pool
// reopens with an approved 1.5x level, B2 levers through leverage_up.
func TestUnleveredEntryLifecycleAndNoLoop(t *testing.T) {
	t.Parallel()
	s := base()
	s.Slot = 42
	s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
	s.BorrowUtilizationBlocked = true
	s.CapacityRaw, s.MaxTargetLTVEntryRaw, s.PolicyLimitRaw = 2_000_000_000, 2_000_000_000, int64(strategyTwoBridgeLegCapRaw)
	s.VoltrIdleRaw, s.TotalVaultNAVRaw = 1_677_000_000, 1_677_000_000
	s.MinimumCollateralDepositRaw = 1
	q := entryQuoteFixture(time.Now().UTC(), onreONycUSDC, 1_677_000_000)
	q.Unlevered, q.BorrowReceiveRaw = true, 0
	in := targetInputs{Entry: &SelectorResult{Action: "ENTER", DestinationLane: onreONycUSDC, SelectedQuote: &q}}
	check := func(action Action, reason string) {
		t.Helper()
		if got := Decide(targetedWith(s, in)); got.Action != action || got.Reason != reason {
			t.Fatalf("want %s %s, got %+v (target %+v)", action, reason, got, targetedWith(s, in).Target)
		}
	}
	check(VoltrAllocateToSquads, entryAllocationReason)
	s.VoltrIdleRaw, s.SquadsIdleRaw = 0, 1_677_000_000
	check(SwapStableToCollateralStep, entrySwapReason)
	s.SquadsIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw = 0, 1_670_000_000, 1_670_000_000
	check(OpenRouteStep, entryDepositReason)
	s.CollateralIdleRaw, s.PrimeIdleRaw = 0, 0
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.StrategyNAVRaw = true, 1_670_000_000, 1_670_000_000, 1_670_000_000
	check(Hold, holdAtTarget)
	// Held, the vault keeps its lane: no selector entry, no re-entry loop.
	if !laneExposure(s) {
		t.Fatal("a funded 1x position is not exposure")
	}
	s.LeverageTargetLevel = 1
	check(Hold, holdAtTarget)
	// Pool reopens with an approved 1.5x level: B2 levers through leverage_up.
	s.BorrowUtilizationBlocked, s.LeverageTargetLevel = false, 1.5
	armLeverageCapacityFixture(&s)
	check(OpenRouteStep, leverageUpReason)
}

// (e) Every Kamino and Jupiter wire the 1x entry and its exit use, compiled
// by the real compiler from the priced recipe, passes the persisted-wire gate.
func TestUnleveredEntryRecipeWiresPassThePersistedWireGate(t *testing.T) {
	t.Parallel()
	m, rpc, client, _ := selectorDestinationFixtureForLane(t, onreONycUSDC, blockDebtUtilization(t, onreONycUSDC))
	q, err := observeSelectorDestinationForecast(context.Background(), rpc, fixtureView(t, rpc), client, m, capturedTestPolicies(), leveredTestMarket(onreONycUSDC), 1_000_000, 42, true)
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{71}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	kamino, swaps := 0, 0
	for _, input := range q.Recipe.Inputs {
		r, _, message, err := input.decode()
		if err != nil {
			t.Fatal(err)
		}
		switch r := r.(type) {
		case KaminoPrimeUSDCRequest:
			kamino++
			// The persisted-evidence fixture pins this blockhash/height.
			r.RecentBlockhash, r.LastValidBlockHeight = bridgeSettings, 99
			compiled, err := compileKaminoMessageForDelegate(r, delegate)
			if err != nil {
				t.Fatal(err)
			}
			if err := signedTestBuildResult(t, key, compiled).validateForDelegate(delegate); err != nil {
				t.Fatalf("Kamino %s wire refused: %v", r.Action, err)
			}
		case JupiterSwapRequest:
			swaps++
			var decodeErr error
			if message[0] == 0x80 {
				_, _, _, _, decodeErr = decodeExactWire(signTestWire(t, key, message))
			} else {
				_, _, _, _, decodeErr = decodeExactWire(signTestWire(t, key, message))
			}
			if decodeErr != nil {
				t.Fatalf("swap %s wire refused: %v", r.Action, decodeErr)
			}
		}
	}
	if kamino != 2 || swaps != 2 {
		t.Fatalf("recipe legs kamino=%d swaps=%d", kamino, swaps)
	}
}
