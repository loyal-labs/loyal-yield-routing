package backyard

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// The selector replay: the live book of 2026-10-12
// (testdata/live_state_2026-10-12.json.gz) and the synthetic flat-vault
// stacks, decided by the selector and priced by the one pricer. Realized
// native yields are fixed per collateral (the Oct 11 30-day figures: AUTO
// 8.87%, syrupUSDC 4.5%, ONyc 11.14%, PRIME 4%, USDe 0%): the capture holds
// the chain, not Kamino's price history.
var replayNativeAPY = map[string]float64{"AUTO": 0.0887, "syrupUSDC": 0.045, "ONyc": 0.1114, "PRIME": 0.04, "USDe": 0}

func replayCollateral(lane string) string {
	for i := 0; i < len(lane); i++ {
		if lane[i] == '/' {
			rest := lane[i+1:]
			for j := 0; j < len(rest); j++ {
				if rest[j] == '/' {
					return rest[:j]
				}
			}
		}
	}
	return ""
}

// replayMarkets are the active lanes' economics from accounts, observed at now.
func replayMarkets(t *testing.T, accounts []ConfirmedAccount, now time.Time) []LaneEconomics {
	t.Helper()
	routes, err := earnFeedRoutes()
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{usdcReference.DebtReserve: true}
	for _, r := range routes {
		wanted[r.Kamino.CollateralReserve], wanted[r.Kamino.DebtReserve] = true, true
	}
	reserves := map[string]reserveEconomics{}
	for _, a := range accounts {
		if !wanted[a.Address] || a.Owner != kamino.ProgramID.String() {
			continue
		}
		r, err := decodeReserveEconomics(a, func() float64 { return 2.5 })
		if err != nil {
			continue
		}
		r.ObservedAt = now
		reserves[r.Reserve] = r
	}
	yields := map[string]nativeYield{}
	for _, r := range routes {
		if apy, ok := replayNativeAPY[replayCollateral(r.Lane)]; ok {
			yields[r.Lane] = nativeYield{APY: apy, ObservedAt: now, EvidenceID: "replay"}
		}
	}
	return combineEconomics(routes, reserves, yields, nil, now, DefaultSelectorPolicy())
}

// liveReplayStack serves the live batch to the pricers: a fake chain holding
// every captured account plus the price observer's mints and a SOL reserve at
// $150 updated at the batch's clock, the account view over it, and a Jupiter
// that quotes every edge at the batch's own reserve prices less lossBPS.
func liveReplayStack(t *testing.T, o Observation, lossBPS int64) (*chain.Client, *View, *jupiter.Client) {
	t.Helper()
	accounts := append([]ConfirmedAccount(nil), o.routeBatch.Accounts...)
	route := autoAUTOPYUSD
	prices, err := batchLanePrices(o.routeBatch.Accounts, route)
	if err != nil {
		t.Fatal(err)
	}
	mints := autoPayoffMints(t, route)
	mints[0].Data[44], mints[1].Data[44] = prices.collateralDecimals, prices.debtDecimals
	accounts = append(accounts, mints...)
	unix := clockUnixTimestamp(accounts)
	sol := reserveFixture(t, budgetSOLReserve, budgetWrappedSOLMint, o.Snapshot.Slot, new(big.Int).Mul(new(big.Int).Lsh(big.NewInt(1), 60), big.NewInt(150)), 1, 1)
	putKey(t, sol.Data[32:64], budgetSOLMarket)
	binary.LittleEndian.PutUint64(sol.Data[272:280], 9)
	binary.LittleEndian.PutUint64(sol.Data[264:272], uint64(unix))
	// Hastra's stake price at about the mainnet price, verified at the batch's
	// clock: the overflow lane's conversions price through it.
	accounts = append(accounts, sol, hastraPriceAccount(hastraTestPrice, unix))
	rpc := budgetBuildRPCWithAccounts(t, 5000, o.Snapshot.Slot, accounts)
	quote := func(in, out string, amount uint64) (uint64, uint64) {
		price := func(mint string) ([16]byte, uint8) {
			switch mint {
			case route.Kamino.CollateralMint:
				return prices.collateral, prices.collateralDecimals
			case route.Kamino.DebtMint:
				return prices.debt, prices.debtDecimals
			}
			return prices.usdc, 6
		}
		inPrice, inDecimals := price(in)
		outPrice, outDecimals := price(out)
		value, err := valueBetweenTokenRaw(amount, inDecimals, outDecimals, inPrice, outPrice, false)
		if err != nil {
			t.Fatal(err)
		}
		value -= value * uint64(lossBPS) / 10_000
		return value, value * 9950 / 10_000
	}
	return rpc, fixtureView(t, rpc), autoJupiterTransport(t, route, quote, nil)
}

