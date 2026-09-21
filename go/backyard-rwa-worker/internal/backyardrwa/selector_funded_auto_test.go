package backyardrwa

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// fundedAutoFixture builds a pilot-sized Maple→AUTO candidate with real bound
// PYUSD debt and AUTO collateral price evidence: the exact priced quote shape
// entry admission and the priced persistence window consume. The quote is
// fresh at the fixture's slot and re-freshed by advanceSelectorFixture.
func fundedAutoFixture(t *testing.T, debtPrice, collateralPrice BudgetPrice) SelectorInput {
	t.Helper()
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.PilotActive = true
	in.Snapshot.Slot = 42
	in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = 100_000_000, 100_000_000
	market := in.Markets[0]
	market.Lane = testAutoLane
	market.EntryCapacity = Capacity{Known: true, Raw: 10_000_000}
	in.Markets = []LaneEconomics{market}
	debt, collateral := debtPrice, collateralPrice
	quote := MoveQuote{
		MinimumIdleRaw: uint64(in.Snapshot.TotalVaultNAVRaw), BorrowReceiveRaw: 5_000_000, BorrowFeeRaw: 10_000,
		DebtPrice: &debt, CollateralAssetPrice: &collateral, RedepositCollateralRaw: 5_000_000,
		SourceLane: in.Snapshot.RouteLane, DestinationLane: testAutoLane, ObservationID: in.Snapshot.ObservationID,
		EquityRaw: 10_000_000, CostRaw: 10_000, ObservedAt: in.Now,
		EvidenceID: sha256Bytes([]byte("auto-priced-quote")), SampleSlot: in.Snapshot.Slot, ValidThroughSlot: in.Snapshot.Slot + budgetMaxObservationLagSlots,
	}
	in.Quotes = []MoveQuote{quote}
	return in
}

// fundedAutoSourceFixture builds the funded follow-up state: an AUTO position
// is the production route, so the selector must keep its economics, hold the
// same-lane baseline, and stay able to unwind. The snapshot carries the
// worker's reviewed approved-cap stamp (Snapshot.PilotTrancheCapLane), so the
// source lane sizes at the pilot tranche exactly as the live stamp does.
func fundedAutoSourceFixture(t *testing.T, debtPrice, collateralPrice BudgetPrice) SelectorInput {
	t.Helper()
	in := fundedAutoFixture(t, debtPrice, collateralPrice)
	s := in.Snapshot
	s.RouteLane, s.StrategyKey = testAutoLane, testAutoLane
	s.PilotTrancheCapLane = testAutoLane
	s.HasPosition = true
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 4_000_000, 4_000_000
	s.PositionDebtRaw, s.PositionDebtValueRaw = 1_000_000, 1_000_000
	s.StrategyNAVRaw = 3_000_000
	s.VoltrIdleRaw = 0
	in.Snapshot = s
	// The destination candidate: a profitable installed lane quoted from the
	// AUTO source at the pilot tranche, carrying the exact bounded exit for
	// the live position.
	source := in.Markets[0]
	destination := source
	destination.Lane = "OnRe/ONyc/USDC"
	destination.NativeAPY = .5
	in.Markets = []LaneEconomics{source, destination}
	quote := MoveQuote{
		MinimumIdleRaw: 100_000_000, BorrowReceiveRaw: 5_000_000,
		SourceExit: &selectorExitBound{MaxCollateralRaw: 4_000_000, MaxDebtRaw: 1_000_000, GrossMicros: 30_000},
		SourceLane: testAutoLane, DestinationLane: destination.Lane, ObservationID: s.ObservationID,
		EquityRaw: 10_000_000, CostRaw: 10_000, ObservedAt: in.Now,
		EvidenceID: sha256Bytes([]byte("auto-source-unwind")), SampleSlot: s.Slot, ValidThroughSlot: s.Slot + budgetMaxObservationLagSlots,
	}
	in.Quotes = []MoveQuote{quote}
	return in
}

func manifestLaneAllowed(m RouteManifest) func(string) bool {
	return func(lane string) bool { return selectorDestinationLaneAuthorized(m, lane) }
}

