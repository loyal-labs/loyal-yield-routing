package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
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
	armLeverageCapacityFixture(&open)
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
		"borrowed debt":    func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw, s.DebtIdleRaw = 1, 1, 1 },
		"unvalued debt":    func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw = 1, 0 },
		"flat":             func(s *Snapshot) { s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = false, 0, 0 },
	} {
		c := s
		mutate(&c)
		if got := Decide(c); got.Reason == topupAllocationReason {
			t.Fatalf("%s: top-up allocation selected: %+v", name, got)
		}
	}
}

// Live 2026-10-09 Earn Max: ~$2.1k AUTO collateral against ~$905 PYUSD debt
// (LTV ~43%) with ~$100,005 idle in Voltr held single_loop_position_ready.
func liveDebtTopupSnapshot() Snapshot {
	s := liveTopupSnapshot()
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 2_058_000_000, 2_100_000_000
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS = 905_000_000, 905_000_000, 905_001_000, 4310
	s.VoltrIdleRaw, s.TopupDepositRoomRaw = 100_005_000_000, 500_000_000_000
	return s
}

// The top-up runs beside a debt-bearing AUTO position: allocate, swap, then
// a collateral-only redeposit into the same obligation, then hold. No leg
// borrows or repays; re-levering stays with the leverage rules.
func TestTopupBesideDebtAllocatesSwapsRedepositsThenHolds(t *testing.T) {
	s := liveDebtTopupSnapshot()
	steps := []struct {
		mutate func(*Snapshot)
		action Action
		reason string
		amount int64
	}{
		{func(*Snapshot) {}, VoltrAllocateToSquads, topupAllocationReason, PilotWorkingTrancheCapRaw},
		{func(s *Snapshot) { s.VoltrIdleRaw, s.SquadsIdleRaw = 5_000_000, PilotWorkingTrancheCapRaw }, SwapStableToCollateralStep, topupSwapReason, PilotWorkingTrancheCapRaw},
		{func(s *Snapshot) {
			s.SquadsIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 0, 97_900_000_000, 97_900_000_000, 99_900_000_000
		}, OpenRouteStep, "single_loop_redeposit", 97_900_000_000},
		{func(s *Snapshot) {
			s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 0, 0, 0
			s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.LTVBPS = 99_958_000_000, 102_000_000_000, 88
		}, Hold, "single_loop_position_ready", 0},
	}
	for i, step := range steps {
		step.mutate(&s)
		got := Decide(s)
		if got.Action != step.action || got.Reason != step.reason || got.AmountRaw != step.amount || got.Validate() != nil {
			t.Fatalf("step %d: %+v", i, got)
		}
	}
	// Sizing is the debt-free tranche rule: deposit-limit room still binds.
	room := liveDebtTopupSnapshot()
	room.TopupDepositRoomRaw = 700_000_000
	if got := Decide(room); got.Reason != topupAllocationReason || got.AmountRaw != 700_000_000 {
		t.Fatalf("deposit-limit room ignored beside debt: %+v", got)
	}
	// Idle collateral beside debt is the leverage loop's: it is redeposited
	// before any Squads cash is swapped.
	loop := liveDebtTopupSnapshot()
	loop.SquadsIdleRaw, loop.CollateralIdleRaw, loop.PrimeIdleRaw = 1_000_000, 35_000_000, 35_000_000
	if got := Decide(loop); got.Action != OpenRouteStep || got.Reason != "single_loop_redeposit" || got.AmountRaw != 35_000_000 {
		t.Fatalf("loop collateral beside top-up cash: %+v", got)
	}
	for name, c := range map[string]struct {
		mutate func(*Snapshot)
		reason string
	}{
		// Borrowed PYUSD is the leverage loop's, never a top-up.
		"debt idle": {func(s *Snapshot) { s.DebtIdleRaw = s.PositionDebtRaw }, "borrowed_debt_requires_collateral_buffer"},
		// Ethena is no leverage lane: debt keeps the installed hold.
		"non-leverage lane": {func(s *Snapshot) { s.RouteLane, s.StrategyKey = ethenaUSDePYUSD.Lane, ethenaUSDePYUSD.Lane }, "single_loop_position_ready"},
	} {
		d := liveDebtTopupSnapshot()
		c.mutate(&d)
		if got := Decide(d); got.Reason != c.reason {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	// OnRe debt is Squads USDC: borrowed cash there still goes to the
	// leverage swap, never the top-up swap.
	onre := liveDebtTopupSnapshot()
	onre.RouteLane, onre.StrategyKey, onre.PilotTrancheCapLane, onre.SquadsIdleRaw = onreONycUSDC, onreONycUSDC, onreONycUSDC, 1_000_000
	if got := Decide(onre); got.Action != SwapDebtToCollateralStep || got.Reason != "borrowed_usdc_requires_prime_buffer" {
		t.Fatalf("OnRe borrowed cash became a top-up: %+v", got)
	}
}

// autoDebtTopupFixture is the owned AUTO batch (pilot, PYUSD debt, released
// collateral) observed by the production observer, reshaped to the top-up
// state: no withdrawal or stage in flight, empty Squads, collateral and debt
// custody, and $50 idle in Voltr.
func autoDebtTopupFixture(t *testing.T) (Observation, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, rpc, client, accounts := partialWithdrawalRestoreFixture(t, autoAUTOPYUSD.Lane, int64(autoFixtureDebtRaw))
	for _, address := range []string{bridgeStrategyATA, bridgeSquadsATA, autoAUTOPYUSD.CollateralCustody, autoAUTOPYUSD.DebtCustody} {
		binary.LittleEndian.PutUint64(accountAt(accounts, address).Data[64:72], 0)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeIdleATA).Data[64:72], 50_000_000)
	s := &o.Snapshot
	s.PartialWithdrawalOperationID, s.PartialWithdrawalLTVBPS, s.LeverageTargetLevel, s.WithdrawalDemandRaw = "", 0, 0, 0
	s.VoltrStrategyIdleRaw, s.StagedAmountRaw, s.StagedAmountKnown, s.StageTransient = 0, 0, false, false
	s.SquadsIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw, s.DebtIdleRaw, s.VoltrIdleRaw = 0, 0, 0, 0, 50_000_000
	if !s.HasPosition || s.PositionDebtRaw <= 0 || s.PositionDebtValueRaw <= 0 || !s.PilotActive {
		t.Fatalf("fixture lost the debt-bearing AUTO position: %+v", *s)
	}
	return o, m, rpc, client, accounts
}

// debtTopupAllocation is the $40 top-up allocation out of the fixture's $50
// Voltr idle.
func debtTopupAllocation(t *testing.T, o Observation) (Decision, BridgeExecutionEvidence) {
	t.Helper()
	d := Decision{Action: VoltrAllocateToSquads, AmountRaw: 40_000_000, StrategyKey: o.Snapshot.RouteLane, Reason: topupAllocationReason, IdempotencyKey: "topup-allocation-debt"}
	effects, _, _, err := bridgeExpectedEffects(d, uint64(o.Snapshot.VoltrIdleRaw), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	effects.Kind, effects.ReturnData = "bridge", expectedAdaptorReturnData(40_000_000)
	request := bridgeTestRequest(VoltrAllocateToSquads, 40_000_000)
	request.Report.Sequence, request.Report.ObservedSlot = uint64(o.Snapshot.Slot), uint64(o.Snapshot.Slot)
	return d, BridgeExecutionEvidence{Request: request, ExpectedEffects: effects}
}

// Beside debt the allocation reserves the complete projected return of the
// whole position (release, funding swap, payoff, withdrawal, conversions)
// and stages the allocated Squads cash with the proceeds.
func TestTopupAllocationAdmissionBesideDebtReservesProjectedReturnWithAllocatedCash(t *testing.T) {
	o, m, rpc, client, accounts := autoDebtTopupFixture(t)
	d, e := debtTopupAllocation(t, o)
	request, effects := e.Request, e.ExpectedEffects
	before := append([]byte(nil), accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation).Data...)
	plan, err := observePhase3TopupAllocationAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	if plan.QuotedExit == nil {
		t.Fatal("return lost its collateral conversion")
	}
	// Stage exactly the allocated cash plus every conversion's upper output.
	proceeds := plan.QuotedExit.EstimatedUpperOutputRaw
	for _, quoted := range plan.AdditionalQuotedExits {
		proceeds += quoted.EstimatedUpperOutputRaw
	}
	var total int64
	staged, repaid := false, false
	for _, step := range plan.Exit {
		total += step.Cost.TotalMicros
		if step.Action == StageSquadsToVoltr && step.Amount == 40_000_000+proceeds {
			staged = true
		}
		if step.Action == DeleverRouteStep && plan.PayoffRepayment != nil && step.Template == plan.PayoffRepayment {
			repaid = true
		}
	}
	if !staged || !repaid || total != plan.ExitAfterMicros || plan.Payoff == nil || plan.PayoffWithdrawal == nil || plan.Snapshot != o.Snapshot ||
		plan.ValidThroughSlot > o.Snapshot.Slot+adaptorMaxReportAgeSlots {
		t.Fatalf("allocation beside debt lacks the complete return with its cash: staged=%t repaid=%t", staged, repaid)
	}
	current, currentEffects, _, err := plan.Input.decodeWithManifest(m)
	if err != nil || current != request || !reflect.DeepEqual(currentEffects, effects) {
		t.Fatal("exit pricing replaced the current allocation", err)
	}
	if !bytes.Equal(before, accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation).Data) {
		t.Fatal("exit pricing mutated observed position accounts")
	}
	// The production dispatcher reaches persistence with this admission.
	productionJupiter = client
	t.Cleanup(func() { productionJupiter = nil })
	assertBudgetHold(t, productionTickRuntime(&Database{}, rpc, m, Credentials{}).admitBridge(context.Background(), "topup-debt", o, d, e), "bridge_admission_database_unavailable")
	for name, mutate := range map[string]func(*Observation){
		"debt idle":       func(o *Observation) { o.Snapshot.DebtIdleRaw = 1 },
		"collateral idle": func(o *Observation) { o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw = 1, 1 },
		"unvalued debt":   func(o *Observation) { o.Snapshot.PositionDebtValueRaw = 0 },
		"demand":          func(o *Observation) { o.Snapshot.WithdrawalDemandRaw = 1 },
		"position moved":  func(o *Observation) { o.Snapshot.PositionCollateralRaw++ },
	} {
		bad := o
		mutate(&bad)
		if _, err := observePhase3TopupAllocationAdmission(context.Background(), rpc, client, m, bad, d, e); err == nil {
			t.Fatalf("%s: unsafe allocation beside debt admitted", name)
		}
	}
}

