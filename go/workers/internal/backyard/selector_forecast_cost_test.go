package backyard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func sfIntervalPrice(mint, program string, decimals byte, slot int64) BudgetPrice {
	scale := new(big.Int).Lsh(big.NewInt(1), 60)
	mk := func(num, den int64) [16]byte {
		var out [16]byte
		value := new(big.Int).Mul(scale, big.NewInt(num))
		value.Div(value, big.NewInt(den))
		bytes := value.Bytes()
		for i := range bytes {
			out[i] = bytes[len(bytes)-1-i]
		}
		return out
	}
	return BudgetPrice{
		Credit:           &BudgetCreditBounds{TokenLowerSF: mk(99, 100), USDCUpperSF: mk(101, 100)},
		Source:           "confirmed-chain-accounts",
		Mint:             mint,
		TokenProgram:     program,
		Decimals:         decimals,
		TokenUpperSF:     mk(101, 100),
		USDCLowerSF:      mk(99, 100),
		ObservedSlot:     slot,
		ValidThroughSlot: slot + budgetMaxObservationLagSlots,
		EvidenceSHA256:   sha256Bytes([]byte("interval-price")),
	}
}

func TestSelectorMidpointPriceValueReconstructsCenterAndRefusesMalformed(t *testing.T) {
	p := sfIntervalPrice("Mint11", "Prog11", 6, 42)
	value, err := selectorMidpointPriceValue(p, 1_000_000, "Mint11", "Prog11", 42)
	if err != nil || value < 999_998 || value > 1_000_002 {
		t.Fatal("midpoint drifts from the observed center", value, err)
	}
	// The interval floor and ceiling must stay strictly wider than the center.
	lower, err := p.valueLower(1_000_000, "Mint11", "Prog11", 42)
	upper, err2 := p.valueUpper(1_000_000, "Mint11", "Prog11", 42)
	if err != nil || err2 != nil || !(lower < value && value < upper) {
		t.Fatal("midpoint is not inside the independent interval", lower, value, upper)
	}
	stale := p
	stale.ValidThroughSlot = 42
	if _, err = selectorMidpointPriceValue(stale, 1, "Mint11", "Prog11", 43); err == nil {
		t.Fatal("stale price accepted")
	}
	noCredit := p
	noCredit.Credit = nil
	if _, err = selectorMidpointPriceValue(noCredit, 1, "Mint11", "Prog11", 42); err == nil {
		t.Fatal("missing credit bounds accepted")
	}
	inverted := p
	raised := inverted.TokenUpperSF
	raised[0]++
	inverted.Credit = &BudgetCreditBounds{TokenLowerSF: raised, USDCUpperSF: inverted.Credit.USDCUpperSF}
	if _, err = selectorMidpointPriceValue(inverted, 1, "Mint11", "Prog11", 42); err == nil {
		t.Fatal("inverted interval accepted")
	}
	// A zero floor is malformed, not a free half-price midpoint.
	zeroFloor := p
	zeroFloor.Credit = &BudgetCreditBounds{TokenLowerSF: [16]byte{}, USDCUpperSF: zeroFloor.Credit.USDCUpperSF}
	if _, err = selectorMidpointPriceValue(zeroFloor, 1, "Mint11", "Prog11", 42); err == nil {
		t.Fatal("zero token lower bound accepted")
	}
	if _, err = selectorMidpointPriceValue(p, 1, "OtherMint", "Prog11", 42); err == nil {
		t.Fatal("mint mismatch accepted")
	}
}

func TestComposeExpectedExpenseFallsBackPerRecipeWithoutErasingDestination(t *testing.T) {
	zero := int64(0)
	seven := int64(7)
	destination := selectorRecipe{CostRaw: 400, ExpectedCostRaw: &seven}
	// Flat idle source: zero bound, no forecast pointer, contributes zero and
	// keeps the destination forecast intact.
	if got, err := composeSelectorExpectedExpense(selectorRecipe{}, destination); err != nil || got != 7 {
		t.Fatal("flat idle source erased destination forecast", got, err)
	}
	if got, err := composeSelectorExpectedExpense(selectorRecipe{CostRaw: 0, ExpectedCostRaw: &zero}, destination); err != nil || got != 7 {
		t.Fatal("explicit zero source changed the forecast", got, err)
	}
	// An old source recipe without a pointer degrades to its own bound.
	if got, err := composeSelectorExpectedExpense(selectorRecipe{CostRaw: 100}, destination); err != nil || got != 107 {
		t.Fatal("old source fallback lost", got, err)
	}
	// A corrupt forecast pointer falls back conservatively to its own bound:
	// source 400 (clamped from 401) plus the untouched destination forecast 7.
	tooBig := int64(401)
	if got, err := composeSelectorExpectedExpense(selectorRecipe{CostRaw: 400, ExpectedCostRaw: &tooBig}, destination); err != nil || got != 407 {
		t.Fatal("corrupt pointer not clamped to bound", got, err)
	}
}