// manifestFundingAllowed is the new-funding authority: installed entry lanes
// plus the reviewed AUTO lane only.
func manifestFundingAllowed(m RouteManifest) func(string) bool {
	return func(lane string) bool { return m.selectorEntryFundingLane(lane, false) }
}

// The candidate AUTO lane must select through ordinary priced persistence: no
// operator canary, no parity assumption. The first profitable sample opens the
// window, a later sample inside MaxSampleGap at Persistence selects, and a gap
// beyond MaxSampleGap restarts the hysteresis. The public embedded wrapper
// keeps the installed Maple-only closure on the identical input.
func TestAutoCandidateOrdinarySelectionAcrossSamples(t *testing.T) {
	_, debtPrice, _ := autoDebtPriceFixture(t, 1_000_000)
	collateralPrice := autoCollateralPriceFixture(t, 1_000_000)
	manifest := autoInitializerFixtureManifest(t)
	in := fundedAutoFixture(t, debtPrice, collateralPrice)
	if public := SelectOpportunity(in, SelectorState{}); public.Reason != "no_worthwhile_executable_move" || len(public.State.Advantages) != 0 {
		t.Fatal("embedded wrapper changed on candidate input", public)
	}
	result := selectOpportunityWithLanes(in, SelectorState{}, manifestLaneAllowed(manifest), manifestFundingAllowed(manifest))
	if result.Action != "KEEP" || result.Reason != "advantage_not_yet_persistent" {
		t.Fatal("first profitable sample must hold for persistence", result)
	}
	window, ok := result.State.Advantages[testAutoLane]
	if !ok || !window.Since.Equal(in.Now) || !window.LastSample.Equal(in.Now) {
		t.Fatal("priced sample did not open the window", result.State)
	}
	for _, c := range result.Candidates {
		if c.Lane == testAutoLane && (!c.CostsKnown || c.BlockedReason != "" || c.BenefitRaw <= 0) {
			t.Fatal("priced candidate lost its economics", c)
		}
	}
	advanceSelectorFixture(&in, time.Minute)
	result = selectOpportunityWithLanes(in, result.State, manifestLaneAllowed(manifest), manifestFundingAllowed(manifest))
	if result.Action != "ENTER" || result.Reason != "persistent_net_benefit" || result.DestinationLane != testAutoLane || result.SelectedQuote == nil {
		t.Fatal("priced persistence did not select the candidate", result)
	}
	// Hysteresis: a gap beyond MaxSampleGap restarts the window instead of
	// carrying stale persistence into an entry.
	advanceSelectorFixture(&in, 3*time.Minute)
	result = selectOpportunityWithLanes(in, result.State, manifestLaneAllowed(manifest), manifestFundingAllowed(manifest))
	if result.Action != "KEEP" || result.Reason != "advantage_not_yet_persistent" {
		t.Fatal("stale window survived a MaxSampleGap breach", result)
	}
	if window, ok := result.State.Advantages[testAutoLane]; !ok || !window.Since.Equal(in.Now) {
		t.Fatal("restarted window lost its fresh start", result.State)
	}
}

