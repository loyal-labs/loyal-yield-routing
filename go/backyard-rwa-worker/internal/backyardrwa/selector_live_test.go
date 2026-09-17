package backyardrwa

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestLiveSelectorCollectsExecutablePartialCapacityAndKeepsFeedImmutable(t *testing.T) {
	m, rpc, client, accounts := selectorDestinationFixture(t)
	route, _ := runtimeRoute(SelectedRouteID)
	debt := accountAt(accounts, route.Kamino.DebtReserve).Data
	used := binary.LittleEndian.Uint64(debt[kaminoOutsideBorrowCounterOffset:])
	binary.LittleEndian.PutUint64(debt[kaminoOutsideBorrowLimitOffset:], used+1_000_000)
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.PilotActive = true
	in.Snapshot.Slot = 42
	in.Snapshot.TotalVaultNAVRaw, in.Snapshot.VoltrIdleRaw = 100_000_000, 100_000_000
	o := tickObservation(in.Snapshot)
	o.ObservedAt = time.Now().UTC()
	market := in.Markets[0]
	market.Lane = SelectedRouteID
	market.EntryCapacity = Capacity{}
	unavailable := market
	unavailable.Lane = "OnRe/ONyc/USDC"
	unavailable.EntryBlockedReason = "reserve_inactive_or_emergency"
	markets := []LaneEconomics{market, unavailable}
	if _, err := observeSelectorDestinationSize(context.Background(), rpc, client, m, SelectedRouteID, 10_000_000, 42, true); err != nil {
		t.Fatal("partial producer", err)
	}
	observed, quotes, err := collectSelectorQuotes(context.Background(), rpc, client, m, o, markets, in.Policy)
	if err != nil || len(quotes) != 1 {
		t.Fatal(err, quotes, observed)
	}
	q := quotes[0]
	if q.DestinationLane != SelectedRouteID || q.EquityRaw <= 0 || q.EquityRaw >= 10_000_000 || observed[0].EntryCapacity.Raw != q.EquityRaw || !observed[0].EntryCapacity.Known || markets[0].EntryCapacity.Known {
		t.Fatal("partial capacity or immutable feed lost", q, observed)
	}
	if observed[1].EntryBlockedReason == "" || observed[1].EntryCapacity.Raw != 0 {
		t.Fatal("unready sibling admitted", observed[1])
	}
	// The exact-size API still refuses, so callers cannot accidentally execute a
	// different amount from the one they asked to price.
	if _, err = observeSelectorDestination(context.Background(), rpc, client, m, SelectedRouteID, 10_000_000, 42); err == nil {
		t.Fatal("exact-size contract silently clamped")
	}
	in.Markets, in.Quotes = observed, quotes
	in.Now = time.Now().UTC()
	result := SelectOpportunity(in, SelectorState{})
	found := false
	for _, candidate := range result.Candidates {
		if candidate.Lane == SelectedRouteID {
			found = candidate.CostsKnown
		}
	}
	if !found {
		t.Fatal("collected partial quote not usable by selector", result)
	}
	binary.LittleEndian.PutUint64(debt[kaminoOutsideBorrowLimitOffset:], used)
	observed, quotes, err = collectSelectorQuotes(context.Background(), rpc, client, m, o, markets, in.Policy)
	if err != nil || len(quotes) != 0 || observed[0].EntryCapacity.Raw != 0 || observed[0].EntryBlockedReason == "" {
		t.Fatal("capacity closure retained stale quote", err, quotes, observed)
	}
}

func TestLiveSelectorRejectsDuplicateMarketFanoutAndStaleSource(t *testing.T) {
	m, rpc, client, _ := selectorDestinationFixture(t)
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.PilotActive = true
	in.Snapshot.Slot = 42
	o := tickObservation(in.Snapshot)
	o.ObservedAt = time.Now().UTC()
	market := in.Markets[0]
	_, _, err := collectSelectorQuotes(context.Background(), rpc, client, m, o, []LaneEconomics{market, market}, in.Policy)
	assertBudgetHold(t, err, "invalid_selector_market_set")
	o.ObservedAt = o.ObservedAt.Add(-time.Minute)
	_, _, err = collectSelectorQuotes(context.Background(), rpc, client, m, o, in.Markets, in.Policy)
	assertBudgetHold(t, err, "selector_live_snapshot_unavailable")
}

