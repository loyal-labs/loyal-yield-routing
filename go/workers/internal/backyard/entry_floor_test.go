package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func TestEntryFloorRejectsMarginalAndKeepsRobustTrade(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apy   float64
		enter bool
	}{
		{"marginal", .30, false}, {"robust", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const equity = 100_000_000
			m, rpc, client, _ := selectorDestinationFixtureForLane(t, MapleSyrupUSDCLaneID, blockDebtUtilization(t, MapleSyrupUSDCLaneID))
			o := flatEntryObservation(42, equity)
			market := leveredTestMarket(MapleSyrupUSDCLaneID)
			market.NativeAPY, market.ObservedAt, market.NativeObservedAt = tc.apy, o.ObservedAt, o.ObservedAt
			market.EvidenceID = "floor-fixture"
			market.EntryCapacity = Capacity{Known: true, Raw: equity}
			q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), o, market, equity)
			if err != nil {
				t.Fatal(err)
			}
			if q.CostRaw <= int64(q.Cost.QuotedTotalMicros) || q.DestinationExit == nil || q.Cost.EntrySwapCostMicros != 500_000 || q.Cost.ExitSwapCostMicros != 497_500 {
				t.Fatalf("floor-sized costs or quoted NAV evidence lost: %+v", q)
			}
			p := DefaultSelectorPolicy()
			p.Horizon = 7 * 24 * time.Hour
			in := SelectorInput{Now: o.ObservedAt, Snapshot: o.Snapshot, Markets: []LaneEconomics{market}, Quotes: []MoveQuote{q}, Policy: p}
			quoted := in
			quoted.Quotes = []MoveQuote{q}
			quoted.Quotes[0].CostRaw = int64(q.Cost.QuotedTotalMicros)
			quotedResult := SelectOpportunity(quoted, SelectorState{})
			if len(quotedResult.Candidates) != 1 || quotedResult.Candidates[0].BenefitRaw <= float64(p.MinimumBenefitRaw) {
				t.Fatalf("quoted fixture not profitable: %+v", quotedResult)
			}
			result := SelectOpportunity(in, SelectorState{})
			for elapsed := time.Duration(0); elapsed < p.Persistence; elapsed += 5 * time.Minute {
				advanceSelectorFixture(&in, 5*time.Minute)
				result = SelectOpportunity(in, result.State)
			}
			if result.Action.enters() != tc.enter {
				t.Fatalf("entry=%v want %v: %+v", result.Action, tc.enter, result)
			}
			if !tc.enter && (len(result.Candidates) != 1 || result.Candidates[0].BenefitRaw > float64(p.MinimumBenefitRaw)) {
				t.Fatalf("marginal floor economics admitted: %+v", result)
			}
		})
	}
}

func TestLeverageFloorPricesAmountMatchedUnwind(t *testing.T) {
	m, rpc, client, accounts := selectorDestinationFixture(t)
	route, _ := runtimeRoute(MapleSyrupUSDCLaneID)
	cost, err := testLanePricer(t, rpc, client, m, 42).priceStep(context.Background(), accounts, route, []string{route.Kamino.CollateralReserve}, 42, 100_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if cost.EntrySwapCostMicros != 500_000 || cost.UnwindSwapCostMicros != 497_500 || cost.TotalMicros != cost.NetworkMicros+997_500 || cost.QuotedTotalMicros != cost.NetworkMicros {
		t.Fatalf("wrong executable floor cost: %+v", cost)
	}
	market := leverageMarket(route.Lane, .30, .04)
	u := upStep{CollateralMicros: 200_000_000, DebtMicros: 100_000_000, AddedLiabilityMicros: 100_000_000, ProceedsMicros: 100_000_000, KeepDebtAPR: .04, StepDebtAPR: .04, Years: 7 / 365.25}
	u.CostMicros = float64(cost.QuotedTotalMicros)
	if gain := upStepGain(market, u); gain <= 0 {
		t.Fatalf("quoted gain %f", gain)
	}
	u.CostMicros = float64(cost.TotalMicros)
	if gain := upStepGain(market, u); gain >= 0 {
		t.Fatalf("floor trade profitable %f", gain)
	}
	market.NativeAPY = 2
	if gain := upStepGain(market, u); gain <= 0 {
		t.Fatalf("robust trade refused %f", gain)
	}
}

func TestEntryFloorMissingOrZeroHolds(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			m, rpc, client, _ := selectorDestinationFixture(t)
			base := fixtureHTTP(client).Transport
			fixtureHTTP(client).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/quote" {
					out := "1"
					if missing {
						out = "0"
					}
					raw, _ := json.Marshal(jupiter.Quote{InputMint: req.URL.Query().Get("inputMint"), OutputMint: req.URL.Query().Get("outputMint"), InAmount: req.URL.Query().Get("amount"), OutAmount: out, OtherAmountThreshold: "0", SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}})
					return response(string(raw)), nil
				}
				return base.RoundTrip(req)
			})
			q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), flatEntryObservation(42, 100_000_000), leveredTestMarket(MapleSyrupUSDCLaneID), 100_000_000)
			if err == nil || q.EvidenceID != "" {
				t.Fatalf("unknown floor priced: %+v %v", q, err)
			}
		})
	}
}