// The live book: both AUTO lanes at their stored 1.75 level. The selector
// keeps the held lanes' capital, decides each lane's level from its spread
// without pricing a step (both sit on their level), and decides no overflow
// level (the overflow lane is held). The pricer is never asked.
func TestSelectorReplayLiveBook(t *testing.T) {
	t.Parallel()
	c := loadLiveCapture(t, "testdata/live_state_2026-10-12.json.gz")
	p := DefaultSelectorPolicy()
	type laneDecision struct {
		next      float64
		reason    string
		borrowRaw uint64
		level     laneLevel
	}
	want := map[bool]map[string]laneDecision{
		false: {
			autoAUTOPYUSD.Lane: {next: 1.75, reason: "spread_rule", level: laneLevel{Level: 1.75}},
			autoAUTOWYLDSLane:  {next: 1.75, reason: "spread_rule", level: laneLevel{Level: 1.75}},
		},
		true: {
			autoAUTOPYUSD.Lane: {next: 1.75, reason: "spread_rule", level: laneLevel{Level: 1.75}},
			autoAUTOWYLDSLane:  {next: 1.75, reason: "spread_rule", level: laneLevel{Level: 1.75}},
		},
	}
	for _, approved := range []bool{false, true} {
		o := observeLiveAtRest(t, c, liveInputs(approved))
		markets := replayMarkets(t, o.routeBatch.Accounts, c.Now)
		result := SelectOpportunity(SelectorInput{Now: c.Now, Snapshot: o.Snapshot, Markets: markets, Policy: p}, SelectorState{})
		if result.Action != "KEEP" || result.Reason != "held_lanes_keep_their_capital" || result.SelectedQuote != nil {
			t.Fatalf("approved=%v: selector %s %s", approved, result.Action, result.Reason)
		}
		for _, lane := range o.lanes {
			priced := false
			d, ok := decideLeverageTarget(lane, result, markets, p, func(uint64) (leverageStepCost, error) {
				priced = true
				return leverageStepCost{}, costOutage("inputs", budgetHold("replay"))
			})
			got := laneDecision{next: d.Next, reason: d.Reason, borrowRaw: d.BorrowRaw, level: d.level()}
			t.Logf("approved=%v %s: %.2fx->%.2fx spread=%d bps reason=%s store=%v", approved, lane.RouteLane, d.Current, d.Next, d.SpreadBPS, d.Reason, d.storeChanges(lane))
			if !ok || priced || got != want[approved][lane.RouteLane] {
				t.Fatalf("approved=%v %s: decided %+v (priced %v)", approved, lane.RouteLane, got, priced)
			}
			if overflow, level, decided := decideOverflowLevel(lane, markets, p, c.Now); decided {
				t.Fatalf("%s decided an overflow level for %s: %+v", lane.RouteLane, overflow, level)
			}
		}
	}
}