func TestLiveSelectorCancelsSlowSiblingAndRetainsCompletedQuote(t *testing.T) {
	m, rpc, client, _ := selectorDestinationFixture(t)
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.PilotActive, in.Snapshot.Slot = true, 42
	in.Snapshot.TotalVaultNAVRaw, in.Snapshot.VoltrIdleRaw = 10_000_000, 10_000_000
	o := tickObservation(in.Snapshot)
	// Leave a bounded one-second collection window without sleeping through the
	// full production window; all actual fixture quotes still use current slots.
	o.ObservedAt = time.Now().UTC().Add(-7 * time.Second)
	market := in.Markets[0]
	market.Lane = SelectedRouteID
	slow := market
	slow.Lane = "OnRe/ONyc/USDC"
	route, _ := runtimeRoute(slow.Lane)
	original := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(route.Kamino.Market)) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return original.RoundTrip(req)
	})
	start := time.Now()
	markets, quotes, err := collectSelectorQuotes(context.Background(), rpc, client, m, o, []LaneEconomics{market, slow}, in.Policy)
	if err != nil || len(quotes) != 1 || quotes[0].DestinationLane != SelectedRouteID || time.Since(start) > 3*time.Second || markets[1].EntryBlockedReason == "" {
		t.Fatal("slow sibling suppressed valid quote", err, quotes, markets, time.Since(start))
	}
	if !quotes[0].currentAtSlot(42) {
		t.Fatal("retained expired quote")
	}
}

func TestPilotSelectorCannotSwitchDuringFundedTranche(t *testing.T) {
	in := selectorFixture()
	in.Snapshot.PilotActive = true
	s := &in.Snapshot
	s.VoltrIdleRaw, s.TotalVaultNAVRaw = 90_000_000, 100_000_000
	s.HasPosition = true
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 10_000_000, 10_000_000
	s.StrategyNAVRaw, s.PriorReportedNAVRaw = 10_000_000, 10_000_000
	current := in.Markets[0]
	current.Lane, current.NativeAPY = s.RouteLane, .10
	in.Markets = append(in.Markets, current)
	in.Quotes[0].EquityRaw, in.Quotes[0].BorrowReceiveRaw = 10_000_000, 5_000_000
	history := SelectorState{SourceLane: s.RouteLane, Advantages: map[string]AdvantageWindow{in.Markets[0].Lane: {Since: in.Now.Add(-time.Hour), LastSample: in.Now.Add(-time.Second)}}}
	if got := SelectOpportunity(in, history); got.Action != "KEEP" || got.Reason != "complete_current_tranche_first" || got.SelectedQuote != nil {
		t.Fatal("temporary unlevered baseline triggered rotation", got)
	}
	s.PositionDebtRaw, s.PositionDebtValueRaw = 5_000_000, 5_000_000
	s.SquadsIdleRaw = 5_000_000
	if !selectorTrancheInProgress(*s) {
		t.Fatal("borrowed cash treated as completed entry")
	}
	s.SquadsIdleRaw, s.CollateralIdleRaw = 0, 5_000_000
	if !selectorTrancheInProgress(*s) {
		t.Fatal("pending redeposit treated as completed entry")
	}
	s.CollateralIdleRaw = 0
	if selectorTrancheInProgress(*s) {
		t.Fatal("settled loop blocked")
	}
	s.CollateralIdleRaw, s.MinimumCollateralDepositRaw = 1, 2
	if selectorTrancheInProgress(*s) {
		t.Fatal("sub-receipt residue blocked economics")
	}
}
