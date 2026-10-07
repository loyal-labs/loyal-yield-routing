package backyard

import (
	"context"
	"encoding/binary"
	"testing"
)

// A funded debt-free OnRe position with idle Voltr cash (B4): the USDC lane
// gets the same plan B3 top-up sequence as AUTO.
func onreTopupSnapshot() Snapshot {
	s := base()
	s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 300_000_000_000, 345_000_000
	s.LTVBPS, s.LiquidationThresholdBPS = 0, 8000
	s.VoltrIdleRaw, s.TopupDepositRoomRaw, s.PilotActive = 1_295_000_000, 5_000_000_000, true
	s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 0, 0, 0 // borrow capacity is irrelevant to a top-up
	return s
}

func TestOnReTopupSequenceOnTheUSDCPath(t *testing.T) {
	s := onreTopupSnapshot()
	check := func(s Snapshot, action Action, reason string, amount int64) {
		t.Helper()
		got := Decide(s)
		if got.Action != action || got.Reason != reason || got.AmountRaw != amount || got.StrategyKey != onreONycUSDC || got.Validate() != nil {
			t.Fatalf("want %s %s %d, got %+v", action, reason, amount, got)
		}
	}
	check(s, VoltrAllocateToSquads, topupAllocationReason, 1_295_000_000)
	s.VoltrIdleRaw, s.SquadsIdleRaw = 0, 1_295_000_000
	check(s, SwapStableToCollateralStep, topupSwapReason, 1_295_000_000)
	s.SquadsIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw = 0, 1_120_000_000_000, 1_120_000_000_000
	check(s, OpenRouteStep, topupDepositReason, 1_120_000_000_000)
	// Once everything is deposited the B2 target borrow of the whole
	// collateral follows (no top-up is left to do).
	s.CollateralIdleRaw, s.PrimeIdleRaw, s.PositionCollateralRaw = 0, 0, 1_420_000_000_000
	s.LeverageTargetLevel = 1.5
	armLeverageCapacityFixture(&s)
	check(s, OpenRouteStep, leverageUpReason, int64(s.LeverageApprovedBorrowRaw))
	// Size: min(idle - buffer, tranche cap, deposit-limit room).
	room := onreTopupSnapshot()
	room.TopupDepositRoomRaw = 400_000_000
	check(room, VoltrAllocateToSquads, topupAllocationReason, 400_000_000)
}

func TestOnReTopupKeepsSafetyPriorityAndOtherLanesUnchanged(t *testing.T) {
	s := onreTopupSnapshot()
	for name, mutate := range map[string]func(*Snapshot){
		"uncovered demand": func(s *Snapshot) { s.WithdrawalDemandRaw = s.VoltrIdleRaw + 1 },
		"covered demand":   func(s *Snapshot) { s.WithdrawalDemandRaw = 1 },
		"unwind":           func(s *Snapshot) { s.Unwind = true },
		"hard ltv": func(s *Snapshot) {
			s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS, s.SquadsIdleRaw = 1, 1, 6000, 1
		},
		"report due":  func(s *Snapshot) { s.PostMutationNAVRequired = true },
		"nonterminal": func(s *Snapshot) { s.Nonterminal = Built },
		"leveraged": func(s *Snapshot) {
			s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS = 100_000_000, 100_000_000, 3000
		},
		"flat": func(s *Snapshot) { s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = false, 0, 0 },
	} {
		c := s
		mutate(&c)
		switch got := Decide(c); got.Reason {
		case topupAllocationReason, topupSwapReason, topupDepositReason:
			t.Fatalf("%s: top-up selected: %+v", name, got)
		}
	}
	// Maple keeps its installed behaviour: cash beside a position is still
	// the manual-recovery hold, never a top-up.
	maple := s
	maple.RouteLane, maple.StrategyKey = SelectedRouteID, SelectedRouteID
	maple.VoltrIdleRaw, maple.SquadsIdleRaw = 0, 1_000_000
	if got := Decide(maple); got.Action != HoldManualRecovery || got.Reason != "entry_tranche_contains_unassigned_cash" {
		t.Fatalf("Maple changed: %+v", got)
	}
	maple.SquadsIdleRaw, maple.VoltrIdleRaw = 0, 1_295_000_000
	if got := Decide(maple); got.Reason == topupAllocationReason {
		t.Fatalf("Maple got a top-up allocation: %+v", got)
	}
}