// The production dispatcher persists the allocation beside debt: the
// family's reserved exit becomes the complete projected return (never less
// than the prior position reserve), as an entry rather than a recovery, and
// no wire or send authority is created.
func TestTopupAllocationBesideDebtProductionAdmissionDB(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 60*time.Second)
	defer cancel()
	defer db.Close()
	o, m, rpc, client, _ := autoDebtTopupFixture(t)
	d, e := debtTopupAllocation(t, o)
	key := fmt.Sprintf("topup-debt-%d", time.Now().UnixNano())
	id := key + "-allocation"
	stateValue := planningPilotState(t)
	budget := stateValue["phase3"].(Phase3Budget)
	family := phase3BudgetFamilyForLane(d.StrategyKey)
	priorReserve := int64(1_000_000)
	row := budget.Families[family]
	row.ExitMicros = priorReserve
	budget.Families[family] = row
	stateValue["phase3"] = budget
	state, _ := json.Marshal(stateValue)
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2::jsonb,2)`, key, string(state)); err != nil {
		t.Fatal(err)
	}
	defer db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key)
	defer db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key=$1`, key)
	if _, err := db.AcquireRouteLease(ctx, key, "topup-debt-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	envelope, _ := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, d, m.SHA256, *m.PolicyCatalog.SHA256)})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5::jsonb)`, id, key, d.Action, d.StrategyKey, string(envelope)); err != nil {
		t.Fatal(err)
	}
	productionJupiter = client
	t.Cleanup(func() { productionJupiter = nil })
	if err := productionTickRuntime(db, rpc, m, Credentials{}).admitBridge(ctx, id, o, d, e); err != nil {
		t.Fatal(err)
	}
	var rawAuth, rawBudget []byte
	var hasWire, hasSend bool
	if err := db.pool.QueryRow(ctx, `SELECT o.expected_effects->'phase3',s.state->'phase3',o.signed_wire IS NOT NULL,o.broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states s USING(route_key) WHERE operation_id=$1`, id).Scan(&rawAuth, &rawBudget, &hasWire, &hasSend); err != nil {
		t.Fatal(err)
	}
	var auth phase3OperationAuthorization
	var after Phase3Budget
	if json.Unmarshal(rawAuth, &auth) != nil || json.Unmarshal(rawBudget, &after) != nil || auth.BridgeAdmission == nil || auth.BuildInput == nil || hasWire || hasSend {
		t.Fatal("missing durable admission or unexpected send authority")
	}
	current, _, _, err := auth.BuildInput.decodeWithManifest(m)
	plan, reservation := auth.BridgeAdmission, after.Reservations[id]
	if err != nil || current != e.Request || plan.Payoff == nil || plan.PayoffRepayment == nil || plan.Snapshot != o.Snapshot {
		t.Fatal("admission changed the allocation or dropped the payoff", err)
	}
	if reservation.Recovery || reservation.ExitBeforeMicros != priorReserve || reservation.ExitAfterMicros != plan.ExitAfterMicros ||
		plan.ExitAfterMicros <= priorReserve || after.Families[family].ExitMicros != plan.ExitAfterMicros {
		t.Fatalf("reserved exit is not the complete projected return: %+v", reservation)
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
