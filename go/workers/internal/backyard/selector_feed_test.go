package backyard

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func nativeFixture(t *testing.T) (RuntimeRoute, time.Time, string) {
	t.Helper()
	r, _ := runtimeRoute(SelectedRouteID)
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	return r, now, `{"` + r.Kamino.CollateralReserve + `":{"token":"` + r.Kamino.CollateralMint + `","underlyingApy":{"current":"0.04965083163","sourceType":"yield-feed","sourceMint":"` + r.Kamino.CollateralMint + `","observedAt":"2026-09-15T23:00:13.804Z"},"borrowCaps":{"globalDebt":{"capacity":"0"}},"actualAvailableLiquidityUsd":"0"}}`
}

// realizedFixture is a lane's realized native yield, observed an hour ago.
func realizedFixture(lane string, now time.Time, apy float64) map[string]nativeYield {
	return map[string]nativeYield{lane: {APY: apy, ObservedAt: now.Add(-time.Hour), EvidenceID: "realized"}}
}

// The yield feed is display only, but what it reports is still read exactly.
func TestFeedYieldAPIContractAndMissingValues(t *testing.T) {
	t.Parallel()
	r, now, body := nativeFixture(t)
	got, err := parseFeedYields([]byte(body), []RuntimeRoute{r}, now, 2*time.Hour)
	if err != nil || got[r.Lane] != .04965083163 {
		t.Fatalf("official decimal string: %+v %v", got, err)
	}
	for _, changed := range []string{
		strings.Replace(body, `"current":"0.04965083163"`, `"current":null`, 1),
		strings.Replace(body, `"current":"0.04965083163"`, `"current":"NaN"`, 1),
		strings.Replace(body, `"sourceType":"yield-feed"`, `"sourceType":"price-premium"`, 1),
		strings.Replace(body, `"sourceMint":"`+r.Kamino.CollateralMint+`"`, `"sourceMint":"wrong"`, 1),
		strings.Replace(body, "2026-09-15T23:00:13.804Z", "2026-09-16T01:00:00Z", 1),
		strings.Replace(body, "2026-09-15T23:00:13.804Z", "2026-09-15T12:00:00Z", 1),
	} {
		parsed, _ := parseFeedYields([]byte(changed), []RuntimeRoute{r}, now, 2*time.Hour)
		if len(parsed) != 0 {
			t.Fatal("invalid native evidence accepted")
		}
	}
	zero := strings.Replace(body, `"current":"0.04965083163"`, `"current":"0"`, 1)
	parsed, err := parseFeedYields([]byte(zero), []RuntimeRoute{r}, now, 2*time.Hour)
	if err != nil || len(parsed) != 1 || parsed[r.Lane] != 0 {
		t.Fatal("known zero confused with absent yield")
	}
}
func TestFeedYieldHTTPBoundaries(t *testing.T) {
	t.Parallel()
	r, now, body := nativeFixture(t)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != "GET" || req.URL.Query().Get("reserve") != r.Kamino.CollateralReserve {
			t.Fatal("unexpected request")
		}
		return response(body), nil
	})}
	got, err := fetchFeedYields(context.Background(), client, "https://kamino.invalid/reserves/batch/stats", []RuntimeRoute{r}, now, 2*time.Hour)
	if err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	for _, status := range []int{429, 503} {
		client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("untrusted body"))}, nil
		})
		if _, err := fetchFeedYields(context.Background(), client, "https://kamino.invalid", []RuntimeRoute{r}, now, time.Hour); err == nil || strings.Contains(err.Error(), "untrusted") {
			t.Fatal("HTTP failure not bounded")
		}
	}
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return response(strings.Repeat("x", (2<<20)+1)), nil })
	if _, err := fetchFeedYields(context.Background(), client, "https://kamino.invalid", []RuntimeRoute{r}, now, time.Hour); err == nil {
		t.Fatal("oversized response accepted")
	}
}
func TestReserveFeedKeepsIdentityAndDoesNotInventPairCapacity(t *testing.T) {
	t.Parallel()
	r, now, _ := nativeFixture(t)
	native := realizedFixture(r.Lane, now, 0.05)
	f := selectorFixture().Markets[0]
	supply := 1_000_000_000_000.0
	borrow := 500_000_000_000.0
	c := reserveEconomics{Reserve: r.Kamino.CollateralReserve, Market: r.Kamino.Market, Mint: r.Kamino.CollateralMint, ObservedAt: now, Slot: 42, Hash: strings.Repeat("a", 64), SupplyAPY: f.SupplyAPY}
	d := c
	d.Reserve = r.Kamino.DebtReserve
	d.Mint = bridgeUSDC
	d.SupplyRaw = supply
	d.BorrowRaw = borrow
	d.HostBPS = f.HostBorrowBPS
	// Real collector padding from the September 16 read-only response.
	if err := json.Unmarshal([]byte(`[{"utilization_rate_bps":0,"borrow_rate_bps":0},{"utilization_rate_bps":9000,"borrow_rate_bps":364},{"utilization_rate_bps":10000,"borrow_rate_bps":1075},{"utilization_rate_bps":10000,"borrow_rate_bps":1075}]`), &d.Curve); err != nil {
		t.Fatal(err)
	}
	rows := map[string]reserveEconomics{c.Reserve: c, d.Reserve: d}
	got := combineEconomics([]RuntimeRoute{r}, rows, native, nil, now, DefaultSelectorPolicy())
	if len(got) != 1 || got[0].EntryCapacity.Known || got[0].DebtSupplyRaw != supply || got[0].DebtBorrowRaw != borrow {
		t.Fatalf("feed: %+v", got)
	}
	curveAPR := (364.0*5000/9000 + f.HostBorrowBPS) / 10_000 // 50% utilization
	if apr, err := projectedBorrowAPR(got[0], 0); err != nil || math.Abs(apr-curveAPR) > 1e-12 || math.Abs(got[0].CurrentBorrowAPY-math.Expm1(curveAPR)) > 1e-12 {
		t.Fatalf("borrow not priced off the plain curve: curve %v got %v / %v %v", curveAPR, apr, got[0].CurrentBorrowAPY, err)
	}
	projected := got[0]
	projected.BorrowCurve = []BorrowCurvePoint{{0, 0}, {10000, 1000}}
	apr, err := projectedBorrowAPR(projected, 100_000_000_000)
	if err != nil || math.Abs(apr-(0.06+projected.HostBorrowBPS/10000)) > 1e-12 {
		t.Fatalf("raw-unit borrow should raise utilization from 50 to 60 percent: %v %v", apr, err)
	}

	d.Mint = "wrong"
	rows[d.Reserve] = d
	if len(combineEconomics([]RuntimeRoute{r}, rows, native, nil, now, DefaultSelectorPolicy())) != 0 {
		t.Fatal("feed identity mismatch accepted")
	}
}
func TestSelectorObservesPriorCustodyAndHoldsEveryExposedLane(t *testing.T) {
	t.Parallel()
	accounts := []ConfirmedAccount{}
	for _, lane := range earnLaneIDs(true) {
		r, _ := runtimeRoute(lane)
		accounts = append(accounts, ConfirmedAccount{Address: r.Kamino.Obligation}, tokenAccountFixture(t, r.CollateralCustody, r.Kamino.CollateralMint, bridgeVault, 0))
	}
	source, _ := runtimeRoute("OnRe/ONyc/USDC")
	for i := range accounts {
		if accounts[i].Address == source.CollateralCustody {
			accounts[i] = tokenAccountFixture(t, source.CollateralCustody, source.Kamino.CollateralMint, bridgeVault, 1)
		}
	}
	got, err := observedSelectorHeldLanes(rwa, accounts, earnLaneIDs(true), SelectedRouteID)
	if err != nil || strings.Join(got, ",") != source.Lane {
		t.Fatalf("old custody hidden: %v %v", got, err)
	}
	other, _ := runtimeRoute(SelectedRouteID)
	for i := range accounts {
		if accounts[i].Address == other.CollateralCustody {
			accounts[i] = tokenAccountFixture(t, other.CollateralCustody, other.Kamino.CollateralMint, bridgeVault, 1)
		}
	}
	// Two exposed lanes are one held set, never silently reduced to one.
	held, err := observedSelectorHeldLanes(rwa, accounts, earnLaneIDs(true), SelectedRouteID)
	if err != nil || strings.Join(held, ",") != SelectedRouteID+","+source.Lane {
		t.Fatalf("two exposed lanes: held %v err %v", held, err)
	}
}

