package backyardrwa

import (
	"context"
	"encoding/binary"
	"testing"
)

// The live state after the payoff and both earlier top-up legs: 1x AUTO,
// no debt, no working cash, $1,295 idle in Voltr, PYUSD borrowing blocked.
func liveTopupSnapshot() Snapshot {
	s := liveIdleDebtSnapshot()
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS, s.DebtIdleRaw = 0, 0, 0, 0, 0
	s.PilotActive, s.BorrowUtilizationBlocked, s.TopupDepositRoomRaw = true, true, 5_000_000_000
	// Production stamps the reviewed candidate lane for the pilot tranche.
	s.PilotTrancheCapLane = autoAUTOPYUSD.Lane
	return s
}

func TestTopupAllocationSizingAndPriority(t *testing.T) {
	s := liveTopupSnapshot()
	got := Decide(s)
	if got.Action != VoltrAllocateToSquads || got.Reason != topupAllocationReason || got.AmountRaw != s.VoltrIdleRaw || got.Validate() != nil {
		t.Fatalf("idle cash was not topped up: %+v", got)
	}
	// Size = min(idle - buffer, working tranche cap, deposit-limit room).
	room := s
	room.TopupDepositRoomRaw = 700_000_000
	if got := Decide(room); got.AmountRaw != 700_000_000 {
		t.Fatalf("deposit-limit room ignored: %+v", got)
	}
	capped := s
	capped.VoltrIdleRaw, capped.TopupDepositRoomRaw = PilotWorkingTrancheCapRaw+5, PilotWorkingTrancheCapRaw+5
	if got := Decide(capped); got.AmountRaw != PilotWorkingTrancheCapRaw {
		t.Fatalf("working tranche cap ignored: %+v", got)
	}
	// Open borrowing does not jump ahead of idle cash: top up first, then the
	// borrow levers the whole collateral.
	open := s
	open.BorrowUtilizationBlocked = false
	if got := Decide(open); got.Reason != topupAllocationReason {
		t.Fatalf("borrow preempted the top-up: %+v", got)
	}
	open.VoltrIdleRaw, open.LeverageTargetLevel = 0, 1.5
	if got := Decide(open); got.Action != OpenRouteStep || got.Reason != leverageUpReason {
		t.Fatalf("no borrow after the top-up is done: %+v", got)
	}
	// The selector-entry pause does not block adding to the current loop.
	paused := s
	paused.SelectorEntryPaused = true
	if got := Decide(paused); got.Reason != topupAllocationReason {
		t.Fatalf("paused selector entry blocked the top-up: %+v", got)
	}
	for name, mutate := range map[string]func(*Snapshot){
		"uncovered demand": func(s *Snapshot) { s.WithdrawalDemandRaw = s.VoltrIdleRaw + 1 },
		"covered demand":   func(s *Snapshot) { s.WithdrawalDemandRaw = 1 },
		"unwind":           func(s *Snapshot) { s.Unwind = true },
		"hard ltv":         func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS, s.DebtIdleRaw = 1, 1, 6000, 1 },
		"report due":       func(s *Snapshot) { s.PostMutationNAVRequired = true },
		"nonterminal":      func(s *Snapshot) { s.Nonterminal = Built },
		"no pilot":         func(s *Snapshot) { s.PilotActive = false },
		"no room":          func(s *Snapshot) { s.TopupDepositRoomRaw = 0 },
		"legacy tranche":   func(s *Snapshot) { s.PilotTrancheCapLane = "" },
		"dust":             func(s *Snapshot) { s.VoltrIdleRaw = topupMinimumRaw - 1 },
		"debt":             func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw = 1, 1 },
		"flat":             func(s *Snapshot) { s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = false, 0, 0 },
	} {
		c := s
		mutate(&c)
		if got := Decide(c); got.Reason == topupAllocationReason {
			t.Fatalf("%s: top-up allocation selected: %+v", name, got)
		}
	}
}

