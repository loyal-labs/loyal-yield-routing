package backyard

import (
	"context"
	"encoding/binary"
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

// The old pricers' outputs on this replay, recorded at 4681fa2d, which ran
// them side by side with the one pricer before they were deleted: the
// six-transaction step pricer and the multi-leg entry recipe. Their
// differences from the one pricer are explained where they are asserted.
var (
	// AUTO/AUTO/PYUSD, 10,000 PYUSD up step on the live batch, every
	// conversion quoted 5 bps under the batch's prices.
	replayOldLiveStep     = leverageStepCost{EntrySwapLossMicros: 4_999_356, ExitSwapLossMicros: 4_996_856, NetworkMicros: 4_596, TotalMicros: 10_000_808}
	replayOldLiveStepGain = 7_531_267.0
)

// One up step priced on the live batch: the conversions of the up and down
// steps the executor assembles, at the batch's prices. The old pricer quoted
// the same conversions and valued them at the midpoint of separately
// observed price intervals, which is the batch price: the swap losses agree
// to the micro. It charged six transactions (borrow, swap, deposit;
// withdraw, swap, repay) the executor no longer sends; the step is two.
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
	got, err := productionLeverageStepPricer(context.Background(), rpc, view, client, m, o, lane)(borrow)
	if err != nil {
		t.Fatal(err)
	}
	old := replayOldLiveStep
	if got.BorrowFeeMicros != old.BorrowFeeMicros || got.EntrySwapLossMicros != old.EntrySwapLossMicros || got.ExitSwapLossMicros != old.ExitSwapLossMicros {
		t.Fatalf("conversion losses moved: %+v, old %+v", got, old)
	}
	if got.NetworkMicros*3 != old.NetworkMicros || got.TotalMicros != old.TotalMicros-old.NetworkMicros+got.NetworkMicros {
		t.Fatalf("network fees: %+v is not two of the old six transactions %+v", got, old)
	}
	// The level decision on the live economics for a lane at 1.5x under a
	// stored 1.75: the whole step to 1.75 is approved as before, its gain
	// higher by the four transactions it no longer charges (and the growth
	// on that capital).
	markets := replayMarkets(t, o.routeBatch.Accounts, c.Now)
	below := lane
	below.LTVBPS, below.LeverageDesired175Raw = leverageLevelLTVBPS(1.5), borrow
	d, ok := decideLeverageTarget(below, SelectorResult{Action: "KEEP"}, markets, DefaultSelectorPolicy(), productionLeverageStepPricer(context.Background(), rpc, view, client, m, o, below))
	t.Log(d.logLine())
	if !ok || d.Reason != "up_move_approved" || d.Next != 1.75 || d.BorrowRaw != borrow || d.CostRaw != float64(got.TotalMicros) ||
		d.GainRaw < replayOldLiveStepGain || d.GainRaw-replayOldLiveStepGain > 1.01*float64(old.TotalMicros-got.TotalMicros) {
		t.Fatalf("live level decision moved: %+v", d)
	}
	// The overflow lane converts through Hastra, the same producer the old
	// pricer called, here through the step.
	pricer, err := newLanePricer(context.Background(), rpc, view, client, m, lane.Slot)
	if err != nil {
		t.Fatal(err)
	}
	pricer.policies, _, _ = stepTestPolicies(t)
	wyldsRoute, _ := runtimeRoute(autoAUTOWYLDSLane)
	wylds, err := pricer.priceStep(context.Background(), o.routeBatch.Accounts, wyldsRoute, []string{wyldsRoute.Kamino.CollateralReserve, wyldsRoute.Kamino.DebtReserve}, lane.Slot, borrow)
	t.Logf("wYLDS %+v", wylds)
	if err != nil || wylds.NetworkMicros != got.NetworkMicros || wylds.TotalMicros <= wylds.NetworkMicros {
		t.Fatalf("Hastra step: %+v (%v)", wylds, err)
	}
}

