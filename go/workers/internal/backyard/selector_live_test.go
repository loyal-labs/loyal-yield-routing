package backyard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func TestLiveSelectorCollectsExecutablePartialCapacityAndKeepsFeedImmutable(t *testing.T) {
	t.Parallel()
	m, rpc, client, accounts := selectorDestinationFixture(t)
	route, _ := runtimeRoute(MapleSyrupUSDCLaneID)
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
	market.Lane = MapleSyrupUSDCLaneID
	market.EntryCapacity = Capacity{}
	unavailable := market
	unavailable.Lane = "OnRe/ONyc/USDC"
	unavailable.EntryBlockedReason = "reserve_inactive_or_emergency"
	markets := []LaneEconomics{market, unavailable}
	observed, quotes, err := collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, markets, in.Policy, nil)
	if err != nil || len(quotes) != 1 {
		t.Fatal(err, quotes, observed)
	}
	q := quotes[0]
	if q.DestinationLane != MapleSyrupUSDCLaneID || q.EquityRaw <= 0 || q.EquityRaw >= 10_000_000 || observed[0].EntryCapacity.Raw != q.EquityRaw || !observed[0].EntryCapacity.Known || markets[0].EntryCapacity.Known {
		t.Fatal("partial capacity or immutable feed lost", q, observed)
	}
	if observed[1].EntryBlockedReason == "" || observed[1].EntryCapacity.Raw != 0 {
		t.Fatal("unready sibling admitted", observed[1])
	}
	in.Markets, in.Quotes = observed, quotes
	in.Now = time.Now().UTC()
	result := SelectOpportunity(in, SelectorState{})
	found := false
	for _, candidate := range result.Candidates {
		if candidate.Lane == MapleSyrupUSDCLaneID {
			found = candidate.CostsKnown
		}
	}
	if !found {
		t.Fatal("collected partial quote not usable by selector", result)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoReserveConfigOffset+160:], deposited)
	observed, quotes, err = collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, markets, in.Policy, nil)
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
	_, _, err := collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, []LaneEconomics{market, market}, in.Policy, nil)
	assertBudgetHold(t, err, "invalid_selector_market_set")
	// A sample whose observation is past its quoting window reads nothing.
	o.ObservedAt = o.ObservedAt.Add(-time.Minute)
	if _, _, err = collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, in.Markets, in.Policy, nil); !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatalf("a stale sample quoted: %v", err)
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
	market.Lane = MapleSyrupUSDCLaneID
	// Control: without a canary lane the fundable lane is priced.
	if _, quotes, err := collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, []LaneEconomics{market}, in.Policy, nil); err != nil || len(quotes) != 1 {
		t.Fatal("control quote missing", err, quotes)
	}
	// A canary for another lane leaves this lane unpriced and not executable.
	markets, quotes, err := collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, []LaneEconomics{market}, in.Policy, &pilotCanaryEntryRequest{Lane: autoAUTOPYUSD.Lane})
	if err != nil || len(quotes) != 0 || markets[0].EntryBlockedReason != "operator_canary_lane_only" || markets[0].EntryCapacity.Raw != 0 {
		t.Fatal("canary collection priced another lane", err, quotes, markets[0])
	}
}

// quoteLegs records the equity-swap quote sizes actually requested.
func quoteLegsTransport(t *testing.T, base http.RoundTripper, legs *[]uint64, impactThreshold, divisor uint64) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/quote" {
			amount, _ := strconv.ParseUint(req.URL.Query().Get("amount"), 10, 64)
			if req.URL.Query().Get("inputMint") == fleet.USDCMint {
				for _, size := range []uint64{10_000_000, 1_000_000, 100_000} {
					if amount == size {
						*legs = append(*legs, amount)
						break
					}
				}
			}
			if req.URL.Query().Get("inputMint") == fleet.USDCMint && amount >= impactThreshold {
				out := amount / divisor
				raw, err := json.Marshal(jupiter.Quote{InputMint: fleet.USDCMint, OutputMint: req.URL.Query().Get("outputMint"), InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(out), OtherAmountThreshold: fmt.Sprint(out * 995 / 1000), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}})
				if err != nil {
					t.Fatal(err)
				}
				return response(string(raw)), nil
			}
		}
		return base.RoundTrip(req)
	})
}

func ladderLiveObservation(t *testing.T, nativeAPY float64) (RouteManifest, *chain.Client, *jupiter.Client, Observation, LaneEconomics, SelectorPolicy) {
	t.Helper()
	m, rpc, client, _ := selectorDestinationFixture(t)
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.Slot = 42
	in.Snapshot.TotalVaultNAVRaw, in.Snapshot.VoltrIdleRaw = 100_000_000, 100_000_000
	o := tickObservation(in.Snapshot)
	o.ObservedAt = time.Now().UTC()
	market := in.Markets[0]
	market.Lane = MapleSyrupUSDCLaneID
	market.NativeAPY = nativeAPY
	market.EntryCapacity = Capacity{Known: true, Unlimited: true}
	policy := DefaultSelectorPolicy()
	policy.MinimumBenefitRaw = 0
	return m, rpc, client, o, market, policy
}

