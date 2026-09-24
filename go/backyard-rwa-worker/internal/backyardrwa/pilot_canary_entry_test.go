package backyardrwa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func pilotCanaryFixture() SelectorInput {
	in := selectorFixture()
	// Canary acceptance is Maple-only for this rollout; retarget the fixture
	// market, quote, and request onto the permitted entry lane.
	in.Markets[0].Lane = SelectedRouteID
	in.Quotes[0].DestinationLane = SelectedRouteID
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Policy = DefaultSelectorPolicy()
	in.Snapshot.PilotActive = true
	in.Snapshot.TotalVaultNAVRaw, in.Snapshot.VoltrIdleRaw = 1_000_000, 1_000_000
	in.Quotes[0].EquityRaw = 1_000_000
	in.Quotes[0].MinimumIdleRaw = 1_000_000
	in.Quotes[0].BorrowReceiveRaw = 500_000
	in.Quotes[0].EvidenceID = sha256Bytes([]byte("actual-complete-quote-fixture"))
	in.canaryRequest = &pilotCanaryEntryRequest{ID: sha256Bytes([]byte("one-acceptance")), Lane: in.Markets[0].Lane, EquityRaw: 1_000_000, ExpiresAt: in.Now.Add(10 * time.Minute)}
	return in
}
func TestPilotCanaryUsesRealQuoteWithoutEconomicRecommendation(t *testing.T) {
	in := pilotCanaryFixture()
	economic := SelectOpportunity(in, SelectorState{})
	if economic.Action != "KEEP" {
		t.Fatal("fixture must be uneconomic", economic)
	}
	result, receipt, err := selectPilotCanaryEntry(in, economic, nil)
	if err != nil || result.Action != "CANARY_ENTER" || receipt == nil || result.SelectedQuote == nil {
		t.Fatal(result, receipt, err)
	}
	if result.Candidates[0].BenefitRaw != economic.Candidates[0].BenefitRaw {
		t.Fatal("canary fabricated economics")
	}
	history := map[string]pilotCanaryEntryReceipt{receipt.Request.ID: *receipt}
	result, again, err := selectPilotCanaryEntry(in, economic, history)
	if err != nil || result.Action != "KEEP" || again != nil {
		t.Fatal("request replayed", result, err)
	}
	for _, change := range []func(*SelectorInput){
		func(i *SelectorInput) { i.Quotes = nil },
		func(i *SelectorInput) { i.Markets[0].EntryCapacity = Capacity{Known: true} },
		func(i *SelectorInput) { i.Snapshot.WithdrawalDemandRaw = 1 },
		func(i *SelectorInput) { i.Snapshot.CapitalMutated = true },
		func(i *SelectorInput) { i.Snapshot.Fresh = false },
		func(i *SelectorInput) { i.Snapshot.LastReportAgeSeconds = 60 },
		func(i *SelectorInput) { i.Snapshot.Nonterminal = "pending" },
		func(i *SelectorInput) { i.Snapshot.ManualReason = "hold" },
		func(i *SelectorInput) { i.Snapshot.SquadsIdleRaw = 1 },
		func(i *SelectorInput) { i.Quotes[0].ValidThroughSlot = i.Snapshot.Slot - 1 },
		func(i *SelectorInput) { i.canaryRequest.ExpiresAt = i.Now },
	} {
		x := pilotCanaryFixture()
		change(&x)
		r, got, _ := selectPilotCanaryEntry(x, SelectOpportunity(x, SelectorState{}), nil)
		if got != nil || r.Action == "CANARY_ENTER" {
			t.Fatal("unsafe canary accepted", r)
		}
	}
}
func TestPilotCanaryConfigFailsClosed(t *testing.T) {
	in := pilotCanaryFixture()
	raw, _ := json.Marshal(in.canaryRequest)
	for _, suffix := range []string{" garbage", " {}"} {
		t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", string(raw)+suffix)
		if _, err := readPilotCanaryEntryRequest(in.Now); err == nil {
			t.Fatal("trailing input accepted")
		}
	}
	t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", string(raw))
	if got, err := readPilotCanaryEntryRequest(in.Now); err != nil || got == nil {
		t.Fatal(err)
	}
}
func TestPilotCanaryReceiptAndEntryCommitOnceAcrossRestart(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("pilot-canary-%d", time.Now().UnixNano())
	prior := emptyTestBudget()
	prior.Families["Maple"] = FamilyBudget{SpentMicros: 1_000_000}
	previous, _ := json.Marshal(prior)
	flat := pilotFlatFixture(t)
	flatJSON, _ := json.Marshal(flat)
	authority := pilotTestAuthority(prior)
	authority.Generation, authority.FinalizedSlot, authority.FlatEvidenceSHA256 = 2, flat.Slot, sha256Bytes(flatJSON)
	budget, err := activatePilotBudget(prior, authority)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"generation": 2, "phase3": budget, "pilotBudgetActivation": pilotBudgetActivation{authority, previous, flat}, "selectorEntryPaused": true}
	raw, _ := json.Marshal(state)
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,2)`, key, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "canary-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	in := pilotCanaryFixture()
	if _, err = db.RecordSelectorEvaluation(ctx, key, in, in.Snapshot.Slot, 1); err == nil {
		t.Fatal("lost generation admitted")
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,expected_effects) VALUES($1,$2,'signed','OPEN_ROUTE_STEP','{}')`, key+"-pending", key); err != nil {
		t.Fatal(err)
	}
	if _, err = db.RecordSelectorEvaluation(ctx, key, in, in.Snapshot.Slot, 2); err == nil {
		t.Fatal("pending operation admitted")
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciled' WHERE operation_id=$1`, key+"-pending"); err != nil {
		t.Fatal(err)
	}
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	result, err := db.RecordSelectorEvaluation(ctx, key, in, in.Snapshot.Slot, 2)
	if err != nil || result.Action != "CANARY_ENTER" {
		t.Fatal(result, err)
	}
	if _, err = db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.AcquireRouteLease(ctx, key, "canary-b", time.Minute); err != nil {
		t.Fatal(err)
	}
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	result, err = restarted.RecordSelectorEvaluation(ctx, key, in, in.Snapshot.Slot, 3)
	if err != nil || result.Action != "KEEP" || result.Reason != "operator_canary_already_consumed" {
		t.Fatal("restart replay", result, err)
	}
	var savedBudget, history []byte
	var version int64
	if err = restarted.pool.QueryRow(ctx, `SELECT state->'phase3',state->'pilotCanaryEntries',state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&savedBudget, &history, &version); err != nil {
		t.Fatal(err)
	}
	var decoded Phase3Budget
	if json.Unmarshal(savedBudget, &decoded) != nil {
		t.Fatal("budget")
	}
	before, _ := json.Marshal(budget)
	after, _ := json.Marshal(decoded)
	if !bytes.Equal(before, after) || version != 3 {
		t.Fatal("canary changed spending history or replayed generation")
	}
	var receipts map[string]pilotCanaryEntryReceipt
	if json.Unmarshal(history, &receipts) != nil || len(receipts) != 1 || receipts[in.canaryRequest.ID].QuoteEvidenceID != in.Quotes[0].EvidenceID {
		t.Fatal("lost one-use receipt")
	}
	entry, err := restarted.LoadSelectorEntry(ctx, key)
	if err != nil || entry == nil || entry.EquityRaw != in.canaryRequest.EquityRaw {
		t.Fatal("lost exact entry", err)
	}
}

// pilotCanaryRetainedHistory builds n distinct retained receipts as valid
// historical requests: the reviewed lane/equity cloned from the live request,
// a replaced ID that never collides with it, and a past expiry with
// AcceptedAt before that expiry.
func pilotCanaryRetainedHistory(t *testing.T, n int, in SelectorInput) map[string]pilotCanaryEntryReceipt {
	t.Helper()
	history := make(map[string]pilotCanaryEntryReceipt, n)
	for i := 0; i < n; i++ {
		id := sha256Bytes([]byte(fmt.Sprintf("retained-canary-%d", i)))
		if id == in.canaryRequest.ID {
			t.Fatal("retained id collided with the live request")
		}
		request := *in.canaryRequest
		request.ID = id
		request.ExpiresAt = in.Now.Add(-time.Duration(i+1) * time.Minute)
		history[id] = pilotCanaryEntryReceipt{
			Request:         request,
			AcceptedAt:      in.Now.Add(-time.Duration(i+2) * time.Minute),
			QuoteEvidenceID: sha256Bytes([]byte(fmt.Sprintf("retained-evidence-%d", i))),
		}
	}
	return history
}

func canaryHoldReason(t *testing.T, err error) string {
	t.Helper()
	hold, ok := err.(*BudgetHold)
	if !ok {
		t.Fatalf("expected a budget hold, got %v", err)
	}
	return hold.Reason
}

func TestPilotCanaryCapacityRetainsHistoryWithoutBlockingCurrentRelease(t *testing.T) {
	in := pilotCanaryFixture()
	economic := SelectOpportunity(in, SelectorState{})

	// Eight retained receipts leave plenty of room: a fresh eligible request is
	// still accepted and every prior receipt is left untouched.
	eight := pilotCanaryRetainedHistory(t, 8, in)
	before, _ := json.Marshal(eight)
	result, receipt, err := selectPilotCanaryEntry(in, economic, eight)
	if err != nil || result.Action != "CANARY_ENTER" || receipt == nil {
		t.Fatal("fresh request blocked by retained history", result, err)
	}
	after, _ := json.Marshal(eight)
	if !bytes.Equal(before, after) || len(eight) != 8 {
		t.Fatal("retained receipts changed")
	}

	// One below the named maximum still admits a fresh request.
	fifteen := pilotCanaryRetainedHistory(t, 15, in)
	if result, receipt, err = selectPilotCanaryEntry(in, economic, fifteen); err != nil || result.Action != "CANARY_ENTER" || receipt == nil {
		t.Fatal("fresh request blocked below capacity", result, err)
	}

	// At the named maximum a fresh request is refused with the named hold.
	full := pilotCanaryRetainedHistory(t, 16, in)
	result, receipt, err = selectPilotCanaryEntry(in, economic, full)
	if err == nil || receipt != nil || canaryHoldReason(t, err) != "pilot_canary_history_full" || result.Action != "KEEP" {
		t.Fatal("capacity gate did not hold at the maximum", result, err)
	}

	// Idempotent replay of an already-consumed request is classified BEFORE the
	// capacity gate: 15 retained receipts plus the consumed live ID is exactly
	// the reachable full boundary, and the exact request still reports
	// consumed, never a hold.
	boundary := pilotCanaryRetainedHistory(t, 15, in)
	boundary[in.canaryRequest.ID] = pilotCanaryEntryReceipt{Request: *in.canaryRequest, AcceptedAt: in.Now, QuoteEvidenceID: sha256Bytes([]byte("consumed-evidence"))}
	if len(boundary) != pilotCanaryReceiptCapacity {
		t.Fatalf("expected exactly %d retained receipts at the boundary, got %d", pilotCanaryReceiptCapacity, len(boundary))
	}
	result, receipt, err = selectPilotCanaryEntry(in, economic, boundary)
	if err != nil || receipt != nil || result.Action != "KEEP" || result.Reason != "operator_canary_already_consumed" {
		t.Fatal("consumed request not idempotent at capacity", result, err)
	}

	// A changed reuse of a retained ID is also classified BEFORE the capacity
	// gate and still rejects with the reuse hold, not the capacity hold.
	reused := pilotCanaryRetainedHistory(t, 16, in)
	if len(reused) != pilotCanaryReceiptCapacity {
		t.Fatalf("expected %d retained receipts, got %d", pilotCanaryReceiptCapacity, len(reused))
	}
	retainedID := in.canaryRequest.ID
	for id := range reused {
		retainedID = id
		break
	}
	reusedRequest := *in.canaryRequest
	reusedRequest.ID = retainedID
	reusedRequest.EquityRaw = in.canaryRequest.EquityRaw - 1
	in.canaryRequest = &reusedRequest
	result, receipt, err = selectPilotCanaryEntry(in, economic, reused)
	if err == nil || receipt != nil || canaryHoldReason(t, err) != "pilot_canary_request_id_reused" {
		t.Fatal("changed reuse not rejected before capacity", result, err)
	}
}

// One unexpired request may re-accept with a fresh quote after its own entry
// lapsed unallocated: a new lane needs the obligation initializer and then the
// allocation, which outlive one 30-second entry (live 2026-09-24). A bound
// allocation, another request's entry or a still-live entry keeps it consumed.
func TestPilotCanaryReacceptsOnlyUnallocatedLapsedEntry(t *testing.T) {
	in := pilotCanaryFixture()
	economic := SelectOpportunity(in, SelectorState{})
	_, receipt, err := selectPilotCanaryEntry(in, economic, nil)
	if err != nil || receipt == nil {
		t.Fatal(receipt, err)
	}
	history := map[string]pilotCanaryEntryReceipt{receipt.Request.ID: *receipt}
	lapsed := SelectorEntry{Lane: receipt.Request.Lane, EquityRaw: receipt.Request.EquityRaw, Quote: in.Quotes[0], AcceptedAt: in.Now.Add(-time.Minute), ExpiresAt: in.Now.Add(-30 * time.Second)}
	lapsed.Quote.EvidenceID = receipt.QuoteEvidenceID
	for name, tc := range map[string]struct {
		change func(*SelectorEntry)
		want   bool
	}{
		"lapsed unallocated": {func(*SelectorEntry) {}, true},
		"allocated":          {func(e *SelectorEntry) { e.AllocationOperationID = "op" }, false},
		"still live":         {func(e *SelectorEntry) { e.ExpiresAt = in.Now.Add(time.Second) }, false},
		"other request":      {func(e *SelectorEntry) { e.Quote.EvidenceID = sha256Bytes([]byte("other")) }, false},
	} {
		e := lapsed
		tc.change(&e)
		x := in
		x.canaryPriorEntry = &e
		result, again, err := selectPilotCanaryEntry(x, economic, history)
		if err != nil || (result.Action == "CANARY_ENTER") != tc.want || (again != nil) != tc.want {
			t.Fatal(name, result.Action, result.Reason, err)
		}
	}
	x := in
	x.canaryPriorEntry = &lapsed
	x.Now = receipt.Request.ExpiresAt
	if result, again, _ := selectPilotCanaryEntry(x, economic, history); result.Action == "CANARY_ENTER" || again != nil {
		t.Fatal("expired request re-accepted")
	}
}
