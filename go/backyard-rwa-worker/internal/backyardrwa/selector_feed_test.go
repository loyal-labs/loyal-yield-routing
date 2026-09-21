package backyardrwa

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
	rows := map[string]verifiedEconomicReserve{c.Reserve: c, d.Reserve: d}
	got := combineEconomics([]RuntimeRoute{r}, rows, native, now, DefaultSelectorPolicy())
	if len(got) != 1 || got[0].EntryCapacity.Known || got[0].DebtSupplyRaw != supply || got[0].DebtBorrowRaw != borrow {
		t.Fatalf("feed: %+v", got)
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
	accounts := []ConfirmedAccount{}
	for _, lane := range selectorLanes {
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

// TestEconomicFeedInventoryIsManifestScoped pins the inventory closure in
// both binding states: the embedded constructor stays exactly the installed
// selector lanes regardless of the manifest, and the manifest-scoped
// constructor appends the candidate AUTO route ONLY for a manifest carrying a
// valid RuntimeBindings.AutoPolicy binding — the explicit absent-binding
// fixture (the shipped pre-install state) and a malformed binding leave the
// inventory identical to the embedded constructor, while the embedded release
// manifest carries the installed binding and appends the route. pgxpool
// connects lazily, so no database is needed to pin inventory shape.
func TestEconomicFeedInventoryIsManifestScoped(t *testing.T) {
	ctx := context.Background()
	const url = "postgresql://backyard_feed@/economic_feed_shape_test"
	embedded := requireEmbeddedInstalledBinding(t)
	plain, err := NewEconomicFeed(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	// The embedded constructor's inventory closure is manifest-independent:
	// exactly the installed selector lanes, never the candidate route.
	if len(plain.routes) != len(selectorLanes) {
		t.Fatalf("embedded inventory changed size: %d", len(plain.routes))
	}
	for i, lane := range selectorLanes {
		want, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		if plain.routes[i].Lane != want.Lane || plain.routes[i].Kamino.CollateralReserve != want.Kamino.CollateralReserve {
			t.Fatalf("embedded installed route %d drifted: %+v", i, plain.routes[i])
		}
		if plain.routes[i].Lane == autoAUTOPYUSD.Lane {
			t.Fatal("embedded inventory contains the candidate route")
		}
	}
	// The absent-binding fixture (the shipped pre-install state) keeps the
	// manifest-scoped inventory identical to the embedded constructor.
	scoped, err := NewEconomicFeedOnManifest(ctx, url, autoAbsentBindingManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer scoped.Close()
	if len(scoped.routes) != len(selectorLanes) {
		t.Fatalf("absent binding changed the scoped inventory size: %d", len(scoped.routes))
	}
	for i, lane := range selectorLanes {
		want, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		if scoped.routes[i].Lane != want.Lane || scoped.routes[i].Kamino.CollateralReserve != want.Kamino.CollateralReserve {
			t.Fatalf("absent binding drifted scoped route %d: %+v", i, scoped.routes[i])
		}
	}
	// The embedded manifest carries the installed binding, so the scoped
	// inventory appends exactly one AUTO route, last.
	installedScoped, err := NewEconomicFeedOnManifest(ctx, url, embedded)
	if err != nil {
		t.Fatal(err)
	}
	defer installedScoped.Close()
	if len(installedScoped.routes) != len(selectorLanes)+1 {
		t.Fatalf("installed binding did not add exactly one route: %d", len(installedScoped.routes))
	}
	if last := installedScoped.routes[len(installedScoped.routes)-1]; last.Lane != autoAUTOPYUSD.Lane {
		t.Fatalf("installed binding did not append the candidate route last: %+v", last)
	}
	autoWant, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	appended := installedScoped.routes[len(installedScoped.routes)-1]
	if appended.Kamino.CollateralReserve != autoWant.Kamino.CollateralReserve {
		t.Fatalf("installed binding appended a drifted candidate route: %+v", appended)
	}
	for i, lane := range selectorLanes {
		want, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		if installedScoped.routes[i].Lane != want.Lane || installedScoped.routes[i].Kamino.CollateralReserve != want.Kamino.CollateralReserve {
			t.Fatalf("installed binding drifted installed route %d: %+v", i, installedScoped.routes[i])
		}
	}
	withAuto, err := NewEconomicFeedOnManifest(ctx, url, autoFixtureManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer withAuto.Close()
	if len(withAuto.routes) != len(selectorLanes)+1 {
		t.Fatalf("valid binding did not add exactly one route: %d", len(withAuto.routes))
	}
	if last := withAuto.routes[len(withAuto.routes)-1]; last.Lane != autoAUTOPYUSD.Lane {
		t.Fatalf("candidate route is not appended last: %+v", last)
	}
	candidateWant, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	candidateAppended := withAuto.routes[len(withAuto.routes)-1]
	if candidateAppended.Kamino.CollateralReserve != candidateWant.Kamino.CollateralReserve {
		t.Fatalf("candidate route drifted from its route config: %+v", candidateAppended)
	}
	for i, lane := range selectorLanes {
		want, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		if withAuto.routes[i].Lane != want.Lane || withAuto.routes[i].Kamino.CollateralReserve != want.Kamino.CollateralReserve {
			t.Fatalf("valid binding drifted installed route %d: %+v", i, withAuto.routes[i])
		}
	}
	malformed := embedded
	malformed.RuntimeBindings.AutoPolicy = &AutoPolicyBinding{Lane: autoAUTOPYUSD.Lane}
	refused, err := NewEconomicFeedOnManifest(ctx, url, malformed)
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()
	if len(refused.routes) != len(selectorLanes) {
		t.Fatalf("malformed binding admitted the candidate route: %d", len(refused.routes))
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
	// The lane authority is the feed's manifest-scoped set; the public
	// combineEconomics wrapper keeps the installed selectorLane scope.
	candidateAllowed := func(lane string) bool {
		return selectorLane(lane) || lane == autoAUTOPYUSD.Lane
	}
	got := combineEconomicsWithLane([]RuntimeRoute{auto}, rows, native, now, DefaultSelectorPolicy(), candidateAllowed)
	if len(got) != 1 || got[0].Lane != autoAUTOPYUSD.Lane || got[0].DebtBorrowRaw != borrow || got[0].DebtSupplyRaw != supply || got[0].HostBorrowBPS != host {
		t.Fatalf("candidate route economics not produced from complete evidence: %+v", got)
	}
	// The public wrapper keeps installed scope: the same complete candidate
	// evidence is NOT accepted through it.
	if got := combineEconomics([]RuntimeRoute{auto}, rows, native, now, DefaultSelectorPolicy()); len(got) != 0 {
		t.Fatalf("installed wrapper accepted the candidate lane: %+v", got)
	}
	// The debt identity is the route's OWN debt mint: a USDC-minted debt row
	// for the AUTO debt reserve is an identity mismatch and is refused.
	wrong := debt
	wrong.Mint = bridgeUSDC
	rows[debt.Reserve] = wrong
	if len(combineEconomicsWithLane([]RuntimeRoute{auto}, rows, native, now, DefaultSelectorPolicy(), candidateAllowed)) != 0 {
		t.Fatal("candidate debt identity mismatch accepted")
	}
	rows[debt.Reserve] = debt
	// Content that cannot project a borrow APR is never emitted, even with a
	// valid manifest binding.
	broken := debt
	broken.Curve = nil
	rows[debt.Reserve] = broken
	if len(combineEconomicsWithLane([]RuntimeRoute{auto}, rows, native, now, DefaultSelectorPolicy(), candidateAllowed)) != 0 {
		t.Fatal("candidate content validation bypassed")
	}
}
