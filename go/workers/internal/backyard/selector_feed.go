package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

const kaminoStatsEndpoint = "https://api.kamino.finance/reserves/batch/stats"

// reserveEconomics is one lane reserve as Backyard's account view holds it:
// the chain at the view slot, stamped with the chain clock at that slot.
type reserveEconomics struct {
	Reserve, Market, Mint, Hash string
	Slot                        int64
	ObservedAt                  time.Time
	// SupplyAPY is kamino.Reserve.Rates: what suppliers accrue.
	SupplyAPY            float64
	SupplyRaw, BorrowRaw float64 // raw liquidity units of the reserve's mint
	Curve                []BorrowCurvePoint
	HostBPS              float64
	Status               uint8
	Emergency            bool
	// PriceUSD and Decimals are the reserve's own oracle price and liquidity
	// mint decimals.
	PriceUSD float64
	Decimals int
}

// nativeYield is a lane collateral's native yield: its realized market price
// growth over realizedYieldWindow (parseRealizedYield), observed at the
// newest history point.
type nativeYield struct {
	APY        float64   `json:"apy"`
	ObservedAt time.Time `json:"observedAt"`
	EvidenceID string    `json:"evidenceId"`
}

// parseFeedYields reads Kamino's yield feed (batch stats underlyingApy) per
// lane: display and comparison only. The feed is not what holding the token
// earns (AUTO 10.84% against 8.85% realized over 30 days on 2026-10-11; USDe
// 4.75% while its price does not grow), so no decision reads it. The official
// API returns a dictionary keyed by reserve and string decimals.
func parseFeedYields(body []byte, routes []RuntimeRoute, now time.Time, maxAge time.Duration) (map[string]float64, error) {
	var stats map[string]struct {
		Token      string `json:"token"`
		Underlying *struct {
			Current    *json.Number `json:"current"`
			SourceType string       `json:"sourceType"`
			SourceMint string       `json:"sourceMint"`
			ObservedAt time.Time    `json:"observedAt"`
		} `json:"underlyingApy"`
	}
	if err := json.Unmarshal(body, &stats); err != nil || stats == nil {
		return nil, fmt.Errorf("kamino_stats_contract_invalid")
	}
	out := map[string]float64{}
	for _, route := range routes {
		row, ok := stats[route.Kamino.CollateralReserve]
		if !ok || row.Token != route.Kamino.CollateralMint || row.Underlying == nil {
			continue
		}
		y := row.Underlying
		if y.Current == nil || y.SourceType != "yield-feed" || y.SourceMint != route.Kamino.CollateralMint || !freshAt(now, y.ObservedAt, maxAge) {
			continue
		}
		rate, err := y.Current.Float64()
		if err != nil || !finite(rate) || rate <= -1 || rate > 10 {
			continue
		}
		out[route.Lane] = rate
	}
	return out, nil
}

func fetchFeedYields(ctx context.Context, client *http.Client, endpoint string, routes []RuntimeRoute, now time.Time, age time.Duration) (map[string]float64, error) {
	if client == nil {
		return nil, fmt.Errorf("kamino_stats_client_unavailable")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("kamino_stats_endpoint_invalid")
	}
	// Lanes can share a collateral reserve (the Prime lanes); ask once.
	reserves := make([]string, 0, len(routes))
	seen := map[string]bool{}
	for _, r := range routes {
		if !seen[r.Kamino.CollateralReserve] {
			seen[r.Kamino.CollateralReserve] = true
			reserves = append(reserves, r.Kamino.CollateralReserve)
		}
	}
	if len(reserves) == 0 || len(reserves) > 100 {
		return nil, fmt.Errorf("kamino_stats_batch_invalid")
	}
	q := u.Query()
	q.Set("reserve", strings.Join(reserves, ","))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("kamino_stats_request_invalid")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kamino_stats_unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kamino_stats_http_%d", resp.StatusCode)
	}
	const maxBody = 2 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return nil, fmt.Errorf("kamino_stats_body_unavailable")
	}
	return parseFeedYields(body, routes, now, age)
}