// TestEconomicFeedInventoryIsTheActiveRegistry pins the feed inventory: every
// active registry lane in registry order — Prime/PRIME/USDC included again
// (owner 2026-10-10, reversing B4) and both Prime siblings — and never the
// exit-only Ethena lane.
func TestEconomicFeedInventoryIsTheActiveRegistry(t *testing.T) {
	t.Parallel()
	feed, err := NewEconomicFeed(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{SelectedRouteID, onreONycUSDC, autoAUTOPYUSD.Lane, PrimeUSDCLaneID, primePRIMEPYUSD.Lane, primePRIMEUSDS.Lane, autoAUTOWYLDSLane}
	if len(feed.routes) != len(want) {
		t.Fatalf("feed inventory size %d, want %d", len(feed.routes), len(want))
	}
	for i, lane := range want {
		route, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		if feed.routes[i].Lane != lane || feed.routes[i].Kamino.CollateralReserve != route.Kamino.CollateralReserve || feed.routes[i].Kamino.DebtReserve != route.Kamino.DebtReserve {
			t.Fatalf("feed route %d drifted: %+v", i, feed.routes[i])
		}
		if feed.routes[i].Lane == ethenaUSDePYUSD.Lane {
			t.Fatal("exit-only Ethena is scored")
		}
	}
}

// TestCombineEconomicsProducesCandidateRouteEconomics pins the candidate
// emission semantics: with the manifest-scoped inventory, complete verified
// evidence for the AUTO reserves produces the AUTO LaneEconomics with its
// true lane, the debt identity is checked against the ROUTE's debt mint
// (PYUSD for AUTO; every installed lane stays USDC), and content that fails
// validation is never emitted. The installed acceptance scope inside
// LaneEconomics.validate is the selector's decision, not the feed's.
func TestCombineEconomicsProducesCandidateRouteEconomics(t *testing.T) {
	t.Parallel()
	auto := autoAUTOPYUSD
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	native := realizedFixture(auto.Lane, now, 0.0612)
	supply, borrow, host := 1_000_000_000_000.0, 500_000_000_000.0, 300.0
	rows := map[string]reserveEconomics{}
	collateral := reserveEconomics{Reserve: auto.Kamino.CollateralReserve, Market: auto.Kamino.Market, Mint: auto.Kamino.CollateralMint,
		ObservedAt: now, Slot: 42, Hash: strings.Repeat("a", 64), SupplyAPY: 0.0004}
	debt := collateral
	debt.Reserve = auto.Kamino.DebtReserve
	debt.Mint = auto.Kamino.DebtMint
	debt.SupplyRaw = supply
	debt.BorrowRaw = borrow
	debt.HostBPS = host
	if err := json.Unmarshal([]byte(`[{"utilization_rate_bps":0,"borrow_rate_bps":0},{"utilization_rate_bps":9000,"borrow_rate_bps":364},{"utilization_rate_bps":10000,"borrow_rate_bps":1075}]`), &debt.Curve); err != nil {
		t.Fatal(err)
	}
	rows[collateral.Reserve] = collateral
	rows[debt.Reserve] = debt
	got := combineEconomics([]RuntimeRoute{auto}, rows, native, nil, now, DefaultSelectorPolicy())
	if len(got) != 1 || got[0].Lane != autoAUTOPYUSD.Lane || got[0].DebtBorrowRaw != borrow || got[0].DebtSupplyRaw != supply || got[0].HostBorrowBPS != host {
		t.Fatalf("candidate route economics not produced from complete evidence: %+v", got)
	}
	// The exit-only Ethena lane is never scored, even from complete evidence.
	ethena := auto
	ethena.Lane = ethenaUSDePYUSD.Lane
	if got := combineEconomics([]RuntimeRoute{ethena}, rows, map[string]nativeYield{ethena.Lane: native[auto.Lane]}, nil, now, DefaultSelectorPolicy()); len(got) != 0 {
		t.Fatalf("exit-only lane scored: %+v", got)
	}
	// The debt identity is the route's OWN debt mint: a USDC-minted debt row
	// for the AUTO debt reserve is an identity mismatch and is refused.
	wrong := debt
	wrong.Mint = bridgeUSDC
	rows[debt.Reserve] = wrong
	if len(combineEconomics([]RuntimeRoute{auto}, rows, native, nil, now, DefaultSelectorPolicy())) != 0 {
		t.Fatal("candidate debt identity mismatch accepted")
	}
	rows[debt.Reserve] = debt
	// Content that cannot project a borrow APR is never emitted, even with a
	// valid manifest binding.
	broken := debt
	broken.Curve = nil
	rows[debt.Reserve] = broken
	if len(combineEconomics([]RuntimeRoute{auto}, rows, native, nil, now, DefaultSelectorPolicy())) != 0 {
		t.Fatal("candidate content validation bypassed")
	}
}
