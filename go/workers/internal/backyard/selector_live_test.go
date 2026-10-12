package backyard

import (
	"context"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLiveSelectorCollectsExecutablePartialCapacityAndKeepsFeedImmutable(t *testing.T) {
	t.Parallel()
	m, rpc, client, accounts := selectorDestinationFixture(t)
	route, _ := runtimeRoute(SelectedRouteID)
	// Maple is a B2 lane (registry): its entry equity is bounded by the
	// collateral reserve's deposit room, independent of debt room.
	collateral, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	deposited, err := ceilScaledBigFraction(collateral.totalLiquiditySF)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoReserveConfigOffset+160:], deposited+3_000_000)
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
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
	if _, err = observeSelectorDestinationForecast(context.Background(), rpc, fixtureView(t, rpc), client, m, capturedTestPolicies(), leveredTestMarket(SelectedRouteID), 10_000_000, 42, true); err != nil {
		t.Fatal("partial producer", err)
	}
	observed, quotes, err := collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, markets, in.Policy)
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
	if _, err = observeSelectorDestinationForecast(context.Background(), rpc, fixtureView(t, rpc), client, m, capturedTestPolicies(), leveredTestMarket(SelectedRouteID), 10_000_000, 42, false); err == nil {
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
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoReserveConfigOffset+160:], deposited)
	observed, quotes, err = collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, markets, in.Policy)
	if err != nil || len(quotes) != 0 || observed[0].EntryCapacity.Raw != 0 || observed[0].EntryBlockedReason == "" {
		t.Fatal("capacity closure retained stale quote", err, quotes, observed)
	}
}

func TestLiveSelectorRejectsDuplicateMarketFanoutAndStaleSource(t *testing.T) {
	t.Parallel()
	m, rpc, client, _ := selectorDestinationFixture(t)
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.Slot = 42
	o := tickObservation(in.Snapshot)
	o.ObservedAt = time.Now().UTC()
	market := in.Markets[0]
	_, _, err := collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, []LaneEconomics{market, market}, in.Policy)
	assertBudgetHold(t, err, "invalid_selector_market_set")
	o.ObservedAt = o.ObservedAt.Add(-time.Minute)
	_, _, err = collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, in.Markets, in.Policy)
	assertBudgetHold(t, err, "selector_live_snapshot_unavailable")
}

func TestLiveSelectorCancelsSlowSiblingAndRetainsCompletedQuote(t *testing.T) {
	t.Parallel()
	m, rpc, client, accounts := selectorDestinationFixture(t)
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.Slot = 42
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
	_, _, _, sibling := selectorDestinationFixtureForLane(t, slow.Lane, nil)
	view := fixtureView(t, budgetBuildRPCWithAccounts(t, 5000, 42, append(accounts, sibling...)))
	jupiterHTTP := fixtureHTTP(client)
	original := jupiterHTTP.Transport
	jupiterHTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.RawQuery, route.Kamino.CollateralMint) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return original.RoundTrip(req)
	})
	start := time.Now()
	markets, quotes, err := collectSelectorQuotes(context.Background(), rpc, view, client, m, o, []LaneEconomics{market, slow}, in.Policy)
	if err != nil || len(quotes) != 1 || quotes[0].DestinationLane != SelectedRouteID || time.Since(start) > 3*time.Second || markets[1].EntryBlockedReason == "" {
		t.Fatal("slow sibling suppressed valid quote", err, quotes, markets, time.Since(start))
	}
	if !quotes[0].currentAtSlot(42) {
		t.Fatal("retained expired quote")
	}
}

func TestCanaryQuoteCollectionSkipsOtherLanes(t *testing.T) {
	t.Parallel()
	m, rpc, client, _ := selectorDestinationFixture(t)
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.Slot = 42
	in.Snapshot.TotalVaultNAVRaw, in.Snapshot.VoltrIdleRaw = 10_000_000, 10_000_000
	o := tickObservation(in.Snapshot)
	o.ObservedAt = time.Now().UTC()
	market := in.Markets[0]
	market.Lane = SelectedRouteID
	// Control: without a canary lane the fundable lane is priced.
	if _, quotes, err := collectSelectorQuotesForLane(context.Background(), rpc, fixtureView(t, rpc), client, m, o, []LaneEconomics{market}, in.Policy, ""); err != nil || len(quotes) != 1 {
		t.Fatal("control quote missing", err, quotes)
	}
	// A canary for another lane leaves this lane unpriced and not executable.
	markets, quotes, err := collectSelectorQuotesForLane(context.Background(), rpc, fixtureView(t, rpc), client, m, o, []LaneEconomics{market}, in.Policy, autoAUTOPYUSD.Lane)
	if err != nil || len(quotes) != 0 || markets[0].EntryBlockedReason != "operator_canary_lane_only" || markets[0].EntryCapacity.Raw != 0 {
		t.Fatal("canary collection priced another lane", err, quotes, markets[0])
	}
}