func TestLiveSelectorLadderProbesSmallerAfterLargestCostExceedsEquity(t *testing.T) {
	t.Parallel()
	m, rpc, client, o, market, policy := ladderLiveObservation(t, .5)
	// This synthetic vault holds only 100 USDC of NAV, so the default 10 bps
	// uncertainty wedge (0.10) would exceed a 1 USDC candidate's whole forecast
	// gain. The ladder here compares pure route depth only; zero the wedge in
	// this one test and assert the surviving candidate is positively profitable.
	policy.UncertaintyBPS = 0
	base := fixtureHTTP(client).Transport
	var legs []uint64
	fixtureHTTP(client).Transport = quoteLegsTransport(t, base, &legs, 5_000_000, 100_000)
	// The largest size loses almost all of its quoted output to route depth
	// (out=in/100000 on the equity leg), so its expected cost reaches its own
	// equity: the pricer refuses exactly that economic outcome for the first
	// ladder size.
	_, err := testLanePricer(t, rpc, client, m, o.Snapshot.Slot).priceEntry(context.Background(), o, leveredTestMarket(MapleSyrupUSDCLaneID), 10_000_000)
	var hold *PreSendHold
	if !errors.As(err, &hold) || holdCause(hold) != "selector_move_cost_exceeds_equity" {
		t.Fatal("largest refusal is not the economic cost outcome", err)
	}
	// The ladder continues past that refused largest size and stops at the
	// smaller profitable 1M quote.
	legs = nil
	observed, quotes, err := collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, []LaneEconomics{market}, policy, &pilotCanaryEntryRequest{EquityRaw: 10_000_000})
	if err != nil || len(quotes) != 1 {
		t.Fatal("smaller profitable size not probed after economic failure", err, quotes, observed)
	}
	if quotes[0].EquityRaw != 1_000_000 || observed[0].EntryCapacity.Raw != quotes[0].EquityRaw || observed[0].EntryBlockedReason != "" {
		t.Fatal("published quote is not the smaller profitable size", quotes[0], observed[0])
	}
	if len(legs) != 2 || legs[0] != 10_000_000 || legs[1] != 1_000_000 {
		t.Fatal("ladder legs", legs)
	}
	if quotes[0].CostRaw <= 0 || quotes[0].CostRaw != quotes[0].Cost.TotalMicros {
		t.Fatal("published quote lost its expected cost", quotes[0].Cost)
	}
	if benefit, admissible := selectorMoveQuoteBenefit(o, []LaneEconomics{market}, policy, quotes[0]); !admissible || benefit <= 0 {
		t.Fatal("smaller candidate is not positively profitable", benefit, admissible)
	}
}

func TestLiveSelectorLadderFallsThroughRefusedStepQuote(t *testing.T) {
	t.Parallel()
	m, rpc, client, o, market, policy := ladderLiveObservation(t, .5)
	lever := leveredTestMarket(MapleSyrupUSDCLaneID)
	market.CurrentBorrowAPY, market.BorrowCurve, market.DebtSupplyRaw, market.DebtBorrowRaw = lever.CurrentBorrowAPY, lever.BorrowCurve, lever.DebtSupplyRaw, lever.DebtBorrowRaw
	route, _ := runtimeRoute(MapleSyrupUSDCLaneID)
	base := fixtureHTTP(client).Transport
	// Jupiter has no route for the largest rung's 1.5x down step (exit_quote,
	// a plain 400); the 10 USDC rung borrows below the minimum and needs none.
	fixtureHTTP(client).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/quote" && req.URL.Query().Get("inputMint") == route.Kamino.CollateralMint && req.URL.Query().Get("amount") == "49749999" {
			return &http.Response{StatusCode: http.StatusBadRequest, Body: http.NoBody, Header: make(http.Header)}, nil
		}
		return base.RoundTrip(req)
	})
	observed, quotes, err := collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, []LaneEconomics{market}, policy, &pilotCanaryEntryRequest{EquityRaw: 100_000_000})
	if err != nil || len(quotes) != 1 || quotes[0].EquityRaw != 10_000_000 || quotes[0].CostRaw <= 0 || observed[0].EntryBlockedReason != "" {
		t.Fatal("refused step quote ended the ladder", err, quotes, observed)
	}
}

func TestLiveSelectorLadderStopsAfterQuoteWindowBudget(t *testing.T) {
	t.Parallel()
	m, rpc, client, o, market, policy := ladderLiveObservation(t, .5)
	policy.UncertaintyBPS = 0
	var legs []uint64
	fixtureHTTP(client).Transport = quoteLegsTransport(t, fixtureHTTP(client).Transport, &legs, 5_000_000, 100_000)
	// Same economics as the ladder test above, but the observation is already
	// past the ladder budget: only the largest size is priced, so no smaller
	// quote can arrive with too few slots left to allocate.
	o.ObservedAt = time.Now().UTC().Add(-selectorLadderBudget - time.Second)
	_, quotes, err := collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, []LaneEconomics{market}, policy, &pilotCanaryEntryRequest{EquityRaw: 10_000_000})
	if err != nil || len(quotes) != 0 || len(legs) != 1 || legs[0] != 10_000_000 {
		t.Fatal("ladder priced smaller sizes after its budget", err, quotes, legs)
	}
}

func TestCanaryQuotedBelowGainCeiling(t *testing.T) {
	t.Parallel()
	m, rpc, client, o, market, policy := ladderLiveObservation(t, .5)
	// No size's gain ceiling clears this minimum, yet an operator canary skips
	// the benefit check, so its size is still quoted.
	policy.MinimumBenefitRaw = 1_000_000_000
	_, quotes, err := collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, []LaneEconomics{market}, policy, &pilotCanaryEntryRequest{Lane: MapleSyrupUSDCLaneID, EquityRaw: 10_000_000})
	if err != nil || len(quotes) != 1 || quotes[0].EquityRaw != 10_000_000 {
		t.Fatal("canary size not quoted below the gain ceiling", err, quotes)
	}
}