func TestMoveQuoteEconomicCostFallsBackAndBindsEvidence(t *testing.T) {
	fallback := MoveQuote{CostRaw: 300_000}
	if fallback.selectorEconomicCostRaw() != 300_000 {
		t.Fatal("old quote must fall back to the bounded cost")
	}
	expected := int64(50_000)
	fresh := MoveQuote{CostRaw: 300_000, ExpectedCostRaw: &expected}
	if fresh.selectorEconomicCostRaw() != 50_000 {
		t.Fatal("forecast cost ignored")
	}
	encoded, err := json.Marshal(fresh)
	if err != nil || !bytes.Contains(encoded, []byte("expectedCostRaw")) {
		t.Fatal("forecast not bound into move evidence", err)
	}
	if encoded, _ := json.Marshal(fallback); bytes.Contains(encoded, []byte("expectedCostRaw")) {
		t.Fatal("nil forecast must stay absent from old-shape evidence")
	}
	// The pure selector comparison consumes the forecast, nothing else.
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	lane := in.Markets[0].Lane
	withBound, withExpected := in.Quotes[0], in.Quotes[0]
	withBound.CostRaw, withExpected.CostRaw = 400_000, 400_000
	withExpected.ExpectedCostRaw = &expected
	gain := func(q MoveQuote) float64 {
		input := in
		input.Quotes = []MoveQuote{q}
		result := SelectOpportunity(input, SelectorState{})
		for _, c := range result.Candidates {
			if c.Lane == lane && c.CostsKnown {
				return c.GainRaw
			}
		}
		return math.NaN()
	}
	older, newer := gain(withBound), gain(withExpected)
	// Continuous dilution makes the shadow-only expense wedge slightly below
	// the old flat 280,000 proxy; it still uses expected, not bound, expense.
	if newer-older < 279_000 || newer-older > 280_000 {
		t.Fatal("selector comparison ignored the forecast expense", older, newer)
	}
}

// quoteLegs records the equity-swap quote sizes actually requested.
func quoteLegsTransport(t *testing.T, base http.RoundTripper, legs *[]uint64, impactThreshold, divisor uint64) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/quote" {
			amount, _ := strconv.ParseUint(req.URL.Query().Get("amount"), 10, 64)
			if req.URL.Query().Get("inputMint") == bridgeUSDC {
				for _, size := range []uint64{10_000_000, 1_000_000, 100_000} {
					if amount == size {
						*legs = append(*legs, amount)
						break
					}
				}
			}
			if req.URL.Query().Get("inputMint") == bridgeUSDC && amount >= impactThreshold {
				out := amount / divisor
				raw, err := json.Marshal(jupiter.Quote{InputMint: bridgeUSDC, OutputMint: req.URL.Query().Get("outputMint"), InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(out), OtherAmountThreshold: fmt.Sprint(out * 995 / 1000), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}})
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
	market.Lane = SelectedRouteID
	market.NativeAPY = nativeAPY
	market.EntryCapacity = Capacity{Known: true, Unlimited: true}
	policy := DefaultSelectorPolicy()
	policy.MinimumBenefitRaw = 0
	return m, rpc, client, o, market, policy
}

func TestLiveSelectorLadderProbesSmallerAfterLargestCostExceedsEquity(t *testing.T) {
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
	// (out=in/100000 on the equity leg), so its whole-move bound cost reaches
	// its own equity: compose must refuse exactly that economic outcome for the
	// first ladder size.
	source, err := observeSelectorSource(context.Background(), rpc, client, m, o)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := observeSelectorDestinationForecast(context.Background(), rpc, client, m, capturedTestPolicies(), SelectedRouteID, 10_000_000, o.Snapshot.Slot, true, nil)
	if err != nil {
		t.Fatal("largest destination quote", err)
	}
	_, err = composeSelectorMove(context.Background(), rpc, o, source, destination)
	var hold *BudgetHold
	if !errors.As(err, &hold) || hold.Reason != "selector_move_cost_exceeds_equity" {
		t.Fatal("largest refusal is not the economic cost outcome", err)
	}
	// The ladder continues past that refused largest size and stops at the
	// smaller profitable 1M quote.
	legs = nil
	observed, quotes, err := collectSelectorQuotes(context.Background(), rpc, client, m, o, []LaneEconomics{market}, policy, 10_000_000)
	if err != nil || len(quotes) != 1 {
		t.Fatal("smaller profitable size not probed after economic failure", err, quotes, observed)
	}
	if quotes[0].EquityRaw != 1_000_000 || observed[0].EntryCapacity.Raw != quotes[0].EquityRaw || observed[0].EntryBlockedReason != "" {
		t.Fatal("published quote is not the smaller profitable size", quotes[0], observed[0])
	}
	if len(legs) != 2 || legs[0] != 10_000_000 || legs[1] != 1_000_000 {
		t.Fatal("ladder legs", legs)
	}
	if quotes[0].ExpectedCostRaw == nil || *quotes[0].ExpectedCostRaw < 0 || *quotes[0].ExpectedCostRaw > quotes[0].CostRaw {
		t.Fatal("published quote lost its forecast expense", quotes[0].ExpectedCostRaw, quotes[0].CostRaw)
	}
	if benefit, admissible := selectorMoveQuoteBenefit(m, o, []LaneEconomics{market}, market.Lane, policy, quotes[0]); !admissible || benefit <= 0 {
		t.Fatal("smaller candidate is not positively profitable", benefit, admissible)
	}
}

