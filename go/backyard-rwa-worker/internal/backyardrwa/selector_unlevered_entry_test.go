package backyardrwa

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"math"
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
	m, rpc, client, _ := selectorDestinationFixtureForLane(t, onreONycUSDC, blockDebtUtilization(t, onreONycUSDC))
	q, err := observeSelectorDestinationForecastAuthorized(context.Background(), rpc, client, m, onreONycUSDC, 1_000_000, 42, true, nil)
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

// (c) Not blocked: the same lane keeps the leveraged entry exactly, and
// Maple never takes the 1x path even when blocked.
func TestUnblockedOrNonLeverageLaneKeepsTheLeveragedEntry(t *testing.T) {
	m, rpc, client, _ := selectorDestinationFixtureForLane(t, onreONycUSDC, nil)
	q, err := observeSelectorDestinationForecastAuthorized(context.Background(), rpc, client, m, onreONycUSDC, 100_000_000, 42, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if q.Unlevered || q.BorrowReceiveRaw == 0 {
		t.Fatalf("unblocked OnRe lost its leveraged entry: %+v", q)
	}
	m, rpc, client, _ = selectorDestinationFixtureForLane(t, SelectedRouteID, blockDebtUtilization(t, SelectedRouteID))
	if _, err = observeSelectorDestinationForecastAuthorized(context.Background(), rpc, client, m, SelectedRouteID, 1_000_000, 42, true, nil); err == nil {
		t.Fatal("blocked Maple priced an entry")
	}
	assertBudgetHold(t, err, "selector_destination_capacity_unavailable")
}

// Live shape 2026-09-29: funded debt-free AUTO at 1x (~$1,677, borrowing
// blocked), OnRe at 11.02% vs AUTO 9.48% at 1x, OnRe borrowing blocked too.
func unleveredSwitchFixture(cost int64) SelectorInput { return unleveredSwitchFixtureAt(cost, .1102) }

func unleveredSwitchFixtureAt(cost int64, onreAPY float64) SelectorInput {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s := base()
	s.RouteLane, s.StrategyKey, s.PilotActive = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, true
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 1_677_000_000, 1_677_000_000
	s.StrategyNAVRaw, s.PriorReportedNAVRaw, s.TotalVaultNAVRaw = 1_677_000_000, 1_677_000_000, 1_677_000_000
	s.BorrowUtilizationBlocked, s.LeverageTargetLevel = true, 1.5
	s.PilotTrancheCapLane = autoAUTOPYUSD.Lane // stamped live by the reviewed manifest
	p := DefaultSelectorPolicy()
	curve := []BorrowCurvePoint{{0, 400}, {9000, 1200}, {10000, 10000}}
	auto := LaneEconomics{Lane: autoAUTOPYUSD.Lane, EvidenceID: "auto", ObservedAt: now, NativeObservedAt: now, NativeAPY: .0948, CurrentBorrowAPY: .12, BorrowCurve: curve, DebtSupplyRaw: 1e12, DebtBorrowRaw: .926e12, EntryCapacity: Capacity{Known: true}}
	onre := LaneEconomics{Lane: onreONycUSDC, EvidenceID: "onre", ObservedAt: now, NativeObservedAt: now, NativeAPY: onreAPY, CurrentBorrowAPY: .12, BorrowCurve: curve, DebtSupplyRaw: 1e12, DebtBorrowRaw: .926e12, EntryCapacity: Capacity{Known: true, Raw: 1_677_000_000}}
	q := MoveQuote{Unlevered: true, MinimumIdleRaw: 1_677_000_000, SourceLane: s.RouteLane, DestinationLane: onreONycUSDC, ObservationID: s.ObservationID, EquityRaw: 1_677_000_000, CostRaw: cost, ObservedAt: now, EvidenceID: "complete-1x-sequence", SampleSlot: s.Slot, ValidThroughSlot: s.Slot + 32,
		SourceExit: &selectorExitBound{MaxCollateralRaw: s.PositionCollateralRaw}}
	return SelectorInput{Now: now, Snapshot: s, Markets: []LaneEconomics{auto, onre}, Quotes: []MoveQuote{q}, Policy: p}
}

func selectUnlevered(in SelectorInput, previous SelectorState) SelectorResult {
	allowed := func(l string) bool { return selectorLane(l) || l == autoAUTOPYUSD.Lane }
	return selectOpportunityWithLanes(in, previous, allowed, allowed)
}

func onreCandidate(t *testing.T, r SelectorResult) CandidateForecast {
	t.Helper()
	for _, c := range r.Candidates {
		if c.Lane == onreONycUSDC {
			return c
		}
	}
	t.Fatal("no OnRe candidate", r)
	return CandidateForecast{}
}

// (a)+(b) The 1x quote is scored at the 1x yield (no borrow) and pays its
// full switch cost; SWITCH needs gain > MinimumBenefit + cost over the 30-day
// horizon, persisted 30 minutes in its own 1x window.
func TestUnleveredSwitchIsScoredAt1xAndNeedsPersistence(t *testing.T) {
	in := unleveredSwitchFixtureAt(3_000_000, .16) // a clearly better 1x lane
	years := in.Policy.Horizon.Hours() / (365.25 * 24)
	first := selectUnlevered(in, SelectorState{})
	c := onreCandidate(t, first)
	invested := float64(1_677_000_000 - 3_000_000)
	gross := forecastGain(invested, invested, 0, in.Markets[1], 0, years) - 3_000_000
	want := float64(in.Snapshot.TotalVaultNAVRaw) * math.Expm1(.8*math.Log1p(gross/float64(in.Snapshot.TotalVaultNAVRaw)))
	if !c.CostsKnown || c.BlockedReason != "" || c.GainRaw != want || c.BorrowAPR != 0 {
		t.Fatalf("1x candidate %+v want gain %.0f", c, want)
	}
	if first.Action != "KEEP" || first.Reason != "advantage_not_yet_persistent" || c.BenefitRaw <= float64(in.Policy.MinimumBenefitRaw) {
		t.Fatalf("first sample: %+v benefit %.0f", first.Reason, c.BenefitRaw)
	}
	if _, ok := first.State.Advantages[unleveredAdvantageKey(onreONycUSDC)]; !ok {
		t.Fatal("1x window not started")
	}
	state := first.State
	for i := 1; i <= 6; i++ { // 30 minutes of 5-minute samples
		advanceSelectorFixture(&in, 5*time.Minute)
		r := selectUnlevered(in, state)
		state = r.State
		if i < 6 && r.Action != "KEEP" {
			t.Fatalf("switched before 30 minutes (%d min): %+v", i*5, r.Reason)
		}
		if i == 6 && (r.Action != "SWITCH" || r.DestinationLane != onreONycUSDC || r.SelectedQuote == nil || !r.SelectedQuote.Unlevered) {
			t.Fatalf("no switch after 30 minutes: %+v %+v", r.Action, r.Reason)
		}
	}
	// A leveraged feed-level window never stands in for the 1x window.
	in = unleveredSwitchFixtureAt(3_000_000, .16)
	lev := SelectorState{SourceLane: autoAUTOPYUSD.Lane, Advantages: map[string]AdvantageWindow{onreONycUSDC: {Since: in.Now.Add(-time.Hour), LastSample: in.Now.Add(-time.Minute)}}}
	if r := selectUnlevered(in, lev); r.Action == "SWITCH" {
		t.Fatal("leveraged window reused for a 1x switch")
	}
	// Gross cost that removes the remaining investor edge: never switches.
	in = unleveredSwitchFixtureAt(0, .16)
	gap := onreCandidate(t, selectUnlevered(in, SelectorState{})).BenefitRaw
	in = unleveredSwitchFixtureAt(int64((gap-float64(in.Policy.MinimumBenefitRaw))*1.25)+1, .16)
	state = SelectorState{}
	for i := 0; i <= 7; i++ {
		r := selectUnlevered(in, state)
		if r.Action == "SWITCH" {
			t.Fatalf("switched with benefit %.0f <= MinimumBenefit", onreCandidate(t, r).BenefitRaw)
		}
		state = r.State
		advanceSelectorFixture(&in, 5*time.Minute)
	}
}

// A 1x quote carrying any borrow is invalid; 1x is AUTO/OnRe only.
func TestUnleveredQuoteShape(t *testing.T) {
	q := unleveredSwitchFixture(1).Quotes[0]
	if !q.validBorrow() {
		t.Fatal("valid 1x quote refused")
	}
	for name, bad := range map[string]func(*MoveQuote){
		"borrow":    func(q *MoveQuote) { q.BorrowReceiveRaw = 1 },
		"fee":       func(q *MoveQuote) { q.BorrowFeeRaw = 1 },
		"maple":     func(q *MoveQuote) { q.DestinationLane = SelectedRouteID },
		"no equity": func(q *MoveQuote) { q.EquityRaw = 0 },
	} {
		c := q
		bad(&c)
		if c.validBorrow() {
			t.Fatalf("%s: invalid 1x quote accepted", name)
		}
	}
}

// (d) After the 1x entry lands: borrow blocked -> B2 holds, the position is a
// finished tranche (no selector freeze, no re-entry loop), and same-lane
// reinvestment stays closed. When the pool reopens, B2 owns leverage.
func TestUnleveredEntryLifecycleAndNoLoop(t *testing.T) {
	s := base()
	s.RouteLane, s.StrategyKey, s.PilotActive = onreONycUSDC, onreONycUSDC, true
	s.BorrowUtilizationBlocked = true
	s.CapacityRaw, s.MaxTargetLTVEntryRaw, s.PolicyLimitRaw = 2_000_000_000, 2_000_000_000, int64(strategyTwoBridgeLegCapRaw)
	s.VoltrIdleRaw, s.TotalVaultNAVRaw, s.SelectorEntryEquityRaw = 1_677_000_000, 1_677_000_000, 1_677_000_000
	s.MinimumCollateralDepositRaw = 1
	check := func(action Action, reason string) {
		t.Helper()
		if got := Decide(s); got.Action != action || got.Reason != reason {
			t.Fatalf("want %s %s, got %+v", action, reason, got)
		}
	}
	check(VoltrAllocateToSquads, "eligible_voltr_idle")
	s.VoltrIdleRaw, s.SquadsIdleRaw = 0, 1_677_000_000
	check(SwapStableToCollateralStep, "usdc_requires_prime_collateral")
	s.SquadsIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw = 0, 1_670_000_000, 1_670_000_000
	check(OpenRouteStep, "prime_collateral_ready")
	s.CollateralIdleRaw, s.PrimeIdleRaw = 0, 0
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.StrategyNAVRaw = true, 1_670_000_000, 1_670_000_000, 1_670_000_000
	check(Hold, "debt_reserve_utilization_blocks_borrow")
	if selectorTrancheInProgress(s) || sameLaneReinvestmentEligibleWithLane(s, DefaultSelectorPolicy(), selectorEntryLane) {
		t.Fatal("1x position looks unfinished or re-enterable")
	}
	s.LeverageTargetLevel = 1
	check(Hold, "debt_reserve_utilization_blocks_borrow")
	// Pool reopens with a 1.5x target: B2 levers through leverage_up.
	s.BorrowUtilizationBlocked, s.LeverageTargetLevel = false, 1.5
	armLeverageCapacityFixture(&s)
	check(OpenRouteStep, leverageUpReason)
}

// (e) Every Kamino and Jupiter wire the 1x entry and its exit use, compiled
// by the real compiler from the priced recipe, passes the persisted-wire gate.
func TestUnleveredEntryRecipeWiresPassThePersistedWireGate(t *testing.T) {
	m, rpc, client, _ := selectorDestinationFixtureForLane(t, onreONycUSDC, blockDebtUtilization(t, onreONycUSDC))
	q, err := observeSelectorDestinationForecastAuthorized(context.Background(), rpc, client, m, onreONycUSDC, 1_000_000, 42, true, nil)
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
			compiled, err := m.compileKaminoMessage(r, delegate)
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
				_, _, _, _, decodeErr = decodeExactV0Wire(signTestWire(t, key, message))
			} else {
				_, _, _, _, decodeErr = decodeExactLegacyWire(signTestWire(t, key, message))
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

// Live numbers (2026-09-29): AUTO 1x 9.48%, OnRe 1x 11.02%, $1,677. Reports
// the largest switch cost the unchanged rule accepts.
func TestUnleveredSwitchLiveBreakEven(t *testing.T) {
	in := unleveredSwitchFixture(0)
	r := selectUnlevered(in, SelectorState{})
	c := onreCandidate(t, r)
	// benefit(cost) = benefit(0) - cost*(1 + ~yield over horizon); the rule
	// needs benefit > MinimumBenefit.
	t.Logf("keep=%.0f onreGain(cost 0)=%.0f uncertainty=%d benefit(cost 0)=%.0f minimum=%d -> max switch cost ~%.0f raw",
		r.KeepGainRaw, c.GainRaw, in.Snapshot.TotalVaultNAVRaw*in.Policy.UncertaintyBPS/10_000, c.BenefitRaw, in.Policy.MinimumBenefitRaw, c.BenefitRaw-float64(in.Policy.MinimumBenefitRaw))
}
