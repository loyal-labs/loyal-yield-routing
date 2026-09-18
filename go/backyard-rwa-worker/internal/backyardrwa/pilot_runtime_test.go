package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestPilotPlannerSizesOneTrancheAndPreservesExitPriority(t *testing.T) {
	for _, lane := range selectorLanes {
		s := base()
		s.RouteLane = lane
		s.StrategyKey = lane
		s.PilotActive = true
		s.SelectorEntryEquityRaw = PilotWorkingTrancheCapRaw
		s.VoltrIdleRaw = PilotWorkingTrancheCapRaw
		s.CapacityRaw = PilotWorkingTrancheCapRaw
		s.PolicyLimitRaw = PilotWorkingTrancheCapRaw
		s.MaxTargetLTVEntryRaw = PilotWorkingTrancheCapRaw
		d := Decide(s)
		if d.Action != VoltrAllocateToSquads || d.AmountRaw != PilotWorkingTrancheCapRaw || d.Validate() != nil {
			t.Fatalf("pilot %s sizing: %+v", lane, d)
		}
		s.PilotActive = false
		if legacy := Decide(s); legacy.AmountRaw != Phase3WorkingTrancheCapRaw {
			t.Fatalf("legacy changed: %+v", legacy)
		}
		s.PilotActive = true
		s.CapacityRaw = 3_000_000
		if stale := Decide(s); stale.Action != Hold {
			t.Fatalf("shrinking capacity reused larger quote: %+v", stale)
		}
		s.SelectorEntryEquityRaw = 3_000_000
		if sized := Decide(s); sized.AmountRaw != 3_000_000 {
			t.Fatalf("capacity ignored: %+v", sized)
		}
		s.VoltrIdleRaw = 0
		s.SquadsIdleRaw = 10_000_000
		s.Unwind = true
		d = Decide(s)
		if d.Action != StageSquadsToVoltr || d.AmountRaw != 10_000_000 {
			t.Fatalf("pilot full return clamped: %+v", d)
		}
		s.Unwind = false
		s.CapacityRaw = 1
		if d = Decide(s); d.Action != StageSquadsToVoltr || d.AmountRaw != 10_000_000 {
			t.Fatalf("capacity race strands cash: %+v", d)
		}
	}
}

func TestPilotWithdrawalPreservesFullExitAmount(t *testing.T) {
	d := Decision{Action: DeleverRouteStep, StrategyKey: SelectedRouteID, Reason: "withdrawal_withdraw_collateral"}
	p := KaminoPosition{HasPosition: true, CollateralDepositedRaw: 12_000_000, RedeemablePrimeRaw: 15_000_000}
	leg, receipt, liquidity, err := selectKaminoLeg(true, d, p)
	if err != nil || leg != kaminoLegWithdraw || receipt != 12_000_000 || liquidity != 15_000_000 {
		t.Fatalf("pilot exit clipped by canary: %d %d %d %v", leg, receipt, liquidity, err)
	}
	_, _, legacy, err := selectKaminoLeg(false, d, p)
	if err != nil || legacy > uint64(Phase2TransactionCapRaw) {
		t.Fatalf("legacy cap changed: %d %v", legacy, err)
	}
}

type pilotPlanningJournal struct {
	stubProductionJournal
	active bool
	err    error
}

func (p *pilotPlanningJournal) PilotRuntimeEnabled(context.Context, string) (bool, error) {
	return p.active, p.err
}
func TestPilotAuthorityEnrichesEveryPlanningSnapshot(t *testing.T) {
	journal := &pilotPlanningJournal{active: true}
	state := productionObserveState{routeKey: "test", journal: journal}
	o := Observation{}
	if err := state.mergeJournal(context.Background(), &o); err != nil || !o.Snapshot.PilotActive {
		t.Fatal("outer/preparation merge omitted pilot authority", err)
	}
	journal.active = false
	if err := state.mergeJournal(context.Background(), &o); err != nil || o.Snapshot.PilotActive {
		t.Fatal("merge retained old authority", err)
	}
	journal.err = budgetHold("incoherent_pilot_activation_evidence")
	if err := state.mergeJournal(context.Background(), &o); err == nil {
		t.Fatal("corrupt authority silently fell back")
	}
}

func TestPilotRuntimeRequiresPersistedVerifiedAuthority(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 20*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("pilot-runtime-planning-%d", time.Now().UnixNano())
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{}',2)`, key); err != nil {
		t.Fatal(err)
	}
	if active, err := db.PilotRuntimeEnabled(ctx, key); err != nil || active {
		t.Fatal("missing budget enabled pilot", err)
	}
	prior := emptyTestBudget()
	previous, _ := json.Marshal(prior)
	flat := pilotFlatFixture(t)
	flatJSON, _ := json.Marshal(flat)
	a := pilotTestAuthority(prior)
	a.Generation = 2
	a.FinalizedSlot = flat.Slot
	a.FlatEvidenceSHA256 = sha256Bytes(flatJSON)
	b, err := activatePilotBudget(prior, a)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"phase3": b, "pilotBudgetActivation": pilotBudgetActivation{a, previous, flat}}
	put := func() {
		t.Helper()
		raw, _ := json.Marshal(state)
		if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$2 WHERE route_key=$1`, key, raw); err != nil {
			t.Fatal(err)
		}
	}
	put()
	if active, err := db.PilotRuntimeEnabled(ctx, key); err != nil || !active {
		t.Fatal("valid pilot not projected", err)
	}
	b.Closed = true
	state["phase3"] = b
	put()
	if active, err := db.PilotRuntimeEnabled(ctx, key); err != nil || active {
		t.Fatal("closed authority enables pilot", err)
	}
	b.Closed = false
	state["phase3"] = b
	delete(state, "pilotBudgetActivation")
	put()
	_, err = db.PilotRuntimeEnabled(ctx, key)
	assertBudgetHold(t, err, "incoherent_pilot_activation_evidence")
	state["phase3"] = nil
	state["pilotBudgetActivation"] = map[string]any{"stale": true}
	put()
	_, err = db.PilotRuntimeEnabled(ctx, key)
	assertBudgetHold(t, err, "pilot_marker_without_budget")
}