func TestSelectorEntryFenceBypassedOnlyForJournaledTopupAllocation(t *testing.T) {
	allocation := BridgeBuildRequest{Action: VoltrAllocateToSquads, AmountRaw: 1}
	if !topupAllocationBypassesEntryFence(allocation, topupAllocationReason, false) {
		t.Fatal("journaled top-up allocation still needs a selector entry")
	}
	for name, c := range map[string]struct {
		request   any
		reason    string
		unwinding bool
	}{
		"entry allocation": {allocation, "eligible_voltr_idle", false},
		"no reason":        {allocation, "", false},
		"unwinding":        {allocation, topupAllocationReason, true},
		"report":           {BridgeBuildRequest{Action: ReportNAV}, topupAllocationReason, false},
		"borrow":           {KaminoPrimeUSDCRequest{Action: OpenRouteStep}, topupAllocationReason, false},
		"initializer":      {KaminoInitializationRequest{}, topupAllocationReason, false},
	} {
		if topupAllocationBypassesEntryFence(c.request, c.reason, c.unwinding) {
			t.Fatalf("%s bypassed the selector entry fence", name)
		}
	}
}

func TestTopupAllocationAdmissionReservesPositionReturnWithAllocatedCash(t *testing.T) {
	o, _, _, m, rpc, client, accounts := payoffAdmissionFixture(t, 20_000)
	clear(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[1208:1408])
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 0)
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.DebtIdleRaw, o.Snapshot.PayoffDebtRaw = 0, 0, 0, 0
	o.Snapshot.VoltrIdleRaw = 50_000
	d := Decision{Action: VoltrAllocateToSquads, AmountRaw: 40_000, StrategyKey: o.Snapshot.RouteLane, Reason: topupAllocationReason, IdempotencyKey: "topup-allocation"}
	effects, _, _, err := bridgeExpectedEffects(d, 50_000, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	effects.Kind, effects.ReturnData = "bridge", expectedAdaptorReturnData(40_000)
	request := bridgeTestRequest(VoltrAllocateToSquads, 40_000)
	request.Report.Sequence, request.Report.ObservedSlot = 42, 42
	e := BridgeExecutionEvidence{Request: request, ExpectedEffects: effects}
	plan, err := observePhase3TopupAllocationAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	var actions []Action
	var total int64
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
		total += step.Cost.TotalMicros
	}
	if len(actions) < 6 || actions[0] != ReportNAV || actions[1] != DeleverRouteStep || total != plan.ExitAfterMicros || plan.Snapshot != o.Snapshot || plan.ValidThroughSlot > 42+adaptorMaxReportAgeSlots {
		t.Fatalf("allocation lacks the complete position return: %v", actions)
	}
	var staged bool
	for _, step := range plan.Exit {
		if step.Action == StageSquadsToVoltr && step.Amount >= 40_000+plan.QuotedExit.EstimatedUpperOutputRaw {
			staged = true
		}
	}
	if !staged {
		t.Fatal("return did not stage the allocated cash with the position proceeds")
	}
	// The plain entry path still refuses a funded position.
	if _, err := observePhase3BridgeAdmission(context.Background(), rpc, o, d, e); err == nil {
		t.Fatal("plain bridge admission accepted a funded position")
	}
	for name, mutate := range map[string]func(*Observation, *Decision){
		"reason":      func(_ *Observation, d *Decision) { d.Reason = "eligible_voltr_idle" },
		"debt":        func(o *Observation, _ *Decision) { o.Snapshot.PositionDebtRaw = 1 },
		"demand":      func(o *Observation, _ *Decision) { o.Snapshot.WithdrawalDemandRaw = 1 },
		"squads cash": func(o *Observation, _ *Decision) { o.Snapshot.SquadsIdleRaw = 1 },
		"flat":        func(o *Observation, _ *Decision) { o.Snapshot.HasPosition = false },
		"over idle":   func(o *Observation, _ *Decision) { o.Snapshot.VoltrIdleRaw = 39_999 },
	} {
		bo, bd := o, d
		mutate(&bo, &bd)
		if _, err := observePhase3TopupAllocationAdmission(context.Background(), rpc, client, m, bo, bd, e); err == nil {
			t.Fatalf("%s: unsafe top-up allocation admitted", name)
		}
	}
}
