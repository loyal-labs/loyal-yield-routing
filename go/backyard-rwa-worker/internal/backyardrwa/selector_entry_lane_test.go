package backyardrwa

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestCanaryRequestCannotTargetDeferredLane(t *testing.T) {
	request := func(lane string) string {
		raw, _ := json.Marshal(pilotCanaryEntryRequest{ID: sha256Bytes([]byte("canary-acceptance")), Lane: lane, EquityRaw: 1_000_000, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)})
		return string(raw)
	}
	for _, lane := range []string{PhaseOneLaneID, "OnRe/ONyc/USDC"} {
		t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", request(lane))
		_, err := readPilotCanaryEntryRequest(time.Now().UTC())
		assertBudgetHold(t, err, "invalid_pilot_canary_request")
	}
	t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", request(SelectedRouteID))
	got, err := readPilotCanaryEntryRequest(time.Now().UTC())
	if err != nil || got == nil || got.Lane != SelectedRouteID {
		t.Fatal("permitted canary request rejected", err, got)
	}
	// An in-memory deferred request (prior durable state) is rejected by the
	// selection-time validation as well.
	in := selectorFixture()
	in.Policy = DefaultSelectorPolicy()
	in.canaryRequest = &pilotCanaryEntryRequest{ID: sha256Bytes([]byte("deferred-acceptance")), Lane: in.Markets[0].Lane, EquityRaw: int64(in.Snapshot.TotalVaultNAVRaw), ExpiresAt: in.Now.Add(10 * time.Minute)}
	result, receipt, err := selectPilotCanaryEntry(in, SelectOpportunity(in, SelectorState{}), nil)
	assertBudgetHold(t, err, "invalid_pilot_canary_request")
	if receipt != nil || result.Action == "CANARY_ENTER" {
		t.Fatal("deferred canary accepted", result, receipt)
	}
}

func TestFundedDeferredTrancheCompletes(t *testing.T) {
	now := time.Now().UTC()
	entry := selectorEntryFixture(now, PhaseOneLaneID, 1_000_000)
	entry.AllocationOperationID = "funded-before-revision"
	s := base()
	s.Slot = entry.Quote.SampleSlot
	s.PilotActive = true
	s.RouteLane, s.StrategyKey = PhaseOneLaneID, PhaseOneLaneID
	s.VoltrIdleRaw = 100_000_000
	s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 10_000_000, 10_000_000, 10_000_000
	// Funded: the quoted equity already left Voltr and the quote window lapsed.
	s.VoltrIdleRaw -= entry.EquityRaw
	s.SquadsIdleRaw = entry.EquityRaw
	s.Slot = entry.Quote.ValidThroughSlot + 1
	if err := applySelectorEntry(&s, &entry, now); err != nil {
		t.Fatal(err)
	}
	if d := Decide(s); d.Action != SwapStableToCollateralStep {
		t.Fatal("funded deferred tranche cannot complete", d)
	}
	borrow, err := selectorBorrowAmount(s, entry.Quote.BorrowReceiveRaw)
	if err != nil || borrow != entry.Quote.BorrowReceiveRaw {
		t.Fatal("funded deferred borrow lost reviewed amount", borrow, err)
	}
}