// One up step priced on the live batch, the old way and the one way: the
// same Jupiter economics, the same reserve prices. The old pricer charged
// six transactions (borrow, swap, deposit; withdraw, swap, repay) the
// executor no longer sends; the step is two (up and down).
func TestSelectorReplayLiveStepCost(t *testing.T) {
	t.Parallel()
	c := loadLiveCapture(t, "testdata/live_state_2026-10-12.json.gz")
	o := observeLiveAtRest(t, c, liveInputs(true))
	rpc, view, client := liveReplayStack(t, o, 5)
	m := embeddedTestManifest(t)
	var lane Snapshot
	for _, l := range o.lanes {
		if l.RouteLane == autoAUTOPYUSD.Lane {
			lane = l
		}
	}
	const borrow = 10_000_000_000 // 10,000 PYUSD
	old, oldErr := productionLeverageStepPricer(context.Background(), rpc, view, client, m, lane)(borrow)
	pricer, err := newLanePricer(context.Background(), rpc, view, client, m, lane.Slot)
	if err != nil {
		t.Fatal(err)
	}
	topology := []string{autoAUTOPYUSD.Kamino.CollateralReserve, autoAUTOPYUSD.Kamino.DebtReserve}
	got, err := pricer.priceStep(context.Background(), o.routeBatch.Accounts, autoAUTOPYUSD, topology, lane.Slot, borrow)
	var outage *leverageCostOutage
	if errors.As(oldErr, &outage) {
		t.Logf("old cause: %v", outage.err)
	}
	t.Logf("old %+v (%v)", old, oldErr)
	t.Logf("new %+v (%v)", got, err)
	if oldErr != nil || err != nil {
		t.Fatal(oldErr, err)
	}
	// The level decision on the live economics for a lane that sits at 1.5x
	// under a stored 1.75: the whole step to 1.75 priced both ways.
	markets := replayMarkets(t, o.routeBatch.Accounts, c.Now)
	below := lane
	below.LTVBPS, below.LeverageDesired175Raw = leverageLevelLTVBPS(1.5), borrow
	keep := SelectorResult{Action: "KEEP"}
	p := DefaultSelectorPolicy()
	oldDecision, _ := decideLeverageTarget(below, keep, markets, p, productionLeverageStepPricer(context.Background(), rpc, view, client, m, below))
	newDecision, _ := decideLeverageTarget(below, keep, markets, p, func(raw uint64) (leverageStepCost, error) {
		return pricer.priceStep(context.Background(), o.routeBatch.Accounts, autoAUTOPYUSD, topology, lane.Slot, raw)
	})
	t.Logf("old %s", oldDecision.logLine())
	t.Logf("new %s", newDecision.logLine())
	// The overflow lane converts through Hastra: the same producer the old
	// pricer called (prepareHastraQuoteEvidence) through the step.
	hastraPricer := pricer
	hastraPricer.policies, _, _ = stepTestPolicies(t)
	wyldsRoute, _ := runtimeRoute(autoAUTOWYLDSLane)
	wylds, err := hastraPricer.priceStep(context.Background(), o.routeBatch.Accounts, wyldsRoute, []string{wyldsRoute.Kamino.CollateralReserve, wyldsRoute.Kamino.DebtReserve}, lane.Slot, borrow)
	if errors.As(err, &outage) {
		t.Logf("wYLDS cause: %v", outage.err)
	}
	t.Logf("wYLDS %+v (%v)", wylds, err)
}

// entryReplay is one flat-vault entry decided on a synthetic stack: the
// quote's numbers and the selector's choice over two samples a persistence
// window apart.
type entryReplay struct {
	equity, borrow, fee int64
	unlevered           bool
	cost                int64
	invested            int64
	benefit             float64
	leverage            float64
	action, reason      string
	blocked             string
	destination         string
	entered             int64
}

func decideEntry(t *testing.T, s Snapshot, market LaneEconomics, q MoveQuote) entryReplay {
	t.Helper()
	now := time.Now().UTC()
	p := DefaultSelectorPolicy()
	p.Persistence, p.MinimumBenefitRaw, p.UncertaintyBPS = time.Minute, 1, 0
	market.ObservedAt, market.NativeObservedAt, market.EntryCapacity, market.EvidenceID = now, now, Capacity{Known: true, Unlimited: true}, "replay"
	q.ObservedAt = now
	in := SelectorInput{Now: now, Snapshot: s, Markets: []LaneEconomics{market}, Quotes: []MoveQuote{q}, Policy: p}
	first := SelectOpportunity(in, SelectorState{})
	advanceSelectorFixture(&in, 2*time.Minute)
	result := SelectOpportunity(in, first.State)
	c := result.Candidates[0]
	out := entryReplay{equity: q.EquityRaw, borrow: int64(q.BorrowReceiveRaw), fee: int64(q.BorrowFeeRaw), unlevered: q.Unlevered, cost: q.selectorEconomicCostRaw(),
		invested: c.InvestedRaw, benefit: c.BenefitRaw, leverage: c.Leverage, action: result.Action, reason: result.Reason, blocked: c.BlockedReason, destination: result.DestinationLane}
	if result.SelectedQuote != nil {
		out.entered = result.SelectedQuote.EquityRaw
	}
	return out
}