// Full, unknown, deferred and unpriced candidate markets must hold without
// accumulating any persistence: only a complete priced quote samples.
func TestAutoCandidateFullAndUnavailableCapacityHoldWithoutPersistence(t *testing.T) {
	_, debtPrice, _ := autoDebtPriceFixture(t, 1_000_000)
	collateralPrice := autoCollateralPriceFixture(t, 1_000_000)
	manifest := autoInitializerFixtureManifest(t)
	cases := []struct {
		name    string
		mutate  func(*SelectorInput)
		blocked string
	}{
		{"full", func(in *SelectorInput) { in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 0} }, "entry_closed"},
		{"unknown", func(in *SelectorInput) { in.Markets[0].EntryCapacity = Capacity{} }, "pair_capacity_unknown"},
		{"deferred", func(in *SelectorInput) { in.Markets[0].EntryBlockedReason = "lane_entry_deferred" }, "lane_entry_deferred"},
		{"unpriced", func(in *SelectorInput) { in.Quotes[0].DebtPrice = nil }, "bounded_borrow_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fundedAutoFixture(t, debtPrice, collateralPrice)
			tc.mutate(&in)
			result := selectOpportunityWithLanes(in, SelectorState{}, manifestLaneAllowed(manifest), manifestFundingAllowed(manifest))
			if result.Action != "KEEP" || result.Reason != "no_worthwhile_executable_move" {
				t.Fatal("held market reached selection", result)
			}
			var candidate *CandidateForecast
			for i := range result.Candidates {
				if result.Candidates[i].Lane == testAutoLane {
					candidate = &result.Candidates[i]
				}
			}
			if candidate == nil || candidate.BlockedReason != tc.blocked {
				t.Fatal("hold not labeled on the candidate", candidate)
			}
			if len(result.State.Advantages) != 0 {
				t.Fatal("held market accumulated persistence", result.State)
			}
		})
	}
	// An explicit manifest without a resolving binding — the explicit
	// absent-binding fixture, the shipped pre-install state — fails the
	// candidate market closed with the same loud set error.
	absent := autoAbsentBindingManifest(t)
	in := fundedAutoFixture(t, debtPrice, collateralPrice)
	result := selectOpportunityWithLanes(in, SelectorState{}, manifestLaneAllowed(absent), manifestFundingAllowed(absent))
	if result.Action != "KEEP" || len(result.State.Advantages) != 0 {
		t.Fatal("absent binding admitted candidate persistence", result)
	}
	for _, c := range result.Candidates {
		if c.Lane == testAutoLane && c.BlockedReason != "economic_evidence_unavailable" {
			t.Fatal("candidate evidence not refused under absent binding", c)
		}
	}
	// The installed state: the embedded manifest's complete installed binding
	// opens the same persistence window the reviewed initializer fixture does.
	installed := requireEmbeddedInstalledBinding(t)
	installedResult := selectOpportunityWithLanes(in, SelectorState{}, manifestLaneAllowed(installed), manifestFundingAllowed(installed))
	if installedResult.Action != "KEEP" || installedResult.Reason != "advantage_not_yet_persistent" || len(installedResult.State.Advantages) != 1 {
		t.Fatal("installed binding did not open the candidate persistence window", installedResult)
	}
	if window, ok := installedResult.State.Advantages[testAutoLane]; !ok || !window.Since.Equal(in.Now) {
		t.Fatal("installed persistence window drifted", installedResult.State)
	}
}

// A funded AUTO allocation must not strand the selector: the candidate lane as
// production source keeps its keep-gain baseline, holds the same-lane
// reinvestment baseline closed, and can select a safe unwind to a better
// installed lane. The public embedded wrapper still refuses the candidate
// source outright.
func TestAutoSourceKeepsEconomicsAndSelectsSafeUnwind(t *testing.T) {
	_, debtPrice, _ := autoDebtPriceFixture(t, 1_000_000)
	collateralPrice := autoCollateralPriceFixture(t, 1_000_000)
	manifest := autoInitializerFixtureManifest(t)
	in := fundedAutoSourceFixture(t, debtPrice, collateralPrice)
	if public := SelectOpportunity(in, SelectorState{}); public.Reason != "pilot_lane_unavailable" {
		t.Fatal("embedded wrapper admitted a candidate source", public)
	}
	allowed, funding := manifestLaneAllowed(manifest), manifestFundingAllowed(manifest)
	result := selectOpportunityWithLanes(in, SelectorState{}, allowed, funding)
	if result.Reason == "pilot_lane_unavailable" || result.KeepGainRaw <= 0 {
		t.Fatal("AUTO source lost its current economics", result)
	}
	for _, c := range result.Candidates {
		if c.Lane == testAutoLane && c.BlockedReason != "current_position_is_keep_baseline" {
			t.Fatal("same-lane baseline not held for the funded source", c)
		}
	}
	if result.Action != "KEEP" || result.Reason != "advantage_not_yet_persistent" {
		t.Fatal("destination persistence not started from AUTO source", result)
	}
	advanceSelectorFixture(&in, time.Minute)
	result = selectOpportunityWithLanes(in, result.State, allowed, funding)
	if result.Action != "SWITCH" || result.DestinationLane != "OnRe/ONyc/USDC" || result.SelectedQuote == nil || result.SelectedQuote.SourceExit == nil {
		t.Fatal("AUTO source could not select its safe unwind", result)
	}
}

