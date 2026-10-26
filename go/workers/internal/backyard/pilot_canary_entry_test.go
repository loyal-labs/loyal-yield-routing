package backyard

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
	in.Markets[0].Lane = MapleSyrupUSDCLaneID
	in.Quotes[0].DestinationLane = MapleSyrupUSDCLaneID
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Policy = DefaultSelectorPolicy()
	in.Snapshot.TotalVaultNAVRaw, in.Snapshot.VoltrIdleRaw = 1_000_000, 1_000_000
	in.Quotes[0].EquityRaw = 1_000_000
	in.Quotes[0].MinimumIdleRaw = 1_000_000
	in.Quotes[0].BorrowReceiveRaw = 500_000
	in.Quotes[0] = parBorrow(in.Quotes[0])
	in.Quotes[0].EvidenceID = sha256Bytes([]byte("actual-complete-quote-fixture"))
	in.Quotes[0].DestinationExit = &ExitCost{Lane: MapleSyrupUSDCLaneID, BPS: 1, QuotedCollateralRaw: 1_000_000, MarkSF: kaminoOne(), Source: exitCostQuote, Slot: in.Snapshot.Slot, At: in.Now}
	in.canaryRequest = &pilotCanaryEntryRequest{ID: sha256Bytes([]byte("one-acceptance")), Lane: in.Markets[0].Lane, EquityRaw: 1_000_000, ExpiresAt: in.Now.Add(10 * time.Minute)}
	return in
}
func TestPilotCanaryUsesRealQuoteWithoutEconomicRecommendation(t *testing.T) {
	t.Parallel()
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
	// While the vault is flat the request keeps its lane on a fresh quote:
	// target() enters the allocated cash on the latest result. Once the lane
	// holds capital the request is spent.
	history := map[string]pilotCanaryEntryReceipt{receipt.Request.ID: *receipt}
	result, again, err := selectPilotCanaryEntry(in, economic, history)
	if err != nil || result.Action != "CANARY_ENTER" || again == nil || again.Request != receipt.Request {
		t.Fatal("flat request lost its lane", result, err)
	}
	held := in
	held.Snapshot.CollateralIdleRaw = 1
	if result, again, err = selectPilotCanaryEntry(held, economic, history); err != nil || result.Action != "KEEP" || again != nil {
		t.Fatal("request replayed into a held lane", result, err)
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
	hold, ok := err.(*PreSendHold)
	if !ok {
		t.Fatalf("expected a budget hold, got %v", err)
	}
	return holdCause(hold)
}

func TestPilotCanaryCapacityRetainsHistoryWithoutBlockingCurrentRelease(t *testing.T) {
	t.Parallel()
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
	fifteen := pilotCanaryRetainedHistory(t, pilotCanaryReceiptCapacity-1, in)
	if result, receipt, err = selectPilotCanaryEntry(in, economic, fifteen); err != nil || result.Action != "CANARY_ENTER" || receipt == nil {
		t.Fatal("fresh request blocked below capacity", result, err)
	}

	// At the named maximum a fresh request is refused with the named hold.
	full := pilotCanaryRetainedHistory(t, pilotCanaryReceiptCapacity, in)
	result, receipt, err = selectPilotCanaryEntry(in, economic, full)
	if err == nil || receipt != nil || canaryHoldReason(t, err) != "pilot_canary_history_full" || result.Action != "KEEP" {
		t.Fatal("capacity gate did not hold at the maximum", result, err)
	}

	// Idempotent replay of an already-consumed request is classified BEFORE the
	// capacity gate: 15 retained receipts plus the consumed live ID is exactly
	// the reachable full boundary, and the exact request still reports
	// consumed, never a hold.
	boundary := pilotCanaryRetainedHistory(t, pilotCanaryReceiptCapacity-1, in)
	boundary[in.canaryRequest.ID] = pilotCanaryEntryReceipt{Request: *in.canaryRequest, AcceptedAt: in.Now, QuoteEvidenceID: sha256Bytes([]byte("consumed-evidence"))}
	if len(boundary) != pilotCanaryReceiptCapacity {
		t.Fatalf("expected exactly %d retained receipts at the boundary, got %d", pilotCanaryReceiptCapacity, len(boundary))
	}
	result, receipt, err = selectPilotCanaryEntry(in, economic, boundary)
	if err != nil || receipt == nil || result.Action != "CANARY_ENTER" {
		t.Fatal("a known request was refused at capacity", result, err)
	}

	// A changed reuse of a retained ID is also classified BEFORE the capacity
	// gate and still rejects with the reuse hold, not the capacity hold.
	reused := pilotCanaryRetainedHistory(t, pilotCanaryReceiptCapacity, in)
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
