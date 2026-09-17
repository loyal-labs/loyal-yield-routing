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
