package backyard

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"testing"
	"time"
)

// Live 2026-09-28 shape: route state still carries the 09-26 AUTO selector
// entry (quote borrow $189.87, allocation bound), the AUTO position is funded
// and debt-free (~$1,676) and borrowing reopens. The installed code borrowed
// the old quote (~1.11x). B2: no target holds; a 1.5x target borrows through
// leverage_up sized to the target, and the entry quote is never used.
func TestStaleEntryDebtFreeReopenUsesTheTargetNeverTheQuote(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC} {
		entry := selectorEntryFixture(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC), lane, 1_000_000_000)
		entry.Quote.BorrowReceiveRaw, entry.AllocationOperationID = 189_873_681, "alloc-0926"
		s := base()
		s.Slot = 451_000_000
		s.RouteLane, s.StrategyKey = lane, lane
		s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 1_676_000_000, 1_676_000_000
		if lane == onreONycUSDC {
			if err := applySelectorEntry(&s, &entry, time.Now().UTC()); err != nil {
				t.Fatal(lane, err)
			}
		} else {
			// The PYUSD quote needs bound price evidence to validate; set
			// what applySelectorEntry stamps for a funded position.
			s.SelectorEntryEquityRaw, s.SelectorBorrowRaw = entry.EquityRaw, entry.Quote.BorrowReceiveRaw
		}
		if s.SelectorEntryPaused || s.SelectorBorrowRaw != 189_873_681 {
			t.Fatalf("%s: fixture is not the live shape: paused=%t borrow=%d", lane, s.SelectorEntryPaused, s.SelectorBorrowRaw)
		}
		if got := Decide(s); got.Action != Hold || got.Reason != "leverage_target_required" {
			t.Fatalf("%s: no target must hold, got %+v", lane, got)
		}
		s.LeverageTargetLevel = 1.5
		armLeverageCapacityFixture(&s)
		got := Decide(s)
		if got.Action != OpenRouteStep || got.Reason != leverageUpReason || got.AmountRaw != 837_162_000 {
			t.Fatalf("%s: target 1.5x must borrow through leverage_up, got %+v", lane, got)
		}
		// Sizing: 50% of collateral value (the target), not the entry quote.
		position := leverageTestPosition(1_676_000_000, 0)
		leg, wire, _, err := selectKaminoLeg(got, position)
		if err != nil || leg != kaminoLegBorrow || wire != 837_162_000 || wire == entry.Quote.BorrowReceiveRaw {
			t.Fatalf("%s: leverage_up sized %d (leg %d, err %v), want 837162000", lane, wire, leg, err)
		}
	}
}

// Six-decimal collateral and debt at $1 each: value raw == amount raw.
func leverageTestPosition(collateral, debt uint64) KaminoPosition {
	p := KaminoPosition{CollateralDepositedRaw: collateral, RedeemablePrimeRaw: collateral, DebtRaw: debt, CollateralDecimals: 6, DebtDecimals: 6}
	binary.LittleEndian.PutUint64(p.CollateralPriceSF[:8], uint64(1)<<60)
	binary.LittleEndian.PutUint64(p.DebtPriceSF[:8], uint64(1)<<60)
	return p
}

func TestLeverageUpDecisionsAtEachLevel(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC} {
		at := func(level, target float64) Snapshot {
			s := leverageSnapshot(level)
			s.RouteLane, s.StrategyKey, s.LeverageTargetLevel = lane, lane, target
			s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 1, 1, 1
			if lane == onreONycUSDC {
				s.DebtIdleRaw = 0
			}
			return s
		}
		for _, tc := range []struct {
			level, target float64
			action        Action
			reason        string
			amount        int64
		}{
			{1, 1.5, OpenRouteStep, leverageUpReason, 499_500_000},
			{1, 1.75, OpenRouteStep, leverageUpReason, 499_500_000}, // one level per move
			{1, 1, Hold, "leverage_target_1x", 0},
			{1.5, 1.75, OpenRouteStep, leverageUpReason, 249_250_000},
			{1.5, 1.5, Hold, "single_loop_position_ready", 0},
			{1.75, 1.75, Hold, "single_loop_position_ready", 0},
		} {
			s := at(tc.level, tc.target)
			got := Decide(s)
			if got.Action != tc.action || got.Reason != tc.reason || got.AmountRaw != tc.amount {
				t.Fatalf("%s %.2fx target %.2fx: %+v", lane, tc.level, tc.target, got)
			}
			// Blocked borrowing never starts an up move.
			s.BorrowUtilizationBlocked = true
			if got := Decide(s); got.Action == OpenRouteStep {
				t.Fatalf("%s %.2fx: blocked borrowing still borrowed: %+v", lane, tc.level, got)
			}
		}
		// The selector-entry pause does not stop an up move on the current
		// lane, and a pending up move keeps the selector frozen.
		s := at(1, 1.5)
		s.SelectorEntryPaused = true
		if got := Decide(s); got.Reason != leverageUpReason || !selectorTrancheInProgress(s) {
			t.Fatalf("%s: paused up move: %+v in-progress=%t", lane, got, selectorTrancheInProgress(s))
		}
		// Withdrawal, unwind and hard LTV keep their priority.
		for name, mutate := range map[string]func(*Snapshot){
			"withdrawal": func(s *Snapshot) { s.WithdrawalDemandRaw = 1 },
			"unwind":     func(s *Snapshot) { s.Unwind = true },
			"hard ltv":   func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS = 1, 1, 6_000 },
		} {
			c := at(1, 1.5)
			mutate(&c)
			if got := Decide(c); got.Reason == leverageUpReason {
				t.Fatalf("%s: %s did not preempt leverage_up", lane, name)
			}
		}
	}
	exitOnly := leverageSnapshot(1.5)
	exitOnly.RouteLane, exitOnly.StrategyKey, exitOnly.LeverageTargetLevel = ethenaUSDePYUSD.Lane, ethenaUSDePYUSD.Lane, 1.75
	if got := Decide(exitOnly); got.Reason == leverageUpReason {
		t.Fatalf("exit-only Ethena levered up: %+v", got)
	}
}

