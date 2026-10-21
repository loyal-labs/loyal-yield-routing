package backyard

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// historyFixture is a real Kamino reserve metrics history read on
// 2026-10-11, trimmed to one point a day plus the last hours; its newest
// point is 2026-10-11 02:00 UTC.
func historyFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/kamino-history/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

var historyNow = time.Date(2026, 10, 11, 2, 30, 0, 0, time.UTC)

// keepHistory keeps the fixture's points newer than since.
func keepHistory(t *testing.T, body []byte, since time.Time) []byte {
	t.Helper()
	var h map[string]any
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatal(err)
	}
	var kept []any
	for _, p := range h["history"].([]any) {
		at, _ := time.Parse(time.RFC3339, p.(map[string]any)["timestamp"].(string))
		if at.After(since) {
			kept = append(kept, p)
		}
	}
	h["history"] = kept
	out, _ := json.Marshal(h)
	return out
}

func historyClient(t *testing.T, bodies map[string][]byte, status int) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query()
		if req.Method != http.MethodGet || q.Get("frequency") != "hour" || q.Get("env") != "mainnet-beta" || q.Get("start") == "" || q.Get("end") == "" {
			t.Fatalf("unexpected history request %s", req.URL)
		}
		for reserve, body := range bodies {
			if strings.HasSuffix(req.URL.Path, "/reserves/"+reserve+"/metrics/history") {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			}
		}
		t.Fatalf("history requested for an unexpected reserve: %s", req.URL.Path)
		return nil, nil
	})}
}

// autoFeedLane builds the AUTO lane's economics the way Refresh does: reserve
// rows from the view, realized history read over HTTP, and the yield feed.
func autoFeedLane(t *testing.T, client *http.Client, feedAPY float64) (*EconomicFeed, []LaneEconomics) {
	t.Helper()
	auto := autoAUTOPYUSD
	feed := &EconomicFeed{client: client, routes: []RuntimeRoute{auto}, realized: map[string]realizedReserve{}}
	feed.refreshRealized(context.Background(), historyNow)
	reserves := map[string]reserveEconomics{
		auto.Kamino.CollateralReserve: feedReserveFixture(auto.Kamino.CollateralReserve, auto.Kamino.CollateralMint, auto.Kamino.Market, historyNow, 1.0239),
		auto.Kamino.DebtReserve:       feedReserveFixture(auto.Kamino.DebtReserve, auto.Kamino.DebtMint, auto.Kamino.Market, historyNow, 0.9998),
	}
	feed.publish(reserves, map[string]float64{auto.Lane: feedAPY}, historyNow, DefaultSelectorPolicy())
	markets, _ := feed.Snapshot()
	return feed, markets
}

// AUTO's native yield is its realized market price growth over the trailing
// 30 days (8.85% on 2026-10-11), not the 10.84% Kamino's yield feed reports;
// the feed figure rides along for display only.
func TestAUTONativeYieldIsRealizedPriceGrowthNotTheFeed(t *testing.T) {
	t.Parallel()
	body := historyFixture(t, "AUTO-2026-10-11.json")
	client := historyClient(t, map[string][]byte{autoAUTOPYUSD.Kamino.CollateralReserve: body}, http.StatusOK)
	_, markets := autoFeedLane(t, client, 0.108384)
	if len(markets) != 1 {
		t.Fatalf("AUTO lane not priced from its realized history: %+v", markets)
	}
	m := markets[0]
	if math.Abs(m.NativeAPY-0.0885) > 0.0005 {
		t.Fatalf("AUTO native %.4f%%, want the realized 8.85%%", m.NativeAPY*100)
	}
	if m.FeedNativeAPY == nil || *m.FeedNativeAPY != 0.108384 {
		t.Fatalf("feed APY not carried for comparison: %v", m.FeedNativeAPY)
	}
	if !m.NativeObservedAt.Equal(time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("native observed at %s, want the newest history point", m.NativeObservedAt)
	}
	// The lane's spread, and so its leverage, moves with the realized figure.
	if spread, ok := leverageBorrowSpread(m, 0); !ok || spread >= *m.FeedNativeAPY+m.SupplyAPY-m.CurrentBorrowAPY-0.019 {
		t.Fatalf("spread %.4f still priced at the feed", spread)
	}
}