// readReserveEconomics reads every lane reserve from Backyard's account view,
// which plans nothing while it is not live. slotRate reads the chain's
// measured slot rate (chain.Client.SlotsPerSecond), once and only for a
// Legacy-basis reserve. A reserve absent from the chain, not decodable, or
// Legacy-basis without a slot rate leaves only its lanes out.
func readReserveEconomics(ctx context.Context, view *View, routes []RuntimeRoute, slotRate func(context.Context) (float64, error)) (map[string]reserveEconomics, error) {
	// The pinned USDC reserve is the USDC side of every non-USDC debt lane's
	// price conversion, as in the live borrow capacity observation.
	reserves := []string{usdcReference.DebtReserve}
	for _, r := range routes {
		reserves = append(reserves, r.Kamino.CollateralReserve, r.Kamino.DebtReserve)
	}
	reserves = uniqueNonzero(reserves)
	slot, accounts, err := view.read(ctx, append([]string{budgetClockAddress}, reserves...), 0, reserves...)
	if err != nil {
		return nil, err
	}
	observedAt := time.Unix(clockUnixTimestamp(accounts), 0).UTC()
	out := map[string]reserveEconomics{}
	slotsPerSecond, read := 0.0, false
	measured := func() float64 {
		if !read {
			slotsPerSecond, _ = slotRate(ctx)
			read = true
		}
		return slotsPerSecond
	}
	for _, account := range accounts[1:] {
		r, err := decodeReserveEconomics(account, measured)
		if err != nil {
			slog.Warn("backyard selector left out a lane reserve it cannot price", "reserve", account.Address, "error", err)
			continue
		}
		r.Slot, r.ObservedAt = slot, observedAt
		out[r.Reserve] = r
	}
	return out, nil
}

func decodeReserveEconomics(account ConfirmedAccount, measured func() float64) (reserveEconomics, error) {
	reserve, err := kamino.DecodeReserve(chainAccount(account, account.Address))
	if err != nil {
		return reserveEconomics{}, err
	}
	var slotsPerSecond float64
	if reserve.NeedsSlotRate() {
		slotsPerSecond = measured()
	}
	total, err := reserve.TotalLiquiditySF()
	if err != nil {
		return reserveEconomics{}, err
	}
	rates, err := reserve.Rates(slotsPerSecond)
	if err != nil {
		return reserveEconomics{}, err
	}
	// The curve and host rate are scaled to what the reserve accrues, so a
	// projected borrow prices as kamino.Reserve.Rates does.
	accrual, _ := reserve.AccrualPerCurve(slotsPerSecond)
	r := reserveEconomics{Reserve: account.Address, Market: reserve.LendingMarket.String(), Mint: reserve.LiquidityMint.String(), Hash: sha256Bytes(account.Data),
		SupplyAPY: rates.SupplyAPY, SupplyRaw: scaledFloat(total), BorrowRaw: scaledFloat(kamino.U128(reserve.BorrowedAmountSF)), HostBPS: float64(reserve.HostFixedInterestRateBPS) * accrual,
		Status: reserve.Status, Emergency: reserve.EmergencyMode, PriceUSD: scaledFloat(kamino.U128(reserve.MarketPriceSF)), Decimals: int(reserve.MintDecimals)}
	for _, point := range reserve.BorrowRateCurve {
		r.Curve = append(r.Curve, BorrowCurvePoint{float64(point.UtilizationRateBPS), float64(point.BorrowRateBPS) * accrual})
	}
	return r, nil
}

// scaledFloat is a KLend scaled fraction (60 fractional bits) as a float.
func scaledFloat(value *big.Int) float64 {
	f, _ := new(big.Float).SetInt(value).Float64()
	return math.Ldexp(f, -60)
}