// selectorEntryFundingLane is the rollout authority table: installed lanes
// always; the candidate AUTO lane only while THIS manifest's binding resolves,
// with the initializer onboarding path requiring the complete initialize
// constraint. Seed/address drift keeps every candidate path closed.
func TestSelectorEntryFundingLaneAuthority(t *testing.T) {
	initializer := autoInitializerFixtureManifest(t)
	policyOnly := autoFixtureManifest(t)
	drifted := autoInitializerFixtureManifest(t)
	// Copy the binding VALUE before mutating: the manifest holds a pointer, and
	// the fixture helper's binding must stay untouched for the other cases.
	binding := *drifted.RuntimeBindings.AutoPolicy
	binding.PolicySeed = autoFixtureSeed
	drifted.RuntimeBindings.AutoPolicy = &binding
	if drifted.RuntimeBindings.AutoPolicy.PolicySeed != autoFixtureSeed || initializer.RuntimeBindings.AutoPolicy.PolicySeed != autoInitializerFixtureSeed {
		t.Fatal("drift fixture setup")
	}
	cases := []struct {
		name        string
		manifest    RouteManifest
		maple       bool
		autoFund    bool
		autoInitial bool
	}{
		{"initializer-binding", initializer, true, true, true},
		{"policy-only-binding", policyOnly, true, true, false},
		{"drifted-binding", drifted, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.manifest.selectorEntryFundingLane(SelectedRouteID, false); got != tc.maple {
				t.Fatal("installed lane funding changed", got)
			}
			if got := tc.manifest.selectorEntryFundingLane(testAutoLane, false); got != tc.autoFund {
				t.Fatal("candidate funding authority", got)
			}
			if got := tc.manifest.selectorEntryFundingLane(testAutoLane, true); got != tc.autoInitial {
				t.Fatal("candidate initializer authority", got)
			}
			if got := tc.manifest.selectorEntryFundingLane("Unreviewed/Lane", false); got {
				t.Fatal("unknown lane admitted", got)
			}
		})
	}
}