// A lane whose collateral history is shorter than the window, has a hole at
// the window start, cannot be read, or has gone stale has no economics and a
// named reason. The feed's yield is never substituted.
func TestLaneWithoutEnoughHistoryIsUnavailable(t *testing.T) {
	t.Parallel()
	auto := autoAUTOPYUSD
	full := historyFixture(t, "AUTO-2026-10-11.json")
	short := keepHistory(t, full, historyNow.Add(-20*24*time.Hour))
	if _, err := parseRealizedYield(short, auto.Kamino.CollateralReserve, auto.Kamino.CollateralMint, historyNow); err == nil || err.Error() != "realized_yield_unavailable(history_short)" {
		t.Fatalf("20 days of history measured: %v", err)
	}
	// A hole: the fixture's oldest point (33 days back), then nothing until
	// 25 days back.
	var h, recent struct {
		Reserve string            `json:"reserve"`
		History []json.RawMessage `json:"history"`
	}
	if json.Unmarshal(full, &h) != nil || json.Unmarshal(keepHistory(t, full, historyNow.Add(-25*24*time.Hour)), &recent) != nil {
		t.Fatal("fixture unreadable")
	}
	h.History = append(h.History[:1], recent.History...)
	holed, _ := json.Marshal(h)
	if _, err := parseRealizedYield(holed, auto.Kamino.CollateralReserve, auto.Kamino.CollateralMint, historyNow); err == nil || err.Error() != "realized_yield_unavailable(history_gap)" {
		t.Fatalf("history with a hole at the window start measured: %v", err)
	}
	if _, err := parseRealizedYield(full, auto.Kamino.CollateralReserve, ethenaUSDePYUSD.Kamino.CollateralMint, historyNow); err == nil {
		t.Fatal("another token's history measured")
	}

	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		reason string
	}{
		{"short", short, http.StatusOK, "realized_yield_unavailable(history_short)"},
		{"unreadable", []byte("untrusted body"), http.StatusServiceUnavailable, "realized_yield_unavailable(history_http_503)"},
	} {
		feed, markets := autoFeedLane(t, historyClient(t, map[string][]byte{auto.Kamino.CollateralReserve: tc.body}, tc.status), 0.108384)
		if len(markets) != 0 {
			t.Fatalf("%s: lane priced without a realized yield (feed substituted?): %+v", tc.name, markets)
		}
		if got := feed.UnavailableLanes(); got != auto.Lane+"="+tc.reason {
			t.Fatalf("%s: unavailable %q, want %q", tc.name, got, auto.Lane+"="+tc.reason)
		}
	}

	// A measured yield whose newest point is older than NativeMaxAge is
	// stale, not carried forward.
	feed, markets := autoFeedLane(t, historyClient(t, map[string][]byte{auto.Kamino.CollateralReserve: full}, http.StatusOK), 0.108384)
	if len(markets) != 1 {
		t.Fatal("fresh history not priced")
	}
	later := historyNow.Add(3 * time.Hour)
	reserves := map[string]reserveEconomics{
		auto.Kamino.CollateralReserve: feedReserveFixture(auto.Kamino.CollateralReserve, auto.Kamino.CollateralMint, auto.Kamino.Market, later, 1.0239),
		auto.Kamino.DebtReserve:       feedReserveFixture(auto.Kamino.DebtReserve, auto.Kamino.DebtMint, auto.Kamino.Market, later, 0.9998),
	}
	feed.publish(reserves, map[string]float64{auto.Lane: 0.108384}, later, DefaultSelectorPolicy())
	if markets, _ := feed.Snapshot(); len(markets) != 0 || feed.UnavailableLanes() != auto.Lane+"=realized_yield_unavailable(history_stale)" {
		t.Fatalf("stale realized yield: %+v %q", markets, feed.UnavailableLanes())
	}
}

// USDe's price does not grow: its realized yield is about zero (peg noise)
// while the feed reports 4.75%.
func TestUSDeRealizedYieldIsAboutZero(t *testing.T) {
	t.Parallel()
	ethena := ethenaUSDePYUSD
	got, err := parseRealizedYield(historyFixture(t, "USDe-2026-10-11.json"), ethena.Kamino.CollateralReserve, ethena.Kamino.CollateralMint, historyNow)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.APY) > 0.01 {
		t.Fatalf("USDe realized %.2f%%, want about 0", got.APY*100)
	}
}

// When the feed overstates one lane, ranking on what the tokens earn picks
// the other: the feed puts PRIME (20%) over ONyc (9%), their realized
// growth puts ONyc (11%) over PRIME (6%).
func TestRankingFlipsWhenTheFeedOverstatesALane(t *testing.T) {
	t.Parallel()
	in := selectorFixture()
	now := in.Now
	onyc, _ := runtimeRoute(onreONycUSDC)
	prime, _ := runtimeRoute(PrimeUSDCLaneID)
	routes := []RuntimeRoute{onyc, prime}
	reserves := map[string]reserveEconomics{}
	for _, r := range routes {
		reserves[r.Kamino.CollateralReserve] = feedReserveFixture(r.Kamino.CollateralReserve, r.Kamino.CollateralMint, r.Kamino.Market, now, 1.1)
		reserves[r.Kamino.DebtReserve] = feedReserveFixture(r.Kamino.DebtReserve, r.Kamino.DebtMint, r.Kamino.Market, now, 1)
	}
	realized := map[string]nativeYield{
		onyc.Lane:  {APY: 0.11, ObservedAt: now.Add(-time.Hour), EvidenceID: "realized"},
		prime.Lane: {APY: 0.06, ObservedAt: now.Add(-time.Hour), EvidenceID: "realized"},
	}
	feed := map[string]float64{onyc.Lane: 0.09, prime.Lane: 0.20}
	markets := combineEconomics(routes, reserves, realized, feed, now, in.Policy)
	if len(markets) != 2 {
		t.Fatalf("lanes not priced: %+v", markets)
	}
	template := in.Quotes[0]
	in.Quotes = nil
	for i := range markets {
		markets[i].EntryCapacity = Capacity{Known: true, Unlimited: true}
		q := template
		q.DestinationLane = markets[i].Lane
		in.Quotes = append(in.Quotes, q)
	}
	in.Markets = markets
	if got := SelectOpportunity(in, SelectorState{}); got.DestinationLane != onyc.Lane {
		t.Fatalf("realized ranking chose %q, want %q: %+v", got.DestinationLane, onyc.Lane, got.Candidates)
	}
	// The same sample scored on the feed's figures picks PRIME: the flip is
	// the feed's overstatement.
	onFeed := append([]LaneEconomics(nil), markets...)
	for i := range onFeed {
		onFeed[i].NativeAPY = *onFeed[i].FeedNativeAPY
	}
	in.Markets = onFeed
	if got := SelectOpportunity(in, SelectorState{}); got.DestinationLane != prime.Lane {
		t.Fatalf("feed ranking chose %q, want %q", got.DestinationLane, prime.Lane)
	}
}