// combineEconomics joins each route's reserve rows and realized native yield
// into its lane economics; only active registry lanes contribute. feed is the
// yield feed's APY per lane, carried for display and comparison only; a lane
// without a realized yield has no economics, whatever the feed says.
func combineEconomics(routes []RuntimeRoute, reserves map[string]reserveEconomics, yields map[string]nativeYield, feed map[string]float64, now time.Time, p SelectorPolicy) []LaneEconomics {
	out := make([]LaneEconomics, 0, len(routes))
	valid := func(r reserveEconomics, reserve, mint, market string) bool {
		return r.Reserve == reserve && r.Mint == mint && r.Market == market
	}
	priced := func(r reserveEconomics) bool {
		return finite(r.PriceUSD) && r.PriceUSD > 0 && r.Decimals <= 18
	}
	usdc := reserves[usdcReference.DebtReserve]
	usdcPriced := valid(usdc, usdcReference.DebtReserve, bridgeUSDC, usdcReference.Market) && priced(usdc) && usdc.Decimals == 6
	for _, route := range routes {
		c, d := reserves[route.Kamino.CollateralReserve], reserves[route.Kamino.DebtReserve]
		y, ok := yields[route.Lane]
		if !ok || !valid(c, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, route.Kamino.Market) || !valid(d, route.Kamino.DebtReserve, route.Kamino.DebtMint, route.Kamino.Market) {
			continue
		}
		// Reserve supply and borrowed amounts are raw debt-mint units. They
		// only ever meet other raw amounts of the same reserve (utilization)
		// or the priced quote's raw debt, never USDC. Both reserves are the
		// view at one slot, so the lane is observed at their chain time.
		e := LaneEconomics{Lane: route.Lane, EvidenceID: c.Hash + ":" + d.Hash + ":" + y.EvidenceID, ObservedAt: d.ObservedAt, NativeObservedAt: y.ObservedAt, NativeAPY: y.APY, SupplyAPY: c.SupplyAPY, BorrowCurve: d.Curve, HostBorrowBPS: d.HostBPS, DebtSupplyRaw: d.SupplyRaw, DebtBorrowRaw: d.BorrowRaw}
		if rate, ok := feed[route.Lane]; ok {
			e.FeedNativeAPY = &rate
		}
		// Every lane's debt reserve row carries its own price, so the projected
		// utilization of a USDC-valued borrow is known for every non-USDC debt
		// lane, not only the one the vault holds. Missing price evidence leaves
		// it unknown (0); USDC debt needs no conversion.
		if route.Kamino.DebtMint != bridgeUSDC && usdcPriced && priced(d) {
			e.DebtRawPerUSDCRaw = usdc.PriceUSD / d.PriceUSD * math.Pow10(d.Decimals-6)
		}
		// Feed capacity is deliberately not pair admission. The action-time adapter
		// supplies exact execution-size capacity after policies, caps and swaps pass.
		if c.Status != 0 || d.Status != 0 || c.Emergency || d.Emergency {
			e.EntryBlockedReason = "reserve_inactive_or_emergency"
		}
		// The accrual-scaled curve (plus host rate) at the observed
		// utilization is the annual borrow APR (kamino.Reserve.Rates).
		apr, aprErr := projectedBorrowAPR(e, 0)
		if aprErr != nil {
			continue
		}
		e.CurrentBorrowAPY = math.Expm1(apr)
		if e.validate(now, p) == nil {
			out = append(out, e)
		}
	}
	return out
}

// EconomicFeed refreshes off the execution path. Snapshot never waits on HTTP
// or the view, and failed refreshes do not make stale entries fresh. Safety
// and withdrawal decisions can run while enrichment is down.
type EconomicFeed struct {
	view    *View
	client  *http.Client
	routes  []RuntimeRoute
	mu      sync.RWMutex
	latest  []LaneEconomics
	failure string
	// realized is the last history read per collateral reserve; unavailable
	// names each active lane left out of latest, with its reason.
	realized    map[string]realizedReserve
	unavailable string
}

// NewEconomicFeed observes every active registry lane: the selector scores
// exactly the lanes it may enter.
func NewEconomicFeed(view *View) (*EconomicFeed, error) {
	routes, err := earnFeedRoutes()
	if err != nil {
		return nil, err
	}
	return &EconomicFeed{view: view, client: &http.Client{Timeout: 5 * time.Second}, routes: routes, realized: map[string]realizedReserve{}}, nil
}

// earnFeedRoutes are the active registry lanes' templates, in registry
// order: a lane's economics are its market's, whichever vault holds it.
func earnFeedRoutes() ([]RuntimeRoute, error) {
	var routes []RuntimeRoute
	for _, l := range earnLanes {
		if !l.exitOnly {
			routes = append(routes, l.route)
		}
	}
	return routes, nil
}
func (f *EconomicFeed) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	now := time.Now().UTC()
	p := DefaultSelectorPolicy()
	f.refreshRealized(ctx, now)
	reserves, err := readReserveEconomics(ctx, f.view, f.routes, f.view.rpc.SlotsPerSecond)
	// The feed's own APY is display only: when it is unreadable the lanes
	// carry none and are priced exactly the same.
	feed, _ := fetchFeedYields(ctx, f.client, kaminoStatsEndpoint, f.routes, now, p.NativeMaxAge)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.failure = err.Error()
		return err
	}
	f.publish(reserves, feed, now, p)
	return nil
}