// Sizing lands each step on its level; receive+fee never takes the instant
// LTV above 50%; the loop result stays at or below 45%.
func TestLeverageUpSizingAndCaps(t *testing.T) {
	t.Parallel()
	noFee := func(uint64) (uint64, error) { return 0, nil }
	// 1x -> 1.5x on $1,000: borrow $500 (debt/equity 0.5).
	p := leverageTestPosition(1_000_000_000, 0)
	if got, err := p.leverageUpBorrowRaw(150); err != nil || got != 500_000_000 {
		t.Fatalf("1x->1.5x %d %v", got, err)
	}
	// 1.5x -> 1.75x: collateral $1,500, debt $500, equity $1,000: borrow 25%
	// of equity = $250; C'=1,750, D'=750, LTV 42.9%.
	p = leverageTestPosition(1_500_000_000, 500_000_000)
	got, err := p.leverageUpBorrowRaw(175)
	if err != nil || got != 250_000_000 {
		t.Fatalf("1.5x->1.75x %d %v", got, err)
	}
	// The 0.1% margin under 50% trims it to $249.25 (lands ~1.748x).
	got, err = leverageUpCapFee(p, got, noFee)
	if err != nil || got != 249_250_000 {
		t.Fatalf("1.5x->1.75x capped %d %v", got, err)
	}
	instant := leverageTestPosition(1_500_000_000, 500_000_000+got)
	if ltv, _ := observedLTVBPS(instant); ltv > TargetLTVBPS {
		t.Fatalf("instant LTV %d above 50%%", ltv)
	}
	after := leverageTestPosition(1_750_000_000, 750_000_000)
	if ltv, _ := observedLTVBPS(after); ltv > leverageMaxLTVBPS {
		t.Fatalf("post-move LTV %d above 45%%", ltv)
	}
	// Already at the level: refused.
	if _, err := leverageTestPosition(1_750_000_000, 750_000_000).leverageUpBorrowRaw(175); err == nil {
		t.Fatal("borrowed at the target level")
	}
	// The 1x->1.5x borrow sits at 50%: the fee shrinks receive so receive+fee
	// stays inside 50% (with the 0.1% margin).
	p = leverageTestPosition(1_000_000_000, 0)
	receive, _ := p.leverageUpBorrowRaw(150)
	capped, err := leverageUpCapFee(p, receive, func(r uint64) (uint64, error) { return r / 1000, nil })
	if err != nil || capped+capped/1000 > 500_000_000-500_000 {
		t.Fatalf("fee cap %d %v", capped, err)
	}
	// Tiny positions are refused, not dust-borrowed.
	if _, err := leverageUpCapFee(leverageTestPosition(10_000_000, 0), 5_000_000, noFee); err == nil {
		t.Fatal("dust borrow admitted")
	}
	for _, bad := range []int64{1, 100, 200} {
		if _, err := p.leverageUpBorrowRaw(bad); err == nil {
			t.Fatalf("level %d accepted", bad)
		}
	}
	if !borrowDebtMatches(0, 0) || borrowDebtMatches(1, 0) || !borrowDebtMatches(500_000_300, 500_000_000) || borrowDebtMatches(501_000_000, 500_000_000) {
		t.Fatal("debt match tolerance")
	}
}