// The locked production path persists an ordinary candidate entry: priced
// persistence from the route state selects ENTER under the fully reviewed
// manifest binding. The authority is the reviewed binding itself, not the
// manifest pointer: a non-nil manifest without the autoPolicy binding keeps
// the same input closed, as does the embedded public path. A closed KEEP
// persistence still records its diagnostic selector result (the existing
// recordSelectorEvaluation contract), so the closed assertions pin exactly
// what that contract guarantees: no entry, unchanged pause, budget, canary
// history and planning generation, and no state_version bump.
func TestLockedManifestSelectorPersistsCandidateEntry(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	_, debtPrice, _ := autoDebtPriceFixture(t, 1_000_000)
	collateralPrice := autoCollateralPriceFixture(t, 1_000_000)
	manifest := autoInitializerFixtureManifest(t)
	// The explicit absent fixture (the shipped pre-install state) is the
	// closed manifest case; the installed case is the embedded release
	// manifest with its exact installed binding.
	bindingless := autoAbsentBindingManifest(t)
	installed := requireEmbeddedInstalledBinding(t)
	cases := []struct {
		name      string
		manifest  *RouteManifest
		wantEntry bool
	}{
		{"manifest", &manifest, true},
		{"bindingless", &bindingless, false},
		{"installed", &installed, true},
		{"embedded", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("auto-entry-%s-%d", tc.name, time.Now().UnixNano())
			prior := emptyTestBudget()
			prior.Families["Maple"] = FamilyBudget{SpentMicros: 1_000_000}
			previous, _ := json.Marshal(prior)
			flat := pilotFlatFixture(t)
			flatJSON, _ := json.Marshal(flat)
			a := pilotTestAuthority(prior)
			a.Generation, a.FinalizedSlot, a.FlatEvidenceSHA256 = 2, flat.Slot, sha256Bytes(flatJSON)
			budget, err := activatePilotBudget(prior, a)
			if err != nil {
				t.Fatal(err)
			}
			budgetJSON, err := json.Marshal(budget)
			if err != nil {
				t.Fatal(err)
			}
			in := fundedAutoFixture(t, debtPrice, collateralPrice)
			history := SelectorResult{State: SelectorState{SourceLane: in.Snapshot.RouteLane, Advantages: map[string]AdvantageWindow{testAutoLane: {Since: in.Now.Add(-2 * time.Minute), LastSample: in.Now.Add(-time.Second)}}}}
			state := map[string]any{"generation": 2, "phase3": budget, "pilotBudgetActivation": pilotBudgetActivation{a, previous, flat}, "selector": map[string]any{"mode": "live", "result": history}, "selectorEntryPaused": true}
			raw, _ := json.Marshal(state)
			if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, raw); err != nil {
				t.Fatal(err)
			}
			if _, err = db.AcquireRouteLease(ctx, key, "auto-entry-a", time.Minute); err != nil {
				t.Fatal(err)
			}
			advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
			result, err := db.recordSelectorEvaluationWithLanes(ctx, key, tc.manifest, in, in.Snapshot.Slot, 2)
			if tc.wantEntry && (err != nil || result.Action != "ENTER" || result.DestinationLane != testAutoLane) {
				t.Fatal("locked candidate entry did not persist its selection", err, result)
			}
			if !tc.wantEntry && (err != nil || result.Action != "KEEP") {
				t.Fatal("closed locked path admitted the candidate", err, result)
			}
			var version int64
			var paused bool
			var stateRaw []byte
			if err = db.pool.QueryRow(ctx, `SELECT state_version,(state->>'selectorEntryPaused')::boolean,state FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&version, &paused, &stateRaw); err != nil {
				t.Fatal(err)
			}
			if !tc.wantEntry {
				if version != 2 {
					t.Fatal("closed KEEP advanced the route fence version", version)
				}
				if !paused {
					t.Fatal("closed KEEP unpaused entry")
				}
				var persisted struct {
					Generation json.RawMessage `json:"generation"`
					Phase3     json.RawMessage `json:"phase3"`
					Entry      json.RawMessage `json:"selectorEntry"`
					Unwind     json.RawMessage `json:"selectorUnwind"`
					Canaries   json.RawMessage `json:"pilotCanaryEntries"`
				}
				if err = json.Unmarshal(stateRaw, &persisted); err != nil {
					t.Fatal(err)
				}
				if len(persisted.Entry) != 0 && string(persisted.Entry) != "null" {
					t.Fatal("closed KEEP persisted an entry", string(persisted.Entry))
				}
				if len(persisted.Unwind) != 0 || len(persisted.Canaries) != 0 {
					t.Fatal("closed KEEP wrote unwind or canary history", string(persisted.Unwind), string(persisted.Canaries))
				}
				// The stored jsonb text is PostgreSQL's normalized rendering, so
				// the unchanged-budget check compares the decoded value, not bytes.
				var storedBudget, expectedBudget any
				if err = json.Unmarshal(persisted.Phase3, &storedBudget); err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(budgetJSON, &expectedBudget); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(storedBudget, expectedBudget) {
					t.Fatal("closed KEEP moved the budget", string(persisted.Phase3))
				}
				if string(persisted.Generation) != "2" {
					t.Fatal("closed KEEP moved the planning generation", string(persisted.Generation))
				}
				if tc.manifest != nil {
					if entry, err := db.LoadSelectorEntryOnManifest(ctx, *tc.manifest, key); err != nil || entry != nil {
						t.Fatal("closed manifest path persisted a candidate entry", err, entry)
					}
				} else if entry, err := db.LoadSelectorEntry(ctx, key); err != nil || entry != nil {
					t.Fatal("closed embedded path persisted an entry", err, entry)
				}
				if _, err = db.ReleaseRouteLease(ctx); err != nil {
					t.Fatal(err)
				}
				return
			}
			if version != 3 {
				t.Fatal("candidate entry did not advance generation exactly once", version)
			}
			if paused {
				t.Fatal("candidate entry left selection paused")
			}
			restarted, err := OpenDatabase(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			entry, err := restarted.LoadSelectorEntryOnManifest(ctx, *tc.manifest, key)
			if err != nil || entry == nil || entry.Lane != testAutoLane || entry.EquityRaw != 10_000_000 {
				t.Fatal("restart lost the candidate entry through its manifest", err, entry)
			}
			if _, err = restarted.LoadSelectorEntry(ctx, key); err == nil {
				t.Fatal("embedded decode admitted a candidate entry")
			}
			if _, err = db.ReleaseRouteLease(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The recorded candidate-source unwind is the anti-strand proof: a funded AUTO
// position can record its bounded economic rotation under the reviewed
// manifest, the intent decodes and re-decodes through that manifest across a
// restart, and the embedded decode stays closed.
func TestLockedManifestSelectorRecordsCandidateSourceUnwind(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	_, debtPrice, _ := autoDebtPriceFixture(t, 1_000_000)
	collateralPrice := autoCollateralPriceFixture(t, 1_000_000)
	manifest := autoInitializerFixtureManifest(t)
	key := fmt.Sprintf("auto-unwind-%d", time.Now().UnixNano())
	prior := emptyTestBudget()
	previous, _ := json.Marshal(prior)
	flat := pilotFlatFixture(t)
	flatJSON, _ := json.Marshal(flat)
	a := pilotTestAuthority(prior)
	a.Generation, a.FinalizedSlot, a.FlatEvidenceSHA256 = 2, flat.Slot, sha256Bytes(flatJSON)
	budget, err := activatePilotBudget(prior, a)
	if err != nil {
		t.Fatal(err)
	}
	// The AUTO exit reservation the recorded unwind consumes is seeded AFTER
	// activation (mirroring the transition semantics), not on the pre-activation
	// budget a pilot transition would reject.
	budget.Families["AUTO"] = FamilyBudget{ExitMicros: 1_000_000}
	in := fundedAutoSourceFixture(t, debtPrice, collateralPrice)
	history := SelectorResult{State: SelectorState{SourceLane: in.Snapshot.RouteLane, Advantages: map[string]AdvantageWindow{"OnRe/ONyc/USDC": {Since: in.Now.Add(-2 * time.Minute), LastSample: in.Now.Add(-time.Second)}}}}
	state := map[string]any{"generation": 2, "phase3": budget, "pilotBudgetActivation": pilotBudgetActivation{a, previous, flat}, "selector": map[string]any{"mode": "live", "result": history}, "selectorEntryPaused": false}
	raw, _ := json.Marshal(state)
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "auto-unwind-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	result, err := db.recordSelectorEvaluationWithLanes(ctx, key, &manifest, in, in.Snapshot.Slot, 2)
	if err != nil || result.Action != "SWITCH" || result.DestinationLane != "OnRe/ONyc/USDC" {
		t.Fatal("candidate-source unwind not recorded", err, result)
	}
	restarted, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	intent, err := restarted.LoadUnwindIntentOnManifest(ctx, manifest, key)
	if err != nil || intent == nil || intent.SourceLane != testAutoLane || intent.BudgetFamily != "AUTO" {
		t.Fatal("restart lost the candidate-source unwind", err, intent)
	}
	if _, err = restarted.LoadUnwindIntent(ctx, key); err == nil {
		t.Fatal("embedded decode admitted a candidate-source unwind")
	}
	var version int64
	if err = restarted.pool.QueryRow(ctx, `SELECT state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatal("unwind write did not advance generation exactly once", version)
	}
}