func TestLiveSelectorUnprofitableSizesStillPublishBestDiagnostics(t *testing.T) {
	m, rpc, client, o, market, policy := ladderLiveObservation(t, .5)
	policy.MinimumBenefitRaw = 1_000_000_000_000
	var legs []uint64
	fixtureHTTP(client).Transport = quoteLegsTransport(t, fixtureHTTP(client).Transport, &legs, math.MaxUint64, 1)
	observed, quotes, err := collectSelectorQuotes(context.Background(), rpc, client, m, o, []LaneEconomics{market}, policy, 10_000_000)
	if err != nil || len(quotes) != 1 {
		t.Fatal("bound-valid diagnostic quote dropped", err, quotes, observed)
	}
	if observed[0].EntryBlockedReason != "" || observed[0].EntryCapacity.Raw != quotes[0].EquityRaw {
		t.Fatal("unprofitable diagnostic mislabeled", observed[0])
	}
	if len(legs) != 3 {
		t.Fatal("insufficient largest quote must ladder down", legs)
	}
	in := SelectorInput{Now: time.Now().UTC(), Snapshot: o.Snapshot, Markets: observed, Quotes: quotes, Policy: policy}
	if got := SelectOpportunity(in, SelectorState{}); got.Action != "KEEP" || got.Reason != "no_worthwhile_executable_move" {
		t.Fatal("pure selector must keep on unprofitable diagnostics", got)
	}
}

func TestRecipeExpectedCostExcludesMarginWhileBoundKeepsFloor(t *testing.T) {
	m, rpc, client, _ := selectorDestinationFixture(t)
	q, err := observeSelectorDestinationForecast(context.Background(), rpc, client, m, capturedTestPolicies(), SelectedRouteID, 1_000_000, 42, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture quotes zero route impact, so the expected expense carries no
	// price-interval margin or slippage allowance, while the bounded CostRaw
	// keeps every conservative component for admission and reservations.
	if q.Recipe.ExpectedCostRaw == nil || *q.Recipe.ExpectedCostRaw <= 0 || *q.Recipe.ExpectedCostRaw >= q.Recipe.CostRaw {
		t.Fatal("expected expense not strictly inside the bound", q.Recipe.ExpectedCostRaw, q.Recipe.CostRaw)
	}
	if q.Recipe.CostRaw-*q.Recipe.ExpectedCostRaw < 10_000 {
		t.Fatal("bound lost its margin wedge", q.Recipe.CostRaw-*q.Recipe.ExpectedCostRaw)
	}
	if q.Recipe.CostRaw <= 0 || q.Recipe.CostRaw >= 1_000_000 {
		t.Fatal("bound economics drifted", q.Recipe.CostRaw)
	}
}

func TestLiveSelectorLadderStopsAfterQuoteWindowBudget(t *testing.T) {
	m, rpc, client, o, market, policy := ladderLiveObservation(t, .5)
	policy.UncertaintyBPS = 0
	var legs []uint64
	fixtureHTTP(client).Transport = quoteLegsTransport(t, fixtureHTTP(client).Transport, &legs, 5_000_000, 100_000)
	// Same economics as the ladder test above, but the observation is already
	// past the ladder budget: only the largest size is priced, so no smaller
	// quote can arrive with too few slots left to allocate.
	o.ObservedAt = time.Now().UTC().Add(-selectorLadderBudget - time.Second)
	_, quotes, err := collectSelectorQuotes(context.Background(), rpc, client, m, o, []LaneEconomics{market}, policy, 10_000_000)
	if err != nil || len(quotes) != 0 || len(legs) != 1 || legs[0] != 10_000_000 {
		t.Fatal("ladder priced smaller sizes after its budget", err, quotes, legs)
	}
}