// The borrow-authority exception is exactly: journaled reason leverage_up,
// a borrow leg, same AUTO/OnRe lane, no unwind, a stored target above 1x.
func TestLeverageUpEntryFenceExceptionIsNarrow(t *testing.T) {
	t.Parallel()
	manifest := embeddedTestManifest(t)
	borrow, err := manifest.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegBorrow, 10_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, onreONycUSDC)
	if err != nil {
		t.Fatal(err)
	}
	deposit, err := manifest.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, onreONycUSDC)
	if err != nil {
		t.Fatal(err)
	}
	target := []byte(`{"lane":"OnRe/ONyc/USDC","level":1.5,"borrowRaw":10000000,"operationId":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","spreadBps":150,"decidedAt":"2026-09-28T12:00:00Z"}`)
	if ok, err := leverageUpBypassesEntryFence(borrow, leverageUpReason, false, onreONycUSDC, target, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"); !ok || err != nil {
		t.Fatalf("valid leverage_up refused: %v", err)
	}
	if ok, err := leverageUpBypassesEntryFence(borrow, "prime_collateral_requires_borrow", false, onreONycUSDC, target, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"); ok || err != nil {
		t.Fatal("other reasons must keep the entry fence")
	}
	for name, c := range map[string]struct {
		request any
		unwind  bool
		lane    string
		target  string
	}{
		"deposit leg":       {deposit, false, onreONycUSDC, string(target)},
		"unwinding":         {borrow, true, onreONycUSDC, string(target)},
		"other op lane":     {borrow, false, autoAUTOPYUSD.Lane, string(target)},
		"no target":         {borrow, false, onreONycUSDC, "null"},
		"1x target":         {borrow, false, onreONycUSDC, `{"lane":"OnRe/ONyc/USDC","level":1,"spreadBps":0,"decidedAt":"2026-09-28T12:00:00Z"}`},
		"other lane target": {borrow, false, onreONycUSDC, `{"lane":"AUTO/AUTO/PYUSD","level":1.5,"spreadBps":0,"decidedAt":"2026-09-28T12:00:00Z"}`},
	} {
		if ok, err := leverageUpBypassesEntryFence(c.request, leverageUpReason, c.unwind, c.lane, []byte(c.target)); ok || err == nil {
			t.Fatalf("%s: leverage_up bypassed the fence", name)
		}
	}
}

// The 1.5x->1.75x borrow refreshes both reserves (debt already open). The
// real compiler's wire must pass the persisted-wire gate for AUTO and OnRe,
// and stay refused for Maple and Prime.
func TestLeverageUpBorrowWithDebtPassesThePersistedWireGate(t *testing.T) {
	t.Parallel()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{61}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	for _, c := range []struct {
		lane     string
		manifest RouteManifest
		accepted bool
	}{
		{autoAUTOPYUSD.Lane, embeddedTestManifest(t), true},
		{onreONycUSDC, embeddedTestManifest(t), true},
		{SelectedRouteID, embeddedTestManifest(t), true},
		{PhaseOneLaneID, embeddedTestManifest(t), true},
		{primePRIMEPYUSD.Lane, embeddedTestManifest(t), true},
		{primePRIMEUSDS.Lane, embeddedTestManifest(t), true},
		{ethenaUSDePYUSD.Lane, embeddedTestManifest(t), false},
	} {
		route, err := runtimeRoute(c.lane)
		if err != nil {
			t.Fatal(err)
		}
		for name, reserves := range map[string][]string{
			"first borrow (collateral)":     {route.Kamino.CollateralReserve},
			"leverage_up (collateral+debt)": {route.Kamino.CollateralReserve, route.Kamino.DebtReserve},
		} {
			request, err := c.manifest.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegBorrow, 250_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, c.lane)
			if err != nil {
				t.Fatalf("%s %s: packet: %v", c.lane, name, err)
			}
			request.ObligationReserves = reserves
			message, err := compileKaminoMessageForDelegate(request, delegate)
			if err != nil {
				t.Fatalf("%s %s: compile: %v", c.lane, name, err)
			}
			err = signedTestBuildResult(t, key, message).validateForDelegate(delegate)
			want := c.accepted || len(reserves) == 1
			if want && err != nil {
				t.Errorf("%s %s: PersistSigned gate refused: %v", c.lane, name, err)
			}
			if !want && err == nil {
				t.Errorf("%s %s: gate accepted a leverage_up topology on an exit-only lane", c.lane, name)
			}
		}
	}
}

// The one-release exit covers 1.5x but not 1.75x: that is why 1.75x runs
// the multi-cycle exit (leverage_exit_pricer.go).
func TestOneReleaseExitCoversOnlyUpTo1_5x(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		collateral, debt uint64
		covered          bool
	}{{1_500_000_000, 500_000_000, true}, {1_750_000_000, 750_000_000, false}} {
		_, release, err := withdrawExcessAtLTV(leverageTestPosition(c.collateral, c.debt), 5500)
		if err != nil || (release >= c.debt) != c.covered {
			t.Fatalf("C=%d D=%d: one release %d covered=%t, want %t", c.collateral, c.debt, release, release >= c.debt, c.covered)
		}
	}
	if leverageMaxLiveLevel != 1.75 {
		t.Fatal("live cap")
	}
}
