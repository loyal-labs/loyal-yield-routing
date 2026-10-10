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
func TestNativeYieldAPIContractAndMissingValues(t *testing.T) {
	t.Parallel()
	r, now, body := nativeFixture(t)
	got, err := parseNativeYields([]byte(body), []RuntimeRoute{r}, now, 2*time.Hour)
	if err != nil || got[r.Lane].APY != .04965083163 {
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
		parsed, _ := parseNativeYields([]byte(changed), []RuntimeRoute{r}, now, 2*time.Hour)
		if len(parsed) != 0 {
			t.Fatal("invalid native evidence accepted")
		}
	}
	zero := strings.Replace(body, `"current":"0.04965083163"`, `"current":"0"`, 1)
	parsed, err := parseNativeYields([]byte(zero), []RuntimeRoute{r}, now, 2*time.Hour)
	if err != nil || len(parsed) != 1 || parsed[r.Lane].APY != 0 {
		t.Fatal("known zero confused with absent yield")
	}
}
func TestNativeYieldHTTPBoundaries(t *testing.T) {
	t.Parallel()
	r, now, body := nativeFixture(t)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != "GET" || req.URL.Query().Get("reserve") != r.Kamino.CollateralReserve {
			t.Fatal("unexpected request")
		}
		return response(body), nil
	})}
	got, err := fetchNativeYields(context.Background(), client, "https://kamino.invalid/reserves/batch/stats", []RuntimeRoute{r}, now, 2*time.Hour)
	if err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	for _, status := range []int{429, 503} {
		client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("untrusted body"))}, nil
		})
		if _, err := fetchNativeYields(context.Background(), client, "https://kamino.invalid", []RuntimeRoute{r}, now, time.Hour); err == nil || strings.Contains(err.Error(), "untrusted") {
			t.Fatal("HTTP failure not bounded")
		}
	}
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return response(strings.Repeat("x", (2<<20)+1)), nil })
	if _, err := fetchNativeYields(context.Background(), client, "https://kamino.invalid", []RuntimeRoute{r}, now, time.Hour); err == nil {
		t.Fatal("oversized response accepted")
	}
}
func TestVerifiedFeedKeepsIdentityAndDoesNotInventPairCapacity(t *testing.T) {
	t.Parallel()
	r, now, body := nativeFixture(t)
	native, _ := parseNativeYields([]byte(body), []RuntimeRoute{r}, now, 2*time.Hour)
	f := selectorFixture().Markets[0]
	zero := 0
	no := false
	schema := 2
	supply := 1_000_000_000_000.0
	borrow := 500_000_000_000.0
	c := verifiedEconomicReserve{Reserve: r.Kamino.CollateralReserve, Market: r.Kamino.Market, Mint: r.Kamino.CollateralMint, ObservedAt: now, Slot: 42, Hash: strings.Repeat("a", 64), Commitment: "confirmed", Schema: &schema, Status: &zero, Emergency: &no, SupplyAPY: &f.SupplyAPY}
	d := c
	d.Reserve = r.Kamino.DebtReserve
	d.Mint = bridgeUSDC
	d.BorrowAPY = &f.CurrentBorrowAPY
	d.SupplyRaw = &supply
	d.BorrowRaw = &borrow
	d.HostBPS = &f.HostBorrowBPS
	// Real collector padding from the September 16 read-only response.
	if err := json.Unmarshal([]byte(`[{"utilization_rate_bps":0,"borrow_rate_bps":0},{"utilization_rate_bps":9000,"borrow_rate_bps":364},{"utilization_rate_bps":10000,"borrow_rate_bps":1075},{"utilization_rate_bps":10000,"borrow_rate_bps":1075}]`), &d.Curve); err != nil {
		t.Fatal(err)
	}
	// The collector's annualized fields run ~2x the on-chain accrual
	// (2026-09-26); borrowing must price off the plain curve regardless.
	inflated := 0.25
	d.BorrowAPR, d.BorrowAPY = &inflated, &inflated
	rows := map[string]verifiedEconomicReserve{c.Reserve: c, d.Reserve: d}
	got := combineEconomics([]RuntimeRoute{r}, rows, native, now, DefaultSelectorPolicy())
	if len(got) != 1 || got[0].EntryCapacity.Known || got[0].DebtSupplyRaw != supply || got[0].DebtBorrowRaw != borrow {
		t.Fatalf("feed: %+v", got)
	}
	curveAPR := (364.0*5000/9000 + f.HostBorrowBPS) / 10_000 // 50% utilization
	if apr, err := projectedBorrowAPR(got[0], 0); err != nil || math.Abs(apr-curveAPR) > 1e-12 || math.Abs(got[0].CurrentBorrowAPY-math.Expm1(curveAPR)) > 1e-12 {
		t.Fatalf("borrow priced off the inflated stored rate: curve %v got %v / %v %v", curveAPR, apr, got[0].CurrentBorrowAPY, err)
	}
	projected := got[0]
	projected.BorrowCurve = []BorrowCurvePoint{{0, 0}, {10000, 1000}}
	apr, err := projectedBorrowAPR(projected, 100_000_000_000)
	if err != nil || math.Abs(apr-(0.06+projected.HostBorrowBPS/10000)) > 1e-12 {
		t.Fatalf("raw-unit borrow should raise utilization from 50 to 60 percent: %v %v", apr, err)
	}

	d.Mint = "wrong"
	rows[d.Reserve] = d
	if len(combineEconomics([]RuntimeRoute{r}, rows, native, now, DefaultSelectorPolicy())) != 0 {
		t.Fatal("feed identity mismatch accepted")
	}
}
func TestSelectorObservesPriorCustodyAndRejectsMultipleExposures(t *testing.T) {
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
	got, err := observedSelectorRoute(accounts, SelectedRouteID)
	if err != nil || got.Lane != source.Lane {
		t.Fatalf("old custody hidden: %s %v", got.Lane, err)
	}
	other, _ := runtimeRoute(SelectedRouteID)
	for i := range accounts {
		if accounts[i].Address == other.CollateralCustody {
			accounts[i] = tokenAccountFixture(t, other.CollateralCustody, other.Kamino.CollateralMint, bridgeVault, 1)
		}
	}
	if _, err = observedSelectorRoute(accounts, SelectedRouteID); err == nil {
		t.Fatal("two lanes silently reduced to one NAV")
	}
}