// A flat vault's entry, priced by the old multi-leg recipe and by the one
// pricer on the same synthetic stack, and decided by the same selector.
func TestSelectorReplayFlatEntry(t *testing.T) {
	t.Parallel()
	type stack struct {
		name   string
		lane   string
		slot   int64
		equity uint64
		build  func(t *testing.T) (RouteManifest, *chain.Client, *jupiter.Client)
	}
	for _, c := range []stack{
		{"maple", SelectedRouteID, 42, 100_000_000, func(t *testing.T) (RouteManifest, *chain.Client, *jupiter.Client) {
			m, rpc, client, _ := selectorDestinationFixture(t)
			return m, rpc, client
		}},
		{"auto", autoAUTOPYUSD.Lane, 77, 200_000_000, func(t *testing.T) (RouteManifest, *chain.Client, *jupiter.Client) {
			m, _, rpc, client := autoCandidateStack(t, 77, nil)
			return m, rpc, client
		}},
		// Every AUTO edge 30 bps below the coherent prices.
		{"auto-lossy", autoAUTOPYUSD.Lane, 77, 200_000_000, func(t *testing.T) (RouteManifest, *chain.Client, *jupiter.Client) {
			m, route, rpc, _ := autoCandidateStack(t, 77, nil)
			coherent := autoCandidateQuote(route)
			return m, rpc, autoJupiterTransport(t, route, func(in, out string, amount uint64) (uint64, uint64) {
				quoted, _ := coherent(in, out, amount)
				quoted -= quoted * 30 / 10_000
				return quoted, quoted
			}, nil)
		}},
	} {
		for _, spread := range []string{"levered", "unlevered"} {
			t.Run(c.name+"/"+spread, func(t *testing.T) {
				m, rpc, client := c.build(t)
				view := fixtureView(t, rpc)
				s := base()
				s.Slot, s.ObservationID, s.VoltrIdleRaw, s.TotalVaultNAVRaw = c.slot, "replay-"+c.name, int64(c.equity), int64(c.equity)
				o := tickObservation(s)
				o.ObservedAt, o.policies = time.Now().UTC(), capturedTestPolicies()
				market := leveredTestMarket(c.lane)
				if spread == "unlevered" {
					// 0.5 points of spread: under the 1.5x rule's 1 point.
					market = leverageMarket(c.lane, 0.08, math.Log1p(0.075))
				}
				oldQuote, err := observeSelectorMove(context.Background(), rpc, view, client, m, o, market, c.equity, 0)
				if err != nil {
					t.Fatal(err)
				}
				sol, err := ObserveNativeSOLBudgetPrice(context.Background(), rpc, view, c.slot)
				if err != nil {
					t.Fatal(err)
				}
				pricer := lanePricer{rpc: rpc, view: view, client: client, manifest: m, policies: capturedTestPolicies(), sol: sol}
				newQuote, err := pricer.priceEntry(context.Background(), o, market, c.equity)
				if err != nil {
					t.Fatal(err)
				}
				old, got := decideEntry(t, o.Snapshot, market, oldQuote), decideEntry(t, o.Snapshot, market, newQuote)
				od, _ := oldQuote.borrowDebtUSDCRaw()
				op, _ := oldQuote.borrowProceedsUSDCRaw()
				t.Logf("old debt %d proceeds %d spreadUnlevered %v; new debt %d proceeds %d spreadUnlevered %v", od, op, oldQuote.SpreadUnlevered, newQuote.BorrowValueRaw, newQuote.ProceedsValueRaw, newQuote.SpreadUnlevered)
				t.Logf("old %+v", old)
				t.Logf("new %+v cost %+v", got, newQuote.Cost)
			})
		}
	}
}