// The three top-up admissions accept a funded debt-free OnRe position through
// the lane's real Jupiter exports and the shared-cash USDC custody.
func TestOnReTopupAdmissionsAcceptTheDebtFreePosition(t *testing.T) {
	o, m, rpc, client, accounts := usdcReturnFixtureForLane(t, onreONycUSDC)
	route, _ := runtimeRoute(onreONycUSDC)
	clear(accountAt(accounts, route.Kamino.Obligation).Data[1208:1408])
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.PayoffDebtRaw, o.Snapshot.DebtIdleRaw = 0, 0, 0, 0
	ctx := context.Background()

	// Leg: swap Squads USDC to ONyc beside the position.
	swap := Decision{Action: SwapStableToCollateralStep, StrategyKey: onreONycUSDC, AmountRaw: 11_000, Reason: topupSwapReason}
	e, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, swap, 11_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	e.Request.EntryReturnReserved, e.Request.TopupReturnReserved = true, true
	plan, err := observePhase3EntrySwapAdmission(ctx, rpc, client, m, o, swap, e)
	if err != nil {
		t.Fatalf("OnRe top-up swap refused: %v", err)
	}
	if len(plan.Exit) < 4 || plan.Exit[0].Action != ReportNAV || plan.Exit[1].Action != DeleverRouteStep {
		t.Fatalf("OnRe top-up swap lacks the position return: %+v", plan.Exit)
	}
	// Leg: allocate idle Voltr cash into (empty) Squads.
	o.Snapshot.SquadsIdleRaw, o.Snapshot.VoltrIdleRaw = 0, 50_000
	alloc := Decision{Action: VoltrAllocateToSquads, AmountRaw: 40_000, StrategyKey: onreONycUSDC, Reason: topupAllocationReason, IdempotencyKey: "onre-topup"}
	effects, _, _, err := bridgeExpectedEffects(alloc, 50_000, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	effects.Kind, effects.ReturnData = "bridge", expectedAdaptorReturnData(40_000)
	request := bridgeTestRequest(VoltrAllocateToSquads, 40_000)
	request.Report.Sequence, request.Report.ObservedSlot = 42, 42
	if _, err := observePhase3TopupAllocationAdmission(ctx, rpc, client, m, o, alloc, BridgeExecutionEvidence{Request: request, ExpectedEffects: effects}); err != nil {
		t.Fatalf("OnRe top-up allocation refused: %v", err)
	}
}

// The deposit admission and its final-send prestate check accept the funded
// debt-free OnRe obligation only with the journaled top-up reason.
func TestOnReTopupDepositPrestate(t *testing.T) {
	_, _, rpc, _, accounts := usdcReturnFixtureForLane(t, onreONycUSDC)
	route, _ := runtimeRoute(onreONycUSDC)
	obligation := accountAt(accounts, route.Kamino.Obligation)
	clear(obligation.Data[1208:1408])
	// At the deposit leg the swap has consumed all Squads cash, which is
	// also OnRe's debt custody.
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 0)
	position, err := decodeKaminoObligation(obligation, route.Kamino)
	if err != nil || position.collateralDepositedRaw == 0 || position.debtRaw != 0 {
		t.Fatalf("fixture is not a funded debt-free obligation: %v %+v", err, position)
	}
	ctx := context.Background()
	if _, err := validateInitialDepositPrestate(ctx, rpc, route, 42, position.collateralDepositedRaw); err != nil {
		t.Fatalf("top-up deposit prestate refused: %v", err)
	}
	if _, err := validateInitialDepositPrestate(ctx, rpc, route, 42, 0); err == nil {
		t.Fatal("initial-deposit prestate accepted a funded obligation")
	}
	if _, err := validateInitialDepositPrestate(ctx, rpc, route, 42, position.collateralDepositedRaw+1); err == nil {
		t.Fatal("top-up deposit prestate accepted a changed position")
	}
	// Squads cash left beside the deposit (it would be debt cash) refuses.
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 1)
	if _, err := validateInitialDepositPrestate(ctx, rpc, route, 42, position.collateralDepositedRaw); err == nil {
		t.Fatal("deposit accepted with Squads cash beside it")
	}
}