// TestEconomicFeedInventoryIsTheActiveRegistry pins the feed inventory: every
// active registry lane in registry order — Prime/PRIME/USDC included again
// (owner 2026-10-10, reversing B4) and both Prime siblings — and never the
// exit-only Ethena lane. pgxpool connects lazily, so no database is needed.
func TestEconomicFeedInventoryIsTheActiveRegistry(t *testing.T) {
	t.Parallel()
	feed, err := NewEconomicFeed(context.Background(), "postgresql://backyard_feed@/economic_feed_shape_test")
	if err != nil {
		t.Fatal(err)
	}
	defer feed.Close()
	want := []string{SelectedRouteID, onreONycUSDC, autoAUTOPYUSD.Lane, PhaseOneLaneID, primePRIMEPYUSD.Lane, primePRIMEUSDS.Lane}
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
	nativeBody := `{"` + auto.Kamino.CollateralReserve + `":{"token":"` + auto.Kamino.CollateralMint + `","underlyingApy":{"current":"0.0612","sourceType":"yield-feed","sourceMint":"` + auto.Kamino.CollateralMint + `","observedAt":"2026-09-15T23:00:13.804Z"}}}`
	native, err := parseNativeYields([]byte(nativeBody), []RuntimeRoute{auto}, now, 2*time.Hour)
	if err != nil || len(native) != 1 {
		t.Fatalf("candidate native evidence refused: %v", err)
	}
	zero, no, schema := 0, false, 2
	supply, borrow, host := 1_000_000_000_000.0, 500_000_000_000.0, 300.0
	supplyAPY, borrowAPY := 0.0004, 0.079
	rows := map[string]verifiedEconomicReserve{}
	collateral := verifiedEconomicReserve{Reserve: auto.Kamino.CollateralReserve, Market: auto.Kamino.Market, Mint: auto.Kamino.CollateralMint,
		ObservedAt: now, Slot: 42, Hash: strings.Repeat("a", 64), Commitment: "confirmed", Schema: &schema, Status: &zero, Emergency: &no, SupplyAPY: &supplyAPY}
	debt := collateral
	debt.Reserve = auto.Kamino.DebtReserve
	debt.Mint = auto.Kamino.DebtMint
	debt.BorrowAPY = &borrowAPY
	debt.SupplyRaw = &supply
	debt.BorrowRaw = &borrow
	debt.HostBPS = &host
	if err := json.Unmarshal([]byte(`[{"utilization_rate_bps":0,"borrow_rate_bps":0},{"utilization_rate_bps":9000,"borrow_rate_bps":364},{"utilization_rate_bps":10000,"borrow_rate_bps":1075}]`), &debt.Curve); err != nil {
		t.Fatal(err)
	}
	rows[collateral.Reserve] = collateral
	rows[debt.Reserve] = debt
	got := combineEconomics([]RuntimeRoute{auto}, rows, native, now, DefaultSelectorPolicy())
	if len(got) != 1 || got[0].Lane != autoAUTOPYUSD.Lane || got[0].DebtBorrowRaw != borrow || got[0].DebtSupplyRaw != supply || got[0].HostBorrowBPS != host {
		t.Fatalf("candidate route economics not produced from complete evidence: %+v", got)
	}
	// The exit-only Ethena lane is never scored, even from complete evidence.
	ethena := auto
	ethena.Lane = ethenaUSDePYUSD.Lane
	if got := combineEconomics([]RuntimeRoute{ethena}, rows, map[string]nativeYield{ethena.Lane: native[auto.Lane]}, now, DefaultSelectorPolicy()); len(got) != 0 {
		t.Fatalf("exit-only lane scored: %+v", got)
	}
	// The debt identity is the route's OWN debt mint: a USDC-minted debt row
	// for the AUTO debt reserve is an identity mismatch and is refused.
	wrong := debt
	wrong.Mint = bridgeUSDC
	rows[debt.Reserve] = wrong
	if len(combineEconomics([]RuntimeRoute{auto}, rows, native, now, DefaultSelectorPolicy())) != 0 {
		t.Fatal("candidate debt identity mismatch accepted")
	}
	rows[debt.Reserve] = debt
	// Content that cannot project a borrow APR is never emitted, even with a
	// valid manifest binding.
	broken := debt
	broken.Curve = nil
	rows[debt.Reserve] = broken
	if len(combineEconomics([]RuntimeRoute{auto}, rows, native, now, DefaultSelectorPolicy())) != 0 {
		t.Fatal("candidate content validation bypassed")
	}
}