func TestSelectorEntryFenceRejectsDeferredNewEntry(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("selector-lane-%d", time.Now().UnixNano())
	entry := selectorEntryFixture(time.Now().UTC(), PhaseOneLaneID, 1_000_000)
	raw, _ := json.Marshal(map[string]any{"selectorEntry": entry})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,$2)`, key, raw); err != nil {
		t.Fatal(err)
	}
	for _, op := range []struct{ id, action string }{
		{key + "-alloc", "VOLTR_ALLOCATE_TO_SQUADS"}, {key + "-init", "INITIALIZE_OBLIGATION"}, {key + "-borrow", "OPEN_ROUTE_STEP"},
	} {
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'failed',$3,$4,'{}')`, op.id, key, op.action, PhaseOneLaneID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AcquireRouteLease(ctx, key, "entry-lane-fence", time.Minute); err != nil {
		t.Fatal(err)
	}
	budget := Phase3Budget{Pilot: &pilotBudgetAuthority{}}
	allocation := BridgeBuildRequest{Action: VoltrAllocateToSquads, AmountRaw: 1_000_000}
	initializer := KaminoInitializationRequest{RouteLane: PhaseOneLaneID}
	run := func(operationID string, admission bool, request any, effects ExpectedEffects, slot int64, want string) {
		t.Helper()
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.lockOperationLease(ctx, tx, operationID); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		err = db.authorizeSelectorEntryTx(ctx, tx, operationID, budget, request, effects, slot, admission)
		_ = tx.Rollback(ctx)
		if want != "" {
			assertBudgetHold(t, err, want)
		} else if err != nil {
			t.Fatal(err)
		}
	}
	// Fresh, unbound, and bound-but-not-broadcast deferred authority is refused
	// at admission and at build/send; binding alone is not a funded tranche.
	run(key+"-alloc", true, allocation, ExpectedEffects{}, entry.Quote.SampleSlot, "selector_entry_lane_deferred")
	run(key+"-alloc", false, allocation, ExpectedEffects{}, entry.Quote.SampleSlot, "selector_entry_lane_deferred")
	run(key+"-init", false, initializer, ExpectedEffects{}, entry.Quote.SampleSlot, "selector_entry_lane_deferred")
	bound := entry
	bound.AllocationOperationID = key + "-alloc"
	storeTestSelectorEntry(t, ctx, db, key, bound)
	run(key+"-alloc", true, allocation, ExpectedEffects{}, entry.Quote.SampleSlot, "selector_entry_lane_deferred")
	run(key+"-alloc", false, allocation, ExpectedEffects{}, entry.Quote.SampleSlot, "selector_entry_lane_deferred")
	// Funded completion through the borrow leg keeps its existing guards.
	request, err := basicPolicyFixtureManifest(t).kaminoPacketForRoute(OpenRouteStep, kaminoLegBorrow, 500_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, PhaseOneLaneID)
	if err != nil {
		t.Fatal(err)
	}
	// Minimal exact borrow graph: the fence only measures the executable debit
	// against the reviewed amount and fee, so no route account graph is needed.
	route, _ := runtimeRoute(PhaseOneLaneID)
	source, destination := kaminoLegCustodiesForRoute(kaminoLegBorrow, route)
	effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-borrow", Conserved: true, Accounts: []ExpectedAccountEffect{
		{Address: source.Address, Mint: source.Mint, Authority: source.Authority, Owner: route.DebtTokenProgram, BeforeRaw: 500_000, AfterRaw: 0},
		{Address: destination.Address, Mint: destination.Mint, Authority: destination.Authority, Owner: route.DebtTokenProgram, BeforeRaw: 0, AfterRaw: 500_000},
		{Address: route.DebtFeeReceiver, Mint: route.Kamino.DebtMint, Authority: route.Kamino.MarketAuthority, Owner: route.DebtTokenProgram, BeforeRaw: 0, AfterRaw: 0},
	}}
	run(key+"-borrow", true, request, effects, 1000, "")
	run(key+"-borrow", false, request, effects, 1000, "")
	if _, err = db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	// Control: the permitted lane is unaffected.
	mapleKey := key + "-maple"
	mapleEntry := selectorEntryFixture(time.Now().UTC(), SelectedRouteID, 1_000_000)
	mapleRaw, _ := json.Marshal(map[string]any{"selectorEntry": mapleEntry})
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,$2)`, mapleKey, mapleRaw); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'failed','VOLTR_ALLOCATE_TO_SQUADS',$3,'{}')`, mapleKey+"-alloc", mapleKey, SelectedRouteID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, mapleKey, "entry-lane-fence", time.Minute); err != nil {
		t.Fatal(err)
	}
	defer db.ReleaseRouteLease(ctx)
	mapleAllocation := BridgeBuildRequest{Action: VoltrAllocateToSquads, AmountRaw: 1_000_000}
	run(mapleKey+"-alloc", true, mapleAllocation, ExpectedEffects{}, mapleEntry.Quote.SampleSlot, "")
}