func TestEntryFloorDoesNotRechargeHeldPosition(t *testing.T) {
	s := leverageSnapshot(1.5)
	armFeeAuthorityFixture(t, &s)
	market := leverageMarket(s.RouteLane, .05, .06)
	settled, err := (lanePricer{manifest: embeddedTestManifest(t)}).settleHeld(context.Background(), Observation{}, s, market)
	if err != nil || settled.CostRaw != 0 || settled.CollateralValueRaw != s.PositionCollateralValueRaw || settled.DebtValueRaw != s.PositionDebtValueRaw {
		t.Fatalf("holding charged entry: %+v %v", settled, err)
	}
}

func TestEntryFloorMarginalNeverAdmitted(t *testing.T) {
	const equity = 100_000_000
	m, rpc, client, _ := selectorDestinationFixtureForLane(t, MapleSyrupUSDCLaneID, blockDebtUtilization(t, MapleSyrupUSDCLaneID))
	o := flatEntryObservation(42, equity)
	market := leveredTestMarket(MapleSyrupUSDCLaneID)
	market.NativeAPY, market.ObservedAt, market.NativeObservedAt, market.EvidenceID = .30, o.ObservedAt, o.ObservedAt, "floor-regression"
	market.EntryCapacity = Capacity{Known: true, Raw: equity}
	q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), o, market, equity)
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultSelectorPolicy()
	p.Horizon = 7 * 24 * time.Hour
	in := SelectorInput{Now: o.ObservedAt, Snapshot: o.Snapshot, Markets: []LaneEconomics{market}, Quotes: []MoveQuote{q}, Policy: p}
	result := SelectOpportunity(in, SelectorState{})
	for elapsed := time.Duration(0); elapsed < p.Persistence; elapsed += 5 * time.Minute {
		advanceSelectorFixture(&in, 5*time.Minute)
		result = SelectOpportunity(in, result.State)
	}
	if result.Action.enters() || result.SelectedQuote != nil {
		t.Fatalf("permitted floor loses money but optional entry admitted: %+v", result)
	}
}

func TestEntryFloorExactHastraConversionKeepsQuotedCost(t *testing.T) {
	c := loadLiveCapture(t, "testdata/live_state_2026-10-12.json.gz")
	o := observeLiveAtRest(t, c, liveInputs(true))
	rpc, view, client := liveReplayStack(t, o, 5)
	m := embeddedTestManifest(t)
	p, err := newLanePricer(context.Background(), rpc, view, client, m, o.Snapshot.Slot)
	if err != nil {
		t.Fatal(err)
	}
	p.policies, _, _ = stepTestPolicies(t)
	route, _ := runtimeRoute(autoAUTOWYLDSLane)
	cost, err := p.priceStep(context.Background(), o.routeBatch.Accounts, route, []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}, o.Snapshot.Slot, 10_000_000_000)
	if err != nil || cost.TotalMicros != cost.QuotedTotalMicros {
		t.Fatalf("exact conversion cost changed: %+v %v", cost, err)
	}
}

func TestEntryFloorExpiredQuoteCollectionHolds(t *testing.T) {
	m, rpc, client, o, market, policy := ladderLiveObservation(t, .5)
	o.ObservedAt = time.Now().UTC().Add(-9 * time.Second)
	_, quotes, err := collectSelectorQuotes(context.Background(), rpc, fixtureView(t, rpc), client, m, o, []LaneEconomics{market}, policy, nil)
	if err == nil || len(quotes) != 0 {
		t.Fatalf("expired collection admitted optional entry: quotes=%+v err=%v", quotes, err)
	}
}