// entryReplay is one flat-vault entry decided on a synthetic stack: the
// quote's numbers and the selector's choice over two samples a persistence
// window apart.
type entryReplay struct {
	borrow    int64
	unlevered bool
	cost      int64
	benefit   float64
	action    string
	entered   int64
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
	if c.BlockedReason != "" || result.DestinationLane != "" && result.DestinationLane != q.DestinationLane {
		t.Fatalf("candidate %+v result %s %s", c, result.Action, result.Reason)
	}
	out := entryReplay{borrow: int64(q.BorrowReceiveRaw), unlevered: q.Unlevered, cost: q.CostRaw, benefit: c.BenefitRaw, action: result.Action}
	if result.SelectedQuote != nil {
		out.entered = result.SelectedQuote.EquityRaw
	}
	return out
}

// A flat vault's entry priced by the one pricer on the synthetic stacks and
// decided by the selector, against the old multi-leg recipe's decisions on
// the same stacks (recorded at 4681fa2d). Every decision is the same: the
// lane, the equity entered, 1x or levered and the borrow (within the old
// recipe's one-raw receipt-rounding term). The cost is lower: the old recipe
// charged the fees of about twenty messages (NAV reports and Voltr legs
// included) and the receipt-rounding windows of its separate legs; the one
// pricer charges the entry swap, the deposit, the up and down steps and the
// atomic exit step, at most $0.01 apart here. The benefit is higher by that, and, on PYUSD debt, by valuing the
// borrow at central prices: the old recipe valued the liability at the
// price interval's top and the proceeds at its bottom, 1% each way (within
// 3% of the benefit).
func TestSelectorReplayFlatEntry(t *testing.T) {
	t.Parallel()
	type stack struct {
		name   string
		lane   string
		slot   int64
		equity uint64
		build  func(t *testing.T) (RouteManifest, *chain.Client, *jupiter.Client)
	}
	old := map[string]entryReplay{
		"maple/levered":        {borrow: 49_700_249, cost: 6_138, benefit: 1_638_930.34, action: "ENTER", entered: 100_000_000},
		"maple/unlevered":      {unlevered: true, cost: 6_137, benefit: 502_045.13, action: "ENTER", entered: 100_000_000},
		"auto/levered":         {borrow: 93_000_000, cost: 11_825, benefit: 3_188_127.53, action: "ENTER", entered: 200_000_000},
		"auto/unlevered":       {unlevered: true, cost: 6_138, benefit: 1_009_023.99, action: "ENTER", entered: 200_000_000},
		"auto-lossy/levered":   {borrow: 93_000_000, cost: 1_763_314, benefit: 1_766_891.64, action: "ENTER", entered: 200_000_000},
		"auto-lossy/unlevered": {unlevered: true, cost: 1_201_345, benefit: 47_429.22, action: "ENTER", entered: 200_000_000},
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
			name := c.name + "/" + spread
			t.Run(name, func(t *testing.T) {
				m, rpc, client := c.build(t)
				market := leveredTestMarket(c.lane)
				if spread == "unlevered" {
					// 0.5 points of spread: under the 1.5x rule's 1 point.
					market = leverageMarket(c.lane, 0.08, math.Log1p(0.075))
				}
				o := flatEntryObservation(c.slot, int64(c.equity))
				q, err := testLanePricer(t, rpc, client, m, c.slot).priceEntry(context.Background(), o, market, c.equity)
				if err != nil {
					t.Fatal(err)
				}
				got, want := decideEntry(t, o.Snapshot, market, q), old[name]
				t.Logf("old %+v", want)
				t.Logf("new %+v cost %+v", got, q.Cost)
				if got.action != want.action || got.entered != want.entered || got.unlevered != want.unlevered || q.SpreadUnlevered != want.unlevered ||
					got.borrow < want.borrow || got.borrow > want.borrow+1 {
					t.Fatalf("decision changed: %+v, old %+v", got, want)
				}
				if got.cost <= 0 || got.cost > want.cost || want.cost-got.cost > 10_000 {
					t.Fatalf("cost %d against the old %d", got.cost, want.cost)
				}
				if got.benefit < want.benefit || got.benefit > want.benefit*1.03 {
					t.Fatalf("benefit %.2f against the old %.2f", got.benefit, want.benefit)
				}
			})
		}
	}
}