// publish prices every lane from this refresh's reserves, the realized
// yields held per collateral reserve and the feed's display APY. The caller
// holds the write lock.
func (f *EconomicFeed) publish(reserves map[string]reserveEconomics, feed map[string]float64, now time.Time, p SelectorPolicy) {
	yields := map[string]nativeYield{}
	for _, route := range f.routes {
		if r := f.realized[route.Kamino.CollateralReserve]; !r.yield.ObservedAt.IsZero() {
			yields[route.Lane] = r.yield
		}
	}
	f.latest = combineEconomics(f.routes, reserves, yields, feed, now, p)
	f.failure, f.unavailable = "", ""
	if len(f.latest) != len(f.routes) {
		f.failure = "economic_evidence_incomplete"
		f.unavailable = f.unavailableLanes(now, p)
	}
}

// refreshRealized reads each collateral reserve's price history that is
// due, concurrently and outside the lock. A failed read keeps the last
// measured yield, which ages out at NativeMaxAge; it never becomes fresh.
func (f *EconomicFeed) refreshRealized(ctx context.Context, now time.Time) {
	f.mu.RLock()
	due := []RuntimeRoute{}
	seen := map[string]bool{}
	for _, route := range f.routes {
		reserve := route.Kamino.CollateralReserve
		if !seen[reserve] && f.realized[reserve].due(now) {
			due = append(due, route)
		}
		seen[reserve] = true
	}
	f.mu.RUnlock()
	type read struct {
		reserve string
		yield   nativeYield
		err     error
	}
	reads := make(chan read, len(due))
	var wg sync.WaitGroup
	for _, route := range due {
		wg.Add(1)
		go func(route RuntimeRoute) {
			defer wg.Done()
			y, err := fetchRealizedYield(ctx, f.client, kaminoReserveHistoryEndpoint, route, now)
			reads <- read{route.Kamino.CollateralReserve, y, err}
		}(route)
	}
	wg.Wait()
	close(reads)
	f.mu.Lock()
	defer f.mu.Unlock()
	for r := range reads {
		held := f.realized[r.reserve]
		held.attempted, held.reason = now, ""
		if r.err != nil {
			held.reason = r.err.Error()
		} else {
			held.yield = r.yield
		}
		f.realized[r.reserve] = held
	}
}

// unavailableLanes names each active lane missing from latest with a closed
// reason, in registry order: its realized yield's, or economic evidence.
func (f *EconomicFeed) unavailableLanes(now time.Time, p SelectorPolicy) string {
	priced := map[string]bool{}
	for _, e := range f.latest {
		priced[e.Lane] = true
	}
	var out []string
	for _, route := range f.routes {
		if priced[route.Lane] {
			continue
		}
		reason := "economic_evidence_unavailable"
		r := f.realized[route.Kamino.CollateralReserve]
		switch {
		case r.yield.ObservedAt.IsZero() && r.reason != "":
			reason = r.reason
		case r.yield.ObservedAt.IsZero():
			reason = realizedYieldUnavailable("history_unread").Error()
		case !freshAt(now, r.yield.ObservedAt, p.NativeMaxAge):
			reason = realizedYieldUnavailable("history_stale").Error()
		}
		out = append(out, route.Lane+"="+reason)
	}
	return strings.Join(out, " ")
}

// UnavailableLanes is the last refresh's active lanes without economics, each
// with its closed reason code ("" when every lane is priced). Safe to log.
func (f *EconomicFeed) UnavailableLanes() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.unavailable
}
func (f *EconomicFeed) Snapshot() ([]LaneEconomics, string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := append([]LaneEconomics(nil), f.latest...)
	for i := range out {
		out[i].BorrowCurve = append([]BorrowCurvePoint(nil), out[i].BorrowCurve...)
	}
	return out, f.failure
}
